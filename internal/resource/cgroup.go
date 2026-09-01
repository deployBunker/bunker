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

// agentUserSliceFn is the function used to compute the agent user slice path.
// All of an agent's processes live under this slice: the systemd-run rootless
// dockerd unit AND every SSH session scope for the agent user (each
// `bunker exec` runs in user.slice/user-<uid>.slice/session-*.scope). The
// slice's memory.max is the agent's --memory limit, so reading the slice is
// the correct per-agent view. It is a variable so tests can override it.
var agentUserSliceFn = func(uid int) string {
	return fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice", uid)
}

// cgroupBaseDir and meminfoFile are variables (not constants) so tests can
// point them at fixtures without touching the real host files.
var (
	cgroupBaseDir = "/sys/fs/cgroup"
	meminfoFile   = "/proc/meminfo"
)

// CgroupMetrics holds resource usage for an agent.
type CgroupMetrics struct {
	CPUUsagePercent  float64
	MemoryUsedBytes  uint64
	MemoryLimitBytes uint64
	CPUQuota         float64
	// HostLevelFallback is true when any memory field could not be read from
	// the agent's own cgroup and was filled (or left zero) from the host-level
	// read. Callers should surface this so host values are not mistaken for
	// agent values.
	HostLevelFallback bool
}

// ReadCgroupMetrics reads CPU and memory usage from cgroup v2.
// For systemd user units, the path is /sys/fs/cgroup/user.slice/user-<uid>.slice/user@<uid>.service/...
// We use a simplified approach: read host-level cgroup stats and scale by agent count.
//
// Memory: cgroup v2 files are primary; when they are unreadable (some hosts
// do not expose memory.current/memory.max), fall back to /proc/meminfo
// (MemTotal as the limit, MemTotal-MemAvailable as used).
//
// CPU: usage_usec is a monotonic counter, so a point-in-time snapshot cannot
// express a percentage. CPUUsagePercent is left at 0 here; callers that want
// a real percent must hold a CPUSampler and compute deltas across calls.
func ReadCgroupMetrics() (*CgroupMetrics, error) {
	m := &CgroupMetrics{}

	memUsedOK := false
	memLimitOK := false

	// Read memory.current for used memory
	memCurrent, err := os.ReadFile(filepath.Join(cgroupBaseDir, "memory.current"))
	if err == nil {
		val, parseErr := strconv.ParseUint(strings.TrimSpace(string(memCurrent)), 10, 64)
		if parseErr == nil {
			m.MemoryUsedBytes = val
			memUsedOK = true
		}
	}

	// Read memory.max for memory limit
	memMax, err := os.ReadFile(filepath.Join(cgroupBaseDir, "memory.max"))
	if err == nil {
		maxStr := strings.TrimSpace(string(memMax))
		if maxStr != "max" {
			val, parseErr := strconv.ParseUint(maxStr, 10, 64)
			if parseErr == nil {
				m.MemoryLimitBytes = val
				memLimitOK = true
			}
		}
	}

	// Fallback: /proc/meminfo when either cgroup memory file is unavailable.
	if !memUsedOK || !memLimitOK {
		if total, available, ok := readMeminfo(); ok {
			if !memUsedOK {
				m.MemoryUsedBytes = total - available
			}
			if !memLimitOK {
				m.MemoryLimitBytes = total
			}
		}
	}

	return m, nil
}

// ReadAgentCgroupMetrics reads memory and CPU usage from the agent's own
// cgroup, not the host root cgroup. The agent's processes (the systemd-run
// rootless dockerd unit and every SSH session scope for `bunker exec`) all
// live under the agent user's slice:
//
//	/sys/fs/cgroup/user.slice/user-<uid>.slice
//
// The slice's memory.max is the agent's --memory limit, so this is the correct
// per-agent view. When the agent cgroup is absent (stopped or destroyed agent,
// deleted user) or any controller file is unreadable, the missing fields
// degrade to the host-level read from ReadCgroupMetrics (which itself falls
// back to /proc/meminfo). This function never returns a non-nil error, so
// callers can always render a coherent metrics response.
func ReadAgentCgroupMetrics(uid int, agentID string) (*CgroupMetrics, error) {
	base := agentUserSliceFn(uid)
	m := &CgroupMetrics{}

	usedOK := false
	limitOK := false

	// memory.current -> MemoryUsedBytes (the agent's own usage).
	if raw, err := os.ReadFile(filepath.Join(base, "memory.current")); err == nil {
		if val, parseErr := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); parseErr == nil {
			m.MemoryUsedBytes = val
			usedOK = true
		}
	}

	// memory.max -> MemoryLimitBytes; "max" means unlimited, leave for fallback.
	if raw, err := os.ReadFile(filepath.Join(base, "memory.max")); err == nil {
		maxStr := strings.TrimSpace(string(raw))
		if maxStr != "max" && maxStr != "" {
			if val, parseErr := strconv.ParseUint(maxStr, 10, 64); parseErr == nil {
				m.MemoryLimitBytes = val
				limitOK = true
			}
		}
	}

	// cpu.max -> CPUQuota (quota/period), same parse as ReadAgentCgroupLimits.
	if raw, err := os.ReadFile(filepath.Join(base, "cpu.max")); err == nil {
		fields := strings.Fields(strings.TrimSpace(string(raw)))
		if len(fields) == 2 && fields[0] != "max" {
			if quota, parseErr := strconv.ParseUint(fields[0], 10, 64); parseErr == nil {
				if period, parseErr := strconv.ParseUint(fields[1], 10, 64); parseErr == nil && period > 0 {
					m.CPUQuota = float64(quota) / float64(period)
				}
			}
		}
	}

	// MANDATORY fallback: when the agent cgroup is absent or any memory field
	// is missing, degrade to the host-level read — never hard-error, never
	// panic. HostLevelFallback is set so callers can warn that the values are
	// HOST values (or zero when the host read also fails), not the agent's
	// own cgroup values. When both are unreadable the result is a zero-valued
	// metrics struct, matching the ReadCgroupMetrics contract.
	if !usedOK || !limitOK {
		m.HostLevelFallback = true
		if host, err := ReadCgroupMetrics(); err == nil {
			if !usedOK {
				m.MemoryUsedBytes = host.MemoryUsedBytes
			}
			if !limitOK {
				m.MemoryLimitBytes = host.MemoryLimitBytes
			}
			if m.CPUQuota == 0 {
				m.CPUQuota = host.CPUQuota
			}
		}
	}

	return m, nil
}

// parseUsageUsec extracts the usage_usec counter from a cpu.stat string.
// It returns ok=false when the field is absent or unparsable.
func parseUsageUsec(cpuStat string) (uint64, bool) {
	for _, line := range strings.Split(strings.TrimSpace(cpuStat), "\n") {
		if strings.HasPrefix(line, "usage_usec ") {
			val, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "usage_usec ")), 10, 64)
			if err == nil {
				return val, true
			}
		}
	}
	return 0, false
}

// parseMeminfo parses MemTotal and MemAvailable from a /proc/meminfo sample.
// Values are reported in kB by /proc/meminfo and converted to bytes.
// A missing or malformed field parses as 0.
func parseMeminfo(meminfo string) (totalBytes uint64, availableBytes uint64) {
	for _, line := range strings.Split(meminfo, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(name) {
		case "MemTotal":
			totalBytes = kb * 1024
		case "MemAvailable":
			availableBytes = kb * 1024
		}
	}
	return totalBytes, availableBytes
}

// readMeminfo reads and parses /proc/meminfo. ok=false on read failure.
func readMeminfo() (totalBytes, availableBytes uint64, ok bool) {
	data, err := os.ReadFile(meminfoFile)
	if err != nil {
		return 0, 0, false
	}
	total, available := parseMeminfo(string(data))
	if total == 0 {
		return 0, 0, false
	}
	return total, available, true
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
