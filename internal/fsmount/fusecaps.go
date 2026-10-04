// Package fsmount — the FUSE capability carrier (BFS-052).
//
// THIS FILE IS OS-NEUTRAL ON PURPOSE. It holds only the reported shapes and
// the requested-vs-effective comparison; the probe that fills them reads
// Linux sources (/proc/self/mountinfo, /sys/fs/fuse/connections,
// /sys/class/bdi) and lives in fusecaps_linux.go, exactly like the rest of
// the platform seam. A future Windows binding reuses these carriers and
// Status() verbatim.
//
// THE ONE VOCABULARY RULE (BFS-052's brief): no second carrier is invented.
// The degradation entry here is the SAME four fields the server's capability
// document has used since the surface shipped its first degradations[] block
// (internal/fsclient/capabilities.go Degradations, rendered by
// internal/server/webdav/ops.go degradations()): capability + scope + mode +
// detail. The mount's own kernel-side verdicts travel in that shape, so a
// consumer reads one degradation vocabulary across the whole surface.
//
// THE STATES ARE THE DF-BUNKER-9 SET (granted / degraded / unknown): an
// unknown is its own state and is never rendered as a safe value — a mount
// that cannot observe a capability reports "unknown, because <source>" and
// never "granted".
package fsmount

import (
	"fmt"
	"sort"
)

// fuseScope is the scope every mount-side degradation carries. The server's
// own entries use "target", "build" and "transport" for their scopes; a
// kernel negotiation is scoped to the mount it happened on.
const fuseScope = "mount"

// CapabilityState is what the kernel actually did with a requested
// capability.
type CapabilityState string

const (
	// CapabilityStateGranted: requested and effective agree (or the kernel
	// runs a strictly larger value than asked). The honest "as asked".
	CapabilityStateGranted CapabilityState = "granted"
	// CapabilityStateDegraded: the kernel is running something OTHER than
	// what was requested — a clamp, an absent feature, a stripped flag.
	// Requested and effective both travel in the entry; a non-empty Reason
	// names the kernel's mechanism (kernel_clamp, feature_absent, ...).
	CapabilityStateDegraded CapabilityState = "degraded"
	// CapabilityStateUnknown: the kernel exposes NO observable for this
	// capability, so the probe cannot say granted or degraded. Unknown is
	// its own state, never a safe default: a mount that reports unknown is
	// saying "I do not know, here is why", not "all is well".
	CapabilityStateUnknown CapabilityState = "unknown"
)

// CapabilityMode is the mode field of a mount-side degradation entry — the
// same field the server's degradations[] uses to name the state the caller
// experiences ("poll" instead of push, "http/1.1" instead of h2).
type CapabilityMode string

const (
	// CapabilityModeDegraded: the capability is in force at the kernel's
	// value, not the requested one.
	CapabilityModeDegraded CapabilityMode = "degraded"
	// CapabilityModeUnknown: the effective state cannot be observed.
	CapabilityModeUnknown CapabilityMode = "unknown"
)

// Capability is one probed kernel capability: what was requested, what the
// kernel is actually running, and why they differ when they do. It is the
// mount-side twin of the server capability document's own degradation
// vocabulary.
type Capability struct {
	// Capability names it, e.g. "fuse:max_readahead", "fuse:readdirplus".
	Capability string `json:"capability"`
	// State is granted, degraded or unknown (the DF-BUNKER-9 set).
	State CapabilityState `json:"state"`
	// Requested is what this mount asked for, rendered as a string ("131072
	// bytes", "on", "off"). Empty when the caller requested nothing — the
	// kernel's own default then applies and there is nothing of ours to
	// refuse.
	Requested string `json:"requested,omitempty"`
	// Effective is what the kernel is actually running, from the observable
	// named in the status block's source. Empty exactly when State is
	// unknown: an unknown state never carries an effective figure, and an
	// effective figure is never reported without the source it was read
	// from.
	Effective string `json:"effective,omitempty"`
	// Reason is the named kernel mechanism whenever State is degraded
	// ("kernel_clamp", "feature_absent", ...) or unknown ("not observable
	// via ..."). Empty only when State is granted.
	Reason string `json:"reason,omitempty"`
}

// Degradation is one requested-but-not-granted (or unobservable) FUSE
// capability. The field set and JSON names are the server capability
// document's degradations[] entry, reused verbatim — one vocabulary, not
// two.
type Degradation struct {
	Capability string `json:"capability"`
	Scope      string `json:"scope"`
	Mode       string `json:"mode"`
	Detail     string `json:"detail"`
}

// String renders the degradation the way the mount log names its other
// structured facts ("capability (scope=... mode=...): detail").
func (d Degradation) String() string {
	return fmt.Sprintf("%s (scope=%s mode=%s): %s", d.Capability, d.Scope, d.Mode, d.Detail)
}

// FuseState is the `fuse` block of the status document (BFS-052): what this
// mount asked the kernel for, what the kernel actually granted, and one
// degradation per capability where the two differ or the probe cannot say.
type FuseState struct {
	// Connection is the kernel's connection id for this mount (the
	// /sys/fs/fuse/connections/<id> directory), when the probe found it.
	Connection *int `json:"connection,omitempty"`
	// Source names where the observable figures were read from — the fact a
	// consumer needs in order to know how each value was learned.
	Source string `json:"source,omitempty"`
	// Capabilities is one entry per probed capability. Absence is a fact
	// too: a capability with no entry was neither requested nor probed.
	Capabilities []Capability `json:"capabilities"`
	// Degradations carries every entry above whose state is not
	// CapabilityStateGranted — the same rule the server's document follows
	// ("anything absent here is available; nothing is implied").
	Degradations []Degradation `json:"degradations,omitempty"`
}

// fuseDegradations derives the degradations[] block from the capability
// entries: one entry per capability whose state is not granted, with the
// requested and effective figures (or the honest unknown) in its detail.
func fuseDegradations(f FuseState) []Degradation {
	var out []Degradation
	for _, c := range f.Capabilities {
		switch c.State {
		case CapabilityStateGranted:
			continue
		case CapabilityStateUnknown:
			// mode=unknown is distinct from mode=degraded — the DF-BUNKER-9
			// rule again: unknown is not a kind of degraded.
			detail := c.Reason
			if detail == "" {
				detail = "the kernel grants no way to observe this capability"
			}
			if c.Requested != "" {
				detail = fmt.Sprintf("requested %s, effective unknown: %s", c.Requested, detail)
			}
			out = append(out, Degradation{
				Capability: c.Capability,
				Scope:      fuseScope,
				Mode:       string(CapabilityModeUnknown),
				Detail:     detail,
			})
		default:
			// A state that is set but neither granted nor unknown is a
			// divergence: requested and effective disagree, and the reason
			// names the kernel's mechanism (kernel clamp, feature absent,
			// stripped by policy). Detail carries BOTH numbers, per the
			// spec's law that a refusal is reported with the figures.
			reason := c.Reason
			if reason == "" {
				reason = "requested value was not granted"
			}
			out = append(out, Degradation{
				Capability: c.Capability,
				Scope:      fuseScope,
				Mode:       string(CapabilityModeDegraded),
				Detail:     fmt.Sprintf("requested %s, effective %s: %s", c.Requested, c.Effective, reason),
			})
		}
	}
	return out
}

// effectiveCap returns the probed entry for one capability name.
func effectiveCap(caps []Capability, name string) (Capability, bool) {
	for _, c := range caps {
		if c.Capability == name {
			return c, true
		}
	}
	return Capability{}, false
}

// formatValue renders a requested/effective boolean as the capability
// vocabulary's value: on/off, never a bare true/false a JSON reader could
// mistake for the state field.
func formatValue(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// sortCapabilities orders the reported entries by name, so a consumer
// comparing two mounts' documents sees a stable list.
func sortCapabilities(caps []Capability) {
	sort.Slice(caps, func(i, j int) bool { return caps[i].Capability < caps[j].Capability })
}
