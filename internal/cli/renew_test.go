package cli

// DF-BUNKER-34 CLI contract tests: the renew command's identity gate, and
// the orphan-uid warning blocks on info/list.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// renewMockServer implements BunkerdHandler with canned GetAgent / Destroy /
// Spawn responses for the renew flow. RenewalDriftReport is unimplemented
// (the CLI warns and continues on a daemon that predates the RPC).
type renewMockServer struct {
	mockBunkerdServer
	agent          *v1.AgentSummary
	destroyStatus  string
	spawnAgentID   string
	destroyErr     error
	spawnErr       error
	destroyCalled  bool
	spawnRequested *v1.SpawnAgentRequest
}

func (m *renewMockServer) GetAgent(ctx context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
	if m.agent == nil {
		return nil, connect.NewError(connect.CodeNotFound, nil)
	}
	return connect.NewResponse(&v1.GetAgentResponse{Agent: m.agent}), nil
}

func (m *renewMockServer) DestroyAgent(ctx context.Context, req *connect.Request[v1.DestroyAgentRequest]) (*connect.Response[v1.DestroyAgentResponse], error) {
	m.destroyCalled = true
	if m.destroyErr != nil {
		return nil, m.destroyErr
	}
	return connect.NewResponse(&v1.DestroyAgentResponse{AgentId: req.Msg.AgentId, Status: m.destroyStatus}), nil
}

func (m *renewMockServer) SpawnAgent(ctx context.Context, req *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error) {
	m.spawnRequested = req.Msg
	if m.spawnErr != nil {
		return nil, m.spawnErr
	}
	return connect.NewResponse(&v1.SpawnAgentResponse{AgentId: m.spawnAgentID, ExpiresAt: "2026-10-01T00:00:00Z"}), nil
}

func (m *renewMockServer) RenewalDriftReport(ctx context.Context, req *connect.Request[v1.RenewalDriftRequest]) (*connect.Response[v1.RenewalDriftResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

// runRenew drives the renew command against the mock and returns (stdout,
// execute error).
func runRenew(t *testing.T, srv *httptest.Server, args ...string) (string, error) {
	t.Helper()
	t.Setenv(SessionTargetEnvVar, "default")
	writeRenewTestConfig(t, srv.URL)
	cmd := NewRenewCommand()
	cmd.SetArgs(args)
	var execErr error
	output := captureStdout(t, func() {
		execErr = cmd.Execute()
	})
	return output, execErr
}

// writeRenewTestConfig writes a CLI config with the given server URL active.
func writeRenewTestConfig(t *testing.T, serverURL string) {
	t.Helper()
	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"default": {Name: "default", URL: serverURL, ConnectedAt: "2026-06-28T00:00:00Z"},
		},
		ActiveServer: "default",
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}
}

// newRenewTestServer mounts the mock as a connect handler.
func newRenewTestServer(t *testing.T, handler bunkerv1connect.BunkerdHandler) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(handler)
	r.Mount(path, h)
	return httptest.NewServer(r)
}

// TestRenewCommand_RefusesWithoutAgentID is the row's identity gate, at the
// CLI: an anonymous renewal is refused LOUDLY with the remedy in the error,
// before any RPC is made (nothing is destroyed, nothing re-spawned).
func TestRenewCommand_RefusesWithoutAgentID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mock := &renewMockServer{}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	_, execErr := runRenew(t, srv)
	if execErr == nil {
		t.Fatal("renew without --agent-id must fail")
	}
	for _, want := range []string{"renewal refused", "--agent-id", "DF-BUNKER-34", "docs/renewal.md"} {
		if !strings.Contains(execErr.Error(), want) {
			t.Errorf("refusal missing %q — got: %v", want, execErr)
		}
	}
	if mock.destroyCalled {
		t.Error("the refusal must come BEFORE the destroy")
	}
	if mock.spawnRequested != nil {
		t.Error("renew without --agent-id must not spawn")
	}
}

// TestRenewCommand_CarriesStableAgentID proves the renewal's core contract:
// the spawn request the command issues carries the SAME agent id it destroyed,
// and the daemon's answer is checked against it (a daemon that minted a
// different id fails the renewal).
func TestRenewCommand_CarriesStableAgentID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mock := &renewMockServer{
		agent: &v1.AgentSummary{
			AgentId:    "eduos-agent",
			SshfsMount: "sshfs -o IdentityFile=/keys/eduos-agent -o idmap=user -o allow_other bunker-eduos-agent@host:/home/bunker-eduos-agent /mnt/bunker/eduos-agent",
		},
		destroyStatus: "destroyed",
		spawnAgentID:  "eduos-agent",
	}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	output, execErr := runRenew(t, srv, "--agent-id", "eduos-agent", "--ttl", "7d")
	if execErr != nil {
		t.Fatalf("renew failed: %v", execErr)
	}
	// The spawn request carried the SAME id.
	if mock.spawnRequested == nil {
		t.Fatal("the renew never issued a spawn")
	}
	if got := mock.spawnRequested.GetAgentId(); got != "eduos-agent" {
		t.Errorf("spawn request agent_id = %q, want the stable id %q", got, "eduos-agent")
	}
	if got := mock.spawnRequested.GetTtl(); got != "7d" {
		t.Errorf("spawn request ttl = %q, want 7d", got)
	}
	if !mock.destroyCalled {
		t.Error("the renew never destroyed the old instance")
	}
	for _, want := range []string{"Destroying agent eduos-agent", "Re-spawning agent eduos-agent (stable identity)", "home path unchanged"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
}

// TestRenewCommand_DaemonMintedDifferentIDIsRefused pins the response check:
// if the daemon ignored the requested id and minted a new one, the renewal
// FAILS LOUDLY instead of reporting a stable renewal that did not happen.
func TestRenewCommand_DaemonMintedDifferentIDIsRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mock := &renewMockServer{
		destroyStatus: "destroyed",
		spawnAgentID:  "2cdce4d0", // the daemon minted a fresh id
	}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	_, execErr := runRenew(t, srv, "--agent-id", "eduos-agent")
	if execErr == nil {
		t.Fatal("a daemon that minted a different id must fail the renewal")
	}
	if !strings.Contains(execErr.Error(), "2cdce4d0") || !strings.Contains(execErr.Error(), "eduos-agent") {
		t.Errorf("error must name both ids, got: %v", execErr)
	}
}

// TestRenewCommand_DestroyRefusalStopsRenewal proves the renewal honors the
// destroy gate: a live_processes refusal (destroy refused) aborts the
// renewal BEFORE any spawn, with the refusal's status in the error.
func TestRenewCommand_DestroyRefusalStopsRenewal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mock := &renewMockServer{
		agent:         &v1.AgentSummary{AgentId: "eduos-agent"},
		destroyStatus: "live_processes",
	}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	output, execErr := runRenew(t, srv, "--agent-id", "eduos-agent")
	if execErr == nil {
		t.Fatal("a refused destroy must abort the renewal")
	}
	if !strings.Contains(execErr.Error(), "live_processes") {
		t.Errorf("error must carry the refusal status, got: %v", execErr)
	}
	if mock.spawnRequested != nil {
		t.Error("the renewal spawned despite the destroy refusal")
	}
	if !strings.Contains(output, "Destroying agent eduos-agent") {
		t.Errorf("output missing the destroy step, got:\n%s", output)
	}
}

// TestRenewCommand_HelpCarriesRecipe documents the recipe pointer in --help.
func TestRenewCommand_Help(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cmd := NewRenewCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		_ = cmd.Execute()
	})
	for _, want := range []string{"--agent-id", "re-spawning the SAME agent id", "docs/renewal.md"} {
		if !strings.Contains(output, want) {
			t.Errorf("help output missing %q, got:\n%s", want, output)
		}
	}
}

// ── info / list orphan surfacing ───────────────────────────────────────────

// orphanInfoMock returns the orphan detail on the agent summary.
type orphanInfoMockServer struct {
	infoMockServer
	orphan string
}

func (m *orphanInfoMockServer) GetAgent(ctx context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
	a := m.agent
	a.OrphanUidDetail = m.orphan
	return connect.NewResponse(&v1.GetAgentResponse{Agent: a}), nil
}

// TestInfoCommand_OrphanUIDWarning proves the info surface renders the
// orphan-uid warning block when the daemon reports one, and nothing when it
// reports none.
func TestInfoCommand_OrphanUIDWarning(t *testing.T) {
	// writeInfoConfig below saves the CLI config; isolate from the real
	// operator ~/.bunker/config.yaml like every other config-writing test.
	t.Setenv("HOME", t.TempDir())

	orphan := "ORPHANED UID: user record bunker-zomb is GONE from the host but 2 live process(es) under uid 1002: pid 1189: node duckbrain.js"
	mock := &orphanInfoMockServer{
		orphan: orphan,
		infoMockServer: infoMockServer{
			agent: &v1.AgentSummary{AgentId: "zomb", Status: "running"},
		},
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	writeInfoConfig(t, srv.URL)
	cmd := NewInfoCommand()
	cmd.SetArgs([]string{"--server", "test", "zomb"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if !strings.Contains(output, "ORPHANED UID DETECTED") || !strings.Contains(output, orphan) {
		t.Errorf("info output missing the orphan warning, got:\n%s", output)
	}

	// The control: no orphan detail, no warning block.
	mock.orphan = ""
	cmd2 := NewInfoCommand()
	cmd2.SetArgs([]string{"--server", "test", "zomb"})
	output2 := captureStdout(t, func() {
		if err := cmd2.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if strings.Contains(output2, "ORPHANED UID DETECTED") {
		t.Errorf("info must not warn when the daemon reports no orphan, got:\n%s", output2)
	}
}

// writeInfoConfig mirrors the info tests' config writer.
func writeInfoConfig(t *testing.T, serverURL string) {
	t.Helper()
	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers:      map[string]ServerEntry{"test": {URL: serverURL}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}
}

// listOrphanMock returns the orphan detail on one listed agent.
type listOrphanMockServer struct {
	mockBunkerdServer
	agents []*v1.AgentSummary
}

func (m *listOrphanMockServer) ListAgents(ctx context.Context, req *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	return connect.NewResponse(&v1.ListAgentsResponse{Agents: m.agents, TotalCount: uint32(len(m.agents))}), nil
}

// TestListCommand_OrphanUIDWarning proves the list surface renders the
// orphan-uid warning under the table when the daemon reports one, and
// nothing when it reports none.
func TestListCommand_OrphanUIDWarning(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "default")
	t.Setenv("HOME", t.TempDir())

	orphan := "ORPHANED UID: user record bunker-zomb is GONE from the host but 1 live process(es) under uid 1002: pid 1189: node duckbrain.js"
	mock := &listOrphanMockServer{
		agents: []*v1.AgentSummary{
			{AgentId: "zomb", Status: "running", OrphanUidDetail: orphan},
			{AgentId: "ok-agent", Status: "running"},
		},
	}
	srv := newListTestServer(t, mock)
	defer srv.Close()
	writeListTestConfig(t, t.TempDir(), srv.URL)

	cmd := NewListCommand()
	cmd.SetArgs([]string{"--server", "default", "--status", "all"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if !strings.Contains(output, "⚠  zomb: ORPHANED UID") || !strings.Contains(output, orphan) {
		t.Errorf("list output missing the orphan warning, got:\n%s", output)
	}
	// The healthy agent contributed no warning.
	if strings.Count(output, "ORPHANED UID") != 1 {
		t.Errorf("expected exactly one orphan warning block, got:\n%s", output)
	}
}
