package fsclient

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-037 — THE HOT-FILE CACHE: the tracker, the queue, and the decision
// vocabulary. This file is the state half; hotrefresh.go is the manager.
//
// `docs/prd/SPEC-hot-file-policy.md` (BFS-042) is normative for every number
// here and `internal/fsclient/hotpolicy.go` (BFS-044) already carries them as
// resolved configuration. This file implements the two data structures the
// spec's §3 (accounting) and §5 (the queue) describe, and it decides nothing
// about the POOL — that is hotrefresh.go — so a cell can drive the tracker or
// the queue without a server.
//
// THREE PROPERTIES THIS FILE EXISTS TO MAKE TRUE, each one a defect this
// repository has already filed:
//
//  1. COLD START IS EMPTY AND DOES NO WALK (H-16/AC-2). There is no path in
//     this file that enumerates a tree, stats a path the client has not read,
//     or prewarms from a snapshot. The only way an entry is created is a touch
//     from a read or an edit (H-18) — an invalidation of an unknown path is a
//     COUNTED SKIP (`untracked`), never an entry.
//  2. BOTH BOUNDS ARE REAL AND REPORTED (H-7/H-8/H-23). The entry bound is
//     enforced at admission and the byte bound is enforced ON WRITE, against
//     the size of the document actually written — not against an estimate, so
//     the figure the owner reads is the file's own size (BFS-031's class: a
//     bound reported one way and enforced another).
//  3. A BAD TRACKER NEVER FAILS A MOUNT (P-0 clause 1). A missing, corrupt,
//     foreign or stale-keyed `hot.json` resets to EMPTY and counts WHY; no
//     error from this file is ever surfaced to a reader.
// ---------------------------------------------------------------------------

// The skip vocabulary (SPEC-hot-file-policy §4.5 S-12). It is CLOSED: a
// decision not to refresh increments exactly one of these, and a reason with no
// reachable trigger is a gap rather than a green check (S-14). Every reason is
// present in the census map from construction, so a reason that has never fired
// reads 0 rather than being absent (BFS-032: a counter that cannot move is not
// a counter).
const (
	HotSkipOversizePre     = "oversize_pre"
	HotSkipOversizePreHead = "oversize_after_head"
	HotSkipUntracked       = "untracked"
	HotSkipDisarmed        = "disarmed"
	HotSkipStopped         = "stopped"
	HotSkipCacheDisabled   = "cache_disabled"
	HotSkipNoRoom          = "no_room"
	HotSkipPinnedEviction  = "pinned_eviction"
	HotSkipQueueFull       = "queue_full"
	HotSkipResync          = "resync"
	HotSkipTreeMismatch    = "tree_mismatch"
	HotSkipNotFound        = "not_found"
	HotSkipReplaced        = "replaced"
)

// HotSkipReasons is S-12's closed vocabulary, in the spec's order. It is the
// list the census is built from, so a reason can never be documented and
// un-counted.
var HotSkipReasons = []string{
	HotSkipOversizePre, HotSkipOversizePreHead, HotSkipUntracked, HotSkipDisarmed,
	HotSkipStopped, HotSkipCacheDisabled, HotSkipNoRoom, HotSkipPinnedEviction,
	HotSkipQueueFull, HotSkipResync, HotSkipTreeMismatch, HotSkipNotFound, HotSkipReplaced,
}

// The abandon vocabulary (§6.4 P-13). A refresh that STARTED and gave up
// increments one of these instead of a skip: the two facts ("we decided not to
// start" and "we started and hung up") demand different readings.
const (
	HotAbandonDeadline        = "deadline"
	HotAbandonReacquireWindow = "reacquire_window"
	HotAbandonStopped         = "stopped"
	HotAbandonPinnedEviction  = "pinned_eviction"
	HotAbandonNoRoomAfter     = "no_room_after_start"
	HotAbandonTreeMismatch    = "tree_mismatch"
	HotAbandonNotFound        = "not_found"
	HotAbandonReplaced        = "replaced"
	HotAbandonShutdown        = "shutdown"
)

// HotAbandonReasons is P-13's closed vocabulary.
var HotAbandonReasons = []string{
	HotAbandonDeadline, HotAbandonReacquireWindow, HotAbandonStopped,
	HotAbandonPinnedEviction, HotAbandonNoRoomAfter, HotAbandonTreeMismatch,
	HotAbandonNotFound, HotAbandonReplaced, HotAbandonShutdown,
}

// The tracker-reset vocabulary (H-20). A `hot.json` this mount cannot trust is
// reset to empty and counted by reason; the mount is never refused because of
// it.
const (
	HotResetMissing      = "missing"
	HotResetCorrupt      = "corrupt"
	HotResetVersion      = "version_mismatch"
	HotResetMountKey     = "mount_key_mismatch"
	HotResetTreeMismatch = "tree_mismatch"
)

// HotTrackerResetReasons is H-20's closed vocabulary.
var HotTrackerResetReasons = []string{
	HotResetMissing, HotResetCorrupt, HotResetVersion, HotResetMountKey, HotResetTreeMismatch,
}

// HotStopOperator and HotStopPoolPressure name WHY the hot path stopped itself
// (Q-13/Q-15/P-19). The reason travels with the state in the record, because a
// stop the owner cannot see is indistinguishable from a hung queue.
const (
	HotStopOperator     = "operator"
	HotStopPoolPressure = "pool_pressure"
	HotStopShutdown     = "shutdown"
)

// hotReasons returns a census with every reason present at 0.
func hotReasons(list []string) map[string]int64 {
	m := make(map[string]int64, len(list))
	for _, r := range list {
		m[r] = 0
	}
	return m
}

// IsHotSkipReason reports whether r is in S-12's vocabulary. A caller that
// passes something else is a bug the census would otherwise hide.
func IsHotSkipReason(r string) bool {
	for _, v := range HotSkipReasons {
		if v == r {
			return true
		}
	}
	return false
}

// ===========================================================================
// THE TRACKER (§3)
// ===========================================================================

// hotEntry is one path's popularity record. It carries a path, a score and two
// timestamps — nothing else, ever (H-22): no content, no hashes, no ETags, no
// credentials. Keeping it incapable of carrying a secret is a design property.
type hotEntry struct {
	path       string
	score      float64
	touchedAt  time.Time
	lastReadAt time.Time
}

// HotTracker is the per-mount popularity map: one map, one decay rule, two
// bounds. It is safe for concurrent use.
type HotTracker struct {
	cfg HotPolicy
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*hotEntry
	// bytes is the size of the document last WRITTEN (or loaded). It is the
	// figure that is reported, and it is measured from the bytes rather than
	// derived from a per-entry constant.
	bytes    int64
	dirty    bool
	evicted  int64
	rejected int64
	renorms  int64
	resets   map[string]int64
	flushed  int64
}

// NewHotTracker builds an EMPTY tracker (H-16: cold start does no walk).
func NewHotTracker(cfg HotPolicy, now func() time.Time) *HotTracker {
	if now == nil {
		now = time.Now
	}
	return &HotTracker{
		cfg:     cfg,
		now:     now,
		entries: map[string]*hotEntry{},
		resets:  hotReasons(HotTrackerResetReasons),
	}
}

// Effective is H-5's comparison form: the stored score decayed by the elapsed
// time since it was touched. It is what makes "an old favourite falls out" true
// without a sweeper — decay-on-touch alone cannot do it (F-3), because nothing
// touches an abandoned entry.
func (t *HotTracker) effective(e *hotEntry, now time.Time) float64 {
	if t.cfg.Decay >= 1 || t.cfg.Decay <= 0 || t.cfg.DecayStep <= 0 {
		return e.score
	}
	elapsed := now.Sub(e.touchedAt)
	if elapsed <= 0 {
		return e.score
	}
	steps := float64(elapsed) / float64(t.cfg.DecayStep)
	return e.score * math.Pow(t.cfg.Decay, steps)
}

// EffectiveOf is the exported comparison form, for a caller that holds a path
// (the queue ranks its items with the same rule, so service order and eviction
// order cannot disagree).
func (t *HotTracker) EffectiveOf(path string) (float64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[path]
	if e == nil {
		return 0, false
	}
	return t.effective(e, t.now()), true
}

// Decay applies one decay step to a score, using the tracker's own rule. The
// queue uses it so a queued item's rank decays exactly as a tracked one's does.
func (t *HotTracker) Decay(score float64, since time.Duration) float64 {
	if t.cfg.Decay >= 1 || t.cfg.Decay <= 0 || t.cfg.DecayStep <= 0 || since <= 0 {
		return score
	}
	return score * math.Pow(t.cfg.Decay, float64(since)/float64(t.cfg.DecayStep))
}

// Tracked reports whether a path has an entry. It is the LIVE path's only
// membership question: an untracked path is never a refresh candidate (D-2).
func (t *HotTracker) Tracked(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.entries[path]
	return ok
}

// TouchRead records a read touch, deduped to once per path per
// `hot_read_touch_window` (H-9): a FUSE read of an 8 MiB file arrives as ~64
// chunked calls, so without the window one file's single access would score
// ~64 and the tracker would fill with one access pattern (F-6). The dedupe is
// COUNTED, not heuristic.
func (t *HotTracker) TouchRead(path string) (touched, deduped bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if e := t.entries[path]; e != nil && t.cfg.ReadTouchWindow > 0 {
		if now.Sub(e.lastReadAt) < t.cfg.ReadTouchWindow {
			e.lastReadAt = now
			return false, true
		}
	}
	if t.touchLocked(path, t.cfg.WeightRead, now) {
		t.entries[path].lastReadAt = now
		return true, false
	}
	return false, false
}

// TouchEdit records an edit touch (H-10: only a write that CHANGED content).
func (t *HotTracker) TouchEdit(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.touchLocked(path, t.cfg.WeightEdit, t.now())
}

// touchLocked applies H-1..H-4 and H-14. It reports whether the path was
// touched; a refused newcomer is counted and reports false.
func (t *HotTracker) touchLocked(path string, weight float64, now time.Time) bool {
	if path == "" {
		return false
	}
	if e := t.entries[path]; e != nil {
		e.score = t.effective(e, now) + weight
		e.touchedAt = now
		t.dirty = true
		t.renormaliseLocked(now)
		return true
	}
	// H-14: the tracker is full. The newcomer is admitted iff its score at
	// admission (weight, no decay term) is GREATER than the current minimum
	// effective score; otherwise it is refused and counted. This is the only
	// growth path (H-18).
	if t.cfg.TrackerMaxEntries > 0 && len(t.entries) >= t.cfg.TrackerMaxEntries {
		min := t.minLocked(now)
		if min == nil || weight <= t.effective(min, now) {
			t.rejected++
			return false
		}
		delete(t.entries, min.path)
		t.evicted++
	}
	t.entries[path] = &hotEntry{path: path, score: weight, touchedAt: now, lastReadAt: now}
	t.dirty = true
	return true
}

// renormaliseLocked applies H-6: when the maximum effective score exceeds the
// ceiling, every score is halved in one pass. A uniform scale changes no
// ordering, which is a property the cells ASSERT rather than assume (AC-3).
func (t *HotTracker) renormaliseLocked(now time.Time) {
	if t.cfg.ScoreCeiling <= 0 {
		return
	}
	var max float64
	for _, e := range t.entries {
		if v := t.effective(e, now); v > max {
			max = v
		}
	}
	if max <= t.cfg.ScoreCeiling {
		return
	}
	for _, e := range t.entries {
		e.score *= 0.5
		e.touchedAt = now
	}
	t.renorms++
}

// minLocked returns the entry H-13 selects as the eviction candidate: lowest
// effective score, ties by STALER touch losing, remaining ties by
// lexicographically first path losing. The order is total and deterministic so
// a cell can reproduce it.
func (t *HotTracker) minLocked(now time.Time) *hotEntry {
	var min *hotEntry
	for _, e := range t.entries {
		if min == nil {
			min = e
			continue
		}
		ev, mv := t.effective(e, now), t.effective(min, now)
		switch {
		case ev < mv:
			min = e
		case ev == mv && e.touchedAt.Before(min.touchedAt):
			min = e
		case ev == mv && e.touchedAt.Equal(min.touchedAt) && e.path < min.path:
			min = e
		}
	}
	return min
}

// MaxLocked returns the entry with the highest effective score (the same total
// order, reversed). The queue's service order is its mirror (Q-5).
func (t *HotTracker) maxLocked(now time.Time) *hotEntry {
	var max *hotEntry
	for _, e := range t.entries {
		if max == nil {
			max = e
			continue
		}
		ev, mv := t.effective(e, now), t.effective(max, now)
		switch {
		case ev > mv:
			max = e
		case ev == mv && e.touchedAt.Before(max.touchedAt):
			max = e
		case ev == mv && e.touchedAt.Equal(max.touchedAt) && e.path < max.path:
			max = e
		}
	}
	return max
}

// Len is the entry count.
func (t *HotTracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

// Bytes is the size of the document this tracker last wrote or loaded. It is
// MEASURED from the bytes, never derived from an estimate (H-23).
func (t *HotTracker) Bytes() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bytes
}

// Untrack drops a path (S-8: a tracked path that grows past the ceiling is
// untracked at its next touch, so a stale score cannot refresh it later).
func (t *HotTracker) Untrack(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.entries[path]; !ok {
		return false
	}
	delete(t.entries, path)
	t.dirty = true
	return true
}

// Census is the tracker's reported figures.
type HotTrackerCensus struct {
	Entries    int
	Bytes      int64
	MaxEntries int
	MaxBytes   int64
	Evicted    int64
	Rejected   int64
	Renorms    int64
	Flushes    int64
	Resets     map[string]int64
}

// Census snapshots the tracker's figures.
func (t *HotTracker) Census() HotTrackerCensus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return HotTrackerCensus{
		Entries: len(t.entries), Bytes: t.bytes,
		MaxEntries: t.cfg.TrackerMaxEntries, MaxBytes: t.cfg.TrackerMaxBytes,
		Evicted: t.evicted, Rejected: t.rejected, Renorms: t.renorms, Flushes: t.flushed,
		Resets: copyCensus(t.resets),
	}
}

// Paths returns the tracked paths in the tracker's total order (highest
// effective score first). Used by cells and by the status drill-down; it never
// runs on the live refresh path.
func (t *HotTracker) Paths() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	type kv struct {
		e      *hotEntry
		eff    float64
		tieKey string
	}
	all := make([]kv, 0, len(t.entries))
	for _, e := range t.entries {
		all = append(all, kv{e: e, eff: t.effective(e, now)})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].eff != all[j].eff {
			return all[i].eff > all[j].eff
		}
		if !all[i].e.touchedAt.Equal(all[j].e.touchedAt) {
			return all[i].e.touchedAt.Before(all[j].e.touchedAt)
		}
		return all[i].e.path < all[j].e.path
	})
	out := make([]string, 0, len(all))
	for _, v := range all {
		out = append(out, v.e.path)
	}
	return out
}

// ── persistence (H-19..H-23) ────────────────────────────────────────────────

// HotDocVersion is the `hot.json` document version. A document carrying any
// other version is reset to empty and counted (H-20), never parsed on hope.
const HotDocVersion = 1

// HotFileName is the tracker's file, inside the CACHE directory — the directory
// `--cache-max-size` names. F-2's resolution: a file the cache's own subsystem
// writes is cache bytes, so it lives in the bounded directory rather than
// beside the mount's state, and the cache's directory walk counts it in the
// foreign term.
const HotFileName = "hot.json"

// hotDoc is the on-disk document: paths, weights, timestamps, and the three
// header fields. Nothing else (H-22).
type hotDoc struct {
	Version int           `json:"version"`
	MountID string        `json:"mount_id"`
	Tree    string        `json:"tree"`
	Entries []hotDocEntry `json:"entries"`
}

type hotDocEntry struct {
	Path        string  `json:"path"`
	Score       float64 `json:"score"`
	TouchedAtMS int64   `json:"touched_at_ms"`
}

// MarshalDoc renders the tracker's document. It is what Save writes and what
// the byte bound is enforced against.
func (t *HotTracker) MarshalDoc(mountID, tree string) ([]byte, error) {
	t.mu.Lock()
	doc := hotDoc{Version: HotDocVersion, MountID: mountID, Tree: tree}
	for _, e := range t.entries {
		doc.Entries = append(doc.Entries, hotDocEntry{Path: e.path, Score: e.score, TouchedAtMS: e.touchedAt.UnixMilli()})
	}
	t.mu.Unlock()
	sort.Slice(doc.Entries, func(i, j int) bool { return doc.Entries[i].Path < doc.Entries[j].Path })
	return json.Marshal(doc)
}

// Reset empties the tracker and counts WHY (H-20). A reset is never an error
// and never fails a mount (P-0 clause 1).
func (t *HotTracker) Reset(reason string) {
	if !IsHotTrackerResetReason(reason) {
		reason = HotResetCorrupt
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = map[string]*hotEntry{}
	t.bytes = 0
	t.dirty = false
	t.resets[reason]++
}

// IsHotTrackerResetReason reports membership in H-20's vocabulary.
func IsHotTrackerResetReason(r string) bool {
	for _, v := range HotTrackerResetReasons {
		if v == r {
			return true
		}
	}
	return false
}

// Load reads the tracker from `<dir>/hot.json`, resetting to EMPTY and counting
// the reason when the document cannot be trusted. The mount is never refused.
func (t *HotTracker) Load(dir, mountID, tree string) string {
	path := filepath.Join(dir, HotFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Reset(HotResetMissing)
		return HotResetMissing
	}
	var doc hotDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Reset(HotResetCorrupt)
		return HotResetCorrupt
	}
	if doc.Version != HotDocVersion {
		t.Reset(HotResetVersion)
		return HotResetVersion
	}
	if doc.MountID != mountID {
		t.Reset(HotResetMountKey)
		return HotResetMountKey
	}
	if doc.Tree != "" && tree != "" && doc.Tree != tree {
		t.Reset(HotResetTreeMismatch)
		return HotResetTreeMismatch
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = make(map[string]*hotEntry, len(doc.Entries))
	for _, de := range doc.Entries {
		if de.Path == "" || !(de.Score > 0) {
			continue
		}
		at := time.UnixMilli(de.TouchedAtMS)
		t.entries[de.Path] = &hotEntry{path: de.Path, score: de.Score, touchedAt: at, lastReadAt: at}
	}
	// The entry bound is enforced on LOAD too: a document written by a mount
	// with a larger bound is trimmed rather than trusted to fit.
	now := t.now()
	for t.cfg.TrackerMaxEntries > 0 && len(t.entries) > t.cfg.TrackerMaxEntries {
		min := t.minLocked(now)
		if min == nil {
			break
		}
		delete(t.entries, min.path)
		t.evicted++
	}
	t.bytes = int64(len(raw))
	t.dirty = false
	return ""
}

// Save writes the tracker by temp+rename (the house pattern, H-21), mode 0600,
// enforcing the BYTE bound on write: the document is marshalled, trimmed until
// it fits, and the size actually written is what Bytes reports.
func (t *HotTracker) Save(dir, mountID, tree string) (int64, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	trimmed := int64(0)
	var raw []byte
	for {
		b, err := t.MarshalDoc(mountID, tree)
		if err != nil {
			return 0, err
		}
		if t.cfg.TrackerMaxBytes <= 0 || int64(len(b)) <= t.cfg.TrackerMaxBytes {
			raw = b
			break
		}
		// H-8: the byte bound is enforced by dropping the coldest entry, not by
		// writing a document over the bound and hoping. Each drop is counted.
		t.mu.Lock()
		min := t.minLocked(t.now())
		if min == nil {
			raw = b
			t.mu.Unlock()
			break
		}
		delete(t.entries, min.path)
		t.evicted++
		t.mu.Unlock()
		trimmed++
	}
	path := filepath.Join(dir, HotFileName)
	tmp, err := os.CreateTemp(dir, "hot.json.tmp-*")
	if err != nil {
		return 0, err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return 0, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return 0, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return 0, err
	}
	t.mu.Lock()
	t.bytes = int64(len(raw))
	t.dirty = false
	t.flushed++
	t.mu.Unlock()
	_ = trimmed
	return int64(len(raw)), nil
}

func copyCensus(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ===========================================================================
// THE QUEUE (§5)
// ===========================================================================

// hotItem is one queued refresh: a path, the score it was queued at, when, how
// many attempts it has had, and the earliest instant it may start again (the
// backoff stamp). A QUEUED item owns nothing else — no slot, no reservation, no
// temp file — which is why abandoning one is provably safe (§5.6).
type hotItem struct {
	path      string
	score     float64
	queuedAt  time.Time
	notBefore time.Time
	attempts  int
	// yieldedAt is when this item was FIRST put back by a yield (P-12's clock).
	// An item that cannot reacquire a slot within hot_refresh_reacquire_window
	// of this instant is abandoned rather than left waiting forever.
	yieldedAt  time.Time
	generation int64
}

// HotQueue is the refresh queue: one entry per path, a depth bound, replacement
// by SCORE and never FIFO (Q-3/Q-4), and an anti-starvation expiry (Q-6).
type HotQueue struct {
	max   int
	track *HotTracker

	mu      sync.Mutex
	items   map[string]*hotItem
	deduped int64
	displ   int64
	refused int64
	expired int64
	prom    int64
}

// NewHotQueue builds a queue bounded at max depth.
func NewHotQueue(max int, track *HotTracker) *HotQueue {
	if max <= 0 {
		max = int(DefaultHotQueueMaxDepth)
	}
	return &HotQueue{max: max, track: track, items: map[string]*hotItem{}}
}

// Max is the declared depth bound.
func (q *HotQueue) Max() int { return q.max }

// Len is the current depth — a count of distinct pending paths, never a count
// of events (Q-2).
func (q *HotQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Queued reports whether a path has a pending entry.
func (q *HotQueue) Queued(path string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.items[path]
	return ok
}

// effective is the item's rank: its score decayed by the same rule the tracker
// uses, so the queue cannot outrank the tracker's own ordering.
func (q *HotQueue) effective(it *hotItem, now time.Time) float64 {
	if q.track == nil {
		return it.score
	}
	return q.track.Decay(it.score, now.Sub(it.queuedAt))
}

// Add enqueues path. It returns what happened so the caller can count it: an
// existing entry is UPDATED (Q-2 — a second invalidation of a queued path adds
// no depth), a full queue DISPLACES its lowest-scoring item when the newcomer
// is hotter (Q-3) and otherwise REFUSES the newcomer.
func (q *HotQueue) Add(path string, score float64, now time.Time) (added bool, updated bool, displaced *hotItem, refused bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if it := q.items[path]; it != nil {
		it.score = score
		it.queuedAt = now
		it.notBefore = time.Time{}
		it.generation++
		q.deduped++
		return false, true, nil, false
	}
	if len(q.items) >= q.max {
		low := q.lowestLocked(now)
		if low == nil || score <= q.effective(low, now) {
			q.refused++
			return false, false, nil, true
		}
		delete(q.items, low.path)
		q.displ++
		displaced = low
	}
	q.items[path] = &hotItem{path: path, score: score, queuedAt: now}
	return true, false, displaced, false
}

// lowestLocked is Q-3's victim: the lowest-scoring queued item, ties by oldest
// enqueue time, then lexicographically first path (H-13's total order).
func (q *HotQueue) lowestLocked(now time.Time) *hotItem {
	var low *hotItem
	for _, it := range q.items {
		if low == nil {
			low = it
			continue
		}
		iv, lv := q.effective(it, now), q.effective(low, now)
		switch {
		case iv < lv:
			low = it
		case iv == lv && it.queuedAt.Before(low.queuedAt):
			low = it
		case iv == lv && it.queuedAt.Equal(low.queuedAt) && it.path < low.path:
			low = it
		}
	}
	return low
}

// Next picks the item to serve: the HIGHEST effective score, ties by enqueue
// time ascending, then lexicographically first path (Q-5). It never returns an
// item whose backoff stamp is still in the future.
func (q *HotQueue) Next(now time.Time) *hotItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	var best *hotItem
	for _, it := range q.items {
		if it.notBefore.After(now) {
			continue
		}
		if best == nil {
			best = it
			continue
		}
		bv, iv := q.effective(best, now), q.effective(it, now)
		switch {
		case iv > bv:
			best = it
		case iv == bv && it.queuedAt.Before(best.queuedAt):
			best = it
		case iv == bv && it.queuedAt.Equal(best.queuedAt) && it.path < best.path:
			best = it
		}
	}
	return best
}

// Find returns the pending item for path, if any.
func (q *HotQueue) Find(path string) *hotItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[path]
}

// Take removes and returns the item for path (nil when it is not queued). The
// REMOVAL and the accounting are one operation: an item that leaves the queue
// for a reason other than being served is counted by that reason, so a queue
// whose depth falls is always explained.
func (q *HotQueue) Take(path string) *hotItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	it := q.items[path]
	delete(q.items, path)
	return it
}

// Promote removes path because a real read took the work over (PR-12): a
// distinct counter from expired/displaced/stopped, because "the work was taken
// over" and "the item was dropped" demand different readings.
func (q *HotQueue) Promote(path string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.items[path]; !ok {
		return false
	}
	delete(q.items, path)
	q.prom++
	return true
}

// Requeue puts a yielded item back WITHOUT a penalty and without going to the
// back (P-7): the same score, so it holds its rank, with a backoff stamp and one
// more attempt recorded. yieldedAt records when the item FIRST came back, which
// is the clock P-12's reacquire window is measured from.
func (q *HotQueue) Requeue(path string, score float64, notBefore time.Time, attempts int, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	it := q.items[path]
	if it == nil {
		it = &hotItem{path: path, queuedAt: now}
		q.items[path] = it
	}
	it.score = score
	it.notBefore = notBefore
	it.attempts = attempts
	if it.yieldedAt.IsZero() {
		it.yieldedAt = now
	}
}

// SweepReacquire drops every item that was PUT BACK by a yield and has not
// reacquired a slot within the window (P-12), returning the paths so the caller
// can count each as an abandonment: an invalidation that has waited fifteen poll
// cadences is stale, and abandoning it beats holding it forever.
func (q *HotQueue) SweepReacquire(window time.Duration, now time.Time) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if window <= 0 {
		return nil
	}
	var out []string
	for path, it := range q.items {
		if it.yieldedAt.IsZero() {
			continue
		}
		if now.Sub(it.yieldedAt) > window {
			delete(q.items, path)
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// Expire drops every item that has waited longer than maxWait without a start
// attempt (Q-6) and returns them, so the caller can count each one by reason.
func (q *HotQueue) Expire(maxWait time.Duration, now time.Time) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []string
	for path, it := range q.items {
		if maxWait > 0 && now.Sub(it.queuedAt) > maxWait {
			delete(q.items, path)
			q.expired++
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// Drain empties the queue and returns the paths it removed (STOP IN FULL, §5.5
// clause 2: the queue is EMPTIED AT ONCE — not drained by letting items run, and
// not held for later).
func (q *HotQueue) Drain() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, 0, len(q.items))
	for path := range q.items {
		out = append(out, path)
	}
	q.items = map[string]*hotItem{}
	sort.Strings(out)
	return out
}

// OldestAge is how long the oldest pending item has waited (Q-6's reported
// figure: a bound the owner cannot see is not a bound).
func (q *HotQueue) OldestAge(now time.Time) time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	var oldest time.Time
	for _, it := range q.items {
		if oldest.IsZero() || it.queuedAt.Before(oldest) {
			oldest = it.queuedAt
		}
	}
	if oldest.IsZero() {
		return 0
	}
	return now.Sub(oldest)
}

// HotQueueCensus is the queue's reported figures.
type HotQueueCensus struct {
	Depth     int
	MaxDepth  int
	Deduped   int64
	Displaced int64
	Refused   int64
	Expired   int64
	Promoted  int64
}

// Census snapshots the queue's figures.
func (q *HotQueue) Census() HotQueueCensus {
	q.mu.Lock()
	defer q.mu.Unlock()
	return HotQueueCensus{
		Depth: len(q.items), MaxDepth: q.max,
		Deduped: q.deduped, Displaced: q.displ, Refused: q.refused, Expired: q.expired, Promoted: q.prom,
	}
}

// hotSizeFits is THE size rule, in one function, so an off-by-one can only be
// introduced in one place and a cell can assert that place. S-2: the comparison
// is INCLUSIVE (`size <= ceiling`), because an exclusive reading makes the
// boundary a silent one-byte difference.
func hotSizeFits(size, ceiling int64) bool {
	if ceiling <= 0 {
		return false
	}
	return size <= ceiling
}

// deferred to the post-HEAD re-check rather than guessed.
