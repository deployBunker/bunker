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
