package server

// SURF-012 tests.
//
// Two levels, and both are load-bearing:
//
//  1. ARGV/SCRIPT SHAPE — persistentContainerRunArgv, the ensure snippet and
//     the list/cleanup scripts are asserted directly, so a drift in the
//     marker labels, the image comparison or the docker filters fails here.
//  2. SEMANTICS THROUGH A REAL SHELL — the built remote command is handed to a
//     real /bin/sh with a stub `docker` on the exec PATH (the same technique
//     the GAP-069 image-exec battery uses). That is what proves the reuse
//     contract end to end: the FIRST invocation creates the container, the
//     SECOND must NOT create another one, and the user command must still
//     arrive inside the container shell.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// TestPersistentModeDocumentedJSONNameDecodes pins the REST spelling the docs
// give (docs/integration.md, "Persistent containers") to the codec the REST
// surface actually runs: connect-go's JSON codec decodes proto messages with
// protojson, which accepts an enum by NAME. Without this, the documented body
// could rot into an "unknown enum value" error with no test noticing.
func TestPersistentModeDocumentedJSONNameDecodes(t *testing.T) {
	body := `{"agent_id":"build-1","command":"go","args":["build","./..."],"persistent_mode":"PERSISTENT_MODE_EXEC"}`
	req := &v1.ExecAgentRequest{}
	if err := protojson.Unmarshal([]byte(body), req); err != nil {
		t.Fatalf("the REST body documented in docs/integration.md does not decode: %v", err)
	}
	if got := req.GetPersistentMode(); got != v1.PersistentMode_PERSISTENT_MODE_EXEC {
		t.Errorf("persistent_mode decoded as %v, want PERSISTENT_MODE_EXEC", got)
	}
	if req.GetCommand() != "go" || !reflect.DeepEqual(req.GetArgs(), []string{"build", "./..."}) {
		t.Errorf("documented body lost its command/args: %q %q", req.GetCommand(), req.GetArgs())
	}
}

// ── 1. argv / script shape ──────────────────────────────────────────────────

func TestPersistentContainerRunArgv_CarriesMarkerMountsAndIdentity(t *testing.T) {
	got := persistentContainerRunArgv(imgTestAgentID, imgTestHome, false, imgTestRef)
	want := []string{
		"docker", "run", "-d",
		"--name", "bunker-exec-" + imgTestAgentID,
		"--label", "bunker.persistent=true",
		"--label", "bunker.agent=" + imgTestAgentID,
		// QA-BUNKER-83: the container is created with the agent-scoped TMPDIR
		// in its config env, so every `docker exec` shell inherits the pin.
		"-e", "TMPDIR=" + imgTestHome + "/.bunker/tmp",
		"-v", imgTestHome + ":" + imgTestHome,
		"-w", imgTestHome,
		"-v", "/run/bunker/" + imgTestAgentID + ":/run/bunker/" + imgTestAgentID,
		"--user", "0",
		"-e", "DOCKER_HOST=unix:///run/bunker/" + imgTestAgentID + "/docker.sock",
		imgTestRef, "sleep", "infinity",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("persistent run argv:\n got: %q\nwant: %q", got, want)
	}
	// The one-shot path's `--rm` must NOT be here: a persistent container is
	// removed by --cleanup-persistent, not by its own exit.
	for _, a := range got {
		if a == "--rm" {
			t.Errorf("persistent run argv must not carry --rm: %q", got)
		}
	}
}

func TestPersistentContainerRunArgv_DisclosureAddsContainerMarker(t *testing.T) {
	got := persistentContainerRunArgv(imgTestAgentID, imgTestHome, true, imgTestRef)
	wantPrefix := []string{
		"docker", "run", "-d",
		"--name", "bunker-exec-" + imgTestAgentID,
		"--label", "bunker.persistent=true",
		"--label", "bunker.agent=" + imgTestAgentID,
		"-e", containmentSandboxEnv,
	}
	if !reflect.DeepEqual(got[:len(wantPrefix)], wantPrefix) {
		t.Errorf("disclosure prefix = %q, want %q", got[:len(wantPrefix)], wantPrefix)
	}
}

// The ensure snippet is the whole reuse contract: compare the container's
// image against the agent's CURRENT image ref, recreate on mismatch, otherwise
// just start it. Asserting the pieces keeps a future "simplification" (e.g.
// dropping the comparison) from silently exec-ing into a stale container.
func TestPersistentEnsureSnippet_ValidatesImageAndReuses(t *testing.T) {
	got := persistentEnsureSnippet(imgTestAgentID, imgTestHome, false, imgTestRef)

	for _, want := range []string{
		"docker inspect -f",   // the comparison must read the container's image
		"'{{.Config.Image}}'", // ... via Config.Image, i.e. the ref it was created from
		"'bunker-exec-" + imgTestAgentID + "'",
		"'docker' 'run' '-d'",                // the create is the shell-joined run argv
		"'--label' 'bunker.persistent=true'", // marker survives as a container label
		"'--label' 'bunker.agent=" + imgTestAgentID + "'",
		"'" + imgTestRef + "'", // creation uses the agent's current image
		"docker rm -f",
		"docker start",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ensure snippet missing %q\nsnippet: %s", want, got)
		}
	}
	// The comparison must be against the image ref passed in, and the failure
	// path must be able to create the container: an empty ref would make the
	// snippet recreate every time, so it is never called with "".
	if got == persistentEnsureSnippet(imgTestAgentID, imgTestHome, false, "other-ref:latest") {
		t.Error("ensure snippet does not depend on the image ref: a stale image would be reused silently")
	}
}

func TestPersistentExecTail_TargetsTheNamedContainer(t *testing.T) {
	got := persistentExecTail(imgTestAgentID, "/AHOME", "echo hi")
	want := "; exec docker exec -w '/AHOME' 'bunker-exec-" + imgTestAgentID + "' sh -lc 'echo hi'"
	if got != want {
		t.Fatalf("exec tail:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildAgentPersistentExecCommand_EnsureThenExecInsideContainer(t *testing.T) {
	remote, err := buildAgentPersistentExecCommand(imgTestAgentID, imgTestHome, "go", []string{"build", "./..."}, false, imgTestRef)
	if err != nil {
		t.Fatalf("buildAgentPersistentExecCommand: %v", err)
	}
	// Ensure runs first, then the container exec — order is the contract.
	ensureIdx := strings.Index(remote, "docker inspect -f")
	execIdx := strings.Index(remote, "exec docker exec -w")
	if ensureIdx < 0 || execIdx < 0 || ensureIdx > execIdx {
		t.Fatalf("expected ensure before the container exec:\n%s", remote)
	}
	// The one-shot container flags must be gone: this command reuses.
	if strings.Contains(remote, "--rm") {
		t.Errorf("persistent exec must not use the one-shot --rm path:\n%s", remote)
	}
	// The user command must run behind the in-container env source, so
	// `bunker env set` values are visible in persistent mode too.
	if !strings.Contains(remote, "set -a; [ -f /run/bunker/"+imgTestAgentID+"/env ]") {
		t.Errorf("in-container env source missing:\n%s", remote)
	}
	// The payload the CONTAINER receives is asserted through a real shell with
	// the stub docker: the snippet crosses two quoting layers, so reconstructing
	// the escaped bytes by hand would only test the test. Derive the expectation
	// from the same helpers the production path uses.
	home := t.TempDir()
	logPath, _ := installPersistentStubDocker(t, home)
	remoteRun, err := buildAgentPersistentExecCommand(imgTestAgentID, home, "go", []string{"build", "./..."}, false, imgTestRef)
	if err != nil {
		t.Fatalf("buildAgentPersistentExecCommand: %v", err)
	}
	if out, err := exec.Command("sh", "-c", remoteRun).CombinedOutput(); err != nil {
		t.Fatalf("built remote command failed: %v\noutput: %s\ncommand: %s", err, out, remoteRun)
	}
	wantPayload := applyAgentDataTmpPin(home, imageInnerSourcePrefix(imgTestAgentID)+buildAgentRemoteCmd("go", []string{"build", "./..."}))
	if got := readStubPayload(t, logPath); got != wantPayload {
		t.Errorf("container payload:\n got: %q\nwant: %q", got, wantPayload)
	}
}

func TestBuildAgentPersistentScriptCommand_UploadsThenExecsInContainer(t *testing.T) {
	remote, err := buildAgentPersistentScriptCommand(imgTestAgentID, imgTestHome, "#!/bin/sh\necho hi\n", false, imgTestRef)
	if err != nil {
		t.Fatalf("buildAgentPersistentScriptCommand: %v", err)
	}
	if !strings.Contains(remote, "EOFSCRIPT") || !strings.Contains(remote, "chmod +x") {
		t.Errorf("script upload prologue missing:\n%s", remote)
	}
	if !strings.Contains(remote, "exec docker exec -w") {
		t.Errorf("script must execute inside the persistent container:\n%s", remote)
	}
	if strings.Contains(remote, "--rm") {
		t.Errorf("persistent script exec must not use the one-shot --rm path:\n%s", remote)
	}
}

func TestPersistentListAndCleanupScripts_FilterOnMarkerLabels(t *testing.T) {
	list := persistentListScript(imgTestAgentID, imgTestHome)
	for _, want := range []string{
		"docker ps -a",
		"--filter 'bunker.persistent=true'",
		"--filter 'bunker.agent=" + imgTestAgentID + "'",
		"--format",
	} {
		if !strings.Contains(list, want) {
			t.Errorf("list script missing %q: %s", want, list)
		}
	}

	cleanup := persistentCleanupScript(imgTestAgentID, imgTestHome)
	for _, want := range []string{
		"docker ps -aq",
		"--filter 'bunker.persistent=true'",
		"--filter 'bunker.agent=" + imgTestAgentID + "'",
		"docker rm -f $ids",
		persistentNoneMessage(imgTestAgentID),
	} {
		if !strings.Contains(cleanup, want) {
			t.Errorf("cleanup script missing %q: %s", want, cleanup)
		}
	}
}

// An unresolvable agent user must fail the build loudly rather than produce a
// container run as container root — the same DF-BUNKER-77-IMGEXEC refusal the
// one-shot image path makes.
func TestPersistentBuilders_UnresolvableAgentUserFailsLoudly(t *testing.T) {
	orig := resolveAgentUID
	resolveAgentUID = func(string) (int, bool) { return 0, false }
	defer func() { resolveAgentUID = orig }()

	if _, err := buildAgentPersistentExecCommand(imgTestAgentID, imgTestHome, "echo", nil, false, imgTestRef); err == nil {
		t.Error("shell-mode persistent exec must refuse an unresolvable agent user")
	}
	if _, err := buildAgentPersistentScriptCommand(imgTestAgentID, imgTestHome, "echo hi", false, imgTestRef); err == nil {
		t.Error("script-mode persistent exec must refuse an unresolvable agent user")
	}
	if _, err := buildExecSSHPersistentImage(context.Background(), imgTestAgentID, imgTestKeyPath, imgTestHome, "echo", nil, false, imgTestRef); err == nil {
		t.Error("ssh shell-mode persistent exec must refuse an unresolvable agent user")
	}
}

func TestExecSSHPersistentImage_RemoteArgIsSingleQuotedShC(t *testing.T) {
	cmd, err := buildExecSSHPersistentImage(context.Background(), imgTestAgentID, imgTestKeyPath, imgTestHome, "echo", []string{"hi"}, false, imgTestRef)
	if err != nil {
		t.Fatalf("buildExecSSHPersistentImage: %v", err)
	}
	remote := cmd.Args[len(cmd.Args)-1]
	if !strings.HasPrefix(remote, "sh -c '") {
		t.Fatalf("ssh remote arg must be a single quoted sh -c string, got %q", remote)
	}
	if last := cmd.Args[len(cmd.Args)-2]; last != "bunker-"+imgTestAgentID+"@localhost" {
		t.Errorf("ssh target = %q, want %q", last, "bunker-"+imgTestAgentID+"@localhost")
	}
}

// ── 2. semantics through a real shell with a stub docker ────────────────────

// installPersistentStubDocker writes a `docker` stub into <home>/bin — the
// first element of the exec PATH the builders set — and returns the argv log
// path and the container-state path. The stub models exactly the three facts
// the ensure snippet depends on: whether a container exists, which image it
// was created from, and whether it is running.
func installPersistentStubDocker(t *testing.T, home string) (logPath, statePath string) {
	t.Helper()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir stub bin: %v", err)
	}
	logPath = filepath.Join(t.TempDir(), "docker-log")
	statePath = filepath.Join(t.TempDir(), "container-image")
	script := `#!/bin/sh
LOG='` + logPath + `'
STATE='` + statePath + `'
PAYLOAD='` + logPath + `.payload'
{
  printf 'VERB %s\n' "$1"
  printf 'ARG %s\n' "$@"
  printf 'END\n'
} >> "$LOG"
case "$1" in
  inspect)
    if [ -f "$STATE" ]; then cat "$STATE"; printf '\n'; exit 0; fi
    printf 'Error: No such object\n' >&2
    exit 1
    ;;
  run)
    prev=""; img=""
    for a in "$@"; do
      if [ "$a" = "sleep" ]; then img="$prev"; break; fi
      prev="$a"
    done
    printf '%s\n' "$img" > "$STATE"
    printf 'containerid0001\n'
    exit 0
    ;;
  start)
    if [ -f "$STATE" ]; then exit 0; fi
    printf 'Error: No such container\n' >&2
    exit 1
    ;;
  exec)
    payload=""
    for a in "$@"; do payload="$a"; done
    printf '%s' "$payload" > "$PAYLOAD"
    sh -c "$payload"
    exit $?
    ;;
  ps)
    for a in "$@"; do
      if [ "$a" = "-aq" ]; then printf 'cid1\ncid2\n'; exit 0; fi
    done
    printf 'bunker-exec-abc123\tbunkerd-imagespec-0123456789ab:latest\tUp 3 minutes\t2026-10-15 12:00:00 +0000 UTC\n'
    exit 0
    ;;
  rm)
    printf 'bunker-exec-abc123\n'
    exit 0
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub docker: %v", err)
	}
	return logPath, statePath
}

// readStubLog returns the recorded docker invocations.
func readStubLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read stub log: %v", err)
	}
	return string(data)
}

// readStubPayload returns the last command string the stub handed to
// `docker exec` — i.e. exactly what the CONTAINER shell would have run.
func readStubPayload(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath + ".payload")
	if err != nil {
		t.Fatalf("read stub payload: %v", err)
	}
	return string(data)
}

// TestPersistentContainer_ReuseAcrossInvocations is THE acceptance test for
// the reuse criterion: the same built remote command, run twice against a
// stub docker that remembers the container, must create the container the
// first time and exec into the SAME container the second time — no second
// `docker run`. That is the difference between persistent mode and today's
// container-per-exec behavior.
func TestPersistentContainer_ReuseAcrossInvocations(t *testing.T) {
	home := t.TempDir()
	logPath, statePath := installPersistentStubDocker(t, home)

	remote, err := buildAgentPersistentExecCommand(imgTestAgentID, home, "echo", []string{"hi"}, false, imgTestRef)
	if err != nil {
		t.Fatalf("buildAgentPersistentExecCommand: %v", err)
	}

	// ── first invocation: no container yet → create, then exec ──────────────
	first, err := exec.Command("sh", "-c", remote).CombinedOutput()
	if err != nil {
		t.Fatalf("first persistent exec failed: %v\noutput: %s\ncommand: %s", err, first, remote)
	}
	if got := strings.TrimSpace(string(first)); got != "hi" {
		t.Errorf("first invocation stdout = %q, want %q (the user command must run INSIDE the container shell)", got, "hi")
	}
	firstLog := readStubLog(t, logPath)
	if !strings.Contains(firstLog, "VERB run\n") {
		t.Errorf("first invocation must CREATE the persistent container; docker log:\n%s", firstLog)
	}
	if !strings.Contains(firstLog, "VERB exec\n") {
		t.Errorf("first invocation must exec into the container; docker log:\n%s", firstLog)
	}
	// The created container is the agent's image, marked as persistent.
	for _, want := range []string{
		"ARG --name\nARG bunker-exec-" + imgTestAgentID + "\n",
		"ARG --label\nARG bunker.persistent=true\n",
		"ARG --label\nARG bunker.agent=" + imgTestAgentID + "\n",
		"ARG " + imgTestRef + "\nARG sleep\nARG infinity\n",
	} {
		if !strings.Contains(firstLog, want) {
			t.Errorf("create argv missing %q; docker log:\n%s", want, firstLog)
		}
	}
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("stub container state: %v", err)
	}
	if strings.TrimSpace(string(state)) != imgTestRef {
		t.Errorf("container created from %q, want the agent's image %q", strings.TrimSpace(string(state)), imgTestRef)
	}
	wantPayload := applyAgentDataTmpPin(home, imageInnerSourcePrefix(imgTestAgentID)+buildAgentRemoteCmd("echo", []string{"hi"}))
	if got := readStubPayload(t, logPath); got != wantPayload {
		t.Errorf("first-invocation container payload:\n got: %q\nwant: %q", got, wantPayload)
	}

	// ── second invocation: container matches → REUSE, never recreate ────────
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("reset stub log: %v", err)
	}
	second, err := exec.Command("sh", "-c", remote).CombinedOutput()
	if err != nil {
		t.Fatalf("second persistent exec failed: %v\noutput: %s\ncommand: %s", err, second, remote)
	}
	if got := strings.TrimSpace(string(second)); got != "hi" {
		t.Errorf("second invocation stdout = %q, want %q", got, "hi")
	}
	secondLog := readStubLog(t, logPath)
	if strings.Contains(secondLog, "VERB run\n") {
		t.Errorf("second invocation must REUSE the existing container, but it ran `docker run` again; docker log:\n%s", secondLog)
	}
	if !strings.Contains(secondLog, "VERB start\n") {
		t.Errorf("second invocation must start the existing container; docker log:\n%s", secondLog)
	}
	if !strings.Contains(secondLog, "VERB exec\n") {
		t.Errorf("second invocation must exec into the existing container; docker log:\n%s", secondLog)
	}
	// Reuse must not change WHAT runs: the container still receives the same
	// command, so persistence is invisible to the caller.
	if got := readStubPayload(t, logPath); got != wantPayload {
		t.Errorf("second-invocation container payload:\n got: %q\nwant: %q", got, wantPayload)
	}
}

// TestPersistentContainer_RecreatesWhenImageRefChanges pins the other half of
// the reuse contract: an agent re-spawned with a different image must not
// inherit a container built from the previous image (whose toolchain and
// mounts belong to the old agent).
func TestPersistentContainer_RecreatesWhenImageRefChanges(t *testing.T) {
	home := t.TempDir()
	logPath, statePath := installPersistentStubDocker(t, home)

	// Seed the stub as if a container from an older image already exists.
	if err := os.WriteFile(statePath, []byte("stale-image:old\n"), 0o644); err != nil {
		t.Fatalf("seed container state: %v", err)
	}

	remote, err := buildAgentPersistentExecCommand(imgTestAgentID, home, "echo", []string{"hi"}, false, imgTestRef)
	if err != nil {
		t.Fatalf("buildAgentPersistentExecCommand: %v", err)
	}
	if out, err := exec.Command("sh", "-c", remote).CombinedOutput(); err != nil {
		t.Fatalf("persistent exec failed: %v\noutput: %s", err, out)
	}

	log := readStubLog(t, logPath)
	if !strings.Contains(log, "VERB rm\n") {
		t.Errorf("a container built from a different image must be removed; docker log:\n%s", log)
	}
	if !strings.Contains(log, "VERB run\n") {
		t.Errorf("a container built from a different image must be recreated; docker log:\n%s", log)
	}
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("stub container state: %v", err)
	}
	if strings.TrimSpace(string(state)) != imgTestRef {
		t.Errorf("container recreated from %q, want %q", strings.TrimSpace(string(state)), imgTestRef)
	}
}

// TestPersistentListCommand_ReachesDockerWithLabelFilters proves the list
// script survives the two shell layers intact — including the Go template,
// whose braces and tabs are exactly the sort of payload a naive quoting
// scheme mangles.
func TestPersistentListCommand_ReachesDockerWithLabelFilters(t *testing.T) {
	home := t.TempDir()
	logPath, _ := installPersistentStubDocker(t, home)

	remote := buildAgentPersistentListCommand(imgTestAgentID, home, false)
	out, err := exec.Command("sh", "-c", remote).CombinedOutput()
	if err != nil {
		t.Fatalf("list command failed: %v\noutput: %s\ncommand: %s", err, out, remote)
	}
	log := readStubLog(t, logPath)
	want := "ARG ps\nARG -a\n" +
		"ARG --filter\nARG bunker.persistent=true\n" +
		"ARG --filter\nARG bunker.agent=" + imgTestAgentID + "\n" +
		"ARG --format\nARG {{.Names}}\t{{.Image}}\t{{.Status}}\t{{.CreatedAt}}\n"
	if !strings.Contains(log, want) {
		t.Errorf("docker ps argv drifted:\n got log: %s\nwant fragment: %s", log, want)
	}
	if !strings.Contains(string(out), "bunker-exec-abc123") {
		t.Errorf("list output must carry the container rows, got %q", out)
	}
}

// TestPersistentCleanupCommand_RemovesLabelledContainers proves cleanup is
// driven by the docker label filter (not by any server-side list) and that the
// empty case is stated rather than silent.
func TestPersistentCleanupCommand_RemovesLabelledContainers(t *testing.T) {
	home := t.TempDir()
	logPath, _ := installPersistentStubDocker(t, home)

	remote := buildAgentPersistentCleanupCommand(imgTestAgentID, home, false)
	out, err := exec.Command("sh", "-c", remote).CombinedOutput()
	if err != nil {
		t.Fatalf("cleanup command failed: %v\noutput: %s\ncommand: %s", err, out, remote)
	}
	log := readStubLog(t, logPath)
	if !strings.Contains(log, "VERB rm\nARG rm\nARG -f\nARG cid1\nARG cid2\n") {
		t.Errorf("cleanup must `docker rm -f` the labelled ids; docker log:\n%s", log)
	}
	if !strings.Contains(string(out), "bunker-exec-abc123") {
		t.Errorf("cleanup output must report the removed container, got %q", out)
	}

	// Empty case: no ids → the script states it and removes nothing.
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("reset stub log: %v", err)
	}
	emptyStub := `#!/bin/sh
LOG='` + logPath + `'
printf 'VERB %s\n' "$1" >> "$LOG"
printf 'ARG %s\n' "$@" >> "$LOG"
exit 0
`
	binDir := filepath.Join(home, "bin")
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(emptyStub), 0o755); err != nil {
		t.Fatalf("write empty stub docker: %v", err)
	}
	out, err = exec.Command("sh", "-c", remote).CombinedOutput()
	if err != nil {
		t.Fatalf("cleanup command (empty) failed: %v\noutput: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != persistentNoneMessage(imgTestAgentID) {
		t.Errorf("empty cleanup output = %q, want %q", strings.TrimSpace(string(out)), persistentNoneMessage(imgTestAgentID))
	}
	if strings.Contains(readStubLog(t, logPath), "ARG rm\n") {
		t.Errorf("empty cleanup must not invoke docker rm; docker log:\n%s", readStubLog(t, logPath))
	}
}

// ── 3. QA-BUNKER-83: agent-scoped TMPDIR ────────────────────────────────────

// TestPersistentExecsPinAgentScopedTmpDir is the QA-BUNKER-83 pin: NOTHING a
// persistent command runs may write temporary files into the shared session
// /tmp. Every family this file builds — the ensure/create snippet, the
// container payload behind `docker exec`, the list and cleanup host scripts,
// and the created container's own config env — must point TMPDIR at the
// agent's own data dir (a path under the agent home, which carries the agent
// id) and must never pin the shared /tmp itself. The outer env(1) wrapper's
// session-TMPDIR advertisement (buildAgentExecCommand, the default exec
// path's documented behavior) is superseded by this pin inside every
// persistent payload.
func TestPersistentExecsPinAgentScopedTmpDir(t *testing.T) {
	wantDir := agentDataTmpDir(imgTestHome) // /home/bunker-abc123/.bunker/tmp — carries the agent id
	// The pin renders in three forms depending on the quoting layer: bare as
	// the container config env pair (-e TMPDIR=<dir>), single-quoted in the
	// raw snippets this file authors (TMPDIR='<dir>'), and quote-escaped once
	// those snippets are embedded in the outer sh -c wrapper. All three must
	// be accepted; each family carries one.
	pinBare := "TMPDIR=" + wantDir
	pinQuoted := "TMPDIR=" + shellQuoteSingle(wantDir)
	// Each single quote of the quoted form is re-escaped by the outer
	// sh -c wrapper exactly as shellQuoteSingle escapes: ' -> '\''.
	pinEscaped := strings.ReplaceAll(pinQuoted, "'", "'\\''")
	hasPin := func(s string) bool {
		return strings.Contains(s, pinBare) || strings.Contains(s, pinQuoted) || strings.Contains(s, pinEscaped)
	}

	execPayload, err := buildAgentPersistentExecCommand(imgTestAgentID, imgTestHome, "go", []string{"build", "./..."}, false, imgTestRef)
	if err != nil {
		t.Fatalf("buildAgentPersistentExecCommand: %v", err)
	}
	scriptPayload, err := buildAgentPersistentScriptCommand(imgTestAgentID, imgTestHome, "#!/bin/sh\necho hi\n", false, imgTestRef)
	if err != nil {
		t.Fatalf("buildAgentPersistentScriptCommand: %v", err)
	}
	fullCommands := map[string]string{
		"exec command":     execPayload,
		"script command":   scriptPayload,
		"list command":     buildAgentPersistentListCommand(imgTestAgentID, imgTestHome, false),
		"cleanup command":  buildAgentPersistentCleanupCommand(imgTestAgentID, imgTestHome, false),
		"ensure snippet":   persistentEnsureSnippet(imgTestAgentID, imgTestHome, false, imgTestRef),
		"list script":      persistentListScript(imgTestAgentID, imgTestHome),
		"cleanup script":   persistentCleanupScript(imgTestAgentID, imgTestHome),
		"container config": strings.Join(persistentContainerRunArgv(imgTestAgentID, imgTestHome, false, imgTestRef), " "),
	}
	for name, got := range fullCommands {
		if !hasPin(got) {
			t.Errorf("%s does not pin TMPDIR to the agent data dir %s:\n%s", name, wantDir, got)
		}
		// The legacy per-agent runtime dir (GAP-075 superseded it for
		// sessions) must not come back as the persistent pin either.
		if strings.Contains(got, "TMPDIR=/run/bunker/"+imgTestAgentID+"/tmp") {
			t.Errorf("%s pins the legacy runtime-dir TMPDIR:\n%s", name, got)
		}
		if name == "exec command" || name == "script command" || name == "list command" || name == "cleanup command" {
			// These pass through the outer env(1) wrapper, whose session
			// TMPDIR advertisement (/tmp) is the default exec path's
			// documented contract (GAP-075, service.go — outside this
			// file's scope). The pin must come AFTER that advertisement:
			// in shell execution order the LAST assignment wins, so this
			// is the supersession assertion.
			if strings.LastIndex(got, pinEscaped) < strings.LastIndex(got, "TMPDIR=/tmp") {
				t.Errorf("%s does not supersede the session /tmp advertisement (pin must follow it):\n%s", name, got)
			}
		} else if strings.Contains(got, "TMPDIR=/tmp") {
			// Surfaces this file authors in full must not pin the shared
			// /tmp anywhere.
			t.Errorf("%s still pins the shared /tmp:\n%s", name, got)
		}
	}

	// Ordering: the pin must precede the first docker verb, so the ensure's
	// `docker run` and the container's inner shell both inherit it.
	pinIdx := strings.Index(execPayload, pinEscaped)
	ensureIdx := strings.Index(execPayload, "docker inspect -f")
	if pinIdx < 0 || ensureIdx < 0 || pinIdx > ensureIdx {
		t.Errorf("TMPDIR pin must precede the first docker verb:\n%s", execPayload)
	}

	// The container must be CREATED with the pin as a config env pair
	// (-e TMPDIR=...), so even a manual `docker exec` into
	// bunker-exec-<id> inherits the agent-scoped directory.
	argv := persistentContainerRunArgv(imgTestAgentID, imgTestHome, false, imgTestRef)
	pinned := false
	for i, a := range argv {
		if i > 0 && a == "-e" && argv[i+1] == pinBare {
			pinned = true
		}
	}
	if !pinned {
		t.Errorf("persistent run argv carries no -e %s element: %q", pinBare, argv)
	}
}

// TestPersistentExecTmpPinTakesEffectInShell runs the built persistent exec
// through a real shell with the stub docker (the same technique the reuse
// tests use) and asserts the CONTAINER shell actually observes the pinned
// TMPDIR — string presence is not the contract, the environment is.
func TestPersistentExecTmpPinTakesEffectInShell(t *testing.T) {
	home := t.TempDir()
	_, _ = installPersistentStubDocker(t, home)
	wantDir := agentDataTmpDir(home)

	remote, err := buildAgentPersistentExecCommand(imgTestAgentID, home, "sh", []string{"-c", "echo tmpdir=${TMPDIR-UNSET}"}, false, imgTestRef)
	if err != nil {
		t.Fatalf("buildAgentPersistentExecCommand: %v", err)
	}
	out, err := exec.Command("sh", "-c", remote).CombinedOutput()
	if err != nil {
		t.Fatalf("persistent exec failed: %v\noutput: %s\ncommand: %s", err, out, remote)
	}
	if got := strings.TrimSpace(string(out)); got != "tmpdir="+wantDir {
		t.Errorf("container shell saw TMPDIR as %q, want the agent data dir %q", got, wantDir)
	}
}

// TestPersistentExecTmpPinFallsBackWhenDataDirUnwritable is the QA-BUNKER-83
// fallback arm: when the agent's data dir cannot be created the exec must NOT
// crash — the pin unsets TMPDIR (downstream consumers fall back to the
// default temp directory of their context), warns on stderr, and the command
// still runs.
func TestPersistentExecTmpPinFallsBackWhenDataDirUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the unwritable-data-dir arm needs a non-root test user: root ignores mode 0555")
	}
	home := t.TempDir()
	// The stub docker must be installed while the home is still writable;
	// the fallback arm then makes the pin's mkdir fail WITHOUT the ensure
	// failing for an unrelated reason (a real docker daemon is never
	// contacted — the stub answers from <home>/bin, which is unaffected by
	// the .bunker subtree being uncreatable).
	_, _ = installPersistentStubDocker(t, home)
	if err := os.Chmod(home, 0o555); err != nil {
		t.Fatalf("chmod home read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o755) })

	remote, err := buildAgentPersistentExecCommand(imgTestAgentID, home, "sh", []string{"-c", "echo tmpdir=${TMPDIR-UNSET}"}, false, imgTestRef)
	if err != nil {
		t.Fatalf("buildAgentPersistentExecCommand: %v", err)
	}
	out, err := exec.Command("sh", "-c", remote).CombinedOutput()
	if err != nil {
		t.Fatalf("the fallback arm must not crash the exec: %v\noutput: %s", err, out)
	}
	// CombinedOutput interleaves stderr: assert the observed value and the
	// warning line by content, not by equality.
	if !strings.Contains(string(out), "tmpdir=UNSET\n") {
		t.Errorf("fallback left TMPDIR set; want the UNSET line in output:\n%s", out)
	}
	if strings.Contains(string(out), "tmpdir=/tmp") {
		t.Errorf("fallback must never re-point at the shared /tmp, output:\n%s", out)
	}
	if !strings.Contains(string(out), "falling back to the default temp dir") {
		t.Errorf("fallback must warn on stderr, got output:\n%s", out)
	}
}
