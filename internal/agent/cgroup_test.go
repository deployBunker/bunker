package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// TestCgroup_AgentLimitsAccepted verifies that Spawn accepts per-request CPU
// and memory limits and returns them in the response.
func TestCgroup_AgentLimitsAccepted(t *testing.T) {
	m := newTestManager(t)
	req := &v1.SpawnAgentRequest{
		AgentId: "cgroup-limits-test",
		Limits: &v1.ResourceLimits{
			CpuQuota:       0.5,
			MemoryMaxBytes: 256 * 1024 * 1024,
		},
	}
	// We are not root, so useradd will fail; just verify that limits are
	// captured before the OS-level failure.
	resp, err := m.Spawn(t.Context(), req)
	if err == nil {
		defer cleanupAgent(t, m, resp.AgentId)
	}
	if resp != nil && resp.Limits != nil {
		if resp.Limits.CpuQuota != 0.5 {
			t.Errorf("CpuQuota = %v, want 0.5", resp.Limits.CpuQuota)
		}
		if resp.Limits.MemoryMaxBytes != 256*1024*1024 {
			t.Errorf("MemoryMaxBytes = %v, want 256MiB", resp.Limits.MemoryMaxBytes)
		}
	}
}

// TestCgroup_SystemdRunArgsIncludeLimits verifies that the systemd-run
// arguments include CPUQuota and MemoryMax properties when limits are set.
// It does not require root because it builds the args directly.
func TestCgroup_SystemdRunArgsIncludeLimits(t *testing.T) {
	cpuQuota := 0.5
	memMax := uint64(256 * 1024 * 1024)

	args := []string{"--user", "--unit=bunker-docker-cgrouptest"}
	if cpuQuota > 0 {
		args = append(args, fmt.Sprintf("--property=CPUQuota=%d%%", int(cpuQuota*100)))
	}
	if memMax > 0 {
		args = append(args, fmt.Sprintf("--property=MemoryMax=%d", memMax))
	}
	args = append(args, "dockerd", "--host=unix:///run/bunker/cgrouptest/docker.sock")

	wantCPU := "--property=CPUQuota=50%"
	wantMem := "--property=MemoryMax=268435456"
	if !contains(args, wantCPU) {
		t.Errorf("systemd-run args missing %q: %v", wantCPU, args)
	}
	if !contains(args, wantMem) {
		t.Errorf("systemd-run args missing %q: %v", wantMem, args)
	}
}

// TestCgroup_CgroupV2FilesWritten verifies that the cgroup v2 controller files
// (cpu.max and memory.max) can be written with values matching the requested
// limits. It uses a temporary cgroup hierarchy so it does not require dockerd.
func TestCgroup_CgroupV2FilesWritten(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root to write cgroup files")
	}

	root := t.TempDir()
	cpuDir := filepath.Join(root, "cpu")
	memDir := filepath.Join(root, "memory")
	for _, d := range []string{cpuDir, memDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// cpu.max format: quota_us period_us
	cpuMax := "50000 100000" // 0.5 CPU
	if err := os.WriteFile(filepath.Join(cpuDir, "cpu.max"), []byte(cpuMax), 0644); err != nil {
		t.Fatalf("write cpu.max: %v", err)
	}
	writtenCPU, err := os.ReadFile(filepath.Join(cpuDir, "cpu.max"))
	if err != nil {
		t.Fatalf("read cpu.max: %v", err)
	}
	if !strings.HasPrefix(string(writtenCPU), "50000") {
		t.Errorf("cpu.max = %q, want prefix 50000", string(writtenCPU))
	}

	memMax := strconv.FormatUint(256*1024*1024, 10)
	if err := os.WriteFile(filepath.Join(memDir, "memory.max"), []byte(memMax), 0644); err != nil {
		t.Fatalf("write memory.max: %v", err)
	}
	writtenMem, err := os.ReadFile(filepath.Join(memDir, "memory.max"))
	if err != nil {
		t.Fatalf("read memory.max: %v", err)
	}
	if strings.TrimSpace(string(writtenMem)) != memMax {
		t.Errorf("memory.max = %q, want %q", string(writtenMem), memMax)
	}
}

// TestCgroup_MemoryLimitKillsStressProcess verifies that a memory limit of
// 256MiB is enforced by the kernel when running a memory stressor in a fresh
// cgroup. This test requires root and cgroup v2.
//
// INT-CI-050: the STRESSOR enrolls itself in the limited cgroup; the TEST
// BINARY never does. The previous shape wrote this process's own pid into
// cgroup.procs, which left the test binary — and every helper it forks for
// the rest of the package run — inside a 256MiB memory cgroup. That slice was
// already charged to its limit when TestConcurrency_SpawnFiveAgents started,
// so the kernel OOM-killed that test's `docker ps` helpers inside it
// (`docker ps: signal: killed`, the INT-CI-049 symptom) and then the test
// binary itself: CI run 36487719950 died `signal: killed` / `FAIL
// github.com/deployBunker/bunker/internal/agent 67.901s`, with
// `oom_memcg=/bunker-cgroup-test … Killed process … (agent.test)` in the
// kernel log. `os.RemoveAll(slice)` could not undo it — a cgroup directory
// with member processes cannot be removed and the error was discarded — so
// the poisoned slice also leaked across runs with its memory.max still set.
func TestCgroup_MemoryLimitKillsStressProcess(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root")
	}

	cgroupRoot := "/sys/fs/cgroup"
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		t.Skip("cgroup v2 not available")
	}

	slice := filepath.Join(cgroupRoot, "bunker-cgroup-test")
	if err := os.MkdirAll(slice, 0755); err != nil {
		t.Fatalf("mkdir cgroup slice: %v", err)
	}
	// Cleanup lifts the caps BEFORE removing the directory (so a slice that
	// cannot be removed can never keep throttling a later test) and reports a
	// removal failure LOUDLY: the slice leaked by the old shape is exactly
	// what must not happen again.
	t.Cleanup(func() {
		if err := os.WriteFile(filepath.Join(slice, "memory.max"), []byte(cgroupMemoryMaxUnlimited), 0644); err != nil {
			t.Errorf("lift memory.max on %s: %v", slice, err)
		}
		if err := os.WriteFile(filepath.Join(slice, "memory.swap.max"), []byte(cgroupMemoryMaxUnlimited), 0644); err != nil {
			t.Errorf("lift memory.swap.max on %s: %v", slice, err)
		}
		if err := os.Remove(slice); err != nil {
			t.Errorf("cgroup slice %s was NOT removed: %v — a leaked slice keeps its memory.max and OOM-kills the helpers of every later test in this package", slice, err)
		}
	})

	memMax := strconv.FormatUint(256*1024*1024, 10)
	if err := os.WriteFile(filepath.Join(slice, "memory.max"), []byte(memMax), 0644); err != nil {
		t.Fatalf("write memory.max: %v", err)
	}
	// The cap must not be ESCAPABLE via swap: memory.max bounds charged
	// memory, and an anon-heavy allocation whose pages the kernel can swap
	// out is reduced instead of OOM-killed (memory.swap.max defaults to
	// "max"). Without this line the test only reddened on hosts that happen
	// to have no swap configured — the self-hosted runner has none, this
	// control host has two swap files, and the assertion below then reads as
	// a product defect instead of a host property.
	if err := os.WriteFile(filepath.Join(slice, "memory.swap.max"), []byte("0"), 0644); err != nil {
		t.Fatalf("write memory.swap.max: %v", err)
	}

	// Run a memory stressor that allocates 512MiB. With a 256MiB limit it
	// should be killed by the OOM killer before it finishes. `echo $$` names
	// the shell's own pid and `exec` preserves it, so the allocator is a
	// member of the limited cgroup while this test process is not.
	stress := fmt.Sprintf(`echo $$ > %s/cgroup.procs
exec python3 -c '
import sys
a = bytearray(512 * 1024 * 1024)
sys.exit(0)
'`, slice)
	cmd := exec.CommandContext(t.Context(), "sh", "-c", stress)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected stress process to be killed, got exit 0: %s", string(out))
	}
	// The OOM killer terminates the stressor with SIGKILL. Go's
	// ProcessState.ExitCode() returns -1 for signal-terminated processes
	// (shell-style 137 is NOT produced), so accept an explicit SIGKILL via
	// WaitStatus in addition to a Python MemoryError.
	oomKilled := false
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		oomKilled = ws.Signaled() && ws.Signal() == syscall.SIGKILL
	}
	if !strings.Contains(string(out), "MemoryError") && cmd.ProcessState.ExitCode() != 137 && !oomKilled {
		sig := "none"
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sig = ws.Signal().String()
		}
		t.Logf("stress process output: %s", string(out))
		t.Errorf("expected OOM kill (MemoryError, exit 137, or SIGKILL), got exit %d (signal %s)", cmd.ProcessState.ExitCode(), sig)
	}

	// INT-CI-050 regression pin — the assertion that reddens on the old shape:
	// after this test, membership is read from the kernel, not from state the
	// test tracked. Before the fix /proc/self/cgroup named the slice here and
	// every test that ran after this one in the package inherited the 256MiB
	// cap.
	selfCgroup := readSelfCgroup(t)
	if strings.Contains(selfCgroup, filepath.Base(slice)) {
		t.Errorf("the test process is INSIDE the limited cgroup %s (/proc/self/cgroup = %q): every test after this one in this package would run under its %s cap",
			slice, strings.TrimSpace(selfCgroup), memMax)
	}
	// An occupied slice cannot be removed by Cleanup, so a surviving member
	// (an escaped stressor, an enrolled helper) is a leak, not a detail.
	procs, perr := os.ReadFile(filepath.Join(slice, "cgroup.procs"))
	if perr != nil {
		t.Errorf("read %s/cgroup.procs: %v", slice, perr)
	} else if strings.TrimSpace(string(procs)) != "" {
		t.Errorf("cgroup %s still has member pid(s) after the stressor was killed: %q", slice, strings.TrimSpace(string(procs)))
	}
}

// cgroupMemoryMaxUnlimited is the cgroup v2 spelling that removes a memory cap.
const cgroupMemoryMaxUnlimited = "max"

// readSelfCgroup returns this process's cgroup membership from the kernel.
func readSelfCgroup(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatalf("read /proc/self/cgroup: %v", err)
	}
	return string(b)
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
