// Package resource: the MOUNT-006 MountSpec summary derivation tests.

package resource

import (
	"testing"
)

// TestDefaultMountDriverMatchesSeam pins the cross-package contract: the
// resource package's literal default must equal the mountdriver seam's.
// (resource does not import mountdriver; a drift here would re-label
// pre-seam records with a wrong default.)
func TestDefaultMountDriverMatchesSeam(t *testing.T) {
	if DefaultMountDriver != "sshfs" {
		t.Fatalf("DefaultMountDriver = %q, want sshfs (must match mountdriver.DefaultDriver)", DefaultMountDriver)
	}
}

// TestNewMountSpecForSummary is table-driven over the identity derivation.
func TestNewMountSpecForSummary(t *testing.T) {
	cases := []struct {
		name        string
		driver      string
		command     string
		wantDriver  string
		wantCommand string
		wantNil     bool
	}{
		{
			name:        "explicit driver",
			driver:      "sshfs",
			command:     "sshfs -o IdentityFile=/k u@h:/home /mnt/x",
			wantDriver:  "sshfs",
			wantCommand: "sshfs -o IdentityFile=/k u@h:/home /mnt/x",
		},
		{
			name:        "empty driver defaults to sshfs",
			driver:      "",
			command:     "sshfs -o IdentityFile=/k u@h:/home /mnt/x",
			wantDriver:  DefaultMountDriver,
			wantCommand: "sshfs -o IdentityFile=/k u@h:/home /mnt/x",
		},
		{
			name:    "no command yields no spec",
			driver:  "sshfs",
			command: "",
			wantNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newMountSpecForSummary(tc.driver, tc.command)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("want nil spec, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("want a spec, got nil")
			}
			if got.Driver != tc.wantDriver {
				t.Errorf("driver = %q, want %q", got.Driver, tc.wantDriver)
			}
			if got.Command != tc.wantCommand {
				t.Errorf("command = %q, want %q", got.Command, tc.wantCommand)
			}
		})
	}
}

// TestAgentRecordSummaryCarriesMountSpec proves ToAgentSummary surfaces the
// explicit identity next to the legacy field (AC a, record half).
func TestAgentRecordSummaryCarriesMountSpec(t *testing.T) {
	rec := &AgentRecord{
		AgentID:     "sum-1",
		Status:      "running",
		SshfsMount:  "sshfs -o IdentityFile=/k u@h:/home /mnt/x",
		MountDriver: "sshfs",
	}
	sum := rec.ToAgentSummary()
	if sum.GetSshfsMount() != rec.SshfsMount {
		t.Errorf("legacy sshfs_mount missing from summary: %q", sum.GetSshfsMount())
	}
	if sum.GetMountSpec().GetDriver() != DefaultMountDriver {
		t.Errorf("MountSpec.Driver = %q, want %q", sum.GetMountSpec().GetDriver(), DefaultMountDriver)
	}
	if sum.GetMountSpec().GetCommand() != rec.SshfsMount {
		t.Errorf("MountSpec.Command = %q, want the stored command", sum.GetMountSpec().GetCommand())
	}
}
