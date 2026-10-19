//go:build unix

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── the fixture surface ─────────────────────────────────────────────────────
//
// The agent record the mock bunkerd returns: the SAME sshfs-mount/tunnel
// shapes the daemon bakes (server-local key path + self-reported hostname),
// which the connect command must rewrite into a client-local ssh invocation.

const surfaceConnectTestMount = "sshfs -o IdentityFile=/etc/bunkerd/ssh/abc123 -o idmap=user -o allow_other bunker-abc123@bunker-mvp:/home/bunker-abc123 /mnt/bunker/abc123"

const surfaceConnectTestTunnel = "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@bunker-mvp -N"

// surfaceConnectMock serves GetAgent (from mockTunnelServer) plus the ONE
// ExecAgent the connect path runs (`id -u`) — the packageFlowFake pattern.
type surfaceConnectMock struct {
	mockTunnelServer
	uid     string
	uidErr  error
	scripts []string
}

func (m *surfaceConnectMock) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest],
	stream *connect.ServerStream[v1.ExecAgentResponse]) error {

	args := req.Msg.GetArgs()
	script := ""
	if n := len(args); n > 0 {
		script = args[n-1]
	}
	m.scripts = append(m.scripts, script)
	if m.uidErr != nil {
		return m.uidErr
	}
	return stream.Send(&v1.ExecAgentResponse{
		Output:   &v1.ExecAgentResponse_Stdout{Stdout: []byte(m.uid + "\n")},
		ExitCode: 0,
	})
}

// setupSurfaceConnectTest wires config + key + mock server for one in-process
// run and returns the mock, the server URL and the temp home.
func setupSurfaceConnectTest(t *testing.T, agent *v1.AgentSummary, uid string) (*surfaceConnectMock, string, string) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &surfaceConnectMock{
		mockTunnelServer: mockTunnelServer{
			getAgentResp: &v1.GetAgentResponse{Agent: agent},
		},
		uid: uid,
	}
	server := newTunnelTestServer(t, mock)
	t.Cleanup(server.Close)
	writeTunnelTestConfig(t, tmpDir, server.URL)
	writeTunnelKey(t, tmpDir, "abc123")
	return mock, server.URL, tmpDir
}

// captureSurfaceConnectExec swaps execCommandContext for a recorder that
// returns a short-lived real child (echo), mirroring the tunnel tests.
func captureSurfaceConnectExec(t *testing.T) (name *string, args *[]string) {
	t.Helper()
	var capturedName string
	var capturedArgs []string
	oldExec := execCommandContext
	execCommandContext = func(ctx context.Context, cmdName string, cmdArgs ...string) *exec.Cmd {
		capturedName = cmdName
		capturedArgs = cmdArgs
		c := exec.CommandContext(ctx, "echo", "mock ssh forward")
		c.Stdout = io.Discard
		c.Stderr = io.Discard
		return c
	}
	t.Cleanup(func() { execCommandContext = oldExec })
	return &capturedName, &capturedArgs
}

// stubSurfaceWait makes the post-start socket wait succeed AND publishes a
// real 0700 unix socket at the requested path — what a StreamLocalBindMask=
// 0077 forward really does — so the RunE's stat-it-back assertion is exercised
// by every flow test instead of being stubbed away (the deliverFileFunc
// pattern). The socket is removed at test cleanup.
func stubSurfaceWait(t *testing.T) {
	t.Helper()
	old := surfaceWaitForSocket
	surfaceWaitForSocket = func(ctx context.Context, path string, within time.Duration, child *exec.Cmd) error {
		ln, err := net.Listen("unix", path)
		if err != nil {
			return err
		}
		t.Cleanup(func() { _ = ln.Close() })
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
		return nil
	}
	t.Cleanup(func() { surfaceWaitForSocket = old })
}

// ── pure units ──────────────────────────────────────────────────────────────

func TestSurfaceConnectLocalSocketDefault(t *testing.T) {
	got := surfaceConnectLocalSocketDefault("abc123")
	if got != "/tmp/bunker-toolsd-abc123.sock" {
		t.Errorf("default local socket = %q, want /tmp/bunker-toolsd-abc123.sock", got)
	}
}

func TestResolveSurfaceConnectTarget(t *testing.T) {
	entry := ServerEntry{URL: "http://127.0.0.1:19090"}
	sshfsAgent := &v1.AgentSummary{AgentId: "abc123", SshfsMount: surfaceConnectTestMount}
	tunnelOnlyAgent := &v1.AgentSummary{AgentId: "abc123", DockerHostTunnel: surfaceConnectTestTunnel}
	emptyAgent := &v1.AgentSummary{AgentId: "abc123"}

	t.Run("sshfs mount is the primary source", func(t *testing.T) {
		got, err := resolveSurfaceConnectTarget(entry, sshfsAgent, "")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.UserAtHost != "bunker-abc123@127.0.0.1" {
			t.Errorf("UserAtHost = %q, want bunker-abc123@127.0.0.1 (host resolved from server URL, not the daemon-hostname record value)", got.UserAtHost)
		}
		if got.ServerHost != "bunker-mvp" {
			t.Errorf("ServerHost = %q, want bunker-mvp", got.ServerHost)
		}
	})

	t.Run("docker tunnel is the fallback", func(t *testing.T) {
		got, err := resolveSurfaceConnectTarget(entry, tunnelOnlyAgent, "")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.UserAtHost != "bunker-abc123@127.0.0.1" {
			t.Errorf("UserAtHost = %q, want bunker-abc123@127.0.0.1", got.UserAtHost)
		}
	})

	t.Run("empty record is a named error", func(t *testing.T) {
		_, err := resolveSurfaceConnectTarget(entry, emptyAgent, "")
		if err == nil {
			t.Fatal("expected an error for a record with neither sshfs mount nor tunnel")
		}
		if !strings.Contains(err.Error(), "cannot resolve an SSH target from the agent record") {
			t.Errorf("error must name the resolution failure, got: %v", err)
		}
	})

	t.Run("nil record is a named error", func(t *testing.T) {
		_, err := resolveSurfaceConnectTarget(entry, nil, "")
		if err == nil || !strings.Contains(err.Error(), "agent record is empty") {
			t.Fatalf("expected the empty-record error, got: %v", err)
		}
	})

	t.Run("ssh-host flag wins over every resolution", func(t *testing.T) {
		got, err := resolveSurfaceConnectTarget(entry, sshfsAgent, "override.example")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.UserAtHost != "bunker-abc123@override.example" {
			t.Errorf("UserAtHost = %q, want the flag host", got.UserAtHost)
		}
	})
}

func TestBuildSurfaceConnectSSHArgs(t *testing.T) {
	opts := surfaceConnectOptions{
		AgentID:    "abc123",
		LocalPath:  "/tmp/bunker-toolsd-abc123.sock",
		RemotePath: "/run/bunker/abc123/toolsd.sock",
		UserAtHost: "bunker-abc123@203.0.113.10",
		KeyPath:    "/home/o/.bunker/keys/abc123",
	}
	args := buildSurfaceConnectSSHArgs(opts)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-o StrictHostKeyChecking=no",
		"-o UserKnownHostsFile=/dev/null",
		"-o LogLevel=ERROR",
		"-o IdentitiesOnly=yes",
		"-o ExitOnForwardFailure=yes",
		"-o StreamLocalBindMask=0077",
		"-i /home/o/.bunker/keys/abc123",
		"-N",
		"-L /tmp/bunker-toolsd-abc123.sock:/run/bunker/abc123/toolsd.sock",
		"bunker-abc123@203.0.113.10",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q, got: %q", want, joined)
		}
	}
	if !strings.HasSuffix(args[len(args)-1], "@203.0.113.10") {
		t.Errorf("the login target must be the LAST positional arg, got tail: %q", args[len(args)-3:])
	}
}

// TestSurfaceAssertLocalSocketMode: the READY gate is the stat-it-back — a
// real socket at 0700 passes; any wider mode, or a non-socket path, fails.
func TestSurfaceAssertLocalSocketMode(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.sock")
	ln, err := net.Listen("unix", good)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	// The temp dir's own mode can leak through bind-time umask application on
	// some hosts; the assertion under test is about the FILE mode, so set it
	// explicitly (what StreamLocalBindMask guarantees in production).
	if err := os.Chmod(good, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := surfaceAssertLocalSocketMode(good); err != nil {
		t.Errorf("a 0700 socket must pass, got: %v", err)
	}

	wide := filepath.Join(dir, "wide.sock")
	ln2, err := net.Listen("unix", wide)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln2.Close() })
	if err := os.Chmod(wide, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	err = surfaceAssertLocalSocketMode(wide)
	if err == nil || !strings.Contains(err.Error(), "0700") {
		t.Errorf("a wider socket must be refused naming 0700, got: %v", err)
	}

	err = surfaceAssertLocalSocketMode(filepath.Join(dir, "absent.sock"))
	if err == nil {
		t.Error("an absent socket must be refused")
	}
	if err := surfaceAssertLocalSocketMode(dir); err == nil {
		t.Error("a non-socket path must be refused")
	}
}

func TestParseSurfaceCheckOutput(t *testing.T) {
	out := "exec 2>&1 noise\nWarning: something\n" +
		"CHECK	socket_active=active\n" +
		"CHECK	socket_meta=bunker-abc123:600\n" +
		"CHECK	dir_meta=bunker-abc123:700\n" +
		"CHECK	toolsd_procs=0\n" +
		"CHECK	tcp_listeners=0\n"
	active, meta, dirMeta, procs, tcp := parseSurfaceCheckOutput(out)
	if active != "active" || meta != "bunker-abc123:600" || dirMeta != "bunker-abc123:700" || procs != "0" || tcp != "0" {
		t.Errorf("parse = (%q, %q, %q, %q, %q), want the five CHECK values", active, meta, dirMeta, procs, tcp)
	}

	active, meta, dirMeta, procs, tcp = parseSurfaceCheckOutput("no check lines at all\n")
	if active != "" || meta != "" || dirMeta != "" || procs != "" || tcp != "" {
		t.Errorf("missing lines must parse empty, got (%q, %q, %q, %q, %q)", active, meta, dirMeta, procs, tcp)
	}
}

func TestEvaluateSurfaceCheck(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "active", "bunker-abc123:600", "bunker-abc123:700", "0", "0", "bunker-abc123")
		if !st.OK {
			t.Errorf("healthy state must pass, failures: %v", st.Failures)
		}
		if st.Owner != "bunker-abc123" || st.Mode != "600" || st.ToolsdProcs != "0" || st.DirMode != "700" || st.TCPListeners != "0" {
			t.Errorf("state fields wrong: %+v", st)
		}
	})
	t.Run("inactive socket named", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "inactive", "bunker-abc123:600", "bunker-abc123:700", "0", "0", "bunker-abc123")
		if st.OK || len(st.Failures) != 1 || !strings.Contains(st.Failures[0], "socket_active: inactive") {
			t.Errorf("inactive socket must be one named failure, got: %+v", st)
		}
	})
	t.Run("empty systemctl answer named", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "", "bunker-abc123:600", "bunker-abc123:700", "0", "0", "bunker-abc123")
		if st.OK || !strings.Contains(strings.Join(st.Failures, "; "), "no answer from systemctl") {
			t.Errorf("empty active must be a named failure, got: %+v", st)
		}
	})
	t.Run("wrong owner named", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "active", "root:600", "bunker-abc123:700", "0", "0", "bunker-abc123")
		if st.OK || !strings.Contains(strings.Join(st.Failures, "; "), `owner is "root"`) {
			t.Errorf("wrong owner must be named, got: %+v", st)
		}
	})
	t.Run("loose mode named", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "active", "bunker-abc123:644", "bunker-abc123:700", "0", "0", "bunker-abc123")
		if st.OK || !strings.Contains(strings.Join(st.Failures, "; "), `mode is "644"`) {
			t.Errorf("loose mode must be named, got: %+v", st)
		}
	})
	t.Run("absent socket named", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "active", "absent", "bunker-abc123:700", "0", "0", "bunker-abc123")
		if st.OK || !strings.Contains(strings.Join(st.Failures, "; "), "socket file is absent") {
			t.Errorf("absent socket must be named, got: %+v", st)
		}
	})
	t.Run("wide runtime dir named", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "active", "bunker-abc123:600", "bunker-abc123:755", "0", "0", "bunker-abc123")
		if st.OK || !strings.Contains(strings.Join(st.Failures, "; "), `dir_meta: mode is "755"`) {
			t.Errorf("a wider runtime dir must be named, got: %+v", st)
		}
	})
	t.Run("foreign runtime-dir owner named", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "active", "bunker-abc123:600", "root:700", "0", "0", "bunker-abc123")
		if st.OK || !strings.Contains(strings.Join(st.Failures, "; "), "dir_meta: owner") {
			t.Errorf("a foreign runtime-dir owner must be named, got: %+v", st)
		}
	})
	t.Run("absent runtime dir named", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "active", "bunker-abc123:600", "absent", "0", "0", "bunker-abc123")
		if st.OK || !strings.Contains(strings.Join(st.Failures, "; "), "dir_meta: the runtime directory") {
			t.Errorf("an absent runtime dir must be named, got: %+v", st)
		}
	})
	t.Run("nonzero pre-connect procs named", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "active", "bunker-abc123:600", "bunker-abc123:700", "2", "0", "bunker-abc123")
		if st.OK || !strings.Contains(strings.Join(st.Failures, "; "), "toolsd_procs: 2") {
			t.Errorf("pre-running toolsd must be named, got: %+v", st)
		}
	})
	t.Run("missing pgrep answer named", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "active", "bunker-abc123:600", "bunker-abc123:700", "", "0", "bunker-abc123")
		if st.OK || !strings.Contains(strings.Join(st.Failures, "; "), "no answer from pgrep") || st.ToolsdProcs != "unknown" {
			t.Errorf("missing proc count must be a named unknown, got: %+v", st)
		}
	})
	t.Run("tcp nonzero named", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "active", "bunker-abc123:600", "bunker-abc123:700", "0", "3", "bunker-abc123")
		if st.OK || !strings.Contains(strings.Join(st.Failures, "; "), "tcp_listeners: 3") {
			t.Errorf("a TCP listener must be named, got: %+v", st)
		}
	})
	t.Run("tcp unknown is a failure not a pass", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "active", "bunker-abc123:600", "bunker-abc123:700", "0", "unknown", "bunker-abc123")
		if st.OK || !strings.Contains(strings.Join(st.Failures, "; "), "UNPROVEN") {
			t.Errorf("tcp=unknown must fail naming the unproven property, got: %+v", st)
		}
	})
	t.Run("missing tcp answer named", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "active", "bunker-abc123:600", "bunker-abc123:700", "0", "", "bunker-abc123")
		if st.OK || !strings.Contains(strings.Join(st.Failures, "; "), "no answer from the ss probe") {
			t.Errorf("a missing tcp answer must be named, got: %+v", st)
		}
	})
	t.Run("every failure accumulates", func(t *testing.T) {
		st := evaluateSurfaceCheck("abc123", "failed", "root:777", "root:777", "9", "9", "bunker-abc123")
		if st.OK || len(st.Failures) != 5 {
			t.Errorf("all five failures must be reported, got: %+v", st)
		}
	})
}

func TestSurfaceCheckAgentUser(t *testing.T) {
	if got := surfaceCheckAgentUser("abc123"); got != "bunker-abc123" {
		t.Errorf("agent user = %q, want bunker-abc123", got)
	}
}

// ── command-level flow (mock transport, captured exec) ──────────────────────

func TestSurfaceConnectCommand_ExecutesForward(t *testing.T) {
	agent := &v1.AgentSummary{AgentId: "abc123", SshfsMount: surfaceConnectTestMount}
	setupSurfaceConnectTest(t, agent, "4242")
	name, args := captureSurfaceConnectExec(t)
	stubSurfaceWait(t)

	var out bytes.Buffer
	cmd := NewSurfaceCommand()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"connect", "abc123"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if *name != "ssh" {
		t.Errorf("expected command name ssh, got %q", *name)
	}
	joined := strings.Join(*args, " ")
	for _, want := range []string{
		"-i", // the client-local key (exact path asserted below)
		"-N",
		"-L /tmp/bunker-toolsd-abc123.sock:/run/bunker/abc123/toolsd.sock",
		"StreamLocalBindMask=0077",
		"bunker-abc123@127.0.0.1",
		"ExitOnForwardFailure=yes",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("captured args missing %q, got: %q", want, joined)
		}
	}
	if strings.Contains(joined, "bunker-mvp") {
		t.Errorf("the daemon-hostname host must never reach the ssh target, got: %q", joined)
	}
	keyPath, _ := defaultSSHKeyPath("abc123")
	if !strings.Contains(joined, "-i "+keyPath) {
		t.Errorf("expected the client-local key %q, got: %q", keyPath, joined)
	}
	if !strings.Contains(out.String(), "READY: /tmp/bunker-toolsd-abc123.sock") {
		t.Errorf("expected a READY line naming the local path, got:\n%s", out.String())
	}
}

func TestSurfaceConnectCommand_NoExecPathDerivation(t *testing.T) {
	agent := &v1.AgentSummary{AgentId: "abc123", SshfsMount: surfaceConnectTestMount}
	mock, _, _ := setupSurfaceConnectTest(t, agent, "") // any exec call fails the run
	_, args := captureSurfaceConnectExec(t)
	stubSurfaceWait(t)

	cmd := NewSurfaceCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"connect", "abc123"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(mock.scripts) != 0 {
		t.Errorf("SURF-B2: connect must derive the remote path from the agent id and run NO exec, but the agent ran: %q", mock.scripts)
	}
	if !strings.Contains(strings.Join(*args, " "), "/run/bunker/abc123/toolsd.sock") {
		t.Errorf("the remote path must be the per-agent runtime-dir socket, got: %q", strings.Join(*args, " "))
	}
}

func TestSurfaceConnectCommand_InvalidAgentIDRefused(t *testing.T) {
	setupSurfaceConnectTest(t, &v1.AgentSummary{AgentId: "abc123", SshfsMount: surfaceConnectTestMount}, "")
	captureSurfaceConnectExec(t)
	stubSurfaceWait(t)

	cmd := NewSurfaceCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"connect", "../evil"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected a refusal for an agent id outside [a-z0-9-]")
	}
	if !strings.Contains(err.Error(), "only [a-z0-9-]") {
		t.Errorf("refusal must name the id rule, got: %v", err)
	}
}

func TestSurfaceConnectCommand_RPCFailureSurfaces(t *testing.T) {
	agent := &v1.AgentSummary{AgentId: "abc123", SshfsMount: surfaceConnectTestMount}
	mock, _, _ := setupSurfaceConnectTest(t, agent, "")
	mock.getAgentErr = connect.NewError(connect.CodeUnavailable, errors.New("agent down"))
	captureSurfaceConnectExec(t)
	stubSurfaceWait(t)

	cmd := NewSurfaceCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"connect", "abc123"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected the RPC failure to surface")
	}
	if !strings.Contains(err.Error(), "get agent") {
		t.Errorf("error must name the failed step, got: %v", err)
	}
}

func TestSurfaceConnectCommand_MissingKey(t *testing.T) {
	agent := &v1.AgentSummary{AgentId: "abc123", SshfsMount: surfaceConnectTestMount}
	setupSurfaceConnectTest(t, agent, "4242") // writes the key; remove it below
	keyPath, err := defaultSSHKeyPath("abc123")
	if err != nil {
		t.Fatalf("defaultSSHKeyPath: %v", err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("remove key: %v", err)
	}
	captureSurfaceConnectExec(t)
	stubSurfaceWait(t)

	cmd := NewSurfaceCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"connect", "abc123"})
	err = cmd.Execute()
	if err == nil {
		t.Fatal("expected an error when the client-local SSH key is missing")
	}
	if !strings.Contains(err.Error(), "SSH key not found") {
		t.Errorf("expected 'SSH key not found', got: %v", err)
	}
}

func TestSurfaceConnectCommand_FailClosedBinding(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(SessionTargetEnvVar, "") // no session binding, no --server

	cmd := NewSurfaceCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"connect", "abc123"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("GAP-093: connect is mutating-adjacent and must refuse without an explicit binding")
	}
	var noTarget *ErrNoTarget
	if !errors.As(err, &noTarget) {
		t.Errorf("expected ErrNoTarget, got: %v", err)
	}
}

func TestSurfaceConnectCommand_LocalSocketBusyRefused(t *testing.T) {
	// unix-only file: unix sockets always exist here.
	agent := &v1.AgentSummary{AgentId: "abc123", SshfsMount: surfaceConnectTestMount}
	_, _, tmpDir := setupSurfaceConnectTest(t, agent, "4242")
	captureSurfaceConnectExec(t)
	stubSurfaceWait(t)

	// A live local socket at the requested path: the command must refuse
	// instead of silently replacing another session's forward.
	sockPath := filepath.Join(tmpDir, "busy.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	cmd := NewSurfaceCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"connect", "abc123", "--socket-path", sockPath})
	err = cmd.Execute()
	if err == nil {
		t.Fatal("expected a refusal to clobber a live local socket")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("refusal must name the live socket, got: %v", err)
	}
}

// TestSurfaceConnectCommand_TunnelContextContract mirrors
// TestTunnelCommand_TunnelContextIndependentOfRPCD: the ssh child's context
// must have NO deadline (the forward is foreground until interrupted) and
// must be cancelled once RunE returns (the teardown runs, no leak).
func TestSurfaceConnectCommand_TunnelContextContract(t *testing.T) {
	agent := &v1.AgentSummary{AgentId: "abc123", SshfsMount: surfaceConnectTestMount}
	setupSurfaceConnectTest(t, agent, "4242")

	type ctxCapture struct {
		ctx      context.Context
		deadline bool
		err      error
	}
	captured := make(chan ctxCapture, 1)
	oldExec := execCommandContext
	execCommandContext = func(ctx context.Context, cmdName string, cmdArgs ...string) *exec.Cmd {
		_, hasDeadline := ctx.Deadline()
		captured <- ctxCapture{ctx: ctx, deadline: hasDeadline, err: ctx.Err()}
		c := exec.CommandContext(ctx, "sleep", "1")
		c.Stdout, c.Stderr = io.Discard, io.Discard
		return c
	}
	t.Cleanup(func() { execCommandContext = oldExec })
	stubSurfaceWait(t)

	done := make(chan error, 1)
	go func() {
		cmd := NewSurfaceCommand()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"connect", "abc123"})
		done <- cmd.Execute()
	}()

	var got ctxCapture
	select {
	case got = <-captured:
	case <-time.After(15 * time.Second):
		t.Fatal("the connect command never reached the ssh exec")
	}
	if got.deadline {
		t.Fatal("GAP-079: the forward context has a deadline — a long-lived ssh child must not inherit one")
	}
	if got.err != nil {
		t.Fatalf("the forward context was already done when the child started: %v", got.err)
	}
	select {
	case <-got.ctx.Done():
		t.Fatal("GAP-079: the forward context was cancelled while the child was still running — chained to an RPC context")
	case <-time.After(250 * time.Millisecond):
	}

	if err := <-done; err != nil {
		t.Fatalf("Execute: %v", err)
	}
	select {
	case <-got.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the forward context was not cancelled after RunE returned (signal context stop() leak)")
	}
}

// ── the live checks (--check) ───────────────────────────────────────────────

func TestSurfaceCheckCommand_HappyPath(t *testing.T) {
	checkScript = nil // reset the script sink across subtests
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	agent := &v1.AgentSummary{AgentId: "abc123", SshfsMount: surfaceConnectTestMount}
	server := newTunnelTestServer(t, &surfaceCheckScriptMock{
		mockTunnelServer: mockTunnelServer{
			getAgentResp: &v1.GetAgentResponse{Agent: agent},
		},
		checkOut:   "CHECK	socket_active=active\nCHECK	socket_meta=bunker-abc123:600\nCHECK	dir_meta=bunker-abc123:700\nCHECK	toolsd_procs=0\nCHECK	tcp_listeners=0\n",
		scriptSink: func(script string) { checkScript = append(checkScript, script) },
	})
	t.Cleanup(server.Close)
	writeTunnelTestConfig(t, tmpDir, server.URL)

	var out bytes.Buffer
	cmd := NewSurfaceCommand()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"connect", "abc123", "--check"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The ONE exec must be the check script, and it must ask the recipe's
	// questions — addressed to the PER-AGENT runtime dir (SURF-B2).
	if len(checkScript) != 1 {
		t.Fatalf("expected exactly one exec, got %d: %q", len(checkScript), checkScript)
	}
	script := checkScript[0]
	for _, want := range []string{
		"systemctl --user is-active toolsd.socket",
		"stat -c '%U'",
		"pgrep -c -x toolsd",
		"/run/bunker/abc123/toolsd.sock",
		"d=/run/bunker/abc123",
		"tcp_listeners",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("check script missing %q, got:\n%s", want, script)
		}
	}
	if strings.Contains(script, "id -u") {
		t.Errorf("check script must derive paths from the agent id, not the uid:\n%s", script)
	}
	checkOut := out.String()
	for _, want := range []string{"socket unit : active", "owner bunker-abc123, mode 600", "result      : OK"} {
		if !strings.Contains(checkOut, want) {
			t.Errorf("check output missing %q, got:\n%s", want, checkOut)
		}
	}
}

func TestSurfaceCheckCommand_FailureNamesChecks(t *testing.T) {
	checkScript = nil
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	agent := &v1.AgentSummary{AgentId: "abc123", SshfsMount: surfaceConnectTestMount}
	server := newTunnelTestServer(t, &surfaceCheckScriptMock{
		mockTunnelServer: mockTunnelServer{
			getAgentResp: &v1.GetAgentResponse{Agent: agent},
		},
		checkOut:   "CHECK	socket_active=inactive\nCHECK	socket_meta=absent\nCHECK	dir_meta=absent\nCHECK	toolsd_procs=0\nCHECK	tcp_listeners=0\n",
		scriptSink: func(script string) { checkScript = append(checkScript, script) },
	})
	t.Cleanup(server.Close)
	writeTunnelTestConfig(t, tmpDir, server.URL)

	var out bytes.Buffer
	cmd := NewSurfaceCommand()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"connect", "abc123", "--check"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("a degraded surface must fail --check")
	}
	checkOut := out.String()
	for _, want := range []string{"socket unit : inactive", "socket file is absent", "FAIL"} {
		if !strings.Contains(checkOut, want) {
			t.Errorf("check output missing %q, got:\n%s", want, checkOut)
		}
	}
	if !strings.Contains(err.Error(), "failed 3 check(s)") {
		t.Errorf("the refusal must count the failures, got: %v", err)
	}
}

// TestSurfaceCheckCommand_TCPUnknownFails: a missing ss(8) means the no-TCP
// property is UNPROVEN — unknown must fail the check, never pass it.
func TestSurfaceCheckCommand_TCPUnknownFails(t *testing.T) {
	checkScript = nil
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	agent := &v1.AgentSummary{AgentId: "abc123", SshfsMount: surfaceConnectTestMount}
	server := newTunnelTestServer(t, &surfaceCheckScriptMock{
		mockTunnelServer: mockTunnelServer{
			getAgentResp: &v1.GetAgentResponse{Agent: agent},
		},
		checkOut:   "CHECK	socket_active=active\nCHECK	socket_meta=bunker-abc123:600\nCHECK	dir_meta=bunker-abc123:700\nCHECK	toolsd_procs=0\nCHECK	tcp_listeners=unknown\n",
		scriptSink: nil,
	})
	t.Cleanup(server.Close)
	writeTunnelTestConfig(t, tmpDir, server.URL)

	var out bytes.Buffer
	cmd := NewSurfaceCommand()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"connect", "abc123", "--check"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("tcp=unknown must fail --check (unproven, not safe)")
	}
	if !strings.Contains(out.String(), "FAIL") || !strings.Contains(out.String(), "tcp_listeners") {
		t.Errorf("the FAIL line must carry the unproven tcp_listeners verdict, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "UNPROVEN") {
		t.Errorf("the failure must name the property as UNPROVEN, got:\n%s", out.String())
	}
}

// checkScript collects the scripts the --check mock received (the mock's
// scriptSink writes here).
var checkScript []string

// surfaceCheckScriptMock serves a canned CHECK output for --check flow tests
// and records the scripts it was asked to run.
type surfaceCheckScriptMock struct {
	mockTunnelServer
	checkOut   string
	scriptSink func(string)
}

func (m *surfaceCheckScriptMock) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest],
	stream *connect.ServerStream[v1.ExecAgentResponse]) error {
	args := req.Msg.GetArgs()
	if n := len(args); n > 0 && m.scriptSink != nil {
		m.scriptSink(args[n-1])
	}
	return stream.Send(&v1.ExecAgentResponse{
		Output:   &v1.ExecAgentResponse_Stdout{Stdout: []byte(m.checkOut)},
		ExitCode: 0,
	})
}

func TestSurfaceCheckCommand_JSON(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	agent := &v1.AgentSummary{AgentId: "abc123", SshfsMount: surfaceConnectTestMount}
	server := newTunnelTestServer(t, &surfaceCheckScriptMock{
		mockTunnelServer: mockTunnelServer{
			getAgentResp: &v1.GetAgentResponse{Agent: agent},
		},
		checkOut: "CHECK	socket_active=active\nCHECK	socket_meta=bunker-abc123:600\nCHECK	dir_meta=bunker-abc123:700\nCHECK	toolsd_procs=0\nCHECK	tcp_listeners=0\n",
	})
	t.Cleanup(server.Close)
	writeTunnelTestConfig(t, tmpDir, server.URL)

	var out bytes.Buffer
	cmd := NewSurfaceCommand()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"connect", "abc123", "--check", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{`"ok":true`, `"socket_active":"active"`, `"socket_mode":"600"`, `"toolsd_procs":"0"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("JSON output missing %q, got:\n%s", want, out.String())
		}
	}
}

// ── the reaping contract, through the REAL binary (GAP-079 shape) ───────────

// TestSurfaceConnectCommand_SignalReapsSSHChild builds the real CLI, points it
// at the in-process mock bunkerd, puts a fake `ssh` first on PATH that records
// its pid, publishes the expected local socket (what a real forward does),
// spawns a long-lived grandchild and waits for it, then SIGTERMs the CLI and
// asserts the whole subtree is gone.
func TestSurfaceConnectCommand_SignalReapsSSHChild(t *testing.T) {
	// POSIX process-group semantics; this is a //go:build unix file.
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 needed by the fake forwarder")
	}
	tmp := t.TempDir()

	agent := &v1.AgentSummary{AgentId: "e2e-main", SshfsMount: strings.ReplaceAll(surfaceConnectTestMount, "abc123", "e2e-main")}
	server := newTunnelTestServer(t, &surfaceConnectMock{
		mockTunnelServer: mockTunnelServer{
			getAgentResp: &v1.GetAgentResponse{Agent: agent},
		},
		uid: "4242",
	})
	t.Cleanup(server.Close)

	home := filepath.Join(tmp, "bunker-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("BUNKER_HOME", home)
	writeTunnelTestConfig(t, home, server.URL)
	writeTunnelKey(t, home, "e2e-main")

	// Fake ssh: records its pid, publishes a unix socket at the path the CLI
	// asked for (FAKE_SSH_SOCKET_PATH — what ssh -L really does), spawns a
	// long-lived grandchild and waits, so the ssh child stays alive.
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	localSock := filepath.Join(tmp, "fwd.sock")
	sshPIDFile := filepath.Join(tmp, "ssh.pid")
	grandchildPIDFile := filepath.Join(tmp, "ssh-grandchild.pid")
	sshScript := `#!/bin/sh
echo "$$" > "$FAKE_SSH_PID_FILE"
python3 -c 'import socket,os,sys,time; p=sys.argv[1]
try: os.unlink(p)
except OSError: pass
s=socket.socket(socket.AF_UNIX, socket.SOCK_STREAM); s.bind(p); s.listen(1); time.sleep(300)' "$FAKE_SSH_SOCKET_PATH" &
echo "$!" > "$FAKE_SSH_GRANDCHILD_PID_FILE"
wait
`
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte(sshScript), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}

	cliBin := buildCLIOnce(t)

	cliLog := filepath.Join(tmp, "cli.log")
	logFile, err := os.Create(cliLog)
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	cli := exec.Command(cliBin, "surface", "connect", "e2e-main", "--socket-path", localSock)
	cli.Dir = tmp
	cli.Env = append(os.Environ(),
		"BUNKER_HOME="+home,
		"HOME="+tmp,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_SSH_PID_FILE="+sshPIDFile,
		"FAKE_SSH_GRANDCHILD_PID_FILE="+grandchildPIDFile,
		"FAKE_SSH_SOCKET_PATH="+localSock,
	)
	// Plain files, not pipes (the tunnel test's rule): the forward inherits
	// the CLI's stdout/stderr and a pipe would block Wait on the copier.
	cli.Stdout, cli.Stderr = logFile, logFile

	if err := cli.Start(); err != nil {
		t.Fatalf("start CLI: %v", err)
	}

	sshPID, grandchildPID := 0, 0
	t.Cleanup(func() {
		if cli.Process != nil {
			_ = cli.Process.Kill()
		}
		for _, pid := range []int{sshPID, grandchildPID} {
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		_ = os.Remove(localSock)
	})

	sshPID = waitForPIDFile(t, sshPIDFile, 15*time.Second, cliLog)
	grandchildPID = waitForPIDFile(t, grandchildPIDFile, 15*time.Second, cliLog)

	// Premise: the fake forwarder really is running and REALLY published the
	// local socket (the READY line's evidence) — otherwise the signal below
	// would be tearing down a CLI that already failed.
	for name, pid := range map[string]int{"fake ssh": sshPID, "fake ssh grandchild": grandchildPID} {
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("%s (pid %d) is not running before the signal: %v", name, pid, err)
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if fi, err := os.Stat(localSock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake forward never published %s (CLI log:\n%s)", localSock, readFileOr(cliLog))
		}
		time.Sleep(50 * time.Millisecond)
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
	gone := time.Now().Add(5 * time.Second)
	for _, child := range []struct {
		name string
		pid  int
	}{
		{"the ssh forward child", sshPID},
		{"the forward grandchild", grandchildPID},
	} {
		for {
			if err := syscall.Kill(child.pid, 0); err != nil {
				break // ESRCH: gone
			}
			if time.Now().After(gone) {
				t.Errorf("GAP-079: %s (pid %d) survived a SIGTERM to the CLI — the forward subtree was orphaned", child.name, child.pid)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// waitForPIDFile / readFileOr come from tunnel_test.go (shared package test
// helpers).
