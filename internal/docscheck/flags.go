// docs flag surface (GAP-087): every `bunker <cmd> …` invocation documented
// in docs/*.md must use long flags the parsed CLI can actually accept for
// that command. The registry is derived from the same sources ParseSurface
// reads (cmd/bunker/main.go + internal/cli), so the check needs no build and
// no daemon — and the rule is deliberately FLAG-LEVEL only: free-form
// argument grammar (placeholder values like <agent-id>, agent-id spelling
// rules, subcommand position, argument counts) is NOT validated, because a
// grammar checker over prose examples cannot be written without crying wolf.
package docscheck

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	// reFlagCall matches every flag registration on a cobra command:
	// `cmd.Flags().StringVar(&serverName, "server", …)` and the non-Var
	// forms (`Flags().String("server", …)`), including PersistentFlags
	// receivers. The verb alternation deliberately excludes Lookup/Changed —
	// `Flags().Lookup("token")` is a read, not a registration.
	reFlagCall = regexp.MustCompile(`(?:Flags|PersistentFlags)\(\)\.(?:String|StringArray|StringSlice|Bool|Int|Int64|Uint8|Uint16|Uint32|Uint64|Float32|Float64|Duration)(Var)?\((?:\s*&[A-Za-z0-9_.]+,\s*)?"([^"\\]+)"`)
	// reFlagPeel matches the manual peel cases in DisableFlagParsing commands
	// (exec, run, env): `case rest[i] == "--detach":`. Those commands parse
	// their own flags before the `--` separator, so a source-level registry
	// that misses them would report documented `--detach`/`--env` usage as
	// unknown.
	reFlagPeel = regexp.MustCompile(`case\s+(?:rest|args)\[i\]\s*==\s*"(--[A-Za-z0-9][A-Za-z0-9-]*)"`)
	// reCallName matches an identifier followed by `(` — the shape of a call
	// to a helper function (e.g. addAuditQueryFlags(cmd, f)) whose body may
	// carry flag registrations.
	reCallName = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\(`)
)

// FlagsFor derives the per-command long-flag registry from a
// `cmd/bunker/main.go` source and the `internal/cli` sources of the same
// tree — the exact inputs ParseSurface takes. The key is the top-level
// command's Use name; the empty key holds the ROOT command's flags (--version,
// and the persistent --config/--daemon-config). Group flags inherit down to
// subcommands, and registrations made through helper functions
// (addAuditQueryFlags-style) are followed one call level deep, because that
// is how several shipped commands register their flags — an under-collected
// registry would false-report legitimate documented usage.
//
// Registration strings built by concatenation (`"ssh-"+"host"`) or backticks
// are skipped rather than half-parsed — an under-approximate registry is the
// honest direction for this rule (a missed registration can cause a false
// report; none exists in this tree, and a false report fails CI loudly and
// gets fixed, while a silently inflated registry would disable the rule).
func FlagsFor(mainSrc string, files map[string]string) map[string]map[string]bool {
	funcs := parseFuncsMap(files)
	flags := map[string]map[string]bool{}
	for _, m := range reAddCommand.FindAllStringSubmatch(mainSrc, -1) {
		body, ok := funcs[m[1]]
		if !ok {
			continue // ParseSurface already rejects this registration loudly
		}
		set := map[string]bool{}
		collectFlagTree(body, funcs, set, map[string]bool{})
		name, err := useName(body)
		if err != nil {
			continue // same registration ParseSurface would have rejected
		}
		flags[name] = set // present even when empty: authoritative "accepts none"
	}
	root := map[string]bool{}
	collectFlagLiterals(mainSrc, root)
	flags[""] = root
	return flags
}

// collectFlagTree gathers every flag name reachable from one command
// constructor body: its own registrations, the registrations of every
// subcommand it AddCommands, and the registrations inside helper functions it
// calls (bounded, visited-set-guarded recursion).
func collectFlagTree(body string, funcs map[string]string, set, visited map[string]bool) {
	if visited[body] {
		return
	}
	visited[body] = true
	collectFlagLiterals(body, set)
	for _, m := range reCallName.FindAllStringSubmatch(body, -1) {
		if helper, ok := funcs[m[1]]; ok && !visited[helper] {
			collectFlagTree(helper, funcs, set, visited)
		}
	}
}

// collectFlagLiterals extracts flag names from direct registration calls in
// one function body.
func collectFlagLiterals(body string, into map[string]bool) {
	for _, locs := range reFlagCall.FindAllStringSubmatchIndex(body, -1) {
		nameEnd := locs[5] // end of the quoted name (group 2)
		rest := body[nameEnd:]
		trimmed := strings.TrimLeft(rest, " \t")
		// A literal followed by `+` is string concatenation: skip it whole
		// rather than half-parsing (`"ssh-"+"host"` yields "ssh-").
		if strings.HasPrefix(trimmed, "+") {
			continue
		}
		addFlagName(into, body[locs[4]:nameEnd])
	}
	for _, m := range reFlagPeel.FindAllStringSubmatch(body, -1) {
		addFlagName(into, strings.TrimPrefix(m[1], "--"))
	}
}

// parseFuncsMap is parseFuncs over every file, matching ParseSurface's
// resolution order (later files win, same as the map-iteration order there).
func parseFuncsMap(files map[string]string) map[string]string {
	funcs := map[string]string{}
	for _, src := range files {
		for name, body := range parseFuncs(src) {
			funcs[name] = body
		}
	}
	return funcs
}

// addFlagName records one long flag name, lowercased (the comparison is
// case-sensitive in cobra, but every real flag here is lowercase and docs
// typos in case should still be caught).
func addFlagName(into map[string]bool, name string) {
	name = strings.TrimSpace(name)
	if !isFlagName(name) {
		return
	}
	into[strings.ToLower(name)] = true
}

// isFlagName reports whether s looks like a long-flag name (the shape every
// registration in this tree uses): lowercase letters, digits and interior
// hyphens — never empty, never leading/trailing hyphen. Guards against a
// half-parsed concatenation fragment ("ssh-") becoming a registry entry.
func isFlagName(s string) bool {
	if s == "" || s[len(s)-1] == '-' || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// ── invocation tokenizing ────────────────────────────────────────────────────

// invocationTokens splits one candidate invocation (a fenced code line, or the
// content of an inline prose span) into shell tokens the way a shell would
// (quote- and escape-aware), and reports the index of the `bunker` binary
// token. Tokenizing stops at the first UNQUOTED shell metacharacter that ends
// the bunker invocation — `|`, `;`, `&&`, `&`, `>`, `<`, or a word-initial `#`
// comment — so pipelines like `bunker audit export --agent a | jq -r …`
// contribute only the bunker segment's flags (jq's own flags are not the
// CLI's business), while flags inside quotes (`--since "$(date …)"`) are kept.
func invocationTokens(s string) ([]string, int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "$ ")
	if reInvocation.FindStringSubmatch(s) == nil {
		return nil, 0, false
	}
	var (
		toks  []string
		cur   strings.Builder
		inTok bool
		quote byte
	)
	flush := func() {
		if inTok {
			toks = append(toks, cur.String())
			cur.Reset()
			inTok = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			inTok = true
		case c == '\\' && i+1 < len(s):
			cur.WriteByte(s[i+1])
			inTok = true
			i++
		case c == ' ' || c == '\t':
			flush()
		case c == '|' || c == ';' || c == '>' || c == '<',
			c == '&' && (i+1 >= len(s) || s[i+1] == '&' || s[i+1] == ' ' || s[i+1] == '\t'),
			c == '#' && !inTok:
			// Unquoted metacharacter: the invocation ends here. Everything
			// after belongs to another command, the shell, or a comment.
			flush()
			return toks, bunkerTokenIndex(toks), true
		default:
			cur.WriteByte(c)
			inTok = true
		}
	}
	flush()
	return toks, bunkerTokenIndex(toks), true
}

// bunkerTokenIndex locates the `bunker` binary token (after an optional
// `sudo` or `./` prefix) so flag scanning starts after the command path.
func bunkerTokenIndex(toks []string) int {
	for i, tok := range toks {
		if tok == "bunker" || strings.HasSuffix(tok, "/bunker") {
			return i
		}
	}
	return -1
}

// ── the rule ────────────────────────────────────────────────────────────────

// checkDocsFlagSurface runs the docs-flag-surface rule over every doc page in
// in.Docs. Skips (never false alarms): no docs read → nothing to check; no
// command in the parsed registry declares any flag → the comparison surface
// is broken and the rule would report every flag as unknown, so it stays
// silent rather than crying wolf (the real-tree test pins that premise).
func checkDocsFlagSurface(in Input) []Problem {
	if len(in.Docs) == 0 || len(in.TreeFlags) == 0 {
		return nil
	}
	var total int
	for _, set := range in.TreeFlags {
		total += len(set)
	}
	if total == 0 {
		return nil
	}
	union := map[string]bool{}
	for _, set := range in.TreeFlags {
		for f := range set {
			union[f] = true
		}
	}
	paths := make([]string, 0, len(in.Docs))
	for p := range in.Docs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var problems []Problem
	for _, path := range paths {
		problems = append(problems, checkDocFlags(path, in.Docs[path], in.TreeFlags, union)...)
	}
	return problems
}

// checkDocFlags checks one document. The Marker relaxation mirrors rule 1:
// a block carrying "requires a build from HEAD" may document a flag the
// newest release does not have, so only flags unknown to EVERY command in the
// parsed surface are reported there (that bar is release-independent, so the
// marker cannot hide a typo behind the release valve).
func checkDocFlags(path, doc string, flags map[string]map[string]bool, union map[string]bool) []Problem {
	blocks := blocksOf(doc)
	var problems []Problem
	for i, b := range blocks {
		labelled := blockLabelled(blocks, i)
		if b.isFence {
			for li, line := range b.lines {
				toks, cmdIdx, ok := invocationTokens(line)
				if !ok || cmdIdx < 0 {
					continue
				}
				lineNo := b.startLine + li + 1 // +1: startLine is the opening fence
				if p, okB := flagProblem(path, lineNo, toks, cmdIdx, flags, union, labelled); okB {
					problems = append(problems, p)
				}
			}
			continue
		}
		// Prose block: inline `bunker …` spans (the GAP-087 defect is one).
		for li, line := range b.lines {
			for _, span := range reSpan.FindAllStringSubmatch(line, -1) {
				toks, cmdIdx, ok := invocationTokens(span[1])
				if !ok || cmdIdx < 0 {
					continue
				}
				lineNo := b.startLine + li
				if p, okB := flagProblem(path, lineNo, toks, cmdIdx, flags, union, labelled); okB {
					problems = append(problems, p)
				}
			}
		}
	}
	return problems
}

// flagProblem builds one finding for a tokenized invocation, or okB=false
// when the invocation carries no unknown flags (or no flag region at all).
func flagProblem(path string, lineNo int, toks []string, cmdIdx int, flags map[string]map[string]bool, union map[string]bool, labelled bool) (Problem, bool) {
	// Resolve the command path: the token right after the binary, unless it
	// is itself a flag (then this is a root-level invocation, e.g.
	// `bunker --version`).
	cmd := ""
	p := cmdIdx + 1
	if p < len(toks) && !strings.HasPrefix(toks[p], "-") {
		cmd = toks[p]
		p++
	}
	if _, known := flags[cmd]; !known {
		// The first path segment is not a registered command of this tree.
		// The command-surface rule owns unknown-COMMAND drift; flag-level
		// checking against the root's registry would be noise, so skip.
		return Problem{}, false
	}
	bad := unknownFlags(flags[cmd], toks[p:], union, labelled)
	if len(bad) == 0 {
		return Problem{}, false
	}
	return Problem{
		File:    path,
		Line:    lineNo,
		Rule:    RuleDocsFlagSurface,
		Message: flagProblemMessage(cmd, bad, flags[cmd], labelled),
	}, true
}

// unknownFlags returns the long flags in argRegion that cmd cannot accept,
// in document order.
func unknownFlags(own map[string]bool, argRegion []string, union map[string]bool, labelled bool) []string {
	var bad []string
	for _, tok := range argRegion {
		if !strings.HasPrefix(tok, "--") {
			continue
		}
		if tok == "--" {
			break // end-of-flags separator: everything after is the child command
		}
		name := strings.ToLower(strings.TrimPrefix(tok, "--"))
		if eq := strings.Index(name, "="); eq >= 0 {
			name = name[:eq]
		}
		if !isFlagName(name) {
			continue
		}
		if own[name] {
			continue
		}
		if labelled && union[name] {
			continue
		}
		bad = append(bad, "--"+name)
		// `--flag=value` consumes its value inline; a space-separated value
		// is left in argRegion. Not tracking which form this was only matters
		// for false negatives (a VALUE that looks like a long flag — none of
		// this tree's flag values do), so values are not skipped.
	}
	return bad
}

// flagProblemMessage renders one finding. The accept list is capped so a
// 13-flag command (host-provision) cannot bury the message.
func flagProblemMessage(cmd string, bad []string, accept map[string]bool, labelled bool) string {
	known := make([]string, 0, len(accept))
	for f := range accept {
		known = append(known, "--"+f)
	}
	sort.Strings(known)
	const maxList = 8
	if len(known) > maxList {
		known = append(known[:maxList:maxList], fmt.Sprintf("… %d more (see `bunker %s --help`)", len(accept)-maxList, cmd))
	}
	if len(known) == 0 {
		known = []string{"(none)"}
	}
	accepted := strings.Join(known, " ")
	shown := cmd
	if shown == "" {
		shown = "bunker (root)"
	}
	if labelled {
		return fmt.Sprintf("%s is not a flag of ANY command in the parsed CLI surface; the block is marked %q, which only "+
			"excuses flags that exist at HEAD — %s does not exist anywhere. `bunker %s` accepts: %s",
			strings.Join(bad, ", "), Marker, strings.Join(bad, ", "), shown, accepted)
	}
	return fmt.Sprintf("%s is not a flag of `bunker %s` in the parsed CLI surface; accept: %s",
		strings.Join(bad, ", "), shown, accepted)
}
