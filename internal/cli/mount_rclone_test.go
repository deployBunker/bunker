//go:build unix

// Package cli: the MOUNT-007 rclone driver client tests. The driver dispatch
// and the sshfs path are pinned by mount_driver_test.go / mount_test.go; this
// file proves the rclone arm end to end: a MountSpec naming rclone rewrites
// the stored command for this client (key, host, mountpoint), execs rclone
// ONCE through the rclone seam — never through the sshfs retry loop — and
// the sshfs path stays untouched.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/deployBunker/bunker/internal/mountdriver"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// mountFixtureRcloneMount is the exact shape buildMountCommand stores for an
// rclone agent (server-local key path, server-reported hostname, default
// mountpoint last) — the same shape rewriteRcloneMount anchors on.
const mountFixtureRcloneMount = "rclone mount :sftp:/home/bunker-agent /mnt/bunker/df0916a --sftp-user bunker-agent --sftp-key-file /etc/bunkerd/ssh/df0916a --sftp-host bunker-host --vfs-cache-mode minimal --daemon"

// newRcloneSpecTestServer starts a mock GetAgent whose AgentSummary carries
// the rclone MountSpec, and points the CLI config at it.
func newRcloneSpecTestServer(t *testing.T) {
	t.Helper()
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(&mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:    "df0916a",
				SshfsMount: mountFixtureRcloneMount,
				MountSpec:  &v1.MountSpec{Driver: mountdriver.DriverRclone, Command: mountFixtureRcloneMount},
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
	t.Setenv(SessionTargetEnvVar, "default")
}

// stubRcloneRun installs a stub for the rcloneRun seam that records each
// invocation and returns wantErr. Returns the recorded calls and a restore
// func.
func stubRcloneRun(t *testing.T, wantErr error) (*[][]string, func()) {
	t.Helper()
	calls := &[][]string{}
	oldRun := rcloneRun
	rcloneRun = func(ctx context.Context, path string, args []string, stdout, stderr io.Writer) error {
		*calls = append(*calls, append([]string{path}, args...))
		if wantErr != nil {
			_, _ = fmt.Fprint(stderr, "rclone: fatal error: connection refused\n")
			return wantErr
		}
		return nil
	}
	return calls, func() { rcloneRun = oldRun }
}

// runRcloneMountExecutes runs `bunker mount df0916a <mountpoint>` against
// the mock server with the rclone runner stubbed, returning the RunE error.
func runRcloneMountExecutes(t *testing.T, mountpoint string) error {
	t.Helper()
	_, restorePreflight := stubRemotePathCheck(t, nil)
	defer restorePreflight()

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

// TestMountCommandRcloneDriverRewritesAndExecsOnce is the end-to-end client
// cell: an agent whose MountSpec names rclone dispatches to the rclone arm,
// the stored command is rewritten (client-local key, resolved host, chosen
// mountpoint), and rclone execs exactly ONCE — no sshfs-shaped retry loop.
func TestMountCommandRcloneDriverRewritesAndExecsOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newRcloneSpecTestServer(t)
	clientKey := writeMountClientKey(t)
	rcloneCalls, restoreRclone := stubRcloneRun(t, nil)
	defer restoreRclone()
	sshfsCalls, restoreSSHFS := stubSSHFSRun(t, 0, nil)
	defer restoreSSHFS()

	mnt := t.TempDir() + "/mnt"
	if err := runRcloneMountExecutes(t, mnt); err != nil {
		t.Fatalf("rclone mount failed: %v", err)
	}
	if len(*rcloneCalls) != 1 {
		t.Fatalf("rclone exec'd %d time(s), want exactly 1 (no retry loop): %v", len(*rcloneCalls), *rcloneCalls)
	}
	if len(*sshfsCalls) != 0 {
		t.Fatalf("the sshfs runner executed %d time(s) for an rclone agent; the arms must not cross", len(*sshfsCalls))
	}
	argv := (*rcloneCalls)[0]
	if argv[0] != "rclone" {
		t.Errorf("exec'd %q, want rclone", argv[0])
	}
	if argv[1] != "mount" {
		t.Errorf("argv[1] = %q, want mount", argv[1])
	}
	foundBackend, foundKey, foundHost, foundMountpoint := false, false, false, false
	for i, a := range argv {
		switch {
		case strings.HasPrefix(a, ":sftp:/home/bunker-agent"):
			foundBackend = true
		case a == "--sftp-key-file" && i+1 < len(argv) && argv[i+1] == clientKey:
			foundKey = true
		case a == "--sftp-host" && i+1 < len(argv) && argv[i+1] != "" && argv[i+1] != "bunker-host":
			// Resolved to the mock server's address (the httptest URL host),
			// per the same resolveSSHHost rule the sshfs arm uses.
			foundHost = true
		case a == mnt:
			foundMountpoint = true
		}
	}
	if !foundBackend {
		t.Errorf("backend :sftp:/home/bunker-agent missing from argv: %v", argv)
	}
	if !foundKey {
		t.Errorf("--sftp-key-file not rewritten to the client key %q: %v", clientKey, argv)
	}
	if !foundHost {
		t.Errorf("--sftp-host not resolved away from the daemon hostname: %v", argv)
	}
	if !foundMountpoint {
		t.Errorf("mountpoint %q not substituted as the last argument: %v", mnt, argv)
	}
	for _, a := range argv {
		if a == "--daemon" {
			return
		}
	}
	t.Errorf("--daemon missing from the rewritten command; bunker mount would block on the VFS loop: %v", argv)
}

// TestMountCommandRcloneFailureSurfacesOnceAndNamesNoClassifier proves the
// failure contract: rclone's own diagnostics surface, the message says no
// classifier applies, and there is exactly one attempt — never an
// sshfs-shaped retry.
func TestMountCommandRcloneFailureSurfacesOnceAndNamesNoClassifier(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	newRcloneSpecTestServer(t)
	writeMountClientKey(t)
	rcloneCalls, restoreRclone := stubRcloneRun(t, errors.New("exit status 1"))
	defer restoreRclone()

	if err := runRcloneMountExecutes(t, t.TempDir()+"/mnt"); err == nil {
		t.Fatal("rclone mount succeeded despite the stubbed failure")
	}
	if got := len(*rcloneCalls); got != 1 {
		t.Fatalf("rclone exec'd %d time(s) on failure, want exactly 1 (the driver declares no classifier, so no retry): %v", got, *rcloneCalls)
	}
}

// TestRewriteRcloneMountRefusesBrokenShapes is the unit grid for the
// rewrite's fail-loud contract: a non-rclone command, a missing key-file
// anchor, or a missing host anchor is a named refusal, never a guessed
// half-rewritten exec.
func TestRewriteRcloneMountRefusesBrokenShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		mountCmd   string
		wantErrHas string
	}{
		{
			name:       "not an rclone command",
			mountCmd:   mountFixtureSshfsMount,
			wantErrHas: "does not start with 'rclone mount'",
		},
		{
			name:       "no key-file anchor",
			mountCmd:   "rclone mount :sftp:/home/u /mnt/bunker/x --sftp-user u --sftp-host h --vfs-cache-mode minimal --daemon",
			wantErrHas: "--sftp-key-file",
		},
		{
			name:       "no :sftp: backend token",
			mountCmd:   "rclone mount /mnt/bunker/x --sftp-user u --sftp-key-file /k --sftp-host h --vfs-cache-mode minimal --daemon",
			wantErrHas: ":sftp:",
		},
		{
			name:       "no host anchor",
			mountCmd:   "rclone mount :sftp:/home/u /mnt/bunker/x --sftp-user u --sftp-key-file /k --vfs-cache-mode minimal --daemon",
			wantErrHas: "--sftp-host",
		},
		{
			name:       "tail without --daemon",
			mountCmd:   "rclone mount :sftp:/home/u /mnt/bunker/x --sftp-user u --sftp-key-file /k --sftp-host h --vfs-cache-mode minimal",
			wantErrHas: "--daemon",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parts, err := rewriteRcloneMount(tc.mountCmd, "resolved-host", "/client/key", "/mnt/x")
			if err == nil {
				t.Fatalf("rewriteRcloneMount accepted %q: %v", tc.mountCmd, parts)
			}
			if !strings.Contains(err.Error(), tc.wantErrHas) {
				t.Errorf("error %q does not name the cause %q", err, tc.wantErrHas)
			}
		})
	}
}

// TestRewriteRcloneMountHappyPath pins the rewrite contract directly:
// key path and host replaced, backend and flags preserved, mountpoint last.
func TestRewriteRcloneMountHappyPath(t *testing.T) {
	t.Parallel()
	parts, err := rewriteRcloneMount(mountFixtureRcloneMount, "resolved-host", "/client/key", "/mnt/choice")
	if err != nil {
		t.Fatalf("rewriteRcloneMount: %v", err)
	}
	joined := strings.Join(parts, " ")
	if !strings.Contains(joined, "--sftp-key-file /client/key") {
		t.Errorf("client key not substituted: %v", parts)
	}
	if !strings.Contains(joined, "--sftp-host resolved-host") {
		t.Errorf("host not resolved: %v", parts)
	}
	if !strings.Contains(joined, ":sftp:/home/bunker-agent") {
		t.Errorf("backend spec must be preserved: %v", parts)
	}
	if !strings.Contains(joined, "--vfs-cache-mode minimal") {
		t.Errorf("flags must be preserved: %v", parts)
	}
	// The mountpoint is the positional immediately after the :sftp: backend
	// token, and --daemon stays last.
	for i, a := range parts {
		if strings.HasPrefix(a, ":sftp:") {
			if parts[i+1] != "/mnt/choice" {
				t.Errorf("mountpoint must follow the :sftp: backend token: %v", parts)
			}
			break
		}
	}
	if got := parts[len(parts)-1]; got != "--daemon" {
		t.Errorf("--daemon must stay last so bunker mount never blocks: %v", parts)
	}
}
