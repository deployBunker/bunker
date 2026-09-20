package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
)

// denyRecorder is a concurrency-safe test DenyFunc.
type denyRecorder struct {
	mu     sync.Mutex
	events []DenyEvent
}

func (r *denyRecorder) sink(ev DenyEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *denyRecorder) snapshot() []DenyEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]DenyEvent, len(r.events))
	copy(out, r.events)
	return out
}

// mustUnary runs the wrapped unary chain once with the given Authorization
// header value ("" removes the header) and returns the error.
func mustUnary(t *testing.T, interceptor connect.Interceptor, authz string) error {
	t.Helper()
	next := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, nil
	}
	req := connect.NewRequest(&dummyMsg{})
	if authz == "" {
		req.Header().Del("Authorization")
	} else {
		req.Header().Set("Authorization", authz)
	}
	_, err := interceptor.WrapUnary(next)(context.Background(), req)
	return err
}

// hdr builds a header map with the given Authorization value ("" omits it).
func hdr(authz string) http.Header {
	h := http.Header{}
	if authz != "" {
		h.Set("Authorization", authz)
	}
	return h
}

// TestDenySink_RecordsEveryDenialShape is the GAP-133 acceptance criterion 1
// table: every denial shape yields exactly one DenyEvent carrying source +
// token fingerprint + reason, and no recorded byte is ever the raw token.
func TestDenySink_RecordsEveryDenialShape(t *testing.T) {
	const good = "correct-token-value"
	const bad = "attacker-supplied-token"
	const src = "203.0.113.7:40123"

	tests := []struct {
		name       string
		authz      string
		wantReason string
		wantFp     string
		wantNoFp   bool
	}{
		{
			name:       "missing header",
			authz:      "",
			wantReason: "missing authorization header",
			wantNoFp:   true,
		},
		{
			name:       "bad format",
			authz:      "Token abc",
			wantReason: "invalid authorization header format",
			wantNoFp:   true,
		},
		{
			name:       "invalid token",
			authz:      "Bearer " + bad,
			wantReason: "invalid token",
			wantFp:     FingerprintToken(bad),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &denyRecorder{}
			a := NewTokenAuth(good)
			a.SetDenySink(rec.sink)

			err := a.authenticate(hdr(tc.authz), src, "/bunker.v1.Bunkerd/ServerInfo")
			if err == nil {
				t.Fatal("expected denial error, got nil")
			}
			if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
				t.Fatalf("code = %v, want Unauthenticated", got)
			}

			evs := rec.snapshot()
			if len(evs) != 1 {
				t.Fatalf("deny events = %d, want exactly 1: %+v", len(evs), evs)
			}
			ev := evs[0]
			if ev.RemoteAddr != src {
				t.Errorf("source = %q, want %q", ev.RemoteAddr, src)
			}
			if ev.Procedure != "/bunker.v1.Bunkerd/ServerInfo" {
				t.Errorf("procedure = %q, want the request procedure", ev.Procedure)
			}
			if !strings.Contains(ev.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", ev.Reason, tc.wantReason)
			}
			if tc.wantNoFp {
				if ev.TokenFingerprint != "" {
					t.Errorf("fingerprint = %q, want empty when no token was presented", ev.TokenFingerprint)
				}
			} else if ev.TokenFingerprint != tc.wantFp {
				t.Errorf("fingerprint = %q, want %q", ev.TokenFingerprint, tc.wantFp)
			}
			// Secret hygiene: the raw presented token must never appear in a
			// recorded event; the fingerprint is the sha256-truncated form.
			dump := fmt.Sprintf("%+v", ev)
			if strings.Contains(dump, bad) {
				t.Errorf("denial event leaked the raw presented token: %s", dump)
			}
			if !tc.wantNoFp && !strings.HasPrefix(ev.TokenFingerprint, "sha256:") {
				t.Errorf("fingerprint %q is not in sha256:<12 hex> form", ev.TokenFingerprint)
			}
		})
	}
}

// TestDenySink_NotCalledOnSuccess proves a valid token never touches the deny
// path (legit clients are unaffected by construction).
func TestDenySink_NotCalledOnSuccess(t *testing.T) {
	rec := &denyRecorder{}
	a := NewTokenAuth("good-token")
	a.SetDenySink(rec.sink)

	if err := mustUnary(t, a, "Bearer good-token"); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if evs := rec.snapshot(); len(evs) != 0 {
		t.Fatalf("deny sink called on a successful auth: %+v", evs)
	}
}

// TestThrottle_BlocksAfterThresholdAndResetsOnSuccess is the GAP-133
// acceptance criterion 2 table: 5 failures from one source inside the window
// make the 6th unauthenticated request throttled (logged), a legitimate client
// from a different source is unaffected, and a subsequent SUCCESS from the
// throttled source resets it.
func TestThrottle_BlocksAfterThresholdAndResetsOnSuccess(t *testing.T) {
	const good = "correct-token"
	const src = "203.0.113.7:40123"
	const other = "203.0.113.8:40124"

	t.Run("threshold then backoff then recovery", func(t *testing.T) {
		rec := &denyRecorder{}
		a := NewTokenAuth(good)
		a.SetDenySink(rec.sink)

		// Failures 1..Threshold are plain Unauthenticated denials; the last of
		// them arms the backoff.
		for i := 1; i <= ThrottleFailureThreshold; i++ {
			err := a.authenticate(hdr("Bearer wrong"), src, "/bunker.v1.Bunkerd/ServerInfo")
			if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
				t.Fatalf("failure %d: code = %v, want Unauthenticated before the 6th request", i, got)
			}
		}

		// The 6th unauthenticated request from that source is throttled.
		err := a.authenticate(hdr("Bearer wrong"), src, "/bunker.v1.Bunkerd/ServerInfo")
		if got := connect.CodeOf(err); got != connect.CodeUnavailable {
			t.Fatalf("6th request: code = %v, want Unavailable (throttled)", got)
		}

		// The throttle is visible in the deny sink.
		var throttled int
		for _, ev := range rec.snapshot() {
			if strings.Contains(ev.Reason, "(throttled)") {
				throttled++
				if ev.RemoteAddr != src {
					t.Errorf("throttled event source = %q, want %q", ev.RemoteAddr, src)
				}
			}
		}
		if throttled == 0 {
			t.Fatal("no throttled denial event recorded — the throttle was not logged")
		}

		// A legitimate client from a DIFFERENT source is unaffected.
		if err := a.authenticate(hdr("Bearer "+good), other, "/bunker.v1.Bunkerd/ServerInfo"); err != nil {
			t.Fatalf("legit client from another source was affected: %v", err)
		}

		// A SUCCESS from the throttled source itself resets its state.
		if err := a.authenticate(hdr("Bearer "+good), src, "/bunker.v1.Bunkerd/ServerInfo"); err != nil {
			t.Fatalf("success from throttled source rejected: %v", err)
		}
		// After the reset the next bad attempt is a plain denial again.
		if got := connect.CodeOf(a.authenticate(hdr("Bearer wrong"), src, "/bunker.v1.Bunkerd/ServerInfo")); got != connect.CodeUnauthenticated {
			t.Fatalf("after reset: code = %v, want Unauthenticated (counter cleared)", got)
		}
	})

	t.Run("window expiry clears the counter", func(t *testing.T) {
		st := newThrottleState()
		now := time.Now()
		for i := 0; i < ThrottleFailureThreshold; i++ {
			st.recordFailure("src", now)
		}
		if allow, _ := st.allow("src", now); allow {
			t.Fatal("source should be throttled immediately after the threshold")
		}
		// Advancing past the window makes the source fresh again.
		later := now.Add(ThrottleFailureWindow + time.Second)
		if allow, _ := st.allow("src", later); !allow {
			t.Fatal("source should not be throttled after the failure window expired")
		}
		// The failure arriving then starts a NEW streak (one is not enough).
		st.recordFailure("src", later)
		if allow, _ := st.allow("src", later); !allow {
			t.Fatal("a single failure after window expiry must not throttle")
		}
	})

	t.Run("backoff is exponential and capped", func(t *testing.T) {
		st := newThrottleState()
		now := time.Now()
		for i := 0; i < ThrottleFailureThreshold; i++ {
			st.recordFailure("src", now)
		}
		_, first := st.allow("src", now)
		if first <= 0 || first > ThrottleMaxBackoff {
			t.Fatalf("first backoff = %v, want 0 < d <= %v", first, ThrottleMaxBackoff)
		}
		for i := 0; i < 10; i++ {
			st.recordFailure("src", now)
			allow, d := st.allow("src", now)
			if allow {
				t.Fatal("source unexpectedly allowed while inside backoff")
			}
			if d > ThrottleMaxBackoff {
				t.Fatalf("backoff %v exceeds the cap %v", d, ThrottleMaxBackoff)
			}
			if d < first && i > 0 {
				t.Fatalf("backoff shrank from %v to %v", first, d)
			}
		}
	})
}

// TestFingerprintToken pins the fingerprint contract: deterministic, truncated
// to 12 hex chars, prefixed, and never the token itself.
func TestFingerprintToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{name: "empty", token: "", want: ""},
		{name: "known value", token: "hunter2", want: "sha256:" + "f52fbd32b2b3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FingerprintToken(tc.token)
			if got != tc.want {
				t.Fatalf("FingerprintToken(%q) = %q, want %q", tc.token, got, tc.want)
			}
			if tc.token != "" {
				if strings.Contains(got, tc.token) {
					t.Fatalf("fingerprint %q contains the raw token", got)
				}
				if len(got) != len("sha256:")+12 {
					t.Fatalf("fingerprint %q is not 12 hex chars after the prefix", got)
				}
			}
			if again := FingerprintToken(tc.token); again != got {
				t.Fatalf("fingerprint is not deterministic: %q then %q", got, again)
			}
		})
	}
}

// TestJWTAuth_DenySink_RecordsDenials proves the JWT interceptor (the one the
// Agent service uses) also records denials.
func TestJWTAuth_DenySink_RecordsDenials(t *testing.T) {
	const src = "198.51.100.4:2200"
	rec := &denyRecorder{}
	a := NewJWTAuth("jwt-secret-value", nil)
	a.SetDenySink(rec.sink)

	_, err := a.authenticate(hdr("Bearer not-a-jwt"), src, "/bunker.v1.Agent/HeartbeatAgent")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
	evs := rec.snapshot()
	if len(evs) != 1 {
		t.Fatalf("deny events = %d, want 1", len(evs))
	}
	if evs[0].TokenFingerprint != FingerprintToken("not-a-jwt") {
		t.Errorf("fingerprint = %q, want the presented token's fingerprint", evs[0].TokenFingerprint)
	}
	if evs[0].Procedure != "/bunker.v1.Agent/HeartbeatAgent" {
		t.Errorf("procedure = %q, want the request procedure", evs[0].Procedure)
	}
	if strings.Contains(fmt.Sprintf("%+v", evs[0]), "not-a-jwt") {
		t.Error("denial event leaked the raw presented token")
	}

	// Missing header on the JWT path is also recorded, with no fingerprint.
	rec2 := &denyRecorder{}
	b := NewJWTAuth("jwt-secret-value", nil)
	b.SetDenySink(rec2.sink)
	if _, err := b.authenticate(hdr(""), src, "/bunker.v1.Agent/HeartbeatAgent"); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("missing header: code = %v, want Unauthenticated", connect.CodeOf(err))
	}
	if evs := rec2.snapshot(); len(evs) != 1 || evs[0].TokenFingerprint != "" {
		t.Fatalf("missing-header denial not recorded correctly: %+v", evs)
	}
}

// TestJWTAuth_StaticFallbackSucceeds proves a valid static token does not touch
// the deny path on the JWT interceptor.
func TestJWTAuth_StaticFallbackSucceeds(t *testing.T) {
	rec := &denyRecorder{}
	a := NewJWTAuthWithStaticFallback("secret", "static-good", nil)
	a.SetDenySink(rec.sink)

	if _, err := a.authenticate(hdr("Bearer static-good"), "10.0.0.1:1", "/x/Y"); err != nil {
		t.Fatalf("static token rejected: %v", err)
	}
	if evs := rec.snapshot(); len(evs) != 0 {
		t.Fatalf("deny sink called on a successful static-token auth: %+v", evs)
	}
}

// TestAttachDenySink_FactoryResults wires the sink through the helper server.go
// uses: every factory result that can deny must accept it, and NoAuth (auth
// disabled) must be a harmless no-op.
func TestAttachDenySink_FactoryResults(t *testing.T) {
	tests := []struct {
		name        string
		interceptor connect.Interceptor
		authz       string
		wantDenials int
	}{
		{
			name:        "static token interceptor",
			interceptor: NewAuthInterceptor("real-token", true),
			authz:       "Bearer wrong-token",
			wantDenials: 1,
		},
		{
			name:        "jwt with static fallback",
			interceptor: NewJWTAuthInterceptor("secret", nil, "real-token", true),
			authz:       "Bearer wrong-token",
			wantDenials: 1,
		},
		{
			name:        "master-only jwt",
			interceptor: NewMasterOnlyAuthInterceptor("secret", nil, "real-token", true),
			authz:       "Bearer wrong-token",
			wantDenials: 1,
		},
		{
			name:        "auth disabled (NoAuth)",
			interceptor: NewAuthInterceptor("", false),
			authz:       "Bearer anything",
			wantDenials: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &denyRecorder{}
			i := AttachDenySink(tc.interceptor, rec.sink)
			_ = mustUnary(t, i, tc.authz)
			if got := len(rec.snapshot()); got != tc.wantDenials {
				t.Fatalf("denials recorded = %d, want %d", got, tc.wantDenials)
			}
		})
	}
}

// TestSetDenySink_NilDetaches proves the sink can be detached without changing
// the plain denial behaviour.
func TestSetDenySink_NilDetaches(t *testing.T) {
	rec := &denyRecorder{}
	a := NewTokenAuth("real")
	a.SetDenySink(rec.sink)
	a.SetDenySink(nil)

	err := mustUnary(t, a, "Bearer wrong")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want Unauthenticated after detach", connect.CodeOf(err))
	}
	if evs := rec.snapshot(); len(evs) != 0 {
		t.Fatalf("detached sink still received events: %+v", evs)
	}
}

// TestDenySink_StreamingCarriesPeer proves the streaming path (which reads the
// peer from the conn) reports the same source + fingerprint + procedure.
func TestDenySink_StreamingCarriesPeer(t *testing.T) {
	rec := &denyRecorder{}
	a := NewTokenAuth("real")
	a.SetDenySink(rec.sink)

	conn := &mockStreamingConn{header: http.Header{"Authorization": []string{"Bearer wrong"}}}
	err := a.WrapStreamingHandler(func(context.Context, connect.StreamingHandlerConn) error {
		return nil
	})(context.Background(), conn)
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
	evs := rec.snapshot()
	if len(evs) != 1 {
		t.Fatalf("deny events = %d, want 1", len(evs))
	}
	if evs[0].RemoteAddr != "127.0.0.1:1234" {
		t.Errorf("source = %q, want the conn peer address", evs[0].RemoteAddr)
	}
	if evs[0].Procedure != "/test.Service/Stream" {
		t.Errorf("procedure = %q, want the stream spec procedure", evs[0].Procedure)
	}
	if evs[0].TokenFingerprint != FingerprintToken("wrong") {
		t.Errorf("fingerprint = %q, want the presented token's fingerprint", evs[0].TokenFingerprint)
	}
}

// TestPeerAddr_UnknownFallback pins the "unknown" fallback for transports that
// expose no peer address.
func TestPeerAddr_UnknownFallback(t *testing.T) {
	if got := peerAddr(connect.Peer{}); got != "unknown" {
		t.Errorf("peerAddr(empty) = %q, want unknown", got)
	}
	if got := peerAddr(connect.Peer{Addr: "1.2.3.4:5"}); got != "1.2.3.4:5" {
		t.Errorf("peerAddr = %q, want the address", got)
	}
}

// TestThrottle_ConcurrentAccess proves the per-source state is race-free under
// concurrent denials (run with -race).
func TestThrottle_ConcurrentAccess(t *testing.T) {
	st := newThrottleState()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			src := fmt.Sprintf("10.0.0.%d:1", n%4)
			st.recordFailure(src, time.Now())
			st.allow(src, time.Now())
			st.recordSuccess(src)
		}(i)
	}
	wg.Wait()
}

// errUnused keeps the errors import honest if future cases drop their use.
var _ = errors.New
