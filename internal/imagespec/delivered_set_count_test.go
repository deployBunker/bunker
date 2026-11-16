package imagespec

import "testing"

// TestDeliveredSetToolCount pins the derivation the spawn records as a
// tools-readiness fact (DOGFOOD-017): the count of delivered-set REGISTRY tools
// a spawn's image spec installs. The four cases are the ones the daemon can
// actually see: no spec (plain spawn), the canonical delivered set (rg+gopls),
// a single tool, and a spec that adds packages but no delivered-set tool — that
// last one must NOT be reported as tools-ready just because an image exists.
func TestDeliveredSetToolCount(t *testing.T) {
	aptOnly := &Spec{Packages: []PackageAdd{{Manager: ManagerAPT, Packages: []string{"ripgrep"}}}}
	goOnly := &Spec{Packages: []PackageAdd{{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@latest"}}}}
	pinned := &Spec{Packages: []PackageAdd{
		{Manager: ManagerAPT, Packages: []string{"ripgrep=14.1.0-1"}},
		{Manager: ManagerGo, Packages: []string{"golang.org/x/tools/gopls@v0.16.2"}},
	}}
	noTools := &Spec{Packages: []PackageAdd{
		{Manager: ManagerAPT, Packages: []string{"jq", "curl"}},
		{Manager: ManagerNPM, Packages: []string{"typescript@5.6.3"}},
	}}

	tests := []struct {
		name string
		spec *Spec
		want int
	}{
		{"plain spawn builds no image", nil, 0},
		{"canonical delivered set", AgentToolSpecSpec(), DeliveredSetSize()},
		{"single registry tool", aptOnly, 1},
		{"language server only", goOnly, 1},
		{"pinned spellings still count", pinned, 2},
		{"image spec without registry tools", noTools, 0},
		{"empty spec", &Spec{}, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeliveredSetToolCount(tc.spec); got != tc.want {
				t.Errorf("DeliveredSetToolCount(%+v) = %d, want %d", tc.spec, got, tc.want)
			}
		})
	}
}

// TestDeliveredSetSizeMatchesThePackages pins the denominator against the set
// itself: a rendered "N of 2" that drifts from AgentToolPackages would report a
// full set as partial (or the reverse) with no test noticing.
func TestDeliveredSetSizeMatchesThePackages(t *testing.T) {
	want := 0
	for _, add := range AgentToolPackages {
		want += len(add.Packages)
	}
	if got := DeliveredSetSize(); got != want {
		t.Errorf("DeliveredSetSize() = %d, want %d (sum of AgentToolPackages)", got, want)
	}
	if got := len(AgentToolPackageNames()); got != want {
		t.Errorf("AgentToolPackageNames() has %d names, want %d", got, want)
	}
}

// TestBasePackageNameStripsManagerPins covers the separators each manager uses
// for a version constraint, plus the un-pinned form that must pass through
// byte-identical.
func TestBasePackageNameStripsManagerPins(t *testing.T) {
	cases := map[string]string{
		"ripgrep":                         "ripgrep",
		"ripgrep=14.1.0-1":                "ripgrep",
		"golang.org/x/tools/gopls@latest": "golang.org/x/tools/gopls",
		"typescript@5.6.3":                "typescript",
		"requests>=2.31":                  "requests",
		"vendor/package:^1.2":             "vendor/package",
	}
	for in, want := range cases {
		if got := basePackageName(in); got != want {
			t.Errorf("basePackageName(%q) = %q, want %q", in, got, want)
		}
	}
}
