package fsclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	// CacheIndexFile is the path index inside the cache directory.
	CacheIndexFile = "index.json"
	// CacheBlobDir holds the content-addressed whole-file blobs.
	CacheBlobDir = "blobs"
)

// CacheConfig configures a Cache. MaxBytes = 0 disables the cache entirely:
// every read goes to the server and every figure reports 0 (BFS-005 §3.1).
type CacheConfig struct {
	Dir           string
	MaxBytes      int64
	MaxEntryBytes int64
	MaxAge        time.Duration
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

// CacheStats is the reported figure set of BFS-005 §3.2. used_bytes is the
// figure AC-5 compares with `du`; max_bytes is what it must never exceed.
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
	// Hits/Misses are the read-path counters; not part of the §3.2 minimum but
	// the only way to show the cache actually served anything.
	Hits   int64 `json:"hits"`
	Misses int64 `json:"misses"`
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

// Cache is a bounded, content-addressed, whole-file cache with a path index.
//
// Consequences of the content-addressing (BFS-005 §3.2), all deliberate: two
// paths with identical content cost ONE blob; the read that fills the cache is
// the read that decides the conflict (its hash is the write's If-Match base);
// and whole-file blobs make `blobs_bytes` agree with `du` up to block rounding.
type Cache struct {
	cfg CacheConfig

	mu       sync.Mutex
	entries  map[string]*cacheEntry // path -> entry
	blobs    map[string]*blobRef    // hash -> blob
	inflight map[string]bool        // blobs feeding an in-flight write: never evicted
	gen      int64
	stats    CacheStats
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
		stats:    CacheStats{MaxBytes: cfg.MaxBytes},
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
		return OutcomeDisabled, nil
	}
	if int64(len(data)) > c.cfg.MaxEntryBytes {
		c.stats.OversizeBypasses++
		return OutcomeOversize, nil
	}
	if e := c.entries[path]; e != nil && e.Hash == hash && !c.expired(e) {
		e.LastHitMS = c.cfg.Now().UnixMilli()
		c.stats.Hits++
		return OutcomeHit, nil
	}

	// Project the post-insert size and evict until it fits. indexBytes is
	// measured from the serialised document, not estimated, so `used_bytes`
	// stays exact and comparable with `du`.
	blob := c.blobs[hash]
	newBlobBytes := int64(0)
	if blob == nil {
		newBlobBytes = int64(len(data))
	}
	if err := c.makeRoomLocked(newBlobBytes, c.indexGrowthLocked(path)); err != nil {
		c.stats.BypassEvents++
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

// makeRoomLocked evicts until newBlobBytes (+ pending index growth) fit under
// the cap. It returns an error when nothing can be freed — the caller then
// bypasses rather than growing or blocking.
func (c *Cache) makeRoomLocked(newBlobBytes, indexGrowth int64) error {
	for c.usedLocked()+newBlobBytes+indexGrowth > c.cfg.MaxBytes {
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

// usedLocked is blobs_bytes + index_bytes — the bytes actually on the client's
// disk (BFS-005 §3.1).
func (c *Cache) usedLocked() int64 {
	var blobs int64
	for _, b := range c.blobs {
		blobs += b.Size
	}
	return blobs + c.indexBytesLocked()
}

// recountLocked refreshes the reported figures. A DISABLED cache reports zeros
// for everything including the index: the figures are the client's declared
// local footprint, and a disabled cache has none.
func (c *Cache) recountLocked() {
	if c.disabled() {
		c.stats = CacheStats{MaxBytes: 0}
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
