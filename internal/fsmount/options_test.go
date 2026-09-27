package fsmount

import (
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/fsclient"
)

// baseOptions is the smallest option set Normalize accepts, so every case below
// varies exactly one thing from a mount that works.
func baseOptions(t *testing.T) Options {
	t.Helper()
	return Options{Mountpoint: t.TempDir(), BaseURL: "http://127.0.0.1:18481/dav"}
}

// armedHot is the default policy with the feature turned ON, for the cases whose
// refusal only applies when the hot path would actually run (see
// HotPolicy.Validate: a relation failure is refused when armed and REPORTED when
// not, which is what keeps `--cache-max-entry-bytes 1k` from refusing a mount
// that never asked for the hot path — S-11).
func armedHot() fsclient.HotPolicy {
	p := fsclient.DefaultHotPolicy()
	p.Enabled = true
	return p
}

// TestNormalizeFillsTheBoundsWithTheSpecDefaults covers the "every flag has a
// safe default" half of BFS-044's acceptance. The entry bound is the one that
// matters most: a byte bound alone does not bound a directory (BFS-031), so a
// normalized mount must ALWAYS carry an entry bound, including when the caller
// never mentioned one.
func TestNormalizeFillsTheBoundsWithTheSpecDefaults(t *testing.T) {
	o := baseOptions(t)
	if err := o.Normalize(); err != nil {
		t.Fatalf("Normalize refused a default option set: %v", err)
	}
	if o.CacheMaxEntries != DefaultCacheMaxEntries {
		t.Errorf("CacheMaxEntries = %d, want %d: the entry bound is always in force", o.CacheMaxEntries, DefaultCacheMaxEntries)
	}
	if o.CacheMaxInFlight != DefaultCacheMaxInFlight {
		t.Errorf("CacheMaxInFlight = %d, want %d", o.CacheMaxInFlight, DefaultCacheMaxInFlight)
	}
	// A zero hot policy means "no policy configured" and takes the defaults.
	if o.Hot.IsZero() {
		t.Fatal("Normalize left the hot policy at the zero value: a mount would then obey zeros that the spec never pinned")
	}
	def := fsclient.DefaultHotPolicy()
	if o.Hot != def {
		t.Errorf("the defaulted policy is not fsclient.DefaultHotPolicy():\n got %+v\nwant %+v", o.Hot, def)
	}
	if o.Hot.Enabled {
		t.Error("the hot path defaults ON: it is performance-only (P-0), unmeasured, and the brief forbids a default a stock client or an old script would notice")
	}
	// And the resolution of that policy against the resolved option set.
	eff := o.EffectiveHotPolicy()
	if eff.Derived.PoolSlots != 3 || eff.Derived.PoolSlotsForeground != 22 {
		t.Errorf("pool resolution = %d slots / %d foreground, want 3 / 22 at --concurrency %d", eff.Derived.PoolSlots, eff.Derived.PoolSlotsForeground, o.Concurrency)
	}
	if eff.Derived.MaxInflightBytes != 2*fsclient.DefaultHotMaxFileBytes {
		t.Errorf("reservation ceiling = %d, want 2 x %d (Q-9/A.5)", eff.Derived.MaxInflightBytes, fsclient.DefaultHotMaxFileBytes)
	}
}

// TestNormalizeRefusesInvalidValuesLoudly is the second half: an invalid value
// is REFUSED, and the refusal names the flag, the value given and the value the
// spec pins. Nothing in this surface is repaired into something quieter — the
// one exception the spec fixes is P-4's clamp, and that is reported as a clamp
// rather than being a silent substitution (see
// TestEffectiveHotPolicyClampsAndReportsTheClamp).
func TestNormalizeRefusesInvalidValuesLoudly(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Options)
		wantFlag string
		wantSubs []string
	}{
		{
			name:     "cache entry bound negative",
			mutate:   func(o *Options) { o.CacheMaxEntries = -1 },
			wantFlag: "--cache-max-entries",
			wantSubs: []string{"-1", "16384", "BFS-031"},
		},
		{
			name:     "staged-refresh width negative",
			mutate:   func(o *Options) { o.CacheMaxInFlight = -2 },
			wantFlag: "--cache-max-inflight",
			wantSubs: []string{"-2", "2"},
		},
		{
			name:     "concurrency negative",
			mutate:   func(o *Options) { o.Concurrency = -3 },
			wantFlag: "--concurrency",
			wantSubs: []string{"-3", "25"},
		},
		{
			name:     "poll interval negative",
			mutate:   func(o *Options) { o.PollInterval = -time.Second },
			wantFlag: "--poll-interval",
			wantSubs: []string{"-1s", "2s"},
		},
		{
			name:     "declared silence deadline negative",
			mutate:   func(o *Options) { o.InvalidateIdleTimeout = -time.Second },
			wantFlag: "--invalidate-idle-timeout",
			wantSubs: []string{"-1s"},
		},
		{
			name:     "size rule above the per-entry cap, ARMED (S-10)",
			mutate:   func(o *Options) { o.Hot = armedHot(); o.Hot.MaxFileBytes = DefaultCacheMaxEntryBytes + 1 },
			wantFlag: "--hot.max-file-bytes",
			wantSubs: []string{"67108865", "67108864"},
		},
		{
			name: "size rule above the cache bound, ARMED (S-9)",
			mutate: func(o *Options) {
				o.CacheMaxBytes = 1 << 20
				o.CacheMaxEntryBytes = 4 << 20
				o.Hot = armedHot()
				o.Hot.MaxFileBytes = 2 << 20
			},
			wantFlag: "--hot.max-file-bytes",
			wantSubs: []string{"2097152", "1048576"},
		},
		{
			name:     "tracker entry bound zero",
			mutate:   func(o *Options) { o.Hot = fsclient.DefaultHotPolicy(); o.Hot.TrackerMaxEntries = 0 },
			wantFlag: "--hot.max-entries",
			wantSubs: []string{"4096"},
		},
		{
			name:     "queue depth zero",
			mutate:   func(o *Options) { o.Hot = fsclient.DefaultHotPolicy(); o.Hot.QueueMaxDepth = 0 },
			wantFlag: "--hot.queue-depth",
			wantSubs: []string{"256"},
		},
		{
			name:     "read weight zero",
			mutate:   func(o *Options) { o.Hot = fsclient.DefaultHotPolicy(); o.Hot.WeightRead = 0 },
			wantFlag: "--hot.read-weight",
			wantSubs: []string{"0", "1"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := baseOptions(t)
			c.mutate(&o)
			err := o.Normalize()
			if err == nil {
				t.Fatal("Normalize accepted an invalid value: nothing may fall back silently")
			}
			msg := err.Error()
			if !strings.Contains(msg, c.wantFlag) {
				t.Errorf("the refusal does not name %s: %s", c.wantFlag, msg)
			}
			for _, sub := range c.wantSubs {
				if !strings.Contains(msg, sub) {
					t.Errorf("the refusal does not name %q: %s", sub, msg)
				}
			}
		})
	}
}

// TestPartialHotPolicyIsRefusedNotCompleted is the no-silent-fallback rule at
// the option-set boundary: a policy that mentions SOME knobs is a caller bug,
// and completing it with defaults would make the mount obey a value the operator
// never wrote. Only the ALL-ZERO policy means "unset".
func TestPartialHotPolicyIsRefusedNotCompleted(t *testing.T) {
	o := baseOptions(t)
	def := fsclient.DefaultHotPolicy()
	def.Decay = 0 // one knob missing
	o.Hot = def
	err := o.Normalize()
	if err == nil {
		t.Fatal("a partially populated hot policy was silently completed")
	}
	if !strings.Contains(err.Error(), "--hot.decay") {
		t.Errorf("the refusal does not name the missing knob: %v", err)
	}
}

// TestEffectiveConfigReportsTheResolvedOptionSet is the "effective values are
// readable at runtime" half, asserted at the source: the same call the status
// document uses. It covers the flag that had no surface before this row — the
// cache's ENTRY bound — and the invalidation cadence, in the spellings an
// operator reads.
func TestEffectiveConfigReportsTheResolvedOptionSet(t *testing.T) {
	o := baseOptions(t)
	o.CacheMaxEntries = 4
	o.CacheMaxInFlight = 3
	o.CacheMaxBytes = 1 << 20
	o.CacheMaxEntryBytes = 8 << 10
	o.CacheMaxAge = 90 * time.Minute
	o.Concurrency = 8
	o.Invalidation = "poll"
	o.PollInterval = 1500 * time.Millisecond
	o.InvalidateIdleTimeout = 45 * time.Second
	o.OnConflict = fsclient.OnConflictRefuse
	o.Snapshot = true
	hot := fsclient.DefaultHotPolicy()
	hot.Enabled = true
	hot.MaxFileBytes = 8 << 10 // S-10: must fit the resolved per-entry cap
	hot.RefreshMaxInflight = 9 // P-4: clamped to the share, never refused
	o.Hot = hot
	if err := o.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	cfg := o.EffectiveConfig()
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"cache_max_entries", cfg.CacheMaxEntries, 4},
		{"cache_max_inflight", cfg.CacheMaxInFlight, 3},
		{"cache_max_entry_bytes", cfg.CacheMaxEntryBytes, int64(8 << 10)},
		{"cache_max_age_ms", cfg.CacheMaxAgeMS, int64(90 * 60 * 1000)},
		{"concurrency", cfg.Concurrency, 8},
		{"invalidation", cfg.Invalidation, "poll"},
		{"poll_interval_ms", cfg.PollIntervalMS, int64(1500)},
		{"invalidation_idle_timeout_ms", cfg.InvalidationIdleTimeoutMS, int64(45000)},
		{"snapshot", cfg.Snapshot, true},
		{"hot.enabled", cfg.Hot.Enabled, true},
		{"hot.state", cfg.Hot.ConfigState, fsclient.HotConfigEnabled},
		{"hot.pool_slots", cfg.Hot.Derived.PoolSlots, 1}, // floor(8/8) = 1
		{"hot.pool_slots_foreground", cfg.Hot.Derived.PoolSlotsForeground, 7},
		{"hot.refresh_max_inflight", cfg.Hot.Derived.RefreshMaxInflight, 1}, // clamped from 9
		{"hot.refresh_max_inflight_clamped", cfg.Hot.Derived.RefreshMaxInflightClamped, true},
		{"hot.max_inflight_bytes", cfg.Hot.Derived.MaxInflightBytes, int64(8 << 10)}, // 1 x 8 KiB
		{"hot.size_rule_inclusive", cfg.Hot.SizeRuleInclusive, true},
		{"hot.queue_replacement", cfg.Hot.QueueReplacement, "score"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("config %s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestEffectiveHotPolicyClampsAndReportsTheClamp pins P-4's shape at the option
// set: a cap above the share is CLAMPED AND REPORTED rather than refused, and
// the clamp is visible as a boolean so a reader can tell a value they asked for
// from one the ceiling imposed.
func TestEffectiveHotPolicyClampsAndReportsTheClamp(t *testing.T) {
	o := baseOptions(t)
	o.Concurrency = 25
	hot := fsclient.DefaultHotPolicy()
	hot.Enabled = true
	hot.RefreshMaxInflight = 6
	o.Hot = hot
	if err := o.Normalize(); err != nil {
		t.Fatalf("a cap above the share was refused instead of clamped: %v", err)
	}
	eff := o.EffectiveHotPolicy()
	if eff.Derived.RefreshMaxInflightConfigured != 6 || eff.Derived.RefreshMaxInflight != 3 {
		t.Errorf("clamp = %d -> %d, want 6 -> 3 (P-4: min(cap, floor(25/8)))", eff.Derived.RefreshMaxInflightConfigured, eff.Derived.RefreshMaxInflight)
	}
	if !eff.Derived.RefreshMaxInflightClamped {
		t.Error("the clamp happened but was not reported: an unreported clamp is a silent fallback")
	}
}

// TestNormalizeDoesNotChangeTheExistingDefaultSurface guards the backward
// compatibility the brief demands ("must not regress the existing mount flags",
// "must not make any feature default-on in a way a stock client or an old script
// notices"): every value a pre-BFS-044 mount resolved to must still resolve to
// it, and the new surface must be inert by default.
func TestNormalizeDoesNotChangeTheExistingDefaultSurface(t *testing.T) {
	o := baseOptions(t)
	if err := o.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if o.CacheMaxBytes != DefaultCacheMaxBytes {
		t.Errorf("CacheMaxBytes = %d, want the existing default %d", o.CacheMaxBytes, DefaultCacheMaxBytes)
	}
	if o.CacheMaxEntryBytes != DefaultCacheMaxEntryBytes {
		t.Errorf("CacheMaxEntryBytes = %d, want the existing default %d", o.CacheMaxEntryBytes, DefaultCacheMaxEntryBytes)
	}
	if o.Concurrency != DefaultConcurrency {
		t.Errorf("Concurrency = %d, want %d", o.Concurrency, DefaultConcurrency)
	}
	if o.PollInterval != DefaultPollInterval {
		t.Errorf("PollInterval = %s, want %s", o.PollInterval, DefaultPollInterval)
	}
	if o.Invalidation != "auto" {
		t.Errorf("Invalidation = %q, want \"auto\"", o.Invalidation)
	}
	if o.CacheMaxAge != DefaultCacheMaxAge {
		t.Errorf("CacheMaxAge = %s, want %s", o.CacheMaxAge, DefaultCacheMaxAge)
	}
	// The new knobs are inert: nothing in this row's surface changes a request
	// until an operator asks for it.
	if o.Hot.Enabled {
		t.Error("the new hot-file feature is on by default")
	}
	if o.InvalidateIdleTimeout != 0 {
		t.Errorf("InvalidateIdleTimeout = %s, want 0 (derive from the server's declaration)", o.InvalidateIdleTimeout)
	}
}

// TestNormalizeDoesNotRefuseAPreExistingFlagCombination is the regression the
// BFS-044 probe actually found, kept as a test so it cannot come back.
//
// The default policy pins an 8 MiB size rule. A caller who shrinks only the
// CACHE — `--cache-max-entry-bytes 1024`, a flag that has existed since BFS-005
// and has nothing to do with the hot path — used to be REFUSED by
// `--hot.max-file-bytes (8388608) exceeds the cache's per-entry cap`, naming a
// knob they never set, for a feature that is off. Measured live:
//
//	bunker fs mount … --cache-max-entry-bytes 1024   ->  exit 1
//
// S-11 forbids exactly this: "Neither refusal is a hot-path feature gate. The
// mount still works; the hot path is inert and reports itself as such
// (hot_state: misconfigured, with the two numbers)." So Normalize accepts it and
// the effective config carries the state AND both numbers, while an ARMED policy
// with the same incoherence is still refused outright (covered in
// internal/fsclient's TestHotPolicyRelationFailuresAreReportedWhenDisabled).
func TestNormalizeDoesNotRefuseAPreExistingFlagCombination(t *testing.T) {
	o := baseOptions(t)
	o.CacheMaxEntryBytes = 1024
	if err := o.Normalize(); err != nil {
		t.Fatalf("a mount that never asked for the hot path was refused over it: %v", err)
	}
	eff := o.EffectiveHotPolicy()
	if eff.Enabled {
		t.Fatal("the hot path is armed: this test is about the disabled case")
	}
	if eff.ConfigState != fsclient.HotConfigMisconfigured {
		t.Errorf("config_state = %q, want %q — an inert knob must not be a silent one (S-11)", eff.ConfigState, fsclient.HotConfigMisconfigured)
	}
	if len(eff.Misconfigured) != 1 {
		t.Fatalf("the effective config carries %d problem(s), want 1 naming the two numbers", len(eff.Misconfigured))
	}
	for _, want := range []string{"--hot.max-file-bytes", "8388608", "1024"} {
		if !strings.Contains(eff.Misconfigured[0], want) {
			t.Errorf("the reported problem does not name %q: %s", want, eff.Misconfigured[0])
		}
	}
	// The same options with the feature ARMED must still refuse the mount.
	armed := o
	armed.Hot = armedHot()
	if err := armed.Normalize(); err == nil {
		t.Fatal("arming the hot path with a size rule above the per-entry cap was accepted: S-10 must refuse it")
	}
}
