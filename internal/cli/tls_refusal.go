package cli

import (
	"errors"
	"fmt"
	"strings"
)

// DF-BUNKER-64: a TLS/trust refusal is a SECURITY verdict, not an outage —
// the daemon's answer is not in question, the client refused to trust the
// channel or its own configuration. Every verb that hits one must say so in a
// way a script can branch on: a named error class on the error chain, and a
// non-zero process exit.
//
// The failure shapes this file names, all observed live (dogfood run
// 2026-09-25b, the nine-arm TLS battery):
//
//   - `bunker list` exits 1 on a pin-mismatch refusal while `bunker status`
//     prints the same refusal and exits 0: formatServerStatus renders the
//     error as an OFFLINE section and RunE returns nil, so a scripted
//     `bunker status || alert` gate never fires (battery arm B1).
//   - the per-invocation insecure-ack refusal arrives as `stream error:
//     unavailable: …` — connect's transport class — although the client never
//     reached the network (arm C3).
//   - `connect http://…` against a TLS daemon reports Go's plaintext-against-
//     TLS reply as `internal: 400 Bad Request` instead of naming the scheme
//     mismatch (arm F).
//
// The contract:
//
//   - ClassifyTLSRefusal(err) names the class for any error a CLI verb can
//     observe. ok is false only for errors that are genuinely not trust or
//     scheme refusals; classification never claims something the chain does
//     not show.
//   - exitCodeForTLSRefusal maps a classified refusal to its process exit
//     code: TLSRefusalExitCode (2) — distinct from the generic 1 so a
//     monitoring gate can tell "the daemon is down" (1) from "the client
//     refused to trust it or its own configuration" (2). A security refusal
//     must never exit 0, and no verb may treat one as success.
//
// errors.As reachability is the whole mechanism, verified against connect
// v1.21.0 (duplex_http_call.go): a transport error is wrapped with
// NewError(CodeUnavailable, err) on BOTH the unary and the streaming path,
// and connect.Error implements Unwrap — so a typed refusal returned by a
// RoundTripper or a VerifyPeerCertificate callback survives to the CLI's
// error site intact. That was proven with a live probe before this change
// landed: a pin refusal raised inside VerifyPeerCertificate comes back as
// `unavailable: <refusal text>` and errors.As still reaches the typed value.
const (
	// TLSClassPinMismatch is a pinned leaf whose hash is not the pin (the
	// certificate changed, or something else is answering on the address).
	TLSClassPinMismatch = "tls-pin-mismatch"
	// TLSClassCertExpired is a pinned leaf outside its validity window.
	TLSClassCertExpired = "tls-cert-expired"
	// TLSClassContradictoryConfig is a mutually exclusive trust decision on
	// one entry (pin + tls_insecure, self-signed mode + insecure, the
	// pin-wins refusal), an unacknowledged insecure knob, or any other
	// unusable trust configuration. The refusal happened BEFORE the network:
	// it must never read as a transport failure (the arm-C3 finding).
	TLSClassContradictoryConfig = "tls-config-contradictory"
	// TLSClassPlaintextToTLS is an http:// URL answered by a TLS endpoint.
	TLSClassPlaintextToTLS = "tls-plaintext-to-tls"
	// TLSClassSchemeMismatch is the inverse mismatch: an https:// URL
	// answered by a plaintext (non-TLS) service. Observed today only on the
	// certificate-observation half; kept adjacent for symmetry.
	TLSClassSchemeMismatch = "tls-scheme-mismatch"

	// TLSRefusalExitCode is the process exit code for a refused TLS/trust
	// channel: non-zero (a security refusal is a failure), and distinct from
	// the generic error exit 1 so a monitor can separate "unreachable" from
	// "untrustworthy". Go and the shell reserve everything at and above 128
	// for signals; this CLI's other reserved codes are 1 (generic), 97
	// (guard) and 141 (broken pipe) — 2 is the conventional CLI
	// processing-failure code and is otherwise unused here.
	TLSRefusalExitCode = 2
)

// TLSRefusalError is a named TLS/trust refusal. It wraps the original error,
// so the operator-facing message text is unchanged and every wording
// assertion against the refusal itself keeps passing.
type TLSRefusalError struct {
	// Class is one of the TLSClass* constants above.
	Class string
	// Err is the original refusal.
	Err error
}

func (e *TLSRefusalError) Error() string {
	return fmt.Sprintf("%s: %v", e.Class, e.Err)
}

// Unwrap exposes the original refusal for errors.Is / errors.As.
func (e *TLSRefusalError) Unwrap() error { return e.Err }

// ClassifyTLSRefusal names the TLS/trust refusal class carried by err, if any.
func ClassifyTLSRefusal(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var refusal *TLSRefusalError
	if errors.As(err, &refusal) {
		return refusal.Class, true
	}

	// 1. Trust-configuration refusals, raised before or during the dial.
	//    Both transport builders (newBunkerdClient, newStatusBunkerdClient)
	//    prefix the refusal with "refusing to dial <url>: " and the refusing
	//    transport returns it for every request; connect-go then wraps it as
	//    CodeUnavailable. The URL prefix plus the refusal's own wording
	//    picks the class — wording that only the client's own trust gate
	//    produces, so a daemon that happens to use similar words in a
	//    response error cannot be misclassified as a client refusal.
	msg := err.Error()
	if strings.Contains(msg, "refusing to dial ") {
		switch {
		case strings.Contains(msg, "contradictory TLS configuration"),
			strings.Contains(msg, "tls_insecure requires an explicit acknowledgement"),
			strings.Contains(msg, "pinning and skipping verification are mutually exclusive"),
			strings.Contains(msg, "no certificate is pinned"),
			strings.Contains(msg, "unusable cert_pin"),
			strings.Contains(msg, "unknown tls_mode"):
			return TLSClassContradictoryConfig, true
		}
	}

	// 1b. Handshake-delivered leaf refusals. verifyPinnedLeaf runs inside
	//     VerifyPeerCertificate; Go's http.Transport folds its verdict into
	//     a tls handshake error whose text carries the verdict verbatim
	//     (verified against a real pinned client and a mismatched leaf), and
	//     connect-go relays that text under CodeUnavailable. The stable
	//     prefixes below are produced ONLY by the client's own trust gate —
	//     a daemon never sees the pin comparison, so its errors can never
	//     carry these words.
	if strings.Contains(msg, "certificate pin mismatch") {
		return TLSClassPinMismatch, true
	}
	if strings.Contains(msg, "certificate pin check") {
		// The same gate, on the validity window: an expired (or not-yet-
		// valid) pinned leaf names itself "certificate pin check for %q".
		if strings.Contains(msg, "EXPIRED") || strings.Contains(msg, "not valid yet") {
			return TLSClassCertExpired, true
		}
		return TLSClassPinMismatch, true
	}

	// 2. A plain-HTTP request answered by a TLS endpoint (dogfood arm F:
	//    `internal: 400 Bad Request` instead of a named class). Two
	//    renderings exist in the field, and both are fingerprints Go itself
	//    writes — a bunker verb never produces them against a healthy
	//    daemon:
	//
	//    a. net/http's plaintext-rejection phrase. A Go TLS listener answers
	//       a plaintext request with HTTP 400 and the body "Client sent an
	//       HTTP request to an HTTPS server."; when that text is visible in
	//       the failure, the mismatch is conclusive.
	//    b. connect-go's rendering of that 400 response: `internal: 400 Bad
	//       Request`, bare (verified against connect v1.21.0: the URL is NOT
	//       quoted). A connect-speaking bunkerd answers every valid RPC with
	//       a connect-encoded error, never a bare non-connect 400, so within
	//       this CLI's surface the bare rendering IS the plaintext-to-TLS
	//       reply. A proxy in front of the daemon that answers 400 out of
	//       band would render the same — and the class text keeps the
	//       original message visible, so nothing is hidden; the operator
	//    still sees the shape they must debug.
	if isPlaintextToTLSDaemonReply(err) {
		return TLSClassPlaintextToTLS, true
	}

	// 3. The certificate-observation half: CertFetchError already carries a
	//    class; a not-tls answer to an https:// probe is the inverse scheme
	//    mismatch.
	var fetchErr *CertFetchError
	if errors.As(err, &fetchErr) && fetchErr.Class == CertFetchClassNotTLS {
		return TLSClassSchemeMismatch, true
	}
	return "", false
}

// exitCodeForTLSRefusal returns the process exit code for a TLS/trust refusal
// (TLSRefusalExitCode) and whether err is one. Any other error reports
// (1, false) and keeps the generic failure exit — this function never turns a
// non-refusal into a refusal or vice versa.
func exitCodeForTLSRefusal(err error) (int, bool) {
	if _, ok := ClassifyTLSRefusal(err); ok {
		return TLSRefusalExitCode, true
	}
	return 1, false
}

// TLSRefusalExitError is the error a verb returns when a TLS/trust refusal
// must terminate the process with TLSRefusalExitCode instead of the generic
// 1. It carries the classified refusal so cmd/bunker's entry point can print
// the operator-facing message (the refusal's own wording and remedies) and
// exit with the distinct code — the same print-then-exit shape main.go
// already uses for ordinary errors, with only the code differing.
type TLSRefusalExitError struct {
	// Class is the TLSClass* constant classified for Err.
	Class string
	// Err is the original refusal (already classified).
	Err error
}

func (e *TLSRefusalExitError) Error() string {
	return fmt.Sprintf("%s: %v", e.Class, e.Err)
}

// Unwrap exposes the classified refusal.
func (e *TLSRefusalExitError) Unwrap() error { return e.Err }

// ExitCode is the process exit status for this refusal.
func (e *TLSRefusalExitError) ExitCode() int { return TLSRefusalExitCode }

// isPlaintextToTLSDaemonReply reports whether err carries the plaintext-
// against-TLS fingerprint. Two renderings, both written by Go itself (never
// by a connect-speaking daemon answering a valid RPC):
//
//   - net/http's rejection phrase, "Client sent an HTTP request to an HTTPS
//     server", in the transport error or the relayed response body;
//   - connect-go's bare `internal: 400 Bad Request` rendering of that reply
//     (the URL is not quoted — verified against connect v1.21.0).
func isPlaintextToTLSDaemonReply(err error) bool {
	msg := err.Error()
	if !strings.Contains(msg, "400 Bad Request") {
		return false
	}
	return strings.Contains(msg, "Client sent an HTTP request to an HTTPS server") ||
		strings.HasPrefix(msg, "internal: 400 Bad Request")
}
