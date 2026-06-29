package cli

import (
	"context"
	"net/http/httptest"
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
