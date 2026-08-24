package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/audit"
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
