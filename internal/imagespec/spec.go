// Package imagespec implements the secure, declarative per-agent image
// specification feature (GAP-064).
//
// An image spec is a small JSON document that customizes the per-agent rootless
// image with PACKAGE-ADD DIRECTIVES ONLY:
//
//	{
//	  "base": "docker.io/library/ubuntu:24.04",
//	  "packages": [
//	    {"manager": "apt", "packages": ["jq", "curl"]},
//	    {"manager": "go",  "packages": ["golang.org/x/tools/gopls@v0.17.0"]},
//	    {"manager": "npm", "packages": ["typescript@5.6.3"]}
//	  ]
//	}
//
// These three forms are also the validated LIFECYCLE HOOK data for GAP-064:
// an agent image is customized exactly once at spawn (the install hook) and
// torn down at destroy; there is no arbitrary command, entrypoint, or
// restart-hook surface. Everything else about the lifecycle is the existing
// agent bootstrap (user → rootless dockerd → agent container).
//
// Security model — the parser REJECTS anything that is not a package add:
//
//   - base-image changes beyond naming an allowed base (no FROM rewrites other
//     than the single declared base, no scratch/local images)
//   - USER, EXPOSE, VOLUME, ENV, LABEL, ENTRYPOINT, CMD, WORKDIR, SHELL,
//     ONBUILD, STOPSIGNAL, HEALTHCHECK, MAINTAINER and any other Dockerfile
//     instruction (the input is NOT Dockerfile text at all — arbitrary
//     Dockerfile text is never accepted)
//   - curl|sh / wget|sh and any shell chaining, substitution, or redirection
//     characters in any token
//   - mounts, devices, sockets, volumes, and other runtime escapes (the spec
//     cannot influence docker run flags; the builder never adds any)
//   - privileged / host-namespace flags (not representable in the grammar)
//   - non-package network or exfiltration commands (only the three constrained
//     package-manager invocations are ever rendered)
//
// Hard bounds: spec size, directive count, packages per directive, token
// length, and a build timeout enforced by the builder.
package imagespec

import "time"

// Bounds on untrusted input. They are deliberately small: a package list is a
// convenience, not a build farm.
const (
	// MaxSpecBytes is the maximum serialized spec size (16 KiB).
	MaxSpecBytes = 16 * 1024
	// MaxDirectives is the maximum number of package-add directives per spec.
	MaxDirectives = 16
	// MaxPackagesPerDirective is the maximum packages per directive.
	MaxPackagesPerDirective = 16
	// MaxTokenBytes is the maximum length of a single package name or version.
	MaxTokenBytes = 256
	// DefaultBuildTimeout is the default rootless image build timeout.
	DefaultBuildTimeout = 20 * time.Minute
)

// PackageManager names the constrained package managers an image spec may
// invoke. The string values are the wire/JSON spelling.
type PackageManager string

const (
	ManagerAPT PackageManager = "apt"
	ManagerGo  PackageManager = "go"
	ManagerNPM PackageManager = "npm"
)

// Valid reports whether m is one of the three supported managers.
func (m PackageManager) Valid() bool {
	switch m {
	case ManagerAPT, ManagerGo, ManagerNPM:
		return true
	}
	return false
}

// PackageAdd is one constrained package-install directive.
type PackageAdd struct {
	Manager PackageManager
	// Packages are package names, optionally pinned with a manager-specific
	// version suffix (apt: =version, go: @version, npm: @version).
	Packages []string
}

// PackageAddSpec is the JSON wire form of a package-add directive (also the
// shared validation shape proto directives map onto — see fromWire).
type PackageAddSpec struct {
	Manager  PackageManager `json:"manager"`
	Packages []string       `json:"packages"`
}

// Spec is a validated per-agent image specification.
//
// The zero value is not valid; construct specs via Parse or Validate.
type Spec struct {
	Base string
	// Packages are the package-add directives, in declaration order, with
	// at most one directive per PackageManager (repeats are rejected).
	Packages []PackageAdd
}

// Lifecycle hook placement (documentation contract, GAP-064):
//
//   - spawn install hook: the spec's package adds are rendered into the
//     per-agent image Dockerfile and built once through the agent's rootless
//     socket (see Builder).
//   - destroy teardown hook: implemented in internal/agent
//     (cleanupAgentContainers) — containers built from the customized image
//     are stopped/removed through the SAME per-agent socket before the
//     rootless dockerd is stopped. The spec carries no hook data for this:
//     teardown is structural, so it cannot be customized into an escape
//     hatch.
