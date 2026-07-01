package agent

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRootlessEnv_DisableDetachNetns(t *testing.T) {
	// rootlesskit v1.1.1 on the server does not support --detach-netns, but the
	// dockerd-rootless.sh script shipped with the installer defaults the flag to
	// true. Verify Spawn disables it explicitly.
	cmd := exec.Command("grep", "-n", "DOCKERD_ROOTLESS_ROOTLESSKIT_DETACH_NETNS=false", "manager.go")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("DOCKERD_ROOTLESS_ROOTLESSKIT_DETACH_NETNS=false not found in manager.go: %v\n%s", err, string(out))
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		t.Fatal("DOCKERD_ROOTLESS_ROOTLESSKIT_DETACH_NETNS=false not found in manager.go")
	}
}

func TestRootlessEnv_DoesNotForceFlagsPidns(t *testing.T) {
	// Spawn intentionally avoids setting DOCKERD_ROOTLESS_ROOTLESSKIT_FLAGS to
	// --pidns because that env-var string is appended to rootlesskit flags by the
	// script. Combined with --detach-netns (disabled above) or unsupported on the
	// server rootlesskit, it would cause the daemon to exit immediately. Verify it
	// is not present.
	cmd := exec.Command("grep", "-n", "DOCKERD_ROOTLESS_ROOTLESSKIT_FLAGS=--pidns", "manager.go")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err == nil && len(strings.TrimSpace(string(out))) > 0 {
		t.Fatalf("DOCKERD_ROOTLESS_ROOTLESSKIT_FLAGS=--pidns should not be set in manager.go")
	}
}
