package imagespec

import (
	"strings"
	"testing"
)

// TestDockerfile_Golden pins the EXACT Dockerfile bytes for every registered
// manager. A registry row that changes rendering for a shipped manager fails
// here first; update the golden bytes deliberately, never silently.
//
// HISTORY
//   - The apt/go/npm expectations were captured from the pre-registry
//     implementation at d1b0654 (hardcoded switch in parse.go Dockerfile) and
//     proved the GAP-147 ManagerDef refactor was byte-identical.
//   - GAP-148 CHANGED those bytes on purpose: every package token is now
//     SINGLE-QUOTED in the rendered RUN line (the version-grammar characters
//     pip/cargo/gem/composer need are inert to the shell that parses the RUN
//     line), and four managers were added. The apt/go/npm changes are the
//     quoting and NOTHING else — compare each expectation below against the
//     unquoted pre-GAP-148 form and the only difference is the surrounding
//     quotes.
//
// The test stays exact-byte (never a substring check): quoting is a security
// property of the emitted line, so the line itself is the thing under test.
func TestDockerfile_Golden(t *testing.T) {
	tests := []struct {
		name string
		spec *Spec
		want string
	}{
		{
			name: "base only",
			spec: &Spec{Base: DefaultBaseImage},
			want: "FROM docker.io/library/ubuntu:24.04\n",
		},
		{
			// pre-GAP-148: ...--no-install-recommends jq curl=8.5.0-2ubuntu10 && rm...
			name: "apt multi",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerAPT, Packages: []string{"jq", "curl=8.5.0-2ubuntu10"}}}},
			want: "FROM docker.io/library/ubuntu:24.04\nRUN apt-get update && apt-get install -y --no-install-recommends 'jq' 'curl=8.5.0-2ubuntu10' && rm -rf /var/lib/apt/lists/*\n",
		},
		{
			// pre-GAP-148: RUN go install golang.org/x/tools/gopls@v0.17.0
			name: "go multi",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@v0.17.0", "honnef.co/go/tools/cmd/staticcheck@v0.5.1"}}}},
			want: "FROM docker.io/library/ubuntu:24.04\nRUN go install 'golang.org/x/tools/gopls@v0.17.0'\nRUN go install 'honnef.co/go/tools/cmd/staticcheck@v0.5.1'\n",
		},
		{
			// pre-GAP-148: RUN npm install -g typescript@5.6.3 @types/node@20.14.0
			name: "npm multi",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerNPM, Packages: []string{"typescript@5.6.3", "@types/node@20.14.0"}}}},
			want: "FROM docker.io/library/ubuntu:24.04\nRUN npm install -g 'typescript@5.6.3' '@types/node@20.14.0'\n",
		},
		{
			name: "pip version specifiers and extras",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerPip, Packages: []string{"requests>=2.31,<3", "flask[async]>=3.0"}}}},
			want: "FROM docker.io/library/ubuntu:24.04\nRUN pip install --no-cache-dir 'requests>=2.31,<3' 'flask[async]>=3.0'\n",
		},
		{
			name: "cargo requirements",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerCargo, Packages: []string{"ripgrep@^14.1", "cargo-edit@~0.12"}}}},
			want: "FROM docker.io/library/ubuntu:24.04\nRUN cargo install 'ripgrep@^14.1' 'cargo-edit@~0.12'\n",
		},
		{
			// gem takes the requirement as a -v ARGUMENT, so each package
			// gets its own line; the name and the requirement are separately
			// quoted.
			name: "gem requirements per package",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerGem, Packages: []string{"rake@~>13.0", "puma"}}}},
			want: "FROM docker.io/library/ubuntu:24.04\nRUN gem install --no-document -v '~>13.0' 'rake'\nRUN gem install --no-document 'puma'\n",
		},
		{
			name: "composer constraints",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerComposer, Packages: []string{"symfony/console:^7.0", "monolog/monolog:>=3.0,<4.0"}}}},
			want: "FROM docker.io/library/ubuntu:24.04\nRUN composer global require --no-interaction 'symfony/console:^7.0' 'monolog/monolog:>=3.0,<4.0'\n",
		},
		{
			name: "all managers in declaration order",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{
				{Manager: ManagerAPT, Packages: []string{"jq", "curl"}},
				{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@v0.17.0"}},
				{Manager: ManagerNPM, Packages: []string{"typescript@5.6.3"}},
				{Manager: ManagerPip, Packages: []string{"requests>=2.31,<3"}},
				{Manager: ManagerCargo, Packages: []string{"ripgrep@^14.1"}},
				{Manager: ManagerGem, Packages: []string{"rake@~>13.0"}},
				{Manager: ManagerComposer, Packages: []string{"symfony/console:^7.0"}},
			}},
			want: "FROM docker.io/library/ubuntu:24.04\n" +
				"RUN apt-get update && apt-get install -y --no-install-recommends 'jq' 'curl' && rm -rf /var/lib/apt/lists/*\n" +
				"RUN go install 'golang.org/x/tools/gopls@v0.17.0'\n" +
				"RUN npm install -g 'typescript@5.6.3'\n" +
				"RUN pip install --no-cache-dir 'requests>=2.31,<3'\n" +
				"RUN cargo install 'ripgrep@^14.1'\n" +
				"RUN gem install --no-document -v '~>13.0' 'rake'\n" +
				"RUN composer global require --no-interaction 'symfony/console:^7.0'\n",
		},
		{
			// Unchanged from pre-GAP-148: an empty list renders no token, so
			// there is nothing to quote. (fromWire drops empty lists; this is
			// the degenerate hand-built shape the pre-registry switch had.)
			name: "empty package lists degenerate",
			spec: &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerAPT, Packages: []string{}}, {Manager: ManagerGo, Packages: []string{}}, {Manager: ManagerNPM, Packages: []string{}}}},
			want: "FROM docker.io/library/ubuntu:24.04\nRUN apt-get update && apt-get install -y --no-install-recommends && rm -rf /var/lib/apt/lists/*\nRUN npm install -g\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.spec.Dockerfile()
			if got != tt.want {
				t.Errorf("Dockerfile() byte drift for %q:\ngot  %q\nwant %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestDockerfile_GoldenEveryTokenIsQuoted is the machine-checked half of the
// golden test: it does not trust the table above to be complete, it proves the
// INVARIANT (criterion 1) over every registered manager — in the rendered
// output of a one-package spec, the token appears exactly once and always
// wrapped in single quotes. A new row that forgets writeQuoted fails here even
// if nobody adds a golden row for it.
func TestDockerfile_GoldenEveryTokenIsQuoted(t *testing.T) {
	for i := range managerDefs {
		def := &managerDefs[i]
		// Build a token the row's own policy accepts, so the spec is the
		// wire-real shape (and a bare name, which every row must accept).
		token := pickAcceptedName(t, def)
		spec := &Spec{
			Base:     DefaultBaseImage,
			Packages: []PackageAdd{{Manager: def.Name, Packages: []string{token}}},
		}
		got := spec.Dockerfile()
		wantQuoted := "'" + token + "'"
		if !strings.Contains(got, wantQuoted) {
			t.Errorf("manager %q rendered %q without single-quoting the token %q", def.Name, got, token)
		}
		if n := strings.Count(got, wantQuoted); n != 1 {
			t.Errorf("manager %q rendered token %q quoted %d times, want exactly 1: %q", def.Name, token, n, got)
		}
		// The quote count must be even: every opening quote is closed, i.e.
		// no token escaped the quoting.
		if q := strings.Count(got, "'"); q%2 != 0 {
			t.Errorf("manager %q rendered an ODD number of single quotes (%d) — quoting is unbalanced: %q", def.Name, q, got)
		}
	}
}

// TestDockerfile_GoldenQuotingIsShellInert proves the quoting is not cosmetic:
// the rendered line, when split the way a SHELL splits it, yields the token as
// ONE word. This is the property that makes pip's ">=2.31,<3" safe to render
// into a RUN line, so it is asserted rather than assumed.
func TestDockerfile_GoldenQuotingIsShellInert(t *testing.T) {
	spec := &Spec{
		Base: DefaultBaseImage,
		Packages: []PackageAdd{
			{Manager: ManagerPip, Packages: []string{"requests>=2.31,<3", "flask[async]"}},
			{Manager: ManagerGem, Packages: []string{"rake@~>13.0"}},
		},
	}
	got := spec.Dockerfile()
	// A shell parses each RUN line's command; the packages are the quoted
	// words. With real quoting, no unquoted shell operator (`<`, `>`) is ever
	// visible to the shell: every one of them sits inside a quoted word.
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "RUN ") {
			continue
		}
		cmd := line[len("RUN "):]
		spans := quotedSpans(cmd)
		if len(spans) == 0 {
			t.Errorf("RUN line has no quoted package word: %q", line)
		}
		for i := 0; i < len(cmd); i++ {
			c := cmd[i]
			if c != '<' && c != '>' {
				continue
			}
			if !withinAnySpan(spans, i) {
				t.Errorf("shell operator %q at index %d is OUTSIDE the quoting in %q", string(c), i, cmd)
			}
		}
	}
}

// TestDockerfile_GemMalformedTokenRendersAsNameOnly pins the conservative gem
// split: only a token with exactly one "@" is name@requirement. Anything else
// (no "@", two "@") is the whole token as the NAME — the renderer never invents
// a requirement out of a malformed token's tail.
func TestDockerfile_GemMalformedTokenRendersAsNameOnly(t *testing.T) {
	tests := []struct {
		token string
		want  string
	}{
		{"puma", "RUN gem install --no-document 'puma'\n"},
		{"rake@~>13.0", "RUN gem install --no-document -v '~>13.0' 'rake'\n"},
		{"a@b@c", "RUN gem install --no-document 'a@b@c'\n"},
		{"a@", "RUN gem install --no-document 'a@'\n"},
	}
	for _, tt := range tests {
		spec := &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{{Manager: ManagerGem, Packages: []string{tt.token}}}}
		got := spec.Dockerfile()
		if !strings.HasSuffix(got, tt.want) {
			t.Errorf("gem token %q rendered\n%q\nwant it to end with\n%q", tt.token, got, tt.want)
		}
		// Whatever the shape, the quoting stays balanced and the token text
		// appears in full — never silently truncated.
		if strings.Count(got, "'")%2 != 0 {
			t.Errorf("gem token %q rendered unbalanced quotes: %q", tt.token, got)
		}
	}
}

// pickAcceptedName returns a plain package name the row accepts: bare names are
// in the shared class, and apt's only denial is "/".
func pickAcceptedName(t *testing.T, def *ManagerDef) string {
	t.Helper()
	for _, cand := range []string{"ripgrep", "jq", "pkgname"} {
		if err := def.Probe(cand); err == nil {
			return cand
		}
	}
	t.Fatalf("manager %q accepts none of the probe names — it has no usable plain name", def.Name)
	return ""
}

// quotedSpans returns the [start,end) byte spans of single-quoted regions in a
// rendered RUN command.
func quotedSpans(cmd string) [][2]int {
	var spans [][2]int
	open := -1
	for i := 0; i < len(cmd); i++ {
		if cmd[i] != '\'' {
			continue
		}
		if open < 0 {
			open = i
			continue
		}
		spans = append(spans, [2]int{open, i + 1})
		open = -1
	}
	return spans
}

func withinAnySpan(spans [][2]int, i int) bool {
	for _, sp := range spans {
		if i >= sp[0] && i < sp[1] {
			return true
		}
	}
	return false
}
