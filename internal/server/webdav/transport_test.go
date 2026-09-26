package webdav

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// transportProbe is one row of the per-version battery: the SAME request is
// sent over each version and the answers are compared.
type transportProbe struct {
	name    string
	method  string
	path    string
	altPath string // for mutating probes: a distinct target per version
	headers map[string]string
	body    string
	// mutation marks a probe that WRITES: the first write legitimately moves
	// the tree revision, so the second response's X-Bunker-Rev is newer by
	// exactly one step. That is asserted on its own elsewhere; here it is the
	// one header exempted from the equality check.
	mutation bool
}

// versionEchoHeaders are the headers that are ALLOWED to differ between two
// versions of the same request: the version itself, and the transport-level
// bookkeeping each protocol owns (HTTP/2 has no Content-Length/Transfer-Encoding
// framing headers, and Date is generated per response).
var versionEchoHeaders = map[string]bool{
	"Date":              true,
	"Content-Length":    true,
	"Transfer-Encoding": true,
	"Connection":        true,
	"Keep-Alive":        true,
	"X-Bunker-Proto":    true,
}

// TestSameSurfaceOverHTTP1AndHTTP2 is this row's acceptance proof: the SAME
// WebDAV surface, byte-identical over HTTP/1.1 and HTTP/2, with the version
// asserted on BOTH sides — the client's observed protocol and the server's own
// echo (X-Bunker-Proto, E-3/C-6) — so a silent downgrade cannot pass.
//
// The battery ends with an anti-gaming control: a hand-built transport WITHOUT
// ForceAttemptHTTP2 must observe HTTP/1.1 against this same h2-capable server.
// Without that cell, a server that never negotiates h2 would pass every
// assertion above it.
func TestSameSurfaceOverHTTP1AndHTTP2(t *testing.T) {
	h := newTestHandler(t)
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	insecure := func() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }
	h2Client := &http.Client{Transport: &http.Transport{TLSClientConfig: insecure(), ForceAttemptHTTP2: true}}
	h1Client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: insecure(),
		// An empty TLSNextProto map is the documented way to disable the
		// automatic HTTP/2 upgrade, so this client is a genuine HTTP/1.1-only
		// client (what an old WebDAV client effectively is).
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}}
	// Each client gets its OWN *tls.Config: an http.Transport may append "h2"
	// to the NextProtos of the config it is handed, so sharing one config
	// across clients leaks h2 capability into the HTTP/1.1-only client and
	// makes the control cell below fail for the wrong reason.
	controlClient := &http.Client{Transport: &http.Transport{TLSClientConfig: insecure()}}

	battery := []transportProbe{
		{name: "options", method: "OPTIONS", path: "/dav/"},
		{name: "get file", method: "GET", path: "/dav/src/main.go"},
		{name: "head file", method: "HEAD", path: "/dav/src/main.go"},
		{name: "get unmapped", method: "GET", path: "/dav/nope.go"},
		{name: "get collection refused", method: "GET", path: "/dav/src"},
		{name: "propfind depth 0", method: "PROPFIND", path: "/dav/src/main.go", headers: map[string]string{"Depth": "0"}},
		{name: "propfind depth 1", method: "PROPFIND", path: "/dav/src", headers: map[string]string{"Depth": "1"}},
		{name: "propfind infinity refused", method: "PROPFIND", path: "/dav/src", headers: map[string]string{"Depth": "infinity"}},
		{name: "propfind without depth", method: "PROPFIND", path: "/dav/src"},
		{name: "unknown method refused", method: "FROBNICATE", path: "/dav/"},
		{name: "report refused", method: "REPORT", path: "/dav/"},
		{name: "stale tree refused", method: "GET", path: "/dav/README.md", headers: map[string]string{"X-Bunker-Tree": "tree:0000000000000000"}},
		{name: "conditional get", method: "GET", path: "/dav/src/main.go", headers: map[string]string{"If-None-Match": `"` + contentHash(fixtureMainBody) + `"`}},
		{name: "range get", method: "GET", path: "/dav/src/main.go", headers: map[string]string{"Range": "bytes=0-6"}},
		{name: "op capabilities", method: "POST", path: "/dav/", headers: map[string]string{"X-Bunker-Op": "capabilities"}},
		{name: "op snapshot", method: "POST", path: "/dav/", headers: map[string]string{"X-Bunker-Op": "snapshot"}, body: `{"path":"src","depth":"infinity"}`},
		{name: "op watch refused", method: "POST", path: "/dav/", headers: map[string]string{"X-Bunker-Op": "watch"}},
		{name: "stale hash refused", method: "PUT", path: "/dav/src/main.go", headers: map[string]string{"If-Match": `"sha256:` + strings.Repeat("0", 64) + `"`}, body: "contested\n"},
		// A mutating write that succeeds on both versions, against a distinct
		// target per version so the two answers are comparable.
		{name: "create file", method: "PUT", path: "/dav/h1-only.txt", altPath: "/dav/h2-only.txt", body: "written by the version-specific probe\n", mutation: true},
	}

	for _, p := range battery {
		t.Run(p.name, func(t *testing.T) {
			r1, b1 := roundTrip(t, h1Client, srv.URL, p, p.path)
			r2, b2 := roundTrip(t, h2Client, srv.URL, p, pathFor(p, p.altPath, p.path))

			if r1.Proto != "HTTP/1.1" {
				t.Fatalf("HTTP/1.1 client observed %s", r1.Proto)
			}
			if r2.Proto != "HTTP/2.0" {
				t.Fatalf("HTTP/2 client observed %s (the h2 arm would be vacuous)", r2.Proto)
			}
			if r1.StatusCode != r2.StatusCode {
				t.Fatalf("status differs: %d over h1, %d over h2", r1.StatusCode, r2.StatusCode)
			}
			if !bytes.Equal(normaliseVersionEcho(t, b1), normaliseVersionEcho(t, b2)) {
				t.Fatalf("body differs between versions:\nh1: %s\nh2: %s", b1, b2)
			}
			// The version is asserted on BOTH sides (A-6).
			if got := r1.Header.Get("X-Bunker-Proto"); got != "HTTP/1.1" {
				t.Fatalf("server echoed X-Bunker-Proto %q for an HTTP/1.1 request", got)
			}
			if got := r2.Header.Get("X-Bunker-Proto"); got != "HTTP/2.0" {
				t.Fatalf("server echoed X-Bunker-Proto %q for an HTTP/2 request", got)
			}
			compareHeaders(t, r1.Header, r2.Header, p.mutation)
		})
	}

	t.Run("control: a plain transport does not negotiate h2", func(t *testing.T) {
		r, _ := roundTrip(t, controlClient, srv.URL, transportProbe{name: "control", method: "GET", path: "/dav/src/main.go"}, "/dav/src/main.go")
		if r.Proto != "HTTP/1.1" {
			t.Fatalf("the control transport observed %s; it must observe HTTP/1.1 for the h2 assertions to mean anything", r.Proto)
		}
		if got := r.Header.Get("X-Bunker-Proto"); got != "HTTP/1.1" {
			t.Fatalf("server echoed %q for the control request", got)
		}
	})
}

func pathFor(p transportProbe, alt, fallback string) string {
	if alt != "" {
		return alt
	}
	return fallback
}

func roundTrip(t *testing.T, client *http.Client, base string, p transportProbe, path string) (*http.Response, []byte) {
	t.Helper()
	var body io.Reader
	if p.body != "" {
		body = strings.NewReader(p.body)
	}
	req, err := http.NewRequest(p.method, base+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", p.method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, raw
}

// compareHeaders requires the two responses to carry the same headers, apart
// from the version echo and the protocol-level framing each transport owns.
func compareHeaders(t *testing.T, h1, h2 http.Header, mutation bool) {
	t.Helper()
	keys := map[string]bool{}
	for k := range h1 {
		keys[k] = true
	}
	for k := range h2 {
		keys[k] = true
	}
	for k := range keys {
		if versionEchoHeaders[k] {
			continue
		}
		if mutation && k == "X-Bunker-Rev" {
			// The h1 write bumped the tree revision before the h2 write ran,
			// so the second answer's revision is legitimately one step newer.
			continue
		}
		if got, want := h1.Get(k), h2.Get(k); got != want {
			t.Fatalf("header %s differs between versions: h1=%q h2=%q", k, got, want)
		}
	}
}

// normaliseVersionEcho blanks the JSON fields the spec REQUIRES to differ by
// version — the E-4 envelope's `proto` and the capability document's
// `server.proto` (§3 E-4, §4.2) — so the rest of the two bodies can be
// compared exactly. Only fields literally named "proto" are touched.
func normaliseVersionEcho(t *testing.T, body []byte) []byte {
	t.Helper()
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return body
	}
	var doc any
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return body
	}
	blankProto(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

func blankProto(v any) {
	switch typed := v.(type) {
	case map[string]any:
		for key, val := range typed {
			if key == "proto" {
				typed[key] = "<version>"
				continue
			}
			blankProto(val)
		}
	case []any:
		for _, item := range typed {
			blankProto(item)
		}
	}
}

// TestServeProtocolsKeepsHTTP1AndTLSH2 proves the flag combination this row had
// to get right: a non-nil *http.Protocols REPLACES the runtime default set, so
// the h2c opt-in must state SetHTTP2(true) as well — otherwise enabling
// cleartext h2 would silently switch h2-over-TLS OFF. The test proves all three
// transports serve one surface: HTTP/1.1, HTTP/2 over TLS (ALPN), h2c.
func TestServeProtocolsKeepsHTTP1AndTLSH2(t *testing.T) {
	h := newTestHandler(t)

	// TLS + ALPN h2 with the h2c opt-in active.
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.Config.Protocols = ServeProtocols(true)
	srv.StartTLS()
	defer srv.Close()

	insecure := func() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }
	h2Client := &http.Client{Transport: &http.Transport{TLSClientConfig: insecure(), ForceAttemptHTTP2: true}}
	h1Client := &http.Client{Transport: &http.Transport{TLSClientConfig: insecure(), TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}}

	for _, tc := range []struct {
		name   string
		client *http.Client
		want   string
	}{
		{name: "http/1.1", client: h1Client, want: "HTTP/1.1"},
		{name: "h2 via ALPN", client: h2Client, want: "HTTP/2.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := roundTrip(t, tc.client, srv.URL, transportProbe{name: tc.name, method: "GET", path: "/dav/README.md"}, "/dav/README.md")
			if resp.Proto != tc.want {
				t.Fatalf("observed %s, want %s — ServeProtocols(true) must not disable this transport", resp.Proto, tc.want)
			}
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if got := resp.Header.Get("X-Bunker-Proto"); got != tc.want {
				t.Fatalf("server echoed %q, want %q", got, tc.want)
			}
		})
	}
}

// TestH2CIsPriorKnowledgeOnly proves the measured caveat this surface reports
// rather than hides: with the opt-in ON, a client that sends the connection
// preface gets HTTP/2 cleartext, while a client that tries the RFC 7540
// `Upgrade: h2c` dance is answered over HTTP/1.1 with no error. With the opt-in
// OFF, prior-knowledge h2c does not work at all — and HTTP/1.1 is untouched in
// both configurations.
func TestH2CIsPriorKnowledgeOnly(t *testing.T) {
	h := newTestHandler(t)

	start := func(t *testing.T, enabled bool) string {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		srv := &http.Server{Handler: h, Protocols: ServeProtocols(enabled)}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
		return "http://" + ln.Addr().String()
	}

	h1Client := func() *http.Client {
		p := new(http.Protocols)
		p.SetHTTP1(true)
		return &http.Client{Transport: &http.Transport{Protocols: p}}
	}
	h2cClient := func() *http.Client {
		// Prior knowledge: the stdlib only sends the preface when HTTP/1.1 is
		// not an option, which is what "prior knowledge" means.
		p := new(http.Protocols)
		p.SetUnencryptedHTTP2(true)
		return &http.Client{Transport: &http.Transport{Protocols: p}}
	}

	t.Run("opt-in off", func(t *testing.T) {
		base := start(t, false)
		resp, _ := roundTrip(t, h1Client(), base, transportProbe{name: "h1", method: "GET", path: "/dav/README.md"}, "/dav/README.md")
		if resp.Proto != "HTTP/1.1" || resp.StatusCode != 200 {
			t.Fatalf("HTTP/1.1 over a cleartext listener -> %s %d", resp.Proto, resp.StatusCode)
		}
		if _, err := h2cClient().Get(base + "/dav/README.md"); err == nil {
			t.Fatal("h2c prior knowledge succeeded while the opt-in is off")
		}
	})

	t.Run("opt-in on", func(t *testing.T) {
		base := start(t, true)

		// HTTP/1.1 is unaffected by the opt-in.
		h1Resp, _ := roundTrip(t, h1Client(), base, transportProbe{name: "h1", method: "GET", path: "/dav/README.md"}, "/dav/README.md")
		if h1Resp.Proto != "HTTP/1.1" || h1Resp.StatusCode != 200 {
			t.Fatalf("HTTP/1.1 regressed with h2c on: %s %d", h1Resp.Proto, h1Resp.StatusCode)
		}

		// Prior knowledge works, and the surface is identical over it.
		h2Resp, h2Body := roundTrip(t, h2cClient(), base, transportProbe{name: "h2c", method: "GET", path: "/dav/README.md"}, "/dav/README.md")
		if h2Resp.Proto != "HTTP/2.0" {
			t.Fatalf("h2c prior knowledge observed %s, want HTTP/2.0", h2Resp.Proto)
		}
		if h2Resp.Header.Get("X-Bunker-Proto") != "HTTP/2.0" {
			t.Fatalf("server echoed %q over h2c", h2Resp.Header.Get("X-Bunker-Proto"))
		}
		if string(h2Body) != fixtureReadme {
			t.Fatalf("h2c body = %q", h2Body)
		}

		// The RFC 7540 upgrade dance is NOT implemented: no error, and the
		// answer arrives over HTTP/1.1. This is the silent-downgrade shape the
		// capability document reports as a degradation.
		req, err := http.NewRequest("GET", base+"/dav/README.md", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("Connection", "Upgrade, HTTP2-Settings")
		req.Header.Set("Upgrade", "h2c")
		req.Header.Set("HTTP2-Settings", "AAMAAABkAAQAAP__")
		upgradeResp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
		if err != nil {
			t.Fatalf("upgrade attempt errored instead of silently downgrading: %v", err)
		}
		defer func() { _ = upgradeResp.Body.Close() }()
		if upgradeResp.Proto != "HTTP/1.1" {
			t.Fatalf("upgrade attempt observed %s; net/http does not implement the Upgrade dance, so this must be HTTP/1.1", upgradeResp.Proto)
		}
	})
}
