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
			fmt.Printf("  cache dir    : %s (bound %d bytes)\n", m.CacheDir(), o.CacheMaxBytes)
			fmt.Printf("  concurrency  : %d request(s) in flight max\n", o.Concurrency)
			fmt.Printf("  invalidation : %s\n", m.Status().Invalidation.Mode)
			st := m.Status()
			fmt.Printf("  snapshot     : %s, %d nodes in %d call(s)\n", st.Snapshot.Source, st.Snapshot.Nodes, st.Snapshot.Calls)
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
	cmd.Flags().DurationVar(&o.CacheMaxAge, "cache-max-age", fsmount.DefaultCacheMaxAge, "backstop TTL for a cache entry")
	cmd.Flags().IntVar(&o.Concurrency, "concurrency", fsmount.DefaultConcurrency, "maximum requests in flight (the measured lever: 25x concurrency beat MaxConnsPerHost=1 by 25x)")
	cmd.Flags().IntVar(&o.MaxConnsPerHost, "max-conns-per-host", 0, "transport connection cap (0 follows --concurrency; 1 reproduces the serial arm)")
	cmd.Flags().StringVar(&o.Invalidation, "invalidation", "auto", "auto|push|poll (auto prefers the pushed channel and DECLARES a downgrade)")
	cmd.Flags().DurationVar(&o.PollInterval, "poll-interval", fsmount.DefaultPollInterval, "declared poll period")
	cmd.Flags().StringVar(&o.OnConflict, "on-conflict", fsclient.OnConflictRefuse, "refuse|overwrite-if-unchanged")
	cmd.Flags().StringVar(&o.CacheDir, "cache-dir", "", "override the cache directory (default $XDG_CACHE_HOME/bunker/fs/<mount-id>)")
	cmd.Flags().BoolVar(&allowOther, "allow-other", false, "requested and STRIPPED: the mountpoint stays private 0700")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "disable the cache (same as --cache-max-size 0)")
	cmd.Flags().BoolVar(&noSnapshot, "no-snapshot", false, "disable the one-call node-tree snapshot (every directory read falls back to PROPFIND)")
	cmd.Flags().Int64Var(&writeBufferMax, "write-buffer-max-bytes", fsmount.DefaultWriteBufferMax, "bound on one open write handle's local buffer")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "log the client's decisions to stderr")
	return cmd
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
	fmt.Fprintf(w, "cache        : used_bytes=%d max_bytes=%d (blobs=%d index=%d) entries=%d blobs=%d\n",
		st.Cache.UsedBytes, st.Cache.MaxBytes, st.Cache.BlobsBytes, st.Cache.IndexBytes, st.Cache.Entries, st.Cache.Blobs)
	fmt.Fprintf(w, "cache events : hits=%d misses=%d evictions=%d bypasses=%d oversize_bypasses=%d pinned=%d\n",
		st.Cache.Hits, st.Cache.Misses, st.Cache.EvictionsTotal, st.Cache.BypassEvents, st.Cache.OversizeBypasses, st.Cache.PinnedBlobs)
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
