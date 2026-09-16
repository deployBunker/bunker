package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

func TestNewMountCommand_Structure(t *testing.T) {
	cmd := NewMountCommand()

	if cmd.Use != "mount <agent-id> [mountpoint]" {
		t.Errorf("Use = %q, want %q", cmd.Use, "mount <agent-id> [mountpoint]")
	}
	if cmd.Short != "Mount an agent's home directory via SSHFS" {
		t.Errorf("Short = %q", cmd.Short)
	}
	// Args: cobra.RangeArgs(1, 2) — minimum 1, maximum 2
	if cmd.Args == nil {
		t.Fatal("Args is nil, expected RangeArgs(1, 2)")
	}
}

func TestNewMountCommand_ArgsValidation_NoArgs(t *testing.T) {
	cmd := NewMountCommand()
	// RangeArgs(1, 2) should reject 0 args
	if cmd.Args(cmd, []string{}) == nil {
		t.Error("expected error for 0 args, got nil")
	}
}

func TestNewMountCommand_ArgsValidation_OneArg(t *testing.T) {
	cmd := NewMountCommand()
	// RangeArgs(1, 2) should accept 1 arg
	if err := cmd.Args(cmd, []string{"agent-1"}); err != nil {
		t.Errorf("unexpected error for 1 arg: %v", err)
	}
}

func TestNewMountCommand_ArgsValidation_TwoArgs(t *testing.T) {
	cmd := NewMountCommand()
	// RangeArgs(1, 2) should accept 2 args
	if err := cmd.Args(cmd, []string{"agent-1", "/tmp/mnt"}); err != nil {
		t.Errorf("unexpected error for 2 args: %v", err)
	}
}

func TestNewMountCommand_ArgsValidation_ThreeArgs(t *testing.T) {
	cmd := NewMountCommand()
	// RangeArgs(1, 2) should reject 3 args
	if cmd.Args(cmd, []string{"agent-1", "/tmp/mnt", "extra"}) == nil {
		t.Error("expected error for 3 args, got nil")
	}
}

func TestNewMountCommand_ServerFlag(t *testing.T) {
	cmd := NewMountCommand()
	flag := cmd.Flags().Lookup("server")
	if flag == nil {
		t.Fatal("--server flag not registered")
	}
	if flag.Name != "server" {
		t.Errorf("flag name = %q, want %q", flag.Name, "server")
	}
}

func TestNewMountCommand_SSHKeyFlag(t *testing.T) {
	cmd := NewMountCommand()
	flag := cmd.Flags().Lookup("ssh-key")
	if flag == nil {
		t.Fatal("--ssh-key flag not registered (must mirror `bunker ssh`)")
	}
	if flag.Name != "ssh-key" {
		t.Errorf("flag name = %q, want %q", flag.Name, "ssh-key")
	}
}

func TestNewMountCommand_RunE_NoActiveServer(t *testing.T) {
	// Isolate from the real ~/.bunker/config.yaml: these tests rewrite the
	// on-disk config and must never touch the user's actual one.
	t.Setenv("HOME", t.TempDir())

	// Clear any active server config.
	cfg, err := LoadCLIConfig()
	if err != nil {
		t.Skipf("cannot load config: %v", err)
	}
	cfg.ActiveServer = ""
	cfg.Servers = map[string]ServerEntry{}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Skipf("cannot save config: %v", err)
	}
	defer func() {
		// Restore empty config
		cfg.ActiveServer = ""
		cfg.Servers = map[string]ServerEntry{}
		_ = SaveCLIConfig(cfg)
	}()

	cmd := NewMountCommand()
	cmd.SetArgs([]string{"test-agent"})
	err = cmd.Execute()
	if err == nil {
		t.Error("expected error for no active server, got nil")
	}
}

func TestNewMountCommand_RunE_ServerNotFound(t *testing.T) {
	// Isolate from the real ~/.bunker/config.yaml: these tests rewrite the
	// on-disk config and must never touch the user's actual one.
	t.Setenv("HOME", t.TempDir())

	cfg, err := LoadCLIConfig()
	if err != nil {
		t.Skipf("cannot load config: %v", err)
	}
	cfg.ActiveServer = "nonexistent"
	cfg.Servers = map[string]ServerEntry{}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Skipf("cannot save config: %v", err)
	}
	defer func() {
		cfg.ActiveServer = ""
		cfg.Servers = map[string]ServerEntry{}
		_ = SaveCLIConfig(cfg)
	}()

	cmd := NewMountCommand()
	cmd.SetArgs([]string{"test-agent"})
	err = cmd.Execute()
	if err == nil {
		t.Error("expected error for server not found, got nil")
	}
}

// newMountTestServer starts an httptest server whose GetAgent returns the
// given stored sshfs mount command, and points the CLI config at it.
func newMountTestServer(t *testing.T, sshfsMount string) {
	t.Helper()
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(&mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:    "df0916a",
				SshfsMount: sshfsMount,
			},
		},
	})
	r.Mount(path, h)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"default": {Name: "default", URL: server.URL},
		},
		ActiveServer: "default",
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}
}

// writeMountClientKey creates the client-local SSH key the CLI must resolve
// for the fixture agent (defaultSSHKeyPath) and returns its path. The mount
// command must fail fast when this file is missing, so behavioral tests call
// this explicitly.
func writeMountClientKey(t *testing.T) string {
	t.Helper()
	keyPath, err := defaultSSHKeyPath("df0916a")
	if err != nil {
		t.Fatalf("defaultSSHKeyPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("FAKE-TEST-KEY-NOT-REAL\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(keyPath) })
	return keyPath
}

// stubSSHFSRun installs a stub for the sshfsRun seam that fails with the
// given error for the first failTimes calls and succeeds afterwards,
// recording each invocation. It returns the recorded calls and a restore
// func; the retry delay is zeroed so tests never sleep.
func stubSSHFSRun(t *testing.T, failTimes int, failErr error) (calls *[][]string, restore func()) {
	t.Helper()
	calls = &[][]string{}
	oldRun := sshfsRun
	oldDelay := sshfsRetryDelay
	sshfsRetryDelay = 0
	sshfsRun = func(ctx context.Context, path string, args []string, stdout, stderr io.Writer) error {
		*calls = append(*calls, append([]string{path}, args...))
		if len(*calls) <= failTimes {
			fmt.Fprint(stderr, "read: Connection reset by peer\n")
			return failErr
		}
		return nil
	}
	return calls, func() {
		sshfsRun = oldRun
		sshfsRetryDelay = oldDelay
	}
}

const mountFixtureSshfsMount = "sshfs -o IdentityFile=/etc/bunkerd/ssh/df0916a -o idmap=user -o allow_other bunker-agent@bunker-host:/home/bunker-agent /mnt/bunker/df0916a"

// runMountExecutesSSHFS runs `bunker mount df0916a <mountpoint>` against the
// mock server with the stubbed runner and returns the RunE error.
func runMountExecutesSSHFS(t *testing.T, mountpoint string) error {
	t.Helper()
	cmd := NewMountCommand()
	args := []string{"df0916a"}
	if mountpoint != "" {
		args = append(args, mountpoint)
	}
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd.Execute()
}

func TestMountCommand_RetriesUntilSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)
	calls, restore := stubSSHFSRun(t, 2, errors.New("exit status 1"))
	defer restore()

	if err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt"); err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if got := len(*calls); got != 3 {
		t.Fatalf("sshfs called %d times, want 3 (fail, fail, succeed)", got)
	}
}

func TestMountCommand_BoundedRetryExhausted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)
	calls, restore := stubSSHFSRun(t, 99, errors.New("exit status 1"))
	defer restore()

	err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt")
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	if got := len(*calls); got != 3 {
		t.Fatalf("sshfs called %d times, want exactly 3 (bounded, never 4)", got)
	}
	// The captured output contains the transient fragment ("Connection
	// reset by peer"), so the session-limit hint IS evidenced here. The
	// classified cause leads the message; the hint is appended only
	// because the transient fragments are actually present in the output.
	wantHint := "sshfs failed after 3 attempts (connection reset by peer) — agent host may be limiting parallel SSH sessions; try again or close other tunnels"
	if !strings.Contains(err.Error(), wantHint) {
		t.Errorf("error missing evidenced session-limit hint, got: %v", err)
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("error missing attempt count, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Connection reset by peer") {
		t.Errorf("error missing captured sshfs output, got: %v", err)
	}
}

func TestMountCommand_NoRetryOnPermanentFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)
	oldRun := sshfsRun
	oldDelay := sshfsRetryDelay
	sshfsRetryDelay = 0
	calls := 0
	sshfsRun = func(ctx context.Context, path string, args []string, stdout, stderr io.Writer) error {
		calls++
		fmt.Fprint(stderr, "fuse:Permission denied\n")
		return errors.New("exit status 1")
	}
	defer func() {
		sshfsRun = oldRun
		sshfsRetryDelay = oldDelay
	}()

	err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt")
	if err == nil {
		t.Fatal("expected error for permanent failure")
	}
	if calls != 1 {
		t.Fatalf("sshfs called %d times, want exactly 1 (no retry on permission denied)", calls)
	}
	if strings.Contains(err.Error(), "limiting parallel SSH sessions") {
		t.Errorf("permanent failure must not hint at session limiting, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("error must preserve the permanent cause, got: %v", err)
	}
}

func TestMountCommand_CausePreservedAfterExhaustedRetries(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)
	_, restore := stubSSHFSRun(t, 99, errors.New("exit status 1"))
	defer restore()

	err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt")
	if err == nil {
		t.Fatal("expected error")
	}
	unwrapped := errors.Unwrap(err)
	if unwrapped == nil {
		t.Fatalf("error must wrap the underlying cause, got: %v", err)
	}
	if !strings.Contains(unwrapped.Error(), "exit status 1") {
		t.Errorf("unwrapped cause = %q, want it to contain \"exit status 1\"", unwrapped.Error())
	}
}

func TestMountCommand_ArgvRegression(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	clientKey := writeMountClientKey(t)
	calls, restore := stubSSHFSRun(t, 0, nil)
	defer restore()

	mountpoint := t.TempDir() + "/mnt"
	if err := runMountExecutesSSHFS(t, mountpoint); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := len(*calls); got != 1 {
		t.Fatalf("sshfs called %d times, want 1", got)
	}
	want := []string{
		"sshfs",
		// Injected ssh options first, in the fixed order.
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		// Stored command parts, with the daemon-local key path rewritten
		// to the client-local key and the daemon hostname resolved to the
		// host the client actually reaches (the server URL hostname).
		"-o", "IdentityFile=" + clientKey,
		"-o", "idmap=user",
		"-o", "allow_other",
		"bunker-agent@127.0.0.1:/home/bunker-agent",
		// Caller mount point last, default replaced.
		mountpoint,
	}
	got := (*calls)[0]
	if len(got) != len(want) {
		t.Fatalf("argv length = %d, want %d\ngot:  %q\nwant: %q", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestMountCommand_UsesClientKeyAndResolvedHost pins DF-BUNKER-14: the args
// handed to the sshfs seam must carry the CLIENT key path and the resolved
// host, never the daemon-local /etc/bunkerd/ssh path or the daemon-only
// hostname baked into the stored command.
func TestMountCommand_UsesClientKeyAndResolvedHost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	clientKey := writeMountClientKey(t)
	calls, restore := stubSSHFSRun(t, 0, nil)
	defer restore()

	if err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(*calls) == 0 {
		t.Fatal("sshfs seam never called")
	}
	argv := strings.Join((*calls)[0], " ")
	if strings.Contains(argv, "/etc/bunkerd/ssh/") {
		t.Errorf("argv must not carry the daemon-local key path, got: %s", argv)
	}
	if !strings.Contains(argv, "IdentityFile="+clientKey) {
		t.Errorf("argv must carry the client key %q, got: %s", clientKey, argv)
	}
	if strings.Contains(argv, "bunker-agent@bunker-host") {
		t.Errorf("argv must not carry the daemon-only hostname, got: %s", argv)
	}
	if !strings.Contains(argv, "bunker-agent@127.0.0.1:") {
		t.Errorf("argv must carry the resolved host (server URL hostname), got: %s", argv)
	}
}

// TestMountCommand_MissingLocalKey_FailsFast pins DF-BUNKER-14 C2: with no
// client-local key on disk the command fails fast with an actionable error
// and the sshfs seam is never invoked.
func TestMountCommand_MissingLocalKey_FailsFast(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	calls, restore := stubSSHFSRun(t, 0, nil)
	defer restore()

	expectedKey, err := defaultSSHKeyPath("df0916a")
	if err != nil {
		t.Fatalf("defaultSSHKeyPath: %v", err)
	}

	mountErr := runMountExecutesSSHFS(t, t.TempDir()+"/mnt")
	if mountErr == nil {
		t.Fatal("expected fast error for missing client key, got success")
	}
	if !strings.Contains(mountErr.Error(), "SSH key not found at") {
		t.Errorf("error missing actionable key message, got: %v", mountErr)
	}
	if !strings.Contains(mountErr.Error(), expectedKey) {
		t.Errorf("error must name the expected key path %q, got: %v", expectedKey, mountErr)
	}
	if !strings.Contains(mountErr.Error(), "--ssh-key") {
		t.Errorf("error must suggest --ssh-key, got: %v", mountErr)
	}
	if got := len(*calls); got != 0 {
		t.Fatalf("sshfs called %d times, want 0 (must never run without the key)", got)
	}
}

// TestMountCommand_SSHKeyFlagOverrideWins pins DF-BUNKER-14 (c): an explicit
// --ssh-key path replaces both the default client key and the daemon-baked
// IdentityFile.
func TestMountCommand_SSHKeyFlagOverrideWins(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t) // default key exists, but the flag must win
	customKey := filepath.Join(t.TempDir(), "custom-key")
	if err := os.WriteFile(customKey, []byte("CUSTOM-TEST-KEY\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	calls, restore := stubSSHFSRun(t, 0, nil)
	defer restore()

	mountpoint := t.TempDir() + "/mnt"
	cmd := NewMountCommand()
	cmd.SetArgs([]string{"df0916a", mountpoint, "--ssh-key", customKey})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(*calls) == 0 {
		t.Fatal("sshfs seam never called")
	}
	argv := strings.Join((*calls)[0], " ")
	if !strings.Contains(argv, "IdentityFile="+customKey) {
		t.Errorf("explicit --ssh-key must win, want IdentityFile=%s in: %s", customKey, argv)
	}
	if strings.Contains(argv, "/etc/bunkerd/ssh/") {
		t.Errorf("daemon-local key path must not survive, got: %s", argv)
	}
}

// TestMountCommand_TransientHintRequiresFragmentInOutput pins DF-BUNKER-14
// (e)/C3: after exhausted retries the final message reports the CLASSIFIED
// cause and only claims sshd session limiting when the captured output
// actually contains the transient fragments that indicate it. An EOF-style
// failure with no fragments must not produce the session-limit hint.
func TestMountCommand_TransientHintRequiresFragmentInOutput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)
	oldRun := sshfsRun
	oldDelay := sshfsRetryDelay
	sshfsRetryDelay = 0
	calls := 0
	sshfsRun = func(ctx context.Context, path string, args []string, stdout, stderr io.Writer) error {
		calls++
		// No output fragments at all; the failure is only visible in the
		// process error (io.EOF classifies transient).
		return io.EOF
	}
	defer func() {
		sshfsRun = oldRun
		sshfsRetryDelay = oldDelay
	}()

	err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt")
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	if calls != 3 {
		t.Fatalf("sshfs called %d times, want exactly 3 (transient class still retries)", calls)
	}
	if strings.Contains(err.Error(), "limiting parallel SSH sessions") {
		t.Errorf("session-limit hint is unevidenced without transient fragments in output, got: %v", err)
	}
	if !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("final message must report the classified cause, got: %v", err)
	}
}

func TestMountCommand_ClassifySSHFSFailure(t *testing.T) {
	cases := []struct {
		name   string
		output string
		err    error
		want   string
	}{
		{"reset", "read: Connection reset by peer", errors.New("exit status 1"), "transient"},
		{"disconnected", "Connection to host closed by remote host.\nsshfs: remote host has disconnected", errors.New("exit status 1"), "transient"},
		{"connection closed", "Connection closed by 1.2.3.4 port 22", errors.New("exit status 1"), "transient"},
		{"permission denied wins over reset", "Permission denied (publickey).\nConnection closed", errors.New("exit status 1"), "permanent"},
		{"no such file", "fuse: mountpoint is not empty\n", errors.New("exit status 1"), "permanent"},
		{"fuse device", "fuse: device not found, try 'modprobe fuse' first", errors.New("exit status 1"), "permanent"},
		{"transport endpoint", "transport endpoint is not connected", errors.New("exit status 1"), "permanent"},
		{"eof error", "", io.EOF, "transient"},
		{"unexpected eof error", "", io.ErrUnexpectedEOF, "transient"},
		{"plain exit, no fragments", "some other failure", errors.New("exit status 1"), "unknown"},
		{"nil error", "", nil, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, _ := classifySSHFSFailure(tc.output, tc.err)
			if class != tc.want {
				t.Errorf("classifySSHFSFailure(%q) class = %q, want %q", tc.output, class, tc.want)
			}
		})
	}
}

func TestMountCommand_CaptureWritersWired(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)
	oldRun := sshfsRun
	oldDelay := sshfsRetryDelay
	sshfsRetryDelay = 0
	sshfsRun = func(ctx context.Context, path string, args []string, stdout, stderr io.Writer) error {
		// The RunE must hand non-nil capture writers to the seam so sshfs
		// output can be classified; the default seam tees those writers into
		// the terminal (see TestMountSSHFSRun_StreamsAndCaptures).
		if stdout == nil || stderr == nil {
			t.Error("RunE must pass non-nil capture writers to sshfsRun")
		}
		return nil
	}
	defer func() {
		sshfsRun = oldRun
		sshfsRetryDelay = oldDelay
	}()

	if err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestMountSSHFSRun_StreamsAndCaptures(t *testing.T) {
	// The default sshfsRun implementation writes the child's output to BOTH
	// the terminal (os.Stdout/os.Stderr via io.MultiWriter) and the
	// caller-provided capture writers — nothing is swallowed on success.
	var stdoutCap, stderrCap bytes.Buffer
	if err := sshfsRun(context.Background(), "sh", []string{"-c", "echo bunker-test-marker; echo bunker-test-err >&2"}, &stdoutCap, &stderrCap); err != nil {
		t.Fatalf("sshfsRun: %v", err)
	}
	if !strings.Contains(stdoutCap.String(), "bunker-test-marker") {
		t.Errorf("stdout capture missing marker, got %q", stdoutCap.String())
	}
	if !strings.Contains(stderrCap.String(), "bunker-test-err") {
		t.Errorf("stderr capture missing marker, got %q", stderrCap.String())
	}
}

func TestMountCommand_TrimSSHFSOutput(t *testing.T) {
	if got := trimSSHFSOutput("  \n"); got != "(none)" {
		t.Errorf("empty output = %q, want %q", got, "(none)")
	}
	long := strings.Repeat("x", 900)
	got := trimSSHFSOutput(long)
	if len(got) > 520 {
		t.Errorf("trimmed output too long: %d chars", len(got))
	}
	if !strings.HasSuffix(got, strings.Repeat("x", 500)) {
		t.Errorf("trimmed output must keep the tail, got %q", got[:50])
	}
}
