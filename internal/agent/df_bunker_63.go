// Package agent manages agent lifecycle: create users, generate SSH keys, start dockerd.
package agent

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/deployBunker/bunker/internal/registry"
)

// ── DF-BUNKER-63: spawn-side uid-collision precheck, destroy --force exit
// hatch, and TTL-reaper destroy-refusal backoff ─────────────────────────────
//
// On a host that also runs host-docker containers, spawn can hand a new agent
// a uid an UNRELATED container process still runs as (the field case: agent
// fad4b89a spawned as uid 1001 while a production container from a previous,
// orphaned agent of the same uid kept running). The isolation promise — a
// per-agent Linux user — is broken by same-uid signal privilege over that
// foreign process, and destroy correctly refuses to userdel the uid while it
// owns live processes, so the TTL reaper then loops forever: expired →
// destroy refused → repeat every minute. The three limbs:
//
//  1. spawn verifies the freshly created user owns NO live process before
//     reporting ready (create-then-verify-then-rollback — the agent itself
//     has no processes yet, so ANY hit is foreign) and fails with a named
//     stage error listing the colliding pids;
//  2. `destroy --force` bypasses the live-process gate by killing the uid's
//     foreign processes with a bounded SIGTERM → SIGKILL escalation (archive
//     home first, then userdel — unchanged order); without force today's
//     refusal semantics are byte-identical;
//  3. a refused destroy is recorded on the durable registry record (GAP-070)
//     and the reaper backs off exponentially (capped at 1h) instead of
//     retrying every minute, while list/status surface the refusal.

// StageUIDCollision is the named spawn stage a uid-collision precheck fails
// in (DF-BUNKER-63). The stage vocabulary is pinned by
// TestSpawnStagesCoverNamedStages; this constant is added to that table's
// coverage so a rename cannot silently break operator tooling that greps the
// journal or the error text.
const StageUIDCollision = "uid-collision"

// spawnProcessScanner is the seam behind the spawn collision precheck: given
// a uid it returns every live process owned by that uid. The package variable
// is the test seam the DF-BUNKER-63 table-driven unit test stubs (no root, no
// /proc writes); production code never swaps it. It reuses the destroy gate's
// /proc probe (listUserProcesses) as its backing implementation.
var spawnProcessScanner = func(uid uint32) ([]userProcess, error) {
	return listUserProcesses(uid)
}

// gateForceKillRunner executes one signal command of the destroy --force
// escalation (kill -TERM / kill -KILL). Package-level seam mirroring
// spawnRollbackRunner: tests inject a recorder instead of signalling anything
// on the machine running the tests; production code never swaps it.
var gateForceKillRunner systemRunner = runSystemCmd

// forceKillGrace is how long the --force escalation waits for SIGTERM'd
// processes to exit before sending SIGKILL. Package var (not const) as the
// same test seam pattern as terminateUserManagerGrace.
var forceKillGrace = 5 * time.Second

// forceKillPollInterval bounds the SIGTERM wait's polling granularity.
const forceKillPollInterval = 100 * time.Millisecond

// forceKillSelfExclusionSlack is the pid window around the destroy's own
// process that the escalation never signals. The kernel wraps pids around
// pid_max, so a recycled pid equal to our own cannot be excluded by identity
// alone; the whole wrap window around os.Getpid() is excluded and later
// re-checked for liveness (a genuinely foreign process there merely survives
// the kill sweep, which the post-sweep probe reports honestly).
const forceKillSelfExclusionSlack = 32768

// buildSpawnUIDCollisionError renders the operator-facing stage error for a
// collided uid (DF-BUNKER-63). It is the single construction for the spawn
// precheck's error, so the returned error and the log line cannot drift.
func buildSpawnUIDCollisionError(username string, uid uint32, procs []userProcess) error {
	summary := (&orphanUIDCheck{UID: uid, UIDKnown: true, UserExists: true, Processes: procs}).Describe()
	return fmt.Errorf(
		"uid collision: user %s was assigned uid %d, which already owns live processes on this host. %s. "+
			"Refusing to spawn an agent into a uid that carries same-uid signal privilege over foreign processes "+
			"(DF-BUNKER-63); the just-created user was rolled back. Retrying the spawn will fail again until the "+
			"foreign processes exit or the uid range is recycled past them",
		username, uid, summary)
}

// checkSpawnUIDCollision is the create-then-verify-then-rollback precheck
// (DF-BUNKER-63 limb 1). It runs AFTER useradd succeeded and BEFORE the spawn
// proceeds to any further stage: the agent itself has no processes yet, so
// ANY live process under the new uid is foreign — a host-docker container, a
// leftover daemon, anything — and handing the uid to the agent would grant it
// same-uid signal privilege over that process (kill -0, signals, /proc reads).
//
// Fail closed: on a collision the caller rolls the just-created user back via
// the standard rollback machinery and the spawn fails with a named stage
// error listing the colliding pids and their cmdlines. The probe itself is
// seam-isolated (spawnProcessScanner); a scanner error FAILS the spawn too —
// "cannot look" must never read as "nothing there" on a security precheck.
// username is passed for error rendering only; the uid comes from the caller,
// which just looked the user up for the isolation stage.
func (m *AgentManager) checkSpawnUIDCollision(ctx context.Context, username string, uid uint32) error {
	procs, err := spawnProcessScanner(uid)
	if err != nil {
		return fmt.Errorf("uid collision precheck for %s (uid %d) could not scan live processes (fail closed): %w",
			username, uid, err)
	}
	if len(procs) == 0 {
		return nil
	}
	err = buildSpawnUIDCollisionError(username, uid, procs)
	m.logger.Error("spawn uid collision precheck failed; rolling back the just-created user",
		"username", username,
		"uid", uid,
		"processes", len(procs),
		"evidence", (&orphanUIDCheck{UID: uid, UIDKnown: true, UserExists: true, Processes: procs}).Describe(),
	)
	return err
}

// killUserProcessesForce terminates every process owned by uid with a BOUNDED
// escalation: SIGTERM to all, wait up to forceKillGrace for exits, SIGKILL to
// the survivors (DF-BUNKER-63 limb 2). The destroy's own process (and the
// kernel's pid-wrap window around it) is excluded — the escalation must never
// signal the destroy that is running it. The kill list is logged: every
// signalled pid, with its command head, at Info, and the survivors after
// SIGKILL at Warn. A liveness re-probe decides the escalation's outcome, not
// the exit status of the kill commands.
//
// This is ONLY called with force=true from the destroy path — never from the
// non-force gate, which keeps today's refusal semantics byte-identical.
func (m *AgentManager) killUserProcessesForce(ctx context.Context, username string, uid uint32, procs []userProcess) {
	self := os.Getpid()
	excluded := func(pid int) bool {
		d := pid - self
		if d < 0 {
			d = -d
		}
		return d <= forceKillSelfExclusionSlack
	}
	signal := func(procs []userProcess, sig string) {
		for _, p := range procs {
			if excluded(p.PID) {
				continue
			}
			m.logger.Info("destroy --force signalling uid process",
				"username", username, "uid", uid, "pid", p.PID, "signal", sig, "cmd", p.Cmd)
			if _, err := gateForceKillRunner(ctx, "kill", "-"+sig, strconv.Itoa(p.PID)); err != nil {
				m.logger.Warn("destroy --force signal failed (process may have already exited)",
					"username", username, "pid", p.PID, "signal", sig, "error", err)
			}
		}
	}

	// Round 1: SIGTERM everyone (except our own pid window), then wait the
	// bounded grace for exits.
	signal(procs, "TERM")
	deadline := time.Now().Add(forceKillGrace)
	var survivors []userProcess
	for {
		remaining, err := spawnProcessScanner(uid)
		if err != nil {
			// The re-probe failed: cannot prove the uid is clear. Fall
			// through to the SIGKILL round over the ORIGINAL list — the
			// escalation errs toward completing the forced teardown, and the
			// post-sweep probe at the end reports the honest final state.
			m.logger.Warn("destroy --force could not re-probe uid processes during the grace wait; escalating to SIGKILL over the original list",
				"username", username, "uid", uid, "error", err)
			survivors = procs
			break
		}
		survivors = nil
		for _, p := range remaining {
			if !excluded(p.PID) {
				survivors = append(survivors, p)
			}
		}
		if len(survivors) == 0 || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			survivors = remaining
		case <-time.After(forceKillPollInterval):
		}
	}

	// Round 2: SIGKILL the survivors.
	if len(survivors) > 0 {
		signal(survivors, "KILL")
		// Give the SIGKILLs a short bounded moment to land before the final
		// verdict probe.
		waitDeadline := time.Now().Add(forceKillGrace)
		for time.Now().Before(waitDeadline) {
			remaining, err := spawnProcessScanner(uid)
			if err != nil {
				break // the final probe below reports the honest state
			}
			clear := true
			for _, p := range remaining {
				if !excluded(p.PID) {
					clear = false
					break
				}
			}
			if clear {
				break
			}
			select {
			case <-ctx.Done():
				goto verdict
			case <-time.After(forceKillPollInterval):
			}
		}
	}

verdict:
	// The verdict is the evidence, never an assumption: report whatever the
	// uid still owns after the full escalation. The gate's caller proceeds
	// either way — force is the operator's explicit exit hatch — but the
	// surviving processes are ON THE RECORD.
	final, err := spawnProcessScanner(uid)
	if err != nil {
		m.logger.Warn("destroy --force final liveness probe failed; surviving state unobservable",
			"username", username, "uid", uid, "error", err)
		return
	}
	var stillAlive []userProcess
	for _, p := range final {
		if !excluded(p.PID) {
			stillAlive = append(stillAlive, p)
		}
	}
	if len(stillAlive) > 0 {
		m.logger.Warn("destroy --force: uid processes survived the SIGTERM/SIGKILL escalation",
			"username", username, "uid", uid,
			"evidence", (&orphanUIDCheck{UID: uid, UIDKnown: true, UserExists: true, Processes: stillAlive}).Describe())
		return
	}
	m.logger.Info("destroy --force cleared the uid's processes",
		"username", username, "uid", uid)
}

// isDestroyRefusalStatus reports whether a destroy-path response status is a
// REFUSAL the reaper must back off from (DF-BUNKER-63 limb 3): the destroy
// deleted nothing and a blind retry every minute would loop forever.
func isDestroyRefusalStatus(status string) bool {
	switch status {
	case StatusLiveProcesses, StatusHomeRetained:
		return true
	}
	return false
}

// backoffForAttempts returns the reaper's retry delay after `attempts`
// recorded refusals: exponential from one minute, capped at one hour.
// attempts 1 → 1m (the next tick), 2 → 2m, 3 → 4m … capped at 60m.
func backoffForAttempts(attempts int) time.Duration {
	const (
		base    = time.Minute
		maxWait = time.Hour
	)
	if attempts < 1 {
		return base
	}
	wait := base
	for i := 1; i < attempts; i++ {
		wait *= 2
		if wait >= maxWait {
			return maxWait
		}
	}
	if wait > maxWait {
		return maxWait
	}
	return wait
}

// recordDestroyRefusal folds a destroy refusal into the agent's durable
// registry record (DF-BUNKER-63 limb 3). Attempts accumulate across restarts
// because the state is durable (GAP-070); the registry write failure is
// logged, never fatal — the destroy refusal itself already happened and the
// next refused attempt will try the append again. Without a registry only
// the in-memory tracker status below marks the agent.
func (m *AgentManager) recordDestroyRefusal(agentID, status string, cause error) {
	if !isDestroyRefusalStatus(status) {
		// The recorder is only for refusals; anything else is a caller bug
		// and must not write durable state.
		m.logger.Error("refusing to record a non-refusal destroy status", "agent_id", agentID, "status", status)
		return
	}
	if m.registry == nil {
		// Registry disabled or unavailable: the durable half of the backoff
		// cannot exist, but the tracker status below still surfaces the
		// refusal for this daemon's lifetime.
		m.logger.Warn("destroy refusal not durably recorded (registry unavailable); marking the tracker status only",
			"agent_id", agentID, "status", status)
	} else {
		prev := m.registry.RefusalOf(agentID)
		refusal := &registry.Refusal{
			Status:        status,
			Attempts:      1,
			LastError:     "",
			LastAttemptAt: time.Now().UTC().Format(time.RFC3339),
		}
		if prev != nil {
			refusal.Attempts = prev.Attempts + 1
			if prev.Status != "" {
				refusal.Status = prev.Status
			}
		}
		if cause != nil {
			refusal.LastError = cause.Error()
		}
		if err := m.registry.AppendRefusal(agentID, refusal); err != nil {
			m.logger.Warn("registry refusal append failed", "agent_id", agentID, "error", err)
		}
	}
	// Surface the state on the live tracker record so list/status show the
	// agent as destroy-refused rather than running-forever. The status keeps
	// the wire vocabulary's "destroy-refused:" prefix distinct from the
	// destroy-path response statuses (which name what the destroy refused to
	// DO), and carries the retry delay for operators.
	wait := backoffForAttempts(m.refusalAttempts(agentID))
	m.tracker.UpdateStatus(agentID, "destroy-refused:"+status)
	m.logger.Warn("destroy refused; recording refusal for reaper backoff",
		"agent_id", agentID,
		"status", status,
		"attempts", m.refusalAttempts(agentID),
		"next_retry_in", wait.String(),
		"error", cause,
	)
}

// refusalAttempts reads the accumulated attempt count for agentID (0 when
// none is recorded or the registry is unavailable).
func (m *AgentManager) refusalAttempts(agentID string) int {
	if m.registry == nil {
		return 0
	}
	if refusal := m.registry.RefusalOf(agentID); refusal != nil {
		return refusal.Attempts
	}
	return 0
}

// reaperBackoffRemaining reports how much longer the reaper must wait before
// retrying agentID's destroy, based on the durable refusal record. Zero means
// the agent may be reaped now (no refusal, no registry, or the backoff has
// elapsed).
func (m *AgentManager) reaperBackoffRemaining(agentID string) time.Duration {
	if m.registry == nil {
		return 0
	}
	refusal := m.registry.RefusalOf(agentID)
	if refusal == nil || refusal.LastAttemptAt == "" {
		return 0
	}
	last, err := time.Parse(time.RFC3339, refusal.LastAttemptAt)
	if err != nil {
		// An unparseable timestamp must not wedge the reaper into silence:
		// treat the refusal as fresh (one full backoff window from now).
		return backoffForAttempts(refusal.Attempts)
	}
	elapsed := time.Since(last)
	wait := backoffForAttempts(refusal.Attempts)
	if elapsed >= wait {
		return 0
	}
	return wait - elapsed
}

// refusalSurfaces is the test-visible shape of the refusal state the CLI's
// list/info read (kept here so the manager owns the vocabulary).
type refusalSurfaces struct {
	Status   string
	Attempts int
	Wait     time.Duration
}

// agentRefusalSurfaces reads the durable refusal state for surfacing.
func (m *AgentManager) agentRefusalSurfaces(agentID string) *refusalSurfaces {
	if m.registry == nil {
		return nil
	}
	refusal := m.registry.RefusalOf(agentID)
	if refusal == nil {
		return nil
	}
	return &refusalSurfaces{
		Status:   refusal.Status,
		Attempts: refusal.Attempts,
		Wait:     backoffForAttempts(refusal.Attempts),
	}
}

// guard strings keep the log-vocabulary greppable (the repo's established
// pattern for test-pinned markers).
const (
	spawnUIDCollisionMarker = "spawn uid collision precheck failed"
	forceKillMarker         = "destroy --force signalling uid process"
	refusalBackoffMarker    = "destroy refused; recording refusal for reaper backoff"
)
