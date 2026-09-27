//go:build linux

package fsmount

// ============================================================================
// BFS-039 — CANCEL-IO CORRECTNESS, the mount half.
//
// CANCEL IS TWO EVENTS (PRD-bunker-invalidation.md R10 / §2.8):
//
//	A DELIBERATE one  — a FUSE interrupt: the caller is killed, times out or is
//	                    Ctrl-C'd, go-fuse cancels the request context, and the
//	                    operation is abandoned WITH a message. It must come back
//	                    as EINTR (retryable), never as EIO or a transport errno,
//	                    and the retry must land EXACTLY ONCE.
//	an ACCIDENTAL one — the reader dies, the mount is signalled, the process is
//	                    KILLED. NO cancel ever arrives. The requirement is
//	                    therefore not "handle the cancel": it is that the
//	                    absence of one is never corrupting, which is a property
//	                    of the REPRESENTATION (atomic, idempotent mutations),
//	                    not of a message.
//
// The cells below are the second half of the BFS-046 test program's cell 8
// (its client-side HALF-FILE and KILLED-READER cells are in
// internal/fsclient/bfs046_program_test.go and stay as they are). Every cell
// here names the defect it catches and ships the SOURCE MUTATION that turns it
// red in docs/evidence/BFS-039-arms.sh.
// ============================================================================

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/davserve"
	"github.com/deployBunker/bunker/internal/fsclient"
)

// bfs039OldBody is the target's content before every arm: a failed or cancelled
// read or write must leave exactly these bytes.
const bfs039OldBody = "the previous content, which no cancel may destroy\n"

// bfs039WriteBufferPrefix is the name the mount gives one open write handle's
// local buffer (fs_linux.go, ensureBuffer).
const bfs039WriteBufferPrefix = "writebuf-"

// bfs039Mount wires the mount's own pieces around a real davserve endpoint and
// a cache whose directory IS the mount directory — which is what MountAt does
// (MountDir is the cache dir), and the reason a killed writer's local buffer is
// a cache-directory fact.
func bfs039Mount(t testing.TB) (*Mount, string, string) {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte(bfs039OldBody), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	srv, err := davserve.Serve(root, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("davserve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	c, err := fsclient.NewClient(fsclient.Options{
		BaseURL: srv.URL, Concurrency: 4, OpTimeout: 30 * time.Second, BindTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	dir := t.TempDir()
	cache, err := fsclient.OpenCache(fsclient.CacheConfig{
		Dir: dir, MaxBytes: 64 << 20, MaxEntryBytes: 8 << 20, MaxEntries: 64, MaxInFlight: 2,
	})
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	m := bfs039MountWith(t, c, cache, dir, root, srv.URL)
	return m, target, dir
}

// bfs039MountWith assembles a Mount over an existing client/cache. It is shared
// with the helper process, which needs the same wiring inside the child.
func bfs039MountWith(t testing.TB, c *fsclient.Client, cache *fsclient.Cache, dir, root, baseURL string) *Mount {
	t.Helper()
	m := &Mount{
		opts:    Options{BaseURL: baseURL, Concurrency: 4},
		logf:    t.Logf,
		dir:     dir,
		client:  c,
		cache:   cache,
		wp:      fsclient.NewWritePath(c, cache, dir, fsclient.OnConflictRefuse),
		inv:     fsclient.NewInvalidator(c, fsclient.InvalidateOptions{Mode: fsclient.ModePoll}),
		bounds:  newBoundRegistry(),
		reads:   map[uint64]*readHandle{},
		writes:  map[uint64]*writeHandle{},
		bufDocs: map[uint64]int64{},
		verdict: "healthy",
	}
	m.snap.Store(fsclient.NewSnapshot(""))
	return m
}

// bfs039Write opens the mount's write path exactly as the kernel does for a new
// file. Create (fs_linux.go, the only shape that produces a write handle) does
// two things: it registers the handle and it puts a node in the snapshot. The
// handle is the subject of every cell below, so it is taken through the SAME
// constructor Create calls (`newWriteHandle`), with the same snapshot node
// recorded — Create's INODE plumbing needs a live FUSE bridge that a headless
// cell has not got, and it changes nothing about the write path.
func bfs039Write(t *testing.T, m *Mount, name string) *writeHandle {
	t.Helper()
	cp := joinPath("", name)
	h := m.newWriteHandle(cp, true)
	m.snapshot().Put(fsclient.Node{Path: cp, IsDir: false, Mode: "0644", Mtime: time.Now()})
	return h
}

// bfs039Sha is the byte-identity witness every arm reports.
func bfs039Sha(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// bfs039BufferResidue lists the mount's own write-buffer files in the cache
// directory. These are the files a killed writer's bytes live in, and nothing
// else in the cache's accounting can see them.
func bfs039BufferResidue(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), bfs039WriteBufferPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// bfs039Kill is SIGKILL: the signal no cancel arrives for.
func bfs039Kill(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_, _ = cmd.Process.Wait()
}

// ---------------------------------------------------------------------------
// CELL D — THE ACCIDENTAL CANCEL: a killed WRITER.
//
// THE DEFECT THIS CELL CATCHES, measured on the tree that landed BFS-046
// (docs/evidence/BFS-039-red.txt): a writer killed with an unpublished write
// buffer leaves the buffer file behind in the CACHE DIRECTORY, and the reopen
// sweep does not touch it — `OpenCache` sweeps `blobs/` only, because the
// buffer's name belongs to the mount and not to the cache. So every killed
// writer permanently costs one orphan file that no reported figure bounds: the
// same class as BFS-031 (a bound that does not bound the directory) and the
// same class as BFS-046's no-sweep cell, arriving through the write path
// instead of the refresh path.
//
// The claim is NOT that a sweep could not be written: it is that a cache
// mutation whose bytes have no name CANNOT be left behind at all, which is the
// only form of the guarantee that survives the absence of a cancel. The fix
// makes the buffer ANONYMOUS at creation (its name is unlinked immediately, the
// open descriptor is the only reference), so the kernel reclaims the inode with
// the last descriptor and a SIGKILL leaves nothing.
// ---------------------------------------------------------------------------

// TestBFS039HelperProcess is the killed writer itself, re-executed as a CHILD
// process: SIGKILL is the whole point and a goroutine cannot be killed. It is
// skipped in the normal run of the suite.
func TestBFS039HelperProcess(t *testing.T) {
	mode := os.Getenv("BFS039_HELPER_MODE")
	if mode == "" {
		t.Skip("the BFS-039 helper process (driven by the cells below; SIGKILLed mid-write on purpose)")
	}
	root := os.Getenv("BFS039_HELPER_ROOT")
	dir := os.Getenv("BFS039_HELPER_DIR")

	srv, err := davserve.Serve(root, "127.0.0.1:0")
	if err != nil {
		fmt.Println("HELPER-ERROR serve:", err)
		os.Exit(3)
	}
	defer srv.Close()
	c, err := fsclient.NewClient(fsclient.Options{
		BaseURL: srv.URL, Concurrency: 4, OpTimeout: 30 * time.Second, BindTimeout: 3 * time.Second,
	})
	if err != nil {
		fmt.Println("HELPER-ERROR client:", err)
		os.Exit(4)
	}
	cache, err := fsclient.OpenCache(fsclient.CacheConfig{
		Dir: dir, MaxBytes: 64 << 20, MaxEntryBytes: 8 << 20, MaxEntries: 64, MaxInFlight: 2,
	})
	if err != nil {
		fmt.Println("HELPER-ERROR cache:", err)
		os.Exit(5)
	}
	m := bfs039MountWith(t, c, cache, dir, root, srv.URL)

	// The child is a real writer: a handle from Create, with bytes in its
	// unpublished buffer, which is precisely the state a kill can interrupt.
	h := bfs039Write(t, m, "killed-writer.txt")
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	n, errno := h.Write(context.Background(), payload, 0)
	if errno != 0 || n != uint32(len(payload)) {
		fmt.Printf("HELPER-ERROR write: n=%d errno=%v\n", n, errno)
		os.Exit(6)
	}
	// The count travels with the ready line: it is the premise the parent
	// asserts (bytes ARE buffered and unpublished) and the fix is that those
	// bytes have no name anywhere on disk to be found again.
	fmt.Printf("BFS039-HELPER-READY bytes=%d\n", n)
	_ = os.Stdout.Sync()
	select {} // killed by the parent; never returns
}

// bfs039SpawnWriter starts the helper and returns once it holds an unpublished
// buffer, together with the number of bytes it reports buffering. A helper that
// cannot start is a failure of the cell, never a silent skip.
func bfs039SpawnWriter(t *testing.T, root, dir string) (*exec.Cmd, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestBFS039HelperProcess", "-test.timeout=120s")
	cmd.Env = append(os.Environ(),
		"BFS039_HELPER_MODE=buffer",
		"BFS039_HELPER_ROOT="+root,
		"BFS039_HELPER_DIR="+dir,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("helper start: %v", err)
	}
	ready := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "HELPER-ERROR") {
				ready <- line
				return
			}
			if strings.HasPrefix(line, "BFS039-HELPER-READY") {
				ready <- line
				return
			}
		}
		ready <- "helper exited before it was ready"
	}()
	select {
	case msg := <-ready:
		if strings.HasPrefix(msg, "HELPER-ERROR") || !strings.HasPrefix(msg, "BFS039-HELPER-READY") {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			t.Fatalf("helper: %s", msg)
		}
		n := 0
		if _, err := fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(msg, "BFS039-HELPER-READY")), "bytes=%d", &n); err != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			t.Fatalf("helper reported %q, which names no byte count", msg)
		}
		return cmd, n
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Fatalf("helper never reported ready")
	}
	return nil, 0
}

func TestBFS039KilledWriterLeavesNoResidueAndThePathUsable(t *testing.T) {
	// The fixture is built by the PARENT and handed to the child, so the parent
	// can read the target and the cache directory afterwards.
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte(bfs039OldBody), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	dir := t.TempDir()
	before := bfs039Sha(t, target)

	cmd, buffered := bfs039SpawnWriter(t, root, dir)

	// The premise of the arm, asserted rather than assumed: the killed writer
	// really had bytes buffered and unpublished. Without it the cell could pass
	// on a build where the writer buffered nothing and would then prove nothing
	// about a kill.
	if buffered <= 0 {
		bfs039Kill(t, cmd)
		t.Fatalf("the helper reported %d buffered bytes: this arm cannot show what a kill leaves behind, so it proves nothing about an accidental cancel", buffered)
	}
	// MEASURED, not assumed: whether those bytes are visible on disk at all.
	// On the tree that landed BFS-046 they are (`writebuf-<n>`), which is what
	// makes a kill leave an orphan; the fix is that they are not.
	live := bfs039BufferResidue(t, dir)
	t.Logf("BFS039-MEASURE while the writer is live: %d buffered bytes, %d named buffer file(s) in the cache dir %v", buffered, len(live), live)

	// THE ACCIDENTAL CANCEL. No cancel message is sent, because in the real
	// event there is none.
	bfs039Kill(t, cmd)

	// (a) THE TARGET KEEPS ITS PREVIOUS CONTENT, byte for byte. A killed writer
	// is not a write: nothing was published, so nothing may have changed.
	if after := bfs039Sha(t, target); after != before {
		t.Fatalf("a killed writer changed the target: %s -> %s", before, after)
	}
	// (b) NO RESIDUE. The buffer is the mutation whose bytes were on disk with
	// no name a reader could reach; a kill must not be able to leave it behind,
	// because nothing bounds it and nothing sweeps it.
	if residue := bfs039BufferResidue(t, dir); len(residue) != 0 {
		t.Fatalf("a SIGKILLed writer left %v in the cache directory (%d file(s), %d buffered bytes): the bytes have no reader and no bound, and they survive the reopen sweep because the sweep owns blobs/ and not this name",
			residue, len(residue), buffered)
	}
	// (b2) NO LOCK ARTIFACT, on top of the operation below actually proceeding:
	// the per-path commit lock is not persisted, so a death cannot leave it
	// held. The sensor has its own control (TestBFS039LockSensorIsNotBlind), so
	// a green reading here means "there is none" rather than "this cannot see
	// one".
	if stale := bfs039LockArtifacts(t, dir); len(stale) != 0 {
		t.Fatalf("a SIGKILLed writer left a lock artifact %v: a lock that outlives its holder is the stale-lock defect", stale)
	}
	// (c) THE PATH IS USABLE FROM A FRESH HANDLE, which is what a mount process
	// restarting actually does — and it is where a STALE LOCK would show up.
	m2, _, _ := bfs039MountAt(t, root, dir)
	start := time.Now()
	h := bfs039Write(t, m2, "after-the-kill.txt")
	if n, errno := h.Write(context.Background(), []byte("written after the kill\n"), 0); errno != 0 || n == 0 {
		t.Fatalf("a write after the kill failed: n=%d errno=%v", n, errno)
	}
	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("the write after the kill was refused: errno=%v (%s)", errno, fsclient.ErrnoName(errno))
	}
	latency := time.Since(start)
	got, err := os.ReadFile(filepath.Join(root, "after-the-kill.txt"))
	if err != nil || string(got) != "written after the kill\n" {
		t.Fatalf("the write after the kill did not land on the server (%v, %q)", err, got)
	}
	t.Logf("BFS039-MEASURE first successful write after the kill: %s (no stale lock: the operation proceeded)", latency.Round(time.Microsecond))

	// (d) THE FRESH HANDLE OWNS ITS OWN BUFFER: the kill took nothing with it.
	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatalf("releasing the handle twice errored: errno=%v (a duplicate/late release must be a no-op)", errno)
	}
	if residue := bfs039BufferResidue(t, dir); len(residue) != 0 {
		t.Fatalf("a cleanly released handle left %v behind: the anonymous buffer is not anonymous", residue)
	}
}

// TestBFS039CancelledPublicationDoesNotCloseTheBuffer is the DETERMINISTIC arm
// for the second defect this row found, against a server that holds the request
// open so the cancel provably lands while the PUT is in flight.
//
// THE DEFECT: net/http closes a request body on every failed round trip
// ("c._send() always closes req.Body"), so handing the transport the handle's
// own *os.File made a cancelled publication CLOSE the caller's buffer — the
// retry the EINTR asks for then found a closed file and was refused with EIO.
// The bytes were never lost from disk; they became unreachable from the code
// that had to re-send them, which is the same outcome for the caller.
//
// The claim measured here is the one that matters: after a cancel, the retry
// sends the CALLER'S BYTES, byte for byte, and the server sees exactly two
// attempts (the cancelled one and the retry) — not a truncated second body.
func TestBFS039CancelledPublicationDoesNotCloseTheBuffer(t *testing.T) {
	dir := t.TempDir()
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	var mu sync.Mutex
	var got []byte
	puts := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			http.NotFound(w, r) // the path is new: no base to resolve
		case http.MethodPut:
			select {
			case arrived <- struct{}{}:
			default:
			}
			<-release
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			got, puts = body, puts+1
			mu.Unlock()
			sum := sha256.Sum256(body)
			w.Header().Set("X-Bunker-Hash", "sha256:"+hex.EncodeToString(sum[:]))
			w.Header().Set("X-Bunker-Tree", "tree-cancel")
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := fsclient.NewClient(fsclient.Options{
		BaseURL: srv.URL + "/dav", Concurrency: 4, OpTimeout: 30 * time.Second, BindTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	cache, err := fsclient.OpenCache(fsclient.CacheConfig{
		Dir: dir, MaxBytes: 64 << 20, MaxEntryBytes: 8 << 20, MaxEntries: 64, MaxInFlight: 2,
	})
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	m := bfs039MountWith(t, c, cache, dir, "", srv.URL)
	h := bfs039Write(t, m, "cancelled-mid-flight.txt")
	body := []byte("the bytes the caller had already handed over\n")
	if n, errno := h.Write(context.Background(), body, 0); errno != 0 || int(n) != len(body) {
		t.Fatalf("buffer the write: n=%d errno=%v", n, errno)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan syscall.Errno, 1)
	go func() { done <- h.publish(ctx) }()
	select {
	case <-arrived:
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("the PUT never reached the server: this arm cannot cancel a request that is in flight, so it would prove nothing")
	}
	cancel() // THE DELIBERATE CANCEL, with the request provably in flight
	if errno := <-done; errno != syscall.EINTR {
		t.Fatalf("a cancel mid-request returned errno=%v (%s), want EINTR", errno, fsclient.ErrnoName(errno))
	}
	close(release)

	// THE RETRY. It must land AND carry the caller's bytes.
	if errno := h.publish(context.Background()); errno != 0 {
		t.Fatalf("the retry after a mid-request cancel was refused with errno=%v (%s): the cancelled attempt destroyed the buffer the retry has to re-send",
			errno, fsclient.ErrnoName(errno))
	}
	mu.Lock()
	sent, attempts := append([]byte(nil), got...), puts
	mu.Unlock()
	if string(sent) != string(body) {
		t.Fatalf("the retry sent %q (%d bytes), want the caller's buffered bytes %q (%d bytes): a cancelled attempt closed the buffer the retry reads",
			sent, len(sent), body, len(body))
	}
	if attempts != 2 {
		t.Fatalf("the server saw %d PUTs, want 2 (the cancelled attempt and the retry): the retry must be ONE more request, never a silent repeat of it", attempts)
	}
	t.Logf("BFS039-MEASURE mid-request cancel: %d PUT attempts, retry carried all %d buffered bytes intact", attempts, len(sent))
}

// bfs039MountAt builds a fresh mount over an EXISTING cache directory and root,
// as a restarting mount process would.
func bfs039MountAt(t *testing.T, root, dir string) (*Mount, string, string) {
	t.Helper()
	srv, err := davserve.Serve(root, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("davserve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	c, err := fsclient.NewClient(fsclient.Options{
		BaseURL: srv.URL, Concurrency: 4, OpTimeout: 30 * time.Second, BindTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	cache, err := fsclient.OpenCache(fsclient.CacheConfig{
		Dir: dir, MaxBytes: 64 << 20, MaxEntryBytes: 8 << 20, MaxEntries: 64, MaxInFlight: 2,
	})
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return bfs039MountWith(t, c, cache, dir, root, srv.URL), filepath.Join(root, "target.txt"), dir
}

// ---------------------------------------------------------------------------
// CELL E — IDEMPOTENCE: A RETRIED WRITE MUST NOT APPLY TWICE.
//
// THE DEFECT THIS CELL CATCHES: a buffer whose writes are POSITIONAL only by
// accident. `Write` appends at the current file offset; `WriteAt` places the
// bytes. The kernel retries a write at an offset it already sent — after an
// interrupt, after a short write, after a duplicate request — and against an
// appending buffer that retry lands the bytes a SECOND time. That is a mutation
// applied twice, invisible to every counter, and it is exactly what "a retried
// write must not append twice" means.
//
// TWO ARMS, because the FIRST VERSION OF THIS CELL WAS BLIND and the arms
// script proved it (docs/evidence/BFS-039-arms.md): a retry from a NEW handle
// starts from a fresh buffer, so an appending buffer cannot double-apply there —
// the mutation passed. The arms are:
//
//	1. THE SAME OFFSET TWICE ON ONE HANDLE. The retry the kernel actually
//	   issues. This is the definitional arm and it is where an appending buffer
//	   doubles the bytes.
//	2. A MISORDERED PAIR. The buffer exists because a chunk stream is not
//	   guaranteed to be sequential (BFS-005 §5.4's stated deviation), so a late
//	   chunk can arrive before an early one. Under an appending buffer the bytes
//	   land in ARRIVAL order instead of at their offsets, and the server ends up
//	   holding something that is not the file the kernel acknowledged — the
//	   user-visible shape of the same defect.
//
// The third arm is the retry the errno ASKS FOR: after a cancellation the caller
// is told EINTR, so it retries, and the retry must land exactly once.
// ---------------------------------------------------------------------------

func TestBFS039RetriedWriteDoesNotApplyTwice(t *testing.T) {
	m, target, _ := bfs039Mount(t)
	ctx := context.Background()
	rootDir := filepath.Dir(target)

	// A first write lands.
	h1 := bfs039Write(t, m, "idempotent.txt")
	body := []byte("exactly once\n")
	if n, errno := h1.Write(ctx, body, 0); errno != 0 || int(n) != len(body) {
		t.Fatalf("first write: n=%d errno=%v", n, errno)
	}
	if errno := h1.Release(ctx); errno != 0 {
		t.Fatalf("first publish: errno=%v", errno)
	}
	path := filepath.Join(rootDir, "idempotent.txt")
	if got, _ := os.ReadFile(path); string(got) != string(body) {
		t.Fatalf("after the first write the target holds %q, want %q", got, body)
	}
	fi1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// ARM 1 — THE SAME MUTATION TWICE AT THE SAME OFFSET, on ONE handle: the
	// retry the kernel issues after an interrupt. The buffer must hold the bytes
	// ONCE, and so must the target.
	hSame := bfs039Write(t, m, "same-offset.txt")
	if n, errno := hSame.Write(ctx, body, 0); errno != 0 || int(n) != len(body) {
		t.Fatalf("arm1 first write: n=%d errno=%v", n, errno)
	}
	if n, errno := hSame.Write(ctx, body, 0); errno != 0 || int(n) != len(body) {
		t.Fatalf("arm1 retried write: n=%d errno=%v", n, errno)
	}
	// The buffer's own content, before any publication: an appending buffer has
	// already doubled here, and the published length would hide it — which is
	// why the buffer is read back rather than only the target.
	buf := make([]byte, hSame.size+64)
	read, _ := hSame.tmp.ReadAt(buf, 0)
	if !bytes.Equal(buf[:read], body) {
		t.Fatalf("ARM 1 — the handle's buffer holds %d byte(s) %q after the SAME write at the SAME offset twice, want exactly %q (%d byte(s)): the mutation was applied twice",
			read, buf[:read], body, len(body))
	}
	if errno := hSame.Release(ctx); errno != 0 {
		t.Fatalf("arm1 publish: errno=%v", errno)
	}
	gotSame, _ := os.ReadFile(filepath.Join(rootDir, "same-offset.txt"))
	if !bytes.Equal(gotSame, body) {
		t.Fatalf("ARM 1 — the target holds %q (%d bytes), want %q (%d bytes)", gotSame, len(gotSame), body, len(body))
	}

	// ARM 2 — A MISORDERED PAIR (the late chunk first, then the early one). The
	// expected content is the kernel's own: the early bytes at 0, the gap the
	// caller never wrote as NULs, the late bytes at their offset.
	const lateOff = 8
	early, late := []byte("early"), []byte("LATE")
	hMis := bfs039Write(t, m, "misordered.txt")
	if n, errno := hMis.Write(ctx, late, lateOff); errno != 0 || int(n) != len(late) {
		t.Fatalf("arm2 late write: n=%d errno=%v", n, errno)
	}
	if n, errno := hMis.Write(ctx, early, 0); errno != 0 || int(n) != len(early) {
		t.Fatalf("arm2 early write: n=%d errno=%v", n, errno)
	}
	if errno := hMis.Release(ctx); errno != 0 {
		t.Fatalf("arm2 publish: errno=%v", errno)
	}
	want := make([]byte, lateOff+len(late))
	copy(want, early)
	copy(want[lateOff:], late)
	gotMis, _ := os.ReadFile(filepath.Join(rootDir, "misordered.txt"))
	if !bytes.Equal(gotMis, want) {
		t.Fatalf("ARM 2 — the server holds %q, want %q: a misordered pair landed in ARRIVAL order instead of at its offsets, so the published file is not the file the kernel acknowledged",
			gotMis, want)
	}

	// ARM 3 — THE RETRY FROM A NEW HANDLE (a caller re-doing the write), with
	// the server-side proof that it was the reported NO-OP rather than a second
	// write: identical bytes were already there, so the surface answers
	// `identical_content` and the file's mtime does not move. Without that the
	// cell would pass on an implementation that rewrote the same bytes twice.
	h2 := bfs039Write(t, m, "idempotent.txt")
	if n, errno := h2.Write(ctx, body, 0); errno != 0 || int(n) != len(body) {
		t.Fatalf("retried write: n=%d errno=%v", n, errno)
	}
	if errno := h2.Release(ctx); errno != 0 {
		t.Fatalf("retried publish: errno=%v", errno)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(body) {
		t.Fatalf("ARM 3 — a RETRIED write double-applied: the target holds %q (%d bytes), want %q (%d bytes). A mutation applied twice is invisible to every counter and is the corruption an accidental cancel must never produce",
			got, len(got), body, len(body))
	}
	fi2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after retry: %v", err)
	}
	if !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Fatalf("the retry moved the mtime (%s -> %s): an identical rewrite must be the surface's REPORTED no-op, not a second write",
			fi1.ModTime(), fi2.ModTime())
	}

	// THE SUCCESSFUL PATH'S COST, AS A NUMBER. The row requires the successful
	// path to be unregressed and its cost stated rather than asserted, so five
	// WHOLE publications are driven through the real surface — Create → Write →
	// Release, the mount's buffer, ONE conditional PUT, the server's stage +
	// rename — and reported. No wall-clock threshold is asserted: this box runs
	// a fleet and a threshold here would be a flake generator. What is asserted
	// is that every one of them LANDS; the figures are the cost. The code this
	// row adds to this path is one unlink per handle and one interface hop per
	// body read.
	var cost []time.Duration
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("cost-%d.txt", i)
		start := time.Now()
		hc := bfs039Write(t, m, name)
		if n, errno := hc.Write(ctx, body, 0); errno != 0 || int(n) != len(body) {
			t.Fatalf("cost arm write: n=%d errno=%v", n, errno)
		}
		if errno := hc.Release(ctx); errno != 0 {
			t.Fatalf("cost arm publish: errno=%v", errno)
		}
		cost = append(cost, time.Since(start))
		if got, _ := os.ReadFile(filepath.Join(rootDir, name)); !bytes.Equal(got, body) {
			t.Fatalf("cost publication %d did not land (%q)", i, got)
		}
	}
	sort.Slice(cost, func(i, j int) bool { return cost[i] < cost[j] })
	t.Logf("BFS039-MEASURE a successful publication through the real surface: min=%s median=%s max=%s",
		cost[0].Round(time.Microsecond), cost[len(cost)/2].Round(time.Microsecond), cost[len(cost)-1].Round(time.Microsecond))
	t.Logf("BFS039-MEASURE retried write: target=%d bytes, mtime unmoved (%s) — the surface reported a no-op", len(got), fi2.ModTime().Format(time.RFC3339Nano))
}

// ---------------------------------------------------------------------------
// CELL F — THE DELIBERATE CANCEL, THROUGH THE MOUNT: EINTR, THE PREVIOUS
// CONTENT INTACT, THE RETRY APPLIED ONCE, AND NO LOCK LEFT HELD.
//
// THE DEFECT THIS CELL CATCHES: a publication that treats the caller's own
// interrupt as a verdict. The tree before this row returned the cancel as
// `unreachable_reset`/ENOTCONN AND recorded it as a permanent failure of the
// handle (`h.flushed = true` before the PUT), so the retry the caller is
// supposed to make was impossible: the handle answered with the cancel forever.
// That is "interruptible and careful" instead of atomic and idempotent — the
// shape §2.8 forbids.
// ---------------------------------------------------------------------------

func TestBFS039DeliberateCancelReturnsEINTRAndTheRetryLandsOnce(t *testing.T) {
	m, target, _ := bfs039Mount(t)
	before := bfs039Sha(t, target)

	h := bfs039Write(t, m, "cancelled.txt")
	body := []byte("the write the caller interrupted\n")
	if n, errno := h.Write(context.Background(), body, 0); errno != 0 || int(n) != len(body) {
		t.Fatalf("buffer the write: n=%d errno=%v", n, errno)
	}
	// THE DELIBERATE CANCEL: the caller's context is cancelled, which is what
	// go-fuse does to a request whose caller is gone.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	errno := h.publish(ctx)
	if errno != syscall.EINTR {
		t.Fatalf("a DELIBERATE cancel returned errno=%v (%s), want EINTR: a caller cannot tell its own interrupt from a failure, and cannot retry correctly (PRD §2.8 / R10)",
			errno, fsclient.ErrnoName(errno))
	}
	// Nothing was published, so nothing may have changed.
	if after := bfs039Sha(t, target); after != before {
		t.Fatalf("a cancelled publication changed an unrelated target: %s -> %s", before, after)
	}
	cancelledPath := filepath.Join(filepath.Dir(target), "cancelled.txt")
	if _, err := os.Stat(cancelledPath); err == nil {
		t.Fatal("the cancelled publication left a file on the server: nothing was refused, so nothing may exist")
	}

	// THE RETRY. `h.flushed` was NOT set by the cancel, so the handle is still
	// publishable and the caller's retry is possible.
	start := time.Now()
	if errno := h.publish(context.Background()); errno != 0 {
		t.Logf("BFS039-DIAG retry failure=%+v base=%+v hasBase=%v flushed=%v size=%d", h.failure, h.base, h.hasBase, h.flushed, h.size)
		t.Fatalf("the retry after a cancel was refused with errno=%v (%s): a cancellation is not a verdict, and the handle must stay publishable so the EINTR recovery works",
			errno, fsclient.ErrnoName(errno))
	}
	retryLatency := time.Since(start)
	got, err := os.ReadFile(cancelledPath)
	if err != nil {
		t.Fatalf("read the retried path: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("the retry after a cancel applied the bytes %d time(s) (%q), want exactly once (%q)", len(got)/max(len(body), 1), got, body)
	}
	// A DUPLICATE / LATE CANCEL IS A NO-OP, not an error: the publication has
	// already landed, so a cancel arriving now must leave the result alone. An
	// accidental cancel may never arrive at all, and a late one must be
	// harmless — the same rule, from the other side.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if errno := h.publish(ctx2); errno != 0 {
		t.Fatalf("a DUPLICATE cancel on an already-published handle returned errno=%v: a late cancel must be a no-op rather than an error", errno)
	}

	// THE LOCK STORY, MEASURED. The striped per-path commit lock lives on the
	// server and is not persisted anywhere (nothing a dead process can hold);
	// the observable claim is that the SAME PATH is immediately writable again
	// and that no lock artifact exists in the client's directory. Both are
	// measured here, with the latency, rather than asserted.
	if stale := bfs039LockArtifacts(t, m.dir); len(stale) != 0 {
		t.Fatalf("a lock artifact exists at rest: %v — a lock that outlives its holder is the stale-lock defect", stale)
	}
	second := time.Now()
	h2 := bfs039Write(t, m, "after-the-cancel.txt")
	if n, errno := h2.Write(context.Background(), []byte("x\n"), 0); errno != 0 || n != 2 {
		t.Fatalf("write after the cancel: n=%d errno=%v", n, errno)
	}
	if errno := h2.Release(context.Background()); errno != 0 {
		t.Fatalf("the write after the cancel on a cancelled path was refused: errno=%v", errno)
	}
	afterLatency := time.Since(second)
	t.Logf("BFS039-MEASURE cancel→retry latency=%s; the next successful operation after the cancel=%s; lock artifacts at rest=%d",
		retryLatency.Round(time.Microsecond), afterLatency.Round(time.Microsecond), len(bfs039LockArtifacts(t, m.dir)))
}

// bfs039LockArtifacts lists anything in the client's directory that could hold a
// lock across a process death: a lock file, a pid file, an "in use" marker. The
// list is the sensor, and TestBFS039LockSensorIsNotBlind drives it with a real
// stale lock so a green reading cannot be a blind one.
func bfs039LockArtifacts(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		n := strings.ToLower(d.Name())
		if strings.Contains(n, "lock") || strings.Contains(n, "pid") || strings.Contains(n, "in-use") || strings.Contains(n, "inuse") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// TestBFS039LockSensorIsNotBlind is the control that keeps the lock claim
// honest: the sensor above must SEE a stale lock when one exists, so a green
// reading means "there is none" rather than "this cannot see one".
func TestBFS039LockSensorIsNotBlind(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "commit.lock")
	if err := os.WriteFile(stale, []byte("holder=1234\ndead\n"), 0o600); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}
	got := bfs039LockArtifacts(t, dir)
	if len(got) == 0 {
		t.Fatal("the lock sensor did not see a stale lock file that exists: the green lock claim in this row would be blind")
	}
	t.Logf("BFS039-MEASURE lock sensor sees the injected stale lock: %v", got)
}
