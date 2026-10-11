// Package operator implements per-operator identities with RBAC roles
// (GAP-130): admin, operator, agent:self.
//
// Before GAP-130 a bunker daemon had ONE credential class — the master
// token (plus the JWT/sub-key machinery built on top of it) — so every
// automation and every human shared the same all-powerful secret. An
// operator identity is a NAMED credential with an explicit role:
//
//   - RoleAdmin     — the master token's behavior: everything, including
//     managing other operators;
//   - RoleOperator  — agent lifecycle (spawn/destroy/exec/...) but NOT
//     operator management and NOT system configuration;
//     operator-management and config RPCs are denied with
//     CodePermissionDenied;
//   - RoleAgentSelf — a machine identity bound to ONE agent: it can only
//     operate on its own agent (the Agent service's ownership guards
//     enforce this; the Bunkerd service's master-only interceptor rejects
//     the credential outright).
//
// The token itself is shown ONCE at creation; the registry stores only a
// SHA-256 hash, exactly like the GAP-132 apikey store. The registry is a
// durable JSONL file (mode 0600 in a 0700 directory) rewritten atomically
// on every mutation — the same crash-safety shape as apikey/store.go — so
// identities survive daemon restarts. Deletion removes the record outright
// (unlike apikey revocation there is no replay ambiguity: a deleted
// operator must never reappear, and last-event-wins over a create replay
// is enforced by keeping the in-memory map the source of truth for
// saveAll rewrites).
package operator

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Role is the RBAC role of an operator identity.
type Role string

const (
	// RoleAdmin allows everything the master credential can do, including
	// managing other operators (OperatorCreate/List/Delete) and system
	// configuration RPCs.
	RoleAdmin Role = "admin"
	// RoleOperator allows the agent-lifecycle RPC surface (spawn, destroy,
	// exec, run, list, metrics, stop/start/restart, renew, heartbeat) and
	// read-only observability. Operator management and system-configuration
	// RPCs are denied.
	RoleOperator Role = "operator"
	// RoleAgentSelf is a machine identity scoped to a single agent: it may
	// operate on its OWN agent only. The name of the identity IS the agent
	// id it is bound to.
	RoleAgentSelf Role = "agent:self"
)

// OperatorRoles is the exhaustive set of valid roles, for validation and
// help text. Keep it in sync with the OperatorRole values above.
func OperatorRoles() []string {
	return []string{string(RoleAdmin), string(RoleOperator), string(RoleAgentSelf)}
}

// ParseRole validates a wire/config role string and returns the Role value.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleAdmin, RoleOperator, RoleAgentSelf:
		return Role(s), nil
	}
	return "", fmt.Errorf("invalid role %q (valid roles: %s)", s, strings.Join(OperatorRoles(), ", "))
}

// DefaultTTL is the credential lifetime applied when an OperatorCreate
// request does not name one: 30 days, long enough for real automation,
// short enough that a leaked token expires on its own.
const DefaultTTL = 30 * 24 * time.Hour

// Identity is one operator identity: a stable name, a role, and the agent
// binding for agent:self identities. Secret material never lives here —
// only TokenHash.
type Identity struct {
	Name      string    `json:"name"`
	Role      Role      `json:"role"`
	AgentID   string    `json:"agent_id,omitempty"` // non-empty for RoleAgentSelf
	TokenHash string    `json:"token_hash"`         // sha256 hex of the bearer token
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Manager holds the operator registry: the in-memory identity set plus the
// durable JSONL store backing it. All mutations persist before reporting
// success — an identity a restart would forget must never look created.
type Manager struct {
	mu        sync.RWMutex
	identites map[string]*Identity // name -> identity
	store     *store
}

// StoreFileName is the operator registry file created inside the store
// directory.
const StoreFileName = "operators.jsonl"

// DirMode is the mode of the directory holding the registry (owner only).
const DirMode os.FileMode = 0o700

// FileMode is the mode of the registry file itself (owner read/write).
const FileMode os.FileMode = 0o600

// NewManager creates an in-memory-only manager (tests and callers that have
// not migrated yet).
func NewManager() *Manager {
	return &Manager{identites: make(map[string]*Identity)}
}

// NewManagerAt creates a manager persisted to <dir>/operators.jsonl (mode
// 0600, dir 0700). Identities created by a previous manager on the same
// store still authenticate after this construction.
func NewManagerAt(dir string) (*Manager, error) {
	st, err := newStore(dir)
	if err != nil {
		return nil, err
	}
	idents, err := st.load()
	if err != nil {
		return nil, err
	}
	return &Manager{identites: idents, store: st}, nil
}

// ValidateName enforces the operator-name grammar: 1..64 chars from
// [a-zA-Z0-9._-]. agent:self identities use the agent id as the name, and
// agent ids are what appear in audit records, so the grammar keeps names
// unambiguous in the trail and safe as filenames/labels.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("operator name is required")
	}
	if len(name) > 64 {
		return fmt.Errorf("operator name %q is longer than 64 characters", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("operator name %q contains invalid character %q (allowed: [a-zA-Z0-9._-])", name, r)
		}
	}
	return nil
}

// Create provisions a new operator identity and returns its bearer token —
// shown EXACTLY ONCE; only the SHA-256 hash is stored.
//
// For RoleAgentSelf, name IS the agent id the identity is bound to (the
// claims will carry that AgentID, so every ownership guard in the Agent
// service treats the token as scoped to exactly that agent). For the other
// roles agentID stays empty. ttlSeconds bounds the credential (0 =
// DefaultTTL). A persistence failure rolls the in-memory state back — an
// operator must never hold a token the registry does not know.
func (m *Manager) Create(name string, role Role, ttlSeconds uint32) (token string, ident *Identity, err error) {
	if err := ValidateName(name); err != nil {
		return "", nil, err
	}
	if ttlSeconds == 0 {
		ttlSeconds = uint32(DefaultTTL / time.Second)
	}
	ttl := time.Duration(ttlSeconds) * time.Second
	if ttl <= 0 {
		return "", nil, fmt.Errorf("operator ttl must be positive")
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("rand read: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))

	now := time.Now()
	ident = &Identity{
		Name:      name,
		Role:      role,
		TokenHash: hex.EncodeToString(hash[:]),
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	if role == RoleAgentSelf {
		ident.AgentID = name
	}

	m.mu.Lock()
	if _, exists := m.identites[name]; exists {
		m.mu.Unlock()
		return "", nil, fmt.Errorf("operator %q already exists", name)
	}
	m.identites[name] = ident
	perr := m.persistLocked()
	m.mu.Unlock()
	if perr != nil {
		m.mu.Lock()
		delete(m.identites, name)
		m.mu.Unlock()
		return "", nil, perr
	}
	return token, ident, nil
}

// Delete removes an operator identity immediately: its token stops
// validating before this returns, and the removal is persisted so it
// survives a restart.
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	ident, ok := m.identites[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("operator %q not found", name)
	}
	_ = ident
	delete(m.identites, name)
	perr := m.persistLocked()
	m.mu.Unlock()
	if perr != nil {
		// Roll back: an unrecorded deletion must not look done — the
		// operator's token would keep working while the caller believes it
		// destroyed.
		m.mu.Lock()
		m.identites[name] = ident
		m.mu.Unlock()
		return perr
	}
	return nil
}

// List returns identity metadata sorted by name, optionally filtered to the
// operator bound to one agent (agentID != ""). The returned slice is a copy;
// callers cannot mutate the registry through it.
func (m *Manager) List(agentID string) []*Identity {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Identity, 0, len(m.identites))
	for _, ident := range m.identites {
		if agentID != "" && ident.AgentID != agentID {
			continue
		}
		cp := *ident
		out = append(out, &cp)
	}
	// Stable order so list output (and its tests) do not depend on map
	// iteration order.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Name < out[j-1].Name; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Validate checks a presented bearer token against the registry. On success
// it returns a copy of the identity. Expired credentials do not validate —
// an expired identity remains listed until deleted, exactly like the
// apikey store's expired keys.
func (m *Manager) Validate(token string) (*Identity, error) {
	if token == "" {
		return nil, fmt.Errorf("token is empty")
	}
	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])

	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, ident := range m.identites {
		// REV-BUNKER-007 discipline: constant-time compare of the digest,
		// consistent with internal/auth and internal/apikey.
		if subtle.ConstantTimeCompare([]byte(ident.TokenHash), []byte(tokenHash)) == 1 {
			if time.Now().After(ident.ExpiresAt) {
				return nil, fmt.Errorf("operator %s expired", ident.Name)
			}
			cp := *ident
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("invalid token")
}

// persistLocked rewrites the store to mirror the in-memory set. Called with
// m.mu held.
func (m *Manager) persistLocked() error {
	if m.store == nil {
		return nil
	}
	return m.store.saveAll(m.identites)
}

// store persists and replays the identity set — the apikey/store.go shape:
// one JSONL file, mode 0600, atomic temp+rename rewrite on every mutation.
type store struct {
	mu   sync.Mutex
	path string
}

func newStore(dir string) (*store, error) {
	if dir == "" {
		return nil, fmt.Errorf("operator store: directory is required")
	}
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return nil, fmt.Errorf("operator store: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, DirMode); err != nil {
		return nil, fmt.Errorf("operator store: set %s mode %#o: %w", dir, DirMode, err)
	}
	return &store{path: filepath.Join(dir, StoreFileName)}, nil
}

// load replays the store into a fresh identity map. A missing file is an
// empty registry, not an error. A malformed line is a hard error: silently
// dropping a line could resurrect or lose live credentials.
func (s *store) load() (map[string]*Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idents := make(map[string]*Identity)
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return idents, nil
		}
		return nil, fmt.Errorf("operator store: open %s: %w", s.path, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ident Identity
		if err := json.Unmarshal(line, &ident); err != nil {
			return nil, fmt.Errorf("operator store: %s line %d: %w", s.path, lineNo, err)
		}
		idents[ident.Name] = &ident
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("operator store: read %s: %w", s.path, err)
	}
	return idents, nil
}

// saveAll rewrites the store atomically (temp file + rename) so a crash can
// never leave a truncated registry that would silently drop identities.
func (s *store) saveAll(idents map[string]*Identity) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, FileMode)
	if err != nil {
		return fmt.Errorf("operator store: create %s: %w", tmp, err)
	}
	w := bufio.NewWriter(f)
	for _, ident := range idents {
		b, err := json.Marshal(ident)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("operator store: encode operator %s: %w", ident.Name, err)
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("operator store: write %s: %w", tmp, err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("operator store: flush %s: %w", tmp, err)
	}
	// The file is created 0600, but tighten explicitly so a registry that
	// replaced a pre-existing wider-mode file cannot inherit its mode.
	if err := f.Chmod(FileMode); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("operator store: chmod %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("operator store: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("operator store: rename %s -> %s: %w", tmp, s.path, err)
	}
	return nil
}
