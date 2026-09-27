package fsclient

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-037 — the cells. Each one names the requirement it grades, and each one
// is written so that a specific MUTATION turns it RED (the arms script applies
// them, `docs/evidence/BFS-037-mutations/`):
//
//	C01  THE SIZE RULE AT THE BOUNDARY      (rows: S-2, S-3, S-4, S-8)
//	C02  THE BOUNDED REFRESH POOL + THE STARVATION MEASUREMENT (P-3..P-9)
//	C03  STOP IN FULL WHILE ITEMS ARE IN FLIGHT (Q-12, §5.5)
//	C04  PROMOTION: LEAVES THE QUEUE, SLEEPS NOT, FETCHES ONCE (PR-1/PR-5)
//	C05  NO READER EVER SEES A PARTIAL FILE (BFS-038 ∩ the refresh)
//	C06  WITH THE FEATURE OFF THE CLIENT IS CORRECT (BFS-044's law, P-0/P-1)
//	C07  THE HANG-UP RULES: the deadline, the ladder, the yield (P-10..P-12)
//	C08  THE SKIP/ABANDON CENSUS, one arm per reason (S-12/P-13, AC-6)
//
// The four cells BFS-046 wrote as PENDING-UNTIL-BFS-037 (05 stampede, 06 stop,
// 07 promotion, 09 census) are completed in bfs046_program_test.go against the
// same fixture and the same claims; these cells are the row's own list.
// ---------------------------------------------------------------------------

// ── C01 ────────────────────────────────────────────────────────────────────

// TestBFS037Cell01TheSizeRuleHoldsAtTheBoundary is the boundary cell. It grades
// S-2 (`size <= hot_max_file_bytes` IS refreshable), S-3 (the cheap pre-check
// costs ZERO requests), S-4 (the post-HEAD re-check refuses BEFORE a byte is
// pulled) and S-8 (the over-ceiling path is untracked, so a stale score can
// never refresh it later).
//
// The defect it catches: the classic off-by-one at a limit, and its worse
// sibling — a bound that is checked only where it costs nothing. Every byte
// assertion here is a SERVER figure, not an inference.
func TestBFS037Cell01TheSizeRuleHoldsAtTheBoundary(t *testing.T) {
	const ceiling = 4096
	stub := newHotStub(t)
	stub.Throttle(0)
	s := newHotSetup(t, stub,
		WithCeiling(ceiling),
		WithTick(2*time.Millisecond),
		WithPolicy(func(p *HotPolicy) { p.QueueMaxWait = time.Hour }),
	)
	// Three tracked, cached paths: exactly at the ceiling, one byte over, and one
	// whose LOCAL size says it is fine while the SERVED size has grown.
	stub.Set("at.txt", hotBody(ceiling, 'a'))
	stub.Set("over.txt", hotBody(ceiling+1, 'b'))
	stub.Set("grown.txt", hotBody(ceiling/2, 'c'))
	for _, p := range []string{"at.txt", "over.txt", "grown.txt"} {
		s.seedRead(p, true)
	}
	st := s.Manager.Stats()
	if !st.SizeRuleInclusive {
		t.Fatalf("the running policy does not report the size rule as inclusive (S-2): %+v", st)
	}
	if st.MaxFileBytes != ceiling {
		t.Fatalf("the running policy's size rule is %d, want the configured %d", st.MaxFileBytes, ceiling)
	}
	before := hotCensus(s.Manager)
	servedBefore := map[string]int{}
	for _, p := range []string{"at.txt", "over.txt", "grown.txt"} {
		servedBefore[p] = stub.BytesServed(p)
	}
	getsBefore := map[string]int{}
	headsBefore := map[string]int{}
	for _, p := range []string{"at.txt", "over.txt", "grown.txt"} {
		getsBefore[p] = stub.GETs(p)
		headsBefore[p] = stub.HEADs(p)
	}
	// The file GREW on the server after the client's last read: the local size is
	// stale, the served size is over the ceiling. This is R5's exact case.
	stub.Set("grown.txt", hotBody(ceiling*2, 'd'))

	s.Manager.NoteInvalidation([]string{"at.txt", "over.txt", "grown.txt"})
	if !hotWaitFor(t, "the at-ceiling refresh to fetch", 3*time.Second, func() bool {
		return stub.GETs("at.txt") > getsBefore["at.txt"]
	}) {
		t.Fatalf("a file EXACTLY at the ceiling was not refreshed (S-2): gets=%d", stub.GETs("at.txt"))
	}
	hotTick(20)
	after := hotCensus(s.Manager)
	st = s.Manager.Stats()

	// (a) AT THE CEILING IS REFRESHABLE, and it really was re-read.
	if n := stub.GETs("at.txt") - getsBefore["at.txt"]; n != 1 {
		t.Fatalf("the at-ceiling path was fetched %d times, want exactly 1", n)
	}
	// (b) ONE BYTE OVER IS NOT, and the pre-check cost ZERO REQUESTS: not a GET,
	// not even a HEAD (S-3).
	if d := hotDelta(before, after, "skip:"+HotSkipOversizePre); d < 1 {
		t.Fatalf("a path one byte over the ceiling did not produce a skip (delta %d): %+v", d, after)
	}
	if got := stub.GETs("over.txt") - getsBefore["over.txt"]; got != 0 {
		t.Fatalf("ceiling+1 pulled a GET: %d", got)
	}
	if got := stub.HEADs("over.txt") - headsBefore["over.txt"]; got != 0 {
		t.Fatalf("ceiling+1 cost a HEAD: the pre-check must decide before any request (S-3), got %d", got)
	}
	if n := stub.BytesServed("over.txt") - servedBefore["over.txt"]; n != 0 {
		t.Fatalf("ceiling+1 served %d body bytes; the size rule must never be a request-shaped hope", n)
	}
	// (c) THE POST-HEAD RE-CHECK (S-4) is the one that matters: the local size
	// said 2048, the served size said 8192 — refused BEFORE A BYTE.
	if d := hotDelta(before, after, "skip:"+HotSkipOversizePreHead); d < 1 {
		t.Fatalf("the grown file was not refused after its HEAD (delta %d): %+v", d, after)
	}
	if n := stub.BytesServed("grown.txt") - servedBefore["grown.txt"]; n != 0 {
		t.Fatalf("the grown file served %d body bytes: R5's exact failure is a refresh that has already issued the GET", n)
	}
	if got := stub.HEADs("grown.txt") - headsBefore["grown.txt"]; got != 1 {
		t.Fatalf("the grown file's HEAD count is %d, want exactly 1 (the size authority is the HEAD, S-5)", got)
	}
	// (d) AND IT IS UNTRACKED (S-8): a stale score cannot refresh it later.
	if s.Manager.Tracked("grown.txt") {
		t.Fatal("a path that grew past the ceiling must be UNTRACKED at its next touch (S-8), or a stale score refreshes it forever")
	}
	// The at-ceiling path, by contrast, is still tracked and still cached.
	if !s.Manager.Tracked("at.txt") {
		t.Fatal("the at-ceiling path must stay tracked: it is refreshable")
	}
	t.Logf("boundary: ceiling=%d at_ceiling_fetches=%d over_skips=%d over_head_cost=%d over_body_bytes=%d grown_body_bytes=%d grown_heads=%d skips={oversize_pre:%d oversize_after_head:%d}",
		ceiling, stub.GETs("at.txt")-getsBefore["at.txt"],
		after["skip:"+HotSkipOversizePre],
		stub.HEADs("over.txt")-headsBefore["over.txt"], stub.BytesServed("over.txt")-servedBefore["over.txt"],
		stub.BytesServed("grown.txt")-servedBefore["grown.txt"], stub.HEADs("grown.txt")-headsBefore["grown.txt"],
		after["skip:"+HotSkipOversizePre], after["skip:"+HotSkipOversizePreHead])
}

// ── C02 ────────────────────────────────────────────────────────────────────

// hotForegroundLatencies runs `readers` concurrent foreground readers for `d`
// and returns every request's WALL TIME. The foreground's own numbers are what
// P-9 measures: not a refresh count, not an aggregate.
func hotForegroundLatencies(t *testing.T, s *hotSetup, readers int, d time.Duration) []time.Duration {
	t.Helper()
	var mu sync.Mutex
	var out []time.Duration
	stop := time.Now().Add(d)
	var wg sync.WaitGroup
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; time.Now().Before(stop); i++ {
				p := fmt.Sprintf("fg/%02d.txt", (r*5+i)%16)
				start := time.Now()
				_, _, _, _ = s.Manager.HotRead(context.Background(), p, "")
				el := time.Since(start)
				mu.Lock()
				out = append(out, el)
				mu.Unlock()
			}
		}(r)
	}
	wg.Wait()
	return out
}

// hotPct is a percentile of a latency sample.
func hotPct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	cp := append([]time.Duration(nil), ds...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(float64(len(cp)-1) * p)
	return cp[idx]
}

// hotMax is the largest sample.
func hotMax(ds []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range ds {
		if d > m {
			m = d
		}
	}
	return m
}

// TestBFS037Cell02TheRefreshPoolIsBoundedAndYields is requirement 2 of the row:
// a mass invalidation must not starve main reads and writes, the refresh work
// runs through a BOUNDED pool, and the yield is PROVEN by a measurement.
//
// THE MEASUREMENT: the FOREGROUND's own latency distribution (p50/p95/max, wall
// time, against a server that is slow to send HEADERS — the interval in which a
// client holds a pool slot) with the refresh queue IDLE and then SATURATED with
// tracked candidates. The refresh budget is a SEPARATE bounded budget (P-3/P-6),
// so the two distributions must be the same, and the refresh's own concurrency,
// measured AT THE SERVER, must never exceed min(configured cap, pool share).
func TestBFS037Cell02TheRefreshPoolIsBoundedAndYields(t *testing.T) {
	const conc = 8
	stub := newHotStub(t)
	s := newHotSetup(t, stub,
		WithConcurrency(conc),
		WithTick(2*time.Millisecond),
		WithPolicy(func(p *HotPolicy) { p.YieldAfter = 50 * time.Millisecond; p.QueueMaxWait = time.Hour }),
	)
	var candidates []string
	for i := 0; i < 64; i++ {
		p := fmt.Sprintf("hot/%02d.txt", i)
		stub.Set(p, hotBody(4096, byte('a'+i%26)))
		s.seedRead(p, false) // tracked: a refresh candidate (D-2)
		candidates = append(candidates, p)
	}
	for i := 0; i < 16; i++ {
		stub.Set(fmt.Sprintf("fg/%02d.txt", i), hotBody(4096, byte('A'+i)))
	}
	// Only NOW does the server get slow: the seeding above must not pay the
	// latency the measurement is about.
	stub.HeadDelay(60 * time.Millisecond)
	stub.Throttle(60 * time.Millisecond)

	quiet := hotForegroundLatencies(t, s, 4, 2*time.Second)
	at := hotCensus(s.Manager)
	s.Manager.NoteInvalidation(candidates)
	if !hotWaitFor(t, "a refresh to be in flight under the burst", 5*time.Second, func() bool {
		return s.Manager.Stats().RefreshInflight > 0
	}) {
		t.Fatalf("no refresh started against %d invalidated tracked paths", len(candidates))
	}
	saturated := hotForegroundLatencies(t, s, 4, 2*time.Second)
	st := s.Manager.Stats()
	after := hotCensus(s.Manager)
	refreshMax := 0
	for _, p := range candidates {
		if m := stub.PathMax(p); m > refreshMax {
			refreshMax = m
		}
	}
	if len(quiet) == 0 || len(saturated) == 0 {
		t.Fatalf("no foreground samples: quiet=%d saturated=%d", len(quiet), len(saturated))
	}
	// THE CLAIM. A saturated queue must not move the foreground's distribution.
	quietP95, satP95 := hotPct(quiet, 0.95), hotPct(saturated, 0.95)
	if satP95 > quietP95*3/2+25*time.Millisecond {
		t.Fatalf("a saturated refresh queue starved the foreground: quiet p95=%s p99=%s max=%s · saturated p95=%s max=%s (P-6/P-9: a refresh must never delay a foreground request)",
			quietP95, hotPct(quiet, 0.99), hotMax(quiet), satP95, hotMax(saturated))
	}
	if refreshMax > st.RefreshMaxInflight {
		t.Fatalf("the refresher ran %d concurrent refreshes at the SERVER, over the governor %d (Q-7/P-4)", refreshMax, st.RefreshMaxInflight)
	}
	if st.RefreshInflightMax > st.RefreshMaxInflight {
		t.Fatalf("refresh_inflight_max=%d exceeds refresh_max_inflight=%d", st.RefreshInflightMax, st.RefreshMaxInflight)
	}
	// THE SHARE IS A SHARE AT THE REQUEST LAYER TOO, not only in the governor's
	// arithmetic: the client's second budget channel must be sized to exactly the
	// governor's width. A channel sized to anything else is a share that is not
	// one — the bound that does not bound (BFS-031) — even when the governor
	// happens to keep the peak low.
	if got := s.Client.RefreshBudget(); got != st.RefreshMaxInflight {
		t.Fatalf("the client's refresh budget has %d slot(s) against a governor of %d: the pool share is not the number the requests are actually bounded by (P-3/P-4)", got, st.RefreshMaxInflight)
	}
	// And it is NOT the foreground pool: the two budgets are separate channels.
	if s.Client.RefreshBudget() >= conc {
		t.Fatalf("the refresh budget (%d) is not smaller than the foreground pool (%d): the share is not a fraction of it", s.Client.RefreshBudget(), conc)
	}
	if d := hotDelta(at, after, "skip:"+HotSkipQueueFull); d != 0 {
		t.Logf("queue refusals under the burst: %d", d)
	}
	// The share arithmetic itself, at the default pool: 1/8 of 25 is 3, which
	// leaves foreground 22 of 25 (88%).
	pol := DefaultHotPolicy()
	pol.Enabled = true
	env25 := HotPolicyEnv{Concurrency: 25, OpTimeout: 30 * time.Second, CacheMaxBytes: 1 << 28, CacheMaxEntryBytes: 1 << 26}
	eff := pol.Effective(env25)
	if eff.Derived.PoolSlots != 3 || eff.Derived.PoolSlotsForeground != 22 {
		t.Fatalf("the share at the default pool is %d slots with %d for the foreground, want 3/22 (P-3)",
			eff.Derived.PoolSlots, eff.Derived.PoolSlotsForeground)
	}
	// And at THIS pool the configured cap of 2 is CLAMPED to the share's 1 —
	// reported, never refused (P-4).
	if eff8 := pol.Effective(HotPolicyEnv{Concurrency: conc, OpTimeout: 30 * time.Second, CacheMaxBytes: 1 << 28, CacheMaxEntryBytes: 1 << 26}); eff8.Derived.PoolSlots != 1 || eff8.Derived.RefreshMaxInflight != 1 || !eff8.Derived.RefreshMaxInflightClamped {
		t.Fatalf("at pool=%d the governor resolved to slots=%d inflight=%d clamped=%v, want 1/1/clamped",
			conc, eff8.Derived.PoolSlots, eff8.Derived.RefreshMaxInflight, eff8.Derived.RefreshMaxInflightClamped)
	}
	if st.PoolSlotsForeground != conc-st.PoolSlots {
		t.Fatalf("the record's foreground floor is %d, want %d-%d", st.PoolSlotsForeground, conc, st.PoolSlots)
	}
	t.Logf("starvation: pool=%d share=%d governor=%d quiet(n=%d) p50=%s p95=%s max=%s · saturated(n=%d) p50=%s p95=%s max=%s · refresh_path_max=%d refresh_inflight_max=%d foreground_slot_wait_max_ms=%d",
		conc, st.PoolSlots, st.RefreshMaxInflight,
		len(quiet), hotPct(quiet, 0.5), quietP95, hotMax(quiet),
		len(saturated), hotPct(saturated, 0.5), satP95, hotMax(saturated),
		refreshMax, st.RefreshInflightMax, s.Client.WaitMaxMS())
}

// ── C03 ────────────────────────────────────────────────────────────────────

// TestBFS037Cell03StopInFullWhileItemsAreInFlight is requirement 3: the queue is
// stopPABLE IN FULL — not drained-and-then-stopped — with items queued AND in
// flight. The two readings the spec refuses are both graded here: a
// drain-then-idle stop fails (i) and (ii); a let-in-flight-finish stop fails
// (iii). Clause (iv) grades the exception: a PROMOTED fetch survives.
func TestBFS037Cell03StopInFullWhileItemsAreInFlight(t *testing.T) {
	stub := newHotStub(t)
	stub.Throttle(80 * time.Millisecond)
	s := newHotSetup(t, stub,
		WithTick(2*time.Millisecond),
		WithPolicy(func(p *HotPolicy) { p.QueueMaxWait = time.Hour; p.StopDeadline = 2 * time.Second }),
	)
	// ONE path is cached and about to be refreshed against a held body; 60 more
	// are tracked and queued, so the queue is genuinely full of work when the
	// stop lands.
	victim := "victim.txt"
	old := hotBody(4096, 'v')
	stub.Set(victim, old)
	s.seedRead(victim, true)
	oldHash, _, ok := s.Cache.Lookup(victim)
	if !ok {
		t.Fatal("the fixture's seeded read must be cached (a refresh is measured against what is published)")
	}
	var queued []string
	for i := 0; i < 60; i++ {
		p := fmt.Sprintf("cold/%03d.txt", i)
		stub.Set(p, hotBody(2048, byte('a'+i%26)))
		s.seedRead(p, false)
		queued = append(queued, p)
	}
	usedBefore := s.Cache.Stats().UsedBytes
	stub.Set(victim, hotBody(4096, 'w')) // the file CHANGED on the server
	stub.Hold()

	s.Manager.NoteInvalidation(append([]string{victim}, queued...))
	if !hotWaitFor(t, "the victim's refresh to be in flight", 3*time.Second, func() bool {
		return stub.GETs(victim) > 0 && s.Manager.Stats().RefreshInflight > 0
	}) {
		t.Fatalf("the victim's refresh never started: gets=%d inflight=%d", stub.GETs(victim), s.Manager.Stats().RefreshInflight)
	}
	if d := s.Manager.Stats().Queue.Depth; d < 40 {
		t.Fatalf("the queue holds %d items, want the invalidation burst to still be pending when the stop lands", d)
	}
	before := hotCensus(s.Manager)
	stopStart := time.Now()

	// THE STOP. It must take effect WHILE items are queued and in flight.
	s.Manager.Stop(HotStopOperator)
	stopElapsed := time.Since(stopStart)
	after := hotCensus(s.Manager)
	st := s.Manager.Stats()

	// (ii) the queue is EMPTIED at once, and every dropped item is COUNTED by
	// reason `stopped` — a drain-then-idle stop cannot pass this.
	if st.Queue.Depth != 0 {
		t.Fatalf("the queue depth is %d immediately after the stop: STOP IN FULL empties the queue at once (§5.5 clause 2)", st.Queue.Depth)
	}
	if d := hotDelta(before, after, "skip:"+HotSkipStopped); d < 40 {
		t.Fatalf("only %d queued items were counted against reason `stopped`; every item the stop empties must be counted", d)
	}
	// (iii) the in-flight refresh is ABANDONED: no publish, no temp blob, no
	// reservation, no slot.
	if d := hotDelta(before, after, "abandon:"+HotAbandonStopped); d < 1 {
		t.Fatalf("the in-flight refresh was not abandoned at its checkpoint (delta %d): a stop that lets in-flight work finish is unbounded in time", d)
	}
	if st.RefreshInflight != 0 {
		t.Fatalf("the refresher still reports %d in flight after the stop deadline", st.RefreshInflight)
	}
	if st.StopDeadlineExceededTotal != 0 {
		t.Fatalf("the stop needed more than its declared deadline: %d", st.StopDeadlineExceededTotal)
	}
	if stopElapsed > 2*time.Second {
		t.Fatalf("the stop took %s to be in force", stopElapsed)
	}
	cs := s.Cache.Stats()
	if cs.InFlightBytes != 0 || cs.StagedBlobs != 0 {
		t.Fatalf("the abandoned refresh left bytes in flight: in_flight=%d staged=%d (§5.6 obligation (c))", cs.InFlightBytes, cs.StagedBlobs)
	}
	if n := stageResidue(t, s.Cache); n != 0 {
		t.Fatalf("%d unpublished stage files survived the abandon (§5.6 obligation (a))", n)
	}
	if cs.UsedBytes != usedBefore {
		t.Fatalf("used_bytes moved during the stop (%d → %d): an abandoned refresh must never publish", usedBefore, cs.UsedBytes)
	}
	nowHash, _, ok := s.Cache.Lookup(victim)
	if !ok || nowHash != oldHash {
		t.Fatalf("the victim's published identity moved during the abandon: %q → %q (Q-16: a reader's bytes and the cache's entry hash are bit-identical)", oldHash, nowHash)
	}
	// The reader's satisfied expectation DURING and AFTER: the old, complete
	// content, served from the cache the pin still holds.
	if data, ok := s.Cache.Get(victim, oldHash); !ok || HashBytes(data) != oldHash {
		t.Fatalf("the reader's bytes moved under the abandoned refresh: ok=%v hash=%s", ok, HashBytes(data))
	}
	// (i) new work is REFUSED while stopped, AND COUNTED — level-triggered.
	refusedBefore := st.RefusedWhileStoppedTotal
	s.Manager.NoteInvalidation(queued[:5])
	hotTick(6)
	st2 := s.Manager.Stats()
	if st2.RefusedWhileStoppedTotal <= refusedBefore {
		t.Fatalf("an invalidation arriving after the stop was accepted: refused_while_stopped=%d (a stop that silently lifts is not a stop)", st2.RefusedWhileStoppedTotal)
	}
	if st2.Queue.Depth != 0 {
		t.Fatalf("a refused invalidation still created queued work: depth=%d", st2.Queue.Depth)
	}
	// A READ is never refused — only the speculation is (§5.5 clause 3, D-3).
	// The hold gate is released first so this read is an ordinary fast read.
	stub.Release()
	stub.Set("fg-while-stopped.txt", hotBody(64, 'z'))
	if _, _, oerr, _ := s.Manager.HotRead(context.Background(), "fg-while-stopped.txt", ""); oerr != nil {
		t.Fatalf("a read was refused while the hot path was stopped: %v (the prohibition is on SPECULATION, never on service)", oerr)
	}
	t.Logf("stop: elapsed=%s queue_depth_after=%d stopped_skips_delta=%d stopped_abandons_delta=%d in_flight_bytes=%d staged=%d stage_residue=%d used_bytes=%d(unmoved) refused_while_stopped=%d",
		stopElapsed, st.Queue.Depth, hotDelta(before, after, "skip:"+HotSkipStopped),
		hotDelta(before, after, "abandon:"+HotAbandonStopped), cs.InFlightBytes, cs.StagedBlobs,
		stageResidue(t, s.Cache), cs.UsedBytes, st2.RefusedWhileStoppedTotal)
}

// ── C04 ────────────────────────────────────────────────────────────────────

// TestBFS037Cell04PromotionLeavesTheQueueAndDoesNotSleep is requirement 5: a
// queued refresh that a process asks for DIRECTLY is PROMOTED TO FULL — it
// leaves the queue, it does not sleep, and the work happens exactly once.
func TestBFS037Cell04PromotionLeavesTheQueueAndDoesNotSleep(t *testing.T) {
	stub := newHotStub(t)
	stub.Throttle(0)
	// A tick long enough that NOTHING starts the queued refresh on its own: the
	// promotion is the only thing that can take it, which is what makes "it left
	// the queue" a claim about the promotion rather than about the refresher.
	s := newHotSetup(t, stub, WithTick(time.Hour), WithPolicy(func(p *HotPolicy) { p.BackoffJitter = HotJitterNone }))
	target := "edit/one.txt"
	stub.Set(target, hotBody(4096, 'p'))
	s.seedRead(target, false) // tracked, NOT cached: the read must really fetch

	s.Manager.NoteInvalidation([]string{target})
	if d := s.Manager.Stats().Queue.Depth; d != 1 {
		t.Fatalf("the invalidation did not queue the tracked path: depth=%d", d)
	}
	before := s.Manager.Stats()
	gets := stub.GETs(target)
	start := time.Now()
	data, hash, oerr, joined := s.Manager.HotRead(context.Background(), target, "")
	elapsed := time.Since(start)
	after := s.Manager.Stats()

	if oerr != nil {
		t.Fatalf("the promoted read failed: %v", oerr)
	}
	if joined {
		t.Fatal("this read must be the LEADER of its fetch (nothing was in flight), not a joiner")
	}
	if HashBytes(data) != hash {
		t.Fatalf("the promoted read returned bytes that do not hash to its own hash")
	}
	if after.Queue.Promoted != before.Queue.Promoted+1 {
		t.Fatalf("the promotion was not counted: promoted=%d → %d (PR-12: a distinct counter, never expired/displaced/stopped)",
			before.Queue.Promoted, after.Queue.Promoted)
	}
	if after.PromotionsTotal != before.PromotionsTotal+1 {
		t.Fatalf("promotions_total did not move: %d → %d", before.PromotionsTotal, after.PromotionsTotal)
	}
	if after.Queue.Depth != 0 {
		t.Fatalf("the queue depth is still %d: the promoted item must LEAVE the queue", after.Queue.Depth)
	}
	if d := stub.GETs(target) - gets; d != 1 {
		t.Fatalf("the promoted read issued %d GETs, want exactly 1 (PR-6: promotion must not double-fetch)", d)
	}
	// "DOES NOT SLEEP" is a measurement: the read's wall time is ONE fetch, not a
	// fetch behind the backoff ladder (250 ms at the first rung).
	if elapsed > time.Duration(before.BackoffBaseMS)*time.Millisecond {
		t.Fatalf("the promoted read took %s: it waited behind the refresh's backoff (%d ms at the first rung) instead of going to the front", elapsed, before.BackoffBaseMS)
	}
	if after.RetriesTotal != before.RetriesTotal || after.BackoffCurrentMS != 0 {
		t.Fatalf("a backoff was applied to a promoted fetch: retries %d → %d, backoff_current_ms=%d (PR-1: no backoff, no refresh slot, no queue order)",
			before.RetriesTotal, after.RetriesTotal, after.BackoffCurrentMS)
	}
	if after.RefreshesTotal != before.RefreshesTotal {
		t.Fatalf("the refresher also ran the promoted path (refreshes %d → %d): the work must happen ONCE",
			before.RefreshesTotal, after.RefreshesTotal)
	}
	t.Logf("promotion: elapsed=%s queue_depth=%d promoted=%d +%d promotions=%d gets=+%d retries=%d backoff_ms=%d elapsed_is_one_fetch=%v",
		elapsed, after.Queue.Depth, after.Queue.Promoted, 1, after.PromotionsTotal, stub.GETs(target)-gets,
		after.RetriesTotal, after.BackoffCurrentMS, elapsed < time.Duration(before.BackoffBaseMS)*time.Millisecond)
}

// TestBFS037Cell04bPromotionJoinsAnInFlightFetchInsteadOfRefetching is the other
// half of AC-11: with the refresh ALREADY fetching that path, 32 concurrent
// readers must produce ZERO extra GETs — they join the one fetch in flight, and
// every waiter receives the same answer (PR-5/PR-9).
func TestBFS037Cell04bPromotionJoinsAnInFlightFetchInsteadOfRefetching(t *testing.T) {
	stub := newHotStub(t)
	stub.Throttle(0)
	s := newHotSetup(t, stub, WithTick(2*time.Millisecond), WithPolicy(func(p *HotPolicy) { p.RefreshDeadline = 25 * time.Second }))
	target := "shared/one.txt"
	stub.Set(target, hotBody(8192, 'j'))
	s.seedRead(target, false)
	seedGets := stub.GETs(target)
	stub.Hold()
	before := s.Manager.Stats()
	s.Manager.NoteInvalidation([]string{target})
	// The wait is on the REFRESH's own fetch being in flight (a leader counted,
	// the queue drained into it) — not merely on a GET count, which the seed
	// read already satisfied. A reader that raced the refresh would become the
	// leader itself, and the cell would then be measuring a different thing.
	if !hotWaitFor(t, "the refresh's fetch to be the leader", 5*time.Second, func() bool {
		st := s.Manager.Stats()
		return st.RefreshInflight == 1 && st.SingleflightLeadersTotal > before.SingleflightLeadersTotal && stub.GETs(target) == seedGets+1
	}) {
		t.Fatalf("the refresh's own GET never became the leader: gets=%d (seed %d) inflight=%d",
			stub.GETs(target), seedGets, s.Manager.Stats().RefreshInflight)
	}
	before = s.Manager.Stats()
	const readers = 32
	var wg sync.WaitGroup
	got := make([][]byte, readers)
	start := make(chan struct{})
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			data, _, _, _ := s.Manager.HotRead(context.Background(), target, "")
			got[i] = data
		}(i)
	}
	close(start)
	// Wait until every reader has JOINED before releasing the body: the claim is
	// about readers that arrive while the fetch is in flight, and a reader
	// scheduled after the fetch closed would legitimately be a second leader.
	if !hotWaitFor(t, "all readers to join the in-flight fetch", 20*time.Second, func() bool {
		return s.Manager.Stats().SingleflightJoinsTotal-before.SingleflightJoinsTotal >= readers
	}) {
		t.Fatalf("only %d of %d readers joined the in-flight fetch (joins=%d)",
			s.Manager.Stats().SingleflightJoinsTotal-before.SingleflightJoinsTotal, readers,
			s.Manager.Stats().SingleflightJoinsTotal)
	}
	stub.Release()
	wg.Wait()
	after := s.Manager.Stats()

	if d := stub.GETs(target) - (seedGets + 1); d != 0 {
		t.Fatalf("%d readers produced %d extra GETs over the refresh's own single GET (seed %d, total %d): promotion and a refresh must share ONE fetch (PR-6)", readers, d, seedGets, stub.GETs(target))
	}
	if d := after.SingleflightJoinsTotal - before.SingleflightJoinsTotal; d != readers {
		t.Fatalf("joins moved by %d, want %d (one per reader that shared the fetch)", d, readers)
	}
	if after.SingleflightEntries != 0 {
		t.Fatalf("the single-flight map still holds %d entries at rest: a map that grows on errors is a leak in the component whose purpose is to bound concurrency (PR-8)", after.SingleflightEntries)
	}
	for i, d := range got {
		if d == nil {
			t.Fatalf("reader %d received no bytes", i)
		}
		if HashBytes(d) != HashBytes(got[0]) {
			t.Fatalf("readers of ONE fetch disagreed about its content (PR-9): reader %d hashes differently", i)
		}
	}
	t.Logf("join: readers=%d wire_gets=%d extra_gets=0 joins_delta=%d leaders_delta=%d singleflight_entries=%d all_waiters_agree=%v",
		readers, stub.GETs(target), after.SingleflightJoinsTotal-before.SingleflightJoinsTotal,
		after.SingleflightLeadersTotal-before.SingleflightLeadersTotal, after.SingleflightEntries, HashBytes(got[0]))
}

// ── C05 ────────────────────────────────────────────────────────────────────

// TestBFS037Cell05NoReaderEverSeesAPartialFile is the row's highest-risk cell:
// a file under refresh must not scare a reader. While the refresh is MID-BODY:
//
//   - every read that hits the cache gets the OLD, COMPLETE content, byte for
//     byte (never a prefix, never a mix, never a truncation);
//   - used_bytes does not move (unpublished bytes are not occupancy);
//   - the published blob on disk is untouched (no in-place rewrite);
//   - and once the refresh commits, the next read gets the NEW complete content.
//
// The guarantee is BFS-038's (a new immutable blob and ONE pointer swap); this
// cell is that guarantee MEETING the refresh system, which is where a
// mutate-in-place refresh would show up as a reader holding half a file.
func TestBFS037Cell05NoReaderEverSeesAPartialFile(t *testing.T) {
	stub := newHotStub(t)
	stub.Throttle(0)
	s := newHotSetup(t, stub, WithTick(2*time.Millisecond), WithPolicy(func(p *HotPolicy) { p.RefreshDeadline = 25 * time.Second }))
	target := "src/main.go"
	oldBody := hotBody(8192, 'o')
	newBody := hotBody(8192, 'n')
	stub.Set(target, oldBody)
	s.seedRead(target, true)
	oldHash, _, ok := s.Cache.Lookup(target)
	if !ok || oldHash != HashBytes(oldBody) {
		t.Fatalf("the seeded read must cache the OLD content: hash=%q want %q", oldHash, HashBytes(oldBody))
	}
	usedBefore := s.Cache.Stats().UsedBytes

	// The file changes on the server, and the refresh stalls INSIDE the body —
	// the exact window a mutating refresh would be visible in.
	stub.Set(target, newBody)
	stub.Hold()
	servedBase := stub.BytesServed(target)
	s.Manager.NoteInvalidation([]string{target})
	// The gate must catch THE REFRESH parked inside its body: the seed read has
	// already served this path's bytes, so "served > 0" alone would be satisfied
	// before the refresh ever started and the whole cell would be measuring
	// nothing. Only a PARTIAL delta belongs to the in-flight refresh.
	if !hotWaitHeld(t, stub, target, len(newBody), servedBase, 5*time.Second) {
		t.Fatalf("the refresh never reached its mid-body window: served=%d (base %d) inflight=%d",
			stub.BytesServed(target), servedBase, s.Manager.Stats().RefreshInflight)
	}

	// WHILE IT IS IN FLIGHT: read the path the way the mount does — lookup, then
	// pinned get — many times, and hold every answer to the same standard.
	hits, misses := 0, 0
	for i := 0; i < 50; i++ {
		h, _, ok := s.Cache.Lookup(target)
		if !ok {
			misses++
			continue
		}
		data, ok := s.Cache.GetPinned(target, h)
		if !ok {
			misses++
			continue
		}
		hits++
		if h != oldHash {
			t.Fatalf("iteration %d: the cache's identity for a path under refresh moved to %q while the refresh is unpublished", i, h)
		}
		if int64(len(data)) != int64(len(oldBody)) {
			t.Fatalf("iteration %d: a reader got %d bytes under refresh, want the old COMPLETE %d — a reader must never see a partial file", i, len(data), len(oldBody))
		}
		if HashBytes(data) != oldHash {
			t.Fatalf("iteration %d: a reader's bytes hash to %s, not the old complete content %s", i, HashBytes(data), oldHash)
		}
		s.Cache.Unpin(h)
	}
	if hits == 0 {
		t.Fatal("no read hit the cache during the refresh: the cell proves nothing about a reader under refresh")
	}
	// The blob ON DISK is still the old content: the refresh has not touched it.
	onDisk, err := os.ReadFile(filepath.Join(s.Cache.Dir(), CacheBlobDir, oldHash[len(HashPrefix):]))
	if err != nil {
		t.Fatalf("the published blob is not readable during the refresh: %v", err)
	}
	if HashBytes(onDisk) != oldHash {
		t.Fatalf("the published blob on disk hashes to %s, not %s: the refresh rewrote content a reader may hold", HashBytes(onDisk), oldHash)
	}
	cs := s.Cache.Stats()
	if cs.UsedBytes != usedBefore {
		t.Fatalf("used_bytes moved to %d while the refresh was mid-body: unpublished bytes are not occupancy (Q-8/Q-10)", cs.UsedBytes)
	}
	midLogged := fmt.Sprintf("mid-refresh: hits=%d misses=%d old_hash=%s used_bytes=%d(unmoved) inflight_bytes=%d staged=%d",
		hits, misses, oldHash[:14], cs.UsedBytes, cs.InFlightBytes, cs.StagedBlobs)

	// Release: the refresh completes, and NOW the new content is what a reader
	// gets — complete, and only after the pointer swap.
	stub.Release()
	if !hotWaitFor(t, "the refresh to publish", 5*time.Second, func() bool {
		h, _, ok := s.Cache.Lookup(target)
		return ok && h == HashBytes(newBody)
	}) {
		t.Fatalf("the refresh never published the new content: hash=%q want %q", func() string {
			h, _, _ := s.Cache.Lookup(target)
			return h
		}(), HashBytes(newBody))
	}
	data, ok := s.Cache.Get(target, HashBytes(newBody))
	if !ok || HashBytes(data) != HashBytes(newBody) || len(data) != len(newBody) {
		t.Fatalf("after the publish a reader did not get the new COMPLETE content: ok=%v len=%d hash=%s", ok, len(data), HashBytes(data))
	}
	after := s.Cache.Stats()
	if after.InFlightBytes != 0 || after.StagedBlobs != 0 {
		t.Fatalf("the refresh left bytes in flight after publishing: in_flight=%d staged=%d", after.InFlightBytes, after.StagedBlobs)
	}
	if n := stageResidue(t, s.Cache); n != 0 {
		t.Fatalf("%d stage files survived a committed refresh", n)
	}
	t.Logf("%s; after_publish: new_hash=%s len=%d used_bytes=%d inflight_bytes=%d stage_residue=%d",
		midLogged, HashBytes(newBody)[:14], len(data), after.UsedBytes, after.InFlightBytes, stageResidue(t, s.Cache))
}

// ── C06 ────────────────────────────────────────────────────────────────────

// TestBFS037Cell06WithTheFeatureOffTheClientIsCorrect is requirement 7 and
// BFS-044's law: with the hot cache disabled the client reads and writes the
// same bytes, nothing depends on the subsystem, and a cold tracker does NO WORK.
func TestBFS037Cell06WithTheFeatureOffTheClientIsCorrect(t *testing.T) {
	stub := newHotStub(t)
	stub.Throttle(0)
	body := hotBody(2048, 'f')
	stub.Set("a.txt", body)

	// (a) A DISABLED policy has no manager AT ALL: the mount's read path cannot
	// reach the subsystem, which is the strongest form of "off".
	dir := t.TempDir()
	cache, err := OpenCache(CacheConfig{Dir: dir, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 19, MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	cl, err := NewClient(Options{BaseURL: stub.URL(), Concurrency: 4, OpTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	off := DefaultHotPolicy() // Enabled == false
	if mgr := NewHotManager(HotManagerConfig{Policy: off, Client: cl, Cache: cache, TrackerDir: dir}); mgr != nil {
		t.Fatal("a disabled policy must produce NO manager: nothing else is a real 'off'")
	}
	// (b) A HOT PATH WITH NO CACHE is reported as such, not armed (D-6).
	on := DefaultHotPolicy()
	on.Enabled = true
	noCacheEnv := HotPolicyEnv{Concurrency: 4, OpTimeout: 30 * time.Second, CacheMaxBytes: 0}
	if got := on.Effective(noCacheEnv).ConfigState; got != HotConfigCacheDisabled {
		t.Fatalf("a policy with no cache reported config_state=%q, want %q (D-6)", got, HotConfigCacheDisabled)
	}
	// (c) A TYPED NIL MANAGER IS INERT: every entry point is a no-op, which is
	// what lets the mount call it unconditionally.
	var off2 *HotManager
	off2.NoteRead("a.txt")
	off2.NoteEdit("a.txt")
	off2.NoteInvalidation([]string{"a.txt"})
	if off2.Promote("a.txt") {
		t.Fatal("a nil manager promoted something")
	}
	if d, _, oerr, joined := off2.HotRead(context.Background(), "a.txt", ""); d != nil || oerr != nil || joined {
		t.Fatal("a nil manager must not fetch anything: the caller owns the read")
	}
	// (d) THE BYTES ARE THE SAME in both worlds, and a cold tracker does no work.
	plain, _, oerr := cl.Get(context.Background(), "a.txt", "")
	if oerr != nil {
		t.Fatalf("the plain read failed: %v", oerr)
	}
	stub2 := newHotStub(t)
	stub2.Set("a.txt", body)
	s := newHotSetup(t, stub2, WithTick(time.Hour))
	requestsBefore := stub2.Requests()
	if n := s.Manager.Stats().Tracker.Entries; n != 0 {
		t.Fatalf("cold start has %d tracker entries, want 0 (H-16)", n)
	}
	if stub2.Requests() != requestsBefore {
		t.Fatalf("arming the manager issued %d requests: a cold start does no walk (H-16/AC-2)", stub2.Requests()-requestsBefore)
	}
	// An invalidation of a path the client never read is an UNTRACKED skip and
	// still costs nothing: the tree never teaches the tracker (H-18/D-2).
	before := hotCensus(s.Manager)
	s.Manager.NoteInvalidation([]string{"never-read.txt"})
	s.Manager.NoteRead("a.txt")
	hot, hash, oerr, _ := s.Manager.HotRead(context.Background(), "a.txt", "")
	if oerr != nil {
		t.Fatalf("the hot read failed: %v", oerr)
	}
	if HashBytes(plain) != HashBytes(hot) || HashBytes(hot) != hash {
		t.Fatalf("the hot path changed what a read returns: plain=%s hot=%s (P-0: no read may return different bytes)",
			HashBytes(plain), HashBytes(hot))
	}
	after := hotCensus(s.Manager)
	if d := hotDelta(before, after, "skip:"+HotSkipUntracked); d != 1 {
		t.Fatalf("an invalidation of an unread path did not produce exactly one `untracked` skip (delta %d)", d)
	}
	if s.Manager.Tracked("never-read.txt") {
		t.Fatal("an invalidation CREATED a tracker entry: that is a walk by another name (H-18)")
	}
	// The tracker's own bounds are reported (H-23) — a bound the owner cannot see
	// is not a bound.
	st := s.Manager.Stats()
	if st.Tracker.MaxEntries != DefaultHotTrackerMaxEntries || st.Tracker.MaxBytes != DefaultHotTrackerMaxBytes || st.MaxFileBytes != s.Policy.MaxFileBytes {
		t.Fatalf("the tracker's bounds are not the pinned ones in the record: %+v", st.Tracker)
	}
	t.Logf("feature-off: manager_for_disabled_policy=nil no_cache_config=%s nil_manager_inert=true cold_entries=%d untracked_skip_delta=%d hot_bytes_equal_plain=%v tracker_bounds=%d/%d",
		HotConfigCacheDisabled, 0, hotDelta(before, after, "skip:"+HotSkipUntracked), HashBytes(hot) == HashBytes(plain),
		st.Tracker.MaxEntries, st.Tracker.MaxBytes)
}

// ── C07 ────────────────────────────────────────────────────────────────────

// TestBFS037Cell07TheHangUpRules covers the hang-up half of requirement 4: a
// refresh gives up rather than hold a slot, and the ladder it comes back behind.
//
// THE RULE, STATED: a refresh releases its slot and discards its partial buffer
// whenever (a) a foreground request has been waiting longer than hot_yield_after
// (it re-enters the queue at its score, behind a backoff — the YIELD), (b) its
// own hot_refresh_deadline elapses (the ABANDON), (c) it was put back and cannot
// start again within hot_refresh_reacquire_window (the ABANDON), or (d) a stop
// is in force. It is never permitted to hold the slot to finish.
func TestBFS037Cell07TheHangUpRules(t *testing.T) {
	stub := newHotStub(t)
	stub.Throttle(0)
	s := newHotSetup(t, stub,
		WithTick(2*time.Millisecond),
		WithPolicy(func(p *HotPolicy) {
			p.RefreshDeadline = 120 * time.Millisecond
			p.BackoffJitter = HotJitterNone
			p.QueueMaxWait = time.Hour
		}),
	)
	target := "hangs/one.txt"
	stub.Set(target, hotBody(8192, 'h'))
	s.seedRead(target, false)
	stub.Hold() // armed AFTER the seed read: the gate must catch the REFRESH, not the seed
	before := hotCensus(s.Manager)
	s.Manager.NoteInvalidation([]string{target})
	if !hotWaitFor(t, "the refresh to reach its deadline", 5*time.Second, func() bool {
		after := hotCensus(s.Manager)
		return hotDelta(before, after, "abandon:"+HotAbandonDeadline) >= 1
	}) {
		t.Fatalf("the refresh did not hang up at hot_refresh_deadline: %+v", hotCensus(s.Manager))
	}
	st := s.Manager.Stats()
	if st.RefreshInflight != 0 {
		t.Fatalf("the abandoned refresh still holds a slot: inflight=%d", st.RefreshInflight)
	}
	if cs := s.Cache.Stats(); cs.InFlightBytes != 0 || cs.StagedBlobs != 0 {
		t.Fatalf("the abandoned refresh left bytes in flight: %+v", cs)
	}
	if n := stageResidue(t, s.Cache); n != 0 {
		t.Fatalf("%d stage files survived the deadline abandon", n)
	}
	if _, _, ok := s.Cache.Lookup(target); ok {
		t.Fatal("an abandoned refresh must not publish: the path is cached although nothing complete was fetched")
	}
	// The ladder itself, asserted as NUMBERS with jitter off: 250 ms · 2ⁿ, capped
	// at 30 s.
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{{1, 250 * time.Millisecond}, {2, 500 * time.Millisecond}, {3, time.Second}, {4, 2 * time.Second}, {20, 30 * time.Second}} {
		if got := s.Manager.backoff(tc.attempt); got != tc.want {
			t.Fatalf("backoff(attempt=%d) = %s, want %s (P-10: 250ms·2ⁿ capped at 30s)", tc.attempt, got, tc.want)
		}
	}
	// The cap may not exceed the client's own operation deadline (A.11).
	if s.Manager.PolicyConfigured().BackoffMax > s.Client.Options().OpTimeout {
		t.Fatalf("the backoff cap %s exceeds the client's op timeout %s: a speculative fetch must never back off longer than the foreground would wait for the same bytes",
			s.Manager.PolicyConfigured().BackoffMax, s.Client.Options().OpTimeout)
	}
	t.Logf("hang-up: deadline_abandons=%d inflight=%d staged=%d stage_residue=%d published=%v ladder={250ms,500ms,1s,2s,...,30s}",
		hotDelta(before, hotCensus(s.Manager), "abandon:"+HotAbandonDeadline), st.RefreshInflight,
		s.Cache.Stats().StagedBlobs, stageResidue(t, s.Cache), false)
}

// TestBFS037Cell07bTheYieldReleasesTheSlotForInteractiveTraffic is the live
// yield: while a refresh is mid-body, a saturating foreground wave must make it
// RELEASE the slot at its next checkpoint, keep its rank in the queue, come back
// behind the ladder — and publish NOTHING from the abandoned body.
//
// The server is slow to send HEADERS here, because a pool slot is held from
// request to headers: without header latency the foreground never queues and
// there is no pressure for the yield rule to answer.
func TestBFS037Cell07bTheYieldReleasesTheSlotForInteractiveTraffic(t *testing.T) {
	stub := newHotStub(t)
	s := newHotSetup(t, stub,
		WithConcurrency(2),
		WithTick(250*time.Millisecond),
		WithPolicy(func(p *HotPolicy) {
			p.YieldAfter = 20 * time.Millisecond
			p.BackoffJitter = HotJitterNone
			p.QueueMaxWait = time.Hour
			p.RefreshReacquireWindow = time.Hour
			p.PoolPressureTicks = 1000
			p.RefreshDeadline = 20 * time.Second
		}),
	)
	target := "yield/one.txt"
	stub.Set(target, hotBody(8192, 'y'))
	seed := s.seedRead(target, false)
	seedHash := HashBytes(seed)
	var fg []string
	for i := 0; i < 8; i++ {
		p := fmt.Sprintf("yfg/%02d.txt", i)
		stub.Set(p, hotBody(4096, byte('A'+i)))
		fg = append(fg, p)
	}
	// From here the server is slow: 150 ms before the headers (the pool slot) and
	// a body spread over another 400 ms (the refresh's checkpoints).
	stub.HeadDelay(150 * time.Millisecond)
	stub.Throttle(400 * time.Millisecond)
	stub.Set(target, hotBody(8192, 'z')) // the content changed, so a publish would be visible
	s.Manager.NoteInvalidation([]string{target})
	if !hotWaitFor(t, "the refresh to be mid-body", 5*time.Second, func() bool {
		return stub.BytesServed(target) > 0 && s.Manager.Stats().RefreshInflight == 1
	}) {
		t.Fatalf("the refresh never got mid-body: served=%d inflight=%d", stub.BytesServed(target), s.Manager.Stats().RefreshInflight)
	}
	before := s.Manager.Stats()
	var wg sync.WaitGroup
	for i := 0; i < len(fg); i++ {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			_, _, _, _ = s.Manager.HotRead(context.Background(), p, "")
		}(fg[i])
	}
	depthAtYield := -1
	yielded := hotWaitFor(t, "the refresh to yield", 8*time.Second, func() bool {
		st := s.Manager.Stats()
		if st.YieldsTotal > before.YieldsTotal {
			depthAtYield = st.Queue.Depth
			return true
		}
		return false
	})
	// FREEZE the world the moment the yield is observed: the yielded item
	// re-enters the queue and the refresher may legitimately start it again on
	// the next tick, so a stop here is what makes "it published nothing" and
	// "it is still queued" assertions about the YIELD rather than about the
	// next attempt.
	s.Manager.Stop(HotStopOperator)
	wg.Wait()
	st := s.Manager.Stats()
	if !yielded {
		t.Fatalf("no yield under a saturating foreground wave: yields=%d foreground_wait_max_ms=%d (P-7 forbids holding the slot to finish)",
			st.YieldsTotal, s.Client.WaitMaxMS())
	}
	if depthAtYield != 1 {
		t.Fatalf("at the yield the queue held %d items, want the yielded item RE-QUEUED at its score (P-7: never the back, never dropped)", depthAtYield)
	}
	if st.RetriesTotal <= before.RetriesTotal {
		t.Fatalf("a yielded refresh did not come back behind the ladder: retries %d → %d (P-7/P-10)", before.RetriesTotal, st.RetriesTotal)
	}
	if st.BackoffCurrentMS < 250 {
		t.Fatalf("the ladder did not apply its first rung: backoff_current_ms=%d, want >= 250 (P-10)", st.BackoffCurrentMS)
	}
	if h, _, ok := s.Cache.Lookup(target); ok && h == HashBytes(hotBody(8192, 'z')) {
		t.Fatal("a yielded refresh published its partial body: a yield must discard the buffer")
	}
	if _, _, ok := s.Cache.Lookup(target); ok {
		t.Fatal("a yielded refresh published anything at all: nothing but a complete, verified blob may be published")
	}
	if d := depthAtYield; d == 0 {
		t.Fatal("a yielded item must RE-ENTER the queue (at its score, never the back)")
	}
	t.Logf("yield: yields=%d retries=%d backoff_current_ms=%d queue_depth=%d published_nothing=%v foreground_wait_max_ms=%d seed_hash=%s",
		st.YieldsTotal, st.RetriesTotal, st.BackoffCurrentMS, s.Manager.Stats().Queue.Depth,
		func() bool { _, _, ok := s.Cache.Lookup(target); return !ok }(), s.Client.WaitMaxMS(), seedHash[:14])
}

// TestBFS037Cell07cSustainedPressureStopsTheHotPathInFull is P-19/Q-13: under
// sustained foreground pressure the hot path stops itself IN FULL rather than
// re-acquiring slots it must hand straight back, and re-arms only when the
// pressure is gone AND the queue is empty.
func TestBFS037Cell07cSustainedPressureStopsTheHotPathInFull(t *testing.T) {
	stub := newHotStub(t)
	s := newHotSetup(t, stub,
		WithConcurrency(2),
		WithMaxConns(4),
		WithTick(5*time.Millisecond),
		WithPolicy(func(p *HotPolicy) {
			p.YieldAfter = 10 * time.Millisecond
			p.PoolPressureTicks = 3
			p.BackoffJitter = HotJitterNone
			p.QueueMaxWait = time.Hour
			p.RefreshDeadline = time.Second
		}),
	)
	stub.Throttle(150 * time.Millisecond)
	var tracked []string
	for i := 0; i < 8; i++ {
		p := fmt.Sprintf("p/%02d.txt", i)
		stub.Set(p, hotBody(2048, byte('a'+i)))
		s.seedRead(p, false)
		tracked = append(tracked, p)
	}
	var fg []string
	for i := 0; i < 16; i++ {
		p := fmt.Sprintf("pfg/%02d.txt", i)
		stub.Set(p, hotBody(2048, byte('A'+i)))
		fg = append(fg, p)
	}
	s.Manager.NoteInvalidation(tracked)
	// Over-subscribe the foreground pool for long enough that several ticks see
	// the wait (P-19's five consecutive ticks, three here).
	var wg sync.WaitGroup
	for i := 0; i < len(fg); i++ {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			_, _, _, _ = s.Manager.HotRead(context.Background(), p, "")
		}(fg[i])
	}
	stopped := hotWaitFor(t, "the hot path to stop itself", 5*time.Second, func() bool {
		return s.Manager.Stopped() && s.Manager.StopReason() == HotStopPoolPressure
	})
	wg.Wait()
	st := s.Manager.Stats()
	if !stopped {
		t.Fatalf("sustained foreground pressure did not stop the hot path in full: state=%s stops=%+v foreground_wait_max_ms=%d",
			st.State, st.Stops, s.Client.WaitMaxMS())
	}
	if st.Stops[HotStopPoolPressure] < 1 {
		t.Fatalf("the pool-pressure stop was not counted by reason: %+v", st.Stops)
	}
	if st.Queue.Depth != 0 {
		t.Fatalf("a stop that leaves %d items queued has stopped nothing (§5.5 clause 2)", st.Queue.Depth)
	}
	// Now the pressure is gone and the queue is empty: Q-13's automatic re-arm.
	rearmed := hotWaitFor(t, "the pool-pressure stop to lift", 5*time.Second, func() bool {
		return !s.Manager.Stopped()
	})
	if !rearmed {
		t.Fatalf("a pool-pressure stop did not re-arm once the pressure cleared and the queue emptied: %+v", s.Manager.Stats())
	}
	if s.Manager.Stats().Resumes[HotStopPoolPressure] < 1 {
		t.Fatalf("the automatic re-arm was not counted: %+v", s.Manager.Stats().Resumes)
	}
	// An OPERATOR stop, by contrast, never lifts by itself (Q-13).
	s.Manager.Stop(HotStopOperator)
	hotTick(30)
	if !s.Manager.Stopped() {
		t.Fatal("an operator stop lifted on its own: only an explicit re-arm may clear it (Q-13)")
	}
	s.Manager.Resume(HotStopOperator)
	if s.Manager.Stopped() {
		t.Fatal("an explicit re-arm did not clear the operator stop")
	}
	t.Logf("pressure: stops=%+v resumes=%+v stopped_then_rearmed=%v operator_stop_is_explicit=%v",
		s.Manager.Stats().Stops, s.Manager.Stats().Resumes, stopped, true)
}

var _ = strings.TrimSpace
