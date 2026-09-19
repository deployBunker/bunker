//go:build unix

package agent

import (
	"os"
	"testing"
)

// foreignSys is an os.FileInfo whose Sys() carries something other than this
// platform's stat structure — the shape a wrapped or proxied stat has, and how
// the "owner is unobservable" branch is reachable on a unix host.
type foreignSys struct {
	os.FileInfo
	sys any
}

func (f foreignSys) Sys() any { return f.sys }

// TestRuntimeDirOwnerIsObservedOnDisk pins the unix half of the ownership seam
// through the PRODUCTION probe: a directory this process created must be
// reported as owned by this process's uid, which is what lets
// classifyRuntimeDir call it fresh rather than resetting it.
func TestRuntimeDirOwnerIsObservedOnDisk(t *testing.T) {
	dir := t.TempDir()
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	uid, ok := statOwnerUID(fi)
	if !ok {
		t.Fatal("statOwnerUID() did not report an owner for a real directory")
	}
	if int(uid) != os.Getuid() {
		t.Errorf("statOwnerUID() = %d, want %d", uid, os.Getuid())
	}

	info, err := probeRuntimeDirOnDisk(dir)
	if err != nil {
		t.Fatalf("probeRuntimeDirOnDisk(%s) error = %v", dir, err)
	}
	if !info.ownerKnown || int(info.owner) != os.Getuid() {
		t.Errorf("probeRuntimeDirOnDisk() owner = %d (known %v), want %d (known true)", info.owner, info.ownerKnown, os.Getuid())
	}
	if stale, reason := classifyRuntimeDir(info, nil, os.Getuid()); stale {
		t.Errorf("classifyRuntimeDir() called this agent's own runtime dir stale: %s", reason)
	}
}

// TestUnobservableOwnerFailsSafe pins the other half: an owner that cannot be
// read is reported as UNKNOWN and classified as STALE — never as "the expected
// uid", which is the direction that would skip a needed reset.
func TestUnobservableOwnerFailsSafe(t *testing.T) {
	fi, err := os.Lstat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		shim foreignSys
	}{
		{"no stat structure", foreignSys{FileInfo: fi}},
		{"a foreign stat structure", foreignSys{FileInfo: fi, sys: &struct{ Mode uint32 }{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := statOwnerUID(tc.shim); ok {
				t.Error("statOwnerUID() reported an owner from a FileInfo that carries none")
			}
			// The probe's unknown-owner shape is what a platform without POSIX
			// ownership produces for a real directory.
			info := runtimeDirInfo{exists: true, isDir: true}
			stale, reason := classifyRuntimeDir(info, nil, os.Getuid())
			if !stale || reason == "" {
				t.Errorf("classifyRuntimeDir() stale = %v (reason %q), want true with a reason", stale, reason)
			}
		})
	}
}
