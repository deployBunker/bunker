package agent

// DF-BUNKER-72: the RootlessKit state directory of a RECYCLED uid.
//
// Live evidence (dogfood run 21, bunker-mvp): agent dfops0925c was spawned on
// uid 1007 — a uid a DESTROYED agent had owned — and its docker daemon never
// came up:
//
//	[rootlesskit:parent] error: failed to lock
//	/run/user/1007/dockerd-rootless/lock, another RootlessKit is running with
//	the same state directory?
//
// while `bunker info` reported `Status: running`. The stale state dir was the
// only variable: clearing it by hand fixed the unit, and the next spawn on a
// fresh uid started docker fine.
//
// These tests pin the two filesystem planes of the fix against temp trees (the
// INT-CI-009 seam pattern: userRuntimeBaseDir + the runner seams), so no root,
// no real systemd, no real agent user and no real /run/user are involved:
//
//   - A1: the SPAWN path scrubs a planted stale state dir, and does so BEFORE
//     the installer starts docker.service (observed at installer time);
//   - A2: the DESTROY path scrubs one for a uid with no live processes;
//   - the guard arms: a LIVE rootlesskit keeps its state dir, and an
//     unobservable process table keeps it too (fail closed).

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
)

// dfb72PlantStateDir creates the state dir RootlessKit leaves behind — its
// lock plus its pid file, the two artefacts the dogfood-21 journal named —
// under parent (the uid's runtime directory) and returns the dir.
func dfb72PlantStateDir(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, dockerdRootlessStateDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rootlesskit.pid"), []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// dfb72SwapProcessProbe swaps the uid-liveness seam so a test can assert both
// guard arms (nothing alive, something alive) without racing the host's real
// process table.
func dfb72SwapProcessProbe(t *testing.T, fn func(context.Context, int) (bool, error)) {
	t.Helper()
	prev := uidDockerdProcessProbe
	uidDockerdProcessProbe = fn
	t.Cleanup(func() { uidDockerdProcessProbe = prev })
}

// dfb72StateDirAbsent asserts the state dir is gone (not merely re-typed).
func dfb72StateDirAbsent(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("rootlesskit state dir %s survived the scrub (stat error = %v, want ENOENT)", dir, err)
	}
}

// ── A1: SPAWN PATH ──────────────────────────────────────────────────────────

// TestDFBUNKER72_SpawnPathScrubsStaleRootlesskitStateDir drives the real
// installRootlessDocker over the userUnitFakeHost seams and asserts the
// planted state dir is gone, in the two shapes that matter:
//
//   - nothing alive for the uid → the scrub removes it;
//   - a live rootlesskit/dockerd → the scrub leaves it (deleting a running
//     daemon's lock would break a WORKING agent, which is worse than the bug);
//   - an unobservable process table → the scrub leaves it (fail closed).
//
// The fixture deliberately reports the runtime DIRECTORY as owned by the uid
// that is about to use it (h.ownership.owner = uid). That is the dogfood-21
// shape and it is what makes this test non-vacuous: classifyRuntimeDir sees a
// FRESH directory, so the pre-existing stale-runtime reset
// (resetUserManagerState, which rm -rf's the whole directory) never runs and
// the planted state dir can only disappear through the new scrub.
func TestDFBUNKER72_SpawnPathScrubsStaleRootlesskitStateDir(t *testing.T) {
	cases := []struct {
		name        string
		probe       func(context.Context, int) (bool, error)
		wantRemoved bool
	}{
		{
			name:        "no live rootlesskit for the uid",
			probe:       func(context.Context, int) (bool, error) { return false, nil },
			wantRemoved: true,
		},
		{
			name:        "a live rootlesskit owns the state dir",
			probe:       func(context.Context, int) (bool, error) { return true, nil },
			wantRemoved: false,
		},
		{
			name:        "the process table cannot be read",
			probe:       func(context.Context, int) (bool, error) { return false, os.ErrPermission },
			wantRemoved: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newUserUnitHost(t)
			h.ownership.owner = uint32(h.uid)
			home := h.userHome(t)
			h.install(t, t.TempDir())
			dfb72SwapProcessProbe(t, tc.probe)

			stateDir := dfb72PlantStateDir(t, h.runtimeDir)

			// ORDER: the installer is what writes ~/.config/systemd/user/
			// docker.service and starts it, so reading the filesystem AT
			// INSTALLER TIME is the observation that proves the scrub ran
			// before docker.service could ever try to take the lock.
			prevInstaller := rootlessInstallerRunner
			stateAtInstaller := "installer-never-ran"
			rootlessInstallerRunner = func(ctx context.Context, username, runtimeDir, script string) ([]byte, error) {
				if _, err := os.Lstat(stateDir); err == nil {
					stateAtInstaller = "present"
				} else {
					stateAtInstaller = "absent"
				}
				return prevInstaller(ctx, username, runtimeDir, script)
			}
			t.Cleanup(func() { rootlessInstallerRunner = prevInstaller })

			if err := installRootlessDocker(context.Background(), h.username, home, uuLogger()); err != nil {
				t.Fatalf("installRootlessDocker() error = %v%s", err, h.callLog())
			}

			if stateAtInstaller == "installer-never-ran" {
				t.Fatalf("the installer never ran; the ordering premise is unproven:%s", h.callLog())
			}

			if tc.wantRemoved {
				dfb72StateDirAbsent(t, stateDir)
				if stateAtInstaller != "absent" {
					t.Errorf("state dir was still %s when the installer ran — the scrub must precede the docker.service start", stateAtInstaller)
				}
				return
			}
			if _, err := os.Lstat(stateDir); err != nil {
				t.Fatalf("state dir %s was removed although %q (stat error = %v)", stateDir, tc.name, err)
			}
			if stateAtInstaller != "present" {
				t.Errorf("state dir was %s at installer time, want present (nothing may touch it in this arm)", stateAtInstaller)
			}
		})
	}
}

// ── A2: DESTROY PATH ────────────────────────────────────────────────────────

// dfb72DestroyFixture builds a manager whose destroy path can run end to end
// unprivileged, points the uid's runtime tree at a temp dir, and resolves the
// agent user to uid.
func dfb72DestroyFixture(t *testing.T, agentID string, uid int) (*AgentManager, string) {
	t.Helper()
	m, cfg, _ := newDestroyArchiveFixture(t)
	root, _ := foreignHome(t, agentID)
	pointHomeAt(t, root)
	cfg.Agent.DestroyArchiveDir = t.TempDir()
	cfg.Agent.DestroyHomePolicy = config.DestroyPolicyArchive
	registerArchiveAgent(t, m, agentID)

	runtimeBase := t.TempDir()
	swapStringSeam(t, &userRuntimeBaseDir, runtimeBase)

	// The destroy path resolves the uid BEFORE userdel (Step 1) — that is the
	// value step 4b scrubs for, so the seam must resolve the user here.
	prevLookup := lookupUser
	lookupUser = func(name string) (*user.User, error) {
		if name != "bunker-"+agentID {
			return nil, os.ErrNotExist
		}
		return &user.User{Uid: strconv.Itoa(uid), Gid: strconv.Itoa(uid), Username: name}, nil
	}
	t.Cleanup(func() { lookupUser = prevLookup })

	return m, filepath.Join(runtimeBase, strconv.Itoa(uid))
}

// TestDFBUNKER72_DestroyPathScrubsStaleRootlesskitStateDir closes the hygiene
// gap at the source: a destroy leaves /run/user/<uid> behind (userdel removes
// the user RECORD, not the tmpfs path), so the next agent that recycles that
// uid inherits whatever RootlessKit left. Step 4a only removes the socket; this
// pins that the state dir goes too — and that it is NOT touched while something
// for the uid is still alive.
func TestDFBUNKER72_DestroyPathScrubsStaleRootlesskitStateDir(t *testing.T) {
	const uid = 61007

	t.Run("uid with no live processes loses its stale state dir", func(t *testing.T) {
		m, uidRuntimeDir := dfb72DestroyFixture(t, "df72des", uid)
		dfb72SwapProcessProbe(t, func(context.Context, int) (bool, error) { return false, nil })
		stateDir := dfb72PlantStateDir(t, uidRuntimeDir)

		resp, err := m.Destroy(context.Background(), "df72des", false)
		if err != nil {
			t.Fatalf("Destroy() error = %v", err)
		}
		if resp.GetStatus() != "destroyed" {
			t.Fatalf("Destroy() status = %q, want destroyed", resp.GetStatus())
		}
		dfb72StateDirAbsent(t, stateDir)
	})

	t.Run("live process for the uid keeps the state dir", func(t *testing.T) {
		m, uidRuntimeDir := dfb72DestroyFixture(t, "df72kept", uid)
		dfb72SwapProcessProbe(t, func(context.Context, int) (bool, error) { return true, nil })
		stateDir := dfb72PlantStateDir(t, uidRuntimeDir)

		if _, err := m.Destroy(context.Background(), "df72kept", false); err != nil {
			t.Fatalf("Destroy() error = %v", err)
		}
		if _, err := os.Lstat(stateDir); err != nil {
			t.Fatalf("state dir %s was removed while a process for the uid was still alive (stat error = %v)", stateDir, err)
		}
	})

	// The destroy path must not have become a synchronous poll on a host
	// without pgrep: the unobservable arm is asserted here too, since a destroy
	// of a BUSY uid is exactly the case where the probe can fail.
	t.Run("unobservable process table keeps the state dir", func(t *testing.T) {
		m, uidRuntimeDir := dfb72DestroyFixture(t, "df72unobs", uid)
		dfb72SwapProcessProbe(t, func(context.Context, int) (bool, error) {
			return false, context.DeadlineExceeded
		})
		stateDir := dfb72PlantStateDir(t, uidRuntimeDir)

		if _, err := m.Destroy(context.Background(), "df72unobs", false); err != nil {
			t.Fatalf("Destroy() error = %v", err)
		}
		if _, err := os.Lstat(stateDir); err != nil {
			t.Fatalf("state dir %s was removed although the process table could not be read (stat error = %v)", stateDir, err)
		}
	})
}

// TestDFBUNKER72_ProbeIsProductionShaped pins the default liveness probe's
// CONTRACT, because the whole fail-closed behavior of the scrub hangs off the
// distinction between pgrep's two failure shapes:
//
//   - "nothing matched" (exit 1) is a definitive false and must NOT be an
//     error, or every scrub on a quiet host would log a warning;
//   - any other failure (missing binary, no permission, killed) is an error,
//     and the scrub then leaves the state dir alone.
//
// It runs the REAL pgrep against a uid no process can own (out of the 32-bit
// uid range), which is the only arm reachable unprivileged without racing the
// host's process table.
func TestDFBUNKER72_ProbeIsProductionShaped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	alive, err := probeUIDDockerdProcesses(ctx, 0x7FFFFFF)
	if err != nil {
		t.Fatalf("probe for a uid with no processes = error %v, want (false, nil): pgrep's no-match exit status is a definitive \"nothing alive\", not a probe failure", err)
	}
	if alive {
		t.Fatalf("probe for a uid with no processes reported %v, want false", alive)
	}
}
