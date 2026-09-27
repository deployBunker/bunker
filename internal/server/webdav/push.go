package webdav

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/deployBunker/bunker/internal/invalidation"
)

// ---------------------------------------------------------------------------
// BFS-036 — THE PUSH CHANNEL: E-6's `watch` op served as a STREAM.
//
// DESIGN AUTHORITY: docs/prd/SPEC-push-channel.md (BFS-041), which chose the
// stream over a bounded long-poll and argued it from this repository's own
// measurements. This file implements that wire form; it does not re-decide it.
// The clauses it implements, by number:
//
//	§1.4 C-1  the channel sits AHEAD of the chi router, so no request deadline
//	          governs it. Nothing here installs a socket deadline on the /dav
//	          listener (server.go:469-497 sets none) and nothing here is an RPC.
//	§2.1 W-1  the response is incremental or it is nothing: headers, then a
//	          line, each written and FLUSHED individually. `net/http` buffers a
//	          response in a 2 KiB writer, so a handler that never flushes can
//	          hold every heartbeat and the client's idle rule would declare a
//	          healthy channel dead.
//	§2.2 W-2  the channel is committed on subscribe with its first line, so
//	          establishing it never waits for an event. A quiet tree is the
//	          normal case, and binding on a quiet tree must not hang.
//	§2.3 W-3  the subscription's `paths` is a filter whose failure mode is
//	          asymmetric — over-reporting is a safe no-op, under-reporting is a
//	          silent-corruption machine. This build applies it to NO line and
//	          SAYS SO (PushSubscriptionWholeTree, published as
//	          extensions.watch.subscription and echoed per response), because
//	          R-5 makes per-subscriber filtering itself unsafe: every line that
//	          advances the seq is written to every attached subscriber, and a
//	          line withheld from one subscriber is a cursor gap it cannot see.
//	§3.1 R-1  the cursor advances on every line, heartbeats included.
//	§3.2      the four resume cases, answered from the one ledger the poll also
//	          uses (resumeLocked, events.go) so the two mechanisms cannot
//	          disagree about what has happened.
//	§3.3 R-3  no answer may stand in for a gap.
//	       R-4  an `overflow` may never be emitted at a seq the presenting
//	          client's own duplicate rule can discard.
//	§3.4 R-5  one counter, one cadence, broadcast: heartbeats consume a seq and
//	          are JOURNALED (so a reconnect can tell heartbeats from loss) and
//	          every line that advances the counter is delivered to every
//	          attached subscriber.
//	§4   B-1  `heartbeat_ms` is the maximum silence; the bound is declared and
//	          measured, and a quiet tree is proved alive by it.
//	§5   B-4  no request is held open on a client that will never read: two
//	          detectors (the request context, and a write deadline on every
//	          line) and a release that frees the buffer, the slot and the
//	          goroutine. B-6: on a transport whose response writer cannot carry
//	          a write deadline (h3) that fact is REPORTED rather than assumed.
//	§6   B-7  a per-subscriber buffer bounded in bytes AND events, both declared
//	       B-8  the watcher never blocks on a subscriber: the fan-out appends
//	          to a buffer and does no I/O; the handler goroutine is the writer.
//	       B-9  drop-oldest with ONE counted `overflow` marker per gap run.
//	       B-10 never coalesce, never truncate, never present a partial list as
//	          complete.
//	       B-13 a capacity refusal is RETRYABLE and must not look like a
//	          degradation: 429 + Retry-After, no verdict and no capability
//	          header, so the client takes its reconnect path and keeps push.
//	§7.2 B-15 the frame's byte bound is enforced where every line is assembled
//	          (events.go's ledger push, BFS-062), so this channel inherits it
//	          rather than re-implementing a second, weaker one.
//
// WHAT THIS IS NOT. It is an upgrade and never a requirement. The poll form
// (`X-Bunker-Op: events`) is untouched; a stock client that sets no
// X-Bunker-Op sees byte-identical behaviour; and a target without a watcher
// still REFUSES this op with the same 501 capability_unavailable it answered
// before this file existed — so a deployment in which nothing can produce
// events serves no stream at all (a stream with no source is the
// healthy-looking channel that carries nothing, §1.2 case 1).
// ---------------------------------------------------------------------------

// eventHeartbeat is the third line name of E-6's vocabulary (events.go emits the
// other two). It bounds LIVENESS and never claims an interval was quiet.
const eventHeartbeat = "heartbeat"

// PushSubscriptionWholeTree is W-3's declaration: the scoping decision this
// build makes, published in the capability document and echoed on every stream
// response so a client can see which of the two it is talking to without
// re-reading the document.
const PushSubscriptionWholeTree = "whole_tree"

// PushSubscriptionHeader carries that declaration per response. It is additive:
// an old client ignores an unknown header, and no existing response changes
// shape because of it.
const PushSubscriptionHeader = "X-Bunker-Subscription"

// The release triggers, §5.3's closed set of two. Nothing else may be reported
// as a reason a subscription ended: "dead client" and "client that will not
// drain" are the same detection and the two counters are what separate them.
const (
	// PushReleaseContext — the request context ended: the connection closed,
	// the client went away, or this process is shutting the surface down.
	PushReleaseContext = "ctx"
	// PushReleaseWriteDeadline — a line could not be written inside the
	// declared deadline: a peer that vanished without closing, or a subscriber
	// that will not read.
	PushReleaseWriteDeadline = "write_deadline"
)

// pushCapabilityBlockKey is where the channel's own counters live in the
// capability document. They are DISJOINT from BFS-040 §8.2's `counters` block by
// design (§6.3): those count what the WATCHER lost, these count what the CHANNEL
// dropped, and one number covering both would make "the watcher is healthy and
// the channel is dropping" indistinguishable from the reverse.
const pushCapabilityBlockKey = "push"

// pushSettings are the resolved bounds and cadences the channel obeys, taken
// from the SAME resolved invalidation surface the watcher obeys (BFS-043). A
// zero value is resolved to the DECLARED default rather than to "no bound": a
// bound a missing field can switch off is not a bound (BFS-062's lesson).
type pushSettings struct {
	bufferBytes    int64
	bufferEvents   int
	maxSubscribers int
	writeDeadline  time.Duration
	heartbeat      time.Duration
}

// resolved fills every unset field from the declared surface.
func (s pushSettings) resolved() pushSettings {
	out := s
	if out.bufferBytes <= 0 {
		out.bufferBytes = invalidation.DefaultPushBufferBytes
	}
	if out.bufferEvents <= 0 {
		out.bufferEvents = invalidation.DefaultPushBufferEvents
	}
	if out.maxSubscribers <= 0 {
		out.maxSubscribers = invalidation.DefaultPushMaxSubscribers
	}
	if out.writeDeadline <= 0 {
		out.writeDeadline = time.Duration(invalidation.DefaultPushWriteDeadlineMS) * time.Millisecond
	}
	if out.heartbeat <= 0 {
		out.heartbeat = time.Duration(invalidation.DefaultWatchHeartbeatMS) * time.Millisecond
	}
	return out
}

// pushLine is one buffered line: the exact bytes a consumer's per-line reader
// must hold, encoded ONCE at fan-out so a burst costs one encode per line rather
// than one per subscriber, plus its size so the buffer's byte bound is measured
// on the wire bytes rather than estimated.
type pushLine struct {
	raw  []byte
	size int64
}

// pushCounters is §6.3's set, named there and laid out here because BFS-045's
// record is what reads them. Every one of them is a fact the owner must be able
// to see: a bound nobody can see is not a bound (PRD §2.7), and this release has
// already found a counter that could never move (BFS-032).
type pushCounters struct {
	active          atomic.Int64
	activeMax       atomic.Int64
	drops           atomic.Int64
	gaps            atomic.Int64
	disconnectsCtx  atomic.Int64
	disconnectsDead atomic.Int64
	highWater       atomic.Int64
	// deadlineRefused counts the times this process was asked to establish a
	// stream over a transport whose response writer does not carry a write
	// deadline (B-6, measured: quic-go v0.63.0's http3 response writer does not
	// implement SetWriteDeadline). The dead-client bound on such a transport is
	// the request context and the QUIC idle timer, NOT this line's deadline, and
	// a client that is told otherwise is being told something that is not true.
	deadlineRefused atomic.Int64
}

// pushHub is the per-tree subscriber registry. It lives on the TREE and not on
// the Handler because the cursor does: one ledger, one counter, one cadence, and
// every subscriber on a tree is fed from the same sequence (R-5).
type pushHub struct {
	tree *tree
	cfg  pushSettings

	mu   sync.Mutex
	subs map[*pushSub]struct{}
	// stop ends the heartbeat goroutine. It is closed by Handler.Close, which is
	// the only thing that owns the tree's lifetime.
	stop     chan struct{}
	stopOnce sync.Once

	c pushCounters
}

// pushSub is one attached subscriber: a bounded buffer, the gap it owes, and the
// wake channel its writer blocks on. Nothing here performs I/O, which is what
// makes B-8 true — the watcher's read loop appends to a buffer and never touches
// a socket.
type pushSub struct {
	h         *pushHub
	maxBytes  int64
	maxEvents int

	mu      sync.Mutex
	buf     []pushLine
	bytes   int64
	closed  bool
	pending bool  // a gap marker is owed to this subscriber (§6.2 B-9)
	gapSeq  int64 // the highest seq the marker may name (R-4: above the client's cursor)

	wake chan struct{}
}

// ---------------------------------------------------------------------------
// The tree's side: one hub per tree, fed by the ONE funnel every line passes
// through.
// ---------------------------------------------------------------------------

// pushChannel returns this tree's hub, or nil when no subscriber has ever
// attached. The caller never creates one: a tree nobody subscribes to pays
// nothing.
func (t *tree) pushChannel() *pushHub {
	t.pushMu.Lock()
	defer t.pushMu.Unlock()
	return t.push
}

// broadcast delivers one line to every attached subscriber. It is called from
// the ledger's single push funnel, which is what makes R-5 structural rather
// than a convention: any line that advances the counter — including a heartbeat
// — is delivered to every subscriber, so two clients can never hold two
// different histories of one tree.
func (t *tree) broadcast(ev eventLine) {
	h := t.pushChannel()
	if h == nil {
		return
	}
	h.deliver(ev)
}

// ensurePushChannel creates the hub and starts its heartbeat goroutine. It is
// called on the first attach.
func (t *tree) ensurePushChannel() *pushHub {
	t.pushMu.Lock()
	defer t.pushMu.Unlock()
	if t.push != nil {
		return t.push
	}
	h := &pushHub{
		tree: t,
		cfg:  t.pushCfg.resolved(),
		subs: map[*pushSub]struct{}{},
		stop: make(chan struct{}),
	}
	t.push = h
	go h.heartbeatLoop()
	return h
}

// closePush ends the heartbeat goroutine and releases every subscriber. It is
// called from Handler.Close.
func (t *tree) closePush() {
	t.pushMu.Lock()
	h := t.push
	t.pushMu.Unlock()
	if h == nil {
		return
	}
	h.stopOnce.Do(func() { close(h.stop) })
}

// deliver encodes one line and appends it to every subscriber's buffer.
//
// A line that cannot be ENCODED is not sent, and that is not a silent drop: an
// unencodable line is not a line, and the alternative (a partial write) would be
// the truncated-list shape §6.2 B-10 forbids.
func (h *pushHub) deliver(ev eventLine) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	subs := h.snapshot()
	if len(subs) == 0 {
		return
	}
	size := int64(len(raw))
	for _, s := range subs {
		s.append(raw, size, ev.Seq)
	}
}

func (h *pushHub) snapshot() []*pushSub {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		return nil
	}
	out := make([]*pushSub, 0, len(h.subs))
	for s := range h.subs {
		out = append(out, s)
	}
	return out
}

// attach registers a subscriber, or reports that the hub is at its declared cap.
// The refusal is the caller's to shape (B-13).
func (h *pushHub) attach() (*pushSub, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= h.cfg.maxSubscribers {
		return nil, false
	}
	s := &pushSub{
		h:         h,
		maxBytes:  h.cfg.bufferBytes,
		maxEvents: h.cfg.bufferEvents,
		wake:      make(chan struct{}, 1),
	}
	h.subs[s] = struct{}{}
	active := int64(len(h.subs))
	h.c.active.Store(active)
	if active > h.c.activeMax.Load() {
		h.c.activeMax.Store(active)
	}
	return s, true
}

// detach releases one subscriber: the slot, the buffer and the counters. The
// trigger must be one of §5.3's closed set of two, so "dead client" and "client
// that will not drain" stay separable in the record (B-5).
func (h *pushHub) detach(s *pushSub, trigger string) {
	h.mu.Lock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
	}
	active := int64(len(h.subs))
	h.mu.Unlock()
	h.c.active.Store(active)

	s.mu.Lock()
	s.closed = true
	s.buf = nil
	s.bytes = 0
	s.pending = false
	s.mu.Unlock()

	switch trigger {
	case PushReleaseContext:
		h.c.disconnectsCtx.Add(1)
	case PushReleaseWriteDeadline:
		h.c.disconnectsDead.Add(1)
	}
}

// heartbeatLoop is the tree's own clock (§4.3). It exists because the client's
// silence is ambiguous by construction: without it a healthy idle tree and a
// stopped reader produce the identical client-visible state, which is the
// obligation BFS-040 §5.4 O-2 makes on the source.
//
// It emits NOTHING while no subscriber is attached — the poll does not generate
// heartbeats (events.go) and an unwatched tree must behave exactly as it did
// before this file existed. While a subscriber IS attached it emits through the
// ledger's funnel, so the heartbeat consumes a seq, is journaled (R-5/§3.4: a
// reconnect must be able to tell "heartbeats passed" from "events were lost")
// and reaches every subscriber at the same number.
func (h *pushHub) heartbeatLoop() {
	t := time.NewTicker(h.cfg.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.mu.Lock()
			active := len(h.subs)
			h.mu.Unlock()
			if active == 0 {
				continue
			}
			h.tree.eventLedger().push(h.tree, eventHeartbeat, nil)
		}
	}
}

// ---------------------------------------------------------------------------
// The subscriber's side: the bounded buffer and B-9's counted gap.
// ---------------------------------------------------------------------------

// append buffers one line, dropping the OLDEST buffered lines until the new one
// fits under either bound and marking exactly one gap for the run (§6.2 B-9).
//
// One marker per gap run, not one per dropped line: a subscriber that is
// permanently behind would otherwise produce a marker flood in the buffer that is
// already full, and the marker is what forces its resync — one is enough, and a
// second marker in the same run buys nothing and costs the very slot the policy
// is trying to keep free.
func (s *pushSub) append(raw []byte, size, head int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for len(s.buf) > 0 && (len(s.buf)+1 > s.maxEvents || s.bytes+size > s.maxBytes) {
		s.popOldestLocked(head)
	}
	if size > s.maxBytes {
		// The line cannot fit even in an empty buffer. Dropping it is the same
		// consequence as any other drop and is counted the same way; admitting it
		// would be a bound that does not bound.
		s.h.c.drops.Add(1)
		s.noteGapLocked(head)
		s.signalLocked()
		return
	}
	s.buf = append(s.buf, pushLine{raw: raw, size: size})
	s.bytes += size
	s.noteHighWaterLocked()
	s.signalLocked()
}

func (s *pushSub) popOldestLocked(head int64) {
	old := s.buf[0]
	s.buf = s.buf[1:]
	s.bytes -= old.size
	s.h.c.drops.Add(1)
	s.noteGapLocked(head)
}

func (s *pushSub) noteGapLocked(head int64) {
	s.pending = true
	if head > s.gapSeq {
		s.gapSeq = head
	}
}

func (s *pushSub) noteHighWaterLocked() {
	for {
		cur := s.h.c.highWater.Load()
		if s.bytes <= cur || s.h.c.highWater.CompareAndSwap(cur, s.bytes) {
			return
		}
	}
}

func (s *pushSub) signalLocked() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// next returns the next line to write to this subscriber: the owed gap marker
// first (one, counted, at a seq above the client's own cursor, R-4), otherwise
// the oldest buffered line. `ok` false means the buffer is empty and the caller
// must wait on wake or on its request context.
func (s *pushSub) next() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	if s.pending {
		s.pending = false
		head := s.gapSeq
		s.gapSeq = 0
		s.h.c.gaps.Add(1)
		raw, err := json.Marshal(eventLine{
			Seq:   head,
			Event: eventOverflow,
			Paths: []string{},
			Rev:   s.h.tree.revToken(),
			Tree:  s.h.tree.identity(),
		})
		if err != nil {
			return nil, false
		}
		return raw, true
	}
	if len(s.buf) == 0 {
		return nil, false
	}
	ln := s.buf[0]
	s.buf = s.buf[1:]
	s.bytes -= ln.size
	return ln.raw, true
}

// ---------------------------------------------------------------------------
// The handler.
// ---------------------------------------------------------------------------

// pushServed reports whether this handler offers the stream on this request:
// only where an event source actually exists. A watcher that is absent, that
// covers only part of the tree, or that has been lost refuses this op exactly as
// it did before the push form existed, with the same verdict, scope and reason —
// so a target without an event source cannot be handed a channel that reports
// itself healthy and carries nothing (§1.2 case 1).
func (h *Handler) pushServed() bool {
	return pushServedFrom(h.watchStatusSnapshot())
}

// handleWatch serves `X-Bunker-Op: watch` as an NDJSON stream (§2).
//
// The refusal path comes FIRST and is byte-identical to the pre-BFS-036 answer:
// where no watcher is established, the op is not served and says why.
func (h *Handler) handleWatch(w http.ResponseWriter, r *http.Request, start time.Time) {
	if !h.pushServed() {
		h.writeEnvelope(w, r, start, "watch", 501, VerdictCapabilityUnavailable, false, nil, h.watchOpError())
		return
	}
	body, f := h.readBody(w, r)
	if f != nil {
		h.failEnvelope(w, r, start, "watch", *f)
		return
	}
	args := struct {
		// Paths is accepted and NOT applied: W-3 lets a server scope or not
		// scope, as long as it says which, and this build says whole_tree
		// because per-subscriber filtering breaks the one-ledger rule (R-5).
		Paths []string `json:"paths"`
		// SinceSeq present is a claim to hold an observation of the tree at that
		// cursor; absent is the claim to hold none, and it is answered with the
		// interval the ledger cannot vouch for — never a quiet or partial tail
		// (the same rule, on the same ledger, as the poll: events.go).
		SinceSeq *int64 `json:"since_seq"`
	}{}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &args); err != nil {
			h.writeEnvelope(w, r, start, "watch", 400, VerdictBadArguments, false, nil,
				&envelopeError{Detail: `watch arguments are not a JSON object of {"paths":[],"since_seq":N}`})
			return
		}
	}
	if args.SinceSeq != nil && *args.SinceSeq < 0 {
		h.writeEnvelope(w, r, start, "watch", 400, VerdictBadArguments, false, nil,
			&envelopeError{Detail: "watch since_seq must be zero or positive"})
		return
	}
	point := resumePoint{}
	if args.SinceSeq != nil {
		point = resumePoint{Cursor: *args.SinceSeq, Baseline: true}
	}

	hub := h.tree.ensurePushChannel()
	sub, ok := hub.attach()
	if !ok {
		// B-13/B-14: a RETRYABLE refusal that must not look like a degradation.
		// No X-Bunker-Verdict and no X-Bunker-Capability: the client's declared
		// downgrade branch keys on those, and a client that reacted to a full
		// subscriber set by abandoning the mechanism would abandon one that is
		// available again in seconds. The client's `default:` branch (any
		// non-capability failure) sends this to its reconnect loop WITH the
		// cursor, which is exactly the right reaction.
		//
		// Every response on this surface carries X-Bunker-Verdict: ok from the
		// common headers, so this one DELETES it: a refusal that keeps the
		// success verdict would be answered into the client's success path.
		w.Header().Del("X-Bunker-Verdict")
		w.Header().Del("X-Bunker-Capability")
		w.Header().Set("X-Bunker-Op", "watch")
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error":{"capability":"watch","scope":"channel","retryable":true,"detail":"the declared maximum subscriber count is reached; this refusal is retryable and is not a capability verdict"}}` + "\n"))
		return
	}
	// Every exit from here releases the subscriber: a slot that is only returned
	// on the happy path is a leak, and "cap" must never be an alias for it (§5.4).
	released := false
	release := func(trigger string) {
		if released {
			return
		}
		released = true
		hub.detach(sub, trigger)
	}
	defer release(PushReleaseContext)

	// W-2: headers, then a line, then flush — the channel is committed before any
	// event is waited for. The commit is FLUSHED here and not left to the first
	// line's flush: a subscriber on a quiet tree must see its 200 and the
	// declared bounds immediately, not after up to one heartbeat period of
	// nothing (the no-fanout arm is what found this — with every line suppressed,
	// the headers never left the server's buffer at all).
	hdr := w.Header()
	hdr.Set("Content-Type", "application/x-ndjson")
	hdr.Set("X-Bunker-Op", "watch")
	hdr.Set("X-Bunker-Verdict", string(VerdictOK))
	hdr.Set("X-Bunker-Tree", h.tree.identity())
	hdr.Set("X-Bunker-Rev", h.tree.revToken())
	hdr.Set(PushSubscriptionHeader, PushSubscriptionWholeTree)
	hdr.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// B-6: the write deadline is what FINDS a client that vanished without
	// closing (§5.2). A transport that cannot carry one is reported rather than
	// assumed — probing with a zero deadline is the documented way to ask
	// without changing the answer, and the reset below leaves no deadline armed
	// for whatever the server writes next.
	deadlineOK := rc.SetWriteDeadline(time.Time{}) == nil
	if !deadlineOK {
		hub.c.deadlineRefused.Add(1)
	}
	// Whatever ends this subscription, no deadline is left armed on a connection
	// the server may still want to write to (§5.2: the deadline is this line's, not
	// the connection's next one's).
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	deadline := hub.cfg.writeDeadline
	arm := func() error {
		if !deadlineOK {
			return nil
		}
		return rc.SetWriteDeadline(time.Now().Add(deadline))
	}
	// The commit is not the writer's job: the headers are flushed before the
	// first line is even computed (W-2), and under the same deadline as every
	// line — a client that stopped reading is found here too, which is why the
	// commit cannot be left to the first line's flush.
	if err := arm(); err != nil {
		release(PushReleaseWriteDeadline)
		return
	}
	if err := rc.Flush(); err != nil {
		release(PushReleaseWriteDeadline)
		return
	}
	writeLine := func(raw []byte) error {
		if err := arm(); err != nil {
			return err
		}
		if _, err := w.Write(append(raw, '\n')); err != nil {
			return err
		}
		return rc.Flush()
	}

	// The resume answer, from the ledger and nothing else: the stream does not
	// observe the tree, it REPLAYS what the tree's one ledger already knows. A
	// line the ledger cannot vouch for is answered with the marker that forces a
	// re-observation (§3.2/§3.3) — never with an empty tail that would read as
	// "nothing moved".
	plan := h.tree.eventLedger().streamResume(h.tree, point)
	for _, ev := range plan {
		raw, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		if err := writeLine(raw); err != nil {
			release(PushReleaseWriteDeadline)
			return
		}
	}
	if len(plan) == 0 {
		// No line to replay: the channel is committed by an establish heartbeat,
		// pushed through the ledger's funnel so it is journaled and broadcast like
		// every other line rather than being a private token invented for this
		// subscriber (R-5/§3.4).
		h.tree.eventLedger().push(h.tree, eventHeartbeat, nil)
	}

	ctx := r.Context()
	for {
		if raw, ok := sub.next(); ok {
			if err := writeLine(raw); err != nil {
				release(PushReleaseWriteDeadline)
				return
			}
			continue
		}
		select {
		case <-ctx.Done():
			release(PushReleaseContext)
			return
		case <-sub.wake:
		}
	}
}

// pushCountersSnapshot is the channel's own counter set, as published (§6.3).
// Every figure is read from where the fact happened, and `deadline_refused_total`
// exists because B-6 makes the absence of a write deadline a REPORTED fact on the
// transport where it is absent rather than a bound that is silently weaker.
type pushCountersSnapshot struct {
	SubscribersActive         int64 `json:"subscribers_active"`
	SubscribersActiveMax      int64 `json:"subscribers_active_max"`
	DropsTotal                int64 `json:"subscriber_drops_total"`
	GapsTotal                 int64 `json:"subscriber_gaps_total"`
	DisconnectsTotal          int64 `json:"subscriber_disconnects_total"`
	DisconnectsCtxTotal       int64 `json:"subscriber_disconnects_ctx_total"`
	DisconnectsDeadlineTotal  int64 `json:"subscriber_disconnects_write_deadline_total"`
	BufferHighWaterBytes      int64 `json:"subscriber_buffer_high_water_bytes"`
	WriteDeadlineRefusedTotal int64 `json:"write_deadline_unavailable_total"`
}

// PushCounters reads the channel's counters for the tree behind h. It is exported
// for the cells: the evidence for the counted gap has to be taken from the same
// numbers the document publishes, never from a private copy.
func (h *Handler) PushCounters() pushCountersSnapshot {
	hub := h.tree.pushChannel()
	if hub == nil {
		return pushCountersSnapshot{}
	}
	return hub.counters()
}

func (h *pushHub) counters() pushCountersSnapshot {
	ctxN, deadN := h.c.disconnectsCtx.Load(), h.c.disconnectsDead.Load()
	return pushCountersSnapshot{
		SubscribersActive:         h.c.active.Load(),
		SubscribersActiveMax:      h.c.activeMax.Load(),
		DropsTotal:                h.c.drops.Load(),
		GapsTotal:                 h.c.gaps.Load(),
		DisconnectsTotal:          ctxN + deadN,
		DisconnectsCtxTotal:       ctxN,
		DisconnectsDeadlineTotal:  deadN,
		BufferHighWaterBytes:      h.c.highWater.Load(),
		WriteDeadlineRefusedTotal: h.c.deadlineRefused.Load(),
	}
}

// pushBlock renders the channel's declaration for the capability document: the
// bounds it obeys, the scoping decision it made, and its own counters (§6.3).
// `write_deadline_supported` is a fact about the TRANSPORT this document arrived
// on — like server.proto, and for the same reason: h1/h2 carry a per-response
// write deadline and h3 does not (B-6).
func (h *Handler) pushBlock(w http.ResponseWriter, served bool) map[string]any {
	cfg := h.tree.pushCfg.resolved()
	// Probing with a ZERO deadline is the documented way to ask whether the
	// transport carries one without changing the answer: on h1/h2 it clears any
	// deadline (there is none) and returns nil, on h3 it returns ErrNotSupported.
	// What this channel does NOT do is claim the bound where the transport cannot
	// carry it (B-6) — and this is the field that says which situation a client
	// is reading.
	carriesDeadline := http.NewResponseController(w).SetWriteDeadline(time.Time{}) == nil
	return map[string]any{
		"subscription":             PushSubscriptionWholeTree,
		"max_subscribers":          cfg.maxSubscribers,
		"buffer_bytes":             cfg.bufferBytes,
		"buffer_events":            cfg.bufferEvents,
		"write_deadline_ms":        cfg.writeDeadline.Milliseconds(),
		"write_deadline_supported": carriesDeadline,
		"heartbeat_ms":             cfg.heartbeat.Milliseconds(),
		"served":                   served,
		"counters":                 h.PushCounters(),
		"subscription_detail":      "every line that advances the tree's seq is delivered to every attached subscriber, so the subscription is not filtered per subscriber (SPEC-push-channel §2.3 W-3 with §3.4 R-5): a line withheld from one subscriber is a cursor gap it cannot see, and over-reporting a path a client does not hold is a no-op drop",
		"counters_detail":          "a zero here is a measured zero: these are read from the running channel, which has dropped nothing and released nobody until a stream is attached",
	}
}
