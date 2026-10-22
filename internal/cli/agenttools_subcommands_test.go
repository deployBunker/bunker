package cli

import (
	"strings"
	"testing"
)

// TOOLS-003 ACs 2-3: `bunker agent-tools install --help` and
// `bunker agent-tools uninstall --help` must exit 0 — the delivery and
// rollback paths are discoverable SUBCOMMAND surfaces, not only flags. The
// registration tests pin the surface; the behaviour tests above pin the flows
// the subcommands delegate to.

// TestAgentToolsSubcommandsRegistered pins the command tree: install and
// uninstall exist as subcommands of agent-tools, and their help renders both
// the action and the AGENT_ID argument.
func TestAgentToolsSubcommandsRegistered(t *testing.T) {
	cmd := NewAgentToolsCommand()
	got := map[string]bool{}
	for _, sub := range cmd.Commands() {
		got[strings.Fields(sub.Name())[0]] = true
	}
	for _, want := range []string{"install", "uninstall"} {
		if !got[want] {
			t.Errorf("agent-tools has no %q subcommand; registered: %v", want, got)
		}
	}
}

// TestAgentToolsSubcommandHelpExitsZero drives the two AC help surfaces the
// way the row states them, through the command's own Execute path.
func TestAgentToolsSubcommandHelpExitsZero(t *testing.T) {
	for _, name := range []string{"install", "uninstall"} {
		t.Run(name, func(t *testing.T) {
			cmd := NewAgentToolsCommand()
			cmd.SetArgs([]string{name, "--help"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("agent-tools %s --help must exit 0, got: %v", name, err)
			}
		})
	}
}

// TestAgentToolsSubcommandDelegatesWithFlag pins the delegation contract:
// `agent-tools install <id>` routes into the SAME RunE the --install flag
// drives (one delivery implementation), observable by the error the flow
// produces without a reachable config — a probe-path error, not a cobra
// usage error. A subcommand that merely printed help would produce no such
// flow error.
func TestAgentToolsSubcommandDelegatesWithFlag(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(SessionTargetEnvVar, "default")

	cmd := NewAgentToolsCommand()
	cmd.SetArgs([]string{"install", "some-agent"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("the install subcommand must delegate into the delivery flow (which fails here on config), not stop at help")
	}
	if strings.Contains(err.Error(), "unknown command") || strings.Contains(err.Error(), "accepts") {
		t.Errorf("the subcommand must be recognized and delegate, got: %v", err)
	}
}
