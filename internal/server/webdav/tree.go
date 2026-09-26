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
	// tempPrefix marks the atomic-write staging files (see writeAtomicFile).
	// Names carrying it are filtered out of directory listings so a partial
	// file is never visible, exactly as AC-6 requires.
	tempPrefix = ".davtmp-"
)

type hashEntry struct {
	size  int64
	mtime int64
	hash  string
}

// tree is the served directory: path confinement, content identity, the
// tree-level tokens and the metadata-keyed hash cache.
type tree struct {
	root     string
	rootReal string

	counter atomic.Uint64

	mu    sync.Mutex
	cache map[string]hashEntry

	revMu  sync.Mutex
	revVal string
	revAt  time.Time
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
// metadata-keyed cache when size and mtime are unchanged.
func (t *tree) hashFile(abs string) (string, error) {
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", abs)
	}
	size, mtime := fi.Size(), fi.ModTime().UnixNano()

	t.mu.Lock()
	e, ok := t.cache[abs]
	t.mu.Unlock()
	if ok && e.size == size && e.mtime == mtime {
		return e.hash, nil
	}

	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	hs := sha256.New()
	if _, err := io.Copy(hs, f); err != nil {
		return "", err
	}
	h := "sha256:" + hex.EncodeToString(hs.Sum(nil))

	t.mu.Lock()
	if len(t.cache) >= hashCacheLimit {
		t.cache = make(map[string]hashEntry)
	}
	t.cache[abs] = hashEntry{size: size, mtime: mtime, hash: h}
	t.mu.Unlock()
	return h, nil
}

// forget drops a path from the hash cache after a mutation writes it.
func (t *tree) forget(abs string) {
	t.mu.Lock()
	delete(t.cache, abs)
	t.mu.Unlock()
}

// bumpRev advances the non-git tree revision counter and invalidates the
// cached git revision, so a write is visible to the very next response.
func (t *tree) bumpRev() {
	t.counter.Add(1)
	t.revMu.Lock()
	t.revAt = time.Time{}
	t.revMu.Unlock()
}

// revToken implements E-3's X-Bunker-Rev. For a git tree it is the resolved
// HEAD commit hash ("git:<40 hex>"); otherwise the server-maintained
// monotonic counter (O-9), bumped by every mutation this surface performs.
func (t *tree) revToken() string {
	if r := t.cachedGitHead(); r != "" {
		return "git:" + r
	}
	return fmt.Sprintf("rev:%d", t.counter.Load())
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

// writeAtomicFile writes body to abs through a temp file + rename in the same
// directory, so a failed or killed request leaves the previous bytes intact
// and no partial file is ever visible (AC-6, §9.7).
func writeAtomicFile(abs string, body []byte) error {
	dir := filepath.Dir(abs)
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(abs); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once the rename succeeded
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, abs)
}

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
