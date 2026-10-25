package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// newerCLIHintInstallPath is the one-command install path exactly as the
// README documents it (README "Install", option 1). The hint quotes it
// verbatim so a stuck old client can act on the error alone.
const newerCLIHintInstallPath = "curl -fsSL https://github.com/deployBunker/bunker/releases/latest/download/install.sh | sh"

// newerCLIArgs validates root-command arguments (DF-BUNKER-69).
//
// Cobra's default legacyArgs rejects any first argument that is not a
// registered subcommand with a bare `unknown command "x" for "bunker"`. An
// old released client therefore tells an operator who asks for a subcommand
// that shipped in a newer release nothing beyond "unknown" — the command
// exists in the product, the docs document it, but this build is too old.
//
// This hook keeps cobra's verdict byte-for-byte for genuinely unknown words,
// and replaces it ONLY when the word is a documented bunker subcommand this
// build does not register: the error then names the release that introduced
// the subcommand and the documented install path. Fail-loud behavior is
// preserved: a misspelled or foreign word still errors non-zero with no hint.
//
// Wired as root.Args in newRootCommand (main.go) — the same verdict position
// legacyArgs occupies — so flag parsing, help and exit status are untouched.
func newerCLIArgs(cmd *cobra.Command, args []string) error {
	// Mirror legacyArgs: only the root command with positional args verdicts
	// here; subcommand argument validation stays with each subcommand.
	if cmd.HasParent() || len(args) == 0 {
		return nil
	}
	requested := args[0]
	for _, sub := range cmd.Commands() {
		if sub.Name() == requested {
			return nil
		}
	}
	if clause, ok := newerCLISubcommandReleases[requested]; ok {
		return fmt.Errorf("unknown command %q for %q%s\n\n"+
			"%q is a bunker subcommand, but this client build does not have it: it first shipped in %s.\n"+
			"Upgrade your bunker CLI, e.g.:\n  %s",
			requested, cmd.CommandPath(), newerCLISuggestions(cmd, requested),
			requested, clause, newerCLIHintInstallPath)
	}
	return fmt.Errorf("unknown command %q for %q%s", requested, cmd.CommandPath(), newerCLISuggestions(cmd, requested))
}

// newerCLISuggestions reproduces cobra's findSuggestions output (the
// "\n\nDid you mean this?" block) from the exported SuggestionsFor, so the
// replaced error keeps the exact suggestion text cobra would have printed.
func newerCLISuggestions(cmd *cobra.Command, arg string) string {
	if cmd.DisableSuggestions {
		return ""
	}
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	suggestions := cmd.SuggestionsFor(arg)
	if len(suggestions) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\nDid you mean this?\n")
	for _, s := range suggestions {
		_, _ = fmt.Fprintf(&sb, "\t%v\n", s)
	}
	return sb.String()
}

// registeredSubcommands returns the set of direct subcommand names this build
// actually registers on the root command.
func registeredSubcommands(root *cobra.Command) map[string]bool {
	names := make(map[string]bool, 64)
	for _, sub := range root.Commands() {
		names[sub.Name()] = true
	}
	return names
}
