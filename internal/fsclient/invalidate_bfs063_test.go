package fsclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-063: the events poll's off-the-end resume is a SILENT GAP for a freshly
// bound client.
//
// The client's own gap rule is guarded on a NON-ZERO cursor (`apply`:
// `ev.Seq > i.seq+1 && i.seq != 0`), so a client whose cursor is 0 — every
// freshly bound client — cannot read a retained tail as a tail. BFS-026 relied
// on that rule for the off-the-end case, and the ledger's only other marker (its
// first-observation `overflow`) is an ordinary journal entry that rotates out.
//
// The arms below drive the REAL client, the REAL surface and the REAL cache, and
// they assert on the client's own record and bytes rather than on the server's
// source:
//
//   - a client that has observed NOTHING is told the interval cannot be vouched
//     for, and converges — one resync, then per-path invalidations, never a
//     resync per poll (the RED is this same measurement against the unfixed rule,
//     recorded verbatim in docs/evidence/BFS-063-*: there the client is told
//     `nothing changed`, drops nothing, resyncs nothing, and goes on serving the
//     pre-edit bytes);
//   - a client that bound WITH a whole-tree snapshot is not handed an unearned
//     overflow — the regression risk of the first arm;
//   - a client that holds a cursor and falls behind the retained journal is still
//     served the retained tail, whose gap its own rule reads. That is the case
//     SPEC-watcher-capability §2.2/§5.3 documents, and the fix must not have
//     turned it into silence.
// ---------------------------------------------------------------------------

// seedThroughAnotherClient is the state every real deployment reaches as soon as
// ANY mount binds: a whole-tree snapshot is taken and the ledger adopts it as its
// baseline. This test uses a SECOND client so the observation is provably not the
// one the client under test holds.
func seedThroughAnotherClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	other, err := NewClient(Options{BaseURL: srvURL, Concurrency: 2, OpTimeout: 10 * time.Second, BindTimeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("second client: %v", err)
	}
	if _, oerr := other.SnapshotTree(context.Background(), "", false); oerr != nil {
		t.Fatalf("the other client's whole-tree snapshot: %v", oerr)
	}
	return other
}

// servedBytes reads a path straight off the served tree — the "bytes differ" side
// of the arms below.
func servedBytes(t *testing.T, root, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, p))
	if err != nil {
		t.Fatalf("read served %s: %v", p, err)
	}
	return b
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// newBFS063Cache is the mount's cache, in a temp dir: nothing here may touch the
// real user cache.
func newBFS063Cache(t *testing.T) *Cache {
	t.Helper()
	cache := newTestCache(t, DefaultCacheMaxBytes, DefaultCacheMaxEntryBytes)
	t.Cleanup(func() { _ = cache.Close() })
	return cache
}

// TestBFS063AFreshClientIsNeverToldNothingChanged is the row's defect, measured
// on the real client with real bytes.
//
// The window it reproduces needs no journal rotation to open, and that is worth
// stating because the row's own hole (H-10) was filed against the rotation: a
// client that has observed NOTHING holds bytes from an earlier life, an
// out-of-band edit lands, and THEN the ledger's baseline is established by
// another client's whole-tree snapshot — which BFS-026 deliberately records as an
// observation WITHOUT emitting an event. The edit is therefore inside the
// ledger's baseline and no event will ever mention it.
//
// What such a client is answered on the unfixed tree is
// `{"events":[],"count":0,"head_seq":0}` — the empty tail that means "I observed,
// and nothing moved in the interval I am accountable for"
// (SPEC-watcher-capability §5.1). For a client that has observed nothing that
// statement is false, and the consequence is the bytes: the cache keeps serving
// the pre-edit content while the tree serves the post-edit content.
func TestBFS063AFreshClientIsNeverToldNothingChanged(t *testing.T) {
	c, root, srv := fixtureEndpoint(t)
	cache := newBFS063Cache(t)
	ctx := context.Background()

	const path = "README.md"
	before := servedBytes(t, root, path)
	beforeHash := HashBytes(before)
	if _, err := cache.Insert(path, beforeHash, before); err != nil {
		t.Fatalf("cache insert: %v", err)
	}

	// The out-of-band edit: no request of ours, no surface call.
	after := []byte("# fixture, edited out of band\n")
	if err := os.WriteFile(filepath.Join(root, path), after, 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if sum(before) == sum(after) {
		t.Fatal("the fixture did not change the bytes, so every arm below would pass vacuously")
	}

	// Another client binds with a whole-tree snapshot: the ledger's knowledge now
	// BEGINS at the post-edit tree, so the edit is absorbed as a baseline.
	seedThroughAnotherClient(t, srv.URL)

	rec := &collectDrops{}
	inv := mountWiring(t, c, rec, cache, false) // this client has observed NOTHING
	if st := inv.State(); st.ResumeSeq != nil {
		t.Fatalf("a client that has observed nothing reported a resume point: %d", *st.ResumeSeq)
	}

	if err := inv.pollEventsOnce(ctx); err != nil {
		t.Fatalf("the poll failed outright, which is not the defect: %v", err)
	}

	st := inv.State()
	// The staleness measurement is taken BEFORE the assertions so that every
	// failure below names the bytes as well as the record: on the unfixed tree the
	// cache keeps the pre-edit copy and the served bytes are the post-edit ones,
	// which is the "nothing changed while the bytes differ" this row was filed for.
	held, stillCached := cache.Get(path, beforeHash)
	staleNote := "the pre-edit copy is gone"
	if stillCached {
		staleNote = "the pre-edit copy is STILL CACHED"
	}
	served := servedBytes(t, root, path)
	measured := func() string {
		return "cached=" + staleNote +
			" cached_sum=" + first12(sum(held)) + " served_sum=" + first12(sum(served)) +
			" drops=" + rec.summary() + " state=" + jsonish(st)
	}

	// THE FIX, in the client's own numbers. Under the defect every one of these
	// is the opposite: drops=0, resyncs=0, no reason, and the pre-edit bytes still
	// cached and served while the tree has moved.
	if st.Resyncs != 1 || rec.full != 1 {
		t.Fatalf("a client that has observed nothing was not told the interval is unvouched: %s", measured())
	}
	if len(rec.paths) != 0 {
		t.Fatalf("the unvouched answer carried paths: %s", measured())
	}
	if st.Gaps != 0 {
		t.Fatalf("the server declared knowledge lost and the client recorded it as a client-detected GAP (%d): they are different facts (%s)", st.Gaps, measured())
	}
	if !strings.Contains(st.Reason, "overflow") {
		t.Fatalf("the reason does not name what arrived: %q (%s)", st.Reason, measured())
	}
	if st.ResumeSeq == nil {
		t.Fatalf("after re-observing, the client still holds no resume point, so every future poll would be answered unvouched (%s)", measured())
	}

	// The bytes: the resync dropped the stale copy, and a read now returns what
	// the tree serves — so the client can no longer serve pre-edit content while
	// the channel reports nothing.
	if stillCached {
		t.Fatalf("the pre-edit copy survived the resync drop: %s", measured())
	}
	got, _, oerr := c.Get(ctx, path, "")
	if oerr != nil {
		t.Fatalf("read after the resync: %v", oerr)
	}
	if sum(got) != sum(after) {
		t.Fatalf("the client still serves the pre-edit bytes: %s vs the served %s", first12(sum(got)), first12(sum(after)))
	}
	t.Logf("BFS-063 measured: pre-edit=%s served=%s resume_seq=%d resyncs=%d gaps=%d drops=%s",
		first12(sum(before)), first12(sum(after)), *st.ResumeSeq, st.Resyncs, st.Gaps, rec.summary())
}

// first12 is the first 12 hex characters of a digest: enough to compare, short
// enough to read in a failure message.
func first12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// TestBFS063ASeededClientIsNotGivenAnUnearnedOverflow is the regression risk of
// the fix, and it is the arm a blanket "no cursor means overflow, so cursor 0
// must too" would break.
//
// A client that bound with a whole-tree snapshot HAS an observation, the ledger
// adopted the same observation as its baseline, and the interval before it is
// genuinely covered — so its poll is answered from the ledger, quietly, with no
// overflow. It is distinguished from the client above by one thing: it presents
// the cursor the snapshot answer minted for it.
func TestBFS063ASeededClientIsNotGivenAnUnearnedOverflow(t *testing.T) {
	c, root, _ := fixtureEndpoint(t)
	ctx := context.Background()
	rec := &collectDrops{}
	inv := mountWiring(t, c, rec, nil, true) // bound WITH the whole-tree snapshot

	st := inv.State()
	if st.ResumeSeq == nil {
		t.Fatal("a client that took a whole-tree snapshot holds no resume point, so it cannot declare what it holds")
	}

	if err := inv.pollEventsOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	st = inv.State()
	if st.Resyncs != 0 || rec.full != 0 {
		t.Fatalf("a seeded client was handed an overflow it did not earn: %+v (%s)", st, rec.summary())
	}
	if len(rec.paths) != 0 {
		t.Fatalf("a quiet, seeded tree produced drops: %s", rec.summary())
	}

	// It still sees a real change, per path, immediately after — so the quiet
	// answer is a measurement and not a channel that has stopped working.
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main\n\nfunc main() { /* edited */ }\n"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if err := inv.pollEventsOnce(ctx); err != nil {
		t.Fatalf("poll after edit: %v", err)
	}
	if !rec.dropped("src/main.go") {
		t.Fatalf("the seeded client did not receive the per-path invalidation: %s", rec.summary())
	}
	if inv.State().Resyncs != 0 {
		t.Fatalf("a seeded client resynced for a per-path change: %s", rec.summary())
	}
}

// TestBFS063OneUnvouchedAnswerThenConvergence pins the cost, not just the
// honesty: a client that has observed nothing is told so ONCE, re-observes, and
// from then on the channel is per-path. A fix that answered `overflow` to every
// poll of such a client would be honest and useless — a resync loop paid on every
// interval, and a journal churned for every other client on the tree.
func TestBFS063OneUnvouchedAnswerThenConvergence(t *testing.T) {
	c, root, _ := fixtureEndpoint(t)
	ctx := context.Background()
	rec := &collectDrops{}
	inv := mountWiring(t, c, rec, nil, false)

	if err := inv.pollEventsOnce(ctx); err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	if inv.State().Resyncs != 1 {
		t.Fatalf("the first answer for a client with no observation was not a resync: %+v", inv.State())
	}
	// The resync re-observed (mountWiring's OnResync, as the mount's does), so the
	// client holds a resume point and the next polls are answered ordinarily.
	for i := 0; i < 3; i++ {
		if err := inv.pollEventsOnce(ctx); err != nil {
			t.Fatalf("poll %d: %v", i+2, err)
		}
	}
	if got := inv.State().Resyncs; got != 1 {
		t.Fatalf("resyncs = %d after four polls: the unvouched answer repeats instead of converging (%s)", got, rec.summary())
	}

	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# fixture, edited\n"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if err := inv.pollEventsOnce(ctx); err != nil {
		t.Fatalf("poll after edit: %v", err)
	}
	if !rec.dropped("README.md") {
		t.Fatalf("the converged client did not receive the per-path invalidation: %s", rec.summary())
	}
	if inv.State().Resyncs != 1 {
		t.Fatalf("a per-path change cost a resync: %s", rec.summary())
	}
}

// TestBFS063AStaleCursorThatTheClientCanReadIsStillATail is the other side of the
// line the rule draws, measured through the client's own counters.
//
// A client that HOLDS a cursor and falls behind the retained journal is served
// the retained tail, which is admissible because the client can recognise it: its
// first seq is above `cursor+1` and the client's own monotonicity rule fires. That
// is the case SPEC-watcher-capability §2.2/§5.3 documents, and the fix must not
// have turned it into silence.
func TestBFS063AStaleCursorThatTheClientCanReadIsStillATail(t *testing.T) {
	c, root, srv := fixtureEndpoint(t)
	ctx := context.Background()
	rec := &collectDrops{}
	inv := mountWiring(t, c, rec, nil, true)

	// Advance this client's own cursor with one real change, so it holds a
	// non-zero resume point: the cursor-0 arm above is a different case and this
	// one must not be it.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# fixture, cursor moved\n"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if err := inv.pollEventsOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if st := inv.State(); st.ResumeSeq == nil || *st.ResumeSeq == 0 {
		t.Fatalf("the fixture did not move this client's cursor off zero: %+v", st)
	}
	before := inv.State()

	// Rotate the journal out from under it, through another client's observations.
	//
	// The loop is bounded by the OUTCOME it exists to reach — the client's cursor
	// provably behind the retained journal — and not by an iteration count
	// (QA-BUNKER-49): the rotation is a property of the LEDGER's state, so the
	// loop watches for it and stops the moment it holds. Each burst iteration
	// writes one change and polls the ledger once, which pushes exactly one
	// invalidate onto the journal, so the structural minimum is the journal's
	// declared 256-line capacity plus the lines the fixture already spent; the
	// probe reads that state directly instead of counting: a poll at
	// since_seq 0 (a presented, zero cursor) is answered the retained tail while
	// seq 1 is still retained, and exactly one positive-seq `overflow` marker
	// once the rotation has dropped it (the ledger's cursor-0 rule, the same
	// resumeLocked table this row's client rule reads). The bound stays far
	// above the structural minimum as a runaway guard — a lost push or a quiet
	// poll now fails LOUDLY here instead of silently under- or over-rotating.
	other := seedThroughAnotherClient(t, srv.URL)
	const burstMax = 3 * 256 // ≫ the journal's declared 256-line capacity: a guard, not the target count
	rotated := false
	for i := 0; i < burstMax && !rotated; i++ {
		if err := os.WriteFile(filepath.Join(root, "burst.txt"), []byte("burst "+strings.Repeat("x", i%7)+"\n"), 0o644); err != nil {
			t.Fatalf("burst write %d: %v", i, err)
		}
		var probe struct {
			Events []struct {
				Seq   int64  `json:"seq"`
				Event string `json:"event"`
			} `json:"events"`
		}
		if _, oerr := other.Op(ctx, "events", map[string]any{"since_seq": 0}, &probe); oerr != nil {
			t.Fatalf("burst poll %d: %v", i, oerr)
		}
		for _, ev := range probe.Events {
			if ev.Event == EventOverflow && ev.Seq > 0 {
				rotated = true
			}
		}
	}
	if !rotated {
		t.Fatalf("the journal never rotated past the client's cursor within %d burst iterations: the cursor cannot be proven stale, so the arms below would measure nothing", burstMax)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "util.go"), []byte("package main\n\nfunc util() { /* moved */ }\n"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if err := inv.pollEventsOnce(ctx); err != nil {
		t.Fatalf("poll after rotation: %v", err)
	}
	st := inv.State()
	if st.Gaps <= before.Gaps {
		t.Fatalf("the client fell behind the retained journal and recorded no gap: %+v (%s)", st, rec.summary())
	}
	if st.Resyncs == 0 {
		t.Fatalf("a gap the client CAN read must still force its resync: %s", rec.summary())
	}
}
