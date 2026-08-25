# Package: `internal/auth`

## Public API

- `JWTAuth` — HS256 JWT validation interceptor for connect-go.
- `NewJWTAuth(secret, keyMgr)` — accepts master and agent-scoped JWTs.
- `NewMasterOnlyJWTAuth(secret, keyMgr)` — rejects agent-scoped tokens; only master tokens allowed.
- `NewJWTAuthWithStaticFallback(secret, staticToken, keyMgr)` / `NewMasterOnlyJWTAuthWithStaticFallback(...)` — also accept a configured static bearer token.
- `(*JWTAuth) IssueMasterToken(ttl)` / `IssueAgentToken(agentID, ttl)` — token issuance helpers.
- `Claims` — embedded `jwt.RegisteredClaims` plus `AgentID` and `KeyID`.
- `ContextWithClaims(ctx, claims)` / `ClaimsFromContext(ctx)` — context propagation helpers.
- `ExtractBearerTokenFromHeader(header)` / `ExtractBearerToken(req)` — Authorization header parsing.
- `TokenAuth` — static bearer-token interceptor. On success it injects a `Claims` with `Subject: "static-token"` into the context (GAP-047) so downstream consumers (the audit interceptor) can attribute the request to "master" without ever seeing the raw token value.
- `NoAuth` — pass-through interceptor when auth is disabled.
- `NewAuthInterceptor(token, enabled)` / `NewJWTAuthInterceptor(...)` / `NewMasterOnlyAuthInterceptor(...)` — factories that choose the right interceptor based on config.
- `MTLSAuth` / `NewMTLSAuth(caCertFile, verifyCN)` / `BuildMTLSConfig(...)` / `BuildMTLSConfigWithCert(...)` — mTLS client-certificate verification and TLS config builders.
- `TLSConnectionStateFromContext(ctx)` — extracts `*tls.ConnectionState` from `http.ServerContextKey`.

## Conventions

- All interceptors implement `connectrpc.com/connect.Interceptor`.
- JWTs are signed with `jwt.SigningMethodHS256`; the secret is configured as `auth.jwt_secret`.
- Agent-scoped tokens carry a non-empty `agent_id` claim; master tokens have an empty `agent_id`.
- Opaque sub-keys are issued by `internal/apikey.Manager` and validated as fallbacks when the bearer token is not a JWT.
- Static-token fallback is supported only for migration; new deployments should rely on JWT secrets or mTLS.
- Successful auth ALWAYS injects `Claims` into the context — `TokenAuth` and the JWTAuth static fallback both use `staticTokenClaims()` (`Subject: "static-token"`), JWT auth injects the parsed claims, sub-key auth injects `Subject: <key_id>`. This identity is the audit trail's only source of caller attribution: the audit interceptor composes INSIDE auth (auth listed first in `connect.WithInterceptors`, so it runs outermost) and reads `ClaimsFromContext` — the raw Authorization header / token value never reaches the audit log.
- mTLS extracts the client certificate from `http.ServerContextKey` (the underlying `*http.Request`), not from connect-go's request type directly.
- Use `ConstantTimeCompare` (or `subtle.ConstantTimeCompare`) for token comparisons.

## Dependencies

- `connectrpc.com/connect` — interceptor interface and error codes.
- `github.com/golang-jwt/jwt/v5` — JWT parsing and signing.
- `internal/apikey` — opaque sub-key manager.
- Standard library: `crypto/subtle`, `crypto/tls`, `crypto/x509`, `crypto/rand`, `encoding/base64`, `net/http`, `context`, `errors`, `fmt`, `strings`, `time`.

## Test Patterns

- Tests table-drive missing/invalid/expired tokens, successful master and agent claims, and CN mismatch for mTLS.
- `jwt_scope_test.go` verifies that `NewMasterOnlyJWTAuth` rejects agent-scoped JWTs and opaque sub-keys while accepting master tokens.
- `auth_streaming_test.go` (TEST-004) verifies streaming handler/client wrapping for `TokenAuth`, `NoAuth`, `JWTAuth`, and `MTLSAuth` (interceptor factory coverage).
- `interceptor_test.go` (GAP-047) asserts that successful static-token auth exposes `Claims` downstream with `Subject: "static-token"` and no agent scope — the identity contract the audit trail relies on — and that the raw token value is not part of those claims.
- mTLS tests construct a test CA, server cert, and client cert chain; `TLSConnectionStateFromContext` is tested by injecting an `http.Request` into a context.
- Static-token fallback is tested by asserting that both a JWT and the static token pass through the same interceptor.
- Use `connect.NewError(...)` assertions with `CodeUnauthenticated` / `CodePermissionDenied` / `CodeInternal`.

## Pitfalls

1. **`masterKeyOnly` must be checked after successful JWT parsing.** A token may be structurally valid but scoped to an agent; if the endpoint is server-level, the interceptor must reject it with `CodeUnauthenticated` even though `parseToken` succeeded.
2. **Static-token fallback is timing-attack safe only if using `subtle.ConstantTimeCompare`.** The fallback uses `ConstantTimeCompare` against the configured static token; never use `==` for secrets.
3. **mTLS TLS state is not on the connect request.** connect-go does not expose TLS state on `connect.AnyRequest`; the interceptor must read `ctx.Value(http.ServerContextKey)` and inspect `req.TLS`. This only works when the server injects the base request.
4. **`isLikelyJWT` is heuristic.** Tokens with three dot-separated base64 segments are treated as JWTs, so a malformed JWT error is surfaced instead of a generic "invalid token" error. Keep the heuristic consistent across token formats.
5. **`ClaimsFromContext` returns nil if no claims were injected.** Callers must always check the `ok` return value before dereferencing `claims.AgentID`.
6. **Auth must run OUTSIDE the audit interceptor.** The server composes interceptors with auth listed first, so the audit interceptor only ever sees authenticated requests. If the order were reversed, failed-auth attempts (including token brute-forcing) would be recorded — and, worse, the audit record would have no identity to attribute. The audit trail's "one record per authenticated RPC" guarantee depends on this composition order.
