package server

// GAP-072 acceptance battery for interactive attach.
//
// Every test here drives the REAL connect stack: a real BunkerdHandler mounted
// on an httptest server, a real bidirectional stream, and a real `ssh` child
// (a stub shell on PATH that plays the remote side). That is deliberate - the
// acceptance criteria are about the live path (bidirectional streaming,
// terminal resize, idle close, audit records, session parity with exec), so a
// unit test on a helper would not have proved any of them.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	"github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"

	"github.com/deployBunker/bunker/internal/audit"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

const (
	attachTestAgentID        = "attach1"
	attachTestStoppedAgentID = "attach2"
	attachTestUnknownAgentID = "attach404"
)

// attachStubSSH installs a stub `ssh` on PATH for the test. body is appended
// to the stub script after the argv recorder, so every invocation writes the
// arguments it was handed to argvDir (one file per pid) and then runs body.
//
// The stub is how the tests play the remote side without a host: the server
// still builds and executes the REAL ssh command line, so the invariants under
// test (the -tt/-T posture, the exec-shaped remote command, the pipes and PTY)
// are the production ones.
func attachStubSSH(t *testing.T, body string) (argvDir string) {
	t.Helper()
	binDir := t.TempDir()
	argvDir = t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argvDir + "/argv.$$\n" +
		body + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub ssh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvDir
}

// attachStubArgv returns the argv of the most recent stub invocation.
func attachStubArgv(t *testing.T, argvDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(argvDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no stub ssh invocation recorded in %s (err=%v)", argvDir, err)
	}
	// The stub names each file by the invoking pid; pick the newest.
	newest := entries[0]
	for _, e := range entries[1:] {
		if e.Name() > newest.Name() {
			newest = e
		}
	}
	raw, err := os.ReadFile(filepath.Join(argvDir, newest.Name()))
	if err != nil {
		t.Fatalf("read stub argv: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// attachTestService builds the production service shape: a tracker holding one
// running agent and one stopped agent, no audit log unless the caller passes
// one.
func attachTestService(t *testing.T, log *audit.AuditLog) *bunkerdService {
	t.Helper()
	logger := testDiscardLogger()
	tracker := resource.NewTracker(10, logger)
	for _, rec := range []*resource.AgentRecord{
		{AgentID: attachTestAgentID, Status: "running", SshPrivateKeyPath: imgTestKeyPath},
		{AgentID: attachTestStoppedAgentID, Status: "stopped", SshPrivateKeyPath: imgTestKeyPath},
	} {
		if err := tracker.Register(rec); err != nil {
			t.Fatalf("register %s: %v", rec.AgentID, err)
		}
	}
	return &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker, auditLog: log}
}

// attachTestClient mounts the handler the way server.go does (real connect
// router) and returns a client pointed at it.
//
// AttachAgent is BIDIRECTIONAL, and connect serves bidirectional streams over
// HTTP/2 only (an HTTP/1.x bidi request is answered 505). The test server must
// therefore speak h2c - cleartext prior-knowledge HTTP/2, exactly the shape a
// daemon with server.h2c_enabled=true serves - and the client must dial with
// the same prior-knowledge protocol set the CLI's attach transport uses. A
// test over plain HTTP/1.1 would prove nothing: it cannot carry this RPC.
func attachTestClient(t *testing.T, svc *bunkerdService) bunkerv1connect.BunkerdClient {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := bunkerv1connect.NewBunkerdHandler(svc)
	mux.Handle(path, handler)

	srv := httptest.NewUnstartedServer(mux)
	srvProtocols := new(http.Protocols)
	srvProtocols.SetHTTP1(true)
	srvProtocols.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = srvProtocols
	srv.Start()
	t.Cleanup(srv.Close)

	// Client side: UnencryptedHTTP2 WITHOUT HTTP1, which is the only
	// configuration net/http uses h2c for on a cleartext http:// URL.
	clientProtocols := new(http.Protocols)
	clientProtocols.SetUnencryptedHTTP2(true)
	client := &http.Client{Transport: &http.Transport{Protocols: clientProtocols}}
	return bunkerv1connect.NewBunkerdClient(client, srv.URL)
}

// attachStartFrame builds the mandatory first frame.
func attachStartFrame(agentID string, mut ...func(*v1.AttachStart)) *v1.AttachAgentRequest {
	start := &v1.AttachStart{AgentId: agentID}
	for _, f := range mut {
		f(start)
	}
	return &v1.AttachAgentRequest{Payload: &v1.AttachAgentRequest_Start{Start: start}}
}

func attachStdin(data string) *v1.AttachAgentRequest {
	return &v1.AttachAgentRequest{Payload: &v1.AttachAgentRequest_Stdin{Stdin: []byte(data)}}
}

func attachStdinEOF() *v1.AttachAgentRequest {
	return &v1.AttachAgentRequest{Payload: &v1.AttachAgentRequest_StdinEof{StdinEof: true}}
}

func attachResize(cols, rows uint32) *v1.AttachAgentRequest {
	return &v1.AttachAgentRequest{Payload: &v1.AttachAgentRequest_Resize{Resize: &v1.AttachResize{Cols: cols, Rows: rows}}}
}

// attachResult is what a drained session produced.
type attachResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int32
	reason   string
	gotExit  bool
}

// attachDrain reads frames until the exit frame (or the stream ends) and
// returns everything it saw. It bounds the whole read with a timeout so a
// broken protocol fails the test instead of hanging it.
func attachDrain(t *testing.T, stream *connect.BidiStreamForClient[v1.AttachAgentRequest, v1.AttachAgentResponse]) attachResult {
	t.Helper()
	type outcome struct {
		res attachResult
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		var res attachResult
		for {
			msg, err := stream.Receive()
			if err != nil {
				ch <- outcome{res: res, err: err}
				return
			}
			switch p := msg.GetPayload().(type) {
			case *v1.AttachAgentResponse_Stdout:
				res.stdout = append(res.stdout, p.Stdout...)
			case *v1.AttachAgentResponse_Stderr:
				res.stderr = append(res.stderr, p.Stderr...)
			case *v1.AttachAgentResponse_Exit:
				res.exitCode = p.Exit.GetExitCode()
				res.reason = p.Exit.GetReason()
				res.gotExit = true
				ch <- outcome{res: res}
				return
			}
		}
	}()
	select {
	case o := <-ch:
		if o.err != nil && !errors.Is(o.err, io.EOF) {
			t.Fatalf("attach stream: %v", o.err)
		}
		return o.res
	case <-time.After(30 * time.Second):
		t.Fatal("attach stream produced no exit frame within 30s")
		return attachResult{}
	}
}

// TestAttachAgent_StdinStdoutRoundTrip is acceptance criterion 2's core: bytes
// the client writes reach the child's stdin, the child's bytes come back, a
// stdin half-close ends the child cleanly and its exit status is reported.
func TestAttachAgent_StdinStdoutRoundTrip(t *testing.T) {
	argvDir := attachStubSSH(t, "cat")
	svc := attachTestService(t, nil)
	client := attachTestClient(t, svc)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream := client.AttachAgent(ctx)
	if err := stream.Send(attachStartFrame(attachTestAgentID, func(s *v1.AttachStart) {
		s.Tty = false
		s.Command = "cat"
	})); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.Send(attachStdin("hello attach\n")); err != nil {
		t.Fatalf("send stdin: %v", err)
	}
	if err := stream.Send(attachStdinEOF()); err != nil {
		t.Fatalf("send stdin_eof: %v", err)
	}
	res := attachDrain(t, stream)

	if got := string(res.stdout); got != "hello attach\n" {
		t.Errorf("stdout = %q, want the exact bytes written to stdin", got)
	}
	if !res.gotExit {
		t.Fatal("no exit frame")
	}
	if res.exitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.exitCode)
	}
	if res.reason != audit.AttachReasonExited {
		t.Errorf("reason = %q, want %q", res.reason, audit.AttachReasonExited)
	}

	argv := attachStubArgv(t, argvDir)
	joined := strings.Join(argv, " ")
	if !containsExact(argv, "-T") {
		t.Errorf("non-tty attach argv has no -T: %q", argv)
	}
	if containsExact(argv, "-tt") {
		t.Errorf("non-tty attach argv requested a remote PTY: %q", argv)
	}
	if !strings.Contains(joined, "bunker-"+attachTestAgentID+"@localhost") {
		t.Errorf("argv does not target the agent user: %q", argv)
	}
}

// TestAttachAgent_TerminalSessionAndResize is acceptance criterion 1's core:
// with tty=true the child sees a real pseudo-terminal, and a resize frame
// changes its window size (which is what makes vi/less redraw).
func TestAttachAgent_TerminalSessionAndResize(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("PTY allocation is implemented on linux")
	}
	// The stub reads one line, then reports the terminal size it sees. The
	// read completes only after the test has sent its resize, so the reported
	// size is deterministic: resize -> stdin -> stty size.
	argvDir := attachStubSSH(t, "read x\nstty size\necho got:$x\n")
	svc := attachTestService(t, nil)
	client := attachTestClient(t, svc)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream := client.AttachAgent(ctx)
	if err := stream.Send(attachStartFrame(attachTestAgentID, func(s *v1.AttachStart) {
		s.Tty = true
		s.Cols = 100
		s.Rows = 40
	})); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.Send(attachResize(120, 50)); err != nil {
		t.Fatalf("send resize: %v", err)
	}
	if err := stream.Send(attachStdin("hi\n")); err != nil {
		t.Fatalf("send stdin: %v", err)
	}
	res := attachDrain(t, stream)

	out := string(res.stdout)
	if !strings.Contains(out, "got:hi") {
		t.Errorf("stdout %q does not carry the echoed line (the child did not read stdin)", out)
	}
	if !strings.Contains(out, "50 120") {
		t.Errorf("stdout %q does not report the RESIZED terminal (rows cols 50 120); resize was not applied", out)
	}
	if res.exitCode != 0 || res.reason != audit.AttachReasonExited {
		t.Errorf("exit=(%d,%q), want (0,%q)", res.exitCode, res.reason, audit.AttachReasonExited)
	}

	argv := attachStubArgv(t, argvDir)
	if !containsExact(argv, "-tt") {
		t.Errorf("tty attach argv does not force a remote PTY (-tt): %q", argv)
	}
	// A PTY session passes NO remote command: sshd then starts the agent
	// user's login shell, which is the interactive REPL `bunker attach`
	// promises. The last argv element must therefore be the target, not a
	// command string.
	if last := argv[len(argv)-1]; last != "bunker-"+attachTestAgentID+"@localhost" {
		t.Errorf("tty attach passed a remote command (%q); a command-less attach must let sshd start the login shell", last)
	}
}

// TestAttachAgent_CommandRunsTheSameSessionAsExec pins the "attach is not a
// bypass" property: with a command, the remote command is built by the very
// same exec builder, so the session carries the agent's PATH, Docker socket
// and private TMPDIR. If attach ever grew its own command assembly, this test
// would be the one to fail.
func TestAttachAgent_CommandRunsTheSameSessionAsExec(t *testing.T) {
	argvDir := attachStubSSH(t, "exit 0")
	svc := attachTestService(t, nil)
	client := attachTestClient(t, svc)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream := client.AttachAgent(ctx)
	if err := stream.Send(attachStartFrame(attachTestAgentID, func(s *v1.AttachStart) {
		s.Tty = false
		s.Command = "uname"
		s.Args = []string{"-a"}
	})); err != nil {
		t.Fatalf("send start: %v", err)
	}
	res := attachDrain(t, stream)
	if res.exitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.exitCode)
	}

	argv := attachStubArgv(t, argvDir)
	remote := argv[len(argv)-1]
	for _, want := range []string{
		"DOCKER_HOST=unix:///run/bunker/" + attachTestAgentID + "/docker.sock",
		"TMPDIR=",
		"uname",
		"-a",
	} {
		if !strings.Contains(remote, want) {
			t.Errorf("attach remote command does not carry %q:\n%s", want, remote)
		}
	}
}

// TestAttachAgent_DefaultLoginShellAndExitCode covers the no-command case and
// the exit-code propagation path: the session is a bare login shell and a
// non-zero remote status survives to the client.
func TestAttachAgent_DefaultLoginShellAndExitCode(t *testing.T) {
	argvDir := attachStubSSH(t, "exit 7")
	svc := attachTestService(t, nil)
	client := attachTestClient(t, svc)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream := client.AttachAgent(ctx)
	if err := stream.Send(attachStartFrame(attachTestAgentID, func(s *v1.AttachStart) {
		s.Tty = false
	})); err != nil {
		t.Fatalf("send start: %v", err)
	}
	res := attachDrain(t, stream)
	if res.exitCode != 7 {
		t.Errorf("exit code = %d, want the child's 7", res.exitCode)
	}

	argv := attachStubArgv(t, argvDir)
	if last := argv[len(argv)-1]; last != "bunker-"+attachTestAgentID+"@localhost" {
		t.Errorf("command-less attach passed a remote command %q; the login shell must be sshd's own choice", last)
	}
}

// TestAttachAgent_StoppedAgentIsAgentStopped is acceptance criterion 3.
func TestAttachAgent_StoppedAgentIsAgentStopped(t *testing.T) {
	attachStubSSH(t, "exit 0")
	svc := attachTestService(t, nil)
	client := attachTestClient(t, svc)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream := client.AttachAgent(ctx)
	if err := stream.Send(attachStartFrame(attachTestStoppedAgentID)); err != nil {
		// The server may abort before the client's first Send completes; the
		// error must still be the stopped sentinel, so check it below via the
		// stream's own error.
		if !strings.Contains(err.Error(), "agent_stopped") {
			t.Fatalf("send start to a stopped agent: %v (want the agent_stopped sentinel)", err)
		}
	}
	var err error
	for {
		if _, rerr := stream.Receive(); rerr != nil {
			err = rerr
			break
		}
	}
	if err == nil {
		t.Fatal("attach to a stopped agent succeeded; want a refusal")
	}
	if code := connect.CodeOf(err); code != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", code)
	}
	if !strings.Contains(err.Error(), "agent_stopped") {
		t.Errorf("error %q does not carry the agent_stopped sentinel", err)
	}
}

// TestAttachAgent_UnknownAgentIsNotFound: a missing agent must stay
// distinguishable from a stopped one.
func TestAttachAgent_UnknownAgentIsNotFound(t *testing.T) {
	attachStubSSH(t, "exit 0")
	svc := attachTestService(t, nil)
	client := attachTestClient(t, svc)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream := client.AttachAgent(ctx)
	_ = stream.Send(attachStartFrame(attachTestUnknownAgentID))
	var err error
	for {
		if _, rerr := stream.Receive(); rerr != nil {
			err = rerr
			break
		}
	}
	if err == nil {
		t.Fatal("attach to an unknown agent succeeded")
	}
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", code)
	}
}

// TestAttachAgent_FirstFrameMustBeStart pins the protocol: a stream that opens
// with anything but a start frame is refused rather than run as a session.
func TestAttachAgent_FirstFrameMustBeStart(t *testing.T) {
	attachStubSSH(t, "exit 0")
	svc := attachTestService(t, nil)
	client := attachTestClient(t, svc)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream := client.AttachAgent(ctx)
	_ = stream.Send(attachStdin("not a start frame"))
	var err error
	for {
		if _, rerr := stream.Receive(); rerr != nil {
			err = rerr
			break
		}
	}
	if err == nil {
		t.Fatal("attach accepted a stdin-first stream")
	}
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", code)
	}
}

// TestAttachAgent_IdleTimeoutClosesTheSession is the anti-leak guard: a session
// with no input and no output is closed on its idle bound, the child is
// killed, and the client is told why.
func TestAttachAgent_IdleTimeoutClosesTheSession(t *testing.T) {
	pidDir := t.TempDir()
	argvDir := attachStubSSH(t, "echo $$ > "+pidDir+"/pid\nsleep 300")
	_ = argvDir
	svc := attachTestService(t, nil)
	client := attachTestClient(t, svc)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream := client.AttachAgent(ctx)
	if err := stream.Send(attachStartFrame(attachTestAgentID, func(s *v1.AttachStart) {
		s.Tty = false
		s.IdleTimeoutSeconds = 1
	})); err != nil {
		t.Fatalf("send start: %v", err)
	}

	start := time.Now()
	res := attachDrain(t, stream)
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("idle close took %s; the 1s idle bound did not fire", elapsed)
	}
	if !res.gotExit {
		t.Fatal("no exit frame after the idle timeout")
	}
	if res.reason != audit.AttachReasonIdleTimeout {
		t.Errorf("reason = %q, want %q", res.reason, audit.AttachReasonIdleTimeout)
	}
	if res.exitCode != attachExitIdleTimeout {
		t.Errorf("exit code = %d, want %d", res.exitCode, attachExitIdleTimeout)
	}

	// The child must be gone: an idle close that leaves the ssh child running
	// has fixed nothing.
	raw, err := os.ReadFile(filepath.Join(pidDir, "pid"))
	if err != nil {
		t.Fatalf("stub did not record its pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse stub pid: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			break // ESRCH: the child is gone
		}
		if time.Now().After(deadline) {
			t.Fatalf("ssh child pid %d is still alive 10s after the idle close", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestAttachAgent_AuditOpenAndCloseWithoutKeystrokes is acceptance criterion 4:
// the trail shows the session opening and closing, and it carries nothing the
// operator typed.
func TestAttachAgent_AuditOpenAndCloseWithoutKeystrokes(t *testing.T) {
	const secretInput = "ATTACH-TOPSECRET-KEYSTROKE-9f3a"
	attachStubSSH(t, "cat")
	log, path := gap142AuditLog(t)
	svc := attachTestService(t, log)
	client := attachTestClient(t, svc)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream := client.AttachAgent(ctx)
	if err := stream.Send(attachStartFrame(attachTestAgentID, func(s *v1.AttachStart) {
		s.Tty = false
		s.Command = "cat"
	})); err != nil {
		t.Fatalf("send start: %v", err)
	}
	if err := stream.Send(attachStdin(secretInput + "\n")); err != nil {
		t.Fatalf("send stdin: %v", err)
	}
	if err := stream.Send(attachStdinEOF()); err != nil {
		t.Fatalf("send stdin_eof: %v", err)
	}
	res := attachDrain(t, stream)
	if !strings.Contains(string(res.stdout), secretInput) {
		t.Fatalf("the session did not echo the input; the round trip is broken")
	}

	recs, err := audit.Query(path, audit.Filter{AgentID: attachTestAgentID})
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	var open, close recs2
	for _, r := range recs {
		switch {
		case strings.HasSuffix(r.Method, audit.AttachOpenMethod):
			open.found, open.rec = true, r
		case strings.HasSuffix(r.Method, audit.AttachCloseMethod):
			close.found, close.rec = true, r
		}
	}
	if !open.found {
		t.Fatalf("no attach-open audit record: %+v", recs)
	}
	if !close.found {
		t.Fatalf("no attach-close audit record: %+v", recs)
	}
	if !strings.Contains(open.rec.Summary, "attach open") || !strings.Contains(open.rec.Summary, "tty=false") {
		t.Errorf("open summary = %q, want it to state the phase and the tty posture", open.rec.Summary)
	}
	if !strings.Contains(close.rec.Summary, "attach close") ||
		!strings.Contains(close.rec.Summary, "reason="+audit.AttachReasonExited) {
		t.Errorf("close summary = %q, want the phase and the close reason", close.rec.Summary)
	}
	if close.rec.DurationMS < 0 {
		t.Errorf("close record duration_ms = %d, want a measured non-negative duration", close.rec.DurationMS)
	}

	// The keystroke guarantee: nothing the operator typed may be anywhere in
	// the trail bytes.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if strings.Contains(string(raw), secretInput) {
		t.Errorf("the audit trail carries terminal input; attach must never log keystrokes")
	}
}

// recs2 is a tiny found-flag holder for the audit assertions above.
type recs2 struct {
	found bool
	rec   audit.Record
}

// TestAttachAndExecConcurrentlyDoNotCorruptStreams is acceptance criterion 2 in
// the daemon's terms: one attach (a command-less session streaming a large
// numbered payload) and one exec run at the same time; each stream must carry
// exactly its own bytes, in order, with nothing from the other.
func TestAttachAndExecConcurrentlyDoNotCorruptStreams(t *testing.T) {
	const (
		attachMarker = "ATTACH-CONCURRENT-TAG"
		execMarker   = "EXEC-CONCURRENT-MARKER-zzz"
		lines        = 400
	)
	// The stub plays both sides: a command-less session (the last argv element
	// is the target host) streams a numbered payload; an exec (the last argv
	// element is the remote command) prints its command line.
	body := "sleep 0.15\n" +
		"last=\"\"\n" +
		"for a in \"$@\"; do last=\"$a\"; done\n" +
		"case \"$last\" in\n" +
		"  *@localhost)\n" +
		"    i=0\n" +
		"    while [ \"$i\" -lt " + strconv.Itoa(lines) + " ]; do\n" +
		"      printf '" + attachMarker + "-%04d\\n' \"$i\"\n" +
		"      i=$((i+1))\n" +
		"    done\n" +
		"    exit 0 ;;\n" +
		"  *)\n" +
		"    printf 'EXEC-WRAPPER:%s\\n' \"$last\"\n" +
		"    exit 0 ;;\n" +
		"esac"
	attachStubSSH(t, body)
	svc := attachTestService(t, nil)
	client := attachTestClient(t, svc)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	attachStream := client.AttachAgent(ctx)
	if err := attachStream.Send(attachStartFrame(attachTestAgentID, func(s *v1.AttachStart) {
		s.Tty = false
	})); err != nil {
		t.Fatalf("send start: %v", err)
	}

	execStream, err := client.ExecAgent(ctx, connect.NewRequest(&v1.ExecAgentRequest{
		AgentId: attachTestAgentID,
		Command: execMarker,
	}))
	if err != nil {
		t.Fatalf("ExecAgent: %v", err)
	}

	var execOut strings.Builder
	for execStream.Receive() {
		if msg := execStream.Msg(); msg.GetStdout() != nil {
			execOut.Write(msg.GetStdout())
		}
	}
	if err := execStream.Err(); err != nil {
		t.Fatalf("exec stream: %v", err)
	}
	res := attachDrain(t, attachStream)

	// The attach stream must carry the whole numbered payload, in order and
	// exactly once, and nothing from the exec.
	got := strings.Split(strings.TrimRight(string(res.stdout), "\n"), "\n")
	if len(got) != lines {
		t.Fatalf("attach stream carried %d lines, want %d (interleaving/corruption)", len(got), lines)
	}
	for i, line := range got {
		want := fmt.Sprintf("%s-%04d", attachMarker, i)
		if line != want {
			t.Fatalf("attach line %d = %q, want %q (stream corrupted)", i, line, want)
		}
	}
	if strings.Contains(string(res.stdout), "EXEC-WRAPPER") {
		t.Error("the exec stream's bytes leaked into the attach stream")
	}
	exec := execOut.String()
	if !strings.Contains(exec, "EXEC-WRAPPER") || !strings.Contains(exec, execMarker) {
		t.Errorf("exec stream = %q, want its own wrapper output", exec)
	}
	if strings.Contains(exec, attachMarker) {
		t.Error("the attach stream's bytes leaked into the exec stream")
	}
}
