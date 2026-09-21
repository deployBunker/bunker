package server

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── GAP-116 server-side preset tests ──────────────────────────────────

func gap116TestService(t *testing.T) (*bunkerdService, *spawnTrackingManager, *bool) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)
	spawnCalled := false
	mgr := &spawnTrackingManager{
		fakeAgentManager: fakeAgentManager{},
		spawnCalled:      &spawnCalled,
	}
	svc := &bunkerdService{
		cfg:      config.DefaultConfig(),
		logger:   logger,
		tracker:  tracker,
		agentMgr: mgr,
	}
	return svc, mgr, &spawnCalled
}

// TestSpawnAgent_UnknownPresetIsInvalidArgument is the AC3 spawn-side hard
// error at the RPC boundary: an unknown per-spawn preset comes back as
// CodeInvalidArgument and NEVER reaches the agent manager (no side effects).
func TestSpawnAgent_UnknownPresetIsInvalidArgument(t *testing.T) {
	svc, _, spawnCalled := gap116TestService(t)

	req := connect.NewRequest(&v1.SpawnAgentRequest{AgentId: "gap116-bad", SafetyPreset: "ultra"})
	_, err := svc.SpawnAgent(context.Background(), req)
	if err == nil {
		t.Fatal("unknown preset accepted at the RPC boundary")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want %s (err: %v)", connect.CodeOf(err), connect.CodeInvalidArgument, err)
	}
	if !strings.Contains(err.Error(), "ultra") {
		t.Errorf("error should name the bad preset: %v", err)
	}
	if *spawnCalled {
		t.Error("agentMgr.Spawn was called despite the invalid preset")
	}
}

// TestSpawnAgent_UnknownPresetEnvIsInvalidArgument proves the env source is
// equally loud at spawn: BUNKERD_SAFETY_PRESET=ultra rejects the spawn even
// with no per-spawn flag (env beats config, and a bad env is a hard error).
func TestSpawnAgent_UnknownPresetEnvIsInvalidArgument(t *testing.T) {
	svc, _, spawnCalled := gap116TestService(t)
	t.Setenv(config.SafetyPresetEnv, "ultra")

	req := connect.NewRequest(&v1.SpawnAgentRequest{AgentId: "gap116-bad-env"})
	_, err := svc.SpawnAgent(context.Background(), req)
	if err == nil {
		t.Fatal("spawn with an unknown BUNKERD_SAFETY_PRESET accepted")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want %s (err: %v)", connect.CodeOf(err), connect.CodeInvalidArgument, err)
	}
	if *spawnCalled {
		t.Error("agentMgr.Spawn was called despite the invalid env preset")
	}
}

// TestSpawnAgent_ValidPresetReachesManager proves a valid preset (and the
// unset case) passes the RPC boundary check and reaches the manager.
func TestSpawnAgent_ValidPresetReachesManager(t *testing.T) {
	for _, preset := range []string{"", "open", "standard", "hardened"} {
		svc, _, spawnCalled := gap116TestService(t)
		req := connect.NewRequest(&v1.SpawnAgentRequest{AgentId: "gap116-ok", SafetyPreset: preset})
		if _, err := svc.SpawnAgent(context.Background(), req); err != nil {
			// The fake manager may fail for other reasons; only the preset
			// boundary check is under test here — a CodeInvalidArgument
			// naming the preset is the failure signal.
			if connect.CodeOf(err) == connect.CodeInvalidArgument && os.Getenv(config.SafetyPresetEnv) == "" {
				t.Fatalf("preset %q rejected at the boundary: %v", preset, err)
			}
		}
		if !*spawnCalled {
			t.Errorf("preset %q never reached the agent manager", preset)
		}
	}
}

// TestRunAgent_UnknownPresetIsInvalidArgument mirrors the spawn mapping on
// the detached-run path.
func TestRunAgent_UnknownPresetIsInvalidArgument(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)
	rec := &resource.AgentRecord{AgentID: "gap116-run", Status: "running"}
	if err := tracker.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	svc := &bunkerdService{
		cfg:      config.DefaultConfig(),
		logger:   logger,
		tracker:  tracker,
		agentMgr: &fakeAgentManager{},
	}
	req := connect.NewRequest(&v1.RunAgentRequest{
		AgentId:      "gap116-run",
		Command:      "echo",
		Detach:       true,
		SafetyPreset: "ultra",
	})
	_, err := svc.RunAgent(context.Background(), req)
	if err == nil {
		t.Fatal("run with an unknown preset accepted")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want %s (err: %v)", connect.CodeOf(err), connect.CodeInvalidArgument, err)
	}
}

// TestGetAgent_CarriesGAP116Fields proves the effective preset and knob set
// ride the EXISTING GetAgent response (AC4's wire path): the tracker record's
// fields flow through ToAgentSummary additively, with no new RPC.
func TestGetAgent_CarriesGAP116Fields(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)
	rec := &resource.AgentRecord{
		AgentID:      "gap116-info",
		Status:       "running",
		SafetyPreset: config.SafetyPresetDefault,
		UnitProperties: []*v1.SystemdProperty{
			{Name: "CPUQuota", Value: "200%"},
			{Name: "MemoryMax", Value: "4294967296"},
			{Name: "LimitFSIZE", Value: "21474836480"},
			{Name: "TasksMax", Value: "4096"},
			{Name: "LimitNOFILE", Value: "65536:65536"},
		},
	}
	if err := tracker.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	svc := &bunkerdService{
		cfg:      config.DefaultConfig(),
		logger:   logger,
		tracker:  tracker,
		agentMgr: &fakeAgentManager{},
	}
	resp, err := svc.GetAgent(context.Background(), connect.NewRequest(&v1.GetAgentRequest{AgentId: "gap116-info"}))
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	a := resp.Msg.GetAgent()
	if a.GetSafetyPreset() != config.SafetyPresetDefault {
		t.Errorf("preset = %q, want %q", a.GetSafetyPreset(), config.SafetyPresetDefault)
	}
	if len(a.GetSystemdProperties()) != 5 {
		t.Fatalf("properties = %d, want 5", len(a.GetSystemdProperties()))
	}
	if got := a.GetSystemdProperties()[0]; got.GetName() != "CPUQuota" || got.GetValue() != "200%" {
		t.Errorf("first property = %s=%s, want CPUQuota=200%%", got.GetName(), got.GetValue())
	}
}
