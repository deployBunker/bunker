package cli

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// defaultHomesRoot is the root the agent homes live under. Like `bunker
// linger` and `bunker registry compact`, home maintenance runs LOCALLY on the
// host that owns the directory — /home is not addressable over the agent wire
// protocol.
const defaultHomesRoot = "/home"

// agentHomePrefix is the managed-name pattern for an agent home directory:
// the prefix plus a non-empty remainder (bunker-<agent-id>, the name of the
// system user the spawn created). Anything else — a human account's home, an
// unrelated directory — is unmanaged and is never classified or removed.
const agentHomePrefix = "bunker-"

// homesRootPath is the root the homes subcommands operate on. Var (not const)
// so tests can point it at t.TempDir(); production code never swaps it (the
// --dir flag overrides it at RunE, the paths.go resolution pattern).
var homesRootPath = defaultHomesRoot

// homeUserExists reports whether the named system user exists. Package-level
// seam: tests inject a fake so pruning never inspects the ambient /etc/passwd.
// Production code never swaps it (production resolves through os/user, whose
// lookup covers /etc/passwd and the configured NSS sources).
//
// The name looked up is the DIRECTORY name verbatim: an agent home is
// /home/bunker-<id> and the system user the spawn created carries exactly that
// name. Stripping the prefix here would judge a live agent's home by whether
// an unrelated human account (<id>) exists — the false-positive that removes a
// running agent's home.
var homeUserExists = func(name string) (bool, error) {
	if _, err := user.Lookup(name); err != nil {
		return false, nil // unknown user: the prune's target class
	}
	return true, nil
}

// removeHomeDir removes one orphaned agent home. Package-level seam for tests:
// production uses os.RemoveAll.
var removeHomeDir = func(path string) error {
	return os.RemoveAll(path)
}

// homesScan is the result of one home-root scan.
type homesScan struct {
	// Scanned is the number of MANAGED entries found (bunker-* only).
	Scanned int
	// Unmanaged counts entries the managed-name pattern does not match. They
	// are ignored: a prune never touches them.
	Unmanaged int
	// Stale names the managed entries whose user no longer exists (sorted).
	Stale []string
	// Kept names the managed entries whose user still exists (sorted).
	Kept []string
	// StaleBytes is the on-disk size of the stale set in bytes. It is a LOWER
	// BOUND whenever SizeUnknown is non-empty.
	StaleBytes uint64
	// SizeUnknown names stale entries whose size could not be measured (a home
	// the caller cannot read, for instance). The report says so instead of
	// printing a silent zero, and it NEVER blocks a prune — an unreadable home
	// is exactly the residue an operator wants gone.
	SizeUnknown []string
}

// managedHomeName reports whether name is a managed agent home directory name:
// the bunker- prefix plus a non-empty remainder. `bunker-` alone, `bunker`,
// dotfiles and unrelated directories are unmanaged.
func managedHomeName(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	return len(name) > len(agentHomePrefix) && strings.HasPrefix(name, agentHomePrefix)
}

// homeDirSize returns the on-disk size of path in bytes. filepath.WalkDir does
// not follow symlinks, so a link out of the home root can neither escape the
// walk nor inflate the measurement.
func homeDirSize(path string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil // vanished mid-walk: not a measurement failure
			}
			return err
		}
		total += uint64(info.Size())
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// scanHomesRoot reads root and classifies every managed entry as stale (user
// gone) or kept (user still exists), measuring the stale set's on-disk size.
// It fails ONLY where the classification itself is impossible: an unreadable
// root, or a user-lookup error (inconclusive, as opposed to "user absent").
// A size it cannot measure is recorded in SizeUnknown and does not fail the
// scan.
func scanHomesRoot(root string) (*homesScan, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	scan := &homesScan{}
	for _, e := range entries {
		name := e.Name()
		if !managedHomeName(name) {
			scan.Unmanaged++
			continue
		}
		scan.Scanned++
		exists, err := homeUserExists(name)
		if err != nil {
			// The existence check itself failed (not "user absent"):
			// give up without removing anything — never remove on an
			// inconclusive probe.
			return nil, fmt.Errorf("check user %q: %w", name, err)
		}
		if exists {
			scan.Kept = append(scan.Kept, name)
			continue
		}
		scan.Stale = append(scan.Stale, name)
		size, err := homeDirSize(filepath.Join(root, name))
		if err != nil {
			scan.SizeUnknown = append(scan.SizeUnknown, name)
			continue
		}
		scan.StaleBytes += size
	}
	sort.Strings(scan.Stale)
	sort.Strings(scan.Kept)
	sort.Strings(scan.SizeUnknown)
	return scan, nil
}

// pruneHomesRoot removes every stale entry from root (unless dryRun) and
// returns the scan it acted on plus the number of entries actually removed.
func pruneHomesRoot(root string, dryRun bool) (*homesScan, int, error) {
	scan, err := scanHomesRoot(root)
	if err != nil {
		return nil, 0, err
	}
	if dryRun {
		return scan, 0, nil
	}
	removed := 0
	for _, name := range scan.Stale {
		if err := removeHomeDir(filepath.Join(root, name)); err != nil {
			return scan, removed, fmt.Errorf("remove home %s: %w", name, err)
		}
		removed++
	}
	return scan, removed, nil
}

// printHomesStaleList renders the sorted stale names, one per line, with the
// lower-bound marker when part of the set could not be measured.
func printHomesStaleList(out io.Writer, scan *homesScan) {
	if len(scan.Stale) == 0 {
		return
	}
	fmt.Fprintf(out, "  stale entries:\n")
	for _, name := range scan.Stale {
		fmt.Fprintf(out, "    %s\n", name)
	}
	if len(scan.SizeUnknown) > 0 {
		fmt.Fprintf(out, "  size unavailable for: %s (size is a lower bound)\n", strings.Join(scan.SizeUnknown, ", "))
	}
}

// printHomesStatus renders the read-only report: the root, the managed counts,
// the stale set's on-disk size and the sorted stale names.
func printHomesStatus(out io.Writer, root string, scan *homesScan) {
	fmt.Fprintf(out, "homes status: %s\n", root)
	fmt.Fprintf(out, "  entries scanned: %d\n", scan.Scanned)
	fmt.Fprintf(out, "  stale (user gone): %d\n", len(scan.Stale))
	fmt.Fprintf(out, "  kept (live user): %d\n", len(scan.Kept))
	fmt.Fprintf(out, "  stale size: %s\n", humanBytes(scan.StaleBytes))
	if scan.Unmanaged > 0 {
		fmt.Fprintf(out, "  unmanaged (ignored): %d\n", scan.Unmanaged)
	}
	printHomesStaleList(out, scan)
	if len(scan.Stale) > 0 {
		fmt.Fprintf(out, "  run `bunker homes prune` to remove them (use --dry-run first)\n")
	}
}

// printHomesScan renders one prune in the registry-compact report style.
// dryRun adds the explicit no-changes marker, mirroring
// `homes: <path> (dry run, no changes written)`.
func printHomesScan(out io.Writer, root string, scan *homesScan, removed int, dryRun bool) {
	if dryRun {
		fmt.Fprintf(out, "homes: %s (dry run, no changes written)\n", root)
	} else {
		fmt.Fprintf(out, "homes pruned: %s\n", root)
	}
	fmt.Fprintf(out, "  scanned: %d\n", scan.Scanned)
	if dryRun {
		fmt.Fprintf(out, "  stale entries (would remove): %d\n", len(scan.Stale))
	} else {
		fmt.Fprintf(out, "  stale removed: %d\n", removed)
	}
	fmt.Fprintf(out, "  kept (live user): %d\n", len(scan.Kept))
	fmt.Fprintf(out, "  stale size: %s\n", humanBytes(scan.StaleBytes))
	if !dryRun {
		fmt.Fprintf(out, "  entries remaining: %d\n", scan.Scanned-removed)
	}
	printHomesStaleList(out, scan)
}

// NewHomesCommand returns the `bunker homes` command group for inspecting and
// maintaining the agent home root (GAP-080). Running the bare group prints the
// read-only classification of every managed home (stale vs kept) plus the
// stale set's on-disk size; `prune` removes homes whose user is gone.
func NewHomesCommand() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "homes",
		Short: "Inspect and maintain orphaned agent home directories",
		Long: `Inspect and maintain the agent home root (/home).

Every agent spawn creates /home/bunker-<id> for its per-agent user. When a
spawn is cancelled past the request deadline, or its rollback cannot finish,
the home outlives its user: userdel without -r leaves exactly that behind.
` + "`bunker status`" + ` reports the plane as orphan homes, but no surface
could clean it (GAP-080: the demo host carried ~125 orphan /home/bunker-*
directories while the daemon showed "Agents: 0/50").

Run with no subcommand to report the host's agent homes (read-only): every
bunker-* entry is classified as STALE (its user no longer exists) or KEPT (its
user still exists), followed by the stale set's on-disk size and its names.
prune removes exactly the stale entries; it never removes an entry whose user
still exists, never an entry that does not match bunker-*, and nothing at all
after an inconclusive user lookup.

Local-only: like ` + "`bunker linger`" + `, this inspects THIS host's own directory —
it is not addressable over the agent wire protocol. Run it on the host that
owns the home root, as root, so a stale home the daemon left behind is
actually removable.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root := homesRootPath
			if cmd.Flags().Changed("dir") {
				root = dir
			}
			return runHomesStatus(cmd.OutOrStdout(), root)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", defaultHomesRoot, "Agent home root to inspect (run on the host that owns it)")
	cmd.AddCommand(newHomesPruneCommand())
	return cmd
}

// runHomesStatus prints the operator report for the agent home root. It is
// read-only; finding residue is not an error, so an orphan home exits 0.
func runHomesStatus(out io.Writer, root string) error {
	scan, err := scanHomesRoot(root)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "homes root %s does not exist; nothing to report\n", root)
			return nil
		}
		return fmt.Errorf("scan agent home root: %w", err)
	}
	printHomesStatus(out, root, scan)
	return nil
}

// newHomesPruneCommand implements `bunker homes prune`, mirroring
// `bunker registry compact` (out io.Writer, --dry-run reporting what WOULD
// change, clean no-op on a missing target).
func newHomesPruneCommand() *cobra.Command {
	var (
		dir    string
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove orphaned agent home directories whose user no longer exists",
		Long: `Remove orphaned agent home directories.

An entry is stale when its name matches bunker-* and its user no longer exists
(user lookup fails). Entries whose user still exists — root, service accounts,
live agents — are NEVER removed, entries that do not match the managed name
pattern are not classified at all, and an INCONCLUSIVE lookup aborts before
anything is removed.

Before/after counts are printed; --dry-run reports what would change. A
missing home root is a clean no-op. Local-only: run it on the host that owns
the home root, as root.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			root := homesRootPath
			if cmd.Flags().Changed("dir") {
				root = dir
			}
			if _, err := os.Stat(root); os.IsNotExist(err) {
				fmt.Fprintf(out, "homes root %s does not exist; nothing to prune\n", root)
				return nil
			}
			scan, removed, err := pruneHomesRoot(root, dryRun)
			if err != nil {
				return fmt.Errorf("prune homes root: %w", err)
			}
			printHomesScan(out, root, scan, removed, dryRun)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", defaultHomesRoot, "Agent home root to prune (run on the host that owns it)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what pruning would remove without changing anything")
	return cmd
}
