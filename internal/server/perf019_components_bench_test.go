package server

// PERF-019 component attribution harness.
//
// The deployed daemon's audit records (audit/interceptor.go WrapUnary) measure
// ONLY the handler: DurationMS = time.Since(start) immediately after the
// handler returns the response message, i.e. before connect encodes it. So the
// 11-24 ms the row quotes is handler-internal, and the CACHED-path question is
// "what does the handler itself spend, once the disk-usage TTL cache is warm".
//
// This file decomposes that handler on a registry shaped to the measured
// production size (2931 records) into the components the row names, so each
// one gets a number and a share instead of a hypothesis:
//
//   - tracker.List() copy              BenchmarkP019_TrackerList
//   - registry read                    (structural: none — see the evidence
//     file; the durable registry is replayed at boot, the handler reads the
//     in-memory tracker only)
//   - per-record assembly              BenchmarkP019_ToAgentSummary (summary
//     conversion) and BenchmarkP019_DiskUsageLookup (TTL-cache read)
//   - whole assembly pass              BenchmarkP019_Assembly
//   - cached handler, no orphan check  BenchmarkP019_HandlerNoOrphan
//   - orphan metadata resolution       BenchmarkP019_OrphanMetadata
//   - the one /proc sweep              BenchmarkP019_HandlerFull (which
//     subsumes it) — the sweep's own cost is measured in
//     orphan_uid_bench_test.go's BenchmarkP019_ProcSweepFiltered
//   - proto / JSON encode              BenchmarkP019_EncodeProto / _EncodeJSON
//
// Every benchmark reports ns/op for the whole 2931-record pass, plus a
// per-record metric where it makes sense. The fleet is built the way
// orphan_uid_bench_test.go builds it: users resolve through the lookup seam,
// no live process belongs to a bench uid, and disk-usage snapshots are
// pre-published so no background walk lands inside a timed iteration.

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
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	bunkerAgent "github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// p019RegistrySize is the production registry size the row measured
// (/var/lib/bunkerd/agents.jsonl: 2931 records).
const p019RegistrySize = 2931

// p019Fleet is one benchmark fleet: the service under test, its tracker, the
// registered ids, and the manager the orphan classifier runs on.
type p019Fleet struct {
	svc     *bunkerdService
	tracker *resource.Tracker
	mgr     *bunkerAgent.AgentManager
	ids     []string
}

// newP019Fleet builds a service tracking n agents with every external seam
// pinned: user lookups resolve from the switched lookup seam, the agent home
// root is a temp dir, the disk-usage TTL cache is a fresh cache seeded with a
// frozen snapshot per agent (so the CACHED path never spawns a walk), and the
// registry stays disabled (the durable registry is a boot-time input — nothing
// in the handler reads it).
func newP019Fleet(b *testing.B, n int) *p019Fleet {
	b.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.DefaultConfig()
	cfg.Agent.BaseDataDir = b.TempDir()
	cfg.Agent.Registry.Enabled = false

	tracker := resource.NewTracker(uint32(n+16), logger)
	mgr := bunkerAgent.NewAgentManager(cfg, logger, tracker, nil, nil)
	b.Cleanup(mgr.Stop)

	// Every agent's user resolves: the batch classifier takes the healthy
	// metadata path (one lookup, no home stat).
	prevHome := bunkerAgent.SwapAgentHomeRoot(b.TempDir())
	b.Cleanup(func() { bunkerAgent.SwapAgentHomeRoot(prevHome) })
	prevLookup := bunkerAgent.SwapLookupUser(func(name string) (*user.User, error) {
		idx, err := strconv.Atoi(strings.TrimPrefix(name, "bunker-bench-"))
		if err != nil || !strings.HasPrefix(name, "bunker-bench-") {
			return nil, user.UnknownUserError(name)
		}
		return &user.User{Uid: strconv.Itoa(60000 + idx), Name: name}, nil
	})
	b.Cleanup(func() { bunkerAgent.SwapLookupUser(prevLookup) })

	// A private disk-usage cache with a frozen clock: every entry is fresh for
	// the whole benchmark, so no walk is ever scheduled.
	frozen := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	prevCache := agentDiskUsageCache
	prevNow := diskUsageNow
	prevWalk := diskUsageWalk
	agentDiskUsageCache = &diskUsageCache{ttl: diskUsageTTL, entries: make(map[string]diskUsageEntry, n)}
	diskUsageNow = func() time.Time { return frozen }
	diskUsageWalk = func(string) uint64 { return 0 }
	b.Cleanup(func() {
		agentDiskUsageCache = prevCache
		diskUsageNow = prevNow
		diskUsageWalk = prevWalk
	})

	f := &p019Fleet{tracker: tracker, mgr: mgr, ids: make([]string, 0, n)}
	for i := 0; i < n; i++ {
		rec := &resource.AgentRecord{
			AgentID:        fmt.Sprintf("bench-%05d", i),
			Status:         "running",
			CreatedAt:      frozen.Add(-time.Duration(i) * time.Minute),
			ExpiresAt:      frozen.Add(24 * time.Hour),
			PublicURL:      fmt.Sprintf("https://bench-%05d.example.invalid", i),
			TailnetIP:      fmt.Sprintf("100.64.%d.%d", i/250, i%250),
			PortRangeStart: uint32(20000 + i*2),
			PortRangeEnd:   uint32(20001 + i*2),
			DiskUsedBytes:  uint64(i) * 1024,
		}
		if err := tracker.Register(rec); err != nil {
			b.Fatalf("register %s: %v", rec.AgentID, err)
		}
		agentDiskUsageCache.entries[rec.AgentID] = diskUsageEntry{bytes: rec.DiskUsedBytes, refreshed: frozen}
		f.ids = append(f.ids, rec.AgentID)
	}

	f.svc = &bunkerdService{
		cfg:                      cfg,
		logger:                   logger,
		tracker:                  tracker,
		agentMgr:                 mgr,
		orphanUIDSummarizer:      mgr.OrphanUIDSummary,
		orphanUIDBatchSummarizer: mgr.OrphanUIDSummaries,
	}
	return f
}

// listRequest is the empty request `bunker list` sends.
func listRequest() *connect.Request[v1.ListAgentsRequest] {
	return connect.NewRequest(&v1.ListAgentsRequest{})
}

// BenchmarkP019_TrackerList is the tracker.List() copy alone: one RLock, one
// 2931-pointer slice allocation, no per-record work.
func BenchmarkP019_TrackerList(b *testing.B) {
	f := newP019Fleet(b, p019RegistrySize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := len(f.tracker.List()); got != p019RegistrySize {
			b.Fatalf("List() returned %d records", got)
		}
	}
}

// BenchmarkP019_ToAgentSummary is the per-record assembly alone: the proto
// message construction for all 2931 records (three time.Format calls each).
func BenchmarkP019_ToAgentSummary(b *testing.B) {
	f := newP019Fleet(b, p019RegistrySize)
	records := f.tracker.List()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sums := make([]*v1.AgentSummary, 0, len(records))
		for _, rec := range records {
			sums = append(sums, rec.ToAgentSummary())
		}
		if len(sums) != p019RegistrySize {
			b.Fatalf("assembled %d summaries", len(sums))
		}
	}
}

// BenchmarkP019_DiskUsageCacheRead is the per-agent TTL-cache read alone:
// 2931 usageFor calls, one mutex acquisition each, all fresh hits.
func BenchmarkP019_DiskUsageCacheRead(b *testing.B) {
	f := newP019Fleet(b, p019RegistrySize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sink uint64
		for _, id := range f.ids {
			sink += agentDiskUsageCache.usageFor(id)
		}
		if sink == ^uint64(0) {
			b.Fatal("unreachable")
		}
	}
}

// BenchmarkP019_Assembly is the handler body WITHOUT the orphan check:
// tracker.List(), the live-id set, pruneLive, and the per-record summary +
// disk-usage assembly (service.go's ListAgents up to the orphan block). It is
// the registry-scaled half of the cached path.
func BenchmarkP019_Assembly(b *testing.B) {
	f := newP019Fleet(b, p019RegistrySize)
	b.ReportAllocs()
	b.ResetTimer()
	b.ReportMetric(float64(p019RegistrySize), "agents")
	for i := 0; i < b.N; i++ {
		records := f.tracker.List()
		liveIDs := make(map[string]struct{}, len(records))
		for _, rec := range records {
			liveIDs[rec.AgentID] = struct{}{}
		}
		agentDiskUsageCache.pruneLive(liveIDs)
		summaries := make([]*v1.AgentSummary, 0, len(records))
		for _, rec := range records {
			summary := rec.ToAgentSummary()
			summary.DiskUsedBytes = agentDiskUsageCache.usageFor(rec.AgentID)
			summaries = append(summaries, summary)
		}
		if len(summaries) != p019RegistrySize {
			b.Fatalf("assembled %d summaries", len(summaries))
		}
	}
}

// BenchmarkP019_HandlerNoOrphan runs the real handler with the orphan
// classifier unwired (orphanUIDBatchSummarizer nil), which is the whole
// registry-scaled cached path: assembly plus the response construction.
func BenchmarkP019_HandlerNoOrphan(b *testing.B) {
	f := newP019Fleet(b, p019RegistrySize)
	f.svc.orphanUIDBatchSummarizer = nil
	f.svc.orphanUIDSummarizer = nil
	b.ReportAllocs()
	b.ResetTimer()
	b.ReportMetric(float64(p019RegistrySize), "agents")
	for i := 0; i < b.N; i++ {
		resp, err := f.svc.ListAgents(context.Background(), listRequest())
		if err != nil {
			b.Fatalf("ListAgents: %v", err)
		}
		if got := len(resp.Msg.GetAgents()); got != p019RegistrySize {
			b.Fatalf("ListAgents returned %d agents", got)
		}
	}
}

// BenchmarkP019_OrphanMetadata isolates the batch classifier's metadata half
// from the /proc sweep: OrphanUIDSummaries runs its real id validation and
// per-agent uid resolution, with the sweep seam substituted by an empty
// process table (so no /proc read is timed).
func BenchmarkP019_OrphanMetadata(b *testing.B) {
	f := newP019Fleet(b, p019RegistrySize)
	prevSweep := bunkerAgent.SwapProcSweep(func() (map[uint32][]bunkerAgent.UserProcess, error) {
		return map[uint32][]bunkerAgent.UserProcess{}, nil
	})
	b.Cleanup(func() { bunkerAgent.SwapProcSweep(prevSweep) })
	b.ReportAllocs()
	b.ResetTimer()
	b.ReportMetric(float64(p019RegistrySize), "agents")
	for i := 0; i < b.N; i++ {
		verdicts := f.svc.orphanUIDBatchSummarizer(f.ids)
		if len(verdicts) != p019RegistrySize {
			b.Fatalf("classifier returned %d verdicts", len(verdicts))
		}
	}
}

// BenchmarkP019_HandlerFull is the production handler path end to end,
// orphan classifier and its one /proc sweep included — the number the daemon's
// audit records quote. It is the composition check for the component table.
func BenchmarkP019_HandlerFull(b *testing.B) {
	if !procBenchAvailable() {
		b.Skip("/proc is not readable on this host")
	}
	f := newP019Fleet(b, p019RegistrySize)
	b.ReportAllocs()
	b.ResetTimer()
	b.ReportMetric(float64(p019RegistrySize), "agents")
	for i := 0; i < b.N; i++ {
		resp, err := f.svc.ListAgents(context.Background(), listRequest())
		if err != nil {
			b.Fatalf("ListAgents: %v", err)
		}
		if got := len(resp.Msg.GetAgents()); got != p019RegistrySize {
			b.Fatalf("ListAgents returned %d agents", got)
		}
	}
}

// BenchmarkP019_EncodeProto is the wire-encode cost the handler's own
// duration_ms does NOT include (connect marshals after the interceptor
// returns): the binary proto encoding of a 2931-record ListAgentsResponse.
func BenchmarkP019_EncodeProto(b *testing.B) {
	f := newP019Fleet(b, p019RegistrySize)
	f.svc.orphanUIDBatchSummarizer = nil
	f.svc.orphanUIDSummarizer = nil
	resp, err := f.svc.ListAgents(context.Background(), listRequest())
	if err != nil {
		b.Fatalf("ListAgents: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		raw, err := proto.Marshal(resp.Msg)
		if err != nil {
			b.Fatalf("marshal: %v", err)
		}
		if len(raw) == 0 {
			b.Fatal("empty encoding")
		}
	}
	b.ReportMetric(float64(len(mustProto(b, resp.Msg))), "wire_bytes")
}

// BenchmarkP019_EncodeJSON is the same response encoded by connect's JSON
// codec path (protojson), which a JSON-configured client pays.
func BenchmarkP019_EncodeJSON(b *testing.B) {
	f := newP019Fleet(b, p019RegistrySize)
	f.svc.orphanUIDBatchSummarizer = nil
	f.svc.orphanUIDSummarizer = nil
	resp, err := f.svc.ListAgents(context.Background(), listRequest())
	if err != nil {
		b.Fatalf("ListAgents: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		raw, err := protojson.Marshal(resp.Msg)
		if err != nil {
			b.Fatalf("json marshal: %v", err)
		}
		if len(raw) == 0 {
			b.Fatal("empty encoding")
		}
	}
}

// p019HostPIDs returns the distinct live pid count and the set of uids that
// own at least one process on THIS host, taken from one untimed full sweep.
func p019HostPIDs(b *testing.B) (int, []uint32) {
	b.Helper()
	owners, err := bunkerAgent.ListUserProcessesByUID()
	if err != nil {
		b.Skipf("/proc sweep unavailable on this host: %v", err)
	}
	uids := make([]uint32, 0, len(owners))
	pids := make(map[int]struct{})
	for uid, procs := range owners {
		uids = append(uids, uid)
		for _, p := range procs {
			pids[p.PID] = struct{}{}
		}
	}
	return len(pids), uids
}

// BenchmarkP019_SweepOwnershipScan is the irreducible half of the /proc
// sweep: the directory walk plus one /proc/<pid>/status read per live pid,
// with command rendering suppressed (empty want set). This is the floor any
// per-request sweep implementation pays, on this host, at this pid count.
func BenchmarkP019_SweepOwnershipScan(b *testing.B) {
	pids, _ := p019HostPIDs(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.ReportMetric(float64(pids), "procs")
	for i := 0; i < b.N; i++ {
		got, err := bunkerAgent.ListUserProcessesByUIDWant(nil)
		if err != nil {
			b.Fatalf("want-set sweep: %v", err)
		}
		if len(got) != 0 {
			b.Fatalf("empty want set returned %d uids", len(got))
		}
	}
}

// BenchmarkP019_SweepWantAllHost is the production-shaped sweep: the want set
// is every uid that owns a live process here, which is what a registry full of
// resident agents looks like (PERF-021's cmdline filter then buys nothing).
// The delta against SweepOwnershipScan is the command-rendering share.
func BenchmarkP019_SweepWantAllHost(b *testing.B) {
	pids, uids := p019HostPIDs(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.ReportMetric(float64(pids), "procs")
	for i := 0; i < b.N; i++ {
		got, err := bunkerAgent.ListUserProcessesByUIDWant(uids)
		if err != nil {
			b.Fatalf("want-set sweep: %v", err)
		}
		if len(got) == 0 {
			b.Fatal("want-all-host sweep returned no uids")
		}
	}
}

// The three per-pid kernel paths a /proc ownership scan can use, measured
// side by side so the sweep's floor is a number rather than an opinion.
// These are floor probes, not proposed implementations: only
// BenchmarkP019_SweepOwnershipScan exercises production code.
//
//   ProcDirWalk    - ReadDirName(getdents64) only: pid list, no owner info.
//   ProcDirStat    - walk + lstat per pid (DirEntry.Info): the CHEAPEST source
//                    of an owner uid the kernel offers. It reports ONE uid per
//                    process, so it cannot express the real+saved-set rule
//                    statusFileOwnedByUID implements (kill(2) same-uid rule).
//   ProcDirStatus  - walk + one /proc/<pid>/status read: what scanProcOwners
//                    does, and the only path carrying all four uids.

func BenchmarkP019_ProcDirWalk(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries, err := os.ReadDir(procRootForBench(b))
		if err != nil {
			b.Fatalf("readdir: %v", err)
		}
		if len(entries) == 0 {
			b.Fatal("empty /proc")
		}
		b.ReportMetric(float64(len(entries)), "entries")
	}
}

func BenchmarkP019_ProcDirStat(b *testing.B) {
	root := procRootForBench(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries, err := os.ReadDir(root)
		if err != nil {
			b.Fatalf("readdir: %v", err)
		}
		owned := 0
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := strconv.Atoi(e.Name()); err != nil {
				continue
			}
			if _, err := e.Info(); err == nil {
				owned++
			}
		}
		b.ReportMetric(float64(owned), "procs")
	}
}

func BenchmarkP019_ProcDirStatus(b *testing.B) {
	root := procRootForBench(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries, err := os.ReadDir(root)
		if err != nil {
			b.Fatalf("readdir: %v", err)
		}
		owned := 0
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := strconv.Atoi(e.Name()); err != nil {
				continue
			}
			if raw, err := os.ReadFile(root + "/" + e.Name() + "/status"); err == nil && len(raw) > 0 {
				owned++
			}
		}
		b.ReportMetric(float64(owned), "procs")
	}
}

// procRootForBench reads the /proc root the product sweep honours (the
// SwapProcStatusPath seam) without disturbing it: set-then-restore returns
// the live value, so a fixture-substituted root is measured too.
func procRootForBench(b *testing.B) string {
	b.Helper()
	cur := bunkerAgent.SwapProcStatusPath("/proc")
	bunkerAgent.SwapProcStatusPath(cur)
	if cur == "" {
		b.Skip("/proc root seam is unset")
	}
	return cur
}

func mustProto(b *testing.B, msg proto.Message) []byte {
	b.Helper()
	raw, err := proto.Marshal(msg)
	if err != nil {
		b.Fatalf("marshal: %v", err)
	}
	return raw
}
