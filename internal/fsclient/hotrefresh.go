package fsclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-037 — THE HOT-FILE CACHE: the manager. hotcache.go is the state (the
// tracker and the queue); this file is the machinery that fills them and the
// policy that bounds it.
//
// THE FOUR THINGS THIS FILE IS SHAPED BY, each a requirement of the row:
//
//  1. THE REFRESH HAS ITS OWN BUDGET (P-3/P-6). A refresh request acquires from
//     `Client.rsem` — a SEPARATE bounded budget sized to the pool share — never
//     from the foreground semaphore, so a refresh cannot consume a foreground
//     slot at all. The yield rule (P-7) then covers the other direction on the
//     one resource the two DO share: the transport's connection cap.
//  2. THE QUEUE IS STOPPABLE IN FULL (§5.5). Stop is LEVEL-TRIGGERED: it refuses
//     new work (counted), EMPTIES the queue at once (each item counted by reason
//     `stopped`), and CANCELS in-flight refreshes at their next checkpoint — while
//     a fetch a READER has joined is foreground work and is untouched (PR-11).
//  3. PROMOTION IS SINGLE-FLIGHT (§7). One per-path fetch record per mount: a
//     reader that finds a fetch in flight JOINS it instead of issuing a second
//     GET, and the promoted fetch stops being the refresh's to abandon.
//  4. PUBLISHING IS NOT A YIELD POINT (§5.6). Every cancel check sits inside the
//     body loop; past the last byte the refresh stages and commits with no
//     checkpoint between "bytes complete" and "pointer swapped" — the one window
//     where abandonment would be corruption rather than a lost optimisation.
// ---------------------------------------------------------------------------

// The RUNTIME states of the hot path (§5.5 Q-15). They are the manager's, and
// they are deliberately a different vocabulary from the CONFIGURATION states in
// hotpolicy.go: "the operator turned it off" and "it stopped itself under
// pressure" must not be reportable as the same thing.
const (
	// HotRunDisarmed — the feature is configured on, but the mount has not yet
	// completed its baseline and first observation (D-1). Arming queues ZERO
	// refreshes: a restored tracker never produces a catch-up burst.
	HotRunDisarmed = "disarmed"
	// HotRunArmed — invalidations enqueue and the refresher runs.
	HotRunArmed = "armed"
	// HotRunStopped — STOP IN FULL is in force; the queue is empty and no new
	// work is admitted. `stop_reason` says who stopped it.
	HotRunStopped = "stopped"
	// HotRunMisconfigured — the configuration cannot be honoured as written
	// (S-9/S-10): the mount still works and the hot path is inert (S-11).
	HotRunMisconfigured = "misconfigured"
)

// The refresh's own sentinels. They are NOT OpErrors: a yield or a stop is a
// decision this subsystem made, not a failure of the request.
var (
	errHotYield    = errors.New("hot: yielded at a checkpoint (a foreground request is waiting)")
	errHotStopped  = errors.New("hot: stopped in full")
	errHotOversize = errors.New("hot: the body exceeded the size rule")
)

// hotFetch is ONE in-flight fetch of one path: the single-flight record (§7.2).
// Its whole value is that N readers of a path share exactly one GET, and that
// the promoted case is expressible — a fetch a reader has joined is FOREGROUND
// and the refresh manager may no longer cancel it (PR-11).
type hotFetch struct {
	mgr  *HotManager
	path string
	done chan struct{}
	once sync.Once

	mu         sync.Mutex
	foreground bool
	data       []byte
	hash       string
	err        *OpError
	joined     int
}

func (f *hotFetch) isForeground() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.foreground
}

// markForeground promotes the fetch: from this instant it is foreground work,
// so STOP IN FULL must not cancel it (PR-11) and its checkpoint stops asking
// whether the refresh may continue.
func (f *hotFetch) markForeground() {
	f.mu.Lock()
	f.foreground = true
	f.joined++
	f.mu.Unlock()
	f.mgr.joins.Add(1)
}

// complete closes the fetch record exactly once and releases the per-path entry
// (PR-8: the map must return to 0 at rest, on success, on error and on abandon —
// a map that grows on errors is a slow leak in the component whose purpose is to
// bound concurrency).
//
// It is also the SINGLE point at which a bytes-fetching request is known to have
// finished, so `fetches_total` is counted here — for a refresh and for a
// promoted reader's own fetch alike. (It used to be counted nowhere: the field
// was declared and reported but never incremented, which is the
// "counter that cannot move" shape this project has filed twice.)
func (f *hotFetch) complete(data []byte, hash string, err *OpError) {
	f.once.Do(func() {
		if err == nil && data != nil {
			f.mgr.fetchesTotal.Add(1)
		}
		f.mu.Lock()
		f.data, f.hash, f.err = data, hash, err
		f.mu.Unlock()
		f.mgr.endFetch(f)
		close(f.done)
	})
}

// Wait blocks until the shared fetch finishes and returns its outcome. Every
// waiter of one fetch receives the SAME outcome — the same bytes or the same
// error (PR-9) — which is what makes the map correct rather than merely
// efficient: a shared fetch is a shared answer.
func (f *hotFetch) Wait(ctx context.Context) ([]byte, string, *OpError) {
	select {
	case <-f.done:
	case <-ctx.Done():
		return nil, "", &OpError{Op: "hot-join", Path: f.path, Errno: portableErrno(4), Cause: CauseCancelled, Err: ctx.Err()}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data, f.hash, f.err
}

// HotManagerConfig is everything the manager needs to exist.
type HotManagerConfig struct {
	Policy HotPolicy
	Client *Client
	Cache  *Cache
	// TrackerDir is the directory `hot.json` lives in: the CACHE directory, so
	// the tracker is inside the bound that names it (F-2).
	TrackerDir string
	// MountKey is `MountID(baseURL)`: the tracker is per endpoint, keyed exactly
	// the way the cache directory is keyed (H-19).
	MountKey string
	// Tree is the pinned tree identity at write time (H-20's third header).
	Tree string
	// Now is the clock seam; nil means time.Now.
	Now func() time.Time
	// Dice is the jitter seam (P-10 full jitter); nil means math/rand-ish.
	Dice func() float64
	Logf func(string, ...any)
}

// HotManager is the hot-file refresh subsystem of ONE mount. A nil *HotManager
// is valid and inert: every method tolerates a nil receiver, which is how the
// mount's read path stays byte-for-byte what it was when the feature is off
// (BFS-044's law).
type HotManager struct {
	pol  HotPolicyEffective
	cfg  HotPolicy
	cl   *Client
	ca   *Cache
	logf func(string, ...any)
	now  func() time.Time
	dice func() float64

	dir  string
	key  string
	tree string

	track *HotTracker
	queue *HotQueue

	baseCtx  context.Context
	cancel   context.CancelFunc
	ticker   *time.Ticker
	tickDone chan struct{}
	started  bool

	mu          sync.Mutex
	state       string
	stopReason  string
	closed      bool
	running     map[*hotFetch]context.CancelFunc
	resyncUntil time.Time
	pressure    int
	skips       map[string]int64
	abandons    map[string]int64
	stops       map[string]int64
	resumes     map[string]int64
	lastFlush   time.Time

	sfMu sync.Mutex
	sf   map[string]*hotFetch
	warm map[string]int64

	inflight    atomic.Int64
	inflightMax atomic.Int64
	wg          sync.WaitGroup

	armsTotal, refreshesTotal, notModifiedTotal, fetchesTotal, retriesTotal atomic.Int64
	yieldsTotal, downloadedBytes, readCount, readTouches, readDeduped       atomic.Int64
	editTouches, promotions, refusedStopped, stopDeadlineMissed             atomic.Int64
	warmHitsTotal, warmHitBytes, leaders, joins, fallbacks                  atomic.Int64
	backoffCurrentMS                                                        atomic.Int64
	flushes                                                                 atomic.Int64
}

// NewHotManager builds the manager for a mount, or returns nil when the hot path
// must not exist at all: the feature is off, the configuration is not
// `enabled`, or the cache cannot store (D-6: warming a client that cannot store
// is bandwidth for a guaranteed discard).
func NewHotManager(cfg HotManagerConfig) *HotManager {
	if !cfg.Policy.Enabled {
		return nil
	}
	if cfg.Client == nil || cfg.Cache == nil {
		return nil
	}
	if eff := cfg.Policy.Effective(cfg.envFromCache()); eff.ConfigState != HotConfigEnabled {
		return nil
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	dice := cfg.Dice
	if dice == nil {
		seed := uint64(now().UnixNano())
		dice = func() float64 {
			// A tiny xorshift: full jitter needs an unpredictable draw, and a
			// shared global RNG would serialize refreshes across mounts.
			seed ^= seed << 13
			seed ^= seed >> 7
			seed ^= seed << 17
			return float64(seed%1_000_000) / 1_000_000
		}
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	h := &HotManager{
		pol:      cfg.Policy.Effective(cfg.envFromCache()),
		cfg:      cfg.Policy,
		cl:       cfg.Client,
		ca:       cfg.Cache,
		logf:     logf,
		now:      now,
		dice:     dice,
		dir:      cfg.TrackerDir,
		key:      cfg.MountKey,
		tree:     cfg.Tree,
		track:    NewHotTracker(cfg.Policy, now),
		state:    HotRunDisarmed,
		running:  map[*hotFetch]context.CancelFunc{},
		sf:       map[string]*hotFetch{},
		warm:     map[string]int64{},
		skips:    hotReasons(HotSkipReasons),
		abandons: hotReasons(HotAbandonReasons),
		stops:    map[string]int64{},
		resumes:  map[string]int64{},
	}
	h.queue = NewHotQueue(cfg.Policy.QueueMaxDepth, h.track)
	// P-3: the refresh budget is its own, sized to the share floor(Concurrency
	// × num/den) with a floor of 1 — never the foreground's semaphore.
	h.cl.EnableRefreshBudget(h.pol.Derived.PoolSlots)
	return h
}

// envFromCache rebuilds the environment a policy is resolved against from the
// cache's own bounds, so the manager and BFS-044's resolution cannot disagree
// about the numbers: the share's denominator, the size rule's cross-checks and
// the entry cap all come from the running cache.
func (c HotManagerConfig) envFromCache() HotPolicyEnv {
	return HotPolicyEnv{
		Concurrency:        c.Client.opt.Concurrency,
		OpTimeout:          c.Client.opt.OpTimeout,
		CacheMaxBytes:      c.Cache.cfg.MaxBytes,
		CacheMaxEntryBytes: c.Cache.cfg.MaxEntryBytes,
	}
}

// Policy is the resolved policy the manager obeys.
func (h *HotManager) Policy() HotPolicyEffective {
	if h == nil {
		return HotPolicyEffective{}
	}
	return h.pol
}

// PolicyConfigured is the policy as configured (the knobs, not the derived
// numbers).
func (h *HotManager) PolicyConfigured() HotPolicy {
	if h == nil {
		return HotPolicy{}
	}
	return h.cfg
}

// Start loads the tracker and starts the manager's tick. It does NOT arm: D-1
// requires the mount's baseline and first observation first.
func (h *HotManager) Start() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return nil
	}
	h.started = true
	h.baseCtx, h.cancel = context.WithCancel(context.Background())
	h.ticker = time.NewTicker(h.cfg.TickInterval)
	h.tickDone = make(chan struct{})
	h.lastFlush = h.now()
	h.mu.Unlock()

	// H-16: cold start is EMPTY and does no walk. The only thing this can do is
	// read one small document written by a previous mount of the same endpoint.
	h.track.Load(h.dir, h.key, h.tree)
	go h.tickLoop()
	return nil
}

// Arm turns the hot path on: from here invalidations enqueue. It is called by
// the mount once the bind-time baseline has completed AND the invalidator has
// established its first observation (D-1). Arming queues ZERO refreshes.
func (h *HotManager) Arm() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.closed || h.state == HotRunArmed {
		h.mu.Unlock()
		return
	}
	h.state = HotRunArmed
	h.stopReason = ""
	h.pressure = 0
	h.mu.Unlock()
	h.armsTotal.Add(1)
}

// Disarm returns the hot path to the pre-arm state (used at unmount and by
// cells).
func (h *HotManager) Disarm() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.state = HotRunDisarmed
	h.mu.Unlock()
}

// State is the runtime state (armed|disarmed|stopped|misconfigured).
func (h *HotManager) State() string {
	if h == nil {
		return HotRunDisarmed
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}

// StopReason is who stopped the hot path, empty when it is not stopped.
func (h *HotManager) StopReason() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopReason
}

// Stopped reports whether STOP IN FULL is in force.
func (h *HotManager) Stopped() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state == HotRunStopped
}

// Stop is STOP IN FULL (§5.5), one sentence, executed literally:
//
//	no refresh is admitted, the queue is emptied at once, and every in-flight
//	refresh is abandoned at its next yield checkpoint with its temporary blob
//	deleted, its pool slot released and its reservation released — while a fetch
//	that has already been promoted to foreground is untouched.
func (h *HotManager) Stop(reason string) {
	if h == nil {
		return
	}
	if reason == "" {
		reason = HotStopOperator
	}
	h.mu.Lock()
	if h.closed && reason != HotStopShutdown {
		h.mu.Unlock()
		return
	}
	already := h.state == HotRunStopped
	h.state = HotRunStopped
	h.stopReason = reason
	h.stops[reason]++
	drained := h.queue.Drain()
	h.skips[HotSkipStopped] += int64(len(drained))
	victims := make([]context.CancelFunc, 0, len(h.running))
	for f, cancel := range h.running {
		// PR-11: a fetch a reader has joined is FOREGROUND from the instant of
		// promotion. The stop has no handle on it and must not cancel it.
		if f.isForeground() {
			continue
		}
		victims = append(victims, cancel)
	}
	h.mu.Unlock()
	if already && len(victims) == 0 {
		return
	}
	for _, cancel := range victims {
		cancel()
	}
	// Q-12: the stop must be fully in force within hot_stop_deadline, and a
	// refresh still running at that instant is counted — a counter that MUST be
	// able to move, driven in Cell 06 by a stalled-server fixture.
	deadline := time.Now().Add(h.cfg.StopDeadline)
	for h.inflight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if h.inflight.Load() > 0 {
		h.stopDeadlineMissed.Add(1)
	}
}

// Resume clears a stop. Q-13: an OPERATOR stop is cleared only by an explicit
// re-arm — no timeout and no burst of successes lifts it — while a
// POOL-PRESSURE stop re-arms automatically once the queue is empty and no
// foreground request is waiting (checked on the manager's own tick).
func (h *HotManager) Resume(reason string) {
	if h == nil {
		return
	}
	if reason == "" {
		reason = HotStopOperator
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	if h.state != HotRunStopped {
		h.mu.Unlock()
		return
	}
	h.state = HotRunArmed
	h.stopReason = ""
	h.pressure = 0
	h.resumes[reason]++
	h.mu.Unlock()
}

// Close stops the hot path in full, abandons what is in flight with reason
// `shutdown`, flushes the tracker and stops the tick. It is idempotent.
func (h *HotManager) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.mu.Unlock()
	h.Stop(HotStopShutdown)
	h.wg.Wait()
	h.Flush()
	h.mu.Lock()
	if h.cancel != nil {
		h.cancel()
	}
	if h.ticker != nil {
		h.ticker.Stop()
	}
	if h.tickDone != nil {
		close(h.tickDone)
		h.tickDone = nil
	}
	h.mu.Unlock()
	return nil
}

// ── the touch/invalidation surface the mount calls ─────────────────────────

// Tracked reports whether a path is in the tracker (D-2: only tracker members
// are ever refresh candidates).
func (h *HotManager) Tracked(path string) bool {
	if h == nil {
		return false
	}
	return h.track.Tracked(path)
}

// NoteRead records a read touch (H-9's dedupe window is inside the tracker).
func (h *HotManager) NoteRead(path string) {
	if h == nil || path == "" {
		return
	}
	h.readCount.Add(1)
	touched, deduped := h.track.TouchRead(path)
	switch {
	case touched:
		h.readTouches.Add(1)
	case deduped:
		h.readDeduped.Add(1)
	}
}

// NoteEdit records an edit touch. It is called only for a write that CHANGED
// content (H-10): the `identical_content` no-op changed nothing, so it is not
// evidence of interest.
func (h *HotManager) NoteEdit(path string) {
	if h == nil || path == "" {
		return
	}
	if h.track.TouchEdit(path) {
		h.editTouches.Add(1)
	}
}

// NoteServed records that a read was answered from a blob the HOT PATH published
// — the figure that lets the subsystem prove it earns its keep (O-1).
func (h *HotManager) NoteServed(path, hash string) {
	if h == nil || hash == "" {
		return
	}
	h.sfMu.Lock()
	_, ok := h.warm[hash]
	h.sfMu.Unlock()
	if !ok {
		return
	}
	h.warmHitsTotal.Add(1)
	h.sfMu.Lock()
	h.warmHitBytes.Add(h.warm[hash])
	h.sfMu.Unlock()
}

// NoteInvalidation reacts to the channel's `invalidate` event: every TRACKED
// path is enqueued; every path the client has never read is a COUNTED skip
// (`untracked`), because the tree never teaches the tracker (D-2/H-18).
func (h *HotManager) NoteInvalidation(paths []string) {
	if h == nil {
		return
	}
	now := h.now()
	for _, p := range paths {
		h.enqueue(p, now)
	}
}

// NoteResync reacts to a gap/overflow/resync (D-11): warming during a resync
// would race the cache's own drop and could publish a blob the drop is about to
// invalidate, so the queue is emptied and enqueues are refused for a quiet
// window, each refusal counted with reason `resync`.
func (h *HotManager) NoteResync(reason string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.resyncUntil = h.now().Add(h.resyncWindow())
	drained := h.queue.Drain()
	h.skips[HotSkipResync] += int64(len(drained))
	h.mu.Unlock()
}

// enqueue is THE decision point for a refresh: every branch below either queues
// the path or increments exactly one skip reason, at the point of the decision
// (S-13 — a counter above which a filter sits is a counter the live path can
// never move).
func (h *HotManager) enqueue(path string, now time.Time) {
	if path == "" {
		return
	}
	h.mu.Lock()
	closed, state, resyncUntil := h.closed, h.state, h.resyncUntil
	h.mu.Unlock()
	switch {
	case closed:
		return
	case state == HotRunStopped:
		h.bump(h.skips, HotSkipStopped)
		h.refusedStopped.Add(1)
		return
	case state != HotRunArmed:
		h.bump(h.skips, HotSkipDisarmed)
		return
	}
	if h.ca.disabled() {
		h.bump(h.skips, HotSkipCacheDisabled)
		return
	}
	if !resyncUntil.IsZero() && now.Before(resyncUntil) {
		h.bump(h.skips, HotSkipResync)
		return
	}
	if h.treeMoved() {
		h.bump(h.skips, HotSkipTreeMismatch)
		return
	}
	if !h.track.Tracked(path) {
		h.bump(h.skips, HotSkipUntracked)
		return
	}
	if size, ok := h.knownSize(path); ok && !hotSizeFits(size, h.cfg.MaxFileBytes) {
		h.bump(h.skips, HotSkipOversizePre)
		h.track.Untrack(path)
		return
	}
	// Q-9's pre-flight: never pull bytes the cache cannot store. When even
	// freeing every unreferenced blob would not make room, the space is held by
	// content a reader is holding and the reason is `pinned_eviction` (D-4).
	if fits, pinned := h.ca.StageWouldFit(path, h.cfg.MaxFileBytes); !fits {
		if pinned {
			h.bump(h.skips, HotSkipPinnedEviction)
		} else {
			h.bump(h.skips, HotSkipNoRoom)
		}
		return
	}
	score, _ := h.track.EffectiveOf(path)
	_, _, _, refused := h.queue.Add(path, score, now)
	if refused {
		h.bump(h.skips, HotSkipQueueFull)
	}
}

// Promote is the requirement this row is graded on: a real READ of a queued path
// takes the work over. The entry LEAVES the queue (counted under its own reason,
// never under expired/displaced/stopped — PR-12), and the read is now on the
// critical path with no backoff and no refresh slot to wait for (PR-1).
func (h *HotManager) Promote(path string) bool {
	if h == nil || path == "" {
		return false
	}
	if !h.queue.Queued(path) {
		return false
	}
	// PR-2: a promotion is a PRIORITY change, never a policy bypass. A path that
	// grew past the ceiling while queued is dropped rather than promoted (S-6).
	if size, ok := h.knownSize(path); ok && !hotSizeFits(size, h.cfg.MaxFileBytes) {
		h.queue.Take(path)
		h.bump(h.skips, HotSkipOversizePre)
		return false
	}
	if h.queue.Promote(path) {
		h.promotions.Add(1)
		return true
	}
	return false
}

// ── the read path (§7) ─────────────────────────────────────────────────────

// HotRead is the mount's read path through the hot subsystem: it PROMOTES a
// queued refresh of the path, JOINS a fetch that is already in flight instead of
// issuing a second GET (PR-5/PR-6), and otherwise fetches exactly once as the
// leader. `joined` reports whether the bytes came from someone else's fetch.
//
// It is correct with the feature OFF in the only way that matters: a nil manager
// performs no work at all, and a caller that gets a nil manager simply fetches
// the way it always did.
func (h *HotManager) HotRead(ctx context.Context, path, ifNoneMatch string) (data []byte, hash string, oerr *OpError, joined bool) {
	if h == nil {
		return nil, "", nil, false
	}
	h.Promote(path)
	f, isJoin := h.beginFetch(path, true)
	if isJoin {
		if d, hh, je := f.Wait(ctx); je == nil && d != nil {
			// (the join is counted by markForeground: one increment, one place)
			return d, hh, nil, true
		}
		// The leader failed or produced nothing. A joiner that fell back issues
		// its own fetch rather than inheriting the leader's failure: the fetch
		// is the truth, and one reader's transport error is not another's.
		h.fallbacks.Add(1)
	}
	d, meta, fe := h.cl.Get(ctx, path, ifNoneMatch)
	hh := ""
	if meta != nil {
		hh = meta.Hash
	}
	if fe == nil && hh == "" && d != nil {
		hh = HashBytes(d)
	}
	if f != nil {
		f.complete(d, hh, fe)
	}
	return d, hh, fe, false
}

// beginFetch returns the fetch record for path: an existing one (isJoin=true —
// the caller must WAIT for it and must NOT complete it) or a fresh one the
// caller owns. A foreground caller marks the fetch as foreground, which is what
// makes a promoted fetch survive a stop (PR-11).
func (h *HotManager) beginFetch(path string, foreground bool) (*hotFetch, bool) {
	h.sfMu.Lock()
	defer h.sfMu.Unlock()
	if h.closed {
		return nil, false
	}
	if f := h.sf[path]; f != nil {
		if foreground {
			f.markForeground()
		}
		return f, true
	}
	f := &hotFetch{mgr: h, path: path, done: make(chan struct{}), foreground: foreground}
	h.sf[path] = f
	// PR-5: the creator of the record IS the leader of the fetch, whether it is
	// a refresh or a foreground reader. `leaders` counts fetches, `joins` counts
	// the readers that shared one — so "N readers ⇒ 1 GET" is readable as
	// `leaders == 1, joins == N`.
	h.leaders.Add(1)
	return f, false
}

// endFetch removes the per-path entry (PR-8). It is idempotent.
func (h *HotManager) endFetch(f *hotFetch) {
	h.sfMu.Lock()
	if h.sf[f.path] == f {
		delete(h.sf, f.path)
	}
	h.sfMu.Unlock()
}

// SingleflightEntries is the reported map size: it must return to 0 at rest
// (PR-8).
func (h *HotManager) SingleflightEntries() int {
	if h == nil {
		return 0
	}
	h.sfMu.Lock()
	defer h.sfMu.Unlock()
	return len(h.sf)
}

// ── the refresher ──────────────────────────────────────────────────────────

func (h *HotManager) tickLoop() {
	for {
		h.mu.Lock()
		tick, done := h.ticker, h.tickDone
		h.mu.Unlock()
		if tick == nil || done == nil {
			return
		}
		select {
		case <-done:
			return
		case <-tick.C:
			h.onTick()
		}
	}
}

// onTick is the manager's one tick (Q-14): the expiry sweep, the tracker flush,
// the pool-pressure check, the automatic re-arm, and as much refresh work as the
// governor allows.
func (h *HotManager) onTick() {
	if h == nil {
		return
	}
	now := h.now()
	// Q-6: an item that has waited a full decay step is no longer the most
	// interesting thing in the queue.
	for _, p := range h.queue.Expire(h.cfg.QueueMaxWait, now) {
		h.bump(h.skips, HotSkipQueueFull) // documented below: expiry is a queue drop
		_ = p
	}
	// P-12: an item put back by a yield that cannot reacquire a slot within the
	// window is ABANDONED, not left waiting forever.
	for range h.queue.SweepReacquire(h.cfg.RefreshReacquireWindow, now) {
		h.bump(h.abandons, HotAbandonReacquireWindow)
	}
	if h.flushDue(now) {
		h.Flush()
	}
	h.mu.Lock()
	state, stopReason, closed := h.state, h.stopReason, h.closed
	h.mu.Unlock()
	if closed {
		return
	}
	// P-19: sustained foreground pressure stops the hot path in full rather than
	// re-acquiring slots it must hand straight back.
	waited := h.cl.WaitedMS()
	if waited >= h.cfg.YieldAfter.Milliseconds() {
		h.mu.Lock()
		h.pressure++
		pressure := h.pressure
		h.mu.Unlock()
		if h.cfg.PoolPressureTicks > 0 && pressure >= h.cfg.PoolPressureTicks && state == HotRunArmed {
			h.Stop(HotStopPoolPressure)
			return
		}
	} else {
		h.mu.Lock()
		h.pressure = 0
		h.mu.Unlock()
	}
	if state == HotRunStopped {
		// Q-13: the ONE automatic re-arm. A pool-pressure stop lifts when the
		// queue is empty AND no foreground request is waiting; an operator stop
		// never lifts by itself.
		if stopReason == HotStopPoolPressure && h.queue.Len() == 0 && waited < h.cfg.YieldAfter.Milliseconds() {
			h.Resume(HotStopPoolPressure)
		}
		return
	}
	if state != HotRunArmed {
		return
	}
	limit := int64(h.governor())
	for h.inflight.Load() < limit {
		it := h.queue.Next(h.now())
		if it == nil {
			return
		}
		if !h.queue.Queued(it.path) {
			return
		}
		h.queue.Take(it.path)
		if !h.launch(it) {
			return
		}
	}
}

// governor is Q-7/P-4's effective width: min(configured cap, the pool share).
func (h *HotManager) governor() int {
	n := h.pol.Derived.RefreshMaxInflight
	if n <= 0 {
		n = 1
	}
	return n
}

// launch starts one refresh under the governor. It returns false when the
// manager is shutting down, so the tick stops scheduling.
func (h *HotManager) launch(it *hotItem) bool {
	f, busy := h.beginFetch(it.path, false)
	if busy {
		// A fetch for this path is already in flight and will publish it: the
		// queued item's work has been taken over, which is a SKIP (`replaced`),
		// not a second GET (PR-6's invariant is at most one bytes-fetching
		// request per path).
		h.bump(h.skips, HotSkipReplaced)
		return true
	}
	ctx, cancel := context.WithTimeout(h.baseCtx, h.cfg.RefreshDeadline)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		cancel()
		h.endFetch(f)
		f.complete(nil, "", nil)
		return false
	}
	h.running[f] = cancel
	h.mu.Unlock()
	h.inflight.Add(1)
	for {
		max := h.inflightMax.Load()
		if h.inflight.Load() <= max || h.inflightMax.CompareAndSwap(max, h.inflight.Load()) {
			break
		}
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer func() {
			h.mu.Lock()
			delete(h.running, f)
			h.mu.Unlock()
			cancel()
			h.inflight.Add(-1)
		}()
		h.refresh(ctx, it, f)
	}()
	return true
}

// refresh is one refresh's whole life. Every exit path releases the slot (the
// deferred cleanup in launch), completes the fetch record, and either published
// a complete blob or discarded everything (§5.6's four obligations).
func (h *HotManager) refresh(ctx context.Context, it *hotItem, f *hotFetch) {
	path := it.path
	// D-9: the served tree is not the pinned one. Warming a tree this mount is
	// no longer bound to would publish content under a stale identity.
	if h.treeMoved() {
		h.bump(h.skips, HotSkipTreeMismatch)
		f.complete(nil, "", nil)
		return
	}
	// S-3 — the CHEAP pre-check, against the size the client already knows: no
	// request at all when the path is definitely too big.
	if size, ok := h.knownSize(path); ok && !hotSizeFits(size, h.cfg.MaxFileBytes) {
		h.bump(h.skips, HotSkipOversizePre)
		h.track.Untrack(path)
		f.complete(nil, "", nil)
		return
	}
	// Q-9 — the pre-flight admission probe: never pull bytes the cache cannot
	// store.
	if fits, pinned := h.ca.StageWouldFit(path, h.cfg.MaxFileBytes); !fits {
		if pinned {
			h.bump(h.skips, HotSkipPinnedEviction)
		} else {
			h.bump(h.skips, HotSkipNoRoom)
		}
		f.complete(nil, "", nil)
		return
	}
	rctx := WithRefreshBudget(ctx)
	// S-4 — the AUTHORITATIVE size check, after the HEAD and before a single
	// byte of the body: a file that grew past the ceiling must not be pulled.
	meta, oerr := h.cl.Head(rctx, path)
	if oerr != nil {
		h.refreshFailed(ctx, it, f, oerr, true)
		return
	}
	if !hotSizeFits(meta.Size, h.cfg.MaxFileBytes) {
		h.bump(h.skips, HotSkipOversizePreHead)
		h.track.Untrack(path)
		f.complete(nil, "", nil)
		return
	}
	h.refreshesTotal.Add(1)
	cached, _, haveCached := h.ca.Lookup(path)
	data, got, stop, gerr := h.cl.GetChecked(rctx, path, cached, h.cfg.MaxFileBytes, func(int) error {
		return h.checkpoint(ctx, f)
	})
	switch {
	case stop != nil:
		h.refreshStopped(ctx, it, f, stop)
		return
	case gerr != nil:
		h.refreshFailed(ctx, it, f, gerr, false)
		return
	}
	if data == nil {
		// 304 Not Modified: the channel gave us the PATH and the CONTENT says
		// unchanged, so nothing is published and no byte is re-downloaded. The
		// decision is the hash's, never the metadata's — BFS-049 landed exactly
		// that rule, and a refresh that decided "changed" from an mtime would
		// reintroduce it.
		h.notModifiedTotal.Add(1)
		f.complete(nil, "", nil)
		return
	}
	h.downloadedBytes.Add(int64(len(data)))
	hash := ""
	if got != nil {
		hash = got.Hash
	}
	if hash == "" {
		hash = HashBytes(data)
	}
	if haveCached && hash == cached {
		h.notModifiedTotal.Add(1)
		f.complete(data, hash, nil)
		return
	}
	// D-9, the abandoned half: the served tree moved while we were reading. The
	// bytes in hand are from a tree this mount is no longer bound to, so they are
	// never published under the pinned identity — this is an ABANDONMENT (the
	// request started), not a skip.
	if h.treeMoved() {
		h.bump(h.abandons, HotAbandonTreeMismatch)
		f.complete(nil, "", nil)
		return
	}
	// ── PUBLISH. There is no checkpoint past this line (§5.6): between "the body
	// is complete" and "the pointer is swapped" a cancel would be corruption
	// rather than a lost optimisation, so the yield checks live INSIDE the body
	// loop and nowhere else.
	st, err := h.ca.Stage(path, hash, int64(len(data)))
	if err != nil {
		if pinned := h.pinnedShortfall(int64(len(data))); pinned {
			h.bump(h.abandons, HotAbandonPinnedEviction)
		} else {
			h.bump(h.abandons, HotAbandonNoRoomAfter)
		}
		f.complete(nil, "", nil)
		return
	}
	if _, werr := st.Write(data); werr != nil {
		_ = st.Abort()
		h.bump(h.abandons, HotAbandonNoRoomAfter)
		f.complete(nil, "", nil)
		return
	}
	if _, cerr := st.Commit(); cerr != nil {
		// Commit's own content-address refusal aborts the stage itself; nothing
		// was published.
		h.bump(h.abandons, HotAbandonNoRoomAfter)
		f.complete(nil, "", nil)
		return
	}
	h.noteWarm(hash, int64(len(data)))
	f.complete(data, hash, nil)
}

// checkpoint is THE yield/hang-up decision, and it is the only place a refresh
// can be stopped mid-flight. It returns nil while the refresh may continue.
func (h *HotManager) checkpoint(ctx context.Context, f *hotFetch) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// PR-11: a fetch a READER has joined is foreground work. STOP IN FULL does
	// not cancel it, does not wait for it, and does not count it as an abandoned
	// refresh — the promoted read is on the critical path now.
	if f != nil && f.isForeground() {
		return nil
	}
	if h.Stopped() {
		return errHotStopped
	}
	// P-7 — Direction 2: a refresh HOLDING a slot releases it at its next
	// checkpoint if any foreground request has been waiting longer than
	// hot_yield_after. A yielded refresh re-enters the queue at its score.
	if h.cl.WaitedMS() >= h.cfg.YieldAfter.Milliseconds() {
		return errHotYield
	}
	return nil
}

// refreshStopped handles each way a refresh's checkpoint can end it. The reason
// matters: a yield is a lost optimisation (the item comes back), a stop and a
// deadline are hang-ups (the item does not come back until the next
// invalidation of that path).
func (h *HotManager) refreshStopped(ctx context.Context, it *hotItem, f *hotFetch, stop error) {
	h.mu.Lock()
	state, stopReason := h.state, h.stopReason
	h.mu.Unlock()
	switch {
	case errors.Is(stop, errHotOversize):
		// The body was longer than the size rule allows even though the HEAD said
		// otherwise: stop reading, publish nothing. The partial buffer is dropped
		// with the fetch record — nothing was staged, so nothing can leak.
		h.bump(h.skips, HotSkipOversizePreHead)
		h.track.Untrack(it.path)
	case errors.Is(stop, errHotYield):
		h.yieldsTotal.Add(1)
		// P-10: a yielded refresh backs off on the SHARED pool — it gives other
		// files their turn instead of immediately re-claiming a slot — but it
		// keeps its rank (P-7: no penalty, never the back).
		h.retry(it)
	case state == HotRunStopped:
		if stopReason == HotStopShutdown {
			h.bump(h.abandons, HotAbandonShutdown)
		} else {
			h.bump(h.abandons, HotAbandonStopped)
		}
	default:
		// The refresh's own deadline (P-11): abandon rather than hold a slot.
		h.bump(h.abandons, HotAbandonDeadline)
	}
	f.complete(nil, "", nil)
}

// refreshFailed handles a request that failed. A dead tree is an abandonment
// (there is nothing to refresh); a transport blip is a RETRY, because dropping
// an invalidation over one 5xx would make the feature quietly useless; and a
// path the server no longer has is a skip at the HEAD and an abandonment at the
// GET (the two are different facts: "it was already gone" vs "it went away while
// we were reading it").
func (h *HotManager) refreshFailed(ctx context.Context, it *hotItem, f *hotFetch, oerr *OpError, head bool) {
	if oerr == nil {
		f.complete(nil, "", nil)
		return
	}
	switch {
	case oerr.Cause == CauseStaleIdentity:
		h.bump(h.abandons, HotAbandonTreeMismatch)
		h.track.Untrack(it.path)
	case oerr.Status == http.StatusNotFound || oerr.Status == http.StatusGone:
		if head {
			h.bump(h.skips, HotSkipNotFound)
			h.track.Untrack(it.path)
		} else {
			h.bump(h.abandons, HotAbandonNotFound)
		}
	case h.Stopped():
		h.mu.Lock()
		stopReason := h.stopReason
		h.mu.Unlock()
		if stopReason == HotStopShutdown {
			h.bump(h.abandons, HotAbandonShutdown)
		} else {
			h.bump(h.abandons, HotAbandonStopped)
		}
	case ctx.Err() != nil:
		// The refresh's OWN deadline (P-11) is the only other way its context
		// ends: hang up rather than hold a slot, and count it as a deadline — a
		// counter that must be able to move, and that must NOT be reported as a
		// stop (two different facts about the client).
		h.bump(h.abandons, HotAbandonDeadline)
	default:
		h.retry(it)
	}
	f.complete(nil, "", nil)
}

// retry re-queues an item behind the backoff ladder (P-10). The ladder is
// 250 ms · 2ⁿ, capped at 30 s, with FULL jitter — the cap equals the client's own
// operation deadline, so a speculative fetch never backs off longer than the
// foreground would wait for the same bytes.
func (h *HotManager) retry(it *hotItem) {
	n := it.attempts + 1
	d := h.backoff(n)
	h.backoffCurrentMS.Store(d.Milliseconds())
	h.retriesTotal.Add(1)
	now := h.now()
	h.queue.Requeue(it.path, it.score, now.Add(d), n, now)
}

// backoff is the lobby's ladder in one function, so a cell can assert the
// numbers rather than the shape.
func (h *HotManager) backoff(attempt int) time.Duration {
	base := h.cfg.BackoffBase
	max := h.cfg.BackoffMax
	f := h.cfg.BackoffFactor
	if base <= 0 {
		base = DefaultHotBackoffBase
	}
	if max <= 0 {
		max = DefaultHotBackoffMax
	}
	if f <= 1 {
		f = DefaultHotBackoffFactor
	}
	d := float64(base)
	for i := 1; i < attempt; i++ {
		d *= f
		if d >= float64(max) {
			d = float64(max)
			break
		}
	}
	if d > float64(max) {
		d = float64(max)
	}
	if h.cfg.BackoffJitter != HotJitterNone {
		d *= h.dice()
	}
	if d < 0 {
		d = 0
	}
	return time.Duration(d)
}

// cell can drive the boundary itself.

// knownSize is the size the client already knows for a path (S-3's pre-check
// source), taken from the cache index — the client's own last observation.
func (h *HotManager) knownSize(path string) (int64, bool) {
	if h.ca == nil {
		return -1, false
	}
	_, size, ok := h.ca.Lookup(path)
	if !ok || size <= 0 {
		return -1, false
	}
	return size, true
}

// pinnedShortfall reports whether a stage the cache just refused could only fit
// if a PINNED blob were evicted (D-4): the hot path abandons with its own reason
// and never evicts.
func (h *HotManager) pinnedShortfall(expected int64) bool {
	_, pinned := h.ca.StageWouldFit("", expected)
	return pinned
}

// treeMoved is D-9: the client is on a different tree than the one this manager
// pinned. An empty identity on either side is not a mismatch (there is nothing
// to disagree about).
func (h *HotManager) treeMoved() bool {
	if h.tree == "" || h.cl == nil {
		return false
	}
	cur := h.cl.Tree()
	return cur != "" && cur != h.tree
}

func (h *HotManager) resyncWindow() time.Duration {
	if h.cfg.TickInterval > 0 {
		return h.cfg.TickInterval
	}
	return time.Second
}

// noteWarm remembers that the hot path published these bytes, so a later read
// served from them can be counted as a WARM HIT (O-1). It is bounded by the
// tracker's own entry bound: the map is trimmed when it exceeds it.
func (h *HotManager) noteWarm(hash string, n int64) {
	if hash == "" {
		return
	}
	h.sfMu.Lock()
	if h.cfg.TrackerMaxEntries > 0 && len(h.warm) >= h.cfg.TrackerMaxEntries {
		h.warm = map[string]int64{}
	}
	h.warm[hash] = n
	h.sfMu.Unlock()
}

// ── reporting ──────────────────────────────────────────────────────────────

// HotStats is the whole hot path as the status record carries it (§9). Every
// bound appears with the counter that moves when it refuses, and every reason
// vocabulary appears with every reason present (a reason that has never fired
// reads 0 rather than going missing).
type HotStats struct {
	State           string           `json:"state"`
	StopReason      string           `json:"stop_reason,omitempty"`
	Armed           bool             `json:"armed"`
	ConfigState     string           `json:"config_state"`
	Tracker         HotTrackerCensus `json:"tracker"`
	Queue           HotQueueCensus   `json:"queue"`
	QueueOldestAgeS int64            `json:"queue_oldest_age_s"`

	RefreshInflight     int `json:"refresh_inflight"`
	RefreshInflightMax  int `json:"refresh_inflight_max"`
	RefreshMaxInflight  int `json:"refresh_max_inflight"`
	PoolSlots           int `json:"pool_slots_for_refresh"`
	PoolSlotsForeground int `json:"pool_slots_for_foreground"`

	ArmsTotal               int64 `json:"arms_total"`
	SkippedTotal            int64 `json:"skipped_total"`
	AbandonedTotal          int64 `json:"abandoned_total"`
	RefreshesTotal          int64 `json:"refreshes_total"`
	NotModifiedTotal        int64 `json:"not_modified_total"`
	PromotionsTotal         int64 `json:"promotions_total"`
	FetchesTotal            int64 `json:"fetches_total"`
	RetriesTotal            int64 `json:"retries_total"`
	YieldsTotal             int64 `json:"yields_total"`
	BytesDownloadedTotal    int64 `json:"bytes_downloaded_total"`
	WarmHitsTotal           int64 `json:"warm_hits_total"`
	WarmHitBytesTotal       int64 `json:"warm_hit_bytes_total"`
	ReadsTotal              int64 `json:"reads_total"`
	ReadTouchesTotal        int64 `json:"read_touches_total"`
	ReadTouchesDedupedTotal int64 `json:"read_touches_deduped_total"`
	EditTouchesTotal        int64 `json:"edit_touches_total"`

	Stops                     map[string]int64 `json:"stops_total"`
	Resumes                   map[string]int64 `json:"resumes_total"`
	RefusedWhileStoppedTotal  int64            `json:"refused_while_stopped_total"`
	StopDeadlineExceededTotal int64            `json:"stop_deadline_exceeded_total"`

	BackoffCurrentMS           int64 `json:"backoff_current_ms"`
	BackoffMaxMS               int64 `json:"backoff_max_ms"`
	ForegroundWaitMSMax        int64 `json:"foreground_wait_ms_max"`
	SingleflightLeadersTotal   int64 `json:"singleflight_leaders_total"`
	SingleflightJoinsTotal     int64 `json:"singleflight_joins_total"`
	SingleflightFallbacksTotal int64 `json:"singleflight_fallbacks_total"`
	SingleflightEntries        int   `json:"singleflight_entries"`
	Flushes                    int64 `json:"flushes_total"`

	Skips     map[string]int64 `json:"skips_total_by_reason"`
	Abandoned map[string]int64 `json:"abandoned_total_by_reason"`

	// The declared bounds, reported beside the figures they bound, so a bound
	// the owner cannot see cannot happen (§2.7).
	MaxFileBytes      int64 `json:"max_file_bytes"`
	SizeRuleInclusive bool  `json:"size_rule_inclusive"`
	QueueMaxWaitMS    int64 `json:"queue_max_wait_ms"`
	RefreshDeadlineMS int64 `json:"refresh_deadline_ms"`
	ReacquireWindowMS int64 `json:"reacquire_window_ms"`
	YieldAfterMS      int64 `json:"yield_after_ms"`
	StopDeadlineMS    int64 `json:"stop_deadline_ms"`
	TickIntervalMS    int64 `json:"tick_interval_ms"`
	BackoffBaseMS     int64 `json:"backoff_base_ms"`

	// Value is O-1's verdict on the subsystem itself: `no_measured_benefit` when
	// bytes were downloaded and no read was ever served from a hot blob. It is a
	// FACT, not an error.
	Value string `json:"value,omitempty"`
}

// Stats snapshots the whole subsystem.
func (h *HotManager) Stats() HotStats {
	if h == nil {
		return HotStats{}
	}
	h.mu.Lock()
	state, stopReason, pressure := h.state, h.stopReason, h.pressure
	stops, resumes := copyCensus(h.stops), copyCensus(h.resumes)
	skips, abandons := copyCensus(h.skips), copyCensus(h.abandons)
	h.mu.Unlock()
	tc := h.track.Census()
	qc := h.queue.Census()
	skipped := int64(0)
	for _, v := range skips {
		skipped += v
	}
	abandoned := int64(0)
	for _, v := range abandons {
		abandoned += v
	}
	st := HotStats{
		State: state, StopReason: stopReason, Armed: state == HotRunArmed,
		ConfigState: h.pol.ConfigState,
		Tracker:     tc, Queue: qc, QueueOldestAgeS: int64(h.queue.OldestAge(h.now()).Seconds()),
		RefreshInflight: int(h.inflight.Load()), RefreshInflightMax: int(h.inflightMax.Load()),
		RefreshMaxInflight: h.pol.Derived.RefreshMaxInflight,
		PoolSlots:          h.pol.Derived.PoolSlots, PoolSlotsForeground: h.pol.Derived.PoolSlotsForeground,
		ArmsTotal: h.armsTotal.Load(), SkippedTotal: skipped, AbandonedTotal: abandoned,
		RefreshesTotal: h.refreshesTotal.Load(), NotModifiedTotal: h.notModifiedTotal.Load(),
		PromotionsTotal: h.promotions.Load(), FetchesTotal: h.fetchesTotal.Load(),
		RetriesTotal: h.retriesTotal.Load(), YieldsTotal: h.yieldsTotal.Load(),
		BytesDownloadedTotal: h.downloadedBytes.Load(),
		WarmHitsTotal:        h.warmHitsTotal.Load(), WarmHitBytesTotal: h.warmHitBytes.Load(),
		ReadsTotal: h.readCount.Load(), ReadTouchesTotal: h.readTouches.Load(),
		ReadTouchesDedupedTotal: h.readDeduped.Load(), EditTouchesTotal: h.editTouches.Load(),
		Stops: stops, Resumes: resumes,
		RefusedWhileStoppedTotal:  h.refusedStopped.Load(),
		StopDeadlineExceededTotal: h.stopDeadlineMissed.Load(),
		BackoffCurrentMS:          h.backoffCurrentMS.Load(), BackoffMaxMS: h.cfg.BackoffMax.Milliseconds(),
		ForegroundWaitMSMax:      h.cl.WaitMaxMS(),
		SingleflightLeadersTotal: h.leaders.Load(), SingleflightJoinsTotal: h.joins.Load(),
		SingleflightFallbacksTotal: h.fallbacks.Load(), SingleflightEntries: h.SingleflightEntries(),
		Flushes: h.flushes.Load(),
		Skips:   skips, Abandoned: abandons,
		MaxFileBytes: h.cfg.MaxFileBytes, SizeRuleInclusive: HotSizeRuleInclusive,
		QueueMaxWaitMS:    h.cfg.QueueMaxWait.Milliseconds(),
		RefreshDeadlineMS: h.cfg.RefreshDeadline.Milliseconds(),
		ReacquireWindowMS: h.cfg.RefreshReacquireWindow.Milliseconds(),
		YieldAfterMS:      h.cfg.YieldAfter.Milliseconds(),
		StopDeadlineMS:    h.cfg.StopDeadline.Milliseconds(),
		TickIntervalMS:    h.cfg.TickInterval.Milliseconds(),
		BackoffBaseMS:     h.cfg.BackoffBase.Milliseconds(),
	}
	if pressure > 0 && state == HotRunStopped {
		st.Value = ""
	}
	if st.WarmHitsTotal == 0 && st.BytesDownloadedTotal > 0 {
		st.Value = "no_measured_benefit"
	}
	return st
}

// Flush writes the tracker if it is dirty (H-21: every hot_flush_interval when
// dirty and once at unmount, so a hard kill loses at most one interval of a
// hint).
func (h *HotManager) Flush() {
	if h == nil || h.dir == "" {
		return
	}
	if _, err := h.track.Save(h.dir, h.key, h.tree); err != nil {
		h.logf("bunker-fs: hot tracker flush failed (%v); the mount is unaffected (the tracker is a hint)", err)
		return
	}
	h.flushes.Add(1)
	h.mu.Lock()
	h.lastFlush = h.now()
	h.mu.Unlock()
}

func (h *HotManager) flushDue(now time.Time) bool {
	h.mu.Lock()
	last := h.lastFlush
	h.mu.Unlock()
	if h.cfg.FlushInterval <= 0 {
		return false
	}
	return !now.Before(last.Add(h.cfg.FlushInterval))
}

func (h *HotManager) bump(m map[string]int64, reason string) {
	h.mu.Lock()
	if _, ok := m[reason]; !ok {
		m[reason] = 0
	}
	m[reason]++
	h.mu.Unlock()
}

// ── client-side hooks (BFS-037's additions to Client) ───────────────────────

// refreshBudgetKey marks a context whose request must acquire from the refresh
// budget instead of the foreground semaphore.
type refreshBudgetKey struct{}

// WithRefreshBudget marks a context as a REFRESH's: the request it carries
// acquires from the refresh budget (P-3), never from the client's foreground
// semaphore (P-6), and its cancellation is not counted as a caller cancel.
func WithRefreshBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, refreshBudgetKey{}, true)
}

func isRefreshRequest(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(refreshBudgetKey{}).(bool)
	return v
}

// EnableRefreshBudget sizes the refresh budget (P-3's share). Zero or negative
// disables it, in which case a refresh acquires nothing — the mount simply never
// arms the hot path without a cache, and a refresh without a budget is a pool
// bypass the spec forbids (P-8).
func (c *Client) EnableRefreshBudget(slots int) {
	if c == nil {
		return
	}
	if slots <= 0 {
		c.rsem = nil
		return
	}
	c.rsem = make(chan struct{}, slots)
}

// RefreshBudget is the size of the refresh budget in slots.
func (c *Client) RefreshBudget() int {
	if c == nil || c.rsem == nil {
		return 0
	}
	return cap(c.rsem)
}

// RefreshRequests reports how many refresh requests were issued and the most
// that were ever in flight — the figure that proves the share is a share.
func (c *Client) RefreshRequests() (total, max int64) {
	if c == nil {
		return 0, 0
	}
	return c.rreq.Load(), c.rreqMax.Load()
}

// markRefreshStart notes one refresh request taking its own slot.
func (c *Client) markRefreshStart() {
	n := c.rreq.Add(1)
	for {
		max := c.rreqMax.Load()
		if n <= max || c.rreqMax.CompareAndSwap(max, n) {
			return
		}
	}
}

// markForegroundWait notes that a foreground request is WAITING for a slot, and
// returns once it has one. The wait is what P-7/P-19 key on: a refresh yields
// while a foreground request is waiting longer than hot_yield_after, and the hot
// path stops itself in full after five consecutive ticks of it (P-19).
func (c *Client) markForegroundWait() {
	c.fgWaiters.Add(1)
	ns := time.Now().UnixNano()
	for {
		cur := c.fgOldestNS.Load()
		if cur != 0 && cur <= ns {
			return
		}
		if c.fgOldestNS.CompareAndSwap(cur, ns) {
			return
		}
	}
}

func (c *Client) clearForegroundWait() {
	if c.fgWaiters.Add(-1) <= 0 {
		c.fgOldestNS.Store(0)
	}
}

// WaitedMS is how long the OLDEST currently-waiting foreground request has
// waited, in milliseconds — 0 when nothing is waiting. It is the measurement the
// yield rule and the self-stop are both built on, and it is taken from the real
// clock because it is a latency, not a policy.
func (c *Client) WaitedMS() int64 {
	if c == nil || c.fgWaiters.Load() <= 0 {
		return 0
	}
	oldest := c.fgOldestNS.Load()
	if oldest == 0 {
		return 0
	}
	ms := (time.Now().UnixNano() - oldest) / int64(time.Millisecond)
	if ms < 0 {
		return 0
	}
	for {
		max := c.fgWaitedMaxMS.Load()
		if ms <= max || c.fgWaitedMaxMS.CompareAndSwap(max, ms) {
			break
		}
	}
	return ms
}

// WaitMaxMS is the longest any foreground request waited for a slot — the
// figure the starvation measurement reports (P-9: the foreground's OWN numbers).
func (c *Client) WaitMaxMS() int64 {
	if c == nil {
		return 0
	}
	return c.fgWaitedMaxMS.Load()
}

// GetChecked is the refresh's GET: it streams the body, calls `check` after each
// chunk (THE yield checkpoint — never past the last byte), and refuses to buffer
// more than `ceiling` bytes even if the server's HEAD lied.
//
// It returns (data, meta, stop, err): `stop` is a checkpoint decision (yield,
// stop, oversize) and is NOT a failure of the request.
func (c *Client) GetChecked(ctx context.Context, path, ifNoneMatch string, ceiling int64, check func(read int) error) ([]byte, *FileMeta, error, *OpError) {
	rctx := WithRefreshBudget(ctx)
	req, err := c.newRequest(rctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, nil, nil, &OpError{Op: "GET", Path: path, Errno: portableErrno(5), Cause: CauseServerError, Err: err}
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ETagFor(ifNoneMatch))
	}
	resp, oerr := c.do(rctx, req)
	if oerr != nil {
		return nil, nil, nil, oerr
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, &FileMeta{Path: path, Hash: ParseETag(resp.Header.Get("ETag"))}, nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, nil, nil, c.failureFrom("GET", path, resp, body)
	}
	var buf bytes.Buffer
	chunk := make([]byte, 64<<10)
	read := 0
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			read += n
			if ceiling > 0 && int64(read) > ceiling {
				return nil, nil, errHotOversize, nil
			}
			buf.Write(chunk[:n])
			if check != nil {
				if cerr := check(read); cerr != nil {
					return nil, nil, cerr, nil
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, nil, nil, classifyRequest(ctx, rerr, "GET", path)
		}
	}
	data := buf.Bytes()
	meta := &FileMeta{Path: path, Size: int64(len(data)), Hash: ParseETag(resp.Header.Get("ETag")), Mtime: headerTime(resp.Header.Get("Last-Modified"))}
	if meta.Hash == "" {
		meta.Hash = HashBytes(data)
	}
	return data, meta, nil, nil
}
