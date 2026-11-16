package cli

import (
	"strings"
	"testing"
	"time"
)

// TestLiveToolsWinsWhenSpawnFactIsStale is the DOGFOOD-017 regression arm:
// the image recorded no tools at spawn, but the current in-agent census found
// all three command -v tools. The operator-facing live value must not silently
// reuse the stale spawn-time zero.
func TestLiveToolsWinsWhenSpawnFactIsStale(t *testing.T) {
	spawnZero := uint32(0)
	liveCount := uint32(3)
	ready := true
	spawn := formatSpawnTools("agent-017", &spawnZero)
	live := formatLiveTools(&ready, &liveCount, time.Now().UTC().Format(time.RFC3339), "in-agent command -v via bunker info")
	if !strings.Contains(spawn, "0 of") {
		t.Fatalf("fixture did not establish the stale spawn fact: %q", spawn)
	}
	if !strings.Contains(live, "ready") || !strings.Contains(live, "3 of 3") {
		t.Fatalf("live census did not replace stale spawn fact: %q", live)
	}
	if strings.Contains(live, "0 of") {
		t.Fatalf("live report silently reused stale spawn fact: %q", live)
	}
}
