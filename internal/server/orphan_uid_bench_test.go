package server

// PERF-021 attribution harness (AC-3/AC-4). ListAgents' cost is ONE /proc
// sweep for the whole fleet, so this harness measures it at the two places
// that matter:
//
//   - P019_ListAgents_Full: the wire handler, in-process, over a registered
//     fleet of N agents — the number the operator surface pays per poll.
//   - P019_ProcSweep: one every-uid /proc sweep on its own, the floor below
//     which ListAgents cannot go for any fleet size.
//
// This file is deliberately compilable against the PRE-PERF-021 tree: it uses
// only surfaces that existed before the want-set variant, so the same harness
// measures both trees and the medians are comparable (AC-3's baseline).
// P019_ProcSweepFiltered lives in orphan_uid_bench_filtered_test.go because
// the want-set entry only exists after the fix.
//
// The fleet is shaped so the sweep dominates: every agent's user resolves
// through the lookup seam (no NSS, no home stat), no live process belongs to
// a bench uid, and per-agent disk usage is pre-published in the TTL cache so
// no background walk can land inside a timed iteration.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// procBenchAvailable reports whether a live /proc is readable; the benchmarks
// skip (never fail) on a host where the probes cannot run.
func procBenchAvailable() bool {
	if _, err := os.ReadDir("/proc"); err != nil {
		return false
	}
	_, err := os.Stat("/proc/self/status")
	return err == nil
}

// BenchmarkP019_ListAgents_Full measures the whole ListAgents path for a fleet
// of 4, 41 and 2931 agents: tracker list, per-agent disk-usage lookup from
// the TTL cache, metadata resolution per agent, and ONE /proc sweep for the
// batch verdict. Nothing is cached across calls (freshness is per-request by
// design, PERF-008), so each iteration pays the sweep.
func BenchmarkP019_ListAgents_Full(b *testing.B) {
	if !procBenchAvailable() {
		b.Skip("/proc is not readable on this host")
	}
	for _, n := range []int{4, 41, 2931} {
		n := n
		b.Run(fmt.Sprintf("agents=%d", n), func(b *testing.B) {
			benchListAgents(b, n)
		})
	}
}

// BenchmarkP019_ProcSweep measures one every-uid sweep through the exported
// entry (the shape both trees carry): os.ReadDir("/proc") + one status read
// per live pid + one cmdline read per live pid.
func BenchmarkP019_ProcSweep(b *testing.B) {
	if !procBenchAvailable() {
		b.Skip("/proc is not readable on this host")
	}
	table, err := agent.ListUserProcessesByUID()
	if err != nil {
		b.Fatalf("full sweep: %v", err)
	}
	pids := 0
	for _, procs := range table {
		pids += len(procs)
	}
	b.ReportMetric(float64(len(table)), "host_uids")
	b.ReportMetric(float64(pids), "host_pids")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := agent.ListUserProcessesByUID(); err != nil {
			b.Fatalf("full sweep: %v", err)
		}
	}
}

// benchListAgents builds a service holding n registered agents and benchmarks
// ListAgents against it.
func benchListAgents(b *testing.B, n int) {
	b.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.DefaultConfig()
	cfg.Agent.BaseDataDir = b.TempDir()
	cfg.Agent.Registry.Enabled = false // local mode: no registry round trip

	m := agent.NewAgentManager(cfg, logger, resource.NewTracker(uint32(n+16), logger), nil, nil)
	b.Cleanup(m.Stop)

	// Every bench agent's user resolves, so the batch classifier takes the
	// healthy metadata path (one lookup, no home stat) and the sweep is the
	// only real work per iteration.
	homes := b.TempDir()
	prevHome := agent.SwapAgentHomeRoot(homes)
	b.Cleanup(func() { agent.SwapAgentHomeRoot(prevHome) })
	prevLookup := agent.SwapLookupUser(func(name string) (*user.User, error) {
		idx, err := strconv.Atoi(strings.TrimPrefix(name, "bunker-bench-"))
		if err != nil || !strings.HasPrefix(name, "bunker-bench-") {
			return nil, user.UnknownUserError(name)
		}
		return &user.User{Uid: strconv.Itoa(60000 + idx), Name: name}, nil
	})
	b.Cleanup(func() { agent.SwapLookupUser(prevLookup) })

	// Pre-publish the disk-usage snapshots: usageFor would otherwise start one
	// background walk per agent on its first miss, and those goroutines would
	// land inside the timed loop.
	prevWalk := diskUsageWalk
	diskUsageWalk = func(string) uint64 { return 0 }
	b.Cleanup(func() { diskUsageWalk = prevWalk })

	tracker := resource.NewTracker(uint32(n+16), logger)
	ids := make([]string, 0, n)
	now := diskUsageNow()
	agentDiskUsageCache.mu.Lock()
	if agentDiskUsageCache.entries == nil {
		agentDiskUsageCache.entries = make(map[string]diskUsageEntry, n)
	}
	for i := 0; i < n; i++ {
		rec := &resource.AgentRecord{AgentID: fmt.Sprintf("bench-%05d", i), Status: "running"}
		if err := tracker.Register(rec); err != nil {
			agentDiskUsageCache.mu.Unlock()
			b.Fatalf("register %s: %v", rec.AgentID, err)
		}
		agentDiskUsageCache.entries[rec.AgentID] = diskUsageEntry{bytes: 0, refreshed: now}
		ids = append(ids, rec.AgentID)
	}
	agentDiskUsageCache.mu.Unlock()

	svc := &bunkerdService{
		cfg:                      cfg,
		logger:                   logger,
		tracker:                  tracker,
		agentMgr:                 m,
		orphanUIDSummarizer:      m.OrphanUIDSummary,
		orphanUIDBatchSummarizer: m.OrphanUIDSummaries,
	}

	// One warm-up call so the first-iteration cost (verdict plumbing, first
	// sweep's page cache) does not inflate the reported median.
	if _, err := svc.ListAgents(context.Background(), connect.NewRequest(&v1.ListAgentsRequest{})); err != nil {
		b.Fatalf("warm-up ListAgents: %v", err)
	}

	b.ReportMetric(float64(n), "agents")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := svc.ListAgents(context.Background(), connect.NewRequest(&v1.ListAgentsRequest{}))
		if err != nil {
			b.Fatalf("ListAgents: %v", err)
		}
		if got := len(resp.Msg.GetAgents()); got != n {
			b.Fatalf("ListAgents returned %d agents, want %d", got, n)
		}
	}
}
