// Egress reporting renderers for the CLI (DF-BUNKER-82). Same honesty
// contract as netmode_render.go: a capability that is not REPORTED is not a
// capability — an empty egress_modes_supported field means the daemon
// predates the egress surface (pre-fd974b51 builds), and the status line
// must say so instead of implying "open".
package cli

import (
	"fmt"
	"strings"
)

// formatEgressModes renders the daemon's reported egress vocabulary for
// `bunker status` (the same surface tmp_isolation / default_network_mode
// ride). One line:
//
//	vocabulary reported -> "  Egress:   open, allowlist, none"
//	empty field         -> "  Egress:   not reported by this daemon — it
//	                       predates egress reporting; enforced --egress-mode
//	                       spawns are REFUSED by this CLI"
func formatEgressModes(modes []string) string {
	if len(modes) == 0 {
		return "  Egress:   not reported by this daemon — it predates egress reporting; --egress-mode allowlist/none spawns are REFUSED by this CLI\n"
	}
	return fmt.Sprintf("  Egress:   %s\n", strings.Join(modes, ", "))
}
