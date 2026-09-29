package imagespec

import (
	"fmt"
	"strings"
)

// Build-failure attribution (DF-BUNKER-79).
//
// A failed image build used to surface as a bare docker error — the operator saw
// `returned a non-zero code: 127` and the raw build output, with nothing saying
// WHICH spec directive died. The attribution below names the step, and it is
// derived from the SAME render path Dockerfile() uses (renderStockToolchain and
// each manager's Render), so the text can never drift from the emitted
// Dockerfile: if the render changes, the attribution follows it for free.

// buildLayer is one rendered build step of a spec: the shell command its RUN
// line carries and the directive that produced it (Manager == "" for the
// always-present stock-userland layer).
type buildLayer struct {
	Manager  PackageManager
	Packages []string
	Command  string // the RUN line's command text, without the leading "RUN "
}

// layers returns the spec's rendered build steps, in Dockerfile order, one per
// rendered RUN line. The first layer is always the stock userland (DF-BUNKER-80);
// then one layer per rendered line of each directive — a go or gem directive
// renders several lines (the go toolchain bootstrap plus one install per
// package), and each is attributable on its own.
func (s *Spec) layers() []buildLayer {
	var out []buildLayer
	var stock strings.Builder
	renderStockToolchain(&stock)
	out = append(out, buildLayer{
		Packages: StockToolchainPackages,
		Command:  commandOf(stock.String()),
	})
	for _, d := range s.Packages {
		def := d.Manager.Def()
		if def == nil || len(d.Packages) == 0 {
			continue
		}
		var b strings.Builder
		def.Render(&b, d.Packages)
		for _, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
			cmd := commandOf(line)
			if cmd == "" {
				continue
			}
			out = append(out, buildLayer{
				Manager:  d.Manager,
				Packages: packagesIn(cmd, d.Packages),
				Command:  cmd,
			})
		}
	}
	return out
}

// packagesIn returns the tokens one rendered step installs: the directive's own
// packages that appear in the line, plus any builder-constant toolchain package
// it names. The second half matters because a directive may render more than one
// step with different package sets — the go directive's bootstrap step installs
// GoToolchainPackages (golang-go), NOT the directive's packages, so attributing
// the directive's list to that step would describe the wrong install and would
// stop the package-name matching pass from recognising it. A line the pool
// cannot explain reports the directive's packages, so the note still says what
// the operator asked for.
func packagesIn(cmd string, directivePkgs []string) []string {
	pool := make([]string, 0, len(directivePkgs)+len(GoToolchainPackages))
	pool = append(pool, directivePkgs...)
	pool = append(pool, GoToolchainPackages...)
	var out []string
	for _, p := range pool {
		if strings.Contains(cmd, p) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return directivePkgs
	}
	return out
}

// Package-manager failure idioms that NAME a package in the build output. The
// second matching pass is built on these rather than on a bare substring search,
// because a substring search is not evidence: a live build of a go-only spec was
// attributed to the STOCK layer simply because `docker.io` — a stock package
// name — is also part of the base image reference every build output carries
// (`docker.io/library/ubuntu:24.04` in the FROM line). Only an idiom that names
// a package makes a match admissible, and the comparison against the layer's
// package list is then EXACT, so a short name like `jq` is usable evidence while
// an unrelated occurrence of the same characters is not.
var packageIdioms = []struct {
	// marker is the idiom (matched case-insensitively).
	marker string
	// quoted reports whether the package name follows the marker between single
	// quotes rather than as a bare word.
	quoted bool
}{
	// apt: "E: Unable to locate package golang-go"
	{marker: "unable to locate package "},
	// apt: "Package 'ripgrep' has no installation candidate"
	{marker: "package '", quoted: true},
}

// namedPackages returns every package name the output names through a
// package-manager failure idiom, deduplicated.
func namedPackages(buildOut string) map[string]bool {
	named := map[string]bool{}
	for _, line := range strings.Split(buildOut, "\n") {
		lower := strings.ToLower(line)
		for _, idiom := range packageIdioms {
			i := strings.Index(lower, idiom.marker)
			if i < 0 {
				continue
			}
			rest := line[i+len(idiom.marker):]
			var name string
			if idiom.quoted {
				if j := strings.IndexByte(rest, '\''); j >= 0 {
					name = rest[:j]
				}
			} else {
				name = firstWord(rest)
			}
			if name != "" {
				named[name] = true
			}
		}
	}
	return named
}

// firstWord returns the first whitespace-delimited word of s, with trailing
// punctuation stripped (apt ends the sentence with a period).
func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " 	\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, ".,:;")
}

// commandOf strips the Dockerfile instruction from one rendered line, so the
// layer holds the text a builder prints in its failure report (both the classic
// builder's `The command '/bin/sh -c <cmd>' ...` and BuildKit's `process
// "/bin/sh -c <cmd>" did not complete successfully` carry the command verbatim).
func commandOf(line string) string {
	return strings.TrimPrefix(strings.TrimSpace(line), "RUN ")
}

// commandSignature returns a command with any leading VAR=VALUE environment
// assignments stripped (`GOBIN=/usr/local/bin go install 'x'` →
// `go install 'x'`). A builder echoes the step as the SHELL sees it and is free
// to collapse or reorder the prefix, so the signature gives the attribution a
// second, still-specific handle on the same line: stripping only makes the
// needle SHORTER within one step, and no two rendered steps differ solely by
// their prefixes, so a signature match can never select a different step.
func commandSignature(cmd string) string {
	for {
		sp := strings.IndexAny(cmd, " 	")
		if sp <= 0 {
			return cmd
		}
		head := cmd[:sp]
		if !strings.Contains(head, "=") || strings.HasPrefix(head, "=") {
			return cmd
		}
		varName := head[:strings.IndexByte(head, '=')]
		if varName == "" {
			return cmd
		}
		for i := 0; i < len(varName); i++ {
			c := varName[i]
			ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')
			if !ok {
				return cmd
			}
		}
		cmd = strings.TrimLeft(cmd[sp:], " 	")
	}
}

// Attribution evidence must come from the FAILURE, not from the step listing:
// BuildKit echoes EVERY step's command while it builds — cached steps included —
// so matching a bare command text attributes an unrelated failure to whichever
// step is listed first. Measured live: a real log whose go install step failed
// was blamed on the stock userland layer, whose command appears as `#5 [2/4] RUN
// apt-get ...` + `#5 CACHED`. The forms below are the ones a builder emits ONLY
// for the instruction that failed, so a match is evidence.
const shellWrapper = "/bin/sh -c "

// failureForms returns the shapes in which a builder names the FAILING command:
//
//   - the shell wrapper inside the error line — both builders print it:
//     BuildKit: `process "/bin/sh -c <cmd>" did not complete successfully`
//     classic:  `The command '/bin/sh -c <cmd>' returned a non-zero code: N`
//   - BuildKit's quiet-mode context block, which marks the failing Dockerfile
//     line with `>>> RUN <cmd>` (no other instruction carries the marker)
func failureForms(cmd string) []string {
	return []string{
		`"` + shellWrapper + cmd + `"`,
		`'` + shellWrapper + cmd + `'`,
		">>> RUN " + cmd,
	}
}

// attributeLayer finds the layer a failed build stopped in: first by the exact
// command text the builder names as the failing one, then by that command with
// its environment prefix stripped (a builder echoes the step as the shell sees
// it and is free to collapse the prefix), then — for an output that names only
// the failing package (`E: Unable to locate package ripgrep`) — by that named
// package. Every pass is ordered by layer, so the stock userland (which the
// spec's own steps build on) wins a tie. It never invents an attribution:
// nothing matches means "unattributed", and a step merely being LISTED in the
// output is not a match.
func (s *Spec) attributeLayer(buildOut string) (buildLayer, bool) {
	layers := s.layers()
	for _, l := range layers {
		if l.Command == "" {
			continue
		}
		for _, f := range failureForms(l.Command) {
			if strings.Contains(buildOut, f) {
				return l, true
			}
		}
	}
	for _, l := range layers {
		sig := commandSignature(l.Command)
		if sig == l.Command {
			continue
		}
		for _, f := range failureForms(sig) {
			if strings.Contains(buildOut, f) {
				return l, true
			}
		}
	}
	if named := namedPackages(buildOut); len(named) > 0 {
		for _, l := range layers {
			for _, p := range l.Packages {
				if named[p] {
					return l, true
				}
			}
		}
	}
	return buildLayer{}, false
}

// BuildFailureNote renders the one-line attribution a failed rootless build
// carries (see Builder.BuildValidated): the failing spec directive and its
// packages, the failing stock-userland layer, or an honest "could not be
// attributed" plus the directives the spec actually declared. It never returns
// the empty string, so callers can append it unconditionally.
func (s *Spec) BuildFailureNote(buildOut string) string {
	if l, ok := s.attributeLayer(buildOut); ok {
		if l.Manager == "" {
			return fmt.Sprintf("the failed step is the stock toolchain layer (packages: %s)",
				strings.Join(l.Packages, ", "))
		}
		return fmt.Sprintf("the failed step is the spec directive %q (packages: %s; rendered step: RUN %s)",
			string(l.Manager), strings.Join(l.Packages, ", "), l.Command)
	}
	return fmt.Sprintf("the failed step could not be attributed to a directive; the spec declares: %s",
		s.directiveSummary())
}

// directiveSummary lists a spec's directives as manager(pkg, pkg) for error text.
func (s *Spec) directiveSummary() string {
	if len(s.Packages) == 0 {
		return "no package directives (the stock toolchain layer is the only step)"
	}
	parts := make([]string, 0, len(s.Packages))
	for _, d := range s.Packages {
		parts = append(parts, fmt.Sprintf("%s(%s)", d.Manager, strings.Join(d.Packages, ", ")))
	}
	return strings.Join(parts, ", ")
}
