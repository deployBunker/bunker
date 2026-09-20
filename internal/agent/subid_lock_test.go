//go:build unix

package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── host-wide lock semantics (GAP-140) ──────────────────────────────────────
//
// These tests are unix-only because they assert mutual exclusion, which is
// exactly what the non-unix fallback does not provide (see
// subid_lock_nonunix.go). bunkerd targets Linux; the fallback exists so the
// package still builds elsewhere.

// TestLockSubIDs_Serializes pins mutual exclusion between two holders: while
// one holds the lock, another non-blocking attempt must be refused rather than
// proceeding into the critical section.
func TestLockSubIDs_Serializes(t *testing.T) {
	h := newSubIDTestHost(t)

	first, err := lockSubIDs()
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	lockPath := filepath.Join(h.lockDir, subIDLockFileName)
	if _, err := lockSubIDFileAttempt(lockPath, true); err == nil {
		t.Fatal("second non-blocking attempt succeeded while the lock was held")
	}
	first()

	// Released: the same attempt now succeeds.
	second, err := lockSubIDs()
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	second()
}

// TestLockSubIDs_TimeoutIsAnErrorNotAFallback pins the fail-closed contract:
// when the lock cannot be taken inside the bound, lockSubIDs returns an error
// instead of silently appending without exclusion.
func TestLockSubIDs_TimeoutIsAnErrorNotAFallback(t *testing.T) {
	newSubIDTestHost(t)
	restoreTimeout := subIDLockTimeout
	restoreInterval := subIDLockRetryInterval
	subIDLockTimeout = 60 * time.Millisecond
	subIDLockRetryInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		subIDLockTimeout = restoreTimeout
		subIDLockRetryInterval = restoreInterval
	})

	holder, err := lockSubIDs()
	if err != nil {
		t.Fatalf("holder lock: %v", err)
	}
	defer holder()

	start := time.Now()
	release, err := lockSubIDs()
	if err == nil {
		release()
		t.Fatal("waiter took a lock another holder owns")
	}
	if !strings.Contains(err.Error(), "held by another process") {
		t.Errorf("error = %v, want a held-lock attribution", err)
	}
	if elapsed := time.Since(start); elapsed < subIDLockTimeout {
		t.Errorf("waiter gave up after %s, before its %s budget", elapsed, subIDLockTimeout)
	}
}
