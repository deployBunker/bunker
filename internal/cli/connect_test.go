package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
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
	info *v1.ServerInfoResponse
	err  error
	// auditRecords, when set, is returned by QueryAudit.
	auditRecords []*v1.AuditRecord
	// spawnProgress, when set, is returned by GetSpawnProgress (DOGFOOD-015).
	// spawnProgressErr, when set, makes that RPC fail instead — the shape a
	// pre-DOGFOOD-015 daemon produces (CodeUnimplemented) and the shape any
	// other transport failure produces.
	spawnProgress    []*v1.SpawnProgress
	spawnProgressErr error
	// spawnProgressSeq, when set, is returned by GetSpawnProgress as a
	// SEQUENCE: each poll consumes the next stage. PERF-022's per-stage mark
	// proof drives this; plain one-stage mocks keep using spawnProgress.
	// The last entry serves every further poll.
	spawnProgressSeq []*v1.SpawnProgress
	// spawnProgressCalls counts GetSpawnProgress calls, so a test can assert
	// the in-flight heartbeat actually polled rather than assuming it did.
	spawnProgressCalls atomic.Int64
	// spawnProgressAuth records the Authorization header of the LAST
	// GetSpawnProgress request (PERF-022: the heartbeat must ride the same
	// operator token as the spawn RPC — an auth-enabled daemon denied every
	// header-less poll).
	spawnProgressAuth atomic.Value
}

// GetSpawnProgress fakes the DOGFOOD-015 in-flight stage report.
func (m *mockBunkerdServer) GetSpawnProgress(_ context.Context, req *connect.Request[v1.GetSpawnProgressRequest]) (*connect.Response[v1.GetSpawnProgressResponse], error) {
	m.spawnProgressCalls.Add(1)
	m.spawnProgressAuth.Store(req.Header().Get("Authorization"))
	if m.spawnProgressErr != nil {
		return nil, m.spawnProgressErr
	}
	// PERF-022: a sequenced mock hands out the CURRENT stage per poll —
	// one in-flight entry whose stage is the newest of the walk, exactly
	// like a real daemon report.
	source := m.spawnProgress
	if m.spawnProgressSeq != nil {
		n := int(m.spawnProgressCalls.Load())
		if n > len(m.spawnProgressSeq) {
			n = len(m.spawnProgressSeq)
		}
		if n < 1 {
			n = 1
		}
		source = m.spawnProgressSeq[n-1 : n]
	}
	resp := &v1.GetSpawnProgressResponse{}
	for _, p := range source {
		if id := req.Msg.GetAgentId(); id != "" && p.GetAgentId() != id {
			continue
		}
		resp.Spawns = append(resp.Spawns, p)
	}
	return connect.NewResponse(resp), nil
}

// GetAgentKey fakes the GAP-128 key-retrieval RPC (unimplemented default).
func (m *mockBunkerdServer) GetAgentKey(context.Context, *connect.Request[v1.GetAgentKeyRequest]) (*connect.Response[v1.GetAgentKeyResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
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
func (m *mockBunkerdServer) StopAgent(ctx context.Context, req *connect.Request[v1.StopAgentRequest]) (*connect.Response[v1.StopAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) StartAgent(ctx context.Context, req *connect.Request[v1.StartAgentRequest]) (*connect.Response[v1.StartAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) RestartAgent(ctx context.Context, req *connect.Request[v1.RestartAgentRequest]) (*connect.Response[v1.RestartAgentResponse], error) {
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
func (m *mockBunkerdServer) RunAgent(ctx context.Context, req *connect.Request[v1.RunAgentRequest]) (*connect.Response[v1.RunAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) HeartbeatAgent(ctx context.Context, req *connect.Request[v1.HeartbeatAgentRequest]) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) QueryAudit(ctx context.Context, req *connect.Request[v1.QueryAuditRequest]) (*connect.Response[v1.QueryAuditResponse], error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.auditRecords != nil {
		return connect.NewResponse(&v1.QueryAuditResponse{Records: m.auditRecords}), nil
	}
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

// GAP-132 key-lifecycle RPCs: unimplemented by this generic mock (the key
// CLI tests supply their own mocks that set these).
func (m *mockBunkerdServer) RotateJWTSecret(context.Context, *connect.Request[v1.RotateJWTSecretRequest]) (*connect.Response[v1.RotateJWTSecretResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) RevokeKey(context.Context, *connect.Request[v1.RevokeKeyRequest]) (*connect.Response[v1.RevokeKeyResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) KeyList(context.Context, *connect.Request[v1.KeyListRequest]) (*connect.Response[v1.KeyListResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

// GAP-130 operator-lifecycle RPCs joined the handler surface; this generic
// mock stays unimplemented for them (the operator tests supply their own).
func (m *mockBunkerdServer) OperatorCreate(context.Context, *connect.Request[v1.OperatorCreateRequest]) (*connect.Response[v1.OperatorCreateResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) OperatorList(context.Context, *connect.Request[v1.OperatorListRequest]) (*connect.Response[v1.OperatorListResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *mockBunkerdServer) OperatorDelete(context.Context, *connect.Request[v1.OperatorDeleteRequest]) (*connect.Response[v1.OperatorDeleteResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

// DF-BUNKER-34: the renewal drift RPC joined the handler surface; this
// generic mock stays unimplemented for it (the renew tests supply their own).
func (m *mockBunkerdServer) RenewalDriftReport(context.Context, *connect.Request[v1.RenewalDriftRequest]) (*connect.Response[v1.RenewalDriftResponse], error) {
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
