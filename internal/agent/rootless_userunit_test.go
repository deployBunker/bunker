package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ── INT-CI-009: user-manager reachability proof + installer retry ───────────
//
// CI run 35142150138 failed rootless-install with
// "Failed to start docker.service: Unit docker.service not found": the
// installer writes ~/.config/systemd/user/docker.service and immediately runs
// `systemctl --user start docker.service`, and when the agent's user manager
// has not observed the freshly written unit (or the session bus was never
// usable) the start fails and the installer exits 1.
//
// These tests drive installRootlessDocker end to end through the seams:
//   - userManagerRunner: the root-side systemctl/loginctl bring-up
//     (bringUpUserManager, unchanged behavior from INT-CI-007/008);
//   - userSessionRunner: every command in the AGENT USER'S session (the
//     reachability probe and the daemon-reload before the retry);
//   - rootlessInstallerRunner: the installer execution itself;
//   - rootlessInstallerDownload: the curl (network must never be touched);
//   - userLookup / runtimeDirProbe / userRuntimeBaseDir / lingerDir: synthetic
//     uid and filesystem views.
//
// No root, no real systemd, no real su, no network.

const (
	uuTestUID      = 1002
	uuTestUsername = "bunker-uu-alpha"
)

// userUnitFakeHost fakes every seam installRootlessDocker touches and records
// the observed call ORDER across all of them, so the ordering assertions
// (probe before installer, reload between the two installer calls) are proven
// against one timeline.
type userUnitFakeHost struct {
	uid        int
	username   string
	runtimeDir string

	// sessionScriptErr models a failing `systemctl --user daemon-reload` in
	// the agent session (the CI "No medium found" state).
	sessionScriptErr bool
	// installerRuns scripts each installer call by index: true = success,
	// false = failure returning installerFailOutput. Indices past the end of
	// the slice default to success.
	installerRuns []bool
	// installerCalls counts the installer calls actually observed.
	installerCalls int
	// installerFailOutput is the CombinedOutput of a scripted failing call.
	installerFailOutput string

	// bringUpOk=false models the root-side user-manager bring-up failing.
	bringUpOk bool

	// ownership simulates the runtime-dir owner (userManagerOwnership from
	// rootless_runtimedir_test.go): the fake chown applies it, the probe
	// reads it.
	ownership *userManagerOwnership

	// waitBudget overrides the package readiness/bring-up budget when non-zero
	// (INT-SPAWN-003): a test whose session bus never answers must shrink it,
	// because the readiness gate polls until the budget is exhausted.
	waitBudget time.Duration

	// sessionScripts records the raw COMMAND STRING handed to the session
	// runner, and installerScripts the one handed to the installer runner.
	// They are recorded verbatim (in-band assignment and all) because that
	// string is what `su -` executes: the bus environment must be IN it
	// (INT-SPAWN-004), not merely in the su process environment.
	sessionScripts   []string
	installerScripts []string

	calls []string // ordered call log across all seams
}

func newUserUnitHost(t *testing.T) *userUnitFakeHost {
	t.Helper()
	return &userUnitFakeHost{
		uid:                 uuTestUID,
		username:            uuTestUsername,
		runtimeDir:          filepath.Join(t.TempDir(), "run", "user", strconv.Itoa(uuTestUID)),
		bringUpOk:           true,
		ownership:           &userManagerOwnership{},
		installerFailOutput: "systemctl --user start docker.service failed: Failed to start docker.service: Unit docker.service not found.",
	}
}

// userHome returns the fake agent home with bin/ pre-created (the production
// chown -R needs the dir to exist and tests run unprivileged).
func (h *userUnitFakeHost) userHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

// install swaps EVERY seam for this host and restores all of them on cleanup.
// Tests in this package run in one process, so a leaked fake would poison
// every later test — the restore rides t.Cleanup, matching
// installFakeCtl/installUserManagerHost.
func (h *userUnitFakeHost) install(t *testing.T, lingerDirPath string) {
	t.Helper()
	prevRunner := userManagerRunner
	prevSession := userSessionRunner
	prevInstaller := rootlessInstallerRunner
	prevDownload := rootlessInstallerDownload
	prevLookup := userLookup
	prevProbe := runtimeDirProbe
	prevLinger := lingerDir
	prevBase := userRuntimeBaseDir
	prevPoll := userManagerPollInterval
	prevRoot := rootHostRunner
	prevTimeout := userManagerWaitTimeoutOverride

	userManagerRunner = h.systemRunner
	userSessionRunner = h.sessionRunner
	rootlessInstallerRunner = h.installerRunner
	rootHostRunner = h.rootRunner
	rootlessInstallerDownload = func(_ context.Context, path string) ([]byte, error) {
		h.calls = append(h.calls, "download "+path)
		// Produce the file the installer would have downloaded: the
		// production flow chmods/chowns it afterwards.
		return nil, os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644)
	}
	userLookup = func(name string) (*user.User, error) {
		if name != h.username {
			return nil, fmt.Errorf("userUnitFakeHost: unexpected user %q", name)
		}
		return &user.User{
			Uid:      strconv.Itoa(h.uid),
			Gid:      strconv.Itoa(h.uid),
			Username: h.username,
			HomeDir:  "/home/" + h.username,
		}, nil
	}
	// The real probe reads the on-disk directory; the fake chown writes the
	// simulated owner, so the probe must read that simulation.
	runtimeDirProbe = h.ownership.probe
	lingerDir = lingerDirPath
	userRuntimeBaseDir = filepath.Dir(h.runtimeDir)
	userManagerPollInterval = time.Millisecond
	if h.waitBudget > 0 {
		userManagerWaitTimeoutOverride = h.waitBudget
	}

	t.Cleanup(func() {
		userManagerRunner = prevRunner
		userSessionRunner = prevSession
		rootlessInstallerRunner = prevInstaller
		rootlessInstallerDownload = prevDownload
		userLookup = prevLookup
		runtimeDirProbe = prevProbe
		lingerDir = prevLinger
		userRuntimeBaseDir = prevBase
		userManagerPollInterval = prevPoll
		rootHostRunner = prevRoot
		userManagerWaitTimeoutOverride = prevTimeout
	})
}

// rootRunner is the root-side filesystem-preparation runner: it services the
// install path's chown calls by applying the simulated ownership (same model
// as userManagerHost's chown case).
func (h *userUnitFakeHost) rootRunner(_ context.Context, name string, args ...string) ([]byte, error) {
	h.calls = append(h.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	switch name {
	case "chown":
		if h.ownership != nil {
			h.ownership.owner = uint32(h.uid)
		}
		return nil, nil
	}
	return nil, fmt.Errorf("userUnitFakeHost: unexpected root command %q", name)
}

// systemRunner is the ROOT-side runner: it services the bring-up
// (systemctl/loginctl/chown/journalctl, the same protocol userManagerHost
// speaks) and never records an installer or session call.
func (h *userUnitFakeHost) systemRunner(_ context.Context, name string, args ...string) ([]byte, error) {
	label := strings.TrimSpace(name + " " + strings.Join(args, " "))
	if !h.bringUpOk {
		h.calls = append(h.calls, label+" [bring-up-fails]")
		return nil, errors.New("exit status 1")
	}
	h.calls = append(h.calls, label)
	switch name {
	case "chown":
		// The bring-up's chown goes through userManagerRunner; apply the
		// simulated ownership so its verification step passes.
		if h.ownership != nil {
			h.ownership.owner = uint32(h.uid)
		}
		return nil, nil
	case "rm":
		if len(args) >= 2 {
			_ = os.RemoveAll(args[len(args)-1])
		}
		return nil, nil
	case "journalctl":
		return []byte("journal: user manager excerpt\n"), nil
	case "loginctl":
		if len(args) > 0 && (args[0] == "enable-linger" || args[0] == "terminate-user") {
			return nil, nil
		}
		return nil, errors.New("loginctl: unexpected verb")
	case "systemctl":
		if len(args) == 0 {
			return nil, errors.New("systemctl: missing verb")
		}
		unit := ""
		if len(args) > 1 {
			unit = args[len(args)-1]
		}
		switch args[0] {
		case "start":
			switch unit {
			case userRuntimeDirUnitName(h.uid):
				if err := os.MkdirAll(h.runtimeDir, 0o700); err != nil {
					return nil, err
				}
				return nil, nil
			case userManagerUnitName(h.uid):
				// A healthy manager publishes its bus socket.
				if err := os.WriteFile(filepath.Join(h.runtimeDir, "bus"), nil, 0o600); err != nil {
					return nil, err
				}
				return nil, nil
			}
			return nil, fmt.Errorf("systemctl start: unexpected unit %q", unit)
		case "stop", "reset-failed":
			return nil, nil
		case "is-active":
			if unit == userManagerUnitName(h.uid) {
				// After the fake manager start the socket exists and the
				// unit reports active; before that, inactive (exit 3).
				if _, err := os.Stat(filepath.Join(h.runtimeDir, "bus")); err == nil {
					return []byte("active\n"), nil
				}
				return []byte("inactive\n"), errors.New("exit status 3")
			}
			return []byte("inactive\n"), errors.New("exit status 3")
		case "show":
			return []byte("exit-code\n"), nil
		case "status":
			return nil, errors.New("exit status 3")
		}
		return nil, fmt.Errorf("systemctl: unexpected verb %q", args[0])
	}
	return nil, fmt.Errorf("unexpected command %q", name)
}

// sessionRunner is the AGENT-USER-session runner: the reachability probe and
// the daemon-reload before the installer retry both arrive here.
func (h *userUnitFakeHost) sessionRunner(_ context.Context, username, runtimeDir, script string) ([]byte, error) {
	if username != h.username {
		return nil, fmt.Errorf("userUnitFakeHost: session runner got user %q", username)
	}
	if runtimeDir != h.runtimeDir {
		return nil, fmt.Errorf("userUnitFakeHost: session runner got runtime dir %q, want %q", runtimeDir, h.runtimeDir)
	}
	// The COMMAND STRING is recorded verbatim: it carries the session bus
	// environment IN-BAND (INT-SPAWN-004), which is what the in-band tests
	// assert. The call label names the caller's own script, so the
	// order/count assertions stay about the command, not about its prefix.
	h.sessionScripts = append(h.sessionScripts, script)
	label := "user-session[" + sessionScriptTail(script) + "]"
	if h.sessionScriptErr {
		h.calls = append(h.calls, label+" [session-bus-down]")
		return []byte("Failed to connect to bus: No medium found"), errors.New("exit status 1")
	}
	h.calls = append(h.calls, label)
	return []byte(""), nil
}

// installerRunner models the official installer: it "writes"
// ~/.config/systemd/user/docker.service on every call and is scripted by
// installerRuns (true = success, false = fail with installerFailOutput).
//
// script is the COMPLETE installer session command (the in-band bus
// environment plus the installer's toggles, then the installer path), so the
// path is recovered from its tail — which is also what proves the caller's
// path is passed through byte-identically.
func (h *userUnitFakeHost) installerRunner(_ context.Context, username, runtimeDir, script string) ([]byte, error) {
	if username != h.username || runtimeDir != h.runtimeDir {
		return nil, fmt.Errorf("userUnitFakeHost: installer runner got user=%q dir=%q", username, runtimeDir)
	}
	h.installerScripts = append(h.installerScripts, script)
	installerPath := sessionScriptTail(script)
	idx := h.installerCalls
	h.installerCalls++
	h.calls = append(h.calls, fmt.Sprintf("installer#%d[%s]", idx, installerPath))
	ok := true
	if idx < len(h.installerRuns) {
		ok = h.installerRuns[idx]
	}
	if !ok {
		return []byte(h.installerFailOutput), errors.New("exit status 1")
	}
	// Simulate the real installer's effect: it installs into ~/bin
	// (dockerd-rootless.sh et al.), which the install flow stats afterwards.
	bin := filepath.Join(filepath.Dir(installerPath), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(bin, "dockerd-rootless.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		return nil, err
	}
	return nil, nil
}

func (h *userUnitFakeHost) callLog() string { return "\n  " + strings.Join(h.calls, "\n  ") }

// indexOf returns the position of the first call whose log entry starts with
// prefix, or -1.
func (h *userUnitFakeHost) indexOf(prefix string) int {
	return h.nextIndexOf(prefix, 0)
}

// nextIndexOf returns the position of the first call at or after `from` whose
// log entry starts with prefix, or -1.
func (h *userUnitFakeHost) nextIndexOf(prefix string, from int) int {
	for i := from; i < len(h.calls); i++ {
		if strings.HasPrefix(h.calls[i], prefix) {
			return i
		}
	}
	return -1
}

// countPrefix counts calls whose log entry starts with prefix.
func (h *userUnitFakeHost) countPrefix(prefix string) int {
	n := 0
	for _, c := range h.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// sessionReloadLabel is the exact user-session daemon-reload call label.
func sessionReloadLabel() string { return "user-session[" + userManagerReloadCmd + "]" }

// uuLogger keeps warn/info output out of test logs; error-text assertions are
// done on the returned errors instead (same approach as ctlLogger).
func uuLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestInstallRootlessDocker_HealthyPathOrder covers AC1: the reachability
// probe runs BEFORE the installer, exactly one probe and one installer call
// occur, and no daemon-reload is issued after the installer (healthy path =
// no retry).
func TestInstallRootlessDocker_HealthyPathOrder(t *testing.T) {
	h := newUserUnitHost(t)
	home := h.userHome(t)
	h.install(t, t.TempDir())

	if err := installRootlessDocker(context.Background(), h.username, home, uuLogger()); err != nil {
		t.Fatalf("installRootlessDocker() error = %v%s", err, h.callLog())
	}

	probeIdx := h.indexOf(sessionReloadLabel())
	installerIdx := h.indexOf("installer#")
	if probeIdx < 0 {
		t.Fatalf("user-session daemon-reload probe never ran:%s", h.callLog())
	}
	if installerIdx < 0 {
		t.Fatalf("installer never ran:%s", h.callLog())
	}
	if probeIdx > installerIdx {
		t.Errorf("reachability probe (call %d) must precede the installer (call %d):%s",
			probeIdx, installerIdx, h.callLog())
	}
	if got := h.countPrefix(sessionReloadLabel()); got != 1 {
		t.Errorf("expected exactly 1 user-session daemon-reload probe, got %d:%s", got, h.callLog())
	}
	if got := h.countPrefix("installer#"); got != 1 {
		t.Errorf("expected exactly 1 installer call, got %d:%s", got, h.callLog())
	}
	// Healthy path: no user-session reload AFTER the installer call.
	for i, c := range h.calls {
		if i > installerIdx && strings.HasPrefix(c, sessionReloadLabel()) {
			t.Errorf("healthy path must not reload after the installer (call %d):%s", i, h.callLog())
		}
	}
	if got := h.countPrefix("download "); got != 1 {
		t.Errorf("expected exactly 1 installer download, got %d:%s", got, h.callLog())
	}
}

// TestInstallRootlessDocker_ProbeFailureBlocksInstaller covers AC2: when the
// user-session daemon-reload fails, installRootlessDocker returns an error
// naming the user manager unit, the observed is-active value and the linger
// count — and the installer runner is NEVER called.
func TestInstallRootlessDocker_ProbeFailureBlocksInstaller(t *testing.T) {
	h := newUserUnitHost(t)
	h.sessionScriptErr = true
	// The session bus never answers, so the readiness gate (INT-SPAWN-003)
	// rides out its own budget before the spawn fails: shrink it (the
	// assertion below needs enough budget to observe the RETRY).
	h.waitBudget = 100 * time.Millisecond
	home := h.userHome(t)
	h.install(t, lingerDirWithEntries(t, 3))

	err := installRootlessDocker(context.Background(), h.username, home, uuLogger())
	if err == nil {
		t.Fatalf("expected an error when the user-session probe fails:%s", h.callLog())
	}
	msg := err.Error()
	for _, want := range []string{
		"rootless-install",
		"user@1002.service",
		"is-active=",
		"linger entries: 3",
		// The readiness gate's own stderr text: the attribution must carry it.
		"Failed to connect to bus: No medium found",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("probe-failure error missing %q, got: %s", want, msg)
		}
	}
	// A not-yet-answering bus is retried on the gate's budget, never judged on
	// a single probe (the tick-453 defect): the pre-fix code made exactly 2
	// session calls here (the single probe plus the recovery's second one).
	if got := h.countPrefix(sessionReloadLabel()); got <= 3 {
		t.Errorf("expected the readiness gate to retry on its own budget, got %d probe(s):%s", got, h.callLog())
	}
	if got := h.countPrefix("installer#"); got != 0 {
		t.Errorf("the installer must NEVER run when the probe fails, got %d installer calls:%s",
			got, h.callLog())
	}
}

// TestInstallRootlessDocker_RetryAfterDaemonReload covers AC3: an installer
// failure carrying the unit-not-found signature is retried ONCE after a
// user-session daemon-reload; a successful second call yields nil and the
// call order is probe → installer#1 → user-session reload → installer#2.
func TestInstallRootlessDocker_RetryAfterDaemonReload(t *testing.T) {
	h := newUserUnitHost(t)
	// Script: installer call #0 fails with the signature, call #1 succeeds.
	h.installerRuns = []bool{false, true}
	home := h.userHome(t)
	h.install(t, t.TempDir())

	if err := installRootlessDocker(context.Background(), h.username, home, uuLogger()); err != nil {
		t.Fatalf("expected the daemon-reload retry to succeed, got: %v%s", err, h.callLog())
	}

	probeIdx := h.indexOf(sessionReloadLabel())
	firstInstaller := h.indexOf("installer#0")
	reloadBeforeRetry := h.nextIndexOf(sessionReloadLabel(), firstInstaller+1)
	secondInstaller := h.indexOf("installer#1")
	if firstInstaller < 0 || secondInstaller < 0 {
		t.Fatalf("expected exactly 2 installer calls:%s", h.callLog())
	}
	if reloadBeforeRetry < 0 {
		t.Fatalf("no user-session daemon-reload between the two installer calls:%s", h.callLog())
	}
	if !(probeIdx < firstInstaller && firstInstaller < reloadBeforeRetry && reloadBeforeRetry < secondInstaller) {
		t.Errorf("call order must be probe → installer#1 → daemon-reload → installer#2 (probe %d, installer#1 %d, reload %d, installer#2 %d):%s",
			probeIdx, firstInstaller, reloadBeforeRetry, secondInstaller, h.callLog())
	}
	// The probe plus the pre-retry reload make exactly two user-session
	// reloads.
	if got := h.countPrefix(sessionReloadLabel()); got != 2 {
		t.Errorf("expected exactly 2 user-session reloads (probe + pre-retry), got %d:%s", got, h.callLog())
	}
	if got := h.countPrefix("installer#"); got != 2 {
		t.Errorf("expected exactly 2 installer calls, got %d:%s", got, h.callLog())
	}
}

// TestInstallRootlessDocker_RetryExhausted covers AC4: when BOTH installer
// calls fail with the signature, the returned error carries the signature
// output (condensed tail) AND the user-manager state fragments.
func TestInstallRootlessDocker_RetryExhausted(t *testing.T) {
	h := newUserUnitHost(t)
	h.installerRuns = []bool{false, false}
	home := h.userHome(t)
	h.install(t, lingerDirWithEntries(t, 2))

	err := installRootlessDocker(context.Background(), h.username, home, uuLogger())
	if err == nil {
		t.Fatalf("expected an error when the retry also fails:%s", h.callLog())
	}
	msg := err.Error()
	for _, want := range []string{
		"docker.service",
		"user@1002.service",
		"is-active=",
		"linger entries: 2",
		"retry",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("retry-exhausted error missing %q, got: %s", want, msg)
		}
	}
	if got := h.countPrefix("installer#"); got != 2 {
		t.Errorf("expected exactly 2 installer calls (never a third), got %d:%s", got, h.callLog())
	}
}

// TestInstallRootlessDocker_UnrelatedFailureDoesNotRetry covers AC5: an
// installer failure WITHOUT the not-found signature must produce a single
// installer call and no extra user-session reload.
func TestInstallRootlessDocker_UnrelatedFailureDoesNotRetry(t *testing.T) {
	h := newUserUnitHost(t)
	h.installerRuns = []bool{false}
	h.installerFailOutput = "curl: (35) OpenSSL SSL_connect: connection reset by peer"
	home := h.userHome(t)
	h.install(t, t.TempDir())

	err := installRootlessDocker(context.Background(), h.username, home, uuLogger())
	if err == nil {
		t.Fatal("expected the unrelated installer failure to surface")
	}
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Errorf("unrelated failure must pass through untouched, got: %s", err.Error())
	}
	if got := h.countPrefix("installer#"); got != 1 {
		t.Errorf("expected exactly 1 installer call (no retry), got %d:%s", got, h.callLog())
	}
	if got := h.countPrefix(sessionReloadLabel()); got != 1 {
		t.Errorf("expected exactly 1 user-session reload (the probe only), got %d:%s", got, h.callLog())
	}
}

// TestIsUserUnitNotFound_Strictness pins the signature matcher: the real
// "Unit docker.service not found" text matches in any case mix, while
// unrelated failures (network, other units, empty output) do not.
func TestIsUserUnitNotFound_Strictness(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "verbatim CI signature",
			out:  "systemctl --user start docker.service failed: Failed to start docker.service: Unit docker.service not found.",
			want: true,
		},
		{
			name: "case insensitive",
			out:  "UNIT DOCKER.SERVICE NOT FOUND",
			want: true,
		},
		{
			name: "unit name without not-found",
			out:  "docker.service: start request repeated too quickly",
			want: false,
		},
		{
			name: "not found for a DIFFERENT unit",
			out:  "Failed to start containerd.service: Unit containerd.service not found.",
			want: false,
		},
		{
			name: "network failure",
			out:  "curl: (35) OpenSSL SSL_connect: connection reset by peer",
			want: false,
		},
		{
			name: "empty output",
			out:  "",
			want: false,
		},
		{
			name: "bus failure without unit name",
			out:  "Failed to connect to bus: No medium found",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUserUnitNotFound([]byte(tc.out)); got != tc.want {
				t.Errorf("isUserUnitNotFound(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// TestProveUserManagerReachable_ProbeSuccessIsSilent checks the probe in
// isolation: a successful user-session daemon-reload returns nil with exactly
// one session call and no attribution noise.
func TestProveUserManagerReachable_ProbeSuccessIsSilent(t *testing.T) {
	h := newUserUnitHost(t)
	h.install(t, t.TempDir())

	if err := proveUserManagerReachable(context.Background(), h.username, h.uid, h.runtimeDir, uuLogger()); err != nil {
		t.Fatalf("probe must succeed through the healthy session, got: %v", err)
	}
	if got := h.countPrefix(sessionReloadLabel()); got != 1 {
		t.Errorf("expected exactly 1 probe call, got %d", got)
	}
}

// TestInstallRootlessDocker_BringUpStillFirst guards the unchanged contract
// from INT-CI-008: the root-side user-manager bring-up runs first and its
// failure still blocks everything (neither the probe nor the installer may be
// reached).
func TestInstallRootlessDocker_BringUpStillFirst(t *testing.T) {
	h := newUserUnitHost(t)
	h.bringUpOk = false
	home := h.userHome(t)
	h.install(t, t.TempDir())

	err := installRootlessDocker(context.Background(), h.username, home, uuLogger())
	if err == nil {
		t.Fatal("expected the bring-up failure to surface")
	}
	if h.countPrefix(sessionReloadLabel()) != 0 {
		t.Errorf("a broken bring-up must fail before the reachability probe:%s", h.callLog())
	}
	if h.countPrefix("installer#") != 0 {
		t.Errorf("a broken bring-up must fail before the installer:%s", h.callLog())
	}
}
