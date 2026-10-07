// GAP-134 lifecycle tests: the agent-manager wiring of the egress policy
// through the injected seam — spawn installs, destroy removes, the sweep
// reconciles, open mode touches nothing, and the failure law holds (a failed
// install in an enforced mode fails the spawn). Every firewall claim here is
// exercised against the RecordingExecutor, never a live host.
package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/egress"
	"github.com/deployBunker/bunker/internal/registry"
	"github.com/deployBunker/bunker/internal/resource"
)

// newEgressTestManager builds a manager with a recording egress seam and a
// registry isolated in the test temp dir.
func newEgressTestManager(t *testing.T) (*AgentManager, *egress.RecordingExecutor) {
	t.Helper()
	m := newTestManager(t)
	exec := &egress.RecordingExecutor{}
	m.egressMgr = egress.NewManagerWith(exec, egress.StaticResolver{})
	return m, exec
}

func TestEgressResolvePrecedence(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		cfgMode   string
		want      string
		wantErr   bool
	}{
		{"default open", "", "", "open", false},
		{"config global", "", "none", "none", false},
		{"request wins", "open", "none", "open", false},
		{"unknown request", "wat", "", "", true},
		{"unknown config", "", "wat", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newEgressTestManager(t)
			m.cfg.Agent.Egress.Mode = tt.cfgMode
			pol, err := m.egressResolve(tt.requested)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("egressResolve(%q) = %+v, want refusal", tt.requested, pol)
				}
				return
			}
			if err != nil {
				t.Fatalf("egressResolve(%q): %v", tt.requested, err)
			}
			if pol.Mode != tt.want {
				t.Errorf("mode = %q, want %q", pol.Mode, tt.want)
			}
		})
	}
}

func TestEgressInstallZeroCallsOpenMode(t *testing.T) {
	// Requirement 4: in open mode NOTHING in the spawn path may invoke a
	// firewall helper — asserted at the manager-seam level (Install is
	// called exactly as spawn calls it).
	m, exec := newEgressTestManager(t)
	pol, err := m.egressResolve("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if pol.Enforced() {
		t.Fatalf("default policy reports enforced (mode %q)", pol.Mode)
	}
	if err := m.egressInstall(pol, 1234); err != nil {
		t.Fatalf("install: %v", err)
	}
	if exec.Count() != 0 {
		t.Fatalf("open-mode install made %d firewall calls, want 0: %+v", exec.Count(), exec.Calls)
	}
}

func TestEgressInstallEnforcedMakesCallsAndFailsLoud(t *testing.T) {
	// Enforced mode installs (calls the seam) and a seam failure comes back
	// as an error — the spawn rolls back (requirement 4).
	m, exec := newEgressTestManager(t)
	m.cfg.Agent.Egress = config.EgressConfig{Mode: "none"}
	pol, err := m.egressResolve("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !pol.Enforced() {
		t.Fatal("none mode must report enforced")
	}
	if err := m.egressInstall(pol, 4321); err != nil {
		t.Fatalf("install (healthy seam): %v", err)
	}
	if exec.Count() == 0 {
		t.Fatal("enforced install made zero firewall calls — the spawn would be unenforced")
	}
	// A seam failure must surface (fail loud), naming the step.
	exec2 := &egress.RecordingExecutor{FailOn: func(int, []string) error {
		return errors.New("nft: permission denied (the daemon must be root to install egress rules)")
	}}
	m.egressMgr = egress.NewManagerWith(exec2, egress.StaticResolver{})
	if err := m.egressInstall(pol, 4321); err == nil {
		t.Fatal("failed install returned nil — spawn would report an unenforced agent as ready")
	} else if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error %q does not carry the backend failure", err)
	}
}

func TestEgressNilManagerRefusals(t *testing.T) {
	// The nil-manager degradation is open-mode-equivalent for install/removal
	// but a LOUD refusal for an enforced policy (never a silent skip).
	m := newTestManager(t) // hand-built: egressMgr nil
	pol := egress.ModePolicy{Mode: egress.ModeNone}
	if err := m.egressInstall(pol, 7); err == nil {
		t.Fatal("enforced install with nil manager returned nil — the documented degradation must refuse loudly")
	}
	if err := m.egressInstall(egress.ModePolicy{Mode: egress.ModeOpen}, 7); err != nil {
		t.Fatalf("open install with nil manager: %v", err)
	}
	if err := m.egressRemove("bunker-x"); err != nil {
		t.Fatalf("remove with nil manager: %v", err)
	}
	// The sweeper never starts a goroutine with a nil manager (no way to
	// assert a goroutine directly; assert the sweep entry point no-ops).
	m.egressSweepStaleChains() // must not panic
}

func TestEgressDestroyRemoveUsesUID(t *testing.T) {
	// egressRemove resolves the agent's uid through the user database seam.
	m, exec := newEgressTestManager(t)
	// With no such user the removal is a no-op (no uid to key on).
	if err := m.egressRemove("bunker-never-existed-xyz"); err != nil {
		t.Fatalf("remove of unknown user: %v", err)
	}
	if exec.Count() != 0 {
		t.Fatalf("remove without a user made %d calls", exec.Count())
	}
}

func TestEgressSweepLiveSetFromPasswdWalk(t *testing.T) {
	// The sweep builds its live-uid set from listSystemAgents (the same
	// authority as the orphan sweep). With the walk stubbed to one live
	// agent whose uid the user-database seam resolves, the sweep must run
	// the backend listing through the seam and protect live chains.
	m, exec := newEgressTestManager(t)
	m.cfg.Agent.Egress.Mode = "none" // arms the sweep gate
	m.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: "a1", Username: "bunker-a1", Home: "/nonexistent"}}, nil
	}
	// lookupAgentUser on a nonexistent user fails → the sweep logs and
	// treats that agent as unresolvable; the backend listing still runs.
	m.egressSweepStaleChains()
	if exec.Count() == 0 {
		t.Fatal("sweep made no backend calls — the listing never ran")
	}
	// A failed system-agent probe must SKIP the sweep entirely (never read
	// as "nobody is live" — that would delete live chains).
	exec2 := &egress.RecordingExecutor{}
	m2, _ := newEgressTestManager(t)
	m2.cfg.Agent.Egress.Mode = "none"
	m2.egressMgr = egress.NewManagerWith(exec2, egress.StaticResolver{})
	m2.listSystemAgents = func() ([]SystemAgent, error) {
		return nil, errors.New("cannot read /etc/passwd")
	}
	m2.egressSweepStaleChains()
	if exec2.Count() != 0 {
		t.Fatalf("sweep ran despite a failed agent probe (%d calls) — a failed probe must not read as nobody-is-live", exec2.Count())
	}
	// OPEN-MODE GATE (requirement 1): a pure-open daemon (open config, no
	// enforced record) never lists the firewall — zero calls, restart-safe.
	exec3 := &egress.RecordingExecutor{}
	m3, _ := newEgressTestManager(t)
	m3.egressMgr = egress.NewManagerWith(exec3, egress.StaticResolver{})
	m3.listSystemAgents = func() ([]SystemAgent, error) { return nil, nil }
	m3.egressSweepStaleChains()
	if exec3.Count() != 0 {
		t.Fatalf("open-mode sweep made %d firewall calls, want 0 (zero-behavior-change law)", exec3.Count())
	}
	// ...but one replayed enforced record arms it even under open config
	// (restart safety: an earlier restricted process may have left chains).
	if err := m3.tracker.Register(&resource.AgentRecord{AgentID: "z", EgressMode: "none"}); err != nil {
		t.Fatalf("register fixture agent: %v", err)
	}
	m3.egressSweepStaleChains()
	if exec3.Count() == 0 {
		t.Fatal("a replayed enforced record must arm the sweep (restart safety)")
	}
}

func TestEgressRecordCarriesMode(t *testing.T) {
	// The spawn stamps the resolved mode on the record (the reporting law);
	// the registry round-trip preserves it.
	rec := &resource.AgentRecord{AgentID: "x", EgressMode: "none"}
	reg := recordToRegistry(rec)
	if reg.EgressMode != "none" {
		t.Fatalf("recordToRegistry lost the egress mode: %q", reg.EgressMode)
	}
	back := registryToRecord(reg)
	if back.EgressMode != "none" {
		t.Fatalf("registryToRecord lost the egress mode: %q", back.EgressMode)
	}
	// A pre-GAP-134 record (empty mode) stays empty — never upgraded.
	empty := registryToRecord(&registry.Record{AgentID: "y"})
	if empty.EgressMode != "" {
		t.Fatalf("pre-GAP-134 record upgraded to %q — absence must stay absence", empty.EgressMode)
	}
}
