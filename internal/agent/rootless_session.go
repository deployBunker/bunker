// Package agent manages agent lifecycle: create users, generate SSH keys, start dockerd.
package agent

import (
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ── DF-BUNKER-66: the rootless-docker session's OWN default.target services ─
//
// Dogfood run 20 (docs/dogfood/2026-09-25c) measured the live-process gate
// refusing destroys that named NO operator process: the counted processes were
// the rootless-docker login session's OWN default.target services —
// dbus-daemon, pipewire, wireplumber, mpris-proxy — plus their user manager.
// Three properties make that class un-remediable from the agent side, which is
// exactly what the gate's operator remedy (the exact kill command the refusal
// now carries: `bunker ssh <agent> -- pkill -U <uid>`, or the `destroy
// --force` escalation) demands:
//
//  1. they are session-infrastructure, not agent payload: the agent image's
//     rootless install enables linger, systemd starts the user manager and
//     its default.target, and the rootless stack's prerequisites pull in
//     dbus-user-session — so EVERY rootless agent carries them;
//  2. `bunker exec` cannot stop them (the exec's session close reaps its
//     children) and a detached `systemctl --user stop` inside the agent is
//     respawned by the user manager, because default.target keeps wanting
//     the socket-activated units alive;
//  3. they are not the failure class DF-BUNKER-34's gate exists to prevent
//     (the cube-las-00 orphan shape: an operator process holding the home
//     and ports after its user record is gone).
//
// The remedy therefore moves into the destroy path itself, in two halves that
// mirror the DF-BUNKER-56 session-pair shape: ABSORB the class when it
// survives (isRootlessSessionProcess), and STOP it at the source before the
// gate (stopRootlessSessionServices). A real operator process — the scheduler
// daemon, the node server — still refuses loudly exactly as before.

// rootlessSessionServiceNames are the well-known session services the
// rootless-docker login session's default.target keeps alive. Matched on the
// basename of the process (the status Name: line — what /proc carries even
// when the cmdline is unreadable, e.g. "(dbus-daemon)" zombie/comm forms).
// The set is deliberately SMALL and named: a process that merely LOOKS like
// session infrastructure (an operator's own `pipewire-recorder` helper) does
// not match — only the exact well-known session daemons do.
var rootlessSessionServiceNames = []string{
	"dbus-daemon",
	"dbus-broker",
	"dbus-broker-launch",
	"pipewire",
	"pipewire-pulse",
	"wireplumber",
	"mpris-proxy",
	"xdg-desktop-portal",
	"xdg-document-portal",
	"xdg-permission-store",
}

// isRootlessSessionProcess reports whether one live process owned by the
// agent uid is the rootless login session's OWN default.target service —
// the dbus/pipewire/wireplumber/mpris-proxy session infrastructure every
// rootless-docker agent legitimately carries (DF-BUNKER-66). These are
// lifecycle infrastructure in the same sense as DF-BUNKER-56's session pair
// (which this classifier SUBSUMES: the pair is checked first): spawn enables
// linger, the user manager starts default.target, and dbus-user-session is
// a rootless-docker prerequisite — the services are not agent payload, the
// agent's operator never started them, and the destroy path stops them at
// the source (stopRootlessSessionServices) before the gate counts anything.
//
// Matching is on the process basename from the /proc record (the name the
// kernel carries even when the cmdline is unreadable), so the same cmdline
// obstructions the session-pair classifier handles stay covered here. A
// basename is considered ONLY when it is exact — no prefix or suffix
// matching — so an operator's own tool whose name merely contains one of
// these strings is never absorbed.
func isRootlessSessionProcess(p userProcess) bool {
	if isAgentSessionProcess(p) {
		// DF-BUNKER-56's pair and its INT-CI-041 systemctl-client tolerance
		// are the narrower, already-shipped class: subsume them.
		return true
	}
	fields := strings.Fields(strings.TrimSpace(p.Cmd))
	if len(fields) == 0 {
		return false
	}
	// The process identity is the basename of argv0; the comm fallback
	// renders as "[name]" / "(name)" (with or without argv), so strip both
	// bracket styles after the base. Exact match ONLY — no prefix or suffix
	// matching — so an operator's own tool whose name merely contains one of
	// these strings is never absorbed.
	base := filepath.Base(fields[0])
	base = strings.TrimSuffix(strings.TrimPrefix(base, "("), ")")
	base = strings.TrimSuffix(strings.TrimPrefix(base, "["), "]")
	for _, name := range rootlessSessionServiceNames {
		if base == name {
			return true
		}
	}
	return false
}

// isRootlessSessionProcessOnly reports whether p is session infrastructure
// WITHOUT subsuming the session-pair classifier — the predicate the stop step
// and the absorption split use when they must distinguish "the pair" from
// "the services" (the pair is terminateAgentUserManager's business, the
// services are the stop sweep's).
func isRootlessSessionProcessOnly(p userProcess) bool {
	if isAgentSessionProcess(p) {
		return false
	}
	return isRootlessSessionProcess(p)
}

// stopRootlessSessionTimeout bounds the whole stop sweep (one stop + one
// verify poll, plus the terminal SIGKILL pass). Package var (test seam, the
// terminateUserManagerGrace convention); production never writes it.
var stopRootlessSessionTimeout = 8 * time.Second

// stopRootlessSessionPollInterval is the verify poll's granularity.
const stopRootlessSessionPollInterval = 200 * time.Millisecond

// stopRootlessSessionRunner runs ONE stop-sweep command (systemctl --user /
// kill). Package-level seam mirroring disableUserUnit / gateForceKillRunner:
// tests inject a recorder so the sweep is provable without a real user
// manager; production code never swaps it.
var stopRootlessSessionRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// stopRootlessSessionServices is the DF-BUNKER-66 remedy: stop the rootless
// login session's default.target services AT THE SOURCE, before the live
// process gate counts them. Ordering is the whole point: the agent's user
// manager is what respawns the services (dogfood run 20: `systemctl --user
// stop` from inside the agent never stuck), so the manager is TERMINATED
// first (the destroy path's existing terminateAgentUserManager, step 2b.05),
// and only then are the services stopped and, if anything survives the
// bounded grace, SIGKILLed by pid. Best-effort BY DESIGN: every failure is
// logged and the destroy proceeds — the gate (with the widened absorption)
// remains the correctness backstop, and a genuinely foreign operator process
// still refuses the destroy exactly as before.
//
// Called from Destroy AFTER terminateAgentUserManager and BEFORE
// gateDestroyOnLiveProcesses. execCtx is the CALLER'S budgeted step context
// (BNK-DF-001): the sweep's execs are compensating actions that must run on
// their own budget, never on the (possibly cancelled) request context.
func (m *AgentManager) stopRootlessSessionServices(execCtx context.Context, username string, logger *slog.Logger) {
	uid, ok := resolveUsernameUID(username)
	if !ok {
		logger.Debug("skipping rootless session-service stop: uid unresolvable",
			"username", username)
		return
	}
	procs, perr := listUserProcesses(uid)
	if perr != nil {
		logger.Warn("rootless session-service stop: uid process scan failed (continuing destroy)",
			"username", username, "uid", uid, "error", perr)
		return
	}
	var targets []userProcess
	for _, p := range procs {
		if isRootlessSessionProcessOnly(p) {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		logger.Debug("no rootless session services to stop",
			"username", username, "uid", uid)
		return
	}
	logger.Info("stopping the rootless session's own services before the destroy gate",
		"username", username, "uid", uid, "services", len(targets))

	// 1. Ask the (already-terminated) user manager to stop the session
	//    target — best-effort; the manager may already be gone, in which
	//    case the services hold no supervisor and stop on their own or die
	//    in the sweep below.
	if _, err := stopRootlessSessionRunner(execCtx, "systemctl", "--user", "stop", "default.target"); err != nil {
		logger.Debug("systemctl --user stop default.target failed (continuing)",
			"username", username, "error", err)
	}

	// 2. Bounded wait for the services to exit, then SIGKILL the survivors
	//    by pid (the manager is gone, so nothing respawns them).
	deadline := time.Now().Add(stopRootlessSessionTimeout)
	for {
		remaining := m.scanRootlessSessionTargets(uid)
		if len(remaining) == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			logger.Warn("rootless session services still alive after the stop grace; SIGKILLing by pid",
				"username", username, "uid", uid, "pids", procPIDs(remaining))
			for _, p := range remaining {
				if _, kerr := stopRootlessSessionRunner(execCtx, "kill", "-KILL", strconv.Itoa(p.PID)); kerr != nil {
					logger.Warn("SIGKILL of a rootless session service failed (process may have already exited)",
						"username", username, "pid", p.PID, "error", kerr)
				}
			}
			return
		}
		select {
		case <-execCtx.Done():
			return
		case <-time.After(stopRootlessSessionPollInterval):
		}
	}
}

// scanRootlessSessionTargets re-probes the uid for live session-service
// processes only. A scan failure yields nothing to kill — the caller then
// logs nothing and exits the sweep; the gate's own probe re-reports the
// state honestly.
func (m *AgentManager) scanRootlessSessionTargets(uid uint32) []userProcess {
	procs, err := listUserProcesses(uid)
	if err != nil {
		return nil
	}
	var out []userProcess
	for _, p := range procs {
		if isRootlessSessionProcessOnly(p) {
			out = append(out, p)
		}
	}
	return out
}

// procPIDs renders the pids of a process list for one log field.
func procPIDs(procs []userProcess) []int {
	out := make([]int, 0, len(procs))
	for _, p := range procs {
		out = append(out, p.PID)
	}
	return out
}

// splitSessionAndForeign classifies the gate's probe result into the
// tolerable session-infrastructure remainder (DF-BUNKER-56 pair +
// DF-BUNKER-66 rootless session services) and the FOREIGN operator
// processes that must still refuse the destroy. Single construction so the
// non-force gate, the force gate and the terminate step's waiter cannot
// drift apart.
func splitSessionAndForeign(procs []userProcess) (session, foreign []userProcess) {
	for _, p := range procs {
		if isRootlessSessionProcess(p) {
			session = append(session, p)
		} else {
			foreign = append(foreign, p)
		}
	}
	return session, foreign
}
