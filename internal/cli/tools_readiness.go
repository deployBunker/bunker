package cli

import (
	"fmt"

	"github.com/deployBunker/bunker/internal/imagespec"
)

// formatSpawnTools renders the tools-readiness datum `bunker info` reports
// (DOGFOOD-017): how many of the delivered set's registry tools the agent's
// spawn image carried, as recorded by the daemon at spawn.
//
// Every branch states a recorded fact or an explicit unknown. A plain spawn's
// recorded 0 is rendered as 0 — with the repair command — rather than dressed
// up as "ready", because the disputed board claim was exactly that a plain
// spawn ships these tools. A nil count means the record predates the field: it
// renders "unknown" plus the command that answers the question for real, never
// a fabricated 0 or 2. toolsd is never part of the delivered-set count — it is
// delivered (or not) by the CLI after the spawn, so it is named in every branch
// instead of being folded into the number.
func formatSpawnTools(agentID string, count *uint32) string {
	total := imagespec.DeliveredSetSize()
	if count == nil {
		return fmt.Sprintf("unknown (this record predates spawn tool reporting; probe with 'bunker agent-tools %s')", agentID)
	}
	got := int(*count)
	switch {
	case got >= total:
		return fmt.Sprintf("%d of %d delivered-set registry tools in the spawn image (rg, gopls); toolsd is delivered only by '--tools-at-spawn' or 'bunker agent-tools %s --install'",
			got, total, agentID)
	case got == 0:
		return fmt.Sprintf("0 of %d delivered-set registry tools in the spawn image — a plain spawn builds no image, so rg and gopls are absent and toolsd is never delivered (add them with 'bunker agent-tools %s --install')",
			total, agentID)
	default:
		return fmt.Sprintf("%d of %d delivered-set registry tools in the spawn image — the spec did not ask for the full delivered set; toolsd is never part of an image (see 'bunker agent-tools %s')",
			got, total, agentID)
	}
}
