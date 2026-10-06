package agent

// QA-BUNKER-61 regression: a TRANSIENT userdel failure (busy home, a
// straggler process, EBUSY — the 4fd101f3 shape) used to go straight from
// ONE attempt to the leak path: agent unregistered + port freed, user + home
// leaked with NO registry record, invisible to `bunker list`, never reaped.
//
// Post-fix the non-force, non-absent-output failure reaps the uid's
// stragglers with the bounded TERM→KILL escalation and retries userdel
// EXACTLY ONCE on a fresh budget step; a second failure records a
// userdel_failed refusal BEFORE the teardown folds the records away.
//
// Every test drives Destroy through SEAMS (userdelRunner, the DF-34 probe,
// lookupUser, procDirFixture) — no root, no real host users, no real userdel.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/registry"
)

// stubUserdelRunner swaps the userdel seam for fn and restores it via
// t.Cleanup (tests in this package run in one process).
func stubUserdelRunner(t *testing.T, fn func(ctx context.Context, username string) ([]byte, error)) {
	t.Helper()
	orig := userdelRunner
	userdelRunner = fn
	t.Cleanup(func() { userdelRunner = orig })
}

// qa61RegistryLive seeds the durable registry record a live agent carries —
// AppendRefusal refuses unknown agents (TestAppendRefusalUnknownAgentFails),
// so the refusal-recording tests must start from a spawned record.
func qa61RegistryLive(t *testing.T, m *AgentManager, id string) {
	t.Helper()
	if err := m.registry.AppendSpawn(&registry.Record{AgentID: id, Status: StatusRunning}); err != nil {
		t.Fatalf("seed registry record for %s: %v", id, err)
	}
}

// qa61RefusalEvents returns every refusal event in the registry journal that
// names agentID. The durable EVENT is the assertion surface — the folded
// RefusalOf view is deleted by the destroy append that follows the refusal
// (registry.AppendDestroy), so replaying the journal is the only way to see
// the recorded refusal after the destroy path returned.
func qa61RefusalEvents(t *testing.T, path, agentID string) []map[string]any {
	t.Helper()
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read registry journal: %v", rerr)
	}
	var out []map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, `"kind":"refusal"`) || !strings.Contains(line, agentID) {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("parse registry event line: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

// TestQA61_UserdelTransientFailure_RetriesOnceAndSucceeds is the row's
// acceptance: the injected runner fails ONCE with the busy-home output then
// succeeds. The destroy must make a SECOND userdel call, flow into the
// NORMAL success path (destroyed, no error), and record NO refusal.
// The fresh-budget-step property is proven inside the seam: the retry call
// must run on a LIVE context whose deadline is strictly LATER than the
// first attempt's (a fresh rb.step(), not the cancelled first ctx).
func TestQA61_UserdelTransientFailure_RetriesOnceAndSucceeds(t *testing.T) {
	restore := shrinkRollbackBudgets(t, time.Second, 400*time.Millisecond, 100*time.Millisecond)
	defer restore()

	var buf bytes.Buffer
	m, regPath := df63Manager(t, &buf)
	const id = "qa61-retry-ok"
	username := "bunker-" + id
	liveAgent(t, m, id)
	qa61RegistryLive(t, m, id)

	// The live-process gate passes and the user record resolves: the destroy
	// must reach userdel, not refuse.
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return nil, 61001, true, nil
	})
	presentUserWithUID(t, username, "61001")
	// No home to archive: keeps the archive precondition out of scope.
	pointHomeAt(t, t.TempDir())

	var calls int
	var firstDeadline time.Time
	stubUserdelRunner(t, func(ctx context.Context, uname string) ([]byte, error) {
		calls++
		dl, _ := ctx.Deadline()
		if ctx.Err() != nil {
			t.Errorf("userdel call %d ran on a dead context (err: %v)", calls, ctx.Err())
		}
		if calls == 1 {
			firstDeadline = dl
			// The real 4fd101f3 output class. Deliberately free of the
			// absent-output markers ("no such user" etc.) — those classify
			// as user-already-gone and take the historical no-retry path.
			return []byte("userdel: user " + uname + " is currently used by process 1234\n"),
				errors.New("exit status 1")
		}
		// THE premise of the retry: attempt #2 runs on a FRESH budget step —
		// a live context with a strictly later deadline than attempt #1's.
		if !dl.After(firstDeadline) {
			t.Errorf("retry ran on the first attempt's context: deadline %v not after %v", dl, firstDeadline)
		}
		if uname != username {
			t.Errorf("retry targeted %q, want %q", uname, username)
		}
		return nil, nil
	})

	resp, derr := m.Destroy(context.Background(), id, false)
	if derr != nil {
		t.Fatalf("destroy with a once-failing userdel must recover through the retry: %v", derr)
	}
	if resp == nil || resp.Status != "destroyed" {
		t.Fatalf("status = %v, want destroyed", resp)
	}
	if calls != 2 {
		t.Fatalf("userdel calls = %d, want exactly 2 (one transient failure + one retry)", calls)
	}
	// The normal success path ran: the tracker record is torn down.
	if rec := m.tracker.Get(id); rec != nil {
		t.Error("tracker record survived a completed destroy")
	}
	// NO refusal for a recovered destroy — not in the folded view, and not
	// in the durable journal.
	if r := m.registry.RefusalOf(id); r != nil {
		t.Errorf("a retry-success must record no refusal, got %+v", r)
	}
	if evs := qa61RefusalEvents(t, regPath, id); len(evs) != 0 {
		t.Errorf("retry-success wrote refusal events to the journal: %v", evs)
	}
	// The retry is narrated: the reap-or-skip line AND the success line.
	for _, want := range []string{"userdel failed once", "userdel retry succeeded"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("journal missing %q — got:\n%s", want, buf.String())
		}
	}
}

// TestQA61_UserdelFailsTwice_RefusalRecordedHardError pins the give-up path:
// both attempts fail, the destroy returns the hard userdel_failed error
// carrying the surviving-state evidence, and the refusal is recorded on the
// durable record (the folded view AND the raw journal event) BEFORE the
// teardown unregisters the agent — so the leak is visible and the reaper
// backs off instead of the agent silently vanishing.
func TestQA61_UserdelFailsTwice_RefusalRecordedHardError(t *testing.T) {
	restore := shrinkRollbackBudgets(t, time.Second, 400*time.Millisecond, 100*time.Millisecond)
	defer restore()

	var buf bytes.Buffer
	m, regPath := df63Manager(t, &buf)
	const id = "qa61-two-fails"
	username := "bunker-" + id
	liveAgent(t, m, id)
	qa61RegistryLive(t, m, id)

	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return nil, 61001, true, nil
	})
	presentUserWithUID(t, username, "61001")
	pointHomeAt(t, t.TempDir())

	var calls int
	stubUserdelRunner(t, func(_ context.Context, uname string) ([]byte, error) {
		calls++
		return []byte("userdel: user " + uname + " is currently used by process 1234\n"),
			errors.New("exit status 1")
	})

	resp, derr := m.Destroy(context.Background(), id, false)
	if derr == nil {
		t.Fatal("a twice-failing userdel must be a hard error, not a success")
	}
	if resp == nil || resp.Status != StatusUserdelFailed {
		t.Fatalf("status = %v, want %q", resp, StatusUserdelFailed)
	}
	if calls != 2 {
		t.Fatalf("userdel calls = %d, want exactly 2 (first attempt + the single retry)", calls)
	}
	// The error keeps the DF-34 evidence contract: destroy name, userdel
	// error, and the surviving-state evidence line.
	for _, want := range []string{"destroy of " + id + " failed", "userdel error", "surviving state:"} {
		if !strings.Contains(derr.Error(), want) {
			t.Errorf("hard error missing %q — got: %v", want, derr)
		}
	}
	// The refusal is on the durable record: the folded view is ALREADY
	// deleted here (the persistDestroy append below the refusal folds it —
	// pinned by TestQA61_UserdelFailed_RefusalVocabularyDurable), so the
	// raw journal EVENT is the assertion surface, carrying the row's status
	// and the evidence in last_error.
	if r := m.registry.RefusalOf(id); r != nil {
		t.Errorf("folded refusal must be folded away by the destroy append, got %+v", r)
	}
	evs := qa61RefusalEvents(t, regPath, id)
	if len(evs) != 1 {
		t.Fatalf("refusal events in journal = %d, want 1: %v", len(evs), evs)
	}
	refusal, _ := evs[0]["refusal"].(map[string]any)
	if got, _ := refusal["status"].(string); got != StatusUserdelFailed {
		t.Errorf("journal refusal status = %v, want %q", got, StatusUserdelFailed)
	}
	if le, _ := refusal["last_error"].(string); !strings.Contains(le, "surviving state:") {
		t.Errorf("journal refusal last_error missing the evidence: %q", le)
	}
	// The agent is unregistered (the historical teardown still runs).
	if rec := m.tracker.Get(id); rec != nil {
		t.Error("tracker record survived the userdel_failed return")
	}
	if !strings.Contains(buf.String(), "destroy refused; recording refusal for reaper backoff") {
		t.Errorf("journal missing the refusal-recording line — got:\n%s", buf.String())
	}
}

// TestQA61_AbsentUser_NoRetry pins the historical user-already-gone path:
// absent-output failure, NO retry (exactly one userdel call), and the
// misleading "treating agent as not found" warn now carries the surviving-
// state evidence so a leak in this class is diagnosable from the journal.
func TestQA61_AbsentUser_NoRetry(t *testing.T) {
	var buf bytes.Buffer
	m, _ := df63Manager(t, &buf)
	const id = "qa61-absent"
	// Tracker-only registration (no registry spawn record): the
	// never-seen-ID arm of the absent path reports not_found. The user
	// record deliberately does NOT resolve — userPresent=false drives the
	// absent branch regardless of the username spelling.
	liveAgent(t, m, id)

	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return nil, 61001, true, nil
	})
	// The user does NOT resolve: userPresent=false drives the absent path.
	pointHomeAt(t, t.TempDir())

	var calls int
	stubUserdelRunner(t, func(_ context.Context, uname string) ([]byte, error) {
		calls++
		return []byte("userdel: user " + uname + " does not exist\n"), errors.New("exit status 1")
	})

	resp, derr := m.Destroy(context.Background(), id, false)
	if derr == nil || !strings.Contains(derr.Error(), "not found") {
		t.Fatalf("err = %v, want the tidy not-found error", derr)
	}
	if resp == nil || resp.Status != "not_found" {
		t.Fatalf("status = %v, want not_found", resp)
	}
	if calls != 1 {
		t.Errorf("userdel calls = %d, want exactly 1 (the absent path must NOT retry)", calls)
	}
	warn := "userdel failed, treating agent as not found"
	if !strings.Contains(buf.String(), warn) {
		t.Errorf("journal missing %q — got:\n%s", warn, buf.String())
	}
	if !strings.Contains(buf.String(), "surviving state:") {
		t.Errorf("the not-found warn must carry the surviving-state evidence — got:\n%s", buf.String())
	}
}

// TestQA61_ForceMode_NoRetry pins force mode: the loud continue-on-failure
// log, NO retry (exactly one userdel call), and the destroy completes.
func TestQA61_ForceMode_NoRetry(t *testing.T) {
	var buf bytes.Buffer
	m, _ := df63Manager(t, &buf)
	const id = "qa61-force"
	username := "bunker-" + id
	liveAgent(t, m, id)
	qa61RegistryLive(t, m, id)

	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return nil, 61001, true, nil
	})
	presentUserWithUID(t, username, "61001")
	pointHomeAt(t, t.TempDir())

	var calls int
	stubUserdelRunner(t, func(_ context.Context, uname string) ([]byte, error) {
		calls++
		return []byte("userdel: user " + uname + " is currently used by process 1234\n"),
			errors.New("exit status 1")
	})

	resp, derr := m.Destroy(context.Background(), id, true)
	if derr != nil {
		t.Fatalf("force-mode destroy must keep its continue-on-failure semantics: %v", derr)
	}
	if resp == nil || resp.Status != "destroyed" {
		t.Fatalf("status = %v, want destroyed", resp)
	}
	if calls != 1 {
		t.Errorf("userdel calls = %d, want exactly 1 (force mode must NOT retry)", calls)
	}
	if !strings.Contains(buf.String(), "userdel failed in force mode; agent state is partially removed") {
		t.Errorf("journal missing the loud force-mode failure line — got:\n%s", buf.String())
	}
}

// TestQA61_ReapBeforeRetry drives the retry helper directly: with live
// stragglers under the uid the bounded TERM→KILL escalation runs BEFORE the
// retry (kill commands on the recorder); with a stale marker and an empty
// re-scan no kill pass runs at all — but the retry happens either way.
func TestQA61_ReapBeforeRetry(t *testing.T) {
	restoreBudgets := shrinkRollbackBudgets(t, time.Second, 400*time.Millisecond, 100*time.Millisecond)
	defer restoreBudgets()

	origGrace := forceKillGrace
	forceKillGrace = 10 * time.Millisecond
	t.Cleanup(func() { forceKillGrace = origGrace })

	for _, tc := range []struct {
		name      string
		straggler bool
	}{
		{name: "stragglers_reaped_before_retry", straggler: true},
		{name: "stale_marker_no_kill_pass", straggler: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			m, _ := df63Manager(t, &buf)
			const id = "qa61-reap"
			username := "bunker-" + id
			presentUserWithUID(t, username, "61003")
			if tc.straggler {
				procDirFixture(t, []procFixtureEntry{
					{PID: "4242", UID: 61003, Name: "schedulerd", Cmd: "/home/bunker-qa61-reap/bin/schedulerd"},
				})
			}

			// Recorder for the kill commands: the escalation must signal the
			// straggler's pid in the straggler case and NOTHING in the
			// stale-marker case.
			killLog := filepath.Join(t.TempDir(), "kill.log")
			origRunner := gateForceKillRunner
			gateForceKillRunner = func(_ context.Context, name string, args ...string) ([]byte, error) {
				f, err := os.OpenFile(killLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err != nil {
					return nil, err
				}
				defer func() { _ = f.Close() }()
				_, _ = fmt.Fprintf(f, "%s %s\n", name, strings.Join(args, " "))
				return nil, nil
			}
			t.Cleanup(func() { gateForceKillRunner = origRunner })

			var calls int
			stubUserdelRunner(t, func(_ context.Context, _ string) ([]byte, error) {
				calls++
				return nil, nil // the retry succeeds
			})

			rb := newRollbackBudget(context.Background())
			rerr := m.retryUserdelOnceAfterReap(rb, id, username,
				[]byte("userdel: user "+username+" is currently used by process 999\n"),
				errors.New("exit status 1"))
			if rerr != nil {
				t.Fatalf("retry after reap must succeed: %v", rerr)
			}
			if calls != 1 {
				t.Errorf("userdel retries = %d, want exactly 1", calls)
			}
			kills, kerr := os.ReadFile(killLog)
			if tc.straggler {
				if kerr != nil || !strings.Contains(string(kills), "kill -TERM 4242") || !strings.Contains(string(kills), "kill -KILL 4242") {
					t.Errorf("straggler case: kill recorder = %q (err %v), want TERM then KILL of pid 4242", kills, kerr)
				}
			} else if kerr == nil && len(kills) > 0 {
				t.Errorf("stale-marker case must not run a kill pass, got %q", kills)
			}
		})
	}
}

// TestQA61_UserdelFailed_RefusalVocabularyDurable pins the store-level piece
// of the fix: StatusUserdelFailed is now a refusal the recorder accepts (the
// reaper's backoff covers the leaked user + home), the folded view follows
// the historical AppendDestroy contract, and the raw journal event survives
// for replay.
func TestQA61_UserdelFailed_RefusalVocabularyDurable(t *testing.T) {
	_, regPath := df63Manager(t, nil)
	store, err := registry.Open(registry.Options{Path: regPath})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()
	const id = "qa61-vocab"
	if err := store.AppendSpawn(&registry.Record{AgentID: id, Status: StatusRunning}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !isDestroyRefusalStatus(StatusUserdelFailed) {
		t.Fatal("StatusUserdelFailed must be a refusal status (reaper backoff covers the leak)")
	}
	cause := fmt.Errorf("destroy of %s failed: userdel error: exit status 1. surviving state: ...", id)
	if err := store.AppendRefusal(id, &registry.Refusal{Status: StatusUserdelFailed, Attempts: 1, LastError: cause.Error()}); err != nil {
		t.Fatalf("AppendRefusal(userdel_failed) must be accepted: %v", err)
	}
	if r := store.RefusalOf(id); r == nil || r.Status != StatusUserdelFailed {
		t.Fatalf("folded refusal = %+v, want status %q", r, StatusUserdelFailed)
	}
	// The destroy append folds the view away (historical contract) but the
	// EVENT stays in the journal.
	if err := store.AppendDestroy(id); err != nil {
		t.Fatalf("AppendDestroy: %v", err)
	}
	if r := store.RefusalOf(id); r != nil {
		t.Errorf("folded refusal must be deleted by the destroy append, got %+v", r)
	}
	if evs := qa61RefusalEvents(t, regPath, id); len(evs) != 1 {
		t.Errorf("journal refusal events = %d, want 1 (the event outlives the folded view)", len(evs))
	}
}
