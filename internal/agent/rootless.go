// Package agent provides rootless Docker setup helpers for unprivileged agents.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// rootlessInstallURL is the official Docker rootless extras installer.
const rootlessInstallURL = "https://get.docker.com/rootless"

// userManagerWaitTimeout bounds the deterministic bring-up of the systemd user
// manager (start attempts + bus-socket wait). The wait must never grow with the
// caller's request deadline: INT-CI-007 showed an explicit-ID spawn burning the
// full 300s client deadline in a passive wait that could never succeed because
// the idempotent enable-linger restarted nothing after the unit was stopped.
const userManagerWaitTimeout = 30 * time.Second

// userManagerWaitTimeoutOverride lets tests shrink the budget to milliseconds.
// 0 means "use userManagerWaitTimeout". Package-level var on purpose: it is a
// test seam, not operator configuration.
var userManagerWaitTimeoutOverride time.Duration

// userManagerPollInterval is the bus-socket poll cadence. Package-level var so
// tests can tighten it without real sleeping.
var userManagerPollInterval = 200 * time.Millisecond

// userManagerStartStage names the deterministic bring-up sub-stage in error
// text. The spawn-level stage stays rootless-install (the failure happens
// inside it); this label makes the journal/breadcrumb grep-able for the exact
// step that failed (INT-CI-007 attribution requirement).
const userManagerStartStage = "user-manager-start"

// lingerDir is the systemd linger directory. Its entry count is reported on a
// failed bring-up because thousands of stale linger entries (INT-CI-007: 8024
// for 2 bunker users) starve user-manager starts host-wide. Var so tests can
// point it at a temp directory and assert a real count.
var lingerDir = "/var/lib/systemd/linger"

// userRuntimeBaseDir is the root of systemd's per-user runtime tree; the
// install path derives <userRuntimeBaseDir>/<uid> as the agent's standard
// runtime directory. Var for the same reason as lingerDir: non-root unit
// tests must be able to point the derivation at a temp tree instead of the
// real /run/user (INT-CI-009).
var userRuntimeBaseDir = "/run/user"

// systemRunner executes a system command and returns its combined output.
type systemRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// runSystemCmd is the production runner: a plain exec with combined output.
func runSystemCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// userManagerRunner executes systemctl/loginctl for the user-manager bring-up
// path. Package-level var so tests can inject a fake and never touch real
// systemd.
var userManagerRunner systemRunner = runSystemCmd

// userManagerReloadCmd is the user-session command used both to prove the
// agent's user manager is reachable and to make a freshly written unit
// visible before the installer retry (INT-CI-009).
const userManagerReloadCmd = "systemctl --user daemon-reload"

// userSessionRunner executes a command inside the agent user's session.
// Package-level seam mirroring userManagerRunner.
var userSessionRunner = runUserSessionCmd

// userSessionEnv builds the environment for a command that runs inside the
// agent user's session: the standard systemd runtime directory and the user
// manager's bus socket layered on top of the inherited environment. The
// installer call uses the SAME construction (plus its own toggles), so every
// user-session command — reachability probe, daemon-reload retry, installer —
// speaks to the same manager (INT-CI-009).
func userSessionEnv(runtimeDir string) []string {
	return append(os.Environ(),
		"XDG_RUNTIME_DIR="+runtimeDir,
		"DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(runtimeDir, "bus"),
	)
}

// runUserSessionCmd is the production runner: `su - <username> -c <script>`
// with the session environment applied.
func runUserSessionCmd(ctx context.Context, username, runtimeDir, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "su", "-", username, "-c", script)
	cmd.Env = userSessionEnv(runtimeDir)
	return cmd.CombinedOutput()
}

// rootlessInstallerRunner executes the downloaded rootless installer as the
// agent user. Package-level seam so the daemon-reload retry (INT-CI-009) is
// testable without a real su, real systemd, or network.
var rootlessInstallerRunner = runRootlessInstallerCmd

// rootlessInstallerDownload fetches the official installer into installerPath.
// Package-level seam so unit tests never touch the network.
var rootlessInstallerDownload = downloadRootlessInstaller

// downloadRootlessInstaller is the production download: curl to the official
// Docker rootless extras URL.
func downloadRootlessInstaller(ctx context.Context, installerPath string) ([]byte, error) {
	curl := exec.CommandContext(ctx, "curl", "-fsSL", "-o", installerPath, rootlessInstallURL)
	return curl.CombinedOutput()
}

// userLookup resolves a system user. Package-level seam so unit tests can
// install a synthetic uid without creating real users (same rationale as
// runtimeDirProbe).
var userLookup = user.Lookup

// rootHostRunner executes the root-side filesystem preparation commands of
// the install path (chown of bin dir, runtime dir, installer, home).
// Package-level seam (same type and production impl as userManagerRunner) so
// the full install flow is unit-testable without real users (INT-CI-009).
var rootHostRunner systemRunner = runSystemCmd

// runRootlessInstallerCmd is the production installer runner: the identical
// session environment to runUserSessionCmd plus the installer's own toggles.
func runRootlessInstallerCmd(ctx context.Context, username, runtimeDir, installerPath string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "su", "-", username, "-c", installerPath)
	cmd.Env = append(userSessionEnv(runtimeDir),
		"FORCE_ROOTLESS_INSTALL=1",
		"SKIP_IPTABLES=1",
	)
	return cmd.CombinedOutput()
}

// userManagerWaitBudget returns the effective bring-up budget.
func userManagerWaitBudget() time.Duration {
	if userManagerWaitTimeoutOverride > 0 {
		return userManagerWaitTimeoutOverride
	}
	return userManagerWaitTimeout
}

// removeMountsUnder lazily unmounts any filesystems mounted under dir. On
// desktop-flavoured hosts (Ubuntu with GNOME packages), the systemd user
// manager starts gvfsd-fuse for the agent user, mounting a FUSE filesystem
// at /run/user/<uid>/gvfs. That mount blocks rm -rf ("Device or resource
// busy") and breaks recursive chown -R ("Permission denied" traversing the
// FUSE mount). Lazy unmount (umount -l) detaches it even if busy.
func removeMountsUnder(ctx context.Context, dir string, logger *slog.Logger) {
	mounts, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		logger.Warn("cannot read /proc/self/mounts", "error", err)
		return
	}
	prefix := dir + "/"
	for _, line := range strings.Split(string(mounts), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		mountPoint := fields[1]
		if mountPoint == dir || strings.HasPrefix(mountPoint, prefix) {
			if out, err := exec.CommandContext(ctx, "umount", "-l", mountPoint).CombinedOutput(); err != nil {
				logger.Warn("lazy umount failed", "mount", mountPoint, "error", err, "output", string(out))
			} else {
				logger.Info("lazily unmounted stale runtime mount", "mount", mountPoint)
			}
		}
	}
}

// configureSubIDs ensures /etc/subuid and /etc/subgid contain a mapping for the
// given username. Rootless Docker needs a contiguous 65,536 UID/GID range per
// user. We map the range starting at the user's own UID/GID so every agent gets
// a unique namespace derived from its system identity.
func configureSubIDs(ctx context.Context, username string) error {
	u, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("lookup user %s: %w", username, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("parse uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return fmt.Errorf("parse gid %q: %w", u.Gid, err)
	}

	if err := ensureSubIDEntry("/etc/subuid", username, uid); err != nil {
		return fmt.Errorf("subuid: %w", err)
	}
	if err := ensureSubIDEntry("/etc/subgid", username, gid); err != nil {
		return fmt.Errorf("subgid: %w", err)
	}
	return nil
}

// ensureSubIDEntry appends a single mapping line to path when no mapping for
// name exists. The mapping is name:start:65536.
func ensureSubIDEntry(path, name string, start int) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) >= 1 && fields[0] == name {
			return nil // already configured
		}
	}
	entry := fmt.Sprintf("%s:%d:65536\n", name, start)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(entry); err != nil {
		return fmt.Errorf("append %s: %w", path, err)
	}
	return nil
}

// installRootlessDocker ensures the agent has the rootless Docker scripts in
// ~/bin. If dockerd-rootless.sh is not present, it downloads the official
// installer and runs it as the target user. The installer is idempotent.
func installRootlessDocker(ctx context.Context, username, userHome string, logger *slog.Logger) error {
	binDir := filepath.Join(userHome, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return fmt.Errorf("create bin dir: %w", err)
	}
	// The bin directory must be owned by the agent user because the installer
	// runs as that user and writes binaries into it.
	if out, err := rootHostRunner(ctx, "chown", "-R", username, binDir); err != nil {
		return fmt.Errorf("chown bin dir: %w (output: %s)", err, string(out))
	}

	rootlessScript := filepath.Join(binDir, "dockerd-rootless.sh")
	if _, err := os.Stat(rootlessScript); err == nil {
		logger.Info("rootless docker already installed", "user", username, "path", rootlessScript)
		return nil
	}

	logger.Info("installing rootless docker", "user", username)

	u, err := userLookup(username)
	if err != nil {
		return fmt.Errorf("lookup user %s: %w", username, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("parse uid %q: %w", u.Uid, err)
	}
	stdRuntimeDir := filepath.Join(userRuntimeBaseDir, strconv.Itoa(uid))

	// Bring the systemd user manager up in a state-consistent order: reset
	// stale state ONLY when there is real stale state, guarantee the runtime
	// directory exists and is owned by the uid, then start the manager. The
	// runtime directory MUST exist before user@<uid>.service starts, otherwise
	// pam_systemd refuses XDG_RUNTIME_DIR and the manager can never go active
	// (INT-CI-008).
	if err := bringUpUserManager(ctx, username, uid, stdRuntimeDir, logger); err != nil {
		return err
	}

	// The installer uses systemctl --user, which requires a writable runtime
	// directory and the systemd user manager bus. Use the standard systemd user
	// runtime path for the install step; the actual daemon runtime is set to
	// /run/bunker/<id>/run when dockerd is started by the manager.
	//
	// bringUpUserManager already created and ownership-verified this directory
	// BEFORE the manager start (INT-CI-008). This block is kept as insurance for
	// the install step, but it is no longer the only place the directory is
	// created.
	if err := os.MkdirAll(stdRuntimeDir, 0700); err != nil {
		return fmt.Errorf("create runtime dir %s: %w", stdRuntimeDir, err)
	}
	// The runtime dir is created by logind as the agent user, so ownership is
	// already correct in the fresh path. Do NOT chown recursively: on
	// desktop-flavoured hosts the user manager mounts gvfsd-fuse at
	// /run/user/<uid>/gvfs, and a FUSE mount without allow_other denies even
	// root (chown -R / find -xdev both fail with "Permission denied"). A
	// non-recursive chown of the top-level dir never descends into the mount.
	if out, err := rootHostRunner(ctx, "chown", username+":", stdRuntimeDir); err != nil {
		return fmt.Errorf("chown runtime dir %s: %w (output: %s)", stdRuntimeDir, err, string(out))
	}

	// Download the installer into the agent's home as root (the installer will
	// be executed by the target user, and we need a reliable download path).
	installerPath := filepath.Join(userHome, "rootless-install.sh")
	if out, err := rootlessInstallerDownload(ctx, installerPath); err != nil {
		return fmt.Errorf("download rootless installer: %w (output: %s)", err, string(out))
	}
	if err := os.Chmod(installerPath, 0755); err != nil {
		return fmt.Errorf("chmod installer: %w", err)
	}
	if out, err := rootHostRunner(ctx, "chown", username, installerPath); err != nil {
		return fmt.Errorf("chown installer: %w (output: %s)", err, string(out))
	}

	// Prove the user manager is reachable from the agent's OWN session before
	// handing over to the installer. The installer writes the docker.service
	// unit and immediately runs `systemctl --user start docker.service`; if the
	// manager bus is not usable from that session, the start fails with "Unit
	// docker.service not found" and the whole install fails (INT-CI-009 CI run
	// 35142150138). A successful daemon-reload IS the proof: it exercises the
	// session bus end to end. Failure here must carry the manager state, the
	// linger count and the journal — not a bare exec error.
	//
	// "Reachable" means the bus ANSWERS, not that its socket file exists: the
	// single probe of a manager that was still coming up became the final
	// attribution and killed every spawn of the standalone gate's nested suite
	// (INT-SPAWN-003). The bounded readiness wait retries the real probe on the
	// package budget instead, so a bus that answers ~0.7s late is a wait.
	//
	// The probe recovers ONCE when the readiness wait is exhausted against an
	// ACTIVE unit: that pair means the uid was recycled from a destroyed agent
	// whose user manager is still live (INT-SPAWN-001). The healthy path is
	// unchanged — a bus that answers on the first probe returns before any
	// fingerprint query or destructive step.
	if err := proveUserManagerReachableWithRecovery(ctx, username, uid, stdRuntimeDir, logger); err != nil {
		return err
	}

	// Run the installer as the target user. It installs binaries into ~/bin.
	// The standard systemd runtime directory and D-Bus bus address are provided
	// so systemctl --user can communicate with the user manager.
	//
	// The installer writes the docker.service unit and then starts it in the
	// same run. When the manager has not yet observed the freshly written unit,
	// that start races the unit lookup and fails with "Unit docker.service not
	// found" even though the manager itself is healthy (INT-CI-009). That
	// failure is retried ONCE after a user-session daemon-reload makes the new
	// unit visible. Any other installer failure is returned untouched.
	installerOut, installerErr := rootlessInstallerRunner(ctx, username, stdRuntimeDir, installerPath)
	if installerErr != nil {
		if !isUserUnitNotFound(installerOut) {
			return fmt.Errorf("run rootless installer as %s: %w (output: %s)", username, installerErr, string(installerOut))
		}
		if logger != nil {
			logger.Warn("rootless installer hit the unit-not-found race; reloading the user manager and retrying once",
				"user", username, "unit", userManagerUnitName(uid),
				"output", condenseJournal(string(installerOut)))
		}
		if _, err := userSessionRunner(ctx, username, stdRuntimeDir, userManagerReloadCmd); err != nil {
			// The reload itself failed: retrying the installer through a dead
			// bus would only reproduce the same not-found failure, so fail
			// with the full attribution instead.
			state := fetchUserManagerState(ctx, userManagerUnitName(uid))
			return fmt.Errorf("rootless-install: user-session daemon-reload before the installer retry failed for %s: %w; %s; linger entries: %d; installer output: %s",
				username, err, state.describe(userManagerUnitName(uid)), countLingerEntries(), condenseJournal(string(installerOut)))
		}
		installerOut, installerErr = rootlessInstallerRunner(ctx, username, stdRuntimeDir, installerPath)
		if installerErr != nil {
			state := fetchUserManagerState(ctx, userManagerUnitName(uid))
			return fmt.Errorf("run rootless installer as %s: %w (retry after user-session daemon-reload also failed; output: %s; %s; linger entries: %d)",
				username, installerErr, condenseJournal(string(installerOut)), state.describe(userManagerUnitName(uid)), countLingerEntries())
		}
		if logger != nil {
			logger.Info("rootless installer succeeded on the daemon-reload retry", "user", username)
		}
	}

	if _, err := os.Stat(rootlessScript); err != nil {
		return fmt.Errorf("rootless install completed but %s is missing: %w", rootlessScript, err)
	}

	// The installer may have created files (config, install script, etc.) as
	// root. Chown the entire home directory to the agent user so userdel -r
	// can clean up cleanly during destroy.
	if out, err := rootHostRunner(ctx, "chown", "-R", username+":", userHome); err != nil {
		logger.Warn("failed to chown agent home after rootless install", "user", username, "error", err, "output", string(out))
	}

	return nil
}

// proveUserManagerReachable verifies that the agent's systemd user manager is
// reachable from the agent user's OWN session before the rootless installer
// runs. The installer writes the docker.service unit and immediately runs
// `systemctl --user start docker.service`; when the session bus is not usable
// from that session, the start fails with "Unit docker.service not found" and
// the installer exits 1 (INT-CI-009 CI run 35142150138: "no user session bus",
// "Failed to connect to bus: No medium found").
//
// The probe is `systemctl --user daemon-reload` executed through
// userSessionRunner — the exact environment the installer receives. A reload
// succeeds only when the bus accepts and answers user-manager requests, so it
// is a proof of reachability, not a mere existence check.
//
// On failure the returned error names the stage, the user-manager state, the
// linger-entry count, a journal excerpt AND the probe's own output, so the
// operator can tell a dead manager from a starving one without re-running the
// spawn. At most one WARN is emitted with the same facts.
//
// The probe's own output is load-bearing attribution, not decoration: the
// session-side stderr ("Failed to connect to bus: No medium found",
// "Permission denied", "Unit docker.service not found") names the cause while
// `systemctl is-active`/`Result` only name the symptom. It is condensed the
// same way the journal excerpt is (one line, rune-safe, bounded) and only when
// there is output at all.
func proveUserManagerReachable(ctx context.Context, username string, uid int, runtimeDir string, logger *slog.Logger) error {
	out, err := probeUserManagerReachable(ctx, username, runtimeDir)
	if err != nil {
		return proveUserManagerReachableErr(ctx, username, uid, runtimeDir, logger, out)
	}
	return nil
}

// probeUserManagerReachable runs the reachability probe and returns the
// session runner's own combined output alongside the error, so a caller that
// must attribute the failure (the recycled-uid recovery's second probe) can
// hand the output to proveUserManagerReachableErr.
func probeUserManagerReachable(ctx context.Context, username, runtimeDir string) ([]byte, error) {
	return userSessionRunner(ctx, username, runtimeDir, userManagerReloadCmd)
}

// proveUserManagerReachableErr builds the attribution error after a failed
// reachability probe. Split from proveUserManagerReachable so the success
// path stays a single seam call. probeOut is the failed probe's own combined
// output; it is appended to the WARN (field session_probe) and to the error
// text so the failure is diagnosable from the log alone.
func proveUserManagerReachableErr(ctx context.Context, username string, uid int, runtimeDir string, logger *slog.Logger, probeOut []byte) error {
	unit := userManagerUnitName(uid)
	state := fetchUserManagerState(ctx, unit)
	lingerCount := countLingerEntries()
	journal := fetchUserManagerJournal(ctx, unit, logger)
	probe := condenseJournal(string(probeOut))
	if logger != nil {
		attrs := []any{
			"stage", "rootless-install",
			"user", username,
			"unit", unit,
			"state", state.describe(unit),
			"linger_entries", lingerCount,
			"journal", journal,
		}
		if probe != "" {
			attrs = append(attrs, "session_probe", probe)
		}
		logger.Warn("agent user manager unreachable from the agent session before the rootless install", attrs...)
	}
	msg := fmt.Sprintf("rootless-install: agent user manager unreachable before the rootless install for %s: daemon-reload through the session bus failed; %s; linger entries: %d; journal: %s",
		username, state.describe(unit), lingerCount, journal)
	if probe != "" {
		msg += "; session probe: " + probe
	}
	// errors.New (not fmt.Errorf): the probe's and the journal's output are
	// arbitrary host text and must never be re-interpreted as a format string.
	return errors.New(msg)
}

// userManagerReadinessRetryInfo is the ONE extra log line the readiness gate
// emits when a retry was actually needed. Its existence is the point: it tells
// an operator that the socket existed but the bus had not started answering
// yet, so a late-answering bus is visibly a WAIT rather than a failure. The
// per-attempt decisions are not logged (the gate can poll for the whole
// budget); only the first retry names itself, with the attempt and the first
// probe's own error.
const userManagerReadinessRetryInfo = "agent user manager socket present but the session bus is not answering yet; retrying within the readiness budget"

// userManagerReadinessDiagTimeout bounds the diagnostics the readiness gate
// gathers for its exhaustion attribution (unit state, linger count, journal
// excerpt). They run DETACHED from the caller's request deadline for the same
// reason userManagerRecoveryDiagTimeout does: a caller deadline that expires
// while the gate polls must not turn the very state that caused the failure
// into "query-error: context deadline exceeded".
const userManagerReadinessDiagTimeout = 5 * time.Second

// waitForUserManagerBus proves that the agent's user manager bus ACCEPTS a
// connection — not merely that its socket file exists. It repeats the REAL
// session-side probe (probeUserManagerReachable: `systemctl --user
// daemon-reload` through userSessionRunner, the exact command the installer
// receives) until it succeeds or the package budget is exhausted.
//
// WHY (INT-SPAWN-003, tick 453 on bunker-mvp): a socket file existing is not
// evidence that the bus answers. Every spawn in the standalone gate's nested
// regression suite died at stage rootless-install with
// "Failed to connect to bus: No medium found" ~0.7s after a CLEAN manager
// start ("Listening on dbus.socket"), because the single probe ran while the
// manager was still coming up and its failure became the FINAL attribution
// instead of a retryable NOT-READY condition. A bus that answers one poll
// interval late must be a wait, not a spawn failure.
//
// BUDGET: bounded by the existing package budget (userManagerWaitBudget, i.e.
// userManagerWaitTimeout / userManagerWaitTimeoutOverride) so the wait never
// grows with the caller's request deadline. The caller's DEADLINE is never
// inherited as the reported cause (INT-CI-007): once it expires the remaining
// probes run DETACHED (context.WithoutCancel) on the same budget, because a
// client that gave up must not be able to declare a reachable bus unreachable.
// A genuine caller CANCEL still returns promptly.
//
// HEALTHY PATH: a bus that answers immediately costs exactly ONE probe call
// and zero destructive action, and logs nothing (the retry line appears only
// when a retry actually happened).
//
// EXHAUSTION: the error is the existing attribution error
// (proveUserManagerReachableErr), carrying stage, user, unit, unit state,
// linger count, journal excerpt AND the LAST probe's own stderr
// (session_probe), so existing greps keep matching.
func waitForUserManagerBus(ctx context.Context, username string, uid int, runtimeDir string, logger *slog.Logger) error {
	budget := userManagerWaitBudget()
	deadline := time.Now().Add(budget)
	ticker := time.NewTicker(userManagerPollInterval)
	defer ticker.Stop()

	// The probes run under the CALLER's context while that context is alive,
	// so a cancellation is honored promptly. Once the caller's DEADLINE
	// expires (or was already expired on entry) the remaining probes run
	// detached: a caller deadline must never truncate a wait that would have
	// succeeded, and must never surface as the reported cause.
	probeCtx := ctx
	detachedCtx, cancelDetached := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancelDetached()

	var lastProbeOut []byte
	attempt := 0
	for {
		if ctx.Err() == context.Canceled {
			return context.Canceled
		}
		if ctx.Err() != nil {
			probeCtx = detachedCtx
		}

		attempt++
		out, err := probeUserManagerReachable(probeCtx, username, runtimeDir)
		if err == nil {
			return nil
		}
		lastProbeOut = out

		// Any probe failure is a NOT-READY condition, including "Failed to
		// connect to bus: No medium found" and a probe that was interrupted by
		// the caller's own deadline. Cancellation is the one exception.
		if ctx.Err() == context.Canceled {
			return context.Canceled
		}
		if !time.Now().Before(deadline) {
			break
		}
		if attempt == 1 && logger != nil {
			logger.Info(userManagerReadinessRetryInfo,
				"stage", "rootless-install",
				"user", username,
				"uid", uid,
				"unit", userManagerUnitName(uid),
				"attempt", attempt,
				"retry_attempt", attempt+1,
				"session_probe", condenseJournal(string(out)),
			)
		}
		if ctx.Err() != nil {
			// The caller's Done channel is closed (deadline), so selecting on
			// it again would return instantly and spin the loop hot. Sleep one
			// poll interval instead; a later genuine cancel() surfaces at the
			// loop-top check within one interval.
			time.Sleep(userManagerPollInterval)
			continue
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}

	// Exhausted on the gate's OWN budget: attribute the failure the way the
	// single-probe path always did, with the diagnostics gathered detached so
	// an expired caller deadline cannot hollow them out.
	attrCtx, cancelAttr := context.WithTimeout(context.WithoutCancel(ctx), userManagerReadinessDiagTimeout)
	defer cancelAttr()
	return proveUserManagerReachableErr(attrCtx, username, uid, runtimeDir, logger, lastProbeOut)
}

// recycledUIDRecoveryMarker names the one-shot recycled-uid recovery in the
// WARN and in the error text, so an operator (and a test) can grep for the
// exact remedy that ran instead of parsing prose.
const recycledUIDRecoveryMarker = "recycled-uid recovery"

// recycledUIDRecoveryWarn is the single WARN emitted when the fingerprint
// matches. It names the recovery, the user, the uid and the unit.
const recycledUIDRecoveryWarn = recycledUIDRecoveryMarker +
	": the unit is ACTIVE but the agent session cannot reach the manager; tearing the foreign manager down and bringing the manager up again"

// userManagerRecoveryDiagTimeout bounds the recovery's own diagnostics (unit
// state, linger count, journal excerpt). They run DETACHED from the caller's
// request deadline: a nearly-expired deadline must not turn the very state that
// triggered the recovery into "query-error: context deadline exceeded".
const userManagerRecoveryDiagTimeout = 5 * time.Second

// proveUserManagerReachableWithRecovery probes the agent's user manager and,
// when the probe fails against an ACTIVE unit, recovers ONCE from a uid
// recycled from a previously destroyed agent.
//
// FINGERPRINT: `systemctl is-active user@<uid>.service` reports active
// (Result=success) while `su - <user> -c "systemctl --user daemon-reload"`
// fails. Only one host state produces that pair — a uid whose PREVIOUS owner's
// user manager is still running: the unit is active for the recycled uid but
// the live manager belongs to the old user and does not serve the new one. It
// is invisible to classifyRuntimeDir, because a recycled uid legitimately OWNS
// its runtime directory, so the state is classified fresh and the INT-CI-008
// reset never runs (tick 450 on bunker-mvp: spawn of regr-alpha died at stage
// rootless-install exactly this way, with linger entries still held for the
// destroyed agents).
//
// RECOVERY, in the INT-CI-008 state-consistent order and using the existing
// seams: emit ONE WARN naming the fingerprint, clear the uid's FOREIGN logind
// user record (see classifyForeignLogindRecord: `loginctl disable-linger
// <recorded name>`, then `loginctl terminate-user <uid>`) so logind has no
// reason to restart the manager again, run resetUserManagerState (stop
// user@<uid>.service, loginctl terminate-user <uid>, unmount under the runtime
// dir, stop user-runtime-dir@<uid>.service, remove the runtime dir), re-check
// the record and name the logind-respawn event when the teardown did not
// stick, re-run the SAME bring-up the healthy path uses so the directory, the
// linger entry and the manager come back in the documented order, then probe a
// FINAL second time.
//
// WHY ONE-SHOT: a manager that is still unreachable after a single teardown has
// a host-level cause (linger churn starving the start, a runtime directory that
// cannot be created) and looping teardowns would only multiply host damage
// while hiding that cause. So the function never loops: exactly one recovery,
// at most two bounded readiness waits (see waitForUserManagerBus), and the
// second wait's failure returns the existing attribution error extended with
// the fact that the recovery was attempted. Too-hard failures are reported,
// not retried away.
//
// The healthy path is unchanged by construction: a bus that answers on the
// first probe returns nil before the fingerprint query, so it performs no stop,
// no terminate-user and no rm. A caller that already gave up (cancelled
// context) gets its own cancellation back promptly instead of having a teardown
// started for it.
func proveUserManagerReachableWithRecovery(ctx context.Context, username string, uid int, runtimeDir string, logger *slog.Logger) error {
	// 1. Prove the session bus ANSWERS before anything destructive: a bus that
	// answers a poll interval late is a bounded WAIT, not a failure
	// (INT-SPAWN-003 — the tick-453 failure was a single probe against a
	// manager that was still coming up). Zero destructive action either way.
	probeErr := waitForUserManagerBus(ctx, username, uid, runtimeDir, logger)
	if probeErr == nil {
		return nil
	}

	// A caller that gave up must surface promptly: starting a teardown on a
	// dead context would only leave the foreign state half-removed.
	if err := ctx.Err(); err != nil {
		return err
	}

	// 2. Fingerprint: an ACTIVE unit behind an unreachable session is the
	// recycled-uid signature. The query runs detached and bounded so the
	// caller's remaining budget cannot hollow out the diagnosis.
	unit := userManagerUnitName(uid)
	diagCtx, cancelDiag := context.WithTimeout(context.WithoutCancel(ctx), userManagerRecoveryDiagTimeout)
	defer cancelDiag()
	state := fetchUserManagerState(diagCtx, unit)
	if state.active != "active" {
		// Not the fingerprint: a manager that is not active owns no foreign
		// state to tear down, so the attribution error (already built by the
		// probe, probe output included) is returned unchanged and no recovery
		// is attempted.
		return probeErr
	}

	// 3. Exactly ONE WARN naming the recovery, the user, the uid and the unit.
	if logger != nil {
		logger.Warn(recycledUIDRecoveryWarn,
			"stage", "rootless-install",
			"user", username,
			"uid", uid,
			"unit", unit,
			"state", state.describe(unit),
			"linger_entries", countLingerEntries(),
			"journal", fetchUserManagerJournal(diagCtx, unit, logger),
		)
	}

	// 4. The stale LOGIND user record — the state that SURVIVES the reset.
	// logind keeps a per-uid user record with its linger flag after the
	// account is destroyed, so it restarts user@<uid>.service for that
	// lingering uid the moment it is stopped: `systemctl is-active` is
	// "active" again right after the stop, ensureUserManagerRunning returns
	// nil immediately and the final probe still cannot reach the manager
	// (tick 452 on bunker-mvp: the recovery FIRED and recovered nothing).
	// Clearing the linger flag before the teardown removes logind's reason to
	// start the manager again, and it also un-blocks the bring-up's own
	// `loginctl enable-linger <user>`, which is a NO-OP while an entry for the
	// uid is already recorded. Every step is best effort.
	record := fetchLogindUserRecord(diagCtx, uid)
	foreign, reason := classifyForeignLogindRecord(record, username)
	if foreign {
		if logger != nil {
			logger.Warn(staleLogindRecordWarn,
				"stage", "rootless-install",
				"user", username,
				"uid", uid,
				"record_name", record.name,
				"record_linger", record.linger,
				"record_state", record.state,
				"reason", reason,
				"linger_entries", countLingerEntries(),
			)
		}
		clearForeignLogindRecord(diagCtx, uid, record, logger)
	}

	// 5. Tear the foreign state down in the INT-CI-008 state-consistent order,
	// then bring the manager back up in the SAME order the healthy path uses
	// (reset → runtime dir → linger → manager start → bus wait). The bring-up
	// re-runs classifyRuntimeDir against the directory the reset removed, so it
	// classifies fresh and does not reset a second time.
	resetUserManagerState(ctx, uid, runtimeDir, logger)

	// 5b. Re-check the record the recovery just cleared. A record that is STILL
	// linger-active, or a unit that is ACTIVE again although the reset stopped
	// it, is the logind-respawn event; naming it is what makes a
	// recovery-that-recovered-nothing diagnosable from the log alone.
	if foreign {
		recheckCtx, cancelRecheck := context.WithTimeout(context.WithoutCancel(ctx), userManagerRecoveryDiagTimeout)
		logLogindRespawn(recheckCtx, uid, record, unit, logger)
		cancelRecheck()
	}

	if err := bringUpUserManager(ctx, username, uid, runtimeDir, logger); err != nil {
		// The teardown or the bring-up itself failed: report it wrapped with
		// the recovery marker instead of probing a manager that was never
		// brought up (that probe's error would name the symptom, not the step
		// that broke).
		return fmt.Errorf("%s: bringing the user manager back up for %s (uid %d) failed; the unit was torn down and re-created: %w",
			recycledUIDRecoveryMarker, username, uid, err)
	}

	// 6. Second and FINAL readiness wait — the same user-session daemon-reload
	// the healthy path issues, again bounded by the package budget, so a
	// manager that answers shortly after the bring-up proceeds instead of
	// failing the spawn.
	finalErr := waitForUserManagerBus(ctx, username, uid, runtimeDir, logger)
	if finalErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%w; %s was attempted once (user@%d.service torn down and brought back up) and the agent session still cannot reach the manager",
			finalErr, recycledUIDRecoveryMarker, uid)
	}
	return nil
}

// ── the stale logind user record (recycled uid) ────────────────────────────
//
// logind keeps a per-UID user record (name, linger flag, state, session ids)
// and it does NOT remove it when the account is destroyed: `loginctl show-user
// <uid>` on the demo host reported the name of a DELETED account as
// State=active, Linger=yes, while `getent passwd` had no such user, and
// /var/lib/systemd/linger held markers for five users that no longer existed
// (tick 452). A recycled uid therefore inherits a linger flag that makes
// logind restart user@<uid>.service the moment anything stops it, and makes
// `loginctl enable-linger <new-user>` a NO-OP. That is the state the existing
// reset cannot remove and the reason the recovery fired and recovered nothing.

// staleLogindRecordMarker names the stale logind user record in WARN text, so
// an operator can grep for the exact host state that made the recovery
// necessary instead of parsing prose.
const staleLogindRecordMarker = "stale logind user record"

// staleLogindRecordWarn is the single WARN emitted when the uid's logind
// record is FOREIGN (see classifyForeignLogindRecord).
const staleLogindRecordWarn = staleLogindRecordMarker +
	": logind still holds a user record and its linger flag for a uid whose previous owner is gone; clearing the record before the manager is torn down"

// logindRespawnMarker names the logind-respawn event in WARN text: the manager
// came back on its own after the teardown. Naming it is what tells a recovery
// that recovered nothing apart from a mystery.
const logindRespawnMarker = "logind respawn"

// logindRespawnWarn is the single WARN emitted when the record the recovery
// cleared is still linger-active, or the unit is active again, after the
// teardown.
const logindRespawnWarn = logindRespawnMarker +
	": logind restarted the user manager for a uid whose previous owner is gone; the teardown did not stick"

// logindUserRecordProperties are the properties the recycled-uid fingerprint
// needs: the recorded Name (whose account may no longer exist), the Linger
// flag (what makes logind restart the manager) and the State (context for the
// operator).
const logindUserRecordProperties = "Name,Linger,State"

// logindUserRecord is logind's per-UID user record, reduced to what the
// fingerprint needs.
type logindUserRecord struct {
	// known is true only when the record carries a NAME we can act on. An
	// unreadable record, or one without a name, is "no evidence" and never
	// "foreign": no destructive step may run on an absence of evidence.
	known bool
	// name is the account logind has recorded for the uid.
	name string
	// linger is the recorded Linger flag ("yes"/"no"); empty when unknown.
	linger string
	// state is the recorded State ("active", ...); diagnostic only.
	state string
	// queryError carries loginctl's own output when the query failed.
	queryError string
}

// fetchLogindUserRecord reads logind's per-uid user record through the same
// seam and with the same best-effort contract as fetchUserManagerState: a
// failed or silent query yields known=false, so the caller takes no action.
// (`loginctl show-user <uid>` is preferred over `loginctl list-users` because
// it is per-uid and bounded; the caller passes a bounded, caller-detached
// context.)
func fetchLogindUserRecord(ctx context.Context, uid int) logindUserRecord {
	out, err := userManagerRunner(ctx, "loginctl", "show-user", strconv.Itoa(uid),
		"--property="+logindUserRecordProperties)
	rec := logindUserRecord{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "Name":
			rec.name = strings.TrimSpace(value)
		case "Linger":
			rec.linger = strings.TrimSpace(value)
		case "State":
			rec.state = strings.TrimSpace(value)
		}
	}
	if err != nil {
		rec.queryError = strings.TrimSpace(string(out))
		if rec.queryError == "" {
			rec.queryError = err.Error()
		}
	}
	rec.known = rec.name != ""
	return rec
}

// classifyForeignLogindRecord decides whether the uid's logind user record
// belongs to a PREVIOUS owner of the uid — the recycled-uid fingerprint that
// survives the existing reset — rather than to the user being brought up.
//
// FOREIGN requires ALL of:
//
//   - logind carries a record for the uid whose Name is readable;
//   - the recorded name is NOT the username being brought up, or the recorded
//     account no longer exists in the passwd database (userdel removes the
//     account; logind keeps the record);
//   - the recorded Linger flag is yes. Without linger logind has no reason to
//     restart the manager, so there is no foreign linger state to clear.
//
// Everything else is NOT foreign — an unreadable record, one with no name, one
// whose linger is off, and one whose name is the user being brought up. The
// last case is a deliberate residual: a record that names the user we are
// creating cannot be distinguished from a healthy one by these properties
// alone (the host re-creates the SAME agent names repeatedly), so it is left
// alone rather than torn down on a guess.
//
// The returned reason names the clause that matched, so the WARN says WHY the
// record was treated as foreign.
func classifyForeignLogindRecord(rec logindUserRecord, username string) (bool, string) {
	if !rec.known {
		return false, ""
	}
	if !strings.EqualFold(rec.linger, "yes") {
		return false, ""
	}
	if rec.name != username {
		return true, fmt.Sprintf("logind has the uid's record under the name %q, not %q", rec.name, username)
	}
	if _, err := userLookup(rec.name); err != nil {
		return true, fmt.Sprintf("logind has the uid's record under %q, whose account no longer exists", rec.name)
	}
	return false, ""
}

// clearForeignLogindRecord clears the foreign logind record for uid:
// `loginctl disable-linger <recorded name>` first — that is the flag that makes
// logind restart the manager, and clearing it also un-blocks the bring-up's own
// enable-linger, which is a NO-OP while an entry for the uid is already
// recorded — then `loginctl terminate-user <uid>`. Rec.name is non-empty
// whenever this runs (classifyForeignLogindRecord requires a readable name).
//
// Every step is best effort and is issued AT MOST ONCE: the recovery is
// one-shot, so a failure is WARNed with loginctl's own output and the recovery
// continues into the teardown.
func clearForeignLogindRecord(ctx context.Context, uid int, rec logindUserRecord, logger *slog.Logger) {
	if out, err := userManagerRunner(ctx, "loginctl", "disable-linger", rec.name); err != nil && logger != nil {
		logger.Warn("could not disable lingering for the stale logind record's user; logind may restart the user manager again",
			"user", rec.name, "uid", uid, "error", err, "output", condenseJournal(string(out)))
	}
	_, _ = userManagerRunner(ctx, "loginctl", "terminate-user", strconv.Itoa(uid))
}

// logLogindRespawn re-reads the logind record and the unit state AFTER the
// teardown and WARNs when either shows the manager came back on its own: the
// record is still linger-active, or the unit is active again although the
// teardown stopped it. Without that line, "the recovery ran and recovered
// nothing" is indistinguishable from a recovery that was never reached.
func logLogindRespawn(ctx context.Context, uid int, cleared logindUserRecord, unit string, logger *slog.Logger) {
	if logger == nil {
		return
	}
	after := fetchLogindUserRecord(ctx, uid)
	state := fetchUserManagerState(ctx, unit)
	stillLingering := strings.EqualFold(after.linger, "yes")
	if !stillLingering && state.active != "active" {
		return
	}
	event := "the unit is active again after the teardown"
	if stillLingering {
		event = "the uid's record is still linger-active after the teardown"
	}
	logger.Warn(logindRespawnWarn,
		"stage", "rootless-install",
		"uid", uid,
		"unit", unit,
		"cleared_name", cleared.name,
		"record_name", after.name,
		"record_linger", after.linger,
		"state", state.describe(unit),
		"event", event,
	)
}

// isUserUnitNotFound reports whether the installer output carries the
// unit-not-found signature: "not found" together with "docker.service",
// case-insensitive. It is deliberately strict — the retry after a
// daemon-reload is only safe for the manager-has-not-seen-the-unit race, so
// unrelated installer failures (network, disk, permissions) must never
// trigger it.
func isUserUnitNotFound(out []byte) bool {
	lower := strings.ToLower(string(out))
	return strings.Contains(lower, "not found") && strings.Contains(lower, "docker.service")
}

// userManagerUnitName is the per-uid systemd user manager unit.
func userManagerUnitName(uid int) string { return fmt.Sprintf("user@%d.service", uid) }

// userRuntimeDirUnitName is logind's runtime-directory unit for the same uid:
// its ExecStart runs /usr/lib/systemd/systemd-user-runtime-dir start <uid>, so
// systemd itself owns the creation of /run/user/<uid>. It is
// Type=oneshot + RemainAfterExit=yes + StopWhenUnneeded=yes, which makes it a
// best-effort first step only: once it has run, starting it again is a no-op
// even when the directory is gone. The directory must therefore be created and
// VERIFIED explicitly (INT-CI-008).
func userRuntimeDirUnitName(uid int) string {
	return fmt.Sprintf("user-runtime-dir@%d.service", uid)
}

// runtimeDirInfo is the observable state of a systemd user runtime directory.
type runtimeDirInfo struct {
	// exists is false when the path is absent (the fresh-uid path).
	exists bool
	// isDir is false when the path exists but is not a directory.
	isDir bool
	// owner is the uid owning the path; ownerKnown is false when the platform
	// stat could not supply it.
	owner      uint32
	ownerKnown bool
}

// runtimeDirProbe inspects a runtime directory. Package-level var (same seam
// style as userManagerRunner) because simulating a directory owned by a
// DIFFERENT uid needs a real chown to another user, which requires root.
var runtimeDirProbe = probeRuntimeDirOnDisk

// probeRuntimeDirOnDisk is the production probe. It uses Lstat so a symlink
// placed at the directory's path is NOT mistaken for a directory.
func probeRuntimeDirOnDisk(path string) (runtimeDirInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return runtimeDirInfo{}, nil
		}
		return runtimeDirInfo{}, err
	}
	st := runtimeDirInfo{exists: true, isDir: info.IsDir()}
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		st.owner = sys.Uid
		st.ownerKnown = true
	}
	return st, nil
}

// classifyRuntimeDir decides whether the observed runtime directory carries
// state left behind by a PREVIOUS user of the same uid (which must be reset) or
// is the fresh path (which must not be touched). It fails SAFE: anything it
// cannot read or cross-check — an unreadable probe, a non-directory entry, an
// unknowable owner — counts as stale, so the historical reset behavior is kept
// rather than silently skipped.
func classifyRuntimeDir(info runtimeDirInfo, probeErr error, uid int) (bool, string) {
	switch {
	case probeErr != nil:
		return true, "runtime dir probe failed: " + probeErr.Error()
	case !info.exists:
		return false, ""
	case !info.isDir:
		return true, "runtime path exists but is not a directory"
	case !info.ownerKnown:
		return true, "runtime dir ownership cannot be determined"
	case info.owner != uint32(uid):
		return true, fmt.Sprintf("runtime dir owned by uid %d, not %d", info.owner, uid)
	}
	return false, ""
}

// resetUserManagerState tears down stale user manager state for uid so the next
// start begins from a known-clean state, in a state-consistent order: stop the
// manager, stop logind's runtime-directory unit (so its RemainAfterExit state
// matches the filesystem instead of claiming a directory it no longer owns),
// unmount whatever the old manager left mounted under the directory, remove the
// directory. Every step is best effort; the caller re-creates the directory
// before starting the manager.
func resetUserManagerState(ctx context.Context, uid int, runtimeDir string, logger *slog.Logger) {
	_, _ = userManagerRunner(ctx, "systemctl", "stop", userManagerUnitName(uid))
	_, _ = userManagerRunner(ctx, "loginctl", "terminate-user", strconv.Itoa(uid))
	removeMountsUnder(ctx, runtimeDir, logger)
	_, _ = userManagerRunner(ctx, "systemctl", "stop", userRuntimeDirUnitName(uid))
	if _, err := os.Stat(runtimeDir); err == nil {
		if out, err := userManagerRunner(ctx, "rm", "-rf", runtimeDir); err != nil && logger != nil {
			logger.Warn("failed to remove stale runtime dir",
				"dir", runtimeDir, "error", err, "output", strings.TrimSpace(string(out)))
		}
	}
}

// ensureUserRuntimeDir guarantees the runtime directory exists and is owned by
// uid BEFORE the user manager is started.
//
// systemd 255's user@.service has no Requires/After on
// user-runtime-dir@<uid>.service, so starting the manager does not create the
// directory. With it missing, pam_systemd refuses to set XDG_RUNTIME_DIR
// ("Failed to stat() runtime directory '/run/user/<uid>'"), the manager exits 1
// and the unit can never become active — the restart that failed the regression
// job (INT-CI-008). Ask systemd's own directory unit to create it, create it
// ourselves when that unit is a no-op, then verify the result.
func ensureUserRuntimeDir(ctx context.Context, username string, uid int, stdRuntimeDir string, logger *slog.Logger) error {
	if out, err := userManagerRunner(ctx, "systemctl", "start", userRuntimeDirUnitName(uid)); err != nil && logger != nil {
		logger.Warn("systemd user-runtime-dir unit did not start; creating the runtime dir directly",
			"unit", userRuntimeDirUnitName(uid), "dir", stdRuntimeDir,
			"error", err, "output", strings.TrimSpace(string(out)))
	}
	if err := os.MkdirAll(stdRuntimeDir, 0o700); err != nil {
		return fmt.Errorf("create runtime dir %s: %w", stdRuntimeDir, err)
	}
	// Non-recursive on purpose: on desktop-flavoured hosts the previous manager
	// may have left a gvfsd-fuse mount under the directory, and a FUSE mount
	// without allow_other denies even root (see removeMountsUnder).
	if out, err := userManagerRunner(ctx, "chown", username+":", stdRuntimeDir); err != nil {
		return fmt.Errorf("chown runtime dir %s: %w (output: %s)", stdRuntimeDir, err, string(out))
	}
	info, err := runtimeDirProbe(stdRuntimeDir)
	if err != nil {
		return fmt.Errorf("verify runtime dir %s after creation: %w", stdRuntimeDir, err)
	}
	if !info.exists || !info.isDir {
		return fmt.Errorf("runtime dir %s is missing after creation", stdRuntimeDir)
	}
	if !info.ownerKnown {
		return fmt.Errorf("runtime dir %s ownership cannot be verified", stdRuntimeDir)
	}
	if info.owner != uint32(uid) {
		return fmt.Errorf("runtime dir %s is owned by uid %d, expected %d", stdRuntimeDir, info.owner, uid)
	}
	return nil
}

// bringUpUserManager brings the systemd user manager for uid up in a
// state-consistent order and waits for its bus socket:
//
//  1. reset the runtime directory ONLY when it carries state from a previous
//     user of that uid (the fresh path takes no destructive action at all);
//  2. create and verify the runtime directory, owned by uid;
//  3. enable lingering so the manager survives without a login session;
//  4. start user@<uid>.service deterministically;
//  5. wait for the manager's bus socket.
//
// The order of steps 2 and 4 is load-bearing (INT-CI-008): a manager started
// without its runtime directory fails permanently, so the start can never be
// allowed to be the thing that creates it. Step 2 also runs before step 3
// because enabling linger can itself attempt a manager start.
func bringUpUserManager(ctx context.Context, username string, uid int, stdRuntimeDir string, logger *slog.Logger) error {
	info, probeErr := runtimeDirProbe(stdRuntimeDir)
	if stale, reason := classifyRuntimeDir(info, probeErr, uid); stale {
		if logger != nil {
			logger.Info("resetting stale user manager runtime",
				"user", username, "uid", uid, "runtime_dir", stdRuntimeDir, "reason", reason)
		}
		resetUserManagerState(ctx, uid, stdRuntimeDir, logger)
	}

	if err := ensureUserRuntimeDir(ctx, username, uid, stdRuntimeDir, logger); err != nil {
		return err
	}

	// Enable systemd lingering so the user manager is available for the
	// installer to run `systemctl --user start docker.service`. This must be
	// done as root before dropping to the target user.
	if out, err := userManagerRunner(ctx, "loginctl", "enable-linger", username); err != nil {
		return fmt.Errorf("enable linger for %s: %w (output: %s)", username, err, string(out))
	}
	// enable-linger is idempotent: when lingering is ALREADY recorded for the
	// user (CI hosts re-spawn the same explicit agent ID repeatedly) it takes
	// no start action, and a unit stopped by the reset above stays down. So
	// bring the manager up deterministically instead of waiting passively
	// (INT-CI-007: the passive wait burned the whole 300s request deadline).
	if err := ensureUserManagerRunning(ctx, uid, logger); err != nil {
		return err
	}
	if err := waitForUserManager(ctx, stdRuntimeDir); err != nil {
		return fmt.Errorf("user manager did not start for %s: %w", username, err)
	}
	return nil
}

// ensureRootlesskitAppArmor writes an AppArmor profile for rootlesskit on
// Ubuntu 24.04+ where unprivileged user namespaces are restricted by
// AppArmor. The profile format matches the one emitted by Docker's own
// rootless installer when it fails on apparmor_restrict_unprivileged_userns.
func ensureRootlesskitAppArmor(ctx context.Context, username string, logger *slog.Logger) error {
	u, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("lookup user %s: %w", username, err)
	}
	rootlesskitBin := filepath.Join(u.HomeDir, "bin", "rootlesskit")
	profileName := fmt.Sprintf("home.%s.bin.rootlesskit", username)
	profilePath := filepath.Join("/etc/apparmor.d", profileName)

	// If the profile already exists, assume it is correct.
	if _, err := os.Stat(profilePath); err == nil {
		return nil
	}

	// apparmor_parser is required; if it's missing we cannot install the profile.
	if _, err := exec.LookPath("apparmor_parser"); err != nil {
		return fmt.Errorf("apparmor_parser not found: %w", err)
	}

	profile := fmt.Sprintf(`# ref: https://ubuntu.com/blog/ubuntu-23-10-restricted-unprivileged-user-namespaces
abi <abi/4.0>,
include <tunables/global>

%s flags=(unconfined) {
  userns,

  # Site-specific additions and overrides. See local/README for details.
  include if exists <local/%s>
}
`, rootlesskitBin, profileName)

	if err := os.WriteFile(profilePath, []byte(profile), 0644); err != nil {
		return fmt.Errorf("write apparmor profile %s: %w", profilePath, err)
	}

	cmd := exec.CommandContext(ctx, "apparmor_parser", "-r", profilePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("load apparmor profile %s: %w (output: %s)", profilePath, err, string(out))
	}
	logger.Info("installed rootlesskit apparmor profile", "profile", profileName)
	return nil
}

// userManagerState captures the unit's last observed state for attribution.
type userManagerState struct {
	// active is the last `systemctl is-active` output (e.g. "active", "failed").
	active string
	// result is the last `systemctl show -p Result --value` output when known.
	result string
}

// describe renders the state as the single-line attribution fragment used in
// error text: "unit user@1002.service: is-active=failed (Result=exit-code)".
func (s userManagerState) describe(unit string) string {
	msg := fmt.Sprintf("unit %s: is-active=%s", unit, s.active)
	if s.result != "" {
		msg += fmt.Sprintf(" (Result=%s)", s.result)
	}
	return msg
}

// fetchUserManagerState queries systemctl for the unit's is-active and Result
// properties. Best-effort: on query failure the fields carry whatever systemctl
// printed (its own error text still aids attribution).
func fetchUserManagerState(ctx context.Context, unit string) userManagerState {
	state := userManagerState{}
	if out, err := userManagerRunner(ctx, "systemctl", "is-active", unit); err != nil {
		state.active = strings.TrimSpace(string(out))
		if state.active == "" {
			state.active = "query-error: " + err.Error()
		}
	} else {
		state.active = strings.TrimSpace(string(out))
	}
	if out, err := userManagerRunner(ctx, "systemctl", "show", "-p", "Result", "--value", unit); err == nil {
		state.result = strings.TrimSpace(string(out))
	}
	return state
}

// startUserManagerUnit starts the unit. It returns the combined output alongside
// the error so the retry loop can attribute a failed attempt.
func startUserManagerUnit(ctx context.Context, unit string) ([]byte, error) {
	return userManagerRunner(ctx, "systemctl", "start", unit)
}

// countLingerEntries counts directory entries under the systemd linger
// directory. This is diagnostic only: a huge count (INT-CI-007: 8024 stale
// entries for 2 bunker users) means logind is churning on stale linger state
// and the HOST, not the agent, is starving the user-manager start.
func countLingerEntries() int {
	entries, err := os.ReadDir(lingerDir)
	if err != nil {
		return -1
	}
	return len(entries)
}

// userManagerJournalQueryTimeout bounds the best-effort journal query used to
// attribute a failed manager start. It is DETACHED from the caller's request
// deadline (context.WithoutCancel) so attribution still works when the client
// already gave up.
const userManagerJournalQueryTimeout = 5 * time.Second

// userManagerJournalMaxLen bounds the journal excerpt embedded in the
// attribution error. The excerpt is condensed to one line, so this is a
// character budget, not a line budget.
const userManagerJournalMaxLen = 400

// fetchUserManagerJournal returns a bounded, single-line excerpt of the unit's
// journal. The systemd job Result alone does not name the cause (INT-CI-008:
// the line that mattered was pam_systemd reporting the missing runtime
// directory). Best effort: journalctl first, systemctl status as the fallback,
// and an empty string when neither is readable — attribution must never turn a
// failed start into a different failure.
func fetchUserManagerJournal(ctx context.Context, unit string, logger *slog.Logger) string {
	queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), userManagerJournalQueryTimeout)
	defer cancel()
	out, err := userManagerRunner(queryCtx, "journalctl", "--no-pager", "-u", unit, "-n", "20")
	if err != nil && strings.TrimSpace(string(out)) == "" {
		if st, stErr := userManagerRunner(queryCtx, "systemctl", "status", "--no-pager", "-n", "20", unit); stErr == nil {
			return condenseJournal(string(st))
		}
		if logger != nil {
			logger.Debug("no journal available for user manager attribution", "unit", unit, "error", err)
		}
	}
	return condenseJournal(string(out))
}

// condenseJournal folds a journal excerpt into ONE line (fragments joined with
// " | ") and truncates it at a rune boundary, so the text can be embedded in an
// error string and in a log field without breaking either.
func condenseJournal(raw string) string {
	var parts []string
	for _, line := range strings.Split(raw, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			parts = append(parts, strings.Join(fields, " "))
		}
	}
	joined := strings.Join(parts, " | ")
	if len(joined) > userManagerJournalMaxLen {
		cut := userManagerJournalMaxLen
		for cut > 0 && !utf8.RuneStart(joined[cut]) {
			cut--
		}
		joined = strings.TrimSpace(joined[:cut]) + "..."
	}
	return joined
}

// ensureUserManagerRunning brings the systemd user manager up deterministically
// instead of waiting passively on the idempotent enable-linger. When the unit
// is already active it does nothing (the healthy spawn path gains zero extra
// start attempts). Otherwise it starts the unit, and on a failed start runs
// `systemctl reset-failed` and retries ONCE — a unit that previously failed
// stays in the "failed" state until reset, and systemd refuses plain starts of
// a failed unit. When the unit still is not active after the bounded attempts,
// it returns an attribution error naming the sub-stage (user-manager-start),
// the unit, its last observed state, and the host's linger-entry count, and
// emits exactly one WARN naming the same facts.
//
// The caller's context cancellation is honored: it is returned promptly (so a
// client that gave up does not wait out the full budget) but it is never
// passed off as the bring-up failure — the attribution error carries the real
// unit state instead of a bare "context canceled" (INT-CI-007).
func ensureUserManagerRunning(ctx context.Context, uid int, logger *slog.Logger) error {
	unit := userManagerUnitName(uid)

	if state := fetchUserManagerState(ctx, unit); state.active == "active" {
		return nil // healthy path: already up, zero start attempts
	}

	var lastState userManagerState
	for attempt := 1; attempt <= 2; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out, startErr := startUserManagerUnit(ctx, unit)
		if startErr == nil {
			lastState = fetchUserManagerState(ctx, unit)
			if lastState.active == "active" {
				return nil
			}
		} else {
			// Attribute the failed attempt: query the unit's Result (best
			// effort), then clear the failed state so the retry's start is
			// not refused for a unit stuck in the "failed" state.
			lastState = fetchUserManagerState(ctx, unit)
			if startErrMsg := strings.TrimSpace(string(out)); startErrMsg != "" {
				lastState.result = startErrMsg
			}
			_, _ = userManagerRunner(ctx, "systemctl", "reset-failed", unit)
		}
		if logger != nil {
			logger.Warn("user manager start attempt did not bring the unit active; retrying"+"\n",
				slog.Group("retry",
					slog.Int("attempt", attempt),
					slog.String("stage", userManagerStartStage),
				),
				"unit", unit,
				"is_active", lastState.active,
			)
		}
	}

	state := lastState
	if state.active == "" {
		state = fetchUserManagerState(ctx, unit)
	}
	lingerCount := countLingerEntries()
	// Attribute the real cause, not just the systemd job Result: the line that
	// names it (pam_systemd refusing XDG_RUNTIME_DIR, a missing runtime
	// directory, a failing ExecStart) never appears in systemctl's own output
	// (INT-CI-008).
	journal := fetchUserManagerJournal(ctx, unit, logger)
	if logger != nil {
		logger.Warn("systemd user manager did not start; host linger churn is a common cause",
			"unit", unit,
			"state", state.describe(unit),
			"linger_entries", lingerCount,
			"journal", journal,
		)
	}
	// errors.New (not fmt.Errorf): the journal excerpt is arbitrary host output
	// and must never be re-interpreted as a format string.
	msg := fmt.Sprintf("%s failed: %s: linger_entries=%d",
		userManagerStartStage, state.describe(unit), lingerCount)
	if journal != "" {
		msg += ": journal=" + journal
	}
	return errors.New(msg)
}

// waitForUserManager polls for the systemd user manager to create the runtime
// directory. ensureUserManagerRunning starts the manager deterministically
// before this is called; the poll is bounded by its OWN package-level budget
// (userManagerWaitTimeout, overridable via userManagerWaitTimeoutOverride in
// tests) and never inherits the caller's request deadline — a client deadline
// expiry must surface as an attributable user-manager error, not as a bare
// 300s "context deadline exceeded" (INT-CI-007). Caller cancellation is still
// honored promptly via ctx.Err().
func waitForUserManager(ctx context.Context, runtimeDir string) error {
	busPath := filepath.Join(runtimeDir, "bus")
	ticker := time.NewTicker(userManagerPollInterval)
	defer ticker.Stop()
	deadline := time.Now().Add(userManagerWaitBudget())
	// detached is set once the CALLER'S DEADLINE expires: from then on the
	// poll runs on its own budget. A caller deadline must never be inherited
	// as the reported cause (INT-CI-007: the 300s client deadline surfaced as
	// a bare "context deadline exceeded"); genuine Cancellation is still
	// honored promptly at every iteration.
	detached := false
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.Canceled {
				return ctx.Err()
			}
			detached = true
		case <-ticker.C:
		}
		if _, err := os.Stat(busPath); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %v waiting for %s (stage %s)",
				userManagerWaitBudget(), busPath, userManagerStartStage)
		}
		if detached {
			// The caller's Done channel is closed (deadline), so selecting on
			// it again would return instantly and spin the loop hot. Sleep
			// one poll interval instead; a later genuine cancel() surfaces at
			// the check below within one interval.
			time.Sleep(userManagerPollInterval)
			if ctx.Err() == context.Canceled {
				return ctx.Err()
			}
		}
	}
}
