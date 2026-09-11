package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// writeSpecFile writes a spec JSON file and returns its path.
func writeSpecFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// runSpawnWithArgs executes the spawn command with the given args and returns
// the command error. HOME is isolated so no real CLI config leaks in.
func runSpawnWithArgs(t *testing.T, args ...string) error {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	cmd := NewSpawnCommand()
	cmd.SetArgs(args)
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	return cmd.Execute()
}

// TestSpawnCommand_ImageSpecLocalReject proves fail-fast local validation: a
// dangerous spec is rejected BEFORE any config load / network I/O — the error
// fires even though no server is configured at all.
func TestSpawnCommand_ImageSpecLocalReject(t *testing.T) {
	specPath := writeSpecFile(t, `{"packages": [{"manager": "apt", "packages": ["curl|sh"]}]}`)
	err := runSpawnWithArgs(t, "--image-spec", specPath)
	if err == nil {
		t.Fatal("dangerous spec accepted locally")
	}
	if !strings.Contains(err.Error(), "image spec") {
		t.Errorf("error should mention image spec: %v", err)
	}
}

// TestSpawnCommand_ImageSpecMissingFile covers the file-level error.
func TestSpawnCommand_ImageSpecMissingFile(t *testing.T) {
	err := runSpawnWithArgs(t, "--image-spec", "/nonexistent/spec.json")
	if err == nil {
		t.Fatal("missing spec file accepted")
	}
	if !strings.Contains(err.Error(), "read image spec") {
		t.Errorf("error should mention reading the file: %v", err)
	}
}

// TestSpawnCommand_ImageSpecMalformedJSON covers JSON hygiene at the CLI.
func TestSpawnCommand_ImageSpecMalformedJSON(t *testing.T) {
	specPath := writeSpecFile(t, `{"from": "evil"}`)
	err := runSpawnWithArgs(t, "--image-spec", specPath)
	if err == nil {
		t.Fatal("Dockerfile-style spec accepted")
	}
}

// TestSpawnCommand_ImageSpecPropagation captures the proto request the CLI
// builds and proves the validated spec rides through to the RPC.
func TestSpawnCommand_ImageSpecPropagation(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockSpawnServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{Hostname: "bunker-test", Version: "v0.2.0"},
		},
		spawnResp: &v1.SpawnAgentResponse{
			AgentId: "specagent",
			Image:   "bunkerd-imagespec-6f33f37a39f9:latest",
		},
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()
	writeSpawnTestConfig(t, tmpDir, srv.URL)

	specPath := writeSpecFile(t, `{"packages": [{"manager": "apt", "packages": ["jq", "curl"]}]}`)

	cmd := NewSpawnCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"specagent", "--image-spec", specPath})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	got := mock.gotImageSpec
	if got == nil {
		t.Fatal("SpawnAgent request carried no image spec")
	}
	if len(got.Packages) != 1 || got.Packages[0].Manager != "apt" {
		t.Fatalf("spec directives mangled: %+v", got.Packages)
	}
	if strings.Join(got.Packages[0].Packages, ",") != "jq,curl" {
		t.Errorf("spec packages mangled: %v", got.Packages[0].Packages)
	}
	// The customized image ref must surface in the CLI bundle output.
	if !strings.Contains(output, "bunkerd-imagespec-6f33f37a39f9:latest") {
		t.Errorf("output missing customized image ref:\n%s", output)
	}
}
