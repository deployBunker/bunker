package agent

// INT-CI-049 regression tests for the container-cap docker ps count.
//
// CI run 36378260696 (head 9cb4df6) failed TestConcurrency_SpawnFiveAgents
// 4/5 at stage container-cap with `docker ps: signal: killed (output: )`:
// under five-way concurrent spawn on the self-hosted runner, the count
// helpers contend with the just-started rootless daemons and the runner's
// OOM/pid pressure SIGKILLs them before they print. The pre-fix code ran
// exactly one helper, on the caller's context, and treated a killed helper
// as a daemon verdict — a transient helper death became a hard spawn
// failure.
//
// These tests drive the REAL countAgentContainers (production seam, PATH
// stub for the docker CLI — no docker daemon, no root) and pin three
// properties:
//
//  1. a helper killed by an external signal is retried and recovery counts;
//  2. once the attempt budget is exhausted the count still fails CLOSED,
//     naming the attempt budget, with the kill preserved in the chain;
//  3. a real daemon verdict (exit status with output) is never retried;
//  4. each attempt runs on its own fresh, detached, bounded context — a
//     cancelled caller must not turn the count into an instant no-op, and
//     one hung helper must not run away with the budget.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeDockerStub drops a `docker` CLI stub into a fresh dir and returns a
// PATH (dir first) plus the stub's log file. Every invocation appends one
// line to the log, so tests can count attempts without touching docker.
func writeDockerStub(t *testing.T, body string) (pathVar, logPath string) {
	t.Helper()
	binDir := t.TempDir()
	logPath = filepath.Join(t.TempDir(), "docker-stub.log")
	script := fmt.Sprintf("#!/bin/sh\nprintf 'x\\n' >> %q\n%s\n", logPath, body)
	stub := filepath.Join(binDir, "docker")
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write docker stub: %v", err)
	}
	return binDir + string(os.PathListSeparator) + os.Getenv("PATH"), logPath
}

func countLogLines(t *testing.T, logPath string) int {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stub log: %v", err)
	}
	return strings.Count(strings.TrimRight(string(b), "\n"), "\n") + 1
}

func stubLogEmpty(t *testing.T, logPath string) bool {
	t.Helper()
	_, err := os.Stat(logPath)
	return errors.Is(err, os.ErrNotExist)
}

// waitForStubStarts waits until the stub log carries want lines (one line per
// helper START — the stub logs on its first instruction) or the bounded
// deadline passes, returning the count actually observed (QA-BUNKER-54:
// observable-signal wait instead of a single sample at loop-exit).
//
// want lines are legal to be slow, never to be missing: every counted attempt
// is an attempt whose exec SUCCEEDED, and the process the exec started cannot
// be un-started by the next attempt's backoff or kill. A fully-scheduled-away
// helper does delay the wait — bounded by the same 20s budget production
// grants one docker ps invocation (containerCapAttemptTimeout), which is what
// the test shrunk the seam to, times two.
func waitForStubStarts(t *testing.T, logPath string, want int, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		n := countLogLines(t, logPath)
		if n >= want {
			return n
		}
		if time.Now().After(deadline) {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCountAgentContainersRetriesKilledHelper: the first two helper
// invocations are SIGKILLed from the outside (the CI failure shape — empty
// output, signal death); the third one answers. The count must recover and
// report the real container count. Pre-fix this failed after ONE killed
// invocation, which is exactly what CI run 36378260696 showed.
func TestCountAgentContainersRetriesKilledHelper(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "docker-stub.log")
	stub := filepath.Join(binDir, "docker")
	script := fmt.Sprintf(`#!/bin/sh
printf 'x\n' >> %q
if [ "$(wc -l < %q)" -le 2 ]; then kill -9 $$; fi
printf 'ctr1\nctr2\n'
`, logPath, logPath)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write docker stub: %v", err)
	}
	pathVar := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", pathVar)

	got, err := countAgentContainers(context.Background(), "/run/bunker/intci049/docker.sock")
	if err != nil {
		t.Fatalf("countAgentContainers should recover from transient helper kills, got error: %v", err)
	}
	if got != 2 {
		t.Fatalf("count = %d, want 2 (the third invocation's output)", got)
	}
	if n := countLogLines(t, logPath); n != 3 {
		t.Fatalf("helper invocations = %d, want 3 (two transient kills + one success)", n)
	}
}

// TestCountAgentContainersFailsClosedWhenAllAttemptsDie: when every attempt
// is killed the count must still fail (enforcement stays fail-closed), name
// the attempt budget, and preserve the signal death in the error chain.
// Pre-fix this failed with an unattributed single attempt.
func TestCountAgentContainersFailsClosedWhenAllAttemptsDie(t *testing.T) {
	pathVar, logPath := writeDockerStub(t, `kill -9 $$`)
	t.Setenv("PATH", pathVar)

	got, err := countAgentContainers(context.Background(), "/run/bunker/intci049/docker.sock")
	if err == nil {
		t.Fatalf("countAgentContainers must fail closed after %d killed attempts, got count %d", containerCapAttempts, got)
	}
	if n := countLogLines(t, logPath); n != containerCapAttempts {
		t.Fatalf("helper invocations = %d, want %d (bounded retry)", n, containerCapAttempts)
	}
	msg := err.Error()
	if !strings.Contains(msg, "signal: killed") || !strings.Contains(msg, "(output: )") {
		t.Fatalf("error must preserve the CI failure shape, got: %s", msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("after %d attempts", containerCapAttempts)) {
		t.Fatalf("error must name the attempt budget, got: %s", msg)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error chain must preserve *exec.ExitError for classification, got %T: %s", err, msg)
	}
}

// TestCountAgentContainersDoesNotRetryDaemonVerdicts: a docker CLI that
// EXITS (connection refused / missing socket) is a real daemon verdict —
// it must be reported immediately, never retried.
func TestCountAgentContainersDoesNotRetryDaemonVerdicts(t *testing.T) {
	pathVar, logPath := writeDockerStub(t, `echo "Cannot connect to the Docker daemon at unix:///run/bunker/intci049/docker.sock" >&2
exit 1`)
	t.Setenv("PATH", pathVar)

	_, err := countAgentContainers(context.Background(), "/run/bunker/intci049/docker.sock")
	if err == nil {
		t.Fatal("a real daemon error must fail the count")
	}
	if n := countLogLines(t, logPath); n != 1 {
		t.Fatalf("helper invocations = %d, want 1 (daemon verdicts are never retried)", n)
	}
	if !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Fatalf("error must carry the daemon's own message, got: %v", err)
	}
}

// TestCountAgentContainersDetachedFromCallerCancellation: the per-attempt
// context is detached (context.WithoutCancel), so a caller that already
// gave up must not turn the count into an instant no-op — the helper still
// runs and its answer still counts. Pre-fix the helper inherited the
// cancelled request context and exec refused to start it (INT-CI-005's
// rollback lesson, applied to the enforcement read).
func TestCountAgentContainersDetachedFromCallerCancellation(t *testing.T) {
	pathVar, logPath := writeDockerStub(t, `printf 'ctr1\n'`)
	t.Setenv("PATH", pathVar)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := countAgentContainers(ctx, "/run/bunker/intci049/docker.sock")
	if err != nil {
		t.Fatalf("a cancelled caller must not no-op the count, got error: %v", err)
	}
	if got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}
	if n := countLogLines(t, logPath); n != 1 {
		t.Fatalf("helper invocations = %d, want 1", n)
	}
}

// TestCountAgentContainersPerAttemptBudgetIsBounded: one hung helper cannot
// run away with the whole retry budget — each attempt runs on its own
// bounded context, so exhausted attempts still terminate the loop quickly.
// Shrinks the production seams (vars, never written outside tests) to
// milliseconds.
//
// The stub log is the count of STARTS (the stub logs on its first
// instruction), and the test reads it as an observable SIGNAL with a bounded
// wait, not as a single sample at loop-exit (QA-BUNKER-54: the single read
// raced a starved helper's first log write and failed under host load with
// "helper invocations = 2, want 3").
//
// Load-bearing premise: every attempt budget must be comfortably wider than
// a helper's worst-case START latency (fork+exec of /bin/sh on a loaded
// box), or a helper killed BEFORE its first instruction leaves no line and
// no read strategy can recover the count. The earlier 300 ms budget was
// ~150x the measured worst case on this host (0.85-1.96 ms over 300
// sequential starts) and was violated once under fleet load; 1 s is ~500x,
// and the stub's `sleep 5` stays well above the budget, so a started helper
// always dies before it can answer. The exact count is still the assertion
// that discriminates the shared-deadline shape: ONE deadline hoisted above
// the loop starts exactly ONE helper (n=1), while honest per-attempt budgets
// start one helper per attempt (n=attempts). Elapsed wall time cannot make
// that separation — both shapes are dominated by the same backoffs.
func TestCountAgentContainersPerAttemptBudgetIsBounded(t *testing.T) {
	origTimeout, origWait := containerCapAttemptTimeout, containerCapAttemptWaitBase
	containerCapAttemptTimeout = time.Second
	containerCapAttemptWaitBase = 10 * time.Millisecond
	t.Cleanup(func() {
		containerCapAttemptTimeout, containerCapAttemptWaitBase = origTimeout, origWait
	})

	// `exec` makes the sleep the process exec.Start launched, so the
	// attempt budget's kill closes the stdout pipe at once — a plain
	// `sleep 5` leaves it as a child of the killed sh, and the orphaned
	// sleep holds the pipe until IT exits, stretching every attempt to the
	// full sleep (measured: 15.05s/test vs ~3.1s).
	pathVar, logPath := writeDockerStub(t, `exec sleep 5`)
	t.Setenv("PATH", pathVar)

	start := time.Now()
	_, err := countAgentContainers(context.Background(), "/run/bunker/intci049/docker.sock")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("premise broken: the stub answered within the attempt budget (the budget's kill did not preempt it — host starvation exceeded the budget's headroom); the count must fail")
	}
	if n := waitForStubStarts(t, logPath, containerCapAttempts, 2*containerCapAttemptTimeout); n != containerCapAttempts {
		t.Fatalf("helper invocations = %d, want %d (each attempt bounded, not one shared deadline)", n, containerCapAttempts)
	}
	// Derived from the shrunken seam, not a fixed number (QA-BUNKER-54):
	// per-attempt exhaustion costs ~attempts×budget, and a CPU-starved test
	// process delivers its own kill timers late (observed ~4s/s-try under a
	// 2-CPU-pinned run at loadavg ~69), so the cap is generous (10x) — it is
	// not the shared-deadline discriminator (the COUNT above is; elapsed
	// cannot separate the shapes, both being dominated by the same
	// backoffs). Its only job: fail if a budget stops bounding at all.
	if elapsed > 10*time.Duration(containerCapAttempts)*containerCapAttemptTimeout {
		t.Fatalf("exhaustion took %s, want ≪ per-attempt-budget × attempts (shared-deadline shape)", elapsed)
	}
	msg := err.Error()
	if !strings.Contains(msg, "signal: killed") && !strings.Contains(msg, context.DeadlineExceeded.Error()) {
		t.Fatalf("exhaustion error must carry the helper's death shape, got: %s", msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("after %d attempts", containerCapAttempts)) {
		t.Fatalf("exhaustion error must name the attempt budget, got: %s", msg)
	}
}
