package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rec builds a Record with the given ts, agent, method, and summary.
func rec(ts string, agent, method, summary string) Record {
	return Record{TS: ts, Caller: "master", Method: method, RemoteAddr: "127.0.0.1:1", AgentID: agent, DurationMS: 1, Outcome: "ok", Summary: summary}
}

// writeQueryRecords writes n records with sequential RFC3339Nano timestamps
// (base + i seconds), alternating agents a0/a1 and methods m0/m1.
func writeQueryRecords(t *testing.T, l *AuditLog, n int, base time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		agent := fmt.Sprintf("a%d", i%2)
		method := fmt.Sprintf("/bunker.v1.Bunkerd/M%d", i%2)
		rec := rec(base.Add(time.Duration(i)*time.Second).UTC().Format(time.RFC3339Nano), agent, method, fmt.Sprintf("rec-%d", i+1))
		if err := l.Log(rec); err != nil {
			t.Fatalf("Log(rec-%d): %v", i+1, err)
		}
	}
}

func TestQuery_NoFilterReturnsAllInOrder(t *testing.T) {
	l, path := newTestLog(t)
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	writeQueryRecords(t, l, 4, base)

	got, err := Query(path, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	for i, r := range got {
		if want := fmt.Sprintf("rec-%d", i+1); r.Summary != want {
			t.Errorf("records[%d].Summary = %q, want %q (chain order oldest-first)", i, r.Summary, want)
		}
	}
}

func TestQuery_AgentFilter(t *testing.T) {
	l, path := newTestLog(t)
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	writeQueryRecords(t, l, 6, base) // agents: a0 a1 a0 a1 a0 a1

	got, err := Query(path, Filter{AgentID: "a1"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for _, r := range got {
		if r.AgentID != "a1" {
			t.Errorf("record AgentID = %q, want a1", r.AgentID)
		}
	}
}

func TestQuery_MethodFilter(t *testing.T) {
	l, path := newTestLog(t)
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	writeQueryRecords(t, l, 4, base)

	// Substring match on the full procedure.
	got, err := Query(path, Filter{Method: "/M1"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for _, r := range got {
		if r.Method != "/bunker.v1.Bunkerd/M1" {
			t.Errorf("record Method = %q, want /bunker.v1.Bunkerd/M1", r.Method)
		}
	}
}

func TestQuery_SinceUntil(t *testing.T) {
	l, path := newTestLog(t)
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	writeQueryRecords(t, l, 5, base) // ts = base .. base+4s

	since := base.Add(1 * time.Second)
	until := base.Add(3 * time.Second)
	got, err := Query(path, Filter{Since: &since, Until: &until})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (inclusive range base+1..base+3)", len(got))
	}
	for _, r := range got {
		ts, err := time.Parse(time.RFC3339Nano, r.TS)
		if err != nil {
			t.Fatalf("record ts %q unparseable: %v", r.TS, err)
		}
		if ts.Before(since) || ts.After(until) {
			t.Errorf("record ts %v outside [%v, %v]", ts, since, until)
		}
	}
}

func TestQuery_UnparseableTS(t *testing.T) {
	l, path := newTestLog(t)
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	// Record 1 has a garbage ts; records 2-3 have real timestamps.
	if err := l.Log(rec("not-a-time", "a0", "/m", "bad-ts")); err != nil {
		t.Fatalf("Log: %v", err)
	}
	writeQueryRecords(t, l, 2, base)

	// No time filter: the timestamp-less record is returned.
	all, err := Query(path, Filter{})
	if err != nil {
		t.Fatalf("Query (no filter): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len(no filter) = %d, want 3", len(all))
	}

	// Time filter: the timestamp-less record is excluded.
	since := base.Add(-1 * time.Hour)
	got, err := Query(path, Filter{Since: &since})
	if err != nil {
		t.Fatalf("Query (since): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(since) = %d, want 2 (unparseable ts excluded)", len(got))
	}
}

func TestQuery_LimitReturnsNewest(t *testing.T) {
	l, path := newTestLog(t)
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	writeQueryRecords(t, l, 5, base)

	got, err := Query(path, Filter{Limit: 2})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Summary != "rec-4" || got[1].Summary != "rec-5" {
		t.Errorf("limit did not keep the newest records: got %q, %q; want rec-4, rec-5", got[0].Summary, got[1].Summary)
	}
}

func TestQuery_ChainOrderAcrossRotation(t *testing.T) {
	l, path := newTestLog(t)
	l.rotateAt = 128 // tiny threshold: every record rotates the previous file
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	writeQueryRecords(t, l, 5, base)
	// Layout: .3=[rec-2], .2=[rec-3], .1=[rec-4], live=[rec-5]; rec-1 dropped.

	got, err := Query(path, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4 (rec-1 rotated away beyond backup budget)", len(got))
	}
	for i, r := range got {
		if want := fmt.Sprintf("rec-%d", i+2); r.Summary != want {
			t.Errorf("records[%d].Summary = %q, want %q (oldest backup first, live file last)", i, r.Summary, want)
		}
	}
}

func TestQuery_LosslessHashChain(t *testing.T) {
	l, path := newTestLog(t)
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	writeQueryRecords(t, l, 3, base)

	got, err := Query(path, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for i, r := range got {
		if r.Hash == "" {
			t.Errorf("records[%d].Hash is empty — export would not be lossless", i)
		}
		if i > 0 && r.PrevHash != got[i-1].Hash {
			t.Errorf("records[%d].PrevHash = %q, want records[%d].Hash %q (chain preserved)", i, r.PrevHash, i-1, got[i-1].Hash)
		}
	}
	// Re-marshalling every returned record must reproduce the file bytes.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var want string
	for _, line := range splitLines(t, string(b)) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line not JSON: %v", err)
		}
		want += string(mustMarshal(t, m))
	}
	var gotBytes string
	for _, r := range got {
		var m map[string]any
		if err := json.Unmarshal(mustMarshal(t, r), &m); err != nil {
			t.Fatalf("re-marshal query result: %v", err)
		}
		gotBytes += string(mustMarshal(t, m))
	}
	if want != gotBytes {
		t.Error("re-marshalled query results differ from the on-disk log — export is not lossless")
	}
}

func TestQuery_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.log")
	if _, err := Query(path, Filter{}); err == nil {
		t.Fatal("Query on a missing log returned nil error")
	}
}

func TestQuery_SkipsGarbageLines(t *testing.T) {
	l, path := newTestLog(t)
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	if err := l.Log(rec(base.UTC().Format(time.RFC3339Nano), "a0", "/m", "good")); err != nil {
		t.Fatalf("Log: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString("this is not json\n"); err != nil {
		t.Fatalf("append garbage: %v", err)
	}
	f.Close()

	got, err := Query(path, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (garbage line skipped)", len(got))
	}
}

// splitLines splits s on newlines, dropping the trailing empty element.
func splitLines(t *testing.T, s string) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// mustMarshal marshals v, failing the test on error.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
