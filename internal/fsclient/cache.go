package fsclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Cache defaults — decided, not measured (BFS-005 §9):
//
//	--cache-max-size        268,435,456 B (256 MiB)   the owner's decision (PRD AC-5)
//	--cache-max-entry-bytes min(max_size, 64 MiB)      one entry may not monopolise a quarter of the bound
//	--cache-max-age         1 h                        a backstop TTL; invalidation is the mechanism
const (
	// DefaultCacheMaxBytes is the hard cap on the bytes this client keeps on disk.
	DefaultCacheMaxBytes int64 = 268435456
	// DefaultCacheMaxEntryBytes caps a single cached file.
	DefaultCacheMaxEntryBytes int64 = 67108864
	// DefaultCacheMaxAge is the backstop TTL of one entry.
	DefaultCacheMaxAge = time.Hour
	// DefaultCacheMaxEntries is the ENTRY bound of the cache directory, and it is
	// a separate bound from the byte one for a measured reason: a byte bound
	// alone does not bound a directory (BFS-031: at a 1 KiB byte bound the
	// directory reached 30,689 B). Every entry also costs a serialised index
	// record the blob census never sees, so a tree of tiny files reaches the
	// entry bound while the byte figure still reads comfortably low.
	//
	// 16384 entries × the measured ≈180 B record ≈ 2.9 MiB of index, 1.1% of
	// the 256 MiB byte bound; it pairs with that bound so a uniform blob size at
	// the entry bound is 16 KiB. BFS-044 owns the flag that exposes this knob;
	// the enforcement and the reported figure live here.
	DefaultCacheMaxEntries = 16384
	// DefaultCacheMaxInFlight is how many STAGED refreshes may hold unpublished
	// bytes at once. It is what makes the in-flight reservation a bound instead
	// of a hope: the reservation ceiling is MaxInFlight × MaxEntryBytes, the
	// 2 × 8 MiB = 16 MiB of the hot-file policy's Q-7 / Appendix A.5.
	DefaultCacheMaxInFlight = 2
	// CacheIndexFile is the path index inside the cache directory.
	CacheIndexFile = "index.json"
	// CacheBlobDir holds the content-addressed whole-file blobs.
	CacheBlobDir = "blobs"
	// CacheStagePrefix names a STAGED (unpublished) blob inside blobs/. It sits
	// in the blob directory deliberately: those bytes are really on disk, so the
	// directory's real size — the thing a bound must bound — includes them, and
	// load's orphan sweep removes the residue of a crashed stage.
	CacheStagePrefix = ".stage-"
)

// CacheConfig configures a Cache. MaxBytes = 0 disables the cache entirely:
// every read goes to the server and every figure reports 0 (BFS-005 §3.1).
type CacheConfig struct {
	Dir           string
	MaxBytes      int64
	MaxEntryBytes int64
	// MaxEntries bounds the directory in ENTRIES. 0 means
	// DefaultCacheMaxEntries. The bound is always in force: a byte bound alone
	// does not bound a directory (BFS-031), so there is no unbounded mode.
	MaxEntries int
	// MaxInFlight bounds how many staged refreshes may hold unpublished bytes.
	// 0 means DefaultCacheMaxInFlight.
	MaxInFlight int
	MaxAge      time.Duration
	// DirMeasureInterval is how long a directory measurement is reused before it
	// is taken again. 0 means DirMeasureTTL; a NEGATIVE value measures on every
	// read. It is a knob because the cost is a walk of the directory (O(files))
	// and the benefit is an independent figure: a deployment with a very large
	// cache can trade freshness for cost, and a test can ask for a fresh walk per
	// read. The reported figure always carries its own age
	// (dir_measured_age_ms), so a reused sample is never read as a fresh one.
	DirMeasureInterval time.Duration
	// Now is the clock seam; nil means time.Now.
	Now func() time.Time
}

// Outcome is what Insert did with an entry. Bypass outcomes are reported (never
// disguised as a healthy bound) so "the bound is respected by caching nothing"
// is visible rather than invisible (BFS-005 §3.4).
type Outcome string

const (
	// OutcomeHit means an entry for (path, hash) was already present.
	OutcomeHit Outcome = "hit"
	// OutcomeStored means the bytes are now a cache entry.
	OutcomeStored Outcome = "stored"
	// OutcomeBypass means the cache is full of unpinned-but-unevictable data,
	// so the read was served straight through and nothing was cached.
	OutcomeBypass Outcome = "bypass"
	// OutcomeOversize means the single entry exceeds --cache-max-entry-bytes
	// and is never cached.
	OutcomeOversize Outcome = "oversize_bypass"
	// OutcomeDisabled means --cache-max-size 0.
	OutcomeDisabled Outcome = "disabled"
)

// Cached reports whether the outcome left the bytes on disk.
func (o Outcome) Cached() bool { return o == OutcomeStored || o == OutcomeHit }

// ---------------------------------------------------------------------------
// BFS-045: the null-reason vocabulary.
//
// The standing rule is that a NULL must carry a REASON, and that a reason is a
// CLASS, not a sentence. Four classes cover every absent figure in the status
// record; the sentence after the colon is the detail a person reads. An
// unexplained null on a live route is junk, not data.
// ---------------------------------------------------------------------------
const (
	// ReasonDisabled — the feature is switched off, so the figure has no source
	// by configuration rather than by failure.
	ReasonDisabled = "disabled"
	// ReasonUnknown — the source exists but could not be read, so the figure is
	// genuinely unknown. Never rendered as 0.
	ReasonUnknown = "unknown"
	// ReasonNotPublished — the source does not publish this figure: a build that
	// predates the field, a document that names no block, or a feature (the
	// hot-refresh queue, BFS-037) that is not in this build.
	ReasonNotPublished = "not_published"
	// ReasonNoSample — nothing has happened yet, so there is no measurement to
	// report: the figure will be there the moment the event occurs.
	ReasonNoSample = "no_sample"
)

// Bypass reason vocabulary: every refusal to store bytes in the cache, by where
// the decision was made. It is CLOSED — BypassReasons() returns all of it, and
// the reported map always carries every key — so adding a refusal site without
// adding its reason is a visible omission rather than an uncounted path.
const (
	// BypassReasonOverEntryCap is the LIVE READ PATH's refusal: the served
	// content is larger than the per-entry cap, so no blob is written. This is
	// the site BFS-032 proved was uncounted: the read path pre-filtered above
	// Cache.Insert, so `oversize_bypasses` could never move from a real read.
	BypassReasonOverEntryCap = "over_entry_cap"
	// BypassReasonInsertOverEntryCap is Cache.Insert's own oversize branch: a
	// caller that did NOT pre-filter. It is kept separate from the read path's
	// reason so the two sites are countable apart — one counter for both would
	// hide which site is actually refusing.
	BypassReasonInsertOverEntryCap = "insert_over_entry_cap"
	// BypassReasonNoRoom is the bound refusing the insert: no room under the
	// byte or entry bound after eviction.
	BypassReasonNoRoom = "no_room"
	// BypassReasonDisabled is --cache-max-size 0: the cache cannot store.
	BypassReasonDisabled = "cache_disabled"
)

// BypassReasons returns the closed vocabulary, so the census is readable by a
// consumer without reading this file.
func BypassReasons() []string {
	return []string{
		BypassReasonOverEntryCap,
		BypassReasonInsertOverEntryCap,
		BypassReasonNoRoom,
		BypassReasonDisabled,
	}
}

// The directory classes DirBytesByClass reports: what the independent
// measurement found, split by the thing that put it there. The split is what
// makes `dir_unaccounted_bytes` a NAMED delta instead of an unexplained gap.
const (
	DirClassIndex     = "index"     // index.json (+ its temp) — the serialised index
	DirClassBlobs     = "blobs"     // published, indexed blobs
	DirClassStaged    = "staged"    // staged (unpublished) refresh blobs
	DirClassOrphan    = "orphan"    // blob files no index entry references
	DirClassStatus    = "status"    // status.json (+ its temp)
	DirClassConflicts = "conflicts" // conflicts.jsonl
	DirClassOther     = "other"     // anything else in the directory
)

// dirClasses is the closed class vocabulary.
func dirClasses() []string {
	return []string{DirClassIndex, DirClassBlobs, DirClassStaged, DirClassOrphan, DirClassStatus, DirClassConflicts, DirClassOther}
}

// DirMeasureTTL is how long a directory measurement is reused. It is a variable
// so a test can drive the cadence rather than wait, and so an operator can be
// told the cost: one walk of the cache directory per TTL, never one per status
// write. The figure reports its own age beside it (dir_measured_age_ms).
var DirMeasureTTL = 30 * time.Second

// CacheStats is the reported figure set of BFS-005 §3.2. used_bytes is the
// figure AC-5 compares with `du`; max_bytes is what it must never exceed.
//
// TWO ACCOUNTS, NOT ONE (BFS-038 / F-1). `used_bytes` is the PUBLISHED figure —
// blobs + index, what eviction reasons about, du-comparable at rest. It is not
// by itself a bound on the directory, because a staged refresh's bytes are on
// disk before they are published: `in_flight_bytes` counts those, and
// `reserved_bytes` (= used + in-flight + the index growth they will publish) is
// the directory's real peak and the figure admission keeps under max_bytes.
// Both are reported, separately, because a bound that is only true of the state
// you chose to count is not a bound (BFS-031's lesson, one layer up).
type CacheStats struct {
	MaxBytes         int64 `json:"max_bytes"`
	BlobsBytes       int64 `json:"blobs_bytes"`
	IndexBytes       int64 `json:"index_bytes"`
	UsedBytes        int64 `json:"used_bytes"`
	Entries          int   `json:"entries"`
	Blobs            int   `json:"blobs"`
	EvictionsTotal   int64 `json:"evictions_total"`
	BypassEvents     int64 `json:"bypass_events"`
	OversizeBypasses int64 `json:"oversize_bypasses"`
	PinnedBlobs      int   `json:"pinned_blobs"`
	// MaxEntries/Entries is the SECOND bound: bytes alone do not bound a
	// directory (BFS-031), so the entry count is bounded and reported too.
	MaxEntries int `json:"max_entries"`
	// MaxEntryBytes is the per-entry cap: the bound whose refusals are counted
	// below. A bound must be readable next to the counter that moves when it
	// refuses (BFS-045; SPEC-hot-file-policy §9.1).
	MaxEntryBytes int64 `json:"max_entry_bytes"`
	// MaxAgeMS is the backstop TTL of one entry (--cache-max-age): a bound the
	// cache expires entries against. Reported for the same reason as the others —
	// a bound the owner cannot see is not a bound — and as a POSITIVE number so
	// its presence needs no separate reason.
	MaxAgeMS int64 `json:"max_age_ms"`
	// InFlightBytes are the bytes staged refreshes have written and not yet
	// published: really on disk, unreachable by any reader.
	InFlightBytes int64 `json:"in_flight_bytes"`
	// ReservedBytes is used + in-flight + the staged index growth: the peak the
	// bound is enforced against, as opposed to the occupancy it is reported at.
	ReservedBytes int64 `json:"reserved_bytes"`
	// StagedBlobs/MaxInFlight report the width of the refresh window whose bytes
	// the reservation covers; without a width bound the reservation is unbounded.
	StagedBlobs int `json:"staged_blobs"`
	MaxInFlight int `json:"max_inflight"`
	// The staged-refresh flow counters. Each one is reachable from the live path
	// and each one is proven to move (BFS-032: a counter that cannot move is a
	// gap, not a green check).
	StagedCommittedTotal int64 `json:"staged_committed_total"`
	StagedAbortedTotal   int64 `json:"staged_aborted_total"`
	StagedNoRoomTotal    int64 `json:"staged_no_room_total"`
	StagedNoSlotTotal    int64 `json:"staged_no_slot_total"`
	// Hits/Misses are the read-path counters; not part of the §3.2 minimum but
	// the only way to show the cache actually served anything.
	Hits   int64 `json:"hits"`
	Misses int64 `json:"misses"`
	// StagedStartedTotal is the number of refreshes ADMITTED to the staged
	// window. It is the denominator of the flow: committed + aborted + (still in
	// flight) must account for it, so a refresh that vanished without a verdict
	// is visible as a gap rather than as nothing.
	StagedStartedTotal int64 `json:"staged_started_total"`
	// BypassReasons counts every refusal to STORE, by reason, from a closed
	// vocabulary (BypassReasons()). Every reason is always a key, so a reason
	// that has never fired reads as 0 and a reason the code cannot reach is
	// visibly absent from the census rather than silently zero (BFS-032's
	// lesson: a counter that can never move is a gap, not a green check).
	//
	// This is the figure that replaces BFS-032's dead counter: the live read
	// path pre-filtered above Cache.Insert, so `oversize_bypasses` could never
	// move; the refusal is now counted WHERE IT IS DECIDED, with its reason.
	BypassReasons map[string]int64 `json:"bypass_reasons"`
	// DirBytes is an INDEPENDENT measurement of the cache directory: the bytes
	// actually on disk, by class, walked from the filesystem rather than derived
	// from the accounting below. It exists because BFS-031's defect was a
	// reported figure that described something other than the thing it claimed
	// to bound: `used_bytes` counts published blobs + the serialised index and
	// does NOT count status.json, conflicts.jsonl, staged blobs or a temp index,
	// so the two must be readable side by side.
	//
	// It is measured on a bounded cadence (DirMeasureTTL) because a walk per
	// status write would cost O(files) per second; DirMeasuredAgeMS reports how
	// long ago the sample was taken, so a stale sample is never read as current.
	DirBytes            int64            `json:"dir_bytes"`
	DirBytesByClass     map[string]int64 `json:"dir_bytes_by_class"`
	DirBytesReason      string           `json:"dir_bytes_reason,omitempty"`
	DirMeasuredAgeMS    *int64           `json:"dir_measured_age_ms"`
	DirUnaccountedBytes int64            `json:"dir_unaccounted_bytes"`
}

// cacheEntry is one path's index record: which blob holds its bytes, and when
// that blob was last *hit* (the LRU key is last hit, not last insert — a hot
// path stays, a one-shot bulk read leaves).
type cacheEntry struct {
	Path      string `json:"path"`
	Hash      string `json:"hash"`
	Size      int64  `json:"size"`
	LastHitMS int64  `json:"last_hit_ms"`
	// Gen is the invalidation generation this entry was stored in; a resync
	// bumps the generation and every older entry is stale by construction.
	Gen int64 `json:"gen"`
}

// blobRef is the content-addressed half: one on-disk blob, referenced by one
// or more path entries, pinned by zero or more live handles.
type blobRef struct {
	Hash string
	Size int64
	Refs int
	Pins int
}

type indexDoc struct {
	Version int          `json:"version"`
	Entries []cacheEntry `json:"entries"`
}

// The staged-refresh refusals. Each is a REFUSAL and never an eviction: eviction
// reasons about published blobs only, and a refresh may never cause one (Q-8).
var (
	// ErrNoRoom is admission refusing a staged refresh: published + in-flight
	// bytes (or entries) leave no room under the bound. The caller maps it to the
	// policy's `no_room` skip reason (S-12) and must not evict to make room.
	ErrNoRoom = errors.New("fsclient: no room for a staged refresh")
	// ErrNoSlot is the in-flight WIDTH refusal: every stage slot is taken. Unlike
	// ErrNoRoom this is not a skip — the caller keeps its item queued until a
	// slot frees.
	ErrNoSlot = errors.New("fsclient: refresh width reached")
	// ErrStageClosed is Write/Commit on a stage that already ended.
	ErrStageClosed = errors.New("fsclient: staged refresh already finished")
	// ErrOversize is a staged write that would pass the per-entry cap; the caller
	// maps it to `oversize_after_head` (S-12) and abandons before pulling more.
	ErrOversize = errors.New("fsclient: staged blob is over the per-entry cap")
	// ErrCacheDisabled is Stage on a cache switched off by --cache-max-size 0.
	ErrCacheDisabled = errors.New("fsclient: cache is disabled")
)

// Cache is a bounded, content-addressed, whole-file cache with a path index.
//
// Consequences of the content-addressing (BFS-005 §3.2), all deliberate: two
// paths with identical content cost ONE blob; the read that fills the cache is
// the read that decides the conflict (its hash is the write's If-Match base);
// and whole-file blobs make `blobs_bytes` agree with `du` up to block rounding.
//
// The path index is also the ATOMICITY MECHANISM (BFS-038): a reader reaches a
// blob only through `entries` under `mu`, so a blob that is not indexed is
// unreachable — not in part, not at all. That is what lets a refresh write a new
// blob and publish it with one pointer swap instead of mutating bytes a reader
// may be reading.
type Cache struct {
	cfg CacheConfig

	mu       sync.Mutex
	entries  map[string]*cacheEntry // path -> entry
	blobs    map[string]*blobRef    // hash -> blob
	inflight map[string]bool        // blobs feeding an in-flight write: never evicted
	// staged holds the refreshes that are holding unpublished bytes on disk.
	// Membership is the in-flight census; each stage's reservation is what
	// admission reasons about.
	staged map[*StagedRefresh]struct{}
	gen    int64
	stats  CacheStats
	// bypassReasons is the live census behind CacheStats.BypassReasons. It is
	// kept as its own map (not inside stats) so recount never has to rebuild it.
	bypassReasons map[string]int64
	// The directory measurement (BFS-031's shape, made visible): what an
	// independent walk of the directory found, when it was taken, and why it is
	// absent when it could not be taken.
	dirMeasuredAt time.Time
	dirBytes      int64
	dirClasses    map[string]int64
	dirErr        string
	// dirBlobClasses/dirBlobAt are the BLOB half of that measurement, which is
	// the expensive half (O(blobs)) and the only half that is reused. The
	// top-level entries are re-stat'ed on every publish, because a figure that
	// does not include the file the mount is writing RIGHT NOW would disagree
	// with `du` at exactly the moment someone checks it — measured on the live
	// route while writing this row's probe: the reported figure read 0 while the
	// directory held 4230 bytes.
	dirBlobClasses map[string]int64
	dirBlobAt      time.Time
}

// OpenCache creates (or reopens) the cache directory. The directory is 0700 and
// every blob is 0600: the cache is the client's own private data.
func OpenCache(cfg CacheConfig) (*Cache, error) {
	if cfg.Dir == "" {
		return nil, errors.New("fsclient: cache dir is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxBytes < 0 {
		return nil, fmt.Errorf("fsclient: cache max bytes must be >= 0 (got %d)", cfg.MaxBytes)
	}
	if cfg.MaxEntryBytes <= 0 || cfg.MaxEntryBytes > cfg.MaxBytes && cfg.MaxBytes > 0 {
		cfg.MaxEntryBytes = cfg.MaxBytes
		if cfg.MaxEntryBytes > DefaultCacheMaxEntryBytes {
			cfg.MaxEntryBytes = DefaultCacheMaxEntryBytes
		}
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = DefaultCacheMaxAge
	}
	// The two bounds that are not about a single blob. Both are always in force:
	// a cache with no entry bound is a directory the byte bound does not bound
	// (BFS-031), and a cache with no in-flight bound is a reservation that is not
	// a bound (F-1). 0 therefore means "the default", never "unlimited".
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = DefaultCacheMaxEntries
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = DefaultCacheMaxInFlight
	}
	if err := os.MkdirAll(filepath.Join(cfg.Dir, CacheBlobDir), 0o700); err != nil {
		return nil, fmt.Errorf("fsclient: create cache dir: %w", err)
	}
	if err := os.Chmod(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("fsclient: chmod cache dir: %w", err)
	}
	c := &Cache{
		cfg:      cfg,
		entries:  map[string]*cacheEntry{},
		blobs:    map[string]*blobRef{},
		inflight: map[string]bool{},
		staged:   map[*StagedRefresh]struct{}{},
		stats:    CacheStats{MaxBytes: cfg.MaxBytes},
		// Every reason in the closed vocabulary is present from the start: a
		// zero that is a VALUE and a missing key that is an ABSENCE must never
		// look the same in the status record (BFS-045).
		bypassReasons: map[string]int64{},
	}
	for _, r := range BypassReasons() {
		c.bypassReasons[r] = 0
	}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

// load reads the path index back from disk and re-derives the blob census from
// what is actually present, so a crash can never leave the figures inflated: a
// blob counted in the index but missing on disk is dropped from the index.
func (c *Cache) load() error {
	raw, err := os.ReadFile(filepath.Join(c.cfg.Dir, CacheIndexFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("fsclient: read cache index: %w", err)
	}
	var doc indexDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		// A corrupt index is a cache, not a source of truth: start empty
		// rather than failing the mount. The blobs directory is swept below.
		doc = indexDoc{}
	}
	seen := map[string]bool{}
	for i := range doc.Entries {
		e := doc.Entries[i]
		if e.Path == "" || !IsHash(e.Hash) {
			continue
		}
		info, err := os.Stat(c.blobPath(e.Hash))
		if err != nil || info.Size() != e.Size {
			continue // index entry without its blob: drop it
		}
		cp := e
		c.entries[cp.Path] = &cp
		// seen is keyed by the blob's FILE name, which is the hash without its
		// algorithm prefix. Keying it by the full `sha256:…` value made every
		// blob look orphaned, so the sweep below DELETED the whole cache on
		// every reopen — measured while building this row: the entry survived in
		// the index and its bytes were gone from disk.
		seen[strings.TrimPrefix(cp.Hash, HashPrefix)] = true
		if b := c.blobs[cp.Hash]; b == nil {
			c.blobs[cp.Hash] = &blobRef{Hash: cp.Hash, Size: cp.Size, Refs: 1}
		} else {
			b.Refs++
		}
	}
	c.sweepOrphanBlobs(seen)
	c.recountLocked()
	return nil
}

// sweepOrphanBlobs removes blob files no index entry references — the residue
// of an interrupted insert. Called only from load, before the cache is shared.
func (c *Cache) sweepOrphanBlobs(keep map[string]bool) {
	dir := filepath.Join(c.cfg.Dir, CacheBlobDir)
	names, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, n := range names {
		if n.IsDir() {
			continue
		}
		if !keep[n.Name()] {
			_ = os.Remove(filepath.Join(dir, n.Name()))
		}
	}
}

func (c *Cache) blobPath(hash string) string {
	return filepath.Join(c.cfg.Dir, CacheBlobDir, strings.TrimPrefix(hash, HashPrefix))
}

// disabled reports whether the cache is switched off (`--cache-max-size 0`).
func (c *Cache) disabled() bool { return c.cfg.MaxBytes == 0 }

// Get serves a path from the cache. A hit requires the hash to match: the
// contract is that a cache entry is valid iff its hash equals the server's, so
// a caller that knows the current hash cannot be served stale bytes.
func (c *Cache) Get(path, hash string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled() {
		c.stats.Misses++
		return nil, false
	}
	e := c.entries[path]
	if e == nil || !IsHash(hash) || e.Hash != hash {
		c.stats.Misses++
		return nil, false
	}
	if c.expired(e) {
		c.dropPathLocked(path)
		c.stats.Misses++
		return nil, false
	}
	data, err := os.ReadFile(c.blobPath(e.Hash))
	if err != nil {
		c.dropPathLocked(path)
		c.stats.Misses++
		return nil, false
	}
	e.LastHitMS = c.cfg.Now().UnixMilli()
	c.stats.Hits++
	return data, true
}

// GetPinned serves path's bytes AND takes the refcount on the blob holding them
// in ONE critical section; release it with Unpin. Get-then-Pin is two critical
// sections, and the window between them is a blob a reader is reading with
// nothing keeping it alive: a Commit that swaps the path's pointer (or a Drop)
// inside that window removes the entry, and if that was the blob's last
// reference the file goes with it. The bytes a caller already holds are its own
// copy either way — the refcount is what keeps the CONTENT ON DISK for the
// handle's lifetime, which is what "a reader holding the old blob keeps it
// alive, so eviction must respect in-flight reads" (BFS-038) means.
func (c *Cache) GetPinned(path, hash string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled() {
		c.stats.Misses++
		return nil, false
	}
	e := c.entries[path]
	if e == nil || !IsHash(hash) || e.Hash != hash {
		c.stats.Misses++
		return nil, false
	}
	if c.expired(e) {
		c.dropPathLocked(path)
		c.stats.Misses++
		return nil, false
	}
	data, err := os.ReadFile(c.blobPath(e.Hash))
	if err != nil {
		c.dropPathLocked(path)
		c.stats.Misses++
		return nil, false
	}
	e.LastHitMS = c.cfg.Now().UnixMilli()
	c.stats.Hits++
	if b := c.blobs[e.Hash]; b != nil {
		b.Pins++
	}
	return data, true
}

// ── the atomic refresh representation (BFS-038, PRD §2.5) ────────────────────
//
// A refresh must never be visible half-written, and that is a REPRESENTATION
// problem, not a locking one: a lock can serialise two writers, it cannot help a
// reader that is already reading the bytes being rewritten. So a refresh writes
// to a NEW immutable blob and publishes it with ONE pointer swap.
//
// Why the swap is atomic here: `entries` under `mu` is the ONLY path from a
// reader to a blob. A staged blob is not in `entries`, so it is unreachable —
// not in part, not at all — until Commit's single map store. The staged FILE is
// renamed to its content address one step earlier, while nothing references it,
// so the pointer can never name a partial file. Between "byte written" and
// "pointer swapped" there is no state a reader can observe.
//
// The two corollaries that make this a contract rather than a hope:
//   - an abandoned or cancelled refresh DISCARDS its blob and never swaps
//     (Abort; idempotent, so a cancel that never arrives is not corrupting);
//   - a reader holding the old blob keeps it alive (Pin/Unpin) and eviction
//     skips it, so the swap can take the old CONTENT away only after the last
//     reader has released it.

// StagedRefresh is one refresh's unpublished blob, between "bytes are arriving"
// and "published". Its whole contract: bytes go to a new file no reader can
// reach; Commit verifies the content address and performs one pointer swap;
// Abort deletes the file, releases the reservation, and never swaps; the
// reservation is held for the entire staging window, so the directory's peak is
// what admission reserved.
type StagedRefresh struct {
	c           *Cache
	path        string
	hash        string
	file        *os.File
	tmpName     string
	hasher      hash.Hash
	entryGrowth int64 // index bytes this stage will publish (booked at admission)
	estimate    int64 // bytes the caller declared it will write (booked at admission)
	reserved    int64 // bytes booked against the bound, guarded by c.mu
	written     atomic.Int64
	mu          sync.Mutex // serialises Write/Commit/Abort on this stage
	done        bool
}

// Stage begins a refresh of path: it reserves admission, opens a fresh
// unpublished blob inside the cache directory, and returns a writer for it.
// Nothing any reader can observe changes until Commit.
//
// expectedBytes is the size the caller expects to write (0 when it does not
// know). It is what admission books up front; the reservation is also enforced
// as the bytes arrive, so a caller whose expectation was wrong is refused rather
// than allowed to overshoot the bound.
func (c *Cache) Stage(path, hash string, expectedBytes int64) (*StagedRefresh, error) {
	if path == "" || !IsHash(hash) {
		return nil, fmt.Errorf("fsclient: a staged refresh needs a path and a well-formed hash")
	}
	if expectedBytes < 0 {
		expectedBytes = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled() {
		return nil, ErrCacheDisabled
	}
	if len(c.staged) >= c.cfg.MaxInFlight {
		c.stats.StagedNoSlotTotal++
		return nil, ErrNoSlot
	}
	if err := c.admitStagedLocked(path, expectedBytes); err != nil {
		c.stats.StagedNoRoomTotal++
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Join(c.cfg.Dir, CacheBlobDir), CacheStagePrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("fsclient: create staged blob: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("fsclient: chmod staged blob: %w", err)
	}
	s := &StagedRefresh{
		c:           c,
		path:        path,
		hash:        hash,
		file:        f,
		tmpName:     f.Name(),
		hasher:      NewHasher(),
		entryGrowth: c.indexGrowthLocked(path),
		estimate:    expectedBytes,
	}
	s.reserved = expectedBytes + s.entryGrowth
	c.staged[s] = struct{}{}
	c.stats.StagedStartedTotal++
	return s, nil
}

// admitStagedLocked is the ADMISSION half of the two accounts (Q-9): the refresh
// is admitted only if published + in-flight + this blob — and the entry it will
// add — fit under the bounds. Eviction is deliberately not consulted, and
// nothing is evicted to make room: eviction reasons about published blobs only
// and a refresh may never cause one (Q-8).
func (c *Cache) admitStagedLocked(path string, expectedBytes int64) error {
	// Every staged refresh that will add an entry consumes an entry slot, and
	// only a path that is not indexed adds one.
	newEntries := 0
	if c.entries[path] == nil && !c.stagedHasPathLocked(path) {
		newEntries = 1
	}
	if len(c.entries)+newEntries > c.cfg.MaxEntries {
		return fmt.Errorf("%w: %d entries at the entry bound %d", ErrNoRoom, len(c.entries), c.cfg.MaxEntries)
	}
	need := c.reservedLocked() + expectedBytes + c.indexGrowthLocked(path)
	if need > c.cfg.MaxBytes {
		return fmt.Errorf("%w: %d reserved + %d needed exceeds the byte bound %d", ErrNoRoom, c.reservedLocked(), expectedBytes, c.cfg.MaxBytes)
	}
	return nil
}

// stagedHasPathLocked reports whether another in-flight refresh already targets
// path (so the second one does not book a second entry slot).
func (c *Cache) stagedHasPathLocked(path string) bool {
	for s := range c.staged {
		if s.path == path {
			return true
		}
	}
	return false
}

// inFlightBytesLocked is what the staged refreshes have already written into the
// cache directory: really on disk (du sees it), unreachable by any reader.
func (c *Cache) inFlightBytesLocked() int64 {
	var n int64
	for s := range c.staged {
		n += s.written.Load()
	}
	return n
}

// stagedReservedLocked is what the in-flight refreshes have booked: their bytes
// (written or expected) plus the index growth they will publish.
func (c *Cache) stagedReservedLocked() int64 {
	var n int64
	for s := range c.staged {
		n += s.reserved
	}
	return n
}

// usedLocked is the PUBLISHED occupancy — what eviction reasons about, and what
// `used_bytes` reports.
func (c *Cache) usedLocked() int64 {
	var blobs int64
	for _, b := range c.blobs {
		blobs += b.Size
	}
	return blobs + c.indexBytesLocked()
}

// reservedLocked is the directory's peak as admission sees it: published bytes
// plus every in-flight refresh's reservation. This is the figure that must stay
// under the bound; at rest it equals usedLocked. F-1's arithmetic is why it
// exists: a cache at 99% plus two 8 MiB refreshes in flight is 16 MiB over the
// bound while the published figure still reads "inside".
func (c *Cache) reservedLocked() int64 {
	return c.usedLocked() + c.stagedReservedLocked()
}

// reserveStagedLocked tops a stage's booking up so that it covers
// max(declared, written) + the index it will publish, refusing when the booked
// total would pass the byte bound. It is a top-up rather than an addition on
// purpose: the declared size is booked at Stage, and re-booking each arriving
// chunk on top of it would refuse every refresh that declared its size honestly.
// The booking is monotone — it never falls while the stage lives — so two
// concurrent writers cannot both be admitted against the same free space.
func (c *Cache) reserveStagedLocked(s *StagedRefresh, n int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	want := s.written.Load() + n
	if want < s.estimate {
		want = s.estimate
	}
	delta := want + s.entryGrowth - s.reserved
	if delta <= 0 {
		return true // already covered by the declared size
	}
	if c.reservedLocked()+delta > c.cfg.MaxBytes {
		return false
	}
	s.reserved += delta
	return true
}

// releaseStagedLocked ends a stage's reservation and its in-flight membership.
// Its bytes are published (they are `used_bytes` now) or gone, so neither figure
// may still count them.
func (c *Cache) releaseStagedLocked(s *StagedRefresh) {
	delete(c.staged, s)
	s.reserved = 0
}

// Write appends to the staged blob, enforcing both structural bounds as the
// bytes arrive: the per-entry cap (a blob larger than MaxEntryBytes could never
// be published, so it is refused before more bandwidth is spent) and the
// admission reservation (the DIRECTORY's bound, not the published figure).
func (s *StagedRefresh) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return 0, ErrStageClosed
	}
	if s.written.Load()+int64(len(p)) > s.c.cfg.MaxEntryBytes {
		return 0, fmt.Errorf("%w: %d bytes would exceed %d", ErrOversize, s.written.Load()+int64(len(p)), s.c.cfg.MaxEntryBytes)
	}
	if !s.c.reserveStagedLocked(s, int64(len(p))) {
		return 0, fmt.Errorf("%w: the staged refresh would pass the cache bound", ErrNoRoom)
	}
	n, err := s.file.Write(p)
	if n > 0 {
		s.written.Add(int64(n))
		_, _ = s.hasher.Write(p[:n])
	}
	if err != nil {
		return n, fmt.Errorf("fsclient: write staged blob: %w", err)
	}
	// A short write needs no correction here: the booking is
	// max(declared, written) + index growth, so a chunk that did not land simply
	// does not raise it — and it is deliberately not LOWERED either, because a
	// reservation that fell mid-stream would admit a second refresh against
	// bytes that are still coming.
	return n, nil
}

// Commit publishes the staged blob with ONE pointer swap, and verifies the
// content address before it does. It is the only operation in this file that
// changes what a reader of `path` can see, and it changes it in a single store
// into the path index. It is single-use: Commit or Abort ends the stage.
func (s *StagedRefresh) Commit() (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return OutcomeBypass, ErrStageClosed
	}
	// The content address is verified BEFORE anything is published: bytes that
	// do not hash to the key they would be filed under are never named as that
	// content, which is the same refusal Insert makes.
	if got := HashTag(s.hasher.Sum(nil)); got != s.hash {
		err := fmt.Errorf("fsclient: refusing to publish %s under %s (content hashes to %s)", s.path, s.hash, got)
		s.abortLocked()
		return OutcomeBypass, err
	}
	size := s.written.Load()
	if err := s.file.Close(); err != nil {
		s.abortLocked()
		return OutcomeBypass, fmt.Errorf("fsclient: close staged blob: %w", err)
	}
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	// 1. The new blob takes its content address. It is an immutable file that
	//    nothing references yet, so this step is invisible to every reader: the
	//    index is the only way to reach a blob.
	blob := c.blobs[s.hash]
	if blob == nil {
		if err := os.Rename(s.tmpName, c.blobPath(s.hash)); err != nil {
			_ = os.Remove(s.tmpName)
			c.releaseStagedLocked(s)
			s.done = true
			return OutcomeBypass, fmt.Errorf("fsclient: publish staged blob: %w", err)
		}
		c.blobs[s.hash] = &blobRef{Hash: s.hash, Size: size}
		blob = c.blobs[s.hash]
	} else {
		// Identical content is already cached: the staged file is redundant, and
		// the pointer still moves to a complete blob.
		_ = os.Remove(s.tmpName)
	}
	// 2. THE ONE POINTER SWAP, in one critical section, with the reference taken
	//    before the old entry is dropped so a re-publish of the same content
	//    cannot reclaim the blob it is about to point at. A reader either holds
	//    the old entry — and the complete old blob it names, alive for as long as
	//    its pin is held — or it gets this one. There is no third state.
	blob.Refs++
	if old := c.entries[s.path]; old != nil {
		c.dropPathLocked(s.path)
	}
	c.entries[s.path] = &cacheEntry{
		Path:      s.path,
		Hash:      s.hash,
		Size:      blob.Size,
		LastHitMS: c.cfg.Now().UnixMilli(),
		Gen:       c.gen,
	}
	// 3. The reservation ends exactly where the published figure begins: these
	//    bytes were in_flight_bytes a moment ago and are used_bytes now, so the
	//    bound is never briefly unenforced.
	c.releaseStagedLocked(s)
	s.done = true
	c.stats.StagedCommittedTotal++
	c.recountLocked()
	_ = c.flushLocked()
	return OutcomeStored, nil
}

// Abort discards the staged blob: the file goes, the reservation goes, and the
// path index is never touched. It is IDEMPOTENT — an abandonment that arrives
// twice, or after a Commit, is a no-op rather than an error — because an
// accidental cancel may never arrive at all (PRD §2.8), and a duplicate one must
// not be corrupting either.
func (s *StagedRefresh) Abort() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	s.abortLocked()
	return nil
}

// abortLocked is Abort's body, called with s.mu held and c.mu NOT held.
func (s *StagedRefresh) abortLocked() {
	if s.file != nil {
		_ = s.file.Close()
	}
	_ = os.Remove(s.tmpName)
	c := s.c
	c.mu.Lock()
	if !s.done {
		c.releaseStagedLocked(s)
		c.stats.StagedAbortedTotal++
		s.done = true
	}
	c.mu.Unlock()
}

// Bytes is how many bytes the stage has written so far — the figure a caller
// reports as downloaded, and the one the in-flight census counts.
func (s *StagedRefresh) Bytes() int64 { return s.written.Load() }

// Path and Hash name what the stage will publish if it is committed.
func (s *StagedRefresh) Path() string { return s.path }
func (s *StagedRefresh) Hash() string { return s.hash }

// Lookup reports whether an entry exists for a path and what hash it holds,
// without reading the bytes. The mount needs this to decide whether a write has
// a base hash at all (BFS-005 §5.3).
func (c *Cache) Lookup(path string) (hash string, size int64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[path]
	if e == nil || c.expired(e) {
		return "", 0, false
	}
	return e.Hash, e.Size, true
}

// expired implements the backstop TTL. A zero MaxAge never expires.
func (c *Cache) expired(e *cacheEntry) bool {
	if c.cfg.MaxAge == 0 {
		return false
	}
	return c.cfg.Now().Sub(time.UnixMilli(e.LastHitMS)) > c.cfg.MaxAge
}

// Insert stores data as the cache entry for path. It is where the hard cap is
// enforced: the entry is evicted-for first, and if it still cannot fit it is
// BYPASSED — the client never grows past the bound, never fails a read for a
// local-capacity reason, and never blocks on a pinned handle (BFS-005 §3.4).
func (c *Cache) Insert(path, hash string, data []byte) (Outcome, error) {
	if path == "" || !IsHash(hash) {
		return OutcomeBypass, fmt.Errorf("fsclient: cache insert needs a path and a well-formed hash")
	}
	if got := HashBytes(data); got != hash {
		// The caller's bytes do not hash to the key. Refusing is the only safe
		// answer: storing them would make the cache lie about its own content
		// address, and the hash is the currency of the write path.
		return OutcomeBypass, fmt.Errorf("fsclient: refusing to store %s under %s (content hashes to %s)", path, hash, got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled() {
		c.countBypassLocked(BypassReasonDisabled)
		return OutcomeDisabled, nil
	}
	if int64(len(data)) > c.cfg.MaxEntryBytes {
		c.stats.OversizeBypasses++
		c.countBypassLocked(BypassReasonInsertOverEntryCap)
		return OutcomeOversize, nil
	}
	if e := c.entries[path]; e != nil && e.Hash == hash && !c.expired(e) {
		e.LastHitMS = c.cfg.Now().UnixMilli()
		c.stats.Hits++
		return OutcomeHit, nil
	}

	// Project the post-insert size and evict until it fits. indexBytes is
	// measured from the serialised document, not estimated, so `used_bytes`
	// stays exact and comparable with `du`. The entry dimension is checked with
	// the byte one: an insert that would add an entry past the entry bound
	// evicts too (BFS-031).
	blob := c.blobs[hash]
	newBlobBytes := int64(0)
	if blob == nil {
		newBlobBytes = int64(len(data))
	}
	newEntries := 1
	if c.entries[path] != nil {
		newEntries = 0
	}
	if err := c.makeRoomLocked(newBlobBytes, c.indexGrowthLocked(path), newEntries); err != nil {
		c.stats.BypassEvents++
		c.countBypassLocked(BypassReasonNoRoom)
		return OutcomeBypass, nil
	}

	if blob == nil {
		if err := c.writeBlob(hash, data); err != nil {
			return OutcomeBypass, err
		}
		c.blobs[hash] = &blobRef{Hash: hash, Size: int64(len(data)), Refs: 0}
		blob = c.blobs[hash]
	}
	if old := c.entries[path]; old != nil {
		c.dropPathLocked(path)
	}
	c.entries[path] = &cacheEntry{
		Path:      path,
		Hash:      hash,
		Size:      int64(len(data)),
		LastHitMS: c.cfg.Now().UnixMilli(),
		Gen:       c.gen,
	}
	blob.Refs++
	c.recountLocked()
	if err := c.flushLocked(); err != nil {
		return OutcomeStored, err
	}
	return OutcomeStored, nil
}

// writeBlob writes a blob atomically (temp + rename) so an interrupted insert
// can never leave a half-written blob that a later read would serve.
func (c *Cache) writeBlob(hash string, data []byte) error {
	final := c.blobPath(hash)
	tmp, err := os.CreateTemp(filepath.Join(c.cfg.Dir, CacheBlobDir), ".tmp-*")
	if err != nil {
		return fmt.Errorf("fsclient: create blob temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("fsclient: write blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("fsclient: close blob: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("fsclient: chmod blob: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("fsclient: publish blob: %w", err)
	}
	return nil
}

// indexGrowthLocked is the exact byte cost the index will grow by for a new
// path entry per path entry (0 for a path already indexed).
func (c *Cache) indexGrowthLocked(path string) int64 {
	if c.entries[path] != nil {
		return 0
	}
	enc, err := json.Marshal(cacheEntry{Path: path, Hash: HashPrefix + strings.Repeat("0", 64), Size: 1 << 30, LastHitMS: 1 << 40, Gen: c.gen})
	if err != nil {
		return 256
	}
	return int64(len(enc)) + 1
}

// makeRoomLocked evicts until newBlobBytes (+ pending index growth, + the new
// entry slots) fit under BOTH bounds. It returns an error when nothing can be
// freed — the caller then bypasses rather than growing or blocking.
//
// The entry count is a bound of its own because a byte bound alone does not
// bound a directory (BFS-031): each entry also costs a serialised index record
// the blob census never sees, so a tree of tiny files reaches the entry bound
// while the byte figure still reads comfortably low.
func (c *Cache) makeRoomLocked(newBlobBytes, indexGrowth int64, newEntries int) error {
	for c.usedLocked()+newBlobBytes+indexGrowth > c.cfg.MaxBytes ||
		len(c.entries)+newEntries > c.cfg.MaxEntries {
		if !c.evictOneLocked() {
			return errors.New("fsclient: cache full and nothing evictable")
		}
	}
	return nil
}

// evictOneLocked removes exactly one path entry — then its blob, if that was
// the blob's last reference — in the order BFS-005 §3.3 fixes: unreferenced
// entries least-recently-HIT first; ties broken by largest blob first (one
// eviction buys the room 400 small ones would); remaining ties by lexicographic
// path so eviction is deterministic and reproducible in a test. A pinned blob is
// never a candidate, and neither is a blob feeding an in-flight write.
func (c *Cache) evictOneLocked() bool {
	candidates := make([]*cacheEntry, 0, len(c.entries))
	for _, e := range c.entries {
		b := c.blobs[e.Hash]
		if b == nil || b.Pins > 0 || c.inflight[e.Hash] {
			continue
		}
		candidates = append(candidates, e)
	}
	if len(candidates) == 0 {
		return false
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.LastHitMS != b.LastHitMS {
			return a.LastHitMS < b.LastHitMS
		}
		if a.Size != b.Size {
			return a.Size > b.Size
		}
		return a.Path < b.Path
	})
	c.dropPathLocked(candidates[0].Path)
	c.stats.EvictionsTotal++
	return true
}

// dropPathLocked removes one path entry, and its blob only when that was the
// blob's last path reference (which is where the content-addressing pays for
// itself) and no handle still pins it.
func (c *Cache) dropPathLocked(path string) {
	e := c.entries[path]
	if e == nil {
		return
	}
	delete(c.entries, path)
	if b := c.blobs[e.Hash]; b != nil {
		b.Refs--
		if b.Refs <= 0 && b.Pins == 0 {
			_ = os.Remove(c.blobPath(b.Hash))
			delete(c.blobs, b.Hash)
		}
	}
}

// Drop invalidates paths: the path index entry, and the blob only if that was
// its last reference. It returns how many path entries went. The caller (the
// invalidation channel, BFS-005 §4.2 step 1) is also responsible for dropping
// the parent directory's readdir snapshot, which lives in the node tree.
func (c *Cache) Drop(paths ...string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, p := range paths {
		if c.entries[p] != nil {
			c.dropPathLocked(p)
			n++
		}
	}
	if n > 0 {
		c.recountLocked()
		_ = c.flushLocked()
	}
	return n
}

// DropAll empties the cache. Used by a full resync (a sequence gap or an
// overflow event is a full resync, never a partial drop — BFS-005 §4.1).
func (c *Cache) DropAll() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.entries)
	for p := range c.entries {
		c.dropPathLocked(p)
	}
	c.gen++
	c.recountLocked()
	_ = c.flushLocked()
	return n
}

// Pin marks a blob as in use by a live handle. A pinned blob is never evicted:
// the handle holds its own reference, so a read in flight can never be reading
// reclaimed bytes (BFS-005 §3.3).
func (c *Cache) Pin(hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b := c.blobs[hash]; b != nil {
		b.Pins++
	}
}

// Unpin releases a pin. When a pinned blob's path entry was evicted while it
// was pinned, the unpin is where the blob finally goes.
func (c *Cache) Unpin(hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.blobs[hash]
	if b == nil {
		return
	}
	if b.Pins > 0 {
		b.Pins--
	}
	if b.Pins == 0 && b.Refs <= 0 {
		_ = os.Remove(c.blobPath(b.Hash))
		delete(c.blobs, b.Hash)
	}
	c.recountLocked()
	_ = c.flushLocked()
}

// MarkInFlight/MarkSettled bracket a write's blob so the eviction loop can
// never reclaim the bytes a write is about to publish (BFS-005 §3.3 rule 2).
func (c *Cache) MarkInFlight(hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight[hash] = true
}

// MarkSettled clears the in-flight marker.
func (c *Cache) MarkSettled(hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, hash)
}

// Generation returns the current invalidation generation.
func (c *Cache) Generation() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// Stats reports the figures of BFS-005 §3.2.
func (c *Cache) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recountLocked()
	return c.stats
}

// Dir returns the cache directory.
func (c *Cache) Dir() string { return c.cfg.Dir }

// MaxEntryBytes returns the per-entry cap.
func (c *Cache) MaxEntryBytes() int64 { return c.cfg.MaxEntryBytes }

// indices returns the entries sorted by path — the index must be byte-stable so
// a write is not the only reason its size changes.
func (c *Cache) indicesLocked() []cacheEntry {
	out := make([]cacheEntry, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// indexBytesLocked is the exact serialised size of the index document as it
// would be written now. Measured, never estimated: used_bytes must be a figure
// `du` can be compared against.
func (c *Cache) indexBytesLocked() int64 {
	raw, err := json.Marshal(indexDoc{Version: 1, Entries: c.indicesLocked()})
	if err != nil {
		return 0
	}
	return int64(len(raw))
}

// recountLocked refreshes the reported figures. A DISABLED cache reports zeros
// for everything including the index: the figures are the client's declared
// local footprint, and a disabled cache has none.
func (c *Cache) recountLocked() {
	if c.disabled() {
		c.stats = CacheStats{MaxBytes: 0}
		c.stats.BypassReasons = c.bypassCensusLocked()
		c.measureDirLocked()
		return
	}
	var blobsBytes int64
	pins := 0
	for _, b := range c.blobs {
		blobsBytes += b.Size
		if b.Pins > 0 {
			pins++
		}
	}
	c.stats.BlobsBytes = blobsBytes
	c.stats.IndexBytes = c.indexBytesLocked()
	c.stats.UsedBytes = blobsBytes + c.stats.IndexBytes
	c.stats.Entries = len(c.entries)
	c.stats.Blobs = len(c.blobs)
	c.stats.PinnedBlobs = pins
	c.stats.MaxBytes = c.cfg.MaxBytes
	// The second dimension of the bound, and the two accounts of the first: the
	// published figure (used_bytes) and the peak admission enforces
	// (reserved_bytes = published + in-flight + the index the in-flight
	// refreshes will publish). Both are reported, never merged (F-1 / O-3).
	c.stats.MaxEntries = c.cfg.MaxEntries
	c.stats.MaxEntryBytes = c.cfg.MaxEntryBytes
	c.stats.MaxAgeMS = c.cfg.MaxAge.Milliseconds()
	c.stats.InFlightBytes = c.inFlightBytesLocked()
	c.stats.ReservedBytes = c.reservedLocked()
	c.stats.StagedBlobs = len(c.staged)
	c.stats.MaxInFlight = c.cfg.MaxInFlight
	c.stats.BypassReasons = c.bypassCensusLocked()
	c.measureDirLocked()
}

// ---------------------------------------------------------------------------
// BFS-045: counting the refusals, and the independent measurement.
// ---------------------------------------------------------------------------

// CountBypass records one refusal to keep bytes in the cache, by reason. It is
// the seam the LIVE READ PATH calls at the point where IT decides not to store
// (the per-entry cap), which is the decision Cache.Insert's own oversize branch
// never sees: BFS-032 was exactly that gap, and a counter the live path cannot
// reach is a gap rather than a green check.
//
// An unknown reason is counted under `other`-style honesty: it is added to the
// map as given rather than silently dropped, because a new refusal site must be
// VISIBLE in the census (and BypassReasons() is the closed list an operator
// compares against).
func (c *Cache) CountBypass(reason string) {
	if reason == "" {
		reason = BypassReasonInsertOverEntryCap
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.countBypassLocked(reason)
}

// AdmitRead is the read path's ONE cache-admission decision: it stores the
// served bytes when they fit the per-entry cap and COUNTS the refusal (with its
// reason) when they do not. Every caller that holds the served hash and bytes
// goes through here, so no caller can pre-filter above the counter again.
//
// It returns whether the bytes are IN the cache afterwards (a store or a hit),
// which is what a caller needs before it pins: a bypass due to the byte bound
// leaves nothing to pin, and saying so is cheaper than making every caller ask
// again.
func (c *Cache) AdmitRead(path, hash string, data []byte) bool {
	if c.MaxEntryBytes() > 0 && int64(len(data)) > c.MaxEntryBytes() {
		// The aggregate figure BFS-005 §3.2 publishes is kept honest: it counts
		// single-entry-over-the-cap refusals, and THIS is the site that makes
		// them, so leaving it to Insert (which never sees this read) is exactly
		// how the figure stayed at 0 through a live over-cap read (BFS-032).
		c.CountBypass(BypassReasonOverEntryCap)
		c.mu.Lock()
		c.stats.OversizeBypasses++
		c.mu.Unlock()
		return false
	}
	out, _ := c.Insert(path, hash, data)
	return out.Cached()
}

func (c *Cache) countBypassLocked(reason string) {
	if c.bypassReasons == nil {
		c.bypassReasons = map[string]int64{}
	}
	c.bypassReasons[reason]++
	c.stats.BypassReasons = c.bypassCensusLocked()
}

// bypassCensusLocked returns a COPY of the census: the reported document must
// never alias live state a caller could mutate.
func (c *Cache) bypassCensusLocked() map[string]int64 {
	out := make(map[string]int64, len(c.bypassReasons)+len(BypassReasons()))
	for _, r := range BypassReasons() {
		out[r] = c.bypassReasons[r]
	}
	for r, n := range c.bypassReasons {
		if _, known := out[r]; !known {
			out[r] = n
		}
	}
	return out
}

// BypassCount reports one reason's count (0 when it has never fired).
func (c *Cache) BypassCount(reason string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bypassReasons[reason]
}

// measureDirLocked walks the cache directory and records what is really there.
//
// WHY THIS EXISTS (BFS-031): `used_bytes` counts published blobs + the
// serialised index and NOT status.json, conflicts.jsonl, staged blobs, a temp
// index, or an orphan blob. A bound reported one way and enforced another is the
// defect; the cure the owner asked for is that the reported figure agree with an
// independent measurement. So the measurement is taken from the FILESYSTEM, by
// class, beside the accounting — and the delta is reported as a named figure
// (dir_unaccounted_bytes) rather than left for someone to discover with `du`.
//
// COST, AND WHY THE MEASUREMENT IS SPLIT IN TWO. The blob directory is O(blobs),
// so it is walked at most once per DirMeasureTTL and the age of THAT half is
// reported (dir_measured_age_ms). The top level — index.json, status.json, the
// conflict log, their temps — is a handful of entries and is re-stat'ed on EVERY
// publish, because the mount rewrites the status document once a second: a
// figure that ignored the file being written right now would disagree with `du`
// at exactly the moment someone checks it. That is not hypothetical: on the live
// route, the first version of this measurement reported dir_bytes=0 while the
// directory held 4230 bytes (docs/evidence/BFS-045-live-mount.txt).
func (c *Cache) measureDirLocked() {
	now := c.cfg.Now()
	ttl := c.cfg.DirMeasureInterval
	if ttl == 0 {
		ttl = DirMeasureTTL
	}
	c.dirMeasuredAt = now
	classes := map[string]int64{}
	for _, k := range dirClasses() {
		classes[k] = 0
	}
	var total int64
	add := func(class string, n int64) {
		classes[class] += n
		total += n
	}
	// (a) The top level, LIVE: the volatile small files, and any foreign
	// subdirectory (summed one level deep so its bytes stay inside the total).
	entries, err := os.ReadDir(c.cfg.Dir)
	if err != nil {
		c.dirErr = fmt.Sprintf("the cache directory %s could not be read: %v", c.cfg.Dir, err)
		c.dirBytes, c.dirClasses = 0, classes
		c.publishDirLocked(now)
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if name != CacheBlobDir {
				if n, derr := dirBytesOf(filepath.Join(c.cfg.Dir, name)); derr == nil {
					add(DirClassOther, n)
				}
			}
			continue
		}
		switch {
		case name == CacheIndexFile, strings.HasPrefix(name, CacheIndexFile+"."):
			n, _ := fileSize(filepath.Join(c.cfg.Dir, name))
			add(DirClassIndex, n)
		case name == StatusFile, strings.HasPrefix(name, StatusFile+"."):
			n, _ := fileSize(filepath.Join(c.cfg.Dir, name))
			add(DirClassStatus, n)
		case name == ConflictsFile:
			n, _ := fileSize(filepath.Join(c.cfg.Dir, name))
			add(DirClassConflicts, n)
		default:
			n, _ := fileSize(filepath.Join(c.cfg.Dir, name))
			add(DirClassOther, n)
		}
	}
	// (b) The blob classes, reused for at most the TTL. A negative interval means
	// "walk every time": the test seam, and what an operator asks for when the
	// figure is being audited against `du`.
	reuse := ttl > 0 && !c.dirBlobAt.IsZero() && now.Sub(c.dirBlobAt) < ttl && c.cfg.DirMeasureInterval >= 0
	if reuse {
		for _, k := range []string{DirClassBlobs, DirClassStaged, DirClassOrphan} {
			add(k, c.dirBlobClasses[k])
		}
		c.dirErr = ""
		c.dirBytes, c.dirClasses = total, classes
		c.publishDirLocked(now)
		return
	}
	blobs, err := os.ReadDir(filepath.Join(c.cfg.Dir, CacheBlobDir))
	if err != nil {
		c.dirErr = fmt.Sprintf("the blob directory %s could not be read: %v", filepath.Join(c.cfg.Dir, CacheBlobDir), err)
		c.dirBlobAt = now
		c.dirBlobClasses = map[string]int64{}
		c.dirBytes, c.dirClasses = 0, classes
		c.publishDirLocked(now)
		return
	}
	blobClasses := map[string]int64{DirClassBlobs: 0, DirClassStaged: 0, DirClassOrphan: 0}
	for _, b := range blobs {
		if b.IsDir() {
			continue
		}
		n, serr := fileSize(filepath.Join(c.cfg.Dir, CacheBlobDir, b.Name()))
		if serr != nil {
			continue
		}
		switch {
		case strings.HasPrefix(b.Name(), CacheStagePrefix):
			blobClasses[DirClassStaged] += n
		case c.blobs[HashPrefix+b.Name()] != nil:
			// The blob FILE is named by the bare digest; the census is keyed by
			// the tagged one. Getting this wrong attributes every published blob
			// to the orphan class — measured while writing this arm, which is
			// why the class assertion exists.
			blobClasses[DirClassBlobs] += n
		default:
			// A blob file no index entry references: the residue of an
			// interrupted insert, really occupying the directory.
			blobClasses[DirClassOrphan] += n
		}
	}
	c.dirBlobAt, c.dirBlobClasses = now, blobClasses
	for k, n := range blobClasses {
		add(k, n)
	}
	c.dirErr = ""
	c.dirBytes, c.dirClasses = total, classes
	c.publishDirLocked(now)
}

// publishDirLocked copies the last measurement (or its absence, with a reason)
// into the reported figures.
func (c *Cache) publishDirLocked(now time.Time) {
	// The age reported is the age of the BLOB census — the half that is sampled.
	// The top-level entries are re-stat'ed on every publish, so reporting their
	// age would have to be 0 and would say nothing.
	if !c.dirBlobAt.IsZero() {
		age := now.Sub(c.dirBlobAt).Milliseconds()
		c.stats.DirMeasuredAgeMS = &age
	} else {
		c.stats.DirMeasuredAgeMS = nil
	}
	cp := make(map[string]int64, len(c.dirClasses))
	for k, v := range c.dirClasses {
		cp[k] = v
	}
	c.stats.DirBytesByClass = cp
	c.stats.DirBytes = c.dirBytes
	c.stats.DirBytesReason = ""
	if c.dirErr != "" {
		// The measurement could not be taken: absent WITH A REASON, never a zero
		// that reads as "the directory is empty".
		c.stats.DirBytes = 0
		c.stats.DirBytesReason = ReasonUnknown + ": " + c.dirErr
	}
	if c.disabled() {
		c.stats.DirBytesReason = ReasonDisabled + ": the cache is switched off (--cache-max-size 0), so no cache directory is accounted for"
	}
	// The named delta: what `used_bytes` does not count. Positive means the
	// directory holds more than the published figure (the BFS-031 shape);
	// negative can only mean the accounting is ahead of the disk (a blob
	// removed out of band), and both directions are worth seeing.
	if c.stats.DirBytesReason == "" {
		c.stats.DirUnaccountedBytes = c.stats.DirBytes - c.stats.UsedBytes
	} else {
		c.stats.DirUnaccountedBytes = 0
	}
}

// fileSize is one file's on-disk logical size.
func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// dirBytesOf sums the files under a directory, one level deep (used only for a
// foreign subdirectory, so it is bounded by what is actually there).
func dirBytesOf(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		n, serr := fileSize(filepath.Join(dir, e.Name()))
		if serr != nil {
			continue
		}
		total += n
	}
	return total, nil
}

// flushLocked persists the index atomically. A disabled cache writes nothing.
func (c *Cache) flushLocked() error {
	if c.disabled() {
		return nil
	}
	raw, err := json.Marshal(indexDoc{Version: 1, Entries: c.indicesLocked()})
	if err != nil {
		return fmt.Errorf("fsclient: encode cache index: %w", err)
	}
	tmp := filepath.Join(c.cfg.Dir, CacheIndexFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("fsclient: write cache index: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(c.cfg.Dir, CacheIndexFile)); err != nil {
		return fmt.Errorf("fsclient: publish cache index: %w", err)
	}
	return nil
}

// Close releases nothing but the process's interest in the cache; the on-disk
// state is already durable. It exists so a mount can flush the index on
// shutdown without reaching into unexported state.
func (c *Cache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flushLocked()
}
