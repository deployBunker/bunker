package agent

// INT-CI-039 regression tests: the destroy session-pair classifier must
// absorb the systemd >= 254 user-manager shape. Since v254 the user manager
// (user@<uid>.service) is started THROUGH systemd-executor, so the process
// that lives for the manager's whole lifetime runs
// "<path>/systemd-executor --deserialize <fd> ..." and the classic
// "systemd --user" argv never appears — bunker-mvp's CI battery refused
// every destroy because the executor was classified as a foreign process
// (CI run 36071328941, pid 119415 fingerprint below).

import "testing"

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
		"systemctl-user-status":     {"systemctl --user status", false},
		"dockerd-debug":             {"dockerd --debug", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := isAgentSessionProcess(userProcess{PID: 1, Cmd: tc.cmd}); got != tc.want {
				t.Errorf("isAgentSessionProcess(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}
