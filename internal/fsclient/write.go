package fsclient

import (
	"context"
	"errors"
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
