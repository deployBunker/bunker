package server

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

func TestResidueReapClassifiesOrphanUsers(t *testing.T) {
	mgr := &fakeAgentManager{residue: agent.ResidueInventory{
		OrphanUsers: 2,
		Status:      agent.ResidueStatusOK,
	}}
	resp := residueReapServerInfo(t, mgr)
	if !mgr.residueCalled {
		t.Fatal("ServerInfo did not consult the residue probe")
	}
	if got := resp.GetOrphanUsers(); got != 2 {
		t.Errorf("ServerInfo orphan users = %d, want 2 from the residue probe", got)
	}
}

func TestResidueReapClassifiesOrphanHomes(t *testing.T) {
	mgr := &fakeAgentManager{residue: agent.ResidueInventory{
		OrphanHomes: 3,
		Status:      agent.ResidueStatusOK,
	}}
	resp := residueReapServerInfo(t, mgr)
	if !mgr.residueCalled {
		t.Fatal("ServerInfo did not consult the residue probe")
	}
	if got := resp.GetOrphanHomes(); got != 3 {
		t.Errorf("ServerInfo orphan homes = %d, want 3 from the residue probe", got)
	}
}

// Drive the ServerInfo response through the same agentManager probe seam used
// by production. The scripted inventory is host-independent and safe as an
// unprivileged test user; no real user, home, process table, or network is used.
func residueReapServerInfo(t *testing.T, mgr *fakeAgentManager) *v1.ResidueInventory {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := &bunkerdService{
		cfg:      config.DefaultConfig(),
		logger:   logger,
		tracker:  resource.NewTracker(10, logger),
		agentMgr: mgr,
	}
	resp, err := svc.ServerInfo(context.Background(), connect.NewRequest(&v1.ServerInfoRequest{}))
	if err != nil {
		t.Fatalf("ServerInfo: %v", err)
	}
	if resp.Msg.GetResidue() == nil {
		t.Fatal("ServerInfo returned no residue inventory")
	}
	return resp.Msg.GetResidue()
}
