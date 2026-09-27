package webdav

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-048's COST, as a number (SPEC-watcher-capability §7.2 R-V2).
//
// The row's (a) — make the token move for an out-of-band edit — can only be
// bought with a stat per file per read, because the only observation that sees a
// working-tree write is an observation of the working tree; there is no watcher
// in this build (BFS-035 owns that). R-V2 forbids exactly that price. So the
// token's cost must be independent of the size of the tree, and this arm
// measures it at two sizes instead of asserting it in prose: the counter would
// have to walk `paths` and the table below shows the walk's real price on the
// same fixture (the one-call snapshot op, which is a stat-only walk).
//
// The numbers are logged, so the evidence document quotes what this test
// produced on this host rather than a remembered figure.
// ---------------------------------------------------------------------------

// tokenCostBFS048 is one tree's measured costs: the token paths (memo hit and
// forced refresh) and the whole-tree read on the same fixture.
type tokenCostBFS048 struct {
	files     int
	memoHit   time.Duration
	refresh   time.Duration
	wholeTree time.Duration
	entries   int
}

// TestRevisionTokenCostIsIndependentOfTreeSize pins R-V2 for this build: the
// revision token is two file reads at most (HEAD + the ref it names), so its
// cost must not grow with the number of files the tree contains, while the
// whole-tree read — the price (a) would have paid on every response — does.
func TestRevisionTokenCostIsIndependentOfTreeSize(t *testing.T) {
	small := measureTreeCostBFS048(t, 1_000)
	large := measureTreeCostBFS048(t, 10_000)

	for _, c := range []tokenCostBFS048{small, large} {
		t.Logf("BFS-048 cost: files=%d token_memo_hit=%s token_refresh=%s whole_tree_read=%s entries=%d",
			c.files, c.memoHit, c.refresh, c.wholeTree, c.entries)
	}

	// The whole-tree read really does visit the tree, so the contrast below is
	// between two different shapes of work rather than between two fixtures.
	if small.entries >= large.entries {
		t.Fatalf("the snapshot op visited %d entries at %d files and %d at %d files: the fixtures do not differ",
			small.entries, small.files, large.entries, large.files)
	}
	// The walk's price is the price (a) would pay per response; it must be
	// visible, or this arm could not tell the two shapes apart.
	if large.wholeTree <= large.memoHit {
		t.Fatalf("the whole-tree read (%s) is not slower than a memo-hit token (%s) at %d files: the fixtures are too small to measure the difference",
			large.wholeTree, large.memoHit, large.files)
	}

	// The rule: 10x the files must not multiply the token's cost. A stat-per-file
	// implementation would show ~10x here — it would cost the whole-tree read
	// above, on every response, forever.
	for _, arm := range []struct {
		name       string
		small, big time.Duration
	}{
		{"memo hit", small.memoHit, large.memoHit},
		{"forced refresh", small.refresh, large.refresh},
	} {
		// Sub-microsecond figures are scheduler-dominated at both sizes, so the
		// ratio there measures nothing; the absolute ceilings below cover them.
		if arm.small >= 2*time.Microsecond || arm.big >= 10*time.Microsecond {
			if arm.big > 6*arm.small {
				t.Fatalf("the %s token path scaled with the tree: %s at %d files, %s at %d files (R-V2 forbids a stat-per-read walk)",
					arm.name, arm.small, small.files, arm.big, large.files)
			}
		}
	}
	// Absolute ceilings, so a tree-size-independent but expensive path cannot
	// pass either: the token is two file reads at most (HEAD, then the ref it
	// names), which cannot cost a millisecond even once the memo has expired.
	if large.memoHit > 10*time.Microsecond {
		t.Fatalf("a memo-hit token cost %s at %d files: the cached path is not a couple of loads", large.memoHit, large.files)
	}
	if large.refresh > time.Millisecond {
		t.Fatalf("a refreshed token cost %s at %d files: that is not two file reads", large.refresh, large.files)
	}
}

// measureTreeCostBFS048 builds a tree of n files, serves it, and measures the
// three costs. Only the minimum over several batches is reported, so a single
// scheduling hiccup cannot move the numbers.
func measureTreeCostBFS048(t *testing.T, n int) tokenCostBFS048 {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < n; i++ {
		dir := filepath.Join(root, fmt.Sprintf("d%02d", i%100))
		if i < 100 {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dir, err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.go", i)), []byte("package main\n"), 0o644); err != nil {
			t.Fatalf("write fixture file: %v", err)
		}
	}
	// A git work tree, so the measured kind is the one this row is about.
	withGitFixture(t, root, strings.Repeat("ab", 20))
	h := newTestHandler(t, func(c *Config) { c.Root = root })

	const iters = 200
	// Warm the memo, then measure the path a normal response takes.
	_ = h.tree.revToken()
	memoHit := minPerCallBFS048(iters, func() { _ = h.tree.revToken() })
	// Measure the path that re-reads HEAD from disk (once per 500 ms in
	// production): force the memo to expire before every call.
	refresh := minPerCallBFS048(iters, func() {
		h.tree.revAt = time.Time{}
		_ = h.tree.revToken()
	})

	start := time.Now()
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "snapshot"},
		`{"path":".","depth":"infinity"}`)
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot -> %d %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result struct {
			Count int `json:"count"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("snapshot envelope: %v", err)
	}
	return tokenCostBFS048{files: n, memoHit: memoHit, refresh: refresh, wholeTree: elapsed, entries: env.Result.Count}
}

// minPerCallBFS048 returns the smallest per-call cost seen across five batches,
// which is the most stable estimator available without a benchmark harness.
func minPerCallBFS048(iters int, fn func()) time.Duration {
	best := time.Duration(1<<62 - 1)
	for batch := 0; batch < 5; batch++ {
		start := time.Now()
		for i := 0; i < iters; i++ {
			fn()
		}
		if d := time.Since(start) / time.Duration(iters); d < best {
			best = d
		}
	}
	return best
}
