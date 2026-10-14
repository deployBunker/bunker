package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/supplychain"
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
		// The runner above is a FAKE: a "download" it records never writes a
		// file, so the GAP-135 checksum gate has nothing to hash here. It is
		// stubbed for the strategy-flow tests and covered for real, against
		// the embedded manifest, in TestArtifactChecksumMismatchIsFatal and
		// in internal/supplychain.
		Verify: func(string, string, string) error { return nil },
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
	// GAP-135: the URL is the PINNED release asset, not a latest/ redirect —
	// the detail is how an operator sees which version was installed.
	if !strings.Contains(fo.Detail, "v1.18") {
		t.Errorf("artifact detail should name the pinned version: %q", fo.Detail)
	}
	if strings.Contains(fo.Detail, "/latest/") {
		t.Errorf("artifact detail still names a moving latest URL: %q", fo.Detail)
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

// ─────────────────────────────────────────────────────────────────────────────
// GAP-135: the artifact strategy is pinned and verified
// ─────────────────────────────────────────────────────────────────────────────

// TestEveryPrerequisiteArtifactIsPinned is the typo gate for the manifest
// lookups: a prerequisite that names an artifact the supply-chain manifest does
// not pin would silently lose its artifact strategy at runtime (an unpinned
// artifact cannot be verified, so it cannot be downloaded). It also requires
// the pinned entry to carry the architecture this host asks for.
func TestEveryPrerequisiteArtifactIsPinned(t *testing.T) {
	f := newFakeHost()
	arch := f.options(true, "").arch()
	for _, p := range rootlessPrerequisites() {
		if p.Artifact == nil {
			continue
		}
		url, err := supplychain.ArtifactURL(p.Artifact.PinName, arch)
		if err != nil {
			t.Errorf("prerequisite %s names artifact %q, which is not pinned: %v", p.Name, p.Artifact.PinName, err)
			continue
		}
		if strings.Contains(url, "/latest/") {
			t.Errorf("prerequisite %s resolves a moving URL: %s", p.Name, url)
		}
		if _, err := supplychain.ArtifactDigest(p.Artifact.PinName, arch); err != nil {
			t.Errorf("prerequisite %s has no pinned sha256: %v", p.Name, err)
		}
	}
}

// TestArtifactChecksumMismatchIsFatal drives the REAL gate (the embedded
// manifest through the default Verify) with bytes that are not the pinned
// artifact: the strategy must fail, the binary must NOT be installed, and the
// failure must be the named checksum error.
func TestArtifactChecksumMismatchIsFatal(t *testing.T) {
	f := newFakeHost()
	f.present["curl"] = "/usr/bin/curl"
	opts := f.options(true, osReleaseFixture(t, "plan9", ""))
	// The fake runner never writes a file, so the gate reports a missing
	// artifact rather than a mismatch — which is exactly the fail-closed
	// behaviour under test here (nothing is installed). The mismatch branch
	// itself is covered by the supplychain table and by the download-path test
	// in rootless_pin_test.go.
	opts.Verify = supplychain.VerifyFile

	p := prerequisite{Name: "slirp4netns", Required: true, Artifact: &artifact{
		PinName:    supplychain.ArtifactSlirp4netns,
		BinaryName: "slirp4netns",
	}}
	out := satisfyOne(context.Background(), opts, familyUnknown, p)
	if out.Satisfied {
		t.Fatalf("an artifact that failed verification was reported satisfied: %+v", out)
	}
	if f.ran("mv", "/usr/local/bin/slirp4netns.download") {
		t.Errorf("an unverified binary was installed: %v", f.calls)
	}
	if !strings.Contains(out.Detail, "artifact strategy failed") {
		t.Errorf("the outcome should record the failed gate: %q", out.Detail)
	}
	if !strings.Contains(out.Detail, "verification failed") {
		t.Errorf("the outcome should name the verification failure: %q", out.Detail)
	}
}

// TestArtifactNotPinnedNeverDownloads pins the fail-closed half of the lookup:
// an artifact the manifest does not pin has no digest to check, so the strategy
// must refuse it WITHOUT downloading anything.
func TestArtifactNotPinnedNeverDownloads(t *testing.T) {
	f := newFakeHost()
	f.present["curl"] = "/usr/bin/curl"
	opts := f.options(true, osReleaseFixture(t, "plan9", ""))

	p := prerequisite{Name: "mystery", Required: true, Artifact: &artifact{
		PinName:    "not-a-pinned-artifact",
		BinaryName: "mystery",
	}}
	out := satisfyOne(context.Background(), opts, familyUnknown, p)

	if out.Satisfied {
		t.Fatalf("an unpinned artifact was reported satisfied: %+v", out)
	}
	if f.ran("curl") {
		t.Errorf("an unpinned artifact must not be downloaded at all, got: %v", f.calls)
	}
	if !strings.Contains(out.Detail, "artifact strategy unavailable") {
		t.Errorf("the outcome should name the unavailable artifact strategy: %q", out.Detail)
	}
	// The detail is the operator-facing half; the lookup itself must be what
	// produced it, with the named error an operator can grep for.
	if _, err := supplychain.ArtifactURL(p.Artifact.PinName, opts.arch()); !errors.Is(err, supplychain.ErrNotPinned) {
		t.Errorf("lookup error %v does not unwrap to ErrNotPinned", err)
	}
}
