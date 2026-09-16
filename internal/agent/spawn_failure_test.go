package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// intci5Manager builds a hermetic manager for the INT-CI-005 tests: temp
// registry, temp SSH dir, recorder-backed host provisioning. All real
// useradd/userdel/ssh-keygen invocations are intercepted via PATH stubs, so
// the tests never touch the host's users and never need root.
func intci5Manager(t *testing.T) *AgentManager {
	t.Helper()
	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	cfg.Agent.SSHDir = filepath.Join(t.TempDir(), "ssh")
	cfg.Agent.Isolation.SharedScratchRoot = filepath.Join(t.TempDir(), "srv/bunker-share")
	cfg.Agent.Isolation.PrivateTmpRoot = instanceRootFor(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
	t.Cleanup(func() { m.Stop() })
	m.hostRunner = newHostRecorder().run
	return m
}

// redirectBreadcrumbJournal points the spawn-failure journal at a temp file
// for the duration of the test.
func redirectBreadcrumbJournal(t *testing.T) string {
	t.Helper()
	journal := filepath.Join(t.TempDir(), "spawn-failures.jsonl")
	old := spawnFailureBreadcrumbPath
	spawnFailureBreadcrumbPath = journal
	t.Cleanup(func() { spawnFailureBreadcrumbPath = old })
	return journal
}

// writeStub writes an executable stub script into binDir and returns its path.
func writeStub(t *testing.T, binDir, name, body string) string {
	t.Helper()
	path := filepath.Join(binDir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// recordingStub returns a stub body that records its argv (one line per
// invocation) to logPath and exits with the given code.
func recordingStub(logPath string, exitCode int) string {
	return "printf '%s\\n' \"$(basename \"$0\") $*\" >> " + logPath + "\n" +
		"echo 'stub failure simulated by INT-CI-005 test' >&2\n" +
		"exit " + itoa(exitCode) + "\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// stubSucceeds is a stub body that records nothing and exits 0.
const stubSucceeds = "exit 0\n"

// readRecord reads a stub's recorded argv lines.
func readRecord(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// TestSpawnRollbackRunsUserdelUnderCancelledContext is the INT-CI-005
// criterion-1 proof, reproducing the CI run 35084607865 fingerprint: the user
// is created while the request context is still healthy, the spawn then hangs
// in a later stage, and the request context is cancelled (the chi
// middleware.Timeout 300s deadline) BEFORE the rollback runs. The rollback
// must still invoke `userdel` through the DETACHED rollback context, and the
// returned error must name the stage. The cancelled row reddens against the
// old behaviour: there the compensating exec borrowed the cancelled request
// ctx, died instantly, and the stub was never invoked.
func TestSpawnRollbackRunsUserdelUnderCancelledContext(t *testing.T) {
	t.Run("cancelled mid-spawn after user creation", func(t *testing.T) {
		m := intci5Manager(t)
		journal := redirectBreadcrumbJournal(t)

		binDir := t.TempDir()
		useraddLog := filepath.Join(t.TempDir(), "useradd-calls")
		userdelLog := filepath.Join(t.TempDir(), "userdel-calls")
		// useradd succeeds and records (the user IS created, like in CI).
		writeStub(t, binDir, "useradd", recordingStub(useraddLog, 0))
		// userdel succeeds and records (the compensating action under test).
		writeStub(t, binDir, "userdel", recordingStub(userdelLog, 0))
		// ssh-keygen sleeps far longer than the request budget, so the
		// request ctx is cancelled while it runs — then it dies with a
		// context error, exactly like a spawn hung past the 300s deadline.
		writeStub(t, binDir, "ssh-keygen", "sleep 2\nexit 1\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		restore := lookupAgentUser
		lookupAgentUser = func(username string) (*user.User, error) {
			return &user.User{Username: username, Uid: "1001", Gid: "1001", HomeDir: "/home/" + username}, nil
		}
		defer func() { lookupAgentUser = restore }()

		// The "request": cancelled after 50ms — while ssh-keygen is still
		// sleeping. All pre-keygen steps complete within a few ms.
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		agentID := uniqueAgentID("intci5-cancel")
		_, err := m.Spawn(ctx, &v1.SpawnAgentRequest{AgentId: agentID, Ttl: "1h"})
		if err == nil {
			t.Fatal("Spawn() succeeded although ssh-keygen was stubbed to hang and the ctx was cancelled")
		}

		// Premise: the user was really created before the failure.
		if calls := readRecord(t, useraddLog); len(calls) == 0 {
			t.Fatalf("test premise broken: useradd stub never ran (calls: %v)", calls)
		}

		// (b) The error names the stage, keeps the cause, and attributes the
		// server request timeout for the cancelled request context.
		if !strings.Contains(err.Error(), "failed at stage "+StageKeygen) {
			t.Errorf("error does not name the keygen stage: %v", err)
		}
		if !strings.Contains(err.Error(), "cancelled by the server request timeout") {
			t.Errorf("cancelled spawn error does not attribute the request timeout: %v", err)
		}

		// (a) The rollback INVOKED userdel even though the request ctx was
		// already cancelled — this is the assertion that reddens under the
		// old exec.CommandContext(ctx, ...) behaviour.
		want := "userdel -r bunker-" + agentID
		found := false
		for _, c := range readRecord(t, userdelLog) {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("rollback never invoked %q (compensating exec used a cancelled context?)", want)
		}

		// The breadcrumb landed exactly once and carries agent_id + stage.
		raw, readErr := os.ReadFile(journal)
		if readErr != nil {
			t.Fatalf("no spawn-failure breadcrumb was written: %v", readErr)
		}
		var lines []string
		for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if l != "" {
				lines = append(lines, l)
			}
		}
		if len(lines) != 1 {
			t.Fatalf("breadcrumb journal has %d lines, want exactly 1: %q", len(lines), lines)
		}
		var bc map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &bc); err != nil {
			t.Fatalf("breadcrumb is not a JSON line: %v (%q)", err, lines[0])
		}
		if bc["agent_id"] != agentID {
			t.Errorf("breadcrumb agent_id = %v, want %q", bc["agent_id"], agentID)
		}
		if bc["stage"] != StageKeygen {
			t.Errorf("breadcrumb stage = %v, want %q", bc["stage"], StageKeygen)
		}
		if _, ok := bc["timestamp"]; !ok {
			t.Errorf("breadcrumb missing UTC timestamp: %v", bc)
		}
	})

	t.Run("healthy context rolls back unchanged", func(t *testing.T) {
		m := intci5Manager(t)
		redirectBreadcrumbJournal(t)

		binDir := t.TempDir()
		userdelLog := filepath.Join(t.TempDir(), "userdel-calls")
		writeStub(t, binDir, "useradd", stubSucceeds)
		writeStub(t, binDir, "userdel", recordingStub(userdelLog, 0))
		writeStub(t, binDir, "ssh-keygen", "echo 'keygen: failure simulated' >&2\nexit 1\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		restore := lookupAgentUser
		lookupAgentUser = func(username string) (*user.User, error) {
			return &user.User{Username: username, Uid: "1001", Gid: "1001", HomeDir: "/home/" + username}, nil
		}
		defer func() { lookupAgentUser = restore }()

		agentID := uniqueAgentID("intci5-healthy")
		_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: agentID, Ttl: "1h"})
		if err == nil {
			t.Fatal("Spawn() unexpectedly succeeded")
		}
		if !strings.Contains(err.Error(), "failed at stage "+StageKeygen) {
			t.Errorf("error does not name the keygen stage: %v", err)
		}
		if strings.Contains(err.Error(), "request timeout") {
			t.Errorf("healthy-context error wrongly blames the request timeout: %v", err)
		}
		if calls := readRecord(t, userdelLog); len(calls) == 0 {
			t.Error("rollback never invoked userdel on the healthy-context failure path")
		}
	})
}

// TestSpawnFailureBreadcrumbCancelledSpawn is the INT-CI-005 criterion-2
// proof for a spawn cancelled at its FIRST side-effecting stage: the journal
// contains exactly one JSON line with the expected agent_id, the failing
// stage, the context error, the rollback outcome, and the error text names
// both the stage and the server request timeout.
func TestSpawnFailureBreadcrumbCancelledSpawn(t *testing.T) {
	m := intci5Manager(t)
	journal := redirectBreadcrumbJournal(t)

	binDir := t.TempDir()
	writeStub(t, binDir, "useradd", "echo 'useradd: failure simulated' >&2\nexit 1\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	agentID := uniqueAgentID("intci5-bc")
	_, err := m.Spawn(ctx, &v1.SpawnAgentRequest{AgentId: agentID, Ttl: "1h"})
	if err == nil {
		t.Fatal("Spawn() unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "failed at stage "+StageUserCreate) {
		t.Errorf("error text does not contain the stage name: %v", err)
	}

	raw, readErr := os.ReadFile(journal)
	if readErr != nil {
		t.Fatalf("breadcrumb journal missing: %v", readErr)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("journal must hold exactly one JSON line, got %d: %q", len(lines), lines)
	}

	var bc map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &bc); err != nil {
		t.Fatalf("breadcrumb is not valid JSON: %v", err)
	}
	if bc["agent_id"] != agentID {
		t.Errorf("agent_id = %v, want %q", bc["agent_id"], agentID)
	}
	if bc["stage"] != StageUserCreate {
		t.Errorf("stage = %v, want %q", bc["stage"], StageUserCreate)
	}
	if got, _ := bc["context_error"].(string); got != context.Canceled.Error() {
		t.Errorf("context_error = %v, want %q", bc["context_error"], context.Canceled.Error())
	}
	if got, _ := bc["error"].(string); !strings.Contains(got, "failed at stage "+StageUserCreate) {
		t.Errorf("breadcrumb error does not name the stage: %v", got)
	}
	ranList, _ := bc["rollback_ran"].([]any)
	var ranStrings []string
	for _, r := range ranList {
		if s, ok := r.(string); ok {
			ranStrings = append(ranStrings, s)
		}
	}
	// No user was created (useradd failed), so the correct compensating set
	// is the port range and the isolation dirs — userdel must NOT be claimed.
	for _, want := range []string{"port-range freed", "isolation removed"} {
		found := false
		for _, r := range ranStrings {
			if r == want {
				found = true
			}
		}
		if !found {
			t.Errorf("rollback_ran %v is missing %q", ranStrings, want)
		}
	}
	for _, r := range ranStrings {
		if strings.HasPrefix(r, "userdel") {
			t.Errorf("rollback_ran claims a userdel for a spawn that never created the user: %v", ranStrings)
		}
	}
	failedList, _ := bc["rollback_failed"].([]any)
	if len(failedList) != 0 {
		t.Errorf("rollback_failed = %v, want empty for this shape", failedList)
	}
	ts, _ := bc["timestamp"].(string)
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Errorf("breadcrumb timestamp %q is not RFC3339: %v", ts, err)
	}
}

// TestRemoveAgentUserRetryTable pins the userdel second-pass hardening inside
// the detached rollback context: a first-pass failure caused by live user
// processes terminates them and retries once; a persistent failure is bounded
// at two attempts and recorded loudly; a cancelled context never runs the
// compensating exec at all.
func TestRemoveAgentUserRetryTable(t *testing.T) {
	t.Run("retry after killing processes succeeds", func(t *testing.T) {
		binDir := t.TempDir()
		logPath := filepath.Join(t.TempDir(), "userdel-calls")
		// First call fails ("user is currently used by process"), second succeeds.
		writeStub(t, binDir, "userdel",
			"printf '%s\\n' \"$(basename \"$0\") $*\" >> "+logPath+"\n"+
				"c=$(cat "+logPath+".count 2>/dev/null || echo 0)\n"+
				"c=$((c+1))\necho $c > "+logPath+".count\n"+
				"if [ \"$c\" = \"1\" ]; then echo 'userdel: user is currently used by process' >&2; exit 8; fi\n"+
				"exit 0\n")
		writeStub(t, binDir, "pkill", stubSucceeds)
		writeStub(t, binDir, "pgrep", "exit 1\n") // no processes left
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		res := &rollbackResult{}
		rollbackCtx, cancel := rollbackContext(context.Background())
		defer cancel()
		removeAgentUser(rollbackCtx, "retry-agent", logger, res)

		calls := readRecord(t, logPath)
		if len(calls) != 2 {
			t.Fatalf("userdel invoked %d times, want exactly 2 (first pass + retry): %v", len(calls), calls)
		}
		if len(res.failed) != 0 {
			t.Errorf("retry succeeded but failures were recorded: %v", res.failed)
		}
		ranList, _ := res.snapshot()
		found := false
		for _, r := range ranList {
			if strings.HasPrefix(r, "userdel bunker-retry-agent") {
				found = true
			}
		}
		if !found {
			t.Errorf("rollbackResult does not record the successful userdel: %v", ranList)
		}
	})

	t.Run("persistent failure is bounded and recorded", func(t *testing.T) {
		binDir := t.TempDir()
		logPath := filepath.Join(t.TempDir(), "userdel-calls")
		writeStub(t, binDir, "userdel", recordingStub(logPath, 8))
		writeStub(t, binDir, "pkill", stubSucceeds)
		writeStub(t, binDir, "pgrep", "exit 1\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		res := &rollbackResult{}
		rollbackCtx, cancel := rollbackContext(context.Background())
		defer cancel()
		removeAgentUser(rollbackCtx, "stuck-agent", logger, res)

		if calls := readRecord(t, logPath); len(calls) != 2 {
			t.Fatalf("userdel invoked %d times, want bounded 2 attempts: %v", len(calls), calls)
		}
		if len(res.failed) == 0 {
			t.Errorf("persistent userdel failure was not recorded as failed")
		}
	})

	t.Run("cancelled context runs no compensating exec", func(t *testing.T) {
		binDir := t.TempDir()
		logPath := filepath.Join(t.TempDir(), "userdel-calls")
		writeStub(t, binDir, "userdel", recordingStub(logPath, 0))
		writeStub(t, binDir, "pkill", stubSucceeds)
		writeStub(t, binDir, "pgrep", "exit 1\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		res := &rollbackResult{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		removeAgentUser(ctx, "ghost-agent", logger, res)

		if calls := readRecord(t, logPath); len(calls) != 0 {
			t.Errorf("userdel ran under a cancelled context (stubs: %v) — the rollback ctx must be detached", calls)
		}
		if len(res.failed) == 0 {
			t.Error("the dead rollback attempt was not recorded as failed")
		}
	})
}

// TestSpawnStageErrWrapsCauseAndNamesTimeout pins the error-text contract:
// stage attribution always; the request-timeout attribution only when the
// request context is actually cancelled/expired; the cause substring always.
func TestSpawnStageErrWrapsCauseAndNamesTimeout(t *testing.T) {
	cause := fmt.Errorf("useradd bunker-x failed: boom")
	t.Run("cancelled context names the server request timeout", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := spawnStageErr(ctx, "agent-a", StageUserCreate, cause)
		for _, want := range []string{
			"spawn agent-a failed at stage user-create",
			"cancelled by the server request timeout",
			"request_timeout",
			"300s",
			"useradd bunker-x failed: boom",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	})
	t.Run("healthy context keeps stage and cause without timeout text", func(t *testing.T) {
		err := spawnStageErr(context.Background(), "agent-a", StageKeygen, cause)
		if !strings.Contains(err.Error(), "spawn agent-a failed at stage keygen") {
			t.Errorf("error %q does not name the stage", err)
		}
		if strings.Contains(err.Error(), "request timeout") {
			t.Errorf("healthy-context error wrongly blames the request timeout: %v", err)
		}
	})
	t.Run("empty agent id gets a placeholder", func(t *testing.T) {
		err := spawnStageErr(context.Background(), "", StageValidate, cause)
		if !strings.Contains(err.Error(), "spawn <unassigned> failed at stage validate") {
			t.Errorf("error %q does not use the placeholder", err)
		}
	})
}

// TestSpawnStagesCoverNamedStages pins the stage vocabulary so a rename cannot
// silently break operator tooling that greps the journal or error text.
func TestSpawnStagesCoverNamedStages(t *testing.T) {
	want := map[string]string{
		"validate":            StageValidate,
		"capacity":            StageCapacity,
		"port-alloc":          StagePortAlloc,
		"user-create":         StageUserCreate,
		"isolation-provision": StageIsolationProvision,
		"keygen":              StageKeygen,
		"authorized-keys":     StageAuthorizedKeys,
		"rootless-install":    StageRootlessInstall,
		"dockerd-start":       StageDockerdStart,
		"container-cap":       StageContainerCap,
		"image-build":         StageImageBuild,
		"slice-limits":        StageSliceLimits,
		"register":            StageRegister,
	}
	for name, constant := range want {
		if constant != name {
			t.Errorf("stage constant %q drifted to %q", name, constant)
		}
	}
}

// keep the user import referenced when the table above evolves.
var _ = user.Lookup
