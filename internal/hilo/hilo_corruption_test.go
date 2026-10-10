package hilo

// hilo_corruption_test.go — QA-BUNKER-B12 + QA-BUNKER-9 (folded): pin the
// DEFINED crash/recovery behavior of the graph state store under truncation
// and corruption.
//
// Ground truth being pinned (verified against the loader, not invented): the
// QA chaos cell truncated .vfs/graph/graph.db and watched the next start.
// graph.db is the Hilo CLI's own store and NO code in this repo reads it —
// bunkerd reads ONLY .vfs/graph/edges.jsonl (load). A per-line JSON failure
// is warn-and-skip, a scanner-level failure is a returned error, a missing
// file is warn-plus-empty, and internal/server/server.go:168-173 turns a
// failed load into "warn + nil graph + keep serving". These tests make each
// of those outcomes contractual so a future loader change cannot silently
// re-open the QA finding.

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newCapturingLogger returns a logger at WARN level plus the buffer it writes
// to, so tests can assert a skip was ANNOUNCED rather than silent.
func newCapturingLogger() (*slog.Logger, *strings.Builder) {
	var buf strings.Builder
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &buf
}

// writeEdgesFile creates .vfs/graph/edges.jsonl under dir with the exact raw
// content given (callers control corruption byte-for-byte) and returns the
// file path.
func writeEdgesFile(t *testing.T, dir, content string) string {
	t.Helper()
	graphDir := filepath.Join(dir, ".vfs", "graph")
	if err := os.MkdirAll(graphDir, 0o755); err != nil {
		t.Fatalf("create graph dir: %v", err)
	}
	path := filepath.Join(graphDir, "edges.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write edges file: %v", err)
	}
	return path
}

// validEdgeLines is the valid baseline every corruption case mutates: three
// complete, well-formed edges.
func validEdgeLines() []string {
	return []string{
		edgeToJSON(Edge{From: "internal/alpha/a.go", To: "std:fmt", Rel: "imports"}),
		edgeToJSON(Edge{From: "internal/beta/b.go", To: "internal/alpha/a.go", Rel: "imports"}),
		edgeToJSON(Edge{From: "internal/gamma/g.go", To: "internal/beta/b.go", Rel: "imports"}),
	}
}

func validFixture() string { return strings.Join(validEdgeLines(), "\n") + "\n" }

// TestNewGraph_CorruptAndTruncatedInput pins NewGraph's outcome for every
// damaged-input class the QA cells and the loader can produce: load
// succeeds, only the damaged lines are lost, and every loss is announced on
// the WARN stream.
func TestNewGraph_CorruptAndTruncatedInput(t *testing.T) {
	lines := validEdgeLines()
	partial := func(s string) string { return s[:len(s)*3/4] } // cut mid-value, no closing brace

	cases := []struct {
		name      string
		content   string
		noFile    bool
		wantEdges int
		wantWarn  string // substring that MUST appear in the WARN stream ("" = load must be silent)
	}{
		{
			name:      "truncated mid-line: partial final JSON object skipped, complete edges load",
			content:   validFixture() + partial(lines[0]),
			wantEdges: 3,
			wantWarn:  "skipping malformed edge line",
		},
		{
			name:      "corrupt line between valid lines: only the bad line is lost",
			content:   strings.Join([]string{lines[0], `{"from": NOT-JSON`, lines[1]}, "\n") + "\n",
			wantEdges: 2,
			wantWarn:  "skipping malformed edge line",
		},
		{
			name:      "blank lines hit the warn-and-skip path (empty bytes fail json.Unmarshal)",
			content:   strings.Join([]string{lines[0], "", lines[1]}, "\n") + "\n",
			wantEdges: 2,
			wantWarn:  "skipping malformed edge line",
		},
		{
			name:      "empty file: empty graph, no error, no warning",
			content:   "",
			wantEdges: 0,
		},
		{
			name:      "missing edges.jsonl: empty graph, warning, no error",
			noFile:    true,
			wantEdges: 0,
			wantWarn:  "edges file not found",
		},
		{
			name:      "valid JSON that is not an edge object unmarshals to a zero-value edge (loader validates syntax, not fields)",
			content:   strings.Join([]string{lines[0], `{}`, lines[1]}, "\n") + "\n",
			wantEdges: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.noFile {
				if err := os.MkdirAll(filepath.Join(dir, ".vfs", "graph"), 0o755); err != nil {
					t.Fatalf("create graph dir: %v", err)
				}
			} else {
				writeEdgesFile(t, dir, tc.content)
			}
			logger, buf := newCapturingLogger()

			g, err := NewGraph(dir, logger)

			if err != nil {
				t.Fatalf("NewGraph on damaged input must not error: %v", err)
			}
			if g == nil {
				t.Fatal("NewGraph returned nil")
			}
			if got := g.Stats().TotalEdges; got != tc.wantEdges {
				t.Fatalf("TotalEdges = %d, want %d (warn stream: %q)", got, tc.wantEdges, buf.String())
			}
			if tc.wantWarn == "" {
				if buf.Len() != 0 {
					t.Fatalf("expected a silent load, got warnings: %q", buf.String())
				}
			} else if !strings.Contains(buf.String(), tc.wantWarn) {
				t.Fatalf("expected warning containing %q, got: %q", tc.wantWarn, buf.String())
			}
			if tc.wantEdges > 0 {
				// Wiring proof: whatever survived the load must be reachable
				// through the lookup indexes, not just counted.
				if edges := g.Related("internal/alpha/a.go"); len(edges) == 0 {
					t.Fatal("complete edges must stay reachable via Related after a damaged load")
				}
			}
		})
	}
}

// TestNewGraph_LineTooLongErrors pins the ONE corrupt-input shape that fails
// the whole load: a single line beyond bufio.Scanner's default 64KiB token
// buffer makes NewGraph return a "scan edges.jsonl" error (and the daemon
// absorbs that at internal/server/server.go:168-173 into warn + nil graph +
// keep serving).
func TestNewGraph_LineTooLongErrors(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("a", 128*1024) // 2x bufio.MaxScanTokenSize, no newline
	writeEdgesFile(t, dir, validEdgeLines()[0]+"\n"+huge)

	logger, _ := newCapturingLogger()
	g, err := NewGraph(dir, logger)
	if err == nil {
		t.Fatal("a line beyond bufio.Scanner's token buffer must FAIL the load, not be silently skipped")
	}
	if !strings.Contains(err.Error(), "scan edges.jsonl") {
		t.Fatalf("error should name the scan step, got: %v", err)
	}
	if g != nil {
		t.Fatal("NewGraph must return a nil graph on a failed load")
	}
}

// TestNewGraph_UnreadableFileErrors pins the missing-vs-unreadable split: a
// MISSING file is tolerated (warn + empty graph), an UNREADABLE one
// (permissions, IO) is a returned error. Skipped where permission bits are
// not enforced (root), after probing that the mode really denies the open.
func TestNewGraph_UnreadableFileErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 000 does not deny opens, case untestable")
	}
	dir := t.TempDir()
	path := writeEdgesFile(t, dir, validFixture())
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod 000: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	// Premise check: the environment must actually deny the open, otherwise
	// the error assertion below would grade a fiction.
	if f, oerr := os.Open(path); oerr == nil {
		_ = f.Close()
		t.Skip("environment ignores file permission bits; unreadable-file case not testable here")
	}

	logger, _ := newCapturingLogger()
	g, err := NewGraph(dir, logger)
	if err == nil {
		t.Fatal("an unreadable edges.jsonl must return an error from NewGraph")
	}
	if !strings.Contains(err.Error(), "open edges.jsonl") {
		t.Fatalf("error should name the open step, got: %v", err)
	}
	if g != nil {
		t.Fatal("NewGraph must return a nil graph on a failed load")
	}
}

// TestGraph_Reload_AfterCorruption pins recovery: Reload drops ALL in-memory
// state and re-reads the file, so whatever corruption did to the file, the
// next Reload converges on the fresh state without panicking — and a Reload
// that FAILS leaves the graph empty rather than stale.
func TestGraph_Reload_AfterCorruption(t *testing.T) {
	const alphaFrom = "internal/alpha/a.go"

	newLoadedGraph := func(t *testing.T, content string) (*Graph, string) {
		t.Helper()
		dir := t.TempDir()
		writeEdgesFile(t, dir, content)
		logger, _ := newCapturingLogger()
		g, err := NewGraph(dir, logger)
		if err != nil || g == nil {
			t.Fatalf("NewGraph: err=%v g=%v", err, g)
		}
		return g, dir
	}

	t.Run("file replaced by garbage: Reload converges on an empty graph", func(t *testing.T) {
		g, dir := newLoadedGraph(t, validFixture())
		writeEdgesFile(t, dir, "not json at all\n")
		if err := g.Reload(); err != nil {
			t.Fatalf("Reload over garbage must not error: %v", err)
		}
		if got := g.Stats().TotalEdges; got != 0 {
			t.Fatalf("after garbage Reload TotalEdges = %d, want 0", got)
		}
		if edges := g.Related(alphaFrom); len(edges) != 0 {
			t.Fatalf("stale edges survived Reload: %v", edges)
		}
	})

	t.Run("file truncated to one complete plus one partial line: Reload keeps the complete one", func(t *testing.T) {
		g, dir := newLoadedGraph(t, validFixture())
		lines := validEdgeLines()
		writeEdgesFile(t, dir, lines[0]+"\n"+lines[1][:len(lines[1])/2])
		if err := g.Reload(); err != nil {
			t.Fatalf("Reload over truncation must not error: %v", err)
		}
		if got := g.Stats().TotalEdges; got != 1 {
			t.Fatalf("after truncation Reload TotalEdges = %d, want 1", got)
		}
	})

	t.Run("file deleted: Reload converges on an empty graph, nil error", func(t *testing.T) {
		g, dir := newLoadedGraph(t, validFixture())
		path := filepath.Join(dir, ".vfs", "graph", "edges.jsonl")
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove edges file: %v", err)
		}
		if err := g.Reload(); err != nil {
			t.Fatalf("Reload after deletion must not error: %v", err)
		}
		if got := g.Stats().TotalEdges; got != 0 {
			t.Fatalf("after deletion Reload TotalEdges = %d, want 0", got)
		}
	})

	t.Run("scanner-level failure (line too long): Reload errors and leaves the graph EMPTY, not stale", func(t *testing.T) {
		g, dir := newLoadedGraph(t, validFixture())
		writeEdgesFile(t, dir, strings.Repeat("a", 128*1024))
		err := g.Reload()
		if err == nil || !strings.Contains(err.Error(), "scan edges.jsonl") {
			t.Fatalf("Reload over a too-long line: err = %v, want a scan edges.jsonl error", err)
		}
		if got := g.Stats().TotalEdges; got != 0 {
			t.Fatalf("a failed Reload must leave the graph empty (not stale): TotalEdges = %d", got)
		}
	})
}
