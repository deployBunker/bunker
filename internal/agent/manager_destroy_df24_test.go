package agent

// MISCHIEF-DF-24: the live-process refusal used to remediate with the
// unreachable advice "stop those processes on the host, then retry" — no
// command, no key, no host, on the very destroy path about to remove the
// operator's access (the catch-22). These tests pin the fix on BOTH refusal
// surfaces: the plain gate refusal must carry the exact kill command
// (`bunker ssh <user> -- pkill -U <uid>`) and name the `destroy --force`
// kill-first exit hatch; the force-survivor refusal must carry the exact
// kill command too. The orphan-safety guarantee is re-proven alongside:
// nothing is deleted when the refusal fires.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuildDestroyLiveProcessRefusal_CarriesExactKillCommand pins the
// MISCHIEF-DF-24 acceptance: the refusal names the exact, runnable kill
// command (bunker ssh <user> -- pkill -U <uid>) — reachable at refusal time
// because the agent user still exists — and names the --force exit hatch
// that IS kill-first (DF-BUNKER-63). The old dead-end remedy
// ("stop those processes on the host") must not come back.
func TestBuildDestroyLiveProcessRefusal_CarriesExactKillCommand(t *testing.T) {
	procs := []userProcess{{PID: 4242, Cmd: "node /home/bunker-x/app/server.js"}}
	msg := buildDestroyLiveProcessRefusal("bunker-x", 61012, procs)
	for _, want := range []string{
		"destroy refused",
		"bunker-x",
		"61012",
		"pid 4242",
		// The exact kill command, runnable as printed.
		"bunker ssh bunker-x -- pkill -U 61012",
		// Why it works: the access userdel is about to remove still exists.
		"still exists",
		// The kill-first exit hatch is named as an alternative.
		"--force",
		// Orphan-safety honesty: nothing has been deleted.
		"Nothing was deleted",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal missing %q — got: %s", want, msg)
		}
	}
	// The dead-end remedy must stay dead.
	if strings.Contains(msg, "stop those processes on the host, then retry") {
		t.Errorf("refusal still carries the unreachable pre-DF-24 remedy: %s", msg)
	}
}

// TestGateRefusal_NamesForceExitHatch drives the gate end-to-end (fake
// probe, hermetic manager) and asserts the REFUSAL ERROR the operator
// actually receives carries both remedies, while nothing is deleted.
func TestGateRefusal_NamesForceExitHatch(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	const id = "df24-gate"
	const username = "bunker-" + id
	liveAgent(t, m, id)

	fake := []userProcess{{PID: 4242, Cmd: "node /home/bunker-df24-gate/app/server.js"}}
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return fake, 61031, true, nil
	})
	presentUserWithUID(t, username, "61031")

	// userdel recorder: the refusal must NEVER reach it.
	userLog := filepath.Join(t.TempDir(), "userdel.log")
	stubDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + userLog + "\"\n"
	if err := os.WriteFile(filepath.Join(stubDir, "userdel"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	resp, err := m.Destroy(context.Background(), id, false)
	if err == nil {
		t.Fatal("destroy with a live uid process must FAIL, not proceed")
	}
	if resp == nil || resp.Status != StatusLiveProcesses {
		t.Fatalf("status = %v, want %q", resp, StatusLiveProcesses)
	}
	for _, want := range []string{
		"bunker ssh bunker-df24-gate -- pkill -U 61031",
		"--force",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing the exact remedy %q — got: %v", want, err)
		}
	}
	// Orphan-safety guarantee intact: userdel never ran.
	if calls, rerr := os.ReadFile(userLog); rerr == nil && len(calls) > 0 {
		t.Errorf("userdel ran despite live-process refusal: %q", calls)
	}
	if rec := m.tracker.Get(id); rec == nil {
		t.Error("tracker record lost on live-process refusal")
	}
}

// TestForceSurvivorRefusal_CarriesExactKillCommand pins the OTHER refusal
// surface: after --force's escalation, processes that SURVIVED it refuse the
// destroy with a remedy that is also a runnable command (pkill -9 -U <uid>),
// not the old bare "stop those processes on the host".
func TestForceSurvivorRefusal_CarriesExactKillCommand(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	const id = "df24-force"
	const username = "bunker-" + id
	liveAgent(t, m, id)

	// The process survives EVERY probe: the escalation ran but could not
	// clear the uid, so the post-escalation re-probe still shows it.
	fake := []userProcess{{PID: 4242, Cmd: "node /home/bunker-df24-force/app/server.js"}}
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return fake, 61032, true, nil
	})
	presentUserWithUID(t, username, "61032")

	// Keep the terminate step's grace wait short (same trick as DF-63).
	oldGrace := terminateUserManagerGrace
	terminateUserManagerGrace = 50 * time.Millisecond
	t.Cleanup(func() { terminateUserManagerGrace = oldGrace })
	m.forceKillUserProcessesFn = func(string, uint32, []userProcess) {}

	userLog := filepath.Join(t.TempDir(), "userdel.log")
	stubDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + userLog + "\"\n"
	if err := os.WriteFile(filepath.Join(stubDir, "userdel"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	resp, err := m.Destroy(context.Background(), id, true)
	if err == nil {
		t.Fatal("force destroy with SURVIVING processes must refuse, not proceed")
	}
	if resp == nil || resp.Status != StatusLiveProcesses {
		t.Fatalf("status = %v, want %q", resp, StatusLiveProcesses)
	}
	for _, want := range []string{
		"survived the destroy --force escalation",
		"bunker ssh bunker-df24-force -- pkill -9 -U 61032",
		"Nothing was deleted",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("force-survivor refusal missing %q — got: %v", want, err)
		}
	}
	// Orphan-safety guarantee intact on the force path too.
	if calls, rerr := os.ReadFile(userLog); rerr == nil && len(calls) > 0 {
		t.Errorf("userdel ran despite force-survivor refusal: %q", calls)
	}
	if rec := m.tracker.Get(id); rec == nil {
		t.Error("tracker record lost on force-survivor refusal")
	}
}
