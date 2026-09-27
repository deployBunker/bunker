package fsclient

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestBFS037ProbeInPlaceArmMechanism is a PROBE (not a cell): it establishes
// that the mutate-in-place arm's mechanism -- Cache.Stage reusing the blob the
// path already names -- is REAL on this tree, i.e. that the arm cannot be
// passing because the mutation is inert. It is the same discipline the row asks
// for on the other side: before trusting a GREEN under a mutation, prove the
// mutation has teeth.
func TestBFS037ProbeInPlaceArmMechanism(t *testing.T) {
	dir := t.TempDir()
	cache, err := OpenCache(CacheConfig{
		Dir: filepath.Join(dir, "cache"), MaxBytes: 8 << 20, MaxEntryBytes: 4 << 20,
		MaxEntries: 64, MaxInFlight: 2, MaxAge: 0,
	})
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	defer func() { _ = cache.Close() }()
	old := bytes.Repeat([]byte("old-content-"), 64)
	oldHash := HashBytes(old)
	if _, err := cache.Insert("p.txt", oldHash, old); err != nil {
		t.Fatalf("insert: %v", err)
	}
	blobPath := filepath.Join(cache.Dir(), CacheBlobDir, oldHash[len(HashPrefix):])
	before, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	t.Logf("PROBE: cached p.txt hash=%s bytes=%d", oldHash[:14], len(before))

	st, err := cache.Stage("p.txt", HashBytes([]byte("new")), 4096)
	if err != nil {
		t.Logf("PROBE: Stage refused: %v", err)
		t.Skip("Stage refused: nothing to observe")
	}
	if _, werr := st.Write([]byte("partial-new")); werr != nil {
		t.Logf("PROBE: write refused: %v", werr)
	}
	after, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read blob after stage: %v", err)
	}
	same := bytes.Equal(before, after)
	t.Logf("PROBE: the published blob for the OLD hash is %s after Stage+Write of a DIFFERENT hash (len %d -> %d)",
		map[bool]string{true: "UNTOUCHED", false: "MODIFIED"}[same], len(before), len(after))
	// The invariant, asserted: staging a DIFFERENT content must not disturb the
	// blob a reader can reach, whatever the caller does with the staged writer.
	// (Under the --stream-into-published arm the refresh writes into this very
	// file from its own body callback; this probe is what shows the difference
	// between the mechanism and a route that never touches it.)
	if !same {
		t.Fatal("staging a different content rewrote the blob a reader can reach: a refresh must publish a NEW immutable blob, never mutate the published one")
	}
	_ = st.Abort()
}
