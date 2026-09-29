package webdav

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/deployBunker/bunker/internal/invalidation"
)

// ---------------------------------------------------------------------------
// BFS-035: the server-side watcher.
//
// This file is the SAME CAPABILITY the `events` op (events.go) polls for, built
// the other way round: instead of observing the tree on every request and paying
// O(paths) (measured: 7.3 ms median for 2000 paths), it observes the CHANGES the
// kernel reports and pays O(changes). Two jobs, and the second is why the row is
// P0 rather than an upgrade:
//
//  1. SEE OUT-OF-BAND WRITERS. A request-handler-triggered invalidate is blind
//     to `git checkout`, an editor, a cron job, a deploy. The poll sees them
//     (mtime/ctime move whoever wrote) but costs a walk per poll.
//  2. ALIGN THE SERVER'S OWN DERIVED STATE. The server serves a revision token
//     and a one-call snapshot; an out-of-band edit makes both stale, so a
//     vouched-for change must move the served revision (D1/D2), forget the
//     path's cached hash (D4) and be journaled in the E-6 ledger (D5) — without
//     a stat per read (SPEC-watcher-capability §7.1/§7.2 R-V1/R-V2).
//
// The design authority is docs/prd/SPEC-watcher-capability.md. Three of its
// sections are the shape of this file, and reading it first is not optional:
//
//   - §5 THE OVERFLOW GUARANTEE. "An interval the watcher cannot vouch for is
//     NEVER reported as quiet." IN_Q_OVERFLOW drops events, so an overflow is
//     ALWAYS a full rescan, ALWAYS counted, and the number of dropped events is
//     never invented (the kernel reports one marker, not how many).
//   - §5.4 O-1/O-2, the two obligations that make the guarantee real rather than
//     a nicety. O-1: a DEDICATED DRAIN goroutine whose only job is to receive
//     from the backend's error channel continuously — handling errors inline in
//     the read loop is non-compliant, because the receive that unblocks the
//     library is the same receive that must keep up. O-2: LIVENESS IS NEVER
//     INFERRED FROM THE ABSENCE OF EVENTS — a healthy idle tree and a stalled
//     reader are indistinguishable from outside, so liveness comes from the
//     watcher's OWN heartbeat on a period it controls, and a quiet channel is
//     never reported as healthy.
//   - §4 THE PROBE MATRIX. Seven reasons a watcher can be absent or
//     untrustworthy, each with its own NAMED verdict and fallback, because
//     collapsing them into one string is what makes an operator hunt for a build
//     flag that will never exist for a netbacked target.
//
// It is a PROBED capability. This build never claims a watcher it does not have
// (§8.1): the state reported is the running process's, and a watcher that is
// configured but not established reports `absent` with the reason its own probe
// produced, never `push`.

// The runtime state vocabulary (§3.2). `overflow` is deliberately NOT an
// absence: the watcher exists and one interval lost its witness.
const (
	WatchStateWatching = "watching"
	WatchStateOverflow = "overflow"
	WatchStateLost     = "lost"
	WatchStateAbsent   = "absent"
)

// The closed reason vocabulary (§4). Seven facts, seven probes, seven names.
// `watch_overflow` is deliberately NOT one of them (§4.7): an overflow neither
// removes the watcher nor changes the mode, it invalidates an interval.
const (
	WatchReasonUnsupportedPlatform = "watch_unsupported_platform"
	WatchReasonLimitExhausted      = "watch_limit_exhausted"
	WatchReasonTargetNetbacked     = "watch_target_netbacked"
	WatchReasonPartialCoverage     = "watch_partial_coverage"
	WatchReasonInstallFailed       = "watch_install_failed"
	WatchReasonLost                = "watch_lost"
	WatchReasonBoundarySplit       = "watch_boundary_split"
)

// The backend names the document reports (§8.2 `backend`). `none` is a fact, not
// an error: a build with no watch facility reports W-1 with it.
const (
	watchBackendInotify = "inotify"
	watchBackendNone    = "none"
)

// Why an interval is unvouched. The kernel-dropped one is the reason the
// guarantee exists; the other two are the same consequence arriving from the
// watcher's own side, and they are named separately so a reader can tell "the
// kernel ate events" from "our reader stopped turning" — a distinction the
// dropped-events count can never make (§4.7, and the honesty rules of §3.3).
const (
	watchCauseKernelQueue   = "kernel_queue_overflow"
	watchCauseReaderStalled = "reader_stalled"
	watchCauseBackendError  = "backend_error"
	watchCauseReinstall     = "reinstall_gap"
)

// The overflow marker's declared null (§8.2): the kernel reports ONE marker, not
// how many events it dropped, so the count is null WITH a reason and never a
// fabricated number (§3.3 rule 2).
const watchDroppedEventsReason = "the kernel reports one overflow marker, not how many events it dropped"

// Tunables. The DECLARED defaults and ranges live in internal/invalidation
// (BFS-043's knob table) and are passed in per watcher through watchOptions:
// there is exactly one place a default is written down. watchStallBeats is not
// a config knob (it is a property of the stall detector's arithmetic, not a
// bound an operator has a reason to move), so it stays here.
const (
	// watchStallBeats is how many consecutive heartbeats may find the event loop
	// parked before the interval is declared unvouched. One beat is a scheduling
	// hiccup; two consecutive is a stopped loop.
	watchStallBeats = 2
)

// ---------------------------------------------------------------------------
// The backend seam.
//
// fsnotify is the basis the row names, and the trap measured in §5.4 lives
// inside it, so the interface below is shaped by that trap rather than around
// it: Events() and Errors() are TWO channels and the drain must read the second
// one continuously (O-1). The seam exists so the negative control of §11 A-5 can
// be built — a watcher with no drain must be able to run and FAIL — and so the
// probe matrix's error classes (ENOSPC, EACCES, ENOENT) can be exercised without
// exhausting a real kernel's watches.
// ---------------------------------------------------------------------------

type watchBackend interface {
	// Add installs one watch on a directory. A failure is returned unwrapped
	// (fsnotify does exactly that), so errors.Is reaches the errno.
	Add(dir string) error
	// Remove drops one watch.
	Remove(dir string) error
	// Close releases the backend and its goroutines.
	Close() error
	// Events is the change channel.
	Events() <-chan fsnotify.Event
	// Errors is the error channel. It MUST be drained (O-1): on the inotify
	// backend it is unbuffered and sendError blocks until received, so a watcher
	// that never reads it stops the library's reader dead.
	Errors() <-chan error
}

// fsnotifyBackend is the production backend: fsnotify v1.10.1, already in
// go.mod, promoted from an indirect dependency to a used one by this row.
type fsnotifyBackend struct{ w *fsnotify.Watcher }

func (b fsnotifyBackend) Add(dir string) error          { return b.w.Add(dir) }
func (b fsnotifyBackend) Remove(dir string) error       { return b.w.Remove(dir) }
func (b fsnotifyBackend) Close() error                  { return b.w.Close() }
func (b fsnotifyBackend) Events() <-chan fsnotify.Event { return b.w.Events }
func (b fsnotifyBackend) Errors() <-chan error          { return b.w.Errors }

func newFsnotifyBackend() (watchBackend, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return fsnotifyBackend{w: w}, nil
}

// mountRecord is one /proc/self/mountinfo record, reduced to the fields the
// probe matrix needs. Detection is by LONGEST MOUNTPOINT PREFIX (§4.3), which is
// the only match that gets a bind mount or a submount right.
type mountRecord struct {
	MountPoint string
	FSType     string
	Source     string
}

// watchCeilings is the configured side of §4.2's "configured vs observed". The
// per-user TOTAL in use is deliberately absent: it is not observable anywhere in
// procfs (App. A.3), which is exactly why the probe is attempt-and-classify
// rather than compute-and-refuse.
type watchCeilings struct {
	MaxUserWatches   int64
	MaxUserInstances int64
	MaxQueuedEvents  int64
	// Reason is set when a ceiling could not be read, so an unreadable file is
	// reported as such rather than silently as zero (§3.3 rule 2).
	Reason string
}

// watchEnv is everything the watcher reads from the host. It is a struct of
// functions rather than direct syscalls so the probe matrix's cases have cells
// that exercise each reason on its own, with the real probes on the control arm.
type watchEnv struct {
	// backendName is the compiled-in watch facility: "inotify" on Linux, "none"
	// where this build has no backend (W-1).
	backendName string
	// newBackend creates a backend, or fails with the errno that classifies it.
	newBackend func() (watchBackend, error)
	// readCeilings reads the configured inotify ceilings.
	readCeilings func() watchCeilings
	// readMounts reads the mount table.
	readMounts func() []mountRecord
}

// watchOptions carries the tunables one watcher runs with. Every numeric field
// is a knob from internal/invalidation (BFS-043), so the values here are the
// values the surface declares and reports; a cell may override any of them to
// exercise the behaviour they bound without waiting 30 s or building a
// 1 048 576-directory tree.
type watchOptions struct {
	heartbeat  time.Duration
	stallBeats int
	headroom   int
	flushEvery time.Duration
	// flushMaxPaths bounds one flushed path list; above it the flush is a full
	// rescan, never a longer or partial list (§5.1 claim 2). Default: the
	// surface's declared max_paths_per_event (4096).
	flushMaxPaths int
	// scanLimit bounds the install walk and a rescan, exactly as the poll's own
	// bound does.
	scanLimit int
	// maxWatches is the requested watch ceiling: 0 means AUTO, i.e. the
	// platform's own ceiling (invalidation.KnobWatchMaxWatches). A value the
	// platform cannot give is REPORTED, never clamped in silence.
	maxWatches int64

	// disableDrain is the O-1 NEGATIVE CONTROL. When true the watcher reads ONLY
	// the event channel — the shape §5.4 measures as silent — so a cell can
	// assert that the overflow it deliberately produces is NOT observed. A cell
	// that cannot fail proves nothing (§11 A-5); this is the switch that makes it
	// able to.
	disableDrain bool
	// loopGate parks the event loop while it is non-nil and unclosed. It exists
	// to reproduce a STALLED CONSUMER deterministically: with the loop parked the
	// library's reader blocks on the unbuffered channel, the kernel queue fills,
	// and IN_Q_OVERFLOW is produced for real (the App. A.4 reproducer, driven
	// through the real library instead of a raw descriptor).
	loopGate chan struct{}
}

// watchOptionsFrom is the ONE conversion from the declared config surface to the
// options the watcher runs with. New and the read-back both go through it, so the
// numbers reported and the numbers obeyed cannot diverge.
func watchOptionsFrom(v invalidation.Values) watchOptions {
	return watchOptions{
		heartbeat:     time.Duration(v.Watch.HeartbeatMS) * time.Millisecond,
		stallBeats:    watchStallBeats,
		headroom:      v.Watch.InstallHeadroom,
		flushEvery:    time.Duration(v.Watch.FlushEveryMS) * time.Millisecond,
		flushMaxPaths: v.Watch.FlushMaxPaths,
		scanLimit:     v.Watch.ScanLimit,
		maxWatches:    v.Watch.MaxWatches,
	}
}

// defaultWatchOptions is the watcher's options at the DECLARED defaults, which is
// what a deployment with no invalidation block gets. It is derived from the knob
// table rather than restated, so a default can only be changed in one place.
func defaultWatchOptions() watchOptions {
	return watchOptionsFrom(invalidation.DefaultValues())
}

// ---------------------------------------------------------------------------
// The reported picture (§8.2's block, as Go values).
// ---------------------------------------------------------------------------

type watchTarget struct {
	MountType      string  `json:"mount_type"`
	MountPoint     string  `json:"mount_point"`
	NetworkBacked  bool    `json:"network_backed"`
	BoundarySplit  triBool `json:"boundary_split"`
	BoundaryReason string  `json:"boundary_split_reason,omitempty"`
}

type watchCoverage struct {
	DirectoriesDesired int      `json:"directories_desired"`
	DirectoriesWatched int      `json:"directories_watched"`
	MissingCount       int      `json:"missing_count"`
	Missing            []string `json:"missing"`
	MissingReason      string   `json:"missing_reason,omitempty"`
	Complete           bool     `json:"complete"`
	Headroom           int      `json:"headroom"`
}

type watchLimits struct {
	Watching     map[string]any `json:"watching"`
	Instances    map[string]any `json:"instances"`
	QueuedEvents map[string]any `json:"queued_events"`
}

type watchCounters struct {
	OverflowsTotal        uint64 `json:"overflows_total"`
	UnvouchedTotal        uint64 `json:"unvouched_total"`
	RescansTotal          uint64 `json:"rescans_total"`
	InstallFailuresTotal  uint64 `json:"install_failures_total"`
	BackendErrorsTotal    uint64 `json:"backend_errors_total"`
	OverflowDroppedEvents *int64 `json:"overflow_dropped_events"`
	OverflowDroppedReason string `json:"overflow_dropped_reason"`
	UnvouchedReason       string `json:"unvouched_reason,omitempty"`
	LastEventAgeMS        *int64 `json:"last_event_age_ms"`
	ChangedSince          string `json:"changed_since,omitempty"`
	HeartbeatsTotal       uint64 `json:"heartbeats_total"`
	EventLoopTicks        uint64 `json:"event_loop_ticks"`
	WatchesAdded          int64  `json:"watches_added"`
}

// watchStatus is the whole running picture. ops.go renders it into §8.2's block
// and the refusal, so the wire shape has exactly one producer.
type watchStatus struct {
	State       string
	Reason      string
	BlocksPush  bool
	Backend     string
	Detail      string
	Target      watchTarget
	Coverage    *watchCoverage
	Limits      watchLimits
	Counters    *watchCounters
	Vouched     bool
	Stalled     bool
	HeartbeatMS int
	// Ceiling is §4.2's arithmetic (requested vs platform vs need) and
	// Unhonoured is the configured-vs-observed pair it produced, when any
	// configured value could not be honoured (BFS-043). Both carriers come from
	// the same field, so the refusal and the document cannot disagree.
	Ceiling    *invalidation.WatchCeiling
	Unhonoured *invalidation.Unhonoured
}

// triBool is §4.8's three-valued field: `true`, `false`, or the string
// "unknown". A silent false for an unmeasurable fact is a fabricated
// measurement (Bane's null doctrine), so the third value has its own spelling.
type triBool struct{ v string }

func triTrue() triBool  { return triBool{v: "true"} }
func triFalse() triBool { return triBool{v: "false"} }
func triUnknown() triBool {
	return triBool{v: "unknown"}
}

func (t triBool) MarshalJSON() ([]byte, error) {
	switch t.v {
	case "true":
		return []byte("true"), nil
	case "false":
		return []byte("false"), nil
	default:
		return []byte(`"unknown"`), nil
	}
}

func (t triBool) IsTrue() bool  { return t.v == "true" }
func (t triBool) IsFalse() bool { return t.v == "false" }
func (t triBool) Reason() string {
	if t.v == "unknown" {
		return "the mount table could not be read, so the served root's superblock relation to its writers is unknown rather than false"
	}
	return ""
}

// ---------------------------------------------------------------------------
// The watcher.
// ---------------------------------------------------------------------------

type watcher struct {
	tree *tree
	root string
	env  watchEnv
	opts watchOptions

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	backend watchBackend

	// rescanCh carries a full-rescan request from a goroutine that must not do
	// slow work on its own receive (O-1). It is buffered by one so the drain never
	// blocks publishing the fact, and a second request while one is pending is
	// coalesced — two overflows in a row are one lost interval, not two rescans.
	rescanCh chan string

	mu        sync.Mutex
	state     string
	reason    string
	detail    string
	target    watchTarget
	coverage  watchCoverage
	limits    watchLimits
	limitsRaw watchCeilings
	// ceiling is §4.2's arithmetic as BFS-043 computes it: requested (the
	// configured watch ceiling), platform (fs.inotify.max_user_watches), what the
	// tree needs, and the ceiling actually in force. It is recorded before
	// anything is installed, so the refusal, the document and the degradation
	// list report the same configured-vs-observed numbers.
	ceiling invalidation.WatchCeiling
	stalled bool
	// stalledNow/wasStalled are the stall detector's state (O-2), guarded by mu.
	// wasStalled is the PREVIOUS beat's verdict and stalledNow the current one:
	// one consecutive parked beat is a scheduling hiccup, two is a stopped loop.
	stalledNow bool
	wasStalled bool
	lastErr    string
	watched    map[string]struct{}
	missing    []string
	pending    map[string]string // tree-relative path -> absolute path
	gate       chan struct{}

	overflows       atomic.Uint64
	unvouched       atomic.Uint64
	rescans         atomic.Uint64
	installFailures atomic.Uint64
	backendErrors   atomic.Uint64
	watchedChanges  atomic.Uint64
	heartbeats      atomic.Uint64
	loopTicks       atomic.Uint64
	lastHeartbeatAt atomic.Int64
	lastEventAt     atomic.Int64
	changedSince    atomic.Int64
	unvouchedReason atomic.Value // string
	loopTicksSeen   uint64
}

// startWatcher probes, installs and starts the watcher. It NEVER returns an
// error: an install that cannot be made is a REPORTABLE state (§4), not a
// failure of the surface, and a watcher that refuses to start must still be able
// to say why.
func startWatcher(t *tree, env watchEnv, opts watchOptions) *watcher {
	w := &watcher{
		tree:     t,
		root:     t.rootPath(),
		env:      env,
		opts:     opts,
		done:     make(chan struct{}),
		rescanCh: make(chan string, 1),
		state:    WatchStateAbsent,
		watched:  map[string]struct{}{},
		pending:  map[string]string{},
		gate:     opts.loopGate,
	}
	w.install()
	return w
}

func (w *watcher) Close() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() { close(w.done) })
	w.mu.Lock()
	b := w.backend
	w.mu.Unlock()
	if b != nil {
		_ = b.Close()
	}
	w.wg.Wait() // no WaitGroup is ever started for an absent watcher; this is a no-op then
}

// ---------------------------------------------------------------------------
// Install: the probe matrix in the order §4.2 mandates.
// ---------------------------------------------------------------------------

func (w *watcher) install() {
	probe := probeWatch(w.root, w.env)
	w.mu.Lock()
	w.target = probe.target
	w.limitsRaw = probe.ceilings
	w.limits = limitsBlock(probe.ceilings)
	w.mu.Unlock()

	// W-1 — no watch facility to use on this build/kernel.
	if probe.backendName == watchBackendNone {
		w.absent(WatchReasonUnsupportedPlatform, fmt.Sprintf(
			"no filesystem watch facility on this build/kernel (backend=none); the push form is not served and cannot be, for any target of this build"))
		w.tree.watchLive.Store(false)
		return
	}

	// W-3 — the local kernel is not the writer. This absence is PERMANENT for
	// this target and must never be reported as a build gap an operator can fix.
	if probe.target.NetworkBacked {
		w.absent(WatchReasonTargetNetbacked, fmt.Sprintf(
			"the served root %s is a %s mount (mount_point %s): the local kernel is not the writer and no local watch can observe it; this is not a build gap and will not change for this target",
			w.root, probe.target.MountType, probe.target.MountPoint))
		w.tree.watchLive.Store(false)
		return
	}

	// The install walk. It is the same walk the installer needs, so it is not
	// extra work — and it is bounded exactly as the poll's observation is.
	dirs, truncated, err := w.walkDirs()
	if err != nil {
		w.installFailures.Add(1)
		w.absent(WatchReasonInstallFailed, fmt.Sprintf(
			"the served tree could not be walked to build the watch set: %v", err))
		w.tree.watchLive.Store(false)
		return
	}
	desired := len(dirs)
	if truncated {
		// A tree larger than the scan bound cannot be claimed as covered, and a
		// truncated watch set is exactly the silent-wrong-answer shape of W-4.
		w.absent(WatchReasonPartialCoverage, fmt.Sprintf(
			"the served tree exceeds the install walk's bound of %d directories: the watch set would be short and a change under an uncovered directory produces no event", w.opts.scanLimit))
		w.tree.watchLive.Store(false)
		return
	}

	// W-2's pre-flight: the ceiling IN FORCE vs the directories this tree wants,
	// WITH headroom. §4.2 mandates the attempt-and-classify shape, and BFS-043
	// makes the ceiling a configured value: the knob asks, the platform gives,
	// and the number actually in force is the smaller of the two. The decision is
	// recorded on the watcher before anything is installed, so the refusal, the
	// capability document and the degradation list all carry the SAME
	// configured-vs-observed pair (§3.1: the carriers can never disagree).
	//
	// It cannot be a proof (the per-user total in use is not observable) and it is
	// not treated as one: the add loop below classifies every error, and that
	// classification is the authoritative signal.
	w.setCeiling(watchCeilingFor(probe.ceilings.MaxUserWatches, desired, w.opts))
	if !w.ceiling.Fits() {
		w.absent(WatchReasonLimitExhausted, fmt.Sprintf(
			"the watch set needs %d watches (%d directories plus %d headroom); the ceiling %s in force is %d, which this tree does not fit under, and it is the %s number (requested %s, platform %d, per-user total in use not observable); the remedy is an operator action on the ceiling",
			w.ceiling.Need(), desired, w.opts.headroom, limitNameWatches, w.ceiling.Effective(),
			w.ceiling.Binding(), requestedSpelling(w.ceiling), probe.ceilings.MaxUserWatches))
		w.setLimitFailure(limitNameWatches, w.ceiling.Effective(), 0, desired, -1, "")
		w.tree.watchLive.Store(false)
		return
	}

	backend, err := w.env.newBackend()
	if err != nil {
		w.installFailures.Add(1)
		if isResourceExhaustion(err) {
			w.absent(WatchReasonLimitExhausted, fmt.Sprintf(
				"a watch instance could not be created: %v; %s is %d and one instance is needed per watcher",
				err, limitNameInstances, probe.ceilings.MaxUserInstances))
			w.setLimitFailure(limitNameInstances, probe.ceilings.MaxUserInstances, 0, desired, 0, errnoName(err))
			w.tree.watchLive.Store(false)
			return
		}
		w.absent(WatchReasonInstallFailed, fmt.Sprintf(
			"the watch backend could not be created: %v", err))
		w.tree.watchLive.Store(false)
		return
	}

	added := 0
	var refused []string
	for _, dir := range dirs {
		if err := backend.Add(dir); err != nil {
			switch {
			case isResourceExhaustion(err):
				// §4.2(f): the partial watch set is TORN DOWN rather than kept.
				w.teardown(backend, added)
				w.absent(WatchReasonLimitExhausted, fmt.Sprintf(
					"the add failed at #%d with %s: the watch set needs %d watches and the configured ceiling %s is %d (this uid's watch total is shared with other processes and its in-use total is not observable); the partial watch set was torn down rather than kept",
					added+1, errnoName(err), desired, limitNameWatches, probe.ceilings.MaxUserWatches))
				w.setLimitFailure(limitNameWatches, probe.ceilings.MaxUserWatches, added, desired, added, errnoName(err))
				w.tree.watchLive.Store(false)
				return
			case errors.Is(err, os.ErrPermission):
				// §4.5: not installable as this uid. Recorded as a missing
				// directory rather than skipped silently — this is how W-4 arises.
				refused = append(refused, relayPath(w.root, dir))
				continue
			default:
				w.teardown(backend, added)
				w.installFailures.Add(1)
				w.absent(WatchReasonInstallFailed, fmt.Sprintf(
					"watch install failed on %s: %s; the incomplete watch set was torn down", relayPath(w.root, dir), errnoName(err)))
				w.mu.Lock()
				w.lastErr = err.Error()
				w.mu.Unlock()
				w.tree.watchLive.Store(false)
				return
			}
		}
		added++
		w.mu.Lock()
		w.watched[dir] = struct{}{}
		w.mu.Unlock()
	}

	w.mu.Lock()
	w.backend = backend
	w.coverage = watchCoverage{
		DirectoriesDesired: desired,
		DirectoriesWatched: added,
		MissingCount:       len(refused),
		Missing:            cappedMissing(refused),
		Complete:           len(refused) == 0,
		Headroom:           w.opts.headroom,
	}
	w.mu.Unlock()

	if len(refused) > 0 {
		// W-4 — the one case where the watcher LOOKS healthy and is quietly
		// wrong. It is never reported as `watching` (§4.4). The watch set is kept
		// (a forward CREATE can heal it) but nothing claims coverage over the
		// missing subtrees.
		w.absent(WatchReasonPartialCoverage, fmt.Sprintf(
			"%d of %d directories are not watched (%s): the watch set is short and a change under a missing directory produces no event",
			len(refused), desired, strings.Join(cappedMissing(refused), ", ")))
		w.tree.watchLive.Store(false)
	} else {
		w.mu.Lock()
		w.state = WatchStateWatching
		w.reason = ""
		w.detail = fmt.Sprintf("%d directories watched (%s, %s): out-of-band changes move the served revision",
			added, w.target.MountType, w.target.MountPoint)
		w.mu.Unlock()
		w.tree.watchLive.Store(true)
	}
	w.start()
}

// teardown drops every watch the install managed to add and closes the backend.
// §4.2(f)/§4.5: a partial set is never kept as a success.
func (w *watcher) teardown(backend watchBackend, added int) {
	w.mu.Lock()
	held := make([]string, 0, len(w.watched))
	for dir := range w.watched {
		held = append(held, dir)
	}
	w.watched = map[string]struct{}{}
	w.mu.Unlock()
	for _, dir := range held {
		_ = backend.Remove(dir)
	}
	_ = backend.Close()
}

// start launches the three goroutines the obligations require. Their roles are
// not interchangeable and the split is the compliance:
//
//	loop      — the EVENT channel: coalesces changes and publishes them.
//	drain     — the ERROR channel: receives continuously (O-1) and does no slow
//	            work on that receive.
//	heartbeat — the watcher's OWN clock (O-2): the only admissible liveness.
func (w *watcher) start() {
	w.wg.Add(2)
	go func() { defer w.wg.Done(); w.loop() }()
	go func() { defer w.wg.Done(); w.heartbeatLoop() }()
	w.startDrain()
}

// startDrain launches the O-1 drain for whichever backend is current when it is
// called, and is called once per backend GENERATION: the first install here, and
// every successful re-install after a stop. A drain that was only ever started
// once would leave the second backend's error channel unread — and because
// sendError blocks until received, that is not a missed notice but a stopped
// reader (§5.4).
//
// The Add happens on the caller's goroutine while the WaitGroup is non-zero
// (the loop that calls this from onBackendClosed is itself counted), so it can
// never race a Close's Wait.
func (w *watcher) startDrain() {
	w.wg.Add(1)
	if w.opts.disableDrain {
		// The negative control. It is deliberately still a goroutine, so the
		// shape under test is "no one reads Errors", not "the process died".
		go func() { defer w.wg.Done() }()
		return
	}
	go func() { defer w.wg.Done(); w.drain() }()
}

// ---------------------------------------------------------------------------
// O-1: the dedicated drain.
// ---------------------------------------------------------------------------

func (w *watcher) drain() {
	ch := w.backendErrorChan()
	for {
		select {
		case <-w.done:
			return
		case err, ok := <-ch:
			if !ok {
				return
			}
			w.onBackendError(err)
		}
	}
}

// onBackendError classifies one error and does nothing else: publishing "this
// interval is unvouched" is the whole job of this receive, and the rescan it
// implies is handed to the loop (O-1: the receive that unblocks the library is
// the same receive that must keep up).
func (w *watcher) onBackendError(err error) {
	switch {
	case errors.Is(err, fsnotify.ErrEventOverflow):
		// The kernel dropped a burst. Loss is unbounded and unreported, so the
		// interval is unvouched: state overflow, ALWAYS a full rescan, ALWAYS
		// counted, and the dropped count is never invented (§5, §5.4).
		w.overflows.Add(1)
		w.markUnvouched(watchCauseKernelQueue)
		w.requestRescan(watchCauseKernelQueue)
	default:
		// Any other backend error means this reader cannot vouch for the interval
		// either. Treated the same way and counted separately, because "the kernel
		// ate events" and "our reader faulted" are different facts.
		w.backendErrors.Add(1)
		w.mu.Lock()
		w.lastErr = err.Error()
		w.mu.Unlock()
		w.markUnvouched(watchCauseBackendError)
		w.requestRescan(watchCauseBackendError)
	}
}

func (w *watcher) requestRescan(cause string) {
	select {
	case w.rescanCh <- cause:
	default:
		// A rescan is already pending: one lost interval is one rescan.
	}
}

// markUnvouched publishes "this interval cannot be vouched for". It is the only
// writer of the overflow state, and it is what makes §5's first sentence true:
// silence is never left to read as quiet.
func (w *watcher) markUnvouched(cause string) {
	w.unvouched.Add(1)
	w.unvouchedReason.Store(cause)
	w.mu.Lock()
	if w.state != WatchStateLost {
		w.state = WatchStateOverflow
	}
	w.mu.Unlock()
	// An unvouched interval does not remove the watcher: the watch set is intact
	// and (per §3.2) an overflow never changes the mode. The token keeps its
	// composite kind while a complete watch set exists.
	w.tree.watchLive.Store(true)
}

// ---------------------------------------------------------------------------
// O-2: the heartbeat, and the stall detector it enables.
// ---------------------------------------------------------------------------

func (w *watcher) heartbeatLoop() {
	t := time.NewTicker(w.opts.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-t.C:
			w.heartbeats.Add(1)
			w.lastHeartbeatAt.Store(time.Now().UnixNano())
			ticks := w.loopTicks.Load()
			w.mu.Lock()
			moved := ticks != w.loopTicksSeen
			w.loopTicksSeen = ticks
			stalled := !moved
			// Two consecutive beats with a parked loop: the reader is not turning.
			// A healthy IDLE tree is excluded by construction — the loop ticks on
			// its own clock as well as on events, so an idle tree still advances
			// it and only a STOPPED loop can double-miss.
			w.stalledNow = stalled && w.wasStalled
			w.wasStalled = stalled
			unvouched := w.stalledNow && w.establishedLocked()
			w.mu.Unlock()
			if unvouched {
				// A stalled reader and a healthy idle tree are indistinguishable
				// from outside; the heartbeat is what makes them distinguishable.
				w.markUnvouched(watchCauseReaderStalled)
				w.requestRescan(watchCauseReaderStalled)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The event loop.
// ---------------------------------------------------------------------------

func (w *watcher) loop() {
	flush := time.NewTicker(w.opts.flushEvery)
	defer flush.Stop()
	for {
		if g := w.currentGate(); g != nil {
			select {
			case <-g:
				w.clearGate()
			case <-w.done:
				return
			}
			continue
		}
		select {
		case <-w.done:
			return
		case ev, ok := <-w.backendEvents():
			if !ok {
				// The backend stopped mid-life (§4.6 W-6). A successful
				// re-install is a NEW backend with NEW channels, and both the
				// event read and the drain have to follow it: returning here
				// would leave a watcher that reports `watching` while nothing is
				// being read — the silent shape O-1/O-2 exist to prevent. Only a
				// failed re-install is an absence.
				if !w.onBackendClosed() {
					return
				}
				continue
			}
			w.loopTicks.Add(1)
			w.recordEvent(ev)
		case <-flush.C:
			w.loopTicks.Add(1)
			w.flush()
		case cause := <-w.rescanCh:
			w.loopTicks.Add(1)
			w.rescan(cause)
		}
	}
}

// recordEvent coalesces one backend event. Nothing slow happens here: the flush
// below does the work, so a burst is O(changes) and never O(changes × work).
func (w *watcher) recordEvent(ev fsnotify.Event) {
	name := ev.Name
	if name == "" {
		return
	}
	if !strings.HasPrefix(name, w.root) {
		return
	}
	base := filepath.Base(name)
	if isTempName(base) {
		// The surface's own staging files are not part of the served tree (every
		// listing skips them), so they are not a change to report.
		return
	}
	w.lastEventAt.Store(time.Now().UnixNano())

	// Forward coverage maintenance (§4.4): the library's recursive watch is
	// disabled (enableRecurse = false, fsnotify.go:503-515), so the set is ours to
	// keep, and a directory created under a watched tree is watched only because
	// this branch adds it.
	if fi, err := os.Stat(name); err == nil && fi.IsDir() {
		w.addWatch(name)
	} else if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		w.dropWatch(name)
	}

	rel := relayPath(w.root, name)
	w.mu.Lock()
	w.pending[rel] = name
	w.mu.Unlock()
}

// addWatch installs one forward watch, counting coverage. A failure is recorded
// as a missing directory rather than ignored: a silently short watch set is
// exactly the invisible wrong answer W-4 exists to prevent.
func (w *watcher) addWatch(dir string) {
	w.mu.Lock()
	_, already := w.watched[dir]
	w.mu.Unlock()
	if already {
		return
	}
	backend := w.currentBackend()
	if backend == nil {
		return
	}
	if err := backend.Add(dir); err != nil {
		w.mu.Lock()
		w.missing = append(w.missing, relayPath(w.root, dir))
		w.mu.Unlock()
		return
	}
	w.mu.Lock()
	w.watched[dir] = struct{}{}
	w.coverage.DirectoriesWatched = len(w.watched)
	w.coverage.Complete = len(w.missing) == 0
	w.mu.Unlock()
}

func (w *watcher) dropWatch(dir string) {
	backend := w.currentBackend()
	if backend != nil {
		_ = backend.Remove(dir)
	}
	w.mu.Lock()
	delete(w.watched, dir)
	w.coverage.DirectoriesWatched = len(w.watched)
	w.mu.Unlock()
}

// flush publishes the coalesced path list and ALIGNS THE SERVER'S OWN DERIVED
// STATE, which is the row's second job:
//
//	D1/D2 — the served revision moves (no stat: the event is the witness, R-V2);
//	D4    — the path's cached hash is forgotten;
//	D5    — the E-6 ledger journals the change, so a poller sees it too.
//
// A flush that cannot be vouched for is a FULL RESCAN, never a longer or partial
// list (§5.1 claim 2).
func (w *watcher) flush() {
	w.mu.Lock()
	if len(w.pending) == 0 {
		w.mu.Unlock()
		return
	}
	pending := w.pending
	w.pending = map[string]string{}
	w.mu.Unlock()

	rels := make([]string, 0, len(pending))
	for rel := range pending {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	if len(rels) > w.opts.flushMaxPaths {
		// Knowledge lost in our own queue: the same consequence as a kernel
		// overflow, handled identically and counted as one.
		w.overflows.Add(1)
		w.markUnvouched(watchCauseKernelQueue)
		w.rescan(watchCauseKernelQueue)
		return
	}

	for _, rel := range rels {
		if abs, ok := pending[rel]; ok {
			w.tree.forget(abs)
		}
	}
	w.noteWatchedChanges(uint64(len(rels)))
	w.journal(eventInvalidate, rels)
}

// rescan is §5's "always a full rescan": one bounded observation of the whole
// tree, diffed against the ledger, answered as an `overflow` event with an EMPTY
// path list — never a diff of the part that fit — and counted.
func (w *watcher) rescan(cause string) {
	state, truncated, err := w.tree.observe()
	if err != nil {
		w.backendErrors.Add(1)
		w.mu.Lock()
		w.lastErr = err.Error()
		w.mu.Unlock()
		return
	}
	l := w.tree.eventLedger()
	l.mu.Lock()
	changed, next := w.tree.ledgerDiff(l.observed, state, time.Now())
	l.started = true
	l.observed = next
	if truncated {
		// An incomplete observation cannot be presented as complete: it is the
		// overflow it is, with no paths at all.
		changed = nil
	}
	// The SERVER's own derived state is aligned BEFORE the line is written, so
	// the line's `rev` is the revision the tree serves once this interval has
	// been accounted for. That is the ordering the poll path already has: there,
	// a mutation through the surface has moved the token by the time its event is
	// journaled, so a line whose `rev` lagged the tree would be the watcher's
	// alone and would disagree with the reader beside it. A truncated
	// observation aligns nothing (nothing can be vouched for).
	if !truncated {
		for _, rel := range changed {
			w.tree.forget(filepath.Join(w.root, filepath.FromSlash(rel)))
		}
		w.noteWatchedChanges(uint64(len(changed)))
	}
	l.push(w.tree, eventOverflow, nil)
	l.mu.Unlock()

	if truncated {
		// Nothing can be vouched for, so nothing is aligned either: the tree is
		// larger than any observation this build can take, and the next rescan
		// says so again rather than pretending.
		w.absent(WatchReasonPartialCoverage, fmt.Sprintf(
			"the served tree exceeds the observation bound of %d paths: a rescan cannot vouch for it and no interval is reported as quiet", w.opts.scanLimit))
		w.tree.watchLive.Store(false)
		return
	}

	w.rescans.Add(1)
	if len(changed) > 0 {
		w.changedSince.Store(time.Now().UnixNano())
	}

	// The interval is vouched for again: the rescan is the evidence, so the state
	// returns to `watching` only here — never because time passed (§5.4 O-2).
	w.mu.Lock()
	if w.state == WatchStateOverflow {
		w.state = WatchStateWatching
		w.reason = ""
		w.detail = fmt.Sprintf("rescan after %s: %d paths changed", cause, len(changed))
	}
	complete := w.coverage.Complete
	w.mu.Unlock()
	w.tree.watchLive.Store(complete)
}

// noteWatchedChanges is D1/D2: the served revision's watcher half. It advances a
// monotonic counter and nothing else — no stat, no walk (R-V2).
func (w *watcher) noteWatchedChanges(n uint64) {
	if n == 0 {
		return
	}
	w.watchedChanges.Add(n)
	// D1/D2: this is the SERVED revision's watcher half. The tree owns the
	// token, so the count is published to it — the token then moves for a change
	// nobody made through this surface, which is R-V1. It costs two atomic loads
	// on a read path and no stat (R-V2), and that is the whole reason the count
	// lives here rather than being measured on every read.
	w.tree.watched.Add(n)
	w.changedSince.Store(time.Now().UnixNano())
}

// journal is D5: the change is recorded in the tree's own E-6 ledger, so a poll
// from a client that was not subscribed still sees it. The ledger's `observed`
// baseline is deliberately NOT advanced here — the watcher knows a path moved,
// not what it moved to, and a later poll that re-reports the path is a
// duplicate drop, never a miss (§6.1).
func (w *watcher) journal(name string, rels []string) {
	l := w.tree.eventLedger()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.push(w.tree, name, rels)
}

// onBackendClosed handles a mid-life stop (§4.6 W-6): the interval is unvouched,
// a re-install is attempted, and only a failed re-install is an absence. It
// reports whether the watcher is serving again, because the event loop has to
// know whether to keep reading (a new backend's channels) or to stop.
func (w *watcher) onBackendClosed() bool {
	w.markUnvouched(watchCauseReinstall)
	if w.reinstall() {
		// The new backend's error channel needs a drain of its OWN: the previous
		// drain returned when the previous channel closed, and the receive that
		// unblocks the library must exist from the new backend's first event
		// (O-1 is a property of the RUNNING watcher, not of its first install).
		w.startDrain()
		w.mu.Lock()
		w.state = WatchStateWatching
		w.reason = ""
		w.detail = "the watch set was re-established after the backend closed; the interval before it is unvouched"
		w.mu.Unlock()
		w.tree.watchLive.Store(true)
		w.requestRescan(watchCauseReinstall)
		return true
	}
	w.mu.Lock()
	w.state = WatchStateLost
	w.reason = WatchReasonLost
	w.detail = fmt.Sprintf("the watcher stopped at %s and could not be re-established; the interval after that instant is unvouched for",
		time.Now().Format(time.RFC3339))
	w.mu.Unlock()
	w.tree.watchLive.Store(false)
	return false
}

func (w *watcher) reinstall() bool {
	backend, err := w.env.newBackend()
	if err != nil {
		w.mu.Lock()
		w.lastErr = err.Error()
		w.mu.Unlock()
		return false
	}
	w.mu.Lock()
	dirs := make([]string, 0, len(w.watched))
	for dir := range w.watched {
		dirs = append(dirs, dir)
	}
	w.mu.Unlock()
	for _, dir := range dirs {
		if err := backend.Add(dir); err != nil {
			_ = backend.Close()
			w.mu.Lock()
			w.lastErr = err.Error()
			w.mu.Unlock()
			return false
		}
	}
	old := w.currentBackend()
	w.mu.Lock()
	w.backend = backend
	w.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return true
}

// ---------------------------------------------------------------------------
// Small helpers.
// ---------------------------------------------------------------------------

// walkDirs collects the directories the watch set must cover, bounded by the
// configured scan limit. It is the same walk the installer needs, so the ceiling
// probe costs no extra pass (§4.2(b)).
func (w *watcher) walkDirs() ([]string, bool, error) {
	var dirs []string
	truncated := false
	err := filepath.WalkDir(w.root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if p == w.root {
				return walkErr
			}
			// A directory that vanished or is unreadable mid-walk is not a reason
			// to fail: it is recorded as missing when the Add is attempted.
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if isTempName(d.Name()) {
			return filepath.SkipDir
		}
		if len(dirs) >= w.opts.scanLimit {
			truncated = true
			return fs.SkipAll
		}
		dirs = append(dirs, p)
		return nil
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		return nil, truncated, err
	}
	return dirs, truncated, nil
}

// cappedMissing applies the same bound the surface already uses for one path
// list (§4.4): above the cap the list is EMPTY and the count carries the number
// — never a truncated list presented as complete.
func cappedMissing(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if len(out) >= eventsMaxPathsPerEvent {
			return []string{}
		}
		out = append(out, p)
	}
	return out
}

func relayPath(root, abs string) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

func (w *watcher) currentBackend() watchBackend {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.backend
}

func (w *watcher) backendEvents() <-chan fsnotify.Event {
	b := w.currentBackend()
	if b == nil {
		return nil
	}
	return b.Events()
}

// backendErrorChan is the backend's error channel. It is deliberately NOT named
// backendErrors: that name is the COUNTER (how many backend errors this watcher
// has seen), and a field and a method of one name cannot coexist.
func (w *watcher) backendErrorChan() <-chan error {
	b := w.currentBackend()
	if b == nil {
		return nil
	}
	return b.Errors()
}

func (w *watcher) currentGate() chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gate
}

func (w *watcher) clearGate() {
	w.mu.Lock()
	w.gate = nil
	w.mu.Unlock()
}

// establishedLocked reports whether a complete watch set exists. Callers hold
// w.mu.
func (w *watcher) establishedLocked() bool {
	return w.state == WatchStateWatching || w.state == WatchStateOverflow
}

// absent records a named absence. `detail` must name the OBSERVED EVIDENCE and
// not the category (§3.1): a detail that could have been written without probing
// anything is a defect.
func (w *watcher) absent(reason, detail string) {
	w.mu.Lock()
	w.state = WatchStateAbsent
	w.reason = reason
	w.detail = detail
	w.mu.Unlock()
}

func (w *watcher) setLimitFailure(name string, configured int64, added, desired, watchesAdded int, errno string) {
	w.mu.Lock()
	blk := limitsBlock(w.limitsRaw)
	entry := map[string]any{
		"limit_name":                   name,
		"configured":                   configured,
		"watches_held_by_this_process": added,
		"desired":                      desired,
		"headroom":                     w.opts.headroom,
		"errno":                        errno,
	}
	if name == limitNameWatches {
		// BFS-043: the requested-vs-platform pair beside the ceiling in force, so
		// "you asked for 8192 watches and this kernel gives 128" is readable as
		// numbers rather than inferred from prose. requested is null when the knob
		// is AUTO (0): a fabricated 0 here would read as "the operator asked for
		// nothing" (§3.3 rule 2 — a number that is not knowable is null).
		entry["requested_max_watches"] = requestedOrNil(w.ceiling.Requested)
		entry["platform_max_user_watches"] = w.ceiling.Platform
		entry["ceiling_in_force"] = w.ceiling.Effective()
	}
	blk.Watching = entry
	w.limits = blk
	w.mu.Unlock()
}

// setCeiling records the ceiling decision (§4.2's arithmetic) on the watcher.
func (w *watcher) setCeiling(c invalidation.WatchCeiling) {
	w.mu.Lock()
	w.ceiling = c
	w.mu.Unlock()
}

// unhonoured is the configured-vs-observed pair this watcher recorded, or nil
// when every configured value was honoured.
func (w *watcher) unhonoured() *invalidation.Unhonoured {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ceiling.Unhonoured()
}

// watchCeilingFor assembles §4.2's arithmetic from the probes and the config:
// what was requested (the knob; 0 = AUTO), what the platform gives, and what the
// tree needs with its declared headroom.
func watchCeilingFor(platform int64, desired int, opts watchOptions) invalidation.WatchCeiling {
	return invalidation.WatchCeiling{
		Requested: opts.maxWatches,
		Platform:  platform,
		Desired:   int64(desired),
		Headroom:  int64(opts.headroom),
	}
}

// requestedSpelling renders the requested ceiling for a detail line: the AUTO
// sentinel is a fact to state, never a number to print.
func requestedSpelling(c invalidation.WatchCeiling) string {
	if c.Requested <= 0 {
		return "AUTO (the platform's own ceiling)"
	}
	return fmt.Sprintf("%d", c.Requested)
}

// requestedOrNil is the null-with-a-reason form for the same value in the
// document: AUTO is reported as null, never as 0.
func requestedOrNil(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

// statusSnapshot is the wire-facing picture.
func (w *watcher) statusSnapshot() watchStatus {
	w.mu.Lock()
	st := watchStatus{
		State:      w.state,
		Reason:     w.reason,
		Detail:     w.detail,
		Target:     w.target,
		Limits:     w.limits,
		Vouched:    w.state == WatchStateWatching,
		Stalled:    w.stalledNow,
		Coverage:   &w.coverage,
		BlocksPush: true,
	}
	w.mu.Unlock()
	st.Backend = w.env.backendName
	st.HeartbeatMS = int(w.opts.heartbeat / time.Millisecond)

	// The ceiling decision and its configured-vs-observed pair. Reading them off
	// the running watcher (not off the config) is what makes the read-back
	// describe what was applied: these are the numbers this process computed from
	// the probes it actually ran.
	w.mu.Lock()
	ceiling := w.ceiling
	w.mu.Unlock()
	st.Ceiling = &ceiling
	st.Unhonoured = ceiling.Unhonoured()

	// §8.2 declares overflow_dropped_events as null and §3.3 rule 2 forbids
	// inventing the number: the kernel reports ONE overflow marker, not how many
	// events it dropped, so the value is null and watchDroppedEventsReason
	// travels beside it. A count here would be a fabricated measurement.
	var droppedPtr *int64
	lastEvent := w.lastEventAt.Load()
	var age *int64
	if lastEvent > 0 {
		v := time.Since(time.Unix(0, lastEvent)).Milliseconds()
		age = &v
	}
	changed := w.changedSince.Load()
	cs := ""
	if changed > 0 {
		cs = time.Unix(0, changed).Format(time.RFC3339)
	}
	reason := ""
	if v, ok := w.unvouchedReason.Load().(string); ok {
		reason = v
	}
	st.Counters = &watchCounters{
		OverflowsTotal:        w.overflows.Load(),
		UnvouchedTotal:        w.unvouched.Load(),
		RescansTotal:          w.rescans.Load(),
		InstallFailuresTotal:  w.installFailures.Load(),
		BackendErrorsTotal:    w.backendErrors.Load(),
		OverflowDroppedEvents: droppedPtr,
		OverflowDroppedReason: watchDroppedEventsReason,
		UnvouchedReason:       reason,
		LastEventAgeMS:        age,
		ChangedSince:          cs,
		HeartbeatsTotal:       w.heartbeats.Load(),
		EventLoopTicks:        w.loopTicks.Load(),
		WatchesAdded:          int64(w.coverage.DirectoriesWatched),
	}
	// §8.2: a reason that is a warning never blocks push, and W-7 is the only one.
	// BFS-036 TIGHTENED THIS FIELD'S MEANING rather than its shape: it is the
	// server's own verdict on whether the push form is blocked here, and since the
	// wire form exists that verdict follows the CHANNEL — an established watcher
	// means the stream is served and nothing is blocked; every other state is an
	// absence that blocks it. Reporting `true` for a healthy watcher was true only
	// while the stream did not exist, and a client reading it would be reading an
	// absence that is no longer there.
	st.BlocksPush = !pushServedFrom(st)
	return st
}

// ---------------------------------------------------------------------------
// The probe: what can be learned about a target without installing anything.
// ---------------------------------------------------------------------------

type watchProbe struct {
	backendName string
	target      watchTarget
	ceilings    watchCeilings
	mountRead   bool
}

// probeWatch runs the target-level probes: the compile-time/runtime backend fact
// (W-1), the mount table (W-3, W-7) and the configured ceilings (W-2's
// configured side). Nothing here installs a watch, which is why it is safe to run
// on a build where the watcher is switched off.
func probeWatch(root string, env watchEnv) watchProbe {
	p := watchProbe{backendName: env.backendName}
	if env.readMounts != nil {
		mounts := env.readMounts()
		if len(mounts) == 0 {
			// No mount table read: the relation between the served root and its
			// writers is UNKNOWN, and unknown is never rendered as false.
			p.target = watchTarget{
				MountType:     "unknown",
				MountPoint:    "",
				BoundarySplit: triUnknown(),
			}
		} else {
			p.mountRead = true
			rec := longestMountPrefix(mounts, root)
			p.target = watchTarget{
				MountType:     rec.FSType,
				MountPoint:    rec.MountPoint,
				NetworkBacked: isNetworkFSType(rec.FSType),
				BoundarySplit: triFalse(),
			}
			if rec.FSType == "overlay" {
				p.target.BoundarySplit = triTrue()
			}
		}
	} else {
		p.target = watchTarget{MountType: "unknown", BoundarySplit: triUnknown()}
	}
	p.target.BoundaryReason = p.target.BoundarySplit.Reason()
	if env.readCeilings != nil {
		p.ceilings = env.readCeilings()
	} else {
		p.ceilings = watchCeilings{Reason: "the inotify ceiling files could not be read on this platform"}
	}
	return p
}

// longestMountPrefix is §4.3's match: the mount whose mountpoint is the longest
// prefix of the path, which is the only match that gets a submount or a bind
// right.
func longestMountPrefix(mounts []mountRecord, path string) mountRecord {
	best := mountRecord{FSType: "unknown"}
	bestLen := -1
	for _, m := range mounts {
		mp := strings.TrimRight(m.MountPoint, "/")
		if mp == "" {
			mp = "/"
		}
		if path == mp || strings.HasPrefix(path, mp+"/") || mp == "/" {
			if len(mp) > bestLen {
				best, bestLen = m, len(mp)
			}
		}
	}
	return best
}

// isNetworkFSType is §4.3's list. `fuse` alone is NOT the verdict (a fuseblk over
// a local disk is watchable); the verdict is "the local kernel is not the writer".
func isNetworkFSType(fstype string) bool {
	switch fstype {
	case "nfs", "nfs4", "cifs", "smb", "smbfs", "sshfs", "fuse.sshfs", "9p", "ceph", "glusterfs", "fuse.gvfsd-fuse":
		return true
	}
	return false
}

const (
	limitNameWatches   = "fs.inotify.max_user_watches"
	limitNameInstances = "fs.inotify.max_user_instances"
	limitNameQueued    = "fs.inotify.max_queued_events"
)

func limitsBlock(c watchCeilings) watchLimits {
	return watchLimits{
		Watching:     map[string]any{"limit_name": limitNameWatches, "configured": c.MaxUserWatches},
		Instances:    map[string]any{"limit_name": limitNameInstances, "configured": c.MaxUserInstances},
		QueuedEvents: map[string]any{"limit_name": limitNameQueued, "configured": c.MaxQueuedEvents},
	}
}

// isResourceExhaustion is the authoritative signal §4.2 classifies: the add
// itself says the kernel is out of watches. It is NOT inferred from the
// configured ceiling, which cannot see the per-user total in use.
func isResourceExhaustion(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE)
}

// errnoName renders an errno for the wire: the NAME, because the detail must
// name the observed evidence and an operator acts on the name.
func errnoName(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, syscall.ENOSPC):
		return "ENOSPC"
	case errors.Is(err, syscall.EMFILE):
		return "EMFILE"
	case errors.Is(err, syscall.ENFILE):
		return "ENFILE"
	case errors.Is(err, syscall.ENOENT):
		return "ENOENT"
	case errors.Is(err, syscall.ENOTDIR):
		return "ENOTDIR"
	case errors.Is(err, syscall.EACCES):
		return "EACCES"
	case errors.Is(err, syscall.EPERM):
		return "EPERM"
	case errors.Is(err, syscall.ENOMEM):
		return "ENOMEM"
	case errors.Is(err, syscall.EINVAL):
		return "EINVAL"
	default:
		return err.Error()
	}
}

// ---------------------------------------------------------------------------
// The Handler's side: probe caching, the wire block, the refusal.
// ---------------------------------------------------------------------------

// watchStatusSnapshot reports the running watcher, or — when this deployment has
// none — the probed absence with its own reason.
func (h *Handler) watchStatusSnapshot() watchStatus {
	if h.watch != nil {
		return h.watch.statusSnapshot()
	}
	h.primeWatchProbe()
	p := h.watchProbe
	// The requested-vs-platform pair is knowable WITHOUT a watcher (both numbers
	// are read facts), so a deployment that has not enabled the watcher still gets
	// an answer to "did my watch ceiling take effect?". The tree's own need is not
	// knowable here — no walk has run — so it is not reported (§3.3 rule 2).
	ceiling := invalidation.WatchCeiling{
		Requested: h.inv.Watch.MaxWatches,
		Platform:  p.ceilings.MaxUserWatches,
		Headroom:  int64(h.inv.Watch.InstallHeadroom),
	}
	st := watchStatus{
		State:       WatchStateAbsent,
		Backend:     p.backendName,
		Target:      p.target,
		Limits:      limitsBlock(p.ceilings),
		BlocksPush:  true,
		HeartbeatMS: h.inv.Watch.HeartbeatMS,
		Ceiling:     &ceiling,
		Unhonoured:  ceiling.RequestedExceedsPlatform(),
	}
	switch {
	case p.backendName == watchBackendNone:
		st.Reason = WatchReasonUnsupportedPlatform
		st.Detail = "no filesystem watch facility on this build/kernel (backend=none); the push form is not served and cannot be"
	case p.target.NetworkBacked:
		st.Reason = WatchReasonTargetNetbacked
		st.Detail = fmt.Sprintf("the served root %s is a %s mount (mount_point %s): the local kernel is not the writer and no local watch can observe it; this is not a build gap and will not change for this target",
			h.tree.rootPath(), p.target.MountType, p.target.MountPoint)
	default:
		// Nothing is wrong with the TARGET: this deployment has not enabled the
		// watcher. Reporting one of the seven reasons here would be reporting a
		// fact that was never probed (§3.3), so no reason is reported and the
		// explanation lives in the deployment-level detail.
		st.Detail = "no watcher is established on this target: the server-side watcher is not enabled in this deployment; the declared poll form X-Bunker-Op: events carries the channel (mode=poll), or poll with HEAD/ETag"
	}
	return st
}

// watchDocumentBlock renders §8.2's `extensions.watch` block from the RUNNING
// process. Fields that cannot be measured are null WITH a reason, never a clean
// default (§3.3 rule 2).
//
// `mode` is the mode IN FORCE, and since BFS-036 that is a per-target fact
// rather than a build constant: where a watcher is established the push form is
// served and the mode is `push`; where none is, the declared poll form carries
// the channel and the mode is `poll`. F-3 (SPEC-push-channel §9) is the rule a
// consumer must follow: read `mode`, never the op list — the op list is a
// declaration of the vocabulary, this is the declaration of the mechanism.
func (h *Handler) watchDocumentBlock(st watchStatus, w http.ResponseWriter) map[string]any {
	served := pushServedFrom(st)
	mode := "poll"
	if served {
		mode = "push"
	}
	blk := map[string]any{
		"name":                "X-Bunker-Op: watch",
		"v":                   1,
		"mode":                mode,
		"heartbeat_ms":        st.HeartbeatMS,
		"max_paths_per_event": eventsMaxPathsPerEvent,
		// BFS-062: the frame's BYTE bound, declared so a consumer can size its
		// own per-line reader to it instead of assuming a number (SPEC-push-
		// channel §7.2). Additive to this block: a peer that predates the field
		// ignores it, and the value published is the one the running surface
		// obeys (the same field the events assembly measures against).
		"max_event_bytes": h.inv.Push.MaxEventBytes,
		"modes": map[string]any{
			"push": "X-Bunker-Op: watch (NDJSON stream, SPEC-push-channel §2)",
			"poll": "X-Bunker-Op: events",
		},
		"state":       st.State,
		"blocks_push": st.BlocksPush,
		"backend":     st.Backend,
		"target":      st.Target,
		"limits":      st.Limits,
		// BFS-036: the channel's own declaration and counters (§6.3). Disjoint
		// from `counters` below by design: those count what the WATCHER lost,
		// these count what the CHANNEL dropped.
		pushCapabilityBlockKey: h.pushBlock(w, served),
		// BFS-043: the knob surface this process is serving with, read out of the
		// running watcher rather than out of the config (see invalidationConfigBlock).
		"config": h.invalidationConfigBlock(st),
	}
	if st.Reason == "" {
		blk["reason"] = nil
	} else {
		blk["reason"] = st.Reason
	}
	if st.Coverage != nil {
		blk["coverage"] = st.Coverage
	} else {
		blk["coverage"] = nil
		blk["coverage_reason"] = "no watch set has been established on this target, so no coverage has been observed"
	}
	if st.Counters != nil {
		blk["counters"] = st.Counters
	} else {
		blk["counters"] = nil
		blk["counters_reason"] = "no watcher has run on this target, so no counter has ever moved"
	}
	// The heartbeat's own evidence, so O-2 is visible rather than asserted: a
	// quiet channel is healthy only when these two are advancing.
	blk["liveness"] = map[string]any{
		"vouched":         st.Vouched,
		"stalled":         st.Stalled,
		"liveness_source": "the watcher's own heartbeat, never the absence of events",
	}
	return blk
}

// watchRefusal is the `watch` op's answer where the push form is NOT served. It
// keeps the four-field shape an old client branches on (verdict, status,
// scope=target, mode=poll) and adds `reason` as a NEW FIELD — never a new verdict
// code (§3.1, §2.3).
//
// BFS-036 CHANGED ITS PREMISE, NOT ITS SHAPE. Before this row there was a second
// branch here answering a BUILD-scope refusal for a target whose watcher WAS
// established, because the push wire form did not exist. The wire form exists
// now, so that branch is gone: where a watcher is established the op streams, and
// every refusal this function can produce is about the TARGET (no watcher, no
// coverage, watch lost, or a deployment that did not enable one). A build-scope
// refusal surviving here would be a claim that is no longer true.
func (h *Handler) watchRefusal(st watchStatus) *envelopeError {
	return &envelopeError{
		Capability: "watch", Scope: "target", Mode: "poll",
		Reason: st.Reason,
		// §4.2's configured-vs-observed pair, when the value that could not be
		// honoured is why this refusal happened.
		Unhonoured: st.Unhonoured,
		Detail:     st.Detail,
	}
}

func coverageWatched(st watchStatus) int {
	if st.Coverage == nil {
		return 0
	}
	return st.Coverage.DirectoriesWatched
}

// watchDegradations renders the watcher's entries for the document's
// `degradations[]`. Two shapes remain, and they are not interchangeable:
//
//   - an ABSENCE with a reason (blocks_push true): the same reason the refusal
//     carries, so the two carriers can never disagree;
//   - a WARNING (W-7, blocks_push false): reported, never a degradation of the
//     channel — a client that reacts by switching mechanisms changes nothing.
//
// BFS-036 REMOVED A THIRD. While the push wire form did not exist, a
// BUILD-level entry was emitted on a target whose watcher WAS established, so
// that "the op 501s" could not read as "the target has no watcher". The wire
// form is served now, so on such a target there is nothing degraded to report —
// and keeping the entry would be reporting an absence that is no longer absent.
func (h *Handler) watchDegradations(st watchStatus) []map[string]any {
	var out []map[string]any
	if !pushServedFrom(st) {
		entry := map[string]any{
			"capability": "watch", "scope": "target", "mode": "poll",
			"detail": fmt.Sprintf("inotify watcher absent on this target: the push form is not served; the declared poll form X-Bunker-Op: events is. %s", st.Detail),
		}
		if st.Reason != "" {
			entry["reason"] = st.Reason
		}
		out = append(out, entry)
	}
	if st.Unhonoured != nil && (st.State == WatchStateWatching || st.State == WatchStateOverflow) {
		// BFS-043: a configured value the platform could not give, while the
		// channel is UP. It is reported as a WARNING (blocks_push:false) for the
		// same reason W-7 is: the watcher is working, and a client that reacted by
		// switching mechanisms would have changed nothing. It is NOT a refusal —
		// the operator's number did not take effect, which is a fact about the
		// configuration rather than a degradation of the channel.
		out = append(out, map[string]any{
			"capability": "watch", "scope": "build", "mode": "push", "blocks_push": false,
			"knob": st.Unhonoured.Knob, "configured": st.Unhonoured.Configured, "observed": st.Unhonoured.Observed,
			"unhonoured": true,
			"detail":     "an invalidation value could not be honoured, and the channel is up regardless: " + st.Unhonoured.Detail,
		})
	}
	if st.Target.BoundarySplit.IsTrue() {
		// §4.8: a TOPOLOGY warning, never a channel degradation. It appears only
		// when it is true; "unknown" is a legitimate value and is not rendered as
		// true or as false.
		out = append(out, map[string]any{
			"capability": "watch", "scope": "target", "mode": "poll",
			"reason": WatchReasonBoundarySplit, "blocks_push": false,
			"detail": fmt.Sprintf("the served root %s is an overlay mount (mount_point %s): a writer whose view resolves into a container upperdir writes bytes no watch on this superblock can see, and neither can the poll — the channel is correct for the served tree and switching mechanisms cannot help",
				h.tree.rootPath(), st.Target.MountPoint),
		})
	}
	return out
}

// watchOpError is used by the `watch` case and the capability document, so the
// refusal, the document and the header can never disagree (A-3's mutation
// control depends on that).
func (h *Handler) watchOpError() *envelopeError {
	return h.watchRefusal(h.watchStatusSnapshot())
}

// sortedKeys is used only to make the reported path lists deterministic.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
