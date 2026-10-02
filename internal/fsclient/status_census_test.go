package fsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-045: every figure the status record reports must be COUNTED, VISIBLE, and
// PROVABLY MOVED by a workload that must move it.
//
// The law is standing and it has been broken twice in exactly this area:
//
//   - BFS-031: the cache bound did not bound the directory, and the reported
//     figure described something other than the thing it claimed to bound;
//   - BFS-032: a counter that EXISTS and is DISPLAYED could never move from the
//     live read path, because a pre-filter sat above it. 0 oversize_bypasses
//     looked like "this never happened" when the truth was "this can never be
//     counted".
//
// So this file does three things that a test which merely asserts a field exists
// cannot do:
//
//  1. THE CENSUS IS COMPLETE OR THE TEST FAILS. Every numeric leaf the record
//     publishes under `cache.*` and `invalidation.*` must appear in the table
//     below, where it is either DRIVEN (and must move — or appear, when it is a
//     figure that only exists once there is something to measure) or a DECLARED
//     BOUND or a PASSTHROUGH whose value is asserted explicitly. A counter added
//     later with no driver makes this test fail with "unclassified figure": the
//     row's whole point is that a figure nobody can move must not be able to
//     ship quietly.
//  2. A NULL CARRIES A REASON. Every `_reason`/`*_reason` field the record
//     publishes must open with one of the four vocabulary classes
//     (disabled | unknown | not_published | no_sample) — never be empty, never a
//     bare null.
//  3. THE MEASUREMENT AGREES WITH AN INDEPENDENT ONE. `dir_bytes` (a walk of the
//     filesystem) must equal a walk the TEST performs for itself, and the
//     unaccounted delta must equal the files the published figure does not
//     count — because BFS-031's defect was precisely a reported figure that
//     described something else.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Small helpers this file needs, kept local so nothing here changes a shared
// fixture another row's arms depend on.
// ---------------------------------------------------------------------------

// newTestCacheWithBounds opens a cache in a temp dir with the exact bounds this
// census needs (a byte bound, an entry bound, a per-entry cap and a staged
// window all small enough that the refusals are reachable in a test).
func newTestCacheWithBounds(t *testing.T, cfg CacheConfig) *Cache {
	t.Helper()
	if cfg.MaxAge == 0 {
		cfg.MaxAge = time.Hour
	}
	c, err := OpenCache(cfg)
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// filepathWalk visits every regular file under dir, root-RELATIVE, in a stable
// order. It is the test's OWN measurement of the directory — deliberately not
// the cache's walker — so the agreement arm compares two independent readings.
func filepathWalk(dir string, visit func(rel string, size int64)) error {
	var paths []string
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		paths = append(paths, p)
		return nil
	}); err != nil {
		return err
	}
	sort.Strings(paths)
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		visit(filepath.ToSlash(rel), info.Size())
	}
	return nil
}

// mustJSON marshals a value whose byte length the arm compares against a figure
// the cache reports.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// entryHashes returns the hashes of the cache's live path entries, so a test can
// hold every one of them without reaching into the live maps.
func (c *Cache) entryHashes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]bool{}
	out := make([]string, 0, len(c.entries))
	for _, e := range c.entries {
		if seen[e.Hash] {
			continue
		}
		seen[e.Hash] = true
		out = append(out, e.Hash)
	}
	sort.Strings(out)
	return out
}

// newestEntryPath returns the path of the entry touched most recently. A test
// uses it to stage against a path that is certainly still cached, without
// predicting which entry the eviction order reclaimed.
func (c *Cache) newestEntryPath() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	best, found := "", false
	var bestAt int64
	for p, e := range c.entries {
		if !found || e.LastHitMS > bestAt || (e.LastHitMS == bestAt && p < best) {
			best, bestAt, found = p, e.LastHitMS, true
		}
	}
	return best, found
}

// osRemoveAllContents empties a directory without removing the directory itself
// (the cache keeps its handle, and the point is a directory whose contents the
// walker can no longer count).
func osRemoveAllContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// The harness: a client whose invalidation answers the test scripts.
// ---------------------------------------------------------------------------

// scriptedChannel is a fake E-4 surface that serves exactly what an arm needs to
// happen next: a capability document with a KNOWN watch block (so the
// passthrough arm has values to compare), and a queue of scripted answers to the
// `events` op and to stream attempts (`watch`).
type scriptedChannel struct {
	t *testing.T

	mu        sync.Mutex
	events    [][]Event // one batch per poll
	watch     []func(w http.ResponseWriter, done chan struct{})
	calls     int
	lastSince string
	lastOp    string
	// closed is shut down at the end of the test so a HELD-OPEN response cannot
	// make httptest.Server.Close block: a handler parked on a client that will
	// never read again is exactly the teardown trap the response owner must
	// close, and this is that owner.
	closed chan struct{}
}

// answerEvents pops the next scripted batch. An empty queue answers "nothing
// moved" — which is exactly what a quiet tree answers.
func (s *scriptedChannel) answerEvents() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		return nil
	}
	batch := s.events[0]
	s.events = s.events[1:]
	return batch
}

func (s *scriptedChannel) pushEvents(batches ...[]Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, batches...)
}

func (s *scriptedChannel) pushWatch(fn func(w http.ResponseWriter, done chan struct{})) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watch = append(s.watch, fn)
}

func (s *scriptedChannel) popWatch() func(http.ResponseWriter, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.watch) == 0 {
		return nil
	}
	fn := s.watch[0]
	s.watch = s.watch[1:]
	return fn
}

func (s *scriptedChannel) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
}

func (s *scriptedChannel) callsMade() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// The values the fake capability document PUBLISHES. They are deliberately
// distinctive: the passthrough arm asserts the client reports THESE, so a client
// that invented a plausible zero instead of copying the server's figure fails.
const (
	fakeWatchState      = "overflow"
	fakeWatchBackend    = "inotify"
	fakeOverflows       = 7
	fakeUnvouched       = 3
	fakeRescans         = 2
	fakeInstallFailures = 1
	fakeBackendErrors   = 5
	fakeHeartbeats      = 11
	fakeLoopTicks       = 99
	fakeLastEventAgeMS  = 1234
	fakeDirsDesired     = 4
	fakeDirsWatched     = 3
	fakeMissingCount    = 1
	fakeHeartbeatMS     = 30000
	// fakeMaxEventBytes is the per-frame BYTE bound the document declares
	// (BFS-062). It is the largest bound the surface's own knob table permits
	// (invalidation.MaxPushMaxEventBytes), which is also what makes it the
	// interesting value: the derived reader cap lands exactly on the consumer's
	// ceiling, so an arm that sized the reader from a constant of its own would
	// fail rather than pass.
	fakeMaxEventBytes   = 8 << 20
	fakeDroppedReason   = "the kernel reports one overflow marker, not how many events it dropped"
	fakeCountersReason  = "no watcher has run on this target, so no counter has ever moved"
	fakeUnvouchedReason = "reader stalled"
)

func (s *scriptedChannel) capabilityDocument() string {
	// The E-4 envelope: the document lives under `result`, exactly as the landed
	// surface serves it (CapsDoc decodes `result.capabilities`).
	return `{
	  "ok": true,
	  "result": {"capabilities": {
	    "surface": "fake", "document_version": 1, "build": "probe",
	    "extensions": {
	      "watch": {
	        "name": "X-Bunker-Op: watch", "v": 1, "mode": "poll",
	        "modes": {"push": "inotify->stream", "poll": "X-Bunker-Op: events"},
	        "heartbeat_ms": ` + strconv.Itoa(fakeHeartbeatMS) + `,
	        "max_paths_per_event": 4096,
	        "max_event_bytes": ` + strconv.Itoa(fakeMaxEventBytes) + `,
	        "state": "` + fakeWatchState + `",
	        "blocks_push": true,
	        "backend": "` + fakeWatchBackend + `",
	        "reason": null,
	        "detail": "the served root is an overlay mount",
	        "target": {"mount_type": "overlay", "mount_point": "/", "network_backed": false,
	                   "boundary_split": "unknown",
	                   "boundary_split_reason": "the mount table could not be read, so the served root's superblock relation to its writers is unknown rather than false"},
	        "coverage": {"directories_desired": ` + strconv.Itoa(fakeDirsDesired) + `, "directories_watched": ` + strconv.Itoa(fakeDirsWatched) + `,
	                     "missing_count": ` + strconv.Itoa(fakeMissingCount) + `, "missing": ["b"], "complete": false, "headroom": 100},
	        "counters": {"overflows_total": ` + strconv.Itoa(fakeOverflows) + `, "unvouched_total": ` + strconv.Itoa(fakeUnvouched) + `,
	                     "rescans_total": ` + strconv.Itoa(fakeRescans) + `, "install_failures_total": ` + strconv.Itoa(fakeInstallFailures) + `,
	                     "backend_errors_total": ` + strconv.Itoa(fakeBackendErrors) + `, "overflow_dropped_events": null,
	                     "overflow_dropped_reason": "` + fakeDroppedReason + `",
	                     "unvouched_reason": "` + fakeUnvouchedReason + `",
	                     "last_event_age_ms": ` + strconv.Itoa(fakeLastEventAgeMS) + `, "changed_since": "2026-09-27T00:00:00Z",
	                     "heartbeats_total": ` + strconv.Itoa(fakeHeartbeats) + `, "event_loop_ticks": ` + strconv.Itoa(fakeLoopTicks) + `,
	                     "watches_added": ` + strconv.Itoa(fakeDirsWatched) + `},
	        "liveness": {"vouched": true, "stalled": false, "liveness_source": "the watcher's own heartbeat, never the absence of events"}
	      },
	      "rev": {"kind": "counter"}
	    },
	    "degradations": []
	  }}
	}`
}

// serve builds the fake surface. The `events` op is scripted; `watch` is scripted
// per attempt; a 500 arm and a stalled arm are expressible without the session
// having to fake a broken socket.
func (s *scriptedChannel) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls++
		op := r.Header.Get("X-Bunker-Op")
		s.lastOp = op
		s.mu.Unlock()
		if r.Method == http.MethodOptions {
			w.Header().Set("DAV", "1")
			w.WriteHeader(http.StatusOK)
			return
		}
		switch op {
		case "capabilities":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, s.capabilityDocument())
		case "events":
			raw, _ := io.ReadAll(r.Body)
			var args struct {
				SinceSeq *int64 `json:"since_seq"`
			}
			_ = json.Unmarshal(raw, &args)
			s.mu.Lock()
			s.lastSince = "none"
			if args.SinceSeq != nil {
				s.lastSince = strconv.FormatInt(*args.SinceSeq, 10)
			}
			s.mu.Unlock()
			res := s.answerEvents()
			// A batch of -1 means "the surface is failing": 500, no envelope.
			if len(res) == 1 && res[0].Seq == -1 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"ok":false,"verdict":"server_error"}`)
				return
			}
			body, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"events": res}})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		case "watch":
			fn := s.popWatch()
			if fn == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotImplemented)
				_, _ = io.WriteString(w, `{"ok":false,"verdict":"capability_unavailable","error":{"capability":"watch","scope":"target","mode":"poll","detail":"no watcher is established on this target"}}`)
				return
			}
			w.Header().Set("Accept", "application/x-ndjson")
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			// The headers must reach the wire BEFORE the scripted body decides
			// what to do: a client that never receives a 200 never reaches its
			// idle rule, and the stall arm would be measuring the wrong thing.
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			done := make(chan struct{}, 1)
			go fn(w, done)
			select {
			case <-done: // the scripted body ENDED the response: a channel end
			case <-r.Context().Done():
			case <-s.closed: // the test is over: never park a handler on a dead client
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
		}
	}))
}

// newScriptedInvalidator wires a real client against the fake surface, plus the
// mount's own wiring (drop / resync / re-observation), so the arms measure the
// objects the mount runs.
func newScriptedInvalidator(t *testing.T, s *scriptedChannel, opt InvalidateOptions) (*Client, *Invalidator) {
	t.Helper()
	s.closed = make(chan struct{})
	srv := s.serve()
	t.Cleanup(s.shutdown)
	t.Cleanup(srv.Close)
	c, err := NewClient(Options{BaseURL: srv.URL, Concurrency: 4, OpTimeout: 5 * time.Second, BindTimeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, oerr := c.CapabilitiesDoc(context.Background()); oerr != nil {
		t.Fatalf("capability document: %v", oerr)
	}
	var inv *Invalidator
	opt.OnResync = func(reason string) {
		// The mount always re-observes on a resync. The fake mints nothing, so the
		// client adopts the cursor the `overflow` notice named — the PROPFIND
		// case of BFS-063.
		inv.Observed(0, false)
	}
	inv = NewInvalidator(c, opt)
	return c, inv
}

// ---------------------------------------------------------------------------
// The figure census walk.
// ---------------------------------------------------------------------------

// figures walks a status document and returns every NUMERIC leaf under the given
// prefixes as a dotted path. Scoped to the blocks this row owns: the cache (the
// invalidation path's storage), the invalidation record itself, and — since
// BFS-031 — the mount's OWN state, which is the storage the cache bound does NOT
// name and which therefore has to be bounded and visible in its own right. The
// other blocks (conflicts, read_bound, write_shape, refusal_holds, transport,
// snapshot) are other rows' subjects with their own drivers.
func figures(t *testing.T, st Status) map[string]float64 {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	out := map[string]float64{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch tv := v.(type) {
		case map[string]any:
			for k, sub := range tv {
				walk(prefix+"."+k, sub)
			}
		case float64:
			out[prefix] = tv
		case bool:
			// A boolean figure is reported as a 0/1 measurement for the census:
			// `within_bound: false` is a MEASUREMENT (the window was exceeded),
			// not an absence, and the census must see it.
			if tv {
				out[prefix] = 1
			} else {
				out[prefix] = 0
			}
		}
	}
	for _, block := range []string{"cache", "invalidation", "state"} {
		if sub, ok := doc[block]; ok {
			walk(block, sub)
		}
	}
	return out
}

// reasons collects every string-valued leaf whose KEY ends in "reason" or
// "why" — the fields whose job is to explain an absent figure.
func reasons(t *testing.T, st Status) map[string]string {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	out := map[string]string{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch tv := v.(type) {
		case map[string]any:
			for k, sub := range tv {
				walk(prefix+"."+k, sub)
			}
		case string:
			if strings.HasSuffix(prefix, "reason") || strings.HasSuffix(prefix, "why") {
				out[prefix] = tv
			}
		}
	}
	for _, block := range []string{"cache", "invalidation", "state"} {
		if sub, ok := doc[block]; ok {
			walk(block, sub)
		}
	}
	return out
}

// kind is what the census table claims about one figure.
type kind string

const (
	// kindMoves — the workload must move it (or make it appear, for a figure
	// that only exists once there is something to measure).
	kindMoves kind = "moves"
	// kindBound — a DECLARED bound or configured value: it must be present and
	// equal the expected value. A bound that moves under a workload is not a
	// bound, so this assertion also catches a bound accidentally counting.
	kindBound kind = "bound"
	// kindPassthrough — the server's own figure, reported verbatim. The expected
	// value is what the fake document published.
	kindPassthrough kind = "passthrough"
	// kindAppears — a measured figure that must be PRESENT after the workload
	// without any claim about movement: its value is whatever the measurement
	// found (a class with no files is a measured 0, and the independent-walk arm
	// is what proves that 0 is real rather than dead).
	kindAppears kind = "appears"
)

type figureSpec struct {
	kind kind
	want float64 // kindBound / kindPassthrough
	why  string
}

// TestEveryFigureInTheStatusRecordMovesOrIsExplained is THE acceptance cell.
func TestEveryFigureInTheStatusRecordMovesOrIsExplained(t *testing.T) {
	cache := newTestCacheWithBounds(t, CacheConfig{
		Dir: t.TempDir(), MaxBytes: 256 << 10, MaxEntryBytes: 64, MaxEntries: 6, MaxInFlight: 1, MaxAge: time.Hour,
		// A fresh walk per Stats() read: the census compares a before/after pair,
		// and a reused sample would be the same measurement twice.
		DirMeasureInterval: -1,
	})

	// Half the census is the CACHE's: the invalidation path's storage bounds.
	// Driven first, so the before/after pair straddles real work.
	before := map[string]float64{}
	after := map[string]float64{}
	// The mount's own directory for the state half of the census (BFS-031): the
	// status document and the refusal log live here, NOT in the cache directory,
	// and the write-buffer spill lands here too.
	censusStateDir := t.TempDir()

	// --- the invalidator's half ------------------------------------------------
	s := &scriptedChannel{t: t}
	// Attempt 1: a stream that speaks once and then ends (H-1's channel end).
	// Signalling `done` is what ENDS the response, so the client reads a clean
	// EOF on a live context — the channel end this arm needs.
	s.pushWatch(func(w http.ResponseWriter, done chan struct{}) {
		if f, ok := w.(http.Flusher); ok {
			_, _ = io.WriteString(w, `{"seq":1,"event":"heartbeat"}`+"\n")
			f.Flush()
		}
		done <- struct{}{}
	})
	// Attempt 2: a stream that says nothing at all — the idle rule's stall. The
	// response is HELD OPEN (nothing is signalled), because a closed body is a
	// channel END, and the arm under test is the silence, not the end.
	s.pushWatch(func(w http.ResponseWriter, done chan struct{}) {
		time.Sleep(2 * time.Second)
	})
	_, inv := newScriptedInvalidator(t, s, InvalidateOptions{
		Mode:         "auto",
		PollInterval: 20 * time.Millisecond,
		IdleTimeout:  150 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stBefore := Status{Mount: "probe"}
	stBefore.Cache = cache.Stats()
	stBefore.Invalidation = inv.State()
	stBefore.Invalidation.Refresh = RefreshFromCache(stBefore.Cache)
	before = figures(t, stBefore)

	// 1. The pushed channel speaks once, then ends: stream_ends + reconnect, and
	//    the line moves the liveness figures.
	go func() { _ = inv.Run(ctx) }()
	if !waitFor(3*time.Second, func() bool { return inv.State().StreamEnds > 0 }) {
		t.Fatalf("the scripted channel end was never observed: %+v", inv.State())
	}
	if !waitFor(3*time.Second, func() bool { return inv.State().Reconnects > 0 }) {
		t.Fatalf("the reconnect after a channel end was never counted: %+v", inv.State())
	}
	// 2. The second attempt stalls on silence: the idle rule fires, the stalled
	//    state is recorded, and the declared poll takes over.
	if !waitFor(5*time.Second, func() bool { return inv.State().IdleFallbacks > 0 }) {
		t.Fatalf("the idle rule never fired on a silent stream: %+v", inv.State())
	}
	if st := inv.State(); st.Liveness == nil || !st.Liveness.Stalled {
		t.Fatalf("the idle rule fired but the record does not report the channel as stalled: %+v", st.Liveness)
	}

	// 3. The declared poll now carries the channel, and it delivers: two change
	//    events (one of them carrying several paths) and a heartbeat.
	//
	//    The seqs are CONTIGUOUS with the cursor the client holds — 1, the
	//    stream's own heartbeat (§3.2 row 2), because a heartbeat advances the
	//    cursor like any other line (BFS-061, §3.1 R-1). They have to be: a tail
	//    whose first seq leaves a hole is a gap the client is REQUIRED to notice
	//    and resync on (§3.3 R-3), and a real server answers that case with the
	//    `overflow` marker the landed poll sends (BFS-063) rather than a bare
	//    tail. This arm is about the counters moving when the poll delivers, not
	//    about the marker; the deliberate gap and the overflow are steps 4 and 5.
	//    (Before BFS-061 this fixture could start at 10: the stream's heartbeat
	//    was consumed and never recorded, so the client's cursor was 0 and the
	//    non-zero-cursor guard swallowed the hole. That silent gap is exactly
	//    what BFS-061 removes.)
	s.pushEvents([]Event{
		{Seq: 2, Event: EventInvalidate, Paths: []string{"a.go"}},
		{Seq: 3, Event: EventInvalidate, Paths: []string{"b.go", "c.go"}},
		{Seq: 4, Event: EventHeartbeat},
	})
	if !waitFor(5*time.Second, func() bool { return inv.State().DroppedPaths >= 3 }) {
		t.Fatalf("the polled events never landed: %+v", inv.State())
	}
	if got := inv.State().Mechanism; got != MechanismEvents {
		t.Fatalf("mechanism=%s, want %s (the pushed form is not served by the fake)", got, MechanismEvents)
	}

	// 4. A sequence GAP: the missing range is never re-requested, so it is a
	//    resync — the counted one.
	s.pushEvents([]Event{{Seq: 99, Event: EventInvalidate, Paths: []string{"d.go"}}})
	if !waitFor(5*time.Second, func() bool { return inv.State().Gaps > 0 }) {
		t.Fatalf("the sequence gap was not counted: %+v", inv.State())
	}

	// 5. An OVERFLOW: knowledge lost, counted, and the resync that follows.
	s.pushEvents([]Event{{Seq: 100, Event: EventOverflow}})
	if !waitFor(5*time.Second, func() bool { return inv.State().Resyncs > 1 }) {
		t.Fatalf("the overflow did not produce a second resync: %+v", inv.State())
	}

	// 6. The surface fails: an attempt that produced NO answer is a failure, and
	//    it must not read as a quiet channel.
	s.pushEvents([]Event{{Seq: -1}})
	if !waitFor(5*time.Second, func() bool { return inv.State().Failures > 0 }) {
		t.Fatalf("a 500 from the surface was not counted as a failure: %+v", inv.State())
	}
	if inv.State().LastFailure == "" {
		t.Fatal("a counted failure must name itself in last_failure")
	}
	stAfter := Status{Mount: "probe"}
	// --- the cache's half ------------------------------------------------------
	// The STAGED-REFRESH flow first, on an empty cache, because each of its four
	// refusals needs a state the later fill would have taken away: one committed,
	// one aborted, one refused for NO SLOT (the window is one wide) and one
	// refused for NO ROOM (its declared size cannot fit the bound).
	stg, err := cache.Stage("staged.go", HashBytes([]byte("staged")), int64(len("staged")))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := stg.Write([]byte("staged")); err != nil {
		t.Fatalf("stage write: %v", err)
	}
	if _, err := stg.Commit(); err != nil {
		t.Fatalf("stage commit: %v", err)
	}
	ab, err := cache.Stage("aborted.go", HashBytes([]byte("aborted")), int64(len("aborted")))
	if err != nil {
		t.Fatalf("stage (abort arm): %v", err)
	}
	_ = ab.Abort()
	holding, err := cache.Stage("holding.go", HashBytes([]byte("holding")), int64(len("holding")))
	if err != nil {
		t.Fatalf("stage (slot arm): %v", err)
	}
	if _, err := cache.Stage("no-slot.go", HashBytes([]byte("no-slot")), int64(len("no-slot"))); err == nil {
		t.Fatal("the second concurrent stage must be refused for NO SLOT at MaxInFlight=1")
	}
	_ = holding.Abort()
	if _, err := cache.Stage("no-room.go", HashBytes([]byte("no-room")), 1<<20); err == nil {
		t.Fatal("a stage whose declared size cannot fit must be refused for NO ROOM")
	}

	// Then the read-path figures: stores, a hit, a miss, the over-cap read
	// (BFS-032's site), an eviction under the entry bound, and a cache whose
	// every entry is pinned REFUSING an insert rather than evicting one.
	for _, p := range []string{"a.go", "b.go", "c.go", "d.go", "e.go"} {
		body := []byte(p + "-body")
		if _, err := cache.Insert(p, HashBytes(body), body); err != nil {
			t.Fatalf("insert %s: %v", p, err)
		}
	}
	cache.Get("a.go", HashBytes([]byte("a.go-body")))                  // hit
	cache.Get("never-there.go", HashBytes([]byte("never-there-body"))) // miss
	// The over-cap read: 82 bytes against a 64-byte cap. It is refused a blob and
	// COUNTED, which is the whole of BFS-032.
	over := []byte("X-REPLACEMENT-CONTENT-0123456789-abcdefghijklmnopqrstuvwxyz-ABCDEFGHIJKLMNOPQRSTUV")
	cache.AdmitRead("big.go", HashBytes(over), over)
	// One more entry than the entry bound allows: an unpinned entry is evicted to
	// make room, so the eviction is counted (and nothing a reader holds is).
	last := []byte("f.go-body")
	if _, err := cache.Insert("f.go", HashBytes(last), last); err != nil {
		t.Fatalf("insert f.go: %v", err)
	}
	// Now pin every surviving entry: the next insert has nowhere to go, so the
	// bound REFUSES it — and a refusal is counted, never silently dropped.
	for _, e := range cache.entryHashes() {
		cache.Pin(e)
	}
	refused := []byte("g.go-body")
	if _, err := cache.Insert("g.go", HashBytes(refused), refused); err != nil {
		t.Fatalf("insert g.go: %v", err)
	}
	// And leave ONE stage open, so the in-flight account (BFS-038's admission
	// reservation) is non-zero in the reported document as well as on disk. It
	// stages a path that is certainly still cached (an in-flight refresh is a
	// refresh OF a cached path, and a new path would be refused by the entry
	// bound the arm above just filled).
	refreshPath, ok := cache.newestEntryPath()
	if !ok {
		t.Fatal("no cached entry to refresh")
	}
	open, err := cache.Stage(refreshPath, HashBytes([]byte("refreshed-a")), int64(len("refreshed-a")))
	if err != nil {
		t.Fatalf("open stage on %s: %v", refreshPath, err)
	}
	if _, err := open.Write([]byte("refreshed-a")); err != nil {
		t.Fatalf("open stage write: %v", err)
	}
	t.Cleanup(func() { _ = open.Abort() })

	stAfter.Cache = cache.Stats()
	stAfter.Invalidation = inv.State()
	stAfter.Invalidation.Refresh = RefreshFromCache(stAfter.Cache)
	// --- the mount's own state (BFS-031) ---------------------------------------
	// The cache directory's bound cannot be true unless the things that are not
	// the cache are bounded too, so the census covers them: a status document, a
	// refusal log that ROTATES (its cap lowered for the arm, so the enforcement's
	// own counter moves without 4 MiB of appends), and a write-buffer spill.
	{
		st := Status{Mount: "census-state", Mode: "poll", Endpoint: "http://127.0.0.1:1/dav"}
		if err := WriteStatus(censusStateDir, st); err != nil {
			t.Fatalf("write status: %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := AppendConflict(censusStateDir, Conflict{Path: fmt.Sprintf("c/%d", i), Code: "hash_mismatch", Detail: strings.Repeat("d", 300)}); err != nil {
				t.Fatalf("append conflict: %v", err)
			}
		}
		stBefore.State = MeasureState(censusStateDir, stBefore.Cache.DirBytes, stBefore.Cache.MaxBytes, censusSpillBudget)
		before = figures(t, stBefore)

		// The after phase: a bigger document, a log driven past its cap (so the
		// rotation's counter moves), and a spill file. The append count is sized
		// by what the arm needs — the rotation counter MOVING — not by a round
		// number: at the 4096 B cap and ~380 B per entry the log retains ~9
		// lines, so 18 appends rotate it at least twice. Each append pays a real
		// fsync plus a rotate (read, rewrite, rename) on the suite's scratch
		// disk, ~2 s each under fleet-host load (measured on the same primitives
		// in TestTheRefusalLogIsBoundedAndTheDropIsCounted), so the fixed count
		// is a package-timeout liability, not extra coverage.
		restore := swapInt64(&ConflictsMaxBytes, 4096)
		for i := 0; i < 18; i++ {
			if err := AppendConflict(censusStateDir, Conflict{Path: fmt.Sprintf("c/after-%d", i), Code: "hash_mismatch", Detail: strings.Repeat("D", 300)}); err != nil {
				t.Fatalf("append conflict: %v", err)
			}
		}
		restore()
		if err := os.WriteFile(filepath.Join(censusStateDir, WriteBufPrefix+"1"), []byte(strings.Repeat("w", 512)), 0o600); err != nil {
			t.Fatalf("spill: %v", err)
		}
		bigger := st
		bigger.ReducedReason = strings.Repeat("r", 1024)
		if err := WriteStatus(censusStateDir, bigger); err != nil {
			t.Fatalf("write bigger status: %v", err)
		}
		stAfter.State = MeasureState(censusStateDir, stAfter.Cache.DirBytes, stAfter.Cache.MaxBytes, censusSpillBudget)
	}
	after = figures(t, stAfter)

	// --- the census ------------------------------------------------------------
	table := censusTable()
	union := map[string]bool{}
	for k := range before {
		union[k] = true
	}
	for k := range after {
		union[k] = true
	}
	var unclassified []string
	for k := range union {
		if _, ok := table[k]; !ok {
			unclassified = append(unclassified, k)
		}
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Fatalf("the record publishes %d figure(s) this census does not classify, so nothing proves they can move (BFS-032's shape):\n  %s\n"+
			"add each to censusTable() as kindMoves (with a workload that moves it) or as a bound/passthrough with its reason",
			len(unclassified), strings.Join(unclassified, "\n  "))
	}

	// Every figure's own assertion, as a subtest so a failure names it.
	var moved []string
	for path, spec := range table {
		path, spec := path, spec
		t.Run("figure/"+path, func(t *testing.T) {
			b, hadBefore := before[path]
			a, present := after[path]
			switch spec.kind {
			case kindMoves:
				if !present {
					t.Fatalf("%s is not in the record after the workload, so nothing moved it: %v", path, after)
				}
				if !hadBefore {
					return // appeared: a figure that only exists once there is something to measure
				}
				if a <= b {
					t.Fatalf("%s did not move under a workload that must move it: before=%v after=%v", path, b, a)
				}
			case kindBound, kindPassthrough:
				if !present {
					t.Fatalf("%s is absent, so the bound/passthrough is not reportable: %v", path, after)
				}
				if a != spec.want {
					t.Fatalf("%s = %v, want %v (%s)", path, a, spec.want, spec.why)
				}
			case kindAppears:
				if !present {
					t.Fatalf("%s is absent after the workload, so no measurement reached the record (%s)", path, spec.why)
				}
			}
		})
		if spec.kind == kindMoves {
			if _, present := after[path]; present {
				moved = append(moved, path)
			}
		}
	}
	sort.Strings(moved)
	t.Logf("figures proven present: %d of %d classified; moved/appeared: %d", len(after), len(table), len(moved))
	for _, p := range moved {
		t.Logf("  moved %-46s %v", p, after[p])
	}

	// A counted failure must name itself, and the mechanism must have asked at
	// least once — both are checked while the channel is still running, so the
	// comparison is an inequality rather than an equality: the invalidator's own
	// goroutine keeps polling, and an equality here would be a race dressed up as
	// an assertion. The "a declared capability refusal is NOT a failure" property
	// has its own deterministic arm below, where the surface answers nothing but
	// that refusal.
	stFinal := inv.State()
	if stFinal.Failures < 1 || stFinal.LastFailure == "" {
		t.Fatalf("a failed attempt must be counted AND named: failures=%d last_failure=%q", stFinal.Failures, stFinal.LastFailure)
	}
	if stFinal.Requests < 1 {
		t.Fatalf("requests_total=%d: the channel's own attempts are not counted", stFinal.Requests)
	}

	// --- the null-reason rule --------------------------------------------------
	// Two different kinds of string end in `_reason` and they must not be
	// conflated:
	//
	//   - CLIENT-AUTHORED ABSENCE REASONS: this client's own explanation for a
	//     figure it cannot report. These must open with one of the four
	//     vocabulary classes, because an unexplained null is junk.
	//   - SERVER-AUTHORED reasons that travel VERBATIM (the server's own
	//     refusal/absence reason for its watcher): rewriting them into this
	//     client's vocabulary would lose the one fact only the server has. They
	//     are asserted against the fake document instead — which is a stronger
	//     check than a class prefix, because it proves the client did not
	//     paraphrase the server.
	for path, text := range reasons(t, stAfter) {
		path, text := path, text
		t.Run("reason/"+path, func(t *testing.T) {
			if strings.TrimSpace(text) == "" {
				t.Fatalf("%s is empty: a null must carry a REASON", path)
			}
			if want, passthrough := verbatimServerReasons[path]; passthrough {
				if text != want {
					t.Fatalf("%s must be the server's own words, not a paraphrase: got %q want %q", path, text, want)
				}
				return
			}
			if absenceReasons[path] {
				if !hasReasonClass(text) {
					t.Fatalf("%s = %q does not open with one of the vocabulary classes (disabled | unknown | not_published | no_sample)", path, text)
				}
				return
			}
			if operatorNotes[path] {
				// The current mode's own explanation ("why am I polling?"). It is
				// a statement about a MECHANISM that exists, not a reason a
				// figure is absent, so the vocabulary does not apply — but an
				// empty one would still be a claim without a reason, which the
				// non-empty check above already refuses.
				return
			}
			t.Fatalf("%s is a reason field the census does not classify: add it to absenceReasons (it is this client's own explanation) or to verbatimServerReasons (the server's own words)", path)
		})
	}
}

// absenceReasons are the fields where THIS client explains why a figure is
// absent. Every one must open with a vocabulary class.
var absenceReasons = map[string]bool{
	"cache.dir_bytes_reason":                true,
	"state.reason":                          true,
	"invalidation.server_reason":            true,
	"invalidation.content_age_reason":       true,
	"invalidation.content_age.bound_reason": true,
	"invalidation.liveness_reason":          true,
	"invalidation.refresh.absent_reason":    true,
}

// operatorNotes are the mode's own explanations: statements about a mechanism
// that IS in force, not reasons a figure is missing.
var operatorNotes = map[string]bool{
	"invalidation.reason": true,
}

// verbatimServerReasons are the fields that carry the SERVER's own words. They
// are checked against the document the fake published, not against a class: the
// client's job is to repeat them, not to re-word them.
var verbatimServerReasons = map[string]string{
	"invalidation.server.overflow_dropped_reason": fakeDroppedReason,
	"invalidation.server.unvouched_reason":        fakeUnvouchedReason,
}

// hasReasonClass reports whether a reason string opens with one of the four
// vocabulary classes. The check is on the CLASS, not on prose: a reason that is
// a sentence with no class in front of it is the unexplained null the row
// forbids.
func hasReasonClass(text string) bool {
	for _, c := range []string{ReasonDisabled, ReasonUnknown, ReasonNotPublished, ReasonNoSample} {
		if strings.HasPrefix(text, c+":") || text == c {
			return true
		}
	}
	return false
}

// censusSpillBudget is the write-buffer budget the census measures its state
// with: the mount reports open buffered handles × the per-handle bound, and a
// fixed number here keeps the footprint's bound deterministic.
const censusSpillBudget = 512

// censusTable is THE inventory: every figure the record publishes under
// `cache.*` and `invalidation.*`, what it is, and what moves it. It is written
// out rather than derived, because the point is that a human (and the test) can
// read which figures are DRIVEN and which are declared — and because a figure
// missing from this table fails the test.
func censusTable() map[string]figureSpec {
	return map[string]figureSpec{
		// ---- the cache bounds -------------------------------------------------
		"cache.max_bytes":       {kindBound, 256 << 10, "the declared byte bound (--cache-max-size)"},
		"cache.max_entry_bytes": {kindBound, 64, "the declared per-entry cap: the bound whose refusal the census below counts"},
		"cache.max_age_ms":      {kindBound, 3600000, "the backstop TTL one entry expires against (--cache-max-age)"},
		"cache.max_entries":     {kindBound, 6, "the declared entry bound (BFS-031: bytes alone do not bound a directory)"},
		"cache.max_inflight":    {kindBound, 1, "the declared staged-window width (BFS-038's reservation bound)"},
		// ---- the cache figures that move --------------------------------------
		"cache.blobs_bytes":            {kindMoves, 0, "a stored blob adds its bytes"},
		"cache.index_bytes":            {kindMoves, 0, "an index record is published with each entry"},
		"cache.used_bytes":             {kindMoves, 0, "the published figure: blobs + the serialised index"},
		"cache.entries":                {kindMoves, 0, "one per cached path"},
		"cache.blobs":                  {kindMoves, 0, "content-addressed: one blob per distinct content"},
		"cache.hits":                   {kindMoves, 0, "a read of a path the cache holds"},
		"cache.misses":                 {kindMoves, 0, "a lookup for a path it does not hold"},
		"cache.evictions_total":        {kindMoves, 0, "an insert that must make room under the bounds"},
		"cache.bypass_events":          {kindMoves, 0, "an insert refused for NO ROOM: the bound refusing, not evicting"},
		"cache.oversize_bypasses":      {kindMoves, 0, "a read whose content is over the per-entry cap — BFS-032's counter, now reachable from the live path"},
		"cache.pinned_blobs":           {kindMoves, 0, "a blob a live reader holds"},
		"cache.in_flight_bytes":        {kindMoves, 0, "the bytes a staged refresh holds unpublished (the ADMISSION account)"},
		"cache.reserved_bytes":         {kindMoves, 0, "published + in-flight + the index they will publish: the directory's real peak"},
		"cache.staged_blobs":           {kindMoves, 0, "the width of the refresh window in use"},
		"cache.staged_started_total":   {kindMoves, 0, "a refresh admitted to the staged window"},
		"cache.staged_committed_total": {kindMoves, 0, "a refresh that published"},
		"cache.staged_aborted_total":   {kindMoves, 0, "a refresh that discarded its blob"},
		"cache.staged_no_slot_total":   {kindMoves, 0, "a refresh refused because every stage slot was taken"},
		"cache.staged_no_room_total":   {kindMoves, 0, "a refresh refused because the bound left no room"},
		// ---- the cache's refusal census (by reason) ---------------------------
		"cache.bypass_reasons.over_entry_cap":        {kindMoves, 0, "the live read path's per-entry-cap refusal"},
		"cache.bypass_reasons.insert_over_entry_cap": {kindAppears, 0, "Insert's own cap branch: reachable from a direct caller, and 0 from the mount because the read path decides first"},
		"cache.bypass_reasons.no_room":               {kindMoves, 0, "the bound refusing an insert after eviction"},
		"cache.bypass_reasons.cache_disabled":        {kindAppears, 0, "--cache-max-size 0: a measured 0 for a cache that is switched on"},
		// ---- the independent measurement --------------------------------------
		"cache.dir_bytes":                    {kindMoves, 0, "the filesystem walk: grows with every file the cache writes"},
		"cache.dir_peak_bytes":               {kindMoves, 0, "the directory's PEAK (blobs + staged booking + TWO index copies + measured foreign bytes): the figure the byte bound is enforced against (BFS-031); it moves when an entry is published"},
		"cache.dir_unaccounted_bytes":        {kindMoves, 0, "dir_bytes − used_bytes: the delta the published figure does not count (BFS-031's shape, named)"},
		"cache.dir_measured_age_ms":          {kindAppears, 0, "how old the sample is; present once a walk has happened"},
		"cache.dir_bytes_by_class.index":     {kindAppears, 0, "index.json — a measured value; the walk arm proves a 0 is real"},
		"cache.dir_bytes_by_class.blobs":     {kindMoves, 0, "published blobs: moves when a blob is written"},
		"cache.dir_bytes_by_class.staged":    {kindAppears, 0, "staged blobs on disk at sample time (0 when none is in flight)"},
		"cache.dir_bytes_by_class.orphan":    {kindAppears, 0, "blob files no index entry references (0 in a healthy cache)"},
		"cache.dir_bytes_by_class.status":    {kindAppears, 0, "status.json: 0 in the CACHE directory since BFS-031 moved the mount's state out of it; the class stays so a foreign status file would still be attributed"},
		"cache.dir_bytes_by_class.conflicts": {kindAppears, 0, "conflicts.jsonl: 0 in the CACHE directory since BFS-031 moved the refusal log out of it"},
		"cache.dir_bytes_by_class.other":     {kindAppears, 0, "anything else in the directory"},
		// ---- the mount's OWN state (BFS-031): the storage the cache bound does
		// NOT name, with its own bound and its own enforcement.
		"state.bytes":                   {kindMoves, 0, "the mount directory's own bytes: grows with every status write, refusal and write-buffer spill"},
		"state.max_bytes":               {kindBound, float64(StateMaxBytes()), "the declared state bound (the log cap + the document cap)"},
		"state.status_bytes":            {kindMoves, 0, "the status document, measured: grows when the document carries more"},
		"state.status_max_bytes":        {kindBound, float64(StatusMaxBytes), "the declared cap of the status document"},
		"state.conflicts_bytes":         {kindMoves, 0, "the refusal log, measured: grows with refusals (and is rotated back under the cap)"},
		"state.conflicts_max_bytes":     {kindBound, float64(ConflictsMaxBytes), "the declared cap of the refusal log"},
		"state.conflicts_dropped_total": {kindMoves, 0, "THE ENFORCEMENT'S COUNTER: entries the log's bound forced out, kept durably so a bounded log is still a record"},
		"state.spill_bytes":             {kindMoves, 0, "one open write handle's spill file on disk"},
		"state.footprint_bytes":         {kindMoves, 0, "the cache directory plus the state: the mount's whole local footprint"},
		"state.footprint_max_bytes":     {kindBound, float64(256<<10 + StateMaxBytes() + censusSpillBudget), "the footprint's bound: the cache bound + the state bound + the write-buffer budget the open handles imply"},
		// ---- the invalidation record ------------------------------------------
		"invalidation.seq":                  {kindMoves, 0, "the journal cursor advances on every applied event"},
		"invalidation.events_total":         {kindMoves, 0, "an applied event (a heartbeat counts as one)"},
		"invalidation.paths_dropped_total":  {kindMoves, 0, "an invalidate event carries paths"},
		"invalidation.resyncs_from_gap":     {kindMoves, 0, "a sequence gap in the applied events"},
		"invalidation.resyncs_total":        {kindMoves, 0, "a gap, an overflow, or a tree change"},
		"invalidation.stream_ends_total":    {kindMoves, 0, "a channel end on a live context"},
		"invalidation.reconnects_total":     {kindMoves, 0, "a reconnect attempt after a fault"},
		"invalidation.idle_fallbacks_total": {kindMoves, 0, "the idle rule firing: the channel was silent past the deadline"},
		"invalidation.requests_total":       {kindMoves, 0, "every attempt to get an invalidation answer"},
		"invalidation.failures_total":       {kindMoves, 0, "an attempt that produced NO answer (a 500, a fault, a stall, a channel end)"},
		"invalidation.last_event_age_ms":    {kindMoves, 0, "appears as soon as the channel speaks once"},
		"invalidation.poll_interval_ms":     {kindBound, 20, "the declared poll period, reported while the poll mechanism is in force"},
		"invalidation.idle_timeout_ms":      {kindBound, 150, "the silence deadline the read loop arms (the mount's own declared bound)"},
		"invalidation.resume_seq":           {kindAppears, 0, "the cursor this view was minted at; appears once the mount observes"},
		// Booleans are figures too: the census reports them as 0/1, and each one
		// names what moves it (or what the server published, verbatim).
		"invalidation.channel_available": {kindMoves, 0, "the channel answering: false until a mechanism answers, true afterwards (H-1: it must stop claiming a channel it does not have)"},
		"invalidation.liveness.stalled":  {kindMoves, 0, "the idle rule's verdict as a STATE, not only as a count: false before the stall, true after"},
		// ---- the content-age bound (the figure that did not exist at all) -----
		"invalidation.content_age.age_ms":             {kindMoves, 0, "appears the moment the client has evidence for its view"},
		"invalidation.content_age.observations_total": {kindMoves, 0, "every re-established piece of evidence (an observation, an event, a poll answer, a line)"},
		"invalidation.content_age.bound_ms":           {kindBound, 40, "the window the mechanism in force implies: two declared poll intervals (interval + one missed tick)"},
		"invalidation.content_age.within_bound":       {kindAppears, 0, "the comparison, made once, here"},
		// ---- the heartbeat / stall state --------------------------------------
		// Every one of these is evidence for LIVENESS, kept apart from content
		// for the reason §5.4 O-2 gives: an idle channel and a dead one look
		// identical from outside, and only a received line tells them apart.
		"invalidation.liveness.heartbeats_total":      {kindMoves, 0, "a heartbeat line off the pushed channel"},
		"invalidation.liveness.last_line_age_ms":      {kindMoves, 0, "appears with the first line the channel delivers (any line: a heartbeat, a duplicate, an unparseable one)"},
		"invalidation.liveness.stalls_total":          {kindMoves, 0, "the idle rule firing: the channel was silent past the deadline"},
		"invalidation.liveness.declared_heartbeat_ms": {kindPassthrough, fakeHeartbeatMS, "the period the server declares; the client reports the declaration, never a period of its own"},
		"invalidation.liveness.idle_timeout_ms":       {kindBound, 150, "the silence deadline the read loop arms (the mount's own declared bound wins over the derived one)"},
		// ---- the server's own watcher figures (passthrough) -------------------
		"invalidation.server.sampled_age_ms":         {kindAppears, 0, "how old the capability sample is"},
		"invalidation.server.heartbeat_ms":           {kindPassthrough, fakeHeartbeatMS, "the period the server declares"},
		"invalidation.server.max_paths_per_event":    {kindPassthrough, 4096, "the declared cap on one event's path list: a longer list is an overflow, never a partial drop"},
		"invalidation.server.max_event_bytes":        {kindPassthrough, fakeMaxEventBytes, "the declared per-frame BYTE bound (BFS-062): the number this consumer sizes its per-line reader from, reported verbatim because a client that derived its own bound would be assuming the other side's limit"},
		"invalidation.server.overflows_total":        {kindPassthrough, fakeOverflows, "the server's own overflow count, reported verbatim"},
		"invalidation.server.unvouched_total":        {kindPassthrough, fakeUnvouched, "intervals the server cannot vouch for"},
		"invalidation.server.rescans_total":          {kindPassthrough, fakeRescans, "full rescans the server performed"},
		"invalidation.server.install_failures_total": {kindPassthrough, fakeInstallFailures, "watch install failures"},
		"invalidation.server.backend_errors_total":   {kindPassthrough, fakeBackendErrors, "the server's watcher backend errors — the invalidation path's own backend-error figure, from the only side that can measure it"},
		"invalidation.server.heartbeats_total":       {kindPassthrough, fakeHeartbeats, "the watcher's own heartbeat count (O-2)"},
		"invalidation.server.event_loop_ticks":       {kindPassthrough, fakeLoopTicks, "the watcher's event-loop ticks"},
		"invalidation.server.last_event_age_ms":      {kindPassthrough, fakeLastEventAgeMS, "how long ago the server saw an event"},
		"invalidation.server.directories_desired":    {kindPassthrough, fakeDirsDesired, "the watch set the server wanted"},
		"invalidation.server.directories_watched":    {kindPassthrough, fakeDirsWatched, "the watch set it actually installed"},
		"invalidation.server.missing_count":          {kindPassthrough, fakeMissingCount, "directories it could not watch"},
		"invalidation.server.blocks_push":            {kindPassthrough, 1, "the server's own verdict on whether the absence of the wire form blocks the channel"},
		"invalidation.server.vouched":                {kindPassthrough, 1, "the server's liveness verdict (its watcher's own heartbeat, never the absence of events)"},
		"invalidation.server.stalled":                {kindPassthrough, 0, "the server's stall verdict"},
		"invalidation.server.coverage_complete":      {kindPassthrough, 0, "false because the document reports a missing directory — a partial watch set is reported, not rounded up"},
		// dropped_events is NULL with a reason: the kernel reports one overflow
		// marker, not how many events it dropped. Asserted in its own arm below.
		// ---- the pushed channel's frame accounting (BFS-062) ------------------
		// The declared bound is a PASSTHROUGH and the reader is DERIVED from it:
		// that pair is the row's whole claim ("the reader is sized from the
		// declaration"), so an implementation that read a constant of its own
		// here would fail rather than pass.
		"invalidation.frame_limit.declared_bytes":   {kindPassthrough, fakeMaxEventBytes, "the declaration the pushed channel is read against, reported as the bound in force"},
		"invalidation.frame_limit.reader_bytes":     {kindBound, fakeMaxEventBytes + 64<<10, "the per-line cap the reader was sized to: the declared bound plus the framing slack the relation requires"},
		"invalidation.frame_limit.ceiling_bytes":    {kindBound, (8 << 20) + 64<<10, "the largest cap this consumer will allocate, derived from the largest declaration the surface may make plus that slack"},
		"invalidation.frame_limit.over_limit":       {kindAppears, 0, "an event frame over the declared bound, as a STATE: false in this healthy workload, and driven true by the BFS-062 arms"},
		"invalidation.frame_limit.over_limit_total": {kindAppears, 0, "an event frame over the declared bound, counted once at the decision: reachable, and 0 here because no frame crossed it"},
		// ---- the refresh accounting -------------------------------------------
		"invalidation.refresh.started_total":         {kindMoves, 0, "a refresh admitted to the staged window"},
		"invalidation.refresh.in_flight":             {kindAppears, 0, "0 at rest: the window is empty once every stage has committed or aborted"},
		"invalidation.refresh.max_inflight":          {kindBound, 1, "the declared staged-window width"},
		"invalidation.refresh.committed_total":       {kindMoves, 0, "a staged refresh that published"},
		"invalidation.refresh.aborted_total":         {kindMoves, 0, "a staged refresh that discarded its blob"},
		"invalidation.refresh.refused_no_slot_total": {kindMoves, 0, "refused FOR BEING FULL: every slot taken"},
		"invalidation.refresh.refused_no_room_total": {kindMoves, 0, "refused because the bound left no room"},
		// The hot-refresh queue is not in this build: its three figures are
		// ABSENT (never 0) and its reason is asserted in its own arm below.
	}
}

// TestTheDroppedEventCountIsNullWithAReason is the null rule applied to the one
// figure the SERVER itself cannot measure: the kernel reports a single overflow
// marker, not the number of events it dropped, so the count is null and the
// reason travels beside it — never a fabricated number (§3.3 rule 2).
func TestTheDroppedEventCountIsNullWithAReason(t *testing.T) {
	s := &scriptedChannel{t: t}
	_, inv := newScriptedInvalidator(t, s, InvalidateOptions{Mode: ModePoll, PollInterval: time.Hour})
	st := inv.State()
	if st.Server == nil {
		t.Fatalf("the capability document carries a watch block, so the record must report it: %+v", st)
	}
	if st.Server.DroppedEvents != nil {
		t.Fatalf("the dropped-event count must be null (the kernel reports one marker, not a count): %d", *st.Server.DroppedEvents)
	}
	if st.Server.DroppedEventsReason != fakeDroppedReason {
		t.Fatalf("the null must carry the server's own reason verbatim: %q", st.Server.DroppedEventsReason)
	}
	// And the figures the server DID publish are the server's, not the client's.
	if st.Server.OverflowsTotal == nil || *st.Server.OverflowsTotal != fakeOverflows || st.Server.State != fakeWatchState {
		t.Fatalf("the server block was not reported verbatim: %+v", st.Server)
	}
	// And the figures a document does NOT publish are ABSENT, not zero: without
	// the counters block there is no measurement to report, and a 0 would read as
	// "this never happened" (BFS-045's rule, applied to the server's own figures).
	bare := serverWatchState(&CapabilityWatch{Name: "X-Bunker-Op: watch", State: "absent"}, 0)
	if bare.OverflowsTotal != nil || bare.RescansTotal != nil || bare.BackendErrorsTotal != nil ||
		bare.HeartbeatsTotal != nil || bare.EventLoopTicks != nil || bare.DirectoriesWatched != nil {
		t.Fatalf("a document with no counters/coverage block must publish no counter: %+v", bare)
	}
	raw, _ := json.Marshal(bare)
	if !strings.Contains(string(raw), `"overflows_total":null`) {
		t.Fatalf("an unpublished counter must be JSON null, not 0 or omitted: %s", raw)
	}
}

// TestTheAbsentHotQueueIsNullWithAReasonAndNotAZero is the row's own discipline
// applied to the row's own subject: the hot-refresh queue (BFS-037) is not in
// this build, so its depth, its bound and its refusals must be ABSENT with a
// reason. A 0 there would read as "the queue was empty" — a measurement this
// build cannot make, and exactly the shape BFS-032 documented.
func TestTheAbsentHotQueueIsNullWithAReasonAndNotAZero(t *testing.T) {
	cache := newTestCacheWithBounds(t, CacheConfig{Dir: t.TempDir(), MaxBytes: 1 << 20, MaxEntryBytes: 64, MaxAge: time.Hour})
	rf := RefreshFromCache(cache.Stats())
	if rf.QueueDepth != nil || rf.QueueMaxDepth != nil || rf.QueueRefusedFullTotal != nil || rf.SkippedOversizeTotal != nil {
		t.Fatalf("the hot queue's figures must be absent, not zero: %+v", rf)
	}
	if !strings.HasPrefix(rf.AbsentReason, ReasonNotPublished) {
		t.Fatalf("the absence must carry a vocabulary reason: %q", rf.AbsentReason)
	}
	// The staged window, by contrast, EXISTS in this build and is reported.
	if rf.MaxInFlight != DefaultCacheMaxInFlight {
		t.Fatalf("the staged window's bound must be reported: %d", rf.MaxInFlight)
	}
	raw, err := json.Marshal(rf)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"queue_depth":null`) {
		t.Fatalf("the queue's depth must be JSON null (a null with a reason), not omitted and not 0: %s", raw)
	}
}

// TestADeclaredCapabilityRefusalIsAnAnswerAndNotABackendError is the deterministic
// half of the request accounting: a surface that refuses an op it cannot serve
// has ANSWERED, and the record must say so. Counting that as a backend error
// would inflate the figure with the one response that is working exactly as
// designed — and the row is about figures that must not mislead.
func TestADeclaredCapabilityRefusalIsAnAnswerAndNotABackendError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("X-Bunker-Op") == "events" {
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = io.WriteString(w, `{"ok":false,"verdict":"capability_unavailable","error":{"capability":"events","scope":"build","mode":"poll","detail":"this build does not serve the events op"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"capabilities":{"surface":"refuser","document_version":1}}}`)
	}))
	defer srv.Close()
	c, err := NewClient(Options{BaseURL: srv.URL, Concurrency: 2, OpTimeout: 3 * time.Second, BindTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	inv := NewInvalidator(c, InvalidateOptions{Mode: ModePoll, PollInterval: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = inv.Run(ctx) }()
	if !waitFor(3*time.Second, func() bool { return inv.State().Requests > 0 }) {
		t.Fatalf("the poll was never attempted: %+v", inv.State())
	}
	cancel()
	st := inv.State()
	if st.Requests < 1 {
		t.Fatalf("requests_total=%d, want at least one attempt", st.Requests)
	}
	if st.Failures != 0 || st.LastFailure != "" {
		t.Fatalf("a declared capability refusal is an ANSWER, not a backend error: failures=%d last_failure=%q",
			st.Failures, st.LastFailure)
	}
}

// TestTheClientAbsentBlocksCarryTheReasonThatFitsTheFact drives the three ways
// the SERVER block can be absent, because they are three different facts: no
// handshake at all is UNKNOWN, a document without the block is NOT PUBLISHED,
// and a document whose watcher is absent is a VALUE with the server's own reason.
func TestTheClientAbsentBlocksCarryTheReasonThatFitsTheFact(t *testing.T) {
	// 1. No document at all.
	c, err := NewClient(Options{BaseURL: "http://127.0.0.1:1/dav", BindTimeout: time.Second, OpTimeout: time.Second})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	inv := NewInvalidator(c, InvalidateOptions{Mode: ModePoll})
	st := inv.State()
	if st.Server != nil {
		t.Fatal("a client with no capability document must not report a server block")
	}
	if !strings.HasPrefix(st.ServerReason, ReasonUnknown) {
		t.Fatalf("no document at all is UNKNOWN (not absent): %q", st.ServerReason)
	}
	if st.ContentAge != nil {
		t.Fatal("a client that has never observed the tree must not report an age")
	}
	if !strings.HasPrefix(st.ContentAgeReason, ReasonNoSample) {
		t.Fatalf("the absent age must be no_sample: %q", st.ContentAgeReason)
	}
	if st.Liveness != nil {
		t.Fatal("a poll mount has no heartbeat to report")
	}
	if !strings.HasPrefix(st.LivenessReason, ReasonDisabled) {
		t.Fatalf("the absent liveness block must say why: %q", st.LivenessReason)
	}

	// 2. A document that carries no watch block: NOT PUBLISHED, which is a
	//    different fact from "there is no watcher".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{"capabilities":{"surface":"old","document_version":1}}}`)
	}))
	defer srv.Close()
	old, err := NewClient(Options{BaseURL: srv.URL, BindTimeout: time.Second, OpTimeout: time.Second})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, oerr := old.CapabilitiesDoc(context.Background()); oerr != nil {
		t.Fatalf("capability document: %v", oerr)
	}
	oldInv := NewInvalidator(old, InvalidateOptions{Mode: ModePoll})
	if got := oldInv.State().ServerReason; !strings.HasPrefix(got, ReasonNotPublished) {
		t.Fatalf("a document without a watch block is NOT PUBLISHED: %q", got)
	}
}

// TestTheReportedDirectoryFigureAgreesWithAnIndependentWalk is the BFS-031 arm:
// the reported figure must agree with a measurement taken by someone else.
// `used_bytes` counts published blobs + the serialised index and does NOT count
// status.json, conflicts.jsonl, staged blobs or a temp index — so the record now
// carries BOTH, plus the delta, and this test takes the walk for itself.
func TestTheReportedDirectoryFigureAgreesWithAnIndependentWalk(t *testing.T) {
	// THE LAYOUT IS PART OF THE CONTRACT (BFS-031). The directory the byte bound
	// names holds the cache and NOTHING ELSE: the mount's own state (status.json
	// and the refusal log) lives in the MOUNT directory, one level up, with its
	// own bound. So this arm builds the mount directory the way the mount does and
	// asserts both halves: the cache directory agrees with an independent walk of
	// the CACHE, and the state files are in the mount directory and are NOT in the
	// cache directory.
	mount := t.TempDir()
	cacheDir := MountCacheDir(mount)
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("cache dir: %v", err)
	}
	cache := newTestCacheWithBounds(t, CacheConfig{Dir: cacheDir, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 16, MaxAge: time.Hour, DirMeasureInterval: -1})
	for _, p := range []string{"x.go", "y.go"} {
		body := []byte(p + "-body")
		if _, err := cache.Insert(p, HashBytes(body), body); err != nil {
			t.Fatalf("insert %s: %v", p, err)
		}
	}
	// The mount's own residents, written where the mount writes them: in the
	// MOUNT directory, beside the cache directory.
	st := Status{Mount: "probe", Endpoint: "http://127.0.0.1:1/dav", Cache: cache.Stats()}
	if err := WriteStatus(mount, st); err != nil {
		t.Fatalf("write status: %v", err)
	}
	if err := AppendConflict(mount, Conflict{Path: "x.go", Code: "hash_mismatch", Detail: "probe"}); err != nil {
		t.Fatalf("append conflict: %v", err)
	}
	// A fresh Stats() re-measures with those files in place (and, because they are
	// NOT in the cache directory, they must not appear in its figures).
	cs := cache.Stats()

	// THE INDEPENDENT MEASUREMENT: a walk this test performs itself, with no
	// knowledge of the implementation's classification.
	var walked int64
	byClass := map[string]int64{}
	err := filepathWalk(cacheDir, func(rel string, size int64) {
		walked += size
		switch {
		case rel == CacheIndexFile:
			byClass[DirClassIndex] += size
		case rel == StatusFile:
			byClass[DirClassStatus] += size
		case rel == ConflictsFile:
			byClass[DirClassConflicts] += size
		case strings.HasPrefix(rel, CacheBlobDir+"/"):
			byClass[DirClassBlobs] += size
		default:
			byClass[DirClassOther] += size
		}
	})
	if err != nil {
		t.Fatalf("independent walk: %v", err)
	}
	if cs.DirBytes != walked {
		t.Fatalf("the reported dir_bytes (%d) disagrees with an independent walk of the same directory (%d) — BFS-031's defect", cs.DirBytes, walked)
	}
	var sum int64
	for _, v := range cs.DirBytesByClass {
		sum += v
	}
	if sum != cs.DirBytes {
		t.Fatalf("the classes sum to %d but the total is %d: the measurement does not add up", sum, cs.DirBytes)
	}
	if cs.DirUnaccountedBytes != cs.DirBytes-cs.UsedBytes {
		t.Fatalf("unaccounted_bytes=%d, want dir_bytes−used_bytes=%d", cs.DirUnaccountedBytes, cs.DirBytes-cs.UsedBytes)
	}
	// THE DECISION, ASSERTED: the state files are in the mount directory and are
	// NOT inside the directory the bound names. If they were, `dir_bytes` would be
	// the sum of two unrelated things again — which is exactly the defect.
	if cs.DirBytesByClass[DirClassStatus] != 0 || cs.DirBytesByClass[DirClassConflicts] != 0 {
		t.Fatalf("the observability files are inside the cache directory: status=%d conflicts=%d — BFS-031 moved them out so the bound can bound what it names",
			cs.DirBytesByClass[DirClassStatus], cs.DirBytesByClass[DirClassConflicts])
	}
	for _, name := range []string{StatusFile, ConflictsFile} {
		info, err := os.Stat(filepath.Join(mount, name))
		if err != nil {
			t.Fatalf("the mount's %s is not in the mount directory (%s): %v", name, mount, err)
		}
		if info.Size() <= 0 {
			t.Fatalf("%s is empty in the mount directory", name)
		}
	}
	if cs.DirBytesByClass[DirClassBlobs] == 0 {
		t.Fatal("two blobs were written and the walk found none")
	}
	// THE ROW'S CLAIM, in the same arm (BFS-031): the directory the bound names is
	// INSIDE the bound, measured on the filesystem rather than derived.
	if cs.DirBytes > cs.MaxBytes {
		t.Fatalf("the cache directory holds %d B, over its %d B bound", cs.DirBytes, cs.MaxBytes)
	}
	if cs.DirPeakBytes > cs.MaxBytes {
		t.Fatalf("the peak the bound is enforced against (%d) exceeds the bound %d", cs.DirPeakBytes, cs.MaxBytes)
	}
	if cs.UsedBytes+cs.IndexBytes > cs.MaxBytes {
		t.Fatalf("the index's temp copy is not reserved: used=%d index=%d max=%d", cs.UsedBytes, cs.IndexBytes, cs.MaxBytes)
	}
	if cs.UsedBytes > cs.MaxBytes {
		t.Fatalf("used_bytes=%d exceeds the bound %d", cs.UsedBytes, cs.MaxBytes)
	}
	if cs.DirMeasuredAgeMS == nil {
		t.Fatal("the sample's age must be reported beside it, so a reused walk is never read as a fresh one")
	}
	t.Logf("independent agreement: reported dir_bytes=%d walk=%d (used_bytes=%d, peak=%d, unaccounted=%d) classes=%v",
		cs.DirBytes, walked, cs.UsedBytes, cs.DirPeakBytes, cs.DirUnaccountedBytes, cs.DirBytesByClass)
}

// TestTheDirectoryMeasurementReportsItsOwnReasoningWhenTheWalkFails is the null
// rule on the measurement itself: a directory that cannot be read must be
// ABSENT WITH A REASON, never a 0 that reads as an empty cache.
func TestTheDirectoryMeasurementReportsItsOwnReasoningWhenTheWalkFails(t *testing.T) {
	cache := newTestCacheWithBounds(t, CacheConfig{Dir: t.TempDir(), MaxBytes: 1 << 20, MaxEntryBytes: 64, MaxAge: time.Hour, DirMeasureInterval: -1})
	body := []byte("gone")
	if _, err := cache.Insert("gone.go", HashBytes(body), body); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Remove the blob directory out from under the measurement: the walk cannot
	// succeed, and the figure must say so rather than report 0 bytes.
	if err := os.RemoveAll(filepath.Join(cache.Dir(), CacheBlobDir)); err != nil {
		t.Fatalf("clear blobs: %v", err)
	}
	cs := cache.Stats()
	if cs.DirBytesReason == "" {
		t.Fatalf("the walk could not have succeeded and the record claims a measurement: dir_bytes=%d", cs.DirBytes)
	}
	if !strings.HasPrefix(cs.DirBytesReason, ReasonUnknown) {
		t.Fatalf("an unreadable measurement is UNKNOWN: %q", cs.DirBytesReason)
	}
}
