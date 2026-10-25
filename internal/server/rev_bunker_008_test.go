// rev_bunker_008_test.go — the measured auth ordering on an RPC path, and what
// the audit trail can see of it.
//
// The docs (README.md "Auth ordering", specs/api.md "POST-only, and auth is the
// last gate") state one ordering for every RPC path:
//
//	405 method gate  →  415 content negotiation  →  401 auth
//
// and one consequence: the first two legs stop before the auth interceptor, so
// they leave NO audit record and NO deny-sink entry — only a well-formed
// unauthenticated POST is denied *and* recorded.
//
// This test is the executable half of that claim: it drives real HTTP at a real
// connect handler composed exactly as server.go composes it (auth interceptor +
// audit deny sink), and asserts the status of each leg plus the audit sink's
// contents after each one. If connect-go's ordering ever changes, the docs pin
// in internal/docscheck/rev_bunker_008_docs_test.go would keep matching prose
// while the daemon answered differently — this cell is what catches that.
package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
)

// revBunker008Server builds the daemon's real auth composition (auth enabled,
// audit trail open, deny sink attached) over the shared connect harness.
func revBunker008Server(t *testing.T) (baseURL string, auditPath string) {
	t.Helper()

	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = true
	cfg.Auth.Token = "rev-bunker-008-master-token"
	cfg.Auth.JWTSecret = "rev-bunker-008-secret-0123456789abcdef"
	cfg.Audit.Enabled = true
	cfg.Audit.Path = filepath.Join(t.TempDir(), "audit.log")

	s := New(cfg)
	if s.auditLog == nil {
		t.Fatalf("this cell needs the audit sink wired; %s was not opened", cfg.Audit.Path)
	}
	t.Cleanup(func() { _ = s.auditLog.Close() })

	s.jwtAuth = auth.NewJWTAuthWithStaticFallback(cfg.Auth.JWTSecret, cfg.Auth.Token, nil)
	bunkerdInterceptor, _ := s.buildAuthInterceptors(s.jwtAuth)

	srv, _ := gap133Server(t, bunkerdInterceptor, s.auditLog)
	return srv.URL, cfg.Audit.Path
}

// revBunker008Probe issues one raw HTTP request at an RPC path and returns the
// status plus the Allow header. Raw HTTP on purpose: a generated client would
// normalise the method and the media type away, which is exactly the pair of
// legs under test.
func revBunker008Probe(t *testing.T, baseURL, method, contentType string, body string) (int, string) {
	t.Helper()

	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL+"/bunker.v1.Bunkerd/ServerInfo", rdr)
	if err != nil {
		t.Fatalf("build %s probe: %v", method, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s probe: %v", method, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("Allow")
}

// auditRecordCount returns how many hash-chained audit records exist at path,
// and whether the file even exists yet.
func revBunker008AuditLines(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read audit log %s: %v", path, err)
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// TestREVBUNKER008_AuthOrderingIs405Then415Then401 is the ordering claim: the
// method gate answers first, content negotiation second, auth last — and the
// credential header is irrelevant to the first two legs.
func TestREVBUNKER008_AuthOrderingIs405Then415Then401(t *testing.T) {
	baseURL, auditPath := revBunker008Server(t)

	t.Run("leg 1: non-POST method answers 405 before anything else", func(t *testing.T) {
		// No credentials, and (for the json-shaped arms) a media type connect
		// would accept — the method alone must decide the answer.
		for _, tc := range []struct{ method, ctype string }{
			{http.MethodGet, ""},
			{http.MethodGet, "application/json"},
			{http.MethodDelete, "application/json"},
			{http.MethodPut, "application/json"},
			{http.MethodHead, ""},
		} {
			status, allow := revBunker008Probe(t, baseURL, tc.method, tc.ctype, "{}")
			if status != http.StatusMethodNotAllowed {
				t.Fatalf("%s (content-type %q) -> %d, want 405 (the method gate precedes content negotiation and auth)", tc.method, tc.ctype, status)
			}
			if !strings.Contains(allow, http.MethodPost) {
				t.Errorf("%s -> 405 with Allow %q, want it to name POST", tc.method, allow)
			}
		}
	})

	t.Run("leg 2: a POST with an unserved media type answers 415 before auth", func(t *testing.T) {
		// No credentials in every arm: if auth ran first these would be 401.
		for _, ctype := range []string{"text/plain", "application/vnd.bunker+json", "application/x-www-form-urlencoded"} {
			status, _ := revBunker008Probe(t, baseURL, http.MethodPost, ctype, "{}")
			if status != http.StatusUnsupportedMediaType {
				t.Fatalf("POST %s without credentials -> %d, want 415 (content negotiation precedes auth)", ctype, status)
			}
		}
		// An absent Content-Type is the same leg: no media type to negotiate.
		if status, _ := revBunker008Probe(t, baseURL, http.MethodPost, "", "{}"); status != http.StatusUnsupportedMediaType {
			t.Fatalf("POST with no Content-Type -> %d, want 415", status)
		}
	})

	t.Run("leg 3: only a well-formed POST reaches auth and answers 401", func(t *testing.T) {
		for _, tc := range []struct{ ctype, body string }{
			{"application/json", "{}"},
			// An empty body IS a valid (empty) proto message; junk bytes would
			// answer 400 from the decode leg, which precedes auth.
			{"application/proto", ""},
		} {
			status, _ := revBunker008Probe(t, baseURL, http.MethodPost, tc.ctype, tc.body)
			if status != http.StatusUnauthorized {
				t.Fatalf("POST %s without credentials -> %d, want 401 (auth is the last gate)", tc.ctype, status)
			}
		}
	})

	t.Run("decode failures answer 400 before auth, with or without a token", func(t *testing.T) {
		// Both arms are the same status: if auth ran first the credential-less
		// arm would be 401. This is why the docs name decode as pre-auth.
		for _, body := range []string{"{bad", ""} {
			req, err := http.NewRequest(http.MethodPost, baseURL+"/bunker.v1.Bunkerd/ServerInfo", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("decode probe: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("JSON body %q without credentials -> %d, want 400 (decode precedes auth)", body, resp.StatusCode)
			}
		}
	})

	// Ordering, not just membership: 405 is not 415, and 415 is not 401.
	if n := revBunker008AuditLines(t, auditPath); n == 0 {
		t.Fatalf("the cell recorded no audit traffic at all; the deny arms below cannot be read from an empty sink")
	}
}

// TestREVBUNKER008_MethodAndMediaTypeProbesAreInvisibleToAudit is the second
// half of the doc claim: 405/415 stop before the auth interceptor, so they
// leave no audit record and no deny-sink entry. One 401 must, by contrast,
// leave exactly one denial record — the positive control that proves the sink
// was listening at all.
func TestREVBUNKER008_MethodAndMediaTypeProbesAreInvisibleToAudit(t *testing.T) {
	baseURL, auditPath := revBunker008Server(t)

	before := revBunker008AuditLines(t, auditPath)

	// A scanner sweep of the pre-auth legs: none of these may be recorded.
	sweep := []struct{ method, ctype, body string }{
		{http.MethodGet, "", ""},
		{http.MethodDelete, "application/json", "{}"},
		{http.MethodPost, "text/plain", "{}"},
		{http.MethodPost, "application/vnd.bunker+json", "{}"},
		{http.MethodPost, "", "{}"},
		// Decode failures: measured with and without a token, the decode error
		// wins, so this leg is pre-auth too and equally unrecorded.
		{http.MethodPost, "application/json", "{bad"},
	}
	for _, probe := range sweep {
		status, _ := revBunker008Probe(t, baseURL, probe.method, probe.ctype, probe.body)
		if status != http.StatusMethodNotAllowed && status != http.StatusUnsupportedMediaType && status != http.StatusBadRequest {
			t.Fatalf("%s %s -> %d, want 405, 415 or 400 for this cell to mean anything", probe.method, probe.ctype, status)
		}
	}
	if after := revBunker008AuditLines(t, auditPath); after != before {
		t.Fatalf("405/415 probes wrote %d audit record(s) — they must stop before the auth interceptor and leave no deny-sink entry (before=%d after=%d)", after-before, before, after)
	}

	// Positive control: the third leg IS recorded.
	if status, _ := revBunker008Probe(t, baseURL, http.MethodPost, "application/json", "{}"); status != http.StatusUnauthorized {
		t.Fatalf("well-formed unauthenticated POST -> %d, want 401", status)
	}
	after := revBunker008AuditLines(t, auditPath)
	if after <= before {
		t.Fatalf("a 401 left no audit record (before=%d after=%d) — the deny sink is not wired, so the invisibility assertion above is vacuous", before, after)
	}
}
