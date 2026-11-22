package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// ── INT-CI-092 proofs: the probe budget under a staggered concurrent-spawn
// wave (hosted run 43789255859, d1403505, job 133043927715). ──

// resetSessionProbeContention zeroes the contention state the session-probe
// budget scales with — the in-flight count and the recent-starts window —
// and restores whatever it was after the test. The proof below needs an
// EXACT contention figure, so earlier tests in the same binary (every
// probeAgentSessionLive call records a start) must not leak into it.
func resetSessionProbeContention(t *testing.T) {
	t.Helper()
	sessionProbeStartsMu.Lock()
	oldStarts := sessionProbeStarts
	sessionProbeStarts = nil
	sessionProbeStartsMu.Unlock()
	oldInFlight := atomic.SwapInt32(&sessionProbesInFlight, 0)
	t.Cleanup(func() {
		sessionProbeStartsMu.Lock()
		sessionProbeStarts = oldStarts
		sessionProbeStartsMu.Unlock()
		atomic.StoreInt32(&sessionProbesInFlight, oldInFlight)
	})
}

// TestProbeAgentSession_TimeoutEscalatesNextAttemptBudget is the INT-CI-092
// retry proof: the hosted run saw e2e-agent-3 fail at stage session-probe
// with every attempt killed by the SAME starved budget — the retry loop
// re-derived the same contention ("budget scaled to 2 concurrent session
// probe(s)") and replayed the identical wait until the spawn failed. A
// per-attempt TIMEOUT is evidence the budget was too small for the host's
// current state, so the next attempt must run at a bigger one (doubled,
// bounded by the hard cap): the bounded retry becomes a real second chance
// instead of a replay.
//
// The budget seams are shrunk so the proof is a few seconds of real time:
// base 400ms, an ssh stub that hangs 600ms on its first two calls (past the
// 400ms budget, inside the escalated 800ms) and succeeds afterwards. The
// assertions pin the ESCALATED shape — success on attempt 2 at exactly twice
// the base budget — not merely "the retries eventually got there": the
// pre-fix code also gets there, but on attempt 3 at the unescalated 400ms.
func TestProbeAgentSession_TimeoutEscalatesNextAttemptBudget(t *testing.T) {
	resetSessionProbeContention(t)

	oldBase, oldPer, oldMax := sessionProbeBudgetBase, sessionProbeBudgetPerSpawn, sessionProbeBudgetMax
	sessionProbeBudgetBase = 400 * time.Millisecond
	sessionProbeBudgetPerSpawn = 400 * time.Millisecond
	sessionProbeBudgetMax = 5 * time.Second
	t.Cleanup(func() {
		sessionProbeBudgetBase, sessionProbeBudgetPerSpawn, sessionProbeBudgetMax = oldBase, oldPer, oldMax
	})

	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "ssh-calls")
	writeStub(t, binDir, "ssh",
		"echo x >> "+logPath+"\n"+
			"n=$(wc -l < "+logPath+")\n"+
			"if [ \"$n\" = \"1\" ] || [ \"$n\" = \"2\" ]; then sleep 0.6; fi\n"+
			"exit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	report, err := probeAgentSessionLive(ctx, "escalate1", "bunker-escalate1", keyPath)
	if err != nil {
		t.Fatalf("probe failed although a timed-out attempt's budget must escalate for the next one: %v", err)
	}
	if report.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (attempt 1 times out at %s; attempt 2 must run at the escalated 2x budget, not repeat the starved one)",
			report.Attempts, sessionProbeBudgetBase)
	}
	if want := 2 * sessionProbeBudgetBase; report.Budget != want {
		t.Errorf("budget = %s, want the escalated %s (2x the timed-out attempt's budget)", report.Budget, want)
	}
	if calls := len(readRecord(t, logPath)); calls != 2 {
		t.Errorf("ssh stub ran %d times, want 2 (the escalated second attempt must succeed, not burn a third try)", calls)
	}
}
