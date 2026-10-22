package cli

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// TOOLS-003: the spawn command's --tools-at-spawn wiring. These tests run the
// spawn command end to end against the mockSpawnServer (the spawn tests'
// shape): the flag must attach the canonical delivered-set spec to the RPC,
// refuse a mismatched custom spec BEFORE any RPC, and invoke the post-spawn
// delivery — whose failure is LOUD and whose success is stated in the output.

// deliveryRecorder replaces deliverSpawnToolsFunc for the duration of one
// test and records the agent id the delivery was invoked with.
type deliveryRecorder struct {
	agentIDs []string
	err      error // nil = succeed
}

func (d *deliveryRecorder) deliver(_ context.Context, client bunkerv1connect.BunkerdClient,
	entry ServerEntry, agentID string, errOut, out io.Writer) error {
	return d.deliverImpl(agentID)
}

// deliverImpl is the recordable half so the tests never touch the RPC types.
func (d *deliveryRecorder) deliverImpl(agentID string) error {
	d.agentIDs = append(d.agentIDs, agentID)
	return d.err
}

// runToolsAtSpawn drives `bunker spawn --tools-at-spawn [id]` with a stubbed
// delivery leg, HOME pointed at a temp config, and the given optional
// --image-spec file content. Returns the spawn stdout and the command error.
func runToolsAtSpawn(t *testing.T, serverURL string, rec *deliveryRecorder, imageSpecJSON string) (string, error) {
	t.Helper()

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(SessionTargetEnvVar, "default")
	writeSpawnTestConfig(t, tmpDir, serverURL)

	orig := deliverSpawnToolsFunc
	deliverSpawnToolsFunc = rec.deliver
	t.Cleanup(func() { deliverSpawnToolsFunc = orig })

	args := []string{"--server", "default", "--tools-at-spawn", "spawn-tools-agent"}
	if imageSpecJSON != "" {
		p := filepath.Join(tmpDir, "spec.json")
		if err := os.WriteFile(p, []byte(imageSpecJSON), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--image-spec", p)
	}

	cmd := NewSpawnCommand()
	cmd.SetArgs(args)
	// The bundle prints via fmt.Println (direct stdout), so capture the real
	// stdout — the same shape TestSpawnCommand_Success uses.
	var out string
	var err error
	out = captureStdout(t, func() {
		err = cmd.Execute()
	})
	return out, err
}

// newMockedSpawnForTools builds the mock + httptest server pair the wiring
// tests spawn against. The returned mock records what reached the wire; the
// command is synchronous, so after runToolsAtSpawn returns the RPC has run.
func newMockedSpawnForTools(t *testing.T, rec *deliveryRecorder) (*mockSpawnServer, *httptest.Server) {
	t.Helper()
	mock := &mockSpawnServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{Hostname: "bunker-tools", Version: "v0.2.0"},
		},
		spawnResp: &v1.SpawnAgentResponse{
			AgentId:       "spawn-tools-agent",
			SshPrivateKey: "test-private-key-data",
		},
	}
	srv := newSpawnTestServer(t, mock)
	t.Cleanup(srv.Close)
	return mock, srv
}

// deliveredSetSpecJSON is the canonical delivered set as an operator would
// paste it (byte-equal to imagespec.AgentToolSpec).
const deliveredSetSpecJSON = `{"packages":[{"manager":"apt","packages":["ripgrep"]},{"manager":"go","packages":["golang.org/x/tools/gopls@latest"]}]}`

// TestSpawnToolsAtSpawnAttachesDeliveredSetSpec: with --tools-at-spawn and no
// --image-spec, the RPC must carry the canonical delivered set (rg via apt,
// gopls via go) so the image itself provisions the registry tools.
func TestSpawnToolsAtSpawnAttachesDeliveredSetSpec(t *testing.T) {
	rec := &deliveryRecorder{}
	mock, srv := newMockedSpawnForTools(t, rec)
	if _, err := runToolsAtSpawn(t, srv.URL, rec, ""); err != nil {
		t.Fatalf("spawn --tools-at-spawn: %v", err)
	}

	spec := mock.gotImageSpec
	if spec == nil {
		t.Fatal("the SpawnAgentRequest carried no image spec; --tools-at-spawn must attach the delivered set")
	}
	if len(spec.GetPackages()) != 2 {
		t.Fatalf("attached spec has %d directives, want 2 (apt ripgrep + go gopls)", len(spec.GetPackages()))
	}
	if spec.GetPackages()[0].GetManager() != "apt" || spec.GetPackages()[0].GetPackages()[0] != "ripgrep" {
		t.Errorf("directive 0 = %s/%v, want apt/ripgrep", spec.GetPackages()[0].GetManager(), spec.GetPackages()[0].GetPackages())
	}
	if spec.GetPackages()[1].GetManager() != "go" || spec.GetPackages()[1].GetPackages()[0] != "golang.org/x/tools/gopls@latest" {
		t.Errorf("directive 1 = %s/%v, want go/golang.org/x/tools/gopls@latest", spec.GetPackages()[1].GetManager(), spec.GetPackages()[1].GetPackages())
	}
	if len(rec.agentIDs) != 1 || rec.agentIDs[0] != "spawn-tools-agent" {
		t.Errorf("post-spawn delivery invoked with %v, want exactly [spawn-tools-agent]", rec.agentIDs)
	}
}

// TestSpawnToolsAtSpawnAcceptsTheDeliveredSetAsCustomSpec: an explicit
// --image-spec that IS the delivered set passes the equality gate unchanged.
func TestSpawnToolsAtSpawnAcceptsTheDeliveredSetAsCustomSpec(t *testing.T) {
	rec := &deliveryRecorder{}
	mock, srv := newMockedSpawnForTools(t, rec)
	if _, err := runToolsAtSpawn(t, srv.URL, rec, deliveredSetSpecJSON); err != nil {
		t.Fatalf("the delivered set itself must pass --tools-at-spawn's equality gate, got: %v", err)
	}
	if mock.gotImageSpec == nil || len(mock.gotImageSpec.GetPackages()) != 2 {
		t.Errorf("the operator's delivered-set spec must reach the wire unchanged, got %+v", mock.gotImageSpec)
	}
}

// TestSpawnToolsAtSpawnRefusesForeignSpec pins the pre-RPC refusal: a custom
// spec that is NOT exactly the delivered set must fail the spawn BEFORE the
// RPC (no agent created, no delivery run) — an unknown merge would let two
// package lists fight over the same image.
func TestSpawnToolsAtSpawnRefusesForeignSpec(t *testing.T) {
	for name, spec := range map[string]string{
		// NOTE: an "extra apt directive" case is unreachable here — the spec
		// parser itself refuses duplicate apt directives (a second gate in
		// front of this one), so the refusal cases are the ones the parser
		// accepts but --tools-at-spawn must still reject.
		"foreign only":  `{"packages":[{"manager":"apt","packages":["sl"]}]}`,
		"wrong manager": `{"packages":[{"manager":"npm","packages":["ripgrep"]},{"manager":"go","packages":["golang.org/x/tools/gopls@latest"]}]}`,
		"swapped order": `{"packages":[{"manager":"go","packages":["golang.org/x/tools/gopls@latest"]},{"manager":"apt","packages":["ripgrep"]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := &deliveryRecorder{}
			mock, srv := newMockedSpawnForTools(t, rec)
			_, err := runToolsAtSpawn(t, srv.URL, rec, spec)
			if err == nil {
				t.Fatal("a non-delivered-set spec must be refused under --tools-at-spawn")
			}
			if !strings.Contains(err.Error(), "--tools-at-spawn") {
				t.Errorf("the refusal must name the flag, got: %v", err)
			}
			// The refusal is a gate BEFORE the RPC: no spawn reached the wire
			// and no delivery ran.
			if mock.gotAgentID != "" {
				t.Errorf("the spawn RPC ran for a refused spec (agent id %q); the gate must fire before the RPC", mock.gotAgentID)
			}
			if len(rec.agentIDs) != 0 {
				t.Errorf("delivery ran for a refused spawn: %v", rec.agentIDs)
			}
		})
	}
}

// TestSpawnToolsAtSpawnFailsLoudOnDeliveryError pins the spawn-surface wiring
// of the row's loud rule: when the delivery leg fails, the SPAWN COMMAND
// fails (non-zero) with the failure attributed to the delivery — the agent
// itself was created (the bundle printed) but is named as NOT provisioned.
func TestSpawnToolsAtSpawnFailsLoudOnDeliveryError(t *testing.T) {
	rec := &deliveryRecorder{err: os.ErrPermission}
	_, srv := newMockedSpawnForTools(t, rec)
	out, err := runToolsAtSpawn(t, srv.URL, rec, "")
	if err == nil {
		t.Fatal("a failed spawn-time delivery must fail the spawn command")
	}
	if !strings.Contains(err.Error(), "tools-at-spawn delivery failed") {
		t.Errorf("the failure must be attributed to the delivery, got: %v", err)
	}
	if !strings.Contains(out, "Agent created") {
		t.Errorf("the bundle must still print: the agent EXISTS (the failure is the provisioning, not the creation), got:\n%s", out)
	}
}

// TestSpawnWithoutToolsAtSpawnIsByteIdentical pins the negative arm: without
// the flag, no spec is attached and the delivery leg never runs — agents
// spawned the old way behave exactly as before.
func TestSpawnWithoutToolsAtSpawnIsByteIdentical(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(SessionTargetEnvVar, "default")

	rec := &deliveryRecorder{}
	mock := &mockSpawnServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{Hostname: "bunker-plain", Version: "v0.2.0"},
		},
		spawnResp: &v1.SpawnAgentResponse{
			AgentId:       "plain-agent",
			SshPrivateKey: "test-private-key-data",
		},
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()
	writeSpawnTestConfig(t, tmpDir, srv.URL)

	orig := deliverSpawnToolsFunc
	deliverSpawnToolsFunc = rec.deliver
	t.Cleanup(func() { deliverSpawnToolsFunc = orig })

	cmd := NewSpawnCommand()
	cmd.SetArgs([]string{"--server", "default", "plain-agent"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("plain spawn: %v", err)
	}

	if mock.gotImageSpec != nil {
		t.Errorf("a plain spawn must not carry an image spec, got %+v", mock.gotImageSpec)
	}
	if len(rec.agentIDs) != 0 {
		t.Errorf("a plain spawn must not run the delivery leg, got %v", rec.agentIDs)
	}
}
