package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRecords appends n records with distinguishable summaries rec-1..rec-n.
func writeRecords(t *testing.T, l *AuditLog, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if err := l.Log(Record{TS: "t", Caller: "master", Method: "/m", Outcome: "ok", Summary: fmt.Sprintf("rec-%d", i)}); err != nil {
			t.Fatalf("Log(rec-%d): %v", i, err)
		}
	}
}

// summaries extracts the summary field of every record in b, in order.
func summaries(t *testing.T, b []byte) []string {
	t.Helper()
	recs := parseRecords(t, b)
	out := make([]string, len(recs))
	for i, rec := range recs {
		out[i], _ = rec["summary"].(string)
	}
	return out
}

func equalSummaries(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRotationCreatesBackup(t *testing.T) {
	l, path := newTestLog(t)
	l.rotateAt = 128 // tiny threshold: a single record pushes the file past it

	// The first record fits (the file starts empty); the second crosses the
	// threshold and forces a rotation before it is written.
	writeRecords(t, l, 2)

	backup := path + ".1"
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("expected rotated backup %s: %v", backup, err)
	}
	if got := summaries(t, readLog(t, path)); !equalSummaries(got, []string{"rec-2"}) {
		t.Errorf("audit.log records = %v, want [rec-2]", got)
	}
	if got := summaries(t, readLog(t, backup)); !equalSummaries(got, []string{"rec-1"}) {
		t.Errorf("audit.log.1 records = %v, want [rec-1]", got)
	}
	// Rotated backups keep mode 0600 (rename preserves the live file's mode).
	for _, p := range []string{path, backup} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", p, perm)
		}
	}
	// The first record of the new file chains to the last record of the
	// backup (rotation does not break the chain).
	records := parseRecords(t, readLog(t, path))
	backupRecs := parseRecords(t, readLog(t, backup))
	if prev, _ := records[0]["prev_hash"].(string); prev != backupRecs[len(backupRecs)-1]["hash"] {
		t.Errorf("first record prev_hash = %v, want backup's last hash %v (chain across rotation)", prev, backupRecs[len(backupRecs)-1]["hash"])
	}
}

func TestRotationShiftsBackupsAndDropsOldest(t *testing.T) {
	l, path := newTestLog(t)
	l.rotateAt = 128

	writeRecords(t, l, 5)

	// After 5 records: the live file holds rec-5; backups .1=.4, .2=.3,
	// .3=.2; rec-1 has been rotated past the 3-backup budget and dropped.
	want := map[string][]string{
		path:        {"rec-5"},
		path + ".1": {"rec-4"},
		path + ".2": {"rec-3"},
		path + ".3": {"rec-2"},
	}
	for file, wantSummaries := range want {
		got := summaries(t, readLog(t, file))
		if !equalSummaries(got, wantSummaries) {
			t.Errorf("%s records = %v, want %v", filepath.Base(file), got, wantSummaries)
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Errorf("expected audit.log.4 to not exist, stat err = %v", err)
	}
}

func TestHashChain(t *testing.T) {
	l, path := newTestLog(t)
	const n = 5
	writeRecords(t, l, n)

	records := parseRecords(t, readLog(t, path))
	if len(records) != n {
		t.Fatalf("expected %d records, got %d", n, len(records))
	}
	lines := strings.Split(strings.TrimRight(string(readLog(t, path)), "\n"), "\n")

	hashes := make([]string, n)
	for i, line := range lines {
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("record %d does not unmarshal into Record: %v", i+1, err)
		}
		declared := rec.Hash
		if len(declared) != 64 {
			t.Fatalf("record %d: hash = %q, want 64-char hex", i+1, declared)
		}
		if _, err := hex.DecodeString(declared); err != nil {
			t.Fatalf("record %d: hash %q is not hex: %v", i+1, declared, err)
		}
		// Own hash = SHA-256 of the canonical line (hash field emptied) —
		// reproduce the writer's digest independently.
		rec.Hash = ""
		canonical, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("record %d: re-marshal: %v", i+1, err)
		}
		sum := sha256.Sum256(canonical)
		if got := hex.EncodeToString(sum[:]); got != declared {
			t.Errorf("record %d: recomputed hash %s != recorded %s", i+1, got, declared)
		}
		hashes[i] = declared

		// Chain link: prev_hash must be the previous record's hash.
		wantPrev := ""
		if i > 0 {
			wantPrev = hashes[i-1]
		}
		if rec.PrevHash != wantPrev {
			t.Errorf("record %d: prev_hash = %q, want %q", i+1, rec.PrevHash, wantPrev)
		}
	}
}

func TestVerifyUntouchedLog(t *testing.T) {
	l, path := newTestLog(t)
	const n = 4
	writeRecords(t, l, n)

	records, firstBad, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify on untouched log: %v", err)
	}
	if records != n {
		t.Errorf("records = %d, want %d", records, n)
	}
	if firstBad != 0 {
		t.Errorf("firstBad = %d, want 0", firstBad)
	}
}

func TestVerifyEmptyLog(t *testing.T) {
	_, path := newTestLog(t) // New creates the file; no records written
	records, firstBad, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify on empty log: %v", err)
	}
	if records != 0 || firstBad != 0 {
		t.Errorf("records, firstBad = %d, %d, want 0, 0", records, firstBad)
	}
}

func TestVerifyMissingFile(t *testing.T) {
	if _, _, err := Verify(filepath.Join(t.TempDir(), "does-not-exist.log")); err == nil {
		t.Fatal("Verify on missing file returned nil error")
	}
}

// tamperSummary rewrites the file, replacing the summary value "rec-<k>" with
// "rec-<k>-edited" (valid JSON, different bytes — the tamper Verify must
// catch). The second parameter names the file's expected content; the edit
// fails loudly if the target record is not where the test expects it.
func tamperSummary(t *testing.T, path string, k int) {
	t.Helper()
	b := readLog(t, path)
	from := fmt.Sprintf(`"rec-%d"`, k)
	if !strings.Contains(string(b), from) {
		t.Fatalf("%s does not contain %s — rotation layout differs from expectation:\n%s", path, from, b)
	}
	line := strings.Replace(strings.TrimRight(string(b), "\n"), from, fmt.Sprintf(`"rec-%d-edited"`, k), 1)
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("write tampered log: %v", err)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	l, path := newTestLog(t)
	writeRecords(t, l, 4)

	// Edit rec-3 (1-based) keeping the JSON valid: the hash changes.
	tamperSummary(t, path, 3)

	records, firstBad, err := Verify(path)
	if err == nil {
		t.Fatal("Verify on tampered log returned nil error")
	}
	if firstBad != 3 {
		t.Errorf("firstBad = %d, want 3", firstBad)
	}
	if records != 4 {
		t.Errorf("records = %d, want 4 (file still has 4 records)", records)
	}
}

func TestVerifyDetectsInvalidJSON(t *testing.T) {
	l, path := newTestLog(t)
	writeRecords(t, l, 3)

	// Corrupt the second line into garbage: still detected at index 2.
	b := readLog(t, path)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	lines[1] = `{"ts":"t","caller":"master",TAMPERED`
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write tampered log: %v", err)
	}

	_, firstBad, err := Verify(path)
	if err == nil {
		t.Fatal("Verify on JSON-corrupted log returned nil error")
	}
	if firstBad != 2 {
		t.Errorf("firstBad = %d, want 2", firstBad)
	}
}

func TestVerifyDetectsBrokenChainLink(t *testing.T) {
	l, path := newTestLog(t)
	writeRecords(t, l, 3)

	// Rewrite the first record's prev_hash (must be ""): both the own-hash
	// check and the chain-head check fail at record 1.
	b := readLog(t, path)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	lines[0] = strings.Replace(lines[0], `"prev_hash":""`, `"prev_hash":"deadbeef"`, 1)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write tampered log: %v", err)
	}

	_, firstBad, err := Verify(path)
	if err == nil {
		t.Fatal("Verify on chain-broken log returned nil error")
	}
	if firstBad != 1 {
		t.Errorf("firstBad = %d, want 1", firstBad)
	}
}

func TestVerifyChainAcrossRotation(t *testing.T) {
	l, path := newTestLog(t)
	l.rotateAt = 128
	writeRecords(t, l, 2) // .1 = [rec-1], live = [rec-2]

	records, firstBad, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify across rotation: %v", err)
	}
	if records != 2 {
		t.Errorf("records = %d, want 2 (across backup + live file)", records)
	}
	if firstBad != 0 {
		t.Errorf("firstBad = %d, want 0", firstBad)
	}
}

func TestVerifyDetectsTamperingInBackup(t *testing.T) {
	l, path := newTestLog(t)
	l.rotateAt = 128
	writeRecords(t, l, 5)
	// Chain layout: .3 = [rec-2], .2 = [rec-3], .1 = [rec-4], live = [rec-5].

	// rec-3 lives in audit.log.2 (layout: .3=[rec-2], .2=[rec-3],
	// .1=[rec-4], live=[rec-5]); edit it there, keeping the JSON valid.
	tamperSummary(t, path+".2", 3)

	records, firstBad, err := Verify(path)
	if err == nil {
		t.Fatal("Verify on tampered backup returned nil error")
	}
	if firstBad != 2 {
		t.Errorf("firstBad = %d, want 2 (rec-2 in .3 is index 1, rec-3 in .2 is index 2)", firstBad)
	}
	if records != 4 {
		t.Errorf("records = %d, want 4 (rec-1 was rotated away beyond the 3-backup budget)", records)
	}
}
