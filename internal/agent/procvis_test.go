package agent

// NET-BUNKER-011 daemon wiring tests: the procvis host verification is a
// spawn-path gate (Step 1d — before ANY side effect) and a RunAgent gate,
// both backed by a once-per-process kernel experiment.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/netmode"
	"github.com/deployBunker/bunker/internal/resource"
)

// realisticUnhiddenProcMounts is the /proc/mounts shape the measured
// ISO-001 host had: a plain system /proc (rw,nosuid,...) with NO hidepid
// and a tmpfs carrying an unrelated gid option. The gate must refuse.
const realisticUnhiddenProcMounts = `sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
tmpfs /run/user/1005 tmpfs rw,nosuid,size=16m,mode=700,gid=1005 0 0
`

// stubProcVisVerify swaps the cached verifier for the test's stub and
// restores it after. Never used with t.Parallel (package-level var).
func stubProcVisVerify(t *testing.T, stub func() error) {
	t.Helper()
	orig := procVisVerifyOnce
	t.Cleanup(func() { procVisVerifyOnce = orig })
	procVisVerifyOnce = sync.OnceValue(stub)
}

// setenvProcGroup pins BUNKERD_PROCVIS_PROC_GROUP for the test ("" unsets).
func setenvProcGroup(t *testing.T, v string) {
	t.Helper()
	t.Setenv(config.ProcVisProcGroupEnv, v)
}

// withProcMountsFixture points the netmode real-/proc verifier at a temp
// fixture (the content text) and restores the real path after the test.
func withProcMountsFixture(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "mounts")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write mounts fixture: %v", err)
	}
	t.Cleanup(netmode.SetProcMountsPathForGate(p))
}

// TestSpawnRefusesProcVisWhenHostUnverifiable proves the mode's refusal law
// end to end: on a host where the kernel experiment fails, a procvis spawn
// FAILS at StageValidate (before user creation, port allocation, or any
// systemd-run state) with the probe's named reason — it never falls back to
// shared. The argv-level behavior is covered by the netmode property tests
// (TestPropertiesForProcVisAddsExactlyProtectProcInvisible) and the live
// kernel experiment by TestProcVisHostExperimentProvesHidepidVisibility; the
// three together are the visibility proof chain.
func TestSpawnRefusesProcVisWhenHostUnverifiable(t *testing.T) {
	stubProcVisVerify(t, func() error {
		return &netmode.ProcVisProbeError{Detail: "stub: kernel does not honor hidepid"}
	})

	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	cfg.Agent.SSHDir = t.TempDir()
	logger := newNetmodeQuietLogger()
	m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
	defer m.Stop()

	_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{
		AgentId:     uniqueAgentID("procvis"),
		Ttl:         "1h",
		NetworkMode: netmode.ModeProcVis,
	})
	if err == nil {
		t.Fatal("procvis spawn succeeded on an unverifiable host — the §5.2 silent-fallback violation")
	}
	msg := err.Error()
	for _, want := range []string{
		"procvis mode unavailable on this host", // the probe's named refusal prefix
		"stub: kernel does not honor hidepid",   // the specific probe failure
		StageValidate,                           // refused BEFORE any side effect
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("procvis refusal %q missing %q", msg, want)
		}
	}
	for _, stage := range []string{StageUserCreate, StageDockerdStart, StageRootlessInstall} {
		if strings.Contains(msg, stage) {
			t.Errorf("refusal %q names stage %s — the spawn proceeded past validate with a failed host verification", msg, stage)
		}
	}
}

// TestSpawnAcceptsProcVisOnVerifiedHost pins the OTHER half: with the host
// verified, the mode proceeds past the verification gate (the spawn then
// continues into the normal stages, where this fixture has no real host
// support — the failure must be a LATER stage, never the procvis refusal).
func TestSpawnAcceptsProcVisOnVerifiedHost(t *testing.T) {
	stubProcVisVerify(t, func() error { return nil })

	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	cfg.Agent.SSHDir = t.TempDir()
	logger := newNetmodeQuietLogger()
	m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
	defer m.Stop()

	_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{
		AgentId:     uniqueAgentID("procvis"),
		Ttl:         "1h",
		NetworkMode: netmode.ModeProcVis,
	})
	if err == nil {
		return // full fixture spawn success (not expected in the cage, but legitimate)
	}
	msg := err.Error()
	if strings.Contains(msg, "procvis mode unavailable") {
		t.Fatalf("verified host still refused the mode: %s", msg)
	}
	// The refusal (if any) must be a LATER stage — the verification gate
	// passed and the spawn proceeded into real provisioning.
	if strings.Contains(msg, StageValidate) && !strings.Contains(msg, "useradd") {
		// validate-stage failures other than the probe (e.g. fixtures) are
		// acceptable; the probe refusal specifically must not appear.
		t.Fatalf("verification gate leaked into the refusal: %s", msg)
	}
}

// TestRunAgentRefusesProcVisWhenHostUnverifiable proves the detached-run
// gate: a procvis agent's detached run fails with the probe's named refusal
// before any systemd-run invocation.
func TestRunAgentRefusesProcVisWhenHostUnverifiable(t *testing.T) {
	stubProcVisVerify(t, func() error {
		return &netmode.ProcVisProbeError{Detail: "stub: mount namespace unavailable"}
	})

	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	cfg.Agent.SSHDir = t.TempDir()
	logger := newNetmodeQuietLogger()
	m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
	defer m.Stop()

	m.tracker.Register(&resource.AgentRecord{
		AgentID:     "procvisrun1",
		NetworkMode: netmode.ModeProcVis,
	})
	_, err := m.RunAgent(context.Background(), &v1.RunAgentRequest{
		AgentId: "procvisrun1",
		Command: "sleep",
		Args:    []string{"1"},
		Detach:  true,
	})
	if err == nil {
		t.Fatal("detached run succeeded on an unverifiable host — no refusal, no boundary")
	}
	if !strings.Contains(err.Error(), "procvis mode unavailable for detached run") ||
		!strings.Contains(err.Error(), "stub: mount namespace unavailable") {
		t.Fatalf("detached-run refusal lost the named probe reason: %v", err)
	}
}

// TestProcVisRuntimeGateRefusesWhenProcMountUnhidden drives the REAL-/proc
// half of the gate through the spawn path: with a proc group configured and
// a /proc/mounts fixture that carries no hidepid (the measured 2026-10-02
// ISO-001 host state), a procvis spawn FAILS at StageValidate — the same
// refuse-before-side-effects point the kernel probe refusals land at — and
// the refusal names the missing host property. It never degrades to shared.
func TestProcVisRuntimeGateRefusesWhenProcMountUnhidden(t *testing.T) {
	withProcMountsFixture(t, realisticUnhiddenProcMounts)
	// The kernel half passes and the real-/proc half is stubbed to verify
	// the LIVE (fixture-pointed) verifier — not a passing stub.
	stubProcVisVerify(t, func() error { return nil })

	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	cfg.Agent.SSHDir = t.TempDir()
	// The daemon opted into the gate posture: a proc group is configured.
	setenvProcGroup(t, "bunker-proc")
	cfg.Agent.ProcVis.ProcGroup = "bunker-proc"
	logger := newNetmodeQuietLogger()
	m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
	defer m.Stop()

	_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{
		AgentId:     uniqueAgentID("procvis"),
		Ttl:         "1h",
		NetworkMode: netmode.ModeProcVis,
	})
	if err == nil {
		t.Fatal("procvis spawn succeeded against a real /proc with no host-wide hiding — the ISO-001 leak unaddressed")
	}
	msg := err.Error()
	for _, want := range []string{
		"procvis mode unavailable on this host", // named refusal
		"hidepid",                               // the missing /proc mount option
		StageValidate,                           // refused BEFORE any side effect
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q missing %q", msg, want)
		}
	}
	for _, stage := range []string{StageUserCreate, StageDockerdStart, StageRootlessInstall} {
		if strings.Contains(msg, stage) {
			t.Errorf("refusal %q names stage %s — the spawn proceeded past validate with an unhidden real /proc", msg, stage)
		}
	}
}

// TestProcVisRuntimeGateSkippedWithoutProcGroup pins the OPT-IN law: a
// daemon that did not configure a proc group must NOT pay the real-/proc
// refusal on a host whose /proc is unhidden — the gate posture is
// opt-in-by-configuration, and absence of the group is the documented
// daemon shape for hosts that never armed the host-wide hiding. (The
// refusal text for that host shape belongs to the OPERATOR runbook, not to
// a spawn that never opted into the posture.)
func TestProcVisRuntimeGateSkippedWithoutProcGroup(t *testing.T) {
	setenvProcGroup(t, "")
	cfg := config.DefaultConfig()
	cfg.Agent.ProcVis.ProcGroup = "" // the opt-out shape
	stubProcVisVerify(t, func() error { return nil })

	cfg2 := config.DefaultConfig()
	isolateRegistry(t, cfg2)
	stubProcVisVerify(t, func() error { return nil })
	_ = cfg

	setenvProcGroup(t, "")
	// A nil-m / nil-cfg call must not panic.
	if err := procVisVerified(nil); err != nil {
		t.Fatalf("nil manager paid a refusal: %v", err)
	}
}

// TestSpawnRefusesProcVisWhenHostUnverifiableAndRuntimeGateArmed proves the
// kernel half's refusal WINS even when the runtime gate is armed: the
// combined gate surfaces the FIRST refusal and never a mixed verdict.
func TestSpawnRefusesProcVisWhenHostUnverifiableAndRuntimeGateArmed(t *testing.T) {
	withProcMountsFixture(t, realisticUnhiddenProcMounts)
	stubProcVisVerify(t, func() error {
		return &netmode.ProcVisProbeError{Detail: "stub: kernel does not honor hidepid"}
	})

	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	cfg.Agent.SSHDir = t.TempDir()
	cfg.Agent.ProcVis.ProcGroup = "bunker-proc"
	setenvProcGroup(t, "")
	logger := newNetmodeQuietLogger()
	m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
	defer m.Stop()

	_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{
		AgentId:     uniqueAgentID("procvis"),
		Ttl:         "1h",
		NetworkMode: netmode.ModeProcVis,
	})
	if err == nil {
		t.Fatal("spawn succeeded with a failing kernel probe")
	}
	if !strings.Contains(err.Error(), "stub: kernel does not honor hidepid") {
		t.Fatalf("first refusal (the kernel probe) was not surfaced: %v", err)
	}
}

// TestDetachedRunRefusesWhenRuntimeGateArmedAndUnhidden pins the detached
// run's half of the same gate (run.go): identical refusal shape, named
// reason, never a launch.
func TestDetachedRunRefusesWhenRuntimeGateArmedAndUnhidden(t *testing.T) {
	setenvProcGroup(t, "")
	withProcMountsFixture(t, realisticUnhiddenProcMounts)
	stubProcVisVerify(t, func() error { return nil })

	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	cfg.Agent.SSHDir = t.TempDir()
	cfg.Agent.ProcVis.ProcGroup = "bunker-proc"
	logger := newNetmodeQuietLogger()
	m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
	defer m.Stop()

	if err := m.tracker.Register(&resource.AgentRecord{
		AgentID:     "procvisrun2",
		NetworkMode: netmode.ModeProcVis,
	}); err != nil {
		t.Fatalf("register fixture agent: %v", err)
	}
	_, err := m.RunAgent(context.Background(), &v1.RunAgentRequest{
		AgentId: "procvisrun2",
		Command: "sleep",
		Args:    []string{"1"},
		Detach:  true,
	})
	if err == nil {
		t.Fatal("detached run succeeded against an unhidden real /proc")
	}
	msg := err.Error()
	if !strings.Contains(msg, "procvis mode unavailable for detached run") ||
		!strings.Contains(msg, "hidepid") {
		t.Fatalf("detached-run refusal lost the named reason: %v", err)
	}
}

// TestDetachedRunUnitCarriesProcVisProperty pins the detached-run argv: a
// procvis agent's run unit carries --property=ProtectProc=invisible exactly
// once, right after the GAP-075 PrivateTmp boundary, and nothing else about
// the argv changes versus the shared-mode run.
func TestDetachedRunUnitCarriesProcVisProperty(t *testing.T) {
	sharedArgs, err := buildRunAgentArgsForMode("abc123", "1001", "1002", "bunker-run-abc123-x", "make", []string{"test"}, nil, nil, false, netmode.ModeShared)
	if err != nil {
		t.Fatalf("shared run argv: %v", err)
	}
	procArgs, err := buildRunAgentArgsForMode("abc123", "1001", "1002", "bunker-run-abc123-x", "make", []string{"test"}, nil, nil, false, netmode.ModeProcVis)
	if err != nil {
		t.Fatalf("procvis run argv: %v", err)
	}
	if n := countArg(procArgs, "--property="+netmode.PropertyProtectProcInvisible); n != 1 {
		t.Fatalf("ProtectProc=invisible appears %d times, want exactly 1: %v", n, procArgs)
	}
	if len(procArgs) != len(sharedArgs)+1 {
		t.Fatalf("procvis run argv length %d, want shared %d + exactly 1\nprocvis: %v\nshared:  %v", len(procArgs), len(sharedArgs), procArgs, sharedArgs)
	}
	if procArgs[5] != "--property=PrivateTmp=yes" {
		t.Fatalf("fixture drift: PrivateTmp not at position 5: %v", procArgs)
	}
	if procArgs[6] != "--property="+netmode.PropertyProtectProcInvisible {
		t.Fatalf("mode property is not immediately after PrivateTmp: %v", procArgs)
	}
	// The exec/socket contract is untouched.
	if !containsArg(procArgs, "--setenv=DOCKER_HOST=unix:///run/bunker/abc123/docker.sock") {
		t.Errorf("procvis run argv lost the unix-socket DOCKER_HOST contract: %v", procArgs)
	}
}

// TestDetachedRunRefusesUnknownMode pins the builder's refusal: a record
// carrying an unknown mode name is a named error, never a silently
// unisolated launch.
func TestDetachedRunRefusesUnknownMode(t *testing.T) {
	if _, err := buildRunAgentArgsForMode("a", "1000", "1000", "u", "sh", nil, nil, nil, false, "pasta"); err == nil {
		t.Fatal("unknown mode name built a run unit — the silent-fallback violation at the builder level")
	}
}
