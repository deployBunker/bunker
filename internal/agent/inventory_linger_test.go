package agent

import (
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"testing"
)

// ── INT-HOST-004: the linger plane must not be name-scoped ─────────────────
//
// The residue inventory counted stale linger entries through the managed-id
// listing (listAgentDirEntries with agentUserPrefix), which silently dropped
// every entry not named bunker-<valid-id>. Measured on bunker-mvp (filing,
// 2026-09-18): the daemon reported 50 stale linger entries while host-local
// `bunker linger` counted 51 at the same instant — a stale entry named
// `bunter-1d2b6cef` (a typo for bunker-) was invisible to the count even
// though logind acts on it. These tests pin the fixed contract: the plane
// scans the raw directory and classifies by USER EXISTENCE (the same seam the
// destroy/rollback path uses), with a managed entry the daemon still knows
// exempted before the lookup.

// lingerSwapUserProbe swaps the lookupUser seam (manager_destroy.go) for fn for
// the duration of one test, restored via t.Cleanup. The residue linger plane
// classifies entries through that SAME user-existence probe, so tests stub it
// instead of probing the ambient /etc/passwd — a stub keeps the results
// host-independent (a user named kara exists on some hosts and not on GitHub
// runners, which would flip the unrelated-entries expectation).
func lingerSwapUserProbe(t *testing.T, fn func(string) (*user.User, error)) {
	t.Helper()
	orig := lookupUser
	lookupUser = fn
	t.Cleanup(func() { lookupUser = orig })
}

// lingerStubbedUsersExist stubs the user-existence probe so exactly the names
// in real resolve and everything else is an unknown user.
func lingerStubbedUsersExist(t *testing.T, real []string) {
	t.Helper()
	lingerSwapUserProbe(t, func(name string) (*user.User, error) {
		if slices.Contains(real, name) {
			return &user.User{Username: name, Uid: "61001", HomeDir: "/home/" + name}, nil
		}
		return nil, user.UnknownUserError(name)
	})
}

// The core regression: a stale linger entry whose name drifted from the
// bunker- pattern (the filing's bunter- typo class) must be counted — exactly
// once — alongside a known-live managed agent's entry.
func TestResidueInventoryLingerCountsNonManagedStaleEntry(t *testing.T) {
	f := newResidueFixture(t)
	f.registerLive(t, "live")
	f.addUser(t, "live")
	f.addLingerEntry(t, "bunker-live")
	lingerStubbedUsersExist(t, []string{"bunker-live"})

	before := f.m.ResidueInventory()
	if before.StaleLinger != 0 {
		t.Fatalf("premise broken: StaleLinger = %d, want 0 before the probe entry is added", before.StaleLinger)
	}

	// The filing's reversible probe, as a fixture fact: a non-managed name
	// whose user is gone.
	f.addLingerEntry(t, "bunter-probe-gap080")

	after := f.m.ResidueInventory()
	if after.StaleLinger != 1 {
		t.Errorf("StaleLinger = %d, want 1: a stale entry named bunter-probe-gap080 must be counted even though it does not carry the bunker- prefix", after.StaleLinger)
	}
	if after.Status != ResidueStatusOK {
		t.Errorf("status = %q, want %q (detail: %s)", after.Status, ResidueStatusOK, after.Detail)
	}
}

// Mirrors the filing's reversible probe end-to-end: removing the non-managed
// stale entry brings the count back down by exactly one.
func TestResidueInventoryLingerRemovingNonManagedEntryUncounts(t *testing.T) {
	f := newResidueFixture(t)
	f.addLingerEntry(t, "bunter-probe-gap080")
	lingerStubbedUsersExist(t, nil)

	if got := f.m.ResidueInventory().StaleLinger; got != 1 {
		t.Fatalf("StaleLinger = %d, want 1 while the stale non-managed entry exists", got)
	}
	if err := os.Remove(filepath.Join(f.linger, "bunter-probe-gap080")); err != nil {
		t.Fatal(err)
	}
	if got := f.m.ResidueInventory().StaleLinger; got != 0 {
		t.Errorf("StaleLinger = %d, want 0 after the stale entry was removed (the removal must decrement by exactly one)", got)
	}
}

// A non-managed entry whose user STILL EXISTS is not residue: the plane must
// never count every stray file, only entries whose user is gone.
func TestResidueInventoryLingerIgnoresRealUsers(t *testing.T) {
	f := newResidueFixture(t)
	f.addLingerEntry(t, "bunter-probe-realuser")
	f.addLingerEntry(t, "kara") // a real (stubbed) system user's linger entry
	lingerStubbedUsersExist(t, []string{"bunter-probe-realuser", "kara"})

	inv := f.m.ResidueInventory()
	if inv.StaleLinger != 0 {
		t.Errorf("StaleLinger = %d, want 0: entries whose user still exists must not count as stale", inv.StaleLinger)
	}
	if inv.Status != ResidueStatusOK {
		t.Errorf("status = %q, want %q (detail: %s)", inv.Status, ResidueStatusOK, inv.Detail)
	}
}

// The managed-prefix behavior is preserved: a bunker-<valid-id> entry the
// daemon does NOT know still counts stale (its user is gone).
func TestResidueInventoryLingerStillCountsManagedUnknown(t *testing.T) {
	f := newResidueFixture(t)
	f.addLingerEntry(t, "bunker-ghost-linger")
	lingerStubbedUsersExist(t, nil)

	if got := f.m.ResidueInventory().StaleLinger; got != 1 {
		t.Errorf("StaleLinger = %d, want 1: the managed stale class must keep counting", got)
	}
}

// A managed entry the daemon still knows is exempt BEFORE the user lookup —
// a healthy agent's linger entry never reads as residue.
func TestResidueInventoryLingerExemptsDaemonKnownAgent(t *testing.T) {
	f := newResidueFixture(t)
	f.registerLive(t, "live")
	f.addLingerEntry(t, "bunker-live")
	// The stub knows NO user at all: the exemption must hold even though the
	// user lookup for bunker-live would report the user gone.
	lingerStubbedUsersExist(t, nil)

	if got := f.m.ResidueInventory().StaleLinger; got != 0 {
		t.Errorf("StaleLinger = %d, want 0: a linger entry for an agent the daemon knows must never count stale", got)
	}
}

// The user-existence translation is the seam's own contract: user.Lookup
// ERRORS when the user is absent, and every in-repo consumer of this seam maps
// err != nil to "user gone" (manager_destroy.go; internal/cli
// lingerUserExists, whose default maps every lookup error to the prune's
// target class). Pin that the linger plane applies the SAME translation — any
// lookup error means a stale entry — so the daemon count can never diverge
// from host-local `bunker linger` on the classification rule.
func TestResidueInventoryLingerLookupErrorMeansUserGone(t *testing.T) {
	f := newResidueFixture(t)
	f.addLingerEntry(t, "bunter-probe-gap080")  // user.UnknownUserError path
	f.addLingerEntry(t, "bunter-probe-nssfail") // a non-absent lookup error
	lingerSwapUserProbe(t, func(name string) (*user.User, error) {
		if name == "bunter-probe-nssfail" {
			return nil, os.ErrPermission
		}
		return nil, user.UnknownUserError(name)
	})

	inv := f.m.ResidueInventory()
	if inv.StaleLinger != 2 {
		t.Errorf("StaleLinger = %d, want 2: every lookup error must classify the entry as stale, matching host-local `bunker linger`", inv.StaleLinger)
	}
	if inv.Status != ResidueStatusOK {
		t.Errorf("status = %q, want %q (detail: %s)", inv.Status, ResidueStatusOK, inv.Detail)
	}
}
