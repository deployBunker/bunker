package hostsetup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The live E2E battery (e2e-full-battery.sh) is the acceptance gate for
// GAP-075, but it only runs on a root host, so the two semantics a rejected
// revision got wrong are pinned here — behaviourally and statically — where the
// Go suite catches a regression on any commit:
//
//  1. the SUMMARY ended with a bare `exit 0`, so the CI E2E step stayed green
//     while section 15 reported failures and no VERIFY-PASS was printed. The
//     summary block is executed for real, under the same `set -euo pipefail`
//     the battery runs with, and its exit code is asserted against the FAIL
//     count;
//  2. the per-agent scratch cap was read from the human-readable mount OPTION
//     (`size=16384k`, findmnt -o OPTIONS) and compared with the statfs BYTE
//     count — never equal — and `16384k` then aborted the `-le` arithmetic
//     ("integer expression expected"), so the over-cap ENOSPC proof was
//     silently skipped. The script must read bytes, validate digits before any
//     arithmetic, and prove the directory is its own tmpfs mount.

const batteryScriptPath = "../../e2e-full-battery.sh"

func readBatteryScript(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(batteryScriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", batteryScriptPath, err)
	}
	return string(data)
}

// batterySummarySnippet returns the battery's summary block (from the
// `# SUMMARY` marker to EOF). The block only reads PASS/FAIL/NOTE and echoes or
// exits, so it can be run on its own with those counters supplied.
func batterySummarySnippet(t *testing.T, script string) string {
	t.Helper()
	lines := strings.Split(script, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "# SUMMARY" {
			start = i
		}
	}
	if start < 0 {
		t.Fatalf("%s: no `# SUMMARY` marker found — the summary block moved, so this guard is stale", batteryScriptPath)
	}
	return strings.Join(lines[start+1:], "\n")
}

// runBatterySummary executes the real summary block with the given counters and
// returns its exit code plus combined output.
func runBatterySummary(t *testing.T, snippet string, pass, fail, note int) (int, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	script := fmt.Sprintf("set -euo pipefail\nPASS=%d\nFAIL=%d\nNOTE=%d\n%s\n", pass, fail, note, snippet)
	path := filepath.Join(t.TempDir(), "battery-summary.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write summary snippet: %v", err)
	}
	out, err := exec.Command("bash", path).CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run summary snippet: %v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	return code, string(out)
}

// TestBatterySummaryExitCodeTracksFailures is the regression guard for the
// status lie: a battery with FAIL>0 must exit non-zero and must NOT print
// VERIFY-PASS, while a clean battery exits 0 and prints exactly that marker.
func TestBatterySummaryExitCodeTracksFailures(t *testing.T) {
	snippet := batterySummarySnippet(t, readBatteryScript(t))
	for _, tc := range []struct {
		name        string
		pass, fail  int
		note        int
		wantCode    int
		wantMarkers []string
		denyMarkers []string
	}{
		{
			name: "clean run exits 0 and prints VERIFY-PASS",
			pass: 107, fail: 0, note: 0,
			wantCode: 0, wantMarkers: []string{"VERIFY-PASS"}, denyMarkers: []string{"VERIFY-FAIL"},
		},
		{
			name: "notes alone do not fail the battery",
			pass: 100, fail: 0, note: 4,
			wantCode: 0, wantMarkers: []string{"VERIFY-PASS", "4 issues documented"}, denyMarkers: []string{"VERIFY-FAIL"},
		},
		{
			name: "one failure exits non-zero and never claims VERIFY-PASS",
			pass: 107, fail: 1, note: 0,
			wantCode: 1, wantMarkers: []string{"VERIFY-FAIL", "1 FAILURES"}, denyMarkers: []string{"VERIFY-PASS"},
		},
		{
			name: "many failures exit non-zero",
			pass: 90, fail: 17, note: 2,
			wantCode: 1, wantMarkers: []string{"VERIFY-FAIL", "17 FAILURES"}, denyMarkers: []string{"VERIFY-PASS"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := runBatterySummary(t, snippet, tc.pass, tc.fail, tc.note)
			if code != tc.wantCode {
				t.Errorf("summary exit code = %d, want %d\n%s", code, tc.wantCode, out)
			}
			for _, marker := range tc.wantMarkers {
				if !strings.Contains(out, marker) {
					t.Errorf("summary output is missing %q:\n%s", marker, out)
				}
			}
			for _, marker := range tc.denyMarkers {
				if strings.Contains(out, marker) {
					t.Errorf("summary output contains %q, which must never appear here:\n%s", marker, out)
				}
			}
		})
	}
}

// TestBatteryReadsScratchCapAsNumericBytes pins the numeric mount-size path and
// the over-cap proof: the cap must come from `findmnt -b -n -o SIZE` (bytes),
// be validated as digits before any arithmetic, and the directory must be its
// own tmpfs mount — otherwise findmnt reports the CONTAINING filesystem and a
// pure size comparison calls an unbounded directory "bounded".
func TestBatteryReadsScratchCapAsNumericBytes(t *testing.T) {
	script := readBatteryScript(t)

	for _, want := range []string{
		`findmnt -b -n -o SIZE --target "$GAP075_ADIR"`, // byte-exact mount size
		`findmnt -n -o FSTYPE --target "$GAP075_ADIR"`,  // it must be tmpfs
		`mountpoint -q "$GAP075_ADIR"`,                  // and its own mount
		"[0-9]+$",                                       // digits validated first
		"stat -f -c %b",                                 // statfs byte cross-check
		"dd if=/dev/zero",                               // the real over-cap write
		"No space left",                                 // ENOSPC is the proof
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the battery no longer contains %q — the numeric cap path or the ENOSPC over-cap proof was dropped", want)
		}
	}

	// The rejected shape: parsing the human-readable mount option gives
	// `16384k`, which can never equal the statfs byte count and aborts numeric
	// arithmetic.
	if strings.Contains(script, `sed -n 's/^size=//p'`) {
		t.Error("the battery parses the human-readable `size=` mount option again: that value carries a unit suffix and cannot be compared with statfs bytes or used in arithmetic")
	}
}

// ────────────────────────────────────────────────────────────────────────────
// QA-BUNKER-B14 — host verification on VERIFY-PASS / 'CI closure' claims.
//
// The board row: commits claiming "E2E VERIFY-PASS" and "CI closure" were
// quoted as covering bunker-las-03 (the host the failing run_battery cell
// targeted) while the E2E battery only ever runs on bunker-mvp (AGENTS.md).
// Green CI + VERIFY-PASS coexisted with a FAILing battery cell on the SAME
// leak class because nothing in the verdict named the host it certified.
//
// The fix is proven here on three layers of the same contract:
//
//  1. BEHAVIOURAL — the summary block, executed for real, prints a
//     CERTIFIED HOST line and appends the host suffix to the verdict marker
//     whenever the probe ran (BATTERY_HOST_SUFFIX/BATTERY_DAEMON_HOSTNAME
//     supplied), and an unset variable degrades to the bare legacy marker so
//     the pre-B14 exit-code contract keeps holding on every path;
//  2. STATIC — the verdict lines interpolate the suffix variable and the
//     connect path records the daemon-reported hostname, so a battery that
//     drops the wiring cannot print a host-proven verdict;
//  3. STATIC (CI) — the E2E battery step pins BATTERY_EXPECTED_HOST, so a
//     "CI closure" claim quoted from a run names which host the run covered.
// ────────────────────────────────────────────────────────────────────────────

// runBatterySummaryHostEnv executes the summary block with the host globals
// preset the way the real battery leaves them after battery_host_connect_probe.
func runBatterySummaryHostEnv(t *testing.T, snippet string, pass, fail, note int, hostEnv map[string]string) (int, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	for k, v := range hostEnv {
		fmt.Fprintf(&b, "export %s=%q\n", k, v)
	}
	fmt.Fprintf(&b, "PASS=%d\nFAIL=%d\nNOTE=%d\n%s\n", pass, fail, note, snippet)
	path := filepath.Join(t.TempDir(), "battery-summary-host.sh")
	if err := os.WriteFile(path, []byte(b.String()), 0o700); err != nil {
		t.Fatalf("write summary snippet: %v", err)
	}
	out, err := exec.Command("bash", path).CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run summary snippet: %v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	return code, string(out)
}

// TestBatterySummaryVerdictCarriesHost is the behavioural half of QA-BUNKER-B14:
// a clean run with the host globals set must print the CERTIFIED HOST line and
// a VERIFY-PASS whose verdict names the host it certified — never a bare
// unqualified VERIFY-PASS — while a failing run names the same host on
// VERIFY-FAIL. The unprobed path (globals unset) must keep the pre-B14
// contract exactly: bare markers, exit 0/1 by FAIL count.
func TestBatterySummaryVerdictCarriesHost(t *testing.T) {
	snippet := batterySummarySnippet(t, readBatteryScript(t))
	for _, tc := range []struct {
		name        string
		pass, fail  int
		note        int
		hostEnv     map[string]string
		wantCode    int
		wantMarkers []string
		denyMarkers []string
	}{
		{
			name: "clean run with a proven host names that host on VERIFY-PASS",
			pass: 107, fail: 0, note: 0,
			hostEnv:  map[string]string{"BATTERY_DAEMON_HOSTNAME": "bunker-mvp", "BATTERY_HOST_STATUS": "ok", "BATTERY_HOST_SUFFIX": " (host: bunker-mvp)"},
			wantCode: 0,
			wantMarkers: []string{
				"VERIFY-PASS (host: bunker-mvp)",
				"CERTIFIED HOST: bunker-mvp (ok)",
			},
			denyMarkers: []string{"VERIFY-FAIL"},
		},
		{
			name: "mismatch suffix still names the certified host on the verdict",
			pass: 107, fail: 0, note: 1,
			hostEnv:  map[string]string{"BATTERY_DAEMON_HOSTNAME": "bunker-mvp", "BATTERY_HOST_STATUS": "mismatch", "BATTERY_HOST_SUFFIX": " (host: bunker-mvp — EXPECTED bunker-las-03)"},
			wantCode: 0,
			wantMarkers: []string{
				"VERIFY-PASS (host: bunker-mvp — EXPECTED bunker-las-03)",
				"CERTIFIED HOST: bunker-mvp (mismatch)",
			},
			denyMarkers: []string{"VERIFY-FAIL"},
		},
		{
			name: "unknown host degrades loudly, never to an unqualified pass",
			pass: 107, fail: 0, note: 1,
			hostEnv:  map[string]string{"BATTERY_DAEMON_HOSTNAME": "", "BATTERY_HOST_STATUS": "unknown", "BATTERY_HOST_SUFFIX": " (host UNKNOWN — not host-proven)"},
			wantCode: 0,
			wantMarkers: []string{
				"VERIFY-PASS (host UNKNOWN — not host-proven)",
				"CERTIFIED HOST: UNKNOWN (unknown)",
			},
			denyMarkers: []string{"VERIFY-FAIL"},
		},
		{
			name: "failing run names the host on VERIFY-FAIL too",
			pass: 106, fail: 1, note: 0,
			hostEnv:  map[string]string{"BATTERY_DAEMON_HOSTNAME": "bunker-mvp", "BATTERY_HOST_STATUS": "ok", "BATTERY_HOST_SUFFIX": " (host: bunker-mvp)"},
			wantCode: 1,
			wantMarkers: []string{
				"VERIFY-FAIL (host: bunker-mvp)",
				"CERTIFIED HOST: bunker-mvp (ok)",
			},
			denyMarkers: []string{"VERIFY-PASS"},
		},
		{
			name: "pre-B14 shape: unset host globals keep the bare legacy marker",
			pass: 107, fail: 0, note: 0,
			hostEnv:     nil,
			wantCode:    0,
			wantMarkers: []string{"VERIFY-PASS\n", "CERTIFIED HOST: UNKNOWN"},
			denyMarkers: []string{"VERIFY-PASS (host"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := runBatterySummaryHostEnv(t, snippet, tc.pass, tc.fail, tc.note, tc.hostEnv)
			if code != tc.wantCode {
				t.Errorf("summary exit code = %d, want %d\n%s", code, tc.wantCode, out)
			}
			for _, marker := range tc.wantMarkers {
				if !strings.Contains(out, marker) {
					t.Errorf("summary output is missing %q:\n%s", marker, out)
				}
			}
			for _, marker := range tc.denyMarkers {
				if strings.Contains(out, marker) {
					t.Errorf("summary output contains %q, which must never appear here:\n%s", marker, out)
				}
			}
		})
	}
}

// TestBatteryVerdictLinesCarryHostSuffix is the static half on the battery
// itself: the verdict lines must interpolate the host suffix, and the connect
// path must record the daemon-reported hostname after the session-target
// binding — a battery that drops either half could print a host-proven
// verdict without ever having asked the daemon which host it runs on.
func TestBatteryVerdictLinesCarryHostSuffix(t *testing.T) {
	script := readBatteryScript(t)

	for _, want := range []string{
		`echo "  VERIFY-PASS${BATTERY_HOST_SUFFIX:-}"`,
		`echo "  VERIFY-FAIL${BATTERY_HOST_SUFFIX:-}"`,
		`CERTIFIED HOST: ${BATTERY_DAEMON_HOSTNAME:-UNKNOWN}`,
		"battery_host_connect_probe",                  // the probe exists
		`bcli status --server "$BATTERY_TARGET_NAME"`, // via the daemon-reported status surface
		"battery_host_connect_probe\n",                // AND is invoked on the main path
		"BATTERY_EXPECTED_HOST",                       // expected-host pin support
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the battery no longer contains %q — VERIFY-PASS can be claimed without naming the certified host (QA-BUNKER-B14 regression)", want)
		}
	}

	// The probe must run AFTER the session-target export (it addresses the
	// daemon through the battery-owned --server name) and BEFORE the summary.
	// Line-number ordering keeps this independent of comment churn.
	lines := strings.Split(script, "\n")
	pos := func(needle string) int {
		for i, line := range lines {
			if strings.Contains(line, needle) {
				return i
			}
		}
		return -1
	}
	exportLine := pos(`export BUNKER_SESSION_TARGET="$BATTERY_TARGET_NAME"`)
	// The main-path INVOCATION is a bare line (`battery_host_connect_probe`);
	// the definition carries `() {` and is matched only by the looser
	// contains-loop above.
	probeLine := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "battery_host_connect_probe" {
			probeLine = i
			break
		}
	}
	if exportLine < 0 || probeLine < 0 {
		t.Fatalf("anchor lines not found: export=%d probe=%d", exportLine, probeLine)
	}
	if probeLine < exportLine {
		t.Errorf("battery_host_connect_probe runs at line %d, BEFORE the session-target export at line %d — the probe would address the daemon without the battery-owned binding", probeLine+1, exportLine+1)
	}
	summaryLine := pos(`VERIFY-PASS${BATTERY_HOST_SUFFIX:-}`)
	if summaryLine < 0 {
		t.Fatalf("summary verdict line not found")
	}
	if probeLine > summaryLine {
		t.Errorf("battery_host_connect_probe runs at line %d, AFTER the summary verdict at line %d — the verdict would print before any host was recorded", probeLine+1, summaryLine+1)
	}
}

// TestCIBatteryPinsExpectedHost closes the 'CI closure' half of QA-BUNKER-B14:
// the E2E battery step in ci.yml must pin BATTERY_EXPECTED_HOST, so every CI
// VERIFY verdict names which host it certified and a claim quoted from the run
// cannot silently read as fleet-wide.
func TestCIBatteryPinsExpectedHost(t *testing.T) {
	workflow := readRepoFile(t, ciWorkflowPath)
	step := workflowStepBlock(t, workflow, e2eBatteryStepName)
	if !strings.Contains(step, "BATTERY_EXPECTED_HOST:") {
		t.Errorf("the E2E battery CI step no longer pins BATTERY_EXPECTED_HOST — a 'CI closure' / VERIFY-PASS claim from this workflow does not state which host it certified (QA-BUNKER-B14):\n%s", step)
	}
	if !strings.Contains(step, "runs bash e2e-full-battery.sh") && !strings.Contains(step, "bash e2e-full-battery.sh") {
		t.Errorf("the pinned step block no longer runs the E2E battery — the guard is stale:\n%s", step)
	}
}
