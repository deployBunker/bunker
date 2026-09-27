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
// publishing it) lives in internal/server/webdav (events_bfs062_test.go).
//
// The row: the server's event frame was bounded by a path COUNT (4096) while
// this consumer's reader was bounded by BYTES (8 MiB). 4096 paths of PATH_MAX is
// ~16.8 MiB of JSON on ONE line, so a frame the server was entitled to send
// exceeded the reader, and `bufio.ErrTooLong` was classified as a TRANSPORT
// fault — a reconnect loop on a DETERMINISTIC input, which is an outage.
//
// THIS FILE IS DELIBERATELY WRITTEN AGAINST THE FILED TREE. It asserts only on
// surface that existed before this row (the state's mode/mechanism/reason/
// reconnects/last_failure, the drop and resync callbacks, the served-stream
// count) and spells the new cause as a string literal, so the RED is reproducible
// by swapping in the base commit's product files — see
// docs/evidence/BFS-062-arms.sh, mode `unfixed`. The field-level assertions that
// can only exist on the fixed tree (frame_limit.*, the derived reader cap, the
// unholdable-declaration refusal) live in invalidate_bfs062_rule_test.go, which
// that mode sets aside, exactly as BFS-060 did for its own reporting arms.
// ---------------------------------------------------------------------------

// The producer's own declared limits, quoted with their provenance rather than
// invented: a frame built outside them would be measuring this test's arithmetic
// instead of the contract. The GREEN-only rule file pins them against the
// product's own constants.
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
	// permits it to declare (invalidation.MaxPushMaxEventBytes).
	bfs062DeclaredMaximum = 8 << 20
	// bfs062FrameSlack is the framing margin the relation adds to the declared
	// bound (the product's eventFrameSlack).
	bfs062FrameSlack = 64 << 10
	// bfs062CauseEventFrameOverLimit is the NAMED cause the fixed tree reports
	// for this condition (the product's CauseEventFrameOverLimit). It is a
	// literal here so this file compiles against the filed tree, where no such
	// cause exists — and it is NOT a spelling this test invented: the rule file
	// asserts the product's cause constant equals it.
	bfs062CauseEventFrameOverLimit = "event_frame_over_limit"
	// bfs062ReasonPrefix is the sentence the record carries when the frame
	// condition moved the mechanism to the declared poll.
	bfs062ReasonPrefix = "event frame over the declared bound"
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

// bfs062FirstPath is the first path of a frame built by bfs062LegalFrame, so an
// arm can assert on what was (or was not) delivered.
func bfs062FirstPath() string { return fmt.Sprintf("%s%06d", strings.Repeat("p", bfs062PathMax-1), 0) }

// bfs062Start wires the stub, a client and a running invalidator, and hands back
// the first served stream. It is the BFS-060 wiring with this row's declared
// bound as the only addition: the bound travels in the capability document the
// stub serves, exactly as a deployment declares it.
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
	return fx, inv, rec, fx.expectStream(t, 3*time.Second)
}

// bfs062Named reports whether the record carries the frame condition, read out
// of surface that exists on the FILED tree too: the reason sentence the
// degradation writes. The count and the state live in frame_limit and are
// asserted by the GREEN-only rule file.
func bfs062Named(inv *Invalidator) bool {
	return strings.Contains(inv.State().Reason, bfs062ReasonPrefix)
}

// TestBFS062ALegalOversizedFrameIsNamedCountedAndNotRetried is the row's RED and
// its GREEN in one cell.
//
// RED (the tree as filed): the frame is legal, the server sends it, the reader
// refuses it, `classifyTransport` calls it a transport fault and the reconnect
// loop runs — this arm fails with the counted reconnect and the transport-
// flavoured `last_failure`.
//
// GREEN (this tree): the same frame is a NAMED cause (event_frame_over_limit),
// COUNTED (frame_limit.over_limit_total = 1, rule file), and NON-RETRYABLE (no
// second stream is ever served and reconnects_total stays 0) — and the mount
// stays usable, on the declared poll.
func TestBFS062ALegalOversizedFrameIsNamedCountedAndNotRetried(t *testing.T) {
	fx, inv, _, stream := bfs062Start(t, bfs062DeclaredDefault)

	// The server's permitted maximum: 4096 paths of the longest path Linux will
	// hand a walk. Nothing about it exceeds a declared limit — which is the whole
	// point of the row.
	frame := bfs062LegalFrame(t, bfs062MaxPathsPerEvent, bfs062PathMax-1, 1)
	if len(frame) <= bfs062DeclaredMaximum {
		t.Fatalf("the frame measured %d bytes, which is not the worst case the row is about", len(frame))
	}
	t.Logf("legal frame: %d paths × %d bytes = %d bytes on one line (%.1f MiB), against the declared %d-byte bound",
		bfs062MaxPathsPerEvent, bfs062PathMax-1, len(frame), float64(len(frame))/(1<<20), bfs062DeclaredDefault)

	stream.line(frame)

	// The fork in the road: the named condition, or the reconnect that must never
	// happen on a deterministic input.
	outcome := ""
	if !waitFor(3*time.Second, func() bool {
		switch {
		case bfs062Named(inv):
			outcome = "named"
		case inv.State().Reconnects > 0:
			outcome = "reconnect"
		default:
			return false
		}
		return true
	}) {
		t.Fatalf("a legal %d-byte frame produced NEITHER a named condition nor a reconnect: %+v (streams served=%d)",
			len(frame), inv.State(), fx.servedCount())
	}
	if outcome != "named" {
		st := inv.State()
		t.Fatalf("A LEGAL SERVER FRAME PUT THE MOUNT INTO THE RECONNECT LOOP: a %d-byte frame (%d paths of PATH_MAX), reconnects_total=%d, last_failure=%q, streams served=%d. "+
			"A reconnect on a deterministic input is an outage; the condition must be NAMED, COUNTED and NON-RETRYABLE.",
			len(frame), bfs062MaxPathsPerEvent, st.Reconnects, st.LastFailure, fx.servedCount())
	}

	st := inv.State()
	// (1) NAMED: the failure reads as the frame condition, never as a transport
	//     fault. This is the assertion the filed tree fails: its last_failure was
	//     `cause=unreachable_connect ... bufio.Scanner: token too long`.
	if !strings.Contains(st.LastFailure, bfs062CauseEventFrameOverLimit) {
		t.Fatalf("the failure does not name the frame condition (BFS-062): %q", st.LastFailure)
	}
	for _, transport := range []string{"unreachable_connect", "unreachable_reset", "unreachable_deadline"} {
		if strings.Contains(st.LastFailure, transport) {
			t.Fatalf("a legal server frame is reported as a TRANSPORT fault (%s), which is what manufactures the reconnect loop: %q", transport, st.LastFailure)
		}
	}
	// (2) NON-RETRYABLE: no reconnect, and no second stream — measured over a
	//     window longer than two backoffs (500 ms then 1 s), so a loop would have
	//     re-served by now.
	time.Sleep(1500 * time.Millisecond)
	st = inv.State()
	if st.Reconnects != 0 {
		t.Fatalf("reconnects_total = %d after 1500 ms: an over-limit frame was RETRIED — the same bytes arrive every time, so this is an outage, not a retry", st.Reconnects)
	}
	if served := fx.servedCount(); served != 1 {
		t.Fatalf("`watch` streams served = %d, want 1: a second stream is the reconnect loop by another name", served)
	}
	// (3) The mount is not dead: the declared next mechanism carries the channel.
	if !waitFor(3*time.Second, func() bool {
		s := inv.State()
		return s.Mode == ModePoll && s.Mechanism == MechanismEvents && s.Available
	}) {
		t.Fatalf("the frame condition did not degrade the channel to the declared poll: %+v", inv.State())
	}
	if got := inv.State().Reason; !strings.Contains(got, bfs062ReasonPrefix) {
		t.Fatalf("the reason does not name the condition that moved the mechanism: %q", got)
	}
	if got := inv.State().Reason; !strings.Contains(got, "the same frame would be sent again") {
		t.Fatalf("the reason does not say why it is not retried: %q", got)
	}
	t.Logf("named + non-retried: reconnects_total=0, streams served=1, reason=%q", inv.State().Reason)
}

// TestBFS062TheReaderFollowsTheDeclaration is the NEGOTIATION cell: the reader's
// cap is the number the server published (plus the framing slack), so the same
// bytes are ACCEPTED under a declaration that admits them and REFUSED under one
// that does not. On the filed tree the second half fails: its reader is a
// constant, so the same frame is read whatever the server declared.
func TestBFS062TheReaderFollowsTheDeclaration(t *testing.T) {
	// One frame, ~200 KiB (50 paths of PATH_MAX), inside the SERVER's own limits
	// in both cases. The only thing that changes between the two arms is the
	// declaration.
	frame := bfs062LegalFrame(t, 50, bfs062PathMax-1, 1)

	// (a) declared 1 MiB (the asserted default): the frame is inside the
	//     declaration, so it is read and applied.
	_, invA, recA, streamA := bfs062Start(t, bfs062DeclaredDefault)
	streamA.line(frame)
	if !waitFor(5*time.Second, func() bool { return recA.dropped(bfs062FirstPath()) }) {
		t.Fatalf("a frame INSIDE the declared bound was not read: %s state=%+v", recA.summary(), invA.State())
	}

	// (b) declared 64 KiB (the surface's declared minimum): the SAME bytes exceed
	//     what the declaration + slack can hold, so they are refused once.
	fxB, invB, recB, streamB := bfs062Start(t, 64<<10)
	streamB.line(frame)
	if !waitFor(3*time.Second, func() bool { return bfs062Named(invB) }) {
		t.Fatalf("the same frame under a smaller declaration produced no named condition: %+v", invB.State())
	}
	if recB.dropped(bfs062FirstPath()) {
		t.Fatalf("the over-limit frame was APPLIED (%s): a frame the reader refused cannot have been decoded", recB.summary())
	}
	time.Sleep(1200 * time.Millisecond)
	st := invB.State()
	if st.Reconnects != 0 || fxB.servedCount() != 1 {
		t.Fatalf("the refusal was retried: reconnects_total=%d streams served=%d — the same bytes arrive every time", st.Reconnects, fxB.servedCount())
	}
}

// TestBFS062TheClassificationDoesNotFireOnOrdinaryFrames is the non-vacuity
// control for the arms above: a channel whose frames are legal and small must
// keep working with nothing counted, so an implementation that refused every
// frame would fail here even though it would pass the arms above. It passes on
// the filed tree too, which is what attributes the two failures above to this
// row's defect rather than to the harness.
func TestBFS062TheClassificationDoesNotFireOnOrdinaryFrames(t *testing.T) {
	fx, inv, rec, stream := bfs062Start(t, bfs062DeclaredDefault)
	stream.line(bfs060Invalidate(1, "src/main.go"))
	stream.line(bfs060Heartbeat(2))
	if !waitFor(3*time.Second, func() bool { return rec.dropped("src/main.go") }) {
		t.Fatalf("an ordinary frame was not delivered: %s state=%+v", rec.summary(), inv.State())
	}
	time.Sleep(300 * time.Millisecond)
	st := inv.State()
	if st.Reconnects != 0 {
		t.Fatalf("an ordinary frame caused a reconnect: %+v", st)
	}
	if st.Mode != ModePush || st.Mechanism != MechanismWatch {
		t.Fatalf("the push channel was not left in force for a healthy frame: %+v", st)
	}
	if fx.servedCount() != 1 {
		t.Fatalf("streams served = %d, want 1: a healthy frame must not cycle the channel", fx.servedCount())
	}
}
