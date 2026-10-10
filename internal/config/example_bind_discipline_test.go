// example_bind_discipline_test.go — SEC-BUNKER-001 docs pin.
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
// must teach the safe choice. This test anchors it to two properties, both
// named by the row's acceptance criteria:
//
//  1. the example PARSES through the daemon's own loader and its listen
//     addresses are NOT wildcard / "*" / ":PORT"-shaped binds — they are
//     loopback, which keeps the example valid for the scratch local run it
//     documents and unreachable from any public interface; and
//  2. the example DOCUMENTS the rule on the lines that carry the addresses:
//     the wildcard hazard, the SEC-BUNKER-001 id, and a pointer to the
//     threat-model section — so the guidance cannot silently regress to
//     bind-any advice.
//
// The claims are deliberately line-scoped (the warning must sit in the address
// block, not anywhere in the file) and token-scoped (the words that carry the
// rule must be present), following the precedent of
// internal/docscheck/disk_semantic_docs_test.go.
//
// RED proof: point BUNKER_EXAMPLE_CONFIG_PATH at the pre-fix revision of the
// example —
//
//	git show <pre-fix-sha>:config.example.yaml > /tmp/config-example-prefix.yaml
//	BUNKER_EXAMPLE_CONFIG_PATH=/tmp/config-example-prefix.yaml go test ./internal/config/ -run 'BindDiscipline' -count=1
//
// — and both tests fail: the parser still succeeds, but the addresses are
// ":9090"/":8080" and the warning is gone. The env override exists so that
// proof never has to overwrite the working-tree file.
package config

import (
	"net"
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
		// Not a wildcard, but still routable advice would be wrong here: the
		// example is the scratch-local example, so it binds loopback.
		if ok, resolved := IsLoopbackAddr(tc.addr); !ok {
			t.Errorf("%s: %s = %q is not a loopback bind (host %q) — the shipped example is the scratch "+
				"local example, so it stays 127.0.0.1-shaped; the tailnet/private rule is guidance for "+
				"real deployments (docs/threat-model.md §9, SEC-BUNKER-001)",
				path, tc.key, tc.addr, resolved)
		}
	}
}

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
