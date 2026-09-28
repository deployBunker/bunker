//go:build linux

package fsmount

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sync"
	"syscall"

	"github.com/deployBunker/bunker/internal/fsclient"
)

// ---------------------------------------------------------------------------
// APPEND (BFS-021): an append through the mount must land its bytes.
// ---------------------------------------------------------------------------
//
// THE DEFECT. `>>` against a file on the mount failed and the appended bytes were
// lost. MEASURED on the tree this row started from
// (docs/evidence/BFS-021-trace.txt, one trace line per FUSE request):
//
//	BFS021-TRACE open  path=src/target.txt flags=0x8401 O_APPEND=true O_WRONLY=true
//	BFS021-TRACE write path=src/target.txt off=82 len=25 handle=*fsmount.readHandle
//
// So an append arrives as an `open(O_WRONLY|O_APPEND)` — the flags DO reach the
// mount — and then as a WRITE **at an offset equal to the current size**, with a
// handle (`*readHandle`) that is not an `fs.FileWriter`. go-fuse's bridge then
// answers `ENOTSUP` for the write (fs/bridge.go: `if fr, ok := f.file.(FileWriter);
// ok {…} return 0, fuse.ENOTSUP`), which is errno **95** — and that is the whole
// of the filed failure: the caller's bytes are dropped, nothing was ever sent.
//
// It is a WRITE, not a resize: `>>` issues no size-carrying SETATTR at all, which
// is why BFS-030's refusal (a resize arriving while a write handle is live) never
// covers it — and why this row needs its own path rather than a widened guard.
//
// THE DECISION. Append is SERVED, by **read-modify-publish**: the handle reads the
// content once, buffers the arriving chunks at their own offsets on top of it, and
// publishes the whole result as ONE conditional PUT through the existing
// `WritePath`. The alternatives were considered and rejected on measured grounds:
//
//   - a RANGED/OFFSET write against the existing resource: the WebDAV surface has
//     no partial-`PUT` shape at all (internal/server/webdav/handler.go stages the
//     whole body and commits it by rename), so serving append this way means a new
//     wire shape PLUS a server-side append, and a server-side append is a DELTA —
//     it cannot satisfy this row's idempotence requirement without a dedupe key.
//     A retried delta double-applies; a retried whole content does not, because
//     the retry IS the final content and the surface already answers that with its
//     reported no-op (BFS-039's `identical_content`, mtime unmoved).
//   - a declared refusal of O_APPEND: legitimate only if it cannot lose data and
//     is LOUD. It cannot lose data (nothing is published), but it is not enough
//     here — MEASURED, one of the shapes of this very defect already reports
//     SUCCESS while dropping the bytes (`exec 3>>f` + three `printf >&3`: shell
//     rc=0, server unchanged, docs/evidence/BFS-021-red.txt arm `multi`). A loud
//     refusal of the single shape the shell reports correctly still leaves the
//     silent one silent, and append is a POSIX guarantee a mount cannot decline
//     without breaking every tool that logs.
//
// WHAT IT COSTS, stated as a number rather than implied: an append transfers the
// file's whole content twice (one GET to build the base, one PUT to publish it),
// so it is O(size) and not O(tail). That is the price of serving append through a
// whole-file surface without a new wire shape, and it is why the handle is bounded
// (see appendBaseMax) and why the buffer is one file, not the tree.
//
// WHAT IT PRESERVES, because a new write path is exactly where these get broken:
//
//	BFS-038 (one atomic swap onto a new immutable blob): the publication is the
//	    existing ONE conditional PUT — the server stages a sibling and renames —
//	    so a reader sees the old complete content or the new complete content and
//	    never a torn mixture, and nothing is ever written into a published blob.
//	BFS-033 (a refusal HOLDS): the publication goes through `WritePath.Publish`,
//	    so a path with an outstanding refusal is refused again with the same
//	    verdict and a refused append cannot land behind it.
//	BFS-039 (cancellation and idempotence): the body is the WHOLE final content
//	    and the buffer is the same anonymous, unlinked temp file the write path
//	    uses, so a cancelled publication is retryable (EINTR) and lands once; and
//	    because the chunks are applied AT THEIR OFFSETS, a chunk retried at the
//	    offset it already occupies is a no-op rather than a second copy.
//	BFS-030 (`>`/O_TRUNC and the in-place-refusal): untouched. No resize is
//	    published by this path, `writeIntentOn` still refuses a resize while a
//	    write-intent handle (which an O_APPEND open is) is live, and a write
//	    through a handle that is NOT an append open is still answered
//	    `EOPNOTSUPP` — the same errno the bridge answered before this method
//	    existed.
//
// The bound. `--write-buffer-max-bytes` (writeBufferMaxDefault) bounds the buffer,
// which for an append holds the WHOLE file, so a file at or above the bound cannot
// be appended through this surface. That is refused loudly with a named cause and
// nothing written — never silently dropped — and the boundary is REPORTED
// (`append.max_base_bytes` in the status document) because a bound the owner
// cannot see is not a bound (PRD-bunker-invalidation.md §2.7).
const appendBaseMax = writeBufferMaxDefault

// appendBound is the bound the running mount ENFORCES and REPORTS. It is a
// variable rather than the constant so a test can prove the boundary at a value
// it can actually reach (a 256 MiB fixture is not a test), and — because the
// status document reads this same variable — a figure reported one way and
// enforced another, which this project keeps re-finding, cannot happen here.
var appendBound int64 = appendBaseMax

// appendHandle is the append state of ONE open file. It is owned by the
// readHandle of the same open (an append is an `open(O_WRONLY|O_APPEND)`, which
// this mount answers with the read handle — BFS-030 measured that, and changing it
// would refuse reads through an O_RDWR handle, a capability that works today).
//
// The buffer is the base content with the arriving chunks written AT THEIR OFFSETS,
// so the publication is a function of (base, chunks, offsets) alone and not of the
// order or the number of attempts: that is what makes a retry land once.
type appendHandle struct {
	mu    sync.Mutex
	m     *Mount
	owner *readHandle
	p     string
	fh    uint64

	// tmp is the buffer: an ANONYMOUS (unlinked) temp file holding base+chunks.
	// Unlinked the moment it exists, so a killed appender leaves nothing behind
	// and no sweep is needed (BFS-039).
	tmp *os.File
	// base is the precondition the publication carries: the hash of the exact
	// content the append was built on, read in the same exchange that read the
	// bytes (a lost update is therefore impossible: a mismatch is refused, and
	// the server's answer names the current hash).
	base    fsclient.WriteBase
	hasBase bool
	// size is the current end of the content held, and baseSize is where the
	// content started — reported, and used to name the bound in a refusal.
	size     int64
	baseSize int64

	flushed bool
	failure *fsclient.OpError
	result  *fsclient.PutResult
}

// anonymousWriteBuffer opens an UNLINKED temp file of its own in dir: the open
// descriptor is the only reference to its bytes, so the kernel reclaims the inode
// with the last descriptor and a killed writer leaves NOTHING behind. This is the
// write path's buffer shape (BFS-039 measured the named one leaving
// `writebuf-<n>` behind after every killed writer), and the append handle uses the
// same construction so there is one buffer rule, not two.
func anonymousWriteBuffer(dir string) (*os.File, syscall.Errno) {
	f, err := os.CreateTemp(dir, "writebuf-*")
	if err != nil {
		return nil, syscall.EIO
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, syscall.EIO
	}
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, syscall.EIO
	}
	return f, 0
}

// write applies one arriving chunk at its own offset. It is deliberately
// OFFSET-ADDRESSED and not "append this to the end": MEASURED
// (docs/evidence/BFS-021-trace.txt, arm `multi`), the kernel sends every one of a
// failing open's writes at the SAME offset (all three chunks of `exec 3>>f` at
// off=82), because i_size never grew — so a handle that concatenated chunks would
// write a three-times-too-long file the moment it started working. Applying each
// chunk at the offset the kernel gave it is also the idempotence: a chunk retried
// at the offset it already occupies rewrites the same bytes in the same place.
func (a *appendHandle) write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failure != nil {
		return 0, syscall.EIO
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	// A chunk arriving AFTER a publication point reopens the handle for
	// publication. The buffer holds the whole content, so the next publication is
	// the whole content again: an fsync followed by more writes must not leave the
	// later bytes buffered and never sent.
	if a.flushed {
		a.flushed, a.result = false, nil
	}
	if errno := a.ensureBaseLocked(ctx); errno != 0 {
		return 0, errno
	}
	// The bound is checked BEFORE the bytes are buffered, so a refused chunk
	// leaves no hole and no growth behind it.
	if end := off + int64(len(data)); end > appendBound {
		return 0, a.refuseLocked(&fsclient.OpError{
			Op: "WRITE", Path: a.p, Errno: fsclient.ErrnoEFBIG, Cause: fsclient.CauseLocalCapability,
			Detail: fmt.Sprintf("refusing to append to %s: a whole-file publication is bounded by --write-buffer-max-bytes, and this write would make the file %d bytes (bound %d). Nothing was written. Append to this file on the server instead, or write it whole", a.p, end, appendBound),
		})
	}
	n, err := a.tmp.WriteAt(data, off)
	if err != nil {
		return 0, a.refuseLocked(&fsclient.OpError{
			Op: "WRITE", Path: a.p, Errno: fsclient.ErrnoEIO, Cause: fsclient.CauseLocalCapability,
			Detail: fmt.Sprintf("the local append buffer rejected %d bytes at offset %d; nothing was published and the file is unchanged", len(data), off),
		})
	}
	if end := off + int64(n); end > a.size {
		a.size = end
	}
	a.m.bounds.Wrote(a.p, a.size)
	a.m.mu.Lock()
	a.m.bufDocs[a.fh] = a.size
	a.m.mu.Unlock()
	return uint32(n), 0
}

// ensureBaseLocked reads the content the append is built on, ONCE per handle: the
// bytes and the hash that pins them come from the SAME exchange, so the
// publication's precondition is the exact content it was composed against.
//
// It deliberately does NOT record the read as one SERVED to the caller
// (`NoteRead`): that call clears a standing refusal (BFS-033), and doing it from
// the mount's own internal read would make the refusal unenforceable — the
// resizing truncate already documents the same rule.
func (a *appendHandle) ensureBaseLocked(ctx context.Context) syscall.Errno {
	if a.hasBase {
		return 0
	}
	data, meta, err := a.m.client.Get(ctx, a.p, "")
	if err != nil {
		a.failure = err
		a.m.recordFailure(err)
		a.m.noteAppendRefused(a.p, err)
		return errnoFor(err)
	}
	hash := meta.Hash
	if hash == "" {
		hash = fsclient.HashBytes(data)
	}
	if int64(len(data)) > appendBound {
		return a.refuseLocked(&fsclient.OpError{
			Op: "WRITE", Path: a.p, Errno: fsclient.ErrnoEFBIG, Cause: fsclient.CauseLocalCapability,
			Detail: fmt.Sprintf("refusing to append to %s: the file is %d bytes and this surface publishes an append as ONE whole-file conditional PUT, bounded by --write-buffer-max-bytes (%d). Nothing was written and the file is unchanged. Append to this file on the server instead, or write it whole", a.p, len(data), appendBound),
		})
	}
	f, errno := anonymousWriteBuffer(a.m.dir)
	if errno != 0 {
		return a.refuseLocked(&fsclient.OpError{
			Op: "WRITE", Path: a.p, Errno: syscall.EIO, Cause: fsclient.CauseLocalCapability,
			Detail: "the local append buffer could not be created in the mount's own directory; nothing was written",
		})
	}
	if _, werr := f.WriteAt(data, 0); werr != nil {
		f.Close()
		return a.refuseLocked(&fsclient.OpError{
			Op: "WRITE", Path: a.p, Errno: syscall.EIO, Cause: fsclient.CauseLocalCapability,
			Detail: "the base content could not be buffered; nothing was written",
		})
	}
	a.tmp, a.hasBase = f, true
	a.size, a.baseSize = int64(len(data)), int64(len(data))
	a.base = fsclient.WriteBase{IfMatch: hash, Source: fsclient.BaseFromFetchCheck}
	return 0
}

// refuseLocked records a refusal and makes it terminal for the handle, exactly as
// the write path does: the caller is told, the owner-facing surface counts it, and
// the bytes are never offered again as if the answer might change.
func (a *appendHandle) refuseLocked(err *fsclient.OpError) syscall.Errno {
	a.failure = err
	a.m.recordFailure(err)
	a.m.noteAppendRefused(a.p, err)
	return errnoFor(err)
}

// publish performs the ONE conditional PUT of the whole appended content. It is
// idempotent and it is the same contract the write path's publication has
// (BFS-039): a landed publication is kept; a refusal or a failure is terminal; a
// CANCELLATION is neither — the handle stays publishable, so the retry the EINTR
// asks for is possible and lands exactly once, because a whole-content PUT of
// bytes the server already holds is the surface's own reported no-op.
func (a *appendHandle) publish(ctx context.Context) syscall.Errno {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.flushed {
		if a.failure != nil {
			return errnoFor(a.failure)
		}
		return 0
	}
	if a.tmp == nil {
		a.flushed = true
		return 0 // opened for append, nothing was written: nothing to publish
	}
	if a.failure != nil {
		a.flushed = true
		return errnoFor(a.failure)
	}
	if _, err := a.tmp.Seek(0, io.SeekStart); err != nil {
		a.flushed = true
		return syscall.EIO
	}
	res, err := a.m.wp.Publish(ctx, a.p, noCloseReader{a.tmp}, a.size, a.base)
	if err != nil {
		if err.Cause == fsclient.CauseCancelled {
			// Nothing was refused and no verdict was given: the handle stays
			// publishable and the retry is ONE more attempt at the same content.
			return errnoFor(err)
		}
		a.flushed = true
		a.failure = err
		a.m.recordFailure(err)
		a.m.noteAppendRefused(a.p, err)
		if err.Cause == fsclient.CauseConflict {
			a.m.conflictCount.Add(1)
		}
		return errnoFor(err)
	}
	a.flushed = true
	a.result = res
	// THE BASE ADVANCES WITH THE PUBLICATION. The kernel sends more than one
	// publication point for one open (measured: FLUSH per close, then RELEASE), and
	// the buffer holds the WHOLE content, so a later publication carries the bytes
	// the earlier one landed plus the new ones. Pinning it to the base this handle
	// started from would make that second publication a stale-precondition refusal
	// — the bytes would be dropped with the caller told the shape failed, which is
	// the defect this row exists for, one publication point further in. MEASURED
	// before this line existed: the three-writes-in-one-open shape landed its first
	// chunk, was refused 412 on the second, and lost the rest
	// (docs/evidence/BFS-021-red.txt, arm `multi` on the fixed tree).
	if res.Hash != "" {
		a.base = fsclient.WriteBase{IfMatch: res.Hash, Source: fsclient.BaseFromServed}
	}
	a.m.recordOK()
	a.m.noteAppendPublished(a.p, a.size)
	a.m.snapshot().Drop(a.p)
	a.m.snapshot().DropReaddir(path.Dir(a.p))
	// The published bytes are the truth for this path now: seed the store and the
	// served record so a read-after-append pays no round trip, and drop the read
	// half of this handle so a read through the same fd sees them.
	if res.Hash != "" && a.size <= cacheSeedWriteMax {
		if _, serr := a.tmp.Seek(0, io.SeekStart); serr == nil {
			if data, rerr := io.ReadAll(io.LimitReader(a.tmp, cacheSeedWriteMax+1)); rerr == nil {
				if _, ierr := a.m.cache.Insert(a.p, res.Hash, data); ierr == nil {
					a.m.wp.NoteServed(a.p, res.Hash)
				}
			}
		}
	}
	if a.owner != nil {
		a.owner.invalidateLoaded()
	}
	return 0
}

// setPath moves the append state's own path after a successful rename (BFS-020),
// so an append that had not been published when the name moved lands under the
// name the file now has rather than resurrecting the old one. It takes only the
// append handle's own mutex: the owner read handle hands the value over and then
// unlocks before calling this, so the two locks are never held at once (the
// append's own publish takes this mutex and then the OWNER's, for the read half's
// invalidation, which is why the order must not be inverted here).
func (a *appendHandle) setPath(p string) {
	a.mu.Lock()
	a.p = p
	a.mu.Unlock()
}

func (a *appendHandle) discard() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tmp != nil {
		name := a.tmp.Name()
		a.tmp.Close()
		os.Remove(name)
		a.tmp = nil
	}
	a.m.mu.Lock()
	delete(a.m.bufDocs, a.fh)
	a.m.mu.Unlock()
}

// Result reports the landed publication, when there was one.
func (a *appendHandle) Result() *fsclient.PutResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.result
}

// invalidateLoaded drops the read half's cached bytes: after an append lands they
// are the OLD content, and serving them to a read on the same descriptor would be
// a stale answer with no error.
func (h *readHandle) invalidateLoaded() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pinned && h.hash != "" {
		h.m.cache.Unpin(h.hash)
		h.pinned = false
	}
	h.data, h.hash, h.loaded = nil, "", false
}

// noteAppendPublished / noteAppendRefused are the owner-facing figures. Both
// directions are reported because a rule that fires silently is not a rule anyone
// can audit (the same reason BFS-030's write-shape refusals are counted).
func (m *Mount) noteAppendPublished(p string, size int64) {
	m.appendPublished.Add(1)
	m.appendLast.Store(fmt.Sprintf("%s: published=%d bytes", p, size))
}

func (m *Mount) noteAppendRefused(p string, err *fsclient.OpError) {
	m.appendRefused.Add(1)
	a := ""
	if err != nil {
		a = fmt.Sprintf(" errno=%v cause=%s", err.Errno, err.Cause)
	}
	m.appendLast.Store(fmt.Sprintf("%s: refused%s", p, a))
}
