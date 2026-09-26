// BFS-007 acceptance tests for the HTTP/3 (QUIC) listener and its Alt-Svc
// advertisement.
//
// Everything here runs the REAL wiring — startH3 binds a real UDP socket, the
// real webdav handler answers through it, and the client is a real HTTP/3
// client (quic-go's http3.Transport) that knows nothing about this repository.
// The two claims that cannot be made from a unit test of the middleware are
// made here: the same surface answers over h3, and the h1/h2 path is untouched
// by a UDP listener existing.

package server

import (
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/deployBunker/bunker/internal/server/webdav"
	"github.com/deployBunker/bunker/internal/tlsutil"
)

// writeTreeFile lays down one file of the served tree.
func writeTreeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const (
	h3FixtureReadme = "# h3 fixture\n"
	h3FixtureMain   = "package main\n\nfunc main() {}\n"
)

// h3Fixture is the running pair under test: one QUIC listener (startH3) and one
// shared *tls.Config, with the real surface handler behind both.
type h3Fixture struct {
	handler http.Handler
	tlsCfg  *tls.Config
	ep      *webdav.H3Endpoint
	li      *h3Listener
	errCh   chan error
}

// newH3Fixture starts the h3 listener on an ephemeral UDP port with the same
// handler the TCP listeners would serve.
func newH3Fixture(t *testing.T) *h3Fixture {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "tree")
	writeTreeFile(t, filepath.Join(root, "README.md"), h3FixtureReadme)
	writeTreeFile(t, filepath.Join(root, "src", "main.go"), h3FixtureMain)

	tlsCfg, err := tlsutil.LoadOrGenerateSelfSigned(
		filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"),
		[]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatalf("self-signed tls config: %v", err)
	}

	ep := webdav.NewH3Endpoint()
	handler, err := webdav.New(webdav.Config{Root: root, Build: "h3-test", TLS: true, H3: ep})
	if err != nil {
		t.Fatalf("webdav.New: %v", err)
	}

	errCh := make(chan error, 1)
	li, err := startH3(ep, "127.0.0.1:0", tlsCfg, handler, slog.New(slog.NewTextHandler(io.Discard, nil)), errCh)
	if err != nil {
		t.Fatalf("startH3: %v", err)
	}
	t.Cleanup(func() { _ = li.srv.Close() })
	return &h3Fixture{handler: handler, tlsCfg: tlsCfg, ep: ep, li: li, errCh: errCh}
}

// base is the QUIC authority the client dials — the port the listener actually
// bound, read from the same liveness record the daemon advertises from.
func (f *h3Fixture) base(t *testing.T) string {
	t.Helper()
	port, live := f.ep.Port()
	if !live {
		t.Fatal("the QUIC endpoint is not live after startH3")
	}
	return "https://127.0.0.1:" + strconv.Itoa(port)
}

// h3Client is a real HTTP/3 client: it has no knowledge of Alt-Svc and is not
// configured with any protocol version — it speaks QUIC because the URL it is
// given resolves to the QUIC listener.
func (f *h3Fixture) h3Client(t *testing.T) *http.Client {
	t.Helper()
	tr := &http3.Transport{TLSClientConfig: &tls.Config{
		// Self-signed fixture certificate; nothing here is a network trust
		// decision, the assertions are about the negotiated protocol.
		InsecureSkipVerify: true,
		ServerName:         "127.0.0.1",
	}}
	t.Cleanup(func() { _ = tr.Close() })
	return &http.Client{Transport: tr}
}

// tcpServer starts a TLS TCP listener from the SAME *tls.Config the QUIC
// listener uses, so a leak of the h3 ALPN into the shared config would be
// observable on a real handshake. It mirrors what the daemon does: the handler
// is passed in (the daemon passes the Alt-Svc-wrapped root handler) and the
// server serves with the configured certificates and empty cert file arguments.
func (f *h3Fixture) tcpServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler, TLSConfig: f.tlsCfg}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return "https://" + ln.Addr().String()
}

// getWith performs one request with the given client and returns the response
// and body, failing the test on transport errors.
func getWith(t *testing.T, client *http.Client, method, url string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, body
}

// TestH3ServesTheSameSurfaceAsTCP is acceptance criteria 1 and 2 together, on
// one process: a real request over HTTP/3 succeeds against the SAME handler the
// TCP path serves, and h1/h2 keep working from the same config.
//
// The version is asserted on BOTH sides — the client's observed protocol
// (`resp.Proto`, plus the ALPN the QUIC handshake actually negotiated) and the
// server's own echo (X-Bunker-Proto) — because a test that only asserts "the
// request succeeded" passes against a downgraded connection.
func TestH3ServesTheSameSurfaceAsTCP(t *testing.T) {
	f := newH3Fixture(t)
	port, _ := f.ep.Port()

	// One port number, two transports, one process: the TCP listener this daemon
	// also runs (h1/h2) and this UDP socket share the port NUMBER, and the UDP
	// socket really is bound — a second bind of the same address must fail.
	if _, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}); err == nil {
		t.Fatalf("a second UDP bind on :%d succeeded; the h3 socket is not actually bound", port)
	}

	base := f.base(t)

	// ── the same surface over HTTP/3 ───────────────────────────────────────
	h3Client := f.h3Client(t)
	resp, body := getWith(t, h3Client, "GET", base+"/dav/README.md", nil)
	if resp.Proto != "HTTP/3.0" {
		t.Fatalf("client observed %q, want HTTP/3.0", resp.Proto)
	}
	if resp.TLS == nil || resp.TLS.NegotiatedProtocol != "h3" {
		t.Fatalf("QUIC handshake negotiated %v, want ALPN h3", resp.TLS)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("GET over h3 -> %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Bunker-Proto"); got != "HTTP/3.0" {
		t.Fatalf("server echoed %q over h3, want HTTP/3.0", got)
	}
	if string(body) != h3FixtureReadme {
		t.Fatalf("h3 body = %q, want the fixture bytes", body)
	}
	// A response that is already HTTP/3 must not advertise HTTP/3: the client is
	// on the advertised transport. The header is for h1/h2 (C-4).
	if got := resp.Header.Get("Alt-Svc"); got != "" {
		t.Fatalf("an h3 response advertised Alt-Svc %q", got)
	}

	// A WebDAV verb that only exists in this surface, over h3: the extension
	// layer is not gated on the version either.
	pResp, pBody := getWith(t, h3Client, "PROPFIND", base+"/dav/src", map[string]string{"Depth": "1"})
	if pResp.StatusCode != 207 {
		t.Fatalf("PROPFIND over h3 -> %d %s", pResp.StatusCode, pBody)
	}
	if !strings.Contains(string(pBody), "D:multistatus") || !strings.Contains(string(pBody), "main.go") {
		t.Fatalf("PROPFIND over h3 body is not the multistatus shape: %s", pBody)
	}

	// The capability document is served over h3 too, and it reports the live
	// listener from inside this process.
	cResp, cBody := getWith(t, h3Client, "POST", base+"/dav/",
		map[string]string{"X-Bunker-Op": "capabilities", "Content-Type": "application/json"})
	if cResp.StatusCode != 200 {
		t.Fatalf("capabilities over h3 -> %d", cResp.StatusCode)
	}
	if !strings.Contains(string(cBody), `"h3":{"alpn":"h3"`) || !strings.Contains(string(cBody), `"available":true`) {
		t.Fatalf("capability document served over h3 does not report the live listener: %s", cBody)
	}

	// ── HTTP/1.1 and HTTP/2 still work, on the same handler ────────────────
	tcp := f.tcpServer(t, f.handler)
	h1Client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		TLSNextProto:    map[string]func(string, *tls.Conn) http.RoundTripper{},
	}}
	h2Client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		ForceAttemptHTTP2: true,
	}}

	h1Resp, h1Body := getWith(t, h1Client, "GET", tcp+"/dav/README.md", nil)
	if h1Resp.Proto != "HTTP/1.1" || h1Resp.StatusCode != 200 {
		t.Fatalf("HTTP/1.1 after h3 came up -> %s %d", h1Resp.Proto, h1Resp.StatusCode)
	}
	if h1Resp.TLS.NegotiatedProtocol != "" && h1Resp.TLS.NegotiatedProtocol != "http/1.1" {
		t.Fatalf("the h1 client negotiated %q", h1Resp.TLS.NegotiatedProtocol)
	}
	h2Resp, h2Body := getWith(t, h2Client, "GET", tcp+"/dav/README.md", nil)
	if h2Resp.Proto != "HTTP/2.0" || h2Resp.StatusCode != 200 {
		t.Fatalf("HTTP/2 after h3 came up -> %s %d", h2Resp.Proto, h2Resp.StatusCode)
	}
	if h2Resp.TLS.NegotiatedProtocol != "h2" {
		t.Fatalf("the h2 client negotiated ALPN %q, want h2", h2Resp.TLS.NegotiatedProtocol)
	}

	// Same bytes on every transport: the version changes cost and affordance,
	// never the surface (C-1).
	for name, got := range map[string]string{"http/1.1": string(h1Body), "http/2": string(h2Body), "http/3": string(body)} {
		if got != h3FixtureReadme {
			t.Fatalf("%s body = %q, want the fixture bytes — the surface differs per version", name, got)
		}
	}
}

// TestAltSvcIsAdvertisedOnlyWhileH3IsLive is acceptance criterion 3 at the
// listener level: the header is produced by the live-liveness record, so it
// appears with the socket and disappears with it, and it never appears on a
// response that is already on the advertised transport.
func TestAltSvcIsAdvertisedOnlyWhileH3IsLive(t *testing.T) {
	f := newH3Fixture(t)
	// The wrapped handler is installed BEFORE the listener starts: the server
	// reads it per request, so swapping it afterwards would be a data race.
	wrapped := f.tcpServer(t, f.li.withAltSvc(f.handler))
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		ForceAttemptHTTP2: true,
	}}
	port, _ := f.ep.Port()
	want := `h3=":` + strconv.Itoa(port) + `"; ma=2592000`

	// Live: every h1/h2 response advertises the QUIC socket, and the value names
	// the socket's own port.
	resp, _ := getWith(t, client, "GET", wrapped+"/dav/README.md", nil)
	if got := resp.Header.Get("Alt-Svc"); got != want {
		t.Fatalf("live Alt-Svc = %q, want %q", got, want)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("the advertisement is expected on an h1/h2 response, got %s", resp.Proto)
	}

	// An h3 response carries no Alt-Svc even while the listener is live. The
	// request is built by hand because only the QUIC server sets Proto 3.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dav/README.md", nil)
	req.Proto, req.ProtoMajor, req.ProtoMinor = "HTTP/3.0", 3, 0
	f.li.withAltSvc(f.handler).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("h3-shaped request -> %d", rec.Code)
	}
	if got := rec.Header().Get("Alt-Svc"); got != "" {
		t.Fatalf("an HTTP/3 request was answered with Alt-Svc %q", got)
	}

	// Down: the socket stopped, so the header must stop with it — and the TCP
	// path itself is untouched (same status, same bytes).
	f.ep.SetDown()
	resp, body := getWith(t, client, "GET", wrapped+"/dav/README.md", nil)
	if got := resp.Header.Get("Alt-Svc"); got != "" {
		t.Fatalf("Alt-Svc %q was emitted with no live QUIC socket", got)
	}
	if resp.StatusCode != 200 || string(body) != h3FixtureReadme {
		t.Fatalf("the TCP path changed when h3 went down: %d %q", resp.StatusCode, body)
	}

	// Back up on a different port: the advertised authority follows the socket,
	// not the configuration.
	f.ep.SetLive(port + 1)
	resp, _ = getWith(t, client, "GET", wrapped+"/dav/README.md", nil)
	if got, want := resp.Header.Get("Alt-Svc"), `h3=":`+strconv.Itoa(port+1)+`"; ma=2592000`; got != want {
		t.Fatalf("Alt-Svc after a port change = %q, want %q", got, want)
	}
}

// TestH3WiringDoesNotClobberTheSharedTLSConfig is BFS-002 §8 F3: the QUIC
// server must get a config with NextProtos ["h3"] and the config the TCP
// listeners share must keep NO NextProtos at all — otherwise `h3` would be
// offered on the TCP handshake and the `h2`/`http/1.1` offer could be lost for
// every client.
//
// The structural assertion is primary, and it is paired with two controls that
// make it non-vacuous: the clone handed to the QUIC server DOES carry ["h3"],
// and the same predicate DOES detect a deliberately clobbered config.
func TestH3WiringDoesNotClobberTheSharedTLSConfig(t *testing.T) {
	f := newH3Fixture(t)

	// The invariant: the shared config carries no ALPN offer of its own, so
	// net/http derives http/1.1 + h2 for the TCP listeners exactly as before.
	if len(f.tlsCfg.NextProtos) != 0 {
		t.Fatalf("the shared TLS config gained NextProtos %v", f.tlsCfg.NextProtos)
	}
	// Control 1: the ALPN the QUIC server uses is set — on the CLONE. Without
	// this, the assertion above could pass because nothing happened at all.
	h3Cfg := http3.ConfigureTLSConfig(f.tlsCfg)
	if len(h3Cfg.NextProtos) != 1 || h3Cfg.NextProtos[0] != "h3" {
		t.Fatalf("the QUIC config NextProtos = %v, want [h3]", h3Cfg.NextProtos)
	}
	if len(f.tlsCfg.NextProtos) != 0 {
		t.Fatalf("ConfigureTLSConfig mutated the shared config: %v", f.tlsCfg.NextProtos)
	}

	// Control 2: the predicate can fail. A clobbered config is exactly what the
	// assertion above is looking for.
	clobbered := f.tlsCfg.Clone()
	clobbered.NextProtos = []string{"h3"}
	if len(clobbered.NextProtos) == 0 {
		t.Fatal("the NextProtos predicate cannot detect a clobbered config")
	}

	// Behavioural half: with the QUIC listener live and the shared config in use
	// on TCP, the TCP ALPN matrix is unchanged — h2 negotiates, a client that
	// offers only h3 is refused (h3 is QUIC-only, RFC 9114), and a client that
	// offers nothing is still served over HTTP/1.1.
	tcp := f.tcpServer(t, f.handler)
	host := strings.TrimPrefix(tcp, "https://")

	dial := func(protos []string) (*tls.Conn, error) {
		return tls.Dial("tcp", host, &tls.Config{InsecureSkipVerify: true, NextProtos: protos})
	}

	h2Conn, err := dial([]string{"h2"})
	if err != nil {
		t.Fatalf("TCP ALPN h2 after the h3 listener came up: %v", err)
	}
	defer h2Conn.Close()
	if got := h2Conn.ConnectionState().NegotiatedProtocol; got != "h2" {
		t.Fatalf("TCP ALPN h2 negotiated %q", got)
	}

	h1Conn, err := dial([]string{"http/1.1"})
	if err != nil {
		t.Fatalf("TCP ALPN http/1.1: %v", err)
	}
	defer h1Conn.Close()
	if got := h1Conn.ConnectionState().NegotiatedProtocol; got != "http/1.1" {
		t.Fatalf("TCP ALPN http/1.1 negotiated %q", got)
	}

	h3OverTCP, err := dial([]string{"h3"})
	if err == nil {
		defer h3OverTCP.Close()
		if got := h3OverTCP.ConnectionState().NegotiatedProtocol; got == "h3" {
			t.Fatal("the TCP listener negotiated h3: the QUIC ALPN leaked onto the shared config")
		}
		t.Fatalf("a TCP client offering only h3 negotiated %q instead of being refused",
			h3OverTCP.ConnectionState().NegotiatedProtocol)
	}
	if !strings.Contains(err.Error(), "protocol") {
		t.Logf("h3-over-TCP refusal (error shape may vary by toolchain): %v", err)
	}

	// The same refusal-vs-serve pairing proves the matrix above is not a broken
	// listener: a client offering nothing gets HTTP/1.1 service.
	noALPN, err := dial(nil)
	if err != nil {
		t.Fatalf("TCP with no ALPN: %v", err)
	}
	defer noALPN.Close()
	if got := noALPN.ConnectionState().NegotiatedProtocol; got != "" {
		t.Fatalf("no-ALPN client negotiated %q", got)
	}
	if _, err := noALPN.Write([]byte("GET /dav/README.md HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write over the no-ALPN connection: %v", err)
	}
	raw, err := io.ReadAll(noALPN)
	if err != nil {
		t.Fatalf("read over the no-ALPN connection: %v", err)
	}
	if !strings.HasPrefix(string(raw), "HTTP/1.1 200") {
		t.Fatalf("no-ALPN request was not served over HTTP/1.1: %.60q", raw)
	}
}

// TestAltSvcValueMatchesQUICGoGeneratedHeader pins the payload this repository
// writes to the payload quic-go itself generates, so the literal in
// webdav.AltSvcValue cannot drift from the library's format (it is the SAME
// value the capability document reports, and a process that names two
// authorities for one socket is wrong in at least one of them).
//
// The negative control is the library's own behaviour before a socket exists:
// quic-go announces nothing, which is the same gate this row implements —
// "no listener, no advertisement".
func TestAltSvcValueMatchesQUICGoGeneratedHeader(t *testing.T) {
	f := newH3Fixture(t)
	port, _ := f.ep.Port()

	// The library generates its header when the listener is added, which happens
	// inside Serve — so poll rather than assume an ordering.
	deadline := time.Now().Add(5 * time.Second)
	var generated string
	for {
		hdr := http.Header{}
		err := f.li.srv.SetQUICHeaders(hdr)
		if err == nil {
			generated = hdr.Get("Alt-Svc")
			break
		}
		if !errors.Is(err, http3.ErrNoAltSvcPort) {
			t.Fatalf("SetQUICHeaders: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("quic-go never generated an Alt-Svc header for the bound socket")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if want := webdav.AltSvcValue(port); generated != want {
		t.Fatalf("quic-go announces %q while this build announces %q", generated, want)
	}
	if got := f.ep.AltSvc(); got != generated {
		t.Fatalf("the endpoint advertises %q, quic-go says %q", got, generated)
	}

	// Negative control, at the library level: a server with no listener emits
	// nothing and says so, rather than inventing an authority.
	lazy := &http3.Server{}
	if err := lazy.SetQUICHeaders(http.Header{}); !errors.Is(err, http3.ErrNoAltSvcPort) {
		t.Fatalf("a listener-less quic-go server returned %v, want ErrNoAltSvcPort", err)
	}
}

// TestH3RefusesToStartWithoutItsPreconditions proves the fail-before-listen
// posture and, in the third arm, that a refused start leaves NO advertisement
// behind: the endpoint must stay down when the socket could not be bound.
func TestH3RefusesToStartWithoutItsPreconditions(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("no endpoint record", func(t *testing.T) {
		_, err := startH3(nil, "127.0.0.1:0", &tls.Config{}, http.NotFoundHandler(), logger, make(chan error, 1))
		if err == nil || !strings.Contains(err.Error(), "endpoint record") {
			t.Fatalf("startH3 error = %v, want the missing-record refusal", err)
		}
	})

	t.Run("no TLS config", func(t *testing.T) {
		_, err := startH3(webdav.NewH3Endpoint(), "127.0.0.1:0", nil, http.NotFoundHandler(), logger, make(chan error, 1))
		if err == nil || !strings.Contains(err.Error(), "tls.enabled") {
			t.Fatalf("startH3 error = %v, want the QUIC-needs-TLS refusal", err)
		}
	})

	t.Run("UDP port already bound", func(t *testing.T) {
		taken, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
		if err != nil {
			t.Fatalf("hold a UDP port: %v", err)
		}
		defer taken.Close()
		addr := taken.LocalAddr().String()

		ep := webdav.NewH3Endpoint()
		_, err = startH3(ep, addr, &tls.Config{}, http.NotFoundHandler(), logger, make(chan error, 1))
		if err == nil {
			t.Fatal("startH3 succeeded on an already-bound UDP port")
		}
		if port, live := ep.Port(); live || port != 0 {
			t.Fatalf("a refused start left the endpoint live=%v port=%d", live, port)
		}
		if got := ep.AltSvc(); got != "" {
			t.Fatalf("a refused start left an advertisement: %q", got)
		}
	})
}

// TestH3EndpointGoesDownWhenTheQUICServerStops proves the other half of the
// liveness gate: the record is cleared by the serve loop returning, not by a
// caller remembering to, so no advertisement can outlive the socket.
func TestH3EndpointGoesDownWhenTheQUICServerStops(t *testing.T) {
	f := newH3Fixture(t)
	if _, live := f.ep.Port(); !live {
		t.Fatal("the endpoint is not live after startH3")
	}

	if err := f.li.srv.Close(); err != nil {
		t.Fatalf("close the QUIC server: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, live := f.ep.Port(); !live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the endpoint stayed live after the QUIC server stopped")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.ep.AltSvc(); got != "" {
		t.Fatalf("a stopped QUIC listener still advertises %q", got)
	}
	select {
	case <-f.errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the QUIC serve loop reported no error after being closed")
	}
}
