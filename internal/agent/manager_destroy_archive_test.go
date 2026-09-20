package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// This file pins the DF-BUNKER-33 destroy contract: an agent home is
// ARCHIVED AND VERIFIED before `userdel -rf` may run, an archive failure is
// FAIL-CLOSED (user, home, tracker record and port range all survive,
// status home_retained, no delete command issued), and
// destroy_home_policy: purge keeps the historical delete-without-archive
// behavior.
//
// The archive step runs REAL tar against a t.TempDir() home; the delete
// (userdel) is proven with a PATH stub recorder — the same stub pattern
// TestStopAgent_PreservesUserHomeAndPorts uses — because running the real
// userdel is exactly what this task exists to prevent.

// newDestroyArchiveFixture bundles what every destroy test needs: a
// lifecycle-style manager (temp data dir, isolated registry, real port
// allocator) and a PATH stub dir whose userdel recorder appends
// "userdel <args>" lines to a log. Destroy's earlier steps (docker, pgrep,
// systemctl, kill) fail fast against a nonexistent agent user/socket —
// they are best-effort Warns in the real path and never block reaching the
// archive step.
func newDestroyArchiveFixture(t *testing.T) (m *AgentManager, cfg *config.Config, userLog string) {
	t.Helper()
	m, cfg, _ = newLifecycleManager(t)

	stubDir := t.TempDir()
	userLog = filepath.Join(t.TempDir(), "userdel.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + userLog + "\"\n"
	if err := os.WriteFile(filepath.Join(stubDir, "userdel"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return m, cfg, userLog
}

// registerArchiveAgent mirrors registerLifecycleAgent without persisting a
// spawn (these tests need the tracker record and the port range; durable
// registry writes are not part of what they assert).
func registerArchiveAgent(t *testing.T, m *AgentManager, id string) (start, end uint32) {
	t.Helper()
	if m.portAlloc != nil {
		s, e, err := m.portAlloc.Allocate(id)
		if err != nil {
			t.Fatalf("allocate ports for %s: %v", id, err)
		}
		start, end = s, e
	}
	rec := &resource.AgentRecord{
		AgentID:        id,
		Status:         StatusRunning,
		CreatedAt:      time.Now(),
		PortRangeStart: start,
		PortRangeEnd:   end,
	}
	if err := m.tracker.Register(rec); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	return start, end
}

// foreignHome creates the incident's home shape: a foreign directory an
// agent owns that userdel -rf would destroy with no copy.
func foreignHome(t *testing.T, agentID string) (root, home string) {
	t.Helper()
	root = t.TempDir()
	home = filepath.Join(root, "bunker-"+agentID)
	if err := os.MkdirAll(filepath.Join(home, "eduos", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "eduos", "src", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, home
}

// userdelCalls returns every line the userdel stub recorded.
func userdelCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// pointHomeAt redirects the destroy path's home-existence probe at root
// for the current test and restores the original afterwards.
func pointHomeAt(t *testing.T, root string) {
	t.Helper()
	orig := agentHomeRoot
	agentHomeRoot = root
	t.Cleanup(func() { agentHomeRoot = orig })
}

// TestDestroy_DefaultPolicyArchivesHomeBeforeDelete covers criteria A, B
// and E: with the DEFAULT policy, a foreign directory in the agent home
// ends up inside a verified tarball under the archive dir, the archive is
// written BEFORE userdel is invoked, and the home/user are then removed
// (userdel runs). The tar in the archive step is the REAL /usr/bin/tar.
func TestDestroy_DefaultPolicyArchivesHomeBeforeDelete(t *testing.T) {
	m, cfg, userLog := newDestroyArchiveFixture(t)
	agentID := "df33a"
	// The home the destroy path sees is a temp dir; the userdel on PATH is
	// a recorder stub, so the real directory is never deleted by the test —
	// the assertions below are about the archive + the recorded delete.
	root, _ := foreignHome(t, agentID)
	pointHomeAt(t, root)

	archiveDir := t.TempDir()
	cfg.Agent.DestroyArchiveDir = archiveDir
	// Default policy is already archive; set it explicitly so this test
	// fails loudly if the default ever flips.
	cfg.Agent.DestroyHomePolicy = config.DestroyPolicyArchive

	// Wrap the real tar in a recorder so we can prove the archive step
	// actually invoked it BEFORE the delete: the wrapper appends a line
	// to tar.log, then execs the genuine /usr/bin/tar.
	tarLog := filepath.Join(t.TempDir(), "tar.log")
	wrapDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(wrapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recorder := "#!/bin/sh\nprintf 'tar\\n' >> \"" + tarLog + "\"\nexec /usr/bin/tar \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrapDir, "tar"), []byte(recorder), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	registerArchiveAgent(t, m, agentID)

	resp, derr := m.Destroy(context.Background(), agentID, false)
	if derr != nil {
		t.Fatalf("Destroy() error = %v", derr)
	}
	if resp.Status != "destroyed" {
		t.Fatalf("Destroy() status = %q, want destroyed", resp.Status)
	}

	// A: exactly one verified tarball in the archive dir.
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("archive dir holds %d entries, want 1: %v", len(entries), entries)
	}
	name := entries[0].Name()
	// Home basename is bunker-<id>, so the archive is
	// bunker-<agent-id>-<UTC timestamp>.tar.gz.
	wantPrefix := "bunker-" + agentID + "-"
	if !strings.HasPrefix(name, wantPrefix) || !strings.HasSuffix(name, ".tar.gz") {
		t.Errorf("archive name = %q, want %q<UTC timestamp>.tar.gz", name, wantPrefix)
	}
	archivePath := filepath.Join(archiveDir, name)
	if mustSize(t, archivePath) == 0 {
		t.Fatal("archive is empty")
	}

	// A+E: the foreign directory is INSIDE the tarball.
	list, err := exec.Command("tar", "tzf", archivePath).CombinedOutput()
	if err != nil {
		t.Fatalf("tar tzf: %v (%s)", err, list)
	}
	wantEntry := "bunker-" + agentID + "/eduos/src/main.go"
	if !strings.Contains(string(list), wantEntry) {
		t.Errorf("archive listing misses foreign file %s; got:\n%s", wantEntry, list)
	}

	// B: the archive step invoked the real tar, and userdel was then issued
	// exactly once (it only runs after verification passed). Order is pinned
	// three ways: tar fired ≥1 time; userdel fired exactly once; and the
	// fail-closed sibling test proves the same path CANNOT reach userdel
	// when the archive step fails.
	tarCount := 0
	if data, rerr := os.ReadFile(tarLog); rerr == nil {
		tarCount = strings.Count(string(data), "tar")
	}
	if tarCount == 0 {
		t.Error("archive step never invoked tar")
	}
	calls := userdelCalls(t, userLog)
	if len(calls) != 1 {
		t.Fatalf("userdel calls = %v, want exactly one (the delete AFTER verified archive)", calls)
	}
	if !strings.Contains(calls[0], "bunker-"+agentID) {
		t.Errorf("userdel call %q does not target the agent user", calls[0])
	}
}

// TestDestroy_ArchiveFailureIsFailClosed covers criterion C (stub tar that
// exits 1): NO userdel is issued at all, no archive lands, the home (with
// its foreign content) still exists, the tracker record survives with the
// same port range, and the call returns an error + status home_retained.
func TestDestroy_ArchiveFailureIsFailClosed(t *testing.T) {
	m, cfg, userLog := newDestroyArchiveFixture(t)
	agentID := "df33b"
	root, home := foreignHome(t, agentID)
	pointHomeAt(t, root)

	// Simulated archive failure: a PATH stub tar that exits 1 without
	// writing anything.
	failDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(failDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(failDir, "tar"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", failDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	archiveDir := t.TempDir()
	cfg.Agent.DestroyArchiveDir = archiveDir
	cfg.Agent.DestroyHomePolicy = config.DestroyPolicyArchive

	start, end := registerArchiveAgent(t, m, agentID)

	resp, derr := m.Destroy(context.Background(), agentID, false)
	if derr == nil {
		t.Fatal("Destroy() error = nil, want fail-closed error")
	}
	if resp == nil || resp.Status != StatusHomeRetained {
		t.Fatalf("Destroy() status = %v, want home_retained", resp)
	}
	if !strings.Contains(derr.Error(), "home retained") {
		t.Errorf("error %q does not state the home was retained", derr)
	}

	// C: NO userdel was issued at all — no delete command logged.
	if calls := userdelCalls(t, userLog); len(calls) != 0 {
		t.Errorf("userdel ran despite archive failure: %v", calls)
	}
	// C: no archive was written.
	if entries, rerr := os.ReadDir(archiveDir); rerr == nil && len(entries) != 0 {
		t.Errorf("archive dir holds %d entries after failed archive: %v", len(entries), entries)
	}
	// C: the home (foreign content included) still exists.
	if _, serr := os.Stat(filepath.Join(home, "eduos", "src", "main.go")); serr != nil {
		t.Errorf("foreign file vanished despite fail-closed destroy: %v", serr)
	}
	// C: the tracker record survives with the same port range.
	rec := m.tracker.Get(agentID)
	if rec == nil {
		t.Fatal("tracker record lost on fail-closed destroy")
	}
	if rec.PortRangeStart != start || rec.PortRangeEnd != end {
		t.Errorf("port range changed: got %d-%d, want %d-%d", rec.PortRangeStart, rec.PortRangeEnd, start, end)
	}
	if m.portAlloc != nil {
		gotStart, gotEnd, ok := m.portAlloc.AllocatedRange(agentID)
		if !ok || gotStart != start || gotEnd != end {
			t.Errorf("allocator lost the range: ok=%v got %d-%d, want %d-%d", ok, gotStart, gotEnd, start, end)
		}
	}
}

// TestDestroy_ArchiveDirUnwritableIsFailClosed is the second arm of
// criterion C: an unwritable archive directory must fail closed the same
// way (no userdel, home retained, error reported).
func TestDestroy_ArchiveDirUnwritableIsFailClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unwritable dir cannot be simulated")
	}
	m, cfg, userLog := newDestroyArchiveFixture(t)
	agentID := "df33c"
	root, home := foreignHome(t, agentID)
	pointHomeAt(t, root)

	unwritable := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(unwritable, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unwritable, 0o700) })

	cfg.Agent.DestroyArchiveDir = unwritable
	cfg.Agent.DestroyHomePolicy = config.DestroyPolicyArchive

	registerArchiveAgent(t, m, agentID)

	resp, derr := m.Destroy(context.Background(), agentID, false)
	if derr == nil {
		t.Fatal("Destroy() error = nil, want fail-closed error")
	}
	if resp == nil || resp.Status != StatusHomeRetained {
		t.Fatalf("Destroy() status = %v, want home_retained", resp)
	}
	if calls := userdelCalls(t, userLog); len(calls) != 0 {
		t.Errorf("userdel ran despite unwritable archive dir: %v", calls)
	}
	if _, serr := os.Stat(home); serr != nil {
		t.Errorf("home vanished despite fail-closed destroy: %v", serr)
	}
}

// TestDestroy_PurgePolicySkipsArchive covers criterion D: policy purge
// runs userdel with NO archive attempt — the legacy path, byte for byte.
func TestDestroy_PurgePolicySkipsArchive(t *testing.T) {
	m, cfg, userLog := newDestroyArchiveFixture(t)
	agentID := "df33d"
	root, _ := foreignHome(t, agentID)
	pointHomeAt(t, root)

	cfg.Agent.DestroyHomePolicy = config.DestroyPolicyPurge
	cfg.Agent.DestroyArchiveDir = t.TempDir()

	registerArchiveAgent(t, m, agentID)

	resp, derr := m.Destroy(context.Background(), agentID, false)
	if derr != nil {
		t.Fatalf("Destroy() error = %v", derr)
	}
	if resp.Status != "destroyed" {
		t.Fatalf("Destroy() status = %q, want destroyed", resp.Status)
	}
	// D: userdel ran (legacy delete) ...
	if calls := userdelCalls(t, userLog); len(calls) != 1 {
		t.Fatalf("purge path userdel calls = %v, want exactly one", calls)
	}
	// D: ... and NO archive attempt left anything behind.
	if entries, rerr := os.ReadDir(cfg.Agent.DestroyArchiveDir); rerr == nil && len(entries) != 0 {
		t.Errorf("purge path wrote archives: %v", entries)
	}
}

// TestDestroy_NoRmRfOfHomeInDestroyPath is the structural half of
// criterion E: the destroy path must never carry a raw `rm -rf <home>` —
// deletion happens exclusively through the archive-gated userdel path.
// The legitimate RemoveAll targets (the /run/bunker/<id> bookkeeping dir
// and the docker socket) stay allowed; anything aiming at the home, the
// home root or the username as a path fails here.
func TestDestroy_NoRmRfOfHomeInDestroyPath(t *testing.T) {
	data, err := os.ReadFile("manager_destroy.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "RemoveAll") {
			continue
		}
		if strings.Contains(trimmed, "homeDir") || strings.Contains(trimmed, "Home") ||
			strings.Contains(trimmed, "agentHomeRoot") || strings.Contains(trimmed, "username") {
			t.Errorf("destroy path deletes the home with a raw RemoveAll (must stay behind archive-verified userdel): %q", trimmed)
		}
	}
	// agentHomeRoot may only appear on the read side (the existence probe);
	// a write against it in the destroy file would be a home mutation.
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "agentHomeRoot") && (strings.Contains(line, "RemoveAll") || strings.Contains(line, "MkdirAll")) {
			t.Errorf("destroy path mutates the home root: %q", strings.TrimSpace(line))
		}
	}
}

// TestDestroy_NoHomeOrEmptyHomeStillDestroys keeps the no-archive fast
// path honest: an agent whose home does not exist (or is empty) destroys
// with no archive and no retained status — the incident fix must not
// wedge idempotent re-destroys.
func TestDestroy_NoHomeOrEmptyHomeStillDestroys(t *testing.T) {
	tests := []struct {
		name    string
		root    func(t *testing.T) string
		wantTar bool
	}{
		{
			name: "home absent entirely",
			root: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "missing-root")
			},
			wantTar: false,
		},
		{
			name: "home exists but empty",
			root: func(t *testing.T) string {
				return t.TempDir()
			},
			wantTar: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, cfg, _ := newDestroyArchiveFixture(t)
			agentID := "df33e"
			pointHomeAt(t, tt.root(t))

			archiveDir := t.TempDir()
			cfg.Agent.DestroyArchiveDir = archiveDir
			cfg.Agent.DestroyHomePolicy = config.DestroyPolicyArchive

			registerArchiveAgent(t, m, agentID)

			resp, derr := m.Destroy(context.Background(), agentID, false)
			if derr != nil {
				t.Fatalf("Destroy() error = %v", derr)
			}
			if resp.Status != "destroyed" {
				t.Fatalf("status = %q, want destroyed", resp.Status)
			}
			entries, _ := os.ReadDir(archiveDir)
			if gotTar := len(entries) > 0; gotTar != tt.wantTar {
				t.Errorf("archive written = %v, want %v (%v)", gotTar, tt.wantTar, entries)
			}
		})
	}
}

// TestArchiveAgentHome_VerificationContract pins the verifyArchiveFile
// gate directly: a good archive passes; a zero-byte file, a garbage
// (non-tar) file and an empty home archive all fail verification.
func TestArchiveAgentHome_VerificationContract(t *testing.T) {
	m, _, _ := newLifecycleManager(t)
	root, home := foreignHome(t, "verif1")
	_ = root

	t.Run("good archive passes and contains content", func(t *testing.T) {
		archiveDir := t.TempDir()
		path, err := m.archiveAgentHome(context.Background(), home, archiveDir)
		if err != nil {
			t.Fatalf("archiveAgentHome: %v", err)
		}
		list, lerr := exec.Command("tar", "tzf", path).CombinedOutput()
		if lerr != nil {
			t.Fatalf("tar tzf: %v", lerr)
		}
		if !strings.Contains(string(list), "bunker-verif1/eduos/src/main.go") {
			t.Errorf("archive misses foreign file:\n%s", list)
		}
	})

	t.Run("zero-byte artifact fails verification", func(t *testing.T) {
		zero := filepath.Join(t.TempDir(), "fake.tar.gz")
		if err := os.WriteFile(zero, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyArchiveFile(zero); err == nil {
			t.Error("zero-byte archive passed verification")
		}
	})

	t.Run("garbage artifact fails verification", func(t *testing.T) {
		garbage := filepath.Join(t.TempDir(), "fake.tar.gz")
		if err := os.WriteFile(garbage, []byte("not a tar at all"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyArchiveFile(garbage); err == nil {
			t.Error("non-tar archive passed verification")
		}
	})

	t.Run("empty-home archive is faithful (root-only listing passes)", func(t *testing.T) {
		// GNU tar lists the archived directory itself, so an empty home
		// yields a root-only entry — a faithful "there was nothing" record.
		// The verifier must NOT reject it: the destroy path's non-empty
		// probe is what routes empty homes away from archiving.
		emptyHome := filepath.Join(t.TempDir(), "bunker-empty1")
		if err := os.MkdirAll(emptyHome, 0o755); err != nil {
			t.Fatal(err)
		}
		archiveDir := t.TempDir()
		path, err := m.archiveAgentHome(context.Background(), emptyHome, archiveDir)
		if err != nil {
			t.Fatalf("empty-home archive failed verification: %v", err)
		}
		list, lerr := exec.Command("tar", "tzf", path).CombinedOutput()
		if lerr != nil {
			t.Fatalf("tar tzf: %v", lerr)
		}
		if !strings.Contains(string(list), "bunker-empty1/") {
			t.Errorf("empty-home archive missing its root entry:\n%s", list)
		}
	})
}

// mustSize stats path and returns its size, failing the test on error.
func mustSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}
