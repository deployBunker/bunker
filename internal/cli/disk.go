package cli

import "fmt"

// ── DF-BUNKER-54: the agent "disk limit" is a PER-FILE cap ───────────────────
//
// agent.default_disk_bytes reaches the host as systemd `LimitFSIZE`, i.e.
// RLIMIT_FSIZE: the maximum size of a SINGLE FILE the agent may create. Nothing
// in bunker caps an agent's TOTAL on-disk usage — real per-user filesystem
// quotas are GAP-161 and are NOT implemented — so comparing real usage against
// that number (the old `218% (43.6 GB/20.0 GB)` cell) asserted an enforcement
// that does not exist.
//
// The two flavours of "disk" in this package are therefore kept apart, and the
// distinction is what the labels have to preserve:
//
//   - HOST filesystem surface (`bunker status`, `bunker metrics` server block):
//     used/total of a real, enforced filesystem → a percentage is meaningful.
//     Rendered by formatDisk/diskUsagePercent/diskAlert/diskWarning.
//   - AGENT cap surface (`bunker list`, `bunker info`, `bunker metrics <id>`):
//     usage is a fact, the cap is a PER-FILE cap → two separate labelled
//     facts, never a ratio. Rendered by formatPerFileCap and the *Header /
//     *Label constants below.
//
// Pinned by disk_semantic_test.go (and the agent-side knob pin in
// internal/agent/disk_cap_semantic_test.go): re-labelling the agent number as
// a total-disk limit, or bringing back a used-vs-cap percentage, fails there.

// Header and label vocabulary for the agent cap surface. One source, so a
// surface cannot drift into claiming a quota (DF-BUNKER-54).
const (
	// diskUsedHeader labels the agent disk USAGE column (a measured fact).
	diskUsedHeader = "Disk Used"
	// perFileCapHeader labels the agent per-file cap column. The header names
	// the semantic: the cap applies to ONE file, not to the agent's disk.
	perFileCapHeader = "Max File Size"
	// maxFileSizeLabel is the detail-view line label for the same number
	// (bunker info / bunker metrics <id>).
	maxFileSizeLabel = "Max File Size"
	// perFileCapQualifier states the semantic where the surface has room for
	// a sentence. Every place that prints the cap next to agent usage carries
	// it, so an operator reading any single surface cannot mistake the number
	// for a total-disk quota.
	perFileCapQualifier = "(per-file cap \u2014 not a total-disk quota)"
	// noCapLabel is what the cap column shows when none is configured. This is
	// the DOCUMENTED-GOOD configuration (internal/agent/SKILL.md: a finite
	// RLIMIT_FSIZE crash-loops .NET apps), so it renders as an explicit
	// absence rather than a 0-byte cap.
	noCapLabel = "\u2014"
)

// diskUsagePercent returns the disk usage percentage as a float between 0 and 100.
//
// HOST filesystem surface only: total must be a real, enforced filesystem size
// (ServerMetrics.DiskTotalBytes). Never call this with an agent's per-file cap
// as `total` — see the file header (DF-BUNKER-54).
func diskUsagePercent(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// formatDisk returns a human-readable disk usage string like "45% (892GB/2.0TB)".
//
// HOST filesystem surface only (bunker status): `total` is the filesystem size,
// which the kernel does enforce, so the ratio is truthful here. The AGENT cap
// surface must NOT route through this function — a per-file cap is not a
// denominator anything measures usage against (DF-BUNKER-54).
func formatDisk(used, total uint64) string {
	pct := diskUsagePercent(used, total)
	return fmt.Sprintf("%.0f%% (%s/%s)", pct, humanBytes(used), humanBytes(total))
}

// formatPerFileCap renders an agent's per-file size cap for the AGENT cap
// surface: the byte figure alone, with no percentage and no used-vs-cap ratio
// (the cap bounds one file, so a ratio against total usage would be a false
// claim — DF-BUNKER-54). A zero cap means no cap is emitted at all, which is
// the documented-good configuration, so it renders as an explicit absence.
func formatPerFileCap(capBytes uint64) string {
	if capBytes == 0 {
		return noCapLabel
	}
	return humanBytes(capBytes)
}

// diskAlert returns true when disk usage exceeds 90%, indicating a critical
// storage condition that requires attention (spawn warnings, status banners, etc.).
//
// HOST filesystem surface only (see diskUsagePercent).
func diskAlert(pct float64) bool {
	return pct > 90
}

// diskWarning returns a warning indicator for disk usage percentage.
// Returns "⚠ " at ≥90%, "! " at ≥80%, empty string otherwise.
//
// HOST filesystem surface only (see diskUsagePercent).
func diskWarning(pct float64) string {
	switch {
	case pct >= 90:
		return "⚠ "
	case pct >= 80:
		return "! "
	default:
		return ""
	}
}
