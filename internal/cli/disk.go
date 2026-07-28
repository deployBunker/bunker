package cli

import "fmt"

// diskUsagePercent returns the disk usage percentage as a float between 0 and 100.
func diskUsagePercent(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// formatDisk returns a human-readable disk usage string like "45% (892GB/2.0TB)".
func formatDisk(used, total uint64) string {
	pct := diskUsagePercent(used, total)
	return fmt.Sprintf("%.0f%% (%s/%s)", pct, humanBytes(used), humanBytes(total))
}

// diskWarning returns a warning indicator for disk usage percentage.
// Returns "⚠ " at ≥90%, "! " at ≥80%, empty string otherwise.
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
