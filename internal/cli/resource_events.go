package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewResourceEventsCommand returns the `bunker resource-events` cobra command.
//
// GAP-121: `bunker metrics <id>` answers "how much is this agent using right
// now"; it cannot answer the question an operator actually asks after a
// runaway agent — "did it HIT its ceiling, and did the kernel kill it?".
// Those are recorded by the kernel in the agent's cgroup: `memory.events`
// (max / high / low / oom / oom_kill / oom_group_kill) and `pids.events`
// (max). This command surfaces them, plus the observed peaks, for one agent.
func NewResourceEventsCommand() *cobra.Command {
	var serverName string

	cmd := &cobra.Command{
		Use:   "resource-events <agent-id>",
		Short: "Show an agent's resource-ceiling events (memory/pids) and observed peaks",
		Long: `Show the resource-ceiling events and observed peaks for one agent.

Every count is read from the agent's own cgroup and is CUMULATIVE since the
agent's cgroup was created — the values are event counts, not rates:

  memory.events   max (memory.max reached), high, low, oom, oom_kill,
                  oom_group_kill
  pids.events     max (a fork was refused because pids.max was reached)

A non-zero "max" or "oom_kill" is a durable fact about the agent's life: it
hit its memory ceiling, or the kernel killed a process inside it to satisfy a
limit. Zero on an UNAVAILABLE reading means "nothing was measured" — the
command says so explicitly rather than reporting a clean bill of health.

Peaks come from the kernel (memory.peak, kernel >= 5.19; pids.peak,
kernel >= 6.1) when available, otherwise from the daemon's own sampler — the
source is printed next to each value, because a daemon-sampled peak is a
lower bound, not the true high-water mark.

Examples:
  bunker resource-events abc12345
  bunker resource-events abc12345 --server staging`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// Read-only convenience default, but the resolved target is
			// always printed (GAP-093) so a read is never mistaken for a
			// read of a different server.
			serverName = ReadOnlyTarget(serverName, cfg.ActiveServer)
			if serverName == "" {
				return fmt.Errorf("no active server; run 'bunker connect' first")
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "bunker: reading server %q\n", serverName)

			entry, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("server %q not found in config", serverName)
			}
			if err := RequireTokenFor(entry, serverName); err != nil {
				return err
			}

			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.AgentMetricsRequest{AgentId: args[0]})
			if token := resolveToken(entry); token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			resp, err := client.AgentMetrics(ctx, req)
			if err != nil {
				return fmt.Errorf("resource events: %w", err)
			}

			_, _ = fmt.Fprint(cmd.OutOrStdout(), formatResourceEvents(resp.Msg))
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")

	return cmd
}

// formatResourceEvents renders a computed resource-event view for one agent.
// It is pure so the rendering (including every "unavailable is not clean"
// branch) is unit-testable without a server.
func formatResourceEvents(msg *v1.AgentMetricsResponse) string {
	var b strings.Builder

	agentID := msg.GetAgentId()
	fmt.Fprintf(&b, "\n══════════ Agent Resource Events: %s ══════════\n\n", agentID)
	if s := msg.GetStatus(); s != "" {
		fmt.Fprintf(&b, "  Status:   %s\n", s)
	}

	ev := msg.GetResourceEvents()
	// A daemon that predates the surface sends no resource_events at all.
	// Say exactly that: absence of a report is never a clean report.
	if ev == nil {
		b.WriteString("  Resource events: NOT REPORTED by this daemon — it predates the resource-event\n")
		b.WriteString("                   surface (GAP-121). Ceiling hits (memory.max, oom_kill,\n")
		b.WriteString("                   pids.max) are not visible from here; build/run a daemon\n")
		b.WriteString("                   from the same commit as this CLI to read them.\n")
		return b.String()
	}

	if p := ev.GetCgroupPath(); p != "" {
		fmt.Fprintf(&b, "  Cgroup:   %s\n", p)
	}

	if !ev.GetAvailable() {
		b.WriteString("\n  ⚠  RESOURCE EVENTS UNAVAILABLE — the counters below were NOT measured.\n")
		b.WriteString("     An unavailable zero is an absence of measurement, never evidence that no\n")
		b.WriteString("     ceiling was hit. Typical cause: a stopped or destroyed agent, or a\n")
		b.WriteString("     deleted agent user (no cgroup left to read).\n")
		if d := ev.GetDetail(); d != "" {
			fmt.Fprintf(&b, "     Detail: %s\n", d)
		}
		b.WriteString("\n")
	}

	// ── memory ──
	b.WriteString("  MEMORY\n")
	if lim := msg.GetMemoryLimitBytes(); lim > 0 {
		fmt.Fprintf(&b, "    limit            %s\n", humanBytes(lim))
	}
	fmt.Fprintf(&b, "    current          %s\n", humanBytes(ev.GetMemoryCurrentBytes()))
	fmt.Fprintf(&b, "    peak             %s   [%s]\n",
		humanBytes(ev.GetMemoryPeakBytes()), peakSourceLabel(ev.GetMemoryPeakSource(), "memory.peak"))
	b.WriteString("\n    memory.events (cumulative since the cgroup was created)\n")
	if !ev.GetMemory().GetAvailable() {
		b.WriteString("      UNAVAILABLE — nothing was measured for this counter set\n")
	} else {
		m := ev.GetMemory()
		fmt.Fprintf(&b, "      %-15s %10d   ceiling reached; reclaim attempted\n", "max", m.GetMax())
		fmt.Fprintf(&b, "      %-15s %10d   usage exceeded memory.high (throttle threshold)\n", "high", m.GetHigh())
		fmt.Fprintf(&b, "      %-15s %10d   usage dropped below memory.low\n", "low", m.GetLow())
		fmt.Fprintf(&b, "      %-15s %10d   cgroup entered an OOM state\n", "oom", m.GetOom())
		fmt.Fprintf(&b, "      %-15s %10d   process killed to satisfy a memory limit\n", "oom_kill", m.GetOomKill())
		fmt.Fprintf(&b, "      %-15s %10d   whole cgroup killed (memory.oom.group)\n", "oom_group_kill", m.GetOomGroupKill())
	}

	// ── pids ──
	b.WriteString("\n  PIDS\n")
	fmt.Fprintf(&b, "    current          %d\n", ev.GetPidsCurrent())
	fmt.Fprintf(&b, "    peak             %d   [%s]\n",
		ev.GetPidsPeak(), peakSourceLabel(ev.GetPidsPeakSource(), "pids.peak"))
	b.WriteString("\n    pids.events (cumulative since the cgroup was created)\n")
	if !ev.GetPids().GetAvailable() {
		b.WriteString("      UNAVAILABLE — nothing was measured for this counter set\n")
	} else {
		fmt.Fprintf(&b, "      %-15s %10d   fork refused: pids.max already reached\n", "max", ev.GetPids().GetMax())
	}

	b.WriteString("\n  " + ceilingSummary(ev) + "\n")
	return b.String()
}

// peakSourceLabel renders where a peak value came from. The source is never
// omitted: a daemon-sampled peak is a lower bound, and presenting it as the
// kernel's high-water mark would be a claim the data does not support.
func peakSourceLabel(source, cgroupFile string) string {
	switch source {
	case "cgroup":
		return "cgroup " + cgroupFile
	case "daemon":
		return "daemon-observed, a LOWER BOUND: usage between samples is not seen"
	default:
		return "no peak available (kernel exposes no " + cgroupFile + " and the daemon has not sampled this agent)"
	}
}

// ceilingSummary is the one-line verdict an operator is looking for, and it is
// written so that an unmeasured agent can never read as a clean one.
func ceilingSummary(ev *v1.ResourceEvents) string {
	memoryMeasured := ev.GetMemory().GetAvailable()
	pidsMeasured := ev.GetPids().GetAvailable()
	if !memoryMeasured && !pidsMeasured {
		return "Ceiling events UNAVAILABLE — not measured (this is not a clean report)."
	}

	var hits []string
	m := ev.GetMemory()
	if memoryMeasured {
		if n := m.GetMax(); n > 0 {
			hits = append(hits, fmt.Sprintf("memory.max reached ×%d", n))
		}
		if n := m.GetOom(); n > 0 {
			hits = append(hits, fmt.Sprintf("OOM ×%d", n))
		}
		if n := m.GetOomKill(); n > 0 {
			hits = append(hits, fmt.Sprintf("OOM kill ×%d", n))
		}
		if n := m.GetOomGroupKill(); n > 0 {
			hits = append(hits, fmt.Sprintf("OOM group kill ×%d", n))
		}
	}
	if pidsMeasured {
		if n := ev.GetPids().GetMax(); n > 0 {
			hits = append(hits, fmt.Sprintf("pids.max reached ×%d", n))
		}
	}

	verdict := "No ceiling events recorded in this agent's cgroup."
	if len(hits) > 0 {
		verdict = "Ceiling events recorded: " + strings.Join(hits, ", ") + "."
		if m.GetOomKill() > 0 || m.GetOomGroupKill() > 0 {
			verdict += "\n  ⚠  The kernel killed process(es) inside this agent to satisfy a memory limit."
		}
	}
	// A partially-read cgroup must not let its measured half imply the whole
	// picture: name the counter set nobody looked at.
	if !memoryMeasured || !pidsMeasured {
		unread := "pids.events"
		if !memoryMeasured {
			unread = "memory.events"
			if !pidsMeasured {
				unread = "memory.events and pids.events"
			}
		}
		verdict += fmt.Sprintf("\n  ⚠  %s could NOT be read — those counters are unmeasured, not zero.", unread)
	}
	return verdict
}
