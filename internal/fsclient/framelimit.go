package fsclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// BFS-062 — the frame-size relation between the pushed channel's PRODUCER and
// this consumer, and the one thing that must never happen to it: a legal server
// frame becoming a reconnect loop.
//
// The defect this file closes is a CONTRACT defect between two of our own
// components, not a bug in either alone: the server's event frame was bounded by
// a path COUNT (4096) while this consumer's reader was bounded by BYTES
// (8 MiB), and 4096 paths of PATH_MAX is ~16.8 MiB of JSON on one line. A frame
// the server was entitled to send therefore exceeded the reader, and the failure
// surfaced as `bufio.ErrTooLong`, which classifyTransport read as a TRANSPORT
// fault — so the client reconnected, read the same bytes, and reconnected again:
// a reconnect loop on a DETERMINISTIC input, which is an outage.
//
// The fix is negotiated rather than guessed (the additive `max_event_bytes` of
// SPEC-push-channel §7.2): the SERVER measures the serialized frame and emits
// `overflow` instead of an over-bound list (webdav/events.go), DECLARES the
// bound in its capability document, and this consumer SIZES ITS READER FROM THE
// DECLARATION — so neither side assumes the other's limit. Whatever the outcome
// of that negotiation, an over-limit frame here is NAMED (CauseEventFrameOverLimit
// and a reader-too-small detail naming both numbers), COUNTED (frame_over_limit_total)
// and NON-RETRYABLE (it takes the declared poll once; it is never fed to
// reconnectLoop).
// ---------------------------------------------------------------------------

const (
	// ClientFrameCeiling is the largest per-line read cap this consumer will
	// allocate for the pushed channel's frames. It is not a guess about the
	// server: it is DERIVED from the largest per-frame bound the surface is
	// allowed to declare (the server's own knob ceiling,
	// server.invalidation.push.max_event_bytes max = 8 MiB) plus the framing
	// slack the relation requires. A client whose ceiling merely EQUALLED the
	// maximum declaration could not hold it — the slack would push it over — and
	// would refuse a conforming server; a client with a larger ceiling would be
	// reserving memory for a frame no deployment can legally send. It is a memory
	// commitment in a FUSE mount, so it is bounded by the contract rather than by
	// "large enough". A declaration above it is a named mismatch — never a
	// silently clamped reader, because a reader smaller than the declared bound
	// is exactly defect H-4.
	ClientFrameCeiling = (8 << 20) + eventFrameSlack
	// eventFrameSlack is the framing margin the declared bound is read with: the
	// relation is "the per-line cap MUST be ≥ max_event_bytes + slack", and the
	// slack is what covers the line terminator and a growing reader's own
	// bookkeeping rather than the frame's payload.
	eventFrameSlack = 64 << 10
)

// FrameLimitState is the pushed channel's frame accounting: the bound DECLARED
// by the server, the per-line cap this consumer actually allocated, the ceiling
// it will never exceed, and whether a frame ever crossed the bound. It exists
// because a bound the owner cannot see is not a bound (PRD §2.8) — and because
// "my reader was too small for a legal frame" must be readable as that, rather
// than as a failing network.
type FrameLimitState struct {
	// DeclaredBytes is what the server published in `extensions.watch`
	// (`max_event_bytes`). Absent when the document published none — a peer that
	// predates the field — which DeclaredReason says in words rather than
	// leaving a zero that could be read as "a bound of zero".
	DeclaredBytes *int64 `json:"declared_bytes,omitempty"`
	// DeclaredReason is why there is no declared bound, when there is none.
	DeclaredReason string `json:"declared_reason,omitempty"`
	// ReaderBytes is the per-line cap the reader was sized to, which is the
	// declaration plus the framing slack whenever a holdable bound was declared.
	ReaderBytes int64 `json:"reader_bytes"`
	// CeilingBytes is the largest cap this consumer will allocate.
	CeilingBytes int64 `json:"ceiling_bytes"`
	// OverLimit is the CONDITION as a state, not only as a count: an event frame
	// crossed the bound (or the declared bound could not be held), so the pushed
	// form was declared unusable and the poll took over.
	OverLimit bool `json:"over_limit"`
	// OverLimitTotal counts the frames that crossed the bound.
	OverLimitTotal int64 `json:"over_limit_total"`
	// Detail names the numbers the verdict was taken from.
	Detail string `json:"detail,omitempty"`
}

// declaredMaxEventBytes is the per-frame byte bound the server DECLARED, or 0
// when it declared none.
func (i *Invalidator) declaredMaxEventBytes() int64 {
	if i.client == nil {
		return 0
	}
	return i.client.Capabilities().MaxEventBytes()
}

// frameLimitMismatch reports the declaration this consumer cannot hold, as an
// OpError, or nil when the declaration is holdable (or absent).
//
// It is checked BEFORE the stream is opened, because the mismatch is a fact
// about the two documents and not about any frame: opening a stream whose frames
// this client cannot hold and discovering it one frame later is how the count/
// byte mismatch became a loop in the first place.
func (i *Invalidator) frameLimitMismatch() *OpError {
	declared := i.declaredMaxEventBytes()
	if declared <= 0 || declared+eventFrameSlack <= ClientFrameCeiling {
		return nil
	}
	return &OpError{
		Op:     "watch",
		Errno:  ErrnoEFBIG,
		Cause:  CauseEventFrameOverLimit,
		Detail: fmt.Sprintf("the server declares a %d-byte per-frame bound, which this mount cannot hold: the per-line cap would have to be at least %d bytes (declared + %d slack) and this client's ceiling is %d (SPEC-push-channel §7.2 requires a client that cannot size its reader to the declared bound to DECLARE the mismatch rather than reconnect-loop on it)", declared, declared+eventFrameSlack, eventFrameSlack, ClientFrameCeiling),
	}
}

// readCap is the per-line cap the reader is sized to for one attempt.
//
// Two cases, and neither is a guess:
//
//   - the server declared a holdable bound: the cap is DERIVED from it
//     (declaration + slack), so this consumer's capacity is the published
//     contract rather than a constant that happens to be large enough;
//   - the server declared none (a peer that predates the field): the cap is this
//     client's own ceiling. There is no declaration to size to, and inventing a
//     bound for another process is the assumption this row exists to remove —
//     the classification below covers that case instead of the relation.
func (i *Invalidator) readCap() int {
	declared := i.declaredMaxEventBytes()
	if declared > 0 && declared+eventFrameSlack <= ClientFrameCeiling {
		return int(declared + eventFrameSlack)
	}
	return ClientFrameCeiling
}

// classifyStreamRead classifies a read failure off the pushed stream.
//
// `bufio.ErrTooLong` is NOT a transport fault and this is the whole of the
// client half of BFS-062: the connection is healthy, the frame is longer than
// this reader's cap, and the same bytes arrive on every attempt — so a transport
// classification turns one deterministic input into an endless reconnect. It is
// named instead, with the numbers that decided it.
func (i *Invalidator) classifyStreamRead(err error) *OpError {
	if errors.Is(err, bufio.ErrTooLong) {
		declared := i.declaredMaxEventBytes()
		detail := fmt.Sprintf("an event frame exceeded this mount's %d-byte per-line cap", i.readCap())
		if declared > 0 {
			detail += fmt.Sprintf(" (the server declared a %d-byte per-frame bound, read with %d bytes of framing slack)", declared, eventFrameSlack)
		} else {
			detail += " (the server's document publishes no max_event_bytes, so this mount used its own ceiling)"
		}
		detail += ": the frame is rejected once and reported, never retried — the same bytes would arrive again"
		return &OpError{
			Op:     "watch",
			Errno:  ErrnoEFBIG,
			Cause:  CauseEventFrameOverLimit,
			Detail: detail,
			Err:    err,
		}
	}
	e := classifyTransport(err)
	e.Op = "watch"
	return e
}

// frameLimitDegradation reports the one condition that must NOT be retried. Both
// entry points — the first attempt and the retry after a fault — ask this one
// predicate, so the two can never drift into different sets (the same rule
// isDeclaredDegradation was written for).
func frameLimitDegradation(err *OpError) bool {
	return err != nil && err.Cause == CauseEventFrameOverLimit
}

// frameLimitToPoll applies BFS-062's declared degradation: the pushed form's
// frames are not holdable by this consumer, so the mechanism that does not read
// one frame per line takes over.
//
// The counting and the STATE are recorded here and the mode change is made
// BEFORE the first poll answers (O-1), so the record never claims a channel it
// does not have. It deliberately does NOT go through reconnectLoop: the input is
// deterministic and retrying it is the outage this cause exists to prevent.
func (i *Invalidator) frameLimitToPoll(ctx context.Context, err *OpError) error {
	i.countFrameOverLimit()
	detail := ""
	if err != nil {
		detail = err.Detail
		if detail == "" {
			detail = err.Error()
		}
	}
	i.mu.Lock()
	i.frameOverLimit = true
	i.frameLimitDetail = detail
	i.mu.Unlock()
	i.setMode(ModePoll, "event frame over the declared bound: "+detail+" — declared poll fallback, no reconnect (the same frame would be sent again)")
	return i.pollLoop(ctx)
}
