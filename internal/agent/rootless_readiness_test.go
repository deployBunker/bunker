package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ── INT-SPAWN-003: the readiness gate proves the bus ANSWERS ────────────────
//
// Tick 453 on bunker-mvp: every spawn in the standalone gate's nested
// regression suite died at stage rootless-install with
//
//	session_probe='Failed to connect to bus: No medium found'
//	unit user@1002.service is-active=active (Result=success), linger_entries=8
//
// while the journal of the same second showed the manager coming up CLEANLY
// ("Listening on dbus.socket") and the su session failing ~0.7s later. The old
// readiness gate polled `os.Stat(<runtimeDir>/bus)`, and a socket FILE existing
// is not evidence that the bus ACCEPTS a connection: the single session-side
// probe ran while the manager was still coming up and its failure became the
// FINAL attribution (and, when the unit was ACTIVE, a bogus recycled-uid
// recovery) instead of a retryable NOT-READY condition.
//
// waitForUserManagerBus repeats the REAL probe (the same seam the installer
// path uses) on the package budget. These tests pin:
//
//	T1 a bus that answers after N refusals is a WAIT: success, exactly N+1
//	   probes, ZERO destructive calls;
//	T2 a bus that never answers is a bounded failure whose attribution carries
//	   the LAST probe's own stderr plus stage/user/unit/state/linger/journal;
//	T3 the healthy path is ONE probe, zero destructive calls and no log line;
//	T4 the caller's DEADLINE is never the reported cause: a deadline that
//	   expires mid-wait does not stop the gate from reaching a late bus, and no
//	   probe is handed the caller's dead context (INT-CI-007 detachment);
//	T5 the tick-453 sequence end to end at the seam: with the unit ACTIVE and
//	   the first probes refused, the spawn PROCEEDS (no teardown) once the bus
//	   answers — and the persistent-refusal variant still fires exactly one
//	   recovery, with the final attribution carrying the last probe's stderr.
//
// Everything runs through the package seams (recycledUidHost / fakeCtl /
// installFakeCtl): no root, no real systemd, no real users, no sleeping for a
// real manager.

// rrBusRefusalText is the verbatim session-side stderr from tick 453: the
// manager's bus socket existed but the bus did not answer yet.
const rrBusRefusalText = "Failed to connect to bus: No medium found"

// rrTestTimeout is a generous ceiling for the gate's own bounded waits. It is
// only a sanity bound: the point of T2/T5b is that the failure is bounded by
// the (shrunk) budget instead of running for the package default.
const rrTestTimeout = 2 * time.Second

// infoLinesWithMarker returns the INFO records carrying marker, so "at most ONE
// extra log line" is asserted on real log records rather than on prose.
func infoLinesWithMarker(log, marker string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "level=INFO") && strings.Contains(line, marker) {
			out = append(out, line)
		}
	}
	return out
}

// rrRefusalText is the failed probe's own output for probe index idx: the text
// is index-tagged so a test can prove the attribution carries the LAST probe's
// stderr and not the first one's.
func rrRefusalText(idx int) []byte {
	return []byte(fmt.Sprintf("%s (probe %d)", rrBusRefusalText, idx))
}

// rrLogger returns a logger writing into buf (log assertions read buf).
func rrLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// rrDestructivePrefixes are the commands the readiness gate must never issue:
// the recovery's teardown, its bring-up, and the record handling. The
// attribution's own READ-ONLY state queries (systemctl is-active, loginctl
// show-user, journalctl) are not in this list — they run only on the
// exhaustion path and change nothing.
var rrDestructivePrefixes = []string{
	"systemctl stop",
	"systemctl start",
	"systemctl reset-failed",
	"loginctl terminate-user",
	"loginctl disable-linger",
	"loginctl enable-linger",
	"rm ",
}

// assertNoDestructiveCalls asserts the gate took ZERO destructive action, with
// the whole call log printed on failure.
func assertNoDestructiveCalls(t *testing.T, h *recycledUidHost) {
	t.Helper()
	for _, forbidden := range rrDestructivePrefixes {
		if got := h.countPrefix(forbidden); got != 0 {
			t.Errorf("the readiness gate must take no destructive action, but %q ran %d time(s):%s",
				forbidden, got, h.callLog())
		}
	}
}

// ── T1: a bus that answers late is a wait, not a failure ───────────────────

// TestWaitForUserManagerBus_RefusedThenAnswers is the tick-453 core: the probe
// is refused with the real "No medium found" text N times, then the bus
// answers. The gate must return nil, having made exactly N+1 probes and issued
// ZERO destructive commands — and it must say so ONCE, naming the attempt and
// the first probe's own error.
func TestWaitForUserManagerBus_RefusedThenAnswers(t *testing.T) {
	const refusals = 3

	h := newRecycledUidHost(t)
	h.waitBudget = 500 * time.Millisecond
	h.sessionFailureText = func(int) []byte { return []byte(rrBusRefusalText) }
	h.sessionBusAnswers = func(idx int) bool { return idx >= refusals }
	h.install(t, t.TempDir())

	var buf bytes.Buffer
	if err := waitForUserManagerBus(context.Background(), h.username, h.uid, h.runtimeDir, rrLogger(&buf)); err != nil {
		t.Fatalf("a bus that answers inside the readiness budget must return nil, got: %v%s", err, h.callLog())
	}

	if h.probeCalls != refusals+1 {
		t.Errorf("expected exactly %d probes (the refusals plus the answering one), got %d:%s",
			refusals+1, h.probeCalls, h.callLog())
	}
	// The probe is the ONLY thing the gate may issue.
	if len(h.calls) != refusals+1 {
		t.Fatalf("the readiness gate must issue nothing but the probe, got:%s", h.callLog())
	}
	for i, c := range h.calls {
		if !strings.HasPrefix(c, "user-session[") {
			t.Errorf("call %d is not the session probe: %q:%s", i, c, h.callLog())
		}
	}
	assertNoDestructiveCalls(t, h)

	retries := infoLinesWithMarker(buf.String(), userManagerReadinessRetryInfo)
	if len(retries) != 1 {
		t.Fatalf("expected exactly 1 retry log line, got %d:\n%s", len(retries), buf.String())
	}
	for _, want := range []string{
		"attempt=1", "retry_attempt=2",
		h.username, "uid=" + fmt.Sprint(h.uid), userManagerUnitName(h.uid),
		rrBusRefusalText,
	} {
		if !strings.Contains(retries[0], want) {
			t.Errorf("the retry log line must name %q:\n%s", want, retries[0])
		}
	}
}

// ── T2: a bus that never answers is a BOUNDED, attributed failure ──────────

// TestWaitForUserManagerBus_NeverAnswersIsBoundedAndAttributed proves the
// exhaustion path: bounded by the package budget (NOT by the caller's request
// deadline), failing with the existing attribution error — stage, user, unit,
// unit state, linger count, journal excerpt — carrying the LAST probe's own
// stderr, and with the retry line emitted exactly once.
func TestWaitForUserManagerBus_NeverAnswersIsBoundedAndAttributed(t *testing.T) {
	const budget = 60 * time.Millisecond

	h := newRecycledUidHost(t)
	h.waitBudget = budget
	h.sessionBusAnswers = func(int) bool { return false }
	h.sessionFailureText = rrRefusalText
	h.install(t, lingerDirWithEntries(t, 3))

	var buf bytes.Buffer
	start := time.Now()
	err := waitForUserManagerBus(context.Background(), h.username, h.uid, h.runtimeDir, rrLogger(&buf))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected the exhausted readiness budget to surface%s", h.callLog())
	}
	if elapsed > budget+rrTestTimeout {
		t.Errorf("the wait took %v for a %v budget: it must be bounded by its OWN budget, not by the caller%s",
			elapsed, budget, h.callLog())
	}
	if h.probeCalls < 2 {
		t.Errorf("the gate must retry before giving up, got %d probe(s):%s", h.probeCalls, h.callLog())
	}

	msg := err.Error()
	for _, want := range []string{
		"rootless-install",
		"agent user manager unreachable",
		h.username,
		userManagerUnitName(h.uid),
		"is-active=inactive",
		"linger entries: 3",
		"journal:",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the exhaustion attribution is missing %q, got: %s", want, msg)
		}
	}
	last := string(rrRefusalText(h.probeCalls - 1))
	if !strings.Contains(msg, last) {
		t.Errorf("the attribution must carry the LAST probe's own stderr %q, got: %s", last, msg)
	}
	warns := warnLinesWithMarker(buf.String(), "agent user manager unreachable")
	if len(warns) != 1 {
		t.Fatalf("expected exactly 1 unreachable WARN, got %d:\n%s", len(warns), buf.String())
	}
	if !strings.Contains(warns[0], last) {
		t.Errorf("the unreachable WARN must carry the LAST probe's own stderr %q:\n%s", last, warns[0])
	}
	if got := len(infoLinesWithMarker(buf.String(), userManagerReadinessRetryInfo)); got != 1 {
		t.Errorf("expected exactly 1 retry log line (not one per attempt), got %d:\n%s", got, buf.String())
	}
	assertNoDestructiveCalls(t, h)
	// The exhaustion path gathers LIVE state for the attribution: exactly one
	// is-active query, and the probe stayed the only bus-facing command.
	if got := h.countPrefix("systemctl is-active"); got != 1 {
		t.Errorf("the attribution must query the unit state exactly once, got %d:%s", got, h.callLog())
	}
}

// ── T3: the healthy path stays one probe, zero action, no log ──────────────

// TestWaitForUserManagerBus_HealthyPathIsOneProbeAndSilent pins the cost of the
// gate when the bus answers immediately: exactly one probe seam call, zero
// destructive calls, and NO log line (the retry line exists only when a retry
// actually happened).
func TestWaitForUserManagerBus_HealthyPathIsOneProbeAndSilent(t *testing.T) {
	h := newRecycledUidHost(t)
	h.waitBudget = 500 * time.Millisecond
	h.install(t, t.TempDir())

	var buf bytes.Buffer
	if err := waitForUserManagerBus(context.Background(), h.username, h.uid, h.runtimeDir, rrLogger(&buf)); err != nil {
		t.Fatalf("healthy bus must return nil, got: %v%s", err, h.callLog())
	}

	if h.probeCalls != 1 {
		t.Errorf("healthy path must probe exactly once, got %d:%s", h.probeCalls, h.callLog())
	}
	if len(h.calls) != 1 || h.calls[0] != sessionProbeLabel() {
		t.Errorf("healthy path must issue exactly the probe and nothing else, got:%s", h.callLog())
	}
	assertNoDestructiveCalls(t, h)
	if buf.Len() != 0 {
		t.Errorf("healthy path must log nothing, got:\n%s", buf.String())
	}
}

// ── T4: the caller's deadline is never the reported cause (INT-CI-007) ─────

// TestWaitForUserManagerBus_BudgetBeatsCallerDeadline mirrors
// TestWaitForUserManager_BudgetBeatsCallerDeadline at the readiness gate: the
// caller's deadline expires WHILE the gate is polling and the bus only starts
// answering after it has expired. Success must still be reached on the gate's
// own budget, the deadline must never be reported, and — because the host's
// session runner fails on a done context exactly as `su` under
// exec.CommandContext does — at most ONE probe may have been handed the
// caller's dead context (the one in flight at the expiry instant; every later
// probe must be detached).
func TestWaitForUserManagerBus_BudgetBeatsCallerDeadline(t *testing.T) {
	h := newRecycledUidHost(t)
	h.waitBudget = 2 * time.Second
	h.sessionHonorsContext = true

	var busReady atomic.Bool
	h.sessionBusAnswers = func(int) bool { return busReady.Load() }
	h.install(t, t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	// The bus answers only AFTER the caller's deadline expired, so a gate that
	// inherited the caller's deadline could never succeed.
	go func() {
		<-ctx.Done()
		busReady.Store(true)
	}()

	if err := waitForUserManagerBus(ctx, h.username, h.uid, h.runtimeDir, ctlLogger()); err != nil {
		t.Fatalf("the gate's own budget must outlive the caller's deadline, got: %v%s", err, h.callLog())
	}
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("test premise broken: the caller deadline must have expired, got ctx.Err()=%v", ctx.Err())
	}
	if !busReady.Load() {
		t.Fatalf("test premise broken: the bus must have answered after the caller deadline expired")
	}
	if h.probeCalls < 3 {
		t.Errorf("expected the gate to keep probing after the caller deadline, got %d probe(s)", h.probeCalls)
	}
	if h.doneCtxProbes > 1 {
		t.Errorf("the gate must detach once the caller's deadline expires, but %d probes were handed a done context:%s",
			h.doneCtxProbes, h.callLog())
	}
	assertNoDestructiveCalls(t, h)
}

// TestWaitForUserManagerBus_CancelledCallerIsPrompt pins the other direction of
// the same rule: a genuine CANCEL is returned promptly and issues no command at
// all (including the probe itself).
func TestWaitForUserManagerBus_CancelledCallerIsPrompt(t *testing.T) {
	h := newRecycledUidHost(t)
	h.waitBudget = 5 * time.Second
	h.sessionBusAnswers = func(int) bool { return false }
	h.install(t, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := waitForUserManagerBus(ctx, h.username, h.uid, h.runtimeDir, ctlLogger()); err != context.Canceled {
		t.Fatalf("expected exactly context.Canceled, got %T: %v", err, err)
	}
	if len(h.calls) != 0 {
		t.Errorf("a cancelled caller must issue no command at all, got:%s", h.callLog())
	}
}

// ── T5: the tick-453 sequence end to end at the recovery seam ──────────────

// TestProveUserManagerReachableWithRecovery_ReadinessWaitReplacesTheBogusRecovery
// is the tick-453 sequence: the unit is ACTIVE (is-active=active) and the first
// probes are refused with the real "No medium found" text, then the bus
// answers. The spawn must PROCEED — no fingerprint query, no recovery, no
// teardown, no rm — because the readiness wait turned a bogus recovery trigger
// into a bounded wait.
func TestProveUserManagerReachableWithRecovery_ReadinessWaitReplacesTheBogusRecovery(t *testing.T) {
	h := newRecycledUidHost(t)
	h.foreignManagerRunning(t) // the tick-453 host state: socket present, unit ACTIVE
	h.waitBudget = 500 * time.Millisecond
	h.sessionFailureText = func(int) []byte { return []byte(rrBusRefusalText) }
	h.sessionBusAnswers = func(idx int) bool { return idx >= 2 }
	h.install(t, lingerDirWithEntries(t, 8))

	var buf bytes.Buffer
	if err := proveUserManagerReachableWithRecovery(context.Background(), h.username, h.uid, h.runtimeDir, rrLogger(&buf)); err != nil {
		t.Fatalf("a bus that answers within the readiness budget must let the spawn proceed, got: %v%s", err, h.callLog())
	}

	if h.probeCalls != 3 {
		t.Errorf("expected exactly 3 probes (2 refusals + the answering one) and then no recovery re-probe, got %d:%s",
			h.probeCalls, h.callLog())
	}
	if len(h.calls) != 3 {
		t.Fatalf("the readiness wait must replace the recovery: nothing but the probe may run, got:%s", h.callLog())
	}
	assertNoDestructiveCalls(t, h)
	for _, marker := range []string{recycledUIDRecoveryMarker, staleLogindRecordMarker, logindRespawnMarker} {
		if got := len(warnLinesWithMarker(buf.String(), marker)); got != 0 {
			t.Errorf("no recovery may be reported when the bus answers, got %d %q WARN(s):\n%s", got, marker, buf.String())
		}
	}
	retries := infoLinesWithMarker(buf.String(), userManagerReadinessRetryInfo)
	if len(retries) != 1 {
		t.Fatalf("expected exactly 1 retry log line, got %d:\n%s", len(retries), buf.String())
	}
	if !strings.Contains(retries[0], rrBusRefusalText) {
		t.Errorf("the retry line must carry the first probe's own stderr %q:\n%s", rrBusRefusalText, retries[0])
	}
}

// TestProveUserManagerReachableWithRecovery_PersistentRefusalRecoversExactlyOnce
// is the persistent-refusal variant: the bus never answers, so the fingerprint
// still matches and the recovery fires — EXACTLY once (every teardown step
// once, one recovery WARN), each of the two readiness waits retrying on its own
// budget, and the final attribution carrying the LAST probe's own stderr inside
// the existing attribution fields.
func TestProveUserManagerReachableWithRecovery_PersistentRefusalRecoversExactlyOnce(t *testing.T) {
	h := newRecycledUidHost(t)
	h.foreignManagerRunning(t)
	h.waitBudget = 40 * time.Millisecond
	h.sessionBusAnswers = func(int) bool { return false }
	h.sessionFailureText = rrRefusalText
	h.install(t, lingerDirWithEntries(t, 5))

	var buf bytes.Buffer
	err := proveUserManagerReachableWithRecovery(context.Background(), h.username, h.uid, h.runtimeDir, rrLogger(&buf))
	if err == nil {
		t.Fatalf("a bus that never answers must surface the attribution error%s", h.callLog())
	}

	// One-shot: every teardown step exactly once, no loop.
	for _, action := range []string{
		"systemctl stop " + userManagerUnitName(h.uid),
		"loginctl terminate-user " + fmt.Sprint(h.uid),
		"systemctl stop " + userRuntimeDirUnitName(h.uid),
		"rm -rf " + h.runtimeDir,
	} {
		if got := h.countPrefix(action); got != 1 {
			t.Errorf("recovery action %q ran %d time(s), want exactly 1 (one-shot):%s", action, got, h.callLog())
		}
	}
	if got := len(warnLinesWithMarker(buf.String(), recycledUIDRecoveryMarker)); got != 1 {
		t.Errorf("expected exactly 1 recovery WARN (no second recovery), got %d:\n%s", got, buf.String())
	}

	// Both readiness waits retried on their own budget: probes before the
	// teardown and probes after the bring-up's manager start.
	stopIdx := h.nextIndexOf("systemctl stop "+userManagerUnitName(h.uid), 0)
	if stopIdx < 0 {
		t.Fatalf("the recovery never stopped the manager:%s", h.callLog())
	}
	before := h.countPrefixBefore("user-session[", stopIdx)
	if before < 2 {
		t.Errorf("the readiness wait must retry before the recovery fires, got %d probe(s) before the teardown:%s",
			before, h.callLog())
	}
	startIdx := h.nextIndexOf("systemctl start "+userManagerUnitName(h.uid), 0)
	if startIdx < 0 {
		t.Fatalf("the recovery never brought the manager back up:%s", h.callLog())
	}
	after := h.countPrefixAfter("user-session[", startIdx)
	if after < 2 {
		t.Errorf("the FINAL readiness wait must retry too, got %d probe(s) after the bring-up:%s", after, h.callLog())
	}

	msg := err.Error()
	for _, want := range []string{
		recycledUIDRecoveryMarker,
		"rootless-install",
		h.username,
		userManagerUnitName(h.uid),
		"is-active=",
		"linger entries: 5",
		"journal:",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the bounded-recovery error is missing %q, got: %s", want, msg)
		}
	}
	last := string(rrRefusalText(h.probeCalls - 1))
	if !strings.Contains(msg, last) {
		t.Errorf("the final attribution must carry the LAST probe's own stderr %q, got: %s", last, msg)
	}
}
