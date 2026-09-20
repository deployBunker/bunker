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

	count, err := git(root, "rev-list", "--count", tag+"..HEAD")
	if err != nil {
		return GitFacts{}, fmt.Errorf("count commits after %s: %w", tag, err)
	}
	var commits int
	if _, err := fmt.Sscanf(strings.TrimSpace(count), "%d", &commits); err != nil {
		return GitFacts{}, fmt.Errorf("parse commit count %q: %w", strings.TrimSpace(count), err)
	}

	return GitFacts{Root: root, LatestTag: tag, Surface: surface, TreeSurface: treeSurface, PostTagCommits: commits}, nil
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

// TagSurface derives the CLI surface of the tree at ref without checking it
// out: it reads `cmd/bunker/main.go` and the `internal/cli` sources straight
// out of the object database.
func TagSurface(dir, ref string) (Surface, error) {
	return tagSurfaceAt(dir, ref)
}

// tagSurfaceAt is the shared reader behind TagSurface and TreeSurface.
func tagSurfaceAt(dir, ref string) (Surface, error) {
	mainSrc, err := git(dir, "show", ref+":cmd/bunker/main.go")
	if err != nil {
		return Surface{}, fmt.Errorf("read cmd/bunker/main.go at %s: %w", ref, err)
	}
	listing, err := git(dir, "ls-tree", "-r", "--name-only", ref, "--", "internal/cli")
	if err != nil {
		return Surface{}, fmt.Errorf("list internal/cli at %s: %w", ref, err)
	}
	files := map[string]string{}
	for _, path := range strings.Fields(listing) {
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := git(dir, "show", ref+":"+path)
		if err != nil {
			return Surface{}, fmt.Errorf("read %s at %s: %w", path, ref, err)
		}
		files[path] = src
	}
	return ParseSurface(mainSrc, files)
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
