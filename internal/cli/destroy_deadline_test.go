package cli

// INT-CI-050: the `bunker destroy` client deadline and the daemon's
// fail-closed home archive.
//
// The daemon archives the agent's whole home before userdel (DF-BUNKER-33) and
// runs that `tar` with exec.CommandContext(ctx, …) on the REQUEST context, so
// the CLI's deadline IS the tar's deadline. The pre-fix 30s literal SIGKILLed
// a ~300MB image-spec home's archive at 25s on the self-hosted runner (CI run
// 36487719950, battery section 13): the destroy was refused with
// home_retained, the agent's user survived every retry ("imgspec agents not
// fully destroyed"), and truncated .tar.gz files accumulated in
// /var/backups/bunker. These tests pin the invariant (the client must not give
// up before the daemon's own request budget) and prove the deadline seam
// actually bounds the RPC rather than being decorative.

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/deployBunker/bunker/internal/config"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// TestDestroyRequestTimeoutCoversDaemonRequestBudget is the pin that would
// have failed on the 30s literal: the destroy client deadline must never be
// SHORTER than the daemon's per-request budget, because the handler hands that
// context to the archive `tar`.
func TestDestroyRequestTimeoutCoversDaemonRequestBudget(t *testing.T) {
	if destroyRequestTimeout < config.DefaultServerRequestTimeout {
		t.Fatalf("destroyRequestTimeout = %s is shorter than the daemon request budget %s: "+
			"the client deadline is the context the daemon hands to `tar` for the fail-closed home "+
			"archive (DF-BUNKER-33), so expiring first SIGKILLs the archive mid-stream and the destroy "+
			"is refused with home_retained while the agent survives", destroyRequestTimeout, config.DefaultServerRequestTimeout)
	}
}

// slowDestroyServer answers DestroyAgent only after delay — a stand-in for a
// daemon that is busy archiving a large home.
type slowDestroyServer struct {
	mockBunkerdServer
	delay time.Duration
}

func (m *slowDestroyServer) DestroyAgent(
	ctx context.Context,
	req *connect.Request[v1.DestroyAgentRequest],
) (*connect.Response[v1.DestroyAgentResponse], error) {
	select {
	case <-time.After(m.delay):
	case <-ctx.Done():
		return nil, connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
	}
	return connect.NewResponse(&v1.DestroyAgentResponse{AgentId: req.Msg.GetAgentId(), Status: "destroyed"}), nil
}

// TestDestroyCommandDeadlineBoundsTheRPC drives the REAL command against that
// server twice. The seam is shrunk only to keep the test fast: what it proves
// is the SHAPE of the failure — a deadline that expires while the daemon is
// still working cancels the destroy (arm 2, the archive-killing mode), and a
// deadline that covers the work returns the daemon's answer (arm 1).
func TestDestroyCommandDeadlineBoundsTheRPC(t *testing.T) {
	orig := destroyRequestTimeout
	t.Cleanup(func() { destroyRequestTimeout = orig })

	const daemonWork = 300 * time.Millisecond
	srv := newDestroyTestServer(t, &slowDestroyServer{delay: daemonWork})
	defer srv.Close()

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(SessionTargetEnvVar, "default")
	writeDestroyTestConfig(t, tmpDir, srv.URL)

	// Arm 1: the deadline covers the daemon's work → the CLI reports the
	// daemon's verdict instead of cancelling.
	destroyRequestTimeout = 10 * time.Second
	cmd := NewDestroyCommand()
	cmd.SetArgs([]string{"--server", "default", "deadline-ok"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("destroy against a slow (archiving) daemon failed with a %s deadline: %v", destroyRequestTimeout, err)
		}
	})
	if !strings.Contains(output, "deadline-ok destroyed") {
		t.Errorf("output missing 'deadline-ok destroyed', got:\n%s", output)
	}

	// Arm 2: the deadline expires first → the RPC is cancelled, which is
	// exactly the client-side shape that killed the daemon's `tar`.
	destroyRequestTimeout = 50 * time.Millisecond
	cmd2 := NewDestroyCommand()
	cmd2.SetArgs([]string{"--server", "default", "deadline-short"})
	err := cmd2.Execute()
	if err == nil {
		t.Fatalf("destroy with a %s deadline against %s of daemon work returned nil, want a deadline error", destroyRequestTimeout, daemonWork)
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error %v does not name the deadline — the seam may have stopped reaching the RPC context", err)
	}
}
