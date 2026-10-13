package server

import (
	"context"
	"log/slog"
	"os/user"
	"strconv"
	"time"

	"github.com/deployBunker/bunker/internal/resource"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── GAP-121: per-agent resource-event observability ─────────────────────────
//
// Two halves live here:
//
//  1. fillAgentResourceEvents — the read path every metrics surface uses. It
//     reads the agent's cgroup counters, merges the daemon's own observed
//     peaks, records this observation, and copies the result onto the
//     response.
//
//  2. the peak sampler — a periodic pass that records memory.current /
//     pids.current for every RUNNING agent, so the daemon has a peak to show
//     on kernels that do not expose memory.peak / pids.peak (and a peak that
//     is not merely "the value at the moment an operator asked"). Without it
//     the peak would only ever reflect metrics RPCs, which is exactly the
//     under-sampling the cgroup files were supposed to fix.

// peakSampleInterval is the sampler's period. A var (not a const) so tests can
// drive the pass directly instead of sleeping.
var peakSampleInterval = 30 * time.Second

// agentUIDLookup resolves an agent id to its Linux uid. It is a package-level
// var so tests can substitute a resolver without creating real users.
var agentUIDLookup = func(agentID string) (int, bool) {
	u, err := user.Lookup("bunker-" + agentID)
	if err != nil {
		return 0, false
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, false
	}
	return uid, true
}

// fillAgentResourceEvents reads the agent's cgroup resource events, merges the
// daemon-observed peaks (a cgroup peak always wins), records the observation
// back into the tracker, and copies the result onto resp.
//
// uid < 0 means the agent user could not be resolved: the counters are then
// unavailable by construction and the response says so, instead of reading
// user-0's slice as if it were the agent's (the same rule AgentMetrics
// follows for its host-level fallback). The read is a read-only host fact
// about a cgroup the caller already has metrics for, so no additional
// per-agent authorization is applied here; callers that need an ownership
// check already performed it (the Agent service's Metrics handler enforces
// claims.AgentID == the target).
func fillAgentResourceEvents(resp *v1.AgentMetricsResponse, uid int, agentID string, peaks *resource.PeakTracker, logger *slog.Logger) {
	if resp == nil {
		return
	}
	if uid < 0 {
		resp.ResourceEvents = &v1.ResourceEvents{
			Available: false,
			Detail:    "agent user not resolved (no bunker-" + agentID + " account): resource events unavailable",
		}
		return
	}

	ev := resource.ReadAgentResourceEvents(uid, agentID)

	// Record what this read saw before merging, so the tracker accumulates
	// real samples even if the cgroup exposes no peak files.
	if ev.MemoryCurrentBytes > 0 || ev.PidsCurrent > 0 {
		peaks.Observe(agentID, ev.MemoryCurrentBytes, ev.PidsCurrent)
	}
	if memPeak, pidsPeak, ok := peaks.Peaks(agentID); ok {
		ev.MergeDaemonPeaks(memPeak, pidsPeak, ok)
	}

	resp.ResourceEvents = &v1.ResourceEvents{
		Memory: &v1.MemoryEvents{
			Max:          ev.MemoryEvents.Max,
			High:         ev.MemoryEvents.High,
			Low:          ev.MemoryEvents.Low,
			Oom:          ev.MemoryEvents.OOM,
			OomKill:      ev.MemoryEvents.OOMKill,
			OomGroupKill: ev.MemoryEvents.OOMGroupKill,
			Available:    ev.MemoryEvents.Available,
		},
		Pids: &v1.PidsEvents{
			Max:       ev.PidsEvents.Max,
			Available: ev.PidsEvents.Available,
		},
		MemoryCurrentBytes: ev.MemoryCurrentBytes,
		PidsCurrent:        ev.PidsCurrent,
		MemoryPeakBytes:    ev.MemoryPeakBytes,
		PidsPeak:           ev.PidsPeak,
		MemoryPeakSource:   ev.MemoryPeakSource,
		PidsPeakSource:     ev.PidsPeakSource,
		CgroupPath:         ev.CgroupPath,
		Available:          ev.Available,
		Detail:             ev.Detail,
	}

	if !ev.Available && logger != nil {
		logger.Debug("agent resource events unavailable",
			"agent_id", agentID, "cgroup_path", ev.CgroupPath, "detail", ev.Detail)
	}
}

// readAgentUsageFn reads one agent's LIVE usage for the sampler. It is a
// package-level var so tests can drive the sampler without a real cgroup;
// the underlying read itself is covered by internal/resource's fixture tests
// (ReadAgentResourceEvents). available is false when the agent's cgroup could
// not be read at all, which the sampler treats as "nothing to record".
var readAgentUsageFn = func(uid int, agentID string) (memBytes, pids uint64, available bool) {
	ev := resource.ReadAgentResourceEvents(uid, agentID)
	if !ev.MemoryEvents.Available && !ev.PidsEvents.Available {
		return 0, 0, false
	}
	return ev.MemoryCurrentBytes, ev.PidsCurrent, true
}

// observeAgentResourcePeaks samples one agent's live usage into the tracker.
// A stopped or unresolvable agent is skipped (its cgroup is gone or unreadable
// by design) — the sampler's job is the live set, and its peak is dropped
// rather than left behind looking live.
func observeAgentResourcePeaks(agentID string, peaks *resource.PeakTracker) {
	if peaks == nil {
		return
	}
	uid, ok := agentUIDLookup(agentID)
	if !ok {
		// Mirror fillAgentResourceEvents: a destroyed agent whose user is
		// gone must not leave a peak behind that looks live.
		peaks.Forget(agentID)
		return
	}
	mem, pids, ok := readAgentUsageFn(uid, agentID)
	if !ok {
		return
	}
	if mem == 0 && pids == 0 {
		return
	}
	peaks.Observe(agentID, mem, pids)
}

// sampleAgentResourcePeaks runs ONE sampler pass over every running agent and
// then prunes peaks for agents that are no longer live, so the tracker stays
// bounded by the live set. It returns the number of agents observed.
func sampleAgentResourcePeaks(tracker *resource.Tracker, peaks *resource.PeakTracker) int {
	if tracker == nil || peaks == nil {
		return 0
	}
	live := make(map[string]struct{})
	observed := 0
	for _, rec := range tracker.List() {
		if rec == nil || rec.AgentID == "" {
			continue
		}
		if rec.Status != "running" {
			// A stopped agent keeps its record but has no live cgroup;
			// keeping it out of the live set is what prunes its peak, which
			// is right: the next run re-establishes it.
			continue
		}
		live[rec.AgentID] = struct{}{}
		observeAgentResourcePeaks(rec.AgentID, peaks)
		observed++
	}
	peaks.Retain(live)
	return observed
}

// startPeakSampler runs sampleAgentResourcePeaks on peakSampleInterval until
// ctx is cancelled. The first pass runs immediately so a freshly started
// daemon does not wait a full interval before it has any peak at all.
func startPeakSampler(ctx context.Context, tracker *resource.Tracker, peaks *resource.PeakTracker, logger *slog.Logger) {
	if tracker == nil || peaks == nil {
		return
	}
	go func() {
		sampleAgentResourcePeaks(tracker, peaks)
		ticker := time.NewTicker(peakSampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sampleAgentResourcePeaks(tracker, peaks)
			case <-ctx.Done():
				return
			}
		}
	}()
	if logger != nil {
		logger.Info("resource peak sampler started", "interval", peakSampleInterval.String())
	}
}
