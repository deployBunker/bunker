package config

import (
	"strings"
	"testing"
)

func TestMetricsEnabledRequiresLoopbackRESTAddress(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "127.12.34.56:8080", "[::1]:8080"} {
		cfg := DefaultConfig()
		cfg.Server.MetricsEnabled = true
		cfg.Server.RESTAddr = addr
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(%q): %v", addr, err)
		}
	}
	for _, addr := range []string{":8080", "0.0.0.0:8080", "192.0.2.1:8080", "localhost:8080", ""} {
		cfg := DefaultConfig()
		cfg.Server.MetricsEnabled = true
		cfg.Server.RESTAddr = addr
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "server.metrics_enabled") {
			t.Errorf("Validate(%q) error = %v, want metrics loopback validation error", addr, err)
		}
	}
}
