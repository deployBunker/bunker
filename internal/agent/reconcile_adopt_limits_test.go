package agent

// DF-BUNKER-53 regression tests: adoptAgent must apply the agent's persisted
// isolation knobs to the host, exactly like a fresh spawn.
//
// The defect (live-evidenced on cube-las-00 agent 2cdce4d0): adoption rebuilt
// the tracker record from m.defaultLimits() and never touched the host —
// `systemctl show user-<uid>.slice` reported CPUQuotaPerSecUSec=infinity and
// MemoryMax=infinity, with no user-<uid>.slice.d drop-in and no
// bunker-docker-<agentID> unit carrying limits. An adopted agent was REPORTED
// with limits while ZERO were enforced.
//
// The fix contracts pinned here:
//
//  1. limits come from the agent's OWN persisted record (the durable registry
//     spawn wrote), never from current config defaults — an agent adopted
//     after an operator raised the config must keep reporting (and applying)
//     what it was spawned with;
//  2. the user-slice knob stage runs with those persisted values, and the
//     per-agent docker unit is created through the same systemd-run shape a
//     fresh spawn uses (unit bunker-docker-<agentID>, CPUQuota/MemoryMax/
//     LimitFSIZE properties) — both through injectable seams, so nothing here
//     ever touches a real host;
//  3. failure of either host stage fails adoption loudly, with NO tracker
//     record and NO port reservation left behind (the existing rollback
//     discipline; Reconcile then destroys the orphan).
//
// Every test injects both seams: no systemd, no users, no sockets.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/registry"
)

// knobStageRecorder records every call to the injected user-slice knob stage.
type knobStageRecorder struct {
	mu sync.Mutex

	// calls carries one entry per invocation: the uid the slice drop-in was
	// destined for, and the resolved limits the stage was handed.
	calls []sliceStageCall

	// fail, when non-nil, is returned by every invocation (after recording).
	fail error
}

type sliceStageCall struct {
	uid      string
	cpuQuota float64
	memMax   uint64
	diskMax  uint64
	maxProcs uint64
	maxFiles uint64
}

func (r *knobStageRecorder) stage() func(context.Context, *user.User, float64, uint64, uint64, uint64, uint64, []SystemdKnob, containmentResolved) (string, error) {
	return func(_ context.Context, u *user.User, cpuQuota float64, memMax, diskMax, maxProcs, maxFiles uint64, _ []SystemdKnob, _ containmentResolved) (string, error) {
		r.mu.Lock()
		r.calls = append(r.calls, sliceStageCall{
			uid:      u.Uid,
			cpuQuota: cpuQuota,
			memMax:   memMax,
			diskMax:  diskMax,
			maxProcs: maxProcs,
			maxFiles: maxFiles,
		})
		fail := r.fail
		r.mu.Unlock()
		if fail != nil {
			return "", fail
		}
		return "[Slice]\nCPUQuota=100%", nil
	}
}

func (r *knobStageRecorder) all() []sliceStageCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sliceStageCall(nil), r.calls...)
}

// dockerUnitRecorder records every call to the injected docker-unit runner.
type dockerUnitRecorder struct {
	mu sync.Mutex

	units []string
	props []string // concatenated NAME=VALUE properties, in argv order

	// fail, when non-nil, is returned by every invocation (after recording).
	fail error
}

func (r *dockerUnitRecorder) runner() func(ctx context.Context, unitName, uid, gid string, knobs []SystemdKnob) error {
	return func(_ context.Context, unitName, uid, gid string, knobs []SystemdKnob) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.units = append(r.units, unitName)
		var props []string
		for _, k := range knobs {
			props = append(props, k.Name+"="+k.Value)
		}
		r.props = append(r.props, strings.Join(props, " "))
		return r.fail
	}
}

func (r *dockerUnitRecorder) snapshot() (units, props []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.units...), append([]string(nil), r.props...)
}

// adoptFixture wires everything the adoption tests need: a manager in adopt
// mode whose agent was spawned by a FIRST daemon (the real persistSpawn path,
// so the on-disk record has the exact production shape), then restarted as a
// SECOND daemon that no longer knows the agent (the orphan precondition) but
// still points at the same registry file.
type adoptFixture struct {
	m        *AgentManager
	destroy  *destroyRecorder
	knobs    *knobStageRecorder
	units    *dockerUnitRecorder
	home     string
	registry *registry.Store
}

// newAdoptFixture builds the orphan shape honestly: the second daemon opens
// an EMPTY durable store (its live fold knows no agent — the adopt walk's
// precondition), and the agent's production-shaped KindSpawn record (the
// event a real spawn's persistSpawn appends) is written to the SAME file by
// a third store handle behind that fold. The in-memory fold never learns the
// agent, so reconciliation sees an orphan; the on-disk record — what
// readPersistedAgentRecord consumes — carries the limits the agent was
// spawned with.
func newAdoptFixture(t *testing.T, agentID string, limits *v1.ResourceLimits) *adoptFixture {
	t.Helper()

	// The durable store file, created empty by a throwaway handle.
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	seed := newRegistryManager(t, path)
	seed.registry.Close()

	// Second daemon: adopt mode over the same file; replay finds nothing.
	m := newRegistryManager(t, path)
	m.cfg.Agent.Reconciliation.Mode = config.ReconcileModeAdopt
	if m.portAlloc == nil {
		t.Fatal("test requires a configured port allocator")
	}

	// Append the agent's KindSpawn record behind the second daemon's fold.
	w, err := registry.Open(registry.Options{
		Path:     path,
		MaxBytes: 1 << 20,
		Logger:   m.logger,
	})
	if err != nil {
		t.Fatalf("open writer registry: %v", err)
	}
	if err := w.AppendSpawn(&registry.Record{
		AgentID:   agentID,
		Status:    "running",
		Limits:    limits,
		PortStart: 10000,
		PortEnd:   10099,
	}); err != nil {
		t.Fatalf("append spawn event: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer registry: %v", err)
	}

	f := &adoptFixture{
		m:       m,
		destroy: &destroyRecorder{},
		knobs:   &knobStageRecorder{},
		units:   &dockerUnitRecorder{},
	}
	f.registry = m.registry
	m.destroyAgent = f.destroy.seam()
	m.applyAdoptedSliceLimits = f.knobs.stage()
	m.runAdoptedDockerUnit = f.units.runner()
	// The adopt path resolves the agent's uid/gid through the package lookup
	// var; stub it so no test ever reads the host /etc/passwd.
	stubUser(t, "bunker-"+agentID)

	// A real orphan home with the exact spawn-written metadata (ports +
	// owner marker carrying THIS daemon's instance id, so the orphan is not
	// foreign) plus the .bunker metadata spawn writes.
	home := t.TempDir()
	writePortMetadata(t, home, "10000-10099\n")
	writeOwnerMarker(t, home, m.instanceID)
	f.home = home
	return f
}

// reopenRegistry reopens the manager's durable store after a Close (the
// registry Store cannot be reopened in place, so the field is re-wired the
// same way openRegistry does it).
func (f *adoptFixture) reopenRegistry(t *testing.T, path string) {
	t.Helper()
	m := f.m
	m.cfg.Agent.Registry.Path = path
	s, err := registry.Open(registry.Options{
		Path:       path,
		MaxBytes:   m.cfg.Agent.Registry.MaxBytes,
		MaxBackups: m.cfg.Agent.Registry.MaxBackups,
		KnownIDCap: m.cfg.Agent.Registry.KnownIDCap,
		Logger:     m.logger,
	})
	if err != nil {
		t.Fatalf("reopen registry: %v", err)
	}
	m.registry = s
	m.registryErr = nil
	f.registry = s
}

// stubUser replaces the agent-user lookup with a fake whose Uid/Gid are the
// fixed test values; restoreUser is the returned cleanup.
func stubUser(t *testing.T, username string) {
	t.Helper()
	prev := lookupAgentUser
	lookupAgentUser = func(name string) (*user.User, error) {
		if name != username {
			return nil, fmt.Errorf("user %q not found (test stub only knows %q)", name, username)
		}
		return &user.User{Username: username, Uid: "4242", Gid: "4242", HomeDir: "/home/" + username}, nil
	}
	t.Cleanup(func() { lookupAgentUser = prev })
}

// runReconcile drives one adopt-mode reconcile over the fixture's orphan and
// returns the report.
func (f *adoptFixture) runReconcile(t *testing.T, agentID, username string) ReconcileReport {
	t.Helper()
	f.m.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: agentID, Username: username, Home: f.home}}, nil
	}
	return f.m.Reconcile(context.Background())
}

// installNoopAdoptSeams wires recording no-op host seams onto a manager a
// pre-existing reconcile test built: without this the REAL systemd stages
// would run (and fail) on the test host. Returns the recorders so a test can
// additionally assert on the calls.
func installNoopAdoptSeams(t *testing.T, m *AgentManager) (*knobStageRecorder, *dockerUnitRecorder) {
	t.Helper()
	knobs := &knobStageRecorder{}
	units := &dockerUnitRecorder{}
	m.applyAdoptedSliceLimits = knobs.stage()
	m.runAdoptedDockerUnit = units.runner()
	return knobs, units
}

// writeOwnerMarker writes the `.bunker/owner` marker spawn would have
// stamped (instance id + informational pool line).
func writeOwnerMarker(t *testing.T, home, instanceID string) {
	t.Helper()
	dir := filepath.Join(home, ".bunker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	content := fmt.Sprintf("%s\n%d-%d\n", instanceID, 10000, 10099)
	if err := os.WriteFile(filepath.Join(dir, ownerMarkerFilename), []byte(content), 0o644); err != nil {
		t.Fatalf("write owner marker: %v", err)
	}
}

// ── Contract 1: persisted limits, not config defaults ─────────────

// TestAdopt_AppliesPersistedLimitsToHost is the DF-BUNKER-53 headline
// contract: adoption applies the agent's OWN persisted limits through both
// host stages (user-slice knobs + per-agent docker unit) and reports those
// same values — never the current config defaults. The config is deliberately
// raised above the persisted values so a regression back to defaultLimits()
// reads as the wrong numbers on every assertion.
func TestAdopt_AppliesPersistedLimitsToHost(t *testing.T) {
	persisted := &v1.ResourceLimits{
		CpuQuota:            1.25,
		MemoryMaxBytes:      268435456, // 256 MiB
		DiskMaxBytes:        1073741824,
		MaxDockerContainers: 7,
	}
	f := newAdoptFixture(t, "adopt-limits", persisted)
	// Current config (the second daemon's) differs from what the agent was
	// spawned with: a regression to defaultLimits() fails every want below.
	f.m.cfg.Agent.DefaultCPUQuota = 8.0
	f.m.cfg.Agent.DefaultMemoryBytes = 64 * 1024 * 1024 * 1024
	f.m.cfg.Agent.DefaultDiskBytes = 999 * 1024 * 1024 * 1024
	f.m.cfg.Agent.DefaultMaxDockerContainers = 99

	rep := f.runReconcile(t, "adopt-limits", "bunker-adopt-limits")

	if rep.Adopted != 1 || rep.Destroyed != 0 {
		t.Fatalf("report = %+v, want 1 adopted / 0 destroyed", rep)
	}
	if calls := f.knobs.all(); len(calls) != 1 {
		t.Fatalf("user-slice stage calls = %d, want exactly 1", len(calls))
	} else {
		c := calls[0]
		if c.cpuQuota != persisted.CpuQuota || c.memMax != persisted.MemoryMaxBytes || c.diskMax != persisted.DiskMaxBytes {
			t.Errorf("slice stage received cpu=%v mem=%v disk=%v, want persisted %v/%v/%v",
				c.cpuQuota, c.memMax, c.diskMax, persisted.CpuQuota, persisted.MemoryMaxBytes, persisted.DiskMaxBytes)
		}
		if c.uid != "4242" {
			t.Errorf("slice stage ran for uid %q, want the agent user's 4242", c.uid)
		}
	}
	units, props := f.units.snapshot()
	if len(units) != 1 || units[0] != "bunker-docker-adopt-limits" {
		t.Fatalf("docker unit runner calls = %v, want exactly [bunker-docker-adopt-limits]", units)
	}
	for _, want := range []string{
		"CPUQuota=125%",           // 1.25 cores → percent (unitKnobsFor)
		"MemoryMax=268435456",     // persisted bytes, not 64 GiB config
		"LimitFSIZE=1073741824",   // persisted DiskMaxBytes (semantics owned by DF-BUNKER-54)
		"TasksMax=4096",           // config default (no persisted value exists for it)
		"LimitNOFILE=65536:65536", // config default (no persisted value exists for it)
	} {
		if !strings.Contains(props[0], want) {
			t.Errorf("unit properties %q missing %q", props[0], want)
		}
	}
	if strings.Contains(props[0], "CPUQuota=800%") || strings.Contains(props[0], "MemoryMax=68719476736") {
		t.Errorf("unit properties %q carry current-config defaults; adoption must apply the persisted set", props[0])
	}

	tracked := f.m.tracker.Get("adopt-limits")
	if tracked == nil {
		t.Fatal("adopted agent missing from the tracker")
	}
	if tracked.Limits == nil ||
		tracked.Limits.CpuQuota != persisted.CpuQuota ||
		tracked.Limits.MemoryMaxBytes != persisted.MemoryMaxBytes ||
		tracked.Limits.DiskMaxBytes != persisted.DiskMaxBytes ||
		tracked.Limits.MaxDockerContainers != persisted.MaxDockerContainers {
		t.Errorf("tracker limits = %+v, want the agent's persisted values %+v", tracked.Limits, persisted)
	}
	if tracked.UnitProperties == nil || len(tracked.UnitProperties) == 0 {
		t.Error("adopted record carries no unit properties; adopted agents must report what they were spawned with")
	}
}

// ── Contract 1b: fallback when nothing persisted is readable ──────

// TestAdopt_FallsBackToConfigDefaultsWithoutPersistedLimits: an agent with no
// readable durable record (spawned by an older build, or the record was
// rotated away) is still adopted — with the documented fallback to
// m.defaultLimits(), applied to the host through the same two stages.
func TestAdopt_FallsBackToConfigDefaultsWithoutPersistedLimits(t *testing.T) {
	f := newAdoptFixture(t, "adopt-legacy", nil)
	// No durable record for this agent at all: point the manager at an empty
	// registry file so the JSONL sidecar read finds nothing.
	emptyPath := filepath.Join(t.TempDir(), "agents.jsonl")
	f.reopenRegistry(t, emptyPath)

	// Distinct non-default config values so the fallback provably sources
	// from m.defaultLimits() and not from zeros.
	f.m.cfg.Agent.DefaultCPUQuota = 3.5
	f.m.cfg.Agent.DefaultMemoryBytes = 5 * 1024 * 1024 * 1024
	f.m.cfg.Agent.DefaultDiskBytes = 50 * 1024 * 1024 * 1024

	rep := f.runReconcile(t, "adopt-legacy", "bunker-adopt-legacy")

	if rep.Adopted != 1 || rep.Destroyed != 0 {
		t.Fatalf("report = %+v, want 1 adopted / 0 destroyed", rep)
	}
	if calls := f.knobs.all(); len(calls) != 1 {
		t.Fatalf("user-slice stage calls = %d, want exactly 1", len(calls))
	} else if c := calls[0]; c.cpuQuota != 3.5 || c.memMax != 5*1024*1024*1024 || c.diskMax != 50*1024*1024*1024 {
		t.Errorf("fallback slice stage received cpu=%v mem=%v disk=%v, want config defaults 3.5/5GiB/50GiB", c.cpuQuota, c.memMax, c.diskMax)
	}
	tracked := f.m.tracker.Get("adopt-legacy")
	if tracked == nil || tracked.Limits == nil {
		t.Fatal("adopted agent missing from the tracker (or limits unset)")
	}
	if tracked.Limits.CpuQuota != 3.5 || tracked.Limits.MemoryMaxBytes != 5*1024*1024*1024 {
		t.Errorf("tracker limits = %+v, want the config-default fallback", tracked.Limits)
	}
}

// ── Contract 2: host-stage failure fails adoption loudly ──────────

// TestAdopt_HostStageFailureLeavesNoHalfManagedState: a failing user-slice
// stage (table: slice write vs docker-unit run) must fail adoption loudly —
// the orphan is destroyed instead — and must leave NO tracker record, NO port
// reservation and NO live registry record. Reported limits never paper over a
// host that is not actually constrained.
func TestAdopt_HostStageFailureLeavesNoHalfManagedState(t *testing.T) {
	cases := []struct {
		name string
		arm  func(f *adoptFixture)
	}{
		{
			name: "user-slice stage fails",
			arm: func(f *adoptFixture) {
				f.knobs.fail = errors.New("systemd daemon-reload failed")
			},
		},
		{
			name: "docker unit stage fails",
			arm: func(f *adoptFixture) {
				f.units.fail = errors.New("systemd-run failed: Unit already loaded")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdoptFixture(t, "adopt-fail", &v1.ResourceLimits{CpuQuota: 1.25, MemoryMaxBytes: 268435456})
			tc.arm(f)

			rep := f.runReconcile(t, "adopt-fail", "bunker-adopt-fail")

			if rep.Adopted != 0 {
				t.Errorf("Adopted = %d, want 0 (a failed host stage must fail adoption)", rep.Adopted)
			}
			if rep.Destroyed != 1 {
				t.Errorf("Destroyed = %d, want 1 (failed adopt falls back to destroy)", rep.Destroyed)
			}
			assertNoHalfManagedState(t, f.m, "adopt-fail")
			assertDestroyedOnce(t, f.destroy, "adopt-fail")
			if !poolIsEmpty(f.m.portAlloc) {
				t.Errorf("port pool leaked: Available() = %d, MaxRanges() = %d",
					f.m.portAlloc.Available(), f.m.portAlloc.MaxRanges())
			}
		})
	}
}

// ── Contract 2b: seams are nil-safe (a manager built by hand) ─────

// TestAdopt_NilHostSeamsStillAdopts: a manager constructed without the host
// seams (the coverage_boost_test.go shape, and any embedder that builds an
// AgentManager literally) must not panic. Nil seams skip the stage they own
// and adoption carries on — the pre-DF-BUNKER-53 behaviour is the nil seam
// degradation, never a crash.
func TestAdopt_NilHostSeamsStillAdopts(t *testing.T) {
	f := newAdoptFixture(t, "adopt-nils", &v1.ResourceLimits{CpuQuota: 1.25, MemoryMaxBytes: 268435456})
	f.m.applyAdoptedSliceLimits = nil
	f.m.runAdoptedDockerUnit = nil

	rep := f.runReconcile(t, "adopt-nils", "bunker-adopt-nils")

	if rep.Adopted != 1 || rep.Destroyed != 0 {
		t.Fatalf("report = %+v, want 1 adopted / 0 destroyed", rep)
	}
	if tracked := f.m.tracker.Get("adopt-nils"); tracked == nil {
		t.Fatal("adopted agent missing from the tracker")
	}
}

// ── Contract 3: the legacy silent-default adoption is gone ────────

// TestAdopt_NoLongerSilentlyReportsConfigDefaults pins the second defect's
// fix at the report surface: with a recorded spawn whose limits differ from
// the current config, the tracker record the adopt walk produces must carry
// the recorded values (this is the `bunker info` surface operators read). The
// docker-unit unit runner is intentionally left failing: this test only cares
// that the FAILURE message names the agent — but the tracker record must
// never be left behind reporting wrong limits either.
func TestAdopt_NoLongerSilentlyReportsConfigDefaults(t *testing.T) {
	f := newAdoptFixture(t, "adopt-report", &v1.ResourceLimits{CpuQuota: 0.75, MemoryMaxBytes: 134217728})
	f.m.cfg.Agent.DefaultCPUQuota = 2.0
	f.m.cfg.Agent.DefaultMemoryBytes = 4 * 1024 * 1024 * 1024
	// Make the host stages fail: the loud-failure contract is contract 2's
	// job; here we prove the HALF-BUILT record is not left behind reporting
	// config defaults after the failure.
	f.knobs.fail = errors.New("boom")

	f.runReconcile(t, "adopt-report", "bunker-adopt-report")

	if f.m.tracker.Get("adopt-report") != nil {
		t.Error("failed adoption left a tracker record behind — reported limits would be config defaults again")
	}
}
