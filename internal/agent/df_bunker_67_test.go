package agent

// ── DF-BUNKER-67: containment landing convergence budget vs measured systemd
// latency ────────────────────────────────────────────────────────────────────
//
// On bunker-las-03 (systemd 257.13) the MemorySwapMax=0 drop-in landed in the
// slice's cgroup up to 349ms after daemon-reload returned, while the original
// convergence budget allowed 5 reads × 25ms = 125ms — so every
// standard/hardened spawn failed at stage slice-limits with
// `containment landing did not converge within 5 reads: memory.swap.max =
// "max", want "0"` despite a correctly written drop-in (live-proven 3/3,
// full rollback each time).
//
// The tests here pin the RAISED budget (40 × 50ms = 2s) three ways:
//
//  1. a fake cgroup that converges at ~600ms — INSIDE the new budget but
//     ~4.8x past the old one — passes (RED on the pre-fix budget: the old
//     5×25ms loop exhausted at 125ms and failed this exact fixture);
//
//  2. an overridden-small budget still fails LOUD with the same error shape,
//     now naming the overridden attempt count (the failure path is intact);
//
//  3. the shipped budget itself is 40×50ms — asserted directly so re-tuning
//     the vars forces a conscious update here, and so the headroom claim
//     (>= 5× the worst measured 349ms landing latency) is executable rather
//     than prose.
//
// Everything is hermetic: temp trees, no root, no real systemd.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
)

// dfBunker67Fixture writes a memory.swap.max fixture answering "max" and
// points cgroupV2Root at it. Returns the swap file's path and a restore func.
func dfBunker67Fixture(t *testing.T, uid string) (swapFile string, restore func()) {
	t.Helper()
	root := t.TempDir()
	sliceDir := filepath.Join(root, "user.slice", "user-"+uid+".slice")
	if err := os.MkdirAll(sliceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	swapFile = filepath.Join(sliceDir, "memory.swap.max")
	if err := os.WriteFile(swapFile, []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prevRoot := cgroupV2Root
	cgroupV2Root = root
	return swapFile, func() { cgroupV2Root = prevRoot }
}

// TestDFBUNKER67_ConvergenceBudgetCoversMeasuredLatency is the acceptance
// table: convergence at a latency inside the NEW budget succeeds, a
// budget overridden small still fails loud with the same message shape
// naming the (overridden) attempt count, and the shipped budget itself is
// the raised one with real headroom over the measured worst case.
func TestDFBUNKER67_ConvergenceBudgetCoversMeasuredLatency(t *testing.T) {
	uid := "4242"
	cfg := config.DefaultConfig()
	m := &AgentManager{cfg: cfg, logger: testLogger()}
	want := containmentResolved{swapBarred: true}

	t.Run("converges at ~600ms, inside the new budget", func(t *testing.T) {
		// NO var overrides: this arm must prove the SHIPPED budget covers
		// a landing latency that the old 5×25ms=125ms budget provably did
		// not. The flip uses the landing watcher hook — the same seam the
		// INT-CI-37 tests drive — so each loop read sees the cgroup state
		// systemd would have published at that moment.
		const flipAfter = 600 * time.Millisecond
		swapFile, restoreRoot := dfBunker67Fixture(t, uid)
		defer restoreRoot()

		start := time.Now()
		prevWatcher := intci37LandingWatcher
		intci37LandingWatcher = func() {
			if time.Since(start) >= flipAfter {
				_ = os.WriteFile(swapFile, []byte("0\n"), 0o644)
			}
		}
		t.Cleanup(func() { intci37LandingWatcher = prevWatcher })

		if err := m.verifyContainmentLandingConverged(t.Context(), uid, want); err != nil {
			t.Fatalf("a cgroup converging at %v failed within the shipped %d×%v budget: %v",
				flipAfter, containmentConvergeAttempts, containmentConvergePoll, err)
		}
	})

	t.Run("overridden-small budget still fails loud, naming the count", func(t *testing.T) {
		swapFile, restoreRoot := dfBunker67Fixture(t, uid)
		defer restoreRoot()
		_ = swapFile // the fixture never converges; the flip seam stays nil

		prevAttempts := containmentConvergeAttempts
		prevPoll := containmentConvergePoll
		containmentConvergeAttempts = 2
		containmentConvergePoll = time.Millisecond // no real waits
		t.Cleanup(func() {
			containmentConvergeAttempts = prevAttempts
			containmentConvergePoll = prevPoll
		})

		err := m.verifyContainmentLandingConverged(t.Context(), uid, want)
		if err == nil {
			t.Fatal("a cgroup that never landed passed an overridden-small budget (enforcement weakened)")
		}
		// Same message shape, naming the NEW (here overridden) attempt count.
		if !strings.Contains(err.Error(), "did not converge within 2 reads") {
			t.Fatalf("exhaustion error does not name the attempt count: %v", err)
		}
		if !strings.Contains(err.Error(), `memory.swap.max = "max"`) || !strings.Contains(err.Error(), `want "0"`) {
			t.Fatalf("exhaustion error lost the requested-vs-observed pair: %v", err)
		}
	})

	t.Run("exhaustion at the shipped budget names the new count", func(t *testing.T) {
		_, restoreRoot := dfBunker67Fixture(t, uid)
		defer restoreRoot()

		// Keep the SHIPPED attempt count (the message must name it); only
		// shrink the poll so 40 exhausted reads cost milliseconds, not 2s.
		prevPoll := containmentConvergePoll
		containmentConvergePoll = time.Millisecond
		t.Cleanup(func() { containmentConvergePoll = prevPoll })

		err := m.verifyContainmentLandingConverged(t.Context(), uid, want)
		if err == nil {
			t.Fatal("a cgroup that never landed passed the shipped budget (enforcement weakened)")
		}
		if !strings.Contains(err.Error(), "did not converge within 40 reads") {
			t.Fatalf("exhaustion error does not name the NEW attempt count: %v", err)
		}
	})

	t.Run("shipped budget is 40x50ms with headroom over the measured worst case", func(t *testing.T) {
		// Executable pin of the re-tune: any change to the shipped vars
		// must consciously update this arm. dfBunker67WorstLandingLatency
		// is the DIRECTLY MEASURED worst landing latency on bunker-las-03
		// (systemd 257.13): 349ms.
		const dfBunker67WorstLandingLatency = 349 * time.Millisecond

		if containmentConvergeAttempts != 40 {
			t.Fatalf("containmentConvergeAttempts = %d, want 40 (DF-BUNKER-67 budget)", containmentConvergeAttempts)
		}
		if containmentConvergePoll != 50*time.Millisecond {
			t.Fatalf("containmentConvergePoll = %v, want 50ms (DF-BUNKER-67 budget)", containmentConvergePoll)
		}
		budget := time.Duration(containmentConvergeAttempts) * containmentConvergePoll
		if budget < 5*dfBunker67WorstLandingLatency {
			t.Fatalf("convergence budget %v < 5× the measured worst landing latency %v — the DF-BUNKER-67 headroom rule is violated",
				budget, 5*dfBunker67WorstLandingLatency)
		}
	})
}
