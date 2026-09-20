package audit

import (
	"context"
	"net/http"
	"strings"

	"connectrpc.com/connect"
)

// This file implements the GAP-141 client-session half of the transport
// provenance the trail already carries. The daemon-side half (GAP-126) marks
// records that arrived over a non-loopback PLAINTEXT listener
// (config.InsecurePlaintextMarker, stamped on every record by the AuditLog
// itself). That covers the LISTENER, not the CLIENT: a daemon serving a correct
// TLS listener cannot tell, by itself, whether the caller at the other end
// verified it.
//
// A caller that skipped verification knows, and it says so on the wire:
//
//  1. the CLI's client transport adds UnverifiedHeader to every request in a
//     session built with InsecureSkipVerify and no certificate pin
//     (internal/cli — newBunkerdClient / newBunkerdClientChecked);
//  2. the audit interceptor (and the exec/run command recorder) reads that
//     declaration off the request and stamps the record it writes with
//     TLSUnverifiedMarker.
//
// The marker is a DECLARATION, not a proof: a server cannot verify a client's
// claim about how it dialed. That is exactly why the marker rides the same
// append-only, hash-chained record as everything else — a client can lie about
// it in a request, but it cannot rewrite the record afterward. What the marker
// buys is that an operator reading the trail is never left assuming a session
// was verified when the client itself declared otherwise.

// UnverifiedHeader is the request header a client sets to declare that the TLS
// session carrying the request skipped certificate verification. The value is
// the single byte "1"; the presence of the header with any other value is
// treated as absent, so a stray proxy header cannot manufacture the marker.
//
// The name is deliberately not in the reserved "Connect-"/"Grpc-" protocol
// namespaces (connect drops or rewrites those on the client side), so it
// survives to the daemon's request headers unchanged.
const UnverifiedHeader = "X-Bunker-TLS-Unverified"

// unverifiedHeaderValue is the only header value that counts as a declaration.
const unverifiedHeaderValue = "1"

// TLSUnverifiedMarker is the audit-record marker stamped on a record that
// arrived from a session declaring an unverified TLS connection. Like
// config.InsecurePlaintextMarker it prefixes the record's Summary field, so the
// Record JSON field set is unchanged and every retained parser keeps working.
// The two markers are independent: a request over a plaintext listener from an
// unverified client carries both.
const TLSUnverifiedMarker = "[TLS-UNVERIFIED]"

// UnverifiedSession reports whether the request that produced ctx declared an
// unverified TLS session (see UnverifiedHeader). It reads the handler call info
// connect attaches to the request context, so it works from any handler — the
// interceptor passes headers it already holds instead, because it runs outside
// the handler's call-info scope for streaming RPCs.
//
// A context with no call info (unit calls, a non-RPC caller) reports false: the
// marker is only ever added on positive evidence, never as a default.
func UnverifiedSession(ctx context.Context) bool {
	info, ok := connect.CallInfoForHandlerContext(ctx)
	if !ok {
		return false
	}
	return unverifiedHeader(info.RequestHeader())
}

// unverifiedHeader reports whether header carries the declaration. The lookup
// is case-insensitive and value-exact: HTTP header names are case-insensitive
// by definition and arrive through several canonicalization layers (net/http,
// the connect protocol, a reverse proxy), so a case-sensitive map lookup would
// be a way for the declaration to be silently dropped. A header present with
// any other value is not a claim of anything.
func unverifiedHeader(header http.Header) bool {
	for name, values := range header {
		if !strings.EqualFold(name, UnverifiedHeader) {
			continue
		}
		for _, v := range values {
			if v == unverifiedHeaderValue {
				return true
			}
		}
	}
	return false
}
