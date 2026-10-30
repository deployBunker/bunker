package bunkerv1connect_test

// Smoke test for the GENERATED connect package (QA-BUNKER-25).
//
// Why it exists: the QA cells only ever saw this package as
//   FAIL ... [build failed]      (broken)  /  ? ... [no test files]  (fine)
// so a build break here and a healthy tree were distinguishable only by the
// absence of a line. With one real test the package reports
//   ok github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect
// on a clean machine — a POSITIVE signal a fresh-system battery can assert.
//
// The stub implements exactly one RPC and embeds the (nil) generated handler
// interface for the rest, so the test pins the generated client↔handler pair
// without a 22-method stub that regeneration would invalidate.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	connect "connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	"github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// serverInfoOnly answers ServerInfo and panics (via the nil embedded interface)
// on every other RPC — that is deliberate: this test must never silently cover
// a route it does not know about.
type serverInfoOnly struct {
	bunkerv1connect.BunkerdHandler
}

func (serverInfoOnly) ServerInfo(context.Context, *connect.Request[v1.ServerInfoRequest]) (*connect.Response[v1.ServerInfoResponse], error) {
	return connect.NewResponse(&v1.ServerInfoResponse{
		Hostname: "smoke-host",
		Version:  "9.9.9",
	}), nil
}

func TestGeneratedServiceRoundTrip(t *testing.T) {
	path, handler := bunkerv1connect.NewBunkerdHandler(serverInfoOnly{})
	if want := "/bunker.v1.Bunkerd/"; path != want {
		t.Fatalf("handler mount path = %q, want %q", path, want)
	}

	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
	resp, err := client.ServerInfo(context.Background(), connect.NewRequest(&v1.ServerInfoRequest{}))
	if err != nil {
		t.Fatalf("ServerInfo over a real HTTP server: %v", err)
	}
	if got, want := resp.Msg.GetHostname(), "smoke-host"; got != want {
		t.Errorf("hostname = %q, want %q", got, want)
	}
	if got, want := resp.Msg.GetVersion(), "9.9.9"; got != want {
		t.Errorf("version = %q, want %q", got, want)
	}
}

// The procedure constants are the HTTP routes; a regenerated proto that renames
// or drops an RPC changes them, so pin the ones the daemon's routes rely on.
func TestProcedureConstants(t *testing.T) {
	cases := []struct{ name, got, want string }{
		{"BunkerdName", bunkerv1connect.BunkerdName, "bunker.v1.Bunkerd"},
		{"AgentName", bunkerv1connect.AgentName, "bunker.v1.Agent"},
		{"ServerInfo", bunkerv1connect.BunkerdServerInfoProcedure, "/bunker.v1.Bunkerd/ServerInfo"},
		{"SpawnAgent", bunkerv1connect.BunkerdSpawnAgentProcedure, "/bunker.v1.Bunkerd/SpawnAgent"},
		{"ExecAgent", bunkerv1connect.BunkerdExecAgentProcedure, "/bunker.v1.Bunkerd/ExecAgent"},
		{"AgentGetInfo", bunkerv1connect.AgentGetInfoProcedure, "/bunker.v1.Agent/GetInfo"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	// A client pointed at a dead server must return a connect error, never a
	// panic or a nil response with a nil error.
	client := bunkerv1connect.NewBunkerdClient(http.DefaultClient, "http://127.0.0.1:1")
	if _, err := client.ServerInfo(context.Background(), connect.NewRequest(&v1.ServerInfoRequest{})); err == nil {
		t.Fatal("ServerInfo against an unreachable server returned nil error")
	}
}
