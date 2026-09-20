package agent

import (
	"os"
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// GAP-128: the spawn response must not carry private key material unless the
// caller explicitly opted in via SpawnAgentRequest.return_ssh_private_key.
// The key itself stays persisted server-side (SshPrivateKeyPath), so exec,
// reconcile and the GetAgentKey RPC all keep working unchanged.
//
// These tests need root for the same reason every Spawn test here does
// (useradd / isolation provisioning), so they skip under a non-root suite and
// are exercised by the root run.

// TestSpawn_DefaultResponseHasNoPrivateKey pins acceptance criterion 2: the
// default (flag-unset) spawn response carries an EMPTY SshPrivateKey while
// every other part of the connection bundle stays populated.
func TestSpawn_DefaultResponseHasNoPrivateKey(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("gap128")
	resp, err := m.Spawn(t.Context(), &v1.SpawnAgentRequest{AgentId: agentID})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer cleanupAgent(t, m, resp.AgentId)

	if resp.SshPrivateKey != "" {
		t.Errorf("default spawn response leaked %d bytes of private key material (GAP-128)", len(resp.SshPrivateKey))
	}
	// The rest of the bundle must stay intact.
	if resp.DockerHostSsh == "" {
		t.Error("DockerHostSsh is empty in the default response")
	}
	if resp.ExpiresAt == "" {
		t.Error("ExpiresAt is empty in the default response")
	}
	// The key must still be persisted server-side for the agent.
	rec := m.tracker.Get(resp.AgentId)
	if rec == nil {
		t.Fatalf("tracker record missing for %s", resp.AgentId)
	}
	if rec.SshPrivateKeyPath == "" {
		t.Fatal("SshPrivateKeyPath empty; the server-side key copy is gone")
	}
	raw, err := os.ReadFile(rec.SshPrivateKeyPath)
	if err != nil {
		t.Fatalf("read persisted key %s: %v", rec.SshPrivateKeyPath, err)
	}
	if !strings.HasPrefix(string(raw), "-----BEGIN") {
		t.Errorf("persisted key does not look like a PEM private key: %q", raw)
	}
}

// TestSpawn_OptInFlagStillReturnsPrivateKey pins the opt-in path: callers who
// set return_ssh_private_key=true keep receiving the key (old clients that
// opt in keep working).
func TestSpawn_OptInFlagStillReturnsPrivateKey(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("gap128opt")
	resp, err := m.Spawn(t.Context(), &v1.SpawnAgentRequest{
		AgentId:             agentID,
		ReturnSshPrivateKey: true,
	})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer cleanupAgent(t, m, resp.AgentId)

	if resp.SshPrivateKey == "" {
		t.Fatal("opt-in spawn response has an empty SshPrivateKey")
	}
	if !strings.HasPrefix(resp.SshPrivateKey, "-----BEGIN") {
		t.Errorf("opt-in key should start with '-----BEGIN', got: %.50s", resp.SshPrivateKey)
	}
}
