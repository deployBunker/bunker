package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
)

// INFRA-BACKUP-01 destroy-flow integration: with destroy_archive_keep=2
// and 4 pre-seeded fake archives, a successful destroy archives the home,
// prunes the dir down to exactly 2 tarballs, and the destroy still
// completes. The prune runs after the archive is verified and never fails
// the destroy.

func TestDestroy_ArchiveKeepPrunesPreSeedArchives(t *testing.T) {
	m, cfg, _ := newDestroyArchiveFixture(t)
	agentID := "infrabk1"
	root, _ := foreignHome(t, agentID)
	pointHomeAt(t, root)

	archiveDir := t.TempDir()
	cfg.Agent.DestroyArchiveDir = archiveDir
	cfg.Agent.DestroyHomePolicy = config.DestroyPolicyArchive
	cfg.Agent.DestroyArchiveKeep = 2
	cfg.Agent.DestroyArchiveMaxBytes = 0

	// Seed 4 fake archives with distinct mtimes (oldest -> newest).
	base := time.Now().Add(-10 * time.Minute)
	oldestSeeds := []string{
		"bunker-old1-20260101T000000Z.tar.gz",
		"bunker-old2-20260102T000000Z.tar.gz",
	}
	for i, name := range []string{
		oldestSeeds[0],
		oldestSeeds[1],
		"bunker-new1-20260103T000000Z.tar.gz",
		"bunker-new2-20260104T000000Z.tar.gz",
	} {
		seedArchive(t, archiveDir, name, 100, base.Add(time.Duration(i)*time.Minute))
	}

	registerArchiveAgent(t, m, agentID)

	resp, derr := m.Destroy(context.Background(), agentID, false)
	if derr != nil {
		t.Fatalf("Destroy() error = %v", derr)
	}
	if resp.Status != "destroyed" {
		t.Fatalf("Destroy() status = %q, want destroyed", resp.Status)
	}

	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tar.gz") {
			names = append(names, e.Name())
		}
	}
	// keep=2 must leave exactly two: the destroy's own archive (newest
	// mtime) and the newest pre-seed; both oldest seeds are evicted.
	if len(names) != 2 {
		t.Fatalf("archive dir holds %d tar.gz files after keep=2 destroy, want 2: %v", len(names), names)
	}
	for _, gone := range oldestSeeds {
		if hasElement(names, gone) {
			t.Errorf("oldest seed %q survived keep=2 pruning", gone)
		}
	}
	if !hasElement(names, "bunker-new2-20260104T000000Z.tar.gz") {
		t.Errorf("expected newest preseed to survive keep=2: %v", names)
	}
	// The destroy's own archive survives (name embeds bunker-<id>-<UTC>).
	ownCount := 0
	for _, n := range names {
		if strings.HasPrefix(n, "bunker-"+agentID+"-") {
			ownCount++
		}
	}
	if ownCount != 1 {
		t.Errorf("destroy's own archive not among survivors: %v", names)
	}
}
