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
// milliseconds; a slow stub (~500ms) exceeds every attempt budget.
//
// INT-CI-049 regression test (this file) under fleet-build-host load: the
// stub's per-invocation log line is written by its FIRST instruction, so a
// helper that is still starting — fork+exec of /bin/sh on a box whose load
// average sits above 40 — has logged nothing when its 100 ms attempt budget
// expires, and the exhausted run legitimately shows fewer log lines than
// attempts. The BUDGET (elapsed ≪ shared-deadline shape) and the DEADLINE
// ERROR SHAPE are what this test exists to pin, and both are observable in
// the returned error alone; the stub log is the count of STARTS, not of
// attempts dispatched, so on a loaded host the test measures the budget
// against the error and keeps the log only as a loud upper bound — an
// invocation count ABOVE the attempt budget would mean the loop is not
// bounded at all, and still fails here.
func TestCountAgentContainersPerAttemptBudgetIsBounded(t *testing.T) {
	origTimeout, origWait := containerCapAttemptTimeout, containerCapAttemptWaitBase
	containerCapAttemptTimeout = 100 * time.Millisecond
	containerCapAttemptWaitBase = 10 * time.Millisecond
	t.Cleanup(func() {
		containerCapAttemptTimeout, containerCapAttemptWaitBase = origTimeout, origWait
	})

	pathVar, logPath := writeDockerStub(t, `sleep 0.5
printf 'ctr1\n'`)
	t.Setenv("PATH", pathVar)

	start := time.Now()
	_, err := countAgentContainers(context.Background(), "/run/bunker/intci049/docker.sock")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("every attempt exceeding its budget must fail the count")
	}
	if n := countLogLines(t, logPath); n > containerCapAttempts {
		t.Fatalf("helper invocations = %d, want <= %d: the retry loop is not bounded — a slow helper must be abandoned by its per-attempt budget, never allowed to start more attempts", n, containerCapAttempts)
	}
	// The shared-deadline shape, detected where the stub log cannot see it: with
	// ONE deadline shared across attempts, attempt 1 consumes the whole budget
	// and every later attempt starts on an already-expired context — the helper
	// is never started, so the stub log undercounts and the total collapses to
	// roughly one budget. Honest per-attempt exhaustion can never be FASTER
	// than attempts×budget (+ backoffs): each attempt's kill timer is its own
	// and cannot fire early. Half the floor leaves room for attempts killed
	// early by outside pressure without letting a shared deadline through.
	if min := time.Duration(containerCapAttempts)*containerCapAttemptTimeout/2 + time.Duration(containerCapAttempts-1)*containerCapAttemptWaitBase; elapsed < min {
		t.Fatalf("exhaustion took %s, below attempts×budget/2 (%s): the attempts did not each consume their own bounded context (shared-deadline shape)", elapsed, min)
	}
	if elapsed > 5*time.Second {
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
