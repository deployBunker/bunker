package fsclient

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-062, the REPORTING and RELATION half — GREEN-only by construction.
//
// These arms assert on surface this row ADDS (the frame_limit group, the derived
// reader cap, the unholdable-declaration refusal) and on the product's own named
// cause, so they cannot compile — let alone pass — against the tree the row was
// filed on. They live in their own file for one reason: the behaviour RED (the
// arms in invalidate_bfs062_test.go) stays reproducible with the FINAL text of
// both files, by setting this one aside — see docs/evidence/BFS-062-arms.sh, mode
// `unfixed`. A RED that needed yesterday's test text to reproduce would be a
// weaker claim than it looks.
// ---------------------------------------------------------------------------

// TestBFS062TheRecordNamesAndCountsTheFrameLimit is the row's acceptance as a
// record: an over-limit frame must be NAMED (the cause and a detail that carries
// both numbers), COUNTED (frame_limit.over_limit_total moves exactly once) and
// reportable as a STATE, with the bound it was measured against visible — because
// "a bound the owner cannot see is not a bound" (PRD §2.8) applies to the
// consumer's own reader as much as to the server's.
func TestBFS062TheRecordNamesAndCountsTheFrameLimit(t *testing.T) {
	// The numbers this file asserts against are the product's, not this test's.
	if got := CauseEventFrameOverLimit; string(got) != bfs062CauseEventFrameOverLimit {
		t.Fatalf("the product's cause is %q, want %q — the arms in invalidate_bfs062_test.go spell it as a literal so they compile against the filed tree, and the two must be one name", got, bfs062CauseEventFrameOverLimit)
	}
	if ClientFrameCeiling != bfs062DeclaredMaximum+bfs062FrameSlack {
		t.Fatalf("ClientFrameCeiling = %d, want the largest declaration the surface may make (%d) + the framing slack (%d): a ceiling that merely equals the declaration refuses a conforming server",
			ClientFrameCeiling, bfs062DeclaredMaximum, bfs062FrameSlack)
	}
	if eventFrameSlack != bfs062FrameSlack {
		t.Fatalf("eventFrameSlack = %d, want %d", eventFrameSlack, bfs062FrameSlack)
	}

	fx, inv, _, stream := bfs062Start(t, bfs062DeclaredDefault)
	frame := bfs062LegalFrame(t, bfs062MaxPathsPerEvent, bfs062PathMax-1, 1)
	stream.line(frame)

	if !waitFor(3*time.Second, func() bool {
		st := inv.State()
		return st.FrameLimit != nil && st.FrameLimit.OverLimitTotal > 0
	}) {
		t.Fatalf("the record never reported the frame condition: %+v", inv.State())
	}

	st := inv.State()
	fl := st.FrameLimit
	if fl == nil {
		t.Fatalf("no frame_limit group in the record: %+v", st)
	}
	// COUNTED, exactly once: counted at the decision, never per attempt or per
	// path in the frame.
	if fl.OverLimitTotal != 1 {
		t.Fatalf("frame_limit.over_limit_total = %d, want exactly 1", fl.OverLimitTotal)
	}
	if !fl.OverLimit {
		t.Fatalf("the condition is not reported as a STATE: %+v", fl)
	}
	// NAMED: the detail names the reader that refused the frame and the
	// declaration it was sized from — the two numbers a reader of the record needs
	// in order to know which side to change.
	if !contains(fl.Detail, "per-line cap") || !contains(fl.Detail, "1048576") || !contains(fl.Detail, "65536") {
		t.Fatalf("the condition does not name the reader cap and the declared bound + slack: %q", fl.Detail)
	}
	// The bound in force is visible, and it is DERIVED from the declaration.
	if fl.DeclaredBytes == nil || *fl.DeclaredBytes != bfs062DeclaredDefault {
		t.Fatalf("frame_limit.declared_bytes = %v, want the declared %d", fl.DeclaredBytes, bfs062DeclaredDefault)
	}
	if fl.DeclaredReason != "" {
		t.Fatalf("a declared bound carries no absence reason: %q", fl.DeclaredReason)
	}
	if fl.ReaderBytes == nil || *fl.ReaderBytes != bfs062DeclaredDefault+bfs062FrameSlack {
		t.Fatalf("frame_limit.reader_bytes = %v, want the declared %d + %d slack", fl.ReaderBytes, bfs062DeclaredDefault, bfs062FrameSlack)
	}
	if fl.CeilingBytes != ClientFrameCeiling {
		t.Fatalf("frame_limit.ceiling_bytes = %d, want %d", fl.CeilingBytes, ClientFrameCeiling)
	}
	// The server's own declaration is reported verbatim beside the reader, so the
	// negotiation is auditable from one record.
	if st.Server == nil || st.Server.MaxEventBytes == nil || *st.Server.MaxEventBytes != bfs062DeclaredDefault {
		t.Fatalf("the server's declared max_event_bytes is not reported: %+v", st.Server)
	}
	// And the mount is still a mount: the declared poll carries the channel.
	if !waitFor(3*time.Second, func() bool {
		s := inv.State()
		return s.Mode == ModePoll && s.Mechanism == MechanismEvents && s.Available
	}) {
		t.Fatalf("the channel did not reach the declared poll after the condition: %+v", inv.State())
	}
	// Counted once AND retried never: one stream served, ever.
	if served := fx.servedCount(); served != 1 {
		t.Fatalf("`watch` streams served = %d, want 1: the condition was retried", served)
	}
	t.Logf("frame_limit: declared=%d reader=%d ceiling=%d over_limit=%v total=%d detail=%q",
		*fl.DeclaredBytes, fl.ReaderBytes, fl.CeilingBytes, fl.OverLimit, fl.OverLimitTotal, fl.Detail)
}

// TestBFS062AnUnholdableDeclarationRefusesTheChannel is the relation's other
// direction, and the one the row's negotiation argument rests on: a server may
// declare a bound this consumer cannot hold, and the honest answer is to DECLARE
// the mismatch — before opening a stream it cannot read — rather than to clamp
// the reader silently (a reader smaller than the declaration is defect H-4) or to
// discover the problem one frame at a time.
//
// It is also the property that keeps the FLEET coherent: the refusal only fires
// for a declaration above the largest bound the server's own knob table permits,
// so a conforming deployment is never refused.
func TestBFS062AnUnholdableDeclarationRefusesTheChannel(t *testing.T) {
	// Above the surface's own ceiling for the value: a declaration no deployment
	// in this fleet can make.
	declared := int64(bfs062DeclaredMaximum) + (1 << 20)

	fx := newBFS060Fixture(t)
	fx.heartbeatMS = 40
	fx.maxEventBytes = declared
	c := fx.client(t)
	rec := &collectDrops{}
	inv := NewInvalidator(c, InvalidateOptions{
		Mode: "auto", PollInterval: 20 * time.Millisecond,
		IdleTimeout: 5 * time.Second, OnDrop: rec.drop, OnResync: rec.resync,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = inv.Run(ctx) }()

	if !waitFor(3*time.Second, func() bool {
		st := inv.State()
		return st.FrameLimit != nil && st.FrameLimit.OverLimitTotal > 0
	}) {
		t.Fatalf("an unholdable declaration produced no named condition: %+v", inv.State())
	}
	st := inv.State()
	if !contains(st.LastFailure, bfs062CauseEventFrameOverLimit) {
		t.Fatalf("the mismatch is not named as the frame condition: %q", st.LastFailure)
	}
	if !contains(st.FrameLimit.Detail, "cannot hold") {
		t.Fatalf("the mismatch does not say the declaration is unholdable: %q", st.FrameLimit.Detail)
	}
	// THE point of checking before the stream is opened: no `watch` request was
	// ever made. The bind-time capability probe is not a subscription (the stub
	// counts it as a probe, not as a served stream), so a served stream here would
	// mean the channel was opened against a bound it could not read.
	if served, probes := fx.counts(); served != 0 {
		t.Fatalf("`watch` streams served = %d (probes %d): the relation must be checked BEFORE a stream is opened — discovering the mismatch frame by frame is how this row's defect worked", served, probes)
	}
	if st.Reconnects != 0 {
		t.Fatalf("reconnects_total = %d: an unholdable declaration is deterministic and must never be retried", st.Reconnects)
	}
	if st.Mode != ModePoll {
		t.Fatalf("mode = %q, want the declared poll fallback: %+v", st.Mode, st)
	}
	// The reader was never SIZED: no stream was opened against a declaration this
	// mount cannot hold, and the record says so by ABSENCE rather than by a
	// reader_bytes of 0 ("a reader of zero bytes" is not a fact about anything).
	if st.FrameLimit.ReaderBytes != nil {
		t.Fatalf("frame_limit.reader_bytes = %d, want it absent: no reader was allocated because no stream was opened", *st.FrameLimit.ReaderBytes)
	}
	if st.FrameLimit.CeilingBytes != ClientFrameCeiling {
		t.Fatalf("frame_limit.ceiling_bytes = %d, want %d: the bound in force is the ceiling, and the declaration above it is the reported fact", st.FrameLimit.CeilingBytes, ClientFrameCeiling)
	}
}

// contains is a local alias so the three substring checks below read as prose at
// the call sites.
func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
