package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// ── QA-BUNKER-58: spawn must not race its own client deadline ──────────────
//
// A client-side deadline (or a DERP-relay connection drop) can expire while
// the daemon is still in rootless-install/dockerd-start; measured 2026-10-04,
// the daemon finished the spawn 20-42s AFTER the CLI printed "spawn failed",
// and each caller's cleanup trap then DESTROYED a healthy agent (spawn
// success observed in the server journal at 20.3s and 41.6s). The request-ctx
// cancellation does NOT abort the daemon's spawn stages, so a transport-class
// failure is AMBIGUOUS: the agent may still be materializing.
//
// The fix is CLI-side reporting semantics only. On an ambiguous error the CLI
// runs a bounded reconciliation probe — re-list agents on the same server —
// and if the requested agent appears (not failed/destroyed), the spawn is
// reported as SUCCESS through the normal bundle output path. Hard refusals
// (invalid argument, unauthenticated, not found, failed precondition,
// permission denied) and any error where the server already reported a
// rollback fail fast exactly as before.

// spawnReconcileAttempts and spawnReconcileInterval are the probe's poll
// shape: 5 polls 3s apart (~12-15s window). The board's measured
// daemon-finishes-later lag is 1-3s past the client failure; a 15s ceiling
// bounds the worst-case added wall time. Vars so tests can shrink the window.
var (
	spawnReconcileAttempts = 5
	spawnReconcileInterval = 3 * time.Second
)

// spawnReconcileWindow bounds each ListAgents call of the probe: the probe
// must never outlive its own budget even when the server stalls (list is a
// cheap in-memory handler server-side; 15s is generous).
const spawnReconcileWindow = 15 * time.Second

// spawnReconcileAutoIDBounds is the creation-time window an AUTO-ID spawn
// (no requested agent id) accepts when attributing a list row to this
// spawn: created up to 2 min before the RPC started (client/daemon clock
// skew, queuing) or 10 min after (slow relay, stalled lists). A row created
// outside the window predates or postdates this spawn and is never claimed.
var spawnReconcileAutoIDBounds = struct {
	past   time.Duration
	future time.Duration
}{past: 2 * time.Minute, future: 10 * time.Minute}

// spawnErrorClass is the QA-BUNKER-58 triage of a failed SpawnAgent RPC.
type spawnErrorClass int

const (
	// spawnErrRefusal: a real server verdict — invalid argument,
	// unauthenticated, not found, failed precondition, permission denied,
	// or any mapped stage error. The spawn did not happen and nothing is
	// materializing; fail exactly as before, no probe.
	spawnErrRefusal spawnErrorClass = iota
	// spawnErrRolledBack: the error text proves the server already ran its
	// rollback (spawnStageErr / "rolled back" / userdel). No agent can be
	// materializing behind it; fail fast, no probe.
	spawnErrRolledBack
	// spawnErrAmbiguous: deadline/context-class, unavailable, canceled, or
	// a transport/EOF-class failure — the RPC failed but the daemon may
	// still be spawning. Probe before reporting failure.
	spawnErrAmbiguous
)

// spawnRollbackMarkers are the error-text signatures of a server-side
// rollback (internal/agent/spawn_failure.go + manager_spawn.go
// spawnStageErr wording). connect maps those causes to typed codes
// (Internal/FailedPrecondition), so the TEXT is the only discriminator that
// proves a rollback ran regardless of which code carried it.
var spawnRollbackMarkers = []string{"rolled back", "userdel"}

// spawnTransportSignatures are the error-text signatures of a raw transport
// failure under CodeUnknown (connect passes the net error through when no
// RPC response was decoded). A signature here means the response may have
// been written by the daemon and lost in transit — ambiguous.
var spawnTransportSignatures = []string{
	"connection reset", "broken pipe", "unexpected eof", "eof",
	"connection refused", "no such host", "i/o timeout",
	"timeout awaiting response", "client.timeout",
}

// classifySpawnRPCError buckets a failed SpawnAgent RPC. nil is a refusal
// (nothing to reconcile); refusal-class errors keep the pre-fix semantics —
// the error is returned to the caller verbatim and ListAgents is never
// called.
func classifySpawnRPCError(err error) spawnErrorClass {
	if err == nil {
		return spawnErrRefusal
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range spawnRollbackMarkers {
		if strings.Contains(msg, marker) {
			return spawnErrRolledBack
		}
	}
	code := connect.CodeOf(err)
	switch code {
	case connect.CodeInvalidArgument,
		connect.CodeUnauthenticated,
		connect.CodeNotFound,
		connect.CodeFailedPrecondition,
		connect.CodePermissionDenied:
		return spawnErrRefusal
	case connect.CodeDeadlineExceeded,
		connect.CodeUnavailable,
		connect.CodeCanceled:
		return spawnErrAmbiguous
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return spawnErrAmbiguous
	}
	// CodeUnknown with no connect verdict at all: a raw transport error
	// (EOF, reset, dropped connection mid-response) is ambiguous — the
	// daemon may have answered and the reply was lost. Anything else
	// (CodeInternal and friends) is a mapped server verdict: by the time it
	// returns, the daemon has finished deciding this spawn's fate.
	if code == connect.CodeUnknown {
		for _, sig := range spawnTransportSignatures {
			if strings.Contains(msg, sig) {
				return spawnErrAmbiguous
			}
		}
	}
	return spawnErrRefusal
}

// reconcileLostSpawn is the bounded reconciliation probe: on an ambiguous
// spawn failure it re-lists agents on the same server and, if the requested
// agent appears in a usable state, rebuilds a SpawnAgentResponse from the
// list summary so the caller gets the SAME bundle output as a clean RPC
// return. When the agent never appears, it returns an error naming BOTH the
// original RPC error and the not-found-on-re-list fact.
//
// The probe is read-only: it never spawns, destroys, or retries the spawn.
func reconcileLostSpawn(
	ctx context.Context,
	client bunkerv1connect.BunkerdClient,
	agentID string,
	spawnStarted time.Time,
	origErr error,
) (*connect.Response[v1.SpawnAgentResponse], error) {
	if agentID != "" {
		fmt.Printf("Reconciling spawn: the RPC failed (%v) but the daemon may still be creating agent %q — re-listing...\n", origErr, agentID)
	} else {
		fmt.Println("Reconciling spawn: the RPC failed but the daemon may still be creating a new (auto-ID) agent — re-listing...")
	}
	for attempt := 1; attempt <= spawnReconcileAttempts; attempt++ {
		if attempt > 1 {
			// A dead spawn ctx (the common deadline case) short-circuits
			// the sleep: poll immediately, the agent may already exist.
			select {
			case <-ctx.Done():
			case <-time.After(spawnReconcileInterval):
			}
		}
		pctx, cancel := context.WithTimeout(context.Background(), spawnReconcileWindow)
		resp, err := client.ListAgents(pctx, newReconcileListRequest())
		cancel()
		if err != nil {
			continue // probe errors are not fatal; the next poll may succeed
		}
		if a, found := matchReconciledAgent(resp.Msg.GetAgents(), agentID, spawnStarted); found {
			if status := strings.ToLower(a.GetStatus()); status == "failed" || status == "destroyed" || status == "deleting" {
				return nil, fmt.Errorf("spawn agent: %v; agent %q was found on re-list but did not complete successfully (status %q) — inspect it with `bunker info %s`",
					origErr, a.GetAgentId(), a.GetStatus(), a.GetAgentId())
			}
			// The agent exists and is not failed/destroyed: treat the spawn
			// as SUCCESS and flow into the normal bundle output path.
			return connect.NewResponse(reconciledSpawnResponse(a)), nil
		}
	}
	label := "agent " + agentID
	if agentID == "" {
		label = "a new (auto-ID) agent"
	}
	return nil, fmt.Errorf("spawn agent: %v; %s was NOT found on re-list after %d attempts — the spawn likely failed server-side; check `bunker list` before retrying to avoid a duplicate",
		origErr, label, spawnReconcileAttempts)
}

// newReconcileListRequest builds the probe's list request. StatusFilter
// "all" is load-bearing: a mid-install agent is pending/starting, which the
// default `bunker list` filter ("running") would hide. The daemon's
// ListAgents serves a flat tracker list (filters/pagination are inert), so
// one call observes every agent.
func newReconcileListRequest() *connect.Request[v1.ListAgentsRequest] {
	return connect.NewRequest(&v1.ListAgentsRequest{StatusFilter: "all"})
}

// matchReconciledAgent finds this spawn's agent in a ListAgents page. With a
// requested id (positional or --agent-id) the id alone identifies the agent —
// the daemon refuses a duplicate id, so an existing row IS this spawn's
// agent. An auto-ID spawn has no name to match, so the probe attributes by
// creation time: an agent created within spawnReconcileAutoIDBounds of the
// spawn start and not in a failed state.
func matchReconciledAgent(agents []*v1.AgentSummary, agentID string, spawnStarted time.Time) (*v1.AgentSummary, bool) {
	if agentID != "" {
		for _, a := range agents {
			if a.GetAgentId() == agentID {
				return a, true
			}
		}
		return nil, false
	}
	for _, a := range agents {
		created, err := time.Parse(time.RFC3339, a.GetCreatedAt())
		if err != nil {
			continue // unparseable stamp: never claimed (a NULL must carry a reason)
		}
		if created.Before(spawnStarted.Add(-spawnReconcileAutoIDBounds.past)) ||
			created.After(spawnStarted.Add(spawnReconcileAutoIDBounds.future)) {
			continue
		}
		if status := strings.ToLower(a.GetStatus()); status == "failed" {
			continue
		}
		return a, true
	}
	return nil, false
}

// reconciledSpawnResponse maps a reconciled AgentSummary onto the response
// shape the normal bundle printer consumes. Fields the list record does not
// carry (docker_host_ssh, api_key, image, inline key) stay empty and their
// bundle lines are skipped by the printer's empty-guards — the SSH key is
// still fetched client-side via GetAgentKey by the shared post-spawn path.
func reconciledSpawnResponse(a *v1.AgentSummary) *v1.SpawnAgentResponse {
	return &v1.SpawnAgentResponse{
		AgentId:          a.GetAgentId(),
		SshfsMount:       a.GetSshfsMount(),
		DockerHostTunnel: a.GetDockerHostTunnel(),
		PublicUrl:        a.GetPublicUrl(),
		TailnetIp:        a.GetTailnetIp(),
		PortRangeStart:   a.GetPortRangeStart(),
		PortRangeEnd:     a.GetPortRangeEnd(),
		ExpiresAt:        a.GetExpiresAt(),
	}
}
