// This file implements DF-BUNKER-22: a REST client that calls a
// server-streaming RPC with the unary JSON media type documented in
// docs/integration.md §2 used to get a bare `415 Unsupported Media Type` with
// `Content-Length: 0` — connect-go rejects the media type inside its own
// ServeHTTP, before any bunker code runs, so the integrator saw neither the
// documented {"code","message"} envelope nor a hint about the streaming media
// type Connect requires.
//
// The wrapper below is deliberately narrow: it only substitutes a body when
// connect itself answered 415 on a server-streaming procedure path and wrote
// no body. Every other exchange — unary REST, connect+json streaming, gRPC,
// auth failures, unknown paths — is passed through untouched.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// streamingExpectedMediaType is the media type a REST client must use to reach
// a server-streaming RPC over Connect (docs/integration.md §5).
const streamingExpectedMediaType = "application/connect+json"

// errorEnvelope is the JSON error shape documented in docs/integration.md §2:
// the connect code as a string plus a human-readable message.
type errorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// serverStreamingProcedures returns the fully-qualified HTTP paths of every
// server-streaming RPC in the Bunkerd proto (currently
// "/bunker.v1.Bunkerd/ExecAgent"). The list is derived from the compiled file
// descriptor so it tracks proto/bunker/v1/bunker.proto instead of a
// hand-maintained table that drifts the moment a streaming RPC is added.
func serverStreamingProcedures() map[string]struct{} {
	out := make(map[string]struct{})
	services := v1.File_proto_bunker_v1_bunker_proto.Services()
	for i := 0; i < services.Len(); i++ {
		svc := services.Get(i)
		methods := svc.Methods()
		for j := 0; j < methods.Len(); j++ {
			m := methods.Get(j)
			if !m.IsStreamingServer() {
				continue
			}
			out["/"+string(svc.FullName())+"/"+string(m.Name())] = struct{}{}
		}
	}
	return out
}

// connectStreamingEnvelope wraps a connect-go handler so that a streaming RPC
// reached with a media type connect refuses answers HTTP 415 with a JSON
// {"code","message"} envelope naming the media type the caller should use,
// instead of an empty body.
//
// Requests to paths that are not server-streaming procedures (and every
// streaming request whose media type connect accepts) are handed to the next
// handler unmodified.
func connectStreamingEnvelope(next http.Handler) http.Handler {
	return &streamingEnvelopeHandler{next: next, streaming: serverStreamingProcedures()}
}

type streamingEnvelopeHandler struct {
	next      http.Handler
	streaming map[string]struct{}
}

func (h *streamingEnvelopeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.streaming[r.URL.Path]; !ok {
		h.next.ServeHTTP(w, r)
		return
	}
	ew := &envelopeResponseWriter{
		ResponseWriter: w,
		procedure:      r.URL.Path,
		sentMediaType:  r.Header.Get("Content-Type"),
	}
	h.next.ServeHTTP(ew, r)
	ew.substituteEmpty415()
}

// envelopeResponseWriter holds back a 415 status line until it knows whether
// the wrapped handler is going to write a body for it. It implements
// http.Flusher because connect refuses to serve a server-streaming RPC through
// a ResponseWriter that cannot flush
// (connect/internal protocol.go: checkServerStreamsCanFlush), so a wrapper
// without Flush would turn every streaming call into a CodeInternal error.
type envelopeResponseWriter struct {
	http.ResponseWriter
	procedure     string
	sentMediaType string
	held415       bool // a 415 was reported and is not yet committed downstream
	settled       bool // the response has been committed downstream as-is
}

func (w *envelopeResponseWriter) WriteHeader(code int) {
	if code == http.StatusUnsupportedMediaType && !w.settled {
		// Delay the status line: if nothing is written for it we replace the
		// empty body with the explanatory envelope.
		w.held415 = true
		return
	}
	w.settled = true
	w.held415 = false
	w.ResponseWriter.WriteHeader(code)
}

func (w *envelopeResponseWriter) Write(p []byte) (int, error) {
	if w.held415 {
		// The wrapped handler produced its own 415 body: keep it untouched.
		w.settled = true
		w.held415 = false
		w.ResponseWriter.WriteHeader(http.StatusUnsupportedMediaType)
	}
	w.settled = true
	return w.ResponseWriter.Write(p)
}

// Flush implements http.Flusher. A held 415 that is being flushed with no body
// is materialized as the envelope first, so the client never sees the empty
// 415 this file exists to remove.
func (w *envelopeResponseWriter) Flush() {
	if w.held415 && !w.settled {
		w.substituteEmpty415()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// substituteEmpty415 replaces a body-less 415 with the JSON error envelope. It
// is a no-op when the response is already committed (any other status, or a
// 415 the wrapped handler filled in itself).
func (w *envelopeResponseWriter) substituteEmpty415() {
	if !w.held415 || w.settled {
		return
	}
	w.settled = true
	w.held415 = false

	header := w.Header()
	body, err := json.Marshal(errorEnvelope{
		Code:    "invalid_argument",
		Message: streamingMediaTypeMessage(w.procedure, w.sentMediaType, header.Get("Accept-Post")),
	})
	if err != nil {
		// Cannot happen for this struct; stay honest rather than writing a
		// half-envelope.
		w.ResponseWriter.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	// connect already set Accept-Post naming the media types it does accept —
	// keep it, it is the machine-readable half of this answer.
	header.Set("Content-Type", "application/json")
	w.ResponseWriter.WriteHeader(http.StatusUnsupportedMediaType)
	_, _ = w.ResponseWriter.Write(body)
}

// streamingMediaTypeMessage is the human-readable half of the 415 envelope: it
// names the procedure, echoes what the client sent, explains why
// application/json is the wrong shape for it, and points at the media type
// that works.
func streamingMediaTypeMessage(procedure, sent, acceptPost string) string {
	what := "the request's media type"
	if sent != "" {
		what = fmt.Sprintf("Content-Type %q", sent)
	}
	msg := fmt.Sprintf(
		"%s is a server-streaming RPC, so %s is not accepted: application/json is the unary REST shape "+
			"(one JSON object in, one JSON object out), while a streaming RPC needs Connect envelope framing. "+
			"Retry with Content-Type: %s and one 5-byte-prefixed envelope per message; the response is an "+
			"envelope stream (see docs/integration.md §5).",
		procedure, what, streamingExpectedMediaType,
	)
	if acceptPost != "" {
		msg += " Accepted content types: " + acceptPost + "."
	}
	return msg
}
