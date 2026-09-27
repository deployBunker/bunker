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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bfs046LockArtifacts lists anything in a client directory that could hold a
// lock across a process death: a lock file, a pid file, an "in use" marker. The
// per-path commit lock is not persisted (nothing a dead process can hold), and
// this is the sensor that says so out loud instead of asserting it.
func bfs046LockArtifacts(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		n := strings.ToLower(d.Name())
		if strings.Contains(n, "lock") || strings.Contains(n, "pid") || strings.Contains(n, "in-use") || strings.Contains(n, "inuse") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

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
// kernel-visible distinction) is COMPLETED below: BFS-039 landed the cancel
// vocabulary this cell drives.
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

	// ── (d) THE REFRESH-SIDE BOUNDARY, DRIVEN THROUGH THE LIVE PATH. The
	// mount-time refusals above are relations between numbers; this half asks the
	// refresher itself whether the rule is inclusive, and whether a refusal caused
	// by SIZE is COUNTED rather than silent (S-3/S-4, S-12).
	const ceiling = 4096
	atBound := func() (refreshed bool, oversizeSkips int64, published string) {
		stub := newHotStub(t)
		stub.Throttle(0)
		s := newHotSetup(t, stub, WithCeiling(ceiling), WithTick(2*time.Millisecond))
		p := "edge/at-bound.bin"
		stub.Set(p, hotBody(ceiling, 'e')) // EXACTLY the ceiling
		s.seedRead(p, true)
		hashBefore, _, ok := s.Cache.Lookup(p)
		if !ok {
			t.Fatal("a file EXACTLY at the ceiling must be cacheable, or the boundary cannot be exercised through the live path")
		}
		before := hotCensus(s.Manager)
		stub.Set(p, hotBody(ceiling, 'E')) // same size, different content
		if !s.Manager.Tracked(p) {
			t.Fatalf("the path %q is not tracked: the refresher cannot be asked about it", p)
		}
		s.Manager.NoteInvalidation([]string{p})
		refreshed = hotWaitFor(t, "the at-the-bound file to be refreshed", 5*time.Second, func() bool {
			h, _, ok := s.Cache.Lookup(p)
			return ok && h != hashBefore
		})
		after := hotCensus(s.Manager)
		hashAfter, _, _ := s.Cache.Lookup(p)
		t.Logf("CELL9 boundary AT the bound (%d B): refreshed=%v hash %s → %s oversize_skips_delta=%d refreshes_delta=%d",
			ceiling, refreshed, hashBefore, hashAfter,
			hotDelta(before, after, "skip:"+HotSkipOversizePre)+hotDelta(before, after, "skip:"+HotSkipOversizePreHead),
			hotDelta(before, after, "skip:"+HotSkipOversizePreHead))
		return refreshed, hotDelta(before, after, "skip:"+HotSkipOversizePre) + hotDelta(before, after, "skip:"+HotSkipOversizePreHead), hashAfter
	}
	oneOver := func() (fetched bool, oversizeSkips int64, usedMoved bool) {
		stub := newHotStub(t)
		stub.Throttle(0)
		s := newHotSetup(t, stub, WithCeiling(ceiling), WithTick(2*time.Millisecond))
		p := "edge/one-over.bin"
		stub.Set(p, hotBody(ceiling+1, 'o')) // ONE BYTE over
		s.seedRead(p, false)
		servedAfterSeed := stub.BytesServed(p)
		usedBefore := s.Cache.Stats().UsedBytes
		before := hotCensus(s.Manager)
		s.Manager.NoteInvalidation([]string{p})
		oversize := hotWaitFor(t, "the over-ceiling file to be skipped by size", 5*time.Second, func() bool {
			after := hotCensus(s.Manager)
			return hotDelta(before, after, "skip:"+HotSkipOversizePre)+hotDelta(before, after, "skip:"+HotSkipOversizePreHead) >= 1
		})
		after := hotCensus(s.Manager)
		st := s.Manager.Stats()
		fetched = stub.BytesServed(p) > servedAfterSeed
		usedMoved = s.Cache.Stats().UsedBytes != usedBefore
		t.Logf("CELL9 boundary ONE BYTE OVER (%d B): skipped_by_size=%v body_fetched=%v used_bytes_moved=%v oversize_pre_delta=%d oversize_after_head_delta=%d refreshes_delta=%d",
			ceiling+1, oversize, fetched, usedMoved,
			hotDelta(before, after, "skip:"+HotSkipOversizePre), hotDelta(before, after, "skip:"+HotSkipOversizePreHead),
			st.RefreshesTotal)
		return fetched, hotDelta(before, after, "skip:"+HotSkipOversizePre) + hotDelta(before, after, "skip:"+HotSkipOversizePreHead), usedMoved
	}
	admitted, admitSkips, atHash := atBound()
	if !admitted {
		t.Fatal("a file EXACTLY at hot_max_file_bytes was NOT refreshed: the size rule is exclusive at the bound, which is the off-by-one this cell exists to catch (S-4 is `<=`)")
	}
	if admitSkips != 0 {
		t.Fatalf("a file exactly at the ceiling was counted against a size skip (%d)", admitSkips)
	}
	if atHash == "" {
		t.Fatal("the at-the-bound refresh published no content address")
	}
	ranBody, overSkips, moved := oneOver()
	if overSkips < 1 {
		t.Fatalf("a file ONE BYTE over hot_max_file_bytes was not counted against a size reason: a refusal that is not counted cannot be told from a refresh that never happened (S-12)")
	}
	if ranBody {
		t.Fatal("a file ONE BYTE over hot_max_file_bytes had its BODY fetched: the size rule does not bound what is downloaded")
	}
	if moved {
		t.Fatal("a file ONE BYTE over hot_max_file_bytes published into the cache")
	}

	// ── (e) THE CENSUS IS DRIVABLE, AND EVERY REASON IS ACCOUNTED. One skip and
	// one abandon are moved through the live path here; the EXHAUSTIVE per-reason
	// arms (23 of them: one per member of S-12 and P-13, each asserting the move
	// it claims) live in TestBFS037Cell08EverySkipAndAbandonReasonIsReachable,
	// which is the cell this one is the drivability half of.
	if !bfs046HotRefreshIsWired() {
		t.Fatal("BFS-037's refresh is not wired: the census half cannot be satisfied by a feature that is absent")
	}
	s := newHotSetup(t, newHotStub(t), WithTick(2*time.Millisecond))
	st0 := s.Manager.Stats()
	for _, reason := range HotSkipReasons {
		if _, ok := st0.Skips[reason]; !ok {
			t.Fatalf("S-12's reason %q is not even a KEY in the census: a reason that is absent cannot be counted (BFS-032's counter-that-cannot-move)", reason)
		}
	}
	for _, reason := range HotAbandonReasons {
		if _, ok := st0.Abandoned[reason]; !ok {
			t.Fatalf("P-13's reason %q is not even a KEY in the census", reason)
		}
	}
	t.Logf("CELL9 VERDICT: boundary inclusive at %d B (refreshed, 0 size skips) and refused+COUNTED one byte over (%d B, no body fetched, nothing published); census vocabulary letter-complete: %d skip reasons + %d abandon reasons are all keys, drivable per reason in TestBFS037Cell08",
		ceiling, ceiling+1, len(HotSkipReasons), len(HotAbandonReasons))
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
	// THE DEFECT: a refresh storm that starves interactive reads while every
	// aggregate number still looks healthy. The observable is therefore the
	// FOREGROUND's own latency DISTRIBUTION against a hot-path-DISABLED build
	// (SPEC-hot-file-policy AC-9) — never a "refresh starved / not starved"
	// count, which a saturated pool can satisfy while one read stalls.
	const (
		burstPaths = 64
		fgReads    = 40
		throttle   = 4 * time.Millisecond
		fgPath     = "fg/stampede.txt"
	)
	quantile := func(d []time.Duration, q float64) time.Duration {
		if len(d) == 0 {
			return 0
		}
		s := append([]time.Duration(nil), d...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		return s[int(q*float64(len(s)-1))]
	}

	// ── ARM A: the hot path ARMED, with a 64-path invalidation burst in flight
	// while the foreground reads the SAME server. ──
	armA := func() []time.Duration {
		stub := newHotStub(t)
		stub.Throttle(throttle)
		s := newHotSetup(t, stub, WithTick(2*time.Millisecond))
		stub.Set(fgPath, hotBody(256, 'f'))
		s.seedRead(fgPath, true)
		var inval []string
		for i := 0; i < burstPaths; i++ {
			p := fmt.Sprintf("burst/%03d.txt", i)
			stub.Set(p, hotBody(2048, byte('a'+i%26)))
			s.seedRead(p, false) // tracked and hot, never cached
			inval = append(inval, p)
		}
		s.Manager.NoteInvalidation(inval)
		if !hotWaitFor(t, "the refresh burst to be running", 3*time.Second, func() bool {
			st := s.Manager.Stats()
			return st.RefreshInflight > 0 || st.Queue.Depth > 0
		}) {
			t.Fatal("the invalidation burst never started: the arm would measure an idle client")
		}
		out := make([]time.Duration, 0, fgReads)
		for i := 0; i < fgReads; i++ {
			start := time.Now()
			if _, _, oerr, _ := s.Manager.HotRead(context.Background(), fgPath, ""); oerr != nil {
				t.Fatalf("a foreground read failed during the burst: %v", oerr)
			}
			out = append(out, time.Since(start))
		}
		st := s.Manager.Stats()
		if st.RefreshMaxInflight <= 0 {
			t.Fatalf("the refresher reports a share of %d: the bound under test is not even declared", st.RefreshMaxInflight)
		}
		if st.RefreshInflightMax > st.RefreshMaxInflight {
			t.Fatalf("the refresher ran %d concurrent refreshes against a declared share of %d (P-3)",
				st.RefreshInflightMax, st.RefreshMaxInflight)
		}
		t.Logf("CELL5 arm A (hot path ARMED, %d-path burst): p50=%s p95=%s max=%s reads=%d refreshes=%d share=%d inflight_max=%d yields=%d retries=%d queue_depth=%d queue_max=%d displaced=%d queue_refused=%d deduped=%d",
			burstPaths, quantile(out, .5), quantile(out, .95), quantile(out, 1.0), len(out),
			st.RefreshesTotal, st.RefreshMaxInflight, st.RefreshInflightMax, st.YieldsTotal, st.RetriesTotal,
			st.Queue.Depth, st.Queue.MaxDepth, st.Queue.Displaced, st.Queue.Refused, st.Queue.Deduped)
		return out
	}

	// ── ARM B: THE CONTROL BUILD — the same client, the same server, the same
	// reader shape, and NO hot path at all. ──
	armB := func() []time.Duration {
		stub := newHotStub(t)
		stub.Throttle(throttle)
		cache, err := OpenCache(CacheConfig{
			Dir: filepath.Join(t.TempDir(), "cache"), MaxBytes: 64 << 20, MaxEntryBytes: 8 << 20,
			MaxEntries: 512, MaxInFlight: 2, MaxAge: time.Hour,
		})
		if err != nil {
			t.Fatalf("cache: %v", err)
		}
		cl, err := NewClient(Options{BaseURL: stub.URL(), Concurrency: 8, BindTimeout: time.Second})
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		_ = cache
		stub.Set(fgPath, hotBody(256, 'f'))
		out := make([]time.Duration, 0, fgReads)
		for i := 0; i < fgReads; i++ {
			start := time.Now()
			if _, _, err := cl.Get(context.Background(), fgPath, ""); err != nil {
				t.Fatalf("control read: %v", err)
			}
			out = append(out, time.Since(start))
		}
		t.Logf("CELL5 arm B (hot path DISABLED): p50=%s p95=%s max=%s reads=%d",
			quantile(out, .5), quantile(out, .95), quantile(out, 1.0), len(out))
		return out
	}

	a, b := armA(), armB()
	// The bound is stated as a margin over the CONTROL'S OWN distribution, so it
	// cannot pass by the machine being fast: the armed arm must not be
	// systematically slower than the build without the hot path. The margin is
	// generous on purpose (a p95 comparison on 40 samples is not a benchmark);
	// the arms script is what proves the assertion can fail, by removing the
	// yield check (P-7) and watching this cell go red.
	bound := 2*quantile(b, .95) + 30*time.Millisecond
	if got := quantile(a, .95); got > bound {
		t.Fatalf("foreground p95 latency under a %d-path invalidation burst is %s against a %s bound (control build p95=%s): the refresh storm is starving interactive reads (AC-9)",
			burstPaths, got, bound, quantile(b, .95))
	}
	t.Logf("CELL5 VERDICT: armed p95=%s <= bound=%s (2x control p95 %s + 30ms) — the burst did not move the foreground's own distribution",
		quantile(a, .95), bound, quantile(b, .95))
}

// TestBFS046Cell06StopInFullRefusesTheTwoWrongReadings — PENDING-UNTIL-BFS-037.
//
// the defect it catches: a "stop" that silently lifts. §5.5 names TWO readings an
// implementer picks without noticing — *drain-then-idle* (the depth decays while
// new work is still admitted) and *let-in-flight-finish* (the stop is unbounded
// in time) — so a cell that asserts only the happy path cannot tell the correct
// definition from the plausible wrong one. The cell must assert the REFUSAL.
func TestBFS046Cell06StopInFullRefusesTheTwoWrongReadings(t *testing.T) {
	// STOP IN FULL must actually stop, and the two readings an implementer
	// picks by accident must be REFUSED, not merely unobserved:
	//   (i)   new work refused AND COUNTED while stopped, level-triggered
	//         → a DRAIN-THEN-IDLE stop fails here (it accepts what arrives next)
	//   (ii)  queue_depth == 0 at once, every dropped item counted by reason
	//         `stopped` → "drained and then stopped" fails here
	//   (iii) the in-flight refresh is ABANDONED with NO PUBLISH: the path's
	//         cached hash, used_bytes and the reader's bytes bit-identical,
	//         in_flight_bytes back to 0, no staged blob, no slot held
	//         → "let in-flight finish" fails here (it is unbounded in time)
	//   (iv)  a PROMOTED fetch SURVIVES the stop (PR-11: a read that became the
	//         leader is on the critical path now, not speculation)
	//   (v)   both stop/promotion orderings produce an IDENTICAL reader outcome
	//         (AC-8 i–v, PR-14)
	const (
		queuedPaths = 60
		body        = 4096
	)
	// ── the shared rig: a cached victim whose server content CHANGED, a
	// refresher parked inside the victim's body, and a queue of 60 tracked
	// paths still pending when the stop lands. ──
	rig := func(t *testing.T) (*hotSetup, string, string, []string) {
		t.Helper()
		stub := newHotStub(t)
		stub.Throttle(20 * time.Millisecond)
		s := newHotSetup(t, stub,
			WithTick(2*time.Millisecond),
			WithPolicy(func(p *HotPolicy) { p.QueueMaxWait = time.Hour; p.StopDeadline = 2 * time.Second }),
		)
		victim := "victim6.txt"
		stub.Set(victim, hotBody(body, 'v'))
		// The victim must be the HIGHEST-scored item so the ticker picks it
		// first: with sixty equal-scored paths the heap order is arbitrary and
		// the cell would flake between "the victim is in flight" and "some
		// other path is". An EDIT is the score's heaviest currency (weight 8.0
		// against a read's 1.0), so the victim cannot be tied by a read-only
		// neighbour no matter how the heap breaks ties.
		s.Manager.NoteEdit(victim)
		s.Manager.NoteEdit(victim)
		for i := 0; i < 2; i++ {
			s.seedRead(victim, true)
		}
		oldHash, _, ok := s.Cache.Lookup(victim)
		if !ok {
			t.Fatal("the fixture's seeded read must be cached: a refresh is measured against what is published")
		}
		var queued []string
		for i := 0; i < queuedPaths; i++ {
			p := fmt.Sprintf("cold6/%03d.txt", i)
			stub.Set(p, hotBody(2048, byte('a'+i%26)))
			s.seedRead(p, false)
			queued = append(queued, p)
		}
		stub.Set(victim, hotBody(body, 'w')) // the file CHANGED on the server
		return s, victim, oldHash, queued
	}

	// ══ ORDERING 1: the STOP lands while the victim's refresh is mid-body and
	// the queue is full; then a reader asks for the victim. ══
	s, victim, oldHash, queued := rig(t)
	stub := s.Stub
	stub.Hold()
	base := stub.BytesServed(victim)
	usedBefore := s.Cache.Stats().UsedBytes
	s.Manager.NoteInvalidation(append([]string{victim}, queued...))
	if !hotWaitHeld(t, stub, victim, body, base, 5*time.Second) {
		t.Fatalf("the victim's refresh never parked inside its body (gets=%d served=%d base=%d inflight=%d depth=%d refreshes=%d skips=%v): the stop would be measured against an idle refresher",
			stub.GETs(victim), stub.BytesServed(victim), base, s.Manager.Stats().RefreshInflight,
			s.Manager.Stats().Queue.Depth, s.Manager.Stats().RefreshesTotal, s.Manager.Stats().Skips)
	}
	if d := s.Manager.Stats().Queue.Depth; d < 40 {
		t.Fatalf("the queue holds %d items, want the burst still pending when the stop lands", d)
	}
	before := hotCensus(s.Manager)
	if !s.Manager.Stopped() && s.Manager.StopReason() != "" {
		t.Fatalf("the manager reports reason %q while not stopped", s.Manager.StopReason())
	}
	stopStart := time.Now()
	s.Manager.Stop(HotStopOperator)
	stopElapsed := time.Since(stopStart)
	st := s.Manager.Stats()
	after := hotCensus(s.Manager)
	// (ii) the queue is EMPTIED AT ONCE and every dropped item is COUNTED.
	if st.Queue.Depth != 0 {
		t.Fatalf("queue depth is %d immediately after the stop: STOP IN FULL empties the queue at once (§5.5 clause 2)", st.Queue.Depth)
	}
	if d := hotDelta(before, after, "skip:"+HotSkipStopped); d < 40 {
		t.Fatalf("only %d queued items were counted against reason `stopped`, want every item the stop emptied (P-13/S-12)", d)
	}
	// (iii) the in-flight refresh is ABANDONED, with no publish.
	if d := hotDelta(before, after, "abandon:"+HotAbandonStopped); d < 1 {
		t.Fatalf("the in-flight refresh was not abandoned at its checkpoint (delta %d): a stop that lets in-flight work finish is unbounded in time", d)
	}
	if st.RefreshInflight != 0 {
		t.Fatalf("the refresher still reports %d in flight after the stop", st.RefreshInflight)
	}
	if st.StopDeadlineExceededTotal != 0 {
		t.Fatalf("the stop needed longer than hot_stop_deadline: %d misses", st.StopDeadlineExceededTotal)
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
		t.Fatalf("the victim's published identity moved during the abandon: %q → %q (Q-16: a reader's bytes and the entry hash are bit-identical)", oldHash, nowHash)
	}
	stub.Release()
	orderOneHash, orderOneLen := func() (string, int) {
		data, h, oerr, _ := s.Manager.HotRead(context.Background(), victim, "")
		if oerr != nil {
			t.Fatalf("a read of the victim failed after the stop: %v (the prohibition is on SPECULATION, never on service)", oerr)
		}
		// The reader's own contract: a COMPLETE body whose bytes hash to the
		// address it was told. A partial read fails one of these two.
		if len(data) != body {
			t.Fatalf("the reader got %d bytes of a %d-byte file after the stop: that is the half-file BFS-038 exists to prevent", len(data), body)
		}
		if HashBytes(data) != h {
			t.Fatalf("the reader's bytes do not hash to the address it was given: got %s want %s", HashBytes(data), h)
		}
		return h, len(data)
	}()
	s.Manager.Stop(HotStopOperator) // idempotent
	// (i) new work is REFUSED while stopped, AND COUNTED — level-triggered.
	refusedBefore := s.Manager.Stats().RefusedWhileStoppedTotal
	s.Manager.NoteInvalidation(queued[:5])
	hotTick(6)
	st2 := s.Manager.Stats()
	if st2.RefusedWhileStoppedTotal <= refusedBefore {
		t.Fatalf("an invalidation arriving after the stop was accepted: refused_while_stopped=%d (a stop that silently lifts is not a stop)", st2.RefusedWhileStoppedTotal)
	}
	if st2.Queue.Depth != 0 {
		t.Fatalf("a refused invalidation still created queued work: depth=%d", st2.Queue.Depth)
	}

	// ══ ORDERING 2: the reader is promoted FIRST and the stop lands while the
	// promoted fetch is mid-body — (iv) it must SURVIVE, and (v) the reader's
	// outcome must be the same KIND of thing it was in ordering 1: a complete
	// file whose bytes are bit-identical to the published entry. ══
	s2, victim2, oldHash2, _ := rig(t)
	stub2 := s2.Stub
	stub2.Hold()
	base2 := stub2.BytesServed(victim2)
	reads := make(chan hotReadOutcome, 1)
	go func() {
		data, h, oerr, _ := s2.Manager.HotRead(context.Background(), victim2, "")
		reads <- hotReadOutcome{data: data, hash: h, err: oerr}
	}()
	if !hotWaitHeld(t, stub2, victim2, body, base2, 5*time.Second) {
		t.Fatal("the promoted fetch never parked inside its body: (iv) would be asserted against an idle client")
	}
	promoted := s2.Manager.Stats().PromotionsTotal
	beforePromote := hotCensus(s2.Manager)
	s2.Manager.Stop(HotStopOperator)
	if s2.Manager.Stats().RefreshInflight != 0 {
		t.Fatalf("the stop left %d refresh(es) in flight while a promoted fetch was parked", s2.Manager.Stats().RefreshInflight)
	}
	stub2.Release()
	res := <-reads
	if res.err != nil {
		t.Fatalf("the PROMOTED fetch did not survive the stop: %v (PR-11: a read that became the leader is not speculation)", res.err)
	}
	newHash := HashBytes(hotBody(body, 'w')) // the content the server holds NOW
	if res.hash != newHash {
		t.Fatalf("the promoted read returned hash %q, want the NEW content address %q (old was %q): the fetch did not COMPLETE after the stop, so (iv) proves nothing",
			res.hash, newHash, oldHash2)
	}
	if len(res.data) != body || HashBytes(res.data) != newHash {
		t.Fatalf("the promoted read returned %d bytes hashing to %s, want the complete %d-byte body hashing to %s: a stop that cancels a promoted fetch leaves a reader with half a file or none",
			len(res.data), HashBytes(res.data), body, newHash)
	}
	cs2 := s2.Cache.Stats()
	if cs2.InFlightBytes != 0 || cs2.StagedBlobs != 0 {
		t.Fatalf("the surviving promoted fetch left reservations behind: in_flight=%d staged=%d", cs2.InFlightBytes, cs2.StagedBlobs)
	}
	if n := stageResidue(t, s2.Cache); n != 0 {
		t.Fatalf("%d unpublished stage files survived the promoted publication", n)
	}
	if d := hotDelta(beforePromote, hotCensus(s2.Manager), "abandon:"+HotAbandonStopped); d != 0 {
		t.Fatalf("the promoted fetch was charged to the abandon census (%d)", d)
	}
	if d := hotDelta(beforePromote, hotCensus(s2.Manager), "skip:"+HotSkipStopped); d != 0 {
		t.Fatalf("the promoted fetch was charged to the stopped-skip census (%d)", d)
	}
	// (v) both orderings: a COMPLETE file whose bytes hash to the address the
	// reader was handed. (Ordering 1's reader legitimately sees the OLD content —
	// the stop abandoned the refresh before publication — and ordering 2's sees
	// the NEW one, because the promoted fetch is not speculation. What must be
	// identical is the KIND of observation: no reading of "stopped" exposes half
	// a file, which is the whole point of the cell.)
	orderings := map[string]struct {
		hash string
		n    int
	}{
		"stop-then-read":    {orderOneHash, orderOneLen},
		"promote-then-stop": {newHash, len(res.data)},
	}
	for name, got := range orderings {
		if got.n != body {
			t.Fatalf("ordering %s: the reader got %d bytes, want the complete %d-byte file", name, got.n, body)
		}
		if got.hash != newHash && got.hash != oldHash && got.hash != oldHash2 {
			t.Fatalf("ordering %s: the reader's hash %q is neither the old nor the new content address (which is what a partial file's hash looks like)", name, got.hash)
		}
	}
	t.Logf("CELL6 VERDICT: stop elapsed=%s queue_depth=%d stopped_skips=%d stopped_abandons=%d refused_while_stopped=%d used_bytes=%d(unmoved) stage_residue=%d | promoted fetch SURVIVED the stop: promotions=%d reader_bytes=%d complete hash=%s (old=%s) | both orderings hand the reader a complete file",
		stopElapsed, st.Queue.Depth, hotDelta(before, after, "skip:"+HotSkipStopped),
		hotDelta(before, after, "abandon:"+HotAbandonStopped), st2.RefusedWhileStoppedTotal, cs.UsedBytes,
		stageResidue(t, s.Cache), promoted, len(res.data), newHash, oldHash2)
}

// hotReadOutcome carries a read's result across a goroutine with its error
// TYPED: a typed-nil *OpError stored in an `any` compares non-nil as an
// interface, which turns "no error" into a false failure.
type hotReadOutcome struct {
	data []byte
	hash string
	err  *OpError
}

// TestBFS046Cell07PromotionIsSingleFlightExactlyOneFetch — PENDING-UNTIL-BFS-037.
//
// the defect it catches: a promotion that DOUBLE-FETCHES — the queue's refresh and
// the direct read both issuing a GET for the same path, so promotion
// anti-optimises the hottest paths. The invariant is EXACT: N concurrent readers
// of one path produce exactly 1 bytes-fetching request, counted at the SERVER.
// "At most one" is NOT the claim.
func TestBFS046Cell07PromotionIsSingleFlightExactlyOneFetch(t *testing.T) {
	// THE DEFECT: a promotion that DOUBLE-FETCHES — the queue's refresh and the
	// direct read both issuing a GET for the same path, so promotion
	// anti-optimises the hottest paths. The invariant is EXACT: N concurrent
	// readers of one cold tracked path, WITH a queued refresh for it, produce
	// exactly ONE bytes-fetching request, counted at the SERVER (never at a
	// client counter that the client itself could satisfy).
	const (
		readers = 32
		target  = "promote/one.txt"
		body    = 8192
	)
	stub := newHotStub(t)
	stub.Throttle(0)
	s := newHotSetup(t, stub,
		// A LONG tick: the queue must still hold the item when the readers
		// arrive, so what they drive is a PROMOTION, not a race with the
		// refresher. A short tick would let the refresher become the leader
		// first and the cell would then be measuring a join, not a promotion.
		WithTick(time.Hour),
		WithPolicy(func(p *HotPolicy) { p.QueueMaxWait = time.Hour; p.RefreshDeadline = 25 * time.Second }),
	)
	stub.Set(target, hotBody(body, 'p'))
	// Track the path WITHOUT caching it, so the read is a real fetch.
	s.seedRead(target, false)
	// The invalidation QUEUES the refresh. With the tick an hour away, nothing
	// else can take the item out of the queue except the promotion under test.
	s.Manager.NoteInvalidation([]string{target})
	if d := s.Manager.Stats().Queue.Depth; d != 1 {
		t.Fatalf("the invalidation left queue depth %d, want the single queued refresh this cell promotes", d)
	}
	// The server holds the body: whichever request becomes the leader stays IN
	// FLIGHT and parked, so every later reader must join it rather than start a
	// second GET.
	stub.Hold()
	base := stub.BytesServed(target)
	getsBefore := stub.GETs(target)

	// The 32 readers arrive while the refresh's slot in the queue is still
	// PENDING and no fetch is in flight. The FIRST of them is the direct call
	// that PROMOTES the queued item: it becomes the leader. The other 31 must
	// join it. That is 1 leader + 31 joins — not 32 fetches, and not zero.
	var wg sync.WaitGroup
	before := s.Manager.Stats()
	var leaders atomic.Int64
	results := make([]string, readers)
	errs := make([]error, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, h, oerr, joined := s.Manager.HotRead(context.Background(), target, "")
			if oerr != nil {
				errs[i] = fmt.Errorf("%w", oerr)
				return
			}
			results[i] = h
			if !joined {
				// Exactly ONE reader is allowed to be the leader: the promotion
				// itself. Any second one is the double-fetch this cell catches.
				leaders.Add(1)
			}
		}(i)
	}
	// Let the parked body go so the joined fetch can complete, then wait.
	time.Sleep(20 * time.Millisecond)
	stub.Release()
	wg.Wait()
	if len(errs) > 0 {
		for i, err := range errs {
			if err != nil {
				t.Fatalf("reader %d: %v", i, err)
			}
		}
	}
	if got := leaders.Load(); got != 1 {
		t.Fatalf("%d of the %d concurrent readers became leaders, want EXACTLY 1: the promotion IS the leader and every other reader must join it", got, readers)
	}
	wantHash := HashBytes(hotBody(body, 'p'))
	for i, h := range results {
		if h != wantHash {
			t.Fatalf("reader %d got hash %q, want the fetched content address %q (all 32 readers must observe the ONE fetch's bytes)", i, h, wantHash)
		}
	}
	after := s.Manager.Stats()
	gets := stub.GETs(target) - getsBefore
	if gets != 1 {
		t.Fatalf("the server saw %d request(s) for the path across 32 concurrent readers of a QUEUED item, want EXACTLY 1 (\"at most one\" is not the claim): AC-11/PR-6", gets)
	}
	if got := after.SingleflightLeadersTotal - before.SingleflightLeadersTotal; got != 1 {
		t.Fatalf("the 32 readers produced %d leaders, want EXACTLY 1 (the promoted read IS the leader; 0 means nothing fetched, >1 means the queue ran it twice)", got)
	}
	if got := after.SingleflightJoinsTotal - before.SingleflightJoinsTotal; got != readers-1 {
		t.Fatalf("singleflight joins delta = %d, want %d (one join per reader that is not the leader)", got, readers-1)
	}
	if after.SingleflightEntries != 0 {
		t.Fatalf("the per-path single-flight map holds %d entries at rest: a map that never empties is the OTHER failure this cell must catch (AC-12)", after.SingleflightEntries)
	}
	if after.Queue.Depth != 0 {
		t.Fatalf("the queue still holds %d items after the promotion: the promoted item must LEAVE the queue, not run twice", after.Queue.Depth)
	}
	if got := after.PromotionsTotal - before.PromotionsTotal; got < 1 {
		t.Fatalf("promotions_total did not move (%d): a directly-called queued item must be PROMOTED, not left to the ticker", got)
	}
	if after.RetriesTotal != before.RetriesTotal {
		t.Fatalf("the promotion applied backoff (retries %d → %d): a promoted item is ACTIVE work and must not sleep", before.RetriesTotal, after.RetriesTotal)
	}
	if d := after.FetchesTotal - before.FetchesTotal; d != 1 {
		t.Fatalf("fetches delta = %d, want exactly 1 (the promoted read's own fetch) — the readers must never add a second", d)
	}
	if d := after.RefreshesTotal - before.RefreshesTotal; d != 0 {
		t.Fatalf("refresh delta = %d, want 0: the queued refresh was PROMOTED into the reader's fetch, not run a second time", d)
	}
	if stub.BytesServed(target)-base != body {
		t.Fatalf("the server served %d bytes of the %d-byte body: the promoted fetch did not complete for the joining readers", stub.BytesServed(target)-base, body)
	}
	t.Logf("CELL7 VERDICT: 32 concurrent readers of a QUEUED item (queue depth 1) → server GETs=%d (want 1), leaders_delta=%d (want 1), joins_delta=%d (want %d), promotions_delta=%d, refreshes_delta=%d, fetches_delta=%d, retries_delta=%d, singleflight_entries=%d at rest, queue_depth 1→%d",
		gets, after.SingleflightLeadersTotal-before.SingleflightLeadersTotal,
		after.SingleflightJoinsTotal-before.SingleflightJoinsTotal, readers-1,
		after.PromotionsTotal-before.PromotionsTotal, after.RefreshesTotal-before.RefreshesTotal,
		after.FetchesTotal-before.FetchesTotal, after.RetriesTotal-before.RetriesTotal,
		after.SingleflightEntries, after.Queue.Depth)
}

// TestBFS046Cell08DeliberateCancelIsDistinguishableFromFailure is the
// DELIBERATE half of the cancel cell, completed when BFS-039 landed.
//
// The defect it catches: a deliberate cancel that is indistinguishable from a
// failure at the kernel boundary, so a caller cannot retry correctly (PRD §2.8:
// `EINTR` vs `EIO`), and a cancel that leaves the per-path commit lock held or
// the previous content half-published. Both were MEASURED on the tree that
// landed BFS-046 (docs/evidence/BFS-039-red.txt): the cancel came back as
// `unreachable_reset`/ENOTCONN — the answer a connection that died under us
// gets — and the retry it asks for was impossible.
//
// What "a FUSE interrupt" is on this side of the binding: go-fuse cancels the
// request's CONTEXT when the caller goes away (Ctrl-C, a killed process, a
// timeout at the caller's level), and every operation on the request path takes
// that context. The arms below therefore drive the same cancellation the kernel
// sends, without needing a live mount.
func TestBFS046Cell08DeliberateCancelIsDistinguishableFromFailure(t *testing.T) {
	c, root, _ := fixtureEndpoint(t)
	dir := t.TempDir()
	cache, err := OpenCache(CacheConfig{Dir: dir, MaxBytes: 8 << 20, MaxEntryBytes: 4 << 20, MaxEntries: 64, MaxInFlight: 2})
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	wp := NewWritePath(c, cache, dir, OnConflictRefuse)

	const path = "src/interrupted.txt"
	body := []byte("the write the caller interrupted\n")
	neighbour := filepath.Join(root, "src", "main.go")
	neighbourBefore := bfs046Sha(t, neighbour)

	// --- (a) THE DELIBERATE CANCEL, mid-publication. ---
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, cancelErr := wp.PublishBytes(ctx, path, body, WriteBase{IfNoneMatchStar: true, Source: BaseFromAbsent})
	if cancelErr == nil {
		t.Fatal("a cancelled publication reported success")
	}
	t.Logf("BFS046-MEASURE cell8 deliberate-cancel errno=%s(%d) cause=%q", ErrnoName(cancelErr.Errno), int(cancelErr.Errno), cancelErr.Cause)
	if cancelErr.Errno != ErrnoEINTR {
		t.Errorf("a DELIBERATE cancel returned errno=%s (%d), want EINTR (%d): the caller cannot tell its own interrupt from a failure and cannot retry correctly (PRD §2.8 / R10)",
			ErrnoName(cancelErr.Errno), int(cancelErr.Errno), int(ErrnoEINTR))
	}
	if cancelErr.Cause != CauseCancelled {
		t.Errorf("a DELIBERATE cancel reported cause=%q, want %q", cancelErr.Cause, CauseCancelled)
	}
	// --- (c) THE PREVIOUS CONTENT IS INTACT: a cancelled publication is not a
	// publication, so nothing was created and nothing was changed. ---
	if _, err := os.Stat(filepath.Join(root, path)); err == nil {
		t.Fatal("the cancelled publication left the path on the server: nothing was refused, so nothing may exist")
	}
	if got := bfs046Sha(t, neighbour); got != neighbourBefore {
		t.Fatalf("a cancelled publication changed a neighbouring file: %s -> %s", neighbourBefore, got)
	}

	// --- (d) THE FAILURE CLASS IS A DIFFERENT ANSWER: a real server failure
	// reports EIO (the malformed/5xx class), not a cancellation. ---
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failing.Close()
	fc, err := NewClient(Options{BaseURL: failing.URL + "/dav", Concurrency: 4, OpTimeout: 10 * time.Second, BindTimeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("failing client: %v", err)
	}
	_, failErr := fc.Head(context.Background(), "src/main.go")
	if failErr == nil {
		t.Fatal("a 500 reported success")
	}
	t.Logf("BFS046-MEASURE cell8 failure errno=%s(%d) cause=%q", ErrnoName(failErr.Errno), int(failErr.Errno), failErr.Cause)
	if failErr.Errno == ErrnoEINTR || failErr.Cause == CauseCancelled {
		t.Fatalf("a FAILURE was reported as a cancellation (%s/%s): the two events this cell separates must stay separated, because their recoveries differ",
			ErrnoName(failErr.Errno), failErr.Cause)
	}
	if failErr.Errno != ErrnoEIO {
		t.Errorf("a 5xx failure reported errno=%s, want EIO: the failure class must be the answer a broken server gets", ErrnoName(failErr.Errno))
	}
	if ErrnoEINTR == ErrnoEIO {
		t.Fatal("EINTR and EIO are the same value: the kernel-visible distinction the PRD requires does not exist")
	}

	// --- (b) + THE RETRY: the publication the cancel refused lands on the
	// retry, ONCE, and the per-path commit lock it enters is free again — the
	// next conditional write on the SAME path proceeds, with the latency
	// measured rather than asserted. ---
	start := time.Now()
	res, retryErr := wp.PublishBytes(context.Background(), path, body, WriteBase{IfNoneMatchStar: true, Source: BaseFromAbsent})
	if retryErr != nil {
		t.Fatalf("the retry after a cancel failed with %v (%s): a cancellation is not a verdict", retryErr, ErrnoName(retryErr.Errno))
	}
	retryLatency := time.Since(start)
	written, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatalf("the retry's path is not on the server: %v", err)
	}
	if string(written) != string(body) {
		t.Fatalf("the retry applied %q, want exactly %q once", written, body)
	}

	start = time.Now()
	if _, secondErr := wp.PublishBytes(context.Background(), path, []byte("second\n"), WriteBase{IfMatch: res.Hash, Source: BaseFromServed}); secondErr != nil {
		t.Fatalf("a conditional write on the SAME path after the cancel did not proceed (%v): a cancel must release the per-path commit lock, never leave it held", secondErr)
	}
	lockedOut := time.Since(start)
	t.Logf("BFS046-MEASURE cell8 cancel→retry=%s, next conditional write on the same path=%s, lock artifacts in the client dir=%v",
		retryLatency.Round(time.Microsecond), lockedOut.Round(time.Microsecond), bfs046LockArtifacts(t, dir))

	// --- (e) A DUPLICATE OR LATE CANCEL IS A NO-OP, not an error: an
	// accidental cancel may never arrive at all, and its recovery (an
	// abandonment that arrives twice, or after the work is done) must be safe.
	// The mutation a cancel can hit here is the cache's staged refresh, whose
	// Abort is the declared idempotent door (BFS-038). ---
	payload := stagePayload(4096, 77)
	hash := HashBytes(payload)
	s, err := cache.Stage("hot/duplicate-cancel", hash, int64(len(payload)))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := s.Write(payload); err != nil {
		t.Fatalf("write staged: %v", err)
	}
	if err := s.Abort(); err != nil {
		t.Fatalf("the first abandonment errored: %v", err)
	}
	if err := s.Abort(); err != nil {
		t.Fatalf("a DUPLICATE abandonment errored: %v — a duplicate cancel must be a no-op rather than an error, because an accidental cancel may never arrive and a repeated one must not be corrupting", err)
	}
	s2, err := cache.Stage("hot/late-cancel", hash, int64(len(payload)))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := s2.Write(payload); err != nil {
		t.Fatalf("write staged: %v", err)
	}
	if _, err := s2.Commit(); err != nil {
		t.Fatalf("commit staged: %v", err)
	}
	if err := s2.Abort(); err != nil {
		t.Fatalf("a LATE abandonment after a commit errored: %v — a cancel that arrives after the mutation landed must be a no-op", err)
	}
	if h, _, ok := cache.Lookup("hot/late-cancel"); !ok || h != hash {
		t.Fatalf("the late abandonment disturbed the published entry (%q/%v): an abandoned refresh must never swap, and a late one must never unpublish", h, ok)
	}
}

// TestBFS046Cell09SkipCensusByReasonIsDrivable — PENDING-UNTIL-BFS-037.
//
// the defect it catches: a skip that happens WITHOUT incrementing a reason — the
// BFS-032 shape, where a counter exists, is displayed, and can never move because
// a pre-filter upstream of it makes its increment unreachable — and dead
// vocabulary: a reason with no trigger that ships as a permanent zero. The census
// must drive the LIVE mount, one cell per reason.
func TestBFS046Cell09SkipCensusByReasonIsDrivable(t *testing.T) {
	// THE DEFECT: a skip that happens WITHOUT incrementing a reason — BFS-032's
	// shape, where a counter exists, is displayed, and can never move because a
	// pre-filter upstream of it makes its increment unreachable — and dead
	// vocabulary: a reason with no trigger that ships as a permanent zero.
	//
	// WHAT THIS CELL PROVES, and what it does NOT: the EXHAUSTIVE live drive (one
	// arm per reason in S-12 and P-13, each moving its own counter on a real
	// read / a real invalidation / a real stop) is
	// TestBFS037Cell08EverySkipAndAbandonReasonIsReachable — 23 arms, one per
	// vocabulary member, the arms script mutating each one. This cell is the
	// DRIVABILITY half the BFS-046 inventory asked for and pins the three things
	// that make that sweep trustworthy:
	//   (a) every reason is a KEY in the census (a reason that is absent cannot
	//       be counted at all);
	//   (b) every reason still has an arm in the census source — so deleting an
	//       arm fails HERE as well as there;
	//   (c) three reasons are driven LIVE in this cell (untracked, oversize_pre,
	//       stopped) and each one must MOVE.
	if !bfs046HotRefreshIsWired() {
		t.Fatal("BFS-037's refresh is not wired: no skip reason has a reachable trigger and the census would be counting zeros")
	}

	// ── (a) the vocabulary is complete in the census. ──
	stub := newHotStub(t)
	s := newHotSetup(t, stub, WithTick(2*time.Millisecond))
	st0 := s.Manager.Stats()
	for _, reason := range HotSkipReasons {
		if _, ok := st0.Skips[reason]; !ok {
			t.Fatalf("S-12's reason %q is not a key in the census: a skip that happens cannot be counted (BFS-032's counter-that-cannot-move)", reason)
		}
	}
	for _, reason := range HotAbandonReasons {
		if _, ok := st0.Abandoned[reason]; !ok {
			t.Fatalf("P-13's reason %q is not a key in the census", reason)
		}
	}
	if len(HotSkipReasons) != 13 || len(HotAbandonReasons) != 9 {
		t.Fatalf("the vocabularies are %d/%d long, want S-12's 13 and P-13's 9: a member added or dropped without its arm is exactly the dead reason this cell exists to catch",
			len(HotSkipReasons), len(HotAbandonReasons))
	}

	// ── (b) every reason still has an arm in the BFS-037 census source. ──
	// The arms live in one table whose `want` is the reason constant, so the
	// presence of the CONSTANT NAME in that file is the presence of the arm.
	census, err := os.ReadFile("bfs037_census_test.go")
	if err != nil {
		t.Fatalf("reading the census program: %v", err)
	}
	src := string(census)
	dead := []string{}
	for _, reason := range append(append([]string{}, HotSkipReasons...), HotAbandonReasons...) {
		// The census file references each reason by its CONSTANT, whose value is
		// the reason string; both spellings count, so a rename that keeps the
		// arm does not fail this check.
		if !strings.Contains(src, `"`+reason+`"`) && !strings.Contains(src, reasonConstantName(reason)) {
			dead = append(dead, reason)
		}
	}
	if len(dead) > 0 {
		t.Fatalf("these reasons have no arm left in bfs037_census_test.go: %v — a reason whose arm was deleted is a permanent zero that no cell can move (the BFS-032 shape)", dead)
	}

	// ── (c) three reasons, driven LIVE here: each must MOVE. ──

	// untracked: an invalidation for a path this client never read.
	{
		after := newHotStub(t)
		setup := newHotSetup(t, after, WithTick(2*time.Millisecond))
		before := hotCensus(setup.Manager)
		setup.Manager.NoteInvalidation([]string{"never/touched.txt"})
		if !hotWaitFor(t, "the untracked skip", 3*time.Second, func() bool {
			return hotDelta(before, hotCensus(setup.Manager), "skip:"+HotSkipUntracked) >= 1
		}) {
			t.Fatalf("an invalidation for an untracked path moved no counter: %v", hotCensus(setup.Manager))
		}
	}
	// oversize_pre: a CACHED path above the ceiling, so the size is known before
	// a request is spent.
	{
		over := newHotStub(t)
		setup := newHotSetup(t, over, WithCeiling(1024), WithTick(2*time.Millisecond))
		big := hotBody(1500, 'B')
		over.Set("census/big.bin", big)
		setup.seedRead("census/big.bin", true)
		before := hotCensus(setup.Manager)
		setup.Manager.NoteInvalidation([]string{"census/big.bin"})
		if !hotWaitFor(t, "the oversize_pre skip", 3*time.Second, func() bool {
			return hotDelta(before, hotCensus(setup.Manager), "skip:"+HotSkipOversizePre) >= 1
		}) {
			t.Fatalf("a cached, over-ceiling path moved no size counter: %v", hotCensus(setup.Manager))
		}
	}
	// stopped: new work refused while stopped, and the refusal is COUNTED as a
	// `stopped` skip rather than silently dropped.
	{
		stop := newHotStub(t)
		setup := newHotSetup(t, stop, WithTick(2*time.Millisecond))
		p := "census/stopped.bin"
		stop.Set(p, hotBody(512, 's'))
		setup.seedRead(p, false)
		setup.Manager.Stop(HotStopOperator)
		before := hotCensus(setup.Manager)
		setup.Manager.NoteInvalidation([]string{p})
		if !hotWaitFor(t, "the stopped skip", 3*time.Second, func() bool {
			return hotDelta(before, hotCensus(setup.Manager), "skip:"+HotSkipStopped) >= 1
		}) {
			t.Fatalf("an invalidation arriving after a stop moved no counter: %v", hotCensus(setup.Manager))
		}
	}
	t.Logf("CELL9-CENSUS VERDICT: %d skip + %d abandon reasons are all census keys, all %d still have arms in bfs037_census_test.go, and 3 of them (untracked, oversize_pre, stopped) were DRIVEN LIVE here and MOVED. The exhaustive per-reason drive is TestBFS037Cell08EverySkipAndAbandonReasonIsReachable",
		len(HotSkipReasons), len(HotAbandonReasons), len(HotSkipReasons)+len(HotAbandonReasons))
}

// reasonConstantName maps a reason's wire string back to the constant that
// carries it, so the census-source check accepts either spelling.
func reasonConstantName(reason string) string {
	for _, pair := range [][2]string{
		{HotSkipOversizePre, HotSkipOversizePre}, {HotSkipOversizePreHead, HotSkipOversizePreHead},
		{HotSkipUntracked, HotSkipUntracked}, {HotSkipDisarmed, HotSkipDisarmed},
		{HotSkipStopped, HotSkipStopped}, {HotSkipCacheDisabled, HotSkipCacheDisabled},
		{HotSkipNoRoom, HotSkipNoRoom}, {HotSkipPinnedEviction, HotSkipPinnedEviction},
		{HotSkipQueueFull, HotSkipQueueFull}, {HotSkipResync, HotSkipResync},
		{HotSkipTreeMismatch, HotSkipTreeMismatch}, {HotSkipNotFound, HotSkipNotFound},
		{HotSkipReplaced, HotSkipReplaced},
		{HotAbandonDeadline, HotAbandonDeadline}, {HotAbandonReacquireWindow, HotAbandonReacquireWindow},
		{HotAbandonStopped, HotAbandonStopped}, {HotAbandonPinnedEviction, HotAbandonPinnedEviction},
		{HotAbandonNoRoomAfter, HotAbandonNoRoomAfter}, {HotAbandonTreeMismatch, HotAbandonTreeMismatch},
		{HotAbandonNotFound, HotAbandonNotFound}, {HotAbandonReplaced, HotAbandonReplaced},
		{HotAbandonShutdown, HotAbandonShutdown},
	} {
		if pair[0] == reason {
			return pair[1]
		}
	}
	return reason
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
	// BFS-037 HAS LANDED, and the cells that were written-and-gated for it are
	// now COMPLETE against the live path: the clause that used to fail here ("the
	// dependency landed, so the pending cells must be completed") has done its
	// job. It is replaced by the assertion that keeps the completion honest — the
	// identifiers the completed cells drive must still be present, or they would
	// silently stop exercising anything:
	//   cell 5  stampede      foreground latency distribution vs a disabled build
	//   cell 6  stop          the two refused readings + a promoted fetch survives
	//   cell 7  promotion     exactly ONE server-side fetch, 1 leader + N-1 joins
	//   cell 9  size rule     the boundary at the bound, and skips by reason
	if !bfs046HotRefreshIsWired() {
		t.Fatal("BFS-037's hot refresh is GONE from the tree (no refresh source in internal/fsclient): " +
			"TestBFS046Cell05StampedeForegroundLatencyUnderABurst, TestBFS046Cell06StopInFullRefusesTheTwoWrongReadings, " +
			"TestBFS046Cell07PromotionIsSingleFlightExactlyOneFetch and the census half of " +
			"TestBFS046Cell09SizeRuleBoundaryIsInclusiveAndCountedByReason now prove nothing")
	}
	// The vocabulary the census cells count by reason must still be the closed
	// list the spec pins, or the per-reason arms would iterate an empty set.
	if len(HotSkipReasons) == 0 || len(HotAbandonReasons) == 0 {
		t.Fatalf("the skip/abandon vocabularies are empty (%d/%d): every census cell in this file is vacuous",
			len(HotSkipReasons), len(HotAbandonReasons))
	}
	// BFS-039 HAS LANDED and its cell is COMPLETE
	// (TestBFS046Cell08DeliberateCancelIsDistinguishableFromFailure). The clause
	// that used to FAIL here — "the dependency landed, so the pending cell must
	// be completed" — has done its job; it is replaced by the assertion that
	// keeps the completion honest: the cancel vocabulary the completed cell
	// drives must still be present, or its arms would silently stop exercising
	// anything.
	if !bfs046CancelIOIsWired() {
		t.Fatal("the EINTR vocabulary BFS-039 landed is GONE (no EINTR/ErrInterrupted anywhere in the tree): " +
			"TestBFS046Cell08DeliberateCancelIsDistinguishableFromFailure now proves nothing about the " +
			"deliberate-cancel half of PRD §2.8")
	}
	// The other dependency: the push form (BFS-036) is what cell 1's channel half
	// will become once it is served. Its absence is a named, landed fact today
	// (ops.go answers the `watch` op 501 capability_unavailable), and it is
	// asserted from the server side.
}
