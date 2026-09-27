package webdav

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/deployBunker/bunker/internal/invalidation"
)

// ---------------------------------------------------------------------------
// BFS-043: the invalidation config surface's own cells.
//
// Two rules drive everything here:
//
//  1. A VALUE THAT CANNOT BE OBEYED IS LOUD. An out-of-range value stops the
//     surface with the knob named; a value the PLATFORM cannot give is reported
//     as configured-vs-observed.
//  2. THE READ-BACK REPORTS WHAT THE CODE OBEYS. So every arm below that reads a
//     number out of the document is paired with an arm that makes the CODE show
//     the same number — a config surface that reports values it does not obey is
//     exactly the defect this row is about.
// ---------------------------------------------------------------------------

// invalidationKnobRow is one row of the runtime read-back, as the WIRE carries
// it (decoded from the capabilities op's answer, never from the Go value that
// produced it).
type invalidationKnobRow struct {
	Knob          string `json:"knob"`
	Value         any    `json:"value"`
	Default       any    `json:"default"`
	Min           int64  `json:"min"`
	Max           int64  `json:"max"`
	Unit          string `json:"unit"`
	Source        string `json:"source"`
	Applied       bool   `json:"applied"`
	AppliedReason string `json:"applied_reason"`
}

type invalidationConfigDoc struct {
	Surface     string                    `json:"surface"`
	ReadAt      string                    `json:"read_at"`
	AppliedFrom string                    `json:"applied_from"`
	Knobs       []invalidationKnobRow     `json:"knobs"`
	Ceiling     map[string]any            `json:"ceiling"`
	Unhonoured  []invalidation.Unhonoured `json:"unhonoured"`
	Declared    string                    `json:"declared"`
}

// invalidationWire is the parts of §8.2's block these cells assert on.
type invalidationWire struct {
	HeartbeatMS int     `json:"heartbeat_ms"`
	State       string  `json:"state"`
	Reason      *string `json:"reason"`
	BlocksPush  bool    `json:"blocks_push"`
	Counters    *struct {
		HeartbeatsTotal uint64 `json:"heartbeats_total"`
		RescansTotal    uint64 `json:"rescans_total"`
		OverflowsTotal  uint64 `json:"overflows_total"`
	} `json:"counters"`
	Coverage *struct {
		Headroom           int `json:"headroom"`
		DirectoriesDesired int `json:"directories_desired"`
		DirectoriesWatched int `json:"directories_watched"`
	} `json:"coverage"`
	Limits struct {
		Watching map[string]any `json:"watching"`
	} `json:"limits"`
	Config invalidationConfigDoc `json:"config"`
}

type invalidationRefusal struct {
	OK      bool   `json:"ok"`
	Verdict string `json:"verdict"`
	Error   *struct {
		Capability string                   `json:"capability"`
		Scope      string                   `json:"scope"`
		Mode       string                   `json:"mode"`
		Reason     string                   `json:"reason"`
		Unhonoured *invalidation.Unhonoured `json:"unhonoured"`
		Detail     string                   `json:"detail"`
	} `json:"error"`
}

// readInvalidationWire drives the capabilities op and decodes the watch block.
func readInvalidationWire(t *testing.T, h *Handler) invalidationWire {
	t.Helper()
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "capabilities"}, "")
	var caps struct {
		Result struct {
			Capabilities struct {
				Extensions struct {
					Watch invalidationWire `json:"watch"`
				} `json:"extensions"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatalf("capabilities envelope is not JSON: %v (%s)", err, rec.Body.String())
	}
	return caps.Result.Capabilities.Extensions.Watch
}

// readWatchRefusal drives the watch op and decodes its refusal.
func readWatchRefusal(t *testing.T, h *Handler) invalidationRefusal {
	t.Helper()
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "watch"}, "")
	var out invalidationRefusal
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("watch refusal is not JSON: %v (%s)", err, rec.Body.String())
	}
	if out.Error == nil {
		t.Fatalf("the watch op answered without an error object: %s", rec.Body.String())
	}
	return out
}

// knobRow finds one knob's row, failing when the read-back does not mention it.
func knobRow(t *testing.T, w invalidationWire, name string) invalidationKnobRow {
	t.Helper()
	for _, r := range w.Config.Knobs {
		if r.Knob == name {
			return r
		}
	}
	t.Fatalf("the read-back has no row for %s: a knob the config accepts with no read-back is invisible at runtime", name)
	return invalidationKnobRow{}
}

// asInt reads a JSON number (or bool) as the table's int64.
func asInt(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return int64(n)
	case bool:
		if n {
			return 1
		}
		return 0
	case int64:
		return n
	case int:
		return int64(n)
	}
	t.Fatalf("read-back value %#v is not a number or a bool", v)
	return 0
}

// invalidationConfig builds a valid Values by mutating the declared defaults, so
// every cell states only the knob it is about.
func invalidationConfig(t *testing.T, mutate func(*invalidation.Values)) invalidation.Values {
	t.Helper()
	v := invalidation.DefaultValues()
	if mutate != nil {
		mutate(&v)
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("the cell's own config is invalid: %v", err)
	}
	return v
}

// ---------------------------------------------------------------------------
// 1. AN INVALID VALUE IS LOUD (and is NOT replaced by a default)
// ---------------------------------------------------------------------------

func TestInvalidationConfigRefusesInvalidValuesAtConstruction(t *testing.T) {
	cases := []struct {
		name    string
		knob    string
		mutate  func(*invalidation.Values)
		wantSub string
	}{
		{"heartbeat zero", invalidation.KnobWatchHeartbeatMS, func(v *invalidation.Values) { v.Watch.HeartbeatMS = 0 }, invalidation.KnobWatchHeartbeatMS},
		{"headroom zero", invalidation.KnobWatchHeadroom, func(v *invalidation.Values) { v.Watch.InstallHeadroom = 0 }, invalidation.KnobWatchHeadroom},
		{"scan limit zero", invalidation.KnobWatchScanLimit, func(v *invalidation.Values) { v.Watch.ScanLimit = 0 }, invalidation.KnobWatchScanLimit},
		{"flush above the declared path bound", invalidation.KnobWatchFlushMaxPaths, func(v *invalidation.Values) { v.Watch.FlushMaxPaths = eventsMaxPathsPerEvent + 1 }, invalidation.KnobWatchFlushMaxPaths},
		{"push buffer below the floor", invalidation.KnobPushBufferBytes, func(v *invalidation.Values) { v.Push.SubscriberBufferBytes = 1024 }, invalidation.KnobPushBufferBytes},
		{"write deadline at the heartbeat", invalidation.KnobPushWriteDeadlineMS, func(v *invalidation.Values) { v.Push.WriteDeadlineMS = invalidation.MaxPushWriteDeadlineMS + 1 }, invalidation.KnobPushWriteDeadlineMS},
		{"max subscribers zero", invalidation.KnobPushMaxSubscribers, func(v *invalidation.Values) { v.Push.MaxSubscribers = 0 }, invalidation.KnobPushMaxSubscribers},
		{"heartbeat below the write deadline", invalidation.KnobWatchHeartbeatMS, func(v *invalidation.Values) {
			v.Watch.HeartbeatMS = 1000 // the declared 10 s write deadline is then NOT below it
		}, invalidation.KnobPushWriteDeadlineMS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := invalidation.DefaultValues()
			tc.mutate(&v)
			h, err := New(Config{Root: fixtureTree(t), Build: "test-build", Invalidation: &v})
			if err == nil {
				t.Fatalf("%s was accepted: the surface must refuse a value it cannot obey rather than serve a different one", tc.knob)
			}
			if h != nil {
				t.Fatal("a refused config must not hand back a handler")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("the refusal must name the knob (%s): %v", tc.wantSub, err)
			}
			if !strings.Contains(err.Error(), "refused") {
				t.Fatalf("the refusal must say the value was refused rather than replaced by a default: %v", err)
			}
		})
	}
}

// TestInvalidationDefaultsDoNotDisableAnything is the "safe defaults" arm: the
// declared surface must not contain a value that turns a channel off. The bool
// knob that defaults false is the watcher's opt-in, and the poll form is not
// behind a knob at all — which the table proves by not naming it.
func TestInvalidationDefaultsDoNotDisableAnything(t *testing.T) {
	v := invalidation.DefaultValues()
	if err := v.Validate(); err != nil {
		t.Fatalf("the declared defaults do not validate: %v", err)
	}
	for _, k := range invalidation.Knobs() {
		// The guard is about the CHANNEL an old client uses, so it looks for a
		// knob naming the poll op or the revision poll as its own subject — not
		// for the word "events" anywhere (the push buffer's event bound is
		// subscriber_buffer_events and has nothing to do with the poll).
		for _, part := range strings.Split(k.Name, ".") {
			switch {
			case part == "poll", part == "events", part == "rev",
				strings.HasPrefix(part, "poll_"), strings.HasPrefix(part, "events_"), strings.HasPrefix(part, "rev_"):
				t.Fatalf("knob %s names the poll form (%q): no knob may be able to switch the channel an old client uses off", k.Name, part)
			}
		}
	}
	if v.Watch.Enabled {
		t.Fatal("the watcher defaults ON: enabling it changes the served revision's kind (BFS-035's opt-in) and this row forbids default-enabling anything that needs the channel to be correct")
	}
	if v.Watch.HeartbeatMS == 0 || v.Watch.InstallHeadroom == 0 {
		t.Fatal("a default that is a zero bound is a bound that stops being a bound")
	}
}

// TestInvalidationDefaultsAlignWithTheDeclaredBounds pins the two defaults that
// must agree with bounds the surface already declares to clients, in ONE
// direction only (the table may not raise a bound the document publishes).
func TestInvalidationDefaultsAlignWithTheDeclaredBounds(t *testing.T) {
	if invalidation.DefaultWatchFlushMaxPaths != eventsMaxPathsPerEvent {
		t.Fatalf("flush_max_paths default = %d, want the declared max_paths_per_event %d", invalidation.DefaultWatchFlushMaxPaths, eventsMaxPathsPerEvent)
	}
	if invalidation.MaxWatchFlushMaxPaths != eventsMaxPathsPerEvent {
		t.Fatalf("flush_max_paths max = %d, want the declared max_paths_per_event %d (a knob may lower the path bound, never raise it past what the document promises)", invalidation.MaxWatchFlushMaxPaths, eventsMaxPathsPerEvent)
	}
	if invalidation.DefaultWatchScanLimit != eventsScanLimit {
		t.Fatalf("scan_limit default = %d, want the poll form's own observation bound %d", invalidation.DefaultWatchScanLimit, eventsScanLimit)
	}
}

// ---------------------------------------------------------------------------
// 2. THE READ-BACK IS COMPLETE, SELF-DESCRIBING, AND DOES NOT REPORT WHAT IT
//    DOES NOT OBEY
// ---------------------------------------------------------------------------

func TestInvalidationReadBackCoversEveryKnobWithItsEnforcedRange(t *testing.T) {
	h := newTestHandler(t, func(c *Config) {})
	t.Cleanup(h.Close)
	w := readInvalidationWire(t, h)

	if w.Config.Surface != "server.invalidation" {
		t.Fatalf("the read-back does not name its surface: %q", w.Config.Surface)
	}
	if w.Config.ReadAt == "" {
		t.Fatal("the read-back has no read_at timestamp: \"when was this true?\" is part of the answer")
	}
	if len(w.Config.Knobs) != len(invalidation.Knobs()) {
		t.Fatalf("the read-back reports %d knobs, want one row per declared knob (%d)", len(w.Config.Knobs), len(invalidation.Knobs()))
	}
	for _, k := range invalidation.Knobs() {
		row := knobRow(t, w, k.Name)
		if row.Min != k.Min || row.Max != k.Max {
			t.Fatalf("%s: the read-back reports the range [%d, %d] and the validator enforces [%d, %d] — a range documented one way and enforced another is the defect this row closes", k.Name, row.Min, row.Max, k.Min, k.Max)
		}
		if asInt(t, row.Default) != k.Default {
			t.Fatalf("%s: the read-back reports default %v, the table declares %d", k.Name, row.Default, k.Default)
		}
		if got := asInt(t, row.Value); got < k.Min || got > k.Max {
			t.Fatalf("%s: the value in force %d is outside its own declared range [%d, %d]", k.Name, got, k.Min, k.Max)
		}
		if row.Source != invalidation.SourceDefault && row.Source != invalidation.SourceOperator {
			t.Fatalf("%s: source %q is neither %q nor %q", k.Name, row.Source, invalidation.SourceDefault, invalidation.SourceOperator)
		}
	}
}

// TestInvalidationReadBackFollowsTheRunningWatcher is the defect-class arm: the
// handler is configured at the DECLARED default heartbeat (30000 ms) while the
// watcher actually running on it beats every 7 ms through the in-package seam. A
// read-back that reports the CONFIG would say 30000; the read-back that obeys this
// row's rule says 7.
func TestInvalidationReadBackFollowsTheRunningWatcher(t *testing.T) {
	root := fixtureTree(t)
	withGitFixture(t, root, strings.Repeat("ab", 20))
	v := invalidationConfig(t, func(v *invalidation.Values) {
		v.Watch.Enabled = true // the surface IS enabled; the seam below supplies the running watcher
	})
	h, err := New(Config{Root: root, Build: "test-build", Invalidation: &v})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(h.Close)

	opts := watchOptionsFrom(v)
	opts.heartbeat = 7 * time.Millisecond
	fw := defaultFakeWatch(h.Root())
	h.startWatch(fw.env(), opts)

	w := readInvalidationWire(t, h)
	if w.HeartbeatMS != 7 {
		t.Fatalf("heartbeat_ms = %d, want the RUNNING watcher's 7 — the config says %d and reporting the config is the defect", w.HeartbeatMS, invalidation.DefaultWatchHeartbeatMS)
	}
	row := knobRow(t, w, invalidation.KnobWatchHeartbeatMS)
	if asInt(t, row.Value) != 7 {
		t.Fatalf("the read-back reports heartbeat_ms %v, want the running watcher's 7", row.Value)
	}
	if !row.Applied {
		t.Fatal("a knob a running watcher obeys must be reported applied:true")
	}
	if w.Config.AppliedFrom == "" {
		t.Fatal("the read-back must say where the applied values came from")
	}
}

// TestInvalidationReadBackMarksWhatNothingIsApplying: with no watcher, the rows
// are still readable (the surface exists) but they say applied:false with a reason,
// except the enable knob, which is what DECIDES that state.
func TestInvalidationReadBackMarksWhatNothingIsApplying(t *testing.T) {
	h := newTestHandler(t, func(c *Config) {}) // watcher off by default
	t.Cleanup(h.Close)
	w := readInvalidationWire(t, h)

	if w.Config.AppliedFrom == "" || !strings.Contains(w.Config.AppliedFrom, "no watcher") {
		t.Fatalf("applied_from = %q, want it to say that no watcher is established", w.Config.AppliedFrom)
	}
	enabled := knobRow(t, w, invalidation.KnobWatchEnabled)
	if !enabled.Applied {
		t.Fatal("watch.enabled is what decides this state, so it is applied")
	}
	if got := asInt(t, enabled.Value); got != 0 {
		t.Fatalf("watch.enabled value = %d, want the declared default 0", got)
	}
	hb := knobRow(t, w, invalidation.KnobWatchHeartbeatMS)
	if hb.Applied || hb.AppliedReason == "" {
		t.Fatalf("a watcher knob with no watcher running must be applied:false WITH a reason: %+v", hb)
	}
	push := knobRow(t, w, invalidation.KnobPushMaxSubscribers)
	if push.Applied || !strings.Contains(push.AppliedReason, "push form is not served") {
		t.Fatalf("the push knobs must report applied:false and why: %+v", push)
	}
}

// ---------------------------------------------------------------------------
// 3. THE CONFIGURED VALUES ARE THE ONES THE CODE OBEYS (one arm per knob)
// ---------------------------------------------------------------------------

// TestInvalidationHeadroomKnobReachesTheInstall is the sharpest of the behavioural
// arms: the tree needs 5 watches and the ceiling is 16, so the CONFIGURED headroom
// of 8 fits and the DECLARED default of 512 does not. If the install used the
// default, this cell would see a watch_limit_exhausted refusal instead of a
// watcher — and the reported headroom would be the default it obeyed.
func TestInvalidationHeadroomKnobReachesTheInstall(t *testing.T) {
	v := invalidationConfig(t, func(v *invalidation.Values) {
		v.Watch.Enabled = true
		v.Watch.InstallHeadroom = 8
	})
	root := fixtureTree(t)
	h, err := New(Config{Root: root, Build: "test-build", Invalidation: &v})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(h.Close)

	fw := defaultFakeWatch(h.Root())
	fw.ceilings.MaxUserWatches = 16 // the tree needs 4 directories + 8 headroom
	h.startWatch(fw.env(), watchOptionsFrom(v))

	st := h.watchStatusSnapshot()
	if st.State != WatchStateWatching {
		t.Fatalf("state = %q, want watching: with the configured headroom of 8 the tree fits under a 16-watch ceiling, and the declared default 512 does not — a refusal here means the install used a value the config did not set (%s)", st.State, st.Detail)
	}
	if st.Coverage == nil || st.Coverage.Headroom != 8 {
		t.Fatalf("coverage.headroom = %+v, want the configured 8", st.Coverage)
	}
	w := readInvalidationWire(t, h)
	if got := asInt(t, knobRow(t, w, invalidation.KnobWatchHeadroom).Value); got != 8 {
		t.Fatalf("the read-back reports headroom %d, want the value the install used (8)", got)
	}
	if got := w.Limits.Watching["headroom"]; got != nil && got != 8 {
		t.Fatalf("limits.watching.headroom = %v, want the configured 8", got)
	}
}

// TestInvalidationHeartbeatKnobIsThePeriodTheWatcherBeats: the read-back says 60 ms
// and the counters show beats arriving at that period, which is the "matches what
// the code obeys" test stated as behaviour rather than as a number.
func TestInvalidationHeartbeatKnobIsThePeriodTheWatcherBeats(t *testing.T) {
	v := invalidationConfig(t, func(v *invalidation.Values) {
		v.Watch.Enabled = true
		v.Watch.HeartbeatMS = 200
		v.Push.WriteDeadlineMS = 100
	})
	root := fixtureTree(t)
	h, err := New(Config{Root: root, Build: "test-build", Invalidation: &v})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(h.Close)
	fw := defaultFakeWatch(h.Root())
	h.startWatch(fw.env(), watchOptionsFrom(v))

	waitFor(t, 2*time.Second, "three heartbeats at the configured 200 ms period", func() bool {
		st := h.watchStatusSnapshot()
		return st.Counters != nil && st.Counters.HeartbeatsTotal >= 3
	})
	w := readInvalidationWire(t, h)
	if w.HeartbeatMS != 200 {
		t.Fatalf("heartbeat_ms = %d, want the configured 200", w.HeartbeatMS)
	}
	if got := asInt(t, knobRow(t, w, invalidation.KnobWatchFlushEveryMS).Value); got != int64(invalidation.DefaultWatchFlushEveryMS) {
		t.Fatalf("an untouched knob reports %d, want its declared default %d", got, invalidation.DefaultWatchFlushEveryMS)
	}
}

// TestInvalidationFlushMaxPathsKnobBoundsOneEvent: 3 changed paths with the knob at
// 2 must produce a RESCAN (an overflow), never a 3-path invalidate — the knob is
// the bound §5.1 claim 2 measures.
func TestInvalidationFlushMaxPathsKnobBoundsOneEvent(t *testing.T) {
	v := invalidationConfig(t, func(v *invalidation.Values) {
		v.Watch.Enabled = true
		v.Watch.FlushMaxPaths = 2
		v.Watch.FlushEveryMS = 5
	})
	root := fixtureTree(t)
	h, err := New(Config{Root: root, Build: "test-build", Invalidation: &v})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(h.Close)
	fw := defaultFakeWatch(h.Root())
	h.startWatch(fw.env(), watchOptionsFrom(v))
	b := fw.backend(0)
	if b == nil {
		t.Fatal("no backend was installed")
	}
	for _, rel := range []string{"a.txt", "b.txt", "c.txt"} {
		mustWrite(t, h.Root()+"/"+rel, "x")
		b.pushEvent(h.Root()+"/"+rel, fsnotify.Create)
	}
	waitFor(t, 2*time.Second, "the flush to decide", func() bool {
		return lastLedgerEventOfKind(t, h, eventOverflow) != nil
	})
	if ev := lastLedgerEventOfKind(t, h, eventInvalidate); ev != nil && len(ev.Paths) > 2 {
		t.Fatalf("an invalidate carried %d paths with the knob at 2: a longer path list is exactly what the bound forbids", len(ev.Paths))
	}
	w := readInvalidationWire(t, h)
	if got := asInt(t, knobRow(t, w, invalidation.KnobWatchFlushMaxPaths).Value); got != 2 {
		t.Fatalf("the read-back reports flush_max_paths %d, want the configured 2", got)
	}
}

// TestInvalidationScanLimitKnobBoundsTheWalk: the knob at 2 on a 4-directory tree
// must refuse coverage (never a short watch set claimed as watching), and the
// detail must name the bound the operator set.
func TestInvalidationScanLimitKnobBoundsTheWalk(t *testing.T) {
	v := invalidationConfig(t, func(v *invalidation.Values) {
		v.Watch.Enabled = true
		v.Watch.ScanLimit = 2
	})
	root := fixtureTree(t)
	h, err := New(Config{Root: root, Build: "test-build", Invalidation: &v})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(h.Close)
	fw := defaultFakeWatch(h.Root())
	h.startWatch(fw.env(), watchOptionsFrom(v))

	st := h.watchStatusSnapshot()
	if st.Reason != WatchReasonPartialCoverage {
		t.Fatalf("reason = %q, want watch_partial_coverage: a walk stopped by the configured bound cannot be claimed as covering the tree", st.Reason)
	}
	if !strings.Contains(st.Detail, "2 directories") {
		t.Fatalf("the detail must name the configured bound (2): %s", st.Detail)
	}
	w := readInvalidationWire(t, h)
	if got := asInt(t, knobRow(t, w, invalidation.KnobWatchScanLimit).Value); got != 2 {
		t.Fatalf("the read-back reports scan_limit %d, want the configured 2", got)
	}
}

// TestInvalidationEnableKnobIsWhatStartsTheWatcher: the switch is a knob like any
// other, and both of its states are observable.
func TestInvalidationEnableKnobIsWhatStartsTheWatcher(t *testing.T) {
	off := invalidationConfig(t, func(v *invalidation.Values) { v.Watch.Enabled = false })
	hOff, err := New(Config{Root: fixtureTree(t), Build: "test-build", Invalidation: &off})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(hOff.Close)
	if hOff.watch != nil {
		t.Fatal("watch.enabled false started a watcher")
	}
	if st := hOff.watchStatusSnapshot(); st.State != WatchStateAbsent {
		t.Fatalf("state = %q, want absent", st.State)
	}

	on := invalidationConfig(t, func(v *invalidation.Values) { v.Watch.Enabled = true })
	root := fixtureTree(t)
	hOn, err := New(Config{Root: root, Build: "test-build", Invalidation: &on})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(hOn.Close)
	if hOn.watch == nil {
		t.Fatal("watch.enabled true did not build a watcher (a watcher that cannot be established is a REPORTABLE state, not a nil one)")
	}
	w := readInvalidationWire(t, hOn)
	if got := asInt(t, knobRow(t, w, invalidation.KnobWatchEnabled).Value); got != 1 {
		t.Fatalf("watch.enabled value = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// 4. A VALUE THE PLATFORM CANNOT HONOUR: CONFIGURED *AND* OBSERVED
// ---------------------------------------------------------------------------

// TestUnhonouredValueIsReportedWithBothNumbers is the row's own example: the
// deployment asks for a watch ceiling the kernel cannot give, and the refusal is a
// capability_unavailable carrying BOTH numbers — so an operator can tell "you asked
// for 8192 watches and this kernel gives 128" from "the watcher is broken".
func TestUnhonouredValueIsReportedWithBothNumbers(t *testing.T) {
	const asked = int64(8192)
	v := invalidationConfig(t, func(v *invalidation.Values) {
		v.Watch.Enabled = true
		v.Watch.MaxWatches = asked
	})
	root := fixtureTree(t)
	h, err := New(Config{Root: root, Build: "test-build", Invalidation: &v})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(h.Close)
	fw := defaultFakeWatch(h.Root())
	fw.ceilings.MaxUserWatches = 128 // the kernel's own ceiling, far below the request
	h.startWatch(fw.env(), watchOptionsFrom(v))

	st := h.watchStatusSnapshot()
	if st.Reason != WatchReasonLimitExhausted {
		t.Fatalf("reason = %q, want watch_limit_exhausted (a kernel ceiling below the request is the limit case, never an install failure)", st.Reason)
	}
	// Carrier 1: the refusal's verdict and its configured-vs-observed pair.
	refusal := readWatchRefusal(t, h)
	if refusal.Verdict != "capability_unavailable" {
		t.Fatalf("verdict = %q, want capability_unavailable", refusal.Verdict)
	}
	if refusal.Error.Unhonoured == nil {
		t.Fatalf("the refusal carries no configured-vs-observed pair: %s", refusal.Error.Detail)
	}
	if refusal.Error.Unhonoured.Configured != asked || refusal.Error.Unhonoured.Observed != 128 {
		t.Fatalf("the pair is configured %d / observed %d, want %d / 128", refusal.Error.Unhonoured.Configured, refusal.Error.Unhonoured.Observed, asked)
	}
	if refusal.Error.Unhonoured.Knob != invalidation.KnobWatchMaxWatches {
		t.Fatalf("the pair names knob %q, want %q", refusal.Error.Unhonoured.Knob, invalidation.KnobWatchMaxWatches)
	}
	if !strings.Contains(refusal.Error.Detail, "8192") || !strings.Contains(refusal.Error.Detail, "128") {
		t.Fatalf("the detail must name both numbers: %s", refusal.Error.Detail)
	}
	// Carrier 2: the document — the same pair, plus the numbers beside the ceiling.
	w := readInvalidationWire(t, h)
	if len(w.Config.Unhonoured) != 1 || w.Config.Unhonoured[0].Configured != asked || w.Config.Unhonoured[0].Observed != 128 {
		t.Fatalf("the document's unhonoured list = %+v, want the requested %d and the platform's 128", w.Config.Unhonoured, asked)
	}
	if got := asInt(t, w.Limits.Watching["requested_max_watches"]); got != asked {
		t.Fatalf("limits.watching.requested_max_watches = %v, want %d", w.Limits.Watching["requested_max_watches"], asked)
	}
	if got := asInt(t, w.Limits.Watching["platform_max_user_watches"]); got != int64(128) {
		t.Fatalf("limits.watching.platform_max_user_watches = %v, want 128", w.Limits.Watching["platform_max_user_watches"])
	}
	if got := asInt(t, w.Limits.Watching["ceiling_in_force"]); got != int64(128) {
		t.Fatalf("limits.watching.ceiling_in_force = %v, want 128 (the platform's number, not the request)", w.Limits.Watching["ceiling_in_force"])
	}
	if got := asInt(t, w.Config.Ceiling["ceiling_in_force"]); got != int64(128) {
		t.Fatalf("config.ceiling.ceiling_in_force = %v, want 128", w.Config.Ceiling["ceiling_in_force"])
	}
	if got := asInt(t, w.Config.Ceiling["requested_max_watches"]); got != asked {
		t.Fatalf("config.ceiling.requested_max_watches = %v, want the requested %d", w.Config.Ceiling["requested_max_watches"], asked)
	}
	if got := asInt(t, w.Config.Ceiling["platform_max_user_watches"]); got != int64(128) {
		t.Fatalf("config.ceiling.platform_max_user_watches = %v, want 128", w.Config.Ceiling["platform_max_user_watches"])
	}
	if got := w.Config.Ceiling["binding"]; got != "platform" {
		t.Fatalf("config.ceiling.binding = %v, want platform (the ceiling in force is the platform's)", got)
	}
	if got := asInt(t, knobRow(t, w, invalidation.KnobWatchMaxWatches).Value); got != asked {
		t.Fatalf("the read-back reports max_watches %d, want the REQUESTED %d: the knob's row is the request, and the platform's number travels beside it", got, asked)
	}
	// The read-back must not claim knobs whose machinery never started: an install
	// that refused (state=absent) still USED the walk bound and the ceiling
	// arithmetic, and never started the heartbeat or the flush at all.
	if row := knobRow(t, w, invalidation.KnobWatchScanLimit); !row.Applied {
		t.Fatalf("scan_limit must be applied: the refused install RAN the walk under it (%+v)", row)
	}
	if row := knobRow(t, w, invalidation.KnobWatchHeartbeatMS); row.Applied {
		t.Fatalf("heartbeat_ms is reported applied while no loop is running: %+v", row)
	} else if !strings.Contains(row.AppliedReason, "state=absent") {
		t.Fatalf("the reason must name the state that makes it unapplied: %q", row.AppliedReason)
	}
	if !strings.Contains(w.Config.AppliedFrom, "ABSENT") {
		t.Fatalf("applied_from must say the install is absent: %q", w.Config.AppliedFrom)
	}
}

// TestUnhonouredValueThatDoesNotBindIsAWarningNotARefusal: the request is above the
// platform's ceiling but the tree still fits, so the channel is UP. Reporting
// capability_unavailable here would be the "the watcher is broken" lie this row
// forbids — so it is a blocks_push:false degradation entry with both numbers.
func TestUnhonouredValueThatDoesNotBindIsAWarningNotARefusal(t *testing.T) {
	const asked = int64(8192)
	v := invalidationConfig(t, func(v *invalidation.Values) {
		v.Watch.Enabled = true
		v.Watch.MaxWatches = asked
	})
	root := fixtureTree(t)
	h, err := New(Config{Root: root, Build: "test-build", Invalidation: &v})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(h.Close)
	fw := defaultFakeWatch(h.Root())
	fw.ceilings.MaxUserWatches = 4096 // above what the tree needs, below what was asked
	h.startWatch(fw.env(), watchOptionsFrom(v))

	st := h.watchStatusSnapshot()
	if st.State != WatchStateWatching {
		t.Fatalf("state = %q, want watching: the value does not bind, and a watcher that is up must never be reported absent (%s)", st.State, st.Detail)
	}
	w := readInvalidationWire(t, h)
	if len(w.Config.Unhonoured) != 1 {
		t.Fatalf("unhonoured = %+v, want the pair even though it does not bind: \"did my number take effect?\" must not depend on the size of the tree today", w.Config.Unhonoured)
	}
	if got := asInt(t, w.Config.Ceiling["ceiling_in_force"]); got != int64(4096) {
		t.Fatalf("config.ceiling.ceiling_in_force = %v, want the platform's 4096", w.Config.Ceiling["ceiling_in_force"])
	}
	var found bool
	for _, d := range h.watchDegradations(st) {
		if d["unhonoured"] == true {
			found = true
			if d["blocks_push"] != false {
				t.Fatalf("an unhonoured value on a WORKING watcher must not block push: %+v", d)
			}
			if asInt(t, d["configured"]) != asked || asInt(t, d["observed"]) != 4096 {
				t.Fatalf("the degradation entry must carry both numbers: %+v", d)
			}
		}
	}
	if !found {
		t.Fatalf("no degradation entry carries the unhonoured value: %v", h.watchDegradations(st))
	}
	// The same pair must reach the carrier a client PROBES — the op's own refusal.
	// Where the push form is served (this cell: the watcher is up) that op STREAMS
	// instead of refusing (BFS-036), so the probe-carrier is asserted on a SECOND
	// handler with the SAME configuration whose watcher is refused. The pair is
	// read from the CONFIG, not from the watcher, so which of the two states the
	// target is in cannot change the numbers a client is told.
	root2 := fixtureTree(t)
	h2, err := New(Config{Root: root2, Build: "test-build", Invalidation: &v})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(h2.Close)
	fw2 := defaultFakeWatch(h2.Root())
	// The SAME platform ceiling the first handler probed, so the pair the refusal
	// carries is the same pair. The absence is injected through a REFUSED
	// DIRECTORY (W-4 partial coverage) rather than through a ceiling, because a
	// ceiling that does not fit would change the observed number under assertion,
	// and a backend-level absence never reaches the ceiling arithmetic at all.
	fw2.ceilings.MaxUserWatches = 4096
	fw2.failAdd = func(dir string) error {
		if filepath.Base(dir) == "empty" {
			return fmt.Errorf("add %s: %w", dir, os.ErrPermission)
		}
		return nil
	}
	h2.startWatch(fw2.env(), watchOptionsFrom(v))
	refusal := readWatchRefusal(t, h2)
	if refusal.Error.Unhonoured == nil || refusal.Error.Unhonoured.Configured != asked || refusal.Error.Unhonoured.Observed != 4096 {
		t.Fatalf("the refusal does not carry the pair: %+v", refusal.Error)
	}
	// NOTE (reported, not smoothed over): watchDegradations' output is asserted
	// here through the FUNCTION because the document's degradations[] does not yet
	// carry the watcher's entries — see docs/evidence/BFS-043-*.md, residual R-2.
	// The pair IS on the wire regardless: config.unhonoured and the refusal.
}

// ---------------------------------------------------------------------------
// 6. THE OPERATOR-FACING SAMPLE
// ---------------------------------------------------------------------------

// TestInvalidationWireSampleForEvidence emits, verbatim, the two carriers an
// operator reads — the `watch` refusal and the document's config block — for a
// deployment whose requested watch ceiling the platform cannot give. It exists so
// docs/evidence/BFS-043-*.txt can quote real bytes from a runnable cell rather than
// a hand-written sample: the assertions keep it honest (the pair must be there),
// and `go test -run TestInvalidationWireSampleForEvidence -v` regenerates it.
func TestInvalidationWireSampleForEvidence(t *testing.T) {
	const asked = int64(8192)
	v := invalidationConfig(t, func(v *invalidation.Values) {
		v.Watch.Enabled = true
		v.Watch.MaxWatches = asked
		v.Watch.HeartbeatMS = 5000
		v.Push.WriteDeadlineMS = 4000
	})
	root := fixtureTree(t)
	h, err := New(Config{Root: root, Build: "test-build", Invalidation: &v})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(h.Close)
	fw := defaultFakeWatch(h.Root())
	fw.ceilings.MaxUserWatches = 128
	h.startWatch(fw.env(), watchOptionsFrom(v))

	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "watch"}, "")
	t.Logf("X-Bunker-Verdict: %s", rec.Header().Get("X-Bunker-Verdict"))
	t.Logf("X-Bunker-Capability: %s", rec.Header().Get("X-Bunker-Capability"))
	t.Logf("refusal: %s", prettyJSON(t, rec.Body.Bytes()))

	w := readInvalidationWire(t, h)
	if len(w.Config.Unhonoured) != 1 {
		t.Fatalf("the sample deployment must produce an unhonourable pair, got %+v", w.Config.Unhonoured)
	}
	doc := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "capabilities"}, "")
	var caps struct {
		Result struct {
			Capabilities struct {
				Extensions struct {
					Watch map[string]any `json:"watch"`
				} `json:"extensions"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(doc.Body.Bytes(), &caps); err != nil {
		t.Fatalf("capabilities envelope is not JSON: %v", err)
	}
	t.Logf("extensions.watch.config: %s", prettyJSONValue(t, caps.Result.Capabilities.Extensions.Watch["config"]))
	t.Logf("extensions.watch.limits.watching: %s", prettyJSONValue(t, caps.Result.Capabilities.Extensions.Watch["limits"]))
	// The document's degradations list, logged for residual R-1: the WATCHER's own
	// entries (watchDegradations) are not wired into it, so the reason-carrying
	// entry §8.2 promises does not reach this list.
	var degs struct {
		Result struct {
			Capabilities struct {
				Degradations []map[string]any `json:"degradations"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(doc.Body.Bytes(), &degs); err != nil {
		t.Fatalf("capabilities envelope is not JSON: %v", err)
	}
	t.Logf("capabilities.degradations: %s", prettyJSONValue(t, degs.Result.Capabilities.Degradations))
}

// prettyJSON indents a JSON document for a log line.
func prettyJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return string(raw)
	}
	return "\n" + out.String()
}

// prettyJSONValue indents any decoded JSON value for a log line.
func prettyJSONValue(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "<unencodable>"
	}
	return "\n" + string(raw)
}

// ---------------------------------------------------------------------------
// 5. STOCK-CLIENT BEHAVIOUR IS UNCHANGED
// ---------------------------------------------------------------------------

// TestInvalidationSurfaceLeavesAStockClientAlone drives the RFC 4918 surface with
// no X-Bunker-* headers on a handler configured at the legal extremes of every
// watcher knob, and requires the SAME bytes as a handler at the declared defaults.
// It also requires the declared poll op to answer: nothing here may make an old
// client stop working.
func TestInvalidationSurfaceLeavesAStockClientAlone(t *testing.T) {
	stock := func(t *testing.T, h *Handler) (int, []byte, int, []byte, int) {
		t.Helper()
		pf := do(t, h, "PROPFIND", "/dav/", map[string]string{"Depth": "1"}, "")
		get := do(t, h, "GET", "/dav/README.md", nil, "")
		ev := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "events"}, "")
		return pf.Code, pf.Body.Bytes(), get.Code, get.Body.Bytes(), ev.Code
	}

	// The two handlers serve the SAME tree: a PROPFIND multistatus carries each
	// resource's last-modified time, so two fixture roots built a moment apart
	// differ for a reason that has nothing to do with the config surface.
	root := fixtureTree(t)
	base := newTestHandler(t, func(c *Config) { c.Root = root })
	t.Cleanup(base.Close)
	basePF, basePFBody, baseGet, baseGetBody, baseEv := stock(t, base)

	extreme := invalidationConfig(t, func(v *invalidation.Values) {
		v.Watch.HeartbeatMS = invalidation.MaxWatchHeartbeatMS
		v.Watch.InstallHeadroom = 1
		v.Watch.FlushEveryMS = invalidation.MaxWatchFlushEveryMS
		v.Watch.FlushMaxPaths = invalidation.MinWatchFlushMaxPaths
		v.Watch.ScanLimit = invalidation.MinWatchScanLimit
		v.Push.SubscriberBufferBytes = invalidation.MaxPushBufferBytes
		v.Push.SubscriberBufferEvents = invalidation.MaxPushBufferEvents
		v.Push.MaxSubscribers = invalidation.MaxPushMaxSubscribers
		v.Push.WriteDeadlineMS = invalidation.MinPushWriteDeadlineMS
		v.Push.MaxEventBytes = invalidation.MaxPushMaxEventBytes
	})
	h := newTestHandler(t, func(c *Config) { c.Root = root; c.Invalidation = &extreme })
	t.Cleanup(h.Close)
	pf, pfBody, get, getBody, ev := stock(t, h)

	if pf != basePF || string(pfBody) != string(basePFBody) {
		t.Fatalf("PROPFIND changed under a configured invalidation surface: %d vs %d", pf, basePF)
	}
	if get != baseGet || string(getBody) != string(baseGetBody) {
		t.Fatalf("GET changed under a configured invalidation surface: %d vs %d", get, baseGet)
	}
	if ev != http.StatusOK {
		t.Fatalf("the declared poll op answered %d, want 200: no knob may switch the channel an old client uses off", ev)
	}
	if baseEv != http.StatusOK {
		t.Fatalf("the poll op answered %d at the declared defaults", baseEv)
	}
}
