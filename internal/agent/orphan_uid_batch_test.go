package agent

// PERF-008: the batch orphan classifier pays ONE /proc sweep for the whole
// fleet. These tests pin the sweep count through listUserProcessesByUIDFn
// (the same seam production goes through) and the classification parity with
// the single-agent probe: healthy stays empty, the orphan shape renders its
// verdict, an unresolvable uid fails closed, and a sweep failure marks every
// resolved agent instead of ever reading as clean.

import (
	"errors"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// batchProcEntry is one fixture /proc entry: a live process owned by uid.
type batchProcEntry struct {
	pid  string
	uid  uint32
	name string
}

// batchProcFixture writes a /proc-shaped tree from the entries and returns
// the root (same status-body shape the wiring test's itoaUID fixtures use).
func batchProcFixture(t *testing.T, procs []batchProcEntry) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range procs {
		dir := filepath.Join(root, p.pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		uidStr := itoaUID(p.uid)
		status := "Name:\t" + p.name + "\nUid:\t" + uidStr + "\t" + uidStr + "\t" + uidStr + "\t" + uidStr + "\n"
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// newBatchTestManager builds a real AgentManager the way the wiring tests do
// (registry disabled, throwaway data dir).
func newBatchTestManager(t *testing.T) *AgentManager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.DefaultConfig()
	cfg.Agent.BaseDataDir = t.TempDir()
	cfg.Agent.Registry.Enabled = false
	return NewAgentManager(cfg, logger, resource.NewTracker(10, logger), nil, nil)
}

// TestCheckOrphanUIDBatch_OneProcSweepForAllAgents is the PERF-008 core
// pin: N>=3 agents in ONE batch call trigger exactly ONE /proc sweep — the
// pre-fix shape (one checkOrphanUID per agent, one listUserProcesses each)
// sweeps N times and fails this test.
func TestCheckOrphanUIDBatch_OneProcSweepForAllAgents(t *testing.T) {
	m := newBatchTestManager(t)
	t.Cleanup(m.Stop)

	// Distinct uids so every agent lands in its own bucket — the sweep must
	// still be shared, not per-uid.
	uids := map[string]uint32{"batch-a": 61001, "batch-b": 61002, "batch-c": 61003, "batch-d": 61004}
	procRoot := batchProcFixture(t, []batchProcEntry{
		{pid: "5001", uid: 61001, name: "node"},
		{pid: "5002", uid: 61002, name: "python3"},
		{pid: "5003", uid: 61003, name: "tail"},
		{pid: "5004", uid: 61004, name: "sleep"},
		{pid: "99", uid: 1, name: "init"}, // unrelated uid: must be ignored
	})
	origProc := SwapProcStatusPath(procRoot)
	t.Cleanup(func() { SwapProcStatusPath(origProc) })

	origLookup := SwapLookupUser(func(name string) (*user.User, error) {
		for id, uid := range uids {
			if name == "bunker-"+id {
				return &user.User{Uid: itoaUID(uid), Name: name}, nil
			}
		}
		return nil, user.UnknownUserError(name)
	})
	t.Cleanup(func() { SwapLookupUser(origLookup) })

	var sweeps int32
	origSweep := listUserProcessesByUIDFn
	listUserProcessesByUIDFn = func() (map[uint32][]userProcess, error) {
		atomic.AddInt32(&sweeps, 1)
		return origSweep()
	}
	t.Cleanup(func() { listUserProcessesByUIDFn = origSweep })

	ids := []string{"batch-a", "batch-b", "batch-c", "batch-d"}
	checks := m.checkOrphanUIDBatch(ids)
	if got := atomic.LoadInt32(&sweeps); got != 1 {
		t.Fatalf("one batch call for %d agents ran %d /proc sweeps; want exactly 1 (PERF-008: sweep once, classify all)", len(ids), got)
	}
	// Users EXIST for every id, so nothing is an orphan even though each uid
	// owns a live process — but the processes must have been attributed.
	for i, id := range ids {
		if checks[i].IsOrphan() {
			t.Errorf("%s classified orphan though its user record exists", id)
		}
		if len(checks[i].Processes) != 1 {
			t.Errorf("%s: got %d processes, want 1 (the sweep result must reach every agent)", id, len(checks[i].Processes))
		}
		if checks[i].ProbeErr != "" {
			t.Errorf("%s: unexpected probe error %q", id, checks[i].ProbeErr)
		}
	}
}

// TestOrphanUIDSummaries_BatchClassification pins the verdicts, not just the
// count: healthy empty, orphan rendered from the shared sweep, invalid and
// unknown-uid ids fail closed with an empty verdict.
func TestOrphanUIDSummaries_BatchClassification(t *testing.T) {
	m := newBatchTestManager(t)
	t.Cleanup(m.Stop)

	// The orphan arm needs a home whose on-disk owner carries the uid (user
	// record gone). Non-root-safe: use THIS test's own uid as the orphaned
	// uid — the fixture home is created by us, so its owner is getuid(), and
	// the fixture process runs under getuid() too.
	myUID := uint32(os.Getuid())

	// agentHomeRoot swap: point the home lookup at a throwaway tree.
	homes := t.TempDir()
	origHome := agentHomeRoot
	agentHomeRoot = homes
	t.Cleanup(func() { agentHomeRoot = origHome })
	orphanHome := filepath.Join(homes, "bunker-gone-x")
	if err := os.MkdirAll(orphanHome, 0o755); err != nil {
		t.Fatal(err)
	}

	procRoot := batchProcFixture(t, []batchProcEntry{
		{pid: "6001", uid: myUID, name: "stray-daemon"}, // under the gone user's uid
		{pid: "6002", uid: 61002, name: "node"},         // healthy agent's process
	})
	origProc := SwapProcStatusPath(procRoot)
	t.Cleanup(func() { SwapProcStatusPath(origProc) })

	origLookup := SwapLookupUser(func(name string) (*user.User, error) {
		if name == "bunker-healthy-y" {
			return &user.User{Uid: "61002", Name: name}, nil
		}
		return nil, user.UnknownUserError(name)
	})
	t.Cleanup(func() { SwapLookupUser(origLookup) })

	var sweeps int32
	origSweep := listUserProcessesByUIDFn
	listUserProcessesByUIDFn = func() (map[uint32][]userProcess, error) {
		atomic.AddInt32(&sweeps, 1)
		return origSweep()
	}
	t.Cleanup(func() { listUserProcessesByUIDFn = origSweep })

	ids := []string{"gone-x", "healthy-y", "", "Bad_ID", "lost-z"}
	summaries := m.OrphanUIDSummaries(ids)
	if got := atomic.LoadInt32(&sweeps); got != 1 {
		t.Fatalf("%d ids classified with %d /proc sweeps; want exactly 1", len(ids), got)
	}
	if !strings.Contains(summaries[0], "ORPHANED UID") || !strings.Contains(summaries[0], "stray-daemon") {
		t.Errorf("orphan verdict wrong: %q", summaries[0])
	}
	if summaries[1] != "" {
		t.Errorf("healthy agent must have an empty verdict, got %q", summaries[1])
	}
	for _, i := range []int{2, 3} {
		if summaries[i] != "" {
			t.Errorf("invalid id %q must never probe or classify, got %q", ids[i], summaries[i])
		}
	}
	if summaries[4] != "" {
		t.Errorf("unknown-uid agent must fail CLOSED (empty verdict), got %q", summaries[4])
	}
}

// TestCheckOrphanUIDBatch_SweepFailureFailsClosed: a failed sweep marks every
// resolved agent's ProbeErr — "cannot look" must never read as "nothing
// there" (same contract as the single-agent probe).
func TestCheckOrphanUIDBatch_SweepFailureFailsClosed(t *testing.T) {
	m := newBatchTestManager(t)
	t.Cleanup(m.Stop)

	origLookup := SwapLookupUser(func(name string) (*user.User, error) {
		if name == "bunker-fail-a" || name == "bunker-fail-b" {
			return &user.User{Uid: "61010", Name: name}, nil
		}
		return nil, user.UnknownUserError(name)
	})
	t.Cleanup(func() { SwapLookupUser(origLookup) })

	var sweeps int32
	origSweep := listUserProcessesByUIDFn
	listUserProcessesByUIDFn = func() (map[uint32][]userProcess, error) {
		atomic.AddInt32(&sweeps, 1)
		return nil, errors.New("proc unavailable")
	}
	t.Cleanup(func() { listUserProcessesByUIDFn = origSweep })

	checks := m.checkOrphanUIDBatch([]string{"fail-a", "fail-b", "lost-c"})
	if got := atomic.LoadInt32(&sweeps); got != 1 {
		t.Fatalf("sweep attempted %d times on failure; want exactly 1", got)
	}
	for _, i := range []int{0, 1} {
		if checks[i].ProbeErr == "" {
			t.Errorf("resolved agent %d read as clean despite a failed sweep", i)
		}
		if checks[i].IsOrphan() {
			t.Errorf("agent %d classified orphan from a failed sweep", i)
		}
	}
	if checks[2].ProbeErr == "" {
		t.Errorf("unresolvable agent must keep its fail-closed ProbeErr")
	}
}
