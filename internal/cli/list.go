package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

const (
	// listDialTimeout bounds the TCP CONNECT phase on the list path only.
	// INT-HOST-012: an offline fleet member (tailscale reports it offline)
	// SILENTLY DROPS the TCP handshake, so the handshake burns whatever
	// deadline applies. Before this bound, `bunker list --server cube-las-00`
	// blocked 30.03/30.04/30.04 s on three consecutive measured calls. The
	// PERF-011 measurement (2026-09-29) puts a live daemon's connect at
	// ~0.35 s, so 3 s is ~8.5x the observed online connect latency while
	// leaving 2 s of margin under the "must fail in under 5 s" contract.
	listDialTimeout = 3 * time.Second

	// listRPCDeadline bounds the whole ListAgents exchange. It is
	// deliberately UNCHANGED from the pre-INT-HOST-012 30 s value: an
	// offline host is now caught by the CONNECT bound above, so a
	// reachable-but-slow server (a large agent list over a relayed link)
	// still gets its full request time. Cutting the whole call to 5 s would
	// have made slow-but-alive servers look offline.
	listRPCDeadline = 30 * time.Second
)

// listDialFunc is the dial function the list transport uses. It is a package
// var so a test can substitute a dialer that simulates a host which SILENTLY
// DROPS the handshake — the actual offline mode measured for INT-HOST-012 —
// and thereby prove the connect bound is wired, not just declared.
var listDialFunc = (&net.Dialer{Timeout: listDialTimeout}).DialContext

// newListBunkerdClient is newBunkerdClient for the list path only. It applies
// the same trust decision (resolveClientTLS — a refusal still surfaces through
// tlsRefusedTransport, never as a verification-free client) but swaps in a
// transport whose DialContext bounds the TCP connect phase at listDialTimeout,
// so a silent-drop offline host fails in seconds instead of burning the full
// request deadline on the handshake.
func newListBunkerdClient(entry ServerEntry) bunkerv1connect.BunkerdClient {
	httpClient := &http.Client{Timeout: listRPCDeadline}

	tlsCfg, err := resolveClientTLS(entry)
	if err != nil {
		httpClient.Transport = tlsRefusedTransport{url: entry.URL, err: err}
		return bunkerv1connect.NewBunkerdClient(httpClient, entry.URL)
	}
	// ProxyFromEnvironment keeps the behaviour the default transport had
	// before this transport took over: the ONLY change here is the dial bound.
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return listDialFunc(ctx, network, addr)
		},
	}
	if tlsCfg != nil {
		transport.TLSClientConfig = tlsCfg
	}
	httpClient.Transport = transport
	return bunkerv1connect.NewBunkerdClient(httpClient, entry.URL, clientOptions(entry)...)
}

// listCallError turns a failed ListAgents into the operator-facing error.
//
// A TLS/trust refusal is NOT a network outage and keeps its own
// classification. Every unreachable mode — a silently dropped handshake, a
// refused connection, no route to host, and a daemon that accepts TCP but
// never answers — collapses into ONE message that names the server, states the
// offline/unreachable state, and names the bound that produced the verdict
// (INT-HOST-012: the previous error was `list agents: …` with no server name).
func listCallError(serverName string, entry ServerEntry, ctx context.Context, err error) error {
	if _, ok := ClassifyTLSRefusal(err); ok {
		return fmt.Errorf("list agents: %w", err)
	}

	code := connect.CodeOf(err)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// The REQUEST deadline itself expired: the connect succeeded (or
		// was never the problem) and the daemon accepted the call without
		// ever answering.
		return fmt.Errorf("server %q is offline: no answer from %s within the %s request deadline%s (the connection was established, the request was never answered)",
			serverName, entry.URL, listRPCDeadline, tailnetNote(entry.URL))
	}
	if code == connect.CodeDeadlineExceeded || code == connect.CodeUnavailable {
		// The CONNECT bound expired (connect-go maps a net dial timeout to
		// DeadlineExceeded, a refused port to Unavailable) or the host is
		// unroutable. Both mean the host is not answering the handshake —
		// never that this client's request deadline was consumed.
		return fmt.Errorf("server %q is offline: cannot reach %s within the %s connect bound%s: %v (a reachable server still gets the full %s request deadline)",
			serverName, entry.URL, listDialTimeout, tailnetNote(entry.URL), err, listRPCDeadline)
	}
	return fmt.Errorf("list agents: %w", err)
}

// tailnetPeer is the best-effort tailnet observation of one server: whether
// the local tailscaled is asked at all, what it reports, and when the peer was
// last seen. It is OBSERVATION, not a verdict — the dial is what decides
// offline; this only names the state the operator can act on.
type tailnetPeer struct {
	// Online is what tailscaled reports for the peer.
	Online bool
	// LastSeen is when the peer was last reachable; zero when never seen.
	LastSeen time.Time
}

// listTailnetObserve resolves the tailnet state of a server URL's host. It is
// a package var so tests can state a deterministic peer instead of depending
// on the test host's tailnet.
var listTailnetObserve = observeTailnetPeer

// listTailnetProbeTimeout bounds the tailscaled query. The probe runs only on
// the offline path, so worst case stays 3s (connect) + 1s (probe) — still
// under the 5s contract; a healthy tailscaled answers in ~15ms.
const listTailnetProbeTimeout = 1 * time.Second

// observeTailnetPeer asks the local tailscaled for the peer behind url. It
// returns ok=false — and therefore adds NOTHING to the error — when the host
// is a loopback literal, when tailscale is not installed, when the query fails
// or times out, or when no peer matches. An unavailable observation must never
// become a claim.
func observeTailnetPeer(rawURL string) (tailnetPeer, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return tailnetPeer{}, false
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host == "" || host == "localhost" || (ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) {
		return tailnetPeer{}, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), listTailnetProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if err != nil {
		return tailnetPeer{}, false
	}

	var status struct {
		Peer map[string]struct {
			HostName     string   `json:"HostName"`
			DNSName      string   `json:"DNSName"`
			Online       bool     `json:"Online"`
			LastSeen     string   `json:"LastSeen"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Peer"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return tailnetPeer{}, false
	}

	for _, p := range status.Peer {
		if !peerMatches(p.HostName, p.DNSName, p.TailscaleIPs, host) {
			continue
		}
		peer := tailnetPeer{Online: p.Online}
		if ts, perr := time.Parse(time.RFC3339, p.LastSeen); perr == nil {
			peer.LastSeen = ts
		}
		return peer, true
	}
	return tailnetPeer{}, false
}

// peerMatches reports whether host (an IP or hostname) is the tailnet peer.
func peerMatches(hostName, dnsName string, ips []string, host string) bool {
	for _, ip := range ips {
		if ip == host {
			return true
		}
	}
	if strings.EqualFold(hostName, host) {
		return true
	}
	// DNSName is fully qualified with a trailing dot ("cube-las-00.tailnet.").
	return strings.EqualFold(strings.TrimSuffix(dnsName, "."), host) ||
		strings.EqualFold(strings.Split(strings.TrimSuffix(dnsName, "."), ".")[0], host)
}

// tailnetNote renders the peer observation for the offline error, or "" when
// there is nothing honest to say. An ONLINE peer is worth saying out loud: the
// host answers on the tailnet, so the failure is bunkerd/port, not the net.
func tailnetNote(rawURL string) string {
	peer, ok := listTailnetObserve(rawURL)
	if !ok {
		return ""
	}
	if peer.Online {
		return " (tailnet: peer online — the host answers on the tailnet, so check bunkerd is running and listening on this port)"
	}
	if peer.LastSeen.IsZero() {
		return " (tailnet: offline, never seen)"
	}
	return fmt.Sprintf(" (tailnet: offline, last seen %s)", humanizeSince(peer.LastSeen, time.Now()))
}

// humanizeSince renders an age the way `tailscale status` does ("2d ago").
func humanizeSince(t, now time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

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
			// Read-only convenience default is allowed, but the resolved
			// target is always printed (GAP-093): a read is never mistaken
			// for a read of a different server.
			serverName = ReadOnlyTarget(serverName, cfg.ActiveServer)
			if serverName == "" {
				return fmt.Errorf("no active server; run 'bunker connect' first")
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "bunker: reading server %q\n", serverName)

			entry, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("server %q not found in config", serverName)
			}
			// GAP-181: fail closed on a missing per-host token BEFORE the
			// dial — the refusal names the exact config path searched.
			if err := RequireTokenFor(entry, serverName); err != nil {
				return err
			}

			// 3. Build request
			// INT-HOST-012: the connect phase is bounded (listDialTimeout)
			// so an offline host cannot hold the command for the request
			// deadline; the request deadline itself is unchanged.
			client := newListBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), listRPCDeadline)
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
			token := resolveToken(entry)
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}

			// 4. Call RPC
			resp, err := client.ListAgents(ctx, req)
			if err != nil {
				// INT-HOST-012: name the server and the offline state
				// instead of a bare transport failure.
				return listCallError(serverName, entry, ctx, err)
			}

			// 5. Print results
			agents := resp.Msg.Agents
			if len(agents) == 0 {
				fmt.Println("No agents found.")
				return nil
			}

			// DF-BUNKER-54: the agent's disk figure is TWO facts, not a
			// ratio. The usage is measured; the cap is a PER-FILE size cap
			// (LimitFSIZE/RLIMIT_FSIZE) with no mechanism capping total
			// usage, so it must never be printed as a used-vs-limit
			// percentage (that was the "218% (43.6 GB/20.0 GB)" cell — real
			// usage compared against a number nothing enforces).
			fmt.Println()
			fmt.Println("══════════ Agents ══════════")
			fmt.Println()
			fmt.Printf("  %-14s %-10s %-14s %-14s %-25s %s\n", "Agent ID", "Status", diskUsedHeader, perFileCapHeader, "Created", "Public URL")
			fmt.Printf("  %-14s %-10s %-14s %-14s %-25s %s\n", "────────", "──────", "─────────", "─────────────", "───────", "──────────")
			for _, a := range agents {
				publicURL := a.PublicUrl
				if publicURL == "" {
					publicURL = "(no URL)"
				}
				diskCap := uint64(0)
				if a.GetLimits() != nil {
					diskCap = a.GetLimits().GetDiskMaxBytes()
				}
				fmt.Printf("  %-14s %-10s %-14s %-14s %-25s %s\n",
					a.AgentId, a.Status, humanBytes(a.DiskUsedBytes), formatPerFileCap(diskCap), a.CreatedAt, publicURL)
				// NET-BUNKER-010 §5.2: the mode + boundary per agent, directly
				// under its row — `bunker list` shows the boundary each agent
				// actually has (three-state vocabulary: empty = not reported).
				fmt.Printf("    └ net: %s\n", formatNetworkIsolation(a.GetNetworkIsolation()))
			}
			fmt.Println()
			fmt.Printf("Total: %d agents (server: %s)\n", resp.Msg.TotalCount, serverName)
			fmt.Printf("Disk Used is the agent's measured usage; %s %s\n", perFileCapHeader, perFileCapQualifier)

			// DF-BUNKER-34: surface the orphan-uid state the table itself cannot
			// carry. An agent whose user record is gone while its uid still owns
			// live processes is the state that made a destroyed agent look
			// healthy for 20+ hours; the list must name it, not bury it.
			for _, a := range agents {
				if orphan := a.GetOrphanUidDetail(); orphan != "" {
					fmt.Println()
					fmt.Printf("⚠  %s: %s\n", a.AgentId, orphan)
					fmt.Println("   Stop these processes on the host; a destroy that proceeds past them orphans them.")
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().StringVar(&status, "status", "running", "Filter by status: running, stopped, all")
	cmd.Flags().Uint32Var(&pageSize, "page-size", 50, "Page size")

	return cmd
}
