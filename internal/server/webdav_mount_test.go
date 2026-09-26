package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/apikey"
	"github.com/deployBunker/bunker/internal/auth"
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
