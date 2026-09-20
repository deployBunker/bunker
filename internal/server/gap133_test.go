package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/deployBunker/bunker/internal/audit"
	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	"github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// gap133Logger returns a logger that discards output — the throttle's
// "auth throttle engaged" line is asserted through a capturing handler in the
// dedicated test below, not through the test log stream.
func gap133Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// gap133Server builds a real connect handler mounted on an httptest server with
// the given auth interceptor (plus the audit interceptor when a log is given),
// exactly as server.go composes them. It returns the server and a client.
func gap133Server(t *testing.T, interceptor connect.Interceptor, log *audit.AuditLog) (*httptest.Server, bunkerv1connect.BunkerdClient) {
	t.Helper()
	logger := gap133Logger()
	mux := http.NewServeMux()
	interceptors := []connect.Interceptor{interceptor}
	if log != nil {
		interceptors = append(interceptors, audit.NewInterceptor(log, logger))
	}
	path, handler := bunkerv1connect.NewBunkerdHandler(
		&bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: resource.NewTracker(10, logger)},
		connect.WithInterceptors(interceptors...),
	)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
}

// callServerInfo performs one unauthenticated-token-tolerant ServerInfo call
// with the given Authorization value ("" omits the header).
func callServerInfo(ctx context.Context, c bunkerv1connect.BunkerdClient, authz string) error {
	req := connect.NewRequest(&v1.ServerInfoRequest{})
	if authz != "" {
		req.Header().Set("Authorization", authz)
	}
	_, err := c.ServerInfo(ctx, req)
	return err
}

// readAuditRecords parses the audit log into Records.
func readAuditRecords(t *testing.T, path string) []audit.Record {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var recs []audit.Record
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r audit.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("audit line is not valid JSON: %v (%s)", err, line)
		}
		recs = append(recs, r)
	}
	return recs
}

// TestGAP133_DenialsReachTheAuditChain is acceptance criteria 1 and 3: a
// denial arrives through the real connect stack as an audit-chain record
// carrying source + token fingerprint + outcome; the interleaved
// allowed/denied trail still passes the chain's own integrity check; and no
// byte of the log contains the raw presented token.
func TestGAP133_DenialsReachTheAuditChain(t *testing.T) {
	const goodToken = "gap133-good-token-value"
	const badToken = "gap133-attacker-token"

	logPath := filepath.Join(t.TempDir(), "audit.log")
	log, err := audit.New(logPath)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	interceptor := auth.NewAuthInterceptor(goodToken, true)
	sink := &authDenySink{log: log}
	auth.AttachDenySink(interceptor, sink.record)

	_, client := gap133Server(t, interceptor, log)
	ctx := context.Background()

	// Interleave: allowed, denied, allowed, denied, denied.
	steps := []struct {
		authz    string
		wantCode connect.Code
	}{
		{authz: "Bearer " + goodToken},
		{authz: "Bearer " + badToken, wantCode: connect.CodeUnauthenticated},
		{authz: "Bearer " + goodToken},
		{authz: "", wantCode: connect.CodeUnauthenticated},
		{authz: "Bearer " + badToken, wantCode: connect.CodeUnauthenticated},
	}
	for i, st := range steps {
		err := callServerInfo(ctx, client, st.authz)
		if st.wantCode == 0 {
			if err != nil {
				t.Fatalf("step %d (allowed): unexpected error %v", i, err)
			}
			continue
		}
		if got := connect.CodeOf(err); got != st.wantCode {
			t.Fatalf("step %d: code = %v, want %v", i, got, st.wantCode)
		}
	}

	recs := readAuditRecords(t, logPath)

	// Every denial produced exactly one "denied" record with source + fp.
	var denied []audit.Record
	for _, r := range recs {
		if r.Outcome == "denied" {
			denied = append(denied, r)
		}
	}
	if len(denied) != 3 {
		t.Fatalf("denied records = %d, want 3 (denials: bad token, missing header, bad token); got %+v",
			len(denied), recs)
	}
	wantFp := auth.FingerprintToken(badToken)
	var sawFp, sawMissingHeader bool
	for _, r := range denied {
		if r.Method != "/bunker.v1.Bunkerd/ServerInfo" {
			t.Errorf("denied record method = %q, want the procedure", r.Method)
		}
		if r.RemoteAddr == "" || r.RemoteAddr == "unknown" {
			t.Errorf("denied record remote_addr = %q, want the client address", r.RemoteAddr)
		}
		if r.Caller != "static-token" {
			t.Errorf("denied record caller = %q, want the static-token label", r.Caller)
		}
		if !strings.HasPrefix(r.Summary, "auth denied: ") {
			t.Errorf("denied record summary = %q, want the auth-denied prefix", r.Summary)
		}
		if strings.Contains(r.Summary, "token fp="+wantFp) {
			sawFp = true
		}
		if strings.Contains(r.Summary, "missing authorization header") {
			sawMissingHeader = true
		}
	}
	if !sawFp {
		t.Errorf("no denial record carried the presented token's fingerprint %q: %+v", wantFp, denied)
	}
	if !sawMissingHeader {
		t.Errorf("the missing-header denial was not recorded distinctly: %+v", denied)
	}

	// Allowed records still ride the chain too (interleaving is real).
	var allowed int
	for _, r := range recs {
		if r.Outcome == "ok" {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("allowed records = %d, want 2 — denials must not have displaced them", allowed)
	}

	// Secret hygiene (acceptance 1): neither the presented attacker token nor
	// the raw valid token appears anywhere in the log bytes.
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read raw audit log: %v", err)
	}
	for _, secret := range []string{badToken, goodToken} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("audit log contains the raw token %q — denials must record only the fingerprint", secret)
		}
	}

	// Acceptance 3: the chain's OWN integrity check still passes.
	n, firstBad, err := audit.Verify(logPath)
	if err != nil {
		t.Fatalf("audit.Verify after interleaved denials: %v (firstBad=%d)", err, firstBad)
	}
	if n != len(recs) {
		t.Errorf("Verify counted %d records, parsed %d", n, len(recs))
	}
	if firstBad != 0 {
		t.Errorf("firstBad = %d, want 0", firstBad)
	}
	// Hash linkage is real, not vacuous: every record after the first chains.
	for i, r := range recs {
		if i == 0 {
			if r.PrevHash != "" {
				t.Errorf("first record prev_hash = %q, want empty (genesis)", r.PrevHash)
			}
			continue
		}
		if r.PrevHash != recs[i-1].Hash {
			t.Errorf("record %d prev_hash %q does not chain to %q", i, r.PrevHash, recs[i-1].Hash)
		}
	}
}

// TestGAP133_ThrottleFiresAndRecovers is acceptance criterion 2 driven through
// the REAL connect stack: 5 failed auths from one client, the 6th is throttled
// with CodeUnavailable and a loud log line, a legitimate client from another
// source is unaffected, and a success from the throttled source resets it.
func TestGAP133_ThrottleFiresAndRecovers(t *testing.T) {
	const goodToken = "gap133-throttle-good-token"

	logPath := filepath.Join(t.TempDir(), "audit.log")
	log, err := audit.New(logPath)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	// Capture the throttle's log line.
	var logBuf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	interceptor := auth.NewAuthInterceptor(goodToken, true)
	sink := &authDenySink{log: log}
	auth.AttachDenySink(interceptor, sink.record)

	_, attacker := gap133Server(t, interceptor, log)
	_, legit := gap133Server(t, interceptor, nil)
	ctx := context.Background()

	// 5 failures inside the window: all plain Unauthenticated.
	for i := 1; i <= auth.ThrottleFailureThreshold; i++ {
		if got := connect.CodeOf(callServerInfo(ctx, attacker, "Bearer wrong-token")); got != connect.CodeUnauthenticated {
			t.Fatalf("failure %d: code = %v, want Unauthenticated", i, got)
		}
	}
	// The 6th unauthenticated request from that source is throttled.
	if got := connect.CodeOf(callServerInfo(ctx, attacker, "Bearer wrong-token")); got != connect.CodeUnavailable {
		t.Fatalf("6th request: code = %v, want Unavailable (throttled)", got)
	}
	// ... and the throttle is logged loudly.
	if !strings.Contains(logBuf.String(), "auth throttle engaged") {
		t.Fatalf("no 'auth throttle engaged' log line found; got:\n%s", logBuf.String())
	}

	// A legitimate client from a different source is unaffected.
	if err := callServerInfo(ctx, legit, "Bearer "+goodToken); err != nil {
		t.Fatalf("legit client from another source was affected: %v", err)
	}

	// A SUCCESS from the throttled source resets its counter. The throttle is
	// per-source, and each httptest client keeps one keep-alive connection, so
	// the attacker's client address is stable — a valid token on that same
	// client must be accepted and must clear the backoff.
	if err := callServerInfo(ctx, attacker, "Bearer "+goodToken); err != nil {
		t.Fatalf("success from the throttled source was rejected: %v", err)
	}
	// After the reset the next bad attempt is a plain denial again (proof the
	// source is no longer in backoff).
	if got := connect.CodeOf(callServerInfo(ctx, attacker, "Bearer wrong-token")); got != connect.CodeUnauthenticated {
		t.Fatalf("after reset: code = %v, want Unauthenticated (counter cleared)", got)
	}

	// The throttled denial is visible in the chain too.
	var throttled int
	for _, r := range readAuditRecords(t, logPath) {
		if strings.Contains(r.Summary, "(throttled)") {
			throttled++
		}
	}
	if throttled == 0 {
		t.Error("the throttled denial never reached the audit chain")
	}
	if _, firstBad, err := audit.Verify(logPath); err != nil || firstBad != 0 {
		t.Fatalf("audit chain broken by the throttle (firstBad=%d): %v", firstBad, err)
	}
}

// TestGAP133_AuthDisabledRecordsNothing proves the sink is inert (and the
// service works) when auth is disabled: NoAuth has no denial path.
func TestGAP133_AuthDisabledRecordsNothing(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.log")
	log, err := audit.New(logPath)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	interceptor := auth.NewAuthInterceptor("", false) // disabled
	sink := &authDenySink{log: log}
	auth.AttachDenySink(interceptor, sink.record)

	_, client := gap133Server(t, interceptor, log)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := callServerInfo(ctx, client, ""); err != nil {
		t.Fatalf("auth-disabled request failed: %v", err)
	}
	for _, r := range readAuditRecords(t, logPath) {
		if r.Outcome == "denied" {
			t.Fatalf("auth disabled but a denial record was written: %+v", r)
		}
	}
}
