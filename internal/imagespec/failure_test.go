package imagespec

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file pins DF-BUNKER-79 criterion 3: a failed image build must say WHICH
// spec directive died. Before the fix the operator saw only docker's raw
// failure — `The command '/bin/sh -c go install ...' returned a non-zero code:
// 127` — which names a shell command but not the directive that produced it, so
// attributing the failure meant re-deriving the render by hand.

// classicBuildOutput is the shape the classic (non-BuildKit) builder prints: it
// echoes the RUN line, then the failing command verbatim.
func classicBuildOutput(step string) string {
	return "Sending build context to Docker daemon  4.096kB\n" +
		"Step 3/5 : " + step + "\n" +
		" ---> Running in 6f33f37a39f9\n" +
		"/bin/sh: 1: go: not found\n" +
		"The command '/bin/sh -c " + commandOf(step) + "' returned a non-zero code: 127\n"
}

// buildkitBuildOutput is the shape BuildKit prints with --quiet: the failing
// step and the process command, wrapped in its own error line.
func buildkitBuildOutput(step string) string {
	return "#4 [3/5] " + step + "\n" +
		"#4 0.216 /bin/sh: 1: go: not found\n" +
		"#4 ERROR: process \"/bin/sh -c " + commandOf(step) + "\" did not complete successfully: exit code: 127\n"
}

// remediationSpec is the verbatim spec `bunker agent-tools --install` prints
// (DF-BUNKER-57): apt ripgrep + go gopls.
var remediationSpec = &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{
	{Manager: ManagerAPT, Packages: []string{"ripgrep"}},
	{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@latest"}},
}}

// TestBuildFailureNote_NamesTheFailingDirective is the table-driven attribution
// test: for every builder output shape and every directive shape, the note names
// the manager that produced the failing step — and never a manager the spec did
// not declare.
func TestBuildFailureNote_NamesTheFailingDirective(t *testing.T) {
	goBootstrapStep := "RUN apt-get update && apt-get install -y --no-install-recommends 'golang-go' && rm -rf /var/lib/apt/lists/*"
	goInstallStep := "RUN GOBIN=/usr/local/bin go install 'golang.org/x/tools/gopls@latest'"
	aptStep := "RUN apt-get update && apt-get install -y --no-install-recommends 'ripgrep' && rm -rf /var/lib/apt/lists/*"

	tests := []struct {
		name    string
		spec    *Spec
		output  string
		want    []string // substrings the note must contain
		notWant []string // substrings the note must NOT contain
	}{
		{
			name:   "classic builder output for the go install step",
			spec:   remediationSpec,
			output: classicBuildOutput(goInstallStep),
			want:   []string{`directive "go"`, "golang.org/x/tools/gopls@latest", "rendered step"},
		},
		{
			name:   "buildkit output for the go bootstrap step",
			spec:   remediationSpec,
			output: buildkitBuildOutput(goBootstrapStep),
			want:   []string{`directive "go"`, "golang-go"},
		},
		{
			name:    "apt directive step fails (no go directive in the spec)",
			spec:    &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerAPT, Packages: []string{"ripgrep"}}}},
			output:  classicBuildOutput(aptStep),
			want:    []string{`directive "apt"`, "ripgrep"},
			notWant: []string{`directive "go"`},
		},
		{
			name:   "output names only the failing package",
			spec:   remediationSpec,
			output: "E: Unable to locate package golang-go",
			want:   []string{`directive "go"`, "golang-go"},
		},
		{
			name:   "output names only a package the stock layer owns",
			spec:   remediationSpec,
			output: "E: Unable to locate package ca-certificates",
			want:   []string{"stock toolchain layer", "ca-certificates"},
		},
		{
			name:   "stock layer step fails while a spec directive also exists",
			spec:   remediationSpec,
			output: classicBuildOutput(stockStepLine()),
			want:   []string{"stock toolchain layer", "docker.io"},
		},
		{
			name:   "unattributable output reports the spec's directives",
			spec:   remediationSpec,
			output: "docker: Error response from daemon: unexpected EOF",
			want:   []string{"could not be attributed", "apt(ripgrep)", "go(golang.org/x/tools/gopls@latest)"},
		},
		{
			// The ONLY admissible package-name evidence is a package-manager
			// idiom that names a package; the match against the layer's list is
			// then exact, so even a four-character name is usable. (`jq` is
			// deliberately NOT used here: it is in the stock set too, so the
			// stock layer — which runs first and wins a tie — owns the name.)
			name:   "an idiom-named short package is evidence",
			spec:   &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerAPT, Packages: []string{"curl"}}}},
			output: "E: Unable to locate package curl",
			want:   []string{`directive "apt"`, "curl"},
		},
		{
			// REGRESSION (found live, DF-BUNKER-79): the first cut matched a
			// package token as a bare substring, and every build output carries
			// `docker.io/library/ubuntu:24.04` in its FROM line — so `docker.io`,
			// a STOCK package, matched and this log was blamed on the stock
			// layer. Verbatim BuildKit output from a real `docker build` whose
			// failing step was a go install.
			name: "base-image boilerplate does not blame the stock layer",
			spec: remediationSpec,
			output: `#0 building with "default" instance using docker driver

#2 [internal] load metadata for docker.io/library/ubuntu:24.04
#4 [1/2] FROM docker.io/library/ubuntu:24.04@sha256:008173c23f95b170204355c12626cb5a965d779a7e1283b09e9cffbb1bf33ca3
#5 [2/2] RUN go install 'golang.org/x/tools/gopls@latest'
#5 0.428 /bin/sh: 1: go: not found
#5 ERROR: process "/bin/sh -c go install 'golang.org/x/tools/gopls@latest'" did not complete successfully: exit code: 127
`,
			want:    []string{`directive "go"`},
			notWant: []string{"stock toolchain layer"},
		},
		{
			// REGRESSION (found live, DF-BUNKER-79, second cut): BuildKit echoes
			// EVERY step's command, cached steps included, so a bare-text match
			// blamed the stock layer's `#5 [2/4] RUN apt-get ...` echo for a
			// failure that happened in the go step. Verbatim output of a real
			// non-quiet `docker build` (stock + golang-go layers CACHED, the go
			// install step failing).
			name: "a CACHED step's echoed command is not failure evidence",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{
				{Manager: ManagerGo, Packages: []string{"example.invalid/nonexistent-tool@latest"}},
			}},
			output: `#5 [2/4] RUN apt-get update && apt-get install -y --no-install-recommends 'ca-certificates' 'git' 'python3' 'make' 'jq' 'docker.io' && rm -rf /var/lib/apt/lists/*
#5 CACHED
#6 [3/4] RUN apt-get update && apt-get install -y --no-install-recommends 'golang-go' && rm -rf /var/lib/apt/lists/*
#6 CACHED
#7 [4/4] RUN GOBIN=/usr/local/bin go install 'example.invalid/nonexistent-tool@latest'
#7 2.329 go: example.invalid/nonexistent-tool@latest: unrecognized import path "example.invalid/nonexistent-tool": https fetch: Get "https://example.invalid/nonexistent-tool?go-get=1": dial tcp: lookup example.invalid on 192.168.123.1:53: no such host
#7 ERROR: process "/bin/sh -c GOBIN=/usr/local/bin go install 'example.invalid/nonexistent-tool@latest'" did not complete successfully: exit code: 1
`,
			want:    []string{`directive "go"`, "example.invalid/nonexistent-tool@latest"},
			notWant: []string{"stock toolchain layer"},
		},
		{
			// Verbatim output of the PRODUCTION invocation shape
			// (`docker build -q`, which is what the builder runs): the failing
			// Dockerfile line is marked `>>> RUN ...` and the error line wraps
			// the failing command in /bin/sh -c.
			name: "production -q output names the go directive",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{
				{Manager: ManagerGo, Packages: []string{"example.invalid/nonexistent-tool@latest"}},
			}},
			output: `Dockerfile.broken-go:4
--------------------
   2 |     RUN apt-get update && apt-get install -y --no-install-recommends 'ca-certificates' 'git' 'python3' 'make' 'jq' 'docker.io' && rm -rf /var/lib/apt/lists/*
   3 |     RUN apt-get update && apt-get install -y --no-install-recommends 'golang-go' && rm -rf /var/lib/apt/lists/*
   4 | >>> RUN GOBIN=/usr/local/bin go install 'example.invalid/nonexistent-tool@latest'
   5 |     
--------------------
ERROR: failed to solve: process "/bin/sh -c GOBIN=/usr/local/bin go install 'example.invalid/nonexistent-tool@latest'" did not complete successfully: exit code: 1
`,
			want:    []string{`directive "go"`, "example.invalid/nonexistent-tool@latest", "rendered step"},
			notWant: []string{"stock toolchain layer"},
		},
		{
			name:   "base-only spec has no directive to blame",
			spec:   &Spec{Base: DefaultBaseImage},
			output: "docker: unexpected EOF",
			want:   []string{"could not be attributed", "no package directives"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			note := tt.spec.BuildFailureNote(tt.output)
			if note == "" {
				t.Fatal("BuildFailureNote returned an empty note — callers append it unconditionally")
			}
			for _, w := range tt.want {
				if !strings.Contains(note, w) {
					t.Errorf("note %q does not contain %q", note, w)
				}
			}
			for _, nw := range tt.notWant {
				if strings.Contains(note, nw) {
					t.Errorf("note %q blames a directive the spec never declared (%q)", note, nw)
				}
			}
		})
	}
}

// TestBuildFailureNote_ExactDirectiveLine pins the note's shape for the canonical
// case — the very failure DF-BUNKER-79 was filed for: the go install step of the
// CLI's own remediation spec. The note must name the manager, the packages, and
// the rendered step the builder actually ran.
func TestBuildFailureNote_ExactDirectiveLine(t *testing.T) {
	out := "Step 3/3 : RUN GOBIN=/usr/local/bin go install 'golang.org/x/tools/gopls@latest'\n" +
		"/bin/sh: 1: go: not found\n" +
		"The command '/bin/sh -c GOBIN=/usr/local/bin go install 'golang.org/x/tools/gopls@latest'' returned a non-zero code: 127\n"
	want := `the failed step is the spec directive "go" (packages: golang.org/x/tools/gopls@latest; rendered step: RUN GOBIN=/usr/local/bin go install 'golang.org/x/tools/gopls@latest')`
	if got := remediationSpec.BuildFailureNote(out); got != want {
		t.Errorf("BuildFailureNote (canonical case)\ngot  %q\nwant %q", got, want)
	}
}

// TestBuildFailureNote_IsDerivedFromTheRender is the anti-drift guard: the step
// text the note quotes must be a line the Dockerfile actually contains. A
// hand-written attribution table could rot silently; this couples the note to
// the same render path.
func TestBuildFailureNote_IsDerivedFromTheRender(t *testing.T) {
	render := remediationSpec.Dockerfile()
	for _, layer := range remediationSpec.layers() {
		if layer.Command == "" {
			t.Fatalf("a rendered layer carries no command: %+v", layer)
		}
		if !strings.Contains(render, "RUN "+layer.Command) {
			t.Errorf("attribution layer is not in the Dockerfile render:\ncommand %q\nrender %q", layer.Command, render)
		}
	}
	// The go directive contributes two layers (bootstrap + install); the apt
	// directive and the stock layer one each. A render change that adds or
	// removes a step must be visible here rather than in a hand-written table.
	if got, want := len(remediationSpec.layers()), 4; got != want {
		t.Errorf("remediation spec has %d attribution layers, want %d: %+v", got, want, remediationSpec.layers())
	}
}

// stockStepLine returns the stock-userland RUN line as a spec renders it, for
// the "the stock layer failed" cell above.
func stockStepLine() string {
	var b strings.Builder
	renderStockToolchain(&b)
	line := strings.TrimRight(b.String(), "\n")
	return "RUN " + commandOf(line)
}

// failingOutputRunner emulates a docker build that failed, returning a fixed
// build output (the text the offline classifier parses).
type failingOutputRunner struct{ out string }

func (r failingOutputRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return []byte(r.out), errors.New("exit status 1")
}

// TestBuildImage_BuildFailureNamesFailingDirective is criterion 3 at the
// builder's own surface: the error BuildImage returns (which the spawn path
// wraps and the CLI prints) names the failing manager, keeps the docker output,
// and still satisfies errors.Is/As on the runner's error.
func TestBuildImage_BuildFailureNamesFailingDirective(t *testing.T) {
	goInstallStep := "RUN GOBIN=/usr/local/bin go install 'golang.org/x/tools/gopls@latest'"
	tests := []struct {
		name    string
		raw     string
		out     string
		want    []string
		notWant []string
	}{
		{
			// The verbatim DF-BUNKER-57 spec and the verbatim failure shape.
			name:    "the CLI's remediation spec fails at go install",
			raw:     `{"packages":[{"manager":"apt","packages":["ripgrep"]},{"manager":"go","packages":["golang.org/x/tools/gopls@latest"]}]}`,
			out:     classicBuildOutput(goInstallStep),
			want:    []string{"rootless build for agent1 failed", `directive "go"`, "golang.org/x/tools/gopls@latest", "(output:", "/bin/sh: 1: go: not found", "exit status 1"},
			notWant: []string{`directive "apt"`},
		},
		{
			name: "apt-only spec failure blames apt",
			raw:  `{"packages":[{"manager":"apt","packages":["ripgrep"]}]}`,
			out:  classicBuildOutput("RUN apt-get update && apt-get install -y --no-install-recommends 'ripgrep' && rm -rf /var/lib/apt/lists/*"),
			want: []string{`directive "apt"`, "ripgrep"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBuilder(failingOutputRunner{out: tt.out}, &CacheOptions{
				Dir:          filepath.Join(t.TempDir(), "cache"),
				BuildTimeout: time.Minute,
			})
			_, err := b.BuildImage(context.Background(), "agent1", []byte(tt.raw))
			if err == nil {
				t.Fatal("expected the build to fail")
			}
			msg := err.Error()
			for _, w := range tt.want {
				if !strings.Contains(msg, w) {
					t.Errorf("build error %q does not contain %q", msg, w)
				}
			}
			for _, nw := range tt.notWant {
				if strings.Contains(msg, nw) {
					t.Errorf("build error %q blames %q, which the spec never declared", msg, nw)
				}
			}
		})
	}
}
