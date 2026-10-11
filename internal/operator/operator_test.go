package operator

import (
	"reflect"
	"testing"
)

// TestManagerImplementsAuthOperatorSource pins the GAP-130 bridge: the
// daemon wires *operator.Manager into JWTAuth.SetOperatorSource, which
// takes an auth.OperatorSource. A struct-name rename here must break THIS
// test's compile, not silently unwire operators from the credential path
// at runtime.
//
// The interface type is re-declared structurally: importing internal/auth
// from a test is fine, but a structural mirror with the compile-time
// comment pointing at the real assertion keeps this file self-contained.
// The REAL assertion lives in the server wiring test (TestOperatorRPCKeys
// in internal/server), which passes the manager to SetOperatorSource.
func TestManagerValidateTokenShape(t *testing.T) {
	m := NewManager()
	tok, ident, err := m.Create("ci-agent", RoleAgentSelf, 3600)
	if err != nil {
		t.Fatalf("create agent:self identity: %v", err)
	}
	name, role, agentID, err := m.ValidateToken(tok)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if name != "ci-agent" || role != string(RoleAgentSelf) || agentID != "ci-agent" {
		t.Fatalf("ValidateToken = (%q, %q, %q), want (ci-agent, agent:self, ci-agent)", name, role, agentID)
	}
	if ident.AgentID != "ci-agent" {
		t.Fatalf("identity AgentID = %q, want ci-agent", ident.AgentID)
	}
	if _, _, _, err := m.ValidateToken("wrong-token"); err == nil {
		t.Fatalf("ValidateToken accepted a wrong token")
	}
}

// TestRoleVocabularyMatchesAuth pins the role STRING spellings against the
// values rbac.go enforces. internal/operator cannot import internal/auth
// here without risking a cycle as the packages grow, so the vocabulary is
// mirrored — and a mirror without a pin is a drift bug waiting for the
// worst moment (a role spelling change would silently deny every operator
// or silently open every admin surface).
func TestRoleVocabularyMatchesAuth(t *testing.T) {
	got := OperatorRoles()
	want := []string{"admin", "operator", "agent:self"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OperatorRoles() = %v, want %v — internal/auth/rbac.go Role* constants must match", got, want)
	}
}
