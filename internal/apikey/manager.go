// Package apikey manages API key generation, validation, and storage.
// Supports top-level static keys and per-agent sub-keys.
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
}

// Key holds metadata for a generated API key.
type Key struct {
	KeyID     string
	TokenHash string
	AgentID   string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// NewManager creates an API key manager with the given master key.
func NewManager(masterKey string) *Manager {
	return &Manager{
		masterKey: masterKey,
		keys:      make(map[string]*Key),
	}
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
	m.mu.Unlock()

	return token, key, nil
}

// Validate checks if a token is valid and returns the associated key.
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
			if time.Now().After(key.ExpiresAt) {
				return nil, fmt.Errorf("key %s expired", key.KeyID)
			}
			return key, nil
		}
	}
	return nil, fmt.Errorf("invalid token")
}

// Revoke removes a key by its keyID.
func (m *Manager) Revoke(keyID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.keys[keyID]; !ok {
		return fmt.Errorf("key %s not found", keyID)
	}
	delete(m.keys, keyID)
	return nil
}

// List returns all active keys, optionally filtered by agentID.
func (m *Manager) List(agentID string) []*Key {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now()
	result := make([]*Key, 0, len(m.keys))
	for _, key := range m.keys {
		if now.After(key.ExpiresAt) {
			continue
		}
		if agentID != "" && key.AgentID != agentID {
			continue
		}
		result = append(result, key)
	}
	return result
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
