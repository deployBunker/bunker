package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── INT-CI-069: docker.service unit-START failure — uninstall-and-retry ─────
//
// CI run 38542837944 (regression job, cell "spawn auto-generates ID"): the
// official installer's own `systemctl --user start docker.service` failed with
// "Job for docker.service failed because the control process exited with error
// code." — a unit-START failure, distinct from the INT-CI-009 unit-not-found
// race. The spawn died with NO journal/state attribution. These tests drive
// the REAL installRootlessDocker over the userUnitFakeHost seams and assert:
//
//   - the CI output triggers exactly ONE bounded retry, after the partial
//     setup was un-installed through the agent session (the setup tool's own
//     prescribed recovery), never more;
//   - a second failure is FATAL with the docker.service journal excerpt in
//     the error (named fail_detail — never a silent skip);
//   - a failed unit whose reset-failed does not converge is a named
//     fail_detail BEFORE the retry (retrying through a `failed` unit would
//     deterministically fail the same way);
//   - the pre-existing unit-not-found race STILL goes down the daemon-reload
//     path (no regression on INT-CI-009), and unrelated installer failures
//     still fail immediately with no retry.

// intCI069CIOutput is the installer CombinedOutput tail CI run 38542837944
// recorded (condensed to the load-bearing lines).
const intCI069CIOutput = `[INFO] Creating /home/bunker-9196cadd/.config/systemd/user/docker.service
[INFO] starting systemd service docker.service
+ systemctl --user start docker.service
Job for docker.service failed because the control process exited with error code.
See "systemctl --user status docker.service" and "journalctl --user -xeu docker.service" for details.
[ERROR] Failed to start docker.service. Run ` + "'journalctl -n 20 --no-pager --user --unit docker.service'" + ` to show the error log.
[ERROR] Before retrying installation, you might need to uninstall the current setup: dockerd-rootless-setuptool.sh uninstall -f
`

// TestINTCI069_DockerUnitStartFailureRetry drives the real
// installRootlessDocker over the userUnitFakeHost seams in the three shapes
// that matter, plus the two guard arms.
func TestINTCI069_DockerUnitStartFailureRetry(t *testing.T) {
	cases := []struct {
		name string

		// installerRuns scripts each installer call (true = success).
		installerRuns []bool
		// installerFailOutput is what a scripted failing call prints.
		installerFailOutput string
		// probeState, when non-empty, is what dockerUnitStateProbe answers.
		probeState string
		// sessionScriptsToFail are script tails whose session call must
		// fail (simulating e.g. a non-converging reset).
		sessionScriptsToFail []string

		wantErrSubstr []string // error must contain ALL of these
		wantNoErr     bool
		wantInstaller int    // total installer calls observed
		wantSession   string // a session script tail that MUST have run
		wantNoSession string // a session script tail that must NOT run
	}{
		{
			name:                "unit-start failure is uninstalled and retried once to success",
			installerRuns:       []bool{false, true},
			installerFailOutput: intCI069CIOutput,
			wantNoErr:           true,
			wantInstaller:       2,
			wantSession:         "dockerd-rootless-setuptool.sh uninstall -f",
		},
		{
			name:                "unit-start failure retried once still failing is fatal with journal",
			installerRuns:       []bool{false, false},
			installerFailOutput: intCI069CIOutput,
			wantErrSubstr: []string{
				"retry after docker.service unit-start failure also failed",
				"docker.service journal:",
			},
			wantInstaller: 2,
			wantSession:   "dockerd-rootless-setuptool.sh uninstall -f",
		},
		{
			name:                "failed unit whose reset does not converge is a named fail_detail before any retry",
			installerRuns:       []bool{false},
			installerFailOutput: intCI069CIOutput,
			probeState:          dockerUnitFailed,
			sessionScriptsToFail: []string{
				dockerUnitResetCmdFmt,
			},
			wantErrSubstr: []string{
				"did not converge",
				dockerServiceUnitName,
			},
			wantInstaller: 1,
			wantNoSession: "dockerd-rootless-setuptool.sh uninstall -f",
		},
		{
			name:                "unit-not-found output still takes the INT-CI-009 daemon-reload path",
			installerRuns:       []bool{false, true},
			installerFailOutput: "systemctl --user start docker.service failed: Failed to start docker.service: Unit docker.service not found.",
			wantNoErr:           true,
			wantInstaller:       2,
			wantSession:         userManagerReloadCmd,
			wantNoSession:       "dockerd-rootless-setuptool.sh uninstall -f",
		},
		{
			name:                "unrelated installer failure is immediate — no uninstall, no retry",
			installerRuns:       []bool{false},
			installerFailOutput: "curl: (56) Recv failure: Connection reset by peer",
			wantErrSubstr:       []string{"run rootless installer as"},
			wantInstaller:       1,
			wantNoSession:       "dockerd-rootless-setuptool.sh uninstall -f",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newUserUnitHost(t)
			h.installerRuns = tc.installerRuns
			h.installerFailOutput = tc.installerFailOutput
			home := h.userHome(t)
			h.install(t, t.TempDir())

			// dockerUnitStateProbe rides the session seam in production
			// (userSessionRunner); point it at the fake session runner so
			// the probe observes this host's session state.
			prevProbe := dockerUnitStateProbe
			if tc.probeState != "" {
				// The table demands a specific unit state: answer it
				// directly (with the state's own exit status, as real
				// `systemctl --user is-active` reports non-active states).
				dockerUnitStateProbe = func(context.Context, string, string) (string, error) {
					return tc.probeState, errors.New("exit status 1")
				}
			} else {
				// Otherwise the probe rides the session seam in production
				// (userSessionRunner); point it at the fake session runner
				// so the probe observes this host's session state.
				dockerUnitStateProbe = func(ctx context.Context, username, runtimeDir string) (string, error) {
					out, err := userSessionRunner(ctx, username, runtimeDir,
						sessionCommand(runtimeDir, dockerUnitProbeCmd))
					return strings.TrimSpace(string(out)), err
				}
			}
			t.Cleanup(func() { dockerUnitStateProbe = prevProbe })

			// Session scripts that must fail: wrap the host's session
			// runner with a per-script fault injector.
			prevSession := userSessionRunner
			userSessionRunner = func(ctx context.Context, username, runtimeDir, script string) ([]byte, error) {
				for _, tail := range tc.sessionScriptsToFail {
					if strings.Contains(script, tail) {
						h.calls = append(h.calls, "user-session["+tail+"][injected-fail]")
						return []byte("injected failure"), errors.New("exit status 1")
					}
				}
				return prevSession(ctx, username, runtimeDir, script)
			}
			t.Cleanup(func() { userSessionRunner = prevSession })

			err := installRootlessDocker(context.Background(), h.username, home, uuLogger())

			if tc.wantNoErr && err != nil {
				t.Fatalf("installRootlessDocker() error = %v%s", err, h.callLog())
			}
			for _, sub := range tc.wantErrSubstr {
				if err == nil || !strings.Contains(err.Error(), sub) {
					t.Errorf("installRootlessDocker() error = %v, want it to contain %q%s", err, sub, h.callLog())
				}
			}
			if tc.wantNoErr || err != nil {
				if got := h.installerCalls; got != tc.wantInstaller {
					t.Errorf("installer calls = %d, want %d%s", got, tc.wantInstaller, h.callLog())
				}
			}
			if tc.wantSession != "" && h.countSessionScript(tc.wantSession) == 0 {
				t.Errorf("session script containing %q never ran%s", tc.wantSession, h.callLog())
			}
			if tc.wantNoSession != "" && h.countSessionScript(tc.wantNoSession) > 0 {
				t.Errorf("session script containing %q ran but must not%s", tc.wantNoSession, h.callLog())
			}
		})
	}
}

// countSessionScript counts recorded session scripts whose tail contains sub.
func (h *userUnitFakeHost) countSessionScript(sub string) int {
	n := 0
	for _, s := range h.sessionScripts {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// TestINTCI069_ClassificationTable pins the classifier's boundaries: only the
// CI job-failure wording triggers the uninstall-and-retry — never a
// unit-not-found body, never a network failure, never an empty output.
func TestINTCI069_ClassificationTable(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "the CI run's job-failure tail",
			out:  intCI069CIOutput,
			want: true,
		},
		{
			name: "job-failure wording without the installer's start line",
			out:  "Job for docker.service failed because the control process exited with error code.",
			want: false,
		},
		{
			name: "unit-not-found race body",
			out:  "Failed to start docker.service: Unit docker.service not found.",
			want: false,
		},
		{name: "network failure", out: "curl: (56) Recv failure: Connection reset by peer", want: false},
		{name: "empty output", out: "", want: false},
		{
			name: "case-insensitive match",
			out:  "[INFO] starting systemd service docker.service\n+ SYSTEMCTL --USER START DOCKER.SERVICE\nJOB FOR DOCKER.SERVICE FAILED BECAUSE THE CONTROL PROCESS EXITED WITH ERROR CODE.",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDockerUnitStartFailure([]byte(tc.out)); got != tc.want {
				t.Errorf("isDockerUnitStartFailure(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// TestINTCI069_ResetSkipOnUnprovableState: when the probe cannot prove the
// unit is `failed` (probe answers empty / errors), the reset is SKIPPED (never
// run blind) and the uninstall still happens, so the retry proceeds.
func TestINTCI069_ResetSkipOnUnprovableState(t *testing.T) {
	h := newUserUnitHost(t)
	h.installerRuns = []bool{false, true}
	h.installerFailOutput = intCI069CIOutput
	home := h.userHome(t)
	h.install(t, t.TempDir())

	// Probe answers the unit-not-found text (the installer's own probe of a
	// vanished unit) — not `failed`, so no reset.
	prevProbe := dockerUnitStateProbe
	dockerUnitStateProbe = func(context.Context, string, string) (string, error) {
		return "unknown", errors.New("exit status 4")
	}
	t.Cleanup(func() { dockerUnitStateProbe = prevProbe })

	if err := installRootlessDocker(context.Background(), h.username, home, uuLogger()); err != nil {
		t.Fatalf("installRootlessDocker() error = %v%s", err, h.callLog())
	}
	if h.countSessionScript(dockerUnitResetCmdFmt) != 0 {
		t.Errorf("reset-failed ran against an unprovable unit state%s", h.callLog())
	}
	if h.countSessionScript("dockerd-rootless-setuptool.sh uninstall -f") == 0 {
		t.Errorf("uninstall never ran%s", h.callLog())
	}
	if h.installerCalls != 2 {
		t.Errorf("installer calls = %d, want 2 (one failure + one retry)%s", h.installerCalls, h.callLog())
	}
}

// TestINTCI069_ResetPathRunsWhenFailed: when the probe DOES report `failed`,
// the reset runs through the session seam (with the in-band bus prefix) before
// the uninstall, and the retry succeeds.
func TestINTCI069_ResetPathRunsWhenFailed(t *testing.T) {
	h := newUserUnitHost(t)
	h.installerRuns = []bool{false, true}
	h.installerFailOutput = intCI069CIOutput
	home := h.userHome(t)
	h.install(t, t.TempDir())

	prevProbe := dockerUnitStateProbe
	dockerUnitStateProbe = func(context.Context, string, string) (string, error) {
		return dockerUnitFailed, errors.New("exit status 1") // the state's own exit status
	}
	t.Cleanup(func() { dockerUnitStateProbe = prevProbe })

	if err := installRootlessDocker(context.Background(), h.username, home, uuLogger()); err != nil {
		t.Fatalf("installRootlessDocker() error = %v%s", err, h.callLog())
	}
	if h.countSessionScript(dockerUnitResetCmdFmt) != 1 {
		t.Errorf("reset-failed ran %d times, want 1%s", h.countSessionScript(dockerUnitResetCmdFmt), h.callLog())
	}
	if h.countSessionScript("dockerd-rootless-setuptool.sh uninstall -f") != 1 {
		t.Errorf("uninstall ran %d times, want 1%s", h.countSessionScript("dockerd-rootless-setuptool.sh uninstall -f"), h.callLog())
	}
	// The rm of the prescribed data dir rides the installer path's parent.
	if h.countSessionScript(rootlessDataDirRel) == 0 {
		t.Errorf("best-effort data-dir rm never ran%s", h.callLog())
	}
	// In-band bus prefix on every recovery script (INT-SPAWN-004 contract).
	for _, s := range h.sessionScripts {
		if strings.Contains(s, "uninstall") || strings.Contains(s, "reset-failed") {
			if !strings.HasPrefix(s, "export ") || !strings.Contains(s, "DBUS_SESSION_BUS_ADDRESS=") {
				t.Errorf("recovery session script lost the in-band bus environment: %q", s)
			}
		}
	}
}

// silence unused-helper vet for environments without these test helpers
var (
	_ = fmt.Sprintf
	_ = filepath.Join
	_ = io.Discard
	_ = os.Getenv
	_ = slog.Default
)
