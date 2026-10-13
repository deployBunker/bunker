package resource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── GAP-121: parse tests ────────────────────────────────────────────────────

func TestParseMemoryEvents_Full(t *testing.T) {
	// The real file's shape (kernel 6.x): one "key value" per line, counters
	// only — no header, no units.
	sample := "low 0\nhigh 4\nmax 3\noom 1\noom_kill 1\noom_group_kill 0\n"
	ev, ok := parseMemoryEvents(sample)
	if !ok {
		t.Fatal("parseMemoryEvents returned ok=false for a well-formed file")
	}
	if !ev.Available {
		t.Error("Available = false for a readable file")
	}
	if ev.Max != 3 || ev.High != 4 || ev.Low != 0 || ev.OOM != 1 || ev.OOMKill != 1 || ev.OOMGroupKill != 0 {
		t.Errorf("parsed counters = %+v, want max=3 high=4 low=0 oom=1 oom_kill=1 oom_group_kill=0", ev)
	}
}

func TestParseMemoryEvents_PartialKeysStillAvailable(t *testing.T) {
	// A kernel that exposes only a subset of the counters is still a valid
	// reading: the keys it does expose are real (GAP-121 rule — a missing
	// counter must not turn the whole file into "unavailable").
	ev, ok := parseMemoryEvents("max 7\n")
	if !ok {
		t.Fatal("parseMemoryEvents rejected a file with one valid key")
	}
	if ev.Max != 7 {
		t.Errorf("Max = %d, want 7", ev.Max)
	}
	if !ev.Available {
		t.Error("Available = false for a partially-populated but parseable file")
	}
}

func TestParseMemoryEvents_UnparsableIsNotACleanReading(t *testing.T) {
	for _, in := range []string{"", "\n\n", "total 5\nanon 3\n", "max notanumber\n"} {
		ev, ok := parseMemoryEvents(in)
		if ok {
			t.Errorf("parseMemoryEvents(%q) = ok, want false (nothing measured is not a clean reading)", in)
		}
		if ev.Available {
			t.Errorf("parseMemoryEvents(%q) set Available=true", in)
		}
	}
}

func TestParsePidsEvents(t *testing.T) {
	ev, ok := parsePidsEvents("max 12\n")
	if !ok || !ev.Available {
		t.Fatalf("parsePidsEvents(valid) = (%+v, %v), want available", ev, ok)
	}
	if ev.Max != 12 {
		t.Errorf("Max = %d, want 12", ev.Max)
	}
	if _, ok := parsePidsEvents(""); ok {
		t.Error("parsePidsEvents(\"\") = ok, want false")
	}
	if _, ok := parsePidsEvents("events 1\n"); ok {
		t.Error("parsePidsEvents with no max key = ok, want false")
	}
}

// ── GAP-121: read tests (fixture cgroup, no host access) ────────────────────

// writeFixtureFile writes one cgroup file into dir, failing the test on error.
func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
}

// newCgroupFixture points agentUserSliceFn at a temp dir and returns it.
func newCgroupFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	original := agentUserSliceFn
	agentUserSliceFn = func(uid int) string { return dir }
	t.Cleanup(func() { agentUserSliceFn = original })
	return dir
}

func TestReadAgentResourceEvents_AllFilesPresent(t *testing.T) {
	dir := newCgroupFixture(t)
	writeFixtureFile(t, dir, "memory.events", "low 1\nhigh 2\nmax 3\noom 4\noom_kill 5\noom_group_kill 6\n")
	writeFixtureFile(t, dir, "pids.events", "max 7\n")
	writeFixtureFile(t, dir, "memory.current", "1048576\n")
	writeFixtureFile(t, dir, "pids.current", "42\n")
	writeFixtureFile(t, dir, "memory.peak", "2097152\n")
	writeFixtureFile(t, dir, "pids.peak", "99\n")

	ev := ReadAgentResourceEvents(1001, "agent-x")
	if !ev.Available {
		t.Fatalf("Available = false, detail = %q", ev.Detail)
	}
	if ev.Detail != "" {
		t.Errorf("Detail = %q, want empty when every read succeeded", ev.Detail)
	}
	if ev.CgroupPath != dir {
		t.Errorf("CgroupPath = %q, want %q", ev.CgroupPath, dir)
	}
	if ev.MemoryEvents.Max != 3 || ev.MemoryEvents.OOMKill != 5 || ev.MemoryEvents.OOMGroupKill != 6 {
		t.Errorf("memory events = %+v, want max=3 oom_kill=5 oom_group_kill=6", ev.MemoryEvents)
	}
	if ev.PidsEvents.Max != 7 {
		t.Errorf("pids max = %d, want 7", ev.PidsEvents.Max)
	}
	if ev.MemoryCurrentBytes != 1048576 || ev.PidsCurrent != 42 {
		t.Errorf("current = (%d, %d), want (1048576, 42)", ev.MemoryCurrentBytes, ev.PidsCurrent)
	}
	if ev.MemoryPeakBytes != 2097152 || ev.MemoryPeakSource != PeakSourceCgroup {
		t.Errorf("memory peak = (%d, %q), want (2097152, %q)", ev.MemoryPeakBytes, ev.MemoryPeakSource, PeakSourceCgroup)
	}
	if ev.PidsPeak != 99 || ev.PidsPeakSource != PeakSourceCgroup {
		t.Errorf("pids peak = (%d, %q), want (99, %q)", ev.PidsPeak, ev.PidsPeakSource, PeakSourceCgroup)
	}
	if !ev.HaveAnyCeilingHit() {
		t.Error("HaveAnyCeilingHit = false for a cgroup with max=3 and oom_kill=5")
	}
}

func TestReadAgentResourceEvents_MissingOptionalPeakFilesIsNotAFailure(t *testing.T) {
	// Kernels < 5.19 / < 6.1 have no memory.peak / pids.peak. Their absence is
	// "unavailable peak", NOT a read failure.
	dir := newCgroupFixture(t)
	writeFixtureFile(t, dir, "memory.events", "max 0\n")
	writeFixtureFile(t, dir, "pids.events", "max 0\n")
	writeFixtureFile(t, dir, "memory.current", "4096\n")
	writeFixtureFile(t, dir, "pids.current", "3\n")

	ev := ReadAgentResourceEvents(1001, "agent-x")
	if !ev.Available {
		t.Fatalf("Available = false on a kernel without peak files (detail %q)", ev.Detail)
	}
	if ev.Detail != "" {
		t.Errorf("Detail = %q, want empty — a missing optional peak file is not a failure", ev.Detail)
	}
	if ev.MemoryPeakSource != "" || ev.PidsPeakSource != "" {
		t.Errorf("peak sources = (%q, %q), want empty when no peak exists", ev.MemoryPeakSource, ev.PidsPeakSource)
	}
	if ev.HaveAnyCeilingHit() {
		t.Error("HaveAnyCeilingHit = true for an all-zero event file")
	}
}

func TestReadAgentResourceEvents_AbsentCgroupIsUnavailableNotClean(t *testing.T) {
	// A stopped/destroyed agent (or deleted user) has no slice at all: the
	// result must say "nothing measured", and must NOT look like zero events.
	dir := newCgroupFixture(t) // empty dir: the cgroup has no files at all
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}

	ev := ReadAgentResourceEvents(1001, "agent-gone")
	if ev.Available {
		t.Fatal("Available = true for an empty cgroup directory")
	}
	if ev.Detail == "" {
		t.Error("Detail is empty while Available is false — the caller cannot tell WHY")
	}
	if !strings.Contains(ev.Detail, "memory.events") || !strings.Contains(ev.Detail, "pids.events") {
		t.Errorf("Detail = %q, want it to name both unreadable event files", ev.Detail)
	}
	if ev.HaveAnyCeilingHit() {
		t.Error("HaveAnyCeilingHit = true on an unmeasured agent")
	}
}

func TestReadAgentResourceEvents_MalformedEventFileIsReported(t *testing.T) {
	dir := newCgroupFixture(t)
	writeFixtureFile(t, dir, "memory.events", "garbage without counters\n")
	writeFixtureFile(t, dir, "pids.events", "max 2\n")

	ev := ReadAgentResourceEvents(1001, "agent-x")
	if ev.MemoryEvents.Available {
		t.Error("memory events Available = true for an unparsable file")
	}
	if !ev.PidsEvents.Available || ev.PidsEvents.Max != 2 {
		t.Errorf("pids events = %+v, want available with max=2", ev.PidsEvents)
	}
	// The cgroup WAS located and one file was read: the top-level flag says so,
	// while the unmeasured block says so separately. The two are different
	// statements and must not be collapsed.
	if !ev.Available {
		t.Error("top-level Available = false although the cgroup was located and pids.events was read")
	}
	if !strings.Contains(ev.Detail, "memory.events") {
		t.Errorf("Detail = %q, want it to name memory.events", ev.Detail)
	}
}

// TestReadAgentResourceEvents_PartialReadIsNotACleanReading pins the semantics
// an operator relies on: one readable counter set does not vouch for the other.
func TestReadAgentResourceEvents_PartialReadIsNotACleanReading(t *testing.T) {
	dir := newCgroupFixture(t)
	writeFixtureFile(t, dir, "memory.events", "max 4\n")
	// pids.events deliberately absent (e.g. the pids controller is not enabled
	// for this slice): a real, measured scenario that must not be silently
	// reported as "pids.max = 0".
	ev := ReadAgentResourceEvents(1001, "agent-x")

	if !ev.MemoryEvents.Available || ev.MemoryEvents.Max != 4 {
		t.Errorf("memory events = %+v, want available with max=4", ev.MemoryEvents)
	}
	if ev.PidsEvents.Available {
		t.Error("pids events reported available although the file is absent")
	}
	if !ev.Available {
		t.Error("top-level Available = false although memory.events was read")
	}
	if !strings.Contains(ev.Detail, "pids.events") {
		t.Errorf("Detail = %q, want it to name pids.events", ev.Detail)
	}
	if !ev.HaveAnyCeilingHit() {
		t.Error("HaveAnyCeilingHit = false for a measured memory.max = 4")
	}
}

// ── GAP-121: peak merge semantics ──────────────────────────────────────────

func TestMergeDaemonPeaks_CgroupWins(t *testing.T) {
	// The kernel's peak is the agent's true high-water mark; a daemon sample
	// that is LOWER must not overwrite it, and a higher one must not either —
	// the kernel already saw everything.
	ev := &AgentResourceEvents{
		MemoryPeakBytes:  900,
		MemoryPeakSource: PeakSourceCgroup,
		PidsPeak:         50,
		PidsPeakSource:   PeakSourceCgroup,
	}
	ev.MergeDaemonPeaks(100, 5, true)
	if ev.MemoryPeakBytes != 900 || ev.MemoryPeakSource != PeakSourceCgroup {
		t.Errorf("memory peak = (%d, %q), want the cgroup value preserved", ev.MemoryPeakBytes, ev.MemoryPeakSource)
	}
	if ev.PidsPeak != 50 || ev.PidsPeakSource != PeakSourceCgroup {
		t.Errorf("pids peak = (%d, %q), want the cgroup value preserved", ev.PidsPeak, ev.PidsPeakSource)
	}
}

func TestMergeDaemonPeaks_DaemonUsedWhenKernelHasNoPeak(t *testing.T) {
	ev := &AgentResourceEvents{}
	ev.MergeDaemonPeaks(4096, 12, true)
	if ev.MemoryPeakBytes != 4096 || ev.MemoryPeakSource != PeakSourceDaemon {
		t.Errorf("memory peak = (%d, %q), want (%d, %q)", ev.MemoryPeakBytes, ev.MemoryPeakSource, 4096, PeakSourceDaemon)
	}
	if ev.PidsPeak != 12 || ev.PidsPeakSource != PeakSourceDaemon {
		t.Errorf("pids peak = (%d, %q), want (12, %q)", ev.PidsPeak, ev.PidsPeakSource, PeakSourceDaemon)
	}
}

func TestMergeDaemonPeaks_UnseenAgentGetsNoSource(t *testing.T) {
	// ok=false means the daemon never sampled this agent: the source must
	// stay empty rather than claiming a measured zero.
	ev := &AgentResourceEvents{}
	ev.MergeDaemonPeaks(0, 0, false)
	if ev.MemoryPeakSource != "" || ev.PidsPeakSource != "" {
		t.Errorf("sources = (%q, %q), want empty for an unsampled agent", ev.MemoryPeakSource, ev.PidsPeakSource)
	}
}

func TestMergeDaemonPeaks_NilReceiverIsSafe(t *testing.T) {
	var ev *AgentResourceEvents
	ev.MergeDaemonPeaks(1, 1, true) // must not panic
}

func TestHaveAnyCeilingHit_NilIsFalse(t *testing.T) {
	var ev *AgentResourceEvents
	if ev.HaveAnyCeilingHit() {
		t.Error("nil receiver reported a ceiling hit")
	}
}

func TestHaveAnyCeilingHit_PidsOnly(t *testing.T) {
	ev := &AgentResourceEvents{PidsEvents: PidsEvents{Max: 1, Available: true}}
	if !ev.HaveAnyCeilingHit() {
		t.Error("pids.max = 1 did not register as a ceiling hit")
	}
}
