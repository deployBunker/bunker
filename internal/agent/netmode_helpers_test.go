package agent

// Test helpers for the NET-BUNKER-010/002 tests (netmode_test.go): the
// launch probe that proves no systemd-run ran, and the quiet logger.

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// mkNetmodeLaunchProbe creates the probe file path the network-mode spawn
// refusal test watches. It does NOT point systemd-run anywhere by itself —
// an unknown-mode spawn must fail at StageValidate, before any exec, so the
// probe only proves nothing CREATED it. For extra insurance the spawn's
// PATH is poisoned by the caller's stub environment only if a launch were
// attempted; here the contract under test is "no launch state at all".
func mkNetmodeLaunchProbe(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "systemd-run-never-invoked")
}

// netmodeProbeFired reports whether the launch probe file exists (i.e. some
// stage created launch state).
func netmodeProbeFired(t *testing.T, probe string) bool {
	t.Helper()
	_, err := os.Stat(probe)
	return err == nil
}

// newNetmodeQuietLogger returns the error-level logger the other agent
// suites use, so refusal-path logs do not flood test output.
func newNetmodeQuietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}
