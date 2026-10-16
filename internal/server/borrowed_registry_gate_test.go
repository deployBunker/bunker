package server

import (
	"os"
	"strings"
	"testing"
)

// TestServerRun_BorrowedRegistryGatePrecedesStateAndManager is the
// DF-BUNKER-102 ordering invariant as a source-level proof, mirroring
// TestServerRun_ForeignCollisionGatePrecedesStateAndManager: in Run the
// borrowed-registry probe must run before the agent manager exists — opening
// a registry that belongs to the default tree replays ANOTHER daemon's agents
// for this daemon's reconciliation sweep — and before the key/operator stores
// are opened, so a borrowing config cannot consume foreign state either.
func TestServerRun_BorrowedRegistryGatePrecedesStateAndManager(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}

	// Scope the offsets to Run's body so a helper elsewhere in the file can
	// neither satisfy nor break the ordering claim.
	const runHeader = "func (s *BunkerdServer) Run(ctx context.Context) error {"
	runStart := strings.Index(string(src), runHeader)
	if runStart < 0 {
		t.Fatal("Run not found in server.go; this guard can no longer prove ordering")
	}
	body := string(src)[runStart:]

	gate := strings.Index(body, "DetectBorrowedRegistryCollision()")
	if gate < 0 {
		t.Fatal("Run no longer calls DetectBorrowedRegistryCollision — the DF-BUNKER-102 boot gate is gone")
	}
	// The DF-BUNKER-95 gate runs beside it; both must precede the same
	// state-opening steps.
	first := strings.Index(body, "DetectForeignInstanceCollision()")
	if first < 0 || first > gate {
		t.Errorf("DetectBorrowedRegistryCollision at offset %d does not sit immediately beside the DF-BUNKER-95 gate (offset %d)",
			gate, first)
	}
	for _, later := range []string{
		"agent.NewAgentManager(",
		"apikey.NewManagerAt(",
		"operator.NewManagerAt(",
		"ListenAndServe",
	} {
		idx := strings.Index(body, later)
		if idx < 0 {
			t.Fatalf("%s not found in Run; this guard can no longer prove ordering", later)
		}
		if gate > idx {
			t.Errorf("DetectBorrowedRegistryCollision at offset %d runs AFTER %s at %d — a borrowing config could replay foreign state first",
				gate, later, idx)
		}
	}
}
