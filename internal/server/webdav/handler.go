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
	"sync"
	"syscall"
	"time"

	"github.com/deployBunker/bunker/internal/invalidation"
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
	// `symlink` was added by BFS-018: an extension a client must be told about
	// before it can rely on the declared type vocabulary, because the failure
	// mode of guessing is not a missing feature — it is a corrupted tree.
	ExtensionsHeader = "identity,if_match_refuse,rev,tree,op,watch,symlink"
	// LinkTargetHeader is the declared request header that creates a symlink
	// (spec §3 E-7). Its value is the link's target path; a PUT that carries it
	// creates a link and takes no body. It is a header rather than a
	// delegated op on purpose: E-4's ops are read-only by invariant, and a
	// mutation must travel on a standard method (§3).
	LinkTargetHeader = "X-Bunker-Link-Target"
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
	// Invalidation is the SERVER-SIDE invalidation config surface (BFS-043):
	// the watcher and push knobs, each with a declared default and range in
	// internal/invalidation. nil means "nothing was configured", and every knob
	// then takes its declared default — a fact about the file rather than a
	// fallback for a broken value.
	//
	// A non-nil surface must be VALID: New refuses to build the handler when a
	// value is outside its declared range, naming the knob, the value and the
	// range. An invalid value is never replaced by a default (BFS-031's bound
	// that did not bound, BFS-032's counter that could never move: both are a
	// value that is reported and not enforced), and a value that is legal but
	// which the PLATFORM cannot honour is reported as an unhonourable pair with
	// configured AND observed — never clamped in silence.
	Invalidation *invalidation.Values
}

// InvalidationValuesOrDeferred reports the surface this handler will serve with,
// which is the configured surface when one was given and the declared defaults
// otherwise. It is exported because the daemon's config layer and the cells both
// need to ask the same question with the same answer.
func InvalidationValuesOrDeferred(cfg *invalidation.Values) invalidation.Values {
	if cfg == nil {
		return invalidation.DefaultValues()
	}
	return *cfg
}

// Handler serves the WebDAV surface.
type Handler struct {
	cfg  Config
	tree *tree
	// inv is the RESOLVED invalidation surface this handler serves with: the
	// configured values when a Config carried them, the declared defaults
	// otherwise. It is what the watcher was built from and what the read-back
	// reports when no watcher is running.
	inv invalidation.Values

	// watch is the server-side watcher (watch.go, BFS-035). It is nil unless the
	// deployment enabled one, and it is never nil silently: watchStatusSnapshot
	// reports the PROBED absence with its own reason when it is.
	watch    *watcher
	watchEnv watchEnv
	// watchProbe is the cached target-level probe (backend fact, mount table,
	// configured ceilings). It is what lets an ABSENT watcher still report its own
	// named reason without installing anything (§4).
	watchProbe     watchProbe
	watchProbeOnce sync.Once

	// testBeforeCommit is a TEST seam and is nil on every production path: it
	// runs inside §6.1 step 5's critical section, after the body has been
	// staged and immediately before the precondition is re-validated. It
	// exists because the window the row is about — a base that changes after
	// the early precondition evaluation, while the write has not landed yet —
	// cannot be reached deterministically from a request alone: a mutation
	// performed while the body is being read lands before the early evaluation
	// and is caught by it. A test sets this to place the out-of-band write in
	// the window itself.
	testBeforeCommit func(abs string)
}

// New builds the surface over cfg.Root. It fails rather than serving a root it
// cannot resolve (the daemon's fail-before-listen posture).
func New(cfg Config) (*Handler, error) {
	t, err := newTree(cfg.Root)
	if err != nil {
		return nil, err
	}
	// BFS-043: the invalidation surface is validated HERE as well as at config
	// load. Two gates, one table: a caller that builds a Config programmatically
	// (a cell, an embedder) cannot slip a value past the surface that the config
	// file would have refused.
	inv := InvalidationValuesOrDeferred(cfg.Invalidation)
	if err := inv.Validate(); err != nil {
		return nil, fmt.Errorf("webdav: invalidation config: %w", err)
	}
	// BFS-062: the frame's byte bound reaches the ONE site that measures a
	// frame (events.go's ledger push) from the resolved surface, so the number
	// the document publishes and the number the assembly obeys are the same
	// number.
	t.eventMaxBytes = inv.Push.MaxEventBytes
	// BFS-036: the push channel's bounds and cadences come from the SAME
	// resolved surface the watcher obeys, so the numbers the document publishes
	// and the numbers the channel enforces cannot drift (BFS-043's rule, one
	// subsystem over).
	t.pushCfg = pushSettings{
		bufferBytes:    inv.Push.SubscriberBufferBytes,
		bufferEvents:   inv.Push.SubscriberBufferEvents,
		maxSubscribers: inv.Push.MaxSubscribers,
		writeDeadline:  time.Duration(inv.Push.WriteDeadlineMS) * time.Millisecond,
		heartbeat:      time.Duration(inv.Watch.HeartbeatMS) * time.Millisecond,
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
	h := &Handler{cfg: cfg, tree: t, watchEnv: defaultWatchEnv(), inv: inv}
	// The target-level probe is taken EAGERLY here, not on the first capability
	// request. It is a per-target fact (mount table + configured inotify
	// ceilings) that cannot change under a running process, and leaving it lazy
	// makes whichever client arrives first pay a /proc/self/mountinfo read that
	// every later request gets for free — a latency cliff that shows up as a
	// first-request outlier, measured at ~1 ms on this box (see
	// docs/evidence/BFS-035-*.md, `BenchmarkProbeWatch`).
	h.primeWatchProbe()
	if inv.Watch.Enabled {
		// A watcher that cannot be established is not an error here: it is a
		// REPORTABLE state (§4), reported by the op, the capability document and
		// the degradations list — never a daemon that refuses to serve the tree.
		h.watch = startWatcher(t, h.watchEnv, watchOptionsFrom(inv))
	}
	return h, nil
}

// startWatch is the in-package seam every probe-matrix cell uses: it installs a
// watcher on an already-built handler with an injected environment and tunables,
// so each reason of §4 can be exercised on its own (a fake backend that returns
// ENOSPC, a mount table that says the root is fuse.sshfs, a heartbeat period of
// milliseconds) while the real probes keep their control arms.
func (h *Handler) startWatch(env watchEnv, opts watchOptions) *watcher {
	if h.watch != nil {
		h.watch.Close()
	}
	h.watchEnv = env
	// The injected environment changes what the target-level probe answers, so
	// the cached answer is re-taken here. Without this a cell that fakes the
	// mount table would be served the real host's probe.
	h.primeWatchProbe()
	h.watch = startWatcher(h.tree, env, opts)
	return h.watch
}

// primeWatchProbe takes the target-level probe (backend fact, mount table,
// configured ceilings) once and caches it. It is called EAGERLY by New so the
// request path never pays a first-call probe, and by startWatch after an
// environment is injected. watchStatusSnapshot calls it too, so a Handler built
// as a bare literal still self-heals instead of reporting a zero-valued probe.
func (h *Handler) primeWatchProbe() {
	h.watchProbeOnce = sync.Once{}
	h.watchProbeOnce.Do(func() { h.watchProbe = probeWatch(h.tree.rootPath(), h.watchEnv) })
}

// Close releases the watcher's goroutines and its watch set. It is a no-op when
// no watcher was established, and it is never required for correctness — a
// process that exits without it is not serving stale state, it is gone.
//
// BFS-036: it also ends the push channel's heartbeat goroutine. An attached
// subscriber whose request is still in flight ends with its own request context
// (the stream is closed by the server shutting down), so nothing here has to wait
// for one.
func (h *Handler) Close() {
	if h.watch != nil {
		h.watch.Close()
	}
	h.tree.closePush()
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
	// LSTAT first (BFS-018). A Stat here reports a symlink as a regular file of
	// its target's size and serves the TARGET's bytes under the link's URI: a
	// caller that asked for the link receives a COPY of what it points at, with
	// no way to tell. Two answers are honest and this surface gives the loud
	// one — the link has no bytes of its own to serve, so the request is
	// refused by name and the target travels as the declared b:link-target
	// property of PROPFIND instead.
	lfi, lerr := os.Lstat(abs)
	if lerr == nil && lfi.Mode()&os.ModeSymlink != 0 {
		w.Header().Set("Allow", strings.Join(servedMethods, ", "))
		h.fail(w, r, failure{
			Status:  405,
			Code:    VerdictSymlinkNotAFile,
			Element: "symlink-not-a-file",
			Pairs: [][2]string{
				{"detail", "this URL is a symlink, not a file: it has no entity bytes of its own, and reading it would return its target's bytes (a copy) under the link's name. PROPFIND this URL and read b:link-target for the link's target path"},
			},
		})
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
//
// Since BFS-018 it also implements §3 E-7: a PUT that carries the declared
// X-Bunker-Link-Target header creates a SYMLINK (and takes no body), and a
// body-bearing PUT onto a URL that is currently a symlink is REFUSED by name
// rather than silently replacing the link with a regular file. That second
// rule is the server-side half of the row: a client that materialised a link as
// a file and writes it back cannot corrupt a tree through this surface even if
// it never learned about the extension.
func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request) {
	abs, f := h.resolveOrFail(w, r)
	if f != nil {
		h.fail(w, r, *f)
		return
	}
	// LSTAT, not Stat: the entry's OWN kind decides this write's shape, and a
	// Stat would report a symlink as whatever it points at (BFS-018).
	lfi, lerr := os.Lstat(abs)
	if lerr == nil && lfi.IsDir() {
		h.fail(w, r, failure{Status: 405, Code: VerdictMethodNotAllowed, Element: "method-not-allowed",
			Pairs: [][2]string{{"detail", "PUT targets a non-collection resource"}}})
		return
	}
	if target := r.Header.Get(LinkTargetHeader); target != "" {
		h.writeLinkPut(w, r, abs, target)
		return
	}
	if lerr == nil && lfi.Mode()&os.ModeSymlink != 0 {
		h.refuseUndeclaredSymlinkReplace(w, r, abs, lfi)
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

	// §6.1 step 4 (with step 6's identical-bytes arm): the early evaluation.
	// It is an EARLY refusal, not the decision — the body has already arrived
	// by the time it runs, so the base it read may be stale by the time the
	// write lands. Step 5 re-evaluates the same predicate inside the commit
	// (writePut), which is what closes that window.
	switch dec, pf := putPrecondition(r, exists, curHash, bodyHash); dec {
	case putNoop:
		answerIdenticalContent(w, curHash)
		return
	case putRefuse:
		h.fail(w, r, *pf)
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

	h.writePut(w, r, abs, body, bodyHash)
}

// writePut is §6.1 step 5: the commit. The body is staged into a temp sibling
// of the target, then — in the same critical section as the rename — the
// current bytes are re-read, re-hashed and the precondition is evaluated
// again, so a base that moved during the body transfer is refused with the
// same 412 + hash_mismatch rather than overwritten.
//
// The same predicate covers the create-only rule (If-None-Match: *): a
// resource created out-of-band during the transfer is refused rather than
// clobbered. R1 is untouched — with neither header sent the predicate is
// silent at both evaluations, so a stock client's write stays unconditional.
func (h *Handler) writePut(w http.ResponseWriter, r *http.Request, abs string, body []byte, bodyHash string) {
	tmp, err := stageBody(abs, body)
	if err != nil {
		h.fail(w, r, storageFailure(err))
		return
	}
	defer func() { _ = os.Remove(tmp) }() // no-op once the rename succeeded

	unlock := h.tree.lockPath(abs)
	defer unlock()

	if hook := h.testBeforeCommit; hook != nil {
		hook(abs)
	}

	// The state as of the commit, read from the bytes on disk right now.
	exists := false
	curHash := ""
	if e, err := h.tree.freshEntry(abs); err != nil {
		if !os.IsNotExist(err) {
			h.fail(w, r, notFoundOrInternal(err))
			return
		}
	} else {
		exists, curHash = true, e.hash
		h.tree.remember(abs, e)
	}

	switch dec, pf := putPrecondition(r, exists, curHash, bodyHash); dec {
	case putNoop:
		// The base moved but the arriving bytes are already there: D3's
		// reported no-op, answered identically wherever the race landed.
		answerIdenticalContent(w, curHash)
		return
	case putRefuse:
		h.fail(w, r, *pf)
		return
	}

	if err := commitStaged(tmp, abs); err != nil {
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

// linkBase is the state of the entry a link PUT is about to replace, read from
// the entry ITSELF. It exists so the early evaluation and the commit-time
// re-validation observe the same three facts through one function (writePut's
// rule, one shape over).
//
// `hash` is intentionally empty for a symlink: a link has no content bytes, so
// it has no content hash, and hashing here would publish the TARGET's hash as
// this link's identity — the dereference BFS-018 is about, committed in the
// precondition path instead of the read path.
func (h *Handler) linkBase(abs string) (exists, isLink bool, target, hash string) {
	fi, err := os.Lstat(abs)
	if err != nil {
		return false, false, "", ""
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t, rerr := os.Readlink(abs)
		if rerr != nil {
			return true, true, "", ""
		}
		return true, true, t, ""
	}
	if !fi.Mode().IsRegular() {
		return true, false, "", ""
	}
	if got, herr := h.tree.hashFile(abs); herr == nil {
		hash = got
	}
	return true, false, "", hash
}

// answerIdenticalLink is D3's reported no-op for the one entity that has no
// content hash: the URL already holds a link with exactly this target, so
// nothing is written and the answer says so. No ETag and no X-Bunker-Hash ride
// on it — forging an empty one would claim an identity this resource does not
// have.
func answerIdenticalLink(w http.ResponseWriter) {
	w.Header().Set("X-Bunker-Noop", "1")
	w.Header().Set("X-Bunker-Verdict", string(VerdictIdenticalContent))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// writeLinkPut implements §3 E-7: PUT + X-Bunker-Link-Target creates a symlink
// whose target is the header's value. The body MUST be empty — a link's entity
// is its target path, and a surface that accepted a body here would have two
// competing answers to "what is this entry's content", one of which is the
// materialisation BFS-018 removes.
//
// The commit is one rename of a staged symlink (stageLink + commitStaged), the
// SAME publication point a staged body uses, under the SAME per-path lock —
// so BFS-020's created-name publication and BFS-038's one-pointer-swap hold for
// this shape too, and a failed or killed request cannot leave a half-created
// entry.
func (h *Handler) writeLinkPut(w http.ResponseWriter, r *http.Request, abs, target string) {
	if target == "" {
		h.fail(w, r, failure{Status: 400, Code: VerdictSymlinkTargetInvalid, Element: "symlink-target-invalid",
			Pairs: [][2]string{{"detail", "X-Bunker-Link-Target is empty: an empty target is not a link this surface can store (the header must carry the link's target path)"}}})
		return
	}
	if len(target) > linkTargetMaxBytes {
		h.fail(w, r, failure{Status: 400, Code: VerdictSymlinkTargetInvalid, Element: "symlink-target-invalid",
			Pairs: [][2]string{{"limit", strconv.Itoa(linkTargetMaxBytes)}, {"got", strconv.Itoa(len(target))},
				{"detail", "the declared link target is longer than this surface stores; a truncated target would be a different link"}}})
		return
	}
	if strings.ContainsRune(target, 0x00) {
		h.fail(w, r, failure{Status: 400, Code: VerdictSymlinkTargetInvalid, Element: "symlink-target-invalid",
			Pairs: [][2]string{{"detail", "the declared link target carries a NUL byte"}}})
		return
	}
	if hasBody(r) {
		h.fail(w, r, failure{Status: 400, Code: VerdictBadArguments, Element: "bad-arguments",
			Pairs: [][2]string{{"detail", "X-Bunker-Link-Target creates a symlink from the declared target, so the request body must be empty: a symlink's entity is its target path and never bytes"}}})
		return
	}
	dir := filepath.Dir(abs)
	if dfi, err := os.Stat(dir); err != nil || !dfi.IsDir() {
		h.fail(w, r, failure{Status: 409, Code: VerdictConflict, Element: "conflict",
			Pairs: [][2]string{{"detail", "parent collection does not exist"}}})
		return
	}

	exists, isLink, curTarget, curHash := h.linkBase(abs)
	// The early evaluation (§6.1 step 4). A tag can never name a link, so an
	// If-Match tag against one is answered with the diagnosis rather than with
	// an empty current hash; the commit re-evaluates what it can.
	if isLink {
		if ifMatch := strings.TrimSpace(r.Header.Get("If-Match")); ifMatch != "" && ifMatch != "*" {
			h.fail(w, r, failure{Status: 412, Code: VerdictPreconditionFailed, Element: "precondition-failed",
				Pairs: [][2]string{{"detail", "If-Match names a content hash and this URL is a symlink: a symlink has no content bytes to hash, so no tag can name it. Use If-Match: * , or DELETE the link first"}}})
			return
		}
	}
	if isLink && curTarget == target && strings.TrimSpace(r.Header.Get("If-None-Match")) != "*" {
		answerIdenticalLink(w)
		return
	}
	if f := checkPreconditions(r, exists, curHash); f != nil {
		h.fail(w, r, *f)
		return
	}

	unlock := h.tree.lockPath(abs)
	defer unlock()
	if hook := h.testBeforeCommit; hook != nil {
		hook(abs)
	}
	// The base as of the commit, re-read inside the critical section: a link
	// that already holds this target is the reported no-op, and a resource
	// created out-of-band during this request is refused rather than clobbered
	// (writePut's rule for the create-only case).
	cExists, cIsLink, cTarget, cHash := h.linkBase(abs)
	if cIsLink && cTarget == target && strings.TrimSpace(r.Header.Get("If-None-Match")) != "*" {
		answerIdenticalLink(w)
		return
	}
	if f := checkPreconditions(r, cExists, cHash); f != nil {
		h.fail(w, r, *f)
		return
	}

	tmp, err := stageLink(abs, target)
	if err != nil {
		h.fail(w, r, storageFailure(err))
		return
	}
	defer func() { _ = os.Remove(tmp) }() // no-op once the rename succeeded
	if err := commitStaged(tmp, abs); err != nil {
		h.fail(w, r, storageFailure(err))
		return
	}
	h.tree.forget(abs)
	h.tree.bumpRev()
	w.Header().Set(LinkTargetHeader, target)
	if exists {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// refuseUndeclaredSymlinkReplace refuses a body-bearing PUT onto a URL that is
// a symlink (BFS-018). It is the corruption guard, not a convenience: the
// client defect this row was filed for materialises a link as a small file
// whose content is the link's target path, and the write-back of that file is
// exactly this request — a PUT with a body onto a link, which would replace the
// link with a regular file and turn a type change into a commit.
//
// The caller's own create-only precondition is answered first: "the base
// exists" is the answer `If-None-Match: *` asks for, and it is not a
// type-change question.
func (h *Handler) refuseUndeclaredSymlinkReplace(w http.ResponseWriter, r *http.Request, abs string, lfi os.FileInfo) {
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == "*" {
		h.fail(w, r, failure{Status: 412, Code: VerdictPreconditionFailed, Element: "precondition-failed"})
		return
	}
	target, _ := h.linkTargetOf(abs, lfi)
	pairs := [][2]string{{"detail", "this URL is a symlink, and a PUT with a body would replace it with a regular file whose content is the body — the type change a client that materialised the link as a file writes back. To create or replace a link, PUT it with X-Bunker-Link-Target; to put a file here, DELETE the link first (or MOVE a file over it)"}}
	if target != "" {
		pairs = append(pairs, [2]string{"link_target", target})
	}
	h.fail(w, r, failure{Status: 409, Code: VerdictSymlinkUndeclaredReplace, Element: "symlink-undeclared-replace", Pairs: pairs})
}

// putDecision is the outcome of evaluating §6.1 step 4's precondition against
// one observed state of the target.
type putDecision int

const (
	// putProceed: the precondition holds, or none was sent — the write may land.
	putProceed putDecision = iota
	// putNoop: the precondition is false but the arriving bytes are already on
	// disk — D3's REPORTED no-op. Nothing is written, the mtime does not move.
	putNoop
	// putRefuse: the precondition is false and the bytes differ — 412.
	putRefuse
)

// putPrecondition evaluates E-2 / §6.1 step 4 against the state it is handed.
// It is called twice on one PUT — once as the early evaluation a request
// answers immediately, and once inside the commit immediately before the
// rename (§6.1 step 5). One function, so the two evaluations cannot drift.
func putPrecondition(r *http.Request, exists bool, currentHash, bodyHash string) (putDecision, *failure) {
	// D3, WIDENED FOR A RETRY (BFS-039). The arriving bytes ARE the current
	// content, so there is nothing to write and the answer is the reported
	// no-op — and this now holds when the precondition SUCCEEDS, not only when
	// the base was stale. The case it adds is the one a cancelled write
	// produces: the caller retries the same bytes, the base is correct, and
	// before this rule the surface re-wrote identical content, moving the
	// mtime and bumping the served revision for a change that did not happen —
	// a spurious invalidation every other client then acts on.
	//
	// The create-only rule (`If-None-Match: *`) is deliberately EXEMPT: a
	// caller that asked to create a resource it believed absent must still be
	// told it is not absent (BFS-015's rule-2 arm), whatever the bytes happen
	// to be.
	if exists && currentHash == bodyHash && r.Header.Get("If-None-Match") != "*" {
		return putNoop, nil
	}
	f := checkPreconditions(r, exists, currentHash)
	switch {
	case f == nil:
		return putProceed, nil
	case exists && currentHash == bodyHash:
		return putNoop, nil
	default:
		return putRefuse, f
	}
}

// answerIdenticalContent is D3's reported no-op: the change the caller asked
// for has already happened, so nothing is written, the mtime does not move,
// and the answer says so in the header and the verdict code rather than
// pretending a write occurred.
func answerIdenticalContent(w http.ResponseWriter, hash string) {
	w.Header().Set("ETag", etagFor(hash))
	w.Header().Set("X-Bunker-Hash", hash)
	w.Header().Set("X-Bunker-Noop", "1")
	w.Header().Set("X-Bunker-Verdict", string(VerdictIdenticalContent))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
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
	// LSTAT (BFS-018): a symlink is not a regular file, so it has no content
	// hash. A Stat here would hand the TARGET's hash to a conditional request
	// aimed at the link — a caller could then name a.txt's hash and delete the
	// link, having never looked at the link at all.
	fi, err := os.Lstat(abs)
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
