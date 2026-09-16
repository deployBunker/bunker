package cli

import (
	"bytes"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the INT-HOST-001 CLI surface: `bunker linger` reports the
// total/live/stale ratio read-only, `bunker linger prune` removes exactly the
// entries whose user no longer exists (never an entry whose user exists,
// including root), --dry-run mutates nothing, and a missing linger directory
// is a clean no-op.

// seedLingerDir creates a linger fixture directory with the given entry names.
func seedLingerDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// lingerNames returns the sorted entry names currently in dir.
func lingerNames(t *testing.T, dir string) []string {
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

// stubLingerUserExists swaps the user-existence seam; exists names the users
// that resolve. Restored via t.Cleanup.
func stubLingerUserExists(t *testing.T, exists map[string]bool) {
	t.Helper()
	orig := lingerUserExists
	lingerUserExists = func(name string) (bool, error) {
		if v, ok := exists[name]; ok {
			return v, nil
		}
		return false, nil
	}
	t.Cleanup(func() { lingerUserExists = orig })
}

// runPrune executes the real prune command against dir and returns its output.
func runPrune(t *testing.T, dir string, extraArgs ...string) string {
	t.Helper()
	cmd := newLingerPruneCommand()
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

// TestLingerCommandSurface pins the CLI shape (registry compact precedent):
// a `linger` group with a `prune` subcommand exposing --dir and --dry-run.
func TestLingerCommandSurface(t *testing.T) {
	cmd := NewLingerCommand()
	if cmd.Use != "linger" {
		t.Errorf("Use = %q, want linger", cmd.Use)
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
		t.Fatal("linger must have a prune subcommand")
	}
	if defaultLingerDir != "/var/lib/systemd/linger" {
		t.Errorf("default linger dir = %q, want /var/lib/systemd/linger", defaultLingerDir)
	}
}

// TestLingerPrune_RemovesOnlyStaleEntries is AC3: against a seeded fixture
// directory and a stubbed user-existence check, prune removes exactly the
// entries whose user no longer exists and keeps every entry whose user
// exists — including root and live agent users.
func TestLingerPrune_RemovesOnlyStaleEntries(t *testing.T) {
	dir := seedLingerDir(t, "bunker-gone-one", "bunker-gone-two", "root", "bunker-live-agent", "sshd")
	stubLingerUserExists(t, map[string]bool{
		"root":              true,
		"bunker-live-agent": true,
		"sshd":              true,
	})

	out := runPrune(t, dir)

	got := lingerNames(t, dir)
	want := []string{"bunker-live-agent", "root", "sshd"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("entries after prune = %v, want %v (live users must survive)", got, want)
	}
	for _, want := range []string{
		"scanned: 5",
		"stale removed: 2",
		"kept (live user): 3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prune output missing %q; output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "dry run") {
		t.Errorf("real prune must not print the dry-run marker; output:\n%s", out)
	}
}

// TestLingerPrune_DryRunMutatesNothing is the dry-run half of AC3: the same
// fixture, --dry-run set, the stale entries are REPORTED but every entry is
// still on disk afterwards.
func TestLingerPrune_DryRunMutatesNothing(t *testing.T) {
	dir := seedLingerDir(t, "bunker-gone-one", "bunker-gone-two", "root")
	stubLingerUserExists(t, map[string]bool{"root": true})

	out := runPrune(t, dir, "--dry-run")

	got := lingerNames(t, dir)
	if len(got) != 3 {
		t.Errorf("dry run must mutate nothing; entries = %v, want all 3", got)
	}
	for _, want := range []string{
		"(dry run, no changes written)",
		"scanned: 3",
		"stale entries (would remove): 2",
		"kept (live user): 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q; output:\n%s", want, out)
		}
	}
}

// TestLingerPrune_RootSurvivesWithRealUserLookup runs the prune with the
// PRODUCTION user-existence seam against a fixture that contains only root:
// the real /etc/passwd lookup must classify root as live and remove nothing.
func TestLingerPrune_RootSurvivesWithRealUserLookup(t *testing.T) {
	if _, err := user.Lookup("root"); err != nil {
		t.Skip("no root user in this environment")
	}
	dir := seedLingerDir(t, "root")

	out := runPrune(t, dir)

	if got := lingerNames(t, dir); len(got) != 1 || got[0] != "root" {
		t.Errorf("root entry must never be pruned; entries = %v", got)
	}
	if !strings.Contains(out, "stale removed: 0") || !strings.Contains(out, "kept (live user): 1") {
		t.Errorf("unexpected report for root-only fixture; output:\n%s", out)
	}
}

// TestLingerPrune_MissingDirIsCleanNoOp pins the registry-compact-style
// no-op: a missing linger directory exits 0 with a clear message.
func TestLingerPrune_MissingDirIsCleanNoOp(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-linger")
	// The prune command's early os.Stat check makes this a no-op before any
	// seam is consulted; the production existence seam is fine here.
	out := runPrune(t, missing)
	if !strings.Contains(out, "does not exist; nothing to prune") {
		t.Errorf("missing dir must print the no-op message; output:\n%s", out)
	}
}

// TestLingerPrune_InconclusiveLookupRemovesNothing proves fail-closed
// classification: when the existence check itself ERRORS (not "user absent"),
// the scan aborts and no entry is removed.
func TestLingerPrune_InconclusiveLookupRemovesNothing(t *testing.T) {
	dir := seedLingerDir(t, "bunker-gone", "root")
	orig := lingerUserExists
	lingerUserExists = func(string) (bool, error) {
		return false, errors.New(" NSS stub unavailable")
	}
	t.Cleanup(func() { lingerUserExists = orig })

	cmd := newLingerPruneCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--dir", dir})
	if err := cmd.Execute(); err == nil {
		t.Fatal("an errored existence probe must fail the prune, not silently keep scanning")
	}
	if got := lingerNames(t, dir); len(got) != 2 {
		t.Errorf("inconclusive probe must remove nothing; entries = %v", got)
	}
}

// TestPruneLingerDir_StopsAtFirstRemoveFailure keeps the partial-progress
// contract honest at the helper level: a failed removal is returned with the
// count of entries removed so far (no silent swallowing).
func TestPruneLingerDir_StopsAtFirstRemoveFailure(t *testing.T) {
	dir := seedLingerDir(t, "gone-a", "gone-b")
	stubLingerUserExists(t, nil) // nothing resolves: both entries are stale
	orig := removeLingerEntry
	realRemove := orig
	removeLingerEntry = func(path string) error {
		if strings.HasSuffix(path, "gone-a") {
			return realRemove(path) // succeed for real
		}
		return errors.New("read-only filesystem")
	}
	t.Cleanup(func() { removeLingerEntry = orig })

	scan, removed, err := pruneLingerDir(dir, false)
	if err == nil {
		t.Fatal("expected the removal failure to surface")
	}
	if !strings.Contains(err.Error(), "gone-b") || !strings.Contains(err.Error(), "read-only filesystem") {
		t.Errorf("error must name the entry and cause, got: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (gone-a succeeded before gone-b failed)", removed)
	}
	if scan == nil || len(scan.Stale) != 2 {
		t.Errorf("scan = %+v, want 2 stale entries", scan)
	}
	if got := lingerNames(t, dir); len(got) != 1 || got[0] != "gone-b" {
		t.Errorf("entries after failed prune = %v, want [gone-b]", got)
	}
}

// TestLingerStatusCommand pins the bare `bunker linger` diagnostic ratio
// (read-only, reachable without root mutation).
func TestLingerStatusCommand(t *testing.T) {
	dir := seedLingerDir(t, "bunker-gone", "root", "bunker-live")
	orig := lingerDirPath
	lingerDirPath = dir
	t.Cleanup(func() { lingerDirPath = orig })
	stubLingerUserExists(t, map[string]bool{"root": true, "bunker-live": true})

	cmd := NewLingerCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("linger execute: %v (output: %s)", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"linger status:",
		"total entries: 3",
		"live users: 2",
		"stale (user gone): 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q; output:\n%s", want, out)
		}
	}
	if got := lingerNames(t, dir); len(got) != 3 {
		t.Errorf("status must be read-only; entries = %v", got)
	}
}

// TestLingerStatusMissingDir covers the clean no-op for the ratio report.
func TestLingerStatusMissingDir(t *testing.T) {
	orig := lingerDirPath
	lingerDirPath = filepath.Join(t.TempDir(), "nope")
	t.Cleanup(func() { lingerDirPath = orig })

	cmd := NewLingerCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("linger execute: %v", err)
	}
	if !strings.Contains(buf.String(), "does not exist; nothing to report") {
		t.Errorf("missing dir must print the no-op message; output:\n%s", buf.String())
	}
}
