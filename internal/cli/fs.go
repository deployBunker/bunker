package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/fsclient"
	"github.com/deployBunker/bunker/internal/fsmount"
	"github.com/deployBunker/bunker/internal/mountdriver"
)

// NewFSCommand builds the `bunker fs` command group: the bunker-fs client's own
// surfaces.
//
// The group exists because of the correction BFS-003 §3(d) makes to the PRD's
// phrasing: the delegated whole-tree operations (`status`, `diff`, `rev-parse`,
// `ls-files`, `log`) CANNOT be reached by making git's syscalls smarter — a FUSE
// filesystem is handed one syscall at a time and cannot tell the caller "I
// answered your walk in one round trip". What IS reachable is a native node tree
// populated from ONE snapshot call (`bunker fs mount`, and `bunker fs snapshot`
// in isolation), and the delegated ops keep their OWN surface: these verbs and
// the `X-Bunker-Op` header. `bunker fs op` is that surface.
func NewFSCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fs",
		Short: "bunker-fs: mount the WebDAV surface as a filesystem (opt-in; sshfs stays the default)",
		Long: `bunker-fs is the opt-in FUSE client for the bunkerd WebDAV surface.

Two surfaces, deliberately:

  * THE MOUNT (bunker fs mount) — the filesystem. Its win is the SNAPSHOT: one
    X-Bunker-Op: snapshot call populates the node tree, and READDIRPLUS then
    resolves every entry in-process, so ` + "`ls -l`" + `, ` + "`stat`" + ` and a walking tool's lstat
    cost zero further round trips. git's own content reads for CHANGED files are
    N reads by construction and no binding changes that.

  * THE VERBS (bunker fs op, bunker fs snapshot) — the delegated surface. These
    call the agent directly over X-Bunker-Op, with no mount in the picture, which
    is the only place a whole-tree operation can be computed remotely instead of
    walked.

It is NOT the default and does not become one until a battery on both DCs shows
zero stalls.`,
	}
	cmd.AddCommand(
		newFSMountCommand(),
		newFSUmountCommand(),
		newFSStatusCommand(),
		newFSConflictsCommand(),
		newFSSnapshotCommand(),
		newFSOpCommand(),
		newFSProbeCommand(),
	)
	return cmd
}

type fsCommonFlags struct {
	url      string
	user     string
	password string
}

func (f *fsCommonFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.url, "url", "", "WebDAV surface root, e.g. http://127.0.0.1:18481/dav (required)")
	cmd.Flags().StringVar(&f.user, "user", "", "HTTP Basic username, when the surface requires one")
	cmd.Flags().StringVar(&f.password, "password", "", "HTTP Basic password (prefer --password-file or the environment in scripts)")
}

func (f *fsCommonFlags) client(timeout time.Duration) (*fsclient.Client, error) {
	if strings.TrimSpace(f.url) == "" {
		return nil, fmt.Errorf("--url is required (the WebDAV surface root, e.g. http://host:18481/dav)")
	}
	return fsclient.NewClient(fsclient.Options{
		BaseURL:     f.url,
		Username:    f.user,
		Password:    f.password,
		OpTimeout:   timeout,
		BindTimeout: fsclient.DefaultBindTimeout,
		UserAgent:   "bunker-fs/1 (CLI)",
	})
}

func newFSMountCommand() *cobra.Command {
	var o fsmount.Options
	var url string
	var allowOther, noCache, noSnapshot, verbose bool
	var writeBufferMax int64
	var poolShare string
	var backoffBaseMS, backoffMaxMS int
	hot := fsclient.DefaultHotPolicy()

	cmd := &cobra.Command{
		Use:   "mount <mountpoint>",
		Short: "Mount the WebDAV surface as a filesystem (Linux; Windows is BFS-010)",
		Long: `Mount the bunkerd WebDAV surface at <mountpoint> using the bunker-fs FUSE client.

The mountpoint is created private (0700) and an existing wider mode is tightened;
--allow-other is STRIPPED (accepted and ignored), never honoured or refused.

The cache is bounded and content-addressed: 256 MiB by default, evicted
least-recently-HIT first at insert, and BYPASSED (read straight through, nothing
cached) when nothing can be evicted — a read never fails for a local-capacity
reason. The kernel keeps no file data (FOPEN_DIRECT_IO + ExplicitDataCacheControl),
so the cache figure ` + "`bunker fs status`" + ` reports is the only local copy.

The mountpoint stays mounted when the transport dies: every operation fails loudly
within the 30 s deadline with a named cause, and recovery is one command.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Mountpoint = args[0]
			o.BaseURL = url
			if noCache {
				o.CacheMaxBytes = 0
			}
			o.Snapshot = !noSnapshot
			o.AllowOther = allowOther
			// The hot-file policy (BFS-044): the flags were bound onto a copy of
			// the spec's defaults, so what lands here is the operator's policy
			// with every unset knob already at the value the spec pins. The two
			// flags whose NAME declares a unit (ms) and the one whose value is a
			// fraction are converted here, and a value that cannot be converted
			// is an error rather than a default.
			o.Hot = hot
			if err := hotFlagsToPolicy(&o.Hot, poolShare, backoffBaseMS, backoffMaxMS); err != nil {
				return err
			}
			if writeBufferMax > 0 {
				// the write buffer bound is a package constant today; the flag
				// exists so the number is discoverable, and an override beyond the
				// default is refused rather than silently ignored.
				if writeBufferMax > fsmount.DefaultWriteBufferMax {
					return fmt.Errorf("--write-buffer-max-bytes cannot exceed the default %d", int64(fsmount.DefaultWriteBufferMax))
				}
			}
			if verbose {
				o.Logf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
			}
			if o.AllowOther {
				fmt.Fprintln(os.Stderr, fsmount.AllowedOtherStripped())
			}
			if err := o.Normalize(); err != nil {
				return err
			}
			m, err := fsmount.MountAt(o)
			if err != nil {
				// A refused mount leaves NOTHING at the mountpoint: the
				// "silent empty tree" failure mode the durability spec records.
				return err
			}
			fmt.Printf("mounted %s on %s\n", o.BaseURL, m.Mountpoint())
			fmt.Printf("  cache dir    : %s (bound %d bytes, %d entries)\n", m.CacheDir(), o.CacheMaxBytes, o.CacheMaxEntries)
			fmt.Printf("  concurrency  : %d request(s) in flight max\n", o.Concurrency)
			fmt.Printf("  invalidation : %s\n", m.Status().Invalidation.Mode)
			st := m.Status()
			fmt.Printf("  snapshot     : %s, %d nodes in %d call(s)\n", st.Snapshot.Source, st.Snapshot.Nodes, st.Snapshot.Calls)
			// The resolved bounds, at the moment of mount. An operator should not
			// have to re-read the command line to know what the mount obeys, and
			// `bunker fs status` prints the same block afterwards.
			printEffectiveConfig(os.Stdout, st.Config)
			fmt.Printf("  unmount      : bunker fs umount %s\n", m.Mountpoint())

			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				<-sig
				fmt.Fprintln(os.Stderr, "\nunmounting…")
				_ = m.Unmount()
				os.Exit(0)
			}()
			m.Wait()
			return m.Unmount()
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "WebDAV surface root, e.g. http://127.0.0.1:18481/dav (required)")
	cmd.Flags().StringVar(&o.Username, "user", "", "HTTP Basic username")
	cmd.Flags().StringVar(&o.Password, "password", "", "HTTP Basic password")
	cmd.Flags().Int64Var(&o.CacheMaxBytes, "cache-max-size", fsmount.DefaultCacheMaxBytes, "hard cap on cache bytes on this client (0 disables the cache)")
	cmd.Flags().Int64Var(&o.CacheMaxEntryBytes, "cache-max-entry-bytes", fsmount.DefaultCacheMaxEntryBytes, "a single file larger than this is never cached")
	cmd.Flags().IntVar(&o.CacheMaxEntries, "cache-max-entries", fsmount.DefaultCacheMaxEntries, "the ENTRY bound of the cache directory (a byte bound alone does not bound a directory: BFS-031)")
	cmd.Flags().IntVar(&o.CacheMaxInFlight, "cache-max-inflight", fsmount.DefaultCacheMaxInFlight, "how many staged (unpublished) blobs may hold bytes at once")
	cmd.Flags().DurationVar(&o.CacheMaxAge, "cache-max-age", fsmount.DefaultCacheMaxAge, "backstop TTL for a cache entry")
	cmd.Flags().IntVar(&o.Concurrency, "concurrency", fsmount.DefaultConcurrency, "maximum requests in flight (the measured lever: 25x concurrency beat MaxConnsPerHost=1 by 25x)")
	cmd.Flags().IntVar(&o.MaxConnsPerHost, "max-conns-per-host", 0, "transport connection cap (0 follows --concurrency; 1 reproduces the serial arm)")
	cmd.Flags().StringVar(&o.Invalidation, "invalidation", "auto", "auto|push|poll (auto prefers the pushed channel and DECLARES a downgrade)")
	cmd.Flags().DurationVar(&o.PollInterval, "poll-interval", fsmount.DefaultPollInterval, "declared poll period")
	cmd.Flags().DurationVar(&o.InvalidateIdleTimeout, "invalidate-idle-timeout", fsmount.DefaultInvalidateIdleTimeout, "how long the pushed channel may be silent before the mount falls back to the poll (0 derives it from the heartbeat period the server declares)")
	cmd.Flags().StringVar(&o.OnConflict, "on-conflict", fsclient.OnConflictRefuse, "refuse|overwrite-if-unchanged")
	cmd.Flags().StringVar(&o.CacheDir, "cache-dir", "", "override the cache directory (default $XDG_CACHE_HOME/bunker/fs/<mount-id>)")
	cmd.Flags().BoolVar(&allowOther, "allow-other", false, "requested and STRIPPED: the mountpoint stays private 0700")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "disable the cache (same as --cache-max-size 0)")
	cmd.Flags().BoolVar(&noSnapshot, "no-snapshot", false, "disable the one-call node-tree snapshot (every directory read falls back to PROPFIND)")
	cmd.Flags().Int64Var(&writeBufferMax, "write-buffer-max-bytes", fsmount.DefaultWriteBufferMax, "bound on one open write handle's local buffer")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "log the client's decisions to stderr")

	// -----------------------------------------------------------------------
	// THE HOT-FILE POLICY (BFS-044). Every default here is the number
	// SPEC-hot-file-policy.md pins, read from the ONE place the defaults exist
	// (fsclient.DefaultHotPolicy), so a flag's help text, its pflag default and
	// the value the client obeys cannot drift apart. Every value is validated
	// in Normalize, and an invalid one is refused naming the flag, the value
	// given and the value the spec pins — nothing falls back silently.
	//
	// The feature itself defaults OFF. It is a PERFORMANCE-ONLY subsystem
	// (P-0), its value is unmeasured, and an existing mount script or any other
	// WebDAV client must not notice this row landing.
	// -----------------------------------------------------------------------
	cmd.Flags().BoolVar(&hot.Enabled, "hot.enabled", fsclient.DefaultHotEnabled, "arm the hot-file refresh (performance-only; correctness never depends on it)")
	cmd.Flags().Float64Var(&hot.WeightRead, "hot.read-weight", fsclient.DefaultHotWeightRead, "popularity weight of a READ touch (H-1)")
	cmd.Flags().Float64Var(&hot.WeightEdit, "hot.edit-weight", fsclient.DefaultHotWeightEdit, "popularity weight of an EDIT touch; must exceed the read weight (H-2)")
	cmd.Flags().Float64Var(&hot.Decay, "hot.decay", fsclient.DefaultHotDecay, "(0,1]: the decay factor applied per step; 1.0 means no decay (H-3)")
	cmd.Flags().DurationVar(&hot.DecayStep, "hot.decay-step", fsclient.DefaultHotDecayStep, "the interval one --hot.decay step covers (H-3)")
	cmd.Flags().Float64Var(&hot.ScoreCeiling, "hot.score-ceiling", fsclient.DefaultHotScoreCeiling, "score above which every score is halved in one pass (H-6)")
	cmd.Flags().DurationVar(&hot.ReadTouchWindow, "hot.read-touch-window", fsclient.DefaultHotReadTouchWindow, "one read touch per path per window, so chunked reads do not inflate a score (H-9)")
	cmd.Flags().DurationVar(&hot.FlushInterval, "hot.flush-interval", fsclient.DefaultHotFlushInterval, "how often a dirty tracker is persisted (H-21)")
	cmd.Flags().IntVar(&hot.TrackerMaxEntries, "hot.max-entries", fsclient.DefaultHotTrackerMaxEntries, "the tracker's ENTRY bound (H-7)")
	cmd.Flags().Int64Var(&hot.TrackerMaxBytes, "hot.max-tracker-bytes", fsclient.DefaultHotTrackerMaxBytes, "the tracker's BYTE bound; both are enforced (H-8)")
	cmd.Flags().Int64Var(&hot.MaxFileBytes, "hot.max-file-bytes", fsclient.DefaultHotMaxFileBytes, "the size rule: a file at or below this is refreshable, above it is never pulled (S-1, inclusive per S-2)")
	cmd.Flags().IntVar(&hot.QueueMaxDepth, "hot.queue-depth", fsclient.DefaultHotQueueMaxDepth, "refresh queue depth; when full the LOWEST-SCORING item is displaced, never the oldest (Q-1/Q-3)")
	cmd.Flags().DurationVar(&hot.QueueMaxWait, "hot.queue-max-wait", fsclient.DefaultHotQueueMaxWait, "a queued item older than this is dropped (Q-6)")
	cmd.Flags().IntVar(&hot.RefreshMaxInflight, "hot.max-concurrent-refresh", fsclient.DefaultHotRefreshMaxInflight, "maximum concurrent refreshes; clamped to the pool share and reported (Q-7/P-4)")
	cmd.Flags().StringVar(&poolShare, "hot.pool-share", fsclient.DefaultHotPoolShare(), "the share of the client's pool a refresh may hold, as a fraction (P-3)")
	cmd.Flags().IntVar(&backoffBaseMS, "hot.backoff-base-ms", int(fsclient.DefaultHotBackoffBase.Milliseconds()), "first retry delay in milliseconds (P-10)")
	cmd.Flags().IntVar(&backoffMaxMS, "hot.backoff-max-ms", int(fsclient.DefaultHotBackoffMax.Milliseconds()), "retry delay cap in milliseconds; may not exceed --op-timeout's 30s (P-10)")
	cmd.Flags().Float64Var(&hot.BackoffFactor, "hot.backoff-factor", fsclient.DefaultHotBackoffFactor, "backoff growth factor (P-10)")
	cmd.Flags().StringVar(&hot.BackoffJitter, "hot.backoff-jitter", fsclient.DefaultHotJitter(), "full|none: full jitter by default, so N mounts cannot return together (P-10)")
	cmd.Flags().DurationVar(&hot.RefreshDeadline, "hot.refresh-deadline", fsclient.DefaultHotRefreshDeadline, "an upper bound on one refresh's life; it is abandoned at this deadline (P-11)")
	cmd.Flags().DurationVar(&hot.RefreshReacquireWindow, "hot.reacquire-window", fsclient.DefaultHotRefreshReacquireWindow, "how long a yielded refresh may wait for its slot back (P-12)")
	cmd.Flags().DurationVar(&hot.YieldAfter, "hot.yield-after", fsclient.DefaultHotYieldAfter, "a refresh holding a slot releases it if a foreground request has waited this long (P-7)")
	cmd.Flags().DurationVar(&hot.StopDeadline, "hot.stop-deadline", fsclient.DefaultHotStopDeadline, "the time within which a stop must be fully in force (Q-12)")
	cmd.Flags().DurationVar(&hot.TickInterval, "hot.tick-interval", fsclient.DefaultHotTickInterval, "the manager's tick: yield check, expiry sweep and re-arm check (Q-14)")
	cmd.Flags().IntVar(&hot.PoolPressureTicks, "hot.pool-pressure-ticks", fsclient.DefaultHotPoolPressureTicks, "consecutive ticks of foreground slot starvation before the hot path stops itself (P-19)")
	return cmd
}

// hotFlagsToPolicy folds the three BFS-044 flags whose SPELLING is not the
// policy's field type into the policy: the two whose name declares its unit
// (--hot.backoff-base-ms / --hot.backoff-max-ms, integers of milliseconds) and
// the one whose value is a fraction (--hot.pool-share, "1/8").
//
// It is a named function rather than three lines inside RunE so that the
// conversion — which is the only place a flag's spelling could disagree with the
// value the client obeys — is directly testable. A value it cannot convert is
// returned as an error, never replaced by a default.
func hotFlagsToPolicy(hot *fsclient.HotPolicy, poolShare string, backoffBaseMS, backoffMaxMS int) error {
	hot.BackoffBase = time.Duration(backoffBaseMS) * time.Millisecond
	hot.BackoffMax = time.Duration(backoffMaxMS) * time.Millisecond
	num, den, err := fsclient.ParsePoolShare(poolShare)
	if err != nil {
		return err
	}
	hot.PoolShareNum, hot.PoolShareDen = num, den
	return nil
}

func newFSUmountCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "umount <mountpoint>",
		Short: "Unmount a bunker-fs mountpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mp := args[0]
			if out, err := execFusermountUnmount(cmd.Context(), mp); err != nil {
				return fmt.Errorf("unmount %s: %v: %s", mp, err, strings.TrimSpace(out))
			}
			fmt.Printf("unmounted %s\n", mp)
			return nil
		},
	}
	return cmd
}

func newFSStatusCommand() *cobra.Command {
	var mountID string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report one mount's cache, invalidation mode, conflicts and transport verdict",
		Long: `Read the mount's status document.

It is a FILE the mount keeps fresh (in the mount's cache directory), not an RPC:
no new server-side requirement, no daemon, and it works from a different terminal
than the one holding the mount.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := resolveMountDir(mountID)
			if err != nil {
				return err
			}
			st, err := fsclient.ReadStatus(dir)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			printStatus(os.Stdout, st)
			return nil
		},
	}
	cmd.Flags().StringVar(&mountID, "mount", "", "mount id (default: the most recent mount in $XDG_CACHE_HOME/bunker/fs)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the status document as JSON")
	return cmd
}

func newFSConflictsCommand() *cobra.Command {
	var mountID string
	var asJSON bool
	var limit int
	cmd := &cobra.Command{
		Use:   "conflicts",
		Short: "List refused writes (the conflict log)",
		Long: `List the refusal log: which write was refused, the base hash it carried, the
server's own machine code, and the current hash when the server named one.

POSIX close(2) ignores errors, which is exactly why the refusal is written here
rather than relying on the syscall alone.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := resolveMountDir(mountID)
			if err != nil {
				return err
			}
			list, err := fsclient.ReadConflicts(dir, limit)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(list)
			}
			if len(list) == 0 {
				fmt.Println("no refusals recorded")
				return nil
			}
			for _, c := range list {
				cur := c.Current
				if cur == "" {
					cur = "(not named: the target was absent)"
				}
				fmt.Printf("%s  %s\n    code=%s expected=%s current=%s\n",
					c.TS.Format(time.RFC3339), c.Path, c.Code, dashIfEmpty(c.Expected), cur)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&mountID, "mount", "", "mount id (default: the most recent mount)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the refusals as JSON")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum refusals to show (0 = all)")
	return cmd
}

func newFSSnapshotCommand() *cobra.Command {
	var f fsCommonFlags
	var root, source string
	var includeHash, asJSON bool
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Populate the node tree in ONE call (X-Bunker-Op: snapshot), with its cost",
		Long: `Take the subtree metadata snapshot.

This is the one place a whole-tree read is ONE server call. It prints the call
count and the source, because the difference between one call
(source=snapshot-op) and one call per directory (source=propfind-walk) is the
entire measurable claim.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.client(fsclient.DefaultOpTimeout)
			if err != nil {
				return err
			}
			if !includeHash {
				includeHash = true
			}
			start := time.Now()
			snap, oerr := c.SnapshotTree(cmd.Context(), root, includeHash)
			if oerr != nil {
				return oerr
			}
			elapsed := time.Since(start)
			if asJSON {
				out := map[string]any{
					"root":           snap.Root(),
					"source":         snap.Source(),
					"nodes":          snap.Count(),
					"calls":          snap.Calls(),
					"truncated":      snap.Truncated(),
					"elapsed_ms":     elapsed.Milliseconds(),
					"requests_total": c.Requests(),
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			src := snap.Source()
			if source != "" {
				src = source
			}
			fmt.Printf("snapshot %s: %d nodes in %d server call(s), source=%s, %s\n",
				quoteIfEmpty(snap.Root(), "/"), snap.Count(), snap.Calls(), src, elapsed.Round(time.Millisecond))
			_, high := c.InFlight()
			fmt.Printf("  requests total (this client) : %d\n", c.Requests())
			fmt.Printf("  in-flight high-water mark    : %d\n", high)
			if snap.Truncated() {
				fmt.Printf("  TRUNCATED by the server's result cap (reported, never silently shortened)\n")
			}
			return nil
		},
	}
	f.bind(cmd)
	cmd.Flags().StringVar(&root, "path", "", "subtree root inside the served tree (default: the whole tree)")
	cmd.Flags().BoolVar(&includeHash, "include-hash", true, "ask the server for content hashes (only files <= 1 MiB)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the snapshot summary as JSON")
	return cmd
}

func newFSOpCommand() *cobra.Command {
	var f fsCommonFlags
	var path, ref string
	var statOnly, short, cached, nameOnly, noRenames bool
	var limit int
	var argsList []string
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "op <op>",
		Short: "Run one delegated whole-tree operation on the agent (X-Bunker-Op) — the second surface",
		Long: `Run one delegated operation.

This is the SECOND SURFACE, and it does not go through the mount: the op is
carried in the X-Bunker-Op request header with its arguments flat in the body, and
its answer is computed on the agent. BFS-003 §3(d) is why it must exist —
` + "`git status`" + ` INSIDE a mount cannot be made one round trip, because git is not a
client of our API and issues lstat/open/read one syscall at a time.

Only the ops the surface publishes are accepted here; a name outside the E-4
catalogue is refused locally rather than sent. An op the surface publishes but this
build does not serve answers a structured 501 capability_unavailable naming the
slice — report that verbatim, do not dress it up.

Catalogue: ` + strings.Join(fsclient.Catalogue, ", "),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			op := args[0]
			c, err := f.client(fsclient.DefaultOpTimeout)
			if err != nil {
				return err
			}
			body := map[string]any{}
			if path != "" {
				body["path"] = path
			}
			if ref != "" {
				body["ref"] = ref
			}
			if cmd.Flags().Changed("stat-only") {
				body["stat_only"] = statOnly
			}
			if cmd.Flags().Changed("short") {
				body["short"] = short
			}
			if cmd.Flags().Changed("cached") {
				body["cached"] = cached
			}
			if cmd.Flags().Changed("name-only") {
				body["name_only"] = nameOnly
			}
			if cmd.Flags().Changed("no-renames") {
				body["no_renames"] = noRenames
			}
			if limit > 0 {
				body["limit"] = limit
			}
			for _, kv := range argsList {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("--arg wants key=value (got %q)", kv)
				}
				body[k] = inferScalar(v)
			}
			res := c.Delegate(cmd.Context(), op, body)
			if res.Err != nil {
				// The refusal is the answer: print the server's own code and
				// fields verbatim, then exit non-zero.
				printDelegatedRefusal(os.Stderr, res)
				return fmt.Errorf("op %s refused: %s", op, res.Err.Error())
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(res.Envelope)
			}
			fmt.Fprintf(os.Stderr, "# op=%s verdict=%s status=%d round_trip=%s (server compute %d ms)\n",
				op, res.Verdict, res.Status, res.Duration.Round(time.Millisecond), res.Envelope.DurationMS)
			if res.Text != "" {
				fmt.Println(res.Text)
			} else if len(res.Envelope.Result) > 0 {
				fmt.Println(string(res.Envelope.Result))
			}
			return nil
		},
	}
	f.bind(cmd)
	cmd.Flags().StringVar(&path, "path", "", "path argument inside the served tree")
	cmd.Flags().StringVar(&ref, "ref", "", "bounded ref argument (e.g. HEAD)")
	cmd.Flags().BoolVar(&statOnly, "stat-only", false, "diff: --stat")
	cmd.Flags().BoolVar(&short, "short", false, "status: --short")
	cmd.Flags().BoolVar(&cached, "cached", false, "diff: --cached")
	cmd.Flags().BoolVar(&nameOnly, "name-only", false, "diff: --name-only")
	cmd.Flags().BoolVar(&noRenames, "no-renames", false, "diff: --no-renames")
	cmd.Flags().IntVar(&limit, "limit", 0, "log: bounded line count")
	cmd.Flags().StringArrayVar(&argsList, "arg", nil, "extra flat argument as key=value (typed as JSON scalar)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the whole envelope as JSON")
	return cmd
}

func newFSProbeCommand() *cobra.Command {
	var f fsCommonFlags
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "probe",
		Short: "Run the bind preflight and print what the surface answered (the mount's own check)",
		Long: `Run the mount's bind preflight in isolation: OPTIONS, the capability document,
the watcher's availability, and the tree identity — under the 5 s deadline a real
mount uses. It is the honest way to check an endpoint before mounting it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.client(fsclient.DefaultBindTimeout)
			if err != nil {
				return err
			}
			start := time.Now()
			info, oerr := c.Handshake(context.Background())
			if oerr != nil {
				return oerr
			}
			elapsed := time.Since(start)
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"tree":              info.Tree,
					"rev":               info.Rev,
					"proto":             info.Proto,
					"allow":             info.Allow,
					"dav":               info.DAV,
					"extensions":        info.Extensions,
					"document_version":  info.DocVersion,
					"watcher_available": info.WatcherAvailable,
					"poll_op_available": info.PollOpAvailable,
					"elapsed_ms":        elapsed.Milliseconds(),
				})
			}
			fmt.Printf("endpoint        : %s\n", f.url)
			fmt.Printf("proto           : %s (HTTP/1.1 is supported; nothing here requires h2/h3)\n", dashIfEmpty(info.Proto))
			fmt.Printf("dav             : %s\n", dashIfEmpty(info.DAV))
			fmt.Printf("extensions      : %s\n", dashIfEmpty(info.Extensions))
			fmt.Printf("methods         : %s\n", strings.Join(info.Allow, ", "))
			fmt.Printf("tree            : %s\n", dashIfEmpty(info.Tree))
			fmt.Printf("rev             : %s\n", dashIfEmpty(info.Rev))
			fmt.Printf("document version: %d\n", info.DocVersion)
			fmt.Printf("watcher         : %v\n", info.WatcherAvailable)
			fmt.Printf("poll form       : %v\n", info.PollOpAvailable)
			if info.Degradation != nil {
				fmt.Printf("degradation     : %s (scope=%s phase=%s mode=%s) %s\n",
					info.Degradation.Capability, dashIfEmpty(info.Degradation.Scope),
					dashIfEmpty(info.Degradation.Phase), dashIfEmpty(info.Degradation.Mode), info.Degradation.Detail)
			}
			fmt.Printf("bind preflight  : OK in %s\n", elapsed.Round(time.Millisecond))
			return nil
		},
	}
	f.bind(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the probe as JSON")
	return cmd
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// execFusermountUnmount unmounts a FUSE mountpoint through fusermount, which is
// how a Linux user unmounts without root. It is a seam (a function variable
// would be equivalent) so the CLI's own tests never need a real FUSE mount.
var execFusermountUnmount = func(ctx context.Context, mountpoint string) (string, error) {
	cmd := exec.CommandContext(ctx, "fusermount", "-u", mountpoint)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// resolveMountDir resolves which mount's cache directory to read.

func resolveMountDir(mountID string) (string, error) {
	if mountID != "" {
		root, err := fsclient.MountRoot()
		if err != nil {
			return "", err
		}
		return filepath.Join(root, mountID), nil
	}
	dirs, err := fsclient.FindMounts()
	if err != nil {
		return "", err
	}
	if len(dirs) == 0 {
		return "", fmt.Errorf("no bunker-fs mount found under %s — mount one first", mustMountRoot())
	}
	return dirs[0], nil
}

func mustMountRoot() string {
	root, err := fsclient.MountRoot()
	if err != nil {
		return "$XDG_CACHE_HOME/bunker/fs"
	}
	return root
}

// printEffectiveConfig prints the mount's option set AS RESOLVED (BFS-044).
//
// It is printed by `bunker fs mount` at the moment of mount and by
// `bunker fs status` afterwards, from the same block of the same document, so
// what an operator reads is what the mount obeys — not a second copy of the
// numbers written out for display. The two derived figures are printed beside
// the configured ones on purpose: P-4's clamp and the reservation ceiling the
// size rule implies are the numbers a reader would otherwise have to compute,
// and a bound nobody can see is not a bound (PRD §2.7).
func printEffectiveConfig(w io.Writer, c fsclient.EffectiveConfig) {
	// A status document written before this block existed carries no config at
	// all, and every field would read zero. Printing a wall of zeros would be an
	// unexplained null: an operator cannot tell "nothing configured" from "this
	// mount did not report it". Normalize guarantees a live mount always has
	// non-zero bounds, so an all-zero block means exactly one thing and it is
	// said out loud.
	if !effectiveConfigReported(c) {
		fmt.Fprintln(w, "  config       : not reported by this mount (its status document predates the effective-config block)")
		return
	}
	fmt.Fprintf(w, "  config       : cache %d B / %d entries (entry cap %d, staged %d, age %s)\n",
		c.CacheMaxBytes, c.CacheMaxEntries, c.CacheMaxEntryBytes, c.CacheMaxInFlight, msDur(c.CacheMaxAgeMS))
	idle := "derived from the declared heartbeat"
	if c.InvalidationIdleTimeoutMS > 0 {
		idle = msDur(c.InvalidationIdleTimeoutMS).String()
	}
	fmt.Fprintf(w, "  invalidation : mode=%s poll_interval=%s idle_timeout=%s\n",
		c.Invalidation, msDur(c.PollIntervalMS), idle)
	fmt.Fprintf(w, "  hot policy   : enabled=%v state=%s share=%s slots=%d (foreground %d) refresh_inflight=%d clamped=%v reserve=%d B\n",
		c.Hot.Enabled, c.Hot.ConfigState, c.Hot.PoolShare,
		c.Hot.Derived.PoolSlots, c.Hot.Derived.PoolSlotsForeground,
		c.Hot.Derived.RefreshMaxInflight, c.Hot.Derived.RefreshMaxInflightClamped,
		c.Hot.Derived.MaxInflightBytes)
	// A configuration that cannot be honoured is REPORTED with both numbers,
	// never swallowed: an operator must be able to tell an inert knob from a
	// silent one (S-11). An ARMED policy with the same incoherence never reaches
	// here — Normalize refuses the mount instead.
	for _, problem := range c.Hot.Misconfigured {
		fmt.Fprintf(w, "  MISCONFIG   : %s\n", problem)
	}
	p := c.Hot.Configured
	half := "no decay"
	if c.Hot.Derived.HalfLifeMS > 0 {
		half = msDur(c.Hot.Derived.HalfLifeMS).Round(time.Second).String()
	}
	fmt.Fprintf(w, "  hot weights  : read=%v edit=%v decay=%v per %s (half-life %s)\n",
		p.WeightRead, p.WeightEdit, p.Decay, p.DecayStep, half)
	fmt.Fprintf(w, "  hot bounds   : tracker %d entries / %d B, size<=%d B (inclusive=%v), queue %d (%s, wait %s), ceiling %v\n",
		p.TrackerMaxEntries, p.TrackerMaxBytes, p.MaxFileBytes, c.Hot.SizeRuleInclusive,
		p.QueueMaxDepth, c.Hot.QueueReplacement, p.QueueMaxWait, p.ScoreCeiling)
	fmt.Fprintf(w, "  hot timing   : tick=%s yield=%s stop_deadline=%s refresh_deadline=%s reacquire=%s touch_window=%s flush=%s\n",
		p.TickInterval, p.YieldAfter, p.StopDeadline, p.RefreshDeadline, p.RefreshReacquireWindow, p.ReadTouchWindow, p.FlushInterval)
	fmt.Fprintf(w, "  hot backoff  : %s x%v cap %s jitter=%s, pressure_ticks=%d\n",
		p.BackoffBase, p.BackoffFactor, p.BackoffMax, p.BackoffJitter, p.PoolPressureTicks)
}

// msDur renders a millisecond count as a duration, so a reported figure and a
// flag's value are the same spelling.
func msDur(ms int64) time.Duration { return time.Duration(ms) * time.Millisecond }

// effectiveConfigReported reports whether a status document carried the
// effective-config block at all. It is deliberately a whole-block test rather
// than a per-field one: a live mount's Normalize guarantees a non-zero byte
// bound, so "every field is zero" cannot be a running mount.
func effectiveConfigReported(c fsclient.EffectiveConfig) bool {
	return c.CacheMaxBytes != 0 || c.CacheMaxEntries != 0 || c.CacheMaxEntryBytes != 0 ||
		c.CacheMaxInFlight != 0 || c.CacheMaxAgeMS != 0 || c.Concurrency != 0 ||
		c.Invalidation != "" || c.PollIntervalMS != 0 || c.InvalidationIdleTimeoutMS != 0 ||
		c.OnConflict != "" || c.Snapshot || c.Hot.Enabled || c.Hot.ConfigState != "" ||
		c.Hot.Configured != (fsclient.HotPolicy{})
}

func printStatus(w io.Writer, st *fsclient.Status) {
	fmt.Fprintf(w, "mount        : %s\n", st.Mount)
	fmt.Fprintf(w, "endpoint     : %s\n", st.Endpoint)
	fmt.Fprintf(w, "mountpoint   : %s\n", st.Mountpoint)
	fmt.Fprintf(w, "mode         : %s (mechanism=%s)\n", st.Mode, dashIfEmpty(st.Invalidation.Mechanism))
	inv := st.Invalidation
	fmt.Fprintf(w, "invalidation : mode=%s seq=%d", inv.Mode, inv.Seq)
	if inv.PollIntervalMS != nil {
		fmt.Fprintf(w, " poll_interval_ms=%d", *inv.PollIntervalMS)
	}
	if inv.LastEventAgeMS != nil {
		fmt.Fprintf(w, " last_event_age_ms=%d", *inv.LastEventAgeMS)
	}
	fmt.Fprintln(w)
	// The resume declaration (BFS-063), for the one mechanism whose coverage
	// rests on it: whether this mount holds an observation of the tree at all,
	// and the cursor it presents. A mount that holds none is that moment's honest
	// state — the server is answering the interval it cannot vouch for instead of
	// a tail that would claim coverage — and a person reading the status must be
	// able to see it, not only a JSON consumer.
	if inv.Mechanism == fsclient.MechanismEvents {
		if inv.ResumeSeq != nil {
			fmt.Fprintf(w, "resume       : seq=%d (the cursor this view was minted at)\n", *inv.ResumeSeq)
		} else {
			fmt.Fprintln(w, "resume       : none — this mount holds no observation yet, so the server answers the interval it cannot vouch for")
		}
	}
	// On the revision tier, say what the revision can and cannot vouch for: a
	// git-tree mount whose last-resort poll is HEAD-only must not read as
	// "everything is current" (BFS-048; the gap is a reported fact, not a
	// footnote).
	if inv.RevKind != "" {
		fmt.Fprintf(w, "rev coverage : kind=%s vouches_for=%s gap=%s\n",
			inv.RevKind, dashIfEmpty(inv.RevVouchesFor), dashIfEmpty(inv.RevGap))
	}
	// The CONTENT-AGE BOUND (BFS-045). Absent WITH ITS REASON when there is no
	// evidence to age: a blank here would read as "fine", and this is the figure
	// that says how stale this mount may be.
	if inv.ContentAge != nil {
		ca := inv.ContentAge
		fmt.Fprintf(w, "content age  : age_ms=%d evidence_from=%s observations=%d", ca.AgeMS, dashIfEmpty(ca.EvidenceFrom), ca.Observations)
		if ca.BoundMS != nil {
			fmt.Fprintf(w, " bound_ms=%d (%s) within_bound=%v", *ca.BoundMS, dashIfEmpty(ca.BoundSource), *ca.WithinBound)
		} else {
			fmt.Fprintf(w, " bound_ms=- within_bound=-(%s)", dashIfEmpty(ca.BoundReason))
		}
		fmt.Fprintln(w)
	} else {
		fmt.Fprintf(w, "content age  : - (no figure to report)\n  why        : %s\n", dashIfEmpty(inv.ContentAgeReason))
	}
	// The server's own watcher state, and — when it is not here — which of the
	// three facts that is (BFS-045's null rule).
	if inv.Server != nil {
		sv := inv.Server
		fmt.Fprintf(w, "server watch : state=%s backend=%s watched=%d/%d", dashIfEmpty(sv.State), dashIfEmpty(sv.Backend), sv.DirectoriesWatched, sv.DirectoriesDesired)
		if sv.Reason != "" {
			fmt.Fprintf(w, " reason=%s", sv.Reason)
		}
		if sv.SampledAgeMS != nil {
			fmt.Fprintf(w, " sampled_age_ms=%d", *sv.SampledAgeMS)
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "server counts: overflows=%d unvouched=%d rescans=%d install_failures=%d backend_errors=%d heartbeats=%d loop_ticks=%d",
			sv.OverflowsTotal, sv.UnvouchedTotal, sv.RescansTotal, sv.InstallFailuresTotal, sv.BackendErrorsTotal, sv.HeartbeatsTotal, sv.EventLoopTicks)
		if sv.DroppedEvents != nil {
			fmt.Fprintf(w, " dropped_events=%d", *sv.DroppedEvents)
		} else if sv.DroppedEventsReason != "" {
			fmt.Fprintf(w, " dropped_events=- (%s)", sv.DroppedEventsReason)
		}
		fmt.Fprintln(w)
		if sv.UnvouchedReason != "" {
			fmt.Fprintf(w, "  unvouched  : %s\n", sv.UnvouchedReason)
		}
		if sv.CountersReason != "" {
			fmt.Fprintf(w, "  counts why : %s\n", sv.CountersReason)
		}
	} else {
		fmt.Fprintf(w, "server watch : - (nothing to report)\n  why        : %s\n", dashIfEmpty(inv.ServerReason))
	}
	// The heartbeat/stall state (BFS-045): a poll mount has no heartbeat, and
	// saying so is part of the record.
	if inv.Liveness != nil {
		lv := inv.Liveness
		fmt.Fprintf(w, "liveness     : heartbeats=%d declared_heartbeat_ms=%d idle_timeout_ms=%d stalled=%v stalls=%d",
			lv.HeartbeatsTotal, lv.DeclaredHeartbeatMS, lv.IdleTimeoutMS, lv.Stalled, lv.StallsTotal)
		if lv.LastLineAgeMS != nil {
			fmt.Fprintf(w, " last_line_age_ms=%d", *lv.LastLineAgeMS)
		}
		fmt.Fprintln(w)
	} else {
		fmt.Fprintf(w, "liveness     : - (no pushed channel on this mount)\n  why        : %s\n", dashIfEmpty(inv.LivenessReason))
	}
	fmt.Fprintf(w, "requests     : requests_total=%d failures_total=%d\n", inv.Requests, inv.Failures)
	if inv.LastFailure != "" {
		fmt.Fprintf(w, "  last       : %s\n", inv.LastFailure)
	}
	// The refresh accounting: the staged window this build HAS, and the hot
	// queue it does NOT — named as absent rather than shown as zero (BFS-045).
	rf := inv.Refresh
	fmt.Fprintf(w, "refresh      : started=%d in_flight=%d/%d committed=%d aborted=%d refused_no_slot=%d refused_no_room=%d\n",
		rf.StartedTotal, rf.InFlight, rf.MaxInFlight, rf.CommittedTotal, rf.AbortedTotal, rf.RefusedNoSlotTotal, rf.RefusedNoRoomTotal)
	if rf.AbsentReason != "" {
		fmt.Fprintf(w, "refresh queue: - (no figure to report)\n  why        : %s\n", rf.AbsentReason)
	}
	fmt.Fprintf(w, "cache        : used_bytes=%d max_bytes=%d (blobs=%d index=%d) entries=%d blobs=%d\n",
		st.Cache.UsedBytes, st.Cache.MaxBytes, st.Cache.BlobsBytes, st.Cache.IndexBytes, st.Cache.Entries, st.Cache.Blobs)
	// The two accounts, separately (BFS-038's F-1 split), and the entry bound —
	// a bound the owner cannot see is not a bound.
	fmt.Fprintf(w, "cache bounds : max_entry_bytes=%d entries=%d/%d in_flight_bytes=%d reserved_bytes=%d staged_blobs=%d/%d\n",
		st.Cache.MaxEntryBytes, st.Cache.Entries, st.Cache.MaxEntries, st.Cache.InFlightBytes,
		st.Cache.ReservedBytes, st.Cache.StagedBlobs, st.Cache.MaxInFlight)
	// The independent measurement: what is REALLY in the directory, and the delta
	// the published figure does not count (BFS-031's shape, made visible).
	if st.Cache.DirBytesReason == "" {
		fmt.Fprintf(w, "cache dir    : dir_bytes=%d unaccounted_bytes=%d", st.Cache.DirBytes, st.Cache.DirUnaccountedBytes)
		if st.Cache.DirMeasuredAgeMS != nil {
			fmt.Fprintf(w, " measured_age_ms=%d", *st.Cache.DirMeasuredAgeMS)
		}
		fmt.Fprintf(w, " by_class=%s\n", classSummary(st.Cache.DirBytesByClass))
	} else {
		fmt.Fprintf(w, "cache dir    : - (no measurement to report)\n  why        : %s\n", st.Cache.DirBytesReason)
	}
	fmt.Fprintf(w, "cache events : hits=%d misses=%d evictions=%d bypasses=%d oversize_bypasses=%d pinned=%d\n",
		st.Cache.Hits, st.Cache.Misses, st.Cache.EvictionsTotal, st.Cache.BypassEvents, st.Cache.OversizeBypasses, st.Cache.PinnedBlobs)
	// Every reason in the closed vocabulary, so a reason that never fires reads
	// as 0 and a reason the code cannot reach is visibly missing from the census
	// rather than silently zero (BFS-032).
	fmt.Fprintf(w, "cache bypass : %s\n", bypassSummary(st.Cache.BypassReasons))
	fmt.Fprintf(w, "conflicts    : refusals_total=%d\n", st.Conflicts.RefusalsTotal)
	if st.Conflicts.Last != nil {
		fmt.Fprintf(w, "  last       : %s code=%s\n", st.Conflicts.Last.Path, st.Conflicts.Last.Code)
	}
	fmt.Fprintf(w, "snapshot     : source=%s nodes=%d calls=%d truncated=%v\n",
		st.Snapshot.Source, st.Snapshot.Nodes, st.Snapshot.Calls, st.Snapshot.Truncated)
	fmt.Fprintf(w, "transport    : verdict=%s in_flight=%d in_flight_max=%d requests=%d proto=%s\n",
		st.Transport.Verdict, st.Transport.InFlight, st.Transport.InFlightMax, st.Transport.Requests, dashIfEmpty(st.Transport.Proto))
	if st.Transport.Cause != "" {
		fmt.Fprintf(w, "  cause      : %s\n", st.Transport.Cause)
	}
	fmt.Fprintf(w, "write buffer : handles=%d bytes=%d\n", st.WriteHandlesBuffered, st.WriteBufferBytes)
	fmt.Fprintf(w, "read bound   : refusals_total=%d corrections_total=%d\n", st.ReadBound.RefusalsTotal, st.ReadBound.CorrectionsTotal)
	if st.ReadBound.Last != "" {
		fmt.Fprintf(w, "  last       : %s\n", st.ReadBound.Last)
	}
	fmt.Fprintf(w, "write shape  : refusals_total=%d\n", st.WriteShape.RefusalsTotal)
	if st.WriteShape.Last != "" {
		fmt.Fprintf(w, "  last       : %s\n", st.WriteShape.Last)
	}
	fmt.Fprintf(w, "refusal holds: held_total=%d outstanding=%d evicted_total=%d\n",
		st.RefusalHolds.HeldTotal, st.RefusalHolds.Outstanding, st.RefusalHolds.EvictedTotal)
	if st.RefusalHolds.Last != "" {
		fmt.Fprintf(w, "  last       : %s\n", st.RefusalHolds.Last)
	}
	// The resolved option set (BFS-044). Printed last, and printed in full,
	// because it is the block that answers "what is this mount actually obeying
	// — including the entry bound, the invalidation cadence and every hot-file
	// knob" without re-reading the command line that started it.
	printEffectiveConfig(w, st.Config)
}

func printDelegatedRefusal(w io.Writer, res *fsclient.DelegatedResult) {
	e := res.Err
	fmt.Fprintf(w, "OP REFUSED: %s status=%d verdict=%s errno=%s cause=%s\n",
		res.Op, e.Status, dashIfEmpty(e.Verdict), fsclient.ErrnoName(e.Errno), e.Cause)
	if e.Capability != "" {
		fmt.Fprintf(w, "  capability : %s (scope=%s phase=%s mode=%s)\n", e.Capability, dashIfEmpty(e.Scope), dashIfEmpty(e.Phase), dashIfEmpty(e.Mode))
	}
	if e.Detail != "" {
		fmt.Fprintf(w, "  detail     : %s\n", e.Detail)
	}
	if e.CurrentHash != "" || e.ExpectedHash != "" {
		fmt.Fprintf(w, "  hashes     : expected=%s current=%s\n", dashIfEmpty(e.ExpectedHash), dashIfEmpty(e.CurrentHash))
	}
	fmt.Fprintf(w, "  (the delegated surface needs the slice the refusal names; the mount is unaffected)\n")
}

// inferScalar types an --arg value as the JSON scalar it looks like, so
// `--arg limit=10` sends a number rather than a string.
func inferScalar(v string) any {
	switch strings.ToLower(v) {
	case "true":
		return true
	case "false":
		return false
	}
	var n int64
	if _, err := fmt.Sscanf(v, "%d", &n); err == nil && fmt.Sprintf("%d", n) == v {
		return n
	}
	return v
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// classSummary renders the directory measurement's classes in a stable order, so
// two runs of `bunker fs status` on the same state print the same line (a map's
// iteration order would make the output un-diffable, and an evidence transcript
// is only useful if it can be diffed).
func classSummary(classes map[string]int64) string {
	if len(classes) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(classes))
	for k := range classes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, classes[k]))
	}
	return strings.Join(parts, " ")
}

// bypassSummary renders the cache's refusal census. Every reason in the closed
// vocabulary is printed, including the ones at zero: a reason that has never
// fired is a fact, and a reason that is MISSING from this line is a counter the
// code cannot reach (BFS-032).
func bypassSummary(reasons map[string]int64) string {
	names := fsclient.BypassReasons()
	parts := make([]string, 0, len(names))
	for _, r := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", r, reasons[r]))
	}
	return strings.Join(parts, " ")
}

func quoteIfEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// sortedKeys is used by tests and by the status printer's stable ordering.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DriverName is the mountdriver identity this client mounts under, so a caller
// asking the server for a driver by name uses the registered string.
const DriverName = mountdriver.DriverBunkerFS
