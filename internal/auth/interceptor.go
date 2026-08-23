// Package auth provides authentication interceptors for connect-go handlers.
// Supports static bearer tokens and JWT-based authentication.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

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
}

// NewTokenAuth creates a TokenAuth that validates against the given token.
func NewTokenAuth(token string) *TokenAuth {
	return &TokenAuth{token: token}
}

// WrapUnary validates the bearer token on unary requests.
func (a *TokenAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := a.authenticate(req.Header()); err != nil {
			return nil, err
		}
		return next(ContextWithClaims(ctx, staticTokenClaims()), req)
	}
}

// WrapStreamingHandler validates the bearer token on streaming requests.
func (a *TokenAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := a.authenticate(conn.RequestHeader()); err != nil {
			return err
		}
		return next(ContextWithClaims(ctx, staticTokenClaims()), conn)
	}
}

// WrapStreamingClient is a no-op — auth is server-side only.
func (a *TokenAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *TokenAuth) authenticate(header http.Header) error {
	authHeader := header.Get("Authorization")
	if authHeader == "" {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("missing Authorization header"))
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid Authorization header format, expected 'Bearer <token>'"))
	}

	token := parts[1]
	if token != a.token {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
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

// NewJWTAuthInterceptor returns a JWT-based interceptor when a JWT secret is configured.
// Falls back to static token auth if jwtSecret is empty but static token is enabled.
// When both jwtSecret and staticToken are set, the JWT interceptor also accepts the
// static token as a fallback so existing clients keep working during JWT rollout.
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
