package provisioning

import (
	"strings"
	"testing"
)

// TestCatalogIsSURF009Set pins the DECLARED set itself: the five tools the
// row measured absent on a fresh agent. Adding an entry is a spec change —
// this test is the alarm.
func TestCatalogIsSURF009Set(t *testing.T) {
	want := []string{"go", "gcc", "make", "node", "cargo"}
	got := CatalogNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("CatalogNames() = %v, want %v", got, want)
	}
	for _, tc := range Catalog() {
		if !tc.Required {
			t.Errorf("toolchain %s: every SURF-009 entry is Required", tc.Name)
		}
		if tc.Builds == "" || tc.NeededBy == "" {
			t.Errorf("toolchain %s: report-facing fields must not be empty", tc.Name)
		}
		switch tc.Name {
		case "go", "node":
			if tc.Kind != KindTarball {
				t.Errorf("toolchain %s: kind %q, want %q", tc.Name, tc.Kind, KindTarball)
			}
		case "gcc", "make":
			if tc.Kind != KindDebExtract {
				t.Errorf("toolchain %s: kind %q, want %q", tc.Name, tc.Kind, KindDebExtract)
			}
		case "cargo":
			if tc.Kind != KindRustup {
				t.Errorf("toolchain %s: kind %q, want %q", tc.Name, tc.Kind, KindRustup)
			}
		}
		if tc.Script == nil {
			t.Errorf("toolchain %s: missing delivery stage", tc.Name)
		}
	}
}

// TestStageScriptsHaveNoUnquotedInterpolation guards the shell-injection
// shape: no Go string interpolation outside quoteSh may reach the scripts.
// The scripts carry exactly two substitution mechanisms: the tab verdict
// format (printf) and backtick-concatenated checksum constants.
func TestStageScriptsHaveNoUnquotedInterpolation(t *testing.T) {
	for _, tc := range Catalog() {
		s := tc.Script()
		if strings.Contains(s, `name="`+tc.Name+`"`) {
			// name must be set through quoteSh (single-quoted), never raw.
			t.Errorf("stage %s: name assignment bypasses quoteSh", tc.Name)
		}
		if !strings.Contains(s, "printf '%s\\tINSTALLED\\t%s\\n' \"$name\"") {
			t.Errorf("stage %s: missing INSTALLED verdict line", tc.Name)
		}
		if !strings.Contains(s, "printf '%s\\tFAILED\\t") {
			t.Errorf("stage %s: missing FAILED verdict lines", tc.Name)
		}
		if !strings.Contains(s, "already-present") {
			t.Errorf("stage %s: missing idempotent re-run probe", tc.Name)
		}
	}
}

// TestStagePinsAreEmbedded asserts each pinned artifact's checksum is
// embedded in the exact stage that downloads it, and that download URLs are
// the publisher origins.
func TestStagePinsAreEmbedded(t *testing.T) {
	goScript := goToolchainStage()
	for _, sha := range []string{GoToolchainSHA256AMD64, GoToolchainSHA256ARM64} {
		if !strings.Contains(goScript, sha) {
			t.Errorf("go stage does not embed %s", sha[:12])
		}
	}
	if !strings.Contains(goScript, dlGoBase+GoToolchainVersion) {
		t.Errorf("go stage URL must be %s%s", dlGoBase, GoToolchainVersion)
	}

	nodeScript := nodeToolchainStage()
	for _, sha := range []string{NodeToolchainSHA256X64, NodeToolchainSHA256ARM64} {
		if !strings.Contains(nodeScript, sha) {
			t.Errorf("node stage does not embed %s", sha[:12])
		}
	}
	if !strings.Contains(nodeScript, nodeBase+NodeToolchainVersion+"/node-") {
		t.Errorf("node stage URL must be %s%s", nodeBase, NodeToolchainVersion)
	}

	cargoScript := cargoToolchainStage()
	for _, sha := range []string{RustupInitSHA256X86_64, RustupInitSHA256AARCH64} {
		if !strings.Contains(cargoScript, sha) {
			t.Errorf("cargo stage does not embed %s", sha[:12])
		}
	}
	if !strings.Contains(cargoScript, rustupBase) || !strings.Contains(cargoScript, RustToolchainVersion) {
		t.Errorf("cargo stage must pin rustup + toolchain %s", RustToolchainVersion)
	}
	if !strings.Contains(cargoScript, "--no-modify-path") {
		t.Errorf("cargo stage must not touch shell profiles")
	}
}

// (The Go-pin-vs-internal/cli cross-check lives in internal/cli's
// toolchain_check_test.go: this package must stay a leaf, so the pin
// comparison runs where BOTH constants are importable.)

// TestProbeScriptShape checks the probe program: all five names in order,
// replaced exactly once, verdict lines preserved.
func TestProbeScriptShape(t *testing.T) {
	s := ProbeScript()
	if !strings.HasPrefix(s, "for t in go gcc make node cargo; do") {
		t.Errorf("probe must iterate the declared set in order, got: %.60q", s)
	}
	if strings.Contains(s, "__TOOLS__") {
		t.Errorf("probe still carries the substitution token")
	}
	for _, want := range []string{`printf '%s\tpresent\t%s\n'`, `printf '%s\tabsent\t\n'`} {
		if !strings.Contains(s, want) {
			t.Errorf("probe missing verdict line %q", want)
		}
	}
}

// TestParseProbeOutput is the table over the three states.
func TestParseProbeOutput(t *testing.T) {
	// Partial fixture: go present, gcc ABSENT, make present, node+cargo
	// omitted (truncated run). gcc is absent, the omitted two are unknown —
	// neither may read as present, so AllPresent must be FALSE and all
	// three must be named in Missing.
	r := ParseProbeOutput("agt-1", "go	present	go version go1.25.4 linux/amd64\n"+
		"gcc	absent	\n"+
		"make	present	GNU Make 4.3\n", 0)
	if r.AllPresent {
		t.Errorf("AllPresent = true, want false while gcc is absent")
	}
	if got := strings.Join(r.Missing, ","); got != "cargo,gcc,node" {
		t.Errorf("Missing = %q, want cargo,gcc,node (absent AND unknown)", got)
	}
	if r.Tools[1].Status != "absent" {
		t.Fatalf("gcc status = %q, want absent", r.Tools[1].Status)
	}
	if r.Tools[3].Status != "unknown" || r.Tools[4].Status != "unknown" {
		t.Errorf("omitted entries must be unknown, got %q/%q", r.Tools[3].Status, r.Tools[4].Status)
	}
	if r.Tools[0].Version == "" || !strings.Contains(r.Tools[0].Version, "go1.25.4") {
		t.Errorf("version column lost: %q", r.Tools[0].Version)
	}

	full := ParseProbeOutput("agt-2",
		"go\tpresent\tgo version go1.25.4 linux/amd64\ngcc\tpresent\tn\nmake\tpresent\tn\nnode\tpresent\tn\ncargo\tpresent\tn\n", 0)
	if !full.AllPresent || len(full.Missing) != 0 {
		t.Errorf("full probe: AllPresent=%v Missing=%v, want true/[]", full.AllPresent, full.Missing)
	}

	// Truncated output: an omitted entry is UNKNOWN, and unknown is
	// missing — a truncated probe can never read as clean.
	trunc := ParseProbeOutput("agt-3", "go\tpresent\tgo1\n", 0)
	if trunc.Tools[4].Status != "unknown" {
		t.Errorf("cargo status = %q, want unknown", trunc.Tools[4].Status)
	}
	if trunc.AllPresent {
		t.Errorf("truncated probe must not read AllPresent")
	}
}

// TestParseStageOutput covers the verdict-line contract including the
// unparsable-output failure.
func TestParseStageOutput(t *testing.T) {
	if o := ParseStageOutput("go", "go\tINSTALLED\tgo version go1.25.4 linux/amd64\n"); o.Status != "INSTALLED" || o.Detail == "" {
		t.Errorf("INSTALLED parse: %+v", o)
	}
	if o := ParseStageOutput("gcc", "gcc\tINSTALLED\talready-present\n"); o.Status != "INSTALLED" || o.Detail != "already-present" {
		t.Errorf("already-present parse: %+v", o)
	}
	if o := ParseStageOutput("node", "node\tFAILED\ttoolchain checksum mismatch\n"); o.Status != "FAILED" {
		t.Errorf("FAILED parse: %+v", o)
	}
	o := ParseStageOutput("cargo", "some warning line\nwithout verdict tabs")
	if o.Status != "FAILED" || !strings.Contains(o.Detail, "unparsable output") {
		t.Errorf("unparsable must be FAILED with the output head, got %+v", o)
	}
	o = ParseStageOutput("go", "")
	if o.Status != "FAILED" || !strings.Contains(o.Detail, "no output") {
		t.Errorf("empty output must be FAILED naming (no output), got %+v", o)
	}
}

// TestStageScriptRoundTrip drives StageScript for every catalog name.
func TestStageScriptRoundTrip(t *testing.T) {
	for _, name := range CatalogNames() {
		s, err := StageScript(name)
		if err != nil || s == "" {
			t.Errorf("StageScript(%q): %v", name, err)
		}
	}
	if _, err := StageScript("rg"); err == nil {
		t.Errorf("a non-catalog tool must be refused by name")
	}
}

// TestCacheAndInstallPathsAreAgentLocal guards the no-root and PATH story:
// everything the stages touch lives under $HOME.
func TestCacheAndInstallPathsAreAgentLocal(t *testing.T) {
	for _, tc := range Catalog() {
		s := tc.Script()
		if strings.Contains(s, " /usr") || strings.Contains(s, " /etc") || strings.Contains(s, " /opt") {
			t.Errorf("stage %s writes outside $HOME", tc.Name)
		}
		if !strings.Contains(s, `$HOME/`+InstallDir) || !strings.Contains(s, `$HOME/`+CacheRoot) {
			t.Errorf("stage %s does not use the agent-local PATH/cache slots", tc.Name)
		}
	}
}
