package cli

// Tests for fail-closed binding (GAP-093).
//
// The keystone property is NEGATIVE: a mutating command with no explicit
// binding must refuse, even when the shared config HAS an active_server —
// because that shared default is exactly how one session re-targets another.
// The static test below is what keeps this true as commands are added: it
// parses every cli command file and fails if a MUTATING command's RunE still
// reads cfg.ActiveServer outside the binding helpers.

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSessionScopedTarget_FlagWins(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "env-server")
	got, err := SessionScopedTarget("flag-server", "shared-default")
	if err != nil {
		t.Fatal(err)
	}
	if got != "flag-server" {
		t.Errorf("flag must win over env and shared default, got %q", got)
	}
}

func TestSessionScopedTarget_EnvSecond(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "env-server")
	got, err := SessionScopedTarget("", "shared-default")
	if err != nil {
		t.Fatal(err)
	}
	if got != "env-server" {
		t.Errorf("env must win over the shared default, got %q", got)
	}
}

// TestSessionScopedTarget_NeverFallsBackToSharedDefault is THE test: with a
// shared default present and nothing explicit, the resolver must refuse —
// returning the shared default here would reopen the cross-session hazard.
func TestSessionScopedTarget_NeverFallsBackToSharedDefault(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "")
	_, err := SessionScopedTarget("", "shared-default")
	if err == nil {
		t.Fatal("resolver fell back to the shared default; mutating commands must refuse instead")
	}
	var noTarget *ErrNoTarget
	if !errors.As(err, &noTarget) {
		t.Fatalf("refusal is not an ErrNoTarget: %v", err)
	}
	if !strings.Contains(err.Error(), SessionTargetEnvVar) {
		t.Errorf("refusal must name the env var remedy, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--server") {
		t.Errorf("refusal must name the flag remedy, got: %v", err)
	}
}

func TestReadOnlyTarget_KeepsConvenienceDefault(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "")
	cases := []struct{ flag, shared, want string }{
		{"flag", "shared", "flag"},
		{"", "shared", "shared"}, // convenience default kept for reads
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := ReadOnlyTarget(tc.flag, tc.shared); got != tc.want {
			t.Errorf("ReadOnlyTarget(%q,%q)=%q want %q", tc.flag, tc.shared, got, tc.want)
		}
	}
}

func TestReadOnlyTarget_EnvBeatsShared(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "env-server")
	if got := ReadOnlyTarget("", "shared"); got != "env-server" {
		t.Errorf("env must beat the shared default for reads too, got %q", got)
	}
}

// TestMutatingCommandsCannotReadActiveServer is the static keystone: it scans
// every mutating command file and fails if its RunE touches ActiveServer other
// than through SessionScopedTarget. This is what stops the next command from
// quietly reintroducing the shared fallback.
func TestMutatingCommandsCannotReadActiveServer(t *testing.T) {
	mutating := []string{
		"cp.go", "deploy.go", "destroy.go", "env.go", "exec.go", "heartbeat.go",
		"mount.go", "restart.go", "run.go", "spawn.go", "ssh.go", "start.go",
		"stop.go", "tunnel.go",
	}
	for _, f := range mutating {
		path := filepath.Join(".", f)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cannot read %s: %v (run from internal/cli)", f, err)
		}
		text := string(src)
		if !strings.Contains(text, "SessionScopedTarget(") {
			t.Errorf("%s is mutating but never calls SessionScopedTarget — it must go through the binding resolver", f)
		}
		// Any direct read of the shared default that is NOT inside a comment
		// and NOT the argument passed to the resolver is a violation.
		lineRe := regexp.MustCompile(`^\s*[^/].*cfg\.ActiveServer`)
		for i, line := range strings.Split(text, "\n") {
			if lineRe.MatchString(line) && !strings.Contains(line, "SessionScopedTarget(") {
				t.Errorf("%s:%d reads cfg.ActiveServer directly: %q — route it through SessionScopedTarget", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
