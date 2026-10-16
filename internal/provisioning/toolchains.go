// Package provisioning owns the DECLARED build-toolchain set for agents
// (SURF-009): the five tools a fresh agent was measured to lack (go, gcc,
// make, node, cargo — python3 only otherwise), the exact delivery program
// per tool, the version pins that program installs, and the probe that
// reports the set present/absent BY NAME.
//
// Ownership follows the agent-tools division of labour
// (docs/prd/SPEC-agent-tool-delivery.md): a tool WE build is copied, a tool a
// distribution ships is installed FROM its registry. Every toolchain here is
// registry-sourced:
//
//   - go   — the dl.google.com/go release tarball, checksum-pinned (the same
//     pin internal/cli's gopls bootstrap already proved live, cross-checked
//     against the official go.dev download JSON);
//   - gcc  — the apt archive itself: the candidate gcc-<major> family
//     downloaded with `apt-get download` and unpacked with dpkg-deb -x into
//     the agent's HOME (no root), the same mechanism `bunker agent-tools
//     --install` uses for ripgrep — extended to the compiler's package
//     family because a C compiler is not a single binary (cc1, binutils,
//     crt objects and libc headers live in sibling packages). Nothing is
//     trusted beyond apt's own signature-verified archives, and the stage
//     VERDICT is a real compile-and-run proof, not a file-exists check;
//   - make — the apt `make` package, single-binary extract (the ripgrep
//     shape);
//   - node — the nodejs.org release tarball, checksum-pinned (official
//     SHASUMS256.txt values fetched and embedded at authoring time);
//   - cargo — the pinned rustup installer with --profile minimal, from
//     static.rust-lang.org, checksum-pinned per architecture. The symlinks
//     point at the TOOLCHAIN's real cargo/rustc binaries (not the rustup
//     proxies), so they run without RUSTUP_HOME in the environment — an
//     agent exec PATH cannot be taught new env vars.
//
// Like AgentToolPackages, the set is DECLARED and CHECKABLE: `bunker
// toolchain-check AGENT_ID` reports every entry present/absent by name, and
// `--deliver` installs the missing ones in place. The probe and delivery run
// through the same audited exec path as `bunker exec` — no new RPC, no new
// credential.
package provisioning

// ToolchainKind names the delivery mechanism, and the report carries it: an
// operator reading "go present" deserves to know it arrived as a pinned
// tarball, not a guess about the machine.
type ToolchainKind string

const (
	KindDebExtract ToolchainKind = "apt-deb-extract"
	KindTarball    ToolchainKind = "pinned-tarball"
	KindRustup     ToolchainKind = "pinned-rustup"
)

// Toolchain is one entry of the declared set.
type Toolchain struct {
	// Name is the probe token: the executable `command -v` looks for on the
	// agent's PATH (which the server already points at $HOME/bin).
	Name string
	// Kind is the delivery mechanism (report data, see ToolchainKind).
	Kind ToolchainKind
	// Builds is what the tool enables, one line, report-facing.
	Builds string
	// NeededBy names the consumers, one line, report-facing.
	NeededBy string
	// Required marks the tools SURF-009 declares the offload build path
	// needs. All five are required today; the field exists so a future
	// optional entry degrades a capability without failing the verdict.
	Required bool
	// Script builds the delivery program run on the agent. It must print
	// the NAME\tSTATUS\tdetail verdict line (parseStageOutput) and treat
	// $HOME/bin as the PATH slot — the server already puts it first on
	// every exec.
	Script func() string
}

// InstallDir is the PATH slot inside the agent, relative to its home — the
// same $HOME/bin the vendored toolsd and the registry tools use
// (internal/cli agentToolInstallDir; duplicated as a literal so this package
// stays independent of the cli package).
const InstallDir = "bin"

// CacheRoot is where the stages keep their extracted toolchains, relative to
// the agent's home. Inside HOME (agent-writable, per-agent lifetime, no root)
// and NOT on PATH — only the entry symlinks/wrappers in $HOME/bin are.
const CacheRoot = ".bunker/toolchains"

// catalog is the declared set, in SURF-009 row order (go, gcc, make, node,
// cargo). It is a function, not a var, so no caller can mutate the declared
// set at runtime.
func catalog() []Toolchain {
	return []Toolchain{
		{
			Name: "go", Kind: KindTarball,
			Builds:   "go build / go test — the offload build path itself",
			NeededBy: "Go projects; `go install` (gopls and other Go tools)",
			Required: true,
			Script:   goToolchainStage,
		},
		{
			Name: "gcc", Kind: KindDebExtract,
			Builds:   "native C (and C++ via g++) — cgo and every compiled dependency",
			NeededBy: "cgo builds; C/C++ sources; python wheels with native code",
			Required: true,
			Script:   gccToolchainStage,
		},
		{
			Name: "make", Kind: KindDebExtract,
			Builds:   "Makefile-driven builds (the C ecosystem's default driver)",
			NeededBy: "make / cmake-less builds; ./configure && make trees",
			Required: true,
			Script:   makeToolchainStage,
		},
		{
			Name: "node", Kind: KindTarball,
			Builds:   "JS/TS builds (npm run build) and npm-installed toolchains",
			NeededBy: "TypeScript projects; vite/tsc/eslint pipelines",
			Required: true,
			Script:   nodeToolchainStage,
		},
		{
			Name: "cargo", Kind: KindRustup,
			Builds:   "Rust binaries (cargo build / cargo test)",
			NeededBy: "Rust projects; rustc via the pinned toolchain",
			Required: true,
			Script:   cargoToolchainStage,
		},
	}
}

// Catalog returns the declared set. Callers must treat the result as
// read-only; mutating it would silently redefine what every probe and
// delivery means.
func Catalog() []Toolchain {
	out := make([]Toolchain, len(catalog()))
	copy(out, catalog())
	return out
}

// CatalogNames returns the probe tokens in declared order.
func CatalogNames() []string {
	names := make([]string, 0, 5)
	for _, t := range catalog() {
		names = append(names, t.Name)
	}
	return names
}

// ImageSpecDirective is the canonical wire form of the toolchain set as an
// image-spec package-add document — the path agents WITH the image-spec
// surface use to bake the same set into the image at spawn (StockToolchain
// images already carry golang-go; this adds the rest). It must parse; the
// lazy validation in ToolchainImageSpec fails the process loudly if the
// constant ever stops being a valid spec.
const ImageSpecDirective = `{"packages":[{"manager":"apt","packages":["gcc","g++","make"]}]}`

// ToolchainImageSpec returns the directive as a validated imagespec.Spec.
//
// NOTE: the constant above is a JSON *string* here rather than a constructed
// imagespec.Spec so this package does not import internal/imagespec (which
// would drag the builder and its Docker render into every consumer of the
// catalog). The string is the same wire form `bunker spawn --image-spec`
// accepts.
const _ = ImageSpecDirective // documented constant; see comment above
