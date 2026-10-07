// Package mountdriver: the rclone driver registration tests (MOUNT-007).
package mountdriver

import (
	"strings"
	"testing"
)

// TestRcloneDriverIsRegisteredAndOptIn pins the MOUNT-007 acceptance: the
// driver resolves by name, is NOT the default (the default must stay sshfs —
// the MOUNT-B2 recorded ruling), and declares its absence of a failure
// classifier with a non-empty, operator-readable reason.
func TestRcloneDriverIsRegisteredAndOptIn(t *testing.T) {
	d, err := Resolve(DriverRclone)
	if err != nil {
		t.Fatalf("Resolve(%q) failed: %v", DriverRclone, err)
	}
	if d.Name != "rclone" {
		t.Errorf("driver name = %q, want %q", d.Name, "rclone")
	}
	if d.Classify != nil {
		t.Error("rclone must NOT carry a classifier — its VFS/retry diagnostics are its own vocabulary, and inheriting the sshfs fragment set would misclassify them")
	}
	if d.NoClassifier == nil {
		t.Fatal("rclone must declare NoClassifier — an undeclared absence violates the registry invariant")
	}
	if strings.TrimSpace(d.NoClassifier.Why) == "" {
		t.Error("NoClassifier.Why must be a non-empty reason naming WHY, not a placeholder")
	}

	// Opt-in, never default: the empty-name resolution still lands on sshfs.
	def, err := Resolve("")
	if err != nil {
		t.Fatalf("Resolve(\"\") failed: %v", err)
	}
	if def.Name == DriverRclone {
		t.Error("the default driver resolved to rclone — the default must stay sshfs")
	}
	if def.Name != DefaultDriver {
		t.Errorf("default driver = %q, want %q", def.Name, DefaultDriver)
	}
	if DefaultDriver != "sshfs" {
		t.Errorf("DefaultDriver = %q, want \"sshfs\" — rclone is opt-in only and must never move the default", DefaultDriver)
	}
	if def.Classify == nil {
		t.Error("the sshfs default must keep its failure classifier")
	}
}

// TestRcloneShapeDriverValidates proves the registry invariant is
// satisfiable with ONLY a NoClassifier declaration: a driver literal with
// rclone's exact registration shape must pass Validate, so the invariant
// really is "classify OR declare absence", not "always classify".
func TestRcloneShapeDriverValidates(t *testing.T) {
	d := Driver{Name: DriverRclone, NoClassifier: &NoClassifierReason{Why: "test reason"}}
	if err := d.Validate(); err != nil {
		t.Errorf("a driver with only a NoClassifier reason must validate: %v", err)
	}
}
