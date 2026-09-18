package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ── INT-CI-008: runtime-directory ordering + conditional reset ──────────────
//
// The regression job failed because installRootlessDocker removed
// /run/user/<uid> as a "reset" preamble and only re-created it AFTER the
// manager start. On systemd 255 user@.service has no Requires/After on
// user-runtime-dir@<uid>.service, so pam_systemd refused to set
// XDG_RUNTIME_DIR ("Failed to stat() runtime directory") and systemd --user
// exited 1 — the unit could never go active.
//
// These tests drive the whole bring-up through the existing userManagerRunner
// seam, so no root, no live systemd and no real user are involved. The only
// state that cannot be produced for real without root is the OWNERSHIP of the
// runtime directory (a chown to a foreign uid), which runtimeDirProbe
// simulates. Directory creation/removal is real on-disk work.

const (
	ciTestUID      = 1002
	ciTestUsername = "bunker-regr-alpha"
)

// userManagerOwnership simulates the owner of the runtime directory.
type userManagerOwnership struct {
	owner uint32

	// transientForeignProbes, when > 0, makes the first N probes report
	// transientForeignOwner instead of owner. That is the INT-CI-019
	// fingerprint: the chown was accepted (owner becomes the uid) yet the path
	// reports a FOREIGN owner right afterwards, because a concurrent actor
	// (logind's user-runtime-dir@<uid>.service during a uid transition) is
	// replacing it. probes counts the probes this simulator answered.
	transientForeignProbes int
	transientForeignOwner  uint32
	probes                 int
}

// probe mirrors probeRuntimeDirOnDisk for existence and type, and substitutes
// the simulated owner so the "stale directory owned by a previous user" case
// is reachable in a test.
func (o *userManagerOwnership) probe(path string) (runtimeDirInfo, error) {
	info, err := probeRuntimeDirOnDisk(path)
	if err != nil || !info.exists {
		return info, err
	}
	o.probes++
	owner := o.owner
	if o.probes <= o.transientForeignProbes {
		owner = o.transientForeignOwner
	}
	info.owner = owner
	info.ownerKnown = true
	return info, nil
}

// userManagerHost is a fake host for the user-manager bring-up. It records every
// argv issued through the userManagerRunner seam and answers from simulated
// state:
//   - `systemctl start user-runtime-dir@<uid>.service` creates the directory
//     owned by the uid, like logind's own unit;
//   - `systemctl start user@<uid>.service` writes the manager's bus socket —
//     unless startAlwaysErr is set, which models the CI failure;
//   - `chown <user>: <dir>` flips the simulated owner to the uid.
type userManagerHost struct {
	uid        int
	username   string
	runtimeDir string

	ownership *userManagerOwnership

	managerState   string // is-active answer for user@<uid>.service
	startAlwaysErr bool   // every start of user@<uid>.service fails
	journalLines   []string
	journalErr     bool // journalctl unavailable → force the status fallback
	statusLines    []string

	// ownershipNotApplied models a host where nothing the bring-up does results
	// in the uid owning the runtime directory (a create/chown that reports
	// success without applying): the verification step must catch that.
	ownershipNotApplied bool

	// observations
	calls                    []string
	managerStartSeen         bool
	dirExistedAtManagerStart bool
}

func newUserManagerHost(t *testing.T) *userManagerHost {
	t.Helper()
	return &userManagerHost{
		uid:          ciTestUID,
		username:     ciTestUsername,
		runtimeDir:   filepath.Join(t.TempDir(), "run", "user", strconv.Itoa(ciTestUID)),
		ownership:    &userManagerOwnership{},
		managerState: "inactive",
	}
}

func (h *userManagerHost) run(_ context.Context, name string, args ...string) ([]byte, error) {
	h.calls = append(h.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	switch name {
	case "chown":
		if !h.ownershipNotApplied {
			h.ownership.owner = uint32(h.uid)
		}
		return nil, nil
	case "rm":
		if len(args) >= 2 {
			if err := os.RemoveAll(args[len(args)-1]); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case "journalctl":
		if h.journalErr {
			return nil, errors.New("exec: \"journalctl\": executable file not found in $PATH")
		}
		return []byte(strings.Join(h.journalLines, "\n") + "\n"), nil
	case "loginctl":
		if len(args) == 0 {
			return nil, errors.New("loginctl: missing verb")
		}
		switch args[0] {
		case "enable-linger", "terminate-user":
			return nil, nil
		}
		return nil, fmt.Errorf("loginctl: unexpected verb %q", args[0])
	case "systemctl":
		if len(args) == 0 {
			return nil, errors.New("systemctl: missing verb")
		}
		unit := ""
		if len(args) > 1 {
			unit = args[len(args)-1]
		}
		switch args[0] {
		case "start":
			return h.startUnit(unit)
		case "stop", "reset-failed":
			return nil, nil
		case "is-active":
			if unit == userManagerUnitName(h.uid) && h.managerState != "active" && h.managerState != "activating" {
				return []byte(h.managerState + "\n"), errors.New("exit status 3")
			}
			return []byte(h.managerState + "\n"), nil
		case "show":
			return []byte("exit-code\n"), nil
		case "status":
			if len(h.statusLines) == 0 {
				return nil, errors.New("exit status 3")
			}
			return []byte(strings.Join(h.statusLines, "\n") + "\n"), nil
		}
		return nil, fmt.Errorf("systemctl: unexpected verb %q", args[0])
	}
	return nil, fmt.Errorf("unexpected command %q", name)
}

func (h *userManagerHost) startUnit(unit string) ([]byte, error) {
	switch unit {
	case userRuntimeDirUnitName(h.uid):
		// logind's runtime-directory unit creates /run/user/<uid> owned by the uid.
		if err := os.MkdirAll(h.runtimeDir, 0o700); err != nil {
			return nil, err
		}
		if !h.ownershipNotApplied {
			h.ownership.owner = uint32(h.uid)
		}
		return nil, nil
	case userManagerUnitName(h.uid):
		h.managerStartSeen = true
		// THE property under test: when the manager start is issued, the runtime
		// directory must already exist and be owned by the uid.
		info, err := h.ownership.probe(h.runtimeDir)
		if err != nil {
			return nil, err
		}
		h.dirExistedAtManagerStart = info.exists && info.isDir && info.ownerKnown && info.owner == uint32(h.uid)
		if h.startAlwaysErr {
			return []byte(fmt.Sprintf("Job for %s failed because the control process exited with error code", unit)),
				errors.New("exit status 1")
		}
		// A healthy manager publishes its bus socket in the runtime directory.
		if err := os.WriteFile(filepath.Join(h.runtimeDir, "bus"), nil, 0o600); err != nil {
			return nil, err
		}
		h.managerState = "active"
		return nil, nil
	}
	return nil, fmt.Errorf("systemctl start: unexpected unit %q", unit)
}

func (h *userManagerHost) ran(command string) bool { return h.callIndex(command) >= 0 }

func (h *userManagerHost) ranPrefix(prefix string) bool {
	for _, c := range h.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (h *userManagerHost) callIndex(command string) int {
	for i, c := range h.calls {
		if c == command {
			return i
		}
	}
	return -1
}

func (h *userManagerHost) callLog() string { return "\n  " + strings.Join(h.calls, "\n  ") }

// installUserManagerHost swaps every user-manager seam for the fake host and
// restores them on cleanup.
func installUserManagerHost(t *testing.T, h *userManagerHost, lingerDirPath string) {
	t.Helper()
	prevRunner := userManagerRunner
	prevProbe := runtimeDirProbe
	prevLinger := lingerDir
	prevPoll := userManagerPollInterval
	prevTimeout := userManagerWaitTimeoutOverride
	userManagerRunner = h.run
	runtimeDirProbe = h.ownership.probe
	lingerDir = lingerDirPath
	userManagerPollInterval = time.Millisecond
	userManagerWaitTimeoutOverride = 5 * time.Second
	t.Cleanup(func() {
		userManagerRunner = prevRunner
		runtimeDirProbe = prevProbe
		lingerDir = prevLinger
		userManagerPollInterval = prevPoll
		userManagerWaitTimeoutOverride = prevTimeout
	})
}

// lingerDirWithEntries returns a temp linger directory holding n entries, so
// the attribution fragment can be asserted exactly.
func lingerDirWithEntries(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("stale-%d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestBringUpUserManager_FreshPathIsNonDestructive covers acceptance (a) and
// (b) for the fresh uid: no stop, no terminate-user, no removal of the runtime
// directory, and the runtime directory exists (owned by the uid) before
// user@<uid>.service is started.
func TestBringUpUserManager_FreshPathIsNonDestructive(t *testing.T) {
	h := newUserManagerHost(t)
	installUserManagerHost(t, h, lingerDirWithEntries(t, 3))

	if err := bringUpUserManager(context.Background(), h.username, h.uid, h.runtimeDir, ctlLogger()); err != nil {
		t.Fatalf("bringUpUserManager() error = %v", err)
	}

	// (a) the fresh path must not take any destructive action.
	for _, forbidden := range []string{
		"systemctl stop " + userManagerUnitName(h.uid),
		"systemctl stop " + userRuntimeDirUnitName(h.uid),
		"loginctl terminate-user " + strconv.Itoa(h.uid),
		"rm -rf " + h.runtimeDir,
	} {
		if h.ran(forbidden) {
			t.Errorf("fresh path issued destructive command %q; it must not reset anything:%s", forbidden, h.callLog())
		}
	}
	if h.ranPrefix("rm ") {
		t.Errorf("fresh path removed something (rm argv present):%s", h.callLog())
	}

	// (b) the runtime directory bring-up precedes the manager start — both by
	// argv order and by the on-disk state observed at the start itself.
	dirIdx := h.callIndex("systemctl start " + userRuntimeDirUnitName(h.uid))
	managerIdx := h.callIndex("systemctl start " + userManagerUnitName(h.uid))
	if dirIdx < 0 {
		t.Fatalf("runtime-directory unit was never started:%s", h.callLog())
	}
	if managerIdx < 0 {
		t.Fatalf("user manager was never started:%s", h.callLog())
	}
	if dirIdx > managerIdx {
		t.Errorf("runtime-dir bring-up (call %d) must precede the manager start (call %d):%s",
			dirIdx, managerIdx, h.callLog())
	}
	chownIdx := h.callIndex("chown " + h.username + ": " + h.runtimeDir)
	if chownIdx < 0 {
		t.Fatalf("runtime dir ownership was never set:%s", h.callLog())
	}
	if chownIdx > managerIdx {
		t.Errorf("runtime dir chown (call %d) must precede the manager start (call %d):%s",
			chownIdx, managerIdx, h.callLog())
	}
	if !h.managerStartSeen {
		t.Fatal("the manager start was never issued")
	}
	if !h.dirExistedAtManagerStart {
		t.Errorf("the runtime directory was missing (or not owned by uid %d) when user@%d.service was started",
			h.uid, h.uid)
	}
	if !h.ran("loginctl enable-linger " + h.username) {
		t.Errorf("linger was not enabled:%s", h.callLog())
	}
}

// TestBringUpUserManager_StaleRuntimeDirTriggersReset covers acceptance (d): a
// runtime directory owned by a DIFFERENT uid (state left behind by a previous
// user of a recycled uid) still takes the reset path, in a state-consistent
// order, and the manager still comes up afterwards.
func TestBringUpUserManager_StaleRuntimeDirTriggersReset(t *testing.T) {
	h := newUserManagerHost(t)
	// The stale directory exists on disk and is owned by another uid.
	if err := os.MkdirAll(h.runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	h.ownership.owner = 4242
	installUserManagerHost(t, h, lingerDirWithEntries(t, 3))

	if err := bringUpUserManager(context.Background(), h.username, h.uid, h.runtimeDir, ctlLogger()); err != nil {
		t.Fatalf("bringUpUserManager() error = %v", err)
	}

	for _, want := range []string{
		"systemctl stop " + userManagerUnitName(h.uid),
		"loginctl terminate-user " + strconv.Itoa(h.uid),
		"systemctl stop " + userRuntimeDirUnitName(h.uid),
		"rm -rf " + h.runtimeDir,
	} {
		if !h.ran(want) {
			t.Errorf("stale runtime dir did not trigger %q:%s", want, h.callLog())
		}
	}

	// State-consistent order: manager stop → runtime-dir unit stop → removal →
	// re-creation → manager start.
	order := []string{
		"systemctl stop " + userManagerUnitName(h.uid),
		"systemctl stop " + userRuntimeDirUnitName(h.uid),
		"rm -rf " + h.runtimeDir,
		"systemctl start " + userRuntimeDirUnitName(h.uid),
		"systemctl start " + userManagerUnitName(h.uid),
	}
	prev := -1
	for _, cmd := range order {
		idx := h.callIndex(cmd)
		if idx <= prev {
			t.Fatalf("reset sequence out of order at %q (call %d, previous %d):%s", cmd, idx, prev, h.callLog())
		}
		prev = idx
	}

	if !h.dirExistedAtManagerStart {
		t.Errorf("the runtime directory was missing (or not owned by uid %d) when the manager restarted",
			h.uid)
	}
}

// TestBringUpUserManager_FailedStartCarriesJournalReason covers acceptance (c):
// a failed start surfaces the journal reason (the line that names the real
// cause, e.g. pam_systemd) in the returned error, alongside the existing
// linger_entries field — while the sub-stage name stays user-manager-start.
func TestBringUpUserManager_FailedStartCarriesJournalReason(t *testing.T) {
	pamLine := "systemd[1381387]: pam_systemd(systemd-user:session): Failed to stat() runtime directory '/run/user/1002': No such file or directory"
	pamLine2 := "systemd[1381387]: pam_systemd(systemd-user:session): Not setting XDG_RUNTIME_DIR, as the directory is not in order."

	t.Run("journalctl", func(t *testing.T) {
		h := newUserManagerHost(t)
		h.startAlwaysErr = true
		// A literal % must survive verbatim: the excerpt is host output, never a
		// format string.
		h.journalLines = []string{pamLine, pamLine2, "100% of starts for %s failed"}
		installUserManagerHost(t, h, lingerDirWithEntries(t, 3))

		err := bringUpUserManager(context.Background(), h.username, h.uid, h.runtimeDir, ctlLogger())
		if err == nil {
			t.Fatal("expected an error when every manager start fails")
		}
		msg := err.Error()
		for _, want := range []string{
			"user-manager-start",
			"linger_entries=3",
			"pam_systemd",
			"XDG_RUNTIME_DIR",
			"100% of starts for %s failed",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("attribution error missing %q, got: %s", want, msg)
			}
		}
		if strings.Contains(msg, "\n") {
			t.Errorf("attribution error must stay single-line, got: %q", msg)
		}
		if strings.Contains(msg, "%!") {
			t.Errorf("journal text was interpreted as a format string: %s", msg)
		}
	})

	t.Run("systemctl_status_fallback", func(t *testing.T) {
		h := newUserManagerHost(t)
		h.startAlwaysErr = true
		h.journalErr = true // journalctl unavailable
		h.statusLines = []string{"user@1002.service - User Manager for UID 1002", pamLine}
		installUserManagerHost(t, h, lingerDirWithEntries(t, 3))

		err := bringUpUserManager(context.Background(), h.username, h.uid, h.runtimeDir, ctlLogger())
		if err == nil {
			t.Fatal("expected an error when every manager start fails")
		}
		msg := err.Error()
		for _, want := range []string{"user-manager-start", "pam_systemd", "linger_entries=3"} {
			if !strings.Contains(msg, want) {
				t.Errorf("attribution error missing %q, got: %s", want, msg)
			}
		}
	})

	t.Run("no_journal_is_not_a_new_failure", func(t *testing.T) {
		h := newUserManagerHost(t)
		h.startAlwaysErr = true
		h.journalErr = true // no journalctl and no usable status
		installUserManagerHost(t, h, lingerDirWithEntries(t, 3))

		err := bringUpUserManager(context.Background(), h.username, h.uid, h.runtimeDir, ctlLogger())
		if err == nil {
			t.Fatal("expected the start failure to still surface")
		}
		msg := err.Error()
		if !strings.Contains(msg, "user-manager-start") || !strings.Contains(msg, "linger_entries=3") {
			t.Errorf("missing-journal attribution must keep the original fields, got: %s", msg)
		}
		if strings.Contains(msg, "journal=") {
			t.Errorf("no journal excerpt was available, so none may be advertised, got: %s", msg)
		}
	})
}

// TestEnsureUserRuntimeDir_VerifiesOwnership pins the verification step: a chown
// that reports success without changing the owner must fail loudly BEFORE the
// manager is started, because pam_systemd would reject that directory anyway.
func TestEnsureUserRuntimeDir_VerifiesOwnership(t *testing.T) {
	t.Run("owned_by_uid", func(t *testing.T) {
		h := newUserManagerHost(t)
		installUserManagerHost(t, h, lingerDirWithEntries(t, 0))
		if err := ensureUserRuntimeDir(context.Background(), h.username, h.uid, h.runtimeDir, ctlLogger()); err != nil {
			t.Fatalf("ensureUserRuntimeDir() error = %v", err)
		}
		if _, err := os.Stat(h.runtimeDir); err != nil {
			t.Errorf("runtime dir was not created: %v", err)
		}
	})

	t.Run("owner_not_applied", func(t *testing.T) {
		h := newUserManagerHost(t)
		h.ownershipNotApplied = true
		h.ownership.owner = 4242
		installUserManagerHost(t, h, lingerDirWithEntries(t, 0))
		err := ensureUserRuntimeDir(context.Background(), h.username, h.uid, h.runtimeDir, ctlLogger())
		if err == nil {
			t.Fatal("expected a verification error when the runtime dir is not owned by the uid")
		}
		for _, want := range []string{"4242", strconv.Itoa(h.uid)} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ownership error missing %q, got: %s", want, err.Error())
			}
		}
	})
}

// TestClassifyRuntimeDir_FailsSafe pins the fail-safe matrix: anything the probe
// cannot read or cross-check keeps the historical reset behavior instead of
// being silently skipped.
func TestClassifyRuntimeDir_FailsSafe(t *testing.T) {
	cases := []struct {
		name     string
		info     runtimeDirInfo
		probeErr error
		want     bool
	}{
		{
			name: "fresh_path_missing",
			info: runtimeDirInfo{},
			want: false,
		},
		{
			name: "owned_by_uid",
			info: runtimeDirInfo{exists: true, isDir: true, owner: ciTestUID, ownerKnown: true},
			want: false,
		},
		{
			name: "owned_by_other_uid",
			info: runtimeDirInfo{exists: true, isDir: true, owner: 4242, ownerKnown: true},
			want: true,
		},
		{
			name: "not_a_directory",
			info: runtimeDirInfo{exists: true, isDir: false},
			want: true,
		},
		{
			name: "owner_unknown",
			info: runtimeDirInfo{exists: true, isDir: true},
			want: true,
		},
		{
			name:     "probe_failed",
			info:     runtimeDirInfo{},
			probeErr: errors.New("permission denied"),
			want:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := classifyRuntimeDir(tc.info, tc.probeErr, ciTestUID)
			if got != tc.want {
				t.Errorf("classifyRuntimeDir() stale = %v, want %v (reason %q)", got, tc.want, reason)
			}
			if got && reason == "" {
				t.Error("a stale classification must carry a reason for the operator")
			}
		})
	}
}

// ── INT-CI-019: the runtime-directory guarantee CONVERGES ───────────────────
//
// CI run 35290635028, job root-suite, sha 613a92ce (a BOARD-ONLY commit)
// failed TestSpawn_GeneratesAgentID at stage rootless-install:
//
//	spawn failed ... error="install rootless docker for bunker-b06bdb59:
//	runtime dir /run/user/1004 is owned by uid 0, expected 1004"
//
// The whole failure lasted 30ms (no installer ever ran) and the SAME run
// brought another agent up on the SAME uid 1004 fourteen seconds later — and
// the string "resetting stale user manager runtime" never appears in the log,
// so the path was NOT owned by root when the spawn classified it: it became
// foreign AFTER the accepted chown, i.e. a concurrent actor (logind's
// user-runtime-dir@<uid>.service across uid recycling) replaced it between the
// chown and the probe. A single-shot verification therefore turns a transient
// race into a failed spawn.
//
// These tests pin the convergence contract: re-assert + re-probe a bounded
// number of times, WARN (and continue) when a transient mismatch converges,
// keep failing (with mount-point attribution) on exhaustion, and never take a
// destructive action on the fresh path.

// rdLogger returns a logger writing into buf, so WARN-field assertions read
// real log records instead of a private counter.
func rdLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// umCount counts the exact argv issued through the userManagerRunner seam.
func umCount(h *userManagerHost, command string) int {
	n := 0
	for _, c := range h.calls {
		if c == command {
			n++
		}
	}
	return n
}

// installMountInfoFixture points the mount-point attribution at a fixture
// /proc/self/mountinfo-style table naming mountPoint as a mount. The seam is
// restored on cleanup: this package's tests share one process.
func installMountInfoFixture(t *testing.T, mountPoint, fstype, source string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mountinfo")
	line := fmt.Sprintf("36 35 98:0 / %s rw,noatime - %s %s rw\n", mountPoint, fstype, source)
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := runtimeDirMountInfoPath
	runtimeDirMountInfoPath = path
	t.Cleanup(func() { runtimeDirMountInfoPath = prev })
}

// TestEnsureUserRuntimeDir_ConvergesTransientOwnershipMismatch is acceptance
// (3): the first probe reports a FOREIGN owner (root, exactly as the CI log
// says: "owned by uid 0") and the next one reports the uid. The guarantee must
// converge — nil error, more than one chown, the transient mismatch reported —
// instead of failing the spawn on the first probe.
func TestEnsureUserRuntimeDir_ConvergesTransientOwnershipMismatch(t *testing.T) {
	h := newUserManagerHost(t)
	// The chown is accepted every time (owner becomes the uid); the probe
	// reports uid 0 for its first answer only.
	h.ownership.transientForeignProbes = 1
	h.ownership.transientForeignOwner = 0
	installUserManagerHost(t, h, lingerDirWithEntries(t, 0))

	var logBuf bytes.Buffer
	if err := ensureUserRuntimeDir(context.Background(), h.username, h.uid, h.runtimeDir, rdLogger(&logBuf)); err != nil {
		t.Fatalf("a transient foreign owner must converge, got error = %v (calls:%s)", err, h.callLog())
	}

	chownCmd := "chown " + h.username + ": " + h.runtimeDir
	if got := umCount(h, chownCmd); got <= 1 {
		t.Errorf("ownership must be re-asserted after a foreign probe, chown ran %d time(s):%s", got, h.callLog())
	}
	if h.ownership.probes <= 1 {
		t.Errorf("verification must re-probe after a foreign owner, probe ran %d time(s)", h.ownership.probes)
	}
	if h.ownership.owner != uint32(h.uid) {
		t.Errorf("runtime dir owner after convergence = %d, want %d", h.ownership.owner, h.uid)
	}

	// The transient mismatch is REPORTED, with the attribution an operator
	// needs: the dir, the observed owner, the expected uid and the attempt.
	log := logBuf.String()
	for _, want := range []string{
		"level=WARN",
		"dir=" + h.runtimeDir,
		"owner=0",
		"expected=" + strconv.Itoa(h.uid),
		"attempt=1",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("convergence WARN missing %q, got log:\n%s", want, log)
		}
	}
}

// TestEnsureUserRuntimeDir_PersistentMismatchFailsWithMountAttribution is
// acceptance (4) and (6): a foreign owner that never goes away must still FAIL
// (fail-safe), the error must keep the greppable ownership fragment and carry
// the dir, the observed uid, the expected uid and the mount-point verdict, and
// the loop must be BOUNDED — exactly runtimeDirOwnershipAttempts chowns.
func TestEnsureUserRuntimeDir_PersistentMismatchFailsWithMountAttribution(t *testing.T) {
	h := newUserManagerHost(t)
	h.ownershipNotApplied = true // the chown reports success without applying
	h.ownership.owner = 0        // ... and the path keeps reporting root
	installUserManagerHost(t, h, lingerDirWithEntries(t, 0))
	// The path IS a mount point in the fixture table: the verdict must name its
	// filesystem type and source, which is what tells an operator a concurrent
	// mount owns the directory now.
	installMountInfoFixture(t, h.runtimeDir, "tmpfs", "tmpfs")

	var logBuf bytes.Buffer
	err := ensureUserRuntimeDir(context.Background(), h.username, h.uid, h.runtimeDir, rdLogger(&logBuf))
	if err == nil {
		t.Fatal("a runtime dir that is never owned by the uid must fail the bring-up")
	}

	msg := err.Error()
	for _, want := range []string{
		h.runtimeDir,
		"is owned by uid 0, expected " + strconv.Itoa(h.uid),
		"mount point: yes",
		"fstype tmpfs",
		"source tmpfs",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("exhaustion error missing %q, got: %s", want, msg)
		}
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("the exhaustion error must stay single-line, got: %q", msg)
	}
	if !strings.Contains(logBuf.String(), "level=WARN") {
		t.Errorf("each non-converging attempt must be reported, got log:\n%s", logBuf.String())
	}

	// (6) BOUNDED: every attempt re-asserts ownership exactly once, and the
	// budget itself stays small.
	if got, want := umCount(h, "chown "+h.username+": "+h.runtimeDir), runtimeDirOwnershipAttempts; got != want {
		t.Errorf("chown ran %d time(s), want exactly %d — the loop must be bounded:%s", got, want, h.callLog())
	}
	if runtimeDirOwnershipAttempts > 3 {
		t.Errorf("the convergence attempt budget must stay small, got %d", runtimeDirOwnershipAttempts)
	}
	if h.ownership.probes != runtimeDirOwnershipAttempts {
		t.Errorf("probe ran %d time(s), want %d (one per attempt)", h.ownership.probes, runtimeDirOwnershipAttempts)
	}
}

// TestRuntimeDirMountVerdict pins the attribution helper's three outcomes so
// the clause embedded in the exhaustion error cannot silently degrade.
func TestRuntimeDirMountVerdict(t *testing.T) {
	t.Run("not_a_mount_point", func(t *testing.T) {
		installMountInfoFixture(t, "/some/other/path", "ext4", "/dev/sda1")
		if got := runtimeDirMountVerdict("/run/user/1002"); got != "mount point: no" {
			t.Errorf("runtimeDirMountVerdict() = %q, want %q", got, "mount point: no")
		}
	})

	t.Run("mount_point_names_source_and_fstype", func(t *testing.T) {
		installMountInfoFixture(t, "/run/user/1002", "tmpfs", "tmpfs")
		got := runtimeDirMountVerdict("/run/user/1002")
		for _, want := range []string{"mount point: yes", "fstype tmpfs", "source tmpfs"} {
			if !strings.Contains(got, want) {
				t.Errorf("runtimeDirMountVerdict() = %q, want it to contain %q", got, want)
			}
		}
	})

	t.Run("unreadable_table_is_unknown_never_no", func(t *testing.T) {
		prev := runtimeDirMountInfoPath
		runtimeDirMountInfoPath = filepath.Join(t.TempDir(), "absent")
		t.Cleanup(func() { runtimeDirMountInfoPath = prev })
		got := runtimeDirMountVerdict("/run/user/1002")
		if !strings.Contains(got, "unknown") {
			t.Errorf("an unreadable mount table must be reported as unknown, got %q", got)
		}
	})
}

// TestEnsureUserRuntimeDir_AbortsOnCancelledContext pins the abort half of the
// loop: once the caller's context is done, the remaining attempts are not
// issued (the pause returns the context error) — a cancelled spawn never sleeps
// through its budget.
func TestEnsureUserRuntimeDir_AbortsOnCancelledContext(t *testing.T) {
	h := newUserManagerHost(t)
	h.ownershipNotApplied = true
	h.ownership.owner = 0
	installUserManagerHost(t, h, lingerDirWithEntries(t, 0))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // done before the first re-assert

	err := ensureUserRuntimeDir(ctx, h.username, h.uid, h.runtimeDir, ctlLogger())
	if err == nil {
		t.Fatal("a done context must abort the convergence loop")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the abort must carry the context error, got: %v", err)
	}
	if got := umCount(h, "chown "+h.username+": "+h.runtimeDir); got != 1 {
		t.Errorf("chown ran %d time(s) after cancellation, want exactly 1 — no attempt may be issued on a done context:%s",
			got, h.callLog())
	}
}

// TestEnsureUserRuntimeDir_FreshPathConvergesWithoutReset is acceptance (5),
// the INT-CI-008 regression guard: with the directory ABSENT the path is fresh,
// so no destructive reset may run at all, and the directory must end up created
// and owned by the uid.
func TestEnsureUserRuntimeDir_FreshPathConvergesWithoutReset(t *testing.T) {
	h := newUserManagerHost(t)
	installUserManagerHost(t, h, lingerDirWithEntries(t, 1))

	if _, err := os.Stat(h.runtimeDir); !os.IsNotExist(err) {
		t.Fatalf("premise: the runtime dir must be absent for the fresh path, stat err = %v", err)
	}

	if err := bringUpUserManager(context.Background(), h.username, h.uid, h.runtimeDir, ctlLogger()); err != nil {
		t.Fatalf("bringUpUserManager() error = %v", err)
	}

	// No destructive action on the fresh path (INT-CI-008), and no removal of
	// anything else either.
	for _, forbidden := range []string{
		"systemctl stop " + userManagerUnitName(h.uid),
		"systemctl stop " + userRuntimeDirUnitName(h.uid),
		"loginctl terminate-user " + strconv.Itoa(h.uid),
		"rm -rf " + h.runtimeDir,
	} {
		if h.ran(forbidden) {
			t.Errorf("the fresh path issued destructive command %q:%s", forbidden, h.callLog())
		}
	}
	if h.ranPrefix("rm ") {
		t.Errorf("the fresh path removed something (rm argv present):%s", h.callLog())
	}

	// The directory ends up created and owned by the uid — on the FIRST attempt
	// (a fresh path has nothing to converge from).
	info, err := os.Stat(h.runtimeDir)
	if err != nil {
		t.Fatalf("runtime dir was not created: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("runtime path %s is not a directory", h.runtimeDir)
	}
	if h.ownership.owner != uint32(h.uid) {
		t.Errorf("runtime dir owner = %d, want %d", h.ownership.owner, h.uid)
	}
	if got := umCount(h, "chown "+h.username+": "+h.runtimeDir); got != 1 {
		t.Errorf("a converging path must not re-assert ownership, chown ran %d time(s):%s", got, h.callLog())
	}
}

// TestCondenseJournal pins the single-line/truncation contract of the excerpt
// embedded in error text and log fields.
func TestCondenseJournal(t *testing.T) {
	t.Run("folds_lines", func(t *testing.T) {
		got := condenseJournal("first  line\r\n\n   second\tline  \n")
		if strings.ContainsAny(got, "\n\r\t") {
			t.Errorf("condensed journal still contains control whitespace: %q", got)
		}
		want := "first line | second line"
		if got != want {
			t.Errorf("condenseJournal() = %q, want %q", got, want)
		}
	})

	t.Run("empty_input", func(t *testing.T) {
		if got := condenseJournal("\n\n  \n"); got != "" {
			t.Errorf("condenseJournal(blank) = %q, want empty", got)
		}
	})

	t.Run("truncates", func(t *testing.T) {
		got := condenseJournal(strings.Repeat("x", userManagerJournalMaxLen+50))
		if len(got) > userManagerJournalMaxLen+3 {
			t.Errorf("condensed journal is not bounded: %d bytes", len(got))
		}
		if !strings.HasSuffix(got, "...") {
			t.Errorf("truncated journal must advertise the cut: %q", got)
		}
	})

	t.Run("truncates_on_rune_boundary", func(t *testing.T) {
		// Every byte of a 3-byte rune straddles the budget boundary.
		got := condenseJournal(strings.Repeat("é", userManagerJournalMaxLen))
		if !strings.HasSuffix(got, "...") {
			t.Fatalf("expected a truncated excerpt, got %q", got)
		}
		trimmed := strings.TrimSuffix(got, "...")
		if !utf8.ValidString(trimmed) {
			t.Errorf("truncation split a UTF-8 rune: %q", trimmed)
		}
	})
}
