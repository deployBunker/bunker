package registry

// REV-BUNKER-P1-PATCH: Open creates a MISSING registry file (MkdirAll +
// O_CREATE), so a daemon booting without durable state replayed an empty live
// set and could not tell "no agents" apart from "no memory of the host". The
// distinction is provenance, not replay, and it is captured once at Open.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreCreatedThisBoot_AbsentFileIsReportedAndPreExistingIsNot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("precondition: %s must not exist (stat err = %v)", path, err)
	}

	first, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open (absent file): %v", err)
	}
	if !first.CreatedThisBoot() {
		t.Error("CreatedThisBoot() = false for a registry file that did not exist before Open")
	}
	// Open DID create it — the flag describes provenance, and the file must
	// exist for the next boot to lose the flag.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Open must have created the active file: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open (existing file): %v", err)
	}
	defer second.Close()
	if second.CreatedThisBoot() {
		t.Error("CreatedThisBoot() = true for a registry file that already existed")
	}
	if second.LiveCount() != 0 {
		t.Errorf("LiveCount() = %d, want 0 — an empty file is an empty live set", second.LiveCount())
	}
}

// A file that exists AND carries a live record is proven in both senses: the
// accessor is false and the live set is non-empty. This is the pair the sweep
// guard reads.
func TestStoreCreatedThisBoot_PreExistingStateIsProven(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	seed, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := seed.AppendSpawn(&Record{AgentID: "live-one", Status: "running"}); err != nil {
		t.Fatalf("AppendSpawn: %v", err)
	}
	if !seed.CreatedThisBoot() {
		t.Error("the seeding store created the file, so its own flag must be true")
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	next, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer next.Close()
	if next.CreatedThisBoot() {
		t.Error("a pre-existing registry carrying a live record must not report as created this boot")
	}
	if next.LiveCount() != 1 {
		t.Errorf("LiveCount() = %d, want 1 after replay", next.LiveCount())
	}
}

// Replay (a later call, or a rotation) must not perturb provenance: the flag is
// captured once, before the file could be created.
func TestStoreCreatedThisBoot_SurvivesReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if _, err := s.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !s.CreatedThisBoot() {
		t.Error("Replay cleared the provenance flag")
	}
}
