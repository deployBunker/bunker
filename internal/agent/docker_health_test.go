package agent

// DF-BUNKER-72 part 3: the degraded-status surface.
//
// On the dogfood-21 incident the agent's docker daemon was dead for the whole
// life of the agent while `bunker info` printed `Status: running`. These tests
// pin the classification and the rendered status string, plus the two things a
// health surface must never do: invent a degraded verdict it cannot prove
// (a missing unit, a dead bus, a uid that no longer resolves) and change the
// status of a healthy agent.

import (
	"context"
	"errors"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// dfb72HealthSwapProbe swaps the docker-unit probe seam and records the
// (username, runtimeDir) each call received.
func dfb72HealthSwapProbe(t *testing.T, state string, err error) *dfb72ProbeCall {
	t.Helper()
	call := &dfb72ProbeCall{}
	prev := dockerUnitStateProbe
	dockerUnitStateProbe = func(_ context.Context, username, runtimeDir string) (string, error) {
		call.calls++
		call.username = username
		call.runtimeDir = runtimeDir
		return state, err
	}
	t.Cleanup(func() { dockerUnitStateProbe = prev })
	return call
}

type dfb72ProbeCall struct {
	calls      int
	username   string
	runtimeDir string
}

// dfb72HealthSwapUser swaps the user-lookup seam so the agent user resolves to
// uid (or does not resolve at all when uid < 0).
func dfb72HealthSwapUser(t *testing.T, agentID string, uid int) {
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

// TestDFBUNKER72_DockerStatusClassification is criterion A3: `failed` yields
// the degraded status string, a healthy `active` leaves the status
// byte-identical, and every unprovable answer claims nothing.
func TestDFBUNKER72_DockerStatusClassification(t *testing.T) {
	const (
		agentID = "df72health"
		uid     = 61009
	)
	runtimeBase := t.TempDir()
	swapStringSeam(t, &userRuntimeBaseDir, runtimeBase)

	cases := []struct {
		name        string
		state       string
		probeErr    error
		wantStatus  string
		wantProbed  bool
		description string
	}{
		{
			name:        "failed unit degrades the status",
			state:       "failed\n",
			probeErr:    errors.New("exit status 3"), // is-active reports non-active states by exiting non-zero
			wantStatus:  "running (docker degraded)",
			wantProbed:  true,
			description: "the dogfood-21 answer: the installer's docker.service restart counter exhausted, unit failed",
		},
		{
			name:        "active unit leaves the status untouched",
			state:       "active\n",
			probeErr:    nil,
			wantStatus:  "running",
			wantProbed:  true,
			description: "healthy path must be zero-delta",
		},
		{
			name:        "inactive unit claims nothing",
			state:       "inactive\n",
			probeErr:    errors.New("exit status 3"),
			wantStatus:  "running",
			wantProbed:  true,
			description: "a stopped unit is not proof of a broken daemon — never upgraded to a verdict",
		},
		{
			name:        "activating unit claims nothing",
			state:       "activating\n",
			probeErr:    nil,
			wantStatus:  "running",
			wantProbed:  true,
			description: "a unit coming up is transient",
		},
		{
			name:        "dead session bus claims nothing",
			state:       "",
			probeErr:    errors.New("Failed to connect to bus: No medium found"),
			wantStatus:  "running",
			wantProbed:  true,
			description: "transport failure: no verdict (the info path must not be slowed or broken by it)",
		},
		{
			name:        "unit not found claims nothing",
			state:       "inactive",
			probeErr:    errors.New("exit status 4"),
			wantStatus:  "running",
			wantProbed:  true,
			description: "a host without the unit installed is not a degraded agent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := newLifecycleManager(t)
			dfb72HealthSwapUser(t, agentID, uid)
			call := dfb72HealthSwapProbe(t, tc.state, tc.probeErr)

			got := m.DockerStatus(agentID, "running")
			if got != tc.wantStatus {
				t.Errorf("DockerStatus(%q) = %q, want %q (%s)", tc.state, got, tc.wantStatus, tc.description)
			}
			if !tc.wantProbed {
				return
			}
			if call.calls != 1 {
				t.Fatalf("probe calls = %d, want exactly 1", call.calls)
			}
			if call.username != "bunker-"+agentID {
				t.Errorf("probe username = %q, want %q", call.username, "bunker-"+agentID)
			}
			wantDir := filepath.Join(runtimeBase, strconv.Itoa(uid))
			if call.runtimeDir != wantDir {
				t.Errorf("probe runtime dir = %q, want %q (the probe must speak to the agent's OWN manager bus)", call.runtimeDir, wantDir)
			}
		})
	}
}

// TestDFBUNKER72_DockerStatusHealthyIsByteIdentical pins the zero-delta law on
// the healthy path for statuses the daemon actually stores, including the empty
// one: a record with no status must never be upgraded to a fabricated
// "running".
func TestDFBUNKER72_DockerStatusHealthyIsByteIdentical(t *testing.T) {
	const agentID = "df72zero"
	swapStringSeam(t, &userRuntimeBaseDir, t.TempDir())
	m, _, _ := newLifecycleManager(t)
	dfb72HealthSwapUser(t, agentID, 61010)
	dfb72HealthSwapProbe(t, "active\n", nil)

	for _, status := range []string{"running", "stopped", "", "home_retained"} {
		if got := m.DockerStatus(agentID, status); got != status {
			t.Errorf("healthy DockerStatus(%q) = %q, want byte-identical", status, got)
		}
	}
}

// TestDFBUNKER72_DockerStatusWithoutAUserClaimsNothing pins that an agent whose
// user record is gone gets no docker verdict — that state belongs to the
// orphan probe (DF-BUNKER-34), and the docker probe must not double-report it.
func TestDFBUNKER72_DockerStatusWithoutAUserClaimsNothing(t *testing.T) {
	const agentID = "df72gone"
	m, _, _ := newLifecycleManager(t)
	dfb72HealthSwapUser(t, agentID, -1) // never resolves
	call := dfb72HealthSwapProbe(t, "failed\n", errors.New("exit status 3"))

	if got := m.DockerStatus(agentID, "running"); got != "running" {
		t.Errorf("DockerStatus without a user = %q, want running (no docker claim)", got)
	}
	if call.calls != 0 {
		t.Errorf("probe calls = %d, want 0 (nothing to ask without a uid)", call.calls)
	}
	if detail := m.DockerDegradedDetail(agentID); detail != "" {
		t.Errorf("DockerDegradedDetail without a user = %q, want empty", detail)
	}
}

// TestDFBUNKER72_DockerDegradedDetailNamesTheUnit pins the cause string the
// operator sees, so a future rename of the qualifier cannot silently drop the
// reason.
func TestDFBUNKER72_DockerDegradedDetailNamesTheUnit(t *testing.T) {
	const agentID = "df72detail"
	swapStringSeam(t, &userRuntimeBaseDir, t.TempDir())
	m, _, _ := newLifecycleManager(t)
	dfb72HealthSwapUser(t, agentID, 61011)
	dfb72HealthSwapProbe(t, "failed\n", errors.New("exit status 3"))

	detail := m.DockerDegradedDetail(agentID)
	if !strings.Contains(detail, dockerServiceUnitName) || !strings.Contains(detail, dockerUnitFailed) {
		t.Fatalf("DockerDegradedDetail = %q, want it to name %s and its %s state", detail, dockerServiceUnitName, dockerUnitFailed)
	}
	if !strings.Contains(DockerDegradedQualifier, "docker") {
		t.Fatalf("DockerDegradedQualifier = %q, want a docker-scoped qualifier", DockerDegradedQualifier)
	}
}

// TestDFBUNKER72_StatusComposition covers the renderer's edge cases directly:
// an empty base status must not gain a fabricated "running".
func TestDFBUNKER72_StatusComposition(t *testing.T) {
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
