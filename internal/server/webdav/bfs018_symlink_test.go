package webdav

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BFS-018 — a symlink is a symlink on the wire.
//
// The defect: a client that enumerated this surface saw a symlink as a REGULAR
// FILE of mode 0777 and size len(target) — the link's target path materialised
// as file content. These cells pin the three surfaces the fix rests on:
//
//   - the DECLARED type vocabulary (b:type) and the link's target
//     (b:link-target) on PROPFIND, plus the E-4 snapshot entry's type
//     and link_target;
//   - GET refusing a link by name rather than serving its target's bytes (a
//     copy is not a link), with the target itself still served normally;
//   - PUT + X-Bunker-Link-Target creating a real symlink, and a body-bearing
//     PUT onto a link being REFUSED so the materialised-pseudo-file write-back
//     cannot replace a link with a file (BFS-033: a refused write must not
//     land).
//
// Every cell has a control: the target file still reads, a DELETE-then-PUT
// still writes, a directory is still a collection. A refusal that is merely
// present, rather than specific, is the failure BFS-028 named.

const (
	linkTargetFile = "a.txt"
	linkTargetBody = "hello from the source tree"
	linkName       = "link-to-a"
	linkPropBody   = `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:b="urn:bunker:fs:1">
  <D:prop>
    <D:resourcetype/>
    <D:getcontentlength/>
    <D:getlastmodified/>
    <D:getetag/>
    <b:type/>
    <b:link-target/>
    <b:hash/>
  </D:prop>
</D:propfind>`
)

// symlinkFixture is the row's tree, verbatim: a 26-byte target and a link whose
// own size is the 5 bytes of "a.txt".
func symlinkFixture(t *testing.T) (*Handler, string) {
	t.Helper()
	h := newTestHandler(t)
	root := h.Root()
	mustWrite(t, filepath.Join(root, linkTargetFile), linkTargetBody)
	target := filepath.Join(root, linkName)
	if err := os.Symlink(linkTargetFile, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return h, root
}

// serverEntryState is the LSTAT of one path on the SERVER's disk: what the
// server holds, which is the only thing that decides whether a tree was
// corrupted.
type serverEntryState struct {
	kind   string
	target string
	size   int64
	body   []byte
	sha    string
}

func serverEntry(t *testing.T, path string) serverEntryState {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		return serverEntryState{kind: "absent"}
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			t.Fatalf("readlink %s: %v", path, err)
		}
		return serverEntryState{kind: "symlink", target: target, size: fi.Size()}
	case fi.IsDir():
		return serverEntryState{kind: "dir"}
	default:
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		sum := sha256.Sum256(body)
		return serverEntryState{kind: "file", size: fi.Size(), body: body, sha: hex.EncodeToString(sum[:])}
	}
}

// propValue reads one property out of a parsed propstat group.
func propValue(t *testing.T, body string, href, want string) (string, bool) {
	t.Helper()
	doc := parseMultiStatus(t, body)
	for _, r := range doc.Responses {
		if r.Href != href {
			continue
		}
		for _, ps := range r.Stats {
			if !strings.Contains(ps.Status, "200") {
				continue
			}
			for _, item := range ps.Props.Items {
				name := item.XMLName.Local
				switch item.XMLName.Space {
				case NSBunker:
					name = "b:" + name
				case NSDav:
					name = "D:" + name
				}
				if name == want {
					return strings.TrimSpace(item.Value), true
				}
			}
		}
	}
	return "", false
}

// C1 — the declared type and the link's target, for BOTH carriers.
func TestSymlinkTypeIsDeclaredOnTheWire(t *testing.T) {
	h, _ := symlinkFixture(t)

	rec := do(t, h, "PROPFIND", "/dav/"+linkName, map[string]string{"Depth": "0"}, linkPropBody)
	if rec.Code != 207 {
		t.Fatalf("PROPFIND link -> %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if got, ok := propValue(t, body, "/dav/"+linkName, "b:type"); !ok || got != "symlink" {
		t.Fatalf("b:type = %q (present=%v), want \"symlink\"; a client cannot tell a link from a file\n%s", got, ok, body)
	}
	if got, ok := propValue(t, body, "/dav/"+linkName, "b:link-target"); !ok || got != linkTargetFile {
		t.Fatalf("b:link-target = %q (present=%v), want %q\n%s", got, ok, linkTargetFile, body)
	}
	// The link's OWN size: 5 bytes, the length of the target path. Not the
	// 26-byte target content — the surface never describes a link as its target.
	if got, _ := propValue(t, body, "/dav/"+linkName, "D:getcontentlength"); got != "5" {
		t.Fatalf("getcontentlength = %q, want 5 (the link's own size)", got)
	}
	// No content identity for a link: a hash here would be the TARGET's, which
	// is a dereference committed in metadata.
	if got, ok := propValue(t, body, "/dav/"+linkName, "D:getetag"); ok {
		t.Fatalf("a link carries getetag %q; it has no content bytes to hash", got)
	}
	if got, ok := propValue(t, body, "/dav/"+linkName, "b:hash"); ok {
		t.Fatalf("a link carries b:hash %q; it has no content bytes to hash", got)
	}

	// Depth: 1 — the child-entry carrier must agree with the named-entry one.
	list := do(t, h, "PROPFIND", "/dav/", map[string]string{"Depth": "1"}, linkPropBody)
	if list.Code != 207 {
		t.Fatalf("PROPFIND collection -> %d %s", list.Code, list.Body.String())
	}
	lb := list.Body.String()
	if got, ok := propValue(t, lb, "/dav/"+linkName, "b:type"); !ok || got != "symlink" {
		t.Fatalf("Depth 1 b:type for the link = %q (present=%v)\n%s", got, ok, lb)
	}
	if got, ok := propValue(t, lb, "/dav/"+linkTargetFile, "b:type"); !ok || got != "file" {
		t.Fatalf("Depth 1 b:type for %s = %q (present=%v), want \"file\"\n%s", linkTargetFile, got, ok, lb)
	}
	if _, ok := propValue(t, lb, "/dav/"+linkTargetFile, "D:getetag"); !ok {
		t.Fatalf("the regular file lost its getetag; the extension property must be additive\n%s", lb)
	}
}

// C2 — GET on a link is refused by NAME; the link's target still reads.
func TestSymlinkGetIsRefusedByNameAndTheTargetStillReads(t *testing.T) {
	h, root := symlinkFixture(t)

	rec := do(t, h, "GET", "/dav/"+linkName, nil, "")
	if rec.Code != 405 {
		t.Fatalf("GET on a link -> %d, want 405 (this surface will not serve a link as its target's bytes)\n%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Bunker-Verdict"); got != string(VerdictSymlinkNotAFile) {
		t.Fatalf("X-Bunker-Verdict = %q, want %q", got, VerdictSymlinkNotAFile)
	}
	if !strings.Contains(rec.Body.String(), "symlink-not-a-file") {
		t.Fatalf("the refusal body does not name what it refused:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), linkTargetBody) {
		t.Fatalf("the refusal body carries the TARGET's bytes:\n%s", rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); !strings.Contains(got, "GET") {
		t.Fatalf("a 405 must name the verb set (RFC 9110 §15.5.6); Allow = %q", got)
	}

	// CONTROL: the refusal is about the LINK, not about GET. The target reads.
	ctrl := do(t, h, "GET", "/dav/"+linkTargetFile, nil, "")
	if ctrl.Code != 200 || ctrl.Body.String() != linkTargetBody {
		t.Fatalf("CONTROL FAILED: GET on the target file -> %d %q", ctrl.Code, ctrl.Body.String())
	}
	// CONTROL 2: a regular file's GET is untouched.
	mustWrite(t, filepath.Join(root, "plain.txt"), "plain")
	plain := do(t, h, "GET", "/dav/plain.txt", nil, "")
	if plain.Code != 200 || plain.Body.String() != "plain" {
		t.Fatalf("CONTROL FAILED: GET on an ordinary file -> %d %q", plain.Code, plain.Body.String())
	}
}

// C3 — PUT + the declared header creates a real symlink, server-side.
func TestSymlinkCreateIsDeclaredAndLandsAsASymlink(t *testing.T) {
	h, root := symlinkFixture(t)
	created := filepath.Join(root, "created-link")

	before := h.tree.revToken()
	rec := do(t, h, "PUT", "/dav/created-link", map[string]string{LinkTargetHeader: linkTargetFile}, "")
	if rec.Code != 201 {
		t.Fatalf("PUT with %s -> %d %s", LinkTargetHeader, rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(LinkTargetHeader); got != linkTargetFile {
		t.Fatalf("the create did not confirm the target: %s = %q", LinkTargetHeader, got)
	}
	st := serverEntry(t, created)
	if st.kind != "symlink" || st.target != linkTargetFile {
		t.Fatalf("the server holds %s target=%q; want a symlink to %q", st.kind, st.target, linkTargetFile)
	}
	if st.size != int64(len(linkTargetFile)) {
		t.Fatalf("the created link is %d bytes, want %d (the target path's length)", st.size, len(linkTargetFile))
	}
	// The corruption shape, asserted on the server's own disk: no regular file
	// anywhere in the tree holds the target path as its content.
	assertNoFileHoldsTheTargetPath(t, root)
	if h.tree.revToken() == before {
		t.Fatalf("the create did not move the served revision")
	}
	// The new link is visible as a link through BOTH carriers.
	rec2 := do(t, h, "PROPFIND", "/dav/created-link", map[string]string{"Depth": "0"}, linkPropBody)
	if got, ok := propValue(t, rec2.Body.String(), "/dav/created-link", "b:type"); !ok || got != "symlink" {
		t.Fatalf("after the create, b:type = %q (present=%v), want symlink", got, ok)
	}
	// Re-issuing the same declaration is D3's reported no-op, not a rewrite.
	again := do(t, h, "PUT", "/dav/created-link", map[string]string{LinkTargetHeader: linkTargetFile}, "")
	if again.Code != 204 || again.Header().Get("X-Bunker-Noop") != "1" {
		t.Fatalf("re-declaring the same link -> %d noop=%q, want 204 + noop", again.Code, again.Header().Get("X-Bunker-Noop"))
	}
	if got := again.Header().Get("X-Bunker-Verdict"); got != string(VerdictIdenticalContent) {
		t.Fatalf("re-declaring the same link verdict = %q, want identical_content", got)
	}
}

// C4 — the link create refuses what cannot be a link target, and still creates
// when the declaration is valid (the control).
func TestSymlinkCreateRefusalsAreSpecific(t *testing.T) {
	h, root := symlinkFixture(t)

	t.Run("a body is not a link", func(t *testing.T) {
		rec := do(t, h, "PUT", "/dav/with-body", map[string]string{LinkTargetHeader: linkTargetFile}, linkTargetFile)
		if rec.Code != 400 || rec.Header().Get("X-Bunker-Verdict") != string(VerdictBadArguments) {
			t.Fatalf("PUT link + body -> %d %s", rec.Code, rec.Body.String())
		}
		if got := serverEntry(t, filepath.Join(root, "with-body")).kind; got != "absent" {
			t.Fatalf("a refused link create left a %s behind", got)
		}
	})
	t.Run("an empty target", func(t *testing.T) {
		rec := do(t, h, "PUT", "/dav/empty-target", map[string]string{LinkTargetHeader: " "}, "")
		// A single space is a legal (if useless) target, so this must CREATE.
		if rec.Code != 201 {
			t.Fatalf("a space target should be a legal target -> %d %s", rec.Code, rec.Body.String())
		}
		if got := serverEntry(t, filepath.Join(root, "empty-target")).kind; got != "symlink" {
			t.Fatalf("space target: server holds %s", got)
		}
	})
	t.Run("a missing parent is still 409", func(t *testing.T) {
		rec := do(t, h, "PUT", "/dav/nope/deep", map[string]string{LinkTargetHeader: linkTargetFile}, "")
		if rec.Code != 409 {
			t.Fatalf("link create in a missing collection -> %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("a link over a collection is still 405", func(t *testing.T) {
		rec := do(t, h, "PUT", "/dav/src", map[string]string{LinkTargetHeader: linkTargetFile}, "")
		if rec.Code != 405 {
			t.Fatalf("link create over a collection -> %d %s", rec.Code, rec.Body.String())
		}
		if got := serverEntry(t, filepath.Join(root, "src")).kind; got != "dir" {
			t.Fatalf("the collection became a %s", got)
		}
	})
}

// C5 — the corruption path: a body-bearing PUT onto a link is refused and
// nothing lands (BFS-033), and the refusal is specific.
func TestUndeclaredSymlinkReplaceIsRefusedAndNothingLands(t *testing.T) {
	h, root := symlinkFixture(t)
	linkPath := filepath.Join(root, linkName)
	targetPath := filepath.Join(root, linkTargetFile)
	before := serverEntry(t, linkPath)
	beforeTarget := serverEntry(t, targetPath)

	// The materialised pseudo-file's write-back: the body IS the target path,
	// which is what a client that believed the link was a 5-byte file sends.
	rec := do(t, h, "PUT", "/dav/"+linkName, nil, linkTargetFile)
	if rec.Code != 409 {
		t.Fatalf("PUT body over a link -> %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Bunker-Verdict"); got != string(VerdictSymlinkUndeclaredReplace) {
		t.Fatalf("verdict = %q, want %q", got, VerdictSymlinkUndeclaredReplace)
	}
	if !strings.Contains(rec.Body.String(), linkTargetFile) {
		t.Fatalf("the refusal does not name the link's target:\n%s", rec.Body.String())
	}
	// BFS-033: the refused write must not have landed, in any form.
	after := serverEntry(t, linkPath)
	if after.kind != "symlink" || after.target != before.target {
		t.Fatalf("after the refusal the server holds %s target=%q, want symlink target=%q", after.kind, after.target, before.target)
	}
	if got := serverEntry(t, targetPath); got.sha != beforeTarget.sha {
		t.Fatalf("the refuse-then-write path modified the target file (%s -> %s)", beforeTarget.sha, got.sha)
	}

	// The create-only precondition is still answered by the existing predicate.
	if r := do(t, h, "PUT", "/dav/"+linkName, map[string]string{"If-None-Match": "*"}, "x"); r.Code != 412 {
		t.Fatalf("If-None-Match: * over an existing link -> %d, want 412\n%s", r.Code, r.Body.String())
	}

	// CONTROL: the refusal is about the LINK TYPE, not about PUT. After a
	// DELETE the same body lands, and DELETE needs no new machinery.
	if d := do(t, h, "DELETE", "/dav/"+linkName, nil, ""); d.Code != 204 {
		t.Fatalf("DELETE the link -> %d %s", d.Code, d.Body.String())
	}
	if p := do(t, h, "PUT", "/dav/"+linkName, nil, linkTargetFile); p.Code != 201 {
		t.Fatalf("CONTROL FAILED: after DELETE the same PUT -> %d %s", p.Code, p.Body.String())
	}
	if got := serverEntry(t, linkPath); got.kind != "file" || string(got.body) != linkTargetFile {
		t.Fatalf("CONTROL FAILED: the server holds %s body=%q", got.kind, string(got.body))
	}
}

// C6 — the E-4 snapshot entry carries the type AND the link target.
func TestSnapshotCarriesTheLinkTarget(t *testing.T) {
	h, _ := symlinkFixture(t)
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "snapshot"},
		`{"depth":"infinity","include_hash":true}`)
	if rec.Code != 200 {
		t.Fatalf("snapshot -> %d %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result struct {
			Entries []struct {
				Path       string `json:"path"`
				Type       string `json:"type"`
				Size       int64  `json:"size"`
				Hash       string `json:"hash"`
				LinkTarget string `json:"link_target"`
			} `json:"entries"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("snapshot envelope did not parse: %v", err)
	}
	var sawLink, sawFile bool
	for _, e := range env.Result.Entries {
		if e.Path == linkName {
			sawLink = true
			if e.Type != "symlink" {
				t.Fatalf("snapshot type for the link = %q, want symlink", e.Type)
			}
			if e.LinkTarget != linkTargetFile {
				t.Fatalf("snapshot link_target = %q, want %q (this is the field the client reads instead of the bytes)", e.LinkTarget, linkTargetFile)
			}
			if e.Hash != "" {
				t.Fatalf("a link carries hash %q in the snapshot; a link has no content hash", e.Hash)
			}
			if e.Size != int64(len(linkTargetFile)) {
				t.Fatalf("snapshot size for the link = %d, want %d", e.Size, len(linkTargetFile))
			}
		}
		if e.Path == linkTargetFile {
			sawFile = true
			if e.Type != "file" || e.Hash == "" {
				t.Fatalf("the target entry = type %q hash %q, want file + hash", e.Type, e.Hash)
			}
		}
	}
	if !sawLink || !sawFile {
		t.Fatalf("the snapshot did not describe both entries (%v)", env.Result.Entries)
	}
}

// C7 — the extension is DECLARED and versioned, twice: OPTIONS and the document.
func TestSymlinkExtensionIsDeclared(t *testing.T) {
	h, _ := symlinkFixture(t)
	opt := do(t, h, "OPTIONS", "/dav/", nil, "")
	if got := opt.Header().Get("X-Bunker-Extensions"); !strings.Contains(got, "symlink") {
		t.Fatalf("X-Bunker-Extensions = %q, want it to name symlink", got)
	}
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "capabilities"}, "")
	if rec.Code != 200 {
		t.Fatalf("capabilities -> %d", rec.Code)
	}
	var doc struct {
		Result struct {
			Capabilities struct {
				Document   int `json:"document_version"`
				Extensions map[string]struct {
					V        int      `json:"v"`
					Name     string   `json:"name"`
					TypeProp string   `json:"type_property"`
					Target   string   `json:"target_property"`
					Types    []string `json:"types"`
				} `json:"extensions"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("capability document did not parse: %v", err)
	}
	ext, ok := doc.Result.Capabilities.Extensions["symlink"]
	if !ok {
		t.Fatalf("the capability document does not declare the symlink extension: a client would have to invent the convention\n%s", rec.Body.String())
	}
	if ext.V != 1 || ext.Name != LinkTargetHeader {
		t.Fatalf("symlink extension = %+v, want v1 named %s", ext, LinkTargetHeader)
	}
	if ext.TypeProp != "b:type" || ext.Target != "b:link-target" {
		t.Fatalf("symlink extension declares properties %q/%q", ext.TypeProp, ext.Target)
	}
	if strings.Join(ext.Types, ",") != "file,dir,symlink" {
		t.Fatalf("symlink extension types = %v", ext.Types)
	}
}

// C8 — ATTRIBUTION. This cell must stay GREEN under every mutation: it pins
// the parts of the changed surfaces that are NOT about links, so a red link
// cell can be attributed to the link behaviour rather than to a broken surface.
func TestSymlinkChangeAttributionCell(t *testing.T) {
	h, root := symlinkFixture(t)
	mustWrite(t, filepath.Join(root, "src", "extra.go"), "package main\n")

	list := do(t, h, "PROPFIND", "/dav/", map[string]string{"Depth": "1"}, linkPropBody)
	if list.Code != 207 {
		t.Fatalf("PROPFIND -> %d", list.Code)
	}
	body := list.Body.String()
	// A collection is still a collection, in both directions.
	if got, ok := propValue(t, body, "/dav/src/", "b:type"); !ok || got != "dir" {
		t.Fatalf("attribution: b:type for a collection = %q (present=%v)", got, ok)
	}
	if !strings.Contains(body, "<D:collection></D:collection>") {
		t.Fatalf("attribution: resourcetype lost its collection value\n%s", body)
	}
	// A regular file still carries my hash identity and its own bytes.
	if got, ok := propValue(t, body, "/dav/"+linkTargetFile, "b:hash"); !ok || !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("attribution: b:hash for a file = %q (present=%v)", got, ok)
	}
	sum := sha256.Sum256([]byte(linkTargetBody))
	if want := "sha256:" + hex.EncodeToString(sum[:]); !strings.Contains(body, want) {
		t.Fatalf("attribution: the hash of the target's bytes is not the one served (%s)", want)
	}
	// allprop is byte-for-byte the old surface: no type vocabulary leaks into it.
	all := do(t, h, "PROPFIND", "/dav/"+linkTargetFile, map[string]string{"Depth": "0"}, "")
	if all.Code != 207 {
		t.Fatalf("allprop PROPFIND -> %d", all.Code)
	}
	if strings.Contains(all.Body.String(), "b:type") {
		t.Fatalf("allprop must not gain the extension property (an old client's response must not change)\n%s", all.Body.String())
	}
	// The snapshot op still answers its declared shape for ordinary entries.
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "snapshot"}, `{"depth":"1"}`)
	if rec.Code != 200 {
		t.Fatalf("attribution: snapshot -> %d %s", rec.Code, rec.Body.String())
	}
	var env struct {
		OK     bool   `json:"ok"`
		Op     string `json:"op"`
		Result struct {
			Count   int `json:"count"`
			Entries []struct {
				Path string `json:"path"`
				Type string `json:"type"`
				Mode string `json:"mode"`
			} `json:"entries"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("attribution: snapshot envelope did not parse: %v", err)
	}
	if !env.OK || env.Op != "snapshot" || env.Result.Count == 0 {
		t.Fatalf("attribution: snapshot envelope = %s", rec.Body.String())
	}
	for _, e := range env.Result.Entries {
		if e.Type == "" || e.Mode == "" {
			t.Fatalf("attribution: snapshot entry lost its declared fields: %+v", e)
		}
	}
}

// assertNoFileHoldsTheTargetPath is the row's headline assertion, made on the
// server's own disk: no regular file in the served tree has the link's target
// path as its content. It is what "the target path was materialised as file
// content" means, checked at the only place that decides whether the tree was
// corrupted.
func assertNoFileHoldsTheTargetPath(t *testing.T, root string) {
	t.Helper()
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if strings.TrimSpace(string(body)) == linkTargetFile {
			t.Fatalf("%s is a REGULAR FILE whose content is the link target %q — the materialisation the row filed", path, linkTargetFile)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
