package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file pins the GAP-120 destroy contract: BEFORE the stop/userdel
// sequence, Destroy writes "1" to the agent user slice's cgroup.kill,
// atomically SIGKILLing the whole subtree so no process can outrun teardown.
// Every test drives the REAL Destroy over the same seams the existing
// destroy tests use (presentUserWithUID, stubDestroyProcessProbe,
// stubUserdelRunner, pointHomeAt) with the kill path pointed at a fixture
// directory — never the host's /sys/fs/cgroup.

// gap120KillFixture builds a fixture directory shaped like a live cgroup v2
// slice (cgroup.controllers probe file + a pre-created cgroup.kill holding
// the kernel's "0") and routes the production kill path (agentKillSlicePath)
// at it. Returns the slice directory and its kill-file path.
func gap120KillFixture(t *testing.T) (slice, killFile string) {
	t.Helper()
	slice = t.TempDir()
	if err := os.WriteFile(filepath.Join(slice, "cgroup.controllers"), []byte("cpuset cpu io memory pids\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	killFile = filepath.Join(slice, "cgroup.kill")
	if err := os.WriteFile(killFile, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := agentKillSlicePath
	agentKillSlicePath = func(int) string { return slice }
	t.Cleanup(func() { agentKillSlicePath = orig })
	return slice, killFile
}

// gap120EmptyRootFixture points the kill path at a directory that does not
// exist — the ErrCgroupKillUnavailable class (no live cgroup v2 directory
// for the agent uid).
func gap120EmptyRootFixture(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "user.slice", "user-61001.slice")
	orig := agentKillSlicePath
	agentKillSlicePath = func(int) string { return missing }
	t.Cleanup(func() { agentKillSlicePath = orig })
}

// gap120Stage is the common GAP-120 destroy scene: a live agent whose user
// resolves to uid 61001, an empty process list (the DF-34 gate passes), no
// home to archive, and a userdel recorder (log path returned).
func gap120Stage(t *testing.T, m *AgentManager, id, username string) string {
	t.Helper()
	rev003Setup(t, m, id, username)
	logPath := filepath.Join(t.TempDir(), "userdel.log")
	stubUserdelRunner(t, func(_ context.Context, uname string) ([]byte, error) {
		return gap120Append(t, logPath, "userdel "+uname), nil
	})
	return logPath
}

// gap120Append appends a line to logPath; the stub hands back nil (a
// successful userdel prints nothing).
func gap120Append(t *testing.T, logPath, line string) []byte {
	t.Helper()
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
	return nil
}

// gap120Calls reads back the recorder log.
func gap120Calls(t *testing.T, logPath string) []string {
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

// TestDestroy_CgroupKillFiresBeforeUserdel is acceptance criterion 4: with a
// resolvable agent user and a live fixture cgroup, Destroy writes "1" to the
// slice's cgroup.kill and the write happens BEFORE the userdel call. The
// order is proven from the filesystem, not from timestamps: the userdel stub
// snapshots the kill file's content AT USERDEL TIME into a sibling marker,
// so if the kill ever ran after userdel the snapshot would read the
// pre-write "0".
func TestDestroy_CgroupKillFiresBeforeUserdel(t *testing.T) {
	restore := shrinkRollbackBudgets(t, time.Second, 400*time.Millisecond, 100*time.Millisecond)
	defer restore()

	const id = "gap120-order"
	username := "bunker-" + id
	m, _ := df63Manager(t, nil)

	slice, killFile := gap120KillFixture(t)
	gap120Stage(t, m, id, username)

	marker := slice + ".userdel-saw"
	t.Cleanup(func() { _ = os.Remove(marker) })
	// Replace the recorder with the ORDER-PROBE stub (same contract: a
	// successful userdel returns nil).
	stubUserdelRunner(t, func(_ context.Context, uname string) ([]byte, error) {
		snapshot, rerr := os.ReadFile(killFile)
		if rerr != nil {
			snapshot = []byte("MISSING")
		}
		if werr := os.WriteFile(marker, snapshot, 0o644); werr != nil {
			t.Errorf("userdel stub could not record the kill-file snapshot: %v", werr)
		}
		return gap120Append(t, filepath.Join(t.TempDir(), "ignored.log"), "userdel "+uname), nil
	})

	resp, derr := m.Destroy(context.Background(), id, false)
	if derr != nil {
		t.Fatalf("Destroy() error = %v", derr)
	}
	if resp == nil || resp.Status != "destroyed" {
		t.Fatalf("Destroy() status = %v, want destroyed", resp)
	}

	// 1: the kill file was written with "1" — the kill step ran.
	data, rerr := os.ReadFile(killFile)
	if rerr != nil {
		t.Fatalf("cgroup.kill was never written: %v", rerr)
	}
	if got := strings.TrimSpace(string(data)); got != "1" {
		t.Fatalf("cgroup.kill = %q, want %q", got, "1")
	}
	// 2: the userdel call — which is AFTER the kill step — observed the
	// written kill file. A kill that ran after userdel would leave "0".
	snap, merr := os.ReadFile(marker)
	if merr != nil {
		t.Fatalf("userdel stub never recorded its snapshot: %v", merr)
	}
	if got := strings.TrimSpace(string(snap)); got != "1" {
		t.Fatalf("userdel observed cgroup.kill = %q, want %q (the kill must precede userdel)", got, "1")
	}
}

// TestDestroy_CgroupKillUnavailableFallsThrough is acceptance criterion 5:
// a host whose agent uid has no live cgroup v2 directory (an older kernel,
// or the slice already gone) must WARN — the log carries the fallback line —
// and then run the existing destroy sequence to completion (userdel still
// issued, status destroyed). The destroy NEVER aborts on an unavailable kill.
func TestDestroy_CgroupKillUnavailableFallsThrough(t *testing.T) {
	restore := shrinkRollbackBudgets(t, time.Second, 400*time.Millisecond, 100*time.Millisecond)
	defer restore()

	const id = "gap120-nocg2"
	username := "bunker-" + id
	var buf bytes.Buffer
	m, _ := df63Manager(t, &buf)

	gap120EmptyRootFixture(t)
	logPath := gap120Stage(t, m, id, username)

	resp, derr := m.Destroy(context.Background(), id, false)
	if derr != nil {
		t.Fatalf("Destroy() error = %v (an unavailable cgroup.kill must never abort the destroy)", derr)
	}
	if resp == nil || resp.Status != "destroyed" {
		t.Fatalf("Destroy() status = %v, want destroyed", resp)
	}
	if calls := gap120Calls(t, logPath); len(calls) != 1 {
		t.Errorf("userdel calls = %v, want exactly one (the fallback destroy completed)", calls)
	}
	log := buf.String()
	for _, want := range []string{
		"cgroup.kill unavailable",
		"falling back",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("destroy log missing fallback warning %q; log:\n%s", want, log)
		}
	}
}

// TestDestroy_CgroupKillWriteFailureContinues covers the third arm: the
// cgroup directory exists but the write is REFUSED (kill file left
// read-only by the fixture — the EACCES class). The destroy continues
// exactly as before: userdel still runs, status destroyed, and the refusal
// is logged as a write-failure warning — NOT the unavailable fallback.
func TestDestroy_CgroupKillWriteFailureContinues(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only kill file cannot be simulated")
	}
	restore := shrinkRollbackBudgets(t, time.Second, 400*time.Millisecond, 100*time.Millisecond)
	defer restore()

	const id = "gap120-eacces"
	username := "bunker-" + id
	var buf bytes.Buffer
	m, _ := df63Manager(t, &buf)

	_, killFile := gap120KillFixture(t)
	if err := os.Chmod(killFile, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(killFile, 0o644) })
	logPath := gap120Stage(t, m, id, username)

	resp, derr := m.Destroy(context.Background(), id, false)
	if derr != nil {
		t.Fatalf("Destroy() error = %v (a refused kill write must never abort the destroy)", derr)
	}
	if resp == nil || resp.Status != "destroyed" {
		t.Fatalf("Destroy() status = %v, want destroyed", resp)
	}
	if calls := gap120Calls(t, logPath); len(calls) != 1 {
		t.Errorf("userdel calls = %v, want exactly one (the destroy continued)", calls)
	}
	log := buf.String()
	if !strings.Contains(log, "cgroup.kill failed") {
		t.Errorf("destroy log missing the write-failure warning; log:\n%s", log)
	}
	if strings.Contains(log, "cgroup.kill unavailable") {
		t.Errorf("write failure misclassified as the unavailable fallback; log:\n%s", log)
	}
}

// TestDestroy_CgroupKillUserAbsentSkipsKill keeps the idempotent path
// untouched: an agent whose user record is already gone owns no slice — the
// kill step must skip without any kill warning, and the destroy still
// completes.
func TestDestroy_CgroupKillUserAbsentSkipsKill(t *testing.T) {
	restore := shrinkRollbackBudgets(t, time.Second, 400*time.Millisecond, 100*time.Millisecond)
	defer restore()

	const id = "gap120-gone"
	var buf bytes.Buffer
	m, _ := df63Manager(t, &buf)

	// The slice fixture is in place, but NO user record resolves (the probe
	// stub reports the user absent → the kill step's lookupUser gate skips).
	// The fixture is a REAL directory so a wrongly-fired kill would be
	// visible in the file; the skip is what the test asserts.
	_, killFile := gap120KillFixture(t)
	liveAgent(t, m, id)
	qa61RegistryLive(t, m, id)
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return nil, 0, false, nil // user record gone: ok=false
	})
	pointHomeAt(t, t.TempDir())
	logPath := filepath.Join(t.TempDir(), "userdel.log")
	stubUserdelRunner(t, func(_ context.Context, uname string) ([]byte, error) {
		return gap120Append(t, logPath, "userdel "+uname), nil
	})

	resp, derr := m.Destroy(context.Background(), id, false)
	if derr != nil {
		t.Fatalf("Destroy() error = %v", derr)
	}
	if resp == nil || resp.Status != "destroyed" {
		t.Fatalf("Destroy() status = %v, want destroyed", resp)
	}
	// The skip is a Debug line, not a Warn: no kill warning may appear.
	log := buf.String()
	if strings.Contains(log, "cgroup.kill failed") || strings.Contains(log, "cgroup.kill unavailable") {
		t.Errorf("absent-user destroy logged a kill warning; log:\n%s", log)
	}
	// And the kill never fired: the fixture's kill file still holds the
	// kernel's pre-write "0".
	data, rerr := os.ReadFile(killFile)
	if rerr != nil {
		t.Fatalf("read kill file: %v", rerr)
	}
	if got := strings.TrimSpace(string(data)); got != "0" {
		t.Errorf("cgroup.kill = %q, want untouched %q (no kill for an absent user)", got, "0")
	}
}

// TestKillAgentCgroupSubtree_IntegrationShape is acceptance criterion 4's
// integration shape: a REAL fork loop (a shell that re-forks in a tight
// loop — the exact process class that outruns a stop-then-kill teardown) is
// running while the production helper performs the documented write; the
// test proves the helper fires without touching the ambient host state
// (fixture path only) and reaps the loop deterministically. The fixture
// cgroup cannot enroll unprivileged real processes, so the kill-delivery
// proof stays at the file level here; the kernel semantics of cgroup.kill
// are exercised end-to-end by the live-server E2E battery on bunker-mvp
// (AGENTS.md gate 3 scope: destroy behavior changes).
func TestKillAgentCgroupSubtree_IntegrationShape(t *testing.T) {
	_, killFile := gap120KillFixture(t)

	// A fork loop: the shell replaces itself with sleep in a tight loop and
	// a child keeps forking — bounded lifetime, torn down by the deferred
	// kill; the test only needs it ALIVE while the helper runs.
	forkLoop := exec.Command("sh", "-c", `end=$((SECONDS+5)); while [ $SECONDS -lt $end ]; do sleep 0.1 & wait $!; done`)
	if err := forkLoop.Start(); err != nil {
		t.Fatalf("start fork loop: %v", err)
	}
	defer func() { _ = forkLoop.Process.Kill() }()

	m, _ := df63Manager(t, nil)
	username := "bunker-integ120"
	presentUserWithUID(t, username, "61001")

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.killAgentCgroupSubtree(context.Background(), username, "integ120", m.logger)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("killAgentCgroupSubtree did not return within 10s while a fork loop was live")
	}

	// The documented write happened, against the AGENT's own fixture slice.
	data, err := os.ReadFile(killFile)
	if err != nil {
		t.Fatalf("cgroup.kill was never written: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "1" {
		t.Fatalf("cgroup.kill = %q, want %q", got, "1")
	}
}
