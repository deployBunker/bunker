package imagespec

import (
	"strings"
	"testing"
)

// This file carries the pins OVER AND ABOVE agent_tools_test.go's set,
// round-trip and render-substring assertions: the FULL byte-golden render of
// the delivered set, its layer ordering, and the delivery-path boundary (rg
// and gopls arrive through the package managers — apt "ripgrep" and go
// "golang.org/x/tools/gopls@latest" — never as copied binaries, and never by
// being absorbed into the stock layers). TOOLS-B2.
//
// The copy path belongs to the tool we build ourselves (toolsd, deliverable via
// `bunker agent-tools --install`); registry tools must keep version pinning and
// signature verification, which only a package manager provides.

// TestDockerfile_DeliveredToolsRenderIsBuildable pins the FULL render of the
// canonical delivered-set directive byte-for-byte: FROM, the DF-BUNKER-80 stock
// userland layer, the spec's own apt (ripgrep) layer, the DF-BUNKER-79 Go
// toolchain bootstrap, then the GOBIN-pinned install line. Pinning the whole
// text is what makes "buildable" structural — any edit that reorders a layer,
// drops the stock userland, or puts the bootstrap after the install flips this
// test in the same commit that changes the render.
//
// LIVE PROOF (bunker-mvp, 2026-10-09): this exact image built and a fresh spawn
// probed `rg` ripgrep 14.1.0 and `gopls` v0.23.0 PRESENT via `bunker
// agent-tools` (agent toolsb2b, image bunkerd-imagespec-80a9a4f0bea2).
func TestDockerfile_DeliveredToolsRenderIsBuildable(t *testing.T) {
	spec, err := Parse([]byte(AgentToolSpec))
	if err != nil {
		t.Fatalf("the canonical delivered-set spec does not parse: %v\n%s", err, AgentToolSpec)
	}
	if spec.Base != DefaultBaseImage {
		t.Errorf("base = %q, want the default base", spec.Base)
	}
	want := "FROM " + DefaultBaseImage + "\n" +
		stockToolchainGoldenLine +
		"RUN apt-get update && apt-get install -y --no-install-recommends 'ripgrep' && rm -rf /var/lib/apt/lists/*\n" +
		goToolchainGoldenLine +
		"RUN GOBIN=" + GoBinDir + " go install 'golang.org/x/tools/gopls@latest'\n"
	got := spec.Dockerfile()
	if got != want {
		t.Errorf("delivered-tools render drifted:\ngot  %q\nwant %q", got, want)
	}
	// Layer-order sanity, independent of the golden text: the stock userland
	// precedes everything, and the Go bootstrap precedes its install.
	stockAt := strings.Index(got, "'git'")
	bootAt := strings.Index(got, "'golang-go'")
	installAt := strings.Index(got, "go install")
	ordered := 0 <= stockAt && stockAt < bootAt && bootAt < installAt
	if !ordered {
		t.Errorf("layer order broken (stock %d, bootstrap %d, install %d):\n%s", stockAt, bootAt, installAt, got)
	}
}

// TestDeliveredToolsAreNotCopiedDeliverables is the boundary half of TOOLS-B2:
// rg and gopls arrive through package managers, so nothing about the COPY path
// may grow to carry them, and the stock layers may not quietly absorb them —
// they are SPEC packages, present only when a spec asks for them.
func TestDeliveredToolsAreNotCopiedDeliverables(t *testing.T) {
	for _, layer := range [][]string{StockToolchainPackages, GoToolchainPackages} {
		for _, pkg := range layer {
			if pkg == "ripgrep" || strings.Contains(pkg, "gopls") {
				t.Errorf("stock layer grew %q — rg/gopls are spec packages, not stock", pkg)
			}
			if strings.Contains(pkg, "toolsd") {
				t.Errorf("toolsd must never ride any package layer (self-built, no registry)")
			}
		}
	}
	for _, d := range AgentToolPackages {
		if d.Manager == ManagerAPT || d.Manager == ManagerGo {
			continue
		}
		t.Errorf("delivered set grew a %q directive; the canonical set is apt ripgrep + go gopls", d.Manager)
	}
}
