package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestNewerCLIHintOnNewerReleaseSubcommand pins the DF-BUNKER-69 contract for
// the branch the task names directly: `bunker renew` on an old client (renew
// first shipped in release v0.2.0 — derived from git history, see
// newercli_releases_gen.go). The error must still fail LOUD (non-nil error →
// non-zero exit), and must carry the actionable hint naming the release
// requirement and the documented install path, instead of the bare
// `unknown command` verdict.
func TestNewerCLIHintOnNewerReleaseSubcommand(t *testing.T) {
	root := newRootCommand()
	// Simulate the OLD released client: this build predates the subcommand,
	// so its registration is absent. RemoveCommand reproduces exactly that —
	// same tree minus one command, no stubs.
	var renew *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "renew" {
			renew = c
		}
	}
	if renew == nil {
		t.Fatalf("premise broken: renew not registered in this build")
	}
	root.RemoveCommand(renew)
	if _, ok := newerCLISubcommandReleases["renew"]; !ok {
		t.Fatalf("premise broken: renew missing from newerCLISubcommandReleases")
	}

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	// The invocation the board row observed: bare `bunker renew`. (Adding
	// subcommand flags here would hit cobra's flag parser first — an old
	// client fails on "unknown flag" before arg validation — so the hint
	// contract is pinned on the exact reported invocation.)
	root.SetArgs([]string{"renew"})
	err := root.Execute()
	if err == nil {
		t.Fatal("bunker renew on a client without the subcommand must fail (non-zero exit), got nil error")
	}
	// Log the full hint so `go test -v` output carries the exact text an old
	// released client would print.
	t.Logf("old-client bunker renew error:\n%v", err)
	msg := err.Error()
	for _, want := range []string{
		`unknown command "renew" for "bunker"`,
		"this client build does not have it",
		"first shipped in release v0.2.0",
		"Upgrade your bunker CLI",
		"curl -fsSL https://github.com/deployBunker/bunker/releases/latest/download/install.sh | sh",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("hint text missing %q in:\n%s", want, msg)
		}
	}
}

// TestNewerCLIUnknownWordStaysBare pins the fail-loud arm: a word that is NOT
// a bunker subcommand must keep cobra's bare `unknown command` verdict — no
// upgrade hint, no invented install path.
func TestNewerCLIUnknownWordStaysBare(t *testing.T) {
	if _, ok := newerCLISubcommandReleases["definitely-not-a-bunker-subcommand"]; ok {
		t.Fatal("premise broken: test word collides with a real subcommand entry")
	}
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"definitely-not-a-bunker-subcommand"})
	err := root.Execute()
	if err == nil {
		t.Fatal("unknown word must fail, got nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, `unknown command "definitely-not-a-bunker-subcommand" for "bunker"`) {
		t.Errorf("bare verdict missing from:\n%s", msg)
	}
	for _, banned := range []string{"Upgrade your bunker CLI", "install.sh", "first shipped in"} {
		if strings.Contains(msg, banned) {
			t.Errorf("unknown word must not carry the hint (found %q) in:\n%s", banned, msg)
		}
	}
}

// TestNewerCLIMapCoversRegisteredSubcommands is the maintenance tripwire: the
// generated table must name exactly the root subcommand set of this build —
// a new command without a map entry (or a removed command left behind) fails
// here and tells the author to re-run the generator.
func TestNewerCLIMapCoversRegisteredSubcommands(t *testing.T) {
	registered := registeredSubcommands(newRootCommand())
	if len(registered) == 0 {
		t.Fatal("no subcommands registered; root command construction changed?")
	}
	for name := range registered {
		if _, ok := newerCLISubcommandReleases[name]; !ok {
			t.Errorf("subcommand %q registered but missing from newerCLISubcommandReleases; re-run: go run cmd/bunker/gen_newercli_map.go", name)
		}
	}
	for name := range newerCLISubcommandReleases {
		if !registered[name] {
			t.Errorf("newerCLISubcommandReleases names %q but no such subcommand is registered; re-run: go run cmd/bunker/gen_newercli_map.go", name)
		}
	}
}

// findRepoReadme locates the repository README.md walking up from the package
// directory (go test runs with cwd = package dir).
func findRepoReadme(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "README.md")
		if b, err := os.ReadFile(candidate); err == nil && strings.Contains(string(b), "### Install") {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("repository README.md (with an Install section) not found above the package dir")
	return ""
}

// TestNewerCLIHintInstallPathMatchesReadme pins acceptance 2: the hint's
// install command must appear VERBATIM in the README's install section, so a
// docs-side edit of the install path breaks this test instead of letting the
// hint drift from the documented path.
func TestNewerCLIHintInstallPathMatchesReadme(t *testing.T) {
	readme := findRepoReadme(t)
	b, err := os.ReadFile(readme)
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	if !strings.Contains(string(b), newerCLIHintInstallPath+"\n") {
		t.Errorf("README %s does not contain the hint install path verbatim:\n%s", readme, newerCLIHintInstallPath)
	}
}
