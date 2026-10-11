package operator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseRole(t *testing.T) {
	for _, valid := range []string{"admin", "operator", "agent:self"} {
		if _, err := ParseRole(valid); err != nil {
			t.Errorf("ParseRole(%q) = %v, want nil", valid, err)
		}
	}
	for _, invalid := range []string{"", "Admin", "root", "agent", "agent:self:", "self"} {
		if _, err := ParseRole(invalid); err == nil {
			t.Errorf("ParseRole(%q) = nil error, want invalid", invalid)
		}
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"ci", "ci-bot", "Agent_1", "b.kelly", "a-b_c.d9"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "has space", "slash/ed", "co:loned", strings.Repeat("x", 65)} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) = nil error, want invalid", bad)
		}
	}
}

func TestCreateReturnsTokenOnceAndStoresHash(t *testing.T) {
	m := NewManager()
	tok, ident, err := m.Create("kelly", RoleOperator, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if tok == "" {
		t.Fatal("token is empty")
	}
	if ident.TokenHash == tok || strings.Contains(ident.TokenHash, tok) {
		t.Fatal("registry identity carries raw token material")
	}
	if len(ident.TokenHash) != 64 { // sha256 hex
		t.Fatalf("token hash length = %d, want 64 (sha256 hex)", len(ident.TokenHash))
	}
	if !ident.ExpiresAt.After(time.Now()) {
		t.Fatal("identity already expired at creation")
	}
	// Default TTL ≈ 30 days.
	if want := time.Now().Add(DefaultTTL); ident.ExpiresAt.Sub(want) > time.Minute || want.Sub(ident.ExpiresAt) > time.Minute {
		t.Fatalf("default TTL: expires %v, want ~%v", ident.ExpiresAt, want)
	}

	// Duplicate name refuses.
	if _, _, err := m.Create("kelly", RoleAdmin, 0); err == nil {
		t.Fatal("duplicate create succeeded, want already-exists error")
	}
}

func TestAgentSelfIdentityBindsNameAsAgentID(t *testing.T) {
	m := NewManager()
	_, ident, err := m.Create("agent-77", RoleAgentSelf, 0)
	if err != nil {
		t.Fatalf("create agent:self: %v", err)
	}
	if ident.AgentID != "agent-77" {
		t.Fatalf("agent:self identity AgentID = %q, want agent-77", ident.AgentID)
	}
	// Non-agent roles carry no agent binding.
	_, ident2, err := m.Create("kelly", RoleOperator, 0)
	if err != nil {
		t.Fatalf("create operator: %v", err)
	}
	if ident2.AgentID != "" {
		t.Fatalf("operator identity AgentID = %q, want empty", ident2.AgentID)
	}
}

func TestValidateAndDelete(t *testing.T) {
	m := NewManager()
	tok, _, err := m.Create("kelly", RoleAdmin, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ident, err := m.Validate(tok)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if ident.Role != RoleAdmin {
		t.Fatalf("validated role = %q, want admin", ident.Role)
	}
	if _, err := m.Validate("wrong"); err == nil {
		t.Fatal("validate accepted a wrong token")
	}
	if err := m.Delete("kelly"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := m.Validate(tok); err == nil {
		t.Fatal("deleted operator token still validates")
	}
	if err := m.Delete("kelly"); err == nil {
		t.Fatal("second delete succeeded, want not-found")
	}
}

func TestExpiredTokenDoesNotValidate(t *testing.T) {
	m := NewManager()
	tok, _, err := m.Create("ghost", RoleOperator, 1) // 1 second
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.Validate(tok); err != nil {
		t.Fatalf("fresh token rejected: %v", err)
	}
	// Force expiry by rewriting the registry.
	m.mu.Lock()
	m.identites["ghost"].ExpiresAt = time.Now().Add(-time.Minute)
	m.mu.Unlock()
	if _, err := m.Validate(tok); err == nil {
		t.Fatal("expired token validated")
	}
}

func TestListSortedAndFiltered(t *testing.T) {
	m := NewManager()
	for _, c := range []struct {
		name string
		role Role
	}{
		{"zeta", RoleAdmin}, {"alpha", RoleOperator}, {"agent-9", RoleAgentSelf}, {"agent-1", RoleAgentSelf},
	} {
		if _, _, err := m.Create(c.name, c.role, 0); err != nil {
			t.Fatalf("create %s: %v", c.name, err)
		}
	}
	all := m.List("")
	if len(all) != 4 {
		t.Fatalf("List() = %d identities, want 4", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Name > all[i].Name {
			t.Fatalf("List() not sorted: %q before %q", all[i-1].Name, all[i].Name)
		}
	}
	only := m.List("agent-9")
	if len(only) != 1 || only[0].Name != "agent-9" {
		t.Fatalf("List(agent-9) = %v, want [agent-9]", only)
	}
	// Mutating a copy must not touch the registry.
	only[0].Role = RoleAdmin
	if got := m.List("agent-9")[0].Role; got != RoleAgentSelf {
		t.Fatalf("List returned a live reference (role mutated to %v)", got)
	}
}

func TestDurableStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	m1, err := NewManagerAt(dir)
	if err != nil {
		t.Fatalf("NewManagerAt: %v", err)
	}
	tok, _, err := m1.Create("kelly", RoleOperator, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := m1.Create("agent-5", RoleAgentSelf, 0); err != nil {
		t.Fatalf("create agent:self: %v", err)
	}
	if err := m1.Delete("kelly"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// A NEW manager on the same directory is the restart.
	m2, err := NewManagerAt(dir)
	if err != nil {
		t.Fatalf("restart NewManagerAt: %v", err)
	}
	if _, err := m2.Validate(tok); err == nil {
		t.Fatal("deleted operator survived the restart")
	}
	if got := m2.List(""); len(got) != 1 || got[0].Name != "agent-5" || got[0].Role != RoleAgentSelf {
		t.Fatalf("restart registry = %+v, want [agent-5 agent:self]", got)
	}
	if _, _, err := m2.Create("kelly", RoleAdmin, 0); err != nil {
		t.Fatalf("recreate deleted name: %v", err)
	}
}

func TestStoreFilePermissions(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManagerAt(dir)
	if err != nil {
		t.Fatalf("NewManagerAt: %v", err)
	}
	if _, _, err := m.Create("kelly", RoleAdmin, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, StoreFileName))
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}
	if got := fi.Mode().Perm(); got != FileMode {
		t.Fatalf("store mode = %#o, want %#o", got, FileMode)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != DirMode {
		t.Fatalf("store dir mode = %#o, want %#o", got, DirMode)
	}
}

func TestStoreRejectsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, StoreFileName), []byte("{not json}\n"), FileMode); err != nil {
		t.Fatalf("seed malformed store: %v", err)
	}
	if _, err := NewManagerAt(dir); err == nil {
		t.Fatal("NewManagerAt accepted a malformed store, want hard error")
	}
}

func TestStoreRecordShapeIsStableJSON(t *testing.T) {
	m := NewManager()
	tok, ident, err := m.Create("kelly", RoleOperator, 3600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, err := json.Marshal(ident)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"name", "role", "token_hash", "created_at", "expires_at"} {
		if _, ok := probe[key]; !ok {
			t.Errorf("store record missing stable key %q: %s", key, b)
		}
	}
	if _, ok := probe["agent_id"]; ok {
		t.Errorf("non-agent identity must omit agent_id (omitempty): %s", b)
	}
	if strings.Contains(string(b), tok) {
		t.Fatal("store record carries raw token material")
	}
}
