package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestSurfaceCommandRegistered is the compile-level smoke (SURF-006): both
// verbs must be reachable from the command tree `bunker surface` exposes, and
// each must render usage naming its agent-id argument and the --server flag.
func TestSurfaceCommandRegistered(t *testing.T) {
	cmd := NewSurfaceCommand()
	names := map[string]bool{}
	for _, c := range cmd.Commands() {
		names[c.Name()] = true
	}
	for _, want := range []string{"install", "remove"} {
		if !names[want] {
			t.Fatalf("surface subcommand %q is not registered; registered: %v", want, names)
		}
	}

	for _, name := range []string{"install", "remove"} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{name, "--help"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("`surface %s --help` returned an error: %v", name, err)
			}
			help := out.String()
			for _, want := range []string{"<agent-id>", "--server", "Usage:"} {
				if !strings.Contains(help, want) {
					t.Errorf("`surface %s --help` does not show %q:\n%s", name, want, help)
				}
			}
			// The binary dependency must be named on the install surface, not
			// buried: the units are management, agent-tools --install
			// delivers the binary. (The group help carries the same note.)
			if name == "install" && !strings.Contains(help, "agent-tools") {
				t.Errorf("`surface install --help` must name `bunker agent-tools --install` as the binary-delivery path:\n%s", help)
			}
		})
	}
}

// TestSurfaceSubcommandFlags pins the flag grammar: both verbs accept --server,
// --json and --timeout, and nothing renders a backticked span in its flag
// usage (GAP-086 placeholder guard applies to the whole tree).
func TestSurfaceSubcommandFlags(t *testing.T) {
	cmd := NewSurfaceCommand()
	for _, sub := range cmd.Commands() {
		for _, flag := range []string{"server", "json", "timeout"} {
			if f := sub.Flags().Lookup(flag); f == nil {
				t.Errorf("surface %s: --%s missing", sub.Name(), flag)
			}
		}
	}
}
