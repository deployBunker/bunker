package server

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"testing"

	"connectrpc.com/connect"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/netmode"
	"github.com/deployBunker/bunker/internal/resource"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// TestServerInfo_NetworkModesSupported (INT-CI-020) pins the server side of the
// network-isolation capability probe: ServerInfo must report the daemon build's
// ENFORCED network-isolation vocabulary, verbatim from internal/netmode (the
// ownership rule — the server re-exports, it never restates the list).
//
// This field is the only thing a spawn CLI can consult BEFORE creating an
// agent, and its empty-means-predates convention (proto field 14) means a
// server that leaves it empty on a current build is indistinguishable from a
// 0.1.x daemon that silently drops --network-mode — the exact false cut
// INT-CI-020 exists for.
func TestServerInfo_NetworkModesSupported(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	resp, err := svc.ServerInfo(context.Background(), connect.NewRequest(&v1.ServerInfoRequest{}))
	if err != nil {
		t.Fatalf("ServerInfo() error: %v", err)
	}
	got := resp.Msg.GetNetworkModesSupported()

	want := netmode.ValidModes()
	if len(got) != len(want) {
		t.Fatalf("ServerInfo().NetworkModesSupported = %v, want the internal/netmode vocabulary %v (empty would mean a pre-capability daemon and the CLI would refuse every enforced spawn)", got, want)
	}
	for i, m := range want {
		if got[i] != m {
			t.Errorf("ServerInfo().NetworkModesSupported[%d] = %q, want %q (order is the vocabulary's declaration order)", i, got[i], m)
		}
	}
	// The vocabulary must actually contain the mode the spawn gate protects. An
	// accidentally-empty re-export would advertise "predates the surface" and
	// make the CLI refuse a perfectly current daemon.
	if !slices.Contains(got, netmode.ModeSystemd) {
		t.Errorf("ServerInfo().NetworkModesSupported missing the enforced mode %q; reported: %v", netmode.ModeSystemd, got)
	}
}
