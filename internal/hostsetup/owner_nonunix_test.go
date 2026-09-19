//go:build !unix

package hostsetup

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOwnershipIsUnobservableOnThisPlatform pins the portability fallback: this
// platform exposes no POSIX owner, so every file reports "unobservable" — never
// a uid that could satisfy the root-ownership comparison. Cross-compiled (and
// type-checked by `GOOS=windows go vet ./...`) rather than run on the Linux CI
// host, which is why it asserts ownership only and not permission bits.
func TestOwnershipIsUnobservableOnThisPlatform(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := platformOwner(fi); ok {
		t.Error("platformOwner() reported an owner on a platform without POSIX ownership")
	}
	if got := ownerString(fi); got != "" {
		t.Errorf("ownerString() = %q, want empty", got)
	}
	if ownerTrusted(ownerString(fi)) {
		t.Error("an unobservable owner must never be trusted")
	}
	// The path is still observed (so the mode half of the boundary applies),
	// but the owner stays empty — which is what makes the readiness verdict
	// fail closed here instead of claiming a root-owned tree.
	owner, _, ok := statOwnerMode(path)
	if !ok {
		t.Fatal("statOwnerMode() did not report a file that exists")
	}
	if owner != "" {
		t.Errorf("statOwnerMode() owner = %q, want empty", owner)
	}
	if _, _, ok := statOwnerMode(filepath.Join(t.TempDir(), "absent")); ok {
		t.Error("statOwnerMode() reported a file that does not exist")
	}
}
