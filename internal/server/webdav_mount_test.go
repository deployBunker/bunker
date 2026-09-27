package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/deployBunker/bunker/internal/apikey"
	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/server/webdav"
)

// TestMountWebDAVReachesEveryMethod proves the /dav subtree is served ahead of
// the router for EVERY method token.
//
// Regression test: mounted on chi instead, a method-agnostic route matches only
// chi's fixed bitmask of standard methods, so PROPFIND — the single most
// important WebDAV verb — was answered by chi's own 405 handler and never
// reached the surface. The unit tests inside the webdav package drive the
// handler directly and therefore could not see it; this one drives the wrapper
// that production installs.
func TestMountWebDAVReachesEveryMethod(t *testing.T) {
	var seen []string
	dav := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method)
		w.WriteHeader(http.StatusMultiStatus)
	})
	rest := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "router:"+r.Method)
		w.WriteHeader(http.StatusTeapot)
	})
	handler := mountWebDAV(rest, "/dav", dav)

	for _, method := range []string{"PROPFIND", "MKCOL", "COPY", "MOVE", "PROPPATCH", "LOCK", "FROBNICATE"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, "/dav/src", nil))
		if rec.Code != http.StatusMultiStatus {
			t.Fatalf("%s /dav/src -> %d, want the surface's own answer", method, rec.Code)
		}
	}
	for _, path := range []string{"/dav", "/dav/", "/dav/src/main.go"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusMultiStatus {
			t.Fatalf("GET %s -> %d, want the surface", path, rec.Code)
		}
	}
	// The prefix test is path-component exact: a sibling path that merely
	// starts with the same characters must reach the router.
	for _, path := range []string{"/davfoo", "/healthz", "/bunker.v1.BunkerdService/GetInfo"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusTeapot {
			t.Fatalf("GET %s -> %d, want the router", path, rec.Code)
		}
	}
	for _, want := range []string{"PROPFIND", "MKCOL", "COPY", "MOVE", "PROPPATCH", "LOCK", "FROBNICATE"} {
		found := false
		for _, got := range seen {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s never reached the WebDAV surface (seen: %v)", want, seen)
		}
	}
}

// TestAsteriskOptionsIsAnsweredBeforeRouting proves the daemon-level half of
// RFC 4918 §10.1: the asterisk-form request target has no router path, so it is
// answered ahead of the router — and the passthrough for everything else is
// untouched.
func TestAsteriskOptionsIsAnsweredBeforeRouting(t *testing.T) {
	var routed int
	routedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routed++
		w.WriteHeader(http.StatusTeapot)
	})
	handler := asteriskOptions(routedHandler)

	req := httptest.NewRequest("OPTIONS", "/", nil)
	req.RequestURI = "*"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS * -> %d", rec.Code)
	}
	if got := rec.Header().Get("DAV"); got != "" {
		t.Fatalf("OPTIONS * advertised DAV: %q", got)
	}
	if routed != 0 {
		t.Fatal("OPTIONS * was routed instead of answered in place")
	}

	other := httptest.NewRequest("GET", "/healthz", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, other)
	if rec2.Code != http.StatusTeapot || routed != 1 {
		t.Fatalf("passthrough broken: status %d routed=%d", rec2.Code, routed)
	}
}

// TestWebDAVAuthenticatorUsesTheDaemonCredentialModel proves the mount does not
// invent a second credential check: the same JWTAuth instance decides, both
// credential forms a WebDAV client can send are accepted, and a wrong token is
// refused.
func TestWebDAVAuthenticatorUsesTheDaemonCredentialModel(t *testing.T) {
	const staticToken = "s3cret-daemon-token"
	jwtAuth := auth.NewJWTAuthWithStaticFallback("0123456789012345678901234567890123456789", staticToken, nil)

	if got := webdavAuthenticator(jwtAuth, false); got != nil {
		t.Fatal("an authenticator was returned while auth is disabled; the caller must warn instead")
	}

	authFn := webdavAuthenticator(jwtAuth, true)
	if authFn == nil {
		t.Fatal("no authenticator with auth enabled")
	}

	bearer := func(token string) *http.Request {
		r := httptest.NewRequest("GET", "/dav/", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}
	basic := func(user, pass string) *http.Request {
		r := httptest.NewRequest("GET", "/dav/", nil)
		cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		r.Header.Set("Authorization", "Basic "+cred)
		return r
	}

	cases := []struct {
		name string
		req  *http.Request
		want bool
	}{
		{name: "bearer static token", req: bearer(staticToken), want: true},
		{name: "basic with the token as password", req: basic("anyone", staticToken), want: true},
		{name: "basic with the token as username", req: basic(staticToken, ""), want: true},
		{name: "bearer wrong token", req: bearer("not-the-token")},
		{name: "basic wrong password", req: basic("anyone", "not-the-token")},
		{name: "no credentials", req: httptest.NewRequest("GET", "/dav/", nil)},
		{name: "malformed basic", req: func() *http.Request {
			r := httptest.NewRequest("GET", "/dav/", nil)
			r.Header.Set("Authorization", "Basic not-base64!!")
			return r
		}()},
		{name: "unknown scheme", req: func() *http.Request {
			r := httptest.NewRequest("GET", "/dav/", nil)
			r.Header.Set("Authorization", "Digest abc")
			return r
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := authFn(tc.req); got != tc.want {
				t.Fatalf("authenticator = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWebDAVAuthenticatorAcceptsAnIssuedKey proves the mount rides the daemon's
// real key model rather than only the static token: a key minted by the API-key
// manager authenticates too.
func TestWebDAVAuthenticatorAcceptsAnIssuedKey(t *testing.T) {
	keyMgr, err := apikey.NewManagerAt("0123456789012345678901234567890123456789", t.TempDir())
	if err != nil {
		t.Fatalf("key manager: %v", err)
	}
	token, _, err := keyMgr.Generate("", time.Hour)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	jwtAuth := auth.NewJWTAuthWithStaticFallback("0123456789012345678901234567890123456789", "", keyMgr)
	authFn := webdavAuthenticator(jwtAuth, true)

	req := httptest.NewRequest("GET", "/dav/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if !authFn(req) {
		t.Fatal("an issued daemon key did not authenticate the WebDAV mount")
	}
}

// TestWebDAVOpIsAnsweredAheadOfTheRPCTimeout verifies WHERE the E-4 op surface
// actually sits — the question BFS-026 had to answer before putting the
// invalidation channel's poll there.
//
// The daemon installs `middleware.Timeout(server.request_timeout)` on the chi
// router (300 s by default, server.go:153) and mounts the WebDAV handler AHEAD
// of that router (server.go:396). An op never passes through the middleware, so
// no socket deadline governs it. That is the same structural fact BFS-006
// recorded for the watch stream: a stream — or, here, the poll the fallback
// depends on — placed behind the router would be governed by a deadline it must
// not have.
//
// Two controls keep this from being a claim about a router that is not really
// there: the router answers a non-/dav path, and the deadline in this harness is
// demonstrably live (a handler that outlives it is refused with 504).
func TestWebDAVOpIsAnsweredAheadOfTheRPCTimeout(t *testing.T) {
	var routed int
	rest := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routed++
		if r.URL.Path == "/slowz" {
			// Deliberately writes nothing: the deadline middleware's own 504 is
			// then the first write, which is what makes it observable.
			time.Sleep(150 * time.Millisecond)
			return
		}
		w.WriteHeader(http.StatusTeapot)
	})
	r := chi.NewRouter()
	r.Use(middleware.Timeout(50 * time.Millisecond))
	r.Handle("/*", rest)

	dav, err := webdav.New(webdav.Config{Root: t.TempDir(), Build: "mount-test"})
	if err != nil {
		t.Fatalf("webdav.New: %v", err)
	}
	handler := mountWebDAV(r, webdav.Prefix, dav)

	req := httptest.NewRequest("POST", "/dav/", nil)
	req.Header.Set("X-Bunker-Op", "events")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("op events -> %d %s", rec.Code, rec.Body.String())
	}
	var env struct {
		OK     bool   `json:"ok"`
		Op     string `json:"op"`
		Result struct {
			Events []json.RawMessage `json:"events"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("the surface's answer is not an envelope: %v", err)
	}
	if !env.OK || env.Op != "events" || len(env.Result.Events) == 0 {
		t.Fatalf("the surface did not answer the poll: %s", rec.Body.String())
	}
	if routed != 0 {
		t.Fatalf("the poll reached the router (%d times): the request deadline would then govern the channel", routed)
	}

	// Control 1: the router IS installed, and it answers everything outside /dav.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusTeapot || routed != 1 {
		t.Fatalf("control: /healthz -> %d routed=%d, want the router to answer", rec.Code, routed)
	}
	// Control 2: the deadline in this harness really bites, so "the op never
	// reached it" is a statement about the mount and not about an inert middleware.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/slowz", nil))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("control: a handler past the router's deadline -> %d, want 504", rec.Code)
	}
}
