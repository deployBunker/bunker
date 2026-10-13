package audit

// GAP-136 / REQ-A2 regression tests: anchoring is on by default, and a
// TRUNCATED or FORGED chain fails verification.
//
// The tests deliberately pair a positive and a negative arm over the SAME
// bytes wherever the claim is "the anchor is what catches this": the forged or
// truncated file verifies clean once the anchor is removed, and fails while it
// is present. Without that control a passing test would not distinguish the
// anchor's work from a chain walk that was already strict.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gap136Log creates a keyed-by-default log (plain New: no options, no config)
// with n records and returns the log and its path.
func gap136Log(t *testing.T, n int) (*AuditLog, string) {
	t.Helper()
	l, path := newTestLog(t)
	writeRecords(t, l, n)
	return l, path
}

// keepFirstLines rewrites path with only its first n lines (whole lines) —
// the tail-removal an attacker or a rolled-back file would produce. Every
// surviving record stays internally consistent, which is exactly why the plain
// hash chain accepts it.
func keepFirstLines(t *testing.T, path string, n int) {
	t.Helper()
	b := readLog(t, path)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if n > len(lines) {
		t.Fatalf("keepFirstLines(%d): file has only %d lines", n, len(lines))
	}
	out := strings.Join(lines[:n], "\n") + "\n"
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatalf("rewrite %s: %v", path, err)
	}
}

// dropRecordAndRehash deletes record idx (1-based) and re-computes every
// remaining record's hash and prev_hash, i.e. a chain that anyone able to write
// the file could produce. It is internally consistent, so the SHA-256 chain
// alone verifies it — the SEC-09 "forgery verifies OK" case.
func dropRecordAndRehash(t *testing.T, path string, idx int) {
	t.Helper()
	b := readLog(t, path)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if idx < 1 || idx > len(lines) {
		t.Fatalf("dropRecordAndRehash: idx %d out of range (1..%d)", idx, len(lines))
	}
	recs := make([]Record, 0, len(lines)-1)
	for i, line := range lines {
		if i+1 == idx {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("parse line %d: %v", i+1, err)
		}
		recs = append(recs, rec)
	}
	prev := ""
	var out strings.Builder
	for i := range recs {
		recs[i].Hash = ""
		recs[i].PrevHash = prev
		canonical, err := json.Marshal(recs[i])
		if err != nil {
			t.Fatalf("marshal record %d: %v", i+1, err)
		}
		sum := sha256.Sum256(canonical)
		recs[i].Hash = hex.EncodeToString(sum[:])
		prev = recs[i].Hash
		line, err := json.Marshal(recs[i])
		if err != nil {
			t.Fatalf("marshal line %d: %v", i+1, err)
		}
		out.Write(line)
		out.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(out.String()), 0o600); err != nil {
		t.Fatalf("rewrite %s: %v", path, err)
	}
}

// TestAnchoringOnByDefault is acceptance criterion 1: anchoring needs no config
// flag. A log opened with plain New — no options, no config — is keyed and
// anchored, and its anchor names the chain head.
func TestAnchoringOnByDefault(t *testing.T) {
	_, path := gap136Log(t, 3)

	// The key is auto-provisioned next to the log, mode 0600.
	ki, err := os.Stat(AnchorKeyPath(path))
	if err != nil {
		t.Fatalf("anchor key was not provisioned at %s: %v", AnchorKeyPath(path), err)
	}
	if perm := ki.Mode().Perm(); perm != 0o600 {
		t.Errorf("anchor key mode = %04o, want 0600", perm)
	}

	// The anchor names the last record of the live file and carries a MAC.
	a, err := ReadAnchor(path)
	if err != nil {
		t.Fatalf("ReadAnchor: %v", err)
	}
	if a == nil {
		t.Fatal("no anchor file after 3 records: anchoring is not on by default")
	}
	if want := lastRecord(t, path)["hash"].(string); a.Head != want {
		t.Errorf("anchor head = %q, want the live tail %q", a.Head, want)
	}
	if a.MAC == "" || a.KeyID == "" {
		t.Errorf("anchor is not keyed: mac=%q key_id=%q", a.MAC, a.KeyID)
	}
	if a.Version != AnchorVersion {
		t.Errorf("anchor version = %d, want %d", a.Version, AnchorVersion)
	}

	// Verify agrees, and reports the anchor as keyed with a validated MAC.
	res, err := VerifyDetailed(path, "")
	if err != nil {
		t.Fatalf("VerifyDetailed on a healthy anchored log: %v", err)
	}
	if res.AnchorState != AnchorStateKeyed {
		t.Errorf("anchor state = %q (note %q), want %q", res.AnchorState, res.AnchorNote, AnchorStateKeyed)
	}
	if !res.Verified() {
		t.Error("Verified() = false on a keyed, validated, clean chain")
	}
	if records, firstBad, err := Verify(path); err != nil || records != 3 || firstBad != 0 {
		t.Errorf("Verify = (%d, %d, %v), want (3, 0, nil)", records, firstBad, err)
	}
}

// TestAnchorRefreshedEveryRecord: the anchor tracks the live tail, so a change
// to the tail is always covered by a fresh anchor.
func TestAnchorRefreshedEveryRecord(t *testing.T) {
	l, path := gap136Log(t, 2)
	first := lastRecord(t, path)["hash"].(string)

	writeRecords(t, l, 1) // a third record: rec-3
	a, err := ReadAnchor(path)
	if err != nil || a == nil {
		t.Fatalf("ReadAnchor after a new record: (%v, %v)", a, err)
	}
	want := lastRecord(t, path)["hash"].(string)
	if a.Head == first {
		t.Error("anchor still names the previous head: it is not refreshed per record")
	}
	if a.Head != want {
		t.Errorf("anchor head = %q, want the new live tail %q", a.Head, want)
	}
}

// TestTruncatedChainFailsVerification is the board row's criterion (1) and
// REQ-A2's PASS line: removing trailing records must FAIL verification.
//
// The control arm is the same bytes with the anchor removed: it verifies OK,
// which is precisely the pre-GAP-136 behaviour the finding recorded.
func TestTruncatedChainFailsVerification(t *testing.T) {
	_, path := gap136Log(t, 5)
	if _, _, err := Verify(path); err != nil {
		t.Fatalf("precondition: healthy log failed verification: %v", err)
	}

	keepFirstLines(t, path, 2) // records 3..5 removed

	// Control: without the anchor the truncated chain is accepted.
	if err := os.Remove(AnchorPath(path)); err != nil {
		t.Fatalf("remove anchor for the control arm: %v", err)
	}
	if records, firstBad, err := Verify(path); err != nil || firstBad != 0 {
		t.Errorf("control arm (no anchor) = (%d, %d, %v), want the truncated chain to verify clean", records, firstBad, err)
	}

	// Now put a keyed-looking anchor back that names a head which does not
	// exist: an anchor whose head is gone is proof of truncation.
	if err := os.WriteFile(AnchorPath(path), []byte(`{"version":1,"head":"deadbeef","ts":"2026-10-13T00:00:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write anchor: %v", err)
	}
	_, firstBad, err := Verify(path)
	if err == nil {
		t.Fatal("Verify accepted a chain whose anchored head is gone: truncation is undetected")
	}
	if firstBad != 0 {
		t.Errorf("firstBad = %d, want 0 (an anchor failure is not record-scoped)", firstBad)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error = %q, want it to name the truncation", err)
	}

	// The real article: a genuine anchor from a longer chain, then truncation.
	_, path2 := gap136Log(t, 5)
	keepFirstLines(t, path2, 2)
	if _, _, err := Verify(path2); err == nil {
		t.Error("Verify accepted a truncated chain that still has its own anchor")
	} else if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error = %q, want it to name the truncation", err)
	}
}

// TestForgedChainFailsVerification: the other half of SEC-09. Deleting a record
// and re-hashing the rest produces an internally consistent chain, so the bare
// SHA-256 chain verifies it — but the anchored head no longer exists, and an
// attacker without the key cannot forge a matching anchor.
func TestForgedChainFailsVerification(t *testing.T) {
	_, path := gap136Log(t, 5)
	dropRecordAndRehash(t, path, 3) // records 3..5 rewritten to hide record 3

	anchorBytes, err := os.ReadFile(AnchorPath(path))
	if err != nil {
		t.Fatalf("read anchor: %v", err)
	}
	// With the anchor present the forgery is caught.
	if records, firstBad, err := Verify(path); err == nil || firstBad != 0 {
		t.Errorf("forged chain = (%d, %d, %v) with the anchor present, want a truncation failure", records, firstBad, err)
	}
	// Control arm: the forged chain is accepted once the anchor is gone, which
	// is what makes this an anchor test and not a test of a stricter walk.
	if err := os.Remove(AnchorPath(path)); err != nil {
		t.Fatalf("remove anchor: %v", err)
	}
	if records, firstBad, err := Verify(path); err != nil {
		t.Errorf("control arm (no anchor) = (%d, %d, %v), want the forged chain to verify clean", records, firstBad, err)
	}

	// A forger who re-anchors the forged head cannot authenticate it: the MAC
	// was computed over the original head.
	var a Anchor
	if err := json.Unmarshal(anchorBytes, &a); err != nil {
		t.Fatalf("parse anchor: %v", err)
	}
	a.Head = lastRecord(t, path)["hash"].(string) // the forged head
	forged, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal forged anchor: %v", err)
	}
	if err := os.WriteFile(AnchorPath(path), append(forged, '\n'), 0o600); err != nil {
		t.Fatalf("write forged anchor: %v", err)
	}
	if _, _, err := Verify(path); err == nil {
		t.Fatal("Verify accepted a re-anchored forged chain: the MAC does not protect the anchor")
	} else if !strings.Contains(err.Error(), "MAC mismatch") {
		t.Errorf("error = %q, want an anchor MAC mismatch", err)
	}
}

// TestTamperedAnchorDetected: editing the anchor file itself is tamper-evidence
// failure, not a silent re-anchor.
func TestTamperedAnchorDetected(t *testing.T) {
	_, path := gap136Log(t, 2)
	a, err := ReadAnchor(path)
	if err != nil || a == nil {
		t.Fatalf("ReadAnchor: (%v, %v)", a, err)
	}
	a.Head = "00" + strings.TrimPrefix(a.Head, "00")
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(AnchorPath(path), append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("write tampered anchor: %v", err)
	}
	if _, _, err := Verify(path); err == nil {
		t.Fatal("Verify accepted a tampered anchor")
	} else if !strings.Contains(err.Error(), "MAC mismatch") {
		t.Errorf("error = %q, want an anchor MAC mismatch", err)
	}
}

// TestUnkeyedAnchorStillDetectsTruncation: when no key can be provisioned the
// anchor degrades to unkeyed — reported as such, and truncation is still caught.
// The key path is forced to fail by putting a DIRECTORY where the key file goes.
func TestUnkeyedAnchorStillDetectsTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	if err := os.MkdirAll(AnchorKeyPath(path), 0o700); err != nil {
		t.Fatalf("create key-path directory: %v", err)
	}
	l, err := New(path)
	if err != nil {
		t.Fatalf("New with an unusable key path: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	writeRecords(t, l, 4)

	a, err := ReadAnchor(path)
	if err != nil || a == nil {
		t.Fatalf("anchor was not written: (%v, %v)", a, err)
	}
	if a.MAC != "" || a.KeyID != "" {
		t.Errorf("anchor claims to be keyed (mac=%q key_id=%q) although no key was available", a.MAC, a.KeyID)
	}
	res, verr := VerifyDetailed(path, "")
	if verr != nil {
		t.Fatalf("unkeyed anchor must not fail verification on its own: %v", verr)
	}
	if res.AnchorState != AnchorStateUnkeyed {
		t.Errorf("anchor state = %q (note %q), want %q", res.AnchorState, res.AnchorNote, AnchorStateUnkeyed)
	}
	if res.Verified() {
		t.Error("Verified() = true for an unkeyed anchor: forgery is not detected there")
	}

	keepFirstLines(t, path, 2)
	if _, _, err := Verify(path); err == nil {
		t.Error("an unkeyed anchor no longer detects truncation")
	}
}

// TestUnanchoredChainStillVerifies: a log with no anchor file at all (a
// pre-GAP-136 tree, or a tree copied without its anchor) is reported as
// unanchored, not as broken — and the report says what that costs.
func TestUnanchoredChainStillVerifies(t *testing.T) {
	_, path := gap136Log(t, 3)
	if err := os.Remove(AnchorPath(path)); err != nil {
		t.Fatalf("remove anchor: %v", err)
	}
	res, err := VerifyDetailed(path, "")
	if err != nil {
		t.Fatalf("unsigned-but-clean chain failed verification: %v", err)
	}
	if res.AnchorState != AnchorStateNone {
		t.Errorf("anchor state = %q, want %q", res.AnchorState, AnchorStateNone)
	}
	if !strings.Contains(res.AnchorNote, "unanchored") {
		t.Errorf("note = %q, want it to say the chain is unanchored", res.AnchorNote)
	}
	if res.Verified() {
		t.Error("Verified() = true without an anchor")
	}
}

// TestUncheckableKeyReportedNotFailed: a keyed anchor whose key is not
// available is reported as unverified — the truncation check still runs, and
// the command stays usable. (An explicit audit.seal_key is deliberately not
// copied next to the log, so this is the state of such a deployment when the
// caller does not supply the key.)
func TestUncheckableKeyReportedNotFailed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := NewWithOptions(path, Options{SealKey: "gap136-explicit-key-not-a-secret"})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	writeRecords(t, l, 3)

	// The key file must NOT have been created for an explicit key.
	if _, err := os.Stat(AnchorKeyPath(path)); !os.IsNotExist(err) {
		t.Errorf("an explicitly configured anchor key was written to %s (err %v) — a configured secret must not be copied to disk", AnchorKeyPath(path), err)
	}

	// With the key supplied, the MAC validates.
	res, err := VerifyDetailed(path, "gap136-explicit-key-not-a-secret")
	if err != nil || res.AnchorState != AnchorStateKeyed {
		t.Errorf("with the key: state = %q (err %v), want %q", res.AnchorState, err, AnchorStateKeyed)
	}
	// Without it, the anchor is reported as unverified — not as keyed, and not
	// as a failure.
	res, err = VerifyDetailed(path, "")
	if err != nil {
		t.Fatalf("an uncheckable MAC must not fail verification: %v", err)
	}
	if res.AnchorState != AnchorStateUnverified {
		t.Errorf("state = %q (note %q), want %q", res.AnchorState, res.AnchorNote, AnchorStateUnverified)
	}
	if !strings.Contains(res.AnchorNote, "MAC not checked") {
		t.Errorf("note = %q, want it to say the MAC was not checked", res.AnchorNote)
	}
	if res.Verified() {
		t.Error("Verified() = true for an anchor whose MAC was never checked")
	}
}

// TestMalformedAnchorFails: a damaged anchor is an integrity failure, not an
// "unanchored" pass.
func TestMalformedAnchorFails(t *testing.T) {
	_, path := gap136Log(t, 2)
	if err := os.WriteFile(AnchorPath(path), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write malformed anchor: %v", err)
	}
	if _, _, err := Verify(path); err == nil {
		t.Fatal("Verify accepted a malformed anchor")
	}
}

// TestAnchorOverwriteIsAtomic: the anchor is published by rename, so a reader
// never sees a partial file (a torn anchor would read as tampering).
func TestAnchorOverwriteIsAtomic(t *testing.T) {
	l, path := gap136Log(t, 1)
	for i := 0; i < 5; i++ {
		writeRecords(t, l, 1)
		if _, err := ReadAnchor(path); err != nil {
			t.Fatalf("iteration %d: anchor unreadable after a write: %v", i, err)
		}
	}
	if _, _, err := Verify(path); err != nil {
		t.Errorf("Verify after repeated anchor refreshes: %v", err)
	}
}

// TestChainContainsHash: the truncation check's primitive.
func TestChainContainsHash(t *testing.T) {
	_, path := gap136Log(t, 3)
	head := lastRecord(t, path)["hash"].(string)
	if ok, err := chainContainsHash(path, head); err != nil || !ok {
		t.Errorf("chainContainsHash(live tail) = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := chainContainsHash(path, "0badc0ffee"); err != nil || ok {
		t.Errorf("chainContainsHash(absent) = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := chainContainsHash(path, ""); err != nil || ok {
		t.Errorf("chainContainsHash(\"\") = (%v, %v), want (false, nil)", ok, err)
	}
}

// TestResolveAnchorKeyIsStableAcrossReopen: the same key file yields the same
// key, so a restarted writer continues the same anchor lineage instead of
// calling every previous anchor tampered.
func TestResolveAnchorKeyIsStableAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l1, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	k1 := l1.anchorKey
	if err := l1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	l2, err := New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	if l2.anchorKey != k1 {
		t.Error("reopening the log resolved a different anchor key: previous anchors would read as tampered")
	}
	if _, _, err := Verify(path); err != nil {
		t.Errorf("Verify after reopen: %v", err)
	}
}
