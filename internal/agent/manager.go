// Package agent manages agent lifecycle: create users, generate SSH keys, start dockerd.
package agent

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/imagespec"
	"github.com/deployBunker/bunker/internal/registry"
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
	// imageBuilder is the GAP-064 rootless image-spec builder/cache. Nil in
	// tests that construct AgentManager directly; nil disables customization.
	imageBuilder *imagespec.Builder

	// registry is the GAP-070 durable lifecycle store (append-only JSONL,
	// replayed at startup). Nil when the feature is disabled or the store
	// could not be opened — registryErr then explains why, and the daemon
	// refuses to serve in that case (a daemon that cannot persist agent
	// state must not report durable spawn success).
	registry    *registry.Store
	registryErr error

	// Seams (production values set in NewAgentManager; tests inject fakes).
	// listSystemAgents enumerates managed agents present on the host.
	listSystemAgents func() ([]SystemAgent, error)
	// destroyAgent is the reconciliation destroy path (defaults to Destroy).
	destroyAgent func(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error)

	// reconcileDone is closed once Reconcile has run (or been given up on).
	// The TTL reaper waits for it so it can never destroy an agent before
	// the registry has been replayed and reconciled against system state.
	reconcileDone chan struct{}
	reconcileOnce sync.Once
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
	am := &AgentManager{
		cfg: cfg, logger: logger, tracker: tracker, portAlloc: pa,
		tunnelMgr: tunnelMgr, tailscaleMgr: tailscaleMgr,
		ttlStop:       make(chan struct{}),
		reconcileDone: make(chan struct{}),
	}
	am.listSystemAgents = defaultListSystemAgents
	am.destroyAgent = am.Destroy
	am.imageBuilder = imagespec.NewBuilder(nil, &imagespec.CacheOptions{
		Dir:          cfg.Agent.ImageSpec.CacheDir,
		BuildTimeout: cfg.Agent.ImageSpec.BuildTimeout,
		Disabled:     !cfg.Agent.ImageSpec.Enabled,
	})
	// GAP-070: replay the durable registry BEFORE the TTL reaper starts, so
	// no agent can be reaped out of a half-restored registry.
	am.openRegistry()
	am.startTTLReaper()
	return am
}

// openRegistry opens and replays the durable agent registry when enabled.
// A failure is recorded in registryErr rather than panicking: callers that
// serve traffic must check RegistryError() (server.Run does) while tests and
// the CLI can keep working with an in-memory-only manager.
func (m *AgentManager) openRegistry() {
	if !m.cfg.Agent.Registry.Enabled {
		m.logger.Info("agent registry disabled — agent state will not survive a restart")
		return
	}
	m.cfg.Agent.Registry.Defaults()
	s, err := registry.Open(registry.Options{
		Path:       m.cfg.Agent.Registry.Path,
		MaxBytes:   m.cfg.Agent.Registry.MaxBytes,
		MaxBackups: m.cfg.Agent.Registry.MaxBackups,
		KnownIDCap: m.cfg.Agent.Registry.KnownIDCap,
		Logger:     m.logger,
	})
	if err != nil {
		m.registryErr = err
		m.logger.Error("agent registry unavailable", "path", m.cfg.Agent.Registry.Path, "error", err)
		return
	}
	m.registry = s
	rep := s.Report()
	m.logger.Info("agent registry replayed",
		"path", s.Path(),
		"files", rep.Files,
		"events", rep.Events,
		"live", rep.Live,
		"known", rep.Known,
		"malformed", rep.Malformed,
		"partial_tail", rep.PartialTail,
	)
}

// RegistryError reports why the durable registry is unavailable, or nil when
// persistence is either active or explicitly disabled by configuration.
func (m *AgentManager) RegistryError() error { return m.registryErr }

// Stop signals the TTL reaper goroutine to exit. It should be called before
// discarding the AgentManager.
func (m *AgentManager) Stop() {
	close(m.ttlStop)
}

// Heartbeat extends an agent's TTL (never shrinking it) and persists the
// extension in the durable registry. Callers (the server's heartbeat RPCs)
// route through here instead of mutating the tracker directly so the durable
// log sees every lifecycle change.
func (m *AgentManager) Heartbeat(agentID string, ttl time.Duration) (*resource.AgentRecord, error) {
	rec := m.tracker.Get(agentID)
	if rec == nil {
		return nil, fmt.Errorf("agent %q not found", agentID)
	}
	if candidate := time.Now().Add(ttl); candidate.After(rec.ExpiresAt) {
		rec.ExpiresAt = candidate
	}
	// A heartbeat is best-effort durable state: failing to append it must
	// never fail the RPC (the in-memory TTL extension already happened and
	// the next heartbeat will retry the write).
	if err := m.persistHeartbeat(agentID, rec.ExpiresAt, rec.Status); err != nil {
		m.logger.Warn("registry heartbeat append failed", "agent_id", agentID, "error", err)
	}
	return rec, nil
}

// persistSpawn durably records a newly spawned agent. When the registry is
// enabled but the write fails, the caller MUST roll the spawn back — a spawn
// that is not persisted would be forgotten by the next replay.
func (m *AgentManager) persistSpawn(rec *resource.AgentRecord) error {
	if m.registry == nil {
		return nil
	}
	if err := m.registry.AppendSpawn(recordToRegistry(rec)); err != nil {
		return fmt.Errorf("persist agent registry spawn: %w", err)
	}
	return nil
}

// persistDestroy durably records that an agent is gone. Failures are
// surfaced to the caller, which keeps the cleanup result honest: resources
// are released either way, but the error is reported in the log.
func (m *AgentManager) persistDestroy(agentID string) error {
	if m.registry == nil {
		return nil
	}
	if err := m.registry.AppendDestroy(agentID); err != nil {
		return fmt.Errorf("persist agent registry destroy: %w", err)
	}
	return nil
}

func (m *AgentManager) persistHeartbeat(agentID string, expiresAt time.Time, status string) error {
	if m.registry == nil {
		return nil
	}
	return m.registry.AppendHeartbeat(agentID, expiresAt, status)
}

// knownAgent reports whether the durable lifecycle store remembers agentID —
// either as a live agent or as one that was destroyed while the store was
// watching. It is what distinguishes an idempotent repeated destroy (known)
// from a never-seen ID (not_found). Without a registry the manager keeps the
// pre-GAP-070 behavior exactly.
func (m *AgentManager) knownAgent(agentID string) bool {
	return m.registry != nil && m.registry.Known(agentID)
}

// startTTLReaper starts a background goroutine that periodically scans for
// expired agents and destroys them. The reaper exits when ttlStop is closed.
// It does not tick until Reconcile has completed (bounded by grace) so a
// restart can never destroy an agent the registry has not yet restored.
func (m *AgentManager) startTTLReaper() {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		const reconcileGrace = 30 * time.Second
		select {
		case <-m.reconcileDone:
		case <-time.After(reconcileGrace):
			m.logger.Warn("TTL reaper starting without completed reconciliation", "grace", reconcileGrace.String())
		case <-m.ttlStop:
			return
		}
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
