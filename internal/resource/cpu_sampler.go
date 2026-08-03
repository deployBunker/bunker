package resource

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CPUSampler computes host CPU usage percent from cgroup v2 cpu.stat deltas.
//
// cpu.stat's usage_usec is the total CPU time consumed by the cgroup since
// boot — a monotonically increasing counter. A percentage requires comparing
// two samples, so the first Percent() call records a baseline and returns 0.
// The daemon holds one sampler instance for its lifetime; each ServerMetrics
// call advances the baseline.
//
// The returned value is a percentage of ONE core and is intentionally
// uncapped: on multi-core hosts sustained load can legitimately exceed 100%
// (e.g. 8 busy cores ≈ 800%). Callers that want a 0-100 view can clamp.
type CPUSampler struct {
	mu         sync.Mutex
	lastUsage  uint64
	lastSample time.Time
	haveSample bool

	// readStatFn and nowFn are injectable for tests.
	readStatFn func() (string, error)
	nowFn      func() time.Time
}

// NewCPUSampler returns a sampler reading the host root cgroup's cpu.stat.
func NewCPUSampler() *CPUSampler {
	return &CPUSampler{
		readStatFn: readHostCPUStat,
		nowFn:      time.Now,
	}
}

// readHostCPUStat reads the host-level cgroup v2 CPU accounting file.
func readHostCPUStat() (string, error) {
	data, err := os.ReadFile(filepath.Join(cgroupBaseDir, "cpu.stat"))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Percent returns host CPU usage as a percentage of one core since the
// previous call. The first call returns 0 (baseline only). If cpu.stat
// cannot be read or usage_usec is absent, it returns 0 and leaves the
// baseline untouched so a later successful sample still computes a valid
// delta.
func (s *CPUSampler) Percent() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	stat, err := s.readStatFn()
	if err != nil {
		return 0
	}
	usage, ok := parseUsageUsec(stat)
	if !ok {
		return 0
	}

	now := s.nowFn()
	if !s.haveSample {
		s.lastUsage = usage
		s.lastSample = now
		s.haveSample = true
		return 0
	}

	// usage_usec is monotonic; a decrease means the counter was reset
	// (e.g. cgroup recreated). Re-baseline instead of computing a garbage
	// negative delta.
	if usage < s.lastUsage {
		s.lastUsage = usage
		s.lastSample = now
		return 0
	}

	usageDelta := usage - s.lastUsage
	wallDelta := now.Sub(s.lastSample)
	s.lastUsage = usage
	s.lastSample = now

	wallUsec := wallDelta.Microseconds()
	if wallUsec <= 0 {
		return 0
	}
	return float64(usageDelta) / float64(wallUsec) * 100
}
