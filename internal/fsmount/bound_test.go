package fsmount

import "testing"

// The read-bound model and the decision it feeds are the correctness of BFS-025,
// so they are pure and tested without a mount, a kernel or a server. The live
// arms (probes/bfs025-truncated-read) exercise the same rule end to end through a
// real mount; these tests are what keeps the rule intact when the mount is
// refactored around it.

func TestBoundRegistryTracksWhatTheKernelWasTold(t *testing.T) {
	r := newBoundRegistry()

	// A path the kernel has never asked about has no bound to violate: unknown is
	// an answer, and guessing one would refuse reads on the guess.
	if _, known := r.Bound("src/main.go"); known {
		t.Fatal("an unpublished path must report no bound")
	}

	// An attrs reply IS the kernel's i_size from that moment on.
	r.Published("src/main.go", 5)
	if got, known := r.Bound("src/main.go"); !known || got != 5 {
		t.Fatalf("after publishing 5: bound=%d known=%v, want 5/true", got, known)
	}

	// A write the kernel acknowledged grows i_size to at least that end.
	r.Wrote("src/main.go", 40)
	if got, _ := r.Bound("src/main.go"); got != 40 {
		t.Fatalf("after writing 40 bytes: bound=%d, want 40", got)
	}
	// A smaller write never shrinks it (§i_size is a max()).
	r.Wrote("src/main.go", 7)
	if got, _ := r.Bound("src/main.go"); got != 40 {
		t.Fatalf("after a smaller write: bound=%d, want 40 (a write cannot shrink the kernel's size)", got)
	}

	// A new attrs reply supersedes both: the kernel sets i_size from the reply,
	// so growth recorded before it no longer bounds anything.
	r.Published("src/main.go", 12)
	if got, _ := r.Bound("src/main.go"); got != 12 {
		t.Fatalf("after re-publishing 12: bound=%d, want 12 (the reply replaces the growth)", got)
	}

	// A path that is only written (no attrs reply yet) still has a lower bound:
	// the kernel grew i_size for the write.
	r.Wrote("src/late.go", 9)
	if got, known := r.Bound("src/late.go"); !known || got != 9 {
		t.Fatalf("write-only path: bound=%d known=%v, want 9/true", got, known)
	}
}

func TestJudgeServed(t *testing.T) {
	cases := []struct {
		name  string
		bound int64
		known bool
		n     int64
		want  servedVerdict
	}{
		{"unknown bound is not a violation", 0, false, 4096, servedAgrees},
		{"equal is the fast path", 5, true, 5, servedAgrees},
		{"content longer than the bound refuses", 5, true, 82, servedBoundTooSmall},
		{"content shorter than the bound is served and corrected", 82, true, 5, servedBoundTooLarge},
		{"empty content under a bound is served", 82, true, 0, servedBoundTooLarge},
		{"a zero bound with content refuses", 0, true, 1, servedBoundTooSmall},
		{"empty content under a zero bound agrees", 0, true, 0, servedAgrees},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := judgeServed(tc.bound, tc.known, tc.n); got != tc.want {
				t.Fatalf("judgeServed(bound=%d known=%v n=%d) = %v, want %v", tc.bound, tc.known, tc.n, got, tc.want)
			}
		})
	}
}
