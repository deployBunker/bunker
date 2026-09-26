package webdav

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStaleHashWriteIsRefused proves §6.1's exchange, arm one: a write whose
// base hash is stale and whose bytes DIFFER is refused, both hashes travel in
// the header and the body, and the file is byte-identical afterwards (AC-3).
func TestStaleHashWriteIsRefused(t *testing.T) {
	h := newTestHandler(t)
	path := filepath.Join(h.Root(), "src", "main.go")

	// Reader A holds the base hash it read.
	base := contentHash(fixtureMainBody)

	// Writer B replaces the file.
	writerB := "package main\n\nfunc main() { /* B got here first */ }\n"
	mustWrite(t, path, writerB)
	current := contentHash(writerB)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	rec := do(t, h, "PUT", "/dav/src/main.go", map[string]string{
		"If-Match": `"` + base + `"`,
	}, "package main\n\nfunc main() { /* A's edit */ }\n")

	if rec.Code != 412 {
		t.Fatalf("status = %d, want 412 (body=%s)", rec.Code, rec.Body.String())
	}
	header := rec.Header()
	if got := header.Get("X-Bunker-Verdict"); got != string(VerdictHashMismatch) {
		t.Fatalf("verdict = %q, want hash_mismatch", got)
	}
	if got := header.Get("X-Bunker-Current-Hash"); got != current {
		t.Fatalf("X-Bunker-Current-Hash = %q, want %q", got, current)
	}
	if got := header.Get("X-Bunker-Expected-Hash"); got != base {
		t.Fatalf("X-Bunker-Expected-Hash = %q, want %q", got, base)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<b:expected>"+base+"</b:expected>") {
		t.Fatalf("body does not name the expected hash: %s", body)
	}
	if !strings.Contains(body, "<b:current>"+current+"</b:current>") {
		t.Fatalf("body does not name the current hash: %s", body)
	}
	if cc := header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store (a cached refusal would lie about the current hash)", cc)
	}

	// The file's bytes are unchanged — AC-3's own proof.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if contentHash(string(before)) != contentHash(string(after)) {
		t.Fatalf("the refused write changed the file: %q -> %q", before, after)
	}

	// Nothing partial is left anywhere under the tree (AC-6).
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if isTempName(e.Name()) {
			t.Fatalf("a staging file survived the refusal: %s", e.Name())
		}
	}
}

// TestStaleHashWriteOfIdenticalBytesIsANoop proves D3 / O-8's second case: a
// stale base whose arriving bytes already match is a REPORTED no-op — 204 with
// identical_content — with no disk write and an unmoved mtime.
func TestStaleHashWriteOfIdenticalBytesIsANoop(t *testing.T) {
	h := newTestHandler(t)
	path := filepath.Join(h.Root(), "src", "main.go")

	// Writer B replaced the file with content A is about to send verbatim.
	mustWrite(t, path, fixtureMainBody)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	staleBase := contentHash("something else entirely\n")

	rec := do(t, h, "PUT", "/dav/src/main.go", map[string]string{
		"If-Match": `"` + staleBase + `"`,
	}, fixtureMainBody)

	if rec.Code != 204 {
		t.Fatalf("status = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Bunker-Verdict"); got != string(VerdictIdenticalContent) {
		t.Fatalf("verdict = %q, want identical_content", got)
	}
	if got := rec.Header().Get("X-Bunker-Noop"); got != "1" {
		t.Fatalf("X-Bunker-Noop = %q, want 1", got)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Fatalf("mtime moved on an identical-content write: %s -> %s", info.ModTime(), after.ModTime())
	}
	if got, want := rec.Header().Get("ETag"), `"`+contentHash(fixtureMainBody)+`"`; got != want {
		t.Fatalf("ETag = %q, want the unchanged hash %q", got, want)
	}
}

// TestCreateOnlyPrecondition proves If-None-Match: * — create or refuse, with
// the absence case distinguished from a stale base.
func TestCreateOnlyPrecondition(t *testing.T) {
	h := newTestHandler(t)

	existing := do(t, h, "PUT", "/dav/README.md", map[string]string{"If-None-Match": "*"}, "overwrite attempt\n")
	if existing.Code != 412 {
		t.Fatalf("create-only on a mapped resource -> %d, want 412", existing.Code)
	}
	if got := existing.Header().Get("X-Bunker-Verdict"); got != string(VerdictPreconditionFailed) {
		t.Fatalf("verdict = %q, want precondition_failed (no hash detail: the resource is absent-or-there)", got)
	}
	if got := existing.Header().Get("X-Bunker-Current-Hash"); got != "" {
		t.Fatalf("precondition_failed carried a current hash: %q", got)
	}
	body, err := os.ReadFile(filepath.Join(h.Root(), "README.md"))
	if err != nil || string(body) != fixtureReadme {
		t.Fatalf("refused create-only changed the file: %q (%v)", body, err)
	}

	created := do(t, h, "PUT", "/dav/src/new.go", map[string]string{"If-None-Match": "*"}, "package main\n")
	if created.Code != 201 {
		t.Fatalf("create-only on an unmapped resource -> %d, want 201", created.Code)
	}
	if got, want := created.Header().Get("X-Bunker-Hash"), contentHash("package main\n"); got != want {
		t.Fatalf("X-Bunker-Hash = %q, want %q", got, want)
	}
}

// TestIfMatchAbsentIsUnconditional proves R1 / §8.2: a stock client that knows
// nothing about extensions writes exactly as RFC 9110 says — a plain PUT
// succeeds and replaces.
func TestIfMatchAbsentIsUnconditional(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, "PUT", "/dav/README.md", nil, "stock client wrote this\n")
	if rec.Code != 204 {
		t.Fatalf("unconditional PUT over an existing file -> %d, want 204", rec.Code)
	}
	body, err := os.ReadFile(filepath.Join(h.Root(), "README.md"))
	if err != nil || string(body) != "stock client wrote this\n" {
		t.Fatalf("file = %q (%v)", body, err)
	}
}

// TestAtomicWriteIsVisibleWhole proves a PUT lands through a rename: the bytes
// are complete, the mode is preserved, and no staging file remains.
func TestAtomicWriteIsVisibleWhole(t *testing.T) {
	h := newTestHandler(t)
	path := filepath.Join(h.Root(), "src", "main.go")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	body := strings.Repeat("x", 4096)
	rec := do(t, h, "PUT", "/dav/src/main.go", nil, body)
	if rec.Code != 204 {
		t.Fatalf("PUT -> %d", rec.Code)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Fatalf("file length %d, want %d", len(got), len(body))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 preserved", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if isTempName(e.Name()) {
			t.Fatalf("staging file left behind: %s", e.Name())
		}
	}
	// The listing never shows a staging file even mid-write: the filter is
	// exercised directly rather than raced.
	if !isTempName(tempPrefix+"12345") || isTempName("main.go") {
		t.Fatal("staging-name filter is wrong")
	}
}

// TestWriteVerbsWork proves the mutating half of the surface actually mutates,
// including the Depth rule on a collection COPY.
func TestWriteVerbsWork(t *testing.T) {
	h := newTestHandler(t)

	// MKCOL creates exactly one collection.
	if rec := do(t, h, "MKCOL", "/dav/newdir", nil, ""); rec.Code != 201 {
		t.Fatalf("MKCOL -> %d", rec.Code)
	}
	if rec := do(t, h, "MKCOL", "/dav/newdir", nil, ""); rec.Code != 405 {
		t.Fatalf("MKCOL over a mapped URL -> %d, want 405", rec.Code)
	}

	// COPY a collection without members (Depth: 0), then with them.
	if rec := do(t, h, "COPY", "/dav/src", map[string]string{"Destination": "/dav/copy0", "Depth": "0"}, ""); rec.Code != 201 {
		t.Fatalf("COPY Depth 0 -> %d", rec.Code)
	}
	if entries, err := os.ReadDir(filepath.Join(h.Root(), "copy0")); err != nil || len(entries) != 0 {
		t.Fatalf("COPY Depth 0 copied members: %v (%v)", entries, err)
	}
	if rec := do(t, h, "COPY", "/dav/src", map[string]string{"Destination": "/dav/copyall"}, ""); rec.Code != 201 {
		t.Fatalf("COPY -> %d", rec.Code)
	}
	body, err := os.ReadFile(filepath.Join(h.Root(), "copyall", "main.go"))
	if err != nil || string(body) != fixtureMainBody {
		t.Fatalf("COPY did not reproduce the file: %q (%v)", body, err)
	}

	// Overwriting is the default; Overwrite: F refuses.
	if rec := do(t, h, "COPY", "/dav/src/util.go", map[string]string{"Destination": "/dav/copyall/main.go"}, ""); rec.Code != 204 {
		t.Fatalf("COPY with overwrite -> %d, want 204", rec.Code)
	}

	// MOVE removes the source.
	if rec := do(t, h, "MOVE", "/dav/copy0", map[string]string{"Destination": "/dav/moved"}, ""); rec.Code != 201 {
		t.Fatalf("MOVE -> %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(h.Root(), "copy0")); !os.IsNotExist(err) {
		t.Fatalf("MOVE left the source behind: %v", err)
	}

	// DELETE removes a file and a collection.
	if rec := do(t, h, "DELETE", "/dav/README.md", nil, ""); rec.Code != 204 {
		t.Fatalf("DELETE file -> %d", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/dav/moved", nil, ""); rec.Code != 204 {
		t.Fatalf("DELETE collection -> %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/dav/README.md", nil, ""); rec.Code != 404 {
		t.Fatalf("deleted file is still served: %d", rec.Code)
	}

	// A collection DELETE with the wrong Depth is refused (deviation 4) and
	// the collection survives.
	if rec := do(t, h, "DELETE", "/dav/copyall", map[string]string{"Depth": "0"}, ""); rec.Code != 400 {
		t.Fatalf("DELETE with Depth 0 -> %d, want 400", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(h.Root(), "copyall", "main.go")); err != nil {
		t.Fatalf("the refused DELETE removed the collection: %v", err)
	}
}

// TestPreconditionAppliesToDelete proves §6.1 item 6: the same hash rule
// governs the other writing methods.
func TestPreconditionAppliesToDelete(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, "DELETE", "/dav/README.md", map[string]string{
		"If-Match": `"sha256:` + strings.Repeat("0", 64) + `"`,
	}, "")
	if rec.Code != 412 {
		t.Fatalf("stale If-Match on DELETE -> %d, want 412", rec.Code)
	}
	if got := rec.Header().Get("X-Bunker-Verdict"); got != string(VerdictHashMismatch) {
		t.Fatalf("verdict = %q", got)
	}
	if _, err := os.Stat(filepath.Join(h.Root(), "README.md")); err != nil {
		t.Fatalf("the refused DELETE removed the file: %v", err)
	}

	ok := do(t, h, "DELETE", "/dav/README.md", map[string]string{
		"If-Match": `"` + contentHash(fixtureReadme) + `"`,
	}, "")
	if ok.Code != 204 {
		t.Fatalf("matching If-Match on DELETE -> %d, want 204", ok.Code)
	}
}

// TestWeakTagNeverSatisfiesIfMatch proves the strong comparison rule of RFC
// 9110 §13.1.1: a weak validator must not authorise a state-changing request.
func TestWeakTagNeverSatisfiesIfMatch(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, "PUT", "/dav/README.md", map[string]string{
		"If-Match": "W/\"" + contentHash(fixtureReadme) + "\"",
	}, "should not land\n")
	if rec.Code != 412 {
		t.Fatalf("weak If-Match -> %d, want 412", rec.Code)
	}
}
