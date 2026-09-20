package cli

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// This file implements the client half of GAP-127: certificate pinning for
// self-signed bunkerd daemons, so the SECURE configuration is also the EASY
// one. The daemon side already exists (internal/tlsutil + internal/server);
// before this, a self-signed daemon could only be reached by turning
// verification off entirely (--tls-insecure / InsecureSkipVerify), which is
// exactly the habit the fleet wants to remove.
//
// Trust model (trust on first use, like SSH host keys):
//
//  1. `bunker connect --tls self-signed URL` performs a real TLS handshake and
//     captures the leaf certificate the daemon presents. Its sha256 is stored
//     in the server entry as cert_pin, with a LOUD warning naming the
//     fingerprint.
//  2. Every later command dials with that pin: the handshake is not trusted on
//     the strength of a CA chain (there is none) but the leaf MUST hash to the
//     pinned value. A different leaf is a named, non-zero-exit refusal.
//  3. A first-use pin is only ever learned from a connection the operator made
//     deliberately; there is NO path that silently falls back to skipping
//     verification, and a contradictory entry (pin AND tls_insecure) is
//     refused rather than resolved in either direction.
//
// The pin is the certificate's identity: hostname matching against the SANs is
// deliberately NOT enforced here because a self-signed daemon's cert is
// generated from `tls.hosts` (default: localhost) and is routinely reached
// under a name it does not carry (public IP, tunnel hostname). The pin is a
// stronger identity check than a name match. The validity window IS enforced.

const (
	// CertPinPrefix is the canonical presentation of a pin.
	CertPinPrefix = "sha256:"

	// certFetchTimeout bounds the trust-on-first-use handshake. It is short on
	// purpose: `connect` is an interactive command and an unreachable server
	// must not hang it.
	certFetchTimeout = 15 * time.Second

	// certPinHexLen is the length of a sha256 digest in hex.
	certPinHexLen = 64
)

// TLSMode names how the CLI decides to trust a bunkerd server's TLS
// certificate. It is persisted per server entry so later commands inherit the
// decision made at `bunker connect` time.
type TLSMode string

const (
	// TLSModeSystem verifies against the system root store (the historical
	// default: correct for a real CA-signed certificate, and for plain HTTP
	// loopback where there is no TLS at all).
	TLSModeSystem TLSMode = "system"
	// TLSModeSelfSigned verifies the leaf certificate against the pinned
	// fingerprint stored in the entry.
	TLSModeSelfSigned TLSMode = "self-signed"
	// TLSModeInsecure disables verification entirely. It is the explicit
	// opt-out (--tls-insecure), never a fallback.
	TLSModeInsecure TLSMode = "insecure"
)

// ParseTLSMode maps the --tls flag value to a mode. An empty value means
// "unset" (the caller then falls back to the entry's recorded mode).
func ParseTLSMode(s string) (TLSMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case "self-signed", "selfsigned", "self_signed", "pin", "pinned", "tofu":
		return TLSModeSelfSigned, nil
	case "system", "roots", "ca":
		return TLSModeSystem, nil
	case "insecure", "skip-verify":
		return TLSModeInsecure, nil
	default:
		return "", fmt.Errorf("unknown --tls mode %q (want %q or %q)", s, TLSModeSelfSigned, TLSModeSystem)
	}
}

// CertPinHex returns the canonical pin of a parsed certificate: lowercase hex
// sha256 of its DER.
func CertPinHex(leaf *x509.Certificate) string {
	if leaf == nil {
		return ""
	}
	return CertPinHexDER(leaf.Raw)
}

// CertPinHexDER returns the canonical pin of a DER-encoded certificate.
func CertPinHexDER(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// ValidCertPin reports whether s is a canonical pin: 64 hex characters.
func ValidCertPin(s string) bool {
	if len(s) != certPinHexLen {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// FormatCertPin renders a pin the way every message and config file shows it.
func FormatCertPin(pin string) string {
	return CertPinPrefix + strings.ToLower(strings.TrimSpace(pin))
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// buildClientTLSConfig returns the TLS configuration for a server entry, or nil
// when the default transport (system roots / plain HTTP) is correct.
//
// It is the single place that maps an entry's trust fields onto a transport, so
// every CLI command — not just `connect` — verifies against the pin. A
// contradictory or incomplete trust configuration is an ERROR here, never a
// silent downgrade:
//
//   - cert_pin + tls_insecure: two mutually exclusive trust decisions.
//   - a malformed cert_pin: an unparseable pin must not silently unpin.
//   - tls_mode self-signed with no pin: pinning was requested but never
//     established; the caller must be told, not handed a verification-free
//     connection.
func buildClientTLSConfig(entry ServerEntry) (*tls.Config, error) {
	if err := validateEntryTLS(entry); err != nil {
		return nil, err
	}
	if entry.TLSInsecure || entry.TLSMode == string(TLSModeInsecure) {
		return &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // explicit, named opt-out (--tls-insecure)
			MinVersion:         tls.VersionTLS12,
		}, nil
	}
	if entry.CertPin != "" {
		return pinnedTLSConfig(entry)
	}
	if entry.TLSMode == string(TLSModeSelfSigned) {
		return nil, fmt.Errorf(
			"refusing to dial %s: TLS mode is %q but no certificate is pinned — "+
				"pin the daemon's certificate first:\n"+
				"  bunker connect --tls self-signed %s\n"+
				"(or, to verify against the system root store instead: bunker connect --tls system %s)\n"+
				"bunker never falls back to skipping verification",
			entry.URL, TLSModeSelfSigned, entry.URL, entry.URL)
	}
	return nil, nil
}

// validateEntryTLS is the fail-closed check applied to every entry the CLI
// dials. It exists so a contradictory trust configuration cannot be resolved
// silently by "whichever branch runs first".
func validateEntryTLS(entry ServerEntry) error {
	name := entry.Name
	if name == "" {
		name = entry.URL
	}
	if entry.TLSInsecure && entry.CertPin != "" {
		return fmt.Errorf(
			"server %q has a contradictory TLS configuration: tls_insecure (skip verification) "+
				"and cert_pin %s (verify against a pinned certificate) are both set — "+
				"remove tls_insecure from ~/.bunker/config.yaml, or re-connect: "+
				"bunker connect --tls-insecure %s (drops the pin) or "+
				"bunker connect --tls self-signed --accept-cert %s (keeps the pin)",
			name, FormatCertPin(entry.CertPin), entry.URL, entry.URL)
	}
	if entry.TLSInsecure && entry.TLSMode == string(TLSModeSelfSigned) {
		return fmt.Errorf(
			"server %q has a contradictory TLS configuration: tls_mode %q with tls_insecure true — "+
				"a pinned connection cannot also skip verification",
			name, TLSModeSelfSigned)
	}
	if entry.CertPin != "" && !ValidCertPin(entry.CertPin) {
		return fmt.Errorf(
			"server %q has an unusable cert_pin: want %d hex characters (sha256), got %d — "+
				"re-pin with: bunker connect --tls self-signed --accept-cert %s",
			name, certPinHexLen, len(entry.CertPin), entry.URL)
	}
	if entry.TLSMode != "" && entry.TLSMode != string(TLSModeSystem) &&
		entry.TLSMode != string(TLSModeSelfSigned) && entry.TLSMode != string(TLSModeInsecure) {
		return fmt.Errorf(
			"server %q has an unknown tls_mode %q (want %q, %q or %q)",
			name, entry.TLSMode, TLSModeSystem, TLSModeSelfSigned, TLSModeInsecure)
	}
	return nil
}

// pinnedTLSConfig builds a TLS config that verifies the server's leaf
// certificate against pin. Normal chain verification is intentionally turned
// off — a self-signed leaf has no chain — but VerifyPeerCertificate is where
// the pin check lives, and it runs on every handshake.
func pinnedTLSConfig(entry ServerEntry) (*tls.Config, error) {
	pin := strings.ToLower(strings.TrimSpace(entry.CertPin))
	if !ValidCertPin(pin) {
		return nil, fmt.Errorf(
			"server %q: cert_pin is not a sha256 hex digest (want %d hex chars, got %d) — "+
				"re-pin with: bunker connect --tls self-signed --accept-cert %s",
			entry.Name, certPinHexLen, len(entry.CertPin), entry.URL)
	}
	return &tls.Config{
		//nolint:gosec // the leaf is pinned below; there is no CA chain to verify
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyPinnedLeaf(pin, entry.NameOrURL(), rawCerts)
		},
	}, nil
}

// NameOrURL is the human label used in pinning messages.
func (e ServerEntry) NameOrURL() string {
	if e.Name != "" {
		return e.Name
	}
	return e.URL
}

// verifyPinnedLeaf is the actual trust decision for a pinned connection: the
// leaf's DER must hash to pin, and the leaf must be inside its validity window.
// A mismatch is a named refusal that says the certificate changed and how to
// re-pin deliberately.
func verifyPinnedLeaf(pin, name string, rawCerts [][]byte) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("certificate pin mismatch for %q: the server presented no certificate (pinned %s)",
			name, FormatCertPin(pin))
	}
	got := CertPinHexDER(rawCerts[0])
	if !strings.EqualFold(got, pin) {
		return fmt.Errorf("certificate pin mismatch for %q: %s", name, PinMismatchDetail(pin, got))
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("certificate pin check for %q: parse pinned leaf: %w", name, err)
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) {
		return fmt.Errorf(
			"certificate pin check for %q: the pinned certificate is not valid yet (notBefore %s, now %s) — "+
				"check the clock on this host, then re-pin if the daemon was re-keyed: "+
				"bunker connect --tls self-signed --accept-cert %s",
			name, leaf.NotBefore.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339), name)
	}
	if now.After(leaf.NotAfter) {
		return fmt.Errorf(
			"certificate pin check for %q: the pinned certificate EXPIRED (notAfter %s) — "+
				"regenerate it on the daemon host (delete tls.cert_file/tls.key_file and restart bunkerd), "+
				"then re-pin: bunker connect --tls self-signed --accept-cert %s",
			name, leaf.NotAfter.UTC().Format(time.RFC3339), name)
	}
	return nil
}

// PinMismatchDetail is the shared wording for a changed certificate. It names
// both fingerprints and the deliberate re-pin command, because an unexplained
// certificate change is indistinguishable from a machine-in-the-middle.
func PinMismatchDetail(pinned, live string) string {
	return fmt.Sprintf(
		"the pinned certificate is %s but the server now presents %s — the daemon's certificate CHANGED "+
			"(regenerated, replaced, re-keyed, or a different host is answering on this address).\n"+
			"  If that change is deliberate, re-pin it explicitly:\n"+
			"    bunker connect --tls self-signed --accept-cert <url>\n"+
			"  If it is not, do not proceed: an unexpected certificate change is what a man-in-the-middle looks like.",
		FormatCertPin(pinned), FormatCertPin(live))
}

// ---------------------------------------------------------------------------
// Trust-on-first-use fetch
// ---------------------------------------------------------------------------

// Certificate-fetch failure classes. They are distinct because the operator's
// remedy is different for each, and "connect failed" alone is not actionable.
const (
	CertFetchClassNotTLS        = "not-tls"
	CertFetchClassDNS           = "dns"
	CertFetchClassTimeout       = "timeout"
	CertFetchClassConnect       = "connection"
	CertFetchClassNoCertificate = "no-certificate"
	CertFetchClassOther         = "other"
)

// CertFetchError is a classified failure to observe a server's certificate.
type CertFetchError struct {
	Class string
	URL   string
	Err   error
}

func (e *CertFetchError) Error() string {
	const help = "there is no certificate to pin"
	switch e.Class {
	case CertFetchClassNotTLS:
		return fmt.Sprintf("%s: the server at %s is not speaking TLS (plain HTTP or a non-TLS service) — %s", e.Class, e.URL, help)
	case CertFetchClassDNS:
		return fmt.Sprintf("%s: cannot resolve the host in %s — %s (%v)", e.Class, e.URL, help, e.Err)
	case CertFetchClassTimeout:
		return fmt.Sprintf("%s: %s did not answer the TLS handshake in time — %s (%v)", e.Class, e.URL, help, e.Err)
	case CertFetchClassConnect:
		return fmt.Sprintf("%s: cannot reach %s — %s (%v)", e.Class, e.URL, help, e.Err)
	case CertFetchClassNoCertificate:
		return fmt.Sprintf("%s: the TLS handshake with %s produced no certificate — %s (%v)", e.Class, e.URL, help, e.Err)
	default:
		return fmt.Sprintf("%s: TLS probe of %s failed — %s (%v)", e.Class, e.URL, help, e.Err)
	}
}

func (e *CertFetchError) Unwrap() error { return e.Err }

// isUnreachableClass reports whether the failure means "the server was not
// reached", as opposed to "the server answered and something was wrong". Only
// the first kind may leave an already-pinned entry registered-but-unverified.
func isUnreachableClass(class string) bool {
	switch class {
	case CertFetchClassDNS, CertFetchClassTimeout, CertFetchClassConnect:
		return true
	default:
		return false
	}
}

// FetchLeafCertificate performs a real TLS handshake with the server named by
// rawURL and returns the pin of the leaf certificate it presented.
//
// The handshake deliberately does not verify trust: this is the OBSERVATION
// half of trust-on-first-use. The caller decides whether the observed
// fingerprint is acceptable (first use: warn and pin; later: compare with the
// stored pin), so a certificate that fails system verification is still
// observable — which is the whole point for a self-signed daemon.
func FetchLeafCertificate(ctx context.Context, rawURL string) (string, *x509.Certificate, error) {
	addr, err := certFetchAddr(rawURL)
	if err != nil {
		return "", nil, &CertFetchError{Class: CertFetchClassOther, URL: rawURL, Err: err}
	}

	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: certFetchTimeout},
		//nolint:gosec // observation only; the observed leaf is pinned or compared by the caller
		Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
	}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", nil, &CertFetchError{Class: classifyCertFetchError(err), URL: rawURL, Err: err}
	}
	defer conn.Close()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return "", nil, &CertFetchError{Class: CertFetchClassOther, URL: rawURL, Err: errors.New("dial produced a non-TLS connection")}
	}
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return "", nil, &CertFetchError{Class: CertFetchClassNoCertificate, URL: rawURL, Err: errors.New("handshake completed without a peer certificate")}
	}
	leaf := state.PeerCertificates[0]
	return CertPinHex(leaf), leaf, nil
}

// certFetchAddr validates the URL and returns the host:port to dial for it.
func certFetchAddr(rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", errors.New("empty server URL")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse server URL %q: %w", rawURL, err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("server URL %q is not https:// — TLS mode needs a TLS listener to observe", rawURL)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("server URL %q has no host", rawURL)
	}
	if u.Port() == "" {
		return net.JoinHostPort(u.Hostname(), "443"), nil
	}
	return u.Host, nil
}

// classifyCertFetchError names the failure class so every caller can print a
// remedy instead of relaying raw net/http prose.
func classifyCertFetchError(err error) string {
	if err == nil {
		return CertFetchClassOther
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return CertFetchClassNotTLS
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return CertFetchClassDNS
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return CertFetchClassTimeout
	}
	var unknownAuth x509.UnknownAuthorityError
	if errors.As(err, &unknownAuth) {
		return CertFetchClassNoCertificate
	}
	var certInvalid x509.CertificateInvalidError
	if errors.As(err, &certInvalid) {
		return CertFetchClassNoCertificate
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return CertFetchClassNoCertificate
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Timeout() {
			return CertFetchClassTimeout
		}
		return CertFetchClassConnect
	}
	return CertFetchClassOther
}

// ---------------------------------------------------------------------------
// In-process pin cache
// ---------------------------------------------------------------------------

// certPinCache memoises pins learned by a successful handshake in THIS process,
// keyed by cachedPinKey(url).
//
// It exists so the single `bunker connect` invocation that pins a certificate
// does not handshake twice under two different trust policies: the TOFU
// observation (which trusts nothing) and the registered entry's pinned
// transport (which requires the pin) would otherwise be two dials, and the
// second one would need the pin to already be on disk.
//
// It is a convenience only. The entry's cert_pin stays authoritative for every
// later process, and a cached pin is itself verified against the leaf the
// server presents, so a cache collision can only make a dial REFUSE (a
// different host's pin will not match) — never accept the wrong certificate.
var certPinCache = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

func cachedPinKey(rawURL string) string {
	s := strings.ToLower(strings.TrimSpace(rawURL))
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	return strings.TrimSuffix(s, "/")
}

// CachedCertPin returns the pin learned for rawURL in this process, if any.
func CachedCertPin(rawURL string) string {
	certPinCache.Lock()
	defer certPinCache.Unlock()
	return certPinCache.m[cachedPinKey(rawURL)]
}

// StoreCertPin remembers pin for rawURL for the rest of this process.
func StoreCertPin(rawURL, pin string) {
	if pin == "" {
		return
	}
	certPinCache.Lock()
	defer certPinCache.Unlock()
	certPinCache.m[cachedPinKey(rawURL)] = strings.ToLower(strings.TrimSpace(pin))
}

// ResetCertPinCache clears the in-process pin cache. Tests only — production
// code never clears it.
func ResetCertPinCache() {
	certPinCache.Lock()
	defer certPinCache.Unlock()
	certPinCache.m = map[string]string{}
}
