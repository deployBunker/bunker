// bfs052probe mounts the bunker surface over a davserve tree at a real FUSE
// mountpoint, runs the BFS-052 capability probe against the LIVE kernel, and
// prints the result — the live evidence the acceptance criteria ask for
// ("the negotiated values against THIS host's kernel").
//
// Usage: bfs052probe <mountpoint>
// The probe unmounts before exiting; it touches no live daemon.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/deployBunker/bunker/internal/davserve"
	"github.com/deployBunker/bunker/internal/fsmount"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: bfs052probe <mountpoint>")
		os.Exit(2)
	}
	mp := os.Args[1]
	root, err := os.MkdirTemp("", "bfs052-src")
	if err != nil {
		fmt.Fprintln(os.Stderr, "source tree:", err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(root) }()
	if err := os.WriteFile(root+"/probe.txt", []byte("bfs-052 live negotiated values\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(mp, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "mountpoint:", err)
		os.Exit(1)
	}
	srv, err := davserve.Serve(root, "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "davserve:", err)
		os.Exit(1)
	}
	defer func() { _ = srv.Close() }()

	m, err := fsmount.MountAt(fsmount.Options{
		Mountpoint:  mp,
		BaseURL:     srv.URL,
		Concurrency: 4,
		OpTimeout:   10 * time.Second,
		BindTimeout: 5 * time.Second,
	})
	if err != nil {
		// The honest outcome on a host that cannot mount: say so and exit
		// non-zero rather than printing figures nobody negotiated.
		fmt.Fprintln(os.Stderr, "live mount refused:", err)
		os.Exit(3)
	}
	defer func() { _ = m.Unmount() }()
	if err := m.Server().WaitMount(); err != nil {
		fmt.Fprintln(os.Stderr, "WaitMount:", err)
		os.Exit(1)
	}
	// One whole-tree observation so the mount is live in the usual shape.
	st := m.Status()
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{
		"mountpoint":     mp,
		"fuse":           st.Fuse,
		"readdirplus":    m.ReaddirPlusNegotiated(),
		"kernel_version": kernelVersion(),
	}); err != nil {
		fmt.Fprintln(os.Stderr, "encode:", err)
		os.Exit(1)
	}
}

func kernelVersion() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return string(trimSpace(b))
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

var _ = fuse.MAX_KERNEL_WRITE
