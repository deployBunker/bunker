package webdav

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAltSvcValueFormat pins the payload's exact bytes: the fields a client
// parses are the protocol ("h3"), the authority (":<port>") and the max-age, and
// the literal is the one quic-go's own generator produces (`h3=":<port>";
// ma=2592000`). TestAltSvcValueMatchesQUICGoGeneratedHeader in package server
// holds this against the library's generator, so the literal cannot drift.
func TestAltSvcValueFormat(t *testing.T) {
	for _, tc := range []struct {
		port int
		want string
	}{
		{port: 18080, want: `h3=":18080"; ma=2592000`},
		{port: 443, want: `h3=":443"; ma=2592000`},
	} {
		if got := AltSvcValue(tc.port); got != tc.want {
			t.Fatalf("AltSvcValue(%d) = %q, want %q", tc.port, got, tc.want)
		}
	}
}

// TestH3EndpointLifecycle proves the liveness record the two advertisements
// share: down until a socket exists, live with the bound port, down again the
// moment the listener stops. A nil endpoint is the "no QUIC listener at all"
// case and must behave exactly like a down one — never a panic, never an
// advertisement.
func TestH3EndpointLifecycle(t *testing.T) {
	var nilEP *H3Endpoint
	if port, live := nilEP.Port(); live || port != 0 {
		t.Fatalf("nil endpoint reported live=%v port=%d", live, port)
	}
	if got := nilEP.AltSvc(); got != "" {
		t.Fatalf("nil endpoint advertised %q", got)
	}
	// The nil-receiver writes must be no-ops rather than panics: a caller that
	// has no endpoint to record must not be able to stop the daemon.
	nilEP.SetLive(18080)
	nilEP.SetDown()

	ep := NewH3Endpoint()
	if port, live := ep.Port(); live || port != 0 {
		t.Fatalf("fresh endpoint reported live=%v port=%d", live, port)
	}
	if got := ep.AltSvc(); got != "" {
		t.Fatalf("fresh endpoint advertised %q", got)
	}

	ep.SetLive(18080)
	port, live := ep.Port()
	if !live || port != 18080 {
		t.Fatalf("after SetLive: live=%v port=%d", live, port)
	}
	if got, want := ep.AltSvc(), `h3=":18080"; ma=2592000`; got != want {
		t.Fatalf("live AltSvc() = %q, want %q", got, want)
	}

	ep.SetDown()
	if port, live := ep.Port(); live || port != 0 {
		t.Fatalf("after SetDown: live=%v port=%d", live, port)
	}
	if got := ep.AltSvc(); got != "" {
		t.Fatalf("down endpoint advertised %q", got)
	}
}

// TestH3EndpointSetLiveIsLastWriteWins proves the record carries ONE authority:
// a second SetLive (a listener restart on a new port) replaces the old one
// rather than advertising both.
func TestH3EndpointSetLiveIsLastWriteWins(t *testing.T) {
	ep := NewH3Endpoint()
	ep.SetLive(18080)
	ep.SetLive(18443)
	port, live := ep.Port()
	if !live || port != 18443 {
		t.Fatalf("live=%v port=%d, want live on 18443", live, port)
	}
	if got, want := ep.AltSvc(), `h3=":18443"; ma=2592000`; got != want {
		t.Fatalf("AltSvc() = %q, want %q", got, want)
	}
}

// h3Capabilities fetches the capability document from a handler and decodes it.
func h3Capabilities(t *testing.T, h *Handler, host string, localAddr net.Addr) capabilityJSON {
	t.Helper()
	req := httptest.NewRequest("POST", "/dav/", nil)
	req.Header.Set("X-Bunker-Op", "capabilities")
	req.Host = host
	if localAddr != nil {
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, localAddr))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("capabilities -> %d %s", rec.Code, rec.Body.String())
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Capabilities capabilityJSON `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope did not parse: %v", err)
	}
	if !env.OK {
		t.Fatalf("capabilities envelope not ok: %s", rec.Body.String())
	}
	return env.Result.Capabilities
}

// TestCapabilityDocumentReportsH3Liveness is A-10 at the handler layer: the
// `transports.h3` block reports the RUNNING process, so the two arms of the
// row's "advertised only while listening" constraint are visible in the served
// document itself.
//
// Three arms, one control:
//   - no endpoint (cleartext daemon, or h3 off): available false, the
//     build-level h3 degradation is enumerated, and alt_svc still names the
//     authority O-5 would use (this listener's own port number) so a client
//     knows where h3 will be, not that it is there;
//   - live endpoint on the same port number as the request: available true,
//     the degradation is GONE (an entry for a capability that now exists would
//     describe a process that is not running);
//   - live endpoint on a DIFFERENT port (an explicit server.h3_addr): the
//     document names THAT port, not the one the request arrived on — the
//     advertised authority is the bound socket, never a guess;
//   - the negative control: a down endpoint must not report the live arm's
//     values, which is what makes the live assertions non-vacuous.
func TestCapabilityDocumentReportsH3Liveness(t *testing.T) {
	local := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18080}

	t.Run("no endpoint", func(t *testing.T) {
		h := newTestHandler(t)
		doc := h3Capabilities(t, h, "127.0.0.1:18080", local)
		if doc.Transports["h3"].Available {
			t.Fatal("h3 reported available with no QUIC endpoint")
		}
		if got, want := doc.Transports["h3"].AltSvc, `h3=":18080"; ma=2592000`; got != want {
			t.Fatalf("alt_svc = %q, want the authority the request arrived on (%q)", got, want)
		}
		if _, ok := doc.degradation("h3"); !ok {
			t.Fatalf("the absent h3 listener is not enumerated: %+v", doc.Degradations)
		}
	})

	t.Run("live on the same port", func(t *testing.T) {
		ep := NewH3Endpoint()
		ep.SetLive(18080)
		h := newTestHandler(t, func(c *Config) { c.TLS = true; c.H3 = ep })
		doc := h3Capabilities(t, h, "127.0.0.1:18080", local)
		if !doc.Transports["h3"].Available {
			t.Fatal("h3 reported unavailable while the QUIC endpoint is live")
		}
		if got, want := doc.Transports["h3"].AltSvc, `h3=":18080"; ma=2592000`; got != want {
			t.Fatalf("alt_svc = %q, want %q", got, want)
		}
		if scope, ok := doc.degradation("h3"); ok {
			t.Fatalf("h3 is live but still degraded (scope %q): %+v", scope, doc.Degradations)
		}
		// The other transports must be untouched by h3 being up: h2 comes from
		// TLS and h2c from its opt-in, and this config has neither.
		if !doc.Transports["h2"].Available {
			t.Fatal("h2 lost availability when h3 came up")
		}
	})

	t.Run("live on an explicit other port", func(t *testing.T) {
		ep := NewH3Endpoint()
		ep.SetLive(18443)
		h := newTestHandler(t, func(c *Config) { c.TLS = true; c.H3 = ep })
		doc := h3Capabilities(t, h, "127.0.0.1:18080", local)
		if !doc.Transports["h3"].Available {
			t.Fatal("h3 reported unavailable while the QUIC endpoint is live")
		}
		if got, want := doc.Transports["h3"].AltSvc, `h3=":18443"; ma=2592000`; got != want {
			t.Fatalf("alt_svc = %q, want the LIVE socket (%q), not the request's own port", got, want)
		}
	})

	t.Run("down endpoint is not the live arm", func(t *testing.T) {
		ep := NewH3Endpoint()
		ep.SetLive(18443)
		ep.SetDown()
		h := newTestHandler(t, func(c *Config) { c.TLS = true; c.H3 = ep })
		doc := h3Capabilities(t, h, "127.0.0.1:18080", local)
		if doc.Transports["h3"].Available {
			t.Fatal("a stopped QUIC listener is still reported available")
		}
		if _, ok := doc.degradation("h3"); !ok {
			t.Fatal("a stopped QUIC listener is not reported as degraded")
		}
	})
}
