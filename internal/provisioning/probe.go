// The probe and its parser. The probe is ONE remote shell program reporting
// every catalog entry over the same audited exec path as `bunker exec`
// (mirroring internal/cli's agentProbeScript), so the check needs no new
// server surface and no new credential. A tool that exists but refuses to
// report a version is still PRESENT — the report says so rather than
// pretending it is missing; a tool the catalog expects but the output omits
// is UNKNOWN, never silently absent.
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

// Finding is one probe result.
type Finding struct {
	Name     string `json:"name"`
	Status   string `json:"status"` // present | absent | unknown
	Version  string `json:"version"`
	Kind     string `json:"kind"`
	Builds   string `json:"builds"`
	NeededBy string `json:"needed_by"`
	Required bool   `json:"required"`
}

// Report is the whole probe result and the --json wire shape of
// `bunker toolchain-check`.
type Report struct {
	Agent   string    `json:"agent"`
	Set     string    `json:"set"`
	Tools   []Finding `json:"tools"`
	Missing []string  `json:"missing"`
	// AllPresent is the acceptance-shaped verdict: every Required entry of
	// the declared set was probed present on this agent.
	AllPresent bool  `json:"all_present"`
	ExitCode   int32 `json:"exit_code"`
}

// ParseProbeOutput turns the probe's tab-separated lines into a Report. An
// omitted catalog entry is reported `unknown` so a truncated probe cannot
// masquerade as a clean bill of health.
func ParseProbeOutput(agentID, output string, exitCode int32) Report {
	seen := map[string]string{}  // name -> version
	present := map[string]bool{} // name -> reported present
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
		present[name] = status == "present"
		seen[name] = version
	}

	report := Report{
		Agent: agentID, Set: "SURF-009 build toolchains",
		Tools: []Finding{}, Missing: []string{}, ExitCode: exitCode,
	}
	for _, t := range catalog() {
		status, version := "unknown", ""
		if isPresent, reported := present[t.Name]; reported {
			version = seen[t.Name]
			if isPresent {
				status = "present"
			} else {
				status = "absent"
			}
		}
		report.Tools = append(report.Tools, Finding{
			Name: t.Name, Status: status, Version: version,
			Kind: string(t.Kind), Builds: t.Builds, NeededBy: t.NeededBy, Required: t.Required,
		})
		if t.Required && status != "present" {
			report.Missing = append(report.Missing, t.Name)
		}
	}
	report.AllPresent = len(report.Missing) == 0
	sort.Strings(report.Missing)
	return report
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
// no stage is an internal contract break, reported as an error.
func StageScript(name string) (string, error) {
	for _, t := range catalog() {
		if t.Name == name {
			if t.Script == nil {
				return "", fmt.Errorf("toolchain %q has no delivery stage", name)
			}
			return t.Script(), nil
		}
	}
	return "", fmt.Errorf("toolchain %q is not in the declared set", name)
}
