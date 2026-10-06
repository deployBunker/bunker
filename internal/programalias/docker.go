package programalias

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Limits are the resource ceilings applied to every alias container.  They
// are shaped from the agent's own spawn-time envelope so an alias can never
// outgrow the agent that runs it.
type Limits struct {
	// MemoryBytes caps the container's memory (docker --memory).  Zero means
	// DefaultMemoryBytes.
	MemoryBytes int64
	// CPUs caps the container's CPU (docker --cpus).  Zero means
	// DefaultCPUs.
	CPUs float64
	// Pids caps the process count (docker --pids-limit).  Zero means
	// DefaultPids.
	Pids int
}

// Defaults applied whenever the agent record carries no explicit ceiling.
// They are deliberately modest: an alias program is a utility (a filter, a
// version query, a compile), not a workload that should be able to take the
// whole box.
const (
	DefaultMemoryBytes int64 = 2 << 30 // 2 GiB
	DefaultCPUs              = 2.0
	DefaultPids              = 512
)

// normalized returns the effective limits with every zero replaced.
func (l Limits) normalized() Limits {
	if l.MemoryBytes <= 0 {
		l.MemoryBytes = DefaultMemoryBytes
	}
	if l.CPUs <= 0 {
		l.CPUs = DefaultCPUs
	}
	if l.Pids <= 0 {
		l.Pids = DefaultPids
	}
	return l
}

// DockerBinary is the program the built argv invokes.  It is a constant so
// tests can assert on it and so the daemon has exactly one place to change if
// the container runtime is ever swapped.
const DockerBinary = "docker"

// dockerRunFlags are the fixed flags every alias container runs under.
// They are chosen here, once, and a caller can never influence them except
// through the Alias fields that are validated above.
//
//   - --rm          : a one-shot program run leaves no container behind.
//   - -i            : stdin is attached, so a program that reads a pipe
//     behaves exactly like the native binary (stdio parity).
//   - --user 0      : inside the agent's own rootless user namespace the
//     agent IS uid 0, and namespace uid 0 owns exactly what the agent owns
//     (the same identity the image-spec exec path uses, DF-BUNKER-77).
//     Files the container writes into the home are therefore owned by the
//     agent — native-run parity.
//
// Note what is ABSENT: no `-v /run/bunker/<id>:...` (that is the bind that
// would hand the container the agent's docker socket), and no `--privileged`
// or `--pid host`.  A program alias has no docker access.
func dockerRunFlags(limits Limits) []string {
	l := limits.normalized()
	return []string{
		"--rm",
		"-i",
		"--user", "0",
		"--memory", strconv.FormatInt(l.MemoryBytes, 10),
		"--cpus", strconv.FormatFloat(l.CPUs, 'f', -1, 64),
		"--pids-limit", strconv.Itoa(l.Pids),
	}
}

// ValidateContainerEnv checks the `K=V` pairs the daemon injects into the
// container.  They are argv elements, so an empty key or a NUL is refused;
// the values themselves are the daemon's own (never agent-supplied).
func ValidateContainerEnv(env []string) error {
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 || strings.ContainsRune(kv, 0) {
			return fmt.Errorf("%w: %q", ErrArgInvalid, kv)
		}
	}
	return nil
}

// BuildDockerArgv builds the complete `docker run` argv for one alias
// invocation.
//
//	argv[0]      = "docker"
//	argv[1:]     = run flags, mounts, image, entrypoint, user args
//
// home is the agent's home directory.  It is ALWAYS bind-mounted at its own
// absolute path and used as the working directory, which is what makes
// host-absolute paths valid inside the container (the parity contract the
// image-spec exec path established).  Every EXTRA mount on the alias is
// validated against home first, so a non-home mount is refused before any
// argv exists.
//
// env are `K=V` pairs injected into the CONTAINER (never into the host-side
// docker CLI) — the daemon uses this for the containment-disclosure marker.
func BuildDockerArgv(a Alias, home string, args []string, limits Limits, env []string) ([]string, error) {
	if err := ValidateAlias(a); err != nil {
		return nil, err
	}
	if err := ValidateContainerEnv(env); err != nil {
		return nil, err
	}
	if home == "" || !strings.HasPrefix(home, "/") {
		return nil, fmt.Errorf("agent home %q must be an absolute path", home)
	}
	if err := ValidateMounts(a.Mounts, home); err != nil {
		return nil, err
	}

	argv := make([]string, 0, 24+len(args))
	argv = append(argv, DockerBinary)
	argv = append(argv, "run")
	argv = append(argv, dockerRunFlags(limits)...)
	if !a.Network {
		argv = append(argv, "--network", "none")
	}
	for _, kv := range env {
		argv = append(argv, "-e", kv)
	}
	// The agent home, at its own absolute path, as the working directory.
	argv = append(argv, "-v", home+":"+home, "-w", home)
	// Extra mounts, validated home-only above.
	for _, m := range a.Mounts {
		spec := m.Host + ":" + m.ContainerPath()
		if m.ReadOnly {
			spec += ":ro"
		}
		argv = append(argv, "-v", spec)
	}
	if len(a.Entrypoint) > 0 {
		// Docker's own --entrypoint: one program, flags before the image.
		argv = append(argv, "--entrypoint", a.Entrypoint[0])
	}
	argv = append(argv, a.Image)
	if len(a.Entrypoint) > 1 {
		// Docker's --entrypoint takes exactly one program; the remaining
		// elements of the alias's entrypoint vector are its leading arguments.
		argv = append(argv, a.Entrypoint[1:]...)
	}
	argv = append(argv, args...)
	return argv, nil
}

// Shim renders the ~/bin/<name> wrapper for one alias.
//
// The shim is a POSIX shell script that execs the docker argv with the
// caller's arguments appended.  Because it `exec`s, the shim process IS the
// docker CLI process: stdout, stderr and the exit code are docker's, with no
// shell in between to alter them.
//
// $@ is expanded quoted, so arguments with spaces or newlines cross intact.
// DOCKER_HOST is inherited from the agent session env (the agent's OWN
// rootless socket); if it is somehow unset the shim falls back to the
// conventional per-agent socket path rather than silently talking to a host
// daemon.
//
// A shim is what makes the alias callable from ANYTHING running inside the
// agent — not just a daemon-mediated exec — so a shell script the agent
// writes (`yq -r .a file.yaml`) resolves the same program the daemon would
// have run.
func Shim(a Alias, home string, limits Limits, env []string) (string, error) {
	if err := ValidateAlias(a); err != nil {
		return "", err
	}
	if err := ValidateMounts(a.Mounts, home); err != nil {
		return "", err
	}
	// Build the argv with a placeholder for the user args, quote each element
	// and splice "$@" in at the placeholder.  Building it the same way the
	// daemon does keeps the two entry points from drifting.
	argv, err := BuildDockerArgv(a, home, nil, limits, env)
	if err != nil {
		return "", err
	}
	quoted := make([]string, 0, len(argv)+1)
	for _, tok := range argv {
		quoted = append(quoted, shellQuote(tok))
	}
	quoted = append(quoted, `"$@"`)

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# bunker program alias: " + a.Name + " -> " + a.Image + "\n")
	b.WriteString("# Generated by bunkerd (GAP-066). Do not edit: the daemon rewrites it.\n")
	b.WriteString(`if [ -z "${DOCKER_HOST:-}" ]; then` + "\n")
	b.WriteString(`  DOCKER_HOST="unix:///run/bunker/${BUNKER_AGENT_ID:-$(id -un | sed 's/^bunker-//')}/docker.sock"` + "\n")
	b.WriteString("  export DOCKER_HOST\n")
	b.WriteString("fi\n")
	b.WriteString("exec " + strings.Join(quoted, " ") + "\n")
	body := b.String()
	// The revision marker is derived from the body WITHOUT the marker, so the
	// hash is stable and self-describing: the install step greps the marker
	// out of the file it finds on disk and compares.
	rev := sha256.Sum256([]byte(body))
	marker := shimRevMarker + hex.EncodeToString(rev[:])[:16]
	return "#!/bin/sh\n" + marker + "\n" + strings.TrimPrefix(body, "#!/bin/sh\n"), nil
}

// shimRevOf returns the revision marker embedded in a rendered shim, or ""
// when the shim carries none.
func shimRevOf(shim string) string {
	for _, line := range strings.Split(shim, "\n") {
		if strings.HasPrefix(line, shimRevMarker) {
			return line
		}
	}
	return ""
}

// ShimRev returns the revision marker embedded in a rendered shim (the full
// "# rev=<hex>" line), or "" when the shim carries none.  It is exported so
// the daemon can report which revision an agent's ~/bin shim is at.
func ShimRev(shim string) string { return shimRevOf(shim) }

// shellQuote wraps s in single quotes, escaping embedded single quotes for
// POSIX sh (: ' -> '\”).  Every element of the docker argv is quoted, so no
// alias field can inject a shell token into the shim.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
