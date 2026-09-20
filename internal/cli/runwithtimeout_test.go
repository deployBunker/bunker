package cli

// Tests that execute the REAL runWithTimeout (DF-BUNKER-38).
//
// The mount preflight shells out to ssh through runWithTimeout with the probe
// script on stdin, and umount shells out to fusermount3/umount the same way —
// so the function's contract is: run the command exactly once, deliver stdin,
// capture stdout+stderr, and kill on deadline. Before DF-BUNKER-38 the
// function called Start() and then CombinedOutput() on the same *exec.Cmd;
// CombinedOutput's internal Start failed with "exec: already started" and the
// real command ran with its output discarded.
//
// Every earlier test routed around the function (remotePathCheck stub,
// BUNKER_SKIP_MOUNT_PREFLIGHT=1 in subprocess tests), so the bug was invisible.
// These tests go through the real code path with stub commands on PATH.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeStubCommand writes an executable shell script with the given body into
// dir under the given name. Callers put dir first on PATH (t.Setenv) so exec
// resolves the stub instead of the real tool.
func writeStubCommand(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
}

// stubDirOnPATH creates a temp bin dir and prepends it to PATH for the test.
func stubDirOnPATH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestRunWithTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-stub based tests require a POSIX shell")
	}

	t.Run("captures combined output and exit error", func(t *testing.T) {
		cmd := exec.Command("sh", "-c", `echo out-line; echo err-line 1>&2; exit 7`)
		out, err := runWithTimeout(cmd, 10*time.Second)
		if err == nil {
			t.Fatal("expected the non-zero exit to surface as an error")
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("expected an exec.ExitError, got %T: %v", err, err)
		}
		if code := exitErr.ExitCode(); code != 7 {
			t.Fatalf("exit code = %d, want 7", code)
		}
		for _, want := range []string{"out-line", "err-line"} {
			if !strings.Contains(out, want) {
				t.Errorf("combined output missing %q, got %q", want, out)
			}
		}
	})

	t.Run("delivers stdin to the command", func(t *testing.T) {
		dir := stubDirOnPATH(t)
		// The stub records what arrived on stdin, then echoes it back so the
		// return value double-checks the same fact.
		writeStubCommand(t, dir, "cat", `#!/bin/sh
tee "$STUB_STDIN_RECORD"
`)
		record := filepath.Join(t.TempDir(), "stdin-record")
		cmd := exec.Command("cat")
		cmd.Env = append(os.Environ(), "STUB_STDIN_RECORD="+record)
		probe := "__BUNKER_PROBE_SCRIPT__"
		cmd.Stdin = strings.NewReader(probe + "\n")

		out, err := runWithTimeout(cmd, 10*time.Second)
		if err != nil {
			t.Fatalf("runWithTimeout: %v", err)
		}
		if !strings.Contains(out, probe) {
			t.Errorf("stdin was not delivered through to the command's stdout: %q", out)
		}
		b, err := os.ReadFile(record)
		if err != nil {
			t.Fatalf("stub did not record stdin: %v", err)
		}
		if !strings.Contains(string(b), probe) {
			t.Errorf("recorded stdin %q missing probe %q", string(b), probe)
		}
	})

	t.Run("timeout kills the command and names it", func(t *testing.T) {
		dir := stubDirOnPATH(t)
		// `exec sleep` replaces the shell with the sleeper, so the kill hits
		// the sleeping pid itself instead of a parent that left grandchildren
		// holding the inherited pipes (which would block Wait past the kill).
		writeStubCommand(t, dir, "bunker-slow-stub", `#!/bin/sh
exec sleep 30
`)
		start := time.Now()
		out, err := runWithTimeout(exec.Command("bunker-slow-stub"), 150*time.Millisecond)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("expected a timeout error")
		}
		if strings.Contains(err.Error(), "already started") {
			t.Fatalf("the Start-then-CombinedOutput bug is back: %v", err)
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("timeout error should say 'timed out', got: %v", err)
		}
		if !strings.Contains(err.Error(), "bunker-slow-stub") {
			t.Fatalf("timeout error must name the command, got: %v", err)
		}
		if elapsed > 10*time.Second {
			t.Fatalf("runWithTimeout returned after %s; the deadline did not kill the child", elapsed)
		}
		if out != "" {
			t.Errorf("expected no output from the killed stub, got %q", out)
		}
	})

	t.Run("output captured even on timeout deadline", func(t *testing.T) {
		// A child that writes and keeps running: the kill must not lose what
		// it already produced (the umount error path surfaces this output).
		dir := stubDirOnPATH(t)
		writeStubCommand(t, dir, "bunker-noisy-stub", `#!/bin/sh
echo partial-output
exec sleep 30
`)
		out, err := runWithTimeout(exec.Command("bunker-noisy-stub"), 150*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("expected a timeout error, got: %v", err)
		}
		if !strings.Contains(out, "partial-output") {
			t.Errorf("output produced before the deadline was lost, got %q", out)
		}
	})
}

// TestRemotePathExists_FakeSSH uses the real remotePathExists -> runWithTimeout
// path end to end with a fake `ssh` placed first on PATH. This is the test the
// old suite could not have: the seam was stubbed and the subprocess tests
// skipped the preflight, so runWithTimeout never executed in CI.
func TestRemotePathExists_FakeSSH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-stub based tests require a POSIX shell")
	}
	dir := stubDirOnPATH(t)

	// The fake ssh reads the probe script from stdin (as `sh -s` would),
	// asserts the marker line is present, then emits the marker + identity
	// lines the real parser expects for an existing directory.
	writeStubCommand(t, dir, "ssh", `#!/bin/sh
script=$(cat)
case "$script" in
  *__BUNKER_DIR__*) ;;
  *) echo "fake-ssh: probe script missing from stdin" 1>&2; exit 99 ;;
esac
echo "__BUNKER_DIR__"
echo "remote=git@github.com:deployBunker/bunker.git"
echo "head=abcdef0123456789"
echo "branch=main"
`)

	ident, err := remotePathExists("user@fake-host", "/tmp/fake-key", "/srv/workspace")
	if err != nil {
		t.Fatalf("remotePathExists with existing remote dir: %v", err)
	}
	if !ident.IsRepo {
		t.Errorf("identity should report a repo, got %+v", ident)
	}
	if ident.GitBranch != "main" || ident.GitHead != "abcdef0123456789" {
		t.Errorf("identity fields not parsed: %+v", ident)
	}
	if ident.GitRemote != "git@github.com:deployBunker/bunker.git" {
		t.Errorf("git remote not parsed: %+v", ident)
	}
}

// TestRemotePathExists_FakeSSHMissingDir: the fake ssh emits the no-dir marker
// and exits non-zero (as the real probe script does for a missing path); the
// preflight must report the path as missing, not "cannot reach".
func TestRemotePathExists_FakeSSHMissingDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-stub based tests require a POSIX shell")
	}
	dir := stubDirOnPATH(t)
	writeStubCommand(t, dir, "ssh", `#!/bin/sh
cat > /dev/null
echo "__BUNKER_NO_DIR__"
exit 3
`)

	_, err := remotePathExists("user@fake-host", "/tmp/fake-key", "/srv/missing")
	if err == nil {
		t.Fatal("expected an error for a missing remote directory")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing path must be reported as such, got: %v", err)
	}
	if strings.Contains(err.Error(), "cannot reach") {
		t.Fatalf("a missing path must not be reported as an unreachable host, got: %v", err)
	}
}

// TestUnmountFallbackChainWithStubs proves the fusermount3 -> lazy fallback is
// live again. The first fusermount3 -u fails, the lazy retry succeeds; before
// DF-BUNKER-38 both legs died on "exec: already started" and the chain could
// never fall through.
func TestUnmountFallbackChainWithStubs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-stub based tests require a POSIX shell")
	}
	dir := stubDirOnPATH(t)
	calls := filepath.Join(t.TempDir(), "calls")
	// Children of the test process inherit this (exec.Cmd.Env is nil), so the
	// stub can record its argv across both invocations.
	t.Setenv("STUB_CALLS", calls)

	// fusermount3: fail the normal attempt, succeed the lazy one; record argv.
	writeStubCommand(t, dir, "fusermount3", `#!/bin/sh
echo "$@" >> "$STUB_CALLS"
if [ "$1" = "-u" ] && [ "$#" -eq 2 ]; then
  echo "fuser: failed to unmount" 1>&2
  exit 1
fi
exit 0
`)
	writeStubCommand(t, dir, "umount", `#!/bin/sh
echo "umount: must not be called when fusermount3 exists" 1>&2
exit 1
`)

	// force=true skips the busy-holder guard: on a loaded host the real
	// system-wide fuser matches unrelated processes and would block the
	// fallback-chain assertion itself. This test is about the runWithTimeout
	// fallback order, not the guard (which mount_lifecycle_test.go covers via
	// the no-op paths).
	if err := unmount(t.TempDir(), true); err != nil {
		t.Fatalf("unmount with working lazy fallback must succeed: %v", err)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("fusermount3 stub was never called: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected exactly 2 fusermount3 calls (normal + lazy), got %d: %v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "-u ") || !strings.HasPrefix(lines[1], "-uz ") {
		t.Fatalf("fallback order wrong, got: %v", lines)
	}
}
