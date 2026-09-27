package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/deployBunker/bunker/internal/fsclient"
	"github.com/deployBunker/bunker/internal/fsmount"
)

// TestFSMountExposesEveryHotFlagWithItsSpecDefault is BFS-044's flag table as a
// test: for every knob SPEC-hot-file-policy.md pins, the mount command must
// carry a flag whose pflag DEFAULT is that number. It is asserted rather than
// eyeballed because the two ways this surface rots are a flag that never got
// wired and a default that drifted from the value the client obeys — and the
// second is invisible until someone mounts with the policy armed.
func TestFSMountExposesEveryHotFlagWithItsSpecDefault(t *testing.T) {
	cmd := newFSMountCommand()
	cases := []struct {
		flag string
		want string
	}{
		{"hot.enabled", fmt.Sprint(fsclient.DefaultHotEnabled)},
		{"hot.read-weight", fmt.Sprint(fsclient.DefaultHotWeightRead)},
		{"hot.edit-weight", fmt.Sprint(fsclient.DefaultHotWeightEdit)},
		{"hot.decay", fmt.Sprint(fsclient.DefaultHotDecay)},
		{"hot.decay-step", fmt.Sprint(fsclient.DefaultHotDecayStep)},
		{"hot.score-ceiling", fmt.Sprint(fsclient.DefaultHotScoreCeiling)},
		{"hot.read-touch-window", fmt.Sprint(fsclient.DefaultHotReadTouchWindow)},
		{"hot.flush-interval", fmt.Sprint(fsclient.DefaultHotFlushInterval)},
		{"hot.max-entries", fmt.Sprint(fsclient.DefaultHotTrackerMaxEntries)},
		{"hot.max-tracker-bytes", fmt.Sprint(fsclient.DefaultHotTrackerMaxBytes)},
		{"hot.max-file-bytes", fmt.Sprint(fsclient.DefaultHotMaxFileBytes)},
		{"hot.queue-depth", fmt.Sprint(fsclient.DefaultHotQueueMaxDepth)},
		{"hot.queue-max-wait", fmt.Sprint(fsclient.DefaultHotQueueMaxWait)},
		{"hot.max-concurrent-refresh", fmt.Sprint(fsclient.DefaultHotRefreshMaxInflight)},
		{"hot.pool-share", fsclient.DefaultHotPoolShare()},
		{"hot.backoff-base-ms", fmt.Sprint(fsclient.DefaultHotBackoffBase.Milliseconds())},
		{"hot.backoff-max-ms", fmt.Sprint(fsclient.DefaultHotBackoffMax.Milliseconds())},
		{"hot.backoff-factor", fmt.Sprint(fsclient.DefaultHotBackoffFactor)},
		{"hot.backoff-jitter", fsclient.DefaultHotJitter()},
		{"hot.refresh-deadline", fmt.Sprint(fsclient.DefaultHotRefreshDeadline)},
		{"hot.reacquire-window", fmt.Sprint(fsclient.DefaultHotRefreshReacquireWindow)},
		{"hot.yield-after", fmt.Sprint(fsclient.DefaultHotYieldAfter)},
		{"hot.stop-deadline", fmt.Sprint(fsclient.DefaultHotStopDeadline)},
		{"hot.tick-interval", fmt.Sprint(fsclient.DefaultHotTickInterval)},
		{"hot.pool-pressure-ticks", fmt.Sprint(fsclient.DefaultHotPoolPressureTicks)},
		// The two bounds BFS-044 adds to the pre-existing surface: the cache's
		// ENTRY bound (a byte bound alone does not bound a directory — BFS-031)
		// and the width of the staged-refresh window.
		{"cache-max-entries", fmt.Sprint(fsmount.DefaultCacheMaxEntries)},
		{"cache-max-inflight", fmt.Sprint(fsmount.DefaultCacheMaxInFlight)},
		// The invalidation feature knob: 0 means DERIVE the silence deadline
		// from the period the server declares (BFS-041 §8.1), which is the
		// Invalidator's documented sentinel and not a fallback.
		{"invalidate-idle-timeout", fmt.Sprint(fsmount.DefaultInvalidateIdleTimeout)},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		seen[c.flag] = true
		f := cmd.Flags().Lookup(c.flag)
		if f == nil {
			t.Errorf("--%s is not exposed: a knob the policy pins with no flag is a number an operator cannot see or move", c.flag)
			continue
		}
		if f.DefValue != c.want {
			t.Errorf("--%s default = %q, want %q (the spec's pinned value)", c.flag, f.DefValue, c.want)
		}
		if strings.TrimSpace(f.Usage) == "" {
			t.Errorf("--%s has no help text: a flag nobody can read is a flag nobody can use", c.flag)
		}
	}
	// And nothing in the hot group may exist WITHOUT being in the table: a flag
	// this test does not know about is a flag whose default is unchecked.
	if n := countFlagsWithPrefix(cmd, "hot."); n != 25 {
		t.Errorf("the --hot group has %d flags, the table names 25: an unchecked flag is an unpinned default", n)
	}
}

// countFlagsWithPrefix counts the registered flags whose name starts with prefix.
func countFlagsWithPrefix(cmd *cobra.Command, prefix string) int {
	n := 0
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if strings.HasPrefix(f.Name, prefix) {
			n++
		}
	})
	return n
}

// TestHotFlagsToPolicyConversion covers the one place a flag's SPELLING could
// disagree with the value the client obeys: the two whose names declare
// milliseconds and the one whose value is a fraction. A value that cannot be
// converted must be an error, never a default.
func TestHotFlagsToPolicyConversion(t *testing.T) {
	hot := fsclient.DefaultHotPolicy()
	if err := hotFlagsToPolicy(&hot, "1/4", 100, 2000); err != nil {
		t.Fatalf("conversion refused a legal set: %v", err)
	}
	if hot.PoolShareNum != 1 || hot.PoolShareDen != 4 {
		t.Errorf("pool share = %d/%d, want 1/4", hot.PoolShareNum, hot.PoolShareDen)
	}
	if hot.BackoffBase != 100*time.Millisecond || hot.BackoffMax != 2*time.Second {
		t.Errorf("backoff = %s..%s, want 100ms..2s: the value the client obeys must be the value the flag named", hot.BackoffBase, hot.BackoffMax)
	}
	for _, bad := range []string{"", "8", "one/eight", "1/8/2"} {
		if err := hotFlagsToPolicy(&hot, bad, 250, 30000); err == nil {
			t.Errorf("--hot.pool-share %q converted without error: it must refuse rather than default", bad)
		}
	}
}

// TestFSMountRefusesAnInvalidHotValueBeforeAnyRequest drives the command the way
// an operator does. The point is the ORDER as much as the message: the refusal
// must happen in Normalize, before the mount is attempted, so a misconfigured
// knob can never half-mount a tree first.
func TestFSMountRefusesAnInvalidHotValueBeforeAnyRequest(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantFlag string
		wantSub  string
	}{
		{"decay out of range", []string{"--hot.decay", "2.0"}, "--hot.decay", "0.9"},
		{"size rule above the entry cap", []string{"--hot.max-file-bytes", "999999999"}, "--hot.max-file-bytes", "67108864"},
		{"negative entry bound", []string{"--cache-max-entries", "-1"}, "--cache-max-entries", "16384"},
		{"share above one", []string{"--hot.pool-share", "3/2"}, "--hot.pool-share", "3/2"},
		{"unparseable share", []string{"--hot.pool-share", "an-eighth"}, "--hot.pool-share", "an-eighth"},
		{"negative silence deadline", []string{"--invalidate-idle-timeout", "-5s"}, "--invalidate-idle-timeout", "-5s"},
		{"jitter vocabulary", []string{"--hot.backoff-jitter", "sometimes"}, "--hot.backoff-jitter", "sometimes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := newFSMountCommand()
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			args := append([]string{t.TempDir(), "--url", "http://127.0.0.1:1/dav"}, c.args...)
			cmd.SetArgs(args)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("the mount command accepted %v: an invalid value must be refused", c.args)
			}
			msg := err.Error()
			if !strings.Contains(msg, c.wantFlag) {
				t.Errorf("the refusal does not name %s: %s", c.wantFlag, msg)
			}
			if !strings.Contains(msg, c.wantSub) {
				t.Errorf("the refusal does not name the offending value %q: %s", c.wantSub, msg)
			}
			// A refusal must not have reached the transport: the error is the
			// validation one, and no mount was attempted.
			if strings.Contains(msg, "bind refused") {
				t.Errorf("the refusal happened AFTER the bind attempt, so a bad knob can half-mount a tree: %s", msg)
			}
		})
	}
}

// TestFSStatusPrintsTheEffectiveConfig is the other half of "the effective
// values are readable at runtime": the block must reach the screen a person
// reads, with the numbers — including the two DERIVED ones (the share's slot
// arithmetic and the reservation ceiling the size rule implies), because those
// are the figures a reader would otherwise have to compute from three flags.
func TestFSStatusPrintsTheEffectiveConfig(t *testing.T) {
	hot := fsclient.DefaultHotPolicy()
	hot.Enabled = true
	hot.MaxFileBytes = 8 << 10
	hot.RefreshMaxInflight = 9
	o := fsmount.Options{
		BaseURL:            "http://127.0.0.1:18481/dav",
		CacheMaxBytes:      1 << 20,
		CacheMaxEntries:    4,
		CacheMaxInFlight:   3,
		CacheMaxEntryBytes: 8 << 10,
		CacheMaxAge:        time.Hour,
		Concurrency:        8,
		Invalidation:       "poll",
		PollInterval:       1500 * time.Millisecond,
		OnConflict:         fsclient.OnConflictRefuse,
		Hot:                hot,
	}
	st := &fsclient.Status{Mount: "m1", Mode: "poll", Endpoint: o.BaseURL}
	st.Config = o.EffectiveConfig()
	var buf bytes.Buffer
	printStatus(&buf, st)
	got := buf.String()
	for _, want := range []string{
		"config       : cache 1048576 B / 4 entries (entry cap 8192, staged 3, age 1h0m0s)",
		"invalidation : mode=poll poll_interval=1.5s idle_timeout=derived from the declared heartbeat",
		"hot policy   : enabled=true state=enabled share=1/8 slots=1 (foreground 7) refresh_inflight=1 clamped=true reserve=8192 B",
		"hot weights  : read=1 edit=8 decay=0.9 per 5m0s (half-life 32m54s)",
		"hot bounds   : tracker 4096 entries / 1048576 B, size<=8192 B (inclusive=true), queue 256 (score, wait 5m0s), ceiling 10000",
		"hot timing   : tick=1s yield=250ms stop_deadline=1.5s refresh_deadline=10s reacquire=30s touch_window=5s flush=30s",
		"hot backoff  : 250ms x2 cap 30s jitter=full, pressure_ticks=5",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the status view is missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestFSStatusToleratesAStatusDocumentFromBeforeThisRow guards the one backward
// compatibility this block could break: a status.json written by a build that
// predates BFS-044 has no config block at all, and `bunker fs status` must still
// print the record rather than a zero-filled lie or a panic. (It prints zeros
// for the new block, which is the honest reading of "that mount did not report
// one" — the figures the old build DID report are untouched.)
func TestFSStatusToleratesAStatusDocumentFromBeforeThisRow(t *testing.T) {
	old := &fsclient.Status{
		Mount: "m0", Mode: "auto", Endpoint: "http://127.0.0.1:18481/dav",
	}
	old.Cache.UsedBytes = 39122
	var buf bytes.Buffer
	printStatus(&buf, old) // must not panic
	got := buf.String()
	if !strings.Contains(got, "used_bytes=39122") {
		t.Errorf("the pre-BFS-044 figures are not printed: %s", got)
	}
	if !strings.Contains(got, "config       : not reported by this mount") {
		t.Errorf("a status document with no config block reads as a wall of unexplained zeros:\n%s", got)
	}
}
