// bunker — CLI for managing Bunker agent hosts
//
// Three-tier CLI:
//
//	bunker infra ...    — manage servers, deploy bunkerd instances
//	bunker host ...     — manage agents on a connected server
//	bunker agent ...    — scoped to single agent (customer-facing)
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/deployBunker/bunker/internal/cli"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bunker: %v\n", err)
		os.Exit(1)
	}
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
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(cli.NewConnectCommand())

	return root.Execute()
}
