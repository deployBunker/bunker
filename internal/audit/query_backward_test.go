package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// shrinkChunks overrides backwardChunkSize for the duration of fn so the
// backward reader's chunk-boundary (partial-line) paths are exercised on
// small fixtures, and restores the previous value afterwards. A chunk of 17
// bytes guarantees most records straddle two or more chunk reads.
func shrinkChunks(t *testing.T, chunk int64, fn func()) {
	t.Helper()
	old := backwardChunkSize
	backwardChunkSize = chunk
	defer func() { backwardChunkSize = old }()
	fn()
}

// writeRawAudit writes lines to path with NO trailing newline after the last
// one when trailingNewline is false, building files the AuditLog writer
// would never produce itself (torn final record, garbage tails).
func writeRawAudit(t *testing.T, path string, lines []string, trailingNewline bool) {
	t.Helper()
	body := strings.Join(lines, "\n")
	if trailingNewline && body != "" {
		body += "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// rawRecord marshals a Record with the given summary (and a distinct ts) to
// a JSON line, for hand-built fixtures.
func rawRecord(t *testing.T, i int) string {
	t.Helper()
	ts := time.Date(2026, 9, 1, 12, 0, 0, i, time.UTC).Format(time.RFC3339Nano)
	b, err := json.Marshal(rec(ts, fmt.Sprintf("a%d", i%2), fmt.Sprintf("/bunker.v1.Bunkerd/M%d", i%2), fmt.Sprintf("rec-%d", i+1)))
	if err != nil {
		t.Fatalf("marshal rec-%d: %v", i+1, err)
	}
	return string(b)
}

// queryForwardGolden runs the query FORCING the forward path (the Observer
// field disables the PERF-003 backward fast path by contract), giving the
// reference result every parity arm compares against. The observer's
// invocations are counted so the parity arms additionally prove the forward
// scan really ran: minLines is how many lines the fixture is known to have
// (an empty-file arm passes 0 and skips the check — there is nothing to
// observe). A silent fallback of the forward path to backward would make
// the comparison vacuous, so the check fails loudly.
func queryForwardGolden(t *testing.T, path string, f Filter, minLines int) []Record {
	t.Helper()
	var lines int
	f.Observer = func(line []byte, err error) error {
		lines++
		return nil
	}
	got, err := Query(path, f)
	if err != nil {
		t.Fatalf("forward query %s: %v", path, err)
	}
	if lines < minLines {
		t.Fatalf("forward query observed %d lines at %s, want >= %d — forward path did not run", lines, path, minLines)
	}
	return got
}

// recordSummaries extracts the Summary field of each record for order
// comparisons. (Named recordSummaries: audit_lifecycle_test.go already owns
// the package-level `summaries` name for its raw-bytes variant.)
func recordSummaries(recs []Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Summary
	}
	return out
}

// TestQueryBackward_ParityWithForward is acceptance criterion 2a/2e: for
// every fixture and every limit, the backward path (taken automatically by
// Query when the filter has a limit and no window) returns EXACTLY the
// records the forward path returns, in the same order. Fixtures cover plain
// files, rotation chains, agent/method filters, garbage lines, and — via
// shrinkChunks — files many chunks large with lines straddling boundaries.
func TestQueryBackward_ParityWithForward(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// chainFixture writes n records through the real AuditLog so hash
	// chains and (when rotateAt is tiny) rotation chains are realistic.
	chainFixture := func(t *testing.T, n int, rotateAt int) (string, *AuditLog) {
		l, path := newTestLog(t)
		if rotateAt > 0 {
			l.rotateAt = int64(rotateAt)
		}
		for i := 0; i < n; i++ {
			ts := base.Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339Nano)
			if err := l.Log(rec(ts, fmt.Sprintf("a%d", i%2), fmt.Sprintf("/bunker.v1.Bunkerd/M%d", i%2), fmt.Sprintf("rec-%d", i+1))); err != nil {
				t.Fatalf("Log(rec-%d): %v", i+1, err)
			}
		}
		return path, l
	}
	// garbageFixture appends two unparseable lines (one without newline at
	// EOF) to a valid 6-record log.
	garbageFixture := func(t *testing.T) string {
		path, l := chainFixture(t, 6, 0)
		_ = l
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		defer f.Close()
		if _, err := f.WriteString("this is not json\n{also not json"); err != nil {
			t.Fatalf("append garbage: %v", err)
		}
		return path
	}

	fixtures := []struct {
		name  string
		build func(t *testing.T) string
	}{
		{"plain 8-record log", func(t *testing.T) string { p, _ := chainFixture(t, 8, 0); return p }},
		{"rotation chain (rotateAt 64)", func(t *testing.T) string { p, _ := chainFixture(t, 8, 64); return p }},
		{"garbage lines incl. unterminated tail", garbageFixture},
		{"hand-built no-trailing-newline", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "audit.log")
			lines := make([]string, 5)
			for i := range lines {
				lines[i] = rawRecord(t, i)
			}
			writeRawAudit(t, path, lines, false) // last line has no '\n'
			return path
		}},
	}

	type arm struct {
		name   string
		filter Filter
	}
	arms := []arm{
		{"limit 1", Filter{Limit: 1}},
		{"limit 2", Filter{Limit: 2}},
		{"limit 3", Filter{Limit: 3}},
		{"limit 5", Filter{Limit: 5}},
		{"limit larger than the file", Filter{Limit: 100}},
		{"limit 2 + agent filter", Filter{AgentID: "a1", Limit: 2}},
		{"limit 2 + method filter", Filter{Method: "/M1", Limit: 2}},
		{"limit 3 + both filters", Filter{AgentID: "a0", Method: "/M0", Limit: 3}},
	}

	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			// Chunk sizes chosen so the last chunk ends mid-line, exactly
			// on a newline, and short of one record respectively.
			for _, chunk := range []int64{17, 64, 10 << 20} {
				t.Run(fmt.Sprintf("chunk=%d", chunk), func(t *testing.T) {
					path := fx.build(t)
					for _, a := range arms {
						t.Run(a.name, func(t *testing.T) {
							shrinkChunks(t, chunk, func() {
								before := backwardReads.Load()
								got, err := Query(path, a.filter)
								if err != nil {
									t.Fatalf("Query: %v", err)
								}
								// Delta counts chain files read backward (live +
								// existing backups), so >= 1 is the wired proof;
								// the exact per-file count is an implementation detail.
								if backwardReads.Load() <= before {
									t.Fatalf("backward path not taken for %+v — parity comparison would be vacuous", a.filter)
								}
								want := queryForwardGolden(t, path, a.filter, 1)
								if len(got) != len(want) {
									t.Fatalf("backward len = %d (%v), forward len = %d (%v)", len(got), recordSummaries(got), len(want), recordSummaries(want))
								}
								if !reflect.DeepEqual(got, want) {
									t.Fatalf("backward != forward:\n backward: %v\n forward:  %v", recordSummaries(got), recordSummaries(want))
								}
							})
						})
					}
				})
			}
		})
	}
}

// TestQueryBackward_DiscoveryEdges is criterion 2b/2c: an empty file and a
// one-line file return the same result on both paths; a trailing record
// without a newline is returned by the backward path exactly as the forward
// path returns it (including when it is the ONLY thing in the file).
func TestQueryBackward_DiscoveryEdges(t *testing.T) {
	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "audit.log")
		writeRawAudit(t, path, nil, true)

		shrinkChunks(t, 16, func() {
			before := backwardReads.Load()
			got, err := Query(path, Filter{Limit: 5})
			if err != nil {
				t.Fatalf("Query(empty): %v", err)
			}
			if backwardReads.Load() != before+1 {
				t.Fatal("backward path not taken for the empty-file arm")
			}
			if len(got) != 0 {
				t.Fatalf("empty file returned %d records, want 0", len(got))
			}
			want := queryForwardGolden(t, path, Filter{Limit: 5}, 0)
			if len(want) != 0 {
				t.Fatalf("forward path returned %d records on an empty file, want 0", len(want))
			}
		})
	})

	t.Run("one-line file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "audit.log")
		writeRawAudit(t, path, []string{rawRecord(t, 0)}, true)

		shrinkChunks(t, 8, func() { // chunk far smaller than the one line
			before := backwardReads.Load()
			got, err := Query(path, Filter{Limit: 10})
			if err != nil {
				t.Fatalf("Query(one-line): %v", err)
			}
			if backwardReads.Load() != before+1 {
				t.Fatal("backward path not taken for the one-line arm")
			}
			want := queryForwardGolden(t, path, Filter{Limit: 10}, 1)
			if len(got) != 1 || !reflect.DeepEqual(got, want) {
				t.Fatalf("one-line file: backward = %v, forward = %v", recordSummaries(got), recordSummaries(want))
			}
		})
	})

	t.Run("single unterminated line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "audit.log")
		writeRawAudit(t, path, []string{rawRecord(t, 3)}, false)

		shrinkChunks(t, 8, func() {
			got, err := Query(path, Filter{Limit: 4})
			if err != nil {
				t.Fatalf("Query(unterminated): %v", err)
			}
			want := queryForwardGolden(t, path, Filter{Limit: 4}, 1)
			if len(got) != 1 || !reflect.DeepEqual(got, want) {
				t.Fatalf("unterminated single line: backward = %v, forward = %v", recordSummaries(got), recordSummaries(want))
			}
			if got[0].Summary != "rec-4" {
				t.Fatalf("unterminated final record not parsed: got %v", recordSummaries(got))
			}
		})
	})

	t.Run("no trailing newline on the final record", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "audit.log")
		lines := make([]string, 4)
		for i := range lines {
			lines[i] = rawRecord(t, i)
		}
		writeRawAudit(t, path, lines, false)

		shrinkChunks(t, 20, func() {
			for _, limit := range []int{1, 2, 4, 9} {
				got, err := Query(path, Filter{Limit: limit})
				if err != nil {
					t.Fatalf("Query(limit %d): %v", limit, err)
				}
				want := queryForwardGolden(t, path, Filter{Limit: limit}, 1)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("limit %d: backward = %v, forward = %v", limit, recordSummaries(got), recordSummaries(want))
				}
				if limit == 1 && (len(got) != 1 || got[0].Summary != "rec-4") {
					t.Fatalf("limit 1 did not select the unterminated final record: %v", recordSummaries(got))
				}
			}
		})
	})
}

// TestQueryBackward_WindowQueriesUseForwardPath is criterion 2d: a filter
// carrying Since or Until — or an Observer, which needs the whole scan —
// must NOT take the backward path. The backwardReads counter is the seam.
func TestQueryBackward_WindowQueriesUseForwardPath(t *testing.T) {
	l, path := newTestLog(t)
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	writeQueryRecords(t, l, 6, base)

	shrinkChunks(t, 16, func() {
		// Control: a plain limited query moves the counter (proves the
		// seam is actually wired to the backward path).
		before := backwardReads.Load()
		if _, err := Query(path, Filter{Limit: 2}); err != nil {
			t.Fatalf("control query: %v", err)
		}
		if backwardReads.Load() != before+1 {
			t.Fatal("seam control failed: a limited window-less query did not take the backward path")
		}

		since := base.Add(1 * time.Second)
		until := base.Add(3 * time.Second)
		windowed := []struct {
			name   string
			filter Filter
		}{
			{"since only", Filter{Since: &since, Limit: 2}},
			{"until only", Filter{Until: &until, Limit: 2}},
			{"since+until", Filter{Since: &since, Until: &until, Limit: 2}},
			{"since without limit", Filter{Since: &since}},
			{"observer forces forward", Filter{Limit: 2, Observer: func([]byte, error) error { return nil }}},
		}
		for _, tt := range windowed {
			t.Run(tt.name, func(t *testing.T) {
				before := backwardReads.Load()
				got, err := Query(path, tt.filter)
				if err != nil {
					t.Fatalf("Query(%s): %v", tt.name, err)
				}
				if backwardReads.Load() != before {
					t.Errorf("%s took the backward path (backwardReads %d -> %d); time windows and observers must use the forward scan", tt.name, before, backwardReads.Load())
				}
				want := queryForwardGolden(t, path, tt.filter, 1)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s: got %v, want %v", tt.name, recordSummaries(got), recordSummaries(want))
				}
			})
		}
	})
}

// TestQueryBackward_GuardTable pins the backwardGuard decision itself so
// the dispatch stays a pure function of the filter.
func TestQueryBackward_GuardTable(t *testing.T) {
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	noop := func([]byte, error) error { return nil }
	tests := []struct {
		name   string
		filter Filter
		want   bool
	}{
		{"limit without window", Filter{Limit: 5}, true},
		{"zero limit", Filter{}, false},
		{"negative limit", Filter{Limit: -1}, false},
		{"since", Filter{Since: &since, Limit: 5}, false},
		{"until", Filter{Until: &until, Limit: 5}, false},
		{"observer", Filter{Limit: 5, Observer: noop}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backwardGuard(tt.filter); got != tt.want {
				t.Errorf("backwardGuard(%+v) = %v, want %v", tt.filter, got, tt.want)
			}
		})
	}
}

// Benchmark fixtures: a 60k-record audit file in the temp dir, matching the
// perf-lane measurement the PERF-003 row cites (60k lines ≈ 215ms forward).
func buildBenchFile(b *testing.B, n int) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "audit.log")
	f, err := os.Create(path)
	if err != nil {
		b.Fatalf("create: %v", err)
	}
	defer f.Close()
	ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	w := new(strings.Builder)
	for i := 0; i < n; i++ {
		line, err := json.Marshal(rec(ts.Add(time.Duration(i)*time.Second).UTC().Format(time.RFC3339Nano), fmt.Sprintf("agent-%d", i%7), "/bunker.v1.Bunkerd/SpawnAgent", fmt.Sprintf("rec-%d", i+1)))
		if err != nil {
			b.Fatalf("marshal: %v", err)
		}
		w.Write(line)
		w.WriteByte('\n')
		if w.Len() >= 1<<20 {
			if _, err := f.WriteString(w.String()); err != nil {
				b.Fatalf("write: %v", err)
			}
			w.Reset()
		}
	}
	if _, err := f.WriteString(w.String()); err != nil {
		b.Fatalf("write: %v", err)
	}
	return path
}

// BenchmarkAuditListLimit20Backward measures `audit list --limit 20` on a
// 60k-line log through the backward path (PERF-003's target shape).
func BenchmarkAuditListLimit20Backward(b *testing.B) {
	path := buildBenchFile(b, 60000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recs, err := Query(path, Filter{Limit: 20})
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		if len(recs) != 20 {
			b.Fatalf("len = %d, want 20", len(recs))
		}
	}
}

// BenchmarkAuditListLimit20Forward is the control: the same query forced
// through the forward path (Observer disables the fast path), i.e. the
// pre-PERF-003 behaviour.
func BenchmarkAuditListLimit20Forward(b *testing.B) {
	path := buildBenchFile(b, 60000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recs, err := Query(path, Filter{Limit: 20, Observer: func([]byte, error) error { return nil }})
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		if len(recs) != 20 {
			b.Fatalf("len = %d, want 20", len(recs))
		}
	}
}

// BenchmarkAuditListFullForward pins the unbounded query (no limit) so the
// full-list number stays measurable after PERF-003 (it keeps the forward
// scan by design — there is no limit to bound it).
func BenchmarkAuditListFullForward(b *testing.B) {
	path := buildBenchFile(b, 60000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recs, err := Query(path, Filter{})
		if err != nil {
			b.Fatalf("query: %v", err)
		}
		if len(recs) != 60000 {
			b.Fatalf("len = %d, want 60000", len(recs))
		}
	}
}
