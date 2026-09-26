package webdav

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The fixture is a small source tree: one collection with two files, one file
// at the root, and one empty collection.
const (
	fixtureMainBody = "package main\n\nfunc main() {}\n"
	fixtureUtilBody = "package main\n\nfunc util() {}\n"
	fixtureReadme   = "# fixture\n"
)

func fixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "src", "main.go"), fixtureMainBody)
	mustWrite(t, filepath.Join(root, "src", "util.go"), fixtureUtilBody)
	mustWrite(t, filepath.Join(root, "README.md"), fixtureReadme)
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	return root
}

// withGitFixture adds the minimum a git work tree has (HEAD + a loose ref), so
// the rev token path is exercised without executing git.
func withGitFixture(t *testing.T, root, head string) {
	t.Helper()
	mustWrite(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(root, ".git", "refs", "heads", "main"), head+"\n")
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newTestHandler(t *testing.T, opts ...func(*Config)) *Handler {
	t.Helper()
	cfg := Config{Root: fixtureTree(t), Build: "test-build"}
	for _, o := range opts {
		o(&cfg)
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// do drives one request through the handler exactly as a listener would.
func do(t *testing.T, h http.Handler, method, target string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// treeDigest is a content digest of the whole served tree, used to prove an
// operation mutated nothing (E-4's read-only invariant, A-9).
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	type entry struct {
		path string
		body string
	}
	var entries []entry
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if info.IsDir() {
			entries = append(entries, entry{path: rel + "/"})
			return nil
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		entries = append(entries, entry{path: rel, body: string(b)})
		return nil
	})
	if err != nil {
		t.Fatalf("digest walk: %v", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(e.path)
		sb.WriteString("\x00")
		sb.WriteString(e.body)
		sb.WriteString("\x00")
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}
