package cli

import (
	"fmt"
	"time"

	"github.com/deployBunker/bunker/internal/imagespec"
)

func formatLiveTools(ready *bool, count *uint32, observedAt, source string) string {
	if ready == nil || count == nil {
		return "unknown (no completed in-agent command -v census; run 'bunker agent-tools <id>')"
	}
	state := "not ready"
	if *ready {
		state = "ready"
	}
	when := observedAt
	if parsed, err := time.Parse(time.RFC3339, observedAt); err == nil {
		when = parsed.Local().Format(time.RFC3339)
	}
	if source == "" {
		source = "in-agent command -v census"
	}
	return fmt.Sprintf("%s (%d of %d; observed %s via %s)", state, *count, len(liveToolNames), when, source)
}

var liveToolNames = []string{"toolsd", "rg", "go"}

// formatSpawnTools renders the spawn-time provenance. It must not be confused
// with the live census rendered by formatLiveTools.
func formatSpawnTools(agentID string, count *uint32) string {
	total := imagespec.DeliveredSetSize()
	if count == nil {
		return fmt.Sprintf("unknown (this record predates spawn tool reporting; probe with 'bunker agent-tools %s')", agentID)
	}
	got := int(*count)
	switch {
	case got >= total:
		return fmt.Sprintf("%d of %d delivered-set registry tools in the spawn image (rg, gopls); toolsd is delivered only by '--tools-at-spawn' or 'bunker agent-tools %s --install'", got, total, agentID)
	case got == 0:
		return fmt.Sprintf("0 of %d delivered-set registry tools in the spawn image — a plain spawn builds no image, so rg and gopls are absent and toolsd is never delivered (add them with 'bunker agent-tools %s --install')", total, agentID)
	default:
		return fmt.Sprintf("%d of %d delivered-set registry tools in the spawn image — the spec did not ask for the full delivered set; toolsd is never part of an image (see 'bunker agent-tools %s')", got, total, agentID)
	}
}
