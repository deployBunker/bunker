// example_bind_discipline_test.go — SEC-BUNKER-001 docs pin + QA-BUNKER-74
// client/daemon rest_addr parity pin.
//
// The daemon's built-in listen defaults are the bare wildcard (":9090" /
// ":8080", internal/config/config.go), which binds EVERY interface of the host.
// On a host with a public interface that publishes the whole control plane to
// the open internet, with the static master token as the only barrier —
// verified live on bunker-mvp (78.46.173.180) on 2026-10-10: wildcard binds,
// ufw rules from Anywhere, and a 200 on GET /healthz with no credentials from
// off the tailnet. docs/threat-model.md §9 states the exposure, the fix
// direction, and the runbook for an existing deployment.
//
// The shipped example is the copy-paste surface operators start from, so it
// must teach the safe choice. This test anchors it to three properties:
//
//  1. the example PARSES through the daemon's own loader and NEITHER listen
//     address is a wildcard / "*" / ":PORT"-shaped bind (SEC-BUNKER-001);
//  2. the example DOCUMENTS the rule on the lines that carry the addresses
//     (the wildcard hazard, the SEC-BUNKER-001 id, the threat-model pointer),
//     so the guidance cannot silently regress to bind-any advice; and
//  3. the example's REST address is a REACHABLE, client-dialable bind —
//     exactly one concrete host:port — not the loopback literal the example
//     shipped before QA-BUNKER-74. A loopback-only rest_addr is the silent-
//     unreachable class QA-BUNKER-74 swept (off-by-one corpus 2759): SEC-001
//     moved daemons to Tailscale IPs, clients kept dialing 127.0.0.1, and the
//     deterministic "connection refused" was indistinguishable from a daemon
//     outage to fail-closed consumers. The tailnet-CIDR placeholder
//     (100.64.0.1:8080) fails LOUDLY when an operator skips the substitution.
//
// The claims are deliberately line-scoped (the warning must sit in the address
// block, not anywhere in the file) and token-scoped (the words that carry the
// rule must be present), following the precedent of
// internal/docscheck/disk_semantic_docs_test.go.
//
// RED proof for the parity pin: point BUNKER_EXAMPLE_CONFIG_PATH at the
// pre-fix revision of the example —
//
//	git show 240f85da:config.example.yaml > /tmp/config-example-prefix.yaml
//	BUNKER_EXAMPLE_CONFIG_PATH=/tmp/config-example-prefix.yaml \
//	    go test ./internal/config/ -run 'BindDiscipline|RESTAddrParity' -count=1
//
// — and the reachable-bind test fails on the loopback literal while the
// client-dial-parity subtests fail ("no dialable rest_addr in the example").
// The env override exists so that proof never has to overwrite the
// working-tree file.
package config

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exampleConfigPathDefault is the shipped example at the repo root. `go test`
// runs with the package directory as the working directory, so the repo root is
// two levels up.
const exampleConfigPathDefault = "../../config.example.yaml"

// exampleConfigPath resolves the example under test, honouring the RED-proof
// override.
func exampleConfigPath() string {
	if p := os.Getenv("BUNKER_EXAMPLE_CONFIG_PATH"); p != "" {
		return p
	}
	return exampleConfigPathDefault
}

// wildcardHosts are the host parts of a listen address that mean "every
// interface". This mirrors the daemon's own rule set (IsLoopbackAddr in
// config.go: an empty host, 0.0.0.0 and :: bind everything) plus the "*"
// spelling that appears in operator documentation.
var wildcardHosts = map[string]bool{
	"":        true,
	"*":       true,
	"0.0.0.0": true,
	"::":      true,
	"[::]":    true,
}

// bindDisciplineWindow is how many lines above the grpc_addr key the warning
// block may start. Line-scoped on purpose: the pin is on the address block, not
// on the file as a whole.
const bindDisciplineWindow = 30

// restAddrParityPlaceholder is the example's tailnet-CIDR documentation
// placeholder. It lives inside 100.64.0.0/10 — the tailnet range the runbook
// binds (docs/threat-model.md §9.3) — and parses as a concrete single-host
// address, so the example is shaped like a real deployment while remaining a
// placeholder an operator must substitute (their own `tailscale ip -4`).
const restAddrParityPlaceholder = "100.64.0.1:8080"

// tailnetCIDR is the CGNAT range Tailscale allocates node addresses from. It
// is spelled out because net/netip's IsPrivate does NOT cover 100.64.0.0/10:
// a tailnet bind is reachable-by-design yet not "private" by that predicate,
// and the example's bind must stay inside it (or private RFC1918 space) so
// the example cannot drift toward teaching a public bind.
var tailnetCIDR = netip.MustParsePrefix("100.64.0.0/10")

func TestExampleConfigBindsAreNotWildcard(t *testing.T) {
	path := exampleConfigPath()

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	if cfg.Server.GRPCAddr == "" || cfg.Server.RESTAddr == "" {
		t.Fatalf("%s must carry both listen addresses, got grpc_addr=%q rest_addr=%q",
			path, cfg.Server.GRPCAddr, cfg.Server.RESTAddr)
	}

	for _, tc := range []struct{ key, addr string }{
		{"grpc_addr", cfg.Server.GRPCAddr},
		{"rest_addr", cfg.Server.RESTAddr},
	} {
		host := tc.addr
		if h, _, splitErr := net.SplitHostPort(tc.addr); splitErr == nil {
			host = h
		}
		if wildcardHosts[host] {
			t.Errorf("%s: %s = %q binds EVERY interface (host %q) — the shipped example must not teach a "+
				"bind-any control plane; bind loopback for the scratch run or the host's tailnet/private "+
				"address (docs/threat-model.md §9, SEC-BUNKER-001)",
				path, tc.key, tc.addr, host)
			continue
		}
	}
}

// TestExampleConfigRESTAddrIsClientDialable is the QA-BUNKER-74 parity pin on
// the example's own rest_addr: the address the daemon listens on is the
// address clients dial, so the example must show exactly one concrete,
// dialable host:port — not the loopback literal the example shipped before
// this row (the silent-unreachable class: a fleet of clients dialing
// 127.0.0.1 against daemons bound to their tailnet address fail with a
// deterministic "connection refused" that fail-closed consumers classify as
// an outage). The tailnet-CIDR placeholder counts as concrete: parsing it
// proves the value carries a real host:port shape an operator substitutes.
func TestExampleConfigRESTAddrIsClientDialable(t *testing.T) {
	path := exampleConfigPath()

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}

	addr := cfg.Server.RESTAddr
	host, port, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		t.Fatalf("%s: rest_addr %q is not host:port — the example must show exactly one "+
			"dialable REST bind (QA-BUNKER-74)", path, addr)
	}
	if port == "" {
		t.Errorf("%s: rest_addr %q has no port — clients cannot dial it", path, addr)
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip == nil {
		t.Errorf("%s: rest_addr host %q is not a concrete IP — the example must show a "+
			"reachable bind (the tailnet placeholder %s), not a wildcard or name",
			path, host, restAddrParityPlaceholder)
	} else if ip.IsLoopback() {
		t.Errorf("%s: rest_addr = %q binds loopback — a client config derived from this "+
			"example cannot reach a daemon bound to its tailnet address (the QA-BUNKER-74 "+
			"silent-unreachable class); show the tailnet bind (%s), not 127.0.0.1",
			path, addr, restAddrParityPlaceholder)
	} else if addr4, ok := netip.AddrFromSlice(ip); !ok || (!addr4.Unmap().IsPrivate() && !tailnetCIDR.Contains(addr4.Unmap())) {
		t.Errorf("%s: rest_addr = %q is neither private nor in the tailnet range %s — the "+
			"example must not teach a public bind (SEC-BUNKER-001)",
			path, addr, tailnetCIDR)
	}
}

// TestClientDialPathDerivesFromRESTAddr and the live-dial proof live in
// internal/cli/restaddr_parity_test.go: the dial path (ServerEntry,
// newBunkerdClientChecked) belongs to the cli package, and cli already
// imports config — an in-package test here could not reference it without an
// import cycle.

func TestExampleConfigDocumentsBindDiscipline(t *testing.T) {
	path := exampleConfigPath()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(raw), "\n")

	addrLine := -1
	for i, line := range lines {
		if strings.Contains(line, "grpc_addr:") {
			addrLine = i
			break
		}
	}
	if addrLine < 0 {
		t.Fatalf("%s: no grpc_addr key found", path)
	}
	start := addrLine - bindDisciplineWindow
	if start < 0 {
		start = 0
	}
	block := strings.ToLower(strings.Join(lines[start:addrLine+1], "\n"))

	// The rule, the hazard named, and the pointer to where the posture is
	// discussed. Deliberately tokens, not exact prose: the requirement is that
	// the guidance is present on the address block, not that it is worded one
	// particular way.
	for _, want := range []struct{ token, why string }{
		{"sec-bunker-001", "the row that states the exposure"},
		{"docs/threat-model.md", "the pointer to the posture discussion"},
		{"wildcard", "the hazard, named (\"bind only for deliberate public exposure\")"},
	} {
		if !strings.Contains(block, want.token) {
			t.Errorf("%s: the grpc_addr/rest_addr block (%d-line window above line %d) must carry the bind "+
				"rule and point at the threat model: missing %q — %s",
				path, bindDisciplineWindow, addrLine+1, want.token, want.why)
		}
	}
}

// TestThreatModelDocumentsSECBunker001 pins the other half of the row: the
// posture discussion the example points at must state the exposure, the fix
// direction (tailnet bind + the tailnet CIDR), and the existing-deployment
// runbook (named commands). Same token discipline as the examples above — this
// is a presence anchor, not a prose paraphrase.
func TestThreatModelDocumentsSECBunker001(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "threat-model.md")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc := strings.ToLower(string(raw))

	for _, want := range []struct{ token, why string }{
		{"sec-bunker-001", "the exposure is named by its row id"},
		{"100.64.0.0/10", "the tailnet CIDR the firewall and the bind rule use"},
		{"ufw limit", "the firewall command that narrows the Anywhere rules"},
		{"systemctl restart bunkerd", "the daemon restart step"},
		{"healthz", "the verification probe on both sides"},
	} {
		if !strings.Contains(doc, want.token) {
			t.Errorf("%s: missing %q — %s", path, want.token, want.why)
		}
	}
}
