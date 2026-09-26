package fsclient

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultConcurrency is the number of requests this client keeps in flight.
//
// The lever is REQUEST CONCURRENCY, not the protocol version: the study's
// measured result is 0.79 s at 25× concurrency against 38.47 s with
// MaxConnsPerHost=1 — 25.0× on the same bytes, same server, same HTTP version
// (PRD-bunker-fs.md:60–61). The client's default therefore reflects it, and
// `bunker fs mount --concurrency` is the knob.
const DefaultConcurrency = 25

// Options configures a Client.
type Options struct {
	// BaseURL is the surface root, e.g. http://127.0.0.1:18481/dav.
	BaseURL string
	// Concurrency is the maximum number of requests in flight, and the
	// default for MaxConnsPerHost.
	Concurrency int
	// MaxConnsPerHost, when > 0, overrides the transport's per-host connection
	// cap independently of Concurrency. It exists so the concurrency lever can
	// be measured as the study measured it (MaxConnsPerHost=1 vs many).
	MaxConnsPerHost int
	// OpTimeout bounds any in-flight operation (§7.2: 30 s).
	OpTimeout time.Duration
	// BindTimeout bounds the mount-time probe (§7.2: 5 s).
	BindTimeout time.Duration
	// Username/Password are used as HTTP Basic credentials when set. The
	// surface reuses the daemon's own credential model; this client does not
	// invent one.
	Username string
	Password string
	// ExpectedTree, when set, pins the served tree identity: a response that
	// carries a different X-Bunker-Tree is §7.1's stale_identity, never an
	// auto-adoption of the new tree.
	ExpectedTree string
	// HTTPClient is the transport seam. Tests inject a client whose
	// MaxConnsPerHost they control; production builds one from Concurrency.
	HTTPClient *http.Client
	// UserAgent identifies this client on the wire.
	UserAgent string
	// MaxResultBytes is the X-Bunker-Max-Bytes budget sent with ops whose
	// results are lists (default 16 MiB, the server's absolute cap).
	MaxResultBytes int64
}

// FileMeta is what a HEAD/PROPFIND learns about one path.
type FileMeta struct {
	Path      string
	IsDir     bool
	Size      int64
	Hash      string // "" when the surface served no ETag (a collection)
	Mode      string // 4 octal digits, e.g. "0644"
	MtimeUnix int64  // milliseconds
	Mtime     time.Time
}

// PutPrecondition is the conditional-write precondition of BFS-004 §6. Exactly
// one of the three states is sent: a strong If-Match, an If-None-Match: *, or
// nothing at all (an unconditional write, which is what a stock client sends
// and which this client never sends on its own).
type PutPrecondition struct {
	// IfMatch is the base hash the write expects to replace, as a bare
	// `sha256:…` value; it is quoted into a strong entity-tag on the wire.
	IfMatch string
	// IfNoneMatchStar asks the server to create only if the path is absent.
	IfNoneMatchStar bool
}

// PutResult is the outcome of one conditional PUT.
type PutResult struct {
	Status int
	// Hash is the hash of the bytes the server now holds.
	Hash string
	// Noop is true for D3's identical-content case: 204 + identical_content,
	// no disk write, mtime preserved.
	Noop bool
	// Verdict is the server's machine code for the outcome.
	Verdict string
}

// Client is the WebDAV-surface client. It is safe for concurrent use: the whole
// point of it is that many requests can be in flight at once.
type Client struct {
	opt  Options
	base *url.URL
	hc   *http.Client
	sem  chan struct{}

	inflight    atomic.Int64
	inflightMax atomic.Int64
	requests    atomic.Int64

	tree  atomic.Value // string
	rev   atomic.Value // string
	proto atomic.Value // string

	capsMu sync.RWMutex
	caps   *Capabilities
}

// NewClient builds a client. The base URL must be absolute; nothing else here
// assumes a particular transport, so HTTP/1.1, h2 and h3 all work unchanged.
func NewClient(opt Options) (*Client, error) {
	if strings.TrimSpace(opt.BaseURL) == "" {
		return nil, errors.New("fsclient: BaseURL is required")
	}
	base, err := url.Parse(opt.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("fsclient: parse base url: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("fsclient: base url %q must be absolute (scheme://host/path)", opt.BaseURL)
	}
	if opt.Concurrency <= 0 {
		opt.Concurrency = DefaultConcurrency
	}
	if opt.OpTimeout <= 0 {
		opt.OpTimeout = DefaultOpTimeout
	}
	if opt.BindTimeout <= 0 {
		opt.BindTimeout = DefaultBindTimeout
	}
	if opt.MaxResultBytes <= 0 {
		opt.MaxResultBytes = 16 << 20
	}
	if opt.UserAgent == "" {
		opt.UserAgent = "bunker-fs/1"
	}
	hc := opt.HTTPClient
	if hc == nil {
		maxConns := opt.MaxConnsPerHost
		if maxConns <= 0 {
			maxConns = opt.Concurrency
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		// The measured lever: many connections in flight. MaxIdleConnsPerHost
		// is kept equal so a burst does not pay connection setup per request
		// (connection setup is 1589 ms unmultiplexed vs 278 ms multiplexed).
		tr.MaxConnsPerHost = maxConns
		tr.MaxIdleConnsPerHost = maxConns
		tr.MaxIdleConns = maxConns * 2
		hc = &http.Client{Transport: tr, Timeout: 0}
	}
	c := &Client{
		opt:  opt,
		base: base,
		hc:   hc,
		sem:  make(chan struct{}, opt.Concurrency),
	}
	return c, nil
}

// Options returns the effective options (after defaults).
func (c *Client) Options() Options { return c.opt }

// InFlight reports the current and the high-water in-flight request count — the
// number acceptance criterion 5 asks to be stated rather than asserted.
func (c *Client) InFlight() (current, max int64) {
	return c.inflight.Load(), c.inflightMax.Load()
}

// Requests reports how many HTTP requests this client has issued. It is the
// figure that separates "one snapshot call" from "N round trips" in a
// measurement.
func (c *Client) Requests() int64 { return c.requests.Load() }

// PinTree fixes the served tree identity this client is bound to. Every later
// response carrying a different X-Bunker-Tree is §7.1's stale_identity — never
// an auto-adoption of the new tree (a fresh tree is not "the same tree, newer").
func (c *Client) PinTree(tree string) { c.opt.ExpectedTree = tree }

// Tree returns the served tree identity observed on the last response.
func (c *Client) Tree() string { return c.loadString(&c.tree) }

// Rev returns the tree revision token observed on the last response.
func (c *Client) Rev() string { return c.loadString(&c.rev) }

// Proto returns the HTTP version the server reported observing.
func (c *Client) Proto() string { return c.loadString(&c.proto) }

// Capabilities returns the capability document, when one has been fetched.
func (c *Client) Capabilities() *Capabilities {
	c.capsMu.RLock()
	defer c.capsMu.RUnlock()
	return c.caps
}

func (c *Client) loadString(v *atomic.Value) string {
	s, _ := v.Load().(string)
	return s
}

// urlFor maps an in-tree path onto the surface's URI space. Each segment is
// percent-encoded independently so a path containing '#' or '?' cannot become a
// different resource.
func (c *Client) urlFor(path string) string {
	trimmed := strings.Trim(path, "/")
	base := strings.TrimRight(c.base.Path, "/")
	if trimmed == "" {
		return c.base.Scheme + "://" + c.base.Host + base + "/"
	}
	segs := strings.Split(trimmed, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return c.base.Scheme + "://" + c.base.Host + base + "/" + strings.Join(segs, "/")
}

// acquire/release implement the client-side in-flight bound, which is what the
// concurrency knob actually controls. The transport's MaxConnsPerHost bounds
// connections; this bounds requests, so a semaphore of 1 gives the serial arm of
// the measurement even when the transport would allow more.
func (c *Client) acquire(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) release() { <-c.sem }

// newRequest builds one request with the headers every call carries.
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.urlFor(path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.opt.UserAgent)
	if c.opt.Username != "" || c.opt.Password != "" {
		req.SetBasicAuth(c.opt.Username, c.opt.Password)
	}
	if t := c.opt.ExpectedTree; t != "" {
		req.Header.Set("X-Bunker-Tree", t)
	}
	return req, nil
}

// cancelOnClose ties a request's deadline to the lifetime of its response BODY,
// not to the return of the call that issued it.
//
// This is not a detail: cancelling the request context as soon as the round trip
// returns makes the caller's very next `io.ReadAll(resp.Body)` fail with
// "context canceled", which surfaces as an unreachable_reset — a transport fault
// where there was none. Measured while building this row: the mount's initial
// snapshot reported `context canceled` and fell back to the PROPFIND path for no
// reason at all.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// do performs one request under the in-flight bound and the operation deadline,
// records the identity headers, and maps a transport failure through §7.1.
func (c *Client) do(ctx context.Context, req *http.Request) (*http.Response, *OpError) {
	var cancel context.CancelFunc
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, c.opt.OpTimeout)
		req = req.WithContext(ctx)
	}
	fail := func(e *OpError) (*http.Response, *OpError) {
		if cancel != nil {
			cancel()
		}
		return nil, e
	}
	if err := c.acquire(ctx); err != nil {
		e := classifyTransport(err)
		e.Op, e.Path = req.Method, req.URL.Path
		return fail(e)
	}
	cur := c.inflight.Add(1)
	for {
		max := c.inflightMax.Load()
		if cur <= max || c.inflightMax.CompareAndSwap(max, cur) {
			break
		}
	}
	defer func() {
		c.inflight.Add(-1)
		c.release()
	}()
	c.requests.Add(1)

	resp, err := c.hc.Do(req)
	if err != nil {
		e := classifyTransport(err)
		e.Op, e.Path = req.Method, req.URL.Path
		return fail(e)
	}
	if cancel != nil {
		resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	}
	c.observe(resp)
	return resp, nil
}

// observe records the identity/version headers every response carries, and
// enforces the tree pin. It is where a re-created tree becomes visible at all:
// every response names the tree it came from.
func (c *Client) observe(resp *http.Response) {
	if t := resp.Header.Get("X-Bunker-Tree"); t != "" {
		c.tree.Store(t)
	}
	if r := resp.Header.Get("X-Bunker-Rev"); r != "" {
		c.rev.Store(r)
	}
	if p := resp.Header.Get("X-Bunker-Proto"); p != "" {
		c.proto.Store(p)
	}
}

// checkTree turns a re-created tree into §7.1's stale_identity. It is called
// where the identity matters (a landed write, a snapshot) rather than on every
// response, because the header is informational until we act on it.
func (c *Client) checkTree(resp *http.Response) *OpError {
	if c.opt.ExpectedTree == "" {
		return nil
	}
	got := resp.Header.Get("X-Bunker-Tree")
	if got == "" || got == c.opt.ExpectedTree {
		return nil
	}
	return &OpError{
		Op: "tree-check", Errno: 0, Cause: CauseStaleIdentity,
		Status: resp.StatusCode, Verdict: VerdictStaleTree,
		Detail: fmt.Sprintf("served tree %s is not the bound tree %s (both identities named; never auto-adopted)", got, c.opt.ExpectedTree),
	}
}

// failureFrom builds the OpError for a non-2xx response, using the server's own
// verdict vocabulary (BFS-004 §5.1) and never inventing a code.
func (c *Client) failureFrom(op, path string, resp *http.Response, body []byte) *OpError {
	e := &OpError{
		Op:      op,
		Path:    path,
		Status:  resp.StatusCode,
		Verdict: strings.TrimSpace(resp.Header.Get("X-Bunker-Verdict")),
		Detail:  strings.TrimSpace(firstNonEmpty(string(body), resp.Status)),
	}
	e.CurrentHash = resp.Header.Get("X-Bunker-Current-Hash")
	e.ExpectedHash = resp.Header.Get("X-Bunker-Expected-Hash")

	// A structured refusal body carries the same fields for a client that reads
	// the body rather than the headers.
	var doc struct {
		XMLName    xml.Name `xml:"error"`
		Capability string   `xml:"capability"`
		Scope      string   `xml:"scope"`
		Phase      string   `xml:"phase"`
		Mode       string   `xml:"mode"`
		Detail     string   `xml:"detail"`
	}
	if strings.Contains(e.Detail, "<") {
		var wrapper struct {
			Inner struct {
				Capability string `xml:"capability"`
				Scope      string `xml:"scope"`
				Phase      string `xml:"phase"`
				Mode       string `xml:"mode"`
				Detail     string `xml:"detail"`
			} `xml:",any"`
		}
		_ = xml.Unmarshal(body, &wrapper)
		_ = doc
		if wrapper.Inner.Capability != "" {
			e.Capability = wrapper.Inner.Capability
		}
		if wrapper.Inner.Scope != "" {
			e.Scope = wrapper.Inner.Scope
		}
		if wrapper.Inner.Phase != "" {
			e.Phase = wrapper.Inner.Phase
		}
		if wrapper.Inner.Mode != "" {
			e.Mode = wrapper.Inner.Mode
		}
		if wrapper.Inner.Detail != "" {
			e.Detail = wrapper.Inner.Detail
		}
	}
	if e.Capability == "" {
		e.Capability = resp.Header.Get("X-Bunker-Capability")
	}

	switch {
	case e.Verdict == VerdictHashMismatch, resp.StatusCode == http.StatusPreconditionFailed:
		// ESTALE (116) — a stale base, recoverable by re-reading ONE file.
		e.Errno = ErrnoESTALE
		e.Cause = CauseConflict
		if e.Verdict == VerdictPreconditionFailed {
			// RFC-plain absence: no hash detail, and the client must not read
			// the absence as "no information" (BFS-004 §3 E-2).
			e.CurrentHash, e.ExpectedHash = "", ""
		}
	case e.Verdict == VerdictStaleTree:
		// EREMOTEIO (121) — a re-created tree, NOT recoverable per file, which
		// is exactly why it is a different errno from ESTALE.
		e.Errno = ErrnoEREMOTEIO
		e.Cause = CauseStaleIdentity
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		e.Errno = ErrnoEACCES
		e.Cause = CauseServerError
	case resp.StatusCode == http.StatusNotFound:
		e.Errno = ErrnoENOENT
		e.Cause = CauseServerError
	case resp.StatusCode == http.StatusMethodNotAllowed:
		e.Errno = ErrnoEOPNOTSUPP
		e.Cause = CauseServerError
	case resp.StatusCode == 422, resp.StatusCode >= 500:
		e.Errno = ErrnoEIO
		e.Cause = CauseServerError
	default:
		e.Errno = ErrnoEIO
		e.Cause = CauseServerError
	}
	return e
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Get fetches one file's bytes. When ifNoneMatch is set and the server answers
// 304, the returned FileMeta carries the hash and Data is nil.
func (c *Client) Get(ctx context.Context, path, ifNoneMatch string) ([]byte, *FileMeta, *OpError) {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, nil, &OpError{Op: "GET", Path: path, Errno: portableErrno(5), Cause: CauseServerError, Err: err}
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ETagFor(ifNoneMatch))
	}
	resp, oerr := c.do(ctx, req)
	if oerr != nil {
		return nil, nil, oerr
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, &FileMeta{Path: path, Hash: ParseETag(resp.Header.Get("ETag"))}, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, nil, c.failureFrom("GET", path, resp, body)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, classifyTransport(err)
	}
	meta := &FileMeta{
		Path:  path,
		Size:  int64(len(data)),
		Hash:  ParseETag(resp.Header.Get("ETag")),
		Mode:  "",
		Mtime: headerTime(resp.Header.Get("Last-Modified")),
	}
	if meta.Hash == "" {
		// The surface always serves an ETag for a file; compute defensively so
		// the cache key is never guessed. A missing ETag is recorded, not hidden.
		meta.Hash = HashBytes(data)
	}
	return data, meta, nil
}

// Head learns one path's metadata without its bytes — the cheap way to fetch a
// base hash for a write to a path we never read (BFS-005 §5.3's
// fetch-then-check).
func (c *Client) Head(ctx context.Context, path string) (*FileMeta, *OpError) {
	req, err := c.newRequest(ctx, http.MethodHead, path, nil)
	if err != nil {
		return nil, &OpError{Op: "HEAD", Path: path, Errno: portableErrno(5), Cause: CauseServerError, Err: err}
	}
	resp, oerr := c.do(ctx, req)
	if oerr != nil {
		return nil, oerr
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, c.failureFrom("HEAD", path, resp, body)
	}
	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	return &FileMeta{
		Path:  path,
		Size:  size,
		Hash:  ParseETag(resp.Header.Get("ETag")),
		Mtime: headerTime(resp.Header.Get("Last-Modified")),
	}, nil
}

// PutBody performs one whole-file conditional PUT: the single request of
// BFS-004 §6, never one request per FUSE WRITE chunk (there are 1023 of those
// for a 4 MiB file — M14). body is streamed straight into the request, so the
// client's write memory is one chunk, not the file.
func (c *Client) PutBody(ctx context.Context, path string, body io.Reader, size int64, pre PutPrecondition) (*PutResult, *OpError) {
	req, err := c.newRequest(ctx, http.MethodPut, path, body)
	if err != nil {
		return nil, &OpError{Op: "PUT", Path: path, Errno: portableErrno(5), Cause: CauseServerError, Err: err}
	}
	if size >= 0 {
		req.ContentLength = size
	}
	switch {
	case pre.IfNoneMatchStar:
		req.Header.Set("If-None-Match", "*")
	case pre.IfMatch != "":
		req.Header.Set("If-Match", ETagFor(pre.IfMatch))
	}
	resp, oerr := c.do(ctx, req)
	if oerr != nil {
		return nil, oerr
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	if resp.StatusCode == http.StatusPreconditionFailed || resp.StatusCode == http.StatusConflict {
		return nil, c.failureFrom("PUT", path, resp, respBody)
	}
	if resp.StatusCode == 422 {
		return nil, c.failureFrom("PUT", path, resp, respBody)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return nil, c.failureFrom("PUT", path, resp, respBody)
	}
	if oerr := c.checkTree(resp); oerr != nil {
		return nil, oerr
	}
	return &PutResult{
		Status:  resp.StatusCode,
		Hash:    firstNonEmpty(resp.Header.Get("X-Bunker-Hash"), ParseETag(resp.Header.Get("ETag"))),
		Noop:    resp.Header.Get("X-Bunker-Noop") == "1",
		Verdict: firstNonEmpty(resp.Header.Get("X-Bunker-Verdict"), VerdictOK),
	}, nil
}

// Mkcol creates a collection.
func (c *Client) Mkcol(ctx context.Context, path string) *OpError {
	req, err := c.newRequest(ctx, "MKCOL", path, nil)
	if err != nil {
		return &OpError{Op: "MKCOL", Path: path, Errno: portableErrno(5), Cause: CauseServerError, Err: err}
	}
	resp, oerr := c.do(ctx, req)
	if oerr != nil {
		return oerr
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return c.failureFrom("MKCOL", path, resp, body)
}

// Delete removes a resource. When ifMatch is set the delete is conditional on
// the same content hash the write path uses.
func (c *Client) Delete(ctx context.Context, path, ifMatch string) *OpError {
	req, err := c.newRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return &OpError{Op: "DELETE", Path: path, Errno: portableErrno(5), Cause: CauseServerError, Err: err}
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ETagFor(ifMatch))
	}
	resp, oerr := c.do(ctx, req)
	if oerr != nil {
		return oerr
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return c.failureFrom("DELETE", path, resp, body)
}

// Move renames a resource. The Destination header must be absolute (RFC 4918
// §9.9.3), so the client builds it from its own base URL.
func (c *Client) Move(ctx context.Context, src, dst string, overwrite bool) *OpError {
	req, err := c.newRequest(ctx, "MOVE", src, nil)
	if err != nil {
		return &OpError{Op: "MOVE", Path: src, Errno: portableErrno(5), Cause: CauseServerError, Err: err}
	}
	req.Header.Set("Destination", c.urlFor(dst))
	if overwrite {
		req.Header.Set("Overwrite", "T")
	} else {
		req.Header.Set("Overwrite", "F")
	}
	resp, oerr := c.do(ctx, req)
	if oerr != nil {
		return oerr
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return c.failureFrom("MOVE", src, resp, body)
}

// Propfind issues a standard PROPFIND at Depth 0 or 1 and decodes the
// multistatus. It is the standard-protocol half of the metadata path: the
// client must be able to read a tree from a server that knows nothing about
// X-Bunker-Op, and that is exactly the fallback the snapshot degrading uses.
func (c *Client) Propfind(ctx context.Context, path, depth string) ([]FileMeta, *OpError) {
	if depth != "0" && depth != "1" {
		return nil, &OpError{Op: "PROPFIND", Path: path, Errno: portableErrno(22), Cause: CauseLocalCapability,
			Detail: "PROPFIND depth must be 0 or 1; whole-tree walks are refused by the surface and served by X-Bunker-Op: snapshot"}
	}
	req, err := c.newRequest(ctx, "PROPFIND", path, strings.NewReader(propfindBody))
	if err != nil {
		return nil, &OpError{Op: "PROPFIND", Path: path, Errno: portableErrno(5), Cause: CauseServerError, Err: err}
	}
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	resp, oerr := c.do(ctx, req)
	if oerr != nil {
		return nil, oerr
	}
	defer resp.Body.Close()
	if resp.StatusCode != 207 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, c.failureFrom("PROPFIND", path, resp, body)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, classifyTransport(err)
	}
	metas, perr := decodeMultistatus(raw, c.base.Path)
	if perr != nil {
		return nil, &OpError{Op: "PROPFIND", Path: path, Errno: portableErrno(5), Cause: CauseServerError, Err: perr}
	}
	return metas, nil
}

// propfindBody asks for exactly the properties the metadata path needs, so the
// response stays in the small-payload class.
const propfindBody = `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:b="urn:bunker:fs:1">
  <D:prop>
    <D:resourcetype/>
    <D:getcontentlength/>
    <D:getlastmodified/>
    <D:getetag/>
    <b:type/>
    <b:mode/>
    <b:mtime/>
    <b:hash/>
  </D:prop>
</D:propfind>`

// multistatus is the RFC 4918 §13 shape, decoded with a permissive namespace
// match: the surface prefixes DAV: properties with `D:` and its extension
// properties with `b:`, and a decoder that skipped the extension ones would
// silently lose the hash and the mode.
type multistatus struct {
	XMLName  xml.Name          `xml:"multistatus"`
	Response []multistatusResp `xml:"response"`
}

type multistatusResp struct {
	Href     string          `xml:"href"`
	Propstat []multistatusPS `xml:"propstat"`
}

type multistatusPS struct {
	Status string          `xml:"status"`
	Prop   multistatusProp `xml:"prop"`
}

type multistatusProp struct {
	ResourceType struct {
		Collection *struct{} `xml:"collection"`
	} `xml:"resourcetype"`
	ContentLength string `xml:"getcontentlength"`
	LastModified  string `xml:"getlastmodified"`
	ETag          string `xml:"getetag"`
	BType         string `xml:"urn:bunker:fs:1 type"`
	BMode         string `xml:"urn:bunker:fs:1 mode"`
	BMtime        string `xml:"urn:bunker:fs:1 mtime"`
	BHash         string `xml:"urn:bunker:fs:1 hash"`
}

// decodeMultistatus turns the XML into FileMeta values, stripping the surface
// prefix so paths are relative to the served root.
func decodeMultistatus(raw []byte, prefix string) ([]FileMeta, error) {
	var doc multistatus
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := make([]FileMeta, 0, len(doc.Response))
	for _, r := range doc.Response {
		path, err := url.PathUnescape(r.Href)
		if err != nil {
			path = r.Href
		}
		path = strings.TrimPrefix(path, prefix)
		path = strings.Trim(path, "/")
		m := FileMeta{Path: path}
		for _, ps := range r.Propstat {
			if !strings.Contains(ps.Status, "200") {
				continue
			}
			p := ps.Prop
			m.IsDir = p.ResourceType.Collection != nil
			if p.ContentLength != "" {
				m.Size, _ = strconv.ParseInt(p.ContentLength, 10, 64)
			}
			m.Mtime = headerTime(p.LastModified)
			if m.Mtime.IsZero() && p.BMtime != "" {
				if ms, err := strconv.ParseInt(p.BMtime, 10, 64); err == nil {
					m.Mtime = time.UnixMilli(ms)
				}
			}
			m.MtimeUnix = m.Mtime.UnixMilli()
			m.Mode = p.BMode
			m.Hash = firstNonEmpty(p.BHash, ParseETag(p.ETag))
		}
		out = append(out, m)
	}
	return out, nil
}

// headerTime parses an HTTP-date, returning the zero time when absent.
func headerTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	if t, err := http.ParseTime(v); err == nil {
		return t
	}
	return time.Time{}
}

// BodyReader lets a caller stream a file into a conditional PUT without
// buffering it, which is what makes the client's write memory one chunk.
func (c *Client) BodyReader(data []byte) io.Reader { return bytes.NewReader(data) }
