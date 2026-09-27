package fsclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// BFS-048 pins what the client's LAST-RESORT revision poll can honestly claim
// about its coverage. The served token's granularity is the revision's declared
// kind: on a git tree ("git:<40 hex>") the token moves only when the served
// tree's HEAD ref moves, so an uncommitted working-tree edit — ours or anyone
// else's — moves nothing, and the mechanism must be understood as quiet there,
// never as a whole-tree currentness oracle. Coverage of uncommitted edits is
// the events mechanism's job (SPEC-watcher-capability §2.1, §2.4, §7 R-V1).
//
// These arms drive pollRevOnce through the same Run loop production uses
// (mechanism "rev" is selected only when the surface serves neither the push
// stream nor the events poll), against a stub OPTIONS that answers exactly
// what a real surface answers: X-Bunker-Rev on every response.
const (
	gitRev1 = "git:0123456789abcdef0123456789abcdef01234567"
	gitRev2 = "git:fdecba9876543210fdecba9876543210fdecba98"
)

// revStubServer serves the shape of a target with no watcher and no events
// poll: OPTIONS is answered with the tree/rev headers every response carries,
// and the pushed stream is refused with the declared capability_unavailable —
// which is the ONLY way the client's Run loop reaches the rev mechanism at all.
func revStubServer(rev func() string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("DAV", "1")
			w.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND, POST")
			w.Header().Set("X-Bunker-Capabilities", "1")
			w.Header().Set("X-Bunker-Tree", "tree:0123456789abcdef")
			w.Header().Set("X-Bunker-Rev", rev())
			w.WriteHeader(http.StatusOK)
			return
		}
		writeStubEnvelope(w, 501, "capability_unavailable", nil, map[string]any{
			"capability": "watch", "scope": "target", "mode": "poll",
			"detail": "no inotify watcher on this target",
		})
	}))
}

// runRevInvalidator runs the invalidator the way the mount does and returns
// (collector, cancel). The stub serves no capabilities document, so
// WatchPollOp() is false and the client's poll tier must select rev — the
// third tier this row is about.
func runRevInvalidator(t *testing.T, srv *httptest.Server, rec *collectDrops) (*Invalidator, context.CancelFunc) {
	t.Helper()
	c, err := NewClient(Options{BaseURL: srv.URL + "/dav", Concurrency: 4, OpTimeout: 5 * time.Second, BindTimeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	inv := NewInvalidator(c, InvalidateOptions{
		Mode:         "auto",
		PollInterval: 20 * time.Millisecond,
		OnDrop:       rec.drop,
		OnResync:     rec.resync,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = inv.Run(ctx) }()
	if !waitFor(5*time.Second, func() bool { return inv.revAnswers() >= 2 }) {
		cancel()
		t.Fatalf("the revision poll never answered: state %+v", inv.State())
	}
	return inv, cancel
}

// TestRevPollQuietWhileGitHeadHoldsIsTheHonestContract is BFS-048's RED
// history as a pinned fact: an uncommitted out-of-band edit happened under a
// git tree, and the revision poll MUST NOT invent a resync for it — the token
// is HEAD, and no ref moved. A poll that fired here would be reporting
// coverage the git kind does not have.
func TestRevPollQuietWhileGitHeadHoldsIsTheHonestContract(t *testing.T) {
	srv := revStubServer(func() string { return gitRev1 })
	defer srv.Close()

	rec := &collectDrops{}
	inv, cancel := runRevInvalidator(t, srv, rec)
	defer cancel()

	if got := inv.Mechanism(); got != MechanismRev {
		t.Fatalf("mechanism = %q, want %q (the last-resort tier)", got, MechanismRev)
	}
	if got := inv.lastRevSeen(); got != gitRev1 {
		t.Fatalf("revSeen = %q, want the served git token", got)
	}
	// The out-of-band edit happens HERE, on the agent, uncommitted: bytes
	// change, HEAD does not, X-Bunker-Rev keeps answering gitRev1 — exactly
	// the blindness the row measured. The honest behaviour is a quiet poll.
	time.Sleep(15 * time.Millisecond)
	if n := len(rec.resyncs); n != 0 {
		t.Fatalf("the rev poll fired %d resync(s) with no revision movement: %v", n, rec.resyncs)
	}
	if rec.full != 0 {
		t.Fatalf("the rev poll forced %d whole-tree drops with no revision movement", rec.full)
	}
}

// TestRevPollFiresOnlyWhenTheServedRevisionMoves pins the other half: a moved
// revision (a commit on a git tree, any mutation on a counter tree) is still
// delivered, exactly once per observed movement, as a full resync — and
// several answered polls at one revision stay quiet.
func TestRevPollFiresOnlyWhenTheServedRevisionMoves(t *testing.T) {
	rev := gitRev1
	srv := revStubServer(func() string { return rev })
	defer srv.Close()

	rec := &collectDrops{}
	inv, cancel := runRevInvalidator(t, srv, rec)
	defer cancel()

	// Two polls over an unchanged revision: quiet, every time.
	before := inv.revAnswers()
	time.Sleep(60 * time.Millisecond)
	if n := len(rec.resyncs); n != 0 {
		t.Fatalf("%d resync(s) before any revision movement: %v", n, rec.resyncs)
	}

	// HEAD moves (the commit the git kind covers).
	rev = gitRev2
	if !waitFor(5*time.Second, func() bool { return inv.lastRevSeen() == gitRev2 }) {
		t.Fatalf("revSeen = %q, want the moved token observed", inv.lastRevSeen())
	}
	if !waitFor(2*time.Second, func() bool { return len(rec.resyncs) == 1 }) {
		t.Fatalf("want exactly 1 resync for one revision movement, got %d: %v", len(rec.resyncs), rec.resyncs)
	}
	got := rec.resyncs[0]
	if !strings.Contains(got, gitRev1) || !strings.Contains(got, gitRev2) {
		t.Fatalf("the resync reason must name both revisions: %q", got)
	}
	if rec.full != 1 {
		t.Fatalf("want exactly 1 whole-tree drop for the movement, got %d", rec.full)
	}
	if inv.revAnswers() < before+3 {
		t.Fatalf("the poll stopped polling after the movement: %d answers, want >= %d", inv.revAnswers(), before+3)
	}
}

// TestRevPollKindScopedContractInCode is the guard this row exists to keep:
// the mechanism's contract comment must state the granularity it actually has
// (BFS-048: the old text claimed the poll answers "is my view still the tree's
// view?" for the whole tree, which is false on a git tree). The claim lives in
// pollRevOnce's own doc comment, so it can only drift together with the code
// that implements it.
func TestRevPollKindScopedContractInCode(t *testing.T) {
	doc := docCommentOf(t, "invalidate.go", "func (i *Invalidator) pollRevOnce")
	if strings.Contains(doc, "is my view still the") || strings.Contains(doc, "for the whole tree") {
		t.Fatalf("pollRevOnce still claims whole-tree coverage: %q", doc)
	}
	for _, want := range []string{"granularity", "extensions.rev.kind", "BFS-048"} {
		if !strings.Contains(doc, want) {
			t.Fatalf("pollRevOnce's contract comment lost %q: %q", want, doc)
		}
	}
}
