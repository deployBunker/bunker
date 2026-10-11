package cli

import (
	"strings"
	"testing"
)

// TestFormatEgressModes (DF-BUNKER-82) pins the status renderer's honesty
// contract: an empty egress_modes_supported field is a pre-capability
// daemon and must say so (never render as "open"), while a reported
// vocabulary renders verbatim.
func TestFormatEgressModes(t *testing.T) {
	cases := []struct {
		name  string
		modes []string
		want  []string // substrings the line must carry
		not   []string // substrings it must NOT carry
	}{
		{
			name:  "pre_capability_daemon_says_not_reported",
			modes: nil,
			want:  []string{"not reported", "predates egress", "REFUSED"},
			not:   []string{"open"},
		},
		{
			name:  "reported_vocabulary_renders_verbatim",
			modes: []string{"open", "allowlist", "none"},
			want:  []string{"open, allowlist, none"},
			not:   []string{"not reported"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatEgressModes(tc.modes)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("render %q missing %q", got, w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("render %q must not contain %q", got, n)
				}
			}
		})
	}
}
