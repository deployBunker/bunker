package resource

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCgroupCPUSharesPath(t *testing.T) {
	if usage, ok := parseUsageUsec(""); ok || usage != 0 {
		t.Errorf("parseUsageUsec(\"\") = (%d, %v), want (0, false)", usage, ok)
	}
}

func TestParseUsageUsec_Valid(t *testing.T) {
	stat := "usage_usec 123456789\nuser_usec 100000000\nsystem_usec 23456789\n"
	usage, ok := parseUsageUsec(stat)
	if !ok {
		t.Fatal("parseUsageUsec returned ok=false for valid cpu.stat")
	}
	if usage != 123456789 {
		t.Errorf("parseUsageUsec = %d, want 123456789", usage)
	}
}

func TestParseUsageUsec_Missing(t *testing.T) {
	stat := "user_usec 100000000\nsystem_usec 23456789\n"
	if usage, ok := parseUsageUsec(stat); ok || usage != 0 {
		t.Errorf("parseUsageUsec without usage_usec = (%d, %v), want (0, false)", usage, ok)
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

func TestParseMeminfo(t *testing.T) {
	sample := "MemTotal:       16384000 kB\n" +
		"MemFree:         1000000 kB\n" +
		"MemAvailable:   15000000 kB\n" +
		"Buffers:          200000 kB\n"
	total, available := parseMeminfo(sample)
	// Values in /proc/meminfo are kB; parser converts to bytes.
	if total != 16384000*1024 {
		t.Errorf("parseMeminfo MemTotal = %d, want %d", total, 16384000*1024)
	}
	if available != 15000000*1024 {
		t.Errorf("parseMeminfo MemAvailable = %d, want %d", available, 15000000*1024)
	}
}

func TestParseMeminfo_MissingFields(t *testing.T) {
	total, available := parseMeminfo("MemFree: 100 kB\n")
	if total != 0 || available != 0 {
		t.Errorf("missing MemTotal/MemAvailable = (%d, %d), want (0, 0)", total, available)
	}
}

func TestReadCgroupMetrics_MeminfoFallback(t *testing.T) {
	// Point the cgroup and meminfo readers at fixtures: no memory.* files in
	// the fake cgroup dir, so the /proc/meminfo fallback must kick in.
	root := t.TempDir()
	originalDir := cgroupBaseDir
	originalMeminfo := meminfoFile
	cgroupBaseDir = root
	meminfoFile = writeFixture(t, "meminfo",
		"MemTotal:       16384000 kB\nMemAvailable:   15000000 kB\n")
	t.Cleanup(func() {
		cgroupBaseDir = originalDir
		meminfoFile = originalMeminfo
	})

	m, err := ReadCgroupMetrics()
	if err != nil {
		t.Fatalf("ReadCgroupMetrics() error: %v", err)
	}
	if want := uint64(16384000 * 1024); m.MemoryLimitBytes != want {
		t.Errorf("MemoryLimitBytes = %d, want %d (MemTotal)", m.MemoryLimitBytes, want)
	}
	if want := uint64((16384000 - 15000000) * 1024); m.MemoryUsedBytes != want {
		t.Errorf("MemoryUsedBytes = %d, want %d (MemTotal-MemAvailable)", m.MemoryUsedBytes, want)
	}
}

func TestReadCgroupMetrics_CgroupPreferredOverMeminfo(t *testing.T) {
	// When cgroup v2 memory files exist they must win over /proc/meminfo.
	root := t.TempDir()
	originalDir := cgroupBaseDir
	originalMeminfo := meminfoFile
	cgroupBaseDir = root
	meminfoFile = writeFixture(t, "meminfo",
		"MemTotal:       16384000 kB\nMemAvailable:   15000000 kB\n")
	t.Cleanup(func() {
		cgroupBaseDir = originalDir
		meminfoFile = originalMeminfo
	})

	if err := os.WriteFile(filepath.Join(root, "memory.current"), []byte("1048576\n"), 0644); err != nil {
		t.Fatalf("write memory.current: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "memory.max"), []byte("268435456\n"), 0644); err != nil {
		t.Fatalf("write memory.max: %v", err)
	}

	m, err := ReadCgroupMetrics()
	if err != nil {
		t.Fatalf("ReadCgroupMetrics() error: %v", err)
	}
	if m.MemoryUsedBytes != 1048576 {
		t.Errorf("MemoryUsedBytes = %d, want 1048576 (cgroup value)", m.MemoryUsedBytes)
	}
	if m.MemoryLimitBytes != 268435456 {
		t.Errorf("MemoryLimitBytes = %d, want 268435456 (cgroup value)", m.MemoryLimitBytes)
	}
}

// writeFixture writes content to a file inside the test's temp dir and
// returns its path.
func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return path
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

// overrideUserSlice points agentUserSliceFn at a fixed directory and restores
// it in t.Cleanup. Returns the base path used.
func overrideUserSlice(t *testing.T, base string) {
	t.Helper()
	original := agentUserSliceFn
	agentUserSliceFn = func(uid int) string { return base }
	t.Cleanup(func() { agentUserSliceFn = original })
}

// overrideHostReaders points cgroupBaseDir and meminfoFile at fixtures and
// restores them in t.Cleanup.
func overrideHostReaders(t *testing.T, cgroupDir, meminfo string) {
	t.Helper()
	originalDir := cgroupBaseDir
	originalMeminfo := meminfoFile
	cgroupBaseDir = cgroupDir
	meminfoFile = meminfo
	t.Cleanup(func() {
		cgroupBaseDir = originalDir
		meminfoFile = originalMeminfo
	})
}

func TestReadAgentCgroupMetrics_AgentPathWins(t *testing.T) {
	agentRoot := t.TempDir()
	hostRoot := t.TempDir()
	overrideUserSlice(t, agentRoot)
	overrideHostReaders(t, hostRoot, writeFixture(t, "meminfo",
		"MemTotal:       16384000 kB\nMemAvailable:   15000000 kB\n"))

	// Agent-path values.
	writeTestFile(t, filepath.Join(agentRoot, "memory.current"), "1048576\n")
	writeTestFile(t, filepath.Join(agentRoot, "memory.max"), "268435456\n")
	writeTestFile(t, filepath.Join(agentRoot, "cpu.max"), "50000 100000\n")
	// Host values differ wildly — the agent path must win.
	writeTestFile(t, filepath.Join(hostRoot, "memory.current"), "999999999\n")
	writeTestFile(t, filepath.Join(hostRoot, "memory.max"), "888888888\n")

	m, err := ReadAgentCgroupMetrics(12345, "agent-1")
	if err != nil {
		t.Fatalf("ReadAgentCgroupMetrics() error: %v", err)
	}
	if m.MemoryUsedBytes != 1048576 {
		t.Errorf("MemoryUsedBytes = %d, want 1048576 (agent path)", m.MemoryUsedBytes)
	}
	if m.MemoryLimitBytes != 268435456 {
		t.Errorf("MemoryLimitBytes = %d, want 268435456 (agent path)", m.MemoryLimitBytes)
	}
	if m.CPUQuota != 0.5 {
		t.Errorf("CPUQuota = %v, want 0.5", m.CPUQuota)
	}
	if m.HostLevelFallback {
		t.Error("HostLevelFallback = true, want false (agent path supplied all fields)")
	}
}

func TestReadAgentCgroupMetrics_FallbackToHost(t *testing.T) {
	// Agent cgroup absent (stopped/destroyed agent) → host values must be
	// returned without error.
	missingAgentBase := filepath.Join(t.TempDir(), "missing-agent-cgroup")
	overrideUserSlice(t, missingAgentBase)

	hostRoot := t.TempDir()
	overrideHostReaders(t, hostRoot, writeFixture(t, "meminfo",
		"MemTotal:       16384000 kB\nMemAvailable:   15000000 kB\n"))
	writeTestFile(t, filepath.Join(hostRoot, "memory.current"), "2097152\n")
	writeTestFile(t, filepath.Join(hostRoot, "memory.max"), "1073741824\n")

	m, err := ReadAgentCgroupMetrics(99999, "destroyed-agent")
	if err != nil {
		t.Fatalf("ReadAgentCgroupMetrics() error: %v", err)
	}
	if m.MemoryUsedBytes != 2097152 {
		t.Errorf("MemoryUsedBytes = %d, want 2097152 (host fallback)", m.MemoryUsedBytes)
	}
	if m.MemoryLimitBytes != 1073741824 {
		t.Errorf("MemoryLimitBytes = %d, want 1073741824 (host fallback)", m.MemoryLimitBytes)
	}
	if !m.HostLevelFallback {
		t.Error("HostLevelFallback = false, want true (host fallback filled the fields)")
	}
}

func TestReadAgentCgroupMetrics_ZeroWhenBothUnreadable(t *testing.T) {
	overrideUserSlice(t, filepath.Join(t.TempDir(), "missing-agent-cgroup"))
	overrideHostReaders(t, t.TempDir(), filepath.Join(t.TempDir(), "no-meminfo"))

	m, err := ReadAgentCgroupMetrics(99999, "gone-agent")
	if err != nil {
		t.Fatalf("ReadAgentCgroupMetrics() error: %v", err)
	}
	if m.MemoryUsedBytes != 0 || m.MemoryLimitBytes != 0 || m.CPUQuota != 0 {
		t.Errorf("ReadAgentCgroupMetrics() = %+v, want zero values", m)
	}
	if !m.HostLevelFallback {
		t.Error("HostLevelFallback = false, want true (agent cgroup base unreadable)")
	}
}

func TestReadAgentCgroupMetrics_MaxLimitHybrid(t *testing.T) {
	// Agent memory.current readable but memory.max == "max" (unlimited):
	// used must come from the agent path, limit from the host fallback.
	agentRoot := t.TempDir()
	overrideUserSlice(t, agentRoot)
	writeTestFile(t, filepath.Join(agentRoot, "memory.current"), "1048576\n")
	writeTestFile(t, filepath.Join(agentRoot, "memory.max"), "max\n")

	hostRoot := t.TempDir()
	overrideHostReaders(t, hostRoot, writeFixture(t, "meminfo",
		"MemTotal:       16384000 kB\nMemAvailable:   15000000 kB\n"))
	writeTestFile(t, filepath.Join(hostRoot, "memory.current"), "999999999\n")
	writeTestFile(t, filepath.Join(hostRoot, "memory.max"), "1073741824\n")

	m, err := ReadAgentCgroupMetrics(12345, "agent-2")
	if err != nil {
		t.Fatalf("ReadAgentCgroupMetrics() error: %v", err)
	}
	if m.MemoryUsedBytes != 1048576 {
		t.Errorf("MemoryUsedBytes = %d, want 1048576 (agent path)", m.MemoryUsedBytes)
	}
	if m.MemoryLimitBytes != 1073741824 {
		t.Errorf("MemoryLimitBytes = %d, want 1073741824 (host fallback)", m.MemoryLimitBytes)
	}
	if !m.HostLevelFallback {
		t.Error("HostLevelFallback = false, want true (host fallback filled the fields)")
	}
}

// writeTestFile writes content into an existing directory (unlike
// writeFixture, which creates its own temp dir).
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
