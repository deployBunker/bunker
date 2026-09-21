package hostsetup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The Regression suite job (self-hosted `bunker` runner) is the gate that runs
// regression-tests.sh against a real host. On run 34778344738 that job was a
// hard FAILURE — 31 PASS / 2 FAIL (`exec whoami returns agent username`,
// `exec propagates exit code`) so the E2E battery step never ran — even though
// the run-level status stayed green (`continue-on-error: true`).
//
// Root cause: regression-tests.sh invokes BARE `bunker` / `bunkerd`, so the
// runner resolved them through PATH to the stale /usr/local host baseline — a
// build that predates the GAP-075 private-/tmp + PAM boundary runtime setup, so
// `exec` into a freshly spawned agent was denied. The job's own freshness guard
// only ever looked at the workspace build, and the E2E battery step is the only
// step that pinned the candidate binaries.
//
// The workflow is the authoritative surface that decides WHICH binaries the
// suite exercises, so it is pinned here — statically, no runner required — next
// to the two script facts that make the PATH wiring effective. A cheap CI edit
// (dropping the PATH export, or repointing the battery at /usr/local) fails in
// `go test ./...` on every push instead of only on the self-hosted runner.

const (
	ciWorkflowPath             = "../../.github/workflows/ci.yml"
	regressionSuiteScriptPath  = "../../regression-tests.sh"
	e2eBatteryScriptPath       = "../../e2e-full-battery.sh"
	rootSuiteScriptPath        = "../../scripts/root-suite.sh"
	regressionSuiteStepName    = "Regression suite"
	e2eBatteryStepName         = "E2E battery"
	rootSuiteStepName          = "Root-gated suite (TestSpawn|TestCgroup|TestConcurrency)"
	githubWorkspaceContextExpr = "${{ github.workspace }}"
)

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// workflowStepBlock returns the raw YAML of the step whose `- name:` is name.
// A step block starts at its `- name:` line and ends at the next `- ` entry at
// the SAME indentation (nested sequences live deeper, so they stay inside).
func workflowStepBlock(t *testing.T, workflow, name string) string {
	t.Helper()
	lines := strings.Split(workflow, "\n")
	start, indent := -1, ""
	for i, line := range lines {
		if strings.TrimSpace(line) != "- name: "+name {
			continue
		}
		start = i
		indent = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		break
	}
	if start < 0 {
		t.Fatalf("%s: no step named %q — this guard is stale", ciWorkflowPath, name)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], indent+"- ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// stripYAMLComments removes whole-line YAML comments so an assertion can never
// be satisfied by prose in the explanatory comment above a step (the fix's own
// comment names the PATH wiring).
func stripYAMLComments(block string) string {
	kept := make([]string, 0, 32)
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// ciRegressionPathPreamble returns the commands the Regression suite step runs
// BEFORE the suite itself — the PATH export and the provenance proof. The step
// must be a literal block scalar for those commands to exist at all.
func ciRegressionPathPreamble(t *testing.T, workflow string) []string {
	t.Helper()
	step := stripYAMLComments(workflowStepBlock(t, workflow, regressionSuiteStepName))
	lines := strings.Split(step, "\n")
	runIdx, runIndent := -1, ""
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "run:") {
			continue
		}
		runIdx = i
		runIndent = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		break
	}
	if runIdx < 0 {
		t.Fatalf("the %q step has no `run:` key:\n%s", regressionSuiteStepName, step)
	}
	if strings.TrimSpace(lines[runIdx]) != "run: |" {
		t.Fatalf("the %q step's run block is no longer a literal block scalar (%q) — the PATH export and its provenance proof have nowhere to live",
			regressionSuiteStepName, strings.TrimSpace(lines[runIdx]))
	}
	var body []string
	for i := runIdx + 1; i < len(lines); i++ {
		line := lines[i]
		if len(line)-len(strings.TrimLeft(line, " \t")) <= len(runIndent) {
			break
		}
		body = append(body, line)
	}
	preamble := make([]string, 0, len(body))
	for _, line := range body {
		if strings.Contains(line, "bash regression-tests.sh") {
			break
		}
		preamble = append(preamble, line)
	}
	return preamble
}

func writeStubBinary(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", path, err)
	}
}

// TestCIRegressionPathPreambleSelectsWorkspaceBinary runs the step's PATH
// preamble for real under the shell semantics GitHub uses (`bash -e`): with a
// decoy `bunker`/`bunkerd` EARLIER on PATH than the workspace, and with the
// workspace binaries present, the provenance proof must accept the workspace
// ones; with the workspace binaries missing the proof must fail the step loudly
// instead of silently exercising the decoy.
func TestCIRegressionPathPreambleSelectsWorkspaceBinary(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	preamble := ciRegressionPathPreamble(t, readRepoFile(t, ciWorkflowPath))
	if len(preamble) == 0 {
		t.Fatalf("the %q step has no commands before `bash regression-tests.sh` — nothing selects the workspace binaries",
			regressionSuiteStepName)
	}
	script := filepath.Join(t.TempDir(), "regression-preamble.sh")
	if err := os.WriteFile(script, []byte("set -euo pipefail\n"+strings.Join(preamble, "\n")+"\n"), 0o700); err != nil {
		t.Fatalf("write preamble script: %v", err)
	}

	decoy := t.TempDir()
	writeStubBinary(t, decoy, "bunker")
	writeStubBinary(t, decoy, "bunkerd")

	run := func(workspace string) (int, string) {
		t.Helper()
		cmd := exec.Command("bash", script)
		cmd.Env = []string{
			"PATH=" + decoy + ":" + os.Getenv("PATH"),
			"GITHUB_WORKSPACE=" + workspace,
		}
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("run preamble: %v\n%s", err, out)
			}
			code = exitErr.ExitCode()
		}
		return code, string(out)
	}

	workspace := t.TempDir()
	writeStubBinary(t, workspace, "bunker")
	writeStubBinary(t, workspace, "bunkerd")
	if code, out := run(workspace); code != 0 {
		t.Errorf("preamble rejected the workspace binaries (exit %d) even though they exist:\n%s", code, out)
	}

	// A runner whose workspace build is missing must NOT fall through to the
	// decoy baseline — that is the silent-stale-binary class this fix removes.
	empty := t.TempDir()
	if code, out := run(empty); code == 0 {
		t.Errorf("preamble accepted a workspace WITHOUT bunker/bunkerd (exit 0), so the suite would silently run the PATH baseline again:\n%s", out)
	}
}

// TestCIRegressionSuiteRunsWorkspaceBinaries pins the fix for the job-level
// rejection: the Regression suite step must put the job's just-built workspace
// binaries FIRST on PATH and prove that resolution before the suite runs, and
// the E2E battery step must keep its explicit BUNKER_BIN/BUNKERD_BIN wiring.
func TestCIRegressionSuiteRunsWorkspaceBinaries(t *testing.T) {
	workflow := readRepoFile(t, ciWorkflowPath)

	step := stripYAMLComments(workflowStepBlock(t, workflow, regressionSuiteStepName))

	// 1. The step must still run the real suite (and not quietly skip it).
	if !strings.Contains(step, "bash regression-tests.sh") {
		t.Errorf("the %q step no longer runs `bash regression-tests.sh`:\n%s", regressionSuiteStepName, step)
	}

	// 2. PATH must be exported with the workspace ahead of the inherited PATH,
	//    and that export must happen BEFORE the suite starts.
	exportLine, suiteLine := -1, -1
	for i, line := range strings.Split(step, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "export PATH="):
			exportLine = i
			value := strings.Trim(strings.TrimPrefix(trimmed, "export PATH="), `"'`)
			first := strings.TrimSpace(strings.Split(value, ":")[0])
			if first != "${GITHUB_WORKSPACE}" && first != "$GITHUB_WORKSPACE" {
				t.Errorf("the %q step puts %q first on PATH; the workspace (%s or ${GITHUB_WORKSPACE}) must come first so bare `bunker`/`bunkerd` resolve to the just-built binaries",
					regressionSuiteStepName, first, githubWorkspaceContextExpr)
			}
		case strings.Contains(trimmed, "bash regression-tests.sh"):
			if suiteLine < 0 {
				suiteLine = i
			}
		}
	}
	if exportLine < 0 {
		t.Fatalf("the %q step does not export PATH — regression-tests.sh invokes bare `bunker`/`bunkerd`, so the runner falls back to the stale /usr/local baseline and `exec` is denied (job 103780758333, 31 PASS / 2 FAIL):\n%s",
			regressionSuiteStepName, step)
	}
	if suiteLine >= 0 && exportLine > suiteLine {
		t.Errorf("the %q step exports PATH after starting the suite, so the suite still resolves the stale host binaries:\n%s", regressionSuiteStepName, step)
	}

	// 3. Fail loud if the resolution ever picks something else: `command -v`
	//    must be compared against the workspace paths, so a future runner
	//    without the workspace build fails instead of silently testing a
	//    stale host binary.
	for _, want := range []string{"command -v bunkerd", "command -v bunker", `"${GITHUB_WORKSPACE}/bunkerd"`, `"${GITHUB_WORKSPACE}/bunker"`} {
		if !strings.Contains(step, want) {
			t.Errorf("the %q step no longer proves the resolved binaries are the workspace build (missing %q) — a stale host binary could be exercised silently:\n%s",
				regressionSuiteStepName, want, step)
		}
	}

	// 4. The battery keeps its explicit wiring (INT-CI-003) — the PATH export
	//    is additive, never a replacement.
	battery := stripYAMLComments(workflowStepBlock(t, workflow, e2eBatteryStepName))
	for _, want := range []string{"BUNKER_BIN: " + githubWorkspaceContextExpr + "/bunker", "BUNKERD_BIN: " + githubWorkspaceContextExpr + "/bunkerd"} {
		if !strings.Contains(battery, want) {
			t.Errorf("the %q step lost its explicit candidate-binary wiring (%q missing):\n%s", e2eBatteryStepName, want, battery)
		}
	}
}

// TestRegressionSuiteInvokesBinariesViaPath pins the script-side fact that makes
// the PATH wiring authoritative: the suite must resolve both binaries through
// PATH. The /usr/local baseline may only be an existence assertion — executing
// it is exactly what produced the 2 failing exec cells.
func TestRegressionSuiteInvokesBinariesViaPath(t *testing.T) {
	script := readRepoFile(t, regressionSuiteScriptPath)

	// Bare invocations = PATH-resolved. The daemon start is the one that
	// matters for spawn/exec behaviour (it writes the systemd units and the
	// SSH forced command that implement the private-/tmp boundary).
	for _, want := range []string{`bunkerd -c "$REGRESSION_CONFIG"`, "bunker connect", "bunker spawn", "bunker exec"} {
		if !strings.Contains(script, want) {
			t.Errorf("%s no longer contains the bare invocation %q — if the suite switched to an absolute path, the CI PATH wiring is dead and the pushed commit is no longer what runs",
				regressionSuiteScriptPath, want)
		}
	}

	// No absolute host path may be executed (command position). Existence
	// assertions are fine; anything else would pin the run to the stale
	// /usr/local build.
	commandPosition := regexp.MustCompile(`^[ \t]*["']?/usr/local/bin/bunkerd?["']?[ \t]`)
	pipelineUse := regexp.MustCompile(`[|&;(][ \t]*["']?/usr/local/bin/bunkerd?["']?[ \t]`)
	for i, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if commandPosition.MatchString(line) || pipelineUse.MatchString(line) {
			t.Errorf("%s:%d executes the absolute host baseline (%q); the suite must keep resolving bunker/bunkerd through PATH", regressionSuiteScriptPath, i+1, strings.TrimSpace(line))
		}
		if strings.Contains(line, "/usr/local/bin/bunker") && !strings.Contains(line, "[ -f ") {
			t.Errorf("%s:%d references the host baseline %q outside an existence assertion — the CI PATH wiring only decides what runs if the suite never executes an absolute path", regressionSuiteScriptPath, i+1, strings.TrimSpace(line))
		}
	}
}

// TestCIWiringArmsInsecureDevOptIn pins the INT-CI-028 wiring: since GAP-126,
// CheckTLS refuses a plaintext daemon on the wildcard battery ports
// (:29090-91/:28080-81 classify NON-loopback), so the CI battery daemons died
// at startup and every cell failed for lack of a server. The daemons get the
// opt-in from the tls.insecure_dev key the suites write into their generated
// configs; these CI steps must ALSO carry the BUNKERD_TLS_INSECURE_DEV env
// name so a future config-shape change cannot silently drop the opt-in. The
// production gate itself is NOT pinned to change — only the opt-in.
func TestCIWiringArmsInsecureDevOptIn(t *testing.T) {
	workflow := readRepoFile(t, ciWorkflowPath)

	for _, tc := range []struct {
		stepName string
		grpcPort string
		restPort string
	}{
		{stepName: regressionSuiteStepName, grpcPort: ":29090", restPort: ":28080"},
		{stepName: e2eBatteryStepName, grpcPort: ":29091", restPort: ":28081"},
	} {
		t.Run(tc.stepName, func(t *testing.T) {
			step := stripYAMLComments(workflowStepBlock(t, workflow, tc.stepName))

			// The step must still target the battery ports (a port change
			// here usually means a renumber, not a security change — but the
			// opt-in below is only meaningful next to these binds).
			for _, want := range []string{"BUNKERD_GRPC_ADDR: \"" + tc.grpcPort + "\"", "BUNKERD_REST_ADDR: \"" + tc.restPort + "\""} {
				if !strings.Contains(step, want) {
					t.Errorf("the %q step no longer binds %s/%s (missing %q):\\n%s", tc.stepName, tc.grpcPort, tc.restPort, want, step)
				}
			}

			// The explicit opt-in env name must be set in the step itself —
			// not inherited from the environment, not commented out.
			if !strings.Contains(step, "BUNKERD_TLS_INSECURE_DEV: \"true\"") {
				t.Errorf("the %q step lost BUNKERD_TLS_INSECURE_DEV: \"true\" — CheckTLS will refuse the wildcard plaintext binds and every cell fails for lack of a server:\\n%s", tc.stepName, step)
			}
		})
	}
}

// TestBatteryScriptsArmInsecureDevInGeneratedConfig pins the script-side half
// of INT-CI-028: BOTH suites start their coexist daemons from GENERATED
// config files, so the opt-in must live in those heredocs — env vars are not
// the mechanism when a config file is passed with -c (viper: explicit file
// values beat AutomaticEnv). Also pinned: the readiness probes that name a
// startup death instead of cascading red cells.
func TestBatteryScriptsArmInsecureDevInGeneratedConfig(t *testing.T) {
	for _, tc := range []struct {
		scriptPath  string
		configVar   string
		tlsKey      string
		refusalNote string
	}{
		{scriptPath: regressionSuiteScriptPath, configVar: "REGRESSION_CONFIG", tlsKey: "tls:", refusalNote: "refusing to bind non-loopback plaintext listener"},
		{scriptPath: e2eBatteryScriptPath, configVar: "BATTERY_CONFIG", tlsKey: "tls:", refusalNote: "refusing to bind non-loopback plaintext listener"},
	} {
		t.Run(tc.scriptPath, func(t *testing.T) {
			script := readRepoFile(t, tc.scriptPath)

			// The generated config heredoc must carry the opt-in key. Search
			// from the heredoc start (`cat > "$VAR"`) to its EOF terminator:
			// a `tls:` line elsewhere (comment, unrelated block) must not
			// satisfy this.
			configArmed := false
			for _, block := range strings.Split(script, "cat > \"$"+tc.configVar+"\"") {
				if len(block) == len(script) {
					continue // split found nothing: the heredoc writer is absent
				}
				heredoc := strings.SplitN(block, "\nEOF\n", 2)[0]
				for _, line := range strings.Split(heredoc, "\n") {
					trimmed := strings.TrimSpace(line)
					if trimmed == "tls:" || strings.HasPrefix(trimmed, "tls:") {
						configArmed = true
					}
				}
			}
			if !configArmed {
				t.Errorf("%s generates $%s WITHOUT a tls: block — CheckTLS refuses wildcard plaintext binds since GAP-126, so the coexist daemon dies at startup (INT-CI-028)", tc.scriptPath, tc.configVar)
			}

			// The readiness diagnostics must name the known refusal shape so
			// a future regression reads its cause, not a cascade of reds.
			if !strings.Contains(script, tc.refusalNote) {
				t.Errorf("%s readiness probe no longer names the CheckTLS refusal shape %q — a startup death cascades into unattributable cell failures", tc.scriptPath, tc.refusalNote)
			}
		})
	}
}

// TestBatteryNestedSuiteRunsCertifiedBinary pins INT-CI-031: the battery's
// nested regression suite must resolve `bunker` from the directory of the
// certified $BUNKER binary. On run 35565852250 the battery's E2E step does
// not pin PATH, so the child's bare `bunker` fell through to the runner's
// stale /usr/local baseline (v0.1.4, 6a6ad20 — predates the GAP-093
// fail-closed binding fix) and the re-check cell got `no active server`
// instead of the required refusal message, while the battery's CERTIFIED
// BINARY line (verdict MATCH) covered only $BUNKER_BIN. The battery must
// exercise the same binary it certifies: the child env gets the certified
// binary's directory PREPENDED to PATH (both bunker and bunkerd live there
// in CI workspace and in the /usr/local default, so one dirname pins both).
func TestBatteryNestedSuiteRunsCertifiedBinary(t *testing.T) {
	script := readRepoFile(t, e2eBatteryScriptPath)

	// Exactly one nested-suite invocation site (INT-CI-012 doctrine), and it
	// must carry the PATH pin.
	pinLine := ""
	invocations := 0
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, `run_capture "nested regression suite"`) {
			invocations++
			pinLine = line
		}
	}
	if invocations != 1 {
		t.Fatalf("e2e-full-battery.sh has %d `run_capture \"nested regression suite\"` invocations, want exactly 1 — the pin is unverifiable with more than one site (last: %s)", invocations, pinLine)
	}

	// env flags stay before name=value pairs, and the certified binary's
	// directory comes FIRST in the child's PATH.
	envFlagAndPin := `env -u BUNKER_SESSION_TARGET PATH="$(dirname "$BUNKER"):$PATH"`
	if !strings.Contains(pinLine, envFlagAndPin) {
		t.Errorf("the nested regression invocation lost the certified-binary PATH pin (%q missing) — the child falls back to the runner's stale /usr/local baseline and the battery no longer exercises the binary it certifies:\n%s", envFlagAndPin, pinLine)
	}

	// Every env assignment the child received before the fix must survive:
	// the pin is additive, never a replacement.
	for _, want := range []string{
		`BUNKER_HOME="$NESTED_CLI_HOME"`,
		`HOME="$NESTED_CLI_HOME"`,
		`BUNKERD_GRPC_ADDR="$NESTED_GRPC_ADDR"`,
		`BUNKERD_REST_ADDR="$NESTED_REST_ADDR"`,
		`bash "$REGRESSION_SCRIPT"`,
	} {
		if !strings.Contains(pinLine, want) {
			t.Errorf("the nested regression invocation lost %q when the PATH pin landed:\n%s", want, pinLine)
		}
	}

	// Self-attributing line: the job log must show which directory the child
	// will resolve `bunker`/`bunkerd` from, next to the CERTIFIED BINARY
	// verdict for the same run.
	if !strings.Contains(script, "nested suite PATH pin:") {
		t.Errorf("e2e-full-battery.sh no longer echoes the `nested suite PATH pin:` line — a repeat of run 35565852250 would not show what the child resolved against the certification verdict")
	}
}

// TestRootSuiteBudgetFitsCIWindow pins the INT-CI-031 budget raise: the
// root-gated suite's real cost grew past the 780s rung (run 35565852250:
// internal/agent consumed the FULL 780s with spawns/destroys still flowing —
// the GAP-126..142 security wave added root-gated spawn tests). Per the
// INT-CI-006 doctrine the budget rises, never the -run filter: 1050s default
// under a 20m CI step window (1200s), leaving 150s of headroom so the
// wrapper's EXIT-trap leak cleanup (GAP-007: zero leaked users/keys) still
// runs inside the window.
func TestRootSuiteBudgetFitsCIWindow(t *testing.T) {
	script := readRepoFile(t, rootSuiteScriptPath)
	const budgetSeconds = 1050
	if !strings.Contains(script, `ROOT_SUITE_TIMEOUT="${ROOT_SUITE_TIMEOUT:-1050s}"`) {
		t.Errorf("%s does not default ROOT_SUITE_TIMEOUT to %ds — the suite's real cost passed the previous 780s rung (run 35565852250, internal/agent consumed the full budget while progressing)", rootSuiteScriptPath, budgetSeconds)
	}

	workflow := readRepoFile(t, ciWorkflowPath)
	step := stripYAMLComments(workflowStepBlock(t, workflow, rootSuiteStepName))
	m := regexp.MustCompile(`(?m)^\s*timeout-minutes:\s*(\d+)\s*$`).FindStringSubmatch(step)
	if m == nil {
		t.Fatalf("the %q step has no timeout-minutes — the budget's cleanup headroom is unguardable:\n%s", rootSuiteStepName, step)
	}
	minutes, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse timeout-minutes %q: %v", m[1], err)
	}
	if minutes != 20 {
		t.Errorf("the %q step timeout-minutes = %d, want 20 (run 35565852250 reddened at the 15m window; the 1050s budget needs 1200s)", rootSuiteStepName, minutes)
	}

	// The encoded headroom invariant: 1200s window - 1050s budget >= 150s
	// for the leak-cleanup EXIT trap. Expressed against the parsed step
	// value so a future window edit must keep the math honest.
	if headroom := minutes*60 - budgetSeconds; headroom < 150 {
		t.Errorf("root-suite window %dm (%ds) leaves only %ds of headroom under the %ds budget; >= 150s is required for the wrapper's EXIT-trap leak cleanup", minutes, minutes*60, headroom, budgetSeconds)
	}
}
