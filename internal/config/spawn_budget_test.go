package config

import (
	"testing"
	"time"
)

// TestSpawnRequestTimeout_DerivesFromMeasuredStages pins the derivation
// (DOGFOOD-015): the budget is the sum of the named stage allowances, not a
// magic number, and it is comfortably longer than the daemon's ordinary
// request_timeout — which is the bound that killed `bunker spawn
// --tools-at-spawn` at 300.8s on a cold host.
func TestSpawnRequestTimeout_DerivesFromMeasuredStages(t *testing.T) {
	got := SpawnRequestTimeout(DefaultServerRequestTimeout)
	want := SpawnBudgetBase + SpawnRootlessInstallBudget + SpawnToolsImageBuildBudget
	if got != want {
		t.Fatalf("SpawnRequestTimeout(%s) = %s, want %s (base + rootless install + tools image build)",
			DefaultServerRequestTimeout, got, want)
	}
	if got <= DefaultServerRequestTimeout {
		t.Fatalf("spawn budget %s is not longer than the ordinary request_timeout %s — the flat-bound defect is back",
			got, DefaultServerRequestTimeout)
	}
	// The two cold stages that the measurement blamed must each be materially
	// present: half a minute of headroom would not have saved the run.
	if SpawnRootlessInstallBudget < 2*time.Minute {
		t.Errorf("rootless-install allowance %s is too small for a measured 60-90s+ ~93MB install", SpawnRootlessInstallBudget)
	}
	if SpawnToolsImageBuildBudget < 5*time.Minute {
		t.Errorf("tools image-build allowance %s is too small: the measured run was still inside this stage at 300s", SpawnToolsImageBuildBudget)
	}
	// Deterministic for the same input: the middleware and the ServerInfo
	// capability report must never disagree about the budget in force.
	if again := SpawnRequestTimeout(DefaultServerRequestTimeout); again != got {
		t.Errorf("not deterministic: %s then %s", got, again)
	}
}

// TestSpawnRequestTimeout_ConstantsFitUnderCap keeps the promised cap honest:
// if a future edit inflates an allowance past SpawnBudgetMax, the derivation
// would silently clamp and the per-stage arithmetic would stop being the
// answer. Fail here instead, where the fix is a deliberate decision.
func TestSpawnRequestTimeout_ConstantsFitUnderCap(t *testing.T) {
	sum := SpawnBudgetBase + SpawnRootlessInstallBudget + SpawnToolsImageBuildBudget
	if sum > SpawnBudgetMax {
		t.Fatalf("stage allowances sum to %s, above SpawnBudgetMax %s — raise the cap deliberately or lower an allowance", sum, SpawnBudgetMax)
	}
}

// TestSpawnRequestTimeout_OrdinaryFloorWins: a spawn must never be bounded
// more tightly than the ordinary per-RPC timeout it is carved out of.
func TestSpawnRequestTimeout_OrdinaryFloorWins(t *testing.T) {
	long := SpawnBudgetMax + 5*time.Minute
	if got := SpawnRequestTimeout(long); got != long {
		t.Errorf("SpawnRequestTimeout(%s) = %s, want %s (the operator's longer ordinary bound wins)", long, got, long)
	}
	// Just under the floor: the derived budget still wins.
	short := time.Minute
	if got := SpawnRequestTimeout(short); got <= short {
		t.Errorf("SpawnRequestTimeout(%s) = %s, want the derived budget", short, got)
	}
}

// TestSpawnRequestTimeout_NonPositiveYieldsDerived: an unset/hand-built config
// must not produce a zero (or negative) timeout — that would kill every spawn
// the instant it started.
func TestSpawnRequestTimeout_NonPositiveYieldsDerived(t *testing.T) {
	want := SpawnBudgetBase + SpawnRootlessInstallBudget + SpawnToolsImageBuildBudget
	for _, ordinary := range []time.Duration{0, -time.Second, -time.Hour} {
		if got := SpawnRequestTimeout(ordinary); got != want {
			t.Errorf("SpawnRequestTimeout(%s) = %s, want %s", ordinary, got, want)
		} else if got <= 0 {
			t.Errorf("SpawnRequestTimeout(%s) = %s, must be positive", ordinary, got)
		}
	}
}

// TestSpawnClientTimeout_OutlivesDaemonBudget pins the ordering law: the CLI
// deadline is the daemon's budget plus the margin, so the client can never
// become the thing that cancels a spawn the daemon is still working on
// (QA-BUNKER-58 measured 20-42s of work after a 300s client deadline).
func TestSpawnClientTimeout_OutlivesDaemonBudget(t *testing.T) {
	for _, budget := range []time.Duration{time.Minute, 10 * time.Minute, 30 * time.Minute} {
		got := SpawnClientTimeout(budget)
		if got != budget+SpawnClientMargin {
			t.Errorf("SpawnClientTimeout(%s) = %s, want %s", budget, got, budget+SpawnClientMargin)
		}
		if got <= budget {
			t.Errorf("SpawnClientTimeout(%s) = %s: client deadline does not outlive the daemon budget", budget, got)
		}
	}
	// A daemon that reports nothing (pre-capability) still gets a deadline
	// derived from the same budget — never "no deadline", never a short one.
	fallback := SpawnClientTimeout(0)
	if want := SpawnRequestTimeout(DefaultServerRequestTimeout) + SpawnClientMargin; fallback != want {
		t.Errorf("SpawnClientTimeout(0) = %s, want %s (derived daemon budget + margin)", fallback, want)
	}
	if fallback <= 305*time.Second {
		t.Errorf("SpawnClientTimeout(0) = %s — a flat 300s-class client deadline is the DOGFOOD-015 defect", fallback)
	}
}

// TestEffectiveSpawnRequestTimeout covers the daemon-side resolution: the
// operator knob wins, an unset knob derives from the ordinary timeout, and a
// nil config still answers (the middleware must never receive a zero budget).
func TestEffectiveSpawnRequestTimeout(t *testing.T) {
	t.Run("knob wins", func(t *testing.T) {
		cfg := &Config{Server: ServerConfig{RequestTimeout: DefaultServerRequestTimeout, SpawnRequestTimeout: 45 * time.Minute}}
		if got := cfg.EffectiveSpawnRequestTimeout(); got != 45*time.Minute {
			t.Errorf("EffectiveSpawnRequestTimeout() = %s, want the 45m override", got)
		}
	})
	t.Run("unset derives", func(t *testing.T) {
		cfg := &Config{Server: ServerConfig{RequestTimeout: DefaultServerRequestTimeout}}
		if got, want := cfg.EffectiveSpawnRequestTimeout(), SpawnRequestTimeout(DefaultServerRequestTimeout); got != want {
			t.Errorf("EffectiveSpawnRequestTimeout() = %s, want derived %s", got, want)
		}
	})
	t.Run("nil config derives", func(t *testing.T) {
		var cfg *Config
		if got := cfg.EffectiveSpawnRequestTimeout(); got <= 0 {
			t.Errorf("nil config yielded %s, must derive a positive budget", got)
		}
	})
}

// TestValidate_SpawnRequestTimeout covers the escape hatch's validation: a
// non-positive value means "derive", and a positive value at or below the
// ordinary request timeout is refused (it would kill spawns at a bound the
// daemon applies to every other RPC).
func TestValidate_SpawnRequestTimeout(t *testing.T) {
	base := func() *Config {
		return &Config{Server: ServerConfig{
			GRPCAddr:       "127.0.0.1:0",
			RESTAddr:       "127.0.0.1:0",
			RequestTimeout: DefaultServerRequestTimeout,
		}}
	}
	t.Run("unset is accepted", func(t *testing.T) {
		if err := base().Validate(); err != nil {
			t.Fatalf("Validate with unset spawn budget: %v", err)
		}
	})
	t.Run("longer override accepted", func(t *testing.T) {
		cfg := base()
		cfg.Server.SpawnRequestTimeout = 45 * time.Minute
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate with 45m spawn budget: %v", err)
		}
	})
	t.Run("shorter override refused", func(t *testing.T) {
		cfg := base()
		cfg.Server.SpawnRequestTimeout = DefaultServerRequestTimeout
		err := cfg.Validate()
		if err == nil {
			t.Fatal("Validate accepted a spawn budget equal to the ordinary request timeout")
		}
		if !contains(err.Error(), "spawn_request_timeout") {
			t.Errorf("error does not name the knob: %v", err)
		}
	})
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
