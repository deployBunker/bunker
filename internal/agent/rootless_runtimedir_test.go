package agent

import (
	"context"
	"errors"
	"fmt"
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
}

// probe mirrors probeRuntimeDirOnDisk for existence and type, and substitutes
// the simulated owner so the "stale directory owned by a previous user" case
// is reachable in a test.
func (o *userManagerOwnership) probe(path string) (runtimeDirInfo, error) {
	info, err := probeRuntimeDirOnDisk(path)
	if err != nil || !info.exists {
		return info, err
	}
	info.owner = o.owner
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
