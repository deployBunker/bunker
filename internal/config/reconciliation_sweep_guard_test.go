package config

// REV-BUNKER-P1-PATCH: the reconciliation surface grows a boot-time
// bulk-destroy guard. These tests pin its SHAPE (armed by default, threshold
// three, disable expressed as a disable) and its fail-closed resolution on a
// zero-valued struct — the shape a hand-built config has, where a "guard: bool"
// defaulting to false would have been the hole itself.

import (
	"strings"
	"testing"
)

func TestReconciliationSweepGuard_TrueByDefaultAndThreeIsTheThreshold(t *testing.T) {
	rc := DefaultConfig().Agent.Reconciliation
	if !rc.SweepGuardEnabled() {
		t.Error("the sweep guard must be armed in the default config")
	}
	if rc.OrphanSweepGuardDisabled {
		t.Error("the default config must not ship the guard disabled")
	}
	if rc.UnprovenOrphanLimit != DefaultUnprovenOrphanLimit {
		t.Errorf("UnprovenOrphanLimit = %d, want DefaultUnprovenOrphanLimit (%d)",
			rc.UnprovenOrphanLimit, DefaultUnprovenOrphanLimit)
	}
	if rc.Mode != ReconcileModeDestroy {
		t.Errorf("Mode = %q, want %q — the row keeps the documented default and guards it", rc.Mode, ReconcileModeDestroy)
	}
}

func TestReconciliationSweepGuard_ZeroValueIsArmedNotUnguarded(t *testing.T) {
	var rc ReconciliationConfig
	if !rc.SweepGuardEnabled() {
		t.Error("a zero-valued ReconciliationConfig must report the guard ARMED — fail-closed has to survive " +
			"not being configured")
	}
	if rc.UnprovenOrphanLimit != 0 {
		t.Errorf("a zero-valued limit must stay 0 (the strictest policy), got %d", rc.UnprovenOrphanLimit)
	}
	if err := rc.Validate(); err != nil {
		t.Errorf("zero-valued config must validate: %v", err)
	}
	if rc.Mode != ReconcileModeDestroy {
		t.Errorf("Validate must normalise the empty mode to destroy, got %q", rc.Mode)
	}
}

func TestReconciliationSweepGuard_NegativeLimitIsRefused(t *testing.T) {
	rc := &ReconciliationConfig{UnprovenOrphanLimit: -1}
	err := rc.Validate()
	if err == nil {
		t.Fatal("a negative unproven_orphan_limit must be refused: there is no 'unlimited' encoding")
	}
	if !strings.Contains(err.Error(), "unproven_orphan_limit") {
		t.Errorf("error must name the key, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "orphan_sweep_guard_disabled") {
		t.Errorf("error must point at the explicit disable knob, got %q", err.Error())
	}
}

func TestReconciliationSweepGuard_ZeroLimitValidates(t *testing.T) {
	rc := &ReconciliationConfig{UnprovenOrphanLimit: 0}
	if err := rc.Validate(); err != nil {
		t.Errorf("limit 0 (refuse every unproven sweep) is a legitimate policy: %v", err)
	}
	if rc.SweepGuardEnabled() != true {
		t.Error("limit 0 must not disable the guard")
	}
}
