package webdav

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errEscape is §6.1 step 1 of the spec: a request path that resolves outside
// the served root, or through a symlink that leaves it, is refused before any
// other step runs. It is never reported as "not found" — a caller must not be
// able to distinguish "outside the tree" from "absent inside it".
var errEscape = errors.New("path escapes the served tree")

const (
	// hashCacheLimit bounds the server-side hash cache (§7.3/§7.4). The cache
	// decides only whether a hash is RECOMPUTED; it never decides whether a
	// write is safe — a metadata mismatch always falls back to hashing.
	hashCacheLimit = 4096
	// gitRevCacheTTL bounds how often a git tree's HEAD is re-read to build
	// X-Bunker-Rev. Mutations clear the cache immediately.
	gitRevCacheTTL = 500 * time.Millisecond
	// tempPrefix marks the atomic-write staging files (see stageBody).
	// Names carrying it are filtered out of directory listings so a partial
	// file is never visible, exactly as AC-6 requires.
	tempPrefix = ".davtmp-"
	// commitStripes is the width of the per-path commit lock (§6.1 step 5).
	commitStripes = 64
)

// hashEntry is one cache entry: the observed IDENTITY of the path at the moment
// its bytes were hashed, and the hash of those bytes.
//
// The identity is `identity` — the same type, produced by the same function
// (`identityOf`, events.go) that the event ledger observes paths with, and the
// cache decides "unchanged" with the SAME predicate the rest of the surface
// derives from it (`sameContentKey`). That is deliberate and it is the whole
// point of BFS-049: the cache and the ledger are two metadata-keyed observers
// of one tree, and while they keyed on different tuples they disagreed about a
// same-size, mtime-restored edit — the ledger reported the path as moved and
// the cache answered "unchanged", so a GET could answer 304 with the ETag of
// bytes that were no longer there (SPEC-watcher-capability R-V6).
//
// A second copy of the tuple is how that divergence recurs, so there is not
// one: ctime is not "added to the cache", the cache adopts the identity the
// ledger already observed.
type hashEntry struct {
	id   identity
	hash string
	// verified is when the bytes were read. It is what lets this memo be taken
	// on metadata alone: the memo is only valid while an edit cannot have
	// changed the bytes without moving the metadata, and that is exactly the
	// property coarseClockAmbiguous decides (QA-BUNKER-36). A zero value is
	// never trusted — an entry with no read behind it cannot vouch for anything.
	verified time.Time
}

// tree is the served directory: path confinement, content identity, the
// tree-level tokens and the metadata-keyed hash cache.
type tree struct {
	root     string
	rootReal string

	counter atomic.Uint64

	// watched counts the changes the SERVER-SIDE WATCHER (watch.go, BFS-035) has
	// vouched for, and watchLive says whether a complete watch set is established
	// over this tree. Together they are how the served revision moves for a change
	// nobody made through this surface (SPEC-watcher-capability §7.1 D1/D2, §7.2
	// R-V1) without paying a stat per read (R-V2).
	watched   atomic.Uint64
	watchLive atomic.Bool

	mu    sync.Mutex
	cache map[string]hashEntry

	// commit holds the per-path critical sections of §6.1 step 5: the
	// commit-time re-validation and the rename that follows it run under one
	// of these, so two conditional writes racing on one path cannot both
	// re-validate a base and then overwrite each other. Striped rather than
	// keyed so the set is bounded (no map to grow unboundedly on a tree with
	// many paths).
	commit [commitStripes]sync.Mutex

	revMu  sync.Mutex
	revVal string
	revAt  time.Time

	// evMu/ev hold E-6's poll-form ledger (events.go): what this process has
	// observed of the served tree and the events it owes a client that polls.
	// It lives here rather than in the Handler because it is per-TREE state —
	// the tree is what a client's cursor refers to — and because it is built
	// lazily: a build or a client that never polls never pays for it.
	evMu sync.Mutex
	ev   *eventLog

	// eventMaxBytes is the declared byte bound on ONE serialized event frame
	// (server.invalidation.push.max_event_bytes), set from the resolved
	// invalidation surface when the surface is built (handler.go, New). Zero
	// means the tree was built without a surface — a cell, an embedder — and
	// eventFrameBound resolves that to the DECLARED default rather than to "no
	// bound": a bound a missing field can switch off is not a bound.
	eventMaxBytes int64

	// pushMu/push hold the push channel's subscriber registry (push.go, BFS-036),
	// built lazily on the first attach: a tree nobody subscribes to pays nothing,
	// and the hub is per-TREE because the cursor is (one ledger, one counter, one
	// cadence — §3.4 R-5). pushCfg carries the resolved bounds and cadences the
	// channel obeys, from the same surface the watcher obeys.
	pushMu  sync.Mutex
	push    *pushHub
	pushCfg pushSettings
}

func newTree(root string) (*tree, error) {
	if root == "" {
		return nil, errors.New("webdav: root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("webdav root: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("webdav root %s is not a directory", abs)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("webdav root: %w", err)
	}
	return &tree{root: abs, rootReal: real, cache: make(map[string]hashEntry)}, nil
}

func (t *tree) rootPath() string { return t.root }

// resolve maps a request URL path onto an absolute path inside the served
// root. Both a lexical check (`..` cannot climb out) and a symlink check (the
// deepest existing ancestor must resolve inside the root) run before the
// caller may touch the filesystem.
func (t *tree) resolve(urlPath string) (string, error) {
	rel := strings.TrimPrefix(urlPath, Prefix)
	if rel == "" {
		rel = "/"
	}
	clean := path.Clean("/" + rel)
	if strings.ContainsRune(clean, '\x00') {
		return "", errEscape
	}
	abs := filepath.Join(t.root, filepath.FromSlash(clean))
	r, err := filepath.Rel(t.root, abs)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
		return "", errEscape
	}
	if err := t.confineSymlinks(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// confineSymlinks walks up from abs to the deepest path component that
// exists, resolves it, and refuses when the resolved target is outside the
// served root. A path whose ancestors are all inside the root is accepted
// even when the leaf itself does not exist yet (MKCOL/PUT create it).
func (t *tree) confineSymlinks(abs string) error {
	p := abs
	for {
		if _, err := os.Lstat(p); err == nil {
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				// The component exists but cannot be resolved: refuse
				// rather than guess (fail closed).
				return errEscape
			}
			r, err := filepath.Rel(t.rootReal, real)
			if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
				return errEscape
			}
			return nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return nil
		}
		p = parent
	}
}

// hashFile returns "sha256:<64 hex>" for the file's bytes, reusing the
// metadata-keyed cache when the observed identity is unchanged.
//
// "Unchanged" is the ledger's own definition of unchanged (identityOf +
// sameContentKey, events.go), not a private (size, mtime) pair: BFS-049.
//
// The sharper identity costs no syscall. ctime is one field of the same
// `syscall.Stat_t` the `os.Stat` below already returns — the identity is read
// out of the FileInfo that stat produced, so a cache lookup is still ONE stat
// and the added work per call is reading two more fields and comparing one more
// int64. What it buys is that a same-size, mtime-restored rewrite is a MISS
// rather than a stale ETag: the class the ledger has always reported and this
// cache used to call current.
func (t *tree) hashFile(abs string) (string, error) {
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", abs)
	}
	id := identityOf(fi)

	t.mu.Lock()
	e, ok := t.cache[abs]
	t.mu.Unlock()
	// A cache HIT is a metadata-only judgement, and it is taken only when the
	// metadata can carry it: the same content key AND a read recent enough that
	// no edit could have changed the bytes under a standing metadata
	// observation. Inside the coarse-clock window the memo is re-derived from
	// the bytes, because on a CONFIG_HZ<=250 kernel a same-size rewrite that
	// restores the mtime is stamped identically to the write before it — the
	// class BFS-049 closed between this cache and the event ledger, closed here
	// for the same reason and by the same window.
	if ok && e.id.sameContentKey(id) && !coarseClockAmbiguous(e.verified, id.Ctime) {
		return e.hash, nil
	}

	h, err := contentDigest(abs)
	if err != nil {
		return "", err
	}

	t.mu.Lock()
	if len(t.cache) >= hashCacheLimit {
		t.cache = make(map[string]hashEntry)
	}
	t.cache[abs] = hashEntry{id: id, hash: h, verified: time.Now()}
	t.mu.Unlock()
	return h, nil
}

// contentDigest returns "sha256:<64 hex>" for the file's bytes.
//
// It streams (io.Copy) and never buffers the file: its callers are poll and read
// paths that may be pointed at arbitrarily large files, and a digest is not a
// reason to hold one in memory.
func contentDigest(abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	hs := sha256.New()
	if _, err := io.Copy(hs, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hs.Sum(nil)), nil
}

// forget drops a path from the hash cache after a mutation writes it.
func (t *tree) forget(abs string) {
	t.mu.Lock()
	delete(t.cache, abs)
	t.mu.Unlock()
}

// freshEntry reads the file's bytes as they are on disk RIGHT NOW and returns
// their hash with the size/mtime of the stat it read them from, bypassing the
// metadata-keyed cache entirely.
//
// §7.3 lets that cache decide only whether the server RECOMPUTES a hash; it
// must never decide whether a write is safe. The commit-time re-validation of
// §6.1 step 5 is a safety decision, so it reads bytes — a writer that changed
// the bytes without moving size and mtime (a restored mtime, an in-place
// same-size rewrite) is invisible to the cache and must not be invisible to
// the commit.
func (t *tree) freshEntry(abs string) (hashEntry, error) {
	fi, err := os.Stat(abs)
	if err != nil {
		return hashEntry{}, err
	}
	if !fi.Mode().IsRegular() {
		return hashEntry{}, fmt.Errorf("%s is not a regular file", abs)
	}
	f, err := os.Open(abs)
	if err != nil {
		return hashEntry{}, err
	}
	defer func() { _ = f.Close() }()
	hs := sha256.New()
	if _, err := io.Copy(hs, f); err != nil {
		return hashEntry{}, err
	}
	return hashEntry{
		id:   identityOf(fi),
		hash: "sha256:" + hex.EncodeToString(hs.Sum(nil)),
		// The read is authoritative AND dated: a memo taken from these bytes may
		// only be reused once the metadata can vouch for them again.
		verified: time.Now(),
	}, nil
}

// remember records an entry read from the bytes. It is used by the
// commit-time re-validation, whose read is authoritative: leaving the
// (size, mtime)-keyed entry that just proved itself insufficient behind would
// make every later read of that path repeat the mistake.
func (t *tree) remember(abs string, e hashEntry) {
	t.mu.Lock()
	if len(t.cache) >= hashCacheLimit {
		t.cache = make(map[string]hashEntry)
	}
	t.cache[abs] = e
	t.mu.Unlock()
}

// lockPath enters §6.1 step 5's critical section for abs — the commit-time
// re-validation and the rename happen under it, so a second conditional write
// to the same path cannot re-validate the base the first one is about to
// replace. It returns the release function; callers defer it.
func (t *tree) lockPath(abs string) func() {
	m := &t.commit[fnv32a(abs)%commitStripes]
	m.Lock()
	return m.Unlock
}

// fnv32a is the stripe selector. It is only a hash: two different paths may
// share a stripe, which costs contention and never correctness.
func fnv32a(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// bumpRev advances the non-git tree revision counter and invalidates the
// cached git revision, so a write is visible to the very next response.
//
// Scope (BFS-048): on a non-git tree this makes the served revision move for
// every mutation this surface performs. On a git tree the served token is the
// resolved HEAD ("git:<40 hex>", revToken below), so an out-of-band or even
// an in-band uncommitted write moves nothing the client's revision poll can
// see — only a commit/checkout/reset moves that ref. That is the token's
// declared kind (extensions.rev.kind = "git"), not a defect to paper over
// here: per SPEC-watcher-capability §7 R-V2 the revision must never be
// advanced by a stat-per-read walk, and aligning it with out-of-band change
// is the watcher's job (BFS-035, R-V1).
func (t *tree) bumpRev() {
	t.counter.Add(1)
	t.revMu.Lock()
	t.revAt = time.Time{}
	t.revMu.Unlock()
}

// revToken implements E-3's X-Bunker-Rev. For a git tree it is the resolved
// HEAD commit hash ("git:<40 hex>"); otherwise the server-maintained monotonic
// counter (O-9), bumped by every mutation this surface performs.
//
// The kinds promise different things: "counter" moves on any mutation through
// this surface; "git" moves only when HEAD's ref moves, so an uncommitted
// working-tree edit moves neither token (BFS-048). The kind in force is declared
// by the capability document (extensions.rev.kind, ops.go).
//
// BFS-035 EXTENDS THE TOKEN, and only while a watcher is established: the
// watcher's vouched-change count is appended (`git:<head>@<n>`,
// `rev:<n>@<m>`), which is how an out-of-band edit moves the served revision for
// the first time (§7.1 D1/D2, R-V1). R-V2 is what forbids the alternative — a
// stat-per-read walk — so the move comes from the watcher's own event and costs
// two atomic loads. R-V3 requires a composite token to be declared under its own
// kind: revKind reports `git+watch` / `counter+watch` for exactly these, so a
// client is never left to infer the coverage from the token's shape. With no
// watcher established the token and its kind are byte-identical to today's.
func (t *tree) revToken() string {
	live := t.watchLive.Load()
	if r := t.cachedGitHead(); r != "" {
		if live {
			return fmt.Sprintf("git:%s@%d", r, t.watched.Load())
		}
		return "git:" + r
	}
	if live {
		return fmt.Sprintf("rev:%d@%d", t.counter.Load(), t.watched.Load())
	}
	return fmt.Sprintf("rev:%d", t.counter.Load())
}

// revKindBase is the un-extended kind, read from the tree itself: the watcher
// suffix below is the only thing that can extend it.
func (t *tree) revKindBase() string {
	if gitHead(t.rootPath()) != "" {
		return "git"
	}
	return "counter"
}

// revKind declares which class of change the token in force moves for. The
// `+watch` suffix is the composite's own kind value (§7.2 R-V3): a client that
// does not know it reports no coverage claimed rather than guessing (fail-closed,
// R-V4), which is the honest answer for a value it was not built to read.
func (t *tree) revKind() string {
	if t.watchLive.Load() {
		return t.revKindBase() + "+watch"
	}
	return t.revKindBase()
}

func (t *tree) cachedGitHead() string {
	t.revMu.Lock()
	defer t.revMu.Unlock()
	if time.Since(t.revAt) < gitRevCacheTTL {
		return t.revVal
	}
	t.revVal = gitHead(t.root)
	t.revAt = time.Now()
	return t.revVal
}

// identity implements E-3's X-Bunker-Tree. The token is derived from the
// served root's path plus the device/inode of the directory itself, so an
// agent (or a tree) destroyed and re-created under the same name binds to a
// DIFFERENT token — the server half of AC-7. Clients treat it as opaque.
func (t *tree) identity() string {
	dev, ino := rootIdentityParts(t.root)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", t.root, dev, ino)))
	return "tree:" + hex.EncodeToString(sum[:])[:16]
}

// gitHead resolves the repository HEAD without executing git: .git/HEAD, the
// ref it names (loose or packed), or a raw hash. An empty string means "not a
// git work tree", which is not an error — O-9 says a non-git tree still
// exposes a revision.
func gitHead(root string) string {
	gitDir, err := findGitDir(root)
	if err != nil {
		return ""
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(head))
	if !strings.HasPrefix(ref, "ref:") {
		return validHash(ref)
	}
	refName := strings.TrimSpace(strings.TrimPrefix(ref, "ref:"))

	// A linked worktree keeps HEAD in .git/worktrees/<name> while refs live
	// in the main repository. Resolve `commondir` when it is present.
	candidates := []string{gitDir}
	if common, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		rel := strings.TrimSpace(string(common))
		if !filepath.IsAbs(rel) {
			rel = filepath.Join(gitDir, rel)
		}
		candidates = append(candidates, filepath.Clean(rel))
	}
	for _, dir := range candidates {
		if v := validHash(readTrimmed(filepath.Join(dir, filepath.FromSlash(refName)))); v != "" {
			return v
		}
	}
	// Packed refs: the last matching line wins, as git writes later entries
	// over earlier ones.
	for _, dir := range candidates {
		if v := packedRef(filepath.Join(dir, "packed-refs"), refName); v != "" {
			return v
		}
	}
	return ""
}

func findGitDir(root string) (string, error) {
	dir := root
	for {
		p := filepath.Join(dir, ".git")
		if fi, err := os.Stat(p); err == nil {
			if fi.IsDir() {
				return p, nil
			}
			// A file form: "gitdir: <path>".
			b, err := os.ReadFile(p)
			if err != nil {
				return "", err
			}
			line := strings.TrimSpace(string(b))
			if !strings.HasPrefix(line, "gitdir:") {
				return "", errors.New("malformed .git file")
			}
			target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
			if !filepath.IsAbs(target) {
				target = filepath.Join(dir, target)
			}
			return filepath.Clean(target), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

func validHash(s string) string {
	s = strings.TrimSpace(s)
	if len(s) != 40 {
		return ""
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return s
}

func readTrimmed(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func packedRef(p, refName string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	found := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == refName {
			found = fields[0]
		}
	}
	return validHash(found)
}

// hashBytes is the content identity of an in-memory body (E-1: the hash is of
// the entity bytes, never of metadata).
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// isTempName reports whether a directory entry is one of this surface's
// in-flight staging files.
func isTempName(name string) bool { return strings.HasPrefix(name, tempPrefix) }

// stageBody writes body into a fresh temp file that is a SIBLING of abs (same
// filesystem, so the rename is atomic) and returns its name. A failed or
// killed request therefore leaves the previous bytes intact and no partial
// file is ever visible (AC-6, §9.7): the name carries tempPrefix, which every
// listing, GET, PROPFIND and snapshot answer filters out.
//
// The caller owns the returned file: it is either renamed into place by
// commitStaged or removed.
func stageBody(abs string, body []byte) (string, error) {
	dir := filepath.Dir(abs)
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(abs); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	fail := func(err error) (string, error) {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", err
	}
	if _, err := tmp.Write(body); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// stageLink creates a fresh symlink holding target at a temp name that is a
// SIBLING of abs, and returns that name (BFS-018). It is stageBody's twin for
// the one directory-entry kind whose entity is not written bytes: the link is
// created under tempPrefix — which every listing, GET, PROPFIND and snapshot
// answer already filters out — and the caller publishes it with the SAME
// rename that publishes a staged body. One publication point for both shapes.
//
// The temp entry is created with symlink(2), never by writing the target
// string into a file: an implementation that wrote first and linked second
// would have to materialise the target path as content, which is exactly the
// wrong answer this row exists to remove.
func stageLink(abs, target string) (string, error) {
	dir := filepath.Dir(abs)
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	// CreateTemp made a regular file, and symlink(2) will not take a name that
	// already exists: the placeholder goes before the link is created.
	if err := os.Remove(name); err != nil {
		return "", err
	}
	if err := os.Symlink(target, name); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// commitStaged publishes a staged file over abs. It is the last step of
// §6.1 step 5's commit and runs under the path's commit lock (see lockPath),
// so the re-validation that precedes it and the rename itself are one
// critical section.
func commitStaged(tmp, abs string) error { return os.Rename(tmp, abs) }

// copyTree copies src to dst. depthInfinity=false copies a collection without
// its members (RFC 4918 §9.8.3's Depth: 0), which is the only non-infinite
// depth this surface accepts.
func copyTree(src, dst string, depthInfinity bool) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case fi.IsDir():
		if err := os.Mkdir(dst, fi.Mode().Perm()); err != nil {
			return err
		}
		if !depthInfinity {
			return nil
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), true); err != nil {
				return err
			}
		}
		return nil
	default:
		return copyFile(src, dst, fi.Mode().Perm())
	}
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
