package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ── DF-BUNKER-72: stale RootlessKit state on a RECYCLED uid ─────────────────
//
// Live evidence (dogfood run 21, bunker-mvp): agent dfops0925c was spawned on
// uid 1007 — a uid a DESTROYED agent (bunker-conctest-3-91761) had owned
// before. `docker --version` worked, `docker ps` failed with
// `dial unix /run/bunker/dfops0925c/docker.sock: no such file`, and
// `journalctl --user -u docker.service` carried
//
//	[rootlesskit:parent] error: failed to lock
//	/run/user/1007/dockerd-rootless/lock, another RootlessKit is running with
//	the same state directory?
//
// (restart counter 3, unit failed). Clearing /run/user/1007/dockerd-rootless
// by hand and starting the unit fixed it, and the next spawn, on a FRESH uid
// (dfops0925d), started docker fine — the stale state directory was the only
// variable. `bunker info` reported `Status: running` the whole time.
//
// The state dir is `<userRuntimeBaseDir>/<uid>/dockerd-rootless`: RootlessKit's
// lock, netns and pid files. Nothing else on the fleet hunts it:
//
//   - manager_destroy.go Step 4a removes only /run/user/<uid>/docker.sock;
//   - `userdel` removes the user RECORD, never /run/user/<uid> (a tmpfs path
//     every future occupant of that numeric uid inherits);
//   - bringUpUserManager's stale-runtime reset (resetUserManagerState) only
//     fires when the runtime DIRECTORY itself is recognisably foreign —
//     unreadable, not a directory, owner unknown, or owned by a different uid
//     (classifyRuntimeDir). A uid recycled after `userdel` can keep
//     /run/user/<uid> owned by the SAME numeric uid, so the directory reads as
//     FRESH while a dead occupant's rootlesskit lock survives inside it. That
//     is exactly the dogfood-21 shape.
//
// So the scrub below is a SIDE step: it changes no argv, no unit property and
// no wire field, and on every healthy host it is a single lstat that finds
// nothing. It is bounded and best-effort on both call sites — a scrub failure
// must never fail a spawn or a destroy.

// dockerdRootlessStateDirName is the RootlessKit state directory name under the
// uid's systemd runtime directory. RootlessKit derives it from
// XDG_RUNTIME_DIR (dockerd-rootless.sh sets XDG_RUNTIME_DIR/dockerd-rootless),
// which is why the installer's docker.service — running under
// XDG_RUNTIME_DIR=/run/user/<uid> — locks THIS path, and why the manager's own
// systemd-run unit (XDG_RUNTIME_DIR=/run/bunker/<id>/run) is a different one.
const dockerdRootlessStateDirName = "dockerd-rootless"

// dockerdStateDirForUID derives the RootlessKit state directory for uid from
// the shared userRuntimeBaseDir seam, so a non-root test can point the whole
// derivation at a temp tree (the INT-CI-009 pattern) instead of /run/user.
func dockerdStateDirForUID(uid int) string {
	return filepath.Join(userRuntimeBaseDir, strconv.Itoa(uid), dockerdRootlessStateDirName)
}

// dockerProcessPattern is the pgrep -f pattern that identifies the processes
// which OWN a uid's RootlessKit state dir: rootlesskit itself (the lock's
// holder) and dockerd (including dockerd-rootless.sh, matched as a substring
// of the full command line).
const dockerProcessPattern = "rootlesskit|dockerd"

// uidDockerdProcessProbe reports whether uid owns a live rootlesskit/dockerd
// process. Package-level seam (mirroring userManagerRunner): a unit test must
// be able to assert BOTH arms — stale (no live owner → scrub) and live (owner
// running → never touch its state) — without racing the host's real process
// table.
var uidDockerdProcessProbe = probeUIDDockerdProcesses

// probeUIDDockerdProcesses is the production probe: `pgrep -u <uid> -f
// rootlesskit|dockerd`. pgrep exits 1 when nothing matches — that is a
// definitive "nothing alive", not an error. Any OTHER failure (pgrep missing,
// no permission, killed) means we could not tell, and callers must not read
// that as "nothing is running" (see scrubStaleDockerdStateDir: it fails
// closed).
func probeUIDDockerdProcesses(ctx context.Context, uid int) (bool, error) {
	out, err := exec.CommandContext(ctx, "pgrep", "-u", strconv.Itoa(uid), "-f", dockerProcessPattern).Output()
	if err == nil {
		return strings.TrimSpace(string(out)) != "", nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil // pgrep: no process matched
	}
	return false, err
}

// scrubStaleDockerdStateDir removes uid's RootlessKit state directory when —
// and only when — no process for that uid is still using it.
//
// Contract (both call sites rely on it):
//   - NEVER returns an error and never panics: spawn and destroy must not fail
//     because a scratch path could not be cleaned. A failure is a WARN.
//   - A live rootlesskit/dockerd keeps its state dir: deleting the lock out
//     from under a running daemon would break a working agent, which is worse
//     than the stale-lock bug this exists to fix.
//   - An UNOBSERVABLE process table also keeps the dir (fail closed, WARN): the
//     cost of not scrubbing is one failed docker unit, loud in its own journal;
//     the cost of scrubbing a live daemon's state is silent breakage.
//   - An absent directory (the ordinary fresh-uid spawn) is a single lstat and
//     nothing else — no subprocess.
func scrubStaleDockerdStateDir(ctx context.Context, uid int, logger *slog.Logger) {
	dir := dockerdStateDirForUID(uid)
	if _, err := os.Lstat(dir); err != nil {
		// Absent, or unreadable: there is nothing we can prove is scratch.
		return
	}
	alive, err := uidDockerdProcessProbe(ctx, uid)
	if err != nil {
		if logger != nil {
			logger.Warn("not scrubbing rootlesskit state dir: could not determine whether the uid still runs docker processes",
				"uid", uid, "dir", dir, "error", err)
		}
		return
	}
	if alive {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		if logger != nil {
			logger.Warn("failed to remove stale rootlesskit state dir", "uid", uid, "dir", dir, "error", err)
		}
		return
	}
	if logger != nil {
		logger.Info("removed stale rootlesskit state dir left by a previous occupant of the uid",
			"uid", uid, "dir", dir)
	}
}
