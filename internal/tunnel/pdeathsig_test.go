package tunnel

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ── GAP-084 (criterion 3): the daemon side must not leak cloudflared ────────
//
// configureTunnelCommand already put cloudflared in its own process group and
// stopTunnelCommand already killed that group, but nothing covered the path
// where bunkerd cannot run a line of code: SIGKILL, the OOM killer, a crash.
// The cloudflared child was then reparented to init and kept serving a tunnel
// no daemon knew about.
//
// The tests below pin both halves: the SysProcAttr wiring, and a real-process
// proof through the PRODUCTION start path (TunnelManager.Start) that a
// SIGKILLed parent leaves no cloudflared behind.

// TestConfigureTunnelCommand_ArmsProcessGroupAndPdeathsig pins the wiring: the
// cloudflared child owns its process group (the group kill in
// stopTunnelCommand needs one) and carries the kernel parent-death backstop.
//
// The Pdeathsig field only exists on Linux/FreeBSD, so it is read reflectively
// and this file stays buildable on every GOOS.
func TestConfigureTunnelCommand_ArmsProcessGroupAndPdeathsig(t *testing.T) {
	cmd := exec.Command("true")
	configureTunnelCommand(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("configureTunnelCommand left SysProcAttr nil: no process group, no parent-death backstop")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Error("configureTunnelCommand must set Setpgid: stopTunnelCommand kills the child's group (-pid)")
	}

	field := reflect.ValueOf(cmd.SysProcAttr).Elem().FieldByName("Pdeathsig")
	if !field.IsValid() {
		t.Logf("this platform (%s) has no Pdeathsig field — the backstop is Linux/FreeBSD-only", runtime.GOOS)
		return
	}
	if got := field.Int(); got != int64(syscall.SIGKILL) {
		t.Errorf("Pdeathsig = %d, want SIGKILL (%d): a SIGKILLed bunkerd must take cloudflared with it", got, syscall.SIGKILL)
	}
}

// TestPdeathsig_SIGKILLedManagerLeavesNoCloudflared is the end-to-end proof of
// criterion 3. A helper process runs the REAL TunnelManager.Start path against
// a mock cloudflared that records its pid, spawns a `sleep 300` child and waits;
// the parent then SIGKILLs the helper — no deferred function, no context
// cancellation, no stopTunnelCommand runs — and the cloudflared child must
// still be gone.
//
// SCOPE, measured rather than assumed: Pdeathsig is a parent-death signal for
// the DIRECT child, so the mock's own `sleep 300` grandchild is beyond the
// reach of a dead process and is logged as an observation (same documented
// scope as the CLI's GAP-079 SIGKILL test). The process that matters here is
// the direct one: that is cloudflared, and its death is what closes the tunnel.
func TestPdeathsig_SIGKILLedManagerLeavesNoCloudflared(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "freebsd" {
		t.Skipf("Pdeathsig is Linux/FreeBSD-only (this is %s)", runtime.GOOS)
	}

	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "cloudflared.pid")
	grandchildPIDFile := filepath.Join(tmp, "cloudflared-grandchild.pid")
	helperLog := filepath.Join(tmp, "helper.log")

	logFile, err := os.Create(helperLog)
	if err != nil {
		t.Fatalf("create %s: %v", helperLog, err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	helper := exec.Command(os.Args[0], "-test.run=^TestPdeathsigManagerHelperProcess$", "-test.v")
	helper.Env = append(os.Environ(),
		"BUNKER_TUNNEL_MGR_HELPER=1",
		"BUNKER_TUNNEL_MGR_DIR="+tmp,
		"BUNKER_TUNNEL_MGR_PIDFILE="+pidFile,
		"BUNKER_TUNNEL_MGR_GRANDCHILD_PIDFILE="+grandchildPIDFile,
	)
	helper.Stdout, helper.Stderr = logFile, logFile
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}

	childPID, grandchildPID := 0, 0
	t.Cleanup(func() {
		if helper.Process != nil {
			_ = helper.Process.Kill()
		}
		for _, pid := range []int{childPID, grandchildPID} {
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	childPID = waitForPIDFileIn(t, pidFile, 20*time.Second, helperLog)
	grandchildPID = waitForPIDFileIn(t, grandchildPIDFile, 20*time.Second, helperLog)
	assertProcessAlive(t, "the cloudflared child", childPID)
	assertProcessAlive(t, "the cloudflared grandchild", grandchildPID)

	// The uncatchable kill: nothing in the helper can run afterwards, so the
	// child's death can only come from the kernel.
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL the helper: %v", err)
	}
	waitProcessExit(t, helper, 10*time.Second, helperLog)

	waitProcessGone(t, "the cloudflared child", childPID, 5*time.Second)

	time.Sleep(300 * time.Millisecond)
	grandchildState := "gone"
	if err := syscall.Kill(grandchildPID, 0); err == nil {
		grandchildState = "still running (expected: Pdeathsig is direct-child only; a dead parent cannot signal anything)"
	}
	t.Logf("SIGKILL backstop: cloudflared child pid %d gone; grandchild pid %d %s", childPID, grandchildPID, grandchildState)
}

// TestPdeathsigManagerHelperProcess is not a test: it is the child-process entry
// point for TestPdeathsig_SIGKILLedManagerLeavesNoCloudflared. With the env
// flag set it starts a tunnel through the production TunnelManager.Start path
// and parks forever (the parent SIGKILLs it), holding the OS thread that
// created cloudflared — which is what keeps the parent-death signal armed.
func TestPdeathsigManagerHelperProcess(t *testing.T) {
	if os.Getenv("BUNKER_TUNNEL_MGR_HELPER") != "1" {
		t.Skip("helper process entry point (set BUNKER_TUNNEL_MGR_HELPER=1)")
	}

	dir := os.Getenv("BUNKER_TUNNEL_MGR_DIR")
	pidFile := os.Getenv("BUNKER_TUNNEL_MGR_PIDFILE")
	grandchildPIDFile := os.Getenv("BUNKER_TUNNEL_MGR_GRANDCHILD_PIDFILE")
	if dir == "" || pidFile == "" || grandchildPIDFile == "" {
		fmt.Fprintln(os.Stderr, "helper: BUNKER_TUNNEL_MGR_{DIR,PIDFILE,GRANDCHILD_PIDFILE} must be set")
		os.Exit(2)
	}

	binaryPath := writeMockCloudflaredWithPIDs(t, dir, pidFile, grandchildPIDFile)
	mgr := newTestManager(t, binaryPath, true)

	publicURL, err := mgr.Start(context.Background(), "agent-pdeathsig", 8080)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: Start: %v\n", err)
		os.Exit(3)
	}
	fmt.Fprintf(os.Stderr, "helper: tunnel registered at %s\n", publicURL)

	// Park with the pin held: the parent SIGKILLs this process, and the kernel
	// then SIGKILLs cloudflared. The sleep is only a safety valve (a test
	// binary that cannot be killed by its parent would otherwise hang forever).
	time.Sleep(5 * time.Minute)
}

// writeMockCloudflaredWithPIDs writes a mock cloudflared that records its own
// pid, prints the TryCloudflare banner (so TunnelManager.Start returns), spawns
// a long-lived grandchild and waits for it — the shape a real cloudflared has
// from the daemon's point of view: a long-lived direct child.
func writeMockCloudflaredWithPIDs(t *testing.T, dir, pidFile, grandchildPIDFile string) string {
	t.Helper()
	path := filepath.Join(dir, "cloudflared-pid")
	content := "#!/bin/sh\n" +
		"echo \"$$\" > \"" + pidFile + "\"\n" +
		"cat <<'EOF'\n" +
		"2026-06-28T00:00:00Z INF Starting cloudflared version 2024.6.1\n" +
		"|  Your quick tunnel has been created! Visit it at (it may take some time to be reachable):  |\n" +
		"|  https://pdeathsig-agent.trycloudflare.com                                                  |\n" +
		"EOF\n" +
		"sleep 300 &\n" +
		"echo \"$!\" > \"" + grandchildPIDFile + "\"\n" +
		"wait\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write mock cloudflared: %v", err)
	}
	return path
}

// waitForPIDFileIn waits for a pid file written by a fixture to appear with a
// positive pid, and returns it.
func waitForPIDFileIn(t *testing.T, path string, within time.Duration, helperLog string) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid in %s after %s (helper log:\n%s)", path, within, readFileOr(helperLog))
	return 0
}

// assertProcessAlive fails when pid is not running: the premise check that
// keeps the post-kill assertion from passing vacuously.
func assertProcessAlive(t *testing.T, name string, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("premise: %s (pid %d) is not running: %v", name, pid, err)
	}
}

// waitProcessExit waits for a helper process to exit, or fails naming its log.
func waitProcessExit(t *testing.T, proc *exec.Cmd, within time.Duration, logPath string) {
	t.Helper()
	waited := make(chan error, 1)
	go func() { waited <- proc.Wait() }()
	select {
	case <-waited:
	case <-time.After(within):
		t.Fatalf("the helper process did not exit within %s (log:\n%s)", within, readFileOr(logPath))
	}
}

// waitProcessGone polls pid until the kernel reports it gone, failing after
// within.
func waitProcessGone(t *testing.T, name string, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			return // ESRCH: gone
		}
		if time.Now().After(deadline) {
			t.Errorf("GAP-084: %s (pid %d) survived a SIGKILL to its parent — no parent-death backstop is armed", name, pid)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// readFileOr returns the file's contents, or the read error as text. (The CLI
// package has its own copy; the two packages' tests are compiled separately.)
func readFileOr(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return string(raw)
}
