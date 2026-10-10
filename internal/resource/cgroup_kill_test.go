package resource

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// killFixtureDir builds a fixture directory shaped like a live cgroup v2
// directory: the feature-probe file (cgroup.controllers) exists and the
// kernel-provided kill file is pre-created with "0". Everything lives under
// a t.TempDir() — the real /sys/fs/cgroup is never touched.
func killFixtureDir(t *testing.T) (dir, killFile string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cgroup.controllers"), []byte("cpuset cpu io memory pids\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	killFile = filepath.Join(dir, "cgroup.kill")
	if err := os.WriteFile(killFile, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, killFile
}

// TestKillCgroup_WritesOneToKillFile covers the happy path: the write lands
// in the AGENT's own directory with the documented payload ("1") — replacing
// the kernel's "0", never appending — and KillCgroup reports success.
func TestKillCgroup_WritesOneToKillFile(t *testing.T) {
	dir, killFile := killFixtureDir(t)

	if err := KillCgroup(dir); err != nil {
		t.Fatalf("KillCgroup() error = %v, want nil", err)
	}
	data, err := os.ReadFile(killFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "1" {
		t.Errorf("cgroup.kill content = %q, want %q", got, "1")
	}
	// Only the kill file is ever written; the probe file stays untouched.
	probe, _ := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if !strings.Contains(string(probe), "cpuset") {
		t.Errorf("feature-probe file was modified: %q", string(probe))
	}
}

// TestKillCgroup_UnavailableWhenDirNotCgroupV2 is the older-kernel fallback
// arm: a target directory without cgroup.controllers — a cgroup v1 host, a
// plain directory, or an already-torn-down slice — must map to
// ErrCgroupKillUnavailable (the sentinel the destroy path warns on) and must
// not create a kill file.
func TestKillCgroup_UnavailableWhenDirNotCgroupV2(t *testing.T) {
	t.Run("plain directory without cgroup.controllers", func(t *testing.T) {
		dir := t.TempDir()
		err := KillCgroup(dir)
		if err == nil {
			t.Fatal("KillCgroup() error = nil, want ErrCgroupKillUnavailable")
		}
		if !errors.Is(err, ErrCgroupKillUnavailable) {
			t.Fatalf("error = %v, want errors.Is ErrCgroupKillUnavailable", err)
		}
		if _, serr := os.Stat(filepath.Join(dir, "cgroup.kill")); serr == nil {
			t.Error("cgroup.kill written despite cgroup v2 being unavailable")
		}
	})
	t.Run("slice directory entirely absent (already torn down)", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "user-61999.slice")
		err := KillCgroup(missing)
		if !errors.Is(err, ErrCgroupKillUnavailable) {
			t.Fatalf("error = %v, want ErrCgroupKillUnavailable for the ENOENT arm", err)
		}
	})
}

// TestKillCgroup_WriteFailureIsNotUnavailable pins the three-valued contract:
// a REFUSED write (the cgroup dir exists but the kill file is not writable)
// is NOT the unavailable sentinel — the caller warns differently for "kernel
// cannot" vs "write failed". Skipped as root (root writes through 0444).
func TestKillCgroup_WriteFailureIsNotUnavailable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only kill file cannot be simulated")
	}
	dir, killFile := killFixtureDir(t)
	if err := os.Chmod(killFile, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(killFile, 0o644) })

	err := KillCgroup(dir)
	if err == nil {
		t.Fatal("KillCgroup() error = nil, want a write error")
	}
	if errors.Is(err, ErrCgroupKillUnavailable) {
		t.Fatalf("write failure classified as unavailable: %v", err)
	}
	if !strings.Contains(err.Error(), "cgroup.kill") {
		t.Errorf("error %v does not name the kill file", err)
	}
}

// TestAgentUserKillSlicePath_OwnSliceNeverHostRoot pins the safety invariant:
// the kill path is ALWAYS the agent user's own slice — with the production
// layout it carries the user-<uid>.slice components and can never collapse
// to the bare host root cgroup — and a test override flows through
// unchanged, so the destroy path and the metrics readers share ONE path
// spelling.
func TestAgentUserKillSlicePath_OwnSliceNeverHostRoot(t *testing.T) {
	original := agentUserSliceFn
	t.Cleanup(func() { agentUserSliceFn = original })

	got := AgentUserKillSlicePath(61001)
	wantSuffix := filepath.Join("user.slice", "user-61001.slice")
	if !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("AgentUserKillSlicePath(61001) = %q, want suffix %q", got, wantSuffix)
	}
	if got == "/sys/fs/cgroup" {
		t.Fatal("kill path resolved to the HOST ROOT cgroup — this would kill every process on the host")
	}
	fake := filepath.Join(t.TempDir(), "override-slice")
	agentUserSliceFn = func(uid int) string { return fmt.Sprintf("%s/user-%d.slice", fake, uid) }
	if got := AgentUserKillSlicePath(61001); got != filepath.Join(fake, "user-61001.slice") {
		t.Errorf("override not honored: got %q", got)
	}
}
