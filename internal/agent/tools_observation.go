package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	toolsObservationTimeout = 2 * time.Second
	toolsObservationSource  = "in-agent command -v via bunker info"
)

var observedTools = []string{"toolsd", "rg", "go"}

// ObserveTools runs the same command -v census an operator uses inside the
// agent, records the result on the live record and persists it. A failed probe
// leaves the previous observation untouched; callers must render that absence
// as unknown rather than falling back to the spawn-time image fact.
func (m *AgentManager) ObserveTools(ctx context.Context, agentID string) error {
	rec := m.tracker.Get(agentID)
	if rec == nil {
		return fmt.Errorf("agent %q not found", agentID)
	}
	probeCtx, cancel := context.WithTimeout(ctx, toolsObservationTimeout)
	defer cancel()

	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=1",
		"-i", rec.SshPrivateKeyPath,
		"bunker-" + agentID + "@localhost",
		"sh", "-c", "for t in toolsd rg go; do if command -v \"$t\" >/dev/null 2>&1; then printf \"%s\\tpresent\\n\" \"$t\"; else printf \"%s\\tabsent\\n\" \"$t\"; fi; done",
	}
	out, err := exec.CommandContext(probeCtx, "ssh", args...).Output()
	if err != nil {
		return fmt.Errorf("tools census: %w", err)
	}

	present := make(map[string]bool, len(observedTools))
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(fields) == 2 && fields[1] == "present" {
			present[fields[0]] = true
		}
	}
	count := uint32(0)
	for _, name := range observedTools {
		if present[name] {
			count++
		}
	}
	ready := count == uint32(len(observedTools))
	now := time.Now().UTC()
	rec.ObservedToolCount = &count
	rec.ToolsReady = &ready
	rec.ToolsObservedAt = now
	rec.ToolsObservationSource = toolsObservationSource
	if m.registry != nil {
		if err := m.registry.AppendToolsObservation(agentID, &count, &ready, now, toolsObservationSource); err != nil {
			return fmt.Errorf("persist tools census: %w", err)
		}
	}
	return nil
}

// ToolsObservation returns the latest live census without probing.
func (m *AgentManager) ToolsObservation(agentID string) (*uint32, *bool, time.Time, string) {
	rec := m.tracker.Get(agentID)
	if rec == nil {
		return nil, nil, time.Time{}, ""
	}
	return rec.ObservedToolCount, rec.ToolsReady, rec.ToolsObservedAt, rec.ToolsObservationSource
}
