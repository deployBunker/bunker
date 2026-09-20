// Command docs-drift fails when the release documentation drifts from the CLI
// that `go install ...@latest` actually installs (GAP-081).
//
// It gathers the newest release tag, the CLI surface of that tag's tree and the
// number of commits after it straight from git (no network, no build), then
// evaluates the README/CHANGELOG rules in internal/docscheck:
//
//   - every command the README documents exists in the newest release tag or
//     its block is marked "requires a build from HEAD";
//   - no release tag older than the newest one is named in the README;
//   - the CHANGELOG carries an `## Unreleased` section while commits exist
//     after the newest release tag;
//   - every long flag used in a docs/*.md example invocation is one the
//     working tree's CLI can accept for that command (GAP-087: the
//     integration walkthrough documented `spawn --name/--mem`, which the
//     shipped CLI rejects — that class of drift used to be invisible to CI).
//
// A tag-less checkout (shallow clone, `act` run, source tarball) skips with a
// warning instead of failing, matching the CI version-authority and tag-build
// steps.
//
// Usage: go run ./cmd/docs-drift [-dir <repo>]
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/deployBunker/bunker/internal/docscheck"
)

func main() {
	dir := flag.String("dir", ".", "repository directory to check")
	flag.Parse()

	if err := run(*dir); err != nil {
		fmt.Fprintf(os.Stderr, "docs-drift: %v\n", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	facts, err := docscheck.Gather(dir)
	if errors.Is(err, docscheck.ErrNoTags) {
		fmt.Println("::warning::no release tags in checkout (act/shallow/fork) — docs drift check skipped " +
			"(hosted CI fetches full depth and enforces it)")
		return nil
	}
	if err != nil {
		return err
	}

	problems, err := docscheck.Verify(facts.Root, facts)
	if err != nil {
		return err
	}

	names := facts.Surface.Names()
	if len(problems) == 0 {
		fmt.Printf("docs OK: README/CHANGELOG/docs match %s (%d released commands, %d commit(s) after the tag)\n",
			facts.LatestTag, len(names), facts.PostTagCommits)
		fmt.Printf("  released: %v\n", names)
		return nil
	}

	fmt.Fprintf(os.Stderr, "%d documentation drift problem(s) against the newest release tag %s:\n",
		len(problems), facts.LatestTag)
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "  - %s\n", p)
	}
	fmt.Fprintf(os.Stderr, "released commands at %s: %v\n", facts.LatestTag, names)
	return fmt.Errorf("documentation does not match the released CLI surface")
}
