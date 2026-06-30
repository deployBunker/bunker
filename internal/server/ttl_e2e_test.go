package server

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// TestHeartbeatAgent_ExtendsTTL verifies that HeartbeatAgent extends the
// agent's ExpiresAt by the configured default TTL.
func TestHeartbeatAgent_ExtendsTTL(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := config.DefaultConfig()
	cfg.Agent.DefaultTTL = 30 * time.Second

	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	created := time.Now()
	rec := &resource.AgentRecord{
		AgentID:   "ttl-heartbeat",
		Status:    "running",
		CreatedAt: created,
		ExpiresAt: created.Add(30 * time.Second),
	}
	if err := tracker.Register(rec); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	svc := &bunkerdService{cfg: cfg, logger: logger, tracker: tracker}

	req := connect.NewRequest(&v1.HeartbeatAgentRequest{AgentId: "ttl-heartbeat"})
	resp, err := svc.HeartbeatAgent(context.Background(), req)
	if err != nil {
		t.Fatalf("HeartbeatAgent: %v", err)
	}
	if !resp.Msg.Acknowledged {
		t.Fatal("expected heartbeat to be acknowledged")
	}

	updated := tracker.Get("ttl-heartbeat")
	if updated == nil {
		t.Fatal("agent disappeared after heartbeat")
	}
	wantMin := time.Now().Add(25 * time.Second)
	wantMax := time.Now().Add(35 * time.Second)
	if updated.ExpiresAt.Before(wantMin) || updated.ExpiresAt.After(wantMax) {
		t.Fatalf("expires_at not extended: got %v, want between %v and %v", updated.ExpiresAt, wantMin, wantMax)
	}
}

// TestHeartbeatAgent_MissingAgent verifies that HeartbeatAgent returns
// NotFound for an agent that does not exist.
func TestHeartbeatAgent_MissingAgent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := config.DefaultConfig()
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	svc := &bunkerdService{cfg: cfg, logger: logger, tracker: tracker}

	req := connect.NewRequest(&v1.HeartbeatAgentRequest{AgentId: "missing"})
	_, err := svc.HeartbeatAgent(context.Background(), req)
	if err == nil {
		t.Fatal("expected NotFound error")
	}
	if connectErr, ok := err.(*connect.Error); !ok || connectErr.Code() != connect.CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %v", err)
	}
}

// TestHeartbeatAgent_UsesRequestedTTL verifies that HeartbeatAgent extends TTL
// by the agent's originally requested TTL when one was recorded.
func TestHeartbeatAgent_UsesRequestedTTL(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := config.DefaultConfig()
	cfg.Agent.DefaultTTL = 6 * time.Hour

	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	rec := &resource.AgentRecord{
		AgentID:   "ttl-requested",
		Status:    "running",
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(30 * time.Second),
	}
	if err := tracker.Register(rec); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	svc := &bunkerdService{cfg: cfg, logger: logger, tracker: tracker}
	req := connect.NewRequest(&v1.HeartbeatAgentRequest{AgentId: "ttl-requested"})
	if _, err := svc.HeartbeatAgent(context.Background(), req); err != nil {
		t.Fatalf("HeartbeatAgent: %v", err)
	}

	updated := tracker.Get("ttl-requested")
	if updated == nil {
		t.Fatal("agent disappeared after heartbeat")
	}
	wantMin := time.Now().Add(5 * time.Hour)
	wantMax := time.Now().Add(7 * time.Hour)
	if updated.ExpiresAt.Before(wantMin) || updated.ExpiresAt.After(wantMax) {
		t.Fatalf("expires_at not extended by default TTL: got %v, want between %v and %v", updated.ExpiresAt, wantMin, wantMax)
	}
}
