package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the INT-HOST-007 reaper: `bunker reap` scans the three
// residue surfaces (rootlesskit AppArmor profiles, agent homes, systemd
// linger) with ONE predicate — does this user exist — and DRY RUN IS THE
// DEFAULT: nothing is removed without --apply, a managed entry whose user
// still exists is never removable, entries that do not match the managed
// naming pattern are never classified, and an inconclusive user lookup
// aborts fail-closed before anything would be removed.

// residueFixture is one test's three residue directories plus the entries it
// seeded. All paths live under t.TempDir(); the package seam vars point at
// them for the duration of the test.
type residueFixture struct {
	profileDir string
	homesRoot  string
	lingerDir  string
}

// seedReapFixture creates three residue directories under t.TempDir() with
// the given entry names, points the package seam vars (reapProfileDirPath,
// homesRootPath, lingerDirPath) at them, and stubs reapUserExists with the
// given live set. Profiles are files, homes are directories, linger entries
// are files — exactly what the reaper expects on a real host.
func seedReapFixture(t *testing.T, live map[string]bool, profiles, homes, linger []string) *residueFixture {
	t.Helper()
	base := t.TempDir()
	fx := &residueFixture{
		profileDir: filepath.Join(base, "apparmor.d"),
		homesRoot:  filepath.Join(base, "home"),
		lingerDir:  filepath.Join(base, "linger"),
	}
	for _, dir := range []string{fx.profileDir, fx.homesRoot, fx.lingerDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range profiles {
		if err := os.WriteFile(filepath.Join(fx.profileDir, name), []byte("profile\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range homes {
		if err := os.MkdirAll(filepath.Join(fx.homesRoot, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range linger {
		if err := os.WriteFile(filepath.Join(fx.lingerDir, name), []byte{}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	origProfileDir, origHomes, origLinger := reapProfileDirPath, homesRootPath, lingerDirPath
	reapProfileDirPath, homesRootPath, lingerDirPath = fx.profileDir, fx.homesRoot, fx.lingerDir
	t.Cleanup(func() {
		reapProfileDirPath, homesRootPath, lingerDirPath = origProfileDir, origHomes, origLinger
	})
	stubReapUserExists(t, live)
	return fx
}

// stubReapUserExists swaps the reaper's user-existence seam; live names the
// users that resolve. Restored via t.Cleanup.
func stubReapUserExists(t *testing.T, live map[string]bool) {
	t.Helper()
	orig := reapUserExists
	reapUserExists = func(name string) (bool, error) {
		if v, ok := live[name]; ok {
			return v, nil
		}
		return false, nil
	}
	t.Cleanup(func() { reapUserExists = orig })
}

// reapEntryExists reports whether path exists on disk (any type).
func reapEntryExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("lstat %s: %v", path, err)
	return false
}

// residueFixturePaths returns one full path per class for the named entry.
func (fx *residueFixture) profilePath(name string) string { return filepath.Join(fx.profileDir, name) }
func (fx *residueFixture) homePath(name string) string    { return filepath.Join(fx.homesRoot, name) }
func (fx *residueFixture) lingerPath(name string) string  { return filepath.Join(fx.lingerDir, name) }

func TestReapProfileUserParsing(t *testing.T) {
	// The user parsed out of a profile filename is the FULL agent user
	// (bunker-<id>): the suffix is stripped, never the prefix — the same
	// name homes and linger look up.
	tests := []struct {
		filename string
		wantUser string
		wantOK   bool
	}{
		{"home.bunker-abc123.bin.rootlesskit", "bunker-abc123", true},
		{"home.bunker-abc123.bin.rootlesskit.txt", "", false}, // trailing junk
		{"bunker-abc123.bin.rootlesskit", "", false},          // missing home. prefix
		{"home..bin.rootlesskit", "", false},                  // empty user
		{"home.bin.rootlesskit", "", false},                   // nothing between
		{"usr.bin.firefox", "", false},                        // unrelated policy file
		{"home.bunker-x.bin.rootlesski", "", false},           // truncated suffix
	}
	for _, tc := range tests {
		gotUser, gotOK := reapProfileUser(tc.filename)
		if gotUser != tc.wantUser || gotOK != tc.wantOK {
			t.Errorf("reapProfileUser(%q) = (%q, %v), want (%q, %v)",
				tc.filename, gotUser, gotOK, tc.wantUser, tc.wantOK)
		}
	}
}

func TestReapScanClassifiesThreeClasses(t *testing.T) {
	// One live user and one gone user, exercised through all three class
	// scans: managed entries split removable (user gone) vs kept (user
	// exists), unmanaged entries are counted but never classified.
	fx := seedReapFixture(t,
		map[string]bool{"bunker-live": true},
		[]string{"home.bunker-gone.bin.rootlesskit", "home.bunker-live.bin.rootlesskit", "usr.bin.firefox"},
		[]string{"bunker-gone", "bunker-live", "kara"},
		[]string{"bunker-gone", "bunker-live"},
	)

	scan, err := reapScanHost(fx.profileDir, fx.homesRoot, fx.lingerDir)
	if err != nil {
		t.Fatalf("reapScanHost: %v", err)
	}

	if got, want := scan.Profiles.WouldRemove, []string{"home.bunker-gone.bin.rootlesskit"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("profiles wouldRemove = %v, want %v", got, want)
	}
	if got, want := scan.Profiles.Kept, []string{"home.bunker-live.bin.rootlesskit"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("profiles kept = %v, want %v", got, want)
	}
	if scan.Profiles.Unmanaged != 1 {
		t.Errorf("profiles unmanaged = %d, want 1 (usr.bin.firefox)", scan.Profiles.Unmanaged)
	}

	if got, want := scan.Homes.WouldRemove, []string{"bunker-gone"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("homes wouldRemove = %v, want %v", got, want)
	}
	if got, want := scan.Homes.Kept, []string{"bunker-live"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("homes kept = %v, want %v", got, want)
	}
	// "kara" (a human home) and nothing else is unmanaged.
	if scan.Homes.Unmanaged != 1 {
		t.Errorf("homes unmanaged = %d, want 1 (kara)", scan.Homes.Unmanaged)
	}

	if got, want := scan.Linger.WouldRemove, []string{"bunker-gone"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("linger wouldRemove = %v, want %v", got, want)
	}
	if got, want := scan.Linger.Kept, []string{"bunker-live"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("linger kept = %v, want %v", got, want)
	}
}

func TestReapDryRunRemovesNothing(t *testing.T) {
	// The no-flag default: every seeded entry (removable or not) must still
	// exist after the scan/report.
	fx := seedReapFixture(t,
		map[string]bool{"bunker-live": true},
		[]string{"home.bunker-gone.bin.rootlesskit", "home.bunker-live.bin.rootlesskit", "usr.bin.firefox"},
		[]string{"bunker-gone", "bunker-live", "kara"},
		[]string{"bunker-gone", "bunker-live"},
	)

	scan, err := reapScanHost(fx.profileDir, fx.homesRoot, fx.lingerDir)
	if err != nil {
		t.Fatalf("reapScanHost: %v", err)
	}
	if len(scan.Profiles.WouldRemove) != 1 || len(scan.Homes.WouldRemove) != 1 || len(scan.Linger.WouldRemove) != 1 {
		t.Fatalf("fixture should classify exactly one orphan per class: %+v", scan)
	}

	for _, p := range []string{
		fx.profilePath("home.bunker-gone.bin.rootlesskit"),
		fx.profilePath("home.bunker-live.bin.rootlesskit"),
		fx.profilePath("usr.bin.firefox"),
		fx.homePath("bunker-gone"),
		fx.homePath("bunker-live"),
		fx.homePath("kara"),
		fx.lingerPath("bunker-gone"),
		fx.lingerPath("bunker-live"),
	} {
		if !reapEntryExists(t, p) {
			t.Errorf("dry run removed %s", p)
		}
	}
}

func TestReapCommandDryRunIsDefault(t *testing.T) {
	// Driving the REAL command with no flags: the report marks itself as a
	// dry run, reports would-remove counts, and mutates nothing.
	fx := seedReapFixture(t,
		map[string]bool{"bunker-live": true},
		[]string{"home.bunker-gone.bin.rootlesskit", "home.bunker-live.bin.rootlesskit"},
		[]string{"bunker-gone", "bunker-live", "kara"},
		[]string{"bunker-gone", "bunker-live"},
	)

	var buf bytes.Buffer
	cmd := NewReapCommand()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("reap execute: %v (output: %s)", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"DRY RUN",
		"would remove (user gone): 1",
		"kept (live user): 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run report missing %q:\n%s", want, out)
		}
	}
	for _, p := range []string{
		fx.profilePath("home.bunker-gone.bin.rootlesskit"),
		fx.homePath("bunker-gone"),
		fx.lingerPath("bunker-gone"),
	} {
		if !reapEntryExists(t, p) {
			t.Errorf("bare `bunker reap` removed %s", p)
		}
	}
}

func TestReapApplyRemovesExactlyOrphans(t *testing.T) {
	fx := seedReapFixture(t,
		map[string]bool{"bunker-live": true},
		[]string{"home.bunker-gone.bin.rootlesskit", "home.bunker-live.bin.rootlesskit", "usr.bin.firefox"},
		[]string{"bunker-gone", "bunker-live", "kara"},
		[]string{"bunker-gone", "bunker-live"},
	)

	scan, err := reapScanHost(fx.profileDir, fx.homesRoot, fx.lingerDir)
	if err != nil {
		t.Fatalf("reapScanHost: %v", err)
	}
	removed, err := reapApply(scan, fx.profileDir, fx.homesRoot, fx.lingerDir)
	if err != nil {
		t.Fatalf("reapApply: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3 (one orphan per class)", removed)
	}

	// Exactly the orphan entries are gone.
	for _, p := range []string{
		fx.profilePath("home.bunker-gone.bin.rootlesskit"),
		fx.homePath("bunker-gone"),
		fx.lingerPath("bunker-gone"),
	} {
		if reapEntryExists(t, p) {
			t.Errorf("--apply left orphan %s in place", p)
		}
	}
	// Kept entries and unmanaged entries survive, all of them.
	for _, p := range []string{
		fx.profilePath("home.bunker-live.bin.rootlesskit"),
		fx.profilePath("usr.bin.firefox"),
		fx.homePath("bunker-live"),
		fx.homePath("kara"),
		fx.lingerPath("bunker-live"),
	} {
		if !reapEntryExists(t, p) {
			t.Errorf("--apply removed a kept/unmanaged entry: %s", p)
		}
	}
}

func TestReapCommandApplyFlag(t *testing.T) {
	// End-to-end through the command: --apply removes the orphans and the
	// report says applied with the removal count.
	fx := seedReapFixture(t,
		map[string]bool{"bunker-live": true},
		[]string{"home.bunker-gone.bin.rootlesskit", "home.bunker-live.bin.rootlesskit"},
		[]string{"bunker-gone", "bunker-live"},
		[]string{"bunker-gone", "bunker-live"},
	)

	var buf bytes.Buffer
	cmd := NewReapCommand()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--apply"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("reap --apply execute: %v (output: %s)", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "reap: applied") || !strings.Contains(out, "removed: 3") {
		t.Errorf("apply report missing applied/removed lines:\n%s", out)
	}
	if reapEntryExists(t, fx.profilePath("home.bunker-gone.bin.rootlesskit")) ||
		reapEntryExists(t, fx.homePath("bunker-gone")) ||
		reapEntryExists(t, fx.lingerPath("bunker-gone")) {
		t.Errorf("--apply did not remove all three orphans:\n%s", out)
	}
	if !reapEntryExists(t, fx.profilePath("home.bunker-live.bin.rootlesskit")) ||
		!reapEntryExists(t, fx.homePath("bunker-live")) ||
		!reapEntryExists(t, fx.lingerPath("bunker-live")) {
		t.Errorf("--apply removed a kept entry:\n%s", out)
	}
}

func TestReapInconclusiveLookupFailsClosed(t *testing.T) {
	// A lookup ERROR is inconclusive (not "user absent"): the scan aborts
	// and — even with removal requested — nothing is deleted.
	fx := seedReapFixture(t,
		map[string]bool{"bunker-live": true},
		[]string{"home.bunker-gone.bin.rootlesskit"},
		[]string{"bunker-gone"},
		[]string{"bunker-gone"},
	)
	orig := reapUserExists
	reapUserExists = func(name string) (bool, error) {
		if name == "bunker-gone" {
			return false, errors.New("nss database unavailable")
		}
		return true, nil
	}
	t.Cleanup(func() { reapUserExists = orig })

	scan, err := reapScanHost(fx.profileDir, fx.homesRoot, fx.lingerDir)
	if err == nil {
		t.Fatalf("inconclusive lookup must fail the scan, got scan %+v", scan)
	}
	if !strings.Contains(err.Error(), "bunker-gone") {
		t.Errorf("error should name the user it could not resolve: %v", err)
	}
	// The verb's ordering guarantees reapApply never runs after a scan
	// error; prove nothing was removed anyway.
	for _, p := range []string{
		fx.profilePath("home.bunker-gone.bin.rootlesskit"),
		fx.homePath("bunker-gone"),
		fx.lingerPath("bunker-gone"),
	} {
		if !reapEntryExists(t, p) {
			t.Errorf("inconclusive lookup path removed %s", p)
		}
	}
}

func TestReapUnmanagedNeverClassified(t *testing.T) {
	// Entries outside the managed patterns — a human home, an unrelated
	// policy file, a bare-prefix directory — are counted unmanaged and are
	// NEVER in the removable set, even with their "user" absent.
	fx := seedReapFixture(t,
		map[string]bool{},
		[]string{"usr.bin.firefox", "home.bin.rootlesskit"},
		[]string{"kara", "bunker-"}, // bunker- alone is unmanaged (empty remainder)
		[]string{},
	)

	scan, err := reapScanHost(fx.profileDir, fx.homesRoot, fx.lingerDir)
	if err != nil {
		t.Fatalf("reapScanHost: %v", err)
	}
	if scan.Profiles.Unmanaged != 2 || len(scan.Profiles.WouldRemove) != 0 {
		t.Errorf("profiles: unmanaged=%d wouldRemove=%v, want 2/[]", scan.Profiles.Unmanaged, scan.Profiles.WouldRemove)
	}
	if scan.Homes.Unmanaged != 2 || len(scan.Homes.WouldRemove) != 0 {
		t.Errorf("homes: unmanaged=%d wouldRemove=%v, want 2/[]", scan.Homes.Unmanaged, scan.Homes.WouldRemove)
	}

	scan2 := &reapScan{Profiles: scan.Profiles, Homes: scan.Homes, Linger: scan.Linger}
	if _, err := reapApply(scan2, fx.profileDir, fx.homesRoot, fx.lingerDir); err != nil {
		t.Fatalf("reapApply: %v", err)
	}
	for _, p := range []string{
		fx.profilePath("usr.bin.firefox"),
		fx.profilePath("home.bin.rootlesskit"),
		fx.homePath("kara"),
		fx.homePath("bunker-"),
	} {
		if !reapEntryExists(t, p) {
			t.Errorf("unmanaged entry %s was removed", p)
		}
	}
}
