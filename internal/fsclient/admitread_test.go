package fsclient

import (
	"testing"
	"time"
)

// TestAdmitReadCountsTheOverCapRefusalOnItsOwn is the smallest possible driver
// for the decision: a cache with a 16-byte cap and 82 bytes of content must
// count the refusal by reason, and must not store a blob.
func TestAdmitReadCountsTheOverCapRefusalOnItsOwn(t *testing.T) {
	c, err := OpenCache(CacheConfig{Dir: t.TempDir(), MaxBytes: 1 << 20, MaxEntryBytes: 16, MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := c.MaxEntryBytes(); got != 16 {
		t.Fatalf("effective per-entry cap = %d, want 16", got)
	}
	data := []byte("X-REPLACEMENT-CONTENT-0123456789-abcdefghijklmnopqrstuvwxyz-ABCDEFGHIJKLMNOPQRSTUV")
	hash := HashBytes(data)
	if c.AdmitRead("p", hash, data) {
		t.Fatal("an over-cap read must not be reported as cached")
	}
	if got := c.BypassCount(BypassReasonOverEntryCap); got != 1 {
		t.Fatalf("over_entry_cap = %d, want 1 (census=%v)", got, c.Stats().BypassReasons)
	}
	if got := c.Stats().OversizeBypasses; got != 1 {
		t.Fatalf("oversize_bypasses = %d, want 1", got)
	}
}
