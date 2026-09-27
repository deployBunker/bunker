package fsclient

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// BFS-031: the mount directory, and the two bounded things inside it.
//
// THE DEFECT. `--cache-max-size` names the cache directory and is enforced
// against `used_bytes`, which counts the published blobs plus the serialised
// index. The directory the bound NAMES held two more files — status.json (the
// mount's own document, rewritten on a 1 s cadence) and conflicts.jsonl (the
// refusal log, appended once per refused write) — so at a deliberately tiny
// 1 KiB bound the directory reached 30,689 B = 29.97x the bound while the
// client's own figures stayed "inside" (docs/evidence/BFS-012-smallbound-edge.txt).
// A bound reported one way and enforced another is not a bound: the number the
// owner could see described something other than the thing it claimed to bound.
//
// THE DECISION, AND WHY IT IS THIS ONE. Two files cannot be brought under a
// 1 KiB bound and still be the observability surface the owner audits:
//
//   - status.json is a ~4 KiB document with a fixed schema (measured: 7,243 B on
//     the live route while this row was written). It cannot be squeezed under a
//     1 KiB budget without either refusing to describe the mount or writing a
//     document that omits the figures the owner asked for;
//   - conflicts.jsonl grows with EDIT VOLUME, not with tree size, so it has no
//     size of its own to fit — only a bound.
//
// So the two are NOT deleted and NOT squeezed: they are given a directory of
// their own, and their own declared, enforced bound. The directory the cache
// bound names holds the cache, and nothing else:
//
//	<mount dir>/                  the mount's own directory (status.json,
//	                              conflicts.jsonl, the write-buffer spills) —
//	                              bounded by StateMaxBytes, see state.go
//	<mount dir>/cache/            THE CACHE DIRECTORY: index.json and blobs/.
//	                              `--cache-max-size` bounds this directory in
//	                              full, and `du` of it is ≤ the bound at all
//	                              times (the flush temp copy of the index is
//	                              reserved, see dirPeakLocked).
//
// Every byte the client writes is inside one of the two reported bounds, and
// both are named in the status record (`cache.dir_bytes` / `cache.max_bytes`
// and `state.bytes` / `state.max_bytes`) with the footprint's own figure beside
// them. Nothing is excluded by omission: a byte is either in the cache's bound,
// in the state's bound, or in the write path's per-handle buffer bound, and the
// record says which.
// ---------------------------------------------------------------------------

// CacheSubdir is the name of the cache directory inside a mount directory: the
// one directory `--cache-max-size` bounds.
const CacheSubdir = "cache"

// MountCacheDir returns the directory the cache bound names, for one mount
// directory. The mount directory is the one `MountDir`/`--cache-dir` resolves;
// the cache lives one level down so that the bound can be a bound on a
// directory that holds only cache bytes.
func MountCacheDir(mountDir string) string {
	return filepath.Join(mountDir, CacheSubdir)
}

// LayoutMigration is what MigrateMountLayout did, so the mount can say it in
// its log rather than migrating silently.
type LayoutMigration struct {
	// Moved is true when a pre-BFS-031 cache was found in the mount directory.
	Moved bool
	// Bytes is how many bytes the legacy cache held (index + blobs + temps).
	Bytes int64
	// Files is how many files moved into the cache directory.
	Files int
	// TempsRemoved counts stale index temps that were removed rather than
	// moved: they are half-written files of a format this client owns, and a
	// stale one is not a document anyone can read.
	TempsRemoved int
}

// MigrateMountLayout moves a pre-BFS-031 cache — `index.json` and `blobs/`
// sitting directly in the mount directory, beside the mount's own state — into
// the cache directory BFS-031 defines.
//
// It MIGRATES rather than discards: the bytes are the owner's cache, the
// rename is atomic per entry, and a cache that is thrown away on upgrade would
// be a silent cost the owner did not ask for. Renames are used because the two
// paths are on one filesystem by construction (a subdirectory of the same
// directory), so this is the cheap, all-or-nothing move.
//
// One-time by construction: after it runs the legacy paths do not exist, and
// the only reason to call it again is a downgrade-then-upgrade.
func MigrateMountLayout(mountDir string) (LayoutMigration, error) {
	var m LayoutMigration
	if mountDir == "" {
		return m, nil
	}
	legacyIndex := filepath.Join(mountDir, CacheIndexFile)
	legacyBlobs := filepath.Join(mountDir, CacheBlobDir)
	_, indexErr := os.Stat(legacyIndex)
	_, blobsErr := os.Stat(legacyBlobs)
	if os.IsNotExist(indexErr) && os.IsNotExist(blobsErr) {
		return m, nil
	}
	m.Moved = true
	cacheDir := MountCacheDir(mountDir)
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return m, fmt.Errorf("fsclient: create cache dir %s: %w", cacheDir, err)
	}
	// The index is only moved when the new location has none: two indexes
	// merged by hand would resurrect entries whose blobs the live index has
	// already dropped. When both exist, the legacy one is left alone and the
	// mount reports it — a leftover is visible, never silently eaten.
	if indexErr == nil {
		if _, err := os.Stat(filepath.Join(cacheDir, CacheIndexFile)); os.IsNotExist(err) {
			info, serr := os.Stat(legacyIndex)
			if serr == nil {
				m.Bytes += info.Size()
			}
			if err := os.Rename(legacyIndex, filepath.Join(cacheDir, CacheIndexFile)); err != nil {
				return m, fmt.Errorf("fsclient: migrate cache index: %w", err)
			}
			m.Files++
		}
	}
	if blobsErr == nil {
		if _, err := os.Stat(filepath.Join(cacheDir, CacheBlobDir)); os.IsNotExist(err) {
			if n, b := dirBytesOf(legacyBlobs); b == nil {
				m.Bytes += n
			}
			if err := os.Rename(legacyBlobs, filepath.Join(cacheDir, CacheBlobDir)); err != nil {
				return m, fmt.Errorf("fsclient: migrate blob directory: %w", err)
			}
			m.Files++
		}
	}
	// The index's own temp file, when a previous run died mid-write. It is a
	// half-written index of the cache's format, so it is removed rather than
	// moved: leaving it would put a second (unreadable) index document in the
	// directory the bound measures.
	if names, err := os.ReadDir(mountDir); err == nil {
		for _, e := range names {
			if e.IsDir() || !strings.HasPrefix(e.Name(), CacheIndexFile+".") {
				continue
			}
			if info, serr := e.Info(); serr == nil {
				m.Bytes += info.Size()
			}
			if err := os.Remove(filepath.Join(mountDir, e.Name())); err == nil {
				m.TempsRemoved++
			}
		}
	}
	return m, nil
}
