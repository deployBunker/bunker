package fsclient

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-031: the bound must bound the DIRECTORY, and the reported figure must
// agree with an independent measurement — at a deliberately tiny bound and at
// the default one.
//
// The defect these arms are written against is measured, not supposed: at a
// 1 KiB bound the cache directory reached 30,689 B (29.97x the bound) while the
// client's own figures stayed "inside", because `used_bytes` counts published
// blobs plus the serialised index and NOT the index's temp copy, status.json or
// the refusal log (docs/evidence/BFS-012-smallbound-edge.txt; re-measured at
// 7.65x on the tree this row was written against, where the refusal-hold rule
// records one log line per refused path).
//
// The arms are deliberately written against the FILESYSTEM (an independent
// walk of the directory, `du`'s own subject) rather than against the cache's
// accounting, because BFS-031's shape is an accounting that agrees with itself.
// ---------------------------------------------------------------------------

// cacheDirWalk is the test's OWN measurement of a cache directory: every regular
// file under it, summed. It deliberately does not use the cache's walker.
func cacheDirWalk(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	if err := filepathWalk(dir, func(rel string, size int64) { total += size }); err != nil {
		t.Fatalf("independent walk of %s: %v", dir, err)
	}
	return total
}

// TestTheBoundBoundsTheCacheDirectoryAtATinyBound is the row's headline cell: at
// a 1 KiB bound, every byte the cache directory holds is inside the bound — and
// the temp copy of the index is reserved, so it is inside it at the moment the
// flush writes it too, not only at rest.
func TestTheBoundBoundsTheCacheDirectoryAtATinyBound(t *testing.T) {
	const bound = 1024
	dir := t.TempDir()
	c := newTestCacheWithBounds(t, CacheConfig{Dir: dir, MaxBytes: bound, MaxEntryBytes: bound, MaxAge: time.Hour, DirMeasureInterval: -1})

	if got := cacheDirWalk(t, dir); got > bound {
		t.Fatalf("an empty cache directory already holds %d B, over the %d B bound", got, bound)
	}
	// 64 B entries: small enough that several fit a 1 KiB bound, large enough that
	// the index record and the bound are both exercised.
	stored, refused := 0, 0
	for i := 0; i < 64; i++ {
		body := blobBytes(t, i, 64)
		out, err := c.Insert(pathFor(i), HashBytes(body), body)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		switch {
		case out.Cached():
			stored++
		case out == OutcomeBypass:
			refused++
		}
		// THE ROW'S CLAIM, measured on the filesystem after EVERY insert: the
		// directory — not the published figure — is inside the bound.
		if got := cacheDirWalk(t, dir); got > bound {
			t.Fatalf("after insert %d the cache DIRECTORY holds %d B, over the %d B bound (stored=%d refused=%d): the bound does not bound the directory",
				i, got, bound, stored, refused)
		}
		st := c.Stats()
		if st.DirBytes > bound {
			t.Fatalf("after insert %d the reported dir_bytes=%d exceeds the bound %d", i, st.DirBytes, bound)
		}
		if st.DirPeakBytes > bound {
			t.Fatalf("after insert %d the peak the bound is enforced against (%d) exceeds the bound %d", i, st.DirPeakBytes, bound)
		}
		if st.UsedBytes > bound {
			t.Fatalf("after insert %d used_bytes=%d exceeds the bound %d", i, st.UsedBytes, bound)
		}
	}
	if stored == 0 {
		t.Fatal("the 1 KiB bound stored nothing at all: the arm proves nothing about a bound that holds")
	}
	// Filling to the bound evicts rather than refusing while anything is
	// evictable, so the refusal arm needs a cache with nothing left to evict:
	// pin every survivor and try once more. The refusal must be REPORTED, not
	// silent (BFS-005 §3.4).
	for _, h := range c.entryHashes() {
		c.Pin(h)
	}
	pinned := blobBytes(t, 99, 128)
	out, err := c.Insert("refused.go", HashBytes(pinned), pinned)
	if err != nil {
		t.Fatalf("insert against a fully pinned cache: %v", err)
	}
	if out.Cached() {
		t.Fatalf("nothing was evictable and the insert still reported %v", out)
	}
	refused++
	st := c.Stats()
	if st.BypassReasons[BypassReasonNoRoom] == 0 {
		t.Fatalf("the refusal was not counted by reason: %v", st.BypassReasons)
	}
	// THE RESERVATION, stated sharply: the index's temp copy is what makes
	// `du ≤ max_bytes` true at the instant of a flush. A cache filled to the OLD
	// arithmetic (blobs + one index) would satisfy `used ≤ max` and fail this.
	if st.UsedBytes+st.IndexBytes > bound {
		t.Fatalf("the index's temp copy is not reserved: used_bytes=%d + index_bytes=%d > the %d B bound, so a flush would put the directory over it",
			st.UsedBytes, st.IndexBytes, bound)
	}
	// The reported figure and the independent walk agree, and the delta is the
	// part of the directory the published figure does not count.
	walked := cacheDirWalk(t, dir)
	if st.DirBytes != walked {
		t.Fatalf("reported dir_bytes=%d disagrees with an independent walk of the same directory (%d)", st.DirBytes, walked)
	}
	if st.DirUnaccountedBytes != st.DirBytes-st.UsedBytes {
		t.Fatalf("dir_unaccounted_bytes=%d, want dir_bytes−used_bytes=%d", st.DirUnaccountedBytes, st.DirBytes-st.UsedBytes)
	}
	t.Logf("1 KiB bound: stored=%d refused=%d dir_bytes=%d (walk=%d) used=%d index=%d peak=%d unaccounted=%d classes=%v",
		stored, refused, st.DirBytes, walked, st.UsedBytes, st.IndexBytes, st.DirPeakBytes, st.DirUnaccountedBytes, st.DirBytesByClass)
}

// TestTheBoundBoundsTheCacheDirectoryAtTheDefaultBound is the same cell at the
// real default, so the fix is not tuned to 1 KiB: the capacity cost of reserving
// the index's temp copy is stated as a number (it is one index copy, ~1.1% of
// the 256 MiB bound at the entry bound), and the directory stays inside the bound
// while the cache actually fills.
func TestTheBoundBoundsTheCacheDirectoryAtTheDefaultBound(t *testing.T) {
	const bound = DefaultCacheMaxBytes
	dir := t.TempDir()
	c := newTestCacheWithBounds(t, CacheConfig{Dir: dir, MaxBytes: bound, MaxEntryBytes: 1 << 16, MaxAge: time.Hour, DirMeasureInterval: -1})

	// 64 KiB entries: a few hundred MiB is too much disk for a unit arm, so the
	// arm fills 32 entries (2 MiB) and checks the INVARIANT at every step. The
	// capacity arithmetic that matters at the default is asserted, not inferred:
	// the reserved index copy is one index, and it is a fraction of a percent of
	// the bound.
	stored := 0
	for i := 0; i < 32; i++ {
		body := blobBytes(t, 1000+i, 64<<10)
		out, err := c.Insert(pathFor(i), HashBytes(body), body)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		if !out.Cached() {
			t.Fatalf("insert %d into a 256 MiB bound was %v: the default bound must hold", i, out)
		}
		stored++
		if got := cacheDirWalk(t, dir); got > bound {
			t.Fatalf("after insert %d the cache directory holds %d B, over the default bound %d", i, got, bound)
		}
	}
	st := c.Stats()
	if st.UsedBytes+st.IndexBytes > bound {
		t.Fatalf("used_bytes=%d + index_bytes=%d exceeds the default bound %d: the temp copy is not reserved", st.UsedBytes, st.IndexBytes, bound)
	}
	if st.EvictionsTotal != 0 {
		t.Fatalf("the default bound evicted %d entries while %d MiB went in: the reservation must cost a fraction of a percent, not force eviction",
			st.EvictionsTotal, (int64(stored) * (64 << 10) >> 20))
	}
	walked := cacheDirWalk(t, dir)
	if st.DirBytes != walked {
		t.Fatalf("reported dir_bytes=%d disagrees with an independent walk (%d) at the default bound", st.DirBytes, walked)
	}
	if got := st.BlobsBytes; got != int64(stored)*(64<<10) {
		t.Fatalf("blobs_bytes=%d, want %d: the cache must actually hold the bytes", got, int64(stored)*(64<<10))
	}
	costPercent := 100 * float64(st.IndexBytes) / float64(bound)
	t.Logf("default bound: stored=%d (%d MiB) dir_bytes=%d (walk=%d) used=%d index=%d peak=%d evictions=%d reserved-index cost=%.4f%% of the bound",
		stored, int64(stored)*(64<<10)>>20, st.DirBytes, walked, st.UsedBytes, st.IndexBytes, st.DirPeakBytes, st.EvictionsTotal, costPercent)
	if costPercent > 2 {
		t.Fatalf("the peak reservation costs %.2f%% of the default bound in this arm; the documented cost is one index copy", costPercent)
	}
}

// TestAForeignFileInTheCacheDirectoryConsumesTheBound is the include-not-omit
// half of the row: a byte in the directory that this cache did not write is
// still a byte the directory holds, so it consumes the bound rather than being
// invisible beside it.
func TestAForeignFileInTheCacheDirectoryConsumesTheBound(t *testing.T) {
	const bound = 1024
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, CacheBlobDir), 0o700); err != nil {
		t.Fatalf("blobs dir: %v", err)
	}
	// 600 B of somebody else's file, sitting in the directory the bound names.
	foreign := make([]byte, 600)
	if err := os.WriteFile(filepath.Join(dir, "not-ours.bin"), foreign, 0o600); err != nil {
		t.Fatalf("foreign file: %v", err)
	}
	c := newTestCacheWithBounds(t, CacheConfig{Dir: dir, MaxBytes: bound, MaxEntryBytes: bound, MaxAge: time.Hour, DirMeasureInterval: -1})

	body := blobBytes(t, 7, 512)
	out, err := c.Insert("foreign-arm.go", HashBytes(body), body)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if out.Cached() {
		st := c.Stats()
		t.Fatalf("a 512 B blob was stored beside 600 B of foreign bytes under a %d B bound (peak=%d, used=%d): the foreign bytes did not consume the bound",
			bound, st.DirPeakBytes, st.UsedBytes)
	}
	st := c.Stats()
	if st.BypassReasons[BypassReasonNoRoom] == 0 {
		t.Fatalf("the refusal was not counted: %v", st.BypassReasons)
	}
	// And the foreign bytes are NAMED in the report rather than merely missing.
	cs := c.Stats()
	if cs.DirBytesByClass[DirClassOther] < 600 {
		t.Fatalf("the foreign file is not attributed: other=%d, the file is 600 B", cs.DirBytesByClass[DirClassOther])
	}
	if cs.DirBytes < 600 {
		t.Fatalf("the reported dir_bytes (%d) is smaller than the foreign file (600 B) on disk", cs.DirBytes)
	}
	// dir_unaccounted_bytes is dir_bytes − used_bytes, and used_bytes counts the
	// in-memory index that is NOT on disk yet when no insert has been published:
	// the delta can therefore be the foreign file MINUS that index. The class
	// figure above is the one that names the foreign bytes; this is the
	// arithmetic identity, asserted rather than described.
	if cs.DirUnaccountedBytes != cs.DirBytes-cs.UsedBytes {
		t.Fatalf("dir_unaccounted_bytes=%d, want dir_bytes−used_bytes=%d", cs.DirUnaccountedBytes, cs.DirBytes-cs.UsedBytes)
	}
	if got := cacheDirWalk(t, dir); got > bound {
		t.Fatalf("the directory holds %d B, over the %d B bound", got, bound)
	}
	t.Logf("foreign arm: dir_bytes=%d (walk=%d) used=%d peak=%d unaccounted=%d classes=%v",
		cs.DirBytes, cacheDirWalk(t, dir), cs.UsedBytes, cs.DirPeakBytes, cs.DirUnaccountedBytes, cs.DirBytesByClass)
}

// TestAPreBFS031CacheIsMigratedIntoTheCacheDirectory proves the layout change is
// a migration rather than a discard: a cache written by an earlier build (index
// + blobs directly in the mount directory) is moved into <mount dir>/cache, its
// entries are still readable afterwards, and the mount's own state files are
// neither moved nor rewritten.
func TestAPreBFS031CacheIsMigratedIntoTheCacheDirectory(t *testing.T) {
	mount := t.TempDir()
	// The pre-BFS-031 layout: a cache directly in the mount directory, beside the
	// mount's state.
	legacy, err := OpenCache(CacheConfig{Dir: mount, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 16, MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("legacy cache: %v", err)
	}
	body := []byte("a file the pre-BFS-031 cache holds")
	if _, err := legacy.Insert("kept.txt", HashBytes(body), body); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("legacy close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mount, CacheIndexFile+".tmp"), []byte("stale"), 0o600); err != nil {
		t.Fatalf("stale temp: %v", err)
	}
	st := Status{Mount: "migration", Endpoint: "http://127.0.0.1:1/dav"}
	if err := WriteStatus(mount, st); err != nil {
		t.Fatalf("write status: %v", err)
	}

	if err := AppendConflict(mount, Conflict{Path: "kept.txt", Code: "hash_mismatch"}); err != nil {
		t.Fatalf("append conflict: %v", err)
	}
	stateBefore := map[string][]byte{}
	for _, name := range []string{StatusFile, ConflictsFile} {
		raw, err := os.ReadFile(filepath.Join(mount, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		stateBefore[name] = raw
	}

	mig, err := MigrateMountLayout(mount)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !mig.Moved || mig.Files == 0 {
		t.Fatalf("migration reported %+v: a legacy cache was present", mig)
	}
	if mig.Bytes <= 0 {
		t.Fatalf("migration moved %d bytes: the bytes are the owner's cache and must be accounted", mig.Bytes)
	}
	if mig.TempsRemoved != 1 {
		t.Fatalf("stale index temps removed = %d, want 1", mig.TempsRemoved)
	}
	for _, name := range []string{CacheIndexFile, CacheBlobDir} {
		if _, err := os.Stat(filepath.Join(mount, name)); !os.IsNotExist(err) {
			t.Fatalf("%s is still in the mount directory after the migration", name)
		}
	}
	if _, err := os.Stat(filepath.Join(mount, CacheIndexFile+".tmp")); !os.IsNotExist(err) {
		t.Fatal("the stale index temp survived the migration; it would sit in the directory the bound measures")
	}
	// The state files are UNTOUCHED: byte-identical, same path.
	for name, want := range stateBefore {
		raw, err := os.ReadFile(filepath.Join(mount, name))
		if err != nil {
			t.Fatalf("state file %s after migration: %v", name, err)
		}
		if string(raw) != string(want) {
			t.Fatalf("the migration rewrote %s: the observability surface must not be touched by a layout move", name)
		}
	}
	// The migrated cache is the SAME cache: its entry is still readable.
	moved, err := OpenCache(CacheConfig{Dir: MountCacheDir(mount), MaxBytes: 1 << 20, MaxEntryBytes: 1 << 16, MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("open migrated cache: %v", err)
	}
	got, ok := moved.Get("kept.txt", HashBytes(body))
	if !ok || string(got) != string(body) {
		t.Fatalf("the migrated cache lost its entry: ok=%v got=%q", ok, got)
	}
	if moved.Dir() != MountCacheDir(mount) {
		t.Fatalf("the migrated cache opened in %s, want %s", moved.Dir(), MountCacheDir(mount))
	}
	// Idempotent: a second migration finds nothing to do (a remount must not
	// re-migrate, and must not report a move that did not happen).
	again, err := MigrateMountLayout(mount)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if again.Moved {
		t.Fatalf("the second migration reported a move: %+v", again)
	}
	t.Logf("migration: files=%d bytes=%d temps=%d; state files untouched; the entry survived", mig.Files, mig.Bytes, mig.TempsRemoved)
}

// blobBytes builds a distinct, reproducible payload of n bytes for entry i (the
// content must differ per entry or content-addressing makes the arm vacuous).
func blobBytes(t *testing.T, i, n int) []byte {
	t.Helper()
	if n < 4 {
		t.Fatalf("blobBytes needs at least 4 bytes, got %d", n)
	}
	data := make([]byte, n)
	for j := range data {
		data[j] = byte('a' + (i*31+j*7)%26)
	}
	// The first bytes carry the index itself: a formula whose period divides 26
	// would make entries i and i+26 the SAME content, and content-addressing then
	// reports a hit — measured while writing this arm, where 32 entries collapsed
	// to 26 distinct blobs.
	data[0], data[1], data[2], data[3] = byte(i), byte(i>>8), byte('a'+i%26), byte('A'+i%26)
	return data
}

// pathFor names entry i. Paths are distinct and their length is representative
// (~10 B), because the index record's size is part of what the bound pays for.
func pathFor(i int) string {
	return "d/" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + ".go"
}
