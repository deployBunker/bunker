package provisioning

import (
	"strings"
	"testing"
)

// TestCatalogIsSURF009SetAndBaseline pins the DECLARED set itself: the five
// tools the row measured absent on a fresh agent, plus the interpreter that
// measurement started from. Adding an entry is a spec change — this test is
// the alarm.
func TestCatalogIsSURF009SetAndBaseline(t *testing.T) {
	want := []string{"python3", "go", "gcc", "make", "node", "cargo"}
	got := CatalogNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("CatalogNames() = %v, want %v", got, want)
	}
	for _, tc := range Catalog() {
		if tc.Builds == "" || tc.NeededBy == "" {
			t.Errorf("toolchain %s: report-facing fields must not be empty", tc.Name)
		}
		switch tc.Name {
		case "python3":
			// The baseline: reported, class-required by minimal, and NOT
			// deliverable — a copy path for the image's own interpreter
			// would be a lie about who provides it.
			if tc.Kind != KindBaseImage {
				t.Errorf("python3: kind %q, want %q", tc.Kind, KindBaseImage)
			}
			if tc.Script != nil {
				t.Errorf("python3 must have no delivery stage")
			}
			continue
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

// TestClassesDeclared pins the class table and its two invariants: every
// required name is a catalog token (a typo must not make a class vacuous),
// and every class requires the baseline interpreter.
func TestClassesDeclared(t *testing.T) {
	wantNames := []string{ClassMinimal, ClassGo, ClassFull}
	if got := ClassNames(); strings.Join(got, ",") != strings.Join(wantNames, ",") {
		t.Errorf("ClassNames() = %v, want %v", got, wantNames)
	}
	for _, c := range Classes() {
		if len(c.Required) == 0 {
			t.Errorf("class %s requires nothing — a class that can never fail is not a check", c.Name)
		}
		if c.Note == "" {
			t.Errorf("class %s: report-facing note must not be empty", c.Name)
		}
		for _, n := range c.Required {
			if _, ok := Lookup(n); !ok {
				t.Errorf("class %s requires %q, which is not in the catalog", c.Name, n)
			}
		}
		// Tools() is the class in CATALOG order, whatever order Required is
		// written in.
		var catOrder []string
		for _, tool := range Catalog() {
			if c.Requires(tool.Name) {
				catOrder = append(catOrder, tool.Name)
			}
		}
		if got := namesOf(c.Tools()); strings.Join(got, ",") != strings.Join(catOrder, ",") {
			t.Errorf("class %s Tools() = %v, want catalog order %v", c.Name, got, catOrder)
		}
	}

	minimal, err := LookupClass(ClassMinimal)
	if err != nil {
		t.Fatalf("LookupClass(minimal): %v", err)
	}
	if len(minimal.Required) != 1 || minimal.Required[0] != "python3" {
		t.Errorf("minimal class = %v, want python3 only (the stock image)", minimal.Required)
	}
	goClass, err := LookupClass(ClassGo)
	if err != nil {
		t.Fatalf("LookupClass(go): %v", err)
	}
	if strings.Join(goClass.Required, ",") != "python3,go,make" {
		t.Errorf("go class = %v, want python3,go,make", goClass.Required)
	}
	full, err := LookupClass(ClassFull)
	if err != nil {
		t.Fatalf("LookupClass(full): %v", err)
	}
	if len(full.Required) != len(CatalogNames()) {
		t.Errorf("full class = %v, want every catalog entry", full.Required)
	}
	if _, err := LookupClass(DefaultClass); err != nil {
		t.Errorf("DefaultClass %q is not a declared class: %v", DefaultClass, err)
	}
	if DefaultClass != ClassFull {
		t.Errorf("DefaultClass = %q; the check that answers 'can this agent build me?' must default to full", DefaultClass)
	}
	if _, err := LookupClass(SpawnClass); err != nil {
		t.Errorf("SpawnClass %q is not a declared class: %v", SpawnClass, err)
	}
}

// TestLookupClassRefusesAndNames: an unknown class is an error naming the
// declared ones — never a silent fallback to a default.
func TestLookupClassRefusesAndNames(t *testing.T) {
	for _, bad := range []string{"", "g", "Go", "FULL", "everything"} {
		_, err := LookupClass(bad)
		if err == nil {
			t.Errorf("LookupClass(%q) must fail", bad)
			continue
		}
		for _, want := range ClassNames() {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("LookupClass(%q) error must name the declared class %q: %v", bad, want, err)
			}
		}
	}
}

// TestClassesAreIndependentCopies: mutating a returned class must not
// redefine the declaration for the next caller.
func TestClassesAreIndependentCopies(t *testing.T) {
	first := Classes()
	for i := range first {
		if first[i].Name == ClassGo {
			first[i].Required[0] = "tampered"
		}
	}
	again, err := LookupClass(ClassGo)
	if err != nil {
		t.Fatalf("LookupClass(go): %v", err)
	}
	if again.Required[0] == "tampered" {
		t.Fatal("a caller mutated the declared class table")
	}
}

// TestStageScriptsHaveNoUnquotedInterpolation guards the shell-injection
// shape: no Go string interpolation outside quoteSh may reach the scripts.
// The scripts carry exactly two substitution mechanisms: the tab verdict
// format (printf) and backtick-concatenated checksum constants.
func TestStageScriptsHaveNoUnquotedInterpolation(t *testing.T) {
	for _, tc := range Catalog() {
		if tc.Script == nil {
			continue
		}
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

// TestProbeScriptShape checks the probe program: every catalog name in
// order, replaced exactly once, verdict lines preserved.
func TestProbeScriptShape(t *testing.T) {
	s := ProbeScript()
	if !strings.HasPrefix(s, "for t in "+strings.Join(CatalogNames(), " ")+"; do") {
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

// TestParseProbeOutput is the table over the three states, on the DEFAULT
// class (full).
func TestParseProbeOutput(t *testing.T) {
	// Partial fixture: python3+go present, gcc ABSENT, make present,
	// node+cargo omitted (truncated run). gcc is absent, the omitted two are
	// unknown — neither may read as present, so AllPresent must be FALSE and
	// all three must be named in Missing.
	r := ParseProbeOutput("agt-1", "python3\tpresent\tPython 3.11.15\n"+
		"go\tpresent\tgo version go1.25.4 linux/amd64\n"+
		"gcc\tabsent\t\n"+
		"make\tpresent\tGNU Make 4.3\n", 0)
	if r.AllPresent {
		t.Errorf("AllPresent = true, want false while gcc is absent")
	}
	if r.Class != DefaultClass {
		t.Errorf("Class = %q, want the default %q", r.Class, DefaultClass)
	}
	if got := strings.Join(r.Missing, ","); got != "cargo,gcc,node" {
		t.Errorf("Missing = %q, want cargo,gcc,node (absent AND unknown)", got)
	}
	if r.Tools[2].Status != StatusAbsent {
		t.Fatalf("gcc status = %q, want %q", r.Tools[2].Status, StatusAbsent)
	}
	if r.Tools[4].Status != StatusUnknown || r.Tools[5].Status != StatusUnknown {
		t.Errorf("omitted entries must be unknown, got %q/%q", r.Tools[4].Status, r.Tools[5].Status)
	}
	if r.Tools[1].Version == "" || !strings.Contains(r.Tools[1].Version, "go1.25.4") {
		t.Errorf("version column lost: %q", r.Tools[1].Version)
	}
	for _, f := range r.Tools {
		if !f.Required {
			t.Errorf("default (full) class must require %s", f.Name)
		}
	}

	full := ParseProbeOutput("agt-2",
		"python3\tpresent\np3\ngo\tpresent\tgo version go1.25.4 linux/amd64\ngcc\tpresent\tn\nmake\tpresent\tn\nnode\tpresent\tn\ncargo\tpresent\tn\n", 0)
	if !full.AllPresent || len(full.Missing) != 0 {
		t.Errorf("full probe: AllPresent=%v Missing=%v, want true/[]", full.AllPresent, full.Missing)
	}
	if got := strings.Join(full.Required, ","); got != strings.Join(CatalogNames(), ",") {
		t.Errorf("Required = %q, want the whole catalog", got)
	}

	// Truncated output: an omitted entry is UNKNOWN, and unknown is
	// missing — a truncated probe can never read as clean.
	trunc := ParseProbeOutput("agt-3", "python3\tpresent\t3.11\ngo\tpresent\tgo1\n", 0)
	if trunc.Tools[5].Status != StatusUnknown {
		t.Errorf("cargo status = %q, want %q", trunc.Tools[5].Status, StatusUnknown)
	}
	if trunc.AllPresent {
		t.Errorf("truncated probe must not read AllPresent")
	}
}

// TestParseProbeOutputForClassIsTheGate: the SAME probe output must pass one
// class and fail another — that is what makes a class a real check.
func TestParseProbeOutputForClassIsTheGate(t *testing.T) {
	// A box that can build Go and C-less projects: python3, go, make.
	output := "python3\tpresent\tPython 3.11.15\n" +
		"go\tpresent\tgo version go1.25.4 linux/amd64\n" +
		"gcc\tabsent\t\n" +
		"make\tpresent\tGNU Make 4.3\n" +
		"node\tabsent\t\n" +
		"cargo\tabsent\t\n"

	goClass, err := LookupClass(ClassGo)
	if err != nil {
		t.Fatalf("LookupClass(go): %v", err)
	}
	r := ParseProbeOutputFor("agt-1", output, 0, goClass)
	if !r.AllPresent {
		t.Errorf("class go: AllPresent=false, missing %v — gcc/node/cargo are NOT required by go", r.Missing)
	}
	if got := strings.Join(r.Required, ","); got != "python3,go,make" {
		t.Errorf("class go Required = %q", got)
	}
	for _, f := range r.Tools {
		want := f.Name != "gcc" && f.Name != "node" && f.Name != "cargo"
		if f.Required != want {
			t.Errorf("class go: %s Required=%v, want %v", f.Name, f.Required, want)
		}
		if f.Name == "gcc" && f.Status != StatusAbsent {
			t.Errorf("gcc must still be REPORTED absent on a go-class box, got %q", f.Status)
		}
	}

	full, err := LookupClass(ClassFull)
	if err != nil {
		t.Fatalf("LookupClass(full): %v", err)
	}
	rf := ParseProbeOutputFor("agt-1", output, 0, full)
	if rf.AllPresent {
		t.Error("class full must fail on the same box that passes class go")
	}
	if got := strings.Join(rf.Missing, ","); got != "cargo,gcc,node" {
		t.Errorf("class full Missing = %q, want cargo,gcc,node", got)
	}
}

// TestMinimalClassChecksTheBaseline: minimal is not a vacuous class — it
// requires the one toolchain the stock image promises.
func TestMinimalClassChecksTheBaseline(t *testing.T) {
	minimal, err := LookupClass(ClassMinimal)
	if err != nil {
		t.Fatalf("LookupClass(minimal): %v", err)
	}
	present := ParseProbeOutputFor("agt-1", "python3\tpresent\tPython 3.11.15\n"+
		"go\tpresent\tgo version go1.25.4 linux/amd64\ngcc\tpresent\tg\nmake\tpresent\tm\nnode\tpresent\tn\ncargo\tpresent\tc\n", 0, minimal)
	if !present.AllPresent {
		t.Errorf("minimal class must pass an agent with python3: %v", present.Missing)
	}
	if got := strings.Join(present.Required, ","); got != "python3" {
		t.Errorf("minimal Required = %q, want python3", got)
	}
	absent := ParseProbeOutputFor("agt-2", "python3\tabsent\t\n"+
		"go\tpresent\tn\ngcc\tpresent\tg\nmake\tpresent\tm\nnode\tpresent\tn\ncargo\tpresent\tc\n", 0, absentClass(t))
	if absent.AllPresent || strings.Join(absent.Missing, ",") != "python3" {
		t.Errorf("minimal class on an agent with no python3: AllPresent=%v Missing=%v", absent.AllPresent, absent.Missing)
	}
}

// absentClass is a helper so the minimal-class case above reads as prose.
func absentClass(t *testing.T) Class {
	t.Helper()
	c, err := LookupClass(ClassMinimal)
	if err != nil {
		t.Fatalf("LookupClass(minimal): %v", err)
	}
	return c
}

// TestParseStageOutput covers the verdict-line contract including the
// unparsable-output failure.
func TestParseStageOutput(t *testing.T) {
	if o := ParseStageOutput("go", "go\tINSTALLED\tgo version go1.25.4 linux/amd64\n"); o.Status != "INSTALLED" || o.Detail == "" {
		t.Errorf("INSTALLED parse: %+v", o)
	}
	if o := ParseStageOutput("gcc", "gcc\tINSTALLED\talread-present\n"); o.Status != "INSTALLED" {
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

// TestStageScriptRoundTrip drives StageScript for every DELIVERABLE catalog
// name, and pins the base-image entry's refusal in its own words.
func TestStageScriptRoundTrip(t *testing.T) {
	for _, tc := range Catalog() {
		s, err := StageScript(tc.Name)
		if tc.Script == nil {
			if err == nil || !strings.Contains(err.Error(), "base image") {
				t.Errorf("StageScript(%q) must refuse with the base-image reason, got %q / %v", tc.Name, s, err)
			}
			continue
		}
		if err != nil || s == "" {
			t.Errorf("StageScript(%q): %v", tc.Name, err)
		}
	}
	if _, err := StageScript("rg"); err == nil || !strings.Contains(err.Error(), "not in the declared set") {
		t.Errorf("a non-catalog tool must be refused by name, got %v", err)
	}
}

// TestImageSpecDirectiveForClass pins the bake-at-spawn half per class and
// keeps the full-class constant the command prints.
func TestImageSpecDirectiveForClass(t *testing.T) {
	full, err := ImageSpecDirectiveFor(ClassFull)
	if err != nil {
		t.Fatalf("ImageSpecDirectiveFor(full): %v", err)
	}
	if full != ImageSpecDirective {
		t.Errorf("full class directive = %q, want the canonical constant %q", full, ImageSpecDirective)
	}
	for _, class := range []string{ClassMinimal, ClassGo} {
		got, err := ImageSpecDirectiveFor(class)
		if err != nil {
			t.Fatalf("ImageSpecDirectiveFor(%s): %v", class, err)
		}
		// The stock userland layer already carries python3, golang-go and
		// make: those classes add nothing to the image.
		if got != `{"packages":[]}` {
			t.Errorf("class %s directive = %q, want an empty package list", class, got)
		}
	}
	if _, err := ImageSpecDirectiveFor("bogus"); err == nil {
		t.Error("an unknown class must not produce a directive")
	}
}

// TestCacheAndInstallPathsAreAgentLocal guards the no-root and PATH story:
// everything the stages touch lives under $HOME.
func TestCacheAndInstallPathsAreAgentLocal(t *testing.T) {
	for _, tc := range Catalog() {
		if tc.Script == nil {
			continue
		}
		s := tc.Script()
		if strings.Contains(s, " /usr") || strings.Contains(s, " /etc") || strings.Contains(s, " /opt") {
			t.Errorf("stage %s writes outside $HOME", tc.Name)
		}
		if !strings.Contains(s, `$HOME/`+InstallDir) || !strings.Contains(s, `$HOME/`+CacheRoot) {
			t.Errorf("stage %s does not use the agent-local PATH/cache slots", tc.Name)
		}
	}
}
