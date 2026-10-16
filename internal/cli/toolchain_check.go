// `bunker toolchain-check AGENT_ID` (SURF-009): report the DECLARED build
// toolchain set (go, gcc, make, node, cargo) present/absent BY NAME on an
// agent, and optionally deliver the missing ones in place.
//
// The command is the agent-tools probe shape reused, not a second design:
// same audited exec path (one authentication story, one audit trail), same
// "a finding is DATA" contract (a missing tool exits 0 with a named list;
// only a probe that could not RUN exits non-zero), same
// probe-first/deliver/re-probe flow as `agent-tools --install` — the
// delivered state is proven by a fresh probe, never claimed by the stage.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/provisioning"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// toolchainStageBudget bounds ONE delivery stage (a toolchain download +
// extract legitimately takes minutes on a cold agent); the parent command's
// --timeout must cover the whole delivery, each stage gets what is left up
// to this cap.
const toolchainStageBudget = 600 * time.Second

// NewToolchainCheckCommand returns the `bunker toolchain-check` command.
func NewToolchainCheckCommand() *cobra.Command {
	var serverName string
	var asJSON bool
	var timeout uint32
	var deliver bool

	cmd := &cobra.Command{
		Use:   "toolchain-check AGENT_ID",
		Short: "Report the declared build toolchains (go, gcc, make, node, cargo) present/absent by name on the agent",
		Long: `Probe the AGENT (not the client) for the declared build-toolchain set
and name every entry present or absent.

A fresh agent was measured (SURF-009) to have python3 only: go, gcc, make,
node and cargo were ALL absent — which makes "the bunker does the building"
untrue for every Go/Rust/C/JS project a mount can put in front of an agent.
This command is the checkable form of that declaration: one command, every
name, an explicit verdict (all_present in the JSON shape).

A missing tool is DATA (exit 0 with a named list), matching ` + "`bunker probe`" + `
and ` + "`bunker agent-tools`" + ` contracts. Only a probe that could not run exits
non-zero.

With --deliver the missing toolchains are installed onto the agent — without
root, into the agent's own HOME — and the agent is re-probed so the result is
evidence rather than a claim:
  - go    pinned dl.google.com release tarball (checksum-verified; the same
          pin ` + "`agent-tools --install`" + `'s gopls bootstrap uses);
  - gcc   the apt archive itself: the release gcc package's dependency
          closure resolved by apt (apt-cache depends --recurse), downloaded
          with apt-get download (the SAME signature-verified .deb files an
          install would use) and unpacked into $HOME; the $HOME/bin entry is
          a wrapper the stage writes FROM the extracted layout, and the
          stage's verdict is a compile-link-run proof;
  - make  the apt ` + "`make`" + ` package (single-binary extract);
  - node  pinned nodejs.org release tarball (SHASUMS256.txt-verified),
          npm/npx symlinks included;
  - cargo pinned rustup installer (--profile minimal, --no-modify-path),
          with cargo/rustc symlinked from the installed toolchain.
Every toolchain is cached under $HOME/.bunker/toolchains, so re-runs are
no-op probes. Agents WITH the image-spec surface can instead bake the C/Make
half into the image at spawn: the directive below is a valid --image-spec
document.

Examples:
  bunker toolchain-check abc12345
  bunker toolchain-check abc12345 --json
  bunker toolchain-check abc12345 --deliver
  bunker toolchain-check abc12345 --deliver --server staging`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			if !agentIDRe.MatchString(agentID) {
				return fmt.Errorf("invalid agent id %q: must match %s", agentID, agentIDRe.String())
			}

			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			serverName = ReadOnlyTarget(serverName, cfg.ActiveServer)
			if serverName == "" {
				return fmt.Errorf("no active server; run 'bunker connect' first")
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "bunker: reading server %q\n", serverName)
			entry, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("server %q not found in config", serverName)
			}

			client := newBunkerdClient(entry)
			ctxTimeout := time.Duration(timeout) * time.Second
			if ctxTimeout <= 0 {
				ctxTimeout = 60 * time.Second
			}
			if deliver && ctxTimeout < 30*time.Minute {
				ctxTimeout = 30 * time.Minute
			}
			ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
			defer cancel()

			if deliver {
				// Deliver with progress on stderr; the final verdict is
				// the re-probe below, printed like a plain check.
				if err := deliverToolchains(cmd.ErrOrStderr(), ctx, client, entry, agentID); err != nil {
					return err
				}
			}

			report, err := probeToolchains(ctx, client, entry, agentID)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			writeToolchainReport(out, report)
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the report as JSON")
	cmd.Flags().Uint32Var(&timeout, "timeout", 60, "Probe timeout in seconds (--deliver raises the floor to 30m)")
	cmd.Flags().BoolVar(&deliver, "deliver", false,
		"Install the missing toolchains onto the agent's own HOME (no root), then re-probe")
	return cmd
}

// probeToolchains runs the declared-set probe on the agent and parses it.
func probeToolchains(ctx context.Context, client bunkerv1connect.BunkerdClient, entry ServerEntry, agentID string) (provisioning.Report, error) {
	stdout, exitCode, err := execOnAgentScript(ctx, client, entry, agentID, provisioning.ProbeScript(), uint32(ctxDeadlineSeconds(ctx)))
	if err != nil {
		return provisioning.Report{}, fmt.Errorf("probe agent toolchains: %w", err)
	}
	if exitCode != 0 {
		// The probe ERRORING is a capability question, not a missing
		// tool: report the agent's own words rather than inventing one.
		return provisioning.Report{}, fmt.Errorf("toolchain probe exited %d on the agent", exitCode)
	}
	return provisioning.ParseProbeOutput(agentID, stdout, exitCode), nil
}

// deliverToolchains probes first, runs one delivery stage per missing entry
// through the audited exec path, and re-probes at the end. The VERDICT is
// the re-probe: any entry still absent fails the command with the stage's
// own words. Progress lines go to errOut so callers can keep stdout clean.
func deliverToolchains(errOut io.Writer, ctx context.Context, client bunkerv1connect.BunkerdClient, entry ServerEntry, agentID string) error {
	before, err := probeToolchains(ctx, client, entry, agentID)
	if err != nil {
		return err
	}
	if before.AllPresent {
		_, _ = fmt.Fprintln(errOut, "bunker: every declared toolchain is already present; nothing to deliver")
		return nil
	}

	var stageFailures []string
	for _, f := range before.Tools {
		if f.Status == "present" {
			continue
		}
		script, serr := provisioning.StageScript(f.Name)
		if serr != nil {
			stageFailures = append(stageFailures, fmt.Sprintf("%s: %v", f.Name, serr))
			continue
		}
		_, _ = fmt.Fprintf(errOut, "bunker: delivering %s (%s) into the agent's $HOME/%s\n",
			f.Name, f.Kind, provisioning.InstallDir)

		budget := stageBudgetSeconds(ctx, toolchainStageBudget)
		out, exitCode, xerr := execOnAgentScript(ctx, client, entry, agentID, script, budget)
		if xerr != nil {
			return fmt.Errorf("deliver %s on agent %q: %w", f.Name, agentID, xerr)
		}
		if exitCode != 0 && strings.TrimSpace(out) == "" {
			return fmt.Errorf("deliver %s on agent %q: the agent exited %d with no output",
				f.Name, agentID, exitCode)
		}
		outcome := provisioning.ParseStageOutput(f.Name, out)
		switch outcome.Status {
		case "INSTALLED":
			_, _ = fmt.Fprintf(errOut, "bunker: %s delivered (%s)\n", f.Name, outcome.Detail)
		case "FAILED":
			stageFailures = append(stageFailures, fmt.Sprintf("%s: %s", f.Name, outcome.Detail))
			_, _ = fmt.Fprintf(errOut, "bunker: WARNING %s delivery FAILED: %s\n", f.Name, outcome.Detail)
		default:
			stageFailures = append(stageFailures, fmt.Sprintf("%s: stage ended in state %q (%s)",
				f.Name, outcome.Status, outcome.Detail))
		}
	}

	// The verdict is the re-probe, not the stages' own words.
	after, err := probeToolchains(ctx, client, entry, agentID)
	if err != nil {
		return err
	}
	if !after.AllPresent {
		return fmt.Errorf("toolchain delivery incomplete on agent %q: still absent after delivery: %s%s",
			agentID, strings.Join(after.Missing, ", "), stageFailureNote(stageFailures))
	}
	if len(stageFailures) > 0 {
		// Every stage failed BUT the re-probe is clean: the tools were
		// delivered by another path mid-flight (or were already present
		// under names the probe resolves). Present is present — the
		// evidence is the probe — but the contradiction is named.
		_, _ = fmt.Fprintf(errOut, "bunker: note: %d stage(s) reported failure yet the re-probe shows the set present; stages: %s\n",
			len(stageFailures), strings.Join(stageFailures, "; "))
	}
	return nil
}

// stageFailureNote formats the stage-cause suffix for the incomplete verdict.
func stageFailureNote(failures []string) string {
	if len(failures) == 0 {
		return ""
	}
	return " (causes: " + strings.Join(failures, "; ") + ")"
}

// writeToolchainReport renders the human form.
func writeToolchainReport(out io.Writer, report provisioning.Report) {
	_, _ = fmt.Fprintf(out, "\nDeclared build toolchains (%s, set: %s)\n\n", report.Agent, report.Set)
	_, _ = fmt.Fprintf(out, "  %-6s %-8s %-16s %s\n", "TOOL", "STATUS", "DELIVERED AS", "VERSION / WHAT IT BUILDS")
	for _, f := range report.Tools {
		detail := f.Version
		if detail == "" {
			detail = f.Builds
		}
		_, _ = fmt.Fprintf(out, "  %-6s %-8s %-16s %s\n", f.Name, f.Status, f.Kind, detail)
	}
	_, _ = fmt.Fprintln(out)
	if report.AllPresent {
		_, _ = fmt.Fprintln(out, "  Every declared toolchain is present: this agent can build Go, C, Make, JS/TS and Rust projects.")
		return
	}
	_, _ = fmt.Fprintf(out, "  MISSING on the agent: %s\n", strings.Join(report.Missing, ", "))
	_, _ = fmt.Fprintln(out, "  Deliver in place (no root):   bunker toolchain-check "+report.Agent+" --deliver")
	_, _ = fmt.Fprintln(out, "  Or bake into the image at spawn (--image-spec directive):")
	_, _ = fmt.Fprintf(out, "    %s\n", provisioning.ImageSpecDirective)
}
