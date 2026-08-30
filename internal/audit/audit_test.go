package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	"github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
	"github.com/golang-jwt/jwt/v5"

	"github.com/deployBunker/bunker/internal/auth"
)

// claimsInterceptor injects Claims into the context before the audit
// interceptor runs — mirroring how the auth interceptor composes outside the
// audit interceptor in production.
type claimsInterceptor struct{ claims *auth.Claims }

func (c claimsInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		return next(auth.ContextWithClaims(ctx, c.claims), req)
	}
}

func (c claimsInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func (c claimsInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// serveUnary mounts a real connect unary handler (so Spec and Peer are
// populated exactly as in production) wrapped with the claims injector + audit
// interceptor, and returns the live server.
func serveUnary[Req, Res any](
	t *testing.T,
	l *AuditLog,
	claims *auth.Claims,
	procedure string,
	handler func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error),
) *httptest.Server {
	t.Helper()
	h := connect.NewUnaryHandler(
		procedure,
		handler,
		connect.WithInterceptors(claimsInterceptor{claims: claims}, NewInterceptor(l, nil)),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// requiredFields are the keys every audit record must carry on the wire.
var requiredFields = []string{"ts", "caller", "method", "remote_addr", "agent_id", "duration_ms", "outcome", "summary"}

// newTestLog creates an AuditLog in a temp dir, registered for cleanup.
func newTestLog(t *testing.T) (*AuditLog, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := New(path)
	if err != nil {
		t.Fatalf("New(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, path
}

// readLog returns the raw bytes of the audit file.
func readLog(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	return b
}

// parseRecords unmarshals every JSONL line into a map, failing on garbage.
func parseRecords(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for i, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not valid JSON (%v): %q", i+1, err, line)
		}
		out = append(out, rec)
	}
	return out
}

func TestRecordJSONShape(t *testing.T) {
	l, path := newTestLog(t)
	srv := serveUnary(t, l, &auth.Claims{}, "/bunker.v1.Bunkerd/ServerInfo",
		func(ctx context.Context, req *connect.Request[v1.ServerInfoRequest]) (*connect.Response[v1.ServerInfoResponse], error) {
			return connect.NewResponse(&v1.ServerInfoResponse{Hostname: "test"}), nil
		})

	client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
	req := connect.NewRequest(&v1.ServerInfoRequest{})
	req.Header().Set("Authorization", "Bearer some-token")
	if _, err := client.ServerInfo(context.Background(), req); err != nil {
		t.Fatalf("RPC failed: %v", err)
	}

	records := parseRecords(t, readLog(t, path))
	if len(records) != 1 {
		t.Fatalf("expected exactly 1 record, got %d", len(records))
	}
	rec := records[0]
	for _, field := range requiredFields {
		if _, ok := rec[field]; !ok {
			t.Errorf("record missing field %q (got keys: %v)", field, keysOf(rec))
		}
	}
	if rec["method"] != "/bunker.v1.Bunkerd/ServerInfo" {
		t.Errorf("method = %v, want /bunker.v1.Bunkerd/ServerInfo", rec["method"])
	}
	if rec["caller"] != "master" {
		t.Errorf("caller = %v, want master", rec["caller"])
	}
	if rec["outcome"] != "ok" {
		t.Errorf("outcome = %v, want ok", rec["outcome"])
	}
	if rec["summary"] != "ServerInfo" {
		t.Errorf("summary = %v, want ServerInfo", rec["summary"])
	}
	if rec["agent_id"] != "" {
		t.Errorf("agent_id = %v, want empty for ServerInfo", rec["agent_id"])
	}
	// The live handler context carries the real peer address.
	addr, ok := rec["remote_addr"].(string)
	if !ok || !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("remote_addr = %v, want 127.0.0.1:port", rec["remote_addr"])
	}
	if _, ok := rec["ts"].(string); !ok {
		t.Errorf("ts = %v (%T), want RFC3339 string", rec["ts"], rec["ts"])
	}
	if _, ok := rec["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms = %v (%T), want number", rec["duration_ms"], rec["duration_ms"])
	}
}

func TestRecordNoTokenValueInLog(t *testing.T) {
	l, path := newTestLog(t)
	srv := serveUnary(t, l, nil, "/bunker.v1.Bunkerd/ServerInfo",
		func(ctx context.Context, req *connect.Request[v1.ServerInfoRequest]) (*connect.Response[v1.ServerInfoResponse], error) {
			return connect.NewResponse(&v1.ServerInfoResponse{}), nil
		})

	const secretToken = "GAP047-DISTINCTIVE-SECRET-TOKEN-9f8e7d6c5b4a3z"
	client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
	req := connect.NewRequest(&v1.ServerInfoRequest{})
	req.Header().Set("Authorization", "Bearer "+secretToken)
	if _, err := client.ServerInfo(context.Background(), req); err != nil {
		t.Fatalf("RPC failed: %v", err)
	}

	logBytes := readLog(t, path)
	if strings.Contains(string(logBytes), secretToken) {
		t.Fatalf("audit log contains raw token value: %s", secretToken)
	}
	if strings.Contains(string(logBytes), "Authorization") {
		t.Errorf("audit log contains Authorization header material")
	}
	// The record must still exist and be well-formed.
	records := parseRecords(t, logBytes)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if rec := records[0]; rec["caller"] != "unknown" {
		t.Errorf("caller = %v, want unknown (no claims in context)", rec["caller"])
	}
}

func TestFileMode0600(t *testing.T) {
	_, path := newTestLog(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat audit log: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("audit log mode = %o, want 600", perm)
	}
}

func TestAppendOnly(t *testing.T) {
	l, path := newTestLog(t)

	// Two records through the logger, then reopen the file and append more:
	// the file must grow monotonically and never be truncated/rewritten.
	sizes := []int64{}
	for i := 0; i < 2; i++ {
		if err := l.Log(Record{TS: "t", Caller: "master", Method: "/m", Outcome: "ok", Summary: "s"}); err != nil {
			t.Fatalf("Log: %v", err)
		}
		info, _ := os.Stat(path)
		sizes = append(sizes, info.Size())
	}

	reopened, err := New(path) // reopen on the same file (e.g. daemon restart)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	for i := 0; i < 3; i++ {
		if err := reopened.Log(Record{TS: "t", Caller: "agent:a1", Method: "/m2", Outcome: "ok", Summary: "s2"}); err != nil {
			t.Fatalf("reopened Log: %v", err)
		}
		info, _ := os.Stat(path)
		sizes = append(sizes, info.Size())
	}

	if len(sizes) != 5 {
		t.Fatalf("expected 5 size samples, got %d", len(sizes))
	}
	for i := 1; i < len(sizes); i++ {
		if sizes[i] <= sizes[i-1] {
			t.Errorf("file did not grow between writes: sizes[%d]=%d -> sizes[%d]=%d",
				i-1, sizes[i-1], i, sizes[i])
		}
	}

	records := parseRecords(t, readLog(t, path))
	if len(records) != 5 {
		t.Fatalf("expected 5 records after append, got %d (file was truncated?)", len(records))
	}
	// Original records must be intact (append-only means nothing was rewritten).
	if records[0]["caller"] != "master" || records[4]["caller"] != "agent:a1" {
		t.Errorf("record ordering not preserved: first=%v last=%v", records[0]["caller"], records[4]["caller"])
	}
}

func TestConcurrentWrites(t *testing.T) {
	l, path := newTestLog(t)
	const workers = 16
	const perWorker = 25

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				_ = l.Log(Record{
					TS:      time.Now().UTC().Format(time.RFC3339Nano),
					Caller:  "agent:worker-" + string(rune('a'+w)),
					Method:  "/bunker.v1.Bunkerd/ServerInfo",
					Outcome: "ok",
					Summary: "ServerInfo",
				})
			}
		}(w)
	}
	wg.Wait()

	records := parseRecords(t, readLog(t, path))
	if len(records) != workers*perWorker {
		t.Fatalf("expected %d records, got %d (lines interleaved or lost)", workers*perWorker, len(records))
	}
}

func TestOutcomeErrorCode(t *testing.T) {
	l, path := newTestLog(t)
	srv := serveUnary(t, l, nil, "/bunker.v1.Bunkerd/GetAgent",
		func(ctx context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.AgentId))
		})

	client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
	if _, err := client.GetAgent(context.Background(), connect.NewRequest(&v1.GetAgentRequest{AgentId: "agt-missing"})); err == nil {
		t.Fatal("expected error from handler")
	}

	records := parseRecords(t, readLog(t, path))
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0]["outcome"] != "not_found" {
		t.Errorf("outcome = %v, want not_found", records[0]["outcome"])
	}
	if records[0]["agent_id"] != "agt-missing" {
		t.Errorf("agent_id = %v, want agt-missing", records[0]["agent_id"])
	}
	if records[0]["method"] != "/bunker.v1.Bunkerd/GetAgent" {
		t.Errorf("method = %v, want /bunker.v1.Bunkerd/GetAgent", records[0]["method"])
	}
	if records[0]["summary"] != "GetAgent agent_id=agt-missing" {
		t.Errorf("summary = %v, want 'GetAgent agent_id=agt-missing'", records[0]["summary"])
	}
}

// fakeStreamConn implements connect.StreamingHandlerConn for interceptor tests.
type fakeStreamConn struct {
	spec  connect.Spec
	peer  connect.Peer
	hdrs  http.Header
	calls int
}

func (f *fakeStreamConn) Spec() connect.Spec           { return f.spec }
func (f *fakeStreamConn) Peer() connect.Peer           { return f.peer }
func (f *fakeStreamConn) Receive(any) error            { return io.EOF }
func (f *fakeStreamConn) RequestHeader() http.Header   { return f.hdrs }
func (f *fakeStreamConn) ResponseHeader() http.Header  { return http.Header{} }
func (f *fakeStreamConn) ResponseTrailer() http.Header { return http.Header{} }
func (f *fakeStreamConn) Send(any) error               { return nil }

func TestStreamingHandlerRecorded(t *testing.T) {
	l, path := newTestLog(t)
	interceptor := NewInterceptor(l, nil)

	conn := &fakeStreamConn{
		spec: connect.Spec{
			Procedure: "/bunker.v1.Bunkerd/ExecAgent",
		},
		peer: connect.Peer{Addr: "10.0.0.7:5555", Protocol: "connect"},
		hdrs: http.Header{"Authorization": []string{"Bearer GAP047-STREAM-SECRET-zzz"}},
	}

	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		time.Sleep(15 * time.Millisecond)
		return connect.NewError(connect.CodePermissionDenied, io.ErrClosedPipe)
	}

	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{AgentID: "agt-self"})
	err := interceptor.WrapStreamingHandler(next)(ctx, conn)
	if err == nil {
		t.Fatal("expected error from stream handler")
	}

	logBytes := readLog(t, path)
	if strings.Contains(string(logBytes), "GAP047-STREAM-SECRET-zzz") {
		t.Fatal("audit log contains streaming token value")
	}

	records := parseRecords(t, logBytes)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	rec := records[0]
	if rec["method"] != "/bunker.v1.Bunkerd/ExecAgent" {
		t.Errorf("method = %v", rec["method"])
	}
	if rec["outcome"] != "permission_denied" {
		t.Errorf("outcome = %v, want permission_denied", rec["outcome"])
	}
	// Duration covers the whole stream (until the handler returned).
	if ms, ok := rec["duration_ms"].(float64); !ok || ms < 15 {
		t.Errorf("duration_ms = %v, want >= 15", rec["duration_ms"])
	}
	// No handler stamp here: agent_id falls back to the caller's claims
	// scope. ExecAgent stamps the real target id via StampStreamAgentID
	// (DOGFOOD-012), covered by TestStreamingHandlerExecAgentStampsAgentID.
	if rec["agent_id"] != "agt-self" {
		t.Errorf("agent_id = %v, want agt-self (claims fallback)", rec["agent_id"])
	}
	if rec["caller"] != "agent:agt-self" {
		t.Errorf("caller = %v, want agent:agt-self", rec["caller"])
	}
}

func TestStreamingHandlerSuccess(t *testing.T) {
	l, path := newTestLog(t)
	interceptor := NewInterceptor(l, nil)

	conn := &fakeStreamConn{
		spec: connect.Spec{Procedure: "/bunker.v1.Bunkerd/ExecAgent"},
		hdrs: http.Header{},
	}
	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error { return nil }
	if err := interceptor.WrapStreamingHandler(next)(context.Background(), conn); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	records := parseRecords(t, readLog(t, path))
	if len(records) != 1 || records[0]["outcome"] != "ok" {
		t.Fatalf("expected 1 record with outcome ok, got %v", records)
	}
}

// TestStreamingHandlerExecAgentStampsAgentID is the DOGFOOD-012 regression
// proof: ExecAgent stamps the real target agent id into the streaming sink,
// so even a master-token caller (empty claims scope) produces an audit record
// with agent_id=<aid> — the primary forensics query ('what did the attacker
// exec on which agent?') now finds exec records.
func TestStreamingHandlerExecAgentStampsAgentID(t *testing.T) {
	l, path := newTestLog(t)
	interceptor := NewInterceptor(l, nil)

	conn := &fakeStreamConn{
		spec: connect.Spec{
			Procedure: "/bunker.v1.Bunkerd/ExecAgent",
		},
		peer: connect.Peer{Addr: "10.0.0.7:5555", Protocol: "connect"},
		hdrs: http.Header{"Authorization": []string{"Bearer DOGFOOD012-STREAM-SECRET-token"}},
	}

	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		// Mirrors internal/server/service.go ExecAgent: the handler stamps
		// the target agent id as soon as it knows it, before any early
		// return.
		StampStreamAgentID(ctx, "agt-exec-9")
		return connect.NewError(connect.CodePermissionDenied, io.ErrClosedPipe)
	}

	// Master claims: empty AgentID scope — the pre-fix code produced
	// agent_id:'' for exactly this case.
	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{})
	err := interceptor.WrapStreamingHandler(next)(ctx, conn)
	if err == nil {
		t.Fatal("expected error from stream handler")
	}

	logBytes := readLog(t, path)
	if strings.Contains(string(logBytes), "DOGFOOD012-STREAM-SECRET-token") {
		t.Fatal("audit log contains streaming token value (GAP047 parity broken)")
	}

	records := parseRecords(t, logBytes)
	if len(records) != 1 {
		t.Fatalf("expected exactly 1 record, got %d", len(records))
	}
	rec := records[0]
	if rec["agent_id"] != "agt-exec-9" {
		t.Errorf("agent_id = %v, want agt-exec-9 (handler-stamped, not claims fallback)", rec["agent_id"])
	}
	if rec["remote_addr"] != "10.0.0.7:5555" {
		t.Errorf("remote_addr = %v, want 10.0.0.7:5555 (from conn.Peer())", rec["remote_addr"])
	}
	if rec["summary"] != "ExecAgent agent_id=agt-exec-9" {
		t.Errorf("summary = %v, want 'ExecAgent agent_id=agt-exec-9'", rec["summary"])
	}
	if rec["caller"] != "master" {
		t.Errorf("caller = %v, want master (empty claims -> master)", rec["caller"])
	}
	if rec["outcome"] != "permission_denied" {
		t.Errorf("outcome = %v, want permission_denied", rec["outcome"])
	}
}

// TestStreamingHandlerRemoteAddrFromPeer fixes the empty-remote_addr half of
// DOGFOOD-012: the streaming interceptor reads the peer address from
// conn.Peer() (populated before the interceptor chain runs), independent of
// any handler stamp. It also proves the unstamped fallback is preserved:
// without a StampStreamAgentID call, agent_id still falls back to claims.
func TestStreamingHandlerRemoteAddrFromPeer(t *testing.T) {
	l, path := newTestLog(t)
	interceptor := NewInterceptor(l, nil)

	conn := &fakeStreamConn{
		spec: connect.Spec{
			Procedure: "/bunker.v1.Bunkerd/ExecAgent",
		},
		peer: connect.Peer{Addr: "10.0.0.7:5555", Protocol: "connect"},
		hdrs: http.Header{},
	}

	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		// Deliberately no stamp: proves the remote_addr fix is independent
		// of agent_id stamping.
		return nil
	}

	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{AgentID: "agt-self"})
	if err := interceptor.WrapStreamingHandler(next)(ctx, conn); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	records := parseRecords(t, readLog(t, path))
	if len(records) != 1 {
		t.Fatalf("expected exactly 1 record, got %d", len(records))
	}
	rec := records[0]
	if rec["remote_addr"] != "10.0.0.7:5555" {
		t.Errorf("remote_addr = %v, want 10.0.0.7:5555 (from conn.Peer())", rec["remote_addr"])
	}
	if rec["agent_id"] != "agt-self" {
		t.Errorf("agent_id = %v, want agt-self (unstamped claims fallback preserved)", rec["agent_id"])
	}
}

func TestCallerFromClaims(t *testing.T) {
	cases := []struct {
		name   string
		claims *auth.Claims
		want   string
	}{
		{name: "nil claims", claims: nil, want: "unknown"},
		{name: "agent scoped", claims: &auth.Claims{AgentID: "agt-1"}, want: "agent:agt-1"},
		{name: "agent scoped with key", claims: &auth.Claims{AgentID: "agt-1", KeyID: "bk_abc"}, want: "agent:agt-1 key:bk_abc"},
		{name: "opaque top-level key", claims: &auth.Claims{KeyID: "bk_top", RegisteredClaims: jwt.RegisteredClaims{Subject: "bk_top"}}, want: "key:bk_top"},
		{name: "static master token", claims: &auth.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "static-token"}}, want: "master"},
		{name: "master jwt no subject", claims: &auth.Claims{}, want: "master"},
		{name: "custom subject", claims: &auth.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "owner@example.com"}}, want: "owner@example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := callerFromClaims(tc.claims); got != tc.want {
				t.Errorf("callerFromClaims(%v) = %q, want %q", tc.claims, got, tc.want)
			}
		})
	}
}

func TestTargetAgentID(t *testing.T) {
	cases := []struct {
		name   string
		msg    any
		claims *auth.Claims
		want   string
	}{
		{name: "destroy", msg: &v1.DestroyAgentRequest{AgentId: "a1"}, want: "a1"},
		{name: "get", msg: &v1.GetAgentRequest{AgentId: "a2"}, want: "a2"},
		{name: "agent metrics", msg: &v1.AgentMetricsRequest{AgentId: "a3"}, want: "a3"},
		{name: "exec", msg: &v1.ExecAgentRequest{AgentId: "a4"}, want: "a4"},
		{name: "run", msg: &v1.RunAgentRequest{AgentId: "a5"}, want: "a5"},
		{name: "heartbeat", msg: &v1.HeartbeatAgentRequest{AgentId: "a6"}, want: "a6"},
		{name: "no agent in message falls back to claims", msg: &v1.ServerInfoRequest{}, claims: &auth.Claims{AgentID: "a7"}, want: "a7"},
		{name: "no message falls back to claims", msg: nil, claims: &auth.Claims{AgentID: "a8"}, want: "a8"},
		{name: "no message no claims", msg: nil, claims: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := targetAgentID(tc.msg, tc.claims); got != tc.want {
				t.Errorf("targetAgentID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
