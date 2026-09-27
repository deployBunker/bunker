package fsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-062 — the consumer half. The producer half (the events assembly measuring
// its own serialized frame against the declared bound, and the document
// publishing that bound) lives in internal/server/webdav
// (events_bfs062_test.go).
//
// The row: the server's event frame was bounded by a path COUNT (4096) while
// this consumer's reader was bounded by BYTES (8 MiB). 4096 paths of PATH_MAX is
// ~16.8 MiB of JSON on ONE line, so a frame the server was entitled to send
// exceeded the reader, and `bufio.ErrTooLong` was classified as a TRANSPORT
// fault — a reconnect loop on a DETERMINISTIC input, which is an outage.
//
// These arms drive the channel's own wire form through the BFS-060 stub
// (`POST X-Bunker-Op: watch`, NDJSON, one JSON object per line), with the
// server's declared limits as the INPUTS:
//
//   - the frame is built from the real limits (4096 paths × PATH_MAX = the
//     server's permitted maximum), never as a hand-rolled giant;
//   - the counters asserted on are the ones the record PUBLISHES
//     (frame_limit.over_limit_total, reconnects_total), so the arms measure what
//     an owner can read;
//   - "no reconnect" is measured twice: the counter stays 0 AND no second
//     `watch` stream is ever served, over a window longer than two backoffs
//     (500 ms → 1 s).
//
// Written to run against the UNFIXED tree too, which is what makes the RED a
// RED: the first arm waits for EITHER the named condition OR a reconnect and
// fails with the reconnect count and the transport-flavoured `last_failure` when
// the misclassification is present.
// ---------------------------------------------------------------------------

// The producer's own declared limits, quoted with their provenance rather than
// invented: a frame built outside them would be measuring this test's arithmetic
// instead of the contract.
const (
	// bfs062MaxPathsPerEvent is BFS-004 §3 E-6's declaration 2 — the cap the
	// server enforces on one event's path list (webdav/events.go's
	// eventsMaxPathsPerEvent, pinned by TestEventsOpDeclaredBounds).
	bfs062MaxPathsPerEvent = 4096
	// bfs062PathMax is Linux's PATH_MAX in bytes, INCLUDING the terminating NUL,
	// so the longest path a walk can hand the assembler is PATH_MAX-1 bytes.
	bfs062PathMax = 4096
	// bfs062DeclaredDefault is the bound Spec §7.2 asserts and the server's knob
	// table defaults to (invalidation.DefaultPushMaxEventBytes).
	bfs062DeclaredDefault = 1 << 20
	// bfs062DeclaredMaximum is the largest bound the server's own knob table
	// permits it to declare (invalidation.MaxPushMaxEventBytes = 8 MiB). It is
	// the number the consumer's ceiling is derived from, so the arms use it
	// rather than the ceiling itself: declaring the ceiling is what the DERIVED
	// ceiling exists to forbid.
	bfs062DeclaredMaximum = 8 << 20
)

// bfs062LegalFrame builds the frame the SERVER's own limits permit at their
// maximum, using the encoder of the very struct this client decodes (Event): the
// frame is one JSON object, so its serialized size IS what a per-line reader must
// hold. `paths` paths of `pathBytes` bytes each.
func bfs062LegalFrame(t *testing.T, paths, pathBytes int, seq int64) string {
	t.Helper()
	list := make([]string, 0, paths)
	seg := strings.Repeat("p", pathBytes)
	for i := 0; i < paths; i++ {
		list = append(list, fmt.Sprintf("%s%06d", seg, i))
	}
	raw, err := json.Marshal(Event{Seq: seq, Event: EventInvalidate, Paths: list, Tree: bfs060Tree})
	if err != nil {
		t.Fatalf("marshal the legal frame: %v", err)
	}
	return string(raw)
}

// bfs062Start wires the stub, a client and a running invalidator, and hands back
// the first served stream. It is the BFS-060 wiring with this row's declared
// bound as the only addition.
func bfs062Start(t *testing.T, declaredBound int64) (*bfs060Fixture, *Invalidator, *collectDrops, *bfs060Stream) {
	t.Helper()
	fx := newBFS060Fixture(t)
	fx.heartbeatMS = 40 // a short declared heartbeat keeps the idle rule from deciding anything here
	fx.maxEventBytes = declaredBound
	c := fx.client(t)
	rec := &collectDrops{}
	inv := NewInvalidator(c, InvalidateOptions{
		Mode: "auto", PollInterval: 20 * time.Millisecond,
		IdleTimeout: 5 * time.Second, OnDrop: rec.drop, OnResync: rec.resync,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = inv.Run(ctx) }()

	if declaredBound+eventFrameSlack > ClientFrameCeiling {
		// The declaration is one this mount cannot hold (the relation is
		// `cap ≥ declared + slack`), so the relation refuses the channel BEFORE a
		// stream is opened and the arm's own assertion below measures that no
		// `watch` request was ever made.
		return fx, inv, rec, nil
	}
	return fx, inv, rec, fx.expectStream(t, 3*time.Second)
}

// TestBFS062ALegalOversizedFrameIsNamedCountedAndNotRetried is the row's RED and
// its GREEN in one cell.
//
// RED (the tree as filed): the frame is legal, the server sends it, the reader
// refuses it, `classifyTransport` calls it a transport fault and the reconnect
// loop runs — the arm fails with the reconnect count and the transport-flavoured
// `last_failure`.
//
// GREEN (this tree): the same frame is a NAMED cause
// (`event_frame_over_limit`), COUNTED (`frame_limit.over_limit_total` = 1), and
// NON-RETRYABLE (no second stream is ever served, and `reconnects_total` stays
// 0) — and the mount stays usable, on the declared poll.
func TestBFS062ALegalOversizedFrameIsNamedCountedAndNotRetried(t *testing.T) {
	fx, inv, _, stream := bfs062Start(t, bfs062DeclaredDefault)

	// The server's permitted maximum: 4096 paths of the longest path Linux will
	// hand a walk. This is a LEGAL frame — nothing about it exceeds a declared
	// limit — which is the whole point of the row.
	frame := bfs062LegalFrame(t, bfs062MaxPathsPerEvent, bfs062PathMax-1, 1)
	if len(frame) <= ClientFrameCeiling {
		t.Fatalf("the frame measured %d bytes, which this consumer's ceiling would hold: the arm is vacuous", len(frame))
	}
	t.Logf("legal frame: %d paths × %d bytes = %d bytes on one line (%.1f MiB), against the declared %d-byte bound",
		bfs062MaxPathsPerEvent, bfs062PathMax-1, len(frame), float64(len(frame))/(1<<20), bfs062DeclaredDefault)

	stream.line(frame)

	// The fork in the road: the named condition, or the reconnect that must never
	// happen on a deterministic input.
	outcome := ""
	if !waitFor(3*time.Second, func() bool {
		st := inv.State()
		switch {
		case st.FrameLimit != nil && st.FrameLimit.OverLimitTotal > 0:
			outcome = "named"
		case st.Reconnects > 0:
			outcome = "reconnect"
		default:
			return false
		}
		return true
	}) {
		st := inv.State()
		t.Fatalf("a legal %d-byte frame produced NEITHER a named condition nor a reconnect: %+v (streams served=%d)", len(frame), st, fx.servedCount())
	}
	if outcome != "named" {
		st := inv.State()
		t.Fatalf("A LEGAL SERVER FRAME PUT THE MOUNT INTO THE RECONNECT LOOP: %d-byte frame (4096 paths of PATH_MAX), reconnects_total=%d, last_failure=%q, streams served=%d. "+
			"A reconnect on a deterministic input is an outage; the condition must be NAMED, COUNTED and NON-RETRYABLE.",
			len(frame), st.Reconnects, st.LastFailure, fx.servedCount())
	}

	st := inv.State()
	// (1) NAMED: the failure reads as the frame condition, never as a transport
	//     fault. This is the assertion the filed tree fails: its last_failure was
	//     `cause=unreachable_connect ... bufio.Scanner: token too long`.
	if !strings.Contains(st.LastFailure, string(CauseEventFrameOverLimit)) {
		t.Fatalf("the failure does not name the frame condition (BFS-062): %q", st.LastFailure)
	}
	for _, transport := range []string{"unreachable_connect", "unreachable_reset", "unreachable_deadline"} {
		if strings.Contains(st.LastFailure, transport) {
			t.Fatalf("a legal server frame is reported as a TRANSPORT fault (%s), which is what manufactures the reconnect loop: %q", transport, st.LastFailure)
		}
	}
	if st.FrameLimit == nil {
		t.Fatalf("the record carries no frame_limit group, so the bound in force is not reportable: %+v", st)
	}
	if !strings.Contains(st.FrameLimit.Detail, "per-line cap") {
		t.Fatalf("the condition does not name the reader that refused it: %q", st.FrameLimit.Detail)
	}
	// (2) COUNTED, once.
	if st.FrameLimit.OverLimitTotal != 1 {
		t.Fatalf("frame_limit.over_limit_total = %d, want exactly 1 (counted at the decision, never per attempt)", st.FrameLimit.OverLimitTotal)
	}
	if !st.FrameLimit.OverLimit {
		t.Fatalf("the condition is not reported as a STATE: %+v", st.FrameLimit)
	}
	// (3) NON-RETRYABLE: no reconnect, and no second stream — measured over a
	//     window longer than two backoffs (500 ms then 1 s), so a loop would have
	//     re-served by now.
	time.Sleep(1500 * time.Millisecond)
	st = inv.State()
	if st.Reconnects != 0 {
		t.Fatalf("reconnects_total = %d after %d ms: an over-limit frame was RETRIED — the same bytes arrive every time, so this is an outage, not a retry", st.Reconnects, 1500)
	}
	if served := fx.servedCount(); served != 1 {
		t.Fatalf("`watch` streams served = %d, want 1: a second stream is the reconnect loop by another name", served)
	}
	// (4) The mount is not dead: the declared next mechanism carries the channel.
	if !waitFor(3*time.Second, func() bool {
		s := inv.State()
		return s.Mode == ModePoll && s.Mechanism == MechanismEvents && s.Available
	}) {
		t.Fatalf("the frame condition did not degrade the channel to the declared poll: %+v", inv.State())
	}
	if got := inv.State().Reason; !strings.Contains(got, "event frame over the declared bound") {
		t.Fatalf("the reason does not name the condition that moved the mechanism: %q", got)
	}
	t.Logf("named + counted + non-retryable: over_limit_total=1, reconnects_total=0, streams served=1, reason=%q, detail=%q",
		inv.State().Reason, inv.State().FrameLimit.Detail)
}

// TestBFS062TheReaderIsSizedFromTheDeclaration is the NEGOTIATION cell: the
// reader's cap is the number the server published (plus the framing slack), so
// the same frame is ACCEPTED when the declaration says it may be that large and
// refused when it does not. Neither side assumes the other's limit.
func TestBFS062TheReaderIsSizedFromTheDeclaration(t *testing.T) {
	t.Run("a frame the declaration admits is read and applied", func(t *testing.T) {
		// The server declares the largest bound its own knob table permits
		// (8 MiB), so a 4 MiB frame is legal and must be read.
		fx, inv, rec, stream := bfs062Start(t, bfs062DeclaredMaximum)
		frame := bfs062LegalFrame(t, 1024, bfs062PathMax-1, 1)
		if len(frame) < 4<<20 {
			t.Fatalf("the frame measured %d bytes, want ~4 MiB so the arm proves a large frame is read", len(frame))
		}
		stream.line(frame)
		if !waitFor(5*time.Second, func() bool { return rec.dropped(fmt.Sprintf("%s%06d", strings.Repeat("p", bfs062PathMax-1), 0)) }) {
			t.Fatalf("a frame inside the declared bound was NOT read and applied: %s state=%+v", rec.summary(), inv.State())
		}
		st := inv.State()
		if st.FrameLimit == nil || st.FrameLimit.OverLimitTotal != 0 {
			t.Fatalf("an in-bound frame was counted as over-limit: %+v", st.FrameLimit)
		}
		if st.FrameLimit.DeclaredBytes == nil || *st.FrameLimit.DeclaredBytes != bfs062DeclaredMaximum {
			t.Fatalf("the declared bound is not reported: %+v", st.FrameLimit)
		}
		if st.FrameLimit.ReaderBytes != bfs062DeclaredMaximum+eventFrameSlack {
			t.Fatalf("reader_bytes = %d, want the declared %d + %d slack: the reader must be sized FROM the declaration", st.FrameLimit.ReaderBytes, bfs062DeclaredMaximum, eventFrameSlack)
		}
		// The derived ceiling is what makes the LARGEST legal declaration
		// holdable: declared + slack is exactly the ceiling, and the ceiling must
		// cover it.
		if ClientFrameCeiling != bfs062DeclaredMaximum+eventFrameSlack {
			t.Fatalf("the consumer's ceiling is %d, want the largest declaration the server may make (%d) plus the slack (%d) — a ceiling that equals the declaration refuses a conforming server", ClientFrameCeiling, bfs062DeclaredMaximum, eventFrameSlack)
		}
		if fx.servedCount() != 1 {
			t.Fatalf("streams served = %d, want 1: an accepted frame must not cycle the channel", fx.servedCount())
		}
	})

	t.Run("the same frame is read or refused by the DECLARATION, not by a constant", func(t *testing.T) {
		// One frame, ~200 KiB (50 paths of PATH_MAX), inside the SERVER's own
		// limits in both cases. The only thing that changes between the two arms
		// is the declaration, and the outcome follows it — which is what
		// "neither side assumes the other's limit" has to mean in practice.
		frame := bfs062LegalFrame(t, 50, bfs062PathMax-1, 1)

		// (a) declared 1 MiB (the asserted default): the frame is inside the
		//     declaration, so it is read and applied.
		_, invA, recA, streamA := bfs062Start(t, bfs062DeclaredDefault)
		first := fmt.Sprintf("%s%06d", strings.Repeat("p", bfs062PathMax-1), 0)
		streamA.line(frame)
		if !waitFor(5*time.Second, func() bool { return recA.dropped(first) }) {
			t.Fatalf("a frame INSIDE the declared bound was not read: %s state=%+v", recA.summary(), invA.State())
		}
		if st := invA.State(); st.FrameLimit.OverLimitTotal != 0 {
			t.Fatalf("a frame inside the declared bound was counted as over-limit: %+v", st.FrameLimit)
		}

		// (b) declared 64 KiB (the surface's declared minimum): the SAME bytes
		//     exceed what the declaration + slack can hold, so they are refused
		//     once, counted, and never retried.
		fxB, invB, recB, streamB := bfs062Start(t, 64<<10)
		streamB.line(frame)
		if !waitFor(3*time.Second, func() bool {
			st := invB.State()
			return st.FrameLimit != nil && st.FrameLimit.OverLimitTotal > 0
		}) {
			t.Fatalf("the same frame under a smaller declaration produced no named condition: %+v", invB.State())
		}
		if recB.dropped(first) {
			t.Fatalf("the over-limit frame was APPLIED (%s): a frame the reader refused cannot have been decoded", recB.summary())
		}
		time.Sleep(1200 * time.Millisecond)
		st := invB.State()
		if st.Reconnects != 0 || fxB.servedCount() != 1 {
			t.Fatalf("the refusal was retried: reconnects_total=%d streams=%d — the same bytes arrive every time", st.Reconnects, fxB.servedCount())
		}
		if st.FrameLimit.ReaderBytes != 64<<10+eventFrameSlack {
			t.Fatalf("reader_bytes = %d, want the declared %d + %d slack", st.FrameLimit.ReaderBytes, 64<<10, eventFrameSlack)
		}
	})

	t.Run("an unholdable declaration refuses the channel before a stream is opened", func(t *testing.T) {
		// A declaration above this consumer's ceiling: the relation (client cap ≥
		// declared + slack) is unsatisfiable, so the mount must DECLARE the
		// mismatch rather than open a stream it cannot read.
		fx, inv, _, _ := bfs062Start(t, bfs062DeclaredMaximum+(1<<20))
		if !waitFor(3*time.Second, func() bool {
			st := inv.State()
			return st.FrameLimit != nil && st.FrameLimit.OverLimitTotal > 0
		}) {
			t.Fatalf("an unholdable declaration produced no named condition: %+v", inv.State())
		}
		st := inv.State()
		if !strings.Contains(st.LastFailure, string(CauseEventFrameOverLimit)) {
			t.Fatalf("the mismatch is not named as the frame condition: %q", st.LastFailure)
		}
		if !strings.Contains(st.FrameLimit.Detail, "cannot hold") {
			t.Fatalf("the mismatch does not say the declaration is unholdable: %q", st.FrameLimit.Detail)
		}
		if served, probes := fx.counts(); served != 0 {
			t.Fatalf("streams served = %d (probes %d): the relation must be checked BEFORE a stream is opened — discovering the mismatch frame by frame is how this row's defect worked", served, probes)
		}
		if st.Reconnects != 0 {
			t.Fatalf("reconnects_total = %d: an unholdable declaration is deterministic and must never be retried", st.Reconnects)
		}
		if st.Mode != ModePoll {
			t.Fatalf("mode = %q, want the declared poll fallback: %+v", st.Mode, st)
		}
	})
}

// TestBFS062TheClassificationDoesNotFireOnOrdinaryFrames is the non-vacuity
// control for the two arms above: a channel whose frames are legal and small must
// keep working with nothing counted, so an implementation that refused every
// frame would fail here even though it would pass the arms above.
func TestBFS062TheClassificationDoesNotFireOnOrdinaryFrames(t *testing.T) {
	_, inv, rec, stream := bfs062Start(t, bfs062DeclaredDefault)
	stream.line(bfs060Invalidate(1, "src/main.go"))
	stream.line(bfs060Heartbeat(2))
	if !waitFor(3*time.Second, func() bool { return rec.dropped("src/main.go") }) {
		t.Fatalf("an ordinary frame was not delivered: %s state=%+v", rec.summary(), inv.State())
	}
	time.Sleep(300 * time.Millisecond)
	st := inv.State()
	if st.FrameLimit != nil && st.FrameLimit.OverLimitTotal != 0 {
		t.Fatalf("an ordinary frame was counted as over the bound: %+v", st.FrameLimit)
	}
	if st.Reconnects != 0 {
		t.Fatalf("an ordinary frame caused a reconnect: %+v", st)
	}
	if st.Mode != ModePush || st.Mechanism != MechanismWatch {
		t.Fatalf("the push channel was not left in force for a healthy frame: %+v", st)
	}
}
