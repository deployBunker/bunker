package resource

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// agentCgroupBaseFn is the function used to compute the per-agent cgroup base
// path. It is a variable so tests can override it without writing to the real
// /sys/fs/cgroup.
var agentCgroupBaseFn = func(uid int, agentID string) string {
	return fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/bunker-docker-%s.service", uid, uid, agentID)
}

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

// agentCgroupBase returns the systemd user unit cgroup path for a rootless
// Docker agent. The unit is created by `systemd-run --system --unit=` and runs
// as the unprivileged agent user, so its cgroup lives under the user's session
// slice, not a system-level bunker slice.
//
// Layout: /sys/fs/cgroup/user.slice/user-<uid>.slice/user@<uid>.service/bunker-docker-<agentID>.service
//
// The returned path is used only for read-back verification; the actual limits
// are enforced by systemd via the unit properties CPUQuota, MemoryMax, TasksMax,
// and LimitNOFILE set at spawn time.
func agentCgroupBase(uid int, agentID string) string {
	return agentCgroupBaseFn(uid, agentID)
}

// CgroupCPUSharesPath returns the cgroup path for an agent's CPU shares.
func CgroupCPUSharesPath(agentID string) string {
	return fmt.Sprintf("/sys/fs/cgroup/bunker-%s.slice", agentID)
}

// CgroupMemoryPath returns the cgroup path for an agent's memory limit.
func CgroupMemoryPath(agentID string) string {
	return fmt.Sprintf("/sys/fs/cgroup/bunker-%s.slice", agentID)
}

// CgroupCPUPath returns the cgroup v2 cpu.max path for a rootless agent unit.
func CgroupCPUPath(uid int, agentID string) string {
	return filepath.Join(agentCgroupBase(uid, agentID), "cpu.max")
}

// CgroupMemoryLimitPath returns the cgroup v2 memory.max path for a rootless agent unit.
func CgroupMemoryLimitPath(uid int, agentID string) string {
	return filepath.Join(agentCgroupBase(uid, agentID), "memory.max")
}

// ReadAgentCgroupLimits reads the cgroup v2 controller files for the agent's
// systemd user unit and returns the parsed limits. This is a best-effort read;
// the authoritative limits are the systemd unit properties. If the cgroup files
// cannot be read, a zero-valued CgroupMetrics is returned without error.
func ReadAgentCgroupLimits(uid int, agentID string) (*CgroupMetrics, error) {
	m := &CgroupMetrics{}

	cpuRaw, err := os.ReadFile(CgroupCPUPath(uid, agentID))
	if err == nil {
		fields := strings.Fields(strings.TrimSpace(string(cpuRaw)))
		if len(fields) == 2 && fields[0] != "max" {
			quota, err := strconv.ParseUint(fields[0], 10, 64)
			if err == nil {
				period, err := strconv.ParseUint(fields[1], 10, 64)
				if err == nil && period > 0 {
					m.CPUQuota = float64(quota) / float64(period)
				}
			}
		}
	}

	memRaw, err := os.ReadFile(CgroupMemoryLimitPath(uid, agentID))
	if err == nil {
		memStr := strings.TrimSpace(string(memRaw))
		if memStr != "max" && memStr != "" {
			val, err := strconv.ParseUint(memStr, 10, 64)
			if err == nil {
				m.MemoryLimitBytes = val
			}
		}
	}

	return m, nil
}
