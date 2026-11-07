package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// netmodeSpawnMock is a fake daemon for the INT-CI-020 gate: it serves
// ServerInfo (the capability probe), SpawnAgent, GetAgent (the post-spawn
// read-back) and DestroyAgent (the fail-closed teardown), recording what the
// CLI actually sent and destroyed.
type netmodeSpawnMock struct {
	mockBunkerdServer

	spawnResp *v1.SpawnAgentResponse

	gotAgentID     string
	gotNetworkMode string
	spawnCalls     int

	// getAgent is what the daemon's durable record reports for the agent.
	getAgent    *v1.AgentSummary
	getAgentErr error

	destroyed  []string
	destroyErr error
}

func (m *netmodeSpawnMock) SpawnAgent(_ context.Context, req *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error) {
	m.spawnCalls++
	m.gotAgentID = req.Msg.GetAgentId()
	m.gotNetworkMode = req.Msg.GetNetworkMode()
	return connect.NewResponse(m.spawnResp), nil
}

func (m *netmodeSpawnMock) GetAgent(_ context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
	if m.getAgentErr != nil {
		return nil, m.getAgentErr
	}
	if m.getAgent == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.GetAgentId()))
	}
	return connect.NewResponse(&v1.GetAgentResponse{Agent: m.getAgent}), nil
}

func (m *netmodeSpawnMock) DestroyAgent(_ context.Context, req *connect.Request[v1.DestroyAgentRequest]) (*connect.Response[v1.DestroyAgentResponse], error) {
	if m.destroyErr != nil {
		return nil, m.destroyErr
	}
	m.destroyed = append(m.destroyed, req.Msg.GetAgentId())
	return connect.NewResponse(&v1.DestroyAgentResponse{}), nil
}

// TestEnsureDaemonSupportsNetworkMode_Table drives the client-side capability
// gate (INT-CI-020) against fake ServerInfo shapes. This is the arm that closes
// the reported defect for an ALREADY-DEPLOYED old daemon (bunker-las-03 at
// 0.1.4): the pre-fix CLI sent the flag, the daemon ignored the unknown proto
// field, exit code was 0 and the agent ran with full egress.
func TestEnsureDaemonSupportsNetworkMode_Table(t *testing.T) {
	tests := []struct {
		name       string
		info       *v1.ServerInfoResponse
		infoErr    error
		mode       string
		wantRefuse bool
		refuseHas  []string
	}{
		{
			// The measured defect: a 0.1.x daemon reports ServerInfo but
			// carries no network-mode capability at all.
			name:       "pre_capability_daemon_refuses_systemd",
			info:       &v1.ServerInfoResponse{Hostname: "bunker-las-03", Version: "0.1.4"},
			mode:       "systemd",
			wantRefuse: true,
			refuseHas:  []string{"--network-mode", "systemd", "predates network-isolation enforcement", "silently ignore", "HOST network namespace", "Upgrade bunkerd"},
		},
		{
			name:       "daemon_missing_requested_mode_refuses",
			info:       &v1.ServerInfoResponse{NetworkModesSupported: []string{"shared"}},
			mode:       "systemd",
			wantRefuse: true,
			refuseHas:  []string{"does not report support", "shared"},
		},
		{
			name:       "supported_systemd_proceeds",
			info:       &v1.ServerInfoResponse{NetworkModesSupported: []string{"shared", "systemd", "procvis"}},
			mode:       "systemd",
			wantRefuse: false,
		},
		{
			// hidepid is the config-facing spelling for procvis: it must match
			// the canonical report, or the alias is mistaken for a mismatch.
			name:       "hidepid_alias_matches_canonical_procvis",
			info:       &v1.ServerInfoResponse{NetworkModesSupported: []string{"shared", "systemd", "procvis"}},
			mode:       "hidepid",
			wantRefuse: false,
		},
		{
			// "shared" is inert: it asks for no isolation, so a pre-capability
			// daemon behaves exactly like a current one. Refusing here would
			// break every pre-existing invocation shape.
			name:       "shared_needs_no_capability",
			info:       &v1.ServerInfoResponse{Hostname: "old"},
			mode:       "shared",
			wantRefuse: false,
		},
		{
			// Empty mode defers to the daemon's resolution chain.
			name:       "empty_mode_defers_to_daemon",
			info:       &v1.ServerInfoResponse{Hostname: "old"},
			mode:       "",
			wantRefuse: false,
		},
		{
			// A daemon we cannot interrogate is not assumed either way.
			name:       "unreachable_daemon_fails_closed",
			infoErr:    connect.NewError(connect.CodeUnavailable, nil),
			mode:       "systemd",
			wantRefuse: true,
			refuseHas:  []string{"capability probe failed", "could not be verified"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := ServerEntry{URL: "http://127.0.0.1:1", Token: "t"}
			if tt.infoErr == nil {
				mock := &netmodeSpawnMock{mockBunkerdServer: mockBunkerdServer{info: tt.info}}
				srv := newSpawnTestServer(t, mock)
				defer srv.Close()
				entry.URL = srv.URL
			}

			err := ensureDaemonSupportsNetworkMode(tt.mode, entry)
			if got := err != nil; got != tt.wantRefuse {
				t.Fatalf("ensureDaemonSupportsNetworkMode(%q) err = %v, wantRefuse=%v", tt.mode, err, tt.wantRefuse)
			}
			for _, want := range tt.refuseHas {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q missing %q", err.Error(), want)
				}
			}
		})
	}
}

// TestVerifySpawnedNetworkMode_Table pins the POST-spawn read-back contract
// (spec §5.2 "requested and provided are different fields"): the mode the
// daemon recorded must equal the canonical requested mode, carry a boundary,
// and carry the daemon's read-back verdict.
func TestVerifySpawnedNetworkMode_Table(t *testing.T) {
	const wantBoundary = "private network namespace (loopback only)"

	tests := []struct {
		name     string
		request  string
		ni       *v1.NetworkIsolation
		wantErr  []string
		wantOKAY bool
	}{
		{
			name:     "systemd_reported_verified_is_accepted",
			request:  "systemd",
			ni:       &v1.NetworkIsolation{Mode: "systemd", Boundary: wantBoundary, Verified: true},
			wantOKAY: true,
		},
		{
			// The measured false cut: on a daemon that ignores the proto field
			// the record carries no network-isolation state at all.
			name:    "no_recorded_mode_is_refused",
			request: "systemd",
			ni:      nil,
			wantErr: []string{"no enforced mode", "NOT verified"},
		},
		{
			name:    "empty_recorded_mode_is_refused",
			request: "systemd",
			ni:      &v1.NetworkIsolation{},
			wantErr: []string{"no enforced mode"},
		},
		{
			name:    "silent_fallback_to_shared_is_refused",
			request: "systemd",
			ni:      &v1.NetworkIsolation{Mode: "shared", Boundary: "host network namespace", Verified: false},
			wantErr: []string{`"systemd"`, `"shared"`, "silent fallback"},
		},
		{
			name:    "mode_without_boundary_is_refused",
			request: "systemd",
			ni:      &v1.NetworkIsolation{Mode: "systemd", Verified: true},
			wantErr: []string{"NO boundary line"},
		},
		{
			// Mode + boundary but no read-back verdict: the daemon accepted the
			// flag and never proved it, which is exactly what INT-CI-020 is.
			name:    "unverified_readback_is_refused",
			request: "systemd",
			ni:      &v1.NetworkIsolation{Mode: "systemd", Boundary: wantBoundary, Verified: false},
			wantErr: []string{"did not read the enforced unit property back", "NOT verified"},
		},
		{
			name:     "hidepid_alias_matches_reported_procvis",
			request:  "hidepid",
			ni:       &v1.NetworkIsolation{Mode: "procvis", Boundary: "private /proc", Verified: true},
			wantOKAY: true,
		},
		{
			// "shared" asks for nothing, so there is nothing to hold the
			// daemon to (and an old daemon must keep working for it).
			name:     "shared_is_not_gated",
			request:  "shared",
			ni:       nil,
			wantOKAY: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifySpawnedNetworkMode(tt.request, tt.ni)
			if tt.wantOKAY {
				if err != nil {
					t.Fatalf("verifySpawnedNetworkMode(%q, %v) = %v, want nil", tt.request, tt.ni, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("verifySpawnedNetworkMode(%q, %v) = nil, want a refusal", tt.request, tt.ni)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q missing %q", err.Error(), want)
				}
			}
		})
	}
}

// TestSpawnCommand_NetworkModeRefusedOnPreCapabilityDaemon is the end-to-end
// red-proof for the reported defect: `bunker spawn --network-mode systemd`
// against a fake 0.1.x daemon (ServerInfo without the network capability) must
// FAIL LOUDLY client-side, and the SpawnAgent RPC must never reach the wire.
// Before this row's fix the same invocation created the agent and exited 0.
func TestSpawnCommand_NetworkModeRefusedOnPreCapabilityDaemon(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &netmodeSpawnMock{
		mockBunkerdServer: mockBunkerdServer{
			// bunker-las-03's shape: the daemon answers, and reports nothing
			// about network-isolation enforcement.
			info: &v1.ServerInfoResponse{Hostname: "bunker-las-03", Version: "0.1.4"},
		},
		spawnResp: &v1.SpawnAgentResponse{AgentId: "never-spawned"},
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()
	writeSpawnTestConfig(t, tmpDir, srv.URL)

	cmd := NewSpawnCommand()
	cmd.SetArgs([]string{"--network-mode", "systemd", "probe1"})
	runErr := cmd.Execute()

	if runErr == nil {
		t.Fatal("spawn with --network-mode systemd against a pre-capability daemon must fail, got nil error")
	}
	for _, want := range []string{"predates network-isolation enforcement", "systemd", "silently ignore"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("refusal %q missing %q", runErr.Error(), want)
		}
	}
	if mock.spawnCalls != 0 {
		t.Errorf("SpawnAgent RPC reached a pre-capability daemon %d time(s) (agentId=%q) — the refusal must fire BEFORE any agent exists", mock.spawnCalls, mock.gotAgentID)
	}
}

// TestSpawnCommand_NetworkModeVerifiedOnCapableDaemon is the positive arm: a
// daemon that reports the vocabulary AND records a verified boundary spawns
// normally, forwards the mode verbatim, prints the boundary marker, and never
// destroys the agent.
func TestSpawnCommand_NetworkModeVerifiedOnCapableDaemon(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &netmodeSpawnMock{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname:              "new-bunkerd",
				Version:               "v0.2.0",
				NetworkModesSupported: []string{"shared", "systemd", "procvis"},
			},
		},
		spawnResp: &v1.SpawnAgentResponse{
			AgentId:       "nm123",
			DockerHostSsh: "DOCKER_HOST=ssh://nm@host",
			SshPrivateKey: "test-private-key-data",
		},
		getAgent: &v1.AgentSummary{
			AgentId:          "nm123",
			NetworkIsolation: &v1.NetworkIsolation{Mode: "systemd", Boundary: "private network namespace (loopback only)", Verified: true},
		},
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()
	writeSpawnTestConfig(t, tmpDir, srv.URL)

	cmd := NewSpawnCommand()
	cmd.SetArgs([]string{"--network-mode", "systemd", "nm123"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "nm123") {
		t.Errorf("output missing the spawned agent id, got:\n%s", output)
	}
	if !strings.Contains(output, "Network Mode: systemd — boundary VERIFIED") {
		t.Errorf("output missing the INT-CI-020 boundary marker, got:\n%s", output)
	}
	if mock.gotNetworkMode != "systemd" {
		t.Errorf("SpawnAgent got network mode %q, want \"systemd\" forwarded verbatim", mock.gotNetworkMode)
	}
	if len(mock.destroyed) != 0 {
		t.Errorf("a verified spawn destroyed the agent: %v", mock.destroyed)
	}
}

// TestSpawnCommand_NetworkModeUnverifiedReadbackDestroysAgent is the fail-closed
// arm for the OTHER half of the defect: the daemon accepts the flag but its
// record shows no verified boundary. The CLI must destroy the agent it just
// created and fail — a refused spawn may not leave an uncontained agent running.
func TestSpawnCommand_NetworkModeUnverifiedReadbackDestroysAgent(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &netmodeSpawnMock{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname:              "half-fixed-bunkerd",
				Version:               "v0.2.0",
				NetworkModesSupported: []string{"shared", "systemd"},
			},
		},
		spawnResp: &v1.SpawnAgentResponse{
			AgentId:       "nm456",
			DockerHostSsh: "DOCKER_HOST=ssh://nm@host",
		},
		// The record carries the mode but NOT the read-back verdict: the daemon
		// accepted the flag and never proved it was applied.
		getAgent: &v1.AgentSummary{
			AgentId:          "nm456",
			NetworkIsolation: &v1.NetworkIsolation{Mode: "systemd", Boundary: "private network namespace (loopback only)"},
		},
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()
	writeSpawnTestConfig(t, tmpDir, srv.URL)

	cmd := NewSpawnCommand()
	cmd.SetArgs([]string{"--network-mode", "systemd", "nm456"})
	var runErr error
	captureStdout(t, func() { runErr = cmd.Execute() })

	if runErr == nil {
		t.Fatal("spawn whose boundary did not verify must fail, got nil error")
	}
	for _, want := range []string{"did NOT verify", "destroyed", "no uncontained agent"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("refusal %q missing %q", runErr.Error(), want)
		}
	}
	if len(mock.destroyed) != 1 || mock.destroyed[0] != "nm456" {
		t.Errorf("destroyed = %v, want exactly [nm456] — an unverified agent must never be left running", mock.destroyed)
	}
}

// TestSpawnCommand_NetworkModeRecordUnreadableDestroysAgent covers the read-back
// transport failure: when GetAgent itself fails the boundary is UNKNOWN, not
// fine, so the agent is destroyed and the refusal names the read-back.
func TestSpawnCommand_NetworkModeRecordUnreadableDestroysAgent(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &netmodeSpawnMock{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname:              "new-bunkerd",
				Version:               "v0.2.0",
				NetworkModesSupported: []string{"shared", "systemd"},
			},
		},
		spawnResp:   &v1.SpawnAgentResponse{AgentId: "nm789", DockerHostSsh: "DOCKER_HOST=ssh://nm@host"},
		getAgentErr: connect.NewError(connect.CodeUnavailable, fmt.Errorf("connection reset")),
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()
	writeSpawnTestConfig(t, tmpDir, srv.URL)

	cmd := NewSpawnCommand()
	cmd.SetArgs([]string{"--network-mode", "systemd", "nm789"})
	var runErr error
	captureStdout(t, func() { runErr = cmd.Execute() })

	if runErr == nil {
		t.Fatal("spawn whose read-back could not be read must fail, got nil error")
	}
	if !strings.Contains(runErr.Error(), "read-back") {
		t.Errorf("refusal %q must name the read-back", runErr.Error())
	}
	if len(mock.destroyed) != 1 || mock.destroyed[0] != "nm789" {
		t.Errorf("destroyed = %v, want exactly [nm789]", mock.destroyed)
	}
}

// TestSpawnCommand_NoNetworkModeFlagUnchanged pins the inert path: a spawn
// WITHOUT --network-mode against an old daemon keeps working byte-identically
// (no capability probe refusal, no read-back RPC, no bundle marker) — the fix
// must not break pre-existing invocations.
func TestSpawnCommand_NoNetworkModeFlagUnchanged(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &netmodeSpawnMock{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{Hostname: "bunker-las-03", Version: "0.1.4"},
		},
		spawnResp: &v1.SpawnAgentResponse{
			AgentId:       "plain1",
			DockerHostSsh: "DOCKER_HOST=ssh://nm@host",
			SshPrivateKey: "test-private-key-data",
		},
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()
	writeSpawnTestConfig(t, tmpDir, srv.URL)

	cmd := NewSpawnCommand()
	cmd.SetArgs([]string{"plain1"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if mock.spawnCalls != 1 {
		t.Errorf("SpawnAgent calls = %d, want 1", mock.spawnCalls)
	}
	if strings.Contains(output, "Network Mode:") {
		t.Errorf("a spawn without --network-mode must not print the boundary marker, got:\n%s", output)
	}
	if len(mock.destroyed) != 0 {
		t.Errorf("the inert path destroyed an agent: %v", mock.destroyed)
	}
}
