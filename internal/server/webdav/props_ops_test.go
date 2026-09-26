package webdav

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
	"testing"
)

// The multistatus shapes below are namespace-agnostic on purpose: the test
// asserts what a CLIENT sees after parsing, which is the only thing the spec's
// consumers care about.

type msProp struct {
	Items []msPropItem `xml:",any"`
}

type msPropItem struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type msPropstat struct {
	Props  msProp `xml:"prop"`
	Status string `xml:"status"`
}

type msResponse struct {
	Href  string       `xml:"href"`
	Stats []msPropstat `xml:"propstat"`
}

type msDocument struct {
	Responses []msResponse `xml:"response"`
}

func parseMultiStatus(t *testing.T, body string) msDocument {
	t.Helper()
	var doc msDocument
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("multistatus did not parse: %v\n%s", err, body)
	}
	return doc
}

// propNames renders the parsed property names of one propstat group.
func propNames(ps msPropstat) []string {
	out := make([]string, 0, len(ps.Props.Items))
	for _, item := range ps.Props.Items {
		name := item.XMLName.Local
		if item.XMLName.Space != "" {
			name = item.XMLName.Space + ":" + name
		}
		out = append(out, name)
	}
	return out
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestPropfindAllProp proves the §2.2 rule that decides what a directory walk
// costs: allprop returns the CHEAP live properties (including bunkerd:rev and
// bunkerd:tree) and NOT the expensive content hash, which is available only
// when a client names it (§7.4).
func TestPropfindAllProp(t *testing.T) {
	h := newTestHandler(t)

	file := do(t, h, "PROPFIND", "/dav/src/main.go", map[string]string{"Depth": "0"}, "")
	if file.Code != 207 {
		t.Fatalf("PROPFIND file -> %d %s", file.Code, file.Body.String())
	}
	doc := parseMultiStatus(t, file.Body.String())
	if len(doc.Responses) != 1 {
		t.Fatalf("Depth 0 returned %d responses, want 1", len(doc.Responses))
	}
	if doc.Responses[0].Href != "/dav/src/main.go" {
		t.Fatalf("href = %q", doc.Responses[0].Href)
	}
	if len(doc.Responses[0].Stats) != 1 {
		t.Fatalf("want one propstat group, got %d", len(doc.Responses[0].Stats))
	}
	ps := doc.Responses[0].Stats[0]
	if ps.Status != "HTTP/1.1 200 OK" {
		t.Fatalf("propstat status = %q", ps.Status)
	}
	names := propNames(ps)
	for _, want := range []string{"DAV::getetag", "DAV::getcontentlength", "DAV::resourcetype", "DAV::getlastmodified", "urn:bunker:fs:1:rev", "urn:bunker:fs:1:tree"} {
		if !hasName(names, want) {
			t.Fatalf("allprop on a file is missing %s (got %v)", want, names)
		}
	}
	if hasName(names, "urn:bunker:fs:1:hash") {
		t.Fatalf("allprop returned the expensive bunkerd:hash: %v", names)
	}
	if hasName(names, "DAV::supportedlock") || hasName(names, "DAV::lockdiscovery") {
		t.Fatalf("allprop advertised lock properties while LOCK is not live: %v", names)
	}
	// The etag value is the content hash.
	for _, item := range ps.Props.Items {
		if item.XMLName.Local == "getetag" && item.Value != `"`+contentHash(fixtureMainBody)+`"` {
			t.Fatalf("getetag = %q", item.Value)
		}
	}

	// allprop on a collection: no content properties at all.
	dir := do(t, h, "PROPFIND", "/dav/src", map[string]string{"Depth": "0"}, "")
	dirDoc := parseMultiStatus(t, dir.Body.String())
	if dirDoc.Responses[0].Href != "/dav/src/" {
		t.Fatalf("collection href = %q, want a trailing slash", dirDoc.Responses[0].Href)
	}
	dirNames := propNames(dirDoc.Responses[0].Stats[0])
	if hasName(dirNames, "DAV::getcontentlength") || hasName(dirNames, "DAV::getetag") {
		t.Fatalf("a collection reported content properties: %v", dirNames)
	}
	if !hasName(dirNames, "DAV::resourcetype") {
		t.Fatalf("a collection is missing resourcetype: %v", dirNames)
	}
}

// TestPropfindNamedProperties proves the named-prop form, the per-property 404
// group, and that a foreign namespace keeps its identity through an allocated
// prefix.
func TestPropfindNamedProperties(t *testing.T) {
	h := newTestHandler(t)
	body := `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:b="urn:bunker:fs:1" xmlns:z="urn:example:1">
  <D:prop><D:getetag/><D:getcontentlength/><b:hash/><z:colour/></D:prop>
</D:propfind>`

	rec := do(t, h, "PROPFIND", "/dav/src/main.go", map[string]string{"Depth": "0", "Content-Type": "application/xml; charset=utf-8"}, body)
	if rec.Code != 207 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	parsed := parseMultiStatus(t, rec.Body.String())
	if len(parsed.Responses) != 1 || len(parsed.Responses[0].Stats) != 2 {
		t.Fatalf("want two propstat groups, got %+v", parsed.Responses)
	}
	present, missing := parsed.Responses[0].Stats[0], parsed.Responses[0].Stats[1]
	if present.Status != "HTTP/1.1 200 OK" || missing.Status != "HTTP/1.1 404 Not Found" {
		t.Fatalf("propstat statuses = %q / %q", present.Status, missing.Status)
	}
	if !hasName(propNames(present), "urn:bunker:fs:1:hash") {
		t.Fatalf("named b:hash was not served: %v", propNames(present))
	}
	if !hasName(propNames(missing), "urn:example:1:colour") {
		t.Fatalf("the unknown property lost its namespace: %v", propNames(missing))
	}
	// The foreign namespace is declared on the root so the response is
	// well-formed XML (the parse above already proves it, but the prefix must
	// be a real declaration rather than a bare prefix).
	if !strings.Contains(rec.Body.String(), `xmlns:e0="urn:example:1"`) {
		t.Fatalf("foreign namespace not declared in the document: %s", rec.Body.String())
	}
}

// TestPropfindDepth1ListsMembers proves Depth: 1 answers with the collection
// and its members, each with its own response — the non-refused recursion.
func TestPropfindDepth1ListsMembers(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, "PROPFIND", "/dav/src", map[string]string{"Depth": "1"}, "")
	if rec.Code != 207 {
		t.Fatalf("status = %d", rec.Code)
	}
	doc := parseMultiStatus(t, rec.Body.String())
	if len(doc.Responses) != 3 {
		t.Fatalf("Depth 1 returned %d responses, want the collection plus two files", len(doc.Responses))
	}
	hrefs := []string{doc.Responses[0].Href, doc.Responses[1].Href, doc.Responses[2].Href}
	want := []string{"/dav/src/", "/dav/src/main.go", "/dav/src/util.go"}
	for i := range want {
		if hrefs[i] != want[i] {
			t.Fatalf("hrefs = %v, want %v", hrefs, want)
		}
	}
}

// TestPropfindPropName proves the RFC 4918 §9.1 propname form: names only, all
// of them, with no values.
func TestPropfindPropName(t *testing.T) {
	h := newTestHandler(t)
	body := `<D:propfind xmlns:D="DAV:"><D:propname/></D:propfind>`
	rec := do(t, h, "PROPFIND", "/dav/src/main.go", map[string]string{"Depth": "0"}, body)
	if rec.Code != 207 {
		t.Fatalf("status = %d", rec.Code)
	}
	doc := parseMultiStatus(t, rec.Body.String())
	ps := doc.Responses[0].Stats[0]
	names := propNames(ps)
	if !hasName(names, "DAV::getetag") || !hasName(names, "urn:bunker:fs:1:tree") {
		t.Fatalf("propname is missing properties: %v", names)
	}
	for _, item := range ps.Props.Items {
		if item.Value != "" {
			t.Fatalf("propname returned a value for %s: %q", item.XMLName.Local, item.Value)
		}
	}
}

// TestProppatchRefusesPerProperty proves §9.2's shape: the method is
// implemented, every property is refused, the first refusal names its cause,
// and the rest of the atomic group answers 424.
func TestProppatchRefusesPerProperty(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantVerdict Verdict
		wantError   string
	}{
		{
			name: "dead property",
			body: `<D:propertyupdate xmlns:D="DAV:" xmlns:z="urn:example:1">
  <D:set><D:prop><z:colour>blue</z:colour></D:prop></D:set>
  <D:set><D:prop><D:displayname>Source</D:displayname></D:prop></D:set>
</D:propertyupdate>`,
			wantVerdict: VerdictDeadPropertiesUnsupported,
			wantError:   "<b:dead-properties-unsupported>",
		},
		{
			name: "live property",
			body: `<D:propertyupdate xmlns:D="DAV:">
  <D:set><D:prop><D:displayname>Source</D:displayname></D:prop></D:set>
</D:propertyupdate>`,
			wantVerdict: VerdictProtectedProperty,
			wantError:   "<D:cannot-modify-protected-property>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(t)
			rec := do(t, h, "PROPPATCH", "/dav/src", map[string]string{"Content-Type": "application/xml; charset=utf-8"}, tc.body)
			if rec.Code != 207 {
				t.Fatalf("status = %d, want 207 (the method is implemented)", rec.Code)
			}
			if got := rec.Header().Get("X-Bunker-Verdict"); got != string(tc.wantVerdict) {
				t.Fatalf("verdict = %q, want %q", got, tc.wantVerdict)
			}
			if !strings.Contains(rec.Body.String(), tc.wantError) {
				t.Fatalf("propstat error element missing: %s", rec.Body.String())
			}
			doc := parseMultiStatus(t, rec.Body.String())
			stats := doc.Responses[0].Stats
			if stats[0].Status != "HTTP/1.1 403 Forbidden" {
				t.Fatalf("first propstat status = %q, want 403", stats[0].Status)
			}
			for _, ps := range stats[1:] {
				if ps.Status != "HTTP/1.1 424 Failed Dependency" {
					t.Fatalf("remaining propstat status = %q, want 424 (all-or-nothing)", ps.Status)
				}
			}
		})
	}
}

// TestProppatchMalformedBody proves the malformed/non-propertyupdate case is a
// loud 400 rather than a silently different answer.
func TestProppatchMalformedBody(t *testing.T) {
	h := newTestHandler(t)
	for _, body := range []string{"", "not xml at all", `<D:anything xmlns:D="DAV:"/>`} {
		rec := do(t, h, "PROPPATCH", "/dav/src", nil, body)
		if rec.Code != 400 {
			t.Fatalf("body %q -> %d, want 400", body, rec.Code)
		}
		if got := rec.Header().Get("X-Bunker-Verdict"); got != string(VerdictBadArguments) {
			t.Fatalf("verdict = %q, want bad_arguments", got)
		}
	}
}

// capabilityJSON is the decoded capability document.
type capabilityJSON struct {
	Surface         string         `json:"surface"`
	DocumentVersion int            `json:"document_version"`
	Classes         []string       `json:"classes"`
	Methods         []string       `json:"methods"`
	Extensions      map[string]any `json:"extensions"`
	Transports      map[string]struct {
		Available bool   `json:"available"`
		Alpn      string `json:"alpn"`
		Mode      string `json:"mode"`
		AltSvc    string `json:"alt_svc"`
	} `json:"transports"`
	Limits struct {
		PropfindDepth         []int `json:"propfind_depth"`
		PropfindDepthInfinity bool  `json:"propfind_depth_infinity"`
		AllPropHashes         bool  `json:"propfind_allprop_hashes"`
	} `json:"limits"`
	Server struct {
		Build string `json:"build"`
		Proto string `json:"proto"`
		Rev   string `json:"rev"`
		Tree  string `json:"tree"`
	} `json:"server"`
	Degradations []struct {
		Capability string `json:"capability"`
		Scope      string `json:"scope"`
		Mode       string `json:"mode"`
		Detail     string `json:"detail"`
	} `json:"degradations"`
}

func (c capabilityJSON) degradation(capability string) (string, bool) {
	for _, d := range c.Degradations {
		if d.Capability == capability {
			return d.Scope, true
		}
	}
	return "", false
}

// TestCapabilityDocumentReportsReality proves §4.2 rule 1: the document
// describes the RUNNING process. The two arms are the TLS/h2c configurations,
// and the negative control is the cleartext default, where h2 must not claim
// to be available.
func TestCapabilityDocumentReportsReality(t *testing.T) {
	cleartext := newTestHandler(t)
	tlsOn := newTestHandler(t, func(c *Config) { c.TLS = true })
	h2cOn := newTestHandler(t, func(c *Config) { c.TLS = true; c.H2C = true })

	fetch := func(h *Handler) capabilityJSON {
		rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "capabilities"}, "")
		if rec.Code != 200 {
			t.Fatalf("capabilities -> %d %s", rec.Code, rec.Body.String())
		}
		var env struct {
			OK     bool `json:"ok"`
			Result struct {
				Capabilities capabilityJSON `json:"capabilities"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("envelope did not parse: %v", err)
		}
		if !env.OK {
			t.Fatalf("capabilities envelope not ok: %s", rec.Body.String())
		}
		out := env.Result.Capabilities
		if out.Surface != Surface || out.DocumentVersion != DocumentVersion {
			t.Fatalf("surface/document_version = %q/%d", out.Surface, out.DocumentVersion)
		}
		return out
	}

	plain := fetch(cleartext)
	if plain.Transports["h2"].Available {
		t.Fatal("h2 reported available on a cleartext listener with h2c off")
	}
	if plain.Transports["h2c"].Available {
		t.Fatal("h2c reported available while the opt-in is off")
	}
	if plain.Transports["h3"].Available {
		t.Fatal("h3 reported available; this build has no QUIC listener (BFS-007)")
	}
	if !plain.Transports["http/1.1"].Available {
		t.Fatal("http/1.1 must always be available")
	}
	if scope, ok := plain.degradation("h2"); !ok || scope != "transport" {
		t.Fatalf("the cleartext h2 absence is not enumerated as a transport degradation: %+v", plain.Degradations)
	}
	if _, ok := plain.degradation("watch"); !ok {
		t.Fatalf("the absent watcher is not enumerated: %+v", plain.Degradations)
	}
	if !hasName(plain.Classes, "1") || hasName(plain.Classes, "2") {
		t.Fatalf("classes = %v, want [1] while LOCK is not live", plain.Classes)
	}
	if hasName(plain.Methods, "LOCK") {
		t.Fatalf("the document claims LOCK is served: %v", plain.Methods)
	}
	if !plain.Limits.PropfindDepthInfinity == false || len(plain.Limits.PropfindDepth) != 2 {
		t.Fatalf("limits do not report the finite-depth truth: %+v", plain.Limits)
	}
	if plain.Server.Proto != "HTTP/1.1" {
		t.Fatalf("server.proto = %q, want the version the request arrived on", plain.Server.Proto)
	}

	withTLS := fetch(tlsOn)
	if !withTLS.Transports["h2"].Available {
		t.Fatal("h2 not reported available with TLS on, where ALPN h2 works from the stdlib")
	}
	if withTLS.Transports["h2c"].Available {
		t.Fatal("h2c reported available while the opt-in is off")
	}
	withH2C := fetch(h2cOn)
	if !withH2C.Transports["h2c"].Available {
		t.Fatal("h2c not reported available after the explicit opt-in")
	}
	if scope, ok := withH2C.degradation("h2c"); !ok || scope != "transport" {
		t.Fatalf("the h2c prior-knowledge caveat is not reported: %+v", withH2C.Degradations)
	}
}

// TestSnapshotOp proves E-4's in-process op: a whole-subtree listing in one
// call, paths relative to the root, and truncation REPORTED rather than
// silently shortened (A-11).
func TestSnapshotOp(t *testing.T) {
	h := newTestHandler(t)

	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "snapshot"},
		`{"path":"src","depth":"infinity","include_hash":true}`)
	if rec.Code != 200 {
		t.Fatalf("snapshot -> %d %s", rec.Code, rec.Body.String())
	}
	var env struct {
		OK        bool `json:"ok"`
		Op        string
		Verdict   string `json:"verdict"`
		Truncated bool   `json:"truncated"`
		Proto     string `json:"proto"`
		Result    struct {
			Count   int `json:"count"`
			Entries []struct {
				Path string `json:"path"`
				Type string `json:"type"`
				Hash string `json:"hash"`
			} `json:"entries"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if !env.OK || env.Verdict != "ok" || env.Op != "snapshot" || env.Truncated {
		t.Fatalf("envelope = %+v", env)
	}
	if env.Result.Count != len(env.Result.Entries) || env.Result.Count != 3 {
		t.Fatalf("snapshot returned %d entries (count=%d), want the dir and its two files", len(env.Result.Entries), env.Result.Count)
	}
	byPath := map[string]string{}
	for _, e := range env.Result.Entries {
		byPath[e.Path] = e.Type
	}
	if byPath["src"] != "dir" || byPath["src/main.go"] != "file" || byPath["src/util.go"] != "file" {
		t.Fatalf("snapshot paths/types = %+v", byPath)
	}
	for _, e := range env.Result.Entries {
		if e.Type == "file" && e.Hash != contentHash(fixtureMainBody) && e.Hash != contentHash(fixtureUtilBody) {
			t.Fatalf("snapshot hash for %s = %q", e.Path, e.Hash)
		}
	}
	if env.Proto != "HTTP/1.1" {
		t.Fatalf("envelope proto = %q", env.Proto)
	}
	if got := rec.Header().Get("X-Bunker-Op"); got != "snapshot" {
		t.Fatalf("X-Bunker-Op = %q", got)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("E-4 responses must not be cached: Cache-Control = %q", cc)
	}

	// A result cap too small for even one entry is a loud 413, not an empty
	// success.
	tiny := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "snapshot", "X-Bunker-Max-Bytes": "256"},
		`{"path":"src","depth":"infinity"}`)
	if tiny.Code != 413 {
		t.Fatalf("tiny cap -> %d, want 413 (body=%s)", tiny.Code, tiny.Body.String())
	}
	if got := tiny.Header().Get("X-Bunker-Verdict"); got != string(VerdictResultTooLarge) {
		t.Fatalf("tiny cap verdict = %q", got)
	}

	// A cap that fits some entries reports the truncation in the envelope.
	partial := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "snapshot", "X-Bunker-Max-Bytes": "380"}, `{"path":"src","depth":"infinity"}`)
	if partial.Code != 200 {
		t.Fatalf("partial cap -> %d %s", partial.Code, partial.Body.String())
	}
	var partialEnv struct {
		Truncated bool `json:"truncated"`
		Result    struct {
			Count int `json:"count"`
		} `json:"result"`
	}
	if err := json.Unmarshal(partial.Body.Bytes(), &partialEnv); err != nil {
		t.Fatalf("partial envelope: %v", err)
	}
	if !partialEnv.Truncated {
		t.Fatalf("a capped snapshot did not report truncation: %s", partial.Body.String())
	}
	if partialEnv.Result.Count >= 3 {
		t.Fatalf("the cap did not bite: %d entries", partialEnv.Result.Count)
	}

	// Invalid arguments are named, never guessed.
	bad := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "snapshot"}, `{"depth":"3"}`)
	if bad.Code != 400 {
		t.Fatalf("bad depth -> %d", bad.Code)
	}
	badMax := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "snapshot", "X-Bunker-Max-Bytes": "lots"}, "")
	if badMax.Code != 400 {
		t.Fatalf("bad max-bytes -> %d", badMax.Code)
	}
}

// TestOpsAreReadOnly proves E-4's load-bearing invariant: every op leaves the
// tree byte-identical (including any repository metadata), so a retried POST is
// harmless. The control proves the digest would notice a mutation.
func TestOpsAreReadOnly(t *testing.T) {
	h := newTestHandler(t)
	withGitFixture(t, h.Root(), strings.Repeat("ab", 20))

	before := treeDigest(t, h.Root())
	for _, op := range []struct {
		name string
		body string
	}{
		{"capabilities", ""},
		{"snapshot", `{"path":".","depth":"infinity","include_hash":true}`},
		{"status", `{}`},
		{"watch", `{}`},
		{"frobnicate", `{}`},
	} {
		rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": op.name}, op.body)
		if rec.Code >= 500 && rec.Code != 501 {
			t.Fatalf("op %s -> %d", op.name, rec.Code)
		}
	}
	if after := treeDigest(t, h.Root()); after != before {
		t.Fatal("an X-Bunker-Op call mutated the served tree")
	}

	// Mutation control: the digest is sensitive, so the assertion above is
	// not vacuous.
	if rec := do(t, h, "PUT", "/dav/README.md", nil, "changed\n"); rec.Code != 204 {
		t.Fatalf("control PUT -> %d", rec.Code)
	}
	if after := treeDigest(t, h.Root()); after == before {
		t.Fatal("the tree digest does not detect a write, so the read-only assertion proves nothing")
	}
}

var _ = fmt.Sprintf
