package audit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/auth"
)

// contextWithTestClaims returns a context carrying static-token claims, the
// same context shape the auth interceptor hands a handler. Used to prove the
// recorder derives the caller from the context and not from a caller-supplied
// string.
func contextWithTestClaims(t *testing.T) context.Context {
	t.Helper()
	return auth.ContextWithClaims(context.Background(), &auth.Claims{})
}

// GAP-142: redaction table. The load-bearing acceptance criterion is that a
// command containing a credential-shaped value NEVER reaches the trail
// unredacted, while an ordinary command passes through unchanged.

func TestRedactCommandSummary_ScrubTable(t *testing.T) {
	cases := []struct {
		name    string
		command string
		args    []string
		// want is the exact expected summary.
		want string
	}{
		{
			name:    "ordinary command passes through unchanged",
			command: "echo",
			args:    []string{"hello", "world"},
			want:    "echo hello world",
		},
		{
			name:    "no args",
			command: "uptime",
			want:    "uptime",
		},
		{
			name:    "shell pipeline with ordinary args",
			command: "sh",
			args:    []string{"-c", "ls -la /tmp | wc -l"},
			// The pipe joins the fields; nothing is credential-shaped.
			want: "sh -c ls -la /tmp | wc -l",
		},
		{
			name:    "credential flag value",
			command: "deploy",
			args:    []string{"--token", "abcdef123456"},
			want:    "deploy --token [REDACTED:len12]",
		},
		{
			name:    "flag=value form",
			command: "deploy",
			args:    []string{"--api-key=s3cr3t-value-here"},
			// 17 characters of value after the flag.
			want: "deploy --api-key=[REDACTED:len17]",
		},
		{
			name:    "quoted Authorization Bearer header",
			command: "curl",
			args:    []string{"-H", `"Authorization: Bearer abc123def456"`, "https://api.example.com"},
			want:    `curl -H Authorization: Bearer [REDACTED:len12] https://api.example.com`,
		},
		{
			name:    "Authorization header with an empty value masks the next argv",
			command: "curl",
			args:    []string{"-H", `"Authorization:"`, "abc123def456", "https://api.example.com"},
			want:    `curl -H Authorization: [REDACTED:len12] https://api.example.com`,
		},
		{
			name:    "env VAR=secret in the command arguments",
			command: "env",
			args:    []string{"API_SECRET=abc123def456", "run-app"},
			want:    "env API_SECRET=[REDACTED:len12] run-app",
		},
		{
			name:    "env var whose value is a bearer header",
			command: "env",
			args:    []string{"MY_FLAG=Authorization:", "Bearer", "abc123def456", "run-app"},
			want:    "env MY_FLAG=Authorization: Bearer [REDACTED:len12] run-app",
		},
		{
			name:    "40-char hex blob behind a credential flag",
			command: "auth",
			args:    []string{"--key", "5f4dcc3b5aa765d61d8327deb882cf99aabbccdd"},
			want:    "auth --key [REDACTED:len40]",
		},
		{
			name:    "bare 40-char hex blob with no flag context",
			command: "register",
			args:    []string{"5f4dcc3b5aa765d61d8327deb882cf99aabbccdd"},
			want:    "register [REDACTED:len40]",
		},
		{
			name:    "base64 blob",
			command: "login",
			args:    []string{"dGhpcy1pc19hLXNlY3JldC10b2tlbg=="},
			want:    "login [REDACTED:len32]",
		},
		{
			name:    "user and password flags (credentials in any shape)",
			command: "mysql",
			args:    []string{"-u", "root", "--password=hunter2", "-e", "select 1"},
			// -u is a credential flag, so its value is masked whatever its shape
			// (the login name is not a plausible credential, but a credential
			// flag's value is masked on principle — a masked benign value is the
			// cheap side of the trade).
			want: "mysql -u [REDACTED:len4] --password=[REDACTED:len7] -e select 1",
		},
		{
			name:    "session cookie header",
			command: "curl",
			args:    []string{"-H", `"Cookie: session=abc123def456"`, "https://x.example"},
			// Header key, colon, one space, then the cookie pair.
			want: `curl -H Cookie: [REDACTED:len20] https://x.example`,
		},
		{
			name:    "long ordinary word is not masked",
			command: "echo",
			args:    []string{"internationalization", "extraordinary"},
			want:    "echo internationalization extraordinary",
		},
		{
			name:    "path with separators is not masked",
			command: "ls",
			args:    []string{"/var/log/bunkerd/audit.log.1", "./internal/server/service.go"},
			want:    "ls /var/log/bunkerd/audit.log.1 ./internal/server/service.go",
		},
		{
			name:    "flag name containing 'key' as a substring is not a credential flag",
			command: "tool",
			args:    []string{"--keynote", "theme"},
			want:    "tool --keynote theme",
		},
		{
			name:    "url is not mistaken for a header",
			command: "curl",
			args:    []string{"https://api.example.com/v1/items"},
			want:    "curl https://api.example.com/v1/items",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactCommandSummary(tc.command, tc.args)
			if got != tc.want {
				t.Errorf("RedactCommandSummary(%q, %q) =\n  %q\nwant\n  %q", tc.command, tc.args, got, tc.want)
			}
		})
	}
}

// TestRedactCommandSummary_NeverLeaksCredentials is the acceptance-critical
// property, asserted independently of the exact placeholder text: for every
// credential-bearing command in the table above, no fragment of the credential
// survives in the summary, and every placeholder is shape-preserving.
func TestRedactCommandSummary_NeverLeaksCredentials(t *testing.T) {
	const (
		token        = "abcdef123456"
		hexSecret    = "5f4dcc3b5aa765d61d8327deb882cf99aabbccdd"
		base64Secret = "dGhpcy1pc19hLXNlY3JldC10b2tlbg=="
		ghToken      = "ghp_AAAABBBBCCCCDDDDEEEEFFFF"
		password     = "hunter2"
		jwtSecret    = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	)
	cases := []struct {
		name    string
		command string
		args    []string
		secrets []string
	}{
		{
			name:    "credential flag",
			command: "deploy",
			args:    []string{"--token", token},
			secrets: []string{token},
		},
		{
			name:    "authorization header",
			command: "curl",
			args:    []string{"-H", `"Authorization: Bearer ` + token + `"`},
			secrets: []string{token},
		},
		{
			name:    "env assignment",
			command: "env",
			args:    []string{"API_KEY=" + hexSecret, "run"},
			secrets: []string{hexSecret},
		},
		{
			name:    "hex blob",
			command: "register",
			args:    []string{hexSecret},
			secrets: []string{hexSecret},
		},
		{
			name:    "base64 blob",
			command: "login",
			args:    []string{base64Secret},
			secrets: []string{base64Secret},
		},
		{
			name:    "prefixed token",
			command: "gh",
			args:    []string{"auth", "login", "--with-token", ghToken},
			secrets: []string{ghToken},
		},
		{
			name:    "password assignment",
			command: "mysql",
			args:    []string{"--password=" + password},
			secrets: []string{password},
		},
		{
			name:    "jwt behind an authorization header",
			command: "curl",
			args:    []string{"-H", `"Authorization: Bearer ` + jwtSecret + `"`},
			secrets: []string{jwtSecret},
		},
		{
			name:    "credential flag whose value has no credential shape",
			command: "deploy",
			args:    []string{"--token", token},
			secrets: []string{token},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactCommandSummary(tc.command, tc.args)
			for _, secret := range tc.secrets {
				if strings.Contains(got, secret) {
					t.Errorf("summary %q leaks credential %q", got, secret)
				}
			}
			if !strings.Contains(got, RedactedPrefix) {
				t.Errorf("summary %q carries no redaction placeholder", got)
			}
			// Every placeholder is well formed: [REDACTED:lenN].
			for _, field := range strings.Fields(got) {
				idx := strings.Index(field, RedactedPrefix)
				if idx < 0 {
					continue
				}
				rest := field[idx+len("REDACTED:len"):]
				if !strings.HasSuffix(rest, "]") || len(rest) < 2 {
					t.Errorf("malformed placeholder in %q (field %q)", got, field)
				}
			}
		})
	}
}

func TestRedactScriptSummary(t *testing.T) {
	script := "#!/bin/sh\necho hi\nexport TOKEN=supersecretvalue\n"
	got := RedactScriptSummary(script)

	if strings.Contains(got, "supersecretvalue") {
		t.Fatalf("script summary leaks the script body: %q", got)
	}
	if !strings.HasPrefix(got, "script bytes=") {
		t.Errorf("script summary %q lacks the bytes= prefix", got)
	}
	// Same script -> same summary (correlation must be stable), different
	// script -> different digest.
	if RedactScriptSummary(script) != got {
		t.Errorf("script summary is not stable across calls: %q vs %q", RedactScriptSummary(script), got)
	}
	if RedactScriptSummary(script+"x") == got {
		t.Error("different scripts produced the same summary digest")
	}
}

func TestRedactCommandSummary_Truncation(t *testing.T) {
	// A very long ordinary command is capped, marked, and never exceeds the cap
	// by more than the marker.
	long := strings.Repeat("ordinary-argument ", 100)
	got := RedactCommandSummary("run", []string{long})
	if len(got) > MaxCommandSummaryLen+len(" …(truncated)") {
		t.Errorf("summary length %d exceeds the cap (%d + marker)", len(got), MaxCommandSummaryLen)
	}
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("truncated summary lacks the marker: %q", got[len(got)-40:])
	}
}

// TestRecordExecCommand_WritesCorrelatedRecord covers the recorder itself: one
// record, on the chain, with the procedure sub-kind, the agent id, the redacted
// command and the caller derived from the context claims.
func TestRecordExecCommand_WritesCorrelatedRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	log, err := New(path)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	// Seed one RPC record the way the interceptor would, then the command
	// record for the same request — the pair must share one chain.
	rpc := Record{
		TS:      "2026-09-20T10:00:00.000000001Z",
		Caller:  "master",
		Method:  "/bunker.v1.Bunkerd/ExecAgent",
		AgentID: "agt-1",
		Outcome: "ok",
		Summary: "ExecAgent agent_id=agt-1",
	}
	if err := log.Log(rpc); err != nil {
		t.Fatalf("Log(rpc): %v", err)
	}

	ctx := contextWithTestClaims(t)
	RecordExecCommand(ctx, log, nil, ExecRecord{
		Procedure:  "/bunker.v1.Bunkerd/ExecAgent",
		AgentID:    "agt-1",
		Outcome:    "ok",
		Summary:    RedactCommandSummary("deploy", []string{"--token", "abcdef123456"}),
		DurationMS: 7,
	})

	recs, err := Query(path, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	cmd := recs[1]
	if cmd.Method != rpc.Method+ExecRecordMethod {
		t.Errorf("command record method = %q, want %q", cmd.Method, rpc.Method+ExecRecordMethod)
	}
	if cmd.AgentID != "agt-1" {
		t.Errorf("command record agent_id = %q, want agt-1", cmd.AgentID)
	}
	if cmd.Caller != "master" {
		t.Errorf("command record caller = %q, want master (from context claims)", cmd.Caller)
	}
	if !strings.HasPrefix(cmd.Summary, "deploy --token [REDACTED:len12]") {
		t.Errorf("command record summary = %q, want the redacted command", cmd.Summary)
	}
	if strings.Contains(cmd.Summary, "abcdef123456") {
		t.Errorf("command record leaks the credential: %q", cmd.Summary)
	}
	if cmd.PrevHash != recs[0].Hash {
		t.Errorf("command record does not chain to the RPC record: prev=%q want %q", cmd.PrevHash, recs[0].Hash)
	}
	if _, firstBad, err := Verify(path); err != nil || firstBad != 0 {
		t.Fatalf("chain verify failed after the command record (firstBad=%d): %v", firstBad, err)
	}

	// The raw bytes on disk never carry the credential either.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(raw), "abcdef123456") {
		t.Error("raw audit bytes contain the credential value")
	}
}

// TestRecordExecCommand_DropsUnredactedSummary proves the fail-closed re-check:
// a caller that hands over a raw credential summary gets NO record rather than
// a leaking one.
func TestRecordExecCommand_DropsUnredactedSummary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	log, err := New(path)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	RecordExecCommand(context.Background(), log, nil, ExecRecord{
		Procedure: "/bunker.v1.Bunkerd/ExecAgent",
		AgentID:   "agt-1",
		Summary:   "--token 5f4dcc3b5aa765d61d8327deb882cf99aabbccdd",
	})

	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(raw), "5f4dcc3b5aa765d61d8327deb882cf99aabbccdd") {
		t.Fatalf("unredacted summary reached the log bytes: %s", raw)
	}
	if recs, _ := Query(path, Filter{}); len(recs) != 0 {
		t.Errorf("records = %d, want 0 (the record must be dropped)", len(recs))
	}
}

// TestRecordExecCommand_NilLogIsNoop proves a daemon with auditing disabled
// records nothing and does not panic.
func TestRecordExecCommand_NilLogIsNoop(t *testing.T) {
	RecordExecCommand(context.Background(), nil, nil, ExecRecord{
		Procedure: "/bunker.v1.Bunkerd/ExecAgent",
		AgentID:   "agt-1",
		Summary:   "echo hi",
	})
}

// TestRecordExecCommand_SummaryIsRecheckIdempotent proves an ALREADY-redacted
// summary passes the re-check unchanged (placeholders are not nested).
func TestRecordExecCommand_SummaryIsRecheckIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	log, err := New(path)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	sum := RedactCommandSummary("curl", []string{"-H", `"Authorization: Bearer abc123def456"`})
	RecordExecCommand(context.Background(), log, nil, ExecRecord{
		Procedure: "/bunker.v1.Bunkerd/ExecAgent",
		AgentID:   "agt-1",
		Summary:   sum,
	})
	recs, err := Query(path, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1 (an already-redacted summary must be accepted)", len(recs))
	}
	if recs[0].Summary != sum {
		t.Errorf("summary = %q, want %q (unchanged)", recs[0].Summary, sum)
	}
	if strings.Count(recs[0].Summary, "REDACTED") != 1 {
		t.Errorf("placeholders nested: %q", recs[0].Summary)
	}
}

// TestCommandSummaryAllowed_Shapes pins the write-path re-check. The script
// digest form is accepted verbatim (a hex digest is legitimately hex-shaped, and
// re-scrubbing it would reject the recorder's own output); an arbitrary
// 'script bytes=' line carrying a secret is NOT accepted; and a redacted command
// summary is accepted while a raw one is refused.
func TestCommandSummaryAllowed_Shapes(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name    string
		summary string
		want    bool
	}{
		{name: "real script digest form", summary: "script bytes=42 sha256=" + digest, want: true},
		{name: "script form with a secret instead of a digest", summary: "script bytes=42 sha256=ghp_AAAABBBBCCCCDDDDEEEEFFFF", want: false},
		// A short non-credential value is not a script digest and not credential
		// material, so it passes as any ordinary token does — the property under
		// test is "no credential material survives", not "only the exact digest
		// form is allowed".
		{name: "script form with a truncated digest", summary: "script bytes=42 sha256=0123456789abcdef", want: true},
		{name: "ordinary command", summary: "echo hello world", want: true},
		{name: "already redacted command", summary: "deploy --token [REDACTED:len12]", want: true},
		{name: "raw credential behind a flag", summary: "--token abcdef123456", want: false},
		{name: "raw hex blob", summary: "register 5f4dcc3b5aa765d61d8327deb882cf99aabbccdd", want: false},
		{name: "raw bearer header", summary: `curl -H Authorization: Bearer abc123def456`, want: false},
		{name: "empty", summary: "", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandSummaryAllowed(tc.summary); got != tc.want {
				t.Errorf("commandSummaryAllowed(%q) = %v, want %v", tc.summary, got, tc.want)
			}
		})
	}
}

// TestRedactCommandSummary_ShellPayloadsNeverLeak drives realistic
// agent-generated payloads — a shell one-liner whose credential is inside single
// quotes, a docker run env push, a heredoc-free curl chain — through the
// scrubber and asserts only the leak property (not an exact rendering), because
// that property is what the acceptance criterion demands.
func TestRedactCommandSummary_ShellPayloadsNeverLeak(t *testing.T) {
	const secret = "s3cr3t-token-value-42"
	cases := []struct {
		name    string
		command string
		args    []string
		secrets []string
	}{
		{
			name:    "shell one-liner with a single-quoted authorization header",
			command: "sh",
			args:    []string{"-c", `curl -sS -H 'Authorization: Bearer ` + secret + `' https://api.example.com/v1/items`},
			secrets: []string{secret},
		},
		{
			name:    "docker run pushing an env credential",
			command: "docker",
			args:    []string{"run", "--rm", "-e", "API_TOKEN=" + secret, "img", "serve"},
			secrets: []string{secret},
		},
		{
			name:    "export then use",
			command: "sh",
			args:    []string{"-c", "export GITHUB_TOKEN=" + secret + "; gh pr list"},
			secrets: []string{secret},
		},
		{
			name:    "flag value in the middle of a chain",
			command: "sh",
			args:    []string{"-c", "set -e && deploy --api-key " + secret + " && echo ok"},
			secrets: []string{secret},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactCommandSummary(tc.command, tc.args)
			for _, s := range tc.secrets {
				if strings.Contains(got, s) {
					t.Errorf("summary leaks %q: %q", s, got)
				}
			}
			if !strings.Contains(got, auditRedactedTestMarker) {
				t.Errorf("summary %q carries no redaction placeholder", got)
			}
		})
	}
}

// auditRedactedTestMarker is the placeholder prefix, spelled out so this file
// does not silently follow a constant change.
const auditRedactedTestMarker = "[REDACTED:len"

// TestRecordExecCommand_ConcurrentWritesStayChained proves the recorder is safe
// under the concurrency a daemon actually has: many exec requests finishing at
// once. That is only true because AuditLog.Log serializes appends under its
// mutex AND RecordExecCommand goes through Log rather than the unexported
// logLocked path — a recorder that reached around the mutex would interleave
// chain links here and the verifier below would fail.
func TestRecordExecCommand_ConcurrentWritesStayChained(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	log, err := New(path)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	const writers = 16
	done := make(chan struct{})
	for i := 0; i < writers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			RecordExecCommand(context.Background(), log, nil, ExecRecord{
				Procedure: "/bunker.v1.Bunkerd/ExecAgent",
				AgentID:   "agt-1",
				Summary:   "echo concurrent",
			})
		}()
	}
	for i := 0; i < writers; i++ {
		<-done
	}

	recs, err := Query(path, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(recs) != writers {
		t.Fatalf("records = %d, want %d (every concurrent write must land)", len(recs), writers)
	}
	if _, firstBad, err := Verify(path); err != nil || firstBad != 0 {
		t.Fatalf("concurrent records broke the chain (firstBad=%d): %v", firstBad, err)
	}
	for i, r := range recs[1:] {
		if r.PrevHash != recs[i].Hash {
			t.Fatalf("record %d does not chain to %d: %q vs %q", i+1, i, r.PrevHash, recs[i].Hash)
		}
	}
}
