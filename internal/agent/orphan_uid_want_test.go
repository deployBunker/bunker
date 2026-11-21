package agent

// PERF-021 (AC-1/AC-2): the want-set sweep must be INDISTINGUISHABLE from the
// every-uid sweep filtered down to the wanted uids — same Processes slice per
// uid, same PID ordering, same rendered command text. Two pins:
//
//   - TestFilteredSweepMatchesFullSweep: a fixture /proc tree, so the answer
//     is exact and the test can never flake on host churn. This is the
//     regression pin (it fails on any drift between the two renders).
//   - TestP019_FilteredSweepMatchesFull: the live host, walking every uid
//     present, retried inside a stable process-table window (AC-2's wording).
//     It skips under -short and when /proc is unavailable.

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// procTreeAvailable reports whether the /proc root the probes read is there
// and listable. Benchmarks and the live equivalence test skip when it is not.
func procTreeAvailable() bool {
	if _, err := os.ReadDir(procStatusPath); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(procStatusPath, "self", "status"))
	return err == nil
}

// filterProcTable is the caller-side filter the want-set sweep must match:
// the full table restricted to the wanted uids, absent uids simply missing.
func filterProcTable(full map[uint32][]userProcess, want map[uint32]struct{}) map[uint32][]userProcess {
	out := make(map[uint32][]userProcess, len(want))
	for uid := range want {
		if procs, ok := full[uid]; ok {
			out[uid] = procs
		}
	}
	return out
}

// writeProcFixtureEntry materialises one pid directory in a fixture /proc
// tree. cmdline may be nil (kernel-thread/zombie shape: the render must fall
// back to the bracketed comm).
func writeProcFixtureEntry(t *testing.T, root, pid, status string, cmdline []byte) {
	t.Helper()
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("fixture pid %s: %v", pid, err)
	}
	if status != "" {
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
			t.Fatalf("fixture pid %s status: %v", pid, err)
		}
	}
	if cmdline != nil {
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), cmdline, 0o644); err != nil {
			t.Fatalf("fixture pid %s cmdline: %v", pid, err)
		}
	}
}

// TestFilteredSweepMatchesFullSweep pins the equivalence on a fixture tree:
// want == every uid present reproduces the full sweep exactly, a strict
// subset reproduces the full sweep filtered to that subset, and a wanted uid
// that owns nothing stays absent (never an empty slice).
func TestFilteredSweepMatchesFullSweep(t *testing.T) {
	root := t.TempDir()
	uidA, uidB, uidC := uint32(4242), uint32(4243), uint32(4244)
	statusFor := func(name string, uids ...uint32) string {
		s := "Name:\t" + name + "\nUid:\t"
		for i, u := range uids {
			if i > 0 {
				s += "\t"
			}
			s += itoaUID(u)
		}
		return s + "\n"
	}
	// 1200/1201: uidA, one with a cmdline and one without (comm fallback).
	writeProcFixtureEntry(t, root, "1200", statusFor("alpha", uidA), []byte("alpha\x00--flag\x00"))
	writeProcFixtureEntry(t, root, "1201", statusFor("alpha-twin", uidA), nil)
	// 1202: uidB with a cmdline.
	writeProcFixtureEntry(t, root, "1202", statusFor("beta", uidB), []byte("beta\x00serve\x00"))
	// 1203: real uid B, saved-set uid C (statusOwnerUIDs groups it under both).
	writeProcFixtureEntry(t, root, "1203", "Name:\tgamma\nUid:\t"+itoaUID(uidB)+"\t"+itoaUID(uidB)+"\t"+itoaUID(uidC)+"\t"+itoaUID(uidC)+"\n", []byte("gamma\x00run\x00"))
	// 1204: uid 1, never wanted by these cases.
	writeProcFixtureEntry(t, root, "1204", statusFor("init", 1), []byte("init\x00"))
	// Not a pid directory, and a pid directory whose status vanished: both
	// skipped by the scan, so both sweeps skip them identically.
	if err := os.MkdirAll(filepath.Join(root, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProcFixtureEntry(t, root, "1205", "", nil)

	prev := SwapProcStatusPath(root)
	t.Cleanup(func() { SwapProcStatusPath(prev) })

	full, err := listUserProcessesByUID()
	if err != nil {
		t.Fatalf("full sweep over fixture: %v", err)
	}
	// Premise: the fixture is actually exercised, so an empty-everything
	// comparison cannot pass vacuously.
	if len(full[uidA]) != 2 || len(full[uidB]) != 2 || len(full[uidC]) != 1 || len(full[1]) != 1 {
		t.Fatalf("fixture premise broken: full sweep = %+v", full)
	}
	if got := full[uidA][0].Cmd; got != "alpha --flag" {
		t.Fatalf("fixture premise broken: rendered cmdline = %q, want %q", got, "alpha --flag")
	}
	if got := full[uidA][1].Cmd; got != "[alpha-twin]" {
		t.Fatalf("fixture premise broken: comm fallback = %q, want %q", got, "[alpha-twin]")
	}

	wantAll := make(map[uint32]struct{}, len(full))
	uids := make([]uint32, 0, len(full))
	for uid := range full {
		wantAll[uid] = struct{}{}
		uids = append(uids, uid)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })

	gotAll, err := listUserProcessesByUIDWant(wantAll)
	if err != nil {
		t.Fatalf("want-set sweep (every uid): %v", err)
	}
	if !reflect.DeepEqual(gotAll, full) {
		t.Fatalf("want-set sweep over EVERY present uid must equal the full sweep:\nfull: %+v\nwant: %+v", full, gotAll)
	}

	for _, want := range []map[uint32]struct{}{
		{uidA: {}},
		{uidB: {}},
		{uidA: {}, uidC: {}},
		{uidA: {}, uidB: {}, uidC: {}},
	} {
		got, err := listUserProcessesByUIDWant(want)
		if err != nil {
			t.Fatalf("want-set sweep %v: %v", want, err)
		}
		wantTable := filterProcTable(full, want)
		if !reflect.DeepEqual(got, wantTable) {
			t.Fatalf("want-set sweep %v drifted from the filtered full sweep:\nwant: %+v\ngot:  %+v", want, wantTable, got)
		}
		for uid, procs := range got {
			for i := 1; i < len(procs); i++ {
				if procs[i-1].PID > procs[i].PID {
					t.Fatalf("uid %d: PIDs not sorted: %+v", uid, procs)
				}
			}
			for _, p := range procs {
				if p.Cmd == "" {
					t.Fatalf("uid %d pid %d: wanted process rendered an empty command", uid, p.PID)
				}
			}
		}
	}

	// A wanted uid owning nothing live is ABSENT — exactly what filtering the
	// full sweep leaves — never an empty non-nil slice.
	gotAbsent, err := listUserProcessesByUIDWant(map[uint32]struct{}{999_999: {}, uidA: {}})
	if err != nil {
		t.Fatalf("want-set sweep with an absent uid: %v", err)
	}
	if _, ok := gotAbsent[999_999]; ok {
		t.Fatalf("absent wanted uid must stay out of the map, got %+v", gotAbsent[999_999])
	}
	if !reflect.DeepEqual(gotAbsent[uidA], full[uidA]) {
		t.Fatalf("present wanted uid drifted: %+v vs %+v", gotAbsent[uidA], full[uidA])
	}
}

// TestP019_FilteredSweepMatchesFull is AC-2 on the live host: walk every uid
// present on /proc, run the want-set sweep over all of them, and require it
// to equal the full sweep filtered down to those uids — same Processes slice,
// same PID order, same rendered command text.
//
// A busy host spawns and reaps constantly, so a uid whose process set changes
// between two sweeps cannot be judged: those uids are EXCLUDED from the
// equality assertion and counted out loud, while a uid that is identical
// before and after the want-set sweep (the stability witness) must match it
// exactly. Zero judgeable uids means the window was useless, so the sweep is
// retried, and a host that never settles skips rather than reporting a lie.
func TestP019_FilteredSweepMatchesFull(t *testing.T) {
	if testing.Short() {
		t.Skip("live /proc equivalence: skipped under -short (AC-2)")
	}
	if !procTreeAvailable() {
		t.Skipf("%s is not readable; the live equivalence cannot be measured", procStatusPath)
	}

	sameProcs := func(a, b []userProcess) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	const attempts = 6
	var compared, churned int
	for attempt := 1; attempt <= attempts; attempt++ {
		full1, err := listUserProcessesByUID()
		if err != nil {
			t.Fatalf("full sweep: %v", err)
		}
		want := make(map[uint32]struct{}, len(full1))
		uids := make([]uint32, 0, len(full1))
		for uid := range full1 {
			want[uid] = struct{}{}
			uids = append(uids, uid)
		}
		sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
		// A strict subset too: uids from the middle of the sorted list, which
		// on a real host is a spread of service uids.
		subset := make(map[uint32]struct{})
		for i := 0; i < len(uids) && i < 2; i++ {
			subset[uids[len(uids)/2-i]] = struct{}{}
		}

		gotAll, err := listUserProcessesByUIDWant(want)
		if err != nil {
			t.Fatalf("want-set sweep over every live uid: %v", err)
		}
		gotSub, err := listUserProcessesByUIDWant(subset)
		if err != nil {
			t.Fatalf("want-set sweep over a subset: %v", err)
		}

		full2, err := listUserProcessesByUID()
		if err != nil {
			t.Fatalf("second full sweep: %v", err)
		}

		// Churn-independent invariants first: the want-set sweep can never
		// invent a uid, misorder PIDs, or render an empty command.
		for uid := range gotAll {
			if _, ok := want[uid]; !ok {
				t.Fatalf("want-set sweep returned uid %d, which was never wanted", uid)
			}
		}
		for _, table := range []map[uint32][]userProcess{gotAll, gotSub} {
			for uid, procs := range table {
				for i := 1; i < len(procs); i++ {
					if procs[i-1].PID > procs[i].PID {
						t.Fatalf("uid %d: PIDs not sorted: %+v", uid, procs)
					}
				}
				for _, p := range procs {
					if p.Cmd == "" {
						t.Fatalf("uid %d pid %d: wanted process rendered an empty command", uid, p.PID)
					}
				}
			}
		}

		compared, churned = 0, 0
		for uid := range want {
			if !sameProcs(full1[uid], full2[uid]) {
				churned++ // the table moved under the sweep: not judgeable
				continue
			}
			if !sameProcs(gotAll[uid], filterProcTable(full1, want)[uid]) {
				t.Fatalf("uid %d: want-set sweep drifted from the full sweep:\nfull: %+v\nwant: %+v", uid, full1[uid], gotAll[uid])
			}
			if _, ok := subset[uid]; ok {
				if !sameProcs(gotSub[uid], filterProcTable(full1, subset)[uid]) {
					t.Fatalf("uid %d: want-set sweep over the subset drifted:\nfull: %+v\nwant: %+v", uid, full1[uid], gotSub[uid])
				}
			}
			compared++
		}
		if compared > 0 {
			t.Logf("live equivalence asserted over %d of %d uids present (%d moved during the window and were excluded)", compared, len(want), churned)
			return
		}
	}
	t.Skipf("host process table churned across all %d comparison windows (%d judged, %d moved); the live equivalence could not be asserted", attempts, compared, churned)
}
