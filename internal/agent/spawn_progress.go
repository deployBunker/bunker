package agent

import (
	"sort"
	"sync/atomic"
	"time"
)

// DOGFOOD-015: in-flight spawn stage marks.
//
// A spawn is synchronous and, on a cold host, minutes long (rootless docker
// install, a --tools-at-spawn image build). Until this file existed the daemon
// held no per-spawn state a client could READ while it waited: the tracker
// record only lands at the register stage, so `ListAgents` showed nothing, and
// a client whose RPC ended without a response body learned nothing at all.
//
// Each stage mark records the stage the spawn is EXECUTING (one of the Stage*
// constants in spawn_failure.go — the same vocabulary spawnStageErr uses for
// failures). The map is bounded by the number of concurrent spawns and is
// cleared on every exit path through Spawn's deferred clear, so a crashed or
// completed spawn can never leave a permanent "still rootless-install" entry.

// SpawnProgress is the read view of one in-flight spawn.
type SpawnProgress struct {
	// AgentID is the id the spawn resolved before entering its first stage.
	AgentID string
	// Stage is the stage constant currently executing. Never a guess: the
	// manager only stores stages it actually entered, and an unmarked spawn
	// (before its first mark) is absent from the report rather than reported
	// with an empty or invented stage.
	Stage string
	// Elapsed is measured from the FIRST stage mark (not from when the
	// daemon accepted the RPC), so it under-reports rather than over-reports.
	Elapsed time.Duration
}

// spawnProgressEntry is the stored form: a POINTER is stored on every mark, so
// a concurrent reader either sees a complete earlier entry or a complete later
// one — never a torn field set.
type spawnProgressEntry struct {
	agentID string
	stage   string
	started time.Time
}

// spawnProgressCount is a cheap gauge of in-flight spawns for operators and
// tests (the map itself is only rangeable).
var spawnProgressCount atomic.Int64

// enterSpawnStage records that agentID's spawn has entered stage, and logs the
// transition (the daemon's existing "spawn entering stage" journal line is the
// same event, so operators and the RPC surface can never disagree about which
// stage an agent was in). The first mark anchors the elapsed clock.
func (m *AgentManager) enterSpawnStage(agentID, stage string) {
	started := time.Now()
	if prev, ok := m.spawnProgress.Load(agentID); ok {
		if p, ok := prev.(*spawnProgressEntry); ok && !p.started.IsZero() {
			started = p.started
		}
	} else {
		spawnProgressCount.Add(1)
	}
	m.logger.Info("spawn entering stage", "agent_id", agentID, "stage", stage)
	m.spawnProgress.Store(agentID, &spawnProgressEntry{agentID: agentID, stage: stage, started: started})
}

// clearSpawnProgress drops the in-flight mark for agentID. Idempotent: clearing
// an id that was never marked (a spawn that failed during validation) is a
// no-op.
func (m *AgentManager) clearSpawnProgress(agentID string) {
	if agentID == "" {
		return
	}
	if _, wasMarked := m.spawnProgress.Load(agentID); wasMarked {
		spawnProgressCount.Add(-1)
	}
	m.spawnProgress.Delete(agentID)
}

// SpawnProgress reports the stages of the spawns currently in flight: only the
// one with this agent id when agentID is non-empty, every in-flight spawn
// otherwise. Sorted by agent id so the report is stable for a polling client
// and for tests.
func (m *AgentManager) SpawnProgress(agentID string) []SpawnProgress {
	now := time.Now()
	out := make([]SpawnProgress, 0, 1)
	m.spawnProgress.Range(func(_, value any) bool {
		e, ok := value.(*spawnProgressEntry)
		if !ok {
			return true
		}
		if agentID != "" && e.agentID != agentID {
			return true
		}
		out = append(out, SpawnProgress{
			AgentID: e.agentID,
			Stage:   e.stage,
			Elapsed: now.Sub(e.started),
		})
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}
