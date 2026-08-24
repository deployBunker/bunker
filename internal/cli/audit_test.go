package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/audit"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// newVerifyLog creates an audit log with n chained records in a temp dir.
func newVerifyLog(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := audit.New(path)
	if err != nil {
		t.Fatalf("audit.New(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	for i := 1; i <= n; i++ {
		if err := l.Log(audit.Record{TS: "t", Caller: "master", Method: "/m", Outcome: "ok", Summary: "rec-" + string(rune('0'+i))}); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}
	return path
}

// newAuditQueryLog creates an audit log with n records carrying real
// RFC3339Nano timestamps (base + i seconds), alternating agents a0/a1.
func newAuditQueryLog(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := audit.New(path)
	if err != nil {
		t.Fatalf("audit.New(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		rec := audit.Record{
			TS:      base.Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339Nano),
			Caller:  "master",
			Method:  "/bunker.v1.Bunkerd/SpawnAgent",
			AgentID: "a" + string(rune('0'+i%2)),
			Outcome: "ok",
			Summary: "rec-" + string(rune('0'+i)),
		}
		if err := l.Log(rec); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}
	return path
}

// auditCmd runs the audit group with args and returns stdout + error.
func auditCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewAuditCommand()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err != nil && errBuf.Len() > 0 {
		t.Logf("stderr: %s", errBuf.String())
	}
	return out.String(), err
}

func TestAuditCommandHelpListsSubcommands(t *testing.T) {
	out, err := auditCmd(t, "--help")
	if err != nil {
		t.Fatalf("audit --help: %v", err)
	}
	for _, sub := range []string{"list", "export", "verify"} {
		if !strings.Contains(out, sub) {
			t.Errorf("audit --help output missing %q subcommand:\n%s", sub, out)
		}
	}
}

func TestAuditListCommand_AgentFilter(t *testing.T) {
	path := newAuditQueryLog(t, 4) // agents: a0 a1 a0 a1

	out, err := auditCmd(t, "list", "--agent", "a1", "--path", path)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	if strings.Contains(out, "Total: 2 records") == false {
		t.Errorf("stdout = %q, want 2 records (only a1)", out)
	}
	if strings.Contains(out, "a0") {
		t.Errorf("stdout contains a0 records that must be filtered out:\n%s", out)
	}
}

func TestAuditListCommand_MethodAndTimeFilters(t *testing.T) {
	path := newAuditQueryLog(t, 4)

	// Method substring filter.
	out, err := auditCmd(t, "list", "--method", "SpawnAgent", "--path", path)
	if err != nil {
		t.Fatalf("audit list --method: %v", err)
	}
	if !strings.Contains(out, "Total: 4 records") {
		t.Errorf("method filter stdout = %q, want 4 records", out)
	}

	// since/until: records 0..3 span base..base+3s; ask for base+1..base+2.
	out, err = auditCmd(t, "list",
		"--since", "2026-08-20T12:00:01Z",
		"--until", "2026-08-20T12:00:02Z",
		"--path", path)
	if err != nil {
		t.Fatalf("audit list --since/--until: %v", err)
	}
	if !strings.Contains(out, "Total: 2 records") {
		t.Errorf("since/until stdout = %q, want 2 records", out)
	}
}

func TestAuditListCommand_InvalidSince(t *testing.T) {
	path := newAuditQueryLog(t, 2)
	if _, err := auditCmd(t, "list", "--since", "not-a-time", "--path", path); err == nil {
		t.Fatal("audit list --since with garbage timestamp returned nil error")
	}
}

func TestAuditExportCommand_ValidJSONL(t *testing.T) {
	path := newAuditQueryLog(t, 3)

	out, err := auditCmd(t, "export", "--path", path)
	if err != nil {
		t.Fatalf("audit export: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("export produced %d lines, want 3", len(lines))
	}
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not valid JSON (%v): %q", i+1, err, line)
		}
		for _, key := range []string{"ts", "caller", "method", "remote_addr", "agent_id", "duration_ms", "outcome", "summary", "hash", "prev_hash"} {
			if _, ok := rec[key]; !ok {
				t.Errorf("line %d missing key %q: %v", i+1, key, rec)
			}
		}
		if rec["hash"] == "" {
			t.Errorf("line %d hash is empty — export not lossless", i+1)
		}
	}
}

func TestAuditExportCommand_Filters(t *testing.T) {
	path := newAuditQueryLog(t, 4) // agents: a0 a1 a0 a1

	out, err := auditCmd(t, "export", "--agent", "a0", "--path", path)
	if err != nil {
		t.Fatalf("audit export: %v", err)
	}
	if got := strings.Count(strings.TrimRight(out, "\n"), "\n") + 1; out != "" && got != 2 {
		t.Fatalf("export --agent a0 produced %d lines, want 2", got)
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line not valid JSON: %v", err)
		}
		if rec["agent_id"] != "a0" {
			t.Errorf("exported record agent_id = %v, want a0", rec["agent_id"])
		}
	}
}

func TestAuditListCommand_Remote(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockBunkerdServer{
		info: &v1.ServerInfoResponse{Hostname: "bunker-audit-test", Version: "v0.2.0"},
		auditRecords: []*v1.AuditRecord{
			{Ts: "2026-08-20T12:00:00Z", Caller: "master", Method: "/bunker.v1.Bunkerd/SpawnAgent", AgentId: "abc123", Outcome: "ok", Summary: "SpawnAgent agent_id=abc123", Hash: "h1", PrevHash: ""},
			{Ts: "2026-08-20T12:00:01Z", Caller: "master", Method: "/bunker.v1.Bunkerd/DestroyAgent", AgentId: "abc123", Outcome: "ok", Summary: "DestroyAgent agent_id=abc123", Hash: "h2", PrevHash: "h1"},
		},
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	// Register the mock daemon as a server entry in the temp HOME config.
	connectCmd := NewConnectCommand()
	connectCmd.SetArgs([]string{"--name", "audit-test", srv.URL})
	if err := connectCmd.Execute(); err != nil {
		t.Fatalf("bunker connect: %v", err)
	}

	// Remote list: the CLI must resolve the server, set the auth header,
	// call the QueryAudit RPC, and render the returned records.
	out, err := auditCmd(t, "list", "--server", "audit-test", "--agent", "abc123")
	if err != nil {
		t.Fatalf("audit list --server: %v", err)
	}
	if !strings.Contains(out, "SpawnAgent agent_id=abc123") {
		t.Errorf("remote list stdout missing SpawnAgent record:\n%s", out)
	}
	if !strings.Contains(out, "DestroyAgent agent_id=abc123") {
		t.Errorf("remote list stdout missing DestroyAgent record:\n%s", out)
	}
	if !strings.Contains(out, "Total: 2 records") {
		t.Errorf("remote list stdout missing total:\n%s", out)
	}
}

func TestAuditExportCommand_Remote(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockBunkerdServer{
		info: &v1.ServerInfoResponse{Hostname: "bunker-audit-test", Version: "v0.2.0"},
		auditRecords: []*v1.AuditRecord{
			{Ts: "2026-08-20T12:00:00Z", Caller: "master", Method: "/bunker.v1.Bunkerd/SpawnAgent", AgentId: "abc123", Outcome: "ok", Summary: "SpawnAgent agent_id=abc123", Hash: "h1", PrevHash: ""},
		},
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	connectCmd := NewConnectCommand()
	connectCmd.SetArgs([]string{"--name", "audit-test", srv.URL})
	if err := connectCmd.Execute(); err != nil {
		t.Fatalf("bunker connect: %v", err)
	}

	out, err := auditCmd(t, "export", "--server", "audit-test")
	if err != nil {
		t.Fatalf("audit export --server: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rec); err != nil {
		t.Fatalf("remote export line not valid JSON (%v): %q", err, out)
	}
	if rec["hash"] != "h1" || rec["agent_id"] != "abc123" {
		t.Errorf("remote export record = %v, want hash h1 / agent abc123", rec)
	}
}

func TestAuditVerifyCommandOK(t *testing.T) {
	path := newVerifyLog(t, 3)

	cmd := NewAuditCommand()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"verify", "--path", path})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("verify on untouched log returned error: %v (stderr: %s)", err, errBuf.String())
	}
	if !strings.Contains(out.String(), "OK (3 records)") {
		t.Errorf("stdout = %q, want 'audit log <path>: OK (3 records)'", out.String())
	}
}

func TestAuditVerifyCommandTampered(t *testing.T) {
	path := newVerifyLog(t, 4)

	// Edit record 3 in place (valid JSON, different bytes).
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	lines[2] = strings.Replace(lines[2], `"rec-3"`, `"rec-3-edited"`, 1)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write tampered log: %v", err)
	}

	cmd := NewAuditCommand()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"verify", "--path", path})
	if err := cmd.Execute(); err == nil {
		t.Fatal("verify on tampered log returned nil error")
	}
	if !strings.Contains(errBuf.String(), "tamper detected at record 3") {
		t.Errorf("stderr = %q, want 'tamper detected at record 3'", errBuf.String())
	}
	if strings.Contains(out.String(), "OK (") {
		t.Errorf("stdout = %q, must not report OK on a tampered log", out.String())
	}
}

func TestAuditVerifyCommandMissingFile(t *testing.T) {
	cmd := NewAuditCommand()
	cmd.SetArgs([]string{"verify", "--path", filepath.Join(t.TempDir(), "missing.log")})
	if err := cmd.Execute(); err == nil {
		t.Fatal("verify on missing file returned nil error")
	}
}
