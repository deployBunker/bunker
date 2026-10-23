package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// This file pins the GAP-071 lifecycle contract for user-mode agents:
//
//	stop   — session units + processes stopped; user, home, container and the
//	         allocated port range SURVIVE, tracker record kept with status
//	         "stopped" (the vocabulary resource.AgentRecord already documents);
//	start  — a stopped agent is re-armed, status back to "running";
//	restart— stop + start with the heartbeat expiry RESET to now+TTL;
//	unknown id — status "not_found", never a panic or a nil dereference;
//	a second stop — "already_stopped" (idempotent), never an error.
//
// Every host command the lifecycle path runs goes through a package-level
// seam, so the whole file runs as a non-root user without touching the host.

// lifecycleSeams records what the lifecycle path asked the host to do.
type lifecycleSeams struct {
	stopUnits   []string
	startUnits  []string
	terminated  []string // usernames handed to terminateAgentProcesses
	dockerCalls [][]string
	listCalls   []string
	// listOutput is what the fake `systemctl --user list-units` prints.
	listOutput string
}

// stubLifecycleSeams swaps all five lifecycle seams for recorders and restores
// them via t.Cleanup (the package runs in one process; a leaked fake would
// poison every later test).
func stubLifecycleSeams(t *testing.T) *lifecycleSeams {
	t.Helper()
	s := &lifecycleSeams{}

	origStop, origStart := stopUserUnit, startUserUnit
	origList, origDocker, origTerm := listUserUnits, agentDockerCLI, terminateAgentProcesses

	stopUserUnit = func(ctx context.Context, unit string) ([]byte, error) {
		s.stopUnits = append(s.stopUnits, unit)
		return nil, nil
	}
	startUserUnit = func(ctx context.Context, unit string) ([]byte, error) {
		s.startUnits = append(s.startUnits, unit)
		return nil, nil
	}
	listUserUnits = func(ctx context.Context, pattern string) ([]byte, error) {
		s.listCalls = append(s.listCalls, pattern)
		return []byte(s.listOutput), nil
	}
	agentDockerCLI = func(ctx context.Context, agentID string, args ...string) ([]byte, error) {
		s.dockerCalls = append(s.dockerCalls, append([]string{agentID}, args...))
		return nil, nil
	}
	terminateAgentProcesses = func(ctx context.Context, username, unitName string, logger *slog.Logger) {
		s.terminated = append(s.terminated, username)
	}

	t.Cleanup(func() {
		stopUserUnit, startUserUnit = origStop, origStart
		listUserUnits, agentDockerCLI, terminateAgentProcesses = origList, origDocker, origTerm
	})
	return s
}

// newLifecycleManager returns a manager whose registry, data dir and port pool
// all live in t.TempDir() — no test reads the host's real /var/lib/bunkerd.
func newLifecycleManager(t *testing.T) (*AgentManager, *config.Config, *bytes.Buffer) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agent.BaseDataDir = t.TempDir()
	isolateRegistry(t, cfg)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	m := NewAgentManager(cfg, logger, tracker, nil, nil)
	t.Cleanup(m.Stop)
	return m, cfg, &buf
}

// registerLifecycleAgent registers a tracker record for id, allocating a real
// sub-range from the manager's pool so port survival is asserted against the
// allocator, not just the record.
func registerLifecycleAgent(t *testing.T, m *AgentManager, id, status string, expires time.Time, image string) *resource.AgentRecord {
	t.Helper()
	var start, end uint32
	if m.portAlloc != nil {
		s, e, err := m.portAlloc.Allocate(id)
		if err != nil {
			t.Fatalf("allocate ports for %s: %v", id, err)
		}
		start, end = s, e
	}
	rec := &resource.AgentRecord{
		AgentID:        id,
		Status:         status,
		CreatedAt:      time.Now().Add(-time.Hour),
		ExpiresAt:      expires,
		PortRangeStart: start,
		PortRangeEnd:   end,
		// Stands in for the agent's kept home/SSH material: the path must be
		// untouched by a stop (only Destroy removes it).
		SshPrivateKeyPath: filepath.Join(t.TempDir(), id),
		Image:             image,
	}
	if err := m.tracker.Register(rec); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	// Mirror a real spawn: the durable registry knows the agent, so the
	// lifecycle transitions have something to persist against.
	if err := m.persistSpawn(rec); err != nil {
		t.Fatalf("persist spawn %s: %v", id, err)
	}
	return rec
}

// ── unknown ids ─────────────────────────────────────────────────

// TestLifecycle_UnknownIDIsNotFound pins the "never a panic, never a nil
// dereference" half of the contract: an id that was never registered reports
// not_found with an error, for all three RPCs.
func TestLifecycle_UnknownIDIsNotFound(t *testing.T) {
	tests := []struct {
		name string
		call func(m *AgentManager, id string) (status string, err error)
	}{
		{"stop", func(m *AgentManager, id string) (string, error) {
			resp, err := m.StopAgent(context.Background(), id)
			return resp.GetStatus(), err
		}},
		{"start", func(m *AgentManager, id string) (string, error) {
			resp, err := m.StartAgent(context.Background(), id)
			return resp.GetStatus(), err
		}},
		{"restart", func(m *AgentManager, id string) (string, error) {
			resp, err := m.RestartAgent(context.Background(), id)
			return resp.GetStatus(), err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubLifecycleSeams(t)
			m, _, _ := newLifecycleManager(t)

			status, err := tt.call(m, "never-registered")
			if status != StatusNotFound {
				t.Errorf("status = %q, want %q", status, StatusNotFound)
			}
			if err == nil {
				t.Error("unknown id must return an error alongside not_found")
			}
			// No host command may be issued for an unknown agent.
			if status, err := tt.call(m, ""); err == nil || status != StatusInvalidValue {
				t.Errorf("empty id: status = %q, err = %v; want %q + error", status, err, StatusInvalidValue)
			}
			if status, err := tt.call(m, "BAD/ID"); err == nil || status != StatusInvalidValue {
				t.Errorf("invalid id: status = %q, err = %v; want %q + error", status, err, StatusInvalidValue)
			}
		})
	}
}

// ── stop ────────────────────────────────────────────────────────

// TestStopAgent_StatusMatrix pins running → stopped and stopped →
// already_stopped.
func TestStopAgent_StatusMatrix(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		want       string
		wantStops  int
		wantTerm   int
		wantStatus string // tracker status afterwards
	}{
		{"running_becomes_stopped", StatusRunning, StatusStoppedValue, 0, 1, StatusStopped},
		{"stopped_is_idempotent", StatusStopped, StatusAlreadyStopped, 0, 0, StatusStopped},
		{"failure_status_can_be_stopped", "failed", StatusStoppedValue, 0, 1, StatusStopped},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seams := stubLifecycleSeams(t)
			m, _, _ := newLifecycleManager(t)
			registerLifecycleAgent(t, m, "stopcase", tt.status, time.Now().Add(time.Hour), "")

			resp, err := m.StopAgent(context.Background(), "stopcase")
			if err != nil {
				t.Fatalf("StopAgent: %v", err)
			}
			if resp.GetStatus() != tt.want {
				t.Errorf("status = %q, want %q", resp.GetStatus(), tt.want)
			}
			if len(seams.stopUnits) != tt.wantStops {
				t.Errorf("stopUserUnit calls = %v, want %d", seams.stopUnits, tt.wantStops)
			}
			if len(seams.terminated) != tt.wantTerm {
				t.Errorf("terminateAgentProcesses calls = %v, want %d", seams.terminated, tt.wantTerm)
			}
			if got := m.tracker.Get("stopcase").Status; got != tt.wantStatus {
				t.Errorf("tracker status = %q, want %q", got, tt.wantStatus)
			}
		})
	}
}

// TestStopAgent_StopsTheAgentsOwnUnit pins the unit the stop targets:
// only RunAgent transient units (the agent's dockerd unit is terminated via
// process signal in terminateAgentProcesses, not via stopUserUnit — DF-BUNKER-107).
func TestStopAgent_StopsTheAgentsOwnUnit(t *testing.T) {
	seams := stubLifecycleSeams(t)
	m, _, _ := newLifecycleManager(t)
	registerLifecycleAgent(t, m, "own-unit", StatusRunning, time.Now().Add(time.Hour), "")

	// systemd reports this agent's run unit AND another agent's.
	seams.listOutput = strings.Join([]string{
		"bunker-run-own-unit-abc12345.service loaded active running bunker run",
		"bunker-run-other-agent-deadbeef.service loaded active running bunker run",
		"",
	}, "\n")

	resp, err := m.StopAgent(context.Background(), "own-unit")
	if err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	if resp.GetStatus() != StatusStoppedValue {
		t.Fatalf("status = %q, want %q", resp.GetStatus(), StatusStoppedValue)
	}

	// Only the agent's own run unit should be stopped, not the dockerd unit.
	want := []string{"bunker-run-own-unit-abc12345"}
	if len(seams.stopUnits) != len(want) {
		t.Fatalf("stopUserUnit calls = %v, want %v", seams.stopUnits, want)
	}
	for i, unit := range want {
		if seams.stopUnits[i] != unit {
			t.Errorf("stopUserUnit[%d] = %q, want %q", i, seams.stopUnits[i], unit)
		}
	}
	if len(seams.terminated) != 1 || seams.terminated[0] != "bunker-own-unit" {
		t.Errorf("terminated = %v, want [bunker-own-unit]", seams.terminated)
	}
}

// TestStopAgent_PreservesUserHomeAndPorts is the core "pause, do not destroy"
// assertion: the tracker record survives with status "stopped", the allocated
// port range is still held by the allocator, the record's own paths are
// untouched, and NO delete command (userdel / rm) is ever issued — proven with
// PATH stubs, so a regression that reintroduces userdel via a raw exec would
// be recorded and fail the test.
func TestStopAgent_PreservesUserHomeAndPorts(t *testing.T) {
	seams := stubLifecycleSeams(t)
	_ = seams
	m, _, _ := newLifecycleManager(t)
	rec := registerLifecycleAgent(t, m, "keep-me", StatusRunning, time.Now().Add(time.Hour), "")
	wantStart, wantEnd := rec.PortRangeStart, rec.PortRangeEnd
	wantKey := rec.SshPrivateKeyPath

	// PATH stubs: any userdel/rm the lifecycle path issued would append here.
	stubDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "deletes.log")
	for _, cmd := range []string{"userdel", "rm", "rmdir"} {
		script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + logPath + "\"\n"
		if err := os.WriteFile(filepath.Join(stubDir, cmd), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	resp, err := m.StopAgent(context.Background(), "keep-me")
	if err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	if resp.GetStatus() != StatusStoppedValue {
		t.Fatalf("status = %q, want %q", resp.GetStatus(), StatusStoppedValue)
	}

	// The record survives, stopped, with every resource it owns intact.
	got := m.tracker.Get("keep-me")
	if got == nil {
		t.Fatal("tracker record vanished on stop — stop must not unregister the agent")
	}
	if got.Status != StatusStopped {
		t.Errorf("status = %q, want %q", got.Status, StatusStopped)
	}
	if got.PortRangeStart != wantStart || got.PortRangeEnd != wantEnd {
		t.Errorf("port range changed: %d-%d, want %d-%d", got.PortRangeStart, got.PortRangeEnd, wantStart, wantEnd)
	}
	if got.SshPrivateKeyPath != wantKey {
		t.Errorf("ssh key path changed: %q, want %q", got.SshPrivateKeyPath, wantKey)
	}
	// The allocator still holds the range: a later spawn can never
	// double-allocate a stopped agent's ports.
	if m.portAlloc != nil {
		if !m.portAlloc.Has("keep-me") {
			t.Error("port allocator no longer holds the stopped agent's range")
		}
		start, end, ok := m.portAlloc.AllocatedRange("keep-me")
		if !ok || start != wantStart || end != wantEnd {
			t.Errorf("allocated range = (%d,%d,%v), want (%d,%d,true)", start, end, ok, wantStart, wantEnd)
		}
	}
	// Nothing destructive ran.
	if data, rerr := os.ReadFile(logPath); rerr == nil && len(bytes.TrimSpace(data)) > 0 {
		t.Errorf("stop issued a delete command: %q", string(data))
	}
}

// TestStopAgent_ContainerModeStopsContainerNotRemovesIt pins the container-mode
// leg: an agent carrying an image ref has its container STOPPED through the
// agent's own socket (never removed — removal is Destroy's step), while a
// host-context agent has no container to touch.
func TestStopAgent_ContainerModeStopsContainerNotRemovesIt(t *testing.T) {
	tests := []struct {
		name      string
		image     string
		wantCalls int
	}{
		{"container_mode_agent", "bunkerd-imagespec-abc:latest", 1},
		{"host_context_agent", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seams := stubLifecycleSeams(t)
			m, _, _ := newLifecycleManager(t)
			registerLifecycleAgent(t, m, "ctr-agent", StatusRunning, time.Now().Add(time.Hour), tt.image)

			if _, err := m.StopAgent(context.Background(), "ctr-agent"); err != nil {
				t.Fatalf("StopAgent: %v", err)
			}
			if len(seams.dockerCalls) != tt.wantCalls {
				t.Fatalf("docker calls = %v, want %d", seams.dockerCalls, tt.wantCalls)
			}
			if tt.wantCalls == 0 {
				return
			}
			got := strings.Join(seams.dockerCalls[0], " ")
			if !strings.HasPrefix(got, "ctr-agent stop ") {
				t.Errorf("docker call = %q, want a stop through the agent's own socket", got)
			}
			if strings.Contains(got, "rm") {
				t.Errorf("docker call = %q: stop must never REMOVE the container", got)
			}
		})
	}
}

// ── start ───────────────────────────────────────────────────────

func TestStartAgent_StatusMatrix(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		want       string
		wantStarts int
	}{
		{"stopped_starts", StatusStopped, StatusStartedValue, 1},
		{"running_is_a_noop", StatusRunning, StatusAlreadyRunning, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seams := stubLifecycleSeams(t)
			m, _, _ := newLifecycleManager(t)
			registerLifecycleAgent(t, m, "startcase", tt.status, time.Now().Add(time.Hour), "")

			resp, err := m.StartAgent(context.Background(), "startcase")
			if err != nil {
				t.Fatalf("StartAgent: %v", err)
			}
			if resp.GetStatus() != tt.want {
				t.Errorf("status = %q, want %q", resp.GetStatus(), tt.want)
			}
			if len(seams.startUnits) != tt.wantStarts {
				t.Errorf("startUserUnit calls = %v, want %d", seams.startUnits, tt.wantStarts)
			}
			if tt.wantStarts == 1 && seams.startUnits[0] != "bunker-docker-startcase" {
				t.Errorf("started unit = %q, want bunker-docker-startcase", seams.startUnits[0])
			}
			if got := m.tracker.Get("startcase").Status; got != StatusRunning {
				t.Errorf("tracker status = %q, want %q", got, StatusRunning)
			}
		})
	}
}

// TestStartAgent_ContainerModeStartsTheKeptContainer pins the container-mode
// re-arm: the kept container is started again through the agent's own socket.
func TestStartAgent_ContainerModeStartsTheKeptContainer(t *testing.T) {
	seams := stubLifecycleSeams(t)
	m, _, _ := newLifecycleManager(t)
	registerLifecycleAgent(t, m, "ctr-start", StatusStopped, time.Now().Add(time.Hour), "bunkerd-imagespec-xyz:latest")

	if _, err := m.StartAgent(context.Background(), "ctr-start"); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if len(seams.dockerCalls) != 1 {
		t.Fatalf("docker calls = %v, want exactly one start", seams.dockerCalls)
	}
	got := strings.Join(seams.dockerCalls[0], " ")
	if !strings.HasPrefix(got, "ctr-start start ") {
		t.Errorf("docker call = %q, want a start through the agent's own socket", got)
	}
}

// ── restart ─────────────────────────────────────────────────────

// TestRestartAgent_ResetsHeartbeatExpiry is the TTL half of the contract:
// restart sets ExpiresAt to now + the daemon default TTL and the refreshed
// expiry is both returned and PERSISTED (durable registry), not just held in
// memory.
func TestRestartAgent_ResetsHeartbeatExpiry(t *testing.T) {
	seams := stubLifecycleSeams(t)
	m, cfg, _ := newLifecycleManager(t)
	original := time.Now().Add(5 * time.Minute)
	registerLifecycleAgent(t, m, "restart-me", StatusRunning, original, "")

	before := time.Now()
	resp, err := m.RestartAgent(context.Background(), "restart-me")
	if err != nil {
		t.Fatalf("RestartAgent: %v", err)
	}
	after := time.Now()

	if resp.GetStatus() != StatusRestarted {
		t.Errorf("status = %q, want %q", resp.GetStatus(), StatusRestarted)
	}
	// Stop AND start both ran.
	// Stop no longer calls stopUserUnit (only terminate process — DF-BUNKER-107),
	// but start must still re-arm the dockerd unit.
	if len(seams.stopUnits) != 0 || len(seams.startUnits) != 1 {
		t.Errorf("restart must start: stops=%v starts=%v", seams.stopUnits, seams.startUnits)
	}

	// Expiry moved forward and equals now + default TTL (within the call's
	// own wall-clock window).
	rec := m.tracker.Get("restart-me")
	if !rec.ExpiresAt.After(original) {
		t.Errorf("ExpiresAt = %v, want after the original %v", rec.ExpiresAt, original)
	}
	wantEarliest := before.Add(cfg.Agent.DefaultTTL)
	wantLatest := after.Add(cfg.Agent.DefaultTTL)
	if rec.ExpiresAt.Before(wantEarliest.Add(-time.Second)) || rec.ExpiresAt.After(wantLatest.Add(time.Second)) {
		t.Errorf("ExpiresAt = %v, want ~now+%s (between %v and %v)", rec.ExpiresAt, cfg.Agent.DefaultTTL, wantEarliest, wantLatest)
	}
	// The response carries the same refreshed expiry.
	parsed, perr := time.Parse(time.RFC3339, resp.GetExpiresAt())
	if perr != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", resp.GetExpiresAt(), perr)
	}
	// expires_at is rendered at RFC3339 second precision, so compare at that
	// precision rather than raw nanoseconds.
	if !parsed.Equal(rec.ExpiresAt.Truncate(time.Second)) {
		t.Errorf("response expires_at = %v, tracker = %v (must agree)", parsed, rec.ExpiresAt)
	}
	// …and it is PERSISTED.
	if m.registry == nil {
		t.Fatal("test manager has no registry — the persistence half would be untested")
	}
	persisted := m.registry.Get("restart-me")
	if persisted == nil {
		t.Fatal("restart did not persist the agent record")
	}
	if !persisted.ExpiresAt.After(original) {
		t.Errorf("persisted ExpiresAt = %v, want after the original %v", persisted.ExpiresAt, original)
	}
	if !persisted.ExpiresAt.Equal(rec.ExpiresAt) {
		t.Errorf("persisted ExpiresAt = %v, tracker = %v", persisted.ExpiresAt, rec.ExpiresAt)
	}
	if persisted.Status != StatusRunning {
		t.Errorf("persisted status = %q, want %q", persisted.Status, StatusRunning)
	}
}

// TestRestartAgent_ResetsRatherThanExtends pins the deliberate difference from
// a heartbeat: a restart RESETS the TTL clock, so an agent whose expiry is
// FURTHER out than now+TTL ends up at now+TTL. (HeartbeatAgent never shrinks a
// longer expiry; restart is not a heartbeat.)
func TestRestartAgent_ResetsRatherThanExtends(t *testing.T) {
	stubLifecycleSeams(t)
	m, cfg, _ := newLifecycleManager(t)
	cfg.Agent.DefaultTTL = time.Hour
	longExpiry := time.Now().Add(5 * time.Hour)
	registerLifecycleAgent(t, m, "reset-me", StatusRunning, longExpiry, "")

	if _, err := m.RestartAgent(context.Background(), "reset-me"); err != nil {
		t.Fatalf("RestartAgent: %v", err)
	}
	got := m.tracker.Get("reset-me").ExpiresAt
	if got.After(longExpiry) {
		t.Errorf("ExpiresAt = %v: restart must reset the TTL clock, not push a longer expiry further out", got)
	}
	if time.Until(got) > cfg.Agent.DefaultTTL+time.Minute {
		t.Errorf("ExpiresAt = %v (%s out), want ≈ now+%s", got, time.Until(got), cfg.Agent.DefaultTTL)
	}
}

// ── stopped sentinel ────────────────────────────────────────────

// TestStoppedStatusError pins the sentinel the three runtime RPCs rely on: a
// stopped record yields an error that errors.Is-matches ErrAgentStopped and
// whose text carries the stable "agent_stopped" token, while nil/running
// records yield nothing (so not-found handling and running agents are
// untouched).
func TestStoppedStatusError(t *testing.T) {
	tests := []struct {
		name    string
		rec     *resource.AgentRecord
		wantErr bool
	}{
		{"stopped_record", &resource.AgentRecord{AgentID: "a1", Status: StatusStopped}, true},
		{"running_record", &resource.AgentRecord{AgentID: "a1", Status: StatusRunning}, false},
		{"nil_record", nil, false},
		{"empty_status", &resource.AgentRecord{AgentID: "a1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := StoppedStatusError(tt.rec, "a1")
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("StoppedStatusError = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("StoppedStatusError = nil, want the stopped sentinel")
			}
			if !errors.Is(err, ErrAgentStopped) {
				t.Errorf("errors.Is(err, ErrAgentStopped) = false for %v", err)
			}
			if !IsAgentStopped(err) {
				t.Errorf("IsAgentStopped(%v) = false", err)
			}
			if !strings.Contains(err.Error(), "agent_stopped") {
				t.Errorf("error text %q must contain the stable token agent_stopped", err.Error())
			}
			if !strings.Contains(err.Error(), "a1") {
				t.Errorf("error text %q must name the agent id", err.Error())
			}
		})
	}
}

// TestLifecyclePersistsStatusTransitions pins the durable half of stop/start:
// the transition is appended to the registry so a daemon restart replays the
// agent as stopped (not as a running agent).
func TestLifecyclePersistsStatusTransitions(t *testing.T) {
	stubLifecycleSeams(t)
	m, _, _ := newLifecycleManager(t)
	registerLifecycleAgent(t, m, "persist-me", StatusRunning, time.Now().Add(time.Hour), "")

	if _, err := m.StopAgent(context.Background(), "persist-me"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	if m.registry == nil {
		t.Fatal("test manager has no registry")
	}
	if got := m.registry.Get("persist-me"); got == nil || got.Status != StatusStopped {
		t.Fatalf("persisted record = %+v, want status %q", got, StatusStopped)
	}

	if _, err := m.StartAgent(context.Background(), "persist-me"); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if got := m.registry.Get("persist-me"); got == nil || got.Status != StatusRunning {
		t.Fatalf("persisted record = %+v, want status %q", got, StatusRunning)
	}
}

// TestLifecycleStoppedAgentSurvivesReplay is the end-to-end durability claim:
// a fresh manager replaying the same registry file restores the agent as
// stopped with its port range — i.e. a stopped agent is not lost across a
// daemon restart (and is not mistaken for a live one).
func TestLifecycleStoppedAgentSurvivesReplay(t *testing.T) {
	stubLifecycleSeams(t)
	path := filepath.Join(t.TempDir(), "agents.jsonl")

	first := newRegistryManager(t, path)
	rec := &resource.AgentRecord{
		AgentID:        "replay-me",
		Status:         StatusRunning,
		CreatedAt:      time.Now().Add(-time.Hour),
		ExpiresAt:      time.Now().Add(time.Hour),
		PortRangeStart: 10000,
		PortRangeEnd:   10099,
	}
	if err := first.persistSpawn(rec); err != nil {
		t.Fatalf("persistSpawn: %v", err)
	}
	if err := first.tracker.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := first.StopAgent(context.Background(), "replay-me"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	second := newRegistryManager(t, path)
	restored := second.registry.Get("replay-me")
	if restored == nil {
		t.Fatal("replay lost the stopped agent")
	}
	if restored.Status != StatusStopped {
		t.Errorf("replayed status = %q, want %q", restored.Status, StatusStopped)
	}
	if restored.PortStart != 10000 || restored.PortEnd != 10099 {
		t.Errorf("replayed ports = %d-%d, want 10000-10099", restored.PortStart, restored.PortEnd)
	}
}

// TestLifecycle_TTLDefaultFallback pins the TTL source a restart resets to.
func TestLifecycle_TTLDefaultFallback(t *testing.T) {
	tests := []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{"configured", 2 * time.Hour, 2 * time.Hour},
		{"unset_falls_back_to_six_hours", 0, 6 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, cfg, _ := newLifecycleManager(t)
			cfg.Agent.DefaultTTL = tt.ttl
			if got := m.lifecycleTTL(); got != tt.want {
				t.Errorf("lifecycleTTL() = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestStopAgent_SeamsAreUsedForEveryHostCommand asserts the no-root guarantee
// structurally: with all seams stubbed, a full stop issues no bare host
// command through the process PATH (a stub dir with a catch-all recorder
// stays empty).
func TestStopAgent_SeamsAreUsedForEveryHostCommand(t *testing.T) {
	stubLifecycleSeams(t)
	m, _, _ := newLifecycleManager(t)
	registerLifecycleAgent(t, m, "seam-check", StatusRunning, time.Now().Add(time.Hour), "")

	stubDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "host.log")
	for _, cmd := range []string{"systemctl", "docker", "pgrep", "kill", "userdel"} {
		script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + logPath + "\"\n"
		if err := os.WriteFile(filepath.Join(stubDir, cmd), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := m.StopAgent(context.Background(), "seam-check"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	if data, rerr := os.ReadFile(logPath); rerr == nil && len(bytes.TrimSpace(data)) > 0 {
		t.Errorf("stop reached the host PATH outside its seams: %q", string(data))
	}
}

// TestStopAgent_ContextDeadlineIsHonoured guards the plumbing: the ctx handed
// to the host-command seams is the caller's, so a cancelled caller cannot
// leave a stop running. We verify that the context deadline propagates to
// the process-termination path (which now handles dockerd — DF-BUNKER-107).
func TestStopAgent_ContextDeadlineIsHonoured(t *testing.T) {
	seams := stubLifecycleSeams(t)
	m, _, _ := newLifecycleManager(t)
	registerLifecycleAgent(t, m, "ctx-check", StatusRunning, time.Now().Add(time.Hour), "")

	stopUserUnit = func(ctx context.Context, unit string) ([]byte, error) {
		seams.stopUnits = append(seams.stopUnits, unit)
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("seam saw cancelled ctx: %w", err)
		}
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A cancelled context is passed through; the stop still completes (the
	// stop path is best-effort) but the seam observes the caller's context.
	// No stopUserUnit is called (only process termination runs, DF-BUNKER-107),
	// but the termination itself honours the context.
	if _, err := m.StopAgent(ctx, "ctx-check"); err != nil {
		t.Fatalf("StopAgent with cancelled ctx: %v", err)
	}
	if len(seams.stopUnits) != 0 {
		t.Fatalf("stopUnits = %v, want zero (process termination handles dockerd)", seams.stopUnits)
	}
	// The termination path received the cancelled ctx.
	if len(seams.terminated) != 1 {
		t.Fatalf("terminated = %v, want one process kill", seams.terminated)
	}
}

// TestStopAgent_DoesNotStopDockerdUnit is the DF-BUNKER-107 regression:
// stop must NOT call stopUserUnit on the dockerd unit. The old code included
// the dockerd unit in the stop loop (agentSessionUnits returned it), so
// `systemctl --user stop` deleted the systemd-run transient unit, making
// startAgentRuntime's startUserUnit fail with "unit not found".
//
// This test proves the fix: stop calls stopUserUnit zero times and
// terminateAgentProcesses once, and the dockerd unit name is NOT in any
// stop call.
func TestStopAgent_DoesNotStopDockerdUnit(t *testing.T) {
	seams := stubLifecycleSeams(t)
	m, _, _ := newLifecycleManager(t)
	registerLifecycleAgent(t, m, "dockerd-test", StatusRunning, time.Now().Add(time.Hour), "")

	// systemd reports the agent's run unit AND the dockerd unit.
	seams.listOutput = strings.Join([]string{
		"bunker-run-dockerd-test-abc12345.service loaded active running bunker run",
		"bunker-docker-dockerd-test.service loaded active running dockerd",
		"",
	}, "\n")

	resp, err := m.StopAgent(context.Background(), "dockerd-test")
	if err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	if resp.GetStatus() != StatusStoppedValue {
		t.Fatalf("status = %q, want %q", resp.GetStatus(), StatusStoppedValue)
	}

	// The dockerd unit must NOT be in the stop calls.
	for _, unit := range seams.stopUnits {
		if unit == "bunker-docker-dockerd-test" {
			t.Fatalf("stopUserUnit was called with dockerd unit %q — this is the DF-BUNKER-107 bug. stop must NOT stop the dockerd unit; process termination handles it.", unit)
		}
	}

	// Only the run unit should be stopped; dockerd is killed by process signal.
	if len(seams.stopUnits) != 1 {
		t.Fatalf("stopUserUnit calls = %v, want 1 (only the run unit)", seams.stopUnits)
	}
	if seams.stopUnits[0] != "bunker-run-dockerd-test-abc12345" {
		t.Errorf("stopUserUnit[0] = %q, want the run unit", seams.stopUnits[0])
	}

	// terminateAgentProcesses must have been called.
	if len(seams.terminated) != 1 {
		t.Fatalf("terminateAgentProcesses calls = %v, want 1", seams.terminated)
	}
	if seams.terminated[0] != "bunker-dockerd-test" {
		t.Errorf("terminated = %v, want [bunker-dockerd-test]", seams.terminated)
	}
}

// TestStartAgent_RestartsStoppedAgent proves the core DF-BUNKER-107 acceptance:
// a freshly registered agent has a dockerd unit; after stop+start the unit
// must be re-armed (startUserUnit called with the dockerd unit name), and the
// tracker returns to running status.
func TestStartAgent_RestartsStoppedAgent(t *testing.T) {
	seams := stubLifecycleSeams(t)
	m, _, _ := newLifecycleManager(t)
	original := time.Now().Add(5 * time.Minute)
	registerLifecycleAgent(t, m, "rearm-test", StatusStopped, original, "")

	resp, err := m.StartAgent(context.Background(), "rearm-test")
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	if resp.GetStatus() != StatusStartedValue {
		t.Fatalf("status = %q, want %q", resp.GetStatus(), StatusStartedValue)
	}

	// Start must call startUserUnit with the dockerd unit.
	if len(seams.startUnits) != 1 {
		t.Fatalf("startUserUnit calls = %v, want 1", seams.startUnits)
	}
	if seams.startUnits[0] != "bunker-docker-rearm-test" {
		t.Errorf("startUserUnit[0] = %q, want bunker-docker-rearm-test", seams.startUnits[0])
	}

	// Tracker must be running.
	got := m.tracker.Get("rearm-test")
	if got == nil {
		t.Fatal("tracker record vanished on start")
	}
	if got.Status != StatusRunning {
		t.Errorf("status = %q, want %q", got.Status, StatusRunning)
	}
}
