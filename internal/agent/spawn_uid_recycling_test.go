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

	// rcForeignRecordName is the account logind has the uid's record under when
	// the uid was recycled from a destroyed agent: a name that is NOT the user
	// being brought up and whose account no longer exists.
	rcForeignRecordName = "bunker-recycled-ghost"

	// rcProbeFailureText is what the fake agent session prints when it cannot
	// reach the manager. It is the probe's OWN output — the text that names the
	// cause while systemctl's Result only names the symptom.
	rcProbeFailureText = "Failed to connect to bus: Permission denied"

	// intSpawn001PresentUID is the uid presentUserStub (shared with the
	// destroy-linger tests) models; the rollback tests pin that coupling with a
	// premise assertion instead of hardcoding the number twice.
	intSpawn001PresentUID = 61001
)

// logindRecordStub is one scripted `loginctl show-user` answer: the uid's
// logind record as the host would report it. A nil entry in
// recycledUidHost.records means "the uid has no logind record at all" (the
// query fails, exactly as loginctl does for a uid it does not know).
type logindRecordStub struct {
	name   string
	linger string
	state  string
}

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
	// the probe fails). Indices past the end succeed. Ignored when
	// sessionBusAnswers or foreignServedAfterTeardown is set.
	sessionResults []bool

	// sessionBusAnswers scripts the probe decision by probe INDEX (true = the
	// manager answers). The readiness gate retries on its OWN budget, so the
	// number of probes it issues is not knowable from an index-list script; a
	// per-index function is what lets a test state "refused N times, then the
	// bus answers" (INT-SPAWN-003).
	sessionBusAnswers func(probeIndex int) bool

	// sessionFailureText overrides the probe's own output per index, so a test
	// can prove the attribution carries the LAST probe's stderr rather than,
	// say, the first one's.
	sessionFailureText func(probeIndex int) []byte

	// sessionHonorsContext models the production runner (`su` under
	// exec.CommandContext): a probe handed an already-DONE context fails with
	// that context error instead of reaching the bus. It is what makes the
	// detached-budget discipline observable — a gate that kept handing the
	// caller's expired context to the probe could never succeed.
	sessionHonorsContext bool

	// foreignServedAfterTeardown models the genuine recycled-uid fingerprint:
	// the socket in the runtime dir belongs to the previous owner and never
	// serves the new user until the recovery removes that foreign state (the
	// manager stop or the runtime-dir removal), after which the newly
	// brought-up manager answers. State-driven on purpose: the readiness gate's
	// probe count depends on its budget, so a per-index script cannot describe
	// "unreachable until the teardown".
	foreignServedAfterTeardown bool
	// foreignTornDown is set by the teardown actions the host observes.
	foreignTornDown bool

	// doneCtxProbes counts the probes that were handed an already-done
	// context. It must stay 0: the gate detaches instead of probing on a dead
	// caller context (INT-CI-007).
	doneCtxProbes int

	// waitBudget overrides the package readiness/bring-up budget for this host
	// when non-zero. A test that exercises the gate's never-answers path must
	// shrink it, because that path polls until the budget is exhausted.
	waitBudget time.Duration

	// dirOwner is the uid owning the runtime directory as the probe reports it.
	dirOwner uint32

	// records scripts the answers of `loginctl show-user <uid>` by index. A nil
	// entry means "the uid has no logind record" (the query fails, as loginctl
	// does for a uid it does not know). Indices past the end repeat the last
	// entry, so a one-element list describes a record that does not change.
	records     []*logindRecordStub
	recordCalls int

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

// showUserRecord answers `loginctl show-user <uid> --property=Name,Linger,State`
// from the scripted records. No record (or a nil entry) answers the way loginctl
// answers a uid it does not know: a failure with "No such user" on stderr —
// which the production query must treat as "no evidence", never as "foreign".
func (h *recycledUidHost) showUserRecord() ([]byte, error) {
	if len(h.records) == 0 {
		return []byte("Failed to get user: No such user\n"), errors.New("exit status 1")
	}
	idx := h.recordCalls
	h.recordCalls++
	if idx >= len(h.records) {
		idx = len(h.records) - 1
	}
	rec := h.records[idx]
	if rec == nil {
		return []byte("Failed to get user: No such user\n"), errors.New("exit status 1")
	}
	return []byte(fmt.Sprintf("Name=%s\nLinger=%s\nState=%s\n", rec.name, rec.linger, rec.state)), nil
}

func (h *recycledUidHost) systemRunner(_ context.Context, name string, args ...string) ([]byte, error) {
	h.calls = append(h.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	switch name {
	case "chown":
		return nil, nil
	case "rm":
		if len(args) >= 1 {
			// The runtime-dir removal takes the foreign manager's socket with
			// it: the foreign state is gone (see foreignServedAfterTeardown).
			h.foreignTornDown = true
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
		case "show-user":
			return h.showUserRecord()
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
		case "stop":
			if unit == userManagerUnitName(h.uid) {
				// Stopping the manager ends the previous owner's bus (see
				// foreignServedAfterTeardown).
				h.foreignTornDown = true
			}
			return nil, nil
		case "reset-failed":
			return nil, nil
		case "status":
			return nil, errors.New("exit status 3")
		}
		return nil, fmt.Errorf("recycledUidHost: unexpected systemctl verb %q", args[0])
	}
	return nil, fmt.Errorf("recycledUidHost: unexpected command %q", name)
}

func (h *recycledUidHost) sessionRunner(ctx context.Context, username, runtimeDir, script string) ([]byte, error) {
	if username != h.username {
		return nil, fmt.Errorf("recycledUidHost: session runner got user %q", username)
	}
	if runtimeDir != h.runtimeDir {
		return nil, fmt.Errorf("recycledUidHost: session runner got runtime dir %q, want %q", runtimeDir, h.runtimeDir)
	}
	idx := h.probeCalls
	h.probeCalls++
	h.calls = append(h.calls, "user-session["+script+"]")
	if ctx.Err() != nil {
		h.doneCtxProbes++
		if h.sessionHonorsContext {
			// The production runner is `su` under exec.CommandContext: a done
			// context kills the command before it can talk to the bus.
			return []byte(ctx.Err().Error()), ctx.Err()
		}
	}
	ok := true
	switch {
	case h.foreignServedAfterTeardown:
		ok = h.foreignTornDown
	case h.sessionBusAnswers != nil:
		ok = h.sessionBusAnswers(idx)
	case idx < len(h.sessionResults):
		ok = h.sessionResults[idx]
	}
	if !ok {
		// The fingerprint of an unreachable manager: the session cannot talk
		// to the live (foreign) manager. The probe's OWN output (which the
		// attribution must carry) is this text.
		return h.probeFailureText(idx), errors.New("exit status 1")
	}
	return nil, nil
}

// probeFailureText is the failed probe's own output for probe index idx.
func (h *recycledUidHost) probeFailureText(idx int) []byte {
	if h.sessionFailureText != nil {
		return h.sessionFailureText(idx)
	}
	return []byte(rcProbeFailureText)
}

// install swaps every seam the probe/recovery/bring-up path touches and
// restores them on cleanup (tests in this package share one process, so a
// leaked fake would poison every later test).
func (h *recycledUidHost) install(t *testing.T, lingerDirPath string) {
	t.Helper()
	prevRunner := userManagerRunner
	prevSession := userSessionRunner
	prevProbe := runtimeDirProbe
	prevLookup := userLookup
	prevLinger := lingerDir
	prevPoll := userManagerPollInterval
	prevTimeout := userManagerWaitTimeoutOverride

	userManagerRunner = h.systemRunner
	userSessionRunner = h.sessionRunner
	runtimeDirProbe = h.probe
	// The passwd database the fingerprint cross-checks against: the user being
	// brought up resolves, the uid's foreign (destroyed) owner does not.
	userLookup = func(name string) (*user.User, error) {
		if name == h.username {
			return &user.User{Username: name, Uid: strconv.Itoa(h.uid)}, nil
		}
		return nil, user.UnknownUserError(name)
	}
	lingerDir = lingerDirPath
	userManagerPollInterval = time.Millisecond
	userManagerWaitTimeoutOverride = 2 * time.Second
	if h.waitBudget > 0 {
		// The readiness gate polls until its budget is exhausted, so the
		// never-answers cases must shrink it (INT-SPAWN-003).
		userManagerWaitTimeoutOverride = h.waitBudget
	}

	t.Cleanup(func() {
		userManagerRunner = prevRunner
		userSessionRunner = prevSession
		runtimeDirProbe = prevProbe
		userLookup = prevLookup
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

// countPrefixBefore counts calls with the prefix that appear strictly BEFORE
// index `to`, so a test can split one call log around a teardown or a bring-up.
func (h *recycledUidHost) countPrefixBefore(prefix string, to int) int {
	n := 0
	for i, c := range h.calls {
		if i >= to {
			break
		}
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// countPrefixAfter counts calls with the prefix that appear strictly AFTER
// index `from`.
func (h *recycledUidHost) countPrefixAfter(prefix string, from int) int {
	n := 0
	for i, c := range h.calls {
		if i > from && strings.HasPrefix(c, prefix) {
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
	// The foreign manager's socket never serves the new user: the probe is
	// refused for the WHOLE readiness budget (INT-SPAWN-003) and only answers
	// once the recovery has removed that foreign state.
	h.foreignServedAfterTeardown = true
	h.waitBudget = 30 * time.Millisecond
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

	// The readiness gate retried on its own budget before the recovery fired
	// (INT-SPAWN-003: one refused probe is a NOT-READY condition, not the final
	// attribution), and the FINAL readiness wait answered on its FIRST probe
	// once the manager was back up.
	stopIdx := h.nextIndexOf("systemctl stop "+userManagerUnitName(h.uid), 0)
	if stopIdx < 0 {
		t.Fatalf("the recovery never stopped the manager:%s", h.callLog())
	}
	if got := h.countPrefixBefore("user-session[", stopIdx); got < 2 {
		t.Errorf("the readiness gate must retry before the recovery fires, got %d probe(s):%s", got, h.callLog())
	}
	startIdx := h.nextIndexOf("systemctl start "+userManagerUnitName(h.uid), 0)
	if startIdx < 0 {
		t.Fatalf("the recovery never brought the manager back up:%s", h.callLog())
	}
	if got := h.countPrefixAfter("user-session[", startIdx); got != 1 {
		t.Errorf("the final readiness wait must answer on its first probe, got %d probe(s) after the bring-up:%s", got, h.callLog())
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
	// The bus never answers, so the readiness gate spends its budget.
	h.sessionBusAnswers = func(int) bool { return false }
	h.waitBudget = 20 * time.Millisecond
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
	if got := h.countPrefix("user-session["); got < 2 {
		t.Errorf("the readiness gate must retry for a not-yet-answering bus (INT-SPAWN-003), got %d probe(s):%s", got, h.callLog())
	}
}

// TestProveUserManagerReachableWithRecovery_BothProbesFailIsBounded proves the
// one-shot bound: the recovery runs exactly once, both readiness waits retry
// and then exhaust their own budget, and the error returned is the existing
// attribution error extended with the fact that a recovery was attempted.
func TestProveUserManagerReachableWithRecovery_BothProbesFailIsBounded(t *testing.T) {
	h := newRecycledUidHost(t)
	h.foreignManagerRunning(t)
	h.sessionBusAnswers = func(int) bool { return false } // still unreachable after the recovery
	h.waitBudget = 20 * time.Millisecond
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
		// The final probe's own output: without it the operator sees a
		// manager that "is active" while the session cannot reach it — the
		// exact reading that hid the tick-452 defect.
		rcProbeFailureText,
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
	if got := h.countPrefix("user-session["); got < 2 {
		t.Errorf("expected the readiness gate to retry (never a single-probe verdict), got %d probe(s):%s", got, h.callLog())
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
	// A pre-cancelled caller: the gate must return the cancellation before any
	// command at all, so the never-answering bus script never matters.
	h.sessionBusAnswers = func(int) bool { return false }
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

// ── INT-SPAWN-002: the stale logind record + probe-output attribution ───────
//
// The recycled-uid recovery of INT-SPAWN-001 FIRED but recovered nothing on
// tick 452: after its teardown and bring-up the session still could not reach
// user@1002.service and the spawn died at stage rootless-install. What it could
// not remove is logind's per-UID user record — a name belonging to a DESTROYED
// account, Linger=yes — which makes logind restart the manager the moment the
// reset stops it: the stop is undone, `systemctl is-active` is "active" again,
// ensureUserManagerRunning returns nil immediately, and the final probe fails
// again (the same linger flag also makes the bring-up's own enable-linger a
// no-op). These tests pin both halves of the fix: the recovery clears that
// record before the teardown, in order and exactly once, and the WARNs/errors
// carry the probe's own output plus the logind-respawn event so the next
// occurrence is diagnosable from the log alone.

// TestProveUserManagerReachable_ProbeOutputIsOnTheRecord covers the
// diagnosability half: the probe's own output (which names the cause while
// `systemctl is-active`/`Result` only name the symptom) reaches the WARN and
// the attribution error, and the healthy path stays one seam call with no log
// line at all.
func TestProveUserManagerReachable_ProbeOutputIsOnTheRecord(t *testing.T) {
	t.Run("failed_probe_carries_its_own_output_into_warn_and_error", func(t *testing.T) {
		h := newRecycledUidHost(t)
		h.sessionResults = []bool{false}
		h.install(t, lingerDirWithEntries(t, 2))

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		err := proveUserManagerReachable(context.Background(), h.username, h.uid, h.runtimeDir, logger)
		if err == nil {
			t.Fatalf("expected the probe failure to surface%s", h.callLog())
		}
		if !strings.Contains(err.Error(), rcProbeFailureText) {
			t.Errorf("the attribution error must carry the probe's own output %q, got: %s", rcProbeFailureText, err)
		}
		warns := warnLinesWithMarker(buf.String(), "agent user manager unreachable from the agent session")
		if len(warns) != 1 {
			t.Fatalf("expected exactly 1 unreachable WARN, got %d:\n%s", len(warns), buf.String())
		}
		if !strings.Contains(warns[0], rcProbeFailureText) {
			t.Errorf("the WARN must carry the probe's own output %q:\n%s", rcProbeFailureText, warns[0])
		}
		if got := h.countPrefix("user-session["); got != 1 {
			t.Errorf("the failure path must still probe exactly once, got %d:%s", got, h.callLog())
		}
	})

	t.Run("healthy_probe_probes_once_and_logs_nothing", func(t *testing.T) {
		h := newRecycledUidHost(t)
		h.sessionResults = []bool{true}
		h.install(t, lingerDirWithEntries(t, 2))

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		if err := proveUserManagerReachable(context.Background(), h.username, h.uid, h.runtimeDir, logger); err != nil {
			t.Fatalf("healthy probe must return nil, got: %v%s", err, h.callLog())
		}
		if buf.Len() != 0 {
			t.Errorf("the healthy probe path must log nothing new, got:\n%s", buf.String())
		}
		if len(h.calls) != 1 || h.calls[0] != sessionProbeLabel() {
			t.Errorf("the healthy probe path must stay one seam call, got:%s", h.callLog())
		}
	})
}

// TestProveUserManagerReachableWithRecovery_ClearsStaleLogindRecord is
// acceptance C2: with a FOREIGN logind record present, the one-shot recovery
// clears it — `loginctl disable-linger <RECORDED name>` immediately followed by
// `loginctl terminate-user <uid>`, exactly once, BEFORE the bring-up starts —
// and names the record in its own WARN.
func TestProveUserManagerReachableWithRecovery_ClearsStaleLogindRecord(t *testing.T) {
	h := newRecycledUidHost(t)
	h.foreignManagerRunning(t)
	// Unreachable until the recovery removes the foreign state, then reachable
	// again (INT-SPAWN-003: the readiness gate is what decides, not one probe).
	h.foreignServedAfterTeardown = true
	h.waitBudget = 30 * time.Millisecond
	// The uid's record belongs to a destroyed account and is lingering; the
	// re-check after the teardown finds it gone (nil = no such record).
	h.records = []*logindRecordStub{{name: rcForeignRecordName, linger: "yes", state: "active"}, nil}
	h.install(t, lingerDirWithEntries(t, 6))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if err := proveUserManagerReachableWithRecovery(context.Background(), h.username, h.uid, h.runtimeDir, logger); err != nil {
		t.Fatalf("the recovery must recover once the foreign logind record is cleared, got: %v%s", err, h.callLog())
	}

	uidArg := strconv.Itoa(h.uid)
	idxDisable := h.nextIndexOf("loginctl disable-linger "+rcForeignRecordName, 0)
	idxTerminate := h.nextIndexOf("loginctl terminate-user "+uidArg, 0)
	idxBringUp := h.nextIndexOf("systemctl start "+userRuntimeDirUnitName(h.uid), 0)
	if idxDisable < 0 {
		t.Fatalf("the recovery must run `loginctl disable-linger %s` for the RECORDED name:%s",
			rcForeignRecordName, h.callLog())
	}
	if idxTerminate < 0 {
		t.Fatalf("the recovery must terminate the uid's session after clearing the record:%s", h.callLog())
	}
	if idxTerminate != idxDisable+1 {
		t.Errorf("disable-linger (call %d) must be immediately followed by terminate-user (call %d):%s",
			idxDisable, idxTerminate, h.callLog())
	}
	if idxBringUp < 0 || idxTerminate > idxBringUp {
		t.Errorf("the record must be cleared BEFORE the bring-up starts (terminate call %d, bring-up call %d):%s",
			idxTerminate, idxBringUp, h.callLog())
	}
	if got := h.countPrefix("loginctl disable-linger " + rcForeignRecordName); got != 1 {
		t.Errorf("disable-linger for the recorded name ran %d time(s), want exactly 1:%s", got, h.callLog())
	}

	warns := warnLinesWithMarker(buf.String(), staleLogindRecordMarker)
	if len(warns) != 1 {
		t.Fatalf("expected exactly 1 stale-record WARN, got %d:\n%s", len(warns), buf.String())
	}
	for _, want := range []string{
		"record_name=" + rcForeignRecordName,
		"record_linger=yes",
		"uid=" + uidArg,
		rcTestUsername,
	} {
		if !strings.Contains(warns[0], want) {
			t.Errorf("the stale-record WARN must name %q:\n%s", want, warns[0])
		}
	}
	if !strings.Contains(warns[0], "not") || !strings.Contains(warns[0], rcForeignRecordName) {
		t.Errorf("the stale-record WARN must carry the reason the record is foreign:\n%s", warns[0])
	}

	// The recovery is still one-shot: one recovery, no respawn, and the
	// readiness gate retried before it (never a single-probe verdict).
	if got := h.countPrefix("user-session["); got < 2 {
		t.Errorf("expected the readiness gate to retry before the recovery, got %d probe(s):%s", got, h.callLog())
	}
	if got := len(warnLinesWithMarker(buf.String(), recycledUIDRecoveryMarker)); got != 1 {
		t.Errorf("expected exactly 1 recovery WARN, got %d:\n%s", got, buf.String())
	}
	if got := len(warnLinesWithMarker(buf.String(), logindRespawnMarker)); got != 0 {
		t.Errorf("the record is gone after the recovery, so no respawn may be reported, got %d:\n%s", got, buf.String())
	}
}

// TestProveUserManagerReachableWithRecovery_StillUnreachableIsBoundedAndNamed
// is the tick-452 shape end to end: the record the recovery cleared is STILL
// linger-active on the re-check (logind restarted the manager for the uid's
// dead owner), the final probe fails again, and the returned error names the
// attempted recovery while carrying the probe's own output. No second teardown.
func TestProveUserManagerReachableWithRecovery_StillUnreachableIsBoundedAndNamed(t *testing.T) {
	h := newRecycledUidHost(t)
	h.foreignManagerRunning(t)
	h.sessionBusAnswers = func(int) bool { return false } // still unreachable after the recovery
	h.waitBudget = 20 * time.Millisecond
	// One stable record: foreign before AND after the teardown.
	h.records = []*logindRecordStub{{name: rcForeignRecordName, linger: "yes", state: "active"}}
	h.install(t, lingerDirWithEntries(t, 6))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	err := proveUserManagerReachableWithRecovery(context.Background(), h.username, h.uid, h.runtimeDir, logger)
	if err == nil {
		t.Fatalf("expected the second probe failure to surface%s", h.callLog())
	}
	msg := err.Error()
	for _, want := range []string{
		recycledUIDRecoveryMarker,
		rcProbeFailureText,
		"rootless-install",
		userManagerUnitName(h.uid),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the bounded-recovery error must contain %q, got: %s", want, msg)
		}
	}

	// The respawn event is named exactly once, with the recorded name.
	respawn := warnLinesWithMarker(buf.String(), logindRespawnMarker)
	if len(respawn) != 1 {
		t.Fatalf("expected exactly 1 logind-respawn WARN, got %d:\n%s", len(respawn), buf.String())
	}
	for _, want := range []string{
		"record_name=" + rcForeignRecordName,
		"cleared_name=" + rcForeignRecordName,
		"record_linger=yes",
		"still linger-active",
	} {
		if !strings.Contains(respawn[0], want) {
			t.Errorf("the respawn WARN must name %q:\n%s", want, respawn[0])
		}
	}

	// One-shot loop guard: every step ran exactly once and probed twice.
	for _, action := range []string{
		"systemctl stop " + userManagerUnitName(h.uid),
		"systemctl stop " + userRuntimeDirUnitName(h.uid),
		"rm -rf " + h.runtimeDir,
		"loginctl disable-linger " + rcForeignRecordName,
	} {
		if got := h.countPrefix(action); got != 1 {
			t.Errorf("%q ran %d time(s), want exactly 1 (no loop):%s", action, got, h.callLog())
		}
	}
	if got := h.countPrefix("user-session["); got < 2 {
		t.Errorf("expected the readiness gate to retry on its own budget, got %d probe(s):%s", got, h.callLog())
	}
	if got := len(warnLinesWithMarker(buf.String(), recycledUIDRecoveryMarker)); got != 1 {
		t.Errorf("expected exactly 1 recovery WARN, got %d:\n%s", got, buf.String())
	}
}

// TestProveUserManagerReachableWithRecovery_NonForeignRecordIsLeftAlone is
// acceptance C3 for the new query: nothing but a genuinely foreign,
// linger-active record triggers a destructive step, so the recovery's teardown
// set is byte-identical to the pre-INT-SPAWN-002 behaviour in every other case.
// (The healthy path is pinned separately by HealthyPathIsNonDestructive, which
// asserts the whole call list is the probe alone — so no logind query runs
// there either.)
func TestProveUserManagerReachableWithRecovery_NonForeignRecordIsLeftAlone(t *testing.T) {
	cases := []struct {
		name    string
		records []*logindRecordStub
	}{
		{"no_record_at_all", nil},
		{"record_names_the_user_being_brought_up", []*logindRecordStub{
			{name: rcTestUsername, linger: "yes", state: "active"},
		}},
		{"foreign_name_but_linger_off", []*logindRecordStub{
			{name: rcForeignRecordName, linger: "no", state: "active"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecycledUidHost(t)
			h.foreignManagerRunning(t)
			// The recovery is what makes the bus answer at all.
			h.foreignServedAfterTeardown = true
			h.waitBudget = 30 * time.Millisecond
			h.records = tc.records
			h.install(t, lingerDirWithEntries(t, 3))

			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			if err := proveUserManagerReachableWithRecovery(context.Background(), h.username, h.uid, h.runtimeDir, logger); err != nil {
				t.Fatalf("the recovery must still work for a non-foreign record, got: %v%s", err, h.callLog())
			}
			if got := h.countPrefix("loginctl disable-linger"); got != 0 {
				t.Errorf("a non-foreign record must never be cleared, disable-linger ran %d time(s):%s", got, h.callLog())
			}
			if got := h.countPrefix("loginctl terminate-user " + strconv.Itoa(h.uid)); got != 1 {
				t.Errorf("the uid's session must be terminated exactly once (by the teardown), got %d:%s", got, h.callLog())
			}
			if got := len(warnLinesWithMarker(buf.String(), staleLogindRecordMarker)); got != 0 {
				t.Errorf("no stale-record WARN may be logged, got %d:\n%s", got, buf.String())
			}
			if got := len(warnLinesWithMarker(buf.String(), logindRespawnMarker)); got != 0 {
				t.Errorf("no respawn WARN may be logged, got %d:\n%s", got, buf.String())
			}
			for _, action := range []string{
				"systemctl stop " + userManagerUnitName(h.uid),
				"systemctl stop " + userRuntimeDirUnitName(h.uid),
				"rm -rf " + h.runtimeDir,
			} {
				if got := h.countPrefix(action); got != 1 {
					t.Errorf("the teardown must be unchanged for a non-foreign record, %q ran %d time(s):%s",
						action, got, h.callLog())
				}
			}
		})
	}
}

// TestProveUserManagerReachableWithRecovery_ExpiredDeadlineIsPrompt is the
// short-context half of acceptance C3: a caller whose deadline already expired
// gets that deadline back promptly and NO destructive action at all — not even
// the new logind query, and certainly no teardown.
func TestProveUserManagerReachableWithRecovery_ExpiredDeadlineIsPrompt(t *testing.T) {
	h := newRecycledUidHost(t)
	h.foreignManagerRunning(t)
	// The bus never answers, so the readiness gate rides out its OWN budget;
	// only then does the expired caller deadline surface — and it must still
	// surface promptly, without any destructive work.
	h.sessionBusAnswers = func(int) bool { return false }
	h.waitBudget = 20 * time.Millisecond
	h.records = []*logindRecordStub{{name: rcForeignRecordName, linger: "yes", state: "active"}}
	h.install(t, t.TempDir())

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	err := proveUserManagerReachableWithRecovery(ctx, h.username, h.uid, h.runtimeDir, logger)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected exactly context.DeadlineExceeded, got %T: %v", err, err)
	}
	for _, forbidden := range []string{
		"systemctl stop", "loginctl terminate-user", "loginctl disable-linger",
		"loginctl show-user", "loginctl enable-linger", "rm -rf", "systemctl start",
	} {
		if got := h.countPrefix(forbidden); got != 0 {
			t.Errorf("an expired caller must not trigger work, but %q ran %d time(s):%s", forbidden, got, h.callLog())
		}
	}
	if got := len(warnLinesWithMarker(buf.String(), staleLogindRecordMarker)); got != 0 {
		t.Errorf("an expired caller must not reach the record handling, got %d WARN(s):\n%s", got, buf.String())
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
