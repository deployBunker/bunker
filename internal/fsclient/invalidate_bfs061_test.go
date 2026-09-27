package fsclient

// ---------------------------------------------------------------------------
// BFS-061 — the heartbeat must not manufacture a gap.
// (SPEC-push-channel §10 H-2; the clause it is fixed under is §3.1 R-1, and the
// clause that forbids the cheap fix is §3.3 R-3.)
//
// Three arms. The first measures the DEFECT; the second and third pin the
// properties the defect must not be traded away for:
//
//   A TestBFS061HeartbeatsDoNotManufactureAGap — N heartbeats, then ONE real
//     change. On the tree this row was filed on the change reads as a gap and
//     costs a whole-tree resync (counted: resyncs_from_gap / resyncs_total);
//     after the fix it is applied precisely and neither counter moves.
//   B TestBFS061ARealGapIsStillDetected — a change whose seq was genuinely
//     skipped still resyncs and still drops the whole view. This arm passes on
//     BOTH trees on purpose: it is the invariant, not the defect, and the
//     mutant that trades it away is `docs/evidence/BFS-061-arms.sh loosegap`.
//   C TestBFS061AHeartbeatCannotMaskARealGap — the same real gap, with a
//     HEARTBEAT as the only surviving line. A heartbeat's seq is evidence about
//     the same interval a change's is, so the cursor may not be advanced over an
//     unobserved range without resyncing: "record it and stay quiet" is the
//     silent-staleness reading of the same rule, and it is exactly what the
//     cheap fix — advance the cursor, exempt the check — would do.
//
// The arms assert on the LANDED counters of BFS-045 (`resyncs_from_gap`,
// `resyncs_total`, `liveness.heartbeats_total`) and add no parallel counter of
// their own. They compile against the filed tree as well as the fixed one —
// nothing here names a symbol the fix introduces — which is what makes the RED
// reproducible from the FINAL test text (see the arms script).
// ---------------------------------------------------------------------------

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// bfs061Observed is every number this row is argued from, read out of the landed
// record in one place so each arm's failure message carries them rather than a
// description of them.
type bfs061Observed struct {
	Seq        int64
	Gaps       int64
	Resyncs    int64
	Events     int64
	Dropped    int64
	Heartbeats int64
	Reason     string
}

func bfs061Of(inv *Invalidator) bfs061Observed {
	st := inv.State()
	o := bfs061Observed{
		Seq: st.Seq, Gaps: st.Gaps, Resyncs: st.Resyncs,
		Events: st.Events, Dropped: st.DroppedPaths, Reason: st.Reason,
	}
	if st.Liveness != nil {
		o.Heartbeats = st.Liveness.HeartbeatsTotal
	}
	return o
}

func (o bfs061Observed) String() string {
	return fmt.Sprintf("seq=%d resyncs_from_gap=%d resyncs_total=%d events_total=%d paths_dropped_total=%d heartbeats_total=%d reason=%q",
		o.Seq, o.Gaps, o.Resyncs, o.Events, o.Dropped, o.Heartbeats, o.Reason)
}

// bfs061FullDrops is the number of FULL-VIEW drops the invalidator asked for:
// each one is a resync of the whole tree, which is the cost this row is about.
func (r *bfs060Run) bfs061FullDrops() int {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	return r.rec.full
}

// bfs061ResyncReasons is the resync reasons, copied under the recorder's lock (a
// bare field read here would be a data race under -race against the invalidator's
// goroutine).
func (r *bfs060Run) bfs061ResyncReasons() []string {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	return append([]string(nil), r.rec.resyncs...)
}

// ---------------------------------------------------------------------------
// Arm A — the defect: N heartbeats then one change must NOT cost a resync.
// ---------------------------------------------------------------------------

func TestBFS061HeartbeatsDoNotManufactureAGap(t *testing.T) {
	const heartbeats = 5

	// No declared heartbeat period, and a 5 s idle bound: the CADENCE is not what
	// this arm measures (BFS-060 owns the idle rule), and the bound keeps the
	// fixture's silence from being read as a stall while the arm is being driven.
	r := bfs060Start(t, 0)

	// (1) One real change. It is what ARMS the gap check: the check is guarded on
	//     a non-zero cursor (BFS-063), and a fresh client has none — so an arm
	//     that skipped this step would measure nothing.
	r.s.line(bfs060Invalidate(1, "src/main.go"))
	if !waitFor(3*time.Second, func() bool { return r.rec.dropped("src/main.go") }) {
		t.Fatalf("the pushed channel delivered nothing: %s", r.rec.summary())
	}
	if st := r.inv.State(); st.Mode != ModePush || st.Mechanism != MechanismWatch || !st.Available {
		t.Fatalf("the lines were not delivered over the pushed channel, so the arm would be measuring the wrong mechanism: %+v", st)
	}
	before := bfs061Of(r.inv)
	if before.Gaps != 0 || before.Resyncs != 0 {
		t.Fatalf("the arm's own baseline is already resyncing, before any heartbeat: %s", before)
	}

	// (2) N heartbeats, each carrying its own ledger seq (SPEC-push-channel §3.4:
	//     a heartbeat consumes one). The wait is on the RECEIPT of the
	//     heartbeats, so the interleaving under test is the scripted one and not
	//     a race between the writer and the reader.
	for n := 2; n <= 1+heartbeats; n++ {
		r.s.line(bfs060Heartbeat(int64(n)))
	}
	if !waitFor(3*time.Second, func() bool { return bfs061Of(r.inv).Heartbeats >= heartbeats }) {
		t.Fatalf("%d heartbeats were written to the stream but the channel recorded %d received: %s",
			heartbeats, bfs061Of(r.inv).Heartbeats, bfs061Of(r.inv))
	}

	// (3) ONE real change, consecutive on the ledger: 1 is the change, 2..6 are
	//     the heartbeats, so this one is 7. Either outcome is waited for — the
	//     resync is the defect and must not turn the arm into a timeout.
	r.s.line(bfs060Invalidate(1+heartbeats+1, "src/util.go"))
	if !waitFor(3*time.Second, func() bool {
		return r.rec.dropped("src/util.go") || bfs061Of(r.inv).Resyncs > before.Resyncs
	}) {
		t.Fatalf("the change after %d heartbeats was neither applied nor resynced: %s", heartbeats, bfs061Of(r.inv))
	}
	after := bfs061Of(r.inv)

	// THE DEFECT, COUNTED: the heartbeat's seq was consumed and never recorded, so
	// the next real event satisfied `Seq > cursor+1`, the gap check fired, and the
	// whole view was dropped — the keep-alive manufacturing the condition it
	// exists to prevent.
	if after.Gaps != before.Gaps || after.Resyncs != before.Resyncs || r.bfs061FullDrops() > 0 {
		t.Fatalf("THE HEARTBEAT MANUFACTURED A GAP: %d heartbeats then ONE change moved resyncs_from_gap %d→%d and resyncs_total %d→%d with %d full-view drop(s) [%s] — every keep-alive costs a whole-tree resync on the next change (H-2). Required: no gap, no resync, and the change applied precisely.",
			heartbeats, before.Gaps, after.Gaps, before.Resyncs, after.Resyncs,
			r.bfs061FullDrops(), after)
	}
	if !r.rec.dropped("src/util.go") {
		t.Fatalf("no gap was reported, but the change was not applied either: %s", after)
	}
	if after.Seq != 1+heartbeats+1 {
		t.Fatalf("the cursor is %d, want %d: the heartbeat's seq was received and not recorded as an observation (SPEC-push-channel §3.1 R-1) — %s",
			after.Seq, 1+heartbeats+1, after)
	}
	if reasons := r.bfs061ResyncReasons(); len(reasons) != 0 {
		t.Fatalf("the change after %d heartbeats drove %d resync(s) (%v): %s", heartbeats, len(reasons), reasons, after)
	}
}

// ---------------------------------------------------------------------------
// Arm B — the invariant: a REAL gap is still a gap.
// ---------------------------------------------------------------------------

// TestBFS061ARealGapIsStillDetected is the control that stops this row being
// "fixed" by loosening the gap check. seq 2 is genuinely missed — the interval
// was never observed — and no answer may stand in for it (§3.3 R-3). The arm
// passes on BOTH the filed tree and the fixed one; the `loosegap` mutant of the
// arms script is what turns it red.
func TestBFS061ARealGapIsStillDetected(t *testing.T) {
	r := bfs060Start(t, 0)
	r.s.line(bfs060Invalidate(1, "src/main.go"))
	if !waitFor(3*time.Second, func() bool { return r.rec.dropped("src/main.go") }) {
		t.Fatalf("the pushed channel delivered nothing: %s", r.rec.summary())
	}

	// seq 2 is missing: the next line the client sees is 3.
	r.s.line(bfs060Invalidate(3, "src/util.go"))
	if !waitFor(3*time.Second, func() bool {
		st := r.inv.State()
		return st.Gaps > 0 || st.Resyncs > 0
	}) {
		t.Fatalf("A REAL GAP WAS NOT DETECTED: a change at seq=3 arrived on a cursor of 1, so the interval [2] was never observed, and the client reported %s having dropped %d path(s) — the gap check has been loosened, which trades this row's P1 for a silent-staleness P0.",
			bfs061Of(r.inv), r.inv.State().DroppedPaths)
	}
	after := bfs061Of(r.inv)
	if after.Gaps < 1 || after.Resyncs < 1 {
		t.Fatalf("the real gap did not move the landed counters: %s", after)
	}
	if r.bfs061FullDrops() < 1 {
		t.Fatalf("the gap was counted but the whole view was never dropped (full-view drops=%d): a post-gap line was applied as though the missing range had been covered — %s",
			r.bfs061FullDrops(), after)
	}
	if r.rec.dropped("src/util.go") {
		t.Fatalf("the post-gap change was applied as a precise drop: the missing range is never re-requested and the client has no way to know it was skipped — %s", after)
	}
	if !strings.Contains(after.Reason, "sequence gap at seq=3") {
		t.Fatalf("the resync does not name the gap it was caused by: reason=%q, want a reason naming seq=3", after.Reason)
	}
	if after.Seq != 3 {
		t.Fatalf("the cursor is %d, want 3: the missing range would be re-requested on the next line (BFS-005 §4.1) — %s", after.Seq, after)
	}
}

// ---------------------------------------------------------------------------
// Arm C — the cheap fix's own probe: a heartbeat may not mask a real gap.
// ---------------------------------------------------------------------------

// TestBFS061AHeartbeatCannotMaskARealGap is the one that matters. The same real
// gap as arm B, but the only line that survives it is a HEARTBEAT at seq 3. A
// heartbeat's seq is evidence about the SAME interval a change's is, so advancing
// the cursor over it and staying quiet would vouch for a range nobody observed:
// a silent gap, which §3.3 R-3 forbids outright. The arm therefore requires the
// heartbeat to be BOTH recorded (R-1) and its jump resynced (§3.3).
func TestBFS061AHeartbeatCannotMaskARealGap(t *testing.T) {
	r := bfs060Start(t, 0)
	r.s.line(bfs060Invalidate(1, "src/main.go"))
	if !waitFor(3*time.Second, func() bool { return r.rec.dropped("src/main.go") }) {
		t.Fatalf("the pushed channel delivered nothing: %s", r.rec.summary())
	}

	// seq 2 is lost; the keep-alive at seq 3 is the only evidence that the
	// interval it sits behind was never observed.
	r.s.line(bfs060Heartbeat(3))
	if !waitFor(3*time.Second, func() bool {
		st := r.inv.State()
		return st.Gaps > 0 || st.Resyncs > 0
	}) {
		t.Fatalf("A HEARTBEAT MASKED A REAL GAP: the keep-alive at seq=3 arrived on a cursor of 1, and the record is %s — the client now holds a cursor or a silence that vouches for the interval [2] it never observed, and nothing will ever re-request it. A heartbeat must be recorded (SPEC-push-channel §3.1 R-1) AND compared to the cursor (§3.3 R-3), never consumed-and-discarded.",
			bfs061Of(r.inv))
	}
	after := bfs061Of(r.inv)
	if after.Gaps < 1 || after.Resyncs < 1 {
		t.Fatalf("the heartbeat's jump did not move the landed counters: %s", after)
	}
	if r.bfs061FullDrops() < 1 {
		t.Fatalf("the heartbeat's jump was counted but the whole view was never dropped (full-view drops=%d): %s", r.bfs061FullDrops(), after)
	}
	if !strings.Contains(after.Reason, "sequence gap at seq=3") {
		t.Fatalf("the resync does not name the gap the heartbeat revealed: reason=%q, want a reason naming seq=3", after.Reason)
	}
	if after.Seq != 3 {
		t.Fatalf("the cursor is %d, want 3: the heartbeat's seq was not recorded, so the next line would be judged against a cursor the client has already overtaken — %s", after.Seq, after)
	}
}
