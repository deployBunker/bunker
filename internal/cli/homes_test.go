package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the GAP-080 CLI surface: `bunker homes` reports every managed
// bunker-* home as STALE (its user no longer exists) or KEPT (its user exists),
// read-only, and `bunker homes prune` removes exactly the stale entries — never
// an entry whose user exists, never an entry that does not match the managed
// name pattern, and nothing at all when a user lookup is INCONCLUSIVE
// (fail-closed). --dry-run mutates nothing and a missing root is a clean no-op.

// seedHomesDir creates a home-root fixture with the given directory names.
func seedHomesDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// homesNames returns the sorted entry names currently in dir.
func homesNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// stubHomeUserExists swaps the user-existence seam; exists names the users that
// resolve. Restored via t.Cleanup.
func stubHomeUserExists(t *testing.T, exists map[string]bool) {
	t.Helper()
	orig := homeUserExists
	homeUserExists = func(name string) (bool, error) {
		if v, ok := exists[name]; ok {
			return v, nil
		}
		return false, nil
	}
	t.Cleanup(func() { homeUserExists = orig })
}

// runHomesPrune executes the real prune command against dir and returns its
// output.
func runHomesPrune(t *testing.T, dir string, extraArgs ...string) string {
	t.Helper()
	cmd := newHomesPruneCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	args := append([]string{"--dir", dir}, extraArgs...)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prune execute: %v (output: %s)", err, buf.String())
	}
	return buf.String()
}

// runHomesGroup executes the bare `bunker homes` command with the given args
// and returns its output plus the error.
func runHomesGroup(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewHomesCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

// TestHomesCommandSurface pins the CLI shape (linger/registry compact
// precedent): a `homes` group with a `prune` subcommand exposing --dir and
// --dry-run, plus --dir on the bare group (its read-only report takes the root
// to inspect).
func TestHomesCommandSurface(t *testing.T) {
	cmd := NewHomesCommand()
	if cmd.Use != "homes" {
		t.Errorf("Use = %q, want homes", cmd.Use)
	}
	if cmd.Flags().Lookup("dir") == nil {
		t.Error("the bare homes group must expose --dir")
	}
	if cmd.RunE == nil {
		t.Error("the bare homes group must run the read-only report")
	}
	var pruneFound bool
	for _, sub := range cmd.Commands() {
		if sub.Name() == "prune" {
			pruneFound = true
			if sub.Flags().Lookup("dir") == nil {
				t.Error("prune must expose --dir")
			}
			if sub.Flags().Lookup("dry-run") == nil {
				t.Error("prune must expose --dry-run")
			}
		}
	}
	if !pruneFound {
		t.Fatal("homes must have a prune subcommand")
	}
	if defaultHomesRoot != "/home" {
		t.Errorf("default homes root = %q, want /home", defaultHomesRoot)
	}
	if homesRootPath != defaultHomesRoot {
		t.Errorf("homesRootPath = %q, want the default %q", homesRootPath, defaultHomesRoot)
	}
}

// TestHomesStatusCommand pins the bare `bunker homes` report: managed entries
// are classified stale/kept, the stale set's on-disk size is printed in a human
// unit, the sorted stale names are listed, unmanaged entries are counted but
// never classified, and nothing is mutated.
func TestHomesStatusCommand(t *testing.T) {
	dir := seedHomesDir(t, "bunker-gone", "bunker-live", "notmanaged")
	// 1024 bytes inside the stale home: the report must print its size.
	if err := os.WriteFile(filepath.Join(dir, "bunker-gone", "payload"), make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := homesRootPath
	homesRootPath = dir
	t.Cleanup(func() { homesRootPath = orig })
	stubHomeUserExists(t, map[string]bool{"bunker-live": true})

	out, err := runHomesGroup(t)
	if err != nil {
		t.Fatalf("homes execute: %v (output: %s)", err, out)
	}
	for _, want := range []string{
		"homes status: " + dir,
		"entries scanned: 2",
		"stale (user gone): 1",
		"kept (live user): 1",
		"stale size: 1.0 KB",
		"unmanaged (ignored): 1",
		"    bunker-gone",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q; output:\n%s", want, out)
		}
	}
	// An unmanaged entry is never classified.
	if strings.Contains(out, "notmanaged") {
		t.Errorf("unmanaged entries must not be classified; output:\n%s", out)
	}
	if got := homesNames(t, dir); len(got) != 3 {
		t.Errorf("status must be read-only; entries = %v", got)
	}
}

// TestHomesStatusCommand_HonoursDirFlag proves the --dir flag wins over the
// seam default: the var points at an unrelated (empty) root, the flag at the
// fixture.
func TestHomesStatusCommand_HonoursDirFlag(t *testing.T) {
	dir := seedHomesDir(t, "bunker-gone")
	orig := homesRootPath
	homesRootPath = t.TempDir() // empty: a report from here would find nothing
	t.Cleanup(func() { homesRootPath = orig })
	stubHomeUserExists(t, nil)

	out, err := runHomesGroup(t, "--dir", dir)
	if err != nil {
		t.Fatalf("homes execute: %v (output: %s)", err, out)
	}
	for _, want := range []string{"homes status: " + dir, "stale (user gone): 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q; output:\n%s", want, out)
		}
	}
}

// TestHomesStatusCommand_MissingDir covers the clean no-op for the report.
func TestHomesStatusCommand_MissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-home-root")
	orig := homesRootPath
	homesRootPath = missing
	t.Cleanup(func() { homesRootPath = orig })

	out, err := runHomesGroup(t)
	if err != nil {
		t.Fatalf("homes execute: %v (output: %s)", err, out)
	}
	if !strings.Contains(out, "does not exist; nothing to report") {
		t.Errorf("missing root must print the no-op message; output:\n%s", out)
	}
	if !strings.Contains(out, missing) {
		t.Errorf("the no-op must name the root; output:\n%s", out)
	}
}

// TestHomesStatusCommand_UnreadableRoot pins the error discipline: a root that
// cannot be READ is an error naming the path and the cause, never a zero report
// (a directory path that is actually a file yields ENOTDIR for any user).
func TestHomesStatusCommand_UnreadableRoot(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "home-root-file")
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := homesRootPath
	homesRootPath = notADir
	t.Cleanup(func() { homesRootPath = orig })

	out, err := runHomesGroup(t)
	if err == nil {
		t.Fatalf("an unreadable root must fail, not report zero; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), notADir) {
		t.Errorf("error must name the path, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error must name the cause, got: %v", err)
	}
}

// TestHomesPrune_RemovesOnlyStaleEntries is the core contract: against a seeded
// fixture and a stubbed user-existence check, prune removes exactly the managed
// entries whose user no longer exists and keeps every entry whose user exists —
// including a live agent user — and never touches an unmanaged directory.
func TestHomesPrune_RemovesOnlyStaleEntries(t *testing.T) {
	dir := seedHomesDir(t, "bunker-gone-one", "bunker-gone-two", "bunker-live-agent", "notmanaged")
	stubHomeUserExists(t, map[string]bool{"bunker-live-agent": true})

	out := runHomesPrune(t, dir)

	got := homesNames(t, dir)
	want := []string{"bunker-live-agent", "notmanaged"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("entries after prune = %v, want %v (live users and unmanaged dirs must survive)", got, want)
	}
	for _, want := range []string{
		"scanned: 3",
		"stale removed: 2",
		"kept (live user): 1",
		"entries remaining: 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prune output missing %q; output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "dry run") {
		t.Errorf("real prune must not print the dry-run marker; output:\n%s", out)
	}
}

// TestHomesPrune_DryRunMutatesNothing is the dry-run half: the same fixture,
// --dry-run set, the stale entries are REPORTED but every entry is still on
// disk afterwards.
func TestHomesPrune_DryRunMutatesNothing(t *testing.T) {
	dir := seedHomesDir(t, "bunker-gone-one", "bunker-gone-two", "bunker-live")
	stubHomeUserExists(t, map[string]bool{"bunker-live": true})

	out := runHomesPrune(t, dir, "--dry-run")

	got := homesNames(t, dir)
	if len(got) != 3 {
		t.Errorf("dry run must mutate nothing; entries = %v, want all 3", got)
	}
	for _, want := range []string{
		"(dry run, no changes written)",
		"scanned: 3",
		"stale entries (would remove): 2",
		"kept (live user): 1",
		"    bunker-gone-one",
		"    bunker-gone-two",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q; output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "entries remaining") {
		t.Errorf("dry run must not claim a post-prune state; output:\n%s", out)
	}
}

// TestHomesPrune_InconclusiveLookupRemovesNothing proves fail-closed
// classification: when the existence check itself ERRORS (not "user absent"),
// the scan aborts and no entry is removed.
func TestHomesPrune_InconclusiveLookupRemovesNothing(t *testing.T) {
	dir := seedHomesDir(t, "bunker-gone", "bunker-live")
	orig := homeUserExists
	homeUserExists = func(string) (bool, error) {
		return false, errors.New(" NSS stub unavailable")
	}
	t.Cleanup(func() { homeUserExists = orig })

	cmd := newHomesPruneCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--dir", dir})
	if err := cmd.Execute(); err == nil {
		t.Fatal("an errored existence probe must fail the prune, not silently keep scanning")
	}
	if got := homesNames(t, dir); len(got) != 2 {
		t.Errorf("inconclusive probe must remove nothing; entries = %v", got)
	}
}

// TestHomesPrune_IgnoresUnmanagedNames pins the name filter: entries that do not
// match bunker-* are never classified, so prune removes nothing and exits 0.
func TestHomesPrune_IgnoresUnmanagedNames(t *testing.T) {
	dir := seedHomesDir(t, "notmanaged", "bunker", "bunkie-x", "bunker-")
	stubHomeUserExists(t, nil) // even a lookup that resolves nothing

	out := runHomesPrune(t, dir)

	got := homesNames(t, dir)
	if len(got) != 4 {
		t.Errorf("unmanaged entries must survive; entries = %v, want all 4", got)
	}
	for _, want := range []string{"scanned: 0", "stale removed: 0", "kept (live user): 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("prune output missing %q; output:\n%s", want, out)
		}
	}
}

// TestHomesPrune_MissingDirIsCleanNoOp pins the registry-compact-style no-op: a
// missing home root exits 0 with a clear message.
func TestHomesPrune_MissingDirIsCleanNoOp(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-home-root")
	// The prune command's early os.Stat check makes this a no-op before any
	// seam is consulted; the production existence seam is fine here.
	out := runHomesPrune(t, missing)
	if !strings.Contains(out, "does not exist; nothing to prune") {
		t.Errorf("missing root must print the no-op message; output:\n%s", out)
	}
}

// TestHomesPrune_KeepsUnreadableHomeDeletable proves a stale home whose size
// cannot be measured is still removed: the size is report-only, so an
// unreadable home (the residue an operator most wants gone) never blocks the
// prune. Skipped as root, where mode 000 does not deny the read.
func TestHomesPrune_KeepsUnreadableHomeDeletable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000 directory")
	}
	dir := seedHomesDir(t, "bunker-gone")
	nested := filepath.Join(dir, "bunker-gone", "locked")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(nested, 0o755) })
	stubHomeUserExists(t, nil)

	statusOut, err := runHomesGroup(t, "--dir", dir)
	if err != nil {
		t.Fatalf("homes execute: %v (output: %s)", err, statusOut)
	}
	if !strings.Contains(statusOut, "size unavailable for: bunker-gone") {
		t.Errorf("an unmeasurable home must be named, not silently counted as 0 B; output:\n%s", statusOut)
	}

	out := runHomesPrune(t, dir)
	if got := homesNames(t, dir); len(got) != 0 {
		t.Errorf("an unmeasurable stale home must still be pruned; entries = %v", got)
	}
	if !strings.Contains(out, "stale removed: 1") {
		t.Errorf("prune output missing the removal count; output:\n%s", out)
	}
}

// TestPruneHomesRoot_StopsAtFirstRemoveFailure keeps the partial-progress
// contract honest at the helper level: a failed removal is returned with the
// count of entries removed so far (no silent swallowing).
func TestPruneHomesRoot_StopsAtFirstRemoveFailure(t *testing.T) {
	dir := seedHomesDir(t, "bunker-gone-a", "bunker-gone-b")
	stubHomeUserExists(t, nil) // nothing resolves: both entries are stale
	orig := removeHomeDir
	realRemove := orig
	removeHomeDir = func(path string) error {
		if strings.HasSuffix(path, "bunker-gone-a") {
			return realRemove(path) // succeed for real
		}
		return errors.New("device or resource busy")
	}
	t.Cleanup(func() { removeHomeDir = orig })

	scan, removed, err := pruneHomesRoot(dir, false)
	if err == nil {
		t.Fatal("expected the removal failure to surface")
	}
	if !strings.Contains(err.Error(), "bunker-gone-b") || !strings.Contains(err.Error(), "device or resource busy") {
		t.Errorf("error must name the entry and cause, got: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (bunker-gone-a succeeded before bunker-gone-b failed)", removed)
	}
	if scan == nil || len(scan.Stale) != 2 {
		t.Errorf("scan = %+v, want 2 stale entries", scan)
	}
	if got := homesNames(t, dir); len(got) != 1 || got[0] != "bunker-gone-b" {
		t.Errorf("entries after failed prune = %v, want [bunker-gone-b]", got)
	}
}
