// Fixture A for the per-protocol WebDAV battery.
//
// The layout is the one the whole study uses (`docs/prd/PRD-bunker-fs.md:126`:
// "Fixture A (150 files / 6 dirs)"), extended with exactly what the battery's
// op list needs to be measurable at all: a 4 MiB file for the large read and
// write cells (the measured write granularity in the study is a 4 MiB file,
// BFS-003 Appendix A.6), a nested chain for the deep-listing cell, an empty
// collection as the parent for the create/mkdir/rename/delete cells, and a
// dedicated conflict file for the stale-base refusal.
//
// CONTENT IS DETERMINISTIC. Every byte is derived from the file's own path and
// an offset, so regenerating the fixture between arms produces byte-identical
// files with identical hashes. That is what makes two arms comparable: the
// battery asserts the server's ETag/X-Bunker-Hash against a hash it computes
// from the file it just wrote, and a fixture generated from /dev/urandom would
// make every arm a different experiment.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Fixture names. They are constants because the battery's op list refers to
// them by name and a typo must be a compile error, not a 404 cell.
const (
	fixRootFile     = "README.md"
	fixSmallFile    = "small.txt"
	fixBigFile      = "big.bin"
	fixConflictFile = "conflict.txt"
	fixScratchDir   = "scratch"
	fixNestedDir    = "d0/nested/deeper"
	fixSmallBytes   = 1024
	fixNestedBytes  = 512
)

// fixtureInfo is what the battery needs to know about the tree it is measuring:
// the file and directory lists it expects the server to reveal, and the sizes
// its read cells must transfer. Everything is derived by walking the local
// fixture root — the battery runs on the same host as the served tree, and the
// alternative (asserting a hard-coded count) would pass vacuously the day the
// generator changes.
type fixtureInfo struct {
	Root       string
	Files      []string // slash-separated, relative to the served root
	Dirs       []string // slash-separated, relative to the served root ("" omitted from Files)
	FilesInDir map[string][]string
	SubDirs    map[string][]string
	BigBytes   int64
}

// deterministicBytes returns n bytes derived from seed. It is a hash chain, so
// the same seed always yields the same bytes on every run and on every host.
func deterministicBytes(seed string, n int) []byte {
	out := make([]byte, 0, n)
	for counter := 0; len(out) < n; counter++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", seed, counter)))
		out = append(out, sum[:]...)
	}
	return out[:n]
}

// writeFile writes deterministic content of size bytes under root/rel.
func writeFile(root, rel string, size int) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, deterministicBytes(rel, size), 0o644)
}

// generateFixture builds Fixture A under root: `dirs` collections of `perDir`
// files each, a deep chain, an empty scratch collection, and the four root
// files. It returns the full expectation set.
//
// A pre-existing root is emptied first, so arm N+1 starts from the same tree as
// arm N — the property that makes the per-arm numbers comparable at all.
func generateFixture(root string, dirs, perDir int, bigBytes int64) (*fixtureInfo, error) {
	if err := os.RemoveAll(root); err != nil {
		return nil, fmt.Errorf("clear fixture root: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}

	info := &fixtureInfo{
		Root:       root,
		FilesInDir: map[string][]string{},
		SubDirs:    map[string][]string{},
		BigBytes:   bigBytes,
	}

	// Root files. big.bin is written from the same deterministic chain, so its
	// hash is reproducible without reading it back.
	for _, f := range []struct {
		name string
		size int
	}{
		{fixRootFile, fixSmallBytes},
		{fixSmallFile, fixSmallBytes},
		{fixConflictFile, fixSmallBytes},
	} {
		if err := writeFile(root, f.name, f.size); err != nil {
			return nil, err
		}
		info.FilesInDir[""] = append(info.FilesInDir[""], f.name)
	}
	if err := writeFile(root, fixBigFile, int(bigBytes)); err != nil {
		return nil, err
	}
	info.FilesInDir[""] = append(info.FilesInDir[""], fixBigFile)

	// Fixture A: 6 collections x 25 files.
	for d := 0; d < dirs; d++ {
		dir := fmt.Sprintf("d%d", d)
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return nil, err
		}
		info.SubDirs[""] = append(info.SubDirs[""], dir)
		for f := 0; f < perDir; f++ {
			name := fmt.Sprintf("f%02d.txt", f)
			if err := writeFile(root, dir+"/"+name, fixSmallBytes); err != nil {
				return nil, err
			}
			info.FilesInDir[dir] = append(info.FilesInDir[dir], name)
		}
	}

	// The deep chain: d0, then d0/nested, then d0/nested/deeper.
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(fixNestedDir)), 0o755); err != nil {
		return nil, err
	}
	info.SubDirs["d0"] = append(info.SubDirs["d0"], "nested")
	info.SubDirs["d0/nested"] = append(info.SubDirs["d0/nested"], "deeper")
	for f := 0; f < 3; f++ {
		name := fmt.Sprintf("n%02d.txt", f)
		if err := writeFile(root, fixNestedDir+"/"+name, fixNestedBytes); err != nil {
			return nil, err
		}
		info.FilesInDir[fixNestedDir] = append(info.FilesInDir[fixNestedDir], name)
	}

	// The empty collection the write-side cells use as a parent.
	if err := os.MkdirAll(filepath.Join(root, fixScratchDir), 0o755); err != nil {
		return nil, err
	}
	info.SubDirs[""] = append(info.SubDirs[""], fixScratchDir)

	// Flatten, sorting so the report and the walk comparison are stable.
	for dir, files := range info.FilesInDir {
		for _, f := range files {
			rel := f
			if dir != "" {
				rel = dir + "/" + f
			}
			info.Files = append(info.Files, rel)
		}
	}
	sort.Strings(info.Files)
	for _, sub := range info.SubDirs {
		sort.Strings(sub)
	}

	// Directories as a walk would discover them: the root (""), every dir the
	// fixture creates (including an EMPTY collection — a directory with no files
	// and no sub-collections is still a directory the server must reveal, and
	// leaving it out of the expectation set is how a listing assertion goes
	// vacuous), plus the intermediate collections of the deep chain.
	seen := map[string]bool{"": true}
	for rel := range info.FilesInDir {
		seen[rel] = true
	}
	for parent, subs := range info.SubDirs {
		seen[parent] = true
		for _, s := range subs {
			if parent == "" {
				seen[s] = true
				continue
			}
			seen[parent+"/"+s] = true
		}
	}
	for _, f := range info.Files {
		for d := filepath.ToSlash(filepath.Dir(f)); d != "." && d != ""; d = filepath.ToSlash(filepath.Dir(d)) {
			seen[d] = true
		}
	}
	for d := range seen {
		info.Dirs = append(info.Dirs, d)
	}
	sort.Strings(info.Dirs)

	return info, nil
}

// sha256OfFile hashes a file on disk — the battery's own instrument for the
// byte-identity and content assertions (the AC-3 proof reads the served tree
// directly, which is legitimate here because the battery and the daemon run on
// the same host against the same directory).
func sha256OfFile(abs string) (string, error) {
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// sha256OfBytes hashes a buffer, in the same `sha256:<hex>` form the surface's
// ETag/X-Bunker-Hash use, so expectations compare without string surgery.
func sha256OfBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// dirOf is the collection a fixture-relative file lives in ("" for the root).
func dirOf(rel string) string {
	d := filepath.ToSlash(filepath.Dir(rel))
	if d == "." || d == "/" {
		return ""
	}
	return strings.TrimPrefix(d, "/")
}
