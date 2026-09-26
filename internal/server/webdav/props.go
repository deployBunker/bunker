package webdav

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// nsRegistry maps a request property's namespace URI onto the prefix the
// response uses: D for DAV:, b for urn:bunker:fs:1, and e0, e1, … for anything
// else a client names (dead properties in foreign namespaces). The prefixes
// are declared once on the multistatus root, exactly as the spec's wire
// examples do.
type nsRegistry struct {
	extra map[string]string
	order []string
}

func newNSRegistry() *nsRegistry { return &nsRegistry{extra: map[string]string{}} }

func (reg *nsRegistry) prefix(ns string) string {
	switch ns {
	case NSDav:
		return "D"
	case NSBunker:
		return "b"
	case "":
		return ""
	}
	if p, ok := reg.extra[ns]; ok {
		return p
	}
	p := fmt.Sprintf("e%d", len(reg.order))
	reg.extra[ns] = p
	reg.order = append(reg.order, ns)
	return p
}

func (reg *nsRegistry) attrs() []xml.Attr {
	attrs := []xml.Attr{
		{Name: xml.Name{Local: "xmlns:D"}, Value: NSDav},
		{Name: xml.Name{Local: "xmlns:b"}, Value: NSBunker},
	}
	for _, ns := range reg.order {
		attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "xmlns:" + reg.extra[ns]}, Value: ns})
	}
	return attrs
}

// qualifiedName renders the response form of a request property.
func (reg *nsRegistry) qualifiedName(n xml.Name) string {
	prefix := reg.prefix(n.Space)
	if prefix == "" {
		return n.Local
	}
	return prefix + ":" + n.Local
}

// allPropFile and allPropCollection are the properties `allprop` returns
// (§2.2). bunkerd:hash is deliberately absent: hashing every member of a
// collection is the measured stall class, so the expensive live property is
// returned only when a client names it (§7.4). supportedlock/lockdiscovery are
// absent because LOCK is not live in this build — an empty supportedlock would
// read as "supports no locks" while the DAV header says 1.
var (
	allPropFile = []string{
		"D:resourcetype", "D:displayname", "D:getcontentlength", "D:getlastmodified",
		"D:creationdate", "D:getcontenttype", "D:getetag", "b:rev", "b:tree",
	}
	allPropCollection = []string{
		"D:resourcetype", "D:displayname", "D:getlastmodified", "D:creationdate",
		"b:rev", "b:tree",
	}
)

type propfindMode int

const (
	pfModeAllProp propfindMode = iota
	pfModePropName
	pfModeProp
)

// XML document shapes. The D:/b:/eN: prefixes are literal element names with
// the namespaces declared once on the root — the shape the spec's examples
// fix — so the wire output is byte-predictable for A-3's golden comparison.

type multiStatusDoc struct {
	XMLName   xml.Name       `xml:"D:multistatus"`
	Attrs     []xml.Attr     `xml:",any,attr"`
	Responses []responseElem `xml:"D:response"`
}

type responseElem struct {
	Href  string         `xml:"D:href"`
	Stats []propstatElem `xml:"D:propstat"`
}

type propstatElem struct {
	Props  propsElem      `xml:"D:prop"`
	Status string         `xml:"D:status"`
	Err    *statusErrElem `xml:"D:error,omitempty"`
}

type propsElem struct {
	Items []propItem `xml:",any"`
}

type propItem struct {
	XMLName xml.Name
	Inner   string `xml:",innerxml"`
}

type statusErrElem struct {
	Inner string `xml:",innerxml"`
}

type propfindRequest struct {
	XMLName  xml.Name       `xml:"propfind"`
	Prop     *propContainer `xml:"prop"`
	AllProp  *struct{}      `xml:"allprop"`
	PropName *struct{}      `xml:"propname"`
	Include  *propContainer `xml:"include"`
}

type propContainer struct {
	Props []rawProp `xml:",any"`
}

type rawProp struct {
	XMLName xml.Name
}

type propertyUpdate struct {
	XMLName xml.Name  `xml:"propertyupdate"`
	Sets    []propSet `xml:"set"`
	Removes []propSet `xml:"remove"`
}

type propSet struct {
	Prop propContainer `xml:"prop"`
}

// handlePropfind implements RFC 4918 §9.1 with the spec's two refusals:
// Depth: infinity is 403 (the RFC's own permitted refusal, with its own
// precondition code) and a missing Depth is 400 rather than a derived default,
// because the RFC's default IS the refused infinity.
func (h *Handler) handlePropfind(w http.ResponseWriter, r *http.Request) {
	abs, f := h.resolveOrFail(w, r)
	if f != nil {
		h.fail(w, r, *f)
		return
	}
	depth := strings.TrimSpace(r.Header.Get("Depth"))
	switch {
	case depth == "":
		h.fail(w, r, failure{Status: 400, Code: VerdictDepthRequired, Element: "depth-required",
			Pairs: [][2]string{{"detail", "PROPFIND requires a Depth header: 0 or 1"}}})
		return
	case strings.EqualFold(depth, "infinity"):
		h.fail(w, r, failure{
			Status:    403,
			Code:      VerdictPropfindFiniteDepth,
			Element:   "propfind-finite-depth",
			ElementNS: NSDav,
			Pairs:     [][2]string{{"detail", "whole-tree walks are refused; recurse with Depth: 1 or use X-Bunker-Op: snapshot"}},
		})
		return
	case depth != "0" && depth != "1":
		h.fail(w, r, failure{Status: 400, Code: VerdictInvalidDepth, Element: "invalid-depth"})
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		h.fail(w, r, notFoundOrInternal(err))
		return
	}
	mode, requested, f := parsePropfind(r)
	if f != nil {
		h.fail(w, r, *f)
		return
	}

	reg := newNSRegistry()
	doc := multiStatusDoc{}
	doc.Responses = append(doc.Responses, h.responseFor(abs, r.URL.Path, fi, mode, requested, reg))
	if depth == "1" && fi.IsDir() {
		entries, err := os.ReadDir(abs)
		if err != nil {
			h.fail(w, r, notFoundOrInternal(err))
			return
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			// A staging file from a concurrent atomic write is not part of
			// the tree a client may see (AC-6's no-partial-file rule).
			if isTempName(e.Name()) {
				continue
			}
			childInfo, err := e.Info()
			if err != nil {
				continue
			}
			childPath := strings.TrimSuffix(r.URL.Path, "/") + "/" + e.Name()
			doc.Responses = append(doc.Responses,
				h.responseFor(filepath.Join(abs, e.Name()), childPath, childInfo, mode, requested, reg))
		}
	}
	doc.Attrs = reg.attrs()
	body := []byte(xmlDecl + mustXML(doc))
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	// The member list is never truncated: a truncated 207 is a false answer
	// (§8.4, A-11), so there is no size cap here.
	w.WriteHeader(207)
	_, _ = w.Write(body)
}

// parsePropfind decodes the request body. An empty body is the allprop request
// and MUST be accepted (RFC 4918 §9.1): every named consumer drives PROPFIND
// with no body (PROTO-010 §8).
func parsePropfind(r *http.Request) (propfindMode, []xml.Name, *failure) {
	body, f := readAllLimited(r, 1<<20)
	if f != nil {
		return 0, nil, f
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return pfModeAllProp, nil, nil
	}
	var req propfindRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		return 0, nil, &failure{Status: 400, Code: VerdictBadArguments, Element: "bad-arguments",
			Pairs: [][2]string{{"detail", "body is not a DAV:propfind element"}}}
	}
	switch {
	case req.AllProp != nil:
		var names []xml.Name
		if req.Include != nil {
			names = namesOf(req.Include)
		}
		return pfModeAllProp, names, nil
	case req.PropName != nil:
		return pfModePropName, nil, nil
	case req.Prop != nil:
		names := namesOf(req.Prop)
		if len(names) == 0 {
			return 0, nil, &failure{Status: 400, Code: VerdictBadArguments, Element: "bad-arguments",
				Pairs: [][2]string{{"detail", "DAV:prop names no properties"}}}
		}
		return pfModeProp, names, nil
	}
	return 0, nil, &failure{Status: 400, Code: VerdictBadArguments, Element: "bad-arguments",
		Pairs: [][2]string{{"detail", "expected one of DAV:prop, DAV:allprop or DAV:propname"}}}
}

func namesOf(c *propContainer) []xml.Name {
	names := make([]xml.Name, 0, len(c.Props))
	for _, p := range c.Props {
		names = append(names, p.XMLName)
	}
	return names
}

// responseFor renders one D:response: the properties the resource has (200)
// and the ones it does not (404), each in its own propstat. A per-property 404
// is not an error for the request (RFC 4918 §9.1.2).
func (h *Handler) responseFor(abs, urlPath string, fi os.FileInfo, mode propfindMode, requested []xml.Name, reg *nsRegistry) responseElem {
	resp := responseElem{Href: hrefFor(urlPath, fi.IsDir())}
	if mode == pfModePropName {
		names := allPropNames(fi.IsDir())
		items := make([]propItem, 0, len(names))
		for _, n := range names {
			items = append(items, propItem{XMLName: xml.Name{Local: n}})
		}
		resp.Stats = append(resp.Stats, propstatElem{Props: propsElem{Items: items}, Status: statusLine(200)})
		return resp
	}

	list := requested
	if mode == pfModeAllProp {
		list = make([]xml.Name, 0, len(allPropNames(fi.IsDir())))
		for _, n := range allPropNames(fi.IsDir()) {
			// The allprop set is written prefix-form ("D:getetag", "b:rev");
			// map each back to the namespace so the response re-renders the
			// same name it advertises.
			prefix, local, _ := strings.Cut(n, ":")
			space := NSBunker
			if prefix == "D" {
				space = NSDav
			}
			list = append(list, xml.Name{Space: space, Local: local})
		}
		// Explicit <include> names ride on top of allprop (§7.4).
		list = append(list, requested...)
	}

	values := h.liveValues(abs, fi)
	var present, missing []propItem
	seen := make(map[string]bool, len(list))
	for _, n := range list {
		key := reg.qualifiedName(n)
		if seen[key] {
			continue
		}
		seen[key] = true
		if inner, ok := values[key]; ok {
			present = append(present, propItem{XMLName: xml.Name{Local: key}, Inner: inner})
			continue
		}
		missing = append(missing, propItem{XMLName: xml.Name{Local: key}})
	}
	if len(present) > 0 {
		resp.Stats = append(resp.Stats, propstatElem{Props: propsElem{Items: present}, Status: statusLine(200)})
	}
	if len(missing) > 0 {
		resp.Stats = append(resp.Stats, propstatElem{Props: propsElem{Items: missing}, Status: statusLine(404)})
	}
	return resp
}

func allPropNames(isDir bool) []string {
	if isDir {
		return allPropCollection
	}
	return allPropFile
}

// liveValues computes the live properties of one resource, keyed by the name
// the response uses. Values are already XML-escaped inner markup.
func (h *Handler) liveValues(abs string, fi os.FileInfo) map[string]string {
	v := make(map[string]string, len(allPropFile))
	isDir := fi.IsDir()
	if isDir {
		v["D:resourcetype"] = "<D:collection></D:collection>"
	} else {
		v["D:resourcetype"] = ""
	}
	v["D:displayname"] = escapeXML(filepath.Base(abs))
	v["D:getlastmodified"] = fi.ModTime().UTC().Format(http.TimeFormat)
	v["D:creationdate"] = fi.ModTime().UTC().Format("2006-01-02T15:04:05Z")
	v["b:rev"] = escapeXML(h.tree.revToken())
	v["b:tree"] = escapeXML(h.tree.identity())
	if isDir {
		return v
	}
	v["D:getcontentlength"] = strconv.FormatInt(fi.Size(), 10)
	v["D:getcontenttype"] = escapeXML(contentTypeFor(abs))
	if hash, err := h.tree.hashFile(abs); err == nil {
		v["D:getetag"] = escapeXML(etagFor(hash))
		v["b:hash"] = escapeXML(hash)
	}
	return v
}

// handleProppatch implements RFC 4918 §9.2 as a METHOD (a hard MUST) while
// refusing every property: live/computed properties are protected, and v1 has
// no dead-property store (declared deviation 2). The refusal is per property
// inside a 207, and the instruction that fails carries its own diagnosis while
// the remaining instructions of the same atomic group answer 424 — the shape
// §10.3 fixes.
func (h *Handler) handleProppatch(w http.ResponseWriter, r *http.Request) {
	abs, f := h.resolveOrFail(w, r)
	if f != nil {
		h.fail(w, r, *f)
		return
	}
	body, f := readAllLimited(r, 1<<20)
	if f != nil {
		h.fail(w, r, *f)
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		h.fail(w, r, failure{Status: 400, Code: VerdictBadArguments, Element: "bad-arguments",
			Pairs: [][2]string{{"detail", "PROPPATCH requires a DAV:propertyupdate body"}}})
		return
	}
	var pu propertyUpdate
	if err := xml.Unmarshal(body, &pu); err != nil {
		h.fail(w, r, failure{Status: 400, Code: VerdictBadArguments, Element: "bad-arguments",
			Pairs: [][2]string{{"detail", "body is not a DAV:propertyupdate element"}}})
		return
	}
	type instruction struct {
		name   xml.Name
		remove bool
	}
	var instructions []instruction
	for _, s := range pu.Sets {
		for _, p := range s.Prop.Props {
			instructions = append(instructions, instruction{name: p.XMLName})
		}
	}
	for _, s := range pu.Removes {
		for _, p := range s.Prop.Props {
			instructions = append(instructions, instruction{name: p.XMLName, remove: true})
		}
	}
	if len(instructions) == 0 {
		h.fail(w, r, failure{Status: 400, Code: VerdictBadArguments, Element: "bad-arguments",
			Pairs: [][2]string{{"detail", "DAV:propertyupdate carries no DAV:set or DAV:remove instructions"}}})
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		h.fail(w, r, notFoundOrInternal(err))
		return
	}

	reg := newNSRegistry()
	resp := responseElem{Href: hrefFor(r.URL.Path, fi.IsDir())}
	verdict := VerdictOK
	for i, in := range instructions {
		key := reg.qualifiedName(in.name)
		item := propItem{XMLName: xml.Name{Local: key}}
		// v1 refuses EVERY instruction — no property of this surface is
		// writable — so the set/remove interleaving cannot change which
		// properties refuse; only the first refusal needs to name its cause.
		if i > 0 {
			resp.Stats = append(resp.Stats, propstatElem{Props: propsElem{Items: []propItem{item}}, Status: statusLine(424)})
			continue
		}
		if in.name.Space == NSDav || in.name.Space == NSBunker || in.name.Space == "" {
			verdict = VerdictProtectedProperty
			resp.Stats = append(resp.Stats, propstatElem{
				Props:  propsElem{Items: []propItem{item}},
				Status: statusLine(403),
				Err:    &statusErrElem{Inner: "<D:cannot-modify-protected-property></D:cannot-modify-protected-property>"},
			})
			continue
		}
		verdict = VerdictDeadPropertiesUnsupported
		resp.Stats = append(resp.Stats, propstatElem{
			Props:  propsElem{Items: []propItem{item}},
			Status: statusLine(403),
			Err:    &statusErrElem{Inner: "<b:dead-properties-unsupported></b:dead-properties-unsupported>"},
		})
	}
	doc := multiStatusDoc{Responses: []responseElem{resp}}
	doc.Attrs = reg.attrs()
	out := []byte(xmlDecl + mustXML(doc))
	w.Header().Set("X-Bunker-Verdict", string(verdict))
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(207)
	_, _ = w.Write(out)
}

// hrefFor renders the D:href of a resource: the request path, escaped, with a
// trailing slash on collections (RFC 4918 §8.3).
func hrefFor(urlPath string, isDir bool) string {
	if urlPath == "" || urlPath == "/" {
		urlPath = Prefix + "/"
	}
	if !strings.HasPrefix(urlPath, Prefix) {
		urlPath = Prefix + "/" + strings.TrimPrefix(urlPath, "/")
	}
	if isDir && !strings.HasSuffix(urlPath, "/") {
		urlPath += "/"
	}
	return (&url.URL{Path: urlPath}).EscapedPath()
}

// readAllLimited reads a metadata body under a fixed cap.
func readAllLimited(r *http.Request, limit int64) ([]byte, *failure) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := readLimited(r.Body, limit)
	if err != nil {
		return nil, &failure{Status: 400, Code: VerdictBadArguments, Element: "bad-arguments",
			Pairs: [][2]string{{"detail", "request body could not be read"}}}
	}
	return body, nil
}

// mustXML marshals a response document. Marshal fails only on a shape this
// package cannot construct (an unsupported field type), so the empty fallback
// is unreachable rather than ignored; the session's tests assert the wire
// bytes.
func mustXML(v any) string {
	b, err := xml.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
