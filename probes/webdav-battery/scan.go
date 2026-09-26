// scanFixture reads the served tree off disk and builds the expectation set the
// battery asserts against.
//
// It scans rather than trusting the generator on purpose: the cells must compare
// what the SERVER reveals with what is actually in the directory being served.
// A battery that asserted a hard-coded count would keep passing after the
// fixture changed underneath it, which is the vacuous-pass class this whole row
// is built to avoid.
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// scanFixture walks root and returns the file/directory expectation set.
func scanFixture(root string) (*fixtureInfo, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if fi, statErr := os.Stat(abs); statErr != nil || !fi.IsDir() {
		return nil, fmt.Errorf("served tree %s is not a directory", abs)
	}

	info := &fixtureInfo{
		Root:       abs,
		FilesInDir: map[string][]string{},
		SubDirs:    map[string][]string{},
	}

	walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(abs, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			info.Dirs = append(info.Dirs, "")
			return nil
		}
		// A staging file from an in-flight atomic write is invisible to every
		// listing by design (BFS-004 §2.1), so it is not part of the fixture
		// either — and a battery that counted one would report a phantom member.
		if strings.HasPrefix(d.Name(), ".davtmp-") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			info.Dirs = append(info.Dirs, rel)
			parent := dirOf(rel)
			info.SubDirs[parent] = append(info.SubDirs[parent], d.Name())
			return nil
		}
		info.Files = append(info.Files, rel)
		parent := dirOf(rel)
		info.FilesInDir[parent] = append(info.FilesInDir[parent], d.Name())
		if rel == fixBigFile {
			entry, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			info.BigBytes = entry.Size()
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("scan %s: %w", abs, walkErr)
	}

	sort.Strings(info.Files)
	sort.Strings(info.Dirs)
	for _, v := range info.FilesInDir {
		sort.Strings(v)
	}
	for _, v := range info.SubDirs {
		sort.Strings(v)
	}
	return info, nil
}
