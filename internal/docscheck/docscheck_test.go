package docscheck

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ── fixtures ────────────────────────────────────────────────────────────────

// testSurface is a synthetic released surface: only these commands are in the
// "newest release tag" the fixtures assert against. No test hardcodes a real
// tag, so the suite keeps working after the next release.
func testSurface(names ...string) Surface {
	if len(names) == 0 {
		names = []string{"connect", "spawn", "list", "exec", "status", "destroy"}
	}
	s := Surface{Commands: map[string]Command{}}
	for _, n := range names {
		s.Commands[n] = Command{Name: n, Subcommands: []string{"prune"}}
	}
	return s
}

// baseInput is a documentation set that satisfies every rule, so each test
// changes exactly one thing.
func baseInput() Input {
	return Input{
		ReadmePath:     "README.md",
		ChangelogPath:  "CHANGELOG.md",
		Readme:         "# Title\n\n## Use the CLI\n\n```bash\nbunker spawn demo\nbunker exec demo -- uname -a\n```\n",
		Changelog:      "# Changelog\n\n## Unreleased\n\n### Added\n- thing\n\n## 0.1.4 (2026-09-13)\n\n### Added\n- older\n",
		LatestTag:      "v0.1.4",
		Surface:        testSurface(),
		PostTagCommits: 3,
	}
}

func rulesOf(problems []Problem) string {
	var names []string
	for _, p := range problems {
		names = append(names, p.Rule)
	}
	return strings.Join(names, ",")
}

// ── rule 1: documented command surface ──────────────────────────────────────

func TestCheckCommandSurfaceFencedBlocks(t *testing.T) {
	tests := []struct {
		name      string
		readme    string
		wantRules string
		wantNames []string
	}{
		{
			name:      "unlabelled release-only command fails",
			readme:    "# Doc\n\n```bash\nbunker spawn demo\nbunker stop demo\n```\n",
			wantRules: RuleCommandSurface,
			wantNames: []string{"stop"},
		},
		{
			name:      "two release-only commands report two findings",
			readme:    "# Doc\n\n```bash\nbunker stop demo\nbunker host-provision --apply\n```\n",
			wantRules: RuleCommandSurface + "," + RuleCommandSurface,
			wantNames: []string{"stop", "host-provision"},
		},
		{
			name:   "marker inside the block passes",
			readme: "# Doc\n\n```bash\n# Requires a build from HEAD — not in the newest release tag.\nbunker stop demo\n```\n",
		},
		{
			name:   "marker on the line above the fence passes",
			readme: "# Doc\n\nRequires a build from HEAD:\n\n```bash\nbunker stop demo\n```\n",
		},
		{
			name:   "marker in the fence info string passes",
			readme: "# Doc\n\n```bash requires a build from HEAD\nbunker stop demo\n```\n",
		},
		{
			name:   "released commands need no marker",
			readme: "# Doc\n\n```bash\nbunker spawn demo\nbunker list\n```\n",
		},
		{
			name:   "daemon binary and build lines are not CLI commands",
			readme: "# Doc\n\n```bash\nsudo ./bunkerd --config /etc/bunkerd/config.yaml\nmake build # produces ./bunker and ./bunkerd\nsudo install -m 0755 bunker /usr/local/bin/bunker\nbunker --version\nbunker\n```\n",
		},
		{
			name:   "comment lines are ignored",
			readme: "# Doc\n\n```bash\n# ./bunker stop is not documented here\ngo build -o bunker ./cmd/bunker\n```\n",
		},
		{
			name:      "cli commands table is checked line by line",
			readme:    "# Doc\n\n" + "```\nbunker connect   Register a server\nbunker linger    Inspect linger\n```\n",
			wantRules: RuleCommandSurface,
			wantNames: []string{"linger"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := baseInput()
			in.Readme = tt.readme
			problems := Check(in)
			if got := rulesOf(problems); got != tt.wantRules {
				t.Fatalf("rules = %q, want %q (problems: %v)", got, tt.wantRules, problems)
			}
			for i, name := range tt.wantNames {
				if !strings.Contains(problems[i].Message, "`bunker "+name+"`") {
					t.Errorf("problem %d = %q, want it to name `bunker %s`", i, problems[i].Message, name)
				}
				if problems[i].Line == 0 {
					t.Errorf("problem %d has no line number: %q", i, problems[i].Message)
				}
			}
		})
	}
}

func TestCheckCommandSurfaceProseBlocks(t *testing.T) {
	tests := []struct {
		name      string
		readme    string
		wantRules string
	}{
		{
			name:      "inline reference to a release-only command fails",
			readme:    "# Doc\n\n`bunker stop` pauses an agent without destroying it.\n",
			wantRules: RuleCommandSurface,
		},
		{
			name:   "inline reference with the marker in the same paragraph passes",
			readme: "# Doc\n\n`bunker stop` (requires a build from HEAD) pauses an agent.\n",
		},
		{
			name:   "inline references to released commands pass",
			readme: "# Doc\n\n`bunker spawn` creates an agent; `bunker status` shows it.\n",
		},
		{
			name:   "agent group, share path and daemon name are not commands",
			readme: "# Doc\n\n`bunker-agents` owns `/srv/bunker-share`; `bunkerd` runs as root.\n",
		},
		{
			name:      "marker in a different paragraph does not cover this one",
			readme:    "# Doc\n\nRequires a build from HEAD.\n\nThe `bunker stop` command pauses an agent.\n",
			wantRules: RuleCommandSurface,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := baseInput()
			in.Readme = tt.readme
			if got := rulesOf(Check(in)); got != tt.wantRules {
				t.Fatalf("rules = %q, want %q", got, tt.wantRules)
			}
		})
	}
}

func TestCheckStaleMarkerIsReported(t *testing.T) {
	// A fence that declares HEAD-only content while every command in it is
	// released means the marker outlived the release it was written for.
	in := baseInput()
	in.Readme = "# Doc\n\n```bash\n# Requires a build from HEAD.\nbunker spawn demo\n```\n"
	problems := Check(in)
	if len(problems) != 1 || problems[0].Rule != RuleStaleMarker {
		t.Fatalf("problems = %v, want one %s", problems, RuleStaleMarker)
	}

	// Prose is exempt: a labelled paragraph may legitimately explain released
	// commands next to HEAD-only ones.
	in.Readme = "# Doc\n\n`bunker spawn` requires a build from HEAD to be documented.\n"
	if problems := Check(in); len(problems) != 0 {
		t.Fatalf("prose problems = %v, want none", problems)
	}
}

// ── rule 2: release-tag wording ─────────────────────────────────────────────

func TestCheckStaleReleaseTagMentions(t *testing.T) {
	tests := []struct {
		name     string
		readme   string
		wantTags []string
	}{
		{
			name:     "older tag named",
			readme:   "# Install\n\n`go install ...@latest` serves the newest release tag (v0.1.3).\n",
			wantTags: []string{"v0.1.3"},
		},
		{
			name:     "older tag named twice",
			readme:   "# Doc\n\nthe v0.1.2 tag\nand the v0.1.0 tag\n",
			wantTags: []string{"v0.1.2", "v0.1.0"},
		},
		{
			name:   "newest tag named",
			readme: "# Doc\n\nthe newest release tag is v0.1.4\n",
		},
		{
			name:   "newer tag named",
			readme: "# Doc\n\nprepares v0.2.0\n",
		},
		{
			name:   "no tag named at all",
			readme: "# Doc\n\nthe newest release tag lags HEAD\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := baseInput()
			in.Readme = tt.readme
			problems := Check(in)
			if len(problems) != len(tt.wantTags) {
				t.Fatalf("problems = %v, want %d %s finding(s)", problems, len(tt.wantTags), RuleStaleTag)
			}
			for i, tag := range tt.wantTags {
				if problems[i].Rule != RuleStaleTag || !strings.Contains(problems[i].Message, tag) {
					t.Errorf("problem %d = %v, want %s naming %s", i, problems[i], RuleStaleTag, tag)
				}
			}
		})
	}
}

func TestCompareTags(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v0.1.4", "v0.1.4", 0},
		{"v0.1.3", "v0.1.4", -1},
		{"v0.2.0", "v0.1.4", 1},
		{"v0.1.10", "v0.1.9", 1},
		{"v1.0.0", "v0.9.9", 1},
		{"garbage", "v0.1.4", -1},
	}
	for _, tt := range tests {
		if got := CompareTags(tt.a, tt.b); got != tt.want {
			t.Errorf("CompareTags(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// ── rule 3: CHANGELOG Unreleased section ────────────────────────────────────

func TestCheckChangelogUnreleasedRule(t *testing.T) {
	withUnreleased := "# Changelog\n\n## Unreleased\n\n### Added\n- x\n\n## 0.1.4 (2026-09-13)\n"
	withoutUnreleased := "# Changelog\n\n## 0.1.4 (2026-09-13)\n\n### Added\n- x\n"
	unreleasedTooLate := "# Changelog\n\n## 0.1.4 (2026-09-13)\n\n### Added\n- x\n\n## Unreleased\n\n- y\n"

	tests := []struct {
		name      string
		changelog string
		commits   int
		want      int
	}{
		{"post-tag commits require Unreleased", withoutUnreleased, 7, 1},
		{"Unreleased present passes", withUnreleased, 7, 0},
		{"no post-tag commits needs nothing", withoutUnreleased, 0, 0},
		{"Unreleased after the release section fails", unreleasedTooLate, 7, 1},
		{"Unreleased after the release section with no commits is fine", unreleasedTooLate, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := baseInput()
			in.Changelog = tt.changelog
			in.PostTagCommits = tt.commits
			problems := Check(in)
			if len(problems) != tt.want {
				t.Fatalf("problems = %v, want %d", problems, tt.want)
			}
			if tt.want == 1 && problems[0].Rule != RuleChangelog {
				t.Fatalf("rule = %s, want %s", problems[0].Rule, RuleChangelog)
			}
		})
	}
}

// ── surface parsing ─────────────────────────────────────────────────────────

const fixtureMain = `package main

func main() {
	root.AddCommand(cli.NewSpawnCommand())
	root.AddCommand(cli.NewSystemdCommand())
}
`

func TestParseSurfaceDistinguishesTopLevelFromSubcommand(t *testing.T) {
	files := map[string]string{
		"internal/cli/spawn.go": "package cli\n\nfunc NewSpawnCommand() *cobra.Command {\n" +
			"\tcmd := &cobra.Command{\n\t\tUse:   \"spawn <agent-id>\",\n\t\tShort: \"Create an agent\",\n\t}\n\treturn cmd\n}\n",
		"internal/cli/systemd.go": "package cli\n\nfunc NewSystemdCommand() *cobra.Command {\n" +
			"\tcmd := &cobra.Command{\n\t\tUse:   \"systemd\",\n\t\tShort: \"Manage the service\",\n\t}\n" +
			"\tcmd.AddCommand(newSystemdStatusCommand())\n\treturn cmd\n}\n\n" +
			"func newSystemdStatusCommand() *cobra.Command {\n" +
			"\treturn &cobra.Command{Use: \"status\", Short: \"Service status\"}\n}\n",
		// Defined in the tree but NOT registered in main.go: the surface must
		// not claim it, or a README could document a dead command.
		"internal/cli/ghost.go": "package cli\n\nfunc NewGhostCommand() *cobra.Command {\n" +
			"\treturn &cobra.Command{Use: \"ghost\", Short: \"Not wired\"}\n}\n",
	}

	surface, err := ParseSurface(fixtureMain, files)
	if err != nil {
		t.Fatalf("ParseSurface: %v", err)
	}
	if !surface.Has("spawn") || !surface.Has("systemd") {
		t.Fatalf("surface = %v, want spawn and systemd", surface.Names())
	}
	if surface.Has("ghost") {
		t.Errorf("surface names unregistered command `ghost`: %v", surface.Names())
	}
	if surface.Has("status") {
		t.Errorf("surface lists subcommand `status` as top-level: %v", surface.Names())
	}
	if got := surface.Commands["systemd"].Subcommands; len(got) != 1 || got[0] != "status" {
		t.Errorf("systemd subcommands = %v, want [status]", got)
	}
	if got := surface.Commands["spawn"].Name; got != "spawn" {
		t.Errorf("spawn name = %q", got)
	}
}

func TestParseSurfaceFailsLoudlyOnUnresolvableRegistration(t *testing.T) {
	_, err := ParseSurface("func main() { root.AddCommand(cli.NewMissingCommand()) }\n", map[string]string{})
	if err == nil {
		t.Fatal("ParseSurface accepted a registration with no constructor")
	}
	if !strings.Contains(err.Error(), "NewMissingCommand") {
		t.Fatalf("error %q does not name the unresolved registration", err)
	}
}

// ── git-backed checks ───────────────────────────────────────────────────────

func repoRoot(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not inside a git work tree: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func factsOrSkip(t *testing.T, root string) GitFacts {
	t.Helper()
	facts, err := Gather(root)
	if errors.Is(err, ErrNoTags) {
		t.Skip("checkout has no release tags (shallow/fork/tarball)")
	}
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	return facts
}

func realInput(t *testing.T, root string, facts GitFacts) Input {
	t.Helper()
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	changelog, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG: %v", err)
	}
	return Input{
		ReadmePath:     "README.md",
		ChangelogPath:  "CHANGELOG.md",
		Readme:         string(readme),
		Changelog:      string(changelog),
		LatestTag:      facts.LatestTag,
		Surface:        facts.Surface,
		PostTagCommits: facts.PostTagCommits,
	}
}

// TestRealTreeHasNoDrift is the pass path for the repository itself: the shipped
// README/CHANGELOG must satisfy every rule against the newest release tag.
func TestRealTreeHasNoDrift(t *testing.T) {
	root := repoRoot(t)
	facts := factsOrSkip(t, root)

	if len(facts.Surface.Names()) == 0 {
		t.Fatalf("surface of %s is empty — the tag tree could not be parsed", facts.LatestTag)
	}
	problems := Check(realInput(t, root, facts))
	for _, p := range problems {
		t.Errorf("drift: %s", p)
	}
	if len(problems) > 0 {
		t.Fatalf("%d drift problem(s) against %s", len(problems), facts.LatestTag)
	}
}

// TestVerifyTempReadmeCopyFailsOnInjectedCommand is the falsification path: a
// COPY of the shipped README (in a temp dir, so the repository is untouched)
// gains a block documenting a command that is not in the newest release tag.
// The real command set is derived from the repository — no tag is hardcoded —
// and the same block passes once it carries the marker.
func TestVerifyTempReadmeCopyFailsOnInjectedCommand(t *testing.T) {
	root := repoRoot(t)
	facts := factsOrSkip(t, root)

	head, err := TagSurface(root, "HEAD")
	if err != nil {
		t.Fatalf("TagSurface(HEAD): %v", err)
	}
	var headOnly string
	for _, name := range head.Names() {
		if !facts.Surface.Has(name) {
			headOnly = name
			break
		}
	}
	if headOnly == "" {
		t.Skipf("every HEAD command is already in %s — nothing to inject", facts.LatestTag)
	}

	base := realInput(t, root, facts)
	if problems := Check(base); len(problems) > 0 {
		t.Fatalf("premise broken: shipped docs already drift (%v)", problems)
	}

	tmp := t.TempDir()
	readmePath := filepath.Join(tmp, "README.md")
	injected := "# Doc\n\n```bash\nbunker " + headOnly + " demo-agent\n```\n"
	if err := os.WriteFile(readmePath, []byte(base.Readme+"\n"+injected), 0o644); err != nil {
		t.Fatalf("write temp README: %v", err)
	}

	problems := Check(Input{
		ReadmePath:     readmePath,
		ChangelogPath:  base.ChangelogPath,
		Readme:         base.Readme + "\n" + injected,
		Changelog:      base.Changelog,
		LatestTag:      facts.LatestTag,
		Surface:        facts.Surface,
		PostTagCommits: facts.PostTagCommits,
	})
	if len(problems) != 1 {
		t.Fatalf("injected `bunker %s` produced %v, want exactly one finding", headOnly, problems)
	}
	if problems[0].Rule != RuleCommandSurface || !strings.Contains(problems[0].Message, headOnly) {
		t.Fatalf("finding = %v, want %s naming %s", problems[0], RuleCommandSurface, headOnly)
	}
	if problems[0].File != readmePath {
		t.Errorf("finding names %q, want %q", problems[0].File, readmePath)
	}

	// Same block, marker present: the label is the documented release valve.
	labelled := "# Doc\n\n```bash\n# requires a build from HEAD\nbunker " + headOnly + " demo-agent\n```\n"
	if got := Check(Input{
		ReadmePath:     readmePath,
		ChangelogPath:  base.ChangelogPath,
		Readme:         base.Readme + "\n" + labelled,
		Changelog:      base.Changelog,
		LatestTag:      facts.LatestTag,
		Surface:        facts.Surface,
		PostTagCommits: facts.PostTagCommits,
	}); len(got) != 0 {
		t.Fatalf("labelled block still reported: %v", got)
	}
}

// ── group doc coverage (GAP-088) ────────────────────────────────────────────

// TestGroupDocCoverageFailsOnMissingSubcommand: a documented group's doc page
// must name every subcommand the tree ships. The fixture registers audit with
// two subcommands in the tree surface; the page names only one.
func TestGroupDocCoverageFailsOnMissingSubcommand(t *testing.T) {
	in := baseInput()
	in.TreeSurface = Surface{Commands: map[string]Command{
		"audit": {Name: "audit", Subcommands: []string{"export", "list", "status", "verify"}},
	}}
	in.GroupDocs = map[string]string{"audit": "# Audit\n\n```bash\nbunker audit verify --path /x\nbunker audit list --agent a\nbunker audit export > out.jsonl\n```\n"}

	problems := Check(in)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one (missing status)", problems)
	}
	p := problems[0]
	if p.Rule != RuleGroupDocCoverage {
		t.Fatalf("rule = %q, want %q", p.Rule, RuleGroupDocCoverage)
	}
	if p.File != "docs/audit.md" {
		t.Errorf("file = %q, want docs/audit.md", p.File)
	}
	if !strings.Contains(p.Message, "`bunker audit status`") {
		t.Errorf("message %q does not name `bunker audit status`", p.Message)
	}
}

// TestGroupDocCoveragePassesWhenEverySubcommandIsDocumented: the fixture page
// names all four subcommands (fenced blocks, prose spans and a table row all
// count — the real page uses all three shapes).
func TestGroupDocCoveragePassesWhenEverySubcommandIsDocumented(t *testing.T) {
	in := baseInput()
	in.TreeSurface = Surface{Commands: map[string]Command{
		"audit": {Name: "audit", Subcommands: []string{"export", "list", "status", "verify"}},
	}}
	in.GroupDocs = map[string]string{"audit": "# Audit\n\n| Command |\n|---------|\n| `bunker audit status` shows state |\n\n```bash\nbunker audit verify\n```\n\nUse `bunker audit list` and `bunker audit export` for queries.\n"}

	if got := Check(in); len(got) != 0 {
		t.Fatalf("all-subcommands page reported: %v", got)
	}
}

// TestGroupDocCoverageIgnoresUnregisteredGroupsAndLeafCommands: groups absent
// from the registry, groups absent from the tree, and groups without
// subcommands never fire the rule — only registered documented groups are
// governed.
func TestGroupDocCoverageIgnoresUnregisteredGroupsAndLeafCommands(t *testing.T) {
	in := baseInput()
	in.TreeSurface = Surface{Commands: map[string]Command{
		"unknown-group": {Name: "unknown-group", Subcommands: []string{"a", "b"}},
		"version":       {Name: "version"}, // leaf: no subcommands
	}}
	// No GroupDocs at all: also the "caller read no pages" shape.
	if got := Check(in); len(got) != 0 {
		t.Fatalf("unregistered/leaf groups reported: %v", got)
	}
}

// TestVerifyReadsGroupDocPagesAndUsesTreeSurface is the falsification path
// against the REAL tree: Verify must read the registered doc page from disk
// and check it against the working-tree surface, so the shipped docs pass —
// and a copy of docs/audit.md with the status coverage stripped fails the rule
// naming `bunker audit status` (the GAP-088 defect, reproduced). The repo is
// never modified: the stripped page is checked via Check, not written to disk.
func TestVerifyReadsGroupDocPagesAndUsesTreeSurface(t *testing.T) {
	root := repoRoot(t)
	facts := factsOrSkip(t, root)

	tree, err := TreeSurface(root)
	if err != nil {
		t.Fatalf("TreeSurface(HEAD): %v", err)
	}
	auditCmd, ok := tree.Commands["audit"]
	if !ok || len(auditCmd.Subcommands) == 0 {
		t.Fatalf("tree surface has no audit group: %+v", tree.Commands)
	}

	// Pass path: Verify reads docs/audit.md from disk and the shipped page
	// covers every subcommand of the tree.
	problems, err := Verify(root, facts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, p := range problems {
		if p.Rule == RuleGroupDocCoverage {
			t.Fatalf("shipped docs/audit.md does not cover the tree's audit subcommands: %v", p)
		}
	}

	// Fail path: strip the status coverage (the GAP-088 state) and the rule
	// must fire naming `bunker audit status`.
	docPage, err := os.ReadFile(filepath.Join(root, "docs/audit.md"))
	if err != nil {
		t.Fatalf("read docs/audit.md: %v", err)
	}
	stripped := stripStatusCoverage(string(docPage))
	if stripped == string(docPage) {
		t.Skip("docs/audit.md carries no `bunker audit status` coverage to strip")
	}
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	changelog, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG: %v", err)
	}
	got := Check(Input{
		ReadmePath: "README.md", ChangelogPath: "CHANGELOG.md",
		Readme: string(readme), Changelog: string(changelog),
		LatestTag: facts.LatestTag, Surface: facts.Surface,
		PostTagCommits: facts.PostTagCommits,
		GroupDocs:      map[string]string{"audit": stripped}, TreeSurface: tree,
	})
	var found *Problem
	for i := range got {
		if got[i].Rule == RuleGroupDocCoverage {
			found = &got[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("stripped page produced no %s finding: %v", RuleGroupDocCoverage, got)
	}
	if !strings.Contains(found.Message, "`bunker audit status`") {
		t.Errorf("finding %q does not name `bunker audit status`", found.Message)
	}
}

// stripStatusCoverage returns doc with every `bunker audit status` phrase
// removed — across fences, spans, table cells and wrapped lines (the
// whitespace-run pattern is the same one the rule matches, so nothing
// survives) — a mechanical stand-in for "the page predates status".
func stripStatusCoverage(doc string) string {
	re := regexp.MustCompile(`(?i)bunker\s+audit\s+status`)
	return re.ReplaceAllString(doc, "")
}

// TestTreeSurfaceSeesSubcommandsTheTagLacks: TreeSurface must derive the
// working tree's group tree (so a subcommand shipped after the newest tag is
// still checked against the docs that ship with it).
func TestTreeSurfaceSeesSubcommandsTheTagLacks(t *testing.T) {
	root := repoRoot(t)
	facts := factsOrSkip(t, root)

	tree, err := TreeSurface(root)
	if err != nil {
		t.Fatalf("TreeSurface: %v", err)
	}
	var treeSubs int
	if c, ok := tree.Commands["audit"]; ok {
		treeSubs = len(c.Subcommands)
	}
	var tagSubs int
	if c, ok := facts.Surface.Commands["audit"]; ok {
		tagSubs = len(c.Subcommands)
	}
	if treeSubs <= tagSubs {
		t.Skipf("tag surface already covers the tree's audit subcommands (tag=%d tree=%d)", tagSubs, treeSubs)
	}
	t.Logf("tree surface sees %d audit subcommand(s) the tag %s lacks", treeSubs-tagSubs, facts.LatestTag)
}

// TestGatherSkipsTaglessCheckout pins the skip path: a repository with no tags
// must report ErrNoTags (callers warn and continue) rather than a hard failure.
func TestGatherSkipsTaglessCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Skipf("git %v unavailable in this environment: %v (%s)", args, err, out)
		}
	}
	if _, err := Gather(dir); !errors.Is(err, ErrNoTags) {
		t.Fatalf("Gather(tagless repo) = %v, want ErrNoTags", err)
	}
}
