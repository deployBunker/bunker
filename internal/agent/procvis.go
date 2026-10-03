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

// procVisUnitProperties builds the mode's systemd-run --property elements
// after verifying the host. procVisVerifyOnce (below) is the gate the spawn
// and RunAgent paths call; this helper is kept next to it so the verified
// property construction stays in one place.
func procVisUnitProperties() []string {
	return netmode.ProcVisUnitProperties()
}
