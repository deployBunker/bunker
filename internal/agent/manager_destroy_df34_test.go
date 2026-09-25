package agent

// DF-BUNKER-34 regression tests. Every test drives the destroy gate, the
// drift probe or the orphan classification through SEAMS — no /proc writes,
// no real userdel, no host users.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// stubDestroyProcessProbe swaps the destroy gate's process probe for fn and
// restores it via t.Cleanup (tests in this package run in one process).
func stubDestroyProcessProbe(t *testing.T, fn func(username string) ([]userProcess, uint32, bool, error)) {
	t.Helper()
	orig := destroyProcessProbe
	destroyProcessProbe = fn
	t.Cleanup(func() { destroyProcessProbe = orig })
}

// presentUserWithUID stubs lookupUser to resolve username to the given uid.
func presentUserWithUID(t *testing.T, username string, uid string) {
	t.Helper()
	stubLookupUser(t, func(name string) (*user.User, error) {
		if name == username {
			return &user.User{Username: name, Uid: uid, Gid: uid, HomeDir: "/home/" + name}, nil
		}
		return nil, user.UnknownUserError(name)
	})
}

// newGateManager is a hermetic manager for the destroy-gate tests: disabled
// registry, no system agents, recorder-backed host provisioning. buf may be
// nil (an io.Discard logger is used).
func newGateManager(t *testing.T, buf *bytes.Buffer) *AgentManager {
	t.Helper()
	var logger *slog.Logger
	if buf == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	} else {
		logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	cfg := config.DefaultConfig()
	cfg.Agent.Registry.Enabled = false
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	m := NewAgentManager(cfg, logger, tracker, nil, nil)
	m.listSystemAgents = func() ([]SystemAgent, error) { return nil, nil }
	t.Cleanup(func() { m.Stop() })
	return m
}

// liveAgent registers a running tracker record so the destroy path runs its
// full teardown sequence against the stubs.
func liveAgent(t *testing.T, m *AgentManager, id string) {
	t.Helper()
	if m.portAlloc != nil {
		if _, _, err := m.portAlloc.Allocate(id); err != nil {
			t.Fatalf("allocate ports for %s: %v", id, err)
		}
	}
	if err := m.tracker.Register(&resource.AgentRecord{
		AgentID:   id,
		Status:    StatusRunning,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

// procFixtureEntry is one fixture /proc/<PID> entry: PID is the directory
// name, UID the ownership, Cmd the (space-joined) cmdline.
type procFixtureEntry struct {
	PID  string
	UID  uint32
	Name string
	Cmd  string
}

// fixtureProcStatus renders a /proc/<pid>/status body the ownership probe
// reads.
func fixtureProcStatus(uid uint32, name string) []byte {
	uidStr := itoaUID(uid)
	return []byte("Name:\t" + name + "\nUid:\t" + uidStr + "\t" + uidStr + "\t" + uidStr + "\t" + uidStr + "\n")
}

func itoaUID(v uint32) string {
	if v == 0 {
		return "0"
	}
	digits := ""
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	return digits
}

// procDirFixture builds a fixture /proc tree (one directory per entry, each
// with a status file and — when cmd is non-empty — a cmdline file) and swaps
// the probes onto it for the test's duration.
func procDirFixture(t *testing.T, procs []procFixtureEntry) {
	t.Helper()
	root := t.TempDir()
	for _, p := range procs {
		dir := filepath.Join(root, p.PID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "status"), fixtureProcStatus(p.UID, p.Name), 0o644); err != nil {
			t.Fatal(err)
		}
		if p.Cmd != "" {
			if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.ReplaceAll(p.Cmd, " ", "\x00")), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	orig := procStatusPath
	procStatusPath = root
	t.Cleanup(func() { procStatusPath = orig })
}

// ── Criterion 2: destroy refuses loudly on a live uid process ──────────────

// TestDestroy_LiveProcessGate is the row's criterion-2 acceptance: a destroy
// whose agent uid still owns a live process (evidence from a FAKE process
// source, no root) fails loudly with a named error that carries the process
// evidence, reports status live_processes, and deletes NOTHING (the user,
// home, tracker record and port range all survive).
//
// DF-BUNKER-63: this table used to iterate force={false,true} and assert the
// refusal for BOTH — that force row encoded the defect this repo inherited
// (there was no operator exit hatch: `destroy --force` refused exactly like
// a plain destroy, so an operator could not clear a wedged agent at all).
// The force arm has been MOVED, not deleted, into
// TestDestroy_Force_KillsUIDProcessesAndCompletes below with FLIPPED
// expectations; the non-force control row here is untouched and must keep
// passing byte-identically.
func TestDestroy_LiveProcessGate(t *testing.T) {
	for _, force := range []bool{false} { // DF-BUNKER-63: the force=true row moved out (flipped) — see above
		name := "non_force"
		if force {
			name = "force"
		}
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			m := newGateManager(t, &buf)
			const id = "dfb34-live"
			const username = "bunker-" + id
			liveAgent(t, m, id)

			// The gate's evidence source: a fake probe reporting one live
			// process under the agent's uid. No /proc, no root.
			fake := []userProcess{{PID: 4242, Cmd: "node /home/bunker-dfb34-live/app/server.js"}}
			stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
				return fake, 61001, true, nil
			})
			presentUserWithUID(t, username, "61001")

			// userdel recorder: the destroy must NEVER reach it.
			userLog := filepath.Join(t.TempDir(), "userdel.log")
			stubDir := t.TempDir()
			script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + userLog + "\"\n"
			if err := os.WriteFile(filepath.Join(stubDir, "userdel"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			resp, err := m.Destroy(context.Background(), id, force)
			if err == nil {
				t.Fatal("destroy with a live uid process must FAIL, not proceed")
			}
			if resp == nil || resp.Status != StatusLiveProcesses {
				t.Fatalf("status = %v, want %q", resp, StatusLiveProcesses)
			}
			// The refusal names the user, the uid and the live process.
			for _, want := range []string{"destroy refused", username, "61001", "pid 4242", "node /home/bunker-dfb34-live/app/server.js"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal error missing %q — got: %v", want, err)
				}
			}
			// The evidence is on the log record too (force mode included).
			if !strings.Contains(buf.String(), "agent uid still owns live processes") {
				t.Errorf("log missing the refusal record; log:\n%s", buf.String())
			}
			// Nothing was deleted: userdel never ran.
			if calls, rerr := os.ReadFile(userLog); rerr == nil && len(calls) > 0 {
				t.Errorf("userdel ran despite live-process refusal: %q", calls)
			}
			// The tracker record and port range survive (nothing destroyed).
			if rec := m.tracker.Get(id); rec == nil {
				t.Error("tracker record lost on live-process refusal")
			}
			if m.portAlloc != nil && !m.portAlloc.Has(id) {
				t.Error("port range leaked on live-process refusal")
			}
		})
	}
}

// TestDestroy_ProcessFreeUserProceeds is the control arm: with the fake
// probe reporting NO live processes, the destroy proceeds exactly as before
// (userdel runs once, status destroyed) — the gate must not block a clean
// teardown.
func TestDestroy_ProcessFreeUserProceeds(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	const id = "dfb34-clean"
	const username = "bunker-" + id
	liveAgent(t, m, id)

	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return nil, 61001, true, nil
	})
	presentUserWithUID(t, username, "61001")

	userLog := filepath.Join(t.TempDir(), "userdel.log")
	stubDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + userLog + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "userdel"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	resp, err := m.Destroy(context.Background(), id, false)
	if err != nil {
		t.Fatalf("process-free destroy must succeed: %v", err)
	}
	if resp.Status != "destroyed" {
		t.Fatalf("status = %q, want destroyed", resp.Status)
	}
	if calls, rerr := os.ReadFile(userLog); rerr != nil || len(calls) == 0 {
		t.Errorf("userdel never ran on the clean path (calls: %q, err: %v)", calls, rerr)
	}
}

// TestDestroy_UserdelFailureSurfacesEvidence covers criterion 2's second
// leg: userdel -rf itself fails on a busy home (NOT the user-absent class)
// and the destroy must surface it as a hard error carrying the
// surviving-process evidence — never the historical silent not_found.
func TestDestroy_UserdelFailureSurfacesEvidence(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	const id = "dfb34-userdel-fail"
	const username = "bunker-" + id
	liveAgent(t, m, id)

	// The gate passes (no live processes at gate time), then userdel fails
	// with the busy-home output.
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return nil, 61001, true, nil
	})
	presentUserWithUID(t, username, "61001")

	// The evidence probe (run AFTER userdel fails) reports the surviving
	// process: the incident's shape — a scheduler daemon still ticking under
	// the agent's uid after the userdel failure.
	procDirFixture(t, []procFixtureEntry{
		{PID: "1220620", UID: 61001, Name: "schedulerd", Cmd: "/home/bunker-dfb34-live/bin/schedulerd --workdir /home/bunker-dfb34-live/eduos"},
	})

	userLog := filepath.Join(t.TempDir(), "userdel.log")
	stubDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + userLog + "\"\necho 'userdel: error removing directory /home/bunker-dfb34-live' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(stubDir, "userdel"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	resp, err := m.Destroy(context.Background(), id, false)
	if err == nil {
		t.Fatal("a failed userdel (busy home) must be a hard error, not not_found")
	}
	if resp == nil || resp.Status != StatusUserdelFailed {
		t.Fatalf("status = %v, want %q", resp, StatusUserdelFailed)
	}
	// The error names the destroy, the userdel failure and the SURVIVING
	// process evidence.
	for _, want := range []string{"destroy of " + id + " failed", "userdel error", "pid 1220620", "schedulerd"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("hard error missing %q — got: %v", want, err)
		}
	}
	// The not_found lie must not come back.
	if strings.Contains(err.Error(), "not found") {
		t.Errorf("error reads as not_found: %v", err)
	}
}

// ── Criterion 4: orphan-uid detection ──────────────────────────────────────

// TestListUserProcesses_ClassifiesByUID is the probe's own table: it lists
// only the processes whose status Uid matches, sorts them by PID, and never
// lets an unrelated uid's process through. (The fixture writes pids in
// NON-sorted file order — 100, 55, 77 — so the sort is proven, not assumed.)
func TestListUserProcesses_ClassifiesByUID(t *testing.T) {
	procDirFixture(t, []procFixtureEntry{
		{PID: "100", UID: 61001, Name: "node", Cmd: "node /home/bunker-x/app.js"},
		{PID: "55", UID: 61002, Name: "python", Cmd: "python /other.py"},
		{PID: "77", UID: 61001, Name: "sh", Cmd: "sh -c long-running"},
		{PID: "nonnum", UID: 61001, Name: "notapidir", Cmd: "should-not-match"},
	})

	procs, err := listUserProcesses(61001)
	if err != nil {
		t.Fatalf("listUserProcesses: %v", err)
	}
	if len(procs) != 2 {
		t.Fatalf("got %d processes, want 2 (the 61001 pids, sorted): %+v", len(procs), procs)
	}
	// Sorted by PID even though /proc directory order is lexicographic
	// (100 < 77 as text, 77 < 100 numerically) — the sort is proven, not
	// assumed.
	if procs[0].PID != 77 || procs[1].PID != 100 {
		t.Errorf("processes not sorted by pid: %+v", procs)
	}
	if procs[0].Cmd != "sh -c long-running" || procs[1].Cmd != "node /home/bunker-x/app.js" {
		t.Errorf("cmd heads wrong: %+v", procs)
	}

	// The other uid sees only its own process.
	procs, err = listUserProcesses(61002)
	if err != nil {
		t.Fatalf("listUserProcesses(61002): %v", err)
	}
	if len(procs) != 1 || procs[0].Cmd != "python /other.py" {
		t.Errorf("uid 61002 processes = %+v, want exactly the python process", procs)
	}
}

// TestOrphanUIDCheck_Classification is the criterion-4 unit table: the
// classification function must call the orphan case exactly (user gone +
// live processes) and must never classify user-present, process-free or
// unknown states as orphan.
func TestOrphanUIDCheck_Classification(t *testing.T) {
	tests := []struct {
		name string
		chk  orphanUIDCheck
		want bool
	}{
		{
			name: "user gone + live processes = ORPHAN",
			chk: orphanUIDCheck{UID: 61001, UIDKnown: true, UserExists: false,
				Processes: []userProcess{{PID: 1, Cmd: "node"}}},
			want: true,
		},
		{
			name: "user present + processes = not orphan (destroy gate's case)",
			chk: orphanUIDCheck{UID: 61001, UIDKnown: true, UserExists: true,
				Processes: []userProcess{{PID: 1, Cmd: "node"}}},
			want: false,
		},
		{
			name: "user gone + no processes = not orphan",
			chk:  orphanUIDCheck{UID: 61001, UIDKnown: true, UserExists: false},
			want: false,
		},
		{
			name: "uid unknown = never orphan (fail closed)",
			chk:  orphanUIDCheck{UserExists: false, Processes: []userProcess{{PID: 1, Cmd: "x"}}},
			want: false,
		},
		{
			name: "probe error = never orphan",
			chk:  orphanUIDCheck{UIDKnown: true, UserExists: false, ProbeErr: "list /proc: permission denied"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.chk.IsOrphan(); got != tt.want {
				t.Errorf("IsOrphan() = %v, want %v (check: %+v)", got, tt.want, tt.chk)
			}
		})
	}
}

// TestOrphanUIDSummary_UserPresentStaysEmpty is the manager-level
// criterion-4 arm that runs everywhere (no root): an agent whose user record
// resolves must never carry an orphan summary — the summary is empty for
// every non-orphan class, so absence never fabricates a verdict.
func TestOrphanUIDSummary_UserPresentStaysEmpty(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)

	// The user record RESOLVES (presentUserWithUID) and the uid owns nothing
	// (empty fixture /proc): healthy, empty summary.
	procDirFixture(t, nil)
	presentUserWithUID(t, "bunker-present-x", "61004")
	if got := m.OrphanUIDSummary("present-x"); got != "" {
		t.Errorf("user-present agent must not carry an orphan summary, got %q", got)
	}
	// Invalid / empty ids stay empty.
	if got := m.OrphanUIDSummary(""); got != "" {
		t.Errorf("empty id must be empty, got %q", got)
	}
	if got := m.OrphanUIDSummary("Bad_ID"); got != "" {
		t.Errorf("invalid id must be empty, got %q", got)
	}
}

// TestOrphanUIDCheck_HomeOwnershipUIDSource proves the orphan path's uid
// source: with the user record gone, the probe reads the uid from the HOME's
// on-disk ownership (the artifact userdel-without-clean-home leaves). The
// fixture home is chowned when the test can (root); otherwise the uid source
// is unobservable and the check must fail CLOSED (unknown, not orphan).
func TestOrphanUIDCheck_HomeOwnershipUIDSource(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)

	homeRoot := t.TempDir()
	origRoot := agentHomeRoot
	agentHomeRoot = homeRoot
	t.Cleanup(func() { agentHomeRoot = origRoot })
	home := filepath.Join(homeRoot, "bunker-orphan-y")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	const uid = 61005
	root := t.TempDir()
	procStatusPathOrig := procStatusPath
	procStatusPath = root
	t.Cleanup(func() { procStatusPath = procStatusPathOrig })

	if os.Geteuid() == 0 {
		// Root: chown the home, seed a live process under the uid, make the
		// user record gone, and assert the FULL orphan classification.
		if err := os.Chown(home, uid, uid); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "901", "status"), fixtureProcStatus(uid, "duckbrain.js"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "900", "status"), fixtureProcStatus(1, "init"), 0o644); err != nil {
			t.Fatal(err)
		}
		stubLookupUser(t, func(name string) (*user.User, error) {
			return nil, user.UnknownUserError(name)
		})

		c := m.checkOrphanUID("orphan-y")
		if !c.IsOrphan() {
			t.Fatalf("orphan case not classified: %+v", c)
		}
		if c.UID != uid {
			t.Errorf("UID = %d, want %d (from the home's ownership)", c.UID, uid)
		}
		if len(c.Processes) != 1 || c.Processes[0].PID != 900 {
			t.Errorf("processes = %+v, want exactly pid 900 (the uid's)", c.Processes)
		}
	} else {
		// Non-root: the home's owner is the test user's own uid, and that IS
		// observable — the probe reads it and (with no process under that uid
		// in the fixture) classifies "user gone + uid owns nothing" as NOT an
		// orphan. The fail-closed property asserted here is the opposite
		// edge: a uid whose probe finds nothing must never read as orphan.
		stubLookupUser(t, func(name string) (*user.User, error) {
			return nil, user.UnknownUserError(name)
		})
		if err := os.MkdirAll(filepath.Join(root, "901"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "901", "status"), fixtureProcStatus(1, "init"), 0o644); err != nil {
			t.Fatal(err)
		}
		c := m.checkOrphanUID("orphan-y")
		if c.IsOrphan() {
			t.Errorf("non-root orphan check classified an orphan with no matching live process: %+v", c)
		}
		if !c.UIDKnown {
			t.Errorf("non-root check should resolve the uid from the home's ownership: %+v", c)
		}
	}
}

// TestDestroy_SessionPairAbsorbed covers DF-BUNKER-56 criterion 1: the
// agent's own systemd session pair ("systemd --user" + "(sd-pam)") is
// lifecycle infrastructure every lingered agent legitimately owns — CI run
// 35998446840 refused EVERY healthy destroy because the DF-BUNKER-34 gate
// counted the pair as orphanable live processes. Destroy must proceed
// (status destroyed, userdel runs) when ONLY the pair remains, in force and
// non-force mode alike.
func TestDestroy_SessionPairAbsorbed(t *testing.T) {
	for _, force := range []bool{false, true} {
		name := "non_force"
		if force {
			name = "force"
		}
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			m := newGateManager(t, &buf)
			const id = "dfb56-pair"
			const username = "bunker-" + id
			liveAgent(t, m, id)

			// The gate's evidence source: the pair, and nothing else. The
			// pids are fakes; the Cmd heads are the CI-measured shapes.
			fake := []userProcess{
				{PID: 5001, Cmd: "/usr/lib/systemd/systemd --user"},
				{PID: 5002, Cmd: "(sd-pam)"},
			}
			stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
				return fake, 61002, true, nil
			})
			presentUserWithUID(t, username, "61002")

			// Keep the terminate step's grace wait short: the stubbed probe
			// never reports the pair gone, so the step rides out the whole
			// deadline — which is exactly the path under test.
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

			resp, err := m.Destroy(context.Background(), id, force)
			if err != nil {
				t.Fatalf("destroy with only the session pair alive must succeed: %v", err)
			}
			if resp.Status != "destroyed" {
				t.Fatalf("status = %q, want destroyed", resp.Status)
			}
			if calls, rerr := os.ReadFile(userLog); rerr != nil || len(calls) == 0 {
				t.Errorf("userdel never ran on the pair-only path (calls: %q, err: %v)", calls, rerr)
			}
			// The terminate step warned (no logind session for the fake user
			// in the test environment) and the destroy continued anyway.
			if !strings.Contains(buf.String(), "loginctl terminate-user failed") {
				t.Errorf("log missing the terminate-user WARN record; log:\n%s", buf.String())
			}
			// The tracker record and port range are gone (destroy completed).
			if rec := m.tracker.Get(id); rec != nil {
				t.Error("tracker record survived a successful destroy")
			}
		})
	}
}

// TestDestroy_OperatorProcessStillRefusedWithPair covers criterion 2: the
// pair absorption must NOT swallow real operator processes — pair + node
// server still refuses with live_processes, and the refusal names ONLY the
// node process.
func TestDestroy_OperatorProcessStillRefusedWithPair(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	const id = "dfb56-mixed"
	const username = "bunker-" + id
	liveAgent(t, m, id)

	fake := []userProcess{
		{PID: 5100, Cmd: "/usr/lib/systemd/systemd --user"},
		{PID: 5101, Cmd: "(sd-pam)"},
		{PID: 5102, Cmd: "node /home/bunker-dfb56-mixed/app/server.js"},
	}
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return fake, 61003, true, nil
	})
	presentUserWithUID(t, username, "61003")

	oldGrace := terminateUserManagerGrace
	terminateUserManagerGrace = 50 * time.Millisecond
	t.Cleanup(func() { terminateUserManagerGrace = oldGrace })

	resp, err := m.Destroy(context.Background(), id, false)
	if err == nil {
		t.Fatal("destroy with an operator process alive must FAIL even when the session pair is present")
	}
	if resp == nil || resp.Status != StatusLiveProcesses {
		t.Fatalf("status = %v, want %q", resp, StatusLiveProcesses)
	}
	for _, want := range []string{"destroy refused", "pid 5102", "node /home/bunker-dfb56-mixed/app/server.js"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal error missing %q — got: %v", want, err)
		}
	}
	for _, banned := range []string{"pid 5100", "pid 5101", "systemd --user", "(sd-pam)"} {
		if strings.Contains(err.Error(), banned) {
			t.Errorf("refusal error must not name the absorbed session pair (%q) — got: %v", banned, err)
		}
	}
}

// TestIsAgentSessionProcess covers the classifier's accepted shapes and its
// negatives: operator processes (node, dockerd, rootlesskit) must never
// classify as the session pair.
func TestIsAgentSessionProcess(t *testing.T) {
	tests := map[string]struct {
		cmd  string
		want bool
	}{
		"ci-measured-systemd":     {"/usr/lib/systemd/systemd --user", true},
		"debian-lib-path":         {"/lib/systemd/systemd --user", true},
		"bare-systemd":            {"systemd --user", true},
		"sd-pam-literal":          {"(sd-pam)", true},
		"sd-pam-bracketed-name":   {"[sd-pam]", true},
		"sd-pam-bracketed-argv":   {"[(sd-pam)]", true},
		"node-server":             {"node /home/bunker-x/app/server.js", false},
		"dockerd":                 {"/usr/bin/dockerd", false},
		"rootlesskit":             {"rootlesskit --net=slirp4netns", false},
		"unknown-empty-cmdline":   {"(unknown)", false},
		"lookalike-suffix-word":   {"tail -f systemd --userLog", false},
		"systemd-user-other-flag": {"/usr/lib/systemd/systemd --user --deserialize", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := isAgentSessionProcess(userProcess{PID: 1, Cmd: tc.cmd}); got != tc.want {
				t.Errorf("isAgentSessionProcess(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// ── DF-BUNKER-63: the destroy --force exit hatch ───────────────────────────

// TestDestroy_Force_KillsUIDProcessesAndCompletes is the FLIPPED force row
// moved out of TestDestroy_LiveProcessGate (see that test's comment): with
// force=true the destroy no longer refuses on the foreign processes — it
// runs the bounded SIGTERM → SIGKILL escalation, the kill list is logged,
// and the destroy completes over the normal path (status destroyed).
func TestDestroy_Force_KillsUIDProcessesAndCompletes(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	const id = "dfb63-force"
	const username = "bunker-" + id
	liveAgent(t, m, id)

	// The gate's evidence source: one foreign operator process. The probe is
	// call-indexed because the destroy path probes THREE times here: the
	// terminate step's wait (1), the gate entry (2), and the post-escalation
	// re-probe (3) — which reports the process GONE (the escalation worked).
	fake := []userProcess{{PID: 4242, Cmd: "node /home/bunker-dfb63-force/app/server.js"}}
	var signalled []string
	probeCalls := 0
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		probeCalls++
		if probeCalls <= 2 {
			return fake, 61005, true, nil
		}
		return nil, 0, true, nil
	})
	// Keep the terminate step's grace wait short: it rides out the whole
	// deadline on the stubbed probe otherwise.
	oldGrace := terminateUserManagerGrace
	terminateUserManagerGrace = 50 * time.Millisecond
	t.Cleanup(func() { terminateUserManagerGrace = oldGrace })
	m.forceKillUserProcessesFn = func(username string, uid uint32, procs []userProcess) {
		for _, p := range procs {
			// The kill list is recorded, never signalled.
			signalled = append(signalled, fmt.Sprintf("pid=%d", p.PID))
		}
	}
	presentUserWithUID(t, username, "61005")

	userLog := filepath.Join(t.TempDir(), "userdel.log")
	stubDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + userLog + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "userdel"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	resp, err := m.Destroy(context.Background(), id, true)
	if err != nil {
		t.Fatalf("force destroy must kill the uid's processes and complete: %v", err)
	}
	if resp.Status != "destroyed" {
		t.Fatalf("status = %q, want destroyed", resp.Status)
	}
	if len(signalled) == 0 {
		t.Error("the force escalation never signalled the foreign process")
	}
	for _, want := range []string{"pid=4242"} {
		found := false
		for _, s := range signalled {
			if s == want {
				found = true
			}
		}
		if !found {
			t.Errorf("kill list missing %q (signalled: %v)", want, signalled)
		}
	}
	if calls, rerr := os.ReadFile(userLog); rerr != nil || len(calls) == 0 {
		t.Errorf("userdel never ran after the force escalation (calls: %q, err: %v)", calls, rerr)
	}
	// The kill list is on the log record.
	if !strings.Contains(buf.String(), "destroy --force cleared the agent uid's live processes") {
		t.Errorf("log missing the force-clear record; log:\n%s", buf.String())
	}
	// The tracker record is gone (destroy completed).
	if rec := m.tracker.Get(id); rec != nil {
		t.Error("tracker record survived a successful force destroy")
	}
}

// TestDestroy_Force_SurvivorsStillRefuse pins the honesty channel: an
// escalation that CANNOT clear the uid (SIGKILL-immune process) leaves the
// destroy refusing loudly — force never trades evidence for momentum, and
// nothing has been deleted when it fires.
func TestDestroy_Force_SurvivorsStillRefuse(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	const id = "dfb63-survivor"
	const username = "bunker-" + id
	liveAgent(t, m, id)

	fake := []userProcess{{PID: 4343, Cmd: "uninterruptible /home/bunker-dfb63-survivor/app"}}
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		return fake, 61006, true, nil
	})
	// The escalation ran; the re-probe still shows the process.
	m.forceKillUserProcessesFn = func(string, uint32, []userProcess) {}
	presentUserWithUID(t, username, "61006")

	userLog := filepath.Join(t.TempDir(), "userdel.log")
	stubDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"" + userLog + "\"\n"
	if err := os.WriteFile(filepath.Join(stubDir, "userdel"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	resp, err := m.Destroy(context.Background(), id, true)
	if err == nil {
		t.Fatal("a process that survives the escalation must still refuse the destroy")
	}
	if resp == nil || resp.Status != StatusLiveProcesses {
		t.Fatalf("status = %v, want %q", resp, StatusLiveProcesses)
	}
	for _, want := range []string{"survived the destroy --force escalation", "pid 4343"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal error missing %q — got: %v", want, err)
		}
	}
	if calls, rerr := os.ReadFile(userLog); rerr == nil && len(calls) > 0 {
		t.Errorf("userdel ran despite surviving processes: %q", calls)
	}
	if rec := m.tracker.Get(id); rec == nil {
		t.Error("tracker record lost on a surviving-process refusal")
	}
}

// TestDestroy_Force_UnobservablePostEscalationRefuses pins the no-blind-userdel
// rule: when the post-escalation probe cannot observe the uid, even --force
// refuses instead of deleting blind.
func TestDestroy_Force_UnobservablePostEscalationRefuses(t *testing.T) {
	var buf bytes.Buffer
	m := newGateManager(t, &buf)
	const id = "dfb63-blind"
	const username = "bunker-" + id
	liveAgent(t, m, id)

	fake := []userProcess{{PID: 4444, Cmd: "node /home/bunker-dfb63-blind/app"}}
	// The probe is call-indexed: the terminate step's wait consumes the first
	// call(s), the gate entry reports the foreign process, and the
	// post-escalation probe reports the user record GONE (unobservable).
	calls := 0
	stubDestroyProcessProbe(t, func(string) ([]userProcess, uint32, bool, error) {
		calls++
		if calls <= 2 {
			return fake, 61007, true, nil
		}
		return nil, 0, false, nil
	})
	// Keep the terminate step's grace wait short.
	oldGrace := terminateUserManagerGrace
	terminateUserManagerGrace = 50 * time.Millisecond
	t.Cleanup(func() { terminateUserManagerGrace = oldGrace })
	m.forceKillUserProcessesFn = func(string, uint32, []userProcess) {}
	presentUserWithUID(t, username, "61007")

	resp, err := m.Destroy(context.Background(), id, true)
	if err == nil {
		t.Fatal("an unobservable post-escalation state must refuse even in force mode")
	}
	if resp == nil || resp.Status != StatusLiveProcesses {
		t.Fatalf("status = %v, want %q", resp, StatusLiveProcesses)
	}
	if !strings.Contains(err.Error(), "unobservable") {
		t.Errorf("refusal error does not name the unobservable state: %v", err)
	}
}
