package agent

// Session-classifier regression tests for the destroy live-process gate.
//
// INT-CI-039: the destroy session-pair classifier must absorb the
// systemd >= 254 user-manager shape. Since v254 the user manager
// (user@<uid>.service) is started THROUGH systemd-executor, so the process
// that lives for the manager's whole lifetime runs
// "<path>/systemd-executor --deserialize <fd> ..." and the classic
// "systemd --user" argv never appears — bunker-mvp's CI battery refused
// every destroy because the executor was classified as a foreign process
// (CI run 36071328941, pid 119415 fingerprint below).
//
// INT-CI-041: the classifier must also absorb the agent's own transient
// "systemctl --user" CLIENT invocations (unset-environment during session
// teardown, show-environment / stop during spawn races) plus the
// cmdline-unreadable comm forms "(ystemctl)" / "[ystemctl]" (the kernel
// caps comm at 15 bytes, so "systemctl" loses its first char). CI runs
// 36133146931 and 36138476899 refused destroys on exactly those shapes,
// leaving the user and home behind ("user not removed after destroy").

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIsAgentSessionProcess_SystemdExecutor covers the v254+ executor shape
// and its negatives: only an executor basename whose first argument starts
// with --deserialize is the session manager; an executor with any other
// argument, and every operator process, must stay foreign.
func TestIsAgentSessionProcess_SystemdExecutor(t *testing.T) {
	tests := map[string]struct {
		cmd  string
		want bool
	}{
		"ci-fingerprint-bunker-mvp": {"/usr/lib/systemd/systemd-executor --deserialize 27 --log-level info --log-target auto", true},
		"debian-lib-path":           {"/lib/systemd/systemd-executor --deserialize 3", true},
		"bare-executor":             {"systemd-executor --deserialize 9", true},
		"executor-other-flag":       {"systemd-executor --other-flag", false},
		"executor-no-args":          {"systemd-executor", false},
		"foreign-sleep":             {"sleep 999", false},
		// INT-CI-041: ANY systemctl client of the agent's own user manager is
		// transient session bookkeeping (spawn-time bring-up, teardown-time
		// unset-environment, races in between), not an orphanable operator
		// process. This row previously asserted false for a pre-teardown
		// "status" probe — the CI refusal (runs 36133146931 /
		// 36138476899) proved every subcommand must absorb.
		"systemctl-user-status":      {"systemctl --user status", true},
		"systemctl-user-unset-env":   {"systemctl --user unset-environment SSH_AUTH_SOCK", true},
		"systemctl-user-show-env":    {"systemctl --user show-environment", true},
		"systemctl-user-stop":        {"systemctl --user stop bunker-agent.service", true},
		"systemctl-user-import-env":  {"systemctl --user import-environment DISPLAY", true},
		"systemctl-user-full-path":   {"/usr/bin/systemctl --user daemon-reload", true},
		"systemctl-user-flags-first": {"systemctl --user --no-pager status", true},
		"systemctl-user-truncated":   {"systemctl --user unset-environment SSH_AUTH_SOCK …", true},
		// The cmdline-unreadable comm forms observed live (CI run
		// 36138476899): comm is capped at 15 bytes, so "systemctl" is
		// rendered as "(ystemctl)" / "[ystemctl]" with the first char lost.
		"systemctl-comm-paren": {"(ystemctl)", true},
		"systemctl-comm-brack": {"[ystemctl]", true},
		"systemctl-comm-plain": {"ystemctl", true},
		"systemctl-comm-full":  {"systemctl", true},
		// Negative: a system-mode systemctl keeps its full readable cmdline
		// and stays foreign — only the user-manager client absorbs.
		"systemctl-system-start":  {"systemctl start nginx.service", false},
		"systemctl-system-status": {"systemctl status docker", false},
		"systemctl-flag-no-user":  {"systemctl --no-pager status nginx", false},
		"dockerd-user-flag":       {"dockerd --user nobody", false},
		"systemd-user-unit":       {"systemd --user --unit=foo", false},
		// Verbless but user-mode: still the user manager's own client.
		"systemctl-user-no-verb": {"systemctl --user", true},
		"dockerd-debug":          {"dockerd --debug", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := isAgentSessionProcess(userProcess{PID: 1, Cmd: tc.cmd}); got != tc.want {
				t.Errorf("isAgentSessionProcess(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestDestroy_SystemctlUserClientAbsorbed is the INT-CI-041 acceptance at the
// gate level, fingerprinting CI run 36133146931 (TestConcurrency_SpawnFiveAgents
// cleanup): the agent uid owns the session pair AND its own transient
// "systemctl --user unset-environment" client, and the destroy refused —
// leaving the user and home behind. The systemctl client is session
// bookkeeping of the manager destroy terminates, so the gate must absorb it
// and userdel must run.
func TestDestroy_SystemctlUserClientAbsorbed(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	const id = "intci041-ctl"
	const username = "bunker-" + id
	liveAgent(t, m, id)

	// The gate's evidence source, exactly as CI measured it: the session
	// pair plus the agent's own teardown-time systemctl --user client.
	fake := []userProcess{
		{PID: 867700, Cmd: "/usr/lib/systemd/systemd --user"},
		{PID: 867701, Cmd: "systemctl --user unset-environment SSH_AUTH_SOCK"},
		{PID: 867702, Cmd: "(sd-pam)"},
	}
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return fake, 61004, true, nil
	})
	presentUserWithUID(t, username, "61004")

	// The stubbed probe never reports the processes gone, so the terminate
	// step rides out its whole grace window — keep it short.
	oldGrace := terminateUserManagerGrace
	terminateUserManagerGrace = 50 * time.Millisecond
	t.Cleanup(func() { terminateUserManagerGrace = oldGrace })

	userLog := filepath.Join(t.TempDir(), "userdel.log")
	stubDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + userLog + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "userdel"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	resp, err := m.Destroy(context.Background(), id, false)
	if err != nil {
		t.Fatalf("destroy with only the session pair and the agent's own systemctl --user client alive must succeed: %v", err)
	}
	if resp.Status != "destroyed" {
		t.Fatalf("status = %q, want destroyed", resp.Status)
	}
	if calls, rerr := os.ReadFile(userLog); rerr != nil || len(calls) == 0 {
		t.Errorf("userdel never ran (calls: %q, err: %v)", calls, rerr)
	}
	for _, banned := range []string{"destroy refused", "pid 867701", "unset-environment"} {
		if strings.Contains(buf.String(), banned) {
			t.Errorf("log must not carry the absorbed systemctl client as refusal evidence (%q); log:\n%s", banned, buf.String())
		}
	}
	if rec := m.tracker.Get(id); rec != nil {
		t.Error("tracker record survived a successful destroy")
	}
}
