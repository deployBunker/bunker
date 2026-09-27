//go:build !linux

// The non-Linux half of the BFS-025 probe: a named refusal rather than a build
// that cannot be attempted. The arm itself needs FUSE, splice(2) and a mounted
// filesystem, none of which exist on this platform's build; the seam that will
// carry it (BFS-010's Windows binding) is the place this file changes.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "bfs025-truncated-read: this arm needs FUSE and splice(2); it runs on Linux only (see internal/fsmount/fs_unsupported.go for the platform seam)")
	os.Exit(2)
}
