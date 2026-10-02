// Network-isolation mode reporting plumbing (NET-BUNKER-010;
// specs/network-isolation.md §5.2 is the reporting law).
//
// A BOUND THAT IS NOT REPORTED IS NOT A BOUND. The resolved mode and the
// boundary it ACTUALLY provides ride the tracker record, the durable registry
// record, and the wire AgentSummary so `bunker status`/`list`/`info`/
// GetAgent can show them. Three meanings stay distinct everywhere:
//
//   - explicit mode ("shared"/"systemd") — the only affirmative statement,
//     paired with netmode.BoundaryFor's honest boundary string;
//   - netmode.ModeUnknown — the daemon could not verify; never claim shared;
//   - EMPTY — the record predates the field (older daemon); never rendered
//     as safe, and the boundary/marker say "not reported" instead.
package agent

import (
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/netmode"
)

// networkIsolationForSummary builds the §5.2 wire payload for an ENFORCED
// mode: the mode name plus the boundary string for exactly that mode. An
// empty mode (a pre-surface caller) yields nil — absence stays absence, it
// is never upgraded to a "shared" claim.
func networkIsolationForSummary(mode string) *v1.NetworkIsolation {
	if mode == "" {
		return nil
	}
	return &v1.NetworkIsolation{
		Mode:     mode,
		Boundary: netmode.BoundaryFor(mode),
	}
}

// networkBoundaryOf extracts the boundary string from a wire payload
// (nil-safe) for the durable registry record.
func networkBoundaryOf(ni *v1.NetworkIsolation) string {
	if ni == nil {
		return ""
	}
	return ni.GetBoundary()
}

// networkIsolationForRecord rebuilds the wire payload from a durable
// registry record. Empty mode = pre-surface record → nil (absence stays
// absence). A non-empty mode always reports with its boundary; when an
// older durable record somehow carries a mode without a boundary, the
// boundary is derived from the mode rather than left blank — a mode with no
// boundary line would fail the §5.2 law this plumbing exists for.
func networkIsolationForRecord(mode, boundary string) *v1.NetworkIsolation {
	if mode == "" {
		return nil
	}
	if boundary == "" {
		boundary = netmode.BoundaryFor(mode)
	}
	return &v1.NetworkIsolation{Mode: mode, Boundary: boundary}
}

// InBandNetworkIsolationMarker is the in-band system-info marker line
// (§5.2), built on the GAP-067 containment-disclosure idiom: one fixed,
// greppable, bracketed line an agent session discovers through normal
// reconnaissance. See netmode.ContainmentMarker for the exact rendering
// (empty/unknown modes render the explicit unknown marker, never shared).
func InBandNetworkIsolationMarker(mode string) string {
	return netmode.ContainmentMarker(mode)
}
