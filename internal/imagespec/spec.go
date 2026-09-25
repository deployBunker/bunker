// Package imagespec implements the secure, declarative per-agent image
// specification feature (GAP-064).
//
// An image spec is a small JSON document that customizes the per-agent rootless
// image with PACKAGE-ADD DIRECTIVES ONLY:
//
//	{
//	  "base": "docker.io/library/ubuntu:24.04",
//	  "packages": [
//	    {"manager": "apt", "packages": ["jq", "curl"]},
//	    {"manager": "go",  "packages": ["golang.org/x/tools/gopls@v0.17.0"]},
//	    {"manager": "npm", "packages": ["typescript@5.6.3"]},
//	    {"manager": "pip", "packages": ["requests>=2.31,<3"]}
//	  ]
//	}
//
// These forms are also the validated LIFECYCLE HOOK data for GAP-064:
// an agent image is customized exactly once at spawn (the install hook) and
// torn down at destroy; there is no arbitrary command, entrypoint, or
// restart-hook surface. Everything else about the lifecycle is the existing
// agent bootstrap (user → rootless dockerd → agent container).
//
// Security model — the parser REJECTS anything that is not a package add:
//
//   - base-image changes beyond naming an allowed base (no FROM rewrites other
//     than the single declared base, no scratch/local images)
//   - USER, EXPOSE, VOLUME, ENV, LABEL, ENTRYPOINT, CMD, WORKDIR, SHELL,
//     ONBUILD, STOPSIGNAL, HEALTHCHECK, MAINTAINER and any other Dockerfile
//     instruction (the input is NOT Dockerfile text at all — arbitrary
//     Dockerfile text is never accepted)
//   - curl|sh / wget|sh and any shell chaining, substitution, or redirection
//     characters in any token: whitespace, ; & | $ ` \ ' " and newlines are
//     refused for EVERY manager (see tokenDangerous), and a `<` or `>` must
//     carry a version-comparator shape (see tokenPolicy)
//   - mounts, devices, sockets, volumes, and other runtime escapes (the spec
//     cannot influence docker run flags; the builder never adds any)
//   - privileged / host-namespace flags (not representable in the grammar)
//   - non-package network or exfiltration commands (only the constrained
//     package-manager invocations declared in managerDefs are ever rendered)
//
// Defence in depth — every rendered package token is SINGLE-QUOTED
// (GAP-148). Dockerfile() emits `RUN pip install --no-cache-dir
// 'requests>=2.31,<3'`, not a bare token, so the version-grammar characters a
// manager declares (pip's `, < > [ ]`, cargo's `^ ~`) reach pip as one
// argument and are inert to the shell that parses the RUN line. The quoting
// cannot be escaped out of: `'` is refused by the shared grammar for every
// manager, so no token can close the quote. A manager therefore declares the
// extra characters its own version grammar needs, and the shared
// shell-dangerous set stays refused for all of them.
//
// Hard bounds: spec size, directive count, packages per directive, token
// length, and a build timeout enforced by the builder.
package imagespec

import (
	"fmt"
	"strings"
	"time"
)

// Bounds on untrusted input. They are deliberately small: a package list is a
// convenience, not a build farm.
const (
	// MaxSpecBytes is the maximum serialized spec size (16 KiB).
	MaxSpecBytes = 16 * 1024
	// MaxDirectives is the maximum number of package-add directives per spec.
	MaxDirectives = 16
	// MaxPackagesPerDirective is the maximum packages per directive.
	MaxPackagesPerDirective = 16
	// MaxTokenBytes is the maximum length of a single package name or version.
	MaxTokenBytes = 256
	// DefaultBuildTimeout is the default rootless image build timeout.
	DefaultBuildTimeout = 20 * time.Minute
)

// PackageManager names the constrained package managers an image spec may
// invoke. The string values are the wire/JSON spelling.
type PackageManager string

const (
	ManagerAPT      PackageManager = "apt"
	ManagerGo       PackageManager = "go"
	ManagerNPM      PackageManager = "npm"
	ManagerPip      PackageManager = "pip"
	ManagerPipx     PackageManager = "pipx"
	ManagerCargo    PackageManager = "cargo"
	ManagerGem      PackageManager = "gem"
	ManagerComposer PackageManager = "composer"
)

// tokenDangerous is the set of bytes that stay FORBIDDEN in a package token
// for EVERY manager, whatever a row declares in TokenExtra: whitespace (space,
// tab, CR, LF — so one token can never split into several shell words or start
// a second Dockerfile line), the shell's separators and operators (`;`, `&`,
// `|`), and substitution/quoting characters (`$`, backtick, `\`, `'`, `"`).
//
// `'` is in this set for a second, load-bearing reason: it is what makes the
// single-quoting in the renderers unescapable (see writeQuoted).
//
// `<` and `>` are deliberately NOT here — pip's `>=2.31,<3` and gem's `~>3.0`
// are real version grammar — but they are not free either: tokenPolicy
// additionally requires a comparator shape, so `x>/etc/passwd` is refused for
// every manager regardless of quoting.
const tokenDangerous = " \t\r\n;&|$`\\'\""

// tokenSharedDisplay is the human-readable spelling of the shared character
// class every row starts from: letters, digits, and the punctuation package
// names and versions genuinely use (`. + - _ : / @ =`). It is DISPLAY ONLY —
// it contains spaces for readability and must never be used as an accept set;
// the accept set is the explicit switch in tokenPolicy. It is reused verbatim
// in the rejection message so the message cannot drift from the grammar.
//
// `<` and `>` are deliberately absent: they are per-row grammar (see
// ManagerDef.TokenExtra), not shared, so apt/go/npm cannot acquire them.
const tokenSharedDisplay = ". + - _ : / @ ="

// quotedExtra renders a row's TokenExtra for an error message: the chars are
// spaced and quoted so a reader sees exactly which characters this manager
// accepts on top of the shared class (e.g. `, < > [ ] ! ~`), and the empty
// string for the rows that need no extension.
func quotedExtra(extra string) string {
	if extra == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(" or ")
	for i := 0; i < len(extra); i++ {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(string(extra[i]))
	}
	return b.String()
}

// dangerousByte reports whether c is in the never-allowed-across-managers set.
func dangerousByte(c byte) bool {
	return strings.IndexByte(tokenDangerous, c) >= 0
}

// versionByte reports whether c can appear in a version run following a
// comparator: version characters only (digits/letters and . + - _). Notably
// NOT `/`, `*`, or any separator — see comparatorWellFormed.
func versionByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '.' || c == '+' || c == '-' || c == '_'
}

// comparatorWellFormed reports whether the `<` or `>` at tok[i] opens a real
// version constraint and nothing else. The accepted shapes are exactly:
//
//	>=1.2   <=1.2    >1.2    <1.2
//
// i.e. an optional `=`, then a version run that MUST START WITH A DIGIT (every
// real parser probed refuses a non-digit version — cargo: "unexpected character
// 'n' while parsing major version number"; PEP 440 and Composer agree), then the
// version run, followed by end-of-token, `,` (the next AND-clause:
// `requests>=2.31,<3`) or `]` (a pip extras group boundary).
//
// Everything else is refused, which is what turns the redirection payloads into
// hard rejections rather than merely-quoted text: `x>/etc/passwd` (the run would
// be empty), `x>/etc/shadow`, `pkg>=` and `jq>-` (no digit), `pkg>1>2` (a second
// `>` where only a separator may follow), `pkg>>x`, `pkg<>x`.
func comparatorWellFormed(tok string, i int) bool {
	j := i + 1
	if j < len(tok) && tok[j] == '=' {
		j++
	}
	start := j
	for j < len(tok) && versionByte(tok[j]) {
		j++
	}
	if j == start || tok[start] < '0' || tok[start] > '9' {
		return false
	}
	if j == len(tok) {
		return true
	}
	return tok[j] == ',' || tok[j] == ']'
}

// ManagerDef defines one constrained package manager. Everything the package
// needs to know about a manager lives in its row — validity, token policy,
// and Dockerfile rendering all derive from the registry — so adding a manager
// is one managerDefs row plus its PackageManager constant (previously four
// scattered edits: the const + Valid switch, validateToken, Dockerfile's
// render switch, and the tests).
type ManagerDef struct {
	// Name is the wire/JSON spelling; it must equal the PackageManager
	// constant the row is registered under.
	Name PackageManager
	// TokenExtra lists characters allowed in a package token BEYOND the
	// shared grammar (letters, digits, and . + - _ : / @ =). It exists so a
	// manager can express its OWN version syntax without widening the
	// grammar for everyone: pip declares ",<>[]!~" for "requests>=2.31,<3"
	// and "flask[async]", cargo declares ",<>^~" for "ripgrep@^14". Empty
	// for managers whose syntax is already in the shared set (go, npm).
	//
	// A row can only ever ADD to the shared grammar, never remove from the
	// dangerous set: register panics if TokenExtra names a
	// tokenDangerous byte, and tokenPolicy refuses those bytes no matter
	// what a row asks for.
	TokenExtra string
	// TokenDeny lists substrings no token may contain, applied after the
	// grammar. apt denies "/" so absolute paths like /var/run/docker.sock or
	// --mount=type=bind,source=/etc,target=/etc cannot ride through as
	// "packages" (a mounted path is not a package name).
	TokenDeny []string
	// Render appends the manager's install lines for pkgs to b. It only ever
	// receives tokens that passed the row's token policy, so the rendered
	// text cannot contain metacharacters by construction; it also
	// single-quotes every token (writeQuoted) so the version-grammar
	// characters a row allows are inert to the shell.
	Render func(b *strings.Builder, pkgs []string)
	// Probe applies the row's full token policy (shared grammar +
	// TokenExtra, minus TokenDeny, plus the shared comparator/dangerous
	// guards) to one candidate token: nil means the token may ride the
	// wire. It is derived by register — never set in table rows — so each
	// row's policy has exactly one source of truth. validateToken delegates
	// to it; tests call it directly.
	Probe func(tok string) error
}

// managerDefs is the manager registry: one row per supported manager, in
// wire-documentation order. This table is the single place a new manager is
// added; nothing else in the package is per-manager.
var managerDefs = []ManagerDef{
	{
		Name:      ManagerAPT,
		TokenDeny: []string{"/"},
		Render:    renderAPT,
	},
	{
		Name:   ManagerGo,
		Render: renderGo,
	},
	{
		Name:   ManagerNPM,
		Render: renderNPM,
	},
	{
		// pip's requirement grammar: comparison operators (>=, <=, !=, ~=,
		// ==), comma-separated AND clauses, and [extras] groups. `*` is
		// deliberately absent even though pip accepts wildcard versions:
		// a glob is a shell-dangerous shape and stays refused everywhere.
		Name:       ManagerPip,
		TokenExtra: ",<>[]!~",
		Render:     renderPip,
	},
	{
		// pipx installs PyPI applications into isolated venvs and inherits
		// pip's requirement grammar for them (PEP 440 specifiers,
		// comma-separated AND clauses, [extras] groups), so it declares the
		// same TokenExtra as pip; `*` stays refused for the same reason.
		Name:       ManagerPipx,
		TokenExtra: ",<>[]!~",
		Render:     renderPipx,
	},
	{
		// cargo's version requirement grammar: comparison operators plus
		// the caret/tilde shorthands, comma-separated. Used as the real
		// CLI form `cargo install crate@^1.2`.
		Name:       ManagerCargo,
		TokenExtra: ",<>^~",
		Render:     renderCargo,
	},
	{
		// Gem::Requirement: `~>` pessimistic, comparison operators, `!=`,
		// and comma-separated requirements (the form `gem install -v`
		// accepts). Carried on the token as name@requirement.
		Name:       ManagerGem,
		TokenExtra: ",<>~!",
		Render:     renderGem,
	},
	{
		// Composer's constraint grammar: `^`, `~`, comparison operators,
		// comma-separated AND. The token is already the CLI form
		// vendor/package:constraint, which is why `/` and `:` matter here.
		Name:       ManagerComposer,
		TokenExtra: ",<>^~!",
		Render:     renderComposer,
	},
}

// managerByName indexes managerDefs for O(1) lookup by wire name.
var managerByName = map[PackageManager]*ManagerDef{}

func init() {
	for i := range managerDefs {
		register(&managerDefs[i])
	}
}

// register indexes one registry row: it derives the row's Probe from its
// token policy fields and inserts it into the lookup map. It refuses a def
// without a render func, an empty name, a duplicate name, or a TokenExtra
// that names a tokenDangerous byte, so a manager cannot exist
// half-registered: every registered manager has both a render func and a
// token policy (the derived Probe), and no row can widen the grammar with a
// shell metacharacter.
func register(d *ManagerDef) {
	if d.Name == "" {
		panic("imagespec: manager registered with an empty name")
	}
	if d.Render == nil {
		panic(fmt.Sprintf("imagespec: manager %q registered without a render func", d.Name))
	}
	for i := 0; i < len(d.TokenExtra); i++ {
		if c := d.TokenExtra[i]; dangerousByte(c) {
			panic(fmt.Sprintf("imagespec: manager %q declares dangerous token character %q in TokenExtra — the set %q is forbidden for every manager",
				d.Name, string(c), tokenDangerous))
		}
	}
	if _, dup := managerByName[d.Name]; dup {
		panic(fmt.Sprintf("imagespec: manager %q registered twice", d.Name))
	}
	d.Probe = tokenPolicy(d.Name, d.TokenExtra, d.TokenDeny...)
	managerByName[d.Name] = d
}

// tokenPolicy builds a Probe from the shared token grammar (letters, digits,
// and . + - _ : / @ =) plus a row's extra allowed characters and denied
// substrings. Errors name the manager so a rejection is attributable to its
// directive.
//
// The order is deliberate and is the security contract:
//
//  1. tokenDangerous bytes are refused FIRST, unconditionally — before any
//     row's TokenExtra is consulted, so no future row can re-admit
//     whitespace, ; & | $ ` \ ' " or a newline.
//  2. The shared class and the row's TokenExtra decide the ordinary
//     characters. `<` and `>` are NOT in the shared class: they are admitted
//     only by a row that declares them (pip, pipx, cargo, gem, composer),
//     because
//     apt/go/npm's accepted set is a frozen contract (GAP-148 criterion 6)
//     and neither manager's version grammar uses a comparator.
//  3. A `<` or `>` that IS declared must carry a comparator shape: it opens
//     a version constraint (pip's `>=2.31,<3`, gem's `~>3.0`), never a shell
//     redirection (`x>/etc/passwd`, `x</etc/shadow`). Quoting would make a
//     bare `>` inert anyway; refusing the redirection SHAPE means the
//     dangerous payload is rejected outright and not merely defanged.
//  4. The row's TokenDeny substrings apply last.
func tokenPolicy(m PackageManager, extra string, deny ...string) func(string) error {
	hasComparator := strings.IndexByte(extra, '<') >= 0 || strings.IndexByte(extra, '>') >= 0
	return func(tok string) error {
		for i := 0; i < len(tok); i++ {
			c := tok[i]
			if dangerousByte(c) {
				return fmt.Errorf("%s package token %q: illegal character %q at position %d — whitespace and the shell metacharacters %s are never allowed in a package token",
					string(m), tok, string(c), i+1, tokenDangerous)
			}
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			case strings.IndexByte(extra, c) >= 0:
			case c == '.' || c == '+' || c == '-' || c == '_' || c == ':' || c == '/' || c == '@' || c == '=':
			default:
				return fmt.Errorf("%s package token %q: illegal character %q at position %d — allowed are letters, digits, %s%s",
					string(m), tok, string(c), i+1, tokenSharedDisplay, quotedExtra(extra))
			}
		}
		if hasComparator {
			for i := 0; i < len(tok); i++ {
				if c := tok[i]; c == '<' || c == '>' {
					if !comparatorWellFormed(tok, i) {
						return fmt.Errorf("%s package token %q: the %q at position %d must open a version constraint (e.g. >=2.31,<3), never a redirection",
							string(m), tok, string(c), i+1)
					}
				}
			}
		}
		for _, bad := range deny {
			if strings.Contains(tok, bad) {
				return fmt.Errorf("%s package names may not contain %q", string(m), bad)
			}
		}
		return nil
	}
}

// Renderers: one per registry row. Each receives policy-validated tokens only;
// each wraps every token with writeQuoted, so the version-grammar characters
// the row allows are inert to the shell that parses the RUN line.

// writeQuoted appends tok wrapped in SINGLE quotes. The quoting is the reason
// a manager may declare characters like `<`, `>`, `,`, `^`, `~` (pip, cargo,
// gem, composer): the shell hands the whole quoted word to the package manager
// as ONE argument, unexpanded and uninterpreted.
//
// The invariant that makes this safe: `'` is in tokenDangerous, so no token
// that reaches a renderer can contain a single quote and therefore cannot
// close the quoting early. A token is attacker-shaped input, never
// attacker-chosen text.
func writeQuoted(b *strings.Builder, tok string) {
	b.WriteByte('\'')
	b.WriteString(tok)
	b.WriteByte('\'')
}

func renderAPT(b *strings.Builder, pkgs []string) {
	b.WriteString("RUN apt-get update && apt-get install -y --no-install-recommends")
	for _, p := range pkgs {
		b.WriteString(" ")
		writeQuoted(b, p)
	}
	b.WriteString(" && rm -rf /var/lib/apt/lists/*\n")
}

func renderGo(b *strings.Builder, pkgs []string) {
	for _, p := range pkgs {
		b.WriteString("RUN go install ")
		writeQuoted(b, p)
		b.WriteString("\n")
	}
}

func renderNPM(b *strings.Builder, pkgs []string) {
	b.WriteString("RUN npm install -g")
	for _, p := range pkgs {
		b.WriteString(" ")
		writeQuoted(b, p)
	}
	b.WriteString("\n")
}

func renderPip(b *strings.Builder, pkgs []string) {
	b.WriteString("RUN pip install --no-cache-dir")
	for _, p := range pkgs {
		b.WriteString(" ")
		writeQuoted(b, p)
	}
	b.WriteString("\n")
}

// renderPipx mirrors renderPip: pipx's install positional takes the same
// PEP 440 requirement specifiers pip does (`pipx install 'black>=24.0,<25'`).
// Every token is single-quoted, so the version-grammar characters are inert
// to the shell that parses the RUN line.
func renderPipx(b *strings.Builder, pkgs []string) {
	b.WriteString("RUN pipx install")
	for _, p := range pkgs {
		b.WriteString(" ")
		writeQuoted(b, p)
	}
	b.WriteString("\n")
}

func renderCargo(b *strings.Builder, pkgs []string) {
	b.WriteString("RUN cargo install")
	for _, p := range pkgs {
		b.WriteString(" ")
		writeQuoted(b, p)
	}
	b.WriteString("\n")
}

// renderGem emits one line per package: gem takes the requirement as a -v
// ARGUMENT (`gem install -v '>=13,<14' rake`), not as part of a name@version
// positional, so a directive's per-package requirements cannot share one line.
// The token's wire form is name@requirement; a token without "@" is a bare
// name. Both halves are quoted, and neither can contain a quote.
//
// The split is deliberately conservative — exactly one "@", with a non-empty
// name on the left AND a non-empty requirement on the right. Anything else (no
// "@", a leading "@", a trailing "@", two "@") is not the name@requirement
// shape, so the WHOLE token is rendered as the name: gem then rejects it as an
// unknown gem. Interpreting a malformed token's tail as a requirement — or
// dropping an unmatched "@" — would either invent grammar the token does not
// have or silently rewrite the operator's input.
func renderGem(b *strings.Builder, pkgs []string) {
	for _, p := range pkgs {
		name, req := p, ""
		if i := strings.IndexByte(p, '@'); i > 0 && i < len(p)-1 && strings.Count(p, "@") == 1 {
			name, req = p[:i], p[i+1:]
		}
		b.WriteString("RUN gem install --no-document")
		if req != "" {
			b.WriteString(" -v ")
			writeQuoted(b, req)
		}
		b.WriteString(" ")
		writeQuoted(b, name)
		b.WriteString("\n")
	}
}

func renderComposer(b *strings.Builder, pkgs []string) {
	b.WriteString("RUN composer global require --no-interaction")
	for _, p := range pkgs {
		b.WriteString(" ")
		writeQuoted(b, p)
	}
	b.WriteString("\n")
}

// Valid reports whether m is a registered manager.
func (m PackageManager) Valid() bool {
	return managerByName[m] != nil
}

// Def returns the registry row for m, or nil if m is not registered.
func (m PackageManager) Def() *ManagerDef {
	return managerByName[m]
}

// PackageAdd is one constrained package-install directive.
type PackageAdd struct {
	Manager PackageManager
	// Packages are package names, optionally pinned with a manager-specific
	// version suffix (apt: =version, go: @version, npm: @version,
	// pip: a PEP 440 specifier, cargo: crate@requirement, gem:
	// name@requirement, composer: vendor/package:constraint).
	Packages []string
}

// PackageAddSpec is the JSON wire form of a package-add directive (also the
// shared validation shape proto directives map onto — see fromWire).
type PackageAddSpec struct {
	Manager  PackageManager `json:"manager"`
	Packages []string       `json:"packages"`
}

// Spec is a validated per-agent image specification.
//
// The zero value is not valid; construct specs via Parse or Validate.
type Spec struct {
	Base string
	// Packages are the package-add directives, in declaration order, with
	// at most one directive per PackageManager (repeats are rejected).
	Packages []PackageAdd
}

// Lifecycle hook placement (documentation contract, GAP-064):
//
//   - spawn install hook: the spec's package adds are rendered into the
//     per-agent image Dockerfile and built once through the agent's rootless
//     socket (see Builder).
//   - destroy teardown hook: implemented in internal/agent
//     (cleanupAgentContainers) — containers built from the customized image
//     are stopped/removed through the SAME per-agent socket before the
//     rootless dockerd is stopped. The spec carries no hook data for this:
//     teardown is structural, so it cannot be customized into an escape
//     hatch.
