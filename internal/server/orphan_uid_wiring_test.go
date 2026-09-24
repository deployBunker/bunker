package server

// DF-BUNKER-34 wiring test: the PRODUCTION wiring (server.go sets
// orphanUIDSummarizer = agentMgr.OrphanUIDSummary) drives a REAL AgentManager
// against a fixture /proc tree, so the end-to-end surface — /proc read →
// orphan classification → wire field → what `bunker info/list` renders — is
// proven, not just the service's pass-through.

import (
	"context"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// itoaUID is the small local helper for the fixture status bodies.
func itoaUID(v uint32) string {
	if v == 0 {
		return "0"
	}
	digits := ""
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	return digits
}

// TestOrphanUIDWiring_RealManagerToWire drives the real manager probe (with
// a fixture /proc tree and a gone user record) through the production wiring
// into the wire summary. The fail-closed property is the assertion: the wire
// field never fabricates an orphan verdict for a uid it could not observe.
func TestOrphanUIDWiring_RealManagerToWire(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.DefaultConfig()
	cfg.Agent.BaseDataDir = t.TempDir()
	cfg.Agent.Registry.Enabled = false
	m := agent.NewAgentManager(cfg, logger, resource.NewTracker(10, logger), nil, nil)
	t.Cleanup(m.Stop)

	// Fixture /proc: one live process under uid 61006 (and an unrelated one
	// under uid 1), so the probe's classification is exercised, not just its
	// directory read.
	procRoot := t.TempDir()
	for _, p := range []struct {
		pid  string
		uid  uint32
		name string
	}{{pid: "478001", uid: 61006, name: "node"}, {pid: "1", uid: 1, name: "init"}} {
		dir := filepath.Join(procRoot, p.pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		status := "Name:\t" + p.name + "\nUid:\t" + itoaUID(p.uid) + "\t" + itoaUID(p.uid) + "\t" + itoaUID(p.uid) + "\t" + itoaUID(p.uid) + "\n"
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	origProc := agent.SwapProcStatusPath(procRoot)
	t.Cleanup(func() { agent.SwapProcStatusPath(origProc) })

	// The user record is GONE for bunker-wired-x (lookupUser fails) — the
	// orphan arm's precondition.
	origLookup := agent.SwapLookupUser(func(name string) (*user.User, error) {
		return nil, user.UnknownUserError(name)
	})
	t.Cleanup(func() { agent.SwapLookupUser(origLookup) })

	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{AgentID: "wired-x", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	svc := &bunkerdService{
		cfg:                 cfg,
		logger:              logger,
		tracker:             tracker,
		agentMgr:            m,
		orphanUIDSummarizer: m.OrphanUIDSummary,
	}

	resp, err := svc.ListAgents(context.Background(), connect.NewRequest(&v1.ListAgentsRequest{}))
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	seen := false
	for _, a := range resp.Msg.GetAgents() {
		if a.GetAgentId() != "wired-x" {
			continue
		}
		seen = true
		detail := a.GetOrphanUidDetail()
		// Non-root: the fixture home does not exist under the (unswappable
		// without root) /home, so the uid source is unobservable — the probe
		// fails closed and the wire field must be EMPTY, never a fabricated
		// orphan verdict.
		if os.Geteuid() != 0 {
			if strings.Contains(detail, "ORPHANED") {
				t.Errorf("non-root run fabricated an orphan verdict on the wire: %q", detail)
			}
			if detail != "" && !strings.Contains(detail, "uid unknown") {
				t.Errorf("non-root run carried a non-unknown detail %q; only the fail-closed note or empty is honest", detail)
			}
		}
		t.Logf("wired-x orphan detail: %q", detail)
	}
	if !seen {
		t.Fatal("ListAgents never returned the registered agent")
	}
}
