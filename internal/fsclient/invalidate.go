package fsclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Event is one line of the E-6 invalidation channel (BFS-004 §3 E-6, §10.5) —
// the wire shape this client consumes verbatim, not a second vocabulary of its
// own. Three event names, and the mapping of BFS-005 §4.1's richer vocabulary
// onto them is why the client's rules are what they are:
//
//   - `invalidate` — bytes or names moved; `paths[]` is non-empty. A rename or an
//     unlink arrives here too, which is why the client's default kernel
//     notification is EntryNotify and DeleteNotify is used only where the CLIENT
//     proved the name is gone.
//   - `heartbeat`  — liveness only; carries no path claims and must not be read
//     as "nothing changed".
//   - `overflow`   — knowledge is lost: drop everything and re-snapshot.
type Event struct {
	Seq   int64    `json:"seq"`
	Event string   `json:"event"`
	Paths []string `json:"paths,omitempty"`
	Rev   string   `json:"rev,omitempty"`
	Tree  string   `json:"tree,omitempty"`
}

// The three event names of the channel.
const (
	EventInvalidate = "invalidate"
	EventHeartbeat  = "heartbeat"
	EventOverflow   = "overflow"
)

// Invalidation modes, as reported by `bunker fs status` (§4.4: a mount silently
// downgraded to polling would be the exact class of defect AC-9 exists to catch).
const (
	ModePush = "push"
	ModePoll = "poll"
)

// Invalidation mechanisms — the concrete call that answered. `watch` is the
// pushed NDJSON stream, `events` the declared poll form of the same channel
// (per-path detail), and `rev` the revision poll this client falls back to when
// a build serves neither: one cheap request per interval whose X-Bunker-Rev
// answers for the tree AT THE REVISION'S OWN GRANULARITY. That granularity is
// declared by the capability document's `extensions.rev.kind` — `counter`
// moves on every mutation this surface performs, `git` moves only when the
// served tree's HEAD ref moves, so an uncommitted working-tree edit (ours or
// anyone else's) moves nothing on a git tree (BFS-048; SPEC-watcher-capability
// §2.4). The mechanism is reported because the difference between per-path
// drops and a whole-tree resync is a cost, not a detail. On the `rev` tier the
// reported state carries that granularity with it — `rev_kind`,
// `rev_vouches_for` and `rev_gap` (SPEC-watcher-capability §7.2 R-V4) — so a
// git-tree mount cannot read as fully current while what it polls is HEAD.
const (
	MechanismWatch  = "watch"
	MechanismEvents = "events"
	MechanismRev    = "rev"
	MechanismNone   = "none"
)

// Revision kinds, exactly as a surface DECLARES them (BFS-004 §4.2's
// `extensions.rev.kind`; SPEC-watcher-capability §7.1 D1/D2). A client reads the
// kind; it never infers one from the token's shape.
const (
	RevKindGit     = "git"
	RevKindCounter = "counter"
)

// What a served revision token MOVES for, per declared kind (§7.1). The
// revision poll is the client's last-resort mechanism, so what it can see is
// part of its contract rather than a footnote: a caller that needs a class of
// change NOT named here must be on a mechanism that observes the tree directly.
const (
	// RevVouchesForCommits — kind `git`: `.git/HEAD`'s ref moving (a commit,
	// checkout or reset). NOT an uncommitted working-tree write — not even one
	// this client made through the surface (BFS-048's measured fact).
	RevVouchesForCommits = "commits"
	// RevVouchesForSurfaceWrites — kind `counter`: mutations this surface
	// performs. NOT a write made by anyone else (§7.1 D2).
	RevVouchesForSurfaceWrites = "surface_writes"
	// RevVouchesForNothing — the document named no kind this client knows, so
	// NO coverage is claimed. It is a report, not a fallback: guessing here
	// would reintroduce the very defect this vocabulary exists to remove.
	RevVouchesForNothing = "nothing_declared"
)

// Revision coverage GAPS (SPEC-watcher-capability §7.2 R-V4): the class of
// change the mechanism IN FORCE cannot see, reported as a gap instead of being
// left to read as "unchanged". A mechanism that claims coverage it does not
// have is the defect class this removes (BFS-048; the same class as BFS-033 and
// BFS-060). One value per declared kind; there is no default and no empty
// string that means "fine".
const (
	// RevGapUncommittedWrites — kind `git`: a working-tree write that is not
	// committed, ours or anyone else's, moves nothing the poll can see. Aligning
	// the token with it is the watcher's job (§7.2 R-V1), never a stat-per-read
	// walk (R-V2) — so this build reports the gap rather than paying that price.
	RevGapUncommittedWrites = "uncommitted_working_tree_writes"
	// RevGapForeignWrites — kind `counter`: a write that does not go through
	// this surface moves nothing the counter can see (§7.1 D2).
	RevGapForeignWrites = "writes_not_through_this_surface"
	// RevGapKindUndeclared — the document named no kind, so the token's coverage
	// is unknown rather than assumed (§3.3: never report a claim that was not
	// probed).
	RevGapKindUndeclared = "revision_kind_not_declared"
)

// revCoverage maps a DECLARED revision kind onto what the token in force can
// and cannot see. A table read off the capability document, not a guess from
// the token's shape: a kind this client does not recognise is reported as no
// coverage claimed (R-V4) rather than as coverage, which is what keeps a new
// server-side kind from silently inheriting an old promise.
func revCoverage(kind string) (vouchesFor, gap string) {
	switch kind {
	case RevKindGit:
		return RevVouchesForCommits, RevGapUncommittedWrites
	case RevKindCounter:
		return RevVouchesForSurfaceWrites, RevGapForeignWrites
	default:
		return RevVouchesForNothing, RevGapKindUndeclared
	}
}

// DropFn applies BFS-005 §4.2's ordered drop: our path entries and metadata,
// then the kernel's copies. `full` means "drop everything and re-snapshot" (a
// sequence gap, an overflow event, a revision change under the rev mechanism).
type DropFn func(paths []string, full bool)

// InvalidateOptions configures an Invalidator.
type InvalidateOptions struct {
	// Paths scopes the watch subscription; empty watches the whole tree.
	Paths []string
	// Mode is auto|push|poll (--invalidation). `push` fails loudly when the
	// watcher is absent rather than silently downgrading; `auto` prefers push
	// and DECLARES the downgrade; `poll` never tries the stream.
	Mode string
	// PollInterval is the declared poll period (default 2 s).
	PollInterval time.Duration
	// IdleTimeout is how long the stream may be silent before the client
	// declares the channel dead and switches to poll. ZERO — the default — means
	// DERIVE it from the server's own declaration: three missed heartbeats of the
	// period its capability document names (SPEC-push-channel §8.1/§8.3), which
	// is DefaultIdleTimeout itself when the document names none (the ≤30 s the
	// surface pins). A non-zero value is the mount's own declared bound and
	// overrides the relation.
	IdleTimeout time.Duration
	// OnDrop is called for every applied invalidation.
	OnDrop DropFn
	// OnResync is called when the source changed and the whole view must be
	// re-established (gap, overflow, revision change). The mount re-snapshots.
	OnResync func(reason string)
}

// DefaultPollInterval is the declared poll period (BFS-005 §9).
const DefaultPollInterval = 2 * time.Second

// DefaultIdleTimeout is the 90 s silence rule of §4.4: three missed heartbeats of
// the ≤30 s period the surface pins when its document names none — exactly what
// Capabilities.Heartbeat() returns in that case. It is the FALLBACK for a client
// whose server declared nothing; where the document declares a period, the rule
// is derived from that period (idleFromHeartbeat) rather than from this number.
const DefaultIdleTimeout = 90 * time.Second

// idleHeartbeats is §8.1/§8.3's relation, kept as a relation rather than a
// constant so a server that declares a shorter heartbeat period is believed.
const idleHeartbeats = 3

// Invalidator runs the client's invalidation channel: the pushed NDJSON stream
// where the target has a watcher, the declared poll form where it does not, and
// the revision poll where it has neither — always reporting which one answered.
type Invalidator struct {
	client *Client
	opt    InvalidateOptions

	mu        sync.Mutex
	mode      string
	mechanism string
	seq       int64
	lastEvent time.Time
	started   time.Time
	gaps      int64
	resyncs   int64
	events    int64
	dropped   int64
	reason    string // why the current mode was chosen / downgraded to
	available bool
	// The push path's own counters (§11.1): a channel that ended and came back
	// must be distinguishable from one that never stopped, and a stall that
	// forced the poll must stay visible after the poll has answered.
	streamEnds    int64
	reconnects    int64
	idleFallbacks int64
	// revSeen is the revision the revision poll observed last, so a change is
	// detectable with one cheap request per interval.
	revSeen string
}

// NewInvalidator builds an invalidator. It does not connect: Run does.
func NewInvalidator(c *Client, opt InvalidateOptions) *Invalidator {
	if opt.PollInterval <= 0 {
		opt.PollInterval = DefaultPollInterval
	}
	// IdleTimeout is deliberately NOT defaulted here: zero means "derive it from
	// the server's declared heartbeat" (effectiveIdle), and a non-zero value is
	// the mount's own declared bound.
	if opt.Mode != ModePush && opt.Mode != ModePoll {
		opt.Mode = "auto"
	}
	return &Invalidator{
		client:    c,
		opt:       opt,
		mode:      opt.Mode,
		mechanism: MechanismNone,
		started:   timeNow(),
	}
}

// InvalidationState is the reported state (BFS-005 §3.2's `invalidation`).
type InvalidationState struct {
	Mode           string        `json:"mode"`
	Mechanism      string        `json:"mechanism"`
	Seq            int64         `json:"seq"`
	LastEventAge   time.Duration `json:"-"`
	LastEventAgeMS *int64        `json:"last_event_age_ms"`
	PollIntervalMS *int64        `json:"poll_interval_ms"`
	Gaps           int64         `json:"resyncs_from_gap"`
	Resyncs        int64         `json:"resyncs_total"`
	Events         int64         `json:"events_total"`
	DroppedPaths   int64         `json:"paths_dropped_total"`
	Reason         string        `json:"reason,omitempty"`
	Available      bool          `json:"channel_available"`
	// RevKind, RevVouchesFor and RevGap are the revision mechanism's coverage
	// report (SPEC-watcher-capability §7.2 R-V4): which kind the token in force
	// is (DECLARED by the capability document, never inferred), which class of
	// change that token moves for, and which class of change it cannot see at
	// all. They exist so that a mount can never read as fully current when the
	// mechanism in force is blind to a class of change — the false claim BFS-048
	// measured, now a reported gap instead of a silence.
	//
	// They are populated ONLY while the mechanism in force is `rev` (the last
	// resort, and the only mechanism whose currency rests on the token), so an
	// empty group means "the mechanism in force is not the revision poll", never
	// "no gap".
	RevKind       string `json:"rev_kind,omitempty"`
	RevVouchesFor string `json:"rev_vouches_for,omitempty"`
	RevGap        string `json:"rev_gap,omitempty"`
	// IdleTimeoutMS is the silence deadline the read loop arms — three declared
	// heartbeats. It is reported whenever the push rule is or has been in force,
	// because a bound the owner cannot see is not a bound (PRD §2.8).
	IdleTimeoutMS *int64 `json:"idle_timeout_ms"`
	// The push channel's own counters (§11.1). Each one is a fact the record must
	// be able to state on its own: the channel ENDED (§8.2 R-6), it was
	// RE-established, and the idle rule FIRED and forced the declared poll
	// (§8.1/§8.3). Without them a channel that died and came back is
	// indistinguishable from one that never died.
	StreamEnds    int64 `json:"stream_ends_total"`
	Reconnects    int64 `json:"reconnects_total"`
	IdleFallbacks int64 `json:"idle_fallbacks_total"`
}

// State reports the current invalidation state.
func (i *Invalidator) State() InvalidationState {
	i.mu.Lock()
	defer i.mu.Unlock()
	st := InvalidationState{
		Mode:          i.mode,
		Mechanism:     i.mechanism,
		Seq:           i.seq,
		Gaps:          i.gaps,
		Resyncs:       i.resyncs,
		Events:        i.events,
		DroppedPaths:  i.dropped,
		Reason:        i.reason,
		Available:     i.available,
		StreamEnds:    i.streamEnds,
		Reconnects:    i.reconnects,
		IdleFallbacks: i.idleFallbacks,
	}
	if !i.lastEvent.IsZero() {
		age := int64(timeSince(i.lastEvent).Milliseconds())
		st.LastEventAgeMS = &age
		st.LastEventAge = timeSince(i.lastEvent)
	}
	if i.mode == ModePoll {
		ms := i.opt.PollInterval.Milliseconds()
		st.PollIntervalMS = &ms
	}
	// The revision tier reports its own coverage (R-V4). Every other mechanism
	// observes the tree directly, so the field group stays empty and means
	// exactly that: the mechanism in force is not the revision poll.
	if i.mechanism == MechanismRev {
		st.RevKind = i.client.RevKind()
		st.RevVouchesFor, st.RevGap = revCoverage(st.RevKind)
	}
	if i.mode == ModePush || i.mechanism == MechanismWatch || i.idleFallbacks > 0 {
		ms := i.effectiveIdle().Milliseconds()
		st.IdleTimeoutMS = &ms
	}
	return st
}

// Mode reports push|poll.
func (i *Invalidator) Mode() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.mode
}

// Mechanism reports watch|events|rev|none.
func (i *Invalidator) Mechanism() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.mechanism
}

// Resync applies a full resync: drop everything, re-establish, advance past the
// gap. The missing range is never re-requested — the re-snapshot already
// establishes current truth, and asking for a range the server says it cannot
// produce only earns another overflow (BFS-005 §4.1).
func (i *Invalidator) Resync(reason string) {
	i.mu.Lock()
	i.resyncs++
	i.reason = reason
	i.mu.Unlock()
	if i.opt.OnDrop != nil {
		i.opt.OnDrop(nil, true)
	}
	if i.opt.OnResync != nil {
		i.opt.OnResync(reason)
	}
}

// apply handles one event. Returns an error only for an unrecoverable stream
// fault; every protocol-level oddity (gap, duplicate, unknown event name) is a
// decision, not an error.
func (i *Invalidator) apply(ev Event) {
	i.mu.Lock()
	if ev.Event == EventHeartbeat {
		i.lastEvent = timeNow()
		i.events++
		i.mu.Unlock()
		return
	}
	// A per-line tree that differs from the stream's own X-Bunker-Tree is a tree
	// change, never a parse error (BFS-005 §4.1, R-3).
	if ev.Tree != "" && i.client.opt.ExpectedTree != "" && ev.Tree != i.client.opt.ExpectedTree {
		i.mu.Unlock()
		i.Resync("tree changed mid-stream (" + ev.Tree + ")")
		return
	}
	gap := false
	if ev.Seq > 0 {
		switch {
		case ev.Seq <= i.seq:
			// A duplicate or a replay: already applied.
			i.mu.Unlock()
			return
		case ev.Seq > i.seq+1 && i.seq != 0:
			gap = true
			i.gaps++
		}
		i.seq = ev.Seq
	}
	i.events++
	i.lastEvent = timeNow()
	i.mu.Unlock()

	if ev.Event == EventOverflow {
		i.Resync("overflow: the server declared knowledge lost")
		return
	}
	if gap {
		i.Resync(fmt.Sprintf("sequence gap at seq=%d: the missing range is never re-requested", ev.Seq))
		return
	}
	if ev.Event != EventInvalidate {
		// An unknown event name is not a resync: it carries no path claim and no
		// knowledge loss, so ignoring it can only leave a later event to fix the
		// view. (A name that DID claim paths would be handled above.)
		return
	}
	if len(ev.Paths) == 0 {
		i.Resync("invalidate event with no paths: nothing to drop precisely, so drop everything")
		return
	}
	if max := i.client.Capabilities().MaxPathsPerEvent(); len(ev.Paths) > max {
		i.Resync(fmt.Sprintf("invalidate carried %d paths (declared cap %d): treated as overflow, never a partial drop", len(ev.Paths), max))
		return
	}
	i.mu.Lock()
	i.dropped += int64(len(ev.Paths))
	i.mu.Unlock()
	if i.opt.OnDrop != nil {
		i.opt.OnDrop(ev.Paths, false)
	}
}

// Run drives the channel until ctx is done. It never blocks the caller: it is
// meant to be run in its own goroutine, and the mount stays usable (with a
// declared staleness window) while it retries.
func (i *Invalidator) Run(ctx context.Context) error {
	// Whatever ends this call, the channel is NOT running afterwards, so the
	// record must stop claiming that it is (H-1: a mount kept reporting
	// `channel_available=true` for the life of the mount after the channel had
	// returned).
	defer i.markStopped()
	if i.opt.Mode == ModePoll {
		i.setMode(ModePoll, "(--invalidation=poll)")
		return i.pollLoop(ctx)
	}
	// Establish the push channel.
	watchErr := i.watchSession(ctx)
	if ctx.Err() != nil {
		return nil
	}
	if watchErr == nil {
		// The one clean close: the context ended the stream, so this client
		// closed it. Every other ending — a clean EOF included — arrives as an
		// OpError (R-6), which is why this branch is no longer reachable from a
		// server-side end.
		return nil
	}
	// The watcher is absent or the stream cannot be established at all.
	switch {
	case i.opt.Mode == ModePush:
		// push-only: fail loudly rather than silently downgrading. A stall is no
		// exception — the mount declared it will not poll, so it is named rather
		// than absorbed into the declaration it opted out of.
		i.setReason("--invalidation=push and the pushed channel is unavailable: " + watchErr.Error())
		return fmt.Errorf("invalidation: push mode requested but the channel is unavailable: %w", watchErr)
	case watchErr.Verdict == VerdictStaleTree || watchErr.Cause == CauseStaleIdentity:
		// NOT a poll trigger: the remedy is a re-bind, never a downgrade.
		i.setReason("stale_identity: the served tree is not the bound tree; re-bind, do not downgrade")
		return fmt.Errorf("invalidation: %w", ErrDeletedTree)
	case watchErr.Cause == CauseStreamStalled:
		// H-3's declared degradation: no line for the idle rule means the channel
		// is dead, and a dead channel is not a quiet tree. Take the mechanism
		// that works and say so (§8.1 idle row, §8.3's fifth condition).
		return i.stallToPoll(ctx, watchErr)
	case isDeclaredDegradation(watchErr):
		i.setMode(ModePoll, fmt.Sprintf("capability_unavailable on watch (scope=%s mode=%s): declared poll fallback", orDash(watchErr.Scope), orDash(watchErr.Mode)))
		return i.pollLoop(ctx)
	default:
		// A transient transport failure is not a poll trigger: reconnect with
		// the cursor, because a transport blip does not mean the events stopped
		// being generated. A clean EOF is the same shape — a channel end — and
		// takes the same path (R-6).
		return i.reconnectLoop(ctx, watchErr)
	}
}

// isDeclaredDegradation is §8.3's set of conditions under which this watcher
// will not serve this target and the declared poll is the mechanism that works:
// an absent capability, an op that predates this build, a bare POST with no op.
// Both entry points — the first attempt and the retry after a fault — ask the
// same predicate, so the two can never drift into different sets: mapping one of
// these onto a transport fault is what makes a client reconnect forever against
// a server that will never serve the op (§8.4 R-7).
func isDeclaredDegradation(err *OpError) bool {
	return err.Verdict == VerdictCapabilityUnavailable || err.Status == 501 ||
		err.Verdict == VerdictOpUnknown || err.Verdict == VerdictExtensionOpMissing
}

// stallToPoll applies the idle rule's declared degradation (H-3): the channel is
// reported dead, counted, and the mechanism that works takes over. The mode
// change is made BEFORE the first poll answers (O-1), and `available` stays
// false until one does — so the record never claims a channel it does not have.
func (i *Invalidator) stallToPoll(ctx context.Context, err *OpError) error {
	i.countIdleFallback()
	reason := sessionEndReason(err) + " — declared poll fallback"
	i.setMode(ModePoll, reason)
	return i.pollLoop(ctx)
}

// reconnectLoop retries the stream on transient faults, with the cursor, until
// ctx ends or the fault turns out to be a declared degradation. Every attempt is
// counted (§11.1) and every attempt is bounded: the delay is capped and the
// attempt itself carries the idle rule, so a stream that is accepted and then
// says nothing cannot park here forever.
func (i *Invalidator) reconnectLoop(ctx context.Context, last *OpError) error {
	backoff := 500 * time.Millisecond
	const maxBackoff = 10 * time.Second
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
		i.countReconnect()
		err := i.watchSession(ctx)
		if err == nil {
			return nil
		}
		last = err
		if err.Cause == CauseStreamStalled {
			return i.stallToPoll(ctx, err)
		}
		if isDeclaredDegradation(err) {
			i.setMode(ModePoll, "watcher disappeared mid-session: declared poll fallback")
			return i.pollLoop(ctx)
		}
		if err.Verdict == VerdictStaleTree {
			i.setReason("stale_identity after reconnect; re-bind required")
			return fmt.Errorf("invalidation: %w", ErrDeletedTree)
		}
	}
}

// watchOnce opens the stream and consumes it until it closes. It returns nil
// ONLY when the CONTEXT ended — i.e. this client closed it. Every other ending
// arrives as an OpError, a clean EOF included: a server-side stream end on a live
// context is a channel end, and reading it as a success is what ended
// invalidation for the life of the mount (H-1, R-6).
func (i *Invalidator) watchOnce(ctx context.Context) *OpError {
	i.mu.Lock()
	since := i.seq
	i.mu.Unlock()

	body, err := json.Marshal(map[string]any{"paths": i.opt.Paths, "since_seq": since})
	if err != nil {
		return &OpError{Op: "watch", Errno: ErrnoEIO, Cause: CauseServerError, Err: err}
	}
	req, rerr := i.client.newRequest(ctx, http.MethodPost, "", bytes.NewReader(body))
	if rerr != nil {
		return &OpError{Op: "watch", Errno: ErrnoENOTCONN, Cause: CauseUnreachableConnect, Err: rerr}
	}
	req.Header.Set("X-Bunker-Op", "watch")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")

	// The stream deliberately bypasses the operation deadline and the in-flight
	// semaphore: its response never ends, so a deadline would kill it by design
	// and a semaphore slot would be held for the life of the mount. On HTTP/1.1
	// this costs one extra TCP connection for the mount session (paid once, not
	// per event); on h2/h3 it is one stream among many (BFS-004 §4.3).
	resp, err := i.client.hc.Do(req)
	if err != nil {
		e := classifyTransport(err)
		e.Op = "watch"
		return e
	}
	defer resp.Body.Close()
	i.client.observe(resp)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		// The refusal shape here is an E-4 envelope, so decode it for the
		// capability/scope/mode fields rather than reporting only the status.
		var env Envelope
		_ = json.Unmarshal(raw, &env)
		e := &OpError{Op: "watch", Status: resp.StatusCode, Errno: ErrnoEOPNOTSUPP, Cause: CauseServerError,
			Verdict: firstNonEmpty(env.Verdict, resp.Header.Get("X-Bunker-Verdict"))}
		if env.Error != nil {
			e.Capability, e.Scope, e.Phase, e.Mode, e.Detail = env.Error.Capability, env.Error.Scope, env.Error.Phase, env.Error.Mode, env.Error.Detail
		}
		if e.Mode == "" {
			e.Mode = parseCapabilityMode(resp.Header.Get("X-Bunker-Capability"))
		}
		if e.Capability == "" {
			e.Capability = firstNonEmpty(envCapability(resp), "watch")
		}
		return e
	}

	// The response header names the tree; a mismatch is stale_identity, never a
	// resync (BFS-005 §4.1).
	if want := i.client.opt.ExpectedTree; want != "" {
		if got := resp.Header.Get("X-Bunker-Tree"); got != "" && got != want {
			return &OpError{Op: "watch", Errno: ErrnoEREMOTEIO, Cause: CauseStaleIdentity, Verdict: VerdictStaleTree,
				Detail: fmt.Sprintf("stream served tree %s, bound tree is %s", got, want)}
		}
	}
	i.mu.Lock()
	i.available = true
	i.mechanism = MechanismWatch
	i.mode = ModePush
	i.lastEvent = timeNow()
	i.mu.Unlock()

	// The read loop runs in its OWN goroutine so the idle rule can be a DEADLINE
	// rather than an inference: with a blocking Scan() there is nothing to select
	// on, so "no line for N seconds" is not observable at all and a stalled
	// channel cannot be told from a quiet tree (H-3; BFS-040 §5.4 O-2 makes that
	// distinction an obligation).
	//
	// Liveness keys on the RECEIPT OF A LINE. Not on the absence of events — O-2
	// forbids that inference, and a quiet tree answers nothing at all — and not
	// on the cursor: a heartbeat, a duplicate, an unknown event name or a line
	// this client cannot parse all prove the channel is turning, and `seq` stays
	// the cursor's own business. So this loop never writes `seq`, and a heartbeat
	// counts as liveness here whether or not apply() ever records it as one —
	// the two mechanisms stay independent (H-2/BFS-061 is that row's to fix).
	lines := make(chan streamRead, 1)
	stop := make(chan struct{})
	defer close(stop)
	go readStream(resp.Body, lines, stop)

	idle := i.effectiveIdle()
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			// We closed it: the one ending that IS a clean close.
			i.markUnavailable("the channel stopped: its context ended")
			return nil
		case <-timer.C:
			return &OpError{
				Op:     "watch",
				Errno:  ErrnoENOTCONN,
				Cause:  CauseStreamStalled,
				Detail: fmt.Sprintf("no line for %s (the idle rule: %d × the declared heartbeat): the channel is stalled, not quiet", idle, idleHeartbeats),
			}
		case rd := <-lines:
			if rd.eof {
				// A clean EOF on a LIVE context is a channel END, and BFS-040
				// §4.6 makes a server-side stream end a designed event
				// (`watch_lost`). Reading it as "we closed it" is what let the
				// channel die silently and permanently (H-1, R-6).
				i.countStreamEnd()
				return &OpError{
					Op: "watch", Errno: ErrnoENOTCONN, Cause: CauseStreamEnded,
					Detail: "the channel ended (a clean EOF on a live context) rather than being closed here",
				}
			}
			if rd.err != nil {
				if ctx.Err() != nil {
					i.markUnavailable("the channel stopped: its context ended")
					return nil
				}
				e := classifyTransport(rd.err)
				e.Op = "watch"
				return e
			}
			// A line arrived, so the channel is demonstrably turning: the idle
			// deadline starts again — before the line is even interpreted.
			resetTimer(timer, idle)
			line := bytes.TrimSpace(rd.line)
			if len(line) == 0 {
				continue
			}
			var ev Event
			if err := json.Unmarshal(line, &ev); err != nil {
				// A line that is not an event is a protocol fault: the channel's
				// contract is one JSON object per line, and guessing would be worse
				// than resyncing once.
				i.Resync("unparseable event line: resync rather than guess")
				continue
			}
			i.apply(ev)
		}
	}
}

// streamRead is one outcome of reading the NDJSON stream: a line, the clean end
// of the stream, or a read failure. Three states on purpose — the landed code
// collapsed the first two into a nil return, and that collapse is defect H-1.
type streamRead struct {
	line []byte
	eof  bool
	err  error
}

// readStream reads lines off body until the body ends, a read fails, or stop is
// closed. Both sends select on stop, so a consumer that leaves early — for the
// idle rule or for cancellation — cannot leak this goroutine: it unblocks as
// soon as the caller closes the response body.
func readStream(body io.Reader, out chan<- streamRead, stop <-chan struct{}) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		select {
		case out <- streamRead{line: line}:
		case <-stop:
			return
		}
	}
	rd := streamRead{eof: true}
	if err := scanner.Err(); err != nil {
		rd = streamRead{err: err}
	}
	select {
	case out <- rd:
	case <-stop:
	}
}

// resetTimer restarts the silent-stream deadline, draining a firing that raced
// the reset.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// effectiveIdle is the silence deadline the read loop arms (H-3): the mount's
// own value where it declared one, otherwise THREE MISSED HEARTBEATS of the
// period the SERVER declares — the heartbeat is the watcher's obligation
// (BFS-040 §5.4 O-2), so the consumer's deadline is derived from the source's
// own declaration rather than from a number of ours. Where the document names no
// period, Capabilities.Heartbeat() answers the ≤30 s the surface pins, which is
// DefaultIdleTimeout.
func (i *Invalidator) effectiveIdle() time.Duration {
	if i.opt.IdleTimeout > 0 {
		return i.opt.IdleTimeout
	}
	if hb := i.client.Capabilities().Heartbeat(); hb > 0 {
		return idleFromHeartbeat(hb)
	}
	return DefaultIdleTimeout
}

// idleFromHeartbeat is §8.1/§8.3's relation as a relation rather than a number,
// so a server that declares a shorter heartbeat period is believed rather than
// tolerated for 90 s.
func idleFromHeartbeat(hb time.Duration) time.Duration {
	return time.Duration(idleHeartbeats) * hb
}

// markUnavailable records that the channel cannot invalidate right now, and why.
// `available` is a CLAIM about the channel, so it goes false the moment the
// channel stops being one — the landed code left it `true` through a clean EOF,
// through a stall, and through every reconnect gap (H-1).
func (i *Invalidator) markUnavailable(reason string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.available = false
	i.reason = reason
}

// markStopped records that Run is over: no mechanism is running any more. A
// reason already recorded is KEPT — the reason that ended the channel is the
// useful thing to still be able to read.
func (i *Invalidator) markStopped() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.available = false
}

// sessionEndReason is what the record says after an attempt at the stream ended.
// The two named channel faults read as what happened rather than as an errno.
func sessionEndReason(err *OpError) string {
	switch err.Cause {
	case CauseStreamEnded:
		return "the channel ended (a clean EOF on a live context) — reconnecting with the cursor"
	case CauseStreamStalled:
		return "the channel stalled: " + err.Detail
	}
	return "the channel is not available: " + err.Error()
}

// watchSession is one attempt at the pushed channel, with the record kept honest
// around it: while no stream is established the channel is NOT available,
// whatever ended the attempt. Every attempt — the first one and every reconnect
// — goes through here, so no path can leave `available=true` standing over a
// channel that is not there.
func (i *Invalidator) watchSession(ctx context.Context) *OpError {
	err := i.watchOnce(ctx)
	if err != nil {
		i.markUnavailable(sessionEndReason(err))
	}
	return err
}

// The push channel's own counters (§11.1). Each is a fact the record must be
// able to state on its own; they are kept where the event happens rather than
// derived from anything, so a channel that died and came back cannot be confused
// with one that never died.
func (i *Invalidator) countStreamEnd()    { i.bump(&i.streamEnds) }
func (i *Invalidator) countReconnect()    { i.bump(&i.reconnects) }
func (i *Invalidator) countIdleFallback() { i.bump(&i.idleFallbacks) }

func (i *Invalidator) bump(field *int64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	*field++
}

// pollLoop is the DECLARED poll mode: one call per interval covering the whole
// tree revision, never silent about the staleness it implies.
func (i *Invalidator) pollLoop(ctx context.Context) error {
	i.mu.Lock()
	i.mode = ModePoll
	if i.mechanism == MechanismNone || i.mechanism == MechanismWatch {
		if i.client.Capabilities().WatchPollOp() {
			i.mechanism = MechanismEvents
		} else {
			i.mechanism = MechanismRev
		}
	}
	mech := i.mechanism
	i.mu.Unlock()

	ticker := time.NewTicker(i.opt.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		switch mech {
		case MechanismEvents:
			if err := i.pollEventsOnce(ctx); err != nil {
				if err.Cause == CauseStaleIdentity {
					return fmt.Errorf("invalidation: %w", ErrDeletedTree)
				}
				// A failed poll is not a mode change; the next tick retries and
				// the staleness window is bounded by the interval, as declared.
				i.mu.Lock()
				i.reason = "poll call failed: " + err.Error()
				i.mu.Unlock()
			}
		default:
			i.pollRevOnce(ctx)
		}
	}
}

// pollEventsOnce is the declared poll form of the channel (X-Bunker-Op: events):
// the same event objects, the same per-tree seq, one call per interval.
func (i *Invalidator) pollEventsOnce(ctx context.Context) *OpError {
	i.mu.Lock()
	since := i.seq
	i.mu.Unlock()
	var out struct {
		Events []Event `json:"events"`
	}
	env, err := i.client.Op(ctx, "events", map[string]any{"since_seq": since}, &out)
	if err != nil {
		return err
	}
	i.mu.Lock()
	i.available = true
	i.mechanism = MechanismEvents
	i.lastEvent = timeNow()
	i.mu.Unlock()
	// Events are applied oldest-first: seq ordering is the channel's contract.
	for _, ev := range out.Events {
		i.apply(ev)
	}
	_ = env
	return nil
}

// pollRevOnce is the revision poll. Every response carries X-Bunker-Rev and
// X-Bunker-Tree, so ONE cheap request per interval answers "has the tree moved
// at the served revision's own granularity?" A changed revision means the
// client cannot know WHICH paths moved — under this mechanism every change is a
// full resync, which is exactly why the mechanism is reported next to the mode.
//
// What the token covers is the revision's declared kind (extensions.rev.kind),
// not the whole tree unconditionally: on a git tree the token is the resolved
// HEAD, so an uncommitted working-tree edit moves nothing and this poll
// correctly reports quiet (BFS-048). Coverage of uncommitted edits is the
// `events` mechanism's job (its ledger observes the tree directly); the rev
// poll is the last resort and must never be read as asserting more than the
// kind it serves.
//
// Reporting it, not just documenting it (R-V4): while this mechanism is in
// force, State() carries `rev_kind`, `rev_vouches_for` and `rev_gap`, so the
// class of change this poll CANNOT see travels with every status read instead
// of living only in this comment. An edit that moves the token is still a
// whole-tree resync (there is no per-path detail here); an edit that does not
// move it is a gap, and it is reported as one.
func (i *Invalidator) pollRevOnce(ctx context.Context) {
	req, err := i.client.newRequest(ctx, http.MethodOptions, "", nil)
	if err != nil {
		return
	}
	pollCtx, cancel := context.WithTimeout(ctx, i.client.opt.OpTimeout)
	defer cancel()
	resp, derr := i.client.do(pollCtx, req.WithContext(pollCtx))
	if derr != nil {
		i.mu.Lock()
		i.reason = "revision poll failed: " + derr.Error()
		i.mu.Unlock()
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	gotTree, gotRev := resp.Header.Get("X-Bunker-Tree"), resp.Header.Get("X-Bunker-Rev")
	if want := i.client.opt.ExpectedTree; want != "" && gotTree != "" && gotTree != want {
		i.mu.Lock()
		i.reason = "stale_identity observed on the revision poll"
		i.mu.Unlock()
		return
	}
	i.mu.Lock()
	i.available = true
	i.mechanism = MechanismRev
	i.events++
	i.lastEvent = timeNow()
	prev := i.revSeen
	i.revSeen = gotRev
	i.mu.Unlock()
	if prev != "" && gotRev != "" && prev != gotRev {
		i.Resync(fmt.Sprintf("tree revision moved %s -> %s (revision poll: no per-path detail, so the whole view is re-established)", prev, gotRev))
	}
}

// lastRevSeen is the last revision the revision poll observed.
func (i *Invalidator) lastRevSeen() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.revSeen
}

// revAnswers reports how many revision-poll requests the server answered, so a
// test can prove the mechanism's shape: ONE cheap request per interval.
func (i *Invalidator) revAnswers() int64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.events
}

func (i *Invalidator) setMode(mode, reason string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.mode = mode
	i.reason = reason
	if mode == ModePoll && (i.mechanism == MechanismNone || i.mechanism == MechanismWatch) {
		if i.client.Capabilities().WatchPollOp() {
			i.mechanism = MechanismEvents
		} else {
			i.mechanism = MechanismRev
		}
	}
}

func (i *Invalidator) setReason(reason string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.reason = reason
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// envCapability reads the capability name out of a refusal.
func envCapability(resp *http.Response) string {
	if v := resp.Header.Get("X-Bunker-Capability"); v != "" {
		return v
	}
	return ""
}

// parseCapabilityMode parses the `watch;scope=target;mode=poll` header form of
// BFS-004 §5.2 into its mode field, so the client reports the mode the SERVER
// declared rather than one it guessed.
func parseCapabilityMode(v string) string {
	for _, part := range bytes.Split([]byte(v), []byte(";")) {
		s := string(bytes.TrimSpace(part))
		if rest, ok := cutPrefix(s, "mode="); ok {
			return rest
		}
	}
	return ""
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return s, false
}

// WatchPollOp reports whether the capability document declares the poll form of
// the channel, so the client can prefer per-path events over a whole-tree
// revision poll.
func (c *Capabilities) WatchPollOp() bool {
	if c == nil {
		return false
	}
	for _, m := range c.Extensions.Watch.Modes {
		if m == "X-Bunker-Op: events" {
			return true
		}
	}
	return false
}
