// Package fleet holds the committed fleet inventory: which bunker hosts the
// fleet declares, on which ports each is expected to answer, and the minimum
// daemon version each must run.
//
// WHY IT EXISTS (GAP-083). The CLI's server registry
// (~/.config/bunker/config.yaml) is per-operator state written by
// `bunker connect`. A host that serves agents but was never registered — or
// was registered on a port its daemon does not listen on — is invisible to
// every status surface: bunker-las-01 ran an ACTIVE daemon for 32+ days while
// `bunker status --all` silently omitted it, and the one entry it did have
// pointed at :19090, a port the host never listened on.
//
// The inventory in inventory.yaml (embedded, so a `go install` binary carries
// it) is the fleet's CONTRACT. `bunker status --all` reconciles the local
// registry against it: a declared host that is missing from the registry,
// unreachable, answering on an undeclared port, or below the daemon version
// floor is reported as a loud failure with a non-zero exit.
//
// The declaration is a contract, not a snapshot: a declared port is the port
// the host is SUPPOSED to answer on, so drift is reported rather than
// absorbed.
package fleet

import (
	_ "embed"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/deployBunker/bunker/internal/hostsetup"
)

//go:embed inventory.yaml
var embeddedInventory []byte

// MinDaemonVersion is the daemon version floor every declared host must meet.
// It IS the repo's own floor (internal/hostsetup.MinDaemonVersion), so the
// inventory audit and `bunker host-provision`'s skew probe cannot drift apart;
// the wiring is pinned by a test.
const MinDaemonVersion = hostsetup.MinDaemonVersion

// Host is one declared fleet host.
type Host struct {
	// Name is the server name the registry must use (`bunker connect <name>`).
	// It is the match key between the declaration and the local registry.
	Name string `yaml:"name"`
	// Address is the host's address on the fleet network (advisory: a registry
	// entry may legitimately use a DNS name or the host's public address, so
	// only the PORT is checked against this declaration).
	Address string `yaml:"address"`
	// Role is what the host is for (agent-host, demo, ...).
	Role string `yaml:"role"`
	// Ports are the ports this host is declared to serve, in preference order.
	Ports []int `yaml:"ports"`
	// MinVersion overrides MinDaemonVersion for this host when non-empty.
	MinVersion string `yaml:"min_version"`
	// PortNote documents a port layout that deviates from the convention (or a
	// known discrepancy under repair). It is printed with a port verdict.
	PortNote string `yaml:"port_note"`
	// Note is free-form context for operators and docs.
	Note string `yaml:"note"`
	// Retired declares the host decommissioned: it is reported as information,
	// never as a coverage failure, so retiring a host stops it being a hole
	// without deleting the record of it.
	Retired bool `yaml:"retired"`
}

// Floor is the daemon version floor that applies to this host.
func (h Host) Floor() string {
	if h.MinVersion != "" {
		return h.MinVersion
	}
	return MinDaemonVersion
}

// AcceptsPort reports whether port is one this host is declared to serve.
func (h Host) AcceptsPort(port int) bool {
	for _, p := range h.Ports {
		if p == port {
			return true
		}
	}
	return false
}

// PortList renders the declared ports as "10001, 10002" for messages.
func (h Host) PortList() string {
	parts := make([]string, 0, len(h.Ports))
	for _, p := range h.Ports {
		parts = append(parts, fmt.Sprintf("%d", p))
	}
	return strings.Join(parts, ", ")
}

// Inventory is the parsed inventory document.
type Inventory struct {
	Version                 int    `yaml:"version"`
	PortConvention          int    `yaml:"port_convention"`
	DefaultMinDaemonVersion string `yaml:"default_min_daemon_version"`
	Hosts                   []Host `yaml:"hosts"`
}

// Load parses the embedded inventory — the committed fleet declaration.
func Load() (*Inventory, error) {
	return Parse(embeddedInventory)
}

// Parse parses an inventory document. It validates the declaration so a
// malformed edit fails loudly at load time rather than producing a silent
// coverage gap (a host with no name or no port would otherwise check nothing).
func Parse(data []byte) (*Inventory, error) {
	var inv Inventory
	if err := yaml.Unmarshal(data, &inv); err != nil {
		return nil, fmt.Errorf("parse fleet inventory: %w", err)
	}
	if inv.Version != 1 {
		return nil, fmt.Errorf("fleet inventory: unsupported version %d (want 1)", inv.Version)
	}
	if len(inv.Hosts) == 0 {
		return nil, fmt.Errorf("fleet inventory: no hosts declared")
	}
	seen := make(map[string]bool, len(inv.Hosts))
	for i, h := range inv.Hosts {
		switch {
		case strings.TrimSpace(h.Name) == "":
			return nil, fmt.Errorf("fleet inventory: host %d has no name", i)
		case seen[h.Name]:
			return nil, fmt.Errorf("fleet inventory: host %q is declared twice", h.Name)
		case strings.TrimSpace(h.Role) == "":
			return nil, fmt.Errorf("fleet inventory: host %q has no role", h.Name)
		case len(h.Ports) == 0:
			return nil, fmt.Errorf("fleet inventory: host %q declares no ports", h.Name)
		}
		seen[h.Name] = true
		for _, p := range h.Ports {
			if p < 1 || p > 65535 {
				return nil, fmt.Errorf("fleet inventory: host %q declares an out-of-range port %d", h.Name, p)
			}
		}
	}
	return &inv, nil
}

// Sorted returns the declared hosts ordered by name, the order every surface
// renders them in.
func (inv *Inventory) Sorted() []Host {
	if inv == nil {
		return nil
	}
	out := make([]Host, len(inv.Hosts))
	copy(out, inv.Hosts)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns the declared host names, sorted.
func (inv *Inventory) Names() []string {
	hosts := inv.Sorted()
	names := make([]string, 0, len(hosts))
	for _, h := range hosts {
		names = append(names, h.Name)
	}
	return names
}

// Lookup returns the declaration for name.
func (inv *Inventory) Lookup(name string) (Host, bool) {
	if inv == nil {
		return Host{}, false
	}
	for _, h := range inv.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return Host{}, false
}

// PortOf extracts the explicit port from a registry URL. ok is false when the
// URL carries no port or cannot be parsed — a registry entry with no explicit
// port cannot be checked against the declaration, which is itself a finding.
func PortOf(rawURL string) (port int, ok bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return 0, false
	}
	if u.Port() == "" {
		return 0, false
	}
	p, err := parsePort(u.Port())
	if err != nil {
		return 0, false
	}
	return p, true
}

func parsePort(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a port: %q", s)
		}
		n = n*10 + int(r-'0')
		if n > 65535 {
			return 0, fmt.Errorf("port out of range: %q", s)
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("empty port")
	}
	return n, nil
}

// Status is the coverage verdict for one declared host.
type Status string

const (
	// StatusOK: declared, registered, reachable, on a declared port, at or
	// above the floor.
	StatusOK Status = "OK"
	// StatusRetired: declared decommissioned — information, not a failure.
	StatusRetired Status = "RETIRED"
	// StatusNotRegistered: declared but absent from the local registry, so no
	// client can select it by name. This is the las-01 hole.
	StatusNotRegistered Status = "NOT_REGISTERED"
	// StatusUnreachable: registered but its daemon did not answer.
	StatusUnreachable Status = "UNREACHABLE"
	// StatusPortMismatch: registered on a port the host is not declared to
	// serve (the las-01 :19090 entry).
	StatusPortMismatch Status = "PORT_MISMATCH"
	// StatusBelowFloor: reachable but running a daemon older than the floor.
	StatusBelowFloor Status = "BELOW_FLOOR"
	// StatusVersionUnproven: reachable but the daemon reported no usable
	// version, so the floor cannot be proved either way.
	StatusVersionUnproven Status = "VERSION_UNPROVEN"
)

// Observation is what the local registry + one live query know about a host.
// The zero value means "no registry entry at all".
type Observation struct {
	Registered bool
	URL        string
	Reachable  bool
	Version    string
}

// Verdict is the reconciliation outcome for one declared host.
type Verdict struct {
	Host   Host
	Status Status
	// Detail is the operator-facing explanation, naming the facts that
	// produced the status.
	Detail string
}

// OK reports whether the verdict is not a coverage failure.
func (v Verdict) OK() bool { return v.Status == StatusOK || v.Status == StatusRetired }

// Failed reports whether the verdict is a coverage failure.
func (v Verdict) Failed() bool { return !v.OK() }

// Reconcile checks every declared host against what was observed, in sorted
// name order. The checks are ordered so the reported status names the FIRST
// thing that makes the host unusable to a client: not registered (no client
// can even select it) beats a registration on a port the host is not declared
// to serve — both are local, static facts about the registry, and the port
// check is deliberately evaluated BEFORE reachability, because
// bunker-las-01's :19090 entry was wrong whether or not the host happened to
// be up — beats unreachable (selected and addressed correctly, but silent)
// beats a version below the floor.
func (inv *Inventory) Reconcile(observed map[string]Observation) []Verdict {
	hosts := inv.Sorted()
	verdicts := make([]Verdict, 0, len(hosts))
	for _, h := range hosts {
		verdicts = append(verdicts, reconcileHost(h, observed[h.Name]))
	}
	return verdicts
}

func reconcileHost(h Host, o Observation) Verdict {
	if h.Retired {
		return Verdict{Host: h, Status: StatusRetired,
			Detail: "declared retired in the fleet inventory; no live daemon is required"}
	}
	if !o.Registered {
		return Verdict{Host: h, Status: StatusNotRegistered,
			Detail: fmt.Sprintf("declared in the fleet inventory (%s, %s ports %s) but absent from the client registry on this host, so no client can select it by name",
				h.Role, h.Address, h.PortList())}
	}
	// Port checks are static (registry entry vs declaration) and come before
	// the network result: a wrong registration is a defect even when the host
	// is also down, and reporting only "unreachable" would hide it.
	port, ok := PortOf(o.URL)
	if !ok {
		return Verdict{Host: h, Status: StatusPortMismatch,
			Detail: fmt.Sprintf("registry URL %s carries no usable port; the host is declared on port(s) %s%s",
				o.URL, h.PortList(), unreachableSuffix(o)) + portNoteSuffix(h)}
	}
	if !h.AcceptsPort(port) {
		return Verdict{Host: h, Status: StatusPortMismatch,
			Detail: fmt.Sprintf("registry URL %s uses port %d, which is not a declared port for this host (declared: %s)%s",
				o.URL, port, h.PortList(), unreachableSuffix(o)) + portNoteSuffix(h)}
	}
	if !o.Reachable {
		return Verdict{Host: h, Status: StatusUnreachable,
			Detail: fmt.Sprintf("registered at %s on a declared port but its daemon did not answer", o.URL)}
	}
	if strings.TrimSpace(o.Version) == "" {
		return Verdict{Host: h, Status: StatusVersionUnproven,
			Detail: fmt.Sprintf("the daemon reported no version, so the %s floor cannot be proved", h.Floor())}
	}
	if !hostsetup.VersionAtLeast(o.Version, h.Floor()) {
		return Verdict{Host: h, Status: StatusBelowFloor,
			Detail: fmt.Sprintf("the daemon reports version %s, below the %s floor — upgrade it with scripts/fleet-upgrade.sh", o.Version, h.Floor())}
	}
	return Verdict{Host: h, Status: StatusOK,
		Detail: fmt.Sprintf("registered at %s, reachable, port %d declared, version %s at or above the %s floor", o.URL, port, o.Version, h.Floor())}
}

// unreachableSuffix records the live half next to a static port finding, so a
// host that is ALSO down does not look like a reachable-but-misconfigured one.
func unreachableSuffix(o Observation) string {
	if o.Reachable {
		return ""
	}
	return " — and it did not answer there either"
}

// portNoteSuffix appends the host's declared port note (the reason a layout
// deviates) to a port-mismatch detail, so the operator reads the explanation
// next to the finding.
func portNoteSuffix(h Host) string {
	if strings.TrimSpace(h.PortNote) == "" {
		return ""
	}
	return " — " + collapseWhitespace(h.PortNote)
}

// collapseWhitespace folds a YAML block scalar into one line so it can be
// printed inside a status line.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
