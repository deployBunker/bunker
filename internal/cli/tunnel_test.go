//go:build unix

package cli

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// newTunnelTestServer starts an httptest server with a chi router mounting
// the connect handler for the given BunkerdHandler implementation.
func newTunnelTestServer(t *testing.T, handler bunkerv1connect.BunkerdHandler) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(handler)
	r.Mount(path, h)
	return httptest.NewServer(r)
}

// mockTunnelServer implements BunkerdHandler with configurable GetAgent response.
type mockTunnelServer struct {
	mockBunkerdServer
	getAgentResp *v1.GetAgentResponse
	getAgentErr  error
}

func (m *mockTunnelServer) GetAgent(
	ctx context.Context,
	req *connect.Request[v1.GetAgentRequest],
) (*connect.Response[v1.GetAgentResponse], error) {
	if m.getAgentErr != nil {
		return nil, m.getAgentErr
	}
	return connect.NewResponse(m.getAgentResp), nil
}

// writeTunnelTestConfig writes a CLIConfig with a single server entry
// pointing at the given URL, and sets it as the active server.
func writeTunnelTestConfig(t *testing.T, home, serverURL string) {
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

// writeTunnelKey writes the client-local SSH key for an agent under
// ~/.bunker/keys/<id> so the tunnel command passes its key-existence check.
func writeTunnelKey(t *testing.T, home, agentID string) string {
	t.Helper()
	keyPath, err := defaultSSHKeyPath(agentID)
	if err != nil {
		t.Fatalf("defaultSSHKeyPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("test-private-key"), 0600); err != nil {
		t.Fatalf("WriteFile key: %v", err)
	}
	return keyPath
}

func TestTunnelCommand_Help(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewTunnelCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		cmd.Execute()
	})

	if !strings.Contains(output, "SSH tunnel") {
		t.Errorf("help output missing SSH tunnel description, got:\n%s", output)
	}
	if !strings.Contains(output, "agent-id") {
		t.Errorf("help output missing agent-id argument, got:\n%s", output)
	}
	if !strings.Contains(output, "local-port") {
		t.Errorf("help output missing local-port argument, got:\n%s", output)
	}
}

func TestTunnelCommand_NoActiveServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewTunnelCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"abc123"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when no active server")
	}
}

func TestTunnelCommand_AgentNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentErr: connect.NewError(connect.CodeNotFound, nil),
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)

	cmd := NewTunnelCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"missing"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for not found agent")
	}
}

func TestTunnelCommand_NoTunnelCommand(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{AgentId: "abc123"},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)

	cmd := NewTunnelCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"abc123"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when agent has no tunnel command")
	}
	if !strings.Contains(err.Error(), "no docker host tunnel command") {
		t.Errorf("expected 'no docker host tunnel command' error, got: %v", err)
	}
}

func TestTunnelCommand_ExecutesStoredCommand(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:          "abc123",
				DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@bunker-mvp -N",
			},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)
	keyPath := writeTunnelKey(t, tmpDir, "abc123")

	// Capture the command that the tunnel command would execute.
	var capturedName string
	var capturedArgs []string
	oldExec := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedName = name
		capturedArgs = args
		return exec.CommandContext(ctx, "echo", "mock tunnel")
	}
	defer func() { execCommandContext = oldExec }()

	cmd := NewTunnelCommand()
	cmd.SetArgs([]string{"abc123"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if capturedName != "ssh" {
		t.Errorf("expected command name ssh, got %q", capturedName)
	}
	joined := strings.Join(capturedArgs, " ")
	for _, want := range []string{
		"StrictHostKeyChecking=no",
		"UserKnownHostsFile=/dev/null",
		"LogLevel=ERROR",
		"-i " + keyPath,
		"-L 2376:/run/bunker/abc123/docker.sock",
		"bunker-abc123@127.0.0.1",
		"-N",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("captured args missing %q, got: %q", want, joined)
		}
	}
	for _, absent := range []string{"/etc/bunkerd/ssh/", "@bunker-mvp"} {
		if strings.Contains(joined, absent) {
			t.Errorf("captured args must not contain %q, got: %q", absent, joined)
		}
	}
}

func TestTunnelCommand_CustomPort(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:          "abc123",
				DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@bunker-mvp -N",
			},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)
	writeTunnelKey(t, tmpDir, "abc123")

	var capturedArgs []string
	oldExec := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedArgs = args
		return exec.CommandContext(ctx, "echo", "mock tunnel")
	}
	defer func() { execCommandContext = oldExec }()

	cmd := NewTunnelCommand()
	cmd.SetArgs([]string{"abc123", "2377"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "-L 2377:/run/bunker/abc123/docker.sock") {
		t.Errorf("expected custom port 2377 in -L spec, got: %q", joined)
	}
}

func TestTunnelCommand_MissingKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:          "abc123",
				DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@bunker-mvp -N",
			},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)

	cmd := NewTunnelCommand()
	cmd.SetArgs([]string{"abc123"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when client-local SSH key is missing")
	}
	if !strings.Contains(err.Error(), "SSH key not found") {
		t.Errorf("expected 'SSH key not found' error, got: %v", err)
	}
}

func TestTunnelCommand_SshHostFlag(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:          "abc123",
				DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@bunker-mvp -N",
			},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)
	writeTunnelKey(t, tmpDir, "abc123")

	var capturedArgs []string
	oldExec := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedArgs = args
		return exec.CommandContext(ctx, "echo", "mock tunnel")
	}
	defer func() { execCommandContext = oldExec }()

	cmd := NewTunnelCommand()
	cmd.SetArgs([]string{"abc123", "--ssh-host", "somehost.example"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "bunker-abc123@somehost.example") {
		t.Errorf("expected --ssh-host override in target, got: %q", joined)
	}
	if strings.Contains(joined, "@bunker-mvp") || strings.Contains(joined, "@127.0.0.1") {
		t.Errorf("target host not overridden, got: %q", joined)
	}
}

func TestTunnelCommand_SshKeyFlag(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:          "abc123",
				DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@bunker-mvp -N",
			},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)

	customKey := filepath.Join(tmpDir, "custom-key")
	if err := os.WriteFile(customKey, []byte("custom-private-key"), 0600); err != nil {
		t.Fatalf("write custom key: %v", err)
	}

	var capturedArgs []string
	oldExec := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedArgs = args
		return exec.CommandContext(ctx, "echo", "mock tunnel")
	}
	defer func() { execCommandContext = oldExec }()

	cmd := NewTunnelCommand()
	cmd.SetArgs([]string{"abc123", "--ssh-key", customKey})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "-i "+customKey) {
		t.Errorf("expected --ssh-key override in args, got: %q", joined)
	}
	if strings.Contains(joined, "/etc/bunkerd/ssh/") {
		t.Errorf("args must not contain server-side key path, got: %q", joined)
	}
}

func TestTunnelCommand_InvalidPort(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewTunnelCommand()
	cmd.SetArgs([]string{"abc123", "not-a-port"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for invalid port")
	}
}

// Ensure imported packages are used (keeps compiler happy in test builds).
var _ = os.Stdout

// ── GAP-079: the tunnel must not outlive the CLI ────────────────────────────
//
// The live gate left orphaned root ssh sessions holding
// "-L 2376:/run/bunker/<agent>/docker.sock" with ppid=1 after the CLI that
// created them was killed by a signal (agent user already deleted).

// tunnelTestHostCommand is the server-baked tunnel command the mock bunkerd
// returns: the same shape the daemon stores (server-side key path and host),
// which the CLI rewrites into a client-local ssh invocation.
const tunnelTestHostCommand = "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@bunker-mvp -N"

// TestTunnelCommand_TunnelContextIndependentOfRPCD exercises the context the
// ssh child is started with, through the execCommandContext hook:
//
//   - it must have NO deadline. The 30s GetAgent deadline used to be handed
//     straight to exec, so the ssh child was killed ~30s after it opened even
//     though the help text promises the foreground "until interrupted".
//   - it must still be alive while the tunnel child runs, and be cancelled once
//     RunE returns — i.e. owned by the tunnel (signal.NotifyContext + stop()),
//     not chained to the RPC context.
func TestTunnelCommand_TunnelContextIndependentOfRPCD(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{AgentId: "abc123", DockerHostTunnel: tunnelTestHostCommand},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)
	writeTunnelKey(t, tmpDir, "abc123")

	type ctxCapture struct {
		ctx        context.Context
		deadline   bool
		deadlineAt time.Time
		err        error
	}
	captured := make(chan ctxCapture, 1)

	oldExec := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		dl, hasDeadline := ctx.Deadline()
		captured <- ctxCapture{ctx: ctx, deadline: hasDeadline, deadlineAt: dl, err: ctx.Err()}
		// A real, short-lived child: the assertions below are about the context
		// the tunnel hands to exec, not about ssh itself.
		c := exec.CommandContext(ctx, "sleep", "1")
		c.Stdout, c.Stderr = io.Discard, io.Discard
		return c
	}
	defer func() { execCommandContext = oldExec }()

	done := make(chan error, 1)
	go func() {
		cmd := NewTunnelCommand()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"abc123"})
		done <- cmd.Execute()
	}()

	var got ctxCapture
	select {
	case got = <-captured:
	case <-time.After(15 * time.Second):
		t.Fatal("the tunnel never reached the ssh exec")
	}

	if got.deadline {
		t.Fatalf("GAP-079: the tunnel context has a deadline (%s from now) — the long-lived ssh child is still bounded by the GetAgent RPC deadline",
			time.Until(got.deadlineAt))
	}
	if got.err != nil {
		t.Fatalf("the tunnel context was already done when the child started: %v", got.err)
	}

	// The RPC has returned (the ssh command was built from its response) and the
	// tunnel child is running: its context must not be done.
	select {
	case <-got.ctx.Done():
		t.Fatal("GAP-079: the tunnel context was cancelled while the tunnel child was still running — it is chained to the RPC context")
	case <-time.After(250 * time.Millisecond):
	}

	if err := <-done; err != nil {
		t.Fatalf("Execute: %v", err)
	}

	select {
	case <-got.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel context was not cancelled after RunE returned (signal context stop() leak)")
	}
}

// TestTunnelCommand_SignalReapsSSHChild is the load-bearing GAP-079 test. It
// builds the real CLI (go build ./cmd/bunker), points it at the in-process mock
// bunkerd, puts a fake `ssh` first on PATH that records its own pid, spawns a
// long-lived grandchild and waits for it, then SIGTERMs the CLI and asserts the
// whole subtree is gone.
//
// Pre-fix the CLI died on the signal with no cleanup at all: the ssh child was
// reparented to init and kept its -L forward (and the root sshd session behind
// it) forever.
func TestTunnelCommand_SignalReapsSSHChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-group semantics")
	}
	tmp := t.TempDir()

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:          "e2e-main",
				DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/e2e-main -L 2376:/run/bunker/e2e-main/docker.sock bunker-e2e-main@bunker-mvp -N",
			},
		},
	})
	defer server.Close()

	// CLI state: config.yaml (points at the mock server) + the agent key.
	home := filepath.Join(tmp, "bunker-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", home, err)
	}
	t.Setenv("BUNKER_HOME", home)
	// NOTE: HOME is set for the CLI CHILD only (below), never for the test
	// process — overriding it here would relocate the go build's module cache
	// (GOPATH defaults to $HOME/go) into the temp dir.
	writeTunnelTestConfig(t, home, server.URL)
	writeTunnelKey(t, home, "e2e-main")

	// Fake ssh: records its pid, spawns a grandchild that would outlive any
	// sane grace period, and waits for it (so the ssh child stays alive).
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", binDir, err)
	}
	sshPIDFile := filepath.Join(tmp, "ssh.pid")
	grandchildPIDFile := filepath.Join(tmp, "ssh-grandchild.pid")
	sshScript := "#!/bin/sh\n" +
		"echo \"$$\" > \"$FAKE_SSH_PID_FILE\"\n" +
		"sleep 300 &\n" +
		"echo \"$!\" > \"$FAKE_SSH_GRANDCHILD_PID_FILE\"\n" +
		"wait\n"
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte(sshScript), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}

	// GAP-090 load hygiene: one shared per-session build (procbuild_test.go);
	// the per-test `go build` here was the suite's worst host-load offender.
	cliBin := buildCLIOnce(t)

	cliLog := filepath.Join(tmp, "cli.log")
	logFile, err := os.Create(cliLog)
	if err != nil {
		t.Fatalf("create %s: %v", cliLog, err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	cli := exec.Command(cliBin, "tunnel", "e2e-main")
	cli.Dir = tmp
	cli.Env = append(os.Environ(),
		"BUNKER_HOME="+home,
		"HOME="+tmp,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_SSH_PID_FILE="+sshPIDFile,
		"FAKE_SSH_GRANDCHILD_PID_FILE="+grandchildPIDFile,
	)
	// Plain files, not pipes: the orphan we are hunting for inherits the CLI's
	// stdout/stderr, and a pipe would keep Wait blocked on the copy goroutine.
	cli.Stdout, cli.Stderr = logFile, logFile

	if err := cli.Start(); err != nil {
		t.Fatalf("start CLI: %v", err)
	}

	sshPID, grandchildPID := 0, 0
	// Clean up whatever survives, so a failing (pre-fix) run does not itself
	// leak the orphaned ssh forward this test exists to prevent.
	t.Cleanup(func() {
		if cli.Process != nil {
			_ = cli.Process.Kill()
		}
		for _, pid := range []int{sshPID, grandchildPID} {
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	sshPID = waitForPIDFile(t, sshPIDFile, 15*time.Second, cliLog)
	grandchildPID = waitForPIDFile(t, grandchildPIDFile, 15*time.Second, cliLog)

	// Premise: the fake ssh (and its grandchild) really are running, otherwise
	// the assertions below would be vacuously true.
	for name, pid := range map[string]int{"fake ssh": sshPID, "fake ssh grandchild": grandchildPID} {
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("%s (pid %d) is not running before the signal: %v", name, pid, err)
		}
	}

	if err := cli.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM the CLI: %v", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- cli.Wait() }()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatalf("the CLI did not exit within 10s of SIGTERM (log:\n%s)", readFileOr(cliLog))
	}

	// The load-bearing assertion: nothing the CLI started may survive it.
	deadline := time.Now().Add(5 * time.Second)
	for _, child := range []struct {
		name string
		pid  int
	}{
		{"the ssh child", sshPID},
		{"the ssh grandchild", grandchildPID},
	} {
		for {
			if err := syscall.Kill(child.pid, 0); err != nil {
				break // ESRCH: gone
			}
			if time.Now().After(deadline) {
				t.Errorf("GAP-079: %s (pid %d) survived a SIGTERM to the CLI — the tunnel subtree was orphaned", child.name, child.pid)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// TestTunnelCommand_SIGKILLReapsSSHChild proves the kernel BACKSTOP, i.e. the
// case a signal-handling teardown can never cover: the CLI is killed with an
// UNCATCHABLE signal (kill -9 / OOM kill / a manager's cleanup), so no context
// is cancelled, no deferred function runs and no reaper executes — and the ssh
// child is still gone. Before the backstop, that child was reparented to init
// with its "-L 2376:.../docker.sock" forward (and the root sshd session behind
// it) alive forever.
//
// Same harness as TestTunnelCommand_SignalReapsSSHChild (real CLI built with
// `go build ./cmd/bunker`, in-process mock bunkerd, fake ssh that records its
// pid, spawns a `sleep 300` grandchild and waits); only the kill signal differs.
//
// SCOPE, measured rather than assumed: Pdeathsig is a PARENT-DEATH signal for
// the DIRECT child — the process that owns the forward — and it is delivered
// when the thread that created the child exits. A process that no longer exists
// cannot signal anything, so the fake ssh's own grandchild cannot die on this
// path by any user-space mechanism (verified with a standalone probe: with
// Setpgid+Pdeathsig, SIGKILLing the parent leaves the direct child GONE and its
// `sleep 300` grandchild ALIVE). What the real command leaks is the ssh client
// and, through it, the remote sshd session — both of which die with the direct
// child, whose death closes the TCP connection the session rides on. The
// grandchild is therefore logged as an observation and only its LIVENESS BEFORE
// the kill is asserted, so the test cannot pass vacuously with a dead fixture.
func TestTunnelCommand_SIGKILLReapsSSHChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX parent-death / process-group semantics")
	}
	tmp := t.TempDir()

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:          "e2e-main",
				DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/e2e-main -L 2376:/run/bunker/e2e-main/docker.sock bunker-e2e-main@bunker-mvp -N",
			},
		},
	})
	defer server.Close()

	// CLI state: config.yaml (points at the mock server) + the agent key.
	home := filepath.Join(tmp, "bunker-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", home, err)
	}
	t.Setenv("BUNKER_HOME", home)
	// NOTE: HOME is set for the CLI CHILD only (below), never for the test
	// process — overriding it here would relocate the go build's module cache
	// (GOPATH defaults to $HOME/go) into the temp dir.
	writeTunnelTestConfig(t, home, server.URL)
	writeTunnelKey(t, home, "e2e-main")

	// Fake ssh: records its pid, spawns a grandchild that would outlive any
	// sane grace period, and waits for it (so the ssh child stays alive).
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", binDir, err)
	}
	sshPIDFile := filepath.Join(tmp, "ssh.pid")
	grandchildPIDFile := filepath.Join(tmp, "ssh-grandchild.pid")
	sshScript := "#!/bin/sh\n" +
		"echo \"$$\" > \"$FAKE_SSH_PID_FILE\"\n" +
		"sleep 300 &\n" +
		"echo \"$!\" > \"$FAKE_SSH_GRANDCHILD_PID_FILE\"\n" +
		"wait\n"
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte(sshScript), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}

	// GAP-090 load hygiene: one shared per-session build (procbuild_test.go);
	// the per-test `go build` here was the suite's worst host-load offender.
	cliBin := buildCLIOnce(t)

	cliLog := filepath.Join(tmp, "cli.log")
	logFile, err := os.Create(cliLog)
	if err != nil {
		t.Fatalf("create %s: %v", cliLog, err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	cli := exec.Command(cliBin, "tunnel", "e2e-main")
	cli.Dir = tmp
	cli.Env = append(os.Environ(),
		"BUNKER_HOME="+home,
		"HOME="+tmp,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_SSH_PID_FILE="+sshPIDFile,
		"FAKE_SSH_GRANDCHILD_PID_FILE="+grandchildPIDFile,
	)
	// Plain files, not pipes: the orphan we are hunting for inherits the CLI's
	// stdout/stderr, and a pipe would keep Wait blocked on the copy goroutine.
	cli.Stdout, cli.Stderr = logFile, logFile

	if err := cli.Start(); err != nil {
		t.Fatalf("start CLI: %v", err)
	}

	sshPID, grandchildPID := 0, 0
	// Clean up whatever survives, so a failing (pre-backstop) run does not
	// itself leak the orphaned ssh forward this test exists to prevent.
	t.Cleanup(func() {
		if cli.Process != nil {
			_ = cli.Process.Kill()
		}
		for _, pid := range []int{sshPID, grandchildPID} {
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	sshPID = waitForPIDFile(t, sshPIDFile, 15*time.Second, cliLog)
	grandchildPID = waitForPIDFile(t, grandchildPIDFile, 15*time.Second, cliLog)

	// Premise: the fake ssh (and its grandchild) really are running, otherwise
	// the assertion below would be vacuously true.
	for name, pid := range map[string]int{"fake ssh": sshPID, "fake ssh grandchild": grandchildPID} {
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("%s (pid %d) is not running before the kill: %v", name, pid, err)
		}
	}

	// The uncatchable kill: SIGKILL to the CLI. Nothing in the CLI can run
	// afterwards — the ssh child's death can only come from the kernel.
	if err := cli.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL the CLI: %v", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- cli.Wait() }()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatalf("the CLI did not exit within 10s of SIGKILL (log:\n%s)", readFileOr(cliLog))
	}

	// The load-bearing assertion: the ssh child — the process holding the
	// docker-sock forward — must be gone even though the CLI had no chance to
	// run a line of code after the signal.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(sshPID, 0); err != nil {
			break // ESRCH: the parent-death backstop fired
		}
		if time.Now().After(deadline) {
			t.Fatalf("GAP-079: the ssh child (pid %d) survived a SIGKILL to the CLI — no parent-death backstop is armed (log:\n%s)",
				sshPID, readFileOr(cliLog))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Observation only (see the SCOPE note above): the grandchild is beyond the
	// reach of a dead process, so its state is reported, not asserted.
	time.Sleep(300 * time.Millisecond)
	grandchildState := "gone"
	if err := syscall.Kill(grandchildPID, 0); err == nil {
		grandchildState = "still running (expected: Pdeathsig is direct-child only; the group teardown cannot run because the CLI was SIGKILLed)"
	}
	t.Logf("SIGKILL backstop: ssh child pid %d gone; grandchild pid %d %s", sshPID, grandchildPID, grandchildState)
}

// waitForPIDFile waits for a pid file written by the fake ssh helper to appear
// with a positive pid, and returns it.
func waitForPIDFile(t *testing.T, path string, within time.Duration, cliLog string) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid in %s after %s (CLI log:\n%s)", path, within, readFileOr(cliLog))
	return 0
}

// readFileOr returns the file's contents, or the read error as text.
func readFileOr(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return string(raw)
}
