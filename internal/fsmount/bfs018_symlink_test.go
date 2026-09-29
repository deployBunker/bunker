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

// BFS-018 THROUGH THE MOUNT, at the handler level: the same objects a live
// mount wires together (a real client against the landed surface, the cache,
// the snapshot, the handle tables), so the RULES can be asserted exactly.
//
// THE DEFECT, as filed: `ls -l` through the mount reported
//
//	-rwxrwxrwx 1 kara kara 5 ... link-to-a
//
// a REGULAR FILE, mode 0777, size 5 — 5 being the length of the target path
// "a.txt". The link's target had been materialised as file content, which is
// neither the link nor its 26-byte target.
//
// These cells pin the fixed behaviour and its refusal halves; the live arms
// that exercise the real kernel path (mount, git checkout, git status) are in
// docs/evidence/BFS-018-arms.sh.

const (
	bfs018TargetName = "a.txt"
	bfs018TargetBody = "hello from the source tree"
	bfs018LinkName   = "link-to-a"
)

// symlinkMount serves a tree containing a 26-byte file and a link to it, and
// returns the mount plus the path of the SERVED tree (so a cell can assert what
// the server holds, which is the only thing that decides whether a tree was
// corrupted).
func symlinkMount(t *testing.T) (*Mount, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, bfs018TargetName), []byte(bfs018TargetBody), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(bfs018TargetName, filepath.Join(root, bfs018LinkName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
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
	info, oerr := c.Handshake(context.Background())
	if oerr != nil {
		t.Fatalf("handshake: %v", oerr)
	}
	c.PinTree(info.Tree)
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
	return m, root
}

// bareMount builds a Mount wired to an existing client and the rest of a live
// mount's state, without a kernel bridge. It is what a cell needs to drive the
// node methods the way FUSE does.
func bareMount(c *fsclient.Client, like *Mount) *Mount {
	m := &Mount{
		opts: like.opts, logf: like.logf, client: c, cache: like.cache, wp: like.wp,
		inv: like.inv, bounds: newBoundRegistry(), reads: map[uint64]*readHandle{},
		writes: map[uint64]*writeHandle{}, bufDocs: map[uint64]int64{}, verdict: "healthy",
	}
	m.snap.Store(fsclient.NewSnapshot(""))
	return m
}

func servedKind(t *testing.T, path string) (string, string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		return "absent", ""
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(path)
		return "symlink", target
	}
	if fi.IsDir() {
		return "dir", ""
	}
	return "file", ""
}

// F1 — the TYPE through the mount: S_IFLNK, size = the target path's length, and
// never a regular file.
func TestSymlinkThroughTheMountIsALinkNotAFile(t *testing.T) {
	m, _ := symlinkMount(t)
	var out fuse.AttrOut
	n := &node{m: m, p: bfs018LinkName}
	if errno := n.Getattr(context.Background(), nil, &out); errno != 0 {
		t.Fatalf("Getattr(%s): errno=%v", bfs018LinkName, errno)
	}
	switch typ := out.Mode & syscall.S_IFMT; typ {
	case syscall.S_IFLNK:
	case syscall.S_IFREG:
		t.Fatalf("the mount reports the link as a REGULAR FILE (mode %#o, size %d) — the defect as filed",
			out.Mode, out.Size)
	default:
		t.Fatalf("mode type = %#o, want S_IFLNK", typ)
	}
	if out.Mode&0o777 != 0o777 {
		t.Fatalf("a symlink's permission bits are 0777 by convention, got %#o", out.Mode&0o777)
	}
	if out.Size != uint64(len(bfs018TargetName)) {
		t.Fatalf("the link's size = %d, want %d (its own target path), not %d (the target's content)",
			out.Size, len(bfs018TargetName), len(bfs018TargetBody))
	}
	if out.Size == uint64(len(bfs018TargetBody)) {
		t.Fatalf("the link was DEREFERENCED: its size is the target's content length")
	}
}

// F2 — Readlink answers the target from the DECLARED metadata, costing ZERO
// requests when the node tree already holds it, and one named PROPFIND when it
// does not.
func TestReadlinkAnswersTheDeclaredTargetWithoutFetchingBytes(t *testing.T) {
	m, _ := symlinkMount(t)
	if err := m.refreshSnapshot(context.Background(), ""); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	before := m.client.Requests()
	got, errno := (&node{m: m, p: bfs018LinkName}).Readlink(context.Background())
	if errno != 0 {
		t.Fatalf("Readlink: errno=%v", errno)
	}
	if string(got) != bfs018TargetName {
		t.Fatalf("Readlink = %q, want %q", got, bfs018TargetName)
	}
	if size := m.client.Requests() - before; size != 0 {
		t.Fatalf("Readlink cost %d request(s): the target was already in the node tree", size)
	}
	// The live arm: a path the node tree does NOT hold costs exactly ONE
	// PROPFIND (named, not hidden).
	fresh := &Mount{client: m.client, logf: t.Logf, cache: m.cache, wp: m.wp, inv: m.inv,
		bounds: newBoundRegistry(), reads: map[uint64]*readHandle{}, writes: map[uint64]*writeHandle{},
		bufDocs: map[uint64]int64{}, verdict: "healthy"}
	fresh.snap.Store(fsclient.NewSnapshot(""))
	before = m.client.Requests()
	if _, errno := (&node{m: fresh, p: bfs018LinkName}).Readlink(context.Background()); errno != 0 {
		t.Fatalf("Readlink (live lookup): errno=%v", errno)
	}
	if n := m.client.Requests() - before; n != 1 {
		t.Fatalf("the live Readlink cost %d request(s), want exactly 1 (PROPFIND Depth: 0)", n)
	}
}

// F3 — readdir hands the kernel the TYPE, not just the name: a directory entry
// whose mode says S_IFREG is how `ls` came to print a file.
func TestReaddirReportsTheLinkType(t *testing.T) {
	m, _ := symlinkMount(t)
	stream, errno := (&node{m: m, p: ""}).Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("Readdir: errno=%v", errno)
	}
	seen := map[string]uint32{}
	for stream.HasNext() {
		e, eerrno := stream.Next()
		if eerrno != 0 {
			t.Fatalf("readdir next: errno=%v", eerrno)
		}
		seen[e.Name] = e.Mode
	}
	if len(seen) == 0 {
		t.Fatal("the listing came back EMPTY")
	}
	if typ := seen[bfs018LinkName] & syscall.S_IFMT; typ != syscall.S_IFLNK {
		t.Fatalf("readdir reported the link as type %#o (mode %#o), want S_IFLNK", typ, seen[bfs018LinkName])
	}
	if typ := seen[bfs018TargetName] & syscall.S_IFMT; typ != syscall.S_IFREG {
		t.Fatalf("readdir reported the ordinary file as type %#o, want S_IFREG", typ)
	}
}

// F4 — a link created THROUGH the mount is a link on the server.
func TestSymlinkCreatedThroughTheMountIsASymlinkOnTheServer(t *testing.T) {
	m, root := symlinkMount(t)
	created := "created-link"
	// The mount-side half a live symlink(2) drives (beginCreate's shape): the
	// inode construction that follows needs a mounted filesystem.
	if errno := m.createLink(context.Background(), created, bfs018TargetName); errno != 0 {
		t.Fatalf("createLink: errno=%v", errno)
	}
	kind, target := servedKind(t, filepath.Join(root, created))
	if kind != "symlink" || target != bfs018TargetName {
		t.Fatalf("the server holds %s (target %q); want a symlink to %q", kind, target, bfs018TargetName)
	}
	// The row's headline assertion, on the server's own disk.
	assertNoFileHoldsTheTargetPath(t, root)
	// And the mount can read it straight back.
	got, errno := (&node{m: m, p: created}).Readlink(context.Background())
	if errno != 0 || string(got) != bfs018TargetName {
		t.Fatalf("the created link does not read back: %q errno=%v", got, errno)
	}
}

// F5 — a HARDLINK is refused by name, and the refusal is VISIBLE in the mount's
// own report (a refusal the owner cannot see is the same defect as no refusal).
func TestHardlinkIsRefusedByNameAndReported(t *testing.T) {
	m, _ := symlinkMount(t)
	var out fuse.EntryOut
	_, errno := (&node{m: m, p: ""}).Link(context.Background(), nil, "hard.txt", &out)
	if errno != syscall.EOPNOTSUPP {
		t.Fatalf("Link errno = %v, want EOPNOTSUPP", errno)
	}
	st := m.Status().Symlink
	if st.RefusalsTotal != 1 {
		t.Fatalf("refusals_total = %d, want 1: the refusal is not reported", st.RefusalsTotal)
	}
	if len(st.LastRefusals) != 1 || !strings.Contains(st.LastRefusals[0], "LINK") ||
		!strings.Contains(st.LastRefusals[0], "hardlink") {
		t.Fatalf("last_refusals = %q, want the LINK refusal with its reason", st.LastRefusals)
	}
	if !st.Declared || st.DeclaredVersion != 1 || st.TargetProperty != "b:link-target" {
		t.Fatalf("the status does not report the surface's declaration: %+v", st)
	}
}

// F6 — a declared type this client cannot present is never a regular file. The
// node table is given a kind the client does not know, exactly as a future
// surface could name one.
func TestUnhonouredDeclaredTypeIsRefusedNotPresentedAsAFile(t *testing.T) {
	m, _ := symlinkMount(t)
	m.snapshot().Put(fsclient.Node{Path: "pipe", Kind: "fifo", Size: 0, Mode: "0644"})
	var out fuse.AttrOut
	errno := (&node{m: m, p: "pipe"}).Getattr(context.Background(), nil, &out)
	if errno != fsclient.ErrnoEIO {
		t.Fatalf("Getattr on an unhonoured type: errno=%v, want EIO", errno)
	}
	if out.Mode&syscall.S_IFMT == syscall.S_IFREG {
		t.Fatalf("the entry was described as a regular file")
	}
	st := m.Status().Symlink
	if st.RefusalsTotal != 1 || !strings.Contains(st.LastRefusals[0], "fifo") {
		t.Fatalf("the type refusal is not reported: %+v", st)
	}
}

// F7 — the DECLARATION gates the write. A mount that never learned the surface
// declares the extension refuses the create BY NAME and sends nothing; the
// control proves the gate is the declaration and not a broken write path.
func TestSymlinkRefusesWithoutTheDeclarationAndWorksWithIt(t *testing.T) {
	m, root := symlinkMount(t)
	// A mount whose client never completed the handshake holds no declaration.
	blindClient, err := fsclient.NewClient(fsclient.Options{
		BaseURL: m.opts.BaseURL, Concurrency: 2, OpTimeout: 5 * time.Second, BindTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	blind := bareMount(blindClient, m)
	if errno := blind.createLink(context.Background(), "blind-link", bfs018TargetName); errno != syscall.EOPNOTSUPP {
		t.Fatalf("createLink without a declaration: errno=%v, want EOPNOTSUPP", errno)
	}
	if kind, _ := servedKind(t, filepath.Join(root, "blind-link")); kind != "absent" {
		t.Fatalf("a refused link create left a %s behind", kind)
	}
	if st := blind.Status().Symlink; st.Declared {
		t.Fatalf("a mount that never handshook reports a declaration: %+v", st)
	}
	if _, total := blind.RefusalRing(); total != 1 {
		t.Fatalf("the refusal was not reported: total=%d", total)
	}
	// CONTROL: the same create on a mount that HAS the declaration succeeds.
	if errno := m.createLink(context.Background(), "real-link", bfs018TargetName); errno != 0 {
		t.Fatalf("CONTROL FAILED: createLink on a declared surface: errno=%v", errno)
	}
	if kind, target := servedKind(t, filepath.Join(root, "real-link")); kind != "symlink" || target != bfs018TargetName {
		t.Fatalf("CONTROL FAILED: the server holds %s target=%q", kind, target)
	}
	assertNoFileHoldsTheTargetPath(t, root)
}

// F8 — ATTRIBUTION. This cell must stay GREEN under every mutation: it pins the
// behaviour the BFS-018 change must NOT alter (ordinary files and directories
// through the same mount), so a red link cell can be attributed to the link
// behaviour rather than to a broken mount.
func TestSymlinkChangeMountAttributionCell(t *testing.T) {
	m, root := symlinkMount(t)
	if err := m.refreshSnapshot(context.Background(), ""); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var out fuse.AttrOut
	if errno := (&node{m: m, p: bfs018TargetName}).Getattr(context.Background(), nil, &out); errno != 0 {
		t.Fatalf("attribution: Getattr(%s): errno=%v", bfs018TargetName, errno)
	}
	if typ := out.Mode & syscall.S_IFMT; typ != syscall.S_IFREG {
		t.Fatalf("attribution: an ordinary file's type = %#o, want S_IFREG", typ)
	}
	if out.Size != uint64(len(bfs018TargetBody)) {
		t.Fatalf("attribution: an ordinary file's size = %d, want %d", out.Size, len(bfs018TargetBody))
	}
	if out.Mode&0o777 != 0o644 {
		t.Fatalf("attribution: an ordinary file's mode = %#o, want 0644", out.Mode&0o777)
	}
	nd, _ := m.snapshot().Lookup(bfs018TargetName)
	if nd.Hash == "" {
		t.Fatalf("attribution: the ordinary file lost its content hash")
	}
	if st := m.Status().Symlink; st.RefusalsTotal != 0 || st.ReadlinksTotal != 0 {
		t.Fatalf("attribution: a mount that performed no link operation reports link activity: %+v", st)
	}
	// The served tree is untouched by the read path.
	assertNoFileHoldsTheTargetPath(t, root)
}

// assertNoFileHoldsTheTargetPath walks the SERVED tree and fails if any regular
// file's content is the link's target path — the materialisation, asserted at
// the only place that decides whether a tree was corrupted.
func assertNoFileHoldsTheTargetPath(t *testing.T, root string) {
	t.Helper()
	_ = filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if strings.TrimSpace(string(body)) == bfs018TargetName {
			t.Fatalf("%s is a REGULAR FILE whose content is the link target %q — the materialisation the row filed", path, bfs018TargetName)
		}
		return nil
	})
}
