package main

import (
	"os"
	"strings"
	"testing"
)

// TestRun_ForeignCollisionGatePrecedesSecretWriteAndServe is the DF-BUNKER-95
// ordering invariant for the process entry point, as a source-level proof: in
// run() the collision probe must run after the config is loaded and BEFORE
// EnsureJWTSecret — which can WRITE a generated jwt_secret into whatever tree
// it resolves, and must never write into a foreign daemon's tree — and before
// the server is built and served, where the agent manager resolves and would
// adopt <base_data_dir>/instance.
func TestRun_ForeignCollisionGatePrecedesSecretWriteAndServe(t *testing.T) {
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

	gate := strings.Index(body, "cfg.DetectForeignInstanceCollision()")
	if gate < 0 {
		t.Fatal("run() no longer calls cfg.DetectForeignInstanceCollision — the DF-BUNKER-95 boot gate is gone")
	}

	load := strings.Index(body, "config.Load(cfgPath)")
	if load < 0 {
		t.Fatal("config.Load not found in run(); this guard can no longer prove ordering")
	}
	if gate < load {
		t.Errorf("the collision gate at offset %d runs BEFORE config.Load at %d — it cannot judge a config it has not read", gate, load)
	}

	// The order that matters: a generated secret must never be written into a
	// foreign tree, and the identity file must never be adopted.
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
			t.Errorf("the collision gate at offset %d runs AFTER %s at %d — a colliding config could write into, or adopt, the foreign tree first",
				gate, later, idx)
		}
	}
}
