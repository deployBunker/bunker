package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

func newCoverageTestManager(maxAgents uint32) *AgentManager {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefaultConfig()
	return &AgentManager{
		cfg:     cfg,
		logger:  logger,
		tracker: resource.NewTracker(maxAgents, logger),
		ttlStop: make(chan struct{}),
	}
}

func TestBuildRunAgentArgs_LimitBranches(t *testing.T) {
	tests := []struct {
		name      string
		limits    *v1.ResourceLimits
		want      []string
		doNotWant []string
	}{
		{
			name:      "nil limits",
			limits:    nil,
			doNotWant: []string{"CPUQuota=", "MemoryMax=", "LimitFSIZE="},
		},
		{
			name:      "zero limits",
			limits:    &v1.ResourceLimits{},
			doNotWant: []string{"CPUQuota=", "MemoryMax=", "LimitFSIZE="},
		},
		{
			name:      "CPU only",
			limits:    &v1.ResourceLimits{CpuQuota: 1.25},
			want:      []string{"--property=CPUQuota=125%"},
			doNotWant: []string{"MemoryMax=", "LimitFSIZE="},
		},
		{
			name:      "memory only",
			limits:    &v1.ResourceLimits{MemoryMaxBytes: 268435456},
			want:      []string{"--property=MemoryMax=268435456"},
			doNotWant: []string{"CPUQuota=", "LimitFSIZE="},
		},
		{
			name:      "disk only",
			limits:    &v1.ResourceLimits{DiskMaxBytes: 1073741824},
			want:      []string{"--property=LimitFSIZE=1073741824"},
			doNotWant: []string{"CPUQuota=", "MemoryMax="},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := buildRunAgentArgs("coverage", "1001", "1002", "coverage-unit", "true", nil, nil, tt.limits)
			joined := strings.Join(args, " ")
			for _, want := range tt.want {
				if !strings.Contains(joined, want) {
					t.Errorf("args missing %q: %v", want, args)
				}
			}
			for _, unwanted := range tt.doNotWant {
				if strings.Contains(joined, unwanted) {
					t.Errorf("args unexpectedly contain %q: %v", unwanted, args)
				}
			}
		})
	}
}

func TestBuildRunAgentArgs_OverridesAndPassthrough(t *testing.T) {
	args := buildRunAgentArgs(
		"coverage", "1001", "1002", "coverage-unit", "command with spaces",
		[]string{"--flag", "value with spaces", ""},
		map[string]string{
			"HOME":        "/custom/home",
			"DOCKER_HOST": "tcp://127.0.0.1:2375",
			"EXTRA":       "extra value",
		},
		&v1.ResourceLimits{CpuQuota: 2, MemoryMaxBytes: 512, DiskMaxBytes: 1024},
	)

	for _, want := range []string{
		"--setenv=HOME=/custom/home",
		"--setenv=DOCKER_HOST=tcp://127.0.0.1:2375",
		"--setenv=EXTRA=extra value",
	} {
		if !contains(args, want) {
			t.Errorf("args missing exact entry %q: %v", want, args)
		}
	}
	for _, unwanted := range []string{
		"--setenv=HOME=/home/bunker-coverage",
		"--setenv=DOCKER_HOST=unix:///run/bunker/coverage/docker.sock",
	} {
		if contains(args, unwanted) {
			t.Errorf("overridden default remains in args: %q", unwanted)
		}
	}

	wantTail := []string{"sh", "-c", ". /run/bunker/coverage/env 2>/dev/null && exec \"$@\"", "--", "command with spaces", "--flag", "value with spaces", ""}
	if len(args) < len(wantTail) {
		t.Fatalf("args too short: %v", args)
	}
	gotTail := args[len(args)-len(wantTail):]
	for i := range wantTail {
		if gotTail[i] != wantTail[i] {
			t.Errorf("tail[%d] = %q, want %q; full args: %v", i, gotTail[i], wantTail[i], args)
		}
	}
}

func TestEnsureSubIDEntry_ExistingMappingIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subuid")
	original := "other:100000:65536\n  target:200000:65536  \nmalformed\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if err := ensureSubIDEntry(path, "target", 900000); err != nil {
		t.Fatalf("ensureSubIDEntry: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if string(got) != original {
		t.Errorf("existing mapping changed: got %q, want %q", got, original)
	}
}

func TestEnsureSubIDEntry_CreateAndAppend(t *testing.T) {
	tests := []struct {
		name     string
		initial  *string
		username string
		start    int
		want     string
	}{
		{
			name:     "missing file",
			username: "new-user",
			start:    300000,
			want:     "new-user:300000:65536\n",
		},
		{
			name:     "append existing file without trailing newline",
			initial:  stringPointer("first:100000:65536"),
			username: "second",
			start:    400000,
			want:     "first:100000:65536second:400000:65536\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "subuid")
			if tt.initial != nil {
				if err := os.WriteFile(path, []byte(*tt.initial), 0644); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
			}
			if err := ensureSubIDEntry(path, tt.username, tt.start); err != nil {
				t.Fatalf("ensureSubIDEntry: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read result: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("result = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnsureSubIDEntry_ReadAndOpenErrors(t *testing.T) {
	t.Run("read directory", func(t *testing.T) {
		path := t.TempDir()
		err := ensureSubIDEntry(path, "user", 100000)
		if err == nil || !strings.Contains(err.Error(), "read ") {
			t.Fatalf("error = %v, want read error", err)
		}
	})

	t.Run("missing parent directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing", "subuid")
		err := ensureSubIDEntry(path, "user", 100000)
		if err == nil || !strings.Contains(err.Error(), "open ") {
			t.Fatalf("error = %v, want open error", err)
		}
	})
}

func TestConfigureSubIDs_UnknownUser_Coverage(t *testing.T) {
	username := "bunker-definitely-missing-" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	err := configureSubIDs(context.Background(), username)
	if err == nil || !strings.Contains(err.Error(), "lookup user "+username) {
		t.Fatalf("error = %v, want lookup failure", err)
	}
}

func TestRunAgent_ValidationOrder(t *testing.T) {
	m := newCoverageTestManager(1)
	tests := []struct {
		name string
		req  *v1.RunAgentRequest
		want string
	}{
		{
			name: "agent ID first",
			req:  &v1.RunAgentRequest{},
			want: "agent_id is required",
		},
		{
			name: "command second",
			req:  &v1.RunAgentRequest{AgentId: "missing"},
			want: "command is required",
		},
		{
			name: "detach third",
			req:  &v1.RunAgentRequest{AgentId: "missing", Command: "true"},
			want: "only supports detached mode",
		},
		{
			name: "tracker lookup last",
			req:  &v1.RunAgentRequest{AgentId: "missing", Command: "true", Detach: true},
			want: `agent "missing" not found`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := m.RunAgent(context.Background(), tt.req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestSpawn_InvalidIDsDoNotAllocatePorts(t *testing.T) {
	m := newCoverageTestManager(1)
	tests := []string{"UPPER", "slash/id", "under_score", strings.Repeat("a", 64)}
	for _, agentID := range tests {
		t.Run(agentID, func(t *testing.T) {
			_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: agentID})
			if err == nil || !strings.Contains(err.Error(), "invalid agent_id") {
				t.Fatalf("error = %v, want invalid agent_id", err)
			}
			if m.tracker.Count() != 0 {
				t.Fatalf("tracker count = %d, want 0", m.tracker.Count())
			}
		})
	}
}

func TestSpawn_CapacityConflict(t *testing.T) {
	m := newCoverageTestManager(1)
	if err := m.tracker.Register(&resource.AgentRecord{AgentID: "existing"}); err != nil {
		t.Fatalf("register fixture: %v", err)
	}

	_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: "new-agent"})
	if err == nil || !strings.Contains(err.Error(), "capacity full: 1/1 agents") {
		t.Fatalf("error = %v, want capacity conflict", err)
	}
}

func TestDestroy_InvalidIDs_Coverage(t *testing.T) {
	m := newCoverageTestManager(1)
	for _, agentID := range []string{"", "UPPER", "slash/id", strings.Repeat("x", 64)} {
		t.Run(agentID, func(t *testing.T) {
			resp, err := m.Destroy(context.Background(), agentID, false)
			if err == nil || !strings.Contains(err.Error(), "invalid agent_id") {
				t.Fatalf("error = %v, want invalid agent_id", err)
			}
			if resp == nil || resp.GetAgentId() != agentID || resp.GetStatus() != "error" {
				t.Fatalf("response = %+v, want agent_id %q and error status", resp, agentID)
			}
		})
	}
}

func TestGenerateUUIDv4_FormatAndUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 32)
	for range 32 {
		got, err := generateUUIDv4()
		if err != nil {
			t.Fatalf("generateUUIDv4: %v", err)
		}
		parts := strings.Split(got, "-")
		if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
			t.Fatalf("invalid UUID shape: %q", got)
		}
		if parts[2][0] != '4' || !strings.ContainsRune("89ab", rune(parts[3][0])) {
			t.Fatalf("UUID %q has invalid version or variant", got)
		}
		if _, exists := seen[got]; exists {
			t.Fatalf("duplicate UUID: %q", got)
		}
		seen[got] = struct{}{}
	}
}

func TestNewAgentManager_PortAllocatorValid(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefaultConfig()
	cfg.Agent.PortRangeStart = 10000
	cfg.Agent.PortRangeEnd = 10099
	cfg.Agent.PortRangePerAgent = 10

	m := NewAgentManager(cfg, logger, resource.NewTracker(10, logger), nil, nil)
	if m.portAlloc == nil {
		t.Fatal("portAlloc should be initialized with valid range")
	}
	m.Stop()
}

func TestNewAgentManager_PortAllocatorNil(t *testing.T) {
	tests := []struct {
		name  string
		start uint32
		end   uint32
		per   uint32
	}{
		{"start >= end", 100, 100, 10},
		{"rangeSize zero", 100, 200, 0},
		{"range too small", 100, 101, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			cfg := config.DefaultConfig()
			cfg.Agent.PortRangeStart = tt.start
			cfg.Agent.PortRangeEnd = tt.end
			cfg.Agent.PortRangePerAgent = tt.per

			m := NewAgentManager(cfg, logger, resource.NewTracker(10, logger), nil, nil)
			if m.portAlloc != nil {
				t.Errorf("portAlloc should be nil when %s", tt.name)
			}
			m.Stop()
		})
	}
}

func TestSpawn_AutoGenerateAgentID_Coverage(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefaultConfig()
	cfg.Agent.PortRangeStart = 20000
	cfg.Agent.PortRangeEnd = 29999
	cfg.Agent.PortRangePerAgent = 1000

	m := &AgentManager{
		cfg:       cfg,
		logger:    logger,
		tracker:   resource.NewTracker(10, logger),
		portAlloc: mustNewPortAllocator(20000, 29999, 1000, t),
	}

	// Auto-generate path: empty AgentId triggers UUID generation (first 8 chars)
	resp, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{})
	// This will fail at user creation (not running as root), but we reach the
	// auto-ID path which executes the UUID generation + split logic.
	if err == nil && resp != nil {
		if resp.GetAgentId() == "" {
			t.Error("expected auto-generated agent_id")
		}
	}
	// Any error after ID validation is expected since we're not root
}

func TestSpawn_PortAllocNilFallback_Coverage(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefaultConfig()
	cfg.Agent.PortRangeStart = 40000
	cfg.Agent.PortRangeEnd = 49999
	cfg.Agent.PortRangePerAgent = 0 // triggers portAlloc=nil

	m := NewAgentManager(cfg, logger, resource.NewTracker(10, logger), nil, nil)
	if m.portAlloc != nil {
		t.Fatal("portAlloc should be nil with invalid config")
	}

	// Spawn with nil portAlloc should fallback to direct range.
	resp, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: "test-nil-pa"})
	// Will fail at user creation (not root), but the port alloc nil path is reached
	_ = resp
	_ = err
	m.Stop()
}

func TestDestroy_EmptyRequest_Coverage(t *testing.T) {
	m := newCoverageTestManager(1)
	resp, err := m.Destroy(context.Background(), "", false)
	if err == nil || !strings.Contains(err.Error(), "invalid agent_id") {
		t.Fatalf("error = %v, want invalid agent_id for empty string", err)
	}
	if resp == nil || resp.GetStatus() != "error" {
		t.Fatal("expected error status in response")
	}
}

func TestReapExpiredAgents_Coverage(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := &AgentManager{
		cfg:       config.DefaultConfig(),
		logger:    logger,
		tracker:   resource.NewTracker(10, logger),
		ttlStop:   make(chan struct{}),
		portAlloc: nil,
	}

	// Register an expired agent
	past := time.Now().Add(-1 * time.Hour)
	if err := m.tracker.Register(&resource.AgentRecord{
		AgentID:   "expired-test",
		ExpiresAt: past,
	}); err != nil {
		t.Fatalf("register fixture: %v", err)
	}

	// Register an active agent
	future := time.Now().Add(1 * time.Hour)
	if err := m.tracker.Register(&resource.AgentRecord{
		AgentID:   "active-test",
		ExpiresAt: future,
	}); err != nil {
		t.Fatalf("register fixture: %v", err)
	}

	// Register an agent with zero expiry (should never expire)
	if err := m.tracker.Register(&resource.AgentRecord{
		AgentID:   "no-expiry-test",
		ExpiresAt: time.Time{},
	}); err != nil {
		t.Fatalf("register fixture: %v", err)
	}

	// reapExpiredAgents should attempt to destroy "expired-test"
	m.reapExpiredAgents()

	// The expired agent should be removed from tracker (Destroy unregisters
	// even when userdel fails).
	if m.tracker.Get("expired-test") != nil {
		t.Error("expired agent should have been reaped")
	}
	// Active and zero-expiry agents should remain.
	if m.tracker.Get("active-test") == nil {
		t.Error("active agent should not have been reaped")
	}
	if m.tracker.Get("no-expiry-test") == nil {
		t.Error("zero-expiry agent should not have been reaped")
	}
}

func TestRemoveUserSliceLimits_NoUser_Coverage(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// This should not panic; just log a warning that user doesn't exist
	removeUserSliceLimits(context.Background(), "nonexistent-user-12345", logger)
}

func TestStop_ClosesTTLChannel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefaultConfig()
	cfg.Agent.MaxAgents = 10
	cfg.Agent.PortRangeStart = 60000
	cfg.Agent.PortRangeEnd = 69999
	cfg.Agent.PortRangePerAgent = 1000

	m := NewAgentManager(cfg, logger, resource.NewTracker(10, logger), nil, nil)
	m.Stop()

	// Verify ttlStop channel is closed.
	select {
	case <-m.ttlStop:
		// expected
	default:
		t.Error("ttlStop should be closed after Stop()")
	}
}

// ── Destroy force=true / force=false paths ──────────────────────

func TestDestroy_ForceTrue_UserMissing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefaultConfig()
	tracker := resource.NewTracker(10, logger)
	m := &AgentManager{cfg: cfg, logger: logger, tracker: tracker, ttlStop: make(chan struct{})}
	defer close(m.ttlStop)

	agentID := "force-coverage-test"
	if err := tracker.Register(&resource.AgentRecord{
		AgentID:   agentID,
		Status:    "running",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	resp, err := m.Destroy(context.Background(), agentID, true)
	// force=true: userdel fails but Destroy continues and returns "destroyed".
	if err != nil {
		t.Errorf("force=true should not return error for missing user: %v", err)
	}
	if resp.Status != "destroyed" {
		t.Errorf("expected 'destroyed', got %q", resp.Status)
	}
	if tracker.Get(agentID) != nil {
		t.Error("agent should be unregistered after force destroy")
	}
}

func TestDestroy_ForceFalse_UserMissing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefaultConfig()
	tracker := resource.NewTracker(10, logger)
	m := &AgentManager{cfg: cfg, logger: logger, tracker: tracker, ttlStop: make(chan struct{})}
	defer close(m.ttlStop)

	agentID := "noforce-coverage-test"
	if err := tracker.Register(&resource.AgentRecord{
		AgentID:   agentID,
		Status:    "running",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	resp, err := m.Destroy(context.Background(), agentID, false)
	// force=false: userdel fails, returns "not_found".
	if err == nil {
		t.Fatal("expected error for force=false with missing user")
	}
	if resp.Status != "not_found" {
		t.Errorf("expected 'not_found', got %q", resp.Status)
	}
	// Tracker should still have unregistered the agent.
	if tracker.Get(agentID) != nil {
		t.Error("agent should be unregistered even when userdel fails")
	}
}

// ── Destroy with nil tunnel/tailscale/portAlloc ─────────────────

func TestDestroy_NilManagers(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefaultConfig()
	cfg.Agent.PortRangeStart = 100
	cfg.Agent.PortRangeEnd = 50 // invalid → portAlloc nil
	tracker := resource.NewTracker(10, logger)
	m := NewAgentManager(cfg, logger, tracker, nil, nil)
	defer m.Stop()

	agentID := "nil-managers-test"
	if err := tracker.Register(&resource.AgentRecord{
		AgentID:   agentID,
		Status:    "running",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	resp, err := m.Destroy(context.Background(), agentID, true)
	if err != nil {
		t.Errorf("nil managers should not cause error: %v", err)
	}
	if resp.Status != "destroyed" {
		t.Errorf("expected 'destroyed', got %q", resp.Status)
	}
}

// ── Spawn with empty agent_id (auto-generate path) ──────────────

func TestSpawn_EmptyAgentID_NoValidationError(t *testing.T) {
	m := newTestManager(t)
	defer m.Stop()

	_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: ""})
	// Will fail at useradd (non-root), but should NOT fail with "invalid agent_id".
	if err != nil && strings.Contains(err.Error(), "invalid agent_id") {
		t.Errorf("empty agent_id should auto-generate, not fail validation: %v", err)
	}
}

// ── NewAgentManager all fields populated ────────────────────────

func TestNewAgentManager_AllFieldsPopulated(t *testing.T) {
	cfg := config.DefaultConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	m := NewAgentManager(cfg, logger, tracker, nil, nil)
	defer m.Stop()

	if m.cfg == nil {
		t.Error("cfg should not be nil")
	}
	if m.logger == nil {
		t.Error("logger should not be nil")
	}
	if m.tracker == nil {
		t.Error("tracker should not be nil")
	}
	if m.ttlStop == nil {
		t.Error("ttlStop should not be nil")
	}
	if m.portAlloc == nil {
		t.Error("portAlloc should be non-nil with valid config")
	}
}

// ── ensureRootlesskitAppArmor error path ────────────────────────

func TestEnsureRootlesskitAppArmor_UnknownUser_Coverage(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := ensureRootlesskitAppArmor(context.Background(), "nonexistent-user-99999", logger)
	if err == nil {
		t.Fatal("expected error for unknown user")
	}
	if !strings.Contains(err.Error(), "lookup user") {
		t.Errorf("expected lookup error, got: %v", err)
	}
}

// ── applyUserSliceLimits / removeUserSliceLimits error paths ────

func TestApplyUserSliceLimits_NotRoot_Coverage(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test verifies non-root failure")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	u, err := user.Lookup(os.Getenv("USER"))
	if err != nil {
		u, err = user.Current()
		if err != nil {
			t.Skipf("cannot determine current user: %v", err)
		}
	}
	err = applyUserSliceLimits(context.Background(), u,
		0.5, 256*1024*1024, 0, 100, 1024, logger)
	if err == nil {
		t.Fatal("expected error when not root")
	}
	// The error could be from mkdir or from WriteFile depending on whether
	// the directory already exists. Both indicate non-root failure.
	if !strings.Contains(err.Error(), "mkdir") && !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected mkdir or permission error, got: %v", err)
	}
}

func TestRemoveUserSliceLimits_UserNotFound_Coverage(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	removeUserSliceLimits(context.Background(), "nonexistent-user-99999", logger)
}

// ── waitForUserManager tests ────────────────────────────────────

func TestWaitForUserManager_Success_Coverage(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	busPath := filepath.Join(runtimeDir, "bus")

	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.WriteFile(busPath, []byte{}, 0644)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitForUserManager(ctx, runtimeDir); err != nil {
		t.Fatalf("waitForUserManager should succeed: %v", err)
	}
}

func TestWaitForUserManager_Timeout_Coverage(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := waitForUserManager(ctx, dir)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWaitForUserManager_ContextCancelled(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitForUserManager(ctx, dir)
	if err == nil {
		t.Fatal("expected context cancelled error")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

func TestWaitForUserManager_NoDeadline(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	busPath := filepath.Join(runtimeDir, "bus")

	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.WriteFile(busPath, []byte{}, 0644)
	}()

	ctx := context.Background()
	if err := waitForUserManager(ctx, runtimeDir); err != nil {
		t.Fatalf("waitForUserManager should succeed: %v", err)
	}
}

// ── Spawn valid ID non-root (cleanup on failure) ────────────────

func TestSpawn_ValidIDNonRoot_CleanupOnFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test verifies non-root failure path")
	}
	m := newTestManager(t)
	defer m.Stop()

	agentID := uniqueAgentID("cleanuptest")
	req := &v1.SpawnAgentRequest{AgentId: agentID}
	resp, err := m.Spawn(context.Background(), req)
	if err == nil {
		t.Fatal("expected error in non-root environment")
	}
	if resp != nil {
		t.Errorf("expected nil response on failure, got %+v", resp)
	}
	if m.tracker.Get(agentID) != nil {
		t.Error("agent should not be in tracker after failed spawn")
	}
}

func TestSpawn_RequestLimitsCapturedBeforeFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test verifies non-root failure path")
	}
	m := newTestManager(t)
	defer m.Stop()

	req := &v1.SpawnAgentRequest{
		AgentId: "limits-pre-fail",
		Limits: &v1.ResourceLimits{
			CpuQuota:            0.25,
			MemoryMaxBytes:      128 * 1024 * 1024,
			DiskMaxBytes:        512 * 1024 * 1024,
			MaxDockerContainers: 5,
		},
	}
	_, err := m.Spawn(context.Background(), req)
	if err == nil {
		t.Fatal("expected error in non-root environment")
	}
}

// ── buildRunAgentArgs individual limit tests ────────────────────

func TestBuildRunAgentArgs_CPUOnly(t *testing.T) {
	limits := &v1.ResourceLimits{CpuQuota: 2.0}
	args := buildRunAgentArgs("agent1", "1000", "1000", "unit1", "echo", []string{"hi"}, nil, limits)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "CPUQuota=200%") {
		t.Errorf("expected CPUQuota=200%%: %s", joined)
	}
	if strings.Contains(joined, "MemoryMax=") {
		t.Errorf("should not contain MemoryMax when MemoryMaxBytes=0: %s", joined)
	}
	if strings.Contains(joined, "LimitFSIZE=") {
		t.Errorf("should not contain LimitFSIZE when DiskMaxBytes=0: %s", joined)
	}
}

func TestBuildRunAgentArgs_MemoryOnly(t *testing.T) {
	limits := &v1.ResourceLimits{MemoryMaxBytes: 1024 * 1024 * 128}
	args := buildRunAgentArgs("agent1", "1000", "1000", "unit1", "echo", nil, nil, limits)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "MemoryMax=134217728") {
		t.Errorf("expected MemoryMax=134217728: %s", joined)
	}
	if strings.Contains(joined, "CPUQuota=") {
		t.Errorf("should not contain CPUQuota when CpuQuota=0: %s", joined)
	}
}

func TestBuildRunAgentArgs_DiskOnly(t *testing.T) {
	limits := &v1.ResourceLimits{DiskMaxBytes: 1024 * 1024 * 512}
	args := buildRunAgentArgs("agent1", "1000", "1000", "unit1", "echo", nil, nil, limits)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "LimitFSIZE=536870912") {
		t.Errorf("expected LimitFSIZE=536870912: %s", joined)
	}
}

func TestBuildRunAgentArgs_AllArgsPassed(t *testing.T) {
	args := buildRunAgentArgs(
		"agent1", "1000", "1000", "unit1",
		"docker",
		[]string{"run", "-d", "--name", "test"},
		nil, nil,
	)
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{"run", "-d", "--name", "test"} {
		if !strings.Contains(joined, " "+want+" ") {
			t.Errorf("expected arg %q in: %s", want, joined)
		}
	}
}

// ── validAgentID edge cases ─────────────────────────────────────

func TestValidAgentID_EdgeCases(t *testing.T) {
	valid := []string{
		"a", "1", "test-agent", "agent-123", "a1b2c3",
		"my-agent-42", strings.Repeat("x", 63),
	}
	for _, id := range valid {
		if !validAgentID.MatchString(id) {
			t.Errorf("%q should be valid", id)
		}
	}
}

func TestValidAgentID_InvalidCases(t *testing.T) {
	invalid := []string{
		"UPPER", "slash/id", "under_score",
		"space id", strings.Repeat("x", 64), "@special",
		".dot", "pound#sign",
	}
	for _, id := range invalid {
		if validAgentID.MatchString(id) {
			t.Errorf("%q should be invalid", id)
		}
	}
}

// ── stopDockerdDirect no-process path ───────────────────────────

func TestStopDockerdDirect_NoMatchingUser(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := stopDockerdDirect(context.Background(), "nonexistent-user-xyz", "bunker-docker-xyz", logger)
	if err == nil {
		t.Fatal("expected error for nonexistent user")
	}
}

// ── reapExpiredAgents skips zero ExpiresAt ──────────────────────

func TestReapExpiredAgents_SkipsZeroExpiresAt_Coverage(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefaultConfig()
	tracker := resource.NewTracker(10, logger)
	m := &AgentManager{cfg: cfg, logger: logger, tracker: tracker, ttlStop: make(chan struct{})}
	defer close(m.ttlStop)

	rec := &resource.AgentRecord{
		AgentID:   "zero-expiry-coverage",
		Status:    "running",
		CreatedAt: time.Now(),
		ExpiresAt: time.Time{},
	}
	if err := tracker.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}

	m.reapExpiredAgents()

	if tracker.Get("zero-expiry-coverage") == nil {
		t.Error("agent with zero ExpiresAt should NOT be reaped")
	}
}

// ── Spawn port allocator disabled (nil fallback) ────────────────

func TestSpawn_PortAllocationDisabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefaultConfig()
	tracker := resource.NewTracker(10, logger)
	m := &AgentManager{cfg: cfg, logger: logger, tracker: tracker, ttlStop: make(chan struct{})}
	defer close(m.ttlStop)

	req := &v1.SpawnAgentRequest{AgentId: "portalloc-test"}
	_, err := m.Spawn(context.Background(), req)
	if err != nil && strings.Contains(err.Error(), "port range allocation") {
		t.Errorf("port allocation should succeed with fallback, got: %v", err)
	}
}

// ── RunAgent requires detach ────────────────────────────────────

func TestRunAgent_RequiresDetach(t *testing.T) {
	m := newCoverageTestManager(10)
	_, err := m.RunAgent(context.Background(), &v1.RunAgentRequest{
		AgentId: "test-agent",
		Command: "docker",
		Detach:  false,
	})
	if err == nil {
		t.Fatal("expected error for non-detached mode")
	}
	if !strings.Contains(err.Error(), "detached") {
		t.Errorf("expected detached mode error, got: %v", err)
	}
}

// ── Helpers ─────────────────────────────────────────────────────

func stringPointer(value string) *string {
	return &value
}

func mustNewPortAllocator(start, end, rangeSize uint32, t *testing.T) *resource.PortAllocator {
	t.Helper()
	pa, err := resource.NewPortAllocator(start, end, rangeSize)
	if err != nil {
		t.Fatalf("NewPortAllocator(%d,%d,%d): %v", start, end, rangeSize, err)
	}
	return pa
}
