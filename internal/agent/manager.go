// Package agent manages agent lifecycle: create users, generate SSH keys, start dockerd.
package agent

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	"github.com/deployBunker/bunker/internal/tailscale"
	"github.com/deployBunker/bunker/internal/tunnel"
)

// validAgentID matches agent IDs: lowercase, digits, hyphens, 1-63 chars.
var validAgentID = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// AgentManager handles the agent spawn lifecycle.
type AgentManager struct {
	cfg          *config.Config
	logger       *slog.Logger
	tracker      *resource.Tracker
	portAlloc    *resource.PortAllocator
	tunnelMgr    *tunnel.TunnelManager
	tailscaleMgr *tailscale.TailscaleManager
	ttlStop      chan struct{}
}

// NewAgentManager creates a new AgentManager.
func NewAgentManager(cfg *config.Config, logger *slog.Logger, tracker *resource.Tracker, tunnelMgr *tunnel.TunnelManager, tailscaleMgr *tailscale.TailscaleManager) *AgentManager {
	pa, err := resource.NewPortAllocator(
		cfg.Agent.PortRangeStart,
		cfg.Agent.PortRangeEnd,
		cfg.Agent.PortRangePerAgent,
	)
	if err != nil {
		logger.Warn("port allocator disabled — invalid port range config",
			"start", cfg.Agent.PortRangeStart,
			"end", cfg.Agent.PortRangeEnd,
			"per_agent", cfg.Agent.PortRangePerAgent,
			"error", err,
		)
		// Port allocator is nil when disabled; spawn will use the full range as fallback.
	}
	am := &AgentManager{cfg: cfg, logger: logger, tracker: tracker, portAlloc: pa, tunnelMgr: tunnelMgr, tailscaleMgr: tailscaleMgr, ttlStop: make(chan struct{})}
	am.startTTLReaper()
	return am
}

// Stop signals the TTL reaper goroutine to exit. It should be called before
// discarding the AgentManager.
func (m *AgentManager) Stop() {
	close(m.ttlStop)
}

// startTTLReaper starts a background goroutine that periodically scans for
// expired agents and destroys them. The reaper exits when ttlStop is closed.
func (m *AgentManager) startTTLReaper() {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.reapExpiredAgents()
			case <-m.ttlStop:
				return
			}
		}
	}()
}

// reapExpiredAgents destroys all agents whose ExpiresAt is in the past.
func (m *AgentManager) reapExpiredAgents() {
	now := time.Now()
	for _, rec := range m.tracker.List() {
		if rec.ExpiresAt.IsZero() || rec.ExpiresAt.After(now) {
			continue
		}
		m.logger.Info("TTL expired, destroying agent", "agent_id", rec.AgentID, "expires_at", rec.ExpiresAt)
		if _, err := m.Destroy(context.Background(), rec.AgentID, false); err != nil {
			m.logger.Error("TTL reaper failed to destroy agent", "agent_id", rec.AgentID, "error", err)
		}
	}
}

// generateUUIDv4 creates a version-4 UUID using crypto/rand.
func generateUUIDv4() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand read: %w", err)
	}
	// Set version 4 and variant bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Spawn creates a new agent: validates/generates agent_id, creates a Linux user,
// generates an SSH keypair, sets up authorized_keys, and starts dockerd via systemd-run.
// On failure, previous steps are cleaned up.
