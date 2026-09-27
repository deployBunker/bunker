package webdav

import (
	"strings"
	"testing"
	"time"
)

// BFS-048 pins the honest contract of E-3's revision token: the two kinds
// promise DIFFERENT things, and a client's revision poll is only as current as
// the kind it is served. On a git tree the token is the resolved HEAD
// ("git:<40 hex>"), so an uncommitted working-tree write — an out-of-band
// editor's or a mutation through this very surface — moves NOTHING the rev
// poll can see; only a commit/checkout/reset moves the ref. On a non-git tree
// the counter moves on every mutation this surface performs. This is the
// declared kind-scoped coverage (SPEC-watcher-capability §7 D1/D2, R-V2), not
// a gap to paper over with a stat-per-read: aligning the revision with
// out-of-band change is the watcher's job (BFS-035, R-V1), not tree.go's.
func TestRevTokenGitKindMovesOnlyOnHeadMovement(t *testing.T) {
	head1 := strings.Repeat("a1b2c3d4", 5)
	head2 := strings.Repeat("0f9e8d7c", 5)
	h := newTestHandler(t)
	withGitFixture(t, h.Root(), head1)

	// Control for the fixture itself: this tree must be served as the git
	// kind, read from disk, or every later arm proves nothing.
	if got, want := h.tree.revToken(), "git:"+head1; got != want {
		t.Fatalf("revToken = %q, want %q (the fixture tree must be served as git)", got, want)
	}

	// An out-of-band, uncommitted working-tree write: bytes on disk, HEAD
	// untouched. The revision token must not move.
	do(t, h, "PUT", "/dav/README.md", nil, "package main\n\nfunc edited() {}\n")
	if got, want := h.tree.revToken(), "git:"+head1; got != want {
		t.Fatalf("revToken = %q, want %q: the git-kind token must not move for an uncommitted working-tree write", got, want)
	}

	// And a mutation THROUGH the surface moves nothing either: bumpRev
	// advances only the counter, which the git kind does not read. The 204
	// proves the arm is a real mutation, not a reported no-op.
	rec := do(t, h, "PUT", "/dav/README.md", nil, "package main\n\nfunc editedAgain() {}\n")
	if rec.Code != 204 {
		t.Fatalf("second PUT = %d, want 204 (a real mutation must have landed)", rec.Code)
	}
	if got, want := h.tree.revToken(), "git:"+head1; got != want {
		t.Fatalf("revToken = %q, want %q after an in-band PUT: bumpRev advances only the non-git counter", got, want)
	}

	// The one change the git kind DOES cover: HEAD's ref moves (a commit).
	// The declared gitRevCacheTTL memo on the HEAD read is part of the
	// contract, so wait out the window rather than reaching past it.
	time.Sleep(gitRevCacheTTL + 10*time.Millisecond)
	withGitFixture(t, h.Root(), head2)
	if got, want := h.tree.revToken(), "git:"+head2; got != want {
		t.Fatalf("revToken = %q, want %q after the HEAD ref moved", got, want)
	}
}

func TestRevTokenCounterKindMovesOnSurfaceMutation(t *testing.T) {
	h := newTestHandler(t) // no .git anywhere under the fixture root

	before := h.tree.revToken()
	if !strings.HasPrefix(before, "rev:") {
		t.Fatalf("revToken = %q, want the rev:<n> counter kind on a non-git tree", before)
	}

	// The counter kind's whole promise: a mutation through this surface moves
	// the token, so the client's rev poll is current for surface writes.
	do(t, h, "PUT", "/dav/README.md", nil, "package main\n\nfunc main() { /* changed */ }\n")
	after := h.tree.revToken()
	if after == before {
		t.Fatalf("counter revision %q did not move after a surface mutation", after)
	}
}
