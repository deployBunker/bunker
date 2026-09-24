package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// ── INT-CI-035: logind teardown removes the runtime dir under the insurance ─
//
// CI job 106567227953 failed rootless-install with
//
//	chown runtime dir /run/user/1004: exit status 1 (output: chown: cannot
//	access /run/user/1004: No such file or directory)
//
// The insurance block runs AFTER bringUpUserManager verified the directory,
// but on a CI runner that reuses the uid across runs a concurrent logind
// teardown (loginctl terminate-user from a prior test rollback, or
// user@.service stop after a linger flip) can delete /run/user/<uid> in the
// window between the block's MkdirAll and its chown — the chown hits ENOENT
// and the whole spawn fails. The fix tolerates that window: recreate the
// directory once and re-run the SAME non-recursive chown.
//
// These tests drive ensureInstallRuntimeDir directly through the
// rootHostRunner seam (the same seam installFakeHost swaps); the directory
// work itself is real on-disk work — only the root-side chown is faked. No
// root, no systemd, no real user.

const (
	raceTestUID      = 1004
	raceTestUsername = "bunker-gap128"

	// The measured failure's CombinedOutput pair: exit status 1 with the
	// chown binary's ENOENT stderr (no wrapped fs.ErrNotExist anywhere).
	raceENOENTOut  = "chown: cannot access /run/user/1004: No such file or directory"
	raceENOENTText = "exit status 1"

	// A chown failure that is NOT the teardown race.
	otherChownOut  = "chown: changing ownership of /run/user/1004: Operation not permitted"
	otherChownText = "exit status 1"
)

// raceHost fakes the root-side runner and records the chown calls. The first
// chown call is scripted separately from the rest, so a test can model the
// teardown window closing (first chown ENOENT, retry healthy) or staying
// hostile (every chown fails).
type raceHost struct {
	uid      int
	username string
	dir      string

	// scriptErr, when non-nil, is returned for the FIRST chown call only.
	scriptErr error
	// scriptOut is the combined output paired with scriptErr.
	scriptOut string
	// persistentErr, when non-nil, is returned by every chown call after
	// the first scripted one (or by all of them when scriptErr is nil).
	persistentErr error
	// persistentOut is the combined output paired with persistentErr.
	persistentOut string

	// raceCalls, when > 0, makes the FIRST raceCalls chown calls return
	// the measured ENOENT teardown-race shape regardless of
	// scriptErr/persistentErr - a teardown window that outlasts earlier
	// retries (INT-CI-038). Zero keeps the legacy scripted behaviour.
	raceCalls int

	chownCalls int
}

func (h *raceHost) run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != "chown" {
		return nil, fmt.Errorf("raceHost: unexpected command %q", name)
	}
	h.chownCalls++
	// The load-bearing shape: exactly `chown <username>: <dir>`. A recursive
	// form (`chown -R ...`, three args) or a bare username must never appear.
	if len(args) != 2 || args[0] != h.username+":" {
		return nil, fmt.Errorf("raceHost: chown must be non-recursive <user>: <dir>, got %v", args)
	}
	if dir := args[len(args)-1]; dir != h.dir {
		return nil, fmt.Errorf("raceHost: chown of unexpected path %q", dir)
	}
	switch {
	case h.raceCalls > 0 && h.chownCalls <= h.raceCalls:
		return []byte(raceENOENTOut), errors.New(raceENOENTText)
	case h.chownCalls == 1 && h.scriptErr != nil:
		return []byte(h.scriptOut), h.scriptErr
	case h.persistentErr != nil:
		return []byte(h.persistentOut), h.persistentErr
	}
	return nil, nil
}

// setupRuntimeDir creates the directory chain and then simulates the logind
// teardown: the runtime dir is REMOVED before the helper runs. When
// parentReplacedByFile is set, the teardown took the parent too and left a
// regular file at its path, so any recreate attempt fails for real.
func (h *raceHost) setupRuntimeDir(t *testing.T, parentReplacedByFile bool) {
	t.Helper()
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(h.dir); err != nil {
		t.Fatal(err)
	}
	if parentReplacedByFile {
		runUser := filepath.Dir(h.dir)
		if err := os.Remove(runUser); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(runUser, []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// requireDirRemoved asserts the simulated teardown actually happened, so a
// fixture regression cannot silently pass the test without a race.
func requireDirRemoved(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("fixture precondition: runtime dir %s must be gone after the simulated teardown, Lstat err = %v", dir, err)
	}
}

func newRaceHost(t *testing.T) *raceHost {
	t.Helper()
	h := &raceHost{
		uid:      raceTestUID,
		username: raceTestUsername,
		dir:      filepath.Join(t.TempDir(), "run", "user", strconvItoa(raceTestUID)),
	}
	// Drive the helper through THIS host's runner (the established seam,
	// save/restore per rootless_userunit_test.go's install()); the real
	// runSystemCmd would invoke the actual chown binary as the test user.
	prev := rootHostRunner
	rootHostRunner = h.run
	t.Cleanup(func() { rootHostRunner = prev })
	return h
}

// strconvItoa exists only to keep the uid formatting next to the constants
// above without importing strconv for one call in newRaceHost.
func strconvItoa(uid int) string {
	return fmt.Sprintf("%d", uid)
}

// TestEnsureInstallRuntimeDir_TeardownRaceRetry covers the measured CI
// failure: the chown hits ENOENT because the runtime dir was removed between
// the MkdirAll and the chown. The helper must recreate the directory once,
// re-run the SAME non-recursive chown, and return success with the directory
// back on disk — the spawn must no longer fail.
func TestEnsureInstallRuntimeDir_TeardownRaceRetry(t *testing.T) {
	tests := map[string]struct {
		// firstErr is the error the first chown call returns. Both shapes
		// must classify as the teardown race: the measured failure is an
		// exec.ExitError from the chown BINARY (ENOENT phrase in the
		// captured output, no wrapped fs.ErrNotExist), while a direct
		// chown(2) wrapper would surface the same errno as a *fs.PathError.
		firstErr error
		// firstOut is the CombinedOutput paired with firstErr.
		firstOut string
	}{
		// The exact CI shape: exit status 1 + "chown: cannot access ...:
		// No such file or directory" in the output.
		"chown-binary-exit1-output-enoent": {
			firstErr: errors.New(raceENOENTText),
			firstOut: raceENOENTOut,
		},
		// A direct chown(2) wrapper surfaces the ENOENT as a wrapped
		// fs.ErrNotExist; the classification must accept that shape too.
		"wrapped-syscall-enoent": {
			firstErr: &fs.PathError{Op: "chown", Path: "/run/user/1004", Err: syscall.ENOENT},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h := newRaceHost(t)
			h.scriptErr = tc.firstErr
			h.scriptOut = tc.firstOut
			h.setupRuntimeDir(t, false)
			requireDirRemoved(t, h.dir)

			if err := ensureInstallRuntimeDir(context.Background(), h.username, h.uid, h.dir); err != nil {
				t.Fatalf("ensureInstallRuntimeDir() must ride out the teardown race, error = %v", err)
			}
			if h.chownCalls != 2 {
				t.Fatalf("expected exactly 2 chown calls (original + retry), got %d", h.chownCalls)
			}
			info, statErr := os.Lstat(h.dir)
			if statErr != nil {
				t.Fatalf("runtime dir %s must exist after the retry: %v", h.dir, statErr)
			}
			if !info.IsDir() {
				t.Fatalf("runtime dir %s is not a directory", h.dir)
			}
		})
	}
}

// TestEnsureInstallRuntimeDir_NonRaceChownFailureStaysLoud proves the retry
// does not swallow real failures: a chown error that is NOT the teardown race
// fails immediately (single call, no retry); a race whose RETRY chown also
// fails still fails the spawn loudly; and a teardown so deep that the
// recreate itself fails surfaces the recreate error instead of success.
func TestEnsureInstallRuntimeDir_NonRaceChownFailureStaysLoud(t *testing.T) {
	tests := map[string]struct {
		// scriptErr/scriptOut model the FIRST chown call.
		scriptErr error
		scriptOut string
		// persistentErr/persistentOut model every later chown call.
		persistentErr error
		persistentOut string
		// parentReplacedByFile makes the recreate MkdirAll fail for real.
		parentReplacedByFile bool
		// wantCalls is the exact number of chown calls the case must observe.
		wantCalls int
		// wantErrSubstr must appear in the returned error.
		wantErrSubstr string
	}{
		// Not the race (no ENOENT anywhere): must fail on the FIRST chown
		// with no retry attempt.
		"non-enoent-chown-failure": {
			persistentErr: errors.New(otherChownText),
			persistentOut: otherChownOut,
			wantCalls:     1,
			wantErrSubstr: "chown runtime dir",
		},
		// The race, but the retry chown also fails: the first call reports
		// the measured ENOENT, the recreate succeeds, and the second chown
		// fails with a different error — the spawn must still fail, loudly.
		"retry-chown-also-fails": {
			scriptErr:     errors.New(raceENOENTText),
			scriptOut:     raceENOENTOut,
			persistentErr: errors.New(otherChownText),
			persistentOut: otherChownOut,
			wantCalls:     2,
			wantErrSubstr: "chown runtime dir",
		},
		// The teardown removed the parent too: the recreate MkdirAll fails
		// before any retry chown, and the error must name the recreate.
		"recreate-blocked-parent-gone": {
			parentReplacedByFile: true,
			wantCalls:            0,
			wantErrSubstr:        "create runtime dir",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h := newRaceHost(t)
			h.scriptErr = tc.scriptErr
			h.scriptOut = tc.scriptOut
			h.persistentErr = tc.persistentErr
			h.persistentOut = tc.persistentOut
			h.setupRuntimeDir(t, tc.parentReplacedByFile)

			err := ensureInstallRuntimeDir(context.Background(), h.username, h.uid, h.dir)
			if err == nil {
				t.Fatal("ensureInstallRuntimeDir() must fail when the retry cannot recover")
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Fatalf("error %q must name %q", err, tc.wantErrSubstr)
			}
			if h.chownCalls != tc.wantCalls {
				t.Fatalf("expected exactly %d chown calls, got %d", tc.wantCalls, h.chownCalls)
			}
		})
	}
}

// TestEnsureInstallRuntimeDir_TeardownRaceConverges covers the INT-CI-038
// upgrade: the INT-CI-035 insurance retried the ENOENT teardown race exactly
// ONCE, and CI run 35978846999 measured a teardown window that outlasted that
// single retry (the second chown died on the same ENOENT and the spawn
// failed). The helper now converges across the same bounded budget as the
// bring-up path (runtimeDirOwnershipAttempts), so a teardown hostile through
// attempt 2 must succeed on attempt 3, and a teardown hostile through every
// attempt must still fail the spawn loudly — with the attempt count, the
// ENOENT output, and the mount verdict in the error.
func TestEnsureInstallRuntimeDir_TeardownRaceConverges(t *testing.T) {
	tests := map[string]struct {
		// raceCalls is how many consecutive chown calls return the
		// measured ENOENT teardown-race shape before the window closes.
		raceCalls int
		// wantErrSubstrings asserts the failure shape; nil means success.
		wantErrSubstrings []string
	}{
		// The measured CI shape: the teardown window outlasts the legacy
		// single retry (attempts 1 and 2 hit ENOENT) and closes before
		// attempt 3 — the spawn must ride it out with exactly
		// runtimeDirOwnershipAttempts chown calls, where the OLD code
		// hard-failed after 2.
		"hostile-through-2-succeeds-on-3": {
			raceCalls: 2,
		},
		// A teardown that outlasts the whole budget: every chown hits the
		// race, the helper must fail LOUDLY after exactly
		// runtimeDirOwnershipAttempts attempts — never loop unbounded,
		// never fail after the legacy single attempt.
		"hostile-through-budget-fails-loudly": {
			raceCalls:         runtimeDirOwnershipAttempts,
			wantErrSubstrings: []string{"after", "convergence attempts", "No such file or directory"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h := newRaceHost(t)
			h.raceCalls = tc.raceCalls
			h.setupRuntimeDir(t, false)
			requireDirRemoved(t, h.dir)

			err := ensureInstallRuntimeDir(context.Background(), h.username, h.uid, h.dir)
			if tc.wantErrSubstrings == nil {
				if err != nil {
					t.Fatalf("ensureInstallRuntimeDir() must converge once the teardown window closes, error = %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("ensureInstallRuntimeDir() must fail loudly when the teardown outlasts the whole convergence budget")
				}
				for _, want := range tc.wantErrSubstrings {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error %q must carry %q", err, want)
					}
				}
			}
			if h.chownCalls != runtimeDirOwnershipAttempts {
				t.Fatalf("expected exactly %d chown calls (bounded convergence budget), got %d",
					runtimeDirOwnershipAttempts, h.chownCalls)
			}
			if tc.wantErrSubstrings == nil {
				info, statErr := os.Lstat(h.dir)
				if statErr != nil || !info.IsDir() {
					t.Fatalf("runtime dir %s must exist as a directory after convergence: %v", h.dir, statErr)
				}
			}
		})
	}
}
