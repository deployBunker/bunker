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

	"github.com/deployBunker/bunker/internal/apikey"
)

// TokenAuth validates incoming requests against a static bearer token.
// Implements connect.Interceptor so it can be used with connect.WithInterceptors.
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
		return next(ctx, req)
	}
}

// WrapStreamingHandler validates the bearer token on streaming requests.
func (a *TokenAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := a.authenticate(conn.RequestHeader()); err != nil {
			return err
		}
		return next(ctx, conn)
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
func NewJWTAuthInterceptor(jwtSecret string, keyMgr *apikey.Manager, staticToken string, enabled bool) connect.Interceptor {
	if !enabled {
		return NoAuth{}
	}
	if jwtSecret != "" {
		return NewJWTAuth(jwtSecret, keyMgr)
	}
	if staticToken != "" {
		return NewTokenAuth(staticToken)
	}
	return NoAuth{}
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
