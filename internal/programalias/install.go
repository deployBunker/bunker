package programalias

import (
	"strings"
	"sync"
)

// shimRevMarker is the marker line the shim carries so the install step can
// tell "already current" from "needs a rewrite" with grep alone (grep is
// guaranteed present; cmp/process-substitution are not).
const shimRevMarker = "# rev="

// RemoteExecScript renders the complete POSIX shell script the daemon runs on
// the agent host for one alias invocation.
//
// It does two things, in this order:
//
//  1. Installs (or refreshes) the alias shim at <home>/bin/<name>.  The step
//     is idempotent: the shim carries its own revision marker and is only
//     rewritten when that marker is absent, which is what makes the second
//     run of an alias a pure cache hit.
//  2. `exec`s the shim with the caller's arguments.
//
// Running the shim rather than a second copy of the docker argv is a
// deliberate parity decision: the daemon-mediated invocation and a shell
// script inside the agent that types `yq …` execute THE SAME FILE, so their
// stdout, stderr and exit code cannot drift apart by construction.
//
// `exec` replaces the shell, so the script's exit status is the shim's
// (docker's, and therefore the container program's) exit status.
func RemoteExecScript(a Alias, home string, args []string, limits Limits, env []string) (string, error) {
	shim, err := Shim(a, home, limits, env)
	if err != nil {
		return "", err
	}
	binDir := strings.TrimSuffix(home, "/") + "/bin"
	shimPath := binDir + "/" + a.Name
	rev := shimRevOf(shim)

	var b strings.Builder
	b.WriteString("mkdir -p " + shellQuote(binDir) + " || exit 1\n")
	b.WriteString("if ! grep -qF " + shellQuote(shimRevMarker+rev) + " " + shellQuote(shimPath) + " 2>/dev/null; then\n")
	b.WriteString("cat > " + shellQuote(shimPath) + " <<'BUNKER_ALIAS_EOF'\n")
	b.WriteString(shim)
	b.WriteString("BUNKER_ALIAS_EOF\n")
	b.WriteString("chmod 755 " + shellQuote(shimPath) + "\n")
	b.WriteString("fi\n")
	b.WriteString("exec " + shellQuote(shimPath))
	for _, a := range args {
		b.WriteString(" " + shellQuote(a))
	}
	b.WriteString("\n")
	return b.String(), nil
}

// RawExecArgv renders the argv (no intermediate shell) for one alias
// invocation, used by `bunker exec --raw`.  Raw mode keeps its no-shell
// contract: the docker binary and every element after it is an argv element,
// never a string a shell parses.
//
// A raw invocation deliberately does NOT install the shim — raw mode exists
// precisely to avoid a shell layer — so it is the daemon-mediated path that
// resolves the alias, and the program runs out of the same image.
func RawExecArgv(a Alias, home string, args []string, limits Limits, env []string) ([]string, error) {
	return BuildDockerArgv(a, home, args, limits, env)
}

// ImageCache remembers images this daemon process has already confirmed (or
// pulled).  A cached image skips the `docker image inspect` round trip
// entirely, which is what makes the second run of an alias cheaper than the
// first — the observable half of "auto-pull on first run, cached after".
//
// The zero value is usable.
type ImageCache struct {
	mu   sync.RWMutex
	seen map[string]struct{}
}

// Has reports whether image is known present.
func (c *ImageCache) Has(image string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.seen[image]
	return ok
}

// Add marks image as present.
func (c *ImageCache) Add(image string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = make(map[string]struct{})
	}
	c.seen[image] = struct{}{}
}

// Forget drops image, so the next use re-inspects it.  Called when a run
// fails in a way that suggests the image is no longer present.
func (c *ImageCache) Forget(image string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.seen, image)
}
