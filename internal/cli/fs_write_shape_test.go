package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/fsclient"
)

// DF-BUNKER-100: the mount's write shape is stated where a USER will meet it.
//
// The gap this row closes was not a code bug: inside a bunker-fs mount `git
// init` failed and `>`/`>>` onto an existing path were refused, the mount's
// pitch in the README could not be exercised as written, and NO surface said
// so — the operator's only feedback was the kernel's "Operation not supported"
// (docs/dogfood/2026-10-16b-bunker-fs-surface.md).
//
// These cells are the pin. They render the REAL cobra command (so a flag or a
// Long that stops being printed fails here rather than in someone's session)
// and read the documentation corpus from disk (so a doc edit that drops the
// contract fails the build that would publish it). The phrases asserted are the
// contract's four facts: whole-file publish is the shape that works, redirection
// and append are refused, git cannot run on the mount, and the remediation is
// named.

// writeShapeDocSurfaces are the documentation surfaces that must carry the
// contract, by repository-relative path. Every one of them is a page a reader
// arrives at before they hit the refusal.
var writeShapeDocSurfaces = []string{
	"README.md",
	"docs/mount-drivers.md",
	"docs/spec/BFS-005-client-cache-and-diff.md",
	"docs/dogfood/2026-10-16b-bunker-fs-surface.md",
}

// TestFSMountHelpRendersTheWriteShapeContract is the acceptance criterion: the
// contract is in the help text of `bunker fs mount`, not only in a doc page.
func TestFSMountHelpRendersTheWriteShapeContract(t *testing.T) {
	var out bytes.Buffer
	cmd := newFSMountCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Help(); err != nil {
		t.Fatalf("render `bunker fs mount --help`: %v", err)
	}
	help := out.String()

	// The whole contract, verbatim from its single source.
	if !strings.Contains(help, fsclient.WriteShapeContract) {
		t.Error("`bunker fs mount --help` must print the write-shape contract verbatim (one source, printed not paraphrased)")
	}
	for _, want := range []string{
		"WHOLE FILES only",
		"REDIRECTION AND IN-PLACE EDITS ARE NOT SUPPORTED",
		"EOPNOTSUPP",
		"GIT DOES NOT WORK ON THE MOUNT",
		"chmod on .git/config.lock failed: Operation not supported",
		"REMEDIATION",
		"cp/tee/editor save",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("`bunker fs mount --help` does not state %q; an operator who reads the help must learn the write shape before they mount", want)
		}
	}
}

// TestFSGroupHelpPointsAtTheWriteShape: someone who runs `bunker fs` (or reads
// the group page) must be told the mount's write shape before they mount it.
func TestFSGroupHelpPointsAtTheWriteShape(t *testing.T) {
	var out bytes.Buffer
	cmd := NewFSCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Help(); err != nil {
		t.Fatalf("render `bunker fs --help`: %v", err)
	}
	help := out.String()
	if !strings.Contains(help, "WRITE SHAPE") {
		t.Error("`bunker fs --help` must name the mount's write shape")
	}
	if !strings.Contains(help, "git") || !strings.Contains(help, "EOPNOTSUPP") {
		t.Error("the group help must name what is refused (git, EOPNOTSUPP), not only that a contract exists")
	}
}

// TestDocsCarryTheWriteShapeContract reads the corpus from disk: the contract
// must be stated in the README's mount section and in every doc page this row
// updated, with the remediation named. It is a docs pin, not a style check —
// each phrase is one of the four facts a reader needs.
func TestDocsCarryTheWriteShapeContract(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, rel := range writeShapeDocSurfaces {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		doc := strings.ToLower(string(raw))
		for _, want := range []string{
			"whole files only", // the shape that works
			"eopnotsupp",       // what the caller sees
			"git",              // the workflow that cannot run on the mount
			"cp",               // the remediation, by name
		} {
			if !strings.Contains(doc, want) {
				t.Errorf("%s does not state the write-shape contract (%q missing): the contract belongs in the docs BEFORE the refusal, not only in the error", rel, want)
			}
		}
	}
}

// TestMountBannerStatesTheWriteShape is the moment-of-mount surface: the banner
// is the one piece of output a user reads while their mount is coming up, so it
// must state the contract there and then — before the first refused write.
func TestMountBannerStatesTheWriteShape(t *testing.T) {
	banner := strings.Join(writeShapeBanner(), "\n")
	for _, want := range []string{
		"WHOLE FILES ONLY",
		"`>`/`>>`",
		"EOPNOTSUPP",
		"git",
		"cp/tee/editor save",
		"bunker fs mount --help",
	} {
		if !strings.Contains(banner, want) {
			t.Errorf("the mount banner does not state %q; that is the output a user sees while their mount comes up, and after dogfood run 27 it is the surface this row exists for", want)
		}
	}
}

// TestDocsNameTheRemediationForGitOnTheMount is the second half of the pin, and
// the half that matters to a user: what to DO. A doc that says "git is not
// supported" without saying "run it in the agent / clone elsewhere and publish
// whole files" leaves the reader exactly where dogfood run 27 was.
func TestDocsNameTheRemediationForGitOnTheMount(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, rel := range []string{"README.md", "docs/mount-drivers.md"} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		doc := string(raw)
		if !strings.Contains(doc, "agent") {
			t.Errorf("%s must name the remediation (run git in the agent), not only the refusal", rel)
		}
		if !strings.Contains(doc, "whole file") && !strings.Contains(doc, "whole-file") && !strings.Contains(doc, "WHOLE FILE") {
			t.Errorf("%s must say to publish whole files as the way to put content on the mount", rel)
		}
	}
}

// TestFSStatusExplainsARefusedWriteShape: `bunker fs status` shows the refusal
// count and the path it happened on — and now the shape that works, beside
// them. A count with no explanation is the surface this row found wanting.
func TestFSStatusExplainsARefusedWriteShape(t *testing.T) {
	st := &fsclient.Status{}
	st.WriteShape = fsclient.WriteShapeState{RefusalsTotal: 2, Last: "src/main.go: write off=0"}

	var buf bytes.Buffer
	printStatus(&buf, st)
	out := buf.String()
	if !strings.Contains(out, "src/main.go: write off=0") {
		t.Errorf("the status must name the path that was refused, got:\n%s", out)
	}
	if !strings.Contains(out, fsclient.WriteShapeNotice) {
		t.Errorf("the status must print the write-shape contract beside the refusals, got:\n%s", out)
	}

	// NEGATIVE control: a mount that has refused nothing must not grow a
	// paragraph nobody asked for.
	clean := &fsclient.Status{}
	var cleanBuf bytes.Buffer
	printStatus(&cleanBuf, clean)
	if strings.Contains(cleanBuf.String(), fsclient.WriteShapeNotice) {
		t.Errorf("a healthy mount must not print the write-shape contract, got:\n%s", cleanBuf.String())
	}
}
