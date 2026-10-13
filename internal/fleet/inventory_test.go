package fleet

import (
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/hostsetup"
)

// TestCommittedInventoryDeclaresEveryKnownFleetHost is the coverage cell for
// GAP-083 acceptance criterion 4: the committed inventory must declare EVERY
// known bunker host, so a host cannot be invisible to the fleet audit by
// omission. The list is pinned literally (not derived) so that dropping a host
// from inventory.yaml fails here instead of silently shrinking the fleet.
//
// bunker-las-01 is the host the row was filed for: it ran an ACTIVE daemon
// (re-probed live 2026-09-19) while `bunker status --all` omitted it.
func TestCommittedInventoryDeclaresEveryKnownFleetHost(t *testing.T) {
	inv, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []string{
		"bunker-las-01", "bunker-las-02", "bunker-las-03", "bunker-las-04",
		"bunker-mvp", "cube-las-00",
	}
	got := inv.Names()
	if len(got) != len(want) {
		t.Fatalf("inventory declares %d hosts %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("declared host %d = %q, want %q (full list: %v)", i, got[i], want[i], got)
		}
	}
	for _, h := range inv.Sorted() {
		if h.Address == "" {
			t.Errorf("host %q declares no address", h.Name)
		}
		if len(h.Ports) == 0 {
			t.Errorf("host %q declares no ports", h.Name)
		}
	}
	if _, ok := inv.Lookup("bunker-las-01"); !ok {
		t.Error("bunker-las-01 (the host this row exists for) is not declared")
	}
}

// TestCommittedInventoryLas01PortDiscrepancyIsDocumented pins GAP-083
// acceptance criterion 3 for the host it was written about: the declaration
// states the fleet contract (:10001/:10002), and the port note records why the
// live host differs (:18080) and why its registry entry (:19090) can never
// connect. Documenting it in the declaration is what makes the finding
// self-explaining at the point an operator reads it.
func TestCommittedInventoryLas01PortDiscrepancyIsDocumented(t *testing.T) {
	inv, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	h, ok := inv.Lookup("bunker-las-01")
	if !ok {
		t.Fatal("bunker-las-01 is not declared")
	}
	if !h.AcceptsPort(10001) || !h.AcceptsPort(10002) {
		t.Errorf("bunker-las-01 declared ports = %v, want the fleet convention 10001 and 10002", h.Ports)
	}
	if h.AcceptsPort(19090) {
		t.Error("bunker-las-01 declares 19090 — that is the port its daemon has never listened on; declaring it would bless the defect")
	}
	for _, want := range []string{"18080", "19090", "10001"} {
		if !strings.Contains(h.PortNote, want) {
			t.Errorf("bunker-las-01 port note does not document %s:\n%s", want, h.PortNote)
		}
	}
}

// TestCommittedInventoryEveryPortDeviationIsDocumented forbids a silent
// exception: any host that does not declare the fleet's port convention must
// carry a port note saying why. Without this, a host could be quietly
// registered on any port and the audit would treat the deviation as normal.
func TestCommittedInventoryEveryPortDeviationIsDocumented(t *testing.T) {
	inv, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inv.PortConvention == 0 {
		t.Fatal("inventory declares no port_convention")
	}
	for _, h := range inv.Sorted() {
		if !h.AcceptsPort(inv.PortConvention) && strings.TrimSpace(h.PortNote) == "" {
			t.Errorf("host %q declares ports %v without the %d convention and has no port_note explaining the deviation",
				h.Name, h.Ports, inv.PortConvention)
		}
	}
}

// TestFleetFloorIsTheRepoFloor pins that the inventory's version floor IS the
// repo's floor (internal/hostsetup.MinDaemonVersion): the fleet audit and
// `bunker host-provision`'s skew probe must never disagree about which daemons
// are acceptable.
func TestFleetFloorIsTheRepoFloor(t *testing.T) {
	if MinDaemonVersion != hostsetup.MinDaemonVersion {
		t.Errorf("fleet.MinDaemonVersion = %q, hostsetup.MinDaemonVersion = %q — the two floors must not drift",
			MinDaemonVersion, hostsetup.MinDaemonVersion)
	}
	inv, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inv.DefaultMinDaemonVersion != MinDaemonVersion {
		t.Errorf("inventory default_min_daemon_version = %q, want %q", inv.DefaultMinDaemonVersion, MinDaemonVersion)
	}
	if h, _ := inv.Lookup("bunker-las-02"); h.Floor() != MinDaemonVersion {
		t.Errorf("a host with no override must use the default floor, got %q", h.Floor())
	}
	if h := (Host{MinVersion: "9.9.9"}); h.Floor() != "9.9.9" {
		t.Errorf("per-host min_version override ignored: %q", h.Floor())
	}
}

// TestParseRejectsMalformedDeclaration makes a bad edit fail at load time: a
// host with no name, no role or no port would otherwise check nothing and
// shrink the audit silently.
func TestParseRejectsMalformedDeclaration(t *testing.T) {
	const good = `
version: 1
port_convention: 10001
default_min_daemon_version: "0.1.4"
hosts:
  - name: h1
    address: 10.0.0.1
    role: agent-host
    ports: [10001]
`
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatalf("the baseline declaration must parse: %v", err)
	}

	tests := []struct {
		name    string
		mutate  string
		wantSub string
	}{
		{"wrong version", strings.Replace(good, "version: 1", "version: 2", 1), "unsupported version"},
		{"no hosts", "version: 1\nport_convention: 10001\nhosts: []\n", "no hosts declared"},
		{"no name", strings.Replace(good, "name: h1", "name: \"\"", 1), "has no name"},
		{"no role", strings.Replace(good, "role: agent-host", "role: \"\"", 1), "has no role"},
		{"no ports", strings.Replace(good, "ports: [10001]", "ports: []", 1), "declares no ports"},
		{"bad port", strings.Replace(good, "ports: [10001]", "ports: [0]", 1), "out-of-range port"},
		{"duplicate host", strings.Replace(good, "  - name: h1", "  - name: h1\n    address: 10.0.0.1\n    role: agent-host\n    ports: [10001]\n  - name: h1", 1), "declared twice"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.mutate))
			if err == nil {
				t.Fatalf("Parse accepted a malformed declaration (%s)", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not name the defect (%q)", err, tc.wantSub)
			}
		})
	}
}

func TestPortOf(t *testing.T) {
	tests := []struct {
		url      string
		wantPort int
		wantOK   bool
	}{
		{"http://100.78.165.52:19090", 19090, true},
		{"https://100.122.57.93:10001", 10001, true},
		{"http://[::1]:18080", 18080, true},
		{"http://host", 0, false},
		{"http://host:99999", 0, false},
		{"http://host:abc", 0, false},
		{"not a url", 0, false},
		{"", 0, false},
	}
	for _, tc := range tests {
		port, ok := PortOf(tc.url)
		if ok != tc.wantOK || port != tc.wantPort {
			t.Errorf("PortOf(%q) = (%d, %v), want (%d, %v)", tc.url, port, ok, tc.wantPort, tc.wantOK)
		}
	}
}

// TestReconcile is the decision table for the audit: every way a declared host
// can be unusable to a client is classified, and the ORDER is pinned so the
// reported status names the first thing that breaks selection.
func TestReconcile(t *testing.T) {
	inv := &Inventory{
		Version:        1,
		PortConvention: 10001,
		Hosts: []Host{
			{Name: "alpha", Address: "10.0.0.1", Role: "agent-host", Ports: []int{10001, 10002}},
			{Name: "bravo", Address: "10.0.0.2", Role: "agent-host", Ports: []int{10001}},
			{Name: "charlie", Address: "10.0.0.3", Role: "agent-host", Ports: []int{18080}, PortNote: "demo layout"},
			{Name: "delta", Address: "10.0.0.4", Role: "agent-host", Ports: []int{10001}, MinVersion: "0.9.0"},
			{Name: "echo", Address: "10.0.0.5", Role: "agent-host", Ports: []int{10001}, Retired: true},
		},
	}

	tests := []struct {
		name       string
		observed   map[string]Observation
		wantStatus map[string]Status
		wantDetail map[string]string
	}{
		{
			name: "healthy host at the floor",
			observed: map[string]Observation{
				"alpha": {Registered: true, URL: "http://10.0.0.1:10001", Reachable: true, Version: "0.1.4"},
			},
			wantStatus: map[string]Status{"alpha": StatusOK},
			wantDetail: map[string]string{"alpha": "version 0.1.4 at or above the 0.1.4 floor"},
		},
		{
			name: "a v-prefixed version still compares",
			observed: map[string]Observation{
				"alpha": {Registered: true, URL: "http://10.0.0.1:10002", Reachable: true, Version: "v0.2.0"},
			},
			wantStatus: map[string]Status{"alpha": StatusOK},
		},
		{
			name:       "declared but never registered is the las-01 hole",
			observed:   map[string]Observation{},
			wantStatus: map[string]Status{"alpha": StatusNotRegistered},
			wantDetail: map[string]string{"alpha": "absent from the client registry"},
		},
		{
			name: "registered but silent",
			observed: map[string]Observation{
				"alpha": {Registered: true, URL: "http://10.0.0.1:10001", Reachable: false},
			},
			wantStatus: map[string]Status{"alpha": StatusUnreachable},
			wantDetail: map[string]string{"alpha": "did not answer"},
		},
		{
			name: "registered on a port it is not declared to serve (the las-01 :19090 entry)",
			observed: map[string]Observation{
				"alpha": {Registered: true, URL: "http://10.0.0.1:19090", Reachable: true, Version: "0.1.4"},
			},
			wantStatus: map[string]Status{"alpha": StatusPortMismatch},
			wantDetail: map[string]string{"alpha": "uses port 19090, which is not a declared port"},
		},
		{
			name: "a wrong port is reported even when the host is also down (the las-01 shape today)",
			observed: map[string]Observation{
				"alpha": {Registered: true, URL: "http://10.0.0.1:19090", Reachable: false},
			},
			wantStatus: map[string]Status{"alpha": StatusPortMismatch},
			wantDetail: map[string]string{"alpha": "uses port 19090, which is not a declared port"},
		},
		{
			name: "registry URL without a port cannot be checked",
			observed: map[string]Observation{
				"alpha": {Registered: true, URL: "http://10.0.0.1", Reachable: true, Version: "0.1.4"},
			},
			wantStatus: map[string]Status{"alpha": StatusPortMismatch},
			wantDetail: map[string]string{"alpha": "carries no usable port"},
		},
		{
			name: "a declared port layout deviation carries its note into the finding",
			observed: map[string]Observation{
				"charlie": {Registered: true, URL: "http://10.0.0.3:10001", Reachable: true, Version: "0.1.4"},
			},
			wantStatus: map[string]Status{"charlie": StatusPortMismatch},
			wantDetail: map[string]string{"charlie": "demo layout"},
		},
		{
			name: "below the floor",
			observed: map[string]Observation{
				"alpha": {Registered: true, URL: "http://10.0.0.1:10001", Reachable: true, Version: "0.1.3"},
			},
			wantStatus: map[string]Status{"alpha": StatusBelowFloor},
			wantDetail: map[string]string{"alpha": "below the 0.1.4 floor"},
		},
		{
			name: "per-host floor override applies",
			observed: map[string]Observation{
				"delta": {Registered: true, URL: "http://10.0.0.4:10001", Reachable: true, Version: "0.1.4"},
			},
			wantStatus: map[string]Status{"delta": StatusBelowFloor},
			wantDetail: map[string]string{"delta": "below the 0.9.0 floor"},
		},
		{
			name: "a daemon that reports no version cannot prove the floor",
			observed: map[string]Observation{
				"alpha": {Registered: true, URL: "http://10.0.0.1:10001", Reachable: true},
			},
			wantStatus: map[string]Status{"alpha": StatusVersionUnproven},
			wantDetail: map[string]string{"alpha": "cannot be proved"},
		},
		{
			name: "an uncomparable version cannot prove the floor either",
			observed: map[string]Observation{
				"alpha": {Registered: true, URL: "http://10.0.0.1:10001", Reachable: true, Version: "unknown"},
			},
			wantStatus: map[string]Status{"alpha": StatusBelowFloor},
			wantDetail: map[string]string{"alpha": "below the 0.1.4 floor"},
		},
		{
			name:       "retired hosts are information, never a coverage hole",
			observed:   map[string]Observation{},
			wantStatus: map[string]Status{"echo": StatusRetired},
			wantDetail: map[string]string{"echo": "retired"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			verdicts := inv.Reconcile(tc.observed)
			if len(verdicts) != len(inv.Hosts) {
				t.Fatalf("Reconcile returned %d verdicts for %d declared hosts — every declared host must be reported",
					len(verdicts), len(inv.Hosts))
			}
			byName := map[string]Verdict{}
			for _, v := range verdicts {
				byName[v.Host.Name] = v
			}
			// Every host not named in wantStatus is unobserved in this case, so it
			// must be reported as NOT_REGISTERED (unless retired).
			for _, h := range inv.Sorted() {
				want, pinned := tc.wantStatus[h.Name]
				if !pinned {
					want = StatusNotRegistered
					if h.Retired {
						want = StatusRetired
					}
				}
				got := byName[h.Name]
				if got.Status != want {
					t.Errorf("host %q status = %s (%s), want %s", h.Name, got.Status, got.Detail, want)
				}
			}
			for name, sub := range tc.wantDetail {
				if !strings.Contains(byName[name].Detail, sub) {
					t.Errorf("host %q detail %q does not contain %q", name, byName[name].Detail, sub)
				}
			}
		})
	}
}

// TestReconcileReportsEveryDeclaredHost is the anti-omission cell: whatever the
// observations, no declared host may be missing from the verdict list — that is
// the exact defect (a host silently absent from the overview) this row exists
// to close.
func TestReconcileReportsEveryDeclaredHost(t *testing.T) {
	inv, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	verdicts := inv.Reconcile(map[string]Observation{})
	if len(verdicts) != len(inv.Hosts) {
		t.Fatalf("Reconcile returned %d verdicts, want one per declared host (%d)", len(verdicts), len(inv.Hosts))
	}
	seen := map[string]bool{}
	for _, v := range verdicts {
		if seen[v.Host.Name] {
			t.Errorf("host %q appears twice", v.Host.Name)
		}
		seen[v.Host.Name] = true
	}
	if !seen["bunker-las-01"] {
		t.Error("bunker-las-01 has no verdict — the host the row was filed for must always be reported")
	}
	// Empty observations = nothing registered: every host is a hole, so the
	// audit must fail closed (never render as covered).
	for _, v := range verdicts {
		if v.OK() {
			t.Errorf("host %q reported %s with no registry entry at all", v.Host.Name, v.Status)
		}
	}
}

// TestPortListRendersDeterministically pins the operator-facing port list.
func TestPortListRendersDeterministically(t *testing.T) {
	h := Host{Name: "h", Ports: []int{10001, 10002}}
	if got := h.PortList(); got != "10001, 10002" {
		t.Errorf("PortList = %q, want %q", got, "10001, 10002")
	}
}
