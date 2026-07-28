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

func TestDiskWarning(t *testing.T) {
	tests := []struct {
		name string
		pct  float64
		want string
	}{
		{"normal", 45.0, ""},
		{"warn threshold edge low", 79.99, ""},
		{"warn threshold edge high", 80.0, "! "},
		{"warn", 85.0, "! "},
		{"critical threshold edge low", 89.99, "! "},
		{"critical threshold edge high", 90.0, "⚠ "},
		{"critical", 95.0, "⚠ "},
		{"full", 100.0, "⚠ "},
		{"zero", 0.0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := diskWarning(tt.pct)
			if got != tt.want {
				t.Errorf("diskWarning(%.2f) = %q, want %q", tt.pct, got, tt.want)
			}
		})
	}
}

func TestDiskAlert(t *testing.T) {
	tests := []struct {
		name string
		pct  float64
		want bool
	}{
		{"normal", 45.0, false},
		{"warn threshold", 80.0, false},
		{"below alert at 89%", 89.0, false},
		{"exactly 90% - no alert (alert is >90)", 90.0, false},
		{"just over 90%", 90.01, true},
		{"91 percent", 91.0, true},
		{"critical", 95.0, true},
		{"full", 100.0, true},
		{"zero", 0.0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := diskAlert(tt.pct)
			if got != tt.want {
				t.Errorf("diskAlert(%.2f) = %v, want %v", tt.pct, got, tt.want)
			}
		})
	}
}
