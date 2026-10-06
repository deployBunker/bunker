package hostsetup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The CI-simulation ("ci-pass") QA cell is recorded by the GENERATED remote
// script — a `cat <<EOF` heredoc inside .hermes/scripts/bunker-qa.sh's
// build_remote_script(). The cell decides its own status, so a wrong branch
// there is a false green that no other gate can see. QA-BUNKER-B1 is exactly
// that: act (the local GH-runner simulation) exited rc=1 —
// `level=fatal msg=EOF` — while the cell still recorded OK, because the native
// suite happened to pass. The cell's own evidence therefore contradicted its
// status.
//
// The fix pinned here: act is the cell's PRIMARY check, so a non-zero act rc
// must grade FAIL and must never be laundered into OK by the native leg. These
// tests extract the real block out of the harness (marker-delimited, so a
// rename/move fails loudly instead of silently), reverse the one level of
// heredoc escaping, and EXECUTE it against a stubbed `cell` recorder with a
// stubbed act exit code — the same shape the harness runs on an agent.
const (
	qaHarnessPath = "../../.hermes/scripts/bunker-qa.sh"
	qaCIPassStart = "#QA-CELL:ci-pass:start"
	qaCIPassEnd   = "#QA-CELL:ci-pass:end"
)

// qaCellRecord is one captured `cell <name> <status> <detail>` call.
type qaCellRecord struct {
	cell   string
	status string
	detail string
}

// qaCIPassBlock returns the ci-pass cell block from the harness generator with
// the heredoc escape level removed (\$ -> $, \` -> `), i.e. the text the
// generated remote script actually executes.
func qaCIPassBlock(t *testing.T) string {
	t.Helper()
	script := readRepoFile(t, qaHarnessPath)
	start := strings.Index(script, qaCIPassStart)
	end := strings.Index(script, qaCIPassEnd)
	if start < 0 || end < 0 || end < start {
		t.Fatalf("%s: ci-pass cell markers (%q .. %q) not found — the block moved, so this guard is stale",
			qaHarnessPath, qaCIPassStart, qaCIPassEnd)
	}
	block := script[start+len(qaCIPassStart) : end]
	block = strings.ReplaceAll(block, "\\`", "`")
	block = strings.ReplaceAll(block, `\$`, `$`)
	if strings.Contains(block, `\$`) {
		t.Fatalf("%s: ci-pass block still carries a heredoc-escaped $ — the extraction is stale", qaHarnessPath)
	}
	return block
}

func qaBoolBit(v bool) int {
	if v {
		return 0
	}
	return 1
}

// runQACIPassCell executes the real ci-pass block with the given act rc
// ("0", "1" or the literal "skip"), native command (a shell function name) and
// stub predicate results, and returns the cells it recorded in order.
func runQACIPassCell(t *testing.T, block, ciRC, nativeCmd string, vacuous, runnerMissing bool) []qaCellRecord {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}

	dir := t.TempDir()
	evidence := filepath.Join(dir, "cells.tsv")
	logd := filepath.Join(dir, "logs")
	ciLog := filepath.Join(logd, "ci.log")

	driver := fmt.Sprintf(`EVID='%s'
LOGD='%s'
CELLS='%s'
mkdir -p "$LOGD"
: > "$CELLS"
: > "$LOGD/ci.log"
printf 'Job succeeded\n' > "%s"
QA_ACT_NOTE=" (no triggerable workflow)"
ACT_WF_COUNT=1
ci_rc=%s
native_ok()   { printf 'ok  \tgithub.com/deployBunker/bunker/internal/x\t0.001s\n'; }
native_fail() { printf -- '--- FAIL: TestX\nFAIL\n'; return 1; }
native_cmd=%s
vacuous_native_suite() { return %d; }
native_runner_missing() { return %d; }
native_runner_missing_cause() { printf 'pytest: command not found\n'; }
build_env_failure() { return 1; }
go_suite_env_failure() { return 1; }
ci_only_failure() { return 1; }
act_failure_context() { printf 'stub-act-context'; }
cell() { printf '%%s\t%%s\t%%s\n' "$1" "$2" "$3" >> "$CELLS"; }
%s
`, evidence, logd, evidence, ciLog, ciRC, nativeCmd, qaBoolBit(vacuous), qaBoolBit(runnerMissing), block)

	path := filepath.Join(dir, "ci-pass-cell.sh")
	if err := os.WriteFile(path, []byte(driver), 0o700); err != nil {
		t.Fatalf("write ci-pass driver: %v", err)
	}
	out, err := exec.Command("bash", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ci-pass cell driver failed (ci_rc=%s native_cmd=%s): %v\n%s", ciRC, nativeCmd, err, out)
	}

	data, err := os.ReadFile(evidence)
	if err != nil {
		t.Fatalf("read recorded cells: %v\n%s", err, out)
	}
	var records []qaCellRecord
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			t.Fatalf("malformed cell record %q", line)
		}
		records = append(records, qaCellRecord{cell: parts[0], status: parts[1], detail: parts[2]})
	}
	return records
}

// TestQACIPassCellFailsWhenActFails is the regression guard for QA-BUNKER-B1:
// act exiting non-zero must grade the ci-pass cell FAIL, and an act that never
// ran (skip) or exited 0 must keep the honest OK. The native leg's own result
// must never flip the act verdict.
func TestQACIPassCellFailsWhenActFails(t *testing.T) {
	block := qaCIPassBlock(t)

	// Drift guard: the false-green wording must not come back, and the act
	// verdict must be carried in the FAIL detail.
	if strings.Contains(block, `cell ci-pass OK "native suite PASS (see ci-act`) {
		t.Error(`the ci-pass block grades OK "native suite PASS (see ci-act ...)" again — act's rc is laundered into OK (QA-BUNKER-B1)`)
	}
	if !strings.Contains(block, `cell ci-pass FAIL "act rc=`) {
		t.Error(`the ci-pass block no longer records FAIL for a non-zero act rc — expected cell ci-pass FAIL "act rc=$ci_rc: ..."`)
	}

	for _, tc := range []struct {
		name          string
		ciRC          string
		nativeCmd     string
		vacuous       bool
		runnerMissing bool
		want          []qaCellRecord
	}{
		{
			name:      "act rc=0 grades OK",
			ciRC:      "0",
			nativeCmd: "native_ok",
			want: []qaCellRecord{
				{cell: "ci-pass", status: "OK"},
			},
		},
		{
			name:      "act rc=1 with a passing native suite grades FAIL (QA-BUNKER-B1)",
			ciRC:      "1",
			nativeCmd: "native_ok",
			want: []qaCellRecord{
				{cell: "ci-pass", status: "FAIL", detail: "stub-act-context"},
				{cell: "ci-act", status: "UNVERIFIED"},
			},
		},
		{
			name:      "act rc=1 with a failing native suite grades FAIL",
			ciRC:      "1",
			nativeCmd: "native_fail",
			want: []qaCellRecord{
				{cell: "ci-pass", status: "FAIL"},
			},
		},
		{
			name:      "act skipped (no triggerable workflow) keeps the native-suite OK",
			ciRC:      "skip",
			nativeCmd: "native_ok",
			want: []qaCellRecord{
				{cell: "ci-pass", status: "OK"},
			},
		},
		{
			name:      "act rc=1 with a vacuous native suite stays UNVERIFIED",
			ciRC:      "1",
			nativeCmd: "native_ok",
			vacuous:   true,
			want: []qaCellRecord{
				{cell: "ci-pass", status: "UNVERIFIED"},
			},
		},
		{
			name:          "act rc=1 with the native runner missing stays UNVERIFIED",
			ciRC:          "1",
			nativeCmd:     "native_fail",
			runnerMissing: true,
			want: []qaCellRecord{
				{cell: "ci-pass", status: "UNVERIFIED"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runQACIPassCell(t, block, tc.ciRC, tc.nativeCmd, tc.vacuous, tc.runnerMissing)
			if len(got) != len(tc.want) {
				t.Fatalf("recorded %d cell(s) %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i, want := range tc.want {
				if got[i].cell != want.cell {
					t.Errorf("cell[%d] = %q, want %q", i, got[i].cell, want.cell)
				}
				if got[i].status != want.status {
					t.Errorf("cell[%d] (%s) status = %q, want %q (detail: %s)", i, got[i].cell, got[i].status, want.status, got[i].detail)
				}
				if want.detail != "" && !strings.Contains(got[i].detail, want.detail) {
					t.Errorf("cell[%d] (%s) detail %q does not carry %q", i, got[i].cell, got[i].detail, want.detail)
				}
				// The whole point of QA-BUNKER-B1: never OK on a failed act.
				if tc.ciRC != "0" && tc.ciRC != "skip" && got[i].cell == "ci-pass" && got[i].status == "OK" {
					t.Errorf("ci-pass graded OK while act exited rc=%s — the act verdict was laundered into a pass (detail: %s)", tc.ciRC, got[i].detail)
				}
			}
		})
	}
}
