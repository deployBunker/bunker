package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// attachMockServer is an in-process daemon for the CLI battery. It implements
// the interactive contract the real handler does: a start frame, echoed stdin,
// an optional terminal resize acknowledgement, then one exit frame.
type attachMockServer struct {
	mockBunkerdServer
	start       *v1.AttachStart
	resizes     []*v1.AttachResize
	exitCode    int32
	reason      string
	echoStdin   bool
	requestSeen bool
}

func (m *attachMockServer) AttachAgent(ctx context.Context, stream *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) error {
	m.requestSeen = true
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("first frame is not a start"))
	}
	m.start = start
	sendExit := func() error {
		reason := m.reason
		if reason == "" {
			reason = "exited"
		}
		return stream.Send(&v1.AttachAgentResponse{
			Payload: &v1.AttachAgentResponse_Exit{Exit: &v1.AttachExit{ExitCode: m.exitCode, Reason: reason}},
		})
	}
	for {
		msg, err := stream.Receive()
		if err != nil {
			return sendExit()
		}
		switch p := msg.GetPayload().(type) {
		case *v1.AttachAgentRequest_Stdin:
			if m.echoStdin {
				if err := stream.Send(&v1.AttachAgentResponse{
					Payload: &v1.AttachAgentResponse_Stdout{Stdout: p.Stdin},
				}); err != nil {
					return err
				}
			}
		case *v1.AttachAgentRequest_Resize:
			m.resizes = append(m.resizes, p.Resize)
		case *v1.AttachAgentRequest_StdinEof:
			return sendExit()
		}
	}
}

// newAttachTestServer starts an h2c-capable test daemon: AttachAgent is a
// bidirectional RPC, and connect serves those over HTTP/2 only.
func newAttachTestServer(t *testing.T, handler bunkerv1connect.BunkerdHandler) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	path, h := bunkerv1connect.NewBunkerdHandler(handler)
	mux.Handle(path, h)
	srv := httptest.NewUnstartedServer(mux)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = protocols
	srv.Start()
	return srv
}

// attachTestSetup wires a scratch HOME + config pointing at the mock daemon
// and returns the mock.
func attachTestSetup(t *testing.T, mock *attachMockServer) *attachMockServer {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(SessionTargetEnvVar, "default")
	srv := newAttachTestServer(t, mock)
	t.Cleanup(srv.Close)
	writeExecTestConfig(t, home, srv.URL)
	return mock
}

// TestAttachCommand_EchoesStdinAndExitsCleanly drives the whole CLI path: a
// piped stdin is streamed to the session and the session's output reaches
// stdout, with a clean exit.
func TestAttachCommand_EchoesStdinAndExitsCleanly(t *testing.T) {
	mock := attachTestSetup(t, &attachMockServer{echoStdin: true})

	cmd := NewAttachCommand()
	var out, errOut bytes.Buffer
	cmd.SetIn(strings.NewReader("hello attach\n"))
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", "default", "abc12345"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("attach: %v (stderr=%s)", err, errOut.String())
	}
	if got := out.String(); !strings.Contains(got, "hello attach") {
		t.Errorf("stdout = %q, want the echoed stdin", got)
	}
	if mock.start == nil {
		t.Fatal("the daemon never received a start frame")
	}
	if mock.start.GetAgentId() != "abc12345" {
		t.Errorf("start agent_id = %q, want abc12345", mock.start.GetAgentId())
	}
	// A piped stdin is not a terminal: no PTY may be requested for it.
	if mock.start.GetTty() {
		t.Error("start requested a tty for a non-terminal stdin")
	}
	if mock.start.GetCommand() != "" {
		t.Errorf("start command = %q, want empty (the login shell)", mock.start.GetCommand())
	}
}

// TestAttachCommand_CommandAndFlagsReachTheDaemon: --command and the trailing
// command form both arrive as the session's command, and --idle-timeout rides
// the start frame.
func TestAttachCommand_CommandAndFlagsReachTheDaemon(t *testing.T) {
	mock := attachTestSetup(t, &attachMockServer{echoStdin: true})

	cmd := NewAttachCommand()
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", "default", "--command", "cd /srv && bash", "--idle-timeout", "90", "abc12345"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got := mock.start.GetCommand(); got != "cd /srv && bash" {
		t.Errorf("start command = %q, want the --command value", got)
	}
	if got := mock.start.GetIdleTimeoutSeconds(); got != 90 {
		t.Errorf("start idle_timeout_seconds = %d, want 90", got)
	}

	// The positional form: everything after the agent id (here after --) is the
	// command and its args, and it must reach the daemon as command+args - not
	// be dropped, and not be re-parsed by any shell on the way.
	mock2 := attachTestSetup(t, &attachMockServer{echoStdin: true})
	cmd2 := NewAttachCommand()
	cmd2.SetIn(strings.NewReader(""))
	cmd2.SetOut(io.Discard)
	cmd2.SetErr(io.Discard)
	cmd2.SetArgs([]string{"--server", "default", "abc12345", "--", "tail", "-f", "/var/log/syslog"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("attach (positional): %v", err)
	}
	if got := mock2.start.GetAgentId(); got != "abc12345" {
		t.Errorf("positional form: agent_id = %q, want abc12345", got)
	}
	if got := mock2.start.GetCommand(); got != "tail" {
		t.Errorf("positional command = %q, want tail", got)
	}
	if got := mock2.start.GetArgs(); len(got) != 2 || got[0] != "-f" || got[1] != "/var/log/syslog" {
		t.Errorf("positional args = %q, want [-f /var/log/syslog]", got)
	}
}

// TestAttachCommand_RemoteExitCodePropagates: the session's status becomes the
// CLI's exit status, ssh-style, and no error text is printed for it.
func TestAttachCommand_RemoteExitCodePropagates(t *testing.T) {
	attachTestSetup(t, &attachMockServer{echoStdin: true, exitCode: 3})

	cmd := NewAttachCommand()
	var errOut bytes.Buffer
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", "default", "abc12345"})

	err := cmd.Execute()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error = %v, want an *ExitError carrying the remote status", err)
	}
	if exitErr.Code != 3 {
		t.Errorf("exit code = %d, want 3", exitErr.Code)
	}
}

// TestAttachCommand_IdleTimeoutIsReported: an idle close is a distinct,
// non-silent outcome.
func TestAttachCommand_IdleTimeoutIsReported(t *testing.T) {
	attachTestSetup(t, &attachMockServer{reason: "idle_timeout", exitCode: 124})

	cmd := NewAttachCommand()
	var errOut bytes.Buffer
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", "default", "--idle-timeout", "60", "abc12345"})

	err := cmd.Execute()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 124 {
		t.Fatalf("error = %v, want an ExitError(124)", err)
	}
	if !strings.Contains(errOut.String(), "no input or output") {
		t.Errorf("stderr = %q, want the idle-close explanation", errOut.String())
	}
}

// TestAttachCommand_RejectsCommandAndPositional: the two ways to name a command
// are mutually exclusive, and the refusal happens before anything is dialled.
func TestAttachCommand_RejectsCommandAndPositional(t *testing.T) {
	mock := attachTestSetup(t, &attachMockServer{})

	cmd := NewAttachCommand()
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", "default", "--command", "true", "abc12345", "ls"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("attach accepted --command together with a positional command")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v, want the mutual-exclusion refusal", err)
	}
	if mock.requestSeen {
		t.Error("the CLI dialled the daemon before refusing; validation must come first")
	}
}

// TestAttachCommand_NoTargetBound: attach is a session command and must never
// fall back to the shared active server.
func TestAttachCommand_NoTargetBound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(SessionTargetEnvVar, "")

	cmd := NewAttachCommand()
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"abc12345"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("attach succeeded with no bound target")
	}
	if !strings.Contains(err.Error(), "no target bound") {
		t.Errorf("error = %v, want the 'no target bound' refusal", err)
	}
}

// TestAttachCommand_HelpAndFlags pins the documented surface.
func TestAttachCommand_HelpAndFlags(t *testing.T) {
	cmd := NewAttachCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("help: %v", err)
	}
	help := buf.String()
	for _, want := range []string{"attach", "--command", "--no-tty", "--idle-timeout", "--server"} {
		if !strings.Contains(help, want) {
			t.Errorf("help output does not mention %q:\n%s", want, help)
		}
	}
}

// TestAttachCommand_RefusesHTTP1DaemonWithAHint is the transport prerequisite
// made visible: a daemon that answers over HTTP/1.1 cannot carry a
// bidirectional stream, and the operator is told what to change instead of
// being handed a bare 505/protocol error.
func TestAttachCommand_RefusesHTTP1DaemonWithAHint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(SessionTargetEnvVar, "default")

	// A plain HTTP/1.1 server: exactly what a daemon with h2c disabled and no
	// TLS serves.
	mux := http.NewServeMux()
	path, h := bunkerv1connect.NewBunkerdHandler(&attachMockServer{echoStdin: true})
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	writeExecTestConfig(t, home, srv.URL)

	cmd := NewAttachCommand()
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", "default", "abc12345"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("attach succeeded against an HTTP/1.1-only daemon")
	}
	if !strings.Contains(err.Error(), "HTTP/2") && !strings.Contains(err.Error(), "h2c") {
		t.Errorf("error = %v, want an actionable HTTP/2 hint", err)
	}
}
