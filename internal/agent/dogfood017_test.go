package agent

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/imagespec"
	"github.com/deployBunker/bunker/internal/registry"
	"github.com/deployBunker/bunker/internal/resource"
)

// TestSpawnImageToolCountRecordedAtSpawn pins the tools-readiness datum the
// daemon stamps on a spawn (DOGFOOD-017): a plain spawn (no spec) records a
// DECIDED 0 — no image, so no registry tool — while a spec that carries the
// delivered set records 2. Neither case may record nil: nil means "this record
// predates the field", which is a different statement from "this spawn built no
// image", and the CLI renders them differently.
func TestSpawnImageToolCountRecordedAtSpawn(t *testing.T) {
	if got := spawnImageToolCount(nil); got == nil {
		t.Fatal("spawnImageToolCount(nil) = nil; a plain spawn must record a decided 0, not unknown")
	} else if *got != 0 {
		t.Errorf("spawnImageToolCount(nil) = %d, want 0", *got)
	}

	full := spawnImageToolCount(imagespec.AgentToolSpecSpec())
	if full == nil {
		t.Fatal("spawnImageToolCount(delivered set) = nil; the spec pins a count")
	}
	if want := uint32(imagespec.DeliveredSetSize()); *full != want {
		t.Errorf("spawnImageToolCount(delivered set) = %d, want %d", *full, want)
	}

	// A spec with no registry tool must not be promoted to "ready" just
	// because an image was built at all.
	noTools := &imagespec.Spec{Packages: []imagespec.PackageAdd{
		{Manager: imagespec.ManagerAPT, Packages: []string{"jq"}},
	}}
	if got := spawnImageToolCount(noTools); got == nil || *got != 0 {
		t.Errorf("spawnImageToolCount(non-tool spec) = %v, want a pointer to 0", got)
	}
}

// TestSpawnImageToolCountSurvivesDurablePlane proves the datum reaches the
// place `bunker info` reads it from: the durable registry record. A count that
// dropped on the record→registry→record hop would make every real agent render
// "unknown" while the unit test above stayed green.
func TestSpawnImageToolCountSurvivesDurablePlane(t *testing.T) {
	count := uint32(2)
	rec := &resource.AgentRecord{
		AgentID:             "dogfood017-rt",
		Status:              "running",
		SpawnImageToolCount: &count,
	}
	durable := recordToRegistry(rec)
	if durable.SpawnImageToolCount == nil || *durable.SpawnImageToolCount != count {
		t.Fatalf("recordToRegistry dropped the spawn tool count: %+v", durable)
	}
	back := registryToRecord(durable)
	if back.SpawnImageToolCount == nil || *back.SpawnImageToolCount != count {
		t.Fatalf("registryToRecord dropped the spawn tool count: %+v", back)
	}

	// Absence must survive too: a pre-field record (nil) must come back nil so
	// the CLI keeps saying "unknown" instead of upgrading it to 0.
	preField := registryToRecord(&registry.Record{AgentID: "dogfood017-prefield", Status: "running"})
	if preField.SpawnImageToolCount != nil {
		t.Errorf("registryToRecord invented a count for a pre-field record: %d", *preField.SpawnImageToolCount)
	}
}

// TestSpawnImageToolCountSurvivesJSON proves the field survives the actual
// storage format, not just the struct hop: the registry persists records as
// JSON, and omitempty on a *uint32 must not drop a RECORDED zero (a plain
// spawn) while dropping an UNRECORDED value (nil).
func TestLiveToolsObservationSurvivesDurableRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	store, err := registry.Open(registry.Options{Path: path})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	count := uint32(2)
	ready := false
	if err := store.AppendSpawn(&registry.Record{AgentID: "dogfood017-live", Status: "running"}); err != nil {
		t.Fatalf("append spawn: %v", err)
	}
	observedAt := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	if err := store.AppendToolsObservation("dogfood017-live", &count, &ready, observedAt, "in-agent command -v via bunker info"); err != nil {
		t.Fatalf("append tools observation: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first registry: %v", err)
	}

	reopened, err := registry.Open(registry.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen registry: %v", err)
	}
	defer reopened.Close()
	rec := reopened.Get("dogfood017-live")
	if rec == nil || rec.ObservedToolCount == nil || *rec.ObservedToolCount != count {
		t.Fatalf("replay lost observed count: %+v", rec)
	}
	if rec.ToolsReady == nil || *rec.ToolsReady != ready || rec.ToolsObservedAt != observedAt.Format(time.RFC3339) {
		t.Fatalf("replay lost live tools observation: %+v", rec)
	}
	if rec.ToolsObservationSource != "in-agent command -v via bunker info" {
		t.Fatalf("replay lost observation source: %q", rec.ToolsObservationSource)
	}
}

func TestSpawnImageToolCountSurvivesJSON(t *testing.T) {
	zero, two := uint32(0), uint32(2)
	for _, tc := range []struct {
		name string
		in   *uint32
		want bool // present after decode
	}{
		{"recorded zero", &zero, true},
		{"recorded two", &two, true},
		{"pre-field nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(&registry.Record{AgentID: "dogfood017-json", SpawnImageToolCount: tc.in})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back registry.Record
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := back.SpawnImageToolCount != nil; got != tc.want {
				t.Fatalf("presence after JSON round trip = %v, want %v (json: %s)", got, tc.want, raw)
			}
			if tc.want && *back.SpawnImageToolCount != *tc.in {
				t.Errorf("count after JSON round trip = %d, want %d", *back.SpawnImageToolCount, *tc.in)
			}
		})
	}
}
