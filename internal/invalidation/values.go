package invalidation

import "fmt"

// Source values for the read-back's per-knob `source` field. The source is
// DERIVED by comparing the value to the table's declared default — there is no
// separate bookkeeping that could drift from the number it describes.
const (
	// SourceDefault means the value in force IS the declared default.
	SourceDefault = "declared_default"
	// SourceOperator means the value in force differs from the declared
	// default, i.e. an operator wrote it.
	SourceOperator = "operator"
)

// WatchValues is the resolved watcher block: every field has a concrete value,
// and every one of them is a knob the running watcher obeys (see
// internal/server/webdav's watchOptions, which is built from exactly these
// fields).
type WatchValues struct {
	Enabled         bool
	HeartbeatMS     int
	InstallHeadroom int
	FlushEveryMS    int
	FlushMaxPaths   int
	ScanLimit       int
	// MaxWatches is the requested watch ceiling; 0 means AUTO (the platform's
	// own ceiling). It is the one knob whose effective value depends on the
	// platform rather than on the file, which is why it is the one knob that
	// can be UNHONOURABLE (§4.2) rather than merely invalid.
	MaxWatches int64
}

// PushValues is the resolved push block. Nothing in this build consumes it yet
// (BFS-036 owns the wire form), which the read-back reports as applied:false
// rather than as a value in force.
type PushValues struct {
	SubscriberBufferBytes  int64
	SubscriberBufferEvents int
	MaxSubscribers         int
	WriteDeadlineMS        int
	MaxEventBytes          int64
}

// Values is the resolved surface: the numbers the code obeys, each with its
// declared default and range available from the table by the knob's own name.
type Values struct {
	Watch WatchValues
	Push  PushValues
}

// DefaultValues is the declared surface: every knob at its declared default.
// It is what a deployment with no invalidation block gets, and none of these
// numbers turns a channel off (the poll form is not behind a knob at all).
func DefaultValues() Values {
	return Values{
		Watch: WatchValues{
			Enabled:         DefaultWatchEnabled,
			HeartbeatMS:     DefaultWatchHeartbeatMS,
			InstallHeadroom: DefaultWatchInstallHeadroom,
			FlushEveryMS:    DefaultWatchFlushEveryMS,
			FlushMaxPaths:   DefaultWatchFlushMaxPaths,
			ScanLimit:       DefaultWatchScanLimit,
			MaxWatches:      DefaultWatchMaxWatches,
		},
		Push: PushValues{
			SubscriberBufferBytes:  DefaultPushBufferBytes,
			SubscriberBufferEvents: DefaultPushBufferEvents,
			MaxSubscribers:         DefaultPushMaxSubscribers,
			WriteDeadlineMS:        DefaultPushWriteDeadlineMS,
			MaxEventBytes:          DefaultPushMaxEventBytes,
		},
	}
}

// KnobValue reads one knob's value out of the resolved surface, by its declared
// name. It is the single accessor the validator and the read-back share, so the
// number that is checked and the number that is reported are the same number.
func (v Values) KnobValue(name string) (int64, bool) {
	switch name {
	case KnobWatchEnabled:
		return boolValue(v.Watch.Enabled), true
	case KnobWatchHeartbeatMS:
		return int64(v.Watch.HeartbeatMS), true
	case KnobWatchHeadroom:
		return int64(v.Watch.InstallHeadroom), true
	case KnobWatchFlushEveryMS:
		return int64(v.Watch.FlushEveryMS), true
	case KnobWatchFlushMaxPaths:
		return int64(v.Watch.FlushMaxPaths), true
	case KnobWatchScanLimit:
		return int64(v.Watch.ScanLimit), true
	case KnobWatchMaxWatches:
		return v.Watch.MaxWatches, true
	case KnobPushBufferBytes:
		return v.Push.SubscriberBufferBytes, true
	case KnobPushBufferEvents:
		return int64(v.Push.SubscriberBufferEvents), true
	case KnobPushMaxSubscribers:
		return int64(v.Push.MaxSubscribers), true
	case KnobPushWriteDeadlineMS:
		return int64(v.Push.WriteDeadlineMS), true
	case KnobPushMaxEventBytes:
		return v.Push.MaxEventBytes, true
	}
	return 0, false
}

func boolValue(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Validate refuses every value outside its declared range, by name, and every
// relation the specs fix between two knobs. It never substitutes a default: a
// value that cannot be obeyed must stop the surface, not be quietly replaced
// (that is BFS-031/BFS-032's defect class — a bound that is reported and not
// enforced).
func (v Values) Validate() error {
	for _, k := range Knobs() {
		got, ok := v.KnobValue(k.Name)
		if !ok {
			// A declared knob with no value in this surface is an internal
			// inconsistency, not an operator error: the table and the accessor
			// have drifted, and the read-back would report a number nobody
			// obeys. Loud, and it is pinned by a test over the whole table.
			return fmt.Errorf("internal: the declared knob %s has no value in this surface (the table and the accessor have drifted)", k.Name)
		}
		if got < k.Min || got > k.Max {
			return rangeErr(k, got)
		}
	}
	// SPEC-push-channel §4.4: push_write_deadline < heartbeat_ms, so a stalled
	// subscriber is detected within one heartbeat period rather than
	// accumulating. Both knobs are individually in range in this case, which is
	// exactly why the relation has to be checked separately.
	if v.Push.WriteDeadlineMS >= v.Watch.HeartbeatMS {
		return fmt.Errorf("%s: %d ms is not below %s (%d ms) — a stalled subscriber must be detected within one heartbeat period (SPEC-push-channel §4.4: push_write_deadline < heartbeat_ms); the pair is refused rather than adjusted",
			KnobPushWriteDeadlineMS, v.Push.WriteDeadlineMS, KnobWatchHeartbeatMS, v.Watch.HeartbeatMS)
	}
	return nil
}

// Row is one knob's runtime read-back: the value in force, the declared default
// and range it is judged against, where the value came from, and whether a
// running component obeys it right now. `value` is never the REQUESTED number:
// the surface fills it from the running watcher's own options when a watcher
// exists (see the webdav read-back), because a config surface that reports
// values it does not obey is the defect this row exists to close.
type Row struct {
	Knob          string `json:"knob"`
	Value         int64  `json:"value"`
	Default       int64  `json:"default"`
	Min           int64  `json:"min"`
	Max           int64  `json:"max"`
	Unit          Unit   `json:"unit"`
	Source        string `json:"source"`
	Applied       bool   `json:"applied"`
	AppliedReason string `json:"applied_reason,omitempty"`
}

// Rows returns one Row per declared knob, in table order, with the values of
// this surface. Applied is left false: whether a running component obeys a knob
// is a fact only the running surface can report, so the caller sets it.
func (v Values) Rows() []Row {
	out := make([]Row, 0, len(Knobs()))
	for _, k := range Knobs() {
		got, _ := v.KnobValue(k.Name)
		source := SourceDefault
		if got != k.Default {
			source = SourceOperator
		}
		out = append(out, Row{
			Knob: k.Name, Value: got, Default: k.Default, Min: k.Min, Max: k.Max,
			Unit: k.Unit, Source: source,
		})
	}
	return out
}

// Spec is the operator-facing block: every field is a POINTER so that "the key
// was not written" (nil, and the declared default applies — a fact about the
// file) is distinguishable from "the key was written" (validated strictly,
// including a zero). That distinction is the whole difference between a knob
// that reports a bound and one that quietly drops a typo'd limit.
//
// The mapstructure tags are the yaml keys under server.invalidation.*; they
// carry no dependency, so this package is a leaf (internal/config imports it to
// validate at load, internal/server/webdav imports it to obey and to report).
type Spec struct {
	Watch WatchSpec `mapstructure:"watch"`
	Push  PushSpec  `mapstructure:"push"`
}

// WatchSpec is the watcher half of the operator's block.
type WatchSpec struct {
	Enabled         *bool  `mapstructure:"enabled"`
	HeartbeatMS     *int   `mapstructure:"heartbeat_ms"`
	InstallHeadroom *int   `mapstructure:"install_headroom"`
	FlushEveryMS    *int   `mapstructure:"flush_every_ms"`
	FlushMaxPaths   *int   `mapstructure:"flush_max_paths"`
	ScanLimit       *int   `mapstructure:"scan_limit"`
	MaxWatches      *int64 `mapstructure:"max_watches"`
}

// PushSpec is the push half of the operator's block.
type PushSpec struct {
	SubscriberBufferBytes  *int64 `mapstructure:"subscriber_buffer_bytes"`
	SubscriberBufferEvents *int   `mapstructure:"subscriber_buffer_events"`
	MaxSubscribers         *int   `mapstructure:"max_subscribers"`
	WriteDeadlineMS        *int   `mapstructure:"write_deadline_ms"`
	MaxEventBytes          *int64 `mapstructure:"max_event_bytes"`
}

// IsZero reports whether the operator wrote nothing at all (every key absent).
// An empty mapping (`invalidation: {}`) is NOT zero in this sense — it is a
// block that was written and left empty — but both resolve to the declared
// defaults, because no key means no value to refuse.
func (s Spec) IsZero() bool {
	return s.Watch == WatchSpec{} && s.Push == PushSpec{}
}

// Resolve applies the declared defaults to every absent key and validates every
// written one, returning the surface the code obeys. It returns the first
// refusal, naming the knob, the value and the range; it never returns a surface
// with a substituted value.
func (s Spec) Resolve() (Values, error) {
	v := DefaultValues()
	if s.Watch.Enabled != nil {
		v.Watch.Enabled = *s.Watch.Enabled
	}
	if s.Watch.HeartbeatMS != nil {
		v.Watch.HeartbeatMS = *s.Watch.HeartbeatMS
	}
	if s.Watch.InstallHeadroom != nil {
		v.Watch.InstallHeadroom = *s.Watch.InstallHeadroom
	}
	if s.Watch.FlushEveryMS != nil {
		v.Watch.FlushEveryMS = *s.Watch.FlushEveryMS
	}
	if s.Watch.FlushMaxPaths != nil {
		v.Watch.FlushMaxPaths = *s.Watch.FlushMaxPaths
	}
	if s.Watch.ScanLimit != nil {
		v.Watch.ScanLimit = *s.Watch.ScanLimit
	}
	if s.Watch.MaxWatches != nil {
		v.Watch.MaxWatches = *s.Watch.MaxWatches
	}
	if s.Push.SubscriberBufferBytes != nil {
		v.Push.SubscriberBufferBytes = *s.Push.SubscriberBufferBytes
	}
	if s.Push.SubscriberBufferEvents != nil {
		v.Push.SubscriberBufferEvents = *s.Push.SubscriberBufferEvents
	}
	if s.Push.MaxSubscribers != nil {
		v.Push.MaxSubscribers = *s.Push.MaxSubscribers
	}
	if s.Push.WriteDeadlineMS != nil {
		v.Push.WriteDeadlineMS = *s.Push.WriteDeadlineMS
	}
	if s.Push.MaxEventBytes != nil {
		v.Push.MaxEventBytes = *s.Push.MaxEventBytes
	}
	if err := v.Validate(); err != nil {
		return Values{}, err
	}
	return v, nil
}

// WatchCeiling is the one platform-relative decision in the surface: what the
// deployment ASKED for, what the platform GIVES, what the tree NEEDS, and which
// of those is the ceiling actually in force. It lives here (not in the watcher)
// so the arithmetic that decides "this value cannot be honoured" has exactly
// one implementation and can be read without a watcher.
type WatchCeiling struct {
	// Requested is the configured watch ceiling; 0 means AUTO (the platform's
	// own ceiling), which is why it is reported as a separate number rather
	// than folded into Effective.
	Requested int64
	// Platform is the platform's own ceiling (fs.inotify.max_user_watches), or
	// 0 when it could not be read — an unreadable ceiling never binds, because
	// a preread that failed is not evidence of a smaller limit.
	Platform int64
	// Desired is the number of directories the watch set wants, and Headroom
	// the declared reserve on top of it (§4.2's non-zero headroom).
	Desired  int64
	Headroom int64
}

// Effective is the watch ceiling actually in force: the smaller of the
// requested value (when one was written) and the platform's own ceiling. 0
// means "no ceiling could be read", which never binds.
func (c WatchCeiling) Effective() int64 {
	switch {
	case c.Requested <= 0:
		return c.Platform
	case c.Platform <= 0:
		return c.Requested
	case c.Requested < c.Platform:
		return c.Requested
	}
	return c.Platform
}

// Need is the number of watches the install requires: the tree's directories
// plus the declared headroom.
func (c WatchCeiling) Need() int64 { return c.Desired + c.Headroom }

// Fits reports whether the watch set plus its headroom fits under the ceiling
// in force. A ceiling of 0 (nothing was readable and nothing was requested)
// never binds: the add loop's own errno classification is then the
// authoritative signal, exactly as §4.2 mandates.
func (c WatchCeiling) Fits() bool {
	eff := c.Effective()
	if eff <= 0 {
		return true
	}
	return c.Need() <= eff
}

// Unhonoured returns the configured-vs-observed pair when the deployment asked
// for a ceiling the platform cannot give it, and nil when it did not. It is
// reported whether or not it binds: a value that was not honoured is a fact
// about the deployment even when the tree happens to still fit, and the
// operator's question ("did my number take effect?") has to have an answer that
// does not depend on how big the tree is today.
func (c WatchCeiling) Unhonoured() *Unhonoured {
	if c.Requested > 0 && c.Platform > 0 && c.Requested > c.Platform {
		return &Unhonoured{
			Knob:       KnobWatchMaxWatches,
			Configured: c.Requested,
			Observed:   c.Platform,
			Detail: fmt.Sprintf("the deployment asked for a watch ceiling of %d but the platform's own ceiling is %d, so %d is the ceiling in force; the per-user watch total in use is not observable, so the platform's ceiling cannot be raised from inside this process (the remedy is an operator action on fs.inotify.max_user_watches)",
				c.Requested, c.Platform, c.Effective()),
		}
	}
	if !c.Fits() {
		// The ceiling in force is what was asked for (the platform gives at
		// least that much), and the number that does not fit is the work's own
		// need: configured = the ceiling, observed = what the tree needs.
		return &Unhonoured{
			Knob:       KnobWatchMaxWatches,
			Configured: c.Effective(),
			Observed:   c.Need(),
			Detail: fmt.Sprintf("the watch set needs %d watches (%d directories plus %d headroom) and the ceiling in force is %d, so the value cannot be honoured for this tree",
				c.Need(), c.Desired, c.Headroom, c.Effective()),
		}
	}
	return nil
}
