package cli

// DF-BUNKER-61: a client deadline expiry is not the daemon's verdict.
//
// The defect (dogfood 2026-09-25): `bunker destroy` reported
// `destroy agent: deadline_exceeded: context deadline exceeded` three times for
// an agent the same DestroyAgent destroyed fine over raw REST. The CLI turned
// its OWN timeout into a hard "the destroy failed" statement, so the operator
// could not tell an aborted teardown from a completed one.
//
// These tests pin the three truthful outcomes — gone → success, still listed →
// NOT CONFIRMED, listing impossible → NOT CONFIRMED with the reason — and pin
// the thing that must NOT happen: a resent DestroyAgent racing the in-flight
// teardown. They also pin the --timeout override reaching the RPC context.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// deadlineDestroyServer never answers DestroyAgent before the client's own
// deadline: the shape of a daemon that is still archiving when the CLI gives
// up. It records how many DestroyAgent calls it saw, so a resent destroy (the
// double-teardown hazard) fails the test rather than passing silently.
type deadlineDestroyServer struct {
	mockBunkerdServer
	agents       []*v1.AgentSummary
	listErr      error
	destroyCalls int32
}

func (m *deadlineDestroyServer) DestroyAgent(
	ctx context.Context,
	req *connect.Request[v1.DestroyAgentRequest],
) (*connect.Response[v1.DestroyAgentResponse], error) {
	atomic.AddInt32(&m.destroyCalls, 1)
	<-ctx.Done()
	return nil, connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
}

func (m *deadlineDestroyServer) ListAgents(
	ctx context.Context,
	req *connect.Request[v1.ListAgentsRequest],
) (*connect.Response[v1.ListAgentsResponse], error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return connect.NewResponse(&v1.ListAgentsResponse{
		Agents:     m.agents,
		TotalCount: uint32(len(m.agents)),
	}), nil
}

// shrinkDestroyDeadlineSeams makes the tests fast without changing the shape
// under test: a short client deadline (so DestroyAgent is cancelled quickly)
// and a short re-list window/backoff.
func shrinkDestroyDeadlineSeams(t *testing.T) {
	t.Helper()
	origTimeout := destroyRequestTimeout
	origWindow := destroyRelistWindow
	origBackoff := destroyRelistBackoff
	t.Cleanup(func() {
		destroyRequestTimeout = origTimeout
		destroyRelistWindow = origWindow
		destroyRelistBackoff = origBackoff
	})
	destroyRequestTimeout = 50 * time.Millisecond
	destroyRelistWindow = 300 * time.Millisecond
	destroyRelistBackoff = 10 * time.Millisecond
}

// runDeadlineDestroy drives the real command against srv and returns the
// captured stdout plus the command's error.
func runDeadlineDestroy(t *testing.T, agentID string, extraArgs ...string) (string, error) {
	t.Helper()
	cmd := NewDestroyCommand()
	args := append([]string{"--server", "default"}, extraArgs...)
	args = append(args, agentID)
	cmd.SetArgs(args)
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	return out, err
}

// setupDeadlineDestroyTest wires HOME/config/session target for the real
// command, returning the mock server so a test can inspect it.
func setupDeadlineDestroyTest(t *testing.T, srv *deadlineDestroyServer) {
	t.Helper()
	httpSrv := newDestroyTestServer(t, srv)
	t.Cleanup(httpSrv.Close)
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(SessionTargetEnvVar, "default")
	writeDestroyTestConfig(t, tmpDir, httpSrv.URL)
}

// TestDestroy_DeadlineExceededRelistConfirmsDestroyed is the defect itself: the
// client deadline expires, but the daemon's registry no longer carries the
// agent, so the destroy DID happen. The command must say so and exit 0 — and
// must not have resent DestroyAgent to learn it.
func TestDestroy_DeadlineExceededRelistConfirmsDestroyed(t *testing.T) {
	shrinkDestroyDeadlineSeams(t)
	srv := &deadlineDestroyServer{agents: nil} // registry empty: the agent is gone
	setupDeadlineDestroyTest(t, srv)

	out, err := runDeadlineDestroy(t, "gone-agent")
	if err != nil {
		t.Fatalf("destroy returned %v after the daemon's list showed the agent gone; want nil "+
			"(a client deadline that expired while the daemon finished the teardown is a SUCCESS, not a failure)\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "gone-agent destroyed") {
		t.Errorf("output does not report the agent destroyed, got:\n%s", out)
	}
	if strings.Contains(out, "deadline_exceeded") {
		t.Errorf("output still surfaces the raw deadline_exceeded error for a completed destroy:\n%s", out)
	}
	if got := atomic.LoadInt32(&srv.destroyCalls); got != 1 {
		t.Errorf("DestroyAgent called %d times, want exactly 1 — a re-list must confirm, never a resent "+
			"destroy racing the daemon's in-flight archive+userdel", got)
	}
}

// TestDestroy_DeadlineExceededStillListedIsNotReportedDestroyed is the
// fail-closed half: the deadline expired AND the daemon still lists the agent,
// so the destroy is unconfirmed. The command must not claim success.
func TestDestroy_DeadlineExceededStillListedIsNotReportedDestroyed(t *testing.T) {
	shrinkDestroyDeadlineSeams(t)
	srv := &deadlineDestroyServer{agents: []*v1.AgentSummary{{AgentId: "stuck-agent", Status: "running"}}}
	setupDeadlineDestroyTest(t, srv)

	out, err := runDeadlineDestroy(t, "stuck-agent")
	if err == nil {
		t.Fatalf("destroy returned nil while the daemon still lists the agent after the deadline\noutput:\n%s", out)
	}
	if !strings.Contains(out, "NOT confirmed destroyed") {
		t.Errorf("output does not say the destroy is unconfirmed, got:\n%s", out)
	}
	if strings.Contains(out, "stuck-agent destroyed") {
		t.Errorf("output claims a destroy it did not witness:\n%s", out)
	}
	if !strings.Contains(out, "bunker info stuck-agent") {
		t.Errorf("output does not tell the operator how to re-check, got:\n%s", out)
	}
	if got := atomic.LoadInt32(&srv.destroyCalls); got != 1 {
		t.Errorf("DestroyAgent called %d times, want exactly 1 (no resend while one may be in flight)", got)
	}
}

// TestDestroy_DeadlineExceededListFailureFailsClosed: the confirmation itself
// is impossible (old daemon without ListAgents, permission refusal). An
// unanswerable list is NOT evidence of absence — the command must report the
// failure, name the reason, and not poll for the whole window.
func TestDestroy_DeadlineExceededListFailureFailsClosed(t *testing.T) {
	shrinkDestroyDeadlineSeams(t)
	srv := &deadlineDestroyServer{listErr: connect.NewError(connect.CodeUnimplemented, errors.New("no ListAgents"))}
	setupDeadlineDestroyTest(t, srv)

	start := time.Now()
	out, err := runDeadlineDestroy(t, "unknown-agent")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("destroy returned nil when the daemon could not be asked to confirm\noutput:\n%s", out)
	}
	if !strings.Contains(out, "NOT confirmed destroyed") {
		t.Errorf("output does not say the destroy is unconfirmed, got:\n%s", out)
	}
	if !strings.Contains(out, "could not be asked to confirm") {
		t.Errorf("output does not name the failed confirmation, got:\n%s", out)
	}
	if elapsed >= destroyRelistWindow {
		t.Errorf("confirmation took %s (>= the %s window) for a listing that can never succeed: "+
			"an errored list must fail closed after ONE attempt", elapsed, destroyRelistWindow)
	}
}

// TestDestroy_TimeoutConfigurable pins the --timeout override reaching the RPC
// context: the daemon needs 300ms, so only the explicit longer deadline can
// return the daemon's answer, and the printed line must show the value used.
func TestDestroy_TimeoutConfigurable(t *testing.T) {
	origTimeout := destroyRequestTimeout
	t.Cleanup(func() { destroyRequestTimeout = origTimeout })
	// The derived floor must be SHORTER than the daemon's work, otherwise the
	// override is not what saves the call and the test would prove nothing.
	destroyRequestTimeout = 50 * time.Millisecond

	srv := &slowDestroyServer{delay: 300 * time.Millisecond}
	httpSrv := newDestroyTestServer(t, srv)
	defer httpSrv.Close()

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(SessionTargetEnvVar, "default")
	writeDestroyTestConfig(t, tmpDir, httpSrv.URL)

	// Arm 1: explicit deadline longer than the work → the daemon's verdict.
	cmd := NewDestroyCommand()
	cmd.SetArgs([]string{"--server", "default", "--timeout", "10s", "timeout-ok"})
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	if err != nil {
		t.Fatalf("--timeout 10s did not override the 50ms derived deadline: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "timeout-ok destroyed") {
		t.Errorf("output missing 'timeout-ok destroyed', got:\n%s", out)
	}
	if !strings.Contains(out, "deadline 10s") {
		t.Errorf("output does not print the operator's deadline (the operator must be able to see what it waited on), got:\n%s", out)
	}

	// Arm 2: explicit deadline shorter than the work → still a deadline
	// expiry, so the seam is the RPC budget and not decoration.
	shrinkDestroyDeadlineSeams(t)
	cmd2 := NewDestroyCommand()
	cmd2.SetArgs([]string{"--server", "default", "--timeout", "50ms", "timeout-short"})
	err = cmd2.Execute()
	if err == nil {
		t.Fatalf("--timeout 50ms against 300ms of daemon work returned nil, want a deadline error")
	}

	// Arm 3: a negative deadline is refused before any RPC is attempted.
	cmd3 := NewDestroyCommand()
	cmd3.SetArgs([]string{"--server", "default", "--timeout", "-5s", "timeout-bad"})
	err = cmd3.Execute()
	if err == nil || !strings.Contains(err.Error(), "--timeout must be a positive duration") {
		t.Errorf("--timeout -5s returned %v, want a refusal naming the flag", err)
	}
}

// TestDestroy_TimeoutFlagInHelp pins the flag surface: an operator cannot use a
// deadline override they cannot find.
func TestDestroy_TimeoutFlagInHelp(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewDestroyCommand()
	out := captureStdout(t, func() {
		cmd.SetArgs([]string{"--server", "default", "--help"})
		_ = cmd.Execute()
	})
	for _, want := range []string{"--timeout", "derives it from the agent home's measured size"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q, got:\n%s", want, out)
		}
	}
}

// TestDestroyIsDeadlineExceeded pins the classification the whole arm hangs
// on: only a deadline expiry is re-checked. A daemon VERDICT (home retained,
// live processes, permission denied) already says the destroy did not happen.
func TestDestroyIsDeadlineExceeded(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"connect deadline exceeded", connect.NewError(connect.CodeDeadlineExceeded, errors.New("boom")), true},
		{"wrapped context deadline", errors.New("destroy agent: " + context.DeadlineExceeded.Error()), false},
		{"bare context deadline", context.DeadlineExceeded, true},
		{"connect internal (home retained)", connect.NewError(connect.CodeInternal, errors.New("home retained")), false},
		{"connect permission denied", connect.NewError(connect.CodePermissionDenied, errors.New("nope")), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := destroyIsDeadlineExceeded(tt.err); got != tt.want {
				t.Errorf("destroyIsDeadlineExceeded(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
