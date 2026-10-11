package agent

// DF-BUNKER-66 regression tests. The destroy live-process gate used to count
// the rootless-docker login session's OWN default.target services (dbus-daemon,
// pipewire, wireplumber, mpris-proxy) as orphanable operator processes, and its
// remedy ("stop those processes on the host") had no agent-side path: bunker
// exec cannot stop them (session close reaps children) and a detached stop is
// respawned by the user manager (dogfood run 20, docs/dogfood/2026-09-25c).
//
// The fix is two halves, and these tests prove both:
//  1. ABSORB — the gate classifies the session's own services as lifecycle
//     infrastructure (like DF-BUNKER-56's pair) and never counts them;
//  2. REMEDY — the destroy path stops the services at the source (after the
//     user-manager terminate, which removes the respawner) before the gate.
//
// Everything runs on SEAMS: fixture /proc trees, stubbed runners and lookup,
// no root, no real userdel, no user manager.

import (
	"bytes"
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubStopRootlessSessionRunner swaps the stop sweep's runner for a recorder
// and restores it via t.Cleanup. Every invocation is appended to the returned
// slice as "name args...".
func stubStopRootlessSessionRunner(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	orig := stopRootlessSessionRunner
	stopRootlessSessionRunner = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	t.Cleanup(func() { stopRootlessSessionRunner = orig })
	return &calls
}

// shortStopRootlessSessionTimeout keeps the sweep's bounded wait fast.
func shortStopRootlessSessionTimeout(t *testing.T) {
	t.Helper()
	orig := stopRootlessSessionTimeout
	stopRootlessSessionTimeout = 150 * time.Millisecond
	t.Cleanup(func() { stopRootlessSessionTimeout = orig })
}

// ── The classifier ──────────────────────────────────────────────────────────

// TestIsRootlessSessionProcess is the DF-BUNKER-66 classifier table: the
// rootless login session's own default.target services (in their cmdline and
// bracketed-comm shapes) classify as session infrastructure — which SUBSUMES
// the DF-BUNKER-56 pair — while operator processes and name lookalikes never
// do.
func TestIsRootlessSessionProcess(t *testing.T) {
	tests := map[string]struct {
		cmd  string
		want bool
	}{
		// The dogfood-measured services, bare and path-prefixed.
		"dbus-daemon-bare":        {"dbus-daemon --session", true},
		"dbus-daemon-path":        {"/usr/bin/dbus-daemon --session --address=systemd: --nofork", true},
		"dbus-broker":             {"/usr/bin/dbus-broker-launch", true},
		"pipewire":                {"/usr/bin/pipewire", true},
		"pipewire-pulse":          {"/usr/bin/pipewire-pulse", true},
		"wireplumber":             {"/usr/bin/wireplumber", true},
		"mpris-proxy":             {"mpris-proxy", true},
		"xdg-desktop-portal":      {"/usr/libexec/xdg-desktop-portal", true},
		"xdg-permission-store":    {"/usr/libexec/xdg-permission-store", true},
		"dbus-comm-bracketed":     {"[dbus-daemon]", true},
		"dbus-comm-paren":         {"(dbus-daemon)", true},
		"pipewire-comm-bracketed": {"[pipewire]", true},
		// DF-BUNKER-56 pair subsumption: the pair is session infrastructure.
		"session-pair-systemd": {"/usr/lib/systemd/systemd --user", true},
		"session-pair-sd-pam":  {"(sd-pam)", true},
		// Operator processes never absorb.
		"node-server":     {"node /home/bunker-x/app/server.js", false},
		"dockerd":         {"/usr/bin/dockerd", false},
		"rootlesskit":     {"rootlesskit --net=slirp4netns", false},
		"schedulerd":      {"/home/bunker-x/bin/schedulerd --workdir /home/bunker-x", false},
		"unknown-comm":    {"(unknown)", false},
		"empty":           {"", false},
		"systemctl-plain": {"systemctl start nginx.service", false},
		// Name LOOKALIKES never absorb: only the exact basename matches.
		"lookalike-arg":      {"node /home/bunker-x/bin/dbus-daemon-emulator", false},
		"lookalike-suffix":   {"pipewire-recorder --monitor", false},
		"lookalike-path-dir": {"/home/bunker-x/tools/dbus-daemon/helper.sh", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := isRootlessSessionProcess(userProcess{PID: 1, Cmd: tc.cmd}); got != tc.want {
				t.Errorf("isRootlessSessionProcess(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestIsRootlessSessionProcessOnly pins the SPLIT the stop sweep relies on:
// the pair is the terminate step's business (false here), the services are
// the sweep's (true).
func TestIsRootlessSessionProcessOnly(t *testing.T) {
	if isRootlessSessionProcessOnly(userProcess{PID: 1, Cmd: "/usr/lib/systemd/systemd --user"}) {
		t.Error("the session pair must NOT be a stop-sweep target (terminate step owns it)")
	}
	if isRootlessSessionProcessOnly(userProcess{PID: 1, Cmd: "(sd-pam)"}) {
		t.Error("sd-pam must NOT be a stop-sweep target")
	}
	if !isRootlessSessionProcessOnly(userProcess{PID: 1, Cmd: "/usr/bin/pipewire"}) {
		t.Error("pipewire must be a stop-sweep target")
	}
	if isRootlessSessionProcessOnly(userProcess{PID: 1, Cmd: "node /home/bunker-x/app.js"}) {
		t.Error("an operator process must never be a stop-sweep target")
	}
}

// ── Gate absorption (the ABSORB half) ───────────────────────────────────────

// TestDestroy_RootlessSessionServicesAbsorbed is the row's headline
// acceptance: the dogfood-run-20 shape — the session pair plus the rootless
// session's dbus/pipewire/wireplumber/mpris-proxy and NOTHING else — must
// NO LONGER refuse the destroy. The destroy completes (status destroyed,
// userdel ran once) in force and non-force mode alike.
func TestDestroy_RootlessSessionServicesAbsorbed(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "non_force"
		if force {
			name = "force"
		}
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			m := newGateManager(t, &buf)
			const id = "dfb66-absorb"
			const username = "bunker-" + id
			liveAgent(t, m, id)

			// The dogfood-run-20 gate evidence: the pair + the session's
			// own default.target services, no operator process.
			fake := []userProcess{
				{PID: 6001, Cmd: "/usr/lib/systemd/systemd --user"},
				{PID: 6002, Cmd: "(sd-pam)"},
				{PID: 6003, Cmd: "/usr/bin/dbus-daemon --session --address=systemd: --nofork"},
				{PID: 6004, Cmd: "/usr/bin/pipewire"},
				{PID: 6005, Cmd: "/usr/bin/wireplumber"},
				{PID: 6006, Cmd: "mpris-proxy"},
			}
			stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
				return fake, 61008, true, nil
			})
			presentUserWithUID(t, username, "61008")

			// Keep the terminate step's grace wait short: the stubbed probe
			// never reports the pair gone, so the step rides out the whole
			// deadline otherwise.
			oldGrace := terminateUserManagerGrace
			terminateUserManagerGrace = 50 * time.Millisecond
			t.Cleanup(func() { terminateUserManagerGrace = oldGrace })
			shortStopRootlessSessionTimeout(t)
			// The sweep's own scan reads the real procStatusPath; point it
			// at an EMPTY fixture so the sweep finds nothing (this arm is
			// about the GATE's absorption, not the sweep).
			procDirFixture(t, nil)

			if force {
				m.forceKillUserProcessesFn = func(string, uint32, []userProcess) {
					t.Error("force escalation must never run for session infrastructure")
				}
			}

			userLog := filepath.Join(t.TempDir(), "userdel.log")
			stubDir := t.TempDir()
			script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + userLog + "\"\nexit 0\n"
			if err := os.WriteFile(filepath.Join(stubDir, "userdel"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			resp, err := m.Destroy(context.Background(), id, force)
			if err != nil {
				t.Fatalf("destroy with only rootless session services alive must succeed: %v", err)
			}
			if resp.Status != "destroyed" {
				t.Fatalf("status = %q, want destroyed", resp.Status)
			}
			if calls, rerr := os.ReadFile(userLog); rerr != nil || len(calls) == 0 {
				t.Errorf("userdel never ran on the session-services-only path (calls: %q, err: %v)", calls, rerr)
			}
			if rec := m.tracker.Get(id); rec != nil {
				t.Error("tracker record survived a successful destroy")
			}
		})
	}
}

// TestDestroy_OperatorProcessStillRefusedWithRootlessSession pins the
// absorption's boundary: pair + services + ONE operator process still
// refuses with live_processes, and the refusal names ONLY the operator
// process — the session infrastructure must not appear in the evidence.
func TestDestroy_OperatorProcessStillRefusedWithRootlessSession(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	const id = "dfb66-mixed"
	const username = "bunker-" + id
	liveAgent(t, m, id)

	fake := []userProcess{
		{PID: 6100, Cmd: "/usr/lib/systemd/systemd --user"},
		{PID: 6101, Cmd: "(sd-pam)"},
		{PID: 6102, Cmd: "/usr/bin/dbus-daemon --session"},
		{PID: 6103, Cmd: "/usr/bin/pipewire"},
		{PID: 6104, Cmd: "node /home/bunker-dfb66-mixed/app/server.js"},
	}
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return fake, 61009, true, nil
	})
	presentUserWithUID(t, username, "61009")

	oldGrace := terminateUserManagerGrace
	terminateUserManagerGrace = 50 * time.Millisecond
	t.Cleanup(func() { terminateUserManagerGrace = oldGrace })
	shortStopRootlessSessionTimeout(t)
	procDirFixture(t, nil)

	userLog := filepath.Join(t.TempDir(), "userdel.log")
	stubDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + userLog + "\"\n"
	if err := os.WriteFile(filepath.Join(stubDir, "userdel"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	resp, err := m.Destroy(context.Background(), id, false)
	if err == nil {
		t.Fatal("destroy with an operator process alive must FAIL even when the rootless session services are present")
	}
	if resp == nil || resp.Status != StatusLiveProcesses {
		t.Fatalf("status = %v, want %q", resp, StatusLiveProcesses)
	}
	for _, want := range []string{"destroy refused", "pid 6104", "node /home/bunker-dfb66-mixed/app/server.js"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal error missing %q — got: %v", want, err)
		}
	}
	// The evidence names ONLY the operator process (the pids of the
	// absorbed pair and services must not appear). The refusal's remedy
	// clause does mention dbus/pipewire as the class the destroy path
	// already stops automatically — that is DF-66's message, not evidence.
	for _, banned := range []string{"pid 6100", "pid 6101", "pid 6102", "pid 6103"} {
		if strings.Contains(err.Error(), banned) {
			t.Errorf("refusal error must not name absorbed session infrastructure (%q) — got: %v", banned, err)
		}
	}
	// The updated remedy is on the record: the destroy path already stops
	// the session's own services automatically.
	for _, want := range []string{"already stops the rootless-docker session's own services"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal error missing the DF-66 remedy (%q) — got: %v", want, err)
		}
	}
	// Nothing was deleted.
	if calls, rerr := os.ReadFile(userLog); rerr == nil && len(calls) > 0 {
		t.Errorf("userdel ran despite the operator-process refusal: %q", calls)
	}
	if rec := m.tracker.Get(id); rec == nil {
		t.Error("tracker record lost on live-process refusal")
	}
}

// ── The stop sweep (the REMEDY half) ────────────────────────────────────────

// rootlessSweepFixture is the /proc fixture the sweep tests read: the pair,
// the session services and one foreign operator process, all under uid.
func rootlessSweepFixture() []procFixtureEntry {
	return []procFixtureEntry{
		{PID: "7001", UID: 61010, Name: "systemd", Cmd: "/usr/lib/systemd/systemd --user"},
		{PID: "7002", UID: 61010, Name: "sd-pam", Cmd: "(sd-pam)"},
		{PID: "7003", UID: 61010, Name: "dbus-daemon", Cmd: "/usr/bin/dbus-daemon --session"},
		{PID: "7004", UID: 61010, Name: "pipewire", Cmd: "/usr/bin/pipewire"},
		{PID: "7005", UID: 61010, Name: "wireplumber", Cmd: "/usr/bin/wireplumber"},
		{PID: "7006", UID: 61010, Name: "node", Cmd: "node /home/bunker-x/app/server.js"},
	}
}

// TestStopRootlessSessionServices_StopsAndKillsTargets proves the sweep's
// shape: it asks the user manager to stop the session target once, waits the
// bounded grace, then SIGKILLs exactly the surviving session-service pids —
// NEVER the pair (the terminate step's business) and NEVER the foreign
// operator process.
func TestStopRootlessSessionServices_StopsAndKillsTargets(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	procDirFixture(t, rootlessSweepFixture())
	presentUserWithUID(t, "bunker-sweep-kill", "61010")
	calls := stubStopRootlessSessionRunner(t)
	shortStopRootlessSessionTimeout(t)

	m.stopRootlessSessionServices(context.Background(), "bunker-sweep-kill", m.logger)

	// The manager stop was attempted exactly once, against the session target.
	var stops []string
	var kills []string
	for _, c := range *calls {
		switch {
		case strings.HasPrefix(c, "systemctl"):
			stops = append(stops, c)
		case strings.HasPrefix(c, "kill"):
			kills = append(kills, c)
		default:
			t.Errorf("unexpected sweep command: %q", c)
		}
	}
	if len(stops) != 1 || stops[0] != "systemctl --user stop default.target" {
		t.Errorf("sweep stop calls = %v, want exactly [systemctl --user stop default.target]", stops)
	}
	// The SIGKILL pass names exactly the surviving service pids.
	if len(kills) == 0 {
		t.Fatal("the sweep never killed the surviving session services")
	}
	for _, want := range []string{"7003", "7004", "7005"} {
		found := false
		for _, k := range kills {
			if strings.Contains(k, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("kill list missing pid %s (kills: %v)", want, kills)
		}
	}
	for _, banned := range []string{"7001", "7002", "7006"} {
		for _, k := range kills {
			if strings.Contains(k, banned) {
				t.Errorf("kill list must not touch pair/foreign pid %s (kills: %v)", banned, kills)
			}
		}
	}
	// The sweep is on the record.
	if !strings.Contains(buf.String(), "stopping the rootless session's own services") {
		t.Errorf("log missing the sweep record; log:\n%s", buf.String())
	}
}

// TestStopRootlessSessionServices_CleanStopAvoidsKills proves the fast path:
// when the manager stop makes the services exit mid-sweep (the stub removes
// the fixture entries), the sweep returns WITHOUT any SIGKILL.
func TestStopRootlessSessionServices_CleanStopAvoidsKills(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	fixture := rootlessSweepFixture()
	procDirFixture(t, fixture)
	presentUserWithUID(t, "bunker-sweep-clean", "61010")
	calls := stubStopRootlessSessionRunner(t)
	shortStopRootlessSessionTimeout(t)

	// The manager stop removes the service entries from the fixture (the
	// manager was the only thing keeping them alive). The pair and the
	// foreign process stay — neither is the sweep's business.
	inner := stopRootlessSessionRunner
	stopRootlessSessionRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, name+" "+strings.Join(args, " "))
		if name == "systemctl" {
			root := procStatusPath
			for _, p := range fixture {
				if p.Name == "dbus-daemon" || p.Name == "pipewire" || p.Name == "wireplumber" {
					_ = os.RemoveAll(filepath.Join(root, p.PID))
				}
			}
		}
		return inner(ctx, name, args...)
	}

	m.stopRootlessSessionServices(context.Background(), "bunker-sweep-clean", m.logger)

	for _, c := range *calls {
		if strings.HasPrefix(c, "kill") {
			t.Errorf("a clean manager stop must avoid the SIGKILL pass; got %q", c)
		}
	}
	if len(*calls) == 0 || !strings.HasPrefix((*calls)[0], "systemctl") {
		t.Errorf("the stop was never attempted: %v", *calls)
	}
}

// TestStopRootlessSessionServices_NoopWithoutTargets proves the sweep is a
// no-op (no systemctl, no kill) when the uid owns no session services.
func TestStopRootlessSessionServices_NoopWithoutTargets(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	procDirFixture(t, nil) // nothing live under the uid
	presentUserWithUID(t, "bunker-sweep-noop", "61011")
	calls := stubStopRootlessSessionRunner(t)

	m.stopRootlessSessionServices(context.Background(), "bunker-sweep-noop", m.logger)

	if len(*calls) != 0 {
		t.Errorf("sweep ran commands despite no session services: %v", *calls)
	}
}

// TestStopRootlessSessionServices_SkipsUnresolvableUser proves the sweep is
// skipped (never executed) when the user record no longer resolves.
func TestStopRootlessSessionServices_SkipsUnresolvableUser(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	calls := stubStopRootlessSessionRunner(t)
	stubLookupUser(t, func(name string) (*user.User, error) {
		return nil, user.UnknownUserError(name)
	})

	m.stopRootlessSessionServices(context.Background(), "bunker-gone", m.logger)

	if len(*calls) != 0 {
		t.Errorf("sweep ran for an unresolvable user: %v", *calls)
	}
}

// ── Ordering: the sweep runs BEFORE the gate (full destroy) ─────────────────

// TestDestroy_StopSweepRunsBeforeGate proves the remedy is INSIDE the destroy
// path, ahead of the gate: with the fixture /proc showing the session
// services and the gate's probe reporting them absorbed, a full destroy
// issues the sweep's kills BEFORE userdel runs (shared-log ordering), and the
// destroy completes.
func TestDestroy_StopSweepRunsBeforeGate(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	const id = "dfb66-order"
	const username = "bunker-" + id
	liveAgent(t, m, id)

	// The sweep's evidence source: a fixture /proc with the pair + services
	// (the gate's stubbed probe reports the same set, absorbed).
	procDirFixture(t, rootlessSweepFixture())
	presentUserWithUID(t, username, "61010")

	oldGrace := terminateUserManagerGrace
	terminateUserManagerGrace = 50 * time.Millisecond
	t.Cleanup(func() { terminateUserManagerGrace = oldGrace })
	shortStopRootlessSessionTimeout(t)

	// Shared ordering log: the sweep's kills and the userdel both append.
	orderLog := filepath.Join(t.TempDir(), "order.log")
	calls := stubStopRootlessSessionRunner(t)
	// Wrap the recorder to also write the shared log.
	inner := stopRootlessSessionRunner
	stopRootlessSessionRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := inner(ctx, name, args...)
		_ = os.WriteFile(orderLog, append(logBytes(t, orderLog), []byte(name+"\n")...), 0o644)
		return out, err
	}

	stubDir := t.TempDir()
	script := "#!/bin/sh\nprintf 'userdel\\n' >> \"" + orderLog + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "userdel"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The gate's probe: only session infrastructure remains.
	fake := []userProcess{
		{PID: 7001, Cmd: "/usr/lib/systemd/systemd --user"},
		{PID: 7002, Cmd: "(sd-pam)"},
		{PID: 7003, Cmd: "/usr/bin/dbus-daemon --session"},
		{PID: 7004, Cmd: "/usr/bin/pipewire"},
	}
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return fake, 61010, true, nil
	})

	resp, err := m.Destroy(context.Background(), id, false)
	if err != nil {
		t.Fatalf("destroy with only session infrastructure must complete: %v", err)
	}
	if resp.Status != "destroyed" {
		t.Fatalf("status = %q, want destroyed", resp.Status)
	}

	// The sweep actually ran (kills recorded against the fixture).
	var sawKill bool
	for _, c := range *calls {
		if strings.HasPrefix(c, "kill") {
			sawKill = true
		}
	}
	if !sawKill {
		t.Errorf("the sweep never killed the fixture's session services: %v", *calls)
	}

	// ORDER: every kill line precedes the userdel line in the shared log.
	data, rerr := os.ReadFile(orderLog)
	if rerr != nil {
		t.Fatalf("order log missing: %v", rerr)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	firstKill, firstUserdel := -1, -1
	for i, l := range lines {
		if firstKill < 0 && l == "kill" {
			firstKill = i
		}
		if firstUserdel < 0 && l == "userdel" {
			firstUserdel = i
		}
	}
	if firstKill < 0 || firstUserdel < 0 {
		t.Fatalf("order log incomplete: kills=%d userdel=%d lines=%v", firstKill, firstUserdel, lines)
	}
	if firstKill > firstUserdel {
		t.Errorf("the stop sweep ran AFTER userdel (kill@%d > userdel@%d): %v", firstKill, firstUserdel, lines)
	}
}

// logBytes reads the ordering log, tolerating absence.
func logBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return data
}

// ── The gate's updated remedy text ──────────────────────────────────────────

// TestBuildDestroyLiveProcessRefusal_NamesTheAutomaticRemedy pins the
// refusal's DF-66 remedy: it tells the operator the destroy path already
// stops the session's own services automatically, so whatever the refusal
// names is operator-owned.
func TestBuildDestroyLiveProcessRefusal_NamesTheAutomaticRemedy(t *testing.T) {
	procs := []userProcess{{PID: 4242, Cmd: "node /home/bunker-x/app/server.js"}}
	msg := buildDestroyLiveProcessRefusal("bunker-x", 61012, procs)
	for _, want := range []string{
		"destroy refused",
		"bunker-x",
		"61012",
		"pid 4242",
		"already stops the rootless-docker session's own services",
		"started by the agent's operator",
		"retry the destroy",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal missing %q — got: %s", want, msg)
		}
	}
	// The stale absolute claim must not come back.
	if strings.Contains(msg, "they are NOT killed by bunker destroy") {
		t.Errorf("refusal still carries the pre-DF-66 absolute claim: %s", msg)
	}
}
