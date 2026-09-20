package audit

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	"github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"

	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
)

// GAP-141, the audit half: a session that dialed its server with certificate
// verification off DECLARES that on the wire, and every record the daemon
// writes for such a request states it. These tests drive the real connect stack
// (handler + interceptors), so the marker is asserted on the bytes the log
// actually contains rather than on an internal struct — the same standard the
// GAP-126 plaintext battery holds itself to.

// gap141Declare adds the GAP-141 declaration to a request the way the CLI's
// client interceptor does.
func gap141Declare[Req any](req *connect.Request[Req]) *connect.Request[Req] {
	req.Header().Set(UnverifiedHeader, unverifiedHeaderValue)
	return req
}

// TestGAP141_UnverifiedSessionMarksTheRecord is the core table: the marker
// appears exactly when the session declared itself, and a secure session's
// record is byte-identical to what it was before the marker existed.
func TestGAP141_UnverifiedSessionMarksTheRecord(t *testing.T) {
	const procedure = "/bunker.v1.Bunkerd/GetAgent"

	tests := []struct {
		name        string
		header      map[string]string
		wantMarked  bool
		wantSummary string // expected summary when unmarked
	}{
		{
			name:        "verified session writes no marker",
			wantSummary: "GetAgent agent_id=gap141-agent",
		},
		{
			name:       "unverified session marks the record",
			header:     map[string]string{UnverifiedHeader: unverifiedHeaderValue},
			wantMarked: true,
		},
		{
			name:        "a header with any other value is not a declaration",
			header:      map[string]string{UnverifiedHeader: "true"},
			wantSummary: "GetAgent agent_id=gap141-agent",
		},
		{
			name:        "a header set to 0 is not a declaration",
			header:      map[string]string{UnverifiedHeader: "0"},
			wantSummary: "GetAgent agent_id=gap141-agent",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.log")
			l, err := New(path)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = l.Close() })

			srv := serveUnary(t, l, &auth.Claims{}, procedure,
				func(_ context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
					return connect.NewResponse(&v1.GetAgentResponse{
						Agent: &v1.AgentSummary{AgentId: req.Msg.GetAgentId()},
					}), nil
				})
			client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)

			req := connect.NewRequest(&v1.GetAgentRequest{AgentId: "gap141-agent"})
			for k, v := range tc.header {
				req.Header().Set(k, v)
			}
			if _, err := client.GetAgent(context.Background(), req); err != nil {
				t.Fatalf("GetAgent: %v", err)
			}
			_ = l.Close()

			records := parseRecords(t, readLog(t, path))
			if len(records) != 1 {
				t.Fatalf("records = %d, want 1", len(records))
			}
			summary, _ := records[0]["summary"].(string)
			if tc.wantMarked {
				if !strings.HasPrefix(summary, TLSUnverifiedMarker) {
					t.Errorf("summary = %q, want it to start with %q", summary, TLSUnverifiedMarker)
				}
				if got := strings.Count(summary, TLSUnverifiedMarker); got != 1 {
					t.Errorf("marker appears %d times, want exactly 1: %q", got, summary)
				}
			} else {
				if strings.Contains(summary, TLSUnverifiedMarker) {
					t.Errorf("summary = %q must not carry %q", summary, TLSUnverifiedMarker)
				}
				if summary != tc.wantSummary {
					t.Errorf("summary = %q, want the unmarked %q", summary, tc.wantSummary)
				}
			}

			// The marker is part of the hashed canonical line: a marked trail
			// still verifies end to end.
			if n, bad, err := Verify(path); err != nil || bad != 0 || n != 1 {
				t.Errorf("Verify(%s) = (records %d, bad %d, err %v), want (1, 0, nil)", path, n, bad, err)
			}
		})
	}
}

// TestGAP141_UnverifiedSessionIsSeenFromTheHandlerContext proves the accessor
// handlers use (RecordExecCommand's path) reads the same declaration: the
// context connect hands a handler carries the request headers.
func TestGAP141_UnverifiedSessionIsSeenFromTheHandlerContext(t *testing.T) {
	const procedure = "/bunker.v1.Bunkerd/GetAgent"

	tests := []struct {
		name       string
		declare    bool
		wantUnverf bool
	}{
		{name: "declared session", declare: true, wantUnverf: true},
		{name: "verified session", declare: false, wantUnverf: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.log")
			l, err := New(path)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = l.Close() })

			var seenInHandler bool
			srv := serveUnary(t, l, &auth.Claims{}, procedure,
				func(ctx context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
					seenInHandler = UnverifiedSession(ctx)
					return connect.NewResponse(&v1.GetAgentResponse{
						Agent: &v1.AgentSummary{AgentId: req.Msg.GetAgentId()},
					}), nil
				})
			client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)

			req := connect.NewRequest(&v1.GetAgentRequest{AgentId: "gap141-agent"})
			if tc.declare {
				req = gap141Declare(req)
			}
			if _, err := client.GetAgent(context.Background(), req); err != nil {
				t.Fatalf("GetAgent: %v", err)
			}
			if seenInHandler != tc.wantUnverf {
				t.Errorf("UnverifiedSession(handler ctx) = %v, want %v", seenInHandler, tc.wantUnverf)
			}
		})
	}
}

// TestGAP141_StreamingRPCIsMarkedToo covers the streaming path: the interceptor
// reads the declaration from the streaming conn, so a server-stream RPC from an
// unverified session is marked exactly like a unary one.
func TestGAP141_StreamingRPCIsMarkedToo(t *testing.T) {
	const procedure = "/bunker.v1.Bunkerd/ExecAgent"

	tests := []struct {
		name       string
		declare    bool
		wantMarked bool
	}{
		{name: "streaming session marks the record", declare: true, wantMarked: true},
		{name: "verified streaming session is unmarked", declare: false, wantMarked: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.log")
			l, err := New(path)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = l.Close() })

			h := connect.NewServerStreamHandler(
				procedure,
				func(_ context.Context, _ *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
					return stream.Send(&v1.ExecAgentResponse{ExitCode: 0})
				},
				connect.WithInterceptors(claimsInterceptor{claims: &auth.Claims{}}, NewInterceptor(l, nil)),
			)
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)

			client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
			req := connect.NewRequest(&v1.ExecAgentRequest{AgentId: "gap141-agent", Command: "uptime"})
			if tc.declare {
				req = gap141Declare(req)
			}
			stream, err := client.ExecAgent(context.Background(), req)
			if err != nil {
				t.Fatalf("ExecAgent: %v", err)
			}
			for stream.Receive() {
			}
			if serr := stream.Err(); serr != nil {
				t.Fatalf("stream error: %v", serr)
			}
			_ = stream.Close()
			_ = l.Close()

			records := parseRecords(t, readLog(t, path))
			if len(records) != 1 {
				t.Fatalf("records = %d, want 1", len(records))
			}
			summary, _ := records[0]["summary"].(string)
			if got := strings.HasPrefix(summary, TLSUnverifiedMarker); got != tc.wantMarked {
				t.Errorf("summary = %q, marked = %v, want %v", summary, got, tc.wantMarked)
			}
		})
	}
}

// TestGAP141_CommandRecordCarriesTheMarker is the criterion that the marked
// record must not be only the RPC envelope: the exec/run command-content record
// — the one that names WHAT was run — carries the same declaration, so a reader
// filtering on the marker cannot miss the command an unverified session issued.
func TestGAP141_CommandRecordCarriesTheMarker(t *testing.T) {
	const procedure = "/bunker.v1.Bunkerd/RunAgent"

	tests := []struct {
		name       string
		declare    bool
		wantMarked bool
	}{
		{name: "unverified session marks the command record", declare: true, wantMarked: true},
		{name: "verified session leaves the command record unmarked", declare: false, wantMarked: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.log")
			l, err := New(path)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = l.Close() })
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))

			srv := serveUnary(t, l, &auth.Claims{}, procedure,
				func(ctx context.Context, req *connect.Request[v1.RunAgentRequest]) (*connect.Response[v1.RunAgentResponse], error) {
					// The production handler calls the recorder with the request
					// context; this reproduces that call shape exactly.
					RecordExecCommand(ctx, l, logger, ExecRecord{
						Procedure: procedure,
						AgentID:   req.Msg.GetAgentId(),
						Outcome:   "ok",
						Summary:   "echo hi",
					})
					return connect.NewResponse(&v1.RunAgentResponse{RunId: "gap141-run"}), nil
				})
			client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)

			req := connect.NewRequest(&v1.RunAgentRequest{AgentId: "gap141-agent", Command: "echo", Args: []string{"hi"}})
			if tc.declare {
				req = gap141Declare(req)
			}
			if _, err := client.RunAgent(context.Background(), req); err != nil {
				t.Fatalf("RunAgent: %v", err)
			}
			_ = l.Close()

			records := parseRecords(t, readLog(t, path))
			if len(records) != 2 {
				t.Fatalf("records = %d, want 2 (RPC record + command record)", len(records))
			}
			var commandRecords int
			for _, rec := range records {
				method, _ := rec["method"].(string)
				if !strings.HasSuffix(method, ExecRecordMethod) {
					continue
				}
				commandRecords++
				summary, _ := rec["summary"].(string)
				marked := strings.HasPrefix(summary, TLSUnverifiedMarker)
				if marked != tc.wantMarked {
					t.Errorf("command record summary = %q, marked = %v, want %v", summary, marked, tc.wantMarked)
				}
				if tc.wantMarked && !strings.Contains(summary, "echo hi") {
					t.Errorf("command record summary = %q, want it to still carry the command", summary)
				}
			}
			if commandRecords != 1 {
				t.Fatalf("command records = %d, want 1", commandRecords)
			}
			if n, bad, err := Verify(path); err != nil || bad != 0 || n != 2 {
				t.Errorf("Verify = (records %d, bad %d, err %v), want (2, 0, nil)", n, bad, err)
			}
		})
	}
}

// TestGAP141_MarkerComposesWithThePlaintextMarker covers the daemon-side pair: a
// request that arrived over a non-loopback plaintext listener (GAP-126, stamped
// by the log itself) from a client that skipped verification (GAP-141, stamped
// by the interceptor) carries BOTH markers, each exactly once.
func TestGAP141_MarkerComposesWithThePlaintextMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := NewWithOptions(path, Options{InsecurePlaintext: true})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	srv := serveUnary(t, l, &auth.Claims{}, "/bunker.v1.Bunkerd/GetAgent",
		func(_ context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
			return connect.NewResponse(&v1.GetAgentResponse{Agent: &v1.AgentSummary{AgentId: req.Msg.GetAgentId()}}), nil
		})
	client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)

	req := gap141Declare(connect.NewRequest(&v1.GetAgentRequest{AgentId: "both-marked"}))
	if _, err := client.GetAgent(context.Background(), req); err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	_ = l.Close()

	rec := parseRecords(t, readLog(t, path))[0]
	summary, _ := rec["summary"].(string)
	for _, marker := range []string{TLSUnverifiedMarker, config.InsecurePlaintextMarker} {
		if got := strings.Count(summary, marker); got != 1 {
			t.Errorf("summary = %q carries %q %d times, want exactly 1", summary, marker, got)
		}
	}
	if !strings.Contains(summary, "GetAgent agent_id=both-marked") {
		t.Errorf("summary = %q lost the request summary", summary)
	}
}

// TestGAP141_UnverifiedSessionIsFalseWithoutCallInfo pins the fail-closed
// direction of the accessor: no call info (a bare context, a unit call, a
// non-RPC caller) means "not declared", never "assume unverified".
func TestGAP141_UnverifiedSessionIsFalseWithoutCallInfo(t *testing.T) {
	if UnverifiedSession(context.Background()) {
		t.Error("a context with no call info must report a verified session")
	}
	if unverifiedHeader(nil) {
		t.Error("a nil header set must not declare an unverified session")
	}
	if !unverifiedHeader(http.Header{UnverifiedHeader: []string{"1"}}) {
		t.Error("a header set carrying the declaration must report it")
	}
}

// TestGAP141_MarkerDoesNotPrefixThePlaintextBattery'sOwnStamp guards the shared
// helper: prependMarker is idempotent per marker and never doubles one, so the
// two markers cannot corrupt each other's Summary.
func TestGAP141_PrependMarkerIsIdempotentPerMarker(t *testing.T) {
	tests := []struct {
		name    string
		marker  string
		summary string
		want    string
	}{
		{name: "empty summary becomes the marker", marker: TLSUnverifiedMarker, summary: "", want: TLSUnverifiedMarker},
		{name: "marker is prepended once", marker: TLSUnverifiedMarker, summary: "op", want: TLSUnverifiedMarker + " op"},
		{name: "already marked is unchanged", marker: TLSUnverifiedMarker, summary: TLSUnverifiedMarker + " op", want: TLSUnverifiedMarker + " op"},
		{
			name:    "a different marker still prepends",
			marker:  config.InsecurePlaintextMarker,
			summary: TLSUnverifiedMarker + " op",
			want:    config.InsecurePlaintextMarker + " " + TLSUnverifiedMarker + " op",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := prependMarker(tc.marker, tc.summary); got != tc.want {
				t.Errorf("prependMarker(%q, %q) = %q, want %q", tc.marker, tc.summary, got, tc.want)
			}
		})
	}
}

// TestGAP141_BrokenAuditLogStillRefusesNothing pins the write-failure posture:
// the marker is a property of the record, not of the request's success. An
// unverified RPC must complete even when the audit log cannot be written.
func TestGAP141_UnverifiedSessionDoesNotFailTheRPCWhenAuditIsBroken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	srv := serveUnary(t, l, &auth.Claims{}, "/bunker.v1.Bunkerd/GetAgent",
		func(_ context.Context, _ *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
			return connect.NewResponse(&v1.GetAgentResponse{Agent: &v1.AgentSummary{AgentId: "a"}}), nil
		})
	client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)

	req := gap141Declare(connect.NewRequest(&v1.GetAgentRequest{AgentId: "a"}))
	if _, err := client.GetAgent(context.Background(), req); err != nil {
		t.Fatalf("an unwritable audit log must not fail the RPC: %v", err)
	}
}
