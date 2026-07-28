package cli

import (
	"strings"
	"testing"
)

func TestDiskUsagePercent(t *testing.T) {
	tests := []struct {
		name  string
		used  uint64
		total uint64
		want  float64
	}{
		{"zero total", 0, 0, 0},
		{"half", 500, 1000, 50},
		{"full", 1000, 1000, 100},
		{"empty", 0, 1000, 0},
		{"45 percent", 45, 100, 45},
		{"realistic 892GB of 2TB", 892 * 1024 * 1024 * 1024, 2 * 1024 * 1024 * 1024 * 1024, 43.5546875},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := diskUsagePercent(tt.used, tt.total)
			if got != tt.want {
				t.Errorf("diskUsagePercent(%d, %d) = %v, want %v", tt.used, tt.total, got, tt.want)
			}
		})
	}
}

func TestFormatDisk(t *testing.T) {
	tests := []struct {
		name  string
		used  uint64
		total uint64
		want  string
	}{
		{
			name:  "zero",
			used:  0,
			total: 0,
			want:  "0% (0 B/0 B)",
		},
		{
			name:  "45 percent gb",
			used:  45 * 1024 * 1024 * 1024,
			total: 100 * 1024 * 1024 * 1024,
			want:  "45% (45.0 GB/100.0 GB)",
		},
		{
			name:  "full disk",
			used:  1000,
			total: 1000,
			want:  "100% (1000 B/1000 B)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDisk(tt.used, tt.total)
			if got != tt.want {
				t.Errorf("formatDisk(%d, %d) = %q, want %q", tt.used, tt.total, got, tt.want)
			}
		})
	}
}

func TestFormatDiskContainsPercent(t *testing.T) {
	// Sanity: all formatDisk output should contain a '%' and parens.
	result := formatDisk(80*1024*1024*1024, 200*1024*1024*1024)
	if !strings.Contains(result, "%") {
		t.Errorf("formatDisk output missing '%%', got: %s", result)
	}
	if !strings.Contains(result, "(") || !strings.Contains(result, ")") {
		t.Errorf("formatDisk output missing parens, got: %s", result)
	}
}
