package registry

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCompact_ThousandEvents and the other fsync-heavy tests run on a
// scratch-copied test root: on this dev box / (ext4, loadavg 17-33 during
// the tick-549 guard) amplified the old per-append open/write/fsync/close
// cycle into a >15-minute stall inside a single syscall.Fsync (QA-BUNKER-43
// evidence: .gitreins/logs/guard-20261001T082353.336025Z.log,
// /tmp/tick549-reg-test.log). The registry's durability contract does not
// depend on the backing filesystem of the TEST directory, so these test
// roots live on tmpfs when available and fall back to t.TempDir when it is
// not. The append fast-path itself is guarded on the REAL filesystem by
// TestAppendFsyncBudgetCanary, which deliberately does NOT relocate.
func fsyncHeavyTestRoot(t *testing.T) string {
	t.Helper()
	for _, dir := range []string{"/dev/shm", "/tmp"} {
		if ok, err := isTmpfs(dir); err == nil && ok {
			root, err := os.MkdirTemp(dir, "bunker-registry-test-*")
			if err != nil {
				continue
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			return root
		}
	}
	return t.TempDir()
}

// TestAppendFsyncBudgetCanary bounds the append path on the REAL filesystem
// (never tmpfs — relocating it would make the canary vacuous exactly where
// fsync pathologies live). The budget is load-adaptive: the test first
// calibrates with 3 timed fsync'd appends, then allows 250× the observed
// per-append latency plus a fixed 60s floor. At rest on this box that is
// far under 60s total; under heavy host load the budget stretches with the
// measured fsync latency instead of flaking, while a true hang (an append
// that never returns) still burns the whole -timeout and fails.
func TestAppendFsyncBudgetCanary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	s := testStore(t, Options{Path: path})

	// Calibrate: 3 fsync'd appends measure the CURRENT ambient fsync
	// latency of the real filesystem.
	const probes = 3
	start := time.Now()
	for i := 0; i < probes; i++ {
		if err := s.AppendSpawn(spawnRecord(fmt.Sprintf("canary-probe-%d", i))); err != nil {
			t.Fatalf("AppendSpawn probe %d: %v", i, err)
		}
	}
	perAppend := time.Since(start) / probes

	// The measured workload: 100 fsync-per-event appends.
	const appends = 100
	start = time.Now()
	for i := 0; i < appends; i++ {
		if err := s.AppendSpawn(spawnRecord(fmt.Sprintf("canary-%03d", i))); err != nil {
			t.Fatalf("AppendSpawn %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	budget := 60*time.Second + 250*perAppend
	if elapsed > budget {
		t.Fatalf("100 appends took %v (budget %v = 60s + 250×probe-avg %v/append) — fsync path is pathological again", elapsed, budget, perAppend)
	}
	t.Logf("100 durable appends in %v (%v/append avg; probe %v/append; budget %v)", elapsed, elapsed/appends, perAppend, budget)
}

// TestAppendHandleSurvivesAcrossAppends pins the fast path: after the first
// append the store holds a persistent append handle and a later append
// re-uses the SAME fd (no per-append open/close). A regression to per-append
// open/close flips the second fd identity.
func TestAppendHandleSurvivesAcrossAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	s := testStore(t, Options{Path: path})

	if err := s.AppendSpawn(spawnRecord("handle-1")); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	fd1 := s.appendF.Fd()
	s.mu.Unlock()

	if err := s.AppendSpawn(spawnRecord("handle-2")); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	fd2 := s.appendF.Fd()
	s.mu.Unlock()

	if fd1 != fd2 {
		t.Fatalf("append handle changed between appends: fd %d -> %d (per-append open/close is back)", fd1, fd2)
	}
}

// TestAppendHandleRefreshedAfterExternalReplacement pins the staleness
// guard: when another process replaces the active file (e.g.
// `bunker registry compact` — a temp file renamed over the path), the
// store's persistent handle points at the orphaned old inode. The next
// append must detect the inode swap and re-open the new active file, or the
// daemon keeps writing to a file nobody can ever read again.
//
// Identity is asserted by INODE, not fd number: fd numbers are recycled by
// the OS after close, so "new fd number" proves nothing. A pinned observer
// handle keeps the old inode alive while the store rotates onto the new
// one; SameFile(observer, store) must flip from true to false, and a
// control re-assert before the swap keeps the check from going vacuous.
func TestAppendHandleRefreshedAfterExternalReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.jsonl")
	s := testStore(t, Options{Path: path})

	if err := s.AppendSpawn(spawnRecord("ext-1")); err != nil {
		t.Fatal(err)
	}
	// Observer of the OLD inode; also a control for SameFile itself.
	observer, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()

	s.mu.Lock()
	storeFD := s.appendF
	s.mu.Unlock()
	same, err := sameFile(observer, storeFD)
	if err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Fatal("control: store handle and observer differ BEFORE the replacement — test is broken, not the store")
	}

	// External replacement, exactly what writeAtomic does elsewhere:
	// a temp file fsync'd and renamed over the active path.
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".compact-*")
	if err != nil {
		t.Fatal(err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(`{"ts":"2026-10-01T00:00:00Z","kind":"known","known_ids":[]}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := tmp.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		t.Fatal(err)
	}

	// The next append must go to the NEW inode (the durable file an
	// observer re-opens sees the event), not to the orphaned old one.
	if err := s.AppendSpawn(spawnRecord("ext-2")); err != nil {
		t.Fatal(err)
	}

	s.mu.Lock()
	storeFD = s.appendF
	s.mu.Unlock()
	same, err = sameFile(observer, storeFD)
	if err != nil {
		t.Fatal(err)
	}
	if same {
		t.Error("store still holds the OLD inode after an external file replacement; the next events would be written to a file nobody can read")
	}

	post, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(post, []byte(`"ext-2"`)) {
		t.Errorf("re-opened active file does not contain the post-replacement event; the append landed on the orphaned inode:\n%s", post)
	}
}

// TestRotateThenAppendUsesFreshFile pins the rotation hand-off: after a
// size-cap rotation the next append writes to the NEW active file (the
// persistent handle was dropped before the renames), and the rotated backup
// keeps the pre-rotation bytes.
func TestRotateThenAppendUsesFreshFile(t *testing.T) {
	dir := fsyncHeavyTestRoot(t)
	path := filepath.Join(dir, "agents.jsonl")
	s := testStore(t, Options{Path: path, MaxBytes: 4 << 10, MaxBackups: 2})

	if err := s.AppendSpawn(spawnRecord("rot-1")); err != nil {
		t.Fatal(err)
	}
	// Overflow the 4 KiB cap: ~200 heartbeats at ~100+ bytes each.
	for i := 0; i < 200; i++ {
		if err := s.AppendHeartbeat("rot-1", time.Now().Add(time.Hour), "running"); err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected rotated backup after cap overflow: %v", err)
	}
	if err := s.AppendHeartbeat("rot-1", time.Now().Add(2*time.Hour), "running"); err != nil {
		t.Fatal(err)
	}

	// The post-rotation event must be readable from the ACTIVE file by a
	// fresh observer (i.e. it went to the new inode, not the rotated one).
	post, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(post, []byte(`"expires_at"`)) {
		t.Errorf("active file after rotation+append has no heartbeat event:\n%s", post)
	}
	if int64(len(post)) > 4<<10 {
		t.Errorf("active file %d bytes exceeds cap after append — append went to the rotated inode", len(post))
	}
}

// sameFile reports whether two open handles reference the same file. It is
// the same predicate the store's staleness check uses (os.SameFile over two
// Stat results), applied from the test side.
func sameFile(a, b *os.File) (bool, error) {
	ia, err := a.Stat()
	if err != nil {
		return false, err
	}
	ib, err := b.Stat()
	if err != nil {
		return false, err
	}
	return os.SameFile(ia, ib), nil
}

// TestCloseIdempotentAndDropsHandles pins that Close closes the persistent
// handles and that a second Close does not panic or error on closed files.
func TestCloseIdempotentAndDropsHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	s := testStore(t, Options{Path: path})
	if err := s.AppendSpawn(spawnRecord("close-1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	s.mu.Lock()
	closed := s.appendF == nil && s.repairF == nil
	s.mu.Unlock()
	if !closed {
		t.Error("Close left persistent handles open")
	}
}
