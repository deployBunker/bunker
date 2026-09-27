package fsclient

// BFS-038 — a refreshed file must never be visible half-written.
//
// The mechanism under test is a REPRESENTATION, not a lock: the refresh writes a
// NEW immutable blob and publishes it with ONE pointer swap into the path index.
// These cells are written around the four claims the row's acceptance criteria
// name, each with the control that proves it can fail:
//
//	1. no interleaving exposes a partial file        -> TestAtomicRefreshHalfFileCell
//	                                                    (with the mutate-in-place
//	                                                    controls that make it fail)
//	2. an abandoned refresh never swaps              -> TestAbandonedRefresh...
//	3. eviction respects in-flight readers           -> TestEvictionRespects...
//	4. eviction and admission account separately and
//	   BOTH are reported                             -> TestTwoAccountsAreReported...
//	                                                    TestInFlightBytesDecideAdmission
//	   plus the entry-count bound the byte bound needs -> TestEntryBoundIsEnforced...

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newStagedCache opens a cache with the two bounds this row adds, stated
// explicitly so a cell never depends on a default it did not name.
func newStagedCache(t *testing.T, maxBytes, maxEntry int64, maxEntries, maxInFlight int) *Cache {
	t.Helper()
	c, err := OpenCache(CacheConfig{
		Dir:           t.TempDir(),
		MaxBytes:      maxBytes,
		MaxEntryBytes: maxEntry,
		MaxEntries:    maxEntries,
		MaxInFlight:   maxInFlight,
		MaxAge:        time.Hour,
	})
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	return c
}

// stagePayload builds a distinct, reproducible payload. Distinct because two
// identical payloads share one blob by content addressing, which would make
// every "the content changed" arm vacuous.
func stagePayload(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + int(seed)*7)
	}
	return b
}

// dirBytes is `du` of the cache directory: every file, counted. This is the
// figure a bound must bound (BFS-031), as opposed to the figure the client
// chooses to report.
func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return total
}

// classify names what a reader actually received.
func classify(got, oldData, newData []byte) string {
	switch {
	case bytes.Equal(got, oldData):
		return "old"
	case bytes.Equal(got, newData):
		return "new"
	default:
		return fmt.Sprintf("partial(%d bytes)", len(got))
	}
}

// observeOnce is ONE read of path through the cache's PUBLIC read path: look the
// path up, then read what the index names. This is the same two calls the mount's
// read handle makes (fs_linux.go load()).
func observeOnce(c *Cache, path string, oldData, newData []byte) string {
	hash, _, ok := c.Lookup(path)
	if !ok {
		return "absent"
	}
	data, ok := c.Get(path, hash)
	if !ok {
		return "absent"
	}
	return classify(data, oldData, newData)
}

// observeCounted is the four-way verdict the free-running measurement counts:
// the two COMPLETE contents, a MISS, or a PARTIAL file. The miss is its own
// bucket on purpose — a reader whose lookup raced a pointer swap reads nothing
// from the cache and falls through to the server, which is correct and is the
// read path's existing behaviour; counting it as a partial file would be a false
// accusation, and the measurement's own diagnostic run (see the evidence file)
// showed the entire count living in that bucket.
func observeCounted(c *Cache, path string, oldData, newData []byte) string {
	hash, _, ok := c.Lookup(path)
	if !ok {
		return "miss"
	}
	data, ok := c.Get(path, hash)
	if !ok {
		return "miss"
	}
	switch {
	case bytes.Equal(data, oldData):
		return "old"
	case bytes.Equal(data, newData):
		return "new"
	default:
		return "partial"
	}
}

// ── the half-file cell, and the two controls that keep it honest ─────────────

// refreshImpl is a way of putting NEW bytes into the cache for a path. halfWay
// is called once, after the implementation has put its first chunk of the new
// content in place and BEFORE it publishes; publish happens when refresh
// returns. That single declared checkpoint is what makes the cell a controlled
// schedule instead of a race.
type refreshImpl struct {
	name string
	want string // the verdict a COMPLETE representation must produce at halfWay
	// violation reports whether this arm is expected to expose a half file.
	// A control declares true: it exists to prove the cell can see the bug.
	violation bool
	refresh   func(c *Cache, path string, data []byte, halfWay func()) error
}

const halfFileViolationWanted = "old"

// halfFileViolation is the SHARED judgement of the cell, used unchanged by the
// shipped arm and by both controls: a refresh is correct for a concurrent reader
// iff the reader sees one of the two COMPLETE contents at the moment the refresh
// is half done. Anything else is the bug this row exists to prevent.
func halfFileViolation(observed string) bool {
	return observed != "old" && observed != "new"
}

// stagedRefresh is the SHIPPED shape: a new immutable blob, one pointer swap.
func stagedRefresh(c *Cache, path string, data []byte, halfWay func()) error {
	s, err := c.Stage(path, HashBytes(data), int64(len(data)))
	if err != nil {
		return err
	}
	defer func() { _ = s.Abort() }() // a no-op after a successful Commit
	half := len(data) / 2
	if _, err := s.Write(data[:half]); err != nil {
		return err
	}
	halfWay()
	if _, err := s.Write(data[half:]); err != nil {
		return err
	}
	if _, err := s.Commit(); err != nil {
		return err
	}
	return nil
}

// mutateOldBlobRefresh is NEGATIVE CONTROL 1a: the shape you get when the
// cache's unit of storage is the PATH rather than the content — "refresh the
// cached copy of P" implemented as "rewrite the file P's entry names". The
// pointer still names the old content address while the bytes under it are being
// replaced, so a reader that looked the path up before the refresh reads a file
// mid-rewrite. It also destroys the old blob, so the content address becomes a
// lie. It is a CONTROL, never a feature.
func mutateOldBlobRefresh(c *Cache, path string, data []byte, halfWay func()) error {
	c.mu.Lock()
	e := c.entries[path]
	if e == nil {
		c.mu.Unlock()
		return fmt.Errorf("control: no entry for %s", path)
	}
	target := c.blobPath(e.Hash)
	oldHash := e.Hash
	c.mu.Unlock()

	f, err := os.OpenFile(target, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	half := len(data) / 2
	if _, err := f.Write(data[:half]); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	halfWay()
	if _, err := f.Write(data[half:]); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// And only now the pointer moves — the half-file window is entirely before
	// this line.
	c.mu.Lock()
	if b := c.blobs[oldHash]; b != nil {
		b.Refs++
	}
	newHash := HashBytes(data)
	c.blobs[newHash] = &blobRef{Hash: newHash, Size: int64(len(data)), Refs: 1}
	c.entries[path] = &cacheEntry{Path: path, Hash: newHash, Size: int64(len(data)), LastHitMS: c.cfg.Now().UnixMilli()}
	c.recountLocked()
	c.mu.Unlock()
	return nil
}

// earlyPublishRefresh is NEGATIVE CONTROL 1b: the other direction of the same
// mistake — publish the pointer (the hash is known from the HEAD, so it is known
// before the body) and then stream the bytes into the destination. A reader that
// looks the path up during the stream is handed the new hash and reads a file
// that is only half written.
func earlyPublishRefresh(c *Cache, path string, data []byte, halfWay func()) error {
	hash := HashBytes(data)
	c.mu.Lock()
	c.blobs[hash] = &blobRef{Hash: hash, Size: int64(len(data)), Refs: 1}
	c.entries[path] = &cacheEntry{Path: path, Hash: hash, Size: int64(len(data)), LastHitMS: c.cfg.Now().UnixMilli()}
	c.recountLocked()
	c.mu.Unlock()

	f, err := os.Create(c.blobPath(hash))
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	half := len(data) / 2
	if _, err := f.Write(data[:half]); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	halfWay()
	if _, err := f.Write(data[half:]); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// runHalfFileCell drives ONE arm and returns what a reader observed at the
// implementation's declared checkpoint, whether the OLD blob file still holds the
// OLD bytes afterwards (the content address staying honest), and what a reader
// observes once the refresh has returned.
func runHalfFileCell(t *testing.T, arm refreshImpl, oldData, newData []byte) (observed string, oldAddressIntact bool, after string) {
	t.Helper()
	c := newStagedCache(t, 8<<20, 4<<20, 64, 2)
	const path = "hot/file"
	oldHash := HashBytes(oldData)
	if _, err := c.Insert(path, oldHash, oldData); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	observedCh := make(chan string, 1)
	release := make(chan struct{})
	errCh := make(chan error, 1)
	halfWay := func() {
		// The observation is taken here, inside the refresher's own goroutine at
		// its declared checkpoint: a controlled schedule, not a race.
		observedCh <- observeOnce(c, path, oldData, newData)
		<-release
	}
	go func() { errCh <- arm.refresh(c, path, newData, halfWay) }()
	got := <-observedCh
	close(release)
	if err := <-errCh; err != nil {
		t.Fatalf("%s: refresh failed: %v", arm.name, err)
	}
	// The content address must not be a lie: the file named by the OLD hash must
	// still hold the OLD bytes (or be legitimately reclaimed, which the caller
	// asserts separately).
	blobFile := filepath.Join(c.Dir(), CacheBlobDir, strings.TrimPrefix(oldHash, HashPrefix))
	onDisk, err := os.ReadFile(blobFile)
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatalf("%s: reading the old blob: %v", arm.name, err)
		}
		return got, true, observeOnce(c, path, oldData, newData)
	}
	return got, bytes.Equal(onDisk, oldData), observeOnce(c, path, oldData, newData)
}

// TestAtomicRefreshHalfFileCell is THE cell: a reader concurrent with a refresh
// must never observe a partial file — and the same cell must turn red for an
// implementation that mutates in place.
func TestAtomicRefreshHalfFileCell(t *testing.T) {
	oldData := stagePayload(256<<10, 1)
	newData := stagePayload(384<<10, 2) // a different length, so a partial is unmistakable

	arms := []refreshImpl{
		{name: "staged-new-blob-swap", refresh: stagedRefresh},
		{name: "control-mutate-old-blob", violation: true, refresh: mutateOldBlobRefresh},
		{name: "control-publish-then-stream", violation: true, refresh: earlyPublishRefresh},
	}
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			observed, oldIntact, after := runHalfFileCell(t, arm, oldData, newData)
			t.Logf("%s: reader observed %q at the half-way checkpoint, %q after the refresh returned; old content address intact=%v",
				arm.name, observed, after, oldIntact)
			failed := halfFileViolation(observed)
			switch {
			case arm.violation && !failed:
				t.Fatalf("NEGATIVE CONTROL IS BLIND: %s observed %q at the checkpoint, so this cell "+
					"cannot catch the half-file bug it exists for", arm.name, observed)
			case !arm.violation && failed:
				t.Fatalf("a reader observed %q during the refresh: a refreshed file was visible "+
					"half-written", observed)
			}
			if arm.violation {
				// A control must be a REAL breach, not merely a missing read: it
				// must either expose half-written bytes (asserted above) or
				// destroy the old content address.
				if oldIntact && !failed {
					t.Fatalf("control %s is neither a half-file breach nor an address breach", arm.name)
				}
				return
			}
			if !oldIntact {
				t.Fatalf("the refresh mutated the blob named by the old content address: the cache's " +
					"own address stopped describing its content")
			}
			if after != "new" {
				t.Fatalf("after a successful refresh the path must serve the complete new content; observed %q", after)
			}
		})
	}
}

// TestConcurrentRefreshObservationMeasurement is the RED as a MEASUREMENT rather
// than a schedule: N reader goroutines hammer the public read path while the
// refresh runs, and every observation is classified. The shipped arm must record
// ZERO partials; the mutate-in-place control records the count, which is the
// number that shows the bug is real and not a hypothesis. The control's window is
// widened deliberately (a sleep between chunks) so the count is a stable
// measurement instead of a coin flip.
func TestConcurrentRefreshObservationMeasurement(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement arm")
	}
	oldData := stagePayload(256<<10, 3)
	newData := stagePayload(384<<10, 4)
	const readers = 8
	const rounds = 12

	measure := func(arm refreshImpl) map[string]int {
		c := newStagedCache(t, 8<<20, 4<<20, 64, 2)
		const path = "hot/file"
		if _, err := c.Insert(path, HashBytes(oldData), oldData); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
		var mu sync.Mutex
		counts := map[string]int{}
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					v := observeCounted(c, path, oldData, newData)
					mu.Lock()
					counts[v]++
					mu.Unlock()
				}
			}()
		}
		for r := 0; r < rounds; r++ {
			// The arm's own concurrency: the refresher runs in this goroutine
			// while the readers run against it.
			err := arm.refresh(c, path, newData, func() {
				if arm.violation {
					// Widen the window so the measurement is stable. The staged
					// representation has no window to widen.
					time.Sleep(2 * time.Millisecond)
				}
			})
			if err != nil {
				t.Fatalf("%s: refresh %d: %v", arm.name, r, err)
			}
			if r < rounds-1 {
				// Re-seed the OLD version so each round refreshes the same way.
				if _, err := c.Insert(path, HashBytes(oldData), oldData); err != nil {
					t.Fatalf("%s: re-seed: %v", arm.name, err)
				}
			}
		}
		close(stop)
		wg.Wait()
		return counts
	}

	shipped := measure(refreshImpl{name: "staged-new-blob-swap", refresh: stagedRefresh})
	t.Logf("MEASUREMENT staged-new-blob-swap: %v", shipped)
	if shipped["partial"] != 0 {
		t.Fatalf("the shipped representation observed %d partial reads under concurrency: %v",
			shipped["partial"], shipped)
	}
	if shipped["old"] == 0 || shipped["new"] == 0 {
		t.Fatalf("the measurement is vacuous: it never observed both sides of the refresh: %v", shipped)
	}

	control := measure(refreshImpl{name: "control-mutate-old-blob", violation: true, refresh: mutateOldBlobRefresh})
	t.Logf("MEASUREMENT control-mutate-old-blob: %v", control)
	if control["partial"] == 0 {
		t.Fatalf("the mutate-in-place control recorded no partial reads: the measurement is blind: %v", control)
	}
}

// ── a reader that started before the swap keeps the complete old content ─────

// TestReaderHoldingTheOldBlobReadsTheOldCompleteContent proves the refcount
// corollary: a reader that acquired the old blob before the swap still reads the
// complete OLD content afterwards, and the old blob's bytes are still on disk
// because that reader's own reference keeps them.
func TestReaderHoldingTheOldBlobReadsTheOldCompleteContent(t *testing.T) {
	c := newStagedCache(t, 4<<20, 2<<20, 64, 2)
	const path = "hot/file"
	oldData := stagePayload(128<<10, 5)
	newData := stagePayload(128<<10, 6) // same length: only the bytes differ
	oldHash := HashBytes(oldData)
	if _, err := c.Insert(path, oldHash, oldData); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	// The reader starts BEFORE the swap, and takes its reference atomically.
	held, ok := c.GetPinned(path, oldHash)
	if !ok {
		t.Fatalf("the seeded entry must be readable")
	}
	defer c.Unpin(oldHash)
	oldBlob := filepath.Join(c.Dir(), CacheBlobDir, strings.TrimPrefix(oldHash, HashPrefix))

	stageAndCommit(t, c, path, newData)

	// The pointer moved...
	if hash, _, ok := c.Lookup(path); !ok || hash != HashBytes(newData) {
		t.Fatalf("after the commit the path must name the new content: hash=%q ok=%v", hash, ok)
	}
	// ...and the reader that started before it still holds the complete old bytes.
	if !bytes.Equal(held, oldData) {
		t.Fatalf("the reader's own copy changed under it")
	}
	// The old blob is still ON DISK, kept alive by that reader's reference: the
	// swap may not reclaim bytes a reader is holding. (Without the reference the
	// swap reclaims them at once — see the arm below.)
	if _, err := os.Stat(oldBlob); err != nil {
		t.Fatalf("the old blob was reclaimed while a reader still held it: %v", err)
	}
	if got, err := os.ReadFile(oldBlob); err != nil || !bytes.Equal(got, oldData) {
		t.Fatalf("the old blob must still hold the old bytes while pinned (err=%v)", err)
	}
	// A reader starting after the swap gets the new complete content.
	if got := observeOnce(c, path, oldData, newData); got != "new" {
		t.Fatalf("a reader after the swap observed %q", got)
	}
	// Releasing the last reference is where the old bytes finally go.
	c.Unpin(oldHash)
	if _, err := os.Stat(oldBlob); !os.IsNotExist(err) {
		t.Fatalf("the old blob must be reclaimed once its last reader released it (err=%v)", err)
	}
}

// TestEvictionRespectsPinnedInFlightReaders drives real eviction pressure while a
// reader holds the old blob, and proves the two directions of the rule: a pinned
// blob survives both the swap and the pressure, and an unpinned one is reclaimed
// by the swap itself.
func TestEvictionRespectsPinnedInFlightReaders(t *testing.T) {
	c := newStagedCache(t, 1<<20, 512<<10, 64, 2)
	const path = "hot/file"
	oldData := stagePayload(64<<10, 7)
	oldHash := HashBytes(oldData)
	if _, err := c.Insert(path, oldHash, oldData); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	pinned, ok := c.GetPinned(path, oldHash)
	if !ok {
		t.Fatalf("the seeded entry must be readable")
	}
	oldBlob := filepath.Join(c.Dir(), CacheBlobDir, strings.TrimPrefix(oldHash, HashPrefix))

	stageAndCommit(t, c, path, stagePayload(64<<10, 8))

	// Now push the cache hard: 12 entries of 96 KiB into a 1 MiB cap forces
	// eviction on nearly every insert.
	for i := 0; i < 12; i++ {
		data := stagePayload(96<<10, byte(20+i))
		if _, err := c.Insert(fmt.Sprintf("other/%d", i), HashBytes(data), data); err != nil {
			t.Fatalf("filler insert %d: %v", i, err)
		}
	}
	if !bytes.Equal(pinned, oldData) {
		t.Fatalf("the pinned reader's bytes changed")
	}
	if _, err := os.Stat(oldBlob); err != nil {
		t.Fatalf("eviction pressure reclaimed a blob a reader still held: %v", err)
	}
	if got, err := os.ReadFile(oldBlob); err != nil || !bytes.Equal(got, oldData) {
		t.Fatalf("the pinned blob must still hold the old bytes (err=%v)", err)
	}
	c.Unpin(oldHash)
	if _, err := os.Stat(oldBlob); !os.IsNotExist(err) {
		t.Fatalf("the pinned blob must be reclaimed when the reader releases it (err=%v)", err)
	}

	// The other direction: with NO reader holding it, the swap reclaims the old
	// blob immediately — which is why the pin is what "keeps it alive" means.
	// (On its own cache, because the first one is deliberately full.)
	c2 := newStagedCache(t, 1<<20, 512<<10, 64, 2)
	const path2 = "hot/unread"
	old2 := stagePayload(64<<10, 9)
	old2Hash := HashBytes(old2)
	if _, err := c2.Insert(path2, old2Hash, old2); err != nil {
		t.Fatalf("seed insert 2: %v", err)
	}
	cleanupBlob := filepath.Join(c2.Dir(), CacheBlobDir, strings.TrimPrefix(old2Hash, HashPrefix))
	stageAndCommit(t, c2, path2, stagePayload(64<<10, 10))
	if _, err := os.Stat(cleanupBlob); !os.IsNotExist(err) {
		t.Fatalf("an unpinned superseded blob must be reclaimed by the swap (err=%v)", err)
	}
}

// TestRefreshOfIdenticalContentKeepsTheBlob is the most common refresh outcome
// there is — the fetch returns the bytes the cache already holds — and it is the
// case where the swap's own reference and the old entry's reference name the SAME
// blob. The reference must be taken before the old entry is dropped, or the
// pointer ends up naming a file the drop just reclaimed.
func TestRefreshOfIdenticalContentKeepsTheBlob(t *testing.T) {
	c := newStagedCache(t, 1<<20, 512<<10, 64, 2)
	const path = "hot/file"
	data := stagePayload(64<<10, 15)
	hash := HashBytes(data)
	if _, err := c.Insert(path, hash, data); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	before := c.Stats()
	refsBefore := c.blobs[hash].Refs
	blobsBefore := len(c.blobs)

	// The identical-content refresh is a normal commit: the same bytes, the same
	// content address, written to a new staged file and published by the swap.
	s, err := c.Stage(path, hash, int64(len(data)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := s.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := s.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	after := c.Stats()
	if got := observeOnce(c, path, data, nil); got != "old" {
		t.Fatalf("the identical-content refresh must still serve its bytes; observed %q", got)
	}
	if c.blobs[hash] == nil {
		t.Fatalf("the refresh of identical content reclaimed the blob it points at")
	}
	if got := c.blobs[hash].Refs; got != refsBefore {
		t.Fatalf("the blob's references drifted on an identical-content refresh: %d -> %d", refsBefore, got)
	}
	if got := len(c.blobs); got != blobsBefore {
		t.Fatalf("the blob census drifted: %d -> %d", blobsBefore, got)
	}
	if _, err := os.Stat(filepath.Join(c.Dir(), CacheBlobDir, strings.TrimPrefix(hash, HashPrefix))); err != nil {
		t.Fatalf("the blob file must still exist after an identical-content refresh: %v", err)
	}
	if after.UsedBytes != before.UsedBytes {
		t.Fatalf("an identical-content refresh must cost no bytes: %d -> %d", before.UsedBytes, after.UsedBytes)
	}
	if after.InFlightBytes != 0 || after.ReservedBytes != after.UsedBytes {
		t.Fatalf("the reservation must be released: %+v", after)
	}
	if d := dirBytes(t, c.Dir()); d != after.UsedBytes {
		t.Fatalf("at rest the directory must hold exactly used_bytes: %d vs %d", d, after.UsedBytes)
	}
	if after.StagedCommittedTotal != 1 {
		t.Fatalf("staged_committed_total=%d, want 1", after.StagedCommittedTotal)
	}
}

// TestPublishedFigureNeverOverstatesTheDirectory is the one-directional
// invariant that survives this row's change, plus the honest measurement of what
// it does NOT cover. `used_bytes` must never claim more bytes than the directory
// holds (an overstated figure would let the cache quietly exceed its bound); the
// other direction is NOT claimed, because the mount directory also holds
// status.json and conflicts.jsonl, which are outside the blob+index accounting —
// that gap is BFS-031's filed defect and this row deliberately does not touch it.
// What this row does own is that the STAGED bytes are inside the reservation,
// which the third assertion measures.
func TestPublishedFigureNeverOverstatesTheDirectory(t *testing.T) {
	c := newStagedCache(t, 4<<20, 2<<20, 64, 2)
	const path = "hot/file"
	data := stagePayload(32<<10, 16)
	if _, err := c.Insert(path, HashBytes(data), data); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	// The mount writes its status document into the same directory.
	if err := WriteStatus(c.Dir(), Status{Mount: "probe", Cache: c.Stats()}); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}
	st := c.Stats()
	dir := dirBytes(t, c.Dir())
	gap := dir - st.UsedBytes
	t.Logf("residual (BFS-031's row, NOT repaired here): used_bytes=%d, directory=%d, gap=%d bytes "+
		"(%s, outside the blob+index accounting)", st.UsedBytes, dir, gap, StatusFile)
	if st.UsedBytes > dir {
		t.Fatalf("used_bytes=%d overstates the directory (%d bytes)", st.UsedBytes, dir)
	}

	// The half this row owns: a staged refresh's bytes are inside the reservation,
	// so the figure that must stay under the bound covers them.
	newData := stagePayload(48<<10, 17)
	s, err := c.Stage(path, HashBytes(newData), int64(len(newData)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := s.Write(newData); err != nil {
		t.Fatalf("Write: %v", err)
	}
	mid := c.Stats()
	if mid.UsedBytes != st.UsedBytes {
		t.Fatalf("the published figure moved while a refresh was in flight")
	}
	if mid.InFlightBytes != int64(len(newData)) {
		t.Fatalf("in_flight_bytes=%d, want %d", mid.InFlightBytes, len(newData))
	}
	if mid.ReservedBytes < mid.UsedBytes+mid.InFlightBytes {
		t.Fatalf("reserved_bytes=%d does not cover used+in-flight (%d)",
			mid.ReservedBytes, mid.UsedBytes+mid.InFlightBytes)
	}
	if err := s.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
}

// stageAndCommit refreshes path with data through the shipped representation and
// fails the test if it cannot.
func stageAndCommit(t *testing.T, c *Cache, path string, data []byte) *StagedRefresh {
	t.Helper()
	return stageAndCommitExpected(t, c, path, data, HashBytes(data))
}

// stageAndCommitExpected writes data but declares declaredHash as its content
// address, so a caller can drive the content-address verification.
func stageAndCommitExpected(t *testing.T, c *Cache, path string, data []byte, declaredHash string) *StagedRefresh {
	t.Helper()
	s, err := c.Stage(path, declaredHash, int64(len(data)))
	if err != nil {
		t.Fatalf("Stage(%s): %v", path, err)
	}
	if _, err := s.Write(data); err != nil {
		t.Fatalf("Write(%s): %v", path, err)
	}
	if _, err := s.Commit(); err != nil {
		t.Fatalf("Commit(%s): %v", path, err)
	}
	return s
}

// ── an abandoned refresh discards its blob and never swaps ───────────────────

// TestAbandonedRefreshDiscardsItsBlobAndNeverSwaps is Q-16: abandoning a refresh
// must be UNOBSERVABLE to every reader — same bytes, same used_bytes, same entry,
// no residue — and it must never swap.
func TestAbandonedRefreshDiscardsItsBlobAndNeverSwaps(t *testing.T) {
	c := newStagedCache(t, 4<<20, 2<<20, 64, 2)
	const path = "hot/file"
	oldData := stagePayload(96<<10, 11)
	oldHash := HashBytes(oldData)
	if _, err := c.Insert(path, oldHash, oldData); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	before := c.Stats()
	beforeBytes := dirBytes(t, c.Dir())
	beforeHash, beforeSize, _ := c.Lookup(path)
	beforeRead := observeOnce(c, path, oldData, nil)

	s, err := c.Stage(path, HashBytes(stagePayload(96<<10, 12)), 96<<10)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := s.Write(stagePayload(96<<10, 12)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// While the refresh is in flight the reader must see the complete old content
	// and the published figure must not have moved.
	if got := observeOnce(c, path, oldData, stagePayload(96<<10, 12)); got != "old" {
		t.Fatalf("a reader during an in-flight refresh observed %q", got)
	}
	mid := c.Stats()
	if mid.UsedBytes != before.UsedBytes {
		t.Fatalf("an in-flight refresh changed the published figure: %d -> %d", before.UsedBytes, mid.UsedBytes)
	}
	if mid.InFlightBytes != 96<<10 {
		t.Fatalf("in_flight_bytes=%d, want %d", mid.InFlightBytes, 96<<10)
	}

	if err := s.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	// Idempotent: a second abandonment (or a cancel that never arrived, and then
	// one that did) is a no-op rather than an error.
	if err := s.Abort(); err != nil {
		t.Fatalf("Abort must be idempotent: %v", err)
	}
	// And a Commit after an Abort must not publish anything.
	if _, err := s.Commit(); !errors.Is(err, ErrStageClosed) {
		t.Fatalf("Commit after Abort must be refused, got %v", err)
	}

	after := c.Stats()
	if after.UsedBytes != before.UsedBytes || after.Entries != before.Entries || after.Blobs != before.Blobs {
		t.Fatalf("abandonment was observable in the published figures: %+v -> %+v", before, after)
	}
	if after.StagedAbortedTotal != before.StagedAbortedTotal+1 {
		t.Fatalf("staged_aborted_total did not move: %d -> %d", before.StagedAbortedTotal, after.StagedAbortedTotal)
	}
	if after.InFlightBytes != 0 || after.StagedBlobs != 0 {
		t.Fatalf("the reservation survived the abandon: in_flight=%d staged=%d", after.InFlightBytes, after.StagedBlobs)
	}
	if after.ReservedBytes != after.UsedBytes {
		t.Fatalf("reserved_bytes=%d must equal used_bytes=%d at rest", after.ReservedBytes, after.UsedBytes)
	}
	hash, size, ok := c.Lookup(path)
	if !ok || hash != beforeHash || size != beforeSize {
		t.Fatalf("the path's entry changed on abandonment: %q/%d -> %q/%d", beforeHash, beforeSize, hash, size)
	}
	if got := observeOnce(c, path, oldData, nil); got != beforeRead {
		t.Fatalf("the reader's verdict changed on abandonment: %q -> %q", beforeRead, got)
	}
	if got := dirBytes(t, c.Dir()); got != beforeBytes {
		t.Fatalf("abandonment left residue: the directory grew from %d to %d bytes", beforeBytes, got)
	}
	assertNoStageResidue(t, c)
}

// assertNoStageResidue fails if any staged file is left inside the blob dir.
func assertNoStageResidue(t *testing.T, c *Cache) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(c.Dir(), CacheBlobDir))
	if err != nil {
		t.Fatalf("read blob dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), CacheStagePrefix) {
			t.Fatalf("a staged blob was left behind: %s", e.Name())
		}
	}
}

// ── the two accounts, both reported ─────────────────────────────────────────

// TestTwoAccountsAreReportedSeparately is F-1's contract: eviction reasons about
// PUBLISHED bytes, admission reserves PUBLISHED + IN-FLIGHT, and the status
// record carries both figures — never merged into one number.
func TestTwoAccountsAreReportedSeparately(t *testing.T) {
	c := newStagedCache(t, 1<<20, 512<<10, 64, 2)
	const path = "hot/file"
	oldData := stagePayload(64<<10, 13)
	newData := stagePayload(96<<10, 14)
	if _, err := c.Insert(path, HashBytes(oldData), oldData); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	atRest := c.Stats()
	if atRest.ReservedBytes != atRest.UsedBytes || atRest.InFlightBytes != 0 {
		t.Fatalf("at rest the two accounts must agree: %+v", atRest)
	}

	s, err := c.Stage(path, HashBytes(newData), int64(len(newData)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := s.Write(newData[:len(newData)/2]); err != nil {
		t.Fatalf("Write: %v", err)
	}
	inFlight := c.Stats()
	half := int64(len(newData) / 2)
	if inFlight.UsedBytes != atRest.UsedBytes {
		t.Fatalf("the published figure moved while a refresh was in flight: %d -> %d",
			atRest.UsedBytes, inFlight.UsedBytes)
	}
	if inFlight.InFlightBytes != half {
		t.Fatalf("in_flight_bytes=%d, want the bytes really written (%d)", inFlight.InFlightBytes, half)
	}
	if inFlight.ReservedBytes != inFlight.UsedBytes+c.stagedReservedLocked() {
		t.Fatalf("reserved_bytes=%d must be published + the staged reservations (%d): %+v",
			inFlight.ReservedBytes, inFlight.UsedBytes+c.stagedReservedLocked(), inFlight)
	}
	if c.stagedReservedLocked() < inFlight.InFlightBytes {
		t.Fatalf("the reservation must cover every byte really written: reserved=%d written=%d",
			c.stagedReservedLocked(), inFlight.InFlightBytes)
	}
	if inFlight.ReservedBytes <= inFlight.UsedBytes {
		t.Fatalf("the reservation must be visible as its own figure: %+v", inFlight)
	}
	if inFlight.StagedBlobs != 1 || inFlight.MaxInFlight != 2 || inFlight.MaxEntries != 64 {
		t.Fatalf("the widths must be reported: %+v", inFlight)
	}
	// The in-flight bytes are REALLY on disk — that is the whole point of
	// admitting them — and yet no reader can reach them.
	if d := dirBytes(t, c.Dir()); d < inFlight.InFlightBytes {
		t.Fatalf("the directory holds %d bytes but in_flight_bytes claims %d", d, inFlight.InFlightBytes)
	}
	if hash, _, ok := c.Lookup("hot/file"); !ok || hash != HashBytes(oldData) {
		t.Fatalf("the in-flight bytes are reachable through the index: %q", hash)
	}
	if _, ok := c.Get(path, HashBytes(newData)); ok {
		t.Fatalf("a staged blob was served to a reader before its pointer swap")
	}

	if _, err := s.Write(newData[len(newData)/2:]); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	if _, err := s.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	published := c.Stats()
	if published.InFlightBytes != 0 || published.StagedBlobs != 0 {
		t.Fatalf("the reservation must end at the publish: %+v", published)
	}
	if published.ReservedBytes != published.UsedBytes {
		t.Fatalf("after the publish reserved_bytes=%d must equal used_bytes=%d",
			published.ReservedBytes, published.UsedBytes)
	}
	if published.UsedBytes <= atRest.UsedBytes {
		t.Fatalf("the published figure must have grown with the new blob: %d -> %d",
			atRest.UsedBytes, published.UsedBytes)
	}
	// The handoff is continuous: the bound is never briefly unenforced.
	if published.UsedBytes > inFlight.ReservedBytes+c.indexGrowthLocked(path) {
		t.Fatalf("the publish overshot the reservation: used=%d reserved-before=%d",
			published.UsedBytes, inFlight.ReservedBytes)
	}

	// THE STATUS RECORD carries both, as separate fields (O-3: never merged).
	raw, err := json.Marshal(Status{Mount: "m", Cache: published})
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	cache, ok := doc["cache"].(map[string]any)
	if !ok {
		t.Fatalf("the status record has no cache block: %s", raw)
	}
	for _, key := range []string{"used_bytes", "reserved_bytes", "in_flight_bytes", "max_entries", "entries", "max_inflight"} {
		if _, present := cache[key]; !present {
			t.Fatalf("the status record is missing %q: %s", key, raw)
		}
	}
	if got := int64(cache["used_bytes"].(float64)); got != published.UsedBytes {
		t.Fatalf("status used_bytes=%d, want %d", got, published.UsedBytes)
	}
	if got := int64(cache["reserved_bytes"].(float64)); got != published.ReservedBytes {
		t.Fatalf("status reserved_bytes=%d, want %d", got, published.ReservedBytes)
	}
	if got := int(cache["max_entries"].(float64)); got != 64 {
		t.Fatalf("status max_entries=%d, want 64", got)
	}
}

// TestInFlightBytesDecideAdmission is F-1's proof, and NEGATIVE CONTROL 2's arm:
// with a published cache near the bound, a second in-flight refresh must be
// REFUSED — because its bytes are really on disk. If admission counted only the
// published figure, the directory would go over the bound while every reported
// figure still read "inside", which is BFS-031's defect one layer up.
func TestInFlightBytesDecideAdmission(t *testing.T) {
	const maxBytes = 1 << 20
	c := newStagedCache(t, maxBytes, 1<<20, 64, 2)
	// Publish most of the bound, so the cache is "at 99%" in miniature.
	published := int64(0)
	for i := 0; published < 450<<10; i++ {
		data := stagePayload(150<<10, byte(30+i))
		if _, err := c.Insert(fmt.Sprintf("warm/%d", i), HashBytes(data), data); err != nil {
			t.Fatalf("warm insert %d: %v", i, err)
		}
		published = c.Stats().UsedBytes
	}
	evictionsBefore := c.Stats().EvictionsTotal
	entriesBefore := c.Stats().Entries

	free := maxBytes - c.Stats().ReservedBytes
	e := free - 8192 // the size ONE refresh can take with room to spare
	if e < 64<<10 {
		t.Fatalf("the fixture left too little room to be meaningful: free=%d", free)
	}
	da := stagePayload(int(e), 40)
	s1, err := c.Stage("hot/a", HashBytes(da), e)
	if err != nil {
		t.Fatalf("the FIRST refresh must be admitted (it fits under the bound): %v", err)
	}
	if _, err := s1.Write(da); err != nil {
		t.Fatalf("writing the first refresh: %v", err)
	}
	mid := c.Stats()
	if mid.InFlightBytes != e {
		t.Fatalf("in_flight_bytes=%d, want %d", mid.InFlightBytes, e)
	}
	if mid.UsedBytes != published {
		t.Fatalf("the published figure moved while a refresh was in flight: %d -> %d", published, mid.UsedBytes)
	}
	t.Logf("F-1 arithmetic: published used_bytes=%d, in_flight_bytes=%d, reserved_bytes=%d, max_bytes=%d; "+
		"a published-only admission check would admit a second %d-byte refresh (published+2x = %d bytes on disk, "+
		"%.1f%% over the bound) while the directory reads 'inside'",
		mid.UsedBytes, mid.InFlightBytes, mid.ReservedBytes, maxBytes, e, mid.UsedBytes+2*e,
		100*float64(mid.UsedBytes+2*e-maxBytes)/float64(maxBytes))

	db := stagePayload(int(e), 41)
	s2, err := c.Stage("hot/b", HashBytes(db), e)
	if err == nil {
		// This is the mutation arm: admission let a second in-flight blob in
		// while the PUBLISHED figure said there was room. Write it and measure.
		_, _ = s2.Write(db)
		_, _ = s2.Commit()
		t.Fatalf("OVER BOUND: admission admitted a second %d-byte refresh against used_bytes=%d "+
			"(in_flight was %d, so the directory reached %d bytes against max_bytes=%d): "+
			"admission is not accounting for in-flight bytes",
			e, mid.UsedBytes, mid.InFlightBytes, dirBytes(t, c.Dir()), maxBytes)
	}
	if !errors.Is(err, ErrNoRoom) {
		t.Fatalf("the second refresh must be refused with no_room, got %v", err)
	}
	after := c.Stats()
	if after.StagedNoRoomTotal != 1 {
		t.Fatalf("staged_no_room_total=%d, want 1", after.StagedNoRoomTotal)
	}
	// Q-8: a refusal is a refusal, never an eviction of published bytes.
	if after.EvictionsTotal != evictionsBefore {
		t.Fatalf("a REFUSED refresh evicted a published blob: evictions %d -> %d",
			evictionsBefore, after.EvictionsTotal)
	}
	if after.Entries != entriesBefore {
		t.Fatalf("a refused refresh changed the entry census: %d -> %d", entriesBefore, after.Entries)
	}
	// Both figures stay under the bound, and so does the DIRECTORY.
	if after.ReservedBytes > after.MaxBytes {
		t.Fatalf("reserved_bytes=%d exceeds max_bytes=%d", after.ReservedBytes, after.MaxBytes)
	}
	if d := dirBytes(t, c.Dir()); d > maxBytes {
		t.Fatalf("the cache directory holds %d bytes against a bound of %d", d, maxBytes)
	}
	// And the incremental check agrees with admission: a write that would pass
	// the bound mid-stream is refused too (an unknown or lying size cannot
	// overshoot). The chunk is small enough that the per-entry cap is not the
	// reason — the refusal has to be the bound.
	if _, werr := s1.Write(stagePayload(64<<10, 42)); !errors.Is(werr, ErrNoRoom) {
		t.Fatalf("a staged write past the bound must be refused, got %v", werr)
	}
	if err := s1.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if st := c.Stats(); st.InFlightBytes != 0 || st.ReservedBytes != st.UsedBytes {
		t.Fatalf("the abandoned reservation must be released: %+v", st)
	}
	if d := dirBytes(t, c.Dir()); d != c.Stats().UsedBytes {
		t.Fatalf("at rest the directory must hold exactly used_bytes: %d vs %d", d, c.Stats().UsedBytes)
	}
}

// TestEntryBoundIsEnforcedAndReported is BFS-031's lesson applied: the byte bound
// alone does not bound a directory, so the entry count is a second bound, it is
// enforced on both the insert path and the refresh path, and it is reported.
func TestEntryBoundIsEnforcedAndReported(t *testing.T) {
	const maxBytes = 8 << 20
	const maxEntries = 8
	c := newStagedCache(t, maxBytes, 1<<20, maxEntries, 2)
	for i := 0; i < 100; i++ {
		data := stagePayload(1<<10, byte(i))
		if _, err := c.Insert(fmt.Sprintf("tiny/%d", i), HashBytes(data), data); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	st := c.Stats()
	if st.MaxEntries != maxEntries {
		t.Fatalf("the entry bound must be reported: max_entries=%d, want %d", st.MaxEntries, maxEntries)
	}
	if st.Entries > maxEntries {
		t.Fatalf("the directory holds %d entries against an entry bound of %d", st.Entries, maxEntries)
	}
	if st.EvictionsTotal == 0 {
		t.Fatalf("nothing was evicted: the entry bound did not bind")
	}
	// The measurement that makes the second bound load-bearing: the byte figure
	// is nowhere near the byte bound while the entry bound is at its ceiling.
	t.Logf("entry bound: entries=%d/%d, used_bytes=%d of max_bytes=%d (%.2f%%), evictions_total=%d",
		st.Entries, st.MaxEntries, st.UsedBytes, st.MaxBytes,
		100*float64(st.UsedBytes)/float64(st.MaxBytes), st.EvictionsTotal)
	if st.UsedBytes*100 > maxBytes {
		t.Fatalf("the fixture must keep the byte figure far below its bound (used=%d, max=%d)",
			st.UsedBytes, maxBytes)
	}
	if d := dirBytes(t, c.Dir()); d > maxBytes {
		t.Fatalf("the directory holds %d bytes against a byte bound of %d", d, maxBytes)
	}

	// The refresh path REFUSES at the entry bound (it never evicts to fit).
	if _, err := c.Stage("hot/new", HashBytes(stagePayload(64<<10, 60)), 64<<10); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("a staged refresh must be refused at the entry bound, got %v", err)
	} else if !strings.Contains(err.Error(), "entry bound") {
		t.Fatalf("the refusal must name the dimension it refused on: %v", err)
	}
	// ...but refreshing a path that is ALREADY indexed adds no entry, so it is
	// admitted: the entry bound is not a blanket refusal.
	live := fmt.Sprintf("tiny/%d", 99) // the last insert is the one still indexed
	if hash, _, ok := c.Lookup(live); !ok {
		t.Fatalf("the fixture entry %s must still be present", live)
	} else {
		s, err := c.Stage(live, hash, 1<<10)
		if err != nil {
			t.Fatalf("refreshing an already-indexed path must be admitted: %v", err)
		}
		_ = s.Abort()
	}
	if st := c.Stats(); st.StagedNoRoomTotal != 1 {
		t.Fatalf("staged_no_room_total=%d, want 1", st.StagedNoRoomTotal)
	}
}

// TestEveryStagedRefusalCanMove covers BFS-032's rule for the counters this row
// adds: a counter that cannot move is a gap, not a green check. Every refusal and
// every terminal outcome get a trigger here.
func TestEveryStagedRefusalCanMove(t *testing.T) {
	t.Run("committed", func(t *testing.T) {
		c := newStagedCache(t, 1<<20, 512<<10, 64, 2)
		stageAndCommit(t, c, "hot/a", stagePayload(8<<10, 70))
		if st := c.Stats(); st.StagedCommittedTotal != 1 {
			t.Fatalf("staged_committed_total=%d, want 1", st.StagedCommittedTotal)
		}
	})
	t.Run("aborted", func(t *testing.T) {
		c := newStagedCache(t, 1<<20, 512<<10, 64, 2)
		s, err := c.Stage("hot/a", HashBytes(stagePayload(8<<10, 71)), 8<<10)
		if err != nil {
			t.Fatalf("Stage: %v", err)
		}
		if err := s.Abort(); err != nil {
			t.Fatalf("Abort: %v", err)
		}
		if st := c.Stats(); st.StagedAbortedTotal != 1 {
			t.Fatalf("staged_aborted_total=%d, want 1", st.StagedAbortedTotal)
		}
	})
	t.Run("no_room_bytes", func(t *testing.T) {
		c := newStagedCache(t, 64<<10, 64<<10, 64, 2)
		if _, err := c.Stage("hot/a", HashBytes(stagePayload(1<<10, 72)), 128<<10); !errors.Is(err, ErrNoRoom) {
			t.Fatalf("want ErrNoRoom, got %v", err)
		}
		if st := c.Stats(); st.StagedNoRoomTotal != 1 {
			t.Fatalf("staged_no_room_total=%d, want 1", st.StagedNoRoomTotal)
		}
	})
	t.Run("no_slot", func(t *testing.T) {
		c := newStagedCache(t, 1<<20, 512<<10, 64, 1)
		if _, err := c.Stage("hot/a", HashBytes(stagePayload(8<<10, 73)), 8<<10); err != nil {
			t.Fatalf("the first stage must be admitted: %v", err)
		}
		if _, err := c.Stage("hot/b", HashBytes(stagePayload(8<<10, 74)), 8<<10); !errors.Is(err, ErrNoSlot) {
			t.Fatalf("want ErrNoSlot, got %v", err)
		}
		if st := c.Stats(); st.StagedNoSlotTotal != 1 {
			t.Fatalf("staged_no_slot_total=%d, want 1", st.StagedNoSlotTotal)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		c := newStagedCache(t, 0, 0, 64, 2)
		if _, err := c.Stage("hot/a", HashBytes(stagePayload(8<<10, 75)), 8<<10); !errors.Is(err, ErrCacheDisabled) {
			t.Fatalf("want ErrCacheDisabled, got %v", err)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		c := newStagedCache(t, 1<<20, 512<<10, 64, 2)
		if _, err := c.Stage("", HashBytes(stagePayload(8<<10, 76)), 8<<10); err == nil {
			t.Fatalf("a stage needs a path")
		}
		if _, err := c.Stage("hot/a", "sha256:nothex", 8<<10); err == nil {
			t.Fatalf("a stage needs a well-formed hash")
		}
	})
}

// TestStagedContentAddressIsVerifiedBeforePublish proves the publish cannot file
// bytes under a name they do not hash to — the same refusal Insert makes, moved
// to the streaming path where it has to happen before the swap.
func TestStagedContentAddressIsVerifiedBeforePublish(t *testing.T) {
	c := newStagedCache(t, 1<<20, 512<<10, 64, 2)
	const path = "hot/file"
	oldData := stagePayload(32<<10, 80)
	oldHash := HashBytes(oldData)
	if _, err := c.Insert(path, oldHash, oldData); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	before := c.Stats()
	beforeBytes := dirBytes(t, c.Dir())

	lying := stagePayload(32<<10, 81)
	s, err := c.Stage(path, HashBytes(stagePayload(32<<10, 82)), int64(len(lying)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := s.Write(lying); err != nil {
		t.Fatalf("Write: %v", err)
	}
	outcome, err := s.Commit()
	if err == nil {
		t.Fatalf("a commit whose bytes do not hash to the declared address must be refused")
	}
	if outcome != OutcomeBypass {
		t.Fatalf("outcome=%v, want bypass", outcome)
	}
	// No swap, no residue, no counter lost.
	if hash, _, _ := c.Lookup(path); hash != oldHash {
		t.Fatalf("the refused commit swapped the pointer: %q", hash)
	}
	if got := observeOnce(c, path, oldData, lying); got != "old" {
		t.Fatalf("a reader observed %q after a refused commit", got)
	}
	after := c.Stats()
	if after.StagedCommittedTotal != before.StagedCommittedTotal {
		t.Fatalf("a refused commit was counted as committed")
	}
	if after.StagedAbortedTotal != before.StagedAbortedTotal+1 {
		t.Fatalf("the refused commit must be counted as an abandonment: %d -> %d",
			before.StagedAbortedTotal, after.StagedAbortedTotal)
	}
	if after.InFlightBytes != 0 || after.StagedBlobs != 0 {
		t.Fatalf("the refused commit left a reservation: %+v", after)
	}
	if got := dirBytes(t, c.Dir()); got != beforeBytes {
		t.Fatalf("the refused commit left residue: %d -> %d bytes", beforeBytes, got)
	}
	if _, err := s.Write([]byte("more")); !errors.Is(err, ErrStageClosed) {
		t.Fatalf("a finished stage must not accept more bytes, got %v", err)
	}
	if _, err := s.Commit(); !errors.Is(err, ErrStageClosed) {
		t.Fatalf("a finished stage must not commit twice, got %v", err)
	}
}

// TestStagedWriteEnforcesBothBounds drives the two structural bounds on the
// staging path: the per-entry cap (a blob that could never be published) and the
// admission reservation.
func TestStagedWriteEnforcesBothBounds(t *testing.T) {
	t.Run("per_entry_cap", func(t *testing.T) {
		c := newStagedCache(t, 1<<20, 16<<10, 64, 2)
		s, err := c.Stage("hot/a", HashBytes(stagePayload(32<<10, 90)), 0)
		if err != nil {
			t.Fatalf("Stage: %v", err)
		}
		defer func() { _ = s.Abort() }()
		if _, err := s.Write(stagePayload(32<<10, 90)); !errors.Is(err, ErrOversize) {
			t.Fatalf("a staged write past the per-entry cap must be refused with ErrOversize, got %v", err)
		}
		if s.Bytes() != 0 {
			t.Fatalf("the refused write must not have landed: %d bytes", s.Bytes())
		}
	})
	t.Run("reservation_mid_stream", func(t *testing.T) {
		c := newStagedCache(t, 64<<10, 64<<10, 64, 2)
		if _, err := c.Insert("warm", HashBytes(stagePayload(8<<10, 91)), stagePayload(8<<10, 91)); err != nil {
			t.Fatalf("warm insert: %v", err)
		}
		before := c.Stats()
		beforeBytes := dirBytes(t, c.Dir())
		s, err := c.Stage("hot/a", HashBytes(stagePayload(56<<10, 92)), 0) // size unknown
		if err != nil {
			t.Fatalf("Stage: %v", err)
		}
		if _, err := s.Write(stagePayload(32<<10, 92)); err != nil {
			t.Fatalf("the first chunk must be admitted: %v", err)
		}
		if _, err := s.Write(stagePayload(32<<10, 93)); !errors.Is(err, ErrNoRoom) {
			t.Fatalf("a chunk that would pass the bound must be refused, got %v", err)
		}
		if err := s.Abort(); err != nil {
			t.Fatalf("Abort: %v", err)
		}
		after := c.Stats()
		if after.UsedBytes != before.UsedBytes {
			t.Fatalf("the published figure changed: %d -> %d", before.UsedBytes, after.UsedBytes)
		}
		if after.InFlightBytes != 0 || after.ReservedBytes != after.UsedBytes {
			t.Fatalf("the reservation survived the abort: %+v", after)
		}
		if got := dirBytes(t, c.Dir()); got != beforeBytes {
			t.Fatalf("the aborted stage left residue: %d -> %d bytes", beforeBytes, got)
		}
	})
}

// TestStagedCrashResidueIsSweptOnReopen is the crash half of abandonment: a mount
// killed mid-refresh leaves its staged file behind, and reopening the cache must
// not inherit it — the residue goes, and the reported figures agree with the
// directory again.
func TestStagedCrashResidueIsSweptOnReopen(t *testing.T) {
	dir := t.TempDir()
	cfg := CacheConfig{Dir: dir, MaxBytes: 1 << 20, MaxEntryBytes: 512 << 10, MaxEntries: 64, MaxInFlight: 2, MaxAge: time.Hour}
	c1, err := OpenCache(cfg)
	if err != nil {
		t.Fatalf("OpenCache: %v", err)
	}
	oldData := stagePayload(16<<10, 95)
	if _, err := c1.Insert("hot/a", HashBytes(oldData), oldData); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	s, err := c1.Stage("hot/a", HashBytes(stagePayload(64<<10, 96)), 64<<10)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, err := s.Write(stagePayload(64<<10, 96)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// The mount dies here: no Abort, no Commit, the handle simply goes away.
	if err := s.file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(s.tmpName); err != nil {
		t.Fatalf("the fixture must leave a staged file behind: %v", err)
	}

	c2, err := OpenCache(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	assertNoStageResidue(t, c2)
	st := c2.Stats()
	if st.InFlightBytes != 0 || st.StagedBlobs != 0 {
		t.Fatalf("reopening inherited an in-flight refresh: %+v", st)
	}
	if st.ReservedBytes != st.UsedBytes {
		t.Fatalf("at rest the two accounts must agree after a sweep: %+v", st)
	}
	if d := dirBytes(t, dir); d != st.UsedBytes {
		t.Fatalf("after the sweep the directory must hold exactly used_bytes: dir=%d used=%d", d, st.UsedBytes)
	}
	if hash, _, ok := c2.Lookup("hot/a"); !ok || hash != HashBytes(oldData) {
		t.Fatalf("the published entry must survive the sweep: %q ok=%v", hash, ok)
	}
}

// TestSuccessfulRefreshCost reports the successful-path numbers the row asks for:
// what one complete refresh publish costs, and what a read through the new
// atomic read pairing costs next to the two-call form it replaces.
func TestSuccessfulRefreshCost(t *testing.T) {
	if testing.Short() {
		t.Skip("cost measurement")
	}
	c := newStagedCache(t, 128<<20, 64<<20, 256, 2)
	const size = 8 << 20
	const rounds = 5
	payloads := make([][]byte, rounds)
	for i := range payloads {
		payloads[i] = stagePayload(size, byte(100+i))
	}
	start := time.Now()
	for i := 0; i < rounds; i++ {
		stageAndCommit(t, c, fmt.Sprintf("hot/%d", i), payloads[i])
	}
	perCommit := time.Since(start) / rounds
	t.Logf("COST successful refresh publish: %v per %d MiB blob (%.1f MiB/s) over %d publishes",
		perCommit, size>>20, float64(size)/(1<<20)/perCommit.Seconds(), rounds)

	// The read path: the atomic pairing against the two calls it replaces.
	hash, _, ok := c.Lookup("hot/0")
	if !ok {
		t.Fatalf("the published entry must be readable")
	}
	const reads = 20
	start = time.Now()
	for i := 0; i < reads; i++ {
		if _, ok := c.GetPinned("hot/0", hash); !ok {
			t.Fatalf("GetPinned must serve the published entry")
		}
		c.Unpin(hash)
	}
	pinned := time.Since(start) / reads
	start = time.Now()
	for i := 0; i < reads; i++ {
		if _, ok := c.Get("hot/0", hash); !ok {
			t.Fatalf("Get must serve the published entry")
		}
		c.Pin(hash)
		c.Unpin(hash)
	}
	twoCall := time.Since(start) / reads
	t.Logf("COST read of a %d MiB blob: GetPinned=%v, Get+Pin=%v (ratio %.2f)",
		size>>20, pinned, twoCall, float64(pinned)/float64(twoCall))
	if pinned > 10*twoCall {
		t.Fatalf("the atomic read pairing regressed the read path: %v vs %v", pinned, twoCall)
	}
	if perCommit > 5*time.Second {
		t.Fatalf("a successful publish must not cost seconds: %v", perCommit)
	}
}

// Benchmarks carry the same two costs in a form the evidence file can quote
// without a loaded box's timer noise hiding them.

func BenchmarkStagedRefreshPublish8MiB(b *testing.B) {
	c, err := OpenCache(CacheConfig{Dir: b.TempDir(), MaxBytes: 128 << 20, MaxEntryBytes: 64 << 20, MaxEntries: 256, MaxInFlight: 2, MaxAge: time.Hour})
	if err != nil {
		b.Fatalf("OpenCache: %v", err)
	}
	data := stagePayload(8<<20, 7)
	hash := HashBytes(data)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s, err := c.Stage(fmt.Sprintf("hot/%d", i), hash, int64(len(data)))
		if err != nil {
			b.Fatalf("Stage: %v", err)
		}
		if _, err := s.Write(data); err != nil {
			b.Fatalf("Write: %v", err)
		}
		if _, err := s.Commit(); err != nil {
			b.Fatalf("Commit: %v", err)
		}
	}
}

func BenchmarkGetPinned8MiB(b *testing.B) {
	benchRead8MiB(b, false)
}

func BenchmarkGetAndPin8MiB(b *testing.B) {
	benchRead8MiB(b, true)
}

func benchRead8MiB(b *testing.B, twoCalls bool) {
	c, err := OpenCache(CacheConfig{Dir: b.TempDir(), MaxBytes: 128 << 20, MaxEntryBytes: 64 << 20, MaxEntries: 256, MaxInFlight: 2, MaxAge: time.Hour})
	if err != nil {
		b.Fatalf("OpenCache: %v", err)
	}
	data := stagePayload(8<<20, 8)
	hash := HashBytes(data)
	if _, err := c.Insert("hot/a", hash, data); err != nil {
		b.Fatalf("insert: %v", err)
	}
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if twoCalls {
			if _, ok := c.Get("hot/a", hash); !ok {
				b.Fatalf("Get failed")
			}
			c.Pin(hash)
		} else {
			if _, ok := c.GetPinned("hot/a", hash); !ok {
				b.Fatalf("GetPinned failed")
			}
		}
		c.Unpin(hash)
	}
}
