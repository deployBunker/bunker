//go:build !unix

package agent

import (
	"os"
	"testing"
)

// TestRuntimeDirOwnerIsUnknownOnThisPlatform pins the portability fallback: this
// platform exposes no POSIX owner, so the probe reports ownership as UNKNOWN —
// never as the uid being reconciled. classifyRuntimeDir treats an unknown owner
// as stale (fail-SAFE: the historical reset still runs), which is the property
// that keeps the cross-build from silently skipping a reset. Cross-compiled and
// type-checked by `GOOS=windows go vet ./...` rather than run on the Linux CI
// host.
func TestRuntimeDirOwnerIsUnknownOnThisPlatform(t *testing.T) {
	dir := t.TempDir()
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := statOwnerUID(fi); ok {
		t.Error("statOwnerUID() reported an owner on a platform without POSIX ownership")
	}

	info, err := probeRuntimeDirOnDisk(dir)
	if err != nil {
		t.Fatalf("probeRuntimeDirOnDisk(%s) error = %v", dir, err)
	}
	if info.ownerKnown {
		t.Error("probeRuntimeDirOnDisk() reported a known owner on a platform without POSIX ownership")
	}
	stale, reason := classifyRuntimeDir(info, nil, os.Getuid())
	if !stale || reason == "" {
		t.Errorf("classifyRuntimeDir() stale = %v (reason %q), want true with a reason", stale, reason)
	}
}
