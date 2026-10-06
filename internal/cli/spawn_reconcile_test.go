package cli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── QA-BUNKER-58: spawn must not report failure while the daemon may still ──
// be materializing the agent. A client deadline (or a DERP-relay connection
// drop) can expire while the daemon's rootless-install keeps running for
// another 20-42s; the old CLI printed "spawn failed" and the caller's cleanup
// trap then DESTROYED a healthy agent. The fix: on deadline/unavailable/
// transport-class SpawnAgent errors the CLI re-lists agents (bounded poll
// window) and treats an appearing agent as SUCCESS, flowing into the same
// bundle output as a clean RPC return. Refusal-class errors (invalid
// argument, unauthenticated, not found, failed precondition) and any error
// where the server already reported a rollback fail fast WITHOUT probing.

// mockReconcileServer extends mockSpawnServer with a ListAgents stub the
// reconcile tests script per call. call is 1-based.
type mockReconcileServer struct {
	mockSpawnServer
	mu      sync.Mutex
	listFn  func(call int) (*v1.ListAgentsResponse, error)
	listLog []*v1.ListAgentsRequest
}

func (m *mockReconcileServer) ListAgents(
	ctx context.Context,
	req *connect.Request[v1.ListAgentsRequest],
) (*connect.Response[v1.ListAgentsResponse], error) {
	m.mu.Lock()
	m.listLog = append(m.listLog, req.Msg)
	call := len(m.listLog)
	fn := m.listFn
	m.mu.Unlock()
	if fn == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, nil)
	}
	resp, err := fn(call)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (m *mockReconcileServer) listCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.listLog)
}

func (m *mockReconcileServer) sawStatusFilter() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.listLog) == 0 {
		return "", false
	}
	return m.listLog[0].GetStatusFilter(), true
}

// speedUpReconcilePoll shrinks the inter-poll backoff for the duration of one
// test so a full never-found window (5 polls) costs milliseconds instead of
// the production 3s interval. The production shape is pinned by
// TestSpawnReconcileProductionWindowShape.
func speedUpReconcilePoll(t *testing.T) {
	t.Helper()
	orig := spawnReconcileInterval
	spawnReconcileInterval = time.Millisecond
	t.Cleanup(func() { spawnReconcileInterval = orig })
}

// runReconcileSpawn boots a mock server + config and executes `bunker spawn`
// with the given args. Returns (combined stdout, exec error).
func runReconcileSpawn(t *testing.T, mock *mockReconcileServer, args ...string) (string, error) {
	t.Helper()
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()
	// Isolate HOME BEFORE the first config write (qabunker23 tripwire:
	// SaveCLIConfig inside writeSpawnTestConfig must land in this test's
	// own TempDir, never an ambient/scratch home).
	t.Setenv("HOME", t.TempDir())
	writeSpawnTestConfig(t, t.TempDir(), srv.URL)

	cmd := NewSpawnCommand()
	cmd.SetArgs(args)
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	return out, err
}

func TestSpawnReconcileProductionWindowShape(t *testing.T) {
	// Criterion 2: the probe polls with backoff over >=3 attempts. The
	// production constants must keep that shape even though tests shrink
	// the interval locally.
	if spawnReconcileAttempts < 3 {
		t.Errorf("spawnReconcileAttempts = %d, want >= 3", spawnReconcileAttempts)
	}
	if spawnReconcileInterval != 3*time.Second {
		t.Errorf("production spawnReconcileInterval = %v, want 3s", spawnReconcileInterval)
	}
}

func TestSpawnCommand_ReconcileOnLostResponse(t *testing.T) {
	tests := []struct {
		name string
		// spawnErr is what the daemon answers to SpawnAgent.
		spawnErr error
		// listFn scripts ListAgents per 1-based call.
		listFn func(call int) (*v1.ListAgentsResponse, error)
		// args beyond the implicit --server default.
		args []string
		// wantSuccessID: when set, the command must SUCCEED and print the
		// same bundle a clean spawn would (Agent created: <id>).
		wantSuccessID string
		// wantErr substrings when the command must fail (original RPC error
		// AND the not-found-on-re-list fact, per criterion 3).
		wantErr []string
		// bounds on ListAgents calls (default 0..1000 when unset).
		wantListCallsMin int
		wantListCallsMax int
		// wantReconcileLine: the "Reconciling" progress line must appear.
		wantReconcileLine bool
	}{
		{
			name:     "deadline_exceeded_agent_appears_on_later_poll_reports_success",
			spawnErr: connect.NewError(connect.CodeDeadlineExceeded, errors.New("deadline exceeded")),
			listFn: func(call int) (*v1.ListAgentsResponse, error) {
				if call < 2 {
					return &v1.ListAgentsResponse{}, nil
				}
				return &v1.ListAgentsResponse{Agents: []*v1.AgentSummary{
					{AgentId: "recon-found", Status: "running"},
				}}, nil
			},
			args:              []string{"--server", "default", "recon-found"},
			wantSuccessID:     "recon-found",
			wantListCallsMin:  2,
			wantReconcileLine: true,
		},
		{
			name:     "deadline_exceeded_agent_never_appears_names_both_facts",
			spawnErr: connect.NewError(connect.CodeDeadlineExceeded, errors.New("deadline exceeded")),
			listFn: func(int) (*v1.ListAgentsResponse, error) {
				return &v1.ListAgentsResponse{}, nil
			},
			args:              []string{"--server", "default", "recon-ghost"},
			wantErr:           []string{"spawn agent", "deadline", "NOT found on re-list", "recon-ghost"},
			wantListCallsMin:  spawnReconcileAttempts,
			wantListCallsMax:  spawnReconcileAttempts,
			wantReconcileLine: true,
		},
		{
			name:     "invalid_argument_refuses_without_probing",
			spawnErr: connect.NewError(connect.CodeInvalidArgument, errors.New(`invalid ttl "6x": bad unit`)),
			args:     []string{"--server", "default", "recon-bad"},
			wantErr:  []string{"spawn agent", `invalid ttl "6x"`},
		},
		{
			name:     "unauthenticated_refuses_without_probing",
			spawnErr: connect.NewError(connect.CodeUnauthenticated, errors.New("missing Authorization header")),
			args:     []string{"--server", "default", "recon-auth"},
			wantErr:  []string{"spawn agent", "missing Authorization header"},
		},
		{
			name:     "failed_precondition_refuses_without_probing",
			spawnErr: connect.NewError(connect.CodeFailedPrecondition, errors.New("daemon is not installed")),
			args:     []string{"--server", "default", "recon-fp"},
			wantErr:  []string{"spawn agent", "daemon is not installed"},
		},
		{
			name:     "not_found_refuses_without_probing",
			spawnErr: connect.NewError(connect.CodeNotFound, errors.New("no such route")),
			args:     []string{"--server", "default", "recon-nf"},
			wantErr:  []string{"spawn agent", "no such route"},
		},
		{
			name:     "server_reported_rollback_refuses_without_probing",
			spawnErr: connect.NewError(connect.CodeInternal, errors.New("spawn recon-rb failed at stage docker: user bunker-recon-rb was rolled back (userdel)")),
			args:     []string{"--server", "default", "recon-rb"},
			wantErr:  []string{"spawn agent", "rolled back"},
		},
		{
			name:     "unavailable_agent_appears_on_first_poll_reports_success",
			spawnErr: connect.NewError(connect.CodeUnavailable, errors.New("connection refused")),
			listFn: func(int) (*v1.ListAgentsResponse, error) {
				return &v1.ListAgentsResponse{Agents: []*v1.AgentSummary{
					{AgentId: "recon-unavail", Status: "starting"},
				}}, nil
			},
			args:             []string{"--server", "default", "recon-unavail"},
			wantSuccessID:    "recon-unavail",
			wantListCallsMin: 1,
		},
		{
			name:     "agent_appears_in_failed_state_is_not_reported_as_success",
			spawnErr: connect.NewError(connect.CodeDeadlineExceeded, errors.New("deadline exceeded")),
			listFn: func(int) (*v1.ListAgentsResponse, error) {
				return &v1.ListAgentsResponse{Agents: []*v1.AgentSummary{
					{AgentId: "recon-dead", Status: "failed"},
				}}, nil
			},
			args:              []string{"--server", "default", "recon-dead"},
			wantErr:           []string{"spawn agent", "did not complete successfully", "recon-dead"},
			wantListCallsMin:  1,
			wantReconcileLine: true,
		},
		{
			name:     "auto_id_agent_created_after_spawn_start_is_reconciled",
			spawnErr: connect.NewError(connect.CodeDeadlineExceeded, errors.New("deadline exceeded")),
			listFn: func(int) (*v1.ListAgentsResponse, error) {
				return &v1.ListAgentsResponse{Agents: []*v1.AgentSummary{
					{
						AgentId:   "auto8c95",
						Status:    "running",
						CreatedAt: time.Now().UTC().Format(time.RFC3339),
					},
				}}, nil
			},
			// No positional / --agent-id: the daemon auto-generates the id,
			// so the probe must attribute by creation time.
			args:             []string{"--server", "default"},
			wantSuccessID:    "auto8c95",
			wantListCallsMin: 1,
		},
		{
			name:     "auto_id_old_agent_is_not_mistaken_for_this_spawn",
			spawnErr: connect.NewError(connect.CodeDeadlineExceeded, errors.New("deadline exceeded")),
			listFn: func(int) (*v1.ListAgentsResponse, error) {
				return &v1.ListAgentsResponse{Agents: []*v1.AgentSummary{
					{
						AgentId:   "stale-old-agent",
						Status:    "running",
						CreatedAt: time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
					},
				}}, nil
			},
			args:              []string{"--server", "default"},
			wantErr:           []string{"spawn agent", "NOT found on re-list"},
			wantListCallsMin:  spawnReconcileAttempts,
			wantListCallsMax:  spawnReconcileAttempts,
			wantReconcileLine: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			speedUpReconcilePoll(t)

			mock := &mockReconcileServer{
				mockSpawnServer: mockSpawnServer{
					mockBunkerdServer: mockBunkerdServer{
						info: &v1.ServerInfoResponse{
							Hostname: "bunker-reconcile",
							Version:  "v0.2.0",
						},
					},
					spawnErr: tt.spawnErr,
				},
				listFn: tt.listFn,
			}

			out, err := runReconcileSpawn(t, mock, tt.args...)
			calls := mock.listCalls()

			if tt.wantSuccessID != "" {
				if err != nil {
					t.Fatalf("spawn should have been reconciled to success, got error: %v\nstdout:\n%s", err, out)
				}
				if !strings.Contains(out, "Agent created: "+tt.wantSuccessID) {
					t.Errorf("reconciled spawn output missing %q; must use the SAME bundle output path as a clean spawn, got:\n%s", "Agent created: "+tt.wantSuccessID, out)
				}
				// Criterion 2: the success block must not be duplicated —
				// exactly one bundle header even after reconciliation.
				if n := strings.Count(out, "Connection Bundle"); n != 1 {
					t.Errorf("output carries %d Connection Bundle headers, want exactly 1:\n%s", n, out)
				}
			}
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("expected error containing %v, got success. stdout:\n%s", tt.wantErr, out)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err.Error(), want)
					}
				}
			}
			// Rows that set min but no max accept any count >= min; rows
			// with NEITHER bound (the refusal rows) demand EXACTLY zero
			// ListAgents calls — criterion 3's counter assertion that a
			// hard refusal never probes.
			if tt.wantListCallsMax == 0 && tt.wantListCallsMin > 0 {
				tt.wantListCallsMax = 1000
			}
			if calls < tt.wantListCallsMin || calls > tt.wantListCallsMax {
				t.Errorf("ListAgents called %d times, want within [%d, %d]", calls, tt.wantListCallsMin, tt.wantListCallsMax)
			}
			if tt.wantReconcileLine && !strings.Contains(out, "Reconciling") {
				t.Errorf("output missing the Reconciling progress line, got:\n%s", out)
			}
			if len(tt.wantErr) > 0 && !tt.wantReconcileLine && strings.Contains(out, "Reconciling") {
				t.Errorf("output has a Reconciling line but the error class must not probe, got:\n%s", out)
			}
		})
	}
}

// TestSpawnCommand_ReconcileProbeUsesFullList pins the probe's list request
// shape: status filter "all" so a starting/pending agent is visible (the
// default `bunker list` filter is "running", which a mid-install agent is
// not yet).
func TestSpawnCommand_ReconcileProbeUsesFullList(t *testing.T) {
	speedUpReconcilePoll(t)

	mock := &mockReconcileServer{
		mockSpawnServer: mockSpawnServer{
			mockBunkerdServer: mockBunkerdServer{info: &v1.ServerInfoResponse{Hostname: "h", Version: "v"}},
			spawnErr:          connect.NewError(connect.CodeUnavailable, errors.New("transport closing")),
		},
		listFn: func(int) (*v1.ListAgentsResponse, error) {
			return &v1.ListAgentsResponse{Agents: []*v1.AgentSummary{
				{AgentId: "recon-filter", Status: "starting"},
			}}, nil
		},
	}

	if _, err := runReconcileSpawn(t, mock, "--server", "default", "recon-filter"); err != nil {
		t.Fatalf("reconciled spawn failed: %v", err)
	}
	got, ok := mock.sawStatusFilter()
	if !ok {
		t.Fatal("probe never listed agents")
	}
	if got != "all" {
		t.Errorf("probe listed with status filter %q, want %q (a mid-install agent is not running yet)", got, "all")
	}
}
