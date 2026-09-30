package agent

import (
	"os/user"
	"sync/atomic"
	"testing"
	"time"
)

// ── PERF-007: the linger plane's user-lookup cost is memoised ───────────────
//
// ServerInfo resolves through bunkerdService.residueInventory →
// AgentManager.ResidueInventory on EVERY call (bunker connect, health checks,
// preflights), and the linger plane classified every entry through one
// lookupUser (user.Lookup — a getpwnam_r per name) per entry: measured on
// bunker-mvp, 724 linger entries cost ~230ms per ServerInfo — uncached — for a
// read-only status RPC. The manager now classifies entries through a short-TTL
// cache (residueLingerCacheTTL). These tests pin the contract with a COUNTING
// lookupUser seam:
//
//   - the first probe pays full price (freshness at call 1);
//   - a probe inside the TTL window invokes lookupUser no further;
//   - a probe after TTL expiry re-invokes it and applies the fresh verdict,
//     so a user that appeared while the verdict sat cached is no longer
//     counted stale;
//   - INSIDE the window a flipped verdict is NOT observed — the documented
//     staleness bound, pinned as behaviour rather than promised in prose;
//   - a hand-built manager without a cache (nil) classifies directly, the
//     pre-PERF-007 behaviour.

// countingUserProbe swaps the lookupUser seam for a stub that resolves exactly
// the names currently in *real and counts every call; the counter is returned
// so a test can assert how many user-database lookups a probe sequence paid.
// The slice is pointed at (not copied) so a test can flip a user's existence
// mid-sequence. Restored via t.Cleanup, like every other lookupUser swap in
// this package.
func countingUserProbe(t *testing.T, real *[]string) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	orig := lookupUser
	lookupUser = func(name string) (*user.User, error) {
		calls.Add(1)
		if real != nil {
			for _, r := range *real {
				if r == name {
					return &user.User{Username: name, Uid: "61001", HomeDir: "/home/" + name}, nil
				}
			}
		}
		return nil, user.UnknownUserError(name)
	}
	t.Cleanup(func() { lookupUser = orig })
	return &calls
}

// The fix's core: the SECOND residue probe inside the TTL window — the
// ServerInfo repeat call bunker connect, health checks and preflights make —
// must not consult the user database again.
func TestResidueInventoryLingerCacheReusesLookupWithinTTL(t *testing.T) {
	f := newResidueFixture(t)
	f.addLingerEntry(t, "bunker-stale-a")
	f.addLingerEntry(t, "bunker-stale-b")
	f.addLingerEntry(t, "bunter-probe-cached")
	calls := countingUserProbe(t, nil)

	// First probe: full price — every non-exempt entry goes through the seam.
	if got := f.m.ResidueInventory(); got.StaleLinger != 3 {
		t.Fatalf("StaleLinger = %d, want 3 (three entries, no users behind them)", got.StaleLinger)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("first probe made %d lookupUser calls, want 3 (freshness at call 1)", got)
	}

	// Second probe, immediately: the verdicts are reused, so the seam is NOT
	// re-invoked and the counts are unchanged.
	if got := f.m.ResidueInventory(); got.StaleLinger != 3 {
		t.Fatalf("StaleLinger = %d, want 3 on the immediate second probe", got.StaleLinger)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("second probe within the TTL window made %d lookupUser calls, want 3 (the cache must serve the repeat call without new lookups)", got)
	}
}

// Expiry: once the TTL clock passes the verdict's age, the probe pays the
// lookup again and the FRESH classification is applied.
func TestResidueInventoryLingerCacheExpiresAndAppliesFreshVerdict(t *testing.T) {
	f := newResidueFixture(t)
	f.addLingerEntry(t, "bunter-probe-exp")
	real := []string(nil)
	calls := countingUserProbe(t, &real)

	if got := f.m.ResidueInventory().StaleLinger; got != 1 {
		t.Fatalf("StaleLinger = %d, want 1 while the user is absent", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("first probe made %d lookupUser calls, want 1", got)
	}

	// The user comes into existence, but only after the TTL window has passed
	// on the cache's own clock: the next probe must re-consult the seam and
	// stop counting the entry stale.
	real = []string{"bunter-probe-exp"}
	f.m.lingerUsers.clock = func() time.Time {
		return time.Now().Add(residueLingerCacheTTL + time.Second)
	}
	if got := f.m.ResidueInventory().StaleLinger; got != 0 {
		t.Fatalf("StaleLinger = %d, want 0 after TTL expiry with the user now present", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("post-expiry probe made %d lookupUser calls, want 2 (an expired verdict must be re-looked-up, not trusted)", got)
	}
}

// The documented staleness bound, pinned: a user that appears MID-window is
// not observed until the TTL expires — the reported residue may be up to
// residueLingerCacheTTL stale, and no lookup is spent re-checking a verdict
// that is still young.
func TestResidueInventoryLingerCacheServesStaleVerdictWithinTTL(t *testing.T) {
	f := newResidueFixture(t)
	f.addLingerEntry(t, "bunter-probe-stale")
	real := []string(nil)
	calls := countingUserProbe(t, &real)

	if got := f.m.ResidueInventory().StaleLinger; got != 1 {
		t.Fatalf("StaleLinger = %d, want 1 while the user is absent", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("first probe made %d lookupUser calls, want 1", got)
	}

	// The user appears mid-window; the clock stays inside the TTL.
	real = []string{"bunter-probe-stale"}
	if got := f.m.ResidueInventory().StaleLinger; got != 1 {
		t.Fatalf("StaleLinger = %d, want 1: inside the TTL window a mid-window flip must not be observed yet (the documented staleness bound)", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("mid-window probe made %d lookupUser calls, want 1 (a young verdict is served, not re-checked)", got)
	}
}

// Nil-cache degradation: a hand-built manager (struct literal, no
// NewAgentManager) carries no cache and must classify every entry through the
// seam directly — the pre-PERF-007 behaviour, never a panic.
func TestLingerUserCacheNilReceiverClassifiesDirectly(t *testing.T) {
	real := []string{"bunker-real"}
	calls := countingUserProbe(t, &real)
	var c *lingerUserCache
	if !c.userExists("bunker-real", lookupUser) {
		t.Fatalf("a present user must classify as existing through the nil-cache fallback")
	}
	if c.userExists("bunker-gone", lookupUser) {
		t.Fatalf("an absent user must classify as gone through the nil-cache fallback")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("nil-cache fallback made %d lookupUser calls, want 2 (direct, uncached)", got)
	}
}
