package fsclient

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestCache(t *testing.T, max, maxEntry int64) *Cache {
	t.Helper()
	c, err := OpenCache(CacheConfig{Dir: t.TempDir(), MaxBytes: max, MaxEntryBytes: maxEntry, MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	return c
}

// blob builds a distinct, reproducible payload of n bytes. The content must
// DIFFER per call: two identical payloads share one blob by construction (that
// is what content-addressing means), which would make an eviction test vacuous.
var blobSeq int

func blob(t *testing.T, n int) (string, []byte) {
	t.Helper()
	blobSeq++
	data := make([]byte, n)
	for i := range data {
		data[i] = byte('a' + (blobSeq*7+i)%26)
	}
	data[0] = byte('a' + n%26)
	return HashBytes(data), data
}

// TestCacheBoundIsHardAndBypassIsReported proves the bound is enforced at
// insert, and that a full cache BYPASSES rather than growing, failing or
// blocking (BFS-005 §3.1/§3.4 — accepted criterion 3).
func TestCacheBoundIsHardAndBypassIsReported(t *testing.T) {
	const max = 4096
	c := newTestCache(t, max, max)

	// Fill with entries that cannot all fit; every insert must leave used_bytes
	// at or below the cap.
	for i := 0; i < 40; i++ {
		h, data := blob(t, 512)
		if _, err := c.Insert(fmt.Sprintf("p/%d", i), h, data); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		st := c.Stats()
		if st.UsedBytes > st.MaxBytes {
			t.Fatalf("after insert %d: used_bytes=%d exceeds max_bytes=%d", i, st.UsedBytes, st.MaxBytes)
		}
	}
	st := c.Stats()
	if st.EvictionsTotal == 0 {
		t.Fatalf("expected evictions while filling a %d byte cache; got 0", max)
	}
	if st.Entries == 0 {
		t.Fatalf("cache ended empty; the bound must evict, not refuse everything")
	}

	// Everything pinned and nothing left to evict => bypass, and the read path
	// still gets its bytes.
	c2 := newTestCache(t, 700, 700)
	h1, d1 := blob(t, 512)
	if o, err := c2.Insert("a", h1, d1); err != nil || o == OutcomeBypass {
		t.Fatalf("first insert: outcome=%v err=%v", o, err)
	}
	c2.Pin(h1)
	h2, d2 := blob(t, 512)
	o, err := c2.Insert("b", h2, d2)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if o != OutcomeBypass {
		t.Fatalf("expected bypass with the only blob pinned; got %v", o)
	}
	if st := c2.Stats(); st.BypassEvents == 0 {
		t.Fatalf("bypass must be REPORTED (bypass_events), not silently accepted")
	}
	if st := c2.Stats(); st.UsedBytes > st.MaxBytes {
		t.Fatalf("bypass grew the cache beyond its bound: %+v", st)
	}
}

// TestCacheOversizeEntryIsNeverCached covers --cache-max-entry-bytes.
func TestCacheOversizeEntryIsNeverCached(t *testing.T) {
	c := newTestCache(t, 1<<20, 1024)
	h, data := blob(t, 4096)
	o, err := c.Insert("big", h, data)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if o != OutcomeOversize {
		t.Fatalf("expected oversize_bypass, got %v", o)
	}
	if st := c.Stats(); st.OversizeBypasses != 1 || st.UsedBytes != st.IndexBytes {
		t.Fatalf("oversize entry was cached: %+v", st)
	}
}

// TestCacheEvictionOrderIsLruByHitThenLargestThenPath pins the order BFS-005
// §3.3 fixes, because a nondeterministic eviction order is untestable and an
// untestable bound is an unproven one. Each arm is sized so that EXACTLY ONE
// eviction is required, which is what makes the assertion meaningful.
func TestCacheEvictionOrderIsLruByHitThenLargestThenPath(t *testing.T) {
	frozen := time.Unix(1000, 0)
	newFrozen := func(max int64) *Cache {
		t.Helper()
		c, err := OpenCache(CacheConfig{
			Dir: t.TempDir(), MaxBytes: max, MaxEntryBytes: max,
			Now: func() time.Time { return frozen },
		})
		if err != nil {
			t.Fatalf("OpenCache: %v", err)
		}
		return c
	}
	insert := func(t *testing.T, c *Cache, path string, size int) string {
		t.Helper()
		h, data := blob(t, size)
		if _, err := c.Insert(path, h, data); err != nil {
			t.Fatalf("insert %s: %v", path, err)
		}
		return h
	}

	t.Run("least-recently-HIT first", func(t *testing.T) {
		c := newFrozen(4096)
		insert(t, c, "old", 1300)
		insert(t, c, "mid", 1300)
		insert(t, c, "new", 1300)
		// Touch "old" at a later instant: its LRU key is now the newest.
		frozen = frozen.Add(time.Second)
		if _, ok := c.Get("old", mustHash(t, "old", c)); !ok {
			t.Fatalf("expected a hit for old")
		}
		insert(t, c, "fresh", 64)
		if _, _, ok := c.Lookup("mid"); ok {
			t.Fatalf("mid has the oldest HIT and must be evicted first")
		}
		if _, _, ok := c.Lookup("old"); !ok {
			t.Fatalf("old was recently HIT and must survive (the LRU key is the last hit, not the insert)")
		}
	})

	t.Run("largest blob first on a hit-tie", func(t *testing.T) {
		c := newFrozen(3300)
		insert(t, c, "small-a", 512)
		insert(t, c, "large", 2048)
		insert(t, c, "small-b", 512)
		insert(t, c, "fresh", 64)
		if _, _, ok := c.Lookup("large"); ok {
			t.Fatalf("on a hit-tie the LARGEST blob goes first (one eviction buys the room 400 small ones would)")
		}
		if _, _, ok := c.Lookup("small-a"); !ok {
			t.Fatalf("small-a should have survived")
		}
	})

	t.Run("lexicographic path order on a size-tie", func(t *testing.T) {
		c := newFrozen(3300)
		insert(t, c, "b/one", 1024)
		insert(t, c, "a/one", 1024)
		insert(t, c, "c/one", 1024)
		insert(t, c, "fresh", 64)
		if _, _, ok := c.Lookup("a/one"); ok {
			t.Fatalf("on a size-tie eviction must be lexicographically deterministic and reproducible")
		}
		if _, _, ok := c.Lookup("c/one"); !ok {
			t.Fatalf("c/one should have survived the one required eviction")
		}
	})
}

// mustHash returns the hash currently recorded for a path.
func mustHash(t *testing.T, path string, c *Cache) string {
	t.Helper()
	h, _, ok := c.Lookup(path)
	if !ok {
		t.Fatalf("no entry for %s", path)
	}
	return h
}

// TestCachePinBlocksEvictionAndUnpinReclaims proves a live handle's blob is
// never evicted, that eviction takes the unpinned neighbour instead, and that a
// blob pinned past its own path entry is reclaimed at the unpin.
func TestCachePinBlocksEvictionAndUnpinReclaims(t *testing.T) {
	c := newTestCache(t, 2600, 2600)
	h1, d1 := blob(t, 1024)
	if _, err := c.Insert("pinned", h1, d1); err != nil {
		t.Fatal(err)
	}
	c.Pin(h1)
	h2, d2 := blob(t, 1024)
	if o, err := c.Insert("other", h2, d2); err != nil || !o.Cached() {
		t.Fatalf("inserting an unpinned neighbour must fit: outcome=%v err=%v", o, err)
	}
	// The next insert forces eviction, and the victim must be "other" — never a
	// blob a live handle is serving from.
	h3, d3 := blob(t, 1024)
	if o, err := c.Insert("third", h3, d3); err != nil {
		t.Fatalf("insert third: %v", err)
	} else if o != OutcomeStored {
		t.Fatalf("expected the third entry to be stored after evicting the unpinned one; got %v", o)
	}
	if _, _, ok := c.Lookup("other"); ok {
		t.Fatalf("the unpinned entry should have been evicted first")
	}
	if data, ok := c.Get("pinned", h1); !ok || len(data) != 1024 {
		t.Fatalf("the pinned entry was evicted while its handle was live")
	}
	// A pinned blob outlives its own path entry: drop the entry, then unpin.
	if n := c.Drop("pinned"); n != 1 {
		t.Fatalf("Drop reported %d entries", n)
	}
	if st := c.Stats(); st.Blobs != 2 || st.PinnedBlobs != 1 {
		t.Fatalf("a pinned blob must survive its path entry being dropped: %+v", st)
	}
	c.Unpin(h1)
	st := c.Stats()
	if st.PinnedBlobs != 0 {
		t.Fatalf("unpin did not clear the pin: %+v", st)
	}
	if st.Blobs != 1 {
		t.Fatalf("the un-referenced, un-pinned blob should be gone; blobs=%d", st.Blobs)
	}
}

// TestCacheContentAddressingSharesBlobs proves two paths with identical content
// cost one blob — the mechanism that keeps a redundant tree from doubling the
// local copy.
func TestCacheContentAddressingSharesBlobs(t *testing.T) {
	c := newTestCache(t, 1<<20, 1<<20)
	h, data := blob(t, 512)
	for _, p := range []string{"a/x", "b/x", "c/x"} {
		if _, err := c.Insert(p, h, data); err != nil {
			t.Fatal(err)
		}
	}
	st := c.Stats()
	if st.Blobs != 1 || st.Entries != 3 {
		t.Fatalf("expected 3 path entries over 1 blob, got entries=%d blobs=%d", st.Entries, st.Blobs)
	}
	if st.BlobsBytes != 512 {
		t.Fatalf("blob bytes should count the content once: got %d", st.BlobsBytes)
	}
	// Dropping one path must not remove the shared blob.
	c.Drop("a/x")
	if st := c.Stats(); st.Blobs != 1 {
		t.Fatalf("shared blob was dropped while two paths still referenced it: %+v", st)
	}
	c.Drop("b/x", "c/x")
	if st := c.Stats(); st.Blobs != 0 || st.BlobsBytes != 0 {
		t.Fatalf("blob survived its last reference: %+v", st)
	}
}

// TestCacheUsedBytesMatchesDisk proves the reported figure is the figure on
// disk (within block rounding), which is what makes it comparable with `du`.
func TestCacheUsedBytesMatchesDisk(t *testing.T) {
	c := newTestCache(t, 1<<20, 1<<20)
	if err := os.MkdirAll(filepath.Join(c.Dir(), CacheBlobDir), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		h, data := blob(t, 1024)
		if _, err := c.Insert(fmt.Sprintf("f%d", i), h, data); err != nil {
			t.Fatal(err)
		}
	}
	var onDisk int64
	err := filepath.WalkDir(c.Dir(), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		onDisk += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walk cache dir: %v", err)
	}
	st := c.Stats()
	if onDisk != st.UsedBytes {
		t.Fatalf("reported used_bytes=%d but the cache dir holds %d bytes", st.UsedBytes, onDisk)
	}
}

// TestCacheDisabledReportsZero covers --cache-max-size 0.
func TestCacheDisabledReportsZero(t *testing.T) {
	c := newTestCache(t, 0, 0)
	h, data := blob(t, 128)
	o, err := c.Insert("a", h, data)
	if err != nil {
		t.Fatal(err)
	}
	if o != OutcomeDisabled {
		t.Fatalf("expected the disabled outcome, got %v", o)
	}
	if _, ok := c.Get("a", h); ok {
		t.Fatalf("a disabled cache must never serve from disk")
	}
	if st := c.stats; st.MaxBytes != 0 || st.UsedBytes != 0 || st.IndexBytes != 0 || st.Entries != 0 {
		t.Fatalf("disabled cache must report zeros: %+v", st)
	}
}

// TestCacheRefusesMismatchedContentHash proves the cache cannot be made to lie
// about its own content address: an insert whose bytes do not hash to the key is
// refused, because that hash is the currency of the write path.
func TestCacheRefusesMismatchedContentHash(t *testing.T) {
	c := newTestCache(t, 1<<20, 1<<20)
	h, _ := blob(t, 64)
	o, err := c.Insert("a", h, []byte("different bytes"))
	if err == nil {
		t.Fatalf("expected a refusal for mismatched content")
	}
	if o != OutcomeBypass {
		t.Fatalf("expected bypass, got %v", o)
	}
	if _, _, ok := c.Lookup("a"); ok {
		t.Fatalf("nothing may be stored under a hash that does not describe the bytes")
	}
}

// TestCacheGenerationBumpsOnDropAll proves a full resync is a generation change,
// so entries stored before it are distinguishable from entries after it.
func TestCacheGenerationBumpsOnDropAll(t *testing.T) {
	c := newTestCache(t, 1<<20, 1<<20)
	h, data := blob(t, 64)
	if _, err := c.Insert("a", h, data); err != nil {
		t.Fatal(err)
	}
	before := c.Generation()
	if n := c.DropAll(); n != 1 {
		t.Fatalf("DropAll should report one dropped entry, got %d", n)
	}
	if c.Generation() <= before {
		t.Fatalf("DropAll must bump the generation (%d -> %d)", before, c.Generation())
	}
	if st := c.Stats(); st.Entries != 0 || st.UsedBytes != st.IndexBytes {
		t.Fatalf("DropAll left bytes behind: %+v", st)
	}
}

// TestCacheSurvivesReopen proves the index is durable and that a blob missing
// from disk does not survive as a phantom entry.
func TestCacheSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenCache(CacheConfig{Dir: dir, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	h, data := blob(t, 300)
	if _, err := c.Insert("keep", h, data); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := OpenCache(CacheConfig{Dir: dir, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := c2.Get("keep", h); !ok || !bytes.Equal(got, data) {
		t.Fatalf("reopened cache did not serve the entry it had stored (stats=%+v)", c2.Stats())
	}
	// Remove the blob and reopen: the entry must not survive as a phantom.
	if err := os.Remove(filepath.Join(dir, CacheBlobDir, strings.TrimPrefix(h, HashPrefix))); err != nil {
		t.Fatal(err)
	}
	c3, err := OpenCache(CacheConfig{Dir: dir, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c3.Lookup("keep"); ok {
		t.Fatalf("an entry whose blob is gone must be dropped on load, not served")
	}
	if st := c3.Stats(); st.BlobsBytes != 0 {
		t.Fatalf("phantom bytes counted: %+v", st)
	}
}

// TestCacheAllPinnedBypassesWithoutLeaking drives the case the bound exists for and
// a serial reader never reaches: the cache is at its cap and EVERY entry is pinned,
// so the eviction loop has no candidate at all. The client must BYPASS — serve the
// read, cache nothing, grow nothing — and it must do so REPEATEDLY without leaking
// anything into the cache directory. The single-pin arm of
// TestCacheBoundIsHardAndBypassIsReported proves one bypass; this proves the state
// is stable (a leak here is "local storage grows with the tree", the owner's one
// hard constraint) and that it ends the moment a pin is released.
func TestCacheAllPinnedBypassesWithoutLeaking(t *testing.T) {
	const max = 4096
	c := newTestCache(t, max, max)

	pinned := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		h, data := blob(t, 1000)
		if _, err := c.Insert(fmt.Sprintf("pin/%d", i), h, data); err != nil {
			t.Fatalf("seed insert %d: %v", i, err)
		}
		c.Pin(h)
		pinned = append(pinned, h)
	}
	seed := c.Stats()
	if seed.PinnedBlobs != 3 || seed.Entries != 3 {
		t.Fatalf("fixture: expected 3 pinned entries, got %+v", seed)
	}

	// The cache is now full of bytes it may not reclaim. Every further insert must
	// BYPASS: no error, no growth, no entry.
	bypassed := 0
	for i := 0; i < 20; i++ {
		h, data := blob(t, 1000)
		o, err := c.Insert(fmt.Sprintf("new/%d", i), h, data)
		if err != nil {
			t.Fatalf("insert %d: a full cache of unpinned-but-unevictable data must BYPASS, not fail: %v", i, err)
		}
		if o == OutcomeBypass {
			bypassed++
		}
		if st := c.Stats(); st.UsedBytes > st.MaxBytes {
			t.Fatalf("bypass %d grew the cache past its bound: %+v", i, st)
		}
	}
	if bypassed != 20 {
		t.Fatalf("expected all 20 inserts to bypass, got %d", bypassed)
	}
	st := c.Stats()
	if st.BypassEvents != 20 {
		t.Fatalf("every bypass must be REPORTED: bypass_events=%d, want 20", st.BypassEvents)
	}
	if st.Entries != 3 || st.Blobs != 3 {
		t.Fatalf("a bypassed insert stored something: %+v", st)
	}
	if st.EvictionsTotal != 0 {
		t.Fatalf("nothing was evictable, so nothing may be counted as evicted: %+v", st)
	}
	if st.UsedBytes != seed.UsedBytes {
		t.Fatalf("20 bypasses changed the footprint: %d -> %d", seed.UsedBytes, st.UsedBytes)
	}

	// Nothing leaked into the cache directory: it holds the index and exactly the
	// three pinned blobs, and no temp file from a half-finished insert.
	allowed := map[string]bool{CacheIndexFile: true}
	for _, h := range pinned {
		allowed[filepath.Join(CacheBlobDir, strings.TrimPrefix(h, HashPrefix))] = true
	}
	if err := filepath.WalkDir(c.Dir(), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(c.Dir(), p)
		if rerr != nil {
			return rerr
		}
		if !allowed[rel] {
			t.Errorf("unaccounted file in the cache directory after 20 bypasses: %s", rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The pinned bytes are still readable, so a bypass never cost a live handle.
	for i, h := range pinned {
		if _, ok := c.Get(fmt.Sprintf("pin/%d", i), h); !ok {
			t.Fatalf("pinned entry %d became unreadable while its handle was live", i)
		}
	}

	// Releasing ONE pin makes an eviction possible again: the difference between
	// "full" and "nothing evictable" is exactly the pin.
	c.Unpin(pinned[0])
	h, data := blob(t, 1000)
	o, err := c.Insert("after/unpin", h, data)
	if err != nil {
		t.Fatalf("insert after unpin: %v", err)
	}
	if o != OutcomeStored {
		t.Fatalf("after a pin is released the insert must be stored, not bypassed: outcome=%v", o)
	}
	if st := c.Stats(); st.EvictionsTotal != 1 || st.UsedBytes > st.MaxBytes {
		t.Fatalf("expected exactly one eviction and a bounded cache: %+v", st)
	}
}
