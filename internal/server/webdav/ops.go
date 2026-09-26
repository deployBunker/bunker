package webdav

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// opCatalogue is E-4's op vocabulary (§3 E-4). implementedOps is what this
// build actually serves; every other name answers a structured
// capability_unavailable naming the slice that will deliver it, rather than
// disappearing (R3).
var (
	opCatalogue    = []string{"capabilities", "status", "diff", "rev-parse", "ls-files", "snapshot", "events", "watch"}
	implementedOps = map[string]bool{"capabilities": true, "snapshot": true}
)

// envelope is the single JSON shape every E-4 response uses, success or
// failure, so a consumer has exactly one decoder (§3 E-4, §10.4).
type envelope struct {
	OK         bool           `json:"ok"`
	Op         string         `json:"op"`
	Verdict    string         `json:"verdict"`
	Rev        string         `json:"rev"`
	Tree       string         `json:"tree"`
	Proto      string         `json:"proto"`
	DurationMS int64          `json:"duration_ms"`
	Truncated  bool           `json:"truncated"`
	Result     any            `json:"result"`
	Error      *envelopeError `json:"error"`
}

type envelopeError struct {
	Capability string `json:"capability,omitempty"`
	Scope      string `json:"scope,omitempty"`
	Phase      string `json:"phase,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// handlePost is E-4: POST is the carrier because RFC 4918 §9.5 leaves POST
// semantics server-defined, every client and proxy understands it, and POST is
// not safe — which is why every op in this catalogue is read-only. A POST
// without an X-Bunker-Op header, or with one this build does not know, is a
// loud 400 rather than a guess at the caller's intent.
func (h *Handler) handlePost(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	op := strings.TrimSpace(r.Header.Get("X-Bunker-Op"))
	if op == "" {
		h.writeEnvelope(w, r, start, "", 400, VerdictExtensionOpMissing, false, nil,
			&envelopeError{Detail: "X-Bunker-Op header is required for a POST on this surface"})
		return
	}
	switch op {
	case "capabilities":
		if _, f := h.readBody(w, r); f != nil {
			h.failEnvelope(w, r, start, op, *f)
			return
		}
		h.writeEnvelope(w, r, start, op, 200, VerdictOK, false,
			map[string]any{"capabilities": h.capabilityDocument(r)}, nil)

	case "snapshot":
		h.handleSnapshot(w, r, start)

	case "watch", "events":
		// No watcher on this target: the declared degradation, with the mode
		// actually in force, so "degraded" can never be mistaken for "quiet"
		// (§3 E-6, AC-9).
		h.writeEnvelope(w, r, start, op, 501, VerdictCapabilityUnavailable, false, nil,
			&envelopeError{
				Capability: op, Scope: "target", Mode: "poll",
				Detail: "no inotify watcher on this target; poll with HEAD/ETag or an X-Bunker-Op: snapshot diff",
			})

	case "status", "diff", "rev-parse", "ls-files":
		// Part of the E-4 catalogue, not in this build (slice C5).
		h.writeEnvelope(w, r, start, op, 501, VerdictCapabilityUnavailable, false, nil,
			&envelopeError{
				Capability: op, Scope: "build", Phase: "C5",
				Detail: "delegated whole-tree op not in this build (slice C5); read the tree with PROPFIND or X-Bunker-Op: snapshot",
			})

	default:
		h.writeEnvelope(w, r, start, op, 400, VerdictOpUnknown, false, nil,
			&envelopeError{Detail: "unknown X-Bunker-Op value"})
	}
}

// snapshotArgs is the fixed argument vocabulary of the snapshot op. There is
// no free-form command field anywhere in E-4: each op maps to a fixed
// structure, so the surface cannot be turned into an execution path.
type snapshotArgs struct {
	Path        string `json:"path"`
	Depth       string `json:"depth"`
	IncludeHash bool   `json:"include_hash"`
}

type snapshotEntry struct {
	Path        string `json:"path"`
	Type        string `json:"type"`
	Size        int64  `json:"size"`
	MtimeUnixMS int64  `json:"mtime_unix_ms"`
	Mode        string `json:"mode"`
	Hash        string `json:"hash,omitempty"`
}

// handleSnapshot serves one call that replaces the refused
// PROPFIND Depth: infinity for our own client (§3 E-4). It is read-only and
// its result is bounded: the entry list is truncated to the caller's
// X-Bunker-Max-Bytes (capped by the server's absolute cap) and every
// truncation is reported in the envelope's `truncated` field (A-11).
func (h *Handler) handleSnapshot(w http.ResponseWriter, r *http.Request, start time.Time) {
	body, f := h.readBody(w, r)
	if f != nil {
		h.failEnvelope(w, r, start, "snapshot", *f)
		return
	}
	args := snapshotArgs{}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &args); err != nil {
			h.writeEnvelope(w, r, start, "snapshot", 400, VerdictBadArguments, false, nil,
				&envelopeError{Detail: "snapshot arguments are not a JSON object of {\"path\",\"depth\",\"include_hash\"}"})
			return
		}
	}
	depthInfinity := false
	switch strings.TrimSpace(args.Depth) {
	case "", "1":
	case "infinity":
		depthInfinity = true
	default:
		h.writeEnvelope(w, r, start, "snapshot", 400, VerdictBadArguments, false, nil,
			&envelopeError{Detail: "snapshot depth must be \"1\" or \"infinity\""})
		return
	}

	abs := h.tree.rootPath()
	if p := strings.TrimSpace(args.Path); p != "" {
		resolved, err := h.tree.resolve(Prefix + "/" + strings.TrimPrefix(p, "/"))
		if err != nil {
			h.writeEnvelope(w, r, start, "snapshot", 403, VerdictWorkspaceInvalid, false, nil,
				&envelopeError{Detail: "snapshot path is outside the served tree"})
			return
		}
		abs = resolved
	}
	if _, err := os.Stat(abs); err != nil {
		h.writeEnvelope(w, r, start, "snapshot", 404, VerdictNotFound, false, nil,
			&envelopeError{Detail: "snapshot path does not exist"})
		return
	}

	budget, f := h.envelopeBudget(r)
	if f != nil {
		h.failEnvelope(w, r, start, "snapshot", *f)
		return
	}
	entries, truncated, err := h.snapshotEntries(abs, depthInfinity, args.IncludeHash, budget)
	if err != nil {
		h.writeEnvelope(w, r, start, "snapshot", 500, VerdictInternal, false, nil,
			&envelopeError{Detail: "snapshot walk failed"})
		return
	}
	if truncated && len(entries) == 0 {
		h.writeEnvelope(w, r, start, "snapshot", 413, VerdictResultTooLarge, false, nil,
			&envelopeError{Detail: "a single snapshot entry exceeds the result cap; raise X-Bunker-Max-Bytes"})
		return
	}
	h.writeEnvelope(w, r, start, "snapshot", 200, VerdictOK, truncated,
		map[string]any{"count": len(entries), "entries": entries}, nil)
}

// envelopeBudget resolves the effective result cap from X-Bunker-Max-Bytes,
// clamped to the server's absolute cap.
func (h *Handler) envelopeBudget(r *http.Request) (int, *failure) {
	max := h.cfg.DefaultMaxBytes
	if raw := strings.TrimSpace(r.Header.Get("X-Bunker-Max-Bytes")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			return 0, &failure{Status: 400, Code: VerdictBadArguments, Element: "bad-arguments",
				Pairs: [][2]string{{"detail", "X-Bunker-Max-Bytes must be a positive integer"}}}
		}
		max = n
	}
	if max > h.cfg.AbsMaxBytes {
		max = h.cfg.AbsMaxBytes
	}
	if max < 256 {
		max = 256
	}
	// Leave room for the envelope around the result.
	return int(max) - 256, nil
}

// snapshotEntries walks the tree and stops at the budget, reporting whether it
// had to. A truncated answer is always reported, never silently shortened.
func (h *Handler) snapshotEntries(abs string, depthInfinity, includeHash bool, budget int) ([]snapshotEntry, bool, error) {
	root := h.tree.rootPath()
	entries := make([]snapshotEntry, 0, 64)
	used := 0
	truncated := false

	add := func(p string, fi os.FileInfo) bool {
		e := snapshotEntry{
			Path:        p,
			Type:        entryType(fi),
			Size:        fi.Size(),
			MtimeUnixMS: fi.ModTime().UnixNano() / int64(time.Millisecond),
			Mode:        fmt.Sprintf("%04o", fi.Mode().Perm()),
		}
		if includeHash && fi.Mode().IsRegular() {
			if hash, err := h.tree.hashFile(filepath.Join(root, filepath.FromSlash(p))); err == nil {
				e.Hash = hash
			}
		}
		encoded, err := json.Marshal(e)
		if err != nil {
			return false
		}
		if used+len(encoded)+2 > budget {
			truncated = true
			return false
		}
		used += len(encoded) + 2
		entries = append(entries, e)
		return true
	}

	rel := func(p string) string {
		r, err := filepath.Rel(root, p)
		if err != nil {
			return p
		}
		return filepath.ToSlash(r)
	}

	if !depthInfinity {
		info, err := os.Stat(abs)
		if err != nil {
			return nil, false, err
		}
		if !add(rel(abs), info) {
			return entries, truncated, nil
		}
		if !info.IsDir() {
			return entries, truncated, nil
		}
		children, err := os.ReadDir(abs)
		if err != nil {
			return nil, false, err
		}
		for _, child := range children {
			if isTempName(child.Name()) {
				continue
			}
			info, err := child.Info()
			if err != nil {
				continue
			}
			if !add(rel(filepath.Join(abs, child.Name())), info) {
				return entries, truncated, nil
			}
		}
		return entries, truncated, nil
	}

	walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if isTempName(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if !add(rel(p), info) {
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		return nil, false, walkErr
	}
	return entries, truncated, nil
}

func entryType(fi os.FileInfo) string {
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return "symlink"
	case fi.IsDir():
		return "dir"
	default:
		return "file"
	}
}

// writeEnvelope renders the E-4 envelope: identical keys on success and
// failure, the verdict in the header AND the body, and no-store caching
// (§3, §6.1 item 4).
func (h *Handler) writeEnvelope(w http.ResponseWriter, r *http.Request, start time.Time, op string, status int, code Verdict, truncated bool, result any, eerr *envelopeError) {
	env := envelope{
		OK:         status == http.StatusOK,
		Op:         op,
		Verdict:    string(code),
		Rev:        h.tree.revToken(),
		Tree:       h.tree.identity(),
		Proto:      r.Proto,
		DurationMS: time.Since(start).Milliseconds(),
		Truncated:  truncated,
		Result:     result,
		Error:      eerr,
	}
	body, err := json.Marshal(env)
	if err != nil {
		status, code = 500, VerdictInternal
		body = []byte(`{"ok":false,"op":"` + op + `","verdict":"internal","result":null,"error":{"detail":"envelope could not be encoded"}}`)
	}
	body = append(body, '\n')
	w.Header().Set("X-Bunker-Op", op)
	w.Header().Set("X-Bunker-Verdict", string(code))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if v := capabilityHeaderValue(eerr2Capability(eerr), eerr2Scope(eerr), eerr2Phase(eerr), eerr2Mode(eerr)); v != "" {
		w.Header().Set("X-Bunker-Capability", v)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// failEnvelope converts a standard-method failure into the E-4 envelope
// shape, so a POST never answers XML and a consumer never needs two decoders.
func (h *Handler) failEnvelope(w http.ResponseWriter, r *http.Request, start time.Time, op string, f failure) {
	h.writeEnvelope(w, r, start, op, f.Status, f.Code, false, nil,
		&envelopeError{Capability: f.Capability, Scope: f.Scope, Phase: f.Phase, Mode: f.Mode})
}

func eerr2Capability(e *envelopeError) string {
	if e == nil {
		return ""
	}
	return e.Capability
}

func eerr2Scope(e *envelopeError) string {
	if e == nil {
		return ""
	}
	return e.Scope
}

func eerr2Phase(e *envelopeError) string {
	if e == nil {
		return ""
	}
	return e.Phase
}

func eerr2Mode(e *envelopeError) string {
	if e == nil {
		return ""
	}
	return e.Mode
}

// capabilityDocument builds §4.2's document from the RUNNING process: it
// reports reality, not aspiration. The one part that is version-dependent is
// `server.proto`, which is the version this request actually arrived on.
func (h *Handler) capabilityDocument(r *http.Request) map[string]any {
	// BFS-007: the authority to advertise is the LIVE QUIC socket when there
	// is one — it may be an explicit server.h3_addr, i.e. a different port
	// than the TCP listener this request arrived on. With no live listener the
	// field still names the authority O-5 gives it (this listener's own port),
	// which is what a client should dial once h3 is switched on. `available`
	// is never derived from that: only a bound socket makes it true.
	port, h3Live := 0, false
	if p, live := h.cfg.H3.Port(); live {
		port, h3Live = p, true
	} else {
		port = localPort(r)
	}
	altSvc := ""
	if port > 0 {
		altSvc = AltSvcValue(port)
	}
	ops := make([]string, 0, len(opCatalogue))
	for _, op := range opCatalogue {
		if implementedOps[op] {
			ops = append(ops, op)
		}
	}
	classes := []string{"1"} // "2" only when LOCK is live and enforcing (A-2)

	return map[string]any{
		"surface":          Surface,
		"document_version": DocumentVersion,
		"classes":          classes,
		"methods":          servedMethods,
		"extensions": map[string]any{
			"identity":        map[string]any{"name": "X-Bunker-Hash", "v": 1, "etag": "strong", "alg": "sha256", "format": "sha256:<64 hex>"},
			"if_match_refuse": map[string]any{"name": "If-Match/If-None-Match", "v": 1, "methods": []string{"PUT", "DELETE", "MOVE", "COPY", "PROPPATCH", "LOCK"}},
			"rev":             map[string]any{"name": "X-Bunker-Rev", "v": 1, "kind": h.revKind()},
			"tree":            map[string]any{"name": "X-Bunker-Tree", "v": 1},
			"op": map[string]any{
				"name": "X-Bunker-Op", "v": 1, "read_only": true, "ops": ops,
				"default_max_bytes": h.cfg.DefaultMaxBytes, "abs_max_bytes": h.cfg.AbsMaxBytes,
			},
			"watch": map[string]any{
				"name": "X-Bunker-Op: watch", "v": 1, "mode": "poll",
				"modes": map[string]any{"push": "inotify\u2192stream", "poll": "X-Bunker-Op: events"},
			},
		},
		"transports": map[string]any{
			"http/1.1": map[string]any{"alpn": nil, "multiplexed": false, "server_push": false, "available": true},
			"h2":       map[string]any{"alpn": "h2", "tls": true, "multiplexed": true, "server_push": false, "available": h.cfg.TLS},
			"h2c":      map[string]any{"mode": "prior-knowledge", "requires_opt_in": true, "upgrade_dance": false, "available": h.cfg.H2C},
			"h3":       map[string]any{"alpn": "h3", "transport": "quic/udp", "requires_tls": "1.3", "alt_svc": altSvc, "available": h3Live},
		},
		"limits": map[string]any{
			"propfind_depth":          []int{0, 1},
			"propfind_depth_infinity": false,
			"propfind_allprop_hashes": false,
			"max_request_bytes":       h.cfg.MaxRequestBytes,
		},
		"server": map[string]any{
			"build": h.cfg.Build,
			"proto": r.Proto,
			"tree":  h.tree.identity(),
			"rev":   h.tree.revToken(),
		},
		"degradations": h.degradations(),
	}
}

func (h *Handler) revKind() string {
	if gitHead(h.tree.rootPath()) != "" {
		return "git"
	}
	return "counter"
}

// degradations enumerates every capability this process does not have, with
// the scope that says why (§4.2 rule 2, §5.2). Anything absent here is
// available; nothing is implied.
func (h *Handler) degradations() []map[string]any {
	out := []map[string]any{
		{
			"capability": "watch", "scope": "target", "mode": "poll",
			"detail": "inotify watcher absent on this target; poll with HEAD/ETag",
		},
		{
			"capability": "lock", "scope": "build", "phase": "C4", "mode": "none",
			"detail": "no lease-backed LOCK in this build; DAV: 1 is advertised, DAV: 2 is not",
		},
	}
	// BFS-007: h3 is no longer a build-level absence — the listener exists and
	// is reported when it is live — so the entry appears only while no QUIC
	// socket is bound. "Anything absent here is available; nothing is implied"
	// cuts both ways: an entry that stayed after the listener came up would be
	// a degradation report that no longer describes the running process.
	if _, live := h.cfg.H3.Port(); !live {
		out = append(out, map[string]any{
			"capability": "h3", "scope": "build", "mode": "http/2",
			"detail": "no HTTP/3 listener is live in this process: QUIC always encrypts, so h3 needs tls.enabled: true plus server.h3_enabled: true (BFS-007)",
		})
	}
	for _, op := range opCatalogue {
		if implementedOps[op] {
			continue
		}
		out = append(out, map[string]any{
			"capability": "op:" + op, "scope": "build", "phase": "C5",
			"detail": "delegated op not in this build (slice C5)",
		})
	}
	if !h.cfg.TLS && !h.cfg.H2C {
		out = append(out, map[string]any{
			"capability": "h2", "scope": "transport", "mode": "http/1.1",
			"detail": "tls.enabled is false and server.h2c_enabled is false: this listener serves HTTP/1.1 only",
		})
	}
	if h.cfg.H2C {
		out = append(out, map[string]any{
			"capability": "h2c", "scope": "transport", "mode": "prior-knowledge",
			"detail": "h2c is prior-knowledge only: net/http does not implement the RFC 7540 Upgrade dance, so a client that sends \"Upgrade: h2c\" silently receives HTTP/1.1",
		})
	}
	return out
}

// localPort is the port this request was served on, used for the h3 Alt-Svc
// authority the capability document reports when no QUIC listener is live. It
// returns 0 when neither the listener address nor the Host header carries a
// usable port, which the caller reports as "no authority" rather than
// inventing one.
func localPort(r *http.Request) int {
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if _, port, err := net.SplitHostPort(addr.String()); err == nil {
			if p, err := strconv.Atoi(port); err == nil {
				return p
			}
		}
	}
	if _, port, err := net.SplitHostPort(r.Host); err == nil {
		if p, err := strconv.Atoi(port); err == nil {
			return p
		}
	}
	return 0
}

// readLimited reads a metadata body, refusing rather than truncating when it
// exceeds the cap.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > limit {
		return nil, errors.New("body exceeds the metadata request limit")
	}
	return buf, nil
}
