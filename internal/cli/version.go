package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Build-time ldflags. Set via: go build -ldflags "-X ..."
var (
	Version   = "0.1.0"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// NewVersionCommand returns the `bunker version` cobra command.
func NewVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the bunker CLI version",
		Long:  `Print the bunker CLI version, Git commit, Go version, and build date.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("bunker %s\n", Version)
			fmt.Printf("  commit:     %s\n", Commit)
			fmt.Printf("  built:      %s\n", BuildDate)
			fmt.Printf("  go version: %s\n", runtime.Version())
			fmt.Printf("  platform:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}
