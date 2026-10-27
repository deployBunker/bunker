package main

import (
	"os"
	"strings"
	"testing"
)

// ── FLEET-INFRA-1: `bunkerd port-pool check` verb ────────────────────────

// TestPortPoolCheckVerb drives the new positional verb through the same
// run() harness TestBunkerdPositionalArgs uses: healthy config → OK verdict,
// mis-related config → boot-refusal verdict with the exact numbers.
func TestPortPoolCheckVerb(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })

	t.Run("ok verdict on valid config", func(t *testing.T) {
		cfgPath := writeTestConfig(t, "server:\n  grpc_addr: \"127.0.0.1:9090\"\nauth:\n  enabled: false\nagent:\n  max_agents: 50\n")
		setArgs(t, "--config", cfgPath, "port-pool", "check")
		var runErr error
		out := captureStdout(t, func() {
			runErr = run()
		})
		if runErr != nil {
			t.Fatalf("run port-pool check: %v", runErr)
		}
		for _, want := range []string{"port-pool check", "pool:", "capacity:", "100 ranges", "verdict:", "OK"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q, got:\n%s", want, out)
			}
		}
	})

	t.Run("refusal verdict names the relation", func(t *testing.T) {
		// max_agents above the 100-range default pool: the same Validate a
		// boot runs must refuse, and the check must render it.
		cfgPath := writeTestConfig(t, "server:\n  grpc_addr: \"127.0.0.1:9090\"\nauth:\n  enabled: false\nagent:\n  max_agents: 500\n")
		setArgs(t, "--config", cfgPath, "port-pool", "check")
		var runErr error
		out := captureStdout(t, func() {
			runErr = run()
		})
		if runErr == nil {
			t.Fatal("expected error for max_agents > pool capacity, got nil")
		}
		if !strings.Contains(runErr.Error(), "exceeds the port pool capacity") {
			t.Errorf("error %q should name the exhausted relation", runErr)
		}
		for _, want := range []string{"BOOT REFUSED", "exceeds the port pool capacity", "lower max_agents"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q, got:\n%s", want, out)
			}
		}
	})

	t.Run("missing subcommand prints usage", func(t *testing.T) {
		setArgs(t, "--config", "/nonexistent/x.yaml", "port-pool")
		runErr := run()
		if runErr == nil || !strings.Contains(runErr.Error(), "usage: bunkerd port-pool check") {
			t.Fatalf("error = %v, want usage hint", runErr)
		}
	})

	t.Run("three-token verb is refused", func(t *testing.T) {
		setArgs(t, "--config", "/nonexistent/x.yaml", "port-pool", "check", "extra")
		runErr := run()
		if runErr == nil {
			t.Fatal("expected refusal for a three-token verb, got nil")
		}
		// Either guard may fire (the outer positional guard or the verb's
		// own); both must refuse WITHOUT loading the missing config.
		if strings.Contains(runErr.Error(), "load config") {
			t.Errorf("refusal %q must precede config.Load", runErr)
		}
	})
}
