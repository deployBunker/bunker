package cli

import (
	"context"
	"errors"
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

// PERF-013 poll knobs, INSIDE the destroyHomeSizeProbeTimeout budget above
// (never on top of it): destroyHomeSizePollWindow is how long the size probe
// may keep polling past a cold disk-usage snapshot cache (internal/server
// diskusage.go answers DiskUsedBytes=0 until the daemon's first per-agent home
// walk lands, within its 5-minute TTL), and destroyHomeSizePollBackoff is the
// sleep between polls. Vars (not consts) purely as test seams — production
// never writes them.
var (
	destroyHomeSizePollWindow  = 10 * time.Second
	destroyHomeSizePollBackoff = 250 * time.Millisecond
)

// destroyHomeSizeProbeWithRetry polls probe until it reports a usable size or
// the poll window expires. A cold cache serves 0 for a moment before the
// daemon's background walk lands, so a 0 answer is treated as "not yet known"
// and retried — but a probe ERROR means the daemon cannot answer metrics at
// all, so that is fail-closed single-attempt (one try, no hammering). The
// window is subdivided out of destroyHomeSizeProbeTimeout's budget, so the
// probe as a whole still never delays the destroy beyond it.
func destroyHomeSizeProbeWithRetry(ctx context.Context, probe func(context.Context) (uint64, error)) (uint64, error) {
	deadline := time.Now().Add(destroyHomeSizePollWindow)
	for {
		size, err := probe(ctx)
		if err == nil && size > 0 {
			return size, nil
		}
		if err != nil || !time.Now().Before(deadline) || ctx.Err() != nil {
			if err != nil {
				return 0, err
			}
			return 0, fmt.Errorf("home size still unknown after %s of polling: %w", destroyHomeSizePollWindow, context.DeadlineExceeded)
		}
		select {
		case <-time.After(destroyHomeSizePollBackoff):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

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

// DF-BUNKER-61: a client deadline expiry is NOT the daemon's verdict.
//
// The repro (dogfood 2026-09-25): `bunker destroy bc0e2890` failed three times
// with `destroy agent: deadline_exceeded: context deadline exceeded` while the
// SAME DestroyAgent over raw REST answered 200 {"status":"destroyed"} in
// seconds — the agent had run `make test-short` (~960MB home) and the CLI's
// deadline ran out while the daemon's teardown was still (successfully)
// working. The CLI reported a hard failure for an operation that completed:
// the operator cannot tell an aborted teardown from a finished one, and the
// natural reaction (retry) is unnecessary at best.
//
// The confirmation is a RE-LIST, never a resent DestroyAgent. A resend can race
// the daemon's in-flight archive+userdel (two archives, two userdels on one
// home — the opposite of idempotence); listing only READS, so it can never
// corrupt a teardown in progress. Absence from the daemon's own registry is the
// truthful signal that the destroy finished: the daemon's destroy handler is
// what removes the record.
//
// These knobs are vars (not consts) purely as test seams — production never
// writes them.
var (
	// destroyRelistWindow bounds how long the confirmation polls for the agent
	// to disappear. The daemon's teardown continues after the client gives up,
	// so an immediate single look would under-report; a bounded window keeps a
	// genuinely wedged destroy from hanging the CLI for a second deadline.
	destroyRelistWindow = 20 * time.Second

	// destroyRelistBackoff is the sleep between confirmation polls.
	destroyRelistBackoff = time.Second

	// destroyListPageSize / destroyListMaxPages bound the paging of the
	// existence check: a hostile or broken daemon that never clears its page
	// token must not spin the CLI forever.
	destroyListPageSize uint32 = 500
	destroyListMaxPages        = 20
)

// destroyListPageTimeout bounds ONE ListAgents page call during the
// confirmation, so a hung read cannot consume the whole re-list window.
var destroyListPageTimeout = 10 * time.Second

// destroyAgentListed reports whether the daemon's registry still carries
// agentID, paging through ListAgents until it finds the agent or runs out of
// pages. An ERROR (an old daemon without the RPC, a permission refusal, a
// dropped connection) is NOT "absent": the caller must fail closed and say it
// could not confirm, never claim a destroy it did not witness.
//
// Var (not a func) purely as a test seam.
var destroyAgentListed = func(ctx context.Context, client bunkerv1connect.BunkerdClient, token, agentID string) (bool, error) {
	pageToken := ""
	for page := 0; page < destroyListMaxPages; page++ {
		pageCtx, cancel := context.WithTimeout(ctx, destroyListPageTimeout)
		req := connect.NewRequest(&v1.ListAgentsRequest{
			PageSize:  destroyListPageSize,
			PageToken: pageToken,
		})
		if token != "" {
			req.Header().Set("Authorization", "Bearer "+token)
		}
		resp, err := client.ListAgents(pageCtx, req)
		cancel()
		if err != nil {
			return false, err
		}
		for _, a := range resp.Msg.GetAgents() {
			if a.GetAgentId() == agentID {
				return true, nil
			}
		}
		pageToken = resp.Msg.GetNextPageToken()
		if pageToken == "" {
			return false, nil
		}
	}
	return false, fmt.Errorf("agent list did not terminate within %d pages", destroyListMaxPages)
}

// destroyConfirmAfterDeadline polls listed until the agent is gone or
// destroyRelistWindow expires. It returns (true, nil) when the daemon's
// registry no longer carries the agent — the truthful "the teardown finished"
// signal — and (false, nil) when the agent is still registered after the
// window. A listing ERROR returns immediately (fail-closed after ONE attempt:
// polling a refused RPC only delays the honest report).
func destroyConfirmAfterDeadline(ctx context.Context, listed func(context.Context) (bool, error)) (bool, error) {
	deadline := time.Now().Add(destroyRelistWindow)
	for {
		stillListed, err := listed(ctx)
		if err != nil {
			return false, err
		}
		if !stillListed {
			return true, nil
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			return false, nil
		}
		select {
		case <-time.After(destroyRelistBackoff):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// destroyIsDeadlineExceeded reports whether err is a client-side deadline
// expiry rather than a verdict from the daemon. Only this class is re-checked:
// a home_retained refusal or a live-processes refusal is the daemon TELLING us
// the destroy did not happen, and re-listing would add nothing.
func destroyIsDeadlineExceeded(err error) bool {
	if err == nil {
		return false
	}
	if connect.CodeOf(err) == connect.CodeDeadlineExceeded {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
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
		timeout    time.Duration
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
--timeout replaces that derived deadline with an explicit one.

A CLIENT DEADLINE IS NOT A VERDICT (DF-BUNKER-61). If the client's
deadline expires while the daemon is still working, the daemon's
teardown keeps running — so this command re-lists the daemon's agents
before reporting anything: if the agent is gone, the destroy is reported
as done (exit 0); if the agent is still listed, the command says the
destroy is NOT CONFIRMED and exits non-zero. It never resends the
destroy while one may be in flight.

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
  bunker destroy abc12345 --timeout 45m
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
			// actual footprint. PERF-013: the probe polls past a cold
			// disk-usage snapshot cache (an immediate 0 is "not yet known",
			// not "known empty") inside the existing probe budget; a probe
			// error or a window that expires still leaves homeBytes at 0
			// and the floor deadline applies. It never fails the destroy.
			//
			// DF-BUNKER-61: --timeout is the operator's explicit override of
			// the whole client deadline. It REPLACES the size derivation,
			// because that is the only reading a script can rely on ("wait
			// exactly this long"), and when it is passed the size probe is
			// skipped entirely — its sole purpose was to size the deadline,
			// so with an explicit deadline it would be a wasted round trip
			// against a daemon that is about to be busy tearing down.
			homeBytes := uint64(0)
			var deadline time.Duration
			if timeout < 0 {
				return fmt.Errorf("--timeout must be a positive duration (got %s); pass 0 (the default) to derive it from the home's size", timeout)
			}
			if timeout > 0 {
				deadline = timeout
			} else {
				sizeCtx, sizeCancel := context.WithTimeout(context.Background(), destroyHomeSizeProbeTimeout)
				if size, serr := destroyHomeSizeProbeWithRetry(sizeCtx, func(ctx context.Context) (uint64, error) {
					return destroyHomeSizeProbe(ctx, client, token, agentID)
				}); serr == nil {
					homeBytes = size
				}
				sizeCancel()
				deadline = destroyDeadlineForHomeSize(homeBytes)
			}

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
				// DF-BUNKER-61: a client deadline expiry is not the daemon's
				// verdict, so it must not be reported as one. Re-list before
				// reporting failure: the daemon's teardown keeps running
				// after the client gives up, and a destroy that COMPLETED is
				// the difference between "your agent is gone" and "your agent
				// may still be there, retry" — the two things an operator
				// acts on. (Never a resent DestroyAgent: that could race the
				// in-flight archive+userdel.)
				if destroyIsDeadlineExceeded(err) {
					confirmCtx, confirmCancel := context.WithTimeout(context.Background(), destroyRelistWindow+destroyListPageTimeout)
					gone, cerr := destroyConfirmAfterDeadline(confirmCtx, func(ctx context.Context) (bool, error) {
						return destroyAgentListed(ctx, client, token, agentID)
					})
					confirmCancel()
					switch {
					case cerr == nil && gone:
						fmt.Printf("Agent %s destroyed — the daemon finished the teardown after the client's %s deadline; it is gone from the daemon's agent list.\n", agentID, deadline)
						return removeLocalSSHKey(agentID, keepKey)
					case cerr == nil:
						fmt.Printf("Agent %s NOT confirmed destroyed: the client deadline (%s) expired and the daemon still lists the agent after %s of re-checking.\n", agentID, deadline, destroyRelistWindow)
						fmt.Printf("  The teardown may still be running — re-check with `bunker info %s` before retrying (destroy is idempotent).\n", agentID)
						fmt.Printf("  For a home that needs longer than the derived deadline, raise it: bunker destroy %s --timeout 45m\n", agentID)
					default:
						fmt.Printf("Agent %s NOT confirmed destroyed: the client deadline (%s) expired and the daemon could not be asked to confirm (%v).\n", agentID, deadline, cerr)
						fmt.Printf("  Re-check with `bunker info %s` before retrying (destroy is idempotent).\n", agentID)
					}
					return fmt.Errorf("destroy agent: %w", err)
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
				// MISCHIEF-DF-24: the live-process refusal is the OTHER
				// fail-closed refusal, and the RPC error that carries it is a
				// wall of wrapped connect noise. Print the same plain guidance
				// line the home-retained case prints — the exact kill command
				// and the --force exit hatch — so the remediation is actionable
				// without parsing the error. Nothing was deleted either way.
				if strings.Contains(err.Error(), "live processes") {
					fmt.Printf("Agent %s NOT destroyed: the agent user still owns live processes (nothing was deleted).\n", agentID)
					fmt.Printf("  Kill them and retry:   bunker ssh %s -- pkill -U <uid of bunker-%s>\n", agentID, agentID)
					fmt.Printf("  Or let destroy do it:  bunker destroy %s --force\n", agentID)
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
	cmd.Flags().BoolVar(&force, "force", false, "Kill the agent user's live processes (SIGTERM, then SIGKILL escalation) and destroy anyway — the kill-first exit hatch for a live_processes refusal; the home is still archived first")
	cmd.Flags().BoolVar(&keepKey, "keep-key", false, "Keep the local SSH key (~/.bunker/keys/<id>) after destroy (key rotation)")
	cmd.Flags().BoolVar(&archive, "archive", true, "Archive the agent home to the daemon's destroy_archive_dir before deletion (default). --archive=false deletes the home with NO copy — the per-destroy equivalent of destroy_home_policy: purge (see also --purge)")
	cmd.Flags().BoolVar(&purge, "purge", false, "Shorthand for --archive=false: delete the home with NO archive and NO copy kept")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Explicit client deadline for the destroy RPC (e.g. 45m). Default 0 derives it from the agent home's measured size (DF-BUNKER-81) so the client always outlives the daemon's archive; an explicit value replaces that derivation and skips the size probe. A deadline shorter than the daemon's archive budget cancels the archive and the daemon then refuses the destroy with the home retained")

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
