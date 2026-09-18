package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── DF-BUNKER-21 acceptance criterion 4 ────────────────────────────────────
//
// The board criterion is: "a test reproduces the recycled-UID-with-stale-slice
// case and asserts zero residue", and the board's AC3 is: "before reusing a UID
// the daemon stops/resets the previous occupant's user-<uid>.slice and linger
// state".
//
// The pre-existing coverage proved the pieces separately and never on one
// spawn:
//
//   - TestProveUserManagerReachableWithRecovery_RecycledUidIsRecoveredOnce is a
//     unit-level CALL-ORDER test of the recovery: it never runs a spawn, never
//     seeds a stale user-<uid>.slice, and never rolls anything back;
//   - TestSpawnRollbackUnderCancelledRequestRunsEveryStepOnALiveContext runs the
//     REAL spawn + rollback, but on a FRESH uid with no stale slice and no
//     recycled manager, so the recovery path is never entered.
//
// This test drives BOTH on ONE spawn against a host that is deliberately dirty
// in the way the QA host (bunker-las-03) was dirty: a uid recycled from a
// destroyed agent whose manager is still live, a linger entry for that dead
// owner, a user-<uid>.slice drop-in carrying the dead agent's cgroup limits, and
// a stale /run/user/<uid> holding the foreign manager's bus. The spawn recovers
// from the recycled uid (the AC3 reset), proceeds, then fails at the rootless
// install stage so the compensating rollback runs.
//
// What it asserts afterwards is RESIDUE ON THE HOST PLANES, not call counts:
// the user is gone from the host's user database, the home is gone, the linger
// directory is EMPTY (the dead owner's entry AND the new user's), the uid's
// slice drop-in is gone, the persisted key is gone, and the in-memory/durable
// planes (port range, tracker, registry) are clean. The residue inventory
// (AC5's probe) is then asked to confirm all four counts are zero, so the
// operator surface is proven against real host state rather than a fixture.
//
// Remaining planes deliberately NOT asserted (and not counted by the inventory
// either): the per-agent scratch dirs under spawnRunRoot (<run>/bunker/<id>/…)
// and /run/user/<uid> itself, which the recovery legitimately recreates as part
// of bringing the manager back up and logind owns thereafter.

const dfb21RecycledAgentID = "dfb21-rec"

// recycledResidueHost is the dirty host the combined regression runs against:
// every host plane the spawn and the rollback touch is a temp directory, so the
// test needs no root and cannot damage the machine it runs on.
type recycledResidueHost struct {
	m       *AgentManager
	sys     *recycledUidHost
	uid     int
	agentID string
	user    string

	homeRoot   string
	sshDir     string
	lingerPath string
	sliceRoot  string
	passwdPath string
	tmpDir     string
	cmdLog     string
}

func newRecycledResidueHost(t *testing.T, agentID string) *recycledResidueHost {
	t.Helper()

	h := &recycledResidueHost{
		uid:     intSpawn001PresentUID, // 61001, the uid the shared user stubs model
		agentID: agentID,
		user:    "bunker-" + agentID,
	}
	homeRoot := filepath.Join(t.TempDir(), "home")
	sshDir := filepath.Join(t.TempDir(), "ssh")
	lingerPath := filepath.Join(t.TempDir(), "linger")
	sliceRoot := filepath.Join(t.TempDir(), "systemd")
	passwdPath := filepath.Join(t.TempDir(), "passwd")
	tmpDir := t.TempDir()
	runtimeBase := filepath.Join(t.TempDir(), "run", "user")
	for _, dir := range []string{homeRoot, sshDir, lingerPath, sliceRoot, runtimeBase, tmpDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A user database that only holds the pieces this test owns: the spawn's
	// useradd stub appends to it and its userdel stub removes the line again, so
	// "is the agent user still on the host?" is a real read of the same file the
	// production probe (defaultListSystemAgents / ResidueInventory) reads.
	if err := os.WriteFile(passwdPath, []byte("root:x:0:0:root:/root:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.homeRoot, h.sshDir, h.lingerPath, h.sliceRoot, h.passwdPath, h.tmpDir = homeRoot, sshDir, lingerPath, sliceRoot, passwdPath, tmpDir

	// The spawn path derives every one of these roots from a package-level var
	// (the existing test-seam pattern); point them at the temp host.
	swapStringSeam(t, &agentHomeRoot, homeRoot)
	swapStringSeam(t, &spawnRunRoot, filepath.Join(t.TempDir(), "run", "bunker"))
	swapStringSeam(t, &userRuntimeBaseDir, runtimeBase)
	swapStringSeam(t, &systemdUnitDirRoot, sliceRoot)
	swapStringSeam(t, &agentPasswdPath, passwdPath)
	swapStringSeam(t, &subUIDPath, filepath.Join(t.TempDir(), "subuid"))
	swapStringSeam(t, &subGIDPath, filepath.Join(t.TempDir(), "subgid"))
	t.Setenv("TMPDIR", tmpDir)

	// The manager: temp registry, temp SSH dir, recorded host provisioning —
	// the hermetic INT-CI-005 fixture.
	m := intci5Manager(t)
	m.cfg.Agent.SSHDir = sshDir
	h.m = m

	// The systemd-side host: scripted user manager, session probe, logind record
	// and — via lingerModelPath — a REAL linger directory the loginctl verbs
	// mutate.
	sys := newRecycledUidHost(t)
	sys.uid = h.uid
	sys.username = h.user
	sys.runtimeDir = filepath.Join(runtimeBase, strconv.Itoa(h.uid))
	sys.dirOwner = uint32(h.uid)
	sys.lingerModelPath = lingerPath
	sys.waitBudget = 50 * time.Millisecond
	sys.foreignServedAfterTeardown = true
	// The uid's logind record belongs to the destroyed previous owner and is
	// lingering; the re-check after the teardown finds it gone.
	sys.records = []*logindRecordStub{{name: rcForeignRecordName, linger: "yes", state: "active"}, nil}
	sys.install(t, lingerPath)
	h.sys = sys

	// configureSubIDs / installRootlessDocker resolve the agent through the
	// userLookup seam; the recycled host's stub carries no Gid, which the
	// rootless uid mapping needs.
	prevUserLookup := userLookup
	userLookup = func(name string) (*user.User, error) {
		if name != h.user {
			return nil, user.UnknownUserError(name)
		}
		return &user.User{
			Username: name,
			Uid:      strconv.Itoa(h.uid),
			Gid:      strconv.Itoa(h.uid),
			HomeDir:  filepath.Join(homeRoot, name),
		}, nil
	}
	t.Cleanup(func() { userLookup = prevUserLookup })

	// The PATH stubs are the real host commands the spawn and the rollback
	// invoke: useradd/userdel keep the temp user database and the agent home in
	// step, so the assertions read host-state facts rather than stub promises.
	h.installHostStubs(t)

	// The remaining seams the spawn path uses.
	restoreAgentUser := lookupAgentUser
	lookupAgentUser = func(name string) (*user.User, error) {
		return &user.User{Username: name, Uid: strconv.Itoa(h.uid), Gid: strconv.Itoa(h.uid), HomeDir: filepath.Join(homeRoot, name)}, nil
	}
	t.Cleanup(func() { lookupAgentUser = restoreAgentUser })

	// The rollback resolves the user through lookupUser before it clears linger
	// and the manager; the uid must be the recycled one.
	stubLookupUser(t, presentUserStub(h.user))

	prevDisable := disableLinger
	disableLinger = func(ctx context.Context, username string) ([]byte, error) {
		// Same host seam the recovery uses, so the linger plane has ONE model.
		if username != h.user {
			return nil, nil
		}
		return sys.systemRunner(ctx, "loginctl", "disable-linger", username)
	}
	t.Cleanup(func() { disableLinger = prevDisable })

	prevRootHost := rootHostRunner
	rootHostRunner = sys.systemRunner
	t.Cleanup(func() { rootHostRunner = prevRootHost })

	prevDownload := rootlessInstallerDownload
	rootlessInstallerDownload = func(_ context.Context, installerPath string) ([]byte, error) {
		if err := os.MkdirAll(filepath.Dir(installerPath), 0o755); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(installerPath, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	}
	t.Cleanup(func() { rootlessInstallerDownload = prevDownload })

	return h
}

// installHostStubs writes the PATH stubs for the commands the spawn runs
// directly (they are not behind a seam, by design: the rollback's semantics are
// about the CONTEXT they are handed, which the budget tests pin separately).
func (h *recycledResidueHost) installHostStubs(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	h.cmdLog = filepath.Join(t.TempDir(), "cmds.log")

	useraddBody := `name=""
for a in "$@"; do
  case "$a" in
    -*) ;;
    *) name="$a" ;;
  esac
done
[ -n "$name" ] || exit 1
mkdir -p "` + h.homeRoot + `/$name"
printf '%s\n' "$name:x:` + strconv.Itoa(h.uid) + `:` + strconv.Itoa(h.uid) + `:bunker test agent:` + h.homeRoot + `/$name:/bin/bash" >> "` + h.passwdPath + `"
printf '%s\n' "useradd $*" >> "` + h.cmdLog + `"
exit 0
`
	// userdel -r really removes the home and the user database line: the "zero
	// residue" claims about the user and home planes are filesystem facts.
	userdelBody := `name=""
for a in "$@"; do
  case "$a" in
    -*) ;;
    *) name="$a" ;;
  esac
done
[ -n "$name" ] || exit 1
printf '%s\n' "userdel $*" >> "` + h.cmdLog + `"
rm -rf "` + h.homeRoot + `/$name"
grep -v "^$name:" "` + h.passwdPath + `" > "` + h.passwdPath + `.new"
mv "` + h.passwdPath + `.new" "` + h.passwdPath + `"
exit 0
`

	writeStub(t, binDir, "useradd", useraddBody)
	writeStub(t, binDir, "userdel", userdelBody)
	writeStub(t, binDir, "chown", stubSucceeds)
	writeStub(t, binDir, "pkill", stubSucceeds)
	writeStub(t, binDir, "pgrep", "exit 1\n")
	writeStub(t, binDir, "ssh-keygen", keygenStubBody)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// seedStaleUidState makes the uid dirty exactly the way a uid recycled from a
// destroyed agent is dirty: the dead owner still lingers, its user-<uid>.slice
// drop-in still carries its limits, and its user manager is still live with its
// bus socket in the uid's runtime directory.
func (h *recycledResidueHost) seedStaleUidState(t *testing.T) {
	t.Helper()
	h.sys.foreignManagerRunning(t)

	if err := os.WriteFile(filepath.Join(h.lingerPath, rcForeignRecordName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dropinDir := userSliceDropinDir(strconv.Itoa(h.uid))
	if err := os.MkdirAll(dropinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The limits the DESTROYED agent had: if the reuse does not reset them, the
	// new agent inherits them (and the file is the residue this test asserts is
	// gone).
	if err := os.WriteFile(filepath.Join(dropinDir, "50-bunker.conf"), []byte("[Slice]\nCPUQuota=50%\nMemoryMax=268435456\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSpawnRecycledUidWithStaleSliceRollsBackToZeroResidue is the criterion-4
// regression: recovery from a recycled uid AND the failed-spawn rollback on ONE
// spawn, asserted as zero residue on every modelled plane.
func TestSpawnRecycledUidWithStaleSliceRollsBackToZeroResidue(t *testing.T) {
	h := newRecycledResidueHost(t, dfb21RecycledAgentID)
	h.seedStaleUidState(t)
	journal := redirectBreadcrumbJournal(t)

	var logBuf strings.Builder
	m := h.m
	m.logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// The spawn must reach the recycled-uid recovery and then FAIL at the
	// rootless install stage, so the compensating rollback runs against a host
	// the recovery has just worked on.
	// The output deliberately carries NEITHER "not found" NOR "docker.service",
	// so the installer failure is not mistaken for the unit-not-found race the
	// install path retries once (isUserUnitNotFound).
	prevRunner := rootlessInstallerRunner
	rootlessInstallerRunner = func(context.Context, string, string, string) ([]byte, error) {
		return []byte("rootless installer: simulated failure\n"), errors.New("exit status 1")
	}
	t.Cleanup(func() { rootlessInstallerRunner = prevRunner })

	// ── premises: the host really is dirty in the way the test claims ──────
	uidArg := strconv.Itoa(h.uid)
	dropinDir := userSliceDropinDir(uidArg)
	if _, err := os.Stat(filepath.Join(dropinDir, "50-bunker.conf")); err != nil {
		t.Fatalf("test premise broken: the stale slice drop-in was not seeded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.lingerPath, rcForeignRecordName)); err != nil {
		t.Fatalf("test premise broken: the dead owner's linger entry was not seeded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.sys.runtimeDir, "bus")); err != nil {
		t.Fatalf("test premise broken: the foreign manager's bus socket was not seeded: %v", err)
	}
	if passwdHasUser(t, h.passwdPath, h.user) {
		t.Fatalf("test premise broken: %s is already in the fixture user database", h.user)
	}

	// ── the spawn ─────────────────────────────────────────────────────────
	resp, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: h.agentID, Ttl: "1h"})
	if err == nil {
		t.Fatalf("Spawn() returned success although the rootless installer failed: %+v", resp)
	}
	if !strings.Contains(err.Error(), "failed at stage "+StageRootlessInstall) {
		t.Errorf("spawn error does not name the rootless-install stage: %v", err)
	}

	// ── the recovery RAN (or the test is not about the recycled-uid case) ──
	for _, want := range []string{
		"loginctl disable-linger " + rcForeignRecordName, // the dead owner's linger state
		"systemctl stop " + userManagerUnitName(h.uid),   // the foreign manager
		"systemctl stop user-" + uidArg + ".slice",       // the previous occupant's slice
		"loginctl enable-linger " + h.user,               // the bring-up after the teardown
	} {
		if h.sys.countPrefix(want) == 0 {
			t.Errorf("the recycled-uid recovery never ran %q; calls:%s", want, h.sys.callLog())
		}
	}
	if got := len(warnLinesWithMarker(logBuf.String(), recycledUIDRecoveryMarker)); got != 1 {
		t.Errorf("expected exactly 1 recycled-uid recovery WARN, got %d:\n%s", got, logBuf.String())
	}

	// ── the rollback RAN ──────────────────────────────────────────────────
	userdelCalls := readRecord(t, h.cmdLog)
	if lineIndex(userdelCalls, "userdel -r "+h.user) < 0 {
		t.Errorf("the rollback never ran `userdel -r %s`: %v", h.user, userdelCalls)
	}
	for _, step := range []string{
		"loginctl disable-linger " + h.user,
		"systemctl stop " + userManagerUnitName(h.uid),
	} {
		if h.sys.countPrefix(step) == 0 {
			t.Errorf("rollback never ran %q; calls:%s", step, h.sys.callLog())
		}
	}
	// The rollback clears linger BEFORE it stops the manager and removes the
	// user (INT-SPAWN-001): the whole chain must precede the userdel, or userdel
	// fails with "currently used by process". The manager stop is looked up
	// AFTER the rollback's own linger call, because the recovery issues the same
	// command earlier in the spawn.
	idxLinger := h.sys.nextIndexOf("loginctl disable-linger "+h.user, 0)
	idxStop := h.sys.nextIndexOf("systemctl stop "+userManagerUnitName(h.uid), idxLinger+1)
	if idxLinger < 0 || idxStop < 0 {
		t.Errorf("the rollback chain is incomplete: linger=%d manager-stop=%d calls:%s", idxLinger, idxStop, h.sys.callLog())
	}

	// ── ZERO RESIDUE on every plane the criterion names ────────────────────
	// user
	if passwdHasUser(t, h.passwdPath, h.user) {
		t.Errorf("agent user %s survived the rollback (still in %s)", h.user, h.passwdPath)
	}
	// home
	if _, statErr := os.Stat(filepath.Join(h.homeRoot, h.user)); !os.IsNotExist(statErr) {
		t.Errorf("agent home survived the rollback: %s (stat error: %v)", filepath.Join(h.homeRoot, h.user), statErr)
	}
	// key material
	if entries, err := os.ReadDir(h.sshDir); err != nil {
		t.Errorf("read agent ssh dir: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("persisted SSH keys survived the rollback: %v", entryNames(entries))
	}
	if _, statErr := os.Stat(filepath.Join(h.tmpDir, "bunker-key-"+h.agentID)); !os.IsNotExist(statErr) {
		t.Errorf("temporary key material survived the rollback")
	}
	// linger (the dead owner's entry AND the new user's)
	if entries, err := os.ReadDir(h.lingerPath); err != nil {
		t.Errorf("read linger dir: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("linger residue survived (the dead owner's entry and/or the rolled-back user's): %v", entryNames(entries))
	}
	// slice
	if _, statErr := os.Stat(dropinDir); !os.IsNotExist(statErr) {
		t.Errorf("stale user slice drop-in for uid %s survived the recycled-uid reset: %s", uidArg, dropinDir)
	}
	// port range / tracker / registry
	if m.portAlloc.Has(h.agentID) {
		t.Errorf("port range for %s was never freed", h.agentID)
	}
	if m.tracker.Get(h.agentID) != nil || m.tracker.Count() != 0 {
		t.Errorf("tracker residue: Get=%v count=%d", m.tracker.Get(h.agentID), m.tracker.Count())
	}
	if m.registry != nil && m.registry.Get(h.agentID) != nil {
		t.Errorf("durable registry still holds a record for %s", h.agentID)
	}

	// ── the operator-facing residue probe agrees (AC5 surface, real planes) ─
	inv := m.ResidueInventory()
	if inv.OrphanUsers != 0 || inv.OrphanHomes != 0 || inv.OrphanKeys != 0 || inv.StaleLinger != 0 {
		t.Errorf("the residue inventory still sees residue after the rollback: %+v", inv)
	}
	if inv.Status != ResidueStatusOK {
		t.Errorf("residue inventory status = %q, want %q (detail: %s)", inv.Status, ResidueStatusOK, inv.Detail)
	}
	if inv.Registered != 0 {
		t.Errorf("residue inventory reports %d registered agents after a fully rolled-back spawn", inv.Registered)
	}

	// ── the breadcrumb is honest about what the rollback could not fix ─────
	bc := readSingleBreadcrumb(t, journal)
	if bc["stage"] != StageRootlessInstall {
		t.Errorf("breadcrumb stage = %v, want %q", bc["stage"], StageRootlessInstall)
	}
	if ran := breadcrumbList(t, bc, "rollback_ran"); !containsPrefix(ran, "userdel "+h.user) {
		t.Errorf("rollback_ran does not carry the userdel: %v", ran)
	}
	for _, f := range breadcrumbList(t, bc, "rollback_failed") {
		if !strings.HasPrefix(f, "isolation: ") {
			t.Errorf("the rollback reported a failure outside the isolation plane: %q", f)
		}
	}
}

// swapStringSeam points a package-level host-path seam at a temp value for one
// test and restores it afterwards.
func swapStringSeam(t *testing.T, target *string, value string) {
	t.Helper()
	prev := *target
	*target = value
	t.Cleanup(func() { *target = prev })
}

// passwdHasUser reports whether the fixture user database still carries a line
// for name — the same parse defaultListSystemAgents and the residue inventory
// use.
func passwdHasUser(t *testing.T, path, name string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Split(line, ":"); len(fields) >= 6 && fields[0] == name {
			return true
		}
	}
	return false
}

// entryNames renders directory entries for a failure message.
func entryNames(entries []os.DirEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
