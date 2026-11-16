package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// TestIsSpawnAgentRequest pins the selector to the generated procedure
// constant: an exact match only. A substring or suffix rule would hand the
// spawn budget to some future procedure whose name merely contains
// "SpawnAgent".
func TestIsSpawnAgentRequest(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{bunkerv1connect.BunkerdSpawnAgentProcedure, true},
		{"/bunker.v1.Bunkerd/ListAgents", false},
		{"/bunker.v1.Bunkerd/GetSpawnProgress", false},
		{"/bunker.v1.Bunkerd/SpawnAgentExtra", false},
		{"/other.Service/SpawnAgent", false},
		{"/bunker.v1.Bunkerd/spawnAgent", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isSpawnAgentRequest(tc.path); got != tc.want {
			t.Errorf("isSpawnAgentRequest(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestRequestTimeoutMiddleware_ScopesSpawnBudget proves BOTH halves of the
// DOGFOOD-015 fix at the HTTP layer: a request that outlives the ordinary
// bound is killed on an ordinary procedure (the 300s bound every other RPC
// keeps) and succeeds on the SpawnAgent procedure (the derived spawn budget).
// Scoping the exemption is the whole point — a daemon-wide raise would let any
// stalled RPC hold a connection for twenty minutes.
func TestRequestTimeoutMiddleware_ScopesSpawnBudget(t *testing.T) {
	const (
		ordinaryBudget = 40 * time.Millisecond
		spawnBudget    = 3 * time.Second
		work           = 200 * time.Millisecond // longer than ordinary, shorter than spawn
	)
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(work):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
			// chi's Timeout middleware owns the response when the deadline
			// fires; this branch only proves the request context died.
			w.WriteHeader(http.StatusGatewayTimeout)
		}
	})
	handler := requestTimeoutMiddleware(ordinaryBudget, spawnBudget)(slow)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	do := func(path string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("do %s: %v", path, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := do(bunkerv1connect.BunkerdSpawnAgentProcedure); code != http.StatusOK {
		t.Errorf("SpawnAgent status = %d, want 200: the spawn budget must not kill a %s stage under a %s ordinary bound",
			code, work, ordinaryBudget)
	}
	if code := do(bunkerv1connect.BunkerdListAgentsProcedure); code == http.StatusOK {
		t.Errorf("ListAgents status = %d, want the ordinary %s deadline to fire", code, ordinaryBudget)
	}
}

// TestServerWiresSpawnBudgetIntoTheTimeoutMiddleware is a WIRING invariant.
// The selector is worthless if the daemon still installs one flat Timeout in
// Run — and the router is composed inline there, so this pins the call site
// rather than booting a daemon. It fails loudly with both the expected and the
// forbidden shape, so a revert to the flat bound is unambiguous.
func TestServerWiresSpawnBudgetIntoTheTimeoutMiddleware(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	body := string(src)
	want := "requestTimeoutMiddleware(s.cfg.Server.RequestTimeout, s.cfg.EffectiveSpawnRequestTimeout())"
	if !strings.Contains(body, want) {
		t.Errorf("Run no longer wires the per-procedure spawn budget; expected the router to install %q", want)
	}
	forbidden := "r.Use(middleware.Timeout(s.cfg.Server.RequestTimeout))"
	if strings.Contains(body, forbidden) {
		t.Errorf("Run installs the flat 300s-class timeout again (%q) — every spawn returns to the DOGFOOD-015 defect", forbidden)
	}
}

// TestRequestTimeoutMiddleware_OrdinaryBudgetStillBindsShortRequests guards the
// other direction: the selector must not accidentally give the ORDINARY budget
// to spawns (which would leave the defect in place) nor an unlimited one to
// anything else.
func TestRequestTimeoutMiddleware_OrdinaryBudgetStillBindsShortRequests(t *testing.T) {
	fast := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := requestTimeoutMiddleware(5*time.Second, 10*time.Second)(fast)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	for _, path := range []string{bunkerv1connect.BunkerdSpawnAgentProcedure, bunkerv1connect.BunkerdListAgentsProcedure} {
		resp, err := srv.Client().Post(srv.URL+path, "application/json", nil)
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("%s status = %d, want 204", path, resp.StatusCode)
		}
	}
}
