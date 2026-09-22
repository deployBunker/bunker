// rotate_test.go — GAP-132 acceptance coverage for zero-downtime jwt_secret
// rotation: a token signed with the retired secret is accepted DURING the
// overlap window and rejected after it expires (review criterion 3).
package auth

import (
	"testing"
	"time"
)

// TestRotateSecret_OldSecretAcceptedDuringOverlapThenRejected drives the
// real dual-accept path end to end: issue under secret A, rotate to B with a
// short window, expect the A-token to keep validating while the window is
// open and to be rejected once it lapses.
func TestRotateSecret_OldSecretAcceptedDuringOverlapThenRejected(t *testing.T) {
	const oldSecret = "gap132-rotation-test-old-secret-32-bytes!!"
	const newSecret = "gap132-rotation-test-new-secret-32-bytes!!"

	a := NewJWTAuth(oldSecret, nil)
	oldTok, err := a.IssueMasterToken(time.Hour)
	if err != nil {
		t.Fatalf("issue under old secret: %v", err)
	}

	fingerprint, err := a.RotateSecret(newSecret, 80*time.Millisecond)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if fingerprint == "" {
		t.Fatal("rotate must report the retired secret's fingerprint")
	}

	// New tokens validate under the new secret immediately...
	newTok, err := a.IssueMasterToken(time.Hour)
	if err != nil {
		t.Fatalf("issue under new secret: %v", err)
	}
	if _, err := a.parseToken(newTok); err != nil {
		t.Fatalf("new token must validate after rotation: %v", err)
	}
	// ...and the OLD token still validates while the window is open.
	if _, err := a.parseToken(oldTok); err != nil {
		t.Fatalf("retired-secret token must validate during the overlap window: %v", err)
	}

	// After the window lapses the old token is rejected.
	time.Sleep(120 * time.Millisecond)
	if _, err := a.parseToken(oldTok); err == nil {
		t.Fatal("retired-secret token must be REJECTED after the overlap window expires")
	}
	if _, err := a.parseToken(newTok); err != nil {
		t.Fatalf("new-secret token must stay valid after the window: %v", err)
	}
}

// rotateWindowProbe reads the live retire deadline of the previous secret.
// Test-only access into the rotating secret's overlap state.
func (r *rotatingSecret) rotateWindowProbe() (hasPrevious bool, retireAt time.Time) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.previous) > 0, r.previousRetireAt
}

// TestRotateSecret_ClampsToMaxOverlap pins the ceiling: a requested window
// beyond MaxRotateOverlap is clamped, never honored (a daemon must not be
// talked into honoring a retired key for days).
func TestRotateSecret_ClampsToMaxOverlap(t *testing.T) {
	const s1 = "gap132-clamp-test-secret-one-32-bytes!!!!!!"
	const s2 = "gap132-clamp-test-secret-two-32-bytes!!!!!!"

	a := NewJWTAuth(s1, nil)
	if _, err := a.RotateSecret(s2, 24*time.Hour); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	hasPrev, retireAt := a.secret.rotateWindowProbe()
	if !hasPrev || retireAt.IsZero() {
		t.Fatal("retired secret must carry a retire deadline")
	}
	if window := time.Until(retireAt); window > MaxRotateOverlap {
		t.Fatalf("overlap window = %s, must be clamped to %s", window, MaxRotateOverlap)
	}
}

// TestRotateSecret_DefaultWhenZero pins the default window: overlap=0 means
// DefaultRotateOverlap (a rotation must never leave the previous secret
// valid forever).
func TestRotateSecret_DefaultWhenZero(t *testing.T) {
	const s1 = "gap132-default-test-secret-one-32-bytes!!!!!"
	const s2 = "gap132-default-test-secret-two-32-bytes!!!!!"

	a := NewJWTAuth(s1, nil)
	if _, err := a.RotateSecret(s2, 0); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	_, retireAt := a.secret.rotateWindowProbe()
	want := time.Now().Add(DefaultRotateOverlap)
	if diff := retireAt.Sub(want); diff < -5*time.Second || diff > 5*time.Second {
		t.Fatalf("retire deadline = %s, want ~%s (DefaultRotateOverlap)", retireAt, want)
	}
}
