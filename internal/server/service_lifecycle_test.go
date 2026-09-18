package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// This file pins the GAP-071 service-layer contract:
//
//   - the three lifecycle RPCs map an unknown agent to CodeNotFound and any
//     other manager failure to CodeInternal (the DestroyAgent shape);
//   - Exec/Run/Heartbeat against a STOPPED agent return the DISTINCT
//     CodeFailedPrecondition carrying the token "agent_stopped" — never
//     CodeNotFound — while running agents and unknown ids keep their existing
//     behaviour exactly.

func lifecycleTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestLifecycleRPCs_ErrorMapping drives all three RPCs through the same fake
// manager and asserts the code mapping. On the error paths the handler returns
// no response message (matching DestroyAgent), so the asserted status is empty
// and the CODE is what distinguishes not_found from a real failure.
func TestLifecycleRPCs_ErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		mgrErr     error
		wantCode   connect.Code
		wantStatus string
		wantErr    bool
	}{
		{
			name:     "stop_unknown_agent_is_not_found",
			status:   agent.StatusNotFound,
			mgrErr:   errors.New("agent not found"),
			wantCode: connect.CodeNotFound,
			wantErr:  true,
		},
		{
			name:     "start_unknown_agent_is_not_found",
			status:   agent.StatusNotFound,
			mgrErr:   errors.New("agent not found"),
			wantCode: connect.CodeNotFound,
			wantErr:  true,
		},
		{
			name:     "restart_unknown_agent_is_not_found",
			status:   agent.StatusNotFound,
			mgrErr:   errors.New("agent not found"),
			wantCode: connect.CodeNotFound,
			wantErr:  true,
		},
		{
			name:     "stop_path_failure_is_internal",
			status:   agent.StatusInvalidValue,
			mgrErr:   errors.New("systemctl exploded"),
			wantCode: connect.CodeInternal,
			wantErr:  true,
		},
		{
			name:       "stop_success",
			status:     agent.StatusStoppedValue,
			wantStatus: agent.StatusStoppedValue,
		},
		{
			name:       "start_success",
			status:     agent.StatusStartedValue,
			wantStatus: agent.StatusStartedValue,
		},
		{
			name:       "restart_success",
			status:     agent.StatusRestarted,
			wantStatus: agent.StatusRestarted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &fakeAgentManager{lifecycleStatus: tt.status, lifecycleErr: tt.mgrErr}
			svc := &bunkerdService{cfg: config.DefaultConfig(), logger: lifecycleTestLogger(), agentMgr: mgr}

			status, err := callLifecycleRPC(svc, tt.name)
			if !mgr.lifecycleCalled {
				t.Fatal("the RPC never reached the agent manager")
			}
			if tt.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want %v", tt.wantCode)
				}
				if got := connect.CodeOf(err); got != tt.wantCode {
					t.Errorf("code = %v, want %v (err: %v)", got, tt.wantCode, err)
				}
			} else if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if status != tt.wantStatus {
				t.Errorf("status = %q, want %q", status, tt.wantStatus)
			}
		})
	}
}

// callLifecycleRPC dispatches by test name so one table can drive all three
// lifecycle RPCs and return their (status, error) pair.
func callLifecycleRPC(svc *bunkerdService, name string) (string, error) {
	ctx := context.Background()
	switch {
	case strings.HasPrefix(name, "stop"):
		resp, err := svc.StopAgent(ctx, connect.NewRequest(&v1.StopAgentRequest{AgentId: "agent-1"}))
		if resp == nil {
			return "", err
		}
		return resp.Msg.GetStatus(), err
	case strings.HasPrefix(name, "start"):
		resp, err := svc.StartAgent(ctx, connect.NewRequest(&v1.StartAgentRequest{AgentId: "agent-1"}))
		if resp == nil {
			return "", err
		}
		return resp.Msg.GetStatus(), err
	default:
		resp, err := svc.RestartAgent(ctx, connect.NewRequest(&v1.RestartAgentRequest{AgentId: "agent-1"}))
		if resp == nil {
			return "", err
		}
		return resp.Msg.GetStatus(), err
	}
}

// TestRestartAgent_CarriesRefreshedExpiry pins the proto field the CLI prints.
func TestRestartAgent_CarriesRefreshedExpiry(t *testing.T) {
	expiry := time.Now().Add(6 * time.Hour).Format(time.RFC3339)
	mgr := &fakeAgentManager{
		lifecycleStatus: agent.StatusRestarted,
		restartResp: &v1.RestartAgentResponse{
			AgentId:   "agent-1",
			Status:    agent.StatusRestarted,
			ExpiresAt: expiry,
		},
	}
	svc := &bunkerdService{cfg: config.DefaultConfig(), logger: lifecycleTestLogger(), agentMgr: mgr}

	resp, err := svc.RestartAgent(context.Background(), connect.NewRequest(&v1.RestartAgentRequest{AgentId: "agent-1"}))
	if err != nil {
		t.Fatalf("RestartAgent: %v", err)
	}
	if resp.Msg.GetExpiresAt() != expiry {
		t.Errorf("expires_at = %q, want %q", resp.Msg.GetExpiresAt(), expiry)
	}
}

// ── stopped-agent guards ────────────────────────────────────────

// TestExecAgent_StoppedAgentRejected drives the real connect handler so the
// error reaches the client as a wire error: a stopped agent must yield
// CodeFailedPrecondition naming "agent_stopped", and the command must NOT be
// attempted (the ssh stub records nothing).
func TestExecAgent_StoppedAgentRejected(t *testing.T) {
	sshArgv := installStubSSHForLifecycle(t)

	logger := lifecycleTestLogger()
	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{
		AgentID:           "stopped-1",
		Status:            agent.StatusStopped,
		SshPrivateKeyPath: filepath.Join(t.TempDir(), "key"),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker}

	path, handler := bunkerv1connect.NewBunkerdHandler(svc)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)

	stream, err := client.ExecAgent(context.Background(), connect.NewRequest(&v1.ExecAgentRequest{
		AgentId: "stopped-1",
		Command: "echo",
		Args:    []string{"hello"},
	}))
	if err != nil {
		t.Fatalf("ExecAgent call: %v", err)
	}
	for stream.Receive() {
		_ = stream.Msg()
	}
	serr := stream.Err()
	if serr == nil {
		t.Fatal("exec against a stopped agent must fail, got a clean stream")
	}
	if got := connect.CodeOf(serr); got != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want %v (err: %v)", got, connect.CodeFailedPrecondition, serr)
	}
	if got := connect.CodeOf(serr); got == connect.CodeNotFound {
		t.Error("a stopped agent must NEVER be reported as not_found")
	}
	if !strings.Contains(serr.Error(), "agent_stopped") {
		t.Errorf("error %q must carry the stable token agent_stopped", serr.Error())
	}
	if _, statErr := os.Stat(sshArgv); statErr == nil {
		t.Error("exec attempted to run a command against a stopped agent (ssh was invoked)")
	}
}

// TestExecAgent_RunningAndUnknownUnchanged is the negative control: a running
// agent still executes (ssh invoked, stream completes) and an unknown id is
// still CodeNotFound.
func TestExecAgent_RunningAndUnknownUnchanged(t *testing.T) {
	t.Run("running_agent_executes", func(t *testing.T) {
		sshArgv := installStubSSHForLifecycle(t)
		logger := lifecycleTestLogger()
		tracker := resource.NewTracker(10, logger)
		if err := tracker.Register(&resource.AgentRecord{
			AgentID:           "running-1",
			Status:            agent.StatusRunning,
			SshPrivateKeyPath: filepath.Join(t.TempDir(), "key"),
		}); err != nil {
			t.Fatalf("register: %v", err)
		}
		svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker}
		client, closeSrv := startTestBunkerd(t, svc)
		defer closeSrv()

		stream, err := client.ExecAgent(context.Background(), connect.NewRequest(&v1.ExecAgentRequest{
			AgentId: "running-1",
			Command: "echo",
			Args:    []string{"hello"},
		}))
		if err != nil {
			t.Fatalf("ExecAgent call: %v", err)
		}
		for stream.Receive() {
			_ = stream.Msg()
		}
		if serr := stream.Err(); serr != nil {
			t.Fatalf("running agent exec failed: %v", serr)
		}
		if _, statErr := os.Stat(sshArgv); statErr != nil {
			t.Errorf("running agent exec never reached ssh: %v", statErr)
		}
	})

	t.Run("unknown_agent_is_not_found", func(t *testing.T) {
		logger := lifecycleTestLogger()
		tracker := resource.NewTracker(10, logger)
		svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker}
		client, closeSrv := startTestBunkerd(t, svc)
		defer closeSrv()

		stream, err := client.ExecAgent(context.Background(), connect.NewRequest(&v1.ExecAgentRequest{
			AgentId: "ghost-1",
			Command: "echo",
		}))
		if err != nil {
			t.Fatalf("ExecAgent call: %v", err)
		}
		for stream.Receive() {
			_ = stream.Msg()
		}
		serr := stream.Err()
		if serr == nil {
			t.Fatal("exec of an unknown agent must fail")
		}
		if got := connect.CodeOf(serr); got != connect.CodeNotFound {
			t.Errorf("code = %v, want %v (err: %v)", got, connect.CodeNotFound, serr)
		}
	})
}

// TestRunAgent_StoppedAgentRejected pins the stopped guard for RunAgent,
// including that the manager is never reached (no unit can be started for a
// stopped agent) and that running/unknown keep their existing codes.
func TestRunAgent_StoppedAgentRejected(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(tracker *resource.Tracker)
		wantCode connect.Code
		wantErr  bool
	}{
		{
			name: "stopped_is_failed_precondition",
			setup: func(tracker *resource.Tracker) {
				_ = tracker.Register(&resource.AgentRecord{AgentID: "a1", Status: agent.StatusStopped})
			},
			wantCode: connect.CodeFailedPrecondition,
			wantErr:  true,
		},
		{
			name: "running_reaches_the_manager",
			setup: func(tracker *resource.Tracker) {
				_ = tracker.Register(&resource.AgentRecord{AgentID: "a1", Status: agent.StatusRunning})
			},
		},
		{
			name:     "unknown_is_not_found",
			setup:    func(tracker *resource.Tracker) {},
			wantCode: connect.CodeNotFound,
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := lifecycleTestLogger()
			tracker := resource.NewTracker(10, logger)
			tt.setup(tracker)
			mock := &runAgentMockManager{runResp: &v1.RunAgentResponse{RunId: "r1", Status: "running", ExitCode: -1}}
			svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker, agentMgr: mock}

			_, err := svc.RunAgent(context.Background(), connect.NewRequest(&v1.RunAgentRequest{
				AgentId: "a1",
				Command: "sleep 1",
				Detach:  true,
			}))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want %v", tt.wantCode)
				}
				if got := connect.CodeOf(err); got != tt.wantCode {
					t.Errorf("code = %v, want %v (err: %v)", got, tt.wantCode, err)
				}
				if mock.runCalled {
					t.Error("the stopped/unknown guard must fire BEFORE the manager is called")
				}
			} else if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

// TestRunAgent_StoppedAgentToken asserts the stable token on the stopped path.
func TestRunAgent_StoppedAgentToken(t *testing.T) {
	logger := lifecycleTestLogger()
	tracker := resource.NewTracker(10, logger)
	_ = tracker.Register(&resource.AgentRecord{AgentID: "a1", Status: agent.StatusStopped})
	svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker, agentMgr: &runAgentMockManager{}}

	_, err := svc.RunAgent(context.Background(), connect.NewRequest(&v1.RunAgentRequest{
		AgentId: "a1", Command: "sleep 1", Detach: true,
	}))
	if err == nil {
		t.Fatal("RunAgent against a stopped agent must fail")
	}
	if !strings.Contains(err.Error(), "agent_stopped") {
		t.Errorf("error %q must carry the stable token agent_stopped", err.Error())
	}
}

// recordingHeartbeatManager counts heartbeat calls so the stopped guard can be
// proven to fire before the manager.
type recordingHeartbeatManager struct {
	calls int
}

func (r *recordingHeartbeatManager) Heartbeat(agentID string, ttl time.Duration) (*resource.AgentRecord, error) {
	r.calls++
	return &resource.AgentRecord{AgentID: agentID, Status: agent.StatusRunning, ExpiresAt: time.Now().Add(ttl)}, nil
}

// TestHeartbeatAgent_StoppedAgentRejected pins the third guard: a stopped agent
// cannot extend its TTL (the extension would silently keep it alive until the
// reaper destroys it), while a running agent and an unknown id behave exactly
// as before.
func TestHeartbeatAgent_StoppedAgentRejected(t *testing.T) {
	tests := []struct {
		name     string
		record   *resource.AgentRecord
		wantCode connect.Code
		wantErr  bool
		wantCall bool
	}{
		{
			name:     "stopped_is_failed_precondition",
			record:   &resource.AgentRecord{AgentID: "h1", Status: agent.StatusStopped},
			wantCode: connect.CodeFailedPrecondition,
			wantErr:  true,
		},
		{
			name:     "running_still_heartbeats",
			record:   &resource.AgentRecord{AgentID: "h1", Status: agent.StatusRunning, ExpiresAt: time.Now().Add(time.Hour)},
			wantCall: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := lifecycleTestLogger()
			tracker := resource.NewTracker(10, logger)
			if tt.record != nil {
				if err := tracker.Register(tt.record); err != nil {
					t.Fatalf("register: %v", err)
				}
			}
			hb := &recordingHeartbeatManager{}
			svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker, heartbeats: hb}

			resp, err := svc.HeartbeatAgent(context.Background(), connect.NewRequest(&v1.HeartbeatAgentRequest{AgentId: "h1"}))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want %v", tt.wantCode)
				}
				if got := connect.CodeOf(err); got != tt.wantCode {
					t.Errorf("code = %v, want %v (err: %v)", got, tt.wantCode, err)
				}
				if got := connect.CodeOf(err); got == connect.CodeNotFound {
					t.Error("a stopped agent must NEVER be reported as not_found")
				}
				if !strings.Contains(err.Error(), "agent_stopped") {
					t.Errorf("error %q must carry the stable token agent_stopped", err.Error())
				}
				if hb.calls != 0 {
					t.Errorf("heartbeat manager calls = %d, want 0 (the TTL must not be extended)", hb.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("HeartbeatAgent: %v", err)
			}
			if !resp.Msg.GetAcknowledged() {
				t.Error("running agent heartbeat must be acknowledged")
			}
			if !tt.wantCall || hb.calls != 1 {
				t.Errorf("heartbeat manager calls = %d, want 1", hb.calls)
			}
		})
	}
}

// ── helpers ─────────────────────────────────────────────────────

// startTestBunkerd mounts svc on a real connect handler and returns a client.
func startTestBunkerd(t *testing.T, svc *bunkerdService) (bunkerv1connect.BunkerdClient, func()) {
	t.Helper()
	path, handler := bunkerv1connect.NewBunkerdHandler(svc)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	return bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL), srv.Close
}

// installStubSSHForLifecycle puts a stub `ssh` first on PATH that records its
// argv and prints a line, returning the argv-record path. It mirrors the
// existing exec-image test helper; the file's absence is the proof that no
// command was attempted.
func installStubSSHForLifecycle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "ssh-argv")
	stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + out + "\nprintf 'hello\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return out
}
