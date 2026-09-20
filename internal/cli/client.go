package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"

	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// newBunkerdClient creates a connect-go client for the given server entry.
// It applies the entry's trust decision — --tls-insecure, the system root
// store, or a pinned self-signed certificate (GAP-127) — so EVERY CLI RPC
// verifies the daemon the same way `bunker connect` decided it should.
//
// An unusable trust configuration (contradictory pin + tls_insecure, malformed
// pin, self-signed mode with no pin) does not degrade to a verification-free
// client: the client is built with a transport that refuses every request with
// the configuration error. Callers therefore report the real problem — and no
// code path can reach the network with verification silently downgraded.
func newBunkerdClient(entry ServerEntry) bunkerv1connect.BunkerdClient {
	httpClient := &http.Client{Timeout: 300 * time.Second}

	tlsCfg, err := resolveClientTLS(entry)
	if err != nil {
		httpClient.Transport = refusingTransport{err: fmt.Errorf("refusing to dial %s: %w", entry.URL, err)}
		return bunkerv1connect.NewBunkerdClient(httpClient, entry.URL)
	}
	if tlsCfg != nil {
		httpClient.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	}
	return bunkerv1connect.NewBunkerdClient(httpClient, entry.URL)
}

// newBunkerdClientChecked is newBunkerdClient for callers that must return the
// trust-configuration error instead of a client that will fail on first use.
func newBunkerdClientChecked(entry ServerEntry, timeout time.Duration) (bunkerv1connect.BunkerdClient, error) {
	httpClient := &http.Client{Timeout: timeout}
	tlsCfg, err := resolveClientTLS(entry)
	if err != nil {
		return nil, err
	}
	if tlsCfg != nil {
		httpClient.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	}
	return bunkerv1connect.NewBunkerdClient(httpClient, entry.URL), nil
}

// resolveClientTLS maps an entry onto a TLS configuration, consulting the
// in-process trust-on-first-use cache when the entry itself carries no pin yet
// (the `bunker connect --tls self-signed` invocation pins a certificate it has
// just observed and must verify the registration dial against it).
func resolveClientTLS(entry ServerEntry) (*tls.Config, error) {
	tlsCfg, err := buildClientTLSConfig(entry)
	if err != nil {
		return nil, err
	}
	if tlsCfg != nil {
		return tlsCfg, nil
	}
	if pin := CachedCertPin(entry.URL); pin != "" {
		cached := entry
		cached.CertPin = pin
		return buildClientTLSConfig(cached)
	}
	return nil, nil
}

// refusingTransport makes a client whose every request fails with the trust
// configuration error. It exists so a command that would otherwise dial with a
// half-configured trust policy reports the real cause.
type refusingTransport struct{ err error }

func (t refusingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

// resolveToken returns the auth token for a server entry, checking the entry,
// viper config, and BUNKER_TOKEN environment variable.
func resolveToken(entry ServerEntry) string {
	token := entry.Token
	if token == "" {
		token = viper.GetString("token")
	}
	if token == "" {
		token = os.Getenv("BUNKER_TOKEN")
	}
	return token
}

// CheckServerCertificate probes a TLS daemon and compares its observed leaf
// certificate with what the CLI already knows: a pin stored on the entry, or an
// entry registered under the same name/URL.
//
// It returns the observed pin so the caller can persist it. It is the ONE place
// the trust decision for `bunker connect` is made, and it never returns a pin it
// did not verify against the entry it was given.
func CheckServerCertificate(entry ServerEntry, acceptCert bool) (string, *ServerEntry, error) {
	observed, leaf, err := FetchLeafCertificate(context.Background(), entry.URL)
	if err != nil {
		return "", nil, err
	}

	known := existingEntryFor(entry)

	// A stored entry that both pins and skips verification is contradictory;
	// refuse it before any of the trust branches below can interpret it.
	if known != nil && known.TLSInsecure && known.CertPin != "" {
		if err := validateEntryTLS(*known); err != nil {
			return "", known, err
		}
	}

	if known != nil && known.CertPin != "" {
		if !acceptCert {
			if err := verifyPinnedLeaf(known.CertPin, known.NameOrURL(), [][]byte{leaf.Raw}); err != nil {
				return "", known, err
			}
			return known.CertPin, known, nil
		}
		if strings.EqualFold(known.CertPin, observed) {
			fmt.Fprintf(os.Stderr, "certificate of %s is unchanged (pin %s kept)\n",
				known.NameOrURL(), FormatCertPin(observed))
			return known.CertPin, known, nil
		}
		// Deliberate re-pin: the previous pin still goes into the warning so
		// the operator sees exactly what they are replacing.
		fmt.Fprintf(os.Stderr, "\n!! RE-PINNING %q: replacing pinned certificate %s with %s\n\n",
			known.NameOrURL(), FormatCertPin(known.CertPin), FormatCertPin(observed))
		return observed, known, nil
	}

	if known != nil && !acceptCert {
		if known.TLSInsecure {
			return "", known, fmt.Errorf(
				"%s is registered with --tls-insecure (verification disabled). Pinning %s while that flag is "+
					"set would be a contradiction — re-register deliberately:\n"+
					"  bunker connect --tls self-signed --accept-cert %s   (pin it, drop tls_insecure)\n"+
					"  bunker connect --tls-insecure %s                    (keep skipping verification)",
				known.NameOrURL(), FormatCertPin(observed), entry.URL, entry.URL)
		}
		if known.TLSMode == string(TLSModeSystem) {
			// The operator previously said "this server has a real CA chain".
			// Observing a certificate is not a reason to silently convert that
			// entry into a pin.
			return "", known, fmt.Errorf(
				"%s is registered with tls_mode %q (system root store) but TLS mode %q was requested — "+
					"either verify it against the system roots (bunker connect --tls system %s) or "+
					"accept the certificate as a new pin (bunker connect --tls self-signed --accept-cert %s)",
				known.NameOrURL(), TLSModeSystem, TLSModeSelfSigned, entry.URL, entry.URL)
		}
	}

	// First use (or an entry that carries no trust decision yet).
	return observed, known, nil
}

// existingEntryFor finds the registered entry this connect would replace, so
// the trust decision is made against what the CLI already knows rather than
// against a blank slate. The returned value is always a copy.
func existingEntryFor(entry ServerEntry) *ServerEntry {
	cfg, err := LoadCLIConfig()
	if err != nil {
		return nil
	}
	if entry.Name != "" {
		if e, ok := cfg.Servers[entry.Name]; ok {
			return &e
		}
	}
	for _, e := range cfg.Servers {
		if e.URL != "" && e.URL == entry.URL {
			cp := e
			return &cp
		}
	}
	return nil
}

// tlsHelpHint returns an operator-facing hint when err looks like a TLS
// certificate-verification failure, so `bunker connect https://self-signed-host`
// without a trust decision explains the secure path instead of relaying
// "x509: certificate signed by unknown authority" alone.
func tlsHelpHint(err error, url string) string {
	if err == nil {
		return ""
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verifyErr *tls.CertificateVerificationError

	looksLikeCertFailure := errors.As(err, &unknownAuthority) ||
		errors.As(err, &hostname) ||
		errors.As(err, &invalid) ||
		errors.As(err, &verifyErr)
	if !looksLikeCertFailure {
		// connect-go does not always unwrap the transport error, so fall back to
		// the textual signature of a certificate failure. This only ever ADDS
		// guidance to an error that already happened.
		msg := strings.ToLower(err.Error())
		looksLikeCertFailure = strings.Contains(msg, "x509:") ||
			strings.Contains(msg, "certificate signed by unknown authority") ||
			strings.Contains(msg, "certificate is not trusted") ||
			strings.Contains(msg, "certificate is valid for")
	}
	if !looksLikeCertFailure {
		return ""
	}
	return fmt.Sprintf("\n"+
		"  The server's TLS certificate is not verifiable against the system root store.\n"+
		"  If this daemon uses a self-signed certificate (tls.self_signed: true), pin it:\n"+
		"    bunker connect --tls self-signed %s\n"+
		"  If it uses a real CA-issued certificate, check the hostname and the local clock,\n"+
		"  or pass --tls system explicitly.\n"+
		"  Do not work around this with --tls-insecure on a network you do not control.", url)
}
