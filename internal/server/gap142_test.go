package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	"github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"

	"github.com/deployBunker/bunker/internal/audit"
	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// GAP-142 acceptance battery. The forensic question this feature answers is
// "what command did this exec actually run, on which agent, by whom?" — and the
// answer must land in the SAME hash chain as the RPC record, with no credential
// in it. Every test here drives the REAL connect stack (handler + streaming
// interceptor + audit interceptor), so the correlation is proved end to end
// rather than by calling the recorder directly.

const (
	gap142AgentID = "gap142a"
	// gap142Token is the static master token this battery authenticates with, so
	// the command record's caller identity is a real authenticated identity
	// ("master") rather than the auth-disabled placeholder.
	gap142Token = "gap142-master-token"
)

// gap142Server mounts the service exactly as server.go composes it: auth
// interceptor outermost, audit interceptor inside. It returns a client that
// authenticates with gap142Token.
func gap142Server(t *testing.T, log *audit.AuditLog) bunkerv1connect.BunkerdClient {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{
		AgentID:           gap142AgentID,
		Status:            "running",
		SshPrivateKeyPath: imgTestKeyPath,
	}); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	svc := &bunkerdService{
		cfg:      config.DefaultConfig(),
		logger:   logger,
		tracker:  tracker,
		auditLog: log,
		// The RunAgent path calls the manager; this harness supplies the same
		// mock the package's other RunAgent tests use so the detach RPC reaches
		// its success path instead of a nil-pointer panic.
		agentMgr: &runAgentMockManager{runResp: &v1.RunAgentResponse{RunId: "gap142-run", Status: "running"}},
	}

	interceptors := []connect.Interceptor{auth.NewAuthInterceptor(gap142Token, true)}
	if log != nil {
		interceptors = append(interceptors, audit.NewInterceptor(log, logger))
	}

	mux := http.NewServeMux()
	path, handler := bunkerv1connect.NewBunkerdHandler(svc, connect.WithInterceptors(interceptors...))
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
}

// gap142Request builds an authenticated request envelope.
func gap142Request[T any](msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+gap142Token)
	return req
}

// gap142AuditLog opens a fresh audit log in a temp dir.
func gap142AuditLog(t *testing.T) (*audit.AuditLog, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := audit.New(path)
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, path
}

// gap142RunExec drives one ExecAgent call to completion and returns its exit
// code. The command builder seam is replaced so no ssh is involved.
func gap142RunExec(t *testing.T, client bunkerv1connect.BunkerdClient, msg *v1.ExecAgentRequest) (int32, error) {
	t.Helper()
	stream, err := client.ExecAgent(context.Background(), gap142Request(msg))
	if err != nil {
		return -1, err
	}
	exit := int32(-1)
	for stream.Receive() {
		if m := stream.Msg(); m.GetStdout() == nil && m.GetStderr() == nil {
			exit = m.GetExitCode()
		}
	}
	serr := stream.Err()
	_ = stream.Close()
	return exit, serr
}

// gap142CommandRecords returns the correlated command-content records for an
// agent (the GAP-142 sub-kind), in chain order.
func gap142CommandRecords(t *testing.T, path, agentID string) []audit.Record {
	t.Helper()
	recs, err := audit.Query(path, audit.Filter{AgentID: agentID})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var out []audit.Record
	for _, r := range recs {
		if strings.HasSuffix(r.Method, audit.ExecRecordMethod) {
			out = append(out, r)
		}
	}
	return out
}

// gap142AllRecords parses every record in the audit log (used in failure
// messages and chain assertions).
func gap142AllRecords(t *testing.T, path string) []audit.Record {
	t.Helper()
	recs, err := audit.Query(path, audit.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return recs
}

// gap142InjectExecCommand replaces the exec builder seams for this test so the
// handler runs argv locally instead of ssh (the same seam
// dfb27InjectExecCommand uses; a distinct name keeps the two batteries
// independent).
func gap142InjectExecCommand(t *testing.T, argv ...string) {
	t.Helper()
	prevShell, prevRaw, prevScript := execSSHCommandBuilder, execSSHRawCommandBuilder, execSSHScriptCommandBuilder
	build := func(ctx context.Context, _, _, _, _ string, _ []string, _ bool, _ string) *exec.Cmd {
		return exec.CommandContext(ctx, argv[0], argv[1:]...)
	}
	buildScript := func(ctx context.Context, _, _, _, _ string, _ bool, _ string) *exec.Cmd {
		return exec.CommandContext(ctx, argv[0], argv[1:]...)
	}
	execSSHCommandBuilder, execSSHRawCommandBuilder, execSSHScriptCommandBuilder = build, build, buildScript
	t.Cleanup(func() {
		execSSHCommandBuilder, execSSHRawCommandBuilder, execSSHScriptCommandBuilder = prevShell, prevRaw, prevScript
	})
}

// TestGAP142_ExecCommandReachesTheChain is acceptance criteria 1, 2 and 5: one
// exec produces a command record carrying the command, the agent id and the
// caller; it is chained to the RPC record the interceptor wrote for the same
// request; and the whole trail still verifies.
func TestGAP142_ExecCommandReachesTheChain(t *testing.T) {
	log, path := gap142AuditLog(t)
	gap142InjectExecCommand(t, "sh", "-c", "exit 0")
	client := gap142Server(t, log)

	exit, err := gap142RunExec(t, client, &v1.ExecAgentRequest{
		AgentId: gap142AgentID,
		Command: "deploy",
		Args:    []string{"--region", "eu-1", "--token", "abcdef123456"},
	})
	if err != nil {
		t.Fatalf("ExecAgent: %v", err)
	}
	if exit != 0 {
		t.Fatalf("exit code = %d, want 0", exit)
	}

	// criterion 1: an exec appears in `audit query` with its command, agent id
	// and caller.
	cmds := gap142CommandRecords(t, path, gap142AgentID)
	if len(cmds) != 1 {
		t.Fatalf("command records = %d, want 1; all records: %+v", len(cmds), gap142AllRecords(t, path))
	}
	rec := cmds[0]
	if rec.AgentID != gap142AgentID {
		t.Errorf("agent_id = %q, want %q", rec.AgentID, gap142AgentID)
	}
	if rec.Caller != "master" {
		t.Errorf("caller = %q, want master (the authenticated static-token identity)", rec.Caller)
	}
	if !strings.Contains(rec.Summary, "deploy") || !strings.Contains(rec.Summary, "--region eu-1") {
		t.Errorf("summary %q does not carry the command", rec.Summary)
	}
	if rec.Outcome != "ok" {
		t.Errorf("outcome = %q, want ok", rec.Outcome)
	}
	if !strings.HasSuffix(rec.Method, "ExecAgent"+audit.ExecRecordMethod) {
		t.Errorf("method = %q, want the ExecAgent procedure plus the command sub-kind", rec.Method)
	}

	// criterion 2: the command record is correlated to the same chain as the RPC
	// record — both records exist for the same agent/caller and chain together.
	all := gap142AllRecords(t, path)
	if len(all) != 2 {
		t.Fatalf("records = %d, want 2 (one command record + one RPC record): %+v", len(all), all)
	}
	var sawRPC bool
	for _, r := range all {
		if r.Method == "/bunker.v1.Bunkerd/ExecAgent" {
			sawRPC = true
		}
		if r.AgentID != gap142AgentID {
			t.Errorf("record %q does not agree on the agent: %q", r.Method, r.AgentID)
		}
		if r.Caller != "master" {
			t.Errorf("record %q does not agree on the caller: %q", r.Method, r.Caller)
		}
	}
	if !sawRPC {
		t.Errorf("no RPC record for the exec procedure: %+v", all)
	}
	if all[1].PrevHash != all[0].Hash {
		t.Errorf("records are not chained: %q does not chain to %q", all[1].PrevHash, all[0].Hash)
	}

	// criterion 5: the chain containing a command record still verifies.
	n, firstBad, err := audit.Verify(path)
	if err != nil || firstBad != 0 {
		t.Fatalf("audit verify after a correlated exec record (firstBad=%d): %v", firstBad, err)
	}
	if n != len(all) {
		t.Errorf("Verify counted %d records, parsed %d", n, len(all))
	}
}

// TestGAP142_CredentialInCommandNeverReachesTheTrail is the caution on the board
// row: command lines can carry secrets, so no raw credential may appear in the
// log bytes or in the summary — only a shape-preserving placeholder.
func TestGAP142_CredentialInCommandNeverReachesTheTrail(t *testing.T) {
	const (
		token     = "gap142-super-secret-token"
		hexSecret = "5f4dcc3b5aa765d61d8327deb882cf99aabbccdd"
	)
	log, path := gap142AuditLog(t)
	gap142InjectExecCommand(t, "sh", "-c", "exit 0")
	client := gap142Server(t, log)

	if _, err := gap142RunExec(t, client, &v1.ExecAgentRequest{
		AgentId: gap142AgentID,
		Command: "curl",
		Args:    []string{"-H", `"Authorization: Bearer ` + token + `"`, "echo", hexSecret},
	}); err != nil {
		t.Fatalf("ExecAgent: %v", err)
	}

	cmds := gap142CommandRecords(t, path, gap142AgentID)
	if len(cmds) != 1 {
		t.Fatalf("command records = %d, want 1", len(cmds))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for _, secret := range []string{token, hexSecret} {
		if strings.Contains(cmds[0].Summary, secret) {
			t.Errorf("summary leaks %q: %q", secret, cmds[0].Summary)
		}
		if strings.Contains(string(raw), secret) {
			t.Errorf("raw audit bytes leak %q", secret)
		}
	}
	if !strings.Contains(cmds[0].Summary, audit.RedactedPrefix) {
		t.Errorf("summary %q carries no redaction placeholder", cmds[0].Summary)
	}
}

// TestGAP142_ScriptUploadIsDigestedNotRecorded proves the stricter rule for
// script uploads: the body is summarized by size + digest, so an embedded
// credential cannot reach the trail even though the body is never scanned.
func TestGAP142_ScriptUploadIsDigestedNotRecorded(t *testing.T) {
	const secret = "gap142-script-embedded-secret"
	log, path := gap142AuditLog(t)
	gap142InjectExecCommand(t, "sh", "-c", "exit 0")
	client := gap142Server(t, log)

	script := "#!/bin/sh\nexport API_KEY=" + secret + "\necho done\n"
	if _, err := gap142RunExec(t, client, &v1.ExecAgentRequest{
		AgentId:       gap142AgentID,
		ScriptContent: script,
	}); err != nil {
		t.Fatalf("ExecAgent(script): %v", err)
	}

	cmds := gap142CommandRecords(t, path, gap142AgentID)
	if len(cmds) != 1 {
		t.Fatalf("command records = %d, want 1", len(cmds))
	}
	if strings.Contains(cmds[0].Summary, secret) {
		t.Fatalf("script summary leaks the embedded credential: %q", cmds[0].Summary)
	}
	if !strings.HasPrefix(cmds[0].Summary, "script bytes=") {
		t.Errorf("script summary = %q, want the bytes+digest form", cmds[0].Summary)
	}
	if _, firstBad, err := audit.Verify(path); err != nil || firstBad != 0 {
		t.Fatalf("audit verify after a script record (firstBad=%d): %v", firstBad, err)
	}
}

// TestGAP142_OutcomeComesFromTheNamedReturn is a source-invariant guard: the
// recorder must derive its outcome from the handler's NAMED return error, not
// from hand-maintained assignments on each path. The earlier hand-assigned shape
// silently recorded a tracker-not-found RunAgent as "ok" (caught by a live probe
// against the real daemon), so this pins the structural fix.
func TestGAP142_OutcomeComesFromTheNamedReturn(t *testing.T) {
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	body := string(src)
	for _, want := range []string{
		"func (s *bunkerdService) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) (err error)",
		"func (s *bunkerdService) RunAgent(ctx context.Context, req *connect.Request[v1.RunAgentRequest]) (result *connect.Response[v1.RunAgentResponse], err error)",
		"execState, &err)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("service.go no longer contains %q — the recorder's outcome is no longer tied to the handler result", want)
		}
	}
	// Both handlers must pass the named error through; two call sites, one per RPC.
	if got := strings.Count(body, "execState, &err)"); got != 2 {
		t.Errorf("recorder call sites passing the named error = %d, want 2 (ExecAgent + RunAgent)", got)
	}
	if strings.Contains(body, "execState.connectErr") {
		t.Error("service.go still hand-assigns execState.connectErr — the removed shape is back")
	}
}

// TestGAP142_NonZeroExitIsRecorded proves the command record reflects what the
// command did, not merely that an RPC happened.
func TestGAP142_NonZeroExitIsRecorded(t *testing.T) {
	log, path := gap142AuditLog(t)
	gap142InjectExecCommand(t, "sh", "-c", "exit 3")
	client := gap142Server(t, log)

	exit, err := gap142RunExec(t, client, &v1.ExecAgentRequest{
		AgentId: gap142AgentID,
		Command: "false",
	})
	if err != nil {
		t.Fatalf("ExecAgent: %v", err)
	}
	if exit != 3 {
		t.Fatalf("exit code = %d, want 3", exit)
	}
	cmds := gap142CommandRecords(t, path, gap142AgentID)
	if len(cmds) != 1 {
		t.Fatalf("command records = %d, want 1", len(cmds))
	}
	if cmds[0].Outcome != "exit_3" {
		t.Errorf("outcome = %q, want exit_3", cmds[0].Outcome)
	}
}

// TestGAP142_HandlerFailureIsRecorded proves a request that never reaches the
// command still produces its command record (with the connect error code), so a
// forensic reader sees the attempted command and why it did not run.
func TestGAP142_HandlerFailureIsRecorded(t *testing.T) {
	log, path := gap142AuditLog(t)
	client := gap142Server(t, log)

	_, err := gap142RunExec(t, client, &v1.ExecAgentRequest{
		AgentId: "no-such-agent",
		Command: "deploy",
		Args:    []string{"--token", "abcdef123456"},
	})
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown agent: code = %v, want CodeNotFound", connect.CodeOf(err))
	}

	cmds := gap142CommandRecords(t, path, "no-such-agent")
	if len(cmds) != 1 {
		t.Fatalf("command records = %d, want 1 (the attempt must be recorded)", len(cmds))
	}
	if cmds[0].Outcome != "not_found" {
		t.Errorf("outcome = %q, want not_found", cmds[0].Outcome)
	}
	if strings.Contains(cmds[0].Summary, "abcdef123456") {
		t.Errorf("summary leaks the credential: %q", cmds[0].Summary)
	}
}

// TestGAP142_RunAgentRecordsItsCommand proves the detach RPC correlates too.
func TestGAP142_RunAgentRecordsItsCommand(t *testing.T) {
	log, path := gap142AuditLog(t)
	client := gap142Server(t, log)

	_, err := client.RunAgent(context.Background(), gap142Request(&v1.RunAgentRequest{
		AgentId: gap142AgentID,
		Command: "long-job",
		Args:    []string{"--apikey", "z9y8x7w6v5u4"},
		Detach:  true,
	}))
	// The agent manager is nil in this harness, so the handler fails after the
	// command was built — which is exactly the path that must still be recorded.
	if err == nil {
		t.Log("RunAgent unexpectedly succeeded (no agent manager); the record path is the subject")
	}

	cmds := gap142CommandRecords(t, path, gap142AgentID)
	if len(cmds) != 1 {
		t.Fatalf("command records = %d, want 1; records: %+v", len(cmds), gap142AllRecords(t, path))
	}
	if !strings.Contains(cmds[0].Summary, "long-job") {
		t.Errorf("summary %q does not carry the command", cmds[0].Summary)
	}
	if strings.Contains(cmds[0].Summary, "z9y8x7w6v5u4") {
		t.Errorf("summary leaks the credential: %q", cmds[0].Summary)
	}
	if !strings.HasSuffix(cmds[0].Method, "RunAgent"+audit.ExecRecordMethod) {
		t.Errorf("method = %q, want the RunAgent procedure plus the command sub-kind", cmds[0].Method)
	}
}

// TestGAP142_BrokenAuditLogDoesNotFailTheExec is the write-failure swallow: a
// closed audit log must not change the exec outcome.
func TestGAP142_BrokenAuditLogDoesNotFailTheExec(t *testing.T) {
	log, _ := gap142AuditLog(t)
	// Close the log: every write from here on fails.
	if err := log.Close(); err != nil {
		t.Fatalf("close audit log: %v", err)
	}

	gap142InjectExecCommand(t, "sh", "-c", "echo still-works")
	client := gap142Server(t, log)

	var out string
	stream, err := client.ExecAgent(context.Background(), gap142Request(&v1.ExecAgentRequest{
		AgentId: gap142AgentID,
		Command: "echo",
		Args:    []string{"hi"},
	}))
	if err != nil {
		t.Fatalf("ExecAgent with a broken audit log: %v", err)
	}
	exit := int32(-1)
	for stream.Receive() {
		msg := stream.Msg()
		if msg.GetStdout() != nil {
			out += string(msg.GetStdout())
		}
		if msg.GetStdout() == nil && msg.GetStderr() == nil {
			exit = msg.GetExitCode()
		}
	}
	if serr := stream.Err(); serr != nil {
		t.Fatalf("stream error with a broken audit log: %v", serr)
	}
	_ = stream.Close()

	if exit != 0 {
		t.Errorf("exit code = %d, want 0 — auditing must never fail the command", exit)
	}
	if out != "still-works\n" {
		t.Errorf("stdout = %q, want the command's real output", out)
	}
}

// TestGAP142_AuditDisabledRecordsNothingButStillExecs proves the nil-log path is
// inert (no panic, no record) and the exec still works.
func TestGAP142_AuditDisabledRecordsNothingButStillExecs(t *testing.T) {
	gap142InjectExecCommand(t, "sh", "-c", "exit 0")
	client := gap142Server(t, nil) // no audit log at all

	if _, err := gap142RunExec(t, client, &v1.ExecAgentRequest{
		AgentId: gap142AgentID,
		Command: "uptime",
	}); err != nil {
		t.Fatalf("ExecAgent with auditing disabled: %v", err)
	}
}

// TestGAP142_RecordsAreQueryableByCommandSubKind proves the documented query
// surface works: the command records are selectable by the method sub-kind and
// by agent, exactly as docs/exec-audit.md tells an operator to do.
func TestGAP142_RecordsAreQueryableByCommandSubKind(t *testing.T) {
	log, path := gap142AuditLog(t)
	gap142InjectExecCommand(t, "sh", "-c", "exit 0")
	client := gap142Server(t, log)

	for _, cmd := range []string{"first-command", "second-command"} {
		if _, err := gap142RunExec(t, client, &v1.ExecAgentRequest{
			AgentId: gap142AgentID,
			Command: cmd,
		}); err != nil {
			t.Fatalf("ExecAgent(%s): %v", cmd, err)
		}
	}

	// The method substring filter the CLI exposes finds both command records.
	byMethod, err := audit.Query(path, audit.Filter{Method: audit.ExecRecordMethod})
	if err != nil {
		t.Fatalf("Query(method): %v", err)
	}
	if len(byMethod) != 2 {
		t.Fatalf("records matching the command sub-kind = %d, want 2", len(byMethod))
	}
	// ... and they carry the commands in chain order.
	if !strings.Contains(byMethod[0].Summary, "first-command") {
		t.Errorf("first record summary = %q, want first-command", byMethod[0].Summary)
	}
	if !strings.Contains(byMethod[1].Summary, "second-command") {
		t.Errorf("second record summary = %q, want second-command", byMethod[1].Summary)
	}
	// The agent filter the CLI exposes finds them too.
	if got := len(gap142CommandRecords(t, path, gap142AgentID)); got != 2 {
		t.Errorf("agent-filtered command records = %d, want 2", got)
	}
}
