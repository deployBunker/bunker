package agent

import "testing"

// Regression test for a boundary bug found while verifying GAP-140: the gap
// finder's break predicate used `cand+subIDRangeSize-1`, so an occupant that
// started on the LAST id of the candidate block (cand+subIDRangeSize-1) was
// treated as "clear of the block" and the block was placed over it — sharing
// exactly one id. That reintroduced the very isolation break GAP-140 fixes.
// The block is half-open [cand, cand+subIDRangeSize), so an occupant must start
// at or after cand+subIDRangeSize to clear it.
func TestChooseSubIDRangeStart_NeverSharesAnOccupantsId(t *testing.T) {
	base := int64(subIDPoolBase)
	one := int64(subIDRangeSize)

	for _, occStart := range []int64{
		base,
		base + one - 1, // the boundary that used to slip through
		base + one,
		base + one + 1,
		base + 2*one,
	} {
		entries := []subIDEntry{{name: "occ", start: occStart, count: 2}}
		got, ok := chooseSubIDRangeStart(entries)
		if !ok {
			t.Fatalf("occStart=%d: expected a free block", occStart)
		}
		newEnd := got + one
		occEnd := occStart + 2
		if got < occEnd && occStart < newEnd {
			t.Fatalf("occStart=%d: allocated block [%d,%d) overlaps occupant [%d,%d)",
				occStart, got, newEnd, occStart, occEnd)
		}
	}
}

// The allocated block must also clear an occupant that ends exactly where the
// block would begin (adjacent, no shared id) — confirming the fix does not
// over-correct into needless skipping.
func TestChooseSubIDRangeStart_AllowsAdjacentOccupant(t *testing.T) {
	base := int64(subIDPoolBase)
	one := int64(subIDRangeSize)
	columns := []subIDEntry{{name: "occ", start: base, count: one}}
	got, ok := chooseSubIDRangeStart(columns)
	if !ok {
		t.Fatal("expected a free block")
	}
	if got != base+one {
		t.Fatalf("allocated %d, want the adjacent free block %d", got, base+one)
	}
}
