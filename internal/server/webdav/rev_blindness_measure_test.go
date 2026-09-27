package webdav

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-048's defect, AS A MEASUREMENT, on a real git work tree.
//
// The row's claim is a measurement, not an opinion: on a git tree, an
// out-of-band edit that is NOT made through this surface and NOT committed
// leaves the served revision token byte-identical, while the file's bytes
// change. This arm takes that measurement with a real `git init` repository
// rather than a hand-written .git fixture, and it records the token, the file
// digests and the memo-refresh evidence (t.Logf) so the numbers in the evidence
// document are the numbers this test produced.
//
// The second half proves the measurement is not simply "the token never moves":
// the one class the git kind DOES cover — HEAD's ref moving — moves it.
// ---------------------------------------------------------------------------

// TestOutOfBandWorkingTreeEditMovesNothingTheRevisionPollCanSee is the RED, kept
// as a pinned fact: the defect is not that the token moves wrongly, it is that
// nothing moves and the mechanism never said so. That reporting is the client's
// half (see internal/fsclient's coverage arms); what is measured here is the
// server-side token.
func TestOutOfBandWorkingTreeEditMovesNothingTheRevisionPollCanSee(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH: this arm measures a real work tree, not a hand-written .git")
	}

	root := t.TempDir()
	file := filepath.Join(root, "src", "main.go")
	mustWrite(t, file, "package main\n\nfunc main() {}\n")
	runGitBFS048(t, gitBin, root, "init", "-q", "-b", "main")
	runGitBFS048(t, gitBin, root, "add", "-A")
	runGitBFS048(t, gitBin, root, "-c", "user.email=bfs048@example.invalid", "-c", "user.name=bfs048",
		"-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")

	h := newTestHandler(t, func(c *Config) { c.Root = root })
	headBefore := gitHeadBFS048(root)
	if headBefore == "" {
		t.Fatal("the fixture is not served as a git tree: no resolved HEAD")
	}

	tokenBefore := revHeaderBFS048(t, h)
	hashBefore := sha256FileBFS048(t, file)
	t.Logf("BFS-048 RED: HEAD=%s rev=%s file=%s", headBefore, tokenBefore, hashBefore)
	if want := "git:" + headBefore; tokenBefore != want {
		t.Fatalf("served revision = %q, want %q (the resolved HEAD) before the edit", tokenBefore, want)
	}

	// THE OUT-OF-BAND EDIT: no WebDAV request, no commit — a working-tree write
	// of exactly the shape an editor, a build or another agent makes.
	mustWrite(t, file, "package main\n\nfunc main() { /* edited out of band, uncommitted */ }\n")
	hashAfter := sha256FileBFS048(t, file)
	if hashAfter == hashBefore {
		t.Fatalf("the out-of-band edit did not change the file's bytes: %s", hashAfter)
	}

	// Wait out the gitRevCacheTTL memo, so the token below is recomputed from
	// disk rather than replayed from the memo cache. revAt advancing is the
	// evidence that the HEAD really was re-read.
	revAtBefore := h.tree.revAt
	time.Sleep(gitRevCacheTTL + 20*time.Millisecond)
	tokenAfter := revHeaderBFS048(t, h)
	if !h.tree.revAt.After(revAtBefore) {
		t.Fatal("the revision memo did not refresh: this arm would be measuring a cached read, not the tree")
	}
	t.Logf("BFS-048 RED: after the out-of-band edit HEAD=%s rev=%s file=%s",
		gitHeadBFS048(root), tokenAfter, hashAfter)

	if hashBefore == hashAfter {
		t.Fatal("bytes did not change: the arm proves nothing")
	}
	if tokenAfter != tokenBefore {
		t.Fatalf("the revision moved for an uncommitted working-tree edit: %q -> %q", tokenBefore, tokenAfter)
	}
	// The token's own source is unchanged as well, which is WHERE the blindness
	// comes from: it is the ref file, not the working tree.
	if got := gitHeadBFS048(root); got != headBefore {
		t.Fatalf("HEAD moved without a commit: %q -> %q", headBefore, got)
	}

	// The other half: the class the git kind DOES cover still moves it, so the
	// arm above is a measurement of blindness and not of a frozen token.
	runGitBFS048(t, gitBin, root, "add", "-A")
	runGitBFS048(t, gitBin, root, "-c", "user.email=bfs048@example.invalid", "-c", "user.name=bfs048",
		"-c", "commit.gpgsign=false", "commit", "-q", "-m", "commit the edit")
	headAfter := gitHeadBFS048(root)
	if headAfter == headBefore {
		t.Fatalf("the commit did not move HEAD: %q", headAfter)
	}
	if got := sha256FileBFS048(t, file); got != hashAfter {
		t.Fatalf("committing changed the file's bytes: %s -> %s", hashAfter, got)
	}
	time.Sleep(gitRevCacheTTL + 20*time.Millisecond)
	tokenCommitted := revHeaderBFS048(t, h)
	t.Logf("BFS-048 control: after the commit HEAD=%s rev=%s file=%s", headAfter, tokenCommitted, hashAfter)
	if tokenCommitted != "git:"+headAfter {
		t.Fatalf("served revision = %q, want %q after the ref moved", tokenCommitted, "git:"+headAfter)
	}
	if tokenCommitted == tokenBefore {
		t.Fatalf("the revision did not move for the commit it DOES cover: %q", tokenCommitted)
	}
}

// revHeaderBFS048 reads the client-visible revision off a real response: the
// header every response carries (E-3), never an internal getter, so what is
// measured is what a poll would see.
func revHeaderBFS048(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := do(t, h, "OPTIONS", "/dav", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS /dav -> %d", rec.Code)
	}
	got := rec.Header().Get("X-Bunker-Rev")
	if got == "" {
		t.Fatal("no X-Bunker-Rev on the response: nothing to poll")
	}
	return got
}

// sha256FileBFS048 is the file's content identity as a reader of the tree would
// compute it, not as the server's cache would report it.
func sha256FileBFS048(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// gitHeadBFS048 resolves the on-disk HEAD the same way git does, so the arm can
// separate "the ref file is unchanged" from "the token is unchanged".
func gitHeadBFS048(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, ".git", "refs", "heads", "main"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(raw))
	if len(head) != 40 {
		return ""
	}
	return head
}

// runGitBFS048 runs one git command in dir and fails the test on error. HOME is
// pointed at a scratch directory and the system config is disabled so the arm
// cannot depend on the host's git setup.
func runGitBFS048(t *testing.T, gitBin, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command(gitBin, full...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+t.TempDir(),
		"GIT_TERMINAL_PROMPT=0",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}
