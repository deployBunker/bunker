package auth

import (
	"encoding/json"
	"net/http"

	"connectrpc.com/connect"
)

// Package-level HTTP credential gate for non-connect surfaces.
//
// REV-BUNKER-005: the daemon serves more than its connect RPCs. The hilo
// dependency-graph endpoints (/graph/stats, /graph/related, /graph/impact) are
// plain chi routes on the SAME router and the SAME listeners that carry the
// RPC plane, so every HTTP version a listener negotiates reaches them. They
// were registered with the daemon's request middleware only
// (RequestID/RealIP/Logger/Recoverer/Timeout), which left the daemon HOST's
// codebase graph — file paths and their dependency edges, i.e. host
// reconnaissance — readable by anyone who could reach the port, on a product
// whose premise is that the host is multi-agent and frequently on an overlay.
//
// The gate below is the same credential model the RPC plane uses: it calls
// JWTAuth.AuthenticateRawToken, which is the identical admission path the
// connect interceptors take (static-token fallback, JWT against the live and
// overlap secrets, opaque sub-keys against the durable store, the SEC-15
// per-source throttle and the GAP-133 deny sink). No second, weaker
// comparison exists here, and no new configuration is required: an operator
// who could already call a Bunkerd RPC presents the credential they already
// hold.

// httpPeerAddr reports the source address for the throttle and the audit
// label, falling back to "unknown" when the transport exposes none.
func httpPeerAddr(r *http.Request) string {
	if r.RemoteAddr == "" {
		return "unknown"
	}
	return r.RemoteAddr
}

// writeAuthStatus writes a non-secret JSON error envelope. The body never
// distinguishes "no credential presented" from "wrong credential" — a caller
// learns only that it is not admitted.
func writeAuthStatus(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message})
}

// RequireBearerHTTP returns net/http middleware that admits a request only
// after the daemon's own credential check succeeds.
//
// Posture per configuration, deliberately identical to the RPC plane:
//
//   - auth disabled (or no validator instance): pass through. The daemon's
//     RPCs are open in this configuration too; a gate here would be a NEW
//     requirement invented by this surface, not the same credentials as the
//     RPCs.
//   - auth enabled: a request without a valid credential is refused 401, and a
//     source already in SEC-15 backoff is refused 503 — the HTTP rendering of
//     the CodeUnavailable the RPC plane returns, so both surfaces tell one
//     story about a throttled source.
//
// The gate rides the SHARED validator instance, so a secret rotation and the
// per-source throttle apply here exactly as they do to the RPCs.
func RequireBearerHTTP(jwa *JWTAuth, enabled bool) func(http.Handler) http.Handler {
	if !enabled || jwa == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// An absent or malformed header is presented as the empty token:
			// it must take the SAME denial path (and count toward the same
			// per-source throttle) as a wrong token, exactly as the connect
			// interceptors do.
			token, _ := ExtractBearerToken(r)
			_, err := jwa.AuthenticateRawToken(token, httpPeerAddr(r), r.Method+" "+r.URL.Path)
			if err == nil {
				next.ServeHTTP(w, r)
				return
			}
			if connect.CodeOf(err) == connect.CodeUnavailable {
				writeAuthStatus(w, http.StatusServiceUnavailable, "unavailable",
					"too many failed authentications from this source; retry later")
				return
			}
			writeAuthStatus(w, http.StatusUnauthorized, "unauthenticated", "valid credentials required")
		})
	}
}
