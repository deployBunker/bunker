package fsclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// BFS-018 on the client's metadata path: the type the surface DECLARES is
// carried, an unknown type is never resolved to a file, and a link create is
// refused by name when the surface never declared the symlink extension.

const symlinkPropfindReply = `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:b="urn:bunker:fs:1">
  <D:response>
    <D:href>/dav/link-to-a</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype/>
        <D:getcontentlength>5</D:getcontentlength>
        <b:type>symlink</b:type>
        <b:link-target>a.txt</b:link-target>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
  <D:response>
    <D:href>/dav/a.txt</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype/>
        <D:getcontentlength>26</D:getcontentlength>
        <D:getetag>"sha256:183ecc9cc7ce6ce571ba01775af5b624301c7111052ecd1fa7ab77afbdac2fa8"</D:getetag>
        <b:type>file</b:type>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
  <D:response>
    <D:href>/dav/src/</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype><D:collection/></D:resourcetype>
        <b:type>dir</b:type>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`

func TestMultistatusCarriesTheDeclaredType(t *testing.T) {
	metas, err := decodeMultistatus([]byte(symlinkPropfindReply), "/dav")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	byPath := map[string]FileMeta{}
	for _, m := range metas {
		byPath[m.Path] = m
	}
	link, ok := byPath["link-to-a"]
	if !ok {
		t.Fatalf("the link did not decode: %v", byPath)
	}
	if link.Kind != KindSymlink {
		t.Fatalf("the link's kind = %q, want %q — a client that reads only resourcetype cannot see it", link.Kind, KindSymlink)
	}
	if link.LinkTarget != "a.txt" {
		t.Fatalf("the link's target = %q, want a.txt", link.LinkTarget)
	}
	if link.IsDir {
		t.Fatalf("a link is not a collection")
	}
	if link.Hash != "" {
		t.Fatalf("a link carries hash %q: hashing a link means hashing its target", link.Hash)
	}
	if link.Size != 5 {
		t.Fatalf("the link's size = %d, want 5 (its own target path's length)", link.Size)
	}
	file := byPath["a.txt"]
	if file.Kind != KindFile || file.Hash == "" {
		t.Fatalf("the regular file = kind %q hash %q, want file + hash", file.Kind, file.Hash)
	}
	dir := byPath["src"]
	if dir.Kind != KindDir || !dir.IsDir {
		t.Fatalf("the collection = kind %q IsDir=%v", dir.Kind, dir.IsDir)
	}
}

// An OLDER surface publishes no b:type. The client must then describe what the
// STANDARD properties describe — a file or a collection — and must never invent
// a link (or the reverse: it cannot know a link from a file, which is the
// residual this row names rather than hides).
func TestSurfaceWithoutTheTypePropertyIsResolvedFromTheStandardProperties(t *testing.T) {
	const old = `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:">
  <D:response>
    <D:href>/dav/link-to-a</D:href>
    <D:propstat>
      <D:prop><D:resourcetype/><D:getcontentlength>5</D:getcontentlength></D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
  <D:response>
    <D:href>/dav/src/</D:href>
    <D:propstat>
      <D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`
	metas, err := decodeMultistatus([]byte(old), "/dav")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, m := range metas {
		if m.Kind == KindSymlink {
			t.Fatalf("a surface that published no type cannot be read as declaring a link (%s)", m.Path)
		}
		switch m.Path {
		case "link-to-a":
			if m.Kind != KindFile {
				t.Fatalf("an untyped non-collection resolves to %q, want file", m.Kind)
			}
		case "src":
			if m.Kind != KindDir {
				t.Fatalf("an untyped collection resolves to %q, want dir", m.Kind)
			}
		}
	}
}

// A type this build does not know is NAMED, not softened into a file.
func TestUnknownDeclaredTypeIsNotAFile(t *testing.T) {
	const fifo = `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:b="urn:bunker:fs:1">
  <D:response>
    <D:href>/dav/pipe</D:href>
    <D:propstat>
      <D:prop><D:resourcetype/><b:type>fifo</b:type></D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`
	metas, err := decodeMultistatus([]byte(fifo), "/dav")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("want one entry, got %d", len(metas))
	}
	m := metas[0]
	if m.Kind != KindUnknown {
		t.Fatalf("declared type \"fifo\" resolved to %q, want %q", m.Kind, KindUnknown)
	}
	nd := Node{Kind: m.Kind}
	if nd.KindHonoured() {
		t.Fatalf("a type this client cannot present must not be reported as honoured")
	}
	if m.Kind == KindFile {
		t.Fatalf("an unknown type must never be spelled as a file")
	}
}

// The snapshot op's `link_target` is the second carrier of the same fact.
func TestSnapshotCarriesTheLinkTargetAndDropsItsHash(t *testing.T) {
	body := `{"ok":true,"op":"snapshot","verdict":"ok","truncated":false,"result":{"count":2,"entries":[` +
		`{"path":"link-to-a","type":"symlink","size":5,"mode":"0777","link_target":"a.txt"},` +
		`{"path":"a.txt","type":"file","size":26,"mode":"0664","hash":"sha256:ab"}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c, err := NewClient(Options{BaseURL: srv.URL, Concurrency: 2, OpTimeout: 5 * time.Second, BindTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	snap, oerr := c.SnapshotTree(context.Background(), "", false)
	if oerr != nil {
		t.Fatalf("snapshot: %v", oerr)
	}
	link, ok := snap.Lookup("link-to-a")
	if !ok {
		t.Fatal("the link is not in the node tree")
	}
	if link.Kind != KindSymlink || link.LinkTarget != "a.txt" {
		t.Fatalf("node = kind %q target %q, want symlink + a.txt", link.Kind, link.LinkTarget)
	}
	if link.Hash != "" {
		t.Fatalf("the node carries hash %q; a link has no content hash", link.Hash)
	}
	file, _ := snap.Lookup("a.txt")
	if file.Kind != KindFile || file.Hash == "" {
		t.Fatalf("the file entry = kind %q hash %q", file.Kind, file.Hash)
	}
}

// A surface that publishes a hash ALONGSIDE a symlink type cannot make the
// client cache the target's bytes under the link's name: the hash is dropped.
func TestSymlinkNodeNeverKeepsAHash(t *testing.T) {
	body := `{"ok":true,"op":"snapshot","verdict":"ok","truncated":false,"result":{"count":1,"entries":[` +
		`{"path":"link","type":"symlink","size":1,"mode":"0777","hash":"sha256:deadbeef","link_target":"a"}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c, _ := NewClient(Options{BaseURL: srv.URL, Concurrency: 1, OpTimeout: 5 * time.Second, BindTimeout: 2 * time.Second})
	snap, oerr := c.SnapshotTree(context.Background(), "", false)
	if oerr != nil {
		t.Fatalf("snapshot: %v", oerr)
	}
	nd, _ := snap.Lookup("link")
	if nd.Hash != "" {
		t.Fatalf("hash %q survived on a symlink node: that hash is the TARGET's and would become the link's cache key", nd.Hash)
	}
}

// PutLink refuses LOCALLY when the surface never declared the extension, and it
// sends nothing at all: a build that would ignore the header must not be handed
// a request that could create a file.
func TestPutLinkRefusesAnUndeclaredSurfaceWithoutSendingAnything(t *testing.T) {
	var puts int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			atomic.AddInt64(&puts, 1)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c, _ := NewClient(Options{BaseURL: srv.URL, Concurrency: 1, OpTimeout: 5 * time.Second, BindTimeout: 2 * time.Second})
	if _, err := c.PutLink(context.Background(), "new-link", "a.txt", PutPrecondition{}); err == nil {
		t.Fatal("a surface that did not declare the symlink extension must not accept a link create")
	} else {
		if err.Errno != ErrnoEOPNOTSUPP {
			t.Fatalf("errno = %v, want EOPNOTSUPP", ErrnoName(err.Errno))
		}
		if !strings.Contains(err.Detail, "extensions.symlink") {
			t.Fatalf("the refusal does not name what it is refusing: %q", err.Detail)
		}
	}
	if got := atomic.LoadInt64(&puts); got != 0 {
		t.Fatalf("%d PUT(s) were sent; a locally refused link create must send nothing", got)
	}
}

// The declared surface IS sent the declared header, with an EMPTY body — the
// target travels as a header, never as content.
func TestPutLinkSendsTheDeclaredHeaderWithNoBody(t *testing.T) {
	var (
		gotTarget string
		gotBody   int
		gotLen    int64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusOK)
			return
		}
		gotTarget = r.Header.Get(LinkTargetHeader)
		buf := make([]byte, 32)
		n, _ := r.Body.Read(buf)
		gotBody = n
		gotLen = r.ContentLength
		w.WriteHeader(http.StatusCreated)
		w.Header().Set(LinkTargetHeader, gotTarget)
	}))
	defer srv.Close()
	c, _ := NewClient(Options{BaseURL: srv.URL, Concurrency: 1, OpTimeout: 5 * time.Second, BindTimeout: 2 * time.Second})
	// The declaration comes from the capability document, so the test supplies
	// one: this is the same carrier the handshake reads.
	c.capsMu.Lock()
	c.caps = &Capabilities{}
	c.caps.Extensions.Symlink = &CapabilitySymlink{Name: LinkTargetHeader, V: 1,
		TypeProperty: "b:type", TargetProperty: "b:link-target"}
	c.capsMu.Unlock()

	if _, err := c.PutLink(context.Background(), "new-link", "a.txt", PutPrecondition{}); err != nil {
		t.Fatalf("PutLink on a declared surface: %v", err)
	}
	if gotTarget != "a.txt" {
		t.Fatalf("%s = %q, want a.txt", LinkTargetHeader, gotTarget)
	}
	if gotBody != 0 || gotLen != 0 {
		t.Fatalf("the link create carried %d bytes (Content-Length %d): a symlink's target is a declared argument, never content", gotBody, gotLen)
	}
}

// The declaration is read from the SAME cached document every other capability
// question reads, and a document without the block is not the same fact as a
// document that declares it.
func TestSymlinkCapabilityReadsTheDeclaredBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{"ok": true, "op": "capabilities", "verdict": "ok", "truncated": false,
			"result": map[string]any{"capabilities": map[string]any{
				"surface": "bunkerd-webdav/1", "document_version": 1,
				"extensions": map[string]any{
					"symlink": map[string]any{"name": LinkTargetHeader, "v": 1,
						"type_property": "b:type", "target_property": "b:link-target",
						"types": []string{"file", "dir", "symlink"}},
				},
			}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()
	c, _ := NewClient(Options{BaseURL: srv.URL, Concurrency: 1, OpTimeout: 5 * time.Second, BindTimeout: 2 * time.Second})
	if got := c.SymlinkCapability(); got.Present {
		t.Fatalf("no document has been fetched yet, but the capability reads as declared: %+v", got)
	}
	if _, err := c.CapabilitiesDoc(context.Background()); err != nil {
		t.Fatalf("capabilities doc: %v", err)
	}
	got := c.SymlinkCapability()
	if !got.Present || got.V != 1 || got.Name != LinkTargetHeader || got.TargetProperty != "b:link-target" {
		t.Fatalf("capability = %+v, want the declared v1 block", got)
	}
}
