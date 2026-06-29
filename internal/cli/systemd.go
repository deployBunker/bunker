package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/systemd"
)

// NewSystemdCommand returns the `bunker systemd` cobra command group.
func NewSystemdCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "systemd",
		Short:  "Manage the bunkerd systemd service",
		Long:   `Install or remove the bunkerd systemd service unit and logrotate config.`,
		Hidden: true,
	}

	cmd.AddCommand(newSystemdInstallCommand())
	cmd.AddCommand(newSystemdUninstallCommand())
	cmd.AddCommand(newSystemdStatusCommand())

	return cmd
}

func newSystemdInstallCommand() *cobra.Command {
	var (
		binaryPath string
		configPath string
		user       string
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the bunkerd systemd service",
		Long: `Install the bunkerd systemd service unit and logrotate config.

The command writes /etc/systemd/system/bunkerd.service and
/etc/logrotate.d/bunkerd, then reloads systemd.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := systemd.DefaultInstallOptions()
			if binaryPath != "" {
				opts.BinaryPath = binaryPath
			}
			if configPath != "" {
				opts.ConfigPath = configPath
			}
			if user != "" {
				opts.User = user
			}

			unitPath, logrotatePath, err := systemd.InstallService(opts)
			if err != nil {
				return fmt.Errorf("install service: %w", err)
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Installed bunkerd systemd service")
			fmt.Fprintf(cmd.OutOrStdout(), "  Unit:      %s\n", unitPath)
			fmt.Fprintf(cmd.OutOrStdout(), "  Logrotate: %s\n", logrotatePath)
			fmt.Fprintln(cmd.OutOrStdout(), "Enable with: systemctl enable --now bunkerd")
			return nil
		},
	}

	cmd.Flags().StringVar(&binaryPath, "binary", "", "Path to the bunkerd binary (default: this binary)")
	cmd.Flags().StringVar(&configPath, "config", "", "Path to the bunkerd config file (default: /etc/bunkerd/config.yaml)")
	cmd.Flags().StringVar(&user, "user", "", "User to run bunkerd as (default: root)")

	return cmd
}

func newSystemdUninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the bunkerd systemd service",
		Long:  `Remove the bunkerd systemd service unit and logrotate config.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := systemd.UninstallService(); err != nil {
				return fmt.Errorf("uninstall service: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Removed bunkerd systemd service")
			return nil
		},
	}
}

func newSystemdStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check whether the bunkerd systemd service is installed",
		RunE: func(cmd *cobra.Command, args []string) error {
			if systemd.IsInstalled() {
				fmt.Fprintln(cmd.OutOrStdout(), "bunkerd systemd service is installed")
				fmt.Fprintf(cmd.OutOrStdout(), "  Unit:      %s\n", systemd.UnitPath)
				fmt.Fprintf(cmd.OutOrStdout(), "  Logrotate: %s\n", systemd.LogrotatePath)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "bunkerd systemd service is not installed")
			}
			return nil
		},
	}
}
