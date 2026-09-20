package audit

import (
	"path/filepath"
	"testing"
)

// lastRecord returns the final parsed record of the file at path.
func lastRecord(t *testing.T, path string) map[string]any {
	t.Helper()
	recs := parseRecords(t, readLog(t, path))
	if len(recs) == 0 {
		t.Fatalf("%s has no records", path)
	}
	return recs[len(recs)-1]
}

// DF-BUNKER-29 regression: reopening an existing chained audit.log must
// re-seed the chain head so the first post-restart record's prev_hash equals
// the pre-restart tail record's hash, and `Verify` must accept the whole file
// (before the fix the restart record carried prev_hash:"" and Verify reported
// "tamper detected" on an untampered log).
func TestRestartReseedsChainHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")

	l1, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeRecords(t, l1, 3)
	tail := lastRecord(t, path)["hash"].(string)
	if err := l1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Simulated restart: a fresh AuditLog on the same file (clean shutdown —
	// the previous process called Close).
	l2, err := New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	writeRecords(t, l2, 2)

	recs := parseRecords(t, readLog(t, path))
	if len(recs) != 5 {
		t.Fatalf("records = %d, want 5", len(recs))
	}
	if got := recs[3]["prev_hash"]; got != tail {
		t.Errorf("first post-restart record prev_hash = %v, want pre-restart tail %v", got, tail)
	}
	if got, want := recs[4]["prev_hash"], recs[3]["hash"]; got != want {
		t.Errorf("second post-restart record prev_hash = %v, want %v (chain continues)", got, want)
	}

	records, firstBad, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify after restart: %v", err)
	}
	if records != 5 || firstBad != 0 {
		t.Errorf("Verify = (%d records, firstBad %d), want (5, 0)", records, firstBad)
	}
}

// Same fix under a crash-style restart: the first process never calls Close
// (kill -9 / power loss) — only the file on disk is known. The second
// process must still recover the chain head from the file's tail.
func TestRestartWithoutCloseReseedsChainHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")

	l1, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeRecords(t, l1, 3)
	tail := lastRecord(t, path)["hash"].(string)
	// Deliberately NO l1.Close(): the process "died" here. l1's fd leaks
	// until test cleanup, which is exactly the crash semantic.

	l2, err := New(path)
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	writeRecords(t, l2, 1)

	recs := parseRecords(t, readLog(t, path))
	if len(recs) != 4 {
		t.Fatalf("records = %d, want 4", len(recs))
	}
	if got := recs[3]["prev_hash"]; got != tail {
		t.Errorf("first post-crash record prev_hash = %v, want pre-crash tail %v", got, tail)
	}

	if records, firstBad, err := Verify(path); err != nil || firstBad != 0 {
		t.Errorf("Verify after crash-style restart = (%d, firstBad %d, err %v), want (4, 0, nil)", records, firstBad, err)
	}
}

// A restart that happens after rotations (and, with a seal key configured,
// after seal records were appended to the fresh live file) must also resume
// the chain: the live file's first record legitimately chains into the
// rotated-away predecessor, and recovery must accept that link the same way
// Verify's oldest-retained-file rule does.
func TestRestartAfterRotationReseedsChainHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")

	l1, err := NewWithOptions(path, Options{SealKey: "df-bunker-29-restart-seal"})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	l1.rotateAt = 128
	writeRecords(t, l1, 2) // .1 = [rec-1], live = [seal, rec-2]
	if err := l1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	l2, err := NewWithOptions(path, Options{SealKey: "df-bunker-29-restart-seal"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	writeRecords(t, l2, 1)

	live := parseRecords(t, readLog(t, path))
	if len(live) != 3 { // seal, rec-2, rec-3
		t.Fatalf("live file has %d records, want 3", len(live))
	}
	if got, want := live[2]["prev_hash"], live[1]["hash"]; got != want {
		t.Errorf("post-restart record prev_hash = %v, want live tail %v", got, want)
	}

	records, firstBad, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify after rotation+restart: %v", err)
	}
	if records != 4 || firstBad != 0 { // .1:1 + live:3
		t.Errorf("Verify = (%d records, firstBad %d), want (4, 0)", records, firstBad)
	}
}

// Existing behavior preserved: a brand-new log's genesis record still has
// prev_hash:"" and verifies — including the "file exists but is empty"
// first-open shape (New creates the file; a restart on the still-empty file
// must NOT invent a chain head).
func TestReopenEmptyFileKeepsGenesis(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")

	l1, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := l1.Close(); err != nil { // file exists, zero records
		t.Fatalf("Close: %v", err)
	}

	l2, err := New(path)
	if err != nil {
		t.Fatalf("reopen empty file: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	writeRecords(t, l2, 1)

	recs := parseRecords(t, readLog(t, path))
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if got := recs[0]["prev_hash"]; got != "" {
		t.Errorf("genesis prev_hash = %v, want \"\"", got)
	}
	if _, firstBad, err := Verify(path); err != nil || firstBad != 0 {
		t.Errorf("Verify = (firstBad %d, err %v), want (0, nil)", firstBad, err)
	}
}

// Recovery must never bless a damaged tail: when the existing file's last
// record is tampered, the chain head is NOT seeded — the next record starts a
// new segment with prev_hash:"" and Verify still reports the tamper at the
// damaged record. This pins the verifier-stays-strict side of DF-BUNKER-29.
func TestReseedSkipsDamagedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")

	l1, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeRecords(t, l1, 2)
	if err := l1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Tamper the LAST record's summary, keeping the JSON valid (hash breaks).
	tamperSummary(t, path, 2)

	l2, err := New(path)
	if err != nil {
		t.Fatalf("reopen damaged log: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	writeRecords(t, l2, 1)

	recs := parseRecords(t, readLog(t, path))
	if len(recs) != 3 {
		t.Fatalf("records = %d, want 3", len(recs))
	}
	if got := recs[2]["prev_hash"]; got != "" {
		t.Errorf("post-recovery record prev_hash = %v, want \"\" (damaged tail must not be seeded)", got)
	}

	// The tamper is still detected, at the damaged record, not blessed.
	records, firstBad, err := Verify(path)
	if err == nil {
		t.Fatal("Verify on damaged-tail log returned nil error")
	}
	if firstBad != 2 {
		t.Errorf("firstBad = %d, want 2 (the tampered record)", firstBad)
	}
	if records != 3 {
		t.Errorf("records = %d, want 3", records)
	}
}

// Unit tests for the recovery helper live in audit_chainhead_test.go.
