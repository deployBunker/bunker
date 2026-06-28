// Package tunnel manages Cloudflare TryCloudflare anonymous and named tunnels for per-agent public URLs.
package tunnel

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/deployBunker/bunker/internal/config"
)

// tryCloudflareRe matches anonymous TryCloudflare tunnel URLs in cloudflared stdout.
var tryCloudflareRe = regexp.MustCompile(`https://[a-zA-Z0-9][-a-zA-Z0-9]*\.trycloudflare\.com`)

// defaultTimeout is the maximum time to wait for cloudflared to print a URL.
const defaultTimeout = 30 * time.Second

// runningTunnel tracks an active cloudflared process for a single agent.
type runningTunnel struct {
	AgentID   string
	PublicURL string
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	port      uint32
}

// TunnelManager manages Cloudflare tunnels (anonymous TryCloudflare + named tunnels).
type TunnelManager struct {
	cfg     *config.TunnelConfig
	logger  *slog.Logger
	mu      sync.Mutex
	tunnels map[string]*runningTunnel

	namedCfg     *config.NamedTunnelConfig
	namedTunnels map[string]*runningTunnel
}

// NewTunnelManager creates a new TunnelManager.
func NewTunnelManager(cfg *config.TunnelConfig, logger *slog.Logger) *TunnelManager {
	return &TunnelManager{
		cfg:     cfg,
		logger:  logger,
		tunnels: make(map[string]*runningTunnel),
	}
}

// NewTunnelManagerWithNamed creates a TunnelManager with named tunnel support.
func NewTunnelManagerWithNamed(cfg *config.TunnelConfig, namedCfg *config.NamedTunnelConfig, logger *slog.Logger) *TunnelManager {
	return &TunnelManager{
		cfg:          cfg,
		namedCfg:     namedCfg,
		logger:       logger,
		tunnels:      make(map[string]*runningTunnel),
		namedTunnels: make(map[string]*runningTunnel),
	}
}

// Start launches a cloudflared anonymous tunnel for the given agent.
// It runs `cloudflared tunnel --url http://localhost:<port>`, parses the
// trycloudflare.com URL from stdout, and keeps the process running.
// Returns the public URL on success.
func (m *TunnelManager) Start(ctx context.Context, agentID string, localPort uint32) (string, error) {
	if !m.cfg.Enabled {
		m.logger.Debug("tunnel disabled, skipping start", "agent_id", agentID)
		return "", nil
	}

	m.mu.Lock()
	if _, exists := m.tunnels[agentID]; exists {
		m.mu.Unlock()
		return "", fmt.Errorf("tunnel already exists for agent %q", agentID)
	}
	m.mu.Unlock()

	// Build the command.
	target := fmt.Sprintf("http://localhost:%d", localPort)
	args := []string{"tunnel", "--url", target}

	if m.cfg.TunnelPort > 0 {
		args = append(args, "--metrics", fmt.Sprintf("localhost:%d", m.cfg.TunnelPort))
	}
	if m.cfg.NoAutoupdate {
		args = append(args, "--no-autoupdate")
	}

	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, m.cfg.BinaryPath, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", fmt.Errorf("cloudflared stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout // cloudflared writes the URL banner to stdout; merge stderr for completeness

	if err := cmd.Start(); err != nil {
		cancel()
		return "", fmt.Errorf("start cloudflared: %w", err)
	}

	// Scan stdout for the TryCloudflare URL, with a timeout.
	type result struct {
		url string
		err error
	}
	resultCh := make(chan result, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			m.logger.Debug("cloudflared", "agent_id", agentID, "line", line)
			if match := tryCloudflareRe.FindString(line); match != "" {
				resultCh <- result{url: match}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			resultCh <- result{err: fmt.Errorf("cloudflared stdout scan: %w", err)}
		} else {
			resultCh <- result{err: fmt.Errorf("cloudflared exited without printing a tunnel URL")}
		}
	}()

	timeout := defaultTimeout
	if m.cfg.StartupTimeout > 0 {
		timeout = m.cfg.StartupTimeout
	}

	var publicURL string
	select {
	case res := <-resultCh:
		if res.err != nil {
			cancel()
			cmd.Wait() //nolint:errcheck
			return "", res.err
		}
		publicURL = res.url
	case <-time.After(timeout):
		cancel()
		cmd.Wait() //nolint:errcheck
		return "", fmt.Errorf("timeout waiting for TryCloudflare URL after %v", timeout)
	case <-ctx.Done():
		cancel()
		cmd.Wait() //nolint:errcheck
		return "", ctx.Err()
	}

	rt := &runningTunnel{
		AgentID:   agentID,
		PublicURL: publicURL,
		cmd:       cmd,
		cancel:    cancel,
		port:      localPort,
	}

	m.mu.Lock()
	m.tunnels[agentID] = rt
	m.mu.Unlock()

	m.logger.Info("TryCloudflare tunnel started",
		"agent_id", agentID,
		"public_url", publicURL,
		"local_port", localPort,
	)

	return publicURL, nil
}

// StartNamed launches a Cloudflare named tunnel for the given agent.
// It runs `cloudflared tunnel run <name> --credentials-file <path> --url http://localhost:<port>`.
// Named tunnels route traffic from a pre-configured custom domain to the local port.
// Returns the custom domain on success.
func (m *TunnelManager) StartNamed(ctx context.Context, agentID string, localPort uint32) (string, error) {
	if m.namedCfg == nil || !m.namedCfg.Enabled {
		m.logger.Debug("named tunnel disabled, skipping start", "agent_id", agentID)
		return "", nil
	}

	// Verify the credentials file exists before launching
	if !fileExists(m.namedCfg.CredentialsFile) {
		return "", fmt.Errorf("named tunnel credentials file not found: %s", m.namedCfg.CredentialsFile)
	}

	m.mu.Lock()
	if _, exists := m.namedTunnels[agentID]; exists {
		m.mu.Unlock()
		return "", fmt.Errorf("named tunnel already exists for agent %q", agentID)
	}
	m.mu.Unlock()

	// Build the command: cloudflared tunnel run <name> --credentials-file <path> --url <target>
	target := fmt.Sprintf("http://localhost:%d", localPort)
	args := []string{
		"tunnel", "run",
		m.namedCfg.Name,
		"--credentials-file", m.namedCfg.CredentialsFile,
		"--url", target,
	}

	if m.cfg.NoAutoupdate {
		args = append(args, "--no-autoupdate")
	}

	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, m.cfg.BinaryPath, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", fmt.Errorf("cloudflared stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		cancel()
		return "", fmt.Errorf("start cloudflared named tunnel: %w", err)
	}

	// Wait for cloudflared to print a "Registered tunnel connection" line, with timeout.
	type result struct {
		err error
	}
	resultCh := make(chan result, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			m.logger.Debug("cloudflared named", "agent_id", agentID, "line", line)
		}
		if err := scanner.Err(); err != nil {
			resultCh <- result{err: fmt.Errorf("cloudflared stdout scan: %w", err)}
		} else {
			resultCh <- result{err: fmt.Errorf("cloudflared named tunnel exited unexpectedly")}
		}
	}()

	timeout := defaultTimeout
	if m.cfg.StartupTimeout > 0 {
		timeout = m.cfg.StartupTimeout
	}

	select {
	case res := <-resultCh:
		cancel()
		cmd.Wait() //nolint:errcheck
		return "", res.err
	case <-time.After(timeout):
		// Named tunnel doesn't print the URL — it uses the pre-configured domain.
		// After the timeout, assume the tunnel started successfully if the process is still running.
		domain := m.namedCfg.Domain
		rt := &runningTunnel{
			AgentID:   agentID,
			PublicURL: domain,
			cmd:       cmd,
			cancel:    cancel,
			port:      localPort,
		}

		m.mu.Lock()
		m.namedTunnels[agentID] = rt
		m.mu.Unlock()

		m.logger.Info("named tunnel started",
			"agent_id", agentID,
			"name", m.namedCfg.Name,
			"domain", domain,
			"local_port", localPort,
		)

		return domain, nil
	case <-ctx.Done():
		cancel()
		cmd.Wait() //nolint:errcheck
		return "", ctx.Err()
	}
}

// Stop terminates the cloudflared tunnel for the given agent (anonymous or named).
func (m *TunnelManager) Stop(agentID string) error {
	// Check anonymous tunnels first
	m.mu.Lock()
	rt, exists := m.tunnels[agentID]
	if exists {
		delete(m.tunnels, agentID)
		m.mu.Unlock()

		rt.cancel()
		err := rt.cmd.Wait()
		if err != nil {
			m.logger.Debug("cloudflared exited", "agent_id", agentID, "error", err)
		}
		m.logger.Info("TryCloudflare tunnel stopped", "agent_id", agentID)
		return nil
	}

	// Check named tunnels
	nrt, namedExists := m.namedTunnels[agentID]
	if namedExists {
		delete(m.namedTunnels, agentID)
		m.mu.Unlock()

		nrt.cancel()
		err := nrt.cmd.Wait()
		if err != nil {
			m.logger.Debug("cloudflared named exited", "agent_id", agentID, "error", err)
		}
		m.logger.Info("named tunnel stopped", "agent_id", agentID)
		return nil
	}
	m.mu.Unlock()

	return nil // already stopped or never started
}

// GetURL returns the cached public URL for the given agent, or empty string if none.
// Checks both anonymous and named tunnels.
func (m *TunnelManager) GetURL(agentID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rt, exists := m.tunnels[agentID]; exists {
		return rt.PublicURL
	}
	if rt, exists := m.namedTunnels[agentID]; exists {
		return rt.PublicURL
	}
	return ""
}

// Shutdown stops all active tunnels.
func (m *TunnelManager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	anonIDs := make([]string, 0, len(m.tunnels))
	for id := range m.tunnels {
		anonIDs = append(anonIDs, id)
	}
	namedIDs := make([]string, 0, len(m.namedTunnels))
	for id := range m.namedTunnels {
		namedIDs = append(namedIDs, id)
	}
	m.mu.Unlock()

	for _, id := range anonIDs {
		if err := m.Stop(id); err != nil {
			m.logger.Warn("tunnel shutdown error", "agent_id", id, "error", err)
		}
	}
	for _, id := range namedIDs {
		if err := m.Stop(id); err != nil {
			m.logger.Warn("named tunnel shutdown error", "agent_id", id, "error", err)
		}
	}
	m.logger.Info("all tunnels shut down")
}

// HasNamedTunnel returns true if the manager supports named tunnels.
func (m *TunnelManager) HasNamedTunnel() bool {
	return m.namedCfg != nil && m.namedCfg.Enabled
}

// fileExists checks if a file exists and is readable.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
