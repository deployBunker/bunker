package audit

// GAP-136 / SEC-16 regression tests for the honest status report.
//
// The finding was not "the status command lacks a field" — it was that the
// report is NOT SELF-CRITICAL: it printed "last verify: n/a" immediately after a
// successful verify and never mentioned that no off-host anchor exists, so a
// truncated chain and a healthy one produced the same confident output.
//
// Every test below asserts BOTH halves: what the report says when everything is
// fine, and that it refuses to claim health when it is not.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// statusLog creates a keyed, anchored log with n records.
func statusLog(t *testing.T, n int) string {
	t.Helper()
	l, path := newTestLog(t)
	writeRecords(t, l, n)
	return path
}

// shipState writes the ship-state file the daemon's shipper leaves next to the
// log when an off-host anchor is configured and has shipped.
func shipState(t *testing.T, path string, st ShipState) {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal ship state: %v", err)
	}
	if err := os.WriteFile(path+shipStateSuffix, b, 0o600); err != nil {
		t.Fatalf("write ship state: %v", err)
	}
}

// TestLocalStatusReportsChainLengthAndAnchor: acceptance criterion 2 — chain
// length, last anchor hash, gaps/breaks and the storage location are all in the
// report.
func TestLocalStatusReportsChainLengthAndAnchor(t *testing.T) {
	path := statusLog(t, 4)
	st, err := LocalStatus(path)
	if err != nil {
		t.Fatalf("LocalStatus: %v", err)
	}

	if st.StoragePath != path {
		t.Errorf("StoragePath = %q, want %q", st.StoragePath, path)
	}
	if !st.Enabled || !st.ChainOK {
		t.Errorf("Enabled=%v ChainOK=%v, want true/true", st.Enabled, st.ChainOK)
	}
	if st.ChainLength != 4 || st.Records != 4 {
		t.Errorf("ChainLength=%d Records=%d, want 4/4", st.ChainLength, st.Records)
	}
	head := lastRecord(t, path)["hash"].(string)
	if st.LastAnchorHash != head || st.ChainHead != head {
		t.Errorf("LastAnchorHash=%q ChainHead=%q, want %q", st.LastAnchorHash, st.ChainHead, head)
	}
	if len(st.Gaps) != 0 {
		t.Errorf("Gaps = %v, want none on a clean chain", st.Gaps)
	}
	if st.ChainError != "" {
		t.Errorf("ChainError = %q, want empty", st.ChainError)
	}
	if st.FirstBadRecord != 0 {
		t.Errorf("FirstBadRecord = %d, want 0", st.FirstBadRecord)
	}

	// The anchor half: present, keyed, MAC validated, and named.
	if st.Anchor == nil {
		t.Fatal("Anchor is nil: the report says nothing about anchoring")
	}
	if st.Anchor.State != AnchorStateKeyed || !st.Anchor.MACValid {
		t.Errorf("anchor state=%q MACValid=%v, want keyed/true", st.Anchor.State, st.Anchor.MACValid)
	}
	if st.Anchor.Head != head || st.Anchor.KeyID == "" || st.Anchor.TS == "" {
		t.Errorf("anchor head=%q keyID=%q ts=%q, want the live head and a key id/timestamp", st.Anchor.Head, st.Anchor.KeyID, st.Anchor.TS)
	}
}

// TestLocalStatusSaysNoOffHostAnchor is the SEC-16 line: with no off-host
// anchor the report must say so in those words, and must NOT call the trail
// healthy.
func TestLocalStatusSaysNoOffHostAnchor(t *testing.T) {
	path := statusLog(t, 3)
	st, err := LocalStatus(path)
	if err != nil {
		t.Fatalf("LocalStatus: %v", err)
	}
	if st.Anchor.OffHost {
		t.Error("OffHost = true with no ship-state file and no configured ship_to")
	}
	if got := st.AnchorStatement(); got != "no off-host anchor configured" {
		t.Errorf("anchor statement = %q, want the literal 'no off-host anchor configured'", got)
	}
	if st.Health == HealthHealthy {
		t.Errorf("Health = %q (reason %q) for a trail with no off-host anchor — that is the SEC-16 false claim", st.Health, st.HealthReason)
	}
	if st.Health != HealthDegraded {
		t.Errorf("Health = %q, want %q", st.Health, HealthDegraded)
	}
	if !strings.Contains(st.HealthReason, "no off-host anchor") {
		t.Errorf("HealthReason = %q, want it to name the missing off-host anchor", st.HealthReason)
	}
}

// TestLocalStatusHealthyOnlyWithOffHostAnchor: "healthy" is reachable, but only
// with a verified chain, a validated anchor MAC AND an off-host anchor.
func TestLocalStatusHealthyOnlyWithOffHostAnchor(t *testing.T) {
	path := statusLog(t, 3)
	shipState(t, path, ShipState{
		ShipTo:      "https://collector.example.net/v1",
		LastAttempt: "2026-09-14T12:00:00Z",
		LastResult:  "ok",
		LastSuccess: "2026-09-14T12:00:00Z",
		QueueDepth:  2,
	})
	st, err := LocalStatus(path)
	if err != nil {
		t.Fatalf("LocalStatus: %v", err)
	}
	if !st.Anchor.OffHost || st.Anchor.OffHostTarget != "https://collector.example.net/v1" {
		t.Errorf("OffHost=%v target=%q, want true/https://collector.example.net/v1", st.Anchor.OffHost, st.Anchor.OffHostTarget)
	}
	if st.Anchor.OffHostLastSuccess != "2026-09-14T12:00:00Z" {
		t.Errorf("OffHostLastSuccess = %q, want the shipped timestamp", st.Anchor.OffHostLastSuccess)
	}
	if !strings.Contains(st.AnchorStatement(), "last shipped") {
		t.Errorf("anchor statement = %q, want it to name the successful shipment", st.AnchorStatement())
	}
	if st.Health != HealthHealthy {
		t.Errorf("Health = %q (reason %q), want %q", st.Health, st.HealthReason, HealthHealthy)
	}
}

// TestLocalStatusOffHostFromDaemonConfig: a configured ship_to that has not
// shipped anything yet leaves no ship-state file — the report must still count
// it as an off-host anchor rather than saying "none configured".
func TestLocalStatusOffHostFromDaemonConfig(t *testing.T) {
	path := statusLog(t, 2)
	st, err := LocalStatusWith(path, StatusOptions{ShipTo: "syslog://anchor.example.net:601"})
	if err != nil {
		t.Fatalf("LocalStatusWith: %v", err)
	}
	if !st.Anchor.OffHost {
		t.Fatal("OffHost = false for a configured ship_to")
	}
	if !strings.Contains(st.AnchorStatement(), "nothing shipped successfully yet") {
		t.Errorf("anchor statement = %q, want it to say nothing has shipped yet", st.AnchorStatement())
	}
	if st.Health != HealthHealthy {
		t.Errorf("Health = %q, want %q (chain verified + off-host anchor configured)", st.Health, HealthHealthy)
	}
}

// TestLocalStatusBrokenChainIsHonest: a TRUNCATED chain must report broken, name
// the gap, and never claim healthy — the whole point of acceptance criterion 3.
func TestLocalStatusBrokenChainIsHonest(t *testing.T) {
	path := statusLog(t, 5)
	keepFirstLines(t, path, 2)

	st, err := LocalStatus(path)
	if err != nil {
		t.Fatalf("LocalStatus on a truncated log must still produce a report: %v", err)
	}
	if st.ChainOK {
		t.Error("ChainOK = true on a truncated chain")
	}
	if st.Health != HealthBroken {
		t.Errorf("Health = %q, want %q", st.Health, HealthBroken)
	}
	if st.Health == HealthHealthy {
		t.Error("Health = healthy on a truncated chain")
	}
	if st.ChainError == "" || !strings.Contains(st.ChainError, "truncated") {
		t.Errorf("ChainError = %q, want it to name the truncation", st.ChainError)
	}
	if len(st.Gaps) == 0 || !strings.Contains(strings.Join(st.Gaps, "; "), "truncated") {
		t.Errorf("Gaps = %v, want the truncation listed", st.Gaps)
	}
	if !strings.Contains(st.HealthReason, "chain integrity failure") {
		t.Errorf("HealthReason = %q, want it to say the chain failed", st.HealthReason)
	}
	// The counts are still reported: a broken chain is reported, not hidden.
	if st.ChainLength != 2 {
		t.Errorf("ChainLength = %d, want 2 (what is actually on disk)", st.ChainLength)
	}
}

// TestLocalStatusEditedRecordIsHonest: an edited record keeps the chain length
// but fails verification at a named record index.
func TestLocalStatusEditedRecordIsHonest(t *testing.T) {
	path := statusLog(t, 4)
	tamperSummary(t, path, 3)

	st, err := LocalStatus(path)
	if err != nil {
		t.Fatalf("LocalStatus on an edited log: %v", err)
	}
	if st.ChainOK || st.Health != HealthBroken {
		t.Errorf("ChainOK=%v Health=%q, want false/%q", st.ChainOK, st.Health, HealthBroken)
	}
	if st.FirstBadRecord != 3 {
		t.Errorf("FirstBadRecord = %d, want 3", st.FirstBadRecord)
	}
	if st.ChainLength != 4 {
		t.Errorf("ChainLength = %d, want 4 (the edit does not change the count)", st.ChainLength)
	}
}

// TestLocalStatusMissingAnchorIsHonest: the anchor removed from a tree that has
// a key is reported as unanchored (with the deletion named), and the health
// verdict drops out of "healthy" territory.
func TestLocalStatusMissingAnchorIsHonest(t *testing.T) {
	path := statusLog(t, 3)
	if err := os.Remove(AnchorPath(path)); err != nil {
		t.Fatalf("remove anchor: %v", err)
	}
	st, err := LocalStatus(path)
	if err != nil {
		t.Fatalf("LocalStatus: %v", err)
	}
	if !st.ChainOK {
		t.Errorf("ChainOK = false for a clean chain with no anchor: %v", st.ChainError)
	}
	if st.Anchor.Present || st.Anchor.State != AnchorStateNone {
		t.Errorf("anchor present=%v state=%q, want false/%q", st.Anchor.Present, st.Anchor.State, AnchorStateNone)
	}
	if !strings.Contains(st.Anchor.Note, "anchor file was removed") {
		t.Errorf("anchor note = %q, want it to name the removed anchor", st.Anchor.Note)
	}
	if st.Health == HealthHealthy {
		t.Error("Health = healthy with no anchor on disk")
	}
}

// TestLocalStatusDisabledHost: no log at all reports disabled, with the
// off-host statement still stated rather than omitted.
func TestLocalStatusDisabledHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	st, err := LocalStatus(path)
	if err != nil {
		t.Fatalf("LocalStatus on a missing log: %v", err)
	}
	if st.Enabled {
		t.Error("Enabled = true with no log file")
	}
	if st.Health != HealthDisabled {
		t.Errorf("Health = %q, want %q", st.Health, HealthDisabled)
	}
	if st.AnchorStatement() != "no off-host anchor configured" {
		t.Errorf("anchor statement = %q, want the literal 'no off-host anchor configured'", st.AnchorStatement())
	}
}

// TestLocalStatusSealAndShippingStillReported: the GAP-073 fields survive the
// GAP-136 rewrite (sealing, ship result, queue depth, rotations).
func TestLocalStatusSealAndShippingStillReported(t *testing.T) {
	l, path := newTestLogOpts(t, Options{SealKey: "gap136-status-seal-key-not-a-secret"})
	l.rotateAt = 128
	writeRecords(t, l, 2) // .1 = [rec-1], live = [seal, rec-2]
	shipState(t, path, ShipState{
		ShipTo:     "https://collector.example.net/v1",
		LastResult: "ok",
		QueueDepth: 1,
	})

	st, err := LocalStatusWith(path, StatusOptions{AnchorKey: "gap136-status-seal-key-not-a-secret"})
	if err != nil {
		t.Fatalf("LocalStatusWith: %v", err)
	}
	if !st.SealingEnabled || st.LastSeal == nil {
		t.Errorf("SealingEnabled=%v LastSeal=%v, want true/non-nil", st.SealingEnabled, st.LastSeal)
	}
	if st.RotationsLowerBound != 1 {
		t.Errorf("RotationsLowerBound = %d, want 1", st.RotationsLowerBound)
	}
	if st.Shipping == nil || st.Shipping.QueueDepth != 1 {
		t.Errorf("Shipping = %+v, want queue depth 1", st.Shipping)
	}
	if st.ChainLength != 3 { // 1 backup + seal + rec-2
		t.Errorf("ChainLength = %d, want 3", st.ChainLength)
	}
	if st.Health != HealthHealthy {
		t.Errorf("Health = %q (reason %q), want %q", st.Health, st.HealthReason, HealthHealthy)
	}
	if st.Anchor.State != AnchorStateKeyed {
		t.Errorf("anchor state = %q (note %q), want %q with the key supplied", st.Anchor.State, st.Anchor.Note, AnchorStateKeyed)
	}
}
