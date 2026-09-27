package webdav

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// BFS-049, the arm that asserts surface this row ADDS.
//
// It lives in its own file because the arms script sets it aside for the RED
// run: the filed tree does not have IdentityDivergenceCounters, so the arm
// cannot compile against it. Everything here is GREEN-only, and the RED cells
// — the ones that must fail on the filed tree — are in identity_bfs049_test.go,
// written against surface that existed before this row. Same split, same
// reason, as BFS-062's invalidate_bfs062_rule_test.go.
// ---------------------------------------------------------------------------

// TestBFS049TheFormerDivergenceClassIsReportedNotSilent pins the backstop.
//
// Sharing the identity is what makes TODAY's two observers agree, and no test
// of today's pair can catch a FUTURE observer that brings its own tuple back:
// it would simply be a third definition, and it would agree with neither
// assertion above. So the class this row closed is counted where it is
// classified — a path whose observed identity moved while the pre-BFS-049
// (size, mtime) key compared equal — and the count is readable from outside the
// package (IdentityDivergenceCounters, the same shape as
// FrameOverBoundCounters).
func TestBFS049TheFormerDivergenceClassIsReportedNotSilent(t *testing.T) {
	h := newTestHandler(t)
	target := filepath.Join(h.Root(), "src", "util.go")
	cursor := seedWithSnapshot(t, h)

	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if ctimeUnixNano(before) == 0 {
		t.Skip("this platform exposes no ctime: no observation here can be in this class (filetime_other.go)")
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	countBefore, _ := IdentityDivergenceCounters()

	edited := []byte(strings.ReplaceAll(string(body), "util", "uti1"))
	if len(edited) != len(body) {
		t.Fatalf("the fixture is not the trap it claims to be: %d -> %d bytes", len(body), len(edited))
	}
	if err := os.WriteFile(target, edited, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(target, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if _, _, moved := ledgerVerdictBFS049(t, h, cursor, "src/util.go"); !moved {
		t.Fatalf("the ledger did not report the edit, so nothing here was classified")
	}

	countAfter, lastPath := IdentityDivergenceCounters()
	t.Logf("BFS-049 report: metadata-key-blind moves %d -> %d, last path %q", countBefore, countAfter, lastPath)
	if countAfter != countBefore+1 {
		t.Fatalf("the (size, mtime)-blind class is not being reported: counter %d -> %d across one such edit (want exactly one)", countBefore, countAfter)
	}
	if lastPath != "src/util.go" {
		t.Fatalf("the report names %q, want the path the class was found on (src/util.go)", lastPath)
	}
}

// TestBFS049TheReportNamesTheClassAndNotEveryChange is the anti-overcount arm,
// and it is what keeps the number above from being a metric that describes
// something other than what it claims: if every change counted, the counter
// would be a change counter with a sharper name and would say nothing about the
// divergence. A change that moves mtime is NOT in the class; a metadata-only
// change (ctime alone) IS.
func TestBFS049TheReportNamesTheClassAndNotEveryChange(t *testing.T) {
	h := newTestHandler(t)
	target := filepath.Join(h.Root(), "src", "util.go")
	cursor := seedWithSnapshot(t, h)

	// (1) an ordinary edit: different bytes, mtime moves with them.
	countBefore, _ := IdentityDivergenceCounters()
	mustWrite(t, target, "package main\n\nfunc util() { /* moved mtime */ }\n")
	if _, _, moved := ledgerVerdictBFS049(t, h, cursor, "src/util.go"); !moved {
		t.Fatalf("the ordinary edit was not reported at all")
	}
	countAfter, _ := IdentityDivergenceCounters()
	t.Logf("BFS-049 report: an edit that moves mtime — counter %d -> %d (want no move: it is not in the class)", countBefore, countAfter)
	if countAfter != countBefore {
		t.Fatalf("a change that moved mtime was counted as (size, mtime)-blind: counter %d -> %d, which makes the number mean something other than the class it names", countBefore, countAfter)
	}

	// (2) a metadata-only move: ctime alone, asserted inside the stimulus.
	// The client's cursor advances to the ledger's head after it has read the
	// events above, which is the resume point a real client would present next.
	cursor = pollAt(t, h, cursor).Result.HeadSeq
	countBefore, _ = IdentityDivergenceCounters()
	moved, why := moveCtimeOnlyBFS049(t, target)
	if !moved {
		t.Skipf("ctime cannot be moved without moving size or mtime here: %s", why)
	}
	if _, _, reported := ledgerVerdictBFS049(t, h, cursor, "src/util.go"); !reported {
		t.Fatalf("the ledger did not report a mode change, so the class this arm counts was never reachable")
	}
	countAfter, lastPath := IdentityDivergenceCounters()
	t.Logf("BFS-049 report: a mode change (ctime alone) — counter %d -> %d, last %q (want exactly one: it IS the class)", countBefore, countAfter, lastPath)
	if countAfter != countBefore+1 {
		t.Fatalf("a metadata-only move was not counted: counter %d -> %d (want exactly one)", countBefore, countAfter)
	}
	if lastPath != "src/util.go" {
		t.Fatalf("the report names %q, want src/util.go", lastPath)
	}
}
