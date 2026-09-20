package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/agent"
)

// NewSubIDMigrateCommand returns the `bunker subid-migrate` command.
//
// GAP-140 / SEC-22 remediation. Early builds wrote each agent's subordinate-id
// range starting at the agent's own uid, so consecutive agents received
// overlapping ranges and the user-namespace separation between them was not
// enforced. This command rewrites every managed agent entry (username prefix
// "bunker-") that overlaps another entry or sits below the allocation pool,
// assigning each a fresh non-overlapping range. It never touches entries for
// other users.
//
// The default is a DRY RUN: it prints what it finds and changes nothing.
// --apply performs the rewrite. Restart `bunkerd` afterwards so it re-checks
// the (now clean) state at startup.
func NewSubIDMigrateCommand() *cobra.Command {
	var apply bool

	cmd := &cobra.Command{
		Use:   "subid-migrate",
		Short: "Rewrite overlapping subordinate-id ranges for managed agents (GAP-140)",
		Long: `Rewrite the subordinate-id ranges in /etc/subuid and /etc/subgid so the ranges
Bunker hands its agent users are globally disjoint.

Only entries whose username begins with "bunker-" are considered; every other
entry is preserved verbatim. An entry is rewritten when its range overlaps
another entry or starts below the allocation pool.

Without --apply this is a dry run: it reports overlaps and changes nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Println("Checking /etc/subuid and /etc/subgid for overlapping subordinate-id ranges...")

			var overlaps []string
			for _, p := range []string{"/etc/subuid", "/etc/subgid"} {
				if err := agent.VerifySubIDPath(p); err != nil {
					overlaps = append(overlaps, err.Error())
				}
			}

			if len(overlaps) == 0 {
				fmt.Println("OK: no overlapping subordinate-id ranges detected. Nothing to migrate.")
				return nil
			}
			for _, o := range overlaps {
				fmt.Printf("OVERLAP  %s\n", o)
			}

			if !apply {
				fmt.Println("\nDry run — no files were changed.")
				fmt.Println("Re-run with --apply to rewrite the managed agent ranges.")
				fmt.Println("Note: a rewrite gives those agents NEW container id mappings; restart them.")
				return nil
			}

			total, err := agent.MigrateSubIDs()
			if err != nil {
				return fmt.Errorf("migrate subordinate ids: %w", err)
			}
			if err := agent.CheckSubIDOverlaps(); err != nil {
				return fmt.Errorf("migration did not resolve every overlap: %w", err)
			}
			fmt.Printf("\nDone: %d managed range(s) rewritten. Subordinate ids are now disjoint.\n", total)
			fmt.Println("Restart bunkerd (and any running agents) to pick up the new mappings.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "perform the rewrite (default: dry run)")
	return cmd
}
