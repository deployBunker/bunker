package server

// SURF-012: persistent-container exec mode.
//
// Today an image-backed `bunker exec` runs its command inside a FRESH
// `docker run --rm` container (GAP-069), so the container writable layer —
// Go module/build caches, apt-installed toolchains, anything written outside
// the bind-mounted home — evaporates between invocations. The measured
// consequence (SURF-012, dedi-2 agent km1): two consecutive execs reported
// different hostnames, `/tmp` and `/root` markers were gone, and every
// building exec had to re-run `apt-get install gcc g++` from cold.
//
// This file adds an OPT-IN persistent container per agent:
//
//   - name    bunker-exec-<agent_id>            (deterministic, one per agent)
//   - labels  bunker.persistent=true            (the marker `--list-persistent`/
//            bunker.agent=<agent_id>            `--cleanup-persistent` select on)
//   - create  docker run -d ... <image> sleep infinity
//   - reuse   the ensure snippet below: recreate only when the existing
//             container's image ref no longer matches the agent's current
//             image ref, then `docker exec` into it.
//
// The marker lives ON THE CONTAINER, inside the agent's own rootless dockerd
// — not in bunkerd state — so it survives a bunkerd restart by construction.
// Nothing here changes the default path: PERSISTENT_MODE_NONE still produces
// the byte-identical one-shot command it produced before this file existed.
//
// QA-BUNKER-83: nothing a persistent exec runs may write temporary files into
// the shared session /tmp. Every command family in this file re-pins TMPDIR
// to the agent's OWN data dir (agentDataTmpDir: <home>/.bunker/tmp, a path
// that carries the agent id) — the ensure/list/cleanup snippets before the
// docker CLI or a host-context script runs, the container payload before the
// user command runs, and the created container itself via -e TMPDIR so even a
// manual `docker exec` into bunker-exec-<id> inherits the pin. When the data
// dir is not writable the pin unsets TMPDIR (consumers then fall back to the
// default session temp directory), prints a warning to stderr, and never
// aborts the exec.

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// persistentContainerPrefix names a persistent exec container. The name is
	// derived from the agent id alone, so the container is addressable without
	// any server-side bookkeeping (and `docker exec` needs no lookup).
	persistentContainerPrefix = "bunker-exec-"
	// persistentLabelKey marks a container as a persistent-exec container.
	// `--list-persistent` / `--cleanup-persistent` filter on it, which is why
	// the label value is the literal "true" rather than a per-agent token.
	persistentLabelKey = "bunker.persistent"
	// persistentAgentLabelKey scopes a container to one agent. The name carries
	// the agent too, but a label makes the scope a first-class docker filter
	// instead of a name-prefix match.
	persistentAgentLabelKey = "bunker.agent"
	// persistentLabelValue is the fixed value of persistentLabelKey.
	persistentLabelValue = "true"
)

// persistentContainerName returns the one deterministic container name for an
// agent's persistent exec container.
func persistentContainerName(agentID string) string {
	return persistentContainerPrefix + agentID
}

// shellJoin renders an argv as a single shell command string: every element is
// POSIX single-quoted and joined with spaces. The result is safe to embed in
// the command string the exec builders hand to `sh -c` (they single-quote that
// whole string again, and shellQuoteSingle escapes the inner quotes), while
// still being word-split back into the original argv by that inner shell.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuoteSingle(a)
	}
	return strings.Join(parts, " ")
}

// agentDataTmpDir returns the agent-scoped temporary directory a persistent
// exec pins TMPDIR to (QA-BUNKER-83): <userHome>/.bunker/tmp. The path is
// derived from the agent's own home (which carries the agent id), lives under
// the agent's data dir rather than any shared /tmp, and — because the home is
// bind-mounted into the container at the same absolute path — is valid at the
// identical path on both sides of every `docker exec` in this file.
func agentDataTmpDir(userHome string) string {
	return filepath.Join(userHome, ".bunker", "tmp")
}

// agentDataTmpPinSnippet returns the POSIX-sh fragment that re-points TMPDIR
// at the agent's own data dir for everything that runs after it in the same
// shell (QA-BUNKER-83). It is composed, not one hardcoded string, so both
// halves are individually assertable: the mkdir must succeed (a `set -e`
// payload dies here rather than silently writing into a shared directory),
// and the assignment supersedes whatever TMPDIR the outer env(1) wrapper
// already advertised (the session /tmp on the default exec path).
//
// Fallback (QA-BUNKER-83 acceptance 4): when the data dir cannot be created
// the pin does NOT abort — it unsets TMPDIR, so downstream consumers fall
// back to the default temp directory of the context they run in, prints a
// one-line warning to stderr, and continues. Never crashing the exec is the
// contract: an agent whose home is temporarily unwritable must still exec.
func agentDataTmpPinSnippet(userHome string) string {
	return "if mkdir -p " + shellQuoteSingle(agentDataTmpDir(userHome)) +
		" 2>/dev/null; then TMPDIR=" + shellQuoteSingle(agentDataTmpDir(userHome)) +
		"; export TMPDIR; else unset TMPDIR; echo 'bunker: agent data dir " +
		agentDataTmpDir(userHome) + " not writable; falling back to the default temp dir' >&2; fi; "
}

// applyAgentDataTmpPin prefixes a shell payload with the agent-scoped TMPDIR
// pin (QA-BUNKER-83): everything the payload runs after this point — the
// docker CLI, the container's inner shell, the user command — inherits the
// agent's own temporary directory instead of the shared session /tmp.
func applyAgentDataTmpPin(userHome, payload string) string {
	return agentDataTmpPinSnippet(userHome) + payload
}

// persistentContainerRunArgv is the `docker run -d` argv that CREATES the
// persistent container. It mirrors buildAgentImageExecCommand's run argv —
// same home bind at the same absolute path, same agent runtime-dir bind, same
// namespace-resolved agent identity (--user 0) and socket env — with two
// deliberate differences: `-d` + `sleep infinity` instead of `--rm` + a
// one-shot command, and the persistent name/labels. Keeping the mounts and
// identity identical is what makes a persistent exec a drop-in for a one-shot
// exec from the command's point of view.
func persistentContainerRunArgv(agentID, userHome string, disclosed bool, imageRef string) []string {
	argv := []string{
		"docker", "run", "-d",
		"--name", persistentContainerName(agentID),
		"--label", persistentLabelKey + "=" + persistentLabelValue,
		"--label", persistentAgentLabelKey + "=" + agentID,
	}
	// GAP-067 containment disclosure: the same in-container marker the one-shot
	// path carries, so an in-container `bunker` view is identical in both modes.
	if disclosed {
		argv = append(argv, "-e", containmentSandboxEnv)
	}
	// QA-BUNKER-83: the container is CREATED with the agent-scoped TMPDIR in
	// its config env, so `docker exec` shells inherit it even when a payload's
	// own shell pin cannot reach them (e.g. a manual `docker exec` into
	// bunker-exec-<id>). The home bind makes the path valid in-container at
	// the same absolute path it has on the host.
	argv = append(argv, "-e", "TMPDIR="+agentDataTmpDir(userHome))
	argv = append(argv, "-v", userHome+":"+userHome, "-w", userHome)
	argv = append(argv, agentRuntimeDirBind(agentID)...)
	argv = applyImageAgentRuntime(argv, agentID)
	return append(argv, imageRef, "sleep", "infinity")
}

// persistentEnsureSnippet returns the shell snippet that guarantees the
// agent's persistent container exists, is running, and was created from the
// image ref the agent currently declares.
//
// The image comparison is what keeps a re-spawn honest: an agent destroyed and
// re-spawned with a different image spec must not exec into a container built
// from the previous image (its toolchain and mounts would be the old agent's).
// A container whose whole point is to survive across invocations is exactly
// the one that outlives a re-spawn, so it has to be validated, not assumed.
// `docker start` afterwards covers the stopped-but-matching case (host or
// dockerd restart, which stops orphan containers) without recreating the
// writable layer.
func persistentEnsureSnippet(agentID, userHome string, disclosed bool, imageRef string) string {
	name := persistentContainerName(agentID)
	runCmd := shellJoin(persistentContainerRunArgv(agentID, userHome, disclosed, imageRef))
	// QA-BUNKER-83: the ensure snippet runs in the agent's outer session shell
	// (whose env(1) advertisement may still be the shared session /tmp) and it
	// creates the container, so it is pinned first — the `docker run` it emits
	// then carries the agent-scoped TMPDIR into the container config too.
	return applyAgentDataTmpPin(userHome, fmt.Sprintf(
		"if [ \"$(docker inspect -f %s %s 2>/dev/null)\" != %s ]; then "+
			"docker rm -f %s >/dev/null 2>&1 || true; "+
			"%s >/dev/null || exit 1; fi; "+
			"docker start %s >/dev/null 2>&1 || true",
		shellQuoteSingle("{{.Config.Image}}"), shellQuoteSingle(name), shellQuoteSingle(imageRef),
		shellQuoteSingle(name), runCmd, shellQuoteSingle(name)))
}

// persistentExecTail returns the snippet fragment that runs `payload` inside
// the agent's persistent container: `docker exec -w <home> <name> sh -lc
// '<payload>'`. `exec` replaces the host-side shell so the container command's
// exit status is the exec's exit status (ssh-style propagation preserved).
func persistentExecTail(agentID, userHome, payload string) string {
	return "; exec docker exec -w " + shellQuoteSingle(userHome) + " " +
		shellQuoteSingle(persistentContainerName(agentID)) + " sh -lc " + shellQuoteSingle(payload)
}

// buildAgentPersistentExecCommand is the shell-mode persistent exec: ensure the
// container, then run the user command inside it. The user command runs behind
// imageInnerSourcePrefix so `bunker env set` injections are visible
// in-container exactly as they are on the one-shot path (DF-BUNKER-77-IMGEXEC).
func buildAgentPersistentExecCommand(agentID, userHome, command string, args []string, disclosed bool, imageRef string) (string, error) {
	if _, ok := agentContainerIdentityFlag(agentID); !ok {
		return "", errAgentUserUnresolved
	}
	payload := applyAgentDataTmpPin(userHome, imageInnerSourcePrefix(agentID)+buildAgentRemoteCmd(command, args))
	snippet := persistentEnsureSnippet(agentID, userHome, disclosed, imageRef) +
		persistentExecTail(agentID, userHome, payload)
	return buildAgentExecCommand(agentID, userHome, snippet, nil, disclosed), nil
}

// buildAgentPersistentScriptCommand is the script-mode persistent exec. The
// upload flow is the one-shot image path's (heredoc into the agent home, which
// the container sees at the same absolute path because the home is
// bind-mounted there); only the final invocation changes — ensure the
// container, then run the uploaded script inside it.
func buildAgentPersistentScriptCommand(agentID, userHome, scriptContent string, disclosed bool, imageRef string) (string, error) {
	if _, ok := agentContainerIdentityFlag(agentID); !ok {
		return "", errAgentUserUnresolved
	}
	scriptPath := testScriptPathOverride
	if scriptPath == "" {
		scriptPath = filepath.Join(userHome, ".bunker", "exec-script.sh")
	}
	escaped := strings.ReplaceAll(scriptContent, "'", "'\\''")
	payload := "set -e; " + applyAgentDataTmpPin(userHome, imageInnerSourcePrefix(agentID)+". "+shellQuoteSingle(scriptPath))
	snippet := persistentEnsureSnippet(agentID, userHome, disclosed, imageRef) +
		persistentExecTail(agentID, userHome, payload)
	return fmt.Sprintf(
		"mkdir -p %q && cat > %q <<'EOFSCRIPT'\n%s\nEOFSCRIPT\nchmod +x %q && %s",
		filepath.Dir(scriptPath), scriptPath, escaped, scriptPath,
		buildAgentExecCommand(agentID, userHome, snippet, nil, disclosed),
	), nil
}

// persistentListScript is the shell script that reports the agent's persistent
// containers. It runs in the HOST context (the docker CLI on the agent host,
// pointed at the agent's own socket by the surrounding env wrapper), not
// inside a container — the container image may have no docker client at all
// (DF-BUNKER-80), and listing must work regardless. QA-BUNKER-83: it is
// prefixed with the agent-scoped TMPDIR pin so its own temporary files land in
// the agent's data dir too.
func persistentListScript(agentID, userHome string) string {
	return applyAgentDataTmpPin(userHome, "docker ps -a --filter "+
		shellQuoteSingle(persistentLabelKey+"="+persistentLabelValue)+
		" --filter "+shellQuoteSingle(persistentAgentLabelKey+"="+agentID)+
		" --format "+shellQuoteSingle("{{.Names}}	{{.Image}}	{{.Status}}	{{.CreatedAt}}"))
}

// persistentCleanupScript is the shell script that destroys the agent's
// persistent containers. It is deliberately `docker rm -f` over the ids the
// same label filter selects, so cleanup is driven by the durable marker on the
// containers rather than by any list this process happens to hold. An agent
// with none prints the same sentence the CLI would, so the command is never
// silent about what it found. QA-BUNKER-83: cleanup is the verb most likely
// to run AFTER an agent's destroy (its residue can outlive the user), so it
// carries the agent-scoped TMPDIR pin as well — its temporary files land in
// the agent's own data dir, never in the shared host /tmp, even for an agent
// that no longer has a live session.
func persistentCleanupScript(agentID, userHome string) string {
	return applyAgentDataTmpPin(userHome, "ids=$(docker ps -aq --filter "+
		shellQuoteSingle(persistentLabelKey+"="+persistentLabelValue)+
		" --filter "+shellQuoteSingle(persistentAgentLabelKey+"="+agentID)+"); "+
		"if [ -z \"$ids\" ]; then printf '%s\\n' "+
		shellQuoteSingle(persistentNoneMessage(agentID))+"; else docker rm -f $ids; fi")
}

// persistentNoneMessage is the one sentence both the cleanup script and the
// CLI print when an agent has no persistent containers.
func persistentNoneMessage(agentID string) string {
	return "no persistent containers for agent " + agentID
}

// buildAgentPersistentListCommand wraps persistentListScript in the host-context
// exec builder (env file sourced, DOCKER_HOST/PATH/TMPDIR set) so the docker
// CLI reaches the agent's own rootless socket.
func buildAgentPersistentListCommand(agentID, userHome string, disclosed bool) string {
	return buildAgentExecCommand(agentID, userHome, persistentListScript(agentID, userHome), nil, disclosed)
}

// buildAgentPersistentCleanupCommand is buildAgentPersistentListCommand's
// cleanup twin.
func buildAgentPersistentCleanupCommand(agentID, userHome string, disclosed bool) string {
	return buildAgentExecCommand(agentID, userHome, persistentCleanupScript(agentID, userHome), nil, disclosed)
}

// ── ssh command builders ────────────────────────────────────────────────────

// buildExecSSHPersistentImage is the shell-mode persistent exec command:
// buildAgentPersistentExecCommand inside `sh -c '<...>'` over ssh, the same
// transport shape the one-shot image builder uses.
func buildExecSSHPersistentImage(ctx context.Context, agentID, sshKeyPath, userHome, command string, args []string, disclosed bool, imageRef string) (*exec.Cmd, error) {
	wrapped, err := buildAgentPersistentExecCommand(agentID, userHome, command, args, disclosed, imageRef)
	if err != nil {
		return nil, err
	}
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, fmt.Sprintf("sh -c %s", shellQuoteSingle(wrapped))), nil
}

// buildExecSSHPersistentScriptImage is the script-mode persistent exec command.
func buildExecSSHPersistentScriptImage(ctx context.Context, agentID, sshKeyPath, userHome, scriptContent string, disclosed bool, imageRef string) (*exec.Cmd, error) {
	wrapped, err := buildAgentPersistentScriptCommand(agentID, userHome, scriptContent, disclosed, imageRef)
	if err != nil {
		return nil, err
	}
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, fmt.Sprintf("sh -c %s", shellQuoteSingle(wrapped))), nil
}

// buildExecSSHPersistentList is the `--list-persistent` command.
func buildExecSSHPersistentList(ctx context.Context, agentID, sshKeyPath, userHome string, disclosed bool) *exec.Cmd {
	wrapped := buildAgentPersistentListCommand(agentID, userHome, disclosed)
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, fmt.Sprintf("sh -c %s", shellQuoteSingle(wrapped)))
}

// buildExecSSHPersistentCleanup is the `--cleanup-persistent` command.
func buildExecSSHPersistentCleanup(ctx context.Context, agentID, sshKeyPath, userHome string, disclosed bool) *exec.Cmd {
	wrapped := buildAgentPersistentCleanupCommand(agentID, userHome, disclosed)
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, fmt.Sprintf("sh -c %s", shellQuoteSingle(wrapped)))
}
