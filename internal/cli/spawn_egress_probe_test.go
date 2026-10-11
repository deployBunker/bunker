package cli

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// egressProbeCase is one row of the DF-BUNKER-82 gate table: what the
// daemon's ServerInfo reports (or how the RPC fails), the --egress-mode the
// operator asked for, and whether the gate must refuse.
type egressProbeCase struct {
	name       string
	info       *v1.ServerInfoResponse
	infoErr    error
	egressMode string
	wantRefuse bool     // gate must return an error
	refuseHas  []string // substrings the refusal must carry
}

// TestEnsureDaemonSupportsEgressMode_Table drives the client-side egress
// capability gate (DF-BUNKER-82) against fake ServerInfo shapes:
// pre-capability daemon (empty field) → refuse; vocabulary without the
// requested mode → refuse; vocabulary with the mode → proceed; inert modes
// ("open", empty) → proceed without any RPC at all; unresolvable
// ServerInfo → fail loudly rather than guess.
func TestEnsureDaemonSupportsEgressMode_Table(t *testing.T) {
	cases := []egressProbeCase{
		{
			// Empty egress_modes_supported = daemon predates the capability
			// (the proto field 13 convention). An un-gated spawn would exit
			// 0 with the agent running fully unfiltered (the exact DF-BUNKER-82
			// silent-degrade defect).
			name:       "pre_capability_daemon_refuses_enforced_mode",
			info:       &v1.ServerInfoResponse{Hostname: "old", Version: "v0.1.x"},
			egressMode: "none",
			wantRefuse: true,
			refuseHas:  []string{"--egress-mode", "none", "predates egress enforcement", "silently ignore", "Upgrade the daemon"},
		},
		{
			name:       "daemon_missing_requested_mode_refuses",
			info:       &v1.ServerInfoResponse{EgressModesSupported: []string{"open", "allowlist"}},
			egressMode: "none",
			wantRefuse: true,
			refuseHas:  []string{"does not report support", "open, allowlist"},
		},
		{
			name:       "supported_none_proceeds",
			info:       &v1.ServerInfoResponse{EgressModesSupported: []string{"open", "allowlist", "none"}},
			egressMode: "none",
			wantRefuse: false,
		},
		{
			name:       "supported_allowlist_proceeds",
			info:       &v1.ServerInfoResponse{EgressModesSupported: []string{"open", "allowlist", "none"}},
			egressMode: "allowlist",
			wantRefuse: false,
		},
		{
			// "open" is inert (no firewall command is ever invoked), so even
			// a pre-capability daemon behaves identically for it — the gate
			// must not break pre-existing invocation shapes (AC1 scopes the
			// refusal to ENFORCED modes).
			name:       "open_mode_needs_no_capability",
			info:       &v1.ServerInfoResponse{Hostname: "old"},
			egressMode: "open",
			wantRefuse: false,
		},
		{
			// Empty mode defers to the daemon's own resolution chain — the
			// daemon enforces whatever its config says, so there is nothing
			// for the client to second-guess.
			name:       "empty_mode_defers_to_daemon_no_refusal",
			info:       &v1.ServerInfoResponse{Hostname: "old"},
			egressMode: "",
			wantRefuse: false,
		},
		{
			// The gate is named for its flag, so the error contract holds
			// even when no mode is requested and the dial fails.
			name:       "no_mode_means_no_gate_even_when_dial_fails",
			infoErr:    connect.NewError(connect.CodeUnavailable, nil),
			egressMode: "",
			wantRefuse: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := ServerEntry{URL: "http://127.0.0.1:1", Token: "t"}
			if tc.infoErr == nil {
				mock := &mockSpawnServer{mockBunkerdServer: mockBunkerdServer{info: tc.info}}
				srv := newSpawnTestServer(t, mock)
				defer srv.Close()
				entry.URL = srv.URL
			}

			err := ensureDaemonSupportsEgressMode(tc.egressMode, entry)
			if got := err != nil; got != tc.wantRefuse {
				t.Fatalf("ensureDaemonSupportsEgressMode(%q) err = %v, wantRefuse=%v", tc.egressMode, err, tc.wantRefuse)
			}
			for _, want := range tc.refuseHas {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q missing %q", err.Error(), want)
				}
			}
		})
	}
}

// TestEnsureDaemonSupportsEgressMode_ProbeFailureNamesProbe pins the
// transport-failure arm: when ServerInfo cannot be resolved at all (daemon
// down, auth rejected), an enforced-mode spawn fails with the probe-named
// error instead of proceeding on an unverified daemon.
func TestEnsureDaemonSupportsEgressMode_ProbeFailureNamesProbe(t *testing.T) {
	err := ensureDaemonSupportsEgressMode("allowlist", ServerEntry{URL: "http://127.0.0.1:1", Token: "t"})
	if err == nil {
		t.Fatal("expected the probe to fail loudly against an unreachable daemon")
	}
	if !strings.Contains(err.Error(), "egress capability probe") {
		t.Errorf("probe failure must be prefixed with the probe name, got %q", err.Error())
	}
	for _, want := range []string{"allowlist", "--egress-mode", "upgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q missing %q", err.Error(), want)
		}
	}
}

// TestSpawnCommand_EgressModeRefusedOnPreCapabilityDaemon drives the full
// `bunker spawn --egress-mode` path end to end through the CLI against a
// fake pre-capability daemon (ServerInfo without the egress fields):
// spawn must FAIL LOUDLY client-side with an error naming that the daemon
// predates egress enforcement, and the SpawnAgent RPC must never reach the
// wire. AC1.
func TestSpawnCommand_EgressModeRefusedOnPreCapabilityDaemon(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockSpawnServer{
		mockBunkerdServer: mockBunkerdServer{
			// A daemon from before the egress surface (pre-fd974b51):
			// ServerInfo answers, but carries no capability fields.
			info: &v1.ServerInfoResponse{Hostname: "old-bunkerd", Version: "v0.1.9"},
		},
		spawnResp: &v1.SpawnAgentResponse{AgentId: "never-spawned"},
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()
	writeSpawnTestConfig(t, tmpDir, srv.URL)

	cmd := NewSpawnCommand()
	cmd.SetArgs([]string{"--egress-mode", "none"})
	runErr := cmd.Execute()

	if runErr == nil {
		t.Fatal("spawn with --egress-mode none against a pre-capability daemon must fail, got nil error")
	}
	for _, want := range []string{"predates egress enforcement", "none", "silently ignore"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("refusal %q missing %q", runErr.Error(), want)
		}
	}
	if mock.gotAgentID != "" {
		t.Errorf("SpawnAgent RPC reached a pre-capability daemon (agentId=%q) — the refusal must fire BEFORE the spawn", mock.gotAgentID)
	}
}

// TestSpawnCommand_EgressModeProceedsOnCapableDaemon is the positive arm:
// the same flag against a daemon that REPORTS its vocabulary spawns
// normally (and the local validation, not the gate, is what would reject a
// bogus mode).
func TestSpawnCommand_EgressModeProceedsOnCapableDaemon(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockSpawnServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname:             "new-bunkerd",
				Version:              "v0.2.0",
				EgressModesSupported: []string{"open", "allowlist", "none"},
			},
		},
		spawnResp: &v1.SpawnAgentResponse{
			AgentId:       "eg123",
			DockerHostSsh: "DOCKER_HOST=ssh://eg@host",
			SshPrivateKey: "test-private-key-data",
		},
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()
	writeSpawnTestConfig(t, tmpDir, srv.URL)

	cmd := NewSpawnCommand()
	cmd.SetArgs([]string{"--egress-mode", "none", "eg123"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "eg123") {
		t.Errorf("output missing spawned agent id, got:\n%s", output)
	}
	if mock.gotAgentID != "eg123" {
		t.Errorf("SpawnAgent did not see the spawn (gotAgentID=%q)", mock.gotAgentID)
	}
	if mock.gotEgressMode != "none" {
		t.Errorf("SpawnAgent got egress mode %q, want \"none\" forwarded verbatim (DF-BUNKER-82)", mock.gotEgressMode)
	}
}
