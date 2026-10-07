package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
	"github.com/spf13/cobra"
)

// The uninstall tests pin the GAP-096 criterion 3 contract at three levels:
// the drift DECISION as a pure table, the uninstall FLOW through a scripted
// ExecAgent fake (same shape the connect tests use), and the REMOVAL SCRIPT
// itself executed under a real /bin/sh against a fake $HOME — because a
// script builder that was never run proves nothing about the shell.

// TestReportVersionDrift is the drift-decision table (GAP-096 criterion 2).
// Containment (the agent's line embedding the local token) is the same build;
// a difference is drift; an unreadable side warns on nothing.
func TestReportVersionDrift(t *testing.T) {
	cases := []struct {
		name         string
		local, agent string
		want         bool
	}{
		{"same token", "98c75fa-dirty", "toolsd version 98c75fa-dirty (built 2026-09-20)", false},
		{"agent embeds the local token", "dev", "toolsd version dev-extra (built x)", false},
		{"genuinely different", "v1.2.3", "toolsd version v1.3.0", true},
		{"agent unreadable", "v1.2.3", "", false},
		{"local unreadable", "", "toolsd version v1.3.0", false},
		{"both unreadable", "", "", false},
	}
	for _, c := range cases {
		if got := reportVersionDrift(c.local, c.agent); got != c.want {
			t.Errorf("%s: reportVersionDrift(%q, %q) = %v, want %v", c.name, c.local, c.agent, got, c.want)
		}
	}
}

// fakeExecBunkerd answers ExecAgent by inspecting the script the CLI sent, so
// the uninstall flow (removal, then the verifying re-probe) can run end to end
// without an agent. Every exec'd script is recorded for the wiring assertions.
type fakeExecBunkerd struct {
	mockBunkerdServer
	scripts []string
	respond func(script string) (string, int32, error)
}

func (m *fakeExecBunkerd) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest],
	stream *connect.ServerStream[v1.ExecAgentResponse]) error {

	args := req.Msg.GetArgs()
	script := ""
	if n := len(args); n > 0 {
		script = args[n-1]
	}
	m.scripts = append(m.scripts, script)
	out, code, err := m.respond(script)
	if err != nil {
		return err
	}
	return stream.Send(&v1.ExecAgentResponse{
		Output:   &v1.ExecAgentResponse_Stdout{Stdout: []byte(out)},
		ExitCode: code,
	})
}

// probeOutput renders the probe script's expected output with every catalogued
// tool in the given state, so a parse quirk on one entry cannot silently pass.
func probeOutput(status string) string {
	lines := make([]string, 0, len(agentToolCatalog))
	for _, dep := range agentToolCatalog {
		v := ""
		if status == "present" {
			v = dep.Name + " version 1.0.0"
		}
		lines = append(lines, dep.Name+"\t"+status+"\t"+v)
	}
	return strings.Join(lines, "\n")
}

// runUninstall drives uninstallAgentTools directly (the connect tests' shape:
// no config file, no SSH — the fake server plays the daemon) and returns what
// the command wrote to stdout and stderr.
func runUninstall(t *testing.T, fake *fakeExecBunkerd) (string, string, error) {
	t.Helper()
	srv := newTestServer(t, fake)
	t.Cleanup(srv.Close)

	client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
	entry := ServerEntry{Name: "default", URL: srv.URL}

	cmd := &cobra.Command{Use: "test"}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	err := uninstallAgentTools(cmd, context.Background(), client, entry, "agent-uninstall-test")
	return out.String(), errOut.String(), err
}

// removalRespond answers the removal script, then the verifying probe, in the
// order the uninstall flow sends them.
func removalRespond(t *testing.T, removalOut string, removalExit int32, probeOut string) func(string) (string, int32, error) {
	t.Helper()
	called := 0
	return func(script string) (string, int32, error) {
		called++
		if strings.Contains(script, "rm -f") {
			return removalOut, removalExit, nil
		}
		if called != 2 {
			t.Fatalf("expected the verifying probe to be exec #2, got call #%d", called)
		}
		return probeOut, 0, nil
	}
}

// TestUninstallAgentToolsRemovesAndProvesAbsence: the happy path removes, then
// re-probes, and the report says what was removed — with the wiring pinned
// (script #1 is the removal script, script #2 is the catalog probe).
func TestUninstallAgentToolsRemovesAndProvesAbsence(t *testing.T) {
	wantRemove := agentToolsRemoveScript()
	wantProbe := strings.Replace(agentProbeScript, "__TOOLS__", catalogNames(), 1)
	fake := &fakeExecBunkerd{respond: removalRespond(t,
		"toolsd\tREMOVED\twas-present\n", 0, probeOutput("absent"))}

	out, errOut, err := runUninstall(t, fake)
	if err != nil {
		t.Fatalf("uninstall failed: %v", err)
	}
	if len(fake.scripts) != 2 {
		t.Fatalf("expected exactly two execs (remove, probe), got %d: %q", len(fake.scripts), fake.scripts)
	}
	if fake.scripts[0] != wantRemove {
		t.Errorf("exec #1 is not the removal script:\n got: %q\nwant: %q", fake.scripts[0], wantRemove)
	}
	if fake.scripts[1] != wantProbe {
		t.Errorf("exec #2 is not the verifying catalog probe:\n got: %q\nwant: %q", fake.scripts[1], wantProbe)
	}
	if !strings.Contains(out, "removed toolsd") || !strings.Contains(out, "was-present") {
		t.Errorf("stdout must name the removed file and its prior state, got:\n%s", out)
	}
	if !strings.Contains(out, "toolsd absent") {
		t.Errorf("stdout must carry the re-probe verdict, got:\n%s", out)
	}
	if strings.Contains(errOut, "WARNING") {
		t.Errorf("the happy path must not warn, got stderr:\n%s", errOut)
	}
}

// TestUninstallAgentToolsToleratesAlreadyAbsent: removing an absent file is a
// named outcome, not an error — the same tone `bunker surface remove` uses.
func TestUninstallAgentToolsToleratesAlreadyAbsent(t *testing.T) {
	fake := &fakeExecBunkerd{respond: removalRespond(t,
		"toolsd\tREMOVED\talready-absent\n", 0, probeOutput("absent"))}

	out, _, err := runUninstall(t, fake)
	if err != nil {
		t.Fatalf("an idempotent re-run must succeed, got: %v", err)
	}
	if !strings.Contains(out, "already-absent") {
		t.Errorf("the absence must be reported by name, got:\n%s", out)
	}
}

// TestUninstallAgentToolsFailsWhenToolsdSurvives is the load-bearing direction:
// a re-probe that still finds toolsd on the agent's PATH fails the command with
// the survivor named, and no success line is printed.
func TestUninstallAgentToolsFailsWhenToolsdSurvives(t *testing.T) {
	fake := &fakeExecBunkerd{respond: removalRespond(t,
		"toolsd\tREMOVED\twas-present\n", 0, probeOutput("present"))}

	out, _, err := runUninstall(t, fake)
	if err == nil {
		t.Fatal("a surviving toolsd must fail the uninstall")
	}
	for _, want := range []string{"uninstall FAILED", "still reachable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure must say %q, got: %v", want, err)
		}
	}
	if strings.Contains(out, "uninstalled delivered tools") {
		t.Errorf("a failed uninstall must not print the success line, got:\n%s", out)
	}
}

// TestUninstallAgentToolsFailsWhenRemovalReportsFailure: the script naming a
// failed rm is an incomplete uninstall even when the re-probe happens to look
// clean — nothing is claimed uninstalled beyond what the probe proves.
func TestUninstallAgentToolsFailsWhenRemovalReportsFailure(t *testing.T) {
	fake := &fakeExecBunkerd{respond: removalRespond(t,
		"toolsd\tFAILED\tPermission denied\n", 0, probeOutput("absent"))}

	out, errOut, err := runUninstall(t, fake)
	if err == nil {
		t.Fatal("a FAILED removal must fail the uninstall")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("the failure must say the uninstall is incomplete, got: %v", err)
	}
	if !strings.Contains(errOut, "Permission denied") {
		t.Errorf("stderr must carry the agent's own words, got:\n%s", errOut)
	}
	if strings.Contains(out, "uninstalled delivered tools") {
		t.Errorf("an incomplete uninstall must not print the success line, got:\n%s", out)
	}
}

// TestUninstallAgentToolsFailsOnUnparsableOutput pins the honesty rule: a
// removal script whose output cannot be parsed is an incomplete uninstall,
// never a silent success.
func TestUninstallAgentToolsFailsOnUnparsableOutput(t *testing.T) {
	fake := &fakeExecBunkerd{respond: removalRespond(t,
		"rm: something went catastrophically wrong\n", 1, probeOutput("absent"))}

	_, _, err := runUninstall(t, fake)
	if err == nil {
		t.Fatal("unparsable removal output must fail the uninstall")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("the failure must say the uninstall is incomplete, got: %v", err)
	}
}

// TestUninstallAgentToolsFailsWhenProbeDies: the removal ran, but the evidence
// step itself failed — the command must refuse to claim absence it cannot see.
func TestUninstallAgentToolsFailsWhenProbeDies(t *testing.T) {
	fake := &fakeExecBunkerd{respond: func(script string) (string, int32, error) {
		if strings.Contains(script, "rm -f") {
			return "toolsd\tREMOVED\twas-present\n", 0, nil
		}
		return "", 0, connect.NewError(connect.CodeUnavailable, nil)
	}}

	_, _, err := runUninstall(t, fake)
	if err == nil {
		t.Fatal("a dead verifying probe must fail the uninstall")
	}
	if !strings.Contains(err.Error(), "probe failed") {
		t.Errorf("the failure must name the probe, got: %v", err)
	}
}

// TestAgentToolsRemoveScriptByExecution runs the generated script under a real
// /bin/sh against a fake $HOME, in all three outcomes: present (removed),
// already gone (named, still success), and unremovable (FAILED line).
func TestAgentToolsRemoveScriptByExecution(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the read-only-directory case cannot fail")
	}
	homes := t.TempDir()

	// 1. Present file: removed, reported was-present.
	home1 := filepath.Join(homes, "present")
	if err := os.MkdirAll(filepath.Join(home1, agentToolInstallDir), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home1, agentToolInstallDir, "toolsd")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := runScriptWithHome(t, agentToolsRemoveScript(), home1)
	if out != "toolsd\tREMOVED\twas-present\n" {
		t.Errorf("removal of a present file = %q, want the was-present line", out)
	}
	if _, err := os.Stat(bin); !os.IsNotExist(err) {
		t.Errorf("the file must be gone after the script ran, stat err: %v", err)
	}

	// 2. Already absent: named, still REMOVED.
	out = runScriptWithHome(t, agentToolsRemoveScript(), home1)
	if out != "toolsd\tREMOVED\talready-absent\n" {
		t.Errorf("idempotent re-run = %q, want the already-absent line", out)
	}

	// 3. Unremovable: the FAILED line carries rm's own words.
	home3 := filepath.Join(homes, "stuck")
	if err := os.MkdirAll(home3, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home3, agentToolInstallDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stuck := filepath.Join(dir, "toolsd")
	if err := os.WriteFile(stuck, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Only NOW lock the directory: a read-only bin/ with no file in it is the
	// already-absent case, not the unremovable one.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	out = runScriptWithHome(t, agentToolsRemoveScript(), home3)
	if !strings.HasPrefix(out, "toolsd\tFAILED\t") {
		t.Errorf("an unremovable file must produce a FAILED line, got %q", out)
	}
}

// runScriptWithHome executes a script under sh with $HOME pointed at home,
// exactly the environment the agent execs into.
func runScriptWithHome(t *testing.T, script, home string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh -c failed: %v\noutput: %s", err, out)
	}
	return string(out)
}

// catalogNames joins the catalog for the probe-script substitution, mirroring
// probeAgentTools.
func catalogNames() string {
	names := make([]string, 0, len(agentToolCatalog))
	for _, dep := range agentToolCatalog {
		names = append(names, dep.Name)
	}
	return strings.Join(names, " ")
}

// TestAgentToolsUninstallFlagRegistered: the uninstall verb is reachable from
// the parent command, mirrors the --install flag, and the two refuse to be
// combined in one invocation.
func TestAgentToolsUninstallFlagRegistered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cmd := NewAgentToolsCommand()
	if f := cmd.Flags().Lookup("uninstall"); f == nil {
		t.Fatal("the agent-tools command must expose --uninstall")
	}

	// Both verbs at once: refused locally, before any config or RPC.
	cmd = NewAgentToolsCommand()
	cmd.SetArgs([]string{"agent-id", "--install", "--uninstall"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--install together with --uninstall must be refused")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("the refusal must name the conflict, got: %v", err)
	}
}
