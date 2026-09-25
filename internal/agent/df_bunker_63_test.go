package agent

// DF-BUNKER-63 regression tests: the spawn-side uid-collision precheck, the
// destroy-refusal reaper backoff and its durable persistence. Every test runs
// through SEAMS — no root, no /proc writes, no real users, no host commands.
//
// AC map:
//   (a) spawn collision → stage error naming pids + user rolled back
//       (TestSpawnUIDCollision_CollidedUIDFailsAndRollsBack)
//   (b) spawn clean → ready (TestSpawnUIDCollision_CleanUIDProceeds)
//   (e) reaper backs off and marks refused state
//       (TestReaperBacksOffAfterRefusal, TestBackoffForAttempts)
//   (f) refused state persists in the durable registry record
//       (TestRefusalPersistsAcrossReplay, TestRefusalClearedByDestroy)

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/registry"
	"github.com/deployBunker/bunker/internal/resource"
)

// df63Manager is a hermetic manager wired to a TEMP registry: the durable
// refusal record the reaper reads must be exercised against a real Store, not
// a mock, because replay is what limb 3's persistence guarantee is about.
func df63Manager(t *testing.T, buf *bytes.Buffer) (*AgentManager, string) {
	t.Helper()
	cfg := config.DefaultConfig()
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	isolateRegistryAt(t, cfg, path)
	var logger *slog.Logger
	if buf == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	} else {
		logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	m := NewAgentManager(cfg, logger, tracker, nil, nil)
	m.listSystemAgents = func() ([]SystemAgent, error) { return nil, nil }
	t.Cleanup(func() { m.Stop() })
	return m, path
}

// isolateRegistryAt points cfg's registry at the given path (isolateRegistry
// uses its own TempDir; the persistence tests need to KNOW the path to reopen
// the store).
func isolateRegistryAt(t *testing.T, cfg *config.Config, path string) {
	t.Helper()
	cfg.Agent.Registry.Enabled = true
	cfg.Agent.Registry.Path = path
	cfg.Agent.Registry.MaxBytes = 1 << 20
	cfg.Agent.Registry.MaxBackups = 3
	cfg.Agent.Registry.KnownIDCap = 100
}

// stubSpawnScanner swaps the spawn collision precheck's process scanner and
// restores it via t.Cleanup.
func stubSpawnScanner(t *testing.T, fn func(uid uint32) ([]userProcess, error)) {
	t.Helper()
	orig := spawnProcessScanner
	spawnProcessScanner = fn
	t.Cleanup(func() { spawnProcessScanner = orig })
}

// ── AC (a): collided uid → stage error + user rolled back ──────────────────

// TestSpawnUIDCollision_CollidedUIDFailsAndRollsBack drives the full spawn
// path end to end with PATH stubs (the intci5Manager hermetic harness) and a
// scanner stub reporting a foreign process on the new uid: the spawn must
// fail at the named uid-collision stage, the error must name the colliding
// pid and its cmdline, the JSONL breadcrumb must carry the stage, and the
// rollback must have removed the just-created user (userdel ran).
func TestSpawnUIDCollision_CollidedUIDFailsAndRollsBack(t *testing.T) {
	m := intci5Manager(t)
	journal := redirectBreadcrumbJournal(t)

	binDir := t.TempDir()
	useraddLog := filepath.Join(t.TempDir(), "useradd-calls")
	userdelLog := filepath.Join(t.TempDir(), "userdel-calls")
	writeStub(t, binDir, "useradd", recordingStub(useraddLog, 0))
	writeStub(t, binDir, "userdel", recordingStub(userdelLog, 0))
	writeStub(t, binDir, "pkill", stubSucceeds)
	writeStub(t, binDir, "pgrep", "exit 1\n")
	writeStub(t, binDir, "ssh-keygen", "echo 'keygen: must not run past the collision precheck' >&2\nexit 1\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	restore := lookupAgentUser
	lookupAgentUser = func(username string) (*user.User, error) {
		return &user.User{Username: username, Uid: "61011", Gid: "61011", HomeDir: "/home/" + username}, nil
	}
	defer func() { lookupAgentUser = restore }()

	// The precheck's scanner: the freshly created uid 61011 already owns a
	// live process — the fad4b89a shape (a foreign container's process).
	stubSpawnScanner(t, func(uid uint32) ([]userProcess, error) {
		if uid == 61011 {
			return []userProcess{{PID: 7085, Cmd: "python3 /srv/imhotep-backend/serve.py"}}, nil
		}
		return nil, nil
	})

	agentID := uniqueAgentID("dfb63-collide")
	_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: agentID, Ttl: "1h"})
	if err == nil {
		t.Fatal("spawn must fail when the new uid already owns a live process")
	}
	if !strings.Contains(err.Error(), "failed at stage "+StageUIDCollision) {
		t.Errorf("error does not name the uid-collision stage: %v", err)
	}
	for _, want := range []string{"uid collision", "pid 7085", "python3 /srv/imhotep-backend/serve.py", "61011"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("collision error missing %q — got: %v", want, err)
		}
	}

	// Premise: the user WAS created (useradd ran) before the precheck fired.
	if calls := readRecord(t, useraddLog); len(calls) == 0 {
		t.Fatal("test premise broken: useradd stub never ran")
	}
	// The rollback removed the just-created user.
	if calls := readRecord(t, userdelLog); len(calls) == 0 {
		t.Error("rollback never removed the collided user (userdel never ran)")
	}
	// The spawn never got past the precheck: keygen must not have run.
	// (Its stub exits 1 with a distinctive message; if spawn HAD reached
	// keygen the failure stage would be keygen, which the stage assertion
	// above already excludes. The breadcrumb is the durable evidence.)
	raw, readErr := os.ReadFile(journal)
	if readErr != nil {
		t.Fatalf("no spawn-failure breadcrumb was written: %v", readErr)
	}
	if !strings.Contains(string(raw), StageUIDCollision) {
		t.Errorf("breadcrumb does not carry the uid-collision stage: %s", string(raw))
	}
	if !strings.Contains(string(raw), "pid 7085") {
		t.Errorf("breadcrumb does not name the colliding pid: %s", string(raw))
	}
	// Nothing registered: the collided spawn must not leave a tracker record.
	if m.tracker.Get(agentID) != nil {
		t.Error("collided spawn left a tracker record behind")
	}
}

// TestSpawnUIDCollision_ScannerFailureFailsClosed pins the fail-closed rule
// for the precheck itself: a scanner that cannot run must fail the spawn, not
// pass it — "cannot look" must never read as "nothing there" on a security
// precheck.
func TestSpawnUIDCollision_ScannerFailureFailsClosed(t *testing.T) {
	m := intci5Manager(t)
	redirectBreadcrumbJournal(t)

	binDir := t.TempDir()
	writeStub(t, binDir, "useradd", stubSucceeds)
	writeStub(t, binDir, "userdel", recordingStub(filepath.Join(t.TempDir(), "userdel-calls"), 0))
	writeStub(t, binDir, "pkill", stubSucceeds)
	writeStub(t, binDir, "pgrep", "exit 1\n")
	writeStub(t, binDir, "ssh-keygen", "exit 1\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	restore := lookupAgentUser
	lookupAgentUser = func(username string) (*user.User, error) {
		return &user.User{Username: username, Uid: "61012", Gid: "61012", HomeDir: "/home/" + username}, nil
	}
	defer func() { lookupAgentUser = restore }()

	stubSpawnScanner(t, func(uint32) ([]userProcess, error) {
		return nil, errors.New("permission denied reading /proc")
	})

	agentID := uniqueAgentID("dfb63-unobservable")
	_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: agentID, Ttl: "1h"})
	if err == nil {
		t.Fatal("an unobservable uid scan must fail the spawn (fail closed)")
	}
	if !strings.Contains(err.Error(), "failed at stage "+StageUIDCollision) {
		t.Errorf("error does not name the uid-collision stage: %v", err)
	}
	if !strings.Contains(err.Error(), "fail closed") {
		t.Errorf("error does not state the fail-closed rule: %v", err)
	}
}

// ── AC (b): clean uid → spawn proceeds past the precheck ───────────────────

// TestSpawnUIDCollision_CleanUIDProceeds is the control arm: with the scanner
// reporting no processes, the spawn passes the precheck and continues into
// the next stage (keygen — whose stub fails, proving the spawn GOT there).
func TestSpawnUIDCollision_CleanUIDProceeds(t *testing.T) {
	m := intci5Manager(t)
	redirectBreadcrumbJournal(t)

	binDir := t.TempDir()
	keygenLog := filepath.Join(t.TempDir(), "keygen-calls")
	writeStub(t, binDir, "useradd", stubSucceeds)
	writeStub(t, binDir, "userdel", stubSucceeds)
	writeStub(t, binDir, "pkill", stubSucceeds)
	writeStub(t, binDir, "pgrep", "exit 1\n")
	writeStub(t, binDir, "ssh-keygen", "printf 'ssh-keygen\\n' >> \""+keygenLog+"\"\nexit 1\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	restore := lookupAgentUser
	lookupAgentUser = func(username string) (*user.User, error) {
		return &user.User{Username: username, Uid: "61013", Gid: "61013", HomeDir: "/home/" + username}, nil
	}
	defer func() { lookupAgentUser = restore }()

	stubSpawnScanner(t, func(uint32) ([]userProcess, error) { return nil, nil })

	agentID := uniqueAgentID("dfb63-clean")
	_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: agentID, Ttl: "1h"})
	if err == nil {
		t.Fatal("the keygen stub was scripted to fail; the spawn must not succeed past it")
	}
	if !strings.Contains(err.Error(), "failed at stage "+StageKeygen) {
		t.Errorf("clean-uid spawn did not proceed past the collision precheck into keygen (err: %v)", err)
	}
	if calls := readRecord(t, keygenLog); len(calls) == 0 {
		t.Errorf("ssh-keygen never ran, so the precheck-to-keygen order is unproven (calls: %v)", calls)
	}
}

// TestBuildSpawnUIDCollisionError unit-pins the error rendering: pids,
// cmdlines, the uid and the isolation rationale all ride the text.
func TestBuildSpawnUIDCollisionError(t *testing.T) {
	err := buildSpawnUIDCollisionError("bunker-abc123", 1001, []userProcess{
		{PID: 7085, Cmd: "python3 serve.py"},
		{PID: 7090, Cmd: "nginx"},
	})
	for _, want := range []string{
		"bunker-abc123", "1001", "pid 7085", "python3 serve.py", "pid 7090", "nginx",
		"same-uid signal privilege", "DF-BUNKER-63",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("collision error missing %q — got: %v", want, err)
		}
	}
}

// ── AC (e): the reaper backs off and marks the refused state ───────────────

// liveExpiredAgent registers an expired agent the way the reaper expects.
// It persists the agent durably too (a live agent ALWAYS carries a registry
// record — spawn's GAP-070 durability gate — and the refusal machinery
// refuses to fabricate durable state for an agent the registry does not
// know).
func liveExpiredAgent(t *testing.T, m *AgentManager, id string) {
	t.Helper()
	if m.portAlloc != nil {
		if _, _, err := m.portAlloc.Allocate(id); err != nil {
			t.Fatalf("allocate ports: %v", err)
		}
	}
	if err := m.tracker.Register(&resource.AgentRecord{
		AgentID:   id,
		Status:    StatusRunning,
		CreatedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-1 * time.Second),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := m.persistSpawn(&resource.AgentRecord{
		AgentID:   id,
		Status:    StatusRunning,
		CreatedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-1 * time.Second),
	}); err != nil {
		t.Fatalf("persist spawn: %v", err)
	}
}

// TestReaperBacksOffAfterRefusal drives the full loop end to end: an expired
// agent whose destroy is refused by the live-process gate (fake probe). After
// ONE refused attempt the reaper must skip the agent on the next pass instead
// of calling destroy again (the old behaviour: a destroy call every minute,
// forever). The durable record carries the refusal and the tracker status is
// marked.
func TestReaperBacksOffAfterRefusal(t *testing.T) {
	var buf bytes.Buffer
	m, _ := df63Manager(t, &buf)
	const id = "dfb63-reap"
	const username = "bunker-" + id
	liveExpiredAgent(t, m, id)

	fake := []userProcess{{PID: 4545, Cmd: "node /home/bunker-dfb63-reap/server.js"}}
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return fake, 61021, true, nil
	})
	presentUserWithUID(t, username, "61021")

	// First reap pass: the destroy runs and is REFUSED.
	m.reapExpiredAgents()
	if m.tracker.Get(id) == nil {
		t.Fatal("the refused agent must stay tracked (nothing was deleted)")
	}
	refusal := m.registry.RefusalOf(id)
	if refusal == nil {
		t.Fatalf("no refusal was recorded after the refused destroy; log:\n%s", buf.String())
	}
	if refusal.Status != StatusLiveProcesses || refusal.Attempts != 1 {
		t.Errorf("refusal = %+v, want status live_processes with 1 attempt", refusal)
	}
	// The refused state is surfaced: the tracker status names it.
	if rec := m.tracker.Get(id); rec == nil || !strings.HasPrefix(rec.Status, "destroy-refused:") {
		t.Errorf("tracker status = %+v, want a destroy-refused marker", rec)
	}

	// Second reap pass (the next minute, in production): the reaper must
	// SKIP the agent — no second destroy attempt while backing off. Count
	// the refusal attempts as the probe: a second destroy would append a
	// second attempt.
	before := m.registry.RefusalOf(id).Attempts
	m.reapExpiredAgents()
	after := m.registry.RefusalOf(id).Attempts
	if after != before {
		t.Errorf("the reaper retried the refused destroy during its backoff window (attempts %d → %d)", before, after)
	}
	if m.tracker.Get(id) == nil {
		t.Error("the backed-off agent must remain tracked")
	}
	if !strings.Contains(buf.String(), "backing off") {
		t.Errorf("the skip is not on the record; log:\n%s", buf.String())
	}
}

// TestBackoffForAttempts pins the exponential curve and its cap.
func TestBackoffForAttempts(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{0, time.Minute}, // no refusals: base
		{1, time.Minute}, // 2^0
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{7, time.Hour},  // 2^6 = 64m → capped
		{50, time.Hour}, // deep cap
	}
	for _, tc := range tests {
		if got := backoffForAttempts(tc.attempts); got != tc.want {
			t.Errorf("backoffForAttempts(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
}

// TestReaperBackoffElapsedRetries proves the backoff ELAPSES: once the retry
// window has passed (simulated by back-dating the last attempt), the reaper
// retries the destroy.
func TestReaperBackoffElapsedRetries(t *testing.T) {
	var buf bytes.Buffer
	m, _ := df63Manager(t, &buf)
	const id = "dfb63-elapsed"
	liveExpiredAgent(t, m, id)

	// Seed a refusal whose last attempt is 2 hours old with attempts=1
	// (backoff 1m) — long elapsed, so the reaper must retry.
	if err := m.registry.AppendRefusal(id, &registry.Refusal{
		Status:        StatusLiveProcesses,
		Attempts:      1,
		LastError:     "seeded",
		LastAttemptAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed refusal: %v", err)
	}
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return []userProcess{{PID: 4646, Cmd: "node"}}, 61022, true, nil
	})
	presentUserWithUID(t, "bunker-"+id, "61022")

	attemptsBefore := m.registry.RefusalOf(id).Attempts
	m.reapExpiredAgents()
	if got := m.registry.RefusalOf(id).Attempts; got != attemptsBefore+1 {
		t.Errorf("refusal attempts = %d, want %d (the reaper must retry after the backoff elapsed)", got, attemptsBefore+1)
	}
}

// ── AC (f): the refusal persists in the durable registry record ────────────

// TestRefusalPersistsAcrossReplay is the GAP-070 durability guarantee for the
// refusal state: a refusal recorded by one manager must be visible to a
// manager that replays the SAME registry file — the backoff survives a
// daemon restart.
func TestRefusalPersistsAcrossReplay(t *testing.T) {
	var buf bytes.Buffer
	m, path := df63Manager(t, &buf)
	const id = "dfb63-durable"
	liveExpiredAgent(t, m, id)

	fake := []userProcess{{PID: 4747, Cmd: "node /home/bunker-dfb63-durable/server.js"}}
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return fake, 61023, true, nil
	})
	presentUserWithUID(t, "bunker-"+id, "61023")

	m.reapExpiredAgents()
	first := m.registry.RefusalOf(id)
	if first == nil {
		t.Fatal("no refusal recorded")
	}

	// Reopen the SAME file: a brand-new store replays the refusal from disk.
	replayed, err := registry.Open(registry.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen registry: %v", err)
	}
	defer replayed.Close()
	got := replayed.RefusalOf(id)
	if got == nil {
		t.Fatal("the replayed registry lost the refusal state (not durable?)")
	}
	if got.Status != first.Status || got.Attempts != first.Attempts {
		t.Errorf("replayed refusal = %+v, want %+v", got, first)
	}
	if got.LastError != first.LastError {
		t.Errorf("replayed refusal last_error = %q, want %q", got.LastError, first.LastError)
	}
	if got.LastAttemptAt != first.LastAttemptAt {
		t.Errorf("replayed refusal last_attempt_at = %q, want %q", got.LastAttemptAt, first.LastAttemptAt)
	}
}

// TestRefusalClearedByDestroy proves the lifecycle: a SUCCESSFUL destroy
// removes the agent (and its refusal) from the live set; a fresh spawn
// supersedes any stale refusal.
func TestRefusalClearedByDestroy(t *testing.T) {
	var buf bytes.Buffer
	m, _ := df63Manager(t, &buf)
	const id = "dfb63-clear"

	// Seed a refusal for a live agent, then destroy it successfully (the
	// user is absent → the idempotent path, which appends a destroy).
	if err := m.tracker.Register(&resource.AgentRecord{AgentID: id, Status: StatusRunning}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := m.persistSpawn(&resource.AgentRecord{AgentID: id, Status: StatusRunning}); err != nil {
		t.Fatalf("persist spawn: %v", err)
	}
	if err := m.registry.AppendRefusal(id, &registry.Refusal{Status: StatusLiveProcesses, Attempts: 3}); err != nil {
		t.Fatalf("seed refusal: %v", err)
	}
	if m.registry.RefusalOf(id) == nil {
		t.Fatal("test premise: the seeded refusal is missing")
	}
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return nil, 0, false, nil // user already gone: the idempotent path
	})
	if _, err := m.Destroy(context.Background(), id, false); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if m.registry.RefusalOf(id) != nil {
		t.Error("a destroyed agent must not carry refusal state")
	}
	if m.registry.Get(id) != nil {
		t.Error("a destroyed agent must be gone from the live set")
	}
}

// TestRefusalClearedByFreshSpawn proves a spawn supersedes a stale refusal
// (the agent was re-created after an operator cleared the wedged state).
func TestRefusalClearedByFreshSpawn(t *testing.T) {
	var buf bytes.Buffer
	m, _ := df63Manager(t, &buf)
	const id = "dfb63-fresh"

	if err := m.tracker.Register(&resource.AgentRecord{AgentID: id, Status: StatusRunning}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := m.persistSpawn(&resource.AgentRecord{AgentID: id, Status: StatusRunning}); err != nil {
		t.Fatalf("persist spawn: %v", err)
	}
	if err := m.registry.AppendRefusal(id, &registry.Refusal{Status: StatusHomeRetained, Attempts: 2}); err != nil {
		t.Fatalf("seed refusal: %v", err)
	}
	if err := m.persistSpawn(&resource.AgentRecord{
		AgentID: id, Status: StatusRunning, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("persistSpawn: %v", err)
	}
	if refusal := m.registry.RefusalOf(id); refusal != nil {
		t.Errorf("a fresh spawn must supersede the stale refusal, got %+v", refusal)
	}
}

// TestRecordDestroyRefusal_AccumulatesAttempts pins the accumulation contract
// across MULTIPLE refused attempts (the reaper's second pass after the backoff
// elapsed must count attempt 2, not reset to 1).
func TestRecordDestroyRefusal_AccumulatesAttempts(t *testing.T) {
	var buf bytes.Buffer
	m, _ := df63Manager(t, &buf)
	const id = "dfb63-accum"
	if err := m.tracker.Register(&resource.AgentRecord{AgentID: id, Status: StatusRunning}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := m.persistSpawn(&resource.AgentRecord{AgentID: id, Status: StatusRunning}); err != nil {
		t.Fatalf("persist spawn: %v", err)
	}

	m.recordDestroyRefusalFn(id, StatusLiveProcesses, errors.New("first"))
	m.recordDestroyRefusalFn(id, StatusLiveProcesses, errors.New("second"))
	got := m.registry.RefusalOf(id)
	if got == nil || got.Attempts != 2 {
		t.Fatalf("refusal = %+v, want 2 accumulated attempts", got)
	}
	if !strings.Contains(got.LastError, "second") {
		t.Errorf("last_error = %q, want the most recent cause", got.LastError)
	}
}

// TestAppendRefusalUnknownAgentFails pins the store's loud refusal of a
// refusal for an agent it does not know as live — durable state must never be
// fabricated for an agent that does not exist.
func TestAppendRefusalUnknownAgentFails(t *testing.T) {
	_, path := df63Manager(t, nil)
	store, err := registry.Open(registry.Options{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if err := store.AppendRefusal("never-spawned", &registry.Refusal{Status: StatusLiveProcesses, Attempts: 1}); err == nil {
		t.Fatal("a refusal for an unknown agent must fail")
	}
}

// TestIsDestroyRefusalStatus pins the refusal vocabulary: exactly the two
// refuse-but-keep statuses back the reaper off; everything else does not.
func TestIsDestroyRefusalStatus(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
	}{
		{StatusLiveProcesses, true},
		{StatusHomeRetained, true},
		{"destroyed", false},
		{"not_found", false},
		{StatusUserdelFailed, false},
		{"", false},
	} {
		if got := isDestroyRefusalStatus(tc.status); got != tc.want {
			t.Errorf("isDestroyRefusalStatus(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// TestReaperBackoffRemaining pins the remaining-window math.
func TestReaperBackoffRemaining(t *testing.T) {
	var buf bytes.Buffer
	m, _ := df63Manager(t, &buf)
	const id = "dfb63-remaining"
	if err := m.tracker.Register(&resource.AgentRecord{AgentID: id, Status: StatusRunning}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := m.persistSpawn(&resource.AgentRecord{AgentID: id, Status: StatusRunning}); err != nil {
		t.Fatalf("persist spawn: %v", err)
	}

	// No refusal → zero.
	if wait := m.reaperBackoffRemaining(id); wait != 0 {
		t.Errorf("no refusal: wait = %s, want 0", wait)
	}
	// Fresh refusal, 1 attempt → just under a minute remaining.
	if err := m.registry.AppendRefusal(id, &registry.Refusal{
		Status:        StatusLiveProcesses,
		Attempts:      1,
		LastAttemptAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if wait := m.reaperBackoffRemaining(id); wait <= 0 || wait > time.Minute {
		t.Errorf("fresh refusal: wait = %s, want (0, 1m]", wait)
	}
	// An unparseable timestamp must not wedge the reaper silent: it reads as
	// a full backoff window, not zero.
	if err := m.registry.AppendRefusal(id, &registry.Refusal{
		Status:        StatusLiveProcesses,
		Attempts:      2,
		LastAttemptAt: "not-a-timestamp",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if wait := m.reaperBackoffRemaining(id); wait != backoffForAttempts(2) {
		t.Errorf("unparseable timestamp: wait = %s, want the full %s window", wait, backoffForAttempts(2))
	}
}

// TestAgentRefusalSurfaces pins the list/info surfacing shape.
func TestAgentRefusalSurfaces(t *testing.T) {
	var buf bytes.Buffer
	m, _ := df63Manager(t, &buf)
	const id = "dfb63-surface"
	if err := m.tracker.Register(&resource.AgentRecord{AgentID: id, Status: StatusRunning}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := m.persistSpawn(&resource.AgentRecord{AgentID: id, Status: StatusRunning}); err != nil {
		t.Fatalf("persist spawn: %v", err)
	}
	if s := m.agentRefusalSurfaces(id); s != nil {
		t.Fatalf("no refusal recorded yet, got %+v", s)
	}
	if err := m.registry.AppendRefusal(id, &registry.Refusal{
		Status:        StatusHomeRetained,
		Attempts:      2,
		LastError:     "archive failed",
		LastAttemptAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	s := m.agentRefusalSurfaces(id)
	if s == nil {
		t.Fatal("the refusal surface is empty after recording one")
	}
	if s.Status != StatusHomeRetained || s.Attempts != 2 {
		t.Errorf("surface = %+v, want home_retained with 2 attempts", s)
	}
}
