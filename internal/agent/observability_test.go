package agent

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/registry"
	"github.com/deployBunker/bunker/internal/resource"
)

func TestLifecycleMetricsCaptureDestroyRefusalAndRegistryState(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := registry.Open(registry.Options{Path: t.TempDir() + "/agents.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const id = "metrics-agent"
	if err := store.AppendSpawn(&registry.Record{AgentID: id, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	tracker := resource.NewTracker(4, logger)
	if err := tracker.Register(&resource.AgentRecord{AgentID: id}); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Agent.DestroyArchiveDir = t.TempDir()
	m := &AgentManager{cfg: cfg, logger: logger, tracker: tracker, registry: store}

	m.recordDestroyRefusal(id, StatusLiveProcesses, errors.New("held process"))
	got := m.LifecycleMetrics()
	if got.RefusedLiveProcesses != 1 {
		t.Fatalf("live-process refusal counter = %d, want 1", got.RefusedLiveProcesses)
	}
	if got.RegistryDestroyRefused[StatusLiveProcesses] != 1 {
		t.Fatalf("live-process refusal gauge = %d, want 1", got.RegistryDestroyRefused[StatusLiveProcesses])
	}
	if got.RegistryEvents != 2 {
		t.Fatalf("registry event gauge = %d, want spawn + refusal = 2", got.RegistryEvents)
	}
}
