package imagespec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DefaultBaseImage is the base image every agent uses today; a spec may only
// re-declare one of the allowed bases, never introduce a new source.
const DefaultBaseImage = "docker.io/library/ubuntu:24.04"

// StockToolchainPackages is the apt package set every image-spec image
// provisions BEFORE the spec's own package directives (DF-BUNKER-80). It is
// the stock agent userland — what a vanilla (non-image-spec) agent has from
// the host on this product's Ubuntu 24.04 hosts — re-installed inside the
// customized image, because an image spec ADDS packages to the agent; it must
// never replace the userland. The membership is the row's named list plus the
// GAP-092-measured stock extras:
//
//	git             — agent-tools REQUIRED ("session, lease"; internal/cli
//	                  agenttools.go); a stock agent has git 2.43.0
//	docker.io       — the docker CLIENT (/usr/bin/docker); exec wires
//	                  DOCKER_HOST at the agent's own rootless socket, so a
//	                  client-less image makes rootless docker unreachable
//	python3, make   — the stock build/scripting baseline
//	jq              — present on a measured fresh stock spawn (GAP-092)
//	ca-certificates — TLS trust for anything the toolchain fetches
//
// All allowed bases are apt-family (ubuntu:24.04/22.04, debian:12/11), and
// every name here exists in each of those archives, so the rendered layer is
// valid for any base a spec may declare. The tokens are builder-chosen
// constants, not spec input: they never pass through the token grammar, and
// they render through the same single-quoting renderer as spec packages.
var StockToolchainPackages = []string{
	"ca-certificates",
	"git",
	"python3",
	"make",
	"jq",
	"docker.io",
}

// GoToolchainPackages is the apt package set the `go` directive's TOOLCHAIN
// BOOTSTRAP installs before its first `go install` line (DF-BUNKER-79).
//
// It exists because the default base (DefaultBaseImage, ubuntu:24.04) ships no
// Go toolchain: the pre-fix render went straight to `RUN go install <pkg>`,
// which failed EVERY go-manager image build with `/bin/sh: 1: go: not found`
// (exit 127) — including the spec `bunker agent-tools --install` itself prints
// as the remediation for tools it cannot deliver (DF-BUNKER-57). The advertised
// path could not build.
//
// golang-go is the distribution metapackage: it exists in the archive of every
// ALLOWED base (ubuntu:24.04/22.04, debian:12/11), and on the default base it
// provides Go 1.22, which understands module-aware `go install pkg@version`
// (Go >= 1.16). It is deliberately ONE unversioned name — a versioned name such
// as golang-1.22-go exists in a single release's archive only. Known limit: the
// apt route bootstraps whatever Go the base's archive carries, so a base whose
// golang-go predates module-aware install (debian:11's is 1.15) would still
// refuse `pkg@version`; the default base and the other allowed bases are fine.
//
// The tokens are builder-chosen constants, not spec input: like
// StockToolchainPackages they never pass through the token grammar, and they
// render through the same single-quoting apt renderer as every other apt step.
var GoToolchainPackages = []string{"golang-go"}

// AgentToolPackages is the DELIVERED SET (TOOLS-B2): the verb-dependency tools
// a fresh agent is measured to LACK (GAP-092 findings F-2/F-3: rg absent, a
// language server absent), recorded here as the image-spec package-add
// directives that deliver them — the remediation the repo canonically prints,
// tests against, and documents.
//
//   - rg → apt `ripgrep`: distribution-owned, version-pinned and
//     signature-verified by apt on every allowed base. A binary copy would
//     silently discard both, which is why agent-tools refuses to ship it.
//   - gopls → go `golang.org/x/tools/gopls@latest`: the go manager already
//     bootstraps the toolchain (GoToolchainPackages) and pins GOBIN to
//     GoBinDir, so the installed server is on every agent exec PATH.
//
// This is the OPT-IN spec surfaced by tooling, not an automatic spawn step: a
// spec change here re-keys every spec-derived image build (Spec.CacheKey), so
// making it default-at-spawn is a live-E2E change filed separately (the
// delivery spec's "Remaining work" #2). Until that lands, `bunker agent-tools`
// prints these exact directives and the JSON form below parses through Parse —
// a test in internal/cli pins both, so the printed remediation can never drift
// from what the builder accepts.
var AgentToolPackages = []PackageAdd{
	{Manager: ManagerAPT, Packages: []string{"ripgrep"}},
	{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@latest"}},
}

// AgentToolSpec is the canonical wire form of AgentToolPackages: the single-line
// JSON `bunker agent-tools` prints and the operator pastes into a spec file.
// It must stay byte-equal to a Parse of the struct's canonical JSON (pinned by
// TestAgentToolSpecRoundTrip), so the printed string and the struct form can
// never disagree.
const AgentToolSpec = `{"packages":[{"manager":"apt","packages":["ripgrep"]},` +
	`{"manager":"go","packages":["golang.org/x/tools/gopls@latest"]}]}`

// AgentToolSpecSpec returns the delivered set as a validated Spec (base +
// directives, declaration order). It fails the build loudly if the constant
// above ever stops parsing — the same guarantee the CLI's pin asserts at test
// time, enforced here at construction time for every caller.
func AgentToolSpecSpec() *Spec {
	spec, err := Parse([]byte(AgentToolSpec))
	if err != nil {
		// Unreachable while the pinned constant stays valid; a panic here is
		// the loudest possible statement that the printed remediation broke.
		panic(fmt.Sprintf("imagespec: AgentToolSpec does not parse: %v", err))
	}
	return spec
}

// AgentToolPackageNames returns every package name the delivered set installs,
// in directive order. Test-facing (membership assertions) and doc-facing
// (the delivered-set table is generated from this list, not hand-copied).
func AgentToolPackageNames() []string {
	names := make([]string, 0, 2)
	for _, d := range AgentToolPackages {
		names = append(names, d.Packages...)
	}
	return names
}

// GoBinDir is the GOBIN the go renderer installs into (DF-BUNKER-79). It is the
// first component of internal/server's agentExecBasePath (and $HOME/bin is
// prepended to that), so a tool `go install`ed here is on the PATH of every
// subsequent agent exec — `bunker agent-tools`' probe included. Without it,
// module-aware `go install` writes to $GOPATH/bin (/root/go/bin for the image's
// root user), which no exec PATH contains: the tool would build and install
// correctly and still read as ABSENT to every consumer.
const GoBinDir = "/usr/local/bin"

// allowedBases is the closed set of base images a spec may name. Everything
// else — private registries, localhost pulls, scratch — is rejected so a spec
// can never change where the agent image comes from beyond this list.
var allowedBases = map[string]bool{
	DefaultBaseImage:                 true,
	"docker.io/library/ubuntu:22.04": true,
	"docker.io/library/debian:12":    true,
	"docker.io/library/debian:11":    true,
}

// tokenAllowed matches a constrained package name/version token. The class
// ranges deliberately exclude EVERY shell metacharacter: whitespace, | & ; < >
// ( ) $ ` \ " ' ! # ? * ~ [ ] { } , and control characters — so chaining,
// substitution, redirection, and globbing are all structurally impossible.
// Allowed: letters, digits, and . + - _ / : @ =
const tokenRe = `^[A-Za-z0-9.+\-_:/@=]+$`

// Parse validates raw JSON bytes into a Spec. It is total: any error means no
// part of the input was accepted.
func Parse(data []byte) (*Spec, error) {
	if len(data) > MaxSpecBytes {
		return nil, fmt.Errorf("spec is %d bytes, exceeds %d byte limit", len(data), MaxSpecBytes)
	}

	// Reject unknown fields so Dockerfile-instruction-shaped inputs ("from",
	// "user", "env", "run", ...) fail loudly instead of being ignored.
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var raw wireImageSpec
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid image spec: %w", err)
	}
	// Only one JSON object per spec — no trailing garbage.
	if dec.More() {
		return nil, fmt.Errorf("invalid image spec: unexpected trailing data")
	}
	return fromWire(&raw)
}

// Default returns the no-op spec: the standard base image with no package
// additions. Agents spawned with it are indistinguishable from today's
// bootstrap image.
func Default() *Spec {
	return &Spec{Base: DefaultBaseImage}
}

// fromWire validates the shared wire shape (JSON and proto both map onto it).
func fromWire(raw *wireImageSpec) (*Spec, error) {
	spec := &Spec{Base: DefaultBaseImage}
	if raw.Base != "" {
		if !allowedBases[raw.Base] {
			return nil, fmt.Errorf("base image %q is not allowed (allowed: %v)", raw.Base, AllowedBases())
		}
		spec.Base = raw.Base
	}

	seen := make(map[PackageManager]bool, len(raw.Packages))
	if len(raw.Packages) > MaxDirectives {
		return nil, fmt.Errorf("too many directives: %d exceeds limit %d", len(raw.Packages), MaxDirectives)
	}
	for _, p := range raw.Packages {
		if !p.Manager.Valid() {
			names := make([]string, 0, len(managerDefs))
			for _, def := range managerDefs {
				names = append(names, string(def.Name))
			}
			return nil, fmt.Errorf("unsupported package manager %q (allowed: %s)", string(p.Manager), strings.Join(names, ", "))
		}
		if seen[p.Manager] {
			return nil, fmt.Errorf("duplicate directive for package manager %q", string(p.Manager))
		}
		seen[p.Manager] = true
		if len(p.Packages) > MaxPackagesPerDirective {
			return nil, fmt.Errorf("too many packages for %q: %d exceeds limit %d", string(p.Manager), len(p.Packages), MaxPackagesPerDirective)
		}
		d := PackageAdd{Manager: p.Manager, Packages: make([]string, 0, len(p.Packages))}
		for _, name := range p.Packages {
			if err := validateToken(p.Manager, name); err != nil {
				return nil, fmt.Errorf("%s package %q: %w", string(p.Manager), name, err)
			}
			d.Packages = append(d.Packages, name)
		}
		if len(d.Packages) > 0 {
			spec.Packages = append(spec.Packages, d)
		}
	}
	return spec, nil
}

// PackageAddSpec is the JSON wire form of a package-add directive.

// AllowedBases returns the sorted list of allowed base images (for error text
// and docs).
func AllowedBases() []string {
	out := make([]string, 0, len(allowedBases))
	for b := range allowedBases {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// validateToken enforces the constrained token grammar on a single package
// name or name@version / name=version token. The per-manager policy (extra
// allowed characters, denied substrings) lives entirely in the manager's
// registry row (see ManagerDef.Probe); this wrapper adds the shared
// size/emptiness bounds. Slashes are denied for apt — never for go module
// paths or npm scoped names — by that row's TokenDeny.
func validateToken(m PackageManager, tok string) error {
	if tok == "" {
		return fmt.Errorf("empty package token")
	}
	if len(tok) > MaxTokenBytes {
		return fmt.Errorf("token exceeds %d bytes", MaxTokenBytes)
	}
	if def := m.Def(); def != nil {
		return def.Probe(tok)
	}
	// Unregistered manager: no policy means no acceptance.
	return fmt.Errorf("unsupported package manager %q", string(m))
}

// CacheKey returns the deterministic SHA-256 hex digest of the canonical spec.
// Two specs with identical (normalized) content share one key regardless of
// JSON field or directive order; any content change yields a different key.
func (s *Spec) CacheKey() string {
	var b strings.Builder
	b.WriteString("base=")
	b.WriteString(s.Base)
	b.WriteString("\n")
	norm := make([]PackageAdd, len(s.Packages))
	copy(norm, s.Packages)
	sort.Slice(norm, func(i, j int) bool { return norm[i].Manager < norm[j].Manager })
	for _, d := range norm {
		b.WriteString(string(d.Manager))
		b.WriteString("=")
		pkgs := append([]string(nil), d.Packages...)
		sort.Strings(pkgs)
		for _, p := range pkgs {
			b.WriteString(p)
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// Hash parses and canonicalizes raw spec bytes into the cache key.
func Hash(data []byte) (string, error) {
	spec, err := Parse(data)
	if err != nil {
		return "", err
	}
	return spec.CacheKey(), nil
}

// Dockerfile renders the validated spec into the exact Dockerfile the builder
// writes. The grammar guarantees every line below is builder-generated — the
// caller's bytes never reach this text. Rendering is fully registry-driven:
// each directive's manager row supplies its line renderer.
//
// The FIRST layer after FROM is always the stock-toolchain layer
// (DF-BUNKER-80): a spec is a package-ADD, not an image replacement, so the
// customized image must retain the stock agent userland (git, the docker
// client, python3, make, jq — see StockToolchainPackages) before any of the
// spec's own package lines. Any spec at all flips exec into container mode
// (GAP-069), so the layer is unconditional — a base-only spec needs it too.
func (s *Spec) Dockerfile() string {
	var b strings.Builder
	b.WriteString("FROM ")
	b.WriteString(s.Base)
	b.WriteString("\n")
	renderStockToolchain(&b)
	for _, d := range s.Packages {
		if def := d.Manager.Def(); def != nil {
			def.Render(&b, d.Packages)
		}
	}
	return b.String()
}

// renderStockToolchain emits the DF-BUNKER-80 stock-userland layer through
// the apt renderer: one RUN line installing StockToolchainPackages, emitted
// immediately after FROM and before every spec directive. It reuses renderAPT
// verbatim so the layer is byte-identical in style to a spec's own apt line
// (single-quoted tokens, --no-install-recommends, apt lists cleaned).
func renderStockToolchain(b *strings.Builder) {
	renderAPT(b, StockToolchainPackages)
}
