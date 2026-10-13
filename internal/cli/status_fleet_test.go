package cli

// GAP-083: the fleet-inventory half of `bunker status --all`.
//
// The row this covers: bunker-las-01 ran an ACTIVE bunkerd for 32+ days while
// `bunker status --all` — the documented cross-host overview — omitted the host
// entirely (it was absent from the registry, and the single entry it later got
// pointed at :19090, a port the host has never listened on). The overview now
// iterates the UNION of the local registry and the committed fleet inventory
// (internal/fleet) and reconciles every declared host against what was
// observed, so a declared host that is not covered is a loud FAILED line and a
// non-zero exit.
//
// These cells stub the loadFleetInventory seam (supplied by TestMain for the
// rest of the package, see disableFleetInventoryForSuite) with small
// declarations, so the rendering and the exit contract are pinned without any
// live host. TestLoadFleetInventoryDefaultLoadsTheCommittedInventory proves the
// PRODUCTION default is the embedded inventory this row added.

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/fleet"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// defaultLoadFleetInventory holds the production loader (fleet.Load) captured
// before TestMain swaps in the suite stub, so the wiring of the real committed
// inventory can still be asserted from a test.
var defaultLoadFleetInventory = func() (*fleet.Inventory, error) { return fleet.Load() }

// disableFleetInventoryForSuite replaces the embedded fleet inventory with an
// empty declaration for the whole package run. The package's per-registry
// rendering tests (status_test.go) pin exactly what a config listing alpha/
// bravo/dead produces, and with the real six-host declaration in play every
// one of them would also report six coverage holes and exit non-zero — an
// unrelated contract. The coverage contract itself is covered by the cells in
// this file, which supply their own declarations, and by
// TestLoadFleetInventoryDefaultLoadsTheCommittedInventory, which proves the
// production default is the committed inventory.
func disableFleetInventoryForSuite() {
	defaultLoadFleetInventory = loadFleetInventory
	loadFleetInventory = func() (*fleet.Inventory, error) { return &fleet.Inventory{}, nil }
}

// withFleetInventory points the seam at a declaration for one test and restores
// the suite stub afterwards.
func withFleetInventory(t *testing.T, inv *fleet.Inventory) {
	t.Helper()
	saved := loadFleetInventory
	loadFleetInventory = func() (*fleet.Inventory, error) { return inv, nil }
	t.Cleanup(func() { loadFleetInventory = saved })
}

// inventoryFrom parses a declaration from YAML — the same path Load uses, so a
// broken fixture fails loudly instead of testing nothing.
func inventoryFrom(t *testing.T, doc string) *fleet.Inventory {
	t.Helper()
	inv, err := fleet.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("fixture declaration does not parse: %v", err)
	}
	return inv
}

// TestLoadFleetInventoryDefaultLoadsTheCommittedInventory proves the wiring the
// row is about: the loader the CLI ships with reads the committed fleet
// declaration, and that declaration contains bunker-las-01.
func TestLoadFleetInventoryDefaultLoadsTheCommittedInventory(t *testing.T) {
	inv, err := defaultLoadFleetInventory()
	if err != nil {
		t.Fatalf("default loader: %v", err)
	}
	names := inv.Names()
	if len(names) < 6 {
		t.Fatalf("committed inventory declares %d hosts %v — the fleet audit would be blind to the rest", len(names), names)
	}
	if _, ok := inv.Lookup("bunker-las-01"); !ok {
		t.Errorf("the committed inventory does not declare bunker-las-01 (declared: %v)", names)
	}
}

// TestStatusCommand_AllServers_FleetDeclaredHostIsAlwaysRendered is the core
// cell: a host that the registry does not know still gets a SECTION, with a
// loud FAILED line, and the command exits non-zero. Before GAP-083 the section
// simply did not exist — which is how las-01 drifted unseen.
func TestStatusCommand_AllServers_FleetDeclaredHostIsAlwaysRendered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := newStatusTestServer(t, &statusMockServer{
		info: &v1.ServerInfoResponse{Hostname: "covered-host", Version: "0.1.4"},
	})
	defer srv.Close()
	coveredPort := portOfTestServer(t, srv)

	// One host is registered (and healthy) at a declared port; the other is
	// declared by the fleet and absent from the registry.
	inv := inventoryFrom(t, fmt.Sprintf(`
version: 1
port_convention: %d
default_min_daemon_version: "0.1.4"
hosts:
  - name: covered
    address: 10.0.0.1
    role: agent-host
    ports: [%d]
  - name: bunker-las-01
    address: 100.78.165.52
    role: agent-host
    ports: [10001, 10002]
    note: >-
      Redeployed to bunkerd 0.1.4 by the ops lane after the row was filed.
`, coveredPort, coveredPort))
	withFleetInventory(t, inv)

	cfg := &CLIConfig{
		ActiveServer: "covered",
		Servers:      map[string]ServerEntry{"covered": {Name: "covered", URL: srv.URL, Token: "t"}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewStatusCommand()
	cmd.SetArgs([]string{"--server", "default", "--all"})
	var execErr error
	output := captureStdout(t, func() { execErr = cmd.Execute() })

	// The declared-but-unregistered host HAS a section and is loud.
	for _, want := range []string{
		"── bunker-las-01 ──",
		"Declared: bunker-las-01 (agent-host) at 100.78.165.52, ports 10001, 10002",
		"NOT_REGISTERED",
		"Fleet:    FAILED",
		"absent from the client registry",
		"Redeployed to bunkerd 0.1.4",
		"Fleet inventory coverage (2 declared, 1 failing)",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	// The covered host prints its version AND its coverage evidence.
	for _, want := range []string{"covered-host", "0.1.4", "Fleet:    OK", "port " + fmt.Sprint(coveredPort) + " declared"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	if execErr == nil {
		t.Fatal("a declared host with no registry entry must exit non-zero — a silent overview is the defect this row exists to close")
	}
	for _, want := range []string{"fleet inventory coverage", "1 of 2", "bunker-las-01 NOT_REGISTERED"} {
		if !strings.Contains(execErr.Error(), want) {
			t.Errorf("exit error %q does not contain %q", execErr, want)
		}
	}
}

// TestStatusCommand_AllServers_FleetCoveredFleetExitsZero is the counter-cell:
// when every declared host IS covered the audit must pass, so the non-zero exit
// cannot be satisfied by failing unconditionally.
func TestStatusCommand_AllServers_FleetCoveredFleetExitsZero(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := newStatusTestServer(t, &statusMockServer{
		info: &v1.ServerInfoResponse{Hostname: "fleet-host", Version: "0.2.0"},
	})
	defer srv.Close()
	port := portOfTestServer(t, srv)

	withFleetInventory(t, inventoryFrom(t, fmt.Sprintf(`
version: 1
port_convention: %d
default_min_daemon_version: "0.1.4"
hosts:
  - name: fleet-host
    address: 10.0.0.1
    role: agent-host
    ports: [%d]
`, port, port)))

	cfg := &CLIConfig{
		ActiveServer: "fleet-host",
		Servers:      map[string]ServerEntry{"fleet-host": {Name: "fleet-host", URL: srv.URL, Token: "t"}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewStatusCommand()
	cmd.SetArgs([]string{"--server", "default", "--all"})
	var execErr error
	output := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("a fully covered fleet must exit zero, got: %v\n%s", execErr, output)
	}
	for _, want := range []string{"Fleet:    OK", "version 0.2.0 at or above the 0.1.4 floor", "0 failing"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "FAILED") {
		t.Errorf("a fully covered fleet must not print FAILED, got:\n%s", output)
	}
}

// TestStatusCommand_AllServers_FleetPortMismatch pins the las-01 :19090 shape:
// a registered host whose registry URL uses a port the host is not declared to
// serve is a loud PORT_MISMATCH, and a host that is ALSO down still reports the
// port defect rather than only "unreachable" — the registration is wrong either
// way.
func TestStatusCommand_AllServers_FleetPortMismatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// A closed port: the host is down AND misregistered.
	dead := "http://127.0.0.1:1"
	withFleetInventory(t, inventoryFrom(t, `
version: 1
port_convention: 10001
default_min_daemon_version: "0.1.4"
hosts:
  - name: bunker-las-01
    address: 100.78.165.52
    role: agent-host
    ports: [10001, 10002]
    port_note: >-
      Measured live: this host serves a single listener on :18080; the registry
      entry points at :19090, a port it has never listened on.
`))

	cfg := &CLIConfig{
		ActiveServer: "bunker-las-01",
		Servers:      map[string]ServerEntry{"bunker-las-01": {Name: "bunker-las-01", URL: dead, Token: "t"}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewStatusCommand()
	cmd.SetArgs([]string{"--server", "default", "--all"})
	var execErr error
	output := captureStdout(t, func() { execErr = cmd.Execute() })

	if execErr == nil {
		t.Fatalf("a port mismatch must exit non-zero, got:\n%s", output)
	}
	for _, want := range []string{
		"PORT_MISMATCH",
		"uses port 1, which is not a declared port for this host (declared: 10001, 10002)",
		"and it did not answer there either",
		":18080", // the documented discrepancy travels with the finding
		":19090",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	if !strings.Contains(execErr.Error(), "bunker-las-01 PORT_MISMATCH") {
		t.Errorf("exit error %q does not name the mismatch", execErr)
	}
}

// TestStatusCommand_AllServers_FleetUnreachableAndBelowFloor covers the other
// two failure kinds the criterion names, with the hosts on DECLARED ports so
// the status is unambiguous.
func TestStatusCommand_AllServers_FleetUnreachableAndBelowFloor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Down host: a listener that is closed again immediately, so the URL is on a
	// declared port but nothing answers.
	deadSrv := newStatusTestServer(t, &statusMockServer{info: &v1.ServerInfoResponse{Version: "0.1.4"}})
	deadURL := deadSrv.URL
	deadPort := portOfTestServer(t, deadSrv)
	deadSrv.Close()

	oldSrv := newStatusTestServer(t, &statusMockServer{
		info: &v1.ServerInfoResponse{Hostname: "old-host", Version: "0.1.3"},
	})
	defer oldSrv.Close()
	oldPort := portOfTestServer(t, oldSrv)

	withFleetInventory(t, inventoryFrom(t, fmt.Sprintf(`
version: 1
port_convention: %d
default_min_daemon_version: "0.1.4"
hosts:
  - name: down-host
    address: 10.0.0.1
    role: agent-host
    ports: [%d]
  - name: old-host
    address: 10.0.0.2
    role: agent-host
    ports: [%d]
`, deadPort, deadPort, oldPort)))

	cfg := &CLIConfig{
		ActiveServer: "old-host",
		Servers: map[string]ServerEntry{
			"down-host": {Name: "down-host", URL: deadURL, Token: "t"},
			"old-host":  {Name: "old-host", URL: oldSrv.URL, Token: "t"},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewStatusCommand()
	cmd.SetArgs([]string{"--server", "default", "--all"})
	var execErr error
	output := captureStdout(t, func() { execErr = cmd.Execute() })

	if execErr == nil {
		t.Fatalf("an unreachable host and a below-floor daemon must exit non-zero, got:\n%s", output)
	}
	for _, want := range []string{
		"UNREACHABLE",
		"on a declared port but its daemon did not answer",
		"BELOW_FLOOR",
		"reports version 0.1.3, below the 0.1.4 floor",
		"fleet-upgrade.sh",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	if !strings.Contains(execErr.Error(), "2 of 2") {
		t.Errorf("exit error %q should report 2 of 2 failing", execErr)
	}
}

// TestStatusCommand_AllServers_FleetRetiredHostIsNotAHole pins the retirement
// path the row's own acceptance names ("or explicitly decommissioned"): a
// declared-retired host is reported as information and does not fail the audit.
func TestStatusCommand_AllServers_FleetRetiredHostIsNotAHole(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := newStatusTestServer(t, &statusMockServer{
		info: &v1.ServerInfoResponse{Hostname: "live-host", Version: "0.1.4"},
	})
	defer srv.Close()
	port := portOfTestServer(t, srv)

	withFleetInventory(t, inventoryFrom(t, fmt.Sprintf(`
version: 1
port_convention: %d
default_min_daemon_version: "0.1.4"
hosts:
  - name: live-host
    address: 10.0.0.1
    role: agent-host
    ports: [%d]
  - name: retired-host
    address: 10.0.0.9
    role: agent-host
    ports: [10001]
    retired: true
    note: decommissioned and removed from DNS
`, port, port)))

	cfg := &CLIConfig{
		ActiveServer: "live-host",
		Servers:      map[string]ServerEntry{"live-host": {Name: "live-host", URL: srv.URL, Token: "t"}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewStatusCommand()
	cmd.SetArgs([]string{"--server", "default", "--all"})
	var execErr error
	output := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("a retired declaration must not fail the audit, got: %v\n%s", execErr, output)
	}
	for _, want := range []string{"── retired-host ──", "RETIRED", "no live daemon is required", "0 failing"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "NOT_REGISTERED") {
		t.Errorf("a retired host must not be reported as a hole, got:\n%s", output)
	}
}

// TestStatusCommand_AllServers_FleetKeepsRegistryOnlySections guards the
// additive half: servers the operator registered that the inventory does not
// declare keep their own sections (the overview grew coverage, it did not stop
// reporting the operator's other servers).
func TestStatusCommand_AllServers_FleetKeepsRegistryOnlySections(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := newStatusTestServer(t, &statusMockServer{
		info: &v1.ServerInfoResponse{Hostname: "unsigned-host", Version: "1.0.0"},
	})
	defer srv.Close()

	withFleetInventory(t, inventoryFrom(t, `
version: 1
port_convention: 10001
default_min_daemon_version: "0.1.4"
hosts:
  - name: some-other-host
    address: 10.0.0.5
    role: agent-host
    ports: [10001]
    retired: true
`))

	cfg := &CLIConfig{
		ActiveServer: "unofficial",
		Servers:      map[string]ServerEntry{"unofficial": {Name: "unofficial", URL: srv.URL, Token: "t"}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewStatusCommand()
	cmd.SetArgs([]string{"--server", "default", "--all"})
	var execErr error
	output := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("Execute: %v\n%s", execErr, output)
	}
	for _, want := range []string{"── unofficial ──", "unsigned-host", "1.0.0", "1 servers registered", "1 declared in the fleet inventory"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
}

// TestStatusCommand_AllServers_FleetBrokenInventoryFailsLoudly: an inventory
// that cannot be loaded is a broken build artifact, never something to degrade
// past silently (a silently-skipped audit is the defect class here).
func TestStatusCommand_AllServers_FleetBrokenInventoryFailsLoudly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := newStatusTestServer(t, &statusMockServer{info: &v1.ServerInfoResponse{Version: "0.1.4"}})
	defer srv.Close()

	saved := loadFleetInventory
	loadFleetInventory = func() (*fleet.Inventory, error) { return nil, fmt.Errorf("inventory unreadable") }
	t.Cleanup(func() { loadFleetInventory = saved })

	cfg := &CLIConfig{
		ActiveServer: "default",
		Servers:      map[string]ServerEntry{"default": {Name: "default", URL: srv.URL, Token: "t"}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewStatusCommand()
	cmd.SetArgs([]string{"--server", "default", "--all"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--all must fail loudly when the fleet inventory cannot be loaded")
	}
	if !strings.Contains(err.Error(), "load fleet inventory") {
		t.Errorf("error %q does not name the inventory", err)
	}
}

// portOfTestServer returns the port an httptest server listens on, which is
// what a declaration must name for the host to be "covered".
func portOfTestServer(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	port, ok := fleet.PortOf(srv.URL)
	if !ok {
		t.Fatalf("test server URL %q carries no usable port", srv.URL)
	}
	return port
}
