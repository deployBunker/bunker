// Package invalidation is the SERVER-SIDE invalidation config surface: the
// declared knob table (name, default, range, unit, failure mode) for the
// server-side watcher (BFS-035) and for the push channel (BFS-036), the strict
// resolution of an operator's block into the Values the running code obeys, and
// the read-back rows the surface reports at runtime (BFS-043).
//
// Three rules shape everything here, and each of them closes a defect class
// this repository has already paid for:
//
//  1. DEFAULTS ARE THE SAFE ONES. Every knob's default is a value the code can
//     actually honour, and none of them turns invalidation off: the poll form
//     (X-Bunker-Op: events) is not behind any knob at all, and the watcher's own
//     switch defaults off because turning it on changes the SERVED REVISION's
//     kind for a git tree (BFS-035's deliberate opt-in), not because a quiet
//     default was convenient.
//
//  2. AN INVALID VALUE FAILS LOUDLY. A written key is obeyed or refused —
//     never replaced by a default (BFS-031's bound that did not bound, BFS-032's
//     counter that could never move: both are a value that was reported and not
//     enforced). This is why Spec's fields are pointers: nil means "the key was
//     not written" (and takes the declared default, which is a fact about the
//     file), while a written key — including 0 — is validated and refused by
//     name, with the value and the range in the message.
//
//  3. THE REPORTED VALUE IS THE ENFORCED VALUE. Knobs(), Values.KnobValue and
//     Values.Rows() read the same table the validator reads, so a range can
//     never be documented one way and enforced another, and the runtime
//     read-back cannot report a value the code does not obey.
//
// A value the PLATFORM cannot honour (a kernel watch ceiling below what the
// deployment asked for) is not a config error and is not silently clamped: it
// is reported as an Unhonoured pair — configured AND observed — so an operator
// can tell "you asked for 8192 watches and this kernel gives 128" from "the
// watcher is broken".
package invalidation

import "fmt"

// Unit labels a knob's numbers so an operator reading the read-back knows what
// the range means (ms vs bytes vs count).
type Unit string

const (
	UnitBool  Unit = "bool"
	UnitMS    Unit = "ms"
	UnitBytes Unit = "bytes"
	UnitCount Unit = "count"
)

// The declared knob names. They are the SAME string an operator writes in the
// config file, the string a bad value is refused under, and the key the runtime
// read-back reports under — one spelling, three consumers, so a knob cannot be
// configured under one name and reported under another.
const (
	KnobWatchEnabled       = "server.invalidation.watch.enabled"
	KnobWatchHeartbeatMS   = "server.invalidation.watch.heartbeat_ms"
	KnobWatchHeadroom      = "server.invalidation.watch.install_headroom"
	KnobWatchFlushEveryMS  = "server.invalidation.watch.flush_every_ms"
	KnobWatchFlushMaxPaths = "server.invalidation.watch.flush_max_paths"
	KnobWatchScanLimit     = "server.invalidation.watch.scan_limit"
	KnobWatchMaxWatches    = "server.invalidation.watch.max_watches"

	KnobPushBufferBytes     = "server.invalidation.push.subscriber_buffer_bytes"
	KnobPushBufferEvents    = "server.invalidation.push.subscriber_buffer_events"
	KnobPushMaxSubscribers  = "server.invalidation.push.max_subscribers"
	KnobPushWriteDeadlineMS = "server.invalidation.push.write_deadline_ms"
	KnobPushMaxEventBytes   = "server.invalidation.push.max_event_bytes"
)

// Declared defaults. Every one is a value the code can honour and none of them
// disables a channel; where the spec fixes the number, the comment names it.
const (
	// DefaultWatchEnabled is false, and that is a CONTRACT default rather than
	// a quiet one: enabling the watcher changes the served revision's kind for
	// a git tree (`git` -> `git+watch`, SPEC-watcher-capability §7.2 R-V3), an
	// operator decision by design (BFS-035). It does NOT default invalidation
	// off — the poll form is not behind this knob and cannot be switched off.
	DefaultWatchEnabled = false
	// DefaultWatchHeartbeatMS is SPEC-push-channel §4.4's inherited declaration
	// (`heartbeat_ms <= 30000`); the client's 90 s dead-channel rule is derived
	// from it (three missed beats), so this is the number the watcher's own
	// heartbeats AND the stream's declared period must both use.
	DefaultWatchHeartbeatMS = 30000
	// DefaultWatchInstallHeadroom is SPEC-watcher-capability §4.2's REQUIRED,
	// reported, non-zero headroom: the tree grows and the per-user watch total
	// in use is not observable, so "we fit exactly" is not a claim this build
	// makes.
	DefaultWatchInstallHeadroom = 512
	// DefaultWatchFlushEveryMS coalesces a burst into one path list before the
	// watcher flushes it (the value BFS-035 landed with).
	DefaultWatchFlushEveryMS = 50
	// DefaultWatchFlushMaxPaths is aligned with the surface's declared path
	// bound (§3 E-6 declaration 2 = 4096 = eventsMaxPathsPerEvent): above it a
	// flush is a full rescan, never a longer or partial list. The range's
	// maximum is the same number, so the knob cannot raise it past the bound
	// the document declares to clients.
	DefaultWatchFlushMaxPaths = 4096
	// DefaultWatchScanLimit is the install/rescan walk bound, aligned with the
	// poll form's own observation bound (eventsScanLimit).
	DefaultWatchScanLimit = 100000
	// DefaultWatchMaxWatches is 0, the documented AUTO sentinel: use the
	// platform's own ceiling (fs.inotify.max_user_watches). It is not a silent
	// fallback — 0 is an explicit "let the platform decide", and the read-back
	// reports the platform's number beside it.
	DefaultWatchMaxWatches = 0

	// DefaultPushBufferBytes is SPEC-push-channel §6.1's asserted default (B-7).
	DefaultPushBufferBytes = 4 << 20
	// DefaultPushBufferEvents equals the journal bound (256, inherited), per
	// §6.1: a subscriber that has fallen a whole journal behind resyncs anyway.
	DefaultPushBufferEvents = 256
	// DefaultPushMaxSubscribers is this row's choice (the spec hands the value
	// to BFS-043): 8 subscribers is far above the one-agent-per-bunker case and
	// bounds the declared worst-case memory at 8 x 4 MiB = 32 MiB.
	DefaultPushMaxSubscribers = 8
	// DefaultPushWriteDeadlineMS is a third of a heartbeat: a stalled
	// subscriber is detected well inside one heartbeat period (§4.4's relation
	// is `push_write_deadline < heartbeat_ms`; Validate enforces it).
	DefaultPushWriteDeadlineMS = 10000
	// DefaultPushMaxEventBytes is §7.2's asserted default. It is bounded above
	// by the landed consumer's own per-line read cap (8 MiB, the client's
	// scanner.Buffer): declaring more than a client can read is the
	// reconnect-loop defect §7.2 measures.
	DefaultPushMaxEventBytes = 1 << 20
)

// Declared ranges. Every range excludes the Go zero value (except the AUTO
// sentinel of max_watches), which is what makes "the key was written with a
// zero" a loud refusal instead of a silent default.
const (
	MinWatchHeartbeatMS = 100
	MaxWatchHeartbeatMS = DefaultWatchHeartbeatMS

	MinWatchHeadroom = 1
	MaxWatchHeadroom = 1 << 20

	MinWatchFlushEveryMS = 1
	MaxWatchFlushEveryMS = 60000

	MinWatchFlushMaxPaths = 1
	MaxWatchFlushMaxPaths = DefaultWatchFlushMaxPaths

	MinWatchScanLimit = 1
	MaxWatchScanLimit = 1 << 31

	MinWatchMaxWatches = DefaultWatchMaxWatches
	MaxWatchMaxWatches = 1 << 31

	MinPushBufferBytes = 64 << 10
	MaxPushBufferBytes = 1 << 30

	MinPushBufferEvents = 1
	MaxPushBufferEvents = 65536

	MinPushMaxSubscribers = 1
	MaxPushMaxSubscribers = 4096

	MinPushWriteDeadlineMS = 100
	MaxPushWriteDeadlineMS = MaxWatchHeartbeatMS - 1

	MinPushMaxEventBytes = 64 << 10
	MaxPushMaxEventBytes = 8 << 20
)

// Knob is one declared knob. The table is the single source of truth: the
// validator refuses outside [Min, Max], the read-back reports the same numbers,
// and Failure states what an invalid value does — so the documented behaviour
// and the enforced behaviour are the same object.
type Knob struct {
	Name    string
	Default int64
	Min     int64
	Max     int64
	Unit    Unit
	// Failure is the declared failure mode: what happens to an invalid value.
	// Every numeric knob's is the same law (refuse by name, never fall back),
	// spelled per knob so the table reads as a contract rather than as a rule
	// the reader has to infer.
	Failure string
	// Why is the justification for the default, with its spec citation.
	Why string
	// AutoSentinel marks the one knob whose zero value is a DOCUMENTED
	// sentinel — max_watches' 0 means "let the platform decide" — rather than a
	// value. Every other numeric knob's range excludes 0, so a key written as 0
	// is a refusal and never a silent default.
	AutoSentinel bool
}

// refuse is the one failure mode every numeric knob shares: the value is
// refused, by name, with the range and the value in the message, and the
// declared default is never substituted for it.
const refuse = "refused by name at config load and at surface construction; never replaced by the declared default"

// Knobs returns the declared table, in reporting order (watcher first, then
// push), matching the order of Values.Rows().
func Knobs() []Knob {
	return []Knob{
		{Name: KnobWatchEnabled, Unit: UnitBool, Default: 0, Min: 0, Max: 1,
			Failure: "refused by the mapping decoder when it is not a boolean; the zero value IS the declared default (false), and false is what the surface is built with",
			Why:     "BFS-035: enabling the watcher changes the served revision's kind for a git tree, so it is an operator opt-in; the poll form is not behind this knob and cannot be switched off"},
		{Name: KnobWatchHeartbeatMS, Unit: UnitMS, Default: DefaultWatchHeartbeatMS, Min: MinWatchHeartbeatMS, Max: MaxWatchHeartbeatMS,
			Failure: refuse,
			Why:     "SPEC-push-channel §4.4: the inherited declaration is heartbeat_ms <= 30000, and the client's 90 s dead-channel rule is three missed beats"},
		{Name: KnobWatchHeadroom, Unit: UnitCount, Default: DefaultWatchInstallHeadroom, Min: MinWatchHeadroom, Max: MaxWatchHeadroom,
			Failure: refuse,
			Why:     "SPEC-watcher-capability §4.2: headroom is required, reported and non-zero, because the per-user watch total in use is not observable"},
		{Name: KnobWatchFlushEveryMS, Unit: UnitMS, Default: DefaultWatchFlushEveryMS, Min: MinWatchFlushEveryMS, Max: MaxWatchFlushEveryMS,
			Failure: refuse,
			Why:     "BFS-035: one coalescing window per flush, so a burst becomes one sorted path list"},
		{Name: KnobWatchFlushMaxPaths, Unit: UnitCount, Default: DefaultWatchFlushMaxPaths, Min: MinWatchFlushMaxPaths, Max: MaxWatchFlushMaxPaths,
			Failure: refuse,
			Why:     "aligned with the declared bound max_paths_per_event = 4096 (BFS-004 §3 E-6 declaration 2): above it a flush is an overflow, never a longer or partial list"},
		{Name: KnobWatchScanLimit, Unit: UnitCount, Default: DefaultWatchScanLimit, Min: MinWatchScanLimit, Max: MaxWatchScanLimit,
			Failure: refuse,
			Why:     "aligned with the poll form's own observation bound (eventsScanLimit): a walk that hits it cannot be claimed as complete coverage"},
		{Name: KnobWatchMaxWatches, Unit: UnitCount, Default: DefaultWatchMaxWatches, Min: MinWatchMaxWatches, Max: MaxWatchMaxWatches,
			Failure:      "0 is the AUTO sentinel (the platform's own ceiling); a positive value above what the platform gives is reported as an unhonourable value with configured AND observed, never clamped in silence",
			Why:          "SPEC-watcher-capability §4.2: the per-user watch total in use is not observable, so the ceiling in force must be declared rather than assumed",
			AutoSentinel: true},
		{Name: KnobPushBufferBytes, Unit: UnitBytes, Default: DefaultPushBufferBytes, Min: MinPushBufferBytes, Max: MaxPushBufferBytes,
			Failure: refuse,
			Why:     "SPEC-push-channel §6.1 B-7: every subscriber has its own buffer, bounded in bytes AND events, both declared and countable"},
		{Name: KnobPushBufferEvents, Unit: UnitCount, Default: DefaultPushBufferEvents, Min: MinPushBufferEvents, Max: MaxPushBufferEvents,
			Failure: refuse,
			Why:     "SPEC-push-channel §6.1: the event bound equals the journal bound, because a subscriber that far behind resyncs anyway"},
		{Name: KnobPushMaxSubscribers, Unit: UnitCount, Default: DefaultPushMaxSubscribers, Min: MinPushMaxSubscribers, Max: MaxPushMaxSubscribers,
			Failure: refuse,
			Why:     "SPEC-push-channel §6.4 B-12: a declared maximum subscriber count with a declared worst-case memory (max_subscribers x subscriber_buffer_bytes)"},
		{Name: KnobPushWriteDeadlineMS, Unit: UnitMS, Default: DefaultPushWriteDeadlineMS, Min: MinPushWriteDeadlineMS, Max: MaxPushWriteDeadlineMS,
			Failure: refuse + "; VALIDATED AGAINST heartbeat_ms as well (the value must be strictly below it)",
			Why:     "SPEC-push-channel §4.4/§5.3: push_write_deadline < heartbeat_ms, so a stalled subscriber is detected within one heartbeat period"},
		{Name: KnobPushMaxEventBytes, Unit: UnitBytes, Default: DefaultPushMaxEventBytes, Min: MinPushMaxEventBytes, Max: MaxPushMaxEventBytes,
			Failure: refuse,
			Why:     "SPEC-push-channel §7.2 B-15: the declared byte bound per line; the landed consumer's own read cap is 8 MiB, so declaring more would produce the reconnect loop that section measures"},
	}
}

// KnobByName returns the declared knob with that name.
func KnobByName(name string) (Knob, bool) {
	for _, k := range Knobs() {
		if k.Name == name {
			return k, true
		}
	}
	return Knob{}, false
}

// rangeErr is the loud refusal: the knob's name, the value, the range, the
// unit, and the explicit statement that the default was NOT substituted.
func rangeErr(k Knob, got int64) error {
	return fmt.Errorf("%s: %d is outside the declared range [%d, %d] %s — refused rather than replaced by the declared default %d (fix the value, or remove the key to take the default)",
		k.Name, got, k.Min, k.Max, k.Unit, k.Default)
}

// Unhonoured is one value the deployment asked for that the platform could not
// give it. Both numbers are always present: `configured` is what the config
// asked for and `observed` is what the platform provides (or what the work
// needs, when the need is the binding number — `detail` says which), so an
// operator can tell "you asked for 8192 watches and this kernel gives 128" from
// "the watcher is broken".
type Unhonoured struct {
	Knob       string `json:"knob"`
	Configured int64  `json:"configured"`
	Observed   int64  `json:"observed"`
	Detail     string `json:"detail"`
}

// String renders the pair as the one line an operator reads in a log.
func (u Unhonoured) String() string {
	return fmt.Sprintf("%s: configured %d, observed %d — %s", u.Knob, u.Configured, u.Observed, u.Detail)
}
