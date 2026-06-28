package tunnel

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
)

// newTestManager creates a TunnelManager for testing with a mock cloudflared binary.
func newTestManager(t *testing.T, binaryPath string, enabled bool) *TunnelManager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &config.TunnelConfig{
		Enabled:        enabled,
		BinaryPath:     binaryPath,
		TunnelPort:     0, // disable metrics port in tests
		NoAutoupdate:   false,
		StartupTimeout: 5 * time.Second,
	}
	return NewTunnelManager(cfg, logger)
}

// writeMockCloudflared writes a shell script that mimics cloudflared's TryCloudflare output.
// If url is non-empty, the script prints the cloudflared banner with that URL and then sleeps.
// If exitAfter is non-zero, it exits after that duration.
func writeMockCloudflared(t *testing.T, dir, name, url string, sleepDuration time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)

	content := "#!/bin/sh\n"
	if url != "" {
		// Simulate real cloudflared output format
		content += fmt.Sprintf(`cat <<'EOF'
2026-06-28T00:00:00Z INF Starting cloudflared version 2024.6.1
2026-06-28T00:00:00Z INF Starting metrics server on 127.0.0.1:0
2026-06-28T00:00:00Z INF Registered tunnel connection
+--------------------------------------------------------------------------------------------+
|  Your quick tunnel has been created! Visit it at (it may take some time to be reachable):  |
|  %s                                                         |
+--------------------------------------------------------------------------------------------+
EOF
`, url)
	}

	if sleepDuration > 0 {
		content += fmt.Sprintf("sleep %d\n", int(sleepDuration.Seconds()))
	} else {
		content += "cat >/dev/null &\nwait\n" // hang forever
	}

	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("write mock cloudflared: %v", err)
	}
	return path
}

// TestTryCloudflareRegex tests URL parsing from cloudflared output.
func TestTryCloudflareRegex(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantURL string
	}{
		{
			name:    "standard banner",
			line:    "|  https://random-name.trycloudflare.com                                                         |",
			wantURL: "https://random-name.trycloudflare.com",
		},
		{
			name:    "just url",
			line:    "https://test123.trycloudflare.com",
			wantURL: "https://test123.trycloudflare.com",
		},
		{
			name:    "embedded in log line",
			line:    "2026-06-28T00:00:00Z INF Tunnel ready at https://my-agent.trycloudflare.com",
			wantURL: "https://my-agent.trycloudflare.com",
		},
		{
			name:    "no trycloudflare url",
			line:    "2026-06-28T00:00:00Z INF Starting tunnel",
			wantURL: "",
		},
		{
			name:    "non-matching url",
			line:    "https://example.com",
			wantURL: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tryCloudflareRe.FindString(tt.line)
			if got != tt.wantURL {
				t.Errorf("FindString(%q) = %q, want %q", tt.line, got, tt.wantURL)
			}
		})
	}
}

// TestStartStop tests the basic tunnel start/stop lifecycle.
func TestStartStop(t *testing.T) {
	dir := t.TempDir()
	url := "https://test-agent-001.trycloudflare.com"
	binaryPath := writeMockCloudflared(t, dir, "cloudflared", url, 0)

	mgr := newTestManager(t, binaryPath, true)

	publicURL, err := mgr.Start(t.Context(), "agent-001", 8080)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if publicURL != url {
		t.Errorf("publicURL = %q, want %q", publicURL, url)
	}

	// Verify the URL is cached
	cached := mgr.GetURL("agent-001")
	if cached != url {
		t.Errorf("GetURL = %q, want %q", cached, url)
	}

	// Stop the tunnel
	if err := mgr.Stop("agent-001"); err != nil {
		t.Errorf("Stop failed: %v", err)
	}

	// URL should be gone after stop
	cached = mgr.GetURL("agent-001")
	if cached != "" {
		t.Errorf("GetURL after stop = %q, want empty", cached)
	}
}

// TestStartDisabled tests that Start returns empty when tunnel is disabled.
func TestStartDisabled(t *testing.T) {
	mgr := newTestManager(t, "nonexistent-cloudflared", false)

	publicURL, err := mgr.Start(t.Context(), "agent-001", 8080)
	if err != nil {
		t.Fatalf("Start should not error when disabled: %v", err)
	}
	if publicURL != "" {
		t.Errorf("publicURL = %q, want empty when disabled", publicURL)
	}
}

// TestStartDuplicateAgent tests that starting a tunnel for an existing agent fails.
func TestStartDuplicateAgent(t *testing.T) {
	dir := t.TempDir()
	url := "https://dup-agent.trycloudflare.com"
	binaryPath := writeMockCloudflared(t, dir, "cloudflared", url, 0)

	mgr := newTestManager(t, binaryPath, true)

	_, err := mgr.Start(t.Context(), "agent-dup", 8080)
	if err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	defer mgr.Stop("agent-dup")

	_, err = mgr.Start(t.Context(), "agent-dup", 8080)
	if err == nil {
		t.Error("expected error for duplicate agent, got nil")
	}
}

// TestStartInvalidBinaryPath tests error handling when cloudflared doesn't exist.
func TestStartInvalidBinaryPath(t *testing.T) {
	mgr := newTestManager(t, "/nonexistent/path/to/cloudflared", true)

	_, err := mgr.Start(t.Context(), "agent-001", 8080)
	if err == nil {
		t.Error("expected error for nonexistent binary, got nil")
	}
}

// TestStartTimeout tests timeout when cloudflared doesn't print a URL.
func TestStartTimeout(t *testing.T) {
	dir := t.TempDir()
	// Script that prints NO trycloudflare URL and just sleeps
	writeMockCloudflared(t, dir, "cloudflared", "", 3*time.Second)

	mgr := newTestManager(t, pathToDirBinary(dir, "cloudflared"), true)
	mgr.cfg.StartupTimeout = 500 * time.Millisecond // short timeout for test

	_, err := mgr.Start(t.Context(), "agent-timeout", 8080)
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
}

func pathToDirBinary(dir, name string) string {
	return filepath.Join(dir, name)
}

// TestStopNonexistent tests that stopping a nonexistent tunnel is a no-op.
func TestStopNonexistent(t *testing.T) {
	mgr := newTestManager(t, "cloudflared", true)
	if err := mgr.Stop("nonexistent"); err != nil {
		t.Errorf("Stop nonexistent should not error: %v", err)
	}
}

// TestStartContextCancel tests that Start respects context cancellation.
func TestStartContextCancel(t *testing.T) {
	dir := t.TempDir()
	// Script that hangs forever (no URL output)
	writeMockCloudflared(t, dir, "cloudflared", "", 0)

	mgr := newTestManager(t, pathToDirBinary(dir, "cloudflared"), true)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	_, err := mgr.Start(ctx, "agent-cancel", 8080)
	if err == nil {
		t.Error("expected context cancellation error, got nil")
	}
}

// TestConcurrentStartStop tests concurrent tunnel creation/destruction.
func TestConcurrentStartStop(t *testing.T) {
	dir := t.TempDir()
	const numAgents = 5

	// Create a unique mock binary for each agent
	makeBinary := func(agentID string) string {
		url := fmt.Sprintf("https://%s.trycloudflare.com", agentID)
		return writeMockCloudflared(t, dir, fmt.Sprintf("cloudflared-%s", agentID), url, 0)
	}

	// Create per-agent managers with their own mock binaries
	type mgrAndAgent struct {
		mgr     *TunnelManager
		agentID string
	}
	mgrs := make([]mgrAndAgent, numAgents)
	for i := 0; i < numAgents; i++ {
		agentID := fmt.Sprintf("agent-%03d", i)
		binaryPath := makeBinary(agentID)
		mgrs[i] = mgrAndAgent{
			mgr:     newTestManager(t, binaryPath, true),
			agentID: agentID,
		}
	}

	// Start all concurrently
	var wg sync.WaitGroup
	errs := make([]error, numAgents)
	urls := make([]string, numAgents)
	for i := 0; i < numAgents; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			m := mgrs[idx]
			urls[idx], errs[idx] = m.mgr.Start(t.Context(), m.agentID, 8080+uint32(idx))
		}(i)
	}
	wg.Wait()

	// Verify all started successfully
	for i := 0; i < numAgents; i++ {
		if errs[i] != nil {
			t.Errorf("concurrent Start(%s) failed: %v", mgrs[i].agentID, errs[i])
		}
		if urls[i] == "" {
			t.Errorf("concurrent Start(%s) returned empty URL", mgrs[i].agentID)
		}
	}

	// Stop all
	for i := 0; i < numAgents; i++ {
		mgrs[i].mgr.Stop(mgrs[i].agentID)
	}
}

// TestGetURLNonexistent tests GetURL for nonexistent agent.
func TestGetURLNonexistent(t *testing.T) {
	mgr := newTestManager(t, "cloudflared", true)
	url := mgr.GetURL("nonexistent")
	if url != "" {
		t.Errorf("GetURL for nonexistent = %q, want empty", url)
	}
}

// TestShutdown tests that Shutdown stops all active tunnels.
func TestShutdown(t *testing.T) {
	dir := t.TempDir()

	_ = newTestManager(t, "unused", true) // unused — test uses per-agent localMgr

	for i := 0; i < 5; i++ {
		agentID := fmt.Sprintf("agent-%03d", i)
		url := fmt.Sprintf("https://%s.trycloudflare.com", agentID)
		binaryPath := writeMockCloudflared(t, dir, fmt.Sprintf("cloudflared-%s", agentID), url, 0)

		localMgr := newTestManager(t, binaryPath, true)
		_, err := localMgr.Start(t.Context(), agentID, 8080+uint32(i))
		if err != nil {
			t.Fatalf("Start(%s) failed: %v", agentID, err)
		}
		// Shutdown each individually (they're separate managers in this test)
		localMgr.Shutdown(t.Context())

		cached := localMgr.GetURL(agentID)
		if cached != "" {
			t.Errorf("GetURL after Shutdown = %q, want empty", cached)
		}
	}
}

// --- Named Tunnel Tests ---

// newTestManagerWithNamed creates a TunnelManager with named tunnel support for testing.
func newTestManagerWithNamed(t *testing.T, binaryPath string, namedEnabled bool, namedDomain string, credsFile string) *TunnelManager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &config.TunnelConfig{
		Enabled:        true,
		BinaryPath:     binaryPath,
		TunnelPort:     0,
		NoAutoupdate:   false,
		StartupTimeout: 5 * time.Second,
	}
	namedCfg := &config.NamedTunnelConfig{
		Enabled:         namedEnabled,
		Name:            "test-tunnel",
		CredentialsFile: credsFile,
		Domain:          namedDomain,
	}
	return NewTunnelManagerWithNamed(cfg, namedCfg, logger)
}

// writeMockCloudflaredNamed writes a shell script that mimics cloudflared named tunnel output.
// For named tunnels, cloudflared writes log lines but not a trycloudflare.com URL.
func writeMockCloudflaredNamed(t *testing.T, dir, name string, sleepDuration time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)

	content := "#!/bin/sh\n"
	content += "cat <<'EOF'\n"
	content += "2026-06-28T00:00:00Z INF Starting cloudflared version 2024.6.1\n"
	content += "2026-06-28T00:00:00Z INF Registered tunnel connection\n"
	content += "2026-06-28T00:00:00Z INF Updated configuration for tunnel test-tunnel\n"
	content += "EOF\n"

	if sleepDuration > 0 {
		content += fmt.Sprintf("sleep %d\n", int(sleepDuration.Seconds()))
	} else {
		content += "sleep 999999\n" // hang until killed
	}

	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("write mock cloudflared named: %v", err)
	}
	return path
}

// TestStartNamedStop tests basic named tunnel start/stop lifecycle.
func TestStartNamedStop(t *testing.T) {
	dir := t.TempDir()
	credsFile := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(credsFile, []byte(`{"AccountTag":"test","TunnelID":"test-uuid","TunnelSecret":"dGVzdA=="}`), 0644); err != nil {
		t.Fatalf("write creds: %v", err)
	}

	binaryPath := writeMockCloudflaredNamed(t, dir, "cloudflared", 0)
	mgr := newTestManagerWithNamed(t, binaryPath, true, "agent.example.com", credsFile)

	domain, err := mgr.StartNamed(t.Context(), "agent-named-001", 8080)
	if err != nil {
		t.Fatalf("StartNamed failed: %v", err)
	}
	if domain != "agent.example.com" {
		t.Errorf("domain = %q, want %q", domain, "agent.example.com")
	}

	// Verify the domain is cached via GetURL
	cached := mgr.GetURL("agent-named-001")
	if cached != "agent.example.com" {
		t.Errorf("GetURL = %q, want %q", cached, "agent.example.com")
	}

	// Stop the tunnel
	if err := mgr.Stop("agent-named-001"); err != nil {
		t.Errorf("Stop failed: %v", err)
	}

	// URL should be gone
	cached = mgr.GetURL("agent-named-001")
	if cached != "" {
		t.Errorf("GetURL after stop = %q, want empty", cached)
	}
}

// TestStartNamedDisabled tests that StartNamed returns empty when disabled.
func TestStartNamedDisabled(t *testing.T) {
	dir := t.TempDir()
	binaryPath := writeMockCloudflaredNamed(t, dir, "cloudflared", 0)
	mgr := newTestManagerWithNamed(t, binaryPath, false, "", "")

	domain, err := mgr.StartNamed(t.Context(), "agent-001", 8080)
	if err != nil {
		t.Fatalf("StartNamed should not error when disabled: %v", err)
	}
	if domain != "" {
		t.Errorf("domain = %q, want empty when disabled", domain)
	}
}

// TestStartNamedDuplicateAgent tests that starting a named tunnel for an existing agent fails.
func TestStartNamedDuplicateAgent(t *testing.T) {
	dir := t.TempDir()
	credsFile := filepath.Join(dir, "creds.json")
	os.WriteFile(credsFile, []byte(`{"AccountTag":"test","TunnelID":"test-uuid","TunnelSecret":"dGVzdA=="}`), 0644)

	binaryPath := writeMockCloudflaredNamed(t, dir, "cloudflared", 0)
	mgr := newTestManagerWithNamed(t, binaryPath, true, "dup.example.com", credsFile)

	_, err := mgr.StartNamed(t.Context(), "agent-dup", 8080)
	if err != nil {
		t.Fatalf("first StartNamed failed: %v", err)
	}
	defer mgr.Stop("agent-dup")

	_, err = mgr.StartNamed(t.Context(), "agent-dup", 8080)
	if err == nil {
		t.Error("expected error for duplicate agent, got nil")
	}
}

// TestStartNamedMissingCredentialsFile tests error when credentials file is missing.
func TestStartNamedMissingCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	binaryPath := writeMockCloudflaredNamed(t, dir, "cloudflared", 0)
	mgr := newTestManagerWithNamed(t, binaryPath, true, "example.com", "/nonexistent/creds.json")

	_, err := mgr.StartNamed(t.Context(), "agent-001", 8080)
	if err == nil {
		t.Error("expected error for missing credentials file, got nil")
	}
}

// TestNamedGetURLNonexistent tests GetURL for nonexistent named agent.
func TestNamedGetURLNonexistent(t *testing.T) {
	dir := t.TempDir()
	binaryPath := writeMockCloudflaredNamed(t, dir, "cloudflared", 0)
	mgr := newTestManagerWithNamed(t, binaryPath, true, "example.com", filepath.Join(dir, "creds.json"))

	url := mgr.GetURL("nonexistent")
	if url != "" {
		t.Errorf("GetURL for nonexistent = %q, want empty", url)
	}
}

// TestStartNamedContextCancel tests that StartNamed respects context cancellation.
func TestStartNamedContextCancel(t *testing.T) {
	dir := t.TempDir()
	credsFile := filepath.Join(dir, "creds.json")
	os.WriteFile(credsFile, []byte(`{"AccountTag":"test","TunnelID":"test-uuid","TunnelSecret":"dGVzdA=="}`), 0644)

	// Script that hangs forever
	binaryPath := writeMockCloudflaredNamed(t, dir, "cloudflared", 0)
	mgr := newTestManagerWithNamed(t, binaryPath, true, "example.com", credsFile)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	_, err := mgr.StartNamed(ctx, "agent-cancel", 8080)
	if err == nil {
		t.Error("expected context cancellation error, got nil")
	}
}

// TestStartNamedTimeout tests timeout when cloudflared doesn't print the ready line.
// Named tunnels succeed after the startup timeout (process is still running).
func TestStartNamedTimeout(t *testing.T) {
	dir := t.TempDir()
	credsFile := filepath.Join(dir, "creds.json")
	os.WriteFile(credsFile, []byte(`{"AccountTag":"test","TunnelID":"test-uuid","TunnelSecret":"dGVzdA=="}`), 0644)

	// Script that hangs forever (no exit, no URL)
	binaryPath := writeMockCloudflaredNamed(t, dir, "cloudflared", 0)
	mgr := newTestManagerWithNamed(t, binaryPath, true, "mydomain.example.com", credsFile)
	mgr.cfg.StartupTimeout = 200 * time.Millisecond // short timeout

	domain, err := mgr.StartNamed(t.Context(), "agent-timeout", 8080)
	if err != nil {
		t.Fatalf("StartNamed should succeed after timeout (process still running): %v", err)
	}
	if domain != "mydomain.example.com" {
		t.Errorf("domain = %q, want %q", domain, "mydomain.example.com")
	}

	// Clean up
	mgr.Stop("agent-timeout")
}

// TestShutdownNamed tests that Shutdown stops named tunnels.
func TestShutdownNamed(t *testing.T) {
	dir := t.TempDir()
	credsFile := filepath.Join(dir, "creds.json")
	os.WriteFile(credsFile, []byte(`{"AccountTag":"test","TunnelID":"test-uuid","TunnelSecret":"dGVzdA=="}`), 0644)

	namedBinary := writeMockCloudflaredNamed(t, dir, "cloudflared-named", 0)

	namedMgr := newTestManagerWithNamed(t, namedBinary, true, "custom.example.com", credsFile)

	_, err := namedMgr.StartNamed(t.Context(), "agent-a", 8080)
	if err != nil {
		t.Fatalf("StartNamed failed: %v", err)
	}
	_, err = namedMgr.StartNamed(t.Context(), "agent-b", 8081)
	if err != nil {
		t.Fatalf("StartNamed failed: %v", err)
	}

	namedMgr.Shutdown(t.Context())

	if url := namedMgr.GetURL("agent-a"); url != "" {
		t.Errorf("GetURL(agent-a) after shutdown = %q, want empty", url)
	}
	if url := namedMgr.GetURL("agent-b"); url != "" {
		t.Errorf("GetURL(agent-b) after shutdown = %q, want empty", url)
	}
}

// TestHasNamedTunnel tests the HasNamedTunnel helper.
func TestHasNamedTunnel(t *testing.T) {
	dir := t.TempDir()
	binaryPath := writeMockCloudflaredNamed(t, dir, "cloudflared", 0)

	// Without named config
	mgr := newTestManager(t, binaryPath, true)
	if mgr.HasNamedTunnel() {
		t.Error("HasNamedTunnel should be false without named config")
	}

	// With named config enabled
	mgrWithNamed := newTestManagerWithNamed(t, binaryPath, true, "example.com", filepath.Join(dir, "creds.json"))
	if !mgrWithNamed.HasNamedTunnel() {
		t.Error("HasNamedTunnel should be true with named config enabled")
	}

	// With named config disabled
	mgrDisabled := newTestManagerWithNamed(t, binaryPath, false, "example.com", filepath.Join(dir, "creds.json"))
	if mgrDisabled.HasNamedTunnel() {
		t.Error("HasNamedTunnel should be false when disabled")
	}
}
