package agent

// INT-CI-044 regression tests: the spawn's bounded uid-CANDIDATE retry around
// the DF-BUNKER-63 collision precheck.
//
// CI run 36198102167 failed TestConcurrency_SpawnFiveAgents on the named-runner
// root suite because the runner carried LEAKED CI residue: spawn
// conctest-0-87636 was assigned uid 1029 — the lowest FREE uid in the passwd
// database — while a leaked residue user (passwd entry long gone, its
// systemd --user manager and a python process still alive) still owned that
// uid. The precheck refused correctly and the spawn then FAILED on the refusal,
// which the test reads as a spawn failure. The same code was 5/5 green in
// adjacent runs, because the trigger is host state the runner mutates between
// runs. The fix retries CANDIDATE SELECTION instead of failing the spawn: the
// just-created user is released and the walk pins the NEXT candidate uid, which
// is re-scanned — so a foreign uid is SKIPPED, never handed out — while the
// refusal itself keeps its exact semantics (fail closed on an unobservable
// scan, never touch a pre-existing agent user, fail loudly when the walk is
// exhausted).
//
// Every test drives the REAL Spawn path: PATH stubs stand in for the host
// commands (useradd/userdel/ssh-keygen), lookupAgentUser and spawnProcessScanner
// are the package seams, and the uid the account carries is read back from the
// useradd argv the spawn actually issued — so no assertion here rests on a
// promise the production code does not make.
//
// AC map:
//   A1 first candidate refused → the spawn proceeds on the NEXT candidate uid
//      (TestSpawnUIDCandidateRetry_FirstCandidateRefusedRetriesNextUID)
//   A2 exhausted walk → the loud DF-BUNKER-63 uid-collision error
//      (TestSpawnUIDCandidateRetry_ExhaustedWalkFailsLoud)
//   plus the branches that must NOT retry and the pure helpers:
//      TestSpawnUIDCandidateRetry_UnobservableScanIsNeverRetried,
//      TestSpawnUIDCandidateRetry_PreexistingUserIsNeverReleased,
//      TestSpawnUIDCandidateRetry_PinnedCandidateAlreadyAnAccountSkipsOn,
//      TestNextUIDCandidate, TestIsUseraddUIDTaken

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── harness ────────────────────────────────────────────────────────────────

// uidWalkHarness is the hermetic spawn harness the walk tests drive: the real
// Spawn path (intci5Manager) with PATH stubs for the host commands, the useradd
// argv log as the ground truth for pinned candidates, and the uid the account
// carries DERIVED from that log (uidFromUseraddCalls) so the account models the
// uid the spawn asked for.
type uidWalkHarness struct {
	m          *AgentManager
	binDir     string
	useraddLog string
	userdelLog string
	keygenLog  string
	scanned    []uint32 // every uid the collision scanner was called with
}

// newUIDWalkHarness installs the stubs and the two seams. scanner is the
// collision precheck's process scanner (the harness records the uids it is
// asked about); systemPick is the uid the host's useradd selects when it is NOT
// pinned.
func newUIDWalkHarness(t *testing.T, systemPick string, scanner func(uid uint32) ([]userProcess, error)) *uidWalkHarness {
	t.Helper()
	redirectBreadcrumbJournal(t)
	h := &uidWalkHarness{
		m:          intci5Manager(t),
		useraddLog: filepath.Join(t.TempDir(), "useradd-calls"),
		userdelLog: filepath.Join(t.TempDir(), "userdel-calls"),
		keygenLog:  filepath.Join(t.TempDir(), "keygen-calls"),
	}
	h.binDir = t.TempDir()
	h.stubUseradd(t, useraddStubRecords(h.useraddLog))
	writeStub(t, h.binDir, "userdel", "printf '%s\\n' \"userdel $*\" >> \""+h.userdelLog+"\"\n")
	writeStub(t, h.binDir, "pkill", stubSucceeds)
	writeStub(t, h.binDir, "pgrep", "exit 1\n")
	// The keygen stub is scripted to FAIL: reaching keygen is the observable
	// proof that the spawn got PAST the collision precheck on an accepted
	// candidate (the same shape TestSpawnUIDCollision_CleanUIDProceeds uses).
	writeStub(t, h.binDir, "ssh-keygen", "printf 'ssh-keygen\\n' >> \""+h.keygenLog+"\"\nexit 1\n")
	t.Setenv("PATH", h.binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	restore := lookupAgentUser
	lookupAgentUser = func(username string) (*user.User, error) {
		uid := uidFromUseraddCalls(t, h.useraddLog, systemPick)
		return &user.User{Username: username, Uid: uid, Gid: uid, HomeDir: "/home/" + username}, nil
	}
	t.Cleanup(func() { lookupAgentUser = restore })

	stubSpawnScanner(t, func(uid uint32) ([]userProcess, error) {
		h.scanned = append(h.scanned, uid)
		return scanner(uid)
	})
	return h
}

// stubUseradd replaces the useradd stub (the default succeeds and records its
// argv). Tests that model a host refusal call it with their own body.
func (h *uidWalkHarness) stubUseradd(t *testing.T, body string) {
	t.Helper()
	writeStub(t, h.binDir, "useradd", body)
}

// spawn runs one spawn through the real path and returns its error.
func (h *uidWalkHarness) spawn(t *testing.T) error {
	t.Helper()
	agentID := uniqueAgentID("intci044")
	_, err := h.m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: agentID, Ttl: "1h"})
	return err
}

func (h *uidWalkHarness) useraddCalls(t *testing.T) []string {
	t.Helper()
	return readRecord(t, h.useraddLog)
}

func (h *uidWalkHarness) userdelCalls(t *testing.T) []string {
	t.Helper()
	return readRecord(t, h.userdelLog)
}

// useraddStubRecords is the plain host model: useradd succeeds and records its
// argv, one line per invocation.
func useraddStubRecords(logPath string) string {
	return "printf '%s\\n' \"useradd $*\" >> \"" + logPath + "\"\n"
}

// collisionProcess is the leaked-residue fingerprint CI run 36198102167 showed
// on the refused uid: a live user manager left behind by a user whose passwd
// entry is gone.
var collisionProcess = userProcess{PID: 4242, Cmd: "/usr/lib/systemd/systemd --user --deserialize 27"}

// uidFromUseraddCalls models the host's own uid assignment: the account useradd
// just created carries the uid the spawn PINNED it to, or the uid the system
// picked when no -u was given. Reading the stub's argv log keeps the test
// honest — the uid the spawn observes is the uid the spawn actually asked for.
func uidFromUseraddCalls(t *testing.T, logPath, systemPick string) string {
	t.Helper()
	lines := readRecord(t, logPath)
	if len(lines) == 0 {
		return systemPick
	}
	fields := strings.Fields(lines[len(lines)-1])
	for i, f := range fields {
		if f == "-u" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return systemPick
}

// ── A1: the first candidate is refused, the NEXT one is accepted ────────────

// TestSpawnUIDCandidateRetry_FirstCandidateRefusedRetriesNextUID is the CI
// 36198102167 fingerprint: the system picks uid 1029 (the lowest free uid) and
// that uid is owned by foreign residue, while every later candidate is clean.
// The spawn must release the refused user, pin 1030, re-scan it, and PROCEED —
// not fail at the uid-collision stage. The keygen stub then fails, which is
// what proves the spawn got past the precheck.
func TestSpawnUIDCandidateRetry_FirstCandidateRefusedRetriesNextUID(t *testing.T) {
	h := newUIDWalkHarness(t, "1029", func(uid uint32) ([]userProcess, error) {
		if uid == 1029 {
			return []userProcess{collisionProcess}, nil
		}
		return nil, nil
	})

	err := h.spawn(t)

	if err == nil {
		t.Fatal("the keygen stub was scripted to fail; the spawn must not report success past it")
	}
	if strings.Contains(err.Error(), StageUIDCollision) {
		t.Errorf("a walk with a free candidate must not fail at the uid-collision stage: %v", err)
	}
	if !strings.Contains(err.Error(), "failed at stage "+StageKeygen) {
		t.Fatalf("the spawn did not proceed past the collision precheck on the retried candidate: %v", err)
	}

	// The walk: two useradd attempts, the retry PINNED to the next candidate.
	useradds := h.useraddCalls(t)
	if len(useradds) != 2 {
		t.Fatalf("want 2 useradd attempts (refused candidate + retry), got %d: %v", len(useradds), useradds)
	}
	if strings.Contains(useradds[0], "-u ") {
		t.Errorf("the first attempt must let useradd pick the uid (no -u): %q", useradds[0])
	}
	if !strings.Contains(useradds[1], "-u 1030") {
		t.Errorf("the retry must pin the candidate after the refused uid: %q", useradds[1])
	}

	// The refused candidate's user was released before the retry — with -rf,
	// because a plain `userdel -r` exits E_USER_BUSY while a foreign process
	// shares the uid (shadow's busy check is uid-based).
	userdels := h.userdelCalls(t)
	if len(userdels) == 0 {
		t.Fatal("the refused candidate's user was never released, so the retry could not have bound another uid")
	}
	if !strings.Contains(userdels[0], "-rf") {
		t.Errorf("the release must use `userdel -rf` (plain -r refuses a busy uid): %q", userdels[0])
	}

	// The safety argument of the whole walk: EVERY candidate is scanned. The
	// first uid was refused, the second accepted — nothing was handed out
	// unscanned.
	want := []uint32{1029, 1030}
	if fmt.Sprint(h.scanned) != fmt.Sprint(want) {
		t.Errorf("scanned uids = %v, want %v (each candidate must be re-scanned)", h.scanned, want)
	}

	// And the retry really did reach keygen instead of stopping at the precheck.
	if calls := readRecord(t, h.keygenLog); len(calls) == 0 {
		t.Errorf("ssh-keygen never ran, so the walk never handed the agent a uid (calls: %v)", calls)
	}
}

// ── A2: the walk is bounded and fails LOUD when it is exhausted ─────────────

// TestSpawnUIDCandidateRetry_ExhaustedWalkFailsLoud drives a uid range that is
// foreign end to end (the residue cluster shape): every candidate is refused.
// The walk must stop after its bounded budget and fail with the DF-BUNKER-63
// uid-collision error — the refusal text stays intact (pids, rationale) — at
// the same named stage, and it must name the candidates it tried.
func TestSpawnUIDCandidateRetry_ExhaustedWalkFailsLoud(t *testing.T) {
	h := newUIDWalkHarness(t, "1029", func(uint32) ([]userProcess, error) {
		return []userProcess{collisionProcess}, nil
	})

	err := h.spawn(t)
	if err == nil {
		t.Fatal("every candidate was refused; the spawn must fail loudly")
	}
	if !strings.Contains(err.Error(), "failed at stage "+StageUIDCollision) {
		t.Errorf("the exhausted walk must fail at the named uid-collision stage: %v", err)
	}
	// The refusal itself is unchanged and still loud.
	for _, want := range []string{
		"uid collision:",
		"pid 4242",
		"/usr/lib/systemd/systemd --user",
		"same-uid signal privilege",
		"DF-BUNKER-63",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("exhausted-walk error lost the DF-BUNKER-63 refusal text %q: %v", want, err)
		}
	}
	// The walk is reported, so an operator can see the candidates were tried.
	if !strings.Contains(err.Error(), "uid-candidate walk exhausted") {
		t.Errorf("the error must name the exhausted candidate walk: %v", err)
	}
	uids := make([]string, 0, 1+uidCollisionCandidateRetries)
	for i := 0; i <= uidCollisionCandidateRetries; i++ {
		uids = append(uids, strconv.Itoa(1029+i))
	}
	if !strings.Contains(err.Error(), "["+strings.Join(uids, " ")+"]") {
		t.Errorf("the error must name the refused candidates [%s]: %v", strings.Join(uids, " "), err)
	}

	// Bounded: 1 system-picked attempt + uidCollisionCandidateRetries retries.
	useradds := h.useraddCalls(t)
	if want := 1 + uidCollisionCandidateRetries; len(useradds) != want {
		t.Errorf("useradd attempts = %d, want %d (1 + the retry budget): %v", len(useradds), want, useradds)
	}
	for i, line := range useradds[1:] {
		// Retry 1 pins the uid after the system-picked 1029, and each later
		// retry pins the uid after the candidate the precheck just refused.
		if want := fmt.Sprintf("-u %d", 1030+i); !strings.Contains(line, want) {
			t.Errorf("retry %d must pin %s: %q", i+1, want, line)
		}
	}

	// Every refused candidate except the LAST was released (the last one is
	// left for the standard rollback, which owns the failure path).
	releases := 0
	for _, c := range h.userdelCalls(t) {
		if strings.Contains(c, "-rf") {
			releases++
		}
	}
	if releases != uidCollisionCandidateRetries {
		t.Errorf("released candidates = %d, want %d (all but the final one): %v", releases, uidCollisionCandidateRetries, h.userdelCalls(t))
	}
	if got := len(h.userdelCalls(t)); got <= releases {
		t.Errorf("the rollback never removed the final refused candidate's user (userdel calls: %v)", h.userdelCalls(t))
	}
}

// ── the branches that must NOT retry ───────────────────────────────────────

// TestSpawnUIDCandidateRetry_UnobservableScanIsNeverRetried pins the fail-closed
// rule across the retry: a scanner that CANNOT look is not a collision, so the
// spawn must refuse immediately — exactly one useradd attempt, or a broken
// scanner would be laundered into a retry loop.
func TestSpawnUIDCandidateRetry_UnobservableScanIsNeverRetried(t *testing.T) {
	h := newUIDWalkHarness(t, "61012", func(uint32) ([]userProcess, error) {
		return nil, fmt.Errorf("permission denied reading /proc")
	})

	err := h.spawn(t)
	if err == nil {
		t.Fatal("an unobservable scan must fail the spawn (fail closed)")
	}
	if !strings.Contains(err.Error(), "failed at stage "+StageUIDCollision) {
		t.Errorf("error does not name the uid-collision stage: %v", err)
	}
	if !strings.Contains(err.Error(), "fail closed") {
		t.Errorf("error does not state the fail-closed rule: %v", err)
	}
	if useradds := h.useraddCalls(t); len(useradds) != 1 {
		t.Errorf("an unobservable scan must not be retried: %d useradd attempts (%v)", len(useradds), useradds)
	}
}

// TestSpawnUIDCandidateRetry_PreexistingUserIsNeverReleased pins the other
// non-retryable branch: on the idempotent re-registration path the account
// PRE-EXISTED, so a collision may not be "fixed" by removing it (its uid is not
// ours to change, and the account may hold real agent state). The spawn must
// refuse at the collision stage and NEVER userdel.
func TestSpawnUIDCandidateRetry_PreexistingUserIsNeverReleased(t *testing.T) {
	h := newUIDWalkHarness(t, "1029", func(uint32) ([]userProcess, error) {
		return []userProcess{collisionProcess}, nil
	})
	// Model a host whose user already exists (bunkerd restart / registry wipe).
	h.stubUseradd(t, "printf '%s\\n' \"useradd $*\" >> \""+h.useraddLog+"\"\n"+
		"echo \"useradd: user 'bunker-x' already exists\" >&2\nexit 9\n")

	err := h.spawn(t)
	if err == nil {
		t.Fatal("a pre-existing agent user on a collided uid must refuse")
	}
	if !strings.Contains(err.Error(), "failed at stage "+StageUIDCollision) {
		t.Errorf("error does not name the uid-collision stage: %v", err)
	}
	if useradds := h.useraddCalls(t); len(useradds) != 1 {
		t.Errorf("a pre-existing user must not be retried on another uid: %d useradd attempts (%v)", len(useradds), useradds)
	}
	if userdels := h.userdelCalls(t); len(userdels) != 0 {
		t.Errorf("a pre-existing agent user was deleted by the walk: %v", userdels)
	}
}

// TestSpawnUIDCandidateRetry_PinnedCandidateAlreadyAnAccountSkipsOn covers the
// walk's second advance cause: the pinned candidate is already an ACCOUNT (a uid
// hole above the lowest free one — the shape the walk meets on a runner where a
// previous spawn has already taken the neighbouring uid). useradd refuses it,
// nothing is created to clean up, and the walk moves to the next candidate.
func TestSpawnUIDCandidateRetry_PinnedCandidateAlreadyAnAccountSkipsOn(t *testing.T) {
	h := newUIDWalkHarness(t, "1029", func(uid uint32) ([]userProcess, error) {
		if uid == 1029 {
			return []userProcess{collisionProcess}, nil
		}
		return nil, nil
	})
	// 1030 is an existing account on this host; everything else is creatable.
	h.stubUseradd(t, useraddStubRecords(h.useraddLog)+
		"case \" $* \" in *\" -u 1030 \"*) echo \"useradd: UID 1030 is not unique\" >&2; exit 4;; esac\n")

	err := h.spawn(t)
	if err == nil {
		t.Fatal("the keygen stub was scripted to fail; the spawn must not report success past it")
	}
	if !strings.Contains(err.Error(), "failed at stage "+StageKeygen) {
		t.Fatalf("the walk must skip an already-used candidate and proceed to keygen: %v", err)
	}

	useradds := h.useraddCalls(t)
	if len(useradds) != 3 {
		t.Fatalf("want 3 useradd attempts (refused, taken, accepted), got %d: %v", len(useradds), useradds)
	}
	if !strings.Contains(useradds[1], "-u 1030") {
		t.Errorf("the second attempt must pin the candidate after the refused uid: %q", useradds[1])
	}
	if !strings.Contains(useradds[2], "-u 1031") {
		t.Errorf("the third attempt must pin the next candidate after the taken uid: %q", useradds[2])
	}
	// Only the collided candidate needed releasing; the taken uid never created
	// an account, so it must not have produced a userdel.
	if releases := strings.Count(strings.Join(h.userdelCalls(t), "\n"), "-rf"); releases != 1 {
		t.Errorf("releases = %d, want 1 (only the refused candidate): %v", releases, h.userdelCalls(t))
	}
}

// ── the pure helpers ───────────────────────────────────────────────────────

// TestNextUIDCandidate pins the walk's step and its boundary: the next uid is
// strictly greater (a refused candidate can never be revisited), and the walk
// reports exhaustion at the top of the uint32 space instead of wrapping onto
// uid values it has already tried.
func TestNextUIDCandidate(t *testing.T) {
	tests := map[string]struct {
		uid      uint32
		want     uint32
		wantMore bool
	}{
		"steps to the next uid":             {uid: 1029, want: 1030, wantMore: true},
		"steps from zero":                   {uid: 0, want: 1, wantMore: true},
		"top of the range has no successor": {uid: math.MaxUint32, want: 0, wantMore: false},
		"one below the top still steps":     {uid: math.MaxUint32 - 1, want: math.MaxUint32, wantMore: true},
		"a refused account uid still steps": {uid: 61011, want: 61012, wantMore: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, more := nextUIDCandidate(tc.uid)
			if got != tc.want || more != tc.wantMore {
				t.Errorf("nextUIDCandidate(%d) = (%d, %v), want (%d, %v)", tc.uid, got, more, tc.want, tc.wantMore)
			}
		})
	}
}

// TestIsUseraddUIDTaken pins the classification of the ONE useradd refusal the
// walk may skip. Everything else must stay a spawn failure: absorbing a real
// useradd error here would hide a broken host behind a candidate walk.
func TestIsUseraddUIDTaken(t *testing.T) {
	tests := map[string]struct {
		out  string
		want bool
	}{
		"util-linux not-unique": {out: "useradd: UID 1030 is not unique", want: true},
		"already exists":        {out: "useradd: user 'bunker-x' already exists", want: false},
		"permission denied":     {out: "useradd: Permission denied", want: false},
		"empty output":          {out: "", want: false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := isUseraddUIDTaken([]byte(tc.out)); got != tc.want {
				t.Errorf("isUseraddUIDTaken(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}
