package cli

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"
)

// defaultLingerDir is the systemd linger directory. Like `bunker registry
// compact` and `bunker audit`, linger maintenance runs LOCALLY on the host
// that owns the directory — /var/lib/systemd/linger is not addressable over
// the agent wire protocol.
const defaultLingerDir = "/var/lib/systemd/linger"

// lingerDirPath is the directory the linger subcommands operate on. Var (not
// const) so tests can point it at t.TempDir(); production code never swaps it.
var lingerDirPath = defaultLingerDir

// lingerUserExists reports whether the named system user exists. Package-level
// seam: tests inject a fake so pruning never inspects the ambient /etc/passwd.
// Production code never swaps it (production resolves through os/user, whose
// lookup covers /etc/passwd and the configured NSS sources).
var lingerUserExists = func(name string) (bool, error) {
	if _, err := user.Lookup(name); err != nil {
		return false, nil // unknown user: the prune's target class
	}
	return true, nil
}

// removeLingerEntry removes a single linger file. Package-level seam for
// tests: production uses os.Remove.
var removeLingerEntry = func(path string) error {
	return os.Remove(path)
}

// lingerScan is the result of one prune scan.
type lingerScan struct {
	// Scanned is the number of entries found in the linger directory.
	Scanned int
	// Stale names the entries whose user no longer exists (in sorted order).
	Stale []string
	// Kept names the entries whose user still exists (in sorted order).
	Kept []string
}

// scanLingerDir reads dir and classifies every entry as stale (user gone) or
// kept (user still exists). A directory that cannot be read returns an error;
// an empty directory yields a zero scan.
func scanLingerDir(dir string) (*lingerScan, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	scan := &lingerScan{Scanned: len(entries)}
	for _, e := range entries {
		exists, err := lingerUserExists(e.Name())
		if err != nil {
			// The existence check itself failed (not "user absent"):
			// give up without removing anything — never remove on an
			// inconclusive probe.
			return nil, fmt.Errorf("check user %q: %w", e.Name(), err)
		}
		if exists {
			scan.Kept = append(scan.Kept, e.Name())
		} else {
			scan.Stale = append(scan.Stale, e.Name())
		}
	}
	sort.Strings(scan.Stale)
	sort.Strings(scan.Kept)
	return scan, nil
}

// pruneLingerDir removes every stale entry from dir (unless dryRun) and
// returns the scan it acted on plus the number of entries actually removed.
func pruneLingerDir(dir string, dryRun bool) (*lingerScan, int, error) {
	scan, err := scanLingerDir(dir)
	if err != nil {
		return nil, 0, err
	}
	if dryRun {
		return scan, 0, nil
	}
	removed := 0
	for _, name := range scan.Stale {
		if err := removeLingerEntry(filepath.Join(dir, name)); err != nil {
			return scan, removed, fmt.Errorf("remove linger entry %s: %w", name, err)
		}
		removed++
	}
	return scan, removed, nil
}

// printLingerScan renders one scan in the registry-compact report style.
// dryRun adds the explicit no-changes marker, mirroring
// `registry: <path> (dry run, no changes written)`.
func printLingerScan(out io.Writer, dir string, scan *lingerScan, removed int, dryRun bool) {
	if dryRun {
		fmt.Fprintf(out, "linger: %s (dry run, no changes written)\n", dir)
	} else {
		fmt.Fprintf(out, "linger pruned: %s\n", dir)
	}
	fmt.Fprintf(out, "  scanned: %d\n", scan.Scanned)
	if dryRun {
		fmt.Fprintf(out, "  stale entries (would remove): %d\n", len(scan.Stale))
	} else {
		fmt.Fprintf(out, "  stale removed: %d\n", removed)
	}
	fmt.Fprintf(out, "  kept (live user): %d\n", len(scan.Kept))
}

// NewLingerCommand returns the `bunker linger` command group for inspecting
// and maintaining the systemd linger directory (INT-HOST-001). Running the
// bare group prints the diagnostic ratio (total entries vs users that still
// exist); `prune` removes entries whose user is gone.
func NewLingerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "linger",
		Short: "Inspect and maintain the systemd linger directory",
		Long: `Inspect and maintain /var/lib/systemd/linger.

Every agent spawn enables systemd linger for its per-agent user. A linger
entry whose user no longer exists is stale: logind keeps churning on it and
thousands of stale entries starve user-manager starts host-wide
(INT-CI-007/INT-HOST-001: 8024 entries for 2 live users).

Run with no subcommand to report the total/live/stale ratio (read-only).
prune removes exactly the stale entries; it never removes an entry whose
user still exists.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLingerStatus(cmd.OutOrStdout(), lingerDirPath)
		},
	}
	cmd.AddCommand(newLingerPruneCommand())
	return cmd
}

// runLingerStatus prints the operator ratio: total entries, entries whose
// user still exists, and stale entries. It is read-only.
func runLingerStatus(out io.Writer, dir string) error {
	scan, err := scanLingerDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "linger dir %s does not exist; nothing to report\n", dir)
			return nil
		}
		return fmt.Errorf("scan linger dir: %w", err)
	}
	fmt.Fprintf(out, "linger status: %s\n", dir)
	fmt.Fprintf(out, "  total entries: %d\n", scan.Scanned)
	fmt.Fprintf(out, "  live users: %d\n", len(scan.Kept))
	fmt.Fprintf(out, "  stale (user gone): %d\n", len(scan.Stale))
	if len(scan.Stale) > 0 {
		fmt.Fprintf(out, "  run `bunker linger prune` to remove them (use --dry-run first)\n")
	}
	return nil
}

// newLingerPruneCommand implements `bunker linger prune`, mirroring
// `bunker registry compact` (out io.Writer, --dry-run reporting what WOULD
// change, clean no-op on a missing target).
func newLingerPruneCommand() *cobra.Command {
	var (
		dir    string
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove linger entries whose user no longer exists",
		Long: `Remove stale systemd linger entries.

An entry is stale when its user no longer exists (user lookup fails).
Entries whose user still exists — root, service accounts, live agents —
are NEVER removed. A missing linger directory is a clean no-op.

Before/after counts are printed; --dry-run reports what would change.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				fmt.Fprintf(out, "linger dir %s does not exist; nothing to prune\n", dir)
				return nil
			}
			scan, removed, err := pruneLingerDir(dir, dryRun)
			if err != nil {
				return fmt.Errorf("prune linger dir: %w", err)
			}
			printLingerScan(out, dir, scan, removed, dryRun)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", defaultLingerDir, "Systemd linger directory (run on the host that owns it)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what pruning would remove without changing anything")
	return cmd
}
