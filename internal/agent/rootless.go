// Package agent provides rootless Docker setup helpers for unprivileged agents.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	if out, err := exec.CommandContext(ctx, "chown", "-R", username, binDir).CombinedOutput(); err != nil {
		return fmt.Errorf("chown bin dir: %w (output: %s)", err, string(out))
	}

	rootlessScript := filepath.Join(binDir, "dockerd-rootless.sh")
	if _, err := os.Stat(rootlessScript); err == nil {
		logger.Info("rootless docker already installed", "user", username, "path", rootlessScript)
		return nil
	}

	logger.Info("installing rootless docker", "user", username)

	u, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("lookup user %s: %w", username, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("parse uid %q: %w", u.Uid, err)
	}
	stdRuntimeDir := filepath.Join("/run", "user", strconv.Itoa(uid))

	// UIDs can be reused after userdel. A stale runtime directory from a previous
	// user breaks systemctl --user (it may be owned by a different user or have
	// an old user manager socket). Stop any existing user manager and remove the
	// stale directory before enabling lingering for this user.
	logger.Info("resetting user manager runtime", "user", username, "uid", uid, "runtime_dir", stdRuntimeDir)
	_ = exec.CommandContext(ctx, "systemctl", "stop", fmt.Sprintf("user@%d.service", uid)).Run()
	_ = exec.CommandContext(ctx, "loginctl", "terminate-user", strconv.Itoa(uid)).Run()
	removeMountsUnder(ctx, stdRuntimeDir, logger)
	if info, err := os.Stat(stdRuntimeDir); err == nil && info.IsDir() {
		if out, err := exec.CommandContext(ctx, "rm", "-rf", stdRuntimeDir).CombinedOutput(); err != nil {
			logger.Warn("failed to remove stale runtime dir", "dir", stdRuntimeDir, "error", err, "output", string(out))
		}
	}

	// Enable systemd lingering so the user manager is available for the
	// installer to run `systemctl --user start docker.service`. This must be done
	// as root before dropping to the target user. After enabling linger, wait for
	// the user manager to create the runtime directory.
	if out, err := exec.CommandContext(ctx, "loginctl", "enable-linger", username).CombinedOutput(); err != nil {
		return fmt.Errorf("enable linger for %s: %w (output: %s)", username, err, string(out))
	}
	// enable-linger is idempotent: when lingering is ALREADY recorded for the
	// user (CI hosts re-spawn the same explicit agent ID repeatedly) it takes
	// no start action, and the unit we stopped two steps above stays down. So
	// bring the manager up deterministically instead of waiting passively
	// (INT-CI-007: the passive wait burned the whole 300s request deadline).
	if err := ensureUserManagerRunning(ctx, uid, logger); err != nil {
		return err
	}
	if err := waitForUserManager(ctx, stdRuntimeDir); err != nil {
		return fmt.Errorf("user manager did not start for %s: %w", username, err)
	}

	// The installer uses systemctl --user, which requires a writable runtime
	// directory and the systemd user manager bus. Use the standard systemd user
	// runtime path for the install step; the actual daemon runtime is set to
	// /run/bunker/<id>/run when dockerd is started by the manager.
	if err := os.MkdirAll(stdRuntimeDir, 0700); err != nil {
		return fmt.Errorf("create runtime dir %s: %w", stdRuntimeDir, err)
	}
	// The runtime dir is created by logind as the agent user, so ownership is
	// already correct in the fresh path. Do NOT chown recursively: on
	// desktop-flavoured hosts the user manager mounts gvfsd-fuse at
	// /run/user/<uid>/gvfs, and a FUSE mount without allow_other denies even
	// root (chown -R / find -xdev both fail with "Permission denied"). A
	// non-recursive chown of the top-level dir never descends into the mount.
	if out, err := exec.CommandContext(ctx, "chown", username+":", stdRuntimeDir).CombinedOutput(); err != nil {
		return fmt.Errorf("chown runtime dir %s: %w (output: %s)", stdRuntimeDir, err, string(out))
	}

	// Download the installer into the agent's home as root (the installer will
	// be executed by the target user, and we need a reliable download path).
	installerPath := filepath.Join(userHome, "rootless-install.sh")
	curl := exec.CommandContext(ctx, "curl", "-fsSL", "-o", installerPath, rootlessInstallURL)
	if out, err := curl.CombinedOutput(); err != nil {
		return fmt.Errorf("download rootless installer: %w (output: %s)", err, string(out))
	}
	if err := os.Chmod(installerPath, 0755); err != nil {
		return fmt.Errorf("chmod installer: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "chown", username, installerPath).CombinedOutput(); err != nil {
		return fmt.Errorf("chown installer: %w (output: %s)", err, string(out))
	}

	// Run the installer as the target user. It installs binaries into ~/bin.
	// The standard systemd runtime directory and D-Bus bus address are provided
	// so systemctl --user can communicate with the user manager.
	cmd := exec.CommandContext(ctx, "su", "-", username, "-c", installerPath)
	cmd.Env = append(os.Environ(),
		"FORCE_ROOTLESS_INSTALL=1",
		"SKIP_IPTABLES=1",
		"XDG_RUNTIME_DIR="+stdRuntimeDir,
		"DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(stdRuntimeDir, "bus"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("run rootless installer as %s: %w (output: %s)", username, err, string(out))
	}

	if _, err := os.Stat(rootlessScript); err != nil {
		return fmt.Errorf("rootless install completed but %s is missing: %w", rootlessScript, err)
	}

	// The installer may have created files (config, install script, etc.) as
	// root. Chown the entire home directory to the agent user so userdel -r
	// can clean up cleanly during destroy.
	if out, err := exec.CommandContext(ctx, "chown", "-R", username+":", userHome).CombinedOutput(); err != nil {
		logger.Warn("failed to chown agent home after rootless install", "user", username, "error", err, "output", string(out))
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
	unit := fmt.Sprintf("user@%d.service", uid)

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
	if logger != nil {
		logger.Warn("systemd user manager did not start; host linger churn is a common cause",
			"unit", unit,
			"state", state.describe(unit),
			"linger_entries", lingerCount,
		)
	}
	return fmt.Errorf("%s failed: %s: linger_entries=%d",
		userManagerStartStage, state.describe(unit), lingerCount)
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
