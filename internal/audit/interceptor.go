package audit

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/auth"
)

// Interceptor records one audit entry per request that reaches it. Compose it
// INSIDE the auth interceptor (auth listed first in connect.WithInterceptors,
// so it runs outermost): unauthenticated requests are rejected before the
// audit interceptor ever sees them, guaranteeing "one record per authenticated
// request". Caller identity is read from the Claims the auth interceptor put
// into the context — the raw Authorization header / token value is never
// touched.
type Interceptor struct {
	log    *AuditLog
	logger *slog.Logger
}

// NewInterceptor creates an audit interceptor writing to l. logger is used to
// report audit write failures and may be nil.
func NewInterceptor(l *AuditLog, logger *slog.Logger) connect.Interceptor {
	return &Interceptor{log: l, logger: logger}
}

// WrapUnary records one entry per unary request, measuring duration across the
// handler call and deriving the outcome from its error.
func (i *Interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		i.record(ctx, req.Spec().Procedure, err, start, req.Any())
		return resp, err
	}
}

// streamSink is a per-request mutable slot shared between a streaming handler
// and the streaming interceptor that wraps it. connect-go streaming
// interceptors cannot see server-stream request messages, so the handler
// stamps the real target agent id into the sink via StampStreamAgentID; the
// interceptor reads it when recording the audit entry after the handler
// returns. The sink is created inside WrapStreamingHandler, so it never
// leaks across requests. remoteAddr is captured up front from conn.Peer(),
// which connect-go populates before the interceptor chain runs (unlike
// CallInfoForHandlerContext, which is only attached inside the handler
// implementation).
type streamSink struct {
	agentID    string
	remoteAddr string
}

// streamSinkKey is the context key for *streamSink.
type streamSinkKey struct{}

// WrapStreamingHandler records one entry per server stream, measuring duration
// until the stream completes (the handler returns) and deriving the outcome
// from the stream error. Note: the request message of a server stream is not
// visible to interceptors, so the handler stamps the target agent id into a
// per-request context sink (see StampStreamAgentID); without a stamp, agent_id
// falls back to the caller's claims scope (empty for master callers).
// remote_addr comes from conn.Peer(), which is populated before the
// interceptor chain runs.
func (i *Interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		sink := &streamSink{remoteAddr: conn.Peer().Addr}
		ctx = context.WithValue(ctx, streamSinkKey{}, sink)
		err := next(ctx, conn)
		i.record(ctx, conn.Spec().Procedure, err, start, nil)
		return err
	}
}

// WrapStreamingClient is a no-op — auditing is server-side only.
func (i *Interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *Interceptor) record(ctx context.Context, procedure string, err error, start time.Time, msg any) {
	claims, _ := auth.ClaimsFromContext(ctx)

	// Streaming handlers stamp the target agent id into the per-request sink;
	// prefer it over the claims fallback (master tokens carry no agent scope).
	agentID := ""
	if sink, _ := ctx.Value(streamSinkKey{}).(*streamSink); sink != nil && sink.agentID != "" {
		agentID = sink.agentID
	} else {
		agentID = targetAgentID(msg, claims)
	}

	outcome := "ok"
	if err != nil {
		outcome = connect.CodeOf(err).String()
	}

	// Streaming conns expose the peer address directly; unary calls rely on
	// CallInfoForHandlerContext. The sink's remoteAddr is authoritative when
	// present.
	remote := ""
	if sink, _ := ctx.Value(streamSinkKey{}).(*streamSink); sink != nil && sink.remoteAddr != "" {
		remote = sink.remoteAddr
	} else {
		remote = remoteAddr(ctx)
	}

	rec := Record{
		TS:         time.Now().UTC().Format(time.RFC3339Nano),
		Caller:     CallerFromClaims(claims),
		Method:     procedure,
		RemoteAddr: remote,
		AgentID:    agentID,
		DurationMS: time.Since(start).Milliseconds(),
		Outcome:    outcome,
		Summary:    summarize(procedure, agentID),
	}
	if err := i.log.Log(rec); err != nil && i.logger != nil {
		i.logger.Warn("audit write failed", "error", err)
	}
}

// StampStreamAgentID records the target agent id for the current streaming
// request so the streaming audit interceptor can attach it to the record it
// writes after the handler returns. It is a no-op when no streaming sink is
// present in ctx (unary handlers and direct unit calls), so it is safe to
// call from any handler.
func StampStreamAgentID(ctx context.Context, agentID string) {
	if sink, ok := ctx.Value(streamSinkKey{}).(*streamSink); ok {
		sink.agentID = agentID
	}
}

// ExecRecordMethod is the audit Method stamped on the ONE correlated
// command-content record an exec/run appends (GAP-142). The per-RPC record the
// interceptor writes keeps the bare connect procedure
// (/bunker.v1.Bunkerd/ExecAgent); this second record names the sub-kind so a
// forensic reader can separate "someone called the exec RPC" from "this exact
// command was issued", without overloading either the procedure namespace or
// the Record schema. Both records ride the SAME AuditLog and therefore the same
// hash chain. The suffix is appended to the procedure rather than replacing it,
// so `bunker audit query --method` and a bare procedure grep both still find
// the command record.
const ExecRecordMethod = "/command"

// ExecRecord is one redacted command-content record destined for the audit
// chain (GAP-142). It exists as a struct rather than a long argument list so
// the single recorder below has ONE call shape that GAP-074's planned exec
// recorder can inherit instead of standing up a second writer.
type ExecRecord struct {
	// Procedure is the connect procedure the command arrived on
	// (/bunker.v1.Bunkerd/ExecAgent or /RunAgent).
	Procedure string
	// AgentID is the exec target — the same value StampStreamAgentID gives the
	// interceptor's record, so the two records correlate on agent_id.
	AgentID string
	// Outcome is the exec result: "ok", "exit_<code>" (the command ran and
	// returned non-zero), or the connect error code string when the handler
	// failed before/around the command.
	Outcome string
	// Summary is the ALREADY-REDACTED command summary (see
	// RedactCommandSummary / RedactScriptSummary). RecordExecCommand re-checks
	// it before writing.
	Summary string
	// DurationMS is the measured handler wall time when the caller has it
	// (0 = unknown).
	DurationMS int64
}

// RecordExecCommand appends ONE correlated command-content record for an
// exec/run to the audit chain. It is the GAP-142 seam and the single writer
// GAP-074 (the planned richer exec recorder) should extend rather than
// duplicate: everything about how a command reaches the trail lives here.
//
// Mirrors the SEC-08 / GAP-133 authDenySink posture exactly:
//
//   - the record goes through the SAME *AuditLog the interceptor uses, so it is
//     hash-chained with the RPC record that caused it;
//   - the redaction is RE-CHECKED here before writing, so a future caller bug
//     that hands over a raw value cannot land it in the trail (belt: the
//     scrubber in RedactCommandSummary; braces: this check, mirroring
//     disclosure.go's fingerprint-shape re-check);
//   - a write failure is logged and swallowed — auditing must never change the
//     operation's outcome.
//
// A nil (or path-less) log is a no-op, so the handler needs no nil-check of its
// own and a daemon with auditing disabled behaves exactly as before.
//
// The caller identity is deliberately NOT passed in: it is read from the
// request context, which is the only source that cannot drift from the
// interceptor's own record (CallerFromClaims is the same function the
// interceptor uses).
func RecordExecCommand(ctx context.Context, log *AuditLog, logger *slog.Logger, ev ExecRecord) {
	if log == nil {
		return
	}
	if !commandSummaryAllowed(ev.Summary) {
		// A command summary that reaches here already masked is fine (passing
		// one through the scrubber twice is idempotent); one that still carries
		// credential material is dropped rather than written, because writing
		// it is the exact failure this feature exists to prevent.
		log.logWarn("exec command audit record dropped: summary not redacted",
			"agent_id", ev.AgentID, "summary_len", len(ev.Summary))
		return
	}
	claims, _ := auth.ClaimsFromContext(ctx)
	rec := Record{
		TS:         time.Now().UTC().Format(time.RFC3339Nano),
		Caller:     CallerFromClaims(claims),
		Method:     ev.Procedure + ExecRecordMethod,
		RemoteAddr: remoteAddr(ctx),
		AgentID:    ev.AgentID,
		DurationMS: ev.DurationMS,
		Outcome:    ev.Outcome,
		Summary:    ev.Summary,
	}
	if err := log.Log(rec); err != nil && logger != nil {
		logger.Warn("exec command audit write failed", "error", err, "agent_id", ev.AgentID)
	}
}

// commandSummaryAllowed reports whether a rendered command summary is safe to
// write. Two sanctioned shapes are accepted:
//
//   - the script-digest form (scriptSummaryRe): a byte count and a hex SHA-256.
//     It is accepted verbatim because the digest is — correctly — hex-shaped,
//     and re-running the command scrubber over it would mask the digest as if it
//     were a credential, rejecting the recorder's own output. Nothing but those
//     two hex-decimal fields matches, so an arbitrary 'script bytes=…' line
//     carrying a secret still fails;
//   - anything else must survive the command scrubber unchanged. Redaction is
//     idempotent, so an already-masked summary passes; a raw credential summary
//     fails and is dropped rather than written.
func commandSummaryAllowed(summary string) bool {
	if scriptSummaryRe.MatchString(summary) {
		return true
	}
	return redactCommandLine(summary) == summary
}

// CallerFromClaims derives a stable, non-secret identity from the authenticated
// claims. The static master token and unscoped master JWTs both map to
// "master"; agent-scoped keys identify the agent (and the specific key when
// present); any other subject is used verbatim. Raw token material is never
// part of the result.
//
// Exported because the exec command recorder needs the SAME caller string the
// interceptor writes for the RPC, taken from the same context — deriving it
// twice by two different rules is how an audit record comes to disagree with
// the record it is meant to correlate with.
func CallerFromClaims(claims *auth.Claims) string {
	if claims == nil {
		return "unknown" // auth disabled — no identity available
	}
	if claims.AgentID != "" {
		if claims.KeyID != "" {
			return "agent:" + claims.AgentID + " key:" + claims.KeyID
		}
		return "agent:" + claims.AgentID
	}
	if claims.KeyID != "" {
		return "key:" + claims.KeyID
	}
	// Master tokens: static fallback and unscoped JWTs carry no usable
	// subject; label them "master".
	if claims.Subject == "" || claims.Subject == "static-token" {
		return "master"
	}
	return claims.Subject
}

// targetAgentID extracts the agent targeted by the request message, falling
// back to the caller's agent scope when the message carries none (e.g.
// SpawnAgent, which has no agent id yet). Streaming requests pass msg == nil
// because interceptors cannot see server-stream request messages.
func targetAgentID(msg any, claims *auth.Claims) string {
	if msg != nil {
		switch m := msg.(type) {
		case *v1.DestroyAgentRequest:
			return m.AgentId
		case *v1.GetAgentRequest:
			return m.AgentId
		case *v1.AgentMetricsRequest:
			return m.AgentId
		case *v1.ExecAgentRequest:
			return m.AgentId
		case *v1.RunAgentRequest:
			return m.AgentId
		case *v1.HeartbeatAgentRequest:
			return m.AgentId
		case *v1.StopAgentRequest:
			return m.AgentId
		case *v1.StartAgentRequest:
			return m.AgentId
		case *v1.RestartAgentRequest:
			return m.AgentId
		}
	}
	if claims != nil {
		return claims.AgentID
	}
	return ""
}

func remoteAddr(ctx context.Context) string {
	info, ok := connect.CallInfoForHandlerContext(ctx)
	if !ok {
		return ""
	}
	return info.Peer().Addr
}

// summarize builds a short human-readable request summary from the procedure
// name and target agent. It deliberately does NOT include request message
// contents (commands, script bodies, args) — those can carry credentials and
// must never land in the audit trail.
func summarize(procedure, agentID string) string {
	base := procedure
	if idx := strings.LastIndexByte(base, '/'); idx >= 0 {
		base = base[idx+1:]
	}
	if agentID != "" {
		return base + " agent_id=" + agentID
	}
	return base
}

// Ensure Interceptor satisfies the interface at compile time.
var _ connect.Interceptor = (*Interceptor)(nil)
