//go:build linux

package fsmount

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/deployBunker/bunker/internal/fsclient"
)

// BFS-021: an append through the mount must LAND its bytes, or be refused LOUDLY
// without losing any.
//
// MEASURED on the tree this row started from (docs/evidence/BFS-021-trace.txt, one
// trace line per FUSE request): `>>` arrives as `open(O_WRONLY|O_APPEND)` — flags
// 0x8401 — and then as a WRITE at an offset equal to the current size, on a handle
// that is not an fs.FileWriter, so go-fuse's bridge answered ENOTSUP (errno 95)
// and the caller's bytes were dropped with nothing ever sent. One shape of the
// same defect is worse than the filed one: a shell that writes several chunks
// inside ONE open (`exec 3>>f`) reports SUCCESS and loses all of them, because a
// failed write never grew i_size and every chunk was re-issued at the same offset.
//
// These are handler-level cells: they drive the SAME objects a live mount wires
// together (a real client against the landed surface, the cache, the snapshot,
// the bound registry), so the RULE can be asserted exactly. The live arms that
// measure the real kernel path are docs/evidence/BFS-021-probes/ (run-arms.sh),
// and they are the ones that go RED on the unfixed tree.
//
// Every cell below has a source mutation that turns it red, and an ATTRIBUTION
// cell that must stay green under that mutation: docs/evidence/BFS-021-arms.sh.

const appendTarget = "target.txt"

// openAppend performs what the kernel does for `>>`: Open with O_WRONLY|O_APPEND.
// The handle it returns is the READ handle (BFS-030 measured that, and changing it
// would refuse reads through an O_RDWR handle, a capability that works today) —
// carrying the append state, which is the whole of this row's write path.
func openAppend(t *testing.T, m *Mount, p string) *readHandle {
	t.Helper()
	fh, _, errno := (&node{m: m, p: p}).Open(context.Background(), syscall.O_WRONLY|syscall.O_APPEND)
	if errno != 0 {
		t.Fatalf("Open(%s, O_WRONLY|O_APPEND): errno=%v", p, errno)
	}
	h, ok := fh.(*readHandle)
	if !ok {
		t.Fatalf("an O_APPEND open must hand back the read handle (BFS-030's premise), got %T", fh)
	}
	if h.ap == nil {
		t.Fatal("an O_APPEND open must carry the append state: without it the write is answered ENOTSUP and the caller's bytes are lost")
	}
	return h
}

// appendChunk is one FUSE WRITE at the offset the kernel chose.
func appendChunk(t *testing.T, m *Mount, h *readHandle, data []byte, off int64) syscall.Errno {
	t.Helper()
	n, errno := (&node{m: m, p: h.p}).Write(context.Background(), h, data, off)
	if errno == 0 && n != uint32(len(data)) {
		t.Fatalf("a write must acknowledge every byte it accepted: got %d of %d", n, len(data))
	}
	return errno
}

func namedWriteBuffers(t *testing.T, dir string) []string {
	t.Helper()
	got, err := filepath.Glob(filepath.Join(dir, "writebuf-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return got
}

// TestAppendThroughAnAppendHandleLandsTheBytes is THE cell: the filed failure
// becomes a landed append, and the read-back is byte-for-byte the original plus
// the tail.
func TestAppendThroughAnAppendHandleLandsTheBytes(t *testing.T) {
	m, target := testMount(t, boundTestLong)
	before := m.client.Requests()
	tail := []byte("\nAPPENDED-TAIL\n")

	h := openAppend(t, m, appendTarget)
	if errno := appendChunk(t, m, h, tail, int64(len(boundTestLong))); errno != 0 {
		t.Fatalf("the append chunk must be accepted, got errno=%v", errno)
	}
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("the publication must succeed, got errno=%v", errno)
	}

	want := append([]byte(boundTestLong), tail...)
	got := fileBytes(t, target)
	if !bytes.Equal(got, want) {
		t.Fatalf("THE APPENDED BYTES MUST LAND: the server holds %d B sha256=%s, want %d B sha256=%s (%q)",
			len(got), fsclient.HashBytes(got)[:16], len(want), fsclient.HashBytes(want)[:16], got)
	}
	// The publication is ONE conditional PUT, and the base costs ONE GET — never
	// one request per chunk.
	if delta := m.client.Requests() - before; delta != 2 {
		t.Fatalf("one append must cost ONE base GET and ONE publication PUT, got %d request(s)", delta)
	}
	// The kernel sends a publication point more than once (FLUSH, then RELEASE).
	// The second one must be answered locally: an append is not re-sent.
	if errno := h.ap.publish(context.Background()); errno != 0 {
		t.Fatalf("the second publication point: errno=%v", errno)
	}
	if delta := m.client.Requests() - before; delta != 2 {
		t.Fatalf("a second publication point RE-SENT the append (%d requests): the retry must be the surface's own no-op", delta)
	}
	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("Release must report the publication's own outcome, got errno=%v", errno)
	}
	st := m.Status()
	if st.Append.PublishedTotal != 1 || st.Append.RefusedTotal != 0 {
		t.Fatalf("the owner-facing figures must show ONE landed append and no refusal, got published=%d refused=%d",
			st.Append.PublishedTotal, st.Append.RefusedTotal)
	}
	if !strings.Contains(st.Append.Last, appendTarget) {
		t.Fatalf("the append figures must name the file, got %q", st.Append.Last)
	}
	if st.Append.MaxFileBytes != appendBound {
		t.Fatalf("the reported bound must be the enforced one: reported=%d enforced=%d", st.Append.MaxFileBytes, appendBound)
	}
}

// TestARetriedAppendChunkAtTheSameOffsetDoesNotDoubleApply is the idempotence cell.
//
// MEASURED, this is not a hypothetical retry: on the tree this row fixes, the
// kernel re-issued ALL THREE chunks of `exec 3>>f` at the SAME offset (off=82,
// off=82, off=82 — docs/evidence/BFS-021-trace.txt), because a failed write never
// grew i_size. A handle that applied chunks "at the end" instead of at the offset
// the kernel gave it would write a doubled, growing file the moment writes started
// landing. Applying each chunk at its own offset makes the retry an OVERWRITE of
// the same bytes in the same place — and because the publication is then the whole
// content the server already holds, the surface answers its own reported no-op and
// the mtime does not move (BFS-039's rule).
func TestARetriedAppendChunkAtTheSameOffsetDoesNotDoubleApply(t *testing.T) {
	m, target := testMount(t, boundTestLong)
	h := openAppend(t, m, appendTarget)
	tail := []byte("\nRETRIED-TAIL\n")
	off := int64(len(boundTestLong))

	if errno := appendChunk(t, m, h, tail, off); errno != 0 {
		t.Fatalf("the first attempt: errno=%v", errno)
	}
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("the first publication: errno=%v", errno)
	}
	landed := fileBytes(t, target)
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mtLanded := fi.ModTime()

	// THE CALLER'S RETRY: the same bytes at the same offset. A write cannot tell a
	// fault it did not hear about from a success, so this shape is the one an
	// accidental cancel produces.
	if errno := appendChunk(t, m, h, tail, off); errno != 0 {
		t.Fatalf("the retried chunk: errno=%v", errno)
	}
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("the retried publication: errno=%v", errno)
	}
	after := fileBytes(t, target)
	if !bytes.Equal(after, landed) {
		t.Fatalf("A RETRIED APPEND DOUBLE-APPLIED: %d B sha256=%s, want the landed content %d B sha256=%s (%q)",
			len(after), fsclient.HashBytes(after)[:16], len(landed), fsclient.HashBytes(landed)[:16], after)
	}
	if n := strings.Count(string(after), string(tail)); n != 1 {
		t.Fatalf("the tail must appear exactly once after a retry, saw %d", n)
	}
	res := h.ap.Result()
	if res == nil || !res.Noop || res.Verdict != fsclient.VerdictIdenticalContent {
		t.Fatalf("the retry must be the surface's REPORTED no-op (identical_content, no disk write), got %+v", res)
	}
	fi2, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !fi2.ModTime().Equal(mtLanded) {
		t.Fatalf("a retried append moved the mtime (%s -> %s): a change that did not happen must not be published as one (BFS-039)",
			mtLanded, fi2.ModTime())
	}
}

// TestAStaleBaseRefusesTheAppendAndNothingLands is the precondition cell: the
// append is built on ONE read, and the publication pins exactly those bytes, so a
// concurrent edit refuses it — loudly, with nothing written — instead of appending
// to content the caller never saw. The refusal HOLDS (BFS-033), and the documented
// recovery (a caller read, then retry) works.
func TestAStaleBaseRefusesTheAppendAndNothingLands(t *testing.T) {
	m, target := testMount(t, boundTestLong)
	base := []byte(boundTestLong)
	tail := []byte("APPENDED\n")

	h := openAppend(t, m, appendTarget)
	// The first chunk is what reads the base (ONE GET) and pins it.
	if errno := appendChunk(t, m, h, tail, int64(len(base))); errno != 0 {
		t.Fatalf("the first chunk: errno=%v", errno)
	}
	// A CONCURRENT EDIT, between the read and the publication.
	edited := bytes.ToUpper(base)
	if err := os.WriteFile(target, edited, 0o644); err != nil {
		t.Fatalf("the concurrent edit: %v", err)
	}
	if errno := h.Flush(context.Background()); errno != fsclient.ErrnoESTALE {
		t.Fatalf("a stale base must be refused loudly with ESTALE, got errno=%v", errno)
	}
	if got := fileBytes(t, target); !bytes.Equal(got, edited) {
		t.Fatalf("A REFUSED APPEND MUST NOT LAND: the target holds %q, want the concurrent edit %q", got, edited)
	}
	st := m.Status()
	if st.Append.RefusedTotal != 1 || st.Append.PublishedTotal != 0 {
		t.Fatalf("the refusal must be counted and nothing published, got refused=%d published=%d",
			st.Append.RefusedTotal, st.Append.PublishedTotal)
	}

	// The refusal HOLDS: a second writer on the same path publishes nothing behind
	// it, and is told the same verdict.
	h2 := openAppend(t, m, appendTarget)
	if errno := appendChunk(t, m, h2, tail, int64(len(edited))); errno != 0 {
		t.Fatalf("the second writer's chunk: errno=%v", errno)
	}
	if errno := h2.Flush(context.Background()); errno != fsclient.ErrnoESTALE {
		t.Fatalf("a standing refusal must refuse the next append on the path too, got errno=%v", errno)
	}
	if got := fileBytes(t, target); !bytes.Equal(got, edited) {
		t.Fatalf("the second writer's bytes reached the target behind the standing refusal: %q", got)
	}

	// THE DOCUMENTED RECOVERY: a CALLER read clears the hold, and the retry lands.
	rh := &readHandle{m: m, p: appendTarget, fh: 90001}
	if _, errno := rh.Read(context.Background(), make([]byte, len(edited)+64), 0); errno != 0 {
		t.Fatalf("the caller's re-read: errno=%v", errno)
	}
	h3 := openAppend(t, m, appendTarget)
	if errno := appendChunk(t, m, h3, tail, int64(len(edited))); errno != 0 {
		t.Fatalf("the retry's chunk: errno=%v", errno)
	}
	if errno := h3.Flush(context.Background()); errno != 0 {
		t.Fatalf("after a caller read the retry must land, got errno=%v", errno)
	}
	want := append(append([]byte(nil), edited...), tail...)
	if got := fileBytes(t, target); !bytes.Equal(got, want) {
		t.Fatalf("the recovered retry landed the wrong bytes: %q, want %q", got, want)
	}
}

// TestAnAppendAboveTheBoundIsRefusedLoudlyAndWritesNothing is the boundary cell.
//
// An append is published as ONE whole-file PUT, so the buffer holds the file and
// `--write-buffer-max-bytes` bounds it. Both sides of the boundary are measured,
// and the refusal must name what the caller should do instead — "Operation not
// supported" with no reason is the complaint this row is about.
func TestAnAppendAboveTheBoundIsRefusedLoudlyAndWritesNothing(t *testing.T) {
	defer func(v int64) { appendBound = v }(appendBound)

	t.Run("the write crosses the bound", func(t *testing.T) {
		appendBound = 20
		base := []byte("0123456789")
		m, target := testMount(t, string(base))
		beforeReqs := m.client.Requests()
		h := openAppend(t, m, appendTarget)
		// 10 bytes of base fit (one GET), 15 more do not.
		if errno := appendChunk(t, m, h, []byte("0123456789abcde"), 10); errno != fsclient.ErrnoEFBIG {
			t.Fatalf("a write past the bound must be refused with EFBIG, got errno=%v", errno)
		}
		if got := fileBytes(t, target); !bytes.Equal(got, base) {
			t.Fatalf("a refused append changed the target: %q", got)
		}
		if delta := m.client.Requests() - beforeReqs; delta != 1 {
			t.Fatalf("a refusal at the bound must publish NOTHING (one base GET only), the client made %d request(s)", delta)
		}
		if d := detailOf(t, h); !strings.Contains(d, "write it whole") {
			t.Fatalf("the refusal must tell the caller what to do instead, got %q", d)
		}
		if st := m.Status(); st.Append.RefusedTotal != 1 || st.Append.PublishedTotal != 0 {
			t.Fatalf("the boundary refusal must be counted, got refused=%d published=%d", st.Append.RefusedTotal, st.Append.PublishedTotal)
		}
	})

	t.Run("the file is already above the bound", func(t *testing.T) {
		appendBound = 40
		m, target := testMount(t, boundTestLong) // 82 bytes
		before := fileBytes(t, target)
		beforeReqs := m.client.Requests()
		h := openAppend(t, m, appendTarget)
		// The base read itself learns the size, so the refusal costs ONE GET and
		// never buffers a file the mount cannot publish.
		if errno := appendChunk(t, m, h, []byte("X"), int64(len(before))); errno != fsclient.ErrnoEFBIG {
			t.Fatalf("a file above the bound must be refused with EFBIG, got errno=%v", errno)
		}
		if got := fileBytes(t, target); !bytes.Equal(got, before) {
			t.Fatalf("a refused append changed the target: %q", got)
		}
		if delta := m.client.Requests() - beforeReqs; delta != 1 {
			t.Fatalf("the refusal must publish NOTHING, the client made %d request(s)", delta)
		}
		if st := m.Status(); st.Append.RefusedTotal != 1 || st.Append.MaxFileBytes != appendBound {
			t.Fatalf("the boundary and the reported bound must agree: refused=%d reported_bound=%d enforced=%d",
				st.Append.RefusedTotal, st.Append.MaxFileBytes, appendBound)
		}
	})
}

// detailOf returns the last refusal's detail, so a cell can assert on what the
// CALLER is told rather than only on the errno.
func detailOf(t *testing.T, h *readHandle) string {
	t.Helper()
	if h.ap == nil || h.ap.failure == nil {
		t.Fatal("the handle must carry the refusal it reported")
	}
	return h.ap.failure.Detail
}

// TestAWriteThroughANonAppendHandleIsStillRefused is the ATTRIBUTION cell for this
// row's whole change: serving append must not start serving every write. `r+b` on
// an existing path is refused with EOPNOTSUPP — byte-for-byte the errno go-fuse's
// bridge answered before node.Write existed — and BFS-030's resize refusal still
// holds while a write-intent handle (an O_APPEND open is one) is live.
func TestAWriteThroughANonAppendHandleIsStillRefused(t *testing.T) {
	m, target := testMount(t, boundTestLong)
	before := fileBytes(t, target)
	beforeReqs := m.client.Requests()

	fh, _, errno := (&node{m: m, p: appendTarget}).Open(context.Background(), syscall.O_RDWR)
	if errno != 0 {
		t.Fatalf("Open(O_RDWR): errno=%v", errno)
	}
	n, errno := (&node{m: m, p: appendTarget}).Write(context.Background(), fh, []byte("XX"), 0)
	if errno != syscall.EOPNOTSUPP {
		t.Fatalf("a write through a non-append handle must still be refused EOPNOTSUPP (the bridge's ENOTSUP), got errno=%v", errno)
	}
	if n != 0 {
		t.Fatalf("a refused write must acknowledge no bytes, got %d", n)
	}
	if delta := m.client.Requests() - beforeReqs; delta != 0 {
		t.Fatalf("the refusal must be local (the bridge answered it without a request), the client made %d request(s)", delta)
	}
	if got := fileBytes(t, target); !bytes.Equal(got, before) {
		t.Fatalf("a refused in-place write changed the target: %q", got)
	}
	// BFS-030's rule, unchanged: the destructive half of a rewrite is refused
	// while a write-intent handle is live on the path.
	if errno := setattrSize(t, m, appendTarget, fh, 0); errno != fsclient.ErrnoEOPNOTSUPP {
		t.Fatalf("BFS-030's resize refusal must be unchanged, got errno=%v", errno)
	}
	// ...including while an APPEND handle is live, which is the new write-intent
	// shape this row adds.
	ah := openAppend(t, m, appendTarget)
	defer func() { _ = ah.Release(context.Background()) }()
	if errno := setattrSize(t, m, appendTarget, nil, 0); errno != fsclient.ErrnoEOPNOTSUPP {
		t.Fatalf("a resize must still be refused while an append handle is live, got errno=%v", errno)
	}
	if got := fileBytes(t, target); !bytes.Equal(got, before) {
		t.Fatalf("a refused resize changed the target: %q", got)
	}
}

// TestALaterPublicationOnTheSameHandleCarriesTheEarlierOnesBytes is the cell for
// the defect this row's own first implementation had, and it is here because the
// arm found it rather than because it was anticipated.
//
// MEASURED on the fixed tree WITHOUT this rule (docs/evidence/BFS-021-red.txt, arm
// `multi` on the fixed tree): the kernel asks for a publication point per close —
// a `printf >&3` duplicates fd 3 onto fd 1 and closes it, and one open produced
// THREE closes — so the handle publishes, then publishes again. Pinning the second
// publication to the base the handle started from made it a stale-precondition
// refusal: the wire shows `PUT 204 (107 bytes)`, then `PUT 412 (120 bytes)`, the
// caller's later chunks were dropped, and the shell still exited 0. The base
// therefore advances with every publication, and this cell pins it.
func TestALaterPublicationOnTheSameHandleCarriesTheEarlierOnesBytes(t *testing.T) {
	m, target := testMount(t, boundTestShort)
	h := openAppend(t, m, appendTarget)
	first := []byte("-one")
	second := []byte("-two")

	if errno := appendChunk(t, m, h, first, int64(len(boundTestShort))); errno != 0 {
		t.Fatalf("the first chunk: errno=%v", errno)
	}
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("the first publication: errno=%v", errno)
	}
	if got := fileBytes(t, target); !bytes.Equal(got, append([]byte(boundTestShort), first...)) {
		t.Fatalf("the first publication landed %q", got)
	}

	// The SAME handle, a second chunk, a second publication point.
	if errno := appendChunk(t, m, h, second, int64(len(boundTestShort)+len(first))); errno != 0 {
		t.Fatalf("the second chunk: errno=%v", errno)
	}
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("the second publication must land on the base the first one published, got errno=%v", errno)
	}
	want := append(append([]byte(boundTestShort), first...), second...)
	got := fileBytes(t, target)
	if !bytes.Equal(got, want) {
		t.Fatalf("A LATER PUBLICATION LOST BYTES: the server holds %q (%d B), want %q (%d B)", got, len(got), want, len(want))
	}
	if st := m.Status(); st.Append.PublishedTotal != 2 || st.Append.RefusedTotal != 0 {
		t.Fatalf("both publications must be counted as landed, got published=%d refused=%d",
			st.Append.PublishedTotal, st.Append.RefusedTotal)
	}
}

// TestAReaderDuringAnAppendSeesAWholeContentNeverATear is the concurrency cell.
//
// The guarantee: a reader sees the OLD complete content or the NEW complete
// content, never a torn mixture. It holds because the publication is ONE
// conditional PUT that the surface commits by staging a sibling and renaming it
// (BFS-038) — the reader's open resolves to one inode or the other. The cell is
// non-vacuous by construction: it asserts the reader actually observed MORE THAN
// ONE complete state, so a reader that never raced the publishes cannot pass it.
func TestAReaderDuringAnAppendSeesAWholeContentNeverATear(t *testing.T) {
	const appends = 40
	base := []byte(boundTestLong)
	m, target := testMount(t, string(base))

	// Every complete state is known up front, so a torn read is a hash that is in
	// no state at all rather than something the assertion has to guess.
	tails := make([][]byte, appends)
	states := [][]byte{append([]byte(nil), base...)}
	for i := range tails {
		tails[i] = []byte(fmt.Sprintf("T%04d\n", i))
		states = append(states, append(append([]byte(nil), states[i]...), tails[i]...))
	}
	allowed := map[string]bool{}
	for _, s := range states {
		allowed[fsclient.HashBytes(s)] = true
	}

	stop := make(chan struct{})
	var mu sync.Mutex
	observed := map[string]int{}
	var torn []string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := os.ReadFile(target)
			if err != nil {
				continue
			}
			h := fsclient.HashBytes(got)
			mu.Lock()
			if !allowed[h] {
				torn = append(torn, fmt.Sprintf("%d B sha256=%s", len(got), h[:16]))
			} else {
				observed[h]++
			}
			mu.Unlock()
		}
	}()

	for i := 0; i < appends; i++ {
		h := openAppend(t, m, appendTarget)
		off := int64(len(states[i]))
		if errno := appendChunk(t, m, h, tails[i], off); errno != 0 {
			t.Fatalf("append %d: errno=%v", i, errno)
		}
		if errno := h.Flush(context.Background()); errno != 0 {
			t.Fatalf("append %d publication: errno=%v", i, errno)
		}
		if errno := h.Release(context.Background()); errno != 0 {
			t.Fatalf("append %d release: errno=%v", i, errno)
		}
		if got := fileBytes(t, target); !bytes.Equal(got, states[i+1]) {
			t.Fatalf("append %d landed the wrong bytes: %d B, want %d B", i, len(got), len(states[i+1]))
		}
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(torn) != 0 {
		t.Fatalf("A TORN READ: %d read(s) returned a mixture that is no complete state: %v", len(torn), torn[:3])
	}
	if len(observed) < 2 {
		t.Fatalf("VACUOUS ARM: the reader observed %d complete state(s) — it never raced the publications, so 'no tear' proves nothing", len(observed))
	}
	if got := m.Status().Append.PublishedTotal; got != appends {
		t.Fatalf("every append must have been published: %d of %d", got, appends)
	}
}

// TestACancelledAppendIsRetryableAppliesOnceAndLeavesNoResidue is the cancellation
// cell (BFS-039's requirement, arriving through the new path).
//
// A deliberate cancel is not a verdict: the handle stays publishable, the retry
// lands ONCE, and the buffer that makes the retry possible is anonymous — the
// directory shows no named buffer even while the handle holds one, which is what
// makes a killed appender leave nothing behind.
func TestACancelledAppendIsRetryableAppliesOnceAndLeavesNoResidue(t *testing.T) {
	m, target := testMount(t, boundTestLong)
	h := openAppend(t, m, appendTarget)
	tail := []byte("\nCANCELLED-THEN-RETRIED\n")
	off := int64(len(boundTestLong))

	if errno := appendChunk(t, m, h, tail, off); errno != 0 {
		t.Fatalf("the chunk: errno=%v", errno)
	}
	// The buffer exists and has NO NAME.
	if h.ap.tmp == nil {
		t.Fatal("the append must hold its buffer at this point, or this cell proves nothing about the buffer")
	}
	if names := namedWriteBuffers(t, m.dir); len(names) != 0 {
		t.Fatalf("the append buffer must be anonymous (unlinked the moment it exists); the mount directory shows %v", names)
	}

	// THE DELIBERATE CANCEL: the caller's context is cancelled, which is what
	// go-fuse does when the caller goes away.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if errno := h.ap.publish(ctx); errno != fsclient.ErrnoEINTR {
		t.Fatalf("a deliberate cancel must be reported as EINTR (retryable), got errno=%v", errno)
	}
	if got := fileBytes(t, target); !bytes.Equal(got, []byte(boundTestLong)) {
		t.Fatalf("a cancelled append must not land: %q", got)
	}

	// THE RETRY, through the same handle (it was left publishable).
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("the retry after a cancel must land, got errno=%v", errno)
	}
	want := append([]byte(boundTestLong), tail...)
	got := fileBytes(t, target)
	if !bytes.Equal(got, want) {
		t.Fatalf("the retry landed the wrong bytes: %q, want %q", got, want)
	}
	if n := strings.Count(string(got), string(tail)); n != 1 {
		t.Fatalf("THE RETRY DOUBLE-APPLIED: the tail appears %d times", n)
	}
	if names := namedWriteBuffers(t, m.dir); len(names) != 0 {
		t.Fatalf("no named buffer may survive the publication, found %v", names)
	}
	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("release: errno=%v", errno)
	}
}

// TestAppendIsServedWithoutPublishingASizeCarryingSetattr pins the premise BFS-030
// rests on: an append is a WRITE and never a resize, so the append path cannot
// publish the destructive half of a rewrite. The trace is the caller's view — the
// write is accepted, and the file's size only ever grows by the bytes written.
func TestAppendIsServedWithoutPublishingASizeCarryingSetattr(t *testing.T) {
	m, target := testMount(t, boundTestShort) // 5 bytes
	h := openAppend(t, m, appendTarget)
	tail := []byte("XYZ")
	before := m.client.Requests()
	if errno := appendChunk(t, m, h, tail, int64(len(boundTestShort))); errno != 0 {
		t.Fatalf("the chunk: errno=%v", errno)
	}
	if errno := h.Flush(context.Background()); errno != 0 {
		t.Fatalf("the publication: errno=%v", errno)
	}
	// ONE base GET and ONE whole-content PUT: the PUT body is the WHOLE result and
	// its declared size is the result's size, never a bare resize.
	if delta := m.client.Requests() - before; delta != 2 {
		t.Fatalf("want one GET and one PUT, got %d request(s)", delta)
	}
	want := append([]byte(boundTestShort), tail...)
	if got := fileBytes(t, target); !bytes.Equal(got, want) {
		t.Fatalf("the server must hold the whole result: %q, want %q", got, want)
	}
	// Nothing the append did may have been published as a resize: the mount's
	// write-shape refusal figure is untouched, and the bytes were served.
	if st := m.Status(); st.WriteShape.RefusalsTotal != 0 {
		t.Fatalf("the append path must not trip BFS-030's write-shape rule, got %d refusal(s)", st.WriteShape.RefusalsTotal)
	}
}
