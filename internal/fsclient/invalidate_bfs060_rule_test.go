package fsclient

// ---------------------------------------------------------------------------
// BFS-060, the REPORTING half — GREEN-only by construction.
//
// These arms assert on the fields this row ADDS to the record (idle_timeout_ms,
// stream_ends_total, reconnects_total, idle_fallbacks_total), so they cannot
// compile against the tree the row was filed on. They live in their own file for
// exactly one reason: the behaviour RED of this row (the three arms in
// invalidate_bfs060_test.go) is reproducible with the FINAL versions of both
// files, by setting this one aside — see docs/evidence/BFS-060-arms.sh. A RED
// that needed yesterday's test text to reproduce would be a weaker claim than it
// looks.
// ---------------------------------------------------------------------------

import (
	"context"
	"strings"
	"testing"
	"time"
)

// bfs060Run is one wired channel: the stub, the invalidator, the drop recorder
// and the stream it is currently reading.
type bfs060Run struct {
	fx     *bfs060Fixture
	inv    *Invalidator
	rec    *collectDrops
	s      *bfs060Stream
	cancel context.CancelFunc
}

// bfs060Start wires a client, an invalidator and a running Run against the stub
// and hands back the first served stream.
func bfs060Start(t *testing.T, heartbeatMS int, tweak ...func(*InvalidateOptions)) *bfs060Run {
	t.Helper()
	fx := newBFS060Fixture(t)
	fx.heartbeatMS = heartbeatMS
	c := fx.client(t)
	rec := &collectDrops{}
	opt := InvalidateOptions{
		Mode: "auto", PollInterval: 20 * time.Millisecond,
		IdleTimeout: 5 * time.Second, OnDrop: rec.drop, OnResync: rec.resync,
	}
	for _, f := range tweak {
		f(&opt)
	}
	inv := NewInvalidator(c, opt)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = inv.Run(ctx) }()
	return &bfs060Run{fx: fx, inv: inv, rec: rec, s: fx.expectStream(t, 3*time.Second), cancel: cancel}
}

// TestBFS060TheRecordNamesTheRuleAndCountsItsFirings is the reporting half of the
// row: a channel that was declared dead must be able to say so ONCE the poll has
// answered, and the bound that decided it must be visible rather than implied
// ("a bound the owner cannot see is not a bound", PRD §2.8). Without these the
// record of a channel that died and came back is the record of one that never
// died — which is the class of defect this row is made of.
func TestBFS060TheRecordNamesTheRuleAndCountsItsFirings(t *testing.T) {
	t.Run("the rule is the relation the server declares", func(t *testing.T) {
		// The document declares 40 ms, so three missed heartbeats is 120 ms —
		// NOT the 90 s default, and not a number the client chose.
		r := bfs060Start(t, 40, func(o *InvalidateOptions) { o.IdleTimeout = 0 })
		r.s.line(bfs060Heartbeat(1))
		// The wait covers the whole degradation, not just the first counter
		// tick: stallToPoll writes idle_fallbacks_total, the stalled flag and
		// the mode change in three separate critical sections, so a reader
		// that samples between the first and the last can see the fallback
		// counted while the record still names the watcher (INT-CI-089's
		// class: an assertion sampled mid-publication measures the scheduler
		// under load, not the channel). The relations are stable once the
		// poll has answered, and every negative case still fails this arm.
		var st InvalidationState
		if !waitFor(3*time.Second, func() bool {
			st = r.inv.State()
			return st.IdleFallbacks > 0 && st.Mode == ModePoll && st.Mechanism != MechanismWatch
		}) {
			t.Fatalf("the idle rule never fired on a stream that went silent: %+v", st)
		}
		if st.IdleTimeoutMS == nil || *st.IdleTimeoutMS != 120 {
			t.Fatalf("idle_timeout_ms = %v, want 120 (3 × the declared 40 ms heartbeat)", st.IdleTimeoutMS)
		}
		if st.IdleFallbacks != 1 {
			t.Fatalf("idle_fallbacks_total = %d, want 1", st.IdleFallbacks)
		}
		if !strings.Contains(st.Reason, "stall") {
			t.Fatalf("the reason does not name the stall: %q", st.Reason)
		}
	})

	t.Run("no declared heartbeat falls back to the pinned 90s", func(t *testing.T) {
		r := bfs060Start(t, 0, func(o *InvalidateOptions) { o.IdleTimeout = 0 })
		// idle_timeout_ms is not a stored field but a REPORT: State() only
		// names it once the push rule is in force, i.e. once the client
		// goroutine has actually established the stream — which can sit
		// hundreds of milliseconds after the stub accepted the POST under
		// load. Sampling the record at the instant the fixture handed the
		// stream over therefore reads mode="auto"/idle_timeout_ms=nil and
		// reds on the scheduler's schedule, not on the channel (observed
		// live at loadavg ~90 while landing INT-CI-089). So both the bound
		// and the liveness verdict wait for the established record; a
		// channel that never establishes still fails this arm, one bound
		// later.
		var st InvalidationState
		if !waitFor(3*time.Second, func() bool {
			st = r.inv.State()
			return st.Available && st.Mode == ModePush && st.Mechanism == MechanismWatch
		}) {
			t.Fatalf("the channel was not established: %+v", st)
		}
		if st.IdleTimeoutMS == nil || *st.IdleTimeoutMS != int64(DefaultIdleTimeout/time.Millisecond) {
			t.Fatalf("idle_timeout_ms = %v, want %d for a document that names no heartbeat", st.IdleTimeoutMS, int64(DefaultIdleTimeout/time.Millisecond))
		}
	})

	t.Run("a clean EOF is counted, and so is the reconnect", func(t *testing.T) {
		r := bfs060Start(t, 40)
		r.s.line(bfs060Invalidate(1, "src/main.go"))
		if !waitFor(3*time.Second, func() bool { return r.rec.dropped("src/main.go") }) {
			t.Fatalf("nothing was delivered before the EOF: %s", r.rec.summary())
		}
		r.s.end()
		_ = r.fx.expectStream(t, 3*time.Second) // the reconnect
		if !waitFor(3*time.Second, func() bool {
			st := r.inv.State()
			return st.StreamEnds == 1 && st.Reconnects >= 1
		}) {
			t.Fatalf("the clean EOF and the reconnect were not both counted: %+v", r.inv.State())
		}
		st := r.inv.State()
		// The counters settle BEFORE the record does: reconnects_total is
		// counted when the retry fires, the stub serves the new stream the
		// moment it accepts its POST, but `available` is published only after
		// the CLIENT has received and checked that response — three scheduling
		// points on three goroutines. Sampling the liveness verdict once, just
		// after the counters agree, reads a window the host can stretch to
		// hundreds of milliseconds under load (INT-CI-089's recorded red:
		// reconnects=1, failures=1, available=false at ~512 ms last-event
		// age). So the verdict is waited for like every other assertion in
		// this file; a channel that never reports live again still fails, one
		// bound later.
		if !waitFor(3*time.Second, func() bool {
			st = r.inv.State()
			return st.Available && st.Mode == ModePush && st.Mechanism == MechanismWatch
		}) {
			t.Fatalf("after the reconnect the channel must be reported live again: %+v", st)
		}
		if !strings.Contains(st.Reason, "ended") {
			t.Fatalf("the reason does not name the channel end: %q", st.Reason)
		}
	})
}
