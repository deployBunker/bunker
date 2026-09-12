package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/registry"
)

// defaultRegistryPath mirrors the daemon's configured registry location
// (internal/config DefaultRegistryPath); --path overrides it. Like
// `bunker audit`, compaction runs LOCALLY on the host that owns the file —
// a remote daemon's registry cannot be compacted over the wire.
const defaultRegistryPath = config.DefaultRegistryPath

// NewRegistryCommand returns the `bunker registry` command group for
// inspecting and maintaining the durable agent registry (GAP-070).
func NewRegistryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Inspect and maintain the durable agent registry",
		Long: `Inspect and maintain bunkerd's durable agent registry.

The registry is an append-only JSONL lifecycle log (spawn / heartbeat /
destroy), replayed by the daemon at startup so agent state survives a
restart. It is size-capped (5 MiB by default) and rotated through three
backups (.1-.3).

compact rewrites the active file to exactly one current-state record per
live agent plus a bounded index of previously-destroyed agent IDs (which
keeps a repeated destroy idempotent). It is safe to run with the daemon
stopped, and when the daemon is running it is serialized against spawn and
destroy writes by a cross-process lock.`,
	}
	cmd.AddCommand(newRegistryCompactCommand())
	return cmd
}

// newRegistryCompactCommand implements `bunker registry compact`.
func newRegistryCompactCommand() *cobra.Command {
	var (
		path   string
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "compact",
		Short: "Rewrite the registry to one current-state record per live agent",
		Long: `Rewrite the agent registry, dropping stale lifecycle events.

All lifecycle events for a live agent collapse into one current-state
record; agents that were destroyed are removed except for their IDs, which
are kept in a bounded index so a repeated destroy stays idempotent and a
never-seen ID still reports not_found.

The rewrite is atomic (temp file + rename, fsync'd) and takes the same
cross-process lock the daemon uses for spawn/destroy appends, so it is safe
against a running daemon. Before/after counts are printed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			store, err := registry.Open(registry.Options{Path: path, Logger: logger})
			if err != nil {
				return fmt.Errorf("open registry: %w", err)
			}
			defer func() { _ = store.Close() }()

			report := store.Report()
			out := cmd.OutOrStdout()

			if dryRun {
				fmt.Fprintf(out, "registry: %s (dry run, no changes written)\n", store.Path())
				fmt.Fprintf(out, "  files read: %d\n", report.Files)
				fmt.Fprintf(out, "  events now: %d (malformed %d, partial tail %v)\n",
					report.Events, report.Malformed, report.PartialTail)
				fmt.Fprintf(out, "  live agents: %d\n", report.Live)
				fmt.Fprintf(out, "  known destroyed ids: %d\n", report.Known)
				fmt.Fprintf(out, "  would write: %d records (%d live + %d index lines)\n",
					report.Live+boolToInt(report.Known > 0), report.Live, boolToInt(report.Known > 0))
				return nil
			}

			stats, err := store.Compact()
			if err != nil {
				return fmt.Errorf("compact registry: %w", err)
			}
			fmt.Fprintf(out, "registry compacted: %s\n", store.Path())
			fmt.Fprintf(out, "  events: %d -> %d\n", stats.BeforeEvents, stats.AfterEvents)
			fmt.Fprintf(out, "  live agents: %d -> %d\n", stats.BeforeLive, stats.AfterLive)
			fmt.Fprintf(out, "  known destroyed ids: %d -> %d\n", stats.BeforeKnown, stats.AfterKnown)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", defaultRegistryPath, "Registry file to compact (run on the host that owns it)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what compaction would do without rewriting the registry")
	return cmd
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ensureFileModeIsPrivate asserts the 0600 invariant of the registry file.
// It backs TestRegistryCompactRewritesAndReports, which proves the CLI path
// creates/keeps the same private mode the daemon uses.
func ensureFileModeIsPrivate(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		return fmt.Errorf("registry %s has mode %o, want 600", path, perm)
	}
	return nil
}
