package cli

// PERF-020: `bunker cp` streams the file through the daemon's ExecAgent by
// default instead of spawning an scp child. These tests pin the decision
// (flag/size routing), the remote script's write/chmod/chown contract, the
// scp-shaped failure vocabulary, and the end-to-end wiring against a stub
// daemon.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// TestCpShouldStreamViaDaemon pins the routing: any explicitly-set SSH
// identity flag keeps scp (the daemon cannot honor an override), and the file
// must fit in one request message.
func TestCpShouldStreamViaDaemon(t *testing.T) {
	tests := []struct {
		name    string
		sshHost string
		sshPort uint32
		sshKey  string
		size    int64
		want    bool
	}{
		{name: "defaults over a small file stream", size: 60, want: true},
		{name: "empty file streams too (scp moves it as well)", size: 0, want: false},
		{name: "explicit host keeps scp", sshHost: "10.0.0.9", size: 60},
		{name: "explicit port keeps scp", sshPort: 2222, size: 60},
		{name: "explicit key keeps scp", sshKey: "/tmp/key", size: 60},
		{name: "file past the cap keeps scp", size: maxInprocFileBytes + 1},
		{name: "file exactly at the cap streams", size: maxInprocFileBytes, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cpShouldStreamViaDaemon(tt.sshHost, tt.sshPort, tt.sshKey, tt.size); got != tt.want {
				t.Errorf("cpShouldStreamViaDaemon(host=%q port=%d key=%q size=%d) = %v, want %v",
					tt.sshHost, tt.sshPort, tt.sshKey, tt.size, got, tt.want)
			}
		})
	}
}

// TestCpRemoteWriteScript pins the remote script: quoted destination, the
// source mode (scp's C-mode contract), the uid/gid chown, and the exit-code
// contract the client maps.
func TestCpRemoteWriteScript(t *testing.T) {
	script := cpRemoteWriteScript("/nonexistent 'dir'/x", 0o600)
	for _, want := range []string{
		`cat > '/nonexistent '\''dir'\''/x'`, // quotePOSIX escaping
		"|| exit 2",
		"chmod 0600",
		`chown "$(id -u):$(id -g)"`,
		"|| exit 3",
		"|| exit 4",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script %q missing %q", script, want)
		}
	}
}

// TestErrnoPhrase pins the scp-vocabulary extractor: the text after the
// remote shell's final ": " is what scp's "dest open" line quoted.
func TestErrnoPhrase(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"bash: line 1: /nonexistent-dir-020/x: No such file or directory", "No such file or directory"},
		{"sh: 1: /x: Permission denied", "Permission denied"},
		{"", ""},
		{"\n  \n", ""},
		{"no separator", ""},
		{"trailing colon: ", ""},
	}
	for _, tt := range tests {
		if got := errnoPhrase(tt.in); got != tt.want {
			t.Errorf("errnoPhrase(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestScpErrnoVocabulary pins the dialect normalization measured on
// bunker-las-03: the daemon-side shell's ENOENT ("Directory nonexistent")
// renders in scp's words; unknown phrases stay verbatim.
func TestScpErrnoVocabulary(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Directory nonexistent", "No such file or directory"},
		{"No such file or directory", "No such file or directory"},
		{"Permission denied", "Permission denied"},
		{"Access denied", "Permission denied"},
		{"Disk quota exceeded", "Disk quota exceeded"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := scpErrnoVocabulary(tt.in); got != tt.want {
			t.Errorf("scpErrnoVocabulary(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// cpExecStreamMock is a cpMockServer whose ExecAgent streams a canned
// exchange; it exists so the cp wiring tests can drive the fast path without
// a real daemon.
type cpExecStreamMock struct {
	*cpMockServer
	execAgent func(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error
}

func (m *cpExecStreamMock) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
	return m.execAgent(ctx, req, stream)
}

func (m *cpExecStreamMock) RenewalDriftReport(context.Context, *connect.Request[v1.RenewalDriftRequest]) (*connect.Response[v1.RenewalDriftResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpExecStreamMock) GetSpawnProgress(context.Context, *connect.Request[v1.GetSpawnProgressRequest]) (*connect.Response[v1.GetSpawnProgressResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpExecStreamMock) GetAgentKey(context.Context, *connect.Request[v1.GetAgentKeyRequest]) (*connect.Response[v1.GetAgentKeyResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpExecStreamMock) RotateJWTSecret(context.Context, *connect.Request[v1.RotateJWTSecretRequest]) (*connect.Response[v1.RotateJWTSecretResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpExecStreamMock) RevokeKey(context.Context, *connect.Request[v1.RevokeKeyRequest]) (*connect.Response[v1.RevokeKeyResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpExecStreamMock) KeyList(context.Context, *connect.Request[v1.KeyListRequest]) (*connect.Response[v1.KeyListResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpExecStreamMock) OperatorCreate(context.Context, *connect.Request[v1.OperatorCreateRequest]) (*connect.Response[v1.OperatorCreateResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpExecStreamMock) OperatorList(context.Context, *connect.Request[v1.OperatorListRequest]) (*connect.Response[v1.OperatorListResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpExecStreamMock) OperatorDelete(context.Context, *connect.Request[v1.OperatorDeleteRequest]) (*connect.Response[v1.OperatorDeleteResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

// stubProbeSeams replaces the failure-probe children (real ssh binaries)
// with seams, keeping these tests off the network.
func stubProbeSeams(t *testing.T) {
	t.Helper()
	t.Setenv("BUNKER_HOME", t.TempDir()) // sshMultiplexArgs under an isolated HOME
	oldCount := scpFileCountFn
	scpFileCountFn = func(string, uint32, string, string) (int, error) { return 0, nil }
	t.Cleanup(func() { scpFileCountFn = oldCount })
	oldProbe := cpOwnershipProbeFn
	cpOwnershipProbeFn = func(string, uint32, string, string) (string, error) { return "", nil }
	t.Cleanup(func() { cpOwnershipProbeFn = oldProbe })
}

// setupCpDaemonStreamTest builds a HOME with a config pointing at a stub
// daemon that resolves the agent through the standard sshfs mount, plus the
// agent's key file, and returns a small local file to copy.
func setupCpDaemonStreamTest(t *testing.T, handler bunkerv1connect.BunkerdHandler) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(SessionTargetEnvVar, "custom-server")

	localFile := filepath.Join(tmpDir, "tiny.txt")
	if err := os.WriteFile(localFile, []byte("payload-60-bytes-0000000000000000000"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	srv := newTestServer(t, handler)
	t.Cleanup(srv.Close)

	cfg := &CLIConfig{
		ActiveServer: "custom-server",
		Servers: map[string]ServerEntry{
			"custom-server": {URL: srv.URL, Token: "test-cli-token"},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}
	// The failure path needs the agent key to exist (the key stat is a
	// pre-transfer preflight both transports share).
	if err := defaultSSHKeyEnsure(t, "test-agent"); err != nil {
		t.Fatalf("ensure agent key: %v", err)
	}
	return localFile
}

// defaultSSHKeyEnsure writes the placeholder key file the cp preflight stats.
func defaultSSHKeyEnsure(t *testing.T, agentID string) error {
	t.Helper()
	keyPath, err := defaultSSHKeyPath(agentID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(keyPath, []byte("test-key"), 0o600)
}

// TestCpDaemonStreamSuccess drives a full default `bunker cp` through a stub
// daemon whose ExecAgent accepts the transfer and exits 0: no scp child,
// stdout reports the copy, and the remote request carried the stdin payload,
// the write script and the mode.
func TestCpDaemonStreamSuccess(t *testing.T) {
	stubProbeSeams(t)
	var gotRequest *v1.ExecAgentRequest
	mock := &cpExecStreamMock{
		cpMockServer: &cpMockServer{
			agent: &v1.AgentSummary{
				AgentId:    "test-agent",
				Status:     "running",
				SshfsMount: cpTestSSHFSMount,
			},
		},
		execAgent: func(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
			gotRequest = req.Msg
			return stream.Send(&v1.ExecAgentResponse{})
		},
	}
	localFile := setupCpDaemonStreamTest(t, mock)

	out, errOut, err := runCpCommand(t, localFile, "test-agent:/tmp/test.txt")
	if err != nil {
		t.Fatalf("cp: %v\nstderr:\n%s", err, errOut)
	}
	if !strings.Contains(out, "Copied") {
		t.Errorf("stdout %q does not report the copy", out)
	}
	if gotRequest == nil {
		t.Fatal("the daemon never received the transfer request")
	}
	if gotRequest.AgentId != "test-agent" {
		t.Errorf("AgentId = %q, want test-agent", gotRequest.AgentId)
	}
	if gotRequest.Command != "sh" || len(gotRequest.Args) != 2 || gotRequest.Args[0] != "-c" {
		t.Fatalf("remote command = %q %v, want sh -c <script>", gotRequest.Command, gotRequest.Args)
	}
	script := gotRequest.Args[1]
	for _, want := range []string{
		"cat > '/tmp/test.txt'",
		"chmod 0644", // the source file's mode rides the script
		`chown "$(id -u):$(id -g)"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("remote script %q missing %q", script, want)
		}
	}
	if !strings.Contains(string(gotRequest.StdinPayload), "payload-60-bytes") {
		t.Errorf("stdin payload %q does not carry the file bytes", gotRequest.StdinPayload)
	}
	if strings.Contains(errOut, "scp failed") {
		t.Errorf("stderr reports a failure on a successful transfer: %q", errOut)
	}
}

// TestCpDaemonStreamDestOpenFailure drives the missing-remote-directory
// failure through the fast path: the CLI prints the scp-child vocabulary
// verbatim (exit 2) and still fails non-zero with "scp: exit status 1".
func TestCpDaemonStreamDestOpenFailure(t *testing.T) {
	stubProbeSeams(t)
	mock := &cpExecStreamMock{
		cpMockServer: &cpMockServer{
			agent: &v1.AgentSummary{
				AgentId:    "test-agent",
				Status:     "running",
				SshfsMount: cpTestSSHFSMount,
			},
		},
		execAgent: func(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
			if err := stream.Send(&v1.ExecAgentResponse{
				Output: &v1.ExecAgentResponse_Stderr{
					Stderr: []byte("bash: line 1: /nonexistent-dir-020/x: No such file or directory\n"),
				},
				ExitCode: 2,
			}); err != nil {
				return err
			}
			return stream.Send(&v1.ExecAgentResponse{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("still streaming after the failure\n")}})
		},
	}
	localFile := setupCpDaemonStreamTest(t, mock)

	out, errOut, err := runCpCommand(t, localFile, "test-agent:/tmp/test.txt")
	if err == nil {
		t.Fatal("cp succeeded against a failed destination open")
	}
	if strings.Contains(out, "Copied") {
		t.Errorf("stdout reports a copy that did not complete:\n%s", out)
	}
	for _, want := range []string{
		`scp: dest open "/tmp/test.txt": No such file or directory`,
		"scp: failed to upload file " + localFile + " to /tmp/test.txt",
		"scp failed: 0 of 1 files landed",
		"scp: exit status 1",
	} {
		if !strings.Contains(errOut+err.Error(), want) {
			t.Errorf("failure output %q missing %q", errOut+err.Error(), want)
		}
	}
}

// TestCpDaemonStreamChownFailure drives the post-write chown failure (exit 4):
// still a hard failure with today's "chown after scp:" vocabulary.
func TestCpDaemonStreamChownFailure(t *testing.T) {
	stubProbeSeams(t)
	mock := &cpExecStreamMock{
		cpMockServer: &cpMockServer{
			agent: &v1.AgentSummary{
				AgentId:    "test-agent",
				Status:     "running",
				SshfsMount: cpTestSSHFSMount,
			},
		},
		execAgent: func(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
			return stream.Send(&v1.ExecAgentResponse{ExitCode: 4})
		},
	}
	localFile := setupCpDaemonStreamTest(t, mock)

	_, errOut, err := runCpCommand(t, localFile, "test-agent:/tmp/test.txt")
	if err == nil {
		t.Fatal("cp succeeded against a failed chown")
	}
	for _, want := range []string{"chown after scp: exit status 1", "scp failed: 0 of 1 files landed"} {
		if !strings.Contains(errOut+err.Error(), want) {
			t.Errorf("failure output %q missing %q", errOut+err.Error(), want)
		}
	}
}

// TestCpDaemonStreamTransportError covers a daemon that streams a transport
// failure: the transfer fails non-zero and the GAP-144 report still runs.
func TestCpDaemonStreamTransportError(t *testing.T) {
	stubProbeSeams(t)
	mock := &cpExecStreamMock{
		cpMockServer: &cpMockServer{
			agent: &v1.AgentSummary{
				AgentId:    "test-agent",
				Status:     "running",
				SshfsMount: cpTestSSHFSMount,
			},
		},
		execAgent: func(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
			return connect.NewError(connect.CodeUnimplemented, nil)
		},
	}
	localFile := setupCpDaemonStreamTest(t, mock)

	_, errOut, err := runCpCommand(t, localFile, "test-agent:/tmp/test.txt")
	if err == nil {
		t.Fatal("cp succeeded against a dead stream")
	}
	if !strings.Contains(err.Error(), "scp:") {
		t.Errorf("error %q lost the scp wrap", err)
	}
	if !strings.Contains(errOut, "scp failed") {
		t.Errorf("stderr %q lost the GAP-144 report", errOut)
	}
}

// TestCpExplicitFlagsKeepScp proves the fallback is real: with an explicit
// --ssh-key the transfer goes through the scp child (PATH stub), not the
// daemon stream.
func TestCpExplicitFlagsKeepScp(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(SessionTargetEnvVar, "custom-server")
	// BUNKER_HOME must be pinned BEFORE the config/key are written: the
	// CLI state dir follows it (probe seams set it here).
	stubProbeSeams(t)

	localFile := filepath.Join(tmpDir, "tiny.txt")
	if err := os.WriteFile(localFile, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	if err := defaultSSHKeyEnsure(t, "test-agent"); err != nil {
		t.Fatalf("ensure agent key: %v", err)
	}

	// The stub scp fails loudly; a daemon stream would never consult it.
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	scpStub := filepath.Join(binDir, "scp")
	if err := os.WriteFile(scpStub, []byte("#!/bin/sh\necho scp-stub-ran >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write scp stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mock := &cpExecStreamMock{
		cpMockServer: &cpMockServer{
			agent: &v1.AgentSummary{
				AgentId:    "test-agent",
				Status:     "running",
				SshfsMount: cpTestSSHFSMount,
			},
		},
		execAgent: func(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
			return stream.Send(&v1.ExecAgentResponse{})
		},
	}
	srv := newTestServer(t, mock)
	t.Cleanup(srv.Close)
	cfg := &CLIConfig{
		ActiveServer: "custom-server",
		Servers: map[string]ServerEntry{
			"custom-server": {URL: srv.URL},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	keyPath, _ := defaultSSHKeyPath("test-agent")
	_, errOut, err := runCpCommand(t, localFile, "test-agent:/tmp/test.txt", "--ssh-key", keyPath)
	if err == nil {
		t.Fatal("cp succeeded against a failing scp stub")
	}
	if !strings.Contains(errOut, "scp-stub-ran") {
		t.Errorf("stderr %q shows the scp stub did not run — the transfer took the daemon path", errOut)
	}
}

// TestCpDefaultPathNeverInvokesScp proves the fast path spawns nothing: with
// the scp binary stubbed to fail loudly, a default cp still succeeds end to
// end (the stub must never be exec'd).
func TestCpDefaultPathNeverInvokesScp(t *testing.T) {
	stubProbeSeams(t)
	mock := &cpExecStreamMock{
		cpMockServer: &cpMockServer{
			agent: &v1.AgentSummary{
				AgentId:    "test-agent",
				Status:     "running",
				SshfsMount: cpTestSSHFSMount,
			},
		},
		execAgent: func(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
			return stream.Send(&v1.ExecAgentResponse{})
		},
	}
	localFile := setupCpDaemonStreamTest(t, mock)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "scp"), []byte("#!/bin/sh\necho scp-should-not-run >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write scp stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, errOut, err := runCpCommand(t, localFile, "test-agent:/tmp/test.txt")
	if err != nil {
		t.Fatalf("cp: %v\nstderr:\n%s", err, errOut)
	}
	if !strings.Contains(out, "Copied") {
		t.Errorf("stdout %q does not report the copy", out)
	}
	if strings.Contains(errOut, "scp-should-not-run") {
		t.Errorf("stderr %q shows scp was exec'd on the fast path", errOut)
	}
}
