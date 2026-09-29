package imagespec

import (
	"strings"
	"testing"
)

// This file pins DF-BUNKER-79: a spec that names a `go`-manager package must
// render an image in which `go install` can actually RUN.
//
// The defect: renderGo emitted `RUN go install <pkg>` against the default base
// (docker.io/library/ubuntu:24.04), which ships no Go toolchain, so every
// go-manager build died at that step with `/bin/sh: 1: go: not found`
// (exit 127) — including the spec `bunker agent-tools --install` prints as the
// remediation for the tools it cannot deliver (DF-BUNKER-57). The advertised
// remediation could not build a single image.
//
// The tests below are byte/structure assertions on the RENDER (no docker, no
// network): the render is what the builder feeds docker, so it is the thing
// under test.

// goToolchainGoldenLine is the exact DF-BUNKER-79 toolchain-bootstrap RUN line:
// renderAPT over GoToolchainPackages. Shared with the per-manager goldens so the
// byte expectation has one source, exactly like stockToolchainGoldenLine.
const goToolchainGoldenLine = "RUN apt-get update && apt-get install -y --no-install-recommends 'golang-go' && rm -rf /var/lib/apt/lists/*\n"

// TestDockerfile_GoDirectiveBootstrapsToolchain is the acceptance test for
// DF-BUNKER-79 criterion 1: for a go-manager package the render carries a
// toolchain bootstrap, and that bootstrap comes BEFORE the first `go install`
// line (a bootstrap after the install would not help). Table-driven across the
// shapes an operator actually writes — one package, several packages, a
// directive next to another manager's, and every allowed base.
func TestDockerfile_GoDirectiveBootstrapsToolchain(t *testing.T) {
	tests := []struct {
		name string
		spec *Spec
	}{
		{
			// The verbatim spec `agent-tools --install` prints (DF-BUNKER-57):
			// apt ripgrep + go gopls.
			name: "the CLI's remediation spec (apt ripgrep + go gopls)",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{
				{Manager: ManagerAPT, Packages: []string{"ripgrep"}},
				{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@latest"}},
			}},
		},
		{
			name: "go only, single package",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{
				{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@v0.17.0"}},
			}},
		},
		{
			name: "go only, several packages",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{
				{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@v0.17.0", "honnef.co/go/tools/cmd/staticcheck@v0.5.1"}},
			}},
		},
		{
			name: "go declared before apt (declaration order is preserved)",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{
				{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@latest"}},
				{Manager: ManagerAPT, Packages: []string{"ripgrep"}},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.spec.Dockerfile()

			boot := strings.Index(got, goToolchainGoldenLine)
			if boot < 0 {
				t.Fatalf("go-manager render has no toolchain bootstrap (want the line %q):\n%s", goToolchainGoldenLine, got)
			}
			// The bootstrap installs the toolchain package, not something else.
			for _, pkg := range GoToolchainPackages {
				if !strings.Contains(got[boot:], "'"+pkg+"'") {
					t.Errorf("bootstrap line does not install %q:\n%s", pkg, got)
				}
			}

			// EVERY `go install` line must come after the bootstrap, and no
			// other line of the render may smuggle one in ahead of it.
			sawInstall := false
			for _, line := range strings.Split(got, "\n") {
				if !strings.Contains(line, "go install") {
					continue
				}
				sawInstall = true
				if at := strings.Index(got, line); at < boot {
					t.Errorf("`go install` line precedes the toolchain bootstrap at byte %d (bootstrap at %d):\n%q\n%s", at, boot, line, got)
				}
			}
			if !sawInstall {
				t.Errorf("render carries no `go install` line at all:\n%s", got)
			}
		})
	}
}

// TestDockerfile_GoDirectiveBootstrapsOnEveryAllowedBase proves the bootstrap is
// not a default-base accident: every base a spec may name renders it (the
// toolchain comes from the base's own apt archive, so the render is valid
// wherever the other apt layers are).
func TestDockerfile_GoDirectiveBootstrapsOnEveryAllowedBase(t *testing.T) {
	for _, base := range AllowedBases() {
		spec := &Spec{Base: base, Packages: []PackageAdd{
			{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@latest"}},
		}}
		got := spec.Dockerfile()
		if !strings.Contains(got, goToolchainGoldenLine) {
			t.Errorf("base %q: render has no Go toolchain bootstrap:\n%s", base, got)
		}
		if strings.Index(got, goToolchainGoldenLine) > strings.Index(got, "go install") {
			t.Errorf("base %q: bootstrap is rendered AFTER the install line:\n%s", base, got)
		}
	}
}

// TestDockerfile_GoInstallIsOnTheAgentExecPath pins the other half of "the
// advertised remediation works": the installed binary has to be FINDABLE.
// Module-aware `go install` writes to $GOBIN, else $GOPATH/bin — for the image's
// root user that is /root/go/bin, which appears in no exec PATH, so an unpinned
// install would build, install, and still read as ABSENT to `agent-tools`. The
// render pins GOBIN to GoBinDir, the first component of the server's agent exec
// PATH (agentExecBasePath) with $HOME/bin prepended.
func TestDockerfile_GoInstallIsOnTheAgentExecPath(t *testing.T) {
	if GoBinDir != "/usr/local/bin" {
		t.Fatalf("GoBinDir = %q, want /usr/local/bin (the first component of agentExecBasePath)", GoBinDir)
	}
	spec := &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{
		{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@latest"}},
	}}
	got := spec.Dockerfile()
	want := "RUN GOBIN=" + GoBinDir + " go install 'golang.org/x/tools/gopls@latest'\n"
	if !strings.Contains(got, want) {
		t.Errorf("go install line does not pin GOBIN:\ngot  %q\nwant it to contain %q", got, want)
	}
	// And nothing re-points GOBIN somewhere else later in the render.
	if n := strings.Count(got, "GOBIN="); n != 1 {
		t.Errorf("render pins GOBIN %d times, want exactly 1:\n%s", n, got)
	}
}

// TestDockerfile_GoBootstrapIsEmittedOnce pins the bootstrap's cardinality: one
// toolchain layer per render (a spec may declare at most one go directive), and
// the install lines never trigger a second apt layer.
func TestDockerfile_GoBootstrapIsEmittedOnce(t *testing.T) {
	spec := &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{
		{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@latest", "honnef.co/go/tools/cmd/staticcheck@latest"}},
	}}
	got := spec.Dockerfile()
	if n := strings.Count(got, goToolchainGoldenLine); n != 1 {
		t.Errorf("toolchain bootstrap rendered %d times, want exactly 1:\n%s", n, got)
	}
	if n := strings.Count(got, "go install"); n != 2 {
		t.Errorf("render carries %d `go install` lines, want 2 (one per package):\n%s", n, got)
	}
}

// TestDockerfile_OnlyTheGoDirectivePullsTheToolchain is the negative half: no
// other manager may drag a Go toolchain into the image (a package add stays the
// package add it claims to be, and apt-only images stay small). An EMPTY go
// directive renders nothing at all rather than bootstrapping Go for no package.
func TestDockerfile_OnlyTheGoDirectivePullsTheToolchain(t *testing.T) {
	for i := range managerDefs {
		def := &managerDefs[i]
		if def.Name == ManagerGo {
			continue
		}
		token := pickAcceptedName(t, def)
		spec := &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: def.Name, Packages: []string{token}}}}
		if got := spec.Dockerfile(); strings.Contains(got, "golang-go") {
			t.Errorf("manager %q render pulls a Go toolchain it never uses:\n%s", def.Name, got)
		}
	}
	empty := &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerGo, Packages: []string{}}}}
	if got := empty.Dockerfile(); strings.Contains(got, "golang-go") {
		t.Errorf("an empty go directive bootstraps a toolchain for no package:\n%s", got)
	}
}

// TestDockerfile_APTSpecsUnchangedByGoBootstrap is DF-BUNKER-79 criterion 2: the
// fix is confined to the go manager. These are the bytes captured from the
// pre-fix tree (d774b99) — the default spec, a single apt package, a multi
// package apt spec, and the always-on DF-BUNKER-80 stock layer — and they must
// stay byte-identical, because the change was supposed to affect exactly one
// manager's render and nothing else.
func TestDockerfile_APTSpecsUnchangedByGoBootstrap(t *testing.T) {
	const (
		baseLine     = "FROM " + "docker.io/library/ubuntu:24.04" + "\n"
		stockLine    = stockToolchainGoldenLine
		aptSingle    = "RUN apt-get update && apt-get install -y --no-install-recommends 'ripgrep' && rm -rf /var/lib/apt/lists/*\n"
		aptMulti     = "RUN apt-get update && apt-get install -y --no-install-recommends 'jq' 'curl=8.5.0-2ubuntu10' && rm -rf /var/lib/apt/lists/*\n"
		npmLine      = "RUN npm install -g 'typescript@5.6.3'\n"
		noDirectives = baseLine + stockLine
	)
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "no directives (default spec)",
			raw:  `{}`,
			want: noDirectives,
		},
		{
			// The CLI's remediation spec MINUS its go directive: the exact spec
			// DF-BUNKER-80's live A/B used to close the rg gap.
			name: "single apt package",
			raw:  `{"packages":[{"manager":"apt","packages":["ripgrep"]}]}`,
			want: baseLine + stockLine + aptSingle,
		},
		{
			name: "multi apt packages with a version pin",
			raw:  `{"packages":[{"manager":"apt","packages":["jq","curl=8.5.0-2ubuntu10"]}]}`,
			want: baseLine + stockLine + aptMulti,
		},
		{
			name: "apt plus a non-go manager",
			raw:  `{"packages":[{"manager":"apt","packages":["jq"]},{"manager":"npm","packages":["typescript@5.6.3"]}]}`,
			want: baseLine + stockLine +
				"RUN apt-get update && apt-get install -y --no-install-recommends 'jq' && rm -rf /var/lib/apt/lists/*\n" +
				npmLine,
		},
		{
			name: "explicit ubuntu 22.04 base, apt only",
			raw:  `{"base":"docker.io/library/ubuntu:22.04","packages":[{"manager":"apt","packages":["ripgrep"]}]}`,
			want: "FROM docker.io/library/ubuntu:22.04\n" + stockLine + aptSingle,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := Parse([]byte(tt.raw))
			if err != nil {
				t.Fatalf("Parse(%s): %v", tt.raw, err)
			}
			if got := spec.Dockerfile(); got != tt.want {
				t.Errorf("apt-only render drifted (DF-BUNKER-79 must touch ONLY the go manager):\ngot  %q\nwant %q", got, tt.want)
			}
		})
	}
}
