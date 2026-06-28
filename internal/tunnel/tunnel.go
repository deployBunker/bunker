// Package tunnel manages Cloudflare TryCloudflare anonymous tunnels for per-agent public URLs.
package tunnel

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
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

// TunnelManager manages Cloudflare TryCloudflare anonymous tunnels.
type TunnelManager struct {
	cfg     *config.TunnelConfig
	logger  *slog.Logger
	mu      sync.Mutex
	tunnels map[string]*runningTunnel
}

// NewTunnelManager creates a new TunnelManager.
func NewTunnelManager(cfg *config.TunnelConfig, logger *slog.Logger) *TunnelManager {
	return &TunnelManager{
		cfg:     cfg,
		logger:  logger,
		tunnels: make(map[string]*runningTunnel),
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

// Stop terminates the cloudflared tunnel for the given agent.
func (m *TunnelManager) Stop(agentID string) error {
	m.mu.Lock()
	rt, exists := m.tunnels[agentID]
	if !exists {
		m.mu.Unlock()
		return nil // already stopped or never started
	}
	delete(m.tunnels, agentID)
	m.mu.Unlock()

	rt.cancel()
	err := rt.cmd.Wait()
	if err != nil {
		// cloudflared exits with non-zero when killed; this is expected
		m.logger.Debug("cloudflared exited", "agent_id", agentID, "error", err)
	}

	m.logger.Info("TryCloudflare tunnel stopped", "agent_id", agentID)
	return nil
}

// GetURL returns the cached public URL for the given agent, or empty string if none.
func (m *TunnelManager) GetURL(agentID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rt, exists := m.tunnels[agentID]; exists {
		return rt.PublicURL
	}
	return ""
}

// Shutdown stops all active tunnels.
func (m *TunnelManager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.tunnels))
	for id := range m.tunnels {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		if err := m.Stop(id); err != nil {
			m.logger.Warn("tunnel shutdown error", "agent_id", id, "error", err)
		}
	}
	m.logger.Info("all tunnels shut down")
}
