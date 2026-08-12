// bunker — CLI for managing Bunker agent hosts
//
// Three-tier CLI:
//
//	bunker infra ...    — manage servers, deploy bunkerd instances
//	bunker host ...     — manage agents on a connected server
//	bunker agent ...    — scoped to single agent (customer-facing)
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/deployBunker/bunker/internal/cli"
	"github.com/deployBunker/bunker/internal/version"
)

func main() {
	if err := run(); err != nil {
		// Propagate a remote command's exit code (bunker exec / bunker run)
		// silently, ssh-style — no "bunker: ..." noise for the exit-code path.
		if code, ok := exitCodeFor(err); ok {
			os.Exit(code)
		}
		fmt.Fprintf(os.Stderr, "bunker: %v\n", err)
		os.Exit(1)
	}
}

// exitCodeFor returns the exit code to propagate when err is (or wraps) an
// *cli.ExitError, e.g. the remote command's exit code from exec/run.
func exitCodeFor(err error) (int, bool) {
	var exitErr *cli.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code, true
	}
	return 0, false
}

func run() error {
	// Bind BUNKER_TOKEN env var early so it is available to subcommands.
	viper.SetEnvPrefix("BUNKER")
	viper.AutomaticEnv()
	_ = viper.BindEnv("token") // BUNKER_TOKEN

	root := &cobra.Command{
		Use:   "bunker",
		Short: "CLI for managing Bunker agent hosts",
		Long: `bunker is the command-line tool for managing Bunker agent hosts.

Manage servers, deploy bunkerd instances, connect to remote hosts,
and control ephemeral development environments — all from the CLI.`,
		// Version auto-adds the --version flag (GAP-035); the value is the
		// build-time metadata default, overridden by make build ldflags.
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(cli.NewConnectCommand())
	root.AddCommand(cli.NewSpawnCommand())
	root.AddCommand(cli.NewListCommand())
	root.AddCommand(cli.NewDestroyCommand())
	root.AddCommand(cli.NewEnvCommand())
	root.AddCommand(cli.NewMetricsCommand())
	root.AddCommand(cli.NewExecCommand())
	root.AddCommand(cli.NewRunCommand())
	root.AddCommand(cli.NewInfoCommand())
	root.AddCommand(cli.NewHeartbeatCommand())
	root.AddCommand(cli.NewSystemdCommand())
	root.AddCommand(cli.NewMountCommand())
	root.AddCommand(cli.NewTunnelCommand())
	root.AddCommand(cli.NewVersionCommand())
	root.AddCommand(cli.NewUseCommand())
	root.AddCommand(cli.NewCpCommand())
	root.AddCommand(cli.NewDeployCommand())
	root.AddCommand(cli.NewSSHCommand())
	root.AddCommand(cli.NewStatusCommand())

	return root.Execute()
}
