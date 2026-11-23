package agent

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// perf024ResidueUser is the stubbed agent user the degraded-leg tests drive
// (uid 61001, the shared user-stub uid of this package's hermetic hosts).
func perf024ResidueUser() *user.User {
	return &user.User{Username: "bunker-p24res", Uid: "61001", Gid: "61001", HomeDir: "/home/bunker-p24res"}
}

// ── PERF-024 AC3 ────────────────────────────────────────────────────────────
//
// The board criterion: "Rollback removes the user-<uid>.slice.d drop-ins it
// wrote (test: a spawn that fails / rolls back leaves no
// /etc/systemd/system/user-*.slice.d residue; assert before and after)".
//
// Live evidence this regression is pinned to (bunker-mvp, 2026-11-22/23): a
// scratch-daemon standard-preset spawn whose containment landing check did not
// converge rolled back EVERYTHING EXCEPT the drop-in it had already written —
// /etc/systemd/system/user-1733.slice.d survived with its full standard-preset
// conf, and a second spawn stranded an EMPTY user-1732.slice.d (the mkdir had
// succeeded, the conf write had not). The mechanism: the spawn site armed the
// rollback's slice cleanup (createdUserSlice) only on a FULLY converged gate,
// so a landing-check failure — and a degradable reload failure whose spawn
// later failed at another stage — skipped the cleanup step while the user and
// the rest of the host state were removed.
//
// The fix arms the cleanup from the FILE FACT (a non-empty drop-in content
// return) and makes the degradable legs preserve that fact. This test drives
// the REAL spawn + rollback hermetically: the drop-in write succeeds (fact
// captured at the daemon-reload seam), the landing cgroup fixture never
// converges (memory.swap.max pinned at "max"), the gate exhausts its bounded
// budget, the spawn fails at slice-limits — and the residue must be gone
// afterwards, with the cleanup named in the failure breadcrumb.

const perf024ResidueAgentID = "p24res"

func TestSpawnLandingCheckFailureRollsBackSliceDropInResidue(t *testing.T) {
	h := newRecycledResidueHost(t, perf024ResidueAgentID)
	journal := redirectBreadcrumbJournal(t)

	uidArg := strconv.Itoa(h.uid)
	dropinDir := userSliceDropinDir(uidArg)
	confPath := filepath.Join(dropinDir, "50-bunker.conf")

	// ── BEFORE: the fixture starts with zero slice residue for this uid ──
	if _, err := os.Stat(dropinDir); !os.IsNotExist(err) {
		t.Fatalf("test premise broken: %s already exists before the spawn (err %v)", dropinDir, err)
	}

	// The rootless install must SUCCEED so the spawn reaches slice-limits
	// (the recycled-residue fixture's default installer stub fails earlier).
	// The install path stats <agentHome>/bin/dockerd-rootless.sh after the
	// runner returns, so the stub drops that binary at the swapped
	// agentHomeRoot the spawn itself resolves.
	prevInstaller := rootlessInstallerRunner
	rootlessInstallerRunner = func(context.Context, string, string, string) ([]byte, error) {
		bin := filepath.Join(agentHomeRoot, "bunker-"+perf024ResidueAgentID, "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(bin, "dockerd-rootless.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			return nil, err
		}
		return []byte("ok\n"), nil
	}
	t.Cleanup(func() { rootlessInstallerRunner = prevInstaller })

	// dockerd readiness: the checker seam reports the process up and drops
	// the socket into the (temp) spawnRunRoot the spawn itself created.
	sockPath := filepath.Join(spawnRunRoot, perf024ResidueAgentID, "docker.sock")
	prevChecker := dockerdProcessChecker
	dockerdProcessChecker = func(context.Context, string) (bool, error) {
		if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
			return false, err
		}
		if err := os.WriteFile(sockPath, nil, 0o600); err != nil {
			return false, err
		}
		return true, nil
	}
	t.Cleanup(func() { dockerdProcessChecker = prevChecker })

	// dockerd-start resolves the agent user through the seam (the stubbed
	// passwd file is not the real one).
	prevDockerdLookup := dockerdStartUserLookup
	dockerdStartUserLookup = func(name string) (*user.User, error) {
		if name != h.user {
			return nil, user.UnknownUserError(name)
		}
		return &user.User{Username: name, Uid: uidArg, Gid: uidArg, HomeDir: filepath.Join(agentHomeRoot, name)}, nil
	}
	t.Cleanup(func() { dockerdStartUserLookup = prevDockerdLookup })

	// systemd-run is a plain exec: stub it so the transient "unit" starts.
	binDir := t.TempDir()
	writeStub(t, binDir, "systemd-run", stubSucceeds)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The container-cap probe is a var seam; answer 0 containers.
	prevCount := countAgentContainers
	countAgentContainers = func(context.Context, string) (uint32, error) { return 0, nil }
	t.Cleanup(func() { countAgentContainers = prevCount })

	// slice-limits: the fake daemon-reload records the reload AND whether the
	// conf file existed at that moment — the mid-flight "the drop-in was
	// written" fact the criterion's before/after check needs.
	prevCtl := sliceApplySystemctl
	reloads := 0
	confExistedAtReload := false
	sliceApplySystemctl = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 0 && args[0] == "daemon-reload" {
			reloads++
			if _, err := os.Stat(confPath); err == nil {
				confExistedAtReload = true
			}
		}
		return []byte(""), nil
	}
	t.Cleanup(func() { sliceApplySystemctl = prevCtl })

	// The landing cgroup: pinned at the untouched-slice value forever, so the
	// bounded convergence exhausts exactly the way the scratch-daemon run did.
	cgroupRoot := t.TempDir()
	sliceDir := filepath.Join(cgroupRoot, "user.slice", "user-"+uidArg+".slice")
	if err := os.MkdirAll(sliceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sliceDir, "memory.swap.max"), []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prevRoot := cgroupV2Root
	cgroupV2Root = cgroupRoot
	t.Cleanup(func() { cgroupV2Root = prevRoot })

	// Shrink the convergence bound: the failure shape (never converges) is
	// what matters, not the wall time.
	prevAttempts, prevPoll := containmentConvergeAttempts, containmentConvergePoll
	containmentConvergeAttempts, containmentConvergePoll = 2, time.Millisecond
	t.Cleanup(func() { containmentConvergeAttempts, containmentConvergePoll = prevAttempts, prevPoll })

	// ── the spawn: must FAIL at slice-limits with the exhaustion pair ──
	resp, err := h.m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: h.agentID, Ttl: "1h"})
	if err == nil {
		t.Fatalf("Spawn() succeeded although the landing fixture never converges: %+v", resp)
	}
	if !strings.Contains(err.Error(), "failed at stage "+StageSliceLimits) {
		t.Errorf("spawn error does not name the slice-limits stage: %v", err)
	}
	if !strings.Contains(err.Error(), "did not converge within") ||
		!strings.Contains(err.Error(), `memory.swap.max = "max"`) {
		t.Errorf("spawn error is not the landing-exhaustion fingerprint: %v", err)
	}

	// ── the drop-in WAS written (the fact the old code failed to act on) ──
	if reloads == 0 {
		t.Fatalf("the daemon-reload never ran; the spawn did not reach the slice-limits apply (calls logged under the seam)")
	}
	if !confExistedAtReload {
		t.Fatalf("test premise broken: %s did not exist when the daemon-reload ran — the drop-in write never landed", confPath)
	}

	// ── AFTER: zero residue on the slice plane, like every other plane ──
	if _, statErr := os.Stat(dropinDir); !os.IsNotExist(statErr) {
		t.Errorf("PERF-024 residue: %s survived the rollback of a landing-check failure (stat error: %v)", dropinDir, statErr)
	}
	if passwdHasUser(t, h.passwdPath, h.user) {
		t.Errorf("agent user %s survived the rollback (still in %s)", h.user, h.passwdPath)
	}

	// ── the breadcrumb names the cleanup step so the residue claim is auditable ──
	bc := readSingleBreadcrumb(t, journal)
	if bc["stage"] != StageSliceLimits {
		t.Errorf("breadcrumb stage = %v, want %q", bc["stage"], StageSliceLimits)
	}
	if ran := breadcrumbList(t, bc, "rollback_ran"); !containsPrefix(ran, "slice-limits removed") {
		t.Errorf("rollback_ran does not carry the slice cleanup: %v", ran)
	}
}

// TestApplyUserSliceLimitsDegradableLegsPreserveTheFileFact pins the two
// degradable-leg contracts the spawn-site arming relies on:
//
//  1. a failed daemon-reload AFTER a successful write returns the CONTENT
//     (non-empty) with the errSliceApplyWrite sentinel — the pre-fix ""
//     return silently disarmed the rollback's slice cleanup for a conf that
//     was already on disk;
//  2. a failed conf WRITE (the mkdir succeeded, the file did not land)
//     removes the EMPTY drop-in directory it just created — the measured
//     empty /etc/systemd/system/user-1732.slice.d residue.
func TestApplyUserSliceLimitsDegradableLegsPreserveTheFileFact(t *testing.T) {
	uid := "61001"
	dropinDir := filepath.Join(t.TempDir(), "user-"+uid+".slice.d")
	prevDir := systemdUnitDirRoot
	systemdUnitDirRoot = filepath.Dir(dropinDir)
	t.Cleanup(func() { systemdUnitDirRoot = prevDir })

	// The rest of the landing check reads nothing here: the gate fails at the
	// daemon-reload seam before any cgroup read matters.
	confPath := filepath.Join(dropinDir, "50-bunker.conf")
	fake := &intci37FakeCtl{denyNext: 1}
	prevCtl := sliceApplySystemctl
	sliceApplySystemctl = fake.run
	t.Cleanup(func() { sliceApplySystemctl = prevCtl })

	content, err := applyUserSliceLimits(context.Background(), perf024ResidueUser(), 2.0, 4<<30, 0, 0, 0, nil, testLogger())
	if err == nil {
		t.Fatal("the denied daemon-reload must fail the degradable leg")
	}
	if !strings.Contains(err.Error(), "slice drop-in write failed") {
		t.Errorf("the reload failure must carry the errSliceApplyWrite sentinel: %v", err)
	}
	if content == "" {
		t.Fatal("PERF-024: a reload failure after a successful write must still return the content — an empty return disarms the rollback's slice cleanup for a conf that is on disk")
	}
	if _, statErr := os.Stat(confPath); statErr != nil {
		t.Errorf("the written conf must be on disk after a degradable reload failure: %v", statErr)
	}

	// Leg 2: the mkdir succeeds (the directory already exists, so MkdirAll is
	// a no-op) but the conf WRITE fails (the directory is not writable) — the
	// measured residue shape is an EMPTY drop-in directory, and the call must
	// remove it again.
	writeFailDir := filepath.Join(t.TempDir(), "user-"+uid+".slice.d")
	prevDir2 := systemdUnitDirRoot
	systemdUnitDirRoot = filepath.Dir(writeFailDir)
	t.Cleanup(func() { systemdUnitDirRoot = prevDir2 })
	if err := os.MkdirAll(writeFailDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writeFailDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(writeFailDir, 0o755) }) // let the temp GC reap it
	_, err = applyUserSliceLimits(context.Background(), perf024ResidueUser(), 2.0, 4<<30, 0, 0, 0, nil, testLogger())
	if err == nil || !strings.Contains(err.Error(), "slice drop-in write failed") {
		t.Errorf("a conf write into an unwritable drop-in dir must fail with the sentinel: %v", err)
	}
	if _, statErr := os.Stat(writeFailDir); !os.IsNotExist(statErr) {
		t.Errorf("PERF-024 residue: the empty drop-in directory survived its own failed write: %v", statErr)
	}
}

// TestApplyUserSliceLimitsSuccessKeepsCleanupArmed guards the fixed contract
// end to end at the gate level: a fully converged gate returns the content,
// which is what arms the spawn rollback's slice cleanup.
func TestApplyUserSliceLimitsSuccessKeepsCleanupArmed(t *testing.T) {
	uid := "61001"
	dropinDir := filepath.Join(t.TempDir(), "user-"+uid+".slice.d")
	prevDir := systemdUnitDirRoot
	systemdUnitDirRoot = filepath.Dir(dropinDir)
	t.Cleanup(func() { systemdUnitDirRoot = prevDir })

	fake := &intci37FakeCtl{}
	prevCtl := sliceApplySystemctl
	sliceApplySystemctl = fake.run
	t.Cleanup(func() { sliceApplySystemctl = prevCtl })

	// No cgroup fixture: an unrequested knob set never reaches a landing
	// read, mirroring a host with no containment knobs (verify skips all).
	content, err := applyUserSliceLimits(context.Background(), perf024ResidueUser(), 2.0, 4<<30, 0, 0, 0, nil, testLogger())
	if err != nil {
		t.Fatalf("the converged gate must succeed: %v", err)
	}
	if content == "" || !strings.Contains(content, "[Slice]") {
		t.Errorf("the gate must return the written drop-in content: %q", content)
	}
	if fake.reloads < 1 {
		t.Errorf("the daemon-reload never ran: %v", fake.calls)
	}
}
