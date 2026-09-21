//go:build unix

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// stubSSHFSVersionProbe replaces the version-probe seam with fn (which may be
// a plain function — no process is started, so these tests are hermetic: no
// real sshfs, no network) and restores it on cleanup.
func stubSSHFSVersionProbe(t *testing.T, fn func(ctx context.Context, path string) (string, error)) *[]string {
	t.Helper()
	calls := &[]string{}
	old := sshfsVersionProbe
	sshfsVersionProbe = func(ctx context.Context, path string) (string, error) {
		*calls = append(*calls, path)
		return fn(ctx, path)
	}
	t.Cleanup(func() { sshfsVersionProbe = old })
	return calls
}

// captureOSError runs fn while capturing everything it writes to os.Stderr,
// so tests can assert on the guard's WARNING output. The guard writes to
// os.Stderr directly (matching the existing bunker: WARNING convention), so
// cmd.SetErr(io.Discard) alone cannot see it.
func captureOSError(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	var wg sync.WaitGroup
	wg.Add(1)
	var captured strings.Builder
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&captured, r)
	}()
	defer func() {
		os.Stderr = oldStderr
		_ = w.Close()
		wg.Wait()
		_ = r.Close()
	}()
	fn()
	_ = w.Close()
	os.Stderr = oldStderr
	wg.Wait()
	return captured.String()
}

// runMountWithSSHFSVersion drives `bunker mount` end-to-end with a fake
// version probe (printing versionOut, or failing with probeErr) and a
// succeeding sshfsRun seam. Returns the RunE error, the guard's stderr, and
// the number of real sshfs execs.
type mountRunResult struct {
	err         error
	guardStderr string
	sshfsRuns   int
	probeCalls  *[]string
}

func runMountWithSSHFSVersion(t *testing.T, versionOut string, probeErr error, extraArgs ...string) mountRunResult {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)

	// The mount preflight shells out to a real host; stub it here so the
	// tests exercise the guard rather than failing at preflight (same as
	// runMountExecutesSSHFS).
	_, restorePreflight := stubRemotePathCheck(t, nil)
	t.Cleanup(restorePreflight)

	probes := stubSSHFSVersionProbe(t, func(ctx context.Context, path string) (string, error) {
		return versionOut, probeErr
	})

	oldRun := sshfsRun
	oldDelay := sshfsRetryDelay
	sshfsRetryDelay = 0
	runs := 0
	sshfsRun = func(ctx context.Context, path string, args []string, stdout, stderr io.Writer) error {
		runs++
		return nil
	}
	t.Cleanup(func() {
		sshfsRun = oldRun
		sshfsRetryDelay = oldDelay
	})

	mountpoint := t.TempDir() + "/mnt"
	var res mountRunResult
	res.guardStderr = captureOSError(t, func() {
		cmd := NewMountCommand()
		args := append([]string{"df0916a", mountpoint}, extraArgs...)
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		res.err = cmd.Execute()
		// The refusal contract: when the run FAILS, no mountpoint may have
		// been created — a guard refusal precedes mountpoint creation.
		if res.err != nil {
			if _, statErr := os.Stat(mountpoint); statErr == nil {
				t.Errorf("mountpoint %s was created; a guard refusal must precede mountpoint creation", mountpoint)
			}
		}
	})
	res.sshfsRuns = runs
	res.probeCalls = probes
	return res
}

// ── unit: version parsing and comparison ────────────────────────────────────

func TestParseSSHFSVersion(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
		ok   bool
	}{
		{"upstream", "SSHFS version 3.7.6\n", "3.7.6", true},
		{"upstream two-part", "SSHFS version 3.7\n", "3.7", true},
		{"lowercase (distro wrappers)", "sshfs version 3.7.3-1.1build5\n", "3.7.3", true},
		{"bare triplet", "3.8.0\n", "3.8.0", true},
		{"triplet embedded in noise", "sshfs 3.9.1 (build 2026)\n", "3.9.1", true},
		{"garbage", "SSHFS: cannot parse me", "", false},
		{"empty", "", "", false},
		{"usage text without number", "usage: sshfs [options]\n", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSSHFSVersion(tc.out)
			if ok != tc.ok || got != tc.want {
				t.Errorf("parseSSHFSVersion(%q) = %q, %v; want %q, %v", tc.out, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSSHFSVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"3.7", "3.7.6", true},
		{"3.7.3", "3.7.6", true},
		{"3.7.5", "3.7.6", true},
		{"3.7.6", "3.7.6", false},
		{"3.8.0", "3.7.6", false},
		{"3.8", "3.7.6", false},
		// The whole reason this is numeric, not lexical: "3.10" < "3.7" as
		// strings but not as versions.
		{"3.10", "3.7", false},
		{"3.7", "3.10", true},
		{"3", "3.0.0", false},
		{"3.7.6", "3.7.6.1", true},
	}
	for _, tc := range cases {
		if got := sshfsVersionLess(tc.a, tc.b); got != tc.want {
			t.Errorf("sshfsVersionLess(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSSHFSAffected(t *testing.T) {
	for _, v := range []string{"3.7.0", "3.7.3", "3.7.5"} {
		if !sshfsAffected(v) {
			t.Errorf("sshfsAffected(%q) = false, want true (below the fix)", v)
		}
	}
	for _, v := range []string{"3.7.6", "3.7.7", "3.8", "3.10.0"} {
		if sshfsAffected(v) {
			t.Errorf("sshfsAffected(%q) = true, want false (at or above the fix)", v)
		}
	}
	if sshfsAffected("") {
		t.Error("sshfsAffected(\"\") = true; unparsable input is never called affected here")
	}
}

// ── unit: the guard's decision table ────────────────────────────────────────

func TestGuardSSHFSVersion(t *testing.T) {
	probeOK := func(out string, err error) func(context.Context, string) (string, error) {
		return func(context.Context, string) (string, error) { return out, err }
	}
	cases := []struct {
		name           string
		probe          func(context.Context, string) (string, error)
		requirePatched bool
		wantAction     string // "silent" | "warn" | "refuse"
	}{
		{"patched -> silent proceed", probeOK("SSHFS version 3.7.6\n", nil), false, "silent"},
		{"patched + require -> silent proceed", probeOK("SSHFS version 3.7.6\n", nil), true, "silent"},
		{"newer -> silent proceed", probeOK("SSHFS version 3.10.0\n", nil), false, "silent"},
		{"affected -> warn", probeOK("SSHFS version 3.7.3\n", nil), false, "warn"},
		{"affected + require -> refuse", probeOK("SSHFS version 3.7.3\n", nil), true, "refuse"},
		{"two-part affected -> warn", probeOK("SSHFS version 3.7\n", nil), false, "warn"},
		{"garbage -> warn", probeOK("total nonsense\n", nil), false, "warn"},
		{"garbage + require -> refuse", probeOK("total nonsense\n", nil), true, "refuse"},
		{"probe failure -> warn", probeOK("", errors.New("exit status 127")), false, "warn"},
		{"probe failure + require -> refuse", probeOK("", errors.New("exit status 127")), true, "refuse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubSSHFSVersionProbe(t, tc.probe)
			var err error
			var stderr string
			stderr = captureOSError(t, func() {
				err = guardSSHFSVersion(context.Background(), "/fake/sshfs", tc.requirePatched)
			})
			switch tc.wantAction {
			case "refuse":
				if err == nil {
					t.Fatal("expected a refusal with --sshfs-require-patched, got nil")
				}
				if !strings.Contains(err.Error(), "--sshfs-require-patched") {
					t.Errorf("refusal must name the flag, got: %v", err)
				}
				if !strings.Contains(err.Error(), "CVE-2026-47187") || !strings.Contains(err.Error(), "CVE-2026-48711") {
					t.Errorf("refusal must name both CVEs, got: %v", err)
				}
			case "warn":
				if err != nil {
					t.Fatalf("default mode must never block, got: %v", err)
				}
				if !strings.Contains(stderr, "WARNING") {
					t.Errorf("expected a WARNING on stderr, got: %q", stderr)
				}
				if !strings.Contains(stderr, "CVE-2026-47187") || !strings.Contains(stderr, "CVE-2026-48711") {
					t.Errorf("warning must name both CVEs, got: %q", stderr)
				}
				if !strings.Contains(stderr, "3.7.6") {
					t.Errorf("warning must name the affected range boundary 3.7.6, got: %q", stderr)
				}
			default: // silent
				if err != nil {
					t.Fatalf("expected silent proceed, got: %v", err)
				}
				if stderr != "" {
					t.Errorf("patched sshfs must proceed silently, got stderr: %q", stderr)
				}
			}
		})
	}
}

// ── flag registration ───────────────────────────────────────────────────────

func TestNewMountCommand_SSHFSRequirePatchedFlag(t *testing.T) {
	cmd := NewMountCommand()
	flag := cmd.Flags().Lookup("sshfs-require-patched")
	if flag == nil {
		t.Fatal("--sshfs-require-patched flag not registered")
	}
	if flag.Name != "sshfs-require-patched" {
		t.Errorf("flag name = %q, want %q", flag.Name, "sshfs-require-patched")
	}
}

// ── end-to-end through `bunker mount` (hermetic fakes, no real sshfs) ───────

func TestMountCommand_AffectedSSHFS_WarnsAndProceeds(t *testing.T) {
	res := runMountWithSSHFSVersion(t, "SSHFS version 3.7.3\n", nil)
	if res.err != nil {
		t.Fatalf("affected sshfs must warn-and-proceed by default, got: %v", res.err)
	}
	if !strings.Contains(res.guardStderr, "WARNING") || !strings.Contains(res.guardStderr, "3.7.3") {
		t.Errorf("expected the affected-range warning naming the version, got: %q", res.guardStderr)
	}
	if res.sshfsRuns != 1 {
		t.Errorf("sshfs called %d times, want 1 (the mount proceeds)", res.sshfsRuns)
	}
}

func TestMountCommand_AffectedSSHFS_RequirePatched_RefusesBeforeMount(t *testing.T) {
	res := runMountWithSSHFSVersion(t, "SSHFS version 3.7.3\n", nil, "--sshfs-require-patched")
	if res.err == nil {
		t.Fatal("expected refusal with --sshfs-require-patched on an affected sshfs")
	}
	if !strings.Contains(res.err.Error(), "CVE-2026-47187") || !strings.Contains(res.err.Error(), "CVE-2026-48711") {
		t.Errorf("refusal must name both CVEs, got: %v", res.err)
	}
	if res.sshfsRuns != 0 {
		t.Errorf("sshfs called %d times, want 0 (refusal must precede any mount attempt)", res.sshfsRuns)
	}
}

func TestMountCommand_PatchedSSHFS_ProceedsSilently(t *testing.T) {
	for _, out := range []string{"SSHFS version 3.7.6\n", "SSHFS version 3.8.0\n"} {
		res := runMountWithSSHFSVersion(t, out, nil)
		if res.err != nil {
			t.Fatalf("patched sshfs (%q) must proceed, got: %v", out, res.err)
		}
		if strings.Contains(res.guardStderr, "WARNING") {
			t.Errorf("patched sshfs (%q) must be silent, got stderr: %q", out, res.guardStderr)
		}
		if res.sshfsRuns != 1 {
			t.Errorf("sshfs called %d times for %q, want 1", res.sshfsRuns, out)
		}
	}
}

func TestMountCommand_GarbageProbeOutput(t *testing.T) {
	// Default: warn + proceed.
	res := runMountWithSSHFSVersion(t, "not a version at all\n", nil)
	if res.err != nil {
		t.Fatalf("unparseable output must never block by default, got: %v", res.err)
	}
	if !strings.Contains(res.guardStderr, "WARNING") {
		t.Errorf("expected a warning for unparseable output, got: %q", res.guardStderr)
	}
	if res.sshfsRuns != 1 {
		t.Errorf("sshfs called %d times, want 1", res.sshfsRuns)
	}

	// With --sshfs-require-patched: refuse before the mount.
	res = runMountWithSSHFSVersion(t, "not a version at all\n", nil, "--sshfs-require-patched")
	if res.err == nil {
		t.Fatal("expected refusal for unparseable output with --sshfs-require-patched")
	}
	if !strings.Contains(res.err.Error(), "version could not be verified") {
		t.Errorf("refusal must name the unverifiable-version cause, got: %v", res.err)
	}
	if res.sshfsRuns != 0 {
		t.Errorf("sshfs called %d times, want 0 (refused before mount)", res.sshfsRuns)
	}
}

func TestMountCommand_ProbeFailure(t *testing.T) {
	// Default: warn + proceed (the probe must never block a working mount).
	res := runMountWithSSHFSVersion(t, "", errors.New("exit status 126"))
	if res.err != nil {
		t.Fatalf("probe failure must never block by default, got: %v", res.err)
	}
	if !strings.Contains(res.guardStderr, "WARNING") {
		t.Errorf("expected a probe-failure warning, got: %q", res.guardStderr)
	}

	// With --sshfs-require-patched: refuse — an operator who demands a
	// patched sshfs must not be satisfied by an unprobeable one.
	res = runMountWithSSHFSVersion(t, "", errors.New("exit status 126"), "--sshfs-require-patched")
	if res.err == nil {
		t.Fatal("expected refusal on probe failure with --sshfs-require-patched")
	}
	if !strings.Contains(res.err.Error(), "probe failed") {
		t.Errorf("refusal must name the probe failure, got: %v", res.err)
	}
	if res.sshfsRuns != 0 {
		t.Errorf("sshfs called %d times, want 0 (refused before mount)", res.sshfsRuns)
	}
}

// TestMountCommand_GuardProbeRunsOnce pins the "probe once per invocation,
// cover every attempt" contract: the guard runs on the shared code path, so
// the retry loop must not multiply probes.
func TestMountCommand_GuardProbeRunsOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)

	probes := stubSSHFSVersionProbe(t, func(ctx context.Context, path string) (string, error) {
		return "SSHFS version 3.7.3\n", nil // affected: forces the warn path but proceeds
	})

	calls, restore := stubSSHFSRun(t, 2, errors.New("exit status 1"))
	t.Cleanup(restore)

	if err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt"); err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if got := len(*calls); got != 3 {
		t.Fatalf("sshfs called %d times, want 3", got)
	}
	if got := len(*probes); got != 1 {
		t.Fatalf("version probe ran %d times, want exactly 1 per invocation", got)
	}
}

// TestMountCommand_GuardProbesTheExecutedBinary pins that the guard probes
// the SAME binary the mount will exec (field 0 of the stored command), not
// some unrelated lookup.
func TestMountCommand_GuardProbesTheExecutedBinary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)

	var probed string
	stubSSHFSVersionProbe(t, func(ctx context.Context, path string) (string, error) {
		probed = path
		return "SSHFS version 3.7.6\n", nil
	})
	calls, restore := stubSSHFSRun(t, 0, nil)
	t.Cleanup(restore)

	if err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if probed != "sshfs" {
		t.Errorf("guard probed %q, want %q (field 0 of the stored command)", probed, "sshfs")
	}
	if len(*calls) != 1 {
		t.Errorf("sshfs called %d times, want 1", len(*calls))
	}
}

// TestMountCommand_ProbeTimeoutBoundsHangingSSHFS exercises the REAL probe
// implementation against a script that ignores its arguments and sleeps: the
// probe must give up within the (shrunk) timeout and the default mode must
// warn-and-proceed, never block the mount.
func TestMountCommand_ProbeTimeoutBoundsHangingSSHFS(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "sshfs")
	hang := fmt.Sprintf("#!/bin/sh\nsleep 30\n")
	if err := os.WriteFile(script, []byte(hang), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	oldTimeout := sshfsProbeTimeout
	sshfsProbeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { sshfsProbeTimeout = oldTimeout })

	t0 := time.Now()
	_, probeErr := sshfsVersionProbe(context.Background(), script)
	elapsed := time.Since(t0)
	if probeErr == nil {
		t.Fatal("expected the probe to fail against a hanging binary")
	}
	if elapsed > 5*time.Second {
		t.Errorf("probe took %v; the timeout must bound it well under the default 5s", elapsed)
	}
	var exitErr *exec.ExitError
	if !errors.As(probeErr, &exitErr) {
		t.Errorf("probe failure should be an exec error, got: %v", probeErr)
	}
}
