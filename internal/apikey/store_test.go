// store_test.go — GAP-132 acceptance coverage for the durable key store:
// persistence across a simulated restart and revocation that survives it.
package apikey

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testMasterKey = "gap132-store-test-master-key-32-bytes!!"

// TestManagerAt_PersistsKeysAcrossRestart covers review criterion (1): a
// manager reconstructed from the same store directory still validates keys
// issued before the simulated restart.
func TestManagerAt_PersistsKeysAcrossRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")

	first, err := NewManagerAt(testMasterKey, dir)
	if err != nil {
		t.Fatalf("first manager: %v", err)
	}
	token, _, err := first.Generate("agent-a", time.Hour)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := first.Validate(token); err != nil {
		t.Fatalf("validate before restart: %v", err)
	}

	// Simulated restart: a brand-new manager over the SAME directory.
	second, err := NewManagerAt(testMasterKey, dir)
	if err != nil {
		t.Fatalf("second manager: %v", err)
	}
	got, err := second.Validate(token)
	if err != nil {
		t.Fatalf("key issued before restart must validate after it: %v", err)
	}
	if got.AgentID != "agent-a" {
		t.Fatalf("agent_id = %q, want agent-a", got.AgentID)
	}
}

// TestManagerAt_RevocationSurvivesRestart covers review criteria (1)+(2):
// Revoke invalidates a live token immediately AND the revoked marker is
// persisted (revoked=true, not deleted), so the token stays dead after a
// restart and the record remains in the store file.
func TestManagerAt_RevocationSurvivesRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")

	first, err := NewManagerAt(testMasterKey, dir)
	if err != nil {
		t.Fatalf("first manager: %v", err)
	}
	token, key, err := first.Generate("agent-a", time.Hour)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := first.Revoke(key.KeyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := first.Validate(token); err == nil {
		t.Fatal("revoked token must stop validating immediately")
	}

	// Restart over the same directory.
	second, err := NewManagerAt(testMasterKey, dir)
	if err != nil {
		t.Fatalf("second manager: %v", err)
	}
	if _, err := second.Validate(token); err == nil {
		t.Fatal("revocation must survive a restart")
	}
	// The record stays on disk with revoked=true (mark, not delete).
	raw, err := os.ReadFile(filepath.Join(dir, StoreFileName))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if !containsRevokedRecord(string(raw), key.KeyID) {
		t.Fatalf("store file must keep the revoked record for %s:\n%s", key.KeyID, raw)
	}
}

// TestManagerAt_StoreFileModes pins the 0600/0700 posture: credentials must
// never land world-readable.
func TestManagerAt_StoreFileModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	m, err := NewManagerAt(testMasterKey, dir)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	if _, _, err := m.Generate("agent-a", time.Hour); err != nil {
		t.Fatalf("generate: %v", err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("store dir mode = %#o, want 0700", di.Mode().Perm())
	}
	fi, err := os.Stat(filepath.Join(dir, StoreFileName))
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("store file mode = %#o, want 0600", fi.Mode().Perm())
	}
}

// containsRevokedRecord reports whether the raw JSONL contains a record for
// keyID whose revoked flag is true.
func containsRevokedRecord(raw, keyID string) bool {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec keyRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.KeyID == keyID && rec.Revoked {
			return true
		}
	}
	return false
}
