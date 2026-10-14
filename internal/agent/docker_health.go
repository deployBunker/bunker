package agent

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ── DF-BUNKER-72 part 3: the health surface ────────────────────────────────
//
// On dogfood run 21 the docker daemon of agent dfops0925c never came up (a
// recycled uid's stale rootlesskit state dir, see dockerd_state_dir.go) and
// every status plane reported the agent as plain `running` for the whole life
// of the agent: `docker ps` dial-failed against a socket that was never
// created, while `bunker info` printed `Status: running`. Nothing surfaced the
// failure at all.
//
// This surface asks the AGENT USER'S OWN systemd manager about the unit
// Docker's rootless installer wrote and starts —
// ~/.config/systemd/user/docker.service, i.e. `systemctl --user is-active
// docker.service` as the agent user, through the same session seam the install
// path uses (userSessionRunner + sessionCommand, so the probe speaks to that
// manager through the in-band bus environment rather than through whatever
// environment the daemon happens to carry).
//
// It is deliberately narrow, because it rides a request path:
//   - bounded (dockerHealthProbeBudget) and best-effort;
//   - it only ever ANNOTATES a status; the healthy path is byte-identical;
//   - it claims degraded ONLY for the one answer that proves a broken daemon
//     (`failed`) and stays silent for everything it cannot prove — a missing
//     unit, a probing error, an unreadable uid. A health surface that guesses
//     is worse than none: it would mark every non-docker host degraded.

const (
	// dockerServiceUnitName is the installer's per-user Docker unit. It is NOT
	// the manager's per-agent unit (bunker-docker-<agentID>, started by
	// systemd-run under a different XDG_RUNTIME_DIR).
	dockerServiceUnitName = "docker.service"

	// dockerUnitActive and dockerUnitFailed are the two `systemctl --user
	// is-active` answers this surface interprets. Every other answer
	// (inactive, activating, deactivating, unknown, the unit-not-found text)
	// carries no verdict here.
	dockerUnitActive = "active"
	dockerUnitFailed = "failed"

	// DockerDegradedQualifier is the parenthesised qualifier appended to an
	// agent's status when its docker is provably broken: "running" becomes
	// "running (docker degraded)".
	DockerDegradedQualifier = "docker degraded"

	// dockerFailedDetail is the cause returned by DockerDegradedDetail — the
	// specific answer the probe classified, kept separate from the qualifier
	// so a caller can distinguish "degraded, docker.service failed" from any
	// future cause without changing the rendered status vocabulary.
	dockerFailedDetail = dockerServiceUnitName + " failed"

	// dockerHealthProbeBudget bounds the whole probe (su + systemctl). The
	// info path must not be slowed by an unresponsive host: the probe is a
	// courtesy signal, never a source of truth that is worth waiting for.
	dockerHealthProbeBudget = 3 * time.Second
)

// dockerUnitProbeCmd is the probe script the agent's login shell runs. The
// runtime directory and the bus address are delivered IN-BAND by
// sessionCommand (INT-SPAWN-004), so a `su -` environment reset cannot strip
// them.
const dockerUnitProbeCmd = "systemctl --user is-active " + dockerServiceUnitName

// dockerUnitStateProbe asks the agent user's manager for the unit state.
// Package-level seam: unit tests drive every branch (active / failed /
// unprovable) without a real su, a real systemd or a real agent user.
var dockerUnitStateProbe = probeDockerUserUnitState

// probeDockerUserUnitState is the production probe. `systemctl --user
// is-active` reports a NON-active state by PRINTING it and exiting non-zero,
// so the returned error is frequently the state's own exit status rather than
// a transport failure — the caller classifies on the output and treats the
// error only as "we got no usable answer" when the output is empty.
func probeDockerUserUnitState(ctx context.Context, username, runtimeDir string) (string, error) {
	out, err := userSessionRunner(ctx, username, runtimeDir, sessionCommand(runtimeDir, dockerUnitProbeCmd))
	return strings.TrimSpace(string(out)), err
}

// classifyDockerUnitState maps the probe's answer onto a degraded-status cause.
//
// It reads the STATE STRING FIRST and the error second ON PURPOSE: for
// `failed` the error IS the state's own exit status, so an
// error-first reading would discard the one answer that matters. An empty or
// unrecognised body — a dead bus, a killed probe, `inactive`, `activating`,
// `unknown`, the unit-not-found text — yields NO cause: absence is never
// upgraded to a degraded verdict.
func classifyDockerUnitState(state string, _ error) string {
	fields := strings.Fields(state)
	if len(fields) == 0 {
		return ""
	}
	switch fields[0] {
	case dockerUnitFailed:
		return dockerFailedDetail
	case dockerUnitActive:
		return ""
	}
	return ""
}

// DockerDegradedDetail returns a degraded-status cause for agentID's rootless
// Docker, or "" when the agent's docker is healthy, not provably broken, or
// has no user to ask (a user record that no longer resolves is the orphan
// probe's state, not this one's).
//
// Bounded and best-effort throughout: every failure path returns "".
func (m *AgentManager) DockerDegradedDetail(agentID string) string {
	username := "bunker-" + agentID
	u, err := userLookup(username)
	if err != nil {
		return "" // no user on the host → no docker unit to have an opinion about
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerHealthProbeBudget)
	defer cancel()
	runtimeDir := filepath.Join(userRuntimeBaseDir, strconv.Itoa(uid))
	state, probeErr := dockerUnitStateProbe(ctx, username, runtimeDir)
	detail := classifyDockerUnitState(state, probeErr)
	switch {
	case detail != "" && m.logger != nil:
		// The operator-facing signal: this is the line that makes a broken
		// docker visible in the daemon log instead of only in the agent's own
		// journal.
		m.logger.Warn("agent reports a degraded docker daemon",
			"agent_id", agentID, "uid", uid, "unit", dockerServiceUnitName,
			"detail", detail, "probe_output", state)
	case detail == "" && probeErr != nil && m.logger != nil:
		// No verdict AND no usable answer: say so at debug level rather than
		// pretending the probe succeeded.
		m.logger.Debug("docker health probe produced no verdict",
			"agent_id", agentID, "error", probeErr, "probe_output", state)
	}
	return detail
}

// DockerStatus qualifies an agent's status with its docker health: a status of
// "running" for an agent whose docker.service is `failed` becomes
// "running (docker degraded)".
//
// The healthy path returns status BYTE-IDENTICAL (zero-delta), and an empty
// status is never invented — a record with no status renders as the bare
// qualifier rather than a fabricated "running".
func (m *AgentManager) DockerStatus(agentID, status string) string {
	if m.DockerDegradedDetail(agentID) == "" {
		return status
	}
	return dockerDegradedStatus(status)
}

// dockerDegradedStatus composes the rendered qualifier onto a base status.
func dockerDegradedStatus(status string) string {
	if status == "" {
		return DockerDegradedQualifier
	}
	return status + " (" + DockerDegradedQualifier + ")"
}
