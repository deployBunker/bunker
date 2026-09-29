package server

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// imgTestStubUID is the fake HOST uid the TestMain seam resolves for the agent
// user. It exists precisely to prove it is NOT used as the container identity.
const imgTestStubUID = 1001

// DF-BUNKER-77-IMGEXEC regression fingerprint (live gate
// bunker-2026-09-29-04-42-35): the image exec runs INSIDE the agent's rootless
// user namespace (uid_map "0 <host agent uid> 1"), so the pre-IMGEXEC fix's
// `--user <host uid>` handed the container an unmapped id that owned none of
// the agent's paths — `bunker env set` failed EACCES on /run/bunker/<id>/env
// and the rootless docker.sock matched nobody (conjuncts A and C red), while
// the same commands were green on a plain agent (control). The production
// builders must therefore carry the agent's NAMESPACE-resolved identity
// (--user 0 — the agent IS ns uid 0), push DOCKER_HOST into the container
// with -e (the outer env(1) prefix reaches only the docker CLI on the agent
// host), and re-source the env file inside the shell-wrapped container so
// `bunker env set` injections are visible. The host uid must never appear as
// the container identity.
//
// This test drives the REAL production builders (the same seam ExecAgent
// selects by imageRef) — not a duplicate helper.
func TestMain(m *testing.M) {
	resolveAgentUID = func(string) (int, bool) { return 1001, true }
	os.Exit(m.Run())
}

// wantImageRuntimeArgs returns the container-identity elements every image
// exec mode must carry: the namespace-resolved identity plus the DOCKER_HOST
// env that reaches the CONTAINER.
func wantImageRuntimeArgs(agentID string) []string {
	return []string{
		"--user", "0",
		"-e", "DOCKER_HOST=unix:///run/bunker/" + agentID + "/docker.sock",
	}
}

func TestImageExecBuilders_CarryNamespaceIdentityAndRuntime(t *testing.T) {
	ctx := context.Background()
	wantRuntimeBind := "-v /run/bunker/" + imgTestAgentID + ":/run/bunker/" + imgTestAgentID

	shell := buildAgentImageExecCommand(imgTestAgentID, imgTestHome, "which", []string{"jq"}, false, imgTestRef)
	for _, el := range wantImageRuntimeArgs(imgTestAgentID) {
		if !strings.Contains(shell, el+" ") {
			t.Errorf("image shell exec missing container element %q: %q", el, shell)
		}
	}
	// DF-BUNKER-77-IMGEXEC: the HOST uid must never be the container identity —
	// it is an unmapped id inside the agent's user namespace (live conjuncts A+C).
	if strings.Contains(shell, "--user "+strconv.Itoa(imgTestStubUID)) {
		t.Errorf("image shell exec demoted the container to the unmapped HOST uid %d: %q", imgTestStubUID, shell)
	}
	if !strings.Contains(shell, wantRuntimeBind) {
		t.Errorf("image shell exec does not bind the agent runtime dir %q: %q", wantRuntimeBind, shell)
	}
	// The env file must be sourced INSIDE the container shell too, so
	// `bunker env set` injections are visible to image-backed commands
	// (pre-IMGEXEC they were sourced on the host side only).
	if !strings.Contains(shell, "[ -f /run/bunker/"+imgTestAgentID+"/env ] && . /run/bunker/"+imgTestAgentID+"/env") {
		t.Errorf("image shell exec does not re-source the env file in-container: %q", shell)
	}

	raw := buildExecSSHRawCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, "which", []string{"jq"}, false, imgTestRef).Args
	rawJoined := strings.Join(raw, " ")
	for _, el := range wantImageRuntimeArgs(imgTestAgentID) {
		if !strings.Contains(rawJoined, " "+el+" ") {
			t.Errorf("image raw exec missing container element %q: %q", el, rawJoined)
		}
	}
	if strings.Contains(rawJoined, "--user "+strconv.Itoa(imgTestStubUID)) {
		t.Errorf("image raw exec demoted the container to the unmapped HOST uid %d: %q", imgTestStubUID, rawJoined)
	}
	if !strings.Contains(rawJoined, wantRuntimeBind) {
		t.Errorf("image raw exec does not bind the agent runtime dir %q: %q", wantRuntimeBind, rawJoined)
	}

	scriptCmd := buildExecSSHScriptCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, "#!/bin/sh\necho hi\n", false, imgTestRef)
	script := scriptCmd.Args[len(scriptCmd.Args)-1]
	for _, el := range wantImageRuntimeArgs(imgTestAgentID) {
		if !strings.Contains(script, el+" ") {
			t.Errorf("image script exec missing container element %q: %q", el, script)
		}
	}
	if strings.Contains(script, "--user "+strconv.Itoa(imgTestStubUID)) {
		t.Errorf("image script exec demoted the container to the unmapped HOST uid %d: %q", imgTestStubUID, script)
	}
	if !strings.Contains(script, wantRuntimeBind) {
		t.Errorf("image script exec does not bind the agent runtime dir %q: %q", wantRuntimeBind, script)
	}
	if !strings.Contains(script, "[ -f /run/bunker/"+imgTestAgentID+"/env ] && . /run/bunker/"+imgTestAgentID+"/env") {
		t.Errorf("image script exec does not re-source the env file in-container: %q", script)
	}
}

// TestImageExecBuilders_UnresolvedAgentUserStillFailsLoudly keeps the
// DF-BUNKER-77 loud-refusal contract across the identity change: an
// unresolvable agent user must fail the exec instead of silently running as
// container root (shell and script arms emit the refusal snippet; raw returns
// errAgentUserUnresolved).
func TestImageExecBuilders_UnresolvedAgentUserStillFailsLoudly(t *testing.T) {
	orig := resolveAgentUID
	resolveAgentUID = func(string) (int, bool) { return 0, false }
	defer func() { resolveAgentUID = orig }()

	shell := buildAgentImageExecCommand(imgTestAgentID, imgTestHome, "which", []string{"jq"}, false, imgTestRef)
	if !strings.Contains(shell, "image exec refused") {
		t.Errorf("image shell exec must refuse loudly on an unresolvable agent user: %q", shell)
	}
	if strings.Contains(shell, imgTestRef) {
		t.Errorf("image shell exec refusal must not run the image: %q", shell)
	}
	raw, err := buildAgentImageRawExecCommand(imgTestAgentID, imgTestHome, "which", []string{"jq"}, false, imgTestRef)
	if err != errAgentUserUnresolved {
		t.Errorf("image raw exec err = %v, want errAgentUserUnresolved", err)
	}
	if raw != nil {
		t.Errorf("image raw exec refusal must not build an argv, got %q", raw)
	}
	script := buildAgentImageScriptCommand(imgTestAgentID, imgTestHome, "#!/bin/sh\necho hi\n", false, imgTestRef)
	if !strings.Contains(script, "image exec refused") {
		t.Errorf("image script exec must refuse loudly on an unresolvable agent user: %q", script)
	}
}
