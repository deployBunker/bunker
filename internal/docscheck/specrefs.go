// spec reference drift (DOC-042): the docs/spec/*.md pages are design
// authority — PRDs, sibling specs and evidence docs cite them by path or row
// id — yet nothing validated the paths they name, or that anything still
// points at them. Two drift classes are checked, both over the same parsed
// docs corpus the gate already reads (no second checker, no second corpus):
//
//  1. Broken references — every `docs/**.md`-shaped token inside a spec page
//     (root-relative, or `../`-relative resolved against the page's own
//     directory) must resolve to a page the corpus provides. A renamed or
//     deleted target is reported naming the token as written, the resolved
//     path, and the line.
//  2. Orphan pages — every docs/spec page must be cited by at least one
//     OTHER docs page, by file base name or by its leading row id (`BFS-004`
//     for `BFS-004-webdav-surface.md`; the corpus cites specs both ways). An
//     uncited spec page is reported on itself: design authority nothing
//     points at is either a missing link or a page to retire.
//
// The citation scan is deliberately documentation-lexical: Go-source comments
// citing a spec (internal/server/webdav/handler.go) are not scanned —
// code-graph importers are hilo's job, this rule governs documentation.
package docscheck

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	// reSpecRef matches a markdown-path token: root-relative (`docs/…`) or
	// parent-relative (`../…`), always ending in `.md`. Trailing line/section
	// pins (`PRD-bunker-fs.md:209`) and `#anchors` fall outside the match, so
	// they neither break nor excuse a reference.
	reSpecRef = regexp.MustCompile(`(?:docs/|\.\./)[A-Za-z0-9][A-Za-z0-9_./-]*\.md`)
	// reSpecRowID extracts the leading row id of a spec file base name
	// (`BFS-004` from `BFS-004-webdav-surface.md`). Pages without one are
	// cited by base name only.
	reSpecRowID = regexp.MustCompile(`^([A-Za-z]{2,}-\d+)`)
)

// checkSpecRefs runs the spec-ref-drift rule over every docs/spec page in
// in.KnownDocs. Skips (never false alarms): no docs read, or no page under
// docs/spec/ — callers that read no spec pages are unaffected.
func checkSpecRefs(in Input) []Problem {
	if len(in.KnownDocs) == 0 {
		return nil
	}
	var specPaths []string
	for p := range in.KnownDocs {
		if strings.HasPrefix(p, "docs/spec/") {
			specPaths = append(specPaths, p)
		}
	}
	if len(specPaths) == 0 {
		return nil
	}
	sort.Strings(specPaths)
	var problems []Problem
	for _, path := range specPaths {
		problems = append(problems, brokenSpecRefProblems(path, in.KnownDocs[path], in.KnownDocs)...)
	}
	problems = append(problems, orphanSpecProblems(in.KnownDocs)...)
	return problems
}

// brokenSpecRefProblems flags every unique path token in doc that resolves to
// a page the corpus does not provide, in first-occurrence order. The
// resolution is the same for both spellings: `docs/…` is root-relative,
// anything else is relative to the page's directory and cleaned
// (`docs/spec` + `../prd/X.md` → `docs/prd/X.md`).
func brokenSpecRefProblems(path, doc string, known map[string]string) []Problem {
	var problems []Problem
	seen := map[string]bool{}
	for _, loc := range reSpecRef.FindAllStringIndex(doc, -1) {
		if loc[0] > 0 && inRefWord(doc[loc[0]-1]) {
			continue // path fragment inside a longer word, not a reference
		}
		token := doc[loc[0]:loc[1]]
		if seen[token] {
			continue // one finding per unique token: repeat cites stay quiet
		}
		seen[token] = true
		resolved := resolveSpecRef(token, path)
		if _, ok := known[resolved]; ok {
			continue
		}
		problems = append(problems, Problem{
			File: path,
			Line: 1 + strings.Count(doc[:loc[0]], "\n"),
			Rule: RuleSpecRefDrift,
			Message: fmt.Sprintf("references %q — resolves to %s, which no docs/**.md page provides; "+
				"fix the path, restore the target, or drop the reference", token, resolved),
		})
	}
	return problems
}

// inRefWord reports whether b is a byte that would make a `docs/`-shaped
// match a suffix of a longer word (`mydocs/x.md`), in which case it is not a
// root-relative reference.
func inRefWord(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '.', b == '-', b == '_':
		return true
	}
	return false
}

// resolveSpecRef maps one token as written to its repo-root-relative path.
func resolveSpecRef(token, pagePath string) string {
	if strings.HasPrefix(token, "docs/") {
		return token
	}
	return filepath.ToSlash(filepath.Join(filepath.Dir(pagePath), token))
}

// orphanSpecProblems flags every docs/spec page no OTHER docs page cites, by
// base name or row id. The page itself is the finding's File, so the report
// points at the page to link or retire, not at a directionless rule name.
func orphanSpecProblems(known map[string]string) []Problem {
	var specPaths []string
	for p := range known {
		if strings.HasPrefix(p, "docs/spec/") {
			specPaths = append(specPaths, p)
		}
	}
	sort.Strings(specPaths)
	var problems []Problem
	for _, path := range specPaths {
		base := filepath.Base(path)
		rowID := specRowID(base)
		var reID *regexp.Regexp
		if rowID != "" {
			reID = regexp.MustCompile(`\b` + regexp.QuoteMeta(rowID) + `\b`)
		}
		cited := false
		for other, body := range known {
			if other == path {
				continue // a page never cites itself out of orphanhood
			}
			if strings.Contains(body, base) || (reID != nil && reID.MatchString(body)) {
				cited = true
				break
			}
		}
		if cited {
			continue
		}
		problems = append(problems, Problem{
			File: path,
			Rule: RuleSpecRefDrift,
			Message: fmt.Sprintf("no docs page cites this spec page — nothing under docs/ names %s%s; "+
				"link it from the owning PRD or spec index, or retire the page",
				base, rowIDClause(rowID)),
		})
	}
	return problems
}

// specRowID returns the leading row id of a spec base name, or "".
func specRowID(base string) string {
	if m := reSpecRowID.FindStringSubmatch(base); m != nil {
		return m[1]
	}
	return ""
}

func rowIDClause(rowID string) string {
	if rowID == "" {
		return ""
	}
	return " (or its row id " + rowID + ")"
}
