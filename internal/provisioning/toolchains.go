// Package provisioning owns the DECLARED build-toolchain sets for agents
// (SURF-009): the tools a fresh agent was measured to lack (go, gcc, make,
// node, cargo — python3 only otherwise), the CLASSES that name which of them
// an agent is supposed to have, the exact delivery program per tool, the
// version pins that program installs, and the probe that reports the set
// present/absent BY NAME.
//
// Ownership follows the agent-tools division of labour
// (docs/prd/SPEC-agent-tool-delivery.md): a tool WE build is copied, a tool a
// distribution ships is installed FROM its registry. Every deliverable
// toolchain here is registry-sourced:
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
//   - python3 — NOT deliverable: it is the stock image's own interpreter and
//     the baseline the SURF-009 measurement started from. It is in the
//     catalog so a class can promise it (minimal does) and the report can
//     show it, and it has no stage by construction.
//
// CLASSES are the "can this agent build me?" noun. A class declares which
// catalog entries it requires: minimal (the base image: python3 only), go
// (go + make), full (the whole measured-absent set). `bunker toolchain-check
// --class` reports against one class; `bunker spawn --toolchains=CLASS`
// provisions one.
//
// Like AgentToolPackages, the set is DECLARED and CHECKABLE: the probe
// reports every entry present/absent by name, and `--deliver` installs the
// missing ones in place. Probe and delivery run through the same audited exec
// path as `bunker exec` — no new RPC, no new credential.
package provisioning

import "fmt"

// ToolchainKind names the delivery mechanism, and the report carries it: an
// operator reading "go present" deserves to know it arrived as a pinned
// tarball, not a guess about the machine.
type ToolchainKind string

const (
	// KindBaseImage marks a tool the stock image already ships. It is
	// reported like every other entry and can be REQUIRED by a class, but it
	// has no delivery stage: the agent cannot lack something the image
	// provides, and if it somehow does, the honest answer is to say so
	// rather than to invent a copy path.
	KindBaseImage ToolchainKind = "base-image"

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
	// Script builds the delivery program run on the agent; nil for a
	// KindBaseImage entry. A stage must print the NAME\tSTATUS\tdetail
	// verdict line (parseStageOutput) and treat $HOME/bin as the PATH slot —
	// the server already puts it first on every exec.
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

// catalog is the declared set, in report order: the baseline the SURF-009
// measurement started from (python3) first, then the five tools it measured
// ABSENT (go, gcc, make, node, cargo), in the row's order. It is a function,
// not a var, so no caller can mutate the declared set at runtime.
func catalog() []Toolchain {
	return []Toolchain{
		{
			Name: "python3", Kind: KindBaseImage,
			Builds:   "Python (interpret-only work, and python projects)",
			NeededBy: "the stock image itself; the only toolchain a fresh agent was measured to have",
		},
		{
			Name: "go", Kind: KindTarball,
			Builds:   "go build / go test — the offload build path itself",
			NeededBy: "Go projects; `go install` (gopls and other Go tools)",
			Script:   goToolchainStage,
		},
		{
			Name: "gcc", Kind: KindDebExtract,
			Builds:   "native C (and C++ via g++) — cgo and every compiled dependency",
			NeededBy: "cgo builds; C/C++ sources; python wheels with native code",
			Script:   gccToolchainStage,
		},
		{
			Name: "make", Kind: KindDebExtract,
			Builds:   "Makefile-driven builds (the C ecosystem's default driver)",
			NeededBy: "make / cmake-less builds; ./configure && make trees",
			Script:   makeToolchainStage,
		},
		{
			Name: "node", Kind: KindTarball,
			Builds:   "JS/TS builds (npm run build) and npm-installed toolchains",
			NeededBy: "TypeScript projects; vite/tsc/eslint pipelines",
			Script:   nodeToolchainStage,
		},
		{
			Name: "cargo", Kind: KindRustup,
			Builds:   "Rust binaries (cargo build / cargo test)",
			NeededBy: "Rust projects; rustc via the pinned toolchain",
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
	return namesOf(catalog())
}

// namesOf projects a []Toolchain onto its probe tokens.
func namesOf(tools []Toolchain) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

// Lookup returns one catalog entry by probe token.
func Lookup(name string) (Toolchain, bool) {
	for _, t := range catalog() {
		if t.Name == name {
			return t, true
		}
	}
	return Toolchain{}, false
}

// Class is a named, DECLARED toolchain set: the answer to "can this agent
// build me?" as a noun a spawn can request and a check can verify.
type Class struct {
	// Name is the wire/flag spelling: minimal, go, full.
	Name string
	// Required names the catalog entries this class declares it must have.
	// Every name is a catalog token — a test pins that, so a typo here cannot
	// silently make a class vacuous.
	Required []string
	// Note is report-facing: what the class promises, one line.
	Note string
}

const (
	// ClassMinimal is the stock image: python3 and nothing else. It is the
	// state every fresh agent was measured in, and the class of a spawn that
	// did not ask for toolchains.
	ClassMinimal = "minimal"
	// ClassGo is a Go build box: go + make + the base interpreter.
	ClassGo = "go"
	// ClassFull is the whole measured-absent set: Go, C/C++, Make, JS/TS, Rust.
	ClassFull = "full"

	// DefaultClass is the class `bunker toolchain-check` checks when --class
	// is not given. The question that command answers is "can this agent
	// build me?", and the full set is SURF-009's declared answer — a check
	// that defaulted to minimal would be a check that can never fail.
	DefaultClass = ClassFull
	// SpawnClass is what a spawn without --toolchains provisions: nothing.
	// Provisioning is opt-in (the row's blast-radius note), so an agent that
	// did not ask for a class is the minimal one the base image already is.
	SpawnClass = ClassMinimal
)

// classes is the declared class table.
func classes() []Class {
	return []Class{
		{
			Name:     ClassMinimal,
			Required: []string{"python3"},
			Note:     "python3 only: interpret-only work (the measured state of a fresh agent)",
		},
		{
			Name:     ClassGo,
			Required: []string{"python3", "go", "make"},
			Note:     "Go projects: go build / go test plus the make driver",
		},
		{
			Name:     ClassFull,
			Required: []string{"python3", "go", "gcc", "make", "node", "cargo"},
			Note:     "every build path SURF-009 measured absent: Go, C/C++, Make, JS/TS, Rust",
		},
	}
}

// Classes returns the declared classes with their Required slices copied, so
// a caller cannot mutate the declaration.
func Classes() []Class {
	src := classes()
	out := make([]Class, 0, len(src))
	for _, c := range src {
		cp := c
		cp.Required = append([]string(nil), c.Required...)
		out = append(out, cp)
	}
	return out
}

// ClassNames returns the class spellings in declared order.
func ClassNames() []string {
	out := make([]string, 0, len(classes()))
	for _, c := range classes() {
		out = append(out, c.Name)
	}
	return out
}

// LookupClass resolves a class spelling. An unknown or empty name is an
// error NAMING the declared classes — never a silent fallback to a default,
// which would make "--class g" an accidental full-set check.
func LookupClass(name string) (Class, error) {
	for _, c := range classes() {
		if c.Name == name {
			return c, nil
		}
	}
	return Class{}, fmt.Errorf("unknown toolchain class %q (declared: %s)", name, joinComma(ClassNames()))
}

// Requires reports whether the class declares this catalog entry.
func (c Class) Requires(tool string) bool {
	for _, n := range c.Required {
		if n == tool {
			return true
		}
	}
	return false
}

// Tools returns the class's required catalog entries, in catalog order — the
// list of what a check of this class actually verifies.
func (c Class) Tools() []Toolchain {
	out := make([]Toolchain, 0, len(c.Required))
	for _, t := range catalog() {
		if c.Requires(t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// joinComma is a tiny local joiner (this package stays dependency-free).
func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// ImageSpecDirective is the canonical wire form of the FULL class's
// apt-served half as an image-spec package-add document — the path agents
// WITH the image-spec surface use to bake the same set into the image at
// spawn (image-spec images already carry the stock userland layer, which
// includes golang-go, python3 and make; this adds the C family). It must
// parse; the lazy validation in ToolchainImageSpec fails the process loudly
// if the constant ever stops being a valid spec.
const ImageSpecDirective = `{"packages":[{"manager":"apt","packages":["gcc","g++","make"]}]}`

// ImageSpecDirectiveFor returns the image-spec package-add document for a
// class — the BAKE-AT-SPAWN half of provisioning, for agents that have the
// image-spec surface.
//
// Boundary, stated plainly because it is a real gap: the image-spec managers
// today are package managers (apt, go install, npm, pip, pipx, cargo install,
// gem, composer). apt can express gcc/g++/make; nothing in that list installs
// a NODE or RUSTUP RUNTIME, so the node and cargo halves of the full class
// cannot be baked through a directive — they are delivered in place by the
// pinned tarball/rustup stages, which is what `--deliver` runs. The stock
// userland layer (imagespec.StockToolchainPackages: ca-certificates,
// golang-go, git, python3, make, jq, docker.io) already covers the minimal
// and go classes' non-apt entries.
func ImageSpecDirectiveFor(class string) (string, error) {
	c, err := LookupClass(class)
	if err != nil {
		return "", err
	}
	switch c.Name {
	case ClassFull:
		return ImageSpecDirective, nil
	default:
		// minimal and go: the stock userland layer already carries python3,
		// golang-go and make, so the bake adds nothing. An empty packages
		// list is a valid spec meaning exactly that.
		return `{"packages":[]}`, nil
	}
}
