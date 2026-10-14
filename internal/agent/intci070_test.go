package agent

// INT-CI-070 regression tests for the dockerd readiness budget.
//
// CI run 2026-10-14T12:40:27Z (the root-gated suite on main, the close of
// INT-CI-069) failed TestConcurrency_SpawnFiveAgents at stage dockerd-start:
// five concurrent rootless dockerd starts contended for CPU, one unit was
// ACTIVE for 29s before its socket appeared, and waitForDockerd granted every
// start the same flat 5s socket deadline. The spawn then failed — and tore down
// an agent whose daemon was coming up fine.
//
// These tests drive the REAL waitForDockerd. The dockerdProcessChecker seam
// stands in for the host's process table and the socket is a file in a temp dir,
// so no root, no systemd and no docker are involved. They pin five properties:
//
//  1. the readiness budget scales with the concurrent starts in flight and is
//     hard-capped (TestDockerdReadyBudget_ScaledToConcurrency);
//  2. the fail-fast process grace scales too and never exceeds the budget
//     (TestDockerdReadyProcessGrace_ScalesAndNeverExceedsBudget);
//  3. a socket that appears AFTER the legacy 5s deadline still succeeds, and the
//     wait polled repeatedly while it waited
//     (TestWaitForDockerd_SocketAppearsAfterLegacyDeadline — the CI
//     fingerprint; it fails against the pre-fix flat deadline);
//  4. the scaling is really WIRED into the wait: three overlapping waits share
//     the scaled budget (TestWaitForDockerd_BudgetScalesWithConcurrentWaits);
//  5. the two failure axes stay distinguishable and bounded — a socket that
//     never appears fails CLOSED naming the unit and the elapsed time, while a
//     dockerd PROCESS that never appears fails FAST instead of waiting out the
//     socket budget, and a cancelled caller still returns promptly.
//
// AC map:
//   - budget scaled to concurrency + hard cap      -> TestDockerdReadyBudget_ScaledToConcurrency
//   - fail-fast window scaled, never past budget   -> TestDockerdReadyProcessGrace_ScalesAndNeverExceedsBudget
//   - slow socket past the legacy deadline passes  -> TestWaitForDockerd_SocketAppearsAfterLegacyDeadline
//   - scaled budget actually used by the wait      -> TestWaitForDockerd_BudgetScalesWithConcurrentWaits
//   - never-coming socket fails with unit+elapsed  -> TestWaitForDockerd_SocketNeverAppearsNamesUnitAndElapsed
//   - never-coming PROCESS fails fast              -> TestWaitForDockerd_ProcessNeverAppearsFailsFast
//   - cancelled spawn returns promptly, no leak    -> TestWaitForDockerd_HonoursCallerContext

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func intci070Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// intci070ShrinkBudgets shrinks the readiness seams so the wait can be driven in
// milliseconds instead of tens of seconds. Cleanup restores every seam, so a
// test can never leak a shortened budget into the next one.
func intci070ShrinkBudgets(t *testing.T, base, perStart, max, graceBase, gracePerStart, poll time.Duration) {
	t.Helper()
	ob, op, om := dockerdReadyBudgetBase, dockerdReadyBudgetPerStart, dockerdReadyBudgetMax
	ogb, ogp, oi, od := dockerdReadyProcessGraceBase, dockerdReadyProcessGracePerStart, dockerdReadyPollInterval, dockerdUnitDiagnosticsTimeout
	t.Cleanup(func() {
		dockerdReadyBudgetBase, dockerdReadyBudgetPerStart, dockerdReadyBudgetMax = ob, op, om
		dockerdReadyProcessGraceBase, dockerdReadyProcessGracePerStart = ogb, ogp
		dockerdReadyPollInterval, dockerdUnitDiagnosticsTimeout = oi, od
	})
	dockerdReadyBudgetBase, dockerdReadyBudgetPerStart, dockerdReadyBudgetMax = base, perStart, max
	dockerdReadyProcessGraceBase, dockerdReadyProcessGracePerStart = graceBase, gracePerStart
	dockerdReadyPollInterval = poll
	// The systemctl/journalctl attribution is a real exec on the failure paths;
	// keep it from dominating a millisecond-scale test.
	dockerdUnitDiagnosticsTimeout = time.Second
}

// intci070StubChecker installs the process-checker seam. answer is consulted on
// every poll, so a test can model a process that appears late; calls counts the
// polls, which is how a test proves the wait RETRIED rather than giving up after
// one look; peak records the highest number of readiness waits observed in
// flight, which is the contention the budget is supposed to answer to.
func intci070StubChecker(t *testing.T, answer func() bool) (calls, peak *int32) {
	t.Helper()
	var n, p int32
	calls, peak = &n, &p
	orig := dockerdProcessChecker
	dockerdProcessChecker = func(context.Context, string) (bool, error) {
		atomic.AddInt32(calls, 1)
		for {
			cur := atomic.LoadInt32(peak)
			active := int32(dockerdReadyWaitsActive())
			if active <= cur || atomic.CompareAndSwapInt32(peak, cur, active) {
				break
			}
		}
		return answer(), nil
	}
	t.Cleanup(func() { dockerdProcessChecker = orig })
	return calls, peak
}

// intci070NoWaitLeak asserts the in-flight counter is released: a wait that
// returned without deregistering itself would inflate every later budget on the
// host for the life of the process.
func intci070NoWaitLeak(t *testing.T) {
	t.Helper()
	if got := dockerdReadyWaitsActive(); got != 0 {
		t.Errorf("in-flight readiness waits = %d after the wait returned, want 0 (the counter must be released)", got)
	}
}

// ── the budget / grace arithmetic ──────────────────────────────────────────

// TestDockerdReadyBudget_ScaledToConcurrency pins the production budget table.
// The defaults are deliberately left in force: they are the numbers the fix
// ships, and the 29s lag CI observed at five concurrent starts is the datum the
// table has to clear.
func TestDockerdReadyBudget_ScaledToConcurrency(t *testing.T) {
	tests := []struct {
		inFlight int
		want     time.Duration
		why      string
	}{
		{0, 30 * time.Second, "an empty signal is treated as a lone start"},
		{1, 30 * time.Second, "a lone start keeps the fast path"},
		{2, 30 * time.Second, "two starts are still inside the base floor"},
		{3, 30 * time.Second, "three starts are still inside the base floor"},
		{4, 40 * time.Second, "the per-start share overtakes the base"},
		{5, 50 * time.Second, "the CI shape: five concurrent starts"},
		{12, 120 * time.Second, "the hard cap"},
		{13, 120 * time.Second, "the cap holds just above it"},
		{1000, 120 * time.Second, "the cap holds for a runaway count"},
	}
	for _, tc := range tests {
		if got := dockerdReadyBudget(tc.inFlight); got != tc.want {
			t.Errorf("dockerdReadyBudget(%d) = %s, want %s (%s)", tc.inFlight, got, tc.want, tc.why)
		}
	}

	// The datum this fix answers to: the socket lag CI observed under five
	// concurrent starts must sit INSIDE the granted budget, not on its edge.
	const observedLag = 29 * time.Second
	if got := dockerdReadyBudget(5); got <= observedLag {
		t.Errorf("budget at five concurrent starts = %s, want more than the %s lag CI observed", got, observedLag)
	}
	// ...and the flat legacy deadline must be gone for a lone start too.
	if got := dockerdReadyBudget(1); got <= 5*time.Second {
		t.Errorf("lone-start budget = %s, want more than the legacy 5s socket deadline", got)
	}
}

// TestDockerdReadyProcessGrace_ScalesAndNeverExceedsBudget pins the fail-fast
// window: it keeps the legacy 5s for a lone start (so a genuinely broken daemon
// still reports as fast as it did before) and is always carved out of the
// readiness budget rather than past it.
func TestDockerdReadyProcessGrace_ScalesAndNeverExceedsBudget(t *testing.T) {
	tests := []struct {
		inFlight int
		want     time.Duration
	}{
		{0, 5 * time.Second},
		{1, 5 * time.Second}, // the legacy deadline, unchanged for a lone start
		{2, 7 * time.Second},
		{5, 13 * time.Second},
	}
	for _, tc := range tests {
		if got := dockerdReadyProcessGrace(tc.inFlight); got != tc.want {
			t.Errorf("dockerdReadyProcessGrace(%d) = %s, want %s", tc.inFlight, got, tc.want)
		}
	}
	for _, n := range []int{1, 2, 5, 50, 1000} {
		grace, budget := dockerdReadyProcessGrace(n), dockerdReadyBudget(n)
		if grace > budget {
			t.Errorf("grace at %d concurrent starts = %s, want <= the readiness budget %s", n, grace, budget)
		}
	}

	// With a shrunken budget the grace is clamped to it — the two seams compose
	// instead of the grace outliving its budget.
	intci070ShrinkBudgets(t, 2*time.Second, time.Second, 2*time.Second, 5*time.Second, 2*time.Second, 10*time.Millisecond)
	if got := dockerdReadyBudget(5); got != 2*time.Second {
		t.Fatalf("shrunken budget at 5 starts = %s, want the 2s cap", got)
	}
	if got := dockerdReadyProcessGrace(5); got != 2*time.Second {
		t.Errorf("grace at 5 starts under a 2s cap = %s, want the grace clamped to the budget", got)
	}
}

// ── the CI fingerprint: a socket that lands past the legacy deadline ────────

// TestWaitForDockerd_SocketAppearsAfterLegacyDeadline is the regression for CI
// run 2026-10-14T12:40:27Z. The production defaults are in force and the socket
// lands at 6s — past the 5s flat deadline the pre-fix code used, and well inside
// the granted budget. The wait must hold on, polling, instead of failing a
// daemon that is coming up.
func TestWaitForDockerd_SocketAppearsAfterLegacyDeadline(t *testing.T) {
	const lag = 6 * time.Second
	calls, _ := intci070StubChecker(t, func() bool { return true })

	sockPath := filepath.Join(t.TempDir(), "docker.sock")
	go func() {
		time.Sleep(lag)
		_ = os.WriteFile(sockPath, []byte(""), 0o644)
	}()

	start := time.Now()
	err := waitForDockerd(context.Background(), "testuser", sockPath, sockPath+".actual", "bunker-docker-intci070", intci070Logger())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("a socket appearing at %s must be waited for (granted budget %s), got: %v",
			lag, dockerdReadyBudget(1), err)
	}
	if elapsed < lag {
		t.Fatalf("wait returned after %s, before the socket existed at %s", elapsed, lag)
	}
	// Reaching the socket at 6s REQUIRED repeated polling: a single look at
	// t=0 (or the pre-fix flat deadline) would have given up first.
	if n := atomic.LoadInt32(calls); n < 3 {
		t.Errorf("process checks = %d, want the wait to poll repeatedly while the socket was coming up", n)
	}
	intci070NoWaitLeak(t)
}

// ── the scaling is wired into the wait ─────────────────────────────────────

// TestWaitForDockerd_BudgetScalesWithConcurrentWaits proves the concurrency
// signal reaches the real wait. With base 800ms and 400ms per concurrent start,
// three overlapping waits get max(800ms, 1200ms) = 1200ms, while a wait that
// answered only to its own start — or to a fixed deadline — would give up at
// 800ms. The starts are STAGGERED on purpose: the first wait begins alone and
// the other two join 50ms later, so the first wait's elapsed time also pins the
// LATCH (a wait that began alone must inherit the headroom of the peers that
// joined it). A base-only budget fails both assertions; a fixed deadline fails
// the first.
func TestWaitForDockerd_BudgetScalesWithConcurrentWaits(t *testing.T) {
	intci070ShrinkBudgets(t,
		800*time.Millisecond, 400*time.Millisecond, 10*time.Second, // budget
		100*time.Millisecond, 10*time.Millisecond, // process grace (well under the budget: the socket axis must be the one that fires)
		10*time.Millisecond) // poll
	_, peak := intci070StubChecker(t, func() bool { return true })

	const n = 3
	root := t.TempDir()
	errs := make([]error, n)
	elapsedOnce := make([]time.Duration, n)
	var wg sync.WaitGroup
	startedAt := make([]time.Time, n)

	run := func(i int) {
		defer wg.Done()
		sock := filepath.Join(root, fmt.Sprintf("docker-%d.sock", i))
		t0 := time.Now()
		errs[i] = waitForDockerd(context.Background(), "testuser", sock, sock+".actual", "bunker-docker-intci070", intci070Logger())
		startedAt[i], elapsedOnce[i] = t0, time.Since(t0)
	}

	wg.Add(1)
	go run(0) // the lone starter
	time.Sleep(50 * time.Millisecond)
	for i := 1; i < n; i++ {
		wg.Add(1)
		go run(i)
	}
	wg.Wait()
	groupElapsed := time.Since(startedAt[0])

	for i, err := range errs {
		if err == nil {
			t.Fatalf("wait %d: no socket ever appeared, so the wait must fail closed", i)
		}
		if !strings.Contains(err.Error(), "dockerd did not start") || !strings.Contains(err.Error(), "bunker-docker-intci070") {
			t.Errorf("wait %d error must name the failure and the unit: %v", i, err)
		}
	}
	// The waits really did overlap; without this the scaling assertions below
	// would be vacuous.
	if got := atomic.LoadInt32(peak); got < n {
		t.Fatalf("peak in-flight readiness waits = %d, want %d (the waits did not overlap, so the scaling is untested)", got, n)
	}
	if groupElapsed < 1100*time.Millisecond {
		t.Errorf("the overlapping waits gave up after %s, want the SCALED budget (~1200ms); a budget that answered only to each start's own count would have failed at 800ms", groupElapsed)
	}
	if elapsedOnce[0] < 1100*time.Millisecond {
		t.Errorf("the wait that began ALONE gave up after %s, want the latch to extend it to the scaled budget (~1200ms) once its peers joined", elapsedOnce[0])
	}
	if groupElapsed > 6*time.Second {
		t.Errorf("the overlapping waits took %s, want the scaled budget bounded", groupElapsed)
	}
	intci070NoWaitLeak(t)
}

// ── the two failure axes ───────────────────────────────────────────────────

// TestWaitForDockerd_SocketNeverAppearsNamesUnitAndElapsed keeps the fail-CLOSED
// contract: a daemon that is running but never produces its socket must still
// fail the spawn, and the error must let an operator act without the CI log —
// it names the systemd unit, the socket, the elapsed time and the budget.
func TestWaitForDockerd_SocketNeverAppearsNamesUnitAndElapsed(t *testing.T) {
	intci070ShrinkBudgets(t,
		400*time.Millisecond, 300*time.Millisecond, 10*time.Second,
		100*time.Millisecond, 10*time.Millisecond,
		10*time.Millisecond)
	intci070StubChecker(t, func() bool { return true })

	sockPath := filepath.Join(t.TempDir(), "docker.sock")
	start := time.Now()
	err := waitForDockerd(context.Background(), "testuser", sockPath, sockPath+".actual", "bunker-docker-intci070", intci070Logger())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a socket that never appears must fail the spawn (enforcement stays fail-closed)")
	}
	msg := err.Error()
	for _, want := range []string{
		"dockerd did not start",
		"bunker-docker-intci070", // the unit
		sockPath,                 // the socket that was looked for
		"elapsed",                // how long the wait actually spent
		"readiness budget",
		"concurrent dockerd start",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must name %q, got: %s", want, msg)
		}
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("wait gave up after %s, want the granted budget honoured", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("wait took %s, want it bounded by its budget", elapsed)
	}
	intci070NoWaitLeak(t)
}

// TestWaitForDockerd_ProcessNeverAppearsFailsFast is the other axis: a unit that
// produces NO dockerd process at all is broken (missing AppArmor profile,
// unmappable subuids, systemd refusing the unit), and no socket can appear for a
// daemon that is not running — so the spawn must fail on the short process grace
// instead of waiting out a long socket budget. The budget here is deliberately
// 30s: the failure must come from the grace, not from the budget expiring.
func TestWaitForDockerd_ProcessNeverAppearsFailsFast(t *testing.T) {
	intci070ShrinkBudgets(t,
		30*time.Second, time.Second, 30*time.Second, // a long socket budget
		250*time.Millisecond, 10*time.Millisecond, // a short process grace
		10*time.Millisecond)
	intci070StubChecker(t, func() bool { return false })

	sockPath := filepath.Join(t.TempDir(), "docker.sock")
	start := time.Now()
	err := waitForDockerd(context.Background(), "testuser", sockPath, sockPath+".actual", "bunker-docker-intci070", intci070Logger())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a unit that never produces a dockerd process must fail the spawn")
	}
	msg := err.Error()
	for _, want := range []string{
		"dockerd did not start",
		"bunker-docker-intci070",
		"elapsed",
		"no dockerd process appeared",
		"process grace",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must name %q, got: %s", want, msg)
		}
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("wait took %s, i.e. it waited out the socket budget instead of failing fast on the process grace", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("fail-fast took %s, want it bounded by the 250ms process grace", elapsed)
	}
	intci070NoWaitLeak(t)
}

// TestWaitForDockerd_HonoursCallerContext pins that the longer budget did not
// make the wait deaf to its caller: a spawn whose context dies must return
// promptly (no hang, no leaked in-flight count) — and the diagnostics collected
// on the way out still run, because attribution is detached from the caller.
func TestWaitForDockerd_HonoursCallerContext(t *testing.T) {
	intci070ShrinkBudgets(t,
		30*time.Second, time.Second, 30*time.Second,
		30*time.Second, time.Second,
		10*time.Millisecond)
	intci070StubChecker(t, func() bool { return true })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	sockPath := filepath.Join(t.TempDir(), "docker.sock")
	start := time.Now()
	err := waitForDockerd(ctx, "testuser", sockPath, sockPath+".actual", "bunker-docker-intci070", intci070Logger())
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want the caller's cancellation reported, got: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("cancelled wait took %s, want it to return promptly", elapsed)
	}
	intci070NoWaitLeak(t)
}
