package invalidation

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestKnobTableIsComplete is the table's own contract: every knob declares a
// name, a default, a range, a unit, a failure mode and a reason, the defaults
// sit INSIDE their ranges, and no knob's zero value is a legal value by
// accident (the one exception is documented: max_watches' 0 is the AUTO
// sentinel, which is why its range starts at 0).
func TestKnobTableIsComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range Knobs() {
		if k.Name == "" {
			t.Fatal("a knob has no name: nothing could be refused or reported under it")
		}
		if seen[k.Name] {
			t.Fatalf("knob %s is declared twice", k.Name)
		}
		seen[k.Name] = true
		if k.Unit == "" {
			t.Fatalf("%s: no unit", k.Name)
		}
		if k.Failure == "" {
			t.Fatalf("%s: no declared failure mode", k.Name)
		}
		if k.Why == "" {
			t.Fatalf("%s: no justification for the default", k.Name)
		}
		if k.Default < k.Min || k.Default > k.Max {
			t.Fatalf("%s: default %d is outside its own declared range [%d, %d]", k.Name, k.Default, k.Min, k.Max)
		}
		if k.Unit != UnitBool && k.Default == 0 && !k.AutoSentinel {
			t.Fatalf("%s: default 0 is inside range [%d, %d], which would make the zero value a silent default — every numeric knob's range must exclude 0 unless its 0 is a documented AUTO sentinel", k.Name, k.Min, k.Max)
		}
		if k.AutoSentinel && k.Min != 0 {
			t.Fatalf("%s: declared an AUTO sentinel but its range starts at %d, so 0 is not writable at all", k.Name, k.Min)
		}
	}
	if len(Knobs()) != 12 {
		t.Fatalf("the table declares %d knobs; the row's surface is 12 (7 watcher + 5 push)", len(Knobs()))
	}
}

// TestDefaultValuesMatchTheTable is the anti-drift pin between the two things a
// reader would otherwise have to trust: the declared table and the struct the
// code is built from. If they disagree, the surface reports one number and obeys
// another — the defect class this row exists to close.
func TestDefaultValuesMatchTheTable(t *testing.T) {
	v := DefaultValues()
	if err := v.Validate(); err != nil {
		t.Fatalf("the declared defaults do not validate: %v", err)
	}
	for _, k := range Knobs() {
		got, ok := v.KnobValue(k.Name)
		if !ok {
			t.Fatalf("%s is declared but no accessor returns it: the read-back would report nothing for a knob the config accepts", k.Name)
		}
		if got != k.Default {
			t.Fatalf("%s: DefaultValues() = %d but the table declares %d", k.Name, got, k.Default)
		}
	}
	// The defaults that matter for the two defects this surface could repeat.
	if v.Watch.Enabled {
		t.Fatal("watch.enabled defaults to true: enabling the watcher changes the served revision's kind (BFS-035's opt-in) and the row forbids default-enabling anything that needs the channel to be correct")
	}
	if v.Watch.InstallHeadroom == 0 {
		t.Fatal("watch.install_headroom defaults to 0, which is the value §4.2 explicitly forbids: a bound the tree can outgrow silently")
	}
	if v.Watch.HeartbeatMS == 0 {
		t.Fatal("watch.heartbeat_ms defaults to 0: a client deriving its dead-channel rule from the declaration would declare the channel dead at once")
	}
	if v.Push.WriteDeadlineMS >= v.Watch.HeartbeatMS {
		t.Fatal("the declared defaults break the spec relation push_write_deadline < heartbeat_ms")
	}
}

// TestEveryKnobRefusesAnInvalidValueLoudly is the row's "one per knob" arm,
// driven off the table so a NEW knob cannot be added without a cell covering
// it. Each case asserts the refusal names the knob and its range, and — the
// point of the row — that the bad value was NOT quietly replaced by the
// default.
func TestEveryKnobRefusesAnInvalidValueLoudly(t *testing.T) {
	for _, k := range Knobs() {
		if k.Unit == UnitBool {
			// A non-boolean cannot reach this table at all: the mapping decoder
			// refuses it before a value exists (internal/config's load test
			// proves that end). What this surface must guarantee is the TYPE,
			// so that a written `enabled:` is a bool or a load error — never a
			// string silently read as false.
			if reflect.TypeOf(WatchSpec{}.Enabled).Kind() != reflect.Ptr || reflect.TypeOf(WatchSpec{}.Enabled).Elem().Kind() != reflect.Bool {
				t.Fatalf("%s: the field must be a *bool so an absent key is distinguishable from a written one", k.Name)
			}
			continue
		}
		t.Run(k.Name, func(t *testing.T) {
			bad := k.Min - 1
			if k.Min == 0 {
				bad = k.Max + 1
			}
			spec, err := specWith(k.Name, bad)
			if err != nil {
				t.Fatal(err)
			}
			v, err := spec.Resolve()
			if err == nil {
				t.Fatalf("%s = %d was ACCEPTED (resolved surface %+v): an invalid value must fail loudly, not fall back", k.Name, bad, v)
			}
			msg := err.Error()
			if !strings.Contains(msg, k.Name) {
				t.Fatalf("the refusal does not name the knob: %s", msg)
			}
			if !strings.Contains(msg, fmt.Sprintf("[%d, %d]", k.Min, k.Max)) {
				t.Fatalf("the refusal does not state the range [%d, %d]: %s", k.Min, k.Max, msg)
			}
			if !strings.Contains(msg, fmt.Sprintf("%d", bad)) {
				t.Fatalf("the refusal does not state the offending value %d: %s", bad, msg)
			}
			if !strings.Contains(msg, "refused") {
				t.Fatalf("the refusal must say the value is refused rather than replaced: %s", msg)
			}
			if v != (Values{}) {
				t.Fatalf("a refused surface must not be returned for use: %+v", v)
			}
		})
	}
}

// TestZeroIsRefusedForEveryNumericKnob is the typo'd-limit case stated on its
// own, because it is the one a silent-default implementation survives: a key
// written as 0 must never read as "the default", and for every numeric knob
// except max_watches' AUTO sentinel it must be refused.
func TestZeroIsRefusedForEveryNumericKnob(t *testing.T) {
	for _, k := range Knobs() {
		if k.Unit == UnitBool || k.Min == 0 {
			continue
		}
		spec, err := specWith(k.Name, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := spec.Resolve(); err == nil {
			t.Fatalf("%s = 0 was accepted: a written zero is how a bound stops being a bound", k.Name)
		}
	}
	// …and the documented exception, which is a sentinel and not a fallback.
	spec, err := specWith(KnobWatchMaxWatches, 0)
	if err != nil {
		t.Fatal(err)
	}
	v, err := spec.Resolve()
	if err != nil {
		t.Fatalf("max_watches = 0 is the AUTO sentinel and must resolve: %v", err)
	}
	if v.Watch.MaxWatches != 0 {
		t.Fatalf("max_watches = %d, want the written 0", v.Watch.MaxWatches)
	}
}

// TestAbsentKeysTakeDefaultsWrittenKeysAreObeyed pins the other half of the
// rule: an ABSENT key takes the declared default (a fact about the file), so a
// deployment that writes one knob does not have to write twelve.
func TestAbsentKeysTakeDefaultsWrittenKeysAreObeyed(t *testing.T) {
	hb := 1000
	spec, err := specWith(KnobWatchHeartbeatMS, int64(hb))
	if err != nil {
		t.Fatal(err)
	}
	// A heartbeat faster than the declared write deadline needs the deadline
	// lowered with it: the relation is refused, never adjusted (§4.4).
	dl := 500
	spec.Push.WriteDeadlineMS = &dl
	v, err := spec.Resolve()
	if err != nil {
		t.Fatalf("a legal value was refused: %v", err)
	}
	if v.Watch.HeartbeatMS != hb {
		t.Fatalf("heartbeat_ms = %d, want the written %d", v.Watch.HeartbeatMS, hb)
	}
	if v.Watch.InstallHeadroom != DefaultWatchInstallHeadroom {
		t.Fatalf("install_headroom = %d, want the declared default %d for an absent key", v.Watch.InstallHeadroom, DefaultWatchInstallHeadroom)
	}
	rows := map[string]Row{}
	for _, r := range v.Rows() {
		rows[r.Knob] = r
	}
	if got := rows[KnobWatchHeartbeatMS].Source; got != SourceOperator {
		t.Fatalf("heartbeat_ms source = %q, want %q", got, SourceOperator)
	}
	if got := rows[KnobWatchHeadroom].Source; got != SourceDefault {
		t.Fatalf("install_headroom source = %q, want %q", got, SourceDefault)
	}
	if got := rows[KnobWatchHeartbeatMS].Value; got != int64(hb) {
		t.Fatalf("the read-back reports %d for heartbeat_ms, want the value in force %d", got, hb)
	}
	if len(v.Rows()) != len(Knobs()) {
		t.Fatalf("the read-back reports %d knobs, want one row per declared knob (%d)", len(v.Rows()), len(Knobs()))
	}
}

// TestRelationBetweenTwoKnobsIsRefused covers the case that is invisible to a
// per-field range check: two values that are each legal and cannot both be
// obeyed.
func TestRelationBetweenTwoKnobsIsRefused(t *testing.T) {
	spec := Spec{}
	hb, dl := 1000, 20000
	spec.Watch.HeartbeatMS = &hb
	spec.Push.WriteDeadlineMS = &dl
	_, err := spec.Resolve()
	if err == nil {
		t.Fatal("heartbeat_ms 1000 with write_deadline_ms 20000 was accepted: a write deadline longer than the heartbeat period means a stalled subscriber is detected after the client has already declared the channel dead")
	}
	for _, want := range []string{KnobPushWriteDeadlineMS, KnobWatchHeartbeatMS, "not below"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the relation refusal must name both knobs and the relation (%q): %s", want, err.Error())
		}
	}
}

// TestSpecIsZero pins the "was the block written at all" question the loader
// asks before it resolves anything.
func TestSpecIsZero(t *testing.T) {
	if !(Spec{}).IsZero() {
		t.Fatal("an empty Spec must report IsZero: a deployment with no invalidation block gets the declared defaults")
	}
	on := false
	spec := Spec{}
	spec.Watch.Enabled = &on
	if spec.IsZero() {
		t.Fatal("a Spec with a written key must not report IsZero: a written false is a decision, and it is not the same as an absent key")
	}
}

// TestWatchCeilingUnhonoured pins the configured-vs-observed arithmetic — the
// numbers an operator reads to tell "you asked for more than this kernel gives"
// from "the watcher is broken" — in all four shapes.
func TestWatchCeilingUnhonoured(t *testing.T) {
	cases := []struct {
		name       string
		c          WatchCeiling
		wantPair   bool
		wantConfig int64
		wantObserv int64
		wantFits   bool
	}{
		{name: "auto ceiling, the tree fits", c: WatchCeiling{Requested: 0, Platform: 1048576, Desired: 10, Headroom: 512}, wantFits: true},
		{name: "asked for more than the kernel gives, the tree still fits", c: WatchCeiling{Requested: 8192, Platform: 4096, Desired: 10, Headroom: 512}, wantPair: true, wantConfig: 8192, wantObserv: 4096, wantFits: true},
		{name: "asked for more than the kernel gives, and it binds", c: WatchCeiling{Requested: 8192, Platform: 128, Desired: 10, Headroom: 512}, wantPair: true, wantConfig: 8192, wantObserv: 128, wantFits: false},
		{name: "asked for less than the tree needs", c: WatchCeiling{Requested: 100, Platform: 1048576, Desired: 40, Headroom: 512}, wantPair: true, wantConfig: 100, wantObserv: 552, wantFits: false},
		{name: "no ceiling readable never binds", c: WatchCeiling{Requested: 0, Platform: 0, Desired: 100000, Headroom: 512}, wantFits: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.Fits(); got != tc.wantFits {
				t.Fatalf("Fits() = %v, want %v (ceiling in force %d, need %d)", got, tc.wantFits, tc.c.Effective(), tc.c.Need())
			}
			u := tc.c.Unhonoured()
			if tc.wantPair != (u != nil) {
				t.Fatalf("Unhonoured() = %v, want pair=%v", u, tc.wantPair)
			}
			if u == nil {
				return
			}
			if u.Configured != tc.wantConfig || u.Observed != tc.wantObserv {
				t.Fatalf("Unhonoured() = configured %d / observed %d, want %d / %d", u.Configured, u.Observed, tc.wantConfig, tc.wantObserv)
			}
			if u.Knob != KnobWatchMaxWatches || u.Detail == "" {
				t.Fatalf("the pair must name the knob and explain the two numbers: %+v", u)
			}
		})
	}
}

// specWith writes one knob by its declared name, so the loud-failure arm can be
// driven off the table rather than off a hand-written list of fields.
func specWith(name string, v int64) (Spec, error) {
	s := Spec{}
	i := int(v)
	i64 := v
	switch name {
	case KnobWatchHeartbeatMS:
		s.Watch.HeartbeatMS = &i
	case KnobWatchHeadroom:
		s.Watch.InstallHeadroom = &i
	case KnobWatchFlushEveryMS:
		s.Watch.FlushEveryMS = &i
	case KnobWatchFlushMaxPaths:
		s.Watch.FlushMaxPaths = &i
	case KnobWatchScanLimit:
		s.Watch.ScanLimit = &i
	case KnobWatchMaxWatches:
		s.Watch.MaxWatches = &i64
	case KnobPushBufferBytes:
		s.Push.SubscriberBufferBytes = &i64
	case KnobPushBufferEvents:
		s.Push.SubscriberBufferEvents = &i
	case KnobPushMaxSubscribers:
		s.Push.MaxSubscribers = &i
	case KnobPushWriteDeadlineMS:
		s.Push.WriteDeadlineMS = &i
	case KnobPushMaxEventBytes:
		s.Push.MaxEventBytes = &i64
	default:
		return Spec{}, fmt.Errorf("specWith: %s is not a settable knob in this helper", name)
	}
	return s, nil
}
