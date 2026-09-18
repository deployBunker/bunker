package audit

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	"github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"

	"github.com/deployBunker/bunker/internal/auth"
)

// This file pins the GAP-071 audit claim: the interceptor sits in front of
// every RPC, so the three new lifecycle methods are recorded with no second
// audit path — and each record carries the TARGET agent id the request named.

// errStoppedStub stands in for the stopped-agent sentinel on the wire.
var errStoppedStub = errors.New("agent_stopped: agent is stopped")

// TestInterceptorRecordsLifecycleRPCs drives the real connect handler (so Spec
// and Peer are populated exactly as in production) for StopAgent, StartAgent
// and RestartAgent and asserts one well-formed audit record per call, naming
// the procedure and the target agent.
func TestInterceptorRecordsLifecycleRPCs(t *testing.T) {
	const (
		stopProcedure    = "/bunker.v1.Bunkerd/StopAgent"
		startProcedure   = "/bunker.v1.Bunkerd/StartAgent"
		restartProcedure = "/bunker.v1.Bunkerd/RestartAgent"
	)

	tests := []struct {
		name      string
		procedure string
		wantAgent string
		// serve mounts this procedure with the audit interceptor and returns
		// the live server.
		serve func(t *testing.T, l *AuditLog) *httptest.Server
		// call performs the matching client RPC.
		call func(t *testing.T, client bunkerv1connect.BunkerdClient)
	}{
		{
			name:      "stop",
			procedure: stopProcedure,
			wantAgent: "stop-agent",
			serve: func(t *testing.T, l *AuditLog) *httptest.Server {
				t.Helper()
				return serveUnary(t, l, &auth.Claims{}, stopProcedure,
					func(ctx context.Context, req *connect.Request[v1.StopAgentRequest]) (*connect.Response[v1.StopAgentResponse], error) {
						return connect.NewResponse(&v1.StopAgentResponse{AgentId: req.Msg.GetAgentId(), Status: "stopped"}), nil
					})
			},
			call: func(t *testing.T, client bunkerv1connect.BunkerdClient) {
				t.Helper()
				resp, err := client.StopAgent(context.Background(), connect.NewRequest(&v1.StopAgentRequest{AgentId: "stop-agent"}))
				if err != nil {
					t.Fatalf("StopAgent: %v", err)
				}
				if got := resp.Msg.GetStatus(); got != "stopped" {
					t.Errorf("status = %q, want stopped", got)
				}
			},
		},
		{
			name:      "start",
			procedure: startProcedure,
			wantAgent: "start-agent",
			serve: func(t *testing.T, l *AuditLog) *httptest.Server {
				t.Helper()
				return serveUnary(t, l, &auth.Claims{}, startProcedure,
					func(ctx context.Context, req *connect.Request[v1.StartAgentRequest]) (*connect.Response[v1.StartAgentResponse], error) {
						return connect.NewResponse(&v1.StartAgentResponse{AgentId: req.Msg.GetAgentId(), Status: "started"}), nil
					})
			},
			call: func(t *testing.T, client bunkerv1connect.BunkerdClient) {
				t.Helper()
				resp, err := client.StartAgent(context.Background(), connect.NewRequest(&v1.StartAgentRequest{AgentId: "start-agent"}))
				if err != nil {
					t.Fatalf("StartAgent: %v", err)
				}
				if got := resp.Msg.GetStatus(); got != "started" {
					t.Errorf("status = %q, want started", got)
				}
			},
		},
		{
			name:      "restart",
			procedure: restartProcedure,
			wantAgent: "restart-agent",
			serve: func(t *testing.T, l *AuditLog) *httptest.Server {
				t.Helper()
				return serveUnary(t, l, &auth.Claims{}, restartProcedure,
					func(ctx context.Context, req *connect.Request[v1.RestartAgentRequest]) (*connect.Response[v1.RestartAgentResponse], error) {
						return connect.NewResponse(&v1.RestartAgentResponse{
							AgentId:   req.Msg.GetAgentId(),
							Status:    "restarted",
							ExpiresAt: "2026-09-18T06:00:00-05:00",
						}), nil
					})
			},
			call: func(t *testing.T, client bunkerv1connect.BunkerdClient) {
				t.Helper()
				resp, err := client.RestartAgent(context.Background(), connect.NewRequest(&v1.RestartAgentRequest{AgentId: "restart-agent"}))
				if err != nil {
					t.Fatalf("RestartAgent: %v", err)
				}
				if got := resp.Msg.GetStatus(); got != "restarted" {
					t.Errorf("status = %q, want restarted", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, path := newTestLog(t)
			srv := tt.serve(t, l)

			client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
			tt.call(t, client)

			records := parseRecords(t, readLog(t, path))
			if len(records) != 1 {
				t.Fatalf("expected exactly 1 audit record, got %d", len(records))
			}
			rec := records[0]
			if rec["method"] != tt.procedure {
				t.Errorf("method = %v, want %v", rec["method"], tt.procedure)
			}
			if rec["outcome"] != "ok" {
				t.Errorf("outcome = %v, want ok", rec["outcome"])
			}
			if rec["agent_id"] != tt.wantAgent {
				t.Errorf("agent_id = %v, want %v", rec["agent_id"], tt.wantAgent)
			}
			for _, k := range requiredFields {
				if _, ok := rec[k]; !ok {
					t.Errorf("record missing required field %q: %v", k, rec)
				}
			}
		})
	}
}

// TestInterceptorRecordsLifecycleFailure pins the outcome side for a lifecycle
// call: a FailedPrecondition (the stopped-agent path) is recorded as an error
// with the code, not as ok — and it still names the target agent.
func TestInterceptorRecordsLifecycleFailure(t *testing.T) {
	l, path := newTestLog(t)
	srv := serveUnary(t, l, &auth.Claims{}, "/bunker.v1.Bunkerd/StopAgent",
		func(ctx context.Context, req *connect.Request[v1.StopAgentRequest]) (*connect.Response[v1.StopAgentResponse], error) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errStoppedStub)
		})
	client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)

	if _, err := client.StopAgent(context.Background(), connect.NewRequest(&v1.StopAgentRequest{AgentId: "stopped-agent"})); err == nil {
		t.Fatal("expected the RPC to fail")
	}

	records := parseRecords(t, readLog(t, path))
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if got := records[0]["outcome"]; got != "failed_precondition" {
		t.Errorf("outcome = %v, want failed_precondition", got)
	}
	if got := records[0]["agent_id"]; got != "stopped-agent" {
		t.Errorf("agent_id = %v, want stopped-agent (errors are audited too)", got)
	}
}
