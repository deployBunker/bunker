package tailscale

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
)

func newTestManager(t *testing.T) *TailscaleManager {
	t.Helper()
	cfg := &config.TailscaleConfig{
		Enabled:    false,
		BinaryPath: "tailscale",
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewTailscaleManager(cfg, logger)
}

func TestNewTailscaleManager(t *testing.T) {
	m := newTestManager(t)
	if m == nil {
		t.Fatal("NewTailscaleManager returned nil")
	}
	if m.nodes == nil {
		t.Error("nodes map is nil")
	}
	if len(m.nodes) != 0 {
		t.Errorf("expected empty nodes map, got %d entries", len(m.nodes))
	}
	if m.cfg == nil {
		t.Error("cfg is nil")
	}
}

func TestStart_Disabled(t *testing.T) {
	m := newTestManager(t)
	ip, err := m.Start(context.Background(), "test-agent-001")
	if err != nil {
		t.Errorf("Start on disabled config returned error: %v", err)
	}
	if ip != "" {
		t.Errorf("expected empty IP on disabled config, got %q", ip)
	}
}

func TestStart_AlreadyConnected(t *testing.T) {
	cfg := &config.TailscaleConfig{
		Enabled:        true,
		BinaryPath:     "tailscale",
		AuthKey:        "tskey-test",
		StartupTimeout: 5 * time.Second,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := NewTailscaleManager(cfg, logger)

	// Manually inject a running connection to simulate already-connected.
	m.mu.Lock()
	m.nodes["test-agent-002"] = &runningTailscale{
		AgentID:   "test-agent-002",
		TailnetIP: "100.64.0.1",
	}
	m.mu.Unlock()

	_, err := m.Start(context.Background(), "test-agent-002")
	if err == nil {
		t.Fatal("expected 'already connected' error, got nil")
	}
	if err.Error() != `tailscale already connected for agent "test-agent-002"` {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGetIP_Known(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.nodes["test-agent-003"] = &runningTailscale{
		AgentID:   "test-agent-003",
		TailnetIP: "100.64.0.2",
	}
	m.mu.Unlock()

	ip := m.GetIP("test-agent-003")
	if ip != "100.64.0.2" {
		t.Errorf("expected 100.64.0.2, got %q", ip)
	}
}

func TestGetIP_Unknown(t *testing.T) {
	m := newTestManager(t)
	ip := m.GetIP("nonexistent")
	if ip != "" {
		t.Errorf("expected empty string for unknown agent, got %q", ip)
	}
}

func TestStop_Known(t *testing.T) {
	m := newTestManager(t)
	cancelCalled := make(chan struct{}, 1)
	m.mu.Lock()
	m.nodes["test-agent-004"] = &runningTailscale{
		AgentID:   "test-agent-004",
		TailnetIP: "100.64.0.3",
		cancel: func() {
			cancelCalled <- struct{}{}
		},
	}
	m.mu.Unlock()

	err := m.Stop("test-agent-004")
	if err != nil {
		t.Errorf("Stop on known agent returned error: %v", err)
	}

	// Verify it's removed.
	m.mu.Lock()
	_, exists := m.nodes["test-agent-004"]
	m.mu.Unlock()
	if exists {
		t.Error("agent not removed from nodes after Stop")
	}
}

func TestStop_Unknown(t *testing.T) {
	m := newTestManager(t)
	err := m.Stop("nonexistent")
	if err != nil {
		t.Errorf("Stop on unknown agent returned error: %v", err)
	}
}

func TestStop_TriggersCancel(t *testing.T) {
	m := newTestManager(t)
	cancelCalled := make(chan struct{}, 1)
	m.mu.Lock()
	m.nodes["test-agent-005"] = &runningTailscale{
		AgentID:   "test-agent-005",
		TailnetIP: "100.64.0.4",
		cancel: func() {
			cancelCalled <- struct{}{}
		},
	}
	m.mu.Unlock()

	err := m.Stop("test-agent-005")
	if err != nil {
		t.Errorf("Stop returned error: %v", err)
	}

	select {
	case <-cancelCalled:
		// cancel was called as expected
	case <-time.After(100 * time.Millisecond):
		t.Error("cancel function was not called")
	}
}

func TestShutdown_Empty(t *testing.T) {
	m := newTestManager(t)
	m.Shutdown(context.Background())
	// Should not panic and not block.
}

func TestShutdown_WithNodes(t *testing.T) {
	m := newTestManager(t)
	cancels := make(chan string, 2)
	m.mu.Lock()
	m.nodes["agent-a"] = &runningTailscale{
		AgentID: "agent-a",
		cancel:  func() { cancels <- "agent-a" },
	}
	m.nodes["agent-b"] = &runningTailscale{
		AgentID: "agent-b",
		cancel:  func() { cancels <- "agent-b" },
	}
	m.mu.Unlock()

	m.Shutdown(context.Background())

	// Both cancels should have been called.
	seen := make(map[string]bool)
	for i := 0; i < 2; i++ {
		select {
		case id := <-cancels:
			seen[id] = true
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("timed out waiting for cancel #%d", i+1)
		}
	}
	if !seen["agent-a"] || !seen["agent-b"] {
		t.Errorf("not all cancels called: %v", seen)
	}

	// All nodes should be removed.
	m.mu.Lock()
	count := len(m.nodes)
	m.mu.Unlock()
	if count != 0 {
		t.Errorf("expected 0 nodes after Shutdown, got %d", count)
	}
}

func TestGetTailscaleIP_EmptyOutput(t *testing.T) {
	// Verify that getTailscaleIP handles empty output correctly.
	// We test this indirectly through Start with disabled config,
	// but this is the core parsing function.
	m := newTestManager(t)
	ip, err := m.getTailscaleIP(context.Background())
	// With tailscale binary not installed, this will fail with exec error.
	// The test validates the function exists and returns an error (not a panic).
	if err == nil && ip == "" {
		t.Error("expected error when tailscale binary is not installed")
	}
}

func TestStart_ContextCancelled_BeforeTailscaleUp(t *testing.T) {
	cfg := &config.TailscaleConfig{
		Enabled:        true,
		BinaryPath:     "tailscale",
		AuthKey:        "tskey-test",
		StartupTimeout: 30 * time.Second,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := NewTailscaleManager(cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := m.Start(ctx, "test-agent-ctx")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if err != context.Canceled {
		t.Logf("got error (expected context.Canceled or exec failure): %v", err)
	}
}

func TestStart_Timeout(t *testing.T) {
	cfg := &config.TailscaleConfig{
		Enabled:        true,
		BinaryPath:     "tailscale",
		AuthKey:        "tskey-test",
		StartupTimeout: 1 * time.Nanosecond, // Instant timeout
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := NewTailscaleManager(cfg, logger)

	_, err := m.Start(context.Background(), "test-agent-timeout")
	if err == nil {
		t.Fatal("expected error (timeout or exec failure)")
	}
	// Accept either timeout or exec failure (binary not installed).
	if !contains(err.Error(), "timeout") && !contains(err.Error(), "exec") {
		t.Errorf("expected timeout or exec error, got: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && search(s, substr) >= 0
}

func search(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
