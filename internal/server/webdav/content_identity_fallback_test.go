package webdav

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// QA-BUNKER-36 — THE CONTENT-VERIFIED FALLBACK.
//
// The defect: on a kernel whose filesystem timestamps come from a COARSE clock
// (CONFIG_HZ), an edit performed within one tick of the write before it is
// stamped with the SAME mtime and the SAME ctime. A same-size rewrite that
// restores its mtime is then identical to the observation recorded a moment
// earlier in EVERY field of the identity, and a metadata-only diff answers
// "unchanged" for bytes that moved. Measured on the host the row was filed from
// (Debian, CONFIG_HZ=250, a 4 ms tick): seed write, edit, `os.Chtimes` restoring
// the mtime, no sleep between them — ctime delta 0 ns on 5/5 iterations, ext4 and
// tmpfs, and five cells that pass on a 1 ms-tick host fail there.
//
// The arms below are kernel-INDEPENDENT by construction, which is the point:
// none of them waits for a clock and none of them asserts a filesystem property.
// Where the natural path depends on the kernel's tick — "did the two writes land
// in one tick?" — the cell builds the record that tick produces DIRECTLY (the
// identity the filesystem reports now, with the digest of the bytes as they
// were before the edit) and hands it to the real diff. That is the hostile
// condition itself, not a proxy for it, so the cells hold on CONFIG_HZ=1000 and
// on CONFIG_HZ=100 alike.
//
// This file asserts surface the row ADDS (coarseClockAmbiguous, observedEntry,
// ledgerRecord/ledgerDiff, ContentVerificationCounters, eventsCoarseClockWindow),
// so the arms script (docs/evidence/BFS-049-arms.sh) sets it aside for the runs
// that swap in the filed production blob — the same split, and the same reason,
// as identity_report_bfs049_test.go.
// ---------------------------------------------------------------------------

// TestQA36TheDeclaredWindowCoversEveryTickALinuxKernelShips pins the declared
// value of the fallback's window, because everything about the fix's cost
// depends on it: the window IS the interval after a path's last metadata change
// in which an edit can still hide inside the timestamps, and it is also the
// interval in which the ledger and the cache refuse to trust metadata alone.
//
// A lowered window would trade the defect back for speed, so the declared value
// is asserted here rather than left to a cell that could lower it silently.
func TestQA36TheDeclaredWindowCoversEveryTickALinuxKernelShips(t *testing.T) {
	// The widest tick a Linux kernel ships is CONFIG_HZ=100, i.e. 10 ms. The
	// declared window must clear it with room for the sub-tick phase of the edit
	// that lands just after the tick boundary.
	if eventsCoarseClockWindow < 20*time.Millisecond {
		t.Fatalf("the declared coarse-clock window is %s: a CONFIG_HZ=100 kernel (a 10 ms tick) makes an edit invisible for a whole 10 ms after the write before it, so a window under 20 ms cannot cover it", eventsCoarseClockWindow)
	}
	// The BFS-049 arms settle a fixture past this window with a literal
	// (settleForCacheHitBFS049) because that file must compile against the filed
	// blob, which has no window symbol to name. The pair is pinned here so the
	// literal cannot fall inside the window and quietly turn those arms into
	// re-read measurements.
	if settleForCacheHitBFS049 <= 2*eventsCoarseClockWindow {
		t.Fatalf("the BFS-049 arms settle for %s, which does not clear twice the %s window: those arms would measure a re-read of a just-written fixture and report it as a cache hit", settleForCacheHitBFS049, eventsCoarseClockWindow)
	}
	// The window is the coarse clock's granularity, which is at most one tick: a
	// window that claimed to be a whole second would mean the fallback reads
	// every file written in the last second, which is not "the tick" at all.
	if eventsCoarseClockWindow > time.Second {
		t.Fatalf("the declared coarse-clock window is %s: that is no longer a clock granularity, it is a policy of re-reading everything touched recently", eventsCoarseClockWindow)
	}
}

// TestQA36ASameMetadataRewriteIsReportedFromTheBytes is this row's RED cell, and
// it CONSTRUCTS the hostile condition instead of waiting for it: the record the
// ledger is given carries the metadata the filesystem reports NOW — after the
// edit — together with the digest of the bytes as they were BEFORE it. That pair
// is exactly what a CONFIG_HZ<=250 kernel leaves behind, and it is unreachable by
// sleeping on a 1 ms-tick host, which is why the cell builds it.
//
// The three verdicts it must produce, and what each one costs:
//
//	same metadata, different bytes   -> REPORTED, one content verification
//	same metadata, same bytes        -> quiet,     one content verification
//	metadata vouched by its own age  -> quiet,     NO read at all
//
// The last one is the cheapness rule and the one that has to stay measurable: a
// settled tree must not start re-reading itself because the fallback exists.
func TestQA36ASameMetadataRewriteIsReportedFromTheBytes(t *testing.T) {
	h := newTestHandler(t)
	target := filepath.Join(h.Root(), "src", "util.go")

	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	preEdit, err := contentDigest(target)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	// THE EDIT: byte count preserved, bytes moved, mtime restored to the
	// nanosecond. Whether this kernel also freezes ctime is deliberately NOT
	// asserted — the cells below state the frozen identity themselves.
	edited := []byte(strings.ReplaceAll(string(body), "util", "uti1"))
	if len(edited) != len(body) {
		t.Fatalf("the fixture is not the trap it claims to be: %d -> %d bytes", len(body), len(edited))
	}
	if err := os.WriteFile(target, edited, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(target, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("the trap did not hold: %d -> %d bytes", before.Size(), after.Size())
	}
	postEdit, err := contentDigest(target)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if postEdit == preEdit {
		t.Fatalf("the fixture is not the trap it claims to be: the bytes did not move (%s)", postEdit)
	}
	frozen := identityOf(after)
	now := time.Now()

	state, truncated, err := h.tree.observe()
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if truncated {
		t.Fatal("the fixture tree is not supposed to hit the observation cap")
	}
	if state["src/util.go"].sameIdentity(frozen) == false {
		t.Fatalf("the constructed record does not match the observation it is compared against: %+v vs %+v", state["src/util.go"], frozen)
	}

	// Every other path gets a record its own age vouches for, so the only thing
	// any cell below measures is the record it installs for src/util.go.
	base := func() map[string]observedEntry {
		m := make(map[string]observedEntry, len(state))
		for p, id := range state {
			if p == "src/util.go" {
				continue
			}
			m[p] = observedEntry{id: id, recordedAt: now.Add(2 * eventsCoarseClockWindow)}
		}
		return m
	}
	movedRecord := frozen
	movedRecord.Ctime--

	cases := []struct {
		name string
		// record is what the ledger holds for src/util.go.
		record observedEntry
		// wantReported is whether that path must be in the diff's path list.
		wantReported bool
		// wantVerifications is how many content reads the cell must pay.
		wantVerifications int64
		// wantBlindMoves is the delta of the (size, mtime)-blind class counter,
		// which is how the divergence between the two observers is reported.
		wantBlindMoves int64
	}{
		{
			// The defect, stated directly: the kernel could not separate the two
			// writes, so every metadata field agrees and only the bytes differ.
			name:              "the kernel could not separate the writes: same metadata, different bytes",
			record:            observedEntry{id: frozen, digest: preEdit, recordedAt: now},
			wantReported:      true,
			wantVerifications: 1,
			wantBlindMoves:    1,
		},
		{
			// The same record with the bytes standing still: the fallback is a
			// verification, not a licence to report everything inside the window.
			name:              "same metadata, same bytes: verified and quiet",
			record:            observedEntry{id: frozen, digest: postEdit, recordedAt: now},
			wantReported:      false,
			wantVerifications: 1,
			wantBlindMoves:    0,
		},
		{
			// A record taken on the far side of the window: an edit after it
			// cannot leave the metadata identical, so the metadata is evidence
			// and no byte is read. (recordedAt is constructed rather than slept
			// for; the cell is about the pair (recordedAt, ctime).)
			name:              "the record is vouched for by its own age: no read, quiet",
			record:            observedEntry{id: frozen, digest: preEdit, recordedAt: now.Add(2 * eventsCoarseClockWindow)},
			wantReported:      false,
			wantVerifications: 0,
			wantBlindMoves:    0,
		},
		{
			// The ordinary metadata move, unchanged by this row: the path is a
			// change on its own evidence. The one verification it pays is the
			// BASELINE read — the digest the next observation compares against —
			// which is why the cheapness rule is "never re-read to DECIDE", not
			// "never read a moved path".
			name:              "the metadata moved: reported on its own evidence",
			record:            observedEntry{id: movedRecord, digest: preEdit, recordedAt: now},
			wantReported:      true,
			wantVerifications: 1,
			wantBlindMoves:    1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := base()
			rec["src/util.go"] = tc.record

			blindBefore, _ := IdentityDivergenceCounters()
			verifyBefore, _ := ContentVerificationCounters()

			changed, next := h.tree.ledgerDiff(rec, state, now)

			verifyAfter, lastPath := ContentVerificationCounters()
			blindAfter, _ := IdentityDivergenceCounters()

			reported := false
			for _, p := range changed {
				if p == "src/util.go" {
					reported = true
				}
			}
			if reported != tc.wantReported {
				t.Fatalf("src/util.go reported=%v, want %v (paths=%v)", reported, tc.wantReported, changed)
			}
			// Sharpness: nothing else may move, so a cell cannot pass because the
			// whole tree was reported.
			wantPaths := 0
			if tc.wantReported {
				wantPaths = 1
			}
			if len(changed) != wantPaths {
				t.Fatalf("the diff named %d paths (%v), want %d — only the path under test may move", len(changed), changed, wantPaths)
			}
			if got := verifyAfter - verifyBefore; got != tc.wantVerifications {
				t.Fatalf("content verifications = %d, want %d (the fallback must read exactly the records its metadata cannot vouch for)", got, tc.wantVerifications)
			}
			if tc.wantVerifications > 0 && lastPath != "src/util.go" {
				t.Fatalf("the verification was paid for %q, want src/util.go", lastPath)
			}
			if got := blindAfter - blindBefore; got != tc.wantBlindMoves {
				t.Fatalf("(size, mtime)-blind class counter moved by %d, want %d", got, tc.wantBlindMoves)
			}
			// The record the diff hands the next observation keeps the path's
			// metadata, so a second identical observation composes rather than
			// oscillating.
			if got := next["src/util.go"].id; !got.sameIdentity(state["src/util.go"]) {
				t.Fatalf("the next record's identity is %+v, want the observed %+v", got, state["src/util.go"])
			}
		})
	}
}

// TestQA36ThePollReportsAFrozenEditTheKernelCouldNotStamp drives the SAME
// hostile condition through the served surface, with the ledger's baseline taken
// by a real whole-tree snapshot (the observation a binding client takes) and the
// verdict read off the poll's own envelope. It is the wiring arm: the unit cell
// above proves the comparison, and this one proves the poll answers a client with
// it.
//
// The freeze is installed the way a CONFIG_HZ<=250 kernel installs it — by
// REPLACING the ledger's record for one path with the metadata the filesystem
// reports now and the digest the bytes had before — rather than by sleeping, so
// the cell measures the same thing on every host.
func TestQA36ThePollReportsAFrozenEditTheKernelCouldNotStamp(t *testing.T) {
	h := newTestHandler(t)
	target := filepath.Join(h.Root(), "src", "util.go")
	cursor := seedWithSnapshot(t, h)

	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// The snapshot's baseline must be a content-verified one, or the cell below
	// would be measuring a baseline that could never have caught the edit.
	l := h.tree.eventLedger()
	l.mu.Lock()
	seeded, ok := l.observed["src/util.go"]
	l.mu.Unlock()
	if !ok {
		t.Fatalf("the whole-tree snapshot recorded no baseline for src/util.go: the ledger's records are %v", keysOf(l.observed))
	}
	if seeded.digest == "" {
		t.Fatalf("the snapshot's baseline record for src/util.go carries no digest: a baseline taken inside the coarse window must be content-verified, or the first poll is as blind as the diff it exists to make honest")
	}
	preEdit, err := contentDigest(target)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	edited := []byte(strings.ReplaceAll(string(body), "util", "uti1"))
	if len(edited) != len(body) {
		t.Fatalf("the fixture is not the trap it claims to be: %d -> %d bytes", len(body), len(edited))
	}
	if err := os.WriteFile(target, edited, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(target, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Install the frozen record: the identity the filesystem reports NOW (so the
	// poll's own stat agrees with it field for field) and the digest of the bytes
	// as they were BEFORE the edit.
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	verifyBefore, _ := ContentVerificationCounters()
	l.mu.Lock()
	l.observed["src/util.go"] = observedEntry{id: identityOf(after), digest: preEdit, recordedAt: time.Now()}
	l.mu.Unlock()

	got := pollAt(t, h, cursor)
	if len(got.Result.Events) != 1 || got.Result.Events[0].Event != eventInvalidate {
		t.Fatalf("a same-size, mtime-preserved edit the coarse clock could not stamp was not reported: %+v", got.Result.Events)
	}
	if paths := got.Result.Events[0].Paths; len(paths) != 1 || paths[0] != "src/util.go" {
		t.Fatalf("paths = %v, want [src/util.go]: the quiet paths must stay quiet", paths)
	}
	verifyAfter, _ := ContentVerificationCounters()
	if verifyAfter <= verifyBefore {
		t.Fatal("the poll reported the edit without verifying any content: on this record the metadata alone is indistinguishable from a quiet tree, so the verdict must have come from the bytes")
	}
	// And the ledger is caught up: the same cursor answered twice must be quiet.
	again := pollAt(t, h, got.Result.HeadSeq)
	if len(again.Result.Events) != 0 {
		t.Fatalf("the caught-up cursor was served the event again: %+v", again.Result.Events)
	}
}

// TestQA36TheFallbackNeverReadsASettledTreeOrAMovedPath is the cheapness rule,
// measured rather than promised: the fallback's price is reading bytes, so the
// two cases in which it must NOT read are asserted against the process's own
// counter.
//
// Part A states the honest cost of a baseline taken a moment ago: a poll inside
// the window re-verifies the fixture's regular files and reports nothing — a
// quiet tree stays quiet, which is the no-event-storm arm. Part B states the
// steady state, with the window lowered to a nanosecond to stand in for "the
// metadata is old enough to vouch for its bytes": a quiet poll reads NOTHING, and
// an edit that moves the metadata is reported and reads NOTHING either.
func TestQA36TheFallbackNeverReadsASettledTreeOrAMovedPath(t *testing.T) {
	t.Run("inside the window: a quiet tree is re-verified, and stays quiet", func(t *testing.T) {
		h := newTestHandler(t)
		cursor := seedWithSnapshot(t, h)

		verifyBefore, _ := ContentVerificationCounters()
		got := pollAt(t, h, cursor)
		verifyAfter, _ := ContentVerificationCounters()

		if len(got.Result.Events) != 0 {
			t.Fatalf("a quiet tree polled inside the coarse window produced events: %+v — verification is not a licence to report", got.Result.Events)
		}
		if verifyAfter <= verifyBefore {
			t.Fatalf("no record was verified on a fixture younger than the declared %s window: this arm is not measuring the fallback at all", eventsCoarseClockWindow)
		}
		t.Logf("QA-BUNKER-36: a quiet poll inside the window verified %d path(s) and reported none", verifyAfter-verifyBefore)
	})

	t.Run("past the window: a settled tree and a moved path are never re-read", func(t *testing.T) {
		h := newTestHandler(t)
		cursor := seedWithSnapshot(t, h)
		target := filepath.Join(h.Root(), "src", "util.go")

		// Stand in for "every record is older than the window" by declaring a
		// window nothing can be inside. The declared value is pinned by
		// TestQA36TheDeclaredWindowCoversEveryTickALinuxKernelShips, so this cell
		// cannot weaken it.
		old := eventsCoarseClockWindow
		eventsCoarseClockWindow = time.Nanosecond
		t.Cleanup(func() { eventsCoarseClockWindow = old })

		// (1) Nothing moves: no read may be paid.
		verifyBefore, _ := ContentVerificationCounters()
		if got := pollAt(t, h, cursor); len(got.Result.Events) != 0 {
			t.Fatalf("a settled tree produced events: %+v", got.Result.Events)
		}
		if verifyAfter, _ := ContentVerificationCounters(); verifyAfter != verifyBefore {
			t.Fatalf("a settled tree paid %d content verification(s): metadata that cannot hide an edit must be taken as evidence, or the fallback re-reads every file on every poll", verifyAfter-verifyBefore)
		}

		// (2) An ordinary edit, whose metadata moved: reported, and still no read
		// — a moved path is a change on its own evidence, and re-hashing it to
		// decide is exactly what the cheapness rule forbids.
		if err := os.WriteFile(target, []byte("package main\n\nfunc util() { /* moved */ }\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		verifyBefore, _ = ContentVerificationCounters()
		got := pollAt(t, h, cursor)
		verifyAfter, _ := ContentVerificationCounters()
		if len(got.Result.Events) != 1 || got.Result.Events[0].Event != eventInvalidate {
			t.Fatalf("the edit was not reported: %+v", got.Result.Events)
		}
		if verifyAfter != verifyBefore {
			t.Fatalf("a path whose metadata MOVED paid %d content verification(s): the move is the change, and re-hashing it is not what this fallback is for", verifyAfter-verifyBefore)
		}
	})
}

// TestQA36TheHashCacheReVerifiesInsideTheCoarseClockWindow is the cache half of
// the same rule, and it is what keeps BFS-049's two observers agreeing rather
// than merely both existing: the hash cache decides whether CONTENT is re-sent (a
// GET's ETag and its 304), so a metadata-keyed memo taken inside the window would
// answer 304 with the ETag of bytes that moved — the exact divergence BFS-049
// closed, one clock granularity lower.
//
// It plants the memo a coarse-clock kernel leaves behind (the entry's own
// metadata, which the filesystem still reports, with a hash that is not the
// bytes') and asserts both sides of the window: inside it the memo is re-derived
// from the bytes, and past it the memo is taken — because a rule that never hits
// would close the divergence by disabling the cache, which is a performance
// regression wearing a correctness fix's clothes.
func TestQA36TheHashCacheReVerifiesInsideTheCoarseClockWindow(t *testing.T) {
	h := newTestHandler(t)
	target := filepath.Join(h.Root(), "src", "util.go")

	trueHash, err := h.tree.hashFile(target)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	// The memo is planted with the entry's own identity and a hash that is not
	// the bytes': that is the state a rewritten-but-metadata-identical path
	// leaves in the cache.
	plantHashCacheBFS049(t, h, target, probeSentinelBFS049)

	got, err := h.tree.hashFile(target)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	if got == probeSentinelBFS049 {
		t.Fatal("the cache answered a metadata-keyed memo for a path written inside the coarse window: its key cannot see the one field a frozen edit leaves standing, so a GET could answer 304 with the ETag of bytes that moved")
	}
	if got != trueHash {
		t.Fatalf("the cache recomputed %s but the file's hash is %s", got, trueHash)
	}

	// Past the window the very same memo IS taken: the window bounds when the
	// cache must re-derive, it does not disable the cache.
	plantHashCacheBFS049(t, h, target, probeSentinelBFS049)
	old := eventsCoarseClockWindow
	eventsCoarseClockWindow = time.Nanosecond
	t.Cleanup(func() { eventsCoarseClockWindow = old })
	settled, err := h.tree.hashFile(target)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	if settled != probeSentinelBFS049 {
		t.Fatalf("a settled path did not hit the cache (got %s): a key that never matches is not a fix, it is a rehash on every read", settled)
	}
}

// TestQA36TheFallbackComparesDigestsNotWholeFiles proves the fallback is a
// STREAMED read and not a buffered one: the digest of a file larger than any
// reasonable buffer must be right, and the observation must not carry the bytes
// with it. It is the cost-shape arm for the one place this row adds a read to a
// poll path.
func TestQA36TheFallbackComparesDigestsNotWholeFiles(t *testing.T) {
	h := newTestHandler(t)
	// A file wide enough that a buffer-it-first implementation would be visible
	// in the numbers, with content that differs in one byte at the end — the
	// worst case for a streaming digest and the only case a size check could
	// never see.
	const size = 1 << 20
	body := make([]byte, size)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	edited := append([]byte(nil), body...)
	edited[size-1] = 'z'

	root := h.Root()
	big := filepath.Join(root, "big.bin")
	if err := os.WriteFile(big, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	first, err := contentDigest(big)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	before, err := os.Stat(big)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.WriteFile(big, edited, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(big, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	after, err := os.Stat(big)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("the trap did not hold: %d -> %d bytes", before.Size(), after.Size())
	}
	second, err := contentDigest(big)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if first == second {
		t.Fatal("a one-byte change at the end of a 1 MiB file produced the same digest: the comparison is not reading the whole file")
	}

	state, _, err := h.tree.observe()
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	now := time.Now()
	prev := map[string]observedEntry{"big.bin": {id: identityOf(after), digest: first, recordedAt: now}}
	changed, _ := h.tree.ledgerDiff(prev, state, now)
	found := false
	for _, p := range changed {
		if p == "big.bin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the 1 MiB same-size edit was not reported: %v", changed)
	}
}

// TestQA36AReadThatCannotBeTakenFabricatesNothing is the cell for the failure
// mode a content-verified comparison can have that a metadata one cannot: the
// bytes are read, so the read can fail (the path vanished mid-observation, the
// open failed, the end of the range was lost). A comparison that never happened
// must not produce an event — the class the arm pins is "an unreadable path must
// not fabricate an invalidate", and it was found the way it should be: by a
// load-stressed suite reporting an extra path on an unchanged tree.
//
// The retry is asserted too, because "no event" is only honest if the change is
// still reported by the observation that CAN see it: the next walk sees the path
// gone and reports the removal.
func TestQA36AReadThatCannotBeTakenFabricatesNothing(t *testing.T) {
	h := newTestHandler(t)
	target := filepath.Join(h.Root(), "src", "util.go")
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	digest, err := contentDigest(target)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	now := time.Now()
	state, _, err := h.tree.observe()
	if err != nil {
		t.Fatalf("observe: %v", err)
	}

	// The path the walk saw is gone before the verification reads it: the read
	// fails, and it must produce no event and renew no record.
	if err := os.Remove(target); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Every other path is given a record its own age vouches for, so the only
	// thing this cell measures is the record under test.
	rec := make(map[string]observedEntry, len(state))
	for p, id := range state {
		if p == "src/util.go" {
			continue
		}
		rec[p] = observedEntry{id: id, recordedAt: now.Add(2 * eventsCoarseClockWindow)}
	}
	rec["src/util.go"] = observedEntry{id: identityOf(before), digest: digest, recordedAt: now}
	verifyBefore, _ := ContentVerificationCounters()
	changed, next := h.tree.ledgerDiff(rec, state, now)
	verifyAfter, _ := ContentVerificationCounters()

	if len(changed) != 0 {
		t.Fatalf("a path whose bytes could not be read was reported as changed: %v — a comparison that never happened is not evidence of a change", changed)
	}
	if verifyAfter != verifyBefore {
		t.Fatalf("the failed read was counted as a content verification (%d -> %d): the counter is the cost of READS, and none happened", verifyBefore, verifyAfter)
	}
	if got := next["src/util.go"]; got.digest != digest || !got.id.sameIdentity(identityOf(before)) {
		t.Fatalf("the record was renewed by a verification that failed: %+v, want the record under test unchanged (%+v)", got, rec["src/util.go"])
	}

	// The retry: the next observation sees the path gone, and THAT is the
	// observation that reports it — so suppressing the unverifiable verdict
	// above did not lose the change.
	state2, _, err := h.tree.observe()
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	changed2, _ := h.tree.ledgerDiff(next, state2, time.Now())
	found := false
	for _, p := range changed2 {
		if p == "src/util.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the removal was never reported: the observation that can see it named %v", changed2)
	}
}

// keysOf is a sorted-by-nothing snapshot of a record map's keys, for a failure
// message that names what the ledger actually holds.
func keysOf(m map[string]observedEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
