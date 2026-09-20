package cli

import (
	"fmt"
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
		tlsMode     string
		acceptCert  bool
	)

	cmd := &cobra.Command{
		Use:   "connect SERVER_URL",
		Short: "Register a bunkerd server",
		Long: `Connect to a bunkerd server and register it in the local CLI config.

The server URL should be the base URL for the connect or gRPC server,
e.g. https://bunker-host:9090 (TLS) or http://localhost:8080 (plain-HTTP
loopback dev).

TLS trust (--tls):

  --tls self-signed   the daemon presents a self-signed certificate (the
                      default when tls.self_signed is set on the daemon). The
                      certificate's sha256 fingerprint is shown and stored on
                      first connect; every later connection is verified against
                      it, and a changed certificate is refused loudly.
  --tls system        verify against the system root store (a real CA-signed
                      certificate).
  (no --tls flag)     http:// loopback stays plain HTTP; https:// uses the
                      system root store. A self-signed daemon is never accepted
                      silently — it fails verification unless you pin it.

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

			mode, err := ParseTLSMode(tlsMode)
			if err != nil {
				return err
			}
			if tlsInsecure && mode == TLSModeSelfSigned {
				return fmt.Errorf("--tls-insecure skips certificate verification while --tls %s requires it — pass one or the other", TLSModeSelfSigned)
			}

			return RegisterServerWithOptions(ConnectOptions{
				Name:       serverName,
				URL:        url,
				Token:      token,
				TLSMode:    mode,
				Insecure:   tlsInsecure,
				AcceptCert: acceptCert,
			})
		},
	}

	cmd.Flags().StringVar(&serverName, "name", "", "Server alias (defaults to hostname from response)")
	cmd.Flags().StringVar(&serverToken, "token", "", "Authentication token ($BUNKER_TOKEN)")
	cmd.Flags().BoolVar(&tlsInsecure, "tls-insecure", false, "Skip TLS certificate verification (explicit opt-out)")
	cmd.Flags().StringVar(&tlsMode, "tls", "", "TLS trust mode: self-signed (pin the daemon's certificate on first use) or system (verify against the system root store)")
	cmd.Flags().BoolVar(&acceptCert, "accept-cert", false, "Accept and pin the certificate the server presents now, replacing any pin already stored for it")

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
