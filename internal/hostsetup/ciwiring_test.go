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

	// INT-CI-083: the classification step and the two self-hosted jobs. The
	// step body is EXECUTED by TestCIChangesReportsCodeTrueForNonPushEvents, so
	// these names are load-bearing, not labels.
	ciChangesStepName   = "Detect code changes"
	ciChangesJobName    = "changes"
	ciRegressionJobName = "regression"
	ciRootSuiteJobName  = "root-suite"
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

// TestRootSuiteBudgetFitsCIWindow pins the root-suite budget and the CI step
// window it must fit inside. Two raises so far, both on real measurements:
// 780s (INT-CI-031, run 35565852250: internal/agent consumed the FULL 780s
// with spawns/destroys still flowing) and 1050s -> 1590s (INT-CI-050a, run
// 36494196879: 45 of the 55 root-gated internal/agent tests in 1049s, still
// progressing, MAX inter-line gap 50.4s — a budget cliff, not a hang).
// Per the INT-CI-006 doctrine the budget rises, never the -run filter: 1590s
// default under a 30m CI step window (1800s), leaving 210s of headroom so the
// wrapper's EXIT-trap leak cleanup (GAP-007: zero leaked users/keys) still
// runs inside the window.
func TestRootSuiteBudgetFitsCIWindow(t *testing.T) {
	script := readRepoFile(t, rootSuiteScriptPath)
	const budgetSeconds = 1590
	if !strings.Contains(script, `ROOT_SUITE_TIMEOUT="${ROOT_SUITE_TIMEOUT:-1590s}"`) {
		t.Errorf("%s does not default ROOT_SUITE_TIMEOUT to %ds — the suite's real cost passed the previous 1050s rung (run 36494196879, internal/agent completed 45 of 55 selected tests in the full 1049s while still progressing)", rootSuiteScriptPath, budgetSeconds)
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
	if minutes != 30 {
		t.Errorf("the %q step timeout-minutes = %d, want 30 (the %ds budget needs a 1800s window)", rootSuiteStepName, minutes, budgetSeconds)
	}

	// The encoded headroom invariant: 1800s window - 1590s budget >= 150s
	// for the leak-cleanup EXIT trap. Expressed against the parsed step
	// value so a future window edit must keep the math honest.
	if headroom := minutes*60 - budgetSeconds; headroom < 150 {
		t.Errorf("root-suite window %dm (%ds) leaves only %ds of headroom under the %ds budget; >= 150s is required for the wrapper's EXIT-trap leak cleanup", minutes, minutes*60, headroom, budgetSeconds)
	}
}

// TestPortPreflightGuardsDaemonStartup pins INT-CI-077: BOTH battery startup
// paths (regression-tests.sh around `bunkerd -c`, e2e-full-battery.sh around
// `"$BUNKERD_BIN" -c`) must pre-flight the EXACT requested ports BEFORE the
// daemon is started and refuse with ONE actionable cell naming the owning
// stale pid, instead of dying at startup on "address already in use" hidden
// in the daemon log (CI run 39544219096, job 119038796197, head 85c802e6: a
// previous job's bunkerd survived on the persistent runner and still held
// :28081/:29091, so the run produced zero cells). The helper stays
// detection-only — the production-daemon safety rules expressed by
// host_state_sweep_allowed / BUNKERD_COEXIST are untouched, the refusal
// carries the reclaim guidance (and the systemd-daemon warning), and the
// kill itself is left to the operator. The behavioural half of this contract
// (refusal shapes, pid-less snapshots, no invented pid) is proven hermetically
// by scripts/test-port-preflight.sh against a stub `ss`; this test pins the
// wiring statically so a dropped invocation or a weakened refusal fails in
// `go test ./...` on every push. RED/GREEN evidence lives in
// scripts/test-port-preflight.sh (RED against git show dae24fdb:<path>).
func TestPortPreflightGuardsDaemonStartup(t *testing.T) {
	for _, tc := range []struct {
		scriptPath string
		configVar  string
		startLine  string
		refusalRC  string
	}{
		{scriptPath: regressionSuiteScriptPath, configVar: "REGRESSION_CONFIG", startLine: `bunkerd -c "$REGRESSION_CONFIG"`, refusalRC: "exit 2"},
		{scriptPath: e2eBatteryScriptPath, configVar: "BATTERY_CONFIG", startLine: `"$BUNKERD_BIN" -c "$BATTERY_CONFIG"`, refusalRC: "exit 42"},
	} {
		t.Run(tc.scriptPath, func(t *testing.T) {
			script := readRepoFile(t, tc.scriptPath)

			if got := strings.Count(script, "port_preflight() {"); got != 1 {
				t.Fatalf("%s defines port_preflight() %d time(s), want exactly 1", tc.scriptPath, got)
			}

			// The invocation site must precede the daemon start in the MAIN
			// path (`if ! port_preflight "$BUNKERD_GRPC_ADDR" ...` — the
			// self-test drives the helper directly and must not match).
			inv := strings.Index(script, `if ! port_preflight "$BUNKERD_GRPC_ADDR" "$BUNKERD_REST_ADDR"; then`)
			if inv < 0 {
				t.Fatalf("%s never invokes the port preflight before its daemon start — a stale listener from a previous job kills the run before any cell (run 39544219096)", tc.scriptPath)
			}
			start := strings.Index(script, tc.startLine)
			if start < 0 {
				t.Fatalf("%s lost its daemon start line %q", tc.scriptPath, tc.startLine)
			}
			if inv > start {
				t.Fatalf("%s invokes the port preflight AFTER the daemon start (offset %d > %d) — too late to prevent the bind loss", tc.scriptPath, inv, start)
			}

			// The refusal must be the harness's own documented preflight exit
			// (regression-tests.sh: 2; e2e-full-battery.sh: 42), not a silent
			// continue past a squatted port.
			if !strings.Contains(script[inv:start], tc.refusalRC) {
				t.Fatalf("%s port-preflight refusal does not %s before the daemon start — the run would continue into a bind loss", tc.scriptPath, tc.refusalRC)
			}

			// One actionable cell: the exact port, the owning stale pid and
			// the reclaim action, plus the production-daemon warning so the
			// systemd-managed bunkerd (INT-CI-012) is never the kill target.
			for _, want := range []string{
				`is already LISTENing`,
				`Owner pid $OWNER_PID`,
				"kill $OWNER_PID",
				`systemctl is-active bunkerd`,
				`address already in use`,
			} {
				if !strings.Contains(script, want) {
					t.Errorf("%s port_preflight lost %q — the refusal cell must name the port, the owning pid, the reclaim action, and warn off the systemd-managed production daemon", tc.scriptPath, want)
				}
			}

			// Detection only: the helper's body must not kill anything and
			// must not sleep the bind failure away (a generic retry papers
			// over exactly the residue this row exists for).
			bodyStart := strings.Index(script, "port_preflight() {")
			bodyEnd := strings.Index(script[bodyStart:], "\n}\n")
			body := script[bodyStart : bodyStart+bodyEnd]
			for _, forbidden := range []string{"pkill ", "\nkill ", "	sleep ", " sleep "} {
				if strings.Contains(body, forbidden) {
					t.Errorf("%s port_preflight body contains %q — the helper stays detection-only; reclamation belongs to the operator (host_state_sweep_allowed / BUNKERD_COEXIST rules untouched)", tc.scriptPath, forbidden)
				}
			}
		})
	}
}

// ───────── INT-CI-083: the CI gate must not skip the live battery ─────────
//
// Measured hole (2026-10-23): run 39898490380 (head 80a1abb4, push
// 04:05:58Z) carried the INT-CI-080 code fix. Its `changes` job armed the gate
// (code=true) and BOTH self-hosted jobs were CREATED (regression job
// 120234563521, root-suite job 120234563480). A BOARD-ONLY push c0fc342a at
// 04:20:16Z then superseded it: run-level `cancel-in-progress` was armed for
// push events, so run 39898490380 was CANCELLED, and the superseding run
// 39899780020 classified its own delta (git diff 80a1abb4 c0fc342a =
// .coding-hermes/board/tasks.jsonl) as code=false — SKIPPING regression and
// root-suite. The run reported success, the fix never ran the battery, and
// there was no way to force it: both self-hosted jobs required
// `github.event_name == 'push'`, so `gh workflow run ci.yml` always skipped them.
//
// The three tests below pin the fix from three different angles so a cheap YAML
// edit fails in `go test ./...` on every push instead of only on the single
// self-hosted runner: the cancellation decision as written, the job-level
// `if:` contract, and the classification step's ACTUAL behaviour executed
// against a real git history.

// workflowJobBlock returns the raw YAML of jobs.<jobName>: it starts at the job
// key (two-space indent under `jobs:`) and ends at the next sibling job key or
// top-level key. workflowStepBlock cannot see job-level keys such as
// `if:`/`needs:`/`runs-on:` — those only exist at this level.
func workflowJobBlock(t *testing.T, workflow, jobName string) string {
	t.Helper()
	lines := strings.Split(workflow, "\n")
	start := -1
	for i, line := range lines {
		if line == "  "+jobName+":" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s: no job %q — this guard is stale", ciWorkflowPath, jobName)
	}
	siblingKey := regexp.MustCompile(`^  [A-Za-z0-9_.-]+:\s*$`)
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		line := lines[i]
		if line == "" {
			continue // blank lines inside a job are not the job's end
		}
		if !strings.HasPrefix(line, " ") || siblingKey.MatchString(line) {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// ciJobIfCondition returns a job's `if:` condition, folding a `|`/`>` block
// scalar into one line. Comments are stripped so the assertion can never be
// satisfied by the explanatory prose above the job (that prose names
// workflow_dispatch too).
func ciJobIfCondition(t *testing.T, block, jobName string) string {
	t.Helper()
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "if:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "if:"))
		switch value {
		case "", "|", "|-", "|+", ">", ">-", ">+":
			indent := len(line) - len(strings.TrimLeft(line, " "))
			var parts []string
			for j := i + 1; j < len(lines); j++ {
				next := lines[j]
				if strings.TrimSpace(next) == "" {
					continue
				}
				if len(next)-len(strings.TrimLeft(next, " ")) <= indent {
					break
				}
				parts = append(parts, strings.TrimSpace(next))
			}
			value = strings.Join(parts, " ")
		}
		return stripYAMLComments(value)
	}
	t.Fatalf("the %q job has no `if:` condition — both self-hosted jobs must stay scoped to push+main+code and must ALSO accept workflow_dispatch:\n%s", jobName, block)
	return ""
}

// ciStepBlock returns the step named stepName from a job block, tolerating
// both `- name: X` (this workflow's house style, and the only shape
// workflowStepBlock can see) and `- id: y` followed by `name: X` on the next
// line — the shape the PRE-FIX classification step used. Scoped to the job so a
// same-named step elsewhere in the file cannot satisfy it.
func ciStepBlock(t *testing.T, jobBlock, stepName string) string {
	t.Helper()
	lines := strings.Split(jobBlock, "\n")
	want := "name: " + stepName
	start, indent := -1, 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.HasSuffix(trimmed, want) {
			continue
		}
		nameIndent := len(line) - len(strings.TrimLeft(line, " "))
		for j := i; j >= 0; j-- {
			if !strings.HasPrefix(strings.TrimSpace(lines[j]), "- ") {
				continue
			}
			dashIndent := len(lines[j]) - len(strings.TrimLeft(lines[j], " "))
			if dashIndent <= nameIndent {
				start, indent = j, dashIndent
				break
			}
		}
		break
	}
	if start < 0 {
		t.Fatalf("the job block has no step named %q — this guard is stale:\n%s", stepName, jobBlock)
	}
	end := len(lines)
	stepPrefix := strings.Repeat(" ", indent) + "- "
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], stepPrefix) {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// stepRunBody returns the commands of a step block's `run:` literal block
// scalar, comment-stripped. Used to run the `changes` classification for real.
func stepRunBody(t *testing.T, stepName, step string) string {
	t.Helper()
	step = stripYAMLComments(step)
	lines := strings.Split(step, "\n")
	runIdx, runIndent := -1, 0
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "run:") {
			continue
		}
		if strings.TrimSpace(line) != "run: |" {
			t.Fatalf("the %q step's run block is no longer a literal block scalar (%q) — its logic cannot be executed for real",
				stepName, strings.TrimSpace(line))
		}
		runIdx = i
		runIndent = len(line) - len(strings.TrimLeft(line, " "))
		break
	}
	if runIdx < 0 {
		t.Fatalf("the %q step has no `run:` key:\n%s", stepName, step)
	}
	var body []string
	for i := runIdx + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			body = append(body, "")
			continue
		}
		if len(line)-len(strings.TrimLeft(line, " ")) <= runIndent {
			break
		}
		body = append(body, line)
	}
	return strings.Join(body, "\n")
}

// ciConcurrencyRegion returns the run-level concurrency block (ok=false when
// the workflow declares none) plus the contiguous comment block directly above
// it, so a cancellation decision is read from ONE place rather than inferred
// from prose elsewhere in the file.
func ciConcurrencyRegion(t *testing.T, workflow string) (block string, ok bool) {
	t.Helper()
	lines := strings.Split(workflow, "\n")
	start := -1
	for i, line := range lines {
		if line == "concurrency:" {
			start = i
			break
		}
	}
	if start < 0 {
		return "", false
	}
	first := start
	for i := start - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			first = i
			continue
		}
		break
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if lines[i] == "" {
			continue
		}
		if !strings.HasPrefix(lines[i], " ") {
			end = i
			break
		}
	}
	return strings.Join(lines[first:end], "\n"), true
}

// ciCancelsOnEvent evaluates the block's `cancel-in-progress:` value AS WRITTEN
// for one event name. Only the shapes this repo can plausibly use are
// evaluated; an unrecognised expression fails the test instead of being
// guessed at — a new shape must extend this evaluator, which is exactly the
// point (no silent pass on an unread decision).
func ciCancelsOnEvent(t *testing.T, block, eventName string) (bool, string) {
	t.Helper()
	raw := ""
	for _, line := range strings.Split(stripYAMLComments(block), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "cancel-in-progress:") {
			raw = strings.TrimSpace(strings.TrimPrefix(trimmed, "cancel-in-progress:"))
		}
	}
	if raw == "" {
		return false, "(cancel-in-progress absent: no run is cancelled)"
	}
	expr := strings.TrimSpace(raw)
	if strings.HasPrefix(expr, "${{") && strings.HasSuffix(expr, "}}") {
		expr = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(expr, "${{"), "}}"))
	}
	switch expr {
	case "true":
		return true, raw
	case "false":
		return false, raw
	}
	if m := regexp.MustCompile(`^github\.event_name\s*==\s*'([^']*)'$`).FindStringSubmatch(expr); m != nil {
		return eventName == m[1], raw
	}
	if m := regexp.MustCompile(`^github\.event_name\s*!=\s*'([^']*)'$`).FindStringSubmatch(expr); m != nil {
		return eventName != m[1], raw
	}
	t.Fatalf("cancel-in-progress expression %q is not one this guard can evaluate — extend ciCancelsOnEvent rather than letting an unread cancellation decision pass", raw)
	return false, raw
}

// ciConcurrencyGroup returns the block's `group:` value ("" when absent).
func ciConcurrencyGroup(t *testing.T, block string) string {
	t.Helper()
	for _, line := range strings.Split(stripYAMLComments(block), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "group:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "group:"))
		}
	}
	return ""
}

// TestCIPushRunCannotBeCancelledIntoASkippedBattery pins INT-CI-083 invariant
// (i): a push run that carries code is never cancelled or replaced into a
// skipped battery. Run 39898490380 had code=true and both self-hosted jobs
// created; board-only push c0fc342a cancelled it, and the superseding run
// 39899780020 skipped regression + root-suite while reporting success.
//
// Two separate mechanisms must both be absent:
//   - cancel-in-progress evaluating true for a push run (the measured cancel);
//   - a concurrency group SHARED between runs: GitHub cancels any PENDING run
//     in a group when a newer run in that group is queued (`queue: single`
//     default), and cancel-in-progress only governs the IN-PROGRESS run — so a
//     code run waiting behind an in-flight ~25min battery would still be
//     replaceable by a later board-only push.
func TestCIPushRunCannotBeCancelledIntoASkippedBattery(t *testing.T) {
	workflow := readRepoFile(t, ciWorkflowPath)

	block, ok := ciConcurrencyRegion(t, workflow)
	if !ok {
		t.Logf("%s declares no run-level concurrency block: no run can cancel or replace another (INT-CI-083)", ciWorkflowPath)
	} else {
		for _, event := range []string{"push", "pull_request", "workflow_dispatch"} {
			if cancels, raw := ciCancelsOnEvent(t, block, event); cancels {
				t.Errorf("the run-level concurrency block cancels a %s run (cancel-in-progress: %s) — a push that carries code must never be cancelled into a skipped battery: board-only push c0fc342a cancelled run 39898490380 after its `changes` job had armed both self-hosted jobs, and the superseding run 39899780020 (board-only delta) then skipped regression + root-suite entirely:\n%s",
					event, raw, block)
			}
		}
		group := ciConcurrencyGroup(t, block)
		if group != "" &&
			!strings.Contains(group, "github.run_id") &&
			!strings.Contains(group, "github.run_number") &&
			!strings.Contains(group, "github.run_attempt") {
			t.Errorf("the concurrency group %q is shared between runs — GitHub replaces a PENDING run in the same group with the newest queued run (queue: single default), so a later board-only push can still displace a code run queued behind an in-flight battery. Drop the block or key the group on github.run_id/run_number/run_attempt:\n%s",
				group, block)
		}
	}

	// The decision must be justified in the workflow itself, naming the two
	// measured runs, so cancellation cannot be re-added without the author
	// reading why it went away.
	for _, want := range []string{"39898490380", "39899780020"} {
		if !strings.Contains(workflow, want) {
			t.Errorf("%s no longer documents run %s — the cancellation rationale must cite run 39898490380 (CANCELLED while carrying the INT-CI-080 code fix) and run 39899780020 (SKIPPED the battery behind a board-only delta)",
				ciWorkflowPath, want)
		}
	}
}

// TestCISelfHostedJobsRunOnDispatch pins INT-CI-083 invariant (ii): `regression`
// and `root-suite` must RUN on workflow_dispatch, so the battery can be forced
// on a ref that already landed (`gh workflow run ci.yml`). Before the fix both
// jobs required `github.event_name == 'push'`, so a dispatched run skipped them
// and the INT-CI-080 fix could not be certified at all. The INT-CI-079
// properties stay pinned in the same condition: push still needs main +
// code=true, and pull_request must never take the single self-hosted runner.
func TestCISelfHostedJobsRunOnDispatch(t *testing.T) {
	workflow := readRepoFile(t, ciWorkflowPath)

	for _, job := range []string{ciRegressionJobName, ciRootSuiteJobName} {
		t.Run(job, func(t *testing.T) {
			cond := ciJobIfCondition(t, workflowJobBlock(t, workflow, job), job)

			if !strings.Contains(cond, "github.event_name == 'workflow_dispatch'") {
				t.Errorf("the %q job's `if:` (%q) does not accept workflow_dispatch — `gh workflow run ci.yml` silently SKIPS the live battery, so a code commit that already landed (e.g. the INT-CI-080 fix 7cfd4c52) can never be certified", job, cond)
			}
			for _, want := range []string{
				"github.event_name == 'push'",
				"github.ref == 'refs/heads/main'",
				"needs.changes.outputs.code == 'true'",
			} {
				if !strings.Contains(cond, want) {
					t.Errorf("the %q job's `if:` (%q) no longer carries %q — a board-only push or a non-main ref could reach the single self-hosted runner again (INT-CI-079)", job, cond, want)
				}
			}
			for _, forbidden := range []string{"github.event_name != 'push'", "'pull_request'", "always()"} {
				if strings.Contains(cond, forbidden) {
					t.Errorf("the %q job's `if:` (%q) contains %q — pull_request runs must never take the single self-hosted runner, and the condition must stay explicit so a widened gate is visible", job, cond, forbidden)
				}
			}
		})
	}
}

// ciTestEnv returns the ambient environment minus every GIT_*/HOME/
// GITHUB_OUTPUT entry, plus the given overrides. Appending to os.Environ()
// instead would leave a host-global core.hooksPath (or an exported GIT_DIR)
// in force — exactly how a scratch-repo test starts testing the host config.
func ciTestEnv(extra map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if strings.HasPrefix(name, "GIT_") || name == "HOME" || name == "XDG_CONFIG_HOME" || name == "GITHUB_OUTPUT" {
			continue
		}
		if _, overridden := extra[name]; overridden {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func writeCIFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ciClassifierRepo builds a throwaway repository whose HEAD~1 -> HEAD delta is
// board-only (optionally plus one code file), so the real `changes` step body
// can be executed against a real git history instead of a text pattern. The
// repo is hermetic: GIT_CONFIG_GLOBAL/SYSTEM are /dev/null and HOME points at
// the temp dir, so the host's git config (hooks, autocrlf, signing) cannot
// decide the outcome.
func ciClassifierRepo(t *testing.T, withCodeChange bool) (dir, baseSHA string) {
	t.Helper()
	dir = t.TempDir()
	env := map[string]string{
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_CONFIG_SYSTEM":   "/dev/null",
		"HOME":                dir,
		"GIT_AUTHOR_NAME":     "ci-wiring-test",
		"GIT_AUTHOR_EMAIL":    "ci-wiring@example.invalid",
		"GIT_COMMITTER_NAME":  "ci-wiring-test",
		"GIT_COMMITTER_EMAIL": "ci-wiring@example.invalid",
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = ciTestEnv(env)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	git("init", "-q", "-b", "main")
	git("config", "commit.gpgsign", "false")
	board := filepath.Join(dir, ".coding-hermes", "board", "tasks.jsonl")
	writeCIFixtureFile(t, board, "{\"id\":\"INT-CI-083-a\"}\n")
	git("add", "-A")
	git("commit", "-q", "-m", "board: seed")
	baseSHA = git("rev-parse", "HEAD")

	writeCIFixtureFile(t, board, "{\"id\":\"INT-CI-083-a\"}\n{\"id\":\"INT-CI-083-b\"}\n")
	if withCodeChange {
		writeCIFixtureFile(t, filepath.Join(dir, "internal", "hostsetup", "ciwiring.go"), "package hostsetup\n")
	}
	git("add", "-A")
	git("commit", "-q", "-m", "next")
	return dir, baseSHA
}

// TestCIChangesReportsCodeTrueForNonPushEvents pins INT-CI-083 invariant (iii)
// BEHAVIOURALLY: the `changes` step's own body is run under `bash -e` against a
// real two-commit repository, with the workflow expressions substituted the way
// GitHub substitutes them per event. On the pre-fix workflow a dispatch on a
// board-only HEAD fell through to `HEAD~1`, classified the bookkeeping delta and
// wrote code=false — which is why run 39899780020 (head c0fc342a,
// .coding-hermes/board/tasks.jsonl only) skipped both self-hosted jobs.
//
// The push arms are the control: they prove INT-CI-079's property survives —
// board-only pushes still report code=false and stay off the single runner,
// while a code delta still arms the battery.
func TestCIChangesReportsCodeTrueForNonPushEvents(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	workflow := readRepoFile(t, ciWorkflowPath)

	// The step must live in the `changes` job — the job whose output the two
	// self-hosted jobs consume through needs.changes.outputs.code.
	changesJob := stripYAMLComments(workflowJobBlock(t, workflow, ciChangesJobName))
	if !strings.Contains(changesJob, "name: "+ciChangesStepName) {
		t.Fatalf("the %q step is no longer inside the %q job — the self-hosted gates read needs.changes.outputs.code and would silently lose their classifier:\n%s",
			ciChangesStepName, ciChangesJobName, changesJob)
	}
	if !strings.Contains(changesJob, "outputs:") || !strings.Contains(changesJob, "steps.diff.outputs.code") {
		t.Fatalf("the %q job no longer exports steps.diff.outputs.code — `needs.changes.outputs.code` would be empty and both self-hosted gates would compare against an empty string:\n%s",
			ciChangesJobName, changesJob)
	}

	body := stepRunBody(t, ciChangesStepName, ciStepBlock(t, changesJob, ciChangesStepName))

	// Static companion: the non-push branch must write code=true BEFORE any
	// diff/classification, so the dispatch path cannot be reordered behind
	// `git diff` (which is what produced code=false on run 39899780020).
	guardIdx := strings.Index(body, `!= "push"`)
	trueIdx := strings.Index(body, `echo "code=true"`)
	diffIdx := strings.Index(body, "git diff --name-only")
	if guardIdx < 0 || trueIdx < 0 {
		t.Fatalf("the %q step has no early non-push branch writing code=true — a workflow_dispatch run would classify its own delta and could SKIP both self-hosted jobs:\n%s",
			ciChangesStepName, body)
	}
	if guardIdx > trueIdx {
		t.Errorf("the %q step writes code=true before testing the event name — the dispatch branch must gate that write:\n%s", ciChangesStepName, body)
	}
	if diffIdx >= 0 && trueIdx > diffIdx {
		t.Errorf("the %q step writes the non-push code=true AFTER `git diff --name-only` — a dispatch on a bookkeeping HEAD would classify its own delta first:\n%s",
			ciChangesStepName, body)
	}

	for _, tc := range []struct {
		name       string
		eventName  string
		withBefore bool
		codeChange bool
		wantCode   string
	}{
		{name: "workflow_dispatch_on_board_only_head", eventName: "workflow_dispatch", codeChange: false, wantCode: "true"},
		{name: "pull_request_on_board_only_delta", eventName: "pull_request", codeChange: false, wantCode: "true"},
		{name: "push_board_only_delta_skips_battery", eventName: "push", withBefore: true, codeChange: false, wantCode: "false"},
		{name: "push_code_delta_arms_battery", eventName: "push", withBefore: true, codeChange: true, wantCode: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, baseSHA := ciClassifierRepo(t, tc.codeChange)

			before := ""
			if tc.withBefore {
				before = baseSHA
			}
			script := strings.NewReplacer(
				"${{ github.event.pull_request.base.sha }}", "",
				"${{ github.event.before }}", before,
				"${{ github.event_name }}", tc.eventName,
			).Replace(body)
			if strings.Contains(script, "${{") {
				t.Fatalf("the %q step grew a GitHub expression this guard does not substitute (%q) — extend the substitution so the step stays executable for real:\n%s",
					ciChangesStepName, "{{", script)
			}

			scriptPath := filepath.Join(t.TempDir(), "changes.sh")
			if err := os.WriteFile(scriptPath, []byte(script+"\n"), 0o700); err != nil {
				t.Fatalf("write step body: %v", err)
			}
			outFile := filepath.Join(t.TempDir(), "github_output")
			cmd := exec.Command("bash", "-e", scriptPath)
			cmd.Dir = repo
			cmd.Env = ciTestEnv(map[string]string{
				"GITHUB_OUTPUT":     outFile,
				"GIT_CONFIG_GLOBAL": "/dev/null",
				"GIT_CONFIG_SYSTEM": "/dev/null",
				"HOME":              repo,
			})
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the %q step body failed for event %q (code change: %v): %v\n%s",
					ciChangesStepName, tc.eventName, tc.codeChange, err, out)
			}

			raw, err := os.ReadFile(outFile)
			if err != nil {
				t.Fatalf("the step wrote no $GITHUB_OUTPUT for event %q: %v\nstep output:\n%s", tc.eventName, err, out)
			}
			var codes []string
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(line, "code=") {
					codes = append(codes, strings.TrimPrefix(line, "code="))
				}
			}
			if len(codes) != 1 {
				t.Fatalf("the step produced %d code= lines (%v), want exactly 1 — an ambiguous classifier output is how a gate silently reads the wrong arm:\n%s",
					len(codes), codes, raw)
			}
			if codes[0] != tc.wantCode {
				t.Errorf("event %q with a code change (%v) classified code=%s, want code=%s. A non-push event must yield code=true (a dispatch is intent to run the full battery); a push must still classify its delta (INT-CI-079: board-only pushes stay off the single self-hosted runner).\nstep output:\n%s",
					tc.eventName, tc.codeChange, codes[0], tc.wantCode, out)
			}
		})
	}
}
