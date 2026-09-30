package server

// PERF-008 wiring: ListAgents classifies the whole fleet from ONE /proc
// sweep (agent.OrphanUIDSummaries — one sweep per call, not one per agent),
// while a service built with only the single-agent summarizer falls back to
// the per-agent path unchanged. The orphan shape (user record gone + live
// uid processes) is driven non-root through the agent package's seams: the
// fixture home is owned by THIS test's uid, and the fixture process table
// carries a process under that same uid.

import (
	"context"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// orphanBatchFixture builds the manager, tracker, service and the gone-user
// fixture shared by the two tests. It registers batch-a/batch-c (users
// exist, healthy) and gone-x (user record gone, one live process under its
// uid = THIS test's uid via the home owner), and returns the service.
func orphanBatchFixture(t *testing.T) (*bunkerdService, *atomic.Int32) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.DefaultConfig()
	cfg.Agent.BaseDataDir = t.TempDir()
	cfg.Agent.Registry.Enabled = false
	m := agent.NewAgentManager(cfg, logger, resource.NewTracker(10, logger), nil, nil)
	t.Cleanup(m.Stop)

	// The orphan arm: bunker-gone-x's user record is gone, its home remains
	// and carries the uid on disk. Created by this process, so the owner is
	// getuid() — and the fixture process 7001 runs under getuid() too.
	homes := t.TempDir()
	origHome := agent.SwapAgentHomeRoot(homes)
	t.Cleanup(func() { agent.SwapAgentHomeRoot(origHome) })
	if err := os.MkdirAll(filepath.Join(homes, "bunker-gone-x"), 0o755); err != nil {
		t.Fatal(err)
	}
	myUID := uint32(os.Getuid())

	procRoot := t.TempDir()
	entries := []struct {
		pid  string
		uid  uint32
		name string
	}{
		{pid: "7001", uid: myUID, name: "stray-daemon"}, // the orphan evidence
		{pid: "7002", uid: 61001, name: "node"},
		{pid: "7003", uid: 61003, name: "python3"},
		{pid: "70", uid: 1, name: "init"},
	}
	for _, p := range entries {
		dir := filepath.Join(procRoot, p.pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		uidStr := itoaUID(p.uid)
		status := "Name:\t" + p.name + "\nUid:\t" + uidStr + "\t" + uidStr + "\t" + uidStr + "\t" + uidStr + "\n"
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	origProc := agent.SwapProcStatusPath(procRoot)
	t.Cleanup(func() { agent.SwapProcStatusPath(origProc) })

	origLookup := agent.SwapLookupUser(func(name string) (*user.User, error) {
		switch name {
		case "bunker-batch-a":
			return &user.User{Uid: "61001", Name: name}, nil
		case "bunker-batch-c":
			return &user.User{Uid: "61003", Name: name}, nil
		}
		return nil, user.UnknownUserError(name) // bunker-gone-x and everyone else
	})
	t.Cleanup(func() { agent.SwapLookupUser(origLookup) })

	tracker := resource.NewTracker(10, logger)
	for _, rec := range []*resource.AgentRecord{
		{AgentID: "batch-a", Status: "running"},
		{AgentID: "gone-x", Status: "running"},
		{AgentID: "batch-c", Status: "running"},
	} {
		if err := tracker.Register(rec); err != nil {
			t.Fatal(err)
		}
	}
	svc := &bunkerdService{
		cfg:                      cfg,
		logger:                   logger,
		tracker:                  tracker,
		agentMgr:                 m,
		orphanUIDSummarizer:      m.OrphanUIDSummary,
		orphanUIDBatchSummarizer: m.OrphanUIDSummaries,
	}
	return svc, nil
}

// TestListAgents_OneProcSweepForWholeFleet is the PERF-008 acceptance pin at
// the wire: ONE ListAgents call over three agents runs the /proc sweep
// EXACTLY ONCE (counted at agent.SwapProcSweep — the seam production goes
// through), and the batch verdicts equal what the single-agent probe would
// have rendered: gone-x carries the ORPHANED verdict, the healthy agents
// stay empty. The pre-fix shape (OrphanUIDSummary per agent, one sweep each)
// counts 3 and fails this test.
func TestListAgents_OneProcSweepForWholeFleet(t *testing.T) {
	svc, _ := orphanBatchFixture(t)

	var sweeps int32
	prevSweep := agent.SwapProcSweep(func() (map[uint32][]agent.UserProcess, error) {
		atomic.AddInt32(&sweeps, 1)
		return agent.ListUserProcessesByUID()
	})
	t.Cleanup(func() { agent.SwapProcSweep(prevSweep) })

	resp, err := svc.ListAgents(context.Background(), connect.NewRequest(&v1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if got := atomic.LoadInt32(&sweeps); got != 1 {
		t.Fatalf("ListAgents over 3 agents ran %d /proc sweeps; want exactly 1 (PERF-008)", got)
	}
	details := map[string]string{}
	for _, a := range resp.Msg.GetAgents() {
		details[a.GetAgentId()] = a.GetOrphanUidDetail()
	}
	if len(details) != 3 {
		t.Fatalf("expected 3 summaries, got %d", len(details))
	}
	if !strings.Contains(details["gone-x"], "ORPHANED UID") ||
		!strings.Contains(details["gone-x"], "stray-daemon") {
		t.Errorf("gone-x verdict wrong through the batch path: %q", details["gone-x"])
	}
	for _, id := range []string{"batch-a", "batch-c"} {
		if details[id] != "" {
			t.Errorf("healthy agent %s must have an empty verdict, got %q", id, details[id])
		}
	}

	// Parity: the single-agent probe on the same fixture renders the same
	// verdict for the orphan (the batch path is a cheaper way to the same
	// answer, not a different one).
	atomic.StoreInt32(&sweeps, 0)
	single := svc.orphanUIDSummarizer("gone-x")
	if single != details["gone-x"] {
		t.Errorf("batch/single verdict drift:\nbatch:  %q\nsingle: %q", details["gone-x"], single)
	}
	if got := atomic.LoadInt32(&sweeps); got != 0 {
		t.Errorf("single-agent probe unexpectedly ran the batch sweep seam %d times", got)
	}
}

// TestListAgents_SingleSummarizerFallback: a service built with ONLY the
// per-agent summarizer (the pre-PERF-008 shape, as hand-constructed tests
// do) still fills every verdict through the per-agent path — the batch seam
// is never touched (count stays 0), and the orphan classification is
// unchanged.
func TestListAgents_SingleSummarizerFallback(t *testing.T) {
	svc, _ := orphanBatchFixture(t)
	svc.orphanUIDBatchSummarizer = nil // the fallback arm

	var sweeps int32
	prevSweep := agent.SwapProcSweep(func() (map[uint32][]agent.UserProcess, error) {
		atomic.AddInt32(&sweeps, 1)
		return agent.ListUserProcessesByUID()
	})
	t.Cleanup(func() { agent.SwapProcSweep(prevSweep) })

	resp, err := svc.ListAgents(context.Background(), connect.NewRequest(&v1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if got := atomic.LoadInt32(&sweeps); got != 0 {
		t.Errorf("fallback path must not touch the batch sweep seam; counted %d", got)
	}
	found := false
	for _, a := range resp.Msg.GetAgents() {
		if a.GetAgentId() != "gone-x" {
			if a.GetOrphanUidDetail() != "" {
				t.Errorf("healthy agent %s carried %q", a.GetAgentId(), a.GetOrphanUidDetail())
			}
			continue
		}
		found = true
		if !strings.Contains(a.GetOrphanUidDetail(), "ORPHANED UID") {
			t.Errorf("fallback lost the orphan verdict: %q", a.GetOrphanUidDetail())
		}
	}
	if !found {
		t.Fatal("gone-x missing from the response")
	}
}
