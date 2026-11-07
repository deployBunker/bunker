// Daemon-side network-isolation READ-BACK (INT-CI-020).
//
// Why this file exists: the spawn path asked systemd for the mode's property
// (--property=PrivateNetwork=yes for `systemd`) and then trusted the request.
// A green `systemd-run` exit status is not evidence that the boundary is in
// force — systemd accepts the argv, and a unit that was created before the
// property took effect, or a property systemd silently normalised, still
// leaves the agent in the host network namespace. specs/network-isolation.md
// §5.2 states the rule this file implements: "requested" and "provided" are
// different fields, the daemon reports what it ACTUALLY ENFORCED, and an
// unverified boundary is never claimed.
//
// So: after the unit is up, read the property BACK from the running unit. A
// read-back that does not confirm the boundary fails the spawn at
// StageNetworkReadback, which means the rollback tears the agent down — no
// record is ever written claiming a boundary that was not observed. Modes
// that enforce nothing on the network axis (`shared`) have no property to
// read back; they report verified=false, which is the honest answer, never a
// manufactured "verified".
package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/deployBunker/bunker/internal/netmode"
)

// unitPropertyTimeout bounds ONE read-back probe. It is short on purpose: this
// runs inside the 300s spawn RPC against a unit that has just been started, so
// `systemctl show` answers immediately or the host is wedged and the spawn must
// fail loudly rather than hang.
const unitPropertyTimeout = 10 * time.Second

// unitPropertyReader is the injected probe seam (the dockerdProcessChecker
// pattern): tests substitute it, production shells out to systemctl. It reads
// ONE property of one unit and returns the value as systemd renders it (the
// `--value` form, whitespace-trimmed).
var unitPropertyReader = func(ctx context.Context, unitName, property string) (string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", unitName, "--property="+property, "--value").Output()
	if err != nil {
		return "", fmt.Errorf("systemctl show %s --property=%s: %w", unitName, property, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// splitProperty turns the pinned argv element form ("Name=value", the exact
// constants internal/netmode emits into the unit argv) into the property name
// and the value the RUNNING unit must report. Using the same constants the
// builder uses keeps the read-back and the request from drifting apart: a mode
// whose argv element changes cannot silently stop being verified.
func splitProperty(element string) (name, want string, ok bool) {
	if i := strings.IndexByte(element, '='); i > 0 && i < len(element)-1 {
		return element[:i], element[i+1:], true
	}
	return "", "", false
}

// requiredUnitProperty maps an isolation mode to the unit property that proves
// it. ok=false means the mode enforces nothing on this axis (shared, empty, or
// an unknown name the caller should have resolved already) — there is nothing
// to read back, and inventing a "verified" answer for it would be a
// manufactured bound.
func requiredUnitProperty(mode string) (name, want string, ok bool) {
	switch mode {
	case netmode.ModeSystemd:
		return splitProperty(netmode.PropertyPrivateNetworkYes)
	case netmode.ModeProcVis:
		return splitProperty(netmode.PropertyProtectProcInvisible)
	default:
		return "", "", false
	}
}

// verifyUnitNetworkMode reads the mode's enforcement property back from the
// RUNNING unit.
//
// Returns verified=true only when systemd reports the exact value the argv
// asked for. verified=false with a nil error is the "this mode enforces
// nothing" arm (shared/empty) and MUST NOT be read as success-by-default: the
// caller records that false, so `bunker info` says the boundary was not
// verified instead of implying it was.
//
// A probe that cannot run and a probe that reports the wrong value are BOTH
// errors: one means the boundary is unverified, the other means it is absent,
// and neither may be reported as contained.
func verifyUnitNetworkMode(ctx context.Context, unitName, mode string) (bool, error) {
	name, want, ok := requiredUnitProperty(mode)
	if !ok {
		return false, nil
	}
	// The probe runs detached from the spawn request (the rollback-budget
	// lesson, DF-BUNKER-21): a client that gives up must not turn a real
	// verification into an instant context error, and this probe must never
	// inherit an already-expired context.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unitPropertyTimeout)
	defer cancel()

	got, err := unitPropertyReader(rctx, unitName, name)
	if err != nil {
		return false, fmt.Errorf("network isolation mode %q was REQUESTED but the enforced unit property could not be read back: %w — refusing to record a boundary the daemon cannot verify", mode, err)
	}
	if got != want {
		return false, fmt.Errorf("network isolation mode %q was REQUESTED but the running unit %s reports %s=%q, want %q — the mode was NOT applied, so this spawn is refused instead of being recorded as contained", mode, unitName, name, got, want)
	}
	return true, nil
}
