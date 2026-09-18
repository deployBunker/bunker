// Package agent manages agent lifecycle: create users, generate SSH keys, start dockerd.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// disableUserUnit runs `systemctl --user disable <unit>` and returns its
// combined output. Package-level seam: tests inject fake systemctl results
// so the destroy path is exercised without touching ambient host state;
// production code never swaps it.
var disableUserUnit = func(ctx context.Context, unit string) ([]byte, error) {
	return exec.CommandContext(ctx, "systemctl", "--user", "disable", unit).CombinedOutput()
}

// normalizeUnitOutput lowercases systemctl output and strips `$` characters
// so needles written against plain variable names match both the `$FOO` and
// bare `FOO` spellings, in whatever case systemd prints them (DF-BUNKER-5
// attempt 1 matched a test paraphrase instead of this output class).
func normalizeUnitOutput(out []byte) string {
	return strings.ReplaceAll(strings.ToLower(string(out)), "$", "")
}

// noUserManagerFailure reports whether a failed `systemctl --user disable`
// is the well-known "this caller has no user session bus" class, which is
// non-actionable for a transient per-agent unit whose user is removed in
// the next step of Destroy.
//
// Matching is on normalised output (see normalizeUnitOutput) and on the
// environment-variable NAMES alone where possible, so the class stays
// robust across systemd versions rewording the sentence around them.
// Verbatim variants observed live (DF-BUNKER-5):
//
//	A (bunker-las-01): Failed to connect to user scope bus via local
//	  transport: $DBUS_SESSION_BUS_ADDRESS and $XDG_RUNTIME_DIR not
//	  defined (consider using --machine=<user>@.host --user ...)
//	B (bunker-mvp):    Failed to connect to bus: No medium found
//	C (dogfood note):  Failed to connect to bus: DBUS_SESSION_BUS_ADDRESS
//	  and XDG_RUNTIME_DIR not defined
func noUserManagerFailure(out []byte) bool {
	o := normalizeUnitOutput(out)
	for _, sig := range []string{
		"dbus_session_bus_address",
		"xdg_runtime_dir",
		"failed to connect to user scope bus",
		"failed to connect to bus",
		"no medium found",
		"has not been booted with systemd",
	} {
		if strings.Contains(o, sig) {
			return true
		}
	}
	return false
}

// unitAbsenceFailure reports whether a failed disable is just systemd
// reporting the unit as absent from the user manager. The per-agent unit is
// the agent's `systemd-run --user` TRANSIENT unit, so it is never "enabled"
// anywhere — "nothing to disable" is the same non-actionable outcome as the
// no-bus class (reproduced live on a destroy whose daemon DID have a bus).
func unitAbsenceFailure(out []byte) bool {
	o := normalizeUnitOutput(out)
	for _, sig := range []string{
		"does not exist",
		"not loaded",
		"is transient or generated",
	} {
		if strings.Contains(o, sig) {
			return true
		}
	}
	return false
}

// lookupUser resolves a username through the system user database. Package-
// level seam: tests inject a fake so destroy never inspects the ambient
// /etc/passwd; production code never swaps it.
var lookupUser = func(username string) (*user.User, error) {
	return user.Lookup(username)
}

// disableLinger runs `loginctl disable-linger <username>` and returns its
// combined output. Package-level seam (mirrors disableUserUnit above): tests
// inject a fake loginctl so the destroy path is exercised without touching
// the host's systemd state; production code never swaps it.
var disableLinger = func(ctx context.Context, username string) ([]byte, error) {
	return exec.CommandContext(ctx, "loginctl", "disable-linger", username).CombinedOutput()
}

// disableAgentLinger runs `loginctl disable-linger <username>` and returns
// whether the call was attempted. It is skipped when username is empty or the
// user is already gone — there is no linger entry to disable for a user that
// does not resolve, and loginctl would only report the absence. Callers treat
// every outcome as best-effort: an error is logged (WARN) and the destroy
// proceeds exactly as before (INT-HOST-001).
func disableAgentLinger(ctx context.Context, username string, logger *slog.Logger) bool {
	if username == "" {
		return false
	}
	if _, err := lookupUser(username); err != nil {
		logger.Debug("skipping linger disable: user absent",
			"username", username, "error", err)
		return false
	}
	out, err := disableLinger(ctx, username)
	if err != nil {
		logger.Warn("loginctl disable-linger failed (continuing destroy)",
			"username", username, "error", err, "output", string(out))
		return true
	}
	logger.Debug("disabled linger for agent user", "username", username)
	return true
}

func (m *AgentManager) Destroy(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error) {
	// Step 0: validate agent_id
	if agentID == "" || !validAgentID.MatchString(agentID) {
		// Free is unconditional and idempotent (no-ops for IDs that never
		// held a range); calling it on every early return keeps the
		// "no destroy path leaks a port range" invariant structural
		// (QA-BUNKER-4).
		if m.portAlloc != nil {
			m.portAlloc.Free(agentID)
		}
		return &v1.DestroyAgentResponse{AgentId: agentID, Status: "error"},
			fmt.Errorf("invalid agent_id %q", agentID)
	}

	m.logger.Info("destroying agent", "agent_id", agentID)

	// Step 0.4: Stop and remove the agent's own container through ONLY that
	// agent's rootless socket — BEFORE the dockerd stop below, so no
	// container leaks past the daemon (specs/container-mode.md §4). Best-
	// effort: a daemon that already died has nothing to clean up.
	if err := cleanupAgentContainers(ctx, agentID, force, m.logger); err != nil {
		m.logger.Warn("agent container cleanup incomplete", "agent_id", agentID, "error", err)
	}

	// Step 0.5: Remove user slice cgroup drop-in so stale limits don't
	// accumulate after the agent is destroyed.
	removeUserSliceLimits(ctx, agentID, m.logger)

	// Step 0.6 (GAP-075): unmount and remove the bounded shared-scratch
	// directory and the private-/tmp instance directory. Idempotent, so a
	// partially provisioned agent still destroys cleanly. Best-effort here
	// (the spawn rollback is the caller that records the outcome).
	if err := m.removeIsolation(ctx, agentID); err != nil {
		m.logger.Warn("isolation removal incomplete", "agent_id", agentID, "error", err)
	}

	// Step 1: Stop the dockerd systemd user unit
	unitName := "bunker-docker-" + agentID
	username := "bunker-" + agentID

	// Look up the UID before userdel so we can clean up the actual rootless socket
	// created under /run/user/<uid>.
	var uid string
	userPresent := true
	if u, err := lookupUser(username); err == nil {
		uid = u.Uid
	} else {
		userPresent = false
		m.logger.Warn("cannot lookup user before destroy", "username", username, "error", err)
	}

	// The dockerd unit was started via systemd-run --user, so it runs under
	// the agent's user session. systemctl --user from the root foreman session
	// targets the wrong user manager. We must either:
	//   (a) use systemctl --user --machine=<user>@.host, or
	//   (b) find the dockerd PID and kill it directly.
	// Option (b) is simpler and avoids DBus/machined dependencies.
	if err := stopDockerdDirect(ctx, username, unitName, m.logger); err != nil {
		m.logger.Warn("direct dockerd stop failed", "unit", unitName, "error", err)
		// Fallback: try systemctl --user (may work if user linger is enabled)
		cmd := exec.CommandContext(ctx, "systemctl", "--user", "stop", unitName)
		if out, err := cmd.CombinedOutput(); err != nil {
			m.logger.Warn("systemctl stop failed (may not exist)", "unit", unitName, "error", err, "output", string(out))
		}
	}

	// Step 2: Disable the unit (prevent auto-restart). The unit is the
	// agent's `systemd-run --user` TRANSIENT unit: when the caller has no
	// user session bus (the normal case for the root bunkerd daemon), the
	// disable exits non-zero and the transient unit is destroyed together
	// with the agent's user manager anyway — the very next steps remove
	// the Linux user (userdel -rf below) and the socket. Both known
	// non-actionable outcomes — no user session bus, and the unit being
	// absent/not-loaded/transient from the caller's manager — are logged
	// at Debug with the raw output preserved; a genuine disable failure
	// (permission denied, operation not permitted, anything unrecognised)
	// still Warns.
	if out, err := disableUserUnit(ctx, unitName); err != nil {
		switch {
		case noUserManagerFailure(out):
			m.logger.Debug("agent user manager unreachable; transient unit disable skipped",
				"unit", unitName, "reason", "no user session bus",
				"error", err, "output", string(out))
		case unitAbsenceFailure(out):
			m.logger.Debug("transient unit absent from user manager; disable skipped",
				"unit", unitName, "reason", "unit absent, not loaded, or transient",
				"error", err, "output", string(out))
		default:
			m.logger.Warn("systemctl disable failed", "unit", unitName, "error", err, "output", string(out))
		}
	}

	// Step 2b: Wait for the agent's rootless processes to actually exit.
	// userdel -rf refuses to remove a user that still owns running processes,
	// and stopDockerdDirect only SIGKILLs the pids from its first scan —
	// stragglers (an orphaned dockerd reparented after rootlesskit died, or
	// respawned children) can still be alive here. Waiting keeps the non-force
	// path below from treating a slow-shutdown agent as not_found.
	waitAgentProcessesExit(ctx, username, m.logger)

	// Step 2c (INT-HOST-001): disable systemd linger so the per-agent linger
	// file goes away WITH the agent. spawn enables linger on every create and
	// no destroy path ever disabled it: every destroyed agent left
	// /var/lib/systemd/linger/<user> behind forever (8024 entries for 2 live
	// users on the demo host; user-manager starts starved host-wide). This
	// must run BEFORE userdel -rf (Step 3) because the username must still
	// resolve, and it is best-effort: a failure is logged (WARN) and the
	// destroy proceeds exactly as before.
	disableAgentLinger(ctx, username, m.logger)

	// Step 3: Remove the Linux user
	cmd := exec.CommandContext(ctx, "userdel", "-rf", username)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Check if user doesn't exist (already destroyed)
		if !force {
			// Free the port range first — the in-memory allocator leaks
			// permanently if a destroy path returns without releasing it.
			// The TTL reaper hit this on bunker-las-03: userdel failed
			// against a still-running rootless dockerd, the tracker slot
			// was freed, and the range stayed allocated until the whole
			// pool was exhausted. Free is unconditional and idempotent —
			// it no-ops for IDs with no allocated range.
			if m.portAlloc != nil {
				m.portAlloc.Free(agentID)
				m.logger.Info("freed port range", "agent_id", agentID)
			}
			m.tracker.Unregister(agentID)

			// GAP-070 idempotent destroy: the system user is gone AND the
			// durable lifecycle store already knew this agent, so this is a
			// repeat destroy (TTL reaper retry, CLI retry, reconcile
			// cleanup) and it succeeds. A never-seen ID still reports
			// not_found below, which is what makes the two cases
			// distinguishable after a restart or a compaction.
			if !userPresent && m.knownAgent(agentID) {
				if perr := m.persistDestroy(agentID); perr != nil {
					m.logger.Warn("registry destroy append failed", "agent_id", agentID, "error", perr)
				}
				m.logger.Info("agent already absent; destroy succeeded idempotently",
					"agent_id", agentID, "username", username)
				return &v1.DestroyAgentResponse{AgentId: agentID, Status: "destroyed"}, nil
			}
			// Raw userdel output stays in the server log for diagnostics;
			// the user-facing error must stay clean so the CLI can present
			// a tidy "agent not found" without leaking command output.
			m.logger.Warn("userdel failed, treating agent as not found", "username", username, "error", err, "output", string(out))
			return &v1.DestroyAgentResponse{AgentId: agentID, Status: "not_found"},
				fmt.Errorf("agent %q not found", agentID)
		}
		// Force mode: log and continue even if userdel fails
		m.logger.Warn("userdel failed in force mode", "username", username, "error", err, "output", string(out))
	}

	// Step 4: Clean up /run/bunker/<id>/ directory
	runDir := fmt.Sprintf("/run/bunker/%s", agentID)
	if err := os.RemoveAll(runDir); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("failed to remove run dir", "dir", runDir, "error", err)
	}

	// Step 4a: Clean up the actual rootless socket under /run/user/<uid>. A stale
	// socket here would prevent the next agent that reuses this UID from binding.
	if uid != "" {
		actualSock := fmt.Sprintf("/run/user/%s/docker.sock", uid)
		if err := os.Remove(actualSock); err != nil && !os.IsNotExist(err) {
			m.logger.Warn("failed to remove actual docker socket", "path", actualSock, "error", err)
		}
	}

	// Step 4.5: Clean up persisted SSH key
	sshKeyPath := filepath.Join(m.cfg.Agent.SSHDir, agentID)
	if err := os.Remove(sshKeyPath); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("failed to remove ssh key", "path", sshKeyPath, "error", err)
	}

	if m.tunnelMgr != nil {
		if err := m.tunnelMgr.Stop(agentID); err != nil {
			m.logger.Warn("tunnel stop failed", "agent_id", agentID, "error", err)
		}
	}

	if m.tailscaleMgr != nil {
		if err := m.tailscaleMgr.Stop(agentID); err != nil {
			m.logger.Warn("tailscale stop failed", "agent_id", agentID, "error", err)
		}
	}

	m.tracker.Unregister(agentID)

	if m.portAlloc != nil {
		m.portAlloc.Free(agentID)
		m.logger.Info("freed port range", "agent_id", agentID)
	}

	// GAP-070: the agent is gone from the host — record that durably so a
	// restart does not resurrect it, and so a repeated destroy of the same
	// ID stays idempotent. The destroy itself already succeeded, so a failed
	// append is logged rather than failing the caller (the next replay would
	// otherwise report a live agent that no longer exists, which
	// reconciliation then purges).
	if err := m.persistDestroy(agentID); err != nil {
		m.logger.Warn("registry destroy append failed", "agent_id", agentID, "error", err)
	}

	m.logger.Info("agent destroyed", "agent_id", agentID)
	return &v1.DestroyAgentResponse{AgentId: agentID, Status: "destroyed"}, nil
}

// waitAgentProcessesExit polls until the agent user owns no rootlesskit or
// dockerd processes (up to ~10s), SIGKILLing stragglers as they are found.
// userdel -rf refuses to remove a user that still owns running processes, so
// destroy must not attempt userdel while rootless docker processes linger —
// a non-force failure there reports not_found, which used to strand the
// agent's port range until the whole pool was exhausted (QA-BUNKER-4).
// pgrep exits non-zero when nothing matches, so an unknown or already-deleted
// user returns immediately.
func waitAgentProcessesExit(ctx context.Context, username string, logger *slog.Logger) {
	const pollInterval = 200 * time.Millisecond
	deadline := time.Now().Add(10 * time.Second)
	for {
		// pgrep -f treats the pattern as an extended regex: match the
		// dockerd daemon and its rootlesskit supervisor.
		cmd := exec.CommandContext(ctx, "pgrep", "-u", username, "-f", "dockerd|rootlesskit")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return // no matching processes — safe to userdel
		}
		pids := strings.Fields(string(out))
		if len(pids) == 0 {
			return
		}
		if time.Now().After(deadline) {
			logger.Warn("agent processes still running after 10s grace; proceeding to userdel",
				"user", username, "pids", strings.Join(pids, ","))
			return
		}
		for _, pid := range pids {
			// Anything still alive here already survived stopDockerdDirect's
			// SIGTERM and 5s grace, so escalate immediately.
			logger.Info("killing lingering agent process", "user", username, "pid", pid)
			_ = exec.CommandContext(ctx, "kill", "-KILL", pid).Run()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}
}
