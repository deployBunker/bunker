package cli

// PERF-013: the destroy size probe must tolerate a COLD disk-usage snapshot
// cache (internal/server diskusage.go answers DiskUsedBytes=0 immediately
// until the daemon's first home walk lands) by polling AgentMetrics inside
// the existing destroyHomeSizeProbeTimeout budget, and only then falling
// back to the size-unknown floor. The regression: the single-shot read
// accepted the cold-cache 0, sized the deadline at the floor, and the
// client deadline expired long before a multi-GB archive finished — the
// same client-cancels-first compounding class as INT-CI-050 (QA-BUNKER-37/39).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
)

func TestDestroyHomeSizeProbeWithRetry(t *testing.T) {
	cold := connect.NewError(connect.CodeUnavailable, errors.New("cold"))
	tests := []struct {
		name       string
		probe      func(context.Context) (uint64, error)
		wantBytes  uint64
		wantErr    bool
		wantCalls  int
		callBudget int // upper bound asserted when > 0
	}{
		{
			name: "first answer wins without polling",
			probe: func(context.Context) (uint64, error) {
				return 688 << 20, nil
			},
			wantBytes: 688 << 20,
			wantCalls: 1,
		},
		{
			name:      "cold cache: 0 then the real size is accepted",
			probe:     seqProbe(0, 688<<20),
			wantBytes: 688 << 20,
			wantCalls: 2,
		},
		{
			name: "always 0 inside the window gives up and reports unknown",
			probe: func(context.Context) (uint64, error) {
				return 0, nil
			},
			wantErr:    true,
			wantCalls:  2,  // window-honoring loop, not a single shot...
			callBudget: 50, // ...and not unbounded hammering either
		},
		{
			name: "probe error is single-attempt (fail closed to unknown)",
			probe: func(context.Context) (uint64, error) {
				return 0, cold
			},
			wantErr:   true,
			wantCalls: 1,
		},
		{
			name:      "error after warm 0s is also single-attempt",
			probe:     seqProbeErr(0, cold),
			wantErr:   true,
			wantCalls: 2, // the one retry sees the error and stops
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origWindow := destroyHomeSizePollWindow
			origBackoff := destroyHomeSizePollBackoff
			destroyHomeSizePollWindow = 20 * time.Millisecond
			destroyHomeSizePollBackoff = time.Millisecond
			t.Cleanup(func() {
				destroyHomeSizePollWindow = origWindow
				destroyHomeSizePollBackoff = origBackoff
			})

			calls := 0
			probe := func(ctx context.Context) (uint64, error) {
				calls++
				return tt.probe(ctx)
			}

			started := time.Now()
			got, err := destroyHomeSizeProbeWithRetry(context.Background(), probe)
			elapsed := time.Since(started)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("destroyHomeSizeProbeWithRetry() = %d, nil error, want a size-unknown error", got)
				}
				if got != 0 {
					t.Errorf("error path returned %d, want 0", got)
				}
			} else {
				if err != nil {
					t.Fatalf("destroyHomeSizeProbeWithRetry() error: %v", err)
				}
				if got != tt.wantBytes {
					t.Errorf("destroyHomeSizeProbeWithRetry() = %d, want %d", got, tt.wantBytes)
				}
			}
			if calls < tt.wantCalls {
				t.Errorf("probe calls = %d, want at least %d", calls, tt.wantCalls)
			}
			if tt.callBudget > 0 && calls > tt.callBudget {
				t.Errorf("probe calls = %d exceeds the bounded budget %d", calls, tt.callBudget)
			}
			if elapsed >= 5*time.Second {
				t.Errorf("poll ran %s — it must stay far inside the 15s probe budget", elapsed)
			}
		})
	}
}

// TestDestroyHomeSizePollNeverExceedsProbeTimeout is the budget pin: even a
// probe that serves 0 forever must give up inside destroyHomeSizeProbeTimeout
// (the window the probe already owns) — production values included.
func TestDestroyHomeSizePollNeverExceedsProbeTimeout(t *testing.T) {
	// Production-size window is 15s and this probe always answers 0 with no
	// delay, so the loop must exit on the WINDOW, long before 15s elapses.
	origWindow := destroyHomeSizePollWindow
	destroyHomeSizePollWindow = 50 * time.Millisecond
	t.Cleanup(func() { destroyHomeSizePollWindow = origWindow })

	probe := func(context.Context) (uint64, error) { return 0, nil }
	started := time.Now()
	_, err := destroyHomeSizeProbeWithRetry(context.Background(), probe)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("an always-0 window must report the size as unknown")
	}
	if elapsed >= destroyHomeSizeProbeTimeout {
		t.Errorf("poll burned %s, exceeding the destroyHomeSizeProbeTimeout budget %s", elapsed, destroyHomeSizeProbeTimeout)
	}
	if elapsed >= 5*time.Second {
		t.Errorf("poll ran %s — the window was not honored", elapsed)
	}
}

// TestDestroyCommand_ColdCacheSizedDeadline is the RED regression at command
// level: a daemon whose metrics go 0, 0, then the real 688M footprint (the
// cold-cache shape, warmed by the background walk mid-poll) must get the
// SIZED deadline, not the floor — and once, unlike today, the window is
// sized honestly, the old single-shot code cannot pass.
func TestDestroyCommand_ColdCacheSizedDeadline(t *testing.T) {
	const mib = uint64(1) << 20
	mock := &destroySizeMockServer{metricsSeq: []uint64{0, 0, 688 * mib}}
	newDestroySizeServer(t, mock)

	cmd := NewDestroyCommand()
	cmd.SetArgs([]string{"abc12345"})
	var execErr error
	output := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("Execute: %v", execErr)
	}
	if mock.metricsCalls < 3 {
		t.Errorf("AgentMetrics probe calls = %d, want at least 3 (cold cache must be polled past)", mock.metricsCalls)
	}
	if mock.destroyReq == nil {
		t.Fatal("no DestroyAgent request reached the daemon")
	}
	for _, want := range []string{"home 688.0 MB", "deadline"} {
		if !strings.Contains(output, want) {
			t.Errorf("output is missing %q, got:\n%s", want, output)
		}
	}
}

// seqProbe builds a probe returning the given answers in order; the last
// answer repeats.
func seqProbe(seq ...uint64) func(context.Context) (uint64, error) {
	i := 0
	return func(context.Context) (uint64, error) {
		used := len(seq) - 1
		if i < used {
			used = i
		}
		i++
		return seq[used], nil
	}
}

// seqProbeErr is seqProbe with a trailing error entry.
func seqProbeErr(seq ...any) func(context.Context) (uint64, error) {
	i := 0
	return func(context.Context) (uint64, error) {
		idx := i
		if idx >= len(seq) {
			idx = len(seq) - 1
		}
		i++
		switch v := seq[idx].(type) {
		case error:
			return 0, v
		case uint64:
			return v, nil
		case int:
			return uint64(v), nil
		default:
			return 0, errors.New("bad seq entry")
		}
	}
}
