package cli

// ── DF-BUNKER-67: renew --preset passthrough ───────────────────────────────
//
// A renewal destroys and re-spawns the agent. Before this row the re-spawn
// request carried no safety preset, so renewing an agent that ran under a
// containment preset (standard/hardened) silently re-spawned under the
// daemon's resolved default — and on bunker-las-03 a standard-preset spawn
// is exactly what the convergence budget broke. The flag mirrors spawn's
// GAP-116 plumbing: registered, locally validated against the same preset
// vocabulary, carried on the SpawnAgentRequest, empty = current behavior
// (the daemon resolves the preset, and re-validates whatever resolves).

import (
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// TestRenewCommand_PresetFlagRegistered pins the flag's existence and
// default: `renew --preset` is a string flag defaulting to empty (defer to
// the daemon's resolution), the same contract spawn's flag pins in
// gap116_test.go.
func TestRenewCommand_PresetFlagRegistered(t *testing.T) {
	cmd := NewRenewCommand()
	f := cmd.Flags().Lookup("preset")
	if f == nil {
		t.Fatal("renew is missing the --preset flag (DF-BUNKER-67)")
	}
	if f.DefValue != "" {
		t.Errorf("--preset default = %q, want empty (empty must defer to daemon resolution)", f.DefValue)
	}
}

// TestRenewCommand_PresetPassthrough proves the renewal's spawn request
// carries the requested preset verbatim — the whole point of the flag: the
// re-spawn must run under the SAME containment preset the agent ran under.
func TestRenewCommand_PresetPassthrough(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mock := &renewMockServer{
		agent:         &v1.AgentSummary{AgentId: "eduos-agent"},
		destroyStatus: "destroyed",
		spawnAgentID:  "eduos-agent",
	}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	if _, execErr := runRenew(t, srv, "--agent-id", "eduos-agent", "--ttl", "7d", "--preset", "standard"); execErr != nil {
		t.Fatalf("renew with --preset failed: %v", execErr)
	}
	if mock.spawnRequested == nil {
		t.Fatal("the renew never issued a spawn")
	}
	if got := mock.spawnRequested.GetSafetyPreset(); got != "standard" {
		t.Errorf("spawn request safety_preset = %q, want \"standard\" (passthrough)", got)
	}
	if got := mock.spawnRequested.GetAgentId(); got != "eduos-agent" {
		t.Errorf("spawn request agent_id = %q, want eduos-agent (passthrough must not disturb the stable id)", got)
	}
}

// TestRenewCommand_PresetDefaultEmptyKeepsCurrentBehavior proves the default
// is wire-compatible with the pre-row behavior: no --preset means the spawn
// request carries an EMPTY preset and the daemon resolves (then re-validates)
// it — exactly what the pre-row renew sent.
func TestRenewCommand_PresetDefaultEmptyKeepsCurrentBehavior(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mock := &renewMockServer{
		agent:         &v1.AgentSummary{AgentId: "eduos-agent"},
		destroyStatus: "destroyed",
		spawnAgentID:  "eduos-agent",
	}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	if _, execErr := runRenew(t, srv, "--agent-id", "eduos-agent", "--ttl", "7d"); execErr != nil {
		t.Fatalf("renew without --preset failed: %v", execErr)
	}
	if mock.spawnRequested == nil {
		t.Fatal("the renew never issued a spawn")
	}
	if got := mock.spawnRequested.GetSafetyPreset(); got != "" {
		t.Errorf("spawn request safety_preset = %q, want empty (no flag = daemon resolution, unchanged)", got)
	}
}

// TestRenewCommand_PresetLocalReject proves the spawn-side fail-fast rule
// carries over (GAP-116 shape): an unknown --preset is rejected LOCALLY,
// before any RPC — the destroy must never have run when the preset name is
// a typo.
func TestRenewCommand_PresetLocalReject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mock := &renewMockServer{}
	srv := newRenewTestServer(t, mock)
	defer srv.Close()

	_, execErr := runRenew(t, srv, "--agent-id", "eduos-agent", "--preset", "ultra")
	if execErr == nil {
		t.Fatal("unknown --preset accepted locally by renew")
	}
	if !strings.Contains(execErr.Error(), "invalid --preset") {
		t.Fatalf("error must name the invalid preset, got: %v", execErr)
	}
	if mock.destroyCalled {
		t.Error("the preset rejection must come BEFORE the destroy")
	}
	if mock.spawnRequested != nil {
		t.Error("the preset rejection must come BEFORE the spawn")
	}
}
