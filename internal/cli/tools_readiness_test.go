package cli

import (
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/imagespec"
)

// TestFormatSpawnTools pins the THREE states of the tools-readiness datum
// (DOGFOOD-017): a recorded plain-spawn zero, a recorded delivered-set count,
// and an absent datum for a record that predates the field. The law the row
// carries is that absence must never render as a number and a plain spawn must
// never render as ready, so each state asserts its own claim AND the absence of
// the other states' wording.
func TestFormatSpawnTools(t *testing.T) {
	total := imagespec.DeliveredSetSize()
	if total == 0 {
		t.Fatal("DeliveredSetSize() = 0; the delivered set is empty, so every count renders as ready")
	}
	zero, full, partial := uint32(0), uint32(total), uint32(1)

	tests := []struct {
		name     string
		count    *uint32
		contains []string
		excludes []string
	}{
		{
			name:  "absent count renders unknown, never a number",
			count: nil,
			contains: []string{
				"unknown",
				"predates spawn tool reporting",
				"bunker agent-tools dfabc123",
			},
			excludes: []string{"0 of", "delivered-set registry tools in the spawn image"},
		},
		{
			name:  "recorded plain-spawn zero says rg and gopls are absent",
			count: &zero,
			contains: []string{
				"0 of ",
				"a plain spawn builds no image",
				"rg and gopls are absent",
				"bunker agent-tools dfabc123 --install",
			},
			excludes: []string{"unknown"},
		},
		{
			name:  "recorded full delivered set names what the image carries",
			count: &full,
			contains: []string{
				"(rg, gopls)",
				"toolsd is delivered only by '--tools-at-spawn'",
			},
			excludes: []string{"unknown", "absent"},
		},
		{
			name:  "recorded partial set does not claim the full set",
			count: &partial,
			contains: []string{
				"did not ask for the full delivered set",
				"toolsd is never part of an image",
			},
			excludes: []string{"unknown", "(rg, gopls)"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatSpawnTools("dfabc123", tc.count)
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("formatSpawnTools(%v) = %q, missing %q", tc.count, got, want)
				}
			}
			for _, banned := range tc.excludes {
				if strings.Contains(got, banned) {
					t.Errorf("formatSpawnTools(%v) = %q, must not contain %q", tc.count, got, banned)
				}
			}
		})
	}
}

// TestFormatSpawnToolsNamesTheDenominator proves the rendered denominator is
// derived from the delivered set rather than hard-coded: if the set grows, the
// line follows instead of claiming every new tool is present.
func TestFormatSpawnToolsNamesTheDenominator(t *testing.T) {
	zero := uint32(0)
	got := formatSpawnTools("abc", &zero)
	want := "0 of " + itoa(imagespec.DeliveredSetSize())
	if !strings.Contains(got, want) {
		t.Errorf("formatSpawnTools(0) = %q, want it to contain %q", got, want)
	}
}

// itoa avoids pulling strconv into a test that renders a small integer.
func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
