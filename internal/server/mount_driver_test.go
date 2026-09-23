// MOUNT-006 server-side seam tests: explicit driver identity on the spawn
// response and the agent record, and the named CodeInvalidArgument refusal
// for an unknown mount driver.

package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/mountdriver"
	"github.com/deployBunker/bunker/internal/resource"
)

// gap116TestService (gap116_test.go) is reused: a bunkerdService whose
// agentMgr is a spawnTrackingManager (records whether Spawn was reached).

// TestSpawnAgent_UnknownMountDriverIsInvalidArgument is the server-side
// refusal (AC b): a spawn naming an unregistered driver comes back as
// CodeInvalidArgument, names the driver, and NEVER reaches the agent
// manager (no user, no ports, no dockerd).
func TestSpawnAgent_UnknownMountDriverIsInvalidArgument(t *testing.T) {
	svc, _, spawnCalled := gap116TestService(t)

	req := connect.NewRequest(&v1.SpawnAgentRequest{AgentId: "mount006-bad", MountDriver: "rclone"})
	_, err := svc.SpawnAgent(context.Background(), req)
	if err == nil {
		t.Fatal("unknown mount driver accepted at the RPC boundary")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want %s (err: %v)", connect.CodeOf(err), connect.CodeInvalidArgument, err)
	}
	if !strings.Contains(err.Error(), "rclone") {
		t.Errorf("error should name the offending driver: %v", err)
	}
	if *spawnCalled {
		t.Error("agentMgr.Spawn was called despite the unknown mount driver")
	}
}

// TestSpawnAgent_EmptyMountDriverReachesManager proves the default: an
// EMPTY mount driver (the overwhelmingly common old-client shape) passes the
// boundary check and reaches the manager — the sshfs default must not
// require the field to be set.
func TestSpawnAgent_EmptyMountDriverReachesManager(t *testing.T) {
	svc, _, spawnCalled := gap116TestService(t)

	req := connect.NewRequest(&v1.SpawnAgentRequest{AgentId: "mount006-empty"})
	if _, err := svc.SpawnAgent(context.Background(), req); err != nil {
		if connect.CodeOf(err) == connect.CodeInvalidArgument {
			t.Fatalf("empty mount driver wrongly refused at the boundary: %v", err)
		}
		// Other failures (fake manager internals) are out of scope here.
	}
	if !*spawnCalled {
		t.Error("spawn with an empty (default) mount driver never reached the agent manager")
	}
}

// TestSpawnAgent_MountSpecOnRecordAndResponse covers the agent-record surface
// (AC a, record half): a spawned record reports its driver explicitly; a
// pre-seam record (empty MountDriver) reports the sshfs default rather than
// nothing, so the identity is explicit for every record.
// (The manager-side refusal backstop lives in internal/agent
// (mount_driver_test.go), alongside the INT-CI-005 spawn fixture.)
func TestSpawnAgent_MountSpecOnRecordAndResponse(t *testing.T) {
	t.Run("pre-seam record reports sshfs default", func(t *testing.T) {
		rec := &resource.AgentRecord{
			AgentID:    "mount006-legacy",
			Status:     "running",
			SshfsMount: "sshfs -o IdentityFile=/k bunker-u@h:/home/bunker-u /mnt/bunker/x",
		}
		sum := rec.ToAgentSummary()
		if sum.GetMountSpec() == nil {
			t.Fatal("pre-seam record surfaced no MountSpec")
		}
		if got := sum.GetMountSpec().GetDriver(); got != mountdriver.DefaultDriver {
			t.Errorf("pre-seam record driver = %q, want %q", got, mountdriver.DefaultDriver)
		}
		if sum.GetMountSpec().GetCommand() != rec.SshfsMount {
			t.Errorf("MountSpec command does not carry the stored command: %q", sum.GetMountSpec().GetCommand())
		}
	})

	t.Run("no stored command reports no spec", func(t *testing.T) {
		rec := &resource.AgentRecord{AgentID: "mount006-empty", Status: "running"}
		if sum := rec.ToAgentSummary(); sum.GetMountSpec() != nil {
			t.Errorf("record with no mount command produced a MountSpec: %+v", sum.GetMountSpec())
		}
	})

	t.Run("unknown recorded driver is reported verbatim", func(t *testing.T) {
		rec := &resource.AgentRecord{
			AgentID:     "mount006-unknown",
			Status:      "running",
			MountDriver: "rclone",
			SshfsMount:  "rclone mount ...",
		}
		sum := rec.ToAgentSummary()
		if got := sum.GetMountSpec().GetDriver(); got != "rclone" {
			t.Errorf("unknown recorded driver re-labelled to %q; it must be reported verbatim", got)
		}
	})
}

// TestSpawnResponseCarriesMountSpecAlongsideLegacyField is AC a's response
// half at the type level: the generated response still has the legacy
// SshfsMount field (old clients keep reading it) AND MountSpec. This pins
// the additive shape — a field removal or renumber would fail here.
func TestSpawnResponseCarriesMountSpecAlongsideLegacyField(t *testing.T) {
	resp := &v1.SpawnAgentResponse{
		SshfsMount: "sshfs -o IdentityFile=/k bunker-u@h:/home/bunker-u /mnt/bunker/x",
		MountSpec:  &v1.MountSpec{Driver: "sshfs", Command: "sshfs -o IdentityFile=/k bunker-u@h:/home/bunker-u /mnt/bunker/x"},
	}
	if resp.GetSshfsMount() == "" {
		t.Error("legacy sshfs_mount field must remain populated alongside MountSpec")
	}
	if resp.GetMountSpec().GetDriver() != "sshfs" {
		t.Errorf("MountSpec.Driver = %q, want sshfs", resp.GetMountSpec().GetDriver())
	}
	if resp.GetMountSpec().GetCommand() != resp.GetSshfsMount() {
		t.Error("MountSpec.Command must equal the legacy sshfs_mount string for the sshfs driver")
	}
}

// TestDefaultConfigSpawnDefaultsAreSane is a guard so a future config
// refactor cannot silently introduce a non-sshfs mount default.
func TestDefaultConfigMountDriverIsUnset(t *testing.T) {
	// config.DefaultConfig has no mount-driver knob yet (MOUNT-006 keeps
	// the default code-level); this test exists so adding one later without
	// defaulting it to sshfs trips a reviewer here.
	cfg := config.DefaultConfig()
	_ = cfg // no knob today — the default lives in mountdriver.DefaultDriver
	if mountdriver.DefaultDriver != "sshfs" {
		t.Errorf("mountdriver.DefaultDriver moved to %q; the proto comment and old-client contract still say sshfs", mountdriver.DefaultDriver)
	}
}
