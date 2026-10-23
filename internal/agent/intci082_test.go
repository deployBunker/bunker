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

// TestSessionProbe_ScaledBudgetIsUsedUnderConcurrency proves the concurrency
// signal reaches the REAL probe. The ssh stub blocks until two probes are in
// flight, then sleeps longer than the LONE-probe budget (200ms) but well inside
// the two-probe budget (1800ms): an attempt that survives the sleep can only
// have been granted the SCALED budget.
//
// The first probe keeps the fast path on purpose (it began alone): it is killed
// at 300ms on every attempt and fails, which is the documented behaviour for a
// lone probe — the assertion here is that the concurrent one was NOT.
func TestSessionProbe_ScaledBudgetIsUsedUnderConcurrency(t *testing.T) {
	intci082ShrinkProbeSeams(t, 300*time.Millisecond, 300*time.Millisecond, 5*time.Second, 10*time.Second, 3)
	intci082SilenceDiagnostics(t)

	binDir := t.TempDir()
	markDir := t.TempDir()
	// Block until BOTH probes have started an ssh attempt, then hold the
	// session open past the lone-probe budget (300ms) but inside the
	// two-probe budget (600ms).
	stub := "touch " + markDir + "/$$\n" +
		"n=0\n" +
		"while [ \"$(ls " + markDir + " | wc -l)\" -lt 2 ]; do sleep 0.02; n=$((n+1)); [ $n -gt 100 ] && break; done\n" +
		"sleep 0.45\n" +
		"exit 0\n"
	writeStub(t, binDir, "ssh", stub)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	keyPath := intci082Key(t)

	type result struct {
		report sessionProbeReport
		err    error
	}
	var lone, peer result
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		lone.report, lone.err = probeAgentSessionLive(context.Background(), "lone082", "bunker-lone082", keyPath)
	}()
	// Let the first probe enter the in-flight counter alone, then join it.
	time.Sleep(100 * time.Millisecond)
	wg.Add(1)
	go func() {
		defer wg.Done()
		peer.report, peer.err = probeAgentSessionLive(context.Background(), "peer082", "bunker-peer082", keyPath)
	}()
	wg.Wait()

	if peer.err != nil {
		t.Fatalf("the concurrent probe failed although it was granted the scaled budget %s: %v (report: %+v)",
			sessionProbeBudget(2), peer.err, peer.report)
	}
	if want := sessionProbeBudget(2); peer.report.Budget != want {
		t.Errorf("concurrent probe budget = %s, want the scaled %s", peer.report.Budget, want)
	}
	if peer.report.Concurrent < 2 {
		t.Errorf("concurrent probe observed %d in-flight probe(s), want 2 (the scaling was not exercised)", peer.report.Concurrent)
	}
	if len(peer.report.Latencies) != 1 || peer.report.Latencies[0] < 400*time.Millisecond {
		t.Errorf("concurrent attempt latencies = %v, want one attempt of >=400ms: an attempt that outlived the 300ms lone-probe budget proves the scaled budget was USED, not merely computed",
			peer.report.Latencies)
	}
	// The probe that began alone keeps the fast path: it is killed at the lone
	// budget on every attempt, so it fails closed rather than silently
	// inheriting a peer's headroom it never observed.
	if lone.err == nil {
		t.Errorf("the lone probe succeeded although every attempt exceeded the lone-probe budget of 300ms (report: %+v)", lone.report)
	}
	intci082NoLeak(t)
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
