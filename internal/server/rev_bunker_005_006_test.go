package server

// rev_bunker_005_006_test.go — REV-BUNKER-005 (the /graph surface is
// uncredentialed) and REV-BUNKER-006 (the brute-force throttle silently
// vanishes when the audit sink is unavailable).
//
// Both drive the daemon's OWN wiring: 005 builds the router through
// registerGraphRoutes and speaks real HTTP to it, 006 builds the interceptors
// through buildAuthInterceptors and speaks the real connect protocol. Neither
// test re-implements the composition it grades.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"

	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/hilo"
)

const (
	revGraphMasterToken = "rev-bunker-005-master-static-token"
	revGraphJWTSecret   = "rev-bunker-005-signing-secret-0123456789abcdef"
)

// revBunkerGraphServer builds the SAME router the daemon serves for the
// /graph surface, with a real hilo graph, and exposes it over HTTP.
func revBunkerGraphServer(t *testing.T, jwa *auth.JWTAuth, authEnabled bool) string {
	t.Helper()
	graph, err := hilo.NewGraph(t.TempDir(), gap133Logger())
	if err != nil {
		t.Fatalf("hilo.NewGraph: %v", err)
	}
	r := chi.NewRouter()
	// The daemon's own registration and the daemon's own gate: the master-only
	// derivation of the shared instance (see Run), so this router answers
	// exactly as the running daemon does — including refusing agent-scoped
	// sub-keys.
	registerGraphRoutes(r, graph, graphAuthMiddleware(auth.NewMasterOnlyJWTAuthFromAuth(jwa), authEnabled))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL
}

// revGet performs a GET with an optional Authorization header and returns the
// status plus the body.
func revGet(t *testing.T, url, authz string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request %s: %v", url, err)
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

// TestREVBUNKER005_GraphRoutesRequireCredentials is the acceptance cell: an
// unauthenticated GET on every /graph route is refused, the daemon's own
// credential (the master token every existing client already holds) reads it,
// and an agent-scoped sub-key does NOT — the same split the Bunkerd RPC plane
// enforces, because the graph describes the HOST's codebase, not an agent's.
func TestREVBUNKER005_GraphRoutesRequireCredentials(t *testing.T) {
	jwa := auth.NewJWTAuthWithStaticFallback(revGraphJWTSecret, revGraphMasterToken, nil)
	base := revBunkerGraphServer(t, jwa, true)

	// A valid, agent-SCOPED sub-key: a real credential for the Agent plane,
	// and deliberately not one for a daemon-host surface.
	agentToken, err := jwa.IssueAgentToken("rev-bunker-005-agent", time.Hour)
	if err != nil {
		t.Fatalf("IssueAgentToken: %v", err)
	}

	t.Run("unauthenticated is refused on every route", func(t *testing.T) {
		for _, path := range []string{"/graph/stats", "/graph/related?path=x", "/graph/impact?path=x"} {
			code, body := revGet(t, base+path, "")
			if code != http.StatusUnauthorized {
				t.Errorf("GET %s without credentials: status = %d, want 401 (body %q)", path, code, body)
			}
		}
	})

	t.Run("a wrong token is refused", func(t *testing.T) {
		code, _ := revGet(t, base+"/graph/stats", "Bearer not-the-master-token")
		if code != http.StatusUnauthorized {
			t.Errorf("GET /graph/stats with a wrong token: status = %d, want 401", code)
		}
	})

	t.Run("an agent-scoped sub-key is refused", func(t *testing.T) {
		code, _ := revGet(t, base+"/graph/stats", "Bearer "+agentToken)
		if code != http.StatusUnauthorized {
			t.Errorf("GET /graph/stats with an agent-scoped token: status = %d, want 401 (the surface is daemon-level, like the Bunkerd RPCs)", code)
		}
	})

	t.Run("the master credential still reads the graph", func(t *testing.T) {
		code, body := revGet(t, base+"/graph/stats", "Bearer "+revGraphMasterToken)
		if code != http.StatusOK {
			t.Fatalf("GET /graph/stats with the master token: status = %d, want 200 (body %q)", code, body)
		}
		// The response SHAPE must be unchanged for an authenticated caller:
		// a stock consumer parses these four fields.
		var stats struct {
			TotalEdges     int `json:"total_edges"`
			UniqueFiles    int `json:"unique_files"`
			UniqueDeps     int `json:"unique_deps"`
			FilesWithEdges int `json:"files_with_edges"`
		}
		if err := json.Unmarshal([]byte(body), &stats); err != nil {
			t.Fatalf("authenticated /graph/stats body is not the documented JSON shape: %v (body %q)", err, body)
		}

		// The two path-scoped routes answer 200 for an authenticated caller
		// as well (an unknown path is an EMPTY edge list, not an error).
		for _, path := range []string{"/graph/related?path=internal/server/server.go", "/graph/impact?path=internal/server/server.go"} {
			if code, body := revGet(t, base+path, "Bearer "+revGraphMasterToken); code != http.StatusOK {
				t.Errorf("GET %s with the master token: status = %d, want 200 (body %q)", path, code, body)
			}
		}
	})

	t.Run("auth disabled keeps the pre-existing open posture", func(t *testing.T) {
		// With auth.enabled:false the whole daemon (RPCs included) serves
		// without credentials; the graph routes must not invent a stricter
		// posture than the plane they sit next to. This arm exists so a
		// future "always require credentials" edit is a visible decision.
		jwa := auth.NewJWTAuthWithStaticFallback(revGraphJWTSecret, revGraphMasterToken, nil)
		base := revBunkerGraphServer(t, jwa, false)
		code, body := revGet(t, base+"/graph/stats", "")
		if code != http.StatusOK {
			t.Errorf("auth disabled: GET /graph/stats = %d, want 200 (body %q)", code, body)
		}
	})
}

// revExtractDurations pulls every value of key=<go duration> out of a captured
// slog stream (the throttle reports retry_in=<duration>).
func revExtractDurations(log, key string) []time.Duration {
	var out []time.Duration
	for _, field := range strings.Fields(log) {
		v, ok := strings.CutPrefix(field, key+"=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		if d, err := time.ParseDuration(v); err == nil {
			out = append(out, d)
		}
	}
	return out
}

// TestREVBUNKER006_ThrottleArmedWithoutAuditSink is the acceptance cell: with
// the audit sink absent — audit disabled in config, or an audit path that has
// become unwritable — repeated bad tokens are still throttled, and the backoff
// is shown MOVING (2s, then 4s) rather than merely existing as a code path.
func TestREVBUNKER006_ThrottleArmedWithoutAuditSink(t *testing.T) {
	const master = "rev-bunker-006-master-static-token"
	const secret = "rev-bunker-006-signing-secret-0123456789abcdef"

	// Two configurations in which the deny sink is absent, both reached the
	// way an operator reaches them.
	unwritableDir := t.TempDir()
	auditPathBlocker := filepath.Join(unwritableDir, "not-a-directory")
	if err := os.WriteFile(auditPathBlocker, []byte("a regular file where a directory is needed\n"), 0o600); err != nil {
		t.Fatalf("prepare the unwritable audit path: %v", err)
	}

	cases := []struct {
		name  string
		setup func(cfg *config.Config)
	}{
		{
			name: "audit disabled in config",
			setup: func(cfg *config.Config) {
				cfg.Audit.Enabled = false
			},
		},
		{
			name: "audit enabled but the path is unwritable",
			setup: func(cfg *config.Config) {
				cfg.Audit.Enabled = true
				// A regular file as a path component: opening
				// <file>/audit.log fails deterministically on every OS.
				cfg.Audit.Path = filepath.Join(auditPathBlocker, "audit.log")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Auth.Enabled = true
			cfg.Auth.Token = master
			cfg.Auth.JWTSecret = secret
			cfg.Audit.Enabled = false
			tc.setup(cfg)

			s := New(cfg)
			if s.auditLog != nil {
				t.Fatalf("this arm requires NO audit sink; the daemon opened one at %s", cfg.Audit.Path)
			}

			// Exactly the composition Run() uses, via the same helper.
			s.jwtAuth = auth.NewJWTAuthWithStaticFallback(cfg.Auth.JWTSecret, cfg.Auth.Token, nil)
			bunkerdInterceptor, _ := s.buildAuthInterceptors(s.jwtAuth)

			// Capture the throttle's own log line.
			var logBuf strings.Builder
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			_, attacker := gap133Server(t, bunkerdInterceptor, nil)
			ctx := context.Background()

			// Failures 1..threshold are ordinary denials; the last one arms
			// the backoff.
			for i := 1; i <= auth.ThrottleFailureThreshold; i++ {
				if got := connect.CodeOf(callServerInfo(ctx, attacker, "Bearer wrong-token")); got != connect.CodeUnauthenticated {
					t.Fatalf("failure %d: code = %v, want Unauthenticated", i, got)
				}
			}

			// The NEXT unauthenticated request from that source is throttled —
			// with no audit sink in the picture at all.
			if got := connect.CodeOf(callServerInfo(ctx, attacker, "Bearer wrong-token")); got != connect.CodeUnavailable {
				t.Fatalf("6th request: code = %v, want Unavailable (throttled) — the throttle did not arm without an audit sink", got)
			}
			firstBackoffs := revExtractDurations(logBuf.String(), "retry_in")
			if len(firstBackoffs) == 0 {
				t.Fatalf("no 'auth throttle engaged ... retry_in=' line captured; log:\n%s", logBuf.String())
			}
			first := firstBackoffs[len(firstBackoffs)-1]
			if first <= 0 || first > auth.ThrottleMaxBackoff {
				t.Fatalf("first backoff = %v, want 0 < d <= %v", first, auth.ThrottleMaxBackoff)
			}
			if first < 1500*time.Millisecond {
				t.Fatalf("first backoff = %v, want the 2^1 = 2s step (allowing for elapsed time)", first)
			}

			// Wait past the backoff, then fail again: the counter is still
			// over the threshold, so the NEXT backoff is a LARGER step. This
			// is the "counter moves" half — a path that merely existed would
			// keep reporting the same number forever.
			time.Sleep(first + 300*time.Millisecond)
			if got := connect.CodeOf(callServerInfo(ctx, attacker, "Bearer wrong-token")); got != connect.CodeUnauthenticated {
				t.Fatalf("post-backoff attempt: code = %v, want Unauthenticated (the backoff expired)", got)
			}
			if got := connect.CodeOf(callServerInfo(ctx, attacker, "Bearer wrong-token")); got != connect.CodeUnavailable {
				t.Fatalf("re-throttle attempt: code = %v, want Unavailable", got)
			}
			after := revExtractDurations(logBuf.String(), "retry_in")
			second := after[len(after)-1]
			if second <= first {
				t.Fatalf("second backoff = %v, want it LARGER than the first (%v): the throttle counter is not moving", second, first)
			}
			if second < 3500*time.Millisecond {
				t.Fatalf("second backoff = %v, want the 2^2 = 4s step (allowing for elapsed time)", second)
			}

			// A legitimate client from the same listener is unaffected: the
			// throttle is per-source, not global.
			_, legit := gap133Server(t, bunkerdInterceptor, nil)
			if err := callServerInfo(ctx, legit, "Bearer "+master); err != nil {
				t.Fatalf("legit client rejected while another source is throttled: %v", err)
			}

			t.Logf("audit sink absent (%s): throttled at 2^1=%v then 2^2=%v", tc.name, first, second)
		})
	}
}

// TestREVBUNKER006_AuditSinkStillRecordsWhenPresent is the positive control
// next to the cell above: with the audit trail present the throttle AND the
// denial records must both be there — 006 decouples the two, it does not
// replace the sink with the throttle.
func TestREVBUNKER006_AuditSinkStillRecordsWhenPresent(t *testing.T) {
	const master = "rev-bunker-006-sink-master-token"
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = true
	cfg.Auth.Token = master
	cfg.Auth.JWTSecret = "rev-bunker-006-sink-secret-0123456789abcdef"
	cfg.Audit.Enabled = true
	cfg.Audit.Path = filepath.Join(t.TempDir(), "audit.log")

	s := New(cfg)
	if s.auditLog == nil {
		t.Fatalf("audit should have opened at %s", cfg.Audit.Path)
	}
	defer s.auditLog.Close()

	s.jwtAuth = auth.NewJWTAuthWithStaticFallback(cfg.Auth.JWTSecret, cfg.Auth.Token, nil)
	bunkerdInterceptor, _ := s.buildAuthInterceptors(s.jwtAuth)

	_, attacker := gap133Server(t, bunkerdInterceptor, s.auditLog)
	ctx := context.Background()
	for i := 1; i <= auth.ThrottleFailureThreshold; i++ {
		if got := connect.CodeOf(callServerInfo(ctx, attacker, "Bearer wrong-token")); got != connect.CodeUnauthenticated {
			t.Fatalf("failure %d: code = %v, want Unauthenticated", i, got)
		}
	}
	if got := connect.CodeOf(callServerInfo(ctx, attacker, "Bearer wrong-token")); got != connect.CodeUnavailable {
		t.Fatalf("6th request: code = %v, want Unavailable (throttled)", got)
	}
	// The denials reached the audit chain (the sink is still wired).
	recs := readAuditRecords(t, cfg.Audit.Path)
	if len(recs) == 0 {
		t.Fatal("no audit records written for the denials — the sink was lost when the throttle was decoupled")
	}
	var throttled int
	for _, r := range recs {
		if strings.Contains(r.Summary, "(throttled)") {
			throttled++
		}
	}
	if throttled == 0 {
		t.Errorf("the throttled denial never reached the audit chain; records: %d", len(recs))
	}
}
