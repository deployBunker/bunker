package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// newListTestServer starts an httptest server with a chi router mounting
// the connect handler for the given BunkerdHandler implementation.
func newListTestServer(t *testing.T, handler bunkerv1connect.BunkerdHandler) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(handler)
	r.Mount(path, h)
	return httptest.NewServer(r)
}

// mockListServer implements BunkerdHandler with configurable
// ListAgents response. All other methods return Unimplemented.
type mockListServer struct {
	mockBunkerdServer
	listResp *v1.ListAgentsResponse
	listErr  error
	// listDelay holds the handler before responding, so a test can prove a
	// reachable-but-slow server still gets its answer (INT-HOST-012 AC3).
	listDelay time.Duration
}

func (m *mockListServer) ListAgents(
	ctx context.Context,
	req *connect.Request[v1.ListAgentsRequest],
) (*connect.Response[v1.ListAgentsResponse], error) {
	if m.listDelay > 0 {
		select {
		case <-time.After(m.listDelay):
		case <-ctx.Done():
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		}
	}
	if m.listErr != nil {
		return nil, m.listErr
	}
	return connect.NewResponse(m.listResp), nil
}

// writeListTestConfig writes a CLIConfig with a single server entry
// pointing at the given URL, and sets it as the active server. The entry
// carries the mock-server token (GAP-181: the CLI refuses a tokenless
// entry before dialing) — the mock BunkerdHandler does not check it.
func writeListTestConfig(t *testing.T, home, serverURL string) {
	t.Helper()
	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"default": {
				Name:        "default",
				URL:         serverURL,
				Token:       "test-cli-token",
				ConnectedAt: "2026-06-28T00:00:00Z",
			},
		},
		ActiveServer: "default",
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}
}

func TestListCommand_Help(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewListCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--server", "default", "--help"})
		cmd.Execute()
	})

	if !strings.Contains(output, "List agents on a bunkerd server") && !strings.Contains(output, "List agents on the active bunkerd server") {
		t.Errorf("help output missing description, got:\n%s", output)
	}
	if !strings.Contains(output, "--status") {
		t.Errorf("help output missing --status flag, got:\n%s", output)
	}
	if !strings.Contains(output, "--page-size") {
		t.Errorf("help output missing --page-size flag, got:\n%s", output)
	}
}

func TestListCommand_NoActiveServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewListCommand()
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when no active server")
	}
}

func TestListCommand_Success_WithAgents(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockListServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-test",
				Version:  "v0.2.0",
			},
		},
		listResp: &v1.ListAgentsResponse{
			Agents: []*v1.AgentSummary{
				{
					AgentId:   "abc12345",
					Status:    "running",
					CreatedAt: "2026-06-28T10:00:00Z",
					PublicUrl: "https://abc12345.trycloudflare.com",
				},
				{
					AgentId:   "def67890",
					Status:    "stopped",
					CreatedAt: "2026-06-27T12:00:00Z",
				},
			},
			TotalCount: 2,
		},
	}
	srv := newListTestServer(t, mock)
	defer srv.Close()

	writeListTestConfig(t, tmpDir, srv.URL)

	cmd := NewListCommand()
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	for _, want := range []string{"abc12345", "running", "def67890", "stopped", "Total: 2 agents"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
}

func TestListCommand_Success_NoAgents(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockListServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-test",
				Version:  "v0.2.0",
			},
		},
		listResp: &v1.ListAgentsResponse{
			Agents:     []*v1.AgentSummary{},
			TotalCount: 0,
		},
	}
	srv := newListTestServer(t, mock)
	defer srv.Close()

	writeListTestConfig(t, tmpDir, srv.URL)

	cmd := NewListCommand()
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "No agents found.") {
		t.Errorf("output missing 'No agents found.', got:\n%s", output)
	}
}

func TestListCommand_ServerNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockListServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-nf",
				Version:  "v0.2.0",
			},
		},
		listResp: &v1.ListAgentsResponse{
			Agents: []*v1.AgentSummary{},
		},
	}
	srv := newListTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"other": {
				Name:        "other",
				URL:         srv.URL,
				ConnectedAt: "2026-06-28T00:00:00Z",
			},
		},
		ActiveServer: "other",
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewListCommand()
	cmd.SetArgs([]string{"--server", "missing-server"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for missing server")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

func TestListCommand_ServerError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockListServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-err",
				Version:  "v0.2.0",
			},
		},
		listErr: connect.NewError(connect.CodeInternal, nil),
	}
	srv := newListTestServer(t, mock)
	defer srv.Close()

	writeListTestConfig(t, tmpDir, srv.URL)

	cmd := NewListCommand()
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error from server")
	}
}

// closedLoopbackPort returns a loopback address with nothing listening on it:
// a listener is bound (kernel-assigned port) and immediately closed, so the
// connect is REFUSED rather than depending on a hard-coded port staying free.
func closedLoopbackPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return "http://" + addr
}

// writeOfflineConfig writes a one-server config named `name` pointing at url.
func writeOfflineConfig(t *testing.T, home, name, url string) {
	t.Helper()
	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			name: {
				Name:        name,
				URL:         url,
				Token:       "test-cli-token",
				ConnectedAt: "2026-06-28T00:00:00Z",
			},
		},
		ActiveServer: name,
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}
}

// TestListCommand_OfflineServer verifies INT-HOST-012: an unreachable
// registered server fails FAST and the error names the server and the offline
// state.
//
// This arm is the behavioural red-proof for the commit that introduced the fix:
// against the pre-INT-HOST-012 implementation the error was
// `list agents: unavailable: …connection refused`, which names neither the
// server nor the offline state, so both message assertions fail there while
// `go test` still compiles (no new symbols are referenced).
func TestListCommand_OfflineServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	const serverName = "cube-offline"
	writeOfflineConfig(t, tmpDir, serverName, closedLoopbackPort(t))

	cmd := NewListCommand()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- cmd.Execute()
	}()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("expected error for offline server, got nil")
		}
		// AC1: under 5 s — not the 30 s client timeout the row measured.
		if elapsed >= 5*time.Second {
			t.Fatalf("offline list took %s, want under 5s", elapsed)
		}
		errStr := err.Error()
		if !strings.Contains(errStr, serverName) {
			t.Errorf("error must name the server %q, got: %v", serverName, errStr)
		}
		if !strings.Contains(errStr, "offline") {
			t.Errorf("error must state the offline state, got: %v", errStr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("command hung longer than 10s; expected fast-fail for offline server")
	}
}

// stallingDialer simulates a host that SILENTLY DROPS the TCP handshake — the
// offline mode measured for cube-las-00 (tailscale offline: SYN goes nowhere,
// the connect never completes). It records that it was consulted, so a
// transport that quietly stopped using the list connect bound fails the test.
type stallingDialer struct {
	mu    sync.Mutex
	calls int
	delay time.Duration
}

func (d *stallingDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()

	timer := time.NewTimer(d.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil, fmt.Errorf("dial tcp %s: i/o timeout", addr)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *stallingDialer) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// TestListCommand_OfflineServer_SilentDropHonoursConnectBound proves the
// connect bound is actually WIRED (not merely declared): the list transport
// must consult listDialFunc, and a silently dropped handshake must surface as
// the single classified offline error well under 5 s.
func TestListCommand_OfflineServer_SilentDropHonoursConnectBound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	const serverName = "silent-drop"
	stub := &stallingDialer{delay: 250 * time.Millisecond}
	oldDial := listDialFunc
	listDialFunc = stub.dial
	t.Cleanup(func() { listDialFunc = oldDial })

	// The URL is never actually dialed through — the stub stands in for the
	// network — but it must be a well-formed origin.
	writeOfflineConfig(t, tmpDir, serverName, "http://100.64.0.1:18080")

	cmd := NewListCommand()
	start := time.Now()
	err := cmd.Execute()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for a silently dropped handshake, got nil")
	}
	if got := stub.callCount(); got != 1 {
		t.Fatalf("dial bound not wired: dialer consulted %d times, want 1", got)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("silent-drop list took %s, want under 5s", elapsed)
	}
	errStr := err.Error()
	for _, want := range []string{serverName, "offline", "connect bound"} {
		if !strings.Contains(errStr, want) {
			t.Errorf("error must contain %q, got: %v", want, errStr)
		}
	}
	if strings.HasPrefix(errStr, "list agents:") {
		t.Errorf("unreachable server must not surface as a bare RPC failure, got: %v", errStr)
	}
}

// TestListCommand_OfflineBounds_Invariants pins the two bounds apart: the
// fast-fail belongs to the CONNECT phase only. A regression that cuts the
// whole request to the connect bound would make a slow-but-alive server look
// offline (INT-HOST-012 AC3).
func TestListCommand_OfflineBounds_Invariants(t *testing.T) {
	if listDialTimeout >= 5*time.Second {
		t.Fatalf("connect bound %s must stay well under the 5s offline contract", listDialTimeout)
	}
	if listRPCDeadline < 30*time.Second {
		t.Fatalf("request deadline %s was cut to the connect bound: a reachable-but-slow server would be reported offline", listRPCDeadline)
	}
}

// TestListCallError_RequestDeadlineArm covers the OTHER offline mode directly:
// a daemon that completes the handshake but never answers. The message must
// blame the request deadline (not the connect bound), or the operator is told
// the wrong thing about a host that is up.
func TestListCallError_RequestDeadlineArm(t *testing.T) {
	entry := ServerEntry{Name: "hung", URL: "http://100.64.0.1:18080"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	err := listCallError("hung", entry, ctx, connect.NewError(connect.CodeUnavailable, errors.New("unavailable")))
	if err == nil {
		t.Fatal("expected an error")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "request deadline") {
		t.Errorf("hung-daemon error must name the request deadline, got: %v", errStr)
	}
	if strings.Contains(errStr, "connect bound") {
		t.Errorf("hung-daemon error must not blame the connect bound, got: %v", errStr)
	}
	if !strings.Contains(errStr, "hung") {
		t.Errorf("error must name the server, got: %v", errStr)
	}
}

// TestTailnetNote renders the peer observation without touching a live
// tailnet: the note is derived from tailscaled's answer, and an unavailable
// observation must add NOTHING rather than a guess.
func TestTailnetNote(t *testing.T) {
	old := listTailnetObserve
	t.Cleanup(func() { listTailnetObserve = old })

	const url = "http://100.64.0.1:18080"

	cases := []struct {
		name string
		peer tailnetPeer
		ok   bool
		want string
	}{
		{
			name: "offline with last-seen",
			peer: tailnetPeer{Online: false, LastSeen: time.Now().Add(-48 * time.Hour)},
			ok:   true,
			want: "tailnet: offline, last seen 2d ago",
		},
		{
			name: "never seen",
			peer: tailnetPeer{Online: false},
			ok:   true,
			want: "tailnet: offline, never seen",
		},
		{
			name: "peer online points at the daemon, not the net",
			peer: tailnetPeer{Online: true},
			ok:   true,
			want: "tailnet: peer online",
		},
		{
			name: "no observation adds no claim",
			ok:   false,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listTailnetObserve = func(string) (tailnetPeer, bool) { return tc.peer, tc.ok }
			got := tailnetNote(url)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("unavailable observation must add nothing, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("tailnet note = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// TestObserveTailnetPeer_SkipsLoopback ensures a local server never pays for a
// tailscaled query and never gets a tailnet claim attached.
func TestObserveTailnetPeer_SkipsLoopback(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:18080", "http://localhost:18080", "http://[::1]:18080"} {
		if _, ok := observeTailnetPeer(u); ok {
			t.Errorf("%s: loopback must not produce a tailnet observation", u)
		}
	}
}

// TestPeerMatches covers the three ways a config URL can name a peer.
func TestPeerMatches(t *testing.T) {
	ips := []string{"100.64.0.1", "fd7a:115c:a1e0::1"}
	cases := []struct {
		host, dns, query string
		want             bool
	}{
		{"cube-las-00", "cube-las-00.tailnet.ts.net.", "100.64.0.1", true},
		{"cube-las-00", "cube-las-00.tailnet.ts.net.", "fd7a:115c:a1e0::1", true},
		{"cube-las-00", "cube-las-00.tailnet.ts.net.", "cube-las-00", true},
		{"cube-las-00", "cube-las-00.tailnet.ts.net.", "cube-las-00.tailnet.ts.net", true},
		{"other-host", "other-host.tailnet.ts.net.", "100.64.0.2", false},
	}
	for _, tc := range cases {
		got := peerMatches(tc.host, tc.dns, ips, tc.query)
		if got != tc.want {
			t.Errorf("peerMatches(%q,%q,%v,%q) = %v, want %v", tc.host, tc.dns, ips, tc.query, got, tc.want)
		}
	}
}

// TestHumanizeSince pins the age rendering the operator reads.
func TestHumanizeSince(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		t    time.Time
		want string
	}{
		{now.Add(-10 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-41 * 24 * time.Hour), "41d ago"},
		{now.Add(time.Hour), "just now"}, // clock skew must not print a negative age
	}
	for _, tc := range cases {
		if got := humanizeSince(tc.t, now); got != tc.want {
			t.Errorf("humanizeSince(%s) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

// TestListCommand_SlowButReachableServerUnaffected verifies AC3 behaviourally:
// a server that ANSWERS slower than the connect bound still returns its list.
func TestListCommand_SlowButReachableServerUnaffected(t *testing.T) {
	if testing.Short() {
		t.Skip("slow-server arm waits past the connect bound")
	}
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	delay := listDialTimeout + 500*time.Millisecond
	mock := &mockListServer{
		listDelay: delay,
		listResp: &v1.ListAgentsResponse{
			Agents:     []*v1.AgentSummary{{AgentId: "slow0001", Status: "running", CreatedAt: "2026-11-06T00:00:00Z"}},
			TotalCount: 1,
		},
	}
	srv := newListTestServer(t, mock)
	defer srv.Close()
	writeListTestConfig(t, tmpDir, srv.URL)

	cmd := NewListCommand()
	start := time.Now()
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("reachable but slow server must not fail: %v", err)
		}
	})
	elapsed := time.Since(start)

	if elapsed < delay {
		t.Fatalf("server responded in %s, expected the %s handler delay — fixture did not exercise the slow path", elapsed, delay)
	}
	if !strings.Contains(output, "slow0001") {
		t.Errorf("agent row missing from list output, got:\n%s", output)
	}
}

func TestListCommand_WithDiskUsage(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockListServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname: "bunker-disk",
				Version:  "v0.2.0",
			},
		},
		listResp: &v1.ListAgentsResponse{
			Agents: []*v1.AgentSummary{
				{
					AgentId:       "agent-abc",
					Status:        "running",
					DiskUsedBytes: 80 * 1024 * 1024 * 1024, // 80 GB
					CreatedAt:     "2026-07-28T10:00:00Z",
					Limits: &v1.ResourceLimits{
						DiskMaxBytes: 200 * 1024 * 1024 * 1024, // 200 GB
					},
				},
				{
					AgentId:       "agent-nolimit",
					Status:        "running",
					DiskUsedBytes: 50 * 1024 * 1024 * 1024, // 50 GB
					CreatedAt:     "2026-07-27T12:00:00Z",
				},
			},
			TotalCount: 2,
		},
	}
	srv := newListTestServer(t, mock)
	defer srv.Close()

	writeListTestConfig(t, tmpDir, srv.URL)

	cmd := NewListCommand()
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	// DF-BUNKER-54: the agent's disk figure is two separately labelled facts —
	// measured usage and a PER-FILE size cap — and NOT a used-vs-cap ratio
	// (the old "40% (80.0 GB/200.0 GB)" cell compared real usage against a
	// number no host mechanism enforces). The assertion is updated to the
	// truthful expectation rather than deleted.
	if !strings.Contains(output, "80.0 GB") {
		t.Errorf("output missing measured disk usage 80.0 GB, got:\n%s", output)
	}
	if !strings.Contains(output, "200.0 GB") {
		t.Errorf("output missing the per-file cap 200.0 GB, got:\n%s", output)
	}
	if !strings.Contains(output, "Max File Size") {
		t.Errorf("output missing the per-file cap column, got:\n%s", output)
	}
	// Agent without a cap: an explicit absence, not "0% (50.0 GB/0 B)".
	if !strings.Contains(output, "50.0 GB") {
		t.Errorf("output missing measured disk usage 50.0 GB, got:\n%s", output)
	}
	if strings.Contains(output, "% (") {
		t.Errorf("output still renders a usage-vs-cap percentage, got:\n%s", output)
	}
	if !strings.Contains(output, "Total: 2 agents") {
		t.Errorf("output missing total, got:\n%s", output)
	}
}
