//go:build linux

// Command bfs025-truncated-read is the acceptance arm for BFS-025: THE READ THAT
// THE KERNEL CANNOT SHOW, SERVED ANYWAY.
//
// THE DEFECT (reproduced by the bankai release driver, and by this probe on the
// tree as it stands): a path that is merely *stat*ed, then replaced with longer
// content on the agent, is read back as a FRAGMENT with rc=0 — 5 bytes of a
// 66-byte file. Nothing fails, nothing warns, and a build or a checksum that
// consumes those bytes gets a wrong answer.
//
// THE MECHANISM this probe measures rather than assumes: the truncation is not in
// the client's cache and not in the daemon's read handler. A FUSE read is bounded
// by the kernel inode's `i_size`, which is the size the mount put in its last
// attrs reply for the path, and a read that goes through the kernel's SPLICE path
// (uutils `cat`, and every tool that copies file→pipe) is clamped to it: the
// daemon returns every byte and the kernel throws the rest away. A plain read()
// is NOT clamped, which is why the two arms below disagree on the unfixed tree and
// why "the reader got 5 bytes" has to be attributed to a reader, never to a byte
// count.
//
// WHAT THIS PROBE ASSERTS
//
//  1. splice read after a stale stat: the FULL new bytes, or a loud error.
//     Never a fragment with rc=0. (RED on the unfixed tree: 5 bytes, rc=0.)
//  2. recovery: after the loud error, a fresh stat + read returns the full
//     bytes — a refusal the caller can recover from, not a dead end.
//  3. the unclamped control: read() of the same path returns the full bytes on
//     both trees, which is what identifies the clamp as the kernel's, not a
//     lost byte in the client.
//  4. read-then-replace (BFS-024's shape): whatever is served, it is never a
//     FRAGMENT — it is the complete old content or the complete new one. This
//     probe does not fix BFS-024 and does not assert that it is fixed.
//  5. the mirror image: a stat whose size is LARGER than the live content. The
//     reader receives every byte the resource has (a true EOF) and the mount
//     reports the divergence rather than leaving it to be discovered.
//
// RUN: go run ./probes/bfs025-truncated-read [--work DIR] [--keep]
// Needs /dev/fuse and fusermount, like any mount arm. Exit code 0 = every check
// passed; 1 = at least one check failed (the failing check is named).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/deployBunker/bunker/internal/davserve"
	"github.com/deployBunker/bunker/internal/fsmount"
)

const (
	shortBody = "SHORT"                                                                              // 5 bytes
	newBody   = "X-REPLACEMENT-CONTENT-0123456789-abcdefghijklmnopqrstuvwxyz-ABCDEFGHIJKLMNOPQRSTUV" // 82 bytes
	// BFS-024's arm needs two contents that differ in LENGTH as well as bytes,
	// or "served the new bytes" and "served the old bytes" cannot be told apart.
	b024Old = "OLD-CONTENT-AAAA"                         // 16 bytes
	b024New = "NEW-CONTENT-BBBB-longer-than-the-old-one" // 38 bytes
)

var failures []string

func main() {
	work := flag.String("work", "", "work directory (default: a fresh mktemp -d)")
	keep := flag.Bool("keep", false, "keep the mount (do not unmount at the end)")
	flag.Parse()

	dir := *work
	if dir == "" {
		d, err := os.MkdirTemp("", "bfs025-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "mktemp: %v\n", err)
			os.Exit(2)
		}
		dir = d
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "work dir: %v\n", err)
		os.Exit(2)
	}
	if err := run(dir, *keep); err != nil {
		fmt.Fprintf(os.Stderr, "\nBFS-025 ARMS: FAIL\n")
		for _, f := range failures {
			fmt.Fprintf(os.Stderr, "  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Printf("\nBFS-025 ARMS: PASS (work dir %s)\n", dir)
}

func fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	failures = append(failures, msg)
	fmt.Printf("  *** FAIL: %s\n", msg)
}

func ok(format string, args ...any) { fmt.Printf("  ok: "+format+"\n", args...) }

func run(dir string, keep bool) error {
	// The endpoint: the landed WebDAV surface on a loopback listener, with no
	// daemon lifecycle (internal/davserve explains why).
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		return err
	}
	target := filepath.Join(tree, "target.txt")
	if err := os.WriteFile(target, []byte(shortBody), 0o644); err != nil {
		return err
	}
	srv, err := davserve.Serve(tree, "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("davserve: %w", err)
	}
	defer srv.Close()
	fmt.Printf("endpoint          : %s (serving %s)\n", srv.URL, tree)

	mnt := filepath.Join(dir, "mnt")
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		return err
	}
	// A leftover mount from an interrupted run would make every path operation
	// below block on a dead daemon: refuse instead, and say how to clear it.
	if mounted, err := isMount(mnt); err != nil {
		return err
	} else if mounted {
		return fmt.Errorf("%s is already a mountpoint (a leftover from an interrupted arm): run `fusermount -u -z %s`, or use a fresh --work dir", mnt, mnt)
	}
	// XDG_CACHE_HOME keeps the cache (and its status document) inside the work
	// dir: the probe must not touch a real mount's cache.
	if err := os.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "xdg")); err != nil {
		return err
	}
	var log strings.Builder
	m, err := fsmount.MountAt(fsmount.Options{
		Mountpoint:    mnt,
		BaseURL:       srv.URL,
		Concurrency:   8,
		CacheMaxBytes: fsmount.DefaultCacheMaxBytes,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(&log, format+"\n", args...)
		},
	})
	if err != nil {
		return fmt.Errorf("mount %s: %w", mnt, err)
	}
	fmt.Printf("mount             : %s on %s\n  cache dir        : %s\n", srv.URL, mnt, m.CacheDir())
	if !keep {
		defer func() { _ = m.Unmount() }()
	}

	// One file per arm: each arm's bound, cache entry and metadata are then
	// independent, and no arm can inherit another's state (the first draft of
	// this probe did, and arm 4/5 measured the leftovers instead of themselves).
	armShort := filepath.Join(tree, "short-then-long.txt")
	armB024 := filepath.Join(tree, "read-then-replace.txt")
	armMirror := filepath.Join(tree, "long-then-short.txt")
	mShort := filepath.Join(mnt, "short-then-long.txt")
	mB024 := filepath.Join(mnt, "read-then-replace.txt")
	mMirror := filepath.Join(mnt, "long-then-short.txt")

	// ------------------------------------------------------------- arms 1/2/3
	fmt.Printf("\n== arms 1/2/3: stat, replace, read (the driver's reproduction) ==\n")
	if err := os.WriteFile(armShort, []byte(shortBody), 0o644); err != nil {
		return err
	}
	if st, err := os.Stat(mShort); err != nil {
		return fmt.Errorf("stat over the mount: %w", err)
	} else {
		fmt.Printf("  stat (no read)     : size=%d  <- this alone is what the kernel will bound a read by\n", st.Size())
	}
	if err := os.WriteFile(armShort, []byte(newBody), 0o644); err != nil {
		return err
	}
	st, err := os.Stat(mShort)
	if err != nil {
		return fmt.Errorf("stat over the mount: %w", err)
	}
	fmt.Printf("  agent now          : size=%d\n", len(newBody))
	fmt.Printf("  stat (post)        : size=%d  (stale on both trees: the mount's metadata has not moved)\n", st.Size())

	n, serr := spliceRead(mShort)
	fmt.Printf("  splice read        : %d bytes errno=%v\n", n, errnoOf(serr))
	switch {
	case serr != nil:
		ok("the read FAILED LOUDLY (%v) instead of returning a fragment with rc=0", errnoOf(serr))
	case n == int64(len(newBody)):
		ok("the read returned every byte of the resource (%d)", n)
	default:
		fail("SILENT TRUNCATION: the splice reader received %d of %d bytes with rc=0", n, len(newBody))
	}

	st2, err := os.Stat(mShort)
	if err != nil {
		return fmt.Errorf("stat over the mount: %w", err)
	}
	n2, serr2 := spliceRead(mShort)
	fmt.Printf("  re-open + reread   : stat=%d splice=%d bytes errno=%v\n", st2.Size(), n2, errnoOf(serr2))
	if serr2 == nil && n2 == int64(len(newBody)) && st2.Size() == int64(len(newBody)) {
		ok("recovered: the retry returned all %d bytes and stat reports the true size", n2)
	} else if serr != nil {
		fail("NOT RECOVERABLE: after a refused read, stat=%d splice=%d bytes errno=%v (want %d bytes)",
			st2.Size(), n2, errnoOf(serr2), len(newBody))
	}

	raw, cerr := os.ReadFile(mShort)
	if cerr == nil && string(raw) == newBody {
		ok("the unclamped control read all %d bytes: the fragment is the kernel's bound, not a lost byte", len(raw))
	} else if cerr != nil {
		fail("the unclamped control failed: %v", cerr)
	} else {
		fail("the unclamped control returned %d bytes (want %d)", len(raw), len(newBody))
	}

	// ------------------------------------------------------------------- arm 4
	fmt.Printf("\n== arm 4: read, replace, read again (BFS-024's shape; NOT this row's fix) ==\n")
	if err := os.WriteFile(armB024, []byte(b024Old), 0o644); err != nil {
		return err
	}
	if _, err := os.Stat(mB024); err != nil {
		return err
	}
	p1, err := os.ReadFile(mB024)
	if err != nil {
		return err
	}
	fmt.Printf("  phase 1 (before)   : read()=%d bytes %q\n", len(p1), string(p1))
	if err := os.WriteFile(armB024, []byte(b024New), 0o644); err != nil {
		return err
	}
	if st, err := os.Stat(mB024); err != nil {
		return err
	} else {
		fmt.Printf("  stat (post)        : size=%d (want %d, the live content)\n", st.Size(), len(b024New))
	}
	p2bytes, p2err := spliceRead(mB024)
	p2, rerr := os.ReadFile(mB024)
	fmt.Printf("  phase 2 (after)    : splice=%d bytes errno=%v ; read()=%d bytes %q\n",
		p2bytes, errnoOf(p2err), len(p2), truncate(string(p2), 28))
	switch {
	case rerr != nil:
		ok("phase 2 was refused (%v) — loud, not a fragment", rerr)
	case string(p2) == b024New:
		ok("the mount served the NEW bytes: BFS-024's acceptance is met by this change (report it if so)")
	case string(p2) == b024Old:
		fmt.Printf("  -> STALE, unchanged by this row: BFS-024 is a filed row with its own acceptance\n")
	case len(p2) != len(b024Old) && len(p2) != len(b024New):
		fail("a FRAGMENT was served (%d bytes: neither the old %d nor the new %d)", len(p2), len(b024Old), len(b024New))
	default:
		fmt.Printf("  -> a complete length but neither content byte-for-byte\n")
	}
	if p2err == nil && p2bytes != int64(len(b024Old)) && p2bytes != int64(len(b024New)) {
		fail("the splice reader of arm 4 received a fragment: %d bytes (old %d / new %d)", p2bytes, len(b024Old), len(b024New))
	}

	// ------------------------------------------------------------------- arm 5
	fmt.Printf("\n== arm 5: the published size LARGER than the live content (the mirror image) ==\n")
	fmt.Printf("  (a path never read: the content is fetched live, so this arm measures the bound and not the cache)\n")
	if err := os.WriteFile(armMirror, []byte(newBody), 0o644); err != nil {
		return err
	}
	if st, err := os.Stat(mMirror); err != nil {
		return err
	} else {
		fmt.Printf("  stat (published)   : size=%d\n", st.Size())
	}
	if err := os.WriteFile(armMirror, []byte(shortBody), 0o644); err != nil {
		return err
	}
	n3, serr3 := spliceRead(mMirror)
	raw3, err3 := os.ReadFile(mMirror)
	fmt.Printf("  live content       : %d bytes %q\n", len(shortBody), shortBody)
	fmt.Printf("  splice read        : %d bytes errno=%v\n", n3, errnoOf(serr3))
	fmt.Printf("  read()             : %d bytes %q errno=%v\n", len(raw3), string(raw3), err3)
	if serr3 == nil && n3 == int64(len(shortBody)) && err3 == nil && string(raw3) == shortBody {
		ok("every byte the resource has was served, then a true EOF (no fragment of the larger published size)")
	} else {
		fail("the mirror arm served %d spliced / %d read bytes (want %d) errno=%v/%v", n3, len(raw3), len(shortBody), errnoOf(serr3), err3)
	}

	m.WriteStatusNow()
	if rb, reported := statusReadBound(m.CacheDir()); reported {
		fmt.Printf("  status.read_bound  : refusals_total=%d corrections_total=%d last=%q\n",
			rb.refusalsTotal, rb.correctionsTotal, rb.last)
		if rb.correctionsTotal == 0 {
			fail("the divergence was not reported on the owner-facing surface (corrections_total=0)")
		} else {
			ok("the divergence is reported, not silent (corrections_total=%d)", rb.correctionsTotal)
		}
	} else {
		fmt.Printf("  status.read_bound  : absent from this build's status document (the pre-fix tree)\n")
	}

	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if strings.Contains(line, "read bound divergence") {
			fmt.Printf("  mount log          : %s\n", line)
		}
	}
	_, high := m.Client().InFlight()
	fmt.Printf("  requests issued    : %d (in-flight max %d)\n", m.Client().Requests(), high)

	if len(failures) > 0 {
		return fmt.Errorf("%d check(s) failed", len(failures))
	}
	return nil
}

// spliceRead copies the path file→pipe with splice(2), which is what uutils cat
// does and what the kernel clamps at the inode's i_size. It returns the bytes the
// reader received and the error it saw (nil when the kernel reported EOF).
//
// The drain is bounded by the count splice REPORTED, never by a budget: Go's pipe
// File parks a read on an empty pipe instead of returning EAGAIN (the runtime owns
// O_NONBLOCK and the netpoller), so an unbounded drain on the error path hangs the
// arm instead of reporting the refusal. Measured while building this row: it did,
// for two runs, until the count-bounded form below replaced it.
func spliceRead(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r, w, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer r.Close()
	defer w.Close()

	var total int64
	buf := make([]byte, 1<<16)
	for {
		n, err := syscall.Splice(int(f.Fd()), nil, int(w.Fd()), nil, 1<<20, 0)
		if err != nil {
			// Nothing was inserted: splice is all-or-nothing, so there is
			// nothing to drain and the error is the reader's answer.
			return total, err
		}
		if n == 0 {
			return total, nil
		}
		for pending := n; pending > 0; {
			k, rerr := r.Read(buf)
			total += int64(k)
			pending -= int64(k)
			if rerr != nil {
				return total, rerr
			}
		}
	}
}

func errnoOf(err error) any {
	if err == nil {
		return nil
	}
	return err
}

// isMount reports whether dir is already a mountpoint, read from /proc/mounts
// rather than by stat(2): stat'ing a stale FUSE mountpoint blocks on a dead
// daemon, which is exactly the state this check exists to catch.
func isMount(dir string) (bool, error) {
	raw, err := os.ReadFile("/proc/mounts")
	if err != nil {
		// /proc is the honest source; without it, no claim rather than a guess.
		return false, nil
	}
	esc := strings.ReplaceAll(dir, " ", `\040`)
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == esc {
			return true, nil
		}
	}
	return false, nil
}

// boundState is the owner-facing read_bound block. On a tree that predates it,
// reported is false rather than an error: the probe must fail for the defect,
// never for the absence of a figure it is also checking.
type boundState struct {
	reported         bool
	refusalsTotal    int64
	correctionsTotal int64
	last             string
}

func statusReadBound(cacheDir string) (boundState, bool) {
	raw, err := os.ReadFile(filepath.Join(cacheDir, "status.json"))
	if err != nil {
		return boundState{}, false
	}
	var doc struct {
		ReadBound *struct {
			RefusalsTotal    int64  `json:"refusals_total"`
			CorrectionsTotal int64  `json:"corrections_total"`
			Last             string `json:"last"`
		} `json:"read_bound"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.ReadBound == nil {
		return boundState{}, false
	}
	return boundState{
		reported:         true,
		refusalsTotal:    doc.ReadBound.RefusalsTotal,
		correctionsTotal: doc.ReadBound.CorrectionsTotal,
		last:             doc.ReadBound.Last,
	}, true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
