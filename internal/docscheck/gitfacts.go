package docscheck

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNoTags reports that the checkout has no release tags — a shallow clone, an
// `act` run, a fresh fork or a source tarball. Callers skip the tag-derived
// rules with a visible warning instead of failing, exactly like the CI
// version-authority and tag-build steps do.
var ErrNoTags = errors.New("no release tags in this checkout")

// GitFacts are the repository facts the release-drift check derives from git.
type GitFacts struct {
	// Root is the repository top-level directory.
	Root string
	// LatestTag is `git describe --tags --abbrev=0`.
	LatestTag string
	// Surface is the CLI surface of the tree AT LatestTag — read out of the
	// tag with `git show`, never from the working tree, so a HEAD-only command
	// cannot present itself as released.
	Surface Surface
	// TreeSurface is the CLI surface of the WORKING TREE (HEAD content read
	// with `git show HEAD:…`). The group-doc-coverage rule uses it: a
	// subcommand that is about to ship must be documented by the same tree
	// that ships it, and a tag-based surface cannot see a subcommand that
	// has no release tag yet (GAP-088: `audit status` was absent from
	// v0.1.4, so a tag-based group check could never flag the doc gap).
	TreeSurface Surface
	// TreeFlags is the per-command long-flag registry of the WORKING TREE
	// (same HEAD sources TreeSurface is parsed from). The docs-flag-surface
	// rule checks against it (GAP-087): a documented flag must be accepted
	// by the tree that ships the docs, and a tag-based registry cannot see
	// a flag that has no release tag yet.
	TreeFlags map[string]map[string]bool
	// PostTagCommits is `git rev-list --count <LatestTag>..HEAD`.
	PostTagCommits int
}

// Gather collects the facts for the repository containing dir.
func Gather(dir string) (GitFacts, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return GitFacts{}, fmt.Errorf("git not available: %w", err)
	}
	root, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return GitFacts{}, err
	}
	root = strings.TrimSpace(root)

	tag, err := git(root, "describe", "--tags", "--abbrev=0")
	if err != nil {
		// `git describe` exits non-zero both when the checkout is shallow/tagless
		// and when it is not a repository at all; distinguish by checking for
		// tags directly so a genuine repo error is not mistaken for "skip".
		if tags, tagErr := git(root, "tag", "--list"); tagErr == nil && strings.TrimSpace(tags) == "" {
			return GitFacts{}, ErrNoTags
		}
		return GitFacts{}, fmt.Errorf("resolve newest release tag: %w", err)
	}
	tag = strings.TrimSpace(tag)

	surface, err := TagSurface(root, tag)
	if err != nil {
		return GitFacts{}, err
	}

	treeSurface, err := TreeSurface(root)
	if err != nil {
		return GitFacts{}, err
	}
	treeFlags, err := TreeFlags(root)
	if err != nil {
		return GitFacts{}, err
	}

	count, err := git(root, "rev-list", "--count", tag+"..HEAD")
	if err != nil {
		return GitFacts{}, fmt.Errorf("count commits after %s: %w", tag, err)
	}
	var commits int
	if _, err := fmt.Sscanf(strings.TrimSpace(count), "%d", &commits); err != nil {
		return GitFacts{}, fmt.Errorf("parse commit count %q: %w", strings.TrimSpace(count), err)
	}

	return GitFacts{Root: root, LatestTag: tag, Surface: surface, TreeSurface: treeSurface, TreeFlags: treeFlags, PostTagCommits: commits}, nil
}

// Verify reads the two documents in root plus every registered group's doc
// page and evaluates every rule against facts.
func Verify(root string, facts GitFacts) ([]Problem, error) {
	readmePath := filepath.Join(root, "README.md")
	changelogPath := filepath.Join(root, "CHANGELOG.md")
	readme, err := os.ReadFile(readmePath)
	if err != nil {
		return nil, fmt.Errorf("read README: %w", err)
	}
	changelog, err := os.ReadFile(changelogPath)
	if err != nil {
		return nil, fmt.Errorf("read CHANGELOG: %w", err)
	}
	// Group doc pages: read the ones the registry names that exist on disk.
	// A page that does not exist is skipped here — a missing doc FILE is a
	// different failure than a gap inside an existing page — but read errors
	// other than not-exist surface loudly.
	groupDocs := map[string]string{}
	for _, rule := range GroupDocCoverageRules {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rule.DocPath)))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", rule.DocPath, err)
		}
		groupDocs[rule.Group] = string(b)
	}
	// Doc pages: docs/*.md carry runnable example invocations (GAP-087).
	// Read them all so the docs-flag-surface rule sees them; a page that
	// disappears is simply not checked. Read errors other than not-exist
	// surface loudly.
	docs := map[string]string{}
	docMatches, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		return nil, fmt.Errorf("list docs/*.md: %w", err)
	}
	for _, docPath := range docMatches {
		b, err := os.ReadFile(docPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", docPath, err)
		}
		rel, err := filepath.Rel(root, docPath)
		if err != nil {
			rel = docPath
		}
		docs[filepath.ToSlash(rel)] = string(b)
	}
	return Check(Input{
		ReadmePath:     "README.md",
		ChangelogPath:  "CHANGELOG.md",
		Readme:         string(readme),
		Changelog:      string(changelog),
		LatestTag:      facts.LatestTag,
		Surface:        facts.Surface,
		PostTagCommits: facts.PostTagCommits,
		GroupDocs:      groupDocs,
		TreeSurface:    facts.TreeSurface,
		Docs:           docs,
		TreeFlags:      facts.TreeFlags,
	}), nil
}

// TreeSurface derives the CLI surface of the WORKING TREE without checking
// anything out: it reads `cmd/bunker/main.go` and the `internal/cli` sources
// of the current HEAD content straight from the object database (git show
// HEAD:<path>), so the check is deterministic under dirty worktrees and
// identical in behavior to TagSurface. Callers use it for rules that govern
// the tree that is about to ship rather than the newest release tag.
func TreeSurface(dir string) (Surface, error) {
	return tagSurfaceAt(dir, "HEAD")
}

// TreeFlags derives the per-command long-flag registry of the WORKING TREE
// from the same HEAD sources TreeSurface reads (GAP-087). Same determinism
// guarantees: object-database reads, no checkout, no build.
func TreeFlags(dir string) (map[string]map[string]bool, error) {
	mainSrc, files, err := treeSources(dir)
	if err != nil {
		return nil, err
	}
	return FlagsFor(mainSrc, files), nil
}

// TagSurface derives the CLI surface of the tree at ref without checking it
// out: it reads `cmd/bunker/main.go` and the `internal/cli` sources straight
// out of the object database.
func TagSurface(dir, ref string) (Surface, error) {
	return tagSurfaceAt(dir, ref)
}

// tagSurfaceAt is the shared reader behind TagSurface and TreeSurface.
func tagSurfaceAt(dir, ref string) (Surface, error) {
	mainSrc, files, err := treeSourcesAt(dir, ref)
	if err != nil {
		return Surface{}, err
	}
	return ParseSurface(mainSrc, files)
}

// treeSources reads cmd/bunker/main.go plus the internal/cli sources of the
// WORKING TREE (HEAD content).
func treeSources(dir string) (string, map[string]string, error) {
	return treeSourcesAt(dir, "HEAD")
}

// treeSourcesAt reads cmd/bunker/main.go and every non-test internal/cli
// source at ref straight out of the object database.
func treeSourcesAt(dir, ref string) (string, map[string]string, error) {
	mainSrc, err := git(dir, "show", ref+":cmd/bunker/main.go")
	if err != nil {
		return "", nil, fmt.Errorf("read cmd/bunker/main.go at %s: %w", ref, err)
	}
	listing, err := git(dir, "ls-tree", "-r", "--name-only", ref, "--", "internal/cli")
	if err != nil {
		return "", nil, fmt.Errorf("list internal/cli at %s: %w", ref, err)
	}
	files := map[string]string{}
	for _, path := range strings.Fields(listing) {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := git(dir, "show", ref+":"+path)
		if err != nil {
			return "", nil, fmt.Errorf("read %s at %s: %w", path, ref, err)
		}
		files[path] = src
	}
	return mainSrc, files, nil
}

// git runs git in dir and returns stdout, folding stderr into the error.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}
