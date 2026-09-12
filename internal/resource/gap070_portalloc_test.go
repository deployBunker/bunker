package resource

import (
	"errors"
	"testing"
)

func testAllocator(t *testing.T) *PortAllocator {
	t.Helper()
	pa, err := NewPortAllocator(10000, 19999, 100)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}
	return pa
}

// TestReserveExactRangeAndIdempotency pins the exact-reservation contract.
func TestReserveExactRangeAndIdempotency(t *testing.T) {
	pa := testAllocator(t)
	full := pa.MaxRanges()

	if err := pa.Reserve("alpha", 10000, 10099); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	start, end, ok := pa.AllocatedRange("alpha")
	if !ok || start != 10000 || end != 10099 {
		t.Fatalf("AllocatedRange = %d-%d (ok=%v), want 10000-10099", start, end, ok)
	}
	if !pa.Has("alpha") {
		t.Error("Has(alpha) = false after Reserve")
	}
	if got, want := pa.Available(), full-1; got != want {
		t.Errorf("Available() = %d, want %d", got, want)
	}

	// Idempotent for the same agent + same range (repeated replay/adopt).
	if err := pa.Reserve("alpha", 10000, 10099); err != nil {
		t.Errorf("repeated Reserve of the same range must be idempotent: %v", err)
	}
	// A different range for the same agent is a conflict.
	if err := pa.Reserve("alpha", 10100, 10199); err == nil {
		t.Error("Reserve of a second range for the same agent must fail")
	}
	// Another agent cannot take a held range.
	err := pa.Reserve("beta", 10000, 10099)
	if err == nil {
		t.Fatal("Reserve of an already-held range must fail")
	}
	if !errors.Is(err, ErrRangeUnavailable) {
		t.Errorf("error = %v, want ErrRangeUnavailable", err)
	}

	// Free returns it to the pool, and it can be reserved again.
	pa.Free("alpha")
	if err := pa.Reserve("beta", 10000, 10099); err != nil {
		t.Fatalf("Reserve after Free: %v", err)
	}
}

// TestReserveValidation covers every geometry rejection.
func TestReserveValidation(t *testing.T) {
	cases := []struct {
		name       string
		start, end uint32
	}{
		{"below pool", 9999, 10098},
		{"above pool", 19900, 20000},
		{"inverted", 10100, 10050},
		{"unaligned", 10050, 10149},
		{"wrong size", 10000, 10050},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pa := testAllocator(t)
			if err := pa.Reserve("agent", tc.start, tc.end); err == nil {
				t.Fatalf("Reserve(%d,%d) succeeded, want a validation error", tc.start, tc.end)
			}
			if pa.Has("agent") {
				t.Error("a rejected reservation must not allocate anything")
			}
			if got, want := pa.Available(), pa.MaxRanges(); got != want {
				t.Errorf("Available() = %d, want %d (pool untouched)", got, want)
			}
		})
	}
}

// TestValidateRangeExported mirrors the Reserve checks for callers that want to
// pre-validate persisted metadata.
func TestValidateRangeExported(t *testing.T) {
	pa := testAllocator(t)
	if err := pa.ValidateRange(10000, 10099); err != nil {
		t.Errorf("ValidateRange(valid) = %v, want nil", err)
	}
	if err := pa.ValidateRange(10050, 10149); err == nil {
		t.Error("ValidateRange(unaligned) = nil, want an error")
	}
}

// TestRestoreMatchesReserve proves Restore is the replay/adopt path with the
// same validation and idempotency, and that a restored range is never handed
// out twice.
func TestRestoreMatchesReserve(t *testing.T) {
	pa := testAllocator(t)

	if err := pa.Restore("adopted", 12300, 12399); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := pa.Restore("adopted", 12300, 12399); err != nil {
		t.Errorf("repeated Restore must be idempotent: %v", err)
	}
	if err := pa.Restore("adopted", 10000, 10099); err == nil {
		t.Error("Restore of a different range for the same agent must fail")
	}
	if err := pa.Restore("other", 12300, 12399); err == nil {
		t.Error("Restore of an already-reserved range must fail")
	}
	if err := pa.Restore("bad", 12345, 12345); err == nil {
		t.Error("Restore must validate the range geometry")
	}

	// No double allocation: exhaust the pool and confirm 12300 never appears.
	allocated := 0
	for i := 0; pa.Available() > 0; i++ {
		id := "fresh-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		s, _, err := pa.Allocate(id)
		if err != nil {
			t.Fatalf("Allocate(%s): %v", id, err)
		}
		allocated++
		if s == 12300 {
			t.Fatal("restored range 12300 was double-allocated")
		}
	}
	if allocated == 0 {
		t.Fatal("pool was already exhausted before the loop")
	}
	if !pa.Has("adopted") {
		t.Error("the adopted reservation was lost while exhausting the pool")
	}
}
