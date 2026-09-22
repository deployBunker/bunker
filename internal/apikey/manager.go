// Package apikey manages API key generation, validation, and storage.
// Supports top-level static keys and per-agent sub-keys.
//
// GAP-132: keys are durably persisted to a mode-0600 JSONL store under the
// registry directory (see store.go), so keys issued before a restart keep
// validating after one. Revoke marks a key revoked (immediately invalid,
// on disk); List reports metadata only.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Manager handles API key lifecycle.
type Manager struct {
	mu        sync.RWMutex
	masterKey string
	keys      map[string]*Key // keyID -> Key
	// store is the durable key store (nil = in-memory only; tests and
	// callers that have not migrated yet). Every mutation persists through
	// it; a persistence failure fails the mutation — an unpersisted key is
	// a credential a restart will not honor, which must never look issued.
	store *store
}

// Key holds metadata for a generated API key.
type Key struct {
	KeyID     string
	TokenHash string
	AgentID   string
	CreatedAt time.Time
	ExpiresAt time.Time
	// Revoked marks a key whose credentials stopped validating (GAP-132).
	// Revoked keys stay in the store so the revocation itself survives a
	// restart; they never validate again.
	Revoked bool
}

// NewManager creates an API key manager with the given master key.
// The key set is in-memory only — construct with NewManagerAt for durable
// persistence.
func NewManager(masterKey string) *Manager {
	return &Manager{
		masterKey: masterKey,
		keys:      make(map[string]*Key),
	}
}

// NewManagerAt creates an API key manager whose key set is persisted to
// <dir>/apikeys.jsonl (mode 0600, dir 0700; GAP-132). Keys issued by a
// previous manager on the same store validate after this construction, and
// every Generate/Revoke here is persisted before it reports success.
func NewManagerAt(masterKey, dir string) (*Manager, error) {
	st, err := newStore(dir)
	if err != nil {
		return nil, err
	}
	keys, err := st.load()
	if err != nil {
		return nil, err
	}
	return &Manager{
		masterKey: masterKey,
		keys:      keys,
		store:     st,
	}, nil
}

// Generate creates a new API key for the given agentID (empty for top-level).
// Returns the raw token (to be shown once) and the key metadata.
func (m *Manager) Generate(agentID string, ttl time.Duration) (token string, key *Key, err error) {
	if m.masterKey == "" {
		return "", nil, fmt.Errorf("master key not configured")
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("rand read: %w", err)
	}

	token = base64.RawURLEncoding.EncodeToString(raw)
	keyID := "bk_" + hex.EncodeToString(raw[:8])

	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	now := time.Now()
	key = &Key{
		KeyID:     keyID,
		TokenHash: tokenHash,
		AgentID:   agentID,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}

	m.mu.Lock()
	m.keys[keyID] = key
	perr := m.persistLocked()
	m.mu.Unlock()
	if perr != nil {
		// Roll the in-memory state back so memory and disk agree: an
		// operator must never hold a token the store does not know.
		m.mu.Lock()
		delete(m.keys, keyID)
		m.mu.Unlock()
		return "", nil, perr
	}

	return token, key, nil
}

// Validate checks if a token is valid and returns the associated key.
// Revoked keys never validate (GAP-132).
func (m *Manager) Validate(token string) (*Key, error) {
	if m.masterKey == "" {
		return nil, fmt.Errorf("master key not configured")
	}
	if token == "" {
		return nil, fmt.Errorf("token is empty")
	}

	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, key := range m.keys {
		if key.TokenHash == tokenHash {
			if key.Revoked {
				return nil, fmt.Errorf("key %s revoked", key.KeyID)
			}
			if time.Now().After(key.ExpiresAt) {
				return nil, fmt.Errorf("key %s expired", key.KeyID)
			}
			return key, nil
		}
	}
	return nil, fmt.Errorf("invalid token")
}

// Revoke marks the key keyID revoked: every credential issued under it
// stops validating immediately (GAP-132). The key's record (with the
// revoked marker) stays in the store so the revocation survives restarts.
func (m *Manager) Revoke(keyID string) error {
	m.mu.Lock()
	key, ok := m.keys[keyID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("key %s not found", keyID)
	}
	if key.Revoked {
		m.mu.Unlock()
		return fmt.Errorf("key %s not found", keyID)
	}
	key.Revoked = true
	perr := m.persistLocked()
	m.mu.Unlock()
	if perr != nil {
		// Roll back: an unrecorded revocation must not look done.
		m.mu.Lock()
		key.Revoked = false
		m.mu.Unlock()
		return perr
	}
	return nil
}

// List returns all active (non-revoked, unexpired) keys, optionally
// filtered by agentID.
func (m *Manager) List(agentID string) []*Key {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now()
	result := make([]*Key, 0, len(m.keys))
	for _, key := range m.keys {
		if key.Revoked || now.After(key.ExpiresAt) {
			continue
		}
		if agentID != "" && key.AgentID != agentID {
			continue
		}
		result = append(result, key)
	}
	return result
}

// persistLocked rewrites the store with the current key set. Called with
// m.mu held (write side). A nil store (NewManager) is a no-op.
func (m *Manager) persistLocked() error {
	if m.store == nil {
		return nil
	}
	return m.store.saveAllLocked(m.keys)
}

// ExtractBearer extracts the bearer token from an Authorization header value.
func ExtractBearer(authHeader string) (string, error) {
	if authHeader == "" {
		return "", fmt.Errorf("missing Authorization header")
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", fmt.Errorf("invalid Authorization header format, expected 'Bearer <token>'")
	}
	return parts[1], nil
}
