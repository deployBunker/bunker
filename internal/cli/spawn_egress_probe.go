// Egress capability probe (DF-BUNKER-82): refuse an enforced --egress-mode
// against a daemon that predates the egress surface, instead of letting the
// old daemon silently drop the field and run the agent unfiltered.
//
// The wire contract is the ServerInfoResponse.egress_modes_supported field
// (proto field 13), which follows the additive capability convention of
// tmp_isolation (field 8) and default_network_mode (field 11): an EMPTY
// value means the daemon predates the capability — absence is not an
// affirmative statement, so it can never be read as "every mode is
// supported". A daemon that reports the field but omits the requested mode
// refuses too: the operator asked for a boundary this build does not
// implement.
package cli

import (
	"fmt"
	"strings"

	"github.com/deployBunker/bunker/internal/config"
)

// daemonEgressModes fetches ServerInfo and returns the daemon's reported
// egress vocabulary. An empty slice is meaningful (pre-capability daemon),
// not an error: the caller decides what absence means. Transport and
// permission failures ARE errors — a daemon we cannot interrogate must not
// be assumed either capable or incapable.
func daemonEgressModes(entry ServerEntry) ([]string, error) {
	info, err := ConnectServerWithEntry(entry)
	if err != nil {
		return nil, fmt.Errorf("egress capability probe: %w", err)
	}
	return info.GetEgressModesSupported(), nil
}

// enforceEgressRequested reports whether the spawn asks for an ENFORCED
// egress mode. "open" is explicit but changes nothing (no firewall command
// is ever invoked), so a daemon without an egress surface behaves exactly
// like a current one for that mode; only allowlist/none need the capability
// check. The empty value defers to the daemon's resolution chain, which is
// itself the capability under test — probed separately below.
func enforceEgressRequested(mode string) bool {
	return mode != "" && mode != config.EgressModeOpen
}

// ensureDaemonSupportsEgressMode is the spawn-path gate (DF-BUNKER-82). It
// refuses — client-side, BEFORE any SpawnAgent RPC — when:
//
//   - the daemon's ServerInfo carries an EMPTY egress_modes_supported field
//     and the spawn requests an enforced mode: the daemon build predates
//     egress enforcement (pre-fd974b51), the flag would be silently
//     ignored, and the agent would run fully unfiltered;
//   - the daemon reports its vocabulary but the requested enforced mode is
//     not in it.
//
// It returns nil without any RPC when the spawn requests no mode (the
// daemon resolves open) or the inert "open" mode — pre-capability daemons
// keep working byte-identically for every pre-existing invocation shape.
// An unresolvable ServerInfo (transport failure, auth rejection) fails the
// spawn loudly: a daemon we cannot interrogate must not be assumed either
// capable or incapable.
func ensureDaemonSupportsEgressMode(mode string, entry ServerEntry) error {
	if !enforceEgressRequested(mode) {
		return nil
	}
	modes, err := daemonEgressModes(entry)
	if err != nil {
		// The daemon could not be interrogated (down, auth rejected,
		// transport broken). Refuse closed — the capability is unknown, not
		// absent — and carry the flag plus the recovery path in the error.
		return fmt.Errorf(
			"refusing to spawn with --egress-mode %q: egress capability probe failed (%v). "+
				"The daemon's egress support could not be verified; upgrade the daemon or drop the flag",
			mode,
			err,
		)
	}
	if len(modes) == 0 {
		return fmt.Errorf(
			"refusing to spawn with --egress-mode %q: the daemon at this server predates egress enforcement "+
				"(ServerInfo carries no egress capability) and would silently ignore the flag — the agent would run "+
				"fully unfiltered. Upgrade the daemon (bunkerd) to a build with egress support (commit fd974b51, "+
				"2026-10-07) or drop the flag; supported modes are reported by `bunker info`",
			mode,
		)
	}
	for _, m := range modes {
		if m == mode {
			return nil
		}
	}
	return fmt.Errorf(
		"refusing to spawn with --egress-mode %q: the daemon does not report support for it (supported: %s). "+
			"Choose a supported mode or upgrade the daemon",
		mode,
		strings.Join(modes, ", "),
	)
}
