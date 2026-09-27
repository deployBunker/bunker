package fsmount

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/davserve"
	"github.com/deployBunker/bunker/internal/fsclient"
)

// ---------------------------------------------------------------------------
// BFS-045: the bound the live READ PATH enforces must be counted where it is
// enforced.
//
// BFS-032 measured the opposite: fs_linux.go pre-filtered `len(data) <=
// MaxEntryBytes` before calling Cache.Insert, so Insert's own oversize branch
// (cache.go:312 then) was unreachable from a live read and `oversize_bypasses`
// stayed 0 whatever the client did. A counter that can never move is a gap, not
// a green check — so the arm below drives the REAL read path (readHandle.Read,
// the same entry the FUSE kernel calls) and asserts the census MOVES, that the
// read still SUCCEEDS, and that the reason is named.
// ---------------------------------------------------------------------------

// testMountWithEntryCap builds the mount's own pieces around a real davserve
// endpoint, with the per-entry cap as the one variable this test needs. No FUSE
// mount is involved: readHandle is the live read path with or without a kernel.
func testMountWithEntryCap(t *testing.T, body string, entryCap int64) (*Mount, string) {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	srv, err := davserve.Serve(root, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("davserve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	c, err := fsclient.NewClient(fsclient.Options{
		BaseURL: srv.URL, Concurrency: 4, OpTimeout: 10 * time.Second, BindTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	cache, err := fsclient.OpenCache(fsclient.CacheConfig{
		Dir: t.TempDir(), MaxBytes: fsclient.DefaultCacheMaxBytes,
		MaxEntryBytes: entryCap, MaxAge: time.Hour,
	})
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	m := &Mount{
		opts:    Options{BaseURL: srv.URL, Concurrency: 4},
		logf:    t.Logf,
		client:  c,
		cache:   cache,
		wp:      fsclient.NewWritePath(c, cache, t.TempDir(), fsclient.OnConflictRefuse),
		inv:     fsclient.NewInvalidator(c, fsclient.InvalidateOptions{Mode: fsclient.ModePoll}),
		bounds:  newBoundRegistry(),
		reads:   map[uint64]*readHandle{},
		writes:  map[uint64]*writeHandle{},
		bufDocs: map[uint64]int64{},
		verdict: "healthy",
	}
	m.snap.Store(fsclient.NewSnapshot(""))
	return m, target
}

// TestAnOversizeReadThroughTheLivePathCountsTheBypassWithItsReason is BFS-032's
// acceptance from the reporting side: an oversize read performed through the
// mount MOVES the counter, and the record says which decision refused it.
func TestAnOversizeReadThroughTheLivePathCountsTheBypassWithItsReason(t *testing.T) {
	const overCap = "X-REPLACEMENT-CONTENT-0123456789-abcdefghijklmnopqrstuvwxyz-ABCDEFGHIJKLMNOPQRSTUV"
	// A 16-byte cap and an 82-byte file: the read is over the cap by 5x.
	m, _ := testMountWithEntryCap(t, overCap, 16)
	before := m.cache.Stats()
	if before.OversizeBypasses != 0 || before.BypassReasons[fsclient.BypassReasonOverEntryCap] != 0 {
		t.Fatalf("the fixture must start with an empty census: oversize=%d reason=%d",
			before.OversizeBypasses, before.BypassReasons[fsclient.BypassReasonOverEntryCap])
	}

	// The kernel's stat, then the read — the same order a real `cat` produces.
	if got := statThroughMount(t, m, "target.txt"); got != uint64(len(overCap)) {
		t.Fatalf("stat: size=%d, want %d", got, len(overCap))
	}
	n, errno := readThroughMount(t, m, "target.txt", 4096)
	if errno != 0 {
		t.Fatalf("an over-cap read must still SUCCEED (the cap decides what is cached, never what is served): errno=%v", errno)
	}
	if n != len(overCap) {
		t.Fatalf("the read served %d bytes, want the whole %d", n, len(overCap))
	}

	after := m.cache.Stats()
	if after.OversizeBypasses != 1 {
		t.Fatalf("the live read path did not count the oversize bypass: oversize_bypasses=%d, want 1 (BFS-032: a counter the live path cannot reach is a gap)", after.OversizeBypasses)
	}
	if got := after.BypassReasons[fsclient.BypassReasonOverEntryCap]; got != 1 {
		t.Fatalf("the bypass was not counted BY REASON: over_entry_cap=%d, want 1 (census=%v)", got, after.BypassReasons)
	}
	if got := after.BypassReasons[fsclient.BypassReasonInsertOverEntryCap]; got != 0 {
		t.Fatalf("the READ path's refusal was attributed to Insert's own branch (%d): the two sites must stay countable apart", got)
	}
	// Nothing was cached, so the directory holds no blob for the path.
	if _, _, ok := m.cache.Lookup("target.txt"); ok {
		t.Fatal("an over-cap read must leave the path uncached")
	}

	// The reason is in the reported census, and the census is the closed
	// vocabulary: a reason the code cannot reach is visibly missing from it.
	for _, r := range fsclient.BypassReasons() {
		if _, present := after.BypassReasons[r]; !present {
			t.Fatalf("the census is missing reason %q: %v", r, after.BypassReasons)
		}
	}
	t.Logf("live over-cap read: oversize_bypasses=%d after=%d census=%v",
		before.OversizeBypasses, after.OversizeBypasses, after.BypassReasons)
}

// TestAnUnderCapReadIsCachedAndCountsNoBypass is the control in the other
// direction: the same live path with a cap the file fits must store it and must
// NOT move the oversize census — otherwise the arm above could pass on a counter
// that just counts every read.
func TestAnUnderCapReadIsCachedAndCountsNoBypass(t *testing.T) {
	const small = "SMALL"
	m, _ := testMountWithEntryCap(t, small, 64)
	if got := statThroughMount(t, m, "target.txt"); got != uint64(len(small)) {
		t.Fatalf("stat: size=%d, want %d", got, len(small))
	}
	n, errno := readThroughMount(t, m, "target.txt", 4096)
	if errno != 0 || n != len(small) {
		t.Fatalf("read: n=%d errno=%v", n, errno)
	}
	after := m.cache.Stats()
	if after.OversizeBypasses != 0 {
		t.Fatalf("a read that fits the cap moved the oversize census: %d", after.OversizeBypasses)
	}
	if _, _, ok := m.cache.Lookup("target.txt"); !ok {
		t.Fatal("a read that fits the cap must be cached")
	}
}

// TestTheMountStatusCarriesTheRefreshBlockAndTheAbsentQueueReason ties the two
// halves together at the mount boundary: the document a person reads has the
// staged window's real figures and the hot queue's absence NAMED.
func TestTheMountStatusCarriesTheRefreshBlockAndTheAbsentQueueReason(t *testing.T) {
	m, _ := testMountWithEntryCap(t, "hello", 64)
	st := m.Status()
	if st.Invalidation.Refresh.MaxInFlight != fsclient.DefaultCacheMaxInFlight {
		t.Fatalf("the mounted record must report the in-flight bound: %d", st.Invalidation.Refresh.MaxInFlight)
	}
	if st.Invalidation.Refresh.QueueDepth != nil || st.Invalidation.Refresh.QueueRefusedFullTotal != nil {
		t.Fatal("the hot-refresh queue is not in this build: its figures must be absent, not zero")
	}
	if !strings.HasPrefix(st.Invalidation.Refresh.AbsentReason, fsclient.ReasonNotPublished) {
		t.Fatalf("the absent queue must carry a reason from the vocabulary: %q", st.Invalidation.Refresh.AbsentReason)
	}
	// The content-age figure is present as a REASON (nothing has been observed
	// yet) rather than as a zero age, which would read as "perfectly current".
	if st.Invalidation.ContentAge != nil {
		t.Fatalf("a mount that never observed the tree must not report an age: %+v", st.Invalidation.ContentAge)
	}
	if !strings.HasPrefix(st.Invalidation.ContentAgeReason, fsclient.ReasonNoSample) {
		t.Fatalf("the absent content age must carry a reason: %q", st.Invalidation.ContentAgeReason)
	}
}
