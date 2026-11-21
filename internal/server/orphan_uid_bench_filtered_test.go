package server

// PERF-021 (AC-3) filtered arm. This file exists separately from
// orphan_uid_bench_test.go on purpose: it references
// agent.ListUserProcessesByUIDWant, which only exists AFTER the fix, so the
// baseline harness copied onto the pre-fix tree stays compilable. Both arms
// are measured on the same host, in the same process shape, back to back.

import (
	"sort"
	"testing"

	"github.com/deployBunker/bunker/internal/agent"
)

// BenchmarkP019_ProcSweepFiltered measures one want-set sweep: the same
// ownership scan as P019_ProcSweep, with command text rendered only for the
// uids a batch classifier would have resolved. The want set is shaped like an
// idle four-agent fleet — four host uids that each own at most two live
// processes — because that is the state ListAgents usually sees (agents
// spawned, sessions running, little else under their uids).
func BenchmarkP019_ProcSweepFiltered(b *testing.B) {
	if !procBenchAvailable() {
		b.Skip("/proc is not readable on this host")
	}
	full, err := agent.ListUserProcessesByUID()
	if err != nil {
		b.Fatalf("full sweep: %v", err)
	}
	want, wantPids := fleetShapedWantSet(full, 4, 2)
	if len(want) == 0 {
		b.Skip("no uid on this host owns a small process set; nothing fleet-shaped to want")
	}
	hostPids := 0
	for _, procs := range full {
		hostPids += len(procs)
	}
	b.ReportAllocs()
	b.ResetTimer()
	// ReportMetric must come AFTER the last ReportAllocs/ResetTimer: both
	// reset the custom-metric set in this toolchain (evidence only).
	b.ReportMetric(float64(len(full)), "host_uids")
	b.ReportMetric(float64(hostPids), "host_pids")
	b.ReportMetric(float64(len(want)), "wanted_uids")
	b.ReportMetric(float64(wantPids), "wanted_pids")
	for i := 0; i < b.N; i++ {
		got, err := agent.ListUserProcessesByUIDWant(want)
		if err != nil {
			b.Fatalf("want-set sweep: %v", err)
		}
		if len(got) > len(want) {
			b.Fatalf("want-set sweep returned %d uids for a %d-uid want set", len(got), len(want))
		}
	}
}

// fleetShapedWantSet picks up to maxUIDs uids that each own at most maxProcs
// live processes, preferring the least busy ones (an idle agent fleet), and
// returns them with the process count they cover.
func fleetShapedWantSet(full map[uint32][]agent.UserProcess, maxUIDs, maxProcs int) ([]uint32, int) {
	uids := make([]uint32, 0, len(full))
	for uid, procs := range full {
		if len(procs) > 0 && len(procs) <= maxProcs {
			uids = append(uids, uid)
		}
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	if len(uids) > maxUIDs {
		uids = uids[:maxUIDs]
	}
	pids := 0
	for _, uid := range uids {
		pids += len(full[uid])
	}
	return uids, pids
}
