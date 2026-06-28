package resource

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// AgentRecord holds the tracked state for a spawned agent.
type AgentRecord struct {
	AgentID        string
	Status         string // "running", "stopped", "failed"
	Limits         *v1.ResourceLimits
	CreatedAt      time.Time
	ExpiresAt      time.Time
	PortRangeStart uint32
	PortRangeEnd   uint32
	PublicURL      string
}

// Tracker manages agent state, capacity, and resource allocation.
type Tracker struct {
	mu        sync.RWMutex
	agents    map[string]*AgentRecord
	maxAgents uint32
	logger    *slog.Logger
}

// NewTracker creates a new Tracker with the given max capacity.
func NewTracker(maxAgents uint32, logger *slog.Logger) *Tracker {
	return &Tracker{
		agents:    make(map[string]*AgentRecord),
		maxAgents: maxAgents,
		logger:    logger,
	}
}

// Register adds a new agent record. Returns error if capacity is full or duplicate ID.
func (t *Tracker) Register(rec *AgentRecord) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if uint32(len(t.agents)) >= t.maxAgents {
		return fmt.Errorf("capacity full: %d/%d agents", len(t.agents), t.maxAgents)
	}
	if _, exists := t.agents[rec.AgentID]; exists {
		return fmt.Errorf("agent %q already registered", rec.AgentID)
	}
	t.agents[rec.AgentID] = rec
	t.logger.Info("agent registered", "agent_id", rec.AgentID, "total", len(t.agents))
	return nil
}

// Unregister removes an agent from tracking.
func (t *Tracker) Unregister(agentID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.agents, agentID)
	t.logger.Info("agent unregistered", "agent_id", agentID, "total", len(t.agents))
}

// UpdateStatus changes an agent's status.
func (t *Tracker) UpdateStatus(agentID, status string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rec, ok := t.agents[agentID]; ok {
		rec.Status = status
	}
}

// Get returns a single agent record, or nil if not found.
func (t *Tracker) Get(agentID string) *AgentRecord {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.agents[agentID]
}

// List returns all agent records.
func (t *Tracker) List() []*AgentRecord {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make([]*AgentRecord, 0, len(t.agents))
	for _, rec := range t.agents {
		result = append(result, rec)
	}
	return result
}

// Count returns the number of tracked agents.
func (t *Tracker) Count() uint32 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return uint32(len(t.agents))
}

// MaxAgents returns the configured max capacity.
func (t *Tracker) MaxAgents() uint32 {
	return t.maxAgents
}

// HasCapacity returns true if there is room for at least n more agents.
func (t *Tracker) HasCapacity(n uint32) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return uint32(len(t.agents))+n <= t.maxAgents
}

// ToAgentSummary converts an AgentRecord to the proto AgentSummary.
func (r *AgentRecord) ToAgentSummary() *v1.AgentSummary {
	return &v1.AgentSummary{
		AgentId:        r.AgentID,
		Status:         r.Status,
		Limits:         r.Limits,
		CreatedAt:      r.CreatedAt.Format(time.RFC3339),
		ExpiresAt:      r.ExpiresAt.Format(time.RFC3339),
		PublicUrl:      r.PublicURL,
		PortRangeStart: r.PortRangeStart,
		PortRangeEnd:   r.PortRangeEnd,
	}
}
