// Tests for DF-BUNKER-22: a server-streaming RPC called with the unary REST
// media type must answer 415 with a {"code","message"} envelope, and every
// other path must stay byte-identical to the unwrapped connect handler.
package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

const (
	execAgentPath  = bunkerv1connect.BunkerdExecAgentProcedure  // streaming
	serverInfoPath = bunkerv1connect.BunkerdServerInfoProcedure // unary
	runAgentPath   = bunkerv1connect.BunkerdRunAgentProcedure   // unary
	agentInfoPath  = bunkerv1connect.AgentGetInfoProcedure      // unary
)

// newEnvelopeTestHandlers returns two live HTTP servers over the SAME real
// connect handler: one wrapped by connectStreamingEnvelope (production shape)
// and one raw, so a test can diff the two responses.
func newEnvelopeTestHandlers(t *testing.T) (wrapped *httptest.Server, raw *httptest.Server) {
	t.Helper()
	logger := testDiscardLogger()
	tracker := resource.NewTracker(10, logger)
	svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker}

	path, handler := bunkerv1connect.NewBunkerdHandler(svc)
	agentPath, agentHandler := bunkerv1connect.NewAgentHandler(
		&agentService{logger: logger, tracker: tracker},
	)

	wrappedMux := http.NewServeMux()
	wrappedMux.Handle(path, connectStreamingEnvelope(handler))
	wrappedMux.Handle(agentPath, connectStreamingEnvelope(agentHandler))
	wrapped = httptest.NewServer(wrappedMux)

	rawMux := http.NewServeMux()
	rawMux.Handle(path, handler)
	rawMux.Handle(agentPath, agentHandler)
	raw = httptest.NewServer(rawMux)

	t.Cleanup(func() {
		wrapped.Close()
		raw.Close()
	})
	return wrapped, raw
}

// post issues a POST with an explicit body (possibly empty) and media type.
func post(t *testing.T, url, mediaType, body string) (status int, respBody []byte, header http.Header) {
	t.Helper()
	return postBytes(t, url, mediaType, []byte(body))
}

// postBytes is post for bodies that are not valid UTF-8 text — the trailing
// end-of-stream forms of DF-BUNKER-22 are raw bytes (a lone 0x02, or the 5-byte
// [0x02][0x00000000] envelope).
func postBytes(t *testing.T, url, mediaType string, body []byte) (status int, respBody []byte, header http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw, resp.Header
}

// streamFrame is one decoded connect JSON-stream envelope:
// [flags:1 byte][length:4 bytes big-endian][payload].
type streamFrame struct {
	flags   byte
	payload []byte
}

// endOfStreamFlag marks the trailer flag connect uses for the final frame. The
// RPC error of a JSON stream arrives inside a 200 body as such a frame.
const endOfStreamFlag byte = 0x02

// decodeStreamFrames splits a connect streaming response body into its
// envelopes, failing loudly on a truncated one.
func decodeStreamFrames(t *testing.T, body []byte) []streamFrame {
	t.Helper()
	var frames []streamFrame
	for len(body) > 0 {
		if len(body) < 5 {
			t.Fatalf("truncated frame header: %d trailing byte(s): %q", len(body), body)
		}
		n := int(binary.BigEndian.Uint32(body[1:5]))
		if len(body) < 5+n {
			t.Fatalf("frame declares %d payload byte(s), only %d remain: %q", n, len(body)-5, body)
		}
		frames = append(frames, streamFrame{flags: body[0], payload: body[5 : 5+n]})
		body = body[5+n:]
	}
	return frames
}

// streamError is the RPC error object a connect JSON stream carries in-band.
type streamError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// decodeSoleErrorFrame requires the body to be exactly one end-of-stream frame
// whose payload is an RPC error, and returns that error.
func decodeSoleErrorFrame(t *testing.T, body []byte) streamError {
	t.Helper()
	frames := decodeStreamFrames(t, body)
	if len(frames) != 1 {
		t.Fatalf("want exactly 1 frame, got %d: %q", len(frames), body)
	}
	if frames[0].flags != endOfStreamFlag {
		t.Errorf("frame flags = %#02x, want %#02x (end-of-stream trailer)", frames[0].flags, endOfStreamFlag)
	}
	var env struct {
		Error streamError `json:"error"`
	}
	if err := json.Unmarshal(frames[0].payload, &env); err != nil {
		t.Fatalf("frame payload is not JSON (%v): %s", err, frames[0].payload)
	}
	if env.Error.Code == "" {
		t.Fatalf("frame payload carries no error object: %s", frames[0].payload)
	}
	return env.Error
}

// TestStreamingEnvelope_UnaryJSONOnStreamingRPC is the acceptance case: the
// exact call docs/integration.md §2 prescribes for a streaming RPC.
func TestStreamingEnvelope_UnaryJSONOnStreamingRPC(t *testing.T) {
	t.Parallel()
	wrapped, _ := newEnvelopeTestHandlers(t)

	status, body, header := post(t, wrapped.URL+execAgentPath, "application/json", `{"agent_id":"abc","command":"id"}`)

	if status != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d (body: %s)", status, http.StatusUnsupportedMediaType, body)
	}
	if got := header.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want a JSON media type", got)
	}
	if len(body) == 0 {
		t.Fatal("body is empty: the pre-fix behavior this task removes")
	}

	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not JSON (%v): %s", err, body)
	}
	if env.Code == "" {
		t.Error("envelope code is empty")
	}
	if env.Code != "invalid_argument" {
		t.Errorf("code = %q, want %q", env.Code, "invalid_argument")
	}
	if env.Message == "" {
		t.Fatal("envelope message is empty: it must explain the failure")
	}
	for _, want := range []string{
		execAgentPath,              // names the procedure it refused
		"application/connect+json", // names the media type that works
		"application/json",         // echoes what the client sent
		"unary REST shape",         // says why application/json does not apply
	} {
		if !strings.Contains(env.Message, want) {
			t.Errorf("message %q must mention %q", env.Message, want)
		}
	}
	// connect's own Accept-Post list is the machine-readable half; keep it.
	if accept := header.Get("Accept-Post"); !strings.Contains(accept, "application/connect+json") {
		t.Errorf("Accept-Post = %q, want it to survive from connect", accept)
	}
}

// TestStreamingEnvelope_LeavesOtherPathsAlone diffs the wrapped server against
// the raw connect handler for every exchange that must not change.
func TestStreamingEnvelope_LeavesOtherPathsAlone(t *testing.T) {
	t.Parallel()
	wrapped, raw := newEnvelopeTestHandlers(t)

	// A connect+json streaming request body: one envelope,
	// [flags:1][len:4 BE][protojson payload].
	payload := []byte(`{"agentId":"nope-not-an-agent"}`)
	framed := make([]byte, 0, 5+len(payload))
	framed = append(framed, 0x00)
	framed = binary.BigEndian.AppendUint32(framed, uint32(len(payload)))
	framed = append(framed, payload...)

	cases := []struct {
		name      string
		path      string
		mediaType string
		body      string
	}{
		{"unary_rest_json", serverInfoPath, "application/json", `{}`},
		{"unary_rest_missing_agent", serverInfoPath, "application/json", `bad`},
		{"unary_run_agent_json", runAgentPath, "application/json", `{"agent_id":"nope","command":"id"}`},
		{"agent_service_unary_json", agentInfoPath, "application/json", `{}`},
		{"streaming_connect_json", execAgentPath, "application/connect+json", string(framed)},
		{"unary_bogus_media_type", serverInfoPath, "text/plain", `{}`},
		{"unknown_path", "/bunker.v1.Bunkerd/NoSuchMethod", "application/json", `{}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wStatus, wBody, wHeader := post(t, wrapped.URL+c.path, c.mediaType, c.body)
			rStatus, rBody, rHeader := post(t, raw.URL+c.path, c.mediaType, c.body)

			if wStatus != rStatus {
				t.Errorf("status = %d, raw = %d", wStatus, rStatus)
			}
			if !bytes.Equal(wBody, rBody) {
				t.Errorf("body differs:\n wrapped: %s\n raw:     %s", wBody, rBody)
			}
			if got, want := wHeader.Get("Content-Type"), rHeader.Get("Content-Type"); got != want {
				t.Errorf("Content-Type = %q, raw = %q", got, want)
			}
		})
	}
}

// TestStreamingEnvelope_StreamingStillWorks proves the wrapper did not break
// the streaming path: connect refuses to serve a server-streaming RPC through a
// ResponseWriter without http.Flusher, so a regression here shows up as a
// CodeInternal "%T does not implement http.Flusher" instead of the domain error.
func TestStreamingEnvelope_StreamingStillWorks(t *testing.T) {
	t.Parallel()
	wrapped, _ := newEnvelopeTestHandlers(t)

	client := bunkerv1connect.NewBunkerdClient(wrapped.Client(), wrapped.URL)
	stream, err := client.ExecAgent(context.Background(), connect.NewRequest(&v1.ExecAgentRequest{
		AgentId: "nope-not-an-agent",
		Command: "id",
	}))
	if err != nil {
		t.Fatalf("ExecAgent call: %v", err)
	}
	for stream.Receive() {
		_ = stream.Msg()
	}
	serr := stream.Err()
	if serr == nil {
		t.Fatal("exec against an unknown agent must fail, got a clean stream")
	}
	if strings.Contains(serr.Error(), "http.Flusher") {
		t.Fatalf("streaming was broken by the wrapper: %v", serr)
	}
	if got := connect.CodeOf(serr); got != connect.CodeNotFound {
		t.Errorf("code = %v, want %v (err: %v)", got, connect.CodeNotFound, serr)
	}
}

// TestStreamingEnvelope_RejectedEndOfStreamForms pins the end-of-stream table
// documented in docs/integration.md §5 "ExecAgent over REST — the streaming
// recipe" and docs/dogfood/2026-09-18-integration.md §4.2: the request body must
// be ended by Content-Length alone, and both spec-shaped endings are rejected —
// the rejection arrives as an HTTP 200 whose only frame is an in-band RPC error,
// so a status-code-only client reads it as success. All three rows of that table
// are pinned here, so the day the daemon changes its mind the failure is a test
// failure and not a doc drift.
func TestStreamingEnvelope_RejectedEndOfStreamForms(t *testing.T) {
	t.Parallel()
	wrapped, raw := newEnvelopeTestHandlers(t)

	// A valid connect+json streaming request: one envelope,
	// [flags:1][len:4 BE][protojson payload]. The agent does not exist, so an
	// accepted request always fails in the domain layer — which is exactly what
	// makes the third case below a proof that the RPC was reached.
	payload := []byte(`{"agentId":"nope-not-an-agent","command":"id"}`)
	framed := make([]byte, 0, 5+len(payload))
	framed = append(framed, 0x00)
	framed = binary.BigEndian.AppendUint32(framed, uint32(len(payload)))
	framed = append(framed, payload...)

	const (
		// The two markers the accepted form must NOT produce.
		rejectedProtocolErr = "protocol error: incomplete envelope: unexpected EOF"
		rejectedUnmarshal   = "unmarshal end stream message: unexpected end of JSON input"
	)

	cases := []struct {
		name string
		tail []byte
		// wantCode/wantMessage are the documented in-band error, pinned verbatim:
		// a reworded message or a re-coded failure is a deliberate behaviour
		// change and must break this test, not slip through it.
		wantCode    string
		wantMessage string
		// wantContains are substrings of an accepted (non-protocol) failure.
		wantContains []string
		// rejected: the request died in the protocol layer, so the accepted-form
		// markers must be absent.
		rejected bool
	}{
		{
			name:     "no_end_of_stream_envelope",
			tail:     nil,
			wantCode: "not_found",
			wantContains: []string{
				`"nope-not-an-agent"`, // proves the RPC ran with our agent id
				"not found",
			},
			rejected: false,
		},
		{
			name:        "trailing_0x02_byte",
			tail:        []byte{0x02},
			wantCode:    "invalid_argument",
			wantMessage: rejectedProtocolErr,
			rejected:    true,
		},
		{
			name:        "trailing_empty_trailer_envelope",
			tail:        []byte{0x02, 0x00, 0x00, 0x00, 0x00},
			wantCode:    "internal",
			wantMessage: rejectedUnmarshal,
			rejected:    true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			body := append(append([]byte{}, framed...), c.tail...)
			const mediaType = "application/connect+json"

			wStatus, wBody, _ := postBytes(t, wrapped.URL+execAgentPath, mediaType, body)

			// The documented shape: not an HTTP error, ever.
			if wStatus != http.StatusOK {
				t.Fatalf("status = %d, want 200 (the rejection is carried in the stream); body: %q", wStatus, wBody)
			}

			// The wrapper is 415-only and must not be what rejects the trailer.
			rStatus, rBody, _ := postBytes(t, raw.URL+execAgentPath, mediaType, body)
			if rStatus != wStatus {
				t.Errorf("raw handler status = %d, wrapped = %d", rStatus, wStatus)
			}
			if !bytes.Equal(wBody, rBody) {
				t.Errorf("the envelope wrapper must pass this exchange through untouched:\n wrapped: %q\n raw:     %q", wBody, rBody)
			}

			streamText := string(wBody)
			errObj := decodeSoleErrorFrame(t, wBody)

			if errObj.Code != c.wantCode {
				t.Errorf("in-stream error code = %q, want %q (message: %s)", errObj.Code, c.wantCode, errObj.Message)
			}
			if c.wantMessage != "" && errObj.Message != c.wantMessage {
				t.Errorf("in-stream error message = %q, want %q", errObj.Message, c.wantMessage)
			}
			for _, want := range c.wantContains {
				if !strings.Contains(errObj.Message, want) {
					t.Errorf("in-stream error message %q must contain %q", errObj.Message, want)
				}
			}

			if c.rejected {
				// Both rejected rows are protocol failures: the command never ran.
				if !strings.Contains(streamText, "protocol error") && !strings.Contains(streamText, "unmarshal end stream") {
					t.Errorf("rejected form %q must be reported as a protocol error, got: %s", c.name, streamText)
				}
				return
			}

			// No end-of-stream envelope: the request reached the RPC, so the
			// stream must carry a DOMAIN error, never a protocol/parse one.
			for _, marker := range []string{rejectedProtocolErr, rejectedUnmarshal} {
				if strings.Contains(streamText, marker) {
					t.Errorf("ending the body without a trailer must not be a protocol error; got %q", streamText)
				}
			}
			if errObj.Message == "" {
				t.Error("the domain error must carry a message")
			}
		})
	}
}

// denyAllInterceptor rejects every RPC — unary and streaming alike — as
// unauthenticated, so the envelope wrapper can be checked against a 401 that
// connect produced itself.
type denyAllInterceptor struct{}

func (denyAllInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing Authorization header"))
	}
}

func (denyAllInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (denyAllInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("missing Authorization header"))
	}
}

// TestStreamingEnvelope_DoesNotTouchConnectErrors proves the substitution is
// 415-only: an auth failure on either the unary or the streaming path keeps the
// status and envelope connect produced. Note the JSON-on-streaming case cannot
// reach the interceptor at all — connect rejects the media type first — which is
// exactly why the 415 needed a body of its own.
func TestStreamingEnvelope_DoesNotTouchConnectErrors(t *testing.T) {
	t.Parallel()
	logger := testDiscardLogger()
	tracker := resource.NewTracker(10, logger)
	svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker}

	payload := []byte(`{"agentId":"nope-not-an-agent"}`)
	framed := make([]byte, 0, 5+len(payload))
	framed = append(framed, 0x00)
	framed = binary.BigEndian.AppendUint32(framed, uint32(len(payload)))
	framed = append(framed, payload...)

	newSrv := func(wrap bool) *httptest.Server {
		path, handler := bunkerv1connect.NewBunkerdHandler(svc,
			connect.WithInterceptors(denyAllInterceptor{}))
		h := http.Handler(handler)
		if wrap {
			h = connectStreamingEnvelope(handler)
		}
		mux := http.NewServeMux()
		mux.Handle(path, h)
		s := httptest.NewServer(mux)
		t.Cleanup(s.Close)
		return s
	}
	wrapped, raw := newSrv(true), newSrv(false)

	cases := []struct {
		name           string
		path           string
		mediaType      string
		body           string
		expectEnvelope bool // JSON on the streaming RPC: the substituted 415
	}{
		{"unary_auth_failure", serverInfoPath, "application/json", `{}`, false},
		{"streaming_auth_failure", execAgentPath, "application/connect+json", string(framed), false},
		{"streaming_unary_media_type_is_refused_before_auth", execAgentPath, "application/json", `{}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wStatus, wBody, _ := post(t, wrapped.URL+c.path, c.mediaType, c.body)
			rStatus, rBody, _ := post(t, raw.URL+c.path, c.mediaType, c.body)

			if c.expectEnvelope {
				// The one exchange the wrapper is allowed to change: same
				// status, but the empty body is replaced by the envelope.
				if rStatus != http.StatusUnsupportedMediaType || len(rBody) != 0 {
					t.Fatalf("premise broken: raw arm must be an empty 415, got %d / %q", rStatus, rBody)
				}
				if wStatus != http.StatusUnsupportedMediaType {
					t.Errorf("status = %d, want %d", wStatus, http.StatusUnsupportedMediaType)
				}
				if len(wBody) == 0 {
					t.Error("JSON on a streaming RPC must carry the envelope")
				}
				return
			}

			if wStatus != rStatus {
				t.Errorf("status = %d, raw = %d (wrapped body: %s)", wStatus, rStatus, wBody)
			}
			if !bytes.Equal(wBody, rBody) {
				t.Errorf("body differs:\n wrapped: %s\n raw:     %s", wBody, rBody)
			}
			if !strings.Contains(string(wBody), `"code":"unauthenticated"`) {
				t.Errorf("body must stay connect's own envelope, got: %s", wBody)
			}
		})
	}
}

func TestServerStreamingProcedures(t *testing.T) {
	t.Parallel()
	got := serverStreamingProcedures()
	if _, ok := got[execAgentPath]; !ok {
		t.Errorf("missing %s: %v", execAgentPath, got)
	}
	for _, path := range []string{serverInfoPath, runAgentPath, agentInfoPath} {
		if _, ok := got[path]; ok {
			t.Errorf("%s is unary and must not be treated as streaming", path)
		}
	}
}

func TestStreamingMediaTypeMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		sent       string
		acceptPost string
		want       []string
	}{
		{
			name:       "json_with_accept_list",
			sent:       "application/json",
			acceptPost: "application/connect+json, application/grpc",
			want: []string{
				execAgentPath,
				`Content-Type "application/json"`,
				"unary REST shape",
				"application/connect+json",
				"Accepted content types: application/connect+json, application/grpc.",
			},
		},
		{
			name: "missing_content_type",
			sent: "",
			want: []string{execAgentPath, "the request's media type", "application/connect+json"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := streamingMediaTypeMessage(execAgentPath, c.sent, c.acceptPost)
			if msg == "" {
				t.Fatal("message is empty")
			}
			for _, want := range c.want {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q must contain %q", msg, want)
				}
			}
			if c.acceptPost == "" && strings.Contains(msg, "Accepted content types") {
				t.Errorf("message must not claim an accepted list it does not have: %q", msg)
			}
		})
	}
}
