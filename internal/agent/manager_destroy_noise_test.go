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
// bus" failure AND the "unit absent from the user manager" failure are
// non-actionable noise for a transient per-agent unit (the user is removed
// by userdel -rf in the very next step) and must be logged at Debug, while
// a genuine disable failure keeps its Warn record.
//
// The fixtures below are the VERBATIM systemctl outputs observed live.
// Attempt 1 (66d4150) wrote its needles against a paraphrased fixture and
// the real bunker-las-01 WARN survived the fix byte-identical — do not
// replace these strings with paraphrases.

// variantANoUserBus is the verbatim `systemctl --user disable` output from
// a root bunkerd on bunker-las-01 (no user session bus), straight from the
// daemon journal. Note it says "user scope bus" (NOT "failed to connect to
// bus") and carries a `$` before EACH variable name — both details broke
// every attempt-1 needle.
const variantANoUserBus = `Failed to connect to user scope bus via local transport: $DBUS_SESSION_BUS_ADDRESS and $XDG_RUNTIME_DIR not defined (consider using --machine=<user>@.host --user to connect to bus of other user)`

// variantBNoMedium is the verbatim wording seen on bunker-mvp in the same
// no-user-manager context.
const variantBNoMedium = `Failed to connect to bus: No medium found`

// variantCShortParaphrase is the short wording used by the dogfood report;
// the classifier must keep covering it too.
const variantCShortParaphrase = `Failed to connect to bus: DBUS_SESSION_BUS_ADDRESS and XDG_RUNTIME_DIR not defined`

// unitAbsenceOutput is the verbatim disable output from a destroy whose
// daemon DID have a user session bus (foreman-reproduced): the transient
// unit is simply not a unit file in the user manager, so there is nothing
// to disable — the same non-actionable outcome as the no-bus class.
const unitAbsenceOutput = `Failed to disable unit: Unit file bunker-docker-probe-fix.service does not exist.`

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
// verbatim real-world "no user manager" signature must classify TRUE, and
// genuine problems (or no output at all) must classify FALSE.
func TestNoUserManagerFailure(t *testing.T) {
	cases := []struct {
		name string
		out  []byte
		want bool
	}{
		{
			name: "Variant A verbatim: user scope bus + $DBUS_SESSION_BUS_ADDRESS and $XDG_RUNTIME_DIR (bunker-las-01)",
			out:  []byte(variantANoUserBus),
			want: true,
		},
		{
			name: "Variant B verbatim: No medium found (bunker-mvp)",
			out:  []byte(variantBNoMedium),
			want: true,
		},
		{
			name: "Variant C: short paraphrase (dogfood report)",
			out:  []byte(variantCShortParaphrase),
			want: true,
		},
		{
			name: "system not booted with systemd (container/chroot host)",
			out:  []byte("System has not been booted with systemd as init system (PID 1). Can't operate."),
			want: true,
		},
		{
			name: "bare $DBUS_SESSION_BUS_ADDRESS mention without the connect prefix",
			out:  []byte("warning: $DBUS_SESSION_BUS_ADDRESS is not set"),
			want: true,
		},
		{
			name: "permission denied is a genuine failure",
			out:  []byte("Failed to disable unit: Permission denied"),
			want: false,
		},
		{
			name: "access denied is a genuine failure",
			out:  []byte("Access denied"),
			want: false,
		},
		{
			name: "operation not permitted is a genuine failure",
			out:  []byte("Failed to disable unit: Operation not permitted"),
			want: false,
		},
		{
			name: "unrecognised output is a genuine failure",
			out:  []byte("systemctl: some totally unexpected diagnostic"),
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

// TestUnitAbsenceFailure is the table for the second non-actionable class:
// systemd reporting the transient unit as absent from the user manager.
func TestUnitAbsenceFailure(t *testing.T) {
	cases := []struct {
		name string
		out  []byte
		want bool
	}{
		{
			name: "verbatim unit file does not exist (foreman-reproduced)",
			out:  []byte(unitAbsenceOutput),
			want: true,
		},
		{
			name: "unit not loaded",
			out:  []byte("Failed to disable unit: Unit bunker-docker-abc.service not loaded."),
			want: true,
		},
		{
			name: "transient or generated unit",
			out:  []byte("Failed to disable unit: Unit file bunker-docker-abc.service is transient or generated."),
			want: true,
		},
		{
			name: "permission denied is NOT unit absence",
			out:  []byte("Failed to disable unit: Permission denied"),
			want: false,
		},
		{
			name: "no medium found is the no-bus class, not absence",
			out:  []byte(variantBNoMedium),
			want: false,
		},
		{
			name: "empty output",
			out:  nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unitAbsenceFailure(tc.out); got != tc.want {
				t.Errorf("unitAbsenceFailure(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// noUserManagerFailureAttempt1 is the attempt-1 matcher logic from 66d4150,
// kept verbatim ONLY as the RED-proof control for
// TestClassifierCatchesWhatAttempt1Missed. Never call it from production
// code paths.
func noUserManagerFailureAttempt1(out []byte) bool {
	o := strings.ToLower(string(out))
	for _, sig := range []string{
		"failed to connect to bus",
		"dbus_session_bus_address and xdg_runtime_dir not defined",
		"no medium found",
		"system has not been booted with systemd",
	} {
		if strings.Contains(o, sig) {
			return true
		}
	}
	return false
}

// TestClassifierCatchesWhatAttempt1Missed proves the new needles actually
// bite where attempt 1's did not: the attempt-1 logic must FAIL to classify
// the verbatim Variant A string (that is exactly why the WARN survived on
// bunker-las-01 with 66d4150 deployed), and the fixed normalisation must
// classify it.
func TestClassifierCatchesWhatAttempt1Missed(t *testing.T) {
	// RED premise: attempt-1 logic returns FALSE for Variant A.
	if noUserManagerFailureAttempt1([]byte(variantANoUserBus)) {
		t.Fatal("attempt-1 control unexpectedly matched Variant A; the RED premise is gone")
	}
	// The specific miss: lowercased Variant A still carries `$` before each
	// variable name, so attempt-1's combined needle
	// "dbus_session_bus_address and xdg_runtime_dir not defined" can never
	// match ("...address and $xdg_runtime_dir...").
	lowered := strings.ToLower(variantANoUserBus)
	if !strings.Contains(lowered, "address and $xdg_runtime_dir not defined") {
		t.Fatalf("premise: lowercased Variant A should contain the $-broken needle context; got %q", lowered)
	}

	// The fix: normalisation strips `$`, so the variable-name needles bite.
	norm := normalizeUnitOutput([]byte(variantANoUserBus))
	if strings.Contains(norm, "$") {
		t.Errorf("normalizeUnitOutput left $ characters behind: %q", norm)
	}
	for _, sig := range []string{
		"dbus_session_bus_address",
		"xdg_runtime_dir",
		"failed to connect to user scope bus",
	} {
		if !strings.Contains(norm, sig) {
			t.Errorf("normalised Variant A lost needle %q", sig)
		}
	}

	// And the real classifier classifies every verbatim variant.
	for name, v := range map[string]string{
		"A": variantANoUserBus,
		"B": variantBNoMedium,
		"C": variantCShortParaphrase,
	} {
		if !noUserManagerFailure([]byte(v)) {
			t.Errorf("Variant %s not classified as no-user-manager", name)
		}
	}
}

// TestDestroy_NoBusDisableIsNotWarned drives the real Destroy path to Step
// 2 with the verbatim Variant A systemctl output and proves (AC1) no Warn
// record with the message "systemctl disable failed" is emitted, (AC2) the
// disable attempt ran against the transient unit name, and the raw output
// is preserved on the Debug record for forensics.
func TestDestroy_NoBusDisableIsNotWarned(t *testing.T) {
	var buf bytes.Buffer
	m := newDestroyLoggerManager(t, &buf)

	var called []string
	stubDisableUserUnit(t, &called,
		[]byte(variantANoUserBus),
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
	// systemctl output preserved as an attribute.
	if !strings.Contains(logged, "transient unit disable skipped") {
		t.Errorf("no Debug record naming the suppression; log:\n%s", logged)
	}
	for _, marker := range []string{"DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "user scope bus"} {
		if !strings.Contains(logged, marker) {
			t.Errorf("Debug record lost raw output marker %q; log:\n%s", marker, logged)
		}
	}
}

// TestDestroy_UnitAbsenceDisableIsNotWarned pins the second non-actionable
// branch at the Destroy level: a disable that reports the transient unit as
// absent logs Debug with the raw output, never Warn.
func TestDestroy_UnitAbsenceDisableIsNotWarned(t *testing.T) {
	var buf bytes.Buffer
	m := newDestroyLoggerManager(t, &buf)

	stubDisableUserUnit(t, nil,
		[]byte(unitAbsenceOutput),
		fmt.Errorf("exit status 1"))

	resp, err := m.Destroy(context.Background(), "absence-check-agent", false)
	if err == nil {
		t.Fatal("destroy of a never-seen ID must fail")
	}
	if resp == nil || resp.Status != "not_found" {
		t.Fatalf("status = %v, want not_found", resp)
	}

	logged := buf.String()
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "systemctl disable failed") {
			t.Errorf("unit-absence disable produced a Warn record: %s", line)
		}
	}
	if !strings.Contains(logged, "unit absent from user manager; disable skipped") {
		t.Errorf("no Debug record naming the unit-absence suppression; log:\n%s", logged)
	}
	if !strings.Contains(logged, "does not exist") {
		t.Errorf("Debug record lost the raw systemctl output; log:\n%s", logged)
	}
}

// TestDestroy_GenuineDisableFailureStillWarns proves the honest-failure half
// of the contract: a disable failure that is NEITHER non-actionable class
// (here: Permission denied, verbatim class of genuine failure) keeps its
// Warn record.
func TestDestroy_GenuineDisableFailureStillWarns(t *testing.T) {
	var buf bytes.Buffer
	m := newDestroyLoggerManager(t, &buf)

	stubDisableUserUnit(t, nil,
		[]byte("Failed to disable unit: Permission denied"),
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
