package main

import (
	"os"
	"strings"
	"testing"
)

// TestRun_BorrowedRegistryGatePrecedesSecretWriteAndServe is the DF-BUNKER-102
// ordering invariant for the process entry point, as a source-level proof,
// mirroring TestRun_ForeignCollisionGatePrecedesSecretWriteAndServe: in run()
// the borrowed-registry probe must run after the config is loaded and BEFORE
// EnsureJWTSecret — which can WRITE a generated jwt_secret into whatever tree
// it resolves, and a borrowing config must fail before touching any state —
// and before the server is built and served, where the registry would be
// replayed and the agent manager would reconcile foreign agents.
func TestRun_BorrowedRegistryGatePrecedesSecretWriteAndServe(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	const runHeader = "func run() error {"
	runStart := strings.Index(string(src), runHeader)
	if runStart < 0 {
		t.Fatal("run() not found in main.go; this guard can no longer prove ordering")
	}
	body := string(src)[runStart:]

	gate := strings.Index(body, "cfg.DetectBorrowedRegistryCollision()")
	if gate < 0 {
		t.Fatal("run() no longer calls cfg.DetectBorrowedRegistryCollision — the DF-BUNKER-102 boot gate is gone")
	}

	load := strings.Index(body, "config.Load(cfgPath)")
	if load < 0 {
		t.Fatal("config.Load not found in run(); this guard can no longer prove ordering")
	}
	if gate < load {
		t.Errorf("the borrowed-registry gate at offset %d runs BEFORE config.Load at %d — it cannot judge a config it has not read", gate, load)
	}

	// The order that matters: a generated secret must never be written before
	// the borrow is refused, and the registry/identity must never be adopted.
	for _, later := range []string{
		"cfg.EnsureJWTSecret()",
		"server.New(cfg)",
		"srv.Run(ctx)",
	} {
		idx := strings.Index(body, later)
		if idx < 0 {
			t.Fatalf("%s not found in run(); this guard can no longer prove ordering", later)
		}
		if gate > idx {
			t.Errorf("the borrowed-registry gate at offset %d runs AFTER %s at %d — a borrowing config could write into, or replay, the foreign registry first",
				gate, later, idx)
		}
	}
}
