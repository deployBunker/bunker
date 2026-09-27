//go:build !unix

// The Windows arm's assertions (BFS-028).
//
// THESE CANNOT BE EXECUTED IN THIS ENVIRONMENT, and saying so is the point: there
// is no Windows host here, so what is verified is that the file COMPILES and
// type-checks for the platform it is written for (`GOOS=windows go vet
// ./internal/cli`, see docs/evidence/BFS-028-windows-umount-seam.md §5 for the
// exact run and the pre-existing blocker that has to be excluded from it). This is
// the convention internal/fsmount/platform_unsupported_test.go uses for its
// non-Linux path.
//
// THE HALF THAT CAN BE EXECUTED ON LINUX IS EXECUTED: the refusal is driven
// through the real runUmount and the real cobra command by
// umount_platform_test.go, which replaces only the platform's ANSWER (the
// umountPlatformRefusal var) and leaves everything else — the gate, the resolution
// order, the side effects — the production code. That file is the reachability
// proof; THIS file is the proof that the arm wired into a Windows build returns
// what the reachability proof exercised, with NO substitution.
//
// What these cells would prove on a Windows host:
//   - platformUmountRefusal() is the named refusal, carrying THIS build's
//     GOOS/GOARCH, the command it refuses and a way out (no generic "unsupported");
//   - runUmount, with the seam untouched, refuses BEFORE resolving anything and
//     writes no success text;
//   - isMountPoint refuses instead of answering "not a mountpoint", which is the
//     fabricated answer that would let the caller delete a live mountpoint.
package cli

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// TestPlatformRefusalIsThisBuildsOwn is the arm's core contract, with no
// substitution anywhere: the value runUmount's gate reads on this platform.
func TestPlatformRefusalIsThisBuildsOwn(t *testing.T) {
	err := platformUmountRefusal()
	if err == nil {
		t.Fatalf("platformUmountRefusal returned nil on %s/%s: a build with no unmount mechanism must refuse, and a nil here is what makes the gate a no-op",
			runtime.GOOS, runtime.GOARCH)
	}
	if !errors.Is(err, ErrUmountUnsupported) {
		t.Errorf("the arm's refusal does not wrap ErrUmountUnsupported, so a caller cannot match it: %v", err)
	}
	for _, want := range []string{runtime.GOOS, runtime.GOARCH, "bunker umount"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the arm's refusal does not name %q: %q", want, err)
		}
	}
}

// TestRunUmountRefusesWithThePlatformDefault: the WIRING, not the value. It calls
// runUmount exactly as the cobra command does — no seam substituted — so a build
// where the gate was forgotten, or where the arm returned nil, fails here.
func TestRunUmountRefusesWithThePlatformDefault(t *testing.T) {
	var out bytes.Buffer
	err := runUmount(&out, "f0901fd3", false)

	if err == nil {
		t.Fatalf("runUmount reported success on %s/%s (output %q); the accidental behaviour on a platform with no kernel mount table to read is a clean report over a mount it cannot see",
			runtime.GOOS, runtime.GOARCH, out.String())
	}
	if !errors.Is(err, ErrUmountUnsupported) {
		t.Errorf("runUmount's refusal does not wrap ErrUmountUnsupported: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("runUmount wrote %q before refusing; the refusal must precede any report and any side effect", out.String())
	}
}

// TestIsMountPointRefusesRatherThanFabricatingNotAMountpoint: (false, nil) here
// would be a FABRICATED answer — "this path is not a mountpoint" — and it is the
// answer the clean path acts on by removing the directory. On a platform that
// cannot compare device ids there is no truthful answer, so the probe must error.
func TestIsMountPointRefusesRatherThanFabricatingNotAMountpoint(t *testing.T) {
	dir := t.TempDir()

	mounted, err := isMountPoint(dir)
	if err == nil {
		t.Fatalf("isMountPoint(%s) answered (mounted=%v) on a platform that cannot tell; a fabricated answer is worse than a refusal", dir, mounted)
	}
	if mounted {
		t.Errorf("isMountPoint returned mounted=true alongside an error; a caller checking only the bool must not be able to act")
	}
	if !errors.Is(err, ErrUmountUnsupported) {
		t.Errorf("isMountPoint's refusal is not matchable: %v", err)
	}
}

// TestUmountCommandIsRegisteredAndRefuses: the command exists in this build (it is
// not `//go:build ignore`-ed away) and refuses through the same cobra path
// cmd/bunker/main.go runs, which prints `bunker: <err>` and exits 1.
func TestUmountCommandIsRegisteredAndRefuses(t *testing.T) {
	cmd := NewUmountCommand()
	if cmd == nil || cmd.RunE == nil {
		t.Fatal("NewUmountCommand returned no runnable command: the command must stay in the build and refuse, not disappear")
	}
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"f0901fd3"})

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("`bunker umount` exited successfully on %s/%s; output was %q", runtime.GOOS, runtime.GOARCH, out.String())
	}
	if !errors.Is(err, ErrUmountUnsupported) {
		t.Errorf("the command's error is not the platform refusal: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("the command printed %q before refusing", out.String())
	}
}
