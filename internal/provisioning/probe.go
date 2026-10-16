// The probe and its parser. The probe is ONE remote shell program reporting
// every catalog entry over the same audited exec path as `bunker exec`
// (mirroring internal/cli's agentProbeScript), so the check needs no new
// server surface and no new credential. A tool that exists but refuses to
// report a version is still PRESENT — the report says so rather than
// pretending it is missing; a tool the catalog expects but the output omits
// is UNKNOWN, never silently absent.
//
// The probe ALWAYS asks for the whole catalog (one exec, ~1s), and the
// CLASS decides which entries the verdict covers. Asking per class would
// make the report a different shape per class and hide an absent tool the
// next class needs; reporting everything and requiring a subset keeps one
// command shape and one wire form.
package provisioning

import (
	"fmt"
	"sort"
	"strings"
)

// ProbeScript is the remote shell program for the declared set. The
// __TOOLS__ token is replaced once with the catalog names (strings.Replace,
// never Sprintf — the body carries literal printf verbs).
const probeBody = `for t in __TOOLS__; do
  if command -v "$t" >/dev/null 2>&1; then
    v=""
    for flag in --version version; do
      v=$("$t" $flag 2>/dev/null | head -n 1)
      if [ -n "$v" ]; then break; fi
    done
    printf '%s\tpresent\t%s\n' "$t" "$v"
  else
    printf '%s\tabsent\t\n' "$t"
  fi
done
`

// ProbeScript returns the probe program with the catalog names substituted.
func ProbeScript() string {
	return strings.Replace(probeBody, "__TOOLS__", strings.Join(CatalogNames(), " "), 1)
}

// The two report statuses, uppercase on every surface (human table and JSON
// alike): the SURF-009 acceptance asks for PRESENT/ABSENT by name, and one
// spelling for both output forms is one less thing to get wrong.
const (
	StatusPresent = "PRESENT"
	StatusAbsent  = "ABSENT"
	// StatusUnknown is a catalog entry the probe did not report at all
	// (truncated output). It counts as missing and is never silently absent.
	StatusUnknown = "UNKNOWN"
)

// Finding is one probe result.
type Finding struct {
	Name     string `json:"name"`
	Status   string `json:"status"` // PRESENT | ABSENT | UNKNOWN
	Version  string `json:"version"`
	Kind     string `json:"kind"`
	Builds   string `json:"builds"`
	NeededBy string `json:"needed_by"`
	// Required is CLASS-derived: true when the checked class declares this
	// entry. An absent tool the class does not require is not a failure.
	Required bool `json:"required"`
}

// Report is the whole probe result and the --json wire shape of
// `bunker toolchain-check`.
type Report struct {
	Agent string `json:"agent"`
	// Class is the class this report was checked against.
	Class string `json:"class"`
	// ClassNote is what that class promises, report-facing.
	ClassNote string `json:"class_note"`
	Set       string `json:"set"`
	// Required names the entries the class declares, in catalog order — the
	// list the verdict is about, so a consumer never has to re-derive it.
	Required []string  `json:"required"`
	Tools    []Finding `json:"tools"`
	Missing  []string  `json:"missing"`
	// AllPresent is the acceptance-shaped verdict: every entry the CLASS
	// requires was probed present on this agent.
	AllPresent bool  `json:"all_present"`
	ExitCode   int32 `json:"exit_code"`
}

// SetName is the declared set's report label (the SURF-009 row's five build
// toolchains, plus the baseline interpreter the classes also promise).
const SetName = "SURF-009 build toolchains"

// ParseProbeOutput turns the probe's tab-separated lines into a Report for
// the DEFAULT class (full). An omitted catalog entry is reported UNKNOWN so a
// truncated probe cannot masquerade as a clean bill of health.
func ParseProbeOutput(agentID, output string, exitCode int32) Report {
	return ParseProbeOutputFor(agentID, output, exitCode, defaultClass())
}

// ParseProbeOutputFor is the class-aware parse: the probe output is the whole
// catalog, and class decides which entries are Required and therefore which
// absence fails the verdict.
func ParseProbeOutputFor(agentID, output string, exitCode int32, class Class) Report {
	seen := map[string]string{}   // name -> version
	reported := map[string]bool{} // name -> reported present
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			continue
		}
		name, status := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		version := ""
		if len(parts) == 3 {
			version = strings.TrimSpace(parts[2])
		}
		reported[name] = strings.EqualFold(status, "present")
		seen[name] = version
	}

	report := Report{
		Agent: agentID, Class: class.Name, ClassNote: class.Note, Set: SetName,
		Required: append([]string{}, class.Required...),
		Tools:    []Finding{}, Missing: []string{}, ExitCode: exitCode,
	}
	for _, t := range catalog() {
		isPresent, wasReported := reported[t.Name]
		status := StatusUnknown
		version := ""
		if wasReported {
			version = seen[t.Name]
			if isPresent {
				status = StatusPresent
			} else {
				status = StatusAbsent
			}
		}
		required := class.Requires(t.Name)
		report.Tools = append(report.Tools, Finding{
			Name: t.Name, Status: status, Version: version,
			Kind: string(t.Kind), Builds: t.Builds, NeededBy: t.NeededBy, Required: required,
		})
		if required && status != StatusPresent {
			report.Missing = append(report.Missing, t.Name)
		}
	}
	report.AllPresent = len(report.Missing) == 0
	sort.Strings(report.Missing)
	return report
}

// defaultClass returns the built-in default class. It panics only if the
// package's own table stops containing DefaultClass — a compile-time-shaped
// error that must never reach an operator as a silent fallback.
func defaultClass() Class {
	c, err := LookupClass(DefaultClass)
	if err != nil {
		panic(fmt.Sprintf("provisioning: %v", err))
	}
	return c
}

// StageOutcome is ONE delivery stage's parsed verdict.
type StageOutcome struct {
	Name   string
	Status string // INSTALLED or FAILED
	Detail string // version, already-present, or the agent's own words
}

// ParseStageOutput reads a delivery stage's verdict line. A line it cannot
// parse is a FAILED outcome carrying the output head: the stage may have
// half-run, and "no verdict" must never read as success.
func ParseStageOutput(name, output string) StageOutcome {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || strings.TrimSpace(parts[0]) != name {
			continue
		}
		return StageOutcome{
			Name:   strings.TrimSpace(parts[0]),
			Status: strings.TrimSpace(parts[1]),
			Detail: strings.TrimSpace(parts[2]),
		}
	}
	head := strings.TrimSpace(output)
	if len(head) > 120 {
		head = head[:120] + "..."
	}
	if head == "" {
		head = "(no output)"
	}
	return StageOutcome{Name: name, Status: "FAILED", Detail: "unparsable output: " + head}
}

// StageScript returns the delivery program for one catalog name; a name with
// no stage is an internal contract break, reported as an error. A base-image
// entry has none by construction, and says so in its own words — an agent
// missing python3 is a broken agent, not a delivery we can invent.
func StageScript(name string) (string, error) {
	for _, t := range catalog() {
		if t.Name != name {
			continue
		}
		if t.Script == nil {
			return "", fmt.Errorf("toolchain %q is provided by the base image and has no delivery stage", name)
		}
		return t.Script(), nil
	}
	return "", fmt.Errorf("toolchain %q is not in the declared set", name)
}
