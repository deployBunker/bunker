package resource

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// CgroupMetrics holds resource usage for an agent.
type CgroupMetrics struct {
	CPUUsagePercent  float64
	MemoryUsedBytes  uint64
	MemoryLimitBytes uint64
	CPUQuota         float64
}

// ReadCgroupMetrics reads CPU and memory usage from cgroup v2.
// For systemd user units, the path is /sys/fs/cgroup/user.slice/user-<uid>.slice/user@<uid>.service/...
// We use a simplified approach: read host-level cgroup stats and scale by agent count.
func ReadCgroupMetrics() (*CgroupMetrics, error) {
	m := &CgroupMetrics{}

	// Read memory.current for used memory
	memCurrent, err := os.ReadFile("/sys/fs/cgroup/memory.current")
	if err == nil {
		val, parseErr := strconv.ParseUint(strings.TrimSpace(string(memCurrent)), 10, 64)
		if parseErr == nil {
			m.MemoryUsedBytes = val
		}
	}

	// Read memory.max for memory limit
	memMax, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err == nil {
		maxStr := strings.TrimSpace(string(memMax))
		if maxStr != "max" {
			val, parseErr := strconv.ParseUint(maxStr, 10, 64)
			if parseErr == nil {
				m.MemoryLimitBytes = val
			}
		}
	}

	// Read cpu.stat for usage_usec total
	cpuStat, err := os.ReadFile("/sys/fs/cgroup/cpu.stat")
	if err == nil {
		m.CPUUsagePercent = parseCPUPercent(string(cpuStat))
	}

	return m, nil
}

// parseCPUPercent extracts usage from cpu.stat and returns a rough percentage.
func parseCPUPercent(cpuStat string) float64 {
	var usageUsec uint64
	for _, line := range strings.Split(strings.TrimSpace(cpuStat), "\n") {
		if strings.HasPrefix(line, "usage_usec ") {
			val, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "usage_usec ")), 10, 64)
			if err == nil {
				usageUsec = val
			}
		}
	}
	// usage_usec is total CPU time in microseconds since boot.
	// This is a point-in-time reading; caller should track deltas for actual percent.
	// For now, return 0 and document that proper percent requires delta tracking.
	_ = usageUsec
	return 0
}

// CgroupCPUSharesPath returns the cgroup path for an agent's CPU shares.
func CgroupCPUSharesPath(agentID string) string {
	return fmt.Sprintf("/sys/fs/cgroup/bunker-%s.slice", agentID)
}

// CgroupMemoryPath returns the cgroup path for an agent's memory limit.
func CgroupMemoryPath(agentID string) string {
	return fmt.Sprintf("/sys/fs/cgroup/bunker-%s.slice", agentID)
}
