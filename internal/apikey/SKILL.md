# Package: `internal/apikey`

## Public API

- `Manager` — API key lifecycle: generation, validation, revocation, listing.
- `Key` — key metadata: KeyID, TokenHash, AgentID, CreatedAt, ExpiresAt.
- `NewManager(masterKey)` — creates a manager with a configured master key.
- `(*Manager) Generate(agentID, ttl)` — creates a cryptographically random 32-byte token, stores SHA-256 hash; returns raw token + metadata.
- `(*Manager) Validate(token)` — SHA-256 hashes the token and looks up by hash; checks expiry.
- `(*Manager) Revoke(keyID)` — removes a key by ID.
- `(*Manager) List(agentID)` — returns active (non-expired) keys, optionally filtered by agent.
- `ExtractBearer(authHeader)` — parses `Bearer <token>` from Authorization header.

## Conventions

- Key IDs use the `bk_` prefix (e.g., `bk_a1b2c3d4e5f6g7h8`).
- Tokens are 32 random bytes, base64-RawURL-encoded.
- Hashes are SHA-256, hex-encoded — never store raw tokens.
- Agent-scoped keys have a non-empty `AgentID`; top-level keys have empty `AgentID`.
- `List` filters out expired keys at call time (no background cleanup).

## Dependencies

- Standard library: `crypto/rand`, `crypto/sha256`, `encoding/base64`, `encoding/hex`, `sync`, `time`.

## Test Patterns

- Table-driven tests: missing master key, empty token, invalid token, expired key.
- `manager_test.go` verifies Generate→Validate round-trip, duplicate revoke, and List filtering by agent.
- Key ID format is tested via prefix check.

## Pitfalls

1. **Raw tokens are shown once.** `Generate` returns the raw token which must be displayed to the caller immediately — only the hash is stored. If the raw token is lost, there is no recovery; generate a new one.
2. **`Validate` is O(n).** It iterates all stored keys to find a matching hash. For large deployments (>10K keys), add an index.
3. **No persistence.** Keys live in memory only. On process restart, all keys are lost and must be regenerated.
4. **`ExtractBearer` is case-insensitive for "Bearer".** It uses `strings.EqualFold` — `bearer`, `BEARER`, `Bearer` all work. But it requires exactly one space after the scheme.
