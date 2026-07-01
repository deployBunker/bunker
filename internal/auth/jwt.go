// Package auth provides authentication interceptors for connect-go handlers.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
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

// JWTAuth validates incoming requests against JWT tokens (HS256).
// Supports a top-level master token and per-agent scoped sub-keys.
type JWTAuth struct {
	secret        []byte
	keyMgr        *apikey.Manager
	masterKey     string
	staticToken   string // optional fallback static bearer token
	masterKeyOnly bool   // when true, reject agent-scoped tokens
}

// NewJWTAuth creates a JWTAuth using the given HS256 secret.
// If keyMgr is non-nil, bearer tokens that are not JWTs are validated
// against the apikey manager as opaque sub-keys.
func NewJWTAuth(secret string, keyMgr *apikey.Manager) *JWTAuth {
	return &JWTAuth{
		secret:    []byte(secret),
		keyMgr:    keyMgr,
		masterKey: secret,
	}
}

// NewMasterOnlyJWTAuth creates a JWTAuth that rejects agent-scoped tokens.
// Only master tokens (with no agent_id claim) are accepted.
func NewMasterOnlyJWTAuth(secret string, keyMgr *apikey.Manager) *JWTAuth {
	return &JWTAuth{
		secret:        []byte(secret),
		keyMgr:        keyMgr,
		masterKey:     secret,
		masterKeyOnly: true,
	}
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
	if len(a.secret) == 0 {
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
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.secret)
}

// WrapUnary validates the JWT on unary requests.
func (a *JWTAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		claims, err := a.authenticate(req.Header())
		if err != nil {
			return nil, err
		}
		return next(ContextWithClaims(ctx, claims), req)
	}
}

// WrapStreamingHandler validates the JWT on streaming requests.
func (a *JWTAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		claims, err := a.authenticate(conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ContextWithClaims(ctx, claims), conn)
	}
}

// WrapStreamingClient is a no-op — auth is server-side only.
func (a *JWTAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *JWTAuth) authenticate(header http.Header) (*Claims, error) {
	token, err := ExtractBearerTokenFromHeader(header.Get("Authorization"))
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, err)
	}

	// Optional static-token fallback for migration/compat.
	if a.staticToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.staticToken)) == 1 {
		return &Claims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: "static-token",
			},
		}, nil
	}

	// First try JWT validation.
	claims, jwtErr := a.parseToken(token)
	if jwtErr == nil {
		if a.masterKeyOnly && claims.AgentID != "" {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("agent-scoped tokens are not allowed for this endpoint"))
		}
		return claims, nil
	}

	// If a key manager is configured, try opaque sub-key validation.
	if a.keyMgr != nil {
		if key, err := a.keyMgr.Validate(token); err == nil {
			if a.masterKeyOnly && key.AgentID != "" {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("agent-scoped tokens are not allowed for this endpoint"))
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

	// If the token looks like a JWT, report the JWT error.
	if isLikelyJWT(token) {
		return nil, connect.NewError(connect.CodeUnauthenticated, jwtErr)
	}

	return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
}

func (a *JWTAuth) parseToken(token string) (*Claims, error) {
	if len(a.secret) == 0 {
		return nil, errors.New("jwt secret not configured")
	}

	claims := &Claims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return a.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return nil, err
	}
	return claims, nil
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
