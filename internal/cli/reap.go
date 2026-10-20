package cli

// INT-HOST-007: one host-residue reaper covering the whole orphan-class
// failure mode as ONE verb with a DRY-RUN default. Every agent spawn leaves
// three kinds of host state behind — an AppArmor profile file, a home
// directory and a systemd linger entry — and every leak path (cancelled
// spawn, rolled-back destroy, userdel without -r) can orphan any of them
// independently (INT-CI-007/INT-HOST-001 linger, GAP-080 homes, INT-HOST-006
// AppArmor). `bunker reap` scans all three surfaces in one pass and reports
// per class: entries it WOULD remove (the user is gone), entries it KEPT
// (the user still exists), and unmanaged entries it never classifies.
//
// Removal is OFF unless --apply is passed explicitly, and an owner decision
// is required for it — the dry run is the default so a bare `bunker reap`
// can never delete host state.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// reapDefaultProfileDir is the directory ensureRootlesskitAppArmor installs
// profiles into (internal/agent/rootless.go: profilePath =
// filepath.Join("/etc/apparmor.d", profileName)). Mirrored here as a local
// constant rather than imported: internal/agent is daemon-side, and the
// reaper is a local-only CLI verb that must not drag server internals into
// the cli package. The naming rule this implies (home.<user>.bin.rootlesskit)
// is mirrored by reapProfileUser below.
const reapDefaultProfileDir = "/etc/apparmor.d"

// reapProfileDirPath is the directory the reap subcommand scans. Var (not
// const) so tests can point it at t.TempDir(); production code never swaps
// it (the --profile-dir flag overrides it at RunE, the homes.go resolution
// pattern).
var reapProfileDirPath = reapDefaultProfileDir

// reapProfileSuffixText is the trailing part of a managed rootlesskit
// AppArmor profile filename: the spawn-side source of truth
// (internal/agent/rootless.go ensureRootlesskitAppArmor) writes
// fmt.Sprintf("home.%s.bin.rootlesskit", username) where username is the
// bunker-<id> agent user. The <user> this verb parses out is therefore the
// FULL agent user name including the bunker- prefix — the same name `homes`
// and `linger` classify — so all three classes agree on exactly one
// predicate: does this user exist.
const reapProfileSuffixText = ".bin.rootlesskit"

// reapProfilePrefix is the leading part of a managed profile filename.
const reapProfilePrefix = "home."

// reapUserExists reports whether the named system user exists. Package-level
// seam: tests inject a fake so scanning never inspects the ambient
// /etc/passwd. Production code never swaps it (production resolves through
// os/user, whose lookup covers /etc/passwd and the configured NSS sources).
//
// The predicate is shared by all three classes and matches the existing
// homes/linger convention: a lookup MISS (unknown user) is the removable
// class; a lookup ERROR is inconclusive, not "absent", so the whole reap
// aborts fail-closed before anything would be removed.
var reapUserExists = func(name string) (bool, error) {
	exists, err := homeUserExists(name)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil // unknown user: the reaper's target class
	}
	return true, nil
}

// reapRemove removes one host-residue entry (a profile file, a home
// directory or a linger file). Package-level seam for tests; production uses
// the per-class removers the homes/linger prunes already use (os.RemoveAll /
// os.Remove), so the deletion semantics never diverge from the single-class
// verbs.
var reapRemove = func(path string, isDir bool) error {
	if isDir {
		return removeHomeDir(path)
	}
	return removeLingerEntry(path)
}

// reapClass is the scan result for one residue class.
type reapClass struct {
	// WouldRemove names the managed entries whose user no longer exists
	// (sorted). Nothing is deleted from this set without --apply.
	WouldRemove []string
	// Kept names the managed entries whose user still exists (sorted) —
	// NEVER removable.
	Kept []string
	// Unmanaged counts entries in the scanned directory that do not match
	// the class's managed naming pattern. They are ignored: a reap never
	// classifies or removes them.
	Unmanaged int
}

// reapScan is the result of one full host-residue scan.
type reapScan struct {
	Profiles reapClass
	Homes    reapClass
	Linger   reapClass
}

// reapProfileUser extracts the user from a managed profile filename
// (home.<user>.bin.rootlesskit): everything between the leading "home." and
// the trailing ".bin.rootlesskit". The suffix is stripped, never the
// "bunker-" prefix — the looked-up name is the full agent user (bunker-<id>),
// identical to the homes/linger name.
func reapProfileUser(filename string) (string, bool) {
	rest, ok := strings.CutPrefix(filename, reapProfilePrefix)
	if !ok {
		return "", false
	}
	user, ok := strings.CutSuffix(rest, reapProfileSuffixText)
	if !ok || user == "" {
		return "", false
	}
	return user, true
}

// reapScanProfiles classifies the rootlesskit profile files in dir. Files
// matching home.<user>.bin.rootlesskit are managed; everything else —
// including subdirectories and unrelated policy files — is unmanaged.
func reapScanProfiles(dir string) (reapClass, error) {
	var class reapClass
	entries, err := os.ReadDir(dir)
	if err != nil {
		return class, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue // AppArmor fragments are files; a subdirectory is unmanaged space
		}
		user, ok := reapProfileUser(e.Name())
		if !ok {
			class.Unmanaged++
			continue
		}
		exists, err := reapUserExists(user)
		if err != nil {
			return reapClass{}, fmt.Errorf("check user %q: %w", user, err)
		}
		if exists {
			class.Kept = append(class.Kept, e.Name())
			continue
		}
		class.WouldRemove = append(class.WouldRemove, e.Name())
	}
	sort.Strings(class.WouldRemove)
	sort.Strings(class.Kept)
	return class, nil
}

// reapScanHomes classifies the agent home directories in root through the
// homes package's own scan (managed = bunker-<something>, human homes and
// unrelated directories are unmanaged), reusing its user-existence predicate.
func reapScanHomes(root string) (reapClass, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return reapClass{}, err
	}
	var class reapClass
	for _, e := range entries {
		if !managedHomeName(e.Name()) {
			class.Unmanaged++
			continue
		}
		exists, err := reapUserExists(e.Name())
		if err != nil {
			return reapClass{}, fmt.Errorf("check user %q: %w", e.Name(), err)
		}
		if exists {
			class.Kept = append(class.Kept, e.Name())
			continue
		}
		class.WouldRemove = append(class.WouldRemove, e.Name())
	}
	sort.Strings(class.WouldRemove)
	sort.Strings(class.Kept)
	return class, nil
}

// reapScanLinger classifies the systemd linger entries in dir. Every entry in
// the linger directory is a managed user file by construction (systemd
// creates them only from `loginctl enable-linger <user>`), so the linger
// convention applies wholesale: a name whose user is gone is removable, one
// whose user remains is kept.
func reapScanLinger(dir string) (reapClass, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return reapClass{}, err
	}
	var class reapClass
	for _, e := range entries {
		exists, err := reapUserExists(e.Name())
		if err != nil {
			return reapClass{}, fmt.Errorf("check user %q: %w", e.Name(), err)
		}
		if exists {
			class.Kept = append(class.Kept, e.Name())
			continue
		}
		class.WouldRemove = append(class.WouldRemove, e.Name())
	}
	sort.Strings(class.WouldRemove)
	sort.Strings(class.Kept)
	return class, nil
}

// reapScanHost runs all three class scans against their host directories.
func reapScanHost(profileDir, homesRoot, lingerDir string) (*reapScan, error) {
	profiles, err := reapScanProfiles(profileDir)
	if err != nil {
		return nil, fmt.Errorf("scan apparmor profiles in %s: %w", profileDir, err)
	}
	homes, err := reapScanHomes(homesRoot)
	if err != nil {
		return nil, fmt.Errorf("scan agent homes in %s: %w", homesRoot, err)
	}
	linger, err := reapScanLinger(lingerDir)
	if err != nil {
		return nil, fmt.Errorf("scan linger entries in %s: %w", lingerDir, err)
	}
	return &reapScan{Profiles: profiles, Homes: homes, Linger: linger}, nil
}

// reapClassIsDir reports whether a class's entries are directories (homes)
// or files (profiles, linger) — the reapRemove seam's discriminator.
func reapClassIsDir(class string) bool {
	return class == "homes"
}

// reapApply removes every would-remove entry of every class. It runs only
// behind the explicit --apply flag; paths are joined inside so the seam never
// sees a caller-assembled path.
func reapApply(scan *reapScan, profileDir, homesRoot, lingerDir string) (int, error) {
	removed := 0
	classes := []struct {
		name  string
		root  string
		class reapClass
	}{
		{"apparmor profiles", profileDir, scan.Profiles},
		{"agent homes", homesRoot, scan.Homes},
		{"linger entries", lingerDir, scan.Linger},
	}
	for _, c := range classes {
		for _, name := range c.class.WouldRemove {
			if err := reapRemove(filepath.Join(c.root, name), reapClassIsDir(c.name)); err != nil {
				return removed, fmt.Errorf("remove %s %s: %w", c.name, name, err)
			}
			removed++
		}
	}
	return removed, nil
}

// reapClassMaxNames bounds the per-class name list in the dry-run report: a
// host with thousands of orphans must not flood the terminal.
const reapClassMaxNames = 20

// printReapClass renders one class's counts and, in dry-run mode, the names
// it would remove (bounded: the first reapClassMaxNames, then a remainder
// line).
func printReapClass(out io.Writer, label, root string, class reapClass, dryRun bool) {
	_, _ = fmt.Fprintf(out, "%s: %s\n", label, root)
	if dryRun {
		_, _ = fmt.Fprintf(out, "  would remove (user gone): %d\n", len(class.WouldRemove))
	}
	_, _ = fmt.Fprintf(out, "  kept (live user): %d\n", len(class.Kept))
	if class.Unmanaged > 0 {
		_, _ = fmt.Fprintf(out, "  unmanaged (ignored): %d\n", class.Unmanaged)
	}
	for i, name := range class.WouldRemove {
		if i == reapClassMaxNames {
			_, _ = fmt.Fprintf(out, "  ... and %d more\n", len(class.WouldRemove)-reapClassMaxNames)
			break
		}
		_, _ = fmt.Fprintf(out, "    %s\n", name)
	}
}

// printReapReport renders the full report. Dry run marks the whole report
// no-changes and names the --apply opt-in; an applied run reports the removal
// count instead of "would remove".
func printReapReport(out io.Writer, scan *reapScan, profileDir, homesRoot, lingerDir string, dryRun bool) {
	if dryRun {
		_, _ = fmt.Fprintf(out, "reap: DRY RUN (no changes written; pass --apply to remove)\n")
	} else {
		_, _ = fmt.Fprintf(out, "reap: applied\n")
	}
	printReapClass(out, "apparmor profiles", profileDir, scan.Profiles, dryRun)
	printReapClass(out, "agent homes", homesRoot, scan.Homes, dryRun)
	printReapClass(out, "linger entries", lingerDir, scan.Linger, dryRun)
}

// NewReapCommand returns the `bunker reap` command (INT-HOST-007): one
// host-residue scan across orphan AppArmor profiles, orphan agent homes and
// stale systemd linger entries. Dry run is the DEFAULT; --apply is the
// explicit opt-in that deletes host state and requires an owner decision.
//
// Flag/seam shape (the homes.go convention): every scanned root defaults to
// its package-level seam var and overrides it only when the flag was passed
// (--profile-dir/--homes-dir/--linger-dir), so tests drive the REAL scan
// path by pointing the vars at t.TempDir() and never inspect ambient host
// directories.
func NewReapCommand() *cobra.Command {
	var (
		profileDir string
		homesDir   string
		lingerDir  string
		apply      bool
	)
	cmd := &cobra.Command{
		Use:   "reap",
		Short: "Report (and with --apply remove) orphaned AppArmor profiles, agent homes and linger entries",
		Long: `Scan host residue left by agent lifecycles and reap it on request.

Every agent spawn leaves three kinds of host state: an AppArmor profile
(/etc/apparmor.d/home.<user>.bin.rootlesskit), a home directory
(/home/bunker-<id>) and a systemd linger entry (/var/lib/systemd/linger/<user>).
When an agent's user disappears without its cleanup running — cancelled spawn
past the deadline, rolled-back destroy, userdel without -r — the state
outlives the user and accumulates (18,043 orphan profiles and ~610/620 orphan
homes measured on the live-E2E host).

DRY RUN IS THE DEFAULT: with no flags ` + "`bunker reap`" + ` scans all three
surfaces and prints, per class, how many entries it WOULD remove (user gone),
how many it keeps (user still exists) and how many are unmanaged. Nothing is
removed.

--apply removes the would-remove entries. THIS DELETES HOST STATE and
requires an owner decision before running. The classification rule is exactly
one predicate for all three classes: does this user exist? An entry whose
user still exists is NEVER removed; entries that do not match the managed
naming pattern are never classified; an inconclusive user lookup aborts
before anything is removed.

Local-only: like ` + "`bunker homes`" + ` and ` + "`bunker linger`" + `, this inspects THIS host's
own directories. Run it on the host that owns them, as root, so the residue
is actually removable.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			pdir, hroot, ldir := reapProfileDirPath, homesRootPath, lingerDirPath
			if cmd.Flags().Changed("profile-dir") {
				pdir = profileDir
			}
			if cmd.Flags().Changed("homes-dir") {
				hroot = homesDir
			}
			if cmd.Flags().Changed("linger-dir") {
				ldir = lingerDir
			}
			scan, err := reapScanHost(pdir, hroot, ldir)
			if err != nil {
				return err
			}
			removed := 0
			dryRun := !apply
			if apply {
				var err error
				if removed, err = reapApply(scan, pdir, hroot, ldir); err != nil {
					return err
				}
			}
			printReapReport(out, scan, pdir, hroot, ldir, dryRun)
			if !dryRun {
				_, _ = fmt.Fprintf(out, "  removed: %d\n", removed)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&profileDir, "profile-dir", reapProfileDirPath, "AppArmor profile directory to scan")
	cmd.Flags().StringVar(&homesDir, "homes-dir", homesRootPath, "Agent home root to scan")
	cmd.Flags().StringVar(&lingerDir, "linger-dir", lingerDirPath, "Systemd linger directory to scan")
	cmd.Flags().BoolVar(&apply, "apply", false, "Remove the orphan entries (DELETES HOST STATE — requires an owner decision; default is a dry run)")
	return cmd
}
