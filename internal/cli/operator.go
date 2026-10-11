package cli

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewOperatorCommand returns the `bunker operator` command group (GAP-130):
// create, list, delete.
//
// All three verbs are ADMIN operations — they must run with the server's
// master credential in BUNKER_TOKEN / the server entry, or with an
// operator token holding the `admin` role. An `operator`-role token is
// denied by the daemon's RBAC interceptor (CodePermissionDenied) and an
// agent-scoped credential by the master-only interceptor
// (CodeUnauthenticated).
func NewOperatorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "operator",
		Short: "Manage operator identities and their RBAC roles (admin only)",
		Long: `Manage operator identities on a bunkerd server (GAP-130).

An operator is a NAMED credential with an explicit role, so humans and
automations stop sharing the master token:

  admin      Everything the master token can do, including managing other
             operators. Provision sparingly.
  operator   Agent lifecycle: spawn, destroy, exec, run, list, metrics,
             stop/start/restart. CANNOT manage operators, rotate the JWT
             secret, revoke keys, query the audit trail, or read host-level
             server info (denied with permission_denied).
  agent:self A machine identity for ONE agent: name IS the agent id, and the
             token can only operate on that agent (Agent service RPCs with
             the usual ownership guards).

operator create  Provision an identity. The bearer token is printed EXACTLY
                 ONCE — only its SHA-256 hash is stored server-side. Store
                 it (env-file / server entry ` + "`token:`" + `) before you lose
                 this output.
operator list    List operator identities (metadata only, no secrets).
                 --agent-id filters to one agent's agent:self identity.
operator delete  Remove an identity. Immediate and durable: the token stops
                 validating and the removal survives daemon restarts.

All verbs require an admin credential (the master token, or an operator
token with the admin role).`,
	}
	cmd.AddCommand(newOperatorCreateCommand())
	cmd.AddCommand(newOperatorListCommand())
	cmd.AddCommand(newOperatorDeleteCommand())
	return cmd
}

// operatorServerEntry resolves the target server for an operator verb —
// the same fail-closed resolution key verbs use (never silently falling
// back to the shared active_server default for mutating operations).
func operatorServerEntry(serverName string) (ServerEntry, error) {
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

// newOperatorCreateCommand builds `bunker operator create <name> --role <r>`.
func newOperatorCreateCommand() *cobra.Command {
	var (
		serverName string
		role       string
		ttlSeconds uint32
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an operator identity; prints its bearer token exactly once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			entry, err := operatorServerEntry(serverName)
			if err != nil {
				return err
			}
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.OperatorCreateRequest{
				Name:       args[0],
				Role:       role,
				TtlSeconds: ttlSeconds,
			})
			if token := resolveToken(entry); token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			resp, err := client.OperatorCreate(ctx, req)
			if err != nil {
				return fmt.Errorf("create operator: %w", err)
			}
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out, "operator %s created (role=%s)\n", resp.Msg.GetName(), resp.Msg.GetRole()); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(out, resp.Msg.GetToken()); err != nil {
				return err
			}
			for _, line := range []string{
				fmt.Sprintf("# expires_at: %s (ttl_seconds: %d)\n", resp.Msg.GetExpiresAt(), resp.Msg.GetTtlSeconds()),
				"# The token above is shown EXACTLY ONCE — the daemon stores only its SHA-256 hash.",
				"# Put it in the operator's own env (BUNKER_TOKEN) or a server entry in ~/.bunker/config.yaml.",
			} {
				if _, err := fmt.Fprintln(out, line); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set; mutating commands never fall back to the shared active default)")
	cmd.Flags().StringVar(&role, "role", "", "RBAC role: admin | operator | agent:self (required)")
	cmd.Flags().Uint32Var(&ttlSeconds, "ttl-seconds", 0, "Credential lifetime in seconds (0 = daemon default, 30 days)")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}

// newOperatorListCommand builds `bunker operator list [--agent-id <id>]`.
func newOperatorListCommand() *cobra.Command {
	var (
		serverName string
		agentID    string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List operator identities (metadata only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			entry, err := operatorServerEntry(serverName)
			if err != nil {
				return err
			}
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.OperatorListRequest{AgentId: agentID})
			if token := resolveToken(entry); token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			resp, err := client.OperatorList(ctx, req)
			if err != nil {
				return fmt.Errorf("list operators: %w", err)
			}
			out := cmd.OutOrStdout()
			if len(resp.Msg.GetOperators()) == 0 {
				if _, err := fmt.Fprintln(out, "no operators configured"); err != nil {
					return err
				}
				return nil
			}
			// Fixed-width table: NAME ROLE AGENT_ID CREATED_AT EXPIRES_AT.
			if _, err := fmt.Fprintf(out, "%-24s %-11s %-24s %-25s %s\n", "NAME", "ROLE", "AGENT_ID", "CREATED_AT", "EXPIRES_AT"); err != nil {
				return err
			}
			for _, op := range resp.Msg.GetOperators() {
				if _, err := fmt.Fprintf(out, "%-24s %-11s %-24s %-25s %s\n",
					op.GetName(), op.GetRole(), op.GetAgentId(), op.GetCreatedAt(), op.GetExpiresAt()); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set)")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "Only the agent:self identity bound to this agent")
	return cmd
}

// newOperatorDeleteCommand builds `bunker operator delete <name>`.
func newOperatorDeleteCommand() *cobra.Command {
	var serverName string
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete an operator identity; its token stops validating immediately",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			entry, err := operatorServerEntry(serverName)
			if err != nil {
				return err
			}
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.OperatorDeleteRequest{Name: args[0]})
			if token := resolveToken(entry); token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			resp, err := client.OperatorDelete(ctx, req)
			if err != nil {
				return fmt.Errorf("delete operator: %w", err)
			}
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out, "operator %s: %s\n", resp.Msg.GetName(), resp.Msg.GetStatus()); err != nil {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set; mutating commands never fall back to the shared active default)")
	return cmd
}
