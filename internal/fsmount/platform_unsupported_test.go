//go:build !linux

// The unsupported-platform seam's assertions (BFS-010).
//
// THESE CANNOT BE EXECUTED IN THIS ENVIRONMENT, and saying so is the point: there
// is no Windows host, so what is verified here is that the file COMPILES and
// type-checks for the platform it is written for —
// `GOOS=windows go vet ./internal/fsmount` (CI lane: probes/cross-GOOS-build.sh).
// This is the same convention internal/hostsetup/owner_nonunix_test.go uses for
// its non-unix path. The half of the contract that CAN be executed on Linux — that
// the refusal's text is classified PERMANENT by the mount-driver layer, so an
// unsupported platform is never retried in a loop — is pinned in
// internal/mountdriver/platform_refusal_test.go, which runs on Linux.
//
// What these tests would prove on a Windows host: MountAt refuses with a named
// error that names the platform, returns NO mount handle, touches nothing on
// disk, and never masks the caller's own configuration error as "unsupported
// platform".
package fsmount

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/fsclient"
)

func mountableOptions(t *testing.T) (Options, string) {
	t.Helper()
	mp := filepath.Join(t.TempDir(), "mnt")
	return Options{Mountpoint: mp, BaseURL: "http://127.0.0.1:18481/dav"}, mp
}

// TestMountAtRefusesWithANamedError is the seam's core contract: a named refusal,
// no handle, and no local side effect.
func TestMountAtRefusesWithANamedError(t *testing.T) {
	opts, mp := mountableOptions(t)

	m, err := MountAt(opts)
	if err == nil {
		t.Fatalf("MountAt reported success on %s/%s: there is no binding for this platform", runtime.GOOS, runtime.GOARCH)
	}
	if m != nil {
		t.Errorf("MountAt returned a mount handle (%+v) alongside an error; a caller that checks only the handle must not be able to proceed", m)
	}
	if !errors.Is(err, ErrPlatformUnsupported) {
		t.Errorf("the refusal does not wrap ErrPlatformUnsupported, so a caller cannot match it: %v", err)
	}
	if !strings.Contains(err.Error(), runtime.GOOS) || !strings.Contains(err.Error(), runtime.GOARCH) {
		t.Errorf("the refusal must name the platform it refuses on; got %q, want it to contain %s/%s",
			err, runtime.GOOS, runtime.GOARCH)
	}

	// No side effect: the refusal happens before PrepareMountpoint/MountDir, so a
	// user on an unsupported platform has nothing created and nothing to clean up.
	if _, statErr := os.Stat(mp); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("MountAt left something at the mountpoint %s (stat err=%v); the refusal must precede any local write", mp, statErr)
	}
}

// TestMountAtDoesNotMaskTheCallersOwnError: a malformed request is reported as
// itself. Were the platform refusal returned first, a user with a missing --url
// would be told to install something instead of fixing their command line.
func TestMountAtDoesNotMaskTheCallersOwnError(t *testing.T) {
	opts, _ := mountableOptions(t)
	opts.BaseURL = ""

	_, err := MountAt(opts)
	if err == nil {
		t.Fatal("MountAt accepted a request with no endpoint")
	}
	if errors.Is(err, ErrPlatformUnsupported) {
		t.Errorf("a missing --url was reported as a platform refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "--url") {
		t.Errorf("the option error should name the option at fault; got %q", err)
	}
}

// TestUnmountRefusesRatherThanReportingSuccess: Unmount's contract is the one
// place a nil return could be read as "a filesystem was detached". It must refuse.
func TestUnmountRefusesRatherThanReportingSuccess(t *testing.T) {
	var m Mount
	err := m.Unmount()
	if err == nil {
		t.Fatal("Unmount on a build with no binding returned nil, which reads as a successful unmount")
	}
	if !errors.Is(err, ErrPlatformUnsupported) {
		t.Errorf("Unmount's refusal must be matchable: %v", err)
	}
}

// TestAccessorsAreEmptyNotFabricated: the accessors exist for signature parity, and
// what they report is the absence of a mount — never a plausible-looking mount fact
// or a status document carrying a verdict.
func TestAccessorsAreEmptyNotFabricated(t *testing.T) {
	var m Mount
	if got := m.Mountpoint(); got != "" {
		t.Errorf("Mountpoint() = %q on a platform with no mount; want the empty string", got)
	}
	if got := m.CacheDir(); got != "" {
		t.Errorf("CacheDir() = %q on a platform with no mount; want the empty string", got)
	}
	st := m.Status()
	if st != (fsclient.Status{}) {
		t.Errorf("Status() invented a document on a platform with no mount: %+v", st)
	}
	if st.Transport.Verdict != "" {
		t.Errorf("Status().Transport.Verdict = %q; an empty document carries no verdict", st.Transport.Verdict)
	}
	// Wait() must return: there is no mount lifetime to wait for. A blocking Wait
	// here would be the hang this seam exists to prevent.
	done := make(chan struct{})
	go func() { m.Wait(); close(done) }()
	select {
	case <-done:
	default:
		t.Error("Wait() blocks on a platform where nothing can be mounted")
	}
}
