package fsclient

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// BFS-033: THE REFUSAL HOLDS.
//
// The defect, measured live through a real mount (docs/evidence/BFS-033-red.txt):
// ONE `truncate(2)` syscall produced a PUT that was REFUSED with 412 and recorded
// in conflicts.jsonl, immediately followed by a PUT 204 THAT LANDED, and the
// caller saw success. Two facts make that happen and neither is a client choice:
//
//   - the kernel re-issues a size-carrying SETATTR once after the mount answers
//     ESTALE (trace: two `BFS033-TRACE setattr` lines for one syscall);
//   - §5.2 rule 3 adopts the refusal's `X-Bunker-Current-Hash` as the path's base
//     — which the re-read-and-retry loop NEEDS — so the re-issued dispatch
//     published against the concurrent edit and landed, below the caller.
//
// §5.2 rule 1 already says the refusal is terminal for that write. These arms pin
// the enforcement of that sentence: a refused path is refused again until the
// CALLER has been served bytes for it (rule 5's re-read). They are the arms the
// live cell cannot replace — the live cell is the one that goes RED on the
// unfixed tree, because the re-issue is the KERNEL's.

// conflictedWritePath drives the same objects the mount wires, against the landed
// server surface. It returns a write path whose path "README.md" has already been
// refused once: the caller read it, an agent-side edit replaced it out of band,
// and the caller's write against its now-stale base was refused.
func conflictedWritePath(t *testing.T) (*WritePath, string) {
	t.Helper()
	c, root, _ := fixtureEndpoint(t)
	dir := t.TempDir()
	cache, err := OpenCache(CacheConfig{Dir: dir, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	wp := NewWritePath(c, cache, dir, OnConflictRefuse)
	ctx := context.Background()

	// 1. The caller's read through the mount: this read IS the base hash.
	_, meta, oerr := c.Get(ctx, "README.md", "")
	if oerr != nil {
		t.Fatalf("GET: %v", oerr)
	}
	if meta.Hash == "" {
		t.Fatal("the surface served no content hash; the write precondition has no base")
	}
	wp.NoteRead("README.md", meta.Hash)

	// 2. A REAL concurrent edit, out of band, and one that does NOT move the mtime
	//    away from what the caller saw (the interleaving BFS-012 measured).
	edited := []byte("# changed on the agent\n")
	if err := os.WriteFile(filepath.Join(root, "README.md"), edited, 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. The caller's write, with the base it read. REFUSED.
	_, werr := wp.PublishBytes(ctx, "README.md", []byte("# client edit\n"), WriteBase{IfMatch: meta.Hash, Source: BaseFromServed})
	if werr == nil {
		t.Fatal("the fixture must produce a refusal (the base was stale)")
	}
	if werr.Cause != CauseConflict {
		t.Fatalf("fixture refusal cause = %s, want conflict", werr.Cause)
	}
	return wp, HashBytes(edited)
}

// TestARefusalHoldsAgainstTheReissuedWrite is THE arm: the write that the server
// refused does not land afterwards, however it is re-added — including with the
// base the refusal itself corrected, which is exactly what the kernel's re-issued
// dispatch carries.
func TestARefusalHoldsAgainstTheReissuedWrite(t *testing.T) {
	wp, agentHash := conflictedWritePath(t)
	ctx := context.Background()

	// The re-issued dispatch: §5.2 rule 3 installed the refusal's current hash as
	// the base, so this is the write the kernel re-sends, base and all.
	base, berr := wp.ResolveBase(ctx, "README.md")
	if berr != nil {
		t.Fatalf("ResolveBase: %v", berr)
	}
	if base.IfMatch != agentHash {
		t.Fatalf("fixture: rule 3 must have adopted the refusal's current hash as the base (got %q)", base.IfMatch)
	}

	before := wp.client.Requests()
	_, werr := wp.PublishBytes(ctx, "README.md", []byte("# client edit\n"), base)
	if werr == nil {
		t.Fatal("THE DEFECT: the re-issued write LANDED behind the refusal")
	}
	if werr.Errno != syscall.ESTALE || werr.Cause != CauseConflict {
		t.Fatalf("the held refusal must keep the refusal's class (ESTALE/conflict): errno=%v cause=%s", werr.Errno, werr.Cause)
	}
	if werr.Verdict != VerdictHashMismatch || werr.CurrentHash != agentHash {
		t.Fatalf("the held refusal must name the standing verdict and the hash it stands on: %+v", werr)
	}
	if !strings.Contains(werr.Detail, "refusal stands") {
		t.Fatalf("the refusal must say why it holds: %q", werr.Detail)
	}
	// A refusal that publishes nothing costs nothing: not one request left the
	// client — there is no PUT to refuse at the server, because there is no PUT.
	if got := wp.client.Requests(); got != before {
		t.Fatalf("the held write made %d request(s); it must make none", got-before)
	}

	// The refusal's own guarantee, restated for the held write: the target's bytes
	// did not move.
	data, meta, oerr := wp.client.Get(ctx, "README.md", "")
	if oerr != nil {
		t.Fatalf("GET after the held refusal: %v", oerr)
	}
	if got := HashBytes(data); got != agentHash {
		t.Fatalf("the held write changed the target: %s", got)
	}
	if meta.Hash != agentHash {
		t.Fatalf("the target's hash moved: %s", meta.Hash)
	}

	// And it is REPORTED: the owner can see the refusal hold.
	held, outstanding, evicted, last := wp.Held()
	if held != 1 || outstanding != 1 || evicted != 0 {
		t.Fatalf("refusal_holds = held:%d outstanding:%d evicted:%d, want 1/1/0", held, outstanding, evicted)
	}
	if !strings.Contains(last, "README.md") || !strings.Contains(last, VerdictHashMismatch) {
		t.Fatalf("the held refusal must be named with the standing verdict: %q", last)
	}
	// The refusal itself is still counted exactly once: the server gave ONE
	// verdict, and the hold did not invent a second.
	if wp.Refusals() != 1 {
		t.Fatalf("refusals = %d, want 1 (the server's own verdicts only)", wp.Refusals())
	}
}

// TestOnlyTheCallersReadClearsTheHold: the recovery §5.2 rule 5 names clears it,
// and nothing else does — not an invalidation notice (a change notice is not a
// read), not a landed write elsewhere, not the client's own internal read.
func TestOnlyTheCallersReadClearsTheHold(t *testing.T) {
	wp, agentHash := conflictedWritePath(t)
	ctx := context.Background()

	// An invalidation of the path must NOT clear the hold: the channel telling us
	// the file changed is not the caller re-reading it.
	wp.NoteInvalidated("README.md")
	if _, outstanding, _, _ := wp.Held(); outstanding != 1 {
		t.Fatalf("an invalidation cleared the hold: outstanding=%d", outstanding)
	}
	// The client's own internal read must NOT clear it either: a resizing truncate
	// reads the path itself before publishing, and clearing the hold there is the
	// defect back again (that is what the live RED shows).
	if _, _, oerr := wp.client.Get(ctx, "README.md", ""); oerr != nil {
		t.Fatalf("GET: %v", oerr)
	}
	if _, outstanding, _, _ := wp.Held(); outstanding != 1 {
		t.Fatalf("an internal read cleared the hold: outstanding=%d", outstanding)
	}
	// The caller's read DOES clear it, and the retry then lands — the enforcement
	// is not a dead end.
	wp.NoteRead("README.md", agentHash)
	held, outstanding, _, _ := wp.Held()
	if outstanding != 0 {
		t.Fatalf("the caller's read did not clear the hold: outstanding=%d", outstanding)
	}
	res, werr := wp.PublishBytes(ctx, "README.md", []byte("# merged\n"), WriteBase{IfMatch: agentHash, Source: BaseFromFetchCheck})
	if werr != nil {
		t.Fatalf("the retry after the caller's re-read must land: %v", werr)
	}
	if res.Hash != HashBytes([]byte("# merged\n")) {
		t.Fatalf("landed hash = %q", res.Hash)
	}
	data, _, oerr := wp.client.Get(ctx, "README.md", "")
	if oerr != nil {
		t.Fatal(oerr)
	}
	if string(data) != "# merged\n" {
		t.Fatalf("the retry's bytes did not land: %q", data)
	}
	// held_total survives the recovery: it counts holds that FIRED, and this one
	// never had to (nothing was published behind the refusal).
	if held != 0 {
		t.Fatalf("held_total = %d, want 0 (nothing was published behind this refusal)", held)
	}
}

// TestACreateOnAHeldPathIsNotTheRefusedWrite: an `If-None-Match: *` publish is a
// create — the server's own rule requires the path to be ABSENT — so it is not the
// refused write. Holding it would strand a caller that removed the path and wrote
// it anew behind a refusal nothing could clear.
func TestACreateOnAHeldPathIsNotTheRefusedWrite(t *testing.T) {
	wp, _ := conflictedWritePath(t)
	ctx := context.Background()

	// The hold is armed on the path the caller is about to CREATE. (The fixture's
	// own refusal stands on README.md, so this makes two.)
	wp.armHold(Conflict{Path: "src/new.go", Code: VerdictHashMismatch, TS: time.Now()})
	if _, outstanding, _, _ := wp.Held(); outstanding != 2 {
		t.Fatalf("fixture: both holds must be armed, outstanding=%d", outstanding)
	}
	res, werr := wp.PublishBytes(ctx, "src/new.go", []byte("package main\n"), WriteBase{IfNoneMatchStar: true, Source: BaseFromAbsent})
	if werr != nil {
		t.Fatalf("a create on a held path is not the refused write and must land: %v", werr)
	}
	if res.Status != 201 {
		t.Fatalf("create status = %d, want 201 (a create of an absent path)", res.Status)
	}
	// What IS refused on that path is the MODIFICATION: the narrowing is the shape
	// of the publish, not the path alone.
	if _, werr := wp.PublishBytes(ctx, "src/new.go", []byte("package main\n\nfunc x() {}\n"), WriteBase{IfMatch: res.Hash, Source: BaseFromServed}); werr == nil {
		t.Fatal("a modification on the held path must be refused")
	}
}

// TestTheHoldIsBoundedAndTheEvictionIsReported: the structure cannot grow with the
// number of conflicts a mount ever sees, and the bound is visible rather than
// silent (PRD-bunker-invalidation.md §2.7 — a bound the owner cannot see is not a
// bound).
func TestTheHoldIsBoundedAndTheEvictionIsReported(t *testing.T) {
	// No fixture: the bound is a property of the structure, and arming it directly
	// keeps this arm from measuring the fixture's own refusal.
	wp := NewWritePath(nil, nil, "", OnConflictRefuse)
	// Arm the bound directly: the hold is armed by a real refusal, and driving
	// refusalHoldMax real refusals would measure the server, not the bound.
	for i := 0; i < refusalHoldMax+3; i++ {
		wp.armHold(Conflict{Path: fmt.Sprintf("p/%d.txt", i), Code: VerdictHashMismatch, TS: time.Now().Add(time.Duration(i) * time.Millisecond)})
	}
	held, outstanding, evicted, _ := wp.Held()
	if held != 0 {
		t.Fatalf("held_total = %d, want 0 (arming a hold is not a held write)", held)
	}
	if outstanding != refusalHoldMax {
		t.Fatalf("outstanding = %d, want the bound %d", outstanding, refusalHoldMax)
	}
	if evicted != 3 {
		t.Fatalf("evicted_total = %d, want 3 (the bound must be reported)", evicted)
	}
}
