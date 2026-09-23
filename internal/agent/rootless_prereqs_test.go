package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers: a fake host, so no test ever touches real host state.
// ─────────────────────────────────────────────────────────────────────────────

// osReleaseFixture writes an /etc/os-release stand-in and returns its path.
func osReleaseFixture(t *testing.T, id, idLike string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "os-release")
	content := fmt.Sprintf("NAME=\"Fixture\"\nID=%s\n", id)
	if idLike != "" {
		content += fmt.Sprintf("ID_LIKE=\"%s\"\n", idLike)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// fakeHost records every command and answers from a script.
type fakeHost struct {
	calls [][]string
	// results maps "name arg1 arg2" -> error (nil = success).
	results map[string]error
	// present maps a probe name to a resolved path ("" = absent).
	present map[string]string
	// outputs holds canned output per command key.
	outputs map[string]string
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		results: map[string]error{},
		present: map[string]string{},
		outputs: map[string]string{},
	}
}

func key(argv []string) string { return strings.Join(argv, " ") }

func (f *fakeHost) runner() systemRunner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		argv := append([]string{name}, args...)
		f.calls = append(f.calls, argv)
		k := key(argv)
		out := []byte(f.outputs[k])
		return out, f.results[k]
	}
}

func (f *fakeHost) lookPath() func(string) (string, error) {
	return func(name string) (string, error) {
		if p, ok := f.present[name]; ok && p != "" {
			return p, nil
		}
		return "", fmt.Errorf("%s: not found", name)
	}
}

func (f *fakeHost) options(apply bool, osRelease string) PrereqOptions {
	return PrereqOptions{
		Apply:         apply,
		OSReleasePath: osRelease,
		Runner:        f.runner(),
		LookPath:      f.lookPath(),
		Arch:          "x86_64",
	}
}

// ran reports whether any recorded command matches the prefix.
func (f *fakeHost) ran(prefix ...string) bool {
	want := strings.Join(prefix, " ")
	for _, c := range f.calls {
		if strings.HasPrefix(key(c), want) {
			return true
		}
	}
	return false
}

func outcomeFor(t *testing.T, outcomes []PrereqOutcome, name string) PrereqOutcome {
	t.Helper()
	for _, o := range outcomes {
		if o.Name == name {
			return o
		}
	}
	t.Fatalf("no outcome for %q", name)
	return PrereqOutcome{}
}

// ─────────────────────────────────────────────────────────────────────────────
// Distribution detection
// ─────────────────────────────────────────────────────────────────────────────

func TestDetectFamily(t *testing.T) {
	tests := []struct {
		name, id, idLike string
		want             pkgFamily
	}{
		{"debian", "debian", "", familyDebian},
		{"ubuntu", "ubuntu", "debian", familyDebian},
		{"linux mint is ubuntu-like", "linuxmint", "ubuntu debian", familyDebian},
		{"fedora", "fedora", "", familyRHEL},
		{"rocky is rhel-like", "rocky", "rhel centos fedora", familyRHEL},
		{"alpine", "alpine", "", familyAlpine},
		{"arch", "arch", "", familyArch},
		{"opensuse", "opensuse-leap", "suse", familySUSE},
		{"unknown", "plan9", "", familyUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := osReleaseFixture(t, tt.id, tt.idLike)
			got := detectFamily(context.Background(), PrereqOptions{OSReleasePath: fixture})
			if got != tt.want {
				t.Fatalf("detectFamily(%s/%s) = %q, want %q", tt.id, tt.idLike, got, tt.want)
			}
		})
	}
}

func TestDetectFamily_MissingFileIsUnknownNotError(t *testing.T) {
	got := detectFamily(context.Background(), PrereqOptions{OSReleasePath: "/nonexistent/os-release"})
	if got != familyUnknown {
		t.Fatalf("missing os-release should yield familyUnknown, got %q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Strategy order
// ─────────────────────────────────────────────────────────────────────────────

func TestPresentWinsWithoutRunningAnything(t *testing.T) {
	f := newFakeHost()
	for _, probe := range []string{"newuidmap", "newgidmap", "fuse-overlayfs", "slirp4netns", "dbus-daemon"} {
		f.present[probe] = "/usr/bin/" + probe
	}
	outcomes, err := EnsureRootlessPrerequisites(context.Background(),
		f.options(true, osReleaseFixture(t, "debian", "")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, o := range outcomes {
		if !o.Satisfied {
			t.Errorf("%s not satisfied, want present", o.Name)
		}
		if o.Strategy != strategyPresent {
			t.Errorf("%s strategy = %q, want %q", o.Name, o.Strategy, strategyPresent)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("present strategies must not run commands, ran: %v", f.calls)
	}
}

func TestPackageStrategyInstallsTheFamilyCorrectName(t *testing.T) {
	f := newFakeHost()
	// newuidmap absent -> uidmap must be installed via apt.
	f.present["slirp4netns"] = "/usr/bin/slirp4netns"
	f.present["dbus-daemon"] = "/usr/bin/dbus-daemon"

	outcomes, err := EnsureRootlessPrerequisites(context.Background(),
		f.options(true, osReleaseFixture(t, "ubuntu", "debian")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	uidmap := outcomeFor(t, outcomes, "uidmap")
	if !uidmap.Satisfied || uidmap.Strategy != strategyPackage {
		t.Fatalf("uidmap outcome = %+v, want satisfied via package", uidmap)
	}
	if !f.ran("env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "--no-install-recommends", "uidmap") {
		t.Fatalf("expected the debian uidmap install argv, got: %v", f.calls)
	}
	// fuse-overlayfs is optional: absent probe, no package present entry for
	// it in this fake means apt runs for it too.
	if !f.ran("env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "--no-install-recommends", "fuse-overlayfs") {
		t.Fatalf("expected a fuse-overlayfs install attempt, got: %v", f.calls)
	}
}

func TestPackageStrategyUsesRHELNamesOnFedora(t *testing.T) {
	f := newFakeHost()
	f.present["fuse-overlayfs"] = "/usr/bin/fuse-overlayfs"
	f.present["slirp4netns"] = "/usr/bin/slirp4netns"
	f.present["dbus-daemon"] = "/usr/bin/dbus-daemon"

	if _, err := EnsureRootlessPrerequisites(context.Background(),
		f.options(true, osReleaseFixture(t, "fedora", ""))); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// uidmap is "shadow-utils" on the RHEL family, not "uidmap".
	if !f.ran("dnf", "install", "-y", "shadow-utils") {
		t.Fatalf("expected dnf install shadow-utils, got: %v", f.calls)
	}
	if f.ran("dnf", "install", "-y", "uidmap") {
		t.Fatalf("must not use the Debian package name on fedora: %v", f.calls)
	}
}

func TestPackageStrategyRefreshesThenRetriesOnce(t *testing.T) {
	f := newFakeHost()
	f.present["fuse-overlayfs"] = "/usr/bin/fuse-overlayfs"
	f.present["slirp4netns"] = "/usr/bin/slirp4netns"
	f.present["dbus-daemon"] = "/usr/bin/dbus-daemon"

	install := "env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends uidmap"
	update := "env DEBIAN_FRONTEND=noninteractive apt-get update"
	f.results[update] = nil
	calls := 0
	opts := f.options(true, osReleaseFixture(t, "debian", ""))
	opts.Runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		argv := append([]string{name}, args...)
		f.calls = append(f.calls, argv)
		if key(argv) == install {
			calls++
			if calls == 1 {
				return []byte("E: stale index"), fmt.Errorf("stale index")
			}
			return nil, nil
		}
		return nil, f.results[key(argv)]
	}

	outcomes, err := EnsureRootlessPrerequisites(context.Background(), opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	uidmap := outcomeFor(t, outcomes, "uidmap")
	if !uidmap.Satisfied || uidmap.Strategy != strategyPackage {
		t.Fatalf("uidmap = %+v, want satisfied via package after refresh", uidmap)
	}
	if !strings.Contains(uidmap.Detail, "after refresh") {
		t.Errorf("detail should record the refresh: %q", uidmap.Detail)
	}
	if !f.ran("env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "update") {
		t.Errorf("expected a metadata refresh between attempts, got: %v", f.calls)
	}
	if calls != 2 {
		t.Errorf("install should be attempted exactly twice, got %d", calls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Fallbacks: artifact, and honest failure
// ─────────────────────────────────────────────────────────────────────────────

func TestArtifactFallbackWhenNoPackageStrategy(t *testing.T) {
	f := newFakeHost()
	// unknown distribution -> no package names; curl present so the artifact
	// strategy is viable.
	f.present["curl"] = "/usr/bin/curl"
	f.present["newuidmap"] = "/usr/bin/newuidmap"
	f.present["newgidmap"] = "/usr/bin/newgidmap"
	f.present["slirp4netns"] = "/usr/bin/slirp4netns"
	f.present["dbus-daemon"] = "/usr/bin/dbus-daemon"

	outcomes, err := EnsureRootlessPrerequisites(context.Background(),
		f.options(true, osReleaseFixture(t, "plan9", "")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fo := outcomeFor(t, outcomes, "fuse-overlayfs")
	if !fo.Satisfied || fo.Strategy != strategyArtifact {
		t.Fatalf("fuse-overlayfs = %+v, want satisfied via artifact", fo)
	}
	if !strings.Contains(fo.Detail, "x86_64") {
		t.Errorf("artifact detail should name the arch-specific URL: %q", fo.Detail)
	}
	if !f.ran("curl", "-fsSL", "-o", "/usr/local/bin/fuse-overlayfs.download") {
		t.Errorf("expected a curl download, got: %v", f.calls)
	}
	if !f.ran("mv", "/usr/local/bin/fuse-overlayfs.download", "/usr/local/bin/fuse-overlayfs") {
		t.Errorf("expected the binary to be installed to /usr/local/bin, got: %v", f.calls)
	}
}

func TestRequiredMissingIsALoudErrorNamingStrategies(t *testing.T) {
	f := newFakeHost()
	// Nothing present, unknown distro (no package), no curl (no artifact),
	// no cc (no source): uidmap cannot be satisfied and it is required.
	outcomes, err := EnsureRootlessPrerequisites(context.Background(),
		f.options(true, osReleaseFixture(t, "plan9", "")))
	if err == nil {
		t.Fatal("expected an error for a required unsatisfied prerequisite")
	}
	if !strings.Contains(err.Error(), "uidmap") {
		t.Errorf("error should name the missing prerequisite: %v", err)
	}
	if !strings.Contains(err.Error(), "source strategy unavailable") {
		t.Errorf("error should record the strategies tried: %v", err)
	}
	// The optional ones must not fail the install.
	if o := outcomeFor(t, outcomes, "fuse-overlayfs"); o.Satisfied {
		t.Errorf("fuse-overlayfs should be unsatisfied here, got %+v", o)
	}
	if o := outcomeFor(t, outcomes, "fuse-overlayfs"); o.Strategy != strategySkipped {
		t.Errorf("optional miss should be %q, got %q", strategySkipped, o.Strategy)
	}
}

func TestOptionalMissingDoesNotError(t *testing.T) {
	f := newFakeHost()
	f.present["newuidmap"] = "/usr/bin/newuidmap"
	f.present["newgidmap"] = "/usr/bin/newgidmap"
	f.present["slirp4netns"] = "/usr/bin/slirp4netns"
	// dbus-daemon absent and optional; family unknown so no package strategy.
	if _, err := EnsureRootlessPrerequisites(context.Background(),
		f.options(true, osReleaseFixture(t, "plan9", ""))); err != nil {
		t.Fatalf("a missing OPTIONAL prerequisite must not fail: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Plan mode
// ─────────────────────────────────────────────────────────────────────────────

func TestPlanChangesNothingAndNamesTheWork(t *testing.T) {
	f := newFakeHost()
	f.present["curl"] = "/usr/bin/curl"

	lines, err := PlanPrerequisites(context.Background(),
		f.options(false, osReleaseFixture(t, "debian", "")))
	if err == nil {
		t.Fatal("planning for a required-missing host should still report the gap")
	}
	if len(f.calls) != 0 {
		t.Fatalf("plan mode must not run commands, ran: %v", f.calls)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"detected distribution family: debian", "uidmap", "fuse-overlayfs", "plan: would install uidmap via debian"} {
		if !strings.Contains(joined, want) {
			t.Errorf("plan missing %q in:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "would download") {
		t.Errorf("plan should mention the artifact fallback:\n%s", joined)
	}
}

func TestPlanRendersEveryPrerequisiteOnce(t *testing.T) {
	f := newFakeHost()
	lines, _ := PlanPrerequisites(context.Background(), f.options(false, osReleaseFixture(t, "debian", "")))
	// one header line + one per prerequisite
	if got, want := len(lines), len(rootlessPrerequisites())+1; got != want {
		t.Fatalf("plan lines = %d, want %d:\n%s", got, want, strings.Join(lines, "\n"))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The dependency table itself
// ─────────────────────────────────────────────────────────────────────────────

func TestEveryPrerequisiteHasAProbeAndCoversKnownFamilies(t *testing.T) {
	families := []pkgFamily{familyDebian, familyRHEL, familyAlpine, familyArch, familySUSE}
	for _, p := range rootlessPrerequisites() {
		if len(p.Probes) == 0 {
			t.Errorf("%s has no probe, so 'present' can never be detected", p.Name)
		}
		if p.Why == "" {
			t.Errorf("%s has no Why, so the plan cannot explain it", p.Name)
		}
		for _, fam := range families {
			if strings.TrimSpace(p.Packages[fam]) == "" {
				t.Errorf("%s has no package name for family %s", p.Name, fam)
			}
		}
	}
}

func TestRequiredPrerequisitesAreTheOnesThatBreakEverything(t *testing.T) {
	// uidmap and slirp4netns are load-bearing; the rest must not block a spawn
	// on a host that is merely minimal.
	required := map[string]bool{}
	for _, p := range rootlessPrerequisites() {
		if p.Required {
			required[p.Name] = true
		}
	}
	for _, name := range []string{"uidmap", "slirp4netns"} {
		if !required[name] {
			t.Errorf("%s should be required", name)
		}
	}
	for _, name := range []string{"fuse-overlayfs", "dbus-user-session"} {
		if required[name] {
			t.Errorf("%s should be optional, not required", name)
		}
	}
}
