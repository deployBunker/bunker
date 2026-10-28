package fsclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
// several answered polls at one revision stay quiet. The post-movement poll
// count is waited for with waitFor (QA-BUNKER-080): a starved poller answers
// LATE, it does not stop, so the property is asserted load-monotone instead of
// inside a fixed sleep window the scheduler can eat.
func TestRevPollFiresOnlyWhenTheServedRevisionMoves(t *testing.T) {
	// The served revision travels to the stub goroutine through an atomic:
	// the stub answers on server goroutines, so a plain captured variable
	// handed the race detector a test-data race (pre-existing at base,
	// QA-BUNKER-080 follow-through; -count=3 reproduced 4 warnings at
	// 29eaa0dc). What is asserted is unchanged.
	var rev atomic.Value // string
	rev.Store(gitRev1)
	srv := revStubServer(func() string { s, _ := rev.Load().(string); return s })
	defer srv.Close()

	rec := &collectDrops{}
	inv, cancel := runRevInvalidator(t, srv, rec)
	defer cancel()

	// Two polls over an unchanged revision: quiet, every time.
	before := inv.revAnswers()
	time.Sleep(60 * time.Millisecond)
	if n := rec.resyncCount(); n != 0 {
		t.Fatalf("%d resync(s) before any revision movement: %v", n, rec.resyncSnapshot())
	}

	// HEAD moves (the commit the git kind covers).
	rev.Store(gitRev2)
	if !waitFor(5*time.Second, func() bool { return inv.lastRevSeen() == gitRev2 }) {
		t.Fatalf("revSeen = %q, want the moved token observed", inv.lastRevSeen())
	}
	if !waitFor(2*time.Second, func() bool { return rec.resyncCount() == 1 }) {
		t.Fatalf("want exactly 1 resync for one revision movement, got %d: %v", rec.resyncCount(), rec.resyncSnapshot())
	}
	got := rec.resyncSnapshot()[0]
	if !strings.Contains(got, gitRev1) || !strings.Contains(got, gitRev2) {
		t.Fatalf("the resync reason must name both revisions: %q", got)
	}
	if full := rec.fullCount(); full != 1 {
		t.Fatalf("want exactly 1 whole-tree drop for the movement, got %d", full)
	}
	// The poller kept polling through the delivery and the assertions above.
	// Waited for, not sampled: under CPU starvation the poll goroutine only
	// answers LATE (the ticker keeps firing; each delayed request still
	// increments the counter when it is served), so the condition is
	// load-monotone where the old immediate assert after a fixed 60ms sleep
	// window was not (QA-BUNKER-080).
	if !waitFor(2*time.Second, func() bool { return inv.revAnswers() >= before+3 }) {
		t.Fatalf("the poll stopped polling after the movement: %d answers, want >= %d", inv.revAnswers(), before+3)
	}
}

// TestRevPollCountRegression_DelayedPollerReproducesTheOldRed is the
// QA-BUNKER-080 AC4 regression arm: it runs the poll-count assertion in its
// OLD form — an immediate assert after a fixed 60ms window, no waitFor —
// against a poller whose answers are artificially delayed, and demands that
// the old form FAIL (golden assertion inversion). It then proves the NEW form
// is the thing keeping the suite green by running waitFor against the SAME
// delayed poller and demanding it SUCCEED once the delay is lifted. If the
// golden step ever stops failing, the fixed window no longer bites a delayed
// poller and the waitFor conversion could be reverted unnoticed — exactly the
// silent regression this arm exists to catch.
//
// The delay is injected at the only seam the poller crosses: a dedicated stub
// parks its OPTIONS answer while parked is set, so the counter (incremented
// per answered poll in pollRevOnce) freezes. The poll loop is serial — one
// request per interval, the next only after the current returns — so a parked
// request also parks the loop itself: no request can be answered until
// release, with certainty rather than by timing luck. The settle sleep here
// IS the injected delay (the old world's hazard), not an assertion window;
// the release is closed via defer, so the arm cannot wedge the suite.
func TestRevPollCountRegression_DelayedPollerReproducesTheOldRed(t *testing.T) {
	var parked atomic.Bool
	release := make(chan struct{}) // closed once: parked answers flow
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) }) // early-exit safety: no held request outlives the test

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			if parked.Load() {
				select {
				case <-release: // the delay is lifted; answer as usual
				case <-r.Context().Done(): // the test ended; drop the held request
					return
				}
			}
			w.Header().Set("DAV", "1")
			w.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND, POST")
			w.Header().Set("X-Bunker-Capabilities", "1")
			w.Header().Set("X-Bunker-Tree", "tree:0123456789abcdef")
			w.Header().Set("X-Bunker-Rev", gitRev1)
			w.WriteHeader(http.StatusOK)
			return
		}
		writeStubEnvelope(w, 501, "capability_unavailable", nil, map[string]any{
			"capability": "watch", "scope": "target", "mode": "poll",
			"detail": "no inotify watcher on this target",
		})
	}))
	defer srv.Close()

	rec := &collectDrops{}
	inv, cancel := runRevInvalidator(t, srv, rec)
	defer cancel()

	// The old form, verbatim shape: fixed wall-clock window, then an
	// immediate assert on the counter delta. Returns -1 where the original
	// test called t.Fatalf.
	oldForm := func() int {
		before := inv.revAnswers()
		time.Sleep(60 * time.Millisecond)
		if inv.revAnswers() < before+3 {
			return -1 // the old form's fatality, reproduced
		}
		return 0
	}

	// Inject the delay and give it longer than one poll interval to take
	// hold (the 20ms ticker must reach the parked stub). The serial loop
	// then has exactly one request in flight, parked, and issues no other.
	base := inv.revAnswers()
	parked.Store(true)
	time.Sleep(60 * time.Millisecond)

	// GOLDEN: the old form must FAIL against this delayed poller.
	if got := oldForm(); got != -1 {
		t.Fatalf("golden inversion failed: the old fixed-window count form returned %d (want -1/FAIL) against a delayed poller — the window no longer reproduces the old red, so the waitFor conversion is unproven", got)
	}

	// And the NEW form must be what keeps it green: lift the delay (safe to
	// also close again at exit via the Once) and the waitFor form succeeds
	// on the same poller the old form just failed.
	releaseOnce.Do(func() { close(release) })
	if !waitFor(2*time.Second, func() bool { return inv.revAnswers() >= base+3 }) {
		t.Fatalf("the waitFor form failed on the same delayed poller after release: %d answers, want >= %d", inv.revAnswers(), base+3)
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
