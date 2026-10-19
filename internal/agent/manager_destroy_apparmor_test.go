package agent

// INT-HOST-006 regression: spawn's ensureRootlesskitAppArmor (rootless.go)
// writes /etc/apparmor.d/home.bunker-<id>.bin.rootlesskit and loads it with
// `apparmor_parser -r`, but no destroy path ever unloaded or removed it —
// every spawn+destroy cycle leaked one loaded profile plus one profile file
// (18,043 orphans measured on bunker-mvp vs ~120 baseline). These tests pin
// the destroy-side cleanup through SEAMS: the apparmor_parser and userdel
// executables are PATH stub recorders (the DF-BUNKER-33 pattern) and the
// profile directory is a package-var override, so no real AppArmor state and
// no host user is touched.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pointAppArmorAt redirects the cleanup path's profile directory at dir for
// the current test and restores the original afterwards.
func pointAppArmorAt(t *testing.T, dir string) {
	t.Helper()
	orig := appArmorProfileDir
	appArmorProfileDir = dir
	t.Cleanup(func() { appArmorProfileDir = orig })
}

// writeRecorderStub writes one PATH stub that appends "$0 $*" (its argv) to
// logPath and exits 0, mirroring the userdel recorder of the DF-BUNKER-33
// tests.
func writeRecorderStub(t *testing.T, stubDir, name, logPath string) {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + logPath + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// recordedCalls returns every line a recorder stub wrote (one invocation per
// line, argv space-joined).
func recordedCalls(t *testing.T, logPath string) []string {
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

// TestDestroy_UnloadsAppArmor drives a full destroy of a live agent and
// asserts the compensating chain reaches the AppArmor cleanup: the profile is
// unloaded through apparmor_parser -R and its file is removed from the
// profile directory.
func TestDestroy_UnloadsAppArmor(t *testing.T) {
	// The stubs shadow these binaries on PATH, but a host without the real
	// parser never exercised the profile machinery the leak is about — skip
	// rather than claim coverage (the brief's skip clause).
	if _, err := exec.LookPath("apparmor_parser"); err != nil {
		t.Skip("apparmor_parser not available on this host")
	}

	// Budgets shrunk to milliseconds (the spawn_failure_test.go pattern):
	// the destroy's compensating steps must be LIVE without waiting out the
	// production budgets.
	restore := shrinkRollbackBudgets(t, 2*time.Second, time.Second, 500*time.Millisecond)
	defer restore()

	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	const id = "inth006aa"
	username := "bunker-" + id
	liveAgent(t, m, id)

	// The live-process gate passes and the user record resolves: the
	// destroy must reach its teardown sequence, not refuse.
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return nil, 61001, true, nil
	})
	presentUserWithUID(t, username, "61001")

	// One stub dir serves both compensating execs: userdel records and
	// exits 0; apparmor_parser records its argv and exits 0.
	stubDir := t.TempDir()
	userLog := filepath.Join(stubDir, "userdel.log")
	parserLog := filepath.Join(stubDir, "apparmor_parser.log")
	writeRecorderStub(t, stubDir, "userdel", userLog)
	writeRecorderStub(t, stubDir, "apparmor_parser", parserLog)
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The profile spawn would have installed: the REAL naming
	// (home.bunker-<id>.bin.rootlesskit) inside a temp profile dir.
	aaDir := t.TempDir()
	pointAppArmorAt(t, aaDir)
	profilePath := agentAppArmorProfilePath(username)
	if err := os.WriteFile(profilePath, []byte("abi <abi/4.0>,\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No home to archive: keeps the archive precondition out of scope.
	pointHomeAt(t, t.TempDir())

	resp, err := m.Destroy(context.Background(), id, false)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if resp == nil || resp.Status != "destroyed" {
		t.Fatalf("status = %v, want destroyed", resp)
	}

	// The profile was unloaded through the parser seam with -R (replace),
	// pointing at the exact spawn-side profile path.
	want := "-R " + profilePath
	found := false
	for _, call := range recordedCalls(t, parserLog) {
		if strings.Contains(call, want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("apparmor_parser calls do not carry %q: %v", want, recordedCalls(t, parserLog))
	}

	// The profile FILE is gone from the profile directory.
	if _, serr := os.Stat(profilePath); !os.IsNotExist(serr) {
		t.Errorf("apparmor profile file survived destroy: %s (stat err %v)", profilePath, serr)
	}
}

// TestRemoveAgentAppArmorProfile_IdempotentNoop: no profile file means
// nothing to clean — the parser must not be invoked and the helper must
// report nil (a repeated destroy of the same agent stays clean).
func TestRemoveAgentAppArmorProfile_IdempotentNoop(t *testing.T) {
	pointAppArmorAt(t, t.TempDir())

	var invocations [][]string
	orig := appArmorParserRunner
	appArmorParserRunner = func(_ context.Context, args ...string) ([]byte, error) {
		invocations = append(invocations, args)
		return nil, nil
	}
	t.Cleanup(func() { appArmorParserRunner = orig })

	if err := RemoveAgentAppArmorProfile("inth006n1"); err != nil {
		t.Fatalf("RemoveAgentAppArmorProfile on absent profile: %v", err)
	}
	if len(invocations) != 0 {
		t.Errorf("parser invoked %d time(s) with no profile installed: %v", len(invocations), invocations)
	}
}

// TestRemoveAgentAppArmorProfile_UnloadsAndRemoves: with a profile file
// present, the helper unloads it through the parser seam with -R and removes
// the file.
func TestRemoveAgentAppArmorProfile_UnloadsAndRemoves(t *testing.T) {
	aaDir := t.TempDir()
	pointAppArmorAt(t, aaDir)
	const id = "inth006u2"
	profilePath := agentAppArmorProfilePath("bunker-" + id)
	if err := os.WriteFile(profilePath, []byte("abi <abi/4.0>,\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var invocations [][]string
	orig := appArmorParserRunner
	appArmorParserRunner = func(_ context.Context, args ...string) ([]byte, error) {
		invocations = append(invocations, args)
		return nil, nil
	}
	t.Cleanup(func() { appArmorParserRunner = orig })

	if err := RemoveAgentAppArmorProfile(id); err != nil {
		t.Fatalf("RemoveAgentAppArmorProfile: %v", err)
	}
	if len(invocations) != 1 {
		t.Fatalf("parser invocations = %d (%v), want 1", len(invocations), invocations)
	}
	if len(invocations[0]) != 2 || invocations[0][0] != "-R" || invocations[0][1] != profilePath {
		t.Errorf("parser args = %v, want [-R %s]", invocations[0], profilePath)
	}
	if _, serr := os.Stat(profilePath); !os.IsNotExist(serr) {
		t.Errorf("profile file survived cleanup: %s (stat err %v)", profilePath, serr)
	}
}

// TestRemoveAgentAppArmorProfile_UnloadFailureStillRemovesFile: a failed -R
// (profile never loaded, AppArmor busy) must not strand the FILE — the file
// is the leak that survives, a not-loaded state is self-limiting. The helper
// reports the unload failure so the destroy path can Warn, after removing.
func TestRemoveAgentAppArmorProfile_UnloadFailureStillRemovesFile(t *testing.T) {
	aaDir := t.TempDir()
	pointAppArmorAt(t, aaDir)
	const id = "inth006u3"
	profilePath := agentAppArmorProfilePath("bunker-" + id)
	if err := os.WriteFile(profilePath, []byte("abi <abi/4.0>,\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := appArmorParserRunner
	appArmorParserRunner = func(_ context.Context, args ...string) ([]byte, error) {
		return []byte("Profile for others/apparmor.d does not exist\n"), fmt.Errorf("exit status 1")
	}
	t.Cleanup(func() { appArmorParserRunner = orig })

	err := RemoveAgentAppArmorProfile(id)
	if err == nil {
		t.Fatal("unload failure must be reported, got nil")
	}
	if !strings.Contains(err.Error(), profilePath) {
		t.Errorf("unload error does not name the profile path: %v", err)
	}
	if _, serr := os.Stat(profilePath); !os.IsNotExist(serr) {
		t.Errorf("profile file stranded after failed unload: %s (stat err %v)", profilePath, serr)
	}
}
