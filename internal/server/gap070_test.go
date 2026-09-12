package server

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/registry"
	"github.com/deployBunker/bunker/internal/resource"
)

// newGAP070Manager wires a real AgentManager to a temp registry.
func newGAP070Manager(t *testing.T, path string) (*agent.AgentManager, *resource.Tracker, *config.Config) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefaultConfig()
	cfg.Agent.Registry.Enabled = true
	cfg.Agent.Registry.Path = path
	cfg.Agent.Registry.MaxBytes = 1 << 20
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	mgr := agent.NewAgentManager(cfg, logger, tracker, nil, nil)
	t.Cleanup(mgr.Stop)
	return mgr, tracker, cfg
}

// TestHeartbeatAgent_PersistsThroughManager proves the daemon's heartbeat RPC
// no longer mutates the tracker directly: the extension reaches the durable
// registry, so it survives a restart (GAP-070).
func TestHeartbeatAgent_PersistsThroughManager(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	mgr, tracker, cfg := newGAP070Manager(t, path)

	const id = "hb-agent"
	expires := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	rec := &resource.AgentRecord{
		AgentID: id, Status: "running", CreatedAt: time.Now(), ExpiresAt: expires,
	}
	if err := tracker.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Seed the durable record the manager would have written at spawn time.
	store, err := registry.Open(registry.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSpawn(&registry.Record{
		AgentID: id, Status: "running", CreatedAt: rec.CreatedAt, ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	cfg.Agent.DefaultTTL = 6 * time.Hour
	svc := &bunkerdService{cfg: cfg, logger: testDiscardLogger(), tracker: tracker, heartbeats: mgr}
	resp, err := svc.HeartbeatAgent(context.Background(), connect.NewRequest(&v1.HeartbeatAgentRequest{AgentId: id}))
	if err != nil {
		t.Fatalf("HeartbeatAgent: %v", err)
	}
	if !resp.Msg.Acknowledged {
		t.Error("heartbeat not acknowledged")
	}
	if got := tracker.Get(id); got == nil || !got.ExpiresAt.After(expires) {
		t.Fatalf("in-memory TTL not extended: %v", got)
	}

	replayed, err := registry.Open(registry.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer replayed.Close()
	stored := replayed.Get(id)
	if stored == nil {
		t.Fatal("registry lost the agent")
	}
	if !stored.ExpiresAt.After(expires.Add(time.Minute)) {
		t.Errorf("heartbeat not persisted: durable expiry %v vs original %v", stored.ExpiresAt, expires)
	}
}

// TestHeartbeatAgent_NotFoundStillNotFound keeps the RPC contract when the
// manager is wired.
func TestHeartbeatAgent_NotFoundStillNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	mgr, tracker, cfg := newGAP070Manager(t, path)
	svc := &bunkerdService{cfg: cfg, logger: testDiscardLogger(), tracker: tracker, heartbeats: mgr}

	_, err := svc.HeartbeatAgent(context.Background(), connect.NewRequest(&v1.HeartbeatAgentRequest{AgentId: "missing"}))
	if err == nil {
		t.Fatal("expected NotFound")
	}
	var cerr *connect.Error
	if !asConnectError(err, &cerr) || cerr.Code() != connect.CodeNotFound {
		t.Fatalf("error = %v, want CodeNotFound", err)
	}
}

// TestAgentServiceHeartbeat_PersistsThroughManager covers the agent-scoped
// heartbeat RPC (fixed 6h extension).
func TestAgentServiceHeartbeat_PersistsThroughManager(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	mgr, tracker, _ := newGAP070Manager(t, path)

	const id = "scoped-agent"
	expires := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	if err := tracker.Register(&resource.AgentRecord{
		AgentID: id, Status: "running", CreatedAt: time.Now(), ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}
	store, err := registry.Open(registry.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSpawn(&registry.Record{
		AgentID: id, Status: "running", CreatedAt: time.Now(), ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	svc := &agentService{logger: testDiscardLogger(), tracker: tracker, heartbeats: mgr}
	resp, err := svc.Heartbeat(context.Background(), connect.NewRequest(&v1.HeartbeatAgentRequest{AgentId: id}))
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !resp.Msg.Acknowledged {
		t.Error("heartbeat not acknowledged")
	}
	replayed, err := registry.Open(registry.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer replayed.Close()
	if stored := replayed.Get(id); stored == nil || !stored.ExpiresAt.After(expires.Add(time.Minute)) {
		t.Errorf("agent-scoped heartbeat was not persisted: %+v", stored)
	}
}

// TestServerRunRefusesWithoutDurableRegistry: a daemon that cannot persist
// agent lifecycle state must not start serving.
func TestServerRunRefusesWithoutDurableRegistry(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.GRPCAddr = "127.0.0.1:0"
	cfg.Server.RESTAddr = ""
	cfg.Auth.Enabled = false
	cfg.Audit.Enabled = false
	// /proc is not writable: opening (and creating the parent of) the
	// registry here always fails.
	cfg.Agent.Registry.Path = "/proc/bunker-does-not-exist/agents.jsonl"

	err := New(cfg).Run(context.Background())
	if err == nil {
		t.Fatal("Run must fail when the durable registry is unavailable")
	}
	if !strings.Contains(err.Error(), "agent registry unavailable") {
		t.Fatalf("error = %v, want an 'agent registry unavailable' failure", err)
	}
}

func asConnectError(err error, target **connect.Error) bool {
	ce, ok := err.(*connect.Error)
	if ok {
		*target = ce
	}
	return ok
}
