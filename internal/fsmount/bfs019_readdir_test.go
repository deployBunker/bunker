//go:build linux

package fsmount

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/deployBunker/bunker/internal/davserve"
	"github.com/deployBunker/bunker/internal/fsclient"
)

// BFS-019 THROUGH THE MOUNT, at the handler level: a directory listing that comes
// back empty (or short) while the directory has entries.
//
// THE DEFECT, as filed: a fresh mount lists the root correctly; after ONE `mkdir`
// through the mount the same listing comes back EMPTY while all the entries still
// exist on the server; after a through-mount write + unlink the listing comes
// back with ONE name while the server holds eight. `find <mount> -mindepth 1`
// answers 0 against the server's 136 and `ls -lR` prints 2 lines against 143 —
// with rc 0 throughout. Lookups keep working in the same instant (`stat`, `cat`),
// and a collection nobody mutated lists 120/120: per-directory corruption of the
// cached child set, not a dead mount.
//
// WHAT THESE CELLS DRIVE: the REAL `Readdir` handler (the production read half,
// answered in-process — no kernel needed) and the production mutation halves the
// kernel's syscalls reach — `beginMkdir`, `beginCreate` + publish, `Unlink`,
// `Rmdir`, `Rename`. Every assertion is a SIDE-BY-SIDE against the served tree's
// own `os.ReadDir`, so no cell can pass on a hardcoded expectation the server
// disagrees with. The live kernel arms and the mutation/attribution harness are
// docs/evidence/BFS-019-arms.sh.
//
// The default arm on the unfixed tree is
// TestBFS019OneMkdirDoesNotEmptyItsParentsListing (an EMPTY listing) and
// TestBFS019ACreateVisitAndUnlinkLeaveTheListingCorrect (a SHORT one, after the
// unlink: only the names this mount created since the invalidation).

// bfs019ServedDir is the served tree the cells mount: the row's shape — six root
// entries, a nested collection, and a collection with many children, so an EMPTY
// answer and a SHORT answer are both visible.
func bfs019ServedDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"pkg", "scratch", "src"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"CHANGELOG.md": "c\n", "README.md": "r\n", "go.mod": "m\n",
		"pkg/one.txt": "1\n", "scratch/two.txt": "2\n",
	}
	for i := 0; i < 12; i++ {
		files[filepath.Join("src", "f"+string(rune('a'+i))+".txt")] = "x\n"
	}
	for p, body := range files {
		if err := os.WriteFile(filepath.Join(root, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// bfs019Mount wires the mount's own pieces around a davserve endpoint over the
// row's tree — a real client against the landed surface, the cache, the write
// path, the snapshot — with no FUSE mount, and takes the one whole-tree
// observation a bind takes.
func bfs019Mount(t *testing.T) (*Mount, string) {
	t.Helper()
	root := bfs019ServedDir(t)
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
	if err := m.refreshSnapshot(context.Background(), ""); err != nil {
		t.Fatalf("refreshSnapshot: %v", err)
	}
	return m, root
}

// bfs019ListingThroughMount is one READDIRPLUS, through the REAL handler.
func bfs019ListingThroughMount(t *testing.T, m *Mount, dir string) []string {
	t.Helper()
	n := &node{m: m, p: dir}
	ds, errno := n.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("Readdir(%q): errno=%v", dir, errno)
	}
	var names []string
	for ds.HasNext() {
		e, errno := ds.Next()
		if errno != 0 {
			t.Fatalf("Readdir(%q).Next: errno=%v", dir, errno)
		}
		names = append(names, e.Name)
	}
	sort.Strings(names)
	return names
}

// bfs019ListingOnTheServer is the NATIVE CONTROL: the served tree's own listing
// for the same directory, taken from the filesystem the server serves.
func bfs019ListingOnTheServer(t *testing.T, root, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		t.Fatalf("ReadDir(%q) on the served tree: %v", dir, err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func bfs019Same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// assertListingMatchesServer is the side-by-side: the mount's count AND names
// against the server's, in one message.
func assertListingMatchesServer(t *testing.T, m *Mount, root, dir, step string) {
	t.Helper()
	got := bfs019ListingThroughMount(t, m, dir)
	want := bfs019ListingOnTheServer(t, root, dir)
	if !bfs019Same(got, want) {
		t.Fatalf("BFS-019 [%s] readdir of %q through the mount answers %d names %v, while the served tree holds %d names %v — rc was 0 and nothing reported a fault",
			step, dir, len(got), got, len(want), want)
	}
}

// TestBFS019OneMkdirDoesNotEmptyItsParentsListing is THE cell: the filed repro's
// first step, through the real handler, side by side with the server.
func TestBFS019OneMkdirDoesNotEmptyItsParentsListing(t *testing.T) {
	m, root := bfs019Mount(t)
	assertListingMatchesServer(t, m, root, "", "fresh")

	if _, errno := m.beginMkdir(context.Background(), "oob-dir", 0o755); errno != 0 {
		t.Fatalf("mkdir through the mount: errno=%v", errno)
	}
	if fi, err := os.Stat(filepath.Join(root, "oob-dir")); err != nil || !fi.IsDir() {
		t.Fatalf("the collection must exist on the served tree: %v", err)
	}
	assertListingMatchesServer(t, m, root, "", "after one mkdir")
}

// TestBFS019ACreateVisitAndUnlinkLeaveTheListingCorrect is the filed repro's
// second step: the write path's own halves (create, publish at the close, unlink)
// each invalidate the collection they happened in — and the listing must come
// back correct after EACH, not only after all three.
func TestBFS019ACreateVisitAndUnlinkLeaveTheListingCorrect(t *testing.T) {
	m, root := bfs019Mount(t)
	h := createThroughMount(t, m, "written.txt")
	writeThroughMount(t, m, h, []byte("hello\n"), 0)
	// The pre-publication view is the mount's own optimistic one (BFS-020: the
	// name is on the server before close(2) returns, and the kernel already holds
	// it) — so the side-by-side starts at the publication point, which is the
	// instant the two are supposed to agree.
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("close (Flush, the publication point): errno=%v", errno)
	}
	assertListingMatchesServer(t, m, root, "", "after the create + write + close")
	if errno := (&node{m: m, p: ""}).Unlink(context.Background(), "written.txt"); errno != 0 {
		t.Fatalf("unlink through the mount: errno=%v", errno)
	}
	assertListingMatchesServer(t, m, root, "", "after the unlink")
}

// TestBFS019ARepeatedlyMutatedDirectoryStaysListable: it takes ONE mkdir to
// poison a directory, so a directory mutated three times and then the source and
// destination of a rename must survive all of it.
func TestBFS019ARepeatedlyMutatedDirectoryStaysListable(t *testing.T) {
	m, root := bfs019Mount(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		name := "repeat-" + string(rune('0'+i))
		if _, errno := m.beginMkdir(ctx, name, 0o755); errno != 0 {
			t.Fatalf("mkdir %s: errno=%v", name, errno)
		}
		assertListingMatchesServer(t, m, root, "", "after mkdir #"+string(rune('0'+i)))
	}
	if errno := renameThroughMount(t, m, "pkg/one.txt", "scratch/one.txt"); errno != 0 {
		t.Fatalf("rename across two collections: errno=%v", errno)
	}
	assertListingMatchesServer(t, m, root, "pkg", "the rename's source collection")
	assertListingMatchesServer(t, m, root, "scratch", "the rename's destination collection")
	assertListingMatchesServer(t, m, root, "", "after the rename")
}

// TestBFS019ANestedCollectionIsListedCorrectly: the same pair one level down —
// the collection that changed is `pkg`, not the root, and the root must not lose
// anything either.
func TestBFS019ANestedCollectionIsListedCorrectly(t *testing.T) {
	m, root := bfs019Mount(t)
	if _, errno := m.beginMkdir(context.Background(), "pkg/inner", 0o755); errno != 0 {
		t.Fatalf("mkdir pkg/inner: errno=%v", errno)
	}
	assertListingMatchesServer(t, m, root, "pkg", "the mutated collection")
	assertListingMatchesServer(t, m, root, "", "the root above it")
	assertListingMatchesServer(t, m, root, "src", "a collection nobody touched")
	h := createThroughMount(t, m, "pkg/inner/deep.txt")
	writeThroughMount(t, m, h, []byte("deep\n"), 0)
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("close of the nested create: errno=%v", errno)
	}
	assertListingMatchesServer(t, m, root, "pkg/inner", "a write inside the nested collection")
	assertListingMatchesServer(t, m, root, "pkg", "its parent")
}

// TestBFS019AnInvalidationDropDoesNotLeaveTheListingShort is the invalidation
// half, driven through the code the invalidator actually calls
// (`Mount.dropPaths`, the `OnDrop` callback) with the paths a watched surface was
// MEASURED to carry: an event for a change to one name names that name, and an
// event for a new name names the changed COLLECTIONS as well — `"."` for the root
// and `"src"` for the collection the new file appeared in. A drop that emptied a
// name out of a directory's index while leaving the directory "read" served a
// SHORT listing: a name that still existed on the server, silently absent, rc 0.
func TestBFS019AnInvalidationDropDoesNotLeaveTheListingShort(t *testing.T) {
	m, root := bfs019Mount(t)

	// A FOREIGN writer's append to an existing file inside src — the event the
	// surface carries for it is paths=["src/fa.txt"].
	m.dropPaths([]string{"src/fa.txt"}, false)
	assertListingMatchesServer(t, m, root, "src", "after an invalidate event naming one changed name")

	// A foreign new name: the surface's own event names the collections too.
	m.dropPaths([]string{".", "server-made.txt", "src", "src/server-made.txt"}, false)
	assertListingMatchesServer(t, m, root, "", "after an invalidate event naming the changed collections")
	assertListingMatchesServer(t, m, root, "src", "the collection the event named")
	assertListingMatchesServer(t, m, root, "pkg", "a collection the event did not name")
}

// TestBFS019AttributionLookupsSurviveTheMutation is the ATTRIBUTION cell: under
// the mutations above a path already in the tree must STILL answer `stat` — the
// row's own evidence is that lookups never broke. It must stay GREEN under the
// BFS-019 mutation (docs/evidence/BFS-019-arms.sh mutations), so a single
// mutation cannot blanket-red the suite.
func TestBFS019AttributionLookupsSurviveEveryMutation(t *testing.T) {
	m, root := bfs019Mount(t)
	ctx := context.Background()
	if _, errno := m.beginMkdir(ctx, "oob-dir", 0o755); errno != 0 {
		t.Fatalf("mkdir: errno=%v", errno)
	}
	if errno := (&node{m: m, p: ""}).Unlink(ctx, "go.mod"); errno != 0 {
		t.Fatalf("unlink go.mod: errno=%v", errno)
	}
	for _, p := range []string{"README.md", "CHANGELOG.md", "pkg/one.txt", "src/fa.txt"} {
		var out fuse.AttrOut
		if errno := (&node{m: m, p: p}).Getattr(ctx, nil, &out); errno != 0 {
			t.Fatalf("attribution: stat of %q must still answer after the listing was invalidated (errno=%v)", p, errno)
		}
		want, err := os.Stat(filepath.Join(root, p))
		if err != nil {
			t.Fatalf("the served tree must hold %q: %v", p, err)
		}
		if out.Attr.Size != uint64(want.Size()) {
			t.Fatalf("attribution: stat %q answered size=%d, the served tree holds %d", p, out.Attr.Size, want.Size())
		}
	}
}
