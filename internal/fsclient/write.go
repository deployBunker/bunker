package fsclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The two values of --on-conflict (BFS-005 §5.2). `refuse` is the default and
// the release's rule: a silent overwrite is a lost update, a refusal is a
// recoverable event.
const (
	OnConflictRefuse = "refuse"
	// OnConflictOverwriteIfUnchanged: a mismatch whose current hash equals the
	// hash we LAST SERVED for that path was metadata-only, and the write is
	// retried with the corrected base. If the current hash differs from what we
	// served it is a real concurrent edit and the write is STILL refused.
	OnConflictOverwriteIfUnchanged = "overwrite-if-unchanged"
)

// WriteBase is where a write's precondition comes from (BFS-005 §5.3). A FUSE
// syscall cannot carry "expected hash" (BFS-003 §3(c)), so the base is always the
// client's own record — resolved here.
type WriteBase struct {
	// IfMatch is the base hash, quoted into a strong entity-tag on the wire.
	IfMatch string
	// IfNoneMatchStar asks the server to create only if the path is absent.
	IfNoneMatchStar bool
	// Source names how the base was learned, for the record:
	// "served" (the bytes we handed the caller), "fetch-then-check" (a HEAD),
	// "absent" (the path does not exist), "unknown-invalidated" (an
	// invalidation dropped the record without a hash).
	Source string
}

// The base-hash sources, reported rather than implied.
const (
	BaseFromServed      = "served"
	BaseFromFetchCheck  = "fetch-then-check"
	BaseFromAbsent      = "absent"
	BaseFromInvalidated = "unknown-invalidated"
)

// --- BFS-033: the refusal that HOLDS -----------------------------------------
//
// A `412` was RECORDED and the write LANDED anyway (measured; see
// docs/evidence/BFS-033-*.md). Two facts make that happen, and neither is a
// client choice:
//
//   - the kernel re-issues a size-carrying SETATTR once after the mount answers
//     ESTALE: ONE `truncate(2)` syscall produces TWO Setattr dispatches (measured
//     with a trace line: `BFS033-TRACE setattr path=src/target.txt size=48` twice,
//     and the syscall count in strace is 1);
//   - §5.2 rule 3 adopts the refusal's `X-Bunker-Current-Hash` as the path's base
//     — exactly what the re-read-and-retry loop needs — so the RE-ISSUED dispatch
//     publishes against the concurrent edit's hash and lands, below the caller.
//
// So the refusal was terminal for the write it refused and for nothing after it.
// §5.2 rule 1 says the refusal is terminal for that write; §5.2 rule 5 names the
// caller's recovery (re-read, then retry). The missing piece is the enforcement
// BETWEEN them: while a refusal on a path is outstanding and unrecovered, no
// publication of the same shape may land behind it. That is what the hold below
// is: a refused path is refused again — with the SAME verdict — until the CALLER
// has been served bytes for it (the re-read rule 5 asks for).
//
// Deliberately narrow, in three places:
//   - it refuses MODIFICATIONS (a publish carrying a precondition against a base
//     the refusal superseded). A CREATE (`If-None-Match: *`, a path that must be
//     absent by the server's own rule) is not the refused write and passes: a
//     caller that unlinked the path and writes it anew is not retrying the
//     refused write, and refusing that would be an unrecoverable refusal.
//   - it is armed by a REAL server refusal only, so it never invents one: the
//     counter and the record both come from the verdict the server already gave.
//   - it is BOUNDED (refusalHoldMax paths) and the bound is REPORTED
//     (refusal_holds.evicted_total) — a bound the owner cannot see is not a
//     bound (PRD-bunker-invalidation.md §2.7).
const (
	// refusalHoldMax bounds how many refused paths can stand at once. The
	// re-issued dispatch this exists for arrives microseconds after the refusal
	// that armed the hold, so the bound is not reachable by the defect path; it
	// exists so the structure cannot grow with the number of conflicts a mount
	// ever sees.
	refusalHoldMax = 1024
)

// refusalHold is ONE path whose conflict refusal stands: the verdict that armed
// it, and how many publications it has refused since.
type refusalHold struct {
	rec  Conflict
	held int64
}

// WritePath owns the write precondition: base-hash resolution, the single
// conditional PUT, the refusal record, and the write's own durability policy.
// It is the client's own classifier, distinct from the sshfs driver's — the
// third mount-driver rule this row is held to.
type WritePath struct {
	client     *Client
	cache      *Cache
	dir        string
	onConflict string

	mu       sync.Mutex
	refusals int64
	last     *Conflict
	// served is the hash this client last SERVED for a path — the record
	// `overwrite-if-unchanged` compares against, and the reason that option can
	// tell a metadata-only mismatch from a real concurrent edit.
	served map[string]string
	// bases is the last base hash adopted per path, so a retry after a refusal
	// starts from truth rather than from the caller's stale belief.
	bases map[string]WriteBase
	// holds is the BFS-033 enforcement: one entry per path whose conflict
	// refusal stands, cleared by a caller-facing read of that path (or by the
	// path being removed). heldTotal/heldLast are the reported figures — a rule
	// that fires silently is not a rule anyone can audit.
	holds       map[string]*refusalHold
	heldTotal   int64
	heldEvicted int64
	heldLast    string
}

// NewWritePath builds a write path. onConflict must be one of the two declared
// values; anything else falls back to refuse (the safe direction) rather than
// inventing a third policy.
func NewWritePath(c *Client, cache *Cache, dir, onConflict string) *WritePath {
	if onConflict != OnConflictOverwriteIfUnchanged {
		onConflict = OnConflictRefuse
	}
	return &WritePath{
		client:     c,
		cache:      cache,
		dir:        dir,
		onConflict: onConflict,
		served:     map[string]string{},
		bases:      map[string]WriteBase{},
		holds:      map[string]*refusalHold{},
	}
}

// NoteServed records the hash of the bytes handed to a reader. The read that
// serves the bytes is the read that decides the next conflict.
func (w *WritePath) NoteServed(path, hash string) {
	if hash == "" {
		return
	}
	w.mu.Lock()
	w.served[path] = hash
	w.mu.Unlock()
}

// Served returns the hash last served for a path.
func (w *WritePath) Served(path string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	h, ok := w.served[path]
	return h, ok
}

// NoteInvalidated drops a path's base hash. The base becomes `unknown`, which
// forces fetch-then-check on the next write rather than a base that no longer
// describes the file — exactly how a lost update gets silently allowed
// (BFS-005 §4.2 step 4).
func (w *WritePath) NoteInvalidated(paths ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range paths {
		delete(w.bases, p)
		delete(w.served, p)
	}
}

// SetBase pins a base for a path (used when the mount has already resolved it).
func (w *WritePath) SetBase(path string, b WriteBase) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.bases[path] = b
}

// NoteRead records that the CALLER was served bytes for a path — the re-read
// §5.2 rule 5 names as the recovery from a refusal. It is the ONE thing that
// clears an outstanding refusal hold: the hold exists so a refused write cannot
// land behind the refusal, and a caller that has re-read the path is no longer
// that refused write (it publishes against what it was just served).
//
// It is called from the mount's read path only, at the point the served bytes
// are recorded — NOT from the client's own internal reads (a resizing truncate
// reads the path itself before publishing, and clearing the hold there would be
// the defect back again) and NOT from an invalidation event (a change notice is
// not a caller read).
func (w *WritePath) NoteRead(path, hash string) {
	w.mu.Lock()
	delete(w.holds, path)
	w.mu.Unlock()
	w.NoteServed(path, hash)
}

// NoteDeleted drops a path's hold: the refusal's subject no longer exists, so
// there is no refused write left to enforce. Called from the mount's own removal
// path, where the name is gone rather than superseded.
func (w *WritePath) NoteDeleted(path string) {
	w.mu.Lock()
	delete(w.holds, path)
	w.mu.Unlock()
}

// armHold records that `rec` refused a write on rec.Path: a later publication on
// that path is refused too, with the same verdict, until the caller re-reads it.
// The oldest hold is evicted when the bound is reached, and the eviction is
// counted (refusal_holds.evicted_total) rather than hidden.
func (w *WritePath) armHold(rec Conflict) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.holds[rec.Path]; !ok && len(w.holds) >= refusalHoldMax {
		oldest, oldestTS := "", rec.TS
		for p, h := range w.holds {
			if oldest == "" || h.rec.TS.Before(oldestTS) {
				oldest, oldestTS = p, h.rec.TS
			}
		}
		delete(w.holds, oldest)
		w.heldEvicted++
	}
	w.holds[rec.Path] = &refusalHold{rec: rec}
}

// checkHold refuses a publication whose path carries an outstanding refusal, and
// it is the whole of BFS-033's fix on the write side. It returns nil when the
// write may proceed, and the refusal (same errno, same cause, the refusing
// verdict's hashes) when it may not.
//
// A CREATE is exempt ON PURPOSE: `If-None-Match: *` asks the server to create a
// path that must be absent, which is not the write the refusal refused, and
// holding it would make the refusal unrecoverable for a caller that removed the
// path and wrote it anew.
func (w *WritePath) checkHold(path string, base WriteBase) *OpError {
	if base.IfNoneMatchStar {
		return nil
	}
	w.mu.Lock()
	h, ok := w.holds[path]
	if !ok {
		w.mu.Unlock()
		return nil
	}
	h.held++
	held, rec := h.held, h.rec
	w.heldTotal++
	w.heldLast = fmt.Sprintf("%s: held=%d code=%s (the refusal stands until the path is read)", path, held, rec.Code)
	w.mu.Unlock()
	return &OpError{
		Op: "PUT", Path: path, Errno: ErrnoESTALE, Cause: CauseConflict,
		Verdict:      rec.Code,
		ExpectedHash: rec.Expected,
		CurrentHash:  rec.Current,
		Detail: fmt.Sprintf("refusing to publish %s: the server already refused a write on this path (code=%s, current=%s) and that refusal stands until the path is read — publishing now would land the bytes the refusal refused. Re-read the path, then retry",
			path, rec.Code, rec.Current),
	}
}

// Held reports the outstanding-refusal figures: how many publications a standing
// refusal has turned away, how many paths a refusal still stands on, how many
// holds the bound evicted, and the most recent one by name. Every one of them is
// in the status document, because §5.2 rule 1's enforcement is a rule the owner
// has to be able to audit.
func (w *WritePath) Held() (total int64, outstanding int, evicted int64, last string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.heldTotal, len(w.holds), w.heldEvicted, w.heldLast
}

// ResolveBase implements §5.3's table:
//
//	path was read (cache hit or miss)      -> the hash of the bytes we served
//	path never read, target exists         -> fetch-then-check (one HEAD)
//	path never read, target absent         -> If-None-Match: *
//	base invalidated without a hash        -> same as "never read"
//
// Fetch-then-check is a decision with a behavioural reason: refusing any write
// to a path we never read would break cp, tar -x and sed -i, and adopting the
// server's hash at write time still makes a lost update impossible, because the
// base is read from the server in the same exchange that decides the write.
func (w *WritePath) ResolveBase(ctx context.Context, path string) (WriteBase, *OpError) {
	w.mu.Lock()
	if b, ok := w.bases[path]; ok {
		w.mu.Unlock()
		return b, nil
	}
	served, hasServed := w.served[path]
	w.mu.Unlock()

	if hasServed && served != "" {
		b := WriteBase{IfMatch: served, Source: BaseFromServed}
		w.SetBase(path, b)
		return b, nil
	}

	// Never read (or invalidated): ask the server, cheaply.
	meta, err := w.client.Head(ctx, path)
	if err != nil {
		if err.Errno == ErrnoENOENT {
			b := WriteBase{IfNoneMatchStar: true, Source: BaseFromAbsent}
			w.SetBase(path, b)
			return b, nil
		}
		return WriteBase{}, err
	}
	if meta.Hash == "" {
		// A resource the surface serves without a content identity cannot be
		// preconditioned; the honest base is "no base" and the caller learns it.
		b := WriteBase{Source: BaseFromInvalidated}
		w.SetBase(path, b)
		return b, nil
	}
	b := WriteBase{IfMatch: meta.Hash, Source: BaseFromFetchCheck}
	w.SetBase(path, b)
	return b, nil
}

// Publish performs ONE conditional PUT — the write protocol of BFS-004 §6,
// never one request per FUSE WRITE chunk. body is streamed into the request, so
// the client's write memory is one chunk rather than the file.
func (w *WritePath) Publish(ctx context.Context, path string, body io.Reader, size int64, base WriteBase) (*PutResult, *OpError) {
	if held := w.checkHold(path, base); held != nil {
		return nil, held
	}
	res, err := w.client.PutBody(ctx, path, body, size, PutPrecondition{
		IfMatch:         base.IfMatch,
		IfNoneMatchStar: base.IfNoneMatchStar,
	})
	if err == nil {
		// A landed write is truth: the new hash becomes both the base and what
		// we have served for this path.
		w.SetBase(path, WriteBase{IfMatch: res.Hash, Source: BaseFromServed})
		w.NoteServed(path, res.Hash)
		return res, nil
	}
	if err.Cause != CauseConflict {
		return nil, err
	}
	return w.handleRefusal(ctx, path, body, size, base, err)
}

// PublishBytes is Publish for an in-memory body (a mkdir-sized write, a test, a
// small editor save).
func (w *WritePath) PublishBytes(ctx context.Context, path string, data []byte, base WriteBase) (*PutResult, *OpError) {
	if held := w.checkHold(path, base); held != nil {
		return nil, held
	}
	res, err := w.client.PutBody(ctx, path, w.client.BodyReader(data), int64(len(data)), PutPrecondition{
		IfMatch:         base.IfMatch,
		IfNoneMatchStar: base.IfNoneMatchStar,
	})
	if err == nil {
		w.SetBase(path, WriteBase{IfMatch: res.Hash, Source: BaseFromServed})
		w.NoteServed(path, res.Hash)
		return res, nil
	}
	if err.Cause != CauseConflict {
		return nil, err
	}
	return w.handleRefusal(ctx, path, w.client.BodyReader(data), int64(len(data)), base, err)
}

// handleRefusal implements §5.2's five rules. The refusal is terminal for that
// write: no blind retry, no merge, no overwrite.
func (w *WritePath) handleRefusal(ctx context.Context, path string, body io.Reader, size int64, base WriteBase, err *OpError) (*PutResult, *OpError) {
	// The server's own machine code, recorded verbatim. `precondition_failed`
	// names no hash on purpose, and the client must not read that absence as
	// "no information".
	code := err.Verdict
	if code == "" {
		if err.CurrentHash != "" {
			code = VerdictHashMismatch
		} else {
			code = VerdictPreconditionFailed
		}
	}
	rec := Conflict{
		Path:     path,
		Expected: base.IfMatch,
		Code:     code,
		Detail:   err.Detail,
	}
	if code == VerdictHashMismatch {
		rec.Current = err.CurrentHash
	}

	// overwrite-if-unchanged is the narrow case the PRD names: metadata-only.
	if w.onConflict == OnConflictOverwriteIfUnchanged && code == VerdictHashMismatch && err.CurrentHash != "" {
		w.mu.Lock()
		lastServed := w.served[path]
		w.mu.Unlock()
		if lastServed != "" && lastServed == err.CurrentHash {
			// Our view of the server's content is exact; the mismatch moved the
			// base only. Retry once, with the corrected base, and say so.
			w.SetBase(path, WriteBase{IfMatch: err.CurrentHash, Source: BaseFromFetchCheck})
			if res, retryErr := w.client.PutBody(ctx, path, body, size, PutPrecondition{IfMatch: err.CurrentHash}); retryErr == nil {
				w.SetBase(path, WriteBase{IfMatch: res.Hash, Source: BaseFromServed})
				w.NoteServed(path, res.Hash)
				return res, nil
			}
		}
		// A real concurrent edit is still refused.
	}

	// Rule 3: the base is UPDATED from the refusal, never kept stale.
	if code == VerdictHashMismatch && err.CurrentHash != "" {
		w.SetBase(path, WriteBase{IfMatch: err.CurrentHash, Source: BaseFromFetchCheck})
	} else {
		w.SetBase(path, WriteBase{IfNoneMatchStar: true, Source: BaseFromAbsent})
	}
	if err.CurrentHash != "" && code == VerdictHashMismatch {
		// We now know the server's bytes exactly, so a re-read is served from
		// truth. The bytes are not in our cache (we never fetched them), so the
		// record only informs the next write's base.
	}

	// Rule 4: counted and listed.
	w.mu.Lock()
	w.refusals++
	w.last = &rec
	w.mu.Unlock()
	// BFS-033: the refusal STANDS on this path until the caller re-reads it, so
	// the write it refused cannot land behind it (the kernel re-issues the
	// resize below the caller; see the note above checkHold).
	w.armHold(rec)
	if w.dir != "" {
		_ = AppendConflict(w.dir, rec)
	}
	return nil, err
}

// Refusals reports how many writes this path refused.
func (w *WritePath) Refusals() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.refusals
}

// LastConflict returns the most recent refusal.
func (w *WritePath) LastConflict() *Conflict {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		return nil
	}
	cp := *w.last
	return &cp
}

// OnConflict returns the configured conflict policy.
func (w *WritePath) OnConflict() string { return w.onConflict }

// ErrTreeRecreated is returned when the served tree is no longer the bound tree.
var ErrTreeRecreated = errors.New("bunker-fs: the served tree identity changed; re-bind (--recover) rather than adopt it")
