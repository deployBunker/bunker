//go:build !linux

// Platform seam (BFS-010). This build has no FUSE binding off Linux, and it says
// so with a named error rather than failing to compile or pretending to mount.
//
// WHY THIS FILE IS NOT THE SEAM'S FIRST VERSION. The file that first claimed this
// seam declared both `func Mount(...)` and `type Mount struct{}` in one scope and
// called a different function (Mount) than the one its callers use (MountAt), so
// `GOOS=windows go build ./...` did not compile it at all — and the CLI, which
// calls MountAt and the accessors below, could not have compiled against it
// either. The full finding is in docs/evidence/BFS-010-windows-mint-decision.md;
// the guard that would have caught it is probes/cross-GOOS-build.sh. A seam that
// is only ever compiled on one platform has not been checked.
//
// THE SHAPE, and why it is this shape:
//
//   - MountAt is the ONLY exported way to obtain a *Mount, and off Linux it
//     always returns (nil, refusal). Every other exported name below exists so
//     that a caller (internal/cli/fs.go) compiles unchanged on a platform with no
//     binding; none of them fabricates a mount fact.
//   - The Linux-only accessors (Server, Client, Cache, Snapshot,
//     ReaddirPlusNegotiated, WriteStatusNow, NotifySupport, …) are DELIBERATELY
//     ABSENT. A future Windows binding that reaches for one gets a compile error
//     — loud, at build time — instead of a nil dereference or a zero value it
//     might believe.
//   - The refusal happens BEFORE any local side effect: no mountpoint is created,
//     no cache directory, no bind preflight, no server contact. A user on an
//     unsupported platform gets one sentence and their disk untouched.
package fsmount

import (
	"fmt"
	"runtime"

	"github.com/deployBunker/bunker/internal/fsclient"
)

// MountAt refuses on this platform: there is no binding to hand the client
// (internal/fsclient) to the OS with.
//
// The refusal is the named sentinel ErrPlatformUnsupported wrapped with the
// platform it refuses on, so the message names the OS rather than leaving the
// user to guess. It is reached only AFTER the caller's own options are validated:
// a missing --url is reported as a missing --url and never as "unsupported
// platform", so a user with a typo is not sent to the wrong fix.
func MountAt(opts Options) (*Mount, error) {
	if err := opts.Normalize(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w — this build is %s/%s, and a mount needs a binding for it",
		ErrPlatformUnsupported, runtime.GOOS, runtime.GOARCH)
}

// Mount is the handle a caller mounts, unmounts and inspects. Off Linux MountAt
// never returns one, so every method below is unreachable in a caller that reads
// MountAt's error; they exist for signature parity with the Linux binding's
// handle, so the CLI compiles on both platforms unchanged.
//
// Unmount — the one method whose contract can carry a refusal — REFUSES instead
// of returning nil, so a caller that ignored MountAt's error cannot believe it
// detached a filesystem. The accessors return the zero value ("" and an EMPTY
// status document), which is the honest reading: there is no mountpoint, no cache
// directory and no mount to report on. They never invent a verdict.
type Mount struct{}

// Unmount refuses: there is nothing mounted on this platform.
func (m *Mount) Unmount() error {
	return fmt.Errorf("%w — there is no mount to unmount", ErrPlatformUnsupported)
}

// Wait returns immediately: nothing was mounted, so there is no mount lifetime to
// wait for. (On Linux this blocks until the kernel detaches the mount.)
func (m *Mount) Wait() {}

// Mountpoint is "" — no mountpoint was created.
func (m *Mount) Mountpoint() string { return "" }

// CacheDir is "" — no cache directory was created.
func (m *Mount) CacheDir() string { return "" }

// Status is the EMPTY status document, because no mount exists to report on. The
// refusal from MountAt is the answer a caller must act on; this document carries
// no verdict, no cache figure and no transport state.
func (m *Mount) Status() fsclient.Status { return fsclient.Status{} }
