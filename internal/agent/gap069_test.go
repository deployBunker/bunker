package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/resource"
)

// gap069ImageRef is a realistic per-agent image-spec ref (imagespec.ImageRef
// format: "bunkerd-imagespec-<key[:12]>:latest").
const gap069ImageRef = "bunkerd-imagespec-0123456789ab:latest"

// TestRegistryRecordRoundTripsImage proves the tracker record ⇄ durable
// registry record conversion carries the image-spec ref in both directions
// (GAP-069). Without it a replayed or adopted agent would exec in the bare
// host user context while its customized image sat unused.
func TestRegistryRecordRoundTripsImage(t *testing.T) {
	rec := &resource.AgentRecord{
		AgentID: "img-agent",
		Status:  "running",
		Image:   gap069ImageRef,
	}
	dur := recordToRegistry(rec)
	if dur == nil || dur.Image != gap069ImageRef {
		t.Fatalf("recordToRegistry dropped the image ref: %+v", dur)
	}
	back := registryToRecord(dur)
	if back == nil || back.Image != gap069ImageRef {
		t.Fatalf("registryToRecord dropped the image ref: %+v", back)
	}

	plain := registryToRecord(recordToRegistry(&resource.AgentRecord{AgentID: "plain"}))
	if plain.Image != "" {
		t.Errorf("plain agent gained an image ref: %q", plain.Image)
	}
}

// TestReconcile_RestoresImageRef proves a relaunch puts the image ref back on
// the LIVE tracker record: reconciliation restores from the durable registry,
// and ExecAgent reads the tracker record — so a restart must not lose the
// container context.
func TestReconcile_RestoresImageRef(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")

	first := newRegistryManager(t, path)
	rec := &resource.AgentRecord{
		AgentID:           "img-restore",
		Status:            "running",
		CreatedAt:         time.Now().Add(-time.Hour),
		ExpiresAt:         time.Now().Add(5 * time.Hour),
		PortRangeStart:    10000,
		PortRangeEnd:      10099,
		SshPrivateKeyPath: "/etc/bunkerd/ssh/img-restore",
		Image:             gap069ImageRef,
	}
	if err := first.persistSpawn(rec); err != nil {
		t.Fatalf("persistSpawn: %v", err)
	}
	if err := first.registry.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Relaunch: a brand-new manager replays the same file.
	second := newRegistryManager(t, path)
	defer second.registry.Close()
	second.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: "img-restore", Username: "bunker-img-restore", Home: "/home/bunker-img-restore"}}, nil
	}

	rep := second.Reconcile(context.Background())
	if rep.Restored != 1 {
		t.Fatalf("reconcile report = %+v, want 1 restored", rep)
	}
	tracked := second.tracker.Get("img-restore")
	if tracked == nil {
		t.Fatal("replayed agent was not restored into the tracker")
	}
	if tracked.Image != gap069ImageRef {
		t.Errorf("restored tracker record image = %q, want %q (exec would fall back to the host context)",
			tracked.Image, gap069ImageRef)
	}
}
