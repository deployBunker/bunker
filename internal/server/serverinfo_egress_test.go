package server

import (
	"context"
	"os"
	"slices"
	"testing"

	"log/slog"

	"connectrpc.com/connect"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// TestServerInfo_EgressModesSupported (DF-BUNKER-82) pins the server side of
// the egress capability probe: ServerInfo must report the daemon build's
// egress vocabulary, verbatim from the internal/egress list (via the
// internal/config re-export — the netmode ownership rule; the server never
// restates the vocabulary). The field is the ONLY source a spawn CLI can
// consult before sending an enforced --egress-mode, and its empty-means-
// predates convention (proto field 13) means a server that leaves it empty
// on a current build is indistinguishable from a pre-fd974b51 daemon.
func TestServerInfo_EgressModesSupported(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.ServerInfoRequest{})
	resp, err := svc.ServerInfo(context.Background(), req)
	if err != nil {
		t.Fatalf("ServerInfo() error: %v", err)
	}
	got := resp.Msg.GetEgressModesSupported()

	want := config.ValidEgressModes()
	if len(got) != len(want) {
		t.Fatalf("ServerInfo().EgressModesSupported = %v, want the internal/egress vocabulary %v (empty would mean a pre-capability daemon)", got, want)
	}
	for i, m := range want {
		if got[i] != m {
			t.Errorf("ServerInfo().EgressModesSupported[%d] = %q, want %q (order is the vocabulary's declaration order)", i, got[i], m)
		}
	}
	// The vocabulary must actually contain the enforced modes the CLI gate
	// exists to protect — an accidentally-empty re-export would make the
	// server advertise "predates egress" and the CLI refuse every enforced
	// spawn against a perfectly current daemon.
	for _, enforced := range []string{config.EgressModeAllowlist, config.EgressModeNone} {
		if !slices.Contains(got, enforced) {
			t.Errorf("ServerInfo().EgressModesSupported missing enforced mode %q; reported: %v", enforced, got)
		}
	}
}
