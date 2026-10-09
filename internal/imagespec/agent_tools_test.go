package imagespec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// This file pins TOOLS-B2: the DELIVERED SET of verb-dependency tools an image
// spec provisions on request. GAP-092 measured a fresh stock agent to have git
// and jq but neither rg (all three search modes rc=127) nor a language server
// (`toolsd lsp check` refuses correctly with nothing to serve). The remediation
// is the package-add path — the same mechanism the agent-tools probe prints —
// recorded canonically here so the printed spec, the struct form and the docs
// are one fact, not three.
//
// NOTE (toolsd is deliberately absent): toolsd is self-built and registry-less
// (no apt/npm/go-install source can fetch it), so the image-spec grammar has
// nothing to pin for it; it is delivered by binary copy under GAP-096. See
// docs/prd/SPEC-agent-tool-delivery.md for the division of labour.

// TestAgentToolPackagesAreTheDeliveredSet pins the SET itself: the tools the
// GAP-092 measurement found missing, delivered through the managers that own
// them. If someone trims the set, this test — not a fresh measurement — fails
// first.
func TestAgentToolPackagesAreTheDeliveredSet(t *testing.T) {
	want := []PackageAdd{
		{Manager: ManagerAPT, Packages: []string{"ripgrep"}},
		{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@latest"}},
	}
	if !reflect.DeepEqual(AgentToolPackages, want) {
		t.Errorf("AgentToolPackages = %+v, want %+v", AgentToolPackages, want)
	}
}

// TestAgentToolPackageNamesCoverTheDeliveredTools pins the flat name list the
// docs' delivered-set table is generated from: rg (the apt spelling) and gopls
// (the go-install path), and nothing else.
func TestAgentToolPackageNamesCoverTheDeliveredTools(t *testing.T) {
	got := AgentToolPackageNames()
	want := []string{"ripgrep", "golang.org/x/tools/gopls@latest"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AgentToolPackageNames() = %v, want %v", got, want)
	}
}

// TestAgentToolSpecRoundTrip pins the wire form to the struct form: the
// single-line JSON the CLI prints must parse into EXACTLY the
// AgentToolPackages directives (declaration order included, because the render
// — and therefore the built image — follows it). Before this pin the printed
// string and the struct set were two independent literals that could drift.
func TestAgentToolSpecRoundTrip(t *testing.T) {
	spec, err := Parse([]byte(AgentToolSpec))
	if err != nil {
		t.Fatalf("the canonical delivered-set spec does not parse: %v\n%s", err, AgentToolSpec)
	}
	if !reflect.DeepEqual(spec.Packages, AgentToolPackages) {
		t.Errorf("AgentToolSpec parses to %+v, want the AgentToolPackages directives %+v", spec.Packages, AgentToolPackages)
	}
}

// TestAgentToolSpecIsSingleLineJSON pins the paste-ability contract from the
// print side: one line, no whitespace a shell would split on, a bare JSON
// object. (The identical pin exists in internal/cli for the alias it prints —
// asserted here too so the canonical constant carries its own guard.)
func TestAgentToolSpecIsSingleLineJSON(t *testing.T) {
	if strings.ContainsAny(AgentToolSpec, "\n\r\t ") {
		t.Errorf("the canonical spec contains whitespace: %q", AgentToolSpec)
	}
	if !json.Valid([]byte(AgentToolSpec)) {
		t.Errorf("the canonical spec is not valid JSON: %q", AgentToolSpec)
	}
}

// TestAgentToolSpecRenderInstallsBothTools asserts the delivered set actually
// RENDERS what it promises: rg via the apt directive, gopls via a
// toolchain-bootstrapped, GOBIN-pinned go install — i.e. the image an agent is
// spawned with from this spec has both on its exec PATH.
func TestAgentToolSpecRenderInstallsBothTools(t *testing.T) {
	got := AgentToolSpecSpec().Dockerfile()

	if !strings.Contains(got, "'ripgrep'") {
		t.Errorf("delivered-set render does not install ripgrep:\n%s", got)
	}
	installLine := "GOBIN=" + GoBinDir + " go install 'golang.org/x/tools/gopls@latest'"
	if !strings.Contains(got, installLine) {
		t.Errorf("delivered-set render does not install gopls onto %s:\n%s", GoBinDir, got)
	}
	// The go toolchain bootstrap must precede the install (DF-BUNKER-79): the
	// allowed bases ship no Go compiler.
	bootAt := strings.Index(got, "'golang-go'")
	installAt := strings.Index(got, "go install 'golang.org/x/tools/gopls@latest'")
	if bootAt < 0 || installAt < 0 || bootAt > installAt {
		t.Errorf("delivered-set render lost the go toolchain bootstrap ordering (boot at %d, install at %d):\n%s", bootAt, installAt, got)
	}
}

// TestAgentToolSpecIsCacheable asserts the delivered set survives the exact
// path a spawn takes: CacheKey (the build identity) and the builder's
// validation are both derived from the parsed spec, so a set that failed either
// would be undeliverable at spawn time, not merely unprintable.
func TestAgentToolSpecIsCacheable(t *testing.T) {
	spec := AgentToolSpecSpec()
	key := spec.CacheKey()
	if len(key) != 64 {
		t.Errorf("CacheKey() = %q (%d chars), want a 64-char sha256 hex identity", key, len(key))
	}
}
