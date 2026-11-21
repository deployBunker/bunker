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
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
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

// sameProcs reports whether two per-uid renders are identical: same PIDs in
// the same order, same rendered command text.
func sameProcs(a, b []userProcess) bool {
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

// pidDelta splits two per-uid renders into the pids only a carries (extra) and
// the pids only b carries (missing). MEMBERSHIP only — rendered text is
// compared separately, because the two directions mean opposite things (see
// classifyUIDEquivalence).
func pidDelta(a, b []userProcess) (extra, missing []int) {
	inB := make(map[int]struct{}, len(b))
	for _, p := range b {
		inB[p.PID] = struct{}{}
	}
	inA := make(map[int]struct{}, len(a))
	for _, p := range a {
		inA[p.PID] = struct{}{}
		if _, ok := inB[p.PID]; !ok {
			extra = append(extra, p.PID)
		}
	}
	for _, p := range b {
		if _, ok := inA[p.PID]; !ok {
			missing = append(missing, p.PID)
		}
	}
	return extra, missing
}

// emptyRenderUIDs returns every uid whose render carries at least one EMPTY
// command. That is the churn marker, not a defect marker: procCmdlineHead
// (orphan_uid.go) never returns "" for a pid that merely vanished — an
// unreadable or empty cmdline falls back to the bracketed comm from the status
// body, then to "(unknown)" when the status is gone too (pinned by
// TestWantSweepVanishedPIDIsSurvivable). The single empty path is a cmdline
// read that SUCCEEDS with non-empty content which trims to nothing: the kernel
// zero-filled a dying process's argument area between the status read and the
// cmdline read. So an empty render is evidence of a process dying mid-render —
// the full sweep pays the identical read — never of a filter defect.
func emptyRenderUIDs(tables ...map[uint32][]userProcess) map[uint32]struct{} {
	tainted := make(map[uint32]struct{})
	for _, table := range tables {
		for uid, procs := range table {
			for _, p := range procs {
				if p.Cmd == "" {
					tainted[uid] = struct{}{}
					break
				}
			}
		}
	}
	return tainted
}

// equivalenceVerdict is what one uid's three observations support.
type equivalenceVerdict int

const (
	// equivalenceJudged: the table held still across the whole window and the
	// want-set render reproduced the full sweep exactly.
	equivalenceJudged equivalenceVerdict = iota
	// equivalenceChurned: the window moved under the sweep, so this uid says
	// nothing either way. NOT a failure — the retry ladder absorbs it.
	equivalenceChurned
	// equivalenceDefect: the sweep observed something the filter cannot
	// structurally produce. Always a failure.
	equivalenceDefect
)

// commRender reports whether a rendered command came from the process's COMM
// (the bracketed Name: fallback) or from no readable source at all, rather
// than from the process's argv.
//
// The distinction matters because comm is NOT a stable property of a pid:
// kernel worker threads rename themselves in place as they take work off
// different workqueues. Measured on this host (2026-11-20): 31 of 292
// comm-rendered pids changed name inside a 6-second window, alternating shapes
// like "kworker/14:2-events" / "kworker/14:2-mm_percpu_wq" and
// "kworker/u64:9+iou_exit" / "kworker/u64:9-iou_exit". A pid that never exits
// therefore renders two different command heads in two sweeps.
func commRender(cmd string) bool {
	return cmd == "(unknown)" || (strings.HasPrefix(cmd, "[") && strings.HasSuffix(cmd, "]"))
}

// textDrift returns the pids whose rendered text differs between two renders
// of the SAME pid set, and whether every one of those differences is
// comm-derived on at least one side (i.e. explained by a rename or a
// readability transition rather than by the filter disagreeing with itself).
// Callers must only pass renders whose pid sets are already known to be equal
// and sorted; the comparison is index-aligned on that guarantee.
func textDrift(a, b []userProcess) (pids []int, commOnly bool) {
	commOnly = true
	if len(a) != len(b) {
		return nil, false
	}
	for i := range a {
		if a[i].PID != b[i].PID || a[i].Cmd == b[i].Cmd {
			continue
		}
		pids = append(pids, a[i].PID)
		if !commRender(a[i].Cmd) && !commRender(b[i].Cmd) {
			commOnly = false
		}
	}
	return pids, commOnly
}

// classifyUIDEquivalence judges ONE uid from the three observations the live
// sweep takes: full1 (before), got (the want-set sweep under test), full2
// (after).
//
// The asymmetry between a MISSING pid and an EXTRA pid is the whole
// race-honesty of this test, and it is deliberate:
//
//   - a pid BOTH full sweeps saw was alive at both ends of the window, hence
//     alive in the middle: a want sweep without it DROPPED a live process,
//     whatever else the host was doing. Defect.
//   - a pid only the want sweep saw was born and reaped entirely inside the
//     window. scanProcOwners can only report a pid whose status read
//     succeeded, so the process did exist; the full sweeps simply bracketed
//     its whole life. Churn.
//
// Rendered TEXT is compared only where it is a stable property: argv-derived
// commands must match exactly, while a comm-derived render may legitimately
// differ because kernel workers rename themselves in place (commRender). An
// empty render taints the uid before the text comparison, because an empty
// command is not an observation of the filter's output at all — it is an
// observation of a process dying (see emptyRenderUIDs). Membership is checked
// before all of that so a dropped pid can never hide behind a rename or a
// dying neighbour.
func classifyUIDEquivalence(full1, full2, got []userProcess, tainted bool) (equivalenceVerdict, string) {
	if !sameProcs(full1, full2) {
		return equivalenceChurned, "the process table changed between the two full sweeps"
	}
	extra, missing := pidDelta(got, full1)
	if len(missing) > 0 {
		return equivalenceDefect, fmt.Sprintf("the want-set sweep lost pids %v that BOTH full sweeps observed:\nfull: %+v\nwant: %+v", missing, full1, got)
	}
	if tainted {
		return equivalenceChurned, "a process died mid-render and rendered an empty command"
	}
	if len(extra) > 0 {
		return equivalenceChurned, fmt.Sprintf("the window spawned and reaped pids %v", extra)
	}
	if pids, commOnly := textDrift(full1, got); len(pids) > 0 {
		if commOnly {
			return equivalenceChurned, fmt.Sprintf("comm-derived renders changed for pids %v (kernel workers rename themselves in place)", pids)
		}
		return equivalenceDefect, fmt.Sprintf("argv-derived renders drifted for pids %v on a stable process set:\nfull: %+v\nwant: %+v", pids, full1, got)
	}
	if !sameProcs(got, full1) {
		return equivalenceDefect, fmt.Sprintf("the want-set sweep rendered a different table for a uid whose process set was stable:\nfull: %+v\nwant: %+v", full1, got)
	}
	return equivalenceJudged, ""
}

// countProcPIDs counts the numeric pid directories under the probe's /proc
// root. scanProcOwners stops after maxProcScan entries in readdir order, so on
// a host above that cap the three sweeps cover different prefixes of the
// table and no comparison between them is meaningful.
func countProcPIDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(procStatusPath)
	if err != nil {
		t.Skipf("%s became unreadable mid-test: %v", procStatusPath, err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err == nil {
			n++
		}
	}
	return n
}

// TestP019_FilteredSweepMatchesFull is AC-2 on the live host: walk every uid
// present on /proc, run the want-set sweep over all of them, and require it
// to equal the full sweep filtered down to those uids — same Processes slice,
// same PID order, same rendered command text.
//
// A busy host spawns and reaps constantly, so a uid whose process set moves
// during the window cannot be judged: those uids are EXCLUDED from the
// equality assertion and counted out loud (see classifyUIDEquivalence for
// which movements are churn and which are defects), while a uid whose table
// held still across the whole window must match exactly. Zero judgeable uids
// means the window was useless, so the sweep is retried, and a host that never
// settles skips rather than reporting a lie.
func TestP019_FilteredSweepMatchesFull(t *testing.T) {
	if testing.Short() {
		t.Skip("live /proc equivalence: skipped under -short (AC-2)")
	}
	if !procTreeAvailable() {
		t.Skipf("%s is not readable; the live equivalence cannot be measured", procStatusPath)
	}
	if n := countProcPIDs(t); n >= maxProcScan {
		t.Skipf("host exposes %d pid entries; the sweep's maxProcScan cap (%d) truncates by readdir order, so the three sweeps cannot be compared", n, maxProcScan)
	}

	const attempts = 6
	var judgedTotal, churnedTotal int
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

		// Churn-INDEPENDENT invariants first: neither sweep can invent a uid
		// nor misorder PIDs, under any host behaviour. renderProcCommands
		// keys its output map off `want` (orphan_uid.go:330-341 — out[uid] is
		// only ever written for a uid taken from want), so a foreign uid is
		// structurally impossible; scanProcOwners sorts every uid's slice
		// before any render runs (orphan_uid.go:318-320), so out-of-order PIDs
		// are too. Both stay Fail, never churn.
		for _, arm := range []struct {
			name string
			want map[uint32]struct{}
			got  map[uint32][]userProcess
		}{
			{name: "every live uid", want: want, got: gotAll},
			{name: "the subset", want: subset, got: gotSub},
		} {
			for uid := range arm.got {
				if _, ok := arm.want[uid]; !ok {
					t.Fatalf("%s: want-set sweep returned uid %d, which was never wanted", arm.name, uid)
				}
			}
			for uid, procs := range arm.got {
				for i := 1; i < len(procs); i++ {
					if procs[i-1].PID > procs[i].PID {
						t.Fatalf("%s: uid %d: PIDs not sorted: %+v", arm.name, uid, procs)
					}
				}
			}
		}

		tainted := emptyRenderUIDs(gotAll, gotSub)
		compared, churned := 0, 0
		for _, uid := range uids {
			_, uidTainted := tainted[uid]
			verdict, detail := classifyUIDEquivalence(full1[uid], full2[uid], gotAll[uid], uidTainted)
			if verdict == equivalenceDefect {
				t.Fatalf("uid %d: %s", uid, detail)
			}
			if verdict == equivalenceChurned {
				churned++
				continue
			}
			if _, ok := subset[uid]; ok {
				subVerdict, subDetail := classifyUIDEquivalence(full1[uid], full2[uid], gotSub[uid], uidTainted)
				if subVerdict == equivalenceDefect {
					t.Fatalf("uid %d (subset): %s", uid, subDetail)
				}
				if subVerdict == equivalenceChurned {
					churned++
					continue
				}
			}
			compared++
		}
		judgedTotal += compared
		churnedTotal += churned
		if compared > 0 {
			t.Logf("live equivalence asserted over %d of %d uids present (%d moved or tainted during the window and were excluded; attempt %d of %d)", compared, len(want), churned, attempt, attempts)
			return
		}
	}
	t.Skipf("host process table churned across all %d comparison windows (%d judged, %d moved or tainted); the live equivalence could not be asserted", attempts, judgedTotal, churnedTotal)
}

// TestWantSweepVanishedPIDIsSurvivable pins the production contract for the
// exact shape the live test must absorb instead of failing on: a pid the
// ownership scan observed that is GONE by the time its command is rendered.
//
// Production today neither drops nor blanks it. renderProcCommands renders
// every pid the scan reported, and procCmdlineHead falls back — to the
// bracketed comm from the status body when only the cmdline is gone, and to
// "(unknown)" when the status is gone too. Rendered text is therefore never
// empty BECAUSE a pid vanished; the only empty path is a cmdline read that
// succeeds with zero-filled content, which is a process dying mid-render. That
// distinction is what this pin nails down, and it is what lets the live test
// treat an empty render as churn while keeping a dropped pid a hard failure.
func TestWantSweepVanishedPIDIsSurvivable(t *testing.T) {
	root := t.TempDir()
	uidVanished, uidDying := uint32(7001), uint32(7002)
	statusFor := func(name string, uid uint32) string {
		u := itoaUID(uid)
		return "Name:	" + name + "\nUid:	" + u + "	" + u + "	" + u + "	" + u + "\n"
	}
	// 2100: readable status at scan time, cmdline already gone -> the pid
	// vanishes between the ownership scan and its command render.
	writeProcFixtureEntry(t, root, "2100", statusFor("ghost", uidVanished), nil)
	// 2101: cmdline present but zero-filled, the dying-process shape.
	writeProcFixtureEntry(t, root, "2101", statusFor("dying", uidDying), make([]byte, 64))

	prev := SwapProcStatusPath(root)
	t.Cleanup(func() { SwapProcStatusPath(prev) })

	got, err := listUserProcessesByUIDWant(map[uint32]struct{}{uidVanished: {}, uidDying: {}})
	if err != nil {
		t.Fatalf("want sweep must SURVIVE a pid vanishing mid-render, got error: %v", err)
	}
	if len(got[uidVanished]) != 1 || got[uidVanished][0].PID != 2100 {
		t.Fatalf("vanished pid must be RETAINED by the render (production drops nothing), got %+v", got[uidVanished])
	}
	if want := "[ghost]"; got[uidVanished][0].Cmd != want {
		t.Fatalf("vanished pid renders the comm fallback, got %q want %q", got[uidVanished][0].Cmd, want)
	}
	if len(got[uidDying]) != 1 || got[uidDying][0].PID != 2101 {
		t.Fatalf("dying pid must be RETAINED, got %+v", got[uidDying])
	}
	if got[uidDying][0].Cmd != "" {
		t.Fatalf("zero-filled cmdline renders empty (the churn marker), got %q", got[uidDying][0].Cmd)
	}

	// The classifier must read those two renders in opposite ways: the comm
	// fallback is judgeable, the empty render is churn — never a defect.
	tainted := emptyRenderUIDs(got)
	if _, ok := tainted[uidDying]; !ok {
		t.Fatalf("an empty render must taint its uid as churn, tainted = %v", tainted)
	}
	if _, ok := tainted[uidVanished]; ok {
		t.Fatalf("a comm fallback is a real observation and must NOT be treated as churn, tainted = %v", tainted)
	}

	// Classifier table: the two directions of movement, the two kinds of
	// render churn, plus a genuine drift, must land on the verdicts their
	// comments claim.
	base := []userProcess{{PID: 2100, Cmd: "[ghost]"}, {PID: 2101, Cmd: "dying"}}
	// commBase: both renders come from the comm fallback, which is exactly the
	// shape a kernel worker presents — and which it rewrites in place as it
	// takes work off a different workqueue (measured: 31 of 292 comm-rendered
	// pids renamed inside 6 seconds on this host).
	commBase := []userProcess{{PID: 2100, Cmd: "[kworker/1:2-events]"}, {PID: 2101, Cmd: "[kworker/2:2-events]"}}
	for _, tc := range []struct {
		name    string
		full1   []userProcess
		full2   []userProcess
		got     []userProcess
		tainted bool
		want    equivalenceVerdict
	}{
		{name: "stable and equal", full1: base, full2: base, got: base, want: equivalenceJudged},
		{name: "table moved between full sweeps", full1: base, full2: base[:1], got: base, want: equivalenceChurned},
		{name: "empty render (dying process)", full1: base, full2: base, got: []userProcess{{PID: 2100, Cmd: "[ghost]"}, {PID: 2101}}, tainted: true, want: equivalenceChurned},
		{name: "pid born and reaped inside the window", full1: base, full2: base, got: append(append([]userProcess{}, base...), userProcess{PID: 2199, Cmd: "gone"}), want: equivalenceChurned},
		{name: "kernel worker renamed in place", full1: commBase, full2: commBase, got: []userProcess{{PID: 2100, Cmd: "[kworker/1:2-mm_percpu_wq]"}, {PID: 2101, Cmd: "[kworker/2:2-events]"}}, want: equivalenceChurned},
		{name: "argv became unreadable (readability transition)", full1: commBase, full2: commBase, got: []userProcess{{PID: 2100, Cmd: "kworker-argv"}, {PID: 2101, Cmd: "[kworker/2:2-events]"}}, want: equivalenceChurned},
		{name: "dropped a pid both full sweeps saw", full1: base, full2: base, got: base[:1], want: equivalenceDefect},
		{name: "different argv on a stable process set", full1: base, full2: base, got: []userProcess{{PID: 2100, Cmd: "[ghost]"}, {PID: 2101, Cmd: "respelled"}}, want: equivalenceDefect},
	} {
		verdict, detail := classifyUIDEquivalence(tc.full1, tc.full2, tc.got, tc.tainted)
		if verdict != tc.want {
			t.Errorf("%s: verdict = %v (%s), want %v", tc.name, verdict, detail, tc.want)
		}
		if verdict != equivalenceJudged && detail == "" {
			t.Errorf("%s: a non-judged verdict must say why", tc.name)
		}
	}

	// Fully vanished pid (status gone too, so no comm is available): the seam
	// stands in for an ownership scan that observed a pid whose directory is
	// gone by render time. Production renders "(unknown)" and keeps the entry.
	origScan := listUserProcessesByUIDFn
	listUserProcessesByUIDFn = func() (map[uint32][]userProcess, error) {
		return map[uint32][]userProcess{7777: {{PID: 7777}}}, nil
	}
	t.Cleanup(func() { listUserProcessesByUIDFn = origScan })

	gotGhost, err := listUserProcessesByUIDWant(map[uint32]struct{}{7777: {}})
	if err != nil {
		t.Fatalf("want sweep must survive a fully vanished pid, got error: %v", err)
	}
	if len(gotGhost[7777]) != 1 || gotGhost[7777][0].PID != 7777 {
		t.Fatalf("fully vanished pid must be RETAINED, got %+v", gotGhost[7777])
	}
	if want := "(unknown)"; gotGhost[7777][0].Cmd != want {
		t.Fatalf("fully vanished pid renders %q, want %q", gotGhost[7777][0].Cmd, want)
	}
}
