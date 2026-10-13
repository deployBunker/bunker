package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/imagespec"
)

// TestCommandNameParsesVersionToken: the drift check compares BUILDS, so it has
// to pull the token out of a whole `toolsd version <token> (built ...)` line.
// Handing back the sentence would make every comparison a mismatch.
func TestCommandNameParsesVersionToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{"toolsd version 98c75fa-dirty (built 2026-09-20T18:44:36Z)", "98c75fa-dirty"},
		{"toolsd version v1.2.3", "v1.2.3"},
		{"toolsd version dev", "dev"},
		// Not the shape we expect: pass it through rather than inventing a token.
		{"something else entirely", "something else entirely"},
		{"", ""},
	}
	for _, c := range cases {
		if got := commandName(c.in); got != c.want {
			t.Errorf("commandName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestIsVendoredDeliverable: only tools we build ourselves are COPIED onto an
// agent. ripgrep and the language servers are in distribution registries; they
// reach the agent through their registry (DF-BUNKER-57 runtime delivery, or the
// image-spec package-add path at spawn), never as an unverified binary copy.
func TestIsVendoredDeliverable(t *testing.T) {
	if !isVendoredDeliverable("toolsd") {
		t.Error("toolsd must be deliverable by copy")
	}
	for _, name := range []string{"rg", "gopls", "git", "jq", ""} {
		if isVendoredDeliverable(name) {
			t.Errorf("%q must NOT be a copied deliverable (it belongs to the package path)", name)
		}
	}
}

// TestResolveDeliverableBinary: an explicit --binary is validated rather than
// trusted, and a missing one fails with the remedy named.
func TestResolveDeliverableBinary(t *testing.T) {
	dir := t.TempDir()

	executable := filepath.Join(dir, "toolsd")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveDeliverableBinary("toolsd", executable); err != nil || got != executable {
		t.Errorf("explicit executable path: got (%q, %v), want (%q, nil)", got, err, executable)
	}

	if _, err := resolveDeliverableBinary("toolsd", filepath.Join(dir, "nope")); err == nil {
		t.Error("a nonexistent --binary must fail, not fall through to PATH")
	}
	if _, err := resolveDeliverableBinary("toolsd", dir); err == nil {
		t.Error("a directory must be refused")
	}

	notExec := filepath.Join(dir, "toolsd-noexec")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveDeliverableBinary("toolsd", notExec); err == nil {
		t.Error("a non-executable --binary must be refused")
	}

	// Absent from PATH and no override: the error must name the remedy.
	_, err := resolveDeliverableBinary("definitely-not-a-real-tool-xyz", "")
	if err == nil {
		t.Fatal("an unresolvable tool must fail")
	}
	if !strings.Contains(err.Error(), "--binary") || !strings.Contains(err.Error(), "make dist") {
		t.Errorf("the failure must name the remedy, got: %v", err)
	}
}

// TestAssertStaticallyLinkedRefusesWhenItCannotConfirm is the load-bearing
// direction of the guard: a file it cannot positively identify as static must be
// REFUSED. A missing path is the cheapest proof that it never defaults to allow.
func TestAssertStaticallyLinkedRefusesWhenItCannotConfirm(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	err := assertStaticallyLinked(missing)
	if err == nil {
		t.Fatal("a path that cannot be examined must be refused, not allowed")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("the refusal must say it is refusing, got: %v", err)
	}
}

// TestAssertStaticallyLinkedAgreesWithLdd: the guard must not be a constant.
// When the running test binary is itself dynamic, the guard must REFUSE it; when
// it is static, it must accept. Either way it has to agree with an independent
// reading of the same file.
func TestAssertStaticallyLinkedAgreesWithLdd(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skip("cannot locate the test binary")
	}
	out, lddErr := exec.Command("ldd", self).CombinedOutput()
	if lddErr != nil && len(out) == 0 {
		t.Skip("ldd unavailable; the guard's ldd branch cannot be exercised here")
	}
	isDynamic := strings.Contains(string(out), "=>")

	gotErr := assertStaticallyLinked(self)
	if isDynamic && gotErr == nil {
		t.Errorf("ldd reports a dynamically linked binary (%s) but the guard accepted it", self)
	}
	if !isDynamic && gotErr != nil {
		t.Errorf("ldd reports a static binary (%s) but the guard refused it: %v", self, gotErr)
	}
}

// TestAgentToolsRemediationSpecBuilds is DF-BUNKER-79 at the surface the
// operator actually touches: the spec `agent-tools --install` prints as the
// remediation for the tools it cannot deliver (DF-BUNKER-57) must be a spec the
// IMAGE BUILDER can build. Before the fix it was not — the go directive rendered
// `RUN go install ...` against a base with no Go toolchain, so following the
// CLI's own advice failed the spawn with `go: not found` (exit 127).
//
// This parses the printed constant through the real parser and asserts the
// render the builder would feed docker: both tools are installed, the go
// directive bootstraps the toolchain BEFORE the install line, and the installed
// binary lands on the agent's exec PATH.
func TestAgentToolsRemediationSpecBuilds(t *testing.T) {
	spec, err := imagespec.Parse([]byte(agentToolsRemediationSpec))
	if err != nil {
		t.Fatalf("the spec the CLI prints does not parse, so it can never be spawned: %v\n%s", err, agentToolsRemediationSpec)
	}
	got := spec.Dockerfile()

	// Both advertised tools are actually installed by the render: rg via apt,
	// gopls via go.
	if !strings.Contains(got, "'ripgrep'") {
		t.Errorf("remediation render does not install ripgrep:\n%s", got)
	}
	installLine := "go install 'golang.org/x/tools/gopls@latest'"
	installAt := strings.Index(got, installLine)
	if installAt < 0 {
		t.Fatalf("remediation render does not install gopls:\n%s", got)
	}

	// The go toolchain bootstrap precedes the install: without this the build
	// dies `go: not found` and the remediation is a lie. bootAt takes the LAST
	// 'golang-go' (DF-BUNKER-52): the stock userland layer installs it too, so
	// the first occurrence would satisfy this check even if the bootstrap layer
	// disappeared.
	bootAt := strings.LastIndex(got, "'golang-go'")
	if bootAt < 0 {
		t.Fatalf("remediation render has no Go toolchain bootstrap:\n%s", got)
	}
	if bootAt > installAt {
		t.Errorf("Go toolchain bootstrap is rendered AFTER the install line (boot at %d, install at %d):\n%s", bootAt, installAt, got)
	}

	// And the installed binary is reachable: GOBIN is the first component of the
	// server's agent exec PATH.
	if !strings.Contains(got, "GOBIN="+imagespec.GoBinDir+" "+installLine) {
		t.Errorf("remediation render installs gopls off the agent's PATH:\n%s", got)
	}
}

// TestAgentToolsRemediationSpecIsSingleLine pins the paste-ability of the
// printed spec: it is emitted inside a one-line CLI hint, so it must contain no
// newline and no whitespace that a shell would split on (an operator copies this
// into a spec file verbatim).
func TestAgentToolsRemediationSpecIsSingleLine(t *testing.T) {
	if strings.ContainsAny(agentToolsRemediationSpec, "\n\r\t ") {
		t.Errorf("the printed remediation spec contains whitespace: %q", agentToolsRemediationSpec)
	}
	if !strings.HasPrefix(agentToolsRemediationSpec, "{") || !strings.HasSuffix(agentToolsRemediationSpec, "}") {
		t.Errorf("the printed remediation spec is not a bare JSON object: %q", agentToolsRemediationSpec)
	}
}

// TestPackageToolInstallersMatchAgentToolPackages is the DF-BUNKER-57 drift
// guard: the tools `agent-tools --install` delivers at runtime must be exactly
// the set the image-spec package-add path delivers at spawn
// (imagespec.AgentToolPackages). A registry tool added to one path without the
// other would leave an agent class where the tool is REQUIRED and installable
// by NO documented path — the exact defect this file's stage builders close.
func TestPackageToolInstallersMatchAgentToolPackages(t *testing.T) {
	want := imagespec.AgentToolPackageNames()
	got := make([]string, 0, len(packageToolInstallers))
	for _, inst := range packageToolInstallers {
		got = append(got, inst.PackageToken)
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("runtime delivery set %v != spawn-time delivered set %v", got, want)
	}

	// Every catalog name that a stage delivers must exist in the probe
	// catalog, and every stage must have a Parse that names its own tool.
	// Both legs fail loudly if the registry and the catalog drift apart.
	for _, inst := range packageToolInstallers {
		f := findingFor(parseAgentToolOutput(inst.CatalogName, inst.CatalogName+"\tabsent\t", 0), inst.CatalogName)
		if f == nil {
			t.Errorf("stage delivers %q, which the agent-tool catalog does not contain", inst.CatalogName)
		}
		if got := inst.Parse(inst.CatalogName + "\tINSTALLED\t1.2.3\n"); got.Status != "INSTALLED" || got.Detail != "1.2.3" {
			t.Errorf("stage Parse misreads its own INSTALLED line: %+v", got)
		}
		if got := inst.Parse(inst.CatalogName + "\tFAILED\tboom\n"); got.Status != "FAILED" || got.Detail != "boom" {
			t.Errorf("stage Parse misreads its own FAILED line: %+v", got)
		}
		if got := inst.Parse("something went wrong\n"); got.Status != "FAILED" ||
			!strings.Contains(got.Detail, "unparsable output") {
			t.Errorf("stage Parse must classify a no-verdict stage as FAILED, got %+v", got)
		}
	}

	// The REQUIRED tool (rg) must have a delivery stage: a REQUIRED tool
	// without one is the DF-BUNKER-57 shape returning through the back door.
	rgStage := false
	for _, inst := range packageToolInstallers {
		if inst.CatalogName == "rg" {
			rgStage = true
		}
	}
	if !rgStage {
		t.Error("rg is REQUIRED by the catalog but has no runtime delivery stage")
	}
}

// TestStageBudgetSecondsNeverExceedsTheContext: a stage is capped by its
// budget AND by what the parent command's context has left — it must not
// outlive the RPC that carries it. The no-deadline case has no context cap, so
// the stage budget itself decides (ctxDeadlineSeconds floors at 60 there).
func TestStageBudgetSecondsNeverExceedsTheContext(t *testing.T) {
	if got := stageBudgetSeconds(context.Background(), 5*time.Minute); got != 60 {
		t.Errorf("stageBudgetSeconds(no deadline, 5m) = %d, want 60 (no deadline: the 60s exec default)", got)
	}
	ctxLong, cancelLong := context.WithTimeout(context.Background(), time.Hour)
	defer cancelLong()
	if got := stageBudgetSeconds(ctxLong, 90*time.Second); got != 90 {
		t.Errorf("stageBudgetSeconds(1h left, 90s cap) = %d, want 90 (cap wins under a long deadline)", got)
	}
	ctxShort, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	if got := stageBudgetSeconds(ctxShort, 5*time.Minute); got < 5 || got > 7 {
		t.Errorf("stageBudgetSeconds(7s left, 5m) = %d, want within the 7s context window", got)
	}
}

// TestFindingForAndStatusOf: the post-delivery verdict reads the probe result,
// and a tool absent from the report must not read as present.
func TestFindingForAndStatusOf(t *testing.T) {
	r := parseAgentToolOutput("a", "toolsd\tpresent\ttoolsd version v9\nrg\tabsent\t", 0)

	if f := findingFor(r, "toolsd"); f == nil || f.Status != "present" {
		t.Errorf("toolsd should be present, got %v", f)
	}
	if f := findingFor(r, "rg"); f == nil || f.Status != "absent" {
		t.Errorf("rg should be absent, got %v", f)
	}
	// A tool the catalog does not contain is simply not found...
	if f := findingFor(r, "not-a-tool"); f != nil {
		t.Errorf("findingFor returned %v for an unknown tool", f)
	}
	// ...and an unfound tool must never be reported as present.
	if got := statusOf(nil); got != "not reported" {
		t.Errorf("statusOf(nil) = %q, want \"not reported\"", got)
	}
}

// TestProvisionedAgentReportsDeliveredToolsPresent is the TOOLS-B2 acceptance
// reading at the probe contract: a provisioned agent (spawned with the
// remediation spec, plus a toolsd delivery) reports rg and gopls PRESENT with
// versions, they appear in NEITHER missing list, and the remaining-missing
// computation (what --install offers to fix) is empty. The exact report shape
// is what `bunker agent-tools` parses — tab-separated name/status/version —
// fed through the real parser, not a hand-built report struct.
func TestProvisionedAgentReportsDeliveredToolsPresent(t *testing.T) {
	probeOut := "toolsd\tpresent\ttoolsd version 98c75fa-dirty (built 2026-10-09T00:00:00Z)\n" +
		"rg\tpresent\tripgrep 14.1.0\n" +
		"git\tpresent\tgit version 2.43.0\n" +
		"jq\tpresent\tjq-1.7\n" +
		"gopls\tpresent\tgolang.org/x/tools/gopls v0.22.0\n"

	r := parseAgentToolOutput("toolsb2", probeOut, 0)
	if len(r.Missing) != 0 || len(r.MissingRequired) != 0 {
		t.Fatalf("provisioned agent reports missing %v / required-missing %v, want none",
			r.Missing, r.MissingRequired)
	}
	for _, want := range []struct{ name, versionWant string }{
		{"rg", "ripgrep 14.1.0"},
		{"gopls", "golang.org/x/tools/gopls"},
	} {
		f := findingFor(r, want.name)
		if f == nil {
			t.Fatalf("%s absent from the report entirely", want.name)
		}
		if f.Status != "present" {
			t.Errorf("%s status = %q, want present", want.name, f.Status)
		}
		if !strings.Contains(f.Version, want.versionWant) {
			t.Errorf("%s version = %q, want it to contain %q", want.name, f.Version, want.versionWant)
		}
	}

	// The absent-shaped control: drop exactly the two delivered tools and the
	// parser must name them both as missing (and only them) — the test cannot
	// pass vacuously by feeding a report where everything is present.
	degraded := parseAgentToolOutput("toolsb2",
		strings.ReplaceAll(strings.ReplaceAll(probeOut,
			"rg\tpresent\tripgrep 14.1.0\n", "rg\tabsent\t\n"),
			"gopls\tpresent\tgolang.org/x/tools/gopls v0.22.0\n", "gopls\tabsent\t\n"), 0)
	if got := strings.Join(degraded.Missing, ","); got != "gopls,rg" {
		t.Errorf("degraded agent missing = %q, want \"gopls,rg\"", got)
	}
	// rg is REQUIRED (search breaks without it); gopls is not, so only rg may
	// land in the required-missing list.
	if got := strings.Join(degraded.MissingRequired, ","); got != "rg" {
		t.Errorf("degraded agent missing_required = %q, want \"rg\"", got)
	}
}

// TestRemainingAfterDeliveryExcludesPackagePathTools pins what --install says
// about its own limits: rg and gopls have package-path delivery stages
// (DF-BUNKER-57), so they must never be offered the COPY remedy nor named as
// "no delivery stage" even when a report carries them as missing — a copy of a
// registry tool would bypass version pinning and signatures, which the
// vendored-only classifier owns.
func TestRemainingAfterDeliveryExcludesPackagePathTools(t *testing.T) {
	r := parseAgentToolOutput("a", "rg	absent	\ngopls	absent	\n", 0)
	remaining := []string{}
	for _, name := range r.Missing {
		if !isPackagePathTool(name) && !isVendoredDeliverable(name) {
			remaining = append(remaining, name)
		}
	}
	if got := strings.Join(remaining, ","); got != "" {
		t.Errorf("remaining = %q, want \"\" (both have delivery stages; neither is a copy candidate)", got)
	}
}

// TestPackageDeliveryStageScriptsAreExecutedAsShell runs both stage builders'
// scripts under a real /bin/sh against a fake $HOME, in the states a bare
// agent can produce WITHOUT network or root:
//   - already-present: the stage must short-circuit INSTALLED (idempotent
//     re-run), never touching the network;
//   - missing with no fetcher/toolchain: the stage must print a FAILED line
//     carrying its own words, never a bare non-zero exit.
//
// This is the same discipline as TestAgentToolsRemoveScriptByExecution: a
// script builder that was never run proves nothing about the shell.
func TestPackageDeliveryStageScriptsAreExecutedAsShell(t *testing.T) {
	for _, inst := range packageToolInstallers {
		t.Run(inst.CatalogName, func(t *testing.T) {
			// Already-present: fake the tool on PATH inside the fake HOME.
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, agentPackageInstallDir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, agentPackageInstallDir, inst.CatalogName),
				[]byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			out := runScriptWithHome(t, inst.Script(), home)
			if !strings.HasPrefix(out, inst.CatalogName+"	INSTALLED	already-present") {
				t.Errorf("already-present run = %q, want the INSTALLED/already-present line", out)
			}

			// Missing + unfetchable: PATH stripped of everything but
			// stub sh/uname/mktemp (mkdir etc. unreachable), so the
			// stage must fail at its first primitive with a FAILED
			// line carrying its own words — never a bare non-zero
			// exit and never a success claim.
			home2 := t.TempDir()
			stub := filepath.Join(home2, ".stub-bin")
			if err := os.MkdirAll(stub, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, b := range []string{"sh", "uname", "mktemp"} {
				if err := os.Symlink("/bin/sh", filepath.Join(stub, b)); err != nil {
					t.Fatal(err)
				}
			}
			out = runScriptWithEnv(t, inst.Script(), home2, []string{"PATH=" + stub})
			if !strings.HasPrefix(out, inst.CatalogName+"	FAILED	") {
				t.Errorf("unfetchable run = %q, want a FAILED line", out)
			}
		})
	}
}

// runScriptWithEnv executes a script under sh with $HOME and PATH overridden,
// simulating both the agent exec environment and a bare, tool-less one.
func runScriptWithEnv(t *testing.T, script, home string, extraEnv []string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	env := []string{"HOME=" + home}
	env = append(env, extraEnv...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh -c failed: %v\noutput: %s", err, out)
	}
	return string(out)
}

// TestFindingForAndStatusOf: the post-delivery verdict reads the probe result,
