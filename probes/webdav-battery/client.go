// The client layer of the per-protocol battery: one transport per HTTP version,
// and a request path that records the version the SERVER observed on every
// response.
//
// WHY THIS FILE EXISTS AT ALL, and not just `curl`: the battery's anti-gaming
// rule (BFS-011) is that a cell must report the protocol it ACTUALLY used, read
// from the response — a cell that reports "h2" because the client was
// configured for h2 proves nothing (PROTO-014 hit the same wall and solved it by
// checking the server's own r.Proto). This surface echoes r.Proto in
// `X-Bunker-Proto` on every response (BFS-004 §3 E-3, §4.3 C-6), so the battery
// records BOTH observations for every request:
//
//   - ClientProto — `resp.Proto`, what the client's own stack received;
//   - ServerProto — the `X-Bunker-Proto` header, the version the server saw.
//
// A cell whose two observations disagree with the arm's protocol is reported
// UNRELIABLE, never quietly passed. The ALPN the TLS (or QUIC) handshake
// negotiated is recorded alongside them as a third, transport-level witness.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// protoKind is the arm's HTTP version. It decides the transport, not the
// vocabulary: every protocol serves the same surface (BFS-004 §4.3, C-1).
type protoKind string

const (
	protoH1 protoKind = "h1"
	protoH2 protoKind = "h2"
	protoH3 protoKind = "h3"
)

// parseProto turns the -proto flag into a kind.
func parseProto(s string) (protoKind, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "h1", "http/1.1", "http1":
		return protoH1, nil
	case "h2", "http/2", "http2":
		return protoH2, nil
	case "h3", "http/3", "http3":
		return protoH3, nil
	}
	return "", fmt.Errorf("unknown protocol %q (want h1, h2 or h3)", s)
}

// wireName is the version string the server's r.Proto and Go's resp.Proto use.
func (p protoKind) wireName() string {
	switch p {
	case protoH2:
		return "HTTP/2.0"
	case protoH3:
		return "HTTP/3.0"
	default:
		return "HTTP/1.1"
	}
}

// requestSpec is one request the battery wants to observe.
type requestSpec struct {
	Method   string
	Path     string // absolute path, e.g. /dav/src/main.go
	Headers  map[string]string
	Body     []byte
	BodyType string
}

// call is the record of one HTTP request: its outcome, its timing, and the
// protocol observed by BOTH sides.
type call struct {
	Method      string
	Path        string
	Status      int
	ClientProto string
	ServerProto string
	ALPN        string
	Verdict     string
	Bytes       int64
	Dur         time.Duration
	Err         error
	Body        []byte
	Header      http.Header
}

// class is the four-way outcome vocabulary the report speaks: ok, refused (an
// expected non-2xx answer such as the conflict 412), stall (a timeout — the
// class the sshfs/NFS baselines are full of), error (anything else).
func (c *call) class() string {
	switch {
	case c.Err != nil && c.timedOut():
		return "stall"
	case c.Err != nil:
		return "error"
	case c.Status >= 200 && c.Status < 300:
		return "ok"
	default:
		return "refused"
	}
}

// timedOut reports whether the call died on a deadline rather than on a
// transport error. Both a per-op context deadline and the client's own timeout
// surface here, and both mean the same thing operationally: the request did not
// complete inside its bound.
func (c *call) timedOut() bool {
	if c.Err == nil {
		return false
	}
	if errors.Is(c.Err, context.DeadlineExceeded) {
		return true
	}
	var netErr interface{ Timeout() bool }
	if errors.As(c.Err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

// protoOK reports whether both sides observed the version this arm claims.
func (c *call) protoOK(want string) bool {
	return c.ClientProto == want && c.ServerProto == want
}

// client is one arm's HTTP client: one transport, one in-flight bound.
type client struct {
	proto protoKind
	base  string
	user  string
	token string
	sem   chan struct{}
	hc    *http.Client
}

// newClient builds the transport for kind. conns is the client's connection
// pool bound (MaxConnsPerHost) — the knob that decides whether an HTTP/1.1 arm
// can be concurrent at all; conc is the in-flight request bound every arm obeys
// through the shared semaphore.
func newClient(kind protoKind, base string, conns, conc int, user, token string, timeout time.Duration) (*client, error) {
	if conc < 1 {
		return nil, fmt.Errorf("concurrency must be >= 1, got %d", conc)
	}
	if conns < 1 {
		return nil, fmt.Errorf("conns must be >= 1, got %d", conns)
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // scratch self-signed daemon on loopback

	var rt http.RoundTripper
	switch kind {
	case protoH3:
		// QUIC always encrypts, and the release premise for HTTP/3 is ONE
		// connection (that is what QUIC is for), so the h3 arms fix the
		// connection count at 1: quic-go's transport caches a single client
		// connection per authority and multiplexes every request onto it.
		rt = &http3.Transport{TLSClientConfig: tlsCfg, DisableCompression: true}
	case protoH1, protoH2:
		rt = &http.Transport{
			TLSClientConfig:     tlsCfg,
			ForceAttemptHTTP2:   kind == protoH2,
			MaxConnsPerHost:     conns,
			MaxIdleConnsPerHost: conns,
			MaxIdleConns:        conns,
			DisableCompression:  true,
			IdleConnTimeout:     2 * time.Minute,
		}
	default:
		return nil, fmt.Errorf("unsupported protocol %q", kind)
	}

	return &client{
		proto: kind,
		base:  strings.TrimSuffix(base, "/"),
		user:  user,
		token: token,
		sem:   make(chan struct{}, conc),
		hc: &http.Client{
			Transport: rt,
			Timeout:   timeout,
		},
	}, nil
}

// close releases the transport.
func (c *client) close() {
	if tr, ok := c.hc.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	if tr, ok := c.hc.Transport.(*http3.Transport); ok {
		_ = tr.Close()
	}
}

// url renders an absolute URL for a surface path.
func (c *client) url(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return c.base + path
}

// do performs one request under the arm's in-flight bound and records what came
// back. Every request in the battery — including the verification requests —
// goes through here, so "concurrency" means one thing everywhere: at most this
// many requests are in flight at any instant of the run.
func (c *client) do(ctx context.Context, spec requestSpec) *call {
	out := &call{Method: spec.Method, Path: spec.Path}

	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		out.Err = fmt.Errorf("waiting for an in-flight slot: %w", ctx.Err())
		return out
	}
	defer func() { <-c.sem }()

	var body io.Reader
	if len(spec.Body) > 0 {
		body = bytes.NewReader(spec.Body)
	}
	req, err := http.NewRequestWithContext(ctx, spec.Method, c.url(spec.Path), body)
	if err != nil {
		out.Err = fmt.Errorf("build request: %w", err)
		return out
	}
	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}
	if len(spec.Body) > 0 && spec.BodyType != "" {
		req.Header.Set("Content-Type", spec.BodyType)
	}
	if c.user != "" || c.token != "" {
		req.SetBasicAuth(c.user, c.token)
	}

	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		out.Dur = time.Since(start)
		out.Err = err
		return out
	}
	defer func() { _ = resp.Body.Close() }()

	b, readErr := io.ReadAll(resp.Body)
	out.Dur = time.Since(start)
	out.Status = resp.StatusCode
	out.ClientProto = resp.Proto
	out.ServerProto = resp.Header.Get("X-Bunker-Proto")
	out.Verdict = resp.Header.Get("X-Bunker-Verdict")
	out.Header = resp.Header
	out.Body = b
	out.Bytes = int64(len(b))
	if resp.TLS != nil {
		out.ALPN = resp.TLS.NegotiatedProtocol
	}
	if readErr != nil {
		out.Err = fmt.Errorf("read body: %w", readErr)
	}
	return out
}

// describe renders a call for a report line: what was asked, what came back,
// and which protocol both sides saw. A stall prints as a stall, never as a
// missing value.
func (c *call) describe() string {
	proto := fmt.Sprintf("client=%s server=%s", orNone(c.ClientProto), orNone(c.ServerProto))
	if c.Err != nil {
		return fmt.Sprintf("%s %s -> %s (%s) %s", c.Method, c.Path, c.class(), shortErr(c.Err), proto)
	}
	return fmt.Sprintf("%s %s -> %d (%s) %s", c.Method, c.Path, c.Status, c.Verdict, proto)
}

func orNone(s string) string {
	if s == "" {
		return "<absent>"
	}
	return s
}

// shortErr trims an error to something a report line can carry. The full error
// text is preserved for the request-level CSV, which is the raw record.
func shortErr(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 160 {
		msg = msg[:160] + "…"
	}
	return msg
}

// hostCPUCount reads the machine's CPU count for the loadavg record — a load
// average without a core count is not interpretable, and the row that filed
// this battery recorded that contention had already corrupted one set of
// numbers in this study.
func hostCPUCount() int {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "processor") {
			n++
		}
	}
	return n
}

// loadAvg reads /proc/loadavg's first field (the 1-minute average).
func loadAvg() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "unavailable"
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return "unavailable"
	}
	return fields[0]
}
