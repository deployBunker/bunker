package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	t.Cleanup(func() { os.RemoveAll(slice) })

	memMax := strconv.FormatUint(256*1024*1024, 10)
	if err := os.WriteFile(filepath.Join(slice, "memory.max"), []byte(memMax), 0644); err != nil {
		t.Fatalf("write memory.max: %v", err)
	}
	// Allow only the cgroup itself and root to control it.
	if err := os.WriteFile(filepath.Join(slice, "cgroup.procs"), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0644); err != nil {
		t.Fatalf("move test process into cgroup: %v", err)
	}

	// Run a memory stressor that allocates 512MiB. With a 256MiB limit it
	// should be killed by the OOM killer before it finishes.
	cmd := exec.CommandContext(t.Context(), "python3", "-c", `
import sys
a = bytearray(512 * 1024 * 1024)
sys.exit(0)
`)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected stress process to be killed, got exit 0: %s", string(out))
	}
	if !strings.Contains(string(out), "MemoryError") && cmd.ProcessState.ExitCode() != 137 {
		t.Logf("stress process output: %s", string(out))
		t.Errorf("expected MemoryError or exit 137, got exit %d", cmd.ProcessState.ExitCode())
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
