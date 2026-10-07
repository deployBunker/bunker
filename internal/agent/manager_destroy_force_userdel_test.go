package agent

// REV-BUNKER-003 regression tests: FORCE-mode destroy must fail VISIBLE when
// userdel fails. Pre-fix, the force branch of the userdel-failure handler
// logged the failure at Error and FELL THROUGH to the normal success path —
// Step 4 cleanup, tracker teardown, response status "destroyed", err nil —
// so a forced destroy (the operator's --force, and ALWAYS the reconcile
// orphan walk via destroyOrphan) reported success while the bunker-<id> user
// + home survived on the host.
//
// Post-fix force runs the same bounded recovery the non-force path has
// (retryUserdelOnceAfterReap); a retry SUCCESS flows into the normal success
// path (status "destroyed", no refusal), and a retry FAILURE fails visible:
// refusal on the durable record, port freed, tracker unregistered, hard
// StatusUserdelFailed error carrying the surviving-state evidence.
//
// Every test drives Destroy through SEAMS (userdelRunner, the DF-34 probe,
// lookupUser) — no root, no real host users, no real userdel. The reconcile
// counterpart (destroyOrphan error must count as Failed, never Destroyed)
// lives at the bottom of this file.

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rev003ForceManager is the hermetic harness for the force-mode rows: the
// df63Manager shape (temp durable registry at the returned path + debug
// logger into buf). The port range for id gets allocated by liveAgent inside
// rev003Setup, so "port freed" is an observable state transition, not a
// no-op on an unallocated id.
func rev003ForceManager(t *testing.T, buf *bytes.Buffer, id string) (*AgentManager, string) {
	t.Helper()
	return df63Manager(t, buf)
}

// rev003Setup stages the host state the destroy path walks before userdel:
// a live tracker+registry record, a resolvable user (uid 61001), no live
// processes (the DF-34 gate passes), and no home to archive (the archive
// precondition stays out of scope).
func rev003Setup(t *testing.T, m *AgentManager, id, username string) {
	t.Helper()
	liveAgent(t, m, id)
	qa61RegistryLive(t, m, id)
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return nil, 61001, true, nil
	})
	presentUserWithUID(t, username, "61001")
	// No home to archive: keeps the archive precondition out of scope.
	pointHomeAt(t, t.TempDir())
}

// TestREV_BUNKER_003_Force_UserdelFailsTwice_FailsVisible is acceptance
// row 3a: force=true, the first userdel attempt fails AND the bounded retry
// fails. The destroy must NOT fall through to success: the response carries
// StatusUserdelFailed, the error carries the surviving-state evidence, the
// refusal is recorded on the durable record, the port range is freed, the
// tracker record is torn down, and exactly TWO userdel calls ran (first
// attempt + the single bounded retry).
func TestREV_BUNKER_003_Force_UserdelFailsTwice_FailsVisible(t *testing.T) {
	restore := shrinkRollbackBudgets(t, time.Second, 400*time.Millisecond, 100*time.Millisecond)
	defer restore()

	var buf bytes.Buffer
	const id = "rev003-force-fail"
	username := "bunker-" + id
	m, regPath := rev003ForceManager(t, &buf, id)
	rev003Setup(t, m, id, username)

	var calls int
	stubUserdelRunner(t, func(_ context.Context, uname string) ([]byte, error) {
		calls++
		// The busy-home failure class, deliberately free of the
		// absent-output markers (those classify as user-already-gone and
		// take the historical no-recovery path).
		return []byte("userdel: user " + uname + " is currently used by process 1234\n"),
			errors.New("exit status 1")
	})

	resp, derr := m.Destroy(context.Background(), id, true)
	if derr == nil {
		t.Fatal("force destroy with a twice-failing userdel must be a hard error, not a silent success")
	}
	if resp == nil || resp.Status != StatusUserdelFailed {
		t.Fatalf("status = %v, want %q", resp, StatusUserdelFailed)
	}
	if calls != 2 {
		t.Errorf("userdel calls = %d, want exactly 2 (force attempt + the single bounded retry)", calls)
	}
	// The error keeps the DF-34 evidence contract: destroy name, userdel
	// error, and the surviving-state evidence line.
	for _, want := range []string{"destroy of " + id + " failed", "userdel error", "surviving state:"} {
		if !strings.Contains(derr.Error(), want) {
			t.Errorf("hard error missing %q — got: %v", want, derr)
		}
	}
	// The refusal EVENT survives in the durable journal (the destroy append
	// folds the folded view away — the journal is the assertion surface,
	// the same contract TestQA61_UserdelFailsTwice_RefusalRecordedHardError
	// pins for the non-force path).
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
	// The teardown ran: port range freed (pre-reserved by the fixture, so
	// this is a real transition), tracker record gone.
	if m.portAlloc.Has(id) {
		t.Error("force-mode userdel failure leaked the port range")
	}
	if rec := m.tracker.Get(id); rec != nil {
		t.Error("tracker record survived the force-mode userdel_failed return")
	}
	// The force give-up is narrated with the failure evidence.
	for _, want := range []string{
		"userdel failed in force mode; retry failed; destroy refused",
		"destroy refused; recording refusal for reaper backoff",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("journal missing %q — got:\n%s", want, buf.String())
		}
	}
	// The old fall-through is gone: a force failure must never read as a
	// completed destroy.
	if strings.Contains(buf.String(), "agent destroyed") {
		t.Errorf("the success-path 'agent destroyed' line must not appear on a refused force destroy — got:\n%s", buf.String())
	}
}

// TestREV_BUNKER_003_Force_UserdelRetrySucceeds_Destroyed is acceptance
// row 3b: force=true, the first attempt fails, the bounded retry succeeds.
// The destroy must flow into the NORMAL success path — status "destroyed",
// err nil — and record NO refusal for the recovered destroy.
func TestREV_BUNKER_003_Force_UserdelRetrySucceeds_Destroyed(t *testing.T) {
	restore := shrinkRollbackBudgets(t, time.Second, 400*time.Millisecond, 100*time.Millisecond)
	defer restore()

	var buf bytes.Buffer
	const id = "rev003-force-retry-ok"
	username := "bunker-" + id
	m, regPath := rev003ForceManager(t, &buf, id)
	rev003Setup(t, m, id, username)

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
			return []byte("userdel: user " + uname + " is currently used by process 1234\n"),
				errors.New("exit status 1")
		}
		// The retry must run on a FRESH budget step — a live context with a
		// strictly later deadline than attempt #1's (the QA-61 property).
		if !dl.After(firstDeadline) {
			t.Errorf("retry ran on the first attempt's context: deadline %v not after %v", dl, firstDeadline)
		}
		if uname != username {
			t.Errorf("retry targeted %q, want %q", uname, username)
		}
		return nil, nil
	})

	resp, derr := m.Destroy(context.Background(), id, true)
	if derr != nil {
		t.Fatalf("force destroy with a once-failing userdel must recover through the retry: %v", derr)
	}
	if resp == nil || resp.Status != "destroyed" {
		t.Fatalf("status = %v, want destroyed", resp)
	}
	if calls != 2 {
		t.Errorf("userdel calls = %d, want exactly 2 (force attempt + one retry)", calls)
	}
	if rec := m.tracker.Get(id); rec != nil {
		t.Error("tracker record survived a completed destroy")
	}
	if m.portAlloc.Has(id) {
		t.Error("a recovered force destroy leaked the port range")
	}
	// NO refusal for a recovered destroy — not in the folded view, and not
	// in the durable journal.
	if r := m.registry.RefusalOf(id); r != nil {
		t.Errorf("a retry-success must record no refusal, got %+v", r)
	}
	if evs := qa61RefusalEvents(t, regPath, id); len(evs) != 0 {
		t.Errorf("retry-success wrote refusal events to the journal: %v", evs)
	}
	if !strings.Contains(buf.String(), "userdel retry succeeded; continuing destroy on the normal success path") {
		t.Errorf("journal missing the retry-success line — got:\n%s", buf.String())
	}
}

// TestREV_BUNKER_003_Reconcile_DestroyOrphanError_CountsFailedNotDestroyed
// is acceptance row 3d: an orphan whose forced destroy returns an ERROR must
// be counted under the report's Failed counter and NEVER under Destroyed
// (pre-fix the error path logged and continued, leaving the orphan counted
// under neither — a non-zero Destroyed was ambiguous about full coverage).
// This is the synchronous orphan walk; ReconcileStartup reaches the same
// closure, so both entry points get the counting by construction.
func TestREV_BUNKER_003_Reconcile_DestroyOrphanError_CountsFailedNotDestroyed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	if m.portAlloc == nil {
		t.Fatal("test requires a configured port allocator")
	}
	defer func() { _ = m.registry.Close() }()

	rec := &destroyRecorder{err: errors.New("userdel forced failure (REV-BUNKER-003 fixture)")}
	m.destroyAgent = rec.seam()

	// OURS by classification: in-pool port metadata + this daemon's owner
	// marker, so the walk reaches the destroy branch instead of skipping
	// the orphan as foreign/unproven.
	home := t.TempDir()
	writePortMetadata(t, home, "12300-12399\n")
	writeOwnerMarker(t, home, m.instanceID)
	m.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: "orphan", Username: "bunker-orphan", Home: home}}, nil
	}

	rep := m.Reconcile(context.Background())
	if rep.Destroyed != 0 {
		t.Errorf("Destroyed = %d, want 0 — an errored destroy must never be counted as destroyed (report %+v)", rep.Destroyed, rep)
	}
	if rep.Failed != 1 {
		t.Errorf("Failed = %d, want 1 — the errored destroy is unreconciled residue (report %+v)", rep.Failed, rep)
	}
	if rep.Adopted != 0 {
		t.Errorf("Adopted = %d, want 0 (report %+v)", rep.Adopted, rep)
	}
	if got := rec.calls(); len(got) != 1 || got[0] != "orphan" {
		t.Errorf("destroy calls = %v, want exactly one for the orphan", got)
	}
}

// TestREV_BUNKER_003_Reconcile_DestroySuccess_StillCountsDestroyed is the
// control for the counter split: a destroy that returns NO error keeps
// counting under Destroyed, and Failed stays zero — the new counter must
// not misclassify healthy destroys.
func TestREV_BUNKER_003_Reconcile_DestroySuccess_StillCountsDestroyed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	if m.portAlloc == nil {
		t.Fatal("test requires a configured port allocator")
	}
	defer func() { _ = m.registry.Close() }()

	rec := &destroyRecorder{}
	m.destroyAgent = rec.seam()

	home := t.TempDir()
	writePortMetadata(t, home, "12300-12399\n")
	writeOwnerMarker(t, home, m.instanceID)
	m.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: "orphan", Username: "bunker-orphan", Home: home}}, nil
	}

	rep := m.Reconcile(context.Background())
	if rep.Destroyed != 1 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want Destroyed=1 / Failed=0", rep)
	}
}
