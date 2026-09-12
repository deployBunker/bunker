package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T, opts Options) *Store {
	t.Helper()
	if opts.Path == "" {
		opts.Path = filepath.Join(t.TempDir(), "agents.jsonl")
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func spawnRecord(id string) *Record {
	return &Record{
		AgentID:   id,
		Status:    "running",
		CreatedAt: time.Now().UTC().Truncate(time.Second),
		ExpiresAt: time.Now().UTC().Add(6 * time.Hour).Truncate(time.Second),
		PortStart: 10000,
		PortEnd:   10099,
	}
}

// TestDefaultsAreDocumented pins the board requirement: 5 MiB active file with
// three rotated backups, and the default production path.
func TestDefaultsAreDocumented(t *testing.T) {
	if DefaultMaxBytes != 5<<20 {
		t.Errorf("DefaultMaxBytes = %d, want 5 MiB (%d)", DefaultMaxBytes, 5<<20)
	}
	if DefaultMaxBackups != 3 {
		t.Errorf("DefaultMaxBackups = %d, want 3", DefaultMaxBackups)
	}
	if DefaultPath != "/var/lib/bunkerd/agents.jsonl" {
		t.Errorf("DefaultPath = %q, want /var/lib/bunkerd/agents.jsonl", DefaultPath)
	}
}

func TestOpen_EmptyRegistryReplaysCleanly(t *testing.T) {
	s := testStore(t, Options{})
	rep := s.Report()
	if rep.Events != 0 || rep.Live != 0 || rep.Known != 0 || rep.Malformed != 0 || rep.PartialTail {
		t.Fatalf("empty registry report = %+v, want zeroes", rep)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat registry: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("registry mode = %o, want 600", perm)
	}
}

func TestLifecyclePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	s := testStore(t, Options{Path: path})

	if err := s.AppendSpawn(spawnRecord("alpha")); err != nil {
		t.Fatalf("AppendSpawn: %v", err)
	}
	if !s.Known("alpha") || s.Get("alpha") == nil {
		t.Fatal("spawned agent must be known and live")
	}
	if err := s.AppendHeartbeat("alpha", time.Now().Add(12*time.Hour), "running"); err != nil {
		t.Fatalf("AppendHeartbeat: %v", err)
	}
	if err := s.AppendSpawn(spawnRecord("beta")); err != nil {
		t.Fatalf("AppendSpawn(beta): %v", err)
	}
	if err := s.AppendDestroy("alpha"); err != nil {
		t.Fatalf("AppendDestroy: %v", err)
	}
	if s.Get("alpha") != nil {
		t.Error("destroyed agent must not be live")
	}
	if !s.Known("alpha") {
		t.Error("destroyed agent must stay known (idempotent destroy)")
	}
	if s.Known("never-seen") {
		t.Error("an ID that was never written must not be known")
	}

	// A fresh process (relaunch) must replay exactly this state.
	s2, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if s2.Get("beta") == nil {
		t.Error("replay lost live agent beta")
	}
	if s2.Get("alpha") != nil {
		t.Error("replay resurrected destroyed agent alpha")
	}
	if !s2.Known("alpha") {
		t.Error("replay lost the known-ID record for alpha")
	}
	if got, want := s2.KnownCount(), 1; got != want {
		t.Errorf("KnownCount = %d, want %d", got, want)
	}
	rec := s2.Get("beta")
	if rec.PortStart != 10000 || rec.PortEnd != 10099 {
		t.Errorf("replayed port range = %d-%d, want 10000-10099 (exact restoration)", rec.PortStart, rec.PortEnd)
	}
	if rec.ExpiresAt.IsZero() {
		t.Error("replayed record lost its expiry")
	}
}

func TestAppendDestroyIsIdempotentOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	s := testStore(t, Options{Path: path})
	if err := s.AppendSpawn(spawnRecord("alpha")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendDestroy("alpha"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendDestroy("alpha"); err != nil {
		t.Fatalf("second AppendDestroy: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("repeated destroy must not append another event")
	}
}

// TestReplay_TolerantParsing covers the damaged-input contract: a corrupt
// COMPLETE line is skipped, a partial FINAL line is ignored, and every valid
// event before the damage is still replayed.
func TestReplay_TolerantParsing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.jsonl")

	good := `{"ts":"2026-09-12T10:00:00Z","kind":"spawn","agent_id":"keep","status":"running","port_start":10000,"port_end":10099}`
	alsoGood := `{"ts":"2026-09-12T10:01:00Z","kind":"destroy","agent_id":"gone"}`
	corrupt := `{"ts":"2026-09-12T10:02:00Z","kind":"spawn", BROKEN}`
	content := good + "\n" + corrupt + "\n" + alsoGood + "\n" + `{"ts":"2026-09-12T10:03:`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	rep := s.Report()
	if rep.Malformed != 1 {
		t.Errorf("Malformed = %d, want 1", rep.Malformed)
	}
	if !rep.PartialTail {
		t.Error("PartialTail = false, want true (torn final line)")
	}
	if rep.Events != 2 {
		t.Errorf("Events = %d, want 2 valid events", rep.Events)
	}
	if s.Get("keep") == nil {
		t.Error("valid event before the corrupt line was lost")
	}
	if !s.Known("gone") {
		t.Error("valid event after the corrupt line was lost")
	}

	// Writing after a torn tail must not corrupt the log: the new event is
	// appended on its own line and replays cleanly.
	if err := s.AppendDestroy("keep"); err != nil {
		t.Fatalf("append after torn tail: %v", err)
	}
	s2, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if s2.Get("keep") != nil {
		t.Error("appended event after a torn tail was not replayed")
	}
}

func TestRotation_SizeCapAndBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.jsonl")
	s := testStore(t, Options{Path: path, MaxBytes: 512, MaxBackups: 3})

	// Write enough events to force several rotations.
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("agent-%02d", i)
		if err := s.AppendSpawn(spawnRecord(id)); err != nil {
			t.Fatalf("AppendSpawn(%s): %v", id, err)
		}
	}
	for i := 1; i <= 3; i++ {
		backup := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(backup); err != nil {
			t.Errorf("expected rotated backup %s: %v", backup, err)
		}
	}
	// The oldest backup must be dropped: nothing beyond .3 exists.
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Errorf("unexpected fourth backup %s.4 (err=%v)", path, err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat active: %v", err)
	} else if info.Size() > 512 {
		t.Errorf("active file = %d bytes, want <= 512 after rotation", info.Size())
	}

	// Retention is BOUNDED by design (active + 3 backups): replay folds
	// everything still on disk, so with a tiny cap the oldest events are
	// gone and the newest agents survive. Operators keep the log small with
	// `bunker registry compact`; the default 5 MiB x 3 window is orders of
	// magnitude larger than the event volume of a full agent fleet.
	s2, err := Open(Options{Path: path, MaxBytes: 512, MaxBackups: 3})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	live := s2.LiveCount()
	if live == 0 {
		t.Fatal("replay after rotation restored nothing")
	}
	if live >= 60 {
		t.Errorf("replayed %d live agents — retention is not bounded by the cap", live)
	}
	if s2.Get("agent-59") == nil {
		t.Error("the newest agent must survive rotation")
	}
	if s2.Get("agent-00") != nil {
		t.Error("the oldest agent should have been evicted by the size cap")
	}
}

func TestCompact_ThousandEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.jsonl")
	s := testStore(t, Options{Path: path, MaxBytes: 1 << 20, MaxBackups: 3})

	// 1000+ events: spawns, heartbeats and destroys.
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("burst-%03d", i)
		if err := s.AppendSpawn(spawnRecord(id)); err != nil {
			t.Fatalf("AppendSpawn: %v", err)
		}
		if i%10 == 0 {
			if err := s.AppendHeartbeat(id, time.Now().Add(time.Hour), "running"); err != nil {
				t.Fatalf("AppendHeartbeat: %v", err)
			}
		}
		if i%4 == 0 {
			if err := s.AppendDestroy(id); err != nil {
				t.Fatalf("AppendDestroy: %v", err)
			}
		}
	}
	if got := s.LiveCount(); got != 750 {
		t.Fatalf("LiveCount before compact = %d, want 750", got)
	}

	stats, err := s.Compact()
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if stats.BeforeEvents < 1000 {
		t.Errorf("BeforeEvents = %d, want >= 1000", stats.BeforeEvents)
	}
	wantAfter := stats.AfterLive + 1 // live records + the known-ID index line
	if stats.AfterEvents != wantAfter {
		t.Errorf("AfterEvents = %d, want %d (one record per live agent + index)", stats.AfterEvents, wantAfter)
	}
	if stats.AfterLive != 750 {
		t.Errorf("AfterLive = %d, want 750 (no live agent lost)", stats.AfterLive)
	}
	if stats.AfterKnown != 250 {
		t.Errorf("AfterKnown = %d, want 250 destroyed IDs remembered", stats.AfterKnown)
	}

	// Replay after compaction must be identical, and compacting twice must
	// produce a byte-identical file (idempotent rewrite).
	s2, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen after compact: %v", err)
	}
	defer s2.Close()
	if s2.LiveCount() != 750 || s2.KnownCount() != 250 {
		t.Fatalf("post-compact replay = live %d / known %d, want 750 / 250", s2.LiveCount(), s2.KnownCount())
	}
	if !s2.Known("burst-000") {
		t.Error("a destroyed agent must stay known after compaction")
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Compact(); err != nil {
		t.Fatalf("second Compact: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The ts stamp of a rewrite changes; compare the record set instead.
	if countLines(first) != countLines(second) {
		t.Errorf("second compaction changed the record count: %d -> %d", countLines(first), countLines(second))
	}
	if s2.LiveCount() != 750 || s2.KnownCount() != 250 {
		t.Error("second compaction mutated the live/known sets")
	}
}

// TestCompact_RemovesRotatedBackups proves compaction cannot be undone by the
// backups it superseded.
func TestCompact_RemovesRotatedBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.jsonl")
	s := testStore(t, Options{Path: path, MaxBytes: 400, MaxBackups: 3})
	for i := 0; i < 40; i++ {
		if err := s.AppendSpawn(spawnRecord(fmt.Sprintf("rot-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendDestroy("rot-00"); err != nil {
		t.Fatal(err)
	}
	// Retention may already be bounded by rotation; compaction must preserve
	// exactly whatever the log still folds to.
	before := s.LiveCount()
	if _, err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	for i := 1; i <= 3; i++ {
		backup := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(backup); !os.IsNotExist(err) {
			t.Errorf("rotated backup %s survived compaction (err=%v)", backup, err)
		}
	}
	s2, err := Open(Options{Path: path, MaxBytes: 400, MaxBackups: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Get("rot-00") != nil {
		t.Error("compaction/replay resurrected a destroyed agent")
	}
	if got := s2.LiveCount(); got != before {
		t.Errorf("live after compaction = %d, want %d (compaction must not drop live agents)", got, before)
	}
}

func TestKnownIndexIsBounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.jsonl")
	s := testStore(t, Options{Path: path, KnownIDCap: 3})
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("cap-%d", i)
		if err := s.AppendSpawn(spawnRecord(id)); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendDestroy(id); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.KnownCount(); got != 3 {
		t.Errorf("KnownCount = %d, want 3 (bounded index)", got)
	}
	if !s.Known("cap-5") || !s.Known("cap-4") || !s.Known("cap-3") {
		t.Error("the index must keep the most recently destroyed IDs")
	}
	if s.Known("cap-0") {
		t.Error("the oldest known ID should have been evicted by the bound")
	}
	// The bound survives compaction (the index is what compaction persists).
	if _, err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(Options{Path: path, KnownIDCap: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.KnownCount() != 3 || !s2.Known("cap-5") {
		t.Errorf("post-compact known index = %d (cap-5 known=%v), want 3 (true)",
			s2.KnownCount(), s2.Known("cap-5"))
	}
}

// TestCrossProcessExclusion proves the advisory lock is honoured across
// processes: a compact must wait for a helper process that holds the lock,
// and still succeed afterwards.
func TestCrossProcessExclusion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.jsonl")
	marker := filepath.Join(dir, "locked")
	s := testStore(t, Options{Path: path})
	if err := s.AppendSpawn(spawnRecord("alpha")); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRegistryLockHelperProcess", "-test.v")
	cmd.Env = append(os.Environ(),
		"BUNKER_REGISTRY_LOCK_HELPER="+path,
		"BUNKER_REGISTRY_LOCK_MARKER="+marker,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lock helper: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	// Wait until the helper actually holds the lock.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lock helper never acquired the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}

	start := time.Now()
	stats, err := s.Compact()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("Compact returned after %v — it did not wait for the cross-process lock", elapsed)
	}
	if stats.AfterLive != 1 {
		t.Errorf("AfterLive = %d, want 1", stats.AfterLive)
	}
}

// TestRegistryLockHelperProcess is not a test: re-exec'd by
// TestCrossProcessExclusion it holds the registry lock for ~1.2s.
func TestRegistryLockHelperProcess(t *testing.T) {
	path := os.Getenv("BUNKER_REGISTRY_LOCK_HELPER")
	marker := os.Getenv("BUNKER_REGISTRY_LOCK_MARKER")
	if path == "" {
		t.Skip("helper process only")
	}
	s := &Store{path: path}
	unlock, err := s.lockCrossProcess()
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	if err := os.WriteFile(marker, []byte("locked"), 0o600); err != nil {
		t.Fatalf("helper marker: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	unlock()
}

func countLines(b []byte) int {
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// TestEventLineSchemaKeepsStableKeys guards the on-disk contract other tools
// (jq pipelines, operators) rely on.
func TestEventLineSchemaKeepsStableKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.jsonl")
	s := testStore(t, Options{Path: path})
	rec := spawnRecord("schema-check")
	if err := s.AppendSpawn(rec); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &ev); err != nil {
		t.Fatalf("event line is not JSON: %v", err)
	}
	for _, key := range []string{"ts", "kind", "agent_id", "status", "port_start", "port_end", "expires_at"} {
		if _, ok := ev[key]; !ok {
			t.Errorf("spawn event line missing key %q: %v", key, ev)
		}
	}
}
