package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewDestroyCommand returns the `bunker destroy` cobra command.
func NewDestroyCommand() *cobra.Command {
	var (
		serverName string
		force      bool
		keepKey    bool
	)

	cmd := &cobra.Command{
		Use:   "destroy <agent-id>",
		Short: "Destroy an agent",
		Long: `Destroy an agent on the active bunkerd server.

The agent's Linux user is removed, and WITH it the agent's entire home
directory (/home/bunker-<id>) — including anything the agent stored there
(cloned repositories, unmerged work, dotfiles and tooling). Under the
default daemon policy the home is first archived to the daemon's
destroy_archive_dir and the archive is verified BEFORE anything is
deleted; if archiving fails, the destroy is refused and the home is
retained (destroy_home_policy: purge restores the historical
delete-without-archive behavior).

Examples:
  bunker destroy abc12345
  bunker destroy abc12345 --force
  bunker destroy abc12345 --server staging
  bunker destroy abc12345 --keep-key`,

		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			// 1. Load CLI config
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// 2. Determine server
			// Fail-closed binding (GAP-093): mutating commands never fall
			// back to the shared active_server default.
			resolved, berr := SessionScopedTarget(serverName, cfg.ActiveServer)
			if berr != nil {
				return berr
			}
			serverName = resolved

			entry, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("server %q not found in config", serverName)
			}

			// 3. Build request
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.DestroyAgentRequest{
				AgentId: agentID,
				Force:   force,
			})

			// Auth token
			token := resolveToken(entry)
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}

			// 4. Call RPC
			resp, err := client.DestroyAgent(ctx, req)
			if err != nil {
				// A not-found agent is an idempotent success, not an error:
				// print the same clean message and exit 0 as the in-band
				// resp.Status == "not_found" branch below. The stale local
				// key (if any) is still removed.
				if connect.CodeOf(err) == connect.CodeNotFound {
					fmt.Printf("Agent %s not found.\n", agentID)
					return removeLocalSSHKey(agentID, keepKey)
				}
				// Real RPC error: the agent may still exist, so the local
				// key is left in place.
				//
				// DF-BUNKER-33: a fail-closed destroy (home could not be
				// archived before userdel) surfaces here as a CodeInternal
				// whose message names the retained home. Print the plain
				// guidance line first so the operator sees the outcome
				// without parsing the wrapped RPC error.
				if strings.Contains(err.Error(), "home retained") {
					fmt.Printf("Agent %s NOT destroyed: the home could not be archived, so it was RETAINED (nothing was deleted).\n", agentID)
				}
				return fmt.Errorf("destroy agent: %w", err)
			}

			// 5. Print result
			if resp.Msg.Status == "not_found" {
				fmt.Printf("Agent %s not found.\n", agentID)
				return removeLocalSSHKey(agentID, keepKey)
			}
			// DF-BUNKER-33: fail-closed destroy — the server refused to delete
			// because the home archive step failed. The agent (user, home, port
			// range) is still registered; the local SSH key must stay too.
			if resp.Msg.Status == "home_retained" {
				fmt.Printf("Agent %s NOT destroyed: the home could not be archived, so it was RETAINED (nothing was deleted).\n", agentID)
				return fmt.Errorf("agent %s retained: home archive failed before delete (nothing was deleted)", agentID)
			}
			fmt.Printf("Agent %s destroyed.\n", agentID)
			return removeLocalSSHKey(agentID, keepKey)
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().BoolVar(&force, "force", false, "Force destroy even if agent is running")
	cmd.Flags().BoolVar(&keepKey, "keep-key", false, "Keep the local SSH key (~/.bunker/keys/<id>) after destroy (key rotation)")

	return cmd
}

// removeLocalSSHKey deletes the client-local SSH key saved by spawn
// (~/.bunker/keys/<agentID>) after a successful destroy. It is a no-op when
// keepKey is set (key rotation workflows) or when the key is already absent
// (idempotent cleanup — removing a missing key is not an error). A real
// removal error is returned so the caller knows the key lingers.
func removeLocalSSHKey(agentID string, keepKey bool) error {
	if keepKey {
		return nil
	}
	keyPath, err := defaultSSHKeyPath(agentID)
	if err != nil {
		// The destroy itself already succeeded; don't turn a best-effort
		// hygiene cleanup into a command failure.
		return nil
	}
	if err := os.Remove(keyPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("remove local SSH key: %w", err)
	}
	fmt.Printf("Removed local SSH key %s\n", keyPath)
	return nil
}
