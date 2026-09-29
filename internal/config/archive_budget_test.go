package config

import (
	"math"
	"testing"
	"time"
)

// DF-BUNKER-81: the size -> budget derivation behind the destroy home archive.
// The three properties that matter are pinned here, without a host or a large
// home:
//
//  1. no size ever yields a budget short enough to SIGKILL a large archive
//     (the floor, and the floor is what a size-UNKNOWN destroy gets);
//  2. the budget GROWS with the home (a 4 GiB home gets more than a 4 MiB one)
//     and is capped, not unbounded;
//  3. the client deadline always outlives the daemon's archive budget
//     (ordering), and never falls below the daemon's request budget.

func TestArchiveBudgetForHomeSize(t *testing.T) {
	const mib = int64(1) << 20
	const gib = int64(1) << 30

	tests := []struct {
		name      string
		homeBytes int64
		want      time.Duration
	}{
		{"unknown size falls back to the floor", 0, ArchiveBudgetMin},
		{"negative size is unknown too", -1, ArchiveBudgetMin},
		{"tiny home is clamped up to the floor", 4 * mib, ArchiveBudgetMin},
		{"a 688M rootless-docker home gets the floor, not 28s", 688 * mib, ArchiveBudgetMin},
		{"base+derive below the floor", 1 * gib, ArchiveBudgetMin},
		{"a 4 GiB home derives base + 1024s", 4 * gib, ArchiveBudgetBase + 1024*time.Second},
		{"a 50 GiB home derives base + 12800s", 50 * gib, ArchiveBudgetBase + 12800*time.Second},
		{"a pathological size is capped", 1 << 50, ArchiveBudgetMax},
		{"math.MaxInt64 does not overflow", math.MaxInt64, ArchiveBudgetMax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ArchiveBudgetForHomeSize(tt.homeBytes); got != tt.want {
				t.Errorf("ArchiveBudgetForHomeSize(%d) = %s, want %s", tt.homeBytes, got, tt.want)
			}
		})
	}
}

// TestArchiveBudgetForHomeSizeIsMonotoneAndBounded pins the shape the two
// clamps exist for: the budget never DECREASES as the home grows, and it is
// never outside [min, max].
func TestArchiveBudgetForHomeSizeIsMonotoneAndBounded(t *testing.T) {
	const mib = int64(1) << 20
	prev := time.Duration(0)
	for size := int64(0); size <= int64(200)<<30; size += 64 * mib {
		got := ArchiveBudgetForHomeSize(size)
		if got < ArchiveBudgetMin || got > ArchiveBudgetMax {
			t.Fatalf("ArchiveBudgetForHomeSize(%d) = %s, outside [%s, %s]", size, got, ArchiveBudgetMin, ArchiveBudgetMax)
		}
		if got < prev {
			t.Fatalf("ArchiveBudgetForHomeSize(%d) = %s decreased from %s", size, got, prev)
		}
		prev = got
	}
}

// TestInterfaceBudgetsAreOrdered is the invariant that makes the whole fix
// work: the client deadline must outlive the daemon's archive budget for the
// SAME home, or the client cancels a request the daemon is still legitimately
// serving (the pre-fix shape: a 30s client deadline SIGKILLed a ~30s gzip).
func TestInterfaceBudgetsAreOrdered(t *testing.T) {
	const mib = int64(1) << 20
	sizes := []int64{0, 1, 100 * mib, 688 * mib, 1 << 30, 4 << 30, 50 << 30, math.MaxInt64}
	for _, size := range sizes {
		archive := ArchiveBudgetForHomeSize(size)
		client := DestroyRequestTimeoutForHomeSize(size)
		if client <= archive {
			t.Errorf("size %d: client deadline %s does not outlive the archive budget %s", size, client, archive)
		}
		if client < DestroyRequestTimeoutMin {
			t.Errorf("size %d: client deadline %s is below the floor %s", size, client, DestroyRequestTimeoutMin)
		}
		if client < DefaultServerRequestTimeout {
			t.Errorf("size %d: client deadline %s is below the daemon request budget %s", size, client, DefaultServerRequestTimeout)
		}
		if got := client - archive; got < DestroyRequestMargin {
			t.Errorf("size %d: client deadline keeps only %s over the archive budget, want at least %s", size, got, DestroyRequestMargin)
		}
	}
}
