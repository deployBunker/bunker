package docscheck

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ── fixture registry ────────────────────────────────────────────────────────

// flagSurfaceFixture is a synthetic per-command flag registry for the
// flag-surface tests. No test hardcodes the real tree's registry: the
// fixtures assert rule logic, the real-tree tests assert the wiring.
func flagSurfaceFixture() map[string]map[string]bool {
	return map[string]map[string]bool{
		"spawn":  {"server": true, "agent-id": true, "cpu": true, "memory": true, "disk": true, "ttl": true},
		"exec":   {"server": true, "timeout": true, "raw": true, "script": true},
		"run":    {"server": true, "timeout": true, "detach": true, "name": true, "env": true},
		"deploy": {"server": true, "ssh-port": true, "ssh-key": true, "ssh-host": true},
		// audit mirrors the real registry's shape: the group's own flag plus
		// the union of its subcommands' flags (list/export's query flags).
		"audit": {"json": true, "server": true, "agent": true, "method": true,
			"since": true, "until": true, "limit": true, "path": true},
		"list": {"server": true, "status": true, "page-size": true},
		"":     {"version": true, "config": true},
	}
}

func docsInput(docs map[string]string) Input {
	in := baseInput()
	in.Docs = docs
	in.TreeFlags = flagSurfaceFixture()
	return in
}

func flagProblems(t *testing.T, in Input) []Problem {
	t.Helper()
	var got []Problem
	for _, p := range Check(in) {
		if p.Rule == RuleDocsFlagSurface {
			got = append(got, p)
		}
	}
	return got
}

// ── reported / clean ────────────────────────────────────────────────────────

// TestCheckDocsFlagSurfaceReportsBadFlag: a docs block using a flag the
// command cannot accept is reported, naming file, line and flag — in fenced
// blocks and in prose inline spans (the GAP-087 defect is a prose span).
func TestCheckDocsFlagSurfaceReportsBadFlag(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		line int
		flag string
	}{
		{
			name: "fenced block: spawn --name is reported",
			doc:  "# Guide\n\n```bash\nbunker spawn --name build-1 --ttl 2h\n```\n",
			line: 4,
			flag: "--name",
		},
		{
			name: "fenced block: spawn --mem is reported",
			doc:  "# Guide\n\n```\nbunker spawn --mem 4g\n```\n",
			line: 4,
			flag: "--mem",
		},
		{
			name: "prose span (the GAP-087 shape) is reported",
			doc:  "# Guide\n\n1. **Spawn** — `bunker spawn --name build-1 --ttl 2h --cpu 2 --mem 4g`\n",
			line: 3,
			flag: "--name",
		},
		{
			name: "pipeline: only the bunker segment is checked",
			doc:  "# Guide\n\n```\nbunker spawn --mem 4g | tee log\n```\n",
			line: 4,
			flag: "--mem",
		},
		{
			name: "equals form is reported",
			doc:  "# Guide\n\n```\nbunker spawn --mem=4g\n```\n",
			line: 4,
			flag: "--mem",
		},
		{
			name: "two bad flags report once naming both",
			doc:  "# Guide\n\n```\nbunker spawn --name a --mem 4g\n```\n",
			line: 4,
			flag: "--name",
		},
		{
			name: "flag valid on a DIFFERENT command is still reported",
			doc:  "# Guide\n\n```\nbunker deploy --timeout 30 d/ a:/p\n```\n",
			line: 4,
			flag: "--timeout",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := flagProblems(t, docsInput(map[string]string{"docs/guide.md": tt.doc}))
			if len(got) != 1 {
				t.Fatalf("problems = %v, want exactly one", got)
			}
			if got[0].File != "docs/guide.md" {
				t.Errorf("file = %q, want docs/guide.md", got[0].File)
			}
			if got[0].Line != tt.line {
				t.Errorf("line = %d, want %d (%q)", got[0].Line, tt.line, tt.doc)
			}
			if !strings.Contains(got[0].Message, tt.flag) {
				t.Errorf("message %q does not name %s", got[0].Message, tt.flag)
			}
			if !strings.Contains(got[0].Message, "bunker spawn") && !strings.Contains(got[0].Message, "bunker deploy") {
				t.Errorf("message %q does not name the command", got[0].Message)
			}
		})
	}
}

// TestCheckDocsFlagSurfaceCleanDocsPass: the same docs, corrected, are clean —
// plus every legitimate docs pattern the real pages use.
func TestCheckDocsFlagSurfaceCleanDocsPass(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{
			name: "corrected spawn line",
			doc:  "# Guide\n\n```bash\nbunker spawn build-1 --ttl 2h --cpu 2 --memory 4294967296\n```\n",
		},
		{
			name: "flag with quoted pipeline value",
			doc:  "# Guide\n\n```\nbunker audit list --since \"$(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ)\"\n```\n",
		},
		{
			name: "pipeline to jq: downstream flags are not the CLI's",
			doc:  "# Guide\n\n```\nbunker audit export --agent abc123 | jq -r '.method' | sort | uniq -c\n```\n",
		},
		{
			name: "run: --detach before the -- separator",
			doc:  "# Guide\n\n```\nbunker run build-1 --detach -- sleep 600\n```\n",
		},
		{
			name: "run: everything after -- is the child command, not flags",
			doc:  "# Guide\n\n```\nbunker exec demo -- docker run --rm --name x hello-world\n```\n",
		},
		{
			name: "redirection after the invocation",
			doc:  "# Guide\n\n```\nbunker audit export > audit.jsonl\n```\n",
		},
		{
			name: "trailing shell comment",
			doc:  "# Guide\n\n```\nbunker use prod       # select default server\n```\n",
		},
		{
			name: "bare command without arguments",
			doc:  "# Guide\n\n```\nbunker spawn\n```\n",
		},
		{
			name: "env-assignment prefix ($ VAR=x bunker …)",
			doc:  "# Guide\n\n```\n$ BUNKER_TOKEN=x bunker connect http://h:1\n```\n",
		},
		{
			name: "sudo prefix",
			doc:  "# Guide\n\n```\nsudo bunker list\n```\n",
		},
		{
			name: "subcommand flags via group inheritance (audit list --server)",
			doc:  "# Guide\n\n```\nbunker audit list --server prod --agent a --limit 20\n```\n",
		},
		{
			name: "marker block with a HEAD-only flag passes",
			doc:  "# Guide\n\n```bash\n# Requires a build from HEAD.\nbunker spawn --agent-id build-1\n```\n",
		},
		{
			name: "prose span mentioning a valid flag",
			doc:  "# Guide\n\nUse `bunker spawn --ttl 6h` or `bunker exec demo --timeout 60 -- ls`.\n",
		},
		{
			name: "non-bunker lines are ignored",
			doc:  "# Guide\n\n```\ncurl -s http://h:1/api --header \"X: y\"\n./bunkerd --config /etc/bunkerd/config.yaml\ndocker run --rm hello\n```\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := flagProblems(t, docsInput(map[string]string{"docs/guide.md": tt.doc})); len(got) != 0 {
				t.Fatalf("clean docs reported: %v", got)
			}
		})
	}
}

// ── marker relaxation scope ─────────────────────────────────────────────────

// TestCheckDocsFlagSurfaceMarkerOnlyExcusesKnownFlags: the "requires a build
// from HEAD" marker excuses a flag that exists at HEAD on some command, never
// a flag that exists NOWHERE — a typo must not hide behind the release valve.
func TestCheckDocsFlagSurfaceMarkerOnlyExcusesKnownFlags(t *testing.T) {
	in := docsInput(map[string]string{"docs/guide.md": "# Guide\n\nRequires a build from HEAD:\n\n```bash\nbunker spawn --future-flag x\n```\n"})
	// --future-flag is on no command: reported despite the marker.
	if got := flagProblems(t, in); len(got) != 1 || !strings.Contains(got[0].Message, "--future-flag") {
		t.Fatalf("unknown flag under marker: got %v, want one finding naming --future-flag", got)
	}
	// --mem exists NOWHERE in the fixture registry (on any command): still
	// reported under the marker, since no command could ever accept it.
	// (--name is a poor stand-in: `run` legitimately has it.)
	// NOTE: the marker goes on the line ABOVE the fence — a marker inside the
	// fence info string does not parse as a fence at all (reFenceOpen's info
	// capture is a single non-whitespace run), which would make this case
	// vacuously green.
	if got := flagProblems(t, docsInput(map[string]string{"docs/guide.md": "# Guide\n\nRequires a build from HEAD:\n\n```bash\nbunker spawn --mem 4g\n```\n"})); len(got) != 1 {
		t.Fatalf("nowhere-known flag under marker: got %v, want one finding", got)
	}
	// A flag that exists on another command is excused by the marker (it may
	// be HEAD-only for THIS command).
	clean := docsInput(map[string]string{"docs/guide.md": "# Guide\n\nRequires a build from HEAD:\n\n```bash\nbunker spawn --detach\n```\n"})
	if got := flagProblems(t, clean); len(got) != 0 {
		t.Fatalf("marker-excused flag reported: %v", got)
	}
}

// ── degenerate registries stay silent ───────────────────────────────────────

// TestCheckDocsFlagSurfaceSilentOnEmptyRegistry: with no registry (or an
// empty one) the rule stays silent instead of reporting every flag as
// unknown — the real-tree test pins that this never happens in practice.
func TestCheckDocsFlagSurfaceSilentOnEmptyRegistry(t *testing.T) {
	doc := map[string]string{"docs/guide.md": "# G\n\n```bash\nbunker spawn --name a\n```\n"}
	in := baseInput()
	in.Docs = doc
	in.TreeFlags = nil
	if got := flagProblems(t, in); len(got) != 0 {
		t.Fatalf("nil registry reported: %v", got)
	}
	in.TreeFlags = map[string]map[string]bool{}
	if got := flagProblems(t, in); len(got) != 0 {
		t.Fatalf("empty registry reported: %v", got)
	}
	// Non-empty map but zero flags across all commands: silent as well.
	in.TreeFlags = map[string]map[string]bool{"spawn": {}}
	if got := flagProblems(t, in); len(got) != 0 {
		t.Fatalf("zero-flag registry reported: %v", got)
	}
}

// ── tokenizing unit tests ───────────────────────────────────────────────────

func TestInvocationTokens(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantToks []string
		wantIdx  int
		wantOK   bool
	}{
		{
			name:     "simple",
			line:     "bunker spawn --name a",
			wantToks: []string{"bunker", "spawn", "--name", "a"},
			wantIdx:  0,
			wantOK:   true,
		},
		{
			name:     "dollar prompt and sudo prefix",
			line:     "$ sudo bunker spawn",
			wantToks: []string{"sudo", "bunker", "spawn"},
			wantIdx:  1,
			wantOK:   true,
		},
		{
			name:     "quoted value keeps spaces and pipes",
			line:     `bunker audit list --since "$(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ)"`,
			wantToks: []string{"bunker", "audit", "list", "--since", "$(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ)"},
			wantIdx:  0,
			wantOK:   true,
		},
		{
			name:     "pipeline breaks the invocation at the pipe",
			line:     "bunker audit export --agent a | jq -r '.method'",
			wantToks: []string{"bunker", "audit", "export", "--agent", "a"},
			wantIdx:  0,
			wantOK:   true,
		},
		{
			name:     "redirection ends the invocation",
			line:     "bunker audit export > audit.jsonl",
			wantToks: []string{"bunker", "audit", "export"},
			wantIdx:  0,
			wantOK:   true,
		},
		{
			name:     "trailing comment is dropped",
			line:     "bunker use prod   # select default server",
			wantToks: []string{"bunker", "use", "prod"},
			wantIdx:  0,
			wantOK:   true,
		},
		{
			name:     "escaped space inside a token",
			line:     `bunker exec demo -- ls /tmp/my\ dir`,
			wantToks: []string{"bunker", "exec", "demo", "--", "ls", "/tmp/my dir"},
			wantIdx:  0,
			wantOK:   true,
		},
		{
			name:   "no invocation",
			line:   "curl -s http://h --header x",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toks, idx, ok := invocationTokens(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v (toks %v), want %v", ok, toks, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if idx != tt.wantIdx {
				t.Errorf("cmd index = %d, want %d", idx, tt.wantIdx)
			}
			if fmt.Sprint(toks) != fmt.Sprint(tt.wantToks) {
				t.Errorf("tokens = %v, want %v", toks, tt.wantToks)
			}
		})
	}
}

// ── registry derivation ─────────────────────────────────────────────────────

// TestFlagsForDerivesRealRegistryShapes: the registry is derived from the
// same sources ParseSurface reads — Var registrations, non-Var registrations,
// manual peels in DisableFlagParsing commands, root persistent flags, and
// group→subcommand inheritance.
func TestFlagsForDerivesRealRegistryShapes(t *testing.T) {
	mainSrc := `package main

func main() {
	root.PersistentFlags().String("config", "", "cfg path")
	root.Flags().BoolVar(&showVersion, "version", false, "v")
	root.AddCommand(cli.NewSpawnCommand())
	root.AddCommand(cli.NewRunCommand())
	root.AddCommand(cli.NewAuditCommand())
}
`
	files := map[string]string{
		"internal/cli/spawn.go": "package cli\n\nfunc NewSpawnCommand() *cobra.Command {\n" +
			"\tcmd := &cobra.Command{Use: \"spawn [agent-id]\"}\n" +
			"\tcmd.Flags().StringVar(&agentID, \"agent-id\", \"\", \"id\")\n" +
			"\tcmd.Flags().Uint64Var(&memoryMax, \"memory\", 0, \"bytes\")\n" +
			"\treturn cmd\n}\n",
		"internal/cli/run.go": "package cli\n\nfunc NewRunCommand() *cobra.Command {\n" +
			"\tcmd := &cobra.Command{Use: \"run\", DisableFlagParsing: true}\n" +
			"\tcmd.Flags().Bool(\"detach\", false, \"d\")\n" +
			"\t// peel loop:\n" +
			"\t// case rest[i] == \"--env\":\n" +
			"\treturn cmd\n}\n",
		"internal/cli/audit.go": "package cli\n\nfunc NewAuditCommand() *cobra.Command {\n" +
			"	cmd := &cobra.Command{Use: \"audit\"}\n" +
			"	cmd.Flags().BoolVar(&wantJSON, \"json\", false, \"j\")\n" +
			"	cmd.AddCommand(newAuditListCommand())\n" +
			"	return cmd\n}\n\n" +
			"func newAuditListCommand() *cobra.Command {\n" +
			"	cmd := &cobra.Command{Use: \"list\"}\n" +
			"	addAuditQueryFlags(cmd, f)\n" +
			"	return cmd\n}\n\n" +
			"func addAuditQueryFlags(cmd *cobra.Command, f *auditQueryFlags) {\n" +
			"	cmd.Flags().StringVar(&f.serverName, \"server\", \"\", \"s\")\n" +
			"}\n",
	}
	got := FlagsFor(mainSrc, files)
	if !got["spawn"]["agent-id"] || !got["spawn"]["memory"] {
		t.Errorf("spawn registry = %v, want agent-id + memory", got["spawn"])
	}
	if !got["run"]["detach"] || !got["run"]["env"] {
		t.Errorf("run registry = %v, want detach + env (peel)", got["run"])
	}
	if !got[""]["config"] || !got[""]["version"] {
		t.Errorf("root registry = %v, want config + version", got[""])
	}
	if !got["audit"]["json"] || !got["audit"]["server"] {
		t.Errorf("audit registry = %v, want json + server (via subcommand's helper call)", got["audit"])
	}
	// Commands with no flags are still present (authoritative "accepts none").
	if set, ok := got["nonexistent"]; ok {
		t.Errorf("unexpected command in registry: %v", set)
	}
}

// TestFlagsForSkipsNonLiteralRegistrations: registration names that are not
// plain string literals are skipped (under-approximation, never half-parsed).
func TestFlagsForSkipsNonLiteralRegistrations(t *testing.T) {
	mainSrc := "func main() { root.AddCommand(cli.NewACommand()) }\n"
	files := map[string]string{
		"internal/cli/a.go": "package cli\n\nfunc NewACommand() *cobra.Command {\n" +
			"\tcmd := &cobra.Command{Use: \"a\"}\n" +
			"\tcmd.Flags().StringVar(&x, \"ssh-\"+\"host\", \"\", \"concat\")\n" +
			"\tcmd.Flags().BoolVar(&y, `backtick`, false, \"raw\")\n" +
			"\treturn cmd\n}\n",
	}
	got := FlagsFor(mainSrc, files)
	if len(got["a"]) != 0 {
		t.Errorf("registry = %v, want empty (non-literal names skipped)", got["a"])
	}
}

// ── real-tree wiring ────────────────────────────────────────────────────────

// TestRealTreeFlagRegistryIsHealthy pins the premise the rule's silence
// depends on: the real registry is populated, spawn has the flags the
// corrected walkthrough uses, and the legacy bad flags exist nowhere.
func TestRealTreeFlagRegistryIsHealthy(t *testing.T) {
	root := repoRoot(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	flags, err := TreeFlags(root)
	if err != nil {
		t.Fatalf("TreeFlags: %v", err)
	}
	for _, cmd := range []string{"spawn", "exec", "deploy", "status", "metrics", "heartbeat", "destroy"} {
		if _, ok := flags[cmd]; !ok {
			t.Errorf("registry missing command %q: have %v", cmd, keysOf(flags))
		}
	}
	if len(flags["spawn"]) < 5 {
		t.Errorf("spawn registry suspiciously small: %v", flags["spawn"])
	}
	for _, want := range []string{"ttl", "cpu", "memory", "agent-id"} {
		if !flags["spawn"][want] {
			t.Errorf("spawn registry lacks --%s: %v", want, flags["spawn"])
		}
	}
	for cmd, set := range flags {
		for bad := range set {
			if bad == "name" && cmd == "spawn" {
				t.Errorf("spawn registry claims --name")
			}
			if bad == "mem" {
				t.Errorf("registry claims --mem on %s", cmd)
			}
		}
	}
}

// TestRealDocsPassFlagSurface is the pass path for the repository itself:
// every docs/*.md example invocation must satisfy the flag rule against the
// real tree registry (this is what the RED/GREEN proof below flips).
func TestRealDocsPassFlagSurface(t *testing.T) {
	root := repoRoot(t)
	facts := factsOrSkip(t, root)

	treeFlags, err := TreeFlags(root)
	if err != nil {
		t.Fatalf("TreeFlags: %v", err)
	}
	docs := map[string]string{}
	for _, name := range []string{"docs/integration.md", "docs/audit.md", "docs/exec-audit.md"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		docs[name] = string(b)
	}
	in := Input{
		ReadmePath:    "README.md",
		ChangelogPath: "CHANGELOG.md",
		Readme:        "# x\n",
		Changelog:     "# x\n\n## Unreleased\n",
		LatestTag:     facts.LatestTag,
		Surface:       facts.Surface,
		Docs:          docs,
		TreeFlags:     treeFlags,
	}
	var found []Problem
	for _, p := range Check(in) {
		if p.Rule == RuleDocsFlagSurface {
			found = append(found, p)
		}
	}
	for _, p := range found {
		t.Errorf("drift: %s", p)
	}
	if len(found) > 0 {
		t.Fatalf("%d docs flag-surface problem(s)", len(found))
	}
}

// TestVerifyRealTreeDocsFlagSurfaceIsClean runs the rule the way cmd/docs-drift
// does — through Verify over the real repository — and fails on any finding.
func TestVerifyRealTreeDocsFlagSurfaceIsClean(t *testing.T) {
	root := repoRoot(t)
	facts := factsOrSkip(t, root)
	problems, err := Verify(root, facts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, p := range problems {
		if p.Rule == RuleDocsFlagSurface {
			t.Errorf("Verify reported docs flag drift on the committed tree: %s", p)
		}
	}
}

// TestRealTreeDocsRejectsLegacyBadFlags is the falsification path: a COPY of
// docs/integration.md with the pre-GAP-087 lines re-injected fails the rule
// naming the file and flag (the RED proof, automated); the corrected page
// passes. The repository is never modified.
func TestRealTreeDocsRejectsLegacyBadFlags(t *testing.T) {
	root := repoRoot(t)
	facts := factsOrSkip(t, root)

	treeFlags, err := TreeFlags(root)
	if err != nil {
		t.Fatalf("TreeFlags: %v", err)
	}
	page, err := os.ReadFile(filepath.Join(root, "docs/integration.md"))
	if err != nil {
		t.Fatalf("read docs/integration.md: %v", err)
	}
	corrected := string(page)
	if !strings.Contains(corrected, "--memory 4294967296") || !strings.Contains(corrected, "deploy dist/ build-1") {
		t.Skip("docs/integration.md does not carry the GAP-087 corrections — nothing to flip")
	}

	var badLine string
	for _, line := range strings.Split(corrected, "\n") {
		if strings.Contains(line, "--memory 4294967296") {
			badLine = line
			break
		}
	}
	if badLine == "" {
		t.Skip("spawn line not found")
	}
	bad := strings.Replace(corrected, badLine,
		"1. **Spawn** — `bunker spawn --name build-1 --ttl 2h --cpu 2 --mem 4g`", 1)
	if bad == corrected {
		t.Skip("could not inject the legacy line")
	}

	in := Input{
		ReadmePath: "README.md", ChangelogPath: "CHANGELOG.md",
		Readme: "# x\n", Changelog: "# x\n\n## Unreleased\n",
		LatestTag: facts.LatestTag, Surface: facts.Surface,
		Docs:      map[string]string{"docs/integration.md": bad},
		TreeFlags: treeFlags,
	}
	var found []Problem
	for _, p := range Check(in) {
		if p.Rule == RuleDocsFlagSurface {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		t.Fatal("legacy bad line produced no docs-flag-surface finding (RED proof failed)")
	}
	var namedFile, namedFlag bool
	for _, p := range found {
		if strings.Contains(p.File, "docs/integration.md") {
			namedFile = true
		}
		if strings.Contains(p.Message, "--name") || strings.Contains(p.Message, "--mem") {
			namedFlag = true
		}
	}
	if !namedFile || !namedFlag {
		t.Fatalf("findings %v: want file docs/integration.md named and --name/--mem named", found)
	}

	// GREEN: the untouched corrected page is clean.
	in.Docs = map[string]string{"docs/integration.md": corrected}
	for _, p := range Check(in) {
		if p.Rule == RuleDocsFlagSurface {
			t.Errorf("corrected page reported: %s", p)
		}
	}
}

func keysOf(m map[string]map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
