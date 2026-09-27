//go:build linux

package fsmount

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/deployBunker/bunker/internal/davserve"
	"github.com/deployBunker/bunker/internal/fsclient"
)

// The handler-level arms for BFS-025. They drive the SAME objects the mount wires
// together — a real client against the landed surface, the cache, the snapshot
// and the bound registry — without a kernel in the middle, so the RULE can be
// asserted exactly (a refusal rather than a fragment) and the recoverability can
// be proven step by step.
//
// The end-to-end arms (probes/bfs025-truncated-read) are the ones that go RED on
// the unfixed tree, because the fragment is produced by the KERNEL's clamp at
// i_size and is invisible to a handler-level test: before the fix the handler
// returned every byte it held and the kernel threw the tail away. These tests pin
// the fix's rule; they do not stand in for the live arm.

const (
	boundTestShort = "SHORT"                                                                              // 5 bytes
	boundTestLong  = "X-REPLACEMENT-CONTENT-0123456789-abcdefghijklmnopqrstuvwxyz-ABCDEFGHIJKLMNOPQRSTUV" // 82 bytes
)

// testMount wires the mount's own pieces around a davserve endpoint and a fixture
// tree, with no FUSE mount: nothing here needs /dev/fuse, and nothing here may
// reach the real user cache (the cache dir is a t.TempDir).
func testMount(t *testing.T, body string) (*Mount, string) {
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
		MaxEntryBytes: fsclient.DefaultCacheMaxEntryBytes, MaxAge: time.Hour,
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

// statThroughMount performs what the kernel does before a read: one attrs reply
// for the path, from the real Getattr, which is also where the published bound is
// recorded.
func statThroughMount(t *testing.T, m *Mount, p string) uint64 {
	t.Helper()
	var out fuse.AttrOut
	nd := &node{m: m, p: p}
	if errno := nd.Getattr(context.Background(), nil, &out); errno != 0 {
		t.Fatalf("Getattr %s: errno=%v", p, errno)
	}
	return out.Attr.Size
}

func readThroughMount(t *testing.T, m *Mount, p string, dest int) (int, syscall.Errno) {
	t.Helper()
	h := &readHandle{m: m, p: p, fh: 1}
	res, errno := h.Read(context.Background(), make([]byte, dest), 0)
	if errno != 0 {
		return 0, errno
	}
	if res == nil {
		return 0, 0
	}
	return res.Size(), 0
}

// TestReadRefusesAFragmentOfNewerContent is the driver's reproduction at the
// handler level: a path that was only STAT'ed, then replaced with longer content
// on the agent. The kernel bounds the read at the stale published size, so the
// bytes may not be served at all.
func TestReadRefusesAFragmentOfNewerContent(t *testing.T) {
	m, target := testMount(t, boundTestShort)

	if got := statThroughMount(t, m, "target.txt"); got != uint64(len(boundTestShort)) {
		t.Fatalf("the first stat: size=%d, want %d", got, len(boundTestShort))
	}
	// The agent replaces the file out of band, with longer content.
	if err := os.WriteFile(target, []byte(boundTestLong), 0o644); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := statThroughMount(t, m, "target.txt"); got != uint64(len(boundTestShort)) {
		t.Fatalf("the post-replacement stat must still be the stale size (the state under test): got %d, want %d", got, len(boundTestShort))
	}

	// THE ARM. A fragment with rc=0 is the one outcome that is not allowed.
	n, errno := readThroughMount(t, m, "target.txt", 4096)
	if errno != syscall.ESTALE {
		t.Fatalf("want a loud ESTALE refusal, got errno=%v with %d bytes", errno, n)
	}
	if n != 0 {
		t.Fatalf("a refusal must carry no bytes, got %d", n)
	}

	// The refusal corrected the mount's metadata from the server's own answer, so
	// the next attrs reply publishes the truth and the retry succeeds.
	if got := statThroughMount(t, m, "target.txt"); got != uint64(len(boundTestLong)) {
		t.Fatalf("after the refusal the stat must report the live size: got %d, want %d", got, len(boundTestLong))
	}
	nd, errno := readThroughMount(t, m, "target.txt", 4096)
	if errno != 0 {
		t.Fatalf("the retry must succeed, got errno=%v", errno)
	}
	if nd != len(boundTestLong) {
		t.Fatalf("the retry must return every byte: got %d, want %d", nd, len(boundTestLong))
	}

	// And it is reported, not silent.
	if got := m.Status().ReadBound.RefusalsTotal; got < 1 {
		t.Fatalf("the refusal must be counted on the owner-facing surface, got %d", got)
	}
	if last := m.Status().ReadBound.Last; !strings.Contains(last, "target.txt") {
		t.Fatalf("the status document must name the path that diverged, got %q", last)
	}
}

// TestReadServesAndCorrectsWhenTheBoundIsLarger is the mirror image: the
// published size is LARGER than the live content. Every byte the resource has
// reaches the reader (a true EOF at end-of-content), nothing fails, and the
// metadata is corrected rather than left wrong.
func TestReadServesAndCorrectsWhenTheBoundIsLarger(t *testing.T) {
	m, target := testMount(t, boundTestLong)
	if got := statThroughMount(t, m, "target.txt"); got != uint64(len(boundTestLong)) {
		t.Fatalf("the stat: size=%d, want %d", got, len(boundTestLong))
	}
	if err := os.WriteFile(target, []byte(boundTestShort), 0o644); err != nil {
		t.Fatalf("replace: %v", err)
	}
	n, errno := readThroughMount(t, m, "target.txt", 4096)
	if errno != 0 {
		t.Fatalf("a shorter content must be served, not refused: errno=%v", errno)
	}
	if n != len(boundTestShort) {
		t.Fatalf("served %d bytes, want every byte the resource has (%d)", n, len(boundTestShort))
	}
	if got := statThroughMount(t, m, "target.txt"); got != uint64(len(boundTestShort)) {
		t.Fatalf("the metadata must be corrected to the served content: stat=%d, want %d", got, len(boundTestShort))
	}
	if got := m.Status().ReadBound.CorrectionsTotal; got < 1 {
		t.Fatalf("the correction must be counted, got %d", got)
	}
	if refusals := m.Status().ReadBound.RefusalsTotal; refusals != 0 {
		t.Fatalf("the mirror case must not refuse anything, got %d refusals", refusals)
	}
}

// TestReadDistrustsACacheEntryLongerThanTheBound: what the client HOLDS
// contradicting the bound is not served either — the entry is dropped and the
// read goes to the server, which is the "re-fetch" half of the rule.
func TestReadDistrustsACacheEntryLongerThanTheBound(t *testing.T) {
	m, _ := testMount(t, boundTestLong)
	if got := statThroughMount(t, m, "target.txt"); got != uint64(len(boundTestLong)) {
		t.Fatalf("the stat: size=%d, want %d", got, len(boundTestLong))
	}
	// A cache entry for the path, as a previous read would have left it.
	hash := fsclient.HashBytes([]byte(boundTestLong))
	if _, err := m.cache.Insert("target.txt", hash, []byte(boundTestLong)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// The kernel is told a SMALLER size than what the client holds (a live
	// stat of a shortened file would do exactly this).
	m.bounds.Published("target.txt", int64(len(boundTestShort)))

	h := &readHandle{m: m, p: "target.txt", fh: 1}
	if !h.distrustCached(int64(len(boundTestLong)), hash) {
		t.Fatal("an entry longer than the kernel's bound must be distrusted")
	}
	if _, _, ok := m.cache.Lookup("target.txt"); ok {
		t.Fatal("the distrusted entry must be dropped")
	}
	// The re-fetch is the live content (82 bytes) and the kernel still holds the
	// stale bound (5), so the read is REFUSED rather than served — re-fetching
	// does not make an untrustworthy bound trustworthy.
	if n, errno := readThroughMount(t, m, "target.txt", 4096); errno != syscall.ESTALE || n != 0 {
		t.Fatalf("the re-fetch must still refuse while the bound is stale: errno=%v bytes=%d", errno, n)
	}
	// And the correction the refusal made is what makes the retry work: a fresh
	// attrs reply publishes the live size, after which the read is served.
	if got := statThroughMount(t, m, "target.txt"); got != uint64(len(boundTestLong)) {
		t.Fatalf("the corrected stat: size=%d, want %d", got, len(boundTestLong))
	}
	if n, errno := readThroughMount(t, m, "target.txt", 4096); errno != 0 || n != len(boundTestLong) {
		t.Fatalf("the retry must serve the content: errno=%v bytes=%d", errno, n)
	}
}

// TestWriteRaisesTheBoundSoReadAfterWriteIsNotRefused: the kernel grows i_size to
// at least the end of every write it acknowledges, so a read after a write
// through the mount is judged against the grown size — not the size the file had
// when it was opened.
func TestWriteRaisesTheBoundSoReadAfterWriteIsNotRefused(t *testing.T) {
	m, _ := testMount(t, "")
	// Create replies attrs for a new file: size 0.
	m.notePublished(fsclient.Node{Path: "new.txt", Size: 0})
	m.bounds.Wrote("new.txt", 100) // a 100-byte write the kernel acknowledged
	if got, _ := m.bounds.Bound("new.txt"); got != 100 {
		t.Fatalf("the bound after a write: %d, want 100", got)
	}
	if v := judgeServed(100, true, 100); v != servedAgrees {
		t.Fatalf("a read of the written bytes must be served: verdict=%v", v)
	}
}

// TestCacheHitBytesAreUnchangedByTheGuard is the control that keeps this row out
// of BFS-024: when the length the client holds AGREES with the bound, the guard
// does not touch the entry, so whatever the cache holds is still what a reader
// receives — stale bytes included. BFS-024 (a path already read keeps serving
// pre-edit bytes) is a filed row with its own acceptance, and this change neither
// fixes nor deepens it.
func TestCacheHitBytesAreUnchangedByTheGuard(t *testing.T) {
	m, target := testMount(t, boundTestShort)
	if got := statThroughMount(t, m, "target.txt"); got != uint64(len(boundTestShort)) {
		t.Fatalf("the stat: size=%d", got)
	}
	// A read that fills the cache, exactly as the live arm does.
	h := &readHandle{m: m, p: "target.txt", fh: 1}
	res, errno := h.Read(context.Background(), make([]byte, 4096), 0)
	if errno != 0 || res == nil || res.Size() != len(boundTestShort) {
		t.Fatalf("the first read: errno=%v size=%d", errno, res.Size())
	}
	// The agent replaces the content with a LONGER one; the bound is unchanged
	// (nothing has revalidated), so the cached entry is not distrusted.
	if err := os.WriteFile(target, []byte(boundTestLong), 0o644); err != nil {
		t.Fatalf("replace: %v", err)
	}
	h2 := &readHandle{m: m, p: "target.txt", fh: 2}
	res2, errno2 := h2.Read(context.Background(), make([]byte, 4096), 0)
	if errno2 != 0 {
		t.Fatalf("a cache hit whose length agrees with the bound must be served, got errno=%v", errno2)
	}
	if res2.Size() != len(boundTestShort) {
		t.Fatalf("the cache hit served %d bytes; the guard must not change which bytes a hit serves (BFS-024 is separate)", res2.Size())
	}
	if refusals := m.Status().ReadBound.RefusalsTotal; refusals != 0 {
		t.Fatalf("no refusal belongs to this arm, got %d", refusals)
	}
}
