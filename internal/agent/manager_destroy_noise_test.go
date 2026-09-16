package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// This file pins the DF-BUNKER-5 logging contract for Destroy Step 2
// (`systemctl --user disable`): the well-known "caller has no user session
// bus" failure is non-actionable noise for a transient per-agent unit (the
// user is removed by userdel -rf in the very next step) and must be logged
// at Debug, while a genuine disable failure keeps its Warn record.

// newDestroyLoggerManager returns a manager whose logger records every
// record (Debug and up) into buf and whose system probe is stubbed to "no
// agents". Destroy is driven against an agent ID that never exists on the
// host: every host command in the destroy path fails fast (no docker
// socket, no user, no processes), nothing is touched, and the call ends in
// the standard not_found branch — the same safe pattern gap070_test.go
// relies on.
func newDestroyLoggerManager(t *testing.T, buf *bytes.Buffer) *AgentManager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := config.DefaultConfig()
	cfg.Agent.Registry.Enabled = false
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	m := NewAgentManager(cfg, logger, tracker, nil, nil)
	m.listSystemAgents = func() ([]SystemAgent, error) { return nil, nil }
	t.Cleanup(func() { m.Stop() })
	return m
}

// stubDisableUserUnit swaps the disableUserUnit seam for one test and
// restores the original via t.Cleanup (tests in this package run in one
// process, so a leaked fake would poison every later Destroy test).
func stubDisableUserUnit(t *testing.T, calls *[]string, out []byte, err error) {
	t.Helper()
	orig := disableUserUnit
	disableUserUnit = func(ctx context.Context, unit string) ([]byte, error) {
		if calls != nil {
			*calls = append(*calls, unit)
		}
		return out, err
	}
	t.Cleanup(func() { disableUserUnit = orig })
}

// TestNoUserManagerFailure is the table for the classifier itself: every
// real-world "no user manager" signature must classify TRUE, and genuine
// problems (or no output at all) must classify FALSE.
func TestNoUserManagerFailure(t *testing.T) {
	cases := []struct {
		name string
		out  []byte
		want bool
	}{
		{
			name: "dbus env vars undefined (the real-world destroy noise)",
			out:  []byte("Failed to connect to bus: DBUS_SESSION_BUS_ADDRESS and XDG_RUNTIME_DIR not defined\nTrying to contact the bus..."),
			want: true,
		},
		{
			name: "failed to connect to bus",
			out:  []byte("systemctl: error while connecting to bus: Failed to connect to bus"),
			want: true,
		},
		{
			name: "no medium found variant",
			out:  []byte("Failed to disable unit: Process org.freedesktop.systemd1 exited with status 1\nFailed to disable unit: No medium found"),
			want: true,
		},
		{
			name: "system not booted with systemd (container/chroot host)",
			out:  []byte("System has not been booted with systemd as init system (PID 1). Can't operate."),
			want: true,
		},
		{
			name: "permission denied is a genuine failure",
			out:  []byte("Failed to disable unit: Permission denied"),
			want: false,
		},
		{
			name: "unit file does not exist is a genuine failure",
			out:  []byte("Failed to disable unit: Unit file bunker-docker-abc.service does not exist."),
			want: false,
		},
		{
			name: "access denied is a genuine failure",
			out:  []byte("Access denied"),
			want: false,
		},
		{
			name: "nil output",
			out:  nil,
			want: false,
		},
		{
			name: "empty output",
			out:  []byte(""),
			want: false,
		},
		{
			name: "whitespace-only output",
			out:  []byte(" \n\t "),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := noUserManagerFailure(tc.out); got != tc.want {
				t.Errorf("noUserManagerFailure(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// noBusOutput is the exact real-world signature this work suppresses, as
// `systemctl --user disable` emits it from a root bunkerd session with no
// user bus:
//
//	Failed to connect to bus: DBUS_SESSION_BUS_ADDRESS and XDG_RUNTIME_DIR not defined
const noBusOutput = "Failed to connect to bus: DBUS_SESSION_BUS_ADDRESS and XDG_RUNTIME_DIR not defined"

// TestDestroy_NoBusDisableIsNotWarned drives the real Destroy path to Step 2
// with a no-bus disable failure and proves (AC1) no Warn record with the
// message "systemctl disable failed" is emitted, (AC2) the disable attempt
// ran against the transient unit name, and the raw output is preserved on
// the Debug record for forensics.
func TestDestroy_NoBusDisableIsNotWarned(t *testing.T) {
	var buf bytes.Buffer
	m := newDestroyLoggerManager(t, &buf)

	var called []string
	stubDisableUserUnit(t, &called,
		[]byte(noBusOutput),
		fmt.Errorf("exit status 1"))

	// Non-force destroy of a never-seen ID: host commands all fail fast and
	// the call ends in the documented not_found branch — nothing on the
	// host is touched (same contract gap070_test.go relies on).
	resp, err := m.Destroy(context.Background(), "noise-check-agent", false)
	if err == nil {
		t.Fatal("destroy of a never-seen ID must fail")
	}
	if resp == nil || resp.Status != "not_found" {
		t.Fatalf("status = %v, want not_found", resp)
	}

	// AC2: the disable attempt ran, against the agent's transient unit.
	if len(called) != 1 || called[0] != "bunker-docker-noise-check-agent" {
		t.Fatalf("disableUserUnit calls = %v, want exactly one call for bunker-docker-noise-check-agent", called)
	}

	// AC1: the known no-bus failure must NOT produce the Warn record.
	logged := buf.String()
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "systemctl disable failed") {
			t.Errorf("no-bus disable produced a Warn record: %s", line)
		}
	}

	// The suppression itself must be on the record at Debug, with the raw
	// systemctl output preserved as an attribute (XDG_RUNTIME_DIR survives
	// the text handler verbatim inside the quoted value).
	if !strings.Contains(logged, "transient unit disable skipped") {
		t.Errorf("no Debug record naming the suppression; log:\n%s", logged)
	}
	if !strings.Contains(logged, "XDG_RUNTIME_DIR") {
		t.Errorf("Debug record lost the raw systemctl output; log:\n%s", logged)
	}
}

// TestDestroy_GenuineDisableFailureStillWarns proves the honest-failure half
// of the contract: a disable failure that is NOT the no-bus class keeps its
// Warn record verbatim.
func TestDestroy_GenuineDisableFailureStillWarns(t *testing.T) {
	var buf bytes.Buffer
	m := newDestroyLoggerManager(t, &buf)

	stubDisableUserUnit(t, nil,
		[]byte("Failed to disable unit: Unit file bunker-docker-genuine-agent.service does not exist."),
		fmt.Errorf("exit status 1"))

	resp, err := m.Destroy(context.Background(), "genuine-agent", false)
	if err == nil {
		t.Fatal("destroy of a never-seen ID must fail")
	}
	if resp == nil || resp.Status != "not_found" {
		t.Fatalf("status = %v, want not_found", resp)
	}

	found := false
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "systemctl disable failed") && strings.Contains(line, "level=WARN") {
			found = true
		}
	}
	if !found {
		t.Errorf("genuine disable failure produced no Warn record; log:\n%s", buf.String())
	}
}
