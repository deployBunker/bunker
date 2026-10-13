package cli

// GAP-136 regression tests at the CLI surface: `bunker audit status` reports
// chain integrity and anchor state honestly (SEC-16), and `bunker audit verify`
// fails on a truncated chain that used to pass.
//
// These live in their own file so the long-standing audit_test.go keeps its
// shape; the helpers they need (statusLine, lastHashOf, truncateLines) are here
// too.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/audit"
)

// statusLine renders an expected status line the way printAuditStatus does
// (label column 22 wide), so assertions pin the operator-visible format rather
// than matching substrings loosely.
func statusLine(label, value string) string {
	return fmt.Sprintf("%-21s %s", label, value)
}

// lastHashOf returns the hash field of the last parseable record in path.
func lastHashOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var rec audit.Record
		if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
			continue
		}
		if rec.Hash != "" {
			return rec.Hash
		}
	}
	t.Fatalf("%s has no hashed record", path)
	return ""
}

// truncateLines rewrites path with only its first n lines — the tail removal a
// truncated chain (or a rolled-back file) presents.
func truncateLines(t *testing.T, path string, n int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if n > len(lines) {
		t.Fatalf("truncateLines(%d): %s has only %d lines", n, path, len(lines))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines[:n], "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("rewrite %s: %v", path, err)
	}
}

// TestAuditStatusCommandTruncatedChainIsHonest is ACCEPTANCE CRITERION 3 at the
// CLI surface: a truncated chain must be reported honestly, with the gap named,
// and must NOT be reported as healthy. Before GAP-136 this exact tree printed
// "last verify: n/a" and nothing else about integrity.
func TestAuditStatusCommandTruncatedChainIsHonest(t *testing.T) {
	path := newVerifyLog(t, 5)
	truncateLines(t, path, 2) // records 3..5 removed

	out, err := auditCmd(t, "status", "--path", path)
	if err != nil {
		// Status is a report, not a gate: it must still exit 0 with a broken
		// chain so monitoring can read the honest verdict.
		t.Fatalf("audit status must report a broken chain, not fail: %v", err)
	}
	for _, want := range []string{
		statusLine("chain status:", "BROKEN — "+path+".anchor: chain truncated:"),
		statusLine("health:", "broken — chain integrity failure:"),
		statusLine("gaps:", path+".anchor: chain truncated:"),
		statusLine("chain length:", "2 records (retained chain)"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, statusLine("health:", "healthy")) {
		t.Errorf("status claimed healthy for a truncated chain:\n%s", out)
	}
	if strings.Contains(out, statusLine("chain status:", "OK")) {
		t.Errorf("status claimed the chain verified when it is truncated:\n%s", out)
	}
	if strings.Contains(out, statusLine("gaps:", "none")) {
		t.Errorf("status reported no gaps for a truncated chain:\n%s", out)
	}
}

// TestAuditStatusCommandSaysNoOffHostAnchor is the SEC-16 line at the CLI
// surface: with a verified but only locally anchored chain the report says, in
// those words, that no off-host anchor is configured — and calls it degraded
// rather than healthy.
func TestAuditStatusCommandSaysNoOffHostAnchor(t *testing.T) {
	path := newVerifyLog(t, 3)
	out, err := auditCmd(t, "status", "--path", path)
	if err != nil {
		t.Fatalf("audit status: %v", err)
	}
	if !strings.Contains(out, statusLine("off-host anchor:", "no off-host anchor configured")) {
		t.Errorf("status output missing the literal 'no off-host anchor configured':\n%s", out)
	}
	if !strings.Contains(out, statusLine("health:", "degraded — chain verified and locally anchored, but no off-host anchor configured")) {
		t.Errorf("status output does not report degraded health honestly:\n%s", out)
	}
	if strings.Contains(out, statusLine("health:", "healthy")) {
		t.Errorf("status claimed healthy with no off-host anchor:\n%s", out)
	}
	if !strings.Contains(out, statusLine("local anchor:", "keyed — MAC validated,")) {
		t.Errorf("status output does not report the keyed anchor:\n%s", out)
	}
}

// TestAuditStatusCommandJSONIsHonestAndMachineReadable: the fields a monitoring
// script needs are in --json, including the anchor object.
func TestAuditStatusCommandJSONIsHonestAndMachineReadable(t *testing.T) {
	path := newVerifyLog(t, 3)
	out, err := auditCmd(t, "status", "--path", path, "--json")
	if err != nil {
		t.Fatalf("audit status --json: %v", err)
	}
	var st struct {
		Enabled        bool
		ChainOK        bool
		ChainLength    int
		LastAnchorHash string
		StoragePath    string
		Health         string
		HealthReason   string
		Gaps           []string
		Anchor         struct {
			Present   bool
			State     string
			Head      string
			KeyID     string
			MACValid  bool
			OffHost   bool
			Statement string
		}
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status --json is not valid JSON (%v):\n%s", err, out)
	}
	if !st.Enabled || !st.ChainOK || st.ChainLength != 3 {
		t.Errorf("json = enabled=%v chain_ok=%v chain_length=%d, want true/true/3", st.Enabled, st.ChainOK, st.ChainLength)
	}
	if st.StoragePath != path {
		t.Errorf("StoragePath = %q, want %q", st.StoragePath, path)
	}
	if st.LastAnchorHash != lastHashOf(t, path) {
		t.Errorf("LastAnchorHash = %q, want the live tail", st.LastAnchorHash)
	}
	if st.Health != "degraded" || st.HealthReason == "" {
		t.Errorf("health = %q (%q), want degraded with a reason", st.Health, st.HealthReason)
	}
	if len(st.Gaps) != 0 {
		t.Errorf("Gaps = %v, want none", st.Gaps)
	}
	if !st.Anchor.Present || st.Anchor.State != "keyed" || !st.Anchor.MACValid {
		t.Errorf("anchor = %+v, want present/keyed/MACValid", st.Anchor)
	}
	if st.Anchor.Statement != "no off-host anchor configured" {
		t.Errorf("anchor statement = %q, want the literal 'no off-host anchor configured'", st.Anchor.Statement)
	}
}

// TestAuditStatusCommandJSONBrokenChainIsHonest: the machine-readable verdict
// says broken too.
func TestAuditStatusCommandJSONBrokenChainIsHonest(t *testing.T) {
	path := newVerifyLog(t, 4)
	truncateLines(t, path, 1)
	out, err := auditCmd(t, "status", "--path", path, "--json")
	if err != nil {
		t.Fatalf("audit status --json: %v", err)
	}
	var st struct {
		ChainOK bool
		Health  string
		Gaps    []string
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status --json is not valid JSON (%v):\n%s", err, out)
	}
	if st.ChainOK || st.Health != "broken" || len(st.Gaps) == 0 {
		t.Errorf("json = chain_ok=%v health=%q gaps=%v, want false/broken/[…]", st.ChainOK, st.Health, st.Gaps)
	}
}

// TestAuditVerifyCommandDetectsTruncation: `bunker audit verify` fails on the
// truncated chain that used to pass.
func TestAuditVerifyCommandDetectsTruncation(t *testing.T) {
	path := newVerifyLog(t, 5)
	truncateLines(t, path, 2)

	cmd := NewAuditCommand()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"verify", "--path", path})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("verify accepted a truncated chain (stdout: %s)", out.String())
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("verify error = %q, want it to name the truncation", err.Error())
	}
	if strings.Contains(out.String(), "OK (") {
		t.Errorf("stdout = %q, must not report OK on a truncated chain", out.String())
	}
}

// TestAuditVerifyCommandReportsAnchorState: a clean run says what the anchor
// proved, so an operator can tell a keyed anchor from an unanchored chain.
func TestAuditVerifyCommandReportsAnchorState(t *testing.T) {
	path := newVerifyLog(t, 3)
	out, err := auditCmd(t, "verify", "--path", path)
	if err != nil {
		t.Fatalf("verify on a healthy anchored log: %v", err)
	}
	if !strings.Contains(out, "OK (3 records)") {
		t.Errorf("stdout = %q, want the chain verdict", out)
	}
	if !strings.Contains(out, "keyed, MAC validated") {
		t.Errorf("stdout = %q, want the anchor verdict (keyed, MAC validated)", out)
	}

	// Remove the anchor: the chain still verifies, but the anchor line must
	// report an UNANCHORED chain rather than staying silent about it.
	if err := os.Remove(path + ".anchor"); err != nil {
		t.Fatalf("remove anchor: %v", err)
	}
	out, err = auditCmd(t, "verify", "--path", path)
	if err != nil {
		t.Fatalf("verify with no anchor file: %v", err)
	}
	if !strings.Contains(out, "NONE") || !strings.Contains(out, "unanchored") {
		t.Errorf("stdout = %q, want it to report the chain as unanchored", out)
	}
}

// TestAuditDeploymentFactsPathGuard: the daemon config's seal_key/ship_to are
// applied ONLY to the log the config describes. Applying them to a different
// tree would make the report claim an anchor that belongs to someone else —
// exactly the confident-but-false status SEC-16 was filed about.
func TestAuditDeploymentFactsPathGuard(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.log")
	cfgPath := filepath.Join(dir, "bunkerd.yaml")
	cfg := "audit:\n" +
		"  path: \"" + logPath + "\"\n" +
		"  ship_to: \"syslog://anchor.example.net:601\"\n" +
		"  seal_key: \"gap136-config-key-not-a-secret\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write daemon config: %v", err)
	}
	SetDaemonConfigPathOverride(cfgPath)
	t.Cleanup(ResetDaemonConfigPathOverride)

	if key, ship := AuditDeploymentFacts(logPath); key != "gap136-config-key-not-a-secret" || ship != "syslog://anchor.example.net:601" {
		t.Errorf("facts for the configured log = (%q, %q), want the configured key and ship_to", key, ship)
	}
	if key, ship := AuditDeploymentFacts(filepath.Join(dir, "other.log")); key != "" || ship != "" {
		t.Errorf("facts for a DIFFERENT log = (%q, %q), want empty/empty (the config does not describe it)", key, ship)
	}
}

// TestAuditStatusCommandOffHostAnchorFromConfig: a configured ship_to with
// nothing shipped yet must still report an off-host anchor (no ship-state file
// exists in that state).
func TestAuditStatusCommandOffHostAnchorFromConfig(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.log")
	l, err := audit.New(logPath)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err := l.Log(audit.Record{TS: "t", Caller: "master", Method: "/m", Outcome: "ok", Summary: "rec-1"}); err != nil {
		t.Fatalf("Log: %v", err)
	}

	cfgPath := filepath.Join(dir, "bunkerd.yaml")
	cfg := "audit:\n  path: \"" + logPath + "\"\n  ship_to: \"https://anchor.example.net/v1\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write daemon config: %v", err)
	}
	SetDaemonConfigPathOverride(cfgPath)
	t.Cleanup(ResetDaemonConfigPathOverride)

	out, err := auditCmd(t, "status", "--path", logPath)
	if err != nil {
		t.Fatalf("audit status: %v", err)
	}
	if !strings.Contains(out, statusLine("off-host anchor:", "off-host anchor configured (https://anchor.example.net/v1), nothing shipped successfully yet")) {
		t.Errorf("status output does not report the configured off-host anchor:\n%s", out)
	}
	if !strings.Contains(out, statusLine("health:", "healthy —")) {
		t.Errorf("status output does not report healthy for a keyed, off-host anchored chain:\n%s", out)
	}
}

// TestAuditStatusCommandHealthyWithOffHostAnchor: the healthy verdict IS
// reachable — verified chain, keyed anchor with a validated MAC, and an
// off-host anchor — and the report shows all three.
func TestAuditStatusCommandHealthyWithOffHostAnchor(t *testing.T) {
	path := newVerifyLog(t, 3)
	st := audit.ShipState{
		ShipTo:      "https://collector.example.net/v1",
		LastAttempt: "2026-09-14T12:00:00Z",
		LastResult:  "ok",
		LastSuccess: "2026-09-14T12:00:00Z",
		QueueDepth:  0,
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal ship state: %v", err)
	}
	if err := os.WriteFile(path+".shipstate", b, 0o600); err != nil {
		t.Fatalf("write ship state: %v", err)
	}

	out, err := auditCmd(t, "status", "--path", path)
	if err != nil {
		t.Fatalf("audit status: %v", err)
	}
	for _, want := range []string{
		statusLine("chain status:", "OK — 3 records verified against the hash chain"),
		statusLine("gaps:", "none"),
		statusLine("local anchor:", "keyed — MAC validated,"),
		statusLine("off-host anchor:", "off-host anchor configured (https://collector.example.net/v1), last shipped 2026-09-14T12:00:00Z"),
		statusLine("health:", "healthy — chain verified, anchor MAC validated, off-host anchor configured"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}
