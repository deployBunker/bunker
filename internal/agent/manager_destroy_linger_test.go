package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// This file pins the INT-HOST-001 linger contract: spawn enables systemd
// linger for every agent user, so Destroy must disable it again — best
// effort, before userdel -rf (the name must still resolve), on every path
// that reaches userdel, and never at the cost of the destroy itself.

// presentUserStub returns a lookupUser stub that resolves exactly the given
// username (so the destroy path believes the user exists) and fails for
// every other name with user.UnknownUserError — the same error class
// user.Lookup returns for a missing /etc/passwd entry.
func presentUserStub(username string) func(string) (*user.User, error) {
	return func(name string) (*user.User, error) {
		if name == username {
			return &user.User{
				Username: name,
				Uid:      "61001",
				Gid:      "61001",
				Name:     "bunker test agent",
				HomeDir:  "/home/" + name,
			}, nil
		}
		return nil, user.UnknownUserError(name)
	}
}

// lingerCall records one disableLinger invocation.
type lingerCall struct {
	username string
}

// newLingerTestLogger returns a logger recording every record (Debug and up)
// into buf, so log-level assertions are possible.
func newLingerTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// stubDisableLinger swaps the disableLinger seam for fn and records every
// call. Restored via t.Cleanup (tests in this package share one process; a
// leaked fake would poison every later destroy test).
func stubDisableLinger(t *testing.T, fn func(ctx context.Context, username string) ([]byte, error)) *[]lingerCall {
	t.Helper()
	calls := &[]lingerCall{}
	orig := disableLinger
	disableLinger = func(ctx context.Context, username string) ([]byte, error) {
		*calls = append(*calls, lingerCall{username: username})
		return fn(ctx, username)
	}
	t.Cleanup(func() { disableLinger = orig })
	return calls
}

// stubLookupUser swaps the lookupUser seam for fn; restored via t.Cleanup.
func stubLookupUser(t *testing.T, fn func(string) (*user.User, error)) {
	t.Helper()
	orig := lookupUser
	lookupUser = fn
	t.Cleanup(func() { lookupUser = orig })
}

// TestDisableAgentLinger_Unit is the direct table for the helper: attempt
// only when the user resolves, never fail the caller, log the failure class.
func TestDisableAgentLinger_Unit(t *testing.T) {
	t.Run("empty_username_never_attempts", func(t *testing.T) {
		var buf bytes.Buffer
		logger := newLingerTestLogger(&buf)
		calls := stubDisableLinger(t, func(context.Context, string) ([]byte, error) {
			t.Error("disableLinger must not run for an empty username")
			return nil, nil
		})
		if disableAgentLinger(context.Background(), "", logger) {
			t.Error("empty username must report not-attempted")
		}
		if len(*calls) != 0 {
			t.Errorf("calls = %v, want none", *calls)
		}
	})

	t.Run("absent_user_skips_without_calling_loginctl", func(t *testing.T) {
		var buf bytes.Buffer
		logger := newLingerTestLogger(&buf)
		stubLookupUser(t, func(string) (*user.User, error) {
			return nil, user.UnknownUserError("bunker-gone")
		})
		calls := stubDisableLinger(t, func(context.Context, string) ([]byte, error) {
			t.Error("disableLinger must not run for an absent user")
			return nil, nil
		})
		if disableAgentLinger(context.Background(), "bunker-gone", logger) {
			t.Error("absent user must report not-attempted")
		}
		if len(*calls) != 0 {
			t.Errorf("calls = %v, want none", *calls)
		}
		if !strings.Contains(buf.String(), "skipping linger disable") {
			t.Errorf("absent-user skip must be on the record; log:\n%s", buf.String())
		}
	})

	t.Run("failure_is_reported_attempted_and_never_returned", func(t *testing.T) {
		var buf bytes.Buffer
		logger := newLingerTestLogger(&buf)
		stubLookupUser(t, presentUserStub("bunker-alive"))
		calls := stubDisableLinger(t, func(context.Context, string) ([]byte, error) {
			return []byte("Failed to disable linger: some systemd diagnostic"), fmt.Errorf("exit status 1")
		})
		if !disableAgentLinger(context.Background(), "bunker-alive", logger) {
			t.Error("a failing loginctl call WAS attempted and must report attempted")
		}
		if len(*calls) != 1 || (*calls)[0].username != "bunker-alive" {
			t.Errorf("calls = %v, want exactly one for bunker-alive", *calls)
		}
		logged := buf.String()
		if !strings.Contains(logged, "loginctl disable-linger failed") {
			t.Errorf("failure must be logged with the disable-linger marker; log:\n%s", logged)
		}
		if !strings.Contains(logged, "level=WARN") {
			t.Errorf("failure must be logged at WARN; log:\n%s", logged)
		}
		if !strings.Contains(logged, "some systemd diagnostic") {
			t.Errorf("raw loginctl output must be preserved on the record; log:\n%s", logged)
		}
	})

	t.Run("success_reports_attempted", func(t *testing.T) {
		var buf bytes.Buffer
		logger := newLingerTestLogger(&buf)
		stubLookupUser(t, presentUserStub("bunker-alive"))
		calls := stubDisableLinger(t, func(context.Context, string) ([]byte, error) {
			return nil, nil
		})
		if !disableAgentLinger(context.Background(), "bunker-alive", logger) {
			t.Error("success must report attempted")
		}
		if len(*calls) != 1 || (*calls)[0].username != "bunker-alive" {
			t.Errorf("calls = %v, want exactly one for bunker-alive", *calls)
		}
	})
}

// TestDestroy_DisablesLingerBeforeUserdel is AC1 at the Destroy level: the
// disable must be attempted for the agent's username, and it must run BEFORE
// userdel -rf (which needs the name to still resolve). Both commands are
// captured by PATH stubs that append to one log file, so the order is real
// execution order, not an inference from call counts.
func TestDestroy_DisablesLingerBeforeUserdel(t *testing.T) {
	var buf bytes.Buffer
	m := newDestroyLoggerManager(t, &buf)

	// The stubs record `$1 $2 ...` one invocation per line. loginctl is
	// invoked as `loginctl disable-linger <user>`, userdel as
	// `userdel -rf <user>` — so the log reads the true order.
	stubDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "cmds.log")
	for _, cmd := range []string{"loginctl", "userdel"} {
		script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + logPath + "\"\n"
		if err := os.WriteFile(filepath.Join(stubDir, cmd), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Real disableLinger (no seam swap): the PATH stub records the call.
	// The user exists (stub), so the disable is attempted.
	stubLookupUser(t, presentUserStub("bunker-order-check-agent"))

	resp, err := m.Destroy(context.Background(), "order-check-agent", false)
	// The stubbed userdel exits 0, so the destroy runs its normal success
	// tail (user_present + userdel success => destroyed). The never-seen
	// not_found branch is covered by the sibling tests; what matters here is
	// that the linger disable happened on the path TO userdel.
	if err != nil {
		t.Fatalf("destroy (with stub userdel) must succeed: %v", err)
	}
	if resp == nil || resp.Status != "destroyed" {
		t.Fatalf("status = %v, want destroyed", resp)
	}

	data, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("command stub log missing (a stubbed command never ran): %v", rerr)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	idxDisable, idxUserdel := -1, -1
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "disable-linger bunker-order-check-agent"):
			idxDisable = i
		case strings.HasPrefix(line, "-rf bunker-order-check-agent"):
			idxUserdel = i
		}
	}
	if idxDisable < 0 {
		t.Errorf("destroy never ran `loginctl disable-linger bunker-order-check-agent`; log: %q", lines)
	}
	if idxUserdel < 0 {
		t.Errorf("destroy never reached userdel; log: %q", lines)
	}
	if idxDisable >= 0 && idxUserdel >= 0 && idxDisable > idxUserdel {
		t.Errorf("disable-linger ran AFTER userdel (log line %d vs %d); the name must still resolve", idxDisable, idxUserdel)
	}
}

// TestDestroy_FailingLingerDisableDoesNotFailDestroy is AC2: a failing
// `loginctl disable-linger` is logged at WARN with the raw output and the
// destroy outcome is exactly what it would have been without the feature.
func TestDestroy_FailingLingerDisableDoesNotFailDestroy(t *testing.T) {
	var buf bytes.Buffer
	m := newDestroyLoggerManager(t, &buf)

	calls := stubDisableLinger(t, func(context.Context, string) ([]byte, error) {
		return []byte("Failed to disable linger: Permission denied"), fmt.Errorf("exit status 1")
	})
	stubLookupUser(t, presentUserStub("bunker-fail-linger-agent"))

	resp, err := m.Destroy(context.Background(), "fail-linger-agent", false)
	if err == nil {
		t.Fatal("destroy of a never-seen ID must still fail (not_found)")
	}
	if resp == nil || resp.Status != "not_found" {
		t.Fatalf("status = %v, want not_found (the disable failure must not change the outcome)", resp)
	}
	if len(*calls) != 1 || (*calls)[0].username != "bunker-fail-linger-agent" {
		t.Errorf("calls = %v, want exactly one for bunker-fail-linger-agent", *calls)
	}
	logged := buf.String()
	if !strings.Contains(logged, "loginctl disable-linger failed") {
		t.Errorf("failing disable must be on the record; log:\n%s", logged)
	}
	if !strings.Contains(logged, "Permission denied") {
		t.Errorf("raw loginctl output must be preserved; log:\n%s", logged)
	}
}

// TestDestroy_AbsentUserSkipsLingerDisable pins the hot-path skip: when the
// user was absent before userdel there is nothing to disable — no loginctl
// call, a Debug note, and the standard not_found outcome unchanged.
func TestDestroy_AbsentUserSkipsLingerDisable(t *testing.T) {
	var buf bytes.Buffer
	m := newDestroyLoggerManager(t, &buf)

	calls := stubDisableLinger(t, func(context.Context, string) ([]byte, error) {
		t.Error("disableLinger must not run when the user never existed")
		return nil, nil
	})
	stubLookupUser(t, func(name string) (*user.User, error) {
		return nil, user.UnknownUserError(name)
	})

	resp, err := m.Destroy(context.Background(), "absent-linger-agent", false)
	if err == nil {
		t.Fatal("destroy of a never-seen ID must still fail (not_found)")
	}
	if resp == nil || resp.Status != "not_found" {
		t.Fatalf("status = %v, want not_found", resp)
	}
	if len(*calls) != 0 {
		t.Errorf("disableLinger calls = %v, want none", *calls)
	}
	if !strings.Contains(buf.String(), "skipping linger disable") {
		t.Errorf("absent-user skip must be on the record; log:\n%s", buf.String())
	}
}

// TestDestroy_ForceModeStillDisablesLinger pins the second early-exit branch
// the brief names: force mode swallows a userdel failure and reports
// destroyed — the linger disable must have been attempted on that path too.
func TestDestroy_ForceModeStillDisablesLinger(t *testing.T) {
	var buf bytes.Buffer
	m := newDestroyLoggerManager(t, &buf)

	calls := stubDisableLinger(t, func(context.Context, string) ([]byte, error) {
		return nil, nil
	})
	stubLookupUser(t, presentUserStub("bunker-force-linger-agent"))

	resp, err := m.Destroy(context.Background(), "force-linger-agent", true)
	if err != nil {
		t.Fatalf("force destroy must succeed: %v", err)
	}
	if resp == nil || resp.Status != "destroyed" {
		t.Fatalf("status = %v, want destroyed", resp)
	}
	if len(*calls) != 1 || (*calls)[0].username != "bunker-force-linger-agent" {
		t.Errorf("calls = %v, want exactly one for bunker-force-linger-agent", *calls)
	}
}
