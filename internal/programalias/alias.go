// Package programalias implements the docker-as-installer program-alias
// registry (GAP-066).
//
// A program alias is a durable name -> container-image mapping that the
// daemon resolves for agent execs.  The point of the feature is to give an
// agent a program (yq, jq, a toolchain) WITHOUT installing it into the agent
// host with apt: the daemon maps the invocation onto a `docker run` of a
// pinned image, so the FIRST run pulls the image and every later run is a
// cache hit.
//
// Layering (all of it daemon-side — an agent never chooses an image, a
// mount, or a resource limit):
//
//   - Registry   — the durable alias store (this package).
//   - Validation — alias grammar, image grammar, and the HOME-ONLY mount
//     rule (ValidateMount).  A mount whose host path is not inside the
//     agent's home directory is refused, so an alias can never hand a
//     container a host path outside the agent's own tree.
//   - BuildDockerArgv — the argv factory.  Every flag the container runs
//     under (identity, mounts, workdir, resource limits, network) is
//     chosen here; the caller supplies only the alias and the user args.
//
// What this package deliberately does NOT do: it never touches a HOST docker
// socket.  The docker CLI it builds runs on the agent host as the agent user
// against the agent's OWN rootless dockerd socket
// (unix:///run/bunker/<agent>/docker.sock), and the container it starts is
// given no docker socket at all.  An alias therefore cannot escalate to the
// host daemon.
package programalias

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// NameMaxLen bounds an alias name (they become file names under the agent's
// ~/bin through the shim, so the grammar is also a file-name grammar).
const NameMaxLen = 64

// nameRe is the alias-name grammar: a leading alphanumeric, then
// alphanumerics plus `.`, `_`, `+`, `-`.  No `/`, no whitespace, no leading
// `-` (a leading dash would be read as a flag by every consumer).
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// imageRe is a pragmatic OCI/Docker image-reference grammar:
//
//	[registry[:port]/]path[:tag][@digest]
//
// It is intentionally stricter than "anything without spaces": an image ref
// is embedded into a shell command line, so a metacharacter here would be a
// command-injection seam.  Only characters that are legal in a reference are
// admitted.
var imageRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?(:[0-9]{1,5})?(/[A-Za-z0-9][A-Za-z0-9._-]*)*(:[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?(@sha256:[a-f0-9]{64})?$`)

// Errors returned by the validation helpers.  They are sentinel so the daemon
// can classify a caller mistake (CodeInvalidArgument) without string
// matching, and so tests can assert the refusal precisely.
var (
	// ErrNameRequired / ErrNameInvalid refuse a malformed alias name.
	ErrNameRequired = errors.New("alias name is required")
	ErrNameInvalid  = errors.New("alias name must start with a letter or digit and contain only letters, digits, '.', '_', '+', '-'")
	// ErrImageRequired / ErrImageInvalid refuse a malformed or unsafe image
	// reference.
	ErrImageRequired = errors.New("image reference is required")
	ErrImageInvalid  = errors.New("image reference is not a valid OCI reference")
	// ErrArgInvalid refuses an entrypoint/argument token that could not be
	// passed to `docker run` verbatim.
	ErrArgInvalid = errors.New("entrypoint entries must be non-empty and free of NUL bytes")
	// ErrMountNotUnderHome is the HOME-ONLY rule at exec time.  It is the
	// refusal an operator sees when they try to hand a container a host path
	// outside the target agent's own tree.
	ErrMountNotUnderHome = errors.New("mount host path must be inside the agent's home directory")
	// ErrMountOutsideHomeRoot is the REGISTRATION-time half of the same rule:
	// bunkerd homes live under AgentHomeRoot, so a mount that does not even
	// start inside that root can never be valid for any agent and is refused
	// where the operator can see it rather than at some later exec.
	ErrMountOutsideHomeRoot = errors.New("mount host path must be inside an agent home (" + AgentHomeRoot + "/bunker-<id>)")
	// ErrMountHostRequired / ErrMountHostRelative refuse a mount that does not
	// name an absolute host path.
	ErrMountHostRequired = errors.New("mount host path is required")
	ErrMountHostRelative = errors.New("mount host path must be absolute")
	// ErrMountContainerRelative refuses a container path that is not absolute.
	ErrMountContainerRelative = errors.New("mount container path must be absolute")
	// ErrMountDockerSock refuses any mount that names a docker socket.  The
	// existing image-spec exec path bind-mounts the agent runtime directory
	// (which contains docker.sock) into the container; a program alias must
	// never do that — GAP-066's "no docker socket into agents" rule is
	// enforced here, not merely documented.
	ErrMountDockerSock = errors.New("mount must not expose a docker socket")
)

// Alias is one registered program: a name mapped to a container image plus
// the argv prefix the program runs as.
type Alias struct {
	// Name is the program name an agent types, e.g. "yq".  It is also the
	// name of the shim installed into the agent's ~/bin.
	Name string `json:"name"`
	// Image is the container image the program runs out of.  It is pulled by
	// the daemon on first use.
	Image string `json:"image"`
	// Entrypoint is the program the container enters through.  It maps onto
	// docker's own `--entrypoint` flag: Entrypoint[0] becomes `--entrypoint
	// <it>` and any remaining elements are prepended to the caller's
	// arguments.  Empty means "use the image's own ENTRYPOINT/CMD" and the
	// caller's arguments are passed straight through.
	//
	// Mapping to the REAL flag (rather than appending a command vector after
	// the image) is what makes `--entrypoint yq` on an image whose entrypoint
	// is already `yq` behave the way an operator expects; appending would run
	// the program twice (`yq yq --version`).
	Entrypoint []string `json:"entrypoint,omitempty"`
	// Mounts are extra bind mounts.  Every mount's HOST path must be inside
	// the agent's home directory (ValidateMount); the home itself is always
	// mounted at its own absolute path by BuildDockerArgv and does not need
	// to be listed here.
	Mounts []Mount `json:"mounts,omitempty"`
	// Network opens the container's network when true.  Default false, which
	// builds `--network none`: a version query or a text filter needs no
	// egress, and the least-privilege default is the safe one.  Network is a
	// deliberate per-alias opt-in (e.g. a toolchain alias that must fetch
	// modules).
	Network bool `json:"network,omitempty"`
	// Description is operator-facing documentation for `bunker alias list`.
	Description string `json:"description,omitempty"`
}

// Mount is one extra bind mount for an alias-managed container.
type Mount struct {
	// Host is the absolute host path to bind.  Must be inside the agent home.
	Host string `json:"host"`
	// Container is the absolute path inside the container; empty means "same
	// path as Host", which is what keeps host-absolute paths valid
	// in-container (the same contract the image-spec exec path uses).
	Container string `json:"container,omitempty"`
	// ReadOnly adds the `:ro` mount option.
	ReadOnly bool `json:"read_only,omitempty"`
}

// ContainerPath returns the effective container-side path of a mount.
func (m Mount) ContainerPath() string {
	if m.Container == "" {
		return m.Host
	}
	return m.Container
}

// ValidateAlias checks an alias for the registry's own invariants (name and
// image grammar, entrypoint tokens).  Mount validation is a separate call
// because it needs the agent's home directory, which is only known at exec
// time (ValidateMount).
func ValidateAlias(a Alias) error {
	if err := ValidateName(a.Name); err != nil {
		return err
	}
	if err := ValidateImage(a.Image); err != nil {
		return err
	}
	for _, tok := range a.Entrypoint {
		if strings.TrimSpace(tok) == "" || strings.ContainsRune(tok, 0) {
			return fmt.Errorf("%w: %q", ErrArgInvalid, tok)
		}
	}
	for _, m := range a.Mounts {
		if err := ValidateMountShape(m); err != nil {
			return err
		}
	}
	return nil
}

// ValidateName checks the alias-name grammar.
func ValidateName(name string) error {
	if name == "" {
		return ErrNameRequired
	}
	if len(name) > NameMaxLen || !nameRe.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrNameInvalid, name)
	}
	return nil
}

// ValidateImage checks the image-reference grammar.
func ValidateImage(image string) error {
	if image == "" {
		return ErrImageRequired
	}
	if !imageRe.MatchString(image) {
		return fmt.Errorf("%w: %q", ErrImageInvalid, image)
	}
	return nil
}

// AgentHomeRoot is the parent directory bunkerd creates agent homes under
// (`/home/bunker-<agent-id>`).  It is the registration-time bound for the
// HOME-ONLY mount rule: a mount whose host path is not under this root can
// never be valid for any agent, so the daemon refuses it at registration
// instead of storing an alias that can only ever fail at exec time.
const AgentHomeRoot = "/home"

// agentHomePrefix is the exact directory-name prefix of an agent home.
const agentHomePrefix = AgentHomeRoot + "/bunker-"

// ValidateMountShape checks a mount's SHAPE only (absolute paths, no docker
// socket, inside the agent-home root).  It is the half of the rule that does
// not need a specific agent, and it runs at registration time so a mount that
// can never be valid is refused where the operator can see it.
func ValidateMountShape(m Mount) error {
	if m.Host == "" {
		return ErrMountHostRequired
	}
	if !filepath.IsAbs(m.Host) {
		return fmt.Errorf("%w: %q", ErrMountHostRelative, m.Host)
	}
	if m.Container != "" && !filepath.IsAbs(m.Container) {
		return fmt.Errorf("%w: %q", ErrMountContainerRelative, m.Container)
	}
	// The docker-socket refusal is checked FIRST so a socket path outside the
	// home root reports the more specific reason.
	if exposesDockerSocket(m.Host) || exposesDockerSocket(m.Container) {
		return fmt.Errorf("%w: %q", ErrMountDockerSock, m.Host)
	}
	if !underAgentHomeRoot(m.Host) {
		return fmt.Errorf("%w: %q", ErrMountOutsideHomeRoot, m.Host)
	}
	return nil
}

// underAgentHomeRoot reports whether a cleaned absolute path starts inside an
// agent home directory (`/home/bunker-...`).
func underAgentHomeRoot(p string) bool {
	clean := filepath.Clean(p)
	return clean == strings.TrimSuffix(agentHomePrefix, "-") || strings.HasPrefix(clean, agentHomePrefix)
}

// ValidateMount is the HOME-ONLY rule at exec time: the mount's host path
// must resolve inside the agent's home directory.  home must be an absolute
// path (the daemon always passes /home/bunker-<id>).
//
// The check is lexical and deliberately NOT symlink-following: the agent
// owns its home and may place a symlink there at any time, so resolving
// symlinks at exec-build time would be a false guarantee.  What the rule
// actually guarantees is that no host path OUTSIDE the agent's home can be
// named as a mount source, which is the escalation this feature must not
// create.
func ValidateMount(m Mount, home string) error {
	if err := ValidateMountShape(m); err != nil {
		return err
	}
	if home == "" || !filepath.IsAbs(home) {
		return fmt.Errorf("agent home %q is not an absolute path", home)
	}
	cleanHome := filepath.Clean(home)
	cleanHost := filepath.Clean(m.Host)
	if cleanHost == cleanHome || strings.HasPrefix(cleanHost, cleanHome+string(filepath.Separator)) {
		return nil
	}
	return fmt.Errorf("%w: %q is not inside %q", ErrMountNotUnderHome, m.Host, cleanHome)
}

// ValidateMounts applies ValidateMount to every mount of an alias.
func ValidateMounts(mounts []Mount, home string) error {
	for i, m := range mounts {
		if err := ValidateMount(m, home); err != nil {
			return fmt.Errorf("mount[%d]: %w", i, err)
		}
	}
	return nil
}

// exposesDockerSocket reports whether a path names a docker socket (or the
// runtime directory that carries them).
func exposesDockerSocket(p string) bool {
	if p == "" {
		return false
	}
	clean := filepath.Clean(p)
	if filepath.Base(clean) == "docker.sock" {
		return true
	}
	// /run/bunker/<id> carries the per-agent dockerd socket; refuse the whole
	// tree so neither the socket file nor a parent directory can be mounted.
	return clean == "/run/bunker" || strings.HasPrefix(clean, "/run/bunker"+string(filepath.Separator))
}
