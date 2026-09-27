package fsclient

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// defaultTestEnv is the surrounding configuration a default mount resolves
// against: the measured defaults from SPEC-hot-file-policy §2 — the client's
// 25-slot pool, the 30 s operation deadline, the 256 MiB cache bound and its
// 64 MiB per-entry cap.
func defaultTestEnv() HotPolicyEnv {
	return HotPolicyEnv{
		Concurrency:        DefaultConcurrency,
		OpTimeout:          DefaultOpTimeout,
		CacheMaxBytes:      DefaultCacheMaxBytes,
		CacheMaxEntryBytes: DefaultCacheMaxEntryBytes,
	}
}

// TestHotPolicyDefaultsAreThePinnedNumbers pins every default to the number
// SPEC-hot-file-policy.md chose, so a later edit cannot move a knob without
// failing here. The last four rows are the DERIVED numbers of §6.1/Q-9 and
// Appendix A.2/A.3 — the ones the spec makes load-bearing and an operator would
// otherwise have to compute by hand.
func TestHotPolicyDefaultsAreThePinnedNumbers(t *testing.T) {
	p := DefaultHotPolicy()
	if p.IsZero() {
		t.Fatal("DefaultHotPolicy() is the zero value: the defaults are not populated")
	}
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"--hot.enabled", p.Enabled, DefaultHotEnabled},
		{"--hot.read-weight (H-1)", p.WeightRead, 1.0},
		{"--hot.edit-weight (H-2)", p.WeightEdit, 8.0},
		{"--hot.decay (H-3)", p.Decay, 0.9},
		{"--hot.decay-step (H-3)", p.DecayStep, 300 * time.Second},
		{"--hot.score-ceiling (H-6)", p.ScoreCeiling, 10000.0},
		{"--hot.read-touch-window (H-9)", p.ReadTouchWindow, 5 * time.Second},
		{"--hot.flush-interval (H-21)", p.FlushInterval, 30 * time.Second},
		{"--hot.max-entries (H-7)", p.TrackerMaxEntries, 4096},
		{"--hot.max-tracker-bytes (H-8)", p.TrackerMaxBytes, int64(1048576)},
		{"--hot.max-file-bytes (S-1)", p.MaxFileBytes, int64(8388608)},
		{"--hot.queue-depth (Q-1)", p.QueueMaxDepth, 256},
		{"--hot.queue-max-wait (Q-6)", p.QueueMaxWait, 300 * time.Second},
		{"--hot.max-concurrent-refresh (Q-7)", p.RefreshMaxInflight, 2},
		{"--hot.pool-share (P-3)", p.PoolShare(), "1/8"},
		{"--hot.backoff-base-ms (P-10)", p.BackoffBase, 250 * time.Millisecond},
		{"--hot.backoff-max-ms (P-10)", p.BackoffMax, 30 * time.Second},
		{"--hot.backoff-factor (P-10)", p.BackoffFactor, 2.0},
		{"--hot.backoff-jitter (P-10)", p.BackoffJitter, HotJitterFull},
		{"--hot.refresh-deadline (P-11)", p.RefreshDeadline, 10 * time.Second},
		{"--hot.reacquire-window (P-12)", p.RefreshReacquireWindow, 30 * time.Second},
		{"--hot.yield-after (P-7)", p.YieldAfter, 250 * time.Millisecond},
		{"--hot.stop-deadline (Q-12)", p.StopDeadline, 1500 * time.Millisecond},
		{"--hot.tick-interval (Q-14)", p.TickInterval, time.Second},
		{"--hot.pool-pressure-ticks (P-19)", p.PoolPressureTicks, 5},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s default = %v, want %v (a default that differs from the spec is an invented number)", c.name, c.got, c.want)
		}
	}
	if err := p.Validate(defaultTestEnv()); err != nil {
		t.Fatalf("the default policy must validate against the default environment: %v", err)
	}
}

// TestHotPolicyEffectiveDerivesThePinnedNumbers covers §6.1's arithmetic and
// Appendix A.5's reservation ceiling. These are the numbers the policy's own
// text states as facts ("floor(Concurrency/8) = 3", "16 MiB"), so they are
// asserted rather than restated.
func TestHotPolicyEffectiveDerivesThePinnedNumbers(t *testing.T) {
	eff := DefaultHotPolicy().Effective(defaultTestEnv())
	if eff.PoolShare != "1/8" {
		t.Errorf("pool_share = %q, want \"1/8\" (P-3)", eff.PoolShare)
	}
	if eff.Derived.PoolSlots != 3 {
		t.Errorf("pool_slots = %d, want 3 (P-3: floor(25/8)); a refresh budget that is not the spec's arithmetic is a different policy", eff.Derived.PoolSlots)
	}
	if eff.Derived.PoolSlotsForeground != 22 {
		t.Errorf("pool_slots_foreground = %d, want 22 (P-3: 88%% of 25)", eff.Derived.PoolSlotsForeground)
	}
	if eff.Derived.RefreshMaxInflight != 2 {
		t.Errorf("refresh_max_inflight = %d, want 2 (Q-7: min(2, 3))", eff.Derived.RefreshMaxInflight)
	}
	if eff.Derived.RefreshMaxInflightClamped {
		t.Error("refresh_max_inflight_clamped = true at the defaults: 2 is under the share, so nothing is clamped")
	}
	if want := int64(2 * 8388608); eff.Derived.MaxInflightBytes != want {
		t.Errorf("max_inflight_bytes = %d, want %d (Q-9/A.5: 16 MiB)", eff.Derived.MaxInflightBytes, want)
	}
	// Appendix A.2's half-life. The spec prints 1973.6 s; the exact arithmetic
	// is 1973.644 s, so the assertion carries the spec's own 0.1 s rounding as
	// its tolerance rather than pinning a truncated print.
	if got, want := eff.Derived.HalfLifeMS, int64(1973600); got < want-100 || got > want+100 {
		t.Errorf("half_life_ms = %d, want ≈%d (Appendix A.2: 0.9 per 300 s is ≈32.9 min)", got, want)
	}
	if mins := float64(eff.Derived.HalfLifeMS) / 60000.0; mins < 32.8 || mins > 33.0 {
		t.Errorf("half-life = %.2f min, want ≈32.9 min (Appendix A.2)", mins)
	}
	// The two RULE facts that are not numbers, reported so a running mount's
	// policy is inspectable rather than inferred (Q-3, S-2).
	if eff.QueueReplacement != HotQueueReplacementScore {
		t.Errorf("queue_replacement = %q, want %q (Q-3: replaced BY SCORE, not FIFO)", eff.QueueReplacement, HotQueueReplacementScore)
	}
	if !eff.SizeRuleInclusive {
		t.Error("size_rule_inclusive = false, want true (S-2: a file exactly at the ceiling IS refreshable)")
	}
}

// TestHotPolicyEffectiveClampsAndReports covers P-4, which is the ONE place the
// spec prefers a clamp to a refusal: an inflight cap above the pool share is
// clamped and REPORTED (`refresh_max_inflight_clamped: true`), because a
// stripped request is strictly safer than a rejected one.
func TestHotPolicyEffectiveClampsAndReports(t *testing.T) {
	cases := []struct {
		name         string
		concurrency  int
		shareNum     int
		shareDen     int
		inflight     int
		wantSlots    int
		wantFore     int
		wantInflight int
		wantClamped  bool
	}{
		{"defaults", 25, 1, 8, 2, 3, 22, 2, false},
		{"cap at the share is not a clamp", 25, 1, 8, 3, 3, 22, 3, false},
		{"cap above the share clamps", 25, 1, 8, 9, 3, 22, 3, true},
		{"a tiny pool still yields one refresh slot", 4, 1, 8, 2, 1, 3, 1, true},
		{"concurrency 1 still yields one slot for the refresh budget", 1, 1, 8, 2, 1, 0, 1, true},
		{"the whole pool as the share", 25, 1, 1, 4, 25, 0, 4, false},
		{"an eighth of a large pool", 200, 1, 8, 2, 25, 175, 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := DefaultHotPolicy()
			p.RefreshMaxInflight = c.inflight
			p.PoolShareNum, p.PoolShareDen = c.shareNum, c.shareDen
			env := defaultTestEnv()
			env.Concurrency = c.concurrency
			if err := p.Validate(env); err != nil {
				t.Fatalf("a cap above the share must be CLAMPED, never refused (P-4): %v", err)
			}
			eff := p.Effective(env)
			if eff.Derived.PoolSlots != c.wantSlots {
				t.Errorf("pool_slots = %d, want %d", eff.Derived.PoolSlots, c.wantSlots)
			}
			if eff.Derived.PoolSlotsForeground != c.wantFore {
				t.Errorf("pool_slots_foreground = %d, want %d", eff.Derived.PoolSlotsForeground, c.wantFore)
			}
			if eff.Derived.RefreshMaxInflight != c.wantInflight {
				t.Errorf("refresh_max_inflight = %d, want %d", eff.Derived.RefreshMaxInflight, c.wantInflight)
			}
			if eff.Derived.RefreshMaxInflightClamped != c.wantClamped {
				t.Errorf("refresh_max_inflight_clamped = %v, want %v", eff.Derived.RefreshMaxInflightClamped, c.wantClamped)
			}
			// The reservation ceiling follows from the CLAMPED width, which is
			// what makes Q-9's arithmetic a bound rather than a hope.
			if want := int64(c.wantInflight) * p.MaxFileBytes; eff.Derived.MaxInflightBytes != want {
				t.Errorf("max_inflight_bytes = %d, want %d", eff.Derived.MaxInflightBytes, want)
			}
		})
	}
}

// TestHotPolicyValidateRefusalsNameBothNumbers is the loud-failure contract: an
// invalid value is refused, the refusal names the flag, and where a relation
// between two numbers is what failed, BOTH numbers are in the message. A
// refusal that says "invalid value" and nothing else sends the operator to the
// docs to guess which of their two numbers is wrong.
//
// The RELATION rows arm the feature (`enabled = true`): a relation failure is
// refused when the hot path would run and reported when it would not, which is
// S-11's reading and the reason `--cache-max-entry-bytes 1k` alone must not
// refuse an unrelated mount (TestNormalizeDoesNotRefuseAPreExistingFlagCombination).
func TestHotPolicyValidateRefusalsNameBothNumbers(t *testing.T) {
	cases := []struct {
		name     string
		enabled  bool
		mutate   func(*HotPolicy)
		env      func(*HotPolicyEnv)
		wantFlag string
		wantSubs []string
	}{
		{
			name:     "read weight zero",
			mutate:   func(p *HotPolicy) { p.WeightRead = 0 },
			wantFlag: "--hot.read-weight",
			wantSubs: []string{"0", "1"},
		},
		{
			name:     "edit weight not above read weight",
			enabled:  true,
			mutate:   func(p *HotPolicy) { p.WeightEdit = 1 },
			wantFlag: "--hot.edit-weight",
			wantSubs: []string{"1", "--hot.read-weight"},
		},
		{
			name:     "decay above one",
			mutate:   func(p *HotPolicy) { p.Decay = 1.5 },
			wantFlag: "--hot.decay",
			wantSubs: []string{"1.5", "0.9"},
		},
		{
			name:     "size rule above the per-entry cap (S-10)",
			enabled:  true,
			mutate:   func(p *HotPolicy) { p.MaxFileBytes = 64 << 20 },
			env:      func(e *HotPolicyEnv) { e.CacheMaxEntryBytes = 8 << 20 },
			wantFlag: "--hot.max-file-bytes",
			wantSubs: []string{"67108864", "8388608"},
		},
		{
			name:     "size rule above the cache bound (S-9)",
			enabled:  true,
			mutate:   func(p *HotPolicy) { p.MaxFileBytes = 4 << 20 },
			env:      func(e *HotPolicyEnv) { e.CacheMaxBytes, e.CacheMaxEntryBytes = 1<<20, 8<<20 },
			wantFlag: "--hot.max-file-bytes",
			wantSubs: []string{"4194304", "1048576"},
		},
		{
			name:     "backoff cap above the client's own op deadline (A.11)",
			enabled:  true,
			mutate:   func(p *HotPolicy) { p.BackoffMax = 45 * time.Second },
			wantFlag: "--hot.backoff-max-ms",
			wantSubs: []string{"45s", "30s"},
		},
		{
			name:     "refresh deadline above the client's own op deadline (A.8)",
			enabled:  true,
			mutate:   func(p *HotPolicy) { p.RefreshDeadline = time.Minute },
			wantFlag: "--hot.refresh-deadline",
			wantSubs: []string{"1m0s", "30s"},
		},
		{
			name:     "stop deadline below tick plus yield (A.6)",
			enabled:  true,
			mutate:   func(p *HotPolicy) { p.StopDeadline = 300 * time.Millisecond },
			wantFlag: "--hot.stop-deadline",
			wantSubs: []string{"300ms", "1s", "250ms"},
		},
		{
			name:     "backoff cap below its own base",
			enabled:  true,
			mutate:   func(p *HotPolicy) { p.BackoffMax = 100 * time.Millisecond },
			wantFlag: "--hot.backoff-max-ms",
			wantSubs: []string{"100ms", "250ms"},
		},
		{
			name:     "backoff cap of zero is a range failure on a disabled policy too",
			mutate:   func(p *HotPolicy) { p.BackoffMax = 0 },
			wantFlag: "--hot.backoff-max-ms",
			wantSubs: []string{"0s", "30s"},
		},
		{
			name:     "share above one",
			mutate:   func(p *HotPolicy) { p.PoolShareNum, p.PoolShareDen = 3, 2 },
			wantFlag: "--hot.pool-share",
			wantSubs: []string{"3/2"},
		},
		{
			name:     "zero share is not how the feature is turned off",
			mutate:   func(p *HotPolicy) { p.PoolShareNum = 0 },
			wantFlag: "--hot.pool-share",
			wantSubs: []string{"0/8", "--hot.enabled=false"},
		},
		{
			name:     "jitter vocabulary",
			mutate:   func(p *HotPolicy) { p.BackoffJitter = "sometimes" },
			wantFlag: "--hot.backoff-jitter",
			wantSubs: []string{"sometimes", "full", "none"},
		},
		{
			name:     "a size rule of zero is named rather than inferred",
			mutate:   func(p *HotPolicy) { p.MaxFileBytes = 0 },
			wantFlag: "--hot.max-file-bytes",
			wantSubs: []string{"--hot.enabled=false"},
		},
		{
			name:     "queue depth zero",
			mutate:   func(p *HotPolicy) { p.QueueMaxDepth = 0 },
			wantFlag: "--hot.queue-depth",
			wantSubs: []string{"256"},
		},
		{
			name:     "refresh width zero",
			mutate:   func(p *HotPolicy) { p.RefreshMaxInflight = 0 },
			wantFlag: "--hot.max-concurrent-refresh",
			wantSubs: []string{"2"},
		},
		{
			name:     "tracker entry bound zero",
			mutate:   func(p *HotPolicy) { p.TrackerMaxEntries = 0 },
			wantFlag: "--hot.max-entries",
			wantSubs: []string{"4096"},
		},
		{
			name:     "tracker byte bound zero",
			mutate:   func(p *HotPolicy) { p.TrackerMaxBytes = 0 },
			wantFlag: "--hot.max-tracker-bytes",
			wantSubs: []string{"1048576"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := DefaultHotPolicy()
			p.Enabled = c.enabled
			c.mutate(&p)
			env := defaultTestEnv()
			if c.env != nil {
				c.env(&env)
			}
			err := p.Validate(env)
			if err == nil {
				t.Fatalf("Validate accepted an invalid policy: nothing may fall back silently")
			}
			msg := err.Error()
			if !strings.Contains(msg, c.wantFlag) {
				t.Errorf("the refusal does not name %s: %s", c.wantFlag, msg)
			}
			if !strings.Contains(msg, "SPEC-hot-file-policy") {
				t.Errorf("the refusal does not cite the spec rule it enforces: %s", msg)
			}
			for _, sub := range c.wantSubs {
				if !strings.Contains(msg, sub) {
					t.Errorf("the refusal does not name %q: %s", sub, msg)
				}
			}
		})
	}
}

// TestHotPolicyValidateAcceptsTheLegalEdges guards the other direction: a
// refusal for a value the spec EXPLICITLY allows would be a defect in the
// surface, and two of these are load-bearing controls their own rows depend on.
func TestHotPolicyValidateAcceptsTheLegalEdges(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*HotPolicy)
		env    func(*HotPolicyEnv)
		why    string
	}{
		{
			name:   "decay exactly 1.0",
			mutate: func(p *HotPolicy) { p.Decay = 1.0 },
			why:    "AC-1's control arm is `hot_decay = 1.0`: a control that cannot be configured is not a control",
		},
		{
			name:   "the share is the whole pool",
			mutate: func(p *HotPolicy) { p.PoolShareNum, p.PoolShareDen = 1, 1 },
			why:    "P-3 fixes the DEFAULT share, not a ceiling on the knob; a share of 1 is expressible",
		},
		{
			name:   "a size rule exactly at the per-entry cap",
			mutate: func(p *HotPolicy) { p.MaxFileBytes = 64 << 20 },
			why:    "S-10 refuses only ABOVE the cap",
		},
		{
			name:   "a refresh width above the share",
			mutate: func(p *HotPolicy) { p.RefreshMaxInflight = 99 },
			why:    "P-4 clamps and reports this rather than refusing it",
		},
		{
			name:   "a backoff cap exactly at the op deadline",
			mutate: func(p *HotPolicy) { p.BackoffMax = 30 * time.Second },
			why:    "A.11 states the cap EQUALS DefaultOpTimeout at the defaults",
		},
		{
			name:   "a refresh deadline exactly at the op deadline",
			mutate: func(p *HotPolicy) { p.RefreshDeadline = 30 * time.Second },
			why:    "P-11 fixes one third as the default, not as a hard maximum",
		},
		{
			name:   "deterministic backoff for a test arm",
			mutate: func(p *HotPolicy) { p.BackoffJitter = HotJitterNone },
			why:    "the ladder's shape is only assertable without jitter",
		},
		{
			name:   "a size rule of one byte",
			mutate: func(p *HotPolicy) { p.MaxFileBytes = 1 },
			why:    "S-2's edge is inclusive and 1 byte is a legal (if useless) ceiling",
		},
		{
			name:   "a size rule above the cache bound while the cache is OFF",
			mutate: func(p *HotPolicy) { p.MaxFileBytes = 300 << 20 },
			env:    func(e *HotPolicyEnv) { e.CacheMaxBytes, e.CacheMaxEntryBytes = 0, 0 },
			why:    "D-6/S-11: with no cache the hot path reports cache_disabled and skips — it does not refuse the mount",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := DefaultHotPolicy()
			c.mutate(&p)
			env := defaultTestEnv()
			if c.env != nil {
				c.env(&env)
			}
			if err := p.Validate(env); err != nil {
				t.Errorf("Validate refused a legal value (%s): %v", c.why, err)
			}
		})
	}
}

// TestHotPolicyRefusesEveryPartiallySetField is the "no silent fallback" proof
// stated as a property rather than as a list: take the fully-defaulted policy,
// zero ONE field at a time, and require a refusal that names a flag and cites a
// spec rule. A field that could be left at its zero value and quietly resolved
// would be a knob whose effect the operator cannot predict from the flags they
// passed — which is the failure S-11 and P-0 clause 1 exist to prevent.
func TestHotPolicyRefusesEveryPartiallySetField(t *testing.T) {
	env := defaultTestEnv()
	typ := reflect.TypeOf(HotPolicy{})
	checked := 0
	for _, enabled := range []bool{false, true} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.Name == "Enabled" {
				// false IS the default, so a zero here is legal by construction.
				continue
			}
			checked++
			p := DefaultHotPolicy()
			p.Enabled = enabled
			v := reflect.ValueOf(&p).Elem().Field(i)
			v.Set(reflect.Zero(v.Type()))
			err := p.Validate(env)
			if err == nil {
				t.Errorf("zeroing %s (enabled=%v) produced no refusal: a partially set policy must be refused, never completed silently", f.Name, enabled)
				continue
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "bunker-fs: --hot.") {
				t.Errorf("zeroing %s produced a refusal that does not name the flag: %s", f.Name, msg)
			}
			if !strings.Contains(msg, "SPEC-hot-file-policy") {
				t.Errorf("zeroing %s produced a refusal that does not cite the spec rule: %s", f.Name, msg)
			}
		}
	}
	if checked < 40 {
		t.Fatalf("reflection walked %d field-values: the policy surface has shrunk and this test has stopped covering it", checked)
	}
	if DefaultHotPolicy().IsZero() {
		// A sanity check on the sentinel itself: the default policy must NOT be
		// mistaken for "no policy configured".
		t.Fatal("DefaultHotPolicy() reports itself as the zero value")
	}
}

// TestHotPolicyRelationFailuresAreReportedWhenDisabled is S-11 stated as a test,
// and it is a REGRESSION GUARD rather than a nicety: the defaults pin an 8 MiB
// size rule, so without it any caller who shrinks the CACHE — an existing flag,
// nothing to do with this row, with the hot path off and never to be armed —
// would have their mount refused by a bound they never set. Measured before the
// split: `bunker fs mount … --cache-max-entry-bytes 1024` exited 1 naming
// --hot.max-file-bytes. S-11: "the mount still works; the hot path is inert and
// reports itself as such (hot_state: misconfigured, with the two numbers)".
func TestHotPolicyRelationFailuresAreReportedWhenDisabled(t *testing.T) {
	env := defaultTestEnv()
	env.CacheMaxEntryBytes = 1024 // an operator shrinks the per-entry cap only
	p := DefaultHotPolicy()       // hot path OFF: the stock configuration

	// 1. The mount is NOT refused.
	if err := p.Validate(env); err != nil {
		t.Fatalf("a disabled policy was refused over a relation it will never use: %v", err)
	}
	// 2. The incoherence is nevertheless REPORTED, with both numbers.
	problems := p.RelationProblems(env)
	if len(problems) != 1 {
		t.Fatalf("a size rule of %d above a per-entry cap of %d produced %d problem(s), want 1", p.MaxFileBytes, env.CacheMaxEntryBytes, len(problems))
	}
	for _, want := range []string{"--hot.max-file-bytes", "8388608", "1024", "S-10"} {
		if !strings.Contains(problems[0], want) {
			t.Errorf("the reported problem does not name %q: %s", want, problems[0])
		}
	}
	// 3. And the state says so, so a reader of the status document cannot
	// mistake an inert knob for a silent one.
	eff := p.Effective(env)
	if eff.ConfigState != HotConfigMisconfigured {
		t.Errorf("config_state = %q, want %q (S-11)", eff.ConfigState, HotConfigMisconfigured)
	}
	if len(eff.Misconfigured) != 1 {
		t.Errorf("the effective report carries %d problem(s), want the same list Effective() resolved", len(eff.Misconfigured))
	}
	// 4. The moment the feature is ARMED, the same configuration is refused.
	armed := DefaultHotPolicy()
	armed.Enabled = true
	err := armed.Validate(env)
	if err == nil {
		t.Fatal("arming the hot path accepted a size rule above the per-entry cap: S-10 must refuse it then")
	}
	if !strings.Contains(err.Error(), "S-10") {
		t.Errorf("the armed refusal does not cite S-10: %v", err)
	}
}

// TestHotPolicyRelationProblemsAreEmptyForTheDefaults is the other side of the
// same guard: the stock configuration must carry no problem at all, or every
// mount would report itself misconfigured and the state would mean nothing.
func TestHotPolicyRelationProblemsAreEmptyForTheDefaults(t *testing.T) {
	for _, env := range []HotPolicyEnv{
		defaultTestEnv(),
		{Concurrency: 1, OpTimeout: DefaultOpTimeout, CacheMaxBytes: 16 << 20, CacheMaxEntryBytes: 16 << 20},
		{Concurrency: 200, OpTimeout: time.Minute, CacheMaxBytes: 1 << 30, CacheMaxEntryBytes: 1 << 30},
	} {
		if got := DefaultHotPolicy().RelationProblems(env); len(got) != 0 {
			t.Errorf("the default policy reports %d problem(s) against %+v: %v", len(got), env, got)
		}
	}
}

// TestParsePoolShare covers the one flag whose value is not a scalar.
func TestParsePoolShare(t *testing.T) {
	for _, ok := range []struct {
		in       string
		num, den int
	}{{"1/8", 1, 8}, {"2/8", 2, 8}, {" 1/4 ", 1, 4}, {"1/1", 1, 1}} {
		num, den, err := ParsePoolShare(ok.in)
		if err != nil {
			t.Errorf("ParsePoolShare(%q) refused a fraction: %v", ok.in, err)
			continue
		}
		if num != ok.num || den != ok.den {
			t.Errorf("ParsePoolShare(%q) = %d/%d, want %d/%d", ok.in, num, den, ok.num, ok.den)
		}
	}
	for _, bad := range []string{"", "8", "1/8/2", "one/eight", "1/", "/8"} {
		if _, _, err := ParsePoolShare(bad); err == nil {
			t.Errorf("ParsePoolShare(%q) accepted a value it cannot represent: it must refuse, never default", bad)
		}
	}
}

// TestHotPolicyEffectiveJSONShape pins the document a consumer reads: the
// effective policy is only "readable at runtime" if its keys and its units are
// the documented ones. Nanosecond counts carry their unit IN THE KEY, so a
// machine cannot mistake 300 s for 300 ns.
func TestHotPolicyEffectiveJSONShape(t *testing.T) {
	p := DefaultHotPolicy()
	p.Enabled = true
	eff := p.Effective(defaultTestEnv())
	raw, err := json.Marshal(eff)
	if err != nil {
		t.Fatalf("marshal effective policy: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal effective policy: %v", err)
	}
	for _, key := range []string{"enabled", "config_state", "configured", "derived", "pool_share", "queue_replacement", "size_rule_inclusive"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("the effective policy document has no %q key", key)
		}
	}
	derived, _ := doc["derived"].(map[string]any)
	for _, key := range []string{"pool_slots", "pool_slots_foreground", "refresh_max_inflight_configured", "refresh_max_inflight", "refresh_max_inflight_clamped", "max_inflight_bytes", "half_life_ms"} {
		if _, ok := derived[key]; !ok {
			t.Errorf("the derived block has no %q key: a derived bound nobody prints is the same defect one layer down (PRD §2.7)", key)
		}
	}
	configured, _ := doc["configured"].(map[string]any)
	for _, key := range []string{"read_weight", "edit_weight", "decay", "decay_step_ns", "tracker_max_entries", "tracker_max_bytes", "max_file_bytes", "queue_max_depth", "refresh_max_inflight_configured", "pool_share_num", "backoff_base_ns", "backoff_max_ns", "backoff_jitter", "refresh_deadline_ns", "yield_after_ns"} {
		if _, ok := configured[key]; !ok {
			t.Errorf("the configured block has no %q key", key)
		}
	}
	if got := configured["decay_step_ns"]; got != float64(DefaultHotDecayStep) {
		t.Errorf("decay_step_ns = %v, want %d nanoseconds", got, DefaultHotDecayStep)
	}
	if eff.ConfigState != HotConfigEnabled {
		t.Errorf("config_state = %q with the feature enabled and a cache present, want %q", eff.ConfigState, HotConfigEnabled)
	}
}

// TestHotPolicyConfigStateNamesTheConfiguration covers the three states this
// CONFIG surface can be in. The runtime states (armed|disarmed|stopped|
// misconfigured) are BFS-037's and must not be reachable from here: a config row
// that reported "stopped" would be reporting a lifecycle it does not own.
func TestHotPolicyConfigStateNamesTheConfiguration(t *testing.T) {
	off := DefaultHotPolicy().Effective(defaultTestEnv())
	if off.ConfigState != HotConfigDisabled {
		t.Errorf("a disabled policy reports config_state=%q, want %q", off.ConfigState, HotConfigDisabled)
	}
	on := DefaultHotPolicy()
	on.Enabled = true
	if got := on.Effective(defaultTestEnv()).ConfigState; got != HotConfigEnabled {
		t.Errorf("an enabled policy with a cache reports config_state=%q, want %q", got, HotConfigEnabled)
	}
	env := defaultTestEnv()
	env.CacheMaxBytes, env.CacheMaxEntryBytes = 0, 0
	if got := on.Effective(env).ConfigState; got != HotConfigCacheDisabled {
		t.Errorf("an enabled policy with NO cache reports config_state=%q, want %q (D-6: a skip reason, not a refusal)", got, HotConfigCacheDisabled)
	}
	for _, st := range []string{off.ConfigState, HotConfigEnabled, HotConfigCacheDisabled} {
		switch st {
		case "armed", "disarmed", "stopped", "misconfigured":
			t.Errorf("the config surface reported the runtime state %q: that vocabulary is BFS-037's (§5.5 Q-15)", st)
		}
	}
}
