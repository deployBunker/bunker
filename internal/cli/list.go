package cli

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// NewListCommand returns the `bunker list` cobra command.
func NewListCommand() *cobra.Command {
	var (
		serverName string
		status     string
		pageSize   uint32
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List agents on a bunkerd server",
		Long: `List agents on the active bunkerd server.

Examples:
  bunker list
  bunker list --status stopped
  bunker list --server staging --status all --page-size 100`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// 1. Load CLI config
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// 2. Determine server
			if serverName == "" {
				serverName = cfg.ActiveServer
			}
			if serverName == "" {
				return fmt.Errorf("no active server; run 'bunker connect' first")
			}

			entry, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("server %q not found in config", serverName)
			}

			// 3. Build HTTP client
			httpClient := &http.Client{Timeout: 30 * time.Second}
			if entry.TLSInsecure {
				httpClient.Transport = &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				}
			}

			// 4. Build request
			client := bunkerv1connect.NewBunkerdClient(httpClient, entry.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			statusFilter := status
			if statusFilter == "all" {
				statusFilter = ""
			}

			req := connect.NewRequest(&v1.ListAgentsRequest{
				StatusFilter: statusFilter,
				PageSize:     pageSize,
			})

			// Auth token
			token := entry.Token
			if token == "" {
				token = viper.GetString("token")
			}
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}

			// 5. Call RPC
			resp, err := client.ListAgents(ctx, req)
			if err != nil {
				return fmt.Errorf("list agents: %w", err)
			}

			// 6. Print results
			agents := resp.Msg.Agents
			if len(agents) == 0 {
				fmt.Println("No agents found.")
				return nil
			}

			fmt.Println()
			fmt.Println("══════════ Agents ══════════")
			fmt.Println()
			fmt.Printf("  %-14s %-10s %-25s %s\n", "Agent ID", "Status", "Created", "Public URL")
			fmt.Printf("  %-14s %-10s %-25s %s\n", "────────", "──────", "───────", "──────────")
			for _, a := range agents {
				publicURL := a.PublicUrl
				if publicURL == "" {
					publicURL = "(no URL)"
				}
				fmt.Printf("  %-14s %-10s %-25s %s\n", a.AgentId, a.Status, a.CreatedAt, publicURL)
			}
			fmt.Println()
			fmt.Printf("Total: %d agents (server: %s)\n", resp.Msg.TotalCount, serverName)

			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().StringVar(&status, "status", "running", "Filter by status: running, stopped, all")
	cmd.Flags().Uint32Var(&pageSize, "page-size", 50, "Page size")

	return cmd
}
