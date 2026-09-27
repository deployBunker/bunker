package fsclient

// ============================================================================
// BFS-046 — THE INVALIDATION TEST PROGRAM (client-side half).
//
// Server-side cells 1-3 live in internal/server/webdav/bfs046_program_test.go.
// This file carries the client-side half of the program:
//
//	cell 4  HALF-FILE     a mutate-in-place implementation must FAIL the cell
//	cell 8  CANCEL        a deliberately cancelled and a SIGKILLed reader
//	cell 9  SIZE RULE     the boundary itself, and skips counted by reason
//	cell 5  STAMPEDE      foreground latency under a burst   PENDING-UNTIL-037
//	cell 6  STOP          STOP IN FULL, and the two refused readings PENDING-037
//	cell 7  PROMOTION     exactly ONE fetch per path          PENDING-UNTIL-037
//
// THE RULE THIS PROGRAM IS BUILT ON: a test that cannot fail proves nothing.
// Each cell below names the defect it catches, and docs/evidence/BFS-046-arms.sh
// carries the SOURCE MUTATION that turns each claiming cell red, with an
// sha256-verified restore. A cell whose feature is not in the build yet is
// WRITTEN AND GATED, never weakened (see TestBFS046PendingUntilLandingGate).
//
// Landed code under test here: internal/fsclient/cache.go (BFS-038's immutable
// blob + single pointer swap, Stage/Commit/Abort, GetPinned/Pin/Unpin),
// internal/fsclient/hotpolicy.go (BFS-044: the pinned numbers and the two
// mount-time refusals), internal/fsclient/status.go (BFS-045).
// ============================================================================

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// CELL 4 — THE HALF-FILE CELL.
//
// the defect it catches: a refresh implemented as a MUTATION of the cached
// copy — "rewrite the file P's entry names" — so a reader that looked P up
// before the refresh reads a torn file. It is the difference between a
// representation (a new immutable blob + one pointer swap) and a lock, and a
// lock cannot save a reader that is already reading the bytes being rewritten.
//
// The CELL ITSELF is landed with BFS-038 and lives in cache_staged_test.go:
// TestAtomicRefreshHalfFileCell runs the shipped representation beside TWO
// mutate-in-place implementations written in that file
// (mutateOldBlobRefresh, earlyPublishRefresh), and fails if either control stops
// being caught ("NEGATIVE CONTROL IS BLIND"). TestConcurrentRefreshObservationMeasurement
// re-measures it as a count rather than a schedule.
//
// The program's contribution is the missing half: a SOURCE mutation that makes
// the shipped cell red, so the cell is not merely described as load-bearing.
// That mutation is docs/evidence/BFS-046-redproof-mutate-in-place.patch and it
// turns Cache.Stage into the mutate-in-place shape — the one an implementer
// arrives at by "optimising" Stage to reuse the blob the path already names.
// The arms script runs it, shows TestAtomicRefreshHalfFileCell FAIL, restores
// the file and verifies the sha256.
// ---------------------------------------------------------------------------

// TestBFS046Cell04HalfFileCellIsLoadBearing asserts the three things that make
// cell 4 a real gate rather than a passing test: the shipped representation is
// the one being driven, the mutate-in-place arms are present and declare
// themselves violations, and the shared judgement (not the arm) decides.
func TestBFS046Cell04HalfFileCellIsLoadBearing(t *testing.T) {
	// (a) The judgement is shared: the shipped arm and both controls are graded
	// by the SAME function, so a control cannot be graded more loosely than the
	// thing it exists to indict.
	if !halfFileViolation("partial") {
		t.Fatal("halfFileViolation does not call a partial read a violation")
	}
	if halfFileViolation("old") || halfFileViolation("new") {
		t.Fatal("halfFileViolation rejects one of the two complete contents: the judgement must be exactly `complete`")
	}

	// (b) The shipped arm publishes through the landed representation, and the
	// representation is what makes the reader's view atomic. Driven directly
	// here so a change to the test file cannot leave the program asserting
	// nothing.
	oldData := stagePayload(64<<10, 21)
	newData := stagePayload(96<<10, 22) // different length: a partial is unmistakable
	c := newStagedCache(t, 8<<20, 4<<20, 64, 2)
	const path = "hot/load-bearing"
	if _, err := c.Insert(path, HashBytes(oldData), oldData); err != nil {
		t.Fatalf("seed: %v", err)
	}

	observed := ""
	s, err := c.Stage(path, HashBytes(newData), int64(len(newData)))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := s.Write(newData[:len(newData)/2]); err != nil {
		t.Fatalf("write first half: %v", err)
	}
	// The half-way checkpoint: half the new bytes are ON DISK, nothing is
	// published, and a reader must therefore still see the complete old content.
	observed = observeOnce(c, path, oldData, newData)
	if halfFileViolation(observed) {
		t.Fatalf("a reader observed %q with a stage in flight: a refreshed file was visible half-written", observed)
	}
	// The staged bytes are reachable by NO reader: the index is the only path to
	// a blob, and the stage is not in it.
	if got, _, ok := c.Lookup(path); !ok || got != HashBytes(oldData) {
		t.Fatalf("the path index moved to the staged content before Commit: %q/%v", got, ok)
	}
	if _, err := s.Write(newData[len(newData)/2:]); err != nil {
		t.Fatalf("write second half: %v", err)
	}
	if _, err := s.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := observeOnce(c, path, oldData, newData); got != "new" {
		t.Fatalf("after Commit the path must serve the complete new content; observed %q", got)
	}
	// The content address must not be a lie: the blob named by the OLD hash
	// either still holds the old bytes or has been legitimately reclaimed
	// (nothing pinned it, so the swap took its last reference away) — it must
	// NEVER hold the new ones, which is the address stopping to describe its
	// content.
	oldBlob := filepath.Join(c.Dir(), CacheBlobDir, strings.TrimPrefix(HashBytes(oldData), HashPrefix))
	onDisk, err := os.ReadFile(oldBlob)
	switch {
	case err == nil && bytes.Equal(onDisk, oldData):
		// intact
	case os.IsNotExist(err):
		// reclaimed, with the old content address no longer referenced
	default:
		t.Fatalf("the blob named by the old content address is neither intact nor reclaimed (%v): the address stopped describing its content", err)
	}
}

// ---------------------------------------------------------------------------
// CELL 8 — CANCEL CELLS: a deliberate cancel AND a KILLED reader.
//
// the defect it catches: correctness that depends on the reader's LIFETIME. A
// design in which a reader takes a lock (an on-disk lock file, a refcount
// persisted outside the process, a "this blob is in use" marker) is correct
// while every process exits politely and corrupt or unreachable the moment one
// is SIGKILLed — and no cancel ever arrives for a dead process (PRD §2.8: "the
// absence of a cancel is never corrupting"). This cell kills a real reader
// process, mid-read, with SIGKILL, and then asks whether the blob, the path and
// the cache directory are still what they were.
//
// The deliberate-cancel half (a FUSE interrupt, and EINTR vs EIO as the
// kernel-visible distinction) is PENDING-UNTIL-BFS-039 and is gated below.
// ---------------------------------------------------------------------------

// TestBFS046HelperProcess is the killed reader/refresher itself, re-executed as
// a CHILD process rather than a goroutine: SIGKILL is the whole point, and a
// goroutine cannot be killed. It is skipped in the normal run of the suite.
func TestBFS046HelperProcess(t *testing.T) {
	mode := os.Getenv("BFS046_HELPER_MODE")
	if mode == "" {
		t.Skip("the BFS-046 helper process (driven by TestBFS046Cell08*; SIGKILLed mid-read on purpose)")
	}
	dir := os.Getenv("BFS046_HELPER_DIR")
	path := os.Getenv("BFS046_HELPER_PATH")
	hash := os.Getenv("BFS046_HELPER_HASH")

	c, err := OpenCache(CacheConfig{Dir: dir, MaxBytes: 64 << 20, MaxEntryBytes: 4 << 20, MaxEntries: 64, MaxInFlight: 2})
	if err != nil {
		fmt.Println("HELPER-ERROR open:", err)
		os.Exit(3)
	}
	switch mode {
	case "pin":
		if _, ok := c.GetPinned(path, hash); !ok {
			fmt.Println("HELPER-ERROR: could not pin", path)
			os.Exit(4)
		}
	case "stage":
		s, err := c.Stage(path, hash, int64(1<<20))
		if err != nil {
			fmt.Println("HELPER-ERROR stage:", err)
			os.Exit(5)
		}
		half := stagePayload(1<<20, 31)
		if _, err := s.Write(half[:len(half)/2]); err != nil {
			fmt.Println("HELPER-ERROR write:", err)
			os.Exit(6)
		}
	default:
		fmt.Println("HELPER-ERROR: unknown mode", mode)
		os.Exit(7)
	}
	// The child is now exactly what the cell wants to kill: it holds a pin (or a
	// stage with bytes on disk) and it will never release it.
	fmt.Println("HELPER-READY")
	_ = os.Stdout.Sync()
	select {} // killed by the parent; never returns
}

// bfs046SpawnHelper starts the helper process and returns once it reports that
// it holds what the cell wants to kill. The wait is explicit and bounded: a
// helper that cannot start is a failure of the cell, not a silent skip.
func bfs046SpawnHelper(t *testing.T, mode, dir, path, hash string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestBFS046HelperProcess", "-test.timeout=120s")
	cmd.Env = append(os.Environ(),
		"BFS046_HELPER_MODE="+mode,
		"BFS046_HELPER_DIR="+dir,
		"BFS046_HELPER_PATH="+path,
		"BFS046_HELPER_HASH="+hash,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("helper start: %v", err)
	}
	ready := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "HELPER-ERROR") {
				ready <- line
				return
			}
			if line == "HELPER-READY" {
				ready <- ""
				return
			}
		}
		ready <- "helper exited before it was ready"
	}()
	select {
	case msg := <-ready:
		if msg != "" {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			t.Fatalf("helper: %s", msg)
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Fatalf("helper never reported ready")
	}
	return cmd
}

// bfs046Kill is SIGKILL — the signal a cancel never arrives for.
func bfs046Kill(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_, _ = cmd.Process.Wait()
}

func bfs046Sha(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return HashBytes(b)
}

// bfs046Strays lists the staged-blob residue in the cache directory: the
// unpublished files a crash can leave, which the next OpenCache must sweep.
func bfs046Strays(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(dir, CacheBlobDir))
	if err != nil {
		t.Fatalf("read blobs dir: %v", err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), CacheStagePrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestBFS046Cell08KilledReaderLeavesTheBlobAndThePathIntact(t *testing.T) {
	dir := t.TempDir()
	const path = "hot/killed-reader"
	data := stagePayload(128<<10, 41)
	hash := HashBytes(data)

	c, err := OpenCache(CacheConfig{Dir: dir, MaxBytes: 64 << 20, MaxEntryBytes: 4 << 20, MaxEntries: 64, MaxInFlight: 2})
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	if _, err := c.Insert(path, hash, data); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = c.Close()

	blob := filepath.Join(dir, CacheBlobDir, strings.TrimPrefix(hash, HashPrefix))
	before := bfs046Sha(t, blob)

	// A REAL reader, pinned and reading, killed with SIGKILL in the middle.
	cmd := bfs046SpawnHelper(t, "pin", dir, path, hash)
	bfs046Kill(t, cmd)

	// (a) The blob is BYTE-IDENTICAL: a reader's death is not a write.
	if after := bfs046Sha(t, blob); after != before {
		t.Fatalf("the blob changed when a reader was killed: %s -> %s", before, after)
	}
	// (b) THE PATH IS STILL REACHABLE — from a FRESH handle, which is what a
	// mount process restarting actually does.
	fresh, err := OpenCache(CacheConfig{Dir: dir, MaxBytes: 64 << 20, MaxEntryBytes: 4 << 20, MaxEntries: 64, MaxInFlight: 2})
	if err != nil {
		t.Fatalf("reopen after the kill: %v", err)
	}
	defer fresh.Close()
	got, ok := fresh.Get(path, hash)
	if !ok {
		t.Fatal("after a killed reader the path is UNREACHABLE: correctness depends on the reader's lifetime (the defect this cell exists to catch)")
	}
	if !bytes.Equal(got, data) {
		t.Fatal("the path served different bytes after a killed reader")
	}
	// (c) No residue and no leak: the killed reader took nothing with it, and
	// left nothing behind.
	if stray := bfs046Strays(t, dir); len(stray) != 0 {
		t.Fatalf("a killed reader left staged residue %v: an unpublished blob left behind is unbounded disk growth (BFS-031's class)", stray)
	}
	// (d) A second reader can still pin the same blob: the pin is not a
	// resource the dead reader is holding.
	if _, ok := fresh.GetPinned(path, hash); !ok {
		t.Fatal("the blob could not be pinned after a killed reader: the pin is being held on the dead process's behalf")
	}
	fresh.Unpin(hash)
}

func TestBFS046Cell08KilledRefresherNeverPublishesAndIsSwept(t *testing.T) {
	dir := t.TempDir()
	const path = "hot/killed-refresher"
	oldData := stagePayload(96<<10, 51)
	oldHash := HashBytes(oldData)
	newData := stagePayload(128<<10, 52)
	newHash := HashBytes(newData)

	c, err := OpenCache(CacheConfig{Dir: dir, MaxBytes: 64 << 20, MaxEntryBytes: 4 << 20, MaxEntries: 64, MaxInFlight: 2})
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	if _, err := c.Insert(path, oldHash, oldData); err != nil {
		t.Fatalf("seed: %v", err)
	}
	usedBefore := c.Stats().UsedBytes
	_ = c.Close()

	// A refresher killed with bytes on disk and nothing published.
	cmd := bfs046SpawnHelper(t, "stage", dir, path, newHash)
	bfs046Kill(t, cmd)

	// The residue is real — that is the point of killing it here — and it must
	// be SWEPT, not published and not counted.
	if stray := bfs046Strays(t, dir); len(stray) == 0 {
		t.Fatal("the killed refresher left no staged residue: this arm cannot show the sweep works, so it proves nothing about abandonment")
	}
	reopened, err := OpenCache(CacheConfig{Dir: dir, MaxBytes: 64 << 20, MaxEntryBytes: 4 << 20, MaxEntries: 64, MaxInFlight: 2})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	// (a) NOTHING WAS PUBLISHED: the path still serves the complete OLD content.
	got, ok := reopened.Get(path, oldHash)
	if !ok || !bytes.Equal(got, oldData) {
		t.Fatalf("after a killed refresher the path no longer serves the old content (ok=%v)", ok)
	}
	if h, _, ok := reopened.Lookup(path); !ok || h != oldHash {
		t.Fatalf("the path index moved to the killed refresher's content: %q/%v — an abandoned refresh must never swap", h, ok)
	}
	// (b) The ephemeral bytes were removed, so the directory is what the
	// published accounting says it is.
	if stray := bfs046Strays(t, dir); len(stray) != 0 {
		t.Fatalf("the staged residue survived the reopen: %v (an unpublished blob left behind is unbounded disk growth)", stray)
	}
	// (c) The published figure does not count bytes that were never published...
	if got := reopened.Stats().UsedBytes; got != usedBefore {
		t.Fatalf("used_bytes = %d after the sweep, want %d: the abandoned refresh left the published figure changed", got, usedBefore)
	}
	// ... and neither does the reservation, which must return to its rest value.
	st := reopened.Stats()
	if st.ReservedBytes != st.UsedBytes {
		t.Fatalf("reserved_bytes = %d != used_bytes = %d at rest: an abandoned refresh's reservation survived (a permanent phantom reservation eventually refuses every refresh)", st.ReservedBytes, st.UsedBytes)
	}
}

// ---------------------------------------------------------------------------
// CELL 9 — THE SIZE RULE: every skip COUNTED BY REASON, and 8 MiB INCLUSIVE,
// asserted AT THE BOUNDARY (an off-by-one at the bound is invisible in a
// coverage number).
//
// the defect it catches: an exclusive comparison at the ceiling, so a file
// exactly at 8 MiB is silently never warmed — a one-byte difference no user can
// see and no coverage figure can detect — and a skip that happens without
// incrementing a reason, i.e. the BFS-032 shape (a counter that exists, is
// displayed, and can never move).
//
// LIVENESS SPLIT, stated rather than smoothed over:
//   - the PINNED NUMBERS and the inclusive rule are landed (BFS-044) and are
//     asserted here as numbers and as the reported rule;
//   - the two mount-time refusals (S-9/S-10, both naming both numbers) are
//     landed and are asserted AT THE EDGE: exactly at the bound is ACCEPTED,
//     one byte over is REFUSED. That is the boundary cell.
//   - the SKIP CENSUS (one cell per reason in S-12/P-13, each driving the live
//     path) is PENDING-UNTIL-BFS-037: there is no refresh to skip yet, so the
//     census is GATED below, never weakened into "the vocabulary is listed".
// ---------------------------------------------------------------------------

func bfs046HotEnvDefault() HotPolicyEnv {
	return HotPolicyEnv{
		Concurrency:        25,
		CacheMaxBytes:      DefaultCacheMaxBytes,
		CacheMaxEntryBytes: DefaultCacheMaxEntryBytes,
		OpTimeout:          30 * time.Second,
	}
}

func TestBFS046Cell09SizeRuleBoundaryIsInclusiveAndCountedByReason(t *testing.T) {
	env := bfs046HotEnvDefault()

	// (a) THE PINNED NUMBER, asserted against the literal: 8 MiB, not "about".
	p := DefaultHotPolicy()
	if p.MaxFileBytes != 8<<20 {
		t.Fatalf("hot_max_file_bytes default = %d, want 8388608 (8 MiB)", p.MaxFileBytes)
	}
	// (b) THE RULE IS DECLARED INCLUSIVE on the running policy, not merely in a
	// spec: a mount reports its own size rule, so a cell can read it back.
	if eff := p.Effective(env); !eff.SizeRuleInclusive {
		t.Fatal("the effective policy does not report the size rule as inclusive (S-2)")
	}
	// (c) THE BOUNDARY ITSELF. `at the bound` must be accepted and `one over`
	// must be refused, both naming both numbers — the two mount-time refusals
	// (S-9 against the cache bound, S-10 against the per-entry cap). The refusal
	// arms are ENABLED policies: a relation failure is a REFUSAL once the hot
	// path is armed and a REPORTED state while it is not (S-11), and both halves
	// are asserted below.
	armed := p
	armed.Enabled = true
	atEntryCap := armed
	atEntryCap.MaxFileBytes = env.CacheMaxEntryBytes // exactly at the per-entry cap
	if err := atEntryCap.Validate(env); err != nil {
		t.Fatalf("a size rule EXACTLY at the per-entry cap was refused (%v): the comparison at the bound is exclusive, which is the off-by-one this cell exists to catch (S-10 is `>`, not `>=`)", err)
	}
	overEntryCap := armed
	overEntryCap.MaxFileBytes = env.CacheMaxEntryBytes + 1
	err := overEntryCap.Validate(env)
	if err == nil {
		t.Fatal("a size rule ONE BYTE over the per-entry cap was accepted: S-10 must refuse here")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", overEntryCap.MaxFileBytes)) ||
		!strings.Contains(err.Error(), fmt.Sprintf("%d", env.CacheMaxEntryBytes)) {
		t.Fatalf("the S-10 refusal does not name both numbers: %v", err)
	}
	// The same edge while DISARMED: not a refusal, but a REPORTED relation
	// failure naming both numbers (S-11 — the mount still works, the hot path
	// says why it is inert). A silent false here would be the fabricated
	// measurement the standing rule forbids.
	if got := overEntryCap.Effective(env); got.ConfigState != HotConfigMisconfigured || len(got.Misconfigured) == 0 {
		t.Fatalf("a disarmed policy over the per-entry cap reported state=%q problems=%v, want misconfigured with the numbers", got.ConfigState, got.Misconfigured)
	}
	// And the same edge against the whole-cache bound (S-9), where a refresh
	// could never be the reason a later read is warm. The environment raises the
	// per-entry cap to the cache bound so S-10's condition is false and the edge
	// under test is S-9's alone — otherwise the cell would be asserting S-10's
	// boundary a second time and calling it S-9's.
	env9 := env
	env9.CacheMaxEntryBytes = env.CacheMaxBytes
	cacheEdge := armed
	cacheEdge.MaxFileBytes = env9.CacheMaxBytes
	if problems := cacheEdge.RelationProblems(env9); len(problems) != 0 {
		t.Fatalf("a size rule exactly at the cache bound reported a relation problem: %v (S-9 is `>`)", problems)
	}
	cacheOver := armed
	cacheOver.MaxFileBytes = env9.CacheMaxBytes + 1
	problems := cacheOver.RelationProblems(env9)
	if len(problems) == 0 {
		t.Fatal("a size rule over the cache bound reported no relation problem: S-9 must name it")
	}
	if !strings.Contains(strings.Join(problems, " "), fmt.Sprintf("%d", cacheOver.MaxFileBytes)) {
		t.Fatalf("the S-9 problem does not name the configured number: %v", problems)
	}
	if err := cacheOver.Validate(env9); err == nil {
		t.Fatal("an ARMED policy over the cache bound was accepted: S-9 must refuse the mount")
	}

	// (d) THE SKIP/VOCABULARY HALF IS GATED, not weakened. The census the spec
	// demands (S-12 skips, P-13 abandons, one cell per reason driving the LIVE
	// path) cannot be written against a refresh that does not exist.
	if bfs046HotRefreshIsWired() {
		t.Fatal("the hot refresh is wired: the skip-by-reason census is no longer pending and MUST be written (S-12/P-13; see TestBFS046PendingUntilLandingGate)")
	}
	t.Log("PENDING-UNTIL-BFS-037: the skip-by-reason census (S-12/P-13) and the refresh-side file-size boundary (S-3/S-4, `size <= hot_max_file_bytes`) need the refresh to exist. This cell asserts the pinned number, the declared inclusive rule, and the two mount-time refusals AT THE EDGE — the parts that are landed — and it does not pretend to assert the census.")
}

// ---------------------------------------------------------------------------
// CELLS 5, 6 AND 7 — PENDING.
//
// These cannot be written against a feature that is not in the build, and the
// brief for this row is explicit: write the cell and mark it pending, never
// weaken it. So the assertions are stated here in the form they must take, the
// gate below fails the moment the feature lands so a pending cell cannot be
// forgotten, and the CELL INVENTORY in docs/evidence/BFS-046-*.md records each
// one as `pending-until-037` (or `-039`) with the exact claim it will carry.
//
//	cell 5 STAMPEDE  — the requirement is FOREGROUND LATENCY under a burst of
//	                   invalidations, not aggregate throughput: an average that
//	                   looks fine while a single foreground read stalls is the
//	                   failure the cell must catch. The observable is the
//	                   foreground's own latency distribution against a
//	                   hot-path-disabled build (SPEC-hot-file-policy AC-9).
//	cell 6 STOP      — STOP IN FULL must actually stop, and the row names TWO
//	                   REFUSED readings that a cell must be able to tell apart
//	                   from the correct one: "drain-then-idle" (the depth decays
//	                   while new work is still admitted) and "let in-flight
//	                   finish" (the stop is unbounded in time). The cell must
//	                   assert the REFUSAL, not only the happy path (AC-8 ii/iii,
//	                   and AC-8 iv: a PROMOTED fetch survives the stop).
//	cell 7 PROMOTION — a directly-called path that is also queued is promoted,
//	                   and the single-flight invariant is EXACT: N concurrent
//	                   readers cause EXACTLY 1 fetch per path, counted at the
//	                   SERVER, with 1 leader + N-1 joins and
//	                   singleflight_entries back to 0 at rest (AC-11/AC-12).
//	                   "at most one" is not the claim.
// ---------------------------------------------------------------------------

// bfs046HotRefreshIsWired reports whether the refresh half of the hot-file
// subsystem (BFS-037) is present. It is the gate's probe.
//
// It looks for the IDENTIFIERS the spec pins and nothing else — not for "a new
// file appeared", because the fleet commits to this repository concurrently and
// a sibling landing an unrelated file in this package would then turn the gate
// into a false alarm for everybody. The probe is deliberately about the
// subsystem's own vocabulary: the per-path single-flight map (PR-4), the hot
// tracker persist file (H-19), and the queue's state name (Q-15).
func bfs046HotRefreshIsWired() bool {
	markers := []string{"singleflight", "hot.json", "hot_queue", "HotQueueDepth", "hotRefreshManager"}
	ents, err := os.ReadDir(".")
	if err != nil {
		return false
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		src := string(b)
		for _, m := range markers {
			if strings.Contains(src, m) {
				return true
			}
		}
	}
	return false
}

// TestBFS046Cell05StampedeForegroundLatencyUnderABurst — PENDING-UNTIL-BFS-037.
//
// the defect it catches: a refresh policy that looks healthy on an AVERAGE while
// a single foreground read stalls behind a burst of invalidations. This is why
// the cell is about the foreground's own LATENCY DISTRIBUTION and not about
// throughput or a refresh count: an aggregate that improves while one read
// stalls is exactly the failure the row's STAMPEDE cell is written for.
func TestBFS046Cell05StampedeForegroundLatencyUnderABurst(t *testing.T) {
	if bfs046HotRefreshIsWired() {
		t.Fatal("PENDING CELL NOW DUE: BFS-037 has landed, so this cell must be completed " +
			"against the live path rather than skipped. The claim it must carry: with a foreground " +
			"stream against a throttled server, the foreground's latency distribution is UNCHANGED " +
			"versus a hot-path-disabled build (SPEC-hot-file-policy AC-9) — assert the foreground's " +
			"own numbers, never a refresh starved/un-starved count. The control that makes it able to " +
			"fail: remove the yield check (P-7) and the foreground-latency arm must go red.")
	}
	t.Skip("PENDING-UNTIL-BFS-037: the burst of invalidations cannot refresh anything yet (no tracker, " +
		"no queue, no refresher), so the foreground's latency under a burst is not measurable. The cell " +
		"exists, its claim is above, and TestBFS046PendingUntilLandingGate fails the moment the " +
		"dependency lands so it cannot be skipped forever.")
}

// TestBFS046Cell06StopInFullRefusesTheTwoWrongReadings — PENDING-UNTIL-BFS-037.
//
// the defect it catches: a "stop" that silently lifts. §5.5 names TWO readings an
// implementer picks without noticing — *drain-then-idle* (the depth decays while
// new work is still admitted) and *let-in-flight-finish* (the stop is unbounded
// in time) — so a cell that asserts only the happy path cannot tell the correct
// definition from the plausible wrong one. The cell must assert the REFUSAL.
func TestBFS046Cell06StopInFullRefusesTheTwoWrongReadings(t *testing.T) {
	if bfs046HotRefreshIsWired() {
		t.Fatal("PENDING CELL NOW DUE: BFS-037 has landed, so this cell must be completed. The claims it " +
			"must carry: (i) new work refused AND COUNTED while stopped, level-triggered — a drain-then-idle " +
			"stop fails this; (ii) queue_depth == 0 within hot_stop_deadline, each dropped item counted by " +
			"reason `stopped`; (iii) an in-flight refresh abandoned with NO publish — the path's cached hash, " +
			"used_bytes and the reader's bytes bit-identical, in_flight_bytes back to 0, no temp blob, no slot " +
			"held — a let-in-flight-finish stop fails this; (iv) a PROMOTED fetch SURVIVES the stop; (v) both " +
			"stop/promotion orderings produce an identical reader outcome (AC-8 i–v, PR-14).")
	}
	t.Skip("PENDING-UNTIL-BFS-037: there is no queue to stop and no in-flight refresh to abandon. The cell " +
		"exists, both REFUSED readings are named above as assertions rather than prose, and the pending gate " +
		"fails when the dependency lands.")
}

// TestBFS046Cell07PromotionIsSingleFlightExactlyOneFetch — PENDING-UNTIL-BFS-037.
//
// the defect it catches: a promotion that DOUBLE-FETCHES — the queue's refresh and
// the direct read both issuing a GET for the same path, so promotion
// anti-optimises the hottest paths. The invariant is EXACT: N concurrent readers
// of one path produce exactly 1 bytes-fetching request, counted at the SERVER.
// "At most one" is NOT the claim.
func TestBFS046Cell07PromotionIsSingleFlightExactlyOneFetch(t *testing.T) {
	if bfs046HotRefreshIsWired() {
		t.Fatal("PENDING CELL NOW DUE: BFS-037 has landed, so this cell must be completed. The claim it " +
			"must carry: 32 concurrent readers of one cold tracked path with a queued refresh produce " +
			"EXACTLY 1 GET on the wire, asserted against the SERVER's own request count and not a client " +
			"counter, with 1 leader + 31 joins (singleflight_leaders_total / singleflight_joins_total) and " +
			"singleflight_entries back to 0 at rest (AC-11/PR-6/PR-8). The control that makes it able to " +
			"fail: disable the per-path map and the cell must observe 32 GETs.")
	}
	t.Skip("PENDING-UNTIL-BFS-037: there is no per-path single-flight map and no promotion path to drive, " +
		"so \"exactly one fetch\" has nothing to count. The cell exists and its control arm (disable the map " +
		"⇒ 32 GETs) is stated above; the pending gate fails when the dependency lands.")
}

// TestBFS046Cell08DeliberateCancelIsDistinguishableFromFailure — PENDING-UNTIL-BFS-039.
//
// the defect it catches: a deliberate cancel that is indistinguishable from a
// failure at the kernel boundary, so a caller cannot retry correctly (PRD §2.8:
// `EINTR` vs `EIO`), and a cancel that leaves the per-path commit lock held or
// the previous content half-published. The KILLED halves of the cancel cell are
// live above (8a/8b) precisely because they need no cancel to arrive at all.
func TestBFS046Cell08DeliberateCancelIsDistinguishableFromFailure(t *testing.T) {
	if bfs046CancelIOIsWired() {
		t.Fatal("PENDING CELL NOW DUE: BFS-039 has landed, so this cell must be completed. The claims it " +
			"must carry: a deliberate FUSE interrupt cancels the operation, releases the striped per-path " +
			"commit lock, leaves the PREVIOUS content intact (never a half-published state), and the error " +
			"returned to the kernel distinguishes `EINTR` from `EIO` (PRD §2.8 / R10); a duplicate or late " +
			"cancel is a no-op rather than an error, because an accidental cancel may never arrive at all.")
	}
	t.Skip("PENDING-UNTIL-BFS-039: no `EINTR` is produced anywhere in the tree yet, so the deliberate half " +
		"of the cancel cell cannot be driven. The killed halves (8a the reader, 8b the refresher) are LIVE " +
		"and are the half that needs no cancel to arrive.")
}

// TestBFS046Cell09SkipCensusByReasonIsDrivable — PENDING-UNTIL-BFS-037.
//
// the defect it catches: a skip that happens WITHOUT incrementing a reason — the
// BFS-032 shape, where a counter exists, is displayed, and can never move because
// a pre-filter upstream of it makes its increment unreachable — and dead
// vocabulary: a reason with no trigger that ships as a permanent zero. The census
// must drive the LIVE mount, one cell per reason.
func TestBFS046Cell09SkipCensusByReasonIsDrivable(t *testing.T) {
	if bfs046HotRefreshIsWired() {
		t.Fatal("PENDING CELL NOW DUE: BFS-037 has landed, so the census must be written. The claims it " +
			"must carry: one cell per reason in S-12 (`oversize_pre`, `oversize_after_head`, `untracked`, " +
			"`disarmed`, `stopped`, `cache_disabled`, `no_room`, `pinned_eviction`, `queue_full`, `resync`, " +
			"`tree_mismatch`, `not_found`, `replaced`) and per reason in P-13, each driving the LIVE mount " +
			"(a real read, a real invalidation, a real stop) and asserting the counter MOVED; plus the " +
			"refresh-side file-size boundary (S-3/S-4, `size <= hot_max_file_bytes`, INCLUSIVE). The single " +
			"most important control (AC-6): insert a pre-filter above any counter and the affected cell must " +
			"fail — BFS-032's shape reproduced as a regression arm.")
	}
	t.Skip("PENDING-UNTIL-BFS-037: there is no refresh to skip, so no skip reason has a reachable trigger " +
		"and a census would be counting zeros. Cell 9's LIVE half (the pinned number, the inclusive rule, " +
		"the two refusals AT THE EDGE) is asserted in TestBFS046Cell09SizeRuleBoundaryIsInclusiveAndCountedByReason " +
		"and that cell does not claim the census.")
}

// bfs046CancelIOIsWired reports whether BFS-039's cancel-IO vocabulary is present.
// The probe is the kernel-visible distinction the spec pins (PRD §2.8).
func bfs046CancelIOIsWired() bool {
	dirs := []string{".", "../fsmount"}
	markers := []string{"EINTR", "ErrInterrupted"}
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(d, name))
			if err != nil {
				continue
			}
			src := string(b)
			for _, m := range markers {
				if strings.Contains(src, m) {
					return true
				}
			}
		}
	}
	return false
}

// TestBFS046PendingUntilLandingGate is the gate that keeps the pending cells
// honest. It FAILS when a dependency of a pending cell lands, because at that
// moment the pending cell must be completed rather than left skipping forever:
// a `t.Skip` that outlives its feature is a green that means nothing, which is
// the exact class this row exists to close.
func TestBFS046PendingUntilLandingGate(t *testing.T) {
	if bfs046HotRefreshIsWired() {
		t.Fatal("BFS-037 has landed (a new source file exists in internal/fsclient). The pending cells " +
			"(5 stampede, 6 stop, 7 promotion-exactly-1, and cell 9's skip-by-reason census) must now be " +
			"written against the live path — see the CELL INVENTORY in docs/evidence/BFS-046-*.md for the " +
			"exact claim each one carries. This gate exists so a pending cell cannot be skipped forever.")
	}
	if bfs046CancelIOIsWired() {
		t.Fatal("BFS-039 has landed (an EINTR vocabulary exists). " +
			"TestBFS046Cell08DeliberateCancelIsDistinguishableFromFailure must now be completed.")
	}
	// The other dependency: the push form (BFS-036) is what cell 1's channel half
	// will become once it is served. Its absence is a named, landed fact today
	// (ops.go answers the `watch` op 501 capability_unavailable), and it is
	// asserted from the server side.
}
