package resource

import (
	"errors"
	"math"
	"strconv"
	"testing"
	"time"
)

// newTestSampler returns a CPUSampler with injectable stat/clock functions.
func newTestSampler(statFn func() (string, error), nowFn func() time.Time) *CPUSampler {
	return &CPUSampler{readStatFn: statFn, nowFn: nowFn}
}

// cpuStatWithUsage renders a cpu.stat sample with the given usage_usec.
func cpuStatWithUsage(usage uint64) string {
	return "usage_usec " + strconv.FormatUint(usage, 10) + "\nuser_usec 1\nsystem_usec 1\n"
}

func closeTo(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

func TestCPUSampler_FirstCallIsBaseline(t *testing.T) {
	t0 := time.Unix(100, 0)
	var usage uint64 = 1_000_000
	s := newTestSampler(
		func() (string, error) { return cpuStatWithUsage(usage), nil },
		func() time.Time { return t0 },
	)

	if got := s.Percent(); got != 0 {
		t.Errorf("first call (baseline) = %v, want 0", got)
	}
	if got := s.Percent(); got != 0 {
		t.Errorf("second call with zero usage delta = %v, want 0", got)
	}
}

func TestCPUSampler_DeltaMath(t *testing.T) {
	t0 := time.Unix(100, 0)
	var usage uint64 = 1_000_000
	now := t0
	s := newTestSampler(
		func() (string, error) { return cpuStatWithUsage(usage), nil },
		func() time.Time { return now },
	)

	// t0: baseline.
	if got := s.Percent(); got != 0 {
		t.Fatalf("baseline = %v, want 0", got)
	}

	// +2s, usage +500ms of CPU → 25% of one core.
	usage += 500_000
	now = t0.Add(2 * time.Second)
	if got := s.Percent(); !closeTo(got, 25) {
		t.Errorf("delta 500ms/2s = %v, want 25", got)
	}

	// +1s more, usage +250ms → 25% again.
	usage += 250_000
	now = now.Add(time.Second)
	if got := s.Percent(); !closeTo(got, 25) {
		t.Errorf("delta 250ms/1s = %v, want 25", got)
	}
}

func TestCPUSampler_UncappedOnMultiCore(t *testing.T) {
	// 8 cores busy for 1s reads as 800% of one core; the sampler intentionally
	// does not clamp so multi-core load is visible.
	t0 := time.Unix(100, 0)
	var usage uint64 = 1_000_000_000
	now := t0
	s := newTestSampler(
		func() (string, error) { return cpuStatWithUsage(usage), nil },
		func() time.Time { return now },
	)

	if got := s.Percent(); got != 0 {
		t.Fatalf("baseline = %v, want 0", got)
	}
	usage += 8_000_000 // 8s of CPU across cores in 1s wall
	now = t0.Add(time.Second)
	if got := s.Percent(); !closeTo(got, 800) {
		t.Errorf("8 cores busy 1s = %v, want 800", got)
	}
}

func TestCPUSampler_CounterResetRebaselines(t *testing.T) {
	t0 := time.Unix(100, 0)
	var usage uint64 = 1_000_000
	now := t0
	s := newTestSampler(
		func() (string, error) { return cpuStatWithUsage(usage), nil },
		func() time.Time { return now },
	)

	if got := s.Percent(); got != 0 {
		t.Fatalf("baseline = %v, want 0", got)
	}

	// Counter decreased (cgroup recreated): re-baseline, report 0.
	usage = 500_000
	now = t0.Add(time.Second)
	if got := s.Percent(); got != 0 {
		t.Errorf("after counter reset = %v, want 0", got)
	}

	// Now a normal delta from the new baseline.
	usage += 1_000_000
	now = now.Add(time.Second)
	if got := s.Percent(); !closeTo(got, 100) {
		t.Errorf("delta after reset = %v, want 100", got)
	}
}

func TestCPUSampler_ReadErrorKeepsBaseline(t *testing.T) {
	t0 := time.Unix(100, 0)
	var usage uint64 = 1_000_000
	statErr := false
	now := t0
	s := newTestSampler(
		func() (string, error) {
			if statErr {
				return "", errors.New("cpu.stat unreadable")
			}
			return cpuStatWithUsage(usage), nil
		},
		func() time.Time { return now },
	)

	if got := s.Percent(); got != 0 {
		t.Fatalf("baseline = %v, want 0", got)
	}

	// Read failure: 0 and baseline untouched.
	statErr = true
	now = t0.Add(time.Second)
	if got := s.Percent(); got != 0 {
		t.Errorf("on read error = %v, want 0", got)
	}

	// Recovery: delta computed from the ORIGINAL baseline.
	statErr = false
	usage += 500_000
	now = now.Add(time.Second)
	if got := s.Percent(); !closeTo(got, 25) {
		t.Errorf("delta after read failure = %v, want 25", got)
	}
}

func TestCPUSampler_MissingUsageField(t *testing.T) {
	t0 := time.Unix(100, 0)
	var usage uint64 = 1_000_000
	noUsage := false
	now := t0
	s := newTestSampler(
		func() (string, error) {
			if noUsage {
				return "user_usec 5\nsystem_usec 3\n", nil
			}
			return cpuStatWithUsage(usage), nil
		},
		func() time.Time { return now },
	)

	if got := s.Percent(); got != 0 {
		t.Fatalf("baseline = %v, want 0", got)
	}

	noUsage = true
	now = t0.Add(time.Second)
	if got := s.Percent(); got != 0 {
		t.Errorf("without usage_usec = %v, want 0", got)
	}

	noUsage = false
	usage += 500_000
	now = now.Add(time.Second)
	if got := s.Percent(); !closeTo(got, 25) {
		t.Errorf("delta after missing field = %v, want 25", got)
	}
}
