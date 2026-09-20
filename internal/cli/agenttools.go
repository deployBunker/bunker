package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// agentTool is one executable the REMOTE editing verbs need ON THE AGENT, not
// on the client. The distinction is the whole point: `bunker mount` and the
// toolsd verbs run their work in the agent's own uid and filesystem context, so
// a tool the client has is irrelevant to them.
type agentTool struct {
	Name     string
	NeededBy string
	// Required marks a tool whose absence breaks a documented verb outright.
	// An optional one downgrades a capability (lsp has nothing to serve) while
	// the verb itself still refuses by name.
	Required bool
	// VersionCommand is the argv used to read a version, most-preferred first.
	VersionCommand []string
}

// agentToolCatalog is the set the probe checks, in report order. It is the
// MEASURED set from the GAP-092 zero-code proof: git and jq were present on a
// fresh spawn, toolsd, rg and gopls were not.
var agentToolCatalog = []agentTool{
	{Name: "toolsd", NeededBy: "read, write, list, edit, patch, apply, lease, diff3, narrate", Required: true,
		VersionCommand: []string{"version"}},
	{Name: "rg", NeededBy: "search (content, files-only, count)", Required: true,
		VersionCommand: []string{"--version"}},
	{Name: "git", NeededBy: "session, lease (the lease registry lives in .git)", Required: true,
		VersionCommand: []string{"--version"}},
	{Name: "jq", NeededBy: "optional: scripted result handling", Required: false,
		VersionCommand: []string{"--version"}},
	{Name: "gopls", NeededBy: "lsp check for Go (a language server; one per language)", Required: false,
		VersionCommand: []string{"version"}},
}

// agentProbeScript is ONE remote shell program that reports every catalog entry.
// It runs through the same audited exec path as `bunker exec`, so the probe
// needs no new server surface and no new credential.
//
// The version read tries each argv in order and keeps the first non-empty line,
// because the tools disagree: git/jq/rg answer --version, gopls answers
// `version`, and toolsd answers its bare subcommand. A tool that exists but
// refuses to report a version is still PRESENT -- the report says so rather
// than pretending it is missing.
const agentProbeScript = `for t in __TOOLS__; do
  if command -v "$t" >/dev/null 2>&1; then
    v=""
    for flag in --version version; do
      v=$("$t" $flag 2>/dev/null | head -n 1)
      if [ -n "$v" ]; then break; fi
    done
    printf '%s\tpresent\t%s\n' "$t" "$v"
  else
    printf '%s\tabsent\t\n' "$t"
  fi
done
`

// agentToolFinding is one probe result.
type agentToolFinding struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Version  string `json:"version"`
	NeededBy string `json:"needed_by"`
	Required bool   `json:"required"`
}

// agentToolReport is the whole probe result, and the --json wire shape.
type agentToolReport struct {
	Agent   string             `json:"agent"`
	Tools   []agentToolFinding `json:"tools"`
	Missing []string           `json:"missing"`
	// MissingRequired is the subset whose absence breaks a documented verb.
	MissingRequired []string `json:"missing_required"`
	ExitCode        int32    `json:"exit_code"`
}

// NewAgentToolsCommand returns the `bunker agent-tools` command: a read-only
// probe of the executables the remote editing verbs need ON THE AGENT.
//
// It is diagnostic, so a finding is DATA, not a failure -- the same contract
// `bunker probe` states for an unreachable endpoint. A missing tool exits 0
// with a NAMED list; only a probe that could not RUN (unreachable agent, or the
// exec stream failing) exits non-zero. That keeps it safe to call from a verify
// step that wants the list rather than an abort.
func NewAgentToolsCommand() *cobra.Command {
	var serverName string
	var asJSON bool
	var timeout uint32

	cmd := &cobra.Command{
		Use:   "agent-tools AGENT_ID",
		Short: "Report which executables the remote editing verbs need, and which are missing on the agent",
		Long: `Probe the AGENT (not the client) for the executables the remote editing
verbs depend on, and name the ones that are absent.

The verbs execute in the agent's own uid and filesystem context, so a tool
present on the client is irrelevant to them -- a fresh agent was measured to
have git and jq but not toolsd, ripgrep or a language server, which is why
search has no working output mode and lsp refuses with nothing to serve.

A missing tool is DATA (exit 0 with a named list), matching bunker probe's
contract. Only a probe that could not run exits non-zero.

Examples:
  bunker agent-tools abc12345
  bunker agent-tools abc12345 --json
  bunker agent-tools abc12345 --server staging`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			// Read-only convenience default, and the resolved target is always
			// printed (GAP-093): a read is never mistaken for a read of a
			// different server.
			serverName = ReadOnlyTarget(serverName, cfg.ActiveServer)
			if serverName == "" {
				return fmt.Errorf("no active server; run 'bunker connect' first")
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "bunker: reading server %q\n", serverName)
			entry, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("server %q not found in config", serverName)
			}

			client := newBunkerdClient(entry)
			ctxTimeout := time.Duration(timeout) * time.Second
			if ctxTimeout <= 0 {
				ctxTimeout = 60 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
			defer cancel()

			report, err := probeAgentTools(ctx, client, entry, agentID)
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
				return nil
			}
			writeAgentToolReport(out, report)
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the report as JSON")
	cmd.Flags().Uint32Var(&timeout, "timeout", 60, "Probe timeout in seconds")
	return cmd
}

// probeAgentTools runs the probe on the agent and parses its output.
func probeAgentTools(ctx context.Context, client bunkerv1connect.BunkerdClient, entry ServerEntry, agentID string) (agentToolReport, error) {
	names := make([]string, 0, len(agentToolCatalog))
	for _, t := range agentToolCatalog {
		names = append(names, t.Name)
	}
	// strings.Replace, not Sprintf: the script body contains literal
	// printf format verbs, which Sprintf would try to consume.
	script := strings.Replace(agentProbeScript, "__TOOLS__", strings.Join(names, " "), 1)

	req := connect.NewRequest(&v1.ExecAgentRequest{
		AgentId:          agentID,
		Command:          "sh",
		Args:             []string{"-c", script},
		TimeoutSeconds:   uint32(ctxDeadlineSeconds(ctx)),
		ResponseEncoding: v1.ExecEncoding_EXEC_ENCODING_TEXT,
	})
	if token := resolveToken(entry); token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}

	stream, err := client.ExecAgent(ctx, req)
	if err != nil {
		return agentToolReport{}, fmt.Errorf("probe agent: %w", err)
	}
	var stdout, stderr strings.Builder
	var exitCode int32
	for stream.Receive() {
		msg := stream.Msg()
		if msg.GetStdout() != nil {
			stdout.Write(msg.GetStdout())
		}
		if msg.GetStderr() != nil {
			stderr.Write(msg.GetStderr())
		}
		if msg.ExitCode != 0 {
			exitCode = msg.ExitCode
		}
	}
	if err := stream.Err(); err != nil {
		return agentToolReport{}, fmt.Errorf("stream error: %w", err)
	}
	if exitCode != 0 {
		// The probe writing an ERROR is a capability question, not a missing
		// tool: report the agent's own words rather than inventing a finding.
		return agentToolReport{}, fmt.Errorf("probe exited %d on the agent: %s",
			exitCode, strings.TrimSpace(stderr.String()))
	}

	return parseAgentToolOutput(agentID, stdout.String(), exitCode), nil
}

// parseAgentToolOutput turns the probe's tab-separated lines into a report.
// A tool the catalog expects but the output omitted is reported as `unknown`
// rather than silently absent, so a truncated probe cannot masquerade as a
// clean bill of health.
func parseAgentToolOutput(agentID, output string, exitCode int32) agentToolReport {
	seen := map[string]string{}  // name -> version
	present := map[string]bool{} // name -> reported present
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			continue
		}
		name, status := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		version := ""
		if len(parts) == 3 {
			version = strings.TrimSpace(parts[2])
		}
		present[name] = status == "present"
		seen[name] = version
	}

	report := agentToolReport{Agent: agentID, ExitCode: exitCode, Tools: []agentToolFinding{}, Missing: []string{}, MissingRequired: []string{}}
	for _, t := range agentToolCatalog {
		status, version := "unknown", ""
		if isPresent, reported := present[t.Name]; reported {
			version = seen[t.Name]
			if isPresent {
				status = "present"
			} else {
				status = "absent"
			}
		}
		report.Tools = append(report.Tools, agentToolFinding{
			Name: t.Name, Status: status, Version: version,
			NeededBy: t.NeededBy, Required: t.Required,
		})
		if status == "absent" {
			report.Missing = append(report.Missing, t.Name)
			if t.Required {
				report.MissingRequired = append(report.MissingRequired, t.Name)
			}
		}
	}
	sort.Strings(report.Missing)
	sort.Strings(report.MissingRequired)
	return report
}

// writeAgentToolReport renders the human form.
func writeAgentToolReport(out io.Writer, report agentToolReport) {
	fmt.Fprintf(out, "\nAgent tool dependencies (%s)\n\n", report.Agent)
	fmt.Fprintf(out, "  %-8s %-8s %-10s %s\n", "TOOL", "STATUS", "REQUIRED", "VERSION / NEEDED BY")
	for _, f := range report.Tools {
		required := "no"
		if f.Required {
			required = "yes"
		}
		detail := f.Version
		if detail == "" {
			detail = "needed by " + f.NeededBy
		}
		fmt.Fprintf(out, "  %-8s %-8s %-10s %s\n", f.Name, f.Status, required, detail)
	}
	fmt.Fprintln(out)
	if len(report.Missing) == 0 {
		fmt.Fprintln(out, "  Every catalogued tool is present on the agent.")
		return
	}
	fmt.Fprintf(out, "  MISSING on the agent: %s\n", strings.Join(report.Missing, ", "))
	if len(report.MissingRequired) > 0 {
		fmt.Fprintf(out, "  Of those, REQUIRED (a verb is broken without them): %s\n",
			strings.Join(report.MissingRequired, ", "))
		fmt.Fprintln(out, "  Deliver them onto the agent's PATH (its own $HOME/bin is already on it),")
		fmt.Fprintln(out, "  or spawn with the image-spec package-add / host-provision extras path.")
	}
}

// ctxDeadlineSeconds reports the whole seconds left on ctx, floored at 1, so the
// server-side timeout never outlives the client's own deadline.
func ctxDeadlineSeconds(ctx context.Context) int {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 60
	}
	secs := int(time.Until(deadline).Seconds())
	if secs < 1 {
		return 1
	}
	return secs
}
