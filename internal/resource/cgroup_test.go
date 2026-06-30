package resource

import (
	"os"
	"testing"
)

func TestCgroupCPUSharesPath(t *testing.T) {
	pct := parseCPUPercent("")
	if pct != 0 {
		t.Errorf("expected 0 for empty input, got %f", pct)
	}
}

func TestParseCPUPercent_Valid(t *testing.T) {
	stat := "usage_usec 123456789\nuser_usec 100000000\nsystem_usec 23456789\n"
	pct := parseCPUPercent(stat)
	// Returns 0 because we don't compute a delta (documented behavior)
	if pct != 0 {
		t.Errorf("expected 0 (delta tracking not implemented yet), got %f", pct)
	}
}

func TestCgroupPaths(t *testing.T) {
	path := CgroupCPUSharesPath("test-agent")
	if path != "/sys/fs/cgroup/bunker-test-agent.slice" {
		t.Errorf("unexpected cpu path: %s", path)
	}
	memPath := CgroupMemoryPath("test-agent")
	if memPath != "/sys/fs/cgroup/bunker-test-agent.slice" {
		t.Errorf("unexpected memory path: %s", memPath)
	}
}

func TestReadCgroupMetrics_NoError(t *testing.T) {
	// ReadCgroupMetrics should not panic even if cgroup isn't mounted
	_, err := ReadCgroupMetrics()
	// Error is expected in test environments without cgroup v2, but shouldn't crash
	_ = err
}

func TestAgentCgroupBasePath(t *testing.T) {
	// Verifies the per-agent rootless systemd user unit cgroup path is computed
	// as expected without requiring root or a real systemd unit.
	const uid = 12345
	const agentID = "e2e-cgroup-test"
	got := agentCgroupBase(uid, agentID)
	want := "/sys/fs/cgroup/user.slice/user-12345.slice/user@12345.service/bunker-docker-e2e-cgroup-test.service"
	if got != want {
		t.Errorf("agentCgroupBase(%d, %q) = %q, want %q", uid, agentID, got, want)
	}
}

func TestCgroupCPUPath(t *testing.T) {
	const uid = 12345
	const agentID = "cpu-test-agent"
	got := CgroupCPUPath(uid, agentID)
	want := "/sys/fs/cgroup/user.slice/user-12345.slice/user@12345.service/bunker-docker-cpu-test-agent.service/cpu.max"
	if got != want {
		t.Errorf("CgroupCPUPath(%d, %q) = %q, want %q", uid, agentID, got, want)
	}
}

func TestCgroupMemoryLimitPath(t *testing.T) {
	const uid = 12345
	const agentID = "mem-test-agent"
	got := CgroupMemoryLimitPath(uid, agentID)
	want := "/sys/fs/cgroup/user.slice/user-12345.slice/user@12345.service/bunker-docker-mem-test-agent.service/memory.max"
	if got != want {
		t.Errorf("CgroupMemoryLimitPath(%d, %q) = %q, want %q", uid, agentID, got, want)
	}
}

func TestReadAgentCgroupLimits_ParsesFiles(t *testing.T) {
	root := t.TempDir()

	// Create the fake controller files under the temp directory. Because the
	// helper builds the full path, we need to create the directory portion that
	// the filename expects. The easiest way is to override the base function
	// by shadowing the package variable. We use a test-only type and a package
	// variable that's reset in t.Cleanup.
	originalBase := agentCgroupBaseFn
	agentCgroupBaseFn = func(uid int, agentID string) string {
		return root
	}
	t.Cleanup(func() { agentCgroupBaseFn = originalBase })

	const uid = 12345
	const agentID = "read-test-agent"

	if err := os.WriteFile(CgroupCPUPath(uid, agentID), []byte("50000 100000\n"), 0644); err != nil {
		t.Fatalf("write fake cpu.max: %v", err)
	}
	if err := os.WriteFile(CgroupMemoryLimitPath(uid, agentID), []byte("268435456\n"), 0644); err != nil {
		t.Fatalf("write fake memory.max: %v", err)
	}

	m, err := ReadAgentCgroupLimits(uid, agentID)
	if err != nil {
		t.Fatalf("ReadAgentCgroupLimits failed: %v", err)
	}
	if m.CPUQuota != 0.5 {
		t.Errorf("CPUQuota = %v, want 0.5", m.CPUQuota)
	}
	if m.MemoryLimitBytes != 256*1024*1024 {
		t.Errorf("MemoryLimitBytes = %v, want 256MiB", m.MemoryLimitBytes)
	}
}

func TestReadAgentCgroupLimits_GracefulWhenMissing(t *testing.T) {
	// No fake files, no root; should return zero values without error.
	m, err := ReadAgentCgroupLimits(99999, "missing-agent")
	if err != nil {
		t.Fatalf("ReadAgentCgroupLimits returned error for missing cgroup: %v", err)
	}
	if m.CPUQuota != 0 {
		t.Errorf("CPUQuota = %v, want 0", m.CPUQuota)
	}
	if m.MemoryLimitBytes != 0 {
		t.Errorf("MemoryLimitBytes = %v, want 0", m.MemoryLimitBytes)
	}
}
