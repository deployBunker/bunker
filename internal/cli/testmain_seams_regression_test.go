package cli

// INT-CI-56 regression: the package TestMain (procbuild_test.go) points HOME
// at a sentinel dir and must ALSO neutralize the ambient resolution seams the
// GAP-181 chain consults before HOME — XDG_CONFIG_HOME, BUNKER_HOME,
// BUNKER_CONFIG_HOME. A hosted runner exports XDG_CONFIG_HOME; before the
// TestMain fix, that ambient value outranked every per-test HOME pin, all 46
// failing tests resolved the config into one fixed runner-level dir
// (/home/runner/.config/bunker/...) OUTSIDE their t.TempDir(), writes
// vanished there (ENOENT at the test's hand-built path) and later tests read
// the leaked file (live dials to leaked servers). This cell pins the seam
// contract itself: inside this test binary the ambient env tiers must be
// blank, so a per-test t.Setenv("HOME", ...) pin alone fully determines
// resolution. RED shape: revert the TestMain neutralization and run the
// package with XDG_CONFIG_HOME set in the ambient environment — this cell
// fails again. The resolution-level consequences are pinned per-test by the
// existing cells (TestDefaultSSHKeyPath, TestSaveAndLoadConfig, ...), which
// reproduce the CI failure classes under that same ambient env.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTestMainSeams_NeutralizedForResolutionChain(t *testing.T) {
	home := os.Getenv("HOME")
	if home == "" {
		t.Fatal("TestMain must pin HOME for the whole package run")
	}
	for _, k := range []string{"XDG_CONFIG_HOME", "BUNKER_HOME", "BUNKER_CONFIG_HOME"} {
		if v := os.Getenv(k); v != "" {
			t.Errorf("ambient %s=%q visible inside the test binary — the TestMain seam neutralization regressed; the GAP-181 chain prefers this tier over $HOME/.config and every HOME-only test resolves outside its t.TempDir()", k, v)
		}
	}
	// The HOME the TestMain pinned must be a disposable sentinel (never the
	// operator's real home) and must not contain a pre-existing config the
	// chain could adopt: pins that this run's isolation actually holds.
	if _, err := os.Stat(filepath.Join(home, ".bunker", "config.yaml")); err == nil {
		t.Errorf("sentinel HOME %s contains a legacy config.yaml — isolation leaked", home)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "bunker", "config.yaml")); err == nil {
		t.Errorf("sentinel HOME %s contains config.yaml in the documented config dir — isolation leaked", home)
	}
}
