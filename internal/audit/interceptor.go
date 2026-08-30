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
		Caller:     callerFromClaims(claims),
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

// callerFromClaims derives a stable, non-secret identity from the
// authenticated claims. The static master token and unscoped master JWTs both
// map to "master"; agent-scoped keys identify the agent (and the specific key
// when present); any other subject is used verbatim. Raw token material is
// never part of the result.
func callerFromClaims(claims *auth.Claims) string {
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
