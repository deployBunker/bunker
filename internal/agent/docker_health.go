package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ── DF-BUNKER-72 part 3 / DF-BUNKER-108: the health surface ────────────────
//
// On dogfood run 21 the docker daemon of agent dfops0925c never came up (a
// recycled uid's stale rootlesskit state dir, see dockerd_state_dir.go) and
// every status plane reported the agent as plain `running` for the whole life
// of the agent. DF-BUNKER-72 answered with a degraded-status qualifier — but
// it classified the INSTALLER'S per-user docker.service, the unit Docker's
// rootless installer writes and starts. That unit is NOT the one that serves
// the agent: the manager starts the agent's dockerd as the transient
// systemd-run unit bunker-docker-<agentID>, and where both start in the same
// second the installer's unit loses a RootlessKit state-lock collision and
// sits `failed` (DF-BUNKER-96) while bunker-docker-<id> serves
// /run/bunker/<agentID>/docker.sock just fine. Live result (DF-BUNKER-108):
// three healthy agents rendered `running (docker degraded)` while their
// `docker ps` answered rc=0.
//
// The law now: the qualifier is derived from the ONLY surface that proves
// the agent's docker works — its socket.
//
//   - /run/bunker/<agentID>/docker.sock present and IS a socket → the
//     agent's docker answers; no qualifier, and nothing else is probed.
//   - socket absent → the serving unit's state decides: `inactive` or
//     `failed` on bunker-docker-<agentID> means the agent has no docker at
//     all, so a `running` status is lying → qualifier. `active` against a
//     missing socket is a transient window (dockerd up, socket not yet
//     bound) and stays silent; so does every answer this probe cannot
//     interpret.
//   - an agent whose stored status does not claim to be serving (stopped,
//     home_retained, failed, empty) is never qualified: its missing socket
//     is expected, not degraded.

const (
	// managerDockerUnitPrefix names the manager's per-agent dockerd unit —
	// bunker-docker-<agentID>, started by systemd-run at spawn (see
	// manager_spawn.go and resource/cgroup.go). This is the unit that
	// actually serves the socket. The installer's per-user docker.service,
	// classified here until DF-BUNKER-108, is deliberately no longer
	// consulted: it collides with the manager unit on recycled uids
	// (DF-BUNKER-96) and its `failed` state says nothing about whether the
	// agent's docker works.
	managerDockerUnitPrefix = "bunker-docker-"

	// dockerSocketName is the per-agent socket file under
	// /run/bunker/<agentID>/ — the DOCKER_HOST every agent session gets and
	// the only interface its docker answers on.
	dockerSocketName = "docker.sock"

	// dockerUnitInactive is the `systemctl --user is-active` answer that,
	// together with a missing socket, proves the agent's docker is gone.
	dockerUnitInactive = "inactive"

	// dockerUnitFailed likewise proves it gone; dockerUnitActive against a
	// missing socket is a transient window and claims nothing.
	dockerUnitFailed = "failed"
	dockerUnitActive = "active"

	// DockerDegradedQualifier is the parenthesised qualifier appended to an
	// agent's status when its docker is provably broken: "running" becomes
	// "running (docker degraded)".
	DockerDegradedQualifier = "docker degraded"

	// dockerServiceUnitName (the INSTALLER's per-user docker.service) is
	// declared in rootless_unitstart.go — the only consumer left that reads
	// it. The health surface deliberately does not consult it (DF-BUNKER-108).

	// dockerHealthProbeBudget bounds the whole probe (su + systemctl). The
	// info path must not be slowed by an unresponsive host: the probe is a
	// courtesy signal, never a source of truth that is worth waiting for.
	// The socket check that precedes it is a single stat.
	dockerHealthProbeBudget = 3 * time.Second

	// dockerStatusStarting is the transient pre-running status a spawn
	// record can carry while the agent comes up. Like `running` it claims
	// the agent is (becoming) live, so its docker ought to answer.
	dockerStatusStarting = "starting"
)

// managerDockerUnitName is the serving unit's name for agentID.
func managerDockerUnitName(agentID string) string {
	return managerDockerUnitPrefix + agentID
}

// managerDockerSocketPath is the socket path the spawn path provisions
// (manager_spawn.go: <spawnRunRoot>/<agentID>/docker.sock) and every agent
// session receives as DOCKER_HOST. spawnRunRoot is the package seam, so
// tests redirect it to a temp root.
func managerDockerSocketPath(agentID string) string {
	return filepath.Join(spawnRunRoot, agentID, dockerSocketName)
}

// dockerSocketServing reports whether path exists and IS a unix socket — the
// one proof that the agent's dockerd is answering. Anything else at that
// path (a missing file, a stale regular file a failed bind left behind) is
// not a serving daemon.
func dockerSocketServing(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSocket != 0
}

// dockerStatusServes reports whether a stored agent status claims the agent
// is up and therefore ought to have a serving docker. Terminal or already
// negative statuses (stopped, home_retained, failed, empty) never qualify:
// their missing socket is expected, and the qualifier must track a LOST
// docker on a live agent — not the absence that follows a stop or a retained
// home (DF-BUNKER-108).
func dockerStatusServes(status string) bool {
	switch status {
	case StatusRunning, dockerStatusStarting:
		return true
	}
	return false
}

// dockerUnitStateProbe asks the agent user's manager for the SERVING unit's
// state. Package-level seam: unit tests drive every branch (inactive /
// failed / active / unprovable) without a real su, a real systemd or a real
// agent user.
var dockerUnitStateProbe = probeDockerUserUnitState

// probeDockerUserUnitState is the production probe. `systemctl --user
// is-active` reports a NON-active state by PRINTING it and exiting non-zero,
// so the returned error is frequently the state's own exit status rather than
// a transport failure — the caller classifies on the output and treats the
// error only as "we got no usable answer" when the output is empty.
func probeDockerUserUnitState(ctx context.Context, username, runtimeDir, unitName string) (string, error) {
	out, err := userSessionRunner(ctx, username, runtimeDir, sessionCommand(runtimeDir, "systemctl --user is-active "+unitName))
	return strings.TrimSpace(string(out)), err
}

// classifyManagerUnitState maps the SERVING unit's answer onto a degraded
// verdict, returned with the classified state string.
//
// It reads the STATE STRING FIRST and the error second ON PURPOSE: for
// `failed` the error IS the state's own exit status, so an error-first
// reading would discard the one answer that matters. `inactive` and
// `failed` together with a missing socket are the only proofs that the
// agent's docker is gone; `active` against a missing socket is the
// not-yet-bound window; an empty or unrecognised body — a dead bus, a killed
// probe, `activating`, `unknown`, the unit-not-found text — yields NO
// verdict: absence is never upgraded past what the answer proves.
func classifyManagerUnitState(state string) (unitState string, degraded bool) {
	fields := strings.Fields(state)
	if len(fields) == 0 {
		return "", false
	}
	switch fields[0] {
	case dockerUnitFailed, dockerUnitInactive:
		return fields[0], true
	}
	return fields[0], false
}

// DockerDegradedDetail returns a degraded-status cause for agentID's docker,
// or "" when the agent's docker is serving, healthy, or not provably broken.
//
// The proof chain (DF-BUNKER-108): the per-agent socket answers → "" without
// consulting anything else; the socket is missing AND the serving unit
// (bunker-docker-<agentID>) reports inactive or failed → the cause names
// both. Every other path — an active unit against a missing socket, a
// probing error, a user record that no longer resolves (the orphan probe's
// state, DF-BUNKER-34) — returns "".
//
// Bounded and best-effort throughout: every failure path returns "".
func (m *AgentManager) DockerDegradedDetail(agentID string) string {
	sockPath := managerDockerSocketPath(agentID)
	if dockerSocketServing(sockPath) {
		// The agent's docker answers on its socket — healthy by the only
		// proof that matters. The installer's docker.service (failed on
		// DF-BUNKER-96 hosts by construction) is deliberately not asked.
		return ""
	}
	username := "bunker-" + agentID
	u, err := userLookup(username)
	if err != nil {
		return "" // no user on the host → orphan-probe territory, no docker verdict here
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return ""
	}
	unitName := managerDockerUnitName(agentID)
	ctx, cancel := context.WithTimeout(context.Background(), dockerHealthProbeBudget)
	defer cancel()
	runtimeDir := filepath.Join(userRuntimeBaseDir, strconv.Itoa(uid))
	state, probeErr := dockerUnitStateProbe(ctx, username, runtimeDir, unitName)
	unitState, degraded := classifyManagerUnitState(state)
	detail := ""
	if degraded {
		detail = fmt.Sprintf("%s %s, %s missing", unitName, unitState, sockPath)
	}
	switch {
	case detail != "" && m.logger != nil:
		// The operator-facing signal: this is the line that makes a broken
		// docker visible in the daemon log instead of only in the agent's own
		// journal.
		m.logger.Warn("agent reports a degraded docker daemon",
			"agent_id", agentID, "uid", uid, "unit", unitName,
			"detail", detail, "probe_output", state)
	case detail == "" && probeErr != nil && m.logger != nil:
		// No verdict AND no usable answer: say so at debug level rather than
		// pretending the probe succeeded.
		m.logger.Debug("docker health probe produced no verdict",
			"agent_id", agentID, "error", probeErr, "probe_output", state)
	}
	return detail
}

// DockerStatus qualifies an agent's status with its docker health: a serving
// status ("running"/"starting") for an agent whose socket is missing and
// whose bunker-docker-<id> unit is inactive/failed becomes
// "running (docker degraded)".
//
// The healthy path returns status BYTE-IDENTICAL (zero-delta), a status
// that does not claim a live agent is never qualified, and an empty status
// is never invented — a record with no status renders as itself rather than
// a fabricated "running".
func (m *AgentManager) DockerStatus(agentID, status string) string {
	if !dockerStatusServes(status) {
		return status
	}
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
