// Package auth provides authentication interceptors for connect-go handlers.
// Supports static bearer tokens and JWT-based authentication.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"

	"github.com/deployBunker/bunker/internal/apikey"
)

// TokenAuth validates incoming requests against a static bearer token.
// Implements connect.Interceptor so it can be used with connect.WithInterceptors.
// On success it injects a Claims with Subject "static-token" into the context,
// mirroring the static-token fallback inside JWTAuth, so downstream consumers
// (e.g. the audit interceptor) can attribute the request to "master" without
// ever seeing the raw token value.
type TokenAuth struct {
	token string
	// deny (optional) receives every authentication denial, never token
	// material — only the FingerprintToken fingerprint. Set via SetDenySink.
	deny DenyFunc
	// throttle (optional) applies SEC-15 per-source backoff to
	// unauthenticated requests. Set via SetDenySink.
	throttle *throttleState
}

// NewTokenAuth creates a TokenAuth that validates against the given token.
func NewTokenAuth(token string) *TokenAuth {
	return &TokenAuth{token: token}
}

// SetDenySink attaches a denial sink and the per-source throttle to this
// interceptor (GAP-133). When deny is non-nil every authentication denial is
// reported to it (with the presented token reduced to a SHA-256 fingerprint)
// and unauthenticated requests become subject to per-source exponential
// backoff: after ThrottleFailureThreshold failures from one source within
// ThrottleFailureWindow, subsequent unauthenticated requests from that source
// are refused with CodeUnavailable until the backoff expires. A successful
// auth resets the source. May be called before or after serving begins; a
// nil deny detaches the sink and disables throttling.
func (a *TokenAuth) SetDenySink(deny DenyFunc) {
	a.deny = deny
	if deny != nil {
		a.throttle = newThrottleState()
	} else {
		a.throttle = nil
	}
}

// WrapUnary validates the bearer token on unary requests.
func (a *TokenAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := a.authenticate(req.Header(), peerAddr(req.Peer()), req.Spec().Procedure); err != nil {
			return nil, err
		}
		return next(ContextWithClaims(ctx, staticTokenClaims()), req)
	}
}

// WrapStreamingHandler validates the bearer token on streaming requests.
func (a *TokenAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := a.authenticate(conn.RequestHeader(), peerAddr(conn.Peer()), conn.Spec().Procedure); err != nil {
			return err
		}
		return next(ContextWithClaims(ctx, staticTokenClaims()), conn)
	}
}

// WrapStreamingClient is a no-op — auth is server-side only.
func (a *TokenAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// denyReason classifies an authentication failure for the deny sink without
// ever carrying token material.
type denyReason struct {
	msg         string // short, non-secret reason
	fingerprint string
}

// newDenyReason builds the classification for a presented raw token ("" when
// none was presented). The token itself never leaves this function.
func newDenyReason(msg, rawToken string) denyReason {
	return denyReason{msg: msg, fingerprint: FingerprintToken(rawToken)}
}

// applyDenySink applies the GAP-133 deny side effects for one failed
// authentication from source: the optional throttle decision (which can
// UPGRADE the denial to CodeUnavailable) and the optional deny sink. It is
// shared by TokenAuth and JWTAuth. reason carries the non-secret
// classification; base is the error the caller would have returned anyway.
// The returned error is what the interceptor must return.
func applyDenySink(deny DenyFunc, throttle *throttleState, source, procedure string, reason denyReason, base *connect.Error) error {
	if throttle == nil && deny == nil {
		return base
	}
	now := time.Now()
	if throttle != nil {
		// Gate FIRST: a source already in backoff is refused with
		// CodeUnavailable, so the failures that just armed the backoff are
		// still reported as ordinary denials and the NEXT request is the one
		// throttled — exactly the "5 failures, then the 6th is throttled"
		// contract.
		if allow, backoff := throttle.allow(source, now); !allow {
			slog.Warn("auth throttle engaged",
				"source", source,
				"procedure", procedure,
				"retry_in", backoff.String(),
			)
			if deny != nil {
				deny(DenyEvent{
					RemoteAddr:       source,
					Procedure:        procedure,
					Reason:           reason.msg + " (throttled)",
					TokenFingerprint: reason.fingerprint,
				})
			}
			return connect.NewError(connect.CodeUnavailable, errors.New("too many failed authentications from this source; retry later"))
		}
		// Count this failure; reaching the threshold arms the backoff that
		// the next request from this source will hit.
		throttle.recordFailure(source, now)
	}
	if deny != nil {
		deny(DenyEvent{
			RemoteAddr:       source,
			Procedure:        procedure,
			Reason:           reason.msg,
			TokenFingerprint: reason.fingerprint,
		})
	}
	return base
}

// denied is TokenAuth's deny path.
func (a *TokenAuth) denied(source, procedure string, reason denyReason, base *connect.Error) error {
	return applyDenySink(a.deny, a.throttle, source, procedure, reason, base)
}

func (a *TokenAuth) authenticate(header http.Header, source, procedure string) error {
	authHeader := header.Get("Authorization")
	if authHeader == "" {
		return a.denied(source, procedure, newDenyReason("missing authorization header", ""), connect.NewError(connect.CodeUnauthenticated, errors.New("missing Authorization header")))
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return a.denied(source, procedure, newDenyReason("invalid authorization header format", ""), connect.NewError(connect.CodeUnauthenticated, errors.New("invalid Authorization header format, expected 'Bearer <token>'")))
	}

	token := parts[1]
	if token != a.token {
		return a.denied(source, procedure, newDenyReason("invalid token", token), connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token")))
	}

	if a.throttle != nil {
		a.throttle.recordSuccess(source)
	}
	return nil
}

// NoAuth is a pass-through interceptor for when auth is disabled.
type NoAuth struct{}

func (NoAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return next
}

func (NoAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func (NoAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// NewAuthInterceptor returns the appropriate interceptor based on config.
// If auth is disabled, returns a no-op interceptor.
// If a static token is set, returns TokenAuth.
func NewAuthInterceptor(token string, enabled bool) connect.Interceptor {
	if !enabled || token == "" {
		return NoAuth{}
	}
	return NewTokenAuth(token)
}

// AttachDenySink attaches the GAP-133 denial sink and per-source throttle to
// an auth interceptor returned by the New*AuthInterceptor factories, whose
// concrete type the caller may not know. NoAuth (auth disabled) has nothing
// to deny and is returned unchanged. The returned interceptor is the same
// instance, mutated in place — safe to call before serving begins.
func AttachDenySink(i connect.Interceptor, deny DenyFunc) connect.Interceptor {
	switch a := i.(type) {
	case *JWTAuth:
		a.SetDenySink(deny)
	case *TokenAuth:
		a.SetDenySink(deny)
	}
	return i
}

// peerAddr extracts the client address from a connect Peer, falling back to
// "unknown" when the transport does not expose one.
func peerAddr(p connect.Peer) string {
	if p.Addr == "" {
		return "unknown"
	}
	return p.Addr
}

// NewJWTAuthInterceptor returns a JWT-based interceptor when a JWT secret is configured.
// Falls back to static token auth if jwtSecret is empty but static token is enabled.
// When both jwtSecret and staticToken are set, the JWT interceptor also accepts the
// static token as a fallback so existing clients keep working during JWT rollout.
//
// New callers must prefer NewJWTAuthInterceptorFromAuth: this string-based form
// builds a PRIVATE JWTAuth, so a later RotateSecret on a different instance never
// reaches the interceptor it returned (the DF-BUNKER-45 live-rotation defect).
func NewJWTAuthInterceptor(jwtSecret string, keyMgr *apikey.Manager, staticToken string, enabled bool) connect.Interceptor {
	if !enabled {
		return NoAuth{}
	}
	if jwtSecret != "" {
		return NewJWTAuthWithStaticFallback(jwtSecret, staticToken, keyMgr)
	}
	if staticToken != "" {
		return NewTokenAuth(staticToken)
	}
	return NoAuth{}
}

// NewMasterOnlyAuthInterceptor returns a JWT interceptor that rejects agent-scoped
// tokens. Only master tokens and static tokens are accepted.
// This should be used for Bunkerd-level RPCs where scoped sub-keys must not be allowed.
//
// Same caveat as NewJWTAuthInterceptor: string-based, builds a PRIVATE JWTAuth.
// Prefer NewMasterOnlyAuthInterceptorFromAuth for anything that must observe
// secret rotation (DF-BUNKER-45).
func NewMasterOnlyAuthInterceptor(jwtSecret string, keyMgr *apikey.Manager, staticToken string, enabled bool) connect.Interceptor {
	if !enabled {
		return NoAuth{}
	}
	if jwtSecret != "" {
		return NewMasterOnlyJWTAuthWithStaticFallback(jwtSecret, staticToken, keyMgr)
	}
	if staticToken != "" {
		return NewTokenAuth(staticToken)
	}
	return NoAuth{}
}

// NewJWTAuthInterceptorFromAuth wraps an EXISTING *JWTAuth as the permissive
// (agent-scoped-capable) interceptor (DF-BUNKER-45): validation reads the
// live rotating secret through the shared instance on every request, so
// RotateSecret's mutation takes effect on this path immediately. The static
// token fallback must already be carried by the instance (build it with
// NewJWTAuthWithStaticFallback). Auth disabled or a nil instance yields
// NoAuth — the same posture the string-based factory produces for those.
func NewJWTAuthInterceptorFromAuth(jwtAuth *JWTAuth, enabled bool) connect.Interceptor {
	if !enabled || jwtAuth == nil {
		return NoAuth{}
	}
	return jwtAuth
}

// NewMasterOnlyAuthInterceptorFromAuth is the master-only counterpart of
// NewJWTAuthInterceptorFromAuth (DF-BUNKER-45): it derives a master-only
// JWTAuth from the SAME instance the key-lifecycle RPCs mutate, so secret
// rotation is observed live while agent-scoped tokens stay rejected
// (SEC-07/GAP-131 posture). The derivation shares the rotating secret, key
// manager and static fallback; only the master-only gate differs. Auth
// disabled or a nil instance yields NoAuth.
func NewMasterOnlyAuthInterceptorFromAuth(jwtAuth *JWTAuth, enabled bool) connect.Interceptor {
	if !enabled || jwtAuth == nil {
		return NoAuth{}
	}
	return NewMasterOnlyJWTAuthFromAuth(jwtAuth)
}

// staticTokenClaims returns the Claims attributed to a successful static
// master token authentication. Subject "static-token" is the shared marker
// used by both TokenAuth and the JWTAuth static fallback; audit consumers map
// it to the "master" label.
func staticTokenClaims() *Claims {
	return &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "static-token",
		},
	}
}

// Ensure our types satisfy the interface at compile time.
var (
	_ connect.Interceptor = (*TokenAuth)(nil)
	_ connect.Interceptor = NoAuth{}
)

// ExtractBearerToken is a helper to extract the bearer token from a request.
func ExtractBearerToken(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", fmt.Errorf("missing Authorization header")
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", fmt.Errorf("invalid Authorization header format")
	}
	return parts[1], nil
}
