//go:build unix

package hostsetup

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// shimFileInfo is an os.FileInfo whose Sys() carries something other than this
// platform's stat structure — the shape a synthetic, wrapped or proxied stat
// has. It is how the "ownership is unobservable" branch is reachable on a unix
// host.
type shimFileInfo struct {
	os.FileInfo
	sys any
}

func (s shimFileInfo) Sys() any { return s.sys }

// TestOwnerStringReportsTheObservedOwner pins the unix extraction: a real file
// reports the owner the kernel reports, in the same "uid:gid" shape the
// readiness checks compare against "0:0".
func TestOwnerStringReportsTheObservedOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is masked by the umask, so set it explicitly — the
	// permission assertion below must not depend on the caller's umask.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	uid, gid, ok := platformOwner(fi)
	if !ok {
		t.Fatal("platformOwner() did not report ownership for a real file")
	}
	if int(uid) != os.Getuid() || int(gid) != os.Getgid() {
		t.Errorf("platformOwner() = %d:%d, want %d:%d", uid, gid, os.Getuid(), os.Getgid())
	}
	want := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if got := ownerString(fi); got != want {
		t.Errorf("ownerString() = %q, want %q", got, want)
	}

	owner, mode, ok := statOwnerMode(path)
	if !ok {
		t.Fatal("statOwnerMode() did not report a file that exists")
	}
	if owner != want || mode != 0o644 {
		t.Errorf("statOwnerMode() = (%q, %04o), want (%q, 0644)", owner, mode, want)
	}
	if _, _, ok := statOwnerMode(filepath.Join(t.TempDir(), "absent")); ok {
		t.Error("statOwnerMode() reported a file that does not exist")
	}
}

// TestUnobservableOwnershipIsNeverTrusted keeps the two halves of the boundary
// together: when the platform (or a shim) cannot supply an owner, the rendering
// is EMPTY and an empty owner is never trusted, so the readiness verdict fails
// CLOSED rather than reading "unobservable" as "root-owned".
func TestUnobservableOwnershipIsNeverTrusted(t *testing.T) {
	fi, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		shim shimFileInfo
	}{
		{"no stat structure", shimFileInfo{FileInfo: fi}},
		{"a foreign stat structure", shimFileInfo{FileInfo: fi, sys: &struct{ Mode uint32 }{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := platformOwner(tc.shim); ok {
				t.Error("platformOwner() reported an owner from a FileInfo that carries none")
			}
			owner := ownerString(tc.shim)
			if owner != "" {
				t.Errorf("ownerString() = %q, want empty", owner)
			}
			if ownerTrusted(owner) {
				t.Error("an unobservable owner must never be trusted")
			}
			// The consumer's shape (see TmpNamespaceStatus): the ownership half
			// of the boundary is computed from this string, so an empty one
			// leaves the check UNMET — the verdict fails closed instead of
			// claiming a root-owned tree it could not observe.
			state := TmpNamespaceState{ConfPresent: true, ConfOwner: owner}
			state.ConfOwnerOK = state.ConfPresent && ownerTrusted(state.ConfOwner)
			if state.ConfOwnerOK {
				t.Error("an unobservable owner must leave the ownership check unmet")
			}
		})
	}
}
