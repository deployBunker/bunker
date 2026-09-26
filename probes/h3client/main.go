// Command h3client is the HTTP/3 instrument for probes/webdav-h1h2-probe.sh:
// a real HTTP/3 (QUIC) client, so the probe can assert the negotiated protocol
// on the WIRE rather than infer it from the server's configuration.
//
// Why a Go client and not curl: BFS-002 measured this repository's curl
// (`8.18.0`, OpenSSL/3.5.5, nghttp2) as having NO HTTP/3 support at all —
// `curl --version` lists no `http3` protocol — so an h3 assertion made with
// curl would either be skipped or, worse, silently measured over HTTP/2. This
// is the same instrument BFS-002 §6.7 prescribed for the transport probe.
//
// It reports what the CLIENT saw, never what the server was configured with:
// the response's own protocol (`proto`), the ALPN the QUIC handshake actually
// negotiated (`alpn`), the TLS version (`tls`), and the Alt-Svc header the
// response carried (empty on an h3 response, by design).
//
// Usage:
//
//	h3client -url https://127.0.0.1:18080/dav/README.md \
//	         [-method GET] [-header 'Depth: 1'] [-user bunker] [-token ...] \
//	         [-out body.bin] [-timeout 15s]
//
// Output: one line of `key=value` pairs on stdout, e.g.
//
//	proto=HTTP/3.0 alpn=h3 tls=TLSv1.3 status=200 server-proto=HTTP/3.0 \
//	alt-svc= bytes=11 sha256=... out=/tmp/body.bin
//
// On failure it prints `err="..."` and exits 1, so a shell caller can treat a
// non-zero exit as "this transport did not serve the request" — which is what
// makes the probe's negative arm (an h3 client against a TCP-only endpoint)
// evidence instead of a tautology.
package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// headerList collects repeated -header flags.
type headerList []string

func (h *headerList) String() string { return strings.Join(*h, ", ") }

func (h *headerList) Set(value string) error {
	if !strings.Contains(value, ":") {
		return fmt.Errorf("header %q is not in 'Name: value' form", value)
	}
	*h = append(*h, value)
	return nil
}

func main() {
	var (
		url      = flag.String("url", "", "absolute https URL to fetch over HTTP/3 (required)")
		method   = flag.String("method", http.MethodGet, "HTTP method")
		user     = flag.String("user", "", "Basic-auth username (optional)")
		token    = flag.String("token", "", "Basic-auth password (optional; never echoed)")
		out      = flag.String("out", "", "write the response body here (optional)")
		timeout  = flag.Duration("timeout", 20*time.Second, "overall request timeout")
		insecure = flag.Bool("insecure", true,
			"skip certificate verification — the probe talks to a self-signed scratch daemon")
		headers headerList
	)
	flag.Var(&headers, "header", "request header in 'Name: value' form (repeatable)")
	flag.Parse()

	if err := run(*url, *method, *user, *token, *out, *timeout, *insecure, headers); err != nil {
		fmt.Printf("err=%q\n", err.Error())
		os.Exit(1)
	}
}

func run(url, method, user, token, out string, timeout time.Duration, insecure bool, headers []string) error {
	if url == "" {
		return fmt.Errorf("-url is required")
	}
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("-url must be https:// (QUIC always encrypts): %q", url)
	}

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	for _, h := range headers {
		name, value, _ := strings.Cut(h, ":")
		req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	if user != "" || token != "" {
		req.SetBasicAuth(user, token)
	}

	transport := &http3.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: insecure, //nolint:gosec // scratch self-signed daemon, see -insecure
	}}
	defer func() { _ = transport.Close() }()
	client := &http.Client{Transport: transport, Timeout: timeout}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s over HTTP/3: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if out != "" {
		if err := os.WriteFile(out, body, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", out, err)
		}
	}

	// The negotiated facts, read off the response rather than assumed: the
	// protocol string, the ALPN the handshake settled on, and the TLS version.
	alpn, tlsVersion := "", ""
	if resp.TLS != nil {
		alpn = resp.TLS.NegotiatedProtocol
		tlsVersion = tls.VersionName(resp.TLS.Version)
	}
	sum := sha256.Sum256(body)

	fmt.Printf("proto=%s alpn=%s tls=%s status=%d server-proto=%s alt-svc=%q bytes=%d sha256=%s out=%s\n",
		resp.Proto, alpn, tlsVersion, resp.StatusCode,
		resp.Header.Get("X-Bunker-Proto"), resp.Header.Get("Alt-Svc"),
		len(body), hex.EncodeToString(sum[:]), out)
	return nil
}
