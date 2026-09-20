package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/cli"
)

func TestExitCodeFor_ExitError(t *testing.T) {
	code, ok := exitCodeFor(&cli.ExitError{Code: 7})
	if !ok || code != 7 {
		t.Errorf("exitCodeFor(ExitError{7}) = (%d, %v), want (7, true)", code, ok)
	}
}

func TestExitCodeFor_WrappedExitError(t *testing.T) {
	err := fmt.Errorf("exec agent: %w", &cli.ExitError{Code: 127})
	code, ok := exitCodeFor(err)
	if !ok || code != 127 {
		t.Errorf("exitCodeFor(wrapped ExitError) = (%d, %v), want (127, true)", code, ok)
	}
}

func TestExitCodeFor_PlainError(t *testing.T) {
	if code, ok := exitCodeFor(errors.New("boom")); ok || code != 0 {
		t.Errorf("exitCodeFor(plain error) = (%d, %v), want (0, false)", code, ok)
	}
}

func TestExitCodeFor_Nil(t *testing.T) {
	if code, ok := exitCodeFor(nil); ok || code != 0 {
		t.Errorf("exitCodeFor(nil) = (%d, %v), want (0, false)", code, ok)
	}
}

// TestVersionFlagMatchesVersionSubcommand asserts the UX-005 contract: the
// --version flag must print the exact same 5-field block as the `version`
// subcommand (GAP-045).
func TestVersionFlagMatchesVersionSubcommand(t *testing.T) {
	flagCmd := newRootCommand()
	var flagOut bytes.Buffer
	flagCmd.SetOut(&flagOut)
	flagCmd.SetErr(io.Discard)
	flagCmd.SetArgs([]string{"--version"})
	if err := flagCmd.Execute(); err != nil {
		t.Fatalf("--version returned error: %v", err)
	}

	subCmd := newRootCommand()
	var subOut bytes.Buffer
	subCmd.SetOut(&subOut)
	subCmd.SetErr(io.Discard)
	subCmd.SetArgs([]string{"version"})
	if err := subCmd.Execute(); err != nil {
		t.Fatalf("version subcommand returned error: %v", err)
	}

	if flagOut.String() != subOut.String() {
		t.Errorf("--version output differs from version subcommand\n--version:\n%s\nversion subcommand:\n%s", flagOut.String(), subOut.String())
	}

	lines := strings.Split(strings.TrimRight(flagOut.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("--version printed %d lines, want 5 (bunker <ver>, commit, built, go version, platform): %q", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "bunker ") {
		t.Errorf("first line = %q, want prefix \"bunker \"", lines[0])
	}
}

// TestRootCommandRegistersLifecycleCommands asserts the GAP-071 CLI surface is
// actually reachable: `bunker stop`, `bunker start` and `bunker restart` are
// registered on the root command (a command file that is never registered is a
// dead command) and each prints its usage through the real root invocation.
func TestRootCommandRegistersLifecycleCommands(t *testing.T) {
	root := newRootCommand()
	registered := map[string]bool{}
	for _, c := range root.Commands() {
		registered[c.Name()] = true
	}

	for _, name := range []string{"stop", "start", "restart"} {
		t.Run(name, func(t *testing.T) {
			if !registered[name] {
				t.Fatalf("command %q is not registered on the root command; registered: %v", name, keysOf(registered))
			}

			// Drive the REAL root invocation so the registered command (not a
			// freshly built one) is what prints.
			root := newRootCommand()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(io.Discard)
			root.SetArgs([]string{name, "--help"})
			if err := root.Execute(); err != nil {
				t.Fatalf("`bunker %s --help` returned an error: %v", name, err)
			}

			help := out.String()
			if !strings.Contains(help, "Usage:") {
				t.Errorf("`bunker %s --help` printed no usage:\n%s", name, help)
			}
			if !strings.Contains(help, "bunker "+name+" <agent-id>") {
				t.Errorf("`bunker %s --help` does not show 'bunker %s <agent-id>':\n%s", name, name, help)
			}
			if !strings.Contains(help, "--server") {
				t.Errorf("`bunker %s --help` does not list --server:\n%s", name, help)
			}
		})
	}
}

// TestFlagUsagePlaceholdersRenderValuelessOrPathLike guards GAP-086: a
// backticked span inside a flag's usage string is promoted by cobra's
// UnquoteUsage into the flag's rendered VALUE placeholder, so `--version`
// once rendered as "--version bunker version" (looking like it takes an
// argument) and `--config` as "--config bunker systemd install --config"
// (hiding which form takes a path). The fix keeps code spans out of flag
// usage strings; this test walks the whole registered command tree, renders
// each command's usage, and asserts every flag line carries either a
// single-token placeholder (path-like, e.g. "string") or none at all.
func TestFlagUsagePlaceholdersRenderValuelessOrPathLike(t *testing.T) {
	root := newRootCommand()
	commands := []*cobra.Command{root}
	for _, c := range root.Commands() {
		commands = append(commands, c)
	}

	for _, cmd := range commands {
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(io.Discard)
		if err := cmd.Usage(); err != nil {
			t.Fatalf("%s: Usage() returned error: %v", cmd.CommandPath(), err)
		}

		for _, line := range strings.Split(buf.String(), "\n") {
			name, placeholder, ok := parseFlagUsageLine(line)
			if !ok {
				continue
			}
			if strings.ContainsAny(placeholder, " \t") {
				t.Errorf("%s: flag --%s renders a prose placeholder %q (a backticked span leaked out of the usage string into UnquoteUsage):\n%s",
					cmd.CommandPath(), name, placeholder, line)
			}
		}
	}

	// Explicit GAP-086 regressions on the two originally affected flags.
	rootHelp := renderHelp(t, root)
	if strings.Contains(rootHelp, "--version bunker version") {
		t.Errorf("root --help renders --version with a value placeholder; --version must be valueless:\n%s", rootHelp)
	}
	if !regexp.MustCompile(`(?m)^\s+--version\s{2,}Print the bunker version`).MatchString(rootHelp) {
		t.Errorf("root --help does not render --version valueless directly beside its description:\n%s", rootHelp)
	}
	if strings.Contains(rootHelp, "--config bunker systemd install --config") {
		t.Errorf("root --help renders --config with the prose span as its value placeholder:\n%s", rootHelp)
	}
}

// renderHelp executes the real root invocation for a subcommand (or the root
// itself) and returns the rendered help text.
func renderHelp(t *testing.T, root *cobra.Command, args ...string) string {
	t.Helper()
	root = newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs(append(args, "--help"))
	if err := root.Execute(); err != nil {
		t.Fatalf("`bunker %s --help` returned an error: %v", strings.Join(args, " "), err)
	}
	return out.String()
}

// parseFlagUsageLine parses one rendered usage line like
// "      --config string   CLI config file. ..." or
// "  -h, --help   help for bunker". It reports the flag name, the rendered
// placeholder ("" when the flag renders valueless), and whether the line is a
// flag line at all. Line shape per pflag's FlagUsages: two leading spaces, an
// optional "-X, " shorthand, the flag name, an optional single-token
// placeholder, then two-or-more spaces before the usage text.
func parseFlagUsageLine(line string) (name, placeholder string, ok bool) {
	rest := strings.TrimSpace(line)
	if !strings.HasPrefix(rest, "--") {
		return "", "", false
	}
	rest = rest[2:]
	// Drop a shorthand if this is a merged "-X, --name" line.
	if i := strings.Index(rest, ", --"); i >= 0 {
		rest = rest[i+4:]
	}
	end := strings.IndexAny(rest, " \t")
	if end < 0 {
		return rest, "", true
	}
	name = rest[:end]
	// The gap between the name and the usage text is two or more spaces; a
	// placeholder is whatever single token sits in between.
	body := rest[end:]
	desc := strings.Index(body, "  ")
	if desc < 0 {
		return name, "", true
	}
	return name, strings.TrimSpace(body[:desc]), true
}

// keysOf returns the map's keys sorted, for deterministic failure messages.
func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
