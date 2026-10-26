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

	"github.com/deployBunker/bunker/internal/config"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// renewOperationTimeout is the client-side budget for one renewal. A renewal
// includes a full spawn (~90s rootless install on a fresh home), not one
// systemctl hop, so it cannot reuse the surface commands' 30s budget.
const renewOperationTimeout = 10 * time.Minute

// renewArchiveReminder is the pre-renewal archive instruction the failure
// paths carry (docs/renewal.md step 0). A renewal DESTROYS the home, so the
// tar taken before the destroy is the only copy that survives a re-spawn
// failure — the CLI reminds the operator at failure time because the recipe's
// step 0 is otherwise the only protection (DF-BUNKER-68). Kept as one format
// string so the re-spawn-failure error and the --no-destroy report cannot
// drift apart.
const renewArchiveReminder = "archive: docs/renewal.md step 0 (tar the agent home BEFORE a renewal) is the only copy that survives a failed re-spawn — restore it into /home/bunker-%s after the recovery spawn"

// appendRenewCarriedFlags appends the invocation's renewal flags (omitting
// the empty ones) so a printed recovery command reproduces the same request.
func appendRenewCarriedFlags(parts []string, ttl, preset, serverName string) []string {
	if ttl != "" {
		parts = append(parts, "--ttl "+ttl)
	}
	if preset != "" {
		parts = append(parts, "--preset "+preset)
	}
	if serverName != "" {
		parts = append(parts, "--server "+serverName)
	}
	return parts
}

// renewRecoverySpawnCommand renders the exact re-spawn-only recovery command
// for a failed renewal: it names the SAME agent id (plus the flags this
// invocation carried), because the id is the home path, the system user and
// every stored path — an anonymous recovery spawn would mint a fresh identity
// and re-open the aa189273 -> eduos-agent -> 2cdce4d0 chain of DF-BUNKER-34.
func renewRecoverySpawnCommand(agentID, ttl, preset, serverName string) string {
	return strings.Join(appendRenewCarriedFlags([]string{"bunker spawn " + agentID}, ttl, preset, serverName), " ")
}

// renewRerunCommand renders the FULL renewal command to re-run once the drift
// report is resolved — the pointer the --no-destroy arm ends with, so the
// report-only mode always hands back the destructive step as copy-paste text.
func renewRerunCommand(agentID, ttl, preset, serverName string) string {
	return strings.Join(appendRenewCarriedFlags([]string{"bunker renew --agent-id " + agentID}, ttl, preset, serverName), " ")
}

// renewRespawnFailure is the error the re-spawn leg returns once the destroy
// leg has ALREADY succeeded: the agent and its home are gone, so the renewal
// cannot be rolled back and the operator needs the recovery path in the error
// itself (DF-BUNKER-68). It carries three things: the one-line verdict with
// the recovery command, the pre-renewal archive reminder, and the re-spawn
// failure that caused it.
func renewRespawnFailure(agentID, ttl, preset, serverName string, cause error) error {
	return fmt.Errorf(
		"renew failed: agent destroyed but re-spawn failed; run 'bunker spawn %s' to recover\n"+
			"  the destroy leg already ran: agent %s and its home /home/bunker-%s are GONE — this renewal cannot be rolled back\n"+
			"  recovery (same stable id, so the home path and every stored path stay valid): %s\n"+
			"  %s\n"+
			"  re-spawn failure: %v",
		agentID,
		agentID, agentID,
		renewRecoverySpawnCommand(agentID, ttl, preset, serverName),
		fmt.Sprintf(renewArchiveReminder, agentID),
		cause,
	)
}

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
		serverName     string
		agentID        string
		ttl            string
		preset         string
		noDestroy      bool
		skipDriftCheck bool
		force          bool
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

--no-destroy is the report-only arm: it runs step 1 ONLY and exits. Nothing
is destroyed, nothing is re-spawned and no agent config is touched on the
host, so it is safe to run against an agent that must stay running (a drift
audit before a planned renewal, or a check of what a renewal would invalidate).

If the destroy leg succeeds and the re-spawn leg then FAILS, the error names
the state in full — the agent and its home are already gone — and carries
two recovery facts: the exact 'bunker spawn <id>' command that re-spawns the
same stable id, and the pre-renewal archive reminder (recipe step 0), which
is the only copy of the home that survives such a failure.
The pre-flight is a GATE, not a courtesy: a daemon that cannot serve it at
all (an older bunkerd without the RenewalDriftReport RPC) answers
CodeUnimplemented, and the renewal then ABORTS before the destroy with a
named error — destroying the agent anyway would take the old home down with
NO report of the stale references it holds. Two flags bypass it deliberately:
--skip-drift-check (the check is not needed, e.g. a renewal of an agent that
never wrote host-side state) and --force (emergency renewal: proceed even
when the check fails).

The daemon-side restore of what the old home held is the renewal operator's
job — this command reports what will go stale and refuses to mint a new
identity. See docs/renewal.md for the full recipe, including the
pre-renewal archive step.

Examples:
  bunker renew --agent-id eduos-agent --ttl 7d
  bunker renew --agent-id eduos-agent --ttl 7d --server staging
  bunker renew --agent-id eduos-agent --no-destroy    (report-only drift audit)`,
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

			// DF-BUNKER-67: validate --preset LOCALLY, exactly like spawn's
			// step 0.7 (GAP-116): an unknown preset name fails fast with the
			// accepted vocabulary, before the destroy ever runs. Empty defers
			// to BUNKERD_SAFETY_PRESET, then the daemon's config global, then
			// the built-in default — the daemon re-validates the resolved
			// value regardless. Without the flag a renewal re-spawns with an
			// empty preset and inherits whatever default the daemon resolves,
			// so an agent pinned to a containment preset must pass --preset
			// again at renew time.
			if preset != "" && !config.ValidSafetyPreset(preset) {
				return fmt.Errorf("invalid --preset %q (valid: %v)", preset, config.ValidSafetyPresets())
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
			agentKnown := false
			infoReq := connect.NewRequest(&v1.GetAgentRequest{AgentId: agentID})
			if token != "" {
				infoReq.Header().Set("Authorization", "Bearer "+token)
			}
			infoResp, gerr := client.GetAgent(ctx, infoReq)
			if gerr == nil && infoResp.Msg.GetAgent() != nil {
				agentKnown = true
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
			//
			// DF-BUNKER-70: a failed pre-flight ABORTS the renewal. It used
			// to be a one-line warning, so a daemon that predates the RPC
			// renewed the agent anyway and took the old home down with no
			// stale-reference report at all — the exact outcome the
			// pre-flight exists to prevent. A daemon without the RPC is a
			// HARD error unless the operator bypasses the gate deliberately
			// (--skip-drift-check) or overrides a failed check (--force).
			if oldHome != "" {
				switch {
				case skipDriftCheck:
					_, _ = fmt.Fprintf(out, "Pre-flight drift report for %s SKIPPED (--skip-drift-check): %s is not scanned for stale references\n", agentID, oldHome)
				default:
					_, _ = fmt.Fprintf(out, "Pre-flight drift report for %s (searching for %s):\n", agentID, oldHome)
					dreq := connect.NewRequest(&v1.RenewalDriftRequest{AgentId: agentID, OldHome: oldHome})
					if token != "" {
						dreq.Header().Set("Authorization", "Bearer "+token)
					}
					dresp, derr := client.RenewalDriftReport(ctx, dreq)
					if derr != nil {
						failure := driftPreflightFailure(derr, agentID, oldHome)
						if !force {
							return failure
						}
						_, _ = fmt.Fprintf(out, "  (warn: --force: continuing despite a failed drift pre-flight: %v)\n", failure)
					} else {
						_, _ = fmt.Fprintln(out, indentLines(dresp.Msg.GetSummary(), "  "))
					}
				}
			}

			// 4. --no-destroy (DF-BUNKER-68): the report-only arm stops
			// here. The drift pre-flight above IS the whole renewal — the
			// agent is left running, nothing is destroyed, nothing is
			// re-spawned, and no agent config is touched on the host. This
			// is the arm an operator runs against a live agent (drift audit
			// before a planned renewal) and the one that cannot destroy a
			// home whose re-spawn might then fail.
			if noDestroy {
				_, _ = fmt.Fprintf(out, "--no-destroy (report-only): the drift pre-flight above is the whole renewal — nothing was destroyed, nothing was re-spawned and no agent config was changed on %s.\n", serverName)
				if agentKnown {
					_, _ = fmt.Fprintf(out, "  agent %s is left RUNNING. To complete the renewal when the drift above is resolved, re-run without --no-destroy: %s\n",
						agentID, renewRerunCommand(agentID, ttl, preset, serverName))
					_, _ = fmt.Fprintf(out, "  %s\n", fmt.Sprintf(renewArchiveReminder, agentID))
				} else {
					_, _ = fmt.Fprintf(out, "  agent %q is not registered on %s, so there was no stored home to scan and nothing to renew — the empty report above is \"no record\", not \"no drift\".\n", agentID, serverName)
				}
				return nil
			}

			// 5. Destroy the agent. A live_processes / userdel_failed refusal
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

			// 6. Re-spawn the SAME id. The spawn request carries agent_id —
			// the wire field spawn has always had — so the home path, the
			// system user and every stored path stay stable across the
			// renewal. DF-BUNKER-67: it now also carries the safety preset,
			// so renewing an agent that runs under a containment preset
			// re-spawns under the SAME preset instead of silently inheriting
			// the daemon's default (empty = daemon resolution, unchanged).
			//
			// DF-BUNKER-68: this leg runs AFTER the destroy has already
			// deleted the agent and its home. A failure here is therefore not
			// a failed renewal the operator can simply re-run — the agent is
			// gone — so every failure return carries the recovery path
			// (re-spawn the same stable id) and the pre-renewal archive
			// reminder instead of only naming the spawn stage.
			fmt.Fprintf(out, "Re-spawning agent %s (stable identity)...\n", agentID)
			sReq := connect.NewRequest(&v1.SpawnAgentRequest{
				AgentId:      agentID,
				Ttl:          ttl,
				SafetyPreset: preset,
			})
			if token != "" {
				sReq.Header().Set("Authorization", "Bearer "+token)
			}
			sresp, serr := client.SpawnAgent(ctx, sReq)
			if serr != nil {
				return renewRespawnFailure(agentID, ttl, preset, serverName, fmt.Errorf("re-spawn %s: %w", agentID, serr))
			}
			if got := sresp.Msg.GetAgentId(); got != agentID {
				return renewRespawnFailure(agentID, ttl, preset, serverName,
					fmt.Errorf("daemon returned agent id %q, want %q — refusing to report a stable renewal", got, agentID))
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
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set; mutating commands never fall back to the shared active default)")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "REQUIRED: the stable agent id to renew (the home path /home/bunker-<id> and every stored path follow it)")
	cmd.Flags().StringVar(&ttl, "ttl", "", "TTL for the re-spawn (6h, 24h, 7d); empty = the daemon default")
	cmd.Flags().StringVar(&preset, "preset", "", "Safety preset for the re-spawn: open, standard, hardened (default: BUNKERD_SAFETY_PRESET, then the server's config, then the built-in default standard (GAP-117))")
	cmd.Flags().BoolVar(&noDestroy, "no-destroy", false,
		"Report-only arm: run the drift pre-flight and exit WITHOUT destroying or re-spawning (safe against a running agent; no agent config is changed)")
	cmd.Flags().BoolVar(&skipDriftCheck, "skip-drift-check", false, "Skip the pre-flight drift check entirely (no stale-reference report; use when the agent's old home holds no host-side state, or on a daemon that cannot serve the check)")
	cmd.Flags().BoolVar(&force, "force", false, "Proceed even when the pre-flight drift check FAILS (emergency renewals: the stale-reference report is unavailable and the renewal deliberately continues)")
	return cmd
}

// driftPreflightFailure builds the DF-BUNKER-70 abort error for a failed
// RenewalDriftReport call. The two failure classes are named apart because
// the operator's remedy differs:
//
//   - CodeUnimplemented: the daemon predates the RPC, so no report is
//     possible at all — upgrade the daemon (or bypass with --skip-drift-check
//     / --force);
//   - anything else: the check exists but this run could not complete —
//     retry, or override deliberately with --force.
//
// Both carry the agent id, the old home that would go stale and the fact
// that NOTHING was destroyed (the caller returns the error before the
// destroy step).
func driftPreflightFailure(derr error, agentID, oldHome string) error {
	if connect.CodeOf(derr) == connect.CodeUnimplemented {
		return fmt.Errorf("renewal ABORTED before destroying %q: this daemon does not support the renewal drift-check RPC "+
			"(RenewalDriftReport is unimplemented on it), so the renewal cannot report the stale references the old home %s "+
			"holds (systemd --user units, cron entries, shell/env and config files). Nothing was destroyed. "+
			"Upgrade bunkerd to a build that serves RenewalDriftReport, or bypass the pre-flight deliberately: "+
			"--skip-drift-check (no check needed) or --force (emergency renewal): %w",
			agentID, oldHome, derr)
	}
	return fmt.Errorf("renewal ABORTED before destroying %q: the drift pre-flight FAILED for old home %s, so the renewal "+
		"cannot report what will go stale. Nothing was destroyed. Retry the pre-flight, or bypass it deliberately with "+
		"--skip-drift-check / --force: %w", agentID, oldHome, derr)
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
