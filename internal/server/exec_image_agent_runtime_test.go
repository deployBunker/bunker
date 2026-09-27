package server

import (
	"context"
	"os"
	"strings"
	"testing"
)

// DF-BUNKER-77 regression fingerprint: image-backed exec binds only $HOME and
// runs as container root, so /run/bunker/<id>/env and docker.sock are hidden
// and agent-tools sees the container instead of the agent. The production
// builders must carry the agent's identity (--user <uid>) and bind the agent
// runtime directory at its absolute path so env and docker.sock stay visible.
//
// This test drives the REAL production builders (the same seam ExecAgent
// selects by imageRef) — not a duplicate helper.
func TestMain(m *testing.M) {
	agentUserFlag = func(string) (string, bool) { return "--user 1001", true }
	os.Exit(m.Run())
}

func TestImageExecBuilders_CarryAgentIdentityAndRuntime(t *testing.T) {
	ctx := context.Background()
	wantRuntimeBind := "-v /run/bunker/" + imgTestAgentID + ":/run/bunker/" + imgTestAgentID

	shell := buildAgentImageExecCommand(imgTestAgentID, imgTestHome, "which", []string{"jq"}, false, imgTestRef)
	if !strings.Contains(shell, "--user ") {
		t.Errorf("image shell exec does not run as the agent user (no --user): %q", shell)
	}
	if !strings.Contains(shell, wantRuntimeBind) {
		t.Errorf("image shell exec does not bind the agent runtime dir %q: %q", wantRuntimeBind, shell)
	}

	raw := buildExecSSHRawCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, "which", []string{"jq"}, false, imgTestRef).Args
	rawJoined := strings.Join(raw, " ")
	if !strings.Contains(rawJoined, " --user ") {
		t.Errorf("image raw exec does not run as the agent user (no --user): %q", rawJoined)
	}
	if !strings.Contains(rawJoined, wantRuntimeBind) {
		t.Errorf("image raw exec does not bind the agent runtime dir %q: %q", wantRuntimeBind, rawJoined)
	}

	scriptCmd := buildExecSSHScriptCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, "#!/bin/sh\necho hi\n", false, imgTestRef)
	script := scriptCmd.Args[len(scriptCmd.Args)-1]
	if !strings.Contains(script, "--user ") {
		t.Errorf("image script exec does not run as the agent user (no --user): %q", script)
	}
	if !strings.Contains(script, wantRuntimeBind) {
		t.Errorf("image script exec does not bind the agent runtime dir %q: %q", wantRuntimeBind, script)
	}
}
