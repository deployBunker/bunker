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
	spawnResp *v1.SpawnAgentResponse
	spawnErr  error
}

func (m *mockSpawnServer) SpawnAgent(
	ctx context.Context,
	req *connect.Request[v1.SpawnAgentRequest],
) (*connect.Response[v1.SpawnAgentResponse], error) {
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
