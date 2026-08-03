package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/version"
)

// NewVersionCommand returns the `bunker version` cobra command.
// Version metadata lives in internal/version and is injected at build time
// via -ldflags (see the Makefile build targets).
func NewVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the bunker CLI version",
		Long:  `Print the bunker CLI version, Git commit, Go version, and build date.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("bunker %s\n", version.Version)
			fmt.Printf("  commit:     %s\n", version.Commit)
			fmt.Printf("  built:      %s\n", version.BuildDate)
			fmt.Printf("  go version: %s\n", runtime.Version())
			fmt.Printf("  platform:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}
