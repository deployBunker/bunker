// The read bound: what the KERNEL thinks a file's size is, and the rule that a
// read may never quietly serve less than the resource has.
//
// WHY THIS FILE EXISTS (BFS-025)
//
// A FUSE read is not bounded by what the daemon returns: it is bounded by the
// kernel inode's `i_size`, which is the size this mount put in its last attrs
// reply (getattr / lookup / create) for that path. A buffered reader that walks
// the kernel's read path gets every byte the daemon returns even when i_size is
// short, but a reader that goes through the kernel's *splice* path (uutils `cat`,
// and any tool that copies file→pipe) is clamped to i_size: measured, with
// i_size = 5 and 82 bytes of content behind it, `cat` printed 5 bytes ('X-REP'),
// returned rc=0, and the daemon's own reply of 82 bytes was silently discarded.
//
// The truncation therefore cannot be detected in the read handler — the kernel
// asks for a page-sized window (measured: 4096) either way and throws away what
// the daemon sends beyond i_size. The only place this client can act is the
// place where i_size gets its value, and the invariant it must hold is:
//
//	A read may never serve content LONGER than the size the kernel was last
//	told for that path. If it would, refuse loudly (ESTALE) — a fragment with
//	rc=0 is the one outcome that is not allowed.
//
// So the mount keeps, per path, the size the kernel is holding: the last attrs
// reply (authoritative — the kernel sets i_size from it and from nothing else)
// raised by any write the kernel acknowledged since (a write grows i_size to
// max(i_size, written end)). That reconstruction is exact for this filesystem,
// because this mount is the only source of both events.
package fsmount

import "sync"

// kernelBound is one path's record of what the kernel has been told.
type kernelBound struct {
	// published is the size of the last attrs reply for this path.
	published int64
	// written is the largest end the kernel has seen written since that reply.
	written int64
	// known is false until the mount has told the kernel anything at all about
	// the path: with nothing published, there is no bound to violate.
	known bool
}

// boundRegistry holds one kernelBound per path the kernel has looked at.
//
// The record is per MOUNT, not per snapshot: the kernel's inode keeps its
// i_size across a snapshot resync (which is exactly why a resync may not reset
// this), and every entry is keyed by the in-tree path, the same key the kernel's
// inode is derived from (inoFor is a pure function of the path).
type boundRegistry struct {
	mu     sync.Mutex
	byPath map[string]kernelBound
}

func newBoundRegistry() *boundRegistry {
	return &boundRegistry{byPath: map[string]kernelBound{}}
}

// Published records an attrs reply for a path: from this moment the kernel holds
// exactly this size, so any growth it was told about earlier is superseded.
func (r *boundRegistry) Published(path string, size int64) {
	if r == nil || path == "" || size < 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byPath[path] = kernelBound{published: size, known: true}
}

// Wrote records a write the kernel acknowledged (the reply carried `end` bytes of
// the file at offset `off`): the kernel grows i_size to at least that end. It
// does not change `published` — a later attrs reply is what supersedes growth.
func (r *boundRegistry) Wrote(path string, end int64) {
	if r == nil || path == "" || end < 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.byPath[path]
	if end > b.written {
		b.written = end
	}
	b.known = true
	r.byPath[path] = b
}

// Bound returns the size the kernel is holding for path and whether this mount
// knows of any bound at all. Unknown is a real answer — a path the kernel never
// asked about has no i_size of ours to violate — and it is reported rather than
// guessed, because guessing a bound would truncate reads on the guess.
func (r *boundRegistry) Bound(path string) (int64, bool) {
	if r == nil {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.byPath[path]
	if !ok || !b.known {
		return 0, false
	}
	if b.written > b.published {
		return b.written, true
	}
	return b.published, true
}

// servedVerdict is what the read path must do with the bytes it holds.
type servedVerdict int

const (
	// servedAgrees: the content is exactly the size the kernel holds. The fast
	// path — no request, no repair, nothing recorded.
	servedAgrees servedVerdict = iota
	// servedBoundTooSmall: the content is LONGER than the kernel's bound, so a
	// reader clamped at that bound would be silently truncated. Refuse loudly.
	servedBoundTooSmall
	// servedBoundTooLarge: the content is shorter than the bound the mount
	// published. Every byte the resource has is still served (the reader hits a
	// true EOF at end-of-content), so the read proceeds — but the metadata the
	// mount is holding is provably wrong and is repaired.
	servedBoundTooLarge
)

// judgeServed decides, from the kernel's bound and the length of the content the
// read holds, whether the read is safe. It is deliberately pure: the decision is
// where the correctness lives, so it is tested without a mount, a kernel or a
// server.
func judgeServed(bound int64, known bool, n int64) servedVerdict {
	switch {
	case !known:
		return servedAgrees
	case n > bound:
		return servedBoundTooSmall
	case n < bound:
		return servedBoundTooLarge
	default:
		return servedAgrees
	}
}
