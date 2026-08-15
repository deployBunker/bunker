package cli

import (
	"fmt"
	"io"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/version"
)

// PrintVersion writes the canonical 5-field version block (UX-005) to w:
// the binary name and version, commit, build date, Go version, and target
// platform. It is the single source of truth for both the `bunker version`
// subcommand and the `bunker --version` flag, so the two surfaces can never
// diverge (GAP-045).
func PrintVersion(w io.Writer) {
	fmt.Fprintf(w, "bunker %s\n", version.Version)
	fmt.Fprintf(w, "  commit:     %s\n", version.Commit)
	fmt.Fprintf(w, "  built:      %s\n", version.BuildDate)
	fmt.Fprintf(w, "  go version: %s\n", runtime.Version())
	fmt.Fprintf(w, "  platform:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
}

// NewVersionCommand returns the `bunker version` cobra command.
// Version metadata lives in internal/version and is injected at build time
// via -ldflags (see the Makefile build targets).
func NewVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the bunker CLI version",
		Long:  `Print the bunker CLI version, Git commit, Go version, and build date.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			PrintVersion(cmd.OutOrStdout())
			return nil
		},
	}
}
