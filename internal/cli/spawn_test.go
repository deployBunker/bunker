package cli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// newSpawnTestServer starts an httptest server with a chi router mounting
// the connect handler for the given BunkerdHandler implementation.
func newSpawnTestServer(t *testing.T, handler bunkerv1connect.BunkerdHandler) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(handler)
	r.Mount(path, h)
	return httptest.NewServer(r)
}

// mockSpawnServer implements BunkerdHandler with configurable
// ServerInfo and SpawnAgent responses. All other methods return Unimplemented.
type mockSpawnServer struct {
	mockBunkerdServer
	spawnResp  *v1.SpawnAgentResponse
	spawnErr   error
	gotAgentID string // AgentId from the last SpawnAgent request (DOGFOOD-008)
}

func (m *mockSpawnServer) SpawnAgent(
	ctx context.Context,
	req *connect.Request[v1.SpawnAgentRequest],
) (*connect.Response[v1.SpawnAgentResponse], error) {
	m.gotAgentID = req.Msg.AgentId
	if m.spawnErr != nil {
		return nil, m.spawnErr
	}
	return connect.NewResponse(m.spawnResp), nil
}

// writeSpawnTestConfig writes a CLIConfig with a single server entry
// pointing at the given URL, and sets it as the active server.
func writeSpawnTestConfig(t *testing.T, home, serverURL string) {
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

func TestSpawnCommand_Help(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewSpawnCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		cmd.Execute()
	})

	if !strings.Contains(output, "Create a new agent") {
		t.Errorf("help output missing description, got:\n%s", output)
	}
	if !strings.Contains(output, "--cpu") {
		t.Errorf("help output missing --cpu flag, got:\n%s", output)
	}
	if !strings.Contains(output, "--memory") {
		t.Errorf("help output missing --memory flag, got:\n%s", output)
	}
	// The positional [agent-id] alias must be documented in help
	// (DOGFOOD-008).
	if !strings.Contains(output, "[agent-id]") {
		t.Errorf("help output missing positional [agent-id], got:\n%s", output)
	}
}

func TestSpawnCommand_NoActiveServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewSpawnCommand()
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when no active server")
	}
}

func TestSpawnCommand_Success(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockSpawnServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-test",
				Version:  "v0.2.0",
			},
		},
		spawnResp: &v1.SpawnAgentResponse{
			AgentId:        "abc12345",
			DockerHostSsh:  "DOCKER_HOST=ssh://bunker-abc12345@host",
			SshPrivateKey:  "test-private-key-data",
			PublicUrl:      "https://abc12345.trycloudflare.com",
			PortRangeStart: 30000,
			PortRangeEnd:   30099,
			ExpiresAt:      "2026-06-29T00:00:00Z",
		},
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()

	writeSpawnTestConfig(t, tmpDir, srv.URL)

	cmd := NewSpawnCommand()
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	for _, want := range []string{"abc12345", "DOCKER_HOST", "trycloudflare.com", "30000-30099"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	// The Docker SSH line must show the client-reachable host (from the
	// server config URL) instead of the hostname the server embedded.
	if !strings.Contains(output, "DOCKER_HOST=ssh://bunker-abc12345@127.0.0.1") {
		t.Errorf("Docker SSH line not rewritten to URL host, got:\n%s", output)
	}
	if strings.Contains(output, "@host") {
		t.Errorf("output still contains server-provided host, got:\n%s", output)
	}
}

// TestSpawnCommand_BundleHostRewrite verifies that the printed SSHFS/Tunnel/
// Docker-SSH bundle lines use the client-resolved host and the client-local
// key path instead of the server's self-reported hostname and server-side key
// path.
func TestSpawnCommand_BundleHostRewrite(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockSpawnServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-mvp",
				Version:  "v0.2.0",
			},
		},
		spawnResp: &v1.SpawnAgentResponse{
			AgentId:          "abc12345",
			DockerHostSsh:    "DOCKER_HOST=ssh://bunker-abc12345@bunker-mvp",
			DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -i /etc/bunkerd/ssh/abc12345 -L 2376:/run/bunker/abc12345/docker.sock bunker-abc12345@bunker-mvp -N",
			SshfsMount:       "sshfs -o IdentityFile=/etc/bunkerd/ssh/abc12345 -o idmap=user -o allow_other bunker-abc12345@bunker-mvp:/home/bunker-abc12345 /mnt/bunker/abc12345",
			SshPrivateKey:    "test-private-key-data",
		},
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()

	writeSpawnTestConfig(t, tmpDir, srv.URL)

	cmd := NewSpawnCommand()
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	keyPath := filepath.Join(tmpDir, ".bunker", "keys", "abc12345")
	for _, want := range []string{
		"DOCKER_HOST=ssh://bunker-abc12345@127.0.0.1",
		"-i " + keyPath,
		"-L 2376:/run/bunker/abc12345/docker.sock",
		"bunker-abc12345@127.0.0.1:/home/bunker-abc12345",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	for _, absent := range []string{"bunker-mvp", "/etc/bunkerd/ssh/"} {
		if strings.Contains(output, absent) {
			t.Errorf("output must not contain %q, got:\n%s", absent, output)
		}
	}
	// The private key must be saved client-locally.
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("client-local key not saved at %s: %v", keyPath, err)
	}
}

// TestSpawnCommand_SshHostFlag verifies --ssh-host overrides the host shown in
// every bundle line.
func TestSpawnCommand_SshHostFlag(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockSpawnServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-mvp",
				Version:  "v0.2.0",
			},
		},
		spawnResp: &v1.SpawnAgentResponse{
			AgentId:          "abc12345",
			DockerHostSsh:    "DOCKER_HOST=ssh://bunker-abc12345@bunker-mvp",
			DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc12345 -L 2376:/run/bunker/abc12345/docker.sock bunker-abc12345@bunker-mvp -N",
			SshfsMount:       "sshfs -o IdentityFile=/etc/bunkerd/ssh/abc12345 -o idmap=user -o allow_other bunker-abc12345@bunker-mvp:/home/bunker-abc12345 /mnt/bunker/abc12345",
			SshPrivateKey:    "test-private-key-data",
		},
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()

	writeSpawnTestConfig(t, tmpDir, srv.URL)

	cmd := NewSpawnCommand()
	cmd.SetArgs([]string{"--ssh-host", "somehost.example"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	for _, want := range []string{
		"DOCKER_HOST=ssh://bunker-abc12345@somehost.example",
		"bunker-abc12345@somehost.example:/home/bunker-abc12345",
		"bunker-abc12345@somehost.example -N",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "bunker-mvp") || strings.Contains(output, "@127.0.0.1") {
		t.Errorf("output must not contain server/URL host, got:\n%s", output)
	}
}

func TestSpawnCommand_Success_Minimal(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockSpawnServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-minimal",
				Version:  "v0.2.0",
			},
		},
		spawnResp: &v1.SpawnAgentResponse{
			AgentId:       "min123",
			DockerHostSsh: "DOCKER_HOST=ssh://min@host",
			SshPrivateKey: "test-private-key-data",
		},
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()

	writeSpawnTestConfig(t, tmpDir, srv.URL)

	cmd := NewSpawnCommand()
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	for _, want := range []string{"min123", "DOCKER_HOST"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	// Public URL should not appear in minimal response.
	if strings.Contains(output, "Public URL") {
		t.Error("output unexpectedly contains Public URL")
	}
}

// TestSpawnCommand_PositionalAgentID covers DOGFOOD-008: `bunker spawn
// demo-agent` must bind the positional argument to the agent ID (it was
// previously silently ignored), a positional + --agent-id combination must
// error, invalid IDs must be rejected locally against the [a-z0-9-]{1,64}
// rule, and more than one positional must hit the cobra usage error.
func TestSpawnCommand_PositionalAgentID(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantID  string // AgentId expected at the server (success rows only)
		wantErr string // substring expected in the error (empty = success)
	}{
		{
			name:   "positional id binds to agent-id",
			args:   []string{"demo-agent"},
			wantID: "demo-agent",
		},
		{
			name:    "positional conflicts with --agent-id",
			args:    []string{"--agent-id", "flag-agent", "pos-agent"},
			wantErr: "use one or the other",
		},
		{
			name:    "invalid positional id rejected before connect",
			args:    []string{"Bad_Name!"},
			wantErr: "[a-z0-9-]{1,64}",
		},
		{
			name:    "invalid --agent-id rejected before connect",
			args:    []string{"--agent-id", "UPPER"},
			wantErr: "[a-z0-9-]{1,64}",
		},
		{
			name:    "more than one positional rejected",
			args:    []string{"one", "two"},
			wantErr: "accepts at most 1 arg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)

			// Error rows run with NO server configured: validation must
			// fire locally, before config load or any RPC.
			var mock *mockSpawnServer
			if tt.wantErr == "" {
				mock = &mockSpawnServer{
					mockBunkerdServer: mockBunkerdServer{
						info: &v1.ServerInfoResponse{
							Hostname: "bunker-test",
							Version:  "v0.2.0",
						},
					},
					spawnResp: &v1.SpawnAgentResponse{AgentId: "resp-id"},
				}
				srv := newSpawnTestServer(t, mock)
				defer srv.Close()
				writeSpawnTestConfig(t, tmpDir, srv.URL)
			}

			cmd := NewSpawnCommand()
			cmd.SetArgs(tt.args)
			err := cmd.Execute()

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if mock.gotAgentID != tt.wantID {
				t.Errorf("server received AgentId %q, want %q", mock.gotAgentID, tt.wantID)
			}
		})
	}
}

// blockingSpawnServer blocks the SpawnAgent RPC until released, simulating
// the ~20s server-side agent creation that GAP-023's progress line must
// precede.
type blockingSpawnServer struct {
	mockSpawnServer
	release chan struct{}
}

func (m *blockingSpawnServer) SpawnAgent(
	ctx context.Context,
	req *connect.Request[v1.SpawnAgentRequest],
) (*connect.Response[v1.SpawnAgentResponse], error) {
	<-m.release
	if m.spawnErr != nil {
		return nil, m.spawnErr
	}
	return connect.NewResponse(m.spawnResp), nil
}

// TestSpawnCommand_ProgressLineWithin2s verifies GAP-023: `bunker spawn`
// prints a "Creating agent..." progress line within 2 seconds of invocation,
// before the (slow) agent-creation RPC returns. The mock server blocks until
// the test has observed the progress line, proving it is printed before the
// RPC completes.
func TestSpawnCommand_ProgressLineWithin2s(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	release := make(chan struct{})
	mock := &blockingSpawnServer{
		mockSpawnServer: mockSpawnServer{
			mockBunkerdServer: mockBunkerdServer{
				info: &v1.ServerInfoResponse{
					Hostname: "bunker-progress",
					Version:  "v0.2.0",
				},
			},
			spawnResp: &v1.SpawnAgentResponse{
				AgentId:       "prog123",
				DockerHostSsh: "DOCKER_HOST=ssh://prog@host",
			},
		},
		release: release,
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()

	writeSpawnTestConfig(t, tmpDir, srv.URL)

	cmd := NewSpawnCommand()

	// Capture stdout on a pipe and read it in a goroutine so we can observe
	// the progress line BEFORE the RPC (still blocked) returns.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	var (
		mu     sync.Mutex
		output bytes.Buffer
	)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Read(buf)
			mu.Lock()
			output.Write(buf[:n])
			mu.Unlock()
			if rerr != nil {
				return
			}
		}
	}()

	execDone := make(chan error, 1)
	go func() {
		execDone <- cmd.Execute()
	}()

	// Wait up to 2s for the progress line while the RPC is still blocked.
	progressSeen := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		progressSeen = strings.Contains(output.String(), "Creating agent...")
		mu.Unlock()
		if progressSeen {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release) // release the RPC so Execute can finish

	if !progressSeen {
		t.Error("GAP-023: 'Creating agent...' progress line NOT printed within 2s of invocation")
	}

	if err := <-execDone; err != nil {
		t.Fatalf("Execute: %v", err)
	}
	w.Close()
	<-readDone
	os.Stdout = old

	mu.Lock()
	full := output.String()
	mu.Unlock()
	if !strings.Contains(full, "Agent created: prog123") {
		t.Errorf("output missing agent result, got:\n%s", full)
	}
}

func TestSpawnCommand_ServerError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockSpawnServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-err",
				Version:  "v0.2.0",
			},
		},
		spawnErr: connect.NewError(connect.CodeInternal, nil),
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()

	writeSpawnTestConfig(t, tmpDir, srv.URL)

	cmd := NewSpawnCommand()
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error from server")
	}
}

func TestSpawnCommand_ServerNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockSpawnServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-nf",
				Version:  "v0.2.0",
			},
		},
		spawnResp: &v1.SpawnAgentResponse{
			AgentId: "noop",
		},
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()

	// Write config with a DIFFERENT server name, not "missing-server".
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

	cmd := NewSpawnCommand()
	cmd.SetArgs([]string{"--server", "missing-server"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for missing server")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}
