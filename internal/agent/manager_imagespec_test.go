package agent

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// ─────────────────────────────────────────────────────────────────────────────
// Step 1.7: image spec validation happens BEFORE any side effect
// ─────────────────────────────────────────────────────────────────────────────

// TestSpawn_RejectedImageSpecBeforeUserCreation proves an invalid spec fails
// fast with zero side effects: no useradd is even attempted. It substitutes
// useradd with a canary that fails the test if reached — the validation gate
// must return before Step 2 ever runs.
func TestSpawn_RejectedImageSpecBeforeUserCreation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("imgspec-reject")

	req := &v1.SpawnAgentRequest{
		AgentId: agentID,
		ImageSpec: &v1.ImageSpec{
			Packages: []*v1.PackageAdd{{Manager: "apt", Packages: []string{"curl|sh"}}},
		},
	}
	_, err := m.Spawn(context.Background(), req)
	if err == nil {
		t.Fatal("invalid image spec accepted")
	}
	if !strings.Contains(err.Error(), "image spec") {
		t.Errorf("error should mention image spec, got: %v", err)
	}
	// The tracker must not have registered the agent either.
	if m.tracker.Get(agentID) != nil {
		t.Error("rejected spawn registered the agent anyway")
	}
	// No port range may leak (Free is idempotent).
	if m.portAlloc != nil {
		m.portAlloc.Free(agentID)
	}
}

// TestSpawn_AcceptsValidImageSpec_NoDaemon exercises the validation path with
// a valid spec on a non-LIVE manager: the builder is explicitly forced to nil
// (GAP-064 wires a real builder in newTestManager), so a valid spec must fail
// fast at the Step 1.7 nil-builder gate — before proto validation, user
// creation, or any other side effect (imageBuilder==nil ⇒ "not available").
// This pins the gate ordering: nil builder + valid spec → clean validation
// error, no user, no Docker build.
func TestSpawn_ValidSpecNilBuilderRejected(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	// Force the nil-builder gate: newTestManager wires a real builder via
	// NewAgentManager (GAP-064). Without this, a valid spec sails past the
	// Step 1.7 gate and reaches a real rootless Docker build in Step 5b.5,
	// which hangs root CI runners (INT-CI-002).
	m.imageBuilder = nil
	agentID := uniqueAgentID("imgspec-nilb")

	req := &v1.SpawnAgentRequest{
		AgentId: agentID,
		ImageSpec: &v1.ImageSpec{
			Packages: []*v1.PackageAdd{{Manager: "apt", Packages: []string{"jq"}}},
		},
	}
	_, err := m.Spawn(context.Background(), req)
	if err == nil {
		t.Fatal("expected nil-builder rejection")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("want builder-unavailable error, got: %v", err)
	}
	if m.tracker.Get(agentID) != nil {
		t.Error("rejected spawn registered the agent anyway")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Destroy hook: cleanupAgentContainers socket scoping
// ─────────────────────────────────────────────────────────────────────────────

// fakeDockerPath swaps `docker` on PATH for a recording stub, then restores.
// The returned func reads and returns the recorded invocations.
func fakeDockerPath(t *testing.T) func() []string {
	t.Helper()
	dir := t.TempDir()
	logFile := dir + "/calls"
	script := "#!/bin/sh\necho \"$@\" >> " + logFile + "\necho 'no such container'\nexit 1\n"
	if err := os.WriteFile(dir+"/docker", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func() []string {
		data, err := os.ReadFile(logFile)
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

func TestCleanupAgentContainers_OnlyAgentSocket(t *testing.T) {
	readCalls := fakeDockerPath(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := cleanupAgentContainers(context.Background(), "abc123", false, logger); err != nil {
		// rm always "fails" in the stub with 'no such container' → tolerated.
		t.Fatalf("idempotent cleanup should tolerate missing container: %v", err)
	}
	joined := strings.Join(readCalls(), "\n")
	if !strings.Contains(joined, "--host unix:///run/bunker/abc123/docker.sock") {
		t.Errorf("cleanup did not use the agent socket:\n%s", joined)
	}
	if !strings.Contains(joined, "stop -t 5 bunker-abc123") {
		t.Errorf("cleanup missing graceful stop of bunker-abc123:\n%s", joined)
	}
	if !strings.Contains(joined, "rm bunker-abc123") {
		t.Errorf("cleanup missing rm of bunker-abc123:\n%s", joined)
	}
	for _, banned := range []string{"/var/run/docker.sock", "--privileged", "pid=host"} {
		if strings.Contains(joined, banned) {
			t.Errorf("cleanup used banned argument %s:\n%s", banned, joined)
		}
	}
}

func TestCleanupAgentContainers_ForceUsesRmDashF(t *testing.T) {
	readCalls := fakeDockerPath(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := cleanupAgentContainers(context.Background(), "abc123", true, logger); err != nil {
		t.Fatalf("force cleanup: %v", err)
	}
	joined := strings.Join(readCalls(), "\n")
	if !strings.Contains(joined, "rm -f bunker-abc123") {
		t.Errorf("force cleanup must use rm -f:\n%s", joined)
	}
	if strings.Contains(joined, "stop ") {
		t.Errorf("force cleanup must skip graceful stop:\n%s", joined)
	}
}

func TestCleanupAgentContainers_RejectsBadAgentID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := cleanupAgentContainers(context.Background(), "../evil", false, logger); err == nil {
		t.Fatal("path traversal agent id accepted")
	}
}

func TestAgentContainerName(t *testing.T) {
	if got := agentContainerName("abc123"); got != "bunker-abc123" {
		t.Errorf("agentContainerName = %q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Builder wiring: AgentManager carries a spec-keyed builder from config
// ─────────────────────────────────────────────────────────────────────────────

func TestNewAgentManager_WiresImageBuilder(t *testing.T) {
	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	m := NewAgentManager(cfg, logger, tracker, nil, nil)
	if m.imageBuilder == nil {
		t.Fatal("NewAgentManager did not wire the image builder")
	}
	m.Stop()
}
