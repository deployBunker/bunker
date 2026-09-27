package webdav

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ---------------------------------------------------------------------------
// THE WATCHER'S INSTRUMENTS (BFS-035).
//
// Everything in this file drives the watcher through the seams the row built:
// an injected environment (so each probe-matrix reason is reachable on its own,
// on a host whose kernel is fine), an injected backend (so the library's channel
// STRUCTURE is reproduced without exhausting a real kernel's watches), and the
// served HTTP surface (so every assertion about a carrier is made on the wire
// and not on an internal struct).
//
// The cells are named after the acceptance criteria they carry (§11 of
// docs/prd/SPEC-watcher-capability.md); docs/evidence/BFS-035-*.md quotes the
// numbers these cells log.
// ---------------------------------------------------------------------------

var errFakeClosed = errors.New("fake watch backend is closed")

// fakeBackend is a watchBackend with fsnotify's SHAPE and none of its kernel
// state: two separate channels, the error one receiving the overflow notice
// while the event one receives nothing at all (§5.4 steps 1-3 are a property of
// the library, and a cell that could not reproduce the structure could not test
// O-1). The channels are buffered so a cell can publish while the event loop is
// parked; production's are unbuffered, which is one of the two facts O-1 exists
// for.
type fakeBackend struct {
	mu      sync.Mutex
	events  chan fsnotify.Event
	errors  chan error
	added   []string
	removed []string
	closed  bool
	// failAdd is the per-directory refusal a probe-matrix cell injects.
	failAdd func(dir string) error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		events: make(chan fsnotify.Event, 64),
		errors: make(chan error, 64),
	}
}

func (b *fakeBackend) Add(dir string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errFakeClosed
	}
	if b.failAdd != nil {
		if err := b.failAdd(dir); err != nil {
			return err
		}
	}
	b.added = append(b.added, dir)
	return nil
}

func (b *fakeBackend) Remove(dir string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removed = append(b.removed, dir)
	return nil
}

func (b *fakeBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	close(b.events)
	close(b.errors)
	return nil
}

func (b *fakeBackend) Events() <-chan fsnotify.Event { return b.events }
func (b *fakeBackend) Errors() <-chan error          { return b.errors }

func (b *fakeBackend) addedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.added)
}

func (b *fakeBackend) removedCopy() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.removed...)
}

func (b *fakeBackend) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// pushEvent publishes a change the way the kernel would, without a kernel.
func (b *fakeBackend) pushEvent(name string, op fsnotify.Op) {
	b.events <- fsnotify.Event{Name: name, Op: op}
}

// pushError publishes an overflow notice on the ERRORS channel, which is where
// the library puts it (§5.4 step 1).
func (b *fakeBackend) pushError(err error) { b.errors <- err }

// fakeWatch is the injected environment: the three probes the watcher reads from
// the host, and a backend FACTORY whose nth call is the nth install (so a
// re-install is a distinct backend, as it is in production).
type fakeWatch struct {
	ceilings   watchCeilings
	mounts     []mountRecord
	handed     []*fakeBackend
	failFrom   int // 1-based newBackend call number from which creation fails
	failErr    error
	failAdd    func(dir string) error
	mu         sync.Mutex
	calls      int
	backendCap int
}

func (f *fakeWatch) env() watchEnv {
	return watchEnv{
		backendName: watchBackendInotify,
		newBackend: func() (watchBackend, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls++
			if f.failFrom > 0 && f.calls >= f.failFrom {
				return nil, f.failErr
			}
			b := newFakeBackend()
			b.failAdd = f.failAdd
			f.handed = append(f.handed, b)
			return b, nil
		},
		readCeilings: func() watchCeilings { return f.ceilings },
		readMounts:   func() []mountRecord { return f.mounts },
	}
}

// backend returns the nth backend handed out (0 is the install).
func (f *fakeWatch) backend(n int) *fakeBackend {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.handed) {
		return nil
	}
	return f.handed[n]
}

func (f *fakeWatch) handedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.handed)
}

// callsCount is how many times the backend factory was CALLED, which counts the
// attempts (a failed re-install is an attempt and shows up here, not above).
func (f *fakeWatch) callsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// defaultFakeWatch is the healthy host: a local ext4 root and this box's own
// configured ceilings (App. A.1), so each cell below diverges in ONE fact.
func defaultFakeWatch(root string) *fakeWatch {
	return &fakeWatch{
		ceilings: watchCeilings{MaxUserWatches: 1048576, MaxUserInstances: 1024, MaxQueuedEvents: 16384},
		mounts:   []mountRecord{{MountPoint: "/", FSType: "ext4", Source: "/dev/sda1"}, {MountPoint: root, FSType: "ext4", Source: "/dev/sda1"}},
	}
}

// watchCell is one installed watcher plus the handler serving its tree.
type watchCell struct {
	h    *Handler
	root string
	fw   *fakeWatch
	opts watchOptions
	w    *watcher
}

// newWatchCell builds a git-work-tree fixture, a handler with the watcher OFF
// (what a deployment gets by default), and then installs a watcher over it with
// an injected environment. configure is called with the RESOLVED root, because
// the mount probe matches on the served path and not on the fixture's spelling.
func newWatchCell(t *testing.T, tune func(*watchOptions), configure func(root string) *fakeWatch) *watchCell {
	t.Helper()
	return newWatchCellOn(t, nil, tune, configure)
}

// newWatchCellOn is newWatchCell with a hook that shapes the served tree before
// the watcher is installed.
func newWatchCellOn(t *testing.T, mutate func(root string), tune func(*watchOptions), configure func(root string) *fakeWatch) *watchCell {
	t.Helper()
	root := fixtureTree(t)
	withGitFixture(t, root, strings.Repeat("ab", 20))
	if mutate != nil {
		mutate(root)
	}
	h := newTestHandler(t, func(c *Config) { c.Root = root })
	resolved := h.tree.rootPath()

	fw := configure
	var spec *fakeWatch
	if fw != nil {
		spec = fw(resolved)
	} else {
		spec = defaultFakeWatch(resolved)
	}

	opts := defaultWatchOptions()
	opts.heartbeat = 30 * time.Second // long by default: only a cell that tests stalling parks the loop
	opts.flushEvery = 2 * time.Millisecond
	if tune != nil {
		tune(&opts)
	}
	w := h.startWatch(spec.env(), opts)
	cell := &watchCell{h: h, root: resolved, fw: spec, opts: opts, w: w}
	t.Cleanup(func() { h.Close() })
	return cell
}

func (c *watchCell) status() watchStatus { return c.h.watchStatusSnapshot() }

func (c *watchCell) counters() watchCounters {
	st := c.status()
	if st.Counters == nil {
		return watchCounters{}
	}
	return *st.Counters
}

// park parks the event loop while the returned channel is unclosed, which is how
// a STALLED CONSUMER is reproduced without a race: the library's reader has
// nobody to hand events to, so the kernel queue fills exactly as it does in
// production.
func (c *watchCell) park() chan struct{} {
	gate := make(chan struct{})
	c.w.mu.Lock()
	c.w.gate = gate
	c.w.mu.Unlock()
	return gate
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

// refute runs for the WHOLE window and fails if cond ever becomes true: it is
// the assertion shape a negative control needs ("this never happened", not
// "this had not happened yet when I looked once").
func refute(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("%s became true, and the cell is built on it never becoming true", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// The wire: the three carriers §3.1 requires to agree.
// ---------------------------------------------------------------------------

type watchErrEnvelope struct {
	OK      bool   `json:"ok"`
	Verdict string `json:"verdict"`
	Rev     string `json:"rev"`
	Error   *struct {
		Capability string `json:"capability"`
		Scope      string `json:"scope"`
		Phase      string `json:"phase"`
		Mode       string `json:"mode"`
		Reason     string `json:"reason"`
		Detail     string `json:"detail"`
	} `json:"error"`
}

// watchDoc is §8.2's block, decoded from the capabilities op's ANSWER (not from
// the Go value that produced it), so the assertion is about the wire.
type watchDoc struct {
	Name             string  `json:"name"`
	V                int     `json:"v"`
	Mode             string  `json:"mode"`
	HeartbeatMS      int     `json:"heartbeat_ms"`
	MaxPathsPerEvent int     `json:"max_paths_per_event"`
	State            string  `json:"state"`
	Reason           *string `json:"reason"`
	BlocksPush       bool    `json:"blocks_push"`
	Backend          string  `json:"backend"`
	Target           struct {
		MountType     string          `json:"mount_type"`
		MountPoint    string          `json:"mount_point"`
		NetworkBacked bool            `json:"network_backed"`
		BoundarySplit json.RawMessage `json:"boundary_split"`
	} `json:"target"`
	Coverage *struct {
		DirectoriesDesired int      `json:"directories_desired"`
		DirectoriesWatched int      `json:"directories_watched"`
		MissingCount       int      `json:"missing_count"`
		Missing            []string `json:"missing"`
		ArchiveReason      string   `json:"missing_reason"`
		Complete           bool     `json:"complete"`
		Headroom           int      `json:"headroom"`
	} `json:"coverage"`
	Counters *struct {
		OverflowsTotal        uint64 `json:"overflows_total"`
		UnvouchedTotal        uint64 `json:"unvouched_total"`
		RescansTotal          uint64 `json:"rescans_total"`
		InstallFailuresTotal  uint64 `json:"install_failures_total"`
		BackendErrorsTotal    uint64 `json:"backend_errors_total"`
		OverflowDroppedEvents *int64 `json:"overflow_dropped_events"`
		OverflowDroppedReason string `json:"overflow_dropped_reason"`
		UnvouchedReason       string `json:"unvouched_reason"`
		LastEventAgeMS        *int64 `json:"last_event_age_ms"`
		ChangedSince          string `json:"changed_since"`
		HeartbeatsTotal       uint64 `json:"heartbeats_total"`
		EventLoopTicks        uint64 `json:"event_loop_ticks"`
		WatchesAdded          int64  `json:"watches_added"`
	} `json:"counters"`
	Liveness struct {
		Vouched        bool   `json:"vouched"`
		Stalled        bool   `json:"stalled"`
		LivenessSource string `json:"liveness_source"`
	} `json:"liveness"`
}

// watchCarriers drives the `watch` op and the `capabilities` op and returns the
// three carriers plus the raw refusal, failing if any of them disagrees about
// the reason.
func watchCarriers(t *testing.T, h *Handler) (watchErrEnvelope, watchDoc, string) {
	t.Helper()
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "watch"}, "")
	var env watchErrEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("watch op envelope is not JSON: %v (%s)", err, rec.Body.String())
	}
	if env.Error == nil {
		t.Fatalf("the watch op answered without an error object: %s", rec.Body.String())
	}
	header := rec.Header().Get("X-Bunker-Capability")
	docRaw := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "capabilities"}, "")
	var caps struct {
		Result struct {
			Capabilities struct {
				Extensions struct {
					Watch watchDoc `json:"watch"`
				} `json:"extensions"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(docRaw.Body.Bytes(), &caps); err != nil {
		t.Fatalf("capabilities envelope is not JSON: %v", err)
	}
	return env, caps.Result.Capabilities.Extensions.Watch, header
}

// headerPart extracts one `k=v` part of the X-Bunker-Capability value.
func headerPart(t *testing.T, header, key string) string {
	t.Helper()
	for _, part := range strings.Split(header, ";") {
		if strings.HasPrefix(part, key+"=") {
			return strings.TrimPrefix(part, key+"=")
		}
	}
	return ""
}

// assertReasonAgrees is A-3: `reason` reaches the envelope, the header and the
// document, and all three say the same thing.
func assertReasonAgrees(t *testing.T, env watchErrEnvelope, doc watchDoc, header, wantReason, wantScope, wantDetailFragment string) {
	t.Helper()
	if got := headerPart(t, header, "reason"); got != wantReason {
		t.Fatalf("X-Bunker-Capability carried reason=%q, want %q (header %q)", got, wantReason, header)
	}
	if got := headerPart(t, header, "scope"); got != wantScope {
		t.Fatalf("X-Bunker-Capability carried scope=%q, want %q (header %q)", got, wantScope, header)
	}
	if env.Error.Reason != wantReason {
		t.Fatalf("envelope error.reason = %q, want %q", env.Error.Reason, wantReason)
	}
	if env.Error.Scope != wantScope {
		t.Fatalf("envelope error.scope = %q, want %q", env.Error.Scope, wantScope)
	}
	if doc.Reason == nil || *doc.Reason != wantReason {
		t.Fatalf("document extensions.watch.reason = %v, want %q", doc.Reason, wantReason)
	}
	if !strings.Contains(env.Error.Detail, wantDetailFragment) {
		t.Fatalf("the detail must name the OBSERVED evidence; it does not contain %q: %s", wantDetailFragment, env.Error.Detail)
	}
	// §3.3's honesty rule: a detail that could have been written without probing
	// anything is a defect, and a one-word detail is one.
	if len(env.Error.Detail) < 40 {
		t.Fatalf("the detail is not a usable sentence about the evidence: %q", env.Error.Detail)
	}
}

// ---------------------------------------------------------------------------
// A-1/A-7/A-8/A-9/A-12: THE PROBE MATRIX, one named reason per case.
// ---------------------------------------------------------------------------

func TestWatchProbeMatrixNamesOneReasonPerCase(t *testing.T) {
	t.Run("control: local ext4 root is watched, and reports no reason", func(t *testing.T) {
		// A-9's control arm and A-12's baseline: with a healthy target the
		// watcher IS established, so the document says so and carries no reason.
		cell := newWatchCell(t, nil, nil)
		waitFor(t, 5*time.Second, "the watcher to be established", func() bool {
			return cell.status().State == WatchStateWatching
		})
		st := cell.status()
		if !st.Vouched {
			t.Fatalf("an established watcher reported vouched=false")
		}
		if st.Reason != "" {
			t.Fatalf("a healthy target reported reason %q: a reason that was never probed is fabricated (§3.3)", st.Reason)
		}
		if st.Target.NetworkBacked {
			t.Fatalf("a local ext4 fixture reported network_backed: mount_type=%q mount_point=%q", st.Target.MountType, st.Target.MountPoint)
		}
		if st.Target.MountType != "ext4" {
			t.Fatalf("mount_type = %q, want the fixture's real fstype", st.Target.MountType)
		}
		if st.Coverage == nil || !st.Coverage.Complete || st.Coverage.DirectoriesWatched != st.Coverage.DirectoriesDesired {
			t.Fatalf("a successful install must report directories_watched == directories_desired: %+v", st.Coverage)
		}
		if st.Coverage.Headroom != watchInstallHeadroom {
			t.Fatalf("coverage.headroom = %d, want the reported non-zero default %d", st.Coverage.Headroom, watchInstallHeadroom)
		}
		// The op still refuses (the push WIRE form is BFS-036) but it must not
		// deny the watcher it has: scope=build, not a target reason.
		env, doc, header := watchCarriers(t, cell.h)
		if env.Error.Scope != "build" {
			t.Fatalf("with a watcher established the refusal scope must be `build` (the push form), got %q: %s", env.Error.Scope, env.Error.Detail)
		}
		if env.Error.Reason != "" {
			t.Fatalf("the build-scope refusal must carry no target reason, got %q", env.Error.Reason)
		}
		if doc.State != WatchStateWatching {
			t.Fatalf("document state = %q, want watching", doc.State)
		}
		if doc.Mode != "poll" {
			t.Fatalf("document mode = %q, want poll", doc.Mode)
		}
		if got := headerPart(t, header, "scope"); got != "build" {
			t.Fatalf("header scope = %q, want build", got)
		}
		if !strings.Contains(env.Error.Detail, "the watcher IS established") {
			t.Fatalf("the build refusal must say the watcher exists, else `the op 501s` reads as `no watcher`: %s", env.Error.Detail)
		}
	})

	t.Run("W-1 watch_unsupported_platform", func(t *testing.T) {
		cell := newWatchCell(t, nil, func(root string) *fakeWatch {
			fw := defaultFakeWatch(root)
			return fw
		})
		// The build fact is injected the way a non-Linux build carries it.
		env := cell.fw.env()
		env.backendName = watchBackendNone
		env.newBackend = nil
		cell.h.startWatch(env, cell.opts)

		st := cell.status()
		if st.State != WatchStateAbsent || st.Reason != WatchReasonUnsupportedPlatform {
			t.Fatalf("state/reason = %q/%q, want absent/watch_unsupported_platform", st.State, st.Reason)
		}
		if st.Backend != watchBackendNone {
			t.Fatalf("backend = %q, want none", st.Backend)
		}
		envl, doc, header := watchCarriers(t, cell.h)
		assertReasonAgrees(t, envl, doc, header, WatchReasonUnsupportedPlatform, "target", "no filesystem watch facility")
		if !doc.BlocksPush {
			t.Fatalf("W-1 must block push")
		}
		if doc.Backend != watchBackendNone {
			t.Fatalf("document backend = %q, want none", doc.Backend)
		}
		if doc.Mode != "poll" {
			t.Fatalf("W-1 must degrade to the poll form, mode = %q", doc.Mode)
		}
		if !strings.Contains(envl.Error.Detail, "backend=none") {
			t.Fatalf("W-1's detail must name the backend fact: %s", envl.Error.Detail)
		}
	})

	t.Run("W-3 watch_target_netbacked is its own case, with the mount type", func(t *testing.T) {
		cell := newWatchCell(t, nil, func(root string) *fakeWatch {
			fw := defaultFakeWatch(root)
			// The served root is a network mount. It is injected as a mount
			// RECORD, which is exactly what the probe reads on a real host.
			fw.mounts = []mountRecord{
				{MountPoint: "/", FSType: "ext4", Source: "/dev/sda1"},
				{MountPoint: root, FSType: "fuse.sshfs", Source: "kara@remote:/srv/tree"},
			}
			return fw
		})
		st := cell.status()
		if st.Reason != WatchReasonTargetNetbacked {
			t.Fatalf("reason = %q, want watch_target_netbacked (a netbacked target is its own case, never `no watcher`)", st.Reason)
		}
		if !st.Target.NetworkBacked || st.Target.MountType != "fuse.sshfs" {
			t.Fatalf("target = %+v, want network_backed true and mount_type fuse.sshfs", st.Target)
		}
		if st.Target.MountPoint != cell.root {
			t.Fatalf("mount_point = %q, want the longest-prefix match %q", st.Target.MountPoint, cell.root)
		}
		if cell.fw.handedCount() != 0 {
			t.Fatalf("a netbacked target must not attempt an install (nothing was probed about the kernel): %d backends created", cell.fw.handedCount())
		}
		envl, doc, header := watchCarriers(t, cell.h)
		assertReasonAgrees(t, envl, doc, header, WatchReasonTargetNetbacked, "target", "fuse.sshfs")
		if !strings.Contains(envl.Error.Detail, "is not a build gap") {
			t.Fatalf("W-3 must say the absence is permanent for this target, else an operator hunts a build flag: %s", envl.Error.Detail)
		}
	})

	t.Run("W-2 watch_limit_exhausted reports configured vs desired", func(t *testing.T) {
		cell := newWatchCell(t, nil, func(root string) *fakeWatch {
			fw := defaultFakeWatch(root)
			fw.ceilings = watchCeilings{MaxUserWatches: 3, MaxUserInstances: 1024, MaxQueuedEvents: 16384}
			return fw
		})
		st := cell.status()
		if st.Reason != WatchReasonLimitExhausted {
			t.Fatalf("reason = %q, want watch_limit_exhausted", st.Reason)
		}
		// §4.2's configured-vs-observed block: the numbers must be there.
		watching := st.Limits.Watching
		if watching == nil {
			t.Fatalf("limits.watching is not reported: %#v", st.Limits)
		}
		if got := watching["configured"]; got != int64(3) {
			t.Fatalf("limits.watching.configured = %v, want the injected ceiling 3", got)
		}
		desired, ok := watching["desired"].(int)
		if !ok || desired < 4 {
			t.Fatalf("limits.watching.desired = %v, want the fixture's own directory count", watching["desired"])
		}
		if got := watching["headroom"]; got != watchInstallHeadroom {
			t.Fatalf("limits.watching.headroom = %v, want %d", got, watchInstallHeadroom)
		}
		if got := watching["limit_name"]; got != limitNameWatches {
			t.Fatalf("limit_name = %v, want %q", got, limitNameWatches)
		}
		if cell.fw.handedCount() != 0 {
			t.Fatalf("the pre-flight refusal must not have created a backend")
		}
		envl, doc, header := watchCarriers(t, cell.h)
		assertReasonAgrees(t, envl, doc, header, WatchReasonLimitExhausted, "target", limitNameWatches)
		if !strings.Contains(envl.Error.Detail, fmt.Sprintf("%d", desired)) {
			t.Fatalf("the detail must carry the desired count %d: %s", desired, envl.Error.Detail)
		}
	})

	t.Run("W-2 from the add's errno: the partial set is TORN DOWN", func(t *testing.T) {
		// §4.2(f): an ENOSPC from the add is the authoritative signal, and the
		// partial watch set is never kept as a success.
		cell := newWatchCell(t, nil, func(root string) *fakeWatch {
			fw := defaultFakeWatch(root)
			fw.failAdd = func(dir string) error {
				if filepath.Base(dir) == "empty" {
					return syscall.ENOSPC
				}
				return nil
			}
			return fw
		})
		st := cell.status()
		if st.Reason != WatchReasonLimitExhausted {
			t.Fatalf("reason = %q, want watch_limit_exhausted", st.Reason)
		}
		watching := st.Limits.Watching
		if got := watching["errno"]; got != "ENOSPC" {
			t.Fatalf("errno = %v, want ENOSPC", got)
		}
		added, _ := watching["watches_held_by_this_process"].(int)
		if added < 1 {
			t.Fatalf("watches_held_by_this_process = %v: `we added N of M and then stopped` must be visible as numbers", watching["watches_held_by_this_process"])
		}
		b := cell.fw.backend(0)
		if b == nil {
			t.Fatalf("no backend was created")
		}
		if !b.isClosed() {
			t.Fatalf("the partial watch set was KEPT: the backend is still open (§4.2(f) forbids it)")
		}
		if got := len(b.removedCopy()); got != added {
			t.Fatalf("%d watches were added and %d were removed: a torn-down set must drop every one", added, got)
		}
		envl, doc, header := watchCarriers(t, cell.h)
		assertReasonAgrees(t, envl, doc, header, WatchReasonLimitExhausted, "target", "ENOSPC")
		if !strings.Contains(envl.Error.Detail, "torn down") {
			t.Fatalf("the detail must say what happened to the partial set: %s", envl.Error.Detail)
		}
	})

	t.Run("W-2 instance ceiling from newBackend's errno", func(t *testing.T) {
		cell := newWatchCell(t, nil, func(root string) *fakeWatch {
			fw := defaultFakeWatch(root)
			fw.failFrom = 1
			fw.failErr = syscall.ENOSPC
			return fw
		})
		st := cell.status()
		if st.Reason != WatchReasonLimitExhausted {
			t.Fatalf("reason = %q, want watch_limit_exhausted (a watcher could not be created)", st.Reason)
		}
		instances := st.Limits.Instances
		if got := instances["configured"]; got != int64(1024) {
			t.Fatalf("limits.instances.configured = %v, want 1024", got)
		}
		envl, doc, header := watchCarriers(t, cell.h)
		assertReasonAgrees(t, envl, doc, header, WatchReasonLimitExhausted, "target", limitNameInstances)
	})

	t.Run("W-5 watch_install_failed names the errno and the path", func(t *testing.T) {
		cell := newWatchCell(t, nil, func(root string) *fakeWatch {
			fw := defaultFakeWatch(root)
			fw.failAdd = func(dir string) error {
				if filepath.Base(dir) == "empty" {
					return syscall.ENOENT
				}
				return nil
			}
			return fw
		})
		st := cell.status()
		if st.Reason != WatchReasonInstallFailed {
			t.Fatalf("reason = %q, want watch_install_failed (ENOENT is not the kernel being out of watches)", st.Reason)
		}
		if b := cell.fw.backend(0); b == nil || !b.isClosed() {
			t.Fatalf("the incomplete watch set was kept: %+v", b)
		}
		envl, doc, header := watchCarriers(t, cell.h)
		assertReasonAgrees(t, envl, doc, header, WatchReasonInstallFailed, "target", "ENOENT")
		if !strings.Contains(envl.Error.Detail, "empty") {
			t.Fatalf("W-5's detail must name the path the add failed on: %s", envl.Error.Detail)
		}
	})

	t.Run("W-4 watch_partial_coverage keeps the set and counts the missing", func(t *testing.T) {
		cell := newWatchCell(t, nil, func(root string) *fakeWatch {
			fw := defaultFakeWatch(root)
			// EACCES is the ONE add failure that is recorded rather than a
			// reason to tear down: it is not installable as this uid.
			fw.failAdd = func(dir string) error {
				if filepath.Base(dir) == "empty" {
					return fmt.Errorf("add %s: %w", dir, os.ErrPermission)
				}
				return nil
			}
			return fw
		})
		st := cell.status()
		if st.Reason != WatchReasonPartialCoverage {
			t.Fatalf("reason = %q, want watch_partial_coverage (the one case that LOOKS healthy)", st.Reason)
		}
		if st.Coverage == nil {
			t.Fatalf("no coverage block was reported")
		}
		if st.Coverage.MissingCount != 1 || len(st.Coverage.Missing) != 1 {
			t.Fatalf("coverage = %+v, want exactly one missing directory", st.Coverage)
		}
		if !strings.Contains(st.Coverage.Missing[0], "empty") {
			t.Fatalf("missing[] = %v, want the refused directory", st.Coverage.Missing)
		}
		if st.Coverage.Complete {
			t.Fatalf("complete=true while a directory is unwatched: this is the invisible wrong answer W-4 exists to prevent")
		}
		if b := cell.fw.backend(0); b == nil || b.isClosed() {
			t.Fatalf("W-4 must KEEP the watch set (a forward CREATE can heal it)")
		}
		envl, doc, header := watchCarriers(t, cell.h)
		assertReasonAgrees(t, envl, doc, header, WatchReasonPartialCoverage, "target", "1 of")
		if doc.Coverage == nil || doc.Coverage.MissingCount != 1 {
			t.Fatalf("the document's coverage block disagrees with the refusal: %+v", doc.Coverage)
		}
	})

	t.Run("W-4 bound: more missing than the cap is an EMPTY list and a full count", func(t *testing.T) {
		// A-8: never a truncated list presented as complete. The cap is
		// eventsMaxPathsPerEvent (4096), so this arm needs a tree with more
		// directories than that and a refusal for every one.
		dirs := eventsMaxPathsPerEvent + 4
		cell := newWatchCellOn(t, func(root string) {
			for i := 0; i < dirs; i++ {
				if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("d%05d", i)), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			}
		}, nil, func(root string) *fakeWatch {
			fw := defaultFakeWatch(root)
			fw.failAdd = func(dir string) error {
				return fmt.Errorf("add %s: %w", dir, os.ErrPermission)
			}
			return fw
		})
		st := cell.status()
		if st.Reason != WatchReasonPartialCoverage {
			t.Fatalf("reason = %q, want watch_partial_coverage", st.Reason)
		}
		if st.Coverage == nil || st.Coverage.MissingCount < dirs {
			t.Fatalf("missing_count = %+v, want at least %d", st.Coverage, dirs)
		}
		if len(st.Coverage.Missing) != 0 {
			t.Fatalf("missing[] carried %d paths while %d are missing: above the cap the list is EMPTY and the count carries the number",
				len(st.Coverage.Missing), st.Coverage.MissingCount)
		}
		if st.Coverage.Complete {
			t.Fatalf("complete=true with %d unwatched directories", st.Coverage.MissingCount)
		}
		// The control for the arm above: a list UNDER the cap is verbatim, so the
		// empty list is the cap's rule and not this function's habit.
		short := cappedMissing([]string{"a", "b", "c"})
		if len(short) != 3 {
			t.Fatalf("cappedMissing returned %v for a short list: the cap must not empty it", short)
		}
		if got := cappedMissing(make([]string, eventsMaxPathsPerEvent+1)); len(got) != 0 {
			t.Fatalf("cappedMissing returned %d paths above the cap", len(got))
		}
	})

	t.Run("W-6 watch_lost when the re-install also fails", func(t *testing.T) {
		cell := newWatchCell(t, nil, func(root string) *fakeWatch {
			fw := defaultFakeWatch(root)
			fw.failFrom = 2 // the install works; the re-install does not
			fw.failErr = syscall.ENOSPC
			return fw
		})
		waitFor(t, 5*time.Second, "the watcher to be established", func() bool {
			return cell.status().State == WatchStateWatching
		})
		// The library's reader stops: the events channel closes mid-life.
		if err := cell.fw.backend(0).Close(); err != nil {
			t.Fatalf("close the first backend: %v", err)
		}
		waitFor(t, 5*time.Second, "the lost state", func() bool {
			return cell.status().State == WatchStateLost
		})
		st := cell.status()
		if st.Reason != WatchReasonLost {
			t.Fatalf("reason = %q, want watch_lost", st.Reason)
		}
		if st.Vouched {
			t.Fatalf("a lost watcher reported vouched=true")
		}
		if !strings.Contains(st.Detail, "could not be re-established") {
			t.Fatalf("W-6's detail must name the failed re-install AND the instant: %s", st.Detail)
		}
		if cell.fw.callsCount() != 2 {
			t.Fatalf("%d backends were created: a re-install must have been ATTEMPTED", cell.fw.callsCount())
		}
		envl, doc, header := watchCarriers(t, cell.h)
		assertReasonAgrees(t, envl, doc, header, WatchReasonLost, "target", "could not be re-established")
		if doc.State != WatchStateLost {
			t.Fatalf("document state = %q, want lost", doc.State)
		}
	})

	t.Run("W-6 transient: a successful re-install is NOT an absence", func(t *testing.T) {
		// §4.6: only (a) stop AND (b) failed re-install is `lost`. (a) alone is a
		// fault to repair, and the interval between is unvouched.
		cell := newWatchCell(t, nil, nil)
		waitFor(t, 5*time.Second, "the watcher to be established", func() bool {
			return cell.status().State == WatchStateWatching
		})
		before := cell.counters().UnvouchedTotal
		if err := cell.fw.backend(0).Close(); err != nil {
			t.Fatalf("close the first backend: %v", err)
		}
		waitFor(t, 5*time.Second, "the re-install", func() bool {
			return cell.fw.handedCount() == 2
		})
		waitFor(t, 5*time.Second, "the watcher to be serving again", func() bool {
			st := cell.status()
			return st.State == WatchStateWatching && st.Vouched
		})
		if got := cell.counters().UnvouchedTotal; got <= before {
			// The GAP itself must be published: the interval between the stop and
			// the re-install is unvouched, and saying so is what stops it reading
			// as quiet.
			t.Fatalf("unvouched_total did not move across a stop-and-reinstall (%d -> %d)", before, got)
		}
		// And the NEW backend is the one being read: an event published on it must
		// be processed (the loop must not have exited with the old channel).
		rev0 := cell.h.tree.revToken()
		abs := filepath.Join(cell.root, "src", "main.go")
		mustWrite(t, abs, "package main\n// after the re-install\n")
		cell.fw.backend(1).pushEvent(abs, fsnotify.Write)
		waitFor(t, 5*time.Second, "the event from the re-installed backend to move the revision", func() bool {
			return cell.h.tree.revToken() != rev0
		})
	})

	t.Run("W-7 boundary split is a WARNING and blocks nothing", func(t *testing.T) {
		cell := newWatchCell(t, nil, func(root string) *fakeWatch {
			fw := defaultFakeWatch(root)
			fw.mounts = []mountRecord{
				{MountPoint: "/", FSType: "ext4", Source: "/dev/sda1"},
				{MountPoint: root, FSType: "overlay", Source: "overlay"},
			}
			return fw
		})
		st := cell.status()
		if !st.Target.BoundarySplit.IsTrue() {
			t.Fatalf("an overlay root must report boundary_split=true, got %q", st.Target.BoundarySplit.v)
		}
		if st.Reason != "" {
			t.Fatalf("W-7 is NOT a refusal reason (§4.7/§4.8): got %q", st.Reason)
		}
		if !st.Vouched {
			t.Fatalf("an overlay root is watchable: the watcher must be established")
		}
		degs := cell.h.watchDegradations(st)
		var sawWarning bool
		for _, d := range degs {
			if d["reason"] == WatchReasonBoundarySplit {
				sawWarning = true
				if bp, ok := d["blocks_push"].(bool); !ok || bp {
					t.Fatalf("the W-7 entry must carry blocks_push=false: %#v", d)
				}
				if !strings.Contains(fmt.Sprint(d["detail"]), "upperdir") {
					t.Fatalf("W-7's detail must state the consequence, not the category: %v", d["detail"])
				}
			}
		}
		if !sawWarning {
			t.Fatalf("no degradations[] entry carried reason=%s: %#v", WatchReasonBoundarySplit, degs)
		}
	})

	t.Run("A-1 control: a build that ran no probe reports NO reason", func(t *testing.T) {
		// The default deployment: no watcher. The document must say `absent`
		// without inventing one of the seven.
		root := fixtureTree(t)
		h := newTestHandler(t, func(c *Config) { c.Root = root })
		_, doc, header := watchCarriers(t, h)
		if doc.State != WatchStateAbsent {
			t.Fatalf("state = %q, want absent", doc.State)
		}
		if doc.Reason != nil {
			t.Fatalf("a deployment with no watcher reported reason=%q: there is no default reason (§3.3)", *doc.Reason)
		}
		if got := headerPart(t, header, "reason"); got != "" {
			t.Fatalf("the header carried reason=%q for a probe that never ran", got)
		}
		if !strings.Contains(doc.Name, "watch") {
			t.Fatalf("the block is not the watch block: %q", doc.Name)
		}
		if doc.MaxPathsPerEvent != eventsMaxPathsPerEvent {
			t.Fatalf("max_paths_per_event = %d, want the declared %d", doc.MaxPathsPerEvent, eventsMaxPathsPerEvent)
		}
		if doc.HeartbeatMS != int(watchHeartbeatPeriod/time.Millisecond) {
			t.Fatalf("heartbeat_ms = %d, want the declared %d", doc.HeartbeatMS, int(watchHeartbeatPeriod/time.Millisecond))
		}
		if doc.Coverage != nil || doc.Counters != nil {
			t.Fatalf("no watcher ran, so coverage/counters must be null WITH a reason: %+v", doc)
		}
	})
}

// ---------------------------------------------------------------------------
// A-4: THE OVERFLOW GUARANTEE, and the control that makes it able to fail.
// ---------------------------------------------------------------------------

func TestWatchOverflowForcesCountedFullRescanAndIsNeverQuiet(t *testing.T) {
	gate := make(chan struct{})
	cell := newWatchCell(t, func(o *watchOptions) {
		// A long heartbeat on purpose: this cell is about the KERNEL's overflow,
		// and the stall detector must not be able to contribute a second
		// unvouched interval to the counters being asserted.
		o.heartbeat = 30 * time.Second
		o.loopGate = gate
	}, nil)

	waitFor(t, 5*time.Second, "the watcher to be established", func() bool {
		return cell.status().State == WatchStateWatching
	})
	// Prime the ledger with one observation, so the rescan's diff is about the
	// change this cell makes and not about its own empty baseline.
	if _, err := cell.h.tree.pollEvents(resumePoint{}); err != nil {
		t.Fatalf("prime the ledger: %v", err)
	}
	rev0 := cell.h.tree.revToken()

	// A change made while the loop is parked and NOT reported on the events
	// channel: this is the dropped event the rescan has to find.
	oob := filepath.Join(cell.root, "src", "out-of-band.txt")
	mustWrite(t, oob, "written while the kernel queue was dropping\n")

	// §5.4 step 1: the overflow notice arrives on the ERRORS channel.
	cell.fw.backend(0).pushError(fsnotify.ErrEventOverflow)

	// The notice must be RECEIVED even though the event loop is parked: that is
	// O-1, and it is why the drain is a goroutine of its own.
	waitFor(t, 5*time.Second, "the overflow to be counted", func() bool {
		return cell.counters().OverflowsTotal == 1
	})

	// §5.1 claim 1, the whole point of the row: the interval is UNVOUCHED, and
	// nothing about it may read as quiet.
	st := cell.status()
	if st.State != WatchStateOverflow {
		t.Fatalf("state after an overflow = %q, want overflow (the watcher exists; its knowledge does not)", st.State)
	}
	if st.Vouched {
		t.Fatalf("an unvouched interval was reported as vouched")
	}
	if st.Counters.UnvouchedTotal != 1 {
		t.Fatalf("unvouched_total = %d, want 1", st.Counters.UnvouchedTotal)
	}
	if st.Counters.UnvouchedReason != watchCauseKernelQueue {
		t.Fatalf("unvouched_reason = %q, want %q (the distinction `the kernel ate events` vs `our reader stopped` is the reason the causes are named separately)",
			st.Counters.UnvouchedReason, watchCauseKernelQueue)
	}
	// §5: the number of lost events is never invented.
	if st.Counters.OverflowDroppedEvents != nil {
		t.Fatalf("overflow_dropped_events = %d: the kernel reports ONE marker, not a count", *st.Counters.OverflowDroppedEvents)
	}
	if st.Counters.OverflowDroppedReason != watchDroppedEventsReason {
		t.Fatalf("a null count must carry its reason, got %q", st.Counters.OverflowDroppedReason)
	}
	// The rescan has not happened yet (its only executor is the parked loop), so
	// the cell can assert the interval's state BEFORE it is repaired.
	if st.Counters.RescansTotal != 0 {
		t.Fatalf("rescans_total = %d before the loop was released: the rescan must be the loop's work, not the drain's (O-1 forbids slow work on that receive)", st.Counters.RescansTotal)
	}

	// And the poll's own view of the same interval: the tree's revision must NOT
	// have been vouched by the watcher yet.
	if got := cell.h.tree.revToken(); got != rev0 {
		t.Fatalf("the revision moved for an unvouched interval (%q -> %q): R-V1 aligns it with a VOUCHED change", rev0, got)
	}

	// §5.1 claim 2: release the loop and the overflow is ALWAYS a full rescan.
	close(gate)
	waitFor(t, 5*time.Second, "the forced full rescan", func() bool {
		return cell.counters().RescansTotal == 1
	})

	// A full rescan means the whole-tree observation and an EMPTY path list:
	// never a diff of the part that fit, and never a partial one.
	line := lastLedgerEventOfKind(t, cell.h, eventOverflow)
	if line == nil {
		t.Fatalf("the rescan published no `overflow` line: the interval it could not vouch for was never announced")
	}
	if len(line.Paths) != 0 {
		t.Fatalf("the rescan published %d paths: an overflow is never a partial list (%v)", len(line.Paths), line.Paths)
	}
	// ... and it really re-observed the tree: the change nobody reported on the
	// channel is now aliged with, which is what makes `full rescan` more than a
	// counter increment.
	if got := cell.h.tree.revToken(); got == rev0 {
		t.Fatalf("the revision did not move after a full rescan that saw %s: the rescan counted but did not observe", oob)
	}
	if line.Rev != cell.h.tree.revToken() {
		t.Fatalf("the overflow line carries rev %q but the tree now serves %q: the two readers of the same tree disagree", line.Rev, cell.h.tree.revToken())
	}

	// The interval is vouched for again only because the rescan is the evidence.
	waitFor(t, 5*time.Second, "the state to return to watching", func() bool {
		return cell.status().State == WatchStateWatching
	})
	if got := cell.counters().UnvouchedTotal; got != 1 {
		t.Fatalf("unvouched_total = %d after one overflow: the counter must count INTERVALS, not reads", got)
	}
}

// TestWatchOverflowNegativeControlDisablesTheDrain is A-5: the same overflow,
// with the O-1 drain disabled, must NOT be observed at all. A cell that cannot
// fail proves nothing, so this one asserts the FAILURE of the compliant arm's
// premise — a watcher that reads only Events is silent on overflow.
func TestWatchOverflowNegativeControlDisablesTheDrain(t *testing.T) {
	cell := newWatchCell(t, func(o *watchOptions) {
		o.disableDrain = true
		o.heartbeat = 30 * time.Second
	}, nil)
	waitFor(t, 5*time.Second, "the watcher to be established (the drain is disabled, not the install)", func() bool {
		return cell.status().State == WatchStateWatching
	})
	cell.fw.backend(0).pushError(fsnotify.ErrEventOverflow)
	// The whole demonstration: with nobody reading Errors, the overflow is
	// invisible. Counters stay flat, the state stays `watching`, and the answer
	// to every question is the same as a quiet tree's.
	refute(t, 500*time.Millisecond, "the disabled drain observed the overflow", func() bool {
		return cell.counters().OverflowsTotal != 0
	})
	st := cell.status()
	if st.State != WatchStateWatching {
		t.Fatalf("state = %q, want watching (this arm is the SILENT one)", st.State)
	}
	if !st.Vouched {
		t.Fatalf("the silent arm reported unvouched: the control has drifted from the behaviour it exists to reproduce")
	}
	if st.Counters.UnvouchedTotal != 0 || st.Counters.RescansTotal != 0 {
		t.Fatalf("the silent arm moved a counter: %+v", st.Counters)
	}
}

// TestWatchOverflowRealKernelReproducer drives §5.4's measured trap through the
// REAL library: the kernel's queue is overflowed while the consumer is parked,
// and the notice travels the Errors channel. The drain arm must see it; the
// no-drain arm must get silence while the process looks perfectly alive.
func TestWatchOverflowRealKernelReproducer(t *testing.T) {
	if testing.Short() {
		t.Skip("the kernel reproducer creates ~15k files; it is skipped under -short")
	}
	ceilings := readInotifyCeilings()
	if ceilings.MaxQueuedEvents <= 0 {
		t.Skipf("the host does not expose fs.inotify.max_queued_events (%s)", ceilings.Reason)
	}
	// The spec's reproducer (App. A.4): 12 000 creations overflowed a 16 384
	// queue. Sized from the HOST's configured ceiling rather than hard-coded, so
	// the arm says the same thing on a tuned box.
	flood := int(ceilings.MaxQueuedEvents) // ~4 events per create, so this clears the queue with margin

	run := func(t *testing.T, disableDrain bool) (*watcher, chan struct{}) {
		t.Helper()
		root := t.TempDir()
		watched := filepath.Join(root, "hot")
		if err := os.MkdirAll(watched, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		h := newTestHandler(t, func(c *Config) { c.Root = root })
		opts := defaultWatchOptions()
		opts.flushEvery = 5 * time.Millisecond
		opts.disableDrain = disableDrain
		w := h.startWatch(defaultWatchEnv(), opts)
		t.Cleanup(func() { h.Close() })
		if w.statusSnapshot().State != WatchStateWatching {
			t.Skipf("the real backend could not be established on this host: %s", w.statusSnapshot().Detail)
		}
		// Park the loop long enough for the kernel queue to FILL: the consumer
		// is stalled, the library's reader has nobody to hand events to, and the
		// queue is the only place the flood can go.
		gate := make(chan struct{})
		w.mu.Lock()
		w.gate = gate
		w.mu.Unlock()
		for i := 0; i < flood; i++ {
			if err := os.WriteFile(filepath.Join(watched, fmt.Sprintf("f%06d", i)), []byte("x"), 0o644); err != nil {
				t.Fatalf("flood file %d: %v", i, err)
			}
		}
		return w, gate
	}

	t.Run("the drain arm observes the kernel's overflow as the backlog drains", func(t *testing.T) {
		w, gate := run(t, false)
		// The overflow record is queued BEHIND the events it replaces, so it is
		// read only once the queue drains — and reading it is the receive that
		// blocks unless someone is draining Errors. That is the trap, and this is
		// the arm where somebody is.
		close(gate)
		waitFor(t, 60*time.Second, "IN_Q_OVERFLOW to be received by the drain", func() bool {
			st := w.statusSnapshot()
			return st.Counters != nil && st.Counters.OverflowsTotal > 0
		})
		st := w.statusSnapshot()
		if st.Counters.OverflowDroppedEvents != nil {
			t.Fatalf("the kernel's drop count was invented: %d", *st.Counters.OverflowDroppedEvents)
		}
		if st.Counters.UnvouchedReason != watchCauseKernelQueue {
			t.Fatalf("unvouched_reason = %q, want %q", st.Counters.UnvouchedReason, watchCauseKernelQueue)
		}
		t.Logf("BFS-035 kernel reproducer: max_queued_events=%d, %d files created, overflows_total=%d, unvouched_total=%d, rescans_total=%d",
			ceilings.MaxQueuedEvents, flood, st.Counters.OverflowsTotal, st.Counters.UnvouchedTotal, st.Counters.RescansTotal)
	})

	t.Run("the no-drain arm gets SILENCE (the trap R-6 names)", func(t *testing.T) {
		w, gate := run(t, true)
		close(gate)
		// Same queue, same overflow record, no drain: the receive that would
		// unblock the library has nobody on the other end, so the reader stops
		// there and the watcher reports a healthy, quiet channel.
		refute(t, 15*time.Second, "the no-drain arm observed the kernel's overflow", func() bool {
			st := w.statusSnapshot()
			return st.Counters != nil && st.Counters.OverflowsTotal > 0
		})
		if st := w.statusSnapshot(); st.State != WatchStateWatching {
			t.Fatalf("state = %q: the silent arm must still look perfectly healthy", st.State)
		}
	})
}

// ---------------------------------------------------------------------------
// O-2 / A-6: liveness is the watcher's own heartbeat, never the absence of
// events.
// ---------------------------------------------------------------------------

func TestWatchLivenessIsTheHeartbeatNeverSilence(t *testing.T) {
	cell := newWatchCell(t, func(o *watchOptions) {
		o.heartbeat = 20 * time.Millisecond
		o.flushEvery = 2 * time.Millisecond
		o.stallBeats = 2
	}, nil)
	waitFor(t, 5*time.Second, "the watcher to be established", func() bool {
		return cell.status().State == WatchStateWatching
	})

	// A healthy IDLE tree: no events at all, and yet the watcher is alive and
	// says so from its OWN clock.
	before := cell.counters()
	waitFor(t, 5*time.Second, "three of the watcher's own heartbeats", func() bool {
		return cell.counters().HeartbeatsTotal >= before.HeartbeatsTotal+3
	})
	if got := cell.counters().EventLoopTicks; got <= before.EventLoopTicks {
		t.Fatalf("event_loop_ticks did not move on an idle tree: an idle tree and a stopped loop must not look alike (O-2)")
	}
	if st := cell.status(); st.Counters.LastEventAgeMS != nil {
		t.Fatalf("last_event_age_ms = %d on a tree with no events: liveness was inferred from events", *st.Counters.LastEventAgeMS)
	}
	// And the quiet channel is NOT the evidence: the document names the source.
	_, doc, _ := watchCarriers(t, cell.h)
	if doc.Liveness.LivenessSource == "" {
		t.Fatalf("the document does not name its liveness source")
	}
	if strings.Contains(strings.ToLower(doc.Liveness.LivenessSource), "absence") == false {
		t.Fatalf("liveness_source must be explicit that the absence of events is not the source: %q", doc.Liveness.LivenessSource)
	}
	if doc.Liveness.Stalled {
		t.Fatalf("a healthy idle tree was reported as stalled")
	}

	// Now genuinely stall the loop. Two consecutive beats with a parked loop is
	// a stopped reader, and a stopped reader must NOT be reported as watching.
	unvouchedBefore := cell.counters().UnvouchedTotal
	gate := cell.park()
	waitFor(t, 5*time.Second, "the stall detector to fire", func() bool {
		return cell.status().Stalled
	})
	st := cell.status()
	if st.State == WatchStateWatching {
		t.Fatalf("a stalled reader was reported as watching: a quiet channel was called healthy")
	}
	if st.Vouched {
		t.Fatalf("a stalled interval was reported as vouched")
	}
	if st.Counters.UnvouchedReason != watchCauseReaderStalled {
		t.Fatalf("unvouched_reason = %q, want %q (not the kernel's fault)", st.Counters.UnvouchedReason, watchCauseReaderStalled)
	}

	// The stall is a FAULT TO REPAIR, not a permanent absence: releasing the loop
	// must clear it through the rescan that the stall itself requested.
	close(gate)
	waitFor(t, 5*time.Second, "the stall to be repaired", func() bool {
		s := cell.status()
		return !s.Stalled && s.State == WatchStateWatching && s.Vouched
	})
	if got := cell.counters().UnvouchedTotal; got <= unvouchedBefore {
		t.Fatalf("unvouched_total did not move across the stall (%d -> %d)", unvouchedBefore, got)
	}
	if got := cell.counters().RescansTotal; got == 0 {
		t.Fatalf("the stalled interval was never rescanned: the fault was reported and then left open")
	}
}

// ---------------------------------------------------------------------------
// A-10 / D1-D6 / R-V1..R-V5: the served revision moves for a change nobody made
// through this surface, and it costs no stat.
// ---------------------------------------------------------------------------

func TestWatchOutOfBandEditMovesServedRevision(t *testing.T) {
	cell := newWatchCell(t, func(o *watchOptions) {
		o.heartbeat = 30 * time.Second
	}, nil)
	waitFor(t, 5*time.Second, "the watcher to be established", func() bool {
		return cell.status().State == WatchStateWatching
	})

	// D4's baseline: a read through the surface populates the hash cache for the
	// path (size+mtime), which is what makes the out-of-band gap of §2.4 visible.
	rec := do(t, cell.h, "GET", "/dav/src/main.go", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET -> %d %s", rec.Code, rec.Body.String())
	}
	revBefore := rec.Header().Get("X-Bunker-Rev")
	if revBefore == "" {
		t.Fatalf("no X-Bunker-Rev on a GET")
	}
	// R-V3: the composite token must be DECLARED under its own kind, on the wire.
	if got := cell.h.revKind(); got != "git+watch" {
		t.Fatalf("revKind() = %q, want git+watch while a watcher is live (an undeclared composite is forbidden by R-V3)", got)
	}

	// The change nobody made through us.
	abs := filepath.Join(cell.root, "src", "main.go")
	mustWrite(t, abs, "package main\n\nfunc main() {}\n// out of band\n")
	cell.fw.backend(0).pushEvent(abs, fsnotify.Write)

	waitFor(t, 5*time.Second, "the served revision to move for the out-of-band edit", func() bool {
		return cell.h.tree.revToken() != revBefore
	})
	if got := cell.h.tree.watched.Load(); got == 0 {
		t.Fatalf("the tree's watched-change count never moved: the token's watcher half is not wired (D1/D2)")
	}
	// D4: the cache was forgotten, so the next read sees the NEW bytes rather
	// than a hash the write path never invalidated.
	after := do(t, cell.h, "GET", "/dav/src/main.go", nil, "")
	if after.Code != http.StatusOK {
		t.Fatalf("GET after the out-of-band edit -> %d %s", after.Code, after.Body.String())
	}
	if !strings.Contains(after.Body.String(), "out of band") {
		t.Fatalf("the out-of-band edit was not seen through the served surface")
	}
	if got := after.Header().Get("X-Bunker-Rev"); got == revBefore {
		t.Fatalf("X-Bunker-Rev did not move for a vouched-for out-of-band change: %q", got)
	}
	// R-V3 on the wire: the capability document declares the extended kind.
	_, doc, _ := watchCarriers(t, cell.h)
	if doc.State != WatchStateWatching {
		t.Fatalf("document state = %q", doc.State)
	}
	var caps struct {
		Result struct {
			Capabilities struct {
				Extensions struct {
					Rev struct {
						Kind string `json:"kind"`
					} `json:"rev"`
				} `json:"extensions"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	raw := do(t, cell.h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "capabilities"}, "")
	if err := json.Unmarshal(raw.Body.Bytes(), &caps); err != nil {
		t.Fatalf("capabilities envelope: %v", err)
	}
	if got := caps.Result.Capabilities.Extensions.Rev.Kind; got != "git+watch" {
		t.Fatalf("extensions.rev.kind = %q, want git+watch (R-V3)", got)
	}

	// D5: the change is in the tree's own ledger, so a POLLER sees it too.
	line := lastLedgerEventOfKind(t, cell.h, eventInvalidate)
	if line == nil {
		t.Fatalf("the watched change was never journaled: a poller would see nothing (D5)")
	}
	if !containsString(line.Paths, "src/main.go") && !containsString(line.Paths, "src") {
		t.Fatalf("the ledger line names %v, want the changed path", line.Paths)
	}

	// A-10's control: an UNVOUCHED interval must not move the revision.
	gate := cell.park()
	revParked := cell.h.tree.revToken()
	other := filepath.Join(cell.root, "src", "util.go")
	mustWrite(t, other, "package main\n\nfunc util() {}\n// unvouched\n")
	cell.fw.backend(0).pushEvent(other, fsnotify.Write)
	time.Sleep(50 * time.Millisecond) // >> flushEvery, and the loop is parked
	if got := cell.h.tree.revToken(); got != revParked {
		t.Fatalf("the revision moved for a change the watcher had NOT vouched for (%q -> %q): R-V4", revParked, got)
	}
	close(gate)
	waitFor(t, 5*time.Second, "the vouched change to move it after the loop resumes", func() bool {
		return cell.h.tree.revToken() != revParked
	})
}

// ---------------------------------------------------------------------------
// R-V2 / A-10: the snapshot fast path as a NUMBER.
// ---------------------------------------------------------------------------

func TestWatchSnapshotFastPathCostWithWatcherLive(t *testing.T) {
	root := t.TempDir()
	// A tree big enough that a stat-per-file operation on a read is visible in
	// the ratio, and small enough to build in a test.
	const files = 4000
	for i := 0; i < files; i++ {
		dir := filepath.Join(root, fmt.Sprintf("d%02d", i%64))
		if i < 64 {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.go", i)), []byte("package main\n"), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	withGitFixture(t, root, strings.Repeat("cd", 20))
	h := newTestHandler(t, func(c *Config) { c.Root = root })

	const iters = 300
	measure := func(label string) (memo, refresh, snapshot time.Duration) {
		_ = h.tree.revToken()
		memo = minPerCallBFS048(iters, func() { _ = h.tree.revToken() })
		refresh = minPerCallBFS048(iters, func() {
			h.tree.revAt = time.Time{}
			_ = h.tree.revToken()
		})
		snapshot = minPerCallBFS048(20, func() {
			rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "snapshot"},
				`{"path":".","depth":"infinity"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("snapshot -> %d", rec.Code)
			}
		})
		t.Logf("BFS-035 fast path (%s): files=%d token_memo_hit=%s token_refresh=%s one_call_snapshot=%s",
			label, files, memo, refresh, snapshot)
		return
	}

	absentMemo, _, absentSnapshot := measure("watcher absent")

	// Now the same tree with the watcher LIVE (the fake backend, so the added
	// cost is the watcher's and not the kernel's).
	opts := defaultWatchOptions()
	opts.heartbeat = 30 * time.Second
	opts.flushEvery = 2 * time.Millisecond
	w := h.startWatch(defaultFakeWatch(h.tree.rootPath()).env(), opts)
	t.Cleanup(func() { h.Close() })
	if st := w.statusSnapshot(); st.State != WatchStateWatching {
		t.Fatalf("the watcher is not established: %s", st.Detail)
	}
	liveMemo, liveRefresh, liveSnapshot := measure("watcher live")

	// R-V2's rule, as a number: the token is the same two loads whether a watcher
	// is live or not, so its cost must not multiply.
	if liveMemo > 20*time.Microsecond {
		t.Fatalf("a memo-hit token with a live watcher cost %s: the watcher added work to a read path", liveMemo)
	}
	if liveRefresh > 2*time.Millisecond {
		t.Fatalf("a refreshed token with a live watcher cost %s: that is not two file reads", liveRefresh)
	}
	if absentMemo > 1*time.Microsecond && liveMemo > 4*absentMemo {
		t.Fatalf("the token path cost %s with the watcher live and %s without: R-V2 forbids the watcher buying a stat per read", liveMemo, absentMemo)
	}
	// The one-call snapshot must not become a stat-per-file operation: the whole
	// tree here is ~4000 files, so a walk would be milliseconds, not micros.
	if liveSnapshot > 3*absentSnapshot+2*time.Millisecond {
		t.Fatalf("the one-call snapshot cost %s with a live watcher and %s without: the snapshot fast path was degraded by the watcher", liveSnapshot, absentSnapshot)
	}
	if liveSnapshot > 500*time.Millisecond {
		t.Fatalf("the one-call snapshot cost %s: that is not a fast path", liveSnapshot)
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// lastLedgerEventOfKind returns the most recent journaled event of one kind, or
// nil. It is how a cell asks the LEDGER (D5) what the server believes it
// announced, rather than asking the watcher to repeat itself.
func lastLedgerEventOfKind(t *testing.T, h *Handler, kind string) *eventLine {
	t.Helper()
	l := h.tree.eventLedger()
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.journal) - 1; i >= 0; i-- {
		if l.journal[i].Event == kind {
			ev := l.journal[i]
			return &ev
		}
	}
	return nil
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want || strings.HasPrefix(s, want) {
			return true
		}
	}
	return false
}
