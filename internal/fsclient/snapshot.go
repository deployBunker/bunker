package fsclient

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Node is one path's metadata as the server describes it. It is deliberately
// metadata-only: no bytes live here, which is what keeps a whole-tree read off
// the byte path (BFS-005 §6.3).
//
// Kind is the server's DECLARED type for the entry (BFS-018): the vocabulary
// the surface publishes as `b:type` and as the snapshot op's `type` field, one
// name for one kind of directory entry on both carriers. It is carried rather
// than collapsed into IsDir because the two are not the same question: a
// SYMLINK is neither a file nor a directory, and a client that folds it into
// "not a directory" reports a regular file the surface never described — which
// is how a link became a 5-byte file whose content was its own target path.
type Node struct {
	Path  string // in-tree path, "/"-separated, no leading slash ("" is the root)
	IsDir bool
	Size  int64
	Mode  string // 4 octal digits
	Mtime time.Time
	Hash  string // "" when the server reported none (a collection, or a file above the snapshot hash budget)
	// Kind is one of KindFile, KindDir or KindSymlink — or KindUnknown, when
	// the surface named a type this client does not know. KindUnknown is a
	// FIRST-CLASS value and never a synonym for "file": see Node.KindHonoured.
	Kind string
	// LinkTarget is the target of a symlink. It is populated only for
	// KindSymlink; a link whose target the surface did not report leaves this
	// empty and is refused by the operations that need it, never guessed at.
	LinkTarget string
}

// The declared entry-type vocabulary (BFS-018). The values are the surface's
// own (`b:type`, the snapshot op's `type`), including the older spellings the
// snapshot op has been seen to accept for a collection.
const (
	KindFile    = "file"
	KindDir     = "dir"
	KindSymlink = "symlink"
	// KindUnknown is "the surface said something this client cannot honour".
	// It is deliberately not the empty string: "" means the server published no
	// type at all (an older surface), which is a different fact from a type
	// this build does not know how to present.
	KindUnknown = "unknown"
	// KindUnreported is what a carrier that published no type leaves behind.
	// It is resolved to a file or a directory from the standard properties,
	// because an absent type on the standard surface is exactly a standard
	// file/collection distinction — never a link, which no standard property
	// can express.
	KindUnreported = ""
)

// KindHonoured reports whether this client knows how to present the entry to a
// caller. A node whose kind is not honoured must FAIL loudly in every operation
// that would have to describe it: reporting an unknown kind as a regular file
// is the class of defect BFS-018 is about, one type over.
func (n Node) KindHonoured() bool {
	switch n.Kind {
	case KindFile, KindDir, KindSymlink, KindUnreported:
		return true
	}
	return false
}

// IsSymlink reports whether the surface declared this entry a symlink.
func (n Node) IsSymlink() bool { return n.Kind == KindSymlink }

// normaliseKind maps a wire type name onto this client's vocabulary. It is the
// ONE place the two carriers' spellings are reconciled, so a `dir` from the
// snapshot op and a `dir` from `b:type` cannot be read differently.
func normaliseKind(wire string) string {
	switch strings.ToLower(strings.TrimSpace(wire)) {
	case "":
		return KindUnreported
	case KindFile:
		return KindFile
	case KindDir, "collection", "directory":
		return KindDir
	case KindSymlink:
		return KindSymlink
	default:
		// A type this build does not know. It is named, not discarded: the
		// caller refuses rather than describing it as something it is not.
		return KindUnknown
	}
}

// Snapshot is the node tree BFS-005 §6.3 describes: the metadata of a whole
// subtree, taken in ONE call, from which `ls`, `stat` and a walking tool's
// `lstat`s are answered in-process with zero further round trips.
//
// It is the SNOP, not a delegation: the caller (the kernel, via READDIRPLUS and
// Lookup) still asks one node at a time, and we answer from memory. That is the
// distinction BFS-003 §3(d) fixes — the walk becomes one server call, the
// operation itself is not delegated.
type Snapshot struct {
	mu        sync.RWMutex
	nodes     map[string]Node
	children  map[string]map[string]struct{}
	read      map[string]bool // directories whose LISTING has been read
	root      string
	source    string
	calls     int
	truncated bool
	taken     time.Time
	// obsSeq/obsOK are the ledger cursor this observation was MINTED at by the
	// server (`result.head_seq`, BFS-063). They are set only for a whole-tree,
	// untruncated snapshot-op answer: that is the one observation a client may
	// declare as its resume point on the events poll, and for the PROPFIND
	// fallback the server minted nothing, so there is nothing to declare.
	obsSeq int64
	obsOK  bool
}

// Snapshot sources, reported so a caller can tell the one-call path from the
// standard-protocol fallback.
const (
	// SourceSnapshotOp is the X-Bunker-Op: snapshot call — ONE request for the
	// whole subtree.
	SourceSnapshotOp = "snapshot-op"
	// SourcePropfindWalk is the concurrent PROPFIND Depth: 1 walk — one request
	// per directory, dispatched with many in flight. It is what the client uses
	// against an agent build that predates the op, and it is the arm that shows
	// what concurrency buys (the request COUNT is unchanged; the WALL CLOCK is
	// not).
	SourcePropfindWalk = "propfind-walk"
	// SourcePropfindSingle is a single Depth: 0 lookup for a path the snapshot
	// does not hold.
	SourcePropfindSingle = "propfind-single"
)

// SnapshotArgs is the fixed argument vocabulary of the snapshot op — the same
// three fields the surface documents, flat, never a command string.
type SnapshotArgs struct {
	Path        string `json:"path,omitempty"`
	Depth       string `json:"depth,omitempty"`
	IncludeHash bool   `json:"include_hash,omitempty"`
}

type snapshotResult struct {
	Count int `json:"count"`
	// HeadSeq is the MINTED resume point (BFS-063): the ledger cursor the server
	// held when this observation began. A client that holds this answer presents
	// it as `since_seq` on the events poll, which is how the server can tell a
	// client whose interval is covered from one that has observed nothing —
	// the distinction a cursor of 0 alone cannot express. It is a pointer
	// because a build that predates the field does not send it, and a client
	// must not then claim a cursor it was never issued.
	HeadSeq *int64 `json:"head_seq"`
	Entries []struct {
		Path        string `json:"path"`
		Type        string `json:"type"`
		Size        int64  `json:"size"`
		MtimeUnixMS int64  `json:"mtime_unix_ms"`
		Mode        string `json:"mode"`
		Hash        string `json:"hash"`
		// LinkTarget is the E-7 addition (BFS-018): the target of a
		// `type: "symlink"` entry. A surface that predates it simply does not
		// send it, which is why the client must never infer a target from
		// anything else.
		LinkTarget string `json:"link_target"`
	} `json:"entries"`
}

// SnapshotTree populates the node tree in ONE server call. When the running
// build does not serve the op (501 capability_unavailable — BFS-004 R3, the
// structured refusal for a catalogue entry the build lacks), the client falls
// back to the standard surface's concurrent walk rather than failing: a client
// must never REQUIRE a capability the surface did not publish.
func (c *Client) SnapshotTree(ctx context.Context, root string, includeHash bool) (*Snapshot, *OpError) {
	root = normalisePath(root)
	before := c.Requests()
	var res snapshotResult
	env, err := c.Op(ctx, "snapshot", SnapshotArgs{Path: root, Depth: "infinity", IncludeHash: includeHash}, &res)
	if err != nil {
		if err.Verdict == VerdictCapabilityUnavailable || err.Status == 501 || err.Verdict == VerdictOpUnknown {
			snap, werr := c.WalkTree(ctx, root)
			if werr != nil {
				return nil, werr
			}
			snap.source = SourcePropfindWalk
			snap.calls = int(c.Requests() - before)
			return snap, nil
		}
		return nil, err
	}
	snap := NewSnapshot(root)
	for _, e := range res.Entries {
		kind := normaliseKind(e.Type)
		n := Node{
			Path:  strings.Trim(normalisePath(e.Path), "/"),
			IsDir: kind == KindDir,
			Size:  e.Size,
			Mode:  e.Mode,
			Hash:  e.Hash,
			Kind:  kind,
			// A link's target is the entry's entity (BFS-018). It is copied
			// verbatim when the surface reports one, and left empty when it
			// does not — an empty target is never fabricated from the size or
			// from the bytes, because the only thing that could produce it
			// would be the materialisation this field exists to replace.
			LinkTarget: e.LinkTarget,
		}
		// A link has no content hash, and the surface must not send one: a hash
		// here would be the target's, which is identity borrowed from another
		// resource. If a surface sends both, the hash is dropped rather than
		// kept as a cache key for a link.
		if kind == KindSymlink {
			n.Hash = ""
		}
		if e.MtimeUnixMS != 0 {
			n.Mtime = time.UnixMilli(e.MtimeUnixMS)
		}
		snap.put(n)
	}
	snap.source = SourceSnapshotOp
	snap.truncated = env.Truncated
	snap.calls = int(c.Requests() - before)
	// The minted resume point (BFS-063). A TRUNCATED answer is not a whole-tree
	// observation, so it mints nothing a client may declare: claiming coverage
	// from a view that is missing paths is the same false claim in a new place.
	if !env.Truncated && res.HeadSeq != nil {
		snap.obsSeq, snap.obsOK = *res.HeadSeq, true
	}
	if !env.Truncated {
		// A depth=infinity answer is the whole subtree, so every listing in it
		// is known. A TRUNCATED answer is not: the missing directories' listings
		// were never seen, and each would then answer a readdir with a lie.
		snap.MarkAllDirsRead()
	}
	return snap, nil
}

// WalkTree builds the same node tree over the STANDARD protocol: PROPFIND
// Depth: 1 per collection, with up to Concurrency requests in flight.
//
// This is the honest answer to "N round trips → 1": with the op unavailable the
// tree still costs N requests, but they are concurrent rather than serial, and
// the whole-tree wall clock is what moves — the same lever the study measured
// (0.79 s at 25× concurrency vs 38.47 s with MaxConnsPerHost=1). The caller can
// see both the request count and the in-flight high-water mark.
func (c *Client) WalkTree(ctx context.Context, root string) (*Snapshot, *OpError) {
	root = normalisePath(root)
	snap := NewSnapshot(root)
	type job struct{ dir string }
	queue := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr *OpError
	var pending sync.WaitGroup

	// The CALLER's context, kept before the walk's own cancel wraps it: the
	// check at the end of this function is reachable with no firstErr only by
	// the caller's cancellation, so it can be attributed correctly (BFS-039).
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	worker := func() {
		defer wg.Done()
		for j := range queue {
			metas, err := c.Propfind(ctx, j.dir, "1")
			mu.Lock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				cancel()
				continue
			}
			mu.Unlock()
			snap.MarkDirRead(j.dir)
			for _, m := range metas {
				p := strings.Trim(normalisePath(m.Path), "/")
				if p == j.dir {
					continue // the collection itself is already in the tree
				}
				snap.put(Node{Path: p, IsDir: m.IsDir, Size: m.Size, Mode: m.Mode, Mtime: m.Mtime, Hash: m.Hash,
					Kind: m.Kind, LinkTarget: m.LinkTarget})
				if m.IsDir {
					pending.Add(1)
					go func(d string) { defer pending.Done(); queue <- job{dir: d} }(p)
				}
			}
		}
	}
	for i := 0; i < c.opt.Concurrency; i++ {
		wg.Add(1)
		go worker()
	}
	pending.Add(1)
	go func(d string) { defer pending.Done(); queue <- job{dir: d} }(root)
	go func() { pending.Wait(); close(queue) }()
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		// No firstErr and the walk context is done: the CALLER cancelled (the
		// internal cancel is only called once a worker has an error). That is a
		// cancellation — EINTR, retryable — and not the transport fault this
		// used to report (BFS-039).
		return nil, classifyRequest(parent, err, "walk", root)
	}
	snap.source = SourcePropfindWalk
	return snap, nil
}

// LookupLive resolves one path against the server (PROPFIND Depth: 0). It is
// what a Lookup does for a path the snapshot does not hold: one round trip,
// named as such.
func (c *Client) LookupLive(ctx context.Context, p string) (*Node, *OpError) {
	metas, err := c.Propfind(ctx, p, "0")
	if err != nil {
		return nil, err
	}
	want := strings.Trim(normalisePath(p), "/")
	for _, m := range metas {
		if strings.Trim(normalisePath(m.Path), "/") != want {
			continue
		}
		return &Node{Path: want, IsDir: m.IsDir, Size: m.Size, Mode: m.Mode, Mtime: m.Mtime, Hash: m.Hash,
			Kind: m.Kind, LinkTarget: m.LinkTarget}, nil
	}
	return nil, &OpError{Op: "PROPFIND", Path: p, Status: 404, Errno: ErrnoENOENT, Cause: CauseServerError,
		Detail: "the server did not report the requested path"}
}

// NewSnapshot builds an empty snapshot rooted at root.
func NewSnapshot(root string) *Snapshot {
	s := &Snapshot{
		nodes:    map[string]Node{},
		children: map[string]map[string]struct{}{},
		read:     map[string]bool{},
		root:     normalisePath(root),
		taken:    timeNow(),
	}
	s.put(Node{Path: "", IsDir: true})
	return s
}

// put inserts or replaces a node and maintains the parent's child set.
func (s *Snapshot) put(n Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putLocked(n)
}

func (s *Snapshot) putLocked(n Node) {
	old, existed := s.nodes[n.Path]
	s.nodes[n.Path] = n
	if n.Path == "" {
		return
	}
	parent := path.Dir(n.Path)
	if parent == "." {
		parent = ""
	}
	if !existed || old.IsDir != n.IsDir {
		if s.children[parent] == nil {
			s.children[parent] = map[string]struct{}{}
		}
		s.children[parent][path.Base(n.Path)] = struct{}{}
	}
	if n.IsDir && s.children[n.Path] == nil {
		s.children[n.Path] = map[string]struct{}{}
	}
}

// Lookup answers a path from the in-memory tree. It is the call READDIRPLUS
// resolution and every `stat` end up in, and it costs ZERO round trips.
func (s *Snapshot) Lookup(p string) (Node, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[strings.Trim(normalisePath(p), "/")]
	return n, ok
}

// Children returns a directory's entries sorted by name — the answer to one
// READDIRPLUS, served from memory.
func (s *Snapshot) Children(dir string) []Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	dir = strings.Trim(normalisePath(dir), "/")
	set := s.children[dir]
	out := make([]Node, 0, len(set))
	for name := range set {
		child := name
		if dir != "" {
			child = dir + "/" + name
		}
		if n, ok := s.nodes[child]; ok {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Has reports whether the snapshot holds a path (even a negative answer must be
// distinguishable from "not in the snapshot" — a walk that never saw a name is a
// different fact from a name known to be absent).
func (s *Snapshot) Has(p string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.nodes[strings.Trim(normalisePath(p), "/")]
	return ok
}

// Known reports whether the snapshot has READ this directory's LISTING.
//
// It is deliberately NOT "the directory node exists": a directory can be present
// in the tree (learned as some other directory's child) without anyone having
// asked what is inside it, and answering that empty would be a lie the caller
// cannot detect. Measured while building this row: with the empty-map semantics,
// `ls` of a directory learned from its parent's listing returned nothing at all.
func (s *Snapshot) Known(dir string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.read[strings.Trim(normalisePath(dir), "/")]
}

// MarkDirRead records that a directory's listing has been read (the snapshot op
// with depth=infinity marks every directory it returned; the PROPFIND path marks
// each directory it walks).
func (s *Snapshot) MarkDirRead(dirs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range dirs {
		s.read[strings.Trim(normalisePath(d), "/")] = true
	}
}

// MarkAllDirsRead marks every directory in the tree as read. It is what a
// successful depth=infinity snapshot means: the server answered the whole
// subtree, so every listing in it is known.
func (s *Snapshot) MarkAllDirsRead() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, n := range s.nodes {
		if n.IsDir {
			s.read[p] = true
		}
	}
}

// Count returns the number of nodes.
func (s *Snapshot) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.nodes)
}

// ObservationCursor reports the ledger cursor this observation was minted at,
// and whether it was minted at all (BFS-063). It is the resume point a caller
// reports to its invalidator via Invalidator.Observed: presenting it on the
// events poll is how a client says "my view is the tree as of this cursor",
// which is the only thing that buys a quiet answer — a client that presents no
// cursor at all is told the interval is unvouched, because the server cannot
// vouch for a view it knows nothing about.
//
// false is returned for anything that is not a whole-tree, untruncated
// snapshot-op answer: the PROPFIND fallback was never minted by the server, and
// a truncated view is missing paths it does not know are missing.
func (s *Snapshot) ObservationCursor() (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.obsSeq, s.obsOK
}

// Source names where this tree came from (SourceSnapshotOp / SourcePropfindWalk).
func (s *Snapshot) Source() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.source
}

// Calls returns how many server calls the tree cost.
func (s *Snapshot) Calls() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.calls
}

// Truncated reports whether the server truncated the answer (always reported,
// never silently shortened — BFS-004 A-11).
func (s *Snapshot) Truncated() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.truncated
}

// Root returns the snapshot's root path.
func (s *Snapshot) Root() string { return s.root }

// Drop removes paths from the node tree, and with each path its parent's
// readdir entry — a new name must appear in the next readdir/READDIRPLUS answer
// (BFS-005 §4.2 step 2). Dropping a directory drops its whole subtree: the
// subtree's metadata is stale by construction once the collection changed.
func (s *Snapshot) Drop(paths ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range paths {
		p = strings.Trim(normalisePath(p), "/")
		if _, ok := s.nodes[p]; !ok {
			// An unknown path still kills its parent's cached child set: the
			// event says the name may have appeared.
			s.dropChildLocked(p)
			continue
		}
		s.dropSubtreeLocked(p)
		s.dropChildLocked(p)
		n++
	}
	return n
}

func (s *Snapshot) dropSubtreeLocked(p string) {
	if p == "" {
		return // the root is always part of the tree; dropping it is never implied
	}
	prefix := p + "/"
	for k := range s.nodes {
		if k == p || strings.HasPrefix(k, prefix) {
			delete(s.nodes, k)
		}
	}
	for k := range s.children {
		if k == p || strings.HasPrefix(k, prefix) {
			delete(s.children, k)
		}
	}
	for k := range s.read {
		if k == p || strings.HasPrefix(k, prefix) {
			delete(s.read, k)
		}
	}
}

func (s *Snapshot) dropChildLocked(p string) {
	parent := path.Dir(p)
	if parent == "." {
		parent = ""
	}
	if set := s.children[parent]; set != nil {
		delete(set, path.Base(p))
	}
}

// Put inserts or replaces one node in the tree (used by the standard-protocol
// PROPFIND path, which learns entries one directory at a time).
func (s *Snapshot) Put(n Node) { s.put(n) }

// DropReaddir forgets a directory's child list — and nothing else. This is the
// precise operation for "a name in this directory may have appeared or gone":
// the directory itself is still there, but its readdir answer must be re-read.
func (s *Snapshot) DropReaddir(dirs ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range dirs {
		d = strings.Trim(normalisePath(d), "/")
		if s.read[d] {
			delete(s.read, d)
			n++
		}
		delete(s.children, d)
	}
	return n
}

// DropAll empties the tree — the full resync a sequence gap or an overflow
// event demands.
func (s *Snapshot) DropAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.nodes)
	s.nodes = map[string]Node{}
	s.children = map[string]map[string]struct{}{}
	s.read = map[string]bool{}
	s.putLocked(Node{Path: "", IsDir: true})
	return n
}

// normalisePath cleans an in-tree path: no leading slash, no "." segments, and
// "/" itself becomes "".
func normalisePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return ""
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == "/" {
		return ""
	}
	return cleaned
}

// snapshotEntryPath is the snapshot op's own path spelling, kept for the message
// the client prints when a path is outside the tree it snapshotted.
func (s *Snapshot) String() string {
	return fmt.Sprintf("snapshot(root=%q source=%s nodes=%d calls=%d truncated=%v)", s.root, s.Source(), s.Count(), s.Calls(), s.Truncated())
}
