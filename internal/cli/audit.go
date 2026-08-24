package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/audit"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// defaultAuditLogPath is the daemon's configured audit log location
// (internal/config AuditConfig.Path); the audit subcommands default to it
// and --path overrides. verify/list/export default to local mode (reading
// this file); passing --server switches them to querying the daemon.
const defaultAuditLogPath = "/var/log/bunkerd/audit.log"

// NewAuditCommand returns the `bunker audit` command group for inspecting
// the bunkerd audit trail. GAP-049 added local log verification; GAP-050
// adds the query surface: list (filtered records), export (JSONL), and
// remote queries against a daemon via --server.
func NewAuditCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Inspect the bunkerd audit trail",
		Long: `Inspect the bunkerd audit trail: an append-only JSONL log of every
authenticated RPC (one record per request; file mode 0600; token values are
never written). Records are hash-chained (SHA-256) across the live file and
rotated backups (.1-.3).

Subcommands read the local log by default (--path) or query a remote
bunkerd daemon when --server is given.`,
	}
	cmd.AddCommand(newAuditVerifyCommand())
	cmd.AddCommand(newAuditListCommand())
	cmd.AddCommand(newAuditExportCommand())
	return cmd
}

// auditQueryFlags holds the filter flags shared by list and export.
type auditQueryFlags struct {
	serverName string
	agentID    string
	method     string
	since      string
	until      string
	limit      uint32
	path       string
}

// addAuditQueryFlags registers the shared filter flags on cmd.
func addAuditQueryFlags(cmd *cobra.Command, f *auditQueryFlags) {
	cmd.Flags().StringVar(&f.serverName, "server", "", "Server alias to query remotely (default: read the local log at --path)")
	cmd.Flags().StringVar(&f.agentID, "agent", "", "Only records for this agent_id (exact match)")
	cmd.Flags().StringVar(&f.method, "method", "", "Only records whose procedure contains this substring, e.g. SpawnAgent")
	cmd.Flags().StringVar(&f.since, "since", "", "Only records at or after this RFC3339 timestamp (inclusive)")
	cmd.Flags().StringVar(&f.until, "until", "", "Only records at or before this RFC3339 timestamp (inclusive)")
	cmd.Flags().Uint32Var(&f.limit, "limit", 0, "Max records to return (most recent first); 0 = no limit")
	cmd.Flags().StringVar(&f.path, "path", defaultAuditLogPath, "Local audit log file to read (ignored when --server is set)")
}

// parseAuditFilter converts the shared flags into an audit.Filter,
// validating the RFC3339 timestamps. Records with unparseable ts are
// excluded when a time filter is set (matching the query helper).
func parseAuditFilter(f *auditQueryFlags) (audit.Filter, error) {
	filter := audit.Filter{
		AgentID: f.agentID,
		Method:  f.method,
		Limit:   int(f.limit),
	}
	if f.since != "" {
		ts, err := time.Parse(time.RFC3339, f.since)
		if err != nil {
			return filter, fmt.Errorf("invalid --since %q (want RFC3339, e.g. 2026-08-20T12:00:00Z): %w", f.since, err)
		}
		filter.Since = &ts
	}
	if f.until != "" {
		ts, err := time.Parse(time.RFC3339, f.until)
		if err != nil {
			return filter, fmt.Errorf("invalid --until %q (want RFC3339, e.g. 2026-08-20T12:00:00Z): %w", f.until, err)
		}
		filter.Until = &ts
	}
	return filter, nil
}

// queryAuditRecords resolves the query source: --server queries the daemon
// via the QueryAudit RPC; otherwise the local log at --path is read. The
// returned records are always audit.Record values (converted from the
// wire form for remote queries), so list and export share one rendering
// path and local/remote output is byte-identical.
func queryAuditRecords(cmd *cobra.Command, f *auditQueryFlags, filter audit.Filter) ([]audit.Record, error) {
	if f.serverName != "" {
		return queryRemoteAudit(cmd, f.serverName, filter)
	}
	records, err := audit.Query(f.path, filter)
	if err != nil {
		return nil, fmt.Errorf("query audit log %s: %w", f.path, err)
	}
	return records, nil
}

// queryRemoteAudit calls the bunkerd QueryAudit RPC on the given server
// alias, mirroring the remote pattern of `bunker list` (resolveServer →
// newBunkerdClient → resolveToken → Authorization header).
func queryRemoteAudit(cmd *cobra.Command, serverName string, filter audit.Filter) ([]audit.Record, error) {
	cfg, err := LoadCLIConfig()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	entry, ok := cfg.Servers[serverName]
	if !ok {
		return nil, fmt.Errorf("server %q not found in config", serverName)
	}

	client := newBunkerdClient(entry)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := connect.NewRequest(&v1.QueryAuditRequest{
		AgentId: filter.AgentID,
		Method:  filter.Method,
		Limit:   uint32(filter.Limit),
	})
	if filter.Since != nil {
		req.Msg.Since = filter.Since.UTC().Format(time.RFC3339)
	}
	if filter.Until != nil {
		req.Msg.Until = filter.Until.UTC().Format(time.RFC3339)
	}

	token := resolveToken(entry)
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}

	resp, err := client.QueryAudit(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("query audit on %s: %w", serverName, err)
	}

	records := make([]audit.Record, 0, len(resp.Msg.Records))
	for _, r := range resp.Msg.Records {
		records = append(records, audit.Record{
			TS:         r.Ts,
			Caller:     r.Caller,
			Method:     r.Method,
			RemoteAddr: r.RemoteAddr,
			AgentID:    r.AgentId,
			DurationMS: r.DurationMs,
			Outcome:    r.Outcome,
			Summary:    r.Summary,
			Hash:       r.Hash,
			PrevHash:   r.PrevHash,
		})
	}
	return records, nil
}

// newAuditVerifyCommand returns `bunker audit verify`, which checks the
// audit log's hash chain and exits non-zero with the first bad record index
// when any record fails verification (own hash mismatch, broken chain link,
// or unparseable line). Rotated backups (.1-.3) are verified as part of the
// chain. Local-log only: verification of a remote trail is performed by the
// daemon's operator on the host.
func newAuditVerifyCommand() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify the audit log hash chain",
		Long: `Verify checks every record of the audit log, including rotated backups
(.1-.3), against its SHA-256 hash chain: each record's hash must match its
line bytes and chain to the previous record. Any tampering is reported with
the first bad record index and exits non-zero; an untouched log prints
"OK (N records)" and exits 0. Local log only — the daemon host's operator
runs this on the host.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			records, firstBad, err := audit.Verify(path)
			if err != nil {
				if firstBad > 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "audit log %s: tamper detected at record %d\n", path, firstBad)
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "audit log %s: OK (%d records)\n", path, records)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", defaultAuditLogPath, "audit log file to verify (rotated backups .1-.3 are checked as part of the chain)")
	return cmd
}

// newAuditListCommand returns `bunker audit list`, which prints matching
// audit records as a readable table. Without --server it reads the local
// log at --path; with --server it queries the remote daemon.
func newAuditListCommand() *cobra.Command {
	f := &auditQueryFlags{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List audit trail records",
		Long: `List audit trail records matching the given filters (all filters are
ANDed; empty filters match everything). Records are returned oldest first.

Without --server the local log at --path is read (rotated backups .1-.3 are
included); with --server the remote daemon's trail is queried and --path is
ignored. Records' hash and prev_hash are not shown in the table but are
included in ` + "`bunker audit export`" + `.

Examples:
  bunker audit list --agent abc123
  bunker audit list --method SpawnAgent --since 2026-08-20T00:00:00Z
  bunker audit list --server prod --agent abc123 --limit 20`,
		RunE: func(cmd *cobra.Command, args []string) error {
			filter, err := parseAuditFilter(f)
			if err != nil {
				return err
			}
			records, err := queryAuditRecords(cmd, f, filter)
			if err != nil {
				return err
			}
			if len(records) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No audit records match.")
				return nil
			}
			printAuditTable(cmd.OutOrStdout(), records)
			return nil
		},
	}
	addAuditQueryFlags(cmd, f)
	return cmd
}

// newAuditExportCommand returns `bunker audit export`, which writes
// matching audit records as JSONL (one JSON object per line) to stdout —
// the same JSON keys the daemon writes, including hash and prev_hash, so
// the export is lossless and can be re-verified or replayed. Format choice:
// JSONL keeps export byte-compatible with the on-disk log format.
func newAuditExportCommand() *cobra.Command {
	f := &auditQueryFlags{}
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export audit trail records as JSONL",
		Long: `Export matching audit trail records to stdout as JSONL — one JSON object
per line, using the same keys as the daemon's log (ts, caller, method,
remote_addr, agent_id, duration_ms, outcome, summary, hash, prev_hash).
Export is lossless: hash and prev_hash are preserved, so the output can be
re-verified or replayed. Records are written oldest first.

Without --server the local log at --path is read (rotated backups .1-.3 are
included); with --server the remote daemon's trail is queried and --path is
ignored.

Examples:
  bunker audit export > audit.jsonl
  bunker audit export --agent abc123 | jq -r .method | sort | uniq -c
  bunker audit export --server prod --since 2026-08-20T00:00:00Z`,
		RunE: func(cmd *cobra.Command, args []string) error {
			filter, err := parseAuditFilter(f)
			if err != nil {
				return err
			}
			records, err := queryAuditRecords(cmd, f, filter)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetEscapeHTML(false)
			for _, r := range records {
				if err := enc.Encode(r); err != nil {
					return fmt.Errorf("encode audit record: %w", err)
				}
			}
			return nil
		},
	}
	addAuditQueryFlags(cmd, f)
	return cmd
}

// printAuditTable renders records as a fixed-width table on w.
func printAuditTable(w io.Writer, records []audit.Record) {
	fmt.Fprintf(w, "  %-30s %-22s %-34s %-10s %-14s %s\n", "TS", "Caller", "Method", "Agent", "Outcome", "Summary")
	fmt.Fprintf(w, "  %-30s %-22s %-34s %-10s %-14s %s\n", "──────────────────────────────", "──────────────────────", "──────────────────────────────────", "──────────", "──────────────", "───────")
	for _, r := range records {
		method := r.Method
		if idx := lastSlash(method); idx >= 0 {
			method = method[idx+1:]
		}
		fmt.Fprintf(w, "  %-30s %-22s %-34s %-10s %-14s %s\n", r.TS, r.Caller, method, r.AgentID, r.Outcome, r.Summary)
	}
	fmt.Fprintf(w, "Total: %d records\n", len(records))
}

// lastSlash returns the index of the last '/' in s, or -1.
func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}
