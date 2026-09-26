package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// renewOperationTimeout is the client-side budget for one renewal. A renewal
// includes a full spawn (~90s rootless install on a fresh home), not one
// systemctl hop, so it cannot reuse the surface commands' 30s budget.
const renewOperationTimeout = 10 * time.Minute

// fetchAndSaveAgentKey fetches an agent's SSH private key through the
// master-credential-gated GetAgentKey RPC and saves it to the client-local
// keys dir, shared by spawn (GAP-128) and renew's re-spawn leg (DF-BUNKER-65:
// a renewal re-keys the agent, so the local copy must be refreshed too or
// every SSH-family verb dies on auth while RPC verbs keep working).
//
// The request authenticates the same way SpawnAgent does — "Bearer "+token —
// because building a fresh connect.Request without the header made an
// auth-enforced daemon answer "unauthenticated" (DF-BUNKER-59).
//
// Returns the saved key path. On RPC or write error returns ("", err) for the
// caller to warn about (spawn and renew treat the fetch as NON-FATAL); an
// empty key returns ("", nil) and writes nothing.
func fetchAndSaveAgentKey(ctx context.Context, client bunkerv1connect.BunkerdClient, agentID, token string) (string, error) {
	keyReq := connect.NewRequest(&v1.GetAgentKeyRequest{
		AgentId: agentID,
	})
	if token != "" {
		keyReq.Header().Set("Authorization", "Bearer "+token)
	}
	keyResp, err := client.GetAgentKey(ctx, keyReq)
	if err != nil {
		return "", err
	}
	key := keyResp.Msg.GetSshPrivateKey()
	if key == "" {
		return "", nil
	}
	cfgPath, err := configFilePath()
	if err != nil {
		return "", err
	}
	keyDir := filepath.Join(filepath.Dir(cfgPath), "keys")
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return "", err
	}
	keyPath := filepath.Join(keyDir, agentID)
	if err := os.WriteFile(keyPath, []byte(key), 0600); err != nil {
		return "", err
	}
	return keyPath, nil
}

// sshfsHomeRE extracts the remote path of an sshfs mount command
// ("<user>@<host>:<path> <mountpoint>" — the shape buildMountCommand
// produces). The path after the colon IS the agent home; this is how renew
// learns the OLD home from the stored record without any new wire field.
var sshfsHomeRE = regexp.MustCompile(`sshfs\s+.*?([A-Za-z0-9_.-]+@[A-Za-z0-9_.:-]+):(\S+)`)

// homeFromSSHFSMount returns the remote home path embedded in an sshfs mount
// command, or "" when it cannot be parsed (the drift pre-flight is then
// skipped with a named note, never a fabricated path).
func homeFromSSHFSMount(sshfsMount string) string {
	m := sshfsHomeRE.FindStringSubmatch(sshfsMount)
	if m == nil {
		return ""
	}
	return m[2]
}

// NewRenewCommand returns the `bunker renew` cobra command — the DF-BUNKER-34
// renewal recipe as a supported command. A renewal destroys and re-spawns an
// agent on the same host, and the ONE rule that makes that safe is that the
// new spawn carries the SAME --agent-id: the id is the home path
// (/home/bunker-<id>), the system user, the SSH identity and every stored
// path. An anonymous renewal mints a random id and therefore a new home path
// every time — the aa189273 -> eduos-agent -> 2cdce4d0 chain this row
// documents — so renew REFUSES to run without one, printing the recipe
// instead of proceeding with a fresh identity.
func NewRenewCommand() *cobra.Command {
	var (
		serverName string
		agentID    string
		ttl        string
	)
	cmd := &cobra.Command{
		Use:   "renew --agent-id <agent-id>",
		Short: "Renew an agent under its STABLE identity (destroy + re-spawn the same id)",
		Long: `Renew an agent by destroying it and re-spawning the SAME agent id.

The stable id is the whole point of a renewal: the agent id is the home path
(/home/bunker-<id>), the system user (bunker-<id>) and every stored path. A
renewal that spawns anonymously mints a NEW home path each time and leaves
every long-lived systemd --user unit, cron entry and config file pointing at
the old one (DF-BUNKER-34) — so this command REFUSES to run without
--agent-id, printing the recipe instead of minting a fresh identity.

What the command does:
  1. pre-flight drift report: scans the CURRENT agent home (read from the
     stored sshfs mount) for references to that old home path — systemd
     --user units, cron entries, shell/env and config files — and prints
     every hit;
  2. destroys the agent (the daemon's destroy refuses loudly if the uid
     still owns live processes — stop those on the host first);
  3. re-spawns the SAME agent id (the home path is therefore stable across
     the renewal).

The daemon-side restore of what the old home held is the renewal operator's
job — this command reports what will go stale and refuses to mint a new
identity. See docs/renewal.md for the full recipe, including the
pre-renewal archive step.

Examples:
  bunker renew --agent-id eduos-agent --ttl 7d
  bunker renew --agent-id eduos-agent --ttl 7d --server staging`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			// Step 0: the identity gate. This is the fail-loud refusal the
			// row names: an anonymous renewal IS the bug, so a missing
			// --agent-id is a hard error with the remedy in the message.
			if agentID == "" {
				return fmt.Errorf("renewal refused: no --agent-id given — renewals MUST carry the stable agent id " +
					"(`bunker renew --agent-id <id>`): spawning anonymously mints a new home path every renewal " +
					"and every long-lived service, cron entry and stored path under the old home goes stale " +
					"(DF-BUNKER-34; the recipe is docs/renewal.md)")
			}
			if !agentIDRe.MatchString(agentID) {
				return fmt.Errorf("invalid agent id %q: must match ^[a-z0-9-]{1,64}$ (lowercase letters, digits, hyphens only)", agentID)
			}

			// 1. Load CLI config + resolve server (fail-closed, like spawn).
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			resolved, berr := SessionScopedTarget(serverName, cfg.ActiveServer)
			if berr != nil {
				return berr
			}
			serverName = resolved
			entry, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("server %q not found in config", serverName)
			}

			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), renewOperationTimeout)
			defer cancel()
			token := resolveToken(entry)

			// 2. Read the CURRENT agent record: its home path (from the
			// stored sshfs mount) is the OLD home the drift report scans for.
			oldHome := ""
			infoReq := connect.NewRequest(&v1.GetAgentRequest{AgentId: agentID})
			if token != "" {
				infoReq.Header().Set("Authorization", "Bearer "+token)
			}
			infoResp, gerr := client.GetAgent(ctx, infoReq)
			if gerr == nil && infoResp.Msg.GetAgent() != nil {
				oldHome = homeFromSSHFSMount(infoResp.Msg.GetAgent().GetSshfsMount())
			} else {
				// Unknown id: a first renewal of a named id. Allowed (the
				// recipe's first run), with no pre-flight drift report.
				fmt.Fprintf(out, "agent %q not registered on %s (renewing as a first spawn of a named id; no pre-flight drift report)\n", agentID, serverName)
			}

			// 3. Pre-flight drift report: scan the OLD home's artifacts for
			// references to the OLD home path. The scan runs through the
			// daemon's RenewalDriftReport RPC (server-side, so it sees the
			// host's real files) and is REPORTED, never auto-rewritten.
			if oldHome != "" {
				fmt.Fprintf(out, "Pre-flight drift report for %s (searching for %s):\n", agentID, oldHome)
				dreq := connect.NewRequest(&v1.RenewalDriftRequest{AgentId: agentID, OldHome: oldHome})
				if token != "" {
					dreq.Header().Set("Authorization", "Bearer "+token)
				}
				dresp, derr := client.RenewalDriftReport(ctx, dreq)
				if derr != nil {
					// The pre-flight is best-effort reporting: a daemon that
					// predates the RPC warns instead of blocking the renewal.
					fmt.Fprintf(out, "  (warn: drift pre-flight unavailable: %v)\n", derr)
				} else {
					fmt.Fprintln(out, indentLines(dresp.Msg.GetSummary(), "  "))
				}
			}

			// 4. Destroy the agent. A live_processes / userdel_failed refusal
			// surfaces here with its full evidence — the operator must stop
			// the surviving processes on the host before a renewal proceeds.
			fmt.Fprintf(out, "Destroying agent %s...\n", agentID)
			dReq := connect.NewRequest(&v1.DestroyAgentRequest{AgentId: agentID})
			if token != "" {
				dReq.Header().Set("Authorization", "Bearer "+token)
			}
			dresp, derr := client.DestroyAgent(ctx, dReq)
			if derr != nil {
				return fmt.Errorf("renewal: destroy %s: %w", agentID, derr)
			}
			if s := dresp.Msg.Status; s != "destroyed" && s != "not_found" {
				return fmt.Errorf("renewal: destroy of %s refused (status %s); nothing was re-spawned — resolve the refusal and re-run", agentID, s)
			}

			// 5. Re-spawn the SAME id. The spawn request carries agent_id —
			// the wire field spawn has always had — so the home path, the
			// system user and every stored path stay stable across the
			// renewal.
			fmt.Fprintf(out, "Re-spawning agent %s (stable identity)...\n", agentID)
			sReq := connect.NewRequest(&v1.SpawnAgentRequest{
				AgentId: agentID,
				Ttl:     ttl,
			})
			if token != "" {
				sReq.Header().Set("Authorization", "Bearer "+token)
			}
			sresp, serr := client.SpawnAgent(ctx, sReq)
			if serr != nil {
				return fmt.Errorf("renewal: re-spawn %s: %w", agentID, serr)
			}
			if got := sresp.Msg.GetAgentId(); got != agentID {
				return fmt.Errorf("renewal: daemon returned agent id %q, want %q — refusing to report a stable renewal", got, agentID)
			}

			// DF-BUNKER-65: the re-spawned agent carries a FRESH host key pair,
			// so the client-local key file from the destroyed agent is dead —
			// without a re-fetch every SSH-family verb (ssh/cp/mount/deploy/
			// tunnel) fails auth while RPC verbs keep working. Re-fetch through
			// the same GAP-128 path spawn uses. The fetch is NON-FATAL: the
			// agent is running, so key recovery must not abort the renewal.
			if keyPath, kerr := fetchAndSaveAgentKey(ctx, client, agentID, token); kerr != nil {
				fmt.Fprintf(out, "  (warn: could not fetch SSH key: %v)\n", kerr)
			} else if keyPath != "" {
				fmt.Fprintln(out, "  SSH Key:      (saved to ~/.bunker/keys/)")
				fmt.Fprintf(out, "                %s\n", keyPath)
			}

			fmt.Fprintf(out, "Renewed: agent %s is running with its original identity (home path unchanged).\n", agentID)
			fmt.Fprintf(out, "Expires: %s\n", sresp.Msg.GetExpiresAt())
			return nil
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "REQUIRED: the stable agent id to renew (the home path /home/bunker-<id> and every stored path follow it)")
	cmd.Flags().StringVar(&ttl, "ttl", "", "TTL for the re-spawn (6h, 24h, 7d); empty = the daemon default")
	return cmd
}

// indentLines prefixes every non-empty line of s with prefix (the drift
// report renders under the command's own two-space indent).
func indentLines(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}
