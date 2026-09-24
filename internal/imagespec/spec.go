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

import (
	"fmt"
	"strings"
	"time"
)

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

// ManagerDef defines one constrained package manager. Everything the package
// needs to know about a manager lives in its row — validity, token policy,
// and Dockerfile rendering all derive from the registry — so adding a manager
// is one managerDefs row plus its PackageManager constant (previously four
// scattered edits: the const + Valid switch, validateToken, Dockerfile's
// render switch, and the tests).
type ManagerDef struct {
	// Name is the wire/JSON spelling; it must equal the PackageManager
	// constant the row is registered under.
	Name PackageManager
	// TokenExtra lists characters allowed in a package token BEYOND the
	// shared grammar (letters, digits, and . + - _ : / @ =). Usually empty.
	TokenExtra string
	// TokenDeny lists substrings no token may contain, applied after the
	// grammar. apt denies "/" so absolute paths like /var/run/docker.sock or
	// --mount=type=bind,source=/etc,target=/etc cannot ride through as
	// "packages" (a mounted path is not a package name).
	TokenDeny []string
	// Render appends the manager's install lines for pkgs to b. It only ever
	// receives tokens that passed the row's token policy, so the rendered
	// text cannot contain metacharacters by construction.
	Render func(b *strings.Builder, pkgs []string)
	// Probe applies the row's full token policy (shared grammar +
	// TokenExtra, minus TokenDeny) to one candidate token: nil means the
	// token may ride the wire. It is derived by register — never set in
	// table rows — so each row's policy has exactly one source of truth.
	// validateToken delegates to it; tests call it directly.
	Probe func(tok string) error
}

// managerDefs is the manager registry: one row per supported manager, in
// wire-documentation order. This table is the single place a new manager is
// added; nothing else in the package is per-manager.
var managerDefs = []ManagerDef{
	{
		Name:      ManagerAPT,
		TokenDeny: []string{"/"},
		Render:    renderAPT,
	},
	{
		Name:   ManagerGo,
		Render: renderGo,
	},
	{
		Name:   ManagerNPM,
		Render: renderNPM,
	},
}

// managerByName indexes managerDefs for O(1) lookup by wire name.
var managerByName = map[PackageManager]*ManagerDef{}

func init() {
	for i := range managerDefs {
		register(&managerDefs[i])
	}
}

// register indexes one registry row: it derives the row's Probe from its
// token policy fields and inserts it into the lookup map. It refuses a def
// without a render func, an empty name, or a duplicate name, so a manager
// cannot exist half-registered: every registered manager has both a render
// func and a token policy (the derived Probe).
func register(d *ManagerDef) {
	if d.Name == "" {
		panic("imagespec: manager registered with an empty name")
	}
	if d.Render == nil {
		panic(fmt.Sprintf("imagespec: manager %q registered without a render func", d.Name))
	}
	if _, dup := managerByName[d.Name]; dup {
		panic(fmt.Sprintf("imagespec: manager %q registered twice", d.Name))
	}
	d.Probe = tokenPolicy(d.Name, d.TokenExtra, d.TokenDeny...)
	managerByName[d.Name] = d
}

// tokenPolicy builds a Probe from the shared token grammar (the hand-rolled
// equivalent of tokenRe) plus a row's extra allowed characters and denied
// substrings. Errors name the manager so a rejection is attributable to its
// directive.
func tokenPolicy(m PackageManager, extra string, deny ...string) func(string) error {
	return func(tok string) error {
		for i := 0; i < len(tok); i++ {
			c := tok[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			case strings.IndexByte(extra, c) >= 0:
			case c == '.' || c == '+' || c == '-' || c == '_' || c == ':' || c == '/' || c == '@' || c == '=':
			default:
				return fmt.Errorf("illegal character %q at position %d: package tokens may contain only letters, digits, and . + - _ : / @ =", string(c), i+1)
			}
		}
		for _, bad := range deny {
			if strings.Contains(tok, bad) {
				return fmt.Errorf("%s package names may not contain %q", string(m), bad)
			}
		}
		return nil
	}
}

// Renderers: one per registry row. Each receives policy-validated tokens
// only; the bodies are byte-identical to the pre-registry switch (pinned by
// TestDockerfile_Golden).

func renderAPT(b *strings.Builder, pkgs []string) {
	b.WriteString("RUN apt-get update && apt-get install -y --no-install-recommends")
	for _, p := range pkgs {
		b.WriteString(" ")
		b.WriteString(p)
	}
	b.WriteString(" && rm -rf /var/lib/apt/lists/*\n")
}

func renderGo(b *strings.Builder, pkgs []string) {
	for _, p := range pkgs {
		b.WriteString("RUN go install ")
		b.WriteString(p)
		b.WriteString("\n")
	}
}

func renderNPM(b *strings.Builder, pkgs []string) {
	b.WriteString("RUN npm install -g")
	for _, p := range pkgs {
		b.WriteString(" ")
		b.WriteString(p)
	}
	b.WriteString("\n")
}

// Valid reports whether m is a registered manager.
func (m PackageManager) Valid() bool {
	return managerByName[m] != nil
}

// Def returns the registry row for m, or nil if m is not registered.
func (m PackageManager) Def() *ManagerDef {
	return managerByName[m]
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
