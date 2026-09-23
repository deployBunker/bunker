// Package agent provides rootless Docker setup helpers for unprivileged agents.
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

// systemdUnitDirRoot is the root of systemd's unit/drop-in tree. The per-agent
// cgroup limits are written as <root>/user-<uid>.slice.d/50-bunker.conf (see
// applyUserSliceLimits) and the recycled-uid recovery resets that same
// directory for the uid it is about to reuse (AC3 of DF-BUNKER-21). Var for the
// same reason as lingerDir: a non-root unit test must be able to point the
// slice plane at a temp tree instead of the real /etc/systemd/system.
var systemdUnitDirRoot = "/etc/systemd/system"

// subUIDPath / subGIDPath are the subordinate-id databases configureSubIDs
// maintains for rootless Docker's uid mapping. Vars for the same reason as
// userRuntimeBaseDir: the spawn path must be drivable end to end without
// writing into the host's /etc.
var (
	subUIDPath = "/etc/subuid"
	subGIDPath = "/etc/subgid"
)

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
// visible before the installer retry (INT-CI-009). It is the SCRIPT of that
// command; the command string the session receives is built by
// userManagerReloadSessionCmd, which carries the session bus environment
// IN-BAND (INT-SPAWN-004).
const userManagerReloadCmd = "systemctl --user daemon-reload"

// ── the session bus environment is delivered IN-BAND (INT-SPAWN-004) ───────
//
// The daemon runs commands inside the agent user's session with
// `su - <user> -c <script>`. A LOGIN shell started by `su -` RESETS the
// environment, so anything set on the `su` PROCESS never reaches the command
// the session actually runs. Measured on the demo host, as root, both ways:
//
//	XDG_RUNTIME_DIR=/run/user/1002 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1002/bus \
//	    su - kara -c 'echo XDG=[$XDG_RUNTIME_DIR] DBUS=[$DBUS_SESSION_BUS_ADDRESS]'
//	→ XDG=[] DBUS=[]                        (the environment was stripped)
//
//	systemd-run --quiet --wait --pipe --unit=probe su - kara -c \
//	    'echo XDG=[$XDG_RUNTIME_DIR] DBUS=[$DBUS_SESSION_BUS_ADDRESS]'
//	→ XDG=[/run/user/1001] DBUS=[unix:path=/run/user/1001/bus]
//
// pam_systemd only supplies the bus environment when the caller is NOT already
// inside a logind session, so an ssh-launched daemon gets nothing while a
// transient service does — and `su -` discards what the daemon set either way.
// The agent session therefore had NEITHER variable and systemctl answered
// "Failed to connect to bus: No medium found", killing every spawn of a daemon
// that was not launched by systemd at stage rootless-install. A readiness
// retry cannot fix that: no amount of waiting makes a stripped variable come
// back.
//
// The delivery that survives the login shell's reset is an IN-BAND assignment:
// the two variables are assigned INSIDE the command the session runs (measured
// working on the same host). ONE construction serves every user-session
// command — the reachability probe, the pre-retry daemon-reload and the
// rootless installer — so they all speak to the same manager (INT-CI-009) and
// all survive the reset (INT-SPAWN-004).

// rootlessInstallerToggles are the rootless installer's own switches. They are
// delivered IN-BAND for the same reason the bus environment is: the installer
// reads them from its environment, and the login shell's reset would otherwise
// leave FORCE_ROOTLESS_INSTALL/SKIP_IPTABLES unset in the installer process.
const (
	rootlessInstallerForceEnv     = "FORCE_ROOTLESS_INSTALL=1"
	rootlessInstallerSkipIptables = "SKIP_IPTABLES=1"
)

// sessionEnvAssignments renders the in-band shell assignments that carry the
// session bus environment into the command the agent's login shell runs: the
// agent's standard systemd runtime directory and the user manager's bus socket
// address. Both values are DERIVED from the runtime directory the caller
// passes, so the construction works for any uid. Values are single-quoted (see
// shellQuote) so a path carrying spaces, quotes or shell metacharacters can
// neither break the command nor expand inside it. extra assignments are
// appended verbatim: they are caller-owned constants (the installer toggles),
// not caller-supplied data.
func sessionEnvAssignments(runtimeDir string, extra ...string) []string {
	assignments := []string{
		"XDG_RUNTIME_DIR=" + shellQuote(runtimeDir),
		"DBUS_SESSION_BUS_ADDRESS=" + shellQuote("unix:path="+filepath.Join(runtimeDir, "bus")),
	}
	return append(assignments, extra...)
}

// sessionEnvPrefix renders the in-band assignments as the shell prefix of a
// session command: one `export` statement, terminated by the separator that
// introduces the caller's script.
func sessionEnvPrefix(runtimeDir string, extra ...string) string {
	return "export " + strings.Join(sessionEnvAssignments(runtimeDir, extra...), " ") + "; "
}

// sessionCommand is the ONE construction of a command string that the agent
// user's login shell must run: the in-band bus environment (plus any caller
// toggles) followed by the caller's script content BYTE-IDENTICAL. Nothing is
// prepended to, quoted around or rewritten inside the script, so the command
// string is the script the caller wrote, with an environment assignment in
// front of it that the login shell applies before the script runs.
func sessionCommand(runtimeDir, script string, extra ...string) string {
	return sessionEnvPrefix(runtimeDir, extra...) + script
}

// shellQuote renders value as a POSIX single-quoted shell word: everything
// inside is literal, and the closing quote is broken only to insert an escaped
// literal quote for values that contain one.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// userManagerReloadSessionCmd is the user-manager daemon-reload as the agent
// session must run it: the session bus environment IN-BAND, then the reload
// command. Both the reachability probe and the pre-retry daemon-reload use
// this SAME construction.
func userManagerReloadSessionCmd(runtimeDir string) string {
	return sessionCommand(runtimeDir, userManagerReloadCmd)
}

// rootlessInstallerSessionCmd is the installer execution as the agent session
// must run it: the session bus environment IN-BAND plus the installer's own
// toggles, then the installer path byte-identical.
func rootlessInstallerSessionCmd(runtimeDir, installerPath string) string {
	return sessionCommand(runtimeDir, installerPath, rootlessInstallerForceEnv, rootlessInstallerSkipIptables)
}

// userSessionRunner executes a command inside the agent user's session.
// Package-level seam mirroring userManagerRunner. Its `script` argument is the
// COMPLETE command string — callers build it with sessionCommand (through
// userManagerReloadSessionCmd / rootlessInstallerSessionCmd), which is what
// makes the in-band environment observable to a test that fakes this seam.
var userSessionRunner = runUserSessionCmd

// userSessionEnv builds the environment for a command that runs inside the
// agent user's session: the standard systemd runtime directory and the user
// manager's bus socket layered on top of the inherited environment.
//
// This is BELT-AND-BRACES, not the delivery mechanism (INT-SPAWN-004): a login
// shell started by `su -` resets the environment, so anything set on the `su`
// PROCESS never reaches the command the session runs. What the session depends
// on is the in-band assignment inside the command string itself
// (sessionCommand). The environment is kept because it is harmless and helps
// in the one shape where it survives — a daemon already inside a logind
// session — but no behavior may depend on it.
func userSessionEnv(runtimeDir string) []string {
	return append(os.Environ(),
		"XDG_RUNTIME_DIR="+runtimeDir,
		"DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(runtimeDir, "bus"),
	)
}

// runUserSessionCmd is the production runner: `su - <username> -c <script>`
// with the session environment ALSO applied to the su process. script is the
// complete session command (sessionCommand-built), so the bus environment it
// needs is in-band and survives the login shell's environment reset; the
// process environment is redundant insurance (see userSessionEnv).
func runUserSessionCmd(ctx context.Context, username, runtimeDir, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "su", "-", username, "-c", script)
	cmd.Env = userSessionEnv(runtimeDir)
	return cmd.CombinedOutput()
}

// rootlessInstallerRunner executes the downloaded rootless installer as the
// agent user. Its `script` argument is the COMPLETE installer session command
// (rootlessInstallerSessionCmd). Package-level seam so the daemon-reload retry
// (INT-CI-009) is testable without a real su, real systemd, or network.
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

// rootlessInstallerCacheDir is the host-level directory where downloaded
// rootless installers are cached between spawns. It is a var so operators can
// override it and tests can point it at a temp directory. The empty string
// means "no host cache"; the production install path falls back to the legacy
// uncached download seam when the cache is not configured.
var rootlessInstallerCacheDir = ""

// RootlessInstallerCacheDirEnv is the environment variable the daemon reads at
// startup to arm rootlessInstallerCacheDir. It overrides the
// agent.rootless_installer_cache_dir config value (env > config > default);
// an empty or unset value never clobbers the config file.
const RootlessInstallerCacheDirEnv = "BUNKER_ROOTLESS_INSTALLER_CACHE_DIR"

// SetRootlessInstallerCacheDir arms (or disarms, with "") the host-level
// rootless installer cache. cmd/bunkerd calls it once at startup from the
// resolved configuration; tests use it to point the cache at a temp directory
// through the same seam production uses. The empty string keeps the previous
// behavior exactly: every spawn downloads the installer from the network.
func SetRootlessInstallerCacheDir(dir string) {
	rootlessInstallerCacheDir = dir
}

// GetRootlessInstallerCacheDir reports the currently armed cache directory
// ("" = no host cache). Read seam for the cmd/bunkerd startup tests.
func GetRootlessInstallerCacheDir() string {
	return rootlessInstallerCacheDir
}

// rootlessInstallerCacheKey returns the cache filename for the current
// rootless installer URL. The filename is derived from the host so a future
// URL change automatically creates a new cache entry instead of reusing a
// stale artifact.
func rootlessInstallerCacheKey() string {
	return "rootless-installer-" + hostCacheSuffix(rootlessInstallURL) + ".sh"
}

// hostCacheSuffix returns a filesystem-safe suffix derived from url. It is
// package-level so the implementation is testable without a real URL.
func hostCacheSuffix(url string) string {
	s := strings.TrimPrefix(url, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ".", "-")
	s = strings.ReplaceAll(s, ":", "-")
	return s
}

// cachedRootlessInstallerDownload is the cached download seam. It returns the
// installer bytes from cache on hit, or downloads, validates, and populates
// the cache on miss. A partial or invalid cache entry is treated as a miss
// and refreshed. Caller cancellation during download does not leave a
// valid-looking cache artifact.
var cachedRootlessInstallerDownload = downloadRootlessInstallerCached

// downloadRootlessInstallerCached is the production cached downloader.
func downloadRootlessInstallerCached(ctx context.Context, installerPath string) ([]byte, error) {
	if err := os.MkdirAll(rootlessInstallerCacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create installer cache dir: %w", err)
	}
	cachePath := filepath.Join(rootlessInstallerCacheDir, rootlessInstallerCacheKey())

	// Cache hit: validate before trusting.
	if cached, err := os.ReadFile(cachePath); err == nil {
		if validationErr := validateCachedInstaller(cachePath, cached); validationErr == nil {
			if out, copyErr := copyFile(cachePath, installerPath); copyErr != nil {
				return nil, fmt.Errorf("copy cached installer to %s: %w (output: %s)", installerPath, copyErr, string(out))
			}
			return cached, nil
		}
		// Partial or invalid cache entry: remove it so a later call retries
		// cleanly instead of hitting the same poisoned artifact.
		_ = os.Remove(cachePath)
	}

	// Cache miss: download to a temp file so a cancelled/interrupted write
	// never leaves a partial artifact at the final cache path.
	tmp, err := os.CreateTemp(rootlessInstallerCacheDir, "rootless-installer-*.sh.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temp installer file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	if out, err := rootlessInstallerDownload(ctx, tmpPath); err != nil {
		return nil, fmt.Errorf("download rootless installer: %w (output: %s)", err, string(out))
	}
	downloaded, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("read downloaded installer: %w", err)
	}
	if validationErr := validateCachedInstaller(tmpPath, downloaded); validationErr != nil {
		return nil, fmt.Errorf("downloaded installer failed validation: %w", validationErr)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return nil, fmt.Errorf("chmod temp installer: %w", err)
	}
	if err := os.Rename(tmpPath, cachePath); err != nil {
		return nil, fmt.Errorf("promote temp installer to cache: %w", err)
	}
	_ = os.Remove(tmpPath)
	defer func(path string) { _ = os.Remove(path) }(tmpPath)

	if out, copyErr := copyFile(cachePath, installerPath); copyErr != nil {
		return nil, fmt.Errorf("copy downloaded installer to %s: %w (output: %s)", installerPath, copyErr, string(out))
	}
	return downloaded, nil
}

// validateCachedInstaller performs the minimum sanity checks on a downloaded
// rootless installer before it is executed or cached. It rejects:
//   - files that are not regular files;
//   - files smaller than the minimum viable size;
//   - files whose leading bytes do not look like a shell script.
//
// The checks are intentionally conservative: they are designed to catch
// interrupted downloads, HTML error pages, and empty/truncated artifacts
// without depending on network access or the installer's own semantics.
func validateCachedInstaller(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat cached installer: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cached installer is not a regular file: %s", path)
	}
	if info.Size() < 8 {
		return fmt.Errorf("cached installer is too small (%d bytes): %s", info.Size(), path)
	}
	if len(data) == 0 {
		return errors.New("cached installer is empty")
	}
	trimmed := bytes.TrimLeft(data, " 	\n\r")
	if !bytes.HasPrefix(trimmed, []byte("#!")) {
		return errors.New("cached installer does not start with a shebang")
	}
	return nil
}

// copyFile copies src to dst using an atomic replace where the platform
// supports it. It returns the combined output of any failure, matching the
// error shape callers already expect from rootHostRunner.
func copyFile(src, dst string) ([]byte, error) {
	in, err := os.Open(src)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() {
		_ = out.Close()
	}()

	if _, err := io.Copy(out, in); err != nil {
		return nil, fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	if err := out.Close(); err != nil {
		return nil, fmt.Errorf("close %s: %w", dst, err)
	}
	return nil, nil
}

// invalidateCachedRootlessInstaller removes the cached installer for the
// current URL. It is best-effort: a missing cache is a no-op. Callers use it
// when they know the cached artifact can no longer be trusted (e.g. after a
// partial download that escaped the temp-file path).
func invalidateCachedRootlessInstaller() error {
	if rootlessInstallerCacheDir == "" {
		return nil
	}
	path := filepath.Join(rootlessInstallerCacheDir, rootlessInstallerCacheKey())
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return os.Remove(path)
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

// rootlessInstallerCommand builds the installer execution as the production
// runner must issue it: `su - <username> -c <script>` where script is the
// COMPLETE installer session command built by rootlessInstallerSessionCmd (bus
// environment AND the installer's own toggles in-band). The process environment
// is kept belt-and-braces (see userSessionEnv): the login shell resets it, so the
// session depends on the in-band assignment, never on cmd.Env (INT-SPAWN-004).
//
// Split from the runner so the process-group wiring (DF-BUNKER-21) is assertable
// against the real command the installer path builds.
func rootlessInstallerCommand(ctx context.Context, username, runtimeDir, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "su", "-", username, "-c", script)
	cmd.Env = append(userSessionEnv(runtimeDir),
		rootlessInstallerForceEnv,
		rootlessInstallerSkipIptables,
	)
	return cmd
}

// runRootlessInstallerCmd runs the installer (see rootlessInstallerCommand) in
// its OWN process group so a context cancellation takes the whole installer
// subtree down (DF-BUNKER-21), not just `su`.
func runRootlessInstallerCmd(ctx context.Context, username, runtimeDir, script string) ([]byte, error) {
	return runInOwnProcessGroup(ctx, rootlessInstallerCommand(ctx, username, runtimeDir, script))
}

// runInOwnProcessGroup runs cmd in its OWN process group and, when ctx is done,
// kills the WHOLE group — not just the direct child, which is all
// exec.CommandContext's default cancellation signals.
//
// WHY (DF-BUNKER-21): the rootless installer is `su - <user> -c <installer>`,
// and the work that matters happens in ITS children — the `curl` fetching ~90MB
// of docker-ce-rootless-extras, the extracted install script, rootlesskit. On
// the QA host the spawn failed 2/2 while that download was at ~1.7MB/s and the
// 300s request deadline expired; killing only `su` orphaned those children as
// the agent user. They kept running (and writing) while the rollback tried to
// remove that very user, holding the home directory and the user manager busy —
// which is precisely what makes `userdel -r` exit 8 with "user is currently used
// by process". SIGKILL to the group ends the subtree at the same instant the
// request gives up.
//
// SAFETY: Setpgid gives the child a group of its own, so the negative-pid kill
// reaches only processes this command started. The daemon, its siblings, and
// the test binary (which is in a different group) are never signalled. Both
// halves are platform-specific (see procgroup_unix.go / procgroup_nonunix.go):
// on a platform without POSIX process groups the setup is a no-op and the
// cancellation kills the direct child instead — the installer it guards is
// Linux-only, so that build is a portability fallback.
func runInOwnProcessGroup(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	setOwnProcessGroup(cmd)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killProcessGroup(cmd)
	}
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

// configureSubIDs ensures /etc/subuid and /etc/subgid contain a subordinate-ID
// mapping for the given username. Rootless Docker needs a contiguous 65,536
// UID/GID range per user; since GAP-140 that range is allocated from a global
// pool and is guaranteed disjoint from every other name's range, and both
// files are edited under one host-wide advisory lock with the read-choose-write
// sequence inside it (see subid_alloc.go for why per-user start=uid overlapped
// for every pair of agents).
//
// The username is resolved through userLookup so the caller's failure mode is
// unchanged from the pre-GAP-140 path: an unresolvable user still refuses the
// spawn before any file is touched.
func configureSubIDs(ctx context.Context, username string) error {
	if _, err := userLookup(username); err != nil {
		return fmt.Errorf("lookup user %s: %w", username, err)
	}

	release, err := lockSubIDs()
	if err != nil {
		return err
	}
	defer release()

	if err := ensureSubIDAllocation(subUIDPath, username); err != nil {
		return fmt.Errorf("subuid: %w", err)
	}
	if err := ensureSubIDAllocation(subGIDPath, username); err != nil {
		return fmt.Errorf("subgid: %w", err)
	}
	return nil
}

// lockSubIDs takes the host-wide advisory lock that serializes subordinate-ID
// allocation, waiting at most subIDLockTimeout for it. The lock is one file for
// BOTH databases because an agent's subuid and subgid blocks are a single unit
// of policy: reading them under separate locks would let two spawns interleave
// and hand the same block to two names.
//
// The wait deliberately does NOT use a blocking flock(2) from each waiter. A
// kernel-blocked flock is uninterruptible: if the holder dies between "lock
// taken" and "writer started" it never releases, and nothing on this side can
// break out. Instead each waiter retries a non-blocking attempt, so every
// iteration re-opens the lock file and re-validates the holder is still there —
// a stale lock file with no live holder resolves immediately. A timed-out wait
// is a hard error (the spawn fails, nothing is written); it is never a silent
// fallback to appending without the lock, which is exactly the TOCTOU this
// function exists to close.
func lockSubIDs() (func(), error) {
	if err := os.MkdirAll(subIDLockDir, 0o755); err != nil {
		return nil, fmt.Errorf("create subid lock dir %s: %w", subIDLockDir, err)
	}
	lockPath := filepath.Join(subIDLockDir, subIDLockFileName)
	deadline := time.Now().Add(subIDLockTimeout)
	for {
		release, err := lockSubIDFileAttempt(lockPath, true)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("subid allocation lock %s held by another process for more than %s: %w",
				lockPath, subIDLockTimeout, err)
		}
		time.Sleep(subIDLockRetryInterval)
	}
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

	// Satisfy the HOST prerequisites the official installer assumes exist.
	// Without them the installer fails in ways that are hard to read — or,
	// worse, succeeds and leaves a daemon that cannot map uids or route
	// container traffic. Each prerequisite is tried as: already present -> OS
	// package (per distribution family) -> prebuilt static binary -> source. A
	// required miss aborts with every strategy that was tried and why; an
	// optional miss only warns and the install continues.
	//
	// This is the fix for "the first spawn on a fresh host died because
	// newuidmap was missing": the failure used to surface as an opaque
	// installer error the operator had to diagnose by hand.
	if outcomes, err := EnsureRootlessPrerequisites(ctx, PrereqOptions{Apply: true, Logger: logger}); err != nil {
		rendered := make([]string, 0, len(outcomes))
		for _, o := range outcomes {
			rendered = append(rendered, o.String())
		}
		return fmt.Errorf("rootless prerequisites for %s: %w (host prerequisite report: %s)",
			username, err, strings.Join(rendered, "; "))
	}

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
	if err := ensureInstallRuntimeDir(ctx, username, uid, stdRuntimeDir); err != nil {
		return err
	}

	// Download the installer into the agent's home as root (the installer will
	// be executed by the target user, and we need a reliable download path).
	// A host-level cache is consulted first; on miss the installer is downloaded,
	// validated, cached, and then copied to the per-install path.
	installerPath := filepath.Join(userHome, "rootless-install.sh")
	if out, err := cachedRootlessInstallerDownload(ctx, installerPath); err != nil {
		// Fall back to the legacy uncached download seam when the cache path
		// is unavailable or disabled. This preserves the existing behavior
		// for operators who have not configured a cache directory.
		if rootlessInstallerCacheDir == "" {
			if out, err := rootlessInstallerDownload(ctx, installerPath); err != nil {
				return fmt.Errorf("download rootless installer: %w (output: %s)", err, string(out))
			}
		} else {
			return fmt.Errorf("download rootless installer: %w (output: %s)", err, string(out))
		}
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
	// IN-BAND inside the command the session runs (rootlessInstallerSessionCmd),
	// together with the installer's own toggles, so `su -`'s environment reset
	// cannot strip them (INT-SPAWN-004).
	//
	// The installer writes the docker.service unit and then starts it in the
	// same run. When the manager has not yet observed the freshly written unit,
	// that start races the unit lookup and fails with "Unit docker.service not
	// found" even though the manager itself is healthy (INT-CI-009). That
	// failure is retried ONCE after a user-session daemon-reload makes the new
	// unit visible. Any other installer failure is returned untouched.
	installerCmd := rootlessInstallerSessionCmd(stdRuntimeDir, installerPath)
	installerOut, installerErr := rootlessInstallerRunner(ctx, username, stdRuntimeDir, installerCmd)
	if installerErr != nil {
		if !isUserUnitNotFound(installerOut) {
			return fmt.Errorf("run rootless installer as %s: %w (output: %s)", username, installerErr, string(installerOut))
		}
		if logger != nil {
			logger.Warn("rootless installer hit the unit-not-found race; reloading the user manager and retrying once",
				"user", username, "unit", userManagerUnitName(uid),
				"output", condenseJournal(string(installerOut)))
		}
		// The SAME construction and the SAME seam call as the reachability
		// probe above, so the reload before the retry speaks to the manager
		// through the identical command string (single construction,
		// INT-CI-009 + INT-SPAWN-004).
		if _, err := probeUserManagerReachable(ctx, username, stdRuntimeDir); err != nil {
			// The reload itself failed: retrying the installer through a dead
			// bus would only reproduce the same not-found failure, so fail
			// with the full attribution instead.
			state := fetchUserManagerState(ctx, userManagerUnitName(uid))
			return fmt.Errorf("rootless-install: user-session daemon-reload before the installer retry failed for %s: %w; %s; linger entries: %d; installer output: %s",
				username, err, state.describe(userManagerUnitName(uid)), countLingerEntries(), condenseJournal(string(installerOut)))
		}
		installerOut, installerErr = rootlessInstallerRunner(ctx, username, stdRuntimeDir, installerCmd)
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

// ensureInstallRuntimeDir is the install-step insurance for the agent's
// standard systemd runtime directory (/run/user/<uid>): MkdirAll, then a
// NON-RECURSIVE chown to the agent user.
//
// The runtime dir is created by logind as the agent user, so ownership is
// already correct in the fresh path. Do NOT chown recursively: on
// desktop-flavoured hosts the user manager mounts gvfsd-fuse at
// /run/user/<uid>/gvfs, and a FUSE mount without allow_other denies even
// root (chown -R / find -xdev both fail with "Permission denied"). A
// non-recursive chown of the top-level dir never descends into the mount.
//
// bringUpUserManager created and ownership-verified this directory BEFORE the
// manager start (INT-CI-008), but a concurrent logind teardown for the same
// uid (loginctl terminate-user from a prior rollback, or user@.service stop
// after a linger flip on a CI runner that reuses the uid) can remove the
// directory in the window between this function's MkdirAll and its chown; the
// chown then hits ENOENT and the whole spawn fails ("chown runtime dir
// /run/user/<uid>: exit status 1 (output: chown: cannot access ...: No such
// file or directory)", INT-CI-035). On that ENOENT the directory is recreated
// once and the same non-recursive chown re-run; only a second failure fails
// the spawn.
func ensureInstallRuntimeDir(ctx context.Context, username string, uid int, stdRuntimeDir string) error {
	if err := os.MkdirAll(stdRuntimeDir, 0700); err != nil {
		return fmt.Errorf("create runtime dir %s: %w", stdRuntimeDir, err)
	}
	out, err := rootHostRunner(ctx, "chown", username+":", stdRuntimeDir)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error()+" "+string(out), "No such file or directory") {
		return fmt.Errorf("chown runtime dir %s: %w (output: %s)", stdRuntimeDir, err, string(out))
	}
	// The runtime dir vanished between the MkdirAll and the chown (logind
	// teardown for the reused uid). Recreate it once and re-run the SAME
	// non-recursive chown; do not widen this into a recursive repair.
	if err := os.MkdirAll(stdRuntimeDir, 0700); err != nil {
		return fmt.Errorf("create runtime dir %s after teardown race: %w", stdRuntimeDir, err)
	}
	if out, err := rootHostRunner(ctx, "chown", username+":", stdRuntimeDir); err != nil {
		return fmt.Errorf("chown runtime dir %s: %w (output: %s)", stdRuntimeDir, err, string(out))
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
//
// The command string is built by userManagerReloadSessionCmd, which carries the
// session bus environment IN-BAND: `su -` resets the environment, so a probe
// whose bus variables only existed on the su process could never reach the
// manager (INT-SPAWN-004). The pre-retry daemon-reload in installRootlessDocker
// calls THIS function, so both reloads are the same construction and the same
// seam call.
func probeUserManagerReachable(ctx context.Context, username, runtimeDir string) ([]byte, error) {
	return userSessionRunner(ctx, username, runtimeDir, userManagerReloadSessionCmd(runtimeDir))
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
	// Ownership extraction is platform-specific (statOwnerUID): a platform that
	// exposes no POSIX owner reports it as UNKNOWN, and classifyRuntimeDir fails
	// SAFE on an unknown owner (it counts as stale).
	if owner, ok := statOwnerUID(info); ok {
		st.owner = owner
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
//
// DF-BUNKER-21 (AC3): the SLICE the previous occupant of a recycled uid left
// behind is reset here too — the slice unit is stopped and its drop-in removed
// (see resetStaleUserSlice). The manager stop alone does not touch either: a
// uid recycled from a destroyed agent keeps /etc/systemd/system/user-<uid>.slice.d
// with the OLD agent's limits, which would then be enforced against the NEW
// agent, and logind keeps the slice loaded for a uid that no longer has its own
// manager.
func resetUserManagerState(ctx context.Context, uid int, runtimeDir string, logger *slog.Logger) {
	_, _ = userManagerRunner(ctx, "systemctl", "stop", userManagerUnitName(uid))
	_, _ = userManagerRunner(ctx, "loginctl", "terminate-user", strconv.Itoa(uid))
	resetStaleUserSlice(ctx, uid, logger)
	removeMountsUnder(ctx, runtimeDir, logger)
	_, _ = userManagerRunner(ctx, "systemctl", "stop", userRuntimeDirUnitName(uid))
	if _, err := os.Stat(runtimeDir); err == nil {
		if out, err := userManagerRunner(ctx, "rm", "-rf", runtimeDir); err != nil && logger != nil {
			logger.Warn("failed to remove stale runtime dir",
				"dir", runtimeDir, "error", err, "output", strings.TrimSpace(string(out)))
		}
	}
}

// runtimeDirOwnershipAttempts / runtimeDirOwnershipPause bound the
// runtime-directory CONVERGENCE loop. A foreign owner observed right after an
// accepted chown is not a permissions error: it is a concurrent actor
// replacing the path — logind's user-runtime-dir@<uid>.service transitioning
// while a recycled uid changes hands (INT-CI-019: the failing spawn died 30ms
// into the stage, before any installer ran, while the SAME run brought other
// agents up on the same uid). The guarantee is therefore "converge with a few
// bounded re-assertions", never "give up on the first probe". The whole budget
// (~100ms) is negligible against the 300s request deadline; it must stay
// SMALL — this is a bounded loop, never an unbounded retry.
const (
	runtimeDirOwnershipAttempts = 3
	runtimeDirOwnershipPause    = 50 * time.Millisecond
)

// runtimeDirMountInfoPath is the kernel mount table read for the exhaustion
// attribution. Var so tests can point the attribution at a fixture table
// instead of the host's mounts.
var runtimeDirMountInfoPath = "/proc/self/mountinfo"

// runtimeDirMountVerdict reports whether path ITSELF is a mount point in
// /proc/self/mountinfo, naming the filesystem type and source when it is. An
// unreadable table is reported as unknown, never as "no".
func runtimeDirMountVerdict(path string) string {
	data, err := os.ReadFile(runtimeDirMountInfoPath)
	if err != nil {
		return fmt.Sprintf("mount point: unknown (cannot read %s: %v)", runtimeDirMountInfoPath, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[4] != path {
			continue
		}
		// The optional fields follow the mount options up to a lone "-", then
		// the filesystem type and its source.
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+2 >= len(fields) {
			return "mount point: yes"
		}
		return fmt.Sprintf("mount point: yes (fstype %s source %s)", fields[sep+1], fields[sep+2])
	}
	return "mount point: no"
}

// verifyRuntimeDir returns nil when one probe result satisfies the bring-up
// contract, or the loud error to report for that outcome. The messages are
// deliberately the pre-existing ones — the ownership one keeps the greppable
// "runtime dir %s is owned by uid %d, expected %d" fragment.
func verifyRuntimeDir(dir string, info runtimeDirInfo, probeErr error, uid int) error {
	switch {
	case probeErr != nil:
		return fmt.Errorf("verify runtime dir %s after creation: %w", dir, probeErr)
	case !info.exists || !info.isDir:
		return fmt.Errorf("runtime dir %s is missing after creation", dir)
	case !info.ownerKnown:
		return fmt.Errorf("runtime dir %s ownership cannot be verified", dir)
	case info.owner != uint32(uid):
		return fmt.Errorf("runtime dir %s is owned by uid %d, expected %d", dir, info.owner, uid)
	}
	return nil
}

// waitRuntimeDirOwnershipRetry waits pause, or returns the context error as
// soon as the caller's context is done, so a cancelled or expired spawn aborts
// the convergence loop instead of sleeping through its remaining attempts.
func waitRuntimeDirOwnershipRetry(ctx context.Context, pause time.Duration) error {
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
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
//
// The verification CONVERGES instead of shooting once (INT-CI-019): each
// attempt re-asserts ownership (MkdirAll + non-recursive chown) and re-probes,
// so a transient foreign owner — a concurrent replacement of the path by
// logind, which is what produced "runtime dir /run/user/<uid> is owned by uid
// 0, expected <uid>" on a host where the same uid came up healthy seconds
// later — is logged and absorbed rather than turned into a failed spawn. The
// loop is bounded (runtimeDirOwnershipAttempts) and aborts immediately when ctx
// is done. On exhaustion the failure still fails the spawn (fail-safe: a
// manager started without a correctly-owned runtime dir can never become
// active), now carrying the mount-point verdict for that path.
func ensureUserRuntimeDir(ctx context.Context, username string, uid int, stdRuntimeDir string, logger *slog.Logger) error {
	if out, err := userManagerRunner(ctx, "systemctl", "start", userRuntimeDirUnitName(uid)); err != nil && logger != nil {
		logger.Warn("systemd user-runtime-dir unit did not start; creating the runtime dir directly",
			"unit", userRuntimeDirUnitName(uid), "dir", stdRuntimeDir,
			"error", err, "output", strings.TrimSpace(string(out)))
	}
	var lastErr error
	for attempt := 1; attempt <= runtimeDirOwnershipAttempts; attempt++ {
		if attempt > 1 {
			if err := waitRuntimeDirOwnershipRetry(ctx, runtimeDirOwnershipPause); err != nil {
				return fmt.Errorf("re-assert ownership of runtime dir %s: %w", stdRuntimeDir, err)
			}
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
		info, probeErr := runtimeDirProbe(stdRuntimeDir)
		if lastErr = verifyRuntimeDir(stdRuntimeDir, info, probeErr, uid); lastErr == nil {
			return nil
		}
		if attempt < runtimeDirOwnershipAttempts && logger != nil {
			logger.Warn("runtime dir ownership not applied yet; re-asserting",
				"dir", stdRuntimeDir, "owner", ownerField(info), "expected", uid,
				"attempt", attempt, "attempts", runtimeDirOwnershipAttempts,
				"error", lastErr)
		}
	}
	return fmt.Errorf("%w after %d ownership attempts (%s)",
		lastErr, runtimeDirOwnershipAttempts, runtimeDirMountVerdict(stdRuntimeDir))
}

// ownerField renders the observed owner for the convergence WARN: the uid when
// the probe could read it, "unknown" when it could not.
func ownerField(info runtimeDirInfo) any {
	if !info.ownerKnown {
		return "unknown"
	}
	return info.owner
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
