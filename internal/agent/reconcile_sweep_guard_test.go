package agent

// REV-BUNKER-P1-PATCH — the GREEN-only cells: everything that asserts surface
// this row ADDS (ReconcileReport.Refused, the refusal log line, the guard's
// config knobs, the async startup path's counter). These cannot compile against
// the filed tree, so the arms script sets this file aside in `unfixed` mode; the
// behaviour RED lives in reconcile_sweep_guard_arms_test.go, which uses only
// pre-existing surface.
//
// The boundary is pinned here in one place: default limit 3 → three unknown users
// are swept, four are refused with the refusal COUNTED.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
)

// unprovenManager wires a manager whose durable registry was created THIS boot
// (the file did not exist) with a recording destroy seam and a captured log —
// the reproduced incident's precondition, minus the host.
func unprovenManager(t *testing.T, buf *bytes.Buffer) (*AgentManager, *destroyRecorder) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("precondition: registry file must not exist yet (stat err = %v)", err)
	}
	m, rec := foreignManager(t, path, false, buf)
	t.Cleanup(func() { m.registry.Close() })
	if !m.registry.CreatedThisBoot() {
		t.Fatal("precondition: the registry file was absent, so CreatedThisBoot() must report true")
	}
	return m, rec
}

// TestReconcileSweepGuard_BoundaryIsThreeSweptFourRefused pins the threshold on
// both sides with the counters a reader can check: three unknown users on an
// unproven registry are swept, four are refused.
func TestReconcileSweepGuard_BoundaryIsThreeSweptFourRefused(t *testing.T) {
	cases := []struct {
		name          string
		orphans       int
		wantDestroyed int
		wantRefused   int
	}{
		{name: "three unknown users — a normal leftovers sweep", orphans: 3, wantDestroyed: 3, wantRefused: 0},
		{name: "four unknown users — one above the limit", orphans: 4, wantDestroyed: 0, wantRefused: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			m, rec := unprovenManager(t, &buf)
			m.listSystemAgents = func() ([]SystemAgent, error) { return sweepOrphanHost(t, tc.orphans), nil }

			rep := m.Reconcile(context.Background())

			if rep.Destroyed != tc.wantDestroyed {
				t.Errorf("Destroyed = %d, want %d", rep.Destroyed, tc.wantDestroyed)
			}
			if rep.Refused != tc.wantRefused {
				t.Errorf("Refused = %d, want %d (report: %+v)", rep.Refused, tc.wantRefused, rep)
			}
			if got := len(rec.calls()); got != tc.wantDestroyed {
				t.Errorf("destroy calls = %d, want %d", got, tc.wantDestroyed)
			}
			// The counter is not decoration: it is reported in the log too,
			// because a bound nobody can read is not a bound. The refusal
			// line names it as refused_orphans (both paths); the async
			// completion line additionally carries refused= for the walk.
			if tc.wantRefused > 0 && !strings.Contains(buf.String(), "refused_orphans=") {
				t.Errorf("nothing in the log reports the refusal counter:\n%s", buf.String())
			}
		})
	}
}

// TestReconcileSweepGuard_RefusalIsCountedAndActionable pins the refusal's full
// contract: one loud ERROR, the counter, the reason, the surviving population,
// and a remedy naming BOTH operator paths (restore the registry first; raise the
// limit only if the registry is intact). A refusal that does not say what to do
// is indistinguishable from a hang.
func TestReconcileSweepGuard_RefusalIsCountedAndActionable(t *testing.T) {
	var buf bytes.Buffer
	m, rec := unprovenManager(t, &buf)
	m.listSystemAgents = func() ([]SystemAgent, error) { return sweepOrphanHost(t, 4), nil }

	rep := m.Reconcile(context.Background())
	log := buf.String()

	if rep.Refused != 4 || rep.Destroyed != 0 {
		t.Fatalf("report = %+v, want Refused=4 and Destroyed=0", rep)
	}
	if got := rec.calls(); len(got) != 0 {
		t.Fatalf("refused sweep still destroyed %v", got)
	}
	for _, want := range []string{
		"REFUSING to destroy unproven orphans",
		"level=ERROR",
		"action=refuse",
		"guard=orphan_sweep_guard",
		"refused_orphans=4",
		"unproven_orphan_limit=3",
		"NOTHING was destroyed",
		"replayed live=0",
		"created by this boot=true",
		"agent.reconciliation.unproven_orphan_limit raised above 4",
		"BUNKERD_AGENT_RECONCILIATION_UNPROVEN_ORPHAN_LIMIT",
		"restore",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("refusal line missing %q — log:\n%s", want, log)
		}
	}
	// The guard announces its armed state and its threshold at every boot, so
	// the threshold in force is never invisible.
	for _, want := range []string{
		"agent reconciliation sweep guard armed",
		"unproven_orphan_limit=3",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("boot line missing %q — log:\n%s", want, log)
		}
	}
}

// TestReconcileSweepGuard_RefusalTouchesNothing proves "destroy NOTHING" in the
// strongest available form: the registry log is byte-identical before and after
// (no destroy event, no knowled record, no rewrite) and every orphan counter is
// zero except Refused.
func TestReconcileSweepGuard_RefusalTouchesNothing(t *testing.T) {
	var buf bytes.Buffer
	m, rec := unprovenManager(t, &buf)
	before, err := os.ReadFile(m.registry.Path())
	if err != nil {
		t.Fatalf("read registry before: %v", err)
	}
	m.listSystemAgents = func() ([]SystemAgent, error) { return sweepOrphanHost(t, 4), nil }

	rep := m.Reconcile(context.Background())

	after, err := os.ReadFile(m.registry.Path())
	if err != nil {
		t.Fatalf("read registry after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("a refused sweep WROTE to the registry:\nbefore: %q\nafter:  %q", before, after)
	}
	if rep.Destroyed != 0 || rep.Adopted != 0 || rep.Restored != 0 || rep.Purged != 0 || rep.Foreign != 0 {
		t.Errorf("report = %+v, want every action counter 0 on a refused sweep", rep)
	}
	if len(rec.calls()) != 0 {
		t.Errorf("destroy seam called %v", rec.calls())
	}
}

// TestReconcileSweepGuard_ZeroLimitIsTheStrictestSetting pins that 0 is a real
// policy (refuse even a single orphan on an unproven registry) and never reads as
// "no limit" — the zero-value trap this knob is designed around.
func TestReconcileSweepGuard_ZeroLimitIsTheStrictestSetting(t *testing.T) {
	var buf bytes.Buffer
	m, rec := unprovenManager(t, &buf)
	m.cfg.Agent.Reconciliation.UnprovenOrphanLimit = 0
	m.listSystemAgents = func() ([]SystemAgent, error) { return sweepOrphanHost(t, 1), nil }

	rep := m.Reconcile(context.Background())

	if rep.Refused != 1 || rep.Destroyed != 0 {
		t.Errorf("report = %+v, want Refused=1 and Destroyed=0 with limit 0", rep)
	}
	if len(rec.calls()) != 0 {
		t.Errorf("limit 0 still destroyed %v", rec.calls())
	}
}

// TestReconcileSweepGuard_RaisedLimitIsTheOperatorEscapeHatch: an operator who
// knows the registry is intact raises the limit and the sweep proceeds — the
// documented way to sweep a larger genuine leftover set.
func TestReconcileSweepGuard_RaisedLimitIsTheOperatorEscapeHatch(t *testing.T) {
	var buf bytes.Buffer
	m, rec := unprovenManager(t, &buf)
	m.cfg.Agent.Reconciliation.UnprovenOrphanLimit = 4
	m.listSystemAgents = func() ([]SystemAgent, error) { return sweepOrphanHost(t, 4), nil }

	rep := m.Reconcile(context.Background())

	if rep.Destroyed != 4 || rep.Refused != 0 {
		t.Errorf("report = %+v, want the 4 swept with the limit raised to 4", rep)
	}
	if len(rec.calls()) != 4 {
		t.Errorf("destroy calls = %v, want 4", rec.calls())
	}
}

// TestReconcileSweepGuard_DisableIsExplicitAndLoud: the one knob that turns the
// guard off works, and the boot line says so at WARNING level. Disabling is a
// deliberate, greppable act — never a default.
func TestReconcileSweepGuard_DisableIsExplicitAndLoud(t *testing.T) {
	var buf bytes.Buffer
	m, rec := unprovenManager(t, &buf)
	m.cfg.Agent.Reconciliation.OrphanSweepGuardDisabled = true
	m.listSystemAgents = func() ([]SystemAgent, error) { return sweepOrphanHost(t, 4), nil }

	rep := m.Reconcile(context.Background())
	log := buf.String()

	if rep.Destroyed != 4 || rep.Refused != 0 {
		t.Errorf("report = %+v, want the sweep to proceed with the guard disabled", rep)
	}
	if len(rec.calls()) != 4 {
		t.Errorf("destroy calls = %v, want 4", rec.calls())
	}
	if !strings.Contains(log, "sweep guard DISABLED") || !strings.Contains(log, "level=WARN") {
		t.Errorf("disabling the guard must be logged as a warning — log:\n%s", log)
	}
}

// TestReconcileSweepGuard_SingleOrphanOnUnprovenRegistryStillSweeps pins ordinary
// operation: one leftover user and an empty live set is BELOW the limit, so the
// sweep happens. Without this the guard could refuse every unproven pass and
// still satisfy the defect cells.
func TestReconcileSweepGuard_SingleOrphanOnUnprovenRegistryStillSweeps(t *testing.T) {
	var buf bytes.Buffer
	m, rec := unprovenManager(t, &buf)
	m.listSystemAgents = func() ([]SystemAgent, error) { return sweepOrphanHost(t, 1), nil }

	rep := m.Reconcile(context.Background())

	if rep.Destroyed != 1 || rep.Refused != 0 {
		t.Errorf("report = %+v, want the single leftover swept", rep)
	}
	if len(rec.calls()) != 1 {
		t.Errorf("destroy calls = %v, want 1", rec.calls())
	}
}

// TestReconcileSweepGuard_StartupPathRefusesAndStillUnblocksTheReaper is the arm
// that matters most for the deployed daemon: server.Run calls ReconcileStartup,
// not Reconcile. The refusal must hold on the async path, land in the FINAL
// report delivered on the channel, and still close reconcileDone — the invariant
// the TTL reaper waits on. A guard that skipped the close would deadlock reaping.
func TestReconcileSweepGuard_StartupPathRefusesAndStillUnblocksTheReaper(t *testing.T) {
	var buf bytes.Buffer
	m, rec := unprovenManager(t, &buf)
	m.listSystemAgents = func() ([]SystemAgent, error) { return sweepOrphanHost(t, 4), nil }

	interim, finalCh := m.ReconcileStartup(context.Background())
	if interim.Refused != 0 {
		t.Errorf("interim Refused = %d, want 0 (the walk is still pending at return)", interim.Refused)
	}

	var final ReconcileReport
	select {
	case final = <-finalCh:
	case <-time.After(30 * time.Second):
		t.Fatal("the async orphan walk never delivered a final report")
	}
	if final.Refused != 4 || final.Destroyed != 0 {
		t.Errorf("final report = %+v, want Refused=4 and Destroyed=0", final)
	}
	if len(rec.calls()) != 0 {
		t.Errorf("the startup path destroyed %v", rec.calls())
	}
	select {
	case <-m.reconcileDone:
	case <-time.After(5 * time.Second):
		t.Error("reconcileDone was not closed by the refused walk — the TTL reaper would wait forever")
	}
	if !strings.Contains(buf.String(), "REFUSING to destroy unproven orphans") {
		t.Errorf("the startup path did not log the refusal:\n%s", buf.String())
	}
}

// TestReconcileSweepGuard_ConfigSurface pins the shipped defaults and the
// validation of the knob: armed by default, limit three, mode still destroy, 0
// accepted as the strictest setting, negative rejected rather than read as
// anything.
func TestReconcileSweepGuard_ConfigSurface(t *testing.T) {
	rc := config.DefaultConfig().Agent.Reconciliation
	if rc.Mode != config.ReconcileModeDestroy {
		t.Errorf("default mode = %q, want %q (the row keeps destroy, behind the guard)", rc.Mode, config.ReconcileModeDestroy)
	}
	if !rc.SweepGuardEnabled() {
		t.Error("the guard must be armed by default")
	}
	if rc.UnprovenOrphanLimit != config.DefaultUnprovenOrphanLimit {
		t.Errorf("default unproven_orphan_limit = %d, want %d", rc.UnprovenOrphanLimit, config.DefaultUnprovenOrphanLimit)
	}
	if config.DefaultUnprovenOrphanLimit != 3 {
		t.Errorf("the threshold moved to %d — the binary choice behind 3 (below it, a handful of leftovers still sweeps; "+
			"above it, the five-agent production host is protected) must be re-derived",
			config.DefaultUnprovenOrphanLimit)
	}
	// The threshold must sit BELOW the smallest deployment this project
	// actually runs, or the guard does not cover it. bunker-mvp was measured
	// at five real agents while this row was open; a limit that equals or
	// exceeds that protects nothing on the host that matters.
	if config.DefaultUnprovenOrphanLimit >= 5 {
		t.Errorf("unproven_orphan_limit = %d would sweep every agent on a five-agent deployment (bunker-mvp, measured)",
			config.DefaultUnprovenOrphanLimit)
	}
	// A zero-valued struct is the shape a hand-built config has: armed, not
	// unguarded.
	var zero config.ReconciliationConfig
	if !zero.SweepGuardEnabled() {
		t.Error("a zero-valued ReconciliationConfig must report the guard as ARMED")
	}
	if err := zero.Validate(); err != nil {
		t.Errorf("zero-valued config must validate (mode normalises to destroy): %v", err)
	}
	if zero.Mode != config.ReconcileModeDestroy {
		t.Errorf("zero-valued mode normalised to %q, want destroy", zero.Mode)
	}
	if err := (&config.ReconciliationConfig{UnprovenOrphanLimit: -1}).Validate(); err == nil {
		t.Error("a negative unproven_orphan_limit must be refused, never read as unlimited")
	} else if !strings.Contains(err.Error(), "unproven_orphan_limit") {
		t.Errorf("validation error must name the key, got %q", err.Error())
	}
	strictest := &config.ReconciliationConfig{UnprovenOrphanLimit: 0}
	if err := strictest.Validate(); err != nil {
		t.Errorf("limit 0 is a legitimate (strictest) policy, Validate returned %v", err)
	}
}
