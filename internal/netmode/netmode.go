// Package netmode is the network-isolation mode surface (NET-BUNKER-010) and
// the `systemd` mode (NET-BUNKER-002) — specs/network-isolation.md is the
// design authority.
//
// The valid set lives in exactly ONE place (ValidModes): NET-BUNKER-003/004/
// 005/012 add future modes by extending that slice and teaching
// PropertiesFor the new mode's unit property — everything else (validation,
// refusals, reporting, boundary strings) reads from here.
//
// The reporting law (spec §5.2): A BOUND THAT IS NOT REPORTED IS NOT A BOUND.
// Three meanings stay distinct — ModeShared/ModeSystemd/ModeProcVis are
// affirmative statements, ModeUnknown means the daemon could not verify, and
// EMPTY means the record predates the field. Empty must never render as safe,
// and unknown must never be upgraded to "shared".
//
// Refusal law (spec §5.2, refuse-loudly): an unknown or unimplemented mode is
// a named error naming the offending value and the valid set — it NEVER falls
// back to shared and reports success. A silent fallback is a manufactured
// bound, which §5.2 forbids.
package netmode

import (
	"fmt"
	"sort"
	"strings"
)

// The mode vocabulary. Exactly two valid values today (spec §1.1, §1.2):
//
//   - shared (the declared default, §5.1): the agent runs in the host network
//     namespace; nothing on the network axis is isolated. Default unchanged
//     until measured (NET-BUNKER-008 owns the measurement).
//   - systemd (NET-BUNKER-002): the agent's dockerd transient unit is created
//     with --property=PrivateNetwork=yes — its own network namespace
//     containing only loopback. A genuinely private 127.0.0.1 and a private
//     port space (protects against co-tenant reach T2 and bind-squat T3).
//   - procvis (NET-BUNKER-011): the agent's dockerd transient unit AND every
//     detached-run unit are created with --property=ProtectProc=invisible —
//     inside the unit's own mount namespace /proc carries hidepid=2
//     semantics, so the unit's processes see only their own user's
//     processes. Process-identity reconnaissance (spec §2 T4 / ISO-001: one
//     agent enumerating another agent's rootlesskit/containerd command lines
//     through the global /proc) dies at the unit boundary. It is
//     per-UNIT, not the host-wide hidepid remount — the host's /proc mount
//     and every other process's view are untouched.
const (
	ModeShared  = "shared"
	ModeSystemd = "systemd"
	ModeProcVis = "procvis"

	// ModeUnknown is the "the daemon cannot verify" state (spec §5.2, the
	// tmp_isolation="unknown" precedent). It is NOT a valid spawn request
	// value; it exists only for reporting.
	ModeUnknown = "unknown"

	// PropertyPrivateNetwork is the systemd property the `systemd` mode adds
	// to the agent's dockerd transient unit. Unix sockets are filesystem
	// objects and cross network namespaces, so the docker.sock contract
	// (DOCKER_HOST=unix:///run/bunker/<id>/docker.sock, the ssh -L 2376:
	// tunnel, sshfs) is UNCHANGED under this mode — that property is why the
	// mode is viable at all (spec §3, composition rule 1).
	PropertyPrivateNetwork = "PrivateNetwork"

	// PropertyPrivateNetworkYes is the exact --property argv element the
	// `systemd` mode adds. Pinned by the mode tests; byte-for-byte.
	PropertyPrivateNetworkYes = "PrivateNetwork=yes"

	// PropertyProtectProcInvisible is the exact --property argv element the
	// `procvis` mode (NET-BUNKER-011) adds. ProtectProc=invisible (systemd
	// ≥247, system units) remounts /proc inside the unit's own mount
	// namespace with hidepid=2 semantics, so the unit's processes see only
	// their own user's processes. It is PER-UNIT — the host's /proc mount
	// and every other process's view are untouched — which is exactly the
	// per-agent property the spec's host-wide-hidepid option (§1.8) lacks.
	PropertyProtectProcInvisible = "ProtectProc=invisible"
)

// validModes is THE list of modes this build implements. A new mode is a
// one-line addition here plus its PropertiesFor/systemdProperty entry — the
// refusal text, validation, reporting vocabulary and CLI all derive from this
// slice, so a new mode cannot half-ship (named in the set but unimplemented,
// or implemented but unreportable).
var validModes = []string{ModeShared, ModeSystemd, ModeProcVis}

// DefaultMode is the declared default (spec §5.1): shared, unchanged, until
// NET-BUNKER-008 measures otherwise. Any change to this constant is a new
// decision with its own row and its own numbers — never a rider.
const DefaultMode = ModeShared

// ValidModes returns the implemented mode names in a stable order.
func ValidModes() []string {
	out := make([]string, len(validModes))
	copy(out, validModes)
	return out
}

// ValidMode reports whether name is an implemented mode. Surrounding
// whitespace is trimmed before the exact match — the GAP-116 preset
// convention (config.ValidSafetyPreset) — so " systemd " names the mode
// while a whitespace-only value is NOT valid here (absence is expressed by
// the empty string and handled by Resolve, not by validation).
func ValidMode(name string) bool {
	name = strings.TrimSpace(name)
	for _, m := range validModes {
		if name == m {
			return true
		}
	}
	return false
}

// Resolve validates a requested mode name and applies the empty-default rule.
// Empty means "defer to the daemon default" and returns DefaultMode;
// surrounding whitespace is trimmed first (the GAP-116 convention). An
// unknown name is a named error carrying the offending TRIMMED value and the
// valid set — the exact text surfaces in spawn refusals and CLI errors. An
// unimplemented future mode (a name accepted by a newer daemon) refuses with
// the same error shape here: this build must never silently accept a name it
// cannot enforce.
func Resolve(requested string) (string, error) {
	trimmed := strings.TrimSpace(requested)
	if trimmed == "" {
		if requested != "" {
			// Whitespace-only is not "unset" — it is a bad value. Refuse;
			// never guess that the operator meant the default.
			return "", fmt.Errorf("unknown network isolation mode %q (valid: %v)", requested, ValidModes())
		}
		return DefaultMode, nil
	}
	if !ValidMode(trimmed) {
		return "", fmt.Errorf("unknown network isolation mode %q (valid: %v)", trimmed, ValidModes())
	}
	return trimmed, nil
}

// BoundaryFor returns the reported boundary string for an ENFORCED mode — the
// plain-language contract of what the mode actually provides (spec §5.2).
// These strings are pinned by tests and rendered by `bunker info`/`bunker
// list` and the in-band marker; they must stay honest about the mode's LIMITS
// (spec §2: implementation rows may narrow claims, never widen them):
//
//   - shared isolates nothing on the network axis — say so instead of
//     manufacturing comfort;
//   - systemd gives a private loopback + port space and loses ALL outbound
//     (docker image pulls fail in this mode — that is the accepted cost, not
//     a bug), and covers only processes inside the unit;
//   - procvis (NET-BUNKER-011) gives the unit's processes a private /proc
//     and says PLAINLY what that is not: it does not cover the agent's
//     SSH/exec sessions (they keep the host's global /proc view —
//     sessions have no /proc visibility knob), it is not a network
//     boundary, and it does NOT stop another agent from enumerating this
//     unit's processes through /proc/<unit-pid>/root/proc — path
//     visibility is NET-BUNKER-012, not this mode. (systemd scopes each
//     service to its own /proc/PID; hidepid only restricts readdir of /
//     — the enumeration is not stopped by design.)
//
// An empty or unknown mode has NO boundary string: the boundary of an
// unverified state is not knowable, and inventing one would be the exact
// manufactured confidence §5.2 forbids.
func BoundaryFor(mode string) string {
	switch mode {
	case ModeShared:
		return "host network namespace (no network isolation: shared loopback, global port space)"
	case ModeSystemd:
		return "private network namespace (loopback only): private 127.0.0.1 and port space; NO outbound networking (image pulls unavailable); covers only processes in the agent's dockerd unit"
	case ModeProcVis:
		return "private /proc in the unit's mount namespace (hidepid=2 semantics): unit processes see only their own user's processes; does NOT cover the agent's SSH/exec sessions (host /proc view unchanged there), is NOT a network boundary, and does NOT stop another agent enumerating this unit via /proc/<unit-pid>/root/proc"
	default:
		// ModeUnknown and the empty pre-surface state report no boundary —
		// see the func comment.
		return ""
	}
}

// systemdProperties lists the EXTRA unit properties the mode adds, in argv
// order. `shared` adds none (byte-identical spawn — spec §5.1 zero-delta);
// `systemd` adds exactly one: --property=PrivateNetwork=yes (NET-BUNKER-002);
// `procvis` adds exactly one: --property=ProtectProc=invisible
// (NET-BUNKER-011). A future mode teaches this table its property and the
// whole spawn path follows.
func systemdProperties(mode string) []string {
	switch mode {
	case ModeSystemd:
		return []string{"--property=" + PropertyPrivateNetworkYes}
	case ModeProcVis:
		return []string{"--property=" + PropertyProtectProcInvisible}
	default:
		return nil
	}
}

// PropertiesFor returns the extra systemd-run --property elements for mode,
// in argv order. The EMPTY mode behaves as shared (no properties): the
// zero-value struct field is the pre-surface/shared path, so every existing
// caller and test keeps its exact argv. Unknown non-empty modes return an
// error — callers resolve the mode first (Resolve refuses unknown names), so
// the builder error is a programming-error guard, and in
// buildRootlessDockerdArgs it fires BEFORE any systemd-run state is created.
func PropertiesFor(mode string) ([]string, error) {
	if mode == "" {
		return nil, nil
	}
	if !ValidMode(mode) {
		return nil, fmt.Errorf("unknown network isolation mode %q (valid: %v)", mode, ValidModes())
	}
	return systemdProperties(mode), nil
}

// containmentMarker renders the in-band system-info marker line (NET-BUNKER-010
// §5.2), reusing the GAP-067 containment-disclosure idiom: one fixed,
// greppable, bracketed line an agent session can discover through normal
// reconnaissance (specs/containment-disclosure.md). An empty or unknown mode
// renders the UNKNOWN marker — never a shared/affirmative one — so an older
// daemon's absence and an unverified state are both visible and neither
// reads as safe.
func containmentMarker(mode string) string {
	switch mode {
	case ModeShared, ModeSystemd, ModeProcVis:
		return fmt.Sprintf("[bunker: network isolation mode %s — %s]", mode, BoundaryFor(mode))
	default:
		return "[bunker: network isolation mode unknown — not reported by this daemon; the boundary is NOT verified]"
	}
}

// ContainmentMarker is the exported marker builder. The mode argument is the
// daemon-resolved mode (empty and "unknown" both render the unknown marker).
// The line is bracketed, self-describing and greppable by design: grep -F
// "network isolation mode" over probe output identifies the reported state.
func ContainmentMarker(mode string) string {
	return containmentMarker(mode)
}

// SortedValidModes returns the valid set sorted (stable refusal text in
// tests and errors that choose to sort).
func SortedValidModes() []string {
	out := ValidModes()
	sort.Strings(out)
	return out
}

// RefusalError builds the canonical refuse-loudly error for an unusable mode
// request: it names the offending value, the valid set, and WHY it refuses
// (never a silent fallback to shared — spec §5.2). Used by the daemon's
// spawn path and by any caller that must refuse before creating state.
func RefusalError(requested string) error {
	return fmt.Errorf("network isolation mode %q is not available on this server (valid: %s) — refusing the spawn, not falling back to %s", requested, strings.Join(SortedValidModes(), ", "), ModeShared)
}
