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
	// BFS-045's own counting. Each figure is kept where the fact happens, so a
	// reader can tell "the channel is quiet" from "the channel is dead" from
	// "the server never answered" instead of inferring one from another.
	//
	// requests counts every ATTEMPT to get an invalidation answer (a stream
	// attempt, a poll call, a revision poll); failures counts the attempts that
	// produced NO answer — a transport fault, a stalled channel, a channel end,
	// a 5xx. A declared capability refusal is deliberately NOT a failure: the
	// server answered, and it answered something true.
	requests    int64
	failures    int64
	lastFailure string
	// heartbeats counts heartbeat lines received on the push channel and
	// lastLine is when the last line of any kind arrived: together they are the
	// LIVENESS evidence, which §5.4 O-2 forbids inferring from the absence of
	// events.
	heartbeats int64
	lastLine   time.Time
	// stalled records that the channel ended by the IDLE RULE (a silence the
	// client cannot vouch for) rather than by a fault or by this client closing
	// it. It is cleared when a stream is established again.
	stalled bool
	// frameOverLimit records that the pushed channel was declared unusable
	// because an event frame crossed the per-frame bound this mount holds
	// (BFS-062) — a CONDITION and not only a count, so a reader can tell "these
	// frames are too big for this mount" from "the network is failing", which
	// is the distinction the transport misclassification destroyed.
	// frameLimitDetail names the numbers the verdict was taken from.
	frameOverLimit   bool
	framesOverLimit  int64
	frameLimitDetail string
	// frameLimitBytes is the per-line cap the reader was actually sized to, so
	// the bound in force is reportable rather than implied by the declaration.
	frameLimitBytes int64
	// vouchedAt/From/OK/Why are the content-age evidence (BFS-045): when the
	// client last had reason to believe its view of the served tree is current,
	// from what kind of evidence, and — when it has none — why not. A resync
	// CLEARS it, because after a knowledge loss the client must re-observe
	// before it can claim anything (BFS-063's rule, applied to the age).
	vouchedAt   time.Time
	vouchedFrom string
	vouchedOK   bool
	vouchedWhy  string
	// lastContent is when this client's knowledge of the served CONTENT was
	// last established or moved — an observation of the tree, or an event that
	// carried paths. A heartbeat deliberately does NOT move it: claiming
	// freshness from a liveness signal is the class of lie this row is about.
	observations int64
	// revSeen is the revision the revision poll observed last, so a change is
	// detectable with one cheap request per interval.
	revSeen string
	// obsSeq/obsOK are the observation this client HOLDS (BFS-063): the ledger
	// cursor a whole-tree answer was minted at, reported by the caller through
	// Observed. obsOK false is the honest state of a client that has observed
	// nothing — including one whose resync has dropped the view and whose
	// re-observation has not answered yet — and it is what makes the poll
	// present NO cursor, which the server answers with the interval it cannot
	// vouch for rather than a tail that claims coverage the client does not
	// have.
	obsSeq int64
	obsOK  bool
	// noticeSeq/noticeOK are the cursor of the last `overflow` the server
	// declared. An observation the server did NOT mint (the PROPFIND fallback,
	// or a build that predates the field) adopts this cursor: the notice was
	// answered before the observation was taken, so the tree that observation
	// saw is at least as new as the ledger state the notice named.
	noticeSeq int64
	noticeOK  bool
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
	// ResumeSeq is the resume point this client PRESENTS (BFS-063): the ledger
	// cursor its view was minted at, or absent when it holds no observation —
	// the state in which the server answers the interval it cannot vouch for
	// instead of a tail that would claim coverage the client does not have. It
	// is reported because the difference between "my view is current" and "I am
	// being told I cannot be vouched for" must be visible to the owner, and
	// because a client stuck without an observation is otherwise
	// indistinguishable from a quiet, healthy channel.
	ResumeSeq *int64 `json:"resume_seq,omitempty"`
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
	// BFS-045: the invalidation path's own request accounting. `requests_total`
	// is the denominator (every attempt to get an answer) and `failures_total`
	// the numerator (attempts that produced none), so "the channel is quiet"
	// cannot be confused with "the server is not answering".
	Requests    int64  `json:"requests_total"`
	Failures    int64  `json:"failures_total"`
	LastFailure string `json:"last_failure,omitempty"`
	// Server is what the SERVER's own capability document said about its
	// watcher — the state, the named reason when it is absent, its counters and
	// its coverage — with `sampled_age_ms` saying how old the sample is. When it
	// is absent, ServerReason says which of the three facts that is (no
	// handshake / block not published / ...), never a bare null.
	Server       *ServerWatchState `json:"server,omitempty"`
	ServerReason string            `json:"server_reason,omitempty"`
	// ContentAge is the CONTENT-AGE BOUND: how old the newest knowledge this
	// client can vouch for is, and the window the mechanism in force implies.
	// The client reported no content-age figure at all before BFS-045; a mount
	// that cannot say how stale it may be is the defect §2.7 names.
	ContentAge       *ContentAgeState `json:"content_age,omitempty"`
	ContentAgeReason string           `json:"content_age_reason,omitempty"`
	// Liveness is the heartbeat/stall state: the declared period, the heartbeats
	// actually received, how long ago the channel last spoke, and whether the
	// idle rule declared it dead. Absent WITH a reason when there is no stream
	// at all (a poll mount has no heartbeat to report).
	Liveness       *LivenessState `json:"liveness,omitempty"`
	LivenessReason string         `json:"liveness_reason,omitempty"`
	// Refresh is the refresh accounting: the staged window this build HAS, and
	// the hot-refresh queue it does NOT — every field of the absent one null
	// with a reason rather than a zero that could be mistaken for a measurement.
	Refresh RefreshState `json:"refresh"`
	// FrameLimit is the pushed channel's FRAME accounting (BFS-062): the bound
	// the server declared, the per-line cap this consumer was sized to, and
	// whether a frame ever crossed it. Absent WITH A REASON when the mount never
	// attempted the pushed channel — there is then no reader and therefore no
	// bound, which is a different fact from a bound of zero.
	FrameLimit       *FrameLimitState `json:"frame_limit,omitempty"`
	FrameLimitReason string           `json:"frame_limit_reason,omitempty"`
}

// ServerWatchState is the server's own watcher block, reported verbatim (the
// client measures none of it) plus the age of the sample it came from.
type ServerWatchState struct {
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Backend    string `json:"backend,omitempty"`
	BlocksPush *bool  `json:"blocks_push,omitempty"`
	Vouched    *bool  `json:"vouched,omitempty"`
	Stalled    *bool  `json:"stalled,omitempty"`
	// SampledAgeMS is how long ago the capability document carrying this block
	// was received. A sample is not the present, and the record says so.
	SampledAgeMS *int64 `json:"sampled_age_ms"`
	HeartbeatMS  *int64 `json:"heartbeat_ms,omitempty"`
	// MaxPathsPerEvent is the declared cap on one event's path list — a bound the
	// invalidation path RELIES ON, because a longer list is treated as an
	// overflow (drop everything and re-snapshot) rather than a partial drop. It
	// is reported because a rule whose bound is invisible cannot be audited.
	MaxPathsPerEvent *int64 `json:"max_paths_per_event,omitempty"`
	// MaxEventBytes is the declared BYTE bound on one event frame
	// (SPEC-push-channel §7.2, BFS-062), reported for the same reason and one
	// more: it is the number this consumer SIZES ITS READER FROM, so a mount
	// whose frames cannot be read must be able to show which declaration it
	// honoured. Absent when the document published none — a peer that predates
	// the additive field — which is a fact and not a bound of zero.
	MaxEventBytes *int64 `json:"max_event_bytes,omitempty"`
	// Coverage — what the watch set actually covers, and what it does not. Also
	// pointers, for the same reason as the counters: no coverage block means no
	// coverage figure was published, and CoverageReason says so in the server's
	// own words.
	DirectoriesDesired *int   `json:"directories_desired"`
	DirectoriesWatched *int   `json:"directories_watched"`
	MissingCount       *int   `json:"missing_count"`
	CoverageComplete   *bool  `json:"coverage_complete,omitempty"`
	CoverageReason     string `json:"coverage_reason,omitempty"`
	// Counters — the server's own flow figures: overflow events, intervals it
	// cannot vouch for, rescans, install failures, backend errors, heartbeats
	// and event-loop ticks.
	//
	// THEY ARE POINTERS ON PURPOSE. A build that publishes no counters block has
	// published no counter — reporting 0 for one would be exactly the figure this
	// row forbids (a number with no source, indistinguishable from a measured
	// zero), and the SERVER'S OWN sentence travels beside it in CountersReason.
	// The same applies to the coverage counts below.
	OverflowsTotal       *int64 `json:"overflows_total"`
	UnvouchedTotal       *int64 `json:"unvouched_total"`
	UnvouchedReason      string `json:"unvouched_reason,omitempty"`
	RescansTotal         *int64 `json:"rescans_total"`
	InstallFailuresTotal *int64 `json:"install_failures_total"`
	BackendErrorsTotal   *int64 `json:"backend_errors_total"`
	// DroppedEvents is null WITH A REASON when the kernel reports one overflow
	// marker and not how many events it dropped — the null rule with the reason
	// travelling beside it, never a fabricated count.
	DroppedEvents       *int64 `json:"overflow_dropped_events"`
	DroppedEventsReason string `json:"overflow_dropped_reason,omitempty"`
	LastEventAgeMS      *int64 `json:"last_event_age_ms,omitempty"`
	HeartbeatsTotal     *int64 `json:"heartbeats_total"`
	EventLoopTicks      *int64 `json:"event_loop_ticks"`
	CountersReason      string `json:"counters_reason,omitempty"`
}

// ContentAgeState is the content-age bound, reported as a figure AND the window
// it is measured against.
type ContentAgeState struct {
	// AgeMS is how long ago this client last had EVIDENCE for its view of the
	// served tree: the observation it took, the change event it applied, the
	// poll that answered, or the line the pushed channel delivered. It is
	// deliberately not "age of the last change": a quiet tree is not a stale
	// one, and a figure that conflated the two would alarm on every idle mount.
	AgeMS int64 `json:"age_ms"`
	// EvidenceFrom names WHICH kind of evidence the age is measured from, and it
	// matters because they are not equally strong: `observation` and `event` are
	// content (the client re-established or learned the tree), `poll` is a
	// whole-tree answer, and `heartbeat` is LIVENESS — under a live pushed
	// channel, delivery is what makes the view current, and the label says that
	// is what is holding the claim up rather than implying a content check
	// happened (§5.4 O-2: liveness is never inferred from the absence of events,
	// and it is never reported as content either).
	EvidenceFrom string `json:"evidence_from"`
	// Observations counts the times evidence was (re-)established, so the age
	// has a denominator rather than being a number out of nowhere.
	Observations int64 `json:"observations_total"`
	// BoundMS is the window the mechanism in force implies: the idle rule's
	// deadline for a live pushed channel, or two declared poll intervals (the
	// interval, plus one missed tick of tolerance) for a poll mechanism. Null
	// when no mechanism implies one, with BoundReason saying why.
	BoundMS     *int64 `json:"bound_ms"`
	BoundSource string `json:"bound_source,omitempty"`
	BoundReason string `json:"bound_reason,omitempty"`
	// WithinBound is the comparison, made HERE so no consumer has to make it
	// differently. Null when there is no bound to compare against.
	WithinBound *bool `json:"within_bound"`
}

// LivenessState is the heartbeat/stall state of the push channel.
type LivenessState struct {
	HeartbeatsTotal     int64  `json:"heartbeats_total"`
	LastLineAgeMS       *int64 `json:"last_line_age_ms"`
	DeclaredHeartbeatMS int64  `json:"declared_heartbeat_ms"`
	IdleTimeoutMS       int64  `json:"idle_timeout_ms"`
	// Stalled is the idle rule's verdict: the channel went silent for longer
	// than the client can vouch for and was declared dead. StallsTotal counts
	// the times it happened (the same fact as idle_fallbacks_total, reported
	// here because the heartbeat block must be readable on its own).
	Stalled     bool  `json:"stalled"`
	StallsTotal int64 `json:"stalls_total"`
}

// RefreshState is the refresh accounting.
//
// TWO DIFFERENT SUBSYSTEMS, and only one of them exists here. The STAGED WINDOW
// is landed (BFS-038): the refreshes that hold unpublished bytes, the bound on
// how many, and the refusals by reason. The HOT-REFRESH QUEUE is BFS-037's and is
// not in this build, so its figures are NULL WITH A REASON — not zeros, because
// a zero would read as "the queue was empty", which is a measurement this build
// cannot make. That distinction is the row's whole point applied to this row's
// own subject: a figure that cannot be sourced must be absent with a reason
// rather than estimated.
type RefreshState struct {
	// StartedTotal counts the refreshes ADMITTED to the staged window;
	// Committed + Aborted + the ones still in flight account for it, so a
	// refresh that vanished without a verdict shows up as a gap.
	StartedTotal   int64 `json:"started_total"`
	InFlight       int   `json:"in_flight"`
	MaxInFlight    int   `json:"max_inflight"`
	CommittedTotal int64 `json:"committed_total"`
	AbortedTotal   int64 `json:"aborted_total"`
	// RefusedNoSlotTotal is the number refused FOR BEING FULL: every slot of
	// the in-flight window was taken.
	RefusedNoSlotTotal int64 `json:"refused_no_slot_total"`
	// RefusedNoRoomTotal is the number refused because published + in-flight
	// bytes leave no room under the bound. It is a different refusal from
	// "full", and both are counted.
	RefusedNoRoomTotal int64 `json:"refused_no_room_total"`
	// The hot-refresh queue (BFS-037): not in this build.
	QueueDepth            *int   `json:"queue_depth"`
	QueueMaxDepth         *int   `json:"queue_max_depth"`
	QueueRefusedFullTotal *int64 `json:"queue_refused_full_total"`
	// SkippedOversizeTotal is the number of refreshes skipped because the file
	// is over the policy's size ceiling. The CACHE's equivalent figure — a read
	// whose content is over the per-entry cap — is real and moving, and it is
	// reported in `cache.bypass_reasons{over_entry_cap}`; this one belongs to a
	// subsystem that does not exist yet, so it is null with the same reason.
	SkippedOversizeTotal *int64 `json:"skipped_oversize_total"`
	AbsentReason         string `json:"absent_reason,omitempty"`
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
		Requests:      i.requests,
		Failures:      i.failures,
		LastFailure:   i.lastFailure,
	}
	// The server's own watcher block, reported as the server stated it, with the
	// age of the sample. The three ways it can be absent are three different
	// facts and are reported as three different reasons (BFS-045's null rule):
	// no handshake at all is UNKNOWN, a document that carried no block is
	// NOT PUBLISHED, and a block whose watcher is absent is a VALUE (state=
	// absent) with the server's own named reason.
	switch {
	case i.client == nil:
		st.ServerReason = ReasonUnknown + ": this mount has no client, so no capability document was ever fetched"
	default:
		block, age, present := i.client.WatchObserved()
		switch {
		case !present && i.client.Capabilities() == nil:
			st.ServerReason = ReasonUnknown + ": no capability document has been received, so the server's watcher state is unknown rather than absent"
		case !present:
			st.ServerReason = ReasonNotPublished + ": the capability document this server served carries no watch block, so it published no watcher state to report"
		default:
			st.Server = serverWatchState(block, age)
		}
	}
	// The content-age bound. Absent WITH A REASON until evidence exists, and
	// again after a resync drops the view, because a claim the client cannot back
	// is worse than no claim (BFS-063's rule, applied to the age).
	if i.vouchedOK {
		cs := &ContentAgeState{
			AgeMS:        int64(timeSince(i.vouchedAt).Milliseconds()),
			EvidenceFrom: i.vouchedFrom,
			Observations: i.observations,
		}
		if bound, source, ok := i.contentAgeWindowLocked(); ok {
			cs.BoundMS = &bound
			cs.BoundSource = source
			within := cs.AgeMS <= bound
			cs.WithinBound = &within
		} else {
			cs.BoundReason = ReasonDisabled + ": no invalidation mechanism is in force on this mount, so it implies no window to compare the age against"
		}
		st.ContentAge = cs
	} else {
		st.ContentAgeReason = ReasonNoSample + ": this mount has no evidence yet that its view is current" + i.vouchedWhy
	}
	// The heartbeat/stall state. A poll mount has no heartbeat to report, and
	// saying so is part of the record: a reader must not read a missing block as
	// a dead channel.
	switch {
	case i.mode == ModePush || i.mechanism == MechanismWatch || i.streamEnds > 0 || i.reconnects > 0 || i.idleFallbacks > 0:
		hb := int64(i.clientHeartbeat().Milliseconds())
		ls := &LivenessState{
			HeartbeatsTotal:     i.heartbeats,
			DeclaredHeartbeatMS: hb,
			IdleTimeoutMS:       i.effectiveIdle().Milliseconds(),
			Stalled:             i.stalled,
			StallsTotal:         i.idleFallbacks,
		}
		if !i.lastLine.IsZero() {
			age := int64(timeSince(i.lastLine).Milliseconds())
			ls.LastLineAgeMS = &age
		}
		st.Liveness = ls
	default:
		st.LivenessReason = ReasonDisabled + ": this mount never attempted the pushed channel (mode=" + i.mode + "), so it has no heartbeat to report; the poll mechanism's currency is reported in content_age"
	}
	if !i.lastEvent.IsZero() {
		age := int64(timeSince(i.lastEvent).Milliseconds())
		st.LastEventAgeMS = &age
		st.LastEventAge = timeSince(i.lastEvent)
	}
	if i.obsOK {
		cursor := i.obsSeq
		if i.seq > cursor {
			cursor = i.seq
		}
		st.ResumeSeq = &cursor
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
	// The frame accounting (BFS-062). It is reported whenever a reader was ever
	// SIZED — an attempt at the pushed channel, whatever then happened to it —
	// and absent with a reason otherwise, because a mount that never opened the
	// channel has no bound to report and a 0 there would read as one.
	if i.frameLimitBytes > 0 || i.framesOverLimit > 0 {
		fl := &FrameLimitState{
			ReaderBytes:    i.frameLimitBytes,
			CeilingBytes:   ClientFrameCeiling,
			OverLimit:      i.frameOverLimit,
			OverLimitTotal: i.framesOverLimit,
			Detail:         i.frameLimitDetail,
		}
		if declared := i.declaredMaxEventBytes(); declared > 0 {
			fl.DeclaredBytes = &declared
		} else {
			fl.DeclaredReason = ReasonNotPublished + ": the server's capability document publishes no max_event_bytes, so this mount sized its reader to its own ceiling rather than to a declaration"
		}
		st.FrameLimit = fl
	} else {
		st.FrameLimitReason = ReasonDisabled + ": this mount never attempted the pushed channel, so no per-frame reader was ever sized and there is no bound to report"
	}
	return st
}

// clientHeartbeat is the server's declared heartbeat period, and it never panics
// on a client-less invalidator (a unit-built Invalidator has no client).
func (i *Invalidator) clientHeartbeat() time.Duration {
	if i.client == nil {
		return DefaultIdleTimeout / idleHeartbeats
	}
	return i.client.Capabilities().Heartbeat()
}

// contentAgeWindowLocked reports the window the mechanism in force implies for
// the age of the client's evidence, and where the number came from.
//
// A pushed channel that is ALIVE vouches for its silence up to the idle rule's
// deadline, so that deadline is its window. A poll mechanism vouches for the
// interval it declared, plus the tolerance of one missed tick (two intervals) —
// stated as a relation rather than a number so a server-configured interval is
// believed. With no mechanism answering there is no window, and the caller
// reports that absence with a reason instead of inventing one.
func (i *Invalidator) contentAgeWindowLocked() (int64, string, bool) {
	if !i.available {
		return 0, "", false
	}
	switch {
	case i.mode == ModePush && i.mechanism == MechanismWatch:
		return i.effectiveIdle().Milliseconds(), "idle_timeout", true
	case i.mode == ModePoll:
		return 2 * i.opt.PollInterval.Milliseconds(), "poll_interval", true
	}
	return 0, "", false
}

// serverWatchState maps the server's published block onto the record, sampling
// age included. Nothing here is inferred: a field the document did not publish
// stays nil rather than becoming a zero (BFS-045's null rule).
func serverWatchState(w *CapabilityWatch, age time.Duration) *ServerWatchState {
	out := &ServerWatchState{
		State:          w.State,
		Backend:        w.Backend,
		BlocksPush:     w.BlocksPush,
		CoverageReason: w.CoverageReason,
		CountersReason: w.CountersReason,
	}
	if w.Reason != nil {
		out.Reason = *w.Reason
	}
	out.Detail = w.Detail
	ms := int64(age.Milliseconds())
	out.SampledAgeMS = &ms
	if w.HeartbeatMS > 0 {
		hb := int64(w.HeartbeatMS)
		out.HeartbeatMS = &hb
	}
	if w.MaxPaths > 0 {
		mp := int64(w.MaxPaths)
		out.MaxPathsPerEvent = &mp
	}
	if w.MaxEventBytes > 0 {
		mb := w.MaxEventBytes
		out.MaxEventBytes = &mb
	}
	if w.Liveness != nil {
		v, s := w.Liveness.Vouched, w.Liveness.Stalled
		out.Vouched, out.Stalled = &v, &s
	}
	if w.Coverage != nil {
		// Every coverage figure is mapped only when the document published the
		// block: a build with no coverage block has published no coverage, and a
		// zero here would be a measurement that was never taken.
		desired, watched, missing := w.Coverage.DirectoriesDesired, w.Coverage.DirectoriesWatched, w.Coverage.MissingCount
		complete := w.Coverage.Complete
		out.DirectoriesDesired, out.DirectoriesWatched, out.MissingCount = &desired, &watched, &missing
		out.CoverageComplete = &complete
	}
	if w.Counters != nil {
		c := w.Counters
		out.OverflowsTotal = &c.OverflowsTotal
		out.UnvouchedTotal = &c.UnvouchedTotal
		out.UnvouchedReason = c.UnvouchedReason
		out.RescansTotal = &c.RescansTotal
		out.InstallFailuresTotal = &c.InstallFailuresTotal
		out.BackendErrorsTotal = &c.BackendErrorsTotal
		out.DroppedEvents = c.OverflowDroppedEvents
		out.DroppedEventsReason = c.OverflowDroppedReason
		out.LastEventAgeMS = c.LastEventAgeMS
		out.HeartbeatsTotal = &c.HeartbeatsTotal
		out.EventLoopTicks = &c.EventLoopTicks
	}
	return out
}

// RefreshFromCache fills the refresh block from the cache's own counters. It is a
// free function rather than a method because the staged window belongs to the
// cache and the queue figures belong to a subsystem this build does not have —
// the invalidator owns neither, and the two must not be merged into one account
// (O-3's rule, one layer over).
func RefreshFromCache(cs CacheStats) RefreshState {
	return RefreshState{
		StartedTotal:       cs.StagedStartedTotal,
		InFlight:           cs.StagedBlobs,
		MaxInFlight:        cs.MaxInFlight,
		CommittedTotal:     cs.StagedCommittedTotal,
		AbortedTotal:       cs.StagedAbortedTotal,
		RefusedNoSlotTotal: cs.StagedNoSlotTotal,
		RefusedNoRoomTotal: cs.StagedNoRoomTotal,
		// The hot-refresh queue is BFS-037's and is not in this build: every one
		// of its figures is null WITH A REASON, never a zero that could be read
		// as a measurement (a queue that reads depth=0 while no queue exists is
		// BFS-032's shape, one subsystem over).
		AbsentReason: ReasonNotPublished + ": the hot-refresh queue (BFS-037) is not in this build, so its depth, its bound and its refusals are not countable; the staged window below is the refresh accounting that does exist",
	}
}

func boolPtr(b bool) *bool { return &b }

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
	// A resync drops everything and re-establishes the view, so the observation
	// that was current a moment ago no longer describes what this client holds:
	// the caller reports the new one through Observed. Until it does, this client
	// declares NO cursor rather than a claim it can no longer back — the whole
	// point of BFS-063 being that a claim the server cannot check is worse than
	// no claim at all.
	i.obsOK = false
	// The same rule governs the CONTENT-AGE evidence (BFS-045): knowledge was
	// just declared lost, so this client has no age to report until it has
	// evidence again. Reporting the pre-drop age would be a figure describing
	// something other than what it claims — the defect class of this row.
	i.vouchedOK = false
	i.vouchedWhy = " (the view was dropped: " + reason + ")"
	i.mu.Unlock()
	if i.opt.OnDrop != nil {
		i.opt.OnDrop(nil, true)
	}
	if i.opt.OnResync != nil {
		i.opt.OnResync(reason)
	}
}

// Observed records that the caller has (re-)established its view of the served
// tree, and where. The mount calls it after every successful whole-tree snapshot
// and after the PROPFIND walk that replaces it (BFS-063).
//
// `minted` distinguishes the two, because they are not equally knowable: a
// snapshot-op answer carries a cursor the SERVER minted for this observation, so
// presenting it is a claim the server can check; the PROPFIND fallback was never
// minted, so this client adopts the cursor of the `overflow` notice that
// provoked the re-observation — sound for the one reason that matters, that the
// observation happened AFTER the notice, so the tree it saw is at least as new
// as the ledger state the notice named. With neither, the client holds nothing it
// can declare, and the next poll says exactly that.
func (i *Invalidator) Observed(cursor int64, minted bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	// The caller has just re-established its view of the served tree: this is
	// the strongest evidence there is, and it is what the content age is
	// measured from (BFS-045).
	i.noteEvidenceLocked("observation")
	switch {
	case minted:
		i.obsSeq, i.obsOK = cursor, true
	case i.noticeOK:
		i.obsSeq, i.obsOK = i.noticeSeq, true
	default:
		i.obsSeq, i.obsOK = 0, false
	}
}

// resumePoint reports the declaration this client presents on the next poll: the
// cursor it holds, and whether it holds one at all. The cursor is the highest seq
// this client has observed — the observation's own cursor, or a later event it
// has already applied (BFS-041 §3.1: the cursor advances on every line).
func (i *Invalidator) resumePoint() (int64, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.obsOK {
		return 0, false
	}
	if i.obsSeq > i.seq {
		return i.obsSeq, true
	}
	return i.seq, true
}

// advanceCursorLocked folds one line's seq into the cursor and reports what the
// advance means: `dup` a line already applied, `gap` an interval that was never
// observed.
//
// SPEC-push-channel §3.1 R-1: the cursor is "the highest seq the client has
// observed, whatever the line's `event` name was" — so a HEARTBEAT advances it
// exactly as an `invalidate` does. The defect this closes (H-2) was the
// opposite: `apply` returned for a heartbeat BEFORE this bookkeeping, so the
// heartbeat's seq was consumed and never recorded, the next real event then
// satisfied `Seq > cursor+1`, and the client paid a whole-tree resync for every
// keep-alive — the mechanism manufacturing the exact condition it exists to
// prevent.
//
// The second half is what keeps that from being a loosening: a jump of more than
// one is a REAL gap whichever line carried it. Advancing the cursor quietly over
// an unobserved range would vouch for an interval nobody saw, which is the
// silent-staleness reading of the same rule (§3.3 R-3) — so a heartbeat that
// stands over a gap resyncs rather than being recorded as if it were just a
// keep-alive.
//
// A seq of zero (or none) is not a cursor claim at all: it is neither advanced
// over nor read as a gap, and the guard on a non-zero cursor is BFS-063's.
func (i *Invalidator) advanceCursorLocked(seq int64) (dup, gap bool) {
	if seq <= 0 {
		return false, false
	}
	if seq <= i.seq {
		// A duplicate or a replay: already applied.
		return true, false
	}
	if seq > i.seq+1 && i.seq != 0 {
		i.gaps++
		gap = true
	}
	// The cursor advances either way: the missing range is never re-requested
	// (BFS-005 §4.1) — the resync that follows establishes current truth.
	i.seq = seq
	return false, gap
}

// apply handles one event. Returns an error only for an unrecoverable stream
// fault; every protocol-level oddity (gap, duplicate, unknown event name) is a
// decision, not an error.
func (i *Invalidator) apply(ev Event) {
	i.mu.Lock()
	// Any line off the channel is the LIVENESS evidence §5.4 O-2 demands, and it
	// is recorded where it arrives: an idle channel and a dead one look the same
	// from outside, and only a received line tells them apart.
	i.lastLine = timeNow()
	if ev.Event == EventHeartbeat {
		i.lastEvent = timeNow()
		i.events++
		i.heartbeats++
		// A heartbeat is LIVENESS evidence and it is recorded as exactly that:
		// under a live pushed channel, delivery is what makes the view current,
		// and `evidence_from: heartbeat` says so rather than pretending a
		// content check happened.
		i.noteEvidenceLocked("heartbeat")
		// R-1 (§3.1): a heartbeat's seq is a cursor observation like every other
		// line's, so it is RECORDED here rather than consumed and discarded
		// (H-2) — and, for that same reason, a jump over it is a real gap and
		// resyncs: recording it in SILENCE would vouch for an interval nobody
		// observed, which is the silent-staleness P0 this row must not trade its
		// P1 for (§3.3 R-3).
		_, gap := i.advanceCursorLocked(ev.Seq)
		i.mu.Unlock()
		if gap {
			i.Resync(fmt.Sprintf("sequence gap at seq=%d: the missing range is never re-requested", ev.Seq))
		}
		return
	}
	// A per-line tree that differs from the stream's own X-Bunker-Tree is a tree
	// change, never a parse error (BFS-005 §4.1, R-3).
	if ev.Tree != "" && i.client.opt.ExpectedTree != "" && ev.Tree != i.client.opt.ExpectedTree {
		i.mu.Unlock()
		i.Resync("tree changed mid-stream (" + ev.Tree + ")")
		return
	}
	dup, gap := i.advanceCursorLocked(ev.Seq)
	if dup {
		// A duplicate or a replay: already applied.
		i.mu.Unlock()
		return
	}
	i.events++
	i.lastEvent = timeNow()
	i.mu.Unlock()

	if ev.Event == EventOverflow {
		// The notice names a cursor, and it is recorded BEFORE the resync that
		// follows: the caller's re-observation may have nothing minted for it
		// (the PROPFIND fallback), and this notice is the freshest cursor the
		// server has named at a moment this client can prove it observed past.
		i.mu.Lock()
		if ev.Seq > 0 {
			i.noticeSeq, i.noticeOK = ev.Seq, true
		}
		i.mu.Unlock()
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
	// An event that carried paths told this client what actually moved: that is
	// content evidence, and it is labelled as such.
	i.noteEvidenceLocked("event")
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
	case frameLimitDegradation(watchErr):
		// BFS-062: a frame this mount cannot hold is DETERMINISTIC — the same
		// bytes arrive on every attempt — so the reconnect loop this error used
		// to land in was an outage manufactured from a size mismatch. Counted,
		// named, and taken once to the mechanism that does not read one frame
		// per line.
		return i.frameLimitToPoll(ctx, watchErr)
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
	i.mu.Lock()
	// The idle rule's verdict, recorded as a STATE and not only as a count: a
	// reader must be able to tell that the channel went silent for longer than
	// the client can vouch for, rather than inferring it from a rising number.
	i.stalled = true
	i.mu.Unlock()
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
		if frameLimitDegradation(err) {
			// BFS-062: never retried. Both entry points ask the same predicate,
			// so the first attempt and the retry after a fault cannot drift into
			// different sets.
			return i.frameLimitToPoll(ctx, err)
		}
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
	// BFS-062: the relation is checked BEFORE a stream is opened. A declaration
	// this mount cannot hold is a mismatch between two published documents, not
	// something to discover one frame at a time — and discovering it frame by
	// frame is how a size mismatch became a reconnect loop.
	if e := i.frameLimitMismatch(); e != nil {
		return e
	}
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
	// A stream is established again, so the idle rule's verdict no longer
	// describes the current channel (the stall COUNT stays where it is: it
	// happened).
	i.stalled = false
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
	// The reader is sized from the SERVER'S DECLARATION (BFS-062): the per-line
	// cap is the declared max_event_bytes plus the framing slack whenever one was
	// declared, and this client's own ceiling when none was. A cap chosen here
	// as a constant is what made a legal server frame unreadable.
	lineCap := i.readCap()
	i.mu.Lock()
	i.frameLimitBytes = int64(lineCap)
	i.mu.Unlock()
	go readStream(resp.Body, lines, stop, lineCap)

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
				return i.classifyStreamRead(rd.err)
			}
			// A line arrived, so the channel is demonstrably turning: the idle
			// deadline starts again — before the line is even interpreted.
			resetTimer(timer, idle)
			i.noteLine()
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
//
// lineCap is the per-line cap the CALLER sized (BFS-062): the declared
// max_event_bytes plus slack, or this client's ceiling when the server declared
// none. A line over it is not a transport failure — the caller classifies it as
// the named, counted, non-retryable frame condition it is.
func readStream(body io.Reader, out chan<- streamRead, stop <-chan struct{}, lineCap int) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), lineCap)
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
	// Every stream attempt is counted once, here, so the instrument cannot drift
	// from the loop that performs it: `requests_total` gains one per attempt and
	// `failures_total` gains one only when the attempt produced no answer.
	i.noteAttempt(err)
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

// countFrameOverLimit counts a frame that crossed the per-frame bound this mount
// holds (BFS-062). It is counted at the DECISION — the mismatch check before a
// stream is opened, or the classified read failure — so the figure is the number
// of times the condition was declared, never an inference from a reason string.
func (i *Invalidator) countFrameOverLimit() { i.bump(&i.framesOverLimit) }

// ---------------------------------------------------------------------------
// BFS-045: evidence and attempts.
// ---------------------------------------------------------------------------

// The evidence labels of the content-age figure. They are a closed vocabulary
// because the STRENGTH of the evidence is part of what is being reported: a
// heartbeat is liveness, an observation is content, and a reader must be able to
// tell which one is holding the claim up.
const (
	EvidenceObservation = "observation"
	EvidenceEvent       = "event"
	EvidencePoll        = "poll"
	EvidenceHeartbeat   = "heartbeat"
)

// noteEvidenceLocked records that this client has evidence for its view of the
// served tree, and what kind it is.
func (i *Invalidator) noteEvidenceLocked(from string) {
	i.vouchedAt = timeNow()
	i.vouchedFrom = from
	i.vouchedOK = true
	i.vouchedWhy = ""
	i.observations++
}

// noteAttempt counts ONE attempt to get an invalidation answer and, when it
// produced none, the failure itself — with the error verbatim, because "the
// channel is quiet" and "the server is not answering" must not read alike.
//
// A DECLARED capability refusal is an ANSWER and is deliberately not a failure:
// the server told us something true (it cannot serve this op on this target),
// and counting that as a backend error would inflate the figure with the one
// response that is working exactly as designed.
func (i *Invalidator) noteAttempt(err *OpError) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.requests++
	if err == nil || isDeclaredDegradation(err) {
		return
	}
	i.failures++
	i.lastFailure = err.Error()
}

// noteLine records that a line arrived off the pushed channel. It is the
// LIVENESS fact (§5.4 O-2) and it is recorded for EVERY line — a heartbeat, a
// duplicate, an unknown event name or a line this client cannot parse all prove
// the channel is turning, which is the only thing that can prove it.
func (i *Invalidator) noteLine() {
	i.mu.Lock()
	i.lastLine = timeNow()
	i.mu.Unlock()
}

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
//
// The request declares what this client HOLDS (BFS-063): `since_seq` at the
// highest cursor it has observed, or NO `since_seq` at all when it has observed
// nothing. The two are different requests and get different answers — a cursor
// buys the retained tail from that point, while no cursor is answered with the
// interval the server cannot vouch for. Presenting a cursor this client does not
// hold would be the dishonest direction: it would buy a quiet answer for a view
// that does not exist, which is exactly the silent gap BFS-063 was filed for.
func (i *Invalidator) pollEventsOnce(ctx context.Context) *OpError {
	since, hold := i.resumePoint()
	var out struct {
		Events []Event `json:"events"`
	}
	args := map[string]any{}
	if hold {
		args["since_seq"] = since
	}
	env, err := i.client.Op(ctx, "events", args, &out)
	i.noteAttempt(err)
	if err != nil {
		return err
	}
	i.mu.Lock()
	i.available = true
	i.mechanism = MechanismEvents
	i.lastEvent = timeNow()
	// A poll answer is a whole-tree statement about the served content, so it is
	// CONTENT evidence — the strongest kind this mechanism has.
	i.noteEvidenceLocked(EvidencePoll)
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
		i.noteAttempt(&OpError{Op: "OPTIONS", Errno: ErrnoENOTCONN, Cause: CauseUnreachableConnect, Err: err})
		return
	}
	pollCtx, cancel := context.WithTimeout(ctx, i.client.opt.OpTimeout)
	defer cancel()
	resp, derr := i.client.do(pollCtx, req.WithContext(pollCtx))
	if derr != nil {
		i.noteAttempt(derr)
		i.mu.Lock()
		i.reason = "revision poll failed: " + derr.Error()
		i.mu.Unlock()
		return
	}
	i.noteAttempt(nil)
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
	// The revision poll is a whole-tree answer at the token's granularity, so it
	// is CONTENT evidence — with the token's own coverage reported beside it
	// (rev_kind/rev_gap), which is what keeps a git-tree mount from reading as
	// fully current while it polls HEAD (BFS-048).
	i.noteEvidenceLocked(EvidencePoll)
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
