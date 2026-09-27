package fsclient

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-044 — THE HOT-FILE POLICY SURFACE (configuration, not implementation).
//
// `docs/prd/SPEC-hot-file-policy.md` is normative for every number in this
// file. This is the surface BFS-037 READS: it carries the policy's knobs, their
// defaults (which are the spec's pinned values, not invented here), its
// validation, and the resolved view an operator can read at runtime. It
// deliberately contains no cache, no tracker, no queue and no refresher: those
// are BFS-037, and building any of them here would put the implementation in
// front of the row that owns it.
//
// THREE RULES THIS FILE IS SHAPED BY, each of them a way the same defect has
// already landed in this repository:
//
//  1. A BOUND THE OWNER CANNOT SEE IS NOT A BOUND (§2.7). Every knob therefore
//     resolves into HotPolicyEffective, which is written into the mount's status
//     document — including the two figures that are DERIVED rather than
//     configured (`refresh_max_inflight` after P-4's clamp, and the reservation
//     ceiling it implies), because a derived bound nobody prints is the same
//     defect one layer down.
//
//  2. A BYTE BOUND ALONE DOES NOT BOUND A DIRECTORY (BFS-031: a 1 KiB byte bound,
//     a 30,689 B directory). The entry-count bound is therefore a first-class
//     bound in this surface in BOTH places a directory can grow: the cache
//     (`--cache-max-entries`, enforced in cache.go, exposed and validated here)
//     and the hot tracker (`--hot.max-entries`, H-7).
//
//  3. AN INVALID VALUE FAILS LOUDLY, AND NOTHING FALLS BACK SILENTLY. The
//     refusals name the flag, the value given and the value the spec pins, and a
//     refusal never degrades into a mount that quietly runs something else. The
//     ONE exception is P-4's clamp — `refresh_max_inflight` above the pool share
//     is CLAMPED AND REPORTED rather than refused, which the spec fixes
//     explicitly ("the share is a hard ceiling; the cap is the ordinary
//     governor", §6.1) — and it is reported as a clamp, which is why the
//     exception is not a silent fallback.
//
// CORRECTNESS IS NOT A FUNCTION OF ANY KNOB HERE (P-0/P-1). Every value in this
// file tunes a PERFORMANCE-ONLY subsystem: a mount with every knob in this file
// at its most restrictive legal value must read and write exactly the bytes it
// would with the subsystem absent. That property is what the BFS-044 evidence
// measures, and the shape of this type is what makes it expressible — there is
// no knob here that can be set to "trust the cache" or "skip validation".
//
// The runtime STATE vocabulary (armed|disarmed|stopped|misconfigured, §5.5
// Q-15) belongs to BFS-037 and the record's layout to BFS-045; this file
// reports only what the CONFIGURATION resolved to, under a vocabulary of its
// own, so the two cannot be confused.
// ---------------------------------------------------------------------------

// The hot-policy default state. DefaultHotEnabled is FALSE and that is the
// safe default for a published flag, deliberately:
//
//   - the subsystem is performance-only (P-0) and its value has not been
//     measured, so arming it by default would spend a user's bandwidth on the
//     strength of an unmeasured claim — the very thing O-1's `hot_value:
//     no_measured_benefit` exists to report;
//   - "must not make any feature default-on in a way that a stock client or an
//     old script notices" (BFS-044's brief): an existing `bunker fs mount`
//     invocation, or any other WebDAV client, must behave identically with this
//     row landed. It does: nothing in this surface changes a single request
//     until an operator asks for it.
//
// The knobs are honoured and validated whether or not the feature is enabled, so
// enabling it is a one-flag act on a policy that has already been checked.
const DefaultHotEnabled = false

// The policy's pinned numbers, one constant per knob, each naming the spec rule
// that fixes it so a later edit cannot move a number without moving the citation
// with it.
const (
	// DefaultHotWeightRead / DefaultHotWeightEdit — H-1/H-2 and Appendix A.1. An
	// edit is a strong signal because the file was worth opening AND will be
	// revisited; 8 is the smallest power of two that outranks a burst of seven
	// neighbouring reads.
	DefaultHotWeightRead = 1.0
	DefaultHotWeightEdit = 8.0
	// DefaultHotDecay / DefaultHotDecayStep — H-3: 0.9 per 300 s, i.e. an
	// effective half-life of 1973.6 s ≈ 32.9 min (Appendix A.2). 1.0 is a LEGAL
	// value and is the AC-1 control arm ("with hot_decay = 1.0 the
	// abandoned-favourite arm must turn red"), which is why the range is (0, 1]
	// rather than [0, 1).
	DefaultHotDecay     = 0.9
	DefaultHotDecayStep = 300 * time.Second
	// DefaultHotScoreCeiling — H-6. Reaching it halves every score in one pass;
	// a uniform scale changes no ordering, which AC-3 asserts rather than
	// assumes.
	DefaultHotScoreCeiling = 10000.0
	// DefaultHotReadTouchWindow — H-9. A FUSE read of an 8 MiB file arrives as
	// ~64 chunked calls, so without this window one file's single access would
	// score ~64 (F-6). It is a countable dedupe, not a heuristic.
	DefaultHotReadTouchWindow = 5 * time.Second
	// DefaultHotFlushInterval — H-21. A hard kill loses at most this much of a
	// hint (P-0).
	DefaultHotFlushInterval = 30 * time.Second
	// DefaultHotTrackerMaxEntries — H-7 / Appendix A.3. The working set of an
	// editing session, three orders of magnitude below the 400k-file tree the
	// PRD names.
	DefaultHotTrackerMaxEntries = 4096
	// DefaultHotTrackerMaxBytes — H-8 / Appendix A.3: 1 MiB, 0.39% of the cache
	// byte bound. A backstop the entry bound should never need.
	DefaultHotTrackerMaxBytes int64 = 1048576
	// DefaultHotMaxFileBytes — S-1 / Appendix A.4: 8 MiB, and the test is
	// INCLUSIVE (S-2), so a file exactly at the ceiling IS refreshable.
	DefaultHotMaxFileBytes int64 = 8388608
	// DefaultHotQueueMaxDepth — Q-1 / Appendix A.9: 256, which is 6.25% of one
	// worst-case 4096-path event, because Q-3's replacement rule (not the depth)
	// is what protects the hot file.
	DefaultHotQueueMaxDepth = 256
	// DefaultHotQueueMaxWait — Q-6: exactly one decay step, so an item that has
	// waited a full step has already lost 10% of its score.
	DefaultHotQueueMaxWait = 300 * time.Second
	// DefaultHotRefreshMaxInflight — Q-7: 2. One serialises the queue behind a
	// slow transfer; 2 keeps warming moving while staying far below the share.
	// It is a GOVERNOR: exceeding the share is clamped (P-4), never refused.
	DefaultHotRefreshMaxInflight = 2
	// DefaultHotPoolShareNum / DefaultHotPoolShareDen — P-3: 1/8, i.e. 3 of the
	// default 25 slots, which leaves foreground 22 of 25 (88%).
	DefaultHotPoolShareNum = 1
	DefaultHotPoolShareDen = 8
	// DefaultHotBackoffBase / DefaultHotBackoffMax / DefaultHotBackoffFactor —
	// P-10 / Appendix A.11: 250 ms · 2ⁿ, cap 30 s, full jitter. The cap equals
	// the client's own DefaultOpTimeout so a speculative fetch never backs off
	// longer than the foreground would wait for the same bytes.
	DefaultHotBackoffBase   = 250 * time.Millisecond
	DefaultHotBackoffMax    = 30 * time.Second
	DefaultHotBackoffFactor = 2.0
	// DefaultHotRefreshDeadline — P-11 / A.8: 10 s, one third of
	// DefaultOpTimeout.
	DefaultHotRefreshDeadline = 10 * time.Second
	// DefaultHotRefreshReacquireWindow — P-12 / A.8: 30 s, 15 poll
	// opportunities at the 2 s default cadence.
	DefaultHotRefreshReacquireWindow = 30 * time.Second
	// DefaultHotYieldAfter — P-7 / A.7: 250 ms, under a third of the measured
	// 0.79 s an entire 25×-concurrent run over the same bytes took.
	DefaultHotYieldAfter = 250 * time.Millisecond
	// DefaultHotStopDeadline — Q-12 / A.6: 1500 ms = one manager tick + one
	// yield quantum + 250 ms of slack.
	DefaultHotStopDeadline = 1500 * time.Millisecond
	// DefaultHotTickInterval — Q-14 / A.6: 1 s, finer than the channel's own 2 s
	// poll cadence so the manager is never the bottleneck.
	DefaultHotTickInterval = time.Second
	// DefaultHotPoolPressureTicks — P-19 / A.13: five consecutive ticks of a
	// foreground request waiting longer than the yield quantum, after which the
	// hot path stops itself in full.
	DefaultHotPoolPressureTicks = 5
)

// The backoff jitter vocabulary (P-10). "full" is the spec's default: a uniform
// draw in [0, delay]. "none" exists because a deterministic arm is the only way
// to assert the ladder's shape in a test, and because BFS-041's H-8 records the
// same defect one mechanism over — a jitterless backoff makes N clients return
// together.
const (
	// HotJitterFull — delay = rand(0, ladder). The default, and the only value
	// a deployment should run.
	HotJitterFull = "full"
	// HotJitterNone — delay = ladder. Deterministic; for tests and controls.
	HotJitterNone = "none"
)

// The queue's replacement rule (Q-3/Q-4) and the size rule's inclusivity (S-2)
// are not numbers and so are not knobs, but they ARE part of the policy an
// operator must be able to read back. They are constants reported in
// HotPolicyEffective, so "the queue replaces by SCORE, not FIFO" is a visible
// fact rather than a comment in a spec. Changing either is a spec change.
const (
	// HotQueueReplacementScore — when the queue is full, the newcomer displaces
	// the LOWEST-SCORING queued item or is refused. Under FIFO a `git checkout`
	// of 10k cold paths would occupy the whole queue and the file the user is
	// editing would wait behind all of them (Q-4, F-4).
	HotQueueReplacementScore = "score"
	// HotSizeRuleInclusive — `size <= hot_max_file_bytes` refreshes; ceiling + 1
	// skips (S-2). An exclusive reading would make the boundary a silent
	// one-byte difference.
	HotSizeRuleInclusive = true
)

// The configuration states this surface can be in. NOT the runtime states
// (armed|disarmed|stopped|misconfigured are BFS-037's, §5.5 Q-15): these three
// describe what the CONFIGURATION resolved to, which is all a config row can
// know.
const (
	// HotConfigDisabled — the feature is off by flag. The knobs are still
	// validated (an invalid value is invalid whether or not it is in use).
	HotConfigDisabled = "disabled"
	// HotConfigCacheDisabled — the feature is not usable because the cache is
	// off (D-6): warming a client that cannot store is bandwidth for a
	// guaranteed discard. This is a SKIP reason, not a mount refusal (S-11).
	HotConfigCacheDisabled = "cache_disabled"
	// HotConfigEnabled — the configuration resolved and the feature is on.
	HotConfigEnabled = "enabled"
	// HotConfigMisconfigured — the configuration cannot be honoured as written:
	// the size rule is above a bound the refreshed bytes could never fit under
	// (S-9/S-10), or two of the policy's own numbers contradict each other. Per
	// S-11 this is NOT a broken mount — "the mount still works; the hot path is
	// inert and reports itself as such" — so the state is reported, with both
	// numbers, and only an ENABLED policy refuses the mount (see Validate).
	// Reporting it is the whole difference between an inert knob and a silent
	// one.
	HotConfigMisconfigured = "misconfigured"
)

// DefaultHotPoolShare renders the pinned share in the spelling the flag accepts
// and the status document reports, derived from the two numbers rather than
// written a second time — a duplicated literal is how a default and its help
// text drift apart.
func DefaultHotPoolShare() string {
	return strconv.Itoa(DefaultHotPoolShareNum) + "/" + strconv.Itoa(DefaultHotPoolShareDen)
}

// DefaultHotJitter is the pinned jitter mode (P-10: full jitter, so N mounts
// stranded by one server restart cannot return together — BFS-041's H-8).
func DefaultHotJitter() string { return HotJitterFull }

// HotPolicyEnv is the surrounding configuration a hot policy is validated and
// resolved against. It is passed in rather than stored so the policy cannot
// carry a stale copy of the pool size or the cache bounds.
type HotPolicyEnv struct {
	// Concurrency is the client's request pool size, which P-3's share is taken
	// from (--concurrency, default 25).
	Concurrency int
	// OpTimeout is the client's own per-operation deadline. Two relations are
	// stated against it: the backoff cap may not exceed it (A.11) and a
	// refresh's deadline may not exceed it (A.8).
	OpTimeout time.Duration
	// CacheMaxBytes and CacheMaxEntryBytes are the RESOLVED cache bounds. A
	// CacheMaxBytes of 0 means the cache is disabled, in which case S-9/S-10's
	// cross-checks do not apply — D-6 makes a disabled cache a skip reason, not
	// a misconfiguration (S-11).
	CacheMaxBytes      int64
	CacheMaxEntryBytes int64
}

// HotPolicy is the client-side hot-file policy as CONFIGURED. Every field is a
// knob from SPEC-hot-file-policy.md §9.1's bound census and defaults to the
// value that census pins. Use DefaultHotPolicy() as the starting point: a
// partially populated policy is refused rather than silently completed, so a
// zero field can only ever be a caller's mistake and never a quiet default.
type HotPolicy struct {
	// Enabled arms the whole subsystem (--hot.enabled). Jitters the D-1
	// disarmed state: an armed policy starts disarmed until the bind-time
	// baseline completes and the observation is established, always.
	Enabled bool `json:"enabled"`

	// Popularity accounting (§3). Durations marshal as nanoseconds with the unit
	// in the field name, so a machine reading the status document cannot mistake
	// a nanosecond count for a second count.
	WeightRead        float64       `json:"read_weight"`          // --hot.read-weight, H-1, 1.0
	WeightEdit        float64       `json:"edit_weight"`          // --hot.edit-weight, H-2, 8.0
	Decay             float64       `json:"decay"`                // --hot.decay, H-3, 0.9 (1.0 = the no-decay control arm)
	DecayStep         time.Duration `json:"decay_step_ns"`        // --hot.decay-step, H-3, 300s
	ScoreCeiling      float64       `json:"score_ceiling"`        // --hot.score-ceiling, H-6, 10000
	ReadTouchWindow   time.Duration `json:"read_touch_window_ns"` // --hot.read-touch-window, H-9, 5s
	FlushInterval     time.Duration `json:"flush_interval_ns"`    // --hot.flush-interval, H-21, 30s
	TrackerMaxEntries int           `json:"tracker_max_entries"`  // --hot.max-entries, H-7, 4096 entries
	TrackerMaxBytes   int64         `json:"tracker_max_bytes"`    // --hot.max-tracker-bytes, H-8, 1 MiB

	// The size rule (§4).
	MaxFileBytes int64 `json:"max_file_bytes"` // --hot.max-file-bytes, S-1, 8 MiB, INCLUSIVE (S-2)

	// The queue (§5).
	QueueMaxDepth int           `json:"queue_max_depth"`   // --hot.queue-depth, Q-1, 256
	QueueMaxWait  time.Duration `json:"queue_max_wait_ns"` // --hot.queue-max-wait, Q-6, 300s

	// The pool policy (§6).
	RefreshMaxInflight int           `json:"refresh_max_inflight_configured"` // --hot.max-concurrent-refresh, Q-7, 2 (CLAMPED to the share, P-4)
	PoolShareNum       int           `json:"pool_share_num"`                  // --hot.pool-share numerator, P-3, 1
	PoolShareDen       int           `json:"pool_share_den"`                  // --hot.pool-share denominator, P-3, 8
	BackoffBase        time.Duration `json:"backoff_base_ns"`                 // --hot.backoff-base-ms, P-10, 250ms
	BackoffMax         time.Duration `json:"backoff_max_ns"`                  // --hot.backoff-max-ms, P-10, 30s
	BackoffFactor      float64       `json:"backoff_factor"`                  // --hot.backoff-factor, P-10, 2
	BackoffJitter      string        `json:"backoff_jitter"`                  // --hot.backoff-jitter, P-10, full|none

	// The deadlines (§5.5, §6.4).
	RefreshDeadline        time.Duration `json:"refresh_deadline_ns"`         // --hot.refresh-deadline, P-11, 10s
	RefreshReacquireWindow time.Duration `json:"refresh_reacquire_window_ns"` // --hot.reacquire-window, P-12, 30s
	YieldAfter             time.Duration `json:"yield_after_ns"`              // --hot.yield-after, P-7, 250ms
	StopDeadline           time.Duration `json:"stop_deadline_ns"`            // --hot.stop-deadline, Q-12, 1500ms
	TickInterval           time.Duration `json:"tick_interval_ns"`            // --hot.tick-interval, Q-14, 1s
	PoolPressureTicks      int           `json:"pool_pressure_ticks"`         // --hot.pool-pressure-ticks, P-19, 5
}

// DefaultHotPolicy returns the policy with every knob at the value
// SPEC-hot-file-policy.md pins, and the feature DISABLED (see DefaultHotEnabled
// for why off is the safe default). This is the only place the defaults exist:
// the CLI binds its flags onto a copy of this value, so a flag's help text, its
// pflag default and the value the client obeys cannot drift apart.
func DefaultHotPolicy() HotPolicy {
	return HotPolicy{
		Enabled:                DefaultHotEnabled,
		WeightRead:             DefaultHotWeightRead,
		WeightEdit:             DefaultHotWeightEdit,
		Decay:                  DefaultHotDecay,
		DecayStep:              DefaultHotDecayStep,
		ScoreCeiling:           DefaultHotScoreCeiling,
		ReadTouchWindow:        DefaultHotReadTouchWindow,
		FlushInterval:          DefaultHotFlushInterval,
		TrackerMaxEntries:      DefaultHotTrackerMaxEntries,
		TrackerMaxBytes:        DefaultHotTrackerMaxBytes,
		MaxFileBytes:           DefaultHotMaxFileBytes,
		QueueMaxDepth:          DefaultHotQueueMaxDepth,
		QueueMaxWait:           DefaultHotQueueMaxWait,
		RefreshMaxInflight:     DefaultHotRefreshMaxInflight,
		PoolShareNum:           DefaultHotPoolShareNum,
		PoolShareDen:           DefaultHotPoolShareDen,
		BackoffBase:            DefaultHotBackoffBase,
		BackoffMax:             DefaultHotBackoffMax,
		BackoffFactor:          DefaultHotBackoffFactor,
		BackoffJitter:          HotJitterFull,
		RefreshDeadline:        DefaultHotRefreshDeadline,
		RefreshReacquireWindow: DefaultHotRefreshReacquireWindow,
		YieldAfter:             DefaultHotYieldAfter,
		StopDeadline:           DefaultHotStopDeadline,
		TickInterval:           DefaultHotTickInterval,
		PoolPressureTicks:      DefaultHotPoolPressureTicks,
	}
}

// IsZero reports whether the policy is the zero value. A zero policy means "no
// policy was configured" and is replaced by the defaults; any OTHER partially
// populated policy is refused, because a half-filled struct is a caller bug that
// validation must surface rather than quietly complete.
func (p HotPolicy) IsZero() bool { return p == HotPolicy{} }

// PoolShare renders the configured share as the fraction an operator wrote
// ("1/8"). It is the spelling the flag accepts and the spelling the status
// document reports, so the two are the same string by construction.
func (p HotPolicy) PoolShare() string {
	return strconv.Itoa(p.PoolShareNum) + "/" + strconv.Itoa(p.PoolShareDen)
}

// ParsePoolShare parses a share written as a fraction ("1/8", "2/8", "1/4").
// It is strict on purpose: a value it cannot represent is a refusal naming what
// it got, never a default.
func ParsePoolShare(s string) (num, den int, err error) {
	numStr, denStr, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return 0, 0, fmt.Errorf("bunker-fs: --hot.pool-share wants a fraction like \"1/8\" (got %q)", s)
	}
	n, err := strconv.Atoi(strings.TrimSpace(numStr))
	if err != nil {
		return 0, 0, fmt.Errorf("bunker-fs: --hot.pool-share numerator is not a number (got %q)", s)
	}
	d, err := strconv.Atoi(strings.TrimSpace(denStr))
	if err != nil {
		return 0, 0, fmt.Errorf("bunker-fs: --hot.pool-share denominator is not a number (got %q)", s)
	}
	return n, d, nil
}

// Validate is the mount-time check: it refuses a configuration that cannot be
// honoured, and it never repairs, clamps or defaults a value — with the single,
// spec-stated exception of P-4's share clamp, which is applied in Effective()
// and reported as a clamp, not silently.
//
// TWO CLASSES OF PROBLEM, TWO ANSWERS, and the split is load-bearing:
//
//   - A RANGE failure says a knob is not a value at all (a zero decay, a
//     negative depth). It is refused ALWAYS, on a disabled policy as much as on
//     an armed one: an invalid number is invalid whether or not it is in use,
//     and a config surface that accepts nonsense because nothing reads it yet is
//     a surface that will accept it on the day something does.
//
//   - A RELATION failure says two configured numbers cannot both hold: the size
//     rule against a cache bound the refreshed bytes could never fit under
//     (S-9/S-10), a backoff cap above the client's own operation deadline
//     (A.11), a refresh deadline above it (A.8), a stop deadline below one tick
//     plus one yield quantum (A.6), or an edit weight that does not outrank a
//     read (H-2). Those are refused WHEN THE HOT PATH IS ENABLED — and when it
//     is not, they are REPORTED as HotConfigMisconfigured with both numbers
//     instead, which is what S-11 requires: "the mount still works; the hot path
//     is inert and reports itself as such".
//
// That second half is not a softening, it is a measured regression guard. The
// defaults pin an 8 MiB size rule, so a caller who only shrinks the cache
// (`--cache-max-entry-bytes 1k`, an existing flag, nothing to do with this row)
// would otherwise have their mount REFUSED by a knob they never set and a
// feature that is off. The probe that found it is docs/evidence/BFS-044-probes/.
func (p HotPolicy) Validate(env HotPolicyEnv) error {
	if err := p.validateRanges(); err != nil {
		return err
	}
	if !p.Enabled {
		return nil
	}
	if problems := p.RelationProblems(env); len(problems) > 0 {
		return fmt.Errorf("%s", problems[0])
	}
	return nil
}

// RelationProblems returns every RELATION failure between two configured
// numbers, each message naming the flag, the value given, the other value and
// the spec rule — in the same words the refusal uses, so an operator reading the
// status document reads exactly what an armed mount would have refused.
//
// The list is empty for a coherent policy, and it is what makes
// HotConfigMisconfigured a report rather than a shrug: the state and the reason
// travel together (an unexplained null is the thing this project keeps filing).
func (p HotPolicy) RelationProblems(env HotPolicyEnv) []string {
	var problems []string
	if p.WeightEdit <= p.WeightRead {
		problems = append(problems, fmt.Sprintf("bunker-fs: --hot.edit-weight (%v) must be greater than --hot.read-weight (%v); SPEC-hot-file-policy H-2's whole argument is that an edit is the STRONGER signal", p.WeightEdit, p.WeightRead))
	}
	if env.CacheMaxBytes > 0 {
		// S-10: a file above the per-entry cap can never be stored, so a refresh
		// of it would spend pool share, bandwidth and a reservation to fetch
		// bytes the cache must discard on arrival.
		if env.CacheMaxEntryBytes > 0 && p.MaxFileBytes > env.CacheMaxEntryBytes {
			problems = append(problems, fmt.Sprintf("bunker-fs: --hot.max-file-bytes (%d) exceeds the cache's per-entry cap (--cache-max-entry-bytes %d); SPEC-hot-file-policy S-10 refuses the mount here, because the refreshed bytes could never be stored (the hot path stays inert either way — S-11, P-0 clause 1)", p.MaxFileBytes, env.CacheMaxEntryBytes))
		}
		// S-9: a refresh of a file larger than the entire cache bound is not a
		// policy, it is a misconfiguration.
		if p.MaxFileBytes > env.CacheMaxBytes {
			problems = append(problems, fmt.Sprintf("bunker-fs: --hot.max-file-bytes (%d) exceeds the cache bound (--cache-max-size %d); SPEC-hot-file-policy S-9 refuses the mount naming both numbers", p.MaxFileBytes, env.CacheMaxBytes))
		}
	}
	if p.BackoffMax < p.BackoffBase {
		problems = append(problems, fmt.Sprintf("bunker-fs: --hot.backoff-max-ms (%s) must not be below --hot.backoff-base-ms (%s): SPEC-hot-file-policy P-10/Appendix A.11 states the ladder as a base, a factor and a cap, so a cap below the first delay is not a cap", p.BackoffMax, p.BackoffBase))
	}
	if env.OpTimeout > 0 && p.BackoffMax > env.OpTimeout {
		problems = append(problems, fmt.Sprintf("bunker-fs: --hot.backoff-max-ms (%s) exceeds the client's operation deadline (%s); SPEC-hot-file-policy P-10/Appendix A.11: a speculative fetch must never back off longer than the foreground would wait for the same bytes, or the hot path is slower than the cold path", p.BackoffMax, env.OpTimeout))
	}
	if env.OpTimeout > 0 && p.RefreshDeadline > env.OpTimeout {
		problems = append(problems, fmt.Sprintf("bunker-fs: --hot.refresh-deadline (%s) exceeds the client's operation deadline (%s); SPEC-hot-file-policy P-11/Appendix A.8: a speculative refresh is never worth a full foreground operation's deadline — abandon it instead", p.RefreshDeadline, env.OpTimeout))
	}
	// Q-12/Appendix A.6: the stop deadline is one manager tick plus one yield
	// quantum plus slack. Below that relation the deadline cannot be met by
	// construction, and `hot_stop_deadline_exceeded_total` would fire on a
	// healthy client — a counter that fires for a configuration reason tells the
	// owner nothing about the client.
	if floor := p.TickInterval + p.YieldAfter; p.StopDeadline < floor {
		problems = append(problems, fmt.Sprintf("bunker-fs: --hot.stop-deadline (%s) is below one manager tick plus one yield quantum (%s + %s = %s); SPEC-hot-file-policy Q-12/Appendix A.6 derives the deadline from that relation, so a smaller value would make a stop unable to complete inside its own deadline", p.StopDeadline, p.TickInterval, p.YieldAfter, floor))
	}
	return problems
}

// validateRanges checks every knob's OWN value — the half of validation that
// holds whether or not the feature is enabled.
func (p HotPolicy) validateRanges() error {
	// ---- popularity accounting (§3) ----
	if !(p.WeightRead > 0) || math.IsInf(p.WeightRead, 0) || math.IsNaN(p.WeightRead) {
		return fmt.Errorf("bunker-fs: --hot.read-weight must be a positive finite number (got %v); SPEC-hot-file-policy H-1 pins %v — a zero weight makes a touch a pure decay step, which is a different feature (Appendix A.1)", p.WeightRead, DefaultHotWeightRead)
	}
	if !(p.WeightEdit > 0) || math.IsInf(p.WeightEdit, 0) || math.IsNaN(p.WeightEdit) {
		return fmt.Errorf("bunker-fs: --hot.edit-weight must be a positive finite number (got %v); SPEC-hot-file-policy H-2 pins %v (a burst of seven reads must not outrank one edit)", p.WeightEdit, DefaultHotWeightEdit)
	}
	if !(p.Decay > 0) || p.Decay > 1 {
		return fmt.Errorf("bunker-fs: --hot.decay must be in (0, 1] (got %v); SPEC-hot-file-policy H-3 pins %v. 1.0 is legal and is the NO-DECAY control arm (AC-1)", p.Decay, DefaultHotDecay)
	}
	if p.DecayStep <= 0 {
		return fmt.Errorf("bunker-fs: --hot.decay-step must be > 0 (got %s); SPEC-hot-file-policy H-3 pins %s", p.DecayStep, DefaultHotDecayStep)
	}
	if !(p.ScoreCeiling > 0) {
		return fmt.Errorf("bunker-fs: --hot.score-ceiling must be > 0 (got %v); SPEC-hot-file-policy H-6 pins %v", p.ScoreCeiling, DefaultHotScoreCeiling)
	}
	if p.ReadTouchWindow <= 0 {
		return fmt.Errorf("bunker-fs: --hot.read-touch-window must be > 0 (got %s); SPEC-hot-file-policy H-9 pins %s — without the window one file's chunked read scores ~64 (F-6)", p.ReadTouchWindow, DefaultHotReadTouchWindow)
	}
	if p.FlushInterval <= 0 {
		return fmt.Errorf("bunker-fs: --hot.flush-interval must be > 0 (got %s); SPEC-hot-file-policy H-21 pins %s", p.FlushInterval, DefaultHotFlushInterval)
	}
	if p.TrackerMaxEntries < 1 {
		return fmt.Errorf("bunker-fs: --hot.max-entries must be >= 1 (got %d); SPEC-hot-file-policy H-7 pins %d — the tracker is bounded in ENTRIES as well as bytes and both are enforced", p.TrackerMaxEntries, DefaultHotTrackerMaxEntries)
	}
	if p.TrackerMaxBytes < 1 {
		return fmt.Errorf("bunker-fs: --hot.max-tracker-bytes must be >= 1 (got %d); SPEC-hot-file-policy H-8 pins %d", p.TrackerMaxBytes, DefaultHotTrackerMaxBytes)
	}

	// ---- the size rule (§4) ----
	if p.MaxFileBytes < 1 {
		return fmt.Errorf("bunker-fs: --hot.max-file-bytes must be >= 1 (got %d); SPEC-hot-file-policy S-1 pins %d. A size rule of zero is not \"off\" — use --hot.enabled=false, so the state is named rather than inferred from a bound", p.MaxFileBytes, DefaultHotMaxFileBytes)
	}

	// ---- the queue (§5) ----
	if p.QueueMaxDepth < 1 {
		return fmt.Errorf("bunker-fs: --hot.queue-depth must be >= 1 (got %d); SPEC-hot-file-policy Q-1 pins %d", p.QueueMaxDepth, DefaultHotQueueMaxDepth)
	}
	if p.QueueMaxWait <= 0 {
		return fmt.Errorf("bunker-fs: --hot.queue-max-wait must be > 0 (got %s); SPEC-hot-file-policy Q-6 pins %s (one decay step, so a stale entry has already lost 10%% of its score)", p.QueueMaxWait, DefaultHotQueueMaxWait)
	}

	// ---- the pool policy (§6) ----
	if p.RefreshMaxInflight < 1 {
		return fmt.Errorf("bunker-fs: --hot.max-concurrent-refresh must be >= 1 (got %d); SPEC-hot-file-policy Q-7 pins %d. Above the pool share the value is CLAMPED and reported (P-4), never refused — but zero refreshes is not a width", p.RefreshMaxInflight, DefaultHotRefreshMaxInflight)
	}
	if p.PoolShareNum < 1 {
		return fmt.Errorf("bunker-fs: --hot.pool-share numerator must be >= 1 (got %d/%d); SPEC-hot-file-policy P-3's floor is max(1, floor(Concurrency × share)), so a zero share is not expressible — use --hot.enabled=false to run without the hot path", p.PoolShareNum, p.PoolShareDen)
	}
	if p.PoolShareDen < 1 {
		return fmt.Errorf("bunker-fs: --hot.pool-share denominator must be >= 1 (got %d/%d); SPEC-hot-file-policy P-3 pins %d/%d", p.PoolShareNum, p.PoolShareDen, DefaultHotPoolShareNum, DefaultHotPoolShareDen)
	}
	if p.PoolShareNum > p.PoolShareDen {
		return fmt.Errorf("bunker-fs: --hot.pool-share must not exceed 1 (got %d/%d); SPEC-hot-file-policy P-3 makes the share a FRACTION of the client's own request pool, and refreshes never take a slot from the foreground semaphore", p.PoolShareNum, p.PoolShareDen)
	}
	if p.BackoffBase <= 0 {
		return fmt.Errorf("bunker-fs: --hot.backoff-base-ms must be > 0 (got %s); SPEC-hot-file-policy P-10 pins %s", p.BackoffBase, DefaultHotBackoffBase)
	}
	if p.BackoffFactor < 1 {
		return fmt.Errorf("bunker-fs: --hot.backoff-factor must be >= 1 (got %v); SPEC-hot-file-policy P-10 pins %v (exponential)", p.BackoffFactor, DefaultHotBackoffFactor)
	}
	if p.BackoffMax <= 0 {
		return fmt.Errorf("bunker-fs: --hot.backoff-max-ms must be > 0 (got %s); SPEC-hot-file-policy P-10/Appendix A.11 pins %s — the ladder's cap is a value, and a cap of zero is not \"no cap\"", p.BackoffMax, DefaultHotBackoffMax)
	}
	switch p.BackoffJitter {
	case HotJitterFull, HotJitterNone:
	default:
		return fmt.Errorf("bunker-fs: --hot.backoff-jitter must be %s or %s (got %q); SPEC-hot-file-policy P-10 pins %s (BFS-041's H-8 records the same defect one mechanism over: a jitterless backoff returns every client together)", HotJitterFull, HotJitterNone, p.BackoffJitter, HotJitterFull)
	}

	// ---- the deadlines (§5.5, §6.4) ----
	if p.RefreshDeadline <= 0 {
		return fmt.Errorf("bunker-fs: --hot.refresh-deadline must be > 0 (got %s); SPEC-hot-file-policy P-11 pins %s", p.RefreshDeadline, DefaultHotRefreshDeadline)
	}
	if p.RefreshReacquireWindow <= 0 {
		return fmt.Errorf("bunker-fs: --hot.refresh-reacquire-window must be > 0 (got %s); SPEC-hot-file-policy P-12 pins %s", p.RefreshReacquireWindow, DefaultHotRefreshReacquireWindow)
	}
	if p.YieldAfter <= 0 {
		return fmt.Errorf("bunker-fs: --hot.yield-after must be > 0 (got %s); SPEC-hot-file-policy P-7 pins %s", p.YieldAfter, DefaultHotYieldAfter)
	}
	if p.TickInterval <= 0 {
		return fmt.Errorf("bunker-fs: --hot.tick-interval must be > 0 (got %s); SPEC-hot-file-policy Q-14 pins %s", p.TickInterval, DefaultHotTickInterval)
	}
	if p.StopDeadline <= 0 {
		return fmt.Errorf("bunker-fs: --hot.stop-deadline must be > 0 (got %s); SPEC-hot-file-policy Q-12 pins %s", p.StopDeadline, DefaultHotStopDeadline)
	}
	if p.PoolPressureTicks < 1 {
		return fmt.Errorf("bunker-fs: --hot.pool-pressure-ticks must be >= 1 (got %d); SPEC-hot-file-policy P-19 pins %d", p.PoolPressureTicks, DefaultHotPoolPressureTicks)
	}
	return nil
}

// HotPolicyDerived is the part of the effective policy that is COMPUTED rather
// than configured. It exists because these are the numbers the spec makes
// load-bearing and the owner cannot see anywhere else: P-4's clamp, the share's
// slot arithmetic, and the reservation ceiling the pair implies (Q-9/P-5). A
// derived bound that is not reported is the same defect as a configured one that
// is not (PRD §2.7).
type HotPolicyDerived struct {
	// PoolSlots is max(1, floor(Concurrency × share)) — P-3. At the defaults
	// (25 slots, 1/8) it is 3.
	PoolSlots int `json:"pool_slots"`
	// PoolSlotsForeground is Concurrency − PoolSlots: the foreground's floor,
	// 22 of 25 (88%) at the defaults. It is arithmetic, not a hope, because the
	// refresh budget is a separate bounded budget (P-3/P-6).
	PoolSlotsForeground int `json:"pool_slots_foreground"`
	// RefreshMaxInflightConfigured is what the operator asked for.
	RefreshMaxInflightConfigured int `json:"refresh_max_inflight_configured"`
	// RefreshMaxInflight is min(configured, PoolSlots) — the value the
	// refresher obeys (Q-7/P-4).
	RefreshMaxInflight int `json:"refresh_max_inflight"`
	// RefreshMaxInflightClamped is true when the two differ. P-4's house rule:
	// clamped AND reported rather than refused, so the operator learns the
	// ceiling was binding without the mount being refused for asking.
	RefreshMaxInflightClamped bool `json:"refresh_max_inflight_clamped"`
	// MaxInflightBytes is RefreshMaxInflight × MaxFileBytes — Q-9/P-5's
	// reservation ceiling, and therefore the bound on the refresh's own memory
	// (P-8: a refresh may not buffer more than one file).
	MaxInflightBytes int64 `json:"max_inflight_bytes"`
	// HalfLifeMS is the effective half-life implied by Decay/DecayStep
	// (step × ln2 / −ln decay), rounded to the millisecond. At the pinned 0.9
	// per 300 s it is 1,973,600 ms ≈ 32.9 min (Appendix A.2). ZERO means
	// "no decay": Decay is 1.0, the AC-1 control arm, and there is no half-life
	// to report.
	HalfLifeMS int64 `json:"half_life_ms"`
}

// HotPolicyEffective is the resolved, runtime-readable form of the policy: what
// the mount actually obeys, with the derived numbers filled in. It is what the
// status document carries, so an operator can see the bounds a running mount is
// held to instead of reading the flags they think they passed.
type HotPolicyEffective struct {
	// Enabled is the flag as resolved.
	Enabled bool `json:"enabled"`
	// ConfigState is disabled | cache_disabled | enabled — what the
	// CONFIGURATION resolved to. The runtime states (armed|disarmed|stopped|
	// misconfigured) are BFS-037's and are reported elsewhere; conflating the
	// two would make "the operator turned it off" indistinguishable from "it
	// stopped itself under pressure".
	ConfigState string `json:"config_state"`
	// Configured is the policy as configured, every knob at its effective value.
	Configured HotPolicy `json:"configured"`
	// Derived is the computed half.
	Derived HotPolicyDerived `json:"derived"`
	// PoolShare is the configured share as a fraction ("1/8") — the same
	// spelling the flag accepts.
	PoolShare string `json:"pool_share"`
	// QueueReplacement and SizeRuleInclusive are the two policy RULES that are
	// not numbers. They are reported so "the queue displaces by score, not
	// FIFO" (Q-3) and "a file exactly at the ceiling refreshes" (S-2) are
	// visible facts about a running mount rather than prose in a spec.
	QueueReplacement  string `json:"queue_replacement"`
	SizeRuleInclusive bool   `json:"size_rule_inclusive"`
	// Misconfigured lists the RELATION failures this configuration carries,
	// each naming both numbers. It is empty for a coherent policy. A non-empty
	// list means the hot path cannot be honoured as written; when the feature is
	// ENABLED that same list is a mount refusal (Validate), and when it is not,
	// S-11 requires the mount to work and the hot path to report itself — which
	// is this list, beside the state. An unreported misconfiguration would be
	// exactly the kind of bound nobody can see that PRD §2.7 forbids.
	Misconfigured []string `json:"misconfigured,omitempty"`
}

// Effective resolves the policy against the surrounding configuration: it
// applies P-4's clamp (and reports it), computes the share's slot arithmetic,
// the reservation ceiling and the decay half-life, and names the configuration
// state. It performs no validation — call Validate first; a policy that cannot
// be honoured must refuse the mount rather than be resolved into something
// quieter.
func (p HotPolicy) Effective(env HotPolicyEnv) HotPolicyEffective {
	share := p.PoolShare()
	slots := 1
	if env.Concurrency > 0 && p.PoolShareDen > 0 {
		if s := env.Concurrency * p.PoolShareNum / p.PoolShareDen; s > 1 {
			slots = s
		}
	}
	inflight := p.RefreshMaxInflight
	clamped := false
	if slots > 0 && inflight > slots {
		inflight = slots
		clamped = true
	}
	foreground := env.Concurrency - slots
	if foreground < 0 {
		foreground = 0
	}
	eff := HotPolicyEffective{
		Enabled:           p.Enabled,
		ConfigState:       HotConfigDisabled,
		Configured:        p,
		PoolShare:         share,
		QueueReplacement:  HotQueueReplacementScore,
		SizeRuleInclusive: HotSizeRuleInclusive,
		Misconfigured:     p.RelationProblems(env),
		Derived: HotPolicyDerived{
			PoolSlots:                    slots,
			PoolSlotsForeground:          foreground,
			RefreshMaxInflightConfigured: p.RefreshMaxInflight,
			RefreshMaxInflight:           inflight,
			RefreshMaxInflightClamped:    clamped,
			MaxInflightBytes:             int64(inflight) * p.MaxFileBytes,
			HalfLifeMS:                   halfLifeMS(p.Decay, p.DecayStep),
		},
	}
	switch {
	case len(eff.Misconfigured) > 0:
		// S-11: this is NOT a broken mount. The state is named and the two
		// numbers travel with it; an ENABLED policy never reaches here, because
		// Validate refuses it instead.
		eff.ConfigState = HotConfigMisconfigured
	case !p.Enabled:
		eff.ConfigState = HotConfigDisabled
	case env.CacheMaxBytes == 0:
		// D-6: no cache, no warming — reported as a skip reason (S-12), never
		// as a refusal (S-11).
		eff.ConfigState = HotConfigCacheDisabled
	default:
		eff.ConfigState = HotConfigEnabled
	}
	return eff
}

// halfLifeMS returns the effective half-life of a decay factor applied per step,
// in milliseconds: step × ln(0.5) / ln(decay). Zero means the decay is 1.0 —
// no decay at all (the AC-1 control arm), where a half-life does not exist.
func halfLifeMS(decay float64, step time.Duration) int64 {
	if decay >= 1 || decay <= 0 || step <= 0 {
		return 0
	}
	return int64(math.Round(float64(step.Milliseconds()) * math.Log(0.5) / math.Log(decay)))
}
