package config

import "testing"

// TestConfig_DefaultImageSpecPolicy pins the GAP-064 image-spec policy
// defaults: the feature ships enabled with a bounded build timeout and a
// server-local cache directory.
func TestConfig_DefaultImageSpecPolicy(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Agent.ImageSpec.Enabled {
		t.Error("agent.image_spec.enabled should default to true")
	}
	if cfg.Agent.ImageSpec.CacheDir == "" {
		t.Error("agent.image_spec.cache_dir should have a non-empty default")
	}
	if cfg.Agent.ImageSpec.BuildTimeout <= 0 {
		t.Errorf("agent.image_spec.build_timeout should be positive, got %v", cfg.Agent.ImageSpec.BuildTimeout)
	}
	if cfg.Agent.ImageSpec.BuildTimeout > imagespecMaxBuildTimeout {
		t.Errorf("agent.image_spec.build_timeout %v exceeds hard ceiling %v", cfg.Agent.ImageSpec.BuildTimeout, imagespecMaxBuildTimeout)
	}
}

// imagespecMaxBuildTimeout mirrors the imagespec package's hard ceiling so
// config tests fail if the two drift. Keep in sync with
// imagespec.DefaultBuildTimeout semantics: a configured timeout above one
// hour is a footgun, not a feature.
const imagespecMaxBuildTimeout = 60 * 60 * 1e9 // 1h in nanoseconds
