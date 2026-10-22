// Daemon-side plumbing for the procvis mode (NET-BUNKER-011;
// specs/network-isolation.md §1.8 and §5.2 are the design authority).
//
// ResolveProcVisUnitProperties is the unit-property step for the mode: it
// hands the builder the mode's exact systemd property (--property=
// ProtectProc=invisible) and — the part that makes the boundary real —
// verifies the kernel semantics ONCE per daemon process before the first
// procvis unit is created. The verification result is cached with
// sync.OnceValue: a failing host fails EVERY procvis spawn (no per-spawn
// re-probing, no partial mode), a passing host pays the experiment cost once.
//
// The probe is dispatched ATOMICALLY (a temp binary + rename): a concurrent
// test or a second bunker process racing the same temp filename can never
// read a half-written script (the null-byte-in-source class — exec would
// fail with a confusing format error, or worse, run a truncated experiment).
package agent

import (
	"sync"

	"github.com/deployBunker/bunker/internal/netmode"
)

// procVisVerifyOnce caches the host's kernel-semantics verdict for the
// lifetime of this daemon process. sync.OnceValue gives both properties the
// refusal law needs: fail-closed on an unverifiable host (the error is
// returned on every call), and exactly one experiment per process.
var procVisVerifyOnce = sync.OnceValue(netmode.VerifyProcVisHost)

// procVisRuntimeVerifyOnce is the REAL-/proc half of the gate (ISO-001):
// the running system /proc that hosts every bunkerd unit must carry the
// hiding options, or sibling agents' processes remain in each other's view.
// Refusals from this half are *ProcVisMountOptionsError; success means the
// host /proc mounts an ENFORCED boundary the units' gates can be verified
// under. (On the shipped hosts the gate is BLIND to foreign uid without a
// proc group the daemon holds, which is why the group is REQUIRED by the
// gate contract — it is a config input, not a host property, and arrives via
// the m.cfg Agent proc-vis gate wiring. Failing to configure it makes the
// mode not verifiable, i.e. unavailable — the same refusal surface.)

var procVisRuntimeVerifyOnce = sync.OnceValue(netmode.VerifyProcVisRuntime)

// procVisVerified runs BOTH halves of the procvis host gate and returns the
// first refusal (nil when the host passes). A refusal is a *netmode error
// carrying the named detail; the caller refuses the spawn/run at its own
// validate stage and NEVER degrades to shared.
func procVisVerified(m *AgentManager) error {
	if err := procVisVerifyOnce(); err != nil {
		return err
	}
	// ISO-001 real-/proc gate: only armed when the daemon configured a proc
	// group (the group-holding posture). A daemon that never opted in
	// (m == nil is nil-safe for procVisVerified(nil) callers) pays no
	// refusal — the boundary claims it makes are the units' own, not the
	// mount's.
	if m != nil && m.cfg != nil && m.cfg.Agent.ProcVis.ProcVisEnabled() {
		return procVisRuntimeVerifyOnce()
	}
	return nil
}

// procVisUnitProperties builds the mode's systemd-run --property elements
// after verifying the host. procVisVerified (below) is the gate the spawn
// and RunAgent paths call; this helper is kept next to it so the verified
// property construction stays in one place.
func procVisUnitProperties() []string {
	return netmode.ProcVisUnitProperties()
}
