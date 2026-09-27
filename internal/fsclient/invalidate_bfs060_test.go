package fsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-060 — the two ways the invalidation channel could be SILENTLY DEAD while
// reporting itself healthy, as arms rather than as prose.
//
//   1. a CLEAN EOF ends the channel without an error: `watchOnce` sees EOF,
//      returns nil, `Run` reads nil as "the stream ended because we closed it"
//      and RETURNS — invalidation is over for the life of the mount while the
//      record still says `mode=push, mechanism=watch, channel_available=true`;
//   2. the DECLARED 90 s idle rule has no implementation, so a stalled channel
//      and a quiet tree produce the SAME observable.
//
// Both defects live in the pushed path, and the landed server refuses `watch`
// with the declared poll fallback (mode=poll), so these arms drive a stub that
// serves the channel's OWN wire form — docs/prd/SPEC-push-channel.md §2/§3:
// `POST X-Bunker-Op: watch`, `Accept: application/x-ndjson`, one JSON object per
// line, `{"paths":[],"since_seq":N}` in the body.
//
// The three arms below are written to run against the UNFIXED tree on purpose:
// they assert on behaviour and on the fields the record already had, never on a
// field this row adds. That is what makes the RED a real one — a compile error
// against the previous code would prove nothing about its behaviour.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// The stub surface: OPTIONS + `capabilities` (so the client's own capability
// document, and therefore the declared heartbeat period, are real), `events`
// (the declared poll form the fallback lands on), and `watch`, which streams
// NDJSON whose every line the TEST decides.
// ---------------------------------------------------------------------------

const bfs060Tree = "tree:0123456789abcdef"
const bfs060Rev = "git:0123456789abcdef0123456789abcdef01234567"

// bfs060Stream is one served `watch` response, driven by the test. Only the
// HANDLER goroutine ever touches the ResponseWriter — net/http's response is not
// goroutine-safe — so the test hands lines in over a channel and the handler
// writes and flushes them. The handler returns (a clean EOF) only when the test
// says so.
type bfs060Stream struct {
	send      chan string
	ready     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// bfs060WaitReady blocks until the handler has written and flushed the response
// head.
func (s *bfs060Stream) bfs060WaitReady(d time.Duration) bool {
	select {
	case <-s.ready:
		return true
	case <-time.After(d):
		return false
	}
}

// line hands one NDJSON line to the serving goroutine, which writes and flushes
// it (the channel's own requirement: BFS-005 §4.1 flushes per line, and that is
// what makes a line an observable event rather than a buffered byte).
func (s *bfs060Stream) line(body string) {
	select {
	case s.send <- body:
	case <-s.done:
	}
}

// end ends the stream CLEANLY: the handler returns, the response body reaches
// its end-of-body, and the client's scanner sees a clean EOF with no error —
// the exact event SPEC-push-channel §8.2 R-6 forbids treating as a success.
func (s *bfs060Stream) end() { s.closeOnce.Do(func() { close(s.done) }) }

// bfs060Fixture is the stub surface plus the bookkeeping the arms measure.
type bfs060Fixture struct {
	srv *httptest.Server

	// heartbeatMS is what the capability document declares; 0 means the
	// document names none (and the client must fall back to its own default).
	heartbeatMS int
	// maxEventBytes is what the document declares as the per-frame BYTE bound
	// (`extensions.watch.max_event_bytes`, BFS-062); 0 means the document
	// publishes none, which is the shape of a peer that predates the field.
	maxEventBytes int64
	// eventsDelay holds the poll's first answer back, so the window in which
	// the record has declared the channel dead is observable rather than a race.
	eventsDelay time.Duration

	mu      sync.Mutex
	streams []*bfs060Stream // the queue the test hands streams out of
	all     []*bfs060Stream // every stream ever served, so shutdown can end them all
	served  int
	probes  int
	since   []int64
}

func newBFS060Fixture(t *testing.T) *bfs060Fixture {
	t.Helper()
	fx := &bfs060Fixture{}
	fx.srv = httptest.NewServer(http.HandlerFunc(fx.serve))
	t.Cleanup(func() {
		// End every open stream BEFORE closing the server: httptest.Server.Close
		// blocks until outstanding handlers return, and a handler parked on a
		// stream the test never ended would hang the whole suite.
		fx.shutdown()
		fx.srv.Close()
	})
	return fx
}

func (fx *bfs060Fixture) shutdown() {
	fx.mu.Lock()
	all := append([]*bfs060Stream(nil), fx.all...)
	fx.streams = nil
	fx.mu.Unlock()
	for _, s := range all {
		s.end()
	}
}

func (fx *bfs060Fixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set("DAV", "1")
		w.Header().Set("Allow", "OPTIONS, POST")
		w.Header().Set("X-Bunker-Capabilities", "1")
		w.Header().Set("X-Bunker-Extensions", "identity,rev,tree,op,watch")
		w.Header().Set("X-Bunker-Tree", bfs060Tree)
		w.Header().Set("X-Bunker-Rev", bfs060Rev)
		w.WriteHeader(http.StatusOK)
		return
	}
	switch op := strings.TrimSpace(r.Header.Get("X-Bunker-Op")); op {
	case "capabilities":
		watch := map[string]any{
			"name": "X-Bunker-Op: watch", "v": 1, "mode": "push",
			"modes": map[string]any{"push": "inotify\u2192stream", "poll": "X-Bunker-Op: events"},
		}
		if fx.heartbeatMS > 0 {
			watch["heartbeat_ms"] = fx.heartbeatMS
		}
		if fx.maxEventBytes > 0 {
			// BFS-062's additive field, declared only when the arm declares one:
			// a document that publishes none must stay indistinguishable from a
			// peer that predates it.
			watch["max_event_bytes"] = fx.maxEventBytes
		}
		bfs060Envelope(w, 200, "ok", map[string]any{"capabilities": map[string]any{
			"surface": "stub/1", "document_version": 1,
			"extensions": map[string]any{"watch": watch},
		}}, nil)
	case "events":
		fx.mu.Lock()
		delay := fx.eventsDelay
		fx.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		// The declared poll form: one answer per interval, nothing moved.
		bfs060Envelope(w, 200, "ok", map[string]any{"events": []any{}}, nil)
	case "watch":
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if !strings.Contains(string(body), "since_seq") {
			// The BIND-TIME PROBE (`probeOp(ctx,"watch")`): a stream that
			// carries nothing and ends immediately — legal (BFS-040 §4.6 makes
			// a server-side stream end a designed event), and it keeps the
			// probe from parking on a stream the test owns.
			fx.mu.Lock()
			fx.probes++
			fx.mu.Unlock()
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("X-Bunker-Tree", bfs060Tree)
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		f, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stub needs a flusher", http.StatusInternalServerError)
			return
		}
		s := &bfs060Stream{
			send:  make(chan string, 8),
			ready: make(chan struct{}),
			done:  make(chan struct{}),
		}
		fx.mu.Lock()
		fx.served++
		fx.since = append(fx.since, bfs060SinceSeq(body))
		fx.streams = append(fx.streams, s)
		fx.all = append(fx.all, s)
		fx.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Bunker-Op", "watch")
		w.Header().Set("X-Bunker-Tree", bfs060Tree)
		w.WriteHeader(http.StatusOK)
		f.Flush()
		close(s.ready)
		// The serving goroutine writes every line, and ends the response only
		// when the test ends the stream.
		for {
			select {
			case <-s.done:
				return
			case ln := <-s.send:
				fmt.Fprintln(w, ln)
				f.Flush()
			}
		}
	default:
		bfs060Envelope(w, 400, "op_unknown", nil, map[string]any{"detail": "unknown X-Bunker-Op value"})
	}
}

// client builds a client against this surface and performs the mount's own bind
// (the order production uses: Handshake, then Run), so the declared heartbeat
// period and the declared poll form are measured rather than assumed.
func (fx *bfs060Fixture) client(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(Options{BaseURL: fx.srv.URL + "/dav", Concurrency: 4, OpTimeout: 10 * time.Second, BindTimeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, oerr := c.Handshake(context.Background()); oerr != nil {
		t.Fatalf("handshake: %v", oerr)
	}
	return c
}

// nextStream hands over the next served stream, or nil if none is available yet.
func (fx *bfs060Fixture) nextStream() *bfs060Stream {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.streams) == 0 {
		return nil
	}
	s := fx.streams[0]
	fx.streams = fx.streams[1:]
	return s
}

// expectStream waits (bounded) for the next served stream.
func (fx *bfs060Fixture) expectStream(t *testing.T, d time.Duration) *bfs060Stream {
	t.Helper()
	var s *bfs060Stream
	if !waitFor(d, func() bool { s = fx.nextStream(); return s != nil }) {
		served, probes := fx.counts()
		t.Fatalf("no `watch` stream was served within %s (%d served, %d probes)", d, served, probes)
	}
	if !s.bfs060WaitReady(d) {
		t.Fatalf("the served stream never published its writer within %s", d)
	}
	return s
}

func (fx *bfs060Fixture) counts() (served, probes int) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return fx.served, fx.probes
}

func (fx *bfs060Fixture) servedCount() int {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return fx.served
}

// cursors reports the `since_seq` every served watch request presented: the
// resume cursor, as the server would see it.
func (fx *bfs060Fixture) cursors() []int64 {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return append([]int64(nil), fx.since...)
}

// bfs060SinceSeq reads `since_seq` out of a watch request body. A missing key is
// what distinguishes the bind-time probe from a real subscription.
func bfs060SinceSeq(body []byte) int64 {
	var in struct {
		Since *int64 `json:"since_seq"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Since == nil {
		return 0
	}
	return *in.Since
}

// bfs060Envelope writes the E-4 envelope shape of BFS-004 §10.4.
func bfs060Envelope(w http.ResponseWriter, status int, verdict string, result any, eerr map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Bunker-Verdict", verdict)
	w.Header().Set("X-Bunker-Tree", bfs060Tree)
	w.Header().Set("X-Bunker-Rev", bfs060Rev)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": status == http.StatusOK, "op": "", "verdict": verdict,
		"rev": bfs060Rev, "tree": bfs060Tree, "proto": "HTTP/1.1",
		"duration_ms": 0, "truncated": false, "result": result, "error": eerr,
	})
}

// bfs060Invalidate is one `invalidate` line of the wire form: a change claim with
// paths, on the channel's own seq.
func bfs060Invalidate(seq int64, path string) string {
	return fmt.Sprintf(`{"seq":%d,"event":"invalidate","paths":[%q],"tree":%q}`, seq, path, bfs060Tree)
}

// bfs060Heartbeat is the liveness line: it carries no path claim and must never
// be read as "nothing changed" (BFS-040 §5.1).
func bfs060Heartbeat(seq int64) string {
	return fmt.Sprintf(`{"seq":%d,"event":"heartbeat","tree":%q}`, seq, bfs060Tree)
}

// ---------------------------------------------------------------------------
// Arm 1 — a clean EOF is a CHANNEL END, not a success (H-1 / R-6 / P-14).
// ---------------------------------------------------------------------------

// TestBFS060CleanEOFIsAChannelEndNotASuccess is the row's first acceptance: the
// server ends the stream cleanly on a live context (the designed `watch_lost`
// event, BFS-040 §4.6) and the client must come back — with the cursor — so that
// a change made after the EOF is still delivered. On the unfixed tree `Run`
// RETURNS here and leaves `channel_available=true` standing, which is the defect:
// invalidation is over for the life of the mount and nothing says so.
func TestBFS060CleanEOFIsAChannelEndNotASuccess(t *testing.T) {
	fx := newBFS060Fixture(t)
	c := fx.client(t)
	rec := &collectDrops{}
	inv := NewInvalidator(c, InvalidateOptions{
		Mode:         "auto",
		PollInterval: 20 * time.Millisecond,
		IdleTimeout:  5 * time.Second, // not under test here: the EOF must fire long before it
		OnDrop:       rec.drop,
		OnResync:     rec.resync,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- inv.Run(ctx) }()

	// (1) The channel works before the EOF, or the arm would be measuring a
	//     channel that was never established.
	first := fx.expectStream(t, 3*time.Second)
	first.line(bfs060Invalidate(1, "src/main.go"))
	if !waitFor(3*time.Second, func() bool { return rec.dropped("src/main.go") }) {
		t.Fatalf("the pushed channel delivered nothing before the EOF: %s state=%+v", rec.summary(), inv.State())
	}
	if st := inv.State(); st.Mode != ModePush || st.Mechanism != MechanismWatch || !st.Available {
		t.Fatalf("the channel was not established as a push/watch channel: %+v", st)
	}

	// (2) The server ends the stream cleanly. This is the H-1 input.
	first.end()

	// (3) Either the channel comes back, or Run returns. ONE bound, and the
	//     two endings are reported differently: a return here IS the defect.
	var second *bfs060Stream
	if !waitFor(3*time.Second, func() bool { second = fx.nextStream(); return second != nil }) {
		select {
		case err := <-runErr:
			st := inv.State()
			t.Fatalf("CLEAN EOF ENDED THE CHANNEL: Run returned (err=%v) and no reconnect was attempted, so nothing can invalidate again for the life of the mount. The record left standing was mode=%q mechanism=%q channel_available=%v, events_total=%d, drops=%d (%d watch stream(s) served)",
				err, st.Mode, st.Mechanism, st.Available, st.Events, st.DroppedPaths, fx.servedCount())
		default:
			t.Fatalf("no reconnect within 3 s and Run has not returned: %d stream(s) served, state=%+v", fx.servedCount(), inv.State())
		}
	}

	// (4) The resume cursor survives the EOF (R-2/R-6: reconnect WITH the cursor).
	if got := fx.cursors(); len(got) < 2 || got[1] != 1 {
		t.Fatalf("the reconnect did not present the cursor: since_seq per request = %v, want a second request carrying 1", got)
	}

	// (5) The acceptance itself: a change AFTER the EOF is still delivered.
	second.line(bfs060Invalidate(2, "src/util.go"))
	if !waitFor(3*time.Second, func() bool { return rec.dropped("src/util.go") }) {
		t.Fatalf("a change made AFTER the clean EOF was never delivered: the channel is a channel in name only; %s state=%+v", rec.summary(), inv.State())
	}
	st := inv.State()
	if !st.Available || st.Mode != ModePush || st.Mechanism != MechanismWatch {
		t.Fatalf("after reconnecting the record must still report a live push/watch channel: %+v", st)
	}

	// (6) Cancellation is the ONE clean close, and it is bounded.
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v after cancellation, want the one clean close (nil)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2 s of cancellation — an unbounded wait")
	}
}

// ---------------------------------------------------------------------------
// Arm 2 — a stalled channel and a quiet tree must NOT look the same (H-3).
// ---------------------------------------------------------------------------

// bfs060Claim is the record's own claim about the channel — the exact thing the
// row is about ("the channel still reports available and healthy"), projected to
// the fields that are claims rather than measurements.
//
// `events_total` is deliberately NOT part of it: both arms' fixtures choose how
// many lines to write, and a line count is the fixture's behaviour, not the
// client's claim about itself. What must differ is what the CLIENT says.
type bfs060Claim struct {
	Mode      string
	Mechanism string
	Available bool
	Reason    string
	Dropped   int64
	Resyncs   int64
	Gaps      int64
}

func (c bfs060Claim) String() string {
	return fmt.Sprintf("{mode:%s mechanism:%s channel_available:%v reason:%q dropped:%d resyncs:%d gaps:%d}",
		c.Mode, c.Mechanism, c.Available, c.Reason, c.Dropped, c.Resyncs, c.Gaps)
}

func bfs060ClaimOf(inv *Invalidator) bfs060Claim {
	st := inv.State()
	return bfs060Claim{Mode: st.Mode, Mechanism: st.Mechanism, Available: st.Available,
		Reason: st.Reason, Dropped: st.DroppedPaths, Resyncs: st.Resyncs, Gaps: st.Gaps}
}

// bfs060Observe runs ONE server behaviour against a fresh client and samples the
// record's claim for a whole bounded hold, returning the distinct claims in
// order. A claim that never moved appears once — which is itself the finding: an
// unbounded silent state, not a transient.
func bfs060Observe(t *testing.T, heartbeatMS int, hold time.Duration, drive func(s *bfs060Stream, stop <-chan struct{})) []bfs060Claim {
	t.Helper()
	fx := newBFS060Fixture(t)
	fx.heartbeatMS = heartbeatMS
	fx.eventsDelay = 250 * time.Millisecond
	c := fx.client(t)
	inv := NewInvalidator(c, InvalidateOptions{
		Mode:         "auto",
		PollInterval: 20 * time.Millisecond,
		// No IdleTimeout: the rule under test is the one the SERVER declares
		// (3 × heartbeat_ms), which is what a mount that declares nothing gets.
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- inv.Run(ctx) }()

	s := fx.expectStream(t, 3*time.Second)
	stop := make(chan struct{})
	driveDone := make(chan struct{})
	go func() { defer close(driveDone); drive(s, stop) }()
	// The driver must be gone before the fixture ends its streams: a write to a
	// ResponseWriter whose handler has already returned is noise at best.
	defer func() {
		close(stop)
		select {
		case <-driveDone:
		case <-time.After(2 * time.Second):
			t.Error("the stream driver did not stop within 2 s of being told to — an unbounded wait")
		}
	}()

	var seen []bfs060Claim
	deadline := time.Now().Add(hold)
	for time.Now().Before(deadline) {
		cl := bfs060ClaimOf(inv)
		if len(seen) == 0 || seen[len(seen)-1] != cl {
			seen = append(seen, cl)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-runErr:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2 s of cancellation — an unbounded wait")
	}
	return seen
}

// TestBFS060StalledChannelIsNotAQuietTree is the row's second acceptance, and it
// is a MEASUREMENT rather than a description: the same client is driven against
// a quiet tree (the server keeps writing its declared liveness line) and against
// a stalled channel (the stream goes silent after one line), and the two records
// are compared. On the unfixed tree they are the same record.
func TestBFS060StalledChannelIsNotAQuietTree(t *testing.T) {
	const heartbeatMS = 40 // the declared period: the rule under test is 3 × this
	const hold = 900 * time.Millisecond

	quiet := bfs060Observe(t, heartbeatMS, hold, func(s *bfs060Stream, stop <-chan struct{}) {
		tick := time.NewTicker(15 * time.Millisecond)
		defer tick.Stop()
		seq := int64(0)
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				seq++
				s.line(bfs060Heartbeat(seq))
			}
		}
	})
	stall := bfs060Observe(t, heartbeatMS, hold, func(s *bfs060Stream, stop <-chan struct{}) {
		// It was alive, and then it went silent — the one input the client
		// cannot tell from a quiet tree by silence alone.
		s.line(bfs060Heartbeat(1))
		<-stop
	})

	t.Logf("quiet tree (%s hold): %v", hold, quiet)
	t.Logf("stalled channel (%s hold): %v", hold, stall)

	last := func(c []bfs060Claim) bfs060Claim { return c[len(c)-1] }
	if last(quiet) == last(stall) {
		t.Fatalf("A STALLED CHANNEL AND A QUIET TREE ARE THE SAME OBSERVABLE: both ended the %s hold as %s — the client cannot tell a dead channel from a healthy idle tree, which is exactly the obligation BFS-040 §5.4 O-2 makes and the fourth fallback condition of SPEC-push-channel §8.3 (idle rule = 3 × %d ms = %s)",
			hold, last(stall), heartbeatMS, 3*heartbeatMS*time.Millisecond)
	}
	if len(quiet) != 1 {
		t.Fatalf("a quiet tree's record moved %d times during a hold in which nothing changed: %v", len(quiet), quiet)
	}
	if q := quiet[0]; !q.Available || q.Mode != ModePush || q.Mechanism != MechanismWatch || q.Reason != "" {
		t.Fatalf("a quiet tree must be reported healthy and quiet, not degraded: %s", q)
	}
	if len(stall) < 2 {
		t.Fatalf("the stalled channel's record never moved for the whole hold: %v", stall)
	}
	// The record must say the channel was NOT available and NAME the stall —
	// "different" is not enough, and a stall is not a quiet answer.
	var dead bfs060Claim
	for _, cl := range stall {
		if !cl.Available {
			dead = cl
			break
		}
	}
	if dead.Available {
		t.Fatalf("the record never reported the channel unavailable across the stall: %v", stall)
	}
	if !strings.Contains(strings.ToLower(dead.Reason), "stall") {
		t.Fatalf("the stall is not named in the record's reason: %q (claims: %v)", dead.Reason, stall)
	}
	if got := last(stall); got.Mode != ModePoll || got.Mechanism == MechanismWatch {
		t.Fatalf("the stalled push channel must end up on the declared poll, not still claiming the watcher: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Arm 3 — cancellation is the ONE clean close, so the fix cannot be "reconnect
// on everything".
// ---------------------------------------------------------------------------

// TestBFS060CancellationIsTheOneCleanClose pins the other direction of R-6: a
// stream the CLIENT closed (ctx cancelled) is a clean close and must not be
// reconnected, and the record must stop claiming a live channel once Run has
// returned.
func TestBFS060CancellationIsTheOneCleanClose(t *testing.T) {
	fx := newBFS060Fixture(t)
	c := fx.client(t)
	rec := &collectDrops{}
	inv := NewInvalidator(c, InvalidateOptions{
		Mode: "auto", PollInterval: 20 * time.Millisecond,
		IdleTimeout: 5 * time.Second, OnDrop: rec.drop, OnResync: rec.resync,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- inv.Run(ctx) }()

	s := fx.expectStream(t, 3*time.Second)
	s.line(bfs060Invalidate(1, "src/main.go"))
	if !waitFor(3*time.Second, func() bool { return rec.dropped("src/main.go") }) {
		t.Fatalf("nothing was delivered before cancellation: %s", rec.summary())
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v on cancellation; the context closing the stream is the one clean close, and it must be silent", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3 s of cancellation — an unbounded wait")
	}
	if got := fx.servedCount(); got != 1 {
		t.Fatalf("cancellation caused %d watch requests, want 1: the one clean close must NOT reconnect", got)
	}
	if st := inv.State(); st.Available {
		t.Fatalf("the record still claims a live channel after Run returned: %+v", st)
	}
}
