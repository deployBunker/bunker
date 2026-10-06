package audit

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/deployBunker/bunker/internal/auth"
)

// AttachRecordMethod suffixes stamp attach lifecycle records onto the
// AttachAgent connect procedure, mirroring the ExecRecordMethod convention
// (GAP-142) so `bunker audit query --method /bunker.v1.Bunkerd/AttachAgent`
// still finds them while a reader can separate "someone opened the attach RPC"
// (the interceptor's own record) from "the session opened/closed, like this".
const (
	AttachOpenMethod  = "/attach-open"
	AttachCloseMethod = "/attach-close"
)

// AttachRecord is one attach lifecycle record destined for the audit chain
// (GAP-072). Two are written per session — one when the stream is accepted and
// the child starts, one when it ends — so the trail answers "was anyone ever
// attached to this agent, when, for how long and how did it end?".
//
// It carries NO terminal input. The record exists in the same package as the
// exec recorder precisely so it can inherit the redaction posture: Command is
// an ALREADY-REDACTED summary (RedactCommandSummary over command+args) and is
// re-checked before writing, and nothing an operator typed is ever a field.
type AttachRecord struct {
	// AgentID is the attach target — the same value StampStreamAgentID gives
	// the interceptor's record, so the two correlate on agent_id.
	AgentID string
	// Phase is "open" or "close".
	Phase string
	// TTY reports whether a pseudo-terminal was requested for the session.
	TTY bool
	// Command is the ALREADY-REDACTED command summary ("" for the default login
	// shell, rendered by the writer as the login-shell placeholder).
	Command string
	// Reason is the close reason: "exited", "idle_timeout" or "client_gone".
	Reason string
	// ExitCode is the command's exit status on close (nil while unknown, e.g.
	// the client vanished before the child was reaped).
	ExitCode *int32
	// DurationMS is the measured session wall time on close (0 on open).
	DurationMS int64
}

// Attach phases.
const (
	AttachPhaseOpen  = "open"
	AttachPhaseClose = "close"
)

// Close reasons (mirrors the AttachExit.reason vocabulary in the proto).
const (
	AttachReasonExited      = "exited"
	AttachReasonIdleTimeout = "idle_timeout"
	AttachReasonClientGone  = "client_gone"
)

// RecordAttachEvent appends ONE attach lifecycle record to the audit chain.
//
// Mirrors RecordExecCommand exactly, and for the same reasons:
//
//   - it writes through the SAME *AuditLog the interceptor uses, so the record
//     is hash-chained with the RPC record that caused it;
//   - the already-redacted command summary is RE-CHECKED here before writing,
//     so a caller bug cannot land a raw credential in the trail;
//   - a nil (or path-less) log is a no-op and a write failure is logged and
//     swallowed — auditing must never change the session's outcome;
//   - the caller identity comes from the request context, the only source that
//     cannot drift from the interceptor's own record.
//
// The summary is deliberately assembled from fixed vocabulary, a boolean, the
// redacted command, the close reason and an exit code: there is no field on
// this record that can carry what the operator typed.
func RecordAttachEvent(ctx context.Context, log *AuditLog, logger *slog.Logger, ev AttachRecord) {
	if log == nil {
		return
	}
	if ev.Command != "" && !commandSummaryAllowed(ev.Command) {
		log.logWarn("attach audit record dropped: summary not redacted",
			"agent_id", ev.AgentID, "summary_len", len(ev.Command))
		return
	}
	summary := attachSummary(ev)
	// GAP-141: the same session-declaration marker the exec record carries, so
	// a forensic reader filtering on the marker does not miss the attach that
	// an unverified session opened.
	if UnverifiedSession(ctx) {
		summary = prependMarker(TLSUnverifiedMarker, summary)
	}
	claims, _ := auth.ClaimsFromContext(ctx)
	suffix := AttachOpenMethod
	if ev.Phase == AttachPhaseClose {
		suffix = AttachCloseMethod
	}
	rec := Record{
		TS:         time.Now().UTC().Format(time.RFC3339Nano),
		Caller:     CallerFromClaims(claims),
		Method:     "/bunker.v1.Bunkerd/AttachAgent" + suffix,
		RemoteAddr: remoteAddr(ctx),
		AgentID:    ev.AgentID,
		DurationMS: ev.DurationMS,
		Outcome:    "ok",
		Summary:    summary,
	}
	if err := log.Log(rec); err != nil && logger != nil {
		logger.Warn("attach audit write failed", "error", err, "agent_id", ev.AgentID, "phase", ev.Phase)
	}
}

// attachSummary renders the fixed-vocabulary summary line for an attach record.
// Exported to the trail as plain text so `grep 'attach open'` works on the raw
// JSONL, exactly as `grep '/command'` finds exec records.
func attachSummary(ev AttachRecord) string {
	if ev.Phase == AttachPhaseClose {
		exit := "unknown"
		if ev.ExitCode != nil {
			exit = strconv.Itoa(int(*ev.ExitCode))
		}
		return fmt.Sprintf("attach close reason=%s exit=%s duration_ms=%d", ev.Reason, exit, ev.DurationMS)
	}
	cmd := ev.Command
	if cmd == "" {
		cmd = loginShellSummary
	}
	return fmt.Sprintf("attach open tty=%t command=%s", ev.TTY, cmd)
}

// loginShellSummary is the placeholder a no-command attach records in place of
// a command summary: `bunker attach <id>` with no command opens the agent
// user's login shell, which is a fact worth recording rather than an empty
// field that reads like a bug.
const loginShellSummary = "<login-shell>"
