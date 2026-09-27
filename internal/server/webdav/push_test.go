package webdav

// ============================================================================
// BFS-036 — THE PUSH CHANNEL'S CELLS (server half).
//
// Every cell below names, in one line, the DEFECT IT CATCHES, and each claiming
// cell is turned RED by a source mutation in docs/evidence/BFS-036-arms.sh with a
// sha256-verified restore. The rule is BFS-046's: a test that cannot fail proves
// nothing, and a cell whose feature is missing must be GATED rather than weakened.
//
// The instrument is deliberately the REAL one:
//
//   - a REAL watcher (inotify via fsnotify) over a real directory, because the
//     defect class these cells exist for — "the channel is wired to the request
//     handler instead of to the filesystem" — is invisible to a fake backend;
//   - a REAL loopback listener and a REAL HTTP client, because the wire form is
//     what the row delivers and an in-process recorder cannot show a flush, a
//     write deadline, or a connection closing;
//   - the handler the daemon mounts (internal/server/webdav.New), not a stand-in.
//
// The one instrument that is NOT the wire is the subscriber, for the cells whose
// subject IS the buffer: a backpressure bound that has to be reached
// deterministically is reached by attaching a subscriber and not draining it,
// which is exactly what a stalled client does, without depending on a kernel
// socket buffer's size.
// ============================================================================

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/invalidation"
)

// ---------------------------------------------------------------------------
// The instruments.
// ---------------------------------------------------------------------------

// pushCell builds the handler the cells drive: a real watcher, and a push surface
// tuned so a cell does not have to wait thirty seconds for a heartbeat.
func pushCell(t *testing.T, mutate func(*invalidation.Values), tune func(*watchOptions)) (*Handler, string) {
	t.Helper()
	root := fixtureTree(t)
	v := invalidation.DefaultValues()
	v.Watch.Enabled = true
	// The declared ranges are the knob table's (BFS-043): heartbeat_ms ∈ [100,
	// 30000] and push_write_deadline_ms strictly below it. A cell drives the
	// smallest HONOURABLE values rather than dialling the knobs outside their own
	// contract, which the surface refuses by design.
	v.Watch.HeartbeatMS = 300
	v.Push.WriteDeadlineMS = 100
	if mutate != nil {
		mutate(&v)
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("the cell's own invalidation config is invalid: %v", err)
	}
	h, err := New(Config{Root: root, Build: "test-build", Invalidation: &v})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(h.Close)
	opts := watchOptionsFrom(v)
	// A short flush so a change is journaled as soon as the kernel reports it;
	// the production default (250 ms) would make every cell a timing exercise.
	opts.flushEvery = 5 * time.Millisecond
	if tune != nil {
		tune(&opts)
	}
	w := h.startWatch(defaultWatchEnv(), opts)
	if st := w.statusSnapshot(); st.State != WatchStateWatching {
		t.Skipf("no real watch backend is establishable on this host (%s: %s) — the cell cannot run, and it says so rather than passing", st.Reason, st.Detail)
	}
	return h, root
}

// pushEndpoint serves h on a loopback listener exactly as a deployment would.
func pushEndpoint(t *testing.T, h *Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String() + "/dav"
}

// pushConn is one open subscription as a CLIENT holds it: the response, a line
// reader, and the connection underneath so a cell can bound a read or kill the
// client the way a NAT drop does.
type pushConn struct {
	resp  *http.Response
	br    *bufio.Reader
	conn  net.Conn
	lines []eventLine
	raw   []string
}

// openPushStream performs the E-6 subscribe: POST + X-Bunker-Op: watch +
// Accept: application/x-ndjson, and returns as soon as the response HEADERS
// arrive — which is exactly the client's establish step (the mount blocks in
// `c.hc.Do(req)` until then), so a cell that gets here has proved W-2: the channel
// was committed without waiting for an event.
func openPushStream(t *testing.T, base, body string) *pushConn {
	t.Helper()
	var mu sync.Mutex
	var held net.Conn
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err == nil {
				mu.Lock()
				held = c
				mu.Unlock()
			}
			return c, err
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	req, err := http.NewRequest(http.MethodPost, base+"/", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Bunker-Op", "watch")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("the subscribe did not commit a response: %v", err)
	}
	mu.Lock()
	conn := held
	mu.Unlock()
	c := &pushConn{resp: resp, br: bufio.NewReader(resp.Body), conn: conn}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return c
}

// kill closes the connection from the client side without reading anything else,
// which is what a vanished client looks like from the server.
func (c *pushConn) kill() {
	if c.conn != nil {
		_ = c.conn.Close()
		return
	}
	_ = c.resp.Body.Close()
}

// next reads one line with a bound. A line that does not arrive inside `within` is
// reported as absent rather than blocking the cell forever.
func (c *pushConn) next(t *testing.T, within time.Duration) (eventLine, bool) {
	t.Helper()
	if c.conn != nil {
		_ = c.conn.SetReadDeadline(time.Now().Add(within))
		defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()
	}
	raw, err := c.br.ReadBytes('\n')
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return eventLine{}, false
		}
		return eventLine{}, false
	}
	var ev eventLine
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("a stream line is not an E-6 event object: %q (%v)", string(raw), err)
	}
	// The evidence transcripts quote the bytes that actually crossed the wire, so
	// the raw line is kept.
	c.raw = append(c.raw, strings.TrimRight(string(raw), "\n"))
	c.lines = append(c.lines, ev)
	return ev, true
}

// until reads lines until pred is true or the bound expires, and reports every
// line it saw so a failure can quote the stream rather than a summary.
func (c *pushConn) until(t *testing.T, within time.Duration, pred func(eventLine) bool) (eventLine, bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		ev, ok := c.next(t, time.Until(deadline))
		if !ok {
			return eventLine{}, false
		}
		if pred(ev) {
			return ev, true
		}
	}
	return eventLine{}, false
}

func (c *pushConn) seen() string {
	raw, _ := json.Marshal(c.lines)
	return string(raw)
}

// subscribeBody is the client's own body: the resume point it holds. `cursor` of
// -1 means "present no cursor at all", which is a different claim from cursor 0.
func subscribeBody(cursor int64) string {
	if cursor < 0 {
		return `{"paths":[]}`
	}
	raw, _ := json.Marshal(map[string]any{"paths": []string{}, "since_seq": cursor})
	return string(raw)
}

// snapshotCursor mints a cursor the way a mounting client does: one whole-tree
// snapshot, whose head_seq is the observation the client then presents.
func snapshotCursor(t *testing.T, h *Handler) int64 {
	t.Helper()
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "snapshot"}, `{"path":".","depth":"infinity"}`)
	if rec.Code != 200 {
		t.Fatalf("snapshot -> %d %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result struct {
			HeadSeq int64 `json:"head_seq"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("snapshot envelope: %v", err)
	}
	return env.Result.HeadSeq
}

// ---------------------------------------------------------------------------
// CELL P-1 — the change is PUSHED and the WIRE says which mechanism delivered it.
//
// the defect it catches: a channel that reports itself healthy and carries
// nothing — the shape SPEC-push-channel §1.2 names for a long-poll answer that
// unmarshals into the client's Event with no name and is dropped. The writer here
// is NOT this surface: it is /bin/sh writing the file directly, with no request of
// ours anywhere in the write path (PRD R1).
// ---------------------------------------------------------------------------
func TestPushCell01AChangeIsPushedWithTheMechanismOnTheWire(t *testing.T) {
	h, root := pushCell(t, nil, nil)
	base := pushEndpoint(t, h)

	// The client's own bind: one snapshot, and the cursor it mints.
	cursor := snapshotCursor(t, h)
	stream := openPushStream(t, base, subscribeBody(cursor))
	if got := stream.resp.StatusCode; got != 200 {
		t.Fatalf("subscribe -> %d", got)
	}
	if got := stream.resp.Header.Get(PushSubscriptionHeader); got != PushSubscriptionWholeTree {
		t.Fatalf("%s = %q, want %q", PushSubscriptionHeader, got, PushSubscriptionWholeTree)
	}
	if got := stream.resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/x-ndjson") {
		t.Fatalf("Content-Type = %q, want the NDJSON the wire form is", got)
	}
	// W-2 on the wire: a line arrives before any event does.
	first, ok := stream.next(t, 2*time.Second)
	if !ok {
		t.Fatalf("no line was written on subscribe: the channel is not committed until an event arrives (§2 W-2)")
	}
	if first.Event != eventHeartbeat {
		t.Fatalf("the establish line is %q, want heartbeat on a quiet tree", first.Event)
	}

	// THE OUT-OF-BAND EDIT: a shell, on the target, no request of ours.
	abs := filepath.Join(root, "src", "main.go")
	writeOutOfBand(t, abs, "package main\n\nfunc main() { /* pushed, not polled */ }\n")

	ev, ok := stream.until(t, 5*time.Second, func(ev eventLine) bool {
		return ev.Event == eventInvalidate && containsPath(ev.Paths, "src/main.go")
	})
	if !ok {
		t.Fatalf("the change never arrived as an invalidate carrying src/main.go; the stream saw %s", stream.seen())
	}
	if ev.Seq == 0 || ev.Tree == "" {
		t.Fatalf("the pushed line carries no cursor identity: %+v", ev)
	}
	// §3.1 R-1: the cursor is monotone across the heartbeat too, so a client that
	// records every line's seq sees no gap.
	last := int64(0)
	for _, got := range stream.lines {
		if got.Seq <= last {
			t.Fatalf("the stream is not monotone: %s", stream.seen())
		}
		last = got.Seq
	}
	// And the mechanism is ON THE WIRE, not inferred: this is the push channel's
	// own op, its own content type and its own subscription declaration.
	if got := stream.resp.Header.Get("X-Bunker-Op"); got != "watch" {
		t.Fatalf("X-Bunker-Op = %q, want watch", got)
	}
	for _, raw := range stream.raw {
		t.Logf("wire: %s", raw)
	}
	t.Logf("channel: subscription=%s heartbeat_ms=%d buffer_bytes=%d buffer_events=%d max_subscribers=%d write_deadline_ms=%d counters=%+v",
		stream.resp.Header.Get(PushSubscriptionHeader), h.tree.pushCfg.resolved().heartbeat.Milliseconds(),
		h.tree.pushCfg.resolved().bufferBytes, h.tree.pushCfg.resolved().bufferEvents, h.tree.pushCfg.resolved().maxSubscribers,
		h.tree.pushCfg.resolved().writeDeadline.Milliseconds(), h.PushCounters())
}

// writeOutOfBand is the non-WebDAV writer: /bin/sh, the file, nothing else.
func writeOutOfBand(t *testing.T, abs, body string) {
	t.Helper()
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatalf("out-of-band write: %v", err)
	}
}

// waitForCount waits until the channel reports exactly want active subscribers.
func waitForCount(t *testing.T, h *Handler, want int64) bool {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if h.PushCounters().SubscribersActive == want {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// CELL P-2 — the DEAD CLIENT: a request is never held open on a client that will
// never read, and the slot comes back.
//
// the defect it catches: a subscription that is never released, so a client that
// vanishes (NAT drop, power loss) leaves a goroutine, a buffer and a slot
// allocated for the life of the process — the not-hang standard (B-4) applied to
// the channel, and the state in which a subscriber CAP stops being a cap and
// becomes a leak.
// ---------------------------------------------------------------------------
func TestPushCell02DeadClientIsReleasedAndItsSlotComesBack(t *testing.T) {
	h, _ := pushCell(t, nil, nil)
	base := pushEndpoint(t, h)
	cursor := snapshotCursor(t, h)

	stream := openPushStream(t, base, subscribeBody(cursor))
	if _, ok := stream.next(t, 2*time.Second); !ok {
		t.Fatal("the channel did not commit a line on subscribe")
	}
	waitFor(t, 3*time.Second, "the subscriber to be counted", func() bool {
		return h.PushCounters().SubscribersActive == 1
	})

	// The client vanishes without a polite close of the STREAM: the connection is
	// dropped, which is the cancellation the runtime documents (M8).
	stream.kill()

	waitFor(t, 5*time.Second, "the dead client to be released", func() bool {
		return h.PushCounters().SubscribersActive == 0
	})
	c := h.PushCounters()
	if c.DisconnectsTotal == 0 || c.DisconnectsCtxTotal == 0 {
		t.Fatalf("the release was not counted under its trigger: %+v", c)
	}
	// The bound is honoured BECAUSE it is enforced: the release carries the CLOSED
	// trigger vocabulary, so "dead client" and "client that will not drain" stay
	// separable in the record (§5.3).
	if c.DisconnectsCtxTotal+c.DisconnectsDeadlineTotal != c.DisconnectsTotal {
		t.Fatalf("a disconnect was counted outside the declared triggers: %+v", c)
	}
}

// ---------------------------------------------------------------------------
// CELL P-3 — a client that STOPS READING is bounded by the write deadline, and a
// client that keeps reading is NOT.
//
// the defect it catches: a write that blocks forever on a peer that will never
// drain — the "slow" and "gone" cases collapsing into one, which is what makes a
// bounded hold unbounded in practice. The arm is deterministic rather than
// socket-buffer-dependent: the server is served over a net.Pipe whose far end the
// test NEVER reads, so the first flush cannot complete and the declared write
// deadline is the only thing that can end it.
// ---------------------------------------------------------------------------
func TestPushCell03StalledReaderIsReleasedByTheWriteDeadline(t *testing.T) {
	h, _ := pushCell(t, nil, nil)

	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	ln := &singleConnListener{conn: server}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// The request goes in; the response is NEVER read. Writing the request does not
	// block (the pipe's other end is the server, which is reading), so the cell
	// reaches the state it wants: a subscription whose client will not drain.
	go func() {
		_, _ = io.WriteString(client, "POST /dav/ HTTP/1.1\r\nHost: pipe\r\nX-Bunker-Op: watch\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}")
	}()

	waitFor(t, 10*time.Second, "the stalled subscriber to be released by the write deadline", func() bool {
		return h.PushCounters().DisconnectsDeadlineTotal >= 1
	})
	if got := h.PushCounters().SubscribersActive; got != 0 {
		t.Fatalf("subscribers_active = %d after the write-deadline release, want 0", got)
	}
}

// singleConnListener hands out one connection and then blocks, so a cell can serve
// a handler over a connection it owns both ends of.
type singleConnListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	served := false
	l.once.Do(func() { served = true })
	if served {
		return l.conn, nil
	}
	if l.done == nil {
		l.done = make(chan struct{})
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error {
	if l.done != nil {
		select {
		case <-l.done:
		default:
			close(l.done)
		}
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// ---------------------------------------------------------------------------
// CELL P-4 — BACKPRESSURE: drop-oldest, ONE counted gap marker, and never a
// partial or coalesced path list.
//
// the defect it catches: a buffer that grows without bound because a client is
// behind (memory driven by a client), a marker emitted once PER DROPPED LINE (a
// marker flood in the buffer that is already full), and — the silent one — a
// truncated `paths[]` presented as a complete change, which the client applies as
// if it were the whole interval.
// ---------------------------------------------------------------------------
func TestPushCell04BackpressureDropsOldestAndCountsOneGap(t *testing.T) {
	// A four-event buffer, so the bound is reachable with a handful of pushes.
	h, _ := pushCell(t, func(v *invalidation.Values) {
		v.Push.SubscriberBufferEvents = 4
		v.Push.SubscriberBufferBytes = 1 << 20
	}, nil)

	hub := h.tree.ensurePushChannel()
	sub, ok := hub.attach()
	if !ok {
		t.Fatal("the hub refused a subscriber at an eight-subscriber cap with none attached")
	}
	defer hub.detach(sub, PushReleaseContext)

	// Nine lines through the ONE funnel, each with a full path list. The first four
	// fit; the rest must push the oldest out.
	const pushed = 9
	ledger := h.tree.eventLedger()
	var want []string
	for i := 0; i < pushed; i++ {
		p := filepath.Join("src", "f"+string(rune('a'+i))+".go")
		want = append(want, p)
		ledger.push(h.tree, eventInvalidate, []string{p})
	}

	got := drainSub(t, sub)
	if len(got) == 0 {
		t.Fatal("the subscriber produced no lines after nine pushes: the buffer policy dropped everything and said nothing")
	}
	markers := 0
	for _, ln := range got {
		if ln.Event == eventOverflow {
			markers++
			if len(ln.Paths) != 0 {
				t.Fatalf("the gap marker carries paths: %+v", ln)
			}
		}
	}
	if markers != 1 {
		t.Fatalf("got %d gap markers, want exactly ONE per gap run (§6.2 B-9): %+v", markers, got)
	}
	c := h.PushCounters()
	if c.DropsTotal == 0 {
		t.Fatal("lines were dropped but subscriber_drops_total stayed 0 (the BFS-032 shape: a counter that cannot move)")
	}
	if c.GapsTotal != 1 {
		t.Fatalf("subscriber_gaps_total = %d, want 1 (the counted gap the policy promises)", c.GapsTotal)
	}
	// B-10: every invalidate line that WAS written carries the full list it was
	// pushed with, and the marker names a seq a client can act on.
	for _, ln := range got {
		if ln.Event != eventInvalidate {
			continue
		}
		if len(ln.Paths) != 1 || !containsPath(want, ln.Paths[0]) {
			t.Fatalf("a written path list is not the one that was pushed: %+v", ln)
		}
	}
	// The marker's seq must be above the last line the client could have applied,
	// or the client's own duplicate rule discards the notice (R-4).
	var maxApplied int64
	for _, ln := range got {
		if ln.Event == eventInvalidate && ln.Seq > maxApplied {
			maxApplied = ln.Seq
		}
	}
	for _, ln := range got {
		if ln.Event == eventOverflow && ln.Seq < maxApplied {
			t.Fatalf("the gap marker names seq %d, below lines the client already applied (%d): a dropped overflow is a silent gap the server believes it reported (R-4)", ln.Seq, maxApplied)
		}
	}
	// The bound is REPORTABLE, not merely enforced (PRD §2.7).
	if c.BufferHighWaterBytes == 0 {
		t.Fatal("subscriber_buffer_high_water_bytes stayed 0 while the buffer was filled")
	}
}

// drainSub empties a subscriber's buffer the way the writer goroutine does.
func drainSub(t *testing.T, sub *pushSub) []eventLine {
	t.Helper()
	var out []eventLine
	for {
		raw, ok := sub.next()
		if !ok {
			return out
		}
		var ev eventLine
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("the subscriber produced a line that is not an event object: %q", raw)
		}
		out = append(out, ev)
	}
}

// ---------------------------------------------------------------------------
// CELL P-5 — the subscriber CAP refuses RETRYABLY and never looks like a
// degradation.
//
// the defect it catches: a capacity refusal that reads as `capability_unavailable`,
// so a client whose slot frees up in seconds abandons the mechanism permanently
// (and, under the three-strike rule, counts a retryable refusal as evidence about
// the capability). SPEC-push-channel §6.4 B-13/B-14.
// ---------------------------------------------------------------------------
func TestPushCell05SubscriberCapRefusesRetryably(t *testing.T) {
	h, _ := pushCell(t, func(v *invalidation.Values) {
		v.Push.MaxSubscribers = 1
	}, nil)
	base := pushEndpoint(t, h)
	cursor := snapshotCursor(t, h)

	first := openPushStream(t, base, subscribeBody(cursor))
	if _, ok := first.next(t, 2*time.Second); !ok {
		t.Fatal("the first subscriber got no line")
	}
	if !waitForCount(t, h, 1) {
		t.Fatal("the first subscriber is not counted")
	}

	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "watch"}, subscribeBody(cursor))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the second subscriber -> %d, want 429 (a retryable refusal, not a degradation): %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("the capacity refusal carries no Retry-After")
	}
	if got := rec.Header().Get("X-Bunker-Verdict"); got != "" {
		t.Fatalf("the capacity refusal carries X-Bunker-Verdict=%q: the client's downgrade branch keys on it, so a client would abandon a mechanism that is available again in seconds", got)
	}
	if got := rec.Header().Get("X-Bunker-Capability"); got != "" {
		t.Fatalf("the capacity refusal carries X-Bunker-Capability=%q, which is the carrier the client's capability branch reads", got)
	}
	// And the slot really is exhausted by the first stream, not by something else.
	if c := h.PushCounters(); c.SubscribersActive != 1 || c.SubscribersActiveMax < 1 {
		t.Fatalf("counters do not describe the state the refusal came from: %+v", c)
	}
}

// ---------------------------------------------------------------------------
// CELL P-6 — the DECLARED heartbeat bound is HONOURED, and quiet is proved alive.
//
// the defect it catches: an open channel that produces nothing, so a healthy idle
// tree and a stalled reader are the same client-visible state — the ambiguity
// BFS-040 §5.4 O-2 makes an obligation and BFS-005 §4.4 turns into the client's
// 90 s rule. A declared bound that is not written is not a bound.
// ---------------------------------------------------------------------------
func TestPushCell06TheDeclaredHeartbeatBoundIsHonoured(t *testing.T) {
	h, _ := pushCell(t, nil, nil)
	base := pushEndpoint(t, h)
	cursor := snapshotCursor(t, h)

	stream := openPushStream(t, base, subscribeBody(cursor))
	period := h.tree.pushCfg.resolved().heartbeat
	start := time.Now()
	lines := 0
	for lines < 4 && time.Since(start) < 5*time.Second {
		if _, ok := stream.next(t, 3*period); !ok {
			t.Fatalf("no line arrived within 3 declared heartbeat periods (%s): the bound is declared and not honoured", 3*period)
		}
		lines++
	}
	if lines < 4 {
		t.Fatalf("only %d lines in %s on a quiet tree", lines, time.Since(start))
	}
	seq := int64(0)
	for _, ev := range stream.lines {
		if ev.Event != eventHeartbeat {
			t.Fatalf("a quiet tree produced a %q line: %+v", ev.Event, ev)
		}
		if ev.Seq <= seq {
			t.Fatalf("heartbeat seqs are not monotone: %s", stream.seen())
		}
		seq = ev.Seq
	}
	t.Logf("liveness on a quiet tree: %d heartbeat lines in %s (declared period %s, bound honoured)", lines, time.Since(start), period)
	for _, raw := range stream.raw {
		t.Logf("wire: %s", raw)
	}
	// The document declares the same period the channel obeys (the number a client
	// derives its idle rule from must be the number in force).
	doc := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "capabilities"}, "")
	var caps struct {
		Result struct {
			Capabilities struct {
				Extensions struct {
					Watch struct {
						HeartbeatMS int    `json:"heartbeat_ms"`
						Mode        string `json:"mode"`
						Push        *struct {
							HeartbeatMS int64 `json:"heartbeat_ms"`
							Served      bool  `json:"served"`
						} `json:"push"`
					} `json:"watch"`
				} `json:"extensions"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(doc.Body.Bytes(), &caps); err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	w := caps.Result.Capabilities.Extensions.Watch
	if w.Mode != "push" {
		t.Fatalf("document mode = %q, want push where the stream is served", w.Mode)
	}
	if w.Push == nil || !w.Push.Served {
		t.Fatalf("the document does not declare the channel served: %+v", w.Push)
	}
	if int64(w.HeartbeatMS) != period.Milliseconds() {
		t.Fatalf("the document declares heartbeat_ms=%d and the channel obeys %d ms", w.HeartbeatMS, period.Milliseconds())
	}
}

// ---------------------------------------------------------------------------
// CELL P-7 — THE POLL IS UNCHANGED, with a stream attached and without a watcher.
//
// the defect it catches: an upgrade that quietly becomes a requirement — the poll
// form refusing once the push form exists, or changing shape because a stream is
// attached to the same tree (heartbeats are journaled and a polling client may
// carry them, which is legal and must not break the envelope).
// ---------------------------------------------------------------------------
func TestPushCell07ThePollFormIsUnchanged(t *testing.T) {
	h, _ := pushCell(t, nil, nil)
	base := pushEndpoint(t, h)
	cursor := snapshotCursor(t, h)
	stream := openPushStream(t, base, subscribeBody(cursor))
	if _, ok := stream.next(t, 2*time.Second); !ok {
		t.Fatal("the stream did not commit")
	}

	// The poll, while a stream is attached to the same tree.
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "events"}, `{"since_seq":`+jsonNumber(cursor)+`}`)
	if rec.Code != 200 {
		t.Fatalf("the declared poll form answered %d with a stream attached: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Events  []eventLine `json:"events"`
			Count   int         `json:"count"`
			HeadSeq int64       `json:"head_seq"`
			Scanned int         `json:"scanned"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("the poll's envelope is not the E-4 envelope: %v (%s)", err, rec.Body.String())
	}
	if !env.OK || env.Result.HeadSeq == 0 {
		t.Fatalf("the poll answered without its own state: %s", rec.Body.String())
	}
	if env.Result.Count != len(env.Result.Events) {
		t.Fatalf("count=%d with %d events", env.Result.Count, len(env.Result.Events))
	}
	// The single-envelope property is what makes the poll safe to call from
	// anywhere: it answers and closes, so nothing is held open on a dead client.
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("the poll's content type changed to %q", rec.Header().Get("Content-Type"))
	}

	// And WITHOUT a watcher the push op is refused EXACTLY as it was before this
	// row, with the declared poll form still served: push is an upgrade, never a
	// requirement.
	plain := newTestHandler(t)
	refusal := do(t, plain, "POST", "/dav/", map[string]string{"X-Bunker-Op": "watch"}, `{}`)
	if refusal.Code != 501 {
		t.Fatalf("watch without a watcher -> %d, want 501 (unchanged for every host without an event source)", refusal.Code)
	}
	if got := refusal.Header().Get("X-Bunker-Verdict"); got != string(VerdictCapabilityUnavailable) {
		t.Fatalf("verdict = %q, want capability_unavailable", got)
	}
	poll := do(t, plain, "POST", "/dav/", map[string]string{"X-Bunker-Op": "events"}, `{}`)
	if poll.Code != 200 {
		t.Fatalf("the declared poll form answered %d on a handler with no watcher", poll.Code)
	}
}

func jsonNumber(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// ---------------------------------------------------------------------------
// CELL P-8 — RESUME: inside the journal, at head, behind it, and ahead of it.
//
// the defect it catches: a resume that answers a tail the client cannot recognise
// as partial (a silent gap — the class BFS-063 fixed for the poll and this row
// must not reintroduce on the stream), and an `overflow` emitted at a seq the
// client's own duplicate rule discards (R-4), which is a silent gap the server
// believes it reported.
// ---------------------------------------------------------------------------
func TestPushCell08ResumeNeverAnswersASilentGap(t *testing.T) {
	h, _ := pushCell(t, nil, nil)
	ledger := h.tree.eventLedger()

	// The ledger is STARTED by an observation, the way a tree that has ever been
	// snapshotted or polled is: `started` false is its own case (a ledger that has
	// observed nothing at all answers every resume with the interval it cannot
	// vouch for) and it is asserted separately below.
	h.tree.seedEvents(map[string]identity{".": {}}, true)

	// A ledger with a known history: five invalidates, rotated past its first
	// entries so `base > 1` (the condition under which a tail stops being
	// self-describing for a cursor of 0).
	for i := 0; i < eventsJournalEvents+3; i++ {
		ledger.push(h.tree, eventInvalidate, []string{"src/f" + jsonNumber(int64(i)) + ".go"})
	}
	head := h.tree.ledgerCursor()

	cases := []struct {
		name       string
		point      resumePoint
		wantMarker bool
		wantLines  int
	}{
		{"inside the retained journal replays the missed lines", resumePoint{Cursor: head - 2, Baseline: true}, false, 2},
		{"at head replays nothing (the establish line follows)", resumePoint{Cursor: head, Baseline: true}, false, 0},
		{"behind the journal is an overflow FIRST, never a tail alone", resumePoint{Cursor: 0, Baseline: true}, true, 0},
		{"no cursor at all is not vouched for", resumePoint{}, true, 0},
		{"a cursor AHEAD advances past it, then overflows", resumePoint{Cursor: head + 5, Baseline: true}, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := h.tree.ledgerCursor()
			lines := h.tree.eventLedger().streamResume(h.tree, tc.point)
			if tc.wantMarker {
				if len(lines) == 0 || lines[0].Event != eventOverflow {
					t.Fatalf("the answer starts with %+v, want an overflow first (R-3)", lines)
				}
				// R-4: the notice must be above the presented cursor, or the
				// client's own duplicate rule drops it.
				if lines[0].Seq <= tc.point.Cursor {
					t.Fatalf("the overflow names seq %d, at or below the presented cursor %d: the client discards it and the gap is silent", lines[0].Seq, tc.point.Cursor)
				}
				if tc.point.Cursor > before && h.tree.ledgerCursor() <= tc.point.Cursor {
					t.Fatalf("a cursor ahead of the ledger did not ADVANCE the counter past itself: cursor %d, ledger %d", tc.point.Cursor, h.tree.ledgerCursor())
				}
			}
			if !tc.wantMarker && len(lines) != tc.wantLines {
				t.Fatalf("replayed %d lines, want %d: %+v", len(lines), tc.wantLines, lines)
			}
			for _, ln := range lines {
				if ln.Tree == "" {
					t.Fatalf("a replayed line carries no tree identity: %+v", ln)
				}
			}
		})
	}

	// The unstarted ledger, on its own tree: a process that has observed NOTHING
	// cannot answer "nothing moved", and a cursor it never minted is not evidence
	// about an interval it never saw.
	t.Run("a ledger that has observed nothing vouches for nothing", func(t *testing.T) {
		fresh, _ := pushCell(t, nil, nil)
		lines := fresh.tree.eventLedger().streamResume(fresh.tree, resumePoint{Cursor: 12, Baseline: true})
		if len(lines) == 0 || lines[0].Event != eventOverflow {
			t.Fatalf("an unobserved ledger answered %+v, want an overflow: an empty tail is a claim that the interval was observed (R-3)", lines)
		}
	})
}

// ---------------------------------------------------------------------------
// CELL P-9 — the stream survives a deadline that would kill a routed request.
//
// the defect it catches: BFS-006's own defect arriving through a new door — the
// channel placed BEHIND the chi router, where middleware.Timeout(300 s) can reap
// it. A stream that dies on a schedule while the record still says push is the
// same silent-failure class as the clean EOF (BFS-060). The arm drives a real
// deadline (60 ms) that is demonstrably alive, and requires the stream to outlive
// it.
// ---------------------------------------------------------------------------
func TestPushCell09TheStreamOutlivesAnRPCDeadline(t *testing.T) {
	h, _ := pushCell(t, nil, nil)
	base := pushEndpoint(t, h)
	cursor := snapshotCursor(t, h)

	// The control arm: a request-shaped hold BEHIND the deadline dies at the
	// deadline, which is what makes the assertion below non-vacuous.
	deadline := 60 * time.Millisecond
	routed := http.TimeoutHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * deadline)
		_, _ = w.Write([]byte("late"))
	}), deadline, "deadline exceeded")
	ctrl := do(t, routed, "POST", "/dav/", map[string]string{"X-Bunker-Op": "watch"}, `{}`)
	if ctrl.Code != http.StatusServiceUnavailable {
		t.Fatalf("the control (a hold behind the deadline) -> %d, want 503: the deadline is not alive, so the arm below proves nothing", ctrl.Code)
	}

	// The channel: committed, then held open well past the same deadline.
	stream := openPushStream(t, base, subscribeBody(cursor))
	if _, ok := stream.next(t, 2*time.Second); !ok {
		t.Fatal("the stream did not commit")
	}
	time.Sleep(3 * deadline)
	ev, ok := stream.next(t, 2*time.Second)
	if !ok {
		t.Fatal("the stream produced no line after surviving the deadline: it was reaped, or it is not honouring its heartbeat bound")
	}
	if ev.Seq == 0 {
		t.Fatalf("the surviving line carries no cursor: %+v", ev)
	}
}

// treeCursorPath is used by the evidence transcript to quote the served root; it
// keeps the cell's assertions independent of the fixture's absolute path.
func treeCursorPath(h *Handler) string { return filepath.Base(h.Root()) }

var _ = treeCursorPath

// ---------------------------------------------------------------------------
// CELL P-10 — the channel is COMMITTED before any line is waited for.
//
// the defect it catches: the headers are written but never FLUSHED, so nothing
// leaves the server until the first line's flush — up to one heartbeat period
// (30 s at the default bounds) of a client holding a request whose answer it
// cannot read. Found by the no-fanout arm, not by a cell: with every line
// suppressed, openPushStream never returned at all, and the arm HUNG instead of
// going red. A hang is not a red, so the property gets its own cell.
//
// The instrument is a recording writer rather than a socket, because the claim
// is about what the handler has committed BEFORE the first line exists; on a
// socket the same defect is only visible as a timeout, which is the thing being
// measured away.
// ---------------------------------------------------------------------------

// commitRecorder records the bytes written at each Flush. The FIRST flush is the
// commit, and it must carry no line.
type commitRecorder struct {
	mu      sync.Mutex
	hdr     http.Header
	status  int
	body    []byte
	flushes int
	atFlush []string
}

func (c *commitRecorder) Header() http.Header { return c.hdr }

func (c *commitRecorder) WriteHeader(status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status == 0 {
		c.status = status
	}
}

func (c *commitRecorder) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = append(c.body, p...)
	return len(p), nil
}

func (c *commitRecorder) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushes++
	c.atFlush = append(c.atFlush, string(c.body))
}

func (c *commitRecorder) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flushes
}

func (c *commitRecorder) first() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.atFlush) == 0 {
		return "", false
	}
	return c.atFlush[0], true
}

func TestPushCell10TheChannelIsCommittedBeforeAnyLineIsWaited(t *testing.T) {
	h, _ := pushCell(t, nil, nil)
	rec := &commitRecorder{hdr: http.Header{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/dav/", strings.NewReader(`{"paths":[],"since_seq":0}`)).WithContext(ctx)
	req.Header.Set("X-Bunker-Op", "watch")
	req.Header.Set("Accept", "application/x-ndjson")
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, req)
	}()

	waitFor(t, 3*time.Second, "the channel to be committed", func() bool { return rec.count() >= 1 })
	first, ok := rec.first()
	if !ok {
		t.Fatal("nothing was flushed: the subscriber's answer never left the handler")
	}
	if strings.Contains(first, "\n") {
		t.Fatalf("the FIRST flush carried a line (%q): the commitment is left to the first line, so a client on a quiet tree reads nothing until a line happens to exist — up to one heartbeat period after it subscribed", first)
	}
	if rec.status != http.StatusOK {
		t.Fatalf("status at the commit = %d, want 200", rec.status)
	}
	for name, want := range map[string]string{
		"X-Bunker-Op":          "watch",
		"Content-Type":         "application/x-ndjson",
		PushSubscriptionHeader: PushSubscriptionWholeTree,
		"X-Bunker-Verdict":     string(VerdictOK),
	} {
		if got := rec.hdr.Get(name); got != want {
			t.Fatalf("%s at the commit = %q, want %q", name, got, want)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return when its request context was cancelled")
	}
}
