package cli

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewKeysCommand returns the `bunker key` command group (GAP-132): rotate
// (zero-downtime jwt_secret rotation), list, revoke.
//
// All three verbs are master-credential operations — they must run with the
// server's master token in BUNKER_TOKEN / the server entry, exactly like
// `bunker spawn` / `bunker destroy`. An agent sub-key is rejected by the
// daemon's master-only interceptor (CodeUnauthenticated).
func NewKeysCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Manage API keys and the JWT signing secret (master only)",
		Long: `Manage API keys and the JWT signing secret on a bunkerd server.

key rotate  Rotate the JWT signing secret WITHOUT downtime. The new secret is
            printed EXACTLY ONCE — store it (env-file / auth.jwt_secret_file)
            before you lose this output. During the overlap window
            (--overlap-seconds, default 600, capped at 3600) the retired
            secret keeps validating existing tokens, so live clients never
            see an auth failure; after the window they are rejected.
key list    List active API sub-keys (metadata only, no secrets).
key revoke  Revoke an API sub-key by key ID. Immediate and durable: the
            credential stops validating and stays revoked across restarts.

All verbs require the master credential (BUNKER_TOKEN or the server entry
token); agent sub-keys are rejected.`,
	}
	cmd.AddCommand(newKeyRotateCommand())
	cmd.AddCommand(newKeyListCommand())
	cmd.AddCommand(newKeyRevokeCommand())
	return cmd
}

// keyServerEntry resolves the target server for a key verb: LoadCLIConfig →
// fail-closed server resolution (never silently falling back to the shared
// active_server default for mutating operations — the GAP-093 posture ssh
// uses). Returns the configured entry for the resolved alias.
func keyServerEntry(serverName string) (ServerEntry, error) {
	cfg, err := LoadCLIConfig()
	if err != nil {
		return ServerEntry{}, fmt.Errorf("load config: %w", err)
	}
	resolved, berr := SessionScopedTarget(serverName, cfg.ActiveServer)
	if berr != nil {
		return ServerEntry{}, berr
	}
	entry, ok := cfg.Servers[resolved]
	if !ok {
		return ServerEntry{}, fmt.Errorf("server %q not found in config", resolved)
	}
	return entry, nil
}

// newKeyRotateCommand builds `bunker key rotate`.
func newKeyRotateCommand() *cobra.Command {
	var (
		serverName     string
		overlapSeconds uint32
	)
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Rotate the JWT signing secret without downtime (new secret printed ONCE)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			entry, err := keyServerEntry(serverName)
			if err != nil {
				return err
			}
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.RotateJWTSecretRequest{OverlapSeconds: overlapSeconds})
			if token := resolveToken(entry); token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			resp, err := client.RotateJWTSecret(ctx, req)
			if err != nil {
				return fmt.Errorf("rotate jwt secret: %w", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, resp.Msg.GetJwtSecret())
			fmt.Fprintf(out, "# rotated_at: %s\n", resp.Msg.GetRotatedAt())
			fmt.Fprintf(out, "# overlap_seconds: %d (old secret still validates until then, then rejected)\n", resp.Msg.GetOverlapSeconds())
			fmt.Fprintf(out, "# previous fingerprint: %s\n", resp.Msg.GetPreviousFingerprint())
			fmt.Fprintln(out, "# The new secret is shown ONCE — persist it now (auth.jwt_secret_file / env-file) and restart bunkerd to load it.")
			return nil
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().Uint32Var(&overlapSeconds, "overlap-seconds", 600, "Dual-accept window: how long the old secret still validates (capped at 3600)")
	return cmd
}

// newKeyListCommand builds `bunker key list`.
func newKeyListCommand() *cobra.Command {
	var (
		serverName string
		agentID    string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List active API sub-keys (metadata only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			entry, err := keyServerEntry(serverName)
			if err != nil {
				return err
			}
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.KeyListRequest{AgentId: agentID})
			if token := resolveToken(entry); token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			resp, err := client.KeyList(ctx, req)
			if err != nil {
				return fmt.Errorf("list keys: %w", err)
			}
			out := cmd.OutOrStdout()
			keys := resp.Msg.GetKeys()
			if len(keys) == 0 {
				fmt.Fprintln(out, "no active keys")
				return nil
			}
			for _, k := range keys {
				fmt.Fprintf(out, "%s	agent=%s	created=%s	expires=%s", k.GetKeyId(), k.GetAgentId(), k.GetCreatedAt(), k.GetExpiresAt())
				if k.GetRevoked() {
					fmt.Fprint(out, "	REVOKED")
				}
				fmt.Fprintln(out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().StringVar(&agentID, "agent", "", "Only keys for this agent_id (exact match)")
	return cmd
}

// newKeyRevokeCommand builds `bunker key revoke <key-id>`.
func newKeyRevokeCommand() *cobra.Command {
	var serverName string
	cmd := &cobra.Command{
		Use:   "revoke <key-id>",
		Short: "Revoke an API sub-key (immediate, survives restarts)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			keyID := args[0]
			entry, err := keyServerEntry(serverName)
			if err != nil {
				return err
			}
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.RevokeKeyRequest{KeyId: keyID})
			if token := resolveToken(entry); token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			resp, err := client.RevokeKey(ctx, req)
			if err != nil {
				return fmt.Errorf("revoke key: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", resp.Msg.GetKeyId(), resp.Msg.GetStatus())
			return nil
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	return cmd
}
