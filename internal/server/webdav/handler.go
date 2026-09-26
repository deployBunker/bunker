// Package webdav implements the WebDAV surface of docs/spec/BFS-004-webdav-surface.md
// for bunkerd: the RFC 4918 standard surface (methods, status codes,
// properties, multistatus) plus the declared urn:bunker:fs:1 extension layer
// (identity, conditional writes that refuse, tree revision/identity,
// X-Bunker-Op, capability discovery, the declared poll-mode invalidation
// degradation).
//
// The handler is transport-agnostic on purpose. It is mounted on bunkerd's
// existing chi router, so every HTTP version the listener can negotiate —
// HTTP/1.1 always, HTTP/2 via TLS ALPN, h2c where the operator opted in —
// serves the SAME bytes: the version changes cost and transport affordance,
// never the vocabulary (§4.1/§4.3, C-1). X-Bunker-Proto echoes the version the
// server actually observed on each response, so a downgrade is a visible fact
// rather than an assumption (§4.3 C-6).
//
// Two separation rules from the spec are structural here, not conventional:
// no extension is load-bearing for a standard request (R1) — a client that
// sends and reads no X-Bunker-* header gets correct RFC 4918 behaviour — and a
// capability this build does not have answers a structured
// capability_unavailable rather than disappearing (R3).
package webdav

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	// Prefix is the router path the surface is mounted at.
	Prefix = "/dav"
	// Surface identifies this wire contract in the capability document (§4.2).
	Surface = "bunkerd-webdav/1"
	// DocumentVersion is the capability document's version (§4.2 rule 3: a
	// consumer that does not know this value must fail closed).
	DocumentVersion = 1
	// ExtensionsHeader is the X-Bunker-Extensions value on OPTIONS (§10.1).
	ExtensionsHeader = "identity,if_match_refuse,rev,tree,op,watch"
	// NSDav is the DAV: namespace; NSBunker is the extension namespace (§3).
	NSDav    = "DAV:"
	NSBunker = "urn:bunker:fs:1"
	// xmlDecl is the declaration the spec's wire examples carry.
	xmlDecl = `<?xml version="1.0" encoding="utf-8"?>` + "\n"
)

// Envelope and request caps (§4.2 `limits`).
const (
	DefaultMaxRequestBytes   = 1 << 30  // 1 GiB
	DefaultEnvelopeMaxBytes  = 1 << 20  // 1 MiB
	AbsoluteEnvelopeMaxBytes = 16 << 20 // 16 MiB
)

// servedMethods is the verb set this build actually serves: §2.1's supported
// column. It is the single source for the Allow header and for the capability
// document's `methods`, so the two can never disagree. LOCK/UNLOCK are absent
// because slice C4 has not landed — RFC 4918 §18.2 makes class 2 a package
// deal, and this surface advertises DAV: 1 only (A-2).
var servedMethods = []string{
	"OPTIONS", "GET", "HEAD", "PUT", "DELETE", "MKCOL",
	"PROPFIND", "PROPPATCH", "COPY", "MOVE", "POST",
}

// refusedVerbs are methods this surface recognises but does not serve: the
// WebDAV extensions of RFC 3253/3744/4791/5323 plus the non-WebDAV HTTP verbs
// (§2.1). They answer 405 + Allow — "known by the origin server but not
// supported by the target resource" (RFC 9110 §15.5.6).
var refusedVerbs = map[string]bool{
	"TRACE": true, "CONNECT": true, "PATCH": true,
	"REPORT": true, "ACL": true, "MKCALENDAR": true, "SEARCH": true,
	"VERSION-CONTROL": true, "CHECKOUT": true, "MKWORKSPACE": true,
	"LABEL": true, "MERGE": true, "UNCHECKOUT": true, "UPDATE": true,
}

// Config configures a Handler.
type Config struct {
	// Root is the directory served under Prefix. Required.
	Root string
	// Build is the daemon version reported in the capability document.
	Build string
	// TLS reports whether the listener this handler is served on can
	// negotiate HTTP/2 through TLS ALPN. It only feeds the capability
	// document: the handler itself does not care which version arrived.
	TLS bool
	// H2C reports whether the cleartext prior-knowledge h2 opt-in is on for
	// this daemon. h2c is prior-knowledge only — net/http does not implement
	// the RFC 7540 Upgrade dance — so it is offered to LAN/self-owned clients
	// and reported as such, never as general HTTP/2 support.
	H2C bool
	// H3 is the live HTTP/3 (QUIC) endpoint of the daemon this handler is
	// served by, or nil when there is none (BFS-007). It exists so the
	// capability document reports `transports.h3` from the RUNNING process
	// rather than from the config: available only while a QUIC socket is
	// bound, with the authority that socket is bound on. A nil endpoint —
	// a cleartext daemon, or h3 switched off — reports h3 unavailable.
	H3 *H3Endpoint
	// Authenticate, when non-nil, must return true for a request to be
	// served. It exists so the WebDAV mount reuses the daemon's credential
	// model instead of exposing the tree anonymously; the credential model
	// itself belongs to the house PRD (O-6), not to this surface.
	Authenticate func(*http.Request) bool
	// MaxRequestBytes caps a request body (§4.2 limits.max_request_bytes).
	MaxRequestBytes int64
	// DefaultMaxBytes/AbsMaxBytes cap an X-Bunker-Op result.
	DefaultMaxBytes int64
	AbsMaxBytes     int64
}

// Handler serves the WebDAV surface.
type Handler struct {
	cfg  Config
	tree *tree
}

// New builds the surface over cfg.Root. It fails rather than serving a root it
// cannot resolve (the daemon's fail-before-listen posture).
func New(cfg Config) (*Handler, error) {
	t, err := newTree(cfg.Root)
	if err != nil {
		return nil, err
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if cfg.DefaultMaxBytes <= 0 {
		cfg.DefaultMaxBytes = DefaultEnvelopeMaxBytes
	}
	if cfg.AbsMaxBytes <= 0 {
		cfg.AbsMaxBytes = AbsoluteEnvelopeMaxBytes
	}
	cfg.Root = t.rootPath()
	return &Handler{cfg: cfg, tree: t}, nil
}

// Root returns the resolved served root (absolute, symlink-free prefix).
func (h *Handler) Root() string { return h.tree.rootPath() }

// ServeHTTP dispatches one request. Order matters: the asterisk-form OPTIONS
// never touches the tree, the credential check precedes any filesystem work,
// and the E-3 tree check runs BEFORE the method executes (§3 E-3).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions && (r.RequestURI == "*" || r.URL.Path == "*") {
		h.handleOptionsAsterisk(w, r)
		return
	}
	h.setCommonHeaders(w, r)

	if h.cfg.Authenticate != nil && !h.cfg.Authenticate(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="bunker", charset="UTF-8"`)
		h.fail(w, r, failure{Status: http.StatusUnauthorized, Code: VerdictUnauthenticated, Element: "unauthenticated"})
		return
	}

	if want := strings.TrimSpace(r.Header.Get("X-Bunker-Tree")); want != "" {
		current := h.tree.identity()
		if subtle.ConstantTimeCompare([]byte(want), []byte(current)) != 1 {
			h.fail(w, r, failure{
				Status:  409,
				Code:    VerdictStaleTree,
				Element: "stale-tree",
				Pairs:   [][2]string{{"expected", want}, {"current", current}},
			})
			return
		}
	}

	switch r.Method {
	case http.MethodOptions:
		h.handleOptions(w, r)
	case http.MethodGet, http.MethodHead:
		h.handleGet(w, r)
	case http.MethodPut:
		h.handlePut(w, r)
	case http.MethodDelete:
		h.handleDelete(w, r)
	case "MKCOL":
		h.handleMkcol(w, r)
	case "PROPFIND":
		h.handlePropfind(w, r)
	case "PROPPATCH":
		h.handleProppatch(w, r)
	case "COPY":
		h.handleCopyMove(w, r, false)
	case "MOVE":
		h.handleCopyMove(w, r, true)
	case http.MethodPost:
		h.handlePost(w, r)
	case "LOCK", "UNLOCK":
		// Part of this surface, not live in this build: a loud, structured
		// "not yet" with the slice that delivers it (§2.1, §5.1).
		h.fail(w, r, failure{
			Status:     501,
			Code:       VerdictNotImplementedYet,
			Capability: strings.ToLower(r.Method),
			Scope:      "build",
			Phase:      "C4",
			Element:    "not-implemented-yet",
		})
	default:
		if refusedVerbs[r.Method] {
			h.fail(w, r, failure{Status: 405, Code: VerdictMethodNotAllowed, Element: "method-not-allowed"})
			return
		}
		h.fail(w, r, failure{Status: 501, Code: VerdictMethodUnknown, Element: "method-unknown"})
	}
}

// setCommonHeaders writes the extension headers every response carries (§3
// E-1/E-3, §10.1). The verdict starts at "ok" and is overwritten by any
// refusal, so one extraction point serves all outcomes.
func (h *Handler) setCommonHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Bunker-Verdict", string(VerdictOK))
	w.Header().Set("X-Bunker-Proto", r.Proto)
	w.Header().Set("X-Bunker-Rev", h.tree.revToken())
	w.Header().Set("X-Bunker-Tree", h.tree.identity())
	w.Header().Set("X-Bunker-Capabilities", strconv.Itoa(DocumentVersion))
}

// handleOptions answers the discovery entry point (§10.1). It is never gated
// on a build or a capability: RFC 4918 §18.1 requires DAV on every OPTIONS
// response of a WebDAV resource.
func (h *Handler) handleOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", strings.Join(servedMethods, ", "))
	w.Header().Set("DAV", "1")
	w.Header().Set("X-Bunker-Extensions", ExtensionsHeader)
	w.WriteHeader(http.StatusOK)
}

// handleOptionsAsterisk answers OPTIONS * WITHOUT the DAV header (RFC 4918
// §10.1), so per-URI discovery stays honest (A-2's negative control).
func (h *Handler) handleOptionsAsterisk(w http.ResponseWriter, r *http.Request) {
	OptionsAsterisk(w, r)
}

// OptionsAsterisk answers the asterisk-form OPTIONS. It is exported because
// the asterisk-form request target never reaches a router path, so the daemon
// answers it ahead of routing (net/http passes it through with URL.Path "*").
func OptionsAsterisk(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", strings.Join(servedMethods, ", "))
	w.Header().Set("X-Bunker-Verdict", string(VerdictOK))
	w.Header().Set("X-Bunker-Proto", r.Proto)
	w.WriteHeader(http.StatusOK)
}

// ServeProtocols is the HTTP protocol set the listeners that serve this
// surface use.
//
// HTTP/1.1 is always enabled, and HTTP/2 over TLS (ALPN "h2") comes from the
// standard library at zero dependency cost. A non-nil *http.Protocols REPLACES
// the runtime default, so SetHTTP2(true) must be stated explicitly: enabling
// only HTTP/1.1 plus unencrypted h2 would silently switch h2-over-TLS OFF —
// precisely the regression class this row exists to prevent.
//
// h2c (cleartext prior-knowledge HTTP/2) is added only on the operator's
// explicit opt-in (server.h2c_enabled, BFS-002 §4). It is PRIOR KNOWLEDGE
// only: net/http does not implement the RFC 7540 `Upgrade: h2c` dance, so a
// generic client that sends `Upgrade: h2c` (what `curl --http2` does over
// cleartext) is answered over HTTP/1.1 with no error at all. It is therefore
// for loopback/LAN and for our own client — never presented as "h2 for old
// clients", and the capability document reports the same caveat.
func ServeProtocols(h2c bool) *http.Protocols {
	if !h2c {
		// nil keeps the runtime default: HTTP/1.1 + HTTP/2 (TLS), no h2c.
		return nil
	}
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

// resolveOrFail confines the request path (§6.1 step 1).
func (h *Handler) resolveOrFail(w http.ResponseWriter, r *http.Request) (string, *failure) {
	abs, err := h.tree.resolve(r.URL.Path)
	if err == nil {
		return abs, nil
	}
	if errors.Is(err, errEscape) {
		return "", &failure{Status: 403, Code: VerdictWorkspaceInvalid, Element: "workspace-invalid"}
	}
	return "", &failure{Status: 400, Code: VerdictBadArguments, Element: "bad-arguments"}
}

func notFoundOrInternal(err error) failure {
	switch {
	case os.IsNotExist(err):
		return failure{Status: 404, Code: VerdictNotFound, Element: "not-found"}
	case errors.Is(err, os.ErrPermission):
		return failure{Status: 403, Code: VerdictForbidden, Element: "forbidden"}
	default:
		return failure{Status: 500, Code: VerdictInternal, Element: "internal"}
	}
}

func storageFailure(err error) failure {
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return failure{Status: 507, Code: VerdictInsufficientStorage, Element: "insufficient-storage"}
	case errors.Is(err, os.ErrPermission):
		return failure{Status: 403, Code: VerdictForbidden, Element: "forbidden"}
	default:
		return failure{Status: 500, Code: VerdictInternal, Element: "internal"}
	}
}

// handleGet serves files: 200 with the bytes, ETag/X-Bunker-Hash as the
// content identity, 206 for a single byte range, 304 on a matching
// If-None-Match, 416 for an unsatisfiable range. A collection answers 405
// (declared deviation 1) — PROPFIND is the standard way to ask for structure.
func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	abs, f := h.resolveOrFail(w, r)
	if f != nil {
		h.fail(w, r, *f)
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		h.fail(w, r, notFoundOrInternal(err))
		return
	}
	if fi.IsDir() {
		h.fail(w, r, failure{Status: 405, Code: VerdictMethodNotAllowed, Element: "method-not-allowed",
			Pairs: [][2]string{{"detail", "GET/HEAD on a collection is refused; use PROPFIND"}}})
		return
	}
	hash, err := h.tree.hashFile(abs)
	if err != nil {
		h.fail(w, r, notFoundOrInternal(err))
		return
	}
	etag := etagFor(hash)
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Bunker-Hash", hash)
	w.Header().Set("Last-Modified", fi.ModTime().UTC().Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")

	// The 304 answer is settled BEFORE the content headers are set: net/http's
	// HTTP/1.1 path strips Content-Type from a 304 while the HTTP/2 path does
	// not, so setting it earlier would make the same answer differ between
	// versions — the one thing §4.3's "Same" column forbids.
	if match := r.Header.Get("If-None-Match"); match != "" && etagListMatches(match, hash, false) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(abs))

	if spec := r.Header.Get("Range"); spec != "" {
		start, length, ok, satisfiable := parseSingleRange(spec, fi.Size())
		if !satisfiable {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", fi.Size()))
			h.fail(w, r, failure{Status: 416, Code: VerdictRangeNotSatisfiable, Element: "range-not-satisfiable"})
			return
		}
		if ok {
			h.writeFileSegment(w, r, abs, start, length, fi.Size())
			return
		}
		// Multiple ranges are answered with the full body: RFC 9110 §14.2
		// permits a server to ignore Range, and a 200 is a louder answer than
		// a partial 206 the client did not ask about.
	}
	h.writeFileSegment(w, r, abs, 0, fi.Size(), fi.Size())
}

// writeFileSegment streams [start, start+length) of abs. length == size means
// the whole entity (200); anything else is a 206 with Content-Range.
func (h *Handler) writeFileSegment(w http.ResponseWriter, r *http.Request, abs string, start, length, size int64) {
	f, err := os.Open(abs)
	if err != nil {
		h.fail(w, r, notFoundOrInternal(err))
		return
	}
	defer func() { _ = f.Close() }()
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			h.fail(w, r, storageFailure(err))
			return
		}
	}
	status := http.StatusOK
	if length != size {
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, size))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead || length == 0 {
		return
	}
	// A failure here happens after the headers are on the wire (the AC-6
	// transport-kill case): the connection is the only place left to say it.
	_, _ = io.Copy(w, io.LimitReader(f, length))
}

// handlePut implements §6.1's ordered algorithm, including D3's identical
// content case and E-1's declared-body-hash verification.
func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request) {
	abs, f := h.resolveOrFail(w, r)
	if f != nil {
		h.fail(w, r, *f)
		return
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		h.fail(w, r, failure{Status: 405, Code: VerdictMethodNotAllowed, Element: "method-not-allowed",
			Pairs: [][2]string{{"detail", "PUT targets a non-collection resource"}}})
		return
	}
	// RFC 4918 §9.7.1: a missing parent collection MUST fail; it is never
	// created implicitly.
	dir := filepath.Dir(abs)
	if dfi, err := os.Stat(dir); err != nil || !dfi.IsDir() {
		h.fail(w, r, failure{Status: 409, Code: VerdictConflict, Element: "conflict",
			Pairs: [][2]string{{"detail", "parent collection does not exist"}}})
		return
	}
	body, f := h.readBody(w, r)
	if f != nil {
		h.fail(w, r, *f)
		return
	}

	bodyHash := hashBytes(body)
	exists := false
	curHash := ""
	if _, err := os.Stat(abs); err == nil {
		exists = true
		hash, err := h.tree.hashFile(abs)
		if err != nil {
			h.fail(w, r, notFoundOrInternal(err))
			return
		}
		curHash = hash
	}

	if f := checkPreconditions(r, exists, curHash); f != nil {
		// D3: when the requested change has already happened — the base is
		// stale but the arriving bytes are identical to what is on disk —
		// this is a reported no-op, not a refusal. Nothing is written and the
		// mtime does not move.
		if exists && curHash == bodyHash {
			w.Header().Set("ETag", etagFor(curHash))
			w.Header().Set("X-Bunker-Hash", curHash)
			w.Header().Set("X-Bunker-Noop", "1")
			w.Header().Set("X-Bunker-Verdict", string(VerdictIdenticalContent))
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.fail(w, r, *f)
		return
	}

	if declared := strings.TrimSpace(r.Header.Get("X-Bunker-Hash")); declared != "" {
		if got := normalizeTag(declared); got != bodyHash {
			h.fail(w, r, failure{
				Status:  422,
				Code:    VerdictBodyHashMismatch,
				Element: "body-hash-mismatch",
				Pairs:   [][2]string{{"expected", got}, {"received", bodyHash}},
			})
			return
		}
	}

	if err := writeAtomicFile(abs, body); err != nil {
		h.fail(w, r, storageFailure(err))
		return
	}
	h.tree.forget(abs)
	h.tree.bumpRev()
	w.Header().Set("ETag", etagFor(bodyHash))
	w.Header().Set("X-Bunker-Hash", bodyHash)
	if exists {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// readBody reads the request body under the configured cap.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, *failure) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.cfg.MaxRequestBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &failure{Status: 413, Code: VerdictPayloadTooLarge, Element: "payload-too-large",
				Pairs: [][2]string{{"max_bytes", strconv.FormatInt(h.cfg.MaxRequestBytes, 10)}}}
		}
		return nil, &failure{Status: 400, Code: VerdictBadArguments, Element: "bad-arguments",
			Pairs: [][2]string{{"detail", "request body could not be read"}}}
	}
	return body, nil
}

// handleDelete removes a file or a collection. A collection DELETE is
// Depth: infinity by definition, and any other Depth value on a collection is
// a loud 400 rather than a silently deeper operation (declared deviation 4).
func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	abs, f := h.resolveOrFail(w, r)
	if f != nil {
		h.fail(w, r, *f)
		return
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		h.fail(w, r, notFoundOrInternal(err))
		return
	}
	if f := checkPreconditions(r, true, h.hashIfRegular(abs)); f != nil {
		h.fail(w, r, *f)
		return
	}
	if fi.IsDir() {
		if d := strings.TrimSpace(r.Header.Get("Depth")); d != "" && !strings.EqualFold(d, "infinity") {
			h.fail(w, r, failure{Status: 400, Code: VerdictInvalidDepth, Element: "invalid-depth",
				Pairs: [][2]string{{"detail", "a collection DELETE is Depth: infinity or it is not a DELETE"}}})
			return
		}
		if filepath.Clean(abs) == filepath.Clean(h.tree.rootPath()) {
			h.fail(w, r, failure{Status: 403, Code: VerdictForbidden, Element: "forbidden",
				Pairs: [][2]string{{"detail", "the served root itself cannot be deleted"}}})
			return
		}
		if err := os.RemoveAll(abs); err != nil {
			h.fail(w, r, storageFailure(err))
			return
		}
	} else if err := os.Remove(abs); err != nil {
		h.fail(w, r, storageFailure(err))
		return
	}
	h.tree.forget(abs)
	h.tree.bumpRev()
	w.WriteHeader(http.StatusNoContent)
}

// handleMkcol creates one collection. §9.3: ancestors must already exist
// (409 otherwise) and an unsupported entity body is 415.
func (h *Handler) handleMkcol(w http.ResponseWriter, r *http.Request) {
	abs, f := h.resolveOrFail(w, r)
	if f != nil {
		h.fail(w, r, *f)
		return
	}
	if hasBody(r) {
		h.fail(w, r, failure{Status: 415, Code: VerdictUnsupportedMediaType, Element: "unsupported-media-type",
			Pairs: [][2]string{{"detail", "this surface accepts no MKCOL body"}}})
		return
	}
	if _, err := os.Lstat(abs); err == nil {
		h.fail(w, r, failure{Status: 405, Code: VerdictMethodNotAllowed, Element: "method-not-allowed",
			Pairs: [][2]string{{"detail", "the URL is already mapped"}}})
		return
	}
	parent := filepath.Dir(abs)
	if fi, err := os.Stat(parent); err != nil || !fi.IsDir() {
		h.fail(w, r, failure{Status: 409, Code: VerdictConflict, Element: "conflict",
			Pairs: [][2]string{{"detail", "an ancestor collection does not exist"}}})
		return
	}
	if err := os.Mkdir(abs, 0o755); err != nil {
		h.fail(w, r, storageFailure(err))
		return
	}
	h.tree.bumpRev()
	w.WriteHeader(http.StatusCreated)
}

// handleCopyMove implements RFC 4918 §9.8/§9.9 with the destination semantics
// the spec's method matrix fixes: 201/204, 412 on Overwrite: F, 409 on a
// missing intermediate collection, 502 when the destination is outside this
// surface's namespace, 403 for an identical source/destination on MOVE.
func (h *Handler) handleCopyMove(w http.ResponseWriter, r *http.Request, move bool) {
	srcAbs, f := h.resolveOrFail(w, r)
	if f != nil {
		h.fail(w, r, *f)
		return
	}
	srcInfo, err := os.Lstat(srcAbs)
	if err != nil {
		h.fail(w, r, notFoundOrInternal(err))
		return
	}
	destHeader := strings.TrimSpace(r.Header.Get("Destination"))
	if destHeader == "" {
		h.fail(w, r, failure{Status: 400, Code: VerdictBadArguments, Element: "bad-arguments",
			Pairs: [][2]string{{"detail", "Destination header is required"}}})
		return
	}
	destPath, f := destinationPath(destHeader)
	if f != nil {
		h.fail(w, r, *f)
		return
	}
	dstAbs, err := h.tree.resolve(destPath)
	if err != nil {
		h.fail(w, r, failure{Status: 403, Code: VerdictWorkspaceInvalid, Element: "workspace-invalid"})
		return
	}
	if move && filepath.Clean(dstAbs) == filepath.Clean(srcAbs) {
		h.fail(w, r, failure{Status: 403, Code: VerdictForbidden, Element: "forbidden",
			Pairs: [][2]string{{"detail", "source and destination are the same resource"}}})
		return
	}
	if srcInfo.IsDir() && isWithin(srcAbs, dstAbs) {
		h.fail(w, r, failure{Status: 409, Code: VerdictConflict, Element: "conflict",
			Pairs: [][2]string{{"detail", "destination is inside the source collection"}}})
		return
	}
	overwrite := !strings.EqualFold(strings.TrimSpace(r.Header.Get("Overwrite")), "F")
	dstExists := false
	if _, err := os.Lstat(dstAbs); err == nil {
		dstExists = true
	}
	if dstExists && !overwrite {
		h.fail(w, r, failure{Status: 412, Code: VerdictPreconditionFailed, Element: "precondition-failed"})
		return
	}
	if parent := filepath.Dir(dstAbs); parent != "" {
		if fi, err := os.Stat(parent); err != nil || !fi.IsDir() {
			h.fail(w, r, failure{Status: 409, Code: VerdictConflict, Element: "conflict",
				Pairs: [][2]string{{"detail", "an intermediate destination collection does not exist"}}})
			return
		}
	}
	depthInfinity := true
	if d := strings.TrimSpace(r.Header.Get("Depth")); d != "" {
		switch {
		case d == "0":
			depthInfinity = false
		case strings.EqualFold(d, "infinity"):
			depthInfinity = true
		default:
			h.fail(w, r, failure{Status: 400, Code: VerdictInvalidDepth, Element: "invalid-depth"})
			return
		}
	}

	if dstExists {
		if err := os.RemoveAll(dstAbs); err != nil {
			h.fail(w, r, storageFailure(err))
			return
		}
	}
	if move {
		err = os.Rename(srcAbs, dstAbs)
		if err != nil {
			// Cross-device: fall back to copy + remove rather than refusing a
			// legal MOVE.
			if cerr := copyTree(srcAbs, dstAbs, true); cerr != nil {
				h.fail(w, r, storageFailure(cerr))
				return
			}
			err = os.RemoveAll(srcAbs)
		}
	} else {
		err = copyTree(srcAbs, dstAbs, depthInfinity)
	}
	if err != nil {
		h.fail(w, r, storageFailure(err))
		return
	}
	h.tree.bumpRev()
	if dstExists {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// hashIfRegular returns the content hash for a regular file and "" otherwise,
// so a conditional DELETE on a collection is not mistaken for a hash failure.
func (h *Handler) hashIfRegular(abs string) string {
	fi, err := os.Stat(abs)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	hash, err := h.tree.hashFile(abs)
	if err != nil {
		return ""
	}
	return hash
}

// destinationPath extracts the request path from a Destination header. A
// destination outside this surface's namespace is 502 — the RFC's own code for
// "the destination namespace refuses to accept the resource" (§9.8.4).
func destinationPath(dest string) (string, *failure) {
	u, err := url.Parse(dest)
	if err != nil {
		return "", &failure{Status: 400, Code: VerdictBadArguments, Element: "bad-arguments",
			Pairs: [][2]string{{"detail", "Destination is not a valid URI"}}}
	}
	p := u.Path
	if p == "" {
		p = u.Opaque
	}
	if p != Prefix && !strings.HasPrefix(p, Prefix+"/") {
		return "", &failure{Status: 502, Code: VerdictBadGateway, Element: "bad-gateway",
			Pairs: [][2]string{{"detail", "destination is outside this surface's namespace"}}}
	}
	return p, nil
}

// isWithin reports whether child is inside parent.
func isWithin(parent, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// checkPreconditions applies E-2 / RFC 9110 §13.1.1. A nil result means the
// request may proceed. If-Match uses the strong comparison function (a weak
// W/ tag never matches); If-None-Match uses the weak one.
func checkPreconditions(r *http.Request, exists bool, currentHash string) *failure {
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	if ifMatch != "" {
		ok := false
		if ifMatch == "*" {
			ok = exists
		} else if exists {
			ok = etagListMatches(ifMatch, currentHash, true)
		}
		if !ok {
			// Absence and a stale base are different diagnoses with different
			// remedies: create it, or re-read it (§3 E-2).
			if !exists {
				return &failure{Status: 412, Code: VerdictPreconditionFailed, Element: "precondition-failed"}
			}
			expected := firstTag(ifMatch)
			return &failure{
				Status:  412,
				Code:    VerdictHashMismatch,
				Element: "hash-mismatch",
				Headers: map[string]string{
					"X-Bunker-Current-Hash":  currentHash,
					"X-Bunker-Expected-Hash": expected,
				},
				Pairs: [][2]string{{"expected", expected}, {"current", currentHash}},
			}
		}
	}
	ifNoneMatch := strings.TrimSpace(r.Header.Get("If-None-Match"))
	if ifNoneMatch != "" {
		if ifNoneMatch == "*" {
			if exists {
				return &failure{Status: 412, Code: VerdictPreconditionFailed, Element: "precondition-failed"}
			}
		} else if exists && etagListMatches(ifNoneMatch, currentHash, false) {
			return &failure{Status: 412, Code: VerdictPreconditionFailed, Element: "precondition-failed"}
		}
	}
	return nil
}

// etagFor renders the strong ETag E-1 fixes: the content hash, quoted, never
// weak (a weak tag would invite the byte-range/conditional-merge semantics the
// refusal rule forbids).
func etagFor(hash string) string { return `"` + hash + `"` }

// etagListMatches compares an If-Match/If-None-Match list against the current
// content hash. strong=true rejects weak (W/) tags outright.
func etagListMatches(list, hash string, strong bool) bool {
	for _, part := range strings.Split(list, ",") {
		tag := strings.TrimSpace(part)
		if tag == "*" {
			return true
		}
		if strings.HasPrefix(tag, "W/") {
			if strong {
				continue
			}
			tag = strings.TrimSpace(strings.TrimPrefix(tag, "W/"))
		}
		if strings.Trim(tag, `"`) == hash {
			return true
		}
	}
	return false
}

// normalizeTag reduces an ETag-ish header value to its bare content hash.
func normalizeTag(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimSpace(strings.TrimPrefix(v, "W/"))
	return strings.Trim(v, `"`)
}

// firstTag returns the bare value of the first tag in a list, which is what a
// refusal echoes when the client sent several.
func firstTag(list string) string {
	parts := strings.Split(list, ",")
	return normalizeTag(parts[0])
}

// parseSingleRange parses a single byte range. ok=false with
// satisfiable=true means "not a single range this surface honours" — the
// caller answers 200 with the full body; satisfiable=false means 416.
func parseSingleRange(spec string, size int64) (start, length int64, ok bool, satisfiable bool) {
	s := strings.TrimSpace(spec)
	if !strings.HasPrefix(s, "bytes=") {
		return 0, 0, false, true
	}
	s = strings.TrimSpace(strings.TrimPrefix(s, "bytes="))
	if s == "" || strings.Contains(s, ",") {
		return 0, 0, false, true
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false, false
	}
	first := strings.TrimSpace(parts[0])
	last := strings.TrimSpace(parts[1])
	switch {
	case first == "":
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, false
		}
		if n > size {
			n = size
		}
		return size - n, n, true, true
	case last == "":
		st, err := strconv.ParseInt(first, 10, 64)
		if err != nil || st < 0 || st >= size {
			return 0, 0, false, false
		}
		return st, size - st, true, true
	default:
		st, err1 := strconv.ParseInt(first, 10, 64)
		en, err2 := strconv.ParseInt(last, 10, 64)
		if err1 != nil || err2 != nil || st < 0 || en < st || st >= size {
			return 0, 0, false, false
		}
		if en >= size {
			en = size - 1
		}
		return st, en - st + 1, true, true
	}
}

// hasBody reports whether a request carries an entity body, for the methods
// whose bodies this surface refuses.
func hasBody(r *http.Request) bool {
	switch {
	case r.ContentLength > 0:
		return true
	case r.ContentLength == 0:
		return false
	case r.Body == nil:
		return false
	}
	buf := make([]byte, 1)
	n, err := r.Body.Read(buf)
	return n > 0 && (err == nil || errors.Is(err, io.EOF))
}

// contentTypeFor is the display content type of a file: the platform's
// extension table, else the RFC's default for an unknown type.
func contentTypeFor(abs string) string {
	if ct := mime.TypeByExtension(filepath.Ext(abs)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// compile-time proof that the handler satisfies http.Handler.
var _ http.Handler = (*Handler)(nil)
