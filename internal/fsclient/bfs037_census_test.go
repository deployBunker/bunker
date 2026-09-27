package fsclient

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-037 — C08, THE SKIP/ABANDON CENSUS (SPEC-hot-file-policy S-12 and P-13,
// and AC-6's "every reason must be reachable from the LIVE path").
//
// THE DEFECT THIS CELL EXISTS TO CATCH is BFS-032's: a counter that exists, is
// displayed, and can never move, because a filter upstream makes its increment
// unreachable. So every arm below drives the subsystem the way the mount does —
// a real read, a real invalidation, a real stop — and asserts the counter MOVED,
// at the decision point. The single most important control (AC-6) is the
// `pre-filter` mutation in the arms script: insert a filter above the counted
// decision and the affected arm must go RED.
//
// TWO REASONS ARE STRUCTURALLY UNREACHABLE, and they are REPORTED rather than
// fabricated:
//
//   - `cache_disabled` (a skip): a mount whose cache is disabled never builds a
//     hot manager at all (D-6), so there is no live path to count it from. The
//     fact is reported as the resolved CONFIGURATION (`config_state:
//     cache_disabled`) and the arm asserts that instead of a zero.
//   - `replaced` (an abandon): "the work was taken over" is decided BEFORE a
//     request is issued (the fetch for that path is already in flight), so it
//     lands in the SKIP census, where the arm drives it. P-13's abandon-side
//     entry has no trigger by construction, and the arm asserts that.
// ---------------------------------------------------------------------------

// censusArm is one reason's drive: it fills `before`, performs the live action,
// and the caller asserts on the delta.
type censusArm struct {
	name    string
	abandon string // "" for a skip
	skip    string
	before  map[string]int64
	after   map[string]int64
	note    string
}

// censusSetup builds an armed hot path with a small ceiling, fast ticks and
// nothing started by the refresher unless the arm asks for it.
func censusSetup(t *testing.T, stub *hotStub, opts ...hotOption) *hotSetup {
	t.Helper()
	base := []hotOption{
		WithCeiling(4096),
		WithTick(2 * time.Millisecond),
		WithPolicy(func(p *HotPolicy) {
			p.QueueMaxWait = time.Hour
			p.RefreshReacquireWindow = time.Hour
			p.BackoffJitter = HotJitterNone
		}),
	}
	return newHotSetup(t, stub, append(base, opts...)...)
}

// trackedPath makes a path a refresh candidate the way the mount does: a read
// through the hot path, which TOUCHES the tracker.
func trackedPath(t *testing.T, s *hotSetup, path string, n int) {
	t.Helper()
	s.Stub.Set(path, hotBody(n, byte('a'+len(path)%26)))
	s.seedRead(path, false)
}

func TestBFS037Cell08EverySkipAndAbandonReasonIsReachable(t *testing.T) {
	run := func(name string, f func(t *testing.T) censusArm) {
		t.Run(name, func(t *testing.T) {
			arm := f(t)
			got := hotDelta(arm.before, arm.after, key(arm))
			if got < 1 {
				t.Fatalf("%s did not move: %s (delta %d)\nskips=%v\nabandons=%v",
					key(arm), arm.note, got, arm.after["skip:"+arm.skip], arm.after)
			}
			t.Logf("%s moved by %d (%s)", key(arm), got, arm.note)
		})
	}

	// ── SKIPS ──────────────────────────────────────────────────────────────

	run("skip_oversize_pre", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub, WithCeiling(1024))
		big := hotBody(1500, 'B')
		stub.Set("big.txt", big)
		s.seedRead("big.txt", true) // CACHED, so the PRE-check has a size to decide on (S-3)
		gets := stub.GETs("big.txt")
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{"big.txt"})
		hotTick(4)
		if got := stub.GETs("big.txt") - gets; got != 0 {
			t.Fatalf("the pre-check cost %d requests: S-3 decides BEFORE any request", got)
		}
		if s.Manager.Tracked("big.txt") {
			t.Fatal("an over-ceiling path must be untracked (S-8)")
		}
		return censusArm{name: "oversize_pre", skip: HotSkipOversizePre, before: before, after: hotCensus(s.Manager),
			note: "a tracked path whose known size is over the ceiling, refused with zero requests"}
	})

	run("skip_oversize_after_head", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub, WithCeiling(1024))
		trackedPath(t, s, "grew.txt", 900)
		trackedPath(t, s, "keep.txt", 900)
		served := stub.BytesServed("grew.txt")
		stub.Set("grew.txt", hotBody(4096, 'Z')) // the served size is now over the ceiling
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{"grew.txt"})
		hotTick(4)
		if got := stub.BytesServed("grew.txt") - served; got != 0 {
			t.Fatalf("the post-HEAD re-check let %d body bytes through: R5's exact failure (S-4)", got)
		}
		if stub.HEADs("grew.txt") == 0 {
			t.Fatal("the authoritative check is the HEAD (S-5), and it did not happen")
		}
		// The other tracked path is untouched by this arm's decision.
		if !s.Manager.Tracked("keep.txt") {
			t.Fatal("an arm's refusal must not untrack an unrelated path")
		}
		return censusArm{name: "oversize_after_head", skip: HotSkipOversizePreHead, before: before, after: hotCensus(s.Manager),
			note: "the local size said 900, the SERVED size said 4096, and not one byte was pulled"}
	})

	run("skip_untracked", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub)
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{"never-read.txt"})
		if s.Manager.Tracked("never-read.txt") {
			t.Fatal("an invalidation must never create a tracker entry (H-18/D-2)")
		}
		return censusArm{name: "untracked", skip: HotSkipUntracked, before: before, after: hotCensus(s.Manager),
			note: "the tree never teaches the tracker"}
	})

	run("skip_disarmed", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub)
		trackedPath(t, s, "cold.txt", 900)
		s.Manager.Disarm()
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{"cold.txt"})
		if s.Manager.Stats().Queue.Depth != 0 {
			t.Fatal("a disarmed manager queued work (D-1: arming queues ZERO refreshes)")
		}
		return censusArm{name: "disarmed", skip: HotSkipDisarmed, before: before, after: hotCensus(s.Manager),
			note: "the mount's baseline and first observation are not both complete yet (D-1)"}
	})

	run("skip_stopped", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub)
		trackedPath(t, s, "cold.txt", 900)
		s.Manager.Stop(HotStopOperator)
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{"cold.txt"})
		return censusArm{name: "stopped", skip: HotSkipStopped, before: before, after: hotCensus(s.Manager),
			note: "a stop is LEVEL-TRIGGERED: it refuses for as long as it is stopped (Q-13)"}
	})

	run("skip_no_room", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub, WithCeiling(4096), WithCacheBounds(16<<10, 4096))
		trackedPath(t, s, "room.txt", 8000)
		// Fill the cache with content NOBODY holds, so the refusal is about the
		// BOUND and not about a pin (that is the next arm).
		for i := 0; i < 4; i++ {
			fill := hotBody(3500, byte('f'+i))
			if _, err := s.Cache.Insert(fmt.Sprintf("fill%d.txt", i), HashBytes(fill), fill); err != nil {
				t.Fatalf("fixture fill %d: %v", i, err)
			}
		}
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{"room.txt"})
		hotTick(4)
		return censusArm{name: "no_room", skip: HotSkipNoRoom, before: before, after: hotCensus(s.Manager),
			note: "the cache could not fit the refresh and unpinned content could have made room: refuse, never evict"}
	})

	run("skip_pinned_eviction", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub, WithCeiling(4096), WithCacheBounds(16<<10, 4096))
		trackedPath(t, s, "pin.txt", 8000)
		// Every blob is PINNED: the space the refresh needs is held by content a
		// reader may be reading, which is D-4's case exactly.
		for i := 0; i < 4; i++ {
			fill := hotBody(3500, byte('p'+i))
			h := HashBytes(fill)
			if _, err := s.Cache.Insert(fmt.Sprintf("pin%d.txt", i), h, fill); err != nil {
				t.Fatalf("fixture pin fill %d: %v", i, err)
			}
			s.Cache.Pin(h)
		}
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{"pin.txt"})
		hotTick(4)
		return censusArm{name: "pinned_eviction", skip: HotSkipPinnedEviction, before: before, after: hotCensus(s.Manager),
			note: "the only room left is held by PINNED content: the hot path abandons rather than evict (D-4)"}
	})

	run("skip_queue_full", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub, WithPolicy(func(p *HotPolicy) { p.QueueMaxDepth = 1; p.TickInterval = time.Hour }))
		// No tick inside the arm: what is queued stays queued, so the refusal is
		// the arm's and not the refresher's doing.
		stub.Set("hot.txt", hotBody(900, 'h'))
		s.seedRead("hot.txt", false)
		s.Manager.NoteEdit("hot.txt") // weight 8: a genuine favourite
		stub.Set("cold.txt", hotBody(900, 'c'))
		s.seedRead("cold.txt", false) // weight 1: colder
		s.Manager.NoteInvalidation([]string{"hot.txt"})
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{"cold.txt"})
		if d := s.Manager.Stats().Queue.Depth; d != 1 {
			t.Fatalf("the queue holds %d items, want the hot one still occupying the only slot", d)
		}
		if got := s.Manager.Stats().Queue.Displaced; got != 0 {
			t.Fatalf("the cold newcomer DISPLACED the hot item (%d): replacement is by SCORE, not by arrival (Q-3/Q-4)", got)
		}
		return censusArm{name: "queue_full", skip: HotSkipQueueFull, before: before, after: hotCensus(s.Manager),
			note: "a full queue refuses a colder newcomer (and would displace a colder item for a hotter one)"}
	})

	run("skip_resync", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub)
		trackedPath(t, s, "resync.txt", 900)
		before := hotCensus(s.Manager)
		s.Manager.NoteResync("gap") // the mount's own OnResync
		s.Manager.NoteInvalidation([]string{"resync.txt"})
		hotTick(4)
		return censusArm{name: "resync", skip: HotSkipResync, before: before, after: hotCensus(s.Manager),
			note: "warming during/just after a resync would race the cache's own drop (D-11)"}
	})

	run("skip_tree_mismatch", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub, WithTree("tree:AAAA"))
		stub.SetTree("tree:BBBB") // the served identity is not the pinned one
		trackedPath(t, s, "stale.txt", 900)
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{"stale.txt"})
		hotTick(4)
		return censusArm{name: "tree_mismatch", skip: HotSkipTreeMismatch, before: before, after: hotCensus(s.Manager),
			note: "the client is on a tree this manager did not pin (D-9)"}
	})

	run("skip_not_found", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub)
		trackedPath(t, s, "gone.txt", 900)
		stub.Fail("gone.txt") // the path is gone: the HEAD answers 404
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{"gone.txt"})
		hotTick(6)
		if s.Manager.Tracked("gone.txt") {
			t.Fatal("a path the server no longer has must be untracked, not refreshed forever")
		}
		return censusArm{name: "not_found", skip: HotSkipNotFound, before: before, after: hotCensus(s.Manager),
			note: "the refresh's HEAD answered 404: nothing to fetch, and the entry goes"}
	})

	run("skip_replaced", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub)
		trackedPath(t, s, "busy.txt", 4000)
		stub.Set("busy.txt", hotBody(4000, 'q'))
		base := stub.BytesServed("busy.txt")
		stub.Hold()
		// A FOREGROUND reader holds the single fetch for this path. The refresher
		// has a free governor slot (a reader's fetch is not a refresh), so it will
		// try to start one — and find the map already busy. That is the `replaced`
		// decision, and it is reachable only this way: with a REFRESH in flight the
		// governor is full and the queue is never attempted, so the item would sit
		// queued and no skip would be counted at all.
		go func() { _, _, _, _ = s.Manager.HotRead(context.Background(), "busy.txt", "") }()
		if !hotWaitFor(t, "a foreground fetch to be in flight", 5*time.Second, func() bool {
			return s.Manager.SingleflightEntries() == 1
		}) {
			t.Fatalf("no foreground fetch was registered for the path")
		}
		if !hotWaitHeld(t, stub, "busy.txt", 4000, base, 5*time.Second) {
			t.Fatalf("the foreground transfer never parked mid-body")
		}
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{"busy.txt"})
		if !hotWaitFor(t, "the queued item to be dropped as replaced", 5*time.Second, func() bool {
			return hotDelta(before, hotCensus(s.Manager), "skip:"+HotSkipReplaced) >= 1
		}) {
			t.Fatalf("a queued item whose fetch is already in flight was not counted as replaced: %v", hotCensus(s.Manager))
		}
		if got := s.Manager.Stats().RefreshInflight; got != 0 {
			t.Fatalf("the refresher issued a SECOND fetch for a path already in flight: inflight=%d (PR-6)", got)
		}
		stub.Release()
		return censusArm{name: "replaced", skip: HotSkipReplaced, before: before, after: hotCensus(s.Manager),
			note: "the work was already in flight (PR-6's single-flight invariant), so no second GET"}
	})

	// ── ABANDONS ───────────────────────────────────────────────────────────

	run("abandon_deadline", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub, WithPolicy(func(p *HotPolicy) { p.RefreshDeadline = 100 * time.Millisecond }))
		trackedPath(t, s, "hang.txt", 4000)
		stub.Hold()
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{"hang.txt"})
		if !hotWaitFor(t, "the deadline abandon", 5*time.Second, func() bool {
			return hotDelta(before, hotCensus(s.Manager), "abandon:"+HotAbandonDeadline) >= 1
		}) {
			t.Fatalf("no deadline abandon: %v", hotCensus(s.Manager))
		}
		if s.Manager.Stats().RefreshInflight != 0 {
			t.Fatal("the abandoned refresh still holds its slot")
		}
		return censusArm{name: "deadline", abandon: HotAbandonDeadline, before: before, after: hotCensus(s.Manager),
			note: "hot_refresh_deadline: hang up rather than hold a slot (P-11)"}
	})

	run("abandon_reacquire_window", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub,
			WithConcurrency(2),
			WithTick(250*time.Millisecond),
			WithPolicy(func(p *HotPolicy) {
				p.YieldAfter = 20 * time.Millisecond
				p.PoolPressureTicks = 1000
				p.RefreshDeadline = 20 * time.Second
				p.RefreshReacquireWindow = time.Millisecond // one tick and the window has passed
			}),
		)
		target := "y.txt"
		stub.Set(target, hotBody(4000, 'y'))
		s.seedRead(target, false)
		var fg []string
		for i := 0; i < 8; i++ {
			p := fmt.Sprintf("yfg/%02d.txt", i)
			stub.Set(p, hotBody(2000, byte('A'+i)))
			fg = append(fg, p)
		}
		stub.HeadDelay(150 * time.Millisecond)
		stub.Set(target, hotBody(4000, 'z'))
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{target})
		if !hotWaitFor(t, "the refresh to be up", 5*time.Second, func() bool {
			return s.Manager.Stats().RefreshInflight == 1
		}) {
			t.Fatalf("no refresh started: %+v", s.Manager.Stats())
		}
		for _, p := range fg {
			go func(p string) { _, _, _, _ = s.Manager.HotRead(context.Background(), p, "") }(p)
		}
		// A yield first, then the sweep: an item that cannot reacquire a slot
		// inside hot_refresh_reacquire_window is ABANDONED, not held forever.
		ok := hotWaitFor(t, "the reacquire-window abandon", 10*time.Second, func() bool {
			return hotDelta(before, hotCensus(s.Manager), "abandon:"+HotAbandonReacquireWindow) >= 1
		})
		if !ok {
			t.Fatalf("a put-back item outlived its reacquire window without being abandoned: %v", hotCensus(s.Manager))
		}
		return censusArm{name: "reacquire_window", abandon: HotAbandonReacquireWindow, before: before, after: hotCensus(s.Manager),
			note: "put back by a yield and unable to start again inside the window (P-12)"}
	})

	run("abandon_stopped", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub)
		trackedPath(t, s, "stopme.txt", 4000)
		stub.Hold()
		s.Manager.NoteInvalidation([]string{"stopme.txt"})
		if !hotWaitFor(t, "the refresh to be in flight", 5*time.Second, func() bool {
			return s.Manager.Stats().RefreshInflight == 1
		}) {
			t.Fatalf("no refresh started: %+v", s.Manager.Stats())
		}
		pre := hotCensus(s.Manager)
		s.Manager.Stop(HotStopOperator)
		return censusArm{name: "stopped", abandon: HotAbandonStopped, before: pre, after: hotCensus(s.Manager),
			note: "STOP IN FULL abandons an in-flight refresh at its next checkpoint (§5.5 clause 3)"}
	})

	run("abandon_shutdown", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub)
		trackedPath(t, s, "bye.txt", 4000)
		stub.Hold()
		s.Manager.NoteInvalidation([]string{"bye.txt"})
		if !hotWaitFor(t, "the refresh to be in flight", 5*time.Second, func() bool {
			return s.Manager.Stats().RefreshInflight == 1
		}) {
			t.Fatalf("no refresh started: %+v", s.Manager.Stats())
		}
		before := hotCensus(s.Manager)
		// Close() is the mount's Unmount path: the stop it takes is a shutdown,
		// and a shutdown abandon is a different fact from an operator's stop.
		_ = s.Manager.Close()
		return censusArm{name: "shutdown", abandon: HotAbandonShutdown, before: before, after: hotCensus(s.Manager),
			note: "the mount went away mid-refresh"}
	})

	run("abandon_tree_mismatch", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub, WithTree("tree:AAAA"))
		stub.SetTree("tree:AAAA")
		trackedPath(t, s, "moves.txt", 4000)
		stub.Hold()
		s.Manager.NoteInvalidation([]string{"moves.txt"})
		if !hotWaitFor(t, "the refresh to be mid-body", 5*time.Second, func() bool {
			return s.Manager.Stats().RefreshInflight == 1 && stub.BytesServed("moves.txt") > 0
		}) {
			t.Fatalf("no refresh in flight: %+v", s.Manager.Stats())
		}
		before := hotCensus(s.Manager)
		// The tree changes WHILE the body is arriving, and the client observes
		// the new identity on the next response it reads.
		stub.SetTree("tree:BBBB")
		_, _, _, _ = s.Manager.HotRead(context.Background(), "observer.txt", "")
		stub.Release()
		if !hotWaitFor(t, "the tree-mismatch abandon", 5*time.Second, func() bool {
			return hotDelta(before, hotCensus(s.Manager), "abandon:"+HotAbandonTreeMismatch) >= 1
		}) {
			t.Fatalf("bytes from a tree this mount is no longer bound to were not refused: %v", hotCensus(s.Manager))
		}
		return censusArm{name: "tree_mismatch", abandon: HotAbandonTreeMismatch, before: before, after: hotCensus(s.Manager),
			note: "the served identity moved mid-fetch: never published under the pinned identity (D-9)"}
	})

	run("abandon_not_found", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub)
		trackedPath(t, s, "vanish.txt", 900)
		stub.FailGET("vanish.txt") // the HEAD succeeds, the GET is 404
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{"vanish.txt"})
		if !hotWaitFor(t, "the not-found abandon", 5*time.Second, func() bool {
			return hotDelta(before, hotCensus(s.Manager), "abandon:"+HotAbandonNotFound) >= 1
		}) {
			t.Fatalf("a GET that 404s after a successful HEAD was not counted as an abandonment: %v", hotCensus(s.Manager))
		}
		return censusArm{name: "not_found", abandon: HotAbandonNotFound, before: before, after: hotCensus(s.Manager),
			note: "the file went away between the HEAD and the GET — a STARTED refresh hung up"}
	})

	run("abandon_no_room_after_start", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub, WithCeiling(4096), WithCacheBounds(16<<10, 4096), WithPolicy(func(p *HotPolicy) {
			p.RefreshDeadline = 20 * time.Second
		}))
		target := "tight.txt"
		stub.Set(target, hotBody(3000, 't'))
		s.seedRead(target, false)
		stub.Set(target, hotBody(3000, 'u'))
		stub.Hold()
		base := stub.BytesServed(target)
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{target})
		if !hotWaitFor(t, "the refresh to be launched", 5*time.Second, func() bool {
			return s.Manager.Stats().RefreshInflight == 1
		}) {
			t.Fatalf("no refresh in flight: %+v", s.Manager.Stats())
		}
		// THE GATE MUST BE LOAD-BEARING: the transfer has served some of its
		// bytes and not all of them, which is the only honest evidence that it is
		// parked mid-body rather than already finished.
		if !hotWaitHeld(t, stub, target, 3000, base, 5*time.Second) {
			t.Fatalf("the held transfer never parked mid-body (served %d of 3000 before the gate): the arm's window never opened",
				stub.BytesServed(target)-base)
		}
		// The cache fills up while the refresh is in flight, with content nothing
		// holds: the stage is refused for ROOM, not for a pin.
		for i := 0; i < 13; i++ {
			fill := hotBody(1000, byte('n'+i))
			if _, err := s.Cache.Insert(fmt.Sprintf("n%d.txt", i), HashBytes(fill), fill); err != nil {
				t.Fatalf("fixture fill %d: %v", i, err)
			}
		}
		stub.Release()
		hotTick(10)
		if !hotWaitFor(t, "the post-fetch admission refusal", 5*time.Second, func() bool {
			return hotDelta(before, hotCensus(s.Manager), "abandon:"+HotAbandonNoRoomAfter) >= 1
		}) {
			t.Fatalf("a refresh refused at the STAGE after its fetch was not counted: %v", hotCensus(s.Manager))
		}
		return censusArm{name: "no_room_after_start", abandon: HotAbandonNoRoomAfter, before: before, after: hotCensus(s.Manager),
			note: "admission refused the staged blob: abandon, never evict (Q-8/Q-10)"}
	})

	run("abandon_pinned_eviction", func(t *testing.T) censusArm {
		stub := newHotStub(t)
		s := censusSetup(t, stub, WithCeiling(4096), WithCacheBounds(16<<10, 4096), WithPolicy(func(p *HotPolicy) {
			p.RefreshDeadline = 20 * time.Second
		}))
		target := "tight2.txt"
		stub.Set(target, hotBody(3000, 't'))
		s.seedRead(target, false)
		stub.Set(target, hotBody(3000, 'u'))
		stub.Hold()
		base := stub.BytesServed(target)
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{target})
		if !hotWaitFor(t, "the refresh to be launched", 5*time.Second, func() bool {
			return s.Manager.Stats().RefreshInflight == 1
		}) {
			t.Fatalf("no refresh in flight: %+v", s.Manager.Stats())
		}
		if !hotWaitHeld(t, stub, target, 3000, base, 5*time.Second) {
			t.Fatalf("the held transfer never parked mid-body (served %d of 3000 before the gate)", stub.BytesServed(target)-base)
		}
		for i := 0; i < 13; i++ {
			fill := hotBody(1000, byte('P'+i))
			h := HashBytes(fill)
			if _, err := s.Cache.Insert(fmt.Sprintf("P%d.txt", i), h, fill); err != nil {
				t.Fatalf("fixture pin fill %d: %v", i, err)
			}
			s.Cache.Pin(h) // every byte is held by a reader
		}
		stub.Release()
		if !hotWaitFor(t, "the pinned-eviction abandonment", 5*time.Second, func() bool {
			return hotDelta(before, hotCensus(s.Manager), "abandon:"+HotAbandonPinnedEviction) >= 1
		}) {
			t.Fatalf("a refresh that could only fit by evicting a PINNED blob was not counted: %v", hotCensus(s.Manager))
		}
		return censusArm{name: "pinned_eviction", abandon: HotAbandonPinnedEviction, before: before, after: hotCensus(s.Manager),
			note: "the only way to fit would be evicting content a reader holds: abandon (D-4)"}
	})

	// ── THE TWO STRUCTURALLY UNREACHABLE REASONS, REPORTED ─────────────────

	t.Run("skip_cache_disabled_is_a_config_state_not_a_counter", func(t *testing.T) {
		pol := DefaultHotPolicy()
		pol.Enabled = true
		env := HotPolicyEnv{Concurrency: 8, OpTimeout: 30 * time.Second, CacheMaxBytes: 0}
		if got := pol.Effective(env).ConfigState; got != HotConfigCacheDisabled {
			t.Fatalf("a hot policy with no cache must report config_state=%q, got %q (D-6)", HotConfigCacheDisabled, got)
		}
		cache, err := OpenCache(CacheConfig{Dir: t.TempDir(), MaxBytes: 0})
		if err != nil {
			t.Fatalf("a disabled cache must still open: %v", err)
		}
		t.Cleanup(func() { _ = cache.Close() })
		stub := newHotStub(t)
		cl, err := NewClient(Options{BaseURL: stub.URL(), Concurrency: 8})
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		if mgr := NewHotManager(HotManagerConfig{Policy: pol, Client: cl, Cache: cache, TrackerDir: t.TempDir()}); mgr != nil {
			t.Fatal("a disabled cache must produce NO manager: there is no live path to count a skip from (D-6)")
		}
		t.Logf("skip:cache_disabled is reported as config_state=%s, with no manager to count from — a counter that could never move would be BFS-032's shape", HotConfigCacheDisabled)
	})

	t.Run("abandon_replaced_has_no_trigger_by_construction", func(t *testing.T) {
		stub := newHotStub(t)
		s := censusSetup(t, stub)
		trackedPath(t, s, "busy2.txt", 4000)
		stub.Set("busy2.txt", hotBody(4000, 'r'))
		base := stub.BytesServed("busy2.txt")
		stub.Hold()
		go func() { _, _, _, _ = s.Manager.HotRead(context.Background(), "busy2.txt", "") }()
		if !hotWaitFor(t, "a foreground fetch to be in flight", 5*time.Second, func() bool {
			return s.Manager.SingleflightEntries() == 1
		}) {
			t.Fatal("no foreground fetch was registered for the path")
		}
		if !hotWaitHeld(t, stub, "busy2.txt", 4000, base, 5*time.Second) {
			t.Fatal("the foreground transfer never parked mid-body")
		}
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{"busy2.txt"})
		if !hotWaitFor(t, "the replaced skip", 5*time.Second, func() bool {
			return hotDelta(before, hotCensus(s.Manager), "skip:"+HotSkipReplaced) >= 1
		}) {
			t.Fatalf("the skip-side `replaced` did not move: %v", hotCensus(s.Manager))
		}
		after := hotCensus(s.Manager)
		stub.Release()
		if d := hotDelta(before, after, "abandon:"+HotAbandonReplaced); d != 0 {
			t.Fatalf("the abandon-side `replaced` moved: this implementation assigns the reason to the decision that actually happens (%d)", d)
		}
		t.Logf("abandon:replaced has no trigger: a path whose fetch is in flight is refused BEFORE a request starts, so the reason lives in the skip census (skip:replaced moved). The abandon-side entry is retained as P-13's vocabulary and reported as structurally unreachable rather than fabricated.")
	})
}

func key(a censusArm) string {
	if a.abandon != "" {
		return "abandon:" + a.abandon
	}
	return "skip:" + a.skip
}
