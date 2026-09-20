//go:build surfproof

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSurfaceProofDump exists ONLY for the local proof harness (run it with
// `go test -tags surfproof -run TestSurfaceProofDump`): it writes the exact
// generated scripts to SURF_PROOF_DIR so a shell harness can execute them
// against a fake systemctl. It is behind a build tag so it never compiles into
// normal builds or test runs.
func TestSurfaceProofDump(t *testing.T) {
	dir := os.Getenv("SURF_PROOF_DIR")
	if dir == "" {
		t.Skip("SURF_PROOF_DIR not set")
	}
	dump := func(name, content string) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dump("install.sh", buildSurfaceInstallScript())
	dump("remove.sh", buildSurfaceRemoveScript())
	dump("verify.sh", surfaceVerifyScript)
	dump("preflight.sh", surfacePreflightScript)
	dump("toolsd-probe.sh", surfaceToolsdProbeScript)
}
