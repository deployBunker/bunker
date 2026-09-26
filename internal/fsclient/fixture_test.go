package fsclient

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/davserve"
)

// The integration fixtures drive the client against the REAL landed server
// surface (internal/server/webdav) served on a loopback listener — the same
// handler bunkerd mounts, with none of the daemon's agent lifecycle.
const (
	fixMainBody   = "package main\n\nfunc main() {}\n"
	fixUtilBody   = "package main\n\nfunc util() {}\n"
	fixReadmeBody = "# fixture\n"
)

// fixtureTree writes a small tree: one collection with two files, one file at the
// root, one empty collection, and a nested directory so the tree walk is not
// flat.
func fixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "src", "main.go"), fixMainBody)
	mustWrite(t, filepath.Join(root, "src", "util.go"), fixUtilBody)
	mustWrite(t, filepath.Join(root, "src", "deep", "nested.go"), "package deep\n")
	mustWrite(t, filepath.Join(root, "README.md"), fixReadmeBody)
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureEndpoint starts the landed surface over a fresh fixture tree and returns
// (client, server root, endpoint).
func fixtureEndpoint(t *testing.T, opts ...func(*Options)) (*Client, string, *davserve.Server) {
	t.Helper()
	root := fixtureTree(t)
	srv, err := davserve.Serve(root, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("davserve.Serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	o := Options{BaseURL: srv.URL, Concurrency: 8, OpTimeout: 10 * time.Second, BindTimeout: 3 * time.Second}
	for _, f := range opts {
		f(&o)
	}
	c, err := NewClient(o)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, root, srv
}
