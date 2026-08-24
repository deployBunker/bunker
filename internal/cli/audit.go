package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/audit"
)

// defaultAuditLogPath is the daemon's configured audit log location
// (internal/config AuditConfig.Path); the verify subcommand defaults to it
// and --path overrides.
const defaultAuditLogPath = "/var/log/bunkerd/audit.log"

// NewAuditCommand returns the `bunker audit` command group for inspecting
// the bunkerd audit trail. GAP-049 adds local log verification; remote audit
// queries are a separate surface (GAP-050).
func NewAuditCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Inspect the bunkerd audit trail",
	}
	cmd.AddCommand(newAuditVerifyCommand())
	return cmd
}

// newAuditVerifyCommand returns `bunker audit verify`, which checks the
// audit log's hash chain and exits non-zero with the first bad record index
// when any record fails verification (own hash mismatch, broken chain link,
// or unparseable line). Rotated backups (.1-.3) are verified as part of the
// chain.
func newAuditVerifyCommand() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify the audit log hash chain",
		Long: `Verify checks every record of the audit log, including rotated backups
(.1-.3), against its SHA-256 hash chain: each record's hash must match its
line bytes and chain to the previous record. Any tampering is reported with
the first bad record index and exits non-zero; an untouched log prints
"OK (N records)" and exits 0.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			records, firstBad, err := audit.Verify(path)
			if err != nil {
				if firstBad > 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "audit log %s: tamper detected at record %d\n", path, firstBad)
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "audit log %s: OK (%d records)\n", path, records)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", defaultAuditLogPath, "audit log file to verify (rotated backups .1-.3 are checked as part of the chain)")
	return cmd
}
