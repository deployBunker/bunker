package fsclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-037's fixture: a WebDAV-ish stub a cell can THROTTLE, HOLD mid-body, and
// COUNT. Everything a refresh cell needs to be non-vacuous lives here:
//
//   - per-path GET and HEAD counts, so "exactly one fetch" is asserted at the
//     SERVER rather than at a client counter the client could be lying about;
//   - per-path BYTES SERVED, so "no bytes were pulled above the size rule" is a
//     measurement and not an inference;
//   - per-path and global concurrency high-water marks, so the pool share is a
//     number taken at the wire;
//   - a HOLD gate between the first and second half of a body, so a cell can sit
//     inside the mid-flight window deterministically instead of sleeping and
//     hoping (the handler also releases on the request's own cancellation, so a
//     test that never releases cannot wedge the server's Close).
// ---------------------------------------------------------------------------

type hotStub struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	bodies    map[string][]byte
	delay     time.Duration
	headDelay time.Duration
	hold      chan struct{}
	gets      map[string]int
	heads     map[string]int
	served    map[string]int
	live      int
	maxLive   int
	pathMax   map[string]int
	pathNow   map[string]int
	gates     int
	// tree is the identity every response carries (D-9's arm switches it).
	tree string
	// getFail makes a path answer 404 to a GET while its HEAD still succeeds:
	// the `not_found` ABANDON's trigger, as distinct from the HEAD-404 skip.
	getFail map[string]bool
}

func newHotStub(t *testing.T) *hotStub {
	t.Helper()
	s := &hotStub{
		t: t, bodies: map[string][]byte{}, gets: map[string]int{}, heads: map[string]int{},
		served: map[string]int{}, pathMax: map[string]int{}, pathNow: map[string]int{},
		getFail: map[string]bool{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(func() {
		s.Release()
		s.srv.Close()
	})
	return s
}

// SetTree changes the identity every response carries. A cell that moves it
// between a mount's arm and its refresh is D-9's arm.
func (s *hotStub) SetTree(t string) {
	s.mu.Lock()
	s.tree = t
	s.mu.Unlock()
}

// FailGET makes a path 404 on GET while its HEAD keeps succeeding.
func (s *hotStub) FailGET(path string) {
	s.mu.Lock()
	s.getFail[path] = true
	s.mu.Unlock()
}

func (s *hotStub) URL() string { return s.srv.URL }

// Set publishes a path's bytes. A second Set is how a cell makes a file CHANGE.
func (s *hotStub) Set(path string, body []byte) {
	s.mu.Lock()
	s.bodies[path] = append([]byte(nil), body...)
	s.mu.Unlock()
}

// Fail makes a path absent (a 404), which is how the `not_found` arms fire.
func (s *hotStub) Fail(path string) {
	s.mu.Lock()
	delete(s.bodies, path)
	s.mu.Unlock()
}

// Throttle sets the per-request pause between the two halves of a body.
func (s *hotStub) Throttle(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

// HeadDelay makes the server slow to send HEADERS. This is the shape that
// contends the CLIENT's pool: the client's slot is held until the response
// headers arrive (the body is streamed after the slot returns), so a fixture
// that only throttles bodies measures nothing about the pool.
func (s *hotStub) HeadDelay(d time.Duration) {
	s.mu.Lock()
	s.headDelay = d
	s.mu.Unlock()
}

// Hold arms the mid-body gate: the next body writes its first half, then WAITS
// until Release (or until the request is cancelled).
func (s *hotStub) Hold() {
	s.mu.Lock()
	if s.hold == nil {
		s.hold = make(chan struct{})
	}
	s.mu.Unlock()
}

// Release opens the gate (idempotent).
func (s *hotStub) Release() {
	s.mu.Lock()
	h := s.hold
	s.hold = nil
	s.mu.Unlock()
	if h != nil {
		close(h)
	}
}

func (s *hotStub) Counting() bool { return true }

func (s *hotStub) GETs(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets[path]
}

func (s *hotStub) HEADs(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heads[path]
}

// BytesServed is how many BODY bytes this path's server ever handed over. It is
// the figure R5's failure is measured in: bytes that were pulled.
func (s *hotStub) BytesServed(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served[path]
}

// PathMax is the most requests this path ever had in flight at once.
func (s *hotStub) PathMax(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pathMax[path]
}

// MaxLive is the most requests of any path in flight at once.
func (s *hotStub) MaxLive() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxLive
}

// Requests is every request the server answered.
func (s *hotStub) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, v := range s.gets {
		n += v
	}
	for _, v := range s.heads {
		n += v
	}
	return n
}

// WaitGETs blocks until path has been GET at least n times (bounded).
func (s *hotStub) WaitGETs(path string, n int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if s.GETs(path) >= n {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return s.GETs(path) >= n
}

func (s *hotStub) enter(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live++
	if s.live > s.maxLive {
		s.maxLive = s.live
	}
	s.pathNow[path]++
	if s.pathNow[path] > s.pathMax[path] {
		s.pathMax[path] = s.pathNow[path]
	}
	return s.pathNow[path]
}

func (s *hotStub) leave(path string) {
	s.mu.Lock()
	s.live--
	s.pathNow[path]--
	s.mu.Unlock()
}

func (s *hotStub) handler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	s.mu.Lock()
	body, ok := s.bodies[path]
	delay, hold := s.delay, s.hold
	headDelay := s.headDelay
	tree := s.tree
	failGet := s.getFail[path]
	if r.Method == http.MethodGet {
		s.gets[path]++
	} else {
		s.heads[path]++
	}
	s.mu.Unlock()
	// The header pause comes FIRST: it is the interval during which the client
	// holds a pool slot, so it is the interval that can contend the pool.
	if headDelay > 0 {
		select {
		case <-time.After(headDelay):
		case <-r.Context().Done():
			return
		}
	}
	if tree != "" {
		w.Header().Set("X-Bunker-Tree", tree)
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	etag := ETagFor(HashBytes(body))
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if failGet {
		http.Error(w, "gone", http.StatusNotFound)
		return
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	s.enter(path)
	defer s.leave(path)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	// The body goes out in PIECES with the pause between them, so a cell has
	// several checkpoints to sit on rather than one: a refresh's yield checks
	// run per chunk, and a body delivered in one write would give the policy a
	// single instant to notice pressure.
	pieces := 4
	if len(body) < pieces {
		pieces = len(body)
	}
	if pieces <= 1 {
		n, _ := w.Write(body)
		s.countServed(path, n)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	step := len(body) / pieces
	pause := delay / time.Duration(pieces-1)
	for i := 0; i < pieces; i++ {
		start := i * step
		end := start + step
		if i == pieces-1 {
			end = len(body)
		}
		n, _ := w.Write(body[start:end])
		s.countServed(path, n)
		if flusher != nil {
			flusher.Flush()
		}
		if i == 0 && hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		if i < pieces-1 && pause > 0 {
			select {
			case <-time.After(pause):
			case <-r.Context().Done():
				return
			}
		}
	}
}

func (s *hotStub) countServed(path string, n int) {
	s.mu.Lock()
	s.served[path] += n
	s.mu.Unlock()
}

// ── the manager fixture ────────────────────────────────────────────────────

// hotSetup is one armed mount's client-side world, without a kernel in it: a
// client, a cache, and a manager, with the tracker directory where the real
// mount puts it (inside the cache directory — F-2).
type hotSetup struct {
	t       *testing.T
	Client  *Client
	Cache   *Cache
	Manager *HotManager
	Dir     string
	Stub    *hotStub
	Policy  HotPolicy
	Tree    string
}

type hotOption func(*hotSetup, *Options, *CacheConfig)

// WithConcurrency sets the client's pool size (the share's denominator).
func WithConcurrency(n int) hotOption {
	return func(_ *hotSetup, o *Options, _ *CacheConfig) { o.Concurrency = n }
}

// WithCeiling sets the size rule, so a boundary arm can be driven with small
// payloads instead of 8 MiB ones.
func WithCeiling(n int64) hotOption {
	return func(s *hotSetup, _ *Options, c *CacheConfig) {
		s.Policy.MaxFileBytes = n
		if c.MaxEntryBytes < n {
			c.MaxEntryBytes = n
		}
	}
}

// WithTick sets the manager's tick.
func WithTick(d time.Duration) hotOption {
	return func(s *hotSetup, _ *Options, _ *CacheConfig) { s.Policy.TickInterval = d }
}

// WithPolicy lets a cell change any knob (depth, deadline, jitter, …).
func WithPolicy(f func(p *HotPolicy)) hotOption {
	return func(s *hotSetup, _ *Options, _ *CacheConfig) { f(&s.Policy) }
}

// WithCacheBounds overrides the cache's bounds (the no_room / pinned arms).
func WithCacheBounds(max, entry int64) hotOption {
	return func(_ *hotSetup, _ *Options, c *CacheConfig) {
		c.MaxBytes = max
		c.MaxEntryBytes = entry
	}
}

// WithOpTimeout sets the client's own operation deadline (the backoff cap's
// relation is stated against it).
func WithOpTimeout(d time.Duration) hotOption {
	return func(_ *hotSetup, o *Options, _ *CacheConfig) { o.OpTimeout = d }
}

// WithTree pins the manager's tree identity, so D-9's arm has something to
// disagree with.
func WithTree(tree string) hotOption {
	return func(s *hotSetup, _ *Options, _ *CacheConfig) { s.Tree = tree }
}

// WithMaxConns lifts the transport's connection cap above the pool, so a share
// arm measures the POOL and not the transport's own cap (they are different
// bounds and a cell must not confuse them).
func WithMaxConns(n int) hotOption {
	return func(_ *hotSetup, o *Options, _ *CacheConfig) { o.MaxConnsPerHost = n }
}

// newHotSetup builds an ARMED hot path over a stub server. Everything is torn
// down by the test's cleanup, in the order that lets a held body be released
// first.
func newHotSetup(t *testing.T, stub *hotStub, opts ...hotOption) *hotSetup {
	t.Helper()
	s := &hotSetup{
		t: t, Stub: stub, Dir: t.TempDir(),
		Policy: func() HotPolicy {
			p := DefaultHotPolicy()
			p.Enabled = true
			p.MaxFileBytes = 1 << 16
			p.TickInterval = 10 * time.Millisecond
			p.BackoffJitter = HotJitterNone
			p.RefreshDeadline = 2 * time.Second
			p.StopDeadline = 2 * time.Second
			p.FlushInterval = time.Hour
			return p
		}(),
	}
	cacheCfg := CacheConfig{
		Dir: filepath.Join(s.Dir, "cache"), MaxBytes: 32 << 20, MaxEntryBytes: 8 << 20,
		MaxEntries: 512, MaxInFlight: 2, MaxAge: time.Hour,
	}
	clientOpts := Options{BaseURL: stub.URL(), Concurrency: 8, BindTimeout: time.Second}
	for _, o := range opts {
		o(s, &clientOpts, &cacheCfg)
	}
	// The policy must be COHERENT for the manager to exist at all: Q-12 derives
	// the stop deadline from one tick plus one yield quantum, so a cell that
	// shortens or lengthens one of those gets the relation satisfied here rather
	// than silently building no manager (which every cell would report as a
	// fixture bug).
	if floor := s.Policy.TickInterval + s.Policy.YieldAfter; s.Policy.StopDeadline < floor {
		s.Policy.StopDeadline = floor
	}
	cache, err := OpenCache(cacheCfg)
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	cl, err := NewClient(clientOpts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	mgr := NewHotManager(HotManagerConfig{
		Policy: s.Policy, Client: cl, Cache: cache, TrackerDir: cacheCfg.Dir,
		MountKey: "hotstub", Tree: s.Tree, Logf: t.Logf,
	})
	if mgr == nil {
		t.Fatalf("the hot manager must exist for this fixture (policy enabled, cache open)")
	}
	if err := mgr.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	mgr.Arm()
	s.Client, s.Cache, s.Manager = cl, cache, mgr
	t.Cleanup(func() {
		_ = mgr.Close()
		_ = cache.Close()
	})
	return s
}

// seedRead makes a path TRACKED and CACHED the way the live mount does: a real
// read through the hot path, admitted to the cache. A refresh candidate is by
// definition a path this client has read (D-2).
func (s *hotSetup) seedRead(path string, admit bool) []byte {
	s.t.Helper()
	// The live mount does BOTH: the read TOUCHES the tracker and fetches through
	// the hot path. A path that is only fetched is not a refresh candidate (D-2),
	// so a fixture that skipped the touch would make every cell inert.
	s.Manager.NoteRead(path)
	data, hash, oerr, _ := s.Manager.HotRead(context.Background(), path, "")
	if oerr != nil {
		s.t.Fatalf("seed read of %s failed: %v", path, oerr)
	}
	if admit {
		s.Cache.AdmitRead(path, hash, data)
		s.Cache.Pin(hash)
	}
	return data
}

// hotWaitHeld waits until `path`'s current transfer has served SOME of its bytes
// and not all of them. That partial state is the only honest evidence that the
// mid-body gate is holding a transfer: a condition like "served > 0" is already
// true after an earlier completed read, which makes a cell blind to whether the
// gate is load-bearing at all.
func hotWaitHeld(t *testing.T, stub *hotStub, path string, total, base int, within time.Duration) bool {
	t.Helper()
	return hotWaitFor(t, "the transfer to be held mid-body", within, func() bool {
		n := stub.BytesServed(path) - base
		return n > 0 && n < total
	})
}

// hotTick lets the manager's ticker run (the fixture's tick is 10 ms).
func hotTick(n int) { time.Sleep(time.Duration(n) * 15 * time.Millisecond) }

// waitFor polls a condition, bounded, so a cell never hangs a suite.
func hotWaitFor(t *testing.T, what string, within time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	if cond() {
		return true
	}
	t.Logf("hotWaitFor(%s): not satisfied within %s", what, within)
	return false
}

func hotBody(n int, marker byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = marker + byte(i%7)
	}
	return b
}

// stageResidue counts the unpublished stage files left in the cache's blob
// directory — the leak §5.6's obligation (a) exists to prevent.
func stageResidue(t *testing.T, cache *Cache) int {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(cache.Dir(), CacheBlobDir))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), CacheStagePrefix) {
			n++
		}
	}
	return n
}

func hotDelta(before, after map[string]int64, reason string) int64 {
	return after[reason] - before[reason]
}

func hotCensus(m *HotManager) map[string]int64 {
	st := m.Stats()
	out := map[string]int64{}
	for k, v := range st.Skips {
		out["skip:"+k] = v
	}
	for k, v := range st.Abandoned {
		out["abandon:"+k] = v
	}
	return out
}

var _ = fmt.Sprintf
