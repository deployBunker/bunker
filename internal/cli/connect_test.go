package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
	"github.com/go-chi/chi/v5"
	"github.com/spf13/cobra"
)

// captureStdout captures os.Stdout while fn runs and returns the captured output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// mockBunkerdServer is a test implementation of BunkerdHandler.
type mockBunkerdServer struct {
	info      *v1.ServerInfoResponse
	err       error
	errStatus int // if > 0, return a plain HTTP error
}

func (m *mockBunkerdServer) ServerInfo(ctx context.Context, req *connect.Request[v1.ServerInfoRequest]) (*connect.Response[v1.ServerInfoResponse], error) {
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(m.info), nil
}

// Remaining methods return unimplemented.
func (m *mockBunkerdServer) ServerMetrics(ctx context.Context, req *connect.Request[v1.ServerMetricsRequest]) (*connect.Response[v1.ServerMetricsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) SpawnAgent(ctx context.Context, req *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) DestroyAgent(ctx context.Context, req *connect.Request[v1.DestroyAgentRequest]) (*connect.Response[v1.DestroyAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) ListAgents(ctx context.Context, req *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) GetAgent(ctx context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) AgentMetrics(ctx context.Context, req *connect.Request[v1.AgentMetricsRequest]) (*connect.Response[v1.AgentMetricsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) HeartbeatAgent(ctx context.Context, req *connect.Request[v1.HeartbeatAgentRequest]) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

// newTestServer starts an httptest server with a chi router mounting the connect handler.
func newTestServer(t *testing.T, mock bunkerv1connect.BunkerdHandler) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	path, handler := bunkerv1connect.NewBunkerdHandler(mock)
	r.Mount(path, handler)

	// Also handle raw 500 for error tests.
	r.Get("/error500", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	})

	return httptest.NewServer(r)
}

func TestConnectCommand_Help(t *testing.T) {
	// Use a temp HOME so we don't touch real config.
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewConnectCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		cmd.Execute()
	})

	if !strings.Contains(output, "Register a bunkerd server") && !strings.Contains(output, "Connect to a bunkerd server") {
		t.Errorf("help output missing description, got:\n%s", output)
	}
	if !strings.Contains(output, "--name") {
		t.Errorf("help output missing --name flag, got:\n%s", output)
	}
}

func TestConnectCommand_MissingArgs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewConnectCommand()
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for missing URL argument")
	}
}

func TestConnectCommand_Success(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockBunkerdServer{
		info: &v1.ServerInfoResponse{
			Hostname:      "bunker-alpha",
			Version:       "v0.2.0",
			UptimeSeconds: 3600,
			AgentCount:    5,
			MaxAgents:     100,
		},
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cmd := NewConnectCommand()
	cmd.SetArgs([]string{srv.URL})

	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "bunker-alpha") {
		t.Errorf("output missing hostname, got:\n%s", output)
	}
	if !strings.Contains(output, "v0.2.0") {
		t.Errorf("output missing version, got:\n%s", output)
	}
	if !strings.Contains(output, "5/100") {
		t.Errorf("output missing agent count, got:\n%s", output)
	}
	if !strings.Contains(output, "Server registered as") {
		t.Errorf("output missing registration message, got:\n%s", output)
	}
}

func TestConnectCommand_Success_WithName(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockBunkerdServer{
		info: &v1.ServerInfoResponse{
			Hostname: "bunker-beta",
			Version:  "v0.2.0",
		},
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cmd := NewConnectCommand()
	cmd.SetArgs([]string{"--name", "my-server", srv.URL})

	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, `"my-server"`) {
		t.Errorf("output missing custom name, got:\n%s", output)
	}
}

func TestConnectCommand_ConnectionRefused(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewConnectCommand()
	cmd.SetArgs([]string{"http://127.0.0.1:19999"})

	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for connection refused")
	}
}

func TestConnectCommand_ServerError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Serve a plain HTTP 500 at the root; the connect client will see a non-connect response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer srv.Close()

	cmd := NewConnectCommand()
	cmd.SetArgs([]string{srv.URL})

	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for server error")
	}
}

func TestConnectCommand_InvalidURL(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewConnectCommand()
	cmd.SetArgs([]string{"://bad-url"})

	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

// Ensure cmd is used (silence compiler warnings on unused import in test builds).
var _ = (*cobra.Command)(nil)
