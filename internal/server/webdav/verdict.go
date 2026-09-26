package webdav

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Verdict is the machine-readable outcome code the surface puts in the
// X-Bunker-Verdict response header AND in the response body (§5.1, §5.3).
// Successes carry one too ("ok", "identical_content"), so a consumer has a
// single extraction point for every outcome.
type Verdict string

// The verdict vocabulary of §5.1, plus four codes this implementation needs
// and the spec implies but does not tabulate: the RFC-plain 409/403 families
// (`conflict`, `forbidden`, `not_found`) and the §6.1 step-1 confinement
// refusal (`workspace_invalid`). They are named in the code, in the
// capability document's `degradations`-adjacent prose, and in the row report.
const (
	VerdictOK                        Verdict = "ok"
	VerdictMethodUnknown             Verdict = "method_unknown"
	VerdictMethodNotAllowed          Verdict = "method_not_allowed"
	VerdictNotImplementedYet         Verdict = "not_implemented_yet"
	VerdictCapabilityUnavailable     Verdict = "capability_unavailable"
	VerdictPreconditionFailed        Verdict = "precondition_failed"
	VerdictHashMismatch              Verdict = "hash_mismatch"
	VerdictIdenticalContent          Verdict = "identical_content"
	VerdictBodyHashMismatch          Verdict = "body_hash_mismatch"
	VerdictStaleTree                 Verdict = "stale_tree"
	VerdictNotARepo                  Verdict = "not_a_repo"
	VerdictProtectedProperty         Verdict = "protected_property"
	VerdictDeadPropertiesUnsupported Verdict = "dead_properties_unsupported"
	VerdictDepthRequired             Verdict = "depth_required"
	VerdictInvalidDepth              Verdict = "invalid_depth"
	VerdictPropfindFiniteDepth       Verdict = "propfind_finite_depth"
	VerdictUnsupportedMediaType      Verdict = "unsupported_media_type"
	VerdictResultTooLarge            Verdict = "result_too_large"
	VerdictInsufficientStorage       Verdict = "insufficient_storage"
	VerdictWorkspaceInvalid          Verdict = "workspace_invalid"
	VerdictExtensionOpMissing        Verdict = "extension_op_missing"
	VerdictOpUnknown                 Verdict = "op_unknown"
	VerdictBadArguments              Verdict = "bad_arguments"
	VerdictUnauthenticated           Verdict = "unauthenticated"
	VerdictPayloadTooLarge           Verdict = "payload_too_large"
	VerdictRangeNotSatisfiable       Verdict = "range_not_satisfiable"
	VerdictInternal                  Verdict = "internal"
	VerdictNotFound                  Verdict = "not_found"
	VerdictForbidden                 Verdict = "forbidden"
	VerdictConflict                  Verdict = "conflict"
	VerdictBadGateway                Verdict = "bad_gateway"
)

// failure is one refusal in the shape §5.3 requires: a standard HTTP status, a
// machine code carried in the header and the body, and — where the refusal has
// detail to convey — the RFC's own DAV:error vehicle with a urn:bunker:fs:1
// child element.
type failure struct {
	Status int
	Code   Verdict
	// Capability/Scope/Mode/Phase feed X-Bunker-Capability (and the E-4
	// envelope's error object) when the refusal is about a capability.
	Capability string
	Scope      string
	Mode       string
	Phase      string
	// Headers are extra response headers, e.g. both hashes on a 412.
	Headers map[string]string
	// Element is the element carried inside DAV:error. An empty Element means
	// "no XML body" (a status only).
	Element string
	// ElementNS is the namespace of Element: NSBunker by default, or NSDav
	// for the refusals RFC 4918 gives its own precondition element
	// (propfind-finite-depth, cannot-modify-protected-property).
	ElementNS string
	// Pairs are the child elements of Element, in document order.
	Pairs [][2]string
}

// capabilityHeaderValue renders the X-Bunker-Capability value the spec's §10.5
// example fixes: "<name>;scope=<scope>[;phase=<phase>][;mode=<mode>]".
func capabilityHeaderValue(capability, scope, phase, mode string) string {
	if capability == "" {
		return ""
	}
	parts := []string{capability, "scope=" + scope}
	if phase != "" {
		parts = append(parts, "phase="+phase)
	}
	if mode != "" {
		parts = append(parts, "mode="+mode)
	}
	return strings.Join(parts, ";")
}

// davErrorDoc is the DAV:error document of §5.3/§10.3.
type davErrorDoc struct {
	XMLName xml.Name   `xml:"D:error"`
	Attrs   []xml.Attr `xml:",any,attr"`
	Inner   string     `xml:",innerxml"`
}

// fail writes a refusal. Every refusal: overwrites the verdict header, is
// marked non-cacheable (§6.1 item 4), names the real verb set when the status
// is 405 (RFC 9110 §15.5.6 requires Allow there), and carries the code in the
// body as well as the header.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, f failure) {
	w.Header().Set("X-Bunker-Verdict", string(f.Code))
	w.Header().Set("Cache-Control", "no-store")
	if f.Status == http.StatusMethodNotAllowed || f.Status == http.StatusNotImplemented {
		w.Header().Set("Allow", strings.Join(servedMethods, ", "))
	}
	for k, v := range f.Headers {
		w.Header().Set(k, v)
	}
	if v := capabilityHeaderValue(f.Capability, f.Scope, f.Phase, f.Mode); v != "" {
		w.Header().Set("X-Bunker-Capability", v)
	}

	var body []byte
	switch {
	case f.Element != "":
		body = []byte(xmlDecl + mustXML(davErrorDocument(f)))
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	case f.Status == http.StatusUnauthorized:
		body = []byte("401 unauthorized\n")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	if body != nil {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(f.Status)
	// HEAD responses carry the same headers and never a body.
	if r.Method != http.MethodHead && len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func davErrorDocument(f failure) davErrorDoc {
	prefix := "b"
	if f.ElementNS == NSDav {
		prefix = "D"
	}
	sb := &strings.Builder{}
	fmt.Fprintf(sb, "<%s:%s>", prefix, f.Element)
	for _, p := range f.Pairs {
		fmt.Fprintf(sb, "<b:%s>%s</b:%s>", p[0], escapeXML(p[1]), p[0])
	}
	fmt.Fprintf(sb, "</%s:%s>", prefix, f.Element)
	return davErrorDoc{
		Attrs: []xml.Attr{
			{Name: xml.Name{Local: "xmlns:D"}, Value: NSDav},
			{Name: xml.Name{Local: "xmlns:b"}, Value: NSBunker},
		},
		Inner: sb.String(),
	}
}

// escapeXML escapes a value for inclusion in element text.
func escapeXML(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// statusLine renders the RFC 4918 status-line string used inside multistatus
// and propstat bodies. The literal "HTTP/1.1" is what the RFC defines the
// element's value to be, on every transport — the negotiated version travels
// in X-Bunker-Proto (E-3), not here.
func statusLine(code int) string {
	return fmt.Sprintf("HTTP/1.1 %d %s", code, http.StatusText(code))
}
