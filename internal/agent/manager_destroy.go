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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/deployBunker/bunker/internal/config"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// StatusHomeRetained is the destroy-path response status for a destroy that
// REFUSED to delete: the agent's home could not be archived and verified, so
// the Linux user, the home directory, the tracker record and the allocated
// port range were all kept (DF-BUNKER-33). Losing the home is the one
// unacceptable outcome; a retained agent can be retried or cleaned up later.
const StatusHomeRetained = "home_retained"

// StatusLiveProcesses is the destroy-path response status for a destroy that
// REFUSED to run userdel because the agent's uid still owns live processes
// (DF-BUNKER-34). Nothing was deleted: the user, the home, the tracker record
// and the port range all survive, and the error names every live process. The
// historical alternative was the partial state userdel -rf leaves when it
// fails on a busy home — user record gone, processes orphaned, the destroy
// reporting not_found (the cube-las-00 incident).
const StatusLiveProcesses = "live_processes"

// archiveAgentHome tars homeDir into archiveDir and VERIFIES the archive
// before the caller is allowed to delete anything. The verification is the
// whole point of the step (DF-BUNKER-33): an archive that does not exist,
// is zero bytes, or lists no real entries is treated as NO archive, and the
// caller must fail closed instead of running userdel -rf behind it.
func (m *AgentManager) archiveAgentHome(ctx context.Context, homeDir, archiveDir string) (string, error) {
	base := filepath.Base(homeDir)
	if err := os.MkdirAll(archiveDir, 0o700); err != nil {
		return "", fmt.Errorf("create archive dir %s: %w", archiveDir, err)
	}
	archivePath := filepath.Join(archiveDir, base+"-"+time.Now().UTC().Format("20060102T150405Z")+".tar.gz")
	if _, err := os.Stat(archivePath); err == nil {
		return "", fmt.Errorf("archive %s already exists (second destroy within the same second?); refusing to overwrite", archivePath)
	}
	cmd := exec.CommandContext(ctx, "tar", "czf", archivePath, "-C", filepath.Dir(homeDir), base)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("archive home %s: %w (output: %s)", homeDir, err, strings.TrimSpace(string(out)))
	}
	if err := verifyArchiveFile(archivePath); err != nil {
		return "", err
	}
	return archivePath, nil
}

// pruneArchiveDir bounds the archive directory's retention (INFRA-BACKUP-01).
// Only files matching the archiveAgentHome naming convention (*.tar.gz under
// archiveDir) are considered; anything else in the directory is never
// touched. keep > 0 keeps the newest keep archives (mtime descending,
// filename as the tiebreaker — the names embed the UTC timestamp) and
// deletes the rest. maxBytes > 0 additionally deletes OLDEST-first until the
// total size of the remaining archives is at or under the cap, never pruning
// down to zero (the single newest archive always survives — it is the one
// the destroy that just succeeded relies on). Removal errors are reported in
// the returned list/log, not aggravated: the caller treats pruning as
// best-effort — losing an old archive is acceptable, failing a destroy is
// not.
func (m *AgentManager) pruneArchiveDir(archiveDir string, keep int, maxBytes int64) (removed []string, err error) {
	if keep <= 0 && maxBytes <= 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type archive struct {
		path    string
		name    string
		size    int64
		modTime time.Time
	}
	var archives []archive
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, serr := e.Info()
		if serr != nil {
			m.logger.Warn("archive prune: stat failed (leaving file alone)",
				"dir", archiveDir, "file", e.Name(), "error", serr)
			continue
		}
		archives = append(archives, archive{
			path:    filepath.Join(archiveDir, e.Name()),
			name:    e.Name(),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
	}
	if len(archives) == 0 {
		return nil, nil
	}
	var remaining int64
	for _, a := range archives {
		remaining += a.size
	}
	// Newest first: mtime descending, filename as the deterministic
	// tiebreaker (archived names embed the UTC timestamp, so a name
	// descending order alone is already a reasonable approximation).
	sort.Slice(archives, func(i, j int) bool {
		if !archives[i].modTime.Equal(archives[j].modTime) {
			return archives[i].modTime.After(archives[j].modTime)
		}
		return archives[i].name > archives[j].name
	})
	remove := func(a archive) {
		if rerr := os.Remove(a.path); rerr != nil {
			// Best-effort: a file we cannot remove simply survives this
			// pass; report it in the log and the removal list so an
			// operator can see the accounting.
			m.logger.Warn("archive prune: remove failed (leaving file alone)",
				"dir", archiveDir, "file", a.name, "error", rerr)
		}
		remaining -= a.size
	}
	// Keep pass: delete everything beyond the newest `keep` archives.
	candidates := archives
	if keep > 0 {
		if keep < len(archives) {
			for _, a := range archives[keep:] {
				m.logger.Info("archive prune: removing old archive beyond keep limit",
					"dir", archiveDir, "file", a.name, "keep", keep)
				remove(a)
				removed = append(removed, a.name)
			}
		}
		candidates = archives[:min(keep, len(archives))]
	}
	// Size pass: still over the cap? Delete OLDEST-first (candidates is
	// newest-first, so the oldest is the last index) until under it,
	// always retaining at least the single newest archive (stop at index
	// 1 — index 0 is the newest and is never removed by the size pass).
	if maxBytes > 0 && len(candidates) > 1 {
		for i := len(candidates) - 1; i >= 1 && remaining > maxBytes; i-- {
			a := candidates[i]
			m.logger.Info("archive prune: removing old archive over size cap",
				"dir", archiveDir, "file", a.name, "max_bytes", maxBytes)
			remove(a)
			removed = append(removed, a.name)
		}
	}
	return removed, nil
}

// verifyArchiveFile is the DF-BUNKER-33 gate between "an archive file was
// produced" and "the home may be deleted": the file must exist, be
// non-empty, and `tar tzf` must list at least one entry. For GNU tar a
// directory argument always lists at least the archived directory itself
// (trailing slash preserved), so a literally empty listing means something
// is genuinely broken with the artifact; an empty HOME also produces a
// faithful root-only listing (nothing to lose) — it is the destroy path's
// non-empty probe, not this verifier, that routes empty homes away from
// archiving altogether.
func verifyArchiveFile(archivePath string) error {
	info, err := os.Stat(archivePath)
	if err != nil {
		return fmt.Errorf("verify archive %s: %w", archivePath, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("verify archive %s: archive is empty", archivePath)
	}
	list, err := exec.Command("tar", "tzf", archivePath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("verify archive %s: listing failed: %w (output: %s)", archivePath, err, strings.TrimSpace(string(list)))
	}
	entries := 0
	for _, line := range strings.Split(string(list), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && line != archivePath {
			entries++
		}
	}
	if entries == 0 {
		return fmt.Errorf("verify archive %s: archive lists no entries", archivePath)
	}
	return nil
}

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

	// Step 2b.1 (DF-BUNKER-34): verify the agent's uid is process-free before
	// anything destructive. waitAgentProcessesExit only SIGKILLs the dockerd
	// and rootlesskit pids its first scan saw; ANY other process still running
	// under the agent's uid (a service the agent's operator installed — a
	// scheduler daemon, a node server, a cron job) survives both the stop
	// stages and this reap, and userdel -rf then fails on the busy home while
	// userdel STILL removes the user record. That exact partial state is the
	// cube-las-00 incident: user record gone, ~35 uid processes alive for 20+
	// hours, one of them holding port 3000 and shadowing the next agent's
	// daemon, and the destroy reporting not_found while a "healthy" fleet kept
	// ticking against a deleted workdir. The gate fails LOUDLY instead: the
	// error names the uid, every live process and the remedy. Processes the
	// normal reap could not kill are evidence, not something to paper over.
	// FORCE MODE IS NOT AN EXCEPTION: --force means "tear the agent down even
	// though it is running", not "orphan every process it owns".
	if err := gateDestroyOnLiveProcesses(ctx, username, m.logger); err != nil {
		return &v1.DestroyAgentResponse{AgentId: agentID, Status: StatusLiveProcesses}, err
	}

	// Step 2c (INT-HOST-001): disable systemd linger so the per-agent linger
	// file goes away WITH the agent. spawn enables linger on every create and
	// no destroy path ever disabled it: every destroyed agent left
	// /var/lib/systemd/linger/<user> behind forever (8024 entries for 2 live
	// users on the demo host; user-manager starts starved host-wide). This
	// must run BEFORE userdel -rf (Step 3) because the username must still
	// resolve, and it is best-effort: a failure is logged (WARN) and the
	// destroy proceeds exactly as before.
	disableAgentLinger(ctx, username, m.logger)

	// Step 2.5 (DF-BUNKER-33): archive the agent home BEFORE userdel -rf.
	// The cube-las-00 incident: a scheduled renewal destroyed an agent whose
	// home held an unmerged product repo (4 git worktrees), .ssh and host
	// tooling, and `userdel -rf` deleted all of it with no copy anywhere.
	// DEFAULT policy "archive": tar the home, verify the archive (exists,
	// non-empty, lists real entries), and only then allow the delete.
	// "purge" preserves the historical userdel-only behavior. An archive or
	// verification failure is FAIL-CLOSED: the user, home, tracker record
	// and port range all survive and the response status is home_retained —
	// losing the home is the one unacceptable outcome.
	homeDir := filepath.Join(agentHomeRoot, username)
	homeExists := false
	if st, serr := os.Stat(homeDir); serr == nil && st.IsDir() {
		if entries, derr := os.ReadDir(homeDir); derr == nil && len(entries) > 0 {
			homeExists = true
		}
	}
	if homeExists && m.cfg.Agent.DestroyHomePolicyOrDefault() == config.DestroyPolicyArchive {
		archiveDir := m.cfg.Agent.DestroyArchiveDirOrDefault()
		m.logger.Info("archiving agent home before delete",
			"agent_id", agentID,
			"policy", config.DestroyPolicyArchive,
			"home", homeDir,
			"archive_dir", archiveDir)
		archivePath, aerr := m.archiveAgentHome(ctx, homeDir, archiveDir)
		if aerr != nil {
			m.logger.Error("agent home archive failed; home RETAINED, userdel NOT run",
				"agent_id", agentID,
				"policy", config.DestroyPolicyArchive,
				"home", homeDir,
				"archive_dir", archiveDir,
				"error", aerr)
			return &v1.DestroyAgentResponse{AgentId: agentID, Status: StatusHomeRetained},
				fmt.Errorf("destroy aborted: agent home %s could not be archived to %s: %w (home retained, nothing deleted)", homeDir, archiveDir, aerr)
		}
		m.logger.Info("agent home archived and verified",
			"agent_id", agentID,
			"policy", config.DestroyPolicyArchive,
			"archive_path", archivePath)
		// INFRA-BACKUP-01: with the archive VERIFIED and this destroy on
		// the success path, prune old archives so the archive dir has real
		// retention. Best-effort: a prune failure is logged, never allowed
		// to fail a destroy that already succeeded — losing an old archive
		// is acceptable, failing a destroy is not. keep=0 (explicit
		// opt-out) and maxBytes=0 disable their respective passes.
		if removed, perr := m.pruneArchiveDir(archiveDir,
			m.cfg.Agent.DestroyArchiveKeepOrZero(),
			m.cfg.Agent.DestroyArchiveMaxBytes); perr != nil {
			m.logger.Warn("archive prune failed (archive dir may grow unbounded)",
				"archive_dir", archiveDir, "error", perr)
		} else if len(removed) > 0 {
			m.logger.Info("archive prune removed old archives",
				"archive_dir", archiveDir, "removed", len(removed))
		}
	} else if homeExists {
		m.logger.Info("destroy_home_policy purge: deleting agent home WITHOUT archiving",
			"agent_id", agentID,
			"policy", config.DestroyPolicyPurge,
			"home", homeDir)
	}

	// Step 3: Remove the Linux user
	cmd := exec.CommandContext(ctx, "userdel", "-rf", username)
	if out, err := cmd.CombinedOutput(); err != nil {
		// DF-BUNKER-34: a userdel failure that is NOT "the user is already
		// gone" means the host is in exactly the partial state this row
		// exists to prevent — userdel -rf fails on a busy home (processes
		// still hold it) and may have removed the user record while the
		// directory and every process under the uid SURVIVE. That state must
		// surface as a hard error carrying the surviving-process evidence,
		// not as a not_found (the cube-las-00 shape: a "healthy" fleet for
		// 20+ hours ticking against a deleted workdir). The check is
		// evidence-based, not string-matched: re-probe the uid for live
		// processes; anything found rides the error.
		if !destroyUserAbsentOutput(out) {
			evidence := m.destroyFailureEvidence(username)
			if force {
				// Force mode keeps its historical continue-on-failure
				// semantics, but the failure is now LOUD on the record: the
				// surviving-process evidence is logged at Error, never
				// swallowed.
				m.logger.Error("userdel failed in force mode; agent state is partially removed",
					"username", username, "error", err, "output", string(out), "evidence", evidence)
			} else {
				m.logger.Error("userdel failed; destroy aborted with evidence",
					"username", username, "error", err, "output", string(out), "evidence", evidence)
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
				if perr := m.persistDestroy(agentID); perr != nil {
					m.logger.Warn("registry destroy append failed", "agent_id", agentID, "error", perr)
				}
				m.removeAgentSSHKeyBestEffort(agentID, m.logger)
				return &v1.DestroyAgentResponse{AgentId: agentID, Status: StatusUserdelFailed},
					fmt.Errorf("destroy of %s failed: userdel error: %v (output: %s). %s",
						agentID, err, strings.TrimSpace(string(out)), evidence)
			}
		} else if !force {
			// The historical "user already gone" path (idempotent destroy or
			// not_found) — unchanged.
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
				// DF-BUNKER-24: this branch returns BEFORE Step 4.5, so the
				// persisted key has to be removed here too. It is the branch
				// the TTL reaper lands on whenever the agent's system user is
				// already gone (post-restart reap), and skipping it left
				// cfg.Agent.SSHDir/<id> behind forever.
				m.removeAgentSSHKeyBestEffort(agentID, m.logger)
				m.logger.Info("agent already absent; destroy succeeded idempotently",
					"agent_id", agentID, "username", username)
				return &v1.DestroyAgentResponse{AgentId: agentID, Status: "destroyed"}, nil
			}
			// Raw userdel output stays in the server log for diagnostics;
			// the user-facing error must stay clean so the CLI can present
			// a tidy "agent not found" without leaking command output.
			m.logger.Warn("userdel failed, treating agent as not found", "username", username, "error", err, "output", string(out))
			// DF-BUNKER-24: a never-seen ID with a persisted key means the
			// registry is unavailable (or compacted) while the host state is
			// already gone — the reported not_found must still not leave a
			// credential under cfg.Agent.SSHDir/<id> for the next agent that
			// reuses the id. Removal is scoped by removeAgentSSHKey.
			m.removeAgentSSHKeyBestEffort(agentID, m.logger)
			return &v1.DestroyAgentResponse{AgentId: agentID, Status: "not_found"},
				fmt.Errorf("agent %q not found", agentID)
		} else {
			// Force mode, user-absent class: historical behavior.
			m.logger.Warn("userdel failed in force mode (user absent)", "username", username, "error", err, "output", string(out))
		}
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

	// Step 4.5: Clean up persisted SSH key (DF-BUNKER-24: the same helper is
	// also used by the two early returns above and by the reconcile purge, so
	// every path that concludes the agent is gone converges on the same state
	// through one scoped implementation).
	m.removeAgentSSHKeyBestEffort(agentID, m.logger)

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

// gateDestroyOnLiveProcesses verifies the agent user's uid owns NO live
// process before destroy may run userdel -rf (DF-BUNKER-34, criterion 2).
// It runs AFTER the dockerd/rootlesskit reap, so the processes it finds are
// exactly the ones the normal teardown could not kill — long-lived user
// services the agent's operator installed (a scheduler daemon, a node
// server, a forward script), which survive SIGTERM/SIGKILL rounds aimed at
// the docker stack and keep the home busy.
//
// The refusal is loud and complete: the error names the username, the uid,
// every live process (pid + command head) and the remedy. No deletion has
// happened when it fires — the caller returns StatusLiveProcesses with the
// user, home, tracker record and port range intact.
//
// The probe is seam-isolated (destroyLiveProcessProbe) so a unit test drives
// the refusal without touching the host's /proc, and a user that no longer
// resolves skips the gate entirely (the idempotent-destroy path owns that
// case; there is no uid to check for an already-gone user).
func gateDestroyOnLiveProcesses(_ context.Context, username string, logger *slog.Logger) error {
	procs, uid, ok, err := destroyProcessProbe(username)
	if err != nil {
		// The probe could not run (e.g. /proc unreadable). "Cannot look" must
		// never read as "nothing there" — but it also must not wedge every
		// destroy on an environment without /proc: the gate REFUSES, and the
		// refusal names the probe failure so the operator knows the destroy
		// was blocked by an unobservable host, not by live processes.
		return fmt.Errorf("destroy refused: cannot verify that user %s owns no live processes: %v", username, err)
	}
	if !ok {
		return nil // user record already gone: no uid to check
	}
	if len(procs) == 0 {
		return nil
	}
	summary := (&orphanUIDCheck{UID: uid, UIDKnown: true, UserExists: true, Processes: procs}).Describe()
	logger.Error("destroy refused: agent uid still owns live processes",
		"username", username,
		"uid", uid,
		"processes", len(procs),
		"evidence", summary)
	return fmt.Errorf("%s", buildDestroyLiveProcessRefusal(username, uid, procs))
}

// destroyProcessProbe is the seam behind the gate: it returns the live
// processes owned by the username's uid, the uid, ok=true when the user
// record resolved, and the probe error. Production resolves through the
// lookupUser seam and reads /proc; tests inject fakes.
var destroyProcessProbe = func(username string) (procs []userProcess, uid uint32, ok bool, err error) {
	u, uerr := lookupUser(username)
	if uerr != nil {
		return nil, 0, false, nil // user gone: the idempotent path, not this gate's case
	}
	v, perr := strconv.ParseUint(u.Uid, 10, 32)
	if perr != nil {
		return nil, 0, false, fmt.Errorf("parse uid of %s: %w", username, perr)
	}
	procs, lerr := listUserProcesses(uint32(v))
	if lerr != nil {
		return nil, 0, false, lerr
	}
	return procs, uint32(v), true, nil
}

// buildDestroyLiveProcessRefusal renders the operator-facing refusal for a
// uid that still owns live processes. It is the single construction for the
// gate error and the orphan report line, so the two surfaces cannot drift.
func buildDestroyLiveProcessRefusal(username string, uid uint32, procs []userProcess) string {
	summary := (&orphanUIDCheck{UID: uid, UIDKnown: true, UserExists: true, Processes: procs}).Describe()
	return fmt.Sprintf(
		"destroy refused: user %s (uid %d) still owns live processes that userdel -rf would orphan. %s. "+
			"Stop those processes on the host (they are NOT killed by bunker destroy — a previous destroy "+
			"that orphaned them is exactly the failure this gate exists to prevent), then retry the destroy",
		username, uid, summary)
}

// StatusUserdelFailed is the destroy-path response status for a userdel -rf
// failure that is NOT the idempotent "user already gone" class (DF-BUNKER-34):
// the host is left in the partial state userdel produces when processes still
// hold the home — the user record may be gone while the directory and every
// uid process survive. The destroy reports it as a hard error carrying the
// surviving-process evidence instead of the historical silent not_found.
const StatusUserdelFailed = "userdel_failed"

// destroyUserAbsentOutput classifies userdel output as the benign
// "user does not exist" class. It is deliberately conservative: anything the
// classifier cannot PROVE is the absent-user case is treated as a real
// failure, because the historical misclassification (every failure read as
// not_found) is the bug. The accepted wordings cover the two shapes userdel
// prints for a missing account (name-based and uid-based); everything else —
// "error removing directory", "user ... is currently used by process",
// permission failures — falls through to the hard-error branch.
func destroyUserAbsentOutput(out []byte) bool {
	o := strings.ToLower(string(out))
	for _, sig := range []string{
		"does not exist",
		"no such user",
		"not found",
	} {
		if strings.Contains(o, sig) {
			return true
		}
	}
	return false
}

// destroyFailureEvidence probes the host state a failed userdel leaves behind
// and renders it as operator-facing evidence: whether the user record is
// still present, whether the uid still owns live processes, and whether the
// home still exists. It is evidence for the destroy error / force-mode log —
// read-only, best-effort, and honest about the parts it could not observe.
func (m *AgentManager) destroyFailureEvidence(username string) string {
	var parts []string
	_, lerr := lookupUser(username)
	switch {
	case lerr == nil:
		parts = append(parts, "user record still present")
	default:
		parts = append(parts, "user record REMOVED from the host while destroy failed")
	}
	if uid, ok := resolveUsernameUID(username); ok {
		procs, perr := listUserProcesses(uid)
		switch {
		case perr != nil:
			parts = append(parts, "process state unobservable: "+perr.Error())
		case len(procs) > 0:
			summary := (&orphanUIDCheck{UID: uid, UIDKnown: true, UserExists: lerr == nil, Processes: procs}).Describe()
			parts = append(parts, summary)
		default:
			parts = append(parts, fmt.Sprintf("no live processes remain under uid %d", uid))
		}
	} else if lerr == nil {
		parts = append(parts, "uid of "+username+" unresolvable")
	}
	home := filepath.Join(agentHomeRoot, username)
	if _, serr := os.Stat(home); serr == nil {
		parts = append(parts, "home directory "+home+" still exists")
	} else {
		parts = append(parts, "home directory "+home+" is gone")
	}
	return "surviving state: " + strings.Join(parts, "; ")
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
