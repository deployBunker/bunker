package cli

// DF-BUNKER-64: TLS/trust refusals under script control.
//
// Dogfood run 2026-09-25b (the nine-arm TLS battery) caught four edges this
// file pins shut:
//
//   - arm B1: `bunker list` exited 1 on a pin-mismatch refusal while
//     `bunker status` printed the same refusal and exited 0 — a scripted
//     `bunker status || alert` gate never fired.
//   - arm A2: the TOFU banner claimed the pin was "stored as cert_pin in …"
//     while an auth failure left the config untouched — nothing was stored.
//   - arm C3: the per-invocation insecure-ack refusal arrived as `stream
//     error: unavailable: …`, connect's transport class, although the client
//     never reached the network.
//   - arm F: `connect http://…` against a TLS daemon reported `internal: 400
//     Bad Request` instead of naming the scheme mismatch.
//
// The contract now: every TLS/trust refusal carries a named class
// (TLSClass*) on its error chain, and every verb that hits one exits
// TLSRefusalExitCode (2) — non-zero, and distinct from the generic 1 so a
// monitor can tell "unreachable" from "untrustworthy".
//
// The process-level cells drive the REAL binary (the shared per-checkout
// build from TestMain) because the acceptance criterion is about process
// exit status; an in-process cmd.Execute() call cannot express it. This is
// the same convention exit_code_pipe_test.go uses for DF-BUNKER-43.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// ---------------------------------------------------------------------------
// 1. Classification and exit code, table-driven over every class.
// ---------------------------------------------------------------------------

// TestTLSRefusalClassesAndExitCodes is the acceptance table: per class, BOTH
// the named error class and the mapped exit code. The err column holds the
// exact error shape each producer leaves on the chain:
//
//   - contradictory config: the refusing transport's URL-prefixed wrap (the
//     C3 shape — through connect-go it reads `unavailable: refusing to dial
//     …` or `stream error: unavailable: …`; the classification keys on the
//     refusal, not the transport code).
//   - plaintext-to-TLS: the net/http POST failure quoting the http:// URL and
//     Go's own reply fingerprint, exactly as a probe of the arm-F shape
//     produces.
//   - pin mismatch / expired: the verifyPinnedLeaf refusals, wrapped the way
//     the TLS handshake delivers them to connect-go.
func TestTLSRefusalClassesAndExitCodes(t *testing.T) {
	const pinned = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const other = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	_ = other

	// The two handshake refusals, produced by verifyPinnedLeaf itself so the
	// table exercises the real wording, not a paraphrase.
	mismatchErr := verifyPinnedLeaf(pinned, "prod", [][]byte{[]byte("not-a-cert")})
	if mismatchErr == nil || !strings.Contains(mismatchErr.Error(), "certificate pin mismatch") {
		t.Fatalf("fixture broken: expected a pin-mismatch refusal, got %v", mismatchErr)
	}
	expiredDER := expiredLeafDER(t)
	expiredErr := verifyPinnedLeaf(CertPinHexDER(expiredDER), "stale", [][]byte{expiredDER})
	if expiredErr == nil || !strings.Contains(expiredErr.Error(), "EXPIRED") {
		t.Fatalf("fixture broken: expected an expiry refusal, got %v", expiredErr)
	}

	// The arm-F shape, produced the way production produces it: a connect
	// client pointed at an http:// URL in front of a TLS listener. The probe
	// that decided this change confirmed the rendering twice: a plain
	// http.Client POST gets a SUCCESSFUL 400 response (no error), and the
	// connect client turns it into `internal: 400 Bad Request` with no URL
	// quoted — so the fixture must go through the connect client, not a
	// bare http.Client.
	plaintextToTLSErr := func() error {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		srv.TLS = selfSignedTLSConfig(t)
		srv.StartTLS()
		t.Cleanup(srv.Close)
		httpURL := "http://" + srv.Listener.Addr().String()
		client := bunkerv1connect.NewBunkerdClient(&http.Client{Timeout: 5 * time.Second}, httpURL)
		_, err := client.ServerInfo(context.Background(), connect.NewRequest(&v1.ServerInfoRequest{}))
		if err == nil {
			t.Fatal("fixture broken: an http:// probe of a TLS daemon must fail")
		}
		if !strings.Contains(err.Error(), "400 Bad Request") {
			t.Fatalf("fixture drift: want the historical `internal: 400 Bad Request` shape, got %v", err)
		}
		return err
	}()

	tests := []struct {
		name     string
		err      error
		wantOK   bool
		wantCode int
	}{
		{
			name:     "pin mismatch classifies and exits " + fmt.Sprint(TLSRefusalExitCode),
			err:      fmt.Errorf(`unavailable: %w`, mismatchErr),
			wantOK:   true,
			wantCode: TLSRefusalExitCode,
		},
		{
			name:     "expired pinned certificate classifies and exits " + fmt.Sprint(TLSRefusalExitCode),
			err:      fmt.Errorf(`unavailable: %w`, expiredErr),
			wantOK:   true,
			wantCode: TLSRefusalExitCode,
		},
		{
			name: "contradictory config (the stream-error unavailable shape) classifies",
			err: errors.New(`unavailable: refusing to dial https://host:9090: contradictory TLS configuration: ` +
				"tls_insecure (skip verification) and cert_pin " + FormatCertPin(pinned) +
				" (verify against a pinned certificate) are both set"),
			wantOK:   true,
			wantCode: TLSRefusalExitCode,
		},
		{
			name:     "unacknowledged insecure knob is a config class",
			err:      errors.New(`unavailable: refusing to dial https://host:9090: ` + mustContain(tlsInsecureAckWording)),
			wantOK:   true,
			wantCode: TLSRefusalExitCode,
		},
		{
			name:     "plaintext http URL against a TLS endpoint is named, not '400 Bad Request'",
			err:      plaintextToTLSErr,
			wantOK:   true,
			wantCode: TLSRefusalExitCode,
		},
		{
			name: "connect's internal 400 rendering is named when the URL quoted is http",
			err: errors.New(`Post "http://127.0.0.1:18093/bunker.v1.Bunkerd/ServerInfo": ` +
				"internal: 400 Bad Request: Client sent an HTTP request to an HTTPS server"),
			wantOK:   true,
			wantCode: TLSRefusalExitCode,
		},
		{
			name:   "an offline-but-trusted daemon is NOT a TLS refusal",
			err:    errors.New(`unavailable: Post "https://192.0.2.1:9090/bunker.v1.Bunkerd/ServerInfo": dial tcp: connection refused`),
			wantOK: false,
		},
		{
			name:   "a daemon auth failure is NOT a TLS refusal",
			err:    errors.New("unauthenticated: invalid token"),
			wantOK: false,
		},
		{
			name:   "nil is not a refusal",
			err:    nil,
			wantOK: false,
		},
		{
			name:   "a 400 from a genuine http:// daemon stays unclassified",
			err:    errors.New(`Post "http://proxy:8080/x": unexpected status`),
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			class, ok := ClassifyTLSRefusal(tc.err)
			if ok != tc.wantOK {
				t.Fatalf("ClassifyTLSRefusal ok = %v (class %q), want %v", ok, class, tc.wantOK)
			}
			code, codeOK := exitCodeForTLSRefusal(tc.err)
			if codeOK != tc.wantOK {
				t.Fatalf("exitCodeForTLSRefusal ok = %v, want %v", codeOK, tc.wantOK)
			}
			if ok && code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d (class %q)", code, tc.wantCode, class)
			}
			if !ok && code != 1 {
				t.Fatalf("non-refusal must keep the generic exit 1, got %d", code)
			}
		})
	}
}

// tlsInsecureAckWording is the stable fragment of the GAP-141 acknowledgement
// refusal, kept here so the table above stays readable.
const tlsInsecureAckWording = "tls_insecure requires an explicit acknowledgement"

// stringsOr is retained for table readability; it is the plain string it
// receives.
func stringsOr(s string) string { return s }

var _ = stringsOr

// expiredLeafDER mints a certificate that expired yesterday — the D1/D2
// fixture, minted (not backdated-cached) per test.
func expiredLeafDER(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "expired"},
		NotBefore:             time.Now().Add(-48 * time.Hour),
		NotAfter:              time.Now().Add(-24 * time.Hour),
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create expired certificate: %v", err)
	}
	return der
}

// mustContain wraps a fragment as an error-shaped value for the table.
func mustContain(s string) string { return s }

// ---------------------------------------------------------------------------
// 2. The class rides the REAL transport seams.
// ---------------------------------------------------------------------------

// TestTLSRefusedTransportCarriesClassThroughConnect proves the seam: a client
// built from a contradictory entry must return an error whose chain carries
// the named class — through connect-go's own CodeUnavailable wrapping, on
// both the unary path and the streaming path (the exact rendering the C3 arm
// reported as `stream error: unavailable: …`).
func TestTLSRefusedTransportCarriesClassThroughConnect(t *testing.T) {
	entry := ServerEntry{
		Name:        "contradictory",
		URL:         "https://127.0.0.1:1",
		CertPin:     gap141Pin,
		TLSInsecure: true,
	}

	t.Run("unary RPC", func(t *testing.T) {
		client := newBunkerdClient(entry)
		_, err := client.ServerInfo(t.Context(), connect.NewRequest(&v1.ServerInfoRequest{}))
		if err == nil {
			t.Fatal("a contradictory entry must refuse, got a successful RPC")
		}
		assertNamedTLSRefusal(t, err, TLSClassContradictoryConfig)
	})

	t.Run("streaming RPC", func(t *testing.T) {
		client := newBunkerdClient(entry)
		stream, openErr := client.ExecAgent(t.Context(), connect.NewRequest(&v1.ExecAgentRequest{}))
		if openErr == nil {
			_ = stream.Receive()
			openErr = stream.Err()
		}
		if openErr == nil {
			t.Fatal("a contradictory entry must refuse the stream too")
		}
		assertNamedTLSRefusal(t, openErr, TLSClassContradictoryConfig)
	})

	t.Run("status path client", func(t *testing.T) {
		client := newStatusBunkerdClient(entry)
		_, err := client.ServerInfo(t.Context(), connect.NewRequest(&v1.ServerInfoRequest{}))
		if err == nil {
			t.Fatal("the status client must refuse the same way")
		}
		assertNamedTLSRefusal(t, err, TLSClassContradictoryConfig)
	})
}

// assertNamedTLSRefusal asserts BOTH halves of the acceptance: the named
// class is reachable (errors.As), and the operator-facing refusal wording is
// intact underneath it (the class must not bury the message).
func assertNamedTLSRefusal(t *testing.T, err error, wantClass string) {
	t.Helper()
	class, ok := ClassifyTLSRefusal(err)
	if !ok {
		t.Fatalf("error carries no TLS refusal class: %v", err)
	}
	if class != wantClass {
		t.Fatalf("class = %q, want %q (err: %v)", class, wantClass, err)
	}
	var refusal *TLSRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("*TLSRefusalError unreachable through the chain: %v", err)
	}
	if refusal.Err == nil || refusal.Err.Error() == "" {
		t.Fatal("the class carrier must keep the operator-facing refusal text")
	}
}

// ---------------------------------------------------------------------------
// 3. `bunker status` exits non-zero on a refusal — process level.
// ---------------------------------------------------------------------------

// writeRefusalConfig writes a CLI config whose single entry is contradictory
// (pin + tls_insecure) and returns the env that isolates the run.
func writeRefusalConfig(t *testing.T) (string, []string) {
	t.Helper()
	base := t.TempDir()
	cfgDir := filepath.Join(base, "xdg-config", "bunker")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("servers:\n  refused:\n    name: refused\n    url: https://127.0.0.1:1\n"+
		"    token: t\n    tls_insecure: true\n    cert_pin: %s\nactive_server: refused\n", gap141Pin)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"HOME="+base,
		"XDG_CONFIG_HOME="+filepath.Join(base, "xdg-config"),
		"BUNKER_HOME=",
		"BUNKER_CONFIG_HOME=",
		"BUNKER_TOKEN=",
	)
	return base, env
}

// TestStatusExitCodeOnTLSRefusal is the acceptance criterion itself, driven
// through the real binary: the same refusal that `bunker list` already
// reports as failure must make `bunker status` EXIT NON-ZERO, with the
// distinct TLSRefusalExitCode, while the operator-facing OFFLINE section is
// still printed.
func TestStatusExitCodeOnTLSRefusal(t *testing.T) {
	bin := buildCLIOnce(t)
	_, env := writeRefusalConfig(t)

	run := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("run %v: %v", args, err)
			}
			code = ee.ExitCode()
		}
		return stdout.String() + stderr.String(), code
	}

	t.Run("status exits TLSRefusalExitCode on a contradictory entry", func(t *testing.T) {
		out, code := run("status")
		if code != TLSRefusalExitCode {
			t.Fatalf("bunker status exited %d, want %d (the security-refusal code):\n%s", code, TLSRefusalExitCode, out)
		}
		if !strings.Contains(out, "OFFLINE") {
			t.Errorf("the OFFLINE section must still be printed for the operator:\n%s", out)
		}
		if !strings.Contains(out, TLSClassContradictoryConfig) {
			t.Errorf("output must name the refusal class %s:\n%s", TLSClassContradictoryConfig, out)
		}
		if !strings.Contains(out, "contradictory TLS configuration") {
			t.Errorf("output must keep the refusal wording:\n%s", out)
		}
	})

	t.Run("status --server names the same refusal", func(t *testing.T) {
		out, code := run("status", "--server", "refused")
		if code != TLSRefusalExitCode {
			t.Fatalf("bunker status --server exited %d, want %d:\n%s", code, TLSRefusalExitCode, out)
		}
		if !strings.Contains(out, TLSClassContradictoryConfig) {
			t.Errorf("output must name the refusal class:\n%s", out)
		}
	})
}

// TestStatusOfflineHostStillExitsZero pins the boundary the refusal fix must
// not move: an offline-but-TRUSTED host (a plain connection refusal, no trust
// fields involved) is a report, not a failure — `bunker status` exits 0 and
// prints OFFLINE, exactly as before DF-BUNKER-64. The refused code is only
// for trust/scheme refusals.
func TestStatusOfflineHostStillExitsZero(t *testing.T) {
	bin := buildCLIOnce(t)
	base := t.TempDir()
	cfgDir := filepath.Join(base, "xdg-config", "bunker")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A trusted plain entry: no TLS fields at all, nothing to refuse.
	cfg := "servers:\n  offline:\n    name: offline\n    url: http://127.0.0.1:1\n" +
		"    token: t\nactive_server: offline\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"HOME="+base,
		"XDG_CONFIG_HOME="+filepath.Join(base, "xdg-config"),
		"BUNKER_HOME=", "BUNKER_CONFIG_HOME=", "BUNKER_TOKEN=",
	)

	cmd := exec.Command(bin, "status")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run: %v", err)
		}
		t.Fatalf("an offline-but-trusted host must stay exit 0, got %d:\n%s%s", ee.ExitCode(), stdout.String(), stderr.String())
	}
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "OFFLINE") {
		t.Errorf("the OFFLINE report must still be printed:\n%s", out)
	}
}

// TestPlaintextToTLSDaemonIsNamedClassified proves the arm-F classification
// end to end over a real TLS listener probed with an http:// URL: the error a
// connect client actually returns must classify as TLSClassPlaintextToTLS —
// not surface as the bare `internal: 400 Bad Request`.
func TestPlaintextToTLSDaemonIsNamedClassified(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = selfSignedTLSConfig(t)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	httpURL := "http://" + srv.Listener.Addr().String()

	client := bunkerv1connect.NewBunkerdClient(&http.Client{Timeout: 5 * time.Second}, httpURL)
	_, err := client.ServerInfo(context.Background(), connect.NewRequest(&v1.ServerInfoRequest{}))
	if err == nil {
		t.Fatal("an http:// probe of a TLS daemon must fail")
	}
	if !strings.Contains(err.Error(), "400 Bad Request") {
		t.Fatalf("fixture drift: want the historical `internal: 400 Bad Request` shape in the chain, got %v", err)
	}
	class, ok := ClassifyTLSRefusal(err)
	if !ok {
		t.Fatalf("the plaintext-to-TLS shape must classify, got nothing for %v", err)
	}
	if class != TLSClassPlaintextToTLS {
		t.Fatalf("class = %q, want %q", class, TLSClassPlaintextToTLS)
	}
	if code, ok := exitCodeForTLSRefusal(err); !ok || code != TLSRefusalExitCode {
		t.Fatalf("exit code = (%d, %v), want (%d, true)", code, ok, TLSRefusalExitCode)
	}
}

// TestStatusAllTLSRefusalFails covers the overview: `bunker status --all`
// renders a refused host as an OFFLINE section (and, with no fleet
// declaration, has no coverage failure to lean on), so the refusal sweep is
// what must fail the command — a security refusal never exits 0 from any
// flag combination.
func TestStatusAllTLSRefusalFails(t *testing.T) {
	bin := buildCLIOnce(t)
	_, env := writeRefusalConfig(t)

	cmd := exec.Command(bin, "status", "--all")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run: %v", err)
		}
		code = ee.ExitCode()
	}
	out := stdout.String() + stderr.String()
	if code != TLSRefusalExitCode {
		t.Fatalf("bunker status --all exited %d, want %d:\n%s", code, TLSRefusalExitCode, out)
	}
	if !strings.Contains(out, TLSClassContradictoryConfig) {
		t.Errorf("output must name the refusal class:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// 4. The TOFU banner never claims a stored pin before the write happens.
// ---------------------------------------------------------------------------

// TestTOFUBannerNeverClaimsStoredPin is the arm-A2 acceptance: the banner's
// wording must promise only what has happened. The pin is NOT on disk when
// the banner prints — the authenticated dial and the config write both come
// after — so the banner must not say "stored".
func TestTOFUBannerNeverClaimsStoredPin(t *testing.T) {
	useTempHome(t)
	ResetConfigPathOverride()
	t.Cleanup(ResetConfigPathOverride)

	stderr := captureStderr(t, func() {
		printTrustOnFirstUse(nil, ConnectOptions{URL: "https://host:9090"}, gap141Pin)
	})
	if !strings.Contains(stderr, "TRUST ON FIRST USE") {
		t.Fatalf("banner missing:\n%s", stderr)
	}
	if strings.Contains(stderr, "\n  stored as") {
		t.Errorf("banner claims the pin is ALREADY stored — nothing is written until the "+
			"registration succeeds (dogfood arm A2):\n%s", stderr)
	}
	if !strings.Contains(stderr, "to be stored as") {
		t.Errorf("banner must say the pin is about to be stored, conditioned on the registration:\n%s", stderr)
	}
	if !strings.Contains(stderr, "ONLY IF the registration below succeeds") {
		t.Errorf("banner must name the condition explicitly:\n%s", stderr)
	}
	if !strings.Contains(stderr, FormatCertPin(gap141Pin)) {
		t.Errorf("banner must still name the fingerprint:\n%s", stderr)
	}
	if !strings.Contains(stderr, mustConfigFilePath()) {
		t.Errorf("banner must still name the resolved config path:\n%s", stderr)
	}
}

// TestAuthFailedTOFUStoresNothingAndSaysSo is the full arm-A2 reproduction:
// a first-use TLS connect with a WRONG token must (a) fail the registration,
// (b) write NOTHING to the config file, and (c) have printed no "stored"
// claim anywhere — the banner says "to be stored … ONLY IF", and the failure
// path prints no success line that would contradict it.
func TestAuthFailedTOFUStoresNothingAndSaysSo(t *testing.T) {
	home := useTempHome(t)
	srv, _ := newTLSBunkerd(t, selfSignedTLSConfig(t), &mockBunkerdServer{
		info: testServerInfo("tls-host"),
		err:  connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token")),
	})

	var stdout, stderr string
	captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			err := RegisterServerWithOptions(ConnectOptions{
				Name:    "tofu-authfail",
				URL:     srv.URL,
				Token:   "wrong-token",
				TLSMode: TLSModeSelfSigned,
			})
			if err == nil {
				t.Fatal("an auth-failed connect must fail")
			}
			if got := err.Error(); !strings.Contains(got, "unauthenticated") {
				t.Fatalf("want the auth failure surfaced, got: %s", got)
			}
		})
	})
	_ = stdout

	// (b) nothing was written — the exact assertion the dogfood run verified
	// by hand against an empty BUNKER_HOME.
	cfgPath := filepath.Join(home, ".config", "bunker", "config.yaml")
	if data, err := os.ReadFile(cfgPath); err == nil && strings.Contains(string(data), "tofu-authfail") {
		t.Fatalf("the auth-failed entry was PERSISTED to %s:\n%s", cfgPath, data)
	}
	if _, err := os.Stat(cfgPath); err == nil {
		if data, rerr := os.ReadFile(cfgPath); rerr == nil && len(bytes.TrimSpace(data)) > 0 {
			t.Fatalf("a config file was written on an auth-failed connect:\n%s", data)
		}
	}
	// (c) the banner promised conditionally, and nothing else claimed storage.
	if strings.Contains(stderr, "\n  stored as") && !strings.Contains(stderr, "to be stored as") {
		t.Errorf("stderr claimed an unconditional stored pin:\n%s", stderr)
	}
	if !strings.Contains(stderr, "to be stored as") {
		t.Errorf("the TOFU banner must still have printed (conditioned wording):\n%s", stderr)
	}

	// The in-process pin cache must not have kept the observed pin either:
	// nothing about this server was stored anywhere. (useTempHome cleared
	// the cache at test start; anything present now was cached by the failed
	// registration itself.)
	if pin := CachedCertPin(srv.URL); pin != "" {
		t.Errorf("the observed pin was cached despite the failed registration: %s", pin)
	}
}

// TestPinnedTLSHandshakeRefusalClassifies proves the handshake-delivered
// refusal (VerifyPeerCertificate) classifies through the FULL real chain:
// pinned client → real TLS listener presenting a different leaf → connect
// client. This is the B1 shape `bunker list` already exits 1 on.
func TestPinnedTLSHandshakeRefusalClassifies(t *testing.T) {
	// Server presents cert B; client pins cert A.
	srvCfg := selfSignedTLSConfig(t)
	handler := &mockBunkerdServer{info: testServerInfo("tls-host")}
	mux := http.NewServeMux()
	path, h := bunkerv1connect.NewBunkerdHandler(handler)
	mux.Handle(path, h)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = http.Serve(tls.NewListener(ln, srvCfg), mux) }()

	pinA := pinOfTLSConfig(t, selfSignedTLSConfig(t)) // deliberately NOT the server's pin
	pinned := &tls.Config{
		//nolint:gosec // the pin below is the real check
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyPinnedLeaf(pinA, "prod", rawCerts)
		},
	}
	client := bunkerv1connect.NewBunkerdClient(
		&http.Client{Transport: &http.Transport{TLSClientConfig: pinned}, Timeout: 5 * time.Second},
		"https://"+ln.Addr().String())
	_, rpcErr := client.ServerInfo(context.Background(), connect.NewRequest(&v1.ServerInfoRequest{}))
	if rpcErr == nil {
		t.Fatal("a mismatched pin must refuse the handshake")
	}

	class, ok := ClassifyTLSRefusal(rpcErr)
	if !ok {
		t.Fatalf("the pin-mismatch refusal must classify through the full chain: %v", rpcErr)
	}
	if class != TLSClassPinMismatch {
		t.Fatalf("class = %q, want %q (err: %v)", class, TLSClassPinMismatch, rpcErr)
	}
	// And the operator-facing wording survives the wrapping untouched.
	if !strings.Contains(rpcErr.Error(), "certificate pin mismatch") {
		t.Errorf("the refusal wording must reach the operator verbatim: %v", rpcErr)
	}
}

// ---------------------------------------------------------------------------
// 5. Scheme-mismatch symmetry (the observation half).
// ---------------------------------------------------------------------------

// TestFetchNotTLSErrorClassifies covers the inverse mismatch: an https://
// probe answered by a plaintext service classifies as TLSClassSchemeMismatch
// via the CertFetchError the connect path already produces.
func TestFetchNotTLSErrorClassifies(t *testing.T) {
	// A plaintext HTTP server probed with https:// → tls.RecordHeaderError →
	// CertFetchClassNotTLS.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	_, _, err := FetchLeafCertificate(context.Background(), "https://"+srv.Listener.Addr().String())
	if err == nil {
		t.Fatal("an https probe of a plaintext server must fail")
	}
	var fetchErr *CertFetchError
	if !errors.As(err, &fetchErr) || fetchErr.Class != CertFetchClassNotTLS {
		t.Fatalf("want a not-tls CertFetchError, got %v", err)
	}
	class, ok := ClassifyTLSRefusal(err)
	if !ok || class != TLSClassSchemeMismatch {
		t.Fatalf("class = (%q, %v), want (%q, true)", class, ok, TLSClassSchemeMismatch)
	}
	if code, ok := exitCodeForTLSRefusal(err); !ok || code != TLSRefusalExitCode {
		t.Fatalf("exit code = (%d, %v), want (%d, true)", code, ok, TLSRefusalExitCode)
	}
}

// guard against unused imports in future edits.
var (
	_ = io.Discard
	_ = sha256.Sum256
	_ = hex.EncodeToString
)
