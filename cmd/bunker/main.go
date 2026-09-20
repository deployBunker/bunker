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
	// DF-BUNKER-43: writes to a pipe whose reader has gone away must not kill
	// this process with SIGPIPE before it can report its status. With the
	// signal ignored, a truncated write surfaces as EPIPE — an ordinary error
	// the entry point can turn into the conventional 141. The headline effect
	// is the one scripts need: a FAILING command keeps its failure status
	// instead of dying by signal in a way a caller reads as success.
	//
	// Installed here, in the process entry point, and not in a package
	// init() — a library that rewrote process-wide signal disposition would
	// also do it inside the test binary and every importer.
	cli.IgnoreSIGPIPE()

	// Bind BUNKER_TOKEN env var early so it is available to subcommands.
	viper.SetEnvPrefix("BUNKER")
	viper.AutomaticEnv()
	_ = viper.BindEnv("token") // BUNKER_TOKEN

	// exitOnBrokenPipe maps a truncated stream onto exit code 141 rather
	// than the generic 1. Everything else — including every genuine failure —
	// passes through to the existing %w printing and os.Exit(1) below
	// unchanged, so no error text and no other exit code moves.
	return cli.ExitOnBrokenPipe(newRootCommand().Execute())
}

// newRootCommand builds the bunker root command with all subcommands.
//
// --version is a first-class flag on the root command: it prints the exact
// same 5-field block as the `version` subcommand (UX-005), instead of cobra's
// auto-added --version flag which rendered a bare one-liner (GAP-045).
//
// --config / --daemon-config are persistent flags for the client-local path
// resolution (internal/cli paths.go). They are transferred into the cli
// package setters as soon as the command tree is built (before Execute
// parses them) via the ChangeHost hook, so every subcommand — all of which
// load the CLI config — sees the explicit value with no per-command flag
// parsing. BUNKER_HOME and BUNKERD_CONFIG provide the env-tier defaults and
// are read by internal/cli directly.
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "bunker",
		Short: "CLI for managing Bunker agent hosts",
		Long: `bunker is the command-line tool for managing Bunker agent hosts.

Manage servers, deploy bunkerd instances, connect to remote hosts,
and control ephemeral development environments — all from the CLI.

Client-side path overrides (persistent flags):
  --config <path>         CLI config file. Precedence: --config >
                          $BUNKER_HOME/config.yaml > $HOME/.bunker/config.yaml.
                          (bunker systemd install's local --config means the
                          DAEMON config file and shadows this flag.)
  --daemon-config <path>  DAEMON config file consulted for the default paths
                          of the local-file commands (audit list/verify/export/
                          status --path, registry compact --path). Precedence:
                          --daemon-config > $BUNKERD_CONFIG > /etc/bunkerd/config.yaml.
                          Missing or unreadable files fall back silently to the
                          documented constants. An explicit --path on those
                          commands always wins.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	var showVersion bool
	root.Flags().BoolVar(&showVersion, "version", false, "Print the bunker version (same as 'bunker version')")

	// Persistent client-side path flags (see the Long help above for the
	// precedence rules). The parsed values are transferred into the cli
	// package setters by the PersistentPreRun wrapper below.
	root.PersistentFlags().String("config", "",
		"CLI config file. Precedence: --config > $BUNKER_HOME/config.yaml > $HOME/.bunker/config.yaml. "+
			"NOTE: 'bunker systemd install --config' means the DAEMON config and shadows this flag")
	root.PersistentFlags().String("daemon-config", "",
		"DAEMON config file for the local-file command defaults (audit --path, registry compact --path). "+
			"Precedence: --daemon-config > $BUNKERD_CONFIG > /etc/bunkerd/config.yaml. "+
			"An explicit --path on those commands always wins")
	configFlag := root.PersistentFlags().Lookup("config")
	daemonFlag := root.PersistentFlags().Lookup("daemon-config")

	// Cobra parses all flags (root persistent flags included) before running
	// the target command, then walks the PersistentPreRun chain. No bunker
	// subcommand defines its own PersistentPreRun, so this wrapper is the
	// single point where the explicit values reach internal/cli before any
	// command's RunE (all of which load the CLI config) executes. Empty or
	// whitespace-only values are treated as unset by the cli package.
	prevPersistentPreRun := root.PersistentPreRun
	root.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		cli.SetConfigPathOverride(configFlag.Value.String())
		cli.SetDaemonConfigPathOverride(daemonFlag.Value.String())
		if prevPersistentPreRun != nil {
			prevPersistentPreRun(cmd, args)
		}
	}

	root.RunE = func(cmd *cobra.Command, args []string) error {
		if showVersion {
			cli.PrintVersion(cmd.OutOrStdout())
			return nil
		}
		return cmd.Help()
	}

	root.AddCommand(cli.NewConnectCommand())
	root.AddCommand(cli.NewSpawnCommand())
	root.AddCommand(cli.NewListCommand())
	root.AddCommand(cli.NewDestroyCommand())
	root.AddCommand(cli.NewStopCommand())
	root.AddCommand(cli.NewStartCommand())
	root.AddCommand(cli.NewRestartCommand())
	root.AddCommand(cli.NewEnvCommand())
	root.AddCommand(cli.NewMetricsCommand())
	root.AddCommand(cli.NewExecCommand())
	root.AddCommand(cli.NewRunCommand())
	root.AddCommand(cli.NewInfoCommand())
	// The agent-side dependency probe: the remote editing verbs execute in the
	// agent's context, so a tool present on the CLIENT is irrelevant to them.
	root.AddCommand(cli.NewAgentToolsCommand())
	root.AddCommand(cli.NewHeartbeatCommand())
	root.AddCommand(cli.NewSystemdCommand())
	root.AddCommand(cli.NewMountCommand())
	root.AddCommand(cli.NewUmountCommand())
	root.AddCommand(cli.NewGuardCommand())
	root.AddCommand(cli.NewTunnelCommand())
	root.AddCommand(cli.NewVersionCommand())
	root.AddCommand(cli.NewUseCommand())
	root.AddCommand(cli.NewCpCommand())
	root.AddCommand(cli.NewDeployCommand())
	root.AddCommand(cli.NewSSHCommand())
	root.AddCommand(cli.NewStatusCommand())
	root.AddCommand(cli.NewAuditCommand())
	root.AddCommand(cli.NewRegistryCommand())
	root.AddCommand(cli.NewLingerCommand())
	root.AddCommand(cli.NewHomesCommand())
	root.AddCommand(cli.NewHostProvisionCommand())
	root.AddCommand(cli.NewSubIDMigrateCommand())

	return root
}
