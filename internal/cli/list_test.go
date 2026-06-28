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

// newListTestServer starts an httptest server with a chi router mounting
// the connect handler for the given BunkerdHandler implementation.
func newListTestServer(t *testing.T, handler bunkerv1connect.BunkerdHandler) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(handler)
	r.Mount(path, h)
	return httptest.NewServer(r)
}

// mockListServer implements BunkerdHandler with configurable
// ListAgents response. All other methods return Unimplemented.
type mockListServer struct {
	mockBunkerdServer
	listResp *v1.ListAgentsResponse
	listErr  error
}

func (m *mockListServer) ListAgents(
	ctx context.Context,
	req *connect.Request[v1.ListAgentsRequest],
) (*connect.Response[v1.ListAgentsResponse], error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return connect.NewResponse(m.listResp), nil
}

// writeListTestConfig writes a CLIConfig with a single server entry
// pointing at the given URL, and sets it as the active server.
func writeListTestConfig(t *testing.T, home, serverURL string) {
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

func TestListCommand_Help(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewListCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		cmd.Execute()
	})

	if !strings.Contains(output, "List agents on a bunkerd server") && !strings.Contains(output, "List agents on the active bunkerd server") {
		t.Errorf("help output missing description, got:\n%s", output)
	}
	if !strings.Contains(output, "--status") {
		t.Errorf("help output missing --status flag, got:\n%s", output)
	}
	if !strings.Contains(output, "--page-size") {
		t.Errorf("help output missing --page-size flag, got:\n%s", output)
	}
}

func TestListCommand_NoActiveServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewListCommand()
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when no active server")
	}
}

func TestListCommand_Success_WithAgents(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockListServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-test",
				Version:  "v0.2.0",
			},
		},
		listResp: &v1.ListAgentsResponse{
			Agents: []*v1.AgentSummary{
				{
					AgentId:   "abc12345",
					Status:    "running",
					CreatedAt: "2026-06-28T10:00:00Z",
					PublicUrl: "https://abc12345.trycloudflare.com",
				},
				{
					AgentId:   "def67890",
					Status:    "stopped",
					CreatedAt: "2026-06-27T12:00:00Z",
				},
			},
			TotalCount: 2,
		},
	}
	srv := newListTestServer(t, mock)
	defer srv.Close()

	writeListTestConfig(t, tmpDir, srv.URL)

	cmd := NewListCommand()
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	for _, want := range []string{"abc12345", "running", "def67890", "stopped", "Total: 2 agents"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
}

func TestListCommand_Success_NoAgents(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockListServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-test",
				Version:  "v0.2.0",
			},
		},
		listResp: &v1.ListAgentsResponse{
			Agents:     []*v1.AgentSummary{},
			TotalCount: 0,
		},
	}
	srv := newListTestServer(t, mock)
	defer srv.Close()

	writeListTestConfig(t, tmpDir, srv.URL)

	cmd := NewListCommand()
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "No agents found.") {
		t.Errorf("output missing 'No agents found.', got:\n%s", output)
	}
}

func TestListCommand_ServerNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockListServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-nf",
				Version:  "v0.2.0",
			},
		},
		listResp: &v1.ListAgentsResponse{
			Agents: []*v1.AgentSummary{},
		},
	}
	srv := newListTestServer(t, mock)
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

	cmd := NewListCommand()
	cmd.SetArgs([]string{"--server", "missing-server"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for missing server")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

func TestListCommand_ServerError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockListServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-err",
				Version:  "v0.2.0",
			},
		},
		listErr: connect.NewError(connect.CodeInternal, nil),
	}
	srv := newListTestServer(t, mock)
	defer srv.Close()

	writeListTestConfig(t, tmpDir, srv.URL)

	cmd := NewListCommand()
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error from server")
	}
}
