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
	runDir     string
}

// seedReapFixture creates four residue directories under t.TempDir() with
// the given entry names, points the package seam vars (reapProfileDirPath,
// homesRootPath, lingerDirPath, reapRunDirPath) at them, and stubs
// reapUserExists with the given live set. Profiles are files, homes and run
// dirs are directories, linger entries are files — exactly what the reaper
// expects on a real host.
func seedReapFixture(t *testing.T, live map[string]bool, profiles, homes, linger, runDirs []string) *residueFixture {
	t.Helper()
	base := t.TempDir()
	fx := &residueFixture{
		profileDir: filepath.Join(base, "apparmor.d"),
		homesRoot:  filepath.Join(base, "home"),
		lingerDir:  filepath.Join(base, "linger"),
		runDir:     filepath.Join(base, "run-bunker"),
	}
	for _, dir := range []string{fx.profileDir, fx.homesRoot, fx.lingerDir, fx.runDir} {
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
	for _, name := range runDirs {
		// A real run dir holds the agent's docker socket symlink and the
		// rootless runtime dir; seed one child so removal must be recursive.
		dir := filepath.Join(fx.runDir, name)
		if err := os.MkdirAll(filepath.Join(dir, "run"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/run/user/0/docker.sock", filepath.Join(dir, "docker.sock")); err != nil {
			t.Fatal(err)
		}
	}
	origProfileDir, origHomes, origLinger, origRun := reapProfileDirPath, homesRootPath, lingerDirPath, reapRunDirPath
	reapProfileDirPath, homesRootPath, lingerDirPath, reapRunDirPath = fx.profileDir, fx.homesRoot, fx.lingerDir, fx.runDir
	t.Cleanup(func() {
		reapProfileDirPath, homesRootPath, lingerDirPath, reapRunDirPath = origProfileDir, origHomes, origLinger, origRun
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
func (fx *residueFixture) runDirPath(name string) string  { return filepath.Join(fx.runDir, name) }

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

func TestReapScanClassifiesFourClasses(t *testing.T) {
	// One live user and one gone user, exercised through all four class
	// scans: managed entries split removable (user gone) vs kept (user
	// exists), unmanaged entries are counted but never classified.
	fx := seedReapFixture(t,
		map[string]bool{"bunker-live": true, "bunker-liveid": true},
		[]string{"home.bunker-gone.bin.rootlesskit", "home.bunker-live.bin.rootlesskit", "usr.bin.firefox"},
		[]string{"bunker-gone", "bunker-live", "kara"},
		[]string{"bunker-gone", "bunker-live"},
		[]string{"goneid", "liveid", "e2e_main", "has.dots"},
	)

	scan, err := reapScanHost(fx.profileDir, fx.homesRoot, fx.lingerDir, fx.runDir)
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

	// Runtime dirs: the DIRECTORY name is the agent id, so the predicate is
	// asked about bunker-<id>. "e2e_main" (underscore) and "has.dots" are not
	// agent-id shaped: unmanaged, never classified, never removable.
	if got, want := scan.RunDirs.WouldRemove, []string{"goneid"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("run dirs wouldRemove = %v, want %v", got, want)
	}
	if got, want := scan.RunDirs.Kept, []string{"liveid"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("run dirs kept = %v, want %v", got, want)
	}
	if scan.RunDirs.Unmanaged != 2 {
		t.Errorf("run dirs unmanaged = %d, want 2 (e2e_main, has.dots)", scan.RunDirs.Unmanaged)
	}
}

func TestReapDryRunRemovesNothing(t *testing.T) {
	// The no-flag default: every seeded entry (removable or not) must still
	// exist after the scan/report.
	fx := seedReapFixture(t,
		map[string]bool{"bunker-live": true, "bunker-liveid": true},
		[]string{"home.bunker-gone.bin.rootlesskit", "home.bunker-live.bin.rootlesskit", "usr.bin.firefox"},
		[]string{"bunker-gone", "bunker-live", "kara"},
		[]string{"bunker-gone", "bunker-live"},
		[]string{"goneid", "liveid"},
	)

	scan, err := reapScanHost(fx.profileDir, fx.homesRoot, fx.lingerDir, fx.runDir)
	if err != nil {
		t.Fatalf("reapScanHost: %v", err)
	}
	if len(scan.Profiles.WouldRemove) != 1 || len(scan.Homes.WouldRemove) != 1 ||
		len(scan.Linger.WouldRemove) != 1 || len(scan.RunDirs.WouldRemove) != 1 {
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
		fx.runDirPath("goneid"),
		fx.runDirPath("liveid"),
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
		map[string]bool{"bunker-live": true, "bunker-liveid": true},
		[]string{"home.bunker-gone.bin.rootlesskit", "home.bunker-live.bin.rootlesskit"},
		[]string{"bunker-gone", "bunker-live", "kara"},
		[]string{"bunker-gone", "bunker-live"},
		[]string{"goneid", "liveid"},
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
		"agent runtime dirs: " + fx.runDir,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run report missing %q:\n%s", want, out)
		}
	}
	for _, p := range []string{
		fx.profilePath("home.bunker-gone.bin.rootlesskit"),
		fx.homePath("bunker-gone"),
		fx.lingerPath("bunker-gone"),
		fx.runDirPath("goneid"),
	} {
		if !reapEntryExists(t, p) {
			t.Errorf("bare `bunker reap` removed %s", p)
		}
	}
}

func TestReapApplyRemovesExactlyOrphans(t *testing.T) {
	fx := seedReapFixture(t,
		map[string]bool{"bunker-live": true, "bunker-liveid": true},
		[]string{"home.bunker-gone.bin.rootlesskit", "home.bunker-live.bin.rootlesskit", "usr.bin.firefox"},
		[]string{"bunker-gone", "bunker-live", "kara"},
		[]string{"bunker-gone", "bunker-live"},
		[]string{"goneid", "liveid", "e2e_main"},
	)

	scan, err := reapScanHost(fx.profileDir, fx.homesRoot, fx.lingerDir, fx.runDir)
	if err != nil {
		t.Fatalf("reapScanHost: %v", err)
	}
	removed, err := reapApply(scan, fx.profileDir, fx.homesRoot, fx.lingerDir, fx.runDir)
	if err != nil {
		t.Fatalf("reapApply: %v", err)
	}
	if removed != 4 {
		t.Errorf("removed = %d, want 4 (one orphan per class)", removed)
	}

	// Exactly the orphan entries are gone.
	for _, p := range []string{
		fx.profilePath("home.bunker-gone.bin.rootlesskit"),
		fx.homePath("bunker-gone"),
		fx.lingerPath("bunker-gone"),
		fx.runDirPath("goneid"),
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
		fx.runDirPath("liveid"),
		fx.runDirPath("e2e_main"),
	} {
		if !reapEntryExists(t, p) {
			t.Errorf("--apply removed a kept/unmanaged entry: %s", p)
		}
	}
	// The live agent's own socket symlink is INTACT inside the kept run dir:
	// removal of a sibling's directory must never follow a symlink out.
	if _, err := os.Lstat(filepath.Join(fx.runDirPath("liveid"), "docker.sock")); err != nil {
		t.Errorf("kept run dir lost its docker.sock: %v", err)
	}
}

func TestReapCommandApplyFlag(t *testing.T) {
	// End-to-end through the command: --apply removes the orphans and the
	// report says applied with the removal count.
	fx := seedReapFixture(t,
		map[string]bool{"bunker-live": true, "bunker-liveid": true},
		[]string{"home.bunker-gone.bin.rootlesskit", "home.bunker-live.bin.rootlesskit"},
		[]string{"bunker-gone", "bunker-live"},
		[]string{"bunker-gone", "bunker-live"},
		[]string{"goneid", "liveid"},
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
	if !strings.Contains(out, "reap: applied") || !strings.Contains(out, "removed: 4") {
		t.Errorf("apply report missing applied/removed lines:\n%s", out)
	}
	if reapEntryExists(t, fx.profilePath("home.bunker-gone.bin.rootlesskit")) ||
		reapEntryExists(t, fx.homePath("bunker-gone")) ||
		reapEntryExists(t, fx.lingerPath("bunker-gone")) ||
		reapEntryExists(t, fx.runDirPath("goneid")) {
		t.Errorf("--apply did not remove all four orphans:\n%s", out)
	}
	if !reapEntryExists(t, fx.profilePath("home.bunker-live.bin.rootlesskit")) ||
		!reapEntryExists(t, fx.homePath("bunker-live")) ||
		!reapEntryExists(t, fx.lingerPath("bunker-live")) ||
		!reapEntryExists(t, fx.runDirPath("liveid")) {
		t.Errorf("--apply removed a kept entry:\n%s", out)
	}
}

func TestReapInconclusiveLookupFailsClosed(t *testing.T) {
	// A lookup ERROR is inconclusive (not "user absent"): the scan aborts
	// and — even with removal requested — nothing is deleted.
	fx := seedReapFixture(t,
		map[string]bool{"bunker-live": true, "bunker-liveid": true},
		[]string{"home.bunker-gone.bin.rootlesskit"},
		[]string{"bunker-gone"},
		[]string{"bunker-gone"},
		[]string{"goneid"},
	)
	orig := reapUserExists
	reapUserExists = func(name string) (bool, error) {
		if name == "bunker-gone" {
			return false, errors.New("nss database unavailable")
		}
		return true, nil
	}
	t.Cleanup(func() { reapUserExists = orig })

	scan, err := reapScanHost(fx.profileDir, fx.homesRoot, fx.lingerDir, fx.runDir)
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
		fx.runDirPath("goneid"),
	} {
		if !reapEntryExists(t, p) {
			t.Errorf("inconclusive lookup path removed %s", p)
		}
	}
}

// TestReapRunDirInconclusiveLookupFailsClosed proves the run-dir class carries
// the SAME fail-closed contract when the RUN-DIR scan is the one that hits the
// inconclusive lookup (the other classes resolve cleanly there): a NSS error on
// bunker-<id> must abort the scan before anything is removable.
func TestReapRunDirInconclusiveLookupFailsClosed(t *testing.T) {
	fx := seedReapFixture(t,
		map[string]bool{"bunker-live": true, "bunker-liveid": true},
		[]string{},
		[]string{},
		[]string{},
		[]string{"goneid"},
	)
	orig := reapUserExists
	reapUserExists = func(name string) (bool, error) {
		if name == "bunker-goneid" {
			return false, errors.New("nss database unavailable")
		}
		return true, nil
	}
	t.Cleanup(func() { reapUserExists = orig })

	if _, err := reapScanHost(fx.profileDir, fx.homesRoot, fx.lingerDir, fx.runDir); err == nil {
		t.Fatal("run-dir class must propagate an inconclusive lookup, got nil error")
	} else if !strings.Contains(err.Error(), "bunker-goneid") {
		t.Errorf("error should name the agent user it could not resolve: %v", err)
	}
	if !reapEntryExists(t, fx.runDirPath("goneid")) {
		t.Error("inconclusive run-dir lookup removed the directory")
	}
}

// TestReapRunDirSymlinkIsNotClassified pins the deletion boundary: a SYMLINK
// under /run/bunker whose name looks like an agent id is never a managed
// directory (removing it would follow/replace someone else's path), and a
// missing /run/bunker is an empty class, not a scan failure.
func TestReapRunDirSymlinkIsNotClassified(t *testing.T) {
	fx := seedReapFixture(t,
		map[string]bool{},
		[]string{}, []string{}, []string{},
		[]string{"realgone"},
	)
	if err := os.Symlink(t.TempDir(), filepath.Join(fx.runDir, "linkid")); err != nil {
		t.Fatal(err)
	}
	scan, err := reapScanHost(fx.profileDir, fx.homesRoot, fx.lingerDir, fx.runDir)
	if err != nil {
		t.Fatalf("reapScanHost: %v", err)
	}
	if got, want := scan.RunDirs.WouldRemove, []string{"realgone"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("run dirs wouldRemove = %v, want %v (the symlink must not be classified)", got, want)
	}
	if scan.RunDirs.Unmanaged != 1 {
		t.Errorf("run dirs unmanaged = %d, want 1 (linkid symlink)", scan.RunDirs.Unmanaged)
	}
	if _, err := reapApply(scan, fx.profileDir, fx.homesRoot, fx.lingerDir, fx.runDir); err != nil {
		t.Fatalf("reapApply: %v", err)
	}
	if !reapEntryExists(t, filepath.Join(fx.runDir, "linkid")) {
		t.Error("--apply removed or replaced the symlink")
	}

	// Missing /run/bunker (tmpfs reset, host never spawned) is an EMPTY class.
	missing := filepath.Join(t.TempDir(), "no-such-run-root")
	scan2, err := reapScanHost(fx.profileDir, fx.homesRoot, fx.lingerDir, missing)
	if err != nil {
		t.Fatalf("missing run dir must not fail the scan: %v", err)
	}
	if len(scan2.RunDirs.WouldRemove) != 0 || len(scan2.RunDirs.Kept) != 0 {
		t.Errorf("missing run dir produced entries: %+v", scan2.RunDirs)
	}
}

// TestReapDryRunFlagIsAcceptedAndExclusive pins the operator spelling: an
// explicit --dry-run is the default and removes nothing, and combining it with
// --apply is refused (a contradictory invocation must never delete host state).
func TestReapDryRunFlagIsAcceptedAndExclusive(t *testing.T) {
	fx := seedReapFixture(t,
		map[string]bool{"bunker-live": true, "bunker-liveid": true},
		[]string{"home.bunker-gone.bin.rootlesskit"},
		[]string{"bunker-gone"},
		[]string{"bunker-gone"},
		[]string{"goneid"},
	)

	var buf bytes.Buffer
	cmd := NewReapCommand()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("reap --dry-run execute: %v (output: %s)", err, buf.String())
	}
	if !strings.Contains(buf.String(), "DRY RUN") {
		t.Errorf("--dry-run report missing DRY RUN marker:\n%s", buf.String())
	}
	if !reapEntryExists(t, fx.runDirPath("goneid")) || !reapEntryExists(t, fx.profilePath("home.bunker-gone.bin.rootlesskit")) {
		t.Error("--dry-run removed host state")
	}

	buf.Reset()
	cmd = NewReapCommand()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--dry-run", "--apply"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("--dry-run --apply must be refused, got success:\n%s", buf.String())
	} else if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("refusal should name the conflict: %v", err)
	}
	if !reapEntryExists(t, fx.runDirPath("goneid")) {
		t.Error("refused invocation still removed host state")
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
		[]string{"e2e_main", ".hidden"},
	)

	scan, err := reapScanHost(fx.profileDir, fx.homesRoot, fx.lingerDir, fx.runDir)
	if err != nil {
		t.Fatalf("reapScanHost: %v", err)
	}
	if scan.Profiles.Unmanaged != 2 || len(scan.Profiles.WouldRemove) != 0 {
		t.Errorf("profiles: unmanaged=%d wouldRemove=%v, want 2/[]", scan.Profiles.Unmanaged, scan.Profiles.WouldRemove)
	}
	if scan.Homes.Unmanaged != 2 || len(scan.Homes.WouldRemove) != 0 {
		t.Errorf("homes: unmanaged=%d wouldRemove=%v, want 2/[]", scan.Homes.Unmanaged, scan.Homes.WouldRemove)
	}
	if scan.RunDirs.Unmanaged != 2 || len(scan.RunDirs.WouldRemove) != 0 {
		t.Errorf("run dirs: unmanaged=%d wouldRemove=%v, want 2/[]", scan.RunDirs.Unmanaged, scan.RunDirs.WouldRemove)
	}

	if _, err := reapApply(scan, fx.profileDir, fx.homesRoot, fx.lingerDir, fx.runDir); err != nil {
		t.Fatalf("reapApply: %v", err)
	}
	for _, p := range []string{
		fx.profilePath("usr.bin.firefox"),
		fx.profilePath("home.bin.rootlesskit"),
		fx.homePath("kara"),
		fx.homePath("bunker-"),
		fx.runDirPath("e2e_main"),
		fx.runDirPath(".hidden"),
	} {
		if !reapEntryExists(t, p) {
			t.Errorf("unmanaged entry %s was removed", p)
		}
	}
}
