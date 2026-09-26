// Package auth provides authentication interceptors for connect-go handlers.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"

	"github.com/deployBunker/bunker/internal/apikey"
)

// Claims carries the authenticated identity for a request.
type Claims struct {
	jwt.RegisteredClaims
	AgentID string `json:"agent_id,omitempty"`
	KeyID   string `json:"key_id,omitempty"`
}

// DefaultRotateOverlap is the dual-accept window applied when a rotation
// request does not name one (GAP-132): long enough for in-flight clients to
// re-authenticate against the new secret, short enough to close a retired
// key fast. MaxRotateOverlap is the hard ceiling — a larger requested window
// is clamped, never widened past it.
const (
	DefaultRotateOverlap = 10 * time.Minute
	MaxRotateOverlap     = time.Hour
)

// rotatingSecret holds the live and (optionally) the most recently retired
// HS256 secrets. The current secret signs immediately after a rotate; the
// retired one keeps VALIDATING until previousRetireAt, after which tokens
// signed with it are rejected (dual accept with a hard expiry — GAP-132).
// All access is under mu: rotation races with request authentication.
type rotatingSecret struct {
	mu               sync.RWMutex
	current          []byte
	previous         []byte
	previousRetireAt time.Time // zero when no retired secret is honored
}

// set installs new as the signing secret and moves the old one into the
// overlap slot for the given window.
func (r *rotatingSecret) set(newSecret string, overlap time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.current) > 0 {
		r.previous = r.current
		r.previousRetireAt = time.Now().Add(overlap)
	}
	r.current = []byte(newSecret)
}

// signing returns the secret tokens are signed with (always current).
func (r *rotatingSecret) signing() []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

// accepting returns every secret that currently validates tokens: the
// current one, plus the previous one while the overlap window is open
// (GAP-132 dual accept). Copies are returned so callers cannot mutate the
// live key material through a slice alias. The bytes are returned verbatim
// — truncating or zero-padding to a fixed width would validate under a
// DIFFERENT key than issueToken signed with whenever a secret is not
// exactly that width (a freshly-issued token would then fail its own
// signature check).
func (r *rotatingSecret) accepting() [][]byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([][]byte, 0, 2)
	cur := make([]byte, len(r.current))
	copy(cur, r.current)
	out = append(out, cur)
	if len(r.previous) > 0 && (r.previousRetireAt.IsZero() || time.Now().Before(r.previousRetireAt)) {
		prev := make([]byte, len(r.previous))
		copy(prev, r.previous)
		out = append(out, prev)
	}
	return out
}

// fingerprint returns "sha256:<12 hex>" of the current secret — enough to
// correlate which secret was retired, never the value.
func (r *rotatingSecret) fingerprint() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return FingerprintSecret(string(r.current))
}

// FingerprintSecret reduces a secret to "sha256:<first 12 hex>" of its
// SHA-256, the same disclosure shape the auth layer already uses for
// presented tokens (auth.FingerprintToken). Safe for audit trails.
func FingerprintSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(sum[:6])
}

// JWTAuth validates incoming requests against JWT tokens (HS256).
// Supports a top-level master token and per-agent scoped sub-keys.
type JWTAuth struct {
	secret        *rotatingSecret
	keyMgr        *apikey.Manager
	masterKey     string // legacy: retained for debug inspection only, never consulted on the auth path
	staticToken   string // optional fallback static bearer token
	masterKeyOnly bool   // when true, reject agent-scoped tokens

	// deny (optional) receives every authentication denial, never token
	// material — only the FingerprintToken fingerprint. Set via SetDenySink.
	deny DenyFunc
	// throttle (optional) applies SEC-15 per-source backoff to
	// unauthenticated requests. Set via SetDenySink.
	throttle *throttleState
}

// NewJWTAuth creates a JWTAuth using the given HS256 secret.
// If keyMgr is non-nil, bearer tokens that are not JWTs are validated
// against the apikey manager as opaque sub-keys.
func NewJWTAuth(secret string, keyMgr *apikey.Manager) *JWTAuth {
	return &JWTAuth{
		secret:    &rotatingSecret{current: []byte(secret)},
		keyMgr:    keyMgr,
		masterKey: secret,
	}
}

// NewMasterOnlyJWTAuth creates a JWTAuth that rejects agent-scoped tokens.
// Only master tokens (with no agent_id claim) are accepted.
func NewMasterOnlyJWTAuth(secret string, keyMgr *apikey.Manager) *JWTAuth {
	return &JWTAuth{
		secret:        &rotatingSecret{current: []byte(secret)},
		keyMgr:        keyMgr,
		masterKey:     secret,
		masterKeyOnly: true,
	}
}

// NewMasterOnlyJWTAuthFromAuth derives a master-only JWTAuth from an EXISTING
// instance without copying key material (DF-BUNKER-45): the returned instance
// shares the base's rotating secret (so RotateSecret on the base is visible
// immediately), its key manager, static-token fallback, deny sink and
// throttle; only the agent-scoped-token rejection is forced on. The base
// instance is never mutated, so it keeps serving permissive validation
// elsewhere. A nil base yields nil — callers treat that as auth-disabled.
func NewMasterOnlyJWTAuthFromAuth(base *JWTAuth) *JWTAuth {
	if base == nil {
		return nil
	}
	// Shallow copy: every field shares the base's backing state. masterKey
	// is re-pointed at the same secret string, masterKeyOnly is flipped, and
	// the rotatingSecret POINTER is shared so rotation is observed live.
	derived := *base
	derived.masterKeyOnly = true
	return &derived
}

// NewJWTAuthWithStaticFallback creates a JWTAuth that also accepts a static
// bearer token as a fallback. This is useful for rolling JWT auth out without
// breaking existing static-token clients.
func NewJWTAuthWithStaticFallback(secret, staticToken string, keyMgr *apikey.Manager) *JWTAuth {
	a := NewJWTAuth(secret, keyMgr)
	a.staticToken = staticToken
	return a
}

// NewMasterOnlyJWTAuthWithStaticFallback creates a master-only JWTAuth with
// static token fallback. Agent-scoped tokens are rejected.
func NewMasterOnlyJWTAuthWithStaticFallback(secret, staticToken string, keyMgr *apikey.Manager) *JWTAuth {
	a := NewMasterOnlyJWTAuth(secret, keyMgr)
	a.staticToken = staticToken
	return a
}

// RotateSecret installs newSecret as the authoritative signing secret
// WITHOUT downtime (GAP-132): the previous secret keeps validating existing
// tokens for the overlap window (0 = DefaultRotateOverlap, values beyond
// MaxRotateOverlap are clamped to it) and is rejected afterwards. The
// returned fingerprint names the retired secret (never its value). Tokens
// minted after this call are signed with newSecret immediately.
func (a *JWTAuth) RotateSecret(newSecret string, overlap time.Duration) (previousFingerprint string, err error) {
	if len(strings.TrimSpace(newSecret)) < 32 {
		return "", fmt.Errorf("jwt secret must be at least 32 bytes")
	}
	switch {
	case overlap <= 0:
		overlap = DefaultRotateOverlap
	case overlap > MaxRotateOverlap:
		overlap = MaxRotateOverlap
	}
	prev := a.secret.fingerprint()
	a.secret.set(newSecret, overlap)
	return prev, nil
}

// GenerateSecret creates a new random HS256 secret of the given byte length.
func GenerateSecret(length int) (string, error) {
	if length < 32 {
		return "", fmt.Errorf("jwt secret must be at least 32 bytes")
	}
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand read: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// IssueMasterToken issues a master JWT with no agent scope.
func (a *JWTAuth) IssueMasterToken(ttl time.Duration) (string, error) {
	return a.issueToken("", "", ttl)
}

// IssueAgentToken issues a JWT scoped to a single agent.
func (a *JWTAuth) IssueAgentToken(agentID string, ttl time.Duration) (string, error) {
	if agentID == "" {
		return "", fmt.Errorf("agent_id is required")
	}
	return a.issueToken(agentID, "", ttl)
}

func (a *JWTAuth) issueToken(agentID, keyID string, ttl time.Duration) (string, error) {
	secret := a.secret.signing()
	if len(secret) == 0 {
		return "", fmt.Errorf("jwt secret not configured")
	}
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		AgentID: agentID,
		KeyID:   keyID,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

// WrapUnary validates the JWT on unary requests.
func (a *JWTAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		claims, err := a.authenticate(req.Header(), peerAddr(req.Peer()), req.Spec().Procedure)
		if err != nil {
			return nil, err
		}
		return next(ContextWithClaims(ctx, claims), req)
	}
}

// WrapStreamingHandler validates the JWT on streaming requests.
func (a *JWTAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		claims, err := a.authenticate(conn.RequestHeader(), peerAddr(conn.Peer()), conn.Spec().Procedure)
		if err != nil {
			return err
		}
		return next(ContextWithClaims(ctx, claims), conn)
	}
}

// SetDenySink attaches a denial sink and the per-source throttle to this
// interceptor (GAP-133). Semantics mirror TokenAuth.SetDenySink: every
// authentication denial is reported with the presented token reduced to a
// SHA-256 fingerprint, unauthenticated requests become subject to per-source
// exponential backoff, and a successful auth resets the source. nil detaches.
func (a *JWTAuth) SetDenySink(deny DenyFunc) {
	a.deny = deny
	if deny != nil {
		a.throttle = newThrottleState()
	} else {
		a.throttle = nil
	}
}

// denied is JWTAuth's deny path.
func (a *JWTAuth) denied(source, procedure string, reason denyReason, base *connect.Error) error {
	return applyDenySink(a.deny, a.throttle, source, procedure, reason, base)
}

// WrapStreamingClient is a no-op — auth is server-side only.
func (a *JWTAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *JWTAuth) authenticate(header http.Header, source, procedure string) (*Claims, error) {
	rawToken, err := ExtractBearerTokenFromHeader(header.Get("Authorization"))
	if err != nil {
		return nil, a.denied(source, procedure, newDenyReason(err.Error(), ""), connect.NewError(connect.CodeUnauthenticated, err))
	}
	return a.AuthenticateRawToken(rawToken, source, procedure)
}

// AuthenticateRawToken validates an already-extracted credential through the
// SAME path the connect interceptors use: the static-token fallback, JWT
// parsing against the live and overlap secrets, and opaque sub-key validation
// against the durable key store, plus the throttle and the deny sink.
//
// It exists so a plain-HTTP surface (the WebDAV mount, which is not a connect
// service and therefore has no interceptor to ride) authenticates with the
// daemon's real credential model instead of a second, weaker comparison of
// its own. source/procedure are audit labels, exactly as in authenticate.
func (a *JWTAuth) AuthenticateRawToken(rawToken, source, procedure string) (*Claims, error) {
	// Optional static-token fallback for migration/compat.
	if a.staticToken != "" && subtle.ConstantTimeCompare([]byte(rawToken), []byte(a.staticToken)) == 1 {
		if a.throttle != nil {
			a.throttle.recordSuccess(source)
		}
		return &Claims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: "static-token",
			},
		}, nil
	}

	// First try JWT validation.
	claims, jwtErr := a.parseToken(rawToken)
	if jwtErr == nil {
		if a.masterKeyOnly && claims.AgentID != "" {
			return nil, a.denied(source, procedure, newDenyReason("agent-scoped tokens are not allowed for this endpoint", rawToken), connect.NewError(connect.CodeUnauthenticated, errors.New("agent-scoped tokens are not allowed for this endpoint")))
		}
		if a.throttle != nil {
			a.throttle.recordSuccess(source)
		}
		return claims, nil
	}

	// If a key manager is configured, try opaque sub-key validation.
	if a.keyMgr != nil {
		if key, keyErr := a.keyMgr.Validate(rawToken); keyErr == nil {
			if a.masterKeyOnly && key.AgentID != "" {
				return nil, a.denied(source, procedure, newDenyReason("agent-scoped tokens are not allowed for this endpoint", rawToken), connect.NewError(connect.CodeUnauthenticated, errors.New("agent-scoped tokens are not allowed for this endpoint")))
			}
			if a.throttle != nil {
				a.throttle.recordSuccess(source)
			}
			return &Claims{
				RegisteredClaims: jwt.RegisteredClaims{
					Subject: key.KeyID,
				},
				AgentID: key.AgentID,
				KeyID:   key.KeyID,
			}, nil
		}
	}

	// DF-BUNKER-46 classification — runs ONLY after every real credential
	// path above failed, so admission order is unchanged. If the presented
	// value is the (live or overlap-window) signing secret itself, the
	// caller pasted key material into the bearer slot: name the category
	// mistake instead of the generic "invalid token" that caused the
	// paste-as-bearer lockout. The compare covers only currently-accepted
	// secrets, so a retired, window-expired secret keeps the generic error.
	if a.matchesSigningSecret(rawToken) {
		return nil, a.denied(source, procedure, newDenyReason(errSigningSecretAsBearer.Error(), rawToken), connect.NewError(connect.CodeUnauthenticated, errSigningSecretAsBearer))
	}

	// If the token looks like a JWT, report the JWT error.
	if isLikelyJWT(rawToken) {
		return nil, a.denied(source, procedure, newDenyReason(jwtErr.Error(), rawToken), connect.NewError(connect.CodeUnauthenticated, jwtErr))
	}

	return nil, a.denied(source, procedure, newDenyReason("invalid token", rawToken), connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token")))
}

func (a *JWTAuth) parseToken(token string) (*Claims, error) {
	accepted := a.secret.accepting()
	if len(accepted) == 0 {
		return nil, errors.New("jwt secret not configured")
	}

	claims := &Claims{}
	// Dual accept (GAP-132): the current secret validates first; while the
	// overlap window is open the retired secret validates too, so tokens
	// minted before a rotation keep working. After the window expires the
	// retired secret is no longer in the accept set and those tokens fail.
	var lastErr error
	for _, secret := range accepted {
		key := make([]byte, len(secret))
		copy(key, secret[:])
		_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return key, nil
		}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
		if err == nil {
			return claims, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("invalid token")
}

// errSigningSecretAsBearer is the DF-BUNKER-46 classification error: the
// presented bearer credential IS the signing secret (or a still-accepted
// retired one), which is the paste-as-bearer category mistake that locked
// operators out of their own CLI config. It names the credential class and
// the bearer alternative instead of the generic "invalid token".
var errSigningSecretAsBearer = errors.New("invalid token: this is the HS256 JWT signing secret, not a bearer token — use the daemon's static auth.token or a signed JWT")

// matchesSigningSecret constant-time-compares presented against every
// signing secret this daemon currently accepts (the live one, plus the
// retired one while its GAP-132 overlap window is open — accepting() holds
// the acceptance rule). DF-BUNKER-46: it answers ONE question only, "is
// this the signing key material?", and runs ONLY after the real credential
// paths (JWT, static fallback, opaque sub-key) have already failed, so it
// never changes admission. Secrets outside the current acceptance set (e.g.
// a retired secret whose overlap window has closed) are NOT compared, so a
// dead secret stays indistinguishable from a random wrong token.
func (a *JWTAuth) matchesSigningSecret(presented string) bool {
	for _, secret := range a.secret.accepting() {
		if subtle.ConstantTimeCompare([]byte(presented), secret) == 1 {
			return true
		}
	}
	return false
}

func isLikelyJWT(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 {
			return false
		}
	}
	return true
}

// ExtractBearerTokenFromHeader extracts the bearer token from an Authorization header.
func ExtractBearerTokenFromHeader(authHeader string) (string, error) {
	if authHeader == "" {
		return "", errors.New("missing Authorization header")
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", errors.New("invalid Authorization header format, expected 'Bearer <token>'")
	}
	return parts[1], nil
}

// contextKey is an unexported type for context keys.
type contextKey struct{}

var claimsContextKey = &contextKey{}

// ContextWithClaims injects Claims into a context.
func ContextWithClaims(ctx context.Context, claims *Claims) context.Context {
	return context.WithValue(ctx, claimsContextKey, claims)
}

// ClaimsFromContext extracts Claims from a context.
func ClaimsFromContext(ctx context.Context) (*Claims, bool) {
	claims, ok := ctx.Value(claimsContextKey).(*Claims)
	return claims, ok
}

// ConstantTimeCompare compares two strings in constant time.
func ConstantTimeCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Ensure JWTAuth satisfies the interface at compile time.
var _ connect.Interceptor = (*JWTAuth)(nil)
