package agent

// DF-BUNKER-24 regression tests: orphan persisted agent private keys.
//
// Destroy removes the agent's persisted private key (`cfg.Agent.SSHDir/<id>`)
// as its last filesystem step, but two cleanup paths concluded the agent was
// already gone and returned BEFORE that step, leaving the key on disk
// forever:
//
//  1. the non-force early returns in Destroy (idempotent destroy of a known
//     agent, and not_found for an unknown one) — the path the TTL reaper
//     takes whenever the agent's system user no longer exists;
//  2. the startup reconcile PURGE of a registry record whose system user is
//     gone: it dropped the durable record and nothing else.
//
// Every test here falsifies against that old behaviour (the key file is
// still present after cleanup). They also pin the scope rule: cleanup may
// only ever touch `cfg.Agent.SSHDir/<managed agent id>` — foreign orphans,
// unmanaged IDs and a failed host enumeration keep their key material.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/resource"
)

const testKeyMaterial = "-----BEGIN OPENSSH PRIVATE KEY-----\nbunker-test-key\n-----END OPENSSH PRIVATE KEY-----\n"

// writeAgentKey writes an agent's persisted private key at the production
// path (cfg.Agent.SSHDir/<agent-id>) and returns that path.
func writeAgentKey(t *testing.T, sshDir, agentID string) string {
	t.Helper()
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("mkdir ssh dir: %v", err)
	}
	path := filepath.Join(sshDir, agentID)
	if err := os.WriteFile(path, []byte(testKeyMaterial), 0o600); err != nil {
		t.Fatalf("write agent key: %v", err)
	}
	return path
}

// keyExists reports whether path is still on disk.
func keyExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true
	case os.IsNotExist(err):
		return false
	default:
		t.Fatalf("stat %s: %v", path, err)
		return false
	}
}

// newKeyManager returns a manager whose SSH key directory is a temp dir, so
// no test ever reads or writes the host's real /etc/bunkerd/ssh.
func newKeyManager(t *testing.T) (*AgentManager, string) {
	t.Helper()
	m := newTestManager(t)
	sshDir := t.TempDir()
	m.cfg.Agent.SSHDir = sshDir
	return m, sshDir
}

// TestTTLReaper_RemovesExpiredAgentKey is AC1: an expired agent's persisted
// key is gone after the reaper runs, on BOTH Destroy outcomes the reaper can
// hit (the registry knows the agent → idempotent "destroyed"; it does not →
// "not_found"). Both are non-force and both used to return before the key
// cleanup. The second reap pins idempotency.
func TestTTLReaper_RemovesExpiredAgentKey(t *testing.T) {
	cases := []struct {
		name  string
		known bool
	}{
		{name: "registry knows the agent", known: true},
		{name: "registry does not know the agent", known: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, sshDir := newKeyManager(t)
			defer m.Stop()

			id := uniqueAgentID("ttlkey")
			keyPath := writeAgentKey(t, sshDir, id)
			// A second agent's key must survive: cleanup is per-agent.
			otherKey := writeAgentKey(t, sshDir, uniqueAgentID("ttlkeyother"))

			if tc.known {
				if err := m.persistSpawn(&resource.AgentRecord{
					AgentID: id, Status: "running", CreatedAt: time.Now().Add(-2 * time.Hour),
					ExpiresAt: time.Now().Add(-time.Minute), SshPrivateKeyPath: keyPath,
				}); err != nil {
					t.Fatalf("persistSpawn: %v", err)
				}
			}
			if err := m.tracker.Register(&resource.AgentRecord{
				AgentID: id, Status: "running", CreatedAt: time.Now().Add(-2 * time.Hour),
				ExpiresAt: time.Now().Add(-time.Minute), SshPrivateKeyPath: keyPath,
			}); err != nil {
				t.Fatalf("register agent: %v", err)
			}

			m.reapExpiredAgents()

			if m.tracker.Get(id) != nil {
				t.Errorf("expired agent %q is still tracked", id)
			}
			if keyExists(t, keyPath) {
				t.Errorf("TTL expiry left the agent's persisted SSH key behind at %s (DF-BUNKER-24)", keyPath)
			}
			if !keyExists(t, otherKey) {
				t.Errorf("TTL cleanup of %q removed the key of an unrelated agent at %s", id, otherKey)
			}

			// The reaper retries on the next tick: a repeat reap must stay
			// silent and idempotent (no error, nothing recreated).
			m.reapExpiredAgents()
			if keyExists(t, keyPath) {
				t.Errorf("repeated TTL cleanup recreated %s", keyPath)
			}
		})
	}
}

// TestDestroy_NonForceEarlyReturnsRemoveManagedKey pins the two Destroy
// outcomes the TTL reaper relies on — including their status contract, which
// must not change — and proves each one removes the managed key (AC1/AC4).
func TestDestroy_NonForceEarlyReturnsRemoveManagedKey(t *testing.T) {
	cases := []struct {
		name     string
		known    bool
		wantStat string
		wantErr  bool
	}{
		{name: "known agent, user already gone", known: true, wantStat: "destroyed", wantErr: false},
		{name: "unknown agent, user already gone", known: false, wantStat: "not_found", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, sshDir := newKeyManager(t)
			defer m.Stop()

			id := uniqueAgentID("destroykey")
			keyPath := writeAgentKey(t, sshDir, id)
			if tc.known {
				if err := m.persistSpawn(&resource.AgentRecord{
					AgentID: id, Status: "running", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
				}); err != nil {
					t.Fatalf("persistSpawn: %v", err)
				}
			}

			// No system user for the ID exists: userdel fails and the
			// non-force path returns early.
			resp, err := m.Destroy(context.Background(), id, false)
			if tc.wantErr && err == nil {
				t.Fatalf("destroy of an unknown absent agent must still error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("destroy of a known absent agent must succeed idempotently: %v", err)
			}
			if resp == nil || resp.Status != tc.wantStat {
				t.Fatalf("status = %v, want %q", resp, tc.wantStat)
			}
			if keyExists(t, keyPath) {
				t.Errorf("non-force destroy returned %q with the key still at %s (DF-BUNKER-24)", tc.wantStat, keyPath)
			}
		})
	}
}

// TestReconcile_StaleRecordRemovesManagedKey is AC2: the startup reconcile
// purge of a registry record whose system user is gone now also removes that
// agent's managed key — and only that one.
func TestReconcile_StaleRecordRemovesManagedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	defer m.registry.Close()
	sshDir := t.TempDir()
	m.cfg.Agent.SSHDir = sshDir

	staleKey := writeAgentKey(t, sshDir, "stale-key")
	liveKey := writeAgentKey(t, sshDir, "live-key")

	if err := m.persistSpawn(&resource.AgentRecord{
		AgentID: "stale-key", Status: "running", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		PortRangeStart: 11000, PortRangeEnd: 11099,
	}); err != nil {
		t.Fatalf("persistSpawn stale: %v", err)
	}
	if err := m.persistSpawn(&resource.AgentRecord{
		AgentID: "live-key", Status: "running", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		PortRangeStart: 10000, PortRangeEnd: 10099,
	}); err != nil {
		t.Fatalf("persistSpawn live: %v", err)
	}

	// Only live-key still has a system user on the host: stale-key's record
	// is the purge case, and its user is gone.
	m.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: "live-key", Username: "bunker-live-key", Home: "/home/bunker-live-key"}}, nil
	}

	rep := m.Reconcile(context.Background())
	if rep.Purged != 1 {
		t.Fatalf("Purged = %d, want 1 (report %+v)", rep.Purged, rep)
	}
	if keyExists(t, staleKey) {
		t.Errorf("startup reconcile purge left the managed SSH key at %s (DF-BUNKER-24)", staleKey)
	}
	if !keyExists(t, liveKey) {
		t.Errorf("reconcile removed the key of an agent still present on the host: %s", liveKey)
	}
}

// TestReconcile_ForeignOrphanKeepsItsKey is AC3: reconciliation leaves a
// foreign orphan — another daemon instance's agent — completely alone,
// including its persisted key material.
func TestReconcile_ForeignOrphanKeepsItsKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	var buf bytes.Buffer
	m, rec := foreignManager(t, path, false, &buf)
	defer m.registry.Close()
	sshDir := t.TempDir()
	m.cfg.Agent.SSHDir = sshDir

	home := t.TempDir()
	// 30000-30099 lies outside this daemon's pool: the agent is foreign.
	writePortMetadata(t, home, "30000-30099\n")
	foreignKey := writeAgentKey(t, sshDir, "foreign-agent")
	m.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: "foreign-agent", Username: "bunker-foreign-agent", Home: home}}, nil
	}

	rep := m.Reconcile(context.Background())
	if rep.Foreign != 1 {
		t.Fatalf("Foreign = %d, want 1 (report %+v)", rep.Foreign, rep)
	}
	if got := rec.calls(); len(got) != 0 {
		t.Errorf("destroy calls = %v, want 0 for a foreign orphan", got)
	}
	if !keyExists(t, foreignKey) {
		t.Errorf("foreign agent's key was removed: %s (DF-BUNKER-24)", foreignKey)
	}
}

// TestReconcile_ProbeFailureKeepsManagedKey pins the non-destructive rule for
// a failed host enumeration: the daemon cannot tell a stale record from a
// live one, so it must not purge records AND must not sweep keys.
func TestReconcile_ProbeFailureKeepsManagedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	defer m.registry.Close()
	sshDir := t.TempDir()
	m.cfg.Agent.SSHDir = sshDir

	const id = "unreadable-host"
	keyPath := writeAgentKey(t, sshDir, id)
	if err := m.persistSpawn(&resource.AgentRecord{
		AgentID: id, Status: "running", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("persistSpawn: %v", err)
	}
	m.listSystemAgents = func() ([]SystemAgent, error) { return nil, fmt.Errorf("passwd unreadable") }

	rep := m.Reconcile(context.Background())
	if rep.Purged != 0 {
		t.Fatalf("Purged = %d, want 0 on a failed probe (report %+v)", rep.Purged, rep)
	}
	if !keyExists(t, keyPath) {
		t.Errorf("failed host enumeration swept the managed key at %s", keyPath)
	}
}

// writeKeyAt writes a file at an explicit path, creating parents, so a scope
// test can populate every path a buggy path derivation could reach.
func writeKeyAt(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(testKeyMaterial), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestRemoveAgentSSHKeyScope pins the helper's boundary. Refused ids remove
// NOTHING — every path a buggy derivation could reach (the SSH dir's sibling
// for a traversal, a nested path for a slash) is populated first and must
// survive; a managed id removes exactly its own key.
func TestRemoveAgentSSHKeyScope(t *testing.T) {
	refused := []struct {
		name    string
		agentID string
	}{
		{name: "empty id", agentID: ""},
		{name: "uppercase id", agentID: "AgentOne"},
		{name: "underscore id", agentID: "agent_one"},
		{name: "dot id", agentID: "agent.one"},
		{name: "traversal id", agentID: "../outside-key"},
		{name: "slash id", agentID: "agent/one"},
		{name: "absolute id", agentID: "/etc/passwd"},
		{name: "over-long id", agentID: strings.Repeat("a", 64)},
	}

	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			m, sshDir := newKeyManager(t)
			defer m.Stop()

			// One file per path a wrong derivation would target, plus the
			// SSH dir's sibling: all of them must still exist afterwards.
			targets := []string{
				filepath.Join(filepath.Dir(sshDir), "outside-key"), // "../outside-key"
				filepath.Join(sshDir, "AgentOne"),
				filepath.Join(sshDir, "agent_one"),
				filepath.Join(sshDir, "agent.one"),
				filepath.Join(sshDir, "agent", "one"),
				filepath.Join(sshDir, strings.Repeat("a", 64)),
			}
			for _, p := range targets {
				writeKeyAt(t, p)
			}

			if err := m.removeAgentSSHKey(tc.agentID, m.logger); err == nil {
				t.Errorf("removeAgentSSHKey(%q) = nil, want an error refusing an unmanaged id", tc.agentID)
			}
			for _, p := range targets {
				if !keyExists(t, p) {
					t.Errorf("cleanup for %q removed %s (escaped cfg.Agent.SSHDir or an unmanaged id)", tc.agentID, p)
				}
			}
		})
	}

	t.Run("managed id", func(t *testing.T) {
		m, sshDir := newKeyManager(t)
		defer m.Stop()

		keyPath := writeAgentKey(t, sshDir, "managed-one")
		sibling := filepath.Join(filepath.Dir(sshDir), "outside-key")
		writeKeyAt(t, sibling)

		if err := m.removeAgentSSHKey("managed-one", m.logger); err != nil {
			t.Fatalf("removeAgentSSHKey(managed-one) = %v, want nil", err)
		}
		if keyExists(t, keyPath) {
			t.Errorf("managed key %s was not removed", keyPath)
		}
		if !keyExists(t, sibling) {
			t.Errorf("cleanup removed %s, outside cfg.Agent.SSHDir", sibling)
		}
	})
}

// TestRemoveAgentSSHKeyEmptyDirIsNoop: an unconfigured SSH key directory
// must never resolve a relative path (filepath.Join("", id) is the bare id,
// i.e. relative to the daemon's working directory).
func TestRemoveAgentSSHKeyEmptyDirIsNoop(t *testing.T) {
	m, _ := newKeyManager(t)
	defer m.Stop()

	workdir := t.TempDir()
	t.Chdir(workdir)
	m.cfg.Agent.SSHDir = ""

	victim := filepath.Join(workdir, "managed-one")
	if err := os.WriteFile(victim, []byte("not-a-key"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	if err := m.removeAgentSSHKey("managed-one", m.logger); err == nil {
		t.Error("removeAgentSSHKey with an empty ssh dir must refuse, not resolve a relative path")
	}
	if !keyExists(t, victim) {
		t.Errorf("cleanup removed %s from the working directory", victim)
	}
}

// TestRemoveAgentSSHKeyIdempotent: a missing key is not an error, and a
// non-regular file carrying an agent id is refused rather than deleted.
func TestRemoveAgentSSHKeyIdempotent(t *testing.T) {
	m, sshDir := newKeyManager(t)
	defer m.Stop()

	if err := m.removeAgentSSHKey("never-existed", m.logger); err != nil {
		t.Errorf("removing an absent key must be a no-op, got %v", err)
	}

	dir := filepath.Join(sshDir, "directory-entry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := m.removeAgentSSHKey("directory-entry", m.logger); err == nil {
		t.Error("a non-regular file carrying an agent id must be refused")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("cleanup removed a directory: %v", err)
	}
}
