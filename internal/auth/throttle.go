package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Throttle constants (GAP-133 / SEC-15): per-source backoff on
// UNAUTHENTICATED requests. Defaults are hardcoded by design — no config
// schema changes in this task.
const (
	// ThrottleFailureThreshold is how many failed auths from one source
	// inside ThrottleFailureWindow trigger throttling of that source.
	ThrottleFailureThreshold = 5
	// ThrottleFailureWindow is the sliding window in which consecutive
	// failures accumulate toward ThrottleFailureThreshold.
	ThrottleFailureWindow = 60 * time.Second
	// ThrottleMaxBackoff caps the exponential backoff applied to a
	// throttled source (2^k seconds, k = consecutive throttled hits).
	ThrottleMaxBackoff = 30 * time.Second
)

// DenyEvent is one authentication denial. It deliberately carries NO token
// material: Token is the presented token's FINGERPRINT only — the first 12
// hex characters of SHA-256 over the raw presented token — so the audit trail
// can correlate attempts without ever recording the secret.
type DenyEvent struct {
	// RemoteAddr is the client address the denial came from ("unknown"
	// when the transport does not expose a peer address).
	RemoteAddr string
	// Procedure is the full connect procedure, e.g. /bunker.v1.Bunkerd/SpawnAgent.
	Procedure string
	// Reason is a short non-secret denial reason ("missing authorization
	// header", "invalid token", ...).
	Reason string
	// TokenFingerprint is "sha256:<12 hex chars>" of the presented token,
	// "" when no token was presented (missing/malformed header).
	TokenFingerprint string
}

// DenyFunc receives every authentication denial. Implementations must be
// safe for concurrent use (interceptors call it on the request path) and
// must not block unnecessarily — denial handling shares the auth hot path.
type DenyFunc func(DenyEvent)

// FingerprintToken derives the non-secret fingerprint recorded in denial
// events: first 12 hex chars of SHA-256 over the raw token. An empty token
// yields "" (nothing was presented).
func FingerprintToken(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// sourceState is the per-source throttle state.
type sourceState struct {
	failCount    int
	lastFail     time.Time
	backoffUntil time.Time
	throttleHits int // consecutive throttled hits; drives 2^k backoff growth
}

// throttleState tracks failed auths per source address and applies
// exponential backoff to UNAUTHENTICATED requests from offending sources.
// In-process only (GAP-133): a restart clears state, which is acceptable —
// the audit trail keeps the visible history.
type throttleState struct {
	mu      sync.Mutex
	sources map[string]*sourceState
}

func newThrottleState() *throttleState {
	return &throttleState{sources: make(map[string]*sourceState)}
}

// allow decides whether an unauthenticated request from source should be
// served the normal auth path (true) or refused with backoff (false). When
// refused, backoff reports how long the source must wait before its next
// attempt — the caller turns this into CodeUnavailable + a loud log line.
// A return of backoff==0 with allow==false is not possible; the backoff is
// always positive when throttled.
func (t *throttleState) allow(source string, now time.Time) (allow bool, backoff time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.sources[source]
	if !ok {
		return true, 0
	}
	// Failures older than the window decay: a source that went quiet for
	// ThrottleFailureWindow is treated as fresh again.
	if !st.lastFail.IsZero() && now.Sub(st.lastFail) > ThrottleFailureWindow {
		st.failCount = 0
		st.throttleHits = 0
	}
	if now.Before(st.backoffUntil) {
		return false, st.backoffUntil.Sub(now)
	}
	return true, 0
}

// recordFailure counts one failed auth from source. When the failure count
// within the window reaches ThrottleFailureThreshold, the source enters
// backoff for 2^k seconds (k = consecutive throttled violations), capped at
// ThrottleMaxBackoff.
func (t *throttleState) recordFailure(source string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.sources[source]
	if !ok {
		st = &sourceState{}
		t.sources[source] = st
	}
	// Window expiry resets the counter before this failure is counted.
	if !st.lastFail.IsZero() && now.Sub(st.lastFail) > ThrottleFailureWindow {
		st.failCount = 0
		st.throttleHits = 0
	}
	st.failCount++
	st.lastFail = now
	if st.failCount >= ThrottleFailureThreshold {
		st.throttleHits++
		d := time.Duration(1<<uint(st.throttleHits)) * time.Second // 2^k seconds
		if d > ThrottleMaxBackoff {
			d = ThrottleMaxBackoff
		}
		st.backoffUntil = now.Add(d)
	}
}

// recordSuccess clears all throttle state for source: a successful auth
// proves the client is legitimate, so its next bad attempt starts from zero.
func (t *throttleState) recordSuccess(source string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sources, source)
}
