package webdav

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// ---------------------------------------------------------------------------
// BFS-049 — TWO OBSERVERS OF ONE TREE, AND ONE IDENTITY.
//
// The hash cache (tree.go, hashFile) and the event ledger (events.go) are both
// metadata-keyed observers of the same served tree. They answer different
// questions for different consumers — the cache decides whether CONTENT is
// re-sent (a GET's ETag and its 304), the ledger decides whether a client is
// told to DROP — but they were keyed on different tuples: the cache on
// (size, mtime), the ledger on (size, mtime, ctime). SPEC-watcher-capability
// R-V6 says the derived artifacts must agree about what a change IS, and the
// class where they did not is one line long: an edit that preserves size and
// restores mtime moves the ledger and does not move the cache.
//
// Every function in this file is written against surface that existed BEFORE
// this row, so the same text runs on the fixed tree and on the filed production
// blob (the arms script swaps only tree.go, sha256-checked). That is what makes
// the RED reproducible from the FINAL test text instead of from a memory of it.
// The one arm that asserts surface this row ADDS lives in
// identity_report_bfs049_test.go, which the arms script sets aside for the RED
// run — the same split BFS-062 uses for its GREEN-only cells.
// ---------------------------------------------------------------------------

// probeSentinelBFS049 is not a hash any file has: it is planted in the cache
// entry for a path so a later read can say whether the cache was CONSULTED
// (the sentinel comes back) or bypassed/recomputed (it does not). The two
// behavioural arms below need exactly that distinction — "the cache hit" and
// "the cache missed" are not observable in a hash the caller cannot distinguish
// from a recomputation.
const probeSentinelBFS049 = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// plantHashCacheBFS049 overwrites the stored hash of the cache entry for abs.
// It fails loudly when there is no entry: a probe on a cold path would measure
// nothing and pass vacuously.
func plantHashCacheBFS049(t *testing.T, h *Handler, abs, hash string) {
	t.Helper()
	h.tree.mu.Lock()
	defer h.tree.mu.Unlock()
	e, ok := h.tree.cache[abs]
	if !ok {
		t.Fatalf("no hash-cache entry for %s: the read before this probe did not populate the cache, so the probe would measure nothing", abs)
	}
	e.hash = hash
	h.tree.cache[abs] = e
}

// moveCtimeOnlyBFS049 changes ONLY the path's ctime. A mode change moves the
// inode's change time and leaves both size and mtime alone, which makes it the
// isolating stimulus for this row: it is a move that a (size, mtime) key cannot
// see by construction, with no bytes changed at all.
//
// It reports (false, why) rather than skipping silently when the filesystem
// cannot express the move: a skip that fires without a reason would make both
// arms of this file vacuous, and this platform's ctime resolution is a fact the
// caller should be able to read. The move is retried because some filesystems
// stamp ctime too coarsely to separate two events inside one tick.
func moveCtimeOnlyBFS049(t *testing.T, path string) (bool, string) {
	t.Helper()
	first, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if ctimeUnixNano(first) == 0 {
		return false, "this platform exposes no ctime at all (filetime_other.go): both observers are (size, mtime) here and agree by being equally blind"
	}
	mode := first.Mode().Perm()
	for i := 0; i < 25; i++ {
		next := os.FileMode(0o600)
		if mode == 0o600 {
			next = 0o640
		}
		if err := os.Chmod(path, next); err != nil {
			t.Fatalf("chmod %s: %v", path, err)
		}
		now, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if now.Size() != first.Size() || !now.ModTime().Equal(first.ModTime()) {
			t.Fatalf("the chmod moved more than ctime: size %d -> %d, mtime %v -> %v", first.Size(), now.Size(), first.ModTime(), now.ModTime())
		}
		if ctimeUnixNano(now) != ctimeUnixNano(first) {
			if i > 0 {
				t.Logf("note: this filesystem needed %d mode change(s) to move ctime", i+1)
			}
			return true, ""
		}
		time.Sleep(3 * time.Millisecond)
	}
	return false, "25 mode changes over ~75 ms did not move ctime on this filesystem"
}

// ledgerVerdictBFS049 returns the ledger's answer to one poll as (event name,
// paths, whether the named path was reported).
func ledgerVerdictBFS049(t *testing.T, h *Handler, cursor int64, path string) (string, []string, bool) {
	t.Helper()
	got := pollAt(t, h, cursor)
	name, paths := "none", []string{}
	if len(got.Result.Events) > 0 {
		name = got.Result.Events[0].Event
		paths = got.Result.Events[0].Paths
	}
	for _, p := range paths {
		if p == path {
			return name, paths, true
		}
	}
	return name, paths, false
}

// TestBFS049TheTwoObserversCannotDisagreeOnASameSizeMtimePreservedEdit is this
// row's RED cell. It CONSTRUCTS the divergence instead of describing it: one
// edit — same byte count, different bytes, mtime restored to the nanosecond —
// is made on the tree under test, and BOTH observers are asked for their verdict
// on that same edit, with both verdicts printed.
//
// On the filed tree the printed lines are the defect: the cache answers the
// OLD hash (a GET would answer 304 with an ETag for bytes that are gone) while
// the ledger reports the path as moved. On the fixed tree the cache misses, the
// two verdicts agree, and the cell passes.
func TestBFS049TheTwoObserversCannotDisagreeOnASameSizeMtimePreservedEdit(t *testing.T) {
	h := newTestHandler(t)
	target := filepath.Join(h.Root(), "src", "util.go")
	cursor := seedWithSnapshot(t, h)

	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if ctimeUnixNano(before) == 0 {
		t.Skip("this platform exposes no ctime: both observers key on (size, mtime) and agree by being equally blind (filetime_other.go)")
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Observer D4, populated the way a GET populates it.
	d4Before, err := h.tree.hashFile(target)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}

	// THE EDIT: same byte count, different bytes, mtime restored to the
	// nanosecond — the one shape a (size, mtime) key cannot see.
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
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the trap did not hold: size %d/%d mtime %v/%v", before.Size(), after.Size(), before.ModTime(), after.ModTime())
	}

	// Observer D4's verdict on the edit: did the CACHE move?
	d4After, err := h.tree.hashFile(target)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	cacheMoved := d4After != d4Before

	// Observer D5's verdict on the SAME edit: did the LEDGER move?
	ledgerEvent, ledgerPaths, ledgerMoved := ledgerVerdictBFS049(t, h, cursor, "src/util.go")

	t.Logf("BFS-049: ONE edit on the tree under test — same size (%d bytes), mtime restored to the nanosecond.", len(body))
	t.Logf("  the bytes on disk:    before=%s", hashBytes(body))
	t.Logf("                        after =%s   (the served bytes did move)", hashBytes(edited))
	t.Logf("  observer D4 (hash cache -> whether content is RE-SENT, a GET's ETag/304): before=%s after=%s moved=%v", d4Before, d4After, cacheMoved)
	t.Logf("  observer D5 (event ledger -> whether a client is told to DROP):            event=%q paths=%v moved=%v", ledgerEvent, ledgerPaths, ledgerMoved)

	if !ledgerMoved {
		t.Errorf("the ledger did not report the edit, so this arm cannot construct the divergence it measures: event=%q paths=%v", ledgerEvent, ledgerPaths)
	}
	if !cacheMoved {
		t.Errorf("DIVERGENCE: the hash cache still answers %s for a path whose bytes are now %s, while the same server reported %q to a polling client — a GET can answer 304 with the ETag of bytes that are no longer on disk", d4Before, hashBytes(edited), ledgerPaths)
	}
}

// TestBFS049AMetadataOnlyMoveIsNotACacheHit isolates the identity MEMBER at
// issue rather than the edit shape: a mode change moves ctime and nothing else,
// so it is a move that a (size, mtime) key cannot see by construction and one
// on which no bytes changed at all. The planted sentinel makes the cache's
// verdict directly observable (a hit returns the sentinel; a miss returns the
// file's real hash), so the two observers' verdicts are read off one stimulus
// with no timing and no inference.
func TestBFS049AMetadataOnlyMoveIsNotACacheHit(t *testing.T) {
	h := newTestHandler(t)
	target := filepath.Join(h.Root(), "src", "util.go")
	cursor := seedWithSnapshot(t, h)

	trueHash, err := h.tree.hashFile(target)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	plantHashCacheBFS049(t, h, target, probeSentinelBFS049)
	// The probe must be able to see a HIT at all, or the arm below would pass
	// by measuring nothing.
	if got, err := h.tree.hashFile(target); err != nil || got != probeSentinelBFS049 {
		t.Fatalf("the probe is not the probe it claims to be: an UNCHANGED read must return the planted sentinel, got %q (err=%v)", got, err)
	}

	moved, why := moveCtimeOnlyBFS049(t, target)
	if !moved {
		t.Skipf("ctime cannot be moved without moving size or mtime here: %s", why)
	}

	got, err := h.tree.hashFile(target)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	cacheHit := got == probeSentinelBFS049
	ledgerEvent, ledgerPaths, ledgerMoved := ledgerVerdictBFS049(t, h, cursor, "src/util.go")

	t.Logf("BFS-049: ONE stimulus on the tree under test — a mode change, which moves ctime and nothing else.")
	t.Logf("  size and mtime are unchanged by the chmod (asserted inside the stimulus), no byte moved.")
	t.Logf("  observer D4 (hash cache): %s", map[bool]string{true: "cache HIT — it answered the sentinel it was planted with, i.e. it judged the path current", false: "cache MISS — it recomputed"}[cacheHit])
	t.Logf("  observer D5 (event ledger): event=%q paths=%v moved=%v", ledgerEvent, ledgerPaths, ledgerMoved)

	if cacheHit {
		t.Errorf("the cache answered a planted sentinel for a path whose ctime moved: its key cannot see the one field the change moved, so it is not the identity the ledger observes")
	}
	if got != trueHash {
		t.Errorf("the cache recomputed %s but the file's hash is %s", got, trueHash)
	}
	if !ledgerMoved {
		t.Errorf("the ledger did not report the metadata-only move: event=%q paths=%v", ledgerEvent, ledgerPaths)
	}
}

// TestBFS049AnUnchangedReadIsStillACacheHit is the attribution arm, and it is
// the one that stops the fix from being "make the cache never hit": a key that
// never matches would close the divergence by never caching, which is a
// performance regression wearing a correctness fix's clothes. It passes on both
// trees — the filed one and the fixed one — which is what makes it usable for
// attributing a failure in the mutation control: in the neutered tree the cache
// still hits here and still misses there, so a red arm there is the shared
// identity being bypassed, not the cache being disabled.
func TestBFS049AnUnchangedReadIsStillACacheHit(t *testing.T) {
	h := newTestHandler(t)
	target := filepath.Join(h.Root(), "src", "util.go")

	trueHash, err := h.tree.hashFile(target)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	plantHashCacheBFS049(t, h, target, probeSentinelBFS049)
	got, err := h.tree.hashFile(target)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	t.Logf("BFS-049: an untouched path read twice — planted sentinel=%s returned=%s", probeSentinelBFS049, got)
	if got != probeSentinelBFS049 {
		t.Fatalf("an UNCHANGED path did not hit the cache: the identity is matching on something that moves on its own, which turns every read into a rehash (the file's real hash is %s)", trueHash)
	}
}

// TestBFS049TheSharperIdentityCostsNoExtraSyscall is the row's blast-radius
// number, and it is a ceiling test rather than a claim: the identity costs no
// syscall because ctime is a field of the same syscall.Stat_t the existing
// os.Stat already returned, so a cache lookup is still ONE stat. What it can
// cost is a wider comparison, a wider cache entry and — where ctime moves
// without the bytes moving — one extra rehash. The first two are measured
// here; the third is bounded by the arms script's syscall counts.
//
// The whole-tree read is measured on the SAME fixture BFS-048 used (1000 and
// 10 000 files), so these numbers are comparable to that row's published
// figures rather than to a new yardstick.
func TestBFS049TheSharperIdentityCostsNoExtraSyscall(t *testing.T) {
	t.Logf("BFS-049 cost: one hash-cache entry = %d bytes (unsafe.Sizeof)", unsafe.Sizeof(hashEntry{}))

	h := newTestHandler(t)
	target := filepath.Join(h.Root(), "src", "util.go")
	if _, err := h.tree.hashFile(target); err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	// Every call below is a HIT: nothing about the file moves. This is the
	// lookup a response actually pays.
	hit := minPerCallBFS048(200, func() { _, _ = h.tree.hashFile(target) })
	t.Logf("BFS-049 cost: hashFile cache HIT = %s per call (one stat + one identity comparison)", hit)
	if hit > 20*time.Microsecond {
		t.Fatalf("a cache hit cost %s: a hit is one stat plus a comparison, not a tenth of a millisecond", hit)
	}

	// The one-call snapshot — the reason this project exists — on BFS-048's own
	// fixture, so the numbers below are directly comparable to its 98.9/119.0/
	// 167.4 ms at 10 106 entries.
	for _, n := range []int{1000, 10000} {
		c := measureTreeCostBFS048(t, n)
		t.Logf("BFS-049 cost: snapshot (no hashes) files=%d entries=%d whole_tree_read=%s", c.files, c.entries, c.wholeTree)
	}
	// And the same op WITH hashes, which is the shape that calls hashFile per
	// path — the only snapshot form where this row's identity is on the path.
	for _, n := range []int{1000, 10000} {
		d, entries := measureSnapshotWithHashesBFS049(t, n)
		t.Logf("BFS-049 cost: snapshot (include_hash) files=%d entries=%d =%s", n, entries, d)
	}
}

// measureSnapshotWithHashesBFS049 serves a tree of n files and takes the
// whole-tree snapshot with include_hash, so the per-path hashFile calls this
// row touched are exercised on the op that matters. It returns the elapsed
// whole-tree read and the entry count the answer reported.
func measureSnapshotWithHashesBFS049(t *testing.T, n int) (time.Duration, int) {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < n; i++ {
		dir := filepath.Join(root, fmt.Sprintf("d%02d", i%100))
		if i < 100 {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dir, err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.go", i)), []byte("package main\n"), 0o644); err != nil {
			t.Fatalf("write fixture file: %v", err)
		}
	}
	h := newTestHandler(t, func(c *Config) { c.Root = root })
	start := time.Now()
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "snapshot"},
		`{"path":".","depth":"infinity","include_hash":true}`)
	elapsed := time.Since(start)
	if rec.Code != 200 {
		t.Fatalf("snapshot(include_hash) -> %d %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result struct {
			Count int `json:"count"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("snapshot envelope: %v", err)
	}
	return elapsed, env.Result.Count
}
