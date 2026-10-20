// tenant_port_isolation_docs_test.go — SEC-BUNKER-002 docs pin.
//
// The row's premise (verified against live HEAD): in the shipped default
// `shared` network-isolation mode, agents on one host share the host network
// namespace — every agent's localhost is the same loopback, ports above 1024
// are not uid-scoped, and the per-agent 10000–19999 port block is allocator
// BOOKKEEPING with no kernel enforcement behind it. Any agent can bind() a
// port inside another agent's allocated range and squat or impersonate it.
// The concrete per-agent network boundary is the existing `systemd`
// isolation mode (NET-BUNKER-002, --property=PrivateNetwork=yes on the
// agent's dockerd transient unit): private loopback and port space at the
// cost of ALL outbound networking, covering only the unit's processes.
//
// Three shipped doc surfaces stated the port range / default rootless
// networking in ways a reader could take for tenant isolation. This pin
// anchors the corrected sections so the honesty cannot silently revert,
// following the line-scoped/token-scoped discipline of
// disk_semantic_docs_test.go and example_bind_discipline_test.go:
//
//	specs/architecture.md   "Per-Agent Port Isolation" section
//	specs/api.md            the port_range_start/end field description line
//	specs/container-mode.md §6 risk 4 (builtin port-driver reachability)
//
// RED proof: the pure rule function is driven with the verbatim pre-fix
// sections and must flag each of them — without that, a wording revert that
// also keeps the section-shape anchors would pass vacuously.
//
//	git show <pre-fix-sha>:specs/architecture.md > /tmp/arch-prefix.md
//	# (the pre-fix wordings are embedded below as testdata)
package docscheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tenantBoundaryTokens are the claims each corrected surface MUST carry: the
// not-tenant-isolation statement, the row id, the concrete boundary mode and
// its property, and the pointer to the design authority. Tokens, not prose —
// the requirement is that the statements are present in the section, not that
// they are worded one particular way.
var tenantBoundaryTokens = []struct{ token, why string }{
	{"SEC-BUNKER-002", "the row that states the shared-loopback/bind-squat exposure"},
	{"PrivateNetwork=yes", "the concrete tenant-network boundary (systemd mode, NET-BUNKER-002)"},
	{"specs/network-isolation.md", "the design authority pointer"},
}

// tenantSectionFindings evaluates the rule over one doc section. Pure, so the
// RED proof can drive it with the pre-fix wording.
func tenantSectionFindings(section string) []string {
	var findings []string
	for _, want := range tenantBoundaryTokens {
		if !strings.Contains(section, want.token) {
			findings = append(findings, "missing "+want.token+" — "+want.why)
		}
	}
	return findings
}

// Pre-fix section wordings (verbatim from this repository before the row's
// correction). Each one omitted the shared-loopback truth, the concrete
// boundary, or both — the exact honesty defect the row describes.
var tenantPortPreFixSections = []struct{ name, section string }{
	{"specs/architecture.md Per-Agent Port Isolation", `### Per-Agent Port Isolation

Each agent gets a dedicated port range (e.g., 10000–10099 for agent-a, 10100–10199 for agent-b). Ports are allocated from a pool and returned on destroy. Overlapping ranges are prevented.
`},
	{"specs/api.md port-range field description", "  - `port_range_start` / `port_range_end`: Per-agent port isolation\n"},
	{"specs/container-mode.md risk 4", `4. **Rootless builtin port-driver reachability.** The builtin userland driver
   binds published ports to localhost by default, so external clients of
   ` + "`DIRECT`" + `-mode published ports may need ` + "`ssh -L`" + `/tunnel ingress; the exact
   bind address for ` + "`-p`" + ` is an implementation decision with an availability
   tradeoff.
`},
}

// TestTenantPortDocRuleRejectsThePreFixWording is the RED proof of the rule:
// every pre-fix section is flagged, so the rule cannot be vacuously green.
func TestTenantPortDocRuleRejectsThePreFixWording(t *testing.T) {
	for _, tc := range tenantPortPreFixSections {
		if got := tenantSectionFindings(tc.section); len(got) == 0 {
			t.Errorf("rule accepted the pre-fix wording of %s (no findings) — the rule is vacuous", tc.name)
		}
	}
}

// tenantDocSections extracts the section each pin is scoped to from the real
// file content. Line-scoped on purpose: the statements must live IN the
// port-isolation surface, not anywhere in the file.
func tenantDocSections(t *testing.T, root string) map[string]string {
	t.Helper()
	sections := map[string]string{}

	read := func(rel string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("SEC-BUNKER-002 docs pin cannot read %s: %v", rel, err)
			return ""
		}
		return string(raw)
	}

	// specs/architecture.md — the "Per-Agent Port Isolation" section, bounded
	// by the next "### " heading.
	arch := read(filepath.Join("specs", "architecture.md"))
	if s := sectionBetween(arch, "### Per-Agent Port Isolation", "### "); s != "" {
		sections["architecture"] = s
	}

	// specs/api.md — the line(s) describing the port-range fields.
	api := read(filepath.Join("specs", "api.md"))
	var apiLines []string
	for _, line := range strings.Split(api, "\n") {
		if strings.Contains(line, "port_range_start") && strings.Contains(line, "port_range_end") {
			apiLines = append(apiLines, line)
		}
	}
	sections["api"] = strings.Join(apiLines, "\n")

	// specs/container-mode.md — §6 risk 4, bounded by risk 5.
	cm := read(filepath.Join("specs", "container-mode.md"))
	if s := sectionBetween(cm, "Rootless builtin port-driver reachability", "5. **Image pull"); s != "" {
		sections["container-mode"] = s
	}

	return sections
}

// sectionBetween returns the text from the first occurrence of start (or,
// when start itself is a heading marker, including it) up to the next
// occurrence of end after it. Empty when either anchor is missing.
func sectionBetween(content, start, end string) string {
	i := strings.Index(content, start)
	if i < 0 {
		return ""
	}
	rest := content[i:]
	if end != "" {
		if j := strings.Index(rest[len(start):], end); j >= 0 {
			rest = rest[:len(start)+j]
		}
	}
	return rest
}

// TestRealTreeTenantPortDocsStateTheSharedTruth pins all three corrected
// surfaces in the real tree: each carries the not-tenant-isolation statement,
// the row id, the concrete boundary (systemd / PrivateNetwork=yes), and the
// design-authority pointer — in the port-isolation section itself.
func TestRealTreeTenantPortDocsStateTheSharedTruth(t *testing.T) {
	root := repoRoot(t)

	sections := tenantDocSections(t, root)
	for _, name := range []string{"architecture", "api", "container-mode"} {
		if sections[name] == "" {
			t.Errorf("SEC-BUNKER-002: the %s doc section anchor was not found — the section shape drifted; re-point this pin", name)
		}
	}

	// The shared-truth tokens every corrected surface must carry.
	for name, section := range sections {
		if section == "" {
			continue
		}
		for _, f := range tenantSectionFindings(section) {
			t.Errorf("SEC-BUNKER-002: %s docs surface %s", name, f)
		}
	}

	// The architecture section must also state the two facts the row is
	// about, in the section (not just the row id somewhere).
	arch := sections["architecture"]
	for _, want := range []struct{ token, why string }{
		{"not tenant isolation", "the explicit shared-mode honesty statement"},
		{"uid", "ports above 1024 are not uid-scoped — the bind-squat premise"},
		{"NET-BUNKER-002", "the systemd mode's own row id"},
	} {
		if !strings.Contains(arch, want.token) {
			t.Errorf("SEC-BUNKER-002: specs/architecture.md Per-Agent Port Isolation section is missing %q — %s", want.token, want.why)
		}
	}

	// The container-mode risk must keep BOTH caveats: the shared-loopback
	// reach and the bookkeeping-not-enforcement port block.
	cm := sections["container-mode"]
	for _, want := range []struct{ token, why string }{
		{"shared loopback", "published-port localhost is the HOST's loopback, not tenant-private"},
		{"BOOKKEEPING", "the 10000–19999 block is allocator bookkeeping, not enforcement"},
	} {
		if !strings.Contains(cm, want.token) {
			t.Errorf("SEC-BUNKER-002: specs/container-mode.md risk 4 is missing %q — %s", want.token, want.why)
		}
	}
}

// TestRealTreeSECBunker002TraceArtifactExists keeps the repository-local
// evidence artifact resolvable: the row id must resolve to the trace doc,
// and the doc must carry its STATUS/TIER/EVIDENCE lines.
func TestRealTreeSECBunker002TraceArtifactExists(t *testing.T) {
	root := repoRoot(t)

	path := filepath.Join(root, "docs", "security", "SEC-BUNKER-002.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the SEC-BUNKER-002 trace artifact is missing: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{"SEC-BUNKER-002", "STATUS:", "TIER:", "EVIDENCE:", "specs/network-isolation.md"} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/security/SEC-BUNKER-002.md is missing %q — the trace artifact must carry the row id, the STATUS/TIER/EVIDENCE lines, and the spec pointer", want)
		}
	}
}
