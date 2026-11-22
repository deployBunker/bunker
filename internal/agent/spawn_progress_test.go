package agent

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func newProgressTestManager() *AgentManager {
	return &AgentManager{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// TestSpawnProgress_RecordsStageAndAnchorsElapsed: the report names the stage
// the spawn ACTUALLY entered, and the elapsed clock is anchored at the FIRST
// mark (a spawn that has been in three stages has been running since the
// first, not since the third).
func TestSpawnProgress_RecordsStageAndAnchorsElapsed(t *testing.T) {
	m := newProgressTestManager()
	defer m.clearSpawnProgress("probe-a")

	m.enterSpawnStage("probe-a", StageValidate)
	got := m.SpawnProgress("probe-a")
	if len(got) != 1 {
		t.Fatalf("SpawnProgress = %d entries, want 1", len(got))
	}
	if got[0].Stage != StageValidate {
		t.Errorf("stage = %q, want %q", got[0].Stage, StageValidate)
	}
	if got[0].AgentID != "probe-a" {
		t.Errorf("agent id = %q, want probe-a", got[0].AgentID)
	}

	time.Sleep(25 * time.Millisecond)
	m.enterSpawnStage("probe-a", StageRootlessInstall)
	got = m.SpawnProgress("probe-a")
	if len(got) != 1 {
		t.Fatalf("SpawnProgress after second mark = %d entries, want 1 (one entry per spawn)", len(got))
	}
	if got[0].Stage != StageRootlessInstall {
		t.Errorf("stage = %q, want the newest mark %q", got[0].Stage, StageRootlessInstall)
	}
	if got[0].Elapsed < 20*time.Millisecond {
		t.Errorf("elapsed = %s, want the clock anchored at the first mark (>=20ms)", got[0].Elapsed)
	}
}

// TestSpawnProgress_FilterSortAndClear covers the read surface a polling client
// relies on: per-id filtering, stable ordering, and no stale entry after the
// spawn ends (a leaked mark would read as a spawn still in flight, forever).
func TestSpawnProgress_FilterSortAndClear(t *testing.T) {
	m := newProgressTestManager()
	m.enterSpawnStage("probe-b", StageKeygen)
	m.enterSpawnStage("probe-a", StageUserCreate)

	all := m.SpawnProgress("")
	if len(all) != 2 {
		t.Fatalf("SpawnProgress(\"\") = %d entries, want 2", len(all))
	}
	if all[0].AgentID != "probe-a" || all[1].AgentID != "probe-b" {
		t.Errorf("entries not sorted by agent id: %+v", all)
	}
	if one := m.SpawnProgress("probe-b"); len(one) != 1 || one[0].Stage != StageKeygen {
		t.Errorf("filtered report = %+v, want only probe-b at keygen", one)
	}
	if none := m.SpawnProgress("probe-c"); len(none) != 0 {
		t.Errorf("unknown id reported %+v, want an empty report (never a fabricated stage)", none)
	}

	m.clearSpawnProgress("probe-a")
	m.clearSpawnProgress("probe-b")
	if left := m.SpawnProgress(""); len(left) != 0 {
		t.Errorf("after clear, report = %+v, want empty", left)
	}
	// Idempotent: the deferred clear runs on paths where nothing was marked.
	m.clearSpawnProgress("probe-a")
	m.clearSpawnProgress("")
	if got := spawnProgressCount.Load(); got != 0 {
		t.Errorf("in-flight gauge = %d after clearing every spawn, want 0", got)
	}
}

// TestEnterSpawnStage_LogsMarkWithElapsed (PERF-022): every stage mark's log
// line carries the stage AND an elapsed value (elapsed_ms since the first
// mark, stage_gap_ms since the previous mark), so the inter-stage gaps of a
// live spawn are attributable from the daemon log alone — the acceptance run
// can grep one mark line per stage and read the phase table off the log.
func TestEnterSpawnStage_LogsMarkWithElapsed(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	m := &AgentManager{logger: logger}
	defer m.clearSpawnProgress("perf022-a")

	m.enterSpawnStage("perf022-a", StageValidate)
	time.Sleep(40 * time.Millisecond)
	m.enterSpawnStage("perf022-a", StageKeygen)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("daemon log = %d mark lines, want 2:\n%s", len(lines), buf.String())
	}
	for _, tc := range []struct{ line, stage string }{{lines[0], StageValidate}, {lines[1], StageKeygen}} {
		if !strings.Contains(tc.line, "spawn entering stage") {
			t.Errorf("mark line does not carry the enter-stage event: %s", tc.line)
		}
		if !strings.Contains(tc.line, "stage="+tc.stage) {
			t.Errorf("mark line missing its stage %s: %s", tc.stage, tc.line)
		}
		if !strings.Contains(tc.line, "elapsed_ms=") {
			t.Errorf("mark line missing the elapsed value: %s", tc.line)
		}
	}
	// The second mark's gap names how long the previous stage kept the spawn:
	// the inter-stage duration is now readable straight off the log.
	gapLine := lines[1]
	end := strings.Index(gapLine, "stage_gap_ms=")
	if end < 0 {
		t.Fatalf("second mark has no stage_gap_ms, got: %s", gapLine)
	}
	gapVal := gapLine[end+len("stage_gap_ms="):]
	if i := strings.Index(gapVal, " "); i >= 0 {
		gapVal = gapVal[:i]
	}
	if gapVal == "0" {
		t.Errorf("second mark's stage_gap_ms = 0, want the previous stage's duration (>=40ms waited), line: %s", gapLine)
	}
}
