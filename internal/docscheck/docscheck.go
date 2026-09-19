// Package docscheck verifies that the release documentation users actually act
// on (README.md + CHANGELOG.md) matches the CLI that is actually installable
// from the newest release tag (GAP-081: the documented surface was not the
// installable one — the README's primary install path, `go install ...@latest`,
// could not run the documented stop/start/restart or host-provision commands,
// and the freshness note called v0.1.3 the newest release).
//
// It evaluates exactly three rules:
//
//  1. Command surface — every command invoked inside the README's blocks
//     (fenced code blocks, including the CLI Commands table, and inline `bunker
//     <cmd>` spans in prose) either exists as a top-level command in the newest
//     release tag's internal/cli tree, or the block carries the explicit marker
//     "requires a build from HEAD". A fenced block that carries the marker
//     while every command in it IS released is reported too, so the marker
//     cannot outlive the release it was written for.
//  2. Release-tag wording — every release tag the README names is the newest
//     release tag or newer. A README that still calls an older tag the newest
//     release is stale.
//  3. CHANGELOG — when commits exist after the newest release tag, the
//     CHANGELOG carries an `## Unreleased` section, and that section comes
//     before the newest release section.
//
// The package is pure: rules operate on document contents plus a Surface and a
// tag/commit count supplied by the caller (see gitfacts.go for the git-backed
// gatherer). No network access, no tag is hardcoded, and every rule is
// testable against fixtures alone.
package docscheck

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Marker is the phrase a README block carries to declare that the commands it
// invokes are not in the newest release tag. Matching is case-insensitive, so
// the docs can write it in prose or as a `#` comment inside a code block.
const Marker = "requires a build from HEAD"

// Rule names reported on Problem.Rule.
const (
	// RuleCommandSurface marks a documented command that is absent from the
	// newest release tag while its block carries no Marker.
	RuleCommandSurface = "readme-command-surface"
	// RuleStaleTag marks a release tag mentioned in the README that is older
	// than the newest release tag.
	RuleStaleTag = "stale-release-tag"
	// RuleStaleMarker marks a block that declares Marker while every command
	// in it exists in the newest release tag.
	RuleStaleMarker = "stale-head-only-marker"
	// RuleChangelog marks a CHANGELOG that lacks the required Unreleased
	// section (or puts it after the newest release section) while commits
	// exist past the newest release tag.
	RuleChangelog = "changelog-unreleased"
)

// Command is one top-level CLI command plus the subcommands registered under it.
type Command struct {
	Name        string
	Subcommands []string
}

// Surface is the CLI surface a source tree exposes, keyed by top-level command
// name. It is derived from a tree (see TagSurface) rather than from a running
// binary so the check needs no build, no daemon and no network.
type Surface struct {
	Commands map[string]Command
}

// Has reports whether name is a top-level command of the surface.
func (s Surface) Has(name string) bool {
	_, ok := s.Commands[name]
	return ok
}

// Names returns the sorted top-level command names.
func (s Surface) Names() []string {
	names := make([]string, 0, len(s.Commands))
	for name := range s.Commands {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CommandRef is one command invocation found in a document.
type CommandRef struct {
	Name string
	Line int
}

// Input is everything the rules need. Every field is injectable, which is what
// keeps the rules free of git, network and ambient state.
type Input struct {
	// ReadmePath and ChangelogPath are used only in messages.
	ReadmePath    string
	ChangelogPath string

	Readme    string
	Changelog string

	// LatestTag is the newest release tag, e.g. "v0.1.4" (from
	// `git describe --tags --abbrev=0`).
	LatestTag string
	// Surface is the CLI surface of the tree AT LatestTag.
	Surface Surface
	// PostTagCommits is `git rev-list --count <LatestTag>..HEAD`.
	PostTagCommits int
}

// Problem is one drift finding.
type Problem struct {
	// File is the document the finding is in ("" for repository-level facts).
	File string
	// Line is the 1-based line number, or 0 when the finding is not tied to
	// one line.
	Line int
	// Rule is one of the Rule* constants.
	Rule    string
	Message string
}

func (p Problem) String() string {
	where := p.File
	if where == "" {
		where = "."
	}
	if p.Line > 0 {
		where = fmt.Sprintf("%s:%d", where, p.Line)
	}
	return fmt.Sprintf("%s: %s: %s", where, p.Rule, p.Message)
}

// Check evaluates every rule over in and returns the findings, in document
// order. An empty result means the documentation matches the release.
func Check(in Input) []Problem {
	var problems []Problem

	blocks := blocksOf(in.Readme)
	for i, b := range blocks {
		refs := b.commands()
		if len(refs) == 0 {
			continue
		}
		var missing []CommandRef
		for _, ref := range refs {
			if !in.Surface.Has(ref.Name) {
				missing = append(missing, ref)
			}
		}
		labelled := blockLabelled(blocks, i)
		switch {
		case len(missing) > 0 && !labelled:
			for _, ref := range missing {
				problems = append(problems, Problem{
					File: in.ReadmePath,
					Line: ref.Line,
					Rule: RuleCommandSurface,
					Message: fmt.Sprintf("`bunker %s` is not a command in the newest release tag (%s); "+
						"either drop it or mark the block that documents it as %q",
						ref.Name, in.LatestTag, Marker),
				})
			}
		case len(missing) == 0 && labelled && b.isFence:
			problems = append(problems, Problem{
				File: in.ReadmePath,
				Line: b.startLine,
				Rule: RuleStaleMarker,
				Message: fmt.Sprintf("block is marked %q but every command in it exists in the newest "+
					"release tag (%s) — remove the marker or split the block", Marker, in.LatestTag),
			})
		}
	}

	for _, mention := range tagMentions(in.Readme) {
		if CompareTags(mention.Tag, in.LatestTag) < 0 {
			problems = append(problems, Problem{
				File: in.ReadmePath,
				Line: mention.Line,
				Rule: RuleStaleTag,
				Message: fmt.Sprintf("names release tag %s, older than the newest release tag %s — "+
					"the newest-release wording is stale", mention.Tag, in.LatestTag),
			})
		}
	}

	if in.PostTagCommits > 0 {
		line, ok := unreleasedLine(in.Changelog)
		switch {
		case !ok:
			problems = append(problems, Problem{
				File: in.ChangelogPath,
				Rule: RuleChangelog,
				Message: fmt.Sprintf("%d commit(s) after %s but no `## Unreleased` section — "+
					"add one describing the not-yet-released work", in.PostTagCommits, in.LatestTag),
			})
		default:
			if first := firstVersionLine(in.Changelog); first > 0 && line > first {
				problems = append(problems, Problem{
					File: in.ChangelogPath,
					Line: line,
					Rule: RuleChangelog,
					Message: fmt.Sprintf("the `## Unreleased` section must come before the newest release "+
						"section (line %d)", first),
				})
			}
		}
	}

	return problems
}

// ParseCommands returns every command the README invokes, in document order.
// It is exported so tooling (and the tests) can print the documented surface.
func ParseCommands(readme string) []CommandRef {
	var refs []CommandRef
	for _, b := range blocksOf(readme) {
		refs = append(refs, b.commands()...)
	}
	return refs
}

// TagMention is a release tag named in a document.
type TagMention struct {
	Tag  string
	Line int
}

// ParseTagMentions returns every `vX.Y.Z` release tag named in doc.
func ParseTagMentions(doc string) []TagMention { return tagMentions(doc) }

// CompareTags compares two vX.Y.Z tags numerically: -1, 0 or 1. A tag that
// cannot be parsed sorts before a parseable one so it is never silently
// treated as newer.
func CompareTags(a, b string) int {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	switch {
	case !aok && !bok:
		return strings.Compare(a, b)
	case !aok:
		return -1
	case !bok:
		return 1
	}
	for i := range av {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVersion(tag string) ([3]int, bool) {
	m := reVersion.FindStringSubmatch(strings.TrimSpace(tag))
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// ── document parsing ────────────────────────────────────────────────────────

// block is one fenced code block or one run of contiguous non-blank prose.
type block struct {
	isFence   bool
	info      string
	startLine int // 1-based line of the opening fence, or of the first prose line
	lines     []string
}

var (
	reFenceOpen  = regexp.MustCompile("^\\s*(`{3,}|~{3,})\\s*(\\S*)\\s*$")
	reInvocation = regexp.MustCompile(`^(?:sudo\s+)?(?:\./)?bunker\s+([A-Za-z][A-Za-z0-9_-]*)`)
	reSpan       = regexp.MustCompile("`([^`\\n]+)`")
	reVersion    = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)
	reTagMention = regexp.MustCompile(`\bv(\d+\.\d+\.\d+)\b`)
)

// blocksOf splits a markdown document into fenced code blocks and prose runs.
func blocksOf(doc string) []block {
	lines := strings.Split(doc, "\n")
	var (
		blocks  []block
		prose   []string
		proseAt int
		fence   string
		cur     block
	)
	flushProse := func() {
		if len(prose) > 0 {
			blocks = append(blocks, block{startLine: proseAt, lines: prose})
			prose = nil
		}
	}
	for i, raw := range lines {
		lineNo := i + 1
		if fence != "" {
			if strings.HasPrefix(strings.TrimSpace(raw), fence) {
				blocks = append(blocks, cur)
				cur = block{}
				fence = ""
				continue
			}
			cur.lines = append(cur.lines, raw)
			continue
		}
		if m := reFenceOpen.FindStringSubmatch(raw); m != nil {
			flushProse()
			fence = m[1]
			cur = block{isFence: true, info: m[2], startLine: lineNo}
			continue
		}
		if strings.TrimSpace(raw) == "" {
			flushProse()
			continue
		}
		if len(prose) == 0 {
			proseAt = lineNo
		}
		prose = append(prose, raw)
	}
	if fence != "" {
		// Unterminated fence: treat what we collected as a block so a broken
		// document cannot silently hide its commands.
		blocks = append(blocks, cur)
	}
	flushProse()
	return blocks
}

// commands returns the commands invoked by the block. Fenced blocks contribute
// one entry per invocation line (comments are skipped); prose blocks contribute
// one entry per inline `bunker <cmd>` span.
func (b block) commands() []CommandRef {
	var refs []CommandRef
	if b.isFence {
		for i, line := range b.lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if ref, ok := invocation(trimmed); ok {
				ref.Line = b.startLine + i + 1
				refs = append(refs, ref)
			}
		}
		return refs
	}
	for i, line := range b.lines {
		for _, m := range reSpan.FindAllStringSubmatch(line, -1) {
			if ref, ok := invocation(strings.TrimSpace(m[1])); ok {
				ref.Line = b.startLine + i
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

// invocation parses one candidate command line/span. It deliberately ignores
// the daemon binary (`./bunkerd`), shell lines that merely mention the binary
// (`make build # ./bunker ...`), and flag-only invocations (`bunker --version`).
func invocation(s string) (CommandRef, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "$ ")
	m := reInvocation.FindStringSubmatch(s)
	if m == nil {
		return CommandRef{}, false
	}
	return CommandRef{Name: m[1]}, true
}

// blockLabelled reports whether the block carries Marker: in its own lines, in
// its fence info string, or on the nearest non-blank line above a fence.
func blockLabelled(blocks []block, idx int) bool {
	b := blocks[idx]
	for _, line := range b.lines {
		if hasMarker(line) {
			return true
		}
	}
	if hasMarker(b.info) {
		return true
	}
	if !b.isFence || idx == 0 {
		return false
	}
	// Only the block directly above the fence counts: a marker further away
	// belongs to a different section.
	for _, line := range blocks[idx-1].lines {
		if hasMarker(line) {
			return true
		}
	}
	return false
}

func hasMarker(s string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(Marker))
}

func tagMentions(doc string) []TagMention {
	var out []TagMention
	for i, line := range strings.Split(doc, "\n") {
		for _, m := range reTagMention.FindAllStringSubmatch(line, -1) {
			out = append(out, TagMention{Tag: "v" + m[1], Line: i + 1})
		}
	}
	return out
}

func unreleasedLine(changelog string) (int, bool) {
	re := regexp.MustCompile(`(?i)^##\s+unreleased\b`)
	for i, line := range strings.Split(changelog, "\n") {
		if re.MatchString(strings.TrimSpace(line)) {
			return i + 1, true
		}
	}
	return 0, false
}

func firstVersionLine(changelog string) int {
	re := regexp.MustCompile(`^##\s+\d`)
	for i, line := range strings.Split(changelog, "\n") {
		if re.MatchString(strings.TrimSpace(line)) {
			return i + 1
		}
	}
	return 0
}

// ── CLI surface parsing ─────────────────────────────────────────────────────

var (
	reAddCommand = regexp.MustCompile(`AddCommand\(\s*cli\.(New[A-Za-z0-9_]+Command)\(\)\s*\)`)
	reFuncDecl   = regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z0-9_]+)\s*\(`)
	reUse        = regexp.MustCompile(`Use:\s*"([^"]*)"`)
	reSubAdd     = regexp.MustCompile(`AddCommand\(\s*([A-Za-z0-9_]+)\(\)\s*\)`)
)

// ParseSurface derives a Surface from a `cmd/bunker/main.go` source and the
// `internal/cli` sources of the same tree. Top-level commands are exactly the
// ones main.go registers; a subcommand is resolved through the AddCommand call
// inside its parent's constructor. Keyed by source path for error messages.
func ParseSurface(mainSrc string, files map[string]string) (Surface, error) {
	type located struct {
		body string
		path string
	}
	funcs := map[string]located{}
	for path, src := range files {
		for name, body := range parseFuncs(src) {
			funcs[name] = located{body: body, path: path}
		}
	}

	surface := Surface{Commands: map[string]Command{}}
	for _, m := range reAddCommand.FindAllStringSubmatch(mainSrc, -1) {
		fn := m[1]
		loc, ok := funcs[fn]
		if !ok {
			return Surface{}, fmt.Errorf("cmd/bunker/main.go registers %s but %s does not define it", fn, "internal/cli")
		}
		name, err := useName(loc.body)
		if err != nil {
			return Surface{}, fmt.Errorf("%s: %w", loc.path, err)
		}
		var subs []string
		for _, sm := range reSubAdd.FindAllStringSubmatch(loc.body, -1) {
			sub, ok := funcs[sm[1]]
			if !ok {
				continue
			}
			if subName, err := useName(sub.body); err == nil {
				subs = append(subs, subName)
			}
		}
		sort.Strings(subs)
		surface.Commands[name] = Command{Name: name, Subcommands: subs}
	}
	if len(surface.Commands) == 0 {
		return Surface{}, fmt.Errorf("no `AddCommand(cli.New…Command())` registrations found in cmd/bunker/main.go")
	}
	return surface, nil
}

// parseFuncs splits a Go source into top-level function bodies keyed by name.
func parseFuncs(src string) map[string]string {
	out := map[string]string{}
	var (
		name  string
		lines []string
	)
	flush := func() {
		if name != "" {
			out[name] = strings.Join(lines, "\n")
		}
		name, lines = "", nil
	}
	for _, line := range strings.Split(src, "\n") {
		if m := reFuncDecl.FindStringSubmatch(line); m != nil && !strings.HasPrefix(line, "\t") {
			flush()
			name = m[1]
			continue
		}
		if name != "" {
			lines = append(lines, line)
		}
	}
	flush()
	return out
}

// useName returns the first word of the first `Use:` field in a constructor.
func useName(body string) (string, error) {
	m := reUse.FindStringSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("no `Use:` field found in constructor body")
	}
	fields := strings.Fields(m[1])
	if len(fields) == 0 {
		return "", fmt.Errorf("empty `Use:` field")
	}
	return fields[0], nil
}
