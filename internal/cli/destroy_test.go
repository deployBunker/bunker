package cli

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// newDestroyTestServer starts an httptest server with a chi router mounting
// the connect handler for the given BunkerdHandler implementation.
func newDestroyTestServer(t *testing.T, handler bunkerv1connect.BunkerdHandler) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(handler)
	r.Mount(path, h)
	return httptest.NewServer(r)
}

// mockDestroyServer implements BunkerdHandler with configurable
// DestroyAgent response. All other methods return Unimplemented.
type mockDestroyServer struct {
	mockBunkerdServer
	destroyResp *v1.DestroyAgentResponse
	destroyErr  error
}

func (m *mockDestroyServer) DestroyAgent(
	ctx context.Context,
	req *connect.Request[v1.DestroyAgentRequest],
) (*connect.Response[v1.DestroyAgentResponse], error) {
	if m.destroyErr != nil {
		return nil, m.destroyErr
	}
	return connect.NewResponse(m.destroyResp), nil
}

// writeDestroyTestConfig writes a CLIConfig with a single server entry
// pointing at the given URL, and sets it as the active server.
func writeDestroyTestConfig(t *testing.T, home, serverURL string) {
	t.Helper()
	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"default": {
				Name:        "default",
				URL:         serverURL,
				ConnectedAt: "2026-06-28T00:00:00Z",
			},
		},
		ActiveServer: "default",
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}
}

func TestDestroyCommand_Help(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewDestroyCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		cmd.Execute()
	})

	if !strings.Contains(output, "Destroy an agent") {
		t.Errorf("help output missing description, got:\n%s", output)
	}
	if !strings.Contains(output, "--force") {
		t.Errorf("help output missing --force flag, got:\n%s", output)
	}
	if !strings.Contains(output, "--server") {
		t.Errorf("help output missing --server flag, got:\n%s", output)
	}
	if !strings.Contains(output, "--keep-key") {
		t.Errorf("help output missing --keep-key flag, got:\n%s", output)
	}
}

func TestDestroyCommand_NoActiveServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewDestroyCommand()
	cmd.SetArgs([]string{"abc12345"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when no active server")
	}
}

func TestDestroyCommand_Success(t *testing.T) {
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
			AgentId: "abc12345",
			Status:  "destroyed",
		},
	}
	srv := newDestroyTestServer(t, mock)
	defer srv.Close()

	writeDestroyTestConfig(t, tmpDir, srv.URL)

	cmd := NewDestroyCommand()
	cmd.SetArgs([]string{"abc12345"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "abc12345 destroyed") {
		t.Errorf("output missing 'abc12345 destroyed', got:\n%s", output)
	}
}

func TestDestroyCommand_NotFound(t *testing.T) {
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
			AgentId: "missing-id",
			Status:  "not_found",
		},
	}
	srv := newDestroyTestServer(t, mock)
	defer srv.Close()

	writeDestroyTestConfig(t, tmpDir, srv.URL)

	cmd := NewDestroyCommand()
	cmd.SetArgs([]string{"missing-id"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "not found") {
		t.Errorf("output missing 'not found', got:\n%s", output)
	}
}

func TestDestroyCommand_Force(t *testing.T) {
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
			AgentId: "abc12345",
			Status:  "destroyed",
		},
	}
	srv := newDestroyTestServer(t, mock)
	defer srv.Close()

	writeDestroyTestConfig(t, tmpDir, srv.URL)

	cmd := NewDestroyCommand()
	cmd.SetArgs([]string{"abc12345", "--force"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "destroyed") {
		t.Errorf("output missing 'destroyed', got:\n%s", output)
	}
}

func TestDestroyCommand_ServerError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockDestroyServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-err",
				Version:  "v0.2.0",
			},
		},
		destroyErr: connect.NewError(connect.CodeInternal, nil),
	}
	srv := newDestroyTestServer(t, mock)
	defer srv.Close()

	writeDestroyTestConfig(t, tmpDir, srv.URL)

	cmd := NewDestroyCommand()
	cmd.SetArgs([]string{"abc12345"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error from server")
	}
}

// TestDestroyCommand_CodeNotFound verifies the connect-error not-found path:
// the server maps a missing agent to connect.CodeNotFound, and the CLI must
// print the same clean message and exit 0 (nil error) as the in-band
// resp.Status == "not_found" branch.
func TestDestroyCommand_CodeNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockDestroyServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-nf",
				Version:  "v0.2.0",
			},
		},
		destroyErr: connect.NewError(connect.CodeNotFound, nil),
	}
	srv := newDestroyTestServer(t, mock)
	defer srv.Close()

	writeDestroyTestConfig(t, tmpDir, srv.URL)

	cmd := NewDestroyCommand()
	cmd.SetArgs([]string{"missing-id"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "Agent missing-id not found.") {
		t.Errorf("output missing 'Agent missing-id not found.', got:\n%s", output)
	}
	for _, leak := range []string{"userdel", "exit status", "destroy agent:"} {
		if strings.Contains(output, leak) {
			t.Errorf("output must not contain %q (raw leak/wrap), got:\n%s", leak, output)
		}
	}
}

func TestDestroyCommand_ServerNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockDestroyServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-nf",
				Version:  "v0.2.0",
			},
		},
		destroyResp: &v1.DestroyAgentResponse{
			AgentId: "noop",
			Status:  "destroyed",
		},
	}
	srv := newDestroyTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"other": {
				Name:        "other",
				URL:         srv.URL,
				ConnectedAt: "2026-06-28T00:00:00Z",
			},
		},
		ActiveServer: "other",
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewDestroyCommand()
	cmd.SetArgs([]string{"abc12345", "--server", "missing-server"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for missing server")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

// TestDestroyCommand_LocalSSHKeyCleanup verifies that a successful destroy
// (including both idempotent not-found shapes) removes the client-local SSH
// key saved at spawn time (~/.bunker/keys/<id>), while --keep-key and real
// RPC errors leave it in place.
func TestDestroyCommand_LocalSSHKeyCleanup(t *testing.T) {
	tests := []struct {
		name            string
		agentID         string
		destroyResp     *v1.DestroyAgentResponse
		destroyErr      error
		args            []string
		seedKey         bool
		wantKeyGone     bool
		wantRemovalLine bool
		wantErr         bool
	}{
		{
			name:            "success removes local key",
			agentID:         "abc12345",
			destroyResp:     &v1.DestroyAgentResponse{AgentId: "abc12345", Status: "destroyed"},
			seedKey:         true,
			wantKeyGone:     true,
			wantRemovalLine: true,
		},
		{
			name:        "success with --keep-key leaves local key",
			agentID:     "abc12345",
			destroyResp: &v1.DestroyAgentResponse{AgentId: "abc12345", Status: "destroyed"},
			args:        []string{"--keep-key"},
			seedKey:     true,
			wantKeyGone: false,
		},
		{
			name:        "rpc error leaves local key",
			agentID:     "abc12345",
			destroyErr:  connect.NewError(connect.CodeInternal, nil),
			seedKey:     true,
			wantKeyGone: false,
			wantErr:     true,
		},
		{
			name:            "in-band not_found still removes local key",
			agentID:         "missing-id",
			destroyResp:     &v1.DestroyAgentResponse{AgentId: "missing-id", Status: "not_found"},
			seedKey:         true,
			wantKeyGone:     true,
			wantRemovalLine: true,
		},
		{
			name:            "connect CodeNotFound still removes local key",
			agentID:         "missing-id",
			destroyErr:      connect.NewError(connect.CodeNotFound, nil),
			seedKey:         true,
			wantKeyGone:     true,
			wantRemovalLine: true,
		},
		{
			name:        "already-absent key is not an error",
			agentID:     "nokey",
			destroyResp: &v1.DestroyAgentResponse{AgentId: "nokey", Status: "destroyed"},
			seedKey:     false,
			wantKeyGone: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)

			// Seed a dummy client-local key at the same path spawn writes.
			keyPath, err := defaultSSHKeyPath(tt.agentID)
			if err != nil {
				t.Fatalf("defaultSSHKeyPath: %v", err)
			}
			if tt.seedKey {
				if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
					t.Fatalf("MkdirAll(%s): %v", filepath.Dir(keyPath), err)
				}
				if err := os.WriteFile(keyPath, []byte("dummy-private-key"), 0600); err != nil {
					t.Fatalf("WriteFile(%s): %v", keyPath, err)
				}
			}

			mock := &mockDestroyServer{
				mockBunkerdServer: mockBunkerdServer{
					info: &v1.ServerInfoResponse{
						Hostname: "bunker-test",
						Version:  "v0.2.0",
					},
				},
				destroyResp: tt.destroyResp,
				destroyErr:  tt.destroyErr,
			}
			srv := newDestroyTestServer(t, mock)
			defer srv.Close()

			writeDestroyTestConfig(t, tmpDir, srv.URL)

			cmd := NewDestroyCommand()
			cmd.SetArgs(append([]string{tt.agentID}, tt.args...))

			var execErr error
			output := captureStdout(t, func() {
				execErr = cmd.Execute()
			})
			if tt.wantErr {
				if execErr == nil {
					t.Error("expected error from server")
				}
			} else if execErr != nil {
				t.Errorf("Execute: %v", execErr)
			}

			_, statErr := os.Stat(keyPath)
			if tt.wantKeyGone {
				if !os.IsNotExist(statErr) {
					t.Errorf("expected local key %s to be removed, stat err: %v", keyPath, statErr)
				}
			} else if statErr != nil {
				t.Errorf("expected local key %s to remain, stat err: %v", keyPath, statErr)
			}

			if tt.wantRemovalLine {
				if !strings.Contains(output, "Removed local SSH key "+keyPath) {
					t.Errorf("output missing removal line, got:\n%s", output)
				}
			}
		})
	}
}
