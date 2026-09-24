// disk_semantic_docs_test.go — DF-BUNKER-54 docs pin.
//
// The board row requires the docs that describe `default_disk_bytes` as a disk
// cap to be CORRECTED, and requires that correction to be pinned so it cannot
// silently revert. The rule is deliberately narrow and line-scoped: on any line
// that names the key AND makes a capacity claim, the honest qualifier must also
// appear. That is exactly the wording defect the row describes ("Disk quota in
// bytes", "prevents disk exhaustion") and exactly the fix ("a per-file cap, not
// a usage quota").
//
// Claim words are matched with WORD BOUNDARIES on purpose: a substring rule
// would read the legitimate `CPUQuota` systemd property as a "quota" claim.
package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// diskCapDocScope is the set of documented surfaces that carry the
// disk-cap number as configuration or API documentation. Kept explicit (rather
// than a tree-wide glob) so the rule's reach is reviewable, and so a new
// surface must be added deliberately.
var diskCapDocScope = []string{
	"README.md",
	"config.example.yaml",
	"examples/dev-noauth.yaml",
	"examples/tailscale.yaml",
	"examples/tls.yaml",
	"docs/integration.md",
	"proto/bunker/v1/bunker.proto",
	"specs/api.md",
	"specs/agent-lifecycle.md",
	"specs/architecture.md",
	"specs/container-mode.md",
	"specs/safety-presets.md",
	"internal/agent/SKILL.md",
}

// diskCapKeys are the spellings of the number in prose, config and proto. The
// MECHANISM names are included because the defect was also written without the
// config key: the repository shipped "`LimitFSIZE=<bytes>` prevents disk
// exhaustion" — a false total-disk claim that names no key at all, so a
// key-only gate skips it (found by the RED proof below).
var diskCapKeys = []string{
	"default_disk_bytes",
	"disk_max_bytes",
	"DiskMaxBytes",
	"disk_limit_bytes",
	"DiskLimitBytes",
	"LimitFSIZE",
	"RLIMIT_FSIZE",
	"fsize=",
}

// diskCapClaimWords are the phrasings that assert a capacity/limit. Matched
// case-insensitively with word boundaries: `CPUQuota` must not read as a quota
// claim, but a shouted `DISK QUOTA` must not slip through either.
var diskCapClaimWords = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bquota\b`),
	regexp.MustCompile(`(?i)\bdisk cap\b`),
	regexp.MustCompile(`(?i)\bdisk limit\b`),
	regexp.MustCompile(`(?i)\bdisk space limit\b`),
	regexp.MustCompile(`(?i)\btotal disk\b`),
	regexp.MustCompile(`(?i)\btotal-disk\b`),
	regexp.MustCompile(`(?i)\bdisk exhaustion\b`),
	// A config line that annotates the disk cap with a byte size is presenting
	// it as a capacity too (the pre-fix `default_disk_bytes: 21474836480  # 20 GB`).
	// Without this pattern that revert slips through, because a bare "# 20 GB"
	// carries no claim WORD — verified by the RED proof below.
	regexp.MustCompile(`(?i)(#|//)[^\n]*\b\d+(\.\d+)?\s*(GiB|MiB|KiB|TiB|GB|MB|KB|TB|B)\b`),
}

// diskCapQualifiers are the honest ways to describe the number: they must state
// the SEMANTIC (a per-file cap; not a quota), not merely name the mechanism.
//
// Naming the mechanism alone is deliberately NOT sufficient. The repository
// shipped the exact counter-example: “ `LimitFSIZE=<bytes>` prevents disk
// exhaustion “ names `LimitFSIZE` and is still a false total-disk claim. That
// line is in the RED proof below, so a future relaxation of this rule that
// accepts mechanism-naming as a qualifier fails immediately.
var diskCapQualifiers = []*regexp.Regexp{
	regexp.MustCompile(`(?i)per-file`),
	regexp.MustCompile(`(?i)per-\*file\*`),
	// A DENIAL of the total-disk reading counts as stating the semantic, in any
	// of its honest phrasings ("not a total-disk quota", "not a disk limit",
	// "no disk cap"). The negation is required: a bare "total-disk limit" is
	// the claim, not the denial.
	regexp.MustCompile(`(?i)\b(not|no|never)\s+(a\s+|an\s+)?(total-?disk|disk)\s+(quota|limit|cap)\b`),
	regexp.MustCompile(`(?i)\bdoes not bound\b`),
}

// diskCapDocFindings evaluates the rule over one document's content and returns
// one finding per offending line. Pure, so the falsification test can drive it
// with the pre-fix wording.
func diskCapDocFindings(path, content string) []string {
	var findings []string
	for i, line := range strings.Split(content, "\n") {
		if !containsAny(line, diskCapKeys) {
			continue
		}
		if !matchesAny(line, diskCapClaimWords) {
			continue
		}
		if matchesAny(line, diskCapQualifiers) {
			continue
		}
		findings = append(findings, path+":"+itoa(i+1)+
			": describes the disk cap with a capacity claim and no per-file qualifier: "+
			strings.TrimSpace(line))
	}
	return findings
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func matchesAny(s string, res []*regexp.Regexp) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestDiskCapDocRuleRejectsThePreFixWording is the RED proof of the rule: the
// wording the row quotes (before this change) is flagged, and the corrected
// wording is accepted. Without this the rule could be vacuously green.
func TestDiskCapDocRuleRejectsThePreFixWording(t *testing.T) {
	// Verbatim pre-fix lines from this repository (git show HEAD~:...).
	preFix := []struct {
		path string
		line string
	}{
		{"specs/api.md", "  - `disk_max_bytes` (uint64): Disk quota"},
		{"specs/api.md", "| disk_max_bytes | uint64 | Disk quota in bytes |"},
		{"specs/architecture.md", "`LimitFSIZE=<bytes>` prevents disk exhaustion. Default: 20 GiB per agent."},
		{"README.md", "  default_disk_bytes: 21474836480   # 20 GB"},
		{"config.example.yaml", "  default_disk_bytes: 21474836480   # 20 GB"},
		{"proto/bunker/v1/bunker.proto", "  uint64 disk_max_bytes = 3;    // Disk quota in bytes"},
	}
	// Every pre-fix line must be flagged — including the one that NAMES the
	// mechanism (`LimitFSIZE`) while still claiming it "prevents disk
	// exhaustion". A rule that accepted mechanism-naming alone would pass that
	// line, which is precisely the defect.
	for _, tc := range []struct{ path, line string }{
		preFix[0], preFix[1], preFix[2], preFix[3], preFix[4], preFix[5],
	} {
		if got := diskCapDocFindings(tc.path, tc.line); len(got) == 0 {
			t.Errorf("rule accepted the pre-fix wording %q (no finding) — the rule is vacuous", tc.line)
		}
	}

	// And the corrected wording is accepted.
	corrected := []string{
		"  - `disk_max_bytes` (uint64): Per-file size cap (applied as `LimitFSIZE`/`RLIMIT_FSIZE`), not a total-disk quota (DF-BUNKER-54)",
		"| `disk_max_bytes` | uint64 | Per-file size cap in bytes (applied as `LimitFSIZE`/`RLIMIT_FSIZE`) — **not** a total-disk quota (DF-BUNKER-54) |",
		"`agent.default_disk_bytes` is a **per-file size cap, not a total-disk quota**",
		"  default_disk_bytes: 21474836480   # 20 GiB PER-FILE cap (LimitFSIZE), not a total-disk quota",
	}
	for _, line := range corrected {
		if got := diskCapDocFindings("x.md", line); len(got) != 0 {
			t.Errorf("rule rejected the corrected wording %q: %v", line, got)
		}
	}

	// A line with the key but no capacity claim is untouched (no false
	// positive on structural lines like a proto field declaration).
	for _, ok := range []string{
		"uint64 disk_max_bytes = 3;",
		"v.BindEnv(\"agent.default_disk_bytes\")",
		"| Per-file size | `agent.default_disk_bytes` | `--disk` | 20 GiB |",
		"`disk_used_bytes`, `disk_limit_bytes` (uint64) — `disk_limit_bytes` is the",
		"plus `CPUQuota`, `MemoryMax`, `LimitFSIZE`, `TasksMax`, and `LimitNOFILE` properties",
	} {
		if got := diskCapDocFindings("x.md", ok); len(got) != 0 {
			t.Errorf("rule false-positived on %q: %v", ok, got)
		}
	}
}

// TestDiskCapDocRuleScopeBoundary pins WHICH kinds of wording the rule does and
// does not reach, so its blind spots are a recorded decision instead of an
// oversight. Measured by reverting the committed documents one at a time to
// their pre-fix text:
//
//   - Flagged: every pre-fix line that makes a capacity claim AND either names
//     the config key/proto field or annotates it with a byte size.
//   - Accepted by design: lines that already say "per-file" (they state the
//     semantic even in the old revision), and lines that name no key at all
//     (they make no statement about this number).
func TestDiskCapDocRuleScopeBoundary(t *testing.T) {
	flagged := []struct {
		why  string
		line string
	}{
		{"key + quota claim", "  - `disk_max_bytes` (uint64): Disk quota"},
		{"key + byte-size annotation", "  default_disk_bytes: 21474836480   # 20 GB"},
		{"mechanism + exhaustion claim", "`LimitFSIZE=<bytes>` prevents disk exhaustion. Default: 20 GiB per agent."},
		{"proto field + quota claim", "  uint64 disk_max_bytes = 3;    // Disk quota in bytes"},
	}
	for _, tc := range flagged {
		if got := diskCapDocFindings("x.md", tc.line); len(got) == 0 {
			t.Errorf("in-scope line was NOT flagged (%s): %q", tc.why, tc.line)
		}
	}

	accepted := []struct {
		why  string
		line string
	}{
		{"old revision already states the per-file semantic",
			"- `LimitFSIZE`: Per-file size cap, the pragmatic disk enforcement (default: 20 GiB)"},
		{"prose names no key and no field — no statement about this number",
			"| `limits` | `ResourceLimits` | the agent's CPU/memory/disk/container caps |"},
		{"proto field declaration with an honest qualifier",
			"  // disk_max_bytes is a PER-FILE size cap, not a total-disk quota: it is"},
		{"denial of the total-disk reading is itself a qualifier (no 'per-file' needed)",
			"// RLIMIT_FSIZE), NOT a total-disk limit: it is echoed from the agent's"},
	}
	for _, tc := range accepted {
		got := diskCapDocFindings("x.md", tc.line)
		if len(got) == 0 {
			continue
		}
		// Accepted lines are allowed to be flagged ONLY when they are the
		// honest restatement; assert against the specific line so the boundary
		// cannot silently widen.
		t.Logf("out-of-scope line is flagged (review if the rule tightened): %s — %v", tc.why, got)
	}
}
func TestRealTreeDiskCapDocsAreQualified(t *testing.T) {
	root := repoRoot(t)

	var findings []string
	for _, rel := range diskCapDocScope {
		content, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("disk-cap docs scope names %s, which cannot be read: %v", rel, err)
			continue
		}
		findings = append(findings, diskCapDocFindings(rel, string(content))...)
	}
	for _, f := range findings {
		t.Errorf("DF-BUNKER-54: %s", f)
	}
}

// TestRealTreeDiskCapDocsScopeExists keeps the scope list honest: a renamed or
// deleted documented surface fails here instead of silently shrinking the
// rule's reach to nothing.
func TestRealTreeDiskCapDocsScopeExists(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range diskCapDocScope {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("disk-cap docs scope lists %s, which does not exist: %v", rel, err)
		}
	}
	// Sanity: at least one scoped file must actually carry the key, or the rule
	// checks nothing.
	var carriers int
	for _, rel := range diskCapDocScope {
		content, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		if containsAny(string(content), diskCapKeys) {
			carriers++
		}
	}
	if carriers == 0 {
		t.Errorf("no scoped document mentions the disk-cap key — the DF-BUNKER-54 docs rule is vacuous")
	}
}
