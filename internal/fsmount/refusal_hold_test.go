//go:build linux

package fsmount

import (
	"bytes"
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// BFS-033: the kernel RE-ISSUES the resize, and the refusal must hold through it.
//
// MEASURED (docs/evidence/BFS-033-trace-red.txt): ONE `truncate(2)` syscall
// through a live mount produces TWO size-carrying Setattr dispatches — the second
// arrives after the mount answers ESTALE for the first — and the second carries
// the base §5.2 rule 3 adopted from the refusal (`X-Bunker-Current-Hash`), so it
// PUBLISHED and LANDED. The caller saw success; conflicts.jsonl kept a refusal
// that landing contradicted.
//
// This arm drives exactly that sequence at the handler level, where the rule can
// be asserted exactly: the same objects the mount wires together, no kernel.
// The live cell (docs/evidence/BFS-033-probes/bfs033-arms.py) is the one that
// goes RED on the unfixed tree, because the re-issue is the kernel's.

// refusalHoldBody is lowercase, so bytes.ToUpper changes the BYTES without
// changing the LENGTH — the interleaving that keeps the conflict alive.
const refusalHoldBody = "bunker-fs-bfs033"

// readBytesThroughMount performs what the kernel does for a read: Open, then Read.
// returns the bytes the mount served. THIS is the read §5.2 rule 5 names as the
// recovery from a refusal — the point at which the mount records the bytes the
// caller was handed — so it is also the only thing that clears a refusal hold.
func readBytesThroughMount(t *testing.T, m *Mount, p string) []byte {
	t.Helper()
	nd := &node{m: m, p: p}
	fh, _, errno := nd.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open(%s): errno=%v", p, errno)
	}
	h, ok := fh.(*readHandle)
	if !ok {
		t.Fatalf("Open returned %T, want *readHandle", fh)
	}
	res, errno := h.Read(context.Background(), make([]byte, 1<<16), 0)
	if errno != 0 {
		t.Fatalf("Read(%s): errno=%v", p, errno)
	}
	out, st := res.Bytes(nil)
	if st != fuse.OK {
		t.Fatalf("Read(%s) status=%v", p, st)
	}
	return out
}

// TestAReissuedResizeIsRefusedUntilTheCallerReReads is THE mount-level arm.
func TestAReissuedResizeIsRefusedUntilTheCallerReReads(t *testing.T) {
	m, target := testMount(t, refusalHoldBody)

	// The caller's read: this read IS the base hash for the write that follows.
	served := readBytesThroughMount(t, m, "target.txt")
	if !bytes.Equal(served, []byte(refusalHoldBody)) {
		t.Fatalf("fixture: the mount served %q", served)
	}
	sb := fileBytes(t, target)
	st := time.Now().Add(-time.Hour).Truncate(time.Second)
	// The concurrent edit, out of band: SAME length, and the mtime put back
	// exactly where the caller saw it (the interleaving BFS-012 measured, and the
	// one whose conflict survives the mount's mtime-based poll).
	edited := bytes.ToUpper(sb)
	if len(edited) != len(sb) {
		t.Fatalf("fixture: the edit changed the size (%d -> %d)", len(sb), len(edited))
	}
	if err := os.WriteFile(target, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, st, st); err != nil {
		t.Fatal(err)
	}

	// DISPATCH 1 — the caller's resize, against its now-stale base. Refused, with
	// the server's own verdict, and the target left alone.
	afterRead := m.client.Requests()
	if errno := setattrSize(t, m, "target.txt", nil, uint64(len(edited)-1)); errno != syscall.ESTALE {
		t.Fatalf("the resize must be refused with ESTALE, got errno=%v", errno)
	}
	if got := m.wp.Refusals(); got != 1 {
		t.Fatalf("the refusal must be recorded once, got %d", got)
	}
	if got := fileBytes(t, target); !bytes.Equal(got, edited) {
		t.Fatalf("the refused resize changed the target")
	}
	// What the first dispatch actually sent: one GET (the resize reads what it is
	// about to replace) and the refused PUT.
	firstCost := m.client.Requests() - afterRead
	if firstCost < 2 {
		t.Fatalf("the first dispatch must have cost a read and a refused PUT, got %d request(s)", firstCost)
	}

	// DISPATCH 2 — the kernel re-issues the SAME resize below the caller. On the
	// unfixed tree this publishes against the refusal's own current hash and LANDS.
	afterFirst := m.client.Requests()
	if errno := setattrSize(t, m, "target.txt", nil, uint64(len(edited)-1)); errno != syscall.ESTALE {
		t.Fatalf("THE DEFECT: the re-issued resize must be refused, got errno=%v (the refusal did not hold)", errno)
	}
	if got := fileBytes(t, target); !bytes.Equal(got, edited) {
		t.Fatalf("THE DEFECT: the re-issued resize landed — the target is now %q, want %q", got, edited)
	}
	// No PUT was made at all: the refusal happens before the request, so the
	// re-issue costs the resize's own read and nothing else.
	if cost := m.client.Requests() - afterFirst; cost > 1 {
		t.Fatalf("the held re-issue made %d request(s); at most the resize's own read is allowed", cost)
	}

	// It is REPORTED: the owner-facing document counts the hold and names it.
	st2 := m.Status()
	if st2.RefusalHolds.HeldTotal != 1 || st2.RefusalHolds.Outstanding != 1 {
		t.Fatalf("refusal_holds = %+v, want held 1 / outstanding 1", st2.RefusalHolds)
	}
	if !strings.Contains(st2.RefusalHolds.Last, "target.txt") {
		t.Fatalf("the held refusal must name the path: %q", st2.RefusalHolds.Last)
	}
	if st2.Conflicts.RefusalsTotal != 1 {
		t.Fatalf("conflicts.refusals_total = %d, want 1 (the server gave one verdict)", st2.Conflicts.RefusalsTotal)
	}

	// THE RECOVERY (§5.2 rule 5): the caller re-reads the path, and the retry
	// lands. The enforcement is not a dead end.
	//
	// WHICH BYTES the re-read serves is deliberately NOT asserted here: this
	// handler-level substrate has no invalidation channel running, so the mount
	// can serve the blob it cached before the out-of-band edit — the cache
	// staleness class filed as BFS-024/026, which this row does not touch. The
	// recovery does not depend on it: the hold is cleared by the caller having
	// read the path, and the retry publishes the bytes the resize read for itself
	// under the base the refusal recorded, so the SERVER still decides. The live
	// retry arm (docs/evidence/BFS-033-probes/bfs033-arms.py --mode retry) runs
	// with the poll live and asserts a FRESH serve.
	fresh := readBytesThroughMount(t, m, "target.txt")
	t.Logf("the re-read served %q (server now holds %q); the hold is cleared either way", fresh, edited)
	if st3 := m.Status(); st3.RefusalHolds.Outstanding != 0 {
		t.Fatalf("the caller's re-read must clear the hold: %+v", st3.RefusalHolds)
	}
	if errno := setattrSize(t, m, "target.txt", nil, uint64(len(edited)-1)); errno != 0 {
		t.Fatalf("the retry after the caller's re-read must land, got errno=%v", errno)
	}
	if got := fileBytes(t, target); !bytes.Equal(got, edited[:len(edited)-1]) {
		t.Fatalf("the retry published %q, want %q", got, edited[:len(edited)-1])
	}
}

// TestTheMountInternalReadIsNotTheCallersRead: the resize's OWN read (the
// `client.Get` inside truncate) must not clear the hold — if it did, the re-issued
// dispatch would land, which is the defect. Proven by the shape: a refused resize
// leaves the hold outstanding even though it just read the path.
func TestTheMountInternalReadIsNotTheCallersRead(t *testing.T) {
	m, target := testMount(t, refusalHoldBody)
	readBytesThroughMount(t, m, "target.txt")
	sb := fileBytes(t, target)
	st := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.WriteFile(target, bytes.ToUpper(sb), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, st, st); err != nil {
		t.Fatal(err)
	}
	if errno := setattrSize(t, m, "target.txt", nil, uint64(len(sb)-1)); errno != syscall.ESTALE {
		t.Fatalf("the resize must be refused, got errno=%v", errno)
	}
	// The refused resize read the path itself (client.Get inside truncate). The
	// hold must still stand.
	if _, outstanding, _, _ := m.wp.Held(); outstanding != 1 {
		t.Fatalf("the resize's own read cleared the hold: outstanding=%d", outstanding)
	}
	// And a second caller-facing read clears it, which is the whole difference.
	readBytesThroughMount(t, m, "target.txt")
	if _, outstanding, _, _ := m.wp.Held(); outstanding != 0 {
		t.Fatalf("a caller read must clear the hold: outstanding=%d", outstanding)
	}
}
