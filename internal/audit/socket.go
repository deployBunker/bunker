package audit

import (
	"fmt"
	"strings"
	"time"
)

// SURF-B4 — audit continuity for socket-based tool delivery.
//
// The agent-owned toolsd socket (SPEC-toolsd-agent-surfaces §3.4, proven live
// in docs/prd/evidence-surfaces-mechanism.md) serves JSON-RPC verbs OUTSIDE
// the daemon's connect interceptors: the proven path is
// client → SSH → agent's socket → systemd → toolsd, with the daemon and its
// hash-chained audit trail outside the data path entirely. Measured
// end-to-end, a tools/list call and file operations completed over the socket
// with no daemon involvement — every one of those operations would be absent
// from `bunker audit verify`'s trail. This file is the trail-side half of the
// chosen fix (option a of the SPEC's §3.4 decision): toolsd writes attributed
// records for every mutating verb into the daemon's chain, on this host, and
// the daemon re-anchors them.
//
// The record contract is deliberately the SAME facts an exec carries — who,
// what verb, what path, under what confinement root — expressed through the
// existing Record fields so `bunker audit list/export/query` need no new
// surface:
//
//	Caller     agent:<agent-id>            (identity passed over the socket,
//	                                        rendered by CallerFromClaims rules;
//	                                        never a raw token)
//	Method     <verb>@socket               (e.g. toolsd_write@socket)
//	AgentID    the agent id
//	SessionID  socket session handle       (redacted by logLocked like every
//	                                        session id)
//	RemoteAddr <agent-uid>@socket          (the agent's own uid — the socket's
//	                                        only auth; never a raw token)
//	Summary    "<verb> path=<path> root=<root> <redacted arg summary>"
//	Outcome    "ok" | the named failure
//
// The summary text is rendered through RedactCommandSummary (the same
// credential scrubber exec command records use) and re-checked by
// commandSummaryAllowed before writing, mirroring RecordExecCommand's
// belt-and-braces posture: a summary that still carries credential material
// is DROPPED, not written.
//
// What this module does NOT do: it cannot force a rogue toolsd to call it.
// Attributed-by-default is a code path, and the SPEC's acceptance criterion
// stays explicit that a socket write which is absent from the hash chain
// fails the audit-continuity row — the JSONL contract below is how the
// verifier and the E2E battery detect exactly that.

// SocketMethodSuffix marks every socket-originated audit record. Suffixing —
// not replacing — keeps `bunker audit query --method <verb>` and a bare
// procedure grep finding the record, the same convention GAP-142 chose for
// correlated exec command records.
const SocketMethodSuffix = "@socket"

// SocketCallerPrefix prefixes the caller identity of a socket-originated
// record. The socket's only authentication is the agent's own uid (SPEC §3.3:
// the socket is a transport, not a boundary), so the attribution identity
// passed over the socket is the agent identity itself.
const SocketCallerPrefix = "agent:"

// SocketRemote is the remote_addr rendered on socket-originated records. The
// record must name the transport honestly: there is no client address to
// report over a unix socket, and pretending otherwise would make socket rows
// indistinguishable from daemon rows.
const SocketRemote = "socket"

// SocketFileOps is the mutating-verb set whose members REQUIRE an audit
// record. toolsd's verb surface is argv-shaped (evidence-surfaces-mechanism:
// every tool is {args, cwd}), so a mutating verb is one that can change the
// tree: write, append, delete/rename, apply/patch, edit and any write-carrying
// MCP projection of those. Read-only verbs (read, list, diff) are recorded
// with RecordSocketVerb when toolsd records them, but are not REQUIRED to be:
// the SPEC's audit criterion speaks of mutating operations. Extend this set
// when toolsd grows a new mutating verb — the E2E battery walks it.
var SocketFileOps = map[string]bool{
	"toolsd_write":  true,
	"toolsd_edit":   true,
	"toolsd_patch":  true,
	"toolsd_apply":  true,
	"toolsd_delete": true,
	"toolsd_rename": true,
	"toolsd_mkdir":  true,
}

// IsSocketMutation reports whether a verb performed over the socket is one
// whose audit record is REQUIRED (the mutating set). Case-sensitive on
// purpose: toolsd verb names are exact CLI verbs and a case-insensitive match
// would let a renamed verb silently escape the requirement.
func IsSocketMutation(verb string) bool {
	return SocketFileOps[verb]
}

// SocketRecord is one attributed socket verb destined for the daemon's audit
// chain (SURF-B4). Field-for-field it carries the facts an exec record
// carries — who, what verb, what path, under what confinement root — as the
// board row requires ("the audit trail must record the SAME facts it records
// today").
type SocketRecord struct {
	// Verb is the exact toolsd verb name (e.g. "toolsd_write"). The record's
	// Method is "<verb>@socket".
	Verb string
	// AgentID is the attribution identity passed over the socket — which
	// agent performed the verb. Empty means unattributed; the recorder
	// refuses to write an unattributed mutating record (refusal, not a
	// silent gap).
	AgentID string
	// Path is the target path of the operation, as the verb received it.
	Path string
	// Root is the confinement root the verb executed under (toolsd's --root
	// / cwd confinement), so the trail can answer "where inside the tree was
	// this write allowed".
	Root string
	// ArgsSummary is the rendered argv summary — the caller (toolsd) renders
	// it through RedactCommandSummary, exactly as the exec recorder requires
	// pre-redacted command text. The recorder RE-CHECKS it (commandSummary
	// Allowed) rather than re-scrubbing: a summary that still carries
	// credential material ANYWHERE (args, path, root) is DROPPED, mirroring
	// RecordExecCommand's belt-and-braces posture.
	ArgsSummary string
	// SessionID is the socket session handle; empty is normalized to
	// "not captured" by logLocked exactly like every other record.
	SessionID string
	// Outcome is "ok" or a named failure ("refused_unattributed",
	// "denied", the verb's error class).
	Outcome string
	// DurationMS is the measured verb wall time (0 = unknown).
	DurationMS int64
	// At is the verb completion time; zero means the recorder stamps now.
	At time.Time
}

// socketSummary renders the one-line summary for a socket verb record: the
// verb, the target path, the confinement root, and the (already redacted)
// argument summary. This is the human-readable half of "same facts as exec".
func socketSummary(ev SocketRecord) string {
	var b strings.Builder
	b.WriteString(ev.Verb)
	if ev.Path != "" {
		b.WriteString(" path=" + ev.Path)
	}
	if ev.Root != "" {
		b.WriteString(" root=" + ev.Root)
	}
	if s := strings.TrimSpace(ev.ArgsSummary); s != "" {
		b.WriteString(" " + s)
	}
	return b.String()
}

// RecordSocketVerb appends ONE attributed record for a verb performed over
// the agent-owned toolsd socket to the daemon's audit chain — the trail-side
// half of the SPEC-toolsd-agent-surfaces §3.4 decision (option a). The
// record rides the SAME *AuditLog the connect interceptor uses, so it is
// hash-chained with every other record and its head is what `bunker audit
// verify` checks.
//
// Posture mirrors RecordExecCommand (GAP-142) and the GAP-133 auth-deny sink:
//
//   - same chain: the record goes through the same AuditLog, chained like any
//     record, so verify covers socket rows without new machinery;
//   - re-check, never re-scrub: the summary is verified with
//     commandSummaryAllowed and a summary that still carries credential
//     material is DROPPED, never written — mirroring RecordExecCommand, where
//     the caller renders via RedactCommandSummary and the recorder is the
//     second, independent check;
//   - no silent unattributed write: a MUTATING verb with an empty AgentID is
//     refused (error), because an unattributed mutating record is the exact
//     hole SURF-B4 exists to close;
//   - failures logged, not fatal to the caller: a write error is returned and
//     the CALLER decides policy (toolsd logs-and-continues; the E2E battery
//     treats any error as a gap).
//
// A nil log is a no-op, so a deployment with auditing disabled behaves
// exactly as before.
func RecordSocketVerb(log *AuditLog, ev SocketRecord) error {
	if log == nil {
		return nil
	}
	if ev.Verb == "" {
		return fmt.Errorf("socket audit record refused: empty verb")
	}
	if IsSocketMutation(ev.Verb) && strings.TrimSpace(ev.AgentID) == "" {
		return fmt.Errorf("socket audit record refused: mutating verb %q is unattributed", ev.Verb)
	}

	summary := socketSummary(ev)
	// Belt and braces: the caller was required to render via
	// RedactCommandSummary (see SocketRecord.ArgsSummary); this RE-CHECK is
	// the independent second arm. A summary that still carries credential
	// material anywhere is dropped rather than written — writing it is the
	// exact failure this feature exists to prevent.
	if !commandSummaryAllowed(summary) {
		log.logWarn("socket audit record dropped: summary not redacted",
			"verb", ev.Verb, "summary_len", len(summary))
		return fmt.Errorf("socket audit record dropped: summary for verb %q is not fully redacted", ev.Verb)
	}

	ts := ev.At
	if ts.IsZero() {
		ts = time.Now()
	}
	caller := ev.AgentID
	if caller == "" {
		caller = "unknown"
	} else if !strings.HasPrefix(caller, SocketCallerPrefix) && !strings.Contains(caller, ":") {
		caller = SocketCallerPrefix + caller
	}

	rec := Record{
		TS:         ts.UTC().Format(time.RFC3339Nano),
		Caller:     caller,
		Method:     ev.Verb + SocketMethodSuffix,
		RemoteAddr: SocketRemote,
		AgentID:    ev.AgentID,
		SessionID:  ev.SessionID,
		DurationMS: ev.DurationMS,
		Outcome:    ev.Outcome,
		Summary:    summary,
	}
	return log.Log(rec)
}

// RequireSocketRecord is the refusal arm of the audit-continuity contract
// (SPEC §3.4 acceptance: "or the surface refuses operations that require
// attribution"): a socket verb that arrives without attribution must be
// REFUSED by the surface, never executed silently. It returns a named error
// listing what is missing so the toolsd side can surface it to the client as
// a real failure rather than an unexplained hang.
func RequireSocketRecord(verb, agentID string) error {
	if strings.TrimSpace(agentID) == "" {
		return fmt.Errorf("verb %q requires attribution: no agent identity was passed over the socket (audit continuity, SURF-B4)", verb)
	}
	return nil
}
