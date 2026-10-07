package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/iobattery"
)

// NewIOBatteryCommand builds `bunker iobattery`: the BFS-057 measurement
// battery that every mount/FUSE performance lever is judged by. It runs
// against any directory target — a real bunker mount point in the intended
// case, an ordinary local directory as the fallback when no bunker agent is
// available — and emits a machine-readable JSON report with all five
// measurement families plus the readahead negative control.
func NewIOBatteryCommand() *cobra.Command {
	var (
		target      string
		outPath     string
		sizeMB      int64
		ops         int
		loadWorkers int
		mode        string
		noControl   bool
	)

	cmd := &cobra.Command{
		Use:   "iobattery --target <dir>",
		Short: "Run the I/O measurement battery (BFS-057) and emit a JSON report",
		Long: `Run the I/O measurement battery (BFS-057) against a filesystem target and
emit a machine-readable JSON report.

The target is any directory: a bunker mount point (mount the agent home with
'bunker mount <agent-id> <dir>' first) or, when no bunker agent is available,
an ordinary local directory as a fallback. The report covers five measurement
families plus a negative control:

  throughput          single-stream sequential write + read (MB/s)
  latency_under_load  p50/p95/p99 of small ops while background load runs
  metadata_ops        create/stat/rename/unlink rate (ops/s)
  iops                random 4K read/write IOPS
  cpu_per_byte        rusage CPU time vs bytes moved (bytes per CPU-second)
  negative_control    readahead lever proven by measurement (buffered vs O_DIRECT)

Every measurement records its counts, elapsed time, derived metric, and the
exact command to reproduce it. No perf change lands without a before/after
pair from this battery (docs/iobattery.md).

Examples:
  bunker iobattery --target /mnt/bunker/agent-dev
  bunker iobattery --target /tmp/io-scratch --size-mb 64 --ops 5000
  bunker iobattery --target /mnt/bunker/agent-dev --out report.json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if target == "" {
				return fmt.Errorf("--target is required (a bunker mount point or a local directory)")
			}
			if mode != "" && mode != "all" {
				switch mode {
				case "throughput", "latency", "metadata", "iops", "cpuperbyte", "control":
				default:
					return fmt.Errorf("unknown --mode %q (throughput|latency|metadata|iops|cpuperbyte|control|all)", mode)
				}
				if _, ferr := fmt.Fprintf(cmd.ErrOrStderr(), "note: --mode %q is a planning aid; the full battery always runs (v1)\n", mode); ferr != nil {
					return ferr
				}
			}
			rep, err := iobattery.Run(iobattery.Options{
				Target:        target,
				SizeMB:        sizeMB,
				Ops:           ops,
				LoadWorkers:   loadWorkers,
				NoNegativeCtl: noControl,
				Command:       cmd.CommandPath(),
			})
			if err != nil {
				return err
			}
			data, merr := rep.Marshal()
			if merr != nil {
				return merr
			}
			if outPath != "" {
				if werr := writeFile0600(outPath, data); werr != nil {
					return fmt.Errorf("write report: %w", werr)
				}
				if _, ferr := fmt.Fprintf(cmd.OutOrStdout(), "report written to %s\n", outPath); ferr != nil {
					return ferr
				}
				return nil
			}
			if _, ferr := cmd.OutOrStdout().Write(append(data, '\n')); ferr != nil {
				return ferr
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&target, "target", "", "directory to measure (bunker mount point or local dir; required)")
	cmd.Flags().StringVar(&outPath, "out", "", "write the JSON report to this file (default: stdout)")
	cmd.Flags().Int64Var(&sizeMB, "size-mb", 32, "payload size in MiB for throughput/cpu-per-byte")
	cmd.Flags().IntVar(&ops, "ops", 2000, "op count for metadata/iops/latency families")
	cmd.Flags().IntVar(&loadWorkers, "load-workers", 4, "background load goroutines for latency-under-load")
	cmd.Flags().StringVar(&mode, "mode", "", "documentation aid; the full battery always runs")
	cmd.Flags().BoolVar(&noControl, "no-control", false, "skip the negative control family")
	return cmd
}

func writeFile0600(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
