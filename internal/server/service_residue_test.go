package server

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// ── DF-BUNKER-21 (AC5): ServerInfo carries the residue inventory ────────────

// TestServerInfoResidueInventory proves the operator surface is WIRED: the
// manager's host probe reaches ServerInfo's wire fields unchanged, including the
// probe status/detail that keep an unreadable plane from reading as "clean".
func TestServerInfoResidueInventory(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)

	mgr := &fakeAgentManager{
		residue: agent.ResidueInventory{
			OrphanUsers: 11,
			OrphanHomes: 3,
			OrphanKeys:  2,
			StaleLinger: 4,
			Registered:  0,
			Status:      agent.ResidueStatusPartial,
			Detail:      "keys: open /etc/bunkerd/ssh: permission denied",
		},
	}
	svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker, agentMgr: mgr}

	resp, err := svc.ServerInfo(context.Background(), connect.NewRequest(&v1.ServerInfoRequest{}))
	if err != nil {
		t.Fatalf("ServerInfo() error: %v", err)
	}
	if !mgr.residueCalled {
		t.Fatal("ServerInfo did not consult the manager's residue inventory")
	}
	res := resp.Msg.GetResidue()
	if res == nil {
		t.Fatal("ServerInfo().Residue is nil although the manager reported an inventory")
	}
	if res.GetOrphanUsers() != 11 || res.GetOrphanHomes() != 3 || res.GetOrphanKeys() != 2 || res.GetStaleLingerEntries() != 4 {
		t.Errorf("residue counts were not carried through: %+v", res)
	}
	if res.GetRegisteredAgents() != 0 {
		t.Errorf("RegisteredAgents = %d, want 0 (the QA-BUNKER-19 fingerprint: residue with nothing registered)", res.GetRegisteredAgents())
	}
	if res.GetStatus() != agent.ResidueStatusPartial {
		t.Errorf("Status = %q, want %q", res.GetStatus(), agent.ResidueStatusPartial)
	}
	if !strings.Contains(res.GetDetail(), "permission denied") {
		t.Errorf("Detail = %q, want the probe failure reason", res.GetDetail())
	}
}

// A service without a manager must NOT report a zero inventory: an absent
// message is how a CLI tells "the daemon cannot see residue" from "the host is
// clean".
func TestServerInfoResidueAbsentWithoutManager(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: resource.NewTracker(10, logger)}

	resp, err := svc.ServerInfo(context.Background(), connect.NewRequest(&v1.ServerInfoRequest{}))
	if err != nil {
		t.Fatalf("ServerInfo() error: %v", err)
	}
	if resp.Msg.GetResidue() != nil {
		t.Errorf("ServerInfo().Residue = %+v, want nil when no manager can probe the host", resp.Msg.GetResidue())
	}
}

// A negative probe count (a future probe that reports "unknown" as -1) must be
// clamped, never wrapped into ~4 billion residue items by the uint32 conversion.
func TestServerInfoResidueClampsNegativeCounts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	mgr := &fakeAgentManager{
		residue: agent.ResidueInventory{
			OrphanUsers: -1, OrphanHomes: -1, OrphanKeys: -1, StaleLinger: -1, Registered: -1,
			Status: agent.ResidueStatusUnavailable,
			Detail: "users: probe unavailable",
		},
	}
	svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: resource.NewTracker(10, logger), agentMgr: mgr}

	resp, err := svc.ServerInfo(context.Background(), connect.NewRequest(&v1.ServerInfoRequest{}))
	if err != nil {
		t.Fatalf("ServerInfo() error: %v", err)
	}
	res := resp.Msg.GetResidue()
	if res == nil {
		t.Fatal("ServerInfo().Residue is nil")
	}
	if res.GetOrphanUsers() != 0 || res.GetOrphanHomes() != 0 || res.GetOrphanKeys() != 0 || res.GetStaleLingerEntries() != 0 {
		t.Errorf("negative counts were not clamped to zero: %+v", res)
	}
	if res.GetStatus() != agent.ResidueStatusUnavailable {
		t.Errorf("Status = %q, want %q (an unavailable probe must stay visible)", res.GetStatus(), agent.ResidueStatusUnavailable)
	}
}
