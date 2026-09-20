package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestParseAgentToolOutputAllPresent: a clean agent reports every catalogued
// tool as present with its version, and names nothing missing.
func TestParseAgentToolOutputAllPresent(t *testing.T) {
	out := strings.Join([]string{
		"toolsd\tpresent\ttoolsd version dev",
		"rg\tpresent\trg 14.1.0",
		"git\tpresent\tgit version 2.43.0",
		"jq\tpresent\tjq-1.7",
		"gopls\tpresent\tgolang.org/x/tools/gopls v0.22.0",
	}, "\n")

	r := parseAgentToolOutput("abc12345", out, 0)

	if len(r.Tools) != len(agentToolCatalog) {
		t.Fatalf("tools reported = %d, want %d", len(r.Tools), len(agentToolCatalog))
	}
	if len(r.Missing) != 0 {
		t.Errorf("Missing = %v, want empty", r.Missing)
	}
	if len(r.MissingRequired) != 0 {
		t.Errorf("MissingRequired = %v, want empty", r.MissingRequired)
	}
	if r.Agent != "abc12345" {
		t.Errorf("Agent = %q, want abc12345", r.Agent)
	}
	for _, f := range r.Tools {
		if f.Status != "present" {
			t.Errorf("%s status = %q, want present", f.Name, f.Status)
		}
		if f.Version == "" {
			t.Errorf("%s reported no version; a present tool must carry one here", f.Name)
		}
	}
}

// TestParseAgentToolOutputNamesTheMissing reproduces the MEASURED fresh-agent
// state (git and jq present; toolsd, rg and a language server absent) and
// requires the report to NAME what is missing, split by severity.
func TestParseAgentToolOutputNamesTheMissing(t *testing.T) {
	out := strings.Join([]string{
		"toolsd\tabsent\t",
		"rg\tabsent\t",
		"git\tpresent\tgit version 2.43.0",
		"jq\tpresent\tjq-1.7",
		"gopls\tabsent\t",
	}, "\n")

	r := parseAgentToolOutput("gap092-proof", out, 0)

	wantMissing := []string{"gopls", "rg", "toolsd"} // sorted
	if strings.Join(r.Missing, ",") != strings.Join(wantMissing, ",") {
		t.Errorf("Missing = %v, want %v", r.Missing, wantMissing)
	}
	// gopls is OPTIONAL: it downgrades a capability, it does not break a verb.
	wantRequired := []string{"rg", "toolsd"}
	if strings.Join(r.MissingRequired, ",") != strings.Join(wantRequired, ",") {
		t.Errorf("MissingRequired = %v, want %v (optional tools must not be listed as breaking)",
			r.MissingRequired, wantRequired)
	}
}

// TestParseAgentToolOutputOmittedToolIsUnknown pins the load-bearing honesty
// rule: a tool the catalog expects but the probe did not report is UNKNOWN, not
// absent. Otherwise a truncated or failed probe would read as "nothing wrong".
func TestParseAgentToolOutputOmittedToolIsUnknown(t *testing.T) {
	// rg simply never appears in the output.
	out := "toolsd\tpresent\ttoolsd version dev\ngit\tpresent\tgit version 2.43.0\n"

	r := parseAgentToolOutput("abc", out, 0)

	var rg *agentToolFinding
	for i := range r.Tools {
		if r.Tools[i].Name == "rg" {
			rg = &r.Tools[i]
		}
	}
	if rg == nil {
		t.Fatal("rg missing from the report entirely; every catalog entry must be reported")
	}
	if rg.Status != "unknown" {
		t.Errorf("rg status = %q, want unknown (an unreported tool is not an absent one)", rg.Status)
	}
	if len(r.Missing) != 0 {
		t.Errorf("Missing = %v; an UNKNOWN tool must not be reported as missing", r.Missing)
	}
}

// TestAgentProbeScriptCoversEveryCatalogEntry is the drift guard: the script's
// tool list and the catalog are built from the same slice, and this asserts the
// SUBSTITUTION actually lands (a placeholder typo would otherwise probe nothing
// and report every tool as unknown, silently).
func TestAgentProbeScriptCoversEveryCatalogEntry(t *testing.T) {
	names := make([]string, 0, len(agentToolCatalog))
	for _, dep := range agentToolCatalog {
		names = append(names, dep.Name)
	}
	script := strings.Replace(agentProbeScript, "__TOOLS__", strings.Join(names, " "), 1)

	if strings.Contains(script, "__TOOLS__") {
		t.Fatal("the tool placeholder survived substitution; the probe would check nothing")
	}
	for _, dep := range agentToolCatalog {
		if !strings.Contains(script, dep.Name) {
			t.Errorf("the probe script does not check %q", dep.Name)
		}
	}
	// The version read must try both spellings: git/jq/rg answer --version,
	// gopls answers `version`, toolsd answers its bare subcommand.
	if !strings.Contains(script, "--version") || !strings.Contains(script, "version") {
		t.Error("the probe script must try both --version and version spellings")
	}
}

// TestWriteAgentToolReportNamesTheRemedy: the human form must name the missing
// tools AND tell the operator what to do about them, and it must distinguish a
// REQUIRED absence from an optional one.
func TestWriteAgentToolReportNamesTheRemedy(t *testing.T) {
	out := strings.Join([]string{
		"toolsd\tabsent\t",
		"rg\tabsent\t",
		"git\tpresent\tgit version 2.43.0",
		"jq\tpresent\tjq-1.7",
		"gopls\tabsent\t",
	}, "\n")
	r := parseAgentToolOutput("gap092-proof", out, 0)

	var buf bytes.Buffer
	writeAgentToolReport(&buf, r)
	got := buf.String()

	for _, want := range []string{
		"gap092-proof",
		"MISSING on the agent: gopls, rg, toolsd",
		"REQUIRED",
		"rg, toolsd",
		"$HOME/bin", // the PATH slot that already exists
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q:\n%s", want, got)
		}
	}

	// A clean report must say so rather than printing an empty missing line.
	clean := parseAgentToolOutput("abc", "toolsd\tpresent\tv\trg\tpresent\tv", 0)
	var buf2 bytes.Buffer
	writeAgentToolReport(&buf2, clean)
	if !strings.Contains(buf2.String(), "Every catalogued tool is present") {
		t.Errorf("a clean report must say so:\n%s", buf2.String())
	}
}

// TestAgentToolCatalogInvariants: the catalog itself must stay sane -- unique
// names, every entry explains what needs it, and the REQUIRED set is not empty
// (a catalog where nothing is required cannot justify its own exit contract).
func TestAgentToolCatalogInvariants(t *testing.T) {
	seen := map[string]bool{}
	required := 0
	for _, dep := range agentToolCatalog {
		if dep.Name == "" {
			t.Error("a catalog entry has no name")
		}
		if seen[dep.Name] {
			t.Errorf("duplicate catalog entry %q", dep.Name)
		}
		seen[dep.Name] = true
		if dep.NeededBy == "" {
			t.Errorf("%s does not say which verb needs it", dep.Name)
		}
		if len(dep.VersionCommand) == 0 {
			t.Errorf("%s has no version command", dep.Name)
		}
		if dep.Required {
			required++
		}
	}
	if required == 0 {
		t.Error("no catalog entry is Required; the report could never name a broken verb")
	}
}
