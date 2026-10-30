package qa

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDetectOrphansPinsTTLAndUncollectedArtifactRules(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	agents := []Agent{
		{ID: "old-uncollected", CreatedAt: now.Add(-2 * time.Hour), BatteryArtifacts: []string{"/var/log/bunker-qa/old.log"}},
		{ID: "young-uncollected", CreatedAt: now.Add(-time.Hour), BatteryArtifacts: []string{"/var/log/bunker-qa/young.log"}},
		{ID: "old-collected", CreatedAt: now.Add(-3 * time.Hour), Collected: true, BatteryArtifacts: []string{"/var/log/bunker-qa/collected.log"}},
		{ID: "old-no-artifact", CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "future", CreatedAt: now.Add(time.Hour), BatteryArtifacts: []string{"/var/log/bunker-qa/future.log"}},
	}
	got := DetectOrphans(agents, now, 2*time.Hour)
	if len(got) != 1 || got[0].ID != "old-uncollected" {
		t.Fatalf("DetectOrphans() = %#v, want only old-uncollected", got)
	}
	if got := DetectOrphans(agents, now, 0); got != nil {
		t.Fatalf("non-positive TTL returned %#v, want nil", got)
	}
}

func TestRecoverOrphanQuarantinesEvidenceBeforeDestroy(t *testing.T) {
	root := t.TempDir()
	logDir := t.TempDir()
	first := filepath.Join(logDir, "battery.log")
	second := filepath.Join(logDir, "battery.stderr.log")
	if err := os.WriteFile(first, []byte("battery output\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("stderr output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := Agent{ID: "78b25793", BatteryArtifacts: []string{first, second}}
	var destroyed string
	recovery, err := RecoverOrphan(agent, root, time.Date(2026, 10, 17, 8, 49, 53, 0, time.UTC), func(id string) error {
		destroyed = id
		entries, readErr := os.ReadDir(root)
		if readErr != nil {
			return readErr
		}
		if len(entries) != 1 {
			return os.ErrNotExist
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RecoverOrphan() error = %v", err)
	}
	if destroyed != agent.ID {
		t.Fatalf("destroyed %q, want %q", destroyed, agent.ID)
	}
	if len(recovery.Artifacts) != 2 {
		t.Fatalf("quarantined artifacts = %d, want 2", len(recovery.Artifacts))
	}
	for _, path := range recovery.Artifacts {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read quarantine artifact %s: %v", path, readErr)
		}
		if !strings.Contains(string(data), "output") {
			t.Errorf("quarantine artifact %s has unexpected contents %q", path, data)
		}
	}
}

func TestRecoverOrphanDoesNotDestroyWithoutEvidence(t *testing.T) {
	root := t.TempDir()
	called := false
	_, err := RecoverOrphan(Agent{ID: "d0e066fe", BatteryArtifacts: []string{filepath.Join(root, "missing.log")}}, root, time.Now(), func(string) error {
		called = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "no battery artifacts preserved") {
		t.Fatalf("error = %v, want no-artifacts safety error", err)
	}
	if called {
		t.Fatal("destroy called despite missing evidence")
	}
}

func TestRecoveryArtifactOrderIsStable(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "a.log"), filepath.Join(dir, "b.log")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recovery, err := RecoverOrphan(Agent{ID: "stable", BatteryArtifacts: paths}, t.TempDir(), time.Now(), func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]string{filepath.Join(recovery.QuarantineDir, "a.log"), filepath.Join(recovery.QuarantineDir, "b.log")}, recovery.Artifacts) {
		t.Fatalf("artifact order changed: %#v", recovery.Artifacts)
	}
}
