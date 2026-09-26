//go:build !linux

// Platform seam (BFS-010): this build has no FUSE binding off Linux, and says so
// with a named error instead of failing to compile or mounting nothing. The
// Windows driver (WinFsp, driven from Go through cgofuse — BFS-003 §4) plugs in
// here; everything it needs already exists in internal/fsclient and in
// options.go, which are OS-neutral by construction. os is imported only to name
// the platform in the refusal.
package fsmount

import (
	"fmt"
	"runtime"
)

// Mount refuses on a platform without a binding. The signature is the Linux
// one's, so callers (the CLI) compile unchanged and the seam is a build tag
// rather than an interface with one implementation.
func Mount(opts Options) (*Mount, error) {
	if err := opts.Normalize(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w (this build is %s/%s)", ErrPlatformUnsupported, runtime.GOOS, runtime.GOARCH)
}

// Mount is the handle a caller unmounts and inspects. Off Linux it is never
// constructed; it exists so callers can name the type unconditionally.
type Mount struct{}

// Unmount is a no-op off Linux: there is nothing mounted.
func (m *Mount) Unmount() error { return nil }
