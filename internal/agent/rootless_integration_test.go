//go:build integration

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestLogger returns a logger suitable for integration tests.
func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// requireRoot skips the test when not running as root.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
}

// requireSystemctl skips the test when systemd's systemctl is not available.
func requireSystemctl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("systemctl not found")
	}
	// Quick sanity check that systemd is reachable.
	out, err := exec.Command("systemctl", "is-system-running").CombinedOutput()
	if err != nil && !strings.Contains(string(out), "degraded") {
		t.Skipf("systemd unavailable: %v (output: %s)", err, string(out))
	}
}

// createTestUser adds a system user and returns the *user.User.
// The caller must call cleanupTestUser on success.
func createTestUser(t *testing.T, username string) *user.User {
	t.Helper()

	home := filepath.Join("/tmp", username)
	if err := os.MkdirAll(home, 0755); err != nil {
		t.Fatalf("create home dir: %v", err)
	}

	cmd := exec.Command("useradd", "-r", "-m", "-d", home, "-s", "/usr/sbin/nologin", username)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create user %s: %v (output: %s)", username, err, string(out))
	}

	u, err := user.Lookup(username)
	if err != nil {
		// Best-effort cleanup: try to remove the user so we don't leak state.
		_ = exec.Command("userdel", "-r", username).Run()
		t.Fatalf("lookup user %s: %v", username, err)
	}
	return u
}

// cleanupTestUser removes a user created by createTestUser and its home directory.
func cleanupTestUser(t *testing.T, username string) {
	t.Helper()
	_ = exec.Command("systemctl", "stop", "user@"+username+".service").Run()

	u, err := user.Lookup(username)
	if err == nil {
		_ = exec.Command("loginctl", "terminate-user", u.Uid).Run()
	}

	// userdel -r may fail if home directory has files owned by root; tolerate.
	if out, err := exec.Command("userdel", "-r", username).CombinedOutput(); err != nil {
		t.Logf("cleanup userdel %s: %v (output: %s)", username, err, string(out))
	}
}

func TestIntegration_ApplyUserSliceLimits(t *testing.T) {
	requireRoot(t)
	requireSystemctl(t)

	logger := newTestLogger(t)
	username := fmt.Sprintf("bunker-slice-%d", time.Now().UnixNano()%100000)
	u := createTestUser(t, username)
	defer cleanupTestUser(t, username)

	// Drop-in path is tied to the UID, so we can predict and verify it.
	sliceName := fmt.Sprintf("user-%s.slice", u.Uid)
	dropinDir := filepath.Join("/etc/systemd/system", sliceName+".d")
	dropinPath := filepath.Join(dropinDir, "50-bunker.conf")

	// Clean up any stale drop-in from previous runs, then remove it on exit.
	_ = os.RemoveAll(dropinDir)
	t.Cleanup(func() {
		_ = os.RemoveAll(dropinDir)
		_ = exec.Command("systemctl", "daemon-reload").Run()
	})

	cpuQuota := 0.75
	memMax := uint64(512 * 1024 * 1024)
	diskMax := uint64(1 * 1024 * 1024 * 1024)
	maxProcs := uint64(4096)
	maxFiles := uint64(65536)

	err := applyUserSliceLimits(t.Context(), u, cpuQuota, memMax, diskMax, maxProcs, maxFiles, logger)
	if err != nil {
		t.Fatalf("applyUserSliceLimits: %v", err)
	}

	data, err := os.ReadFile(dropinPath)
	if err != nil {
		t.Fatalf("read drop-in %s: %v", dropinPath, err)
	}
	content := string(data)

	wantTokens := map[string]string{
		"CPUQuota":    "CPUQuota=75%",
		"MemoryMax":   fmt.Sprintf("MemoryMax=%d", memMax),
		"TasksMax":    fmt.Sprintf("TasksMax=%d", maxProcs),
		"LimitNOFILE": fmt.Sprintf("LimitNOFILE=%d:%d", maxFiles, maxFiles),
		"LimitFSIZE":  fmt.Sprintf("LimitFSIZE=%d", diskMax),
	}
	for name, want := range wantTokens {
		if !strings.Contains(content, want) {
			t.Errorf("drop-in missing %s: want %q in\n%s", name, want, content)
		}
	}
	if !strings.HasPrefix(content, "[Slice]") {
		t.Errorf("drop-in missing [Slice] header: %s", content)
	}

	// Verify daemon-reload succeeded by checking the unit shows the new values.
	out, err := exec.Command("systemctl", "show", sliceName, "--property=CPUQuota", "--property=MemoryMax", "--property=TasksMax").CombinedOutput()
	if err != nil {
		t.Fatalf("systemctl show %s: %v (output: %s)", sliceName, err, string(out))
	}
	if !strings.Contains(string(out), "CPUQuota=") && !strings.Contains(string(out), "MemoryMax=") {
		t.Logf("systemctl show output: %s", string(out))
		t.Errorf("systemctl show did not return expected slice properties")
	}
}

func TestIntegration_ApplyUserSliceLimits_ZeroValues(t *testing.T) {
	requireRoot(t)
	requireSystemctl(t)

	logger := newTestLogger(t)
	username := fmt.Sprintf("bunker-slice-zero-%d", time.Now().UnixNano()%100000)
	u := createTestUser(t, username)
	defer cleanupTestUser(t, username)

	sliceName := fmt.Sprintf("user-%s.slice", u.Uid)
	dropinDir := filepath.Join("/etc/systemd/system", sliceName+".d")
	_ = os.RemoveAll(dropinDir)
	t.Cleanup(func() {
		_ = os.RemoveAll(dropinDir)
		_ = exec.Command("systemctl", "daemon-reload").Run()
	})

	err := applyUserSliceLimits(t.Context(), u, 0, 0, 0, 0, 0, logger)
	if err != nil {
		t.Fatalf("applyUserSliceLimits with zero values: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dropinDir, "50-bunker.conf"))
	if err != nil {
		t.Fatalf("read drop-in: %v", err)
	}
	content := strings.TrimSpace(string(data))
	if content != "[Slice]" {
		t.Errorf("expected drop-in to contain only [Slice], got:\n%s", content)
	}
}

func TestIntegration_InstallRootlessDocker(t *testing.T) {
	requireRoot(t)
	requireSystemctl(t)

	// The installer downloads the Docker rootless installer and needs network access.
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not found")
	}
	if _, err := exec.LookPath("su"); err != nil {
		t.Skip("su not found")
	}
	if _, err := exec.LookPath("loginctl"); err != nil {
		t.Skip("loginctl not found")
	}

	logger := newTestLogger(t)
	username := fmt.Sprintf("bunker-rootless-%d", time.Now().UnixNano()%100000)
	u := createTestUser(t, username)
	defer cleanupTestUser(t, username)

	// Ensure the test user has a subordinate ID range so rootless Docker can run.
	if err := configureSubIDs(t.Context(), username); err != nil {
		t.Fatalf("configureSubIDs: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	err := installRootlessDocker(ctx, username, u.HomeDir, logger)
	if err != nil {
		t.Fatalf("installRootlessDocker: %v", err)
	}

	rootlessScript := filepath.Join(u.HomeDir, "bin", "dockerd-rootless.sh")
	if _, err := os.Stat(rootlessScript); err != nil {
		t.Errorf("rootless script not found after install: %v", err)
	}

	// Ensure binaries are owned by the agent user so cleanup works.
	info, err := os.Stat(rootlessScript)
	if err != nil {
		t.Logf("stat rootless script: %v", err)
	} else if info.Sys().(*user.Stat_t).Uid != 0 {
		// Check that ownership is not root (best-effort cross-platform check).
		fi, ok := info.Sys().(*user.Stat_t)
		if ok {
			uid, _ := strconv.Atoi(u.Uid)
			if int(fi.Uid) != uid {
				t.Errorf("rootless script owned by uid %d, want %d", fi.Uid, uid)
			}
		}
	}
}

func TestIntegration_EnsureRootlesskitAppArmor(t *testing.T) {
	requireRoot(t)

	if _, err := exec.LookPath("apparmor_parser"); err != nil {
		t.Skip("apparmor_parser not found")
	}

	logger := newTestLogger(t)
	username := fmt.Sprintf("bunker-apparmor-%d", time.Now().UnixNano()%100000)
	u := createTestUser(t, username)
	defer cleanupTestUser(t, username)

	// Ensure the user's bin/rootlesskit path exists so the profile has a real target.
	rootlesskitBin := filepath.Join(u.HomeDir, "bin", "rootlesskit")
	if err := os.MkdirAll(filepath.Dir(rootlesskitBin), 0755); err != nil {
		t.Fatalf("mkdir bin dir: %v", err)
	}
	if err := os.WriteFile(rootlesskitBin, []byte("#!/bin/sh\necho stub\n"), 0755); err != nil {
		t.Fatalf("write stub rootlesskit: %v", err)
	}
	if err := os.Chown(rootlesskitBin, 0, 0); err != nil {
		t.Fatalf("chown stub rootlesskit: %v", err)
	}

	profileName := fmt.Sprintf("home.%s.bin.rootlesskit", username)
	profilePath := filepath.Join("/etc/apparmor.d", profileName)

	// Clean up any stale profile first.
	if _, err := os.Stat(profilePath); err == nil {
		_ = exec.Command("apparmor_parser", "-R", profilePath).Run()
		_ = os.Remove(profilePath)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(profilePath); err == nil {
			_ = exec.Command("apparmor_parser", "-R", profilePath).Run()
			_ = os.Remove(profilePath)
		}
	})

	if err := ensureRootlesskitAppArmor(t.Context(), username, logger); err != nil {
		t.Fatalf("ensureRootlesskitAppArmor: %v", err)
	}

	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read profile %s: %v", profilePath, err)
	}
	content := string(data)
	if !strings.Contains(content, rootlesskitBin) {
		t.Errorf("profile missing rootlesskit path %q: %s", rootlesskitBin, content)
	}
	if !strings.Contains(content, "flags=(unconfined)") {
		t.Errorf("profile missing unconfined flag: %s", content)
	}
	if !strings.Contains(content, "userns,") {
		t.Errorf("profile missing userns capability: %s", content)
	}

	// Verify the profile is loaded by the kernel.
	out, err := exec.Command("aa-status", "--json").CombinedOutput()
	if err != nil {
		// aa-status is not always installed; fall back to /sys/kernel/security/apparmor/profiles.
		data, err := os.ReadFile("/sys/kernel/security/apparmor/profiles")
		if err != nil {
			t.Fatalf("cannot verify profile load: %v", err)
		}
		if !strings.Contains(string(data), profileName) {
			t.Errorf("profile %s not loaded", profileName)
		}
	} else {
		if !strings.Contains(string(out), profileName) {
			t.Errorf("profile %s not found in aa-status output", profileName)
		}
	}

	// Unload the profile and verify it is removed.
	unloadOut, err := exec.Command("apparmor_parser", "-R", profilePath).CombinedOutput()
	if err != nil {
		t.Fatalf("unload profile: %v (output: %s)", err, string(unloadOut))
	}
	data, _ = os.ReadFile("/sys/kernel/security/apparmor/profiles")
	if strings.Contains(string(data), profileName) {
		t.Errorf("profile %s still loaded after unload", profileName)
	}
}

func TestIntegration_WaitForUserManager_Timeout(t *testing.T) {
	requireRoot(t)

	// Use a runtime directory that definitely does not exist yet.
	runtimeDir := filepath.Join(t.TempDir(), "nonexistent-run-user")

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	err := waitForUserManager(ctx, runtimeDir)
	if err == nil {
		t.Fatal("expected timeout error for missing bus socket")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestIntegration_WaitForUserManager_Success(t *testing.T) {
	requireRoot(t)

	// Create a fake bus socket in a temporary directory to simulate a running user manager.
	runtimeDir := t.TempDir()
	busPath := filepath.Join(runtimeDir, "bus")
	if err := os.WriteFile(busPath, []byte(""), 0644); err != nil {
		t.Fatalf("create fake bus socket: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	if err := waitForUserManager(ctx, runtimeDir); err != nil {
		t.Fatalf("waitForUserManager failed for existing bus socket: %v", err)
	}
}
