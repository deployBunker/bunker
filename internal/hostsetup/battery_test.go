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
