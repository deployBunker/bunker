package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/hostsetup"
)

// TestHostProvisionCommand_DryRunAndStatus runs the installer in its two
// read-only modes against the real host. Neither mode may change host state:
// the dry run prints the plan and the status probe only reports what exists.
func TestHostProvisionCommand_DryRunAndStatus(t *testing.T) {
	t.Run("status", func(t *testing.T) {
		cmd := NewHostProvisionCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"--status"})
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("host-provision --status: %v", err)
		}
		got := out.String()
		for _, want := range []string{
			"per-session private /tmp (pam_namespace, agent-scoped)",
			"classifier module:",
			"precondition module:",
			"agent name pattern:",
			"agent group:",
			"deployed block group:",
			"namespace drop-in:",
			"drop-in rule ok:",
			"drop-in owner/mode:",
			"precondition helper:",
			"helper manifest:",
			"manifest owner/mode:",
			"helper directory:",
			"helper dir owner/mode:",
			"sshd PAM block:",
			"ownership verified:",
			"ACTIVE:",
			"shared scratch",
			"root mode ok:",
			"host /tmp tmpfs cap",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("status output missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("status json carries the scoping facts", func(t *testing.T) {
		cmd := NewHostProvisionCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"--status", "--json"})
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("host-provision --status --json: %v", err)
		}
		got := out.String()
		for _, want := range []string{
			"\"classifier_module\"", "\"pam_exec_module\"", "\"agent_name_pattern\"", "\"agent_group\"",
			"\"agent_group_present\"", "\"helper_integrity_ok\"", "\"sshd_pam_block\"",
			// The trust chain, the drop-in's rule/owner/mode and the instance
			// parent are part of the verdict, so they are part of the surface.
			"\"namespace_drop_in_rule_ok\"", "\"namespace_drop_in_owner\"", "\"namespace_drop_in_mode\"",
			"\"helper_dir\"", "\"helper_dir_owner_ok\"", "\"helper_dir_mode_ok\"",
			"\"helper_owner_ok\"", "\"helper_mode_ok\"",
			"\"manifest_owner_ok\"", "\"manifest_mode_ok\"",
			"\"instance_root_mode_ok\"", "\"instance_root_owner_ok\"",
			"\"ownership_verifiable\"",
			"\"root_mode_ok\"", "\"root_expected_mode\"",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("json status missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("status json never claims isolation without a verified boundary", func(t *testing.T) {
		cmd := NewHostProvisionCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"--status", "--json"})
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("host-provision --status --json: %v", err)
		}
		var payload struct {
			Isolated bool `json:"isolated"`
			Private  struct {
				ModulePresent    bool `json:"module_present"`
				DropInRuleOK     bool `json:"namespace_drop_in_rule_ok"`
				HelperDirModeOK  bool `json:"helper_dir_mode_ok"`
				HelperModeOK     bool `json:"helper_mode_ok"`
				ManifestModeOK   bool `json:"manifest_mode_ok"`
				HelperIntegrity  bool `json:"helper_integrity_ok"`
				InstanceRootOK   bool `json:"instance_root_mode_ok"`
				HelperDirPresent bool `json:"helper_dir_present"`
			} `json:"private_tmp"`
		}
		if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
			t.Fatalf("decode status json: %v\n%s", err, out.String())
		}
		// The verdict is the conjunction of the observed boundary. Whatever
		// the host's state is, an isolated claim must come with every one of
		// these true — this is what stops the false green the review found.
		if payload.Isolated {
			for name, ok := range map[string]bool{
				"module present":     payload.Private.ModulePresent,
				"drop-in rule":       payload.Private.DropInRuleOK,
				"helper dir present": payload.Private.HelperDirPresent,
				"helper dir mode":    payload.Private.HelperDirModeOK,
				"helper mode":        payload.Private.HelperModeOK,
				"manifest mode":      payload.Private.ManifestModeOK,
				"helper integrity":   payload.Private.HelperIntegrity,
				"instance root mode": payload.Private.InstanceRootOK,
			} {
				if !ok {
					t.Errorf("isolated=true while %s is false:\n%s", name, out.String())
				}
			}
		}
	})

	t.Run("dry run prints a plan and mutates nothing", func(t *testing.T) {
		// The plan path probes the INSTALLED daemon (INT-DEMO-001), so this
		// subtest points --daemon-binary at a fixture reporting a current
		// version: the test must hold on any host, whatever daemon is
		// installed there. The refusal itself (older daemon) is covered by
		// the daemon-skew decision tests in internal/hostsetup.
		dir := t.TempDir()
		bin := filepath.Join(dir, "bunkerd-version-fixture")
		script := "#!/bin/sh\necho 'bunkerd 0.1.4'\necho '  commit:     abcdef0'\necho '  built:      2026-09-12T00:00:00Z'\necho '  caps:       " + hostsetup.GrantCapability + "'\n"
		if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := NewHostProvisionCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"--daemon-binary", bin})
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("host-provision (dry run): %v", err)
		}
		got := out.String()
		if !strings.Contains(got, "dry run") {
			t.Errorf("dry run did not say so:\n%s", got)
		}
		if !strings.Contains(got, "plan") {
			t.Errorf("dry run printed no plan lines:\n%s", got)
		}
	})

	t.Run("status json is machine readable", func(t *testing.T) {
		cmd := NewHostProvisionCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"--status", "--json"})
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("host-provision --status --json: %v", err)
		}
		if !strings.Contains(out.String(), "\"isolated\"") {
			t.Errorf("json status missing the isolation verdict:\n%s", out.String())
		}
	})
}

// TestHostProvisionCommand_UninstallDryRun confirms the uninstall path is
// dry-run by default: without --apply it must not touch the sshd PAM stack.
func TestHostProvisionCommand_UninstallDryRun(t *testing.T) {
	cmd := NewHostProvisionCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--uninstall"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("host-provision --uninstall (dry run): %v", err)
	}
	if !strings.Contains(out.String(), "dry run") {
		t.Errorf("uninstall dry run did not say so:\n%s", out.String())
	}
}
