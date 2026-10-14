package agent

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
)

// ── INT-CI-069: the installer's docker.service unit-START failure class ─────
//
// CI run 38542837944 (regression job, cell "spawn auto-generates ID"): the
// official rootless installer downloaded, wrote
// ~/.config/systemd/user/docker.service, and then its own
//
//	systemctl --user start docker.service
//
// failed with
//
//	Job for docker.service failed because the control process exited with
//	error code.
//
// and the installer exited 1 ("Failed to start docker.service. Run
// `journalctl -n 20 --no-pager --user --unit docker.service` to show the error
// log." / "Before retrying installation, you might need to uninstall the
// current setup: `.../dockerd-rootless-setuptool.sh uninstall -f ; .../rootlesskit
// rm -rf ~/.local/share/docker`").
//
// This is a DIFFERENT failure class from the unit-not-found race the
// daemon-reload retry in installRootlessDocker covers (INT-CI-009): here the
// manager SAW the freshly written unit and the start JOB itself failed — the
// docker daemon never came up. A daemon-reload cannot help, and neither can a
// bare installer re-run: the failed unit sits in systemd's `failed` state
// (systemd refuses to start a failed unit until `systemctl --user
// reset-failed`), and the partial setup (binaries, unit file, data dir) is
// still in place. The setup tool's own error message prescribes the recovery:
// uninstall the current setup, then retry.
//
// The recovery implemented here is therefore, in order:
//  1. `systemctl --user reset-failed docker.service` (ONLY when the unit
//     actually reports `failed` — never on an unprovable state), retried
//     within a tiny bounded loop because a just-failed unit can still be
//     settling;
//  2. the setup tool's own `uninstall -f`;
//  3. best-effort removal of the daemon data dir the setup tool names
//     (its error text prescribes it; a failure there only logs).
// Then the caller retries the installer ONCE. A second failure is fatal and
// carries the docker.service JOURNAL excerpt: the job error ("control process
// exited with error code") names the symptom, never the cause — only the
// unit's journal does (e.g. the rootlesskit lock error DF-BUNKER-72
// documented, or a missing fusermount3/newuidmap). Never a silent skip.
//
// Bounds: 2 reset attempts with a bounded pause between, one uninstall, one
// journal query — all far inside the spawn budget.

// rootlessSetupResetAttempts bounds the reset-failed convergence loop. A
// start job that just failed can take a moment to settle into the terminal
// `failed` state the reset requires; two attempts cover it. Bounded on
// purpose — never an unbounded retry.
const rootlessSetupResetAttempts = 2

// rootlessSetupResetPause is the pause between reset attempts.
const rootlessSetupResetPause = 500 * time.Millisecond

// rootlessUninstallCmdFmt is the setup tool's uninstall as the agent session
// must run it. The setup tool lives beside the installer in the agent's bin
// directory (%s = absolute path). `-f` matches the recovery command the
// installer's own error message prescribes.
const rootlessUninstallCmdFmt = "%s uninstall -f"

// rootlessDataDirRel is the daemon data directory the setup tool's recovery
// message prescribes removing before a retry, relative to the agent home
// (the installer path's parent directory).
const rootlessDataDirRel = ".local/share/docker"

// dockerUnitResetCmdFmt is the unit-start recovery's first step: clear the
// failed state so systemd will start the unit again. Never run blind — the
// caller probes first and only resets a provably `failed` unit.
const dockerUnitResetCmdFmt = "systemctl --user reset-failed " + dockerServiceUnitName

// isDockerUnitStartFailure reports whether the installer output carries the
// unit-START failure signature INT-CI-069 observed: the installer reached its
// `systemctl --user start docker.service` and the JOB failed
// ("Job for docker.service failed because the control process exited with
// error code", followed by the setup tool's failed-start/uninstall advice).
// Case-insensitive like isUserUnitNotFound, and deliberately strict: only the
// job-failure wording TOGETHER with the installer's own start line triggers
// the uninstall-and-retry, so unrelated installer failures (network, download,
// disk) never trigger it.
func isDockerUnitStartFailure(out []byte) bool {
	lower := strings.ToLower(string(out))
	jobFailed := strings.Contains(lower, "job for docker.service failed")
	startedSeen := strings.Contains(lower, "systemctl --user start docker.service")
	return jobFailed && startedSeen
}

// resetPartialRootlessSetup un-sticks and un-installs the partial rootless
// setup a failed docker.service start leaves behind, so the caller's single
// installer retry starts from the same state a fresh install would. Steps are
// in the setup tool's own prescribed order (reset the failed unit, uninstall,
// clear the data dir); each runs through the SAME session seam the installer
// uses (userSessionRunner + sessionCommand), so the bus environment is
// delivered in-band exactly as the installer receives it (INT-SPAWN-004).
//
// Error contract: the reset is REQUIRED (a failed unit cannot be retried
// through), so a reset that does not converge returns an error naming the
// fail_detail (unit, observed state, reset command, last reset output). The
// uninstall returns its error too (a retry over a half-uninstalled setup is
// exactly the half-written state this step exists to remove). The data-dir rm
// is best-effort: the setup tool re-creates the dir on install, so a failed
// rm only logs.
func resetPartialRootlessSetup(ctx context.Context, username, runtimeDir, installerPath string, logger *slog.Logger) error {
	unitState, probeErr := dockerUnitStateProbe(ctx, username, runtimeDir)
	if classifyDockerUnitState(unitState, probeErr) != dockerFailedDetail {
		// Not provably failed (already inactive/gone, or the probe could
		// not read it): there is no failed state to reset — proceed to
		// the uninstall. The probe's answer is recorded in the log for
		// attribution, never guessed at.
		if logger != nil {
			logger.Info("docker.service not in a failed state before the unit-start retry; skipping the reset",
				"user", username, "unit", dockerServiceUnitName,
				"probe_output", unitState, "probe_error", fmt.Sprint(probeErr))
		}
	} else {
		resetCmd := dockerUnitResetCmdFmt
		var lastOut []byte
		var lastErr error
		for attempt := 1; attempt <= rootlessSetupResetAttempts; attempt++ {
			if attempt > 1 {
				select {
				case <-ctx.Done():
					return fmt.Errorf("reset of failed %s for %s cancelled: %w", dockerServiceUnitName, username, ctx.Err())
				case <-time.After(rootlessSetupResetPause):
				}
			}
			lastOut, lastErr = userSessionRunner(ctx, username, runtimeDir, sessionCommand(runtimeDir, resetCmd))
			if lastErr == nil {
				break
			}
		}
		if lastErr != nil {
			// Named fail_detail: which unit, which state, what was run,
			// what it printed. Never a silent skip — a retry over a unit
			// still in `failed` would deterministically fail the same way.
			return fmt.Errorf("systemctl --user reset-failed %s did not converge for %s (observed state: %q; attempts: %d; output: %s)",
				dockerServiceUnitName, username, unitState, rootlessSetupResetAttempts, condenseJournal(string(lastOut)))
		}
	}

	uninstallCmd := fmt.Sprintf(rootlessUninstallCmdFmt, setupToolPathFor(installerPath))
	if out, err := userSessionRunner(ctx, username, runtimeDir, sessionCommand(runtimeDir, uninstallCmd)); err != nil {
		return fmt.Errorf("rootless setup uninstall before the installer retry failed for %s: %w (command: %s; output: %s)",
			username, err, uninstallCmd, condenseJournal(string(out)))
	}

	dataDir := filepath.Join(filepath.Dir(installerPath), rootlessDataDirRel)
	rmCmd := "rm -rf " + shellQuote(dataDir)
	if out, err := userSessionRunner(ctx, username, runtimeDir, sessionCommand(runtimeDir, rmCmd)); err != nil {
		// Best-effort by contract: the installer recreates the data dir,
		// so a failed rm is a WARN, never a spawn failure.
		if logger != nil {
			logger.Warn("best-effort removal of the rootless docker data dir failed before the installer retry",
				"user", username, "dir", dataDir, "error", err, "output", condenseJournal(string(out)))
		}
	}
	return nil
}

// setupToolPathFor derives the dockerd-rootless-setuptool.sh path from the
// installer path: the installer script and the setup tool are installed
// together in the agent's bin directory (the installer's own error message
// references the setup tool by that sibling path).
func setupToolPathFor(installerPath string) string {
	return filepath.Join(filepath.Dir(installerPath), "dockerd-rootless-setuptool.sh")
}

// dockerUnitJournalQueryTimeout bounds the docker.service journal query on the
// retry-failure attribution path. Same detached-bounded pattern as the user
// manager's journal fetch (userManagerJournalQueryTimeout): a caller deadline
// that expires during the retry must not hollow out the diagnosis.
const dockerUnitJournalQueryTimeout = 5 * time.Second

// fetchDockerUnitJournal returns a bounded, single-line excerpt of the agent
// user's docker.service journal through the SAME session seam the unit itself
// runs under — the installer's own failure message points here ("Run
// `journalctl -n 20 --no-pager --user --unit docker.service` to show the error
// log"), and the job error alone names the symptom, never the cause. Best
// effort: an empty string when the journal cannot be read — attribution must
// never turn a failed retry into a different failure.
func fetchDockerUnitJournal(ctx context.Context, username string, uid int, logger *slog.Logger) string {
	runtimeDir := filepath.Join(userRuntimeBaseDir, fmt.Sprintf("%d", uid))
	queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dockerUnitJournalQueryTimeout)
	defer cancel()
	out, err := userSessionRunner(queryCtx, username, runtimeDir,
		sessionCommand(runtimeDir, "journalctl --no-pager -n 20 --user --unit "+dockerServiceUnitName))
	if err != nil && strings.TrimSpace(string(out)) == "" {
		if logger != nil {
			logger.Debug("no journal available for docker.service attribution", "user", username, "unit", dockerServiceUnitName, "error", err)
		}
		return ""
	}
	return condenseJournal(string(out))
}
