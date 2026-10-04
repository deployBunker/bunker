//go:build linux

// The Linux binding (BFS-003 §2: go-fuse, pure Go, CGO_ENABLED=0). Everything
// platform-specific in this package is in this file and in fs_unsupported.go;
// the client it drives (internal/fsclient) is OS-neutral by construction.
//
// What is NATIVE here and what is OURS (BFS-005 §2):
//
//   - `fuse.MountOptions.ExplicitDataCacheControl = true` and a per-open
//     `FOPEN_DIRECT_IO` make the kernel keep NO file data, so our bounded
//     content-addressed cache is the only byte cache and the only figure that
//     can be compared with `du`.
//   - `Server.InodeNotify` / `EntryNotify` / `DeleteNotify` are the kernel half
//     of invalidation; availability is probed, never assumed.
//   - The write precondition is OURS, on the guarantee that no go-fuse option
//     enables CAP_WRITEBACK_CACHE: every chunk is visible to us.
//   - THE PUBLICATION POINT IS FLUSH, never RELEASE (BFS-020). FLUSH is the one
//     request the kernel waits for on close(2) — measured: the RELEASE handler's
//     PUT completed 4.1 ms after close(2) had already returned, which is the
//     window in which a following `rename` reached the server as a 404 and came
//     back to the caller as a bare ENOENT for a file whose close had succeeded.
//     A name is therefore on the server BEFORE the caller's close(2) returns, and
//     a following operation on the same mount sees it.
//   - The node tree is populated from ONE `X-Bunker-Op: snapshot` call, and
//     READDIRPLUS resolves each entry through OUR Lookup in-process — which is
//     the whole measurable win, and is explicitly NOT delegation.
package fsmount

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/deployBunker/bunker/internal/fsclient"
)

// writeBufferMaxDefault bounds one open write handle's local buffer. The buffer
// is ONE file being written, not a copy of the tree (the storage constraint this
// row exists for is about the tree); it is reported in `bunker fs status` as
// write_handles_buffered/write_buffer_bytes, and a write exceeding it fails with
// a named EFBIG rather than growing without bound.
const writeBufferMaxDefault = DefaultWriteBufferMax

// cacheSeedWriteMax is the largest landed write whose bytes are re-inserted into
// the cache. Above it the cache is left alone and a read-after-write pays one
// round trip; the alternative would be holding the bytes in memory.
const cacheSeedWriteMax = 8 << 20

// Mount is one mounted bunker-fs filesystem.
type Mount struct {
	opts Options
	// dir is the MOUNT directory: the client's local footprint for this
	// endpoint. It holds the cache directory (cacheDir) and the mount's own
	// state (status.json, conflicts.jsonl, the write-buffer spills).
	dir string
	// cacheDir is the directory `--cache-max-size` bounds: <dir>/cache. See
	// fsclient/layout.go for why the bound names a directory that holds only
	// cache bytes (BFS-031).
	cacheDir string
	logf     func(string, ...any)
	client   *fsclient.Client
	cache    *fsclient.Cache
	wp       *fsclient.WritePath

	snap atomic.Pointer[fsclient.Snapshot]
	inv  *fsclient.Invalidator

	server    *fuse.Server
	rootInode *fs.Inode

	mu      sync.Mutex
	reads   map[uint64]*readHandle
	writes  map[uint64]*writeHandle
	nextFh  uint64
	bufDocs map[uint64]int64

	treeMu    sync.RWMutex
	treePin   string
	protoSeen string

	// The negotiated MaxWrite (BFS-052): the figure the INIT reply carried,
	// stored at the point the mount code still has it in hand — go-fuse's
	// KernelSettings view does not retain the reply's own MaxWrite field, so
	// this is the mount's record of what the kernel agreed to. Read by the
	// capability probe under fuseMu (the probe runs after it is set; the
	// field is never written again).
	negotiatedMaxWrite atomic.Int64

	// bounds is what the kernel has been told each path's size is — the size a
	// FUSE read is bounded by (see bound.go). boundRefusals counts reads
	// refused because the content was longer than that bound (the silent
	// truncation this row exists for); boundRepairs counts the times the
	// metadata was corrected from the content a read fetched.
	bounds        *boundRegistry
	boundRefusals atomic.Int64
	boundRepairs  atomic.Int64
	// boundLast names the most recent bound divergence (path + the two figures
	// that disagreed), so `bunker fs status` can point at the file rather than
	// only counting. atomic.Value: written from the read path, read by the
	// status loop.
	boundLast atomic.Value // string

	// writeShapeRefusals counts write shapes REFUSED before they published
	// anything because this surface cannot complete them (BFS-030): a resize
	// arriving while a write-intent handle is live on the path — the destructive
	// half of an in-place rewrite whose write half cannot land. writeShapeLast
	// names the most recent refusal, so the figure can be audited per file.
	// Reported because a rule that fires silently is not a rule anyone can
	// audit (the same reason boundRefusals is reported).
	writeShapeRefusals atomic.Int64
	writeShapeLast     atomic.Value // string

	// appendPublished/appendRefused are the append figures (BFS-021): an append
	// that LANDED and an append that was REFUSED before it published anything
	// (a boundary refusal, a conflict refusal, or a base that could not be
	// read). appendLast names the most recent event, so the figure can be
	// audited per file. Reported for the same reason the write-shape refusals
	// are: a rule that fires silently is not a rule anyone can audit.
	appendPublished atomic.Int64
	appendRefused   atomic.Int64
	appendLast      atomic.Value // string

	// The symlink figures (BFS-018). readlinks counts targets answered from the
	// surface's DECLARED target, symlinkCreated counts links created through
	// this mount, and refusals/lastRefusal name every operation this mount
	// refused with a reason (an undeclared surface, a link whose target the
	// surface did not publish, a hardlink, or an entry whose declared type this
	// client cannot present). Every one of them is reported for the same reason
	// the figures above are: a refusal the owner cannot see is indistinguishable
	// from one that never happened, and this row's whole subject is a wrong
	// answer that used to be silent.
	readlinks      atomic.Int64
	symlinkCreated atomic.Int64
	typeRefusals   atomic.Int64
	typeRefusalMu  sync.Mutex
	typeRefusalAt  []string // bounded ring of the most recent refusals

	transportMu sync.RWMutex
	verdict     string
	cause       string
	lastOK      time.Time

	statusMu sync.Mutex
	stop     chan struct{}
	stopped  chan struct{}
	once     sync.Once

	// Counters for the measurement: the figures a battery reads.
	opsTotal       atomic.Int64
	notifiesTotal  atomic.Int64
	snapshotCalls  atomic.Int64
	conflictCount  atomic.Int64
	delegatedCalls atomic.Int64

	notifyMu     sync.Mutex
	notifyProbed bool
	notifyFlags  [3]bool

	// The FUSE capability negotiation (BFS-052): what this mount requested
	// versus what the kernel granted, probed AFTER fs.Mount succeeds and
	// reported in the status document's fuse block. Guarded by its own
	// mutex: the probe writes once, the status loop reads every tick.
	fuseMu    sync.RWMutex
	fuseState FuseState

	// notifyCh carries kernel invalidation to a DEDICATED goroutine. It must
	// never be issued from inside a FUSE request handler: the write to
	// /dev/fuse in NotifyEntry/NotifyDelete/NotifyContent can block on the
	// kernel, and the kernel is at that moment waiting for our reply to the very
	// request we are handling. Measured while building this row: a `mv` through
	// the mount deadlocked the whole filesystem — the rename landed on the
	// server, the client never answered, and every later operation on the tree
	// hung until the process was killed.
	notifyCh      chan notifyRequest
	notifyDropped atomic.Int64
}

// notifyRequest is one unit of kernel invalidation work.
type notifyRequest struct {
	paths []string
	// deleted names the paths the CLIENT proved are gone (its own unlink/rmdir/
	// rename, or a Lookup that answered ENOENT). Everything else gets
	// EntryNotify, which BFS-005 §4.2 declares sufficient to stop the kernel
	// serving the name.
	deleted []string
	// full is a whole-tree invalidation: a resync, or a coalesced batch.
	full bool
}

// node is one inode in the filesystem. It carries its own in-tree path: the
// snapshot is keyed by path, the kernel hands us names, and a path walk per
// operation would be the only other option.
type node struct {
	fs.Inode
	m *Mount
	p string
}

// MountAt mounts the WebDAV surface at opts.Mountpoint. The bind preflight runs
// under the 5 s deadline: a failure REFUSES the mount and leaves nothing at the
// mountpoint (never the "silent empty tree" the durability spec records as live
// today).
func MountAt(opts Options) (*Mount, error) {
	if err := opts.Normalize(); err != nil {
		return nil, err
	}
	m := &Mount{
		opts:     opts,
		logf:     opts.Logf,
		reads:    map[uint64]*readHandle{},
		writes:   map[uint64]*writeHandle{},
		bufDocs:  map[uint64]int64{},
		bounds:   newBoundRegistry(),
		verdict:  "healthy",
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
		notifyCh: make(chan notifyRequest, 256),
	}
	if m.logf == nil {
		m.logf = func(string, ...any) {}
	}
	if opts.AllowOther {
		m.logf("%s", AllowedOtherStripped())
	}
	tightened, err := PrepareMountpoint(opts.Mountpoint)
	if err != nil {
		return nil, err
	}
	if tightened {
		m.logf("bunker-fs: tightened mountpoint %s to 0700 (it was group/world accessible)", opts.Mountpoint)
	}
	dir, err := MountDir(opts)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("bunker-fs: create mount dir: %w", err)
	}
	// BFS-031: a cache written by an earlier build sits DIRECTLY in the mount
	// directory, beside the mount's own state. It is migrated into the cache
	// subdirectory rather than discarded (the bytes are the owner's cache) or
	// merged by hand: the move is a rename inside one directory.
	if mig, merr := fsclient.MigrateMountLayout(dir); merr != nil {
		m.logf("bunker-fs: cache layout migration: %v", merr)
	} else if mig.Moved {
		m.logf("bunker-fs: migrated a pre-BFS-031 cache: %d entr(y|ies), %d B, %d stale temp(s) → %s",
			mig.Files, mig.Bytes, mig.TempsRemoved, fsclient.MountCacheDir(dir))
	}
	m.dir = dir
	m.cacheDir = fsclient.MountCacheDir(dir)

	client, err := fsclient.NewClient(fsclient.Options{
		BaseURL:         opts.BaseURL,
		Concurrency:     opts.Concurrency,
		MaxConnsPerHost: opts.MaxConnsPerHost,
		OpTimeout:       opts.OpTimeout,
		BindTimeout:     opts.BindTimeout,
		Username:        opts.Username,
		Password:        opts.Password,
		UserAgent:       "bunker-fs/1 (Linux; go-fuse)",
	})
	if err != nil {
		return nil, err
	}
	m.client = client

	// Bind preflight: the capability handshake and the tree identity.
	info, oerr := client.Handshake(context.Background())
	if oerr != nil {
		return nil, fmt.Errorf("bunker-fs: bind refused (%s, cause %s): %w", fsclient.ErrnoName(oerr.Errno), oerr.Cause, oerr)
	}
	client.PinTree(info.Tree)
	m.treePin = info.Tree
	m.protoSeen = info.Proto
	m.recordOK()
	m.logf("bunker-fs: bound to %s tree=%s rev=%s proto=%s extensions=%q",
		opts.BaseURL, info.Tree, info.Rev, info.Proto, info.Extensions)
	if info.Degradation != nil {
		m.logf("bunker-fs: declared degradation: %s (scope=%s mode=%s): %s",
			info.Degradation.Capability, info.Degradation.Scope, info.Degradation.Mode, info.Degradation.Detail)
	}

	cache, err := fsclient.OpenCache(fsclient.CacheConfig{
		Dir:           m.cacheDir,
		MaxBytes:      opts.CacheMaxBytes,
		MaxEntryBytes: opts.CacheMaxEntryBytes,
		// The ENTRY bound and the staged-refresh width (BFS-044's
		// --cache-max-entries / --cache-max-inflight). Both are enforced by the
		// cache and both are read back by `bunker fs status` from the cache's own
		// figures, so a flag that did not reach the cache would be visible
		// rather than assumed.
		MaxEntries:  opts.CacheMaxEntries,
		MaxInFlight: opts.CacheMaxInFlight,
		MaxAge:      opts.CacheMaxAge,
	})
	if err != nil {
		return nil, err
	}
	m.cache = cache
	m.wp = fsclient.NewWritePath(client, cache, dir, opts.OnConflict)
	m.snap.Store(fsclient.NewSnapshot(""))

	if opts.Snapshot {
		if err := m.refreshSnapshot(context.Background(), ""); err != nil {
			m.logf("bunker-fs: initial snapshot failed (%v); falling back to the standard PROPFIND path", err)
		}
	}

	// The kernel options. The three TTLs are set to ZERO explicitly: with nil
	// Options go-fuse applies ONE SECOND entry and attribute timeouts, and a
	// nonzero TTL lets the kernel's mtime-driven attribute cache serve a stale
	// stat — which is exactly how git decides a file did not change.
	zero := time.Duration(0)
	fsOpts := &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:     "bunker-fs",
			Name:       "bunker-fs",
			AllowOther: false, // STRIPPED: always private, whatever was requested
			// The kernel keeps no file data; our cache is the only one.
			ExplicitDataCacheControl: true,
			DisableXAttrs:            true,
			// Keep enough requests in flight that the client's concurrency is
			// the binding constraint, not go-fuse's default byte budget: the
			// measured lever is concurrency, so nothing here may cap it below
			// what the client was configured for.
			MaxBackground:           max(32, opts.Concurrency*2),
			MaxInflightRequestBytes: max(32, opts.Concurrency*2) * (2 << 20),
			MaxWrite:                1 << 20,
			Logger:                  nil,
		},
		AttrTimeout:     &zero,
		EntryTimeout:    &zero,
		NegativeTimeout: &zero,
		NullPermissions: false,
	}
	root := &node{m: m, p: ""}
	// The negotiated MaxWrite is recorded BEFORE fs.Mount: the value goes to
	// the kernel inside the INIT exchange fs.Mount drives, and the probe that
	// runs right after needs it as the effective figure (go-fuse retains the
	// kernel's request, not its own reply, on KernelSettings). What is
	// recorded is the figure AS THE LIBRARY CAPS IT — a request above
	// MAX_KERNEL_WRITE never reaches the kernel, and the probe reports that
	// divergence rather than the request.
	if fsOpts.MaxWrite > fuse.MAX_KERNEL_WRITE {
		fsOpts.MaxWrite = fuse.MAX_KERNEL_WRITE
	}
	m.negotiatedMaxWrite.Store(int64(fsOpts.MaxWrite))
	server, err := fs.Mount(opts.Mountpoint, root, fsOpts)
	if err != nil {
		return nil, fmt.Errorf("bunker-fs: mount %s: %w", opts.Mountpoint, err)
	}
	m.server = server
	m.rootInode = root.EmbeddedInode()

	// THE CAPABILITY PROBE (BFS-052): the mount now exists, so the kernel's
	// per-connection observables are readable and the INIT exchange has
	// happened. Requested vs effective is compared per capability and the
	// result is reported in the status document's fuse block — a requested
	// cap the kernel clamped, lacks, or exposes no evidence for is a named
	// degradation, never a silent success. Read-only; it never fails the
	// mount.
	m.probeFuseState(fsOpts.MountOptions)

	// The invalidation channel: push where the target has a watcher, the
	// declared poll form where it does not, the revision poll where it has
	// neither — always reporting which one answered. The revision poll covers
	// the tree only at the served revision's granularity: on a git tree that
	// is HEAD, so uncommitted edits move it only on commit (BFS-048); the
	// events poll is the mechanism that sees uncommitted edits.
	m.inv = fsclient.NewInvalidator(client, fsclient.InvalidateOptions{
		Mode:         opts.Invalidation,
		PollInterval: opts.PollInterval,
		// IdleTimeout is the mount's own declared silence deadline; ZERO (the
		// default) leaves it to the invalidator to DERIVE from the period the
		// server's capability document declares — which is that option's
		// documented contract, not a fallback (BFS-041 §8.1). The armed value is
		// reported in the status document's invalidation block.
		IdleTimeout: opts.InvalidateIdleTimeout,
		OnDrop:      m.dropPaths,
		OnResync: func(reason string) {
			m.logf("bunker-fs: resync (%s)", reason)
			ctx, cancel := context.WithTimeout(context.Background(), opts.OpTimeout)
			defer cancel()
			if err := m.refreshSnapshot(ctx, ""); err != nil {
				m.logf("bunker-fs: resync snapshot failed: %v", err)
			}
		},
	})
	// The bind-time observation, reported before the channel starts: the mount
	// took this tree's view with `--snapshot`, and the channel must be able to
	// say WHERE that view is from (BFS-063) or every poll would be answered as an
	// unvouched interval.
	if snap := m.snapshot(); snap != nil {
		cursor, minted := snap.ObservationCursor()
		m.inv.Observed(cursor, minted)
	}
	go func() {
		if err := m.inv.Run(context.Background()); err != nil {
			m.logf("bunker-fs: invalidation channel stopped: %v", err)
		}
	}()
	go m.notifyLoop()
	go m.statusLoop()
	go func() {
		if err := server.WaitMount(); err == nil {
			m.logf("bunker-fs: readdirplus=%v", m.readdirPlus())
		}
	}()
	return m, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Wait blocks until the kernel unmounts the filesystem.
func (m *Mount) Wait() { m.server.Wait() }

// Unmount tears the mount down: stop the status loop, flush the write path's
// state, unmount, and write a final status document.
func (m *Mount) Unmount() error {
	m.once.Do(func() {
		close(m.stop)
		err := m.server.Unmount()
		if err != nil {
			_ = err
		}
		_ = m.cache.Close()
		m.writeStatus()
	})
	return nil
}

// Server exposes the go-fuse server (used by tests to unmount).
func (m *Mount) Server() *fuse.Server { return m.server }

// Mountpoint returns the resolved mountpoint.
func (m *Mount) Mountpoint() string { return m.opts.Mountpoint }

// CacheDir returns the resolved cache directory — the directory the byte bound
// `--cache-max-size` names and is enforced against (<mount dir>/cache).
func (m *Mount) CacheDir() string {
	if m.cacheDir != "" {
		return m.cacheDir
	}
	return m.dir
}

// MountDir returns the mount's own directory: the client's local footprint for
// this endpoint, holding the cache directory and the mount's state.
func (m *Mount) MountDir() string { return m.dir }

// Client exposes the client (measurements read its request and in-flight
// counters).
func (m *Mount) Client() *fsclient.Client { return m.client }

// Cache exposes the cache (tests assert the bound).
func (m *Mount) Cache() *fsclient.Cache { return m.cache }

// Snapshot exposes the current node tree.
func (m *Mount) Snapshot() *fsclient.Snapshot { return m.snap.Load() }

// readdirPlus reports whether the kernel negotiated READDIRPLUS, which is what
// makes one snapshot answer a whole `ls -l`.
func (m *Mount) readdirPlus() bool {
	if m.server == nil {
		return false
	}
	return m.server.KernelSettings().Flags&fuse.CAP_READDIRPLUS != 0
}

// readdirPlusNegotiated is the exported form for the CLI and the battery.
func (m *Mount) ReaddirPlusNegotiated() bool { return m.readdirPlus() }

// refreshSnapshot replaces the node tree with ONE server call.
func (m *Mount) refreshSnapshot(ctx context.Context, root string) *fsclient.OpError {
	snap, err := m.client.SnapshotTree(ctx, root, true)
	if err != nil {
		m.recordFailure(err)
		return err
	}
	m.snap.Store(snap)
	m.snapshotCalls.Add(1)
	m.recordOK()
	// The observation this mount now holds is reported to the invalidation
	// channel (BFS-063), so the channel can declare WHERE its view is from: the
	// whole-tree answer's minted cursor when the snapshot op served it, or the
	// cursor of the notice that provoked this re-observation when the PROPFIND
	// fallback did. Without a report the channel presents no cursor and is told
	// the interval is unvouched — honest, and a resync per poll, which is why
	// every path that establishes a view reports it.
	if m.inv != nil {
		cursor, minted := snap.ObservationCursor()
		m.inv.Observed(cursor, minted)
	}
	return nil
}

// dropPaths applies BFS-005 §4.2 in order: our path entry, our metadata (and
// the parent's readdir answer), then the kernel's copies.
func (m *Mount) dropPaths(paths []string, full bool) {
	if full {
		m.cache.DropAll()
		if snap := m.snapshot(); snap != nil {
			snap.DropAll()
		}
		m.wp.NoteInvalidated(paths...)
		m.queueNotify(notifyRequest{full: true})
		return
	}
	m.cache.Drop(paths...)
	if snap := m.snapshot(); snap != nil {
		snap.Drop(paths...)
	}
	m.wp.NoteInvalidated(paths...)
	m.queueNotify(notifyRequest{paths: paths})
}

// queueNotify hands kernel invalidation to the notifier goroutine. It NEVER
// blocks and never touches an inode: a FUSE request handler must return, and a
// notification issued from inside one can wait on the kernel that is waiting for
// that handler's reply (measured: a `mv` deadlocked the whole filesystem).
//
// A full queue is COALESCED into one whole-tree invalidation rather than
// dropped silently: dropping would leave the kernel serving names we know are
// stale, and a whole-tree notify is what BFS-005 §4.1 prescribes when the client
// cannot say which names moved.
func (m *Mount) queueNotify(req notifyRequest) {
	select {
	case m.notifyCh <- req:
		return
	default:
	}
	m.notifyDropped.Add(1)
	select {
	case m.notifyCh <- notifyRequest{full: true}:
	default:
	}
}

// notifyLoop is the ONLY goroutine that issues kernel notifications.
func (m *Mount) notifyLoop() {
	for {
		select {
		case <-m.stop:
			return
		case req := <-m.notifyCh:
			if req.full {
				m.notifyTree("")
				continue
			}
			deleted := map[string]bool{}
			for _, d := range req.deleted {
				deleted[d] = true
			}
			for _, p := range req.paths {
				if deleted[p] {
					m.notifyDeleted(p)
					continue
				}
				m.notifyPath(p)
			}
		}
	}
}

// NotifyDropped reports how many notifications had to be coalesced because the
// queue was full. It is reported rather than hidden: a coalesced batch widened
// an invalidation, it did not lose one.
func (m *Mount) NotifyDropped() int64 { return m.notifyDropped.Load() }

// notifyTree walks the inode tree and invalidates everything. Used by a full
// resync, where the client cannot say which names moved.
func (m *Mount) notifyTree(dir string) {
	if m.server == nil || m.rootInode == nil {
		return
	}
	ino := m.findInode(dir)
	if ino == nil {
		return
	}
	_ = ino.NotifyContent(0, 0)
	for name, child := range ino.Children() {
		if child == nil {
			continue
		}
		m.notifiesTotal.Add(1)
		_ = ino.NotifyEntry(name)
		if child.IsDir() {
			m.notifyTree(joinPath(dir, name))
		} else {
			_ = child.NotifyContent(0, 0)
		}
	}
}

// notifyPath invalidates one name: EntryNotify by default, DeleteNotify only
// where the CLIENT proved the name is gone (BFS-005 §4.2 step 3), because the
// event does not carry the kind.
func (m *Mount) notifyPath(p string) {
	if m.server == nil || m.rootInode == nil {
		return
	}
	dir := path.Dir(p)
	if dir == "." {
		dir = ""
	}
	name := path.Base(p)
	parent := m.findInode(dir)
	if parent == nil {
		return
	}
	m.notifiesTotal.Add(1)
	_ = parent.NotifyEntry(name)
	if child := parent.GetChild(name); child != nil {
		_ = child.NotifyContent(0, 0)
	}
}

// notifyDeleted issues DeleteNotify for a name the client itself removed or
// learned was gone. Availability is PROBED, never assumed: DeleteNotify's floor
// is 7.18, and below it the kernel falls back to EntryNotify, which is still
// enough to stop the kernel serving the name (BFS-005 §4.2 step 3).
func (m *Mount) notifyDeleted(p string) {
	if m.server == nil || m.rootInode == nil {
		return
	}
	dir := path.Dir(p)
	if dir == "." {
		dir = ""
	}
	name := path.Base(p)
	parent := m.findInode(dir)
	if parent == nil {
		return
	}
	child := parent.GetChild(name)
	if child == nil || !m.notifySupported()[2] {
		m.notifyPath(p)
		return
	}
	m.notifiesTotal.Add(1)
	_ = parent.NotifyDelete(name, child)
}

// notifySupported probes the kernel's notification floors at runtime, as the
// spec requires (never assumed). Order: [inode, entry, delete].
func (m *Mount) notifySupported() [3]bool {
	m.notifyMu.Lock()
	defer m.notifyMu.Unlock()
	if m.notifyProbed {
		return m.notifyFlags
	}
	ks := m.server.KernelSettings()
	m.notifyFlags = [3]bool{
		ks.SupportsNotify(fuse.NOTIFY_INVAL_INODE),
		ks.SupportsNotify(fuse.NOTIFY_INVAL_ENTRY),
		ks.SupportsNotify(fuse.NOTIFY_DELETE),
	}
	m.notifyProbed = true
	m.logf("bunker-fs: kernel notify floors probed: inode=%v entry=%v delete=%v",
		m.notifyFlags[0], m.notifyFlags[1], m.notifyFlags[2])
	return m.notifyFlags
}

// NotifySupport exposes the probed floors for the status document and the
// battery (a kernel below 7.18 loses only the DeleteNotify refinement).
func (m *Mount) NotifySupport() (inode, entry, del bool) {
	f := m.notifySupported()
	return f[0], f[1], f[2]
}

// findInode resolves an in-tree path to the kernel's inode, as far as the
// kernel has actually looked at it. A path never looked up has no kernel entry
// and needs no notify: there is nothing to invalidate.
func (m *Mount) findInode(p string) *fs.Inode {
	if m.rootInode == nil {
		return nil
	}
	p = strings.Trim(p, "/")
	if p == "" {
		return m.rootInode
	}
	cur := m.rootInode
	for _, seg := range strings.Split(p, "/") {
		next := cur.GetChild(seg)
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}

// snapshot returns the current node tree (never nil after New).
func (m *Mount) snapshot() *fsclient.Snapshot { return m.snap.Load() }

// recordOK marks the transport healthy.
func (m *Mount) recordOK() {
	m.transportMu.Lock()
	m.verdict = "healthy"
	m.cause = ""
	m.lastOK = time.Now()
	m.transportMu.Unlock()
}

// recordFailure records the transport verdict and its named cause. The causes
// are unchanged from §7.1; the verdict adds the tree-identity class, which
// demands different recovery from a transport fault.
func (m *Mount) recordFailure(err *fsclient.OpError) {
	if err == nil {
		return
	}
	m.transportMu.Lock()
	defer m.transportMu.Unlock()
	m.cause = err.Error()
	switch err.Cause {
	case fsclient.CauseStaleIdentity:
		m.verdict = "stale_identity"
	case fsclient.CauseUnreachableConnect, fsclient.CauseUnreachableDeadline, fsclient.CauseUnreachableReset:
		m.verdict = "unreachable"
	case fsclient.CauseConflict:
		// A conflict is per file and the transport is fine.
		return
	case fsclient.CauseStaleBound:
		// A stale published size is per file too: the tree and the transport
		// are fine, and the recovery (re-open the path) is local.
		return
	}
}

// refusalRingMax bounds the refusal ring `bunker fs status` can show. It is a
// ring, not a log: the COUNTS are unbounded (a figure), the recent entries are
// bounded (a sample), and the bound is named so the sample cannot be read as
// the whole history.
const refusalRingMax = 8

// recordRefusal records an operation this mount REFUSED with a named cause
// (BFS-018): a link create or read against a surface that did not declare the
// extension, a hardlink, or an entry whose declared type this client cannot
// present. It is the audit surface for a refusal that used to be a wrong
// answer — counting it and naming the most recent few is what makes "the mount
// refused" distinguishable from "the mount silently did something else".
//
// It delegates to recordFailure for a SERVER-side failure (a real 409/412, a
// transport fault) and deliberately does NOT for a local-capability refusal: a
// surface that cannot express a link has not made the transport unhealthy, and
// recording it as a transport verdict would send the operator to the wrong
// subsystem.
func (m *Mount) recordRefusal(err *fsclient.OpError) {
	if err == nil {
		return
	}
	m.typeRefusals.Add(1)
	m.typeRefusalMu.Lock()
	entry := fmt.Sprintf("%s %s: %s", err.Op, err.Path, err.Detail)
	if entry == fmt.Sprintf("%s %s: ", err.Op, err.Path) || err.Detail == "" {
		entry = fmt.Sprintf("%s %s: %s", err.Op, err.Path, err.Error())
	}
	m.typeRefusalAt = append(m.typeRefusalAt, entry)
	if len(m.typeRefusalAt) > refusalRingMax {
		m.typeRefusalAt = m.typeRefusalAt[len(m.typeRefusalAt)-refusalRingMax:]
	}
	m.typeRefusalMu.Unlock()
	if err.Cause != fsclient.CauseLocalCapability {
		m.recordFailure(err)
	}
}

// symlinkState is the `symlink` block of the status document (BFS-018): the
// surface's own declaration, what this mount did with it, and what it refused
// by name.
func (m *Mount) symlinkState() fsclient.SymlinkState {
	caps := m.client.SymlinkCapability()
	st := fsclient.SymlinkState{
		Declared:        caps.Present,
		DeclaredVersion: caps.V,
		DeclaredName:    caps.Name,
		TypeProperty:    caps.TypeProperty,
		TargetProperty:  caps.TargetProperty,
		ReadlinksTotal:  m.readlinks.Load(),
		CreatedTotal:    m.symlinkCreated.Load(),
	}
	st.LastRefusals, st.RefusalsTotal = m.RefusalRing()
	return st
}

// RefusalRing returns the most recent refusals, oldest first, and the total
// count: the two figures `bunker fs status` reports.
func (m *Mount) RefusalRing() ([]string, int64) {
	m.typeRefusalMu.Lock()
	defer m.typeRefusalMu.Unlock()
	out := make([]string, len(m.typeRefusalAt))
	copy(out, m.typeRefusalAt)
	return out, m.typeRefusals.Load()
}

// Status builds the reported state document.
func (m *Mount) Status() fsclient.Status {
	cs := m.cache.Stats()
	st := fsclient.Status{
		Mount:       fsclient.MountID(m.opts.BaseURL),
		Mode:        m.inv.Mode(),
		Endpoint:    m.opts.BaseURL,
		Mountpoint:  m.opts.Mountpoint,
		Concurrency: m.opts.Concurrency,
		OnConflict:  m.wp.OnConflict(),
		Cache:       cs,
		Transport: fsclient.TransportState{
			Verdict:      m.verdictNow(),
			Requests:     m.client.Requests(),
			CancelsTotal: m.client.Cancels(),
			Proto:        m.client.Proto(),
			Tree:         m.client.Tree(),
			Rev:          m.client.Rev(),
		},
		Conflicts: fsclient.ConflictState{RefusalsTotal: m.wp.Refusals(), Last: m.wp.LastConflict()},
		Symlink:   m.symlinkState(),
	}
	cur, high := m.client.InFlight()
	st.Transport.InFlight, st.Transport.InFlightMax = cur, high
	m.transportMu.RLock()
	if !m.lastOK.IsZero() {
		age := time.Since(m.lastOK).Milliseconds()
		st.Transport.LastOKAgeMS = &age
	}
	st.Transport.Cause = m.cause
	m.transportMu.RUnlock()
	st.Invalidation = m.inv.State()
	// The resolved option set (BFS-044): what this mount actually obeys, so the
	// cache's entry bound, the invalidation cadence and every hot-file knob are
	// readable at runtime instead of only knowable from the command line.
	st.Config = m.opts.EffectiveConfig()
	// The refresh block is assembled from the CACHE's own counters and the
	// invalidator's state, because neither owns both: the staged window is the
	// cache's, the mechanism is the invalidator's, and the queue belongs to a
	// subsystem that is not in this build (BFS-045).
	st.Invalidation.Refresh = fsclient.RefreshFromCache(cs)
	if snap := m.snapshot(); snap != nil {
		st.Snapshot = fsclient.SnapshotState{
			Source:    snap.Source(),
			Nodes:     snap.Count(),
			Calls:     snap.Calls(),
			Truncated: snap.Truncated(),
		}
	}
	m.mu.Lock()
	for _, n := range m.bufDocs {
		if n > 0 {
			st.WriteHandlesBuffered++
		}
		st.WriteBufferBytes += n
	}
	m.mu.Unlock()
	st.ReadBound = fsclient.ReadBoundState{
		RefusalsTotal:    m.boundRefusals.Load(),
		CorrectionsTotal: m.boundRepairs.Load(),
	}
	if v, ok := m.boundLast.Load().(string); ok {
		st.ReadBound.Last = v
	}
	st.WriteShape = fsclient.WriteShapeState{RefusalsTotal: m.writeShapeRefusals.Load()}
	if v, ok := m.writeShapeLast.Load().(string); ok {
		st.WriteShape.Last = v
	}
	// The append figures (BFS-021), including the bound the read-modify-publish
	// obeys: a file at or above it cannot be appended through this surface, and a
	// bound the owner cannot see is not a bound.
	st.Append = fsclient.AppendState{
		PublishedTotal: m.appendPublished.Load(),
		RefusedTotal:   m.appendRefused.Load(),
		MaxFileBytes:   appendBound,
	}
	if v, ok := m.appendLast.Load().(string); ok {
		st.Append.Last = v
	}
	// The FUSE capability negotiation (BFS-052): the requested-vs-effective
	// figure set, with one degradation per capability the kernel clamped,
	// lacks, or gave no evidence for. SPEC-linux-io-max §6's law — the
	// EFFECTIVE values appear in the mount's status output.
	if skipped, reason := fuseProbeSkipped(); skipped {
		// Unreachable on Linux builds; the shape exists so every build's
		// status document answers the same question with the same fields.
		st.Fuse = FuseState{Source: reason}
	} else {
		st.Fuse = m.FuseState()
	}
	heldTotal, outstanding, evicted, heldLast := m.wp.Held()
	st.RefusalHolds = fsclient.RefusalHoldState{
		HeldTotal:    heldTotal,
		Outstanding:  outstanding,
		EvictedTotal: evicted,
		Last:         heldLast,
	}
	// The mount's OWN storage, measured beside its own bound (BFS-031). The
	// cache directory's bytes and bound come from the cache's own figures; the
	// state files and the write-buffer spills are measured here, because only
	// the mount knows where it puts them. The spill budget is what the open
	// buffered handles may hold on disk, which is bounded per handle and
	// reported as a count (write_handles_buffered).
	spillBudget := int64(st.WriteHandlesBuffered) * writeBufferMaxDefault
	st.State = fsclient.MeasureState(m.dir, st.Cache.DirBytes, st.Cache.MaxBytes, spillBudget)
	return st
}

func (m *Mount) verdictNow() string {
	m.transportMu.RLock()
	defer m.transportMu.RUnlock()
	if m.verdict == "" {
		return "healthy"
	}
	return m.verdict
}

// WriteStatusNow writes the status document (the CLI calls it on demand via a
// signal-free path: the mount writes it on a cadence anyway).
func (m *Mount) WriteStatusNow() { m.writeStatus() }

func (m *Mount) writeStatus() {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	if err := fsclient.WriteStatus(m.dir, m.Status()); err != nil {
		m.logf("bunker-fs: write status: %v", err)
	}
}

// statusLoop keeps the status document fresh, so `bunker fs status` from any
// terminal reads a file rather than talking to the mount.
func (m *Mount) statusLoop() {
	defer close(m.stopped)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	m.writeStatus()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.writeStatus()
		}
	}
}

// ---------------------------------------------------------------------------
// The node tree.
// ---------------------------------------------------------------------------

func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// inoFor derives a stable inode number from a path, so the kernel's inode for a
// name survives a re-lookup and our notify calls have a target.
func inoFor(p string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(p))
	return h.Sum64() | 1 // never 0: 0 means "no inode" to the kernel
}

// modeOf renders a node's mode bits.
//
// THE TYPE IS THE WHOLE POINT (BFS-018). A node the surface declared a symlink
// becomes S_IFLNK, and that single bit is what makes the kernel resolve the
// link ITSELF — so an open of a link path reaches the target instead of being
// served, as the defect did, a regular file of mode 0777 whose content was the
// link's own target path. The permission bits of a symlink are 0777 by
// convention and Linux ignores them (there is no chmod on a link), so a link's
// mode is the convention and not the surface's octal value: reporting the
// wire's 0777 as a REGULAR file's permissions is exactly the shape that was
// filed, and reporting it under S_IFLNK is what the same number means.
//
// A node whose declared kind this client does not know is refused by
// typeRefusal at every site that would have to describe it: it never falls
// through to S_IFREG, because "a type I cannot present" is not "a file".
func modeOf(nd fsclient.Node) uint32 {
	if nd.IsSymlink() {
		return syscall.S_IFLNK | 0o777
	}
	var perm uint32 = 0o644
	if nd.IsDir {
		perm = 0o755
	}
	if len(nd.Mode) == 4 || len(nd.Mode) == 3 {
		var v uint32
		if _, err := fmt.Sscanf(nd.Mode, "%o", &v); err == nil {
			perm = v & 0o7777
		}
	}
	if nd.IsDir {
		return syscall.S_IFDIR | perm
	}
	return syscall.S_IFREG | perm
}

// typeRefusal is the named refusal for a node whose DECLARED kind this client
// cannot present (BFS-018). It exists so that "the surface said something I do
// not know" is a loud, specific error rather than a plausible-looking regular
// file: reporting an unhonoured type as a file is the same defect one type
// over, and a caller that has to be told has to be told BY NAME.
func typeRefusal(nd fsclient.Node, op string) *fsclient.OpError {
	return &fsclient.OpError{
		Op: op, Path: nd.Path, Errno: fsclient.ErrnoEIO, Cause: fsclient.CauseLocalCapability,
		Detail: fmt.Sprintf("this surface declares the entry type %q, which this client cannot present; refusing rather than describing it as a regular file (a link, or any other non-file entry, must never be materialised as a file)", nd.Kind),
	}
}

func fillAttr(a *fuse.Attr, nd fsclient.Node) {
	a.Mode = modeOf(nd)
	a.Size = uint64(nd.Size)
	if nd.IsSymlink() {
		// lstat(2) reports a symlink's size as the length of its target path,
		// and that is what the surface sends. A surface that sent no size is
		// answered from the target this client actually holds, so st_size and
		// readlink cannot disagree about the link.
		a.Size = uint64(len(nd.LinkTarget))
	}
	a.Ino = inoFor(nd.Path)
	a.Nlink = 1
	if nd.IsDir {
		a.Nlink = 2
	}
	a.Blksize = 4096
	a.Blocks = (uint64(nd.Size) + 511) / 512
	mt := nd.Mtime
	if mt.IsZero() {
		mt = time.Unix(0, 0)
	}
	a.Mtime = uint64(mt.Unix())
	a.Atime = a.Mtime
	a.Ctime = a.Mtime
	a.Owner.Uid = uint32(os.Getuid())
	a.Owner.Gid = uint32(os.Getgid())
}

// errnoFor maps a client refusal onto the filesystem's errno (§7.1).
func errnoFor(err *fsclient.OpError) syscall.Errno {
	if err == nil {
		return 0
	}
	if err.Errno == 0 {
		return syscall.EIO
	}
	return err.Errno
}

// Getattr answers from the snapshot, with zero round trips whenever the node was
// snapshotted. A path the snapshot does not hold costs ONE PROPFIND — named,
// not hidden.
func (n *node) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	nd, ok := n.m.snapshot().Lookup(n.p)
	if !ok {
		live, err := n.m.client.LookupLive(ctx, n.p)
		if err != nil {
			n.m.recordFailure(err)
			return errnoFor(err)
		}
		nd = *live
		n.m.snapshot().Put(nd)
	}
	if !nd.KindHonoured() {
		err := typeRefusal(nd, "GETATTR")
		n.m.recordRefusal(err)
		return errnoFor(err)
	}
	fillAttr(&out.Attr, nd)
	// This reply IS the kernel's i_size for the path: record what we told it, so
	// a later read can tell whether it is about to serve longer content than the
	// kernel will let a reader see (bound.go).
	n.m.notePublished(nd)
	out.SetTimeout(0)
	n.m.opsTotal.Add(1)
	return 0
}

// Lookup is where READDIRPLUS resolution lands: go-fuse resolves each entry of a
// readdir through this method in-process, so one snapshot answers a whole
// `ls -l`. Zero round trips for a snapshotted path.
func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	cp := joinPath(n.p, name)
	nd, ok := n.m.snapshot().Lookup(cp)
	if !ok {
		live, err := n.m.client.LookupLive(ctx, cp)
		if err != nil {
			if err.Errno == fsclient.ErrnoENOENT {
				return nil, syscall.ENOENT
			}
			n.m.recordFailure(err)
			return nil, errnoFor(err)
		}
		nd = *live
		n.m.snapshot().Put(nd)
	}
	if !nd.KindHonoured() {
		err := typeRefusal(nd, "LOOKUP")
		n.m.recordRefusal(err)
		return nil, errnoFor(err)
	}
	fillAttr(&out.Attr, nd)
	n.m.notePublished(nd)
	out.SetEntryTimeout(0)
	out.SetAttrTimeout(0)
	n.m.opsTotal.Add(1)
	child := &node{m: n.m, p: cp}
	return n.NewInode(ctx, child, fs.StableAttr{Mode: modeOf(nd), Ino: inoFor(cp)}), 0
}

// Readdir answers from the snapshot. A directory the snapshot has not read costs
// ONE PROPFIND Depth: 1 — the standard-protocol path, so this client works
// against a build with no snapshot op.
func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	snap := n.m.snapshot()
	if !snap.Known(n.p) {
		metas, err := n.m.client.Propfind(ctx, n.p, "1")
		if err != nil {
			n.m.recordFailure(err)
			return nil, errnoFor(err)
		}
		for _, meta := range metas {
			p := strings.Trim(meta.Path, "/")
			if p == n.p {
				continue
			}
			snap.Put(fsclient.Node{Path: p, IsDir: meta.IsDir, Size: meta.Size, Mode: meta.Mode, Mtime: meta.Mtime, Hash: meta.Hash,
				Kind: meta.Kind, LinkTarget: meta.LinkTarget})
		}
		// Only NOW is this directory's listing known: the PROPFIND above is what
		// read it.
		snap.MarkDirRead(n.p)
	}
	kids := snap.Children(n.p)
	entries := make([]fuse.DirEntry, 0, len(kids)+2)
	for _, k := range kids {
		entries = append(entries, fuse.DirEntry{
			Name: path.Base(k.Path),
			Mode: modeOf(k),
			Ino:  inoFor(k.Path),
		})
	}
	n.m.opsTotal.Add(1)
	return fs.NewListDirStream(entries), 0
}

// Open returns a handle and FOPEN_DIRECT_IO: the kernel keeps no file data, so
// our cache is the only byte cache (and the only figure comparable with `du`).
//
// The handle is a READ handle whatever the flags say — which is the premise
// BFS-030's refusal rests on (a write handle is only ever created by Create, for a
// path that does not exist). ONE exception is carried on it rather than created by
// it: an O_APPEND open gets an append state, so the write half of POSIX append can
// be served as a whole-file publication (BFS-021). No other write through an
// existing path is served, and a write through a non-append handle is still
// answered EOPNOTSUPP by node.Write.
func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	n.m.mu.Lock()
	n.m.nextFh++
	fh := n.m.nextFh
	h := &readHandle{m: n.m, p: n.p, fh: fh, writeIntent: flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0}
	if flags&syscall.O_APPEND != 0 {
		h.ap = &appendHandle{m: n.m, owner: h, p: n.p, fh: fh}
	}
	n.m.reads[fh] = h
	n.m.mu.Unlock()
	return h, fuse.FOPEN_DIRECT_IO, 0
}

// Write is the ONE dispatch point for a FUSE write, and it exists so the kernel's
// chunk stream reaches the RIGHT handle:
//
//   - a CREATE's write handle (`*writeHandle`) is called exactly as the bridge
//     called it, so the whole existing write path is untouched;
//   - an O_APPEND handle's append state buffers the chunk at the offset the kernel
//     gave it (BFS-021);
//   - anything else is answered EOPNOTSUPP — which is byte-for-byte what
//     go-fuse's bridge answered before this method existed
//     (`return 0, fuse.ENOTSUP`, fs/bridge.go), so BFS-012's refusal of in-place
//     writes through an existing path is unchanged.
func (n *node) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	switch h := fh.(type) {
	case *writeHandle:
		return h.Write(ctx, data, off)
	case *readHandle:
		if h.ap != nil {
			return h.ap.write(ctx, data, off)
		}
	}
	return 0, syscall.EOPNOTSUPP
}

// Create starts the write path: the handle buffers the arriving chunks and
// publishes them at the first publication point (Flush/Fsync/Release).
func (n *node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	cp := joinPath(n.p, name)
	m := n.m
	h, nd := m.beginCreate(cp, mode)
	fillAttr(&out.Attr, nd)
	out.SetEntryTimeout(0)
	out.SetAttrTimeout(0)
	child := &node{m: m, p: cp}
	return n.NewInode(ctx, child, fs.StableAttr{Mode: modeOf(nd), Ino: inoFor(cp)}), h, fuse.FOPEN_DIRECT_IO, 0
}

// beginCreate starts the write path for a path the kernel believes is absent: the
// write handle, and the metadata entry the create is answerable for. It is the
// half of Create that does not need a live kernel bridge, so a handler-level cell
// can start the SAME state — which is what BFS-020's cells do: the defect they
// pin is entirely in the handle's publication and the rename that follows it, and
// the inode construction above (n.NewInode) requires a mounted filesystem.
func (m *Mount) beginCreate(cp string, mode uint32) (*writeHandle, fsclient.Node) {
	h := m.newWriteHandle(cp, true)
	nd := fsclient.Node{Path: cp, IsDir: false, Mode: fmt.Sprintf("%04o", mode&0o777), Mtime: time.Now(), Kind: fsclient.KindFile}
	m.snapshot().Put(nd)
	m.notePublished(nd)
	// A create means the name is absent as far as the kernel is concerned, so any
	// cached bytes this client still holds for it belong to something else (a file
	// removed under us, or a name this mount itself moved away): drop them, or a
	// read of the new file is served the old bytes.
	m.cache.Drop(cp)
	return h, nd
}

// Mkdir creates a collection.
func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	cp := joinPath(n.p, name)
	nd, errno := n.m.beginMkdir(ctx, cp, mode)
	if errno != 0 {
		return nil, errno
	}
	fillAttr(&out.Attr, nd)
	out.SetEntryTimeout(0)
	out.SetAttrTimeout(0)
	return n.NewInode(ctx, &node{m: n.m, p: cp}, fs.StableAttr{Mode: modeOf(nd), Ino: inoFor(cp)}), 0
}

// beginMkdir is the mount-side half of mkdir(2): the request, the snapshot
// entry, the invalidation of the collection it appeared in, the counters — and
// nothing that needs a live kernel bridge. It exists for the same reason
// beginCreate and createLink do: a handler-level cell can drive the SAME state a
// mounted filesystem drives, so a rule about what a mutation does to the
// directory it was made in can be asserted exactly rather than only through a
// kernel (BFS-019).
func (n *Mount) beginMkdir(ctx context.Context, cp string, mode uint32) (fsclient.Node, syscall.Errno) {
	if err := n.client.Mkcol(ctx, cp); err != nil {
		n.recordFailure(err)
		return fsclient.Node{}, errnoFor(err)
	}
	nd := fsclient.Node{Path: cp, IsDir: true, Mode: fmt.Sprintf("%04o", mode&0o777), Mtime: time.Now(), Kind: fsclient.KindDir}
	n.snapshot().Put(nd)
	n.snapshot().DropReaddir(path.Dir(cp)) // the parent's readdir answer changed
	n.recordOK()
	return nd, 0
}

// Symlink creates a symlink THROUGH the mount (BFS-018). It is the write half
// of the row: with Readlink answering the target and this creating a link, a
// git checkout of a tree carrying symlinks produces real symlinks on the server
// instead of regular files holding a target path.
//
// The target is sent as a DECLARED request header (X-Bunker-Link-Target) with an
// empty body, never as file content: the surface has no way to spell "this entry
// is a link" in bytes, and every attempt to do so is the defect — a file whose
// content is the target path. When the surface never declared the extension the
// operation is REFUSED by name (EOPNOTSUPP) rather than attempted: a build that
// ignored the header would answer 201 for a file, and the caller would be told a
// link was created.
func (n *node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	cp := joinPath(n.p, name)
	if errno := n.m.createLink(ctx, cp, target); errno != 0 {
		return nil, errno
	}
	nd, _ := n.m.snapshot().Lookup(cp)
	fillAttr(&out.Attr, nd)
	out.SetEntryTimeout(0)
	out.SetAttrTimeout(0)
	return n.NewInode(ctx, &node{m: n.m, p: cp}, fs.StableAttr{Mode: modeOf(nd), Ino: inoFor(cp)}), 0
}

// createLink is the mount-side half of symlink(2): the declared request, the
// snapshot entry, the counters and the notification bookkeeping — everything
// except the inode construction, which needs a live kernel bridge.
//
// It is a method for the same reason beginCreate is (BFS-020): a cell can drive
// the SAME half a live mount drives, so the rule can be asserted exactly
// without a mounted filesystem, and a live arm only adds the kernel's own path.
func (n *Mount) createLink(ctx context.Context, cp, target string) syscall.Errno {
	if _, err := n.client.PutLink(ctx, cp, target, fsclient.PutPrecondition{}); err != nil {
		n.recordRefusal(err)
		return errnoFor(err)
	}
	nd := fsclient.Node{Path: cp, Mode: "0777", Mtime: time.Now(),
		Kind: fsclient.KindSymlink, LinkTarget: target}
	n.symlinkCreated.Add(1)
	n.snapshot().Put(nd)
	n.snapshot().DropReaddir(path.Dir(cp)) // the parent's readdir answer changed
	// A name that now holds a LINK holds no bytes of its own: anything cached
	// under it belongs to whatever the name used to be.
	n.cache.Drop(cp)
	n.recordOK()
	return 0
}

// Readlink answers the target of a symlink.
//
// THE TARGET IS NOT THE BYTES (BFS-018). This method reads the target the
// surface DECLARED (b:link-target, or the snapshot entry's link_target) and
// never fetches the path's bytes: fetching them is the dereference-and-copy
// answer, and there is no code path in this client that could turn a link into
// a copy of its target. Where the surface gave a type of symlink but published
// no target — an older build — the read is REFUSED by name rather than guessed:
// an empty target would be a link to nowhere, which would corrupt the tree just
// as surely as a file holding the target path.
func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	nd, ok := n.m.snapshot().Lookup(n.p)
	if !ok || nd.Kind == fsclient.KindUnreported {
		// An entry with no declared type cannot be a link as far as this client
		// knows (no standard property expresses one), so the live lookup is what
		// tells us: one PROPFIND, named and bounded.
		live, err := n.m.client.LookupLive(ctx, n.p)
		if err != nil {
			n.m.recordFailure(err)
			return nil, errnoFor(err)
		}
		nd, ok = *live, true
		n.m.snapshot().Put(nd)
	}
	if !ok || !nd.IsSymlink() {
		// The kernel resolves links itself, so a readlink on a path this
		// surface did not declare a link is a stale dentry; EINVAL is what
		// Linux answers for that.
		return nil, syscall.EINVAL
	}
	if nd.LinkTarget == "" {
		err := &fsclient.OpError{Op: "READLINK", Path: n.p, Errno: fsclient.ErrnoEOPNOTSUPP, Cause: fsclient.CauseLocalCapability,
			Detail: "this surface declares the entry a symlink but published no target for it (b:link-target / link_target absent): the link cannot be read without inventing a target, so the read is refused"}
		n.m.recordRefusal(err)
		return nil, errnoFor(err)
	}
	n.m.readlinks.Add(1)
	n.m.opsTotal.Add(1)
	return []byte(nd.LinkTarget), 0
}

// Link (a HARDLINK) is refused, and the refusal is argued from the wire rather
// than from effort: the surface describes a directory entry per PATH
// (`path, type, size, mtime_unix_ms, mode, hash`), and it carries no inode
// number and no link count, so two names for one inode are not expressible.
// A client that accepted the request would hold two independent cache entries
// and two independent write records for one file, and a write through one name
// would leave the other name's cached bytes looking current — a stale read this
// client has no way to detect, because nothing on the wire would ever tell it
// the two names are one file.
//
// EOPNOTSUPP is the answer Linux gives for a filesystem without hard links, and
// `ln` reports it as "Operation not supported" — loud, specific, and never a
// silently-made copy.
func (n *node) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	cp := joinPath(n.p, name)
	err := &fsclient.OpError{Op: "LINK", Path: cp, Errno: fsclient.ErrnoEOPNOTSUPP, Cause: fsclient.CauseLocalCapability,
		Detail: "this surface describes one directory entry per path and carries no inode or link count, so a hardlink cannot be represented: two names for one file would get two independent metadata and cache records and a write through one name would leave the other stale. Use a symlink (supported) or a copy"}
	n.m.recordFailure(err)
	n.m.recordRefusal(err)
	return nil, syscall.EOPNOTSUPP
}

// Unlink removes a file.
//
// The type is checked against what this mount most recently told the kernel
// BEFORE the request goes out, because the server's DELETE on a COLLECTION is
// RECURSIVE: if this mount ever believed a directory was a file, an unlink would
// take the whole subtree with it. The check costs nothing and needs no request —
// the kernel only unlinks a path whose attrs it holds, and those attrs are this
// mount's own.
func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	cp := joinPath(n.p, name)
	if nd, ok := n.m.snapshot().Lookup(cp); ok && nd.IsDir {
		return syscall.EISDIR
	}
	if err := n.m.client.Delete(ctx, cp, ""); err != nil {
		n.m.recordFailure(err)
		return errnoFor(err)
	}
	n.m.snapshot().Drop(cp)
	n.m.snapshot().DropReaddir(n.p)
	n.m.cache.Drop(cp)
	// BFS-033: the removal answers the refusal's subject — there is no refused
	// write left to enforce on a name that no longer exists, so the hold goes
	// with the file rather than standing on a path nothing can read again.
	n.m.wp.NoteDeleted(cp)
	n.m.queueNotify(notifyRequest{paths: []string{cp}, deleted: []string{cp}})
	n.m.recordOK()
	return 0
}

// Rmdir removes a collection.
//
// POSIX: `rmdir` on a NON-EMPTY directory FAILS with ENOTEMPTY — and that failure
// is load-bearing. git walks UP the directory chain after deleting a ref and
// stops at the first level that will not go away (refs/files-backend.c,
// remove_empty_directories), so a mount that passes the request through turns a
// ref cleanup into a RECURSIVE DELETE: MEASURED on the tree this row started from,
// `rmdir <mount>/.git` returned **rc=0** and the served tree's `.git` went from 10
// entries to 0 — the whole directory, because the surface's DELETE on a
// collection removes the subtree (docs/evidence/BFS-020-rmdir.txt). Found while
// running this row's acceptance: the write-lock defect was masking it by failing
// git one step earlier.
//
// The emptiness is asked of the SERVER (one `Depth: 1` PROPFIND), never of this
// mount's own readdir answer: that listing is exactly what git believed when it
// decided the directory was empty, and BFS-018 is the open row for its coming
// back empty while the directory has entries. The cost is one request per rmdir,
// and it is named rather than implied.
func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	cp := joinPath(n.p, name)
	metas, perr := n.m.client.Propfind(ctx, cp, "1")
	if perr != nil {
		n.m.recordFailure(perr)
		return errnoFor(perr)
	}
	for _, meta := range metas {
		if strings.Trim(meta.Path, "/") != cp {
			return syscall.ENOTEMPTY
		}
	}
	if err := n.m.client.Delete(ctx, cp, ""); err != nil {
		n.m.recordFailure(err)
		return errnoFor(err)
	}
	n.m.snapshot().Drop(cp)
	n.m.snapshot().DropReaddir(n.p)
	n.m.cache.Drop(cp)
	// The name is gone: everything remembered for it goes with it (BFS-033's hold
	// and BFS-020's base record — a create of this name later must resolve its own
	// base rather than inherit one that describes a file that is not there).
	n.m.wp.NoteDeleted(cp)
	n.m.queueNotify(notifyRequest{paths: []string{cp}, deleted: []string{cp}})
	n.m.recordOK()
	return 0
}

// Rename moves a name. The destination is a MOVE on the surface, and both
// parents' readdir answers are dropped.
//
// TWO THINGS THIS DOES THAT A BARE MOVE DOES NOT (BFS-020):
//
//  1. THE PUBLICATION BARRIER. If this mount is still holding bytes for `src`
//     that the server does not have yet — a create that has not reached a
//     publication point, or an O_APPEND open whose append has not been published
//     — the MOVE would be answered by the SERVER's 404 and reach the caller as a
//     bare ENOENT for a file whose close(2) succeeded. So the pending publication
//     is made FIRST, and its refusal (if any) is returned as itself rather than
//     flattened into ENOENT: the caller is told what actually happened.
//  2. THE HANDLES FOLLOW THE NAME. The write path is path-addressed, so after a
//     successful move the live handles for `src` are RE-TARGETED at `dst`.
//     Without that, a later publication on a still-open handle would publish
//     under the old name — resurrecting a name the caller moved away, which is a
//     lie about the file existing (the same class the stale-lock case names).
func (n *node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	np, ok := newParent.(*node)
	if !ok {
		return syscall.EXDEV
	}
	src := joinPath(n.p, name)
	dst := joinPath(np.p, newName)
	if errno := n.m.publishPending(ctx, src); errno != 0 {
		return errno
	}
	if err := n.m.client.Move(ctx, src, dst, true); err != nil {
		n.m.recordFailure(err)
		return errnoFor(err)
	}
	n.m.retargetHandles(src, dst)
	n.m.snapshot().Drop(src, dst)
	n.m.snapshot().DropReaddir(n.p, np.p)
	// BOTH names' CACHED BYTES go, not only the source's (BFS-020): a rename
	// REPLACES the destination, so the entry this client holds for it describes the
	// file that was just moved away. Keeping it made the mount serve the OLD
	// content for a name it had itself just replaced — MEASURED: after
	// `printf … > .git/HEAD.lock; mv .git/HEAD.lock .git/HEAD` the mount answered
	// `ref: refs/heads/main` while the served tree held `ref: refs/heads/…`, and a
	// live `git checkout -b` read its own just-written HEAD back as the old branch
	// (docs/evidence/BFS-020-content.txt).
	n.m.cache.Drop(src, dst)
	// BOTH names' records go (BFS-020): the source name no longer refers to the
	// file this client remembered (a later create of it would inherit a stale base
	// and be refused 412 — measured on a live `git checkout -b`), and the
	// destination name now holds a DIFFERENT file than the one the record
	// describes.
	n.m.wp.NoteDeleted(src)
	n.m.wp.NoteDeleted(dst)
	n.m.queueNotify(notifyRequest{paths: []string{src}, deleted: []string{src}})
	n.m.queueNotify(notifyRequest{paths: []string{dst}})
	n.m.recordOK()
	return 0
}

// writeIntentOn reports whether a handle opened for WRITING is live on this
// path. It is read from the mount's own handle table — the same table the read
// path keeps and Release empties — so the record cannot outlive the handle it
// describes, and it needs no second bookkeeping to keep in step.
//
// The predicate is deliberately NOT the Setattr's `fh`: MEASURED
// (docs/evidence/BFS-030-trace.txt), the SETATTR the kernel sends for
// an O_TRUNC open arrives with fh=0 and valid=FATTR_SIZE|FATTR_FH, naming no
// handle at all — indistinguishable from a deliberate path-based resize. The
// live write-intent handle is the only signal that separates "the destructive
// half of a rewrite this surface cannot complete" from "a deliberate resize".
func (m *Mount) writeIntentOn(p string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.reads {
		if h.p == p && h.writeIntent {
			return true
		}
	}
	return false
}

// pendingFor answers the ONE question a following operation has to ask before it
// acts on a name: is this mount still holding bytes for this path that the SERVER
// does not have? It reads the mount's own handle table — the same table Release
// empties — so it cannot describe a handle that no longer exists.
//
// It returns the live write handles (a create that has not reached a publication
// point) and any append state (an O_APPEND open with an unpublished append).
func (m *Mount) pendingFor(p string) ([]*writeHandle, []*appendHandle) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ws []*writeHandle
	for _, h := range m.writes {
		if h.p == p {
			ws = append(ws, h)
		}
	}
	var as []*appendHandle
	for _, h := range m.reads {
		if h.ap != nil && h.ap.p == p {
			as = append(as, h.ap)
		}
	}
	return ws, as
}

// publishPending is THE PUBLICATION BARRIER a following operation on the same
// mount takes before it acts on a name this mount is holding unpublished bytes
// for (BFS-020). Publishing is idempotent, so calling it when there is nothing
// pending is free — one lock and one map walk, no request.
//
// It returns the publication's own refusal when there is one, rather than
// letting the caller see the server's 404 for a file the mount simply had not
// sent yet: "the file does not exist" and "I have not sent it" are different
// answers and the caller can act on only one of them.
func (m *Mount) publishPending(ctx context.Context, p string) syscall.Errno {
	ws, as := m.pendingFor(p)
	for _, h := range ws {
		if errno := h.publish(ctx); errno != 0 {
			return errno
		}
	}
	for _, a := range as {
		if errno := a.publish(ctx); errno != 0 {
			return errno
		}
	}
	return 0
}

// retargetHandles moves the live handles' OWN PATH from src to dst after a
// successful rename, so a later publication on a still-open handle lands under
// the name the file now has. The write path is path-addressed (the snapshot, the
// cache, the bound registry and the publication's precondition are all keyed by
// path), so a handle that kept the old path would publish a SECOND name the
// caller moved away — a resurrected file, and a lie about what exists.
//
// The mount lock is taken only to snapshot the handle list, and each handle is
// then locked on its own: the handlers lock their own mutex before the mount's
// (writeHandle.Write takes h.mu and then m.mu for the buffer accounting), so
// holding m.mu across a handle lock would invert that order.
func (m *Mount) retargetHandles(src, dst string) {
	m.mu.Lock()
	var ws []*writeHandle
	for _, h := range m.writes {
		if h.p == src {
			ws = append(ws, h)
		}
	}
	var rs []*readHandle
	for _, h := range m.reads {
		if h.p == src {
			rs = append(rs, h)
		}
	}
	m.mu.Unlock()
	for _, h := range ws {
		h.setPath(dst)
	}
	for _, h := range rs {
		h.setPath(dst)
	}
}

// Setattr supports truncation (the one attribute a build tool actually moves)
// and refuses the rest loudly rather than pretending: mode/uid/gid/times are
// not carried by this surface.
func (n *node) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if size, ok := in.GetSize(); ok {
		if errno := n.truncate(ctx, size); errno != 0 {
			return errno
		}
	} else if in.Valid&(fuse.FATTR_MODE|fuse.FATTR_UID|fuse.FATTR_GID|fuse.FATTR_ATIME|fuse.FATTR_MTIME) != 0 {
		return syscall.EOPNOTSUPP
	}
	h := &node{m: n.m, p: n.p}
	return h.Getattr(ctx, fh, out)
}

// truncate rewrites the file at a new size. It is a read-modify-write published
// as one conditional PUT, which is why it is the only supported Setattr: the
// precondition is whole-file on both sides of the wire.
//
// It is REFUSED — before anything is read or published — when a handle opened
// for writing is live on this path. Such a resize is the destructive half of an
// in-place rewrite, and this surface cannot complete the write half of one: Open
// hands back a READ handle whatever the open flags say, and a write handle is
// only ever created by Create, for a path that does not exist (BFS-012 measured
// every in-place shape as refused). Publishing the resize anyway is BFS-030,
// the write-side twin of BFS-025: `open(path,'wb')` emptied the file as ONE
// conditional PUT of zero bytes, the write that followed failed with
// EOPNOTSUPP, and the caller lost their content while being told the operation
// had failed. A refusal costs the caller the shape it could not complete
// anyway; a published resize costs them the file.
//
// The predicate is the mount's own live-handle table, not the Setattr's fh:
// MEASURED (docs/evidence/BFS-030-trace.txt), the SETATTR the kernel
// sends for an O_TRUNC open carries fh=0 with valid=FATTR_SIZE|FATTR_FH — it
// names no handle at all, exactly like a deliberate path-based resize. The live
// write-intent handle is the only signal that tells the two apart.
//
// A deliberate resize with no write handle open (`truncate -s N file`,
// `os.truncate`) is unaffected, which is the shape BFS-012's truncate
// measurements rely on.
func (n *node) truncate(ctx context.Context, size uint64) syscall.Errno {
	if n.m.writeIntentOn(n.p) {
		err := &fsclient.OpError{
			Op: "SETATTR", Path: n.p, Errno: fsclient.ErrnoEOPNOTSUPP,
			Cause:  fsclient.CauseWriteShapeUnsupported,
			Detail: fmt.Sprintf("refusing to resize %s to %d bytes: a handle opened for writing is live on the path, and this surface cannot write an existing file in place — publishing the resize would destroy the original before the write that follows it fails. The file is left byte-identical; use a whole-file write (create the path anew) or `truncate -s` outside a write handle", n.p, int64(size)),
		}
		n.m.writeShapeRefusals.Add(1)
		n.m.writeShapeLast.Store(fmt.Sprintf("%s: size=%d", n.p, int64(size)))
		n.m.logf("bunker-fs: refused to resize %s to %d bytes: a write handle is live on the path and this surface cannot write an existing file in place, so the resize would be published before a write that cannot land. The file is unchanged", n.p, int64(size))
		n.m.recordFailure(err)
		return errnoFor(err)
	}
	// read through the normal path so the cache and the base hash stay in
	// agreement with what we are about to replace
	data, _, cerr := n.m.client.Get(ctx, n.p, "")
	if cerr != nil {
		n.m.recordFailure(cerr)
		return errnoFor(cerr)
	}
	current := data
	if uint64(len(current)) == size {
		return 0
	}
	if uint64(len(current)) < size {
		current = append(current, make([]byte, int(size)-len(current))...)
	} else {
		current = current[:size]
	}
	base, berr := n.m.wp.ResolveBase(ctx, n.p)
	if berr != nil {
		n.m.recordFailure(berr)
		return errnoFor(berr)
	}
	if _, perr := n.m.wp.PublishBytes(ctx, n.p, current, base); perr != nil {
		n.m.recordFailure(perr)
		return errnoFor(perr)
	}
	n.m.snapshot().Drop(n.p)
	n.m.recordOK()
	return 0
}

// Statfs reports the cache's bound, so `df` shows the number that actually
// bounds a copy of the tree on this client.
func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	cs := n.m.cache.Stats()
	free := cs.MaxBytes - cs.UsedBytes
	if free < 0 {
		free = 0
	}
	out.Bsize = 4096
	out.Frsize = 4096
	out.Blocks = uint64(cs.MaxBytes / 4096)
	out.Bfree = uint64(free / 4096)
	out.Bavail = out.Bfree
	out.Files = uint64(cs.Entries)
	out.Ffree = 1 << 20
	out.NameLen = 255
	return 0
}

// ---------------------------------------------------------------------------
// Read handles.
// ---------------------------------------------------------------------------

// readHandle serves one open file: it fetches once per (path, hash), pins the
// blob it serves, and returns bytes from memory afterwards (direct I/O means the
// kernel asks every time, and every time is answered locally).
type readHandle struct {
	mu          sync.Mutex
	m           *Mount
	p           string
	fh          uint64
	writeIntent bool
	// ap is the append state, non-nil exactly when this open carried O_APPEND
	// (BFS-021). An append is the one write shape an EXISTING path can serve
	// here, because it can be expressed as a whole-file publication; every other
	// write through an existing path stays refused, which is BFS-012's rule and
	// BFS-030's premise.
	ap     *appendHandle
	data   []byte
	hash   string
	loaded bool
	pinned bool
}

// setPath moves this handle's own path after a successful rename (BFS-020), so a
// later publication — and a later read — uses the name the file now has. The
// append state's path is a copy for the same reason: an unpublished append on a
// renamed file must land under the new name.
func (h *readHandle) setPath(p string) {
	h.mu.Lock()
	h.p = p
	ap := h.ap
	h.mu.Unlock()
	if ap != nil {
		ap.setPath(p)
	}
}

func (h *readHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.loaded {
		if errno := h.load(ctx); errno != 0 {
			return nil, errno
		}
	}
	if off >= int64(len(h.data)) {
		return fuse.ReadResultData(nil), 0
	}
	end := off + int64(len(dest))
	if end > int64(len(h.data)) {
		end = int64(len(h.data))
	}
	return fuse.ReadResultData(h.data[off:end]), 0
}

// load fills the handle's bytes: from our cache when we hold this path, else
// with ONE GET whose hash is the base hash a later write will carry. The read
// that fills the cache is the read that decides the conflict.
//
// Before anything is served, the bytes are reconciled with the size the kernel
// is holding for the path (guardServed): a FUSE read is clamped by that size, so
// serving longer content is a silent truncation — the defect this rule exists
// for. The reconciliation costs a comparison on the fast path and no request.
func (h *readHandle) load(ctx context.Context) syscall.Errno {
	if hash, _, ok := h.m.cache.Lookup(h.p); ok {
		// The bytes and the reference on them are taken in ONE critical section
		// (BFS-038). Between Get and Pin there is a window in which a concurrent
		// refresh's pointer swap — or a Drop — removes the path entry this
		// reader just read, and if that was the blob's last reference the file
		// goes with it: a reference taken after that is a reference to nothing,
		// and the handle would be holding bytes it can no longer vouch for.
		if data, ok := h.m.cache.GetPinned(h.p, hash); ok {
			if !h.distrustCached(int64(len(data)), hash) {
				h.data, h.hash, h.loaded, h.pinned = data, hash, true, true
				h.m.wp.NoteRead(h.p, hash)
				return 0
			}
			// Not servable after all: the entry was dropped, so the reference
			// the read took is given straight back.
			h.m.cache.Unpin(hash)
		}
	}
	data, meta, err := h.m.client.Get(ctx, h.p, "")
	if err != nil {
		h.m.recordFailure(err)
		if err.Cause == fsclient.CauseUnreachableConnect || err.Cause == fsclient.CauseUnreachableDeadline || err.Cause == fsclient.CauseUnreachableReset {
			h.m.wp.NoteInvalidated(h.p)
		}
		return errnoFor(err)
	}
	hash := meta.Hash
	if hash == "" {
		hash = fsclient.HashBytes(data)
	}
	if errno := h.m.guardServed(h.p, int64(len(data)), hash); errno != 0 {
		// The bytes are fresh and verified — they are the server's answer — so
		// they are worth keeping: the retry (once the metadata is corrected)
		// then costs no round trip. They are deliberately NOT recorded as
		// served: they were never handed to a caller.
		//
		// AdmitRead is the ONE admission decision, and it COUNTS the refusal
		// (reason over_entry_cap) where the decision is made. The previous
		// `if len(data) <= MaxEntryBytes` pre-filter here is what made
		// `oversize_bypasses` unreachable from a live read (BFS-032): a
		// counter above which a filter sits is a counter the live path can
		// never move.
		h.m.cache.AdmitRead(h.p, hash, data)
		return errno
	}
	h.data, h.hash, h.loaded = data, hash, true
	h.m.wp.NoteRead(h.p, hash)
	if h.m.cache.AdmitRead(h.p, hash, data) {
		h.m.cache.Pin(hash)
		h.pinned = true
	}
	h.m.recordOK()
	return 0
}

// distrustCached reports whether the entry we hold for a path may not be served,
// and drops it when that is the case. The one disqualifying fact is the length:
// content LONGER than the size the kernel is holding would be clamped by the
// kernel mid-copy, so a reader would receive a fragment of bytes we cannot vouch
// for. Dropping the entry sends the read to the server instead — RE-FETCH, which
// is what this client does when it cannot trust what it holds.
//
// A cached entry SHORTER than the bound is served: every byte it holds reaches
// the reader (a true EOF at end-of-content), and whether those bytes are the
// server's current ones is the cache-staleness class of BFS-024/026, which this
// row does not touch.
func (h *readHandle) distrustCached(n int64, hash string) bool {
	bound, known := h.m.bounds.Bound(h.p)
	if !known || n <= bound {
		return false
	}
	h.m.noteBoundDivergence(h.p, bound, n, false)
	h.m.logf("bunker-fs: the cache entry for %s (%d bytes) is longer than the size the kernel holds (%d): it is dropped and re-fetched rather than served", h.p, n, bound)
	h.m.cache.Drop(h.p)
	return true
}

// notePublished records what an attrs reply just told the kernel about a path.
// Only a FILE is recorded: a read is bounded by a regular file's i_size, and a
// directory is never read through this handle. It is the only source of the
// bound (see bound.go), so it is called wherever fillAttr replies to the kernel.
func (m *Mount) notePublished(nd fsclient.Node) {
	if nd.IsDir {
		return
	}
	m.bounds.Published(nd.Path, nd.Size)
}

// guardServed reconciles the content a read is about to serve with the size the
// kernel is holding for that path, and it is the whole of BFS-025's fix:
//
//   - content longer than the bound: REFUSE, loudly, with ESTALE. A FUSE read is
//     clamped by the kernel at that bound, so serving these bytes would hand the
//     caller a fragment with rc=0 — the one outcome that is not allowed. The
//     snapshot's entry for the path is corrected from the server's own answer
//     first, so the retry (a re-open, which is what ESTALE tells a tool to do) is
//     answered with the true size and succeeds.
//   - content shorter than the bound: serve. Every byte the resource has reaches
//     the reader (EOF is our own end-of-content, not the kernel's clamp), but the
//     metadata we published was demonstrably wrong, so it is corrected and
//     counted rather than left to be discovered later.
//   - equal: the fast path. One comparison, no request, nothing recorded.
func (m *Mount) guardServed(p string, n int64, hash string) syscall.Errno {
	bound, known := m.bounds.Bound(p)
	switch judgeServed(bound, known, n) {
	case servedBoundTooSmall:
		m.noteBoundDivergence(p, bound, n, true)
		m.boundRefusals.Add(1)
		m.correctSnapshot(p, n, hash)
		err := &fsclient.OpError{
			Op: "READ", Path: p, Errno: fsclient.ErrnoESTALE, Cause: fsclient.CauseStaleBound,
			Detail: fmt.Sprintf("refusing to serve %d bytes where the kernel bounds this path at %d: a reader bound by that size would receive a silent fragment. The published size is stale; re-open the path (the metadata has been corrected from this content) and retry", n, bound),
		}
		m.recordFailure(err)
		return errnoFor(err)
	case servedBoundTooLarge:
		m.noteBoundDivergence(p, bound, n, false)
		m.correctSnapshot(p, n, hash)
		return 0
	default:
		return 0
	}
}

// correctSnapshot replaces the snapshot's entry for a path with what the SERVER
// just said the file is, so the next attrs reply for the path publishes a true
// size — which is what makes the refused read recoverable. It is deliberately
// not called from a cache hit: our own memory is not evidence about the server,
// which is the whole reason a cache entry is distrusted by length rather than
// trusted for a size.
func (m *Mount) correctSnapshot(p string, n int64, hash string) {
	snap := m.snapshot()
	if snap == nil {
		return
	}
	nd, ok := snap.Lookup(p)
	if !ok || nd.IsDir {
		// A path the snapshot does not hold is answered live on the next attrs
		// reply, which is the correction this exists for.
		return
	}
	nd.Size = n
	if hash != "" {
		nd.Hash = hash
	}
	snap.Put(nd)
}

// noteBoundDivergence records one read-bound divergence: the counter, the last
// one by name, and a log line. Both directions and both discovery paths go
// through here, so the reported figures and the log can never disagree.
func (m *Mount) noteBoundDivergence(p string, published, content int64, refused bool) {
	m.boundRepairs.Add(1)
	m.boundLast.Store(fmt.Sprintf("%s: published=%d content=%d refused=%v", p, published, content, refused))
	m.logf("bunker-fs: read bound divergence %s: published=%d content=%d refused=%v (the metadata the kernel holds for this path was stale)", p, published, content, refused)
}

// Flush is a publication point for the write path when the handle was opened
// for writing (POSIX close(2) semantics: the refusal is reported here). For an
// O_APPEND open it is the publication point of the buffered append (BFS-021):
// the kernel sends FLUSH before RELEASE, and the append's publication is
// idempotent, so the RELEASE that follows is answered without a second request.
func (h *readHandle) Flush(ctx context.Context) syscall.Errno {
	if h.ap != nil {
		return h.ap.publish(ctx)
	}
	h.m.mu.Lock()
	w := h.m.writes[h.fh]
	h.m.mu.Unlock()
	if w == nil {
		return 0
	}
	return w.publish(ctx)
}

// Release drops the handle: the pin goes, and with it the blob if its path entry
// was evicted while it was pinned.
//
// An O_APPEND handle publishes before anything is dropped (BFS-021): the
// write-intent record is still live while the append is in flight, so BFS-030's
// refusal protects a concurrent resize from landing mid-append, and POSIX close(2)
// gets the publication's own error rather than a silent 0 — the same contract
// writeHandle.Release has.
func (h *readHandle) Release(ctx context.Context) syscall.Errno {
	var errno syscall.Errno
	if h.ap != nil {
		errno = h.ap.publish(ctx)
	}
	h.mu.Lock()
	if h.pinned && h.hash != "" {
		h.m.cache.Unpin(h.hash)
		h.pinned = false
	}
	h.mu.Unlock()
	h.m.mu.Lock()
	delete(h.m.reads, h.fh)
	w := h.m.writes[h.fh]
	delete(h.m.writes, h.fh)
	delete(h.m.bufDocs, h.fh)
	h.m.mu.Unlock()
	if w != nil {
		w.discard()
	}
	if h.ap != nil {
		h.ap.discard()
	}
	return errno
}

// ---------------------------------------------------------------------------
// Write handles.
// ---------------------------------------------------------------------------

// writeHandle accumulates the chunk stream of one open write.
//
// THE ONE DELIBERATE DEVIATION FROM THE SPEC, stated plainly: BFS-005 §5.4 has
// the chunks stream straight into the PUT request body. That is only sound if
// chunks arrive in order — which the measured pattern does (1023 sequential
// WRITE ops, M14) but which nothing guarantees, and a non-sequential write
// cannot be fed to an already-started request body. So the handle buffers to a
// per-handle temp file under the cache directory and publishes at the first
// publication point (Flush/Fsync/Release) by STREAMING that file into ONE
// conditional PUT. What the spec's rule protects is kept: one request per file
// rather than one per chunk, one chunk of RAM rather than the file, the
// precondition evaluated on the whole body, and no partial file on the server.
// What is paid is one temp file per open write handle, bounded by
// --write-buffer-max-bytes and reported as write_buffer_bytes.
type writeHandle struct {
	mu      sync.Mutex
	m       *Mount
	p       string
	fh      uint64
	created bool
	base    fsclient.WriteBase
	hasBase bool
	size    int64
	tmp     *os.File
	flushed bool
	result  *fsclient.PutResult
	failure *fsclient.OpError
}

func (m *Mount) newWriteHandle(p string, created bool) *writeHandle {
	m.mu.Lock()
	m.nextFh++
	fh := m.nextFh
	h := &writeHandle{m: m, p: p, fh: fh, created: created}
	m.writes[fh] = h
	m.mu.Unlock()
	return h
}

// ensureBuffer opens this handle's local buffer.
//
// THE BUFFER HAS NO NAME (BFS-039), and that is the whole of its crash safety:
// the file is unlinked the moment it exists, so the open descriptor is the only
// reference to its bytes and the kernel reclaims the inode with the last
// descriptor. A writer killed mid-write therefore leaves NOTHING behind — no
// cancel message is needed, no sweep is needed, and there is no orphan for a
// figure the owner cannot see to omit (BFS-031's class). MEASURED on the tree
// that landed BFS-046: a named buffer left `writebuf-<n>` in the cache
// directory after every killed writer, and nothing ever removed it
// (docs/evidence/BFS-039-red.txt).
//
// The construction is anonymousWriteBuffer (append.go), shared with the append
// handle so there is ONE buffer rule rather than two.
func (h *writeHandle) ensureBuffer() syscall.Errno {
	if h.tmp != nil {
		return 0
	}
	f, errno := anonymousWriteBuffer(h.m.dir)
	if errno != 0 {
		return errno
	}
	h.tmp = f
	return 0
}

// noCloseReader hands the transport a READ-ONLY VIEW of the write buffer.
//
// net/http CLOSES a request body on every failed round trip ("c._send() always
// closes req.Body"), so passing the handle's own *os.File as the PUT body meant
// a CANCELLED or failed publication closed the buffer the retry needs: the
// next attempt's Seek failed and the caller was told EIO, which is precisely
// the recovery EINTR exists to make possible (BFS-039). MEASURED: the retry
// after a cancel failed 5 runs in 12 before this view existed
// (docs/evidence/BFS-039-red.txt), and the failure depended on whether the
// request had started — an intermittent byte-shaped defect, which is the worst
// kind to leave in a write path.
//
// The view is an io.Reader and NOT an io.ReadCloser, so net/http wraps it in
// io.NopCloser and its close is a no-op. Nothing else changes: the file's
// offset is reset before every attempt.
type noCloseReader struct{ r io.Reader }

func (n noCloseReader) Read(p []byte) (int, error) { return n.r.Read(p) }

func (h *writeHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failure != nil {
		return 0, syscall.EIO
	}
	// A chunk arriving AFTER a publication point REOPENS the handle for
	// publication. The kernel sends more than one publication point for one open
	// (MEASURED: a FLUSH per close(2), and a shell that writes several commands
	// to one descriptor closes it more than once), and the buffer holds the WHOLE
	// file — so without this the bytes after the first publication point would be
	// buffered and never sent, with the caller's close(2) reporting success. That
	// is the silent-loss class this row exists for, one publication point further
	// in. It is the same rule the append handle states (append.go, BFS-021).
	// A sticky failure is NOT cleared: a refusal is a verdict, not a pause.
	if h.flushed {
		h.flushed, h.result = false, nil
	}
	if errno := h.ensureBuffer(); errno != 0 {
		return 0, errno
	}
	if !h.hasBase {
		base, err := h.m.wp.ResolveBase(ctx, h.p)
		if err != nil {
			h.failure = err
			return 0, errnoFor(err)
		}
		h.base, h.hasBase = base, true
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	n, err := h.tmp.WriteAt(data, off)
	if err != nil {
		return 0, syscall.EIO
	}
	if end := off + int64(n); end > h.size {
		h.size = end
	}
	if h.size > writeBufferMaxDefault {
		// Bounded, and said so: the write path's local buffer never grows past
		// its stated bound. The file is still written; only the buffer is
		// refused, loudly, with a named cause.
		h.failure = &fsclient.OpError{Op: "WRITE", Path: h.p, Errno: fsclient.ErrnoEFBIG, Cause: fsclient.CauseLocalCapability,
			Detail: fmt.Sprintf("write handle buffer bound (%d bytes) exceeded; raise --write-buffer-max-bytes", int64(writeBufferMaxDefault))}
		return 0, syscall.EFBIG
	}
	h.m.mu.Lock()
	h.m.bufDocs[h.fh] = h.size
	h.m.mu.Unlock()
	// The kernel grows the inode's i_size to max(i_size, this end) on every write
	// it acknowledges, so a read of this path is bounded by at least these bytes
	// from now on (bound.go). Recorded on the SUCCESS path only: a refused write
	// never reached the kernel's size.
	h.m.bounds.Wrote(h.p, h.size)
	return uint32(n), 0
}

// publish performs the file's ONE conditional PUT. It is idempotent: Flush may
// be called more than once for a duplicated descriptor.
//
// TWO EVENTS, TWO ANSWERS (BFS-039, PRD §2.8/R10). A publication can end in
// three ways and they are not interchangeable:
//
//	it LANDED            → the result is kept and every later Flush returns it.
//	it was REFUSED or it
//	FAILED               → the failure is recorded and stays terminal: the
//	                       server gave a verdict, so re-offering the same bytes
//	                       is re-offering a write it already refused.
//	the CALLER cancelled → nothing was refused and no verdict was given. The
//	                       handle is left PUBLISHABLE (`flushed` stays false and
//	                       no failure is recorded), the cancel is returned as
//	                       EINTR, and the retry the errno asks for is the same
//	                       ONE conditional PUT. That retry lands exactly once
//	                       whether or not the cancelled attempt had reached the
//	                       server, because a whole-file PUT of bytes the server
//	                       already holds is its own reported no-op
//	                       (`identical_content`, nothing written, mtime
//	                       unmoved) — so a cancel can never double-apply a
//	                       write, which is the property that makes an
//	                       ACCIDENTAL cancel safe to not hear about at all.
func (h *writeHandle) publish(ctx context.Context) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.flushed {
		if h.failure != nil {
			return errnoFor(h.failure)
		}
		return 0
	}
	if h.tmp == nil && !h.created {
		h.flushed = true
		return 0 // opened for write but nothing was written
	}
	if h.failure != nil {
		h.flushed = true
		// BFS-020: a handle that never publishes must not leave the mount
		// claiming a name the server does not have. The create's snapshot entry
		// is dropped, so the next attrs reply for the path is answered LIVE and
		// says what is true (the file is not there) rather than repeating a
		// size-0 entry this mount invented.
		h.m.snapshot().Drop(h.p)
		return errnoFor(h.failure)
	}
	if !h.hasBase {
		base, err := h.m.wp.ResolveBase(ctx, h.p)
		if err != nil {
			if err.Cause == fsclient.CauseCancelled {
				// No base was resolved and nothing was published: the retry
				// re-resolves and publishes once.
				return errnoFor(err)
			}
			h.flushed, h.failure = true, err
			h.m.snapshot().Drop(h.p)
			return errnoFor(err)
		}
		h.base, h.hasBase = base, true
	}
	// THE BODY. A CREATE with NO chunk is a caller asking for an EMPTY FILE, and
	// an empty file is still a name: `: > f`, `printf '' > f` and `touch f` all
	// arrive as a create with no WRITE at all, and MEASURED on the tree this row
	// started from the name was then NEVER sent — the mount claimed it (0 bytes)
	// while the served tree never had it, so `mv` after close failed ENOENT
	// forever, not for a window (docs/evidence/BFS-020-empty.txt). Publishing a
	// zero-byte body is the whole of that repair; for a handle that was never
	// created nothing is published, exactly as before.
	var body io.Reader = strings.NewReader("")
	if h.tmp != nil {
		if _, err := h.tmp.Seek(0, io.SeekStart); err != nil {
			h.flushed = true
			return syscall.EIO
		}
		body = noCloseReader{h.tmp}
	}
	res, err := h.m.wp.Publish(ctx, h.p, body, h.size, h.base)
	if err != nil {
		if err.Cause == fsclient.CauseCancelled {
			// The caller cancelled: EINTR, and the handle stays publishable so
			// the retry is possible and lands once. The cancellation is counted
			// by the CLIENT (transport.cancels_total), which is where every
			// cancelled request is counted, rather than a second time here.
			return errnoFor(err)
		}
		h.flushed = true
		h.failure = err
		h.m.recordFailure(err)
		// A refused publication published NOTHING, so the name the create put in
		// the snapshot must go: leaving it would make the mount answer "the file
		// is here" for a file the server refused to create.
		h.m.snapshot().Drop(h.p)
		if err.Cause == fsclient.CauseConflict {
			h.m.conflictCount.Add(1)
		}
		return errnoFor(err)
	}
	h.flushed = true
	h.result = res
	// THE BASE ADVANCES WITH THE PUBLICATION. The kernel sends more than one
	// publication point for one open (a FLUSH per close, then RELEASE), and a
	// chunk after one of them reopens the handle — so a later publication carries
	// the bytes the earlier one landed plus the new ones. Keeping the base this
	// handle started from (for a create, `If-None-Match: *`) would make that
	// second publication a stale-precondition refusal and drop the later bytes:
	// the same defect BFS-021 measured one publication point into the append path
	// (docs/evidence/BFS-021-red.txt, arm `multi`). The landed hash is truth, and
	// the client has already adopted it as the path's base.
	if res.Hash != "" {
		h.base = fsclient.WriteBase{IfMatch: res.Hash, Source: fsclient.BaseFromServed}
	} else {
		// A landed publication whose result names no content identity (a surface
		// that serves without one): the base this handle holds may no longer
		// describe the server, so the NEXT publication resolves it again rather
		// than re-offering a create-only precondition against a file that now
		// exists.
		h.hasBase = false
	}
	h.m.recordOK()
	// The write landed: our metadata for the path is stale by construction, and
	// the bytes we just published are the truth — seed the cache with them so a
	// read-after-write does not pay a round trip.
	h.m.snapshot().Drop(h.p)
	h.m.snapshot().DropReaddir(path.Dir(h.p))
	if res.Hash != "" && h.tmp != nil && h.size <= cacheSeedWriteMax {
		if _, err := h.tmp.Seek(0, io.SeekStart); err == nil {
			if data, rerr := io.ReadAll(io.LimitReader(h.tmp, cacheSeedWriteMax+1)); rerr == nil {
				if _, ierr := h.m.cache.Insert(h.p, res.Hash, data); ierr == nil {
					h.m.wp.NoteServed(h.p, res.Hash)
				}
			}
		}
	}
	return 0
}

func (h *writeHandle) Fsync(ctx context.Context, flags uint32) syscall.Errno { return h.publish(ctx) }

// Flush is THE POINT THAT FIXES BFS-020, and it is a FUSE-protocol fact rather
// than a preference:
//
//	FLUSH is sent once per close(2) and the kernel WAITS for its reply, so a
//	publication made here is complete before close(2) returns to the caller.
//	RELEASE — where this handle published until now — is (measured) NOT waited
//	for: the mount's own trace shows the RELEASE handler's PUT completing 4.1 ms
//	AFTER close(2) had already returned, which is exactly the window in which a
//	following `rename` reached the server as a 404 and came back as a bare ENOENT
//	(docs/evidence/BFS-020-mechanism.txt).
//
// The publication is idempotent, so the RELEASE that follows answers from the
// recorded result with no second request. The append path has published here
// since BFS-021 for the same reason; this makes the write path obey the same
// rule: the point the caller's close(2) waits for is a publication point.
func (h *writeHandle) Flush(ctx context.Context) syscall.Errno { return h.publish(ctx) }

// setPath moves this handle's own path after a successful rename (BFS-020).
func (h *writeHandle) setPath(p string) {
	h.mu.Lock()
	h.p = p
	h.mu.Unlock()
}

func (h *writeHandle) Release(ctx context.Context) syscall.Errno {
	// POSIX close(2) ignores the error, which is exactly why a refusal is also
	// written to the mount's conflict log and surfaced in `bunker fs conflicts`.
	errno := h.publish(ctx)
	h.discard()
	return errno
}

func (h *writeHandle) discard() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tmp != nil {
		name := h.tmp.Name()
		h.tmp.Close()
		os.Remove(name)
		h.tmp = nil
	}
	h.m.mu.Lock()
	delete(h.m.bufDocs, h.fh)
	delete(h.m.writes, h.fh)
	h.m.mu.Unlock()
}

// Result reports the landed write, when there was one.
func (h *writeHandle) Result() *fsclient.PutResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.result
}

// compile-time assertions: the handles implement what the node tree relies on.
var (
	_ fs.FileReader    = (*readHandle)(nil)
	_ fs.FileFlusher   = (*readHandle)(nil)
	_ fs.FileReleaser  = (*readHandle)(nil)
	_ fs.FileWriter    = (*writeHandle)(nil)
	_ fs.FileFlusher   = (*writeHandle)(nil)
	_ fs.FileFsyncer   = (*writeHandle)(nil)
	_ fs.FileReleaser  = (*writeHandle)(nil)
	_ fs.NodeGetattrer = (*node)(nil)
	_ fs.NodeLookuper  = (*node)(nil)
	_ fs.NodeReaddirer = (*node)(nil)
	_ fs.NodeOpener    = (*node)(nil)
	_ fs.NodeCreater   = (*node)(nil)
	_ fs.NodeMkdirer   = (*node)(nil)
	_ fs.NodeUnlinker  = (*node)(nil)
	_ fs.NodeRmdirer   = (*node)(nil)
	_ fs.NodeRenamer   = (*node)(nil)
	// The three BFS-018 interfaces. Their ABSENCE was the client's half of the
	// defect: with no NodeSymlinker the kernel's symlink(2) reached go-fuse's
	// bridge, which answers ENOTSUP; with no NodeReadlinker a `readlink` did
	// too, so a link could only ever be described as a file.
	_ fs.NodeSymlinker  = (*node)(nil)
	_ fs.NodeReadlinker = (*node)(nil)
	_ fs.NodeLinker     = (*node)(nil)
	_ fs.NodeSetattrer  = (*node)(nil)
	_ fs.NodeStatfser   = (*node)(nil)
	_ fs.NodeWriter     = (*node)(nil)
)
