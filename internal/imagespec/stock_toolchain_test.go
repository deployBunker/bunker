package imagespec

import (
	"strings"
	"testing"
)

// This file pins DF-BUNKER-80: an image spec is a package-ADD surface, so the
// rendered image must retain the STOCK agent userland. Before the fix,
// Dockerfile() emitted FROM <bare base> + only the spec's package lines — the
// customized image REPLACED the userland, so an apt-only spec (ripgrep + jq)
// produced an agent with rg present but git ABSENT (agent-tools REQUIRED),
// the docker client gone from PATH, and python3/make gone (dogfood run 22,
// docs/dogfood/2026-09-26-image-spec-surface.md §2).

// TestDockerfile_PreservesStockToolchain asserts a package-add spec's render
// provisions every stock-toolchain item IN ADDITION TO the spec's own
// packages: the toolchain layer is present, it names each stock package, and
// it is emitted BEFORE the spec's own package lines (a spec package may build
// on the toolchain, never the reverse).
func TestDockerfile_PreservesStockToolchain(t *testing.T) {
	spec := &Spec{
		Base:     DefaultBaseImage,
		Packages: []PackageAdd{{Manager: ManagerAPT, Packages: []string{"ripgrep", "jq"}}},
	}
	got := spec.Dockerfile()

	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("render has %d lines, want FROM + toolchain layer + spec layer:\n%s", len(lines), got)
	}
	if lines[0] != "FROM "+DefaultBaseImage {
		t.Errorf("first line = %q, want the FROM line", lines[0])
	}
	toolchainLine := lines[1]
	if !strings.HasPrefix(toolchainLine, "RUN ") {
		t.Fatalf("second line = %q, want the stock-toolchain RUN layer:\n%s", toolchainLine, got)
	}
	for _, pkg := range StockToolchainPackages {
		if !strings.Contains(toolchainLine, "'"+pkg+"'") {
			t.Errorf("toolchain layer does not install %q: %q", pkg, toolchainLine)
		}
	}
	// The spec's own packages survive: rg (the dfspec-e deliverable) is still
	// rendered by the spec's apt directive, AFTER the toolchain layer.
	specLine := strings.Join(lines[2:], "\n")
	if !strings.Contains(specLine, "'ripgrep'") {
		t.Errorf("spec package 'ripgrep' missing from the render:\n%s", got)
	}
	if !strings.Contains(specLine, "'jq'") {
		t.Errorf("spec package 'jq' missing from the render:\n%s", got)
	}
}

// TestDockerfile_StockToolchainSetIsTheStockUserland pins the CONTENT of the
// stock set itself: every item the row names (git, the docker client,
// python3, make) plus the agent-tools-REQUIRED item a stock agent satisfies
// (git — the GAP-092 measurement: a fresh stock agent has git and jq; toolsd,
// rg and gopls are absent there). If someone trims the set, this test — not a
// dogfood run — is what fails first.
func TestDockerfile_StockToolchainSetIsTheStockUserland(t *testing.T) {
	set := map[string]bool{}
	for _, pkg := range StockToolchainPackages {
		set[pkg] = true
	}
	// git is REQUIRED by the agent-tools probe (internal/cli/agenttools.go:
	// "session, lease") and satisfied by a stock agent (2.43.0 on the
	// product's Ubuntu 24.04 hosts); the image build must not lose it.
	if !set["git"] {
		t.Error("stock toolchain lost git (agent-tools REQUIRED; present on every stock agent)")
	}
	// The docker CLIENT: a stock agent has /usr/bin/docker on PATH and exec
	// wires DOCKER_HOST at the agent's own rootless socket; an image without
	// the client makes rootless docker unreachable from exec.
	if !set["docker.io"] {
		t.Error("stock toolchain lost the docker client (docker.io provides /usr/bin/docker)")
	}
	// The row's remaining named items plus the GAP-092-measured stock jq.
	for _, pkg := range []string{"python3", "make", "jq", "ca-certificates"} {
		if !set[pkg] {
			t.Errorf("stock toolchain lost %q", pkg)
		}
	}
}

// TestDockerfile_StockToolchainOnEveryAllowedBase proves the preservation is
// not a default-base accident: every base a spec may name renders the
// toolchain layer (all allowed bases are apt-family, so the layer is valid
// everywhere).
func TestDockerfile_StockToolchainOnEveryAllowedBase(t *testing.T) {
	for _, base := range AllowedBases() {
		spec := &Spec{
			Base:     base,
			Packages: []PackageAdd{{Manager: ManagerAPT, Packages: []string{"ripgrep"}}},
		}
		got := spec.Dockerfile()
		for _, pkg := range StockToolchainPackages {
			if !strings.Contains(got, "'"+pkg+"'") {
				t.Errorf("base %q: render does not provision stock package %q:\n%s", base, pkg, got)
			}
		}
	}
}

// TestDockerfile_StockToolchainLineIsExact pins the rendered toolchain layer
// byte-for-byte: the layer rides renderAPT, so it must read exactly like a
// spec's own apt line. The golden constant below is what every per-manager
// golden row inserts after FROM — a deliberate change to the stock set flips
// this test AND the goldens in one place.
func TestDockerfile_StockToolchainLineIsExact(t *testing.T) {
	spec := &Spec{Base: DefaultBaseImage}
	got := spec.Dockerfile()
	want := "FROM " + DefaultBaseImage + "\n" + stockToolchainGoldenLine
	if got != want {
		t.Errorf("base-only Dockerfile() =\n%q\nwant\n%q", got, want)
	}
}

// stockToolchainGoldenLine is the exact DF-BUNKER-80 stock-toolchain RUN
// line: renderAPT over StockToolchainPackages. Shared by every golden row so
// the byte expectation has one source.
const stockToolchainGoldenLine = "RUN apt-get update && apt-get install -y --no-install-recommends 'ca-certificates' 'git' 'python3' 'make' 'jq' 'docker.io' && rm -rf /var/lib/apt/lists/*\n"

// TestDockerfile_StockToolchainSurvivesNoPackageSpec pins the boundary case:
// even a spec with NO package directives (base only) flips exec into
// container mode (GAP-069), so its image needs the stock userland too —
// otherwise spawning with an "empty" customization silently degrades the
// agent.
func TestDockerfile_StockToolchainSurvivesNoPackageSpec(t *testing.T) {
	spec := &Spec{Base: DefaultBaseImage}
	got := spec.Dockerfile()
	for _, pkg := range StockToolchainPackages {
		if !strings.Contains(got, "'"+pkg+"'") {
			t.Errorf("base-only spec render does not provision stock package %q:\n%s", pkg, got)
		}
	}
}
