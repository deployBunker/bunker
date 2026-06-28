package cli

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// NewConnectCommand returns the `bunker connect` cobra command.
func NewConnectCommand() *cobra.Command {
	var (
		serverName  string
		serverToken string
		tlsInsecure bool
	)

	cmd := &cobra.Command{
		Use:   "connect SERVER_URL",
		Short: "Register a bunkerd server",
		Long: `Connect to a bunkerd server and register it in the local CLI config.

The server URL should be the base URL for the connect or gRPC server,
e.g. http://localhost:9090 or https://bunker.example.com.

On success the server is saved to ~/.bunker/config.yaml and becomes
the active server for subsequent commands.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url := args[0]

			// Token: flag takes priority, then env var.
			token := serverToken
			if token == "" {
				token = viper.GetString("bunker_token")
			}
			if token == "" {
				token = os.Getenv("BUNKER_TOKEN")
			}

			return RegisterServer(serverName, url, token, tlsInsecure)
		},
	}

	cmd.Flags().StringVar(&serverName, "name", "", "Server alias (defaults to hostname from response)")
	cmd.Flags().StringVar(&serverToken, "token", "", "Authentication token ($BUNKER_TOKEN)")
	cmd.Flags().BoolVar(&tlsInsecure, "tls-insecure", false, "Skip TLS certificate verification")

	_ = viper.BindEnv("bunker_token", "BUNKER_TOKEN")
	_ = viper.BindPFlag("bunker_token", cmd.Flags().Lookup("token"))

	return cmd
}

// SetRootCommandOutput is a test helper; see docstring.
var SetRootCommandOutput = func() {}

func init() {
	// Ensure fmt.Println output is not buffered in test contexts.
	_ = SetRootCommandOutput
}
