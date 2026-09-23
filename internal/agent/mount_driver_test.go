// Package agent: the MOUNT-006 manager-side driver-seam tests.
//
// The RPC boundary (internal/server) maps unknown mount drivers to
// CodeInvalidArgument; this file proves the MANAGER backstop — Spawn itself
// refuses an unregistered driver at the validate stage, before any side
// effect, using the hermetic INT-CI-005 fixture (no root, no host state).
package agent

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/mountdriver"
	"github.com/deployBunker/bunker/internal/resource"
)

// TestSpawnUnknownMountDriverRefusedAtValidateStage is the manager-side
// backstop (AC b): even a caller that bypasses the RPC boundary gets a
// named error from Spawn's Step 1c — before user creation, port
// allocation, or dockerd start. Never a silent sshfs fallback.
func TestSpawnUnknownMountDriverRefusedAtValidateStage(t *testing.T) {
	m := intci5Manager(t)

	_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{
		AgentId:     uniqueAgentID("mount006-mgr"),
		Ttl:         "1h",
		MountDriver: "fusedoesnotexist",
	})
	if err == nil {
		t.Fatal("manager accepted an unknown mount driver")
	}
	if !strings.Contains(err.Error(), "fusedoesnotexist") {
		t.Errorf("refusal does not name the offending driver: %v", err)
	}
	// Refusal happens at the validate stage, BEFORE user creation.
	if !strings.Contains(err.Error(), "failed at stage "+StageValidate) {
		t.Errorf("refusal must fire at the validate stage (before any side effect): %v", err)
	}
	// The cause is the seam's unknown-driver refusal, so callers can match
	// on the wrapped error (mountdriver.ErrUnknownDriver via errors.Is on
	// the seam error); the text here names the driver and lists what IS
	// registered, which is the diagnosable form.
	if !strings.Contains(err.Error(), "unknown mount driver") {
		t.Errorf("refusal does not carry the unknown-driver cause: %v", err)
	}
}

// TestResolveMountDriverDefaultsToSSHFS pins the manager's resolution: the
// empty request resolves to the registered sshfs driver — the default the
// old-client contract is written against.
func TestResolveMountDriverDefaultsToSSHFS(t *testing.T) {
	d, err := resolveMountDriver("")
	if err != nil {
		t.Fatalf("resolveMountDriver(\"\") failed: %v", err)
	}
	if d.Name != mountdriver.DefaultDriver {
		t.Errorf("default mount driver = %q, want %q", d.Name, mountdriver.DefaultDriver)
	}
	if d.Classify == nil {
		t.Error("the default driver must carry a failure classifier")
	}
}

// TestResolveMountDriverUnknownNamesRefused pins the named refusal shape
// resolveMountDriver produces for the server and manager paths.
func TestResolveMountDriverUnknownNamesRefused(t *testing.T) {
	for _, name := range []string{"rclone", "io_uring", "Rclone"} {
		d, err := resolveMountDriver(name)
		if err == nil {
			t.Errorf("resolveMountDriver(%q) returned driver %+v; must refuse", name, d)
			continue
		}
		if d.Name != "" {
			t.Errorf("resolveMountDriver(%q) returned a driver alongside the error", name)
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal for %q does not name it: %v", name, err)
		}
	}
}

// TestBuildMountCommandSSHFSByteIdentical pins the sshfs command shape: the
// default driver's command must stay byte-identical to the pre-seam
// hardcoded format (old clients parse this string with strings.Fields).
func TestBuildMountCommandSSHFSByteIdentical(t *testing.T) {
	d, err := resolveMountDriver("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got, err := buildMountCommand(d, "/etc/bunkerd/ssh/abc123", "bunker-abc123", "myhost", "/home/bunker-abc123", "abc123")
	if err != nil {
		t.Fatalf("buildMountCommand: %v", err)
	}
	want := "sshfs -o IdentityFile=/etc/bunkerd/ssh/abc123 -o idmap=user -o allow_other bunker-abc123@myhost:/home/bunker-abc123 /mnt/bunker/abc123"
	if got != want {
		t.Errorf("sshfs mount command changed shape:\n got  %s\n want %s", got, want)
	}
}

// TestMountDriverSurvivesRegistryRoundTrip proves the durable plane: a
// record carrying a mount driver survives recordToRegistry → registryToRecord
// unchanged, so a replayed/adopted agent keeps the driver its spawn selected.
func TestMountDriverSurvivesRegistryRoundTrip(t *testing.T) {
	rec := &resource.AgentRecord{
		AgentID:     "mount006-rt",
		Status:      "running",
		MountDriver: "sshfs",
	}
	durable := recordToRegistry(rec)
	if durable.MountDriver != "sshfs" {
		t.Fatalf("recordToRegistry dropped the mount driver: %+v", durable)
	}
	back := registryToRecord(durable)
	if back.MountDriver != "sshfs" {
		t.Errorf("registryToRecord dropped the mount driver: %+v", back)
	}
}
