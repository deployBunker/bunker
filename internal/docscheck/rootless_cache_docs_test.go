// rootless_cache_docs_test.go — REV-BUNKER-004 (GAP-091) docs pin.
//
// The row: the GAP-091 host cache was documented as caching "downloaded
// rootless Docker installers (~93MB)" such that "a fresh-agent spawn skips the
// network download entirely". The code says otherwise, on both halves:
//
//   - WHAT the cache holds. rootlessInstallerCacheKey() returns
//     "rootless-installer-" + hostCacheSuffix(rootlessInstallURL) + "-" +
//     rootlessInstallerPinTag() + ".sh" — a SHELL SCRIPT filename — and
//     validateCachedInstaller() accepts a cached entry only when it is a
//     regular file of at least 8 bytes whose first non-blank bytes are "#!".
//     One ~9 KB setup script from get.docker.com/rootless. Not 93 MB.
//   - WHERE the payload goes. The ~90 MB docker-ce-rootless-extras tarball is
//     fetched by the installer's OWN CHILDREN inside the agent home (see
//     runInOwnProcessGroup's doc comment: "the `curl` fetching ~90MB of
//     docker-ce-rootless-extras"). installRootlessDocker's only per-home skip
//     is the `os.Stat(~/bin/dockerd-rootless.sh)` short-circuit; the payload
//     never enters the host cache.
//
// So a cache hit saves the SCRIPT fetch, not the payload. This file pins that
// reading on every surface that documents the knob, in the style of
// disk_semantic_docs_test.go: the rule is a pure function over document text
// (so it can be driven with the pre-fix wording as a RED proof), it is scoped
// to an explicit file list (so its reach is reviewable), and the scope list is
// itself asserted to still exist and still carry the knob (so a rename cannot
// shrink the rule to nothing).
package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rootlessCacheDocScope is the set of surfaces that document
// agent.rootless_installer_cache_dir. Explicit rather than a tree-wide glob:
// adding a surface to the rule's reach is a deliberate edit.
//
// README.md + config.example.yaml are the two the board row names. The spec
// table and the config package skill are included because they carried the
// same false "~93 MB installer" claim — leaving them out would let the
// finding re-file against a different surface after this one is fixed.
var rootlessCacheDocScope = []string{
	"README.md",
	"config.example.yaml",
	"specs/configuration.md",
	"internal/config/SKILL.md",
}

// rootlessCacheDocAnchors identify the line that documents the knob. Any one
// match starts the documentation block.
var rootlessCacheDocAnchors = []string{
	"rootless_installer_cache_dir",
	"Rootless installer cache",
	"Host-level cache for the rootless",
}

// rootlessCacheContentMarkers are the honest statements of WHAT the cache
// holds. The cache holds the setup script; a document that never says so has
// not corrected the overclaim.
var rootlessCacheContentMarker = regexp.MustCompile(`(?i)\bsetup script\b`)

// rootlessCachePayloadNames name the artifact that is NOT cached here. Naming
// it is what lets the reader connect the denial to the ~90 MB download the
// old text advertised.
var rootlessCachePayloadNames = []*regexp.Regexp{
	regexp.MustCompile(`(?i)docker-ce-rootless-extras`),
	regexp.MustCompile(`(?i)\bpayload\b`),
}

// rootlessCacheDenials deny that the payload is cached. Tolerance of a few
// non-word chars between the negation and the cache word is intentional: the
// spec table writes "is **not** cached here", and a strict `not\s+cached`
// would read that honest sentence as a claim.
var rootlessCacheDenial = regexp.MustCompile(`(?i)\b(not|never|isn't|aren't|isnt|arent)\b[^\n]{0,25}\b(cached|stored|held|holds?)\b`)

// rootlessCachePerHomeMarkers say where the payload IS fetched. Without this
// the document could deny caching and still leave the reader thinking a cache
// hit is enough.
var rootlessCachePerHomeMarkers = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\beach agent\b`),
	regexp.MustCompile(`(?i)\bper-agent\b`),
	regexp.MustCompile(`(?i)\bper home\b`),
	regexp.MustCompile(`(?i)\beach home\b`),
	regexp.MustCompile(`(?i)\bagent'?s? home\b`),
	regexp.MustCompile(`(?i)\bagent home\b`),
}

// rootlessCacheBigArtifact matches a multi-MB size — the shape of the
// overclaim. KB sizes are deliberately out of scope: "~9 KB setup script" is
// the honest number this rule exists to protect.
var rootlessCacheBigArtifact = regexp.MustCompile(`(?i)~?\s?\d{1,4}(?:\.\d+)?\s?(?:MB|GB|GiB|MiB)\b`)

// rootlessCacheWord matches the words that turn a size into a caching claim.
var rootlessCacheWord = regexp.MustCompile(`(?i)\bcach(?:e|es|ed|ing)\b|\bcached\b`)

// rootlessCacheSentences splits a normalized block into sentence-ish units so
// the overclaim rule can be scoped to one claim. Go's regexp has no
// lookbehind, so splitting is done by hand on ". ".
func rootlessCacheSentences(block string) []string {
	var out []string
	for _, part := range strings.Split(block, ". ") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// rootlessCacheMatchesAny reports whether s matches any of res. Local to this
// rule (rather than reusing the disk-cap rule's helper) so the two pins stay
// independent: editing one must not silently change the other.
func rootlessCacheMatchesAny(s string, res []*regexp.Regexp) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// rootlessCacheDocBlock returns the documentation block for the knob: the
// anchor line, extended over the adjacent comment lines that carry the
// explanation. Comment lines only, so a config block does not swallow the next
// unrelated section past a blank line.
func rootlessCacheDocBlock(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	anchor := -1
	for i, line := range lines {
		for _, a := range rootlessCacheDocAnchors {
			if strings.Contains(line, a) {
				anchor = i
				break
			}
		}
		if anchor >= 0 {
			break
		}
	}
	if anchor < 0 {
		return "", false
	}
	isComment := func(i int) bool {
		return i >= 0 && i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "#")
	}
	start, end := anchor, anchor
	for isComment(start - 1) {
		start--
	}
	for isComment(end + 1) {
		end++
	}
	var b strings.Builder
	for _, line := range lines[start : end+1] {
		t := strings.TrimSpace(line)
		t = strings.TrimPrefix(t, "#")
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		b.WriteString(t)
		b.WriteString(" ")
	}
	return strings.TrimSpace(b.String()), true
}

// rootlessCacheFindings evaluates the GAP-091 docs rule over one document.
// Pure, so the falsification test can drive it with the pre-fix wording.
func rootlessCacheFindings(path, content string) []string {
	block, ok := rootlessCacheDocBlock(content)
	if !ok {
		return []string{path + ": documents no rootless-installer cache knob (none of " +
			strings.Join(rootlessCacheDocAnchors, ", ") + " appears)"}
	}

	var findings []string
	add := func(format string) { findings = append(findings, path+": "+format) }

	// (1) What the cache holds: the setup script.
	if !rootlessCacheContentMarker.MatchString(block) {
		add("does not state that the cache holds the rootless SETUP SCRIPT")
	}

	// (2) The payload must be named, denied as cached here, and placed where
	// it is actually fetched.
	if !rootlessCacheMatchesAny(block, rootlessCachePayloadNames) {
		add("never names the docker-ce-rootless-extras payload the cache does NOT hold")
	}
	if !rootlessCacheDenial.MatchString(block) {
		add("does not deny that the payload is cached here (no \"not cached\"/\"does not hold\" statement)")
	}
	if !rootlessCacheMatchesAny(block, rootlessCachePerHomeMarkers) {
		add("does not say the payload is fetched inside each agent home")
	}

	// (3) The overclaim itself: no sentence may advertise a multi-MB artifact
	// as cached without denying it. This is the pre-fix wording
	// ("a valid cached artifact (~93MB from get.docker.com)").
	for _, s := range rootlessCacheSentences(block) {
		if rootlessCacheBigArtifact.MatchString(s) &&
			rootlessCacheWord.MatchString(s) &&
			!rootlessCacheDenial.MatchString(s) {
			add("claims a multi-MB artifact is cached without denying it: " + s)
		}
	}

	return findings
}

// TestRootlessCacheDocRuleRejectsThePreFixWording is the RED proof: the exact
// wording the row quotes (config.example.yaml and README.md at HEAD~) is
// flagged, and the corrected wording is accepted. Without this the rule could
// be vacuously green.
func TestRootlessCacheDocRuleRejectsThePreFixWording(t *testing.T) {
	// Verbatim pre-fix blocks from this repository.
	preFix := []struct {
		name string
		text string
	}{
		{
			name: "config.example.yaml",
			text: "  # Host-level cache for downloaded rootless Docker installers (GAP-091).\n" +
				"  # With a valid cached artifact (~93MB from get.docker.com) a fresh-agent\n" +
				"  # spawn skips the network download entirely — retries no longer depend on\n" +
				"  # get.docker.com throughput. The daemon (root) creates the directory as\n" +
				"  # 0755 and stores one installer file in it. Empty string (\"\") disables\n" +
				"  # the cache and restores the legacy uncached download path.\n" +
				"  # rootless_installer_cache_dir: /var/cache/bunker/rootless-installer  # default\n",
		},
		{
			name: "README.md",
			text: "  # Rootless installer cache (GAP-091): downloaded rootless Docker installers\n" +
				"  # (~93MB) are cached here so fresh-agent spawns skip the get.docker.com\n" +
				"  # download. Empty string disables the cache (legacy uncached path). Env\n" +
				"  # override: BUNKER_ROOTLESS_INSTALLER_CACHE_DIR.\n" +
				"  rootless_installer_cache_dir: /var/cache/bunker/rootless-installer\n",
		},
	}
	for _, tc := range preFix {
		got := rootlessCacheFindings(tc.name, tc.text)
		if len(got) == 0 {
			t.Errorf("rule accepted the pre-fix %s block — the rule is vacuous", tc.name)
			continue
		}
		var sawOverclaim bool
		for _, f := range got {
			if strings.Contains(f, "claims a multi-MB artifact is cached") {
				sawOverclaim = true
			}
		}
		if !sawOverclaim {
			t.Errorf("%s pre-fix block was flagged but not for the overclaim itself: %v", tc.name, got)
		}
	}

	// The corrected wording is accepted, including the table-row and bullet
	// shapes of the spec/skill surfaces.
	corrected := []struct {
		name string
		text string
	}{
		{
			name: "config.example.yaml",
			text: "  # Host-level cache for the rootless Docker setup script only (GAP-091).\n" +
				"  # The cache holds ONE file: the ~9 KB `get.docker.com/rootless` setup\n" +
				"  # script. It does NOT hold the ~90 MB `docker-ce-rootless-extras`\n" +
				"  # payload the script downloads: the installer fetches that payload inside\n" +
				"  # EACH agent home, so a fresh-agent spawn still pays that download.\n" +
				"  # rootless_installer_cache_dir: /var/cache/bunker/rootless-installer  # default\n",
		},
		{
			name: "README.md",
			text: "  # Rootless installer cache (GAP-091): host-level cache of the rootless\n" +
				"  # Docker SETUP SCRIPT only — one ~9 KB file from get.docker.com/rootless.\n" +
				"  # The ~90 MB docker-ce-rootless-extras payload that script downloads is\n" +
				"  # NOT cached here: the installer fetches it inside each agent's home.\n" +
				"  rootless_installer_cache_dir: /var/cache/bunker/rootless-installer\n",
		},
		{
			name: "specs/configuration.md",
			text: "| `agent.rootless_installer_cache_dir` | `/var/cache/bunker/rootless-installer` | Host-level cache of the rootless Docker **setup script** only: the ~90 MB `docker-ce-rootless-extras` payload is **not** cached here — the installer fetches it inside each agent home. |\n",
		},
		{
			name: "internal/config/SKILL.md",
			text: "- `RootlessInstallerCacheDir` (`rootless_installer_cache_dir`, GAP-091) — host-level cache for the rootless Docker **setup script** only: not the ~90 MB `docker-ce-rootless-extras` payload, which the installer downloads inside each agent home (so it is not cached here).\n",
		},
	}
	for _, tc := range corrected {
		if got := rootlessCacheFindings(tc.name, tc.text); len(got) != 0 {
			t.Errorf("rule rejected the corrected %s wording: %v", tc.name, got)
		}
	}
}

// TestRootlessCacheDocRuleScopeBoundary pins what the rule does and does not
// reach, so its blind spots are a recorded decision.
func TestRootlessCacheDocRuleScopeBoundary(t *testing.T) {
	// In scope: a sentence that advertises a multi-MB artifact as cached.
	overclaim := "  # Host-level cache for the rootless Docker installers (GAP-091): a ~93MB\n" +
		"  # artifact is cached here so spawns skip the download.\n" +
		"  # rootless_installer_cache_dir: /var/cache/bunker/rootless-installer\n"
	if got := rootlessCacheFindings("x.yaml", overclaim); len(got) == 0 {
		t.Errorf("in-scope overclaim was NOT flagged: %q", overclaim)
	}

	// Accepted: a document that never mentions the knob is out of the rule's
	// reach by construction — checked separately by the scope-exists test, not
	// by findings (a missing anchor IS a finding).
	if got := rootlessCacheFindings("x.yaml", "# nothing about caches here\n"); len(got) == 0 {
		t.Errorf("a document with no anchor should be a finding (it cannot document the knob)")
	}
}

// TestRealTreeRootlessCacheDocsAreHonest is the live-tree check: every scoped
// surface must describe the cache as holding the setup script and must deny
// that the ~90 MB payload is cached.
func TestRealTreeRootlessCacheDocsAreHonest(t *testing.T) {
	root := repoRoot(t)

	var findings []string
	for _, rel := range rootlessCacheDocScope {
		content, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("GAP-091 docs scope names %s, which cannot be read: %v", rel, err)
			continue
		}
		findings = append(findings, rootlessCacheFindings(rel, string(content))...)
	}
	for _, f := range findings {
		t.Errorf("REV-BUNKER-004: %s", f)
	}
}

// TestRealTreeRootlessCacheDocsScopeExists keeps the scope list honest: a
// renamed or deleted surface fails here instead of silently shrinking the
// rule's reach, and at least the two surfaces the row names must carry the
// knob or the rule checks nothing.
func TestRealTreeRootlessCacheDocsScopeExists(t *testing.T) {
	root := repoRoot(t)

	var carriers int
	for _, rel := range rootlessCacheDocScope {
		content, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("GAP-091 docs scope lists %s, which does not exist: %v", rel, err)
			continue
		}
		if _, ok := rootlessCacheDocBlock(string(content)); ok {
			carriers++
		}
	}
	if carriers < 2 {
		t.Errorf("only %d scoped document(s) mention the rootless-installer cache knob — the GAP-091 docs rule is vacuous", carriers)
	}
}
