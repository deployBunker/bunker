package fsclient

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// BFS-048's other half, for the client's OWN documentation: the defect was a
// claim, and a claim lives in prose as well as in code. The client's spec said
// the revision poll's scope was "one call covers the whole tree revision, not
// one call per directory" — true for a counter tree and false for a git tree,
// where the token is HEAD. Correcting the Go comment while the spec kept
// claiming whole-tree coverage would have moved the lie, not removed it, so this
// arm pins the claim where the client publishes it: the spec must scope the poll
// to the DECLARED kind and must name the coverage report, and the old sentence
// must not come back in any document under docs/.
//
// It reads from disk exactly as a reviewer would, so the text it pins is the
// text that ships.

// claimInside is the verbatim sentence the client's spec carried before this
// row. Its return is the regression this arm exists to refuse.
const claimInside = "one call covers the whole tree revision"

func TestClientSpecScopesTheRevisionPollToTheDeclaredKind(t *testing.T) {
	root := repoRootBFS048(t)
	spec := readRepoFileBFS048(t, filepath.Join(root, "docs", "spec", "BFS-005-client-cache-and-diff.md"))

	if strings.Contains(spec, claimInside) {
		t.Fatalf("the client spec still claims unconditional whole-tree scope (%q): the revision poll is only as wide as the declared kind", claimInside)
	}
	// The replacement must actually SAY the narrower thing, and must name the
	// report a consumer reads — otherwise deleting the sentence would pass.
	for _, want := range []string{"extensions.rev.kind", "rev_gap", "rev_vouches_for", "uncommitted", "BFS-048"} {
		if !strings.Contains(spec, want) {
			t.Errorf("the client spec does not name %q: the scope of the revision poll is not stated where the client publishes it", want)
		}
	}
}

// TestNoDocumentClaimsWholeTreeRevisionCoverage applies the same rule to every
// NORMATIVE document under docs/, so the claim cannot simply move to a
// neighbouring file. Evidence artifacts (docs/evidence/**, docs/prd/evidence-*)
// are excluded deliberately: their job is to quote a defect verbatim — this
// row's own evidence file quotes the sentence below in the mutation that
// restores it — and a quotation is not a claim. (The guard failed on its first
// run for exactly that reason; the exclusion is the fix, not a concession.)
func TestNoDocumentClaimsWholeTreeRevisionCoverage(t *testing.T) {
	root := repoRootBFS048(t)
	docs := filepath.Join(root, "docs")
	var offenders []string
	scanned := 0
	err := filepath.WalkDir(docs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		if isEvidenceArtifactBFS048(docs, path) {
			return nil
		}
		scanned++
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(raw), claimInside) {
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
	// Non-vacuity: an exclusion rule that swallowed the whole tree would make
	// this arm pass by looking at nothing.
	if scanned < 10 {
		t.Fatalf("the guard scanned only %d documents: it is not looking where the claims live", scanned)
	}
	if len(offenders) > 0 {
		t.Fatalf("these documents still claim whole-tree revision scope: %v", offenders)
	}
}

// isEvidenceArtifactBFS048 reports whether a document is a measurement
// transcript rather than a normative one: anything under a directory named
// `evidence`, or a file named `evidence-*`.
func isEvidenceArtifactBFS048(docsRoot, path string) bool {
	rel, err := filepath.Rel(docsRoot, path)
	if err != nil {
		return false
	}
	if strings.HasPrefix(filepath.Base(path), "evidence-") {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "evidence" {
			return true
		}
	}
	return false
}

// repoRootBFS048 walks up from this file to the module root (the directory
// holding go.mod), so the arm reads the documents the repository ships rather
// than a path that only works from one working directory.
func repoRootBFS048(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("no go.mod above this test file: cannot locate the repository root")
	return ""
}

func readRepoFileBFS048(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
