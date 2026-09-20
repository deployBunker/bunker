package cli

import (
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// DF-BUNKER-33 CLI contract: a fail-closed destroy (home_retained) must
// print a clear retention message AND exit non-zero, keeping the local SSH
// key; --help must document that the home is deleted and archived first
// under the default policy.

// TestDestroyCommand_HomeRetainedStatusIsNonZeroError proves the in-band
// fail-closed surface: the server answers 200 with status home_retained,
// the CLI prints the retention guidance and RETURNS AN ERROR (cobra exits
// non-zero on a returned error).
func TestDestroyCommand_HomeRetainedStatusIsNonZeroError(t *testing.T) {
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
		destroyResp: &v1.DestroyAgentResponse{
			AgentId: "held-agent",
			Status:  "home_retained",
		},
	}
	srv := newDestroyTestServer(t, mock)
	defer srv.Close()

	writeDestroyTestConfig(t, tmpDir, srv.URL)

	cmd := NewDestroyCommand()
	cmd.SetArgs([]string{"--server", "default", "held-agent"})
	var output string
	var execErr error
	output = captureStdout(t, func() {
		execErr = cmd.Execute()
	})

	if execErr == nil {
		t.Fatal("home_retained destroy returned nil error; CLI would exit 0")
	}
	if !strings.Contains(execErr.Error(), "retained") {
		t.Errorf("error %q does not mention retention", execErr)
	}
	if !strings.Contains(output, "NOT destroyed") || !strings.Contains(output, "RETAINED") {
		t.Errorf("output missing retention guidance, got:\n%s", output)
	}
}

// TestDestroyCommand_HomeRetainedRPCErrorAlsoExplains covers the other wire
// shape: the server maps the fail-closed destroy to a CodeInternal whose
// message names the retained home — the CLI must still print the guidance
// line before the wrapped error.
func TestDestroyCommand_HomeRetainedRPCErrorAlsoExplains(t *testing.T) {
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
		destroyErr: connectInternalError("destroy aborted: agent home /home/bunker-x could not be archived to /var/backups/bunker: boom (home retained, nothing deleted)"),
	}
	srv := newDestroyTestServer(t, mock)
	defer srv.Close()

	writeDestroyTestConfig(t, tmpDir, srv.URL)

	cmd := NewDestroyCommand()
	cmd.SetArgs([]string{"--server", "default", "held-agent"})
	var output string
	var execErr error
	output = captureStdout(t, func() {
		execErr = cmd.Execute()
	})

	if execErr == nil {
		t.Fatal("RPC-error destroy returned nil error")
	}
	if !strings.Contains(output, "RETAINED") {
		t.Errorf("output missing retention guidance for the RPC-error shape, got:\n%s", output)
	}
}

// TestDestroyCommand_HelpDocumentsHomeDeletionAndArchive covers criterion
// F: the help text must say the home is deleted and that the default
// policy archives it first.
func TestDestroyCommand_HelpDocumentsHomeDeletionAndArchive(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewDestroyCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--server", "default", "--help"})
		_ = cmd.Execute()
	})

	for _, needle := range []string{
		"home",
		"deleted",
		"archive",
		"purge",
	} {
		if !strings.Contains(strings.ToLower(output), needle) {
			t.Errorf("help output missing %q; got:\n%s", needle, output)
		}
	}
}

// connectInternalError builds the connect error the real server produces
// for a fail-closed destroy (CodeInternal carrying the manager's message).
func connectInternalError(msg string) error {
	return connect.NewError(connect.CodeInternal, fmt.Errorf("%s", msg))
}
