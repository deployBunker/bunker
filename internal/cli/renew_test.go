package cli

// DF-BUNKER-34 CLI contract tests: the renew command's identity gate, and
// the orphan-uid warning blocks on info/list.

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// renewMockServer implements BunkerdHandler with canned GetAgent / Destroy /
// Spawn responses for the renew flow. DF-BUNKER-70: RenewalDriftReport answers
// a benign empty report by default; driftErr makes it FAIL (an old daemon
// answers CodeUnimplemented, which the command now treats as a hard error).
type renewMockServer struct {
	mockBunkerdServer
	agent          *v1.AgentSummary
	destroyStatus  string
	spawnAgentID   string
	destroyErr     error
	spawnErr       error
	destroyCalled  bool
	spawnRequested *v1.SpawnAgentRequest
	// DF-BUNKER-70: the drift pre-flight seam. driftErr (when set) is
	// returned verbatim — CodeUnimplemented models a daemon that predates
	// the RPC. driftCalled/gotDriftRequest record whether the CLI asked at
	// all, so --skip-drift-check can be proven to make NO RPC.
	driftErr        error
	driftResp       *v1.RenewalDriftResponse
	driftCalled     bool
	gotDriftRequest *v1.RenewalDriftRequest
	// DF-BUNKER-65: GetAgentKey plumbing, mirroring mockSpawnServer's fake.
	// gotKeyRequested/gotKeyAgentID record whether (and for whom) the CLI
	// fetched the key after the re-spawn; keyResp/keyErr control the fake
	// response. When both are unset the mock answers CodeNotFound.
	gotKeyRequested bool
	gotKeyAgentID   string
	keyResp         *v1.GetAgentKeyResponse
	keyErr          error
	// DF-BUNKER-68: the drift pre-flight is what the --no-destroy
	// (report-only) arm exists for, so the mock can serve a real summary
	// instead of the historical CodeUnimplemented warning.
	driftSummary string
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
	m.driftCalled = true
	m.gotDriftRequest = req.Msg
	if m.driftErr != nil {
		return nil, m.driftErr
	}
	if m.driftResp != nil {
		return connect.NewResponse(m.driftResp), nil
	}
	if m.driftSummary != "" {
		return connect.NewResponse(&v1.RenewalDriftResponse{
			AgentId: req.Msg.GetAgentId(),
			OldHome: req.Msg.GetOldHome(),
			Summary: m.driftSummary,
		}), nil
	}
	return connect.NewResponse(&v1.RenewalDriftResponse{
		AgentId: req.Msg.GetAgentId(),
		OldHome: req.Msg.GetOldHome(),
		Summary: "drift pre-flight: 0 stale references",
	}), nil
}

// GetAgentKey fakes the GAP-128 key-retrieval RPC for the renew re-spawn leg
// (DF-BUNKER-65), recording the request like mockSpawnServer's fake does.
func (m *renewMockServer) GetAgentKey(
	ctx context.Context,
	req *connect.Request[v1.GetAgentKeyRequest],
) (*connect.Response[v1.GetAgentKeyResponse], error) {
	m.gotKeyRequested = true
	m.gotKeyAgentID = req.Msg.GetAgentId()
	if m.keyErr != nil {
		return nil, m.keyErr
	}
	if m.keyResp != nil {
		return connect.NewResponse(m.keyResp), nil
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.GetAgentId()))
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
			"default": {Name: "default", URL: serverURL, Token: "test-cli-token", ConnectedAt: "2026-06-28T00:00:00Z"},
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
	for _, want := range []string{"--agent-id", "re-spawning the SAME agent id", "docs/renewal.md",
		// DF-BUNKER-68: the report-only arm must be discoverable.
		"--no-destroy", "report-only"} {
		if !strings.Contains(output, want) {
			t.Errorf("help output missing %q, got:\n%s", want, output)
		}
	}
}

// ── DF-BUNKER-68: failed re-spawn after a successful destroy ───────────────

// captureStderrRenew captures os.Stderr while fn runs (cobra prints the
// Execute error there, which is where an operator sees the recovery hint).
func captureStderrRenew(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = old
	var buf strings.Builder
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return buf.String()
}

// TestRenew_RespawnFailure_Hint is the row's core contract: when the destroy
// leg succeeds and the re-spawn leg FAILS, the agent is already gone, so the
// failure must ship its own recovery path — the 'bunker spawn <same-id>'
// command and the pre-renewal archive reminder — instead of naming only the
// spawn stage (the state that left an operator with a destroyed agent and no
// hint on 2026-09-25).
func TestRenew_RespawnFailure_Hint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mock := &renewMockServer{
		agent: &v1.AgentSummary{
			AgentId:    "eduos-agent",
			SshfsMount: "sshfs -o IdentityFile=/keys/eduos-agent bunker-eduos-agent@host:/home/bunker-eduos-agent /mnt/bunker/eduos-agent",
		},
		destroyStatus: "destroyed",
		spawnErr:      connect.NewError(connect.CodeInternal, fmt.Errorf("install failed: node missing")),
	}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	var output, errOut string
	var execErr error
	errOut = captureStderrRenew(t, func() {
		output, execErr = runRenew(t, srv, "--agent-id", "eduos-agent", "--ttl", "7d")
	})

	if execErr == nil {
		t.Fatal("a failed re-spawn leg must fail the renewal")
	}
	// The destroy ran and the re-spawn was attempted: this is the destroyed-
	// without-respawn state, not an aborted-before-destroy renewal.
	if !mock.destroyCalled {
		t.Error("the mock never saw the destroy leg run")
	}
	if mock.spawnRequested == nil {
		t.Error("the renewal never attempted the re-spawn leg")
	}
	if !strings.Contains(output, "Re-spawning agent eduos-agent (stable identity)") {
		t.Errorf("stdout missing the re-spawn step, got:\n%s", output)
	}

	// The verdict line the row specifies, naming the SAME stable id.
	for _, want := range []string{
		"renew failed: agent destroyed but re-spawn failed; run 'bunker spawn eduos-agent' to recover",
		"agent eduos-agent and its home /home/bunker-eduos-agent are GONE",
		"bunker spawn eduos-agent --ttl 7d", // the retry command, same id
		"docs/renewal.md",                   // the archive reminder's recipe pointer
		"restore it into /home/bunker-eduos-agent",
		"re-spawn failure", "install failed: node missing", // the cause is preserved
	} {
		if !strings.Contains(execErr.Error(), want) {
			t.Errorf("renewal error missing %q, got:\n%v", want, execErr)
		}
	}

	// And the operator sees it on stderr (cobra prints the Execute error),
	// not only in the returned value.
	for _, want := range []string{
		"agent destroyed but re-spawn failed",
		"bunker spawn eduos-agent",
		"docs/renewal.md",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q, got:\n%s", want, errOut)
		}
	}
}

// TestRenew_NoDestroyFlag pins the report-only arm: --no-destroy runs ONLY
// the drift pre-flight — no destroy, no re-spawn, no agent change on the host
// — and hands back the full renewal command to run once the drift is
// resolved. The negative arm (same mock, flag omitted) proves the flag is
// what gates the destructive legs.
func TestRenew_NoDestroyFlag(t *testing.T) {
	const drift = "2 artifacts reference /home/bunker-eduos-agent: /home/bunker-eduos-agent/.config/systemd/user/duckbrain-local.service:5"

	baseMock := func() *renewMockServer {
		return &renewMockServer{
			agent: &v1.AgentSummary{
				AgentId:    "eduos-agent",
				SshfsMount: "sshfs -o IdentityFile=/keys/eduos-agent bunker-eduos-agent@host:/home/bunker-eduos-agent /mnt/bunker/eduos-agent",
			},
			destroyStatus: "destroyed",
			spawnAgentID:  "eduos-agent",
			driftSummary:  drift,
		}
	}

	// Arm 1: --no-destroy leaves the agent alone.
	t.Setenv("HOME", t.TempDir())
	mock := baseMock()
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	output, execErr := runRenew(t, srv, "--agent-id", "eduos-agent", "--ttl", "7d", "--no-destroy")
	if execErr != nil {
		t.Fatalf("--no-destroy must succeed as a report-only run, got: %v", execErr)
	}
	if !mock.driftCalled {
		t.Error("--no-destroy did not run the drift pre-flight (that IS the report-only arm)")
	}
	if mock.destroyCalled {
		t.Error("--no-destroy destroyed the agent")
	}
	if mock.spawnRequested != nil {
		t.Error("--no-destroy re-spawned the agent")
	}
	for _, want := range []string{
		"report-only",
		"nothing was destroyed",
		drift,          // the drift report itself
		"left RUNNING", // the agent-state statement
		"bunker renew --agent-id eduos-agent --ttl 7d", // the pointer back to the destructive arm
		"docs/renewal.md",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("--no-destroy output missing %q, got:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Destroying agent") {
		t.Errorf("--no-destroy must not announce a destroy, got:\n%s", output)
	}

	// Arm 2 (the control): the same invocation WITHOUT the flag destroys and
	// re-spawns — proving the flag, not the fixture, gates the destructive
	// legs.
	t.Setenv("HOME", t.TempDir())
	ctrl := baseMock()
	srv2 := newRenewTestServer(t, ctrl)
	defer srv2.Close()

	output2, execErr2 := runRenew(t, srv2, "--agent-id", "eduos-agent", "--ttl", "7d")
	if execErr2 != nil {
		t.Fatalf("control renewal failed: %v", execErr2)
	}
	if !ctrl.destroyCalled || ctrl.spawnRequested == nil {
		t.Fatalf("control run must destroy + re-spawn (destroy=%v spawn=%v)", ctrl.destroyCalled, ctrl.spawnRequested != nil)
	}
	if !strings.Contains(output2, "Renewed: agent eduos-agent") {
		t.Errorf("control run missing the success line, got:\n%s", output2)
	}

	// Arm 3: an unregistered id under --no-destroy reports "no record", never
	// a clean "no drift" — absence must not read as health.
	t.Setenv("HOME", t.TempDir())
	unknown := &renewMockServer{driftSummary: drift}
	srv3 := newRenewTestServer(t, unknown)
	defer srv3.Close()

	output3, execErr3 := runRenew(t, srv3, "--agent-id", "ghost-agent", "--no-destroy")
	if execErr3 != nil {
		t.Fatalf("--no-destroy on an unregistered id must not fail, got: %v", execErr3)
	}
	if unknown.destroyCalled || unknown.spawnRequested != nil {
		t.Error("--no-destroy on an unregistered id must not destroy or spawn")
	}
	for _, want := range []string{"not registered", `"no record", not "no drift"`} {
		if !strings.Contains(output3, want) {
			t.Errorf("--no-destroy output missing %q for an unregistered id, got:\n%s", want, output3)
		}
	}
	if strings.Contains(output3, "left RUNNING") {
		t.Errorf("an unregistered id must not be reported as left running, got:\n%s", output3)
	}
}

// ── DF-BUNKER-65: the re-spawn leg must re-fetch the agent SSH key ──────────

// TestRenewCommand_RespawnFetchesAndSavesAgentKey proves the renewal's
// re-spawn leg goes through the same GAP-128 key path spawn does: after a
// successful SpawnAgent, the CLI calls GetAgentKey for the stable id and
// writes the returned key to ~/.bunker/keys/<agent-id> at 0600 — otherwise
// every SSH-family verb dies on auth the moment a renewal completes, because
// the local file still holds the DESTROYED agent's key.
func TestRenewCommand_RespawnFetchesAndSavesAgentKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &renewMockServer{
		agent: &v1.AgentSummary{
			AgentId:    "eduos-agent",
			SshfsMount: "sshfs -o IdentityFile=/keys/eduos-agent bunker-eduos-agent@host:/home/bunker-eduos-agent /mnt/bunker/eduos-agent",
		},
		destroyStatus: "destroyed",
		spawnAgentID:  "eduos-agent",
		keyResp: &v1.GetAgentKeyResponse{
			AgentId:       "eduos-agent",
			SshPrivateKey: "renewed-private-key-data",
		},
	}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	output, execErr := runRenew(t, srv, "--agent-id", "eduos-agent")
	if execErr != nil {
		t.Fatalf("renew failed: %v", execErr)
	}

	// The fetch must have gone through GetAgentKey for the stable id.
	if !mock.gotKeyRequested {
		t.Fatal("renew did not call GetAgentKey after the re-spawn (DF-BUNKER-65)")
	}
	if mock.gotKeyAgentID != "eduos-agent" {
		t.Errorf("GetAgentKey asked for agent %q, want eduos-agent", mock.gotKeyAgentID)
	}

	// The key file under the test HOME must carry the fetched key at 0600.
	keyPath := filepath.Join(tmpDir, ".config", "bunker", "keys", "eduos-agent")
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("client-local key not written after renewal: %v", err)
	}
	if string(raw) != "renewed-private-key-data" {
		t.Errorf("local key content %q, want the key returned by GetAgentKey", raw)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("local key mode %v, want 0600", got)
	}

	// The renewal output mentions the saved key and still reports success.
	for _, want := range []string{"SSH Key:", "(saved to ~/.bunker/keys/)", "Renewed: agent eduos-agent"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
}

// TestRenewCommand_KeyFetchFailureStillRenews proves the key fetch is
// NON-FATAL on the renewal path: a daemon that cannot serve the key (an old
// daemon, an RPC failure) still completes the renewal — the agent is running,
// and key recovery must not abort the reported success.
func TestRenewCommand_KeyFetchFailureStillRenews(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mock := &renewMockServer{
		agent:         &v1.AgentSummary{AgentId: "eduos-agent"},
		destroyStatus: "destroyed",
		spawnAgentID:  "eduos-agent",
		keyErr:        connect.NewError(connect.CodeInternal, fmt.Errorf("key store unavailable")),
	}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	output, execErr := runRenew(t, srv, "--agent-id", "eduos-agent")
	if execErr != nil {
		t.Fatalf("renewal must still succeed when the key fetch fails, got: %v", execErr)
	}
	if !mock.gotKeyRequested {
		t.Fatal("renew did not attempt GetAgentKey")
	}
	if !strings.Contains(output, "(warn: could not fetch SSH key:") {
		t.Errorf("output missing the key-fetch warn line, got:\n%s", output)
	}
	if !strings.Contains(output, "Renewed: agent eduos-agent") {
		t.Errorf("output missing the success line, got:\n%s", output)
	}
}

// TestRenewCommand_EmptyKeyWritesNothing proves the empty-key contract: an
// GetAgentKey answer with NO key material writes no file and prints no key
// line, but the renewal itself still completes.
func TestRenewCommand_EmptyKeyWritesNothing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &renewMockServer{
		agent:         &v1.AgentSummary{AgentId: "eduos-agent"},
		destroyStatus: "destroyed",
		spawnAgentID:  "eduos-agent",
		keyResp:       &v1.GetAgentKeyResponse{AgentId: "eduos-agent"},
	}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	output, execErr := runRenew(t, srv, "--agent-id", "eduos-agent")
	if execErr != nil {
		t.Fatalf("renew failed: %v", execErr)
	}
	if !mock.gotKeyRequested {
		t.Fatal("renew did not attempt GetAgentKey")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, ".config", "bunker", "keys", "eduos-agent")); err == nil {
		t.Error("an empty GetAgentKey answer must not write a key file")
	}
	if strings.Contains(output, "SSH Key:") || strings.Contains(output, "(saved to ~/.bunker/keys/)") {
		t.Errorf("output must not carry a key line for an empty key, got:\n%s", output)
	}
	if !strings.Contains(output, "Renewed: agent eduos-agent") {
		t.Errorf("output missing the success line, got:\n%s", output)
	}
}

// ── DF-BUNKER-70: the drift pre-flight is a GATE, not a courtesy ────────────
//
// Before this row a failed pre-flight printed one warn line and the renewal
// continued: on a daemon that predates RenewalDriftReport the agent was
// destroyed and re-spawned with NO stale-reference report at all — the exact
// outcome the pre-flight exists to prevent. The pre-flight now aborts the
// renewal before the destroy; --skip-drift-check bypasses it deliberately and
// --force overrides a failed check for emergency renewals.

// driftAgent is an agent whose stored sshfs mount yields a parsable OLD home
// (/home/bunker-eduos-agent), which is what makes the pre-flight run at all.
func driftAgent() *v1.AgentSummary {
	return &v1.AgentSummary{
		AgentId:    "eduos-agent",
		SshfsMount: "sshfs -o IdentityFile=/keys/eduos-agent bunker-eduos-agent@host:/home/bunker-eduos-agent /mnt/bunker/eduos-agent",
	}
}

// TestRenew_DriftCheckUnsupported_Error proves the row's first criterion: a
// daemon that does not support the drift-check RPC makes the renewal FAIL
// with a clear error BEFORE anything is destroyed — no warn line and a
// completed renewal.
func TestRenew_DriftCheckUnsupported_Error(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mock := &renewMockServer{
		agent:         driftAgent(),
		destroyStatus: "destroyed",
		spawnAgentID:  "eduos-agent",
		driftErr:      connect.NewError(connect.CodeUnimplemented, fmt.Errorf("renewal drift report is not available on this daemon")),
	}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	output, execErr := runRenew(t, srv, "--agent-id", "eduos-agent")
	if execErr == nil {
		t.Fatalf("renew must FAIL when the daemon cannot serve the drift pre-flight; output:\n%s", output)
	}
	for _, want := range []string{
		"ABORTED before destroying \"eduos-agent\"",
		"does not support the renewal drift-check RPC",
		"/home/bunker-eduos-agent",
		"Nothing was destroyed",
		"--skip-drift-check",
		"--force",
	} {
		if !strings.Contains(execErr.Error(), want) {
			t.Errorf("abort error missing %q — got: %v", want, execErr)
		}
	}

	// The gate must come BEFORE the destroy: nothing destroyed, nothing spawned.
	if mock.destroyCalled {
		t.Error("the renewal destroyed the agent despite the unsupported drift pre-flight")
	}
	if mock.spawnRequested != nil {
		t.Error("the renewal re-spawned the agent despite the unsupported drift pre-flight")
	}
	// The old degraded behavior (warn + continue) must be gone.
	if strings.Contains(output, "drift pre-flight unavailable") {
		t.Errorf("the degraded one-line warn is still emitted:\n%s", output)
	}
	if strings.Contains(output, "Renewed: agent eduos-agent") {
		t.Errorf("renewal reported success despite the unsupported drift pre-flight:\n%s", output)
	}
}

// TestRenew_SkipDriftCheckFlag proves --skip-drift-check bypasses the gate
// DELIBERATELY: the command makes no RenewalDriftReport RPC at all, says so,
// and completes the renewal on a daemon that cannot serve the check.
func TestRenew_SkipDriftCheckFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mock := &renewMockServer{
		agent:         driftAgent(),
		destroyStatus: "destroyed",
		spawnAgentID:  "eduos-agent",
		driftErr:      connect.NewError(connect.CodeUnimplemented, fmt.Errorf("renewal drift report is not available on this daemon")),
	}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	output, execErr := runRenew(t, srv, "--agent-id", "eduos-agent", "--skip-drift-check")
	if execErr != nil {
		t.Fatalf("--skip-drift-check must let the renewal proceed, got: %v", execErr)
	}
	if mock.driftCalled {
		t.Error("--skip-drift-check must NOT issue the RenewalDriftReport RPC")
	}
	for _, want := range []string{
		"SKIPPED (--skip-drift-check)",
		"/home/bunker-eduos-agent",
		"Renewed: agent eduos-agent",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	if !mock.destroyCalled {
		t.Error("the renewal never destroyed the old instance")
	}
	if mock.spawnRequested == nil || mock.spawnRequested.GetAgentId() != "eduos-agent" {
		t.Error("the renewal never re-spawned the stable agent id")
	}
}

// TestRenew_ForceFlag proves --force is the emergency override: the pre-flight
// still runs (and still fails on an old daemon), the failure is REPORTED with
// the --force marker, and the renewal proceeds to destroy + re-spawn.
func TestRenew_ForceFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mock := &renewMockServer{
		agent:         driftAgent(),
		destroyStatus: "destroyed",
		spawnAgentID:  "eduos-agent",
		driftErr:      connect.NewError(connect.CodeUnimplemented, fmt.Errorf("renewal drift report is not available on this daemon")),
	}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	output, execErr := runRenew(t, srv, "--agent-id", "eduos-agent", "--force")
	if execErr != nil {
		t.Fatalf("--force must let the renewal proceed despite the failed pre-flight, got: %v", execErr)
	}
	if !mock.driftCalled {
		t.Error("--force must still ATTEMPT the drift pre-flight (the failure has to be reported)")
	}
	for _, want := range []string{
		"(warn: --force: continuing despite a failed drift pre-flight:",
		"does not support the renewal drift-check RPC",
		"Destroying agent eduos-agent",
		"Renewed: agent eduos-agent",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	if !mock.destroyCalled {
		t.Error("--force must carry the renewal through the destroy")
	}
	if mock.spawnRequested == nil || mock.spawnRequested.GetAgentId() != "eduos-agent" {
		t.Error("--force must carry the renewal through the re-spawn of the stable id")
	}
}

// TestRenew_DriftCheckFailed_Aborts proves the gate is not special-cased to
// CodeUnimplemented: ANY failed pre-flight aborts by default (the report the
// renewal is supposed to produce is unavailable either way).
func TestRenew_DriftCheckFailed_Aborts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mock := &renewMockServer{
		agent:         driftAgent(),
		destroyStatus: "destroyed",
		spawnAgentID:  "eduos-agent",
		driftErr:      connect.NewError(connect.CodeInternal, fmt.Errorf("scan failed: no such directory")),
	}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	_, execErr := runRenew(t, srv, "--agent-id", "eduos-agent")
	if execErr == nil {
		t.Fatal("a failed drift pre-flight must abort the renewal")
	}
	for _, want := range []string{"drift pre-flight FAILED", "no such directory", "Nothing was destroyed"} {
		if !strings.Contains(execErr.Error(), want) {
			t.Errorf("abort error missing %q — got: %v", want, execErr)
		}
	}
	if mock.destroyCalled {
		t.Error("the renewal destroyed the agent despite the failed pre-flight")
	}
}

// TestRenew_DriftReportHitsStillRenew pins the OTHER half of the contract: the
// report is REPORT-ONLY. A pre-flight that SUCCEEDS and finds stale references
// must not read as a failure — the hits are printed and the renewal proceeds
// (the operator rewrites them; the command has no authority to).
func TestRenew_DriftReportHitsStillRenew(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mock := &renewMockServer{
		agent:         driftAgent(),
		destroyStatus: "destroyed",
		spawnAgentID:  "eduos-agent",
		driftResp: &v1.RenewalDriftResponse{
			AgentId:      "eduos-agent",
			OldHome:      "/home/bunker-eduos-agent",
			FilesScanned: 12,
			Summary:      "drift: 1 stale reference in 12 files scanned",
			Hits: []*v1.RenewalDriftHit{
				{File: "/home/bunker-eduos-agent/.config/systemd/user/tick.service", Line: 4, Text: "ExecStart=/home/bunker-eduos-agent/bin/tick"},
			},
		},
	}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	output, execErr := runRenew(t, srv, "--agent-id", "eduos-agent")
	if execErr != nil {
		t.Fatalf("a drift report WITH hits must not abort the renewal, got: %v", execErr)
	}
	if !strings.Contains(output, "1 stale reference in 12 files scanned") {
		t.Errorf("output missing the drift summary, got:\n%s", output)
	}
	if !strings.Contains(output, "Renewed: agent eduos-agent") {
		t.Errorf("output missing the success line, got:\n%s", output)
	}
	if !mock.destroyCalled {
		t.Error("the renewal must proceed after a successful report")
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
		Servers:      map[string]ServerEntry{"test": {URL: serverURL, Token: "test-cli-token"}},
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
