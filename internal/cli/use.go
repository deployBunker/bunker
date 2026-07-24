package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// NewUseCommand returns the `bunker use` cobra command.
// It switches the active bunkerd server so subsequent commands target it.
func NewUseCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "use SERVER_NAME",
		Short: "Switch the active bunkerd server",
		Long: `Switch the active bunkerd server.

This changes which server subsequent commands (list, spawn, destroy, etc.)
target by default. You must have already registered the server via
'bunker connect'.

Examples:
  bunker use staging
  bunker use prod-us-east`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			// 1. Load CLI config
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// 2. Handle empty config
			if len(cfg.Servers) == 0 {
				return fmt.Errorf("no servers configured — run 'bunker connect' first")
			}

			// 3. Look up server
			entry, ok := cfg.Servers[name]
			if !ok {
				var names []string
				for n := range cfg.Servers {
					names = append(names, n)
				}
				sort.Strings(names)
				return fmt.Errorf("server %q not found. Available servers: %s", name, strings.Join(names, ", "))
			}

			// 4. Set active server and save
			cfg.ActiveServer = name
			if err := SaveCLIConfig(cfg); err != nil {
				return fmt.Errorf("save config: %w", err)
			}

			// 5. Confirm
			fmt.Printf("Active server set to %q (%s)\n", name, entry.URL)
			return nil
		},
	}

	return cmd
}
