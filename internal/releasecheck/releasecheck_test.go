package releasecheck

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	repoRoot    = "../.."
	workflowRel = repoRoot + "/.github/workflows/release.yml"
	// legacyRel is the pre-INT-CI-021 workflow, captured byte-for-byte from
	// HEAD before the repair. It is the RED half of the proof: the repaired
	// rules must reject the shape that failed run 35425592977.
	legacyRel = "testdata/release-pre-int-ci-021.yml"
	// probeAnchor identifies the entrypoint-resolution step inside the live
	// workflow (the dry-run probe of the Makefile target).
	probeAnchor = "make -n release-binaries"
)

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func mustParse(t *testing.T, path string) *Workflow {
	t.Helper()
	w, err := Load(path)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return w
}

// rulesOf collapses findings to their rule names (order preserved, duplicates
// kept so a rule firing twice is visible).
func rulesOf(problems []Problem) []string {
	var names []string
	for _, p := range problems {
		names = append(names, p.Rule)
	}
	return names
}

func ruleSet(problems []Problem) map[string]bool {
	set := map[string]bool{}
	for _, p := range problems {
		set[p.Rule] = true
	}
	return set
}

func report(t *testing.T, problems []Problem) string {
	t.Helper()
	if len(problems) == 0 {
		return "<no problems>"
	}
	var b strings.Builder
	for _, p := range problems {
		b.WriteString("\n  - " + p.String())
	}
	return b.String()
}

// ── the live workflow must satisfy the contract ─────────────────────────────

func TestRepoReleaseWorkflowSatisfiesContract(t *testing.T) {
	w := mustParse(t, workflowRel)
	if problems := w.Verify(); len(problems) != 0 {
		t.Errorf("%s violates the release contract:%s", workflowRel, report(t, problems))
	}
}

// ── RED: the pre-fix shape must fail the same rules ─────────────────────────

// TestLegacyReleaseWorkflowFailsContract is the negative control. The fixture is
// the workflow as it stood when run 35425592977 failed: it invoked
// `make release-binaries` unconditionally in a checkout of a tag that predates
// the target, and its only provenance check was the version STRING — which a
// build of any other ref stamped with the tag's version satisfies (the assets
// attached to v0.1.4 embed vcs.revision=235e715, a main commit).
func TestLegacyReleaseWorkflowFailsContract(t *testing.T) {
	legacy := mustParse(t, legacyRel)
	if live := readRepoFile(t, workflowRel); live == readRepoFile(t, legacyRel) {
		t.Fatalf("%s equals %s — the workflow was not repaired, so this control proves nothing", workflowRel, legacyRel)
	}

	problems := legacy.Verify()
	got := ruleSet(problems)
	// Visible with -v: the findings the repaired contract reports for the shape
	// that failed run 35425592977.
	t.Logf("pre-fix workflow violations:%s", report(t, problems))
	for _, want := range []string{RuleBuildEntrypoint, RuleTagProvenance} {
		if !got[want] {
			t.Errorf("the pre-fix workflow does not trip rule %q, so that rule cannot catch the failure it exists for:%s",
				want, report(t, problems))
		}
	}
	// The pre-fix workflow kept the asset contract intact and checked out the
	// tag — the repair has to be additive, not a rewrite of those properties.
	for _, unwanted := range []string{RuleAssetContract, RuleSourceRef} {
		if got[unwanted] {
			t.Errorf("rule %q trips on the pre-fix workflow, so it does not isolate the INT-CI-021 defect:%s",
				unwanted, report(t, problems))
		}
	}

	// The pre-fix shape really is "invoke the target unconditionally".
	build, ok := legacy.FindStep(func(s Step) bool { return makeInvokeRE.MatchString(s.Run) })
	if !ok {
		t.Fatalf("%s no longer contains the ungated `make release-binaries` invocation the control asserts against", legacyRel)
	}
	if build.If != "" {
		t.Errorf("the pre-fix build step is gated (%q); the fixture must stay the ungated shape", build.If)
	}
	if _, ok := legacy.FindStep(func(s Step) bool { return makeProbeRE.MatchString(s.Run) }); ok {
		t.Errorf("%s probes the entrypoint already — it is not the pre-fix shape", legacyRel)
	}
}

// ── one-change-per-test rule coverage ───────────────────────────────────────

// baseWorkflow satisfies all four rules, so each case below changes exactly one
// thing and asserts exactly the rule that change must trip.
const baseWorkflow = `name: Release
env:
  RELEASE_ASSETS: >-
    dist/bunker-linux-amd64
    dist/bunkerd-linux-amd64
    dist/bunker-linux-arm64
    dist/bunkerd-linux-arm64
    dist/SHA256SUMS
    dist/install.sh
jobs:
  release:
    steps:
      - name: Resolve the release tag
        id: tag
        run: |
          set -euo pipefail
          echo "tag=v0.1.4" >> "$GITHUB_OUTPUT"
      - uses: actions/checkout@v4
        with:
          ref: ${{ steps.tag.outputs.tag }}
          fetch-depth: 0
      - name: Resolve the release build entrypoint from the tag tree
        id: build
        run: |
          set -euo pipefail
          if ! make -n release-binaries >/dev/null 2>&1; then
            echo "::error::v0.1.4 cannot build the assets from its own tree" >&2
            exit 1
          fi
          echo "build_path=make" >> "$GITHUB_OUTPUT"
      - name: Cross-compile the release binaries
        if: steps.build.outputs.build_path == 'make'
        run: make release-binaries VERSION=0.1.4
      - name: Verify the cross-compiled assets
        run: |
          set -euo pipefail
          for platform in amd64 arm64; do
            go version -m "dist/bunker-linux-${platform}" | grep -q "GOARCH=${platform}"
            go version -m "dist/bunker-linux-${platform}" | grep -q 'GOOS=linux'
          done
          dist/bunker-linux-amd64 version | grep -q "^bunker 0.1.4$"
          cmp scripts/install.sh dist/install.sh
          ( cd dist && sha256sum --check SHA256SUMS )
          tag_commit="$(git rev-parse --verify "v0.1.4^{commit}")"
          head_commit="$(git rev-parse HEAD)"
          test "${tag_commit}" = "${head_commit}" || exit 1
          revision="$(go version -m dist/bunker-linux-amd64 | awk '$1 == "build" && index($2, "vcs.revision=") == 1 { sub("vcs.revision=", "", $2); print $2 }')"
          test -n "${revision}"
      - name: Publish the GitHub Release
        run: |
          set -euo pipefail
          gh release upload "v0.1.4" dist/bunker-linux-amd64 --clobber
          for want in bunker-linux-amd64 bunkerd-linux-amd64 bunker-linux-arm64 bunkerd-linux-arm64 SHA256SUMS install.sh; do
            echo "${want}"
          done
`

func mutate(t *testing.T, base, old, new string) string {
	t.Helper()
	if !strings.Contains(base, old) {
		t.Fatalf("the fixture no longer contains %q — this case is stale", old)
	}
	return strings.Replace(base, old, new, 1)
}

func TestReleaseWorkflowRules(t *testing.T) {
	if problems := mustParseString(t, baseWorkflow).Verify(); len(problems) != 0 {
		t.Fatalf("the base fixture trips rules before any mutation, so every case below is vacuous:%s", report(t, problems))
	}

	const (
		probeIfAnchor   = "        id: build\n        run: |\n"
		buildIfAnchor   = "        if: steps.build.outputs.build_path == 'make'\n"
		provenanceLine  = "          revision=\"$(go version -m dist/bunker-linux-amd64 | awk '$1 == \"build\" && index($2, \"vcs.revision=\") == 1 { sub(\"vcs.revision=\", \"\", $2); print $2 }')\"\n"
		revParseLine    = "          tag_commit=\"$(git rev-parse --verify \"v0.1.4^{commit}\")\"\n"
		revParseHead    = "          head_commit=\"$(git rev-parse HEAD)\"\n"
		installFromList = "    dist/install.sh\n"
		clobberFlag     = " --clobber"
	)

	cases := []struct {
		name      string
		workflow  string
		wantRules []string
	}{
		{
			name: "target invoked without any probe",
			workflow: mutate(t, baseWorkflow,
				"make -n release-binaries", "make -n some-other-target"),
			wantRules: []string{RuleBuildEntrypoint},
		},
		{
			name: "probe is skippable",
			workflow: mutate(t, baseWorkflow,
				probeIfAnchor, "        id: build\n        if: github.event_name == 'push'\n        run: |\n"),
			wantRules: []string{RuleBuildEntrypoint},
		},
		{
			name: "probe cannot fail the job",
			workflow: mutate(t, baseWorkflow,
				"            exit 1\n", ""),
			wantRules: []string{RuleBuildEntrypoint},
		},
		{
			name:      "invocation is not gated on the probe",
			workflow:  mutate(t, baseWorkflow, buildIfAnchor, ""),
			wantRules: []string{RuleBuildEntrypoint},
		},
		{
			name:      "no provenance of the built tree",
			workflow:  mutate(t, baseWorkflow, provenanceLine, ""),
			wantRules: []string{RuleTagProvenance},
		},
		{
			name:      "checkout is never compared to the tag commit",
			workflow:  mutate(t, baseWorkflow, revParseLine+revParseHead, ""),
			wantRules: []string{RuleTagProvenance},
		},
		{
			name:      "installer dropped from the published assets",
			workflow:  mutate(t, baseWorkflow, installFromList, ""),
			wantRules: []string{RuleAssetContract},
		},
		{
			name: "arm64 no longer verified",
			workflow: mutate(t, baseWorkflow,
				"for platform in amd64 arm64", "for platform in amd64"),
			wantRules: []string{RuleAssetContract},
		},
		{
			name:      "publish is no longer idempotent",
			workflow:  mutate(t, baseWorkflow, clobberFlag, ""),
			wantRules: []string{RuleAssetContract},
		},
		{
			name: "build tree is a branch instead of the tag",
			workflow: mutate(t, baseWorkflow,
				"ref: ${{ steps.tag.outputs.tag }}", "ref: main"),
			wantRules: []string{RuleSourceRef},
		},
		{
			name: "job switches ref before building",
			workflow: mutate(t, baseWorkflow,
				"      - name: Publish the GitHub Release\n",
				"      - name: Build from main instead\n        run: git checkout main\n      - name: Publish the GitHub Release\n"),
			wantRules: []string{RuleSourceRef},
		},
		{
			// A guard a comment can pass guards nothing: the probe must be
			// executable shell, not a mention of what the probe would do.
			name: "probe is only mentioned in a comment",
			workflow: mutate(t, baseWorkflow,
				"          if ! make -n release-binaries >/dev/null 2>&1; then\n",
				"          # we could ask `make -n release-binaries` first\n"),
			wantRules: []string{RuleBuildEntrypoint},
		},
		{
			name: "provenance is only described in a comment",
			workflow: mutate(t, baseWorkflow,
				"          revision=\"$(go version -m dist/bunker-linux-amd64 | awk '$1 == \"build\" && index($2, \"vcs.revision=\") == 1 { sub(\"vcs.revision=\", \"\", $2); print $2 }')\"\n",
				"          # the toolchain records the tree as vcs.revision\n"),
			wantRules: []string{RuleTagProvenance},
		},
		{
			name: "installer comparison is only described in a comment",
			workflow: mutate(t, baseWorkflow,
				"          cmp scripts/install.sh dist/install.sh\n",
				"          # cmp scripts/install.sh dist/install.sh\n"),
			wantRules: []string{RuleAssetContract},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := mustParseString(t, tc.workflow).Verify()
			got := ruleSet(problems)
			for _, want := range tc.wantRules {
				if !got[want] {
					t.Errorf("rule %q did not trip:%s", want, report(t, problems))
				}
			}
			if len(got) != len(tc.wantRules) {
				t.Errorf("expected exactly %v, got %v:%s", tc.wantRules, rulesOf(problems), report(t, problems))
			}
		})
	}
}

func mustParseString(t *testing.T, yml string) *Workflow {
	t.Helper()
	w, err := Parse([]byte(yml))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return w
}

// ── the workflow's own entrypoint resolver, executed against real trees ─────

// TestEntrypointResolverRefusesTreesWithoutTheTarget runs the resolver step —
// the shell block lifted verbatim out of release.yml — against three trees:
//
//	the repo (defines the target)          → resolves to `make`
//	a tree without the target or installer → refuses, actionable message, no build
//	a real historical release tag tree     → refuses, and the pre-fix shape
//	                                         (`make release-binaries VERSION=x`)
//	                                         still dies there with the exact
//	                                         make error run 35425592977 hit
//
// This is the dynamic half of the contract: the probe is not asserted in prose,
// it is executed.
func TestEntrypointResolverRefusesTreesWithoutTheTarget(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make not available: %v", err)
	}
	script := entrypointProbeScript(t)

	t.Run("repo tree resolves to make", func(t *testing.T) {
		code, out, outputs := runStepScript(t, script, repoRoot, map[string]string{"RELEASE_TAG": "v0.1.4"})
		if code != 0 {
			t.Fatalf("the resolver rejected the repo tree, which defines the target (exit %d):\n%s", code, out)
		}
		if !strings.Contains(outputs, BuildPathOutput+"=make") {
			t.Errorf("the resolver did not publish %s=make for a tree that defines the target; GITHUB_OUTPUT held:\n%s",
				BuildPathOutput, outputs)
		}
	})

	t.Run("tree without the target or installer refuses", func(t *testing.T) {
		tree := t.TempDir()
		// A Makefile without the release target (what a tag cut before
		// DF-BUNKER-25 carries) and no scripts/install.sh at all.
		makefile := ".PHONY: all\nall:\n\t@echo nothing to do\n"
		if err := os.WriteFile(filepath.Join(tree, "Makefile"), []byte(makefile), 0o644); err != nil {
			t.Fatalf("write fixture Makefile: %v", err)
		}
		code, out, outputs := runStepScript(t, script, tree, map[string]string{"RELEASE_TAG": "v0.0.0-notag"})
		if code == 0 {
			t.Fatalf("the resolver accepted a tree with neither the release target nor the installer:\n%s", out)
		}
		for _, want := range []string{"::error::", "v0.0.0-notag", "release-binaries", "scripts/install.sh", "Refusing to build"} {
			if !strings.Contains(out, want) {
				t.Errorf("the refusal message does not mention %q — the failure must be diagnosable:\n%s", want, out)
			}
		}
		if strings.Contains(outputs, BuildPathOutput+"=make") {
			t.Errorf("the resolver published %s=make for an unbuildable tree; GITHUB_OUTPUT held:\n%s", BuildPathOutput, outputs)
		}
		if _, err := os.Stat(filepath.Join(tree, "dist")); !os.IsNotExist(err) {
			t.Errorf("the refusal touched %s/dist (stat err=%v) — a refused tag must build nothing", tree, err)
		}
	})

	t.Run("historical release tag tree", func(t *testing.T) {
		tag := newestTagWithoutReleaseTarget(t)
		tree := t.TempDir()
		if err := extractTag(t, tag, tree); err != nil {
			t.Skipf("cannot materialise %s in this checkout (%v) — a shallow/partial clone; the synthetic arm above still covers the refusal path", tag, err)
		}

		// Premise: this tag really is one the pre-fix workflow could not build.
		if _, err := os.Stat(filepath.Join(tree, "Makefile")); err != nil {
			t.Fatalf("premise broken: %s has no Makefile at all (%v)", tag, err)
		}
		if _, err := os.Stat(filepath.Join(tree, "scripts", "install.sh")); err == nil {
			t.Fatalf("premise broken: %s carries scripts/install.sh, so it is not a historical tag", tag)
		}
		if targetDefined(t, tree) {
			t.Fatalf("premise broken: %s's Makefile defines release-binaries", tag)
		}

		code, out, outputs := runStepScript(t, script, tree, map[string]string{"RELEASE_TAG": tag})
		if code == 0 {
			t.Fatalf("the repaired resolver accepted %s, whose tree predates the release target — assets would be built from something else:\n%s", tag, out)
		}
		for _, want := range []string{"::error::", tag, "release-binaries", "scripts/install.sh", "Refusing to build", "Nothing was built, uploaded or clobbered"} {
			if !strings.Contains(out, want) {
				t.Errorf("the refusal for %s does not mention %q:\n%s", tag, want, out)
			}
		}
		if strings.Contains(outputs, BuildPathOutput+"=make") {
			t.Errorf("the resolver published %s=make for %s; GITHUB_OUTPUT held:\n%s", BuildPathOutput, tag, outputs)
		}
		if _, err := os.Stat(filepath.Join(tree, "dist")); !os.IsNotExist(err) {
			t.Errorf("the refusal built into %s/dist (stat err=%v) — nothing may be produced for an unbuildable tag", tree, err)
		}

		// The pre-fix shape, replayed: this is the exact step that died in run
		// 35425592977, and it must still die — which is what makes the refusal
		// above the repair rather than a cosmetic rename.
		legacy := mustParse(t, legacyRel)
		build, ok := legacy.FindStep(func(s Step) bool { return makeInvokeRE.MatchString(s.Run) })
		if !ok {
			t.Fatalf("%s has no `make release-binaries` step to replay", legacyRel)
		}
		legacyRun := strings.ReplaceAll(build.Run, "${{ steps.tag.outputs.version }}", strings.TrimPrefix(tag, "v"))
		code, out, _ = runStepScript(t, "set -euo pipefail\n"+legacyRun+"\n", tree, nil)
		if code == 0 {
			t.Fatalf("the pre-fix invocation %q succeeded in %s's tree — this test no longer models the CI failure", legacyRun, tag)
		}
		if !strings.Contains(out, "No rule to make target") {
			t.Errorf("the pre-fix invocation failed for an unexpected reason in %s's tree:\n%s", tag, out)
		}
	})
}

// entrypointProbeScript lifts the resolver step's shell out of the live
// workflow, so the test executes the workflow's real logic (not a copy that can
// drift from it).
func entrypointProbeScript(t *testing.T) string {
	t.Helper()
	w := mustParse(t, workflowRel)
	step, ok := w.FindStep(func(s Step) bool { return strings.Contains(s.Run, probeAnchor) })
	if !ok {
		t.Fatalf("%s has no step probing with %q — the entrypoint is resolved nowhere", workflowRel, probeAnchor)
	}
	if step.If != "" {
		t.Errorf("the resolver step %q is conditional (%q); it must always run", step.Name, step.If)
	}
	if !strings.Contains(step.Run, BuildPathOutput) {
		t.Fatalf("the resolver step %q never publishes %s, so the build cannot be gated on it", step.Name, BuildPathOutput)
	}
	return step.Run
}

// runStepScript executes one workflow step's shell with the environment GitHub
// gives it and returns (exit code, combined output, GITHUB_OUTPUT contents).
func runStepScript(t *testing.T, script, dir string, extraEnv map[string]string) (int, string, string) {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "step.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write step script: %v", err)
	}
	outputPath := filepath.Join(t.TempDir(), "github_output")
	if err := os.WriteFile(outputPath, nil, 0o600); err != nil {
		t.Fatalf("create GITHUB_OUTPUT: %v", err)
	}
	summaryPath := filepath.Join(t.TempDir(), "summary.md")

	cmd := exec.Command("bash", scriptPath)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GITHUB_OUTPUT="+outputPath,
		"GITHUB_STEP_SUMMARY="+summaryPath,
	)
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run step script: %v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	outputs, readErr := os.ReadFile(outputPath)
	if readErr != nil {
		t.Fatalf("read GITHUB_OUTPUT: %v", readErr)
	}
	return code, string(out), string(outputs)
}

var releaseTargetLine = regexp.MustCompile(`(?m)^release-binaries:`)

// targetDefined reports whether dir's Makefile defines the release target.
func targetDefined(t *testing.T, dir string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "Makefile"))
	if err != nil {
		return false
	}
	return releaseTargetLine.Match(data)
}

// newestTagWithoutReleaseTarget returns the newest v* tag whose tree has no
// release-binaries target — the shape run 35425592977 tried to build (v0.1.4).
// No test hardcodes a tag, so the suite keeps working after the next release.
func newestTagWithoutReleaseTarget(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	listed, err := exec.Command("git", "-C", repoRoot, "tag", "--list", "--sort=-v:refname", "v*").Output()
	if err != nil {
		t.Skipf("cannot list tags (%v) — a shallow checkout skips this arm; the synthetic arm above still covers the refusal path", err)
	}
	for _, tag := range strings.Fields(string(listed)) {
		makefile, err := exec.Command("git", "-C", repoRoot, "show", tag+":Makefile").Output()
		if err != nil || !releaseTargetLine.Match(makefile) {
			return tag
		}
	}
	t.Skipf("every release tag in this checkout defines the release target (tag-less or shallow clone) — " +
		"the historical-tag arm is skipped; the synthetic arm above still covers the refusal path")
	return ""
}

// extractTag materialises a tag's tree into dir. A shallow or partial checkout
// cannot serve the tag's objects; the caller skips that arm (the synthetic arm
// still covers the refusal path) rather than failing on an unavailable fixture.
func extractTag(t *testing.T, tag, dir string) error {
	t.Helper()
	archive, err := exec.Command("git", "-C", repoRoot, "archive", tag).Output()
	if err != nil {
		return err
	}
	untar := exec.Command("tar", "-x", "-C", dir)
	untar.Stdin = bytes.NewReader(archive)
	if out, err := untar.CombinedOutput(); err != nil {
		return fmt.Errorf("extract %s: %w\n%s", tag, err, out)
	}
	return nil
}
