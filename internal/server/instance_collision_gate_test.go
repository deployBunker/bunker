package server

import (
	"os"
	"strings"
	"testing"
)

// TestServerRun_ForeignCollisionGatePrecedesStateAndManager is the DF-BUNKER-95
// ordering invariant as a source-level proof. In Run the collision probe must
// run before the agent manager exists — agent.NewAgentManager resolves, and
// would ADOPT, <base_data_dir>/instance before it starts the reconciliation
// sweep that destroys that identity's orphans — and before the key/operator
// stores are opened under base_data_dir, so a colliding config cannot write
// anything into the foreign tree either.
func TestServerRun_ForeignCollisionGatePrecedesStateAndManager(t *testing.T) {
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

	gate := strings.Index(body, "DetectForeignInstanceCollision()")
	if gate < 0 {
		t.Fatal("Run no longer calls DetectForeignInstanceCollision — the DF-BUNKER-95 boot gate is gone")
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
			t.Errorf("DetectForeignInstanceCollision at offset %d runs AFTER %s at %d — a colliding config could adopt or open foreign state first",
				gate, later, idx)
		}
	}
}
