package webdav

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMethodMatrix drives every row of the spec's method matrix (§2.1) and
// asserts the status AND the machine code — in the header, and in the body,
// which §5.3 requires for both. The table is the acceptance battery of A-1:
// each refusal is a cell, and each cell checks the code travels twice.
func TestMethodMatrix(t *testing.T) {
	h := newTestHandler(t)

	cases := []struct {
		name       string
		method     string
		target     string
		headers    map[string]string
		body       string
		wantStatus int
		wantCode   Verdict
		// wantElement overrides the derived DAV:error child name; jsonEnvelope
		// marks an E-4 response, whose body carries the code in JSON.
		wantElement  string
		jsonEnvelope bool
		wantAllow    bool
	}{
		{name: "unknown method", method: "FROBNICATE", target: "/dav/", wantStatus: 501, wantCode: VerdictMethodUnknown},
		{name: "REPORT is not allowed", method: "REPORT", target: "/dav/", wantStatus: 405, wantCode: VerdictMethodNotAllowed, wantAllow: true},
		{name: "ACL is not allowed", method: "ACL", target: "/dav/", wantStatus: 405, wantCode: VerdictMethodNotAllowed, wantAllow: true},
		{name: "PATCH is not allowed", method: "PATCH", target: "/dav/README.md", wantStatus: 405, wantCode: VerdictMethodNotAllowed, wantAllow: true},
		{name: "LOCK is not built yet", method: "LOCK", target: "/dav/README.md", wantStatus: 501, wantCode: VerdictNotImplementedYet, wantAllow: true},
		{name: "UNLOCK is not built yet", method: "UNLOCK", target: "/dav/README.md", wantStatus: 501, wantCode: VerdictNotImplementedYet, wantAllow: true},
		{name: "GET on a collection", method: "GET", target: "/dav/src", wantStatus: 405, wantCode: VerdictMethodNotAllowed, wantAllow: true},
		{name: "HEAD on a collection", method: "HEAD", target: "/dav/src", wantStatus: 405, wantCode: VerdictMethodNotAllowed, wantAllow: true},
		{name: "GET unmapped", method: "GET", target: "/dav/nope.go", wantStatus: 404, wantCode: VerdictNotFound},
		{name: "DELETE unmapped", method: "DELETE", target: "/dav/nope.go", wantStatus: 404, wantCode: VerdictNotFound},
		{name: "PROPFIND without Depth", method: "PROPFIND", target: "/dav/src", wantStatus: 400, wantCode: VerdictDepthRequired},
		{name: "PROPFIND Depth 2", method: "PROPFIND", target: "/dav/src", headers: map[string]string{"Depth": "2"}, wantStatus: 400, wantCode: VerdictInvalidDepth},
		{
			name: "PROPFIND Depth infinity", method: "PROPFIND", target: "/dav/src",
			headers: map[string]string{"Depth": "infinity"}, wantStatus: 403, wantCode: VerdictPropfindFiniteDepth,
			wantElement: "<D:propfind-finite-depth>",
		},
		{name: "collection DELETE with Depth 0", method: "DELETE", target: "/dav/src", headers: map[string]string{"Depth": "0"}, wantStatus: 400, wantCode: VerdictInvalidDepth},
		{name: "PUT on a collection", method: "PUT", target: "/dav/src", body: "x", wantStatus: 405, wantCode: VerdictMethodNotAllowed},
		{name: "PUT with a missing parent", method: "PUT", target: "/dav/nodir/x.go", body: "x", wantStatus: 409, wantCode: VerdictConflict},
		{name: "PUT with a declared hash that does not match", method: "PUT", target: "/dav/src/util.go", headers: map[string]string{"X-Bunker-Hash": "sha256:" + strings.Repeat("0", 64)}, body: "new bytes", wantStatus: 422, wantCode: VerdictBodyHashMismatch},
		{name: "MKCOL with a body", method: "MKCOL", target: "/dav/newcol", headers: map[string]string{"Content-Type": "text/plain"}, body: "body", wantStatus: 415, wantCode: VerdictUnsupportedMediaType},
		{name: "MKCOL with a missing ancestor", method: "MKCOL", target: "/dav/a/b/c", wantStatus: 409, wantCode: VerdictConflict},
		{name: "COPY with Overwrite F onto a mapped destination", method: "COPY", target: "/dav/README.md", headers: map[string]string{"Destination": "/dav/src/main.go", "Overwrite": "F"}, wantStatus: 412, wantCode: VerdictPreconditionFailed},
		{name: "COPY to a destination outside the namespace", method: "COPY", target: "/dav/README.md", headers: map[string]string{"Destination": "/elsewhere/x"}, wantStatus: 502, wantCode: VerdictBadGateway},
		{name: "COPY without Destination", method: "COPY", target: "/dav/README.md", wantStatus: 400, wantCode: VerdictBadArguments},
		{name: "MOVE onto itself", method: "MOVE", target: "/dav/README.md", headers: map[string]string{"Destination": "/dav/README.md"}, wantStatus: 403, wantCode: VerdictForbidden},
		{name: "stale tree token", method: "GET", target: "/dav/README.md", headers: map[string]string{"X-Bunker-Tree": "tree:0000000000000000"}, wantStatus: 409, wantCode: VerdictStaleTree},
		{name: "POST without an op", method: "POST", target: "/dav/", wantStatus: 400, wantCode: VerdictExtensionOpMissing, jsonEnvelope: true},
		{name: "POST with an unknown op", method: "POST", target: "/dav/", headers: map[string]string{"X-Bunker-Op": "frobnicate"}, wantStatus: 400, wantCode: VerdictOpUnknown, jsonEnvelope: true},
		{name: "POST watch degrades to poll", method: "POST", target: "/dav/", headers: map[string]string{"X-Bunker-Op": "watch"}, wantStatus: 501, wantCode: VerdictCapabilityUnavailable, jsonEnvelope: true},
		{name: "POST status is not in this build", method: "POST", target: "/dav/", headers: map[string]string{"X-Bunker-Op": "status"}, wantStatus: 501, wantCode: VerdictCapabilityUnavailable, jsonEnvelope: true},
		{
			name: "OPTIONS", method: "OPTIONS", target: "/dav/",
			wantStatus: 200, wantCode: VerdictOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, tc.method, tc.target, tc.headers, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("X-Bunker-Verdict"); got != string(tc.wantCode) {
				t.Fatalf("X-Bunker-Verdict = %q, want %q", got, tc.wantCode)
			}
			if tc.wantAllow && rec.Header().Get("Allow") == "" {
				t.Fatal("405/501 refusal carries no Allow header (RFC 9110 §15.5.6 requires it)")
			}
			body := rec.Body.String()
			if tc.jsonEnvelope {
				if !strings.Contains(body, `"verdict":"`+string(tc.wantCode)+`"`) {
					t.Fatalf("E-4 body does not carry the code: %s", body)
				}
				return
			}
			if tc.wantCode == VerdictOK {
				return
			}
			// A HEAD refusal carries the same headers and, like HEAD itself,
			// no body: the status and the header are the whole answer.
			if tc.method == "HEAD" {
				if body != "" {
					t.Fatalf("HEAD wrote a body: %q", body)
				}
				return
			}
			// §5.3: the code is in the header AND in the body. The body
			// element is the code with underscores hyphenated, unless the
			// refusal uses RFC 4918's own DAV:-namespaced element.
			element := tc.wantElement
			if element == "" {
				element = "<b:" + strings.ReplaceAll(string(tc.wantCode), "_", "-") + ">"
			}
			if !strings.Contains(body, element) {
				t.Fatalf("body does not carry %s: %s", element, body)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
				t.Fatalf("refusal Content-Type = %q, want an XML body", ct)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store on a refusal", cc)
			}
		})
	}
}

// TestHeadHasNoBody proves the HEAD path sets identical headers and writes
// nothing (RFC 9110): a client's cheap way to fetch a base hash (§7.4).
func TestHeadHasNoBody(t *testing.T) {
	h := newTestHandler(t)

	getRec := do(t, h, "GET", "/dav/src/main.go", nil, "")
	if getRec.Code != 200 {
		t.Fatalf("GET status = %d", getRec.Code)
	}
	headRec := do(t, h, "HEAD", "/dav/src/main.go", nil, "")
	if headRec.Code != 200 {
		t.Fatalf("HEAD status = %d", headRec.Code)
	}
	if headRec.Body.Len() != 0 {
		t.Fatalf("HEAD wrote a body of %d bytes", headRec.Body.Len())
	}
	for _, header := range []string{"ETag", "X-Bunker-Hash", "Content-Length", "Last-Modified", "Content-Type"} {
		if got, want := headRec.Header().Get(header), getRec.Header().Get(header); got != want {
			t.Fatalf("HEAD %s = %q, GET %s = %q", header, got, header, want)
		}
	}

	// A refusal on HEAD carries the same headers and no body.
	missing := do(t, h, "HEAD", "/dav/nope.go", nil, "")
	if missing.Code != 404 || missing.Body.Len() != 0 {
		t.Fatalf("HEAD 404 -> status %d, body %q", missing.Code, missing.Body.String())
	}
	if got := missing.Header().Get("X-Bunker-Verdict"); got != string(VerdictNotFound) {
		t.Fatalf("HEAD 404 verdict = %q", got)
	}
}

// TestContentIdentityIsTheBytes proves E-1: the ETag IS the content hash, the
// hash names its algorithm, and a rewrite with identical bytes does NOT change
// it (the touch-without-change rule, §7).
func TestContentIdentityIsTheBytes(t *testing.T) {
	h := newTestHandler(t)
	wantHash := contentHash(fixtureMainBody)

	rec := do(t, h, "GET", "/dav/src/main.go", nil, "")
	if rec.Code != 200 || rec.Body.String() != fixtureMainBody {
		t.Fatalf("GET -> %d %q", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("ETag"), `"`+wantHash+`"`; got != want {
		t.Fatalf("ETag = %q, want %q", got, want)
	}
	if got := rec.Header().Get("X-Bunker-Hash"); got != wantHash {
		t.Fatalf("X-Bunker-Hash = %q, want %q", got, wantHash)
	}
	if strings.HasPrefix(rec.Header().Get("ETag"), "W/") {
		t.Fatal("ETag is weak; E-1 requires a strong validator")
	}

	// Rewrite the file with the SAME bytes and a NEW mtime: the identity is
	// the bytes, so the ETag must not move.
	path := filepath.Join(h.Root(), "src", "main.go")
	stamp := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	again := do(t, h, "GET", "/dav/src/main.go", nil, "")
	if again.Header().Get("X-Bunker-Hash") != wantHash {
		t.Fatalf("hash moved on a touch-without-change: %q", again.Header().Get("X-Bunker-Hash"))
	}
}

// TestConditionalGet proves the stock-client conditional path: 304 on a
// matching If-None-Match, 206 + Content-Range for a single byte range, 416 for
// an unsatisfiable one, and the full body for a multi-range request.
func TestConditionalGet(t *testing.T) {
	h := newTestHandler(t)
	etag := `"` + contentHash(fixtureMainBody) + `"`
	size := len(fixtureMainBody)
	suffix := fixtureMainBody[size-2:]

	cases := []struct {
		name       string
		headers    map[string]string
		wantStatus int
		wantBody   string
		wantRange  string
	}{
		{name: "if-none-match hit", headers: map[string]string{"If-None-Match": etag}, wantStatus: 304},
		{name: "if-none-match miss", headers: map[string]string{"If-None-Match": `"sha256:deadbeef"`}, wantStatus: 200, wantBody: fixtureMainBody},
		{name: "single prefix range", headers: map[string]string{"Range": "bytes=0-6"}, wantStatus: 206, wantBody: fixtureMainBody[:7], wantRange: fmt.Sprintf("bytes 0-6/%d", size)},
		{name: "single suffix range", headers: map[string]string{"Range": "bytes=-2"}, wantStatus: 206, wantBody: suffix, wantRange: fmt.Sprintf("bytes %d-%d/%d", size-2, size-1, size)},
		{name: "unsatisfiable range", headers: map[string]string{"Range": "bytes=99-200"}, wantStatus: 416},
		{name: "multi-range is answered whole", headers: map[string]string{"Range": "bytes=0-1,4-5"}, wantStatus: 200, wantBody: fixtureMainBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, "GET", "/dav/src/main.go", tc.headers, "")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantBody != "" && rec.Body.String() != tc.wantBody {
				t.Fatalf("body = %q, want %q", rec.Body.String(), tc.wantBody)
			}
			if tc.wantRange != "" && rec.Header().Get("Content-Range") != tc.wantRange {
				t.Fatalf("Content-Range = %q, want %q", rec.Header().Get("Content-Range"), tc.wantRange)
			}
			if tc.wantStatus == 416 && rec.Header().Get("Content-Range") != fmt.Sprintf("bytes */%d", size) {
				t.Fatalf("416 Content-Range = %q, want bytes */%d", rec.Header().Get("Content-Range"), size)
			}
		})
	}
}

// TestSymlinkEscapeIsRefused proves the confinement rule (§6.1 step 1): a
// symlink inside the served tree that resolves outside it is refused, for both
// the link itself and a path through it, and it is refused as "outside the
// tree" rather than as "not found".
func TestSymlinkEscapeIsRefused(t *testing.T) {
	h := newTestHandler(t)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), "not yours\n")
	if err := os.Symlink(outside, filepath.Join(h.Root(), "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, target := range []string{"/dav/escape", "/dav/escape/secret.txt"} {
		rec := do(t, h, "GET", target, nil, "")
		if rec.Code != 403 {
			t.Fatalf("GET %s = %d, want 403 (body=%s)", target, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("X-Bunker-Verdict"); got != string(VerdictWorkspaceInvalid) {
			t.Fatalf("GET %s verdict = %q, want workspace_invalid", target, got)
		}
	}
	// Control: a symlink that stays INSIDE the tree is served normally, so the
	// refusal above is about the escape and not about symlinks as such.
	if err := os.Symlink(filepath.Join(h.Root(), "README.md"), filepath.Join(h.Root(), "inside")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	rec := do(t, h, "GET", "/dav/inside", nil, "")
	if rec.Code != 200 || rec.Body.String() != fixtureReadme {
		t.Fatalf("in-tree symlink GET = %d %q, want the target's bytes", rec.Code, rec.Body.String())
	}
}

// TestTreeAndRevisionTokens proves E-3's two headers are on every response,
// that a git work tree reports git:<40 hex>, and that a stale tree token is
// refused before the method runs.
func TestTreeAndRevisionTokens(t *testing.T) {
	head := strings.Repeat("a1b2c3d4", 5)
	h := newTestHandler(t)
	withGitFixture(t, h.Root(), head)

	rec := do(t, h, "GET", "/dav/README.md", nil, "")
	if got, want := rec.Header().Get("X-Bunker-Rev"), "git:"+head; got != want {
		t.Fatalf("X-Bunker-Rev = %q, want %q", got, want)
	}
	tree := rec.Header().Get("X-Bunker-Tree")
	if !strings.HasPrefix(tree, "tree:") || len(tree) != len("tree:")+16 {
		t.Fatalf("X-Bunker-Tree = %q, want a tree:<16 hex> token", tree)
	}
	if got := rec.Header().Get("X-Bunker-Proto"); got != "HTTP/1.1" {
		t.Fatalf("X-Bunker-Proto = %q, want the observed HTTP/1.1", got)
	}
	if got := rec.Header().Get("X-Bunker-Capabilities"); got != "1" {
		t.Fatalf("X-Bunker-Capabilities = %q, want 1", got)
	}

	// The current token is accepted.
	ok := do(t, h, "GET", "/dav/README.md", map[string]string{"X-Bunker-Tree": tree}, "")
	if ok.Code != 200 {
		t.Fatalf("matching tree token -> %d", ok.Code)
	}
	// A stale token is refused with both identities named, before the method.
	stale := do(t, h, "PUT", "/dav/README.md", map[string]string{"X-Bunker-Tree": "tree:ffffffffffffffff"}, "x")
	if stale.Code != 409 {
		t.Fatalf("stale tree -> %d", stale.Code)
	}
	body := stale.Body.String()
	if !strings.Contains(body, "<b:expected>tree:ffffffffffffffff</b:expected>") || !strings.Contains(body, "<b:current>"+tree+"</b:current>") {
		t.Fatalf("stale_tree body names neither/both identities: %s", body)
	}
	if got, want := stale.Header().Get("X-Bunker-Verdict"), string(VerdictStaleTree); got != want {
		t.Fatalf("verdict = %q, want stale_tree", got)
	}
}

// TestTreeTokenIsPerTree proves the tree token separates two different trees —
// the server half of AC-7 (an agent destroyed and re-created must not answer
// as the old one).
func TestTreeTokenIsPerTree(t *testing.T) {
	first := newTestHandler(t)
	second := newTestHandler(t)
	token := func(h *Handler) string {
		return do(t, h, "GET", "/dav/README.md", nil, "").Header().Get("X-Bunker-Tree")
	}
	if token(first) == token(second) {
		t.Fatal("two different served roots produced the same tree token")
	}
	if token(first) == "" {
		t.Fatal("no tree token on the response")
	}
}

// TestOptionsAsteriskHasNoDAVHeader proves RFC 4918 §10.1's asterisk-form rule
// (A-2's negative control): whole-server OPTIONS does not advertise DAV, so
// per-URI discovery stays honest.
func TestOptionsAsteriskHasNoDAVHeader(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest("OPTIONS", "/", nil)
	req.RequestURI = "*"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("OPTIONS * -> %d", rec.Code)
	}
	if rec.Header().Get("DAV") != "" {
		t.Fatalf("OPTIONS * advertised DAV: %q", rec.Header().Get("DAV"))
	}
	if rec.Header().Get("Allow") == "" {
		t.Fatal("OPTIONS * carries no Allow header")
	}

	// Per-URI OPTIONS does advertise DAV: 1 and the extension set (§10.1).
	perURI := do(t, h, "OPTIONS", "/dav/", nil, "")
	if got := perURI.Header().Get("DAV"); got != "1" {
		t.Fatalf("OPTIONS DAV = %q, want 1 (LOCK is not live, so class 2 must not be advertised)", got)
	}
	if got := perURI.Header().Get("X-Bunker-Extensions"); got != ExtensionsHeader {
		t.Fatalf("X-Bunker-Extensions = %q, want %q", got, ExtensionsHeader)
	}
	if allow := perURI.Header().Get("Allow"); strings.Contains(allow, "LOCK") || !strings.Contains(allow, "PROPFIND") {
		t.Fatalf("Allow = %q, want the served set without LOCK", allow)
	}
}

func TestMethodMatrixNoWritesOnRefusal(t *testing.T) {
	h := newTestHandler(t)
	before := treeDigest(t, h.Root())
	// Every refusal in the matrix runs against the same tree; the mutating
	// ones (PUT/DELETE/MKCOL/COPY/MOVE) are aimed at paths they must not
	// touch.
	for _, req := range []struct {
		method, target string
		headers        map[string]string
		body           string
	}{
		{"PUT", "/dav/src/util.go", map[string]string{"X-Bunker-Hash": "sha256:" + strings.Repeat("0", 64)}, "mutating bytes"},
		{"COPY", "/dav/README.md", map[string]string{"Destination": "/elsewhere/x"}, ""},
		{"MOVE", "/dav/README.md", map[string]string{"Destination": "/dav/README.md"}, ""},
		{"MKCOL", "/dav/a/b/c", nil, ""},
		{"MKCOL", "/dav/README.md", nil, ""},
		{"DELETE", "/dav/README.md", map[string]string{"If-Match": `"sha256:` + strings.Repeat("0", 64) + `"`}, ""},
	} {
		rec := do(t, h, req.method, req.target, req.headers, req.body)
		if rec.Code >= 200 && rec.Code < 300 {
			t.Fatalf("%s %s unexpectedly succeeded with %d", req.method, req.target, rec.Code)
		}
	}
	if after := treeDigest(t, h.Root()); after != before {
		t.Fatal("a refused request mutated the tree")
	}
}

// TestHandlerRefusesAnUnusableRoot proves the fail-before-serve posture: a
// surface over a missing root is an error at construction, never a handler
// that answers 500 to everything.
func TestHandlerRefusesAnUnusableRoot(t *testing.T) {
	if _, err := New(Config{Root: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("New accepted a missing root")
	}
	if _, err := New(Config{Root: ""}); err == nil {
		t.Fatal("New accepted an empty root")
	}
	file := filepath.Join(t.TempDir(), "file.txt")
	mustWrite(t, file, "not a directory\n")
	if _, err := New(Config{Root: file}); err == nil {
		t.Fatal("New accepted a file as the root")
	}
}
