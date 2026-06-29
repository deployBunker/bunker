// Package tailscale manages Tailscale mesh networking for per-agent tailnet IPs.
package tailscale

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/deployBunker/bunker/internal/config"
)

// defaultTimeout is the maximum time to wait for tailscale up to complete.
const defaultTimeout = 30 * time.Second

// runningTailscale tracks an active tailscale connection for a single agent.
type runningTailscale struct {
	AgentID   string
	TailnetIP string
	cancel    context.CancelFunc
}

// TailscaleManager manages Tailscale per-agent connections.
type TailscaleManager struct {
	cfg    *config.TailscaleConfig
	logger *slog.Logger
	mu     sync.Mutex
	nodes  map[string]*runningTailscale
}

// NewTailscaleManager creates a new TailscaleManager.
func NewTailscaleManager(cfg *config.TailscaleConfig, logger *slog.Logger) *TailscaleManager {
	return &TailscaleManager{
		cfg:    cfg,
		logger: logger,
		nodes:  make(map[string]*runningTailscale),
	}
}

// Start connects an agent to the tailnet via `tailscale up --authkey=X --hostname=bunker-<agentID>`.
// After tailscale up completes, it runs `tailscale ip -4` to get the tailnet IP.
// Returns the tailnet IPv4 address on success.
func (m *TailscaleManager) Start(ctx context.Context, agentID string) (string, error) {
	if !m.cfg.Enabled {
		m.logger.Debug("tailscale disabled, skipping start", "agent_id", agentID)
		return "", nil
	}

	m.mu.Lock()
	if _, exists := m.nodes[agentID]; exists {
		m.mu.Unlock()
		return "", fmt.Errorf("tailscale already connected for agent %q", agentID)
	}
	m.mu.Unlock()

	hostname := "bunker-" + agentID

	// Build the tailscale up command.
	args := []string{"up", "--authkey=" + m.cfg.AuthKey, "--hostname=" + hostname}

	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, m.cfg.BinaryPath, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", fmt.Errorf("tailscale stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		cancel()
		return "", fmt.Errorf("start tailscale up: %w", err)
	}

	// Scan stdout for completion, with a timeout.
	type result struct {
		err error
	}
	resultCh := make(chan result, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			m.logger.Debug("tailscale up", "agent_id", agentID, "line", line)
		}
		if err := scanner.Err(); err != nil {
			resultCh <- result{err: fmt.Errorf("tailscale up stdout scan: %w", err)}
		} else {
			resultCh <- result{} // success — tailscale up exited cleanly
		}
	}()

	timeout := defaultTimeout
	if m.cfg.StartupTimeout > 0 {
		timeout = m.cfg.StartupTimeout
	}

	select {
	case res := <-resultCh:
		if res.err != nil {
			cancel()
			cmd.Wait() //nolint:errcheck
			return "", res.err
		}
		// tailscale up exited cleanly; now get the IP
	case <-time.After(timeout):
		cancel()
		cmd.Wait() //nolint:errcheck
		return "", fmt.Errorf("timeout waiting for tailscale up after %v", timeout)
	case <-ctx.Done():
		cancel()
		cmd.Wait() //nolint:errcheck
		return "", ctx.Err()
	}

	// tailscale up succeeded; get the tailnet IPv4 address.
	tailnetIP, err := m.getTailscaleIP(ctx)
	if err != nil {
		cancel()
		return "", fmt.Errorf("get tailscale IP: %w", err)
	}

	rt := &runningTailscale{
		AgentID:   agentID,
		TailnetIP: tailnetIP,
		cancel:    cancel,
	}

	m.mu.Lock()
	m.nodes[agentID] = rt
	m.mu.Unlock()

	m.logger.Info("tailscale connected",
		"agent_id", agentID,
		"hostname", hostname,
		"tailnet_ip", tailnetIP,
	)

	return tailnetIP, nil
}

// getTailscaleIP runs `tailscale ip -4` and returns the parsed IPv4 address.
func (m *TailscaleManager) getTailscaleIP(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, m.cfg.BinaryPath, "ip", "-4")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tailscale ip -4: %w", err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("tailscale ip -4 returned empty output")
	}
	return ip, nil
}

// Stop disconnects the agent from the tailnet via `tailscale down`.
func (m *TailscaleManager) Stop(agentID string) error {
	m.mu.Lock()
	rt, exists := m.nodes[agentID]
	if exists {
		delete(m.nodes, agentID)
	}
	m.mu.Unlock()

	if !exists {
		return nil // already stopped or never started
	}

	if rt.cancel != nil {
		rt.cancel()
	}

	// Run tailscale down to disconnect.
	// We use a background context since the agent's context may be cancelled already.
	downCtx, downCancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer downCancel()

	cmd := exec.CommandContext(downCtx, m.cfg.BinaryPath, "down")
	if out, err := cmd.CombinedOutput(); err != nil {
		m.logger.Debug("tailscale down", "agent_id", agentID, "error", err, "output", string(out))
	} else {
		m.logger.Info("tailscale disconnected", "agent_id", agentID)
	}

	return nil
}

// GetIP returns the cached tailnet IP for the given agent, or empty string if none.
func (m *TailscaleManager) GetIP(agentID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rt, exists := m.nodes[agentID]; exists {
		return rt.TailnetIP
	}
	return ""
}

// Shutdown stops all active tailscale connections.
func (m *TailscaleManager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.nodes))
	for id := range m.nodes {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		if err := m.Stop(id); err != nil {
			m.logger.Warn("tailscale shutdown error", "agent_id", id, "error", err)
		}
	}
	m.logger.Info("all tailscale connections shut down")
}
