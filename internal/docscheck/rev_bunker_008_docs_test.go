// rev_bunker_008_docs_test.go — REV-BUNKER-008 docs pin.
//
// The row: the README's auth paragraph said only "an unauthenticated POST
// receives 401, a non-POST request returns 405 before auth runs" — it omitted
// the 415 content-negotiation leg between them, and said nothing about what
// the audit trail sees of the rejected probes. specs/api.md had the same gap.
//
// Both surfaces now state the ordering (405 method gate → 415 content
// negotiation → 401 auth) and the consequence (the 405/415 legs stop before
// the auth interceptor, so they leave no audit record and no deny-sink entry).
// This pin keeps the two documents AGREEING on that ordering: every block in
// either document that declares the auth ordering must carry all three status
// tokens in order plus the audit-invisibility statement. Prose may vary; the
// claims may not.
//
// The runtime half of the same claim — that the daemon really answers in that
// order and really writes nothing for the first two legs — is
// internal/server/rev_bunker_008_test.go. A docs-only pin would stay green
// while the daemon changed; a runtime-only pin would stay green while the
// documents drifted. Both are required.
//
// RED proof: the pre-fix wording of both surfaces (embedded below verbatim) is
// driven through the rule and must be flagged — without that, a revert that
// kept a marker but dropped the 415 leg could pass vacuously.
package docscheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// authOrderingMarker selects the blocks that make a claim about the ordering.
// Lowercase, matched case-insensitively: the README writes "Auth ordering on an
// RPC path" in its quickstart comment, "Auth ordering on the REST surface" in
// the demo paragraph and "Auth ordering is fixed" in the Services section;
// specs/api.md writes "Auth ordering on every RPC path".
const authOrderingMarker = "auth ordering"

// authOrderingLegs are the three gates, in the order every declaring block must
// state them.
var authOrderingLegs = []string{"405", "415", "401"}

// auditInvisibilityTokens are the claims that make the omission this row is
// about impossible to reintroduce: a reader must learn that the pre-auth legs
// are not recorded anywhere.
var auditInvisibilityTokens = []struct{ token, why string }{
	{"no audit record", "the pre-auth legs must be stated as unrecorded, not merely pre-auth"},
	{"deny-sink", "the sink that would have carried the refusal is named, so the claim is checkable"},
}

// authOrderingSections returns the markdown sections (split on any heading
// line) of doc that declare an ordering. Section-level on purpose: the README
// states the claim as a paragraph plus a numbered list plus a closing
// paragraph, so a paragraph-scoped rule would split one claim across three
// blocks and fail a correct document.
func authOrderingSections(doc string) []string {
	var sections []string
	var cur []string
	inFence := false
	flush := func() {
		if len(cur) > 0 {
			sections = append(sections, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		// Fenced code blocks carry "# ..." shell comments that are not
		// headings; a naive split would shred the quickstart claim.
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			cur = append(cur, line)
			continue
		}
		if !inFence && strings.HasPrefix(line, "#") {
			flush()
		}
		cur = append(cur, line)
	}
	flush()

	var declared []string
	for _, section := range sections {
		if strings.Contains(strings.ToLower(section), authOrderingMarker) {
			declared = append(declared, section)
		}
	}
	return declared
}

// authOrderingFindings evaluates the rule over one section: the three legs must
// appear, and they must appear in ascending order (405 before 415 before 401).
// A section that states 405 → 401 while dropping 415 is exactly the defect the
// row describes, so ordering is asserted, not mere presence.
func authOrderingFindings(section string) []string {
	var findings []string

	prev := -1
	prevLeg := ""
	for _, leg := range authOrderingLegs {
		idx := strings.Index(section, leg)
		if idx < 0 {
			findings = append(findings, "missing the "+leg+" leg")
			continue
		}
		if idx < prev {
			findings = append(findings, leg+" is stated before "+prevLeg+" — the ordering is 405 → 415 → 401")
		}
		prev, prevLeg = idx, leg
	}

	for _, want := range auditInvisibilityTokens {
		if !strings.Contains(strings.ToLower(section), want.token) {
			findings = append(findings, "missing "+want.token+" — "+want.why)
		}
	}
	return findings
}

// authOrderingDocFindings applies the rule to a whole document: at least one
// section must declare the ordering, and every section that does must state all
// of it (so a partial restatement cannot be added next to a complete one).
func authOrderingDocFindings(doc string) []string {
	sections := authOrderingSections(doc)
	if len(sections) == 0 {
		return []string{"no section declares the auth ordering (marker \"" + authOrderingMarker + "\" not found) — the ordering claim is absent entirely"}
	}
	var findings []string
	for i, section := range sections {
		for _, f := range authOrderingFindings(section) {
			findings = append(findings, "section "+itoa(i+1)+": "+f)
		}
	}
	return findings
}

// revBunker008PrefixBlocks are the verbatim pre-fix wordings of both surfaces.
// Each is embedded with no ordering marker (the pre-fix docs never said "auth
// ordering") and, separately, in a marker-carrying form that still omits the
// 415 leg — so both arms of the rule have to fire.
var revBunker008PrefixBlocks = []struct {
	name string
	doc  string
}{
	{
		name: "pre-fix README Services section",
		doc: "The REST surface is **POST-only** (connect-go, mounted without `WithHTTPGet`):\n" +
			"every method is `POST /bunker.v1.Bunkerd/<Rpc>` (or `/bunker.v1.Agent/<Rpc>`),\n" +
			"and any other HTTP method returns `405`. The auth interceptor runs on the POST\n" +
			"path only, so an unauthenticated request gets `401` **only when sent as POST** —\n" +
			"a `GET` returns `405` before auth.\n",
	},
	{
		name: "pre-fix README demo paragraph",
		doc: "Auth is enforced: every request needs a bearer token (`bunker connect --token`); " +
			"an unauthenticated **POST** receives `401` (the REST surface is POST-only — " +
			"a non-POST request returns `405` before auth runs).\n",
	},
	{
		name: "pre-fix specs/api.md POST-only blockquote",
		doc: "> **POST-only:** connect-go serves all RPCs over POST (HTTP/1.1 and HTTP/2);\n" +
			"> no route accepts any other method. Sending any non-POST request to one of\n" +
			"> the paths above returns `405 Method Not Allowed`. The auth interceptor runs\n" +
			"> on the POST path only, so an unauthenticated request receives `401\n" +
			"> Unauthenticated` only when sent as POST — a non-POST request returns 405\n" +
			"> instead of 401 because it never reaches the interceptor.\n",
	},
	{
		name: "marker present but the 415 leg dropped",
		doc: "Auth ordering is fixed: a non-POST method returns 405, then an unauthenticated " +
			"POST returns 401. Those legs leave no audit record and no deny sink entry.\n",
	},
	{
		name: "marker present but the legs are out of order",
		doc: "Auth ordering is fixed: an unauthenticated POST returns 401 after 405 for a " +
			"non-POST method, and 415 for a wrong media type. The legs leave no audit record " +
			"and no deny sink entry.\n",
	},
}

// TestREVBUNKER008_DocsRuleRejectsThePreFixWording is the RED proof: every
// pre-fix wording is flagged, so the rule cannot be vacuously green. The last
// two arms prove it is not just a marker check — a block that keeps the marker
// but drops the 415 leg, or states 401 before 415, still fails.
func TestREVBUNKER008_DocsRuleRejectsThePreFixWording(t *testing.T) {
	for _, tc := range revBunker008PrefixBlocks {
		if got := authOrderingDocFindings(tc.doc); len(got) == 0 {
			t.Errorf("the rule accepted %s (no findings) — it is vacuous:\n%s", tc.name, tc.doc)
		}
	}
}

// TestREVBUNKER008_DocsRuleAcceptsTheCorrectedWording is the positive control:
// the shipped wording shape must pass, otherwise the pin would fail on a
// correct document and get deleted by the next person.
func TestREVBUNKER008_DocsRuleAcceptsTheCorrectedWording(t *testing.T) {
	ok := "Auth ordering on an RPC path is fixed: a non-POST method returns 405, a POST " +
		"whose Content-Type is unserved returns 415, and only then does auth run — an " +
		"unauthenticated POST receives 401. The 405 and 415 legs stop before the auth " +
		"interceptor, so they leave no audit record and no deny-sink entry.\n"
	if got := authOrderingDocFindings(ok); len(got) != 0 {
		t.Fatalf("the rule rejected a correct block: %v\n%s", got, ok)
	}
}

// TestRealTreeREVBUNKER008ReadmeAndSpecAgreeOnTheAuthOrdering pins the two
// shipped surfaces: each declares the auth ordering at least once, every
// declaring block states 405 → 415 → 401, and every one of them says the
// pre-auth legs leave no audit record and no deny-sink entry.
func TestRealTreeREVBUNKER008ReadmeAndSpecAgreeOnTheAuthOrdering(t *testing.T) {
	root := repoRoot(t)

	for _, rel := range []string{"README.md", filepath.Join("specs", "api.md")} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("REV-BUNKER-008 docs pin cannot read %s: %v", rel, err)
		}
		doc := string(raw)
		if findings := authOrderingDocFindings(doc); len(findings) > 0 {
			for _, f := range findings {
				t.Errorf("REV-BUNKER-008: %s %s", rel, f)
			}
		}
		if sections := authOrderingSections(doc); len(sections) == 0 {
			t.Errorf("REV-BUNKER-008: %s declares no auth ordering at all", rel)
		} else {
			t.Logf("%s: %d section(s) declare the auth ordering", rel, len(sections))
		}
	}
}
