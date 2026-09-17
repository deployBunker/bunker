package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ── INT-SPAWN-001: recycled-uid recovery + failed-spawn linger rollback ─────
//
// Defect A: the daemon allocated a uid RECYCLED from an agent destroyed
// minutes earlier. The destroyed agent's user manager was still running, so
// `systemctl is-active user@<uid>.service` said active while
// `su - <user> -c "systemctl --user daemon-reload"` failed — and
// classifyRuntimeDir cannot see it, because a recycled uid legitimately OWNS
// its runtime directory. The spawn died at stage rootless-install (tick 450 on
// bunker-mvp). proveUserManagerReachableWithRecovery must detect that exact
// fingerprint, tear the foreign state down in the INT-CI-008 state-consistent
// order, bring the manager back up and probe once more — while leaving the
// healthy path completely untouched.
//
// Defect B: the failed-spawn rollback went straight to `userdel -r`, which
// failed with "user ... is currently used by process 2168325" (the leftover
// manager) and left linger entries behind for users that no longer existed.
// removeAgentUser must un-linger and stop the uid's manager BEFORE userdel.
//
// Everything runs through the package seams: no root, no real systemd, no real
// users, no network.

const (
	rcTestUID      = 1002
	rcTestUsername = "bunker-recycled-alpha"

	// intSpawn001PresentUID is the uid presentUserStub (shared with the
	// destroy-linger tests) models; the rollback tests pin that coupling with a
	// premise assertion instead of hardcoding the number twice.
	intSpawn001PresentUID = 61001
)

// recycledUidHost is an in-memory systemd host for the recovery tests. It
// answers the root-side runner and the agent-session runner from scripted state
// and records the observed ORDER across both, so the ordering assertions are
// proven against one timeline (the fakeCtl/userManagerHost pattern).
//
// The manager's liveness is modelled by its bus socket on disk, exactly as the
// real host behaves: `systemctl start user@<uid>.service` publishes
// <runtimeDir>/bus and `systemctl is-active` reports active only while that
// socket exists. A test that wants the FOREIGN-manager fingerprint creates the
// socket itself (foreignManagerRunning) while the session probe is scripted to
// fail — the state of a recycled uid whose socket belongs to the previous
// owner.
type recycledUidHost struct {
	uid        int
	username   string
	runtimeDir string

	// sessionResults scripts each user-session daemon-reload by index (false =
	// the probe fails). Indices past the end succeed.
	sessionResults []bool

	// dirOwner is the uid owning the runtime directory as the probe reports it.
	dirOwner uint32

	calls      []string
	probeCalls int
}

func newRecycledUidHost(t *testing.T) *recycledUidHost {
	t.Helper()
	return &recycledUidHost{
		uid:        rcTestUID,
		username:   rcTestUsername,
		runtimeDir: filepath.Join(t.TempDir(), "run", "user", strconv.Itoa(rcTestUID)),
		dirOwner:   uint32(rcTestUID),
	}
}

// foreignManagerRunning materialises the previous owner's live manager: the
// unit is active and its bus socket occupies the uid's runtime directory, but
// the socket does not serve the new user (modelled by a failing session probe).
func (h *recycledUidHost) foreignManagerRunning(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(h.runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.runtimeDir, "bus"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// probe mirrors production ownership reporting: a recycled uid owns its runtime
// directory, which is exactly why classifyRuntimeDir reports it as fresh.
func (h *recycledUidHost) probe(path string) (runtimeDirInfo, error) {
	info, err := probeRuntimeDirOnDisk(path)
	if err != nil || !info.exists {
		return info, err
	}
	info.owner = h.dirOwner
	info.ownerKnown = true
	return info, nil
}

func (h *recycledUidHost) systemRunner(_ context.Context, name string, args ...string) ([]byte, error) {
	h.calls = append(h.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	switch name {
	case "chown":
		return nil, nil
	case "rm":
		if len(args) >= 1 {
			return nil, os.RemoveAll(args[len(args)-1])
		}
		return nil, nil
	case "journalctl":
		return []byte("journal: recycled uid excerpt\n"), nil
	case "loginctl":
		if len(args) == 0 {
			return nil, errors.New("loginctl: missing verb")
		}
		switch args[0] {
		case "enable-linger", "terminate-user", "disable-linger":
			return nil, nil
		}
		return nil, fmt.Errorf("recycledUidHost: unexpected loginctl verb %q", args[0])
	case "systemctl":
		if len(args) == 0 {
			return nil, errors.New("systemctl: missing verb")
		}
		unit := ""
		if len(args) > 1 {
			unit = args[len(args)-1]
		}
		switch args[0] {
		case "is-active":
			if unit == userManagerUnitName(h.uid) {
				if _, err := os.Stat(filepath.Join(h.runtimeDir, "bus")); err == nil {
					return []byte("active\n"), nil
				}
				return []byte("inactive\n"), errors.New("exit status 3")
			}
			return []byte("inactive\n"), errors.New("exit status 3")
		case "show":
			return []byte("exit-code\n"), nil
		case "start":
			if unit == userManagerUnitName(h.uid) {
				// A started manager publishes its bus socket in the runtime
				// directory, which the runtime-dir unit has already created.
				if err := os.WriteFile(filepath.Join(h.runtimeDir, "bus"), nil, 0o600); err != nil {
					return nil, err
				}
			}
			return nil, nil
		case "stop", "reset-failed":
			return nil, nil
		case "status":
			return nil, errors.New("exit status 3")
		}
		return nil, fmt.Errorf("recycledUidHost: unexpected systemctl verb %q", args[0])
	}
	return nil, fmt.Errorf("recycledUidHost: unexpected command %q", name)
}

func (h *recycledUidHost) sessionRunner(_ context.Context, username, runtimeDir, script string) ([]byte, error) {
	if username != h.username {
		return nil, fmt.Errorf("recycledUidHost: session runner got user %q", username)
	}
	if runtimeDir != h.runtimeDir {
		return nil, fmt.Errorf("recycledUidHost: session runner got runtime dir %q, want %q", runtimeDir, h.runtimeDir)
	}
	idx := h.probeCalls
	h.probeCalls++
	h.calls = append(h.calls, "user-session["+script+"]")
	ok := true
	if idx < len(h.sessionResults) {
		ok = h.sessionResults[idx]
	}
	if !ok {
		// The fingerprint of an unreachable manager: the session cannot talk
		// to the live (foreign) manager.
		return []byte("Failed to connect to bus: Permission denied"), errors.New("exit status 1")
	}
	return nil, nil
}

// install swaps every seam the probe/recovery/bring-up path touches and
// restores them on cleanup (tests in this package share one process, so a
// leaked fake would poison every later test).
func (h *recycledUidHost) install(t *testing.T, lingerDirPath string) {
	t.Helper()
	prevRunner := userManagerRunner
	prevSession := userSessionRunner
	prevProbe := runtimeDirProbe
	prevLinger := lingerDir
	prevPoll := userManagerPollInterval
	prevTimeout := userManagerWaitTimeoutOverride

	userManagerRunner = h.systemRunner
	userSessionRunner = h.sessionRunner
	runtimeDirProbe = h.probe
	lingerDir = lingerDirPath
	userManagerPollInterval = time.Millisecond
	userManagerWaitTimeoutOverride = 2 * time.Second

	t.Cleanup(func() {
		userManagerRunner = prevRunner
		userSessionRunner = prevSession
		runtimeDirProbe = prevProbe
		lingerDir = prevLinger
		userManagerPollInterval = prevPoll
		userManagerWaitTimeoutOverride = prevTimeout
	})
}

func (h *recycledUidHost) callLog() string { return "\n  " + strings.Join(h.calls, "\n  ") }

// nextIndexOf returns the position of the first call at or after `from` whose
// label starts with prefix.
func (h *recycledUidHost) nextIndexOf(prefix string, from int) int {
	for i := from; i < len(h.calls); i++ {
		if strings.HasPrefix(h.calls[i], prefix) {
			return i
		}
	}
	return -1
}

// countPrefix counts calls whose label starts with prefix.
func (h *recycledUidHost) countPrefix(prefix string) int {
	n := 0
	for _, c := range h.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// sessionProbeLabel is the exact user-session daemon-reload label.
func sessionProbeLabel() string { return "user-session[" + userManagerReloadCmd + "]" }

// warnLinesWithMarker returns the WARN log lines that carry marker, so the
// "exactly ONE WARN" requirement is asserted on real log records instead of on
// prose.
func warnLinesWithMarker(log, marker string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, marker) {
			out = append(out, line)
		}
	}
	return out
}

// ── Change A: the one-shot recovery ────────────────────────────────────────

// TestProveUserManagerReachableWithRecovery_HealthyPathIsNonDestructive is the
// INT-CI-008 regression guard: a probe that succeeds must issue NOTHING but the
// probe — no fingerprint query, no stop, no terminate-user, no rm, no log line.
func TestProveUserManagerReachableWithRecovery_HealthyPathIsNonDestructive(t *testing.T) {
	h := newRecycledUidHost(t)
	h.sessionResults = []bool{true}
	h.install(t, t.TempDir())

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if err := proveUserManagerReachableWithRecovery(context.Background(), h.username, h.uid, h.runtimeDir, logger); err != nil {
		t.Fatalf("healthy probe must return nil, got: %v%s", err, h.callLog())
	}
	if len(h.calls) != 1 || h.calls[0] != sessionProbeLabel() {
		t.Errorf("healthy path must issue exactly the probe and nothing else, got:%s", h.callLog())
	}
	for _, forbidden := range []string{
		"systemctl stop", "loginctl terminate-user", "rm ", "rm -rf",
		"systemctl is-active", "loginctl enable-linger", "systemctl start",
	} {
		if got := h.countPrefix(forbidden); got != 0 {
			t.Errorf("healthy path issued %q %d time(s):%s", forbidden, got, h.callLog())
		}
	}
	if buf.Len() != 0 {
		t.Errorf("healthy path must not log anything, got:\n%s", buf.String())
	}
}

// TestProveUserManagerReachableWithRecovery_RecycledUidIsRecoveredOnce proves
// the fingerprint match, the INT-CI-008 teardown order, the documented bring-up
// order, the single WARN and the two-probe bound.
func TestProveUserManagerReachableWithRecovery_RecycledUidIsRecoveredOnce(t *testing.T) {
	h := newRecycledUidHost(t)
	h.foreignManagerRunning(t)
	h.sessionResults = []bool{false, true} // unreachable, then reachable again
	h.install(t, lingerDirWithEntries(t, 3))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if err := proveUserManagerReachableWithRecovery(context.Background(), h.username, h.uid, h.runtimeDir, logger); err != nil {
		t.Fatalf("the one-shot recovery must bring the manager back, got: %v%s", err, h.callLog())
	}

	// Every reset action ran exactly once — the state-consistent teardown.
	for _, action := range []string{
		"systemctl stop " + userManagerUnitName(h.uid),
		"loginctl terminate-user " + strconv.Itoa(h.uid),
		"systemctl stop " + userRuntimeDirUnitName(h.uid),
		"rm -rf " + h.runtimeDir,
	} {
		if got := h.countPrefix(action); got != 1 {
			t.Errorf("reset action %q ran %d time(s), want exactly 1:%s", action, got, h.callLog())
		}
	}

	// Exactly two session probes: the failing probe and the FINAL re-probe.
	if got := h.countPrefix("user-session["); got != 2 {
		t.Errorf("expected exactly 2 session probes, got %d:%s", got, h.callLog())
	}

	// Exactly ONE WARN carrying the recovery marker, naming the user, uid and unit.
	warns := warnLinesWithMarker(buf.String(), recycledUIDRecoveryMarker)
	if len(warns) != 1 {
		t.Fatalf("expected exactly 1 WARN carrying the recovery marker, got %d:\n%s", len(warns), buf.String())
	}
	for _, want := range []string{h.username, strconv.Itoa(h.uid), userManagerUnitName(h.uid)} {
		if !strings.Contains(warns[0], want) {
			t.Errorf("recovery WARN does not name %q:\n%s", want, warns[0])
		}
	}

	// Documented order: probe → teardown (manager, session, runtime-dir unit,
	// removal) → directory → linger → manager start → final probe.
	order := []string{
		sessionProbeLabel(),
		"systemctl stop " + userManagerUnitName(h.uid),
		"loginctl terminate-user " + strconv.Itoa(h.uid),
		"systemctl stop " + userRuntimeDirUnitName(h.uid),
		"rm -rf " + h.runtimeDir,
		"systemctl start " + userRuntimeDirUnitName(h.uid),
		"loginctl enable-linger " + h.username,
		"systemctl start " + userManagerUnitName(h.uid),
	}
	prev := -1
	for _, cmd := range order {
		idx := h.nextIndexOf(cmd, prev+1)
		if idx < 0 {
			t.Fatalf("recovery sequence missing %q after call %d:%s", cmd, prev, h.callLog())
		}
		prev = idx
	}
	if second := h.nextIndexOf(sessionProbeLabel(), prev+1); second < 0 {
		t.Errorf("the final re-probe must run AFTER the manager came back up:%s", h.callLog())
	}
}

// TestProveUserManagerReachableWithRecovery_InactiveUnitIsNotRecovered pins the
// non-fingerprint branch: a manager that is NOT active has no foreign state to
// tear down, so the existing attribution error is returned unchanged and no
// destructive action is taken.
func TestProveUserManagerReachableWithRecovery_InactiveUnitIsNotRecovered(t *testing.T) {
	h := newRecycledUidHost(t)
	// No bus socket: the manager is genuinely down (not the recycled-uid case).
	h.sessionResults = []bool{false}
	h.install(t, lingerDirWithEntries(t, 3))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	err := proveUserManagerReachableWithRecovery(context.Background(), h.username, h.uid, h.runtimeDir, logger)
	if err == nil {
		t.Fatalf("expected the attribution error%s", h.callLog())
	}
	msg := err.Error()
	for _, want := range []string{
		"rootless-install",
		userManagerUnitName(h.uid),
		"is-active=inactive",
		"linger entries: 3",
		"journal:",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("attribution error missing %q, got: %s", want, msg)
		}
	}
	for _, forbidden := range []string{
		"systemctl stop", "loginctl terminate-user", "rm -rf", "systemctl start", "loginctl enable-linger",
	} {
		if got := h.countPrefix(forbidden); got != 0 {
			t.Errorf("no recovery may run for an inactive unit, but %q ran %d time(s):%s", forbidden, got, h.callLog())
		}
	}
	if strings.Contains(msg, recycledUIDRecoveryMarker) {
		t.Errorf("no recovery was attempted, so the error must not advertise one: %s", msg)
	}
	if got := len(warnLinesWithMarker(buf.String(), recycledUIDRecoveryMarker)); got != 0 {
		t.Errorf("no recovery was attempted, so no recovery WARN may be logged, got %d:\n%s", got, buf.String())
	}
	if got := h.countPrefix("user-session["); got != 1 {
		t.Errorf("expected exactly 1 probe (no retry) for the inactive unit, got %d:%s", got, h.callLog())
	}
}

// TestProveUserManagerReachableWithRecovery_BothProbesFailIsBounded proves the
// one-shot bound: the recovery runs exactly once, the probe exactly twice, and
// the error returned is the existing attribution error extended with the fact
// that a recovery was attempted.
func TestProveUserManagerReachableWithRecovery_BothProbesFailIsBounded(t *testing.T) {
	h := newRecycledUidHost(t)
	h.foreignManagerRunning(t)
	h.sessionResults = []bool{false, false} // still unreachable after the recovery
	h.install(t, lingerDirWithEntries(t, 1))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	err := proveUserManagerReachableWithRecovery(context.Background(), h.username, h.uid, h.runtimeDir, logger)
	if err == nil {
		t.Fatalf("expected the second probe failure to surface%s", h.callLog())
	}
	msg := err.Error()
	for _, want := range []string{
		recycledUIDRecoveryMarker,
		"rootless-install",
		userManagerUnitName(h.uid),
		"is-active=",
		"linger entries: 1",
		"journal:",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("bounded-recovery error missing %q, got: %s", want, msg)
		}
	}

	// Exactly one recovery: each teardown action once, no loop.
	for _, action := range []string{
		"systemctl stop " + userManagerUnitName(h.uid),
		"loginctl terminate-user " + strconv.Itoa(h.uid),
		"systemctl stop " + userRuntimeDirUnitName(h.uid),
		"rm -rf " + h.runtimeDir,
	} {
		if got := h.countPrefix(action); got != 1 {
			t.Errorf("recovery action %q ran %d time(s), want exactly 1 (no loop):%s", action, got, h.callLog())
		}
	}
	if got := h.countPrefix("user-session["); got != 2 {
		t.Errorf("expected exactly 2 probes total, got %d:%s", got, h.callLog())
	}
	if got := len(warnLinesWithMarker(buf.String(), recycledUIDRecoveryMarker)); got != 1 {
		t.Errorf("expected exactly 1 recovery WARN, got %d:\n%s", got, buf.String())
	}
}

// TestProveUserManagerReachableWithRecovery_CancelledContextIsPrompt proves a
// caller that already gave up never has a teardown started on its behalf: the
// cancellation surfaces unchanged and no destructive command is issued.
func TestProveUserManagerReachableWithRecovery_CancelledContextIsPrompt(t *testing.T) {
	h := newRecycledUidHost(t)
	h.foreignManagerRunning(t)
	h.sessionResults = []bool{false}
	h.install(t, t.TempDir())

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := proveUserManagerReachableWithRecovery(ctx, h.username, h.uid, h.runtimeDir, logger)
	if err != context.Canceled {
		t.Fatalf("expected exactly context.Canceled, got %T: %v", err, err)
	}
	for _, forbidden := range []string{
		"systemctl stop", "loginctl terminate-user", "rm -rf", "systemctl start", "loginctl enable-linger",
	} {
		if got := h.countPrefix(forbidden); got != 0 {
			t.Errorf("a cancelled caller must not trigger a teardown, but %q ran %d time(s):%s", forbidden, got, h.callLog())
		}
	}
	if got := len(warnLinesWithMarker(buf.String(), recycledUIDRecoveryMarker)); got != 0 {
		t.Errorf("no recovery may start on a cancelled context, got %d recovery WARN(s):\n%s", got, buf.String())
	}
}

// ── Change B: the failed-spawn rollback ────────────────────────────────────

// intSpawn001CommandLog appends one command line to path, so commands issued
// through Go-side seams and commands issued by PATH stubs land in ONE log and
// the assertion reads true execution order (the technique
// TestDestroy_DisablesLingerBeforeUserdel already uses).
func intSpawn001CommandLog(t *testing.T, path string) func(string) {
	t.Helper()
	return func(line string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("open command log: %v", err)
		}
		defer f.Close()
		if _, err := fmt.Fprintln(f, line); err != nil {
			t.Fatalf("append command log: %v", err)
		}
	}
}

// lineIndex returns the index of the first log line with the given prefix, or -1.
func lineIndex(lines []string, prefix string) int {
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

// recorded reports whether res recorded the action as run (ok) or failed (err).
func recorded(t *testing.T, res *rollbackResult, want string, failed bool) bool {
	t.Helper()
	ran, failedActions := res.snapshot()
	haystack := ran
	if failed {
		haystack = failedActions
	}
	for _, a := range haystack {
		if strings.HasPrefix(a, want) {
			return true
		}
	}
	return false
}

// TestRemoveAgentUser_ClearsLingerAndManagerBeforeUserdel is the ordering proof
// for the defect-B fix: `loginctl disable-linger <user>` and the uid's manager
// stop both run BEFORE userdel -r, and both outcomes are recorded.
func TestRemoveAgentUser_ClearsLingerAndManagerBeforeUserdel(t *testing.T) {
	const agentID = "intspawn001-clear"
	username := "bunker-" + agentID
	unit := userManagerUnitName(intSpawn001PresentUID)
	uidArg := strconv.Itoa(intSpawn001PresentUID)

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "cmds.log")
	record := intSpawn001CommandLog(t, logPath)

	// userdel succeeds; pkill/pgrep are stubbed so the retry path can never
	// reach the real host.
	writeStub(t, binDir, "userdel", "printf '%s\\n' \"userdel $*\" >> \""+logPath+"\"\nexit 0\n")
	writeStub(t, binDir, "pkill", stubSucceeds)
	writeStub(t, binDir, "pgrep", "exit 1\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stubLookupUser(t, presentUserStub(username))
	// Premise: the shared helper really models the uid this test asserts on.
	if u, err := lookupUser(username); err != nil || u.Uid != uidArg {
		t.Fatalf("test premise broken: lookupUser(%q) = %+v, %v", username, u, err)
	}

	stubDisableLinger(t, func(_ context.Context, name string) ([]byte, error) {
		record("loginctl disable-linger " + name)
		return nil, nil
	})
	restoreRunner := userManagerRunner
	userManagerRunner = func(_ context.Context, name string, args ...string) ([]byte, error) {
		record(strings.TrimSpace(name + " " + strings.Join(args, " ")))
		return nil, nil
	}
	t.Cleanup(func() { userManagerRunner = restoreRunner })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	res := &rollbackResult{}
	rollbackCtx, cancel := rollbackContext(context.Background())
	defer cancel()
	removeAgentUser(rollbackCtx, agentID, logger, res)

	lines := readRecord(t, logPath)
	idxDisable := lineIndex(lines, "loginctl disable-linger "+username)
	idxStop := lineIndex(lines, "systemctl stop "+unit)
	idxTerminate := lineIndex(lines, "loginctl terminate-user "+uidArg)
	idxUserdel := lineIndex(lines, "userdel -r "+username)

	if idxDisable < 0 {
		t.Errorf("rollback never ran `loginctl disable-linger %s`; log: %v", username, lines)
	}
	if idxStop < 0 {
		t.Errorf("rollback never stopped %s; log: %v", unit, lines)
	}
	if idxTerminate < 0 {
		t.Errorf("rollback never terminated the session of uid %s; log: %v", uidArg, lines)
	}
	if idxUserdel < 0 {
		t.Errorf("rollback never reached userdel; log: %v", lines)
	}
	if idxDisable >= 0 && idxUserdel >= 0 && idxDisable > idxUserdel {
		t.Errorf("disable-linger ran AFTER userdel (log %d vs %d): %v", idxDisable, idxUserdel, lines)
	}
	if idxStop >= 0 && idxUserdel >= 0 && idxStop > idxUserdel {
		t.Errorf("the manager stop ran AFTER userdel (log %d vs %d): %v", idxStop, idxUserdel, lines)
	}
	if idxTerminate >= 0 && idxUserdel >= 0 && idxTerminate > idxUserdel {
		t.Errorf("terminate-user ran AFTER userdel (log %d vs %d): %v", idxTerminate, idxUserdel, lines)
	}

	for _, want := range []string{
		"loginctl disable-linger " + username,
		"systemctl stop " + unit,
		"loginctl terminate-user " + uidArg,
		"userdel " + username,
	} {
		if !recorded(t, res, want, false) {
			t.Errorf("rollbackResult did not record %q as run", want)
		}
	}
	if _, failed := res.snapshot(); len(failed) != 0 {
		t.Errorf("clean rollback recorded failures: %v", failed)
	}
}

// TestRemoveAgentUser_DisableLingerFailureStillRunsUserdel pins the
// best-effort contract: a failing loginctl is recorded and logged, but the
// rollback continues into the manager stop and userdel exactly as before.
func TestRemoveAgentUser_DisableLingerFailureStillRunsUserdel(t *testing.T) {
	const agentID = "intspawn001-lingerfail"
	username := "bunker-" + agentID
	unit := userManagerUnitName(intSpawn001PresentUID)

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "userdel.log")
	writeStub(t, binDir, "userdel", "printf '%s\\n' \"userdel $*\" >> \""+logPath+"\"\nexit 0\n")
	writeStub(t, binDir, "pkill", stubSucceeds)
	writeStub(t, binDir, "pgrep", "exit 1\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stubLookupUser(t, presentUserStub(username))
	stubDisableLinger(t, func(context.Context, string) ([]byte, error) {
		return []byte("Failed to disable linger: Permission denied"), fmt.Errorf("exit status 1")
	})
	stoppedUnits := &[]string{}
	restoreRunner := userManagerRunner
	userManagerRunner = func(_ context.Context, name string, args ...string) ([]byte, error) {
		*stoppedUnits = append(*stoppedUnits, strings.TrimSpace(name+" "+strings.Join(args, " ")))
		return nil, nil
	}
	t.Cleanup(func() { userManagerRunner = restoreRunner })

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	res := &rollbackResult{}
	rollbackCtx, cancel := rollbackContext(context.Background())
	defer cancel()
	removeAgentUser(rollbackCtx, agentID, logger, res)

	if calls := readRecord(t, logPath); len(calls) != 1 {
		t.Errorf("userdel must still run after a failing disable-linger, calls: %v", calls)
	}
	if !recorded(t, res, "loginctl disable-linger "+username, true) {
		t.Errorf("the failing disable-linger was not recorded as failed: %+v", res)
	}
	if !recorded(t, res, "userdel "+username, false) {
		t.Errorf("the successful userdel was not recorded as run: %+v", res)
	}
	// The manager stop is not skipped because linger failed.
	sawStop := false
	for _, c := range *stoppedUnits {
		if strings.HasPrefix(c, "systemctl stop "+unit) {
			sawStop = true
		}
	}
	if !sawStop {
		t.Errorf("a failing disable-linger must not skip the manager stop, got: %v", *stoppedUnits)
	}
	logged := buf.String()
	if !strings.Contains(logged, "loginctl disable-linger failed during spawn rollback") {
		t.Errorf("the failure must be on the record; log:\n%s", logged)
	}
	if !strings.Contains(logged, "Permission denied") {
		t.Errorf("the raw loginctl output must be preserved; log:\n%s", logged)
	}
}

// TestRemoveAgentUser_AbsentUserSkipsManagerCleanup pins the best-effort
// resolution: a user that no longer resolves has no uid whose manager could
// hold it busy, so the rollback goes straight to the existing userdel path.
func TestRemoveAgentUser_AbsentUserSkipsManagerCleanup(t *testing.T) {
	const agentID = "intspawn001-absent"
	username := "bunker-" + agentID

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "userdel.log")
	writeStub(t, binDir, "userdel", "printf '%s\\n' \"userdel $*\" >> \""+logPath+"\"\nexit 0\n")
	writeStub(t, binDir, "pkill", stubSucceeds)
	writeStub(t, binDir, "pgrep", "exit 1\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stubLookupUser(t, func(name string) (*user.User, error) {
		return nil, user.UnknownUserError(name)
	})
	stubDisableLinger(t, func(context.Context, string) ([]byte, error) {
		t.Error("disableLinger must not run when the user does not resolve")
		return nil, nil
	})
	restoreRunner := userManagerRunner
	userManagerRunner = func(_ context.Context, name string, args ...string) ([]byte, error) {
		t.Errorf("the user manager must not be stopped when the user does not resolve: %s %v", name, args)
		return nil, nil
	}
	t.Cleanup(func() { userManagerRunner = restoreRunner })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	res := &rollbackResult{}
	rollbackCtx, cancel := rollbackContext(context.Background())
	defer cancel()
	removeAgentUser(rollbackCtx, agentID, logger, res)

	if calls := readRecord(t, logPath); len(calls) != 1 {
		t.Errorf("the existing userdel path must still run, calls: %v", calls)
	}
	if !recorded(t, res, "userdel "+username, false) {
		t.Errorf("userdel outcome was not recorded: %+v", res)
	}
	if recorded(t, res, "loginctl disable-linger", false) || recorded(t, res, "loginctl disable-linger", true) {
		t.Errorf("no linger action may be recorded for an absent user: %+v", res)
	}
}
