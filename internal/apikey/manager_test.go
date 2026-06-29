package apikey

import (
	"testing"
	"time"
)

func TestManager_Generate(t *testing.T) {
	m := NewManager("master-secret")

	token, key, err := m.Generate("agent-1", time.Hour)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if token == "" {
		t.Fatal("token should not be empty")
	}
	if key == nil {
		t.Fatal("key should not be nil")
	}
	if key.KeyID == "" {
		t.Fatal("keyID should not be empty")
	}
	if key.AgentID != "agent-1" {
		t.Fatalf("agentID = %q, want %q", key.AgentID, "agent-1")
	}
	if key.TokenHash == "" {
		t.Fatal("tokenHash should not be empty")
	}
	if time.Now().After(key.ExpiresAt) {
		t.Fatal("expiresAt should be in the future")
	}
}

func TestManager_Generate_NoMasterKey(t *testing.T) {
	m := NewManager("")
	_, _, err := m.Generate("agent-1", time.Hour)
	if err == nil {
		t.Fatal("expected error when master key is empty")
	}
}

func TestManager_Validate(t *testing.T) {
	m := NewManager("master-secret")
	token, key, err := m.Generate("agent-1", time.Hour)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	validated, err := m.Validate(token)
	if err != nil {
		t.Fatalf("validate token: %v", err)
	}
	if validated.KeyID != key.KeyID {
		t.Fatalf("keyID = %q, want %q", validated.KeyID, key.KeyID)
	}
}

func TestManager_Validate_Invalid(t *testing.T) {
	m := NewManager("master-secret")
	_, err := m.Validate("invalid-token")
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestManager_Validate_Empty(t *testing.T) {
	m := NewManager("master-secret")
	_, err := m.Validate("")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestManager_Validate_Expired(t *testing.T) {
	m := NewManager("master-secret")
	token, _, err := m.Generate("agent-1", -time.Hour) // already expired
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	_, err = m.Validate(token)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestManager_Revoke(t *testing.T) {
	m := NewManager("master-secret")
	_, key, err := m.Generate("agent-1", time.Hour)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	if err := m.Revoke(key.KeyID); err != nil {
		t.Fatalf("revoke key: %v", err)
	}

	err = m.Revoke(key.KeyID)
	if err == nil {
		t.Fatal("expected error revoking already-revoked key")
	}
}

func TestManager_List(t *testing.T) {
	m := NewManager("master-secret")

	// Generate two keys for different agents
	_, k1, err := m.Generate("agent-1", time.Hour)
	if err != nil {
		t.Fatalf("generate key 1: %v", err)
	}
	_, k2, err := m.Generate("agent-2", time.Hour)
	if err != nil {
		t.Fatalf("generate key 2: %v", err)
	}
	// Generate an expired key (should not appear in list)
	_, _, err = m.Generate("agent-3", -time.Hour)
	if err != nil {
		t.Fatalf("generate expired key: %v", err)
	}

	all := m.List("")
	if len(all) != 2 {
		t.Fatalf("len(all) = %d, want 2", len(all))
	}

	ids := make(map[string]bool)
	for _, k := range all {
		ids[k.KeyID] = true
	}
	if !ids[k1.KeyID] {
		t.Fatalf("expected k1 in list")
	}
	if !ids[k2.KeyID] {
		t.Fatalf("expected k2 in list")
	}

	agent1Only := m.List("agent-1")
	if len(agent1Only) != 1 {
		t.Fatalf("len(agent1Only) = %d, want 1", len(agent1Only))
	}
	if agent1Only[0].KeyID != k1.KeyID {
		t.Fatalf("agent1Only[0].KeyID = %q, want %q", agent1Only[0].KeyID, k1.KeyID)
	}
}

func TestExtractBearer(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"valid", "Bearer abc123", "abc123", false},
		{"missing header", "", "", true},
		{"wrong scheme", "Basic abc123", "", true},
		{"no space", "Bearerabc123", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractBearer(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ExtractBearer(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ExtractBearer(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
