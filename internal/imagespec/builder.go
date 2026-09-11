package imagespec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Runner executes the constrained docker invocations the builder needs.
// Tests inject a recording fake; production injects the exec-based runner.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execRunner shells out to a binary. The builder only ever passes "docker"
// with the per-agent --host flag, and argv is fully builder-generated.
type execRunner struct{}

// Run executes name with args via exec.CommandContext and returns combined
// output. If the context deadline fired, the context error wins so callers
// can errors.As a timeout.
func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil && ctx.Err() != nil {
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), ctx.Err())
	}
	return out, err
}

// CacheOptions configures the rootless image builder and its cache.
type CacheOptions struct {
	// Dir is the server-local cache root (one subdir per agent, one per spec
	// cache key within an agent).
	Dir string
	// BuildTimeout bounds a single build; derived from server config.
	BuildTimeout time.Duration
	// Disabled mirrors agent.image_spec.enabled=false server policy.
	Disabled bool
}

// Builder builds and caches customized per-agent images through an agent's
// own rootless docker socket. Image refs exist only inside the target agent's
// rootless daemon, so the cache is tracked per (agent, spec key); every cache
// hit is verified against the agent's daemon with `docker image inspect`
// before reuse, and a missing image falls through to a rebuild.
type Builder struct {
	runner Runner
	opts   CacheOptions
}

// NewBuilder returns a Builder backed by the given runner.
func NewBuilder(r Runner, opts *CacheOptions) *Builder {
	if r == nil {
		r = execRunner{}
	}
	if opts == nil {
		opts = &CacheOptions{}
	}
	if opts.BuildTimeout <= 0 {
		opts.BuildTimeout = DefaultBuildTimeout
	}
	return &Builder{runner: r, opts: *opts}
}

// ErrDisabled is returned when the feature is off and a spec was supplied.
var ErrDisabled = errors.New("image spec support is disabled on this server")

// validAgentID enforces the same agent-id shape as the resource tracker:
// lowercase letters, digits, and hyphens, 1-64 characters.
func validAgentID(agentID string) bool {
	if agentID == "" || len(agentID) > 64 {
		return false
	}
	for i := 0; i < len(agentID); i++ {
		c := agentID[i]
		if c != '-' && !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// agentSockPath returns the ONLY socket the builder may ever talk to for the
// given agent: /run/bunker/<agent>/docker.sock. The location is structural —
// not configurable — so a spec can never redirect docker at the host daemon
// or another agent's socket.
func agentSockPath(agentID string) (string, error) {
	if !validAgentID(agentID) {
		return "", fmt.Errorf("invalid agent id %q", agentID)
	}
	return filepath.Join("/run/bunker", agentID, "docker.sock"), nil
}

// cacheDirFor maps (agent, spec cache key) to its cache directory, refusing
// path traversal, bad keys, or bad agent ids. Layout: <Dir>/<agentID>/<key>.
func cacheDirFor(dir, agentID, key string) (string, error) {
	if !validAgentID(agentID) {
		return "", fmt.Errorf("invalid agent id %q", agentID)
	}
	if key == "" || len(key) != 64 {
		return "", fmt.Errorf("invalid cache key length %d", len(key))
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		ok := (c >= 'a' && c <= 'f') || (c >= '0' && c <= '9')
		if !ok {
			return "", fmt.Errorf("invalid cache key %q", key)
		}
	}
	return filepath.Join(dir, agentID, key), nil
}

// BuildImage validates raw spec bytes and returns the image reference for the
// agent's customized image, building it through the agent's rootless socket
// if needed. Validation ALWAYS happens before any side effect.
func (b *Builder) BuildImage(ctx context.Context, agentID string, raw []byte) (string, error) {
	if b.opts.Disabled {
		return "", ErrDisabled
	}
	spec, err := Parse(raw)
	if err != nil {
		// Zero side effects: validation failed, so no docker call of any kind.
		return "", err
	}
	return b.BuildValidated(ctx, agentID, spec)
}

// BuildValidated is BuildImage for an already-validated spec (the server
// validates the proto form before spawning; this avoids a second parse).
func (b *Builder) BuildValidated(ctx context.Context, agentID string, spec *Spec) (string, error) {
	if b.opts.Disabled {
		return "", ErrDisabled
	}
	key := spec.CacheKey()
	ref := ImageRef(key)

	cacheDir, err := cacheDirFor(b.opts.Dir, agentID, key)
	if err != nil {
		return "", err
	}
	marker := filepath.Join(cacheDir, "built")

	if _, err := os.Stat(marker); err == nil {
		// Cache hit — but image refs live only inside the target agent's
		// rootless daemon, so verify the image still exists there before
		// reusing it. A missing image (fresh daemon after re-spawn, pruned
		// data) falls through to a rebuild.
		if b.imageExists(ctx, agentID, ref) {
			return ref, nil
		}
	}

	sock, err := agentSockPath(agentID)
	if err != nil {
		return "", err
	}

	// Build under a bounded deadline derived from server config.
	buildCtx, cancel := context.WithTimeout(ctx, b.opts.BuildTimeout)
	defer cancel()

	if err := os.MkdirAll(b.opts.Dir, 0o755); err != nil {
		return "", fmt.Errorf("create cache root: %w", err)
	}
	tmp, err := os.MkdirTemp(b.opts.Dir, "build-*")
	if err != nil {
		return "", fmt.Errorf("create build dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	dfPath := filepath.Join(tmp, "Dockerfile")
	if err := os.WriteFile(dfPath, []byte(spec.Dockerfile()), 0o644); err != nil {
		return "", fmt.Errorf("write Dockerfile: %w", err)
	}

	// Constrained build argv: builder-generated only, per-agent socket only.
	buildArgs := []string{
		"--host", "unix://" + sock,
		"build",
		"-q",
		"-f", dfPath,
		"-t", ref,
		tmp,
	}
	buildOut, err := b.runner.Run(buildCtx, "docker", buildArgs...)
	if err != nil {
		return "", fmt.Errorf("rootless build for %s failed: %w (output: %s)", agentID, err, strings.TrimSpace(string(buildOut)))
	}

	// Only a successful build creates the cache marker — failed builds are
	// retried on the next request.
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	if err := os.WriteFile(marker, []byte(ref+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write cache marker: %w", err)
	}
	return ref, nil
}

// imageExists runs `docker image inspect -q <ref>` through the agent's
// rootless socket. Any error means "not sure" → callers rebuild.
func (b *Builder) imageExists(ctx context.Context, agentID, ref string) bool {
	sock, err := agentSockPath(agentID)
	if err != nil {
		return false
	}
	_, err = b.runner.Run(ctx, "docker",
		"--host", "unix://"+sock,
		"image", "inspect", "-q", ref,
	)
	return err == nil
}

// ForgetAgent removes the agent's cache entries (destroy-time bookkeeping —
// the images themselves die with the agent's daemon).
func (b *Builder) ForgetAgent(agentID string) {
	if !validAgentID(agentID) || b.opts.Dir == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(b.opts.Dir, agentID))
}

// ImageRef maps a cache key to the bunkerd-internal image reference. The ref
// never leaves the server: it exists only in the agent's rootless daemon.
func ImageRef(key string) string {
	return "bunkerd-imagespec-" + key[:12] + ":latest"
}
