package webdav

// ============================================================================
// BFS-046 — THE INVALIDATION TEST PROGRAM (server-side half).
//
// This row is the GATE: BFS-036/037/039/047 depend on it, so the cells exist
// before the features they judge. The program is written as CELLS WITH A NAMED
// DEFECT EACH, not as coverage: every cell below states, in one line, the
// defect it CATCHES, and the mutation that turns it red lives in
// docs/evidence/BFS-046-arms.sh (which also sha256-verifies every restore).
//
// The point of cells 1 and 2 is not that "a change is seen". It is that the
// writer is NOT WebDAV: cell 2 writes with /bin/sh and with a real `git
// checkout`, with no request of ours anywhere in the write path. A suite that
// only tests WebDAV-triggered invalidation proves nothing about the case the
// whole design was built for (PRD R1), so cell 2 also carries the CONTROL that
// makes it non-vacuous: on a tree with NO watcher, the same write moves nothing
// (the measured §2.4 blindness) — which is what attributes the movement to the
// watcher rather than to some other re-read of the tree.
//
// Live here (landed code: BFS-035 watcher, BFS-026 poll channel, BFS-043 config,
// BFS-045 status):
//
//	cell 1  SMOKE          the channel and the watcher come up, a change is seen
//	cell 2  INTEGRATION    a REAL non-WebDAV edit (shell write, git checkout)
//	cell 3  OVERFLOW       COUNTED FULL RESCAN and never reported quiet
//
// Cell 3's two arms are the landed cells of BFS-035
// (TestWatchOverflowForcesCountedFullRescanAndIsNeverQuiet, the fake-backend
// deterministic arm; TestWatchOverflowNegativeControlDisablesTheDrain, the
// control that makes the cell able to fail; TestWatchOverflowRealKernelReproducer,
// the REAL kernel overflow of SPEC-watcher-capability App. A.4). The program
// adds the arm those three lack: a mutation that neuters the RESCAN (rather than
// the drain) must turn the counted-full-rescan assertion red, and that mutation
// is BFS-046-redproof-rescan.patch.
//
// Cell 5/6/7 (stampede, stop, promotion) and the refresh half of cell 9 are
// PENDING-UNTIL-037/039 and live in internal/fsclient/bfs046_program_test.go;
// they are gated, not weakened — see TestBFS046PendingUntilLandingGate.
// ============================================================================

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ---------------------------------------------------------------------------
// The instrument: a REAL watcher over a real directory.
//
// Every cell in this file drives the production backend (inotify via fsnotify)
// rather than the injected fake, because the defect class cells 1-2 exist for —
// "the channel was wired to the request handler instead of to the filesystem" —
// is invisible to a fake backend: a fake only publishes what the test hands it.
// ---------------------------------------------------------------------------

func bfs046RealWatch(t *testing.T, root string, tune func(*watchOptions)) (*Handler, *watcher) {
	t.Helper()
	h := newTestHandler(t, func(c *Config) { c.Root = root })
	opts := defaultWatchOptions()
	opts.flushEvery = 5 * time.Millisecond
	if tune != nil {
		tune(&opts)
	}
	w := h.startWatch(defaultWatchEnv(), opts)
	t.Cleanup(func() { h.Close() })
	if st := w.statusSnapshot(); st.State != WatchStateWatching {
		t.Skipf("no real watch backend is establishable on this host (%s: %s) — the cell cannot run, and it says so rather than passing", st.Reason, st.Detail)
	}
	return h, w
}

// bfs046ShellWrite is the NON-WebDAV writer: a real shell, on the target, with
// no request of ours in the path. The body is passed as an ARGUMENT rather than
// interpolated into the script text, so a body containing shell metacharacters
// cannot turn the writer into something other than a plain write.
func bfs046ShellWrite(t *testing.T, abs, body string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", `printf '%s' "$1" > "$2"`, "sh", body, abs)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shell write to %s: %v (%s)", abs, err, out)
	}
}

// bfs046EventsPoll is one call of the CHANNEL in its landed poll form, returned
// as the parsed envelope. It is the mechanism half of cell 1: the watcher can be
// up and the channel still not answer.
func bfs046EventsPoll(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "events"}, "")
	if rec.Code != 200 {
		t.Fatalf("the channel's poll form answered %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("events envelope: %v (%s)", err, rec.Body.String())
	}
	if !env.OK {
		t.Fatalf("the channel's poll form answered ok=false: %s", rec.Body.String())
	}
	return env.Result
}

// ---------------------------------------------------------------------------
// CELL 1 — SMOKE.
//
// the defect it catches: the channel and the watcher never come up together, so
// every later cell is measuring a dead surface. Concretely: a watcher that
// reports `watching` with no backend installed, a channel whose op is refused,
// or a change that reaches neither the served revision nor the ledger.
// ---------------------------------------------------------------------------

func TestBFS046Cell01SmokeWatcherAndChannelComeUp(t *testing.T) {
	root := fixtureTree(t)
	withGitFixture(t, root, strings.Repeat("ab", 20))
	h, w := bfs046RealWatch(t, root, nil)

	// (a) THE WATCHER comes up, and says so through the production probe.
	st := w.statusSnapshot()
	if st.State != WatchStateWatching {
		t.Fatalf("watcher state = %q, want watching (%s)", st.State, st.Detail)
	}
	if st.Backend != watchBackendInotify {
		t.Fatalf("backend = %q, want %q on this build", st.Backend, watchBackendInotify)
	}
	if st.Reason != "" {
		t.Fatalf("a WATCHING watcher carries a refusal reason (%q): an absence and a healthy watcher must be distinguishable", st.Reason)
	}
	if st.Coverage == nil || st.Coverage.DirectoriesWatched == 0 {
		t.Fatalf("the watch set covers nothing: %+v", st.Coverage)
	}
	if st.Coverage.DirectoriesWatched != st.Coverage.DirectoriesDesired {
		t.Fatalf("coverage %d of %d on a healthy tree: a short set is W-4, never `watching`",
			st.Coverage.DirectoriesWatched, st.Coverage.DirectoriesDesired)
	}

	// (b) THE CHANNEL comes up: the poll form answers, and the capability
	// document tells a client what it is talking to.
	first := bfs046EventsPoll(t, h)
	if _, ok := first["events"]; !ok {
		t.Fatalf("the channel's poll form answered without an `events` array: %v", first)
	}

	// And the capability document reports the RUNNING watcher, so a client can
	// discover it without inferring it from a flag (SPEC-watcher §8.1).
	caps := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "capabilities"}, "")
	if caps.Code != 200 || !strings.Contains(caps.Body.String(), `"state":"watching"`) {
		t.Fatalf("the capability document does not report state=watching: %d %s", caps.Code, caps.Body.String())
	}

	// (c) A CHANGE IS SEEN — made by a real shell on the target, not by us.
	rev0 := h.tree.revToken()
	abs := filepath.Join(root, "src", "main.go")
	bfs046ShellWrite(t, abs, "package main\n\nfunc main() { /* smoke */ }\n")

	waitFor(t, 5*time.Second, "the served revision to move for a real shell write", func() bool {
		return h.tree.revToken() != rev0
	})
	if got := h.tree.watched.Load(); got == 0 {
		t.Fatal("the watched-change count never moved: the token's watcher half is not wired (D1/D2)")
	}

	// The channel carries it too (D5): the ledger is what a POLLER reads, so a
	// client that never subscribes still learns about the change.
	waitFor(t, 5*time.Second, "the change to reach the ledger the channel serves", func() bool {
		line := lastLedgerEventOfKind(t, h, eventInvalidate)
		return line != nil && containsString(line.Paths, "src/main.go")
	})

	// The stage the row is really about: the token must be DECLARED as composite
	// while a watcher is live, or a client cannot tell this coverage from the
	// un-extended token it already understands (R-V3).
	if got := h.revKind(); got != "git+watch" {
		t.Fatalf("revKind() = %q, want git+watch while a watcher is live", got)
	}
}

// ---------------------------------------------------------------------------
// CELL 2 — INTEGRATION OVER A REAL NON-WebDAV WRITE.
//
// the defect it catches: an invalidation path wired to the REQUEST HANDLER
// instead of to the filesystem. That implementation passes every WebDAV-driven
// test in the suite and is blind to `git checkout`, an editor, a cron job and a
// deploy — i.e. to everything PRD R1 was written for. The writer in this cell is
// a shell and `git`; this test issues NO request that mutates the tree.
//
// THE CONTROL (which is what makes the cell non-vacuous): the same write on the
// same tree with NO watcher must move NOTHING. If it moved the token anyway, the
// movement would be an artefact of something else and the cell would be proving
// the wrong thing.
// ---------------------------------------------------------------------------

func TestBFS046Cell02OutOfBandEditOverRealFilesystem(t *testing.T) {
	t.Run("a shell write on the target", func(t *testing.T) {
		root := fixtureTree(t)
		withGitFixture(t, root, strings.Repeat("ab", 20))
		h, _ := bfs046RealWatch(t, root, nil)
		// Prime the ledger's baseline, so the change this cell makes cannot be
		// confused with the ledger's own first observation.
		if _, err := h.tree.pollEvents(resumePoint{}); err != nil {
			t.Fatalf("prime the ledger: %v", err)
		}

		rev0 := h.tree.revToken()
		abs := filepath.Join(root, "src", "util.go")
		bfs046ShellWrite(t, abs, "package main\n\nfunc util() { /* written by /bin/sh, not by WebDAV */ }\n")

		waitFor(t, 5*time.Second, "the watcher to vouch for the shell write", func() bool {
			return h.tree.revToken() != rev0
		})
		// The served BYTES moved too: the server's own derived state was aligned
		// (D4 forget + D5 ledger), which is R2's second half.
		rec := do(t, h, "GET", "/dav/src/util.go", nil, "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "/bin/sh, not by WebDAV") {
			t.Fatalf("the served read did not see the out-of-band bytes: %d %s", rec.Code, rec.Body.String())
		}

		// THE CONTROL: no watcher, the identical write, and nothing moves.
		control := newTestHandler(t, func(c *Config) { c.Root = root })
		t.Cleanup(func() { control.Close() })
		if control.watch != nil {
			t.Fatal("the control tree has a watcher; it must not, or it proves nothing")
		}
		controlRev := control.tree.revToken()
		abs2 := filepath.Join(root, "src", "main.go")
		bfs046ShellWrite(t, abs2, "package main\n\nfunc main() {}\n// control write\n")
		time.Sleep(50 * time.Millisecond)
		if got := control.tree.revToken(); got != controlRev {
			t.Fatalf("CONTROL FAILED: with no watcher the revision moved (%q -> %q). The cell's movement must be attributable to the watcher", controlRev, got)
		}
		if got := control.revKind(); got != "git" {
			t.Fatalf("CONTROL FAILED: revKind = %q with no watcher, want the un-extended `git`", got)
		}
	})

	t.Run("a real git checkout", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git is not on PATH: the git writer cannot run and the cell says so rather than passing on the shell arm's authority")
		}
		repo := t.TempDir()
		bfs046Git(t, repo, "init", "-q")
		bfs046Git(t, repo, "config", "user.email", "bfs046@example.invalid")
		bfs046Git(t, repo, "config", "user.name", "BFS-046")
		tracked := filepath.Join(repo, "tracked.go")
		if err := os.WriteFile(tracked, []byte("package main\n\nconst Committed = 1\n"), 0o644); err != nil {
			t.Fatalf("write tracked file: %v", err)
		}
		bfs046Git(t, repo, "add", "tracked.go")
		bfs046Git(t, repo, "commit", "-q", "-m", "init")

		h, _ := bfs046RealWatch(t, repo, nil)
		if _, err := h.tree.pollEvents(resumePoint{}); err != nil {
			t.Fatalf("prime the ledger: %v", err)
		}

		// A dirty working tree, then a `git checkout -- .` that restores it.
		// The WRITER IS GIT: it rewrites the file's bytes in place, exactly as a
		// developer's branch switch does, and the served path resolves to what it
		// wrote.
		if err := os.WriteFile(tracked, []byte("package main\n\nconst Committed = 2\n"), 0o644); err != nil {
			t.Fatalf("dirty the tree: %v", err)
		}
		waitFor(t, 5*time.Second, "the dirty write to be vouched for", func() bool {
			return h.tree.watched.Load() > 0
		})
		before := h.tree.watched.Load()

		bfs046Git(t, repo, "checkout", "--", "tracked.go")

		waitFor(t, 5*time.Second, "the git checkout to be vouched for", func() bool {
			return h.tree.watched.Load() > before
		})
		rec := do(t, h, "GET", "/dav/tracked.go", nil, "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Committed = 1") {
			t.Fatalf("the served read did not see the checkout's bytes: %d %s", rec.Code, rec.Body.String())
		}
	})
}

func bfs046Git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_DATE=2026-09-27T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-09-27T00:00:00Z",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
	}
}

// ---------------------------------------------------------------------------
// CELL 3 — THE OVERFLOW CELL, as the program states it.
//
// Beyond BFS-035's three landed arms (deterministic fake-backend overflow, its
// disableDrain control, and the real-kernel reproducer), the program pins the
// one shape the row's text asks for by name: "with the rescan neutered the cell
// goes red". A drain that OBSERVES the marker while the rescan does nothing is a
// counted overflow with no rescan — i.e. exactly the state the guarantee forbids
// ("an overflow is always a full rescan"). This cell asserts the three facts the
// neutered-rescan mutant must break, on the DETERMINISTIC arm, so the mutation
// red-proof in the arms script is reproducible without a kernel flood:
//
//	the rescan is counted (rescans_total == 1)
//	it published ONE `overflow` line with an EMPTY path list, never a partial one
//	it actually re-observed the tree (the unseen change is now aligned with)
// ---------------------------------------------------------------------------

func TestBFS046Cell03OverflowIsACountedFullRescanAndNeverQuiet(t *testing.T) {
	gate := make(chan struct{})
	cell := newWatchCell(t, func(o *watchOptions) {
		// This cell is about the KERNEL's overflow: a long heartbeat keeps the
		// stall detector from contributing a second unvouched interval, and the
		// gate parks the event loop so the rescan cannot run until released.
		o.heartbeat = 30 * time.Second
		o.loopGate = gate
	}, nil)

	waitFor(t, 5*time.Second, "the watcher to be established", func() bool {
		return cell.status().State == WatchStateWatching
	})
	if _, err := cell.h.tree.pollEvents(resumePoint{}); err != nil {
		t.Fatalf("prime the ledger: %v", err)
	}
	rev0 := cell.h.tree.revToken()

	// The change made while the queue was dropping: reported on NO channel.
	oob := filepath.Join(cell.root, "src", "unseen.txt")
	mustWrite(t, oob, "written while the kernel queue was dropping\n")
	cell.fw.backend(0).pushError(fsnotify.ErrEventOverflow)

	// The notice arrives on the ERRORS channel although the loop is parked: the
	// drain is a goroutine of its own (O-1), and that is what makes the interval
	// reportable at all.
	waitFor(t, 5*time.Second, "the overflow to be counted while the loop is parked", func() bool {
		return cell.counters().OverflowsTotal == 1
	})
	if st := cell.status(); st.State != WatchStateOverflow || st.Vouched {
		t.Fatalf("state/vouched = %q/%v after an overflow, want overflow/false: an unvouched interval was reported as quiet", st.State, st.Vouched)
	}
	if got := cell.counters().RescansTotal; got != 0 {
		t.Fatalf("rescans_total = %d before the loop was released: the rescan must be the loop's work, not the drain's", got)
	}

	close(gate)
	waitFor(t, 5*time.Second, "the forced full rescan, counted", func() bool {
		return cell.counters().RescansTotal == 1
	})
	line := lastLedgerEventOfKind(t, cell.h, eventOverflow)
	if line == nil {
		t.Fatal("the rescan published no `overflow` line: the interval it could not vouch for was never announced")
	}
	if len(line.Paths) != 0 {
		t.Fatalf("the rescan published %d paths (%v): an overflow is never a partial list", len(line.Paths), line.Paths)
	}
	if got := cell.h.tree.revToken(); got == rev0 {
		t.Fatalf("the revision did not move after a full rescan that observed %s: the rescan counted but did not observe", oob)
	}
	if line.Rev != cell.h.tree.revToken() {
		t.Fatalf("the overflow line carries rev %q but the tree serves %q: the two readers disagree", line.Rev, cell.h.tree.revToken())
	}
	waitFor(t, 5*time.Second, "the state to return to watching only because the rescan is the evidence", func() bool {
		return cell.status().State == WatchStateWatching
	})
	if got := cell.counters().UnvouchedTotal; got != 1 {
		t.Fatalf("unvouched_total = %d after one overflow: the counter counts INTERVALS, not reads", got)
	}
}
