package cli

// MISCHIEF-DF-24 CLI contract: a live_processes refusal (the RPC-error wire
// shape — the server maps the gate refusal to CodeInternal) must print the
// plain, actionable guidance — the exact kill command and the --force
// kill-first exit hatch — BEFORE the wrapped error, mirroring the
// home_retained guidance pattern (DF-BUNKER-33). The in-band status shape
// cannot occur (the manager always pairs status live_processes with a
// non-nil error), so the RPC-error surface is the only one to cover.

import (
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// TestDestroyCommand_LiveProcessRefusalGuidesKillAndForce proves the CLI
// prints the kill command and the --force hint when the destroy is refused
// on live processes, and still exits non-zero.
func TestDestroyCommand_LiveProcessRefusalGuidesKillAndForce(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "default")
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockDestroyServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-test",
				Version:  "v0.2.0",
			},
		},
		destroyErr: connectInternalError("destroy refused: user bunker-df24-cli (uid 61041) still owns live processes that userdel -rf would orphan. 1 live process(es) under uid 61041: pid 4242: node server.js. Nothing was deleted; kill the processes and retry: bunker ssh bunker-df24-cli -- pkill -U 61041 — or let the destroy kill them for you: bunker destroy <agent-id> --force (SIGTERM, then SIGKILL escalation; the home is still archived first)"),
	}
	srv := newDestroyTestServer(t, mock)
	defer srv.Close()

	writeDestroyTestConfig(t, tmpDir, srv.URL)

	cmd := NewDestroyCommand()
	cmd.SetArgs([]string{"--server", "default", "df24-cli"})
	var output string
	var execErr error
	output = captureStdout(t, func() {
		execErr = cmd.Execute()
	})

	if execErr == nil {
		t.Fatal("live_processes destroy returned nil error; CLI would exit 0")
	}
	for _, want := range []string{
		// The plain guidance line (no wrapped-connect parsing required).
		"NOT destroyed",
		"live processes",
		"nothing was deleted",
		// The exact kill command, with THIS agent's id filled in.
		"bunker ssh df24-cli -- pkill",
		// The kill-first exit hatch, ready to paste.
		"bunker destroy df24-cli --force",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
}

// TestDestroyCommand_HelpNamesForceKillFirst pins the flag-surface half of
// MISCHIEF-DF-24: `--force` help must say it IS kill-first (SIGTERM →
// SIGKILL escalation, home archived first) so `bunker destroy --help`
// answers the refusal's question on its own.
func TestDestroyCommand_HelpNamesForceKillFirst(t *testing.T) {
	cmd := NewDestroyCommand()
	cmd.SetArgs([]string{"--help"})
	output := captureStdout(t, func() {
		_ = cmd.Execute()
	})
	if !strings.Contains(output, "--force") {
		t.Fatalf("help output missing --force flag, got:\n%s", output)
	}
	for _, want := range []string{"SIGTERM", "SIGKILL", "kill-first"} {
		if !strings.Contains(output, want) {
			t.Errorf("help for --force missing %q (kill-first contract), got:\n%s", want, output)
		}
	}
}
