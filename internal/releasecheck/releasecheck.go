// Package releasecheck pins the contract of .github/workflows/release.yml — the
// workflow that publishes the six release assets scripts/install.sh downloads
// (DF-BUNKER-25) — so a cheap workflow edit cannot silently re-open the failure
// modes that shipped a broken release path.
//
// INT-CI-021: run 35425592977 dispatched the release workflow from main for the
// historical tag v0.1.4 and died in 26s with
//
//	make: *** No rule to make target 'release-binaries'. Stop.
//
// A run's workflow DEFINITION comes from the ref that dispatched it, but the
// checked out TREE is the selected tag, and v0.1.4's tree predates the release
// target (added by 777a0cd / DF-BUNKER-25). Worse, the version-stamp check the
// workflow did have is satisfied by ANY tree built with VERSION=<tag>: the six
// assets currently attached to v0.1.4 embed vcs.revision=235e715 (a main commit,
// built from a dirty tree), which the workflow could not detect.
//
// The four rules below evaluate exactly the properties that make the difference:
//
//  1. Build entrypoint — the tree is probed for `make release-binaries`
//     (`make -n`, no recipe executed) by an unconditional step that fails closed
//     with an actionable message, and the step that INVOKES the target is gated
//     on that probe's output. A tag whose tree lacks the target therefore
//     refuses with the tag, the missing ingredients and the remedy instead of
//     dying inside make.
//  2. Tag provenance — after building, the workflow proves the checkout IS the
//     tag commit and that every binary embeds it as the toolchain's
//     `vcs.revision`, so assets can never be published under a tag from another
//     ref's tree.
//  3. Asset contract — the six published names, both architectures, the linux
//     target, the tag's version stamp, the byte-identical installer, the
//     SHA256SUMS verification and the post-publish six-asset assertion all stay
//     in place.
//  4. Source ref — the build tree is the selected tag (never a branch) and no
//     run command switches, archives, fetches or clones another ref.
//
// The package is pure: rules operate on parsed workflow content, no network, no
// runner, no git. The companion test executes this workflow's own entrypoint
// resolver against real historical tag trees (see releasecheck_test.go).
package releasecheck

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Rule names reported on Problem.Rule.
const (
	// RuleBuildEntrypoint marks a release workflow whose build entrypoint is
	// not resolved from the checked-out tree before it is invoked.
	RuleBuildEntrypoint = "release-build-entrypoint"
	// RuleTagProvenance marks a release workflow that never proves the
	// published assets were built from the selected tag's tree.
	RuleTagProvenance = "release-tag-provenance"
	// RuleAssetContract marks a release workflow that no longer publishes or
	// verifies the six-asset contract install.sh depends on.
	RuleAssetContract = "release-asset-contract"
	// RuleSourceRef marks a release workflow that may build from another ref.
	RuleSourceRef = "release-source-ref"
)

// BuildPathOutput is the step output the entrypoint probe writes and the build
// step is gated on ("make" when the checked-out tree defines the target).
const BuildPathOutput = "build_path"

// shellCode returns the executable lines of a step's shell block: every line
// whose first non-space character is '#' is dropped, so no rule can be
// satisfied by an explanatory comment (the workflow documents itself heavily,
// and a guard that a comment can pass guards nothing). The `#` inside a
// parameter expansion (`${tag#v}`) never starts a line, so it is unaffected.
func shellCode(run string) string {
	kept := make([]string, 0, 16)
	for _, line := range strings.Split(run, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// ReleaseAssets are the six assets a release must carry and publish, in the
// order the workflow's RELEASE_ASSETS lists them (install.sh's download names).
var ReleaseAssets = []string{
	"bunker-linux-amd64",
	"bunkerd-linux-amd64",
	"bunker-linux-arm64",
	"bunkerd-linux-arm64",
	"SHA256SUMS",
	"install.sh",
}

// The commands that switch what the tree IS. The release build must only ever
// consume the ref actions/checkout selected; anything else can publish another
// tree's assets under this tag.
var refSwitchRE = regexp.MustCompile(`\bgit\s+(checkout|switch|archive|worktree|fetch|clone|reset)\b`)

// A dry-run probe of the Makefile target: make answering "does this tree define
// it?" without executing a recipe.
var makeProbeRE = regexp.MustCompile(`\bmake\s+-n\s+release-binaries\b`)

// The target invocation itself (the dry run above does not match: `-n` sits
// between `make` and the target name).
var makeInvokeRE = regexp.MustCompile(`\bmake\s+release-binaries\b`)

// Step is one workflow step, flattened across jobs.
type Step struct {
	Name string
	ID   string
	If   string
	Run  string
	Uses string
	With map[string]string
	Env  map[string]string
}

// Workflow is the part of a release workflow the contract cares about.
type Workflow struct {
	// Env is the workflow-level env block (RELEASE_ASSETS lives there).
	Env map[string]string
	// Steps are every job's steps, in job-name then declaration order.
	Steps []Step
}

// Problem is one contract violation.
type Problem struct {
	Rule   string
	Detail string
}

func (p Problem) String() string { return p.Rule + ": " + p.Detail }

// Load reads and parses a workflow file.
func Load(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse parses workflow YAML into the contract view.
func Parse(data []byte) (*Workflow, error) {
	var raw struct {
		Env  map[string]any `yaml:"env"`
		Jobs map[string]struct {
			Steps []struct {
				Name string         `yaml:"name"`
				ID   string         `yaml:"id"`
				If   string         `yaml:"if"`
				Run  string         `yaml:"run"`
				Uses string         `yaml:"uses"`
				With map[string]any `yaml:"with"`
				Env  map[string]any `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	w := &Workflow{Env: stringify(raw.Env)}
	// Deterministic step order: job names sorted, steps in declaration order.
	names := make([]string, 0, len(raw.Jobs))
	for name := range raw.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, job := range names {
		for _, s := range raw.Jobs[job].Steps {
			w.Steps = append(w.Steps, Step{
				Name: s.Name,
				ID:   s.ID,
				If:   s.If,
				Run:  s.Run,
				Uses: s.Uses,
				With: stringify(s.With),
				Env:  stringify(s.Env),
			})
		}
	}
	return w, nil
}

// FindStep returns the first step matching pred, in step order.
func (w *Workflow) FindStep(pred func(Step) bool) (Step, bool) {
	for _, s := range w.Steps {
		if pred(s) {
			return s, true
		}
	}
	return Step{}, false
}

// VerifyApplyer collects findings while the rules walk the workflow.
type verifyCollector struct {
	problems []Problem
}

func (c *verifyCollector) add(rule, format string, args ...any) {
	c.problems = append(c.problems, Problem{Rule: rule, Detail: fmt.Sprintf(format, args...)})
}

// Verify evaluates every rule and returns the violations in rule order. An
// empty result means the workflow satisfies the release contract.
func (w *Workflow) Verify() []Problem {
	c := &verifyCollector{}
	w.checkBuildEntrypoint(c)
	w.checkTagProvenance(c)
	w.checkAssetContract(c)
	w.checkSourceRef(c)
	return c.problems
}

// checkBuildEntrypoint: rule 1.
func (w *Workflow) checkBuildEntrypoint(c *verifyCollector) {
	var probes, invocations []Step
	for _, s := range w.Steps {
		switch code := shellCode(s.Run); {
		case makeProbeRE.MatchString(code):
			probes = append(probes, s)
		case makeInvokeRE.MatchString(code):
			invocations = append(invocations, s)
		}
	}
	if len(probes) == 0 {
		c.add(RuleBuildEntrypoint,
			"no step probes the checked-out tree for the 'release-binaries' target before it is invoked — "+
				"a tag whose tree predates the target (v0.1.4 vs 777a0cd) is otherwise invoked anyway and the run dies inside "+
				"make with `No rule to make target 'release-binaries'` (run 35425592977). Probe first with "+
				"`make -n release-binaries` (a dry run: no recipe executes), then gate the build on the result.")
	}
	if len(invocations) == 0 {
		c.add(RuleBuildEntrypoint,
			"no step invokes `make release-binaries` — the Makefile target is the single source of truth for the release "+
				"artifacts (DF-BUNKER-25); re-implementing the cross-compile in YAML lets a local `make release-binaries` and "+
				"a published release drift.")
	}
	for _, p := range probes {
		if p.If != "" {
			c.add(RuleBuildEntrypoint,
				"the entrypoint probe step %q is conditional (%q): a probe that can be skipped guards nothing — it must run "+
					"unconditionally so an incompatible tag tree always fails the job.", p.Name, p.If)
		}
		if !strings.Contains(shellCode(p.Run), "exit 1") {
			c.add(RuleBuildEntrypoint,
				"the entrypoint probe step %q never fails the job when the tag's tree cannot satisfy the asset contract — "+
					"it must fail closed (exit 1) instead of letting an unbuildable tag continue.", p.Name)
		}
		if !strings.Contains(shellCode(p.Run), "::error::") {
			c.add(RuleBuildEntrypoint,
				"the entrypoint probe step %q refuses without an actionable '::error::' message naming the tag, what is "+
					"missing and the remedy — the point of the probe is a diagnosable failure, not a bare make error.", p.Name)
		}
	}
	for _, s := range invocations {
		if !strings.Contains(s.If, "steps.") || !strings.Contains(s.If, ".outputs."+BuildPathOutput) {
			c.add(RuleBuildEntrypoint,
				"the step invoking `make release-binaries` (%q) is not gated on the resolved build path (if: ... "+
					"steps.<id>.outputs.%s == 'make') — an ungated invocation is the INT-CI-021 failure mode: the target is "+
					"assumed to exist in the checked-out tree.", s.Name, BuildPathOutput)
		}
	}
}

// checkTagProvenance: rule 2.
func (w *Workflow) checkTagProvenance(c *verifyCollector) {
	verify, ok := w.FindStep(func(s Step) bool { return strings.Contains(shellCode(s.Run), "sha256sum --check SHA256SUMS") })
	if !ok {
		c.add(RuleTagProvenance,
			"no step verifies the built assets (nothing runs `sha256sum --check SHA256SUMS`) — publish without verification "+
				"would attach whatever dist/ holds.")
		return
	}
	code := shellCode(verify.Run)
	if !strings.Contains(code, "vcs.revision") {
		c.add(RuleTagProvenance,
			"the asset verification step never proves WHICH tree the binaries were built from: read the toolchain's "+
				"embedded revision (`go version -m <asset>` → `vcs.revision`) and require it to equal the checked-out tag "+
				"commit. The `bunker <version>` stamp alone is satisfied by any tree built with VERSION=<tag> — the assets "+
				"currently attached to v0.1.4 are such a build (vcs.revision=235e715, a main commit).")
	}
	if !strings.Contains(code, "git rev-parse") {
		c.add(RuleTagProvenance,
			"the asset verification step never resolves the tag commit (`git rev-parse ${RELEASE_TAG}^{commit}` vs `git rev-parse HEAD`) — "+
				"the workflow must prove the checkout IS the tag it publishes assets for, so a checkout of another ref fails "+
				"loudly instead of publishing that ref's build under the tag.")
	}
	if !strings.Contains(code, "exit 1") {
		c.add(RuleTagProvenance,
			"the asset verification step reports but does not enforce provenance (no `exit 1`) — a mismatch must fail the "+
				"job before the upload step runs.")
	}
}

// checkAssetContract: rule 3.
func (w *Workflow) checkAssetContract(c *verifyCollector) {
	declared := strings.Fields(w.Env["RELEASE_ASSETS"])
	have := map[string]bool{}
	for _, a := range declared {
		have[strings.TrimPrefix(a, "dist/")] = true
	}
	for _, want := range ReleaseAssets {
		if !have[want] {
			c.add(RuleAssetContract,
				"RELEASE_ASSETS no longer carries %q — install.sh downloads the six published assets by name; dropping one "+
					"breaks the documented one-command install.", want)
		}
	}
	for name := range have {
		if !contains(ReleaseAssets, name) {
			c.add(RuleAssetContract,
				"RELEASE_ASSETS carries %q, which is not part of the six-asset contract — an extra asset is either a typo "+
					"or a contract change that install.sh and the publish assertions must be updated for together.", name)
		}
	}
	if len(declared) != len(ReleaseAssets) {
		c.add(RuleAssetContract,
			"RELEASE_ASSETS lists %d assets; the contract is exactly %d (4 binaries + SHA256SUMS + install.sh).",
			len(declared), len(ReleaseAssets))
	}

	verify, ok := w.FindStep(func(s Step) bool { return strings.Contains(shellCode(s.Run), "sha256sum --check SHA256SUMS") })
	if !ok {
		return // reported by checkTagProvenance.
	}
	verifyCode := shellCode(verify.Run)
	for _, anchor := range []struct{ needle, why string }{
		{"for platform in amd64 arm64", "both published architectures must be built and verified"},
		{"GOARCH=", "each binary must be proven to target the architecture it is published for"},
		{"GOOS=linux", "each binary must be proven to be a linux build — only linux/amd64 and linux/arm64 are published"},
		{"version | grep -q", "the binary's version stamp must match the tag being published"},
		{"cmp scripts/install.sh dist/install.sh", "the published installer must be byte-identical to the one in the checked-out tree"},
		{"sha256sum --check SHA256SUMS", "the release checksums must be verified before anything is published"},
	} {
		if !strings.Contains(verifyCode, anchor.needle) {
			c.add(RuleAssetContract, "the asset verification step lost %q: %s.", anchor.needle, anchor.why)
		}
	}

	publish, ok := w.FindStep(func(s Step) bool { return strings.Contains(shellCode(s.Run), "gh release ") })
	if !ok {
		c.add(RuleAssetContract,
			"no step publishes the release (`gh release create`/`upload`) — the six assets have nowhere to land.")
		return
	}
	publishCode := shellCode(publish.Run)
	if !strings.Contains(publishCode, "--clobber") {
		c.add(RuleAssetContract,
			"the publish step no longer uploads idempotently (`--clobber`): re-running the workflow for a tag would fail "+
				"on the assets that already exist there.")
	}
	for _, want := range ReleaseAssets {
		if !strings.Contains(publishCode, want) {
			c.add(RuleAssetContract,
				"the publish step no longer asserts the published asset %q — a release missing it must fail the job, not "+
					"report success with a partial asset set.", want)
		}
	}
}

// checkSourceRef: rule 4.
func (w *Workflow) checkSourceRef(c *verifyCollector) {
	checkouts := 0
	for _, s := range w.Steps {
		if !strings.HasPrefix(s.Uses, "actions/checkout") {
			continue
		}
		checkouts++
		ref := s.With["ref"]
		if !strings.Contains(ref, "steps.tag.outputs.tag") {
			c.add(RuleSourceRef,
				"the %s step checks out ref %q instead of the resolved release tag (steps.tag.outputs.tag) — the build tree "+
					"must be the tag whose assets are published, never a branch.", s.Uses, ref)
		}
	}
	if checkouts == 0 {
		c.add(RuleSourceRef,
			"no actions/checkout step found — the release build must run in a checkout of the selected tag's tree.")
	}
	for _, s := range w.Steps {
		if s.Run == "" {
			continue
		}
		if m := refSwitchRE.FindString(shellCode(s.Run)); m != "" {
			c.add(RuleSourceRef,
				"step %q runs %q: switching, archiving, fetching or cloning a ref inside the release job can build one tree "+
					"and publish it under another tag. Select the ref once, in actions/checkout `ref:` (the resolved tag), and "+
					"build only the checked-out tree.", s.Name, m)
		}
	}
}

func stringify(in map[string]any) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = fmt.Sprint(v)
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
