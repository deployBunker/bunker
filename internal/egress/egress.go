// Package egress is the per-agent network egress policy surface (GAP-134;
// REQ-E1 / SEC-06 — docs/prd/security-readiness.md §5.4 and
// docs/threat-model.md "BT4 — Egress" are the design authorities).
//
// Three modes, one declared safe default:
//
//   - open (the default): zero behavior change. No firewall command is ever
//     invoked — a fleet that never configures egress runs byte-identically
//     to a pre-GAP-134 daemon (pinned by TestInstall_OpenModeZeroFirewallCalls).
//   - allowlist: a per-agent default-deny firewall chain keyed on the agent's
//     uid (nftables preferred, iptables fallback). Outbound traffic is dropped
//     except loopback, return traffic of established inbound connections, and
//     the explicitly allowlisted CIDRs/domains (resolved to IPs at rule-install
//     time; DNS itself must be allowed explicitly — see docs/egress-policy.md).
//   - none: deny-all outbound except loopback and established/related return
//     traffic — the bunker control channel (the agent's inbound SSH session
//     and its unix-socket docker control) keeps working by construction.
//
// The vocabulary, chain naming and boundary reporting strings live in exactly
// ONE place (this package) — the netmode ownership rule. The config layer
// re-exports the vocabulary for validation and error text; it never invents
// its own list.
//
// Safety model (requirement 3): every firewall mutation runs in the ROOT
// daemon (bunkerd) through this package. No agent-visible code path, unit
// property, profile line or env var grants an agent any firewall capability —
// an agent cannot see, alter or bypass its chain short of a kernel exploit.
//
// Refusal law (the netmode §5.2 shape): an unknown mode is a named error
// naming the offending value and the valid set — it never silently falls back
// to open. A failed rule installation in allowlist/none mode fails the spawn
// LOUD (manager.Install returns the error; internal/agent rolls the spawn
// back) — an agent is never left running unenforced while its config claims
// it is restricted.
package egress

import (
	"fmt"
	"sort"
	"strings"
)

// The mode vocabulary. Exactly three valid values:
//
//   - ModeOpen: no enforcement, no firewall interaction (the default).
//   - ModeAllowlist: per-agent chain, default-deny, allowlist accepts.
//   - ModeNone: per-agent chain, deny-all except the control channel.
const (
	ModeOpen      = "open"
	ModeAllowlist = "allowlist"
	ModeNone      = "none"

	// ModeUnknown is the "the daemon cannot verify" reporting state (the
	// netmode.ModeUnknown precedent). It is NOT a valid spawn request value;
	// it exists only for reporting.
	ModeUnknown = "unknown"
)

// validModes is THE list of modes this build implements. A new mode is a
// one-line addition here plus its BoundaryFor entry and its Install branch —
// refusal text, config validation and reporting all derive from this slice,
// so a mode cannot half-ship.
var validModes = []string{ModeOpen, ModeAllowlist, ModeNone}

// DefaultMode is the declared safe default (REQ-E1 "safe default"): open.
// Every existing config, every existing spawn and every existing test keeps
// its exact behavior until an operator explicitly configures egress.
const DefaultMode = ModeOpen

// ValidModes returns the implemented mode names in a stable order.
func ValidModes() []string {
	out := make([]string, len(validModes))
	copy(out, validModes)
	return out
}

// ValidMode reports whether name is an implemented mode. Surrounding
// whitespace is trimmed before the exact match (the GAP-116 preset
// convention); a whitespace-only value is NOT valid — absence is expressed
// by the empty string and resolved by the config resolver, never here.
func ValidMode(name string) bool {
	name = strings.TrimSpace(name)
	for _, m := range validModes {
		if name == m {
			return true
		}
	}
	return false
}

// SortedValidModes returns the valid set sorted (stable refusal text).
func SortedValidModes() []string {
	out := ValidModes()
	sort.Strings(out)
	return out
}

// Resolve validates a requested mode name and applies the empty-default rule.
// Empty means "defer to the daemon default" and returns DefaultMode; an
// unknown name is a named error carrying the offending trimmed value and the
// valid set — the exact shape of netmode.Resolve (never a silent fallback to
// open).
func Resolve(requested string) (string, error) {
	trimmed := strings.TrimSpace(requested)
	if trimmed == "" {
		if requested != "" {
			return "", fmt.Errorf("unknown egress mode %q (valid: %v)", requested, ValidModes())
		}
		return DefaultMode, nil
	}
	if !ValidMode(trimmed) {
		return "", fmt.Errorf("unknown egress mode %q (valid: %v)", trimmed, ValidModes())
	}
	return trimmed, nil
}

// BoundaryFor returns the honest reporting string for an enforced mode (the
// netmode.BoundaryFor reporting law: a bound that is not reported is not a
// bound, and a report must never overstate). An empty or unknown mode has NO
// boundary string — the boundary of an unverified state is not knowable.
func BoundaryFor(mode string) string {
	switch mode {
	case ModeOpen:
		return "no egress enforcement: unrestricted outbound network access"
	case ModeAllowlist:
		return "default-deny egress chain on the agent's uid: loopback, established/related return traffic and explicitly allowlisted destinations only (nftables, iptables fallback)"
	case ModeNone:
		return "deny-all egress chain on the agent's uid: loopback and established/related return traffic only — the bunker control channel (inbound SSH, unix-socket docker control) still works"
	default:
		return ""
	}
}

// EgressTableName is the nftables table all per-agent chains live in.
// Underscored so the name is a bare nft identifier (no quoting).
const EgressTableName = "bunker_egress"

// ChainName is the deterministic per-agent nftables chain name for uid:
// bunker-egress-<uid>. The name is a pure function of the uid — the same
// agent uid always maps to the same chain, which is what makes removal and
// orphan sweeping bookkeeping-free (no handles, no state file).
func ChainName(uid uint32) string {
	return fmt.Sprintf("bunker-egress-%d", uid)
}

// IPTablesChainName is the per-agent iptables chain for uid. iptables chain
// names are conventionally upper-case and capped at 28 chars; the longest
// possible name (BUNKER-EGRESS-4294967295) is 24.
func IPTablesChainName(uid uint32) string {
	return fmt.Sprintf("BUNKER-EGRESS-%d", uid)
}

// chainNameUIDSuffix is the prefix of every chain this package creates, in
// both backends' spellings. Sweep parses uids out of backend listings with
// these.
const (
	nftChainPrefix      = "bunker-egress-"
	ipTablesChainPrefix = "BUNKER-EGRESS-"
)

// ParseChainUID extracts the uid from a chain name produced by ChainName or
// IPTablesChainName. ok is false for anything else (including a root-uid
// chain that was not created by this package's spelling, foreign chains, and
// the empty string) — the sweep never deletes a chain it cannot positively
// attribute to this package's naming scheme.
func ParseChainUID(name string) (uid uint32, ok bool) {
	for _, prefix := range []string{nftChainPrefix, ipTablesChainPrefix} {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		digits := strings.TrimPrefix(name, prefix)
		if digits == "" {
			return 0, false
		}
		for _, r := range digits {
			if r < '0' || r > '9' {
				return 0, false
			}
		}
		var v uint64
		for _, r := range digits {
			v = v*10 + uint64(r-'0')
			if v > 0xFFFFFFFF {
				return 0, false
			}
		}
		return uint32(v), true
	}
	return 0, false
}
