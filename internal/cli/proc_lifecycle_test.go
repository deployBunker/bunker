//go:build unix

package cli

import (
	"bytes"
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

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── GAP-084: long-lived children must not outlive the CLI ───────────────────
//
// The four commands below each spawned a long-lived local child under a
// context they cancelled with `defer cancel()`, with no signal handling and no
// process group — so an abrupt CLI death ran nothing and the child was
// reparented to init (the GAP-079 defect class, fixed for `tunnel` and applied
// to cp/deploy/mount/ssh through internal/cli/proc.go).
//
// Every test here is a REAL-PROCESS test in the style of the GAP-079 tunnel
// tests: it builds the real CLI (`go build -o <tmp>/bunker ./cmd/bunker`), puts
// a fake child binary (`scp`/`sshfs`/`ssh`) first on PATH that records its own
// pid, spawns a `sleep 300` grandchild and waits for it, drives the command
// against the in-process mock bunkerd, then signals the CLI and asserts on the
// pid files. Each one asserts the fixtures were ALIVE before the signal, so
// none of them can pass vacuously with a dead fixture.

const procTestAgentID = "e2e-main"

// procTestMountCommand is the server-baked sshfs mount command the mock bunkerd
// returns for the agent: the shape the daemon stores (server-local key path and
// host), which the client rewrites. cp/deploy/ssh/mount all resolve the SSH
// target from this field.
const procTestMountCommand = "sshfs -o IdentityFile=/etc/bunkerd/ssh/e2e-main -o idmap=user -o allow_other bunker-e2e-main@bunker-mvp:/home/bunker-e2e-main /mnt/bunker/e2e-main"

// procTestHarness is the fixture set the GAP-084 real-process tests share: a
// mock bunkerd, the CLI's state dir (config + agent key), a directory for the
// fake child binaries and the built CLI.
type procTestHarness struct {
	tmp    string
	home   string
	binDir string
	cliBin string
	cliLog string
}

func newProcTestHarness(t *testing.T) *procTestHarness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-group / parent-death semantics")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not on PATH, cannot build the CLI under test: %v", err)
	}

	tmp := t.TempDir()

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:    procTestAgentID,
				SshfsMount: procTestMountCommand,
			},
		},
	})
	t.Cleanup(server.Close)

	// The CLI's state dir hangs off BUNKER_HOME (paths.go): config.yaml and the
	// agent key both live under it, so the child process never needs HOME —
	// overriding HOME would relocate `go build`'s module cache.
	home := filepath.Join(tmp, "bunker-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", home, err)
	}
	t.Setenv("BUNKER_HOME", home)
	writeTunnelTestConfig(t, home, server.URL)
	writeTunnelKey(t, home, procTestAgentID)

	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", binDir, err)
	}

	cliBin := filepath.Join(tmp, "bunker")
	build := exec.Command("go", "build", "-o", cliBin, "./cmd/bunker")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/bunker: %v\n%s", err, out)
	}

	cliLog := filepath.Join(tmp, "cli.log")
	logFile, err := os.Create(cliLog)
	if err != nil {
		t.Fatalf("create %s: %v", cliLog, err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	return &procTestHarness{tmp: tmp, home: home, binDir: binDir, cliBin: cliBin, cliLog: cliLog}
}

// env returns the CLI child's environment: the harness state dir plus the fake
// binaries first on PATH (the CLI spawns children BY NAME, so PATH is the seam
// that replaces scp/ssh/sshfs).
func (h *procTestHarness) env() []string {
	return append(os.Environ(),
		"BUNKER_HOME="+h.home,
		"PATH="+h.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
}

// startCLI starts the real CLI with args. stdout/stderr go to a PLAIN FILE, not
// a pipe: an orphaned child inherits the CLI's stdio, and a pipe would keep
// exec.Cmd.Wait blocked on the copy goroutine of a process we are hunting.
func (h *procTestHarness) startCLI(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	logFile, err := os.OpenFile(h.cliLog, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", h.cliLog, err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	cli := exec.Command(h.cliBin, args...)
	cli.Dir = h.tmp
	cli.Env = h.env()
	cli.Stdout, cli.Stderr = logFile, logFile

	if err := cli.Start(); err != nil {
		t.Fatalf("start CLI %v: %v", args, err)
	}
	return cli
}

// reapFixture is the cleanup every test registers: kill the CLI and any fixture
// that survived, so a failing (pre-fix) run does not itself leak the orphan the
// test exists to prevent.
func reapFixture(cli *exec.Cmd, pids ...*int) func() {
	return func() {
		if cli != nil && cli.Process != nil {
			_ = cli.Process.Kill()
		}
		for _, p := range pids {
			if p != nil && *p > 0 {
				_ = syscall.Kill(*p, syscall.SIGKILL)
			}
		}
	}
}

// writeFakeChildBinary installs a fake long-lived child (scp/ssh/sshfs) that
// records its own pid, spawns a grandchild that outlives any sane grace period,
// and waits for it — so it stays alive until it is killed.
func writeFakeChildBinary(t *testing.T, binDir, name string) (pidFile, grandchildPIDFile string) {
	t.Helper()
	pidFile = filepath.Join(binDir, name+".pid")
	grandchildPIDFile = filepath.Join(binDir, name+"-grandchild.pid")
	script := "#!/bin/sh\n" +
		"echo \"$$\" > \"" + pidFile + "\"\n" +
		"sleep 300 &\n" +
		"echo \"$!\" > \"" + grandchildPIDFile + "\"\n" +
		"wait\n"
	if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return pidFile, grandchildPIDFile
}

// assertAlive fails when pid is not running: the premise check that keeps every
// assertion below from passing vacuously.
func assertAlive(t *testing.T, name string, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("premise: %s (pid %d) is not running: %v", name, pid, err)
	}
}

// waitCLIExit waits for the CLI process to exit, or fails naming the log.
func waitCLIExit(t *testing.T, cli *exec.Cmd, within time.Duration, cliLog string) {
	t.Helper()
	waited := make(chan error, 1)
	go func() { waited <- cli.Wait() }()
	select {
	case <-waited:
	case <-time.After(within):
		t.Fatalf("the CLI did not exit within %s (log:\n%s)", within, readFileOr(cliLog))
	}
}

// waitForGone polls pid until the kernel reports it gone, failing after within.
func waitForGone(t *testing.T, name string, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			return // ESRCH: gone
		}
		if time.Now().After(deadline) {
			t.Errorf("GAP-084: %s (pid %d) survived: it was reparented to init and kept running after the CLI stopped", name, pid)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCpCommand_SIGTERMReapsSCPChild is the GAP-084 acceptance test for
// `bunker cp`: a SIGTERM to the CLI in the middle of a transfer must end the
// transfer (the scp child and everything it spawned) instead of leaving it
// running with no CLI attached.
func TestCpCommand_SIGTERMReapsSCPChild(t *testing.T) {
	h := newProcTestHarness(t)
	scpPIDFile, scpGrandchildPIDFile := writeFakeChildBinary(t, h.binDir, "scp")

	local := filepath.Join(h.tmp, "payload.bin")
	if err := os.WriteFile(local, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write %s: %v", local, err)
	}

	cli := h.startCLI(t, "cp", local, procTestAgentID+":/tmp/payload.bin")
	scpPID, scpGrandchildPID := 0, 0
	t.Cleanup(reapFixture(cli, &scpPID, &scpGrandchildPID))

	scpPID = waitForPIDFile(t, scpPIDFile, 15*time.Second, h.cliLog)
	scpGrandchildPID = waitForPIDFile(t, scpGrandchildPIDFile, 15*time.Second, h.cliLog)
	assertAlive(t, "the scp child", scpPID)
	assertAlive(t, "the scp grandchild", scpGrandchildPID)

	if err := cli.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM the CLI: %v", err)
	}
	waitCLIExit(t, cli, 10*time.Second, h.cliLog)

	// The load-bearing assertion: nothing the CLI started may survive it.
	waitForGone(t, "the scp child", scpPID, 5*time.Second)
	waitForGone(t, "the scp grandchild", scpGrandchildPID, 5*time.Second)
}

// TestDeployCommand_SIGTERMReapsSCPChild is the GAP-084 acceptance test for
// `bunker deploy`, the recursive twin of `bunker cp`: same two long-lived
// children (scp -r, then the chown ssh) and the same requirement — a SIGTERM
// must end them instead of leaving the recursive transfer running with no CLI
// attached.
func TestDeployCommand_SIGTERMReapsSCPChild(t *testing.T) {
	h := newProcTestHarness(t)
	scpPIDFile, scpGrandchildPIDFile := writeFakeChildBinary(t, h.binDir, "scp")

	local := filepath.Join(h.tmp, "payload-dir")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", local, err)
	}
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	cli := h.startCLI(t, "deploy", local, procTestAgentID+":/tmp/payload-dir")
	scpPID, scpGrandchildPID := 0, 0
	t.Cleanup(reapFixture(cli, &scpPID, &scpGrandchildPID))

	scpPID = waitForPIDFile(t, scpPIDFile, 15*time.Second, h.cliLog)
	scpGrandchildPID = waitForPIDFile(t, scpGrandchildPIDFile, 15*time.Second, h.cliLog)
	assertAlive(t, "the scp -r child", scpPID)
	assertAlive(t, "the scp -r grandchild", scpGrandchildPID)

	if err := cli.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM the CLI: %v", err)
	}
	waitCLIExit(t, cli, 10*time.Second, h.cliLog)

	waitForGone(t, "the scp -r child", scpPID, 5*time.Second)
	waitForGone(t, "the scp -r grandchild", scpGrandchildPID, 5*time.Second)
}

// TestMountCommand_SIGINTReapsSSHFSAttempt is the GAP-084 acceptance test for
// `bunker mount`: Ctrl-C (SIGINT) during the mount window must leave no sshfs
// attempt process and no ssh descendant behind.
//
// The established, daemonised sshfs mount that the command later reports is by
// design and is NOT part of this test: only the in-flight ATTEMPT is.
func TestMountCommand_SIGINTReapsSSHFSAttempt(t *testing.T) {
	h := newProcTestHarness(t)
	sshfsPIDFile, sshfsGrandchildPIDFile := writeFakeChildBinary(t, h.binDir, "sshfs")

	mountPoint := filepath.Join(h.tmp, "mnt")

	cli := h.startCLI(t, "mount", procTestAgentID, mountPoint)
	sshfsPID, sshfsGrandchildPID := 0, 0
	t.Cleanup(reapFixture(cli, &sshfsPID, &sshfsGrandchildPID))

	sshfsPID = waitForPIDFile(t, sshfsPIDFile, 15*time.Second, h.cliLog)
	sshfsGrandchildPID = waitForPIDFile(t, sshfsGrandchildPIDFile, 15*time.Second, h.cliLog)
	assertAlive(t, "the sshfs attempt", sshfsPID)
	assertAlive(t, "the sshfs attempt's ssh descendant", sshfsGrandchildPID)

	// Ctrl-C equivalent. The CLI's terminal is not shared with the child here
	// (no tty in the test), so this is the signal the CLI itself receives.
	if err := cli.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT the CLI: %v", err)
	}
	waitCLIExit(t, cli, 10*time.Second, h.cliLog)

	waitForGone(t, "the sshfs attempt", sshfsPID, 5*time.Second)
	waitForGone(t, "the sshfs attempt's ssh descendant", sshfsGrandchildPID, 5*time.Second)
}

// TestSSHCommand_SIGTERMReapsSSHChild is the GAP-084 acceptance test for
// `bunker ssh`: a manager-issued SIGTERM to the CLI alone (no signal reaches
// the child, which stays in the CLI's own process group for terminal reasons —
// see runTerminalChildCommand) must still end the session.
//
// SCOPE, measured rather than assumed: the interactive session deliberately
// does NOT get its own process group, so only the DIRECT ssh child is reaped
// here; the fake ssh's `sleep 300` grandchild is beyond the reach of a
// per-process signal and is logged as an observation. For the real binary the
// direct child IS the session: its death closes the TCP connection that the
// remote sshd session rides on.
func TestSSHCommand_SIGTERMReapsSSHChild(t *testing.T) {
	h := newProcTestHarness(t)
	sshPIDFile, sshGrandchildPIDFile := writeFakeChildBinary(t, h.binDir, "ssh")

	cli := h.startCLI(t, "ssh", procTestAgentID)
	sshPID, sshGrandchildPID := 0, 0
	t.Cleanup(reapFixture(cli, &sshPID, &sshGrandchildPID))

	sshPID = waitForPIDFile(t, sshPIDFile, 15*time.Second, h.cliLog)
	sshGrandchildPID = waitForPIDFile(t, sshGrandchildPIDFile, 15*time.Second, h.cliLog)
	assertAlive(t, "the ssh child", sshPID)
	assertAlive(t, "the ssh grandchild", sshGrandchildPID)

	if err := cli.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM the CLI: %v", err)
	}
	waitCLIExit(t, cli, 10*time.Second, h.cliLog)

	waitForGone(t, "the ssh child", sshPID, 5*time.Second)

	time.Sleep(300 * time.Millisecond)
	grandchildState := "gone"
	if err := syscall.Kill(sshGrandchildPID, 0); err == nil {
		grandchildState = "still running (expected: the session child shares the CLI's process group, so only it is signalled)"
	}
	t.Logf("SIGTERM teardown: ssh child pid %d gone; grandchild pid %d %s", sshPID, sshGrandchildPID, grandchildState)
}

// TestRunDetachedChildCommand_GroupKillsSIGTERMIgnoringChild pins the GROUP
// semantics of the shared helper with a REAL child tree: a shell that IGNORES
// SIGTERM and the `sleep` it starts (the ignored disposition is inherited
// across fork+exec, so both ignore it). Liveness of both is asserted after
// sending SIGTERM straight at the shell, then the context is cancelled and the
// test asserts the whole group died — including the descendant, which only a
// GROUP signal can reach.
//
// WHY the descendant matters: a leader-only teardown (exec's default Cancel, or
// a terminate that signals the pid instead of -pgid) still ends the leader —
// exec's WaitDelay SIGKILLs it — so a test that looked only at the leader would
// pass while the child's descendants kept running. The descendant here ignores
// SIGTERM as well, so the group SIGKILL is the only thing that can end it, and
// the elapsed-time assertion pins that it arrived after the grace rather than
// via the first signal.
func TestRunDetachedChildCommand_GroupKillsSIGTERMIgnoringChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-group semantics")
	}

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	descendantPIDFile := filepath.Join(t.TempDir(), "child-descendant.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The command carries its own context: that is the one the exec package
	// watches, and cancelling it is what runs the group teardown under test.
	//
	// `trap '' TERM` makes the shell ignore SIGTERM, and the ignored
	// disposition is inherited across fork+exec — so the shell AND the `sleep`
	// it starts both ignore SIGTERM, and only a group SIGKILL can end them.
	cmd := newLongLivedCommand(ctx, "sh", "-c",
		fmt.Sprintf("trap '' TERM; sleep 300 & echo $! > '%s'; echo $$ > '%s'; wait", descendantPIDFile, pidFile))

	done := make(chan error, 1)
	go func() { done <- runDetachedChildCommand(cmd) }()

	pid := waitForPIDFile(t, pidFile, 10*time.Second, "")
	descendantPID := waitForPIDFile(t, descendantPIDFile, 10*time.Second, "")
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_ = syscall.Kill(descendantPID, syscall.SIGKILL)
	})
	assertAlive(t, "the SIGTERM-ignoring child", pid)
	assertAlive(t, "the child's SIGTERM-ignoring descendant", descendantPID)

	// Live group wiring: the child must be the LEADER of its own process group.
	// Asserting the SysProcAttr field alone would not prove Start() applied it.
	if pgrp, ok := processGroupOf(pid); ok {
		if pgrp != pid {
			t.Errorf("the child's process group is %d, want its own pid %d: Setpgid did not take effect", pgrp, pid)
		}
		if own := syscall.Getpgrp(); pgrp == own {
			t.Errorf("the child shares the test process's group (%d): a group teardown would reach the parent too", own)
		}
	} else {
		t.Logf("no /proc on this platform: the live process-group assertion was skipped")
	}

	// Premise: this child really does ignore SIGTERM (so the SIGKILL backstop,
	// not SIGTERM, is what ends it below).
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM the child: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	assertAlive(t, "the SIGTERM-ignoring child after a direct SIGTERM", pid)

	start := time.Now()
	cancel()

	select {
	case err := <-done:
		_ = err // a killed child reports its exit status; the point is that it ended
	case <-time.After(10 * time.Second):
		t.Fatalf("runDetachedChildCommand did not return within 10s (log: the group teardown never reaped the child)")
	}

	elapsed := time.Since(start)
	if elapsed < childShutdownGrace {
		t.Errorf("the child was reaped in %s, less than the %s grace: SIGTERM cannot have ended a child that ignores it, so the group SIGKILL backstop did not run",
			elapsed, childShutdownGrace)
	}
	waitForGone(t, "the SIGTERM-ignoring child", pid, 5*time.Second)
	waitForGone(t, "the child's SIGTERM-ignoring descendant", descendantPID, 5*time.Second)
}

// processGroupOf reads a process's group id out of /proc (the "pgrp" field of
// stat). ok is false when /proc is unavailable, so the caller can say the
// assertion was skipped rather than pass it vacuously.
func processGroupOf(pid int) (pgrp int, ok bool) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	// The comm field is parenthesised and may itself contain spaces: cut at the
	// LAST ')' and index from there (state, ppid, pgrp, ...).
	rest := raw
	if i := bytes.LastIndexByte(raw, ')'); i >= 0 {
		rest = raw[i+1:]
	}
	fields := strings.Fields(string(rest))
	if len(fields) < 3 {
		return 0, false
	}
	value, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, false
	}
	return value, true
}

// TestConfigureLongLivedChild_ProcessGroupAndPdeathsigWiring pins the wiring of
// the shared helper: the detached form (cp/deploy/mount/tunnel) must own a
// process group AND arm the parent-death backstop, while the terminal form
// (interactive ssh) must arm the backstop and must NOT own a group.
//
// The Pdeathsig field only exists on Linux/FreeBSD, so it is read
// reflectively: this file stays buildable on every GOOS.
func TestConfigureLongLivedChild_ProcessGroupAndPdeathsigWiring(t *testing.T) {
	detached := exec.Command("true")
	configureDetachedChild(detached)
	if detached.SysProcAttr == nil {
		t.Fatal("configureDetachedChild left SysProcAttr nil: no process group, no parent-death backstop")
	}
	if !detached.SysProcAttr.Setpgid {
		t.Error("configureDetachedChild must set Setpgid: the group teardown needs a group of its own")
	}
	assertPdeathsigArmed(t, "configureDetachedChild", detached)

	terminal := exec.Command("true")
	configureTerminalChild(terminal)
	if terminal.SysProcAttr != nil && terminal.SysProcAttr.Setpgid {
		t.Error("configureTerminalChild must NOT set Setpgid: the interactive ssh child has to stay in the terminal's foreground process group or the session is stopped by SIGTTIN/SIGTTOU")
	}
	assertPdeathsigArmed(t, "configureTerminalChild", terminal)
}

// assertPdeathsigArmed checks the kernel backstop is armed, on the platforms
// that have one.
func assertPdeathsigArmed(t *testing.T, fn string, cmd *exec.Cmd) {
	t.Helper()
	if cmd.SysProcAttr == nil {
		t.Fatalf("%s: SysProcAttr is nil, so no Pdeathsig could be armed", fn)
	}
	field := reflect.ValueOf(cmd.SysProcAttr).Elem().FieldByName("Pdeathsig")
	if !field.IsValid() {
		t.Logf("%s: this platform (%s) has no Pdeathsig field — the backstop is Linux/FreeBSD-only", fn, runtime.GOOS)
		return
	}
	if got := field.Int(); got != int64(syscall.SIGKILL) {
		t.Errorf("%s: Pdeathsig = %d, want SIGKILL (%d): a SIGKILLed CLI must take its child with it", fn, got, syscall.SIGKILL)
	}
}

// TestLongLivedChild_PdeathsigBackstopReapsDirectChild proves the kernel
// backstop with a REAL child process: a helper process starts a child through
// the shared contract (so the creating OS thread is pinned for the child's
// whole life) and is then SIGKILLed — the case where nothing in the parent can
// run. The child must still be gone.
//
// Without the pin this test is the random-kill bug in reverse: Pdeathsig fires
// when the THREAD that created the child exits, so an unpinned start would let
// the child outlive (or randomly outlive) its parent.
func TestLongLivedChild_PdeathsigBackstopReapsDirectChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX parent-death semantics")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "freebsd" {
		t.Skipf("Pdeathsig is Linux/FreeBSD-only (this is %s)", runtime.GOOS)
	}

	pidFile := filepath.Join(t.TempDir(), "grandchild-of-helper.pid")
	helperLog := filepath.Join(t.TempDir(), "helper.log")

	logFile, err := os.Create(helperLog)
	if err != nil {
		t.Fatalf("create %s: %v", helperLog, err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	helper := exec.Command(os.Args[0], "-test.run=^TestLongLivedChildHelperProcess$", "-test.v")
	helper.Env = append(os.Environ(), "BUNKER_PROC_HELPER=1", "BUNKER_PROC_HELPER_PIDFILE="+pidFile)
	helper.Stdout, helper.Stderr = logFile, logFile
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	t.Cleanup(func() {
		if helper.Process != nil {
			_ = helper.Process.Kill()
		}
	})

	childPID := waitForPIDFile(t, pidFile, 15*time.Second, helperLog)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	assertAlive(t, "the helper's child", childPID)

	// The uncatchable kill: nothing in the helper can run afterwards, so the
	// child's death can only come from the kernel.
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL the helper: %v", err)
	}
	waitCLIExit(t, helper, 10*time.Second, helperLog)

	waitForGone(t, "the helper's child", childPID, 5*time.Second)
}

// TestLongLivedChildHelperProcess is not a test: it is the child-process entry
// point for TestLongLivedChild_PdeathsigBackstopReapsDirectChild. With the env
// flag set it starts one child through the shared contract and parks forever
// (the parent SIGKILLs it), which is exactly what keeps the parent-death signal
// armed.
func TestLongLivedChildHelperProcess(t *testing.T) {
	if os.Getenv("BUNKER_PROC_HELPER") != "1" {
		t.Skip("helper process entry point (set BUNKER_PROC_HELPER=1)")
	}

	pidFile := os.Getenv("BUNKER_PROC_HELPER_PIDFILE")
	if pidFile == "" {
		fmt.Fprintln(os.Stderr, "helper: BUNKER_PROC_HELPER_PIDFILE is not set")
		os.Exit(2)
	}

	// `exec sleep 300` replaces the shell, so the recorded pid IS the direct
	// child — the process the kernel's Pdeathsig is armed on.
	cmd := newLongLivedCommand(context.Background(), "sh", "-c",
		fmt.Sprintf("echo $$ > '%s'; exec sleep 300", pidFile))
	err := runDetachedChildCommand(cmd)
	fmt.Fprintf(os.Stderr, "helper: runDetachedChildCommand returned %v\n", err)
	os.Exit(0)
}
