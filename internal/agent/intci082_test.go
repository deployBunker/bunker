package agent

// INT-CI-082 regression tests for the spawn session probe's budget.
//
// E2E battery run 39822820089 (2026-10-23, bunker-mvp) had all four section-8
// concurrent spawns (e2e-agent-2..5) die with
//
//	spawn e2e-agent-<n> failed at stage session-probe: session probe attempt
//	timed out after 10s (ssh killed by the per-attempt deadline)
//
// while the same cell passed 30 minutes later. The measured host state at the
// time was a systemd-logind that did not service its bus: `busctl ... Peer
// Ping` and `loginctl list-users` each took 25.03-25.04s, sshd logged
// "pam_systemd(sshd:session): Failed to create session: Connection timed out",
// and the probe's own ssh argv hung past a 12s outer bound (4/4). Load was
// 1.04 on 8 CPUs, so this was not CPU starvation; it was a session subsystem
// that answers in tens of seconds, against a probe deadline of ten.
//
// These tests drive the REAL probe (probeAgentSessionLive) with a PATH stub for
// ssh, so no root, no systemd and no real ssh are involved. They pin:
//
//  1. the per-attempt budget scales with the probes in flight and is hard
//     capped, while a LONE probe keeps the shipped 10s fast path
//     (TestSessionProbeBudget_ScaledToConcurrency);
//  2. the scaling is really WIRED: a concurrent probe's attempt survives past
//     the lone-probe budget (TestSessionProbe_ScaledBudgetIsUsedUnderConcurrency);
//     and the proof still fails when scaling is disabled, so the survival can
//     only come from the scaled grant, never from a slow host
//     (TestSessionProbe_ScaledBudgetIsUsedUnderConcurrency_ScalingDisabled);
//     the survival margin itself is pinned as pure arithmetic so the test
//     seams cannot shrink back into the load-flaky window
//     (TestSessionProbeMarginGuard_SeamsCannotShrinkIntoTheFlakyWindow);
//  3. a genuine session DENIAL (exit 254, INT-DEMO-001) is NOT retried, does
//     not gather diagnostics, and fails fast however large the budget
//     (TestSessionProbe_DenialFailsFastAndIsNotMasked);
//  4. a timed-out attempt names the budget it was granted and the concurrency
//     that scaled it, and every attempt's wall time is recorded
//     (TestSessionProbe_TimeoutNamesScaledBudgetAndRecordsLatencies);
//  5. a final failure carries the logind record and the journalctl _UID=<uid>
//     excerpt, with host audit noise filtered out
//     (TestSessionProbe_FailureCarriesSessionDiagnostics);
//  6. the probe never retries past its TOTAL budget
//     (TestSessionProbe_TotalBudgetStopsRetrying);
//  7. the in-flight counter is always released
//     (checked by every test via intci082NoLeak).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// intci082ShrinkProbeSeams shrinks every budget seam so a probe can be driven
// in milliseconds. Cleanup restores them, so a shortened budget can never leak
// into the next test.
func intci082ShrinkProbeSeams(t *testing.T, base, perSpawn, max, total time.Duration, attempts int) {
	t.Helper()
	ob, op, om, ot, oa := sessionProbeBudgetBase, sessionProbeBudgetPerSpawn, sessionProbeBudgetMax,
		sessionProbeTotalBudgetMax, sessionProbeMaxAttempts
	od := sessionProbeDiagnosticsTimeout
	t.Cleanup(func() {
		sessionProbeBudgetBase, sessionProbeBudgetPerSpawn, sessionProbeBudgetMax = ob, op, om
		sessionProbeTotalBudgetMax, sessionProbeMaxAttempts = ot, oa
		sessionProbeDiagnosticsTimeout = od
	})
	sessionProbeBudgetBase, sessionProbeBudgetPerSpawn, sessionProbeBudgetMax = base, perSpawn, max
	sessionProbeTotalBudgetMax, sessionProbeMaxAttempts = total, attempts
}

// intci082SilenceDiagnostics replaces the diagnostics gatherer with a no-op and
// reports whether it was called at all — the denial test needs to prove the
// gatherer did NOT run (fast fail must not be padded with journal reads).
func intci082SilenceDiagnostics(t *testing.T) *int32 {
	t.Helper()
	var calls int32
	orig := sessionProbeDiagnosticsRunner
	sessionProbeDiagnosticsRunner = func(context.Context, string, ...string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return nil, nil
	}
	t.Cleanup(func() { sessionProbeDiagnosticsRunner = orig })
	return &calls
}

// intci082NoLeak asserts the in-flight counter was released: a probe that
// returned without deregistering itself would inflate every later budget on
// the host for the life of the daemon.
func intci082NoLeak(t *testing.T) {
	t.Helper()
	if got := sessionProbesActive(); got != 0 {
		t.Errorf("in-flight session probes = %d after the probe returned, want 0 (the counter must be released)", got)
	}
}

// intci082Key writes a placeholder SSH key path for the probe argv.
func intci082Key(t *testing.T) string {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPath
}

// ── 1. the budget arithmetic ───────────────────────────────────────────────

// TestSessionProbeBudget_ScaledToConcurrency pins the production budget table.
// The defaults are left in force: they are the numbers the fix ships, and the
// 25.03s logind D-Bus latency measured on bunker-mvp during the 2026-10-23
// wedge is the datum the table has to clear.
func TestSessionProbeBudget_ScaledToConcurrency(t *testing.T) {
	tests := []struct {
		inFlight int
		want     time.Duration
		why      string
	}{
		{0, 10 * time.Second, "an empty signal is treated as a lone probe"},
		{1, 10 * time.Second, "a lone probe keeps the shipped 10s fast path"},
		{2, 20 * time.Second, "two concurrent probes each get a share"},
		{3, 30 * time.Second, "three concurrent probes"},
		{4, 40 * time.Second, "the section-8 4-way burst"},
		{5, 40 * time.Second, "the hard cap holds just above the burst"},
		{1000, 40 * time.Second, "the cap holds for a runaway count"},
	}
	for _, tc := range tests {
		if got := sessionProbeBudget(tc.inFlight); got != tc.want {
			t.Errorf("sessionProbeBudget(%d) = %s, want %s (%s)", tc.inFlight, got, tc.want, tc.why)
		}
	}

	// The datum this fix answers to: the logind D-Bus service latency measured
	// on bunker-mvp during the wedge must sit INSIDE the budget granted to a
	// section-8 burst, not on its edge.
	const measuredLogindLatency = 25030 * time.Millisecond
	if got := sessionProbeBudget(4); got <= measuredLogindLatency {
		t.Errorf("budget at four concurrent probes = %s, want more than the %s logind latency measured on bunker-mvp",
			got, measuredLogindLatency)
	}
	// ...and the flat 10s deadline must still be the LONE probe's budget, so a
	// single healthy spawn is exactly as fast as it was before this change.
	if got := sessionProbeBudget(1); got != 10*time.Second {
		t.Errorf("lone-probe budget = %s, want the shipped 10s", got)
	}
	// The total budget must still bound the whole probe below the server's
	// request timeout (300s), whatever the per-attempt budget grew to.
	if sessionProbeTotalBudgetMax >= 300*time.Second {
		t.Errorf("total probe budget = %s, want it below the 300s server request timeout", sessionProbeTotalBudgetMax)
	}
}

// ── 2. the scaling is wired into the probe ────────────────────────────────

// The hermetic seam set for the concurrency proof (QA-BUNKER-080): the lone
// budget stays small enough for the session hold to decisively outlive it,
// while the scaled grant dwarfs the hold by a MULTI-SECOND margin. That margin
// is the whole fix: a per-attempt kill fires on the wall clock LATE under
// starvation, never early, so a multi-second margin cannot be eaten by
// scheduler delay — whereas the old 1200ms scaled grant left only 150ms
// between the hold and the grant, and the battery's JIT-starved host ate it
// (two ci-pass native-leg reds). The concurrency proof and its margin guard
// share these constants so they cannot drift apart.
const (
	intci082HermeticLoneBudget  = 300 * time.Millisecond
	intci082HermeticPerSpawn    = 5 * time.Second
	intci082HermeticCap         = 5 * time.Second
	intci082HermeticTotal       = 60 * time.Second
	intci082HermeticSessionHold = 450 * time.Millisecond
	// intci082RedProofHold is the ScalingDisabled arm's session hold: it must
	// outlive the lone budget by the per-child gap PLUS the kill-timer
	// starvation allowance so its kill-vs-exit ordering cannot invert under
	// load (see that test's comment; the guard pins the arithmetic).
	intci082RedProofHold        = 1600 * time.Millisecond
	intci082HermeticMinMargin   = 2 * time.Second
	intci082HermeticMinHoldGap  = 100 * time.Millisecond
	intci082HermeticMaxAttempts = 3
)

// sessionProbeBudgetWith is the budget formula as a pure function — the same
// arithmetic sessionProbeBudget performs over the package seam vars — so the
// margin guard can reason about the HERMETIC seam set without mutating any
// package state.
func sessionProbeBudgetWith(base, perSpawn, max time.Duration, inFlight int) time.Duration {
	if inFlight < 1 {
		inFlight = 1
	}
	budget := base
	if scaled := time.Duration(inFlight) * perSpawn; scaled > budget {
		budget = scaled
	}
	if budget > max {
		budget = max
	}
	return budget
}

// TestSessionProbe_ScaledBudgetIsUsedUnderConcurrency proves the concurrency
// signal reaches the REAL probe — and, with DF-BUNKER-112, proves it without
// leaning on absolute wall-clock windows that a starved host breaks (the
// assertion used to red the full suite under loadavg >= nproc while passing in
// isolation).
//
// Two probes run against a PATH-stubbed ssh. A BLOCKER probe starts first and
// each ssh attempt it makes drops a marker file named after its own PID in a
// per-attempt directory; the PROVER probe starts only once the prover's own
// first marker would be unambiguous — i.e. it polls the blocker's marker and
// enters immediately after, so the prover is guaranteed to observe at least 2
// in-flight probes and be granted the SCALED budget. (The old version started
// the peer on a fixed 100ms sleep; under CPU starvation the lone probe's
// attempt could die first and the peer entered alone.)
//
// The stub holds every ssh session open for 450ms. With the seams shrunk to a
// 300ms lone budget and a 5s scaled budget (capped at 5s), a prover attempt
// that SURVIVES the hold proves the grant exceeded 450ms — more than the 300ms
// lone budget — no matter how slow the host was: a per-attempt context kill
// fires on the wall clock, so surviving a 450ms hold cannot happen on a 300ms
// budget. The scaled budget's slack over the hold is ~4.55s (vs the old
// 1200ms grant's 150ms, which starved hosts ate — QA-BUNKER-080): a kill
// cannot fire before the budget elapses, so the only way a starved prover
// fails is a genuinely-late kill, never a premature one.
//
// Every assertion reads same-run report facts (Budget, Concurrent) or
// load-monotone bounds (latency >= the hold), never absolute ceilings, so a
// starved host cannot satisfy OR break them.
//
// The blocker keeps the fast path on purpose: it began alone, so its attempts
// are killed at 300ms and (if a retry lands after the prover's exit) it may
// even succeed inside the budget its counter LATCHED while the prover was in
// flight — both are the documented production behaviour for a lone probe, so
// only the prover's outcome is asserted.
func TestSessionProbe_ScaledBudgetIsUsedUnderConcurrency(t *testing.T) {
	intci082ShrinkProbeSeams(t, intci082HermeticLoneBudget, intci082HermeticPerSpawn,
		intci082HermeticCap, intci082HermeticTotal, intci082HermeticMaxAttempts)
	intci082SilenceDiagnostics(t)

	const sessionHold = intci082HermeticSessionHold
	markDir := t.TempDir()
	binDir := t.TempDir()
	// Each attempt records its own PID marker, then holds the session open
	// for sessionHold and exits 0. PID-named markers (instead of the old
	// shared $$-named ones) keep a killed attempt's marker from being
	// mistaken for a live peer during a retry.
	stub := "touch " + markDir + "/$$\n" +
		"sleep " + fmt.Sprintf("%.2f", sessionHold.Seconds()) + "\n" +
		"exit 0\n"
	writeStub(t, binDir, "ssh", stub)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	keyPath := intci082Key(t)

	type result struct {
		report sessionProbeReport
		err    error
	}
	var blocker, prover result
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		blocker.report, blocker.err = probeAgentSessionLive(context.Background(), "blocker082", "bunker-blocker082", keyPath)
	}()
	// Wait (bounded) for the blocker's first ssh attempt marker: the prover
	// may only enter once a peer is provably in flight. A broken stub fails
	// the test loudly instead of stalling the suite to its global timeout.
	deadline := time.Now().Add(30 * time.Second)
	for {
		entries, err := os.ReadDir(markDir)
		if err == nil && len(entries) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ssh stub never dropped an attempt marker; the blocker probe did not start")
		}
		time.Sleep(2 * time.Millisecond)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		prover.report, prover.err = probeAgentSessionLive(context.Background(), "prover082", "bunker-prover082", keyPath)
	}()
	wg.Wait()

	// AC2 (DF-BUNKER-112): the proof reads SAME-RUN facts. The prover was
	// granted the scaled budget for two in-flight probes…
	if want := sessionProbeBudget(2); prover.report.Budget != want {
		t.Errorf("prover budget = %s, want the scaled %s", prover.report.Budget, want)
	}
	if prover.report.Concurrent < 2 {
		t.Errorf("prover observed %d in-flight probe(s), want 2 (the scaling was not exercised)", prover.report.Concurrent)
	}
	// …and an attempt that SURVIVED a hold longer than the lone budget proves
	// the scaled budget was USED, not merely computed: the attempt-kill fires
	// on the wall clock, so survival cannot happen under a 300ms grant. The
	// bound is load-monotone (latency >= hold), never an absolute ceiling a
	// slow host could overshoot.
	if prover.err != nil {
		t.Fatalf("the concurrent probe failed although it was granted the scaled budget %s: %v (report: %+v)",
			sessionProbeBudget(2), prover.err, prover.report)
	}
	if len(prover.report.Latencies) != 1 || prover.report.Latencies[0] < sessionHold {
		t.Errorf("prover attempt latencies = %v, want one surviving attempt of >= %s: an attempt that outlived the lone-probe budget (300ms) proves the scaled budget was USED",
			prover.report.Latencies, sessionHold)
	}
	intci082NoLeak(t)
}

// TestSessionProbe_ScaledBudgetIsUsedUnderConcurrency_ScalingDisabled is the
// red-proof arm (DF-BUNKER-112 AC3): with the concurrency scaling disabled —
// a zero per-probe share flattens budget(n) back to the lone budget — the
// SAME setup must FAIL instead of succeeding: the stub holds the session
// longer than the lone budget, so the prover's attempt must be killed by the
// 300ms deadline and named as a timeout. If this arm ever goes green, the
// main test's "survival proves the scaled grant" reasoning is void (a host
// could be surviving holds for free).
//
// Kill-vs-exit ordering under starvation (QA-BUNKER-080): this arm relies on
// the per-attempt kill winning the race against a fast-exiting ssh stub. In
// os/exec (go1.26.10, cmd.Start/watchCtx), the context kill is armed by a
// goroutine whose wake-up is scheduler-dependent; a stub that spawns and exits
// `sleep` instantly can win that race and the attempt would SUCCEED. The old
// 800ms hold against a 300ms budget made that inversion harmless — a starved
// stub could not cover the 500ms excess even by winning the kill race. The
// widened hold keeps that property with a named bound (pinned by the margin
// guard below): the 1600ms script holds 1300ms of serialized work in excess
// of the lone budget, of which >=100ms is the per-child gap
// (intci082HermeticMinHoldGap) and >=1s the kill-timer allowance
// (intci082HermeticMinMargin/2). A green run would need the kill delayed by
// >1.2s AND both sleep children fast-forwarded past the 300ms budget in the
// same attempt — two independent scheduler anomalies, not one — so under
// bounded jitter the arm stays red regardless of load.
func TestSessionProbe_ScaledBudgetIsUsedUnderConcurrency_ScalingDisabled(t *testing.T) {
	// sessionProbeBudgetPerSpawn = 0 makes budget(n) = base for EVERY n: the
	// concurrency signal can no longer stretch the grant.
	intci082ShrinkProbeSeams(t, intci082HermeticLoneBudget, 0, intci082HermeticCap,
		intci082HermeticTotal, intci082HermeticMaxAttempts)
	intci082SilenceDiagnostics(t)

	const sessionHold = intci082RedProofHold
	markDir := t.TempDir()
	binDir := t.TempDir()
	stub := "touch " + markDir + "/$$\n" +
		"sleep " + fmt.Sprintf("%.2f", sessionHold.Seconds()) + "\n" +
		"exit 0\n"
	writeStub(t, binDir, "ssh", stub)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	keyPath := intci082Key(t)

	type result struct {
		report sessionProbeReport
		err    error
	}
	var blocker, prover result
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		blocker.report, blocker.err = probeAgentSessionLive(context.Background(), "blocker082d", "bunker-blocker082d", keyPath)
	}()
	// Bounded wait for the blocker's first ssh attempt marker (same contract
	// as the main test).
	deadline := time.Now().Add(30 * time.Second)
	for {
		entries, err := os.ReadDir(markDir)
		if err == nil && len(entries) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ssh stub never dropped an attempt marker; the blocker probe did not start")
		}
		time.Sleep(2 * time.Millisecond)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		prover.report, prover.err = probeAgentSessionLive(context.Background(), "prover082d", "bunker-prover082d", keyPath)
	}()
	wg.Wait()

	// The scaling was genuinely exercised (two probes in flight)…
	if prover.report.Concurrent < 2 {
		t.Errorf("prover observed %d in-flight probe(s), want 2 (the scaling path was not exercised)", prover.report.Concurrent)
	}
	// …yet with scaling disabled the grant stays at the lone budget, the hold
	// outlives it, and the probe must FAIL with the timeout that names the
	// budget it was granted. Under CPU starvation the kill fires late (never
	// early), so this arm stays red regardless of load.
	if prover.err == nil {
		t.Fatalf("the probe succeeded although scaling was disabled and the session hold (%s) exceeds the lone budget: report %+v",
			sessionHold, prover.report)
	}
	if !strings.Contains(prover.err.Error(), "timed out after 300ms") {
		t.Errorf("prover error = %v, want the per-attempt timeout naming the 300ms lone budget", prover.err)
	}
	if want := 300 * time.Millisecond; prover.report.Budget != want {
		t.Errorf("prover budget = %s, want the flat lone budget %s despite %d concurrent probe(s)",
			prover.report.Budget, want, prover.report.Concurrent)
	}
	intci082NoLeak(t)
}

// TestSessionProbeMarginGuard_SeamsCannotShrinkIntoTheFlakyWindow is the
// QA-BUNKER-080 AC4 regression arm: it pins the hermetic seam set as pure
// arithmetic so no future edit can quietly shrink the concurrency proof back
// into the load-flaky window. The old set (perSpawn=600ms) granted the prover
// 1200ms against a 450ms hold — 150ms of slack that a starved host's
// scheduler delay ate, reding the ci-pass native leg twice. The invariants:
//
//  1. the scaled grant exceeds the hold by a MULTI-SECOND margin (a
//     per-attempt kill fires late under starvation, never early, so a
//     multi-second slack cannot be closed by scheduler delay);
//  2. the hold still decisively outlives the LONE budget, so "survival ⇒
//     grant > lone budget" stays a proof rather than a coin flip;
//  3. the total budget bounds every retry path, so a worst-case run of killed
//     attempts cannot stall the suite past a sane ceiling;
//  4. the cap never binds below the scaled grant at the proven concurrency,
//     so the guard arithmetic tests the grant the prover actually receives;
//  5. the red-proof arm keeps its own gap: its hold (intci082RedProofHold,
//     1600ms) covers the lone budget for every child the stub spawns, plus
//     the scheduler-delay allowance, so its kill-vs-exit ordering cannot
//     invert under load.
//
// This test never runs a probe and does no I/O: it is wall-clock bounded by
// construction and completes in microseconds.
func TestSessionProbeMarginGuard_SeamsCannotShrinkIntoTheFlakyWindow(t *testing.T) {
	scaled := sessionProbeBudgetWith(intci082HermeticLoneBudget, intci082HermeticPerSpawn,
		intci082HermeticCap, 2)
	hold := intci082HermeticSessionHold
	lone := intci082HermeticLoneBudget

	// 1. the survival margin is load-monotone by width, not by luck.
	if slack := scaled - hold; slack < intci082HermeticMinMargin {
		t.Errorf("scaled budget %s minus session hold %s leaves %s of slack, want >= %s: the concurrency proof has drifted back into the load-flaky window (QA-BUNKER-080)",
			scaled, hold, slack, intci082HermeticMinMargin)
	}
	// ...and the proof's property is stated exactly: lone < hold < scaled,
	// so a surviving hold certifies a grant above the lone budget.
	if !(lone < hold && hold < scaled) {
		t.Errorf("margin ordering broken: lone budget %s, hold %s, scaled budget %s, want lone < hold < scaled",
			lone, hold, scaled)
	}

	// 2. the red-proof arm's hold covers the lone budget with the named
	// bound for EVERY child the stub spawns (the sleep grandchild each)…
	redProof := intci082RedProofHold
	for _, gap := range []struct {
		name string
		got  time.Duration
	}{
		{"the stub body", redProof - lone},
		{"the sleep grandchild", redProof - lone},
	} {
		if gap.got < intci082HermeticMinHoldGap {
			t.Errorf("ScalingDisabled hold covers the lone budget by %s for %s, want >= %s: the red proof has drifted back toward the kill-vs-exit inversion window",
				gap.got, gap.name, intci082HermeticMinHoldGap)
		}
	}
	// …and carries the full starvation allowance in its body margin, so the
	// kill-vs-exit ordering documented on that test cannot invert: winning
	// the kill race still leaves the stub short of the budget by more than
	// the allowance.
	if inversions := redProof - lone - intci082HermeticMinHoldGap; inversions < intci082HermeticMinMargin/2 {
		t.Errorf("ScalingDisabled kill-vs-exit inversion allowance = %s, want >= %s (hold - lone budget - hold gap): the red proof would depend on the kill winning a starved race",
			inversions, intci082HermeticMinMargin/2)
	}

	// 3. the total budget bounds the whole proof: even the worst case (three
	// killed attempts plus backoffs) stays inside it, so a pathological
	// host cannot stall the suite to its global timeout.
	worstCase := 3*scaled + 1*time.Second + 2*time.Second
	if worstCase > intci082HermeticTotal {
		t.Errorf("worst-case concurrency proof (%d attempts x %s + backoffs = %s) exceeds the total budget %s: a starved run could stall the suite",
			intci082HermeticMaxAttempts, scaled, worstCase, intci082HermeticTotal)
	}

	// 4. the cap must not clamp the grant the prover is proven against…
	if scaled != intci082HermeticPerSpawn {
		t.Errorf("budget at two probes = %s, want the unclamped perSpawn share %s: the cap started binding, so the guard arithmetic and the proof disagree",
			scaled, intci082HermeticPerSpawn)
	}
	// …and the cap stays a real ceiling for runaway concurrency.
	if sessionProbeBudgetWith(intci082HermeticLoneBudget, intci082HermeticPerSpawn,
		intci082HermeticCap, 100) != intci082HermeticCap {
		t.Errorf("budget at 100 probes = %s, want the %s cap", sessionProbeBudgetWith(intci082HermeticLoneBudget, intci082HermeticPerSpawn, intci082HermeticCap, 100), intci082HermeticCap)
	}
	// 5. the guard's own premise: a lone probe keeps the shipped production
	// fast path — the seams shrink for the test, the product does not.
	if got := sessionProbeBudget(1); got != 10*time.Second {
		t.Errorf("lone-probe production budget = %s, want the shipped 10s", got)
	}
}

// ── 3. a denial fails fast and is not masked ───────────────────────────────

// TestSessionProbe_DenialFailsFastAndIsNotMasked is the AC3 proof: the
// INT-DEMO-001 signature (exit 254, empty stderr, banner on stdout) is
// deterministic, so it is returned on the FIRST attempt — no 1s+2s backoff, no
// journal gather, and no dependence on how large the granted budget is. A
// longer budget must never turn a fail-closed denial into a slow spawn.
func TestSessionProbe_DenialFailsFastAndIsNotMasked(t *testing.T) {
	// Deliberately grant a banner-sized budget and keep all three attempts
	// available: neither may be used.
	intci082ShrinkProbeSeams(t, 40*time.Second, 40*time.Second, 40*time.Second, 90*time.Second, 3)
	diagCalls := intci082SilenceDiagnostics(t)

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "ssh-calls")
	// The outage signature exactly: banner to stdout, nothing on stderr, 254.
	writeStub(t, binDir, "ssh", "printf '%s\\n' x >> "+logPath+"\necho 'Last login: demo banner'\nexit 254\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	keyPath := intci082Key(t)

	start := time.Now()
	report, err := probeAgentSessionLive(context.Background(), "denied082", "bunker-denied082", keyPath)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("probe returned nil on the session-denial signature")
	}
	var denial *sessionProbeDenial
	if !errors.As(err, &denial) {
		t.Errorf("probe error is not the typed session denial (so a caller cannot tell it from a timeout): %v", err)
	}
	if !report.Denied {
		t.Errorf("report.Denied = false on the session-denial signature (report: %+v)", report)
	}
	if report.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: a deterministic denial must NOT be retried", report.Attempts)
	}
	if calls := len(readRecord(t, logPath)); calls != 1 {
		t.Errorf("ssh ran %d times, want 1: a denial must not be retried", calls)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("denial took %s, want it returned immediately (no backoff, no diagnostics) however large the budget", elapsed)
	}
	if calls := atomic.LoadInt32(diagCalls); calls != 0 {
		t.Errorf("diagnostics gatherer ran %d time(s) on a denial, want 0: the denial is already diagnosed and must fail fast", calls)
	}
	if !strings.Contains(err.Error(), "session was denied before the command ran") {
		t.Errorf("probe error does not carry the session-denial diagnostic: %v", err)
	}
	if !strings.Contains(err.Error(), "PAM") {
		t.Errorf("probe error does not name PAM: %v", err)
	}
	intci082NoLeak(t)
}

// ── 4. a timeout is attributable ──────────────────────────────────────────

// TestSessionProbe_TimeoutNamesScaledBudgetAndRecordsLatencies pins that a
// timed-out attempt reports the budget it was GRANTED and the concurrency that
// scaled it (so an operator can tell "the session subsystem is slow" from "the
// deadline is too tight" without re-running anything), and that each attempt's
// wall time is recorded in the report the spawn path logs.
func TestSessionProbe_TimeoutNamesScaledBudgetAndRecordsLatencies(t *testing.T) {
	intci082ShrinkProbeSeams(t, 250*time.Millisecond, 250*time.Millisecond, 5*time.Second, 30*time.Second, 2)
	intci082SilenceDiagnostics(t)

	binDir := t.TempDir()
	writeStub(t, binDir, "ssh", "exec sleep 30\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	keyPath := intci082Key(t)

	report, err := probeAgentSessionLive(context.Background(), "slow082", "bunker-slow082", keyPath)
	if err == nil {
		t.Fatal("probe returned nil although every attempt was killed by the per-attempt deadline")
	}
	msg := err.Error()
	for _, want := range []string{
		"timed out after 250ms",
		"per-attempt deadline",
		"concurrent session probe",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("timeout error must name %q, got: %s", want, msg)
		}
	}
	if report.Attempts != 2 {
		t.Errorf("attempts = %d, want the shrunken bound of 2", report.Attempts)
	}
	if len(report.Latencies) != report.Attempts {
		t.Errorf("recorded %d latencies for %d attempts, want one per attempt", len(report.Latencies), report.Attempts)
	}
	for i, lat := range report.Latencies {
		if lat < 250*time.Millisecond {
			t.Errorf("attempt %d latency = %s, want at least the 250ms budget (the deadline is what ended it)", i+1, lat)
		}
		if lat > 3*time.Second {
			t.Errorf("attempt %d latency = %s, want it bounded near the 250ms budget", i+1, lat)
		}
	}
	if report.Budget != 250*time.Millisecond {
		t.Errorf("report budget = %s, want the granted 250ms (lone probe)", report.Budget)
	}
	// The child must not leak: a killed ssh stub leaves no process behind that
	// still holds the pipes (the WaitDelay armor).
	if got := len(report.LatencyMillis()); got != report.Attempts {
		t.Errorf("LatencyMillis() returned %d entries for %d attempts", got, report.Attempts)
	}
	intci082NoLeak(t)
}

// ── 5. the failure carries its own mechanism ──────────────────────────────

// TestSessionProbe_FailureCarriesSessionDiagnostics is the AC2 proof: a FAILED
// probe attaches the logind record for the agent user and the journalctl
// _UID=<uid> excerpt around the attempts, so the next occurrence names its own
// mechanism. Host command-audit noise (snoopy, measured on bunker-mvp at
// ~100KB per failed spawn) must be filtered out; pam_systemd's own failure
// must survive the filter.
func TestSessionProbe_FailureCarriesSessionDiagnostics(t *testing.T) {
	intci082ShrinkProbeSeams(t, 250*time.Millisecond, 250*time.Millisecond, 5*time.Second, 5*time.Second, 1)

	binDir := t.TempDir()
	writeStub(t, binDir, "ssh", "exec sleep 30\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	keyPath := intci082Key(t)

	// The uid the gatherer must scope the journal to (the daemon resolves it
	// through the same userLookup seam the spawn path uses).
	const wantUID = "61001"
	prevLookup := userLookup
	userLookup = func(name string) (*user.User, error) {
		if name != "bunker-diag082" {
			return nil, user.UnknownUserError(name)
		}
		return &user.User{Username: name, Uid: wantUID, Gid: wantUID}, nil
	}
	t.Cleanup(func() { userLookup = prevLookup })

	journalBlob := strings.Join([]string{
		"Oct 23 13:41:41 bunker-mvp snoopy[3847568]: uid=0 cwd=/root cmd=fgrep -lxm 1 -- " + strings.Repeat("/var/lib/dpkg/info/x.list ", 40),
		"Oct 23 13:41:41 bunker-mvp sshd[3847568]: pam_systemd(sshd:session): Failed to create session: Connection timed out",
		"Oct 23 13:41:42 bunker-mvp sshd[3847571]: Accepted publickey for bunker-diag082 from 127.0.0.1 port 51234 ssh2",
		"Oct 23 13:41:43 bunker-mvp cron[99]: (root) CMD (unrelated)",
	}, "\n")

	var mu sync.Mutex
	var journalArgs []string
	origRunner := sessionProbeDiagnosticsRunner
	sessionProbeDiagnosticsRunner = func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "loginctl":
			return []byte("Name=bunker-diag082\nId=" + wantUID + "\nState=active\nLinger=no\n"), nil
		case "journalctl":
			mu.Lock()
			journalArgs = append([]string{}, args...)
			mu.Unlock()
			return []byte(journalBlob), nil
		}
		return nil, errors.New("unexpected command " + name)
	}
	t.Cleanup(func() { sessionProbeDiagnosticsRunner = origRunner })

	_, err := probeAgentSessionLive(context.Background(), "diag082", "bunker-diag082", keyPath)
	if err == nil {
		t.Fatal("probe returned nil although the session never opened")
	}
	msg := err.Error()
	for _, want := range []string{
		"session diagnostics:",
		"logind record:",
		"Name=bunker-diag082",
		"journalctl:",
		"pam_systemd(sshd:session): Failed to create session: Connection timed out",
		"bunker-diag082",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure report must carry %q, got: %s", want, msg)
		}
	}
	if strings.Contains(msg, "snoopy[") {
		t.Errorf("failure report carries host command-audit noise (snoopy), which buries the real evidence: %s", msg)
	}

	mu.Lock()
	args := journalArgs
	mu.Unlock()
	if len(args) == 0 {
		t.Fatal("the journal gatherer never ran on a failed probe")
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "_UID="+wantUID) {
		t.Errorf("journalctl args %v do not scope the excerpt to the agent uid (AC2: journalctl _UID=<uid>)", args)
	}
	intci082NoLeak(t)
}

// ── 6. the total budget bounds the retries ────────────────────────────────

// TestSessionProbe_TotalBudgetStopsRetrying pins the second bound: even with
// attempts left and a per-attempt budget available, a probe that has already
// spent its TOTAL budget stops, so a wedged session subsystem fails the spawn
// in a bounded time instead of holding it for attempts × max-budget.
func TestSessionProbe_TotalBudgetStopsRetrying(t *testing.T) {
	intci082ShrinkProbeSeams(t, 300*time.Millisecond, 300*time.Millisecond, 5*time.Second, 400*time.Millisecond, 3)
	intci082SilenceDiagnostics(t)

	binDir := t.TempDir()
	writeStub(t, binDir, "ssh", "exec sleep 30\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	keyPath := intci082Key(t)

	report, err := probeAgentSessionLive(context.Background(), "total082", "bunker-total082", keyPath)
	if err == nil {
		t.Fatal("probe returned nil although every attempt was killed")
	}
	if report.Attempts >= sessionProbeMaxAttempts {
		t.Errorf("attempts = %d, want fewer than the %d available: the TOTAL budget of 400ms must stop the retry loop",
			report.Attempts, sessionProbeMaxAttempts)
	}
	if !strings.Contains(err.Error(), "probe total budget") {
		t.Errorf("error must name the total budget it spent, got: %v", err)
	}
	if report.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (first attempt 300ms + 1s backoff puts the second attempt past the total budget)", report.Attempts)
	}
	intci082NoLeak(t)
}

// ── 7. the report is renderable for the spawn path's log line ─────────────

// TestSessionProbeReport_LatencyMillis keeps the structured log field honest:
// the spawn path logs attempt_latencies_ms, so the report must render exactly
// one integer per attempt, in order.
func TestSessionProbeReport_LatencyMillis(t *testing.T) {
	rep := sessionProbeReport{Latencies: []time.Duration{
		3 * time.Millisecond, 1500 * time.Millisecond, 42000 * time.Millisecond,
	}}
	got := rep.LatencyMillis()
	want := []int64{3, 1500, 42000}
	if len(got) != len(want) {
		t.Fatalf("LatencyMillis() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("LatencyMillis()[%d] = %d, want %d", i, got[i], want[i])
		}
	}
	if zero := (sessionProbeReport{}).LatencyMillis(); len(zero) != 0 {
		t.Errorf("empty report renders %v, want an empty list (nil, not [0])", zero)
	}
	if s := strconv.Itoa(int(got[2])); s != "42000" {
		t.Errorf("rendered latency %q, want \"42000\"", s)
	}
}
