// Network-isolation reporting renderers for the CLI (NET-BUNKER-010;
// specs/network-isolation.md §5.2 — the reporting law).
//
// A BOUND THAT IS NOT REPORTED IS NOT A BOUND. Three meanings stay DISTINCT
// in every renderer below, or the report re-introduces the silence it exists
// to remove:
//
//   - EMPTY mode: the record/daemon PREDATES the field — it must never
//     render as safe (an older daemon says nothing about the boundary);
//   - "unknown": the daemon could not verify — it must not claim "shared";
//   - an explicit value ("shared"/"systemd") is the only affirmative
//     statement, paired with the boundary the mode ACTUALLY provides.
package cli

import (
	"fmt"

	"github.com/deployBunker/bunker/internal/netmode"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// formatNetworkIsolation renders the per-agent network-isolation state for
// `bunker info`. One line; the boundary rides the same line so a screenshot
// or a paste of info output carries the whole contract.
func formatNetworkIsolation(ni *v1.NetworkIsolation) string {
	if ni == nil || ni.GetMode() == "" {
		// Pre-surface record (older daemon): absence is not an affirmative
		// statement — say what is missing instead of rendering a default.
		return "not reported by this daemon — it predates network-isolation reporting; the network boundary is NOT verified"
	}
	if ni.GetMode() == "unknown" {
		// The daemon could not verify: never upgrade this to "shared".
		return "unknown — the daemon could not verify the enforced boundary"
	}
	boundary := ni.GetBoundary()
	if boundary == "" {
		// A mode with no boundary line would fail the reporting law; render
		// the mode but flag the gap instead of inventing comfort.
		return fmt.Sprintf("%s — boundary not reported", ni.GetMode())
	}
	line := fmt.Sprintf("%s — %s", ni.GetMode(), boundary)
	// INT-CI-020: a mode that ENFORCES something is only "verified" when the
	// daemon read the enforced unit property back from the RUNNING unit at
	// spawn (proto NetworkIsolation.verified). Reporting the mode alone is the
	// false cut this row exists for: accepted-as-a-flag is not applied. The
	// unverified arm is stated as unproven, never softened into a pass.
	if netmodeEnforcesBoundary(ni.GetMode()) {
		if ni.GetVerified() {
			line += " [verified: the daemon read the enforced unit property back from the running unit at spawn]"
		} else {
			line += " [NOT verified: the daemon never read the boundary back from the running unit — treat this boundary as unproven]"
		}
	}
	return line
}

// netmodeEnforcesBoundary reports whether a mode claims an enforced bound that
// must be verified (systemd's private network namespace, procvis's private
// /proc view). "shared" enforces nothing, so it has no verification claim to
// make or withhold.
func netmodeEnforcesBoundary(mode string) bool {
	switch mode {
	case netmode.ModeSystemd, netmode.ModeProcVis:
		return true
	default:
		return false
	}
}

// formatDefaultNetworkMode renders the daemon-wide default mode line for
// `bunker status` (same three-state vocabulary as formatNetworkIsolation).
func formatDefaultNetworkMode(mode, boundary string) string {
	switch mode {
	case "":
		return "  Net mode: not reported by this daemon — it predates network-isolation reporting\n"
	case "unknown":
		return "  Net mode: unknown — the daemon's default network-isolation mode could not be resolved (check agent.network_mode)\n"
	default:
		if boundary == "" {
			return fmt.Sprintf("  Net mode: %s — boundary not reported\n", mode)
		}
		return fmt.Sprintf("  Net mode: %s — %s\n", mode, boundary)
	}
}
