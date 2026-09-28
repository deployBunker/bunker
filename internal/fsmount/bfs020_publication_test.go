//go:build linux

package fsmount

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/deployBunker/bunker/internal/fsclient"
)

// BFS-020: a file CREATED and CLOSED through the mount can be RENAMED (and
// looked up) immediately — its name is on the server before close(2) returns.
//
// THE DEFECT, as the row filed it: `printf x > <mount>/f.lock` returns rc=0 and
// `mv <mount>/f.lock <mount>/f.final` IMMEDIATELY after fails ENOENT, while the
// same rename after a 2 s settle succeeds. So the close() completion the caller
// observed was not the point at which the name became visible.
//
// THE MECHANISM, measured rather than assumed (docs/evidence/BFS-020-mechanism.txt,
// the mount's own trace against the caller's clock):
//
//	  -0.021 ms  flush-write   trace.lock      <- FLUSH arrived; the mount had no handler for it
//	  +0.010 ms  release-in    trace.lock      <- RELEASE starts the publication
//	  +0.015 ms  publish-in    trace.lock
//	  +0.447 ms  rename-in     trace.lock -> trace.final   <- the caller's `mv`, 0.02 ms after close(2)
//	  +0.645 ms  rename-out    ... MOVE ... errno=ENOENT cause=server_error status=404
//	  +4.145 ms  publish-put-done trace.lock size=1 err=<nil>   <- the name appears AFTER close(2)
//
// The publication was deferred to RELEASE, and RELEASE is not a request close(2)
// waits for. FLUSH is (the kernel blocks on its reply) — and the mount had no
// FLUSH handler at all, so the one barrier the protocol offers was unused.
//
// These are handler-level cells: they drive the SAME objects a live mount wires
// together (a real client against the landed surface, the cache, the snapshot,
// the bound registry, the handle tables), so the RULE can be asserted exactly.
// The live arms that measure the real kernel path are
// docs/evidence/BFS-020-probes/ (run-arms.sh), and they are the ones that go RED
// on the unfixed tree.
//
// Every cell below has a source mutation that turns it red, and an ATTRIBUTION
// cell that must stay green under that mutation: docs/evidence/BFS-020-arms.sh.

// createThroughMount starts the write path the way the kernel's create does:
// `node.Create`'s own mount-side half (beginCreate — handle + metadata entry),
// which is exactly what a live CREATE produces, minus the inode construction that
// needs a mounted filesystem. The handle it returns is the write handle, so the
// cells drive the same objects the kernel's CREATE/WRITE/FLUSH/RELEASE path does.
func createThroughMount(t *testing.T, m *Mount, name string) *writeHandle {
	t.Helper()
	h, _ := m.beginCreate(name, 0o644)
	if h == nil {
		t.Fatalf("Create(%s) must hand back the write handle", name)
	}
	if h.p != name {
		t.Fatalf("the write handle's path: got %q, want %q", h.p, name)
	}
	if !h.created {
		t.Fatalf("a create's handle must be marked as a create: it is what makes an empty file publishable")
	}
	return h
}

// writeThroughMount is one FUSE WRITE at the offset the kernel chose.
func writeThroughMount(t *testing.T, m *Mount, h *writeHandle, data []byte, off int64) {
	t.Helper()
	n, errno := (&node{m: m, p: h.p}).Write(context.Background(), h, data, off)
	if errno != 0 {
		t.Fatalf("Write(%s, off=%d): errno=%v", h.p, off, errno)
	}
	if n != uint32(len(data)) {
		t.Fatalf("a write must acknowledge every byte it accepted: got %d of %d", n, len(data))
	}
}

// renameThroughMount performs what the kernel does for rename(2): it splits both
// paths into (parent node, name) and dispatches ONE Rename, which is where the
// row's ENOENT came from (the server's own 404 for a name the mount had not sent).
func renameThroughMount(t *testing.T, m *Mount, src, dst string) syscall.Errno {
	t.Helper()
	sp, sn := path.Split(src)
	dp, dn := path.Split(dst)
	srcNode := &node{m: m, p: strings.TrimSuffix(sp, "/")}
	dstNode := &node{m: m, p: strings.TrimSuffix(dp, "/")}
	return srcNode.Rename(context.Background(), sn, dstNode, dn, 0)
}

// serverPath is the served tree's own path for a mount-relative path: the clock
// the mount cannot influence, and the only place a name "really exists".
func serverPath(target, p string) string { return filepath.Join(filepath.Dir(target), p) }

func mustNotExist(t *testing.T, path string, why string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("%s: %s exists on the served tree", path, why)
	}
}

// TestACreatedAndClosedFileCanBeRenamedImmediately is THE cell: create, write,
// CLOSE (Flush — the point close(2) waits for), then RENAME with no settle at all.
// Both the rename and the served tree must agree that the file is there.
//
// The cell asserts the PROTOCOL FACT first, on its own: after the FLUSH the name is
// on the served tree — before the RELEASE that follows it, and before any other
// operation. That is the assertion that distinguishes this fix from publishing at
// RELEASE: the kernel does not wait for RELEASE (measured), so a publication made
// there is still in flight when the caller's next syscall arrives.
func TestACreatedAndClosedFileCanBeRenamedImmediately(t *testing.T) {
	m, target := testMount(t, boundTestShort)

	h := createThroughMount(t, m, "f.lock")
	body := []byte("x")
	writeThroughMount(t, m, h, body, 0)

	// CLOSE. In a live mount this is the FLUSH the kernel sends per close(2) and
	// then the RELEASE; the protocol fact this row rests on is that FLUSH's reply
	// is the one the kernel waits for.
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("close: the publication must succeed, got errno=%v", errno)
	}
	// THE PROTOCOL FACT. The served tree must already hold the name and the bytes.
	if got := fileBytes(t, serverPath(target, "f.lock")); string(got) != string(body) {
		t.Fatalf("THE PUBLICATION POINT MUST BE THE FLUSH CLOSE(2) WAITS FOR: after it, the served tree holds %q, want %q (nothing was published until RELEASE, which the kernel does not wait for)", got, body)
	}
	// The RELEASE the kernel sends next must not publish a second time.
	before := m.client.Requests()
	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("release: errno=%v", errno)
	}
	if delta := m.client.Requests() - before; delta != 0 {
		t.Fatalf("the RELEASE after a FLUSH must add no request (the publication is idempotent), but it made %d", delta)
	}

	// THE RENAME, immediately and with no settle. This is the shape git uses for
	// every lock file it ever writes.
	if errno := renameThroughMount(t, m, "f.lock", "f.final"); errno != 0 {
		t.Fatalf("A FILE CREATED AND CLOSED THROUGH THE MOUNT MUST BE RENAMEABLE IMMEDIATELY: rename errno=%v (the caller's close(2) returned 0)", errno)
	}
	if got := fileBytes(t, serverPath(target, "f.final")); string(got) != string(body) {
		t.Fatalf("the renamed file's bytes on the served tree: %q, want %q", got, body)
	}
	mustNotExist(t, serverPath(target, "f.lock"), "the source name")
}

// TestACreateWithNoChunkStillPublishesTheName is the same defect one shape further
// in, and it does NOT have a window: `: > f`, `printf ” > f` and `touch f` all
// arrive as a create with NO WRITE at all, and MEASURED on the unfixed tree the
// name was then never sent — the mount claimed it (0 bytes) while the served tree
// never had it, so the rename failed ENOENT forever
// (docs/evidence/BFS-020-empty.txt). An empty file is a file.
func TestACreateWithNoChunkStillPublishesTheName(t *testing.T) {
	m, target := testMount(t, boundTestShort)

	h := createThroughMount(t, m, "e.lock")
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("close of an empty create: errno=%v", errno)
	}
	// THE CELL: the name and the (zero-byte) content are on the served tree as soon
	// as the FLUSH returns. Without this the file exists only in this mount's
	// imagination — MEASURED on the unfixed tree: `: > f`, `printf '' > f` and
	// `touch f` all returned 0 and the served tree NEVER had the file, so every
	// later operation on it failed ENOENT forever
	// (docs/evidence/BFS-020-empty.txt).
	if _, err := os.Stat(serverPath(target, "e.lock")); err != nil {
		t.Fatalf("AN EMPTY FILE CREATED AND CLOSED THROUGH THE MOUNT MUST EXIST ON THE SERVED TREE: %v (the served tree never got the name)", err)
	}
	if errno := renameThroughMount(t, m, "e.lock", "e.final"); errno != 0 {
		t.Fatalf("an EMPTY created file must be renameable immediately too: rename errno=%v", errno)
	}
	got, err := os.ReadFile(serverPath(target, "e.final"))
	if err != nil {
		t.Fatalf("an empty file created through the mount must exist on the served tree: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the empty file's content: %d bytes, want 0", len(got))
	}
	mustNotExist(t, serverPath(target, "e.lock"), "the source name")
}

// TestARenameOfAFileThisMountIsStillHoldingPublishesItFirst is the second half of
// the fix: a rename is a following operation like any other, and when the mount is
// still holding the bytes (the handle has not reached a publication point yet),
// the barrier publishes them rather than letting the server's 404 reach the caller
// as a bare ENOENT. No data is lost either way — the file keeps its bytes.
func TestARenameOfAFileThisMountIsStillHoldingPublishesItFirst(t *testing.T) {
	m, target := testMount(t, boundTestShort)

	h := createThroughMount(t, m, "held.lock")
	body := []byte("held-bytes")
	writeThroughMount(t, m, h, body, 0)

	// NO publication point yet: the bytes are only in the handle's buffer.
	if errno := renameThroughMount(t, m, "held.lock", "held.final"); errno != 0 {
		t.Fatalf("a rename of a name this mount is holding must publish it rather than answer ENOENT: errno=%v", errno)
	}
	if got := fileBytes(t, serverPath(target, "held.final")); string(got) != string(body) {
		t.Fatalf("the held bytes must be published by the rename's barrier: got %q, want %q", got, body)
	}
	mustNotExist(t, serverPath(target, "held.lock"), "the source name")
}

// TestAHandleFollowsARenameAndDoesNotResurrectTheOldName: after the name moves, a
// later write on the still-open handle must land under the NEW name. The write
// path is path-addressed, so a handle that kept the old path would publish a
// second file the caller moved away — a name the caller removed, back again, and
// the content split across two names.
func TestAHandleFollowsARenameAndDoesNotResurrectTheOldName(t *testing.T) {
	m, target := testMount(t, boundTestShort)

	h := createThroughMount(t, m, "move.lock")
	writeThroughMount(t, m, h, []byte("first-"), 0)
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("close: errno=%v", errno)
	}
	if errno := renameThroughMount(t, m, "move.lock", "move.final"); errno != 0 {
		t.Fatalf("rename: errno=%v", errno)
	}
	// The caller writes MORE through the same descriptor and closes again — the
	// `mv` of a file another process still has open.
	writeThroughMount(t, m, h, []byte("second"), int64(len("first-")))
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("close after the rename: errno=%v", errno)
	}
	if got := fileBytes(t, serverPath(target, "move.final")); string(got) != "first-second" {
		t.Fatalf("the later bytes must land under the name the file now has: got %q, want %q", got, "first-second")
	}
	mustNotExist(t, serverPath(target, "move.lock"), "the name the caller moved away (the handle resurrected it)")
	if h2 := h; h2.p != "move.final" {
		t.Fatalf("the handle's own path must follow the rename: got %q, want %q", h2.p, "move.final")
	}
}

// TestAWriteAfterAPublicationPointIsStillPublished: the kernel sends more than one
// publication point for one open, and a chunk that arrives after one of them must
// still be sent. Without this rule the later bytes are buffered and never
// published while the caller's close(2) reports success — the silent-loss class
// this row exists for, one publication point further in.
func TestAWriteAfterAPublicationPointIsStillPublished(t *testing.T) {
	m, target := testMount(t, boundTestShort)

	h := createThroughMount(t, m, "two.lock")
	writeThroughMount(t, m, h, []byte("AAAA"), 0)
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("first publication: errno=%v", errno)
	}
	if got := fileBytes(t, serverPath(target, "two.lock")); string(got) != "AAAA" {
		t.Fatalf("the first publication must land: got %q", got)
	}
	// A second chunk AFTER the publication point (a dup'd descriptor closing
	// twice, an fsync mid-write, a shell sending several commands to one fd).
	writeThroughMount(t, m, h, []byte("BBBB"), 4)
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("second publication: errno=%v", errno)
	}
	if got := fileBytes(t, serverPath(target, "two.lock")); string(got) != "AAAABBBB" {
		t.Fatalf("a write after a publication point must still land: got %q, want %q", got, "AAAABBBB")
	}
}

// TestACreateAfterAnUnlinkOfTheSameNameResolvesAFreshBase is git's OTHER lock
// shape, and it is the reason the base record must not outlive the name: git locks
// `.git/AUTO_MERGE.lock` (empty), unlinks it, and locks the same name AGAIN a few
// milliseconds later. With the hash of the removed file still remembered as the
// path's base, the second create is refused 412 — a refusal the caller cannot
// explain, for a name it has every right to create.
func TestACreateAfterAnUnlinkOfTheSameNameResolvesAFreshBase(t *testing.T) {
	m, target := testMount(t, boundTestShort)

	// The lock, written and closed.
	h := createThroughMount(t, m, "lock.lock")
	writeThroughMount(t, m, h, []byte("first"), 0)
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("first lock close: errno=%v", errno)
	}
	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("first lock release: errno=%v", errno)
	}
	// Removed (git does this for a lock it only ever held to delete a ref).
	if errno := (&node{m: m, p: ""}).Unlink(context.Background(), "lock.lock"); errno != 0 {
		t.Fatalf("unlink: errno=%v", errno)
	}
	mustNotExist(t, serverPath(target, "lock.lock"), "the unlinked name")

	// The SAME name, created again — the second lock of the same command.
	h2 := createThroughMount(t, m, "lock.lock")
	writeThroughMount(t, m, h2, []byte("second"), 0)
	if errno := h2.Flush(context.Background()); errno != 0 {
		t.Fatalf("A CREATE OF A NAME THAT WAS JUST REMOVED MUST RESOLVE ITS OWN BASE: the second lock's close returned errno=%v", errno)
	}
	if got := fileBytes(t, serverPath(target, "lock.lock")); string(got) != "second" {
		t.Fatalf("the second lock's content on the served tree: %q, want %q", got, "second")
	}
}

// TestACreateAfterARenameOfTheSameNameResolvesAFreshBase is the same rule for the
// shape that broke a live `git checkout -b` outright: git writes
// `.git/index.lock`, closes it, RENAMES it over `.git/index`, and later writes
// `.git/index.lock` again. The rename moved the file the client remembered, so the
// old name's base record must go with it — measured, the second create inherited
// the first one's hash, the server refused it 412, and git died
// `fatal: unable to write new index file`.
func TestACreateAfterARenameOfTheSameNameResolvesAFreshBase(t *testing.T) {
	m, target := testMount(t, boundTestShort)

	h := createThroughMount(t, m, "index.lock")
	writeThroughMount(t, m, h, []byte("index-v1"), 0)
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("first close: errno=%v", errno)
	}
	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("first release: errno=%v", errno)
	}
	if errno := renameThroughMount(t, m, "index.lock", "index"); errno != 0 {
		t.Fatalf("rename over the target: errno=%v", errno)
	}
	if got := fileBytes(t, serverPath(target, "index")); string(got) != "index-v1" {
		t.Fatalf("the moved file's content: %q, want %q", got, "index-v1")
	}

	// The same name again: the index was written a second time.
	h2 := createThroughMount(t, m, "index.lock")
	writeThroughMount(t, m, h2, []byte("index-v2"), 0)
	if errno := h2.Flush(context.Background()); errno != 0 {
		t.Fatalf("A CREATE OF A NAME THIS MOUNT JUST MOVED AWAY MUST RESOLVE ITS OWN BASE: the second index.lock close returned errno=%v", errno)
	}
	if got := fileBytes(t, serverPath(target, "index.lock")); string(got) != "index-v2" {
		t.Fatalf("the second lock's content: %q, want %q", got, "index-v2")
	}
	if errno := renameThroughMount(t, m, "index.lock", "index"); errno != 0 {
		t.Fatalf("second rename over the target: errno=%v", errno)
	}
	if got := fileBytes(t, serverPath(target, "index")); string(got) != "index-v2" {
		t.Fatalf("the target after the second rename: %q, want %q", got, "index-v2")
	}
}

// TestAReadAfterARenameOverTheTargetServesTheMovedContent is the CONTENT half of
// the same claim the row makes about names: a following operation on the same mount
// must SEE what this mount itself just did. Measured before this rule existed: a
// rename REPLACES the destination, the cache entry held for the destination
// survived it, and every later read through the mount was served the replaced file
// (docs/evidence/BFS-020-content.txt) — which is how a live `git checkout -b` read
// its own just-written HEAD back as the branch it had left.
func TestAReadAfterARenameOverTheTargetServesTheMovedContent(t *testing.T) {
	m, target := testMount(t, boundTestShort)

	// The read that populates this client's cache for the destination.
	if got := statThroughMount(t, m, "target.txt"); got != uint64(len(boundTestShort)) {
		t.Fatalf("stat target.txt: size=%d, want %d", got, len(boundTestShort))
	}
	if got := readBytesThroughMount(t, m, "target.txt"); string(got) != boundTestShort {
		t.Fatalf("the first read: %q, want %q", got, boundTestShort)
	}

	// Replace the target the way every atomic-save tool does: write a sibling,
	// close it, rename it over the target.
	moved := []byte("THE-MOVED-CONTENT-0123456789-abcdefghijklmnopqrstuvwxyz")
	h := createThroughMount(t, m, "new.lock")
	writeThroughMount(t, m, h, moved, 0)
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("close: errno=%v", errno)
	}
	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("release: errno=%v", errno)
	}
	if errno := renameThroughMount(t, m, "new.lock", "target.txt"); errno != 0 {
		t.Fatalf("rename over the target: errno=%v", errno)
	}
	if got := fileBytes(t, target); string(got) != string(moved) {
		t.Fatalf("the served tree after the rename: %q, want %q", got, moved)
	}

	// THE CELL. The kernel re-stats a name whose attributes it invalidated (an
	// entry timeout of 0 and a rename both do that), so the read follows a stat —
	// exactly what the kernel does before `cat`'s read.
	if got := statThroughMount(t, m, "target.txt"); got != uint64(len(moved)) {
		t.Fatalf("the post-rename stat must be the server's size: %d, want %d", got, len(moved))
	}
	if got := readBytesThroughMount(t, m, "target.txt"); string(got) != string(moved) {
		t.Fatalf("A READ AFTER THIS MOUNT REPLACED THE FILE MUST SERVE THE NEW CONTENT: got %q, want %q (the cache entry for the replaced destination survived the rename)", got, moved)
	}
}

// TestRmdirOfANonEmptyCollectionIsRefused is the POSIX guarantee git's own
// directory walk depends on: rmdir on a NON-EMPTY directory fails with ENOTEMPTY.
// Passed through, the request is a RECURSIVE DELETE — the surface's DELETE on a
// collection removes the subtree — so a caller that only wanted to tidy up an
// empty directory destroys everything under it. MEASURED before this check
// existed: `rmdir <mount>/rm` returned rc=0 and every file under it was gone from
// the served tree (docs/evidence/BFS-020-rmdir.txt).
func TestRmdirOfANonEmptyCollectionIsRefused(t *testing.T) {
	m, target := testMount(t, boundTestShort)
	root := filepath.Dir(target)
	// A small tree, created on the server (what the caller sees through the mount).
	if err := os.MkdirAll(filepath.Join(root, "rm", "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, p := range []string{"rm/g.txt", "rm/sub/f.txt"} {
		if err := os.WriteFile(filepath.Join(root, p), []byte("keep me"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	if errno := (&node{m: m, p: ""}).Rmdir(context.Background(), "rm"); errno != syscall.ENOTEMPTY {
		t.Fatalf("rmdir of a NON-EMPTY directory must fail with ENOTEMPTY, got errno=%v", errno)
	}
	// THE DATA: every file under it must still be there on the served tree.
	for _, p := range []string{"rm/g.txt", "rm/sub/f.txt"} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Fatalf("RMDIR OF A NON-EMPTY DIRECTORY DESTROYED THE SERVED SUBTREE: %s is gone (%v)", p, err)
		}
	}
	// And an EMPTY directory is still removable — the check is not a blanket
	// refusal of directory removal.
	if err := os.Remove(filepath.Join(root, "rm", "sub", "f.txt")); err != nil {
		t.Fatalf("clear the subdir: %v", err)
	}
	if errno := (&node{m: m, p: "rm"}).Rmdir(context.Background(), "sub"); errno != 0 {
		t.Fatalf("rmdir of an EMPTY directory must succeed, got errno=%v", errno)
	}
	if _, err := os.Stat(filepath.Join(root, "rm", "sub")); err == nil {
		t.Fatal("the emptied directory must be gone from the served tree")
	}
}

// TestUnlinkOfACollectionIsRefused: the surface's DELETE on a collection is
// RECURSIVE, so an unlink must never be pointed at a directory. The check is
// local (the metadata this mount most recently gave the kernel) and costs no
// request.
func TestUnlinkOfACollectionIsRefused(t *testing.T) {
	m, target := testMount(t, boundTestShort)
	root := filepath.Dir(target)
	if err := os.MkdirAll(filepath.Join(root, "d", "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "d", "sub", "f.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The metadata entry Lookup leaves behind for a collection (the kernel only
	// unlinks a path whose attrs it holds, and those attrs are this mount's).
	m.snapshot().Put(fsclient.Node{Path: "d", IsDir: true, Mode: "0755"})

	if errno := (&node{m: m, p: ""}).Unlink(context.Background(), "d"); errno != syscall.EISDIR {
		t.Fatalf("unlink of a collection must fail with EISDIR, got errno=%v", errno)
	}
	if _, err := os.Stat(filepath.Join(root, "d", "sub", "f.txt")); err != nil {
		t.Fatalf("an unlink pointed at a collection DESTROYED the served subtree: %v", err)
	}
}

// TestARefusedCreateDoesNotLeaveTheMountClaimingTheName: a publication the server
// REFUSES must not leave the mount answering "the file is here" from the entry its
// Create invented. The one leak left in this class after the barrier, and the same
// rule the stale-lock case needs: never claim a name the server does not have.
func TestARefusedCreateDoesNotLeaveTheMountClaimingTheName(t *testing.T) {
	m, target := testMount(t, boundTestShort)

	h := createThroughMount(t, m, "raced.txt")
	writeThroughMount(t, m, h, []byte("ours"), 0)

	// The name appears on the server out of band between the create and its
	// publication: the create's own precondition (`If-None-Match: *`) must refuse
	// — the server's answer, not the mount's belief, decides.
	planted := []byte("planted-by-someone-else")
	if err := os.WriteFile(serverPath(target, "raced.txt"), planted, 0o644); err != nil {
		t.Fatalf("plant: %v", err)
	}
	if errno := h.Flush(context.Background()); errno == 0 {
		t.Fatal("the publication must be REFUSED: the name was created on the server under it (the defect this row forbids is a landed write behind a refusal)")
	}
	if got := fileBytes(t, serverPath(target, "raced.txt")); string(got) != string(planted) {
		t.Fatalf("the refused publication must land nothing: the served tree holds %q, want %q", got, planted)
	}
	// THE HONESTY ASSERTION: what the mount says the file is must be what the
	// server says, not the size-0 entry the create invented.
	if size := statThroughMount(t, m, "raced.txt"); size != uint64(len(planted)) {
		t.Fatalf("the mount must answer from the SERVER after its own publication was refused: stat=%d, want %d (it is still claiming the entry its create invented)", size, len(planted))
	}
}

// TestAPublicationRefusalAtCloseIsReportedToTheCaller pins the surface change this
// row makes deliberately: the refusal is returned by Flush — the point close(2)
// waits for — instead of only being recorded on the owner-facing surface. It is
// the same contract the append path has had since BFS-021, and it is strictly more
// honest: a caller whose bytes were refused learns it at the close rather than
// only from `bunker fs conflicts`. What must NOT change is the semantics: nothing
// lands, the original survives, and the refusal is recorded.
func TestAPublicationRefusalAtCloseIsReportedToTheCaller(t *testing.T) {
	m, target := testMount(t, boundTestShort)

	h := createThroughMount(t, m, "refused.txt")
	writeThroughMount(t, m, h, []byte("ours"), 0)

	planted := []byte("the-other-writer-won")
	if err := os.WriteFile(serverPath(target, "refused.txt"), planted, 0o644); err != nil {
		t.Fatalf("plant: %v", err)
	}
	// A REFUSAL AT CLOSE IS NOT SILENT.
	if errno := h.Flush(context.Background()); errno != fsclient.ErrnoESTALE {
		t.Fatalf("the refusal must reach the caller at close(2), want ESTALE, got errno=%v", errno)
	}
	if got := fileBytes(t, serverPath(target, "refused.txt")); string(got) != string(planted) {
		t.Fatalf("the refused write must not land: got %q, want %q", got, planted)
	}
	if st := m.Status(); st.Conflicts.RefusalsTotal == 0 {
		t.Fatal("the refusal must still be recorded on the owner-facing surface (a rule that fires silently is not a rule anyone can audit)")
	}
	// And it STICKS: the RELEASE that follows reports the same verdict rather than
	// a success, and offers the server nothing.
	if errno := h.Release(context.Background()); errno != fsclient.ErrnoESTALE {
		t.Fatalf("the refusal must be terminal for the handle: release errno=%v, want ESTALE", errno)
	}
}
