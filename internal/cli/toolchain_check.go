// `bunker toolchain-check AGENT_ID` (SURF-009): report the DECLARED build
// toolchain CLASS (minimal, go, full) present/absent BY NAME on an agent, and
// optionally deliver the missing ones in place.
//
// The command is the agent-tools probe shape reused, not a second design:
// same audited exec path (one authentication story, one audit trail), same
// probe-first/deliver/re-probe flow as `agent-tools --install` — the
// delivered state is proven by a fresh probe, never claimed by the stage.
//
// Exit code: this is the OFFLOAD GATE. `bunker toolchain-check X --class go`
// is what a driver runs before shipping a Go build to X, so an incomplete
// class exits 1 with the missing names. `--report-only` keeps the reporting
// contract for callers that want the answer as data (the shape
// `bunker agent-tools` uses): report, exit 0. A probe that could not RUN
// exits non-zero either way — that is a failure of the check, not a finding.
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
	var class string
	var reportOnly bool

	cmd := &cobra.Command{
		Use:   "toolchain-check AGENT_ID",
		Short: "Report a declared build-toolchain class (minimal, go, full) present/absent by name on the agent",
		Long: `Probe the AGENT (not the client) for the declared build-toolchain set and
name every entry PRESENT or ABSENT against one CLASS.

A fresh agent was measured (SURF-009) to have python3 only: go, gcc, make,
node and cargo were ALL absent — which makes "the bunker does the building"
untrue for every Go/Rust/C/JS project a mount can put in front of an agent.
This command is the checkable form of that declaration: one command, every
name, an explicit verdict.

Classes (--class, default ` + provisioning.DefaultClass + `):
  minimal  python3 only — the stock image; the state a fresh agent is in
  go       python3 + go + make — a Go build box
  full     python3 + go + gcc + make + node + cargo — every measured-absent path

EXIT CODE is the gate: 0 when every toolchain the selected class requires is
PRESENT on the agent, 1 when any is missing (the missing names are printed and
carried in the JSON ` + "`missing`" + ` array). --report-only turns the same check into a
pure report (always exit 0), and a probe that could not RUN exits non-zero
regardless: a check that could not run is not a finding.

With --deliver the missing class toolchains are installed onto the agent —
without root, into the agent's own HOME — and the agent is re-probed so the
result is evidence rather than a claim:
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
no-op probes. python3 is the stock image's own interpreter: it is reported and
can be required, but it has no delivery stage by construction. Agents WITH the
image-spec surface can instead bake the apt half into the image at spawn: the
directive below is a valid --image-spec document.

Examples:
  bunker toolchain-check abc12345 --class go
  bunker toolchain-check abc12345 --class go --json
  bunker toolchain-check abc12345 --class go --report-only
  bunker toolchain-check abc12345 --deliver
  bunker toolchain-check abc12345 --deliver --class go --server staging`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			if !agentIDRe.MatchString(agentID) {
				return fmt.Errorf("invalid agent id %q: must match %s", agentID, agentIDRe.String())
			}
			// Resolve the class BEFORE any RPC: an unknown class is a local
			// usage error, and it must never silently degrade to a default
			// (which would turn "--class g" into a full-set check).
			selected, err := provisioning.LookupClass(class)
			if err != nil {
				return err
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
				if err := deliverToolchains(cmd.ErrOrStderr(), ctx, client, entry, agentID, selected); err != nil {
					return err
				}
			}

			report, err := probeToolchains(ctx, client, entry, agentID, selected)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return err
				}
			} else {
				writeToolchainReport(out, report)
			}
			return toolchainVerdict(report, reportOnly)
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the report as JSON")
	cmd.Flags().Uint32Var(&timeout, "timeout", 60, "Probe timeout in seconds (--deliver raises the floor to 30m)")
	cmd.Flags().StringVar(&class, "class", provisioning.DefaultClass,
		"Toolchain class to check: minimal (python3 only), go (go+make), full (go+gcc+make+node+cargo)")
	cmd.Flags().BoolVar(&deliver, "deliver", false,
		"Install the class's missing toolchains onto the agent's own HOME (no root), then re-probe")
	cmd.Flags().BoolVar(&reportOnly, "report-only", false,
		"Never fail on a missing toolchain: report and exit 0 (the default is a gate: exit 1 when the class is incomplete)")
	return cmd
}

// toolchainVerdict turns a report into the command's exit semantics: the
// gate (default) fails when the checked class is incomplete; --report-only
// returns nil, leaving the report as the answer. The report is already
// printed either way, so the error text repeats only the names a caller
// scraping stderr needs.
func toolchainVerdict(report provisioning.Report, reportOnly bool) error {
	if report.AllPresent || reportOnly {
		return nil
	}
	return fmt.Errorf("toolchain class %q incomplete on agent %q: missing %s",
		report.Class, report.Agent, strings.Join(report.Missing, ", "))
}

// probeToolchains runs the declared-set probe on the agent and parses it
// against the selected class.
func probeToolchains(ctx context.Context, client bunkerv1connect.BunkerdClient, entry ServerEntry, agentID string, class provisioning.Class) (provisioning.Report, error) {
	stdout, exitCode, err := execOnAgentScript(ctx, client, entry, agentID, provisioning.ProbeScript(), uint32(ctxDeadlineSeconds(ctx)))
	if err != nil {
		return provisioning.Report{}, fmt.Errorf("probe agent toolchains: %w", err)
	}
	if exitCode != 0 {
		// The probe ERRORING is a capability question, not a missing
		// tool: report the agent's own words rather than inventing one.
		return provisioning.Report{}, fmt.Errorf("toolchain probe exited %d on the agent", exitCode)
	}
	return provisioning.ParseProbeOutputFor(agentID, stdout, exitCode, class), nil
}

// deliverToolchains probes first, runs one delivery stage per missing
// class toolchain through the audited exec path, and re-probes at the end.
// The VERDICT is the re-probe: any class toolchain still absent fails the
// command with the stage's own words. Progress lines go to errOut so callers
// can keep stdout clean.
func deliverToolchains(errOut io.Writer, ctx context.Context, client bunkerv1connect.BunkerdClient, entry ServerEntry, agentID string, class provisioning.Class) error {
	before, err := probeToolchains(ctx, client, entry, agentID, class)
	if err != nil {
		return err
	}
	if before.AllPresent {
		_, _ = fmt.Fprintf(errOut, "bunker: every toolchain class %q requires is already present; nothing to deliver\n", class.Name)
		return nil
	}

	var stageFailures []string
	for _, f := range before.Tools {
		if !f.Required || f.Status == provisioning.StatusPresent {
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
	after, err := probeToolchains(ctx, client, entry, agentID, class)
	if err != nil {
		return err
	}
	if !after.AllPresent {
		return fmt.Errorf("toolchain delivery incomplete on agent %q (class %s): still absent after delivery: %s%s",
			agentID, class.Name, strings.Join(after.Missing, ", "), stageFailureNote(stageFailures))
	}
	if len(stageFailures) > 0 {
		// Every stage failed BUT the re-probe is clean: the tools were
		// delivered by another path mid-flight (or were already present
		// under names the probe resolves). Present is present — the
		// evidence is the probe — but the contradiction is named.
		_, _ = fmt.Fprintf(errOut, "bunker: note: %d stage(s) reported failure yet the re-probe shows the class present; stages: %s\n",
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

// writeToolchainReport renders the human form: one line per catalog entry,
// PRESENT or ABSENT by name, with the class's requirement marked so an
// absent-but-not-required tool is visibly not a failure.
func writeToolchainReport(out io.Writer, report provisioning.Report) {
	_, _ = fmt.Fprintf(out, "\nDeclared build toolchains (%s, class: %s — %s)\n\n",
		report.Agent, report.Class, report.ClassNote)
	_, _ = fmt.Fprintf(out, "  %-8s %-8s %-4s %s\n", "TOOL", "STATUS", "REQ", "VERSION / WHAT IT BUILDS")
	for _, f := range report.Tools {
		req := "no"
		if f.Required {
			req = "yes"
		}
		detail := f.Version
		if detail == "" {
			detail = f.Builds
		}
		_, _ = fmt.Fprintf(out, "  %-8s %-8s %-4s %s\n", f.Name, f.Status, req, detail)
	}
	_, _ = fmt.Fprintln(out)
	if report.AllPresent {
		_, _ = fmt.Fprintf(out, "  class %s: every required toolchain is PRESENT (%s) — this agent can build %s\n",
			report.Class, strings.Join(report.Required, ", "), classCapability(report.Class))
		return
	}
	_, _ = fmt.Fprintf(out, "  class %s: MISSING on the agent: %s\n", report.Class, strings.Join(report.Missing, ", "))
	_, _ = fmt.Fprintln(out, "  Deliver in place (no root):   bunker toolchain-check "+report.Agent+" --deliver --class "+report.Class)
	_, _ = fmt.Fprintln(out, "  Or bake into the image at spawn (--image-spec directive):")
	if directive, derr := provisioning.ImageSpecDirectiveFor(report.Class); derr == nil {
		_, _ = fmt.Fprintf(out, "    %s\n", directive)
	}
}

// classCapability names the project kinds a class covers, for the verdict
// line. Keep it in the class's own terms — it is read by the operator who is
// about to ship a build here.
func classCapability(class string) string {
	switch class {
	case provisioning.ClassMinimal:
		return "Python projects (no compiled toolchain is declared)"
	case provisioning.ClassGo:
		return "Go projects"
	case provisioning.ClassFull:
		return "Go, C/C++, Make, JS/TS and Rust projects"
	default:
		return "the projects its class declares"
	}
}
