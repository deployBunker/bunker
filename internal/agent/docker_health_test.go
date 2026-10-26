package agent

// DF-BUNKER-72 part 3 / DF-BUNKER-108: the degraded-status surface.
//
// DF-BUNKER-72's original surface classified the INSTALLER's per-user
// docker.service — which on DF-BUNKER-96 hosts sits `failed` on every
// affected agent one second after spawn (RootlessKit state-lock collision
// with the manager's bunker-docker-<id> unit), so healthy agents rendered
// `running (docker degraded)` (DF-BUNKER-108, three live arms). These tests
// pin the corrected proof chain, the board row's acceptance arms:
//
//   (a) a socket that answers (`docker ps` rc=0) never renders the
//       qualifier — even when the installer's docker.service is failed;
//   (b) the verdict derives from the SERVING unit (bunker-docker-<id>)
//       plus the socket, never from docker.service;
//   (c) the cause names the unit AND the missing socket;
//   (d) the regression arms: unit failed/inactive + socket absent →
//       qualifier; anything else → silence; a status that does not claim
//       a live agent is never qualified.

import (
	"context"
	"errors"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// dfb108SwapProbe swaps the serving-unit probe seam and records every call
// (username, runtimeDir, unitName). state="" routes through the session seam
// instead of answering directly (the health surface's production fallback in
// these tests).
func dfb108SwapProbe(t *testing.T, state string, err error) *dfb108ProbeCall {
	t.Helper()
	call := &dfb108ProbeCall{}
	prev := dockerUnitStateProbe
	dockerUnitStateProbe = func(_ context.Context, username, runtimeDir, unitName string) (string, error) {
		call.calls++
		call.username = username
		call.runtimeDir = runtimeDir
		call.unitName = unitName
		return state, err
	}
	t.Cleanup(func() { dockerUnitStateProbe = prev })
	return call
}

type dfb108ProbeCall struct {
	calls      int
	username   string
	runtimeDir string
	unitName   string
}

// dfb108SwapUser swaps the user-lookup seam so the agent user resolves to
// uid (or does not resolve at all when uid < 0).
func dfb108SwapUser(t *testing.T, agentID string, uid int) {
	t.Helper()
	prev := userLookup
	userLookup = func(name string) (*user.User, error) {
		if name != "bunker-"+agentID || uid < 0 {
			return nil, errors.New("no such user")
		}
		return &user.User{Uid: strconv.Itoa(uid), Gid: strconv.Itoa(uid), Username: name}, nil
	}
	t.Cleanup(func() { userLookup = prev })
}

// dfb108ListenSocket binds a REAL unix socket at path and keeps it open for
// the test's duration — the live shape of a serving dockerd socket.
func dfb108ListenSocket(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

// dfb108SocketsRoot swaps spawnRunRoot to a temp root and returns it.
func dfb108SocketsRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	swapStringSeam(t, &spawnRunRoot, root)
	return root
}

// TestDFBUNKER108_SocketPresentNeverDegrades is acceptance arm (a) and the
// incident's regression arm (d, first half): a healthy agent — its socket
// answering on /run/bunker/<id>/docker.sock — is NEVER qualified, even with
// an installer docker.service that reports `failed` the moment it is asked.
// The installer unit must not even be consulted.
func TestDFBUNKER108_SocketPresentNeverDegrades(t *testing.T) {
	const agentID = "df108healthy"
	root := dfb108SocketsRoot(t)
	dfb108ListenSocket(t, filepath.Join(root, agentID, dockerSocketName))
	m, _, _ := newLifecycleManager(t)
	dfb108SwapUser(t, agentID, 61100)
	// The probe would answer `failed` if it were ever called — it must not be.
	call := dfb108SwapProbe(t, dockerUnitFailed, errors.New("exit status 1"))

	for _, status := range []string{StatusRunning, dockerStatusStarting} {
		if got := m.DockerStatus(agentID, status); got != status {
			t.Errorf("serving-socket DockerStatus(%q) = %q, want byte-identical (docker answers)", status, got)
		}
	}
	if call.calls != 0 {
		t.Errorf("probe calls = %d, want 0 — a serving socket must settle the verdict alone", call.calls)
	}
	if detail := m.DockerDegradedDetail(agentID); detail != "" {
		t.Errorf("DockerDegradedDetail with a serving socket = %q, want empty", detail)
	}
}

// TestDFBUNKER108_StaleFileIsNotAServingSocket pins the socket-mode guard: a
// regular file left at the socket path is not a daemon — the probe chain
// still runs and a dead serving unit still qualifies.
func TestDFBUNKER108_StaleFileIsNotAServingSocket(t *testing.T) {
	const agentID = "df108stale"
	root := dfb108SocketsRoot(t)
	sockPath := filepath.Join(root, agentID, dockerSocketName)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(sockPath), err)
	}
	if err := os.WriteFile(sockPath, []byte("not a socket"), 0o644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	m, _, _ := newLifecycleManager(t)
	dfb108SwapUser(t, agentID, 61101)
	dfb108SwapProbe(t, dockerUnitInactive, errors.New("exit status 3"))

	if got := m.DockerStatus(agentID, StatusRunning); got != StatusRunning+" ("+DockerDegradedQualifier+")" {
		t.Errorf("stale-file DockerStatus = %q, want the qualified running status", got)
	}
}

// TestDFBUNKER108_MissingSocketUnitStateTable is acceptance arm (d): with the
// socket absent, the SERVING unit's state decides — inactive/failed qualify,
// everything the probe cannot prove stays silent. Every probe call must
// address the agent's own manager and the agent's OWN unit.
func TestDFBUNKER108_MissingSocketUnitStateTable(t *testing.T) {
	const (
		agentID = "df108table"
		uid     = 61102
	)
	runtimeBase := t.TempDir()
	swapStringSeam(t, &userRuntimeBaseDir, runtimeBase)
	dfb108SocketsRoot(t) // no socket is ever created under this root

	cases := []struct {
		name        string
		state       string
		probeErr    error
		wantStatus  string
		description string
	}{
		{
			name:        "inactive serving unit degrades the status",
			state:       "inactive\n",
			probeErr:    errors.New("exit status 3"),
			wantStatus:  "running (docker degraded)",
			description: "unit gone + socket gone = the agent has no docker while claiming running",
		},
		{
			name:        "failed serving unit degrades the status",
			state:       "failed\n",
			probeErr:    errors.New("exit status 1"),
			wantStatus:  "running (docker degraded)",
			description: "the manager unit lost the rootlesskit race and died",
		},
		{
			name:        "active unit against a missing socket claims nothing",
			state:       "active\n",
			probeErr:    nil,
			wantStatus:  "running",
			description: "dockerd up, socket not yet bound — a transient window, never a verdict",
		},
		{
			name:        "activating unit claims nothing",
			state:       "activating\n",
			probeErr:    nil,
			wantStatus:  "running",
			description: "a unit coming up is transient",
		},
		{
			name:        "dead session bus claims nothing",
			state:       "",
			probeErr:    errors.New("Failed to connect to bus: No medium found"),
			wantStatus:  "running",
			description: "transport failure: no verdict (the info path must not be slowed or broken by it)",
		},
		{
			name:        "unrecognised answer claims nothing",
			state:       "unknown\n",
			probeErr:    errors.New("exit status 4"),
			wantStatus:  "running",
			description: "absence is never upgraded past what the answer proves",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := newLifecycleManager(t)
			dfb108SwapUser(t, agentID, uid)
			call := dfb108SwapProbe(t, tc.state, tc.probeErr)

			if got := m.DockerStatus(agentID, StatusRunning); got != tc.wantStatus {
				t.Errorf("DockerStatus = %q, want %q (%s)", got, tc.wantStatus, tc.description)
			}
			if call.calls != 1 {
				t.Fatalf("probe calls = %d, want exactly 1", call.calls)
			}
			if call.username != "bunker-"+agentID {
				t.Errorf("probe username = %q, want %q", call.username, "bunker-"+agentID)
			}
			if want := managerDockerUnitName(agentID); call.unitName != want {
				t.Errorf("probe unit = %q, want the SERVING unit %q (never the installer's docker.service)", call.unitName, want)
			}
			if want := filepath.Join(runtimeBase, strconv.Itoa(uid)); call.runtimeDir != want {
				t.Errorf("probe runtime dir = %q, want %q (the probe must speak to the agent's OWN manager bus)", call.runtimeDir, want)
			}
		})
	}
}

// TestDFBUNKER108_NonServingStatusesNeverQualified: a status that does not
// claim a live agent (stopped / home_retained / failed / empty) is never
// qualified — its missing socket is expected, not degraded — and the probe
// must not even run for it.
func TestDFBUNKER108_NonServingStatusesNeverQualified(t *testing.T) {
	const agentID = "df108stopped"
	dfb108SocketsRoot(t)
	m, _, _ := newLifecycleManager(t)
	dfb108SwapUser(t, agentID, 61103)
	call := dfb108SwapProbe(t, dockerUnitInactive, errors.New("exit status 3"))

	for _, status := range []string{StatusStopped, "home_retained", "failed", ""} {
		if got := m.DockerStatus(agentID, status); got != status {
			t.Errorf("non-serving DockerStatus(%q) = %q, want byte-identical", status, got)
		}
	}
	if call.calls != 0 {
		t.Errorf("probe calls = %d, want 0 — a stopped agent owes no socket", call.calls)
	}
}

// TestDFBUNKER108_StartingStatusQualifies pins that the transient pre-running
// status claims a (becoming-)live agent, so a lost docker qualifies it too.
func TestDFBUNKER108_StartingStatusQualifies(t *testing.T) {
	const agentID = "df108start"
	dfb108SocketsRoot(t)
	m, _, _ := newLifecycleManager(t)
	dfb108SwapUser(t, agentID, 61104)
	dfb108SwapProbe(t, dockerUnitInactive, errors.New("exit status 3"))

	if got := m.DockerStatus(agentID, dockerStatusStarting); got != "starting (docker degraded)" {
		t.Errorf("starting DockerStatus = %q, want %q", got, "starting (docker degraded)")
	}
}

// TestDFBUNKER108_DetailNamesServingUnitAndSocket is acceptance arm (c): the
// cause string names the serving unit, its state, and the missing socket —
// the operator-visible "why" the DF-BUNKER-108 finding demanded.
func TestDFBUNKER108_DetailNamesServingUnitAndSocket(t *testing.T) {
	const agentID = "df108detail"
	root := dfb108SocketsRoot(t)
	m, _, _ := newLifecycleManager(t)
	dfb108SwapUser(t, agentID, 61105)
	dfb108SwapProbe(t, dockerUnitInactive, errors.New("exit status 3"))

	detail := m.DockerDegradedDetail(agentID)
	for _, want := range []string{
		managerDockerUnitName(agentID),
		dockerUnitInactive,
		filepath.Join(root, agentID, dockerSocketName),
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("DockerDegradedDetail = %q, want it to name %q", detail, want)
		}
	}
	if strings.Contains(detail, dockerServiceUnitName) {
		t.Errorf("DockerDegradedDetail = %q, must not cite the installer unit %s", detail, dockerServiceUnitName)
	}
	if !strings.Contains(DockerDegradedQualifier, "docker") {
		t.Fatalf("DockerDegradedQualifier = %q, want a docker-scoped qualifier", DockerDegradedQualifier)
	}
}

// TestDFBUNKER108_DockerStatusWithoutAUserClaimsNothing: an agent whose user
// record is gone gets no docker verdict — that state belongs to the orphan
// probe (DF-BUNKER-34), and the docker probe must not double-report it.
func TestDFBUNKER108_DockerStatusWithoutAUserClaimsNothing(t *testing.T) {
	const agentID = "df108gone"
	dfb108SocketsRoot(t)
	m, _, _ := newLifecycleManager(t)
	dfb108SwapUser(t, agentID, -1) // never resolves
	call := dfb108SwapProbe(t, dockerUnitInactive, errors.New("exit status 3"))

	if got := m.DockerStatus(agentID, StatusRunning); got != StatusRunning {
		t.Errorf("DockerStatus without a user = %q, want running (no docker claim)", got)
	}
	if call.calls != 0 {
		t.Errorf("probe calls = %d, want 0 (nothing to ask without a uid)", call.calls)
	}
	if detail := m.DockerDegradedDetail(agentID); detail != "" {
		t.Errorf("DockerDegradedDetail without a user = %q, want empty", detail)
	}
}

// TestDFBUNKER108_StatusComposition covers the renderer's edge cases directly:
// an empty base status must not gain a fabricated "running".
func TestDFBUNKER108_StatusComposition(t *testing.T) {
	cases := []struct{ status, want string }{
		{"running", "running (docker degraded)"},
		{"stopped", "stopped (docker degraded)"},
		{"", DockerDegradedQualifier},
	}
	for _, tc := range cases {
		if got := dockerDegradedStatus(tc.status); got != tc.want {
			t.Errorf("dockerDegradedStatus(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}
