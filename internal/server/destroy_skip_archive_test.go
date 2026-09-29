package server

// DF-BUNKER-81 criterion 4, server side: the DestroyAgent handler must turn
// the request's skip_archive field into the manager's archive opt-out option,
// and must pass NO option for a normal destroy (so the daemon-wide policy stays
// in force for every client that does not ask).
//
// The option's own semantics (no archive written, home still deleted, and the
// skip honoured even when the archive dir is unwritable) are pinned in
// internal/agent (TestDestroy_SkipHomeArchiveOption / TestSkipHomeArchiveOption),
// and the CLI -> wire hop in internal/cli
// (TestDestroyCommand_ArchiveChoicePlumbing).

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"connectrpc.com/connect"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

func TestDestroyAgent_SkipArchivePlumbing(t *testing.T) {
	tests := []struct {
		name     string
		req      *v1.DestroyAgentRequest
		wantOpts int
	}{
		{
			name:     "default destroy passes no option (policy unchanged)",
			req:      &v1.DestroyAgentRequest{AgentId: "agent-1"},
			wantOpts: 0,
		},
		{
			name:     "skip_archive asks the manager for the opt-out",
			req:      &v1.DestroyAgentRequest{AgentId: "agent-1", SkipArchive: true},
			wantOpts: 1,
		},
		{
			name:     "skip_archive composes with force",
			req:      &v1.DestroyAgentRequest{AgentId: "agent-1", Force: true, SkipArchive: true},
			wantOpts: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
			mgr := &fakeAgentManager{
				destroyResp: &v1.DestroyAgentResponse{AgentId: tt.req.GetAgentId(), Status: "destroyed"},
			}
			svc := &bunkerdService{
				cfg:      config.DefaultConfig(),
				logger:   logger,
				tracker:  resource.NewTracker(10, logger),
				agentMgr: mgr,
			}

			resp, err := svc.DestroyAgent(context.Background(), connect.NewRequest(tt.req))
			if err != nil {
				t.Fatalf("DestroyAgent() error: %v", err)
			}
			if resp.Msg.GetStatus() != "destroyed" {
				t.Errorf("Status = %q, want destroyed", resp.Msg.GetStatus())
			}
			if !mgr.destroyCalled {
				t.Fatal("DestroyAgent() did not call agentMgr.Destroy")
			}
			if len(mgr.destroyOpts) != tt.wantOpts {
				t.Errorf("manager received %d options, want %d (skip_archive=%v)",
					len(mgr.destroyOpts), tt.wantOpts, tt.req.GetSkipArchive())
			}
			for i, opt := range mgr.destroyOpts {
				if opt == nil {
					t.Errorf("option[%d] is nil: the handler must pass a real option", i)
				}
			}
		})
	}
}
