// Egress policy config (GAP-134, REQ-E1 / SEC-06): the daemon-wide DEFAULT
// egress mode and the admin-managed allowlist, plus the resolver every
// spawn-shaped path goes through. The vocabulary is OWNED by internal/egress
// (the netmode ownership rule); this file re-exports it for validation and
// error text and never invents its own list.
package config

import (
	"fmt"
	"strings"

	"github.com/deployBunker/bunker/internal/egress"
)

// EgressMode* re-export the internal/egress vocabulary (the netmode const
// re-export convention at the top of this file).
const (
	EgressModeOpen      = egress.ModeOpen
	EgressModeAllowlist = egress.ModeAllowlist
	EgressModeNone      = egress.ModeNone

	// EgressModeDefault is the declared SAFE default (REQ-E1): open — zero
	// behavior change, no firewall interaction. A config that does not
	// mention egress gets exactly the pre-GAP-134 daemon.
	EgressModeDefault = egress.DefaultMode
)

// ValidEgressModes lists the accepted egress mode names — the internal/egress
// vocabulary, verbatim.
func ValidEgressModes() []string {
	return egress.ValidModes()
}

// ValidEgressMode reports whether name is an implemented egress mode.
func ValidEgressMode(name string) bool {
	return egress.ValidMode(name)
}

// ValidEgressAllowlistEntry validates one allowlist entry (CIDR, bare IP, or
// plausible hostname). Exported for the config Validate path and the CLI.
func ValidEgressAllowlistEntry(entry string) error {
	return egress.ValidAllowlistEntry(entry)
}

// EgressConfig holds the daemon-wide egress policy defaults (GAP-134).
// Admin-controlled and hidden-by-default in the GAP-067 sense: the zero
// value changes nothing.
type EgressConfig struct {
	// Mode is the daemon-wide DEFAULT egress mode new spawns resolve to
	// when the spawn request does not name one: "open" (the default —
	// unrestricted outbound, no firewall interaction), "allowlist"
	// (per-agent default-deny chain; only loopback, established/related
	// return traffic and the entries below are accepted), or "none"
	// (deny-all except the control channel). Empty = unset = open.
	// Validate rejects any other value — a typoed mode never silently
	// resolves to a different (weaker or stronger) boundary.
	Mode string `mapstructure:"mode"`
	// Allowlist is the admin-managed accept list for allowlist mode: CIDRs,
	// bare IPs, or hostnames (resolved to IPs at rule-install time; a
	// rotating DNS answer goes stale until the next reinstall —
	// docs/egress-policy.md). DNS itself must be allowed explicitly (add
	// your resolver's addresses). Entries are validated at config load;
	// an empty allowlist with mode=allowlist fails the SPAWN at install
	// time (fail loud — never an accept-nothing chain reported as
	// enforced), because Install refuses to claim enforcement it cannot
	// state.
	Allowlist []string `mapstructure:"allowlist"`
}

// Validate checks the configured mode against the vocabulary and every
// allowlist entry against the entry grammar. Empty is the unset default and
// always passes.
func (e EgressConfig) Validate() error {
	mode := strings.TrimSpace(e.Mode)
	if mode == "" {
		mode = EgressModeDefault
	}
	if !ValidEgressMode(mode) {
		return fmt.Errorf("agent.egress.mode must be one of %v, got %q", ValidEgressModes(), e.Mode)
	}
	for i, entry := range e.Allowlist {
		if err := ValidEgressAllowlistEntry(entry); err != nil {
			return fmt.Errorf("agent.egress.allowlist[%d]: %w", i, err)
		}
	}
	return nil
}

// ModeOrDefault returns the configured mode, or the declared default when
// unset. Validation guarantees a non-empty value is in the vocabulary.
func (e EgressConfig) ModeOrDefault() string {
	if m := strings.TrimSpace(e.Mode); m != "" {
		return m
	}
	return EgressModeDefault
}

// ResolveEgressMode is the SINGLE precedence resolver for the egress mode
// (GAP-134): per-spawn request > the config global (agent.egress.mode) >
// the declared default (open). Every spawn-shaped code path resolves through
// this function so the sources can never disagree. The allowlist ALWAYS
// comes from the daemon config (the admin's managed list — a spawn request
// does not carry its own accept list).
//
// An unknown name from ANY source is a hard error naming the source — never
// a silent fallback to open (the GAP-116 preset / NET-BUNKER-010 refusal
// law).
func (c *Config) ResolveEgressMode(requested string) (EgressConfig, error) {
	mode, err := egress.Resolve(firstNonEmpty(requested, c.Agent.Egress.Mode))
	if err != nil {
		if strings.TrimSpace(requested) != "" {
			return EgressConfig{}, fmt.Errorf("spawn request: %w", err)
		}
		return EgressConfig{}, fmt.Errorf("agent.egress.mode: %w", err)
	}
	out := EgressConfig{Mode: mode}
	if mode == EgressModeAllowlist {
		out.Allowlist = c.Agent.Egress.Allowlist
	}
	return out, nil
}

// firstNonEmpty returns the first non-empty (post-trim) string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}
