package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/config"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// destroyRequestTimeout is the client-side deadline `bunker destroy` uses when
// it CANNOT resolve the agent's home size.
//
// DF-BUNKER-81: the deadline is SIZE-DERIVED — the daemon archives the whole
// home before userdel (DF-BUNKER-33) and runs that `tar` under its own budget
// (config.ArchiveBudgetForHomeSize), so the client must outlive the archive it
// is waiting on. A fixed literal cannot do that: the historical 30s value
// SIGKILLed a ~300MB image-spec home's archive at 25s (INT-CI-050, CI run
// 36487719950: `archive home /home/bunker-e2e-imgspec: signal: killed`), and
// on a rootless-docker home the docker data-root lives INSIDE $HOME, so the
// archive got slower still and the destroy became UNFINISHABLE — five
// attempts, five fail-closed refusals (home_retained), agent still running.
//
// This var is the FLOOR (size unknown: a probe that failed, an unreachable
// daemon, a daemon too old to answer AgentMetrics). When the probe below
// succeeds, the deadline grows with the home (destroyDeadlineForHomeSize). A
// var (not a const) purely as a test seam — production never writes it.
var destroyRequestTimeout = config.DestroyRequestTimeoutForHomeSize(0)

// destroyHomeSizeProbeTimeout bounds the pre-destroy size probe. It is
// deliberately short: the probe is an OPTIMISATION (it buys a deadline sized to
// the home), and a daemon that cannot answer within it must not delay the
// destroy — the size-unknown floor above is already safe.
var destroyHomeSizeProbeTimeout = 15 * time.Second

// destroyHomeSizeProbe resolves the agent's on-disk footprint in bytes. The
// daemon serves it from its per-agent disk-usage snapshot cache
// (internal/server diskusage.go: one home walk per agent per 5m TTL), so a
// destroy following a `bunker list`/`bunker info` pays nothing. It is
// BEST-EFFORT: any error means "size unknown" and the caller falls back to
// destroyRequestTimeout. Var (not a func) purely as a test seam.
var destroyHomeSizeProbe = func(ctx context.Context, client bunkerv1connect.BunkerdClient, token, agentID string) (uint64, error) {
	req := connect.NewRequest(&v1.AgentMetricsRequest{AgentId: agentID})
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}
	resp, err := client.AgentMetrics(ctx, req)
	if err != nil {
		return 0, err
	}
	return resp.Msg.GetDiskUsedBytes(), nil
}

// destroyDeadlineForHomeSize returns the client deadline for destroying an
// agent whose home holds homeBytes bytes. An unknown size (0) keeps the
// configured floor — never a shorter deadline than the seam already holds, so
// a test that shrinks destroyRequestTimeout keeps control of the RPC budget.
func destroyDeadlineForHomeSize(homeBytes uint64) time.Duration {
	if homeBytes == 0 {
		return destroyRequestTimeout
	}
	deadline := config.DestroyRequestTimeoutForHomeSize(int64(homeBytes))
	if deadline < destroyRequestTimeout {
		return destroyRequestTimeout
	}
	return deadline
}

// destroyProgressLine renders what the operator is about to wait for: the
// home's measured size, whether it will be archived (or deleted outright), and
// the deadline the client will allow. DF-BUNKER-81 prints the size because the
// size IS the reason a destroy takes minutes, and the operator needs to be
// able to tell "slow because the home is 700M" from "slow because something
// is wrong".
func destroyProgressLine(agentID string, homeBytes uint64, deadline time.Duration, skipArchive bool) string {
	size := "home size unknown"
	if homeBytes > 0 {
		size = "home " + humanBytes(homeBytes)
	}
	if skipArchive {
		return fmt.Sprintf("Destroying agent %s (%s) — archive skipped: the home will be deleted with NO copy; deadline %s…",
			agentID, size, deadline)
	}
	return fmt.Sprintf("Destroying agent %s (%s; archiving before delete, deadline %s)…",
		agentID, size, deadline)
}

// resolveArchiveChoice folds the two operator spellings — --archive (default
// true) and its shorthand --purge — into ONE decision, and refuses the
// contradictory combination instead of silently picking a side.
func resolveArchiveChoice(cmd *cobra.Command, archive, purge bool) (bool, error) {
	if purge && cmd.Flags().Changed("archive") && archive {
		return false, fmt.Errorf("--purge and --archive=true contradict each other: pass one of them")
	}
	return purge || !archive, nil
}

// NewDestroyCommand returns the `bunker destroy` cobra command.
func NewDestroyCommand() *cobra.Command {
	var (
		serverName string
		force      bool
		keepKey    bool
		archive    bool
		purge      bool
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

ARCHIVING (DF-BUNKER-81). The client deadline and the daemon's archive
budget are derived from the home's SIZE, and this command prints the
measured size and its deadline before sending the request — a large home
is slow because it is large, not because the destroy is stuck. The
agent's own rootless docker data-root (<home>/.local/share/docker —
container and overlay layers) is EXCLUDED from every archive: it is
runtime state, not user data, and the daemon rebuilds it on demand.

--archive=false (or --purge) SKIPS the archive for THIS destroy: the home
is deleted with NO copy anywhere. Use it when the archive is the blocker —
for example a home dominated by runtime state — and the contents are
expendable. It is a per-destroy choice: every other destroy (and the TTL
reaper) still runs under the configured policy.

Examples:
  bunker destroy abc12345
  bunker destroy abc12345 --force
  bunker destroy abc12345 --archive=false
  bunker destroy abc12345 --purge
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

			// 3. Operator's archive choice (DF-BUNKER-81 criterion 4)
			skipArchive, aerr := resolveArchiveChoice(cmd, archive, purge)
			if aerr != nil {
				return aerr
			}

			// 4. Build request
			client := newBunkerdClient(entry)
			token := resolveToken(entry)

			// DF-BUNKER-81 criterion 1: size the deadline from the agent's
			// actual footprint. The probe is best-effort — a daemon that
			// cannot answer leaves homeBytes at 0 and the floor deadline
			// applies — and it never fails the destroy.
			homeBytes := uint64(0)
			sizeCtx, sizeCancel := context.WithTimeout(context.Background(), destroyHomeSizeProbeTimeout)
			if size, serr := destroyHomeSizeProbe(sizeCtx, client, token, agentID); serr == nil {
				homeBytes = size
			}
			sizeCancel()

			deadline := destroyDeadlineForHomeSize(homeBytes)
			fmt.Println(destroyProgressLine(agentID, homeBytes, deadline, skipArchive))

			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()

			req := connect.NewRequest(&v1.DestroyAgentRequest{
				AgentId:     agentID,
				Force:       force,
				SkipArchive: skipArchive,
			})

			// Auth token
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}

			// 5. Call RPC
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

			// 6. Print result
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

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set; mutating commands never fall back to the shared active default)")
	cmd.Flags().BoolVar(&force, "force", false, "Force destroy even if agent is running")
	cmd.Flags().BoolVar(&keepKey, "keep-key", false, "Keep the local SSH key (~/.bunker/keys/<id>) after destroy (key rotation)")
	cmd.Flags().BoolVar(&archive, "archive", true, "Archive the agent home to the daemon's destroy_archive_dir before deletion (default). --archive=false deletes the home with NO copy — the per-destroy equivalent of destroy_home_policy: purge (see also --purge)")
	cmd.Flags().BoolVar(&purge, "purge", false, "Shorthand for --archive=false: delete the home with NO archive and NO copy kept")

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
