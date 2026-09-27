package fsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Catalogue is E-4's op list, verbatim from BFS-004 §3 E-4 — including
// `capabilities` and `watch`, which the handshake itself depends on. A name that
// is not in this list is a 400 op_unknown; a name in the list that the running
// build does not serve answers a structured 501 capability_unavailable naming
// the slice (BFS-004 R3). Nothing here is a second vocabulary.
var Catalogue = []string{
	"capabilities", "status", "diff", "rev-parse", "ls-files", "log", "snapshot", "events", "watch",
}

// DelegatedOps is the subset of the catalogue that computes an answer on the
// agent: the "second surface" of BFS-005 §6.4. These are NOT routed through the
// mount — a FUSE filesystem is handed one syscall at a time and cannot make
// git's walk one server call (BFS-003 §3(d), BFS-005 §6.4). They are invoked by
// this client's own verbs (`bunker fs status`, `bunker fs diff`, …).
var DelegatedOps = []string{"status", "diff", "rev-parse", "ls-files", "log", "snapshot", "events"}

// Envelope is the single JSON shape every E-4 response uses, success or failure
// (BFS-004 §10.4), so a consumer has exactly one decoder.
type Envelope struct {
	OK         bool            `json:"ok"`
	Op         string          `json:"op"`
	Verdict    string          `json:"verdict"`
	Rev        string          `json:"rev"`
	Tree       string          `json:"tree"`
	Proto      string          `json:"proto"`
	DurationMS int64           `json:"duration_ms"`
	Truncated  bool            `json:"truncated"`
	Result     json.RawMessage `json:"result"`
	Error      *EnvelopeError  `json:"error"`
	// Status is the HTTP status the envelope arrived with; Raw is the undecoded
	// body, so a caller can show the server's own answer verbatim.
	Status int             `json:"-"`
	Raw    json.RawMessage `json:"-"`
}

// EnvelopeError is the structured refusal inside the envelope (§10.4). `scope`
// is the field that separates "not supported on this target" from "not in this
// build yet" (§5.2), and `mode` names the mode actually in force when the
// server degrades.
type EnvelopeError struct {
	Capability string `json:"capability,omitempty"`
	Scope      string `json:"scope,omitempty"`
	Phase      string `json:"phase,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// Op performs one delegated operation: the op in the `X-Bunker-Op` header, its
// arguments FLAT in the JSON body (§3 E-4, §6.1 F-3). It is the second surface's
// only transport, and it never carries a shell command string — the op maps to a
// fixed server-side structure, so the surface cannot be turned into execution.
//
// decodeInto, when non-nil, receives the envelope's `result`. A structured
// refusal is returned as an *OpError whose Verdict is the server's code and
// whose Capability/Scope/Phase/Mode are the server's own fields.
func (c *Client) Op(ctx context.Context, op string, args any, decodeInto any) (*Envelope, *OpError) {
	if !inCatalogue(op) {
		return nil, &OpError{
			Op: "op", Errno: ErrnoEINVAL, Cause: CauseLocalCapability, Verdict: VerdictOpUnknown,
			Detail: fmt.Sprintf("op %q is not in the E-4 catalogue (%s); the client never sends a name the server does not publish", op, strings.Join(Catalogue, ", ")),
		}
	}
	var body io.Reader
	if args != nil {
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, &OpError{Op: "op:" + op, Errno: ErrnoEINVAL, Cause: CauseServerError, Err: err}
		}
		body = bytes.NewReader(raw)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := c.newRequest(ctx, http.MethodPost, "", body)
	if err != nil {
		return nil, &OpError{Op: "op:" + op, Errno: ErrnoEIO, Cause: CauseServerError, Err: err}
	}
	req.Header.Set("X-Bunker-Op", op)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.opt.MaxResultBytes > 0 {
		req.Header.Set("X-Bunker-Max-Bytes", strconv.FormatInt(c.opt.MaxResultBytes, 10))
	}
	resp, oerr := c.do(ctx, req)
	if oerr != nil {
		oerr.Op = "op:" + op
		return nil, oerr
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		// A cancelled read is a cancellation, not a transport fault: the caller
		// gets EINTR and retries the same op (BFS-039).
		return nil, classifyRequest(ctx, err, "op:"+op, "")
	}
	env := &Envelope{Status: resp.StatusCode, Raw: raw}
	if err := json.Unmarshal(raw, env); err != nil {
		return nil, &OpError{
			Op: "op:" + op, Errno: ErrnoEIO, Cause: CauseServerError, Status: resp.StatusCode,
			Verdict: strings.TrimSpace(resp.Header.Get("X-Bunker-Verdict")),
			Detail:  "response body is not an E-4 envelope",
			Err:     err,
		}
	}
	env.Rev = firstNonEmpty(env.Rev, resp.Header.Get("X-Bunker-Rev"))
	env.Tree = firstNonEmpty(env.Tree, resp.Header.Get("X-Bunker-Tree"))
	env.Proto = firstNonEmpty(env.Proto, resp.Header.Get("X-Bunker-Proto"))

	if resp.StatusCode >= 400 || !env.OK {
		e := &OpError{
			Op: "op:" + op, Status: resp.StatusCode, Cause: CauseServerError, Errno: ErrnoEIO,
			Verdict: firstNonEmpty(env.Verdict, strings.TrimSpace(resp.Header.Get("X-Bunker-Verdict"))),
		}
		if env.Error != nil {
			e.Capability, e.Scope, e.Phase, e.Mode, e.Detail = env.Error.Capability, env.Error.Scope, env.Error.Phase, env.Error.Mode, env.Error.Detail
		}
		if e.Verdict == VerdictCapabilityUnavailable || resp.StatusCode == http.StatusNotImplemented {
			e.Errno, e.Cause = ErrnoEOPNOTSUPP, CauseServerError
			if e.Detail == "" {
				e.Detail = "the running build does not serve this op"
			}
		}
		return env, e
	}
	if decodeInto != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, decodeInto); err != nil {
			return env, &OpError{Op: "op:" + op, Status: 200, Errno: ErrnoEIO, Cause: CauseServerError,
				Detail: "envelope result did not match the caller's shape", Err: err}
		}
	}
	return env, nil
}

// inCatalogue reports whether op is published by the surface spec.
func inCatalogue(op string) bool {
	for _, o := range Catalogue {
		if o == op {
			return true
		}
	}
	return false
}

// DelegatedResult is one delegated call's outcome, kept in the shape the CLI and
// the measurements both consume: the verbatim envelope plus the wall clock this
// client observed (the envelope carries the server's own compute time, and the
// difference between the two is the round trip the design is priced on).
type DelegatedResult struct {
	Op       string
	OK       bool
	Verdict  string
	Status   int
	Duration time.Duration
	Envelope *Envelope
	Err      *OpError
	// Text is the server's answer rendered for a terminal, when the op returned
	// a text payload (git output). Kept verbatim.
	Text string
}

// Delegate runs one op and renders its answer as text where the payload is a
// string list or a string field — the shape the delegated git ops return.
func (c *Client) Delegate(ctx context.Context, op string, args map[string]any) *DelegatedResult {
	start := timeNow()
	var out map[string]json.RawMessage
	env, err := c.Op(ctx, op, args, &out)
	res := &DelegatedResult{Op: op, Duration: timeSince(start), Envelope: env, Err: err}
	if err != nil {
		res.Status = err.Status
		res.Verdict = err.Verdict
		return res
	}
	res.OK, res.Verdict, res.Status = true, env.Verdict, env.Status
	res.Text = renderResult(out)
	return res
}

// renderResult flattens a delegated result into terminal text without inventing
// structure: a "stdout" field is printed verbatim, a list of objects is printed
// as JSON lines, and anything else is printed as JSON.
func renderResult(out map[string]json.RawMessage) string {
	if raw, ok := out["stdout"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	if len(out) == 0 {
		return ""
	}
	for _, key := range []string{"result", "lines", "entries", "paths", "data"} {
		if raw, ok := out[key]; ok {
			var lines []string
			if json.Unmarshal(raw, &lines) == nil {
				return strings.Join(lines, "\n")
			}
			return string(raw)
		}
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}
