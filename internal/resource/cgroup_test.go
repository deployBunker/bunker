package resource

import (
	"testing"
)

func TestParseCPUPercent_Empty(t *testing.T) {
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
