package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gap069ImageRef is a realistic per-agent image-spec ref (see
// imagespec.ImageRef: "bunkerd-imagespec-<key[:12]>:latest").
const gap069ImageRef = "bunkerd-imagespec-0123456789ab:latest"

// TestSpawnEventCarriesImageRef proves the durable registry persists the
// image-spec ref (GAP-069): the spawn line carries it on the wire and a
// relaunch replays it, so an agent that survives a daemon restart keeps the
// container context its spawn established.
func TestSpawnEventCarriesImageRef(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	s := testStore(t, Options{Path: path})

	rec := spawnRecord("img-agent")
	rec.Image = gap069ImageRef
	if err := s.AppendSpawn(rec); err != nil {
		t.Fatalf("AppendSpawn: %v", err)
	}

	// Wire form: the field must be present under the documented key.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &ev); err != nil {
		t.Fatalf("event line is not JSON: %v", err)
	}
	if got, _ := ev["image"].(string); got != gap069ImageRef {
		t.Errorf("spawn event image = %q, want %q (line: %v)", got, gap069ImageRef, ev)
	}

	// Replay form: a fresh store must restore it.
	s2, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	replayed := s2.Get("img-agent")
	if replayed == nil {
		t.Fatal("replay lost the agent")
	}
	if replayed.Image != gap069ImageRef {
		t.Errorf("replayed image = %q, want %q", replayed.Image, gap069ImageRef)
	}
}

// TestHeartbeatAndCompactPreserveImageRef proves neither a TTL extension nor a
// compaction drop the image ref: compaction rewrites the live set from folded
// state, so a field missing from that rewrite would silently strip every
// image-backed agent of its container context after `bunker registry compact`.
func TestHeartbeatAndCompactPreserveImageRef(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	s := testStore(t, Options{Path: path})

	withImage := spawnRecord("img-agent")
	withImage.Image = gap069ImageRef
	if err := s.AppendSpawn(withImage); err != nil {
		t.Fatal(err)
	}
	plain := spawnRecord("plain-agent")
	if err := s.AppendSpawn(plain); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendHeartbeat("img-agent", withImage.ExpiresAt.Add(6*time.Hour), "running"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	s2, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen after compaction: %v", err)
	}
	defer s2.Close()
	if got := s2.Get("img-agent"); got == nil || got.Image != gap069ImageRef {
		t.Errorf("compaction lost the image ref: %+v", got)
	}
	if got := s2.Get("plain-agent"); got == nil {
		t.Error("compaction lost the plain agent")
	} else if got.Image != "" {
		t.Errorf("plain agent gained an image ref: %q", got.Image)
	}
}
