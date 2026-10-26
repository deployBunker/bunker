package cli

import (
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── GAP-121: resource-events CLI surface ────────────────────────────────────

func TestResourceEventsCommand_Help(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewResourceEventsCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		_ = cmd.Execute()
	})
	for _, want := range []string{"resource-events", "memory.events", "pids.events", "peak"} {
		if !strings.Contains(output, want) {
			t.Errorf("help output missing %q:\n%s", want, output)
		}
	}
}

func TestResourceEventsCommand_RequiresAgentID(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewResourceEventsCommand()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("missing <agent-id> was accepted")
	}
}

func TestResourceEventsCommand_NoServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(TLSInsecureAckEnv, "1")

	cmd := NewResourceEventsCommand()
	cmd.SetArgs([]string{"agent-1"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error with no server configured")
	}
	if !strings.Contains(err.Error(), "no active server") {
		t.Fatalf("expected 'no active server', got: %v", err)
	}
}

// TestResourceEventsCommand_EndToEnd drives the command against an httptest
// server and asserts the rendered surface: the counters, the peak with its
// source, and the ceiling verdict.
func TestResourceEventsCommand_EndToEnd(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(TLSInsecureAckEnv, "1")

	mock := &metricsMockServer{
		agentMetrics: &v1.AgentMetricsResponse{
			AgentId:           "agent-1",
			Status:            "running",
			MemoryLimitBytes:  1024 * 1024 * 1024,
			MemoryUsedBytes:   512 * 1024 * 1024,
			HostLevelFallback: false,
			ResourceEvents: &v1.ResourceEvents{
				Memory: &v1.MemoryEvents{
					Max: 3, High: 4, Low: 0, Oom: 1, OomKill: 1, OomGroupKill: 0,
					Available: true,
				},
				Pids:               &v1.PidsEvents{Max: 7, Available: true},
				MemoryCurrentBytes: 512 * 1024 * 1024,
				PidsCurrent:        23,
				MemoryPeakBytes:    768 * 1024 * 1024,
				PidsPeak:           99,
				MemoryPeakSource:   "cgroup",
				PidsPeakSource:     "daemon",
				CgroupPath:         "/sys/fs/cgroup/user.slice/user-1001.slice",
				Available:          true,
			},
		},
	}
	ts := newMetricsTestServer(t, mock)
	defer ts.Close()

	// Register with a token so the GAP-181 fail-closed token check is
	// satisfied the way a real operator's config satisfies it.
	if err := RegisterServer("test", ts.URL, "test-token", true); err != nil {
		t.Fatalf("register server: %v", err)
	}

	cmd := NewResourceEventsCommand()
	cmd.SetArgs([]string{"agent-1", "--server", "test"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	for _, want := range []string{
		"Agent Resource Events: agent-1",
		"memory.events",
		"pids.events",
		"/sys/fs/cgroup/user.slice/user-1001.slice",
		"memory.max reached ×3",
		"OOM kill ×1",
		"pids.max reached ×7",
		"cgroup memory.peak",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}
	// The daemon-sampled peak must be labelled as such, never presented as the
	// kernel's high-water mark.
	if !strings.Contains(output, "LOWER BOUND") {
		t.Errorf("daemon-sourced pids peak is not labelled a lower bound:\n%s", output)
	}
}

// TestFormatResourceEvents_NotReportedByOlderDaemon pins the pre-GAP-121
// daemon contract: no resource_events at all must render as "not reported",
// never as a clean or all-zero reading.
func TestFormatResourceEvents_NotReportedByOlderDaemon(t *testing.T) {
	out := formatResourceEvents(&v1.AgentMetricsResponse{AgentId: "agent-1", Status: "running"})
	if !strings.Contains(out, "NOT REPORTED") {
		t.Errorf("missing 'NOT REPORTED' for a nil resource_events:\n%s", out)
	}
	if strings.Contains(out, "No ceiling events recorded") {
		t.Errorf("an absent surface rendered as a clean reading:\n%s", out)
	}
}

func TestFormatResourceEvents_UnavailableIsNotClean(t *testing.T) {
	out := formatResourceEvents(&v1.AgentMetricsResponse{
		AgentId: "agent-1",
		ResourceEvents: &v1.ResourceEvents{
			Available:  false,
			Detail:     "memory.events (no such file or directory)",
			CgroupPath: "/sys/fs/cgroup/user.slice/user-1001.slice",
		},
	})
	if !strings.Contains(out, "UNAVAILABLE") {
		t.Errorf("unavailable reading was not flagged:\n%s", out)
	}
	if !strings.Contains(out, "memory.events (no such file or directory)") {
		t.Errorf("detail was not surfaced:\n%s", out)
	}
	if strings.Contains(out, "No ceiling events recorded") {
		t.Errorf("an unmeasured reading rendered as a clean verdict:\n%s", out)
	}
}

// TestFormatResourceEvents_PartialReadNamesTheUnreadCounterSet proves one
// readable counter set never vouches for the other.
func TestFormatResourceEvents_PartialReadNamesTheUnreadCounterSet(t *testing.T) {
	out := formatResourceEvents(&v1.AgentMetricsResponse{
		AgentId: "agent-1",
		ResourceEvents: &v1.ResourceEvents{
			Memory:    &v1.MemoryEvents{Max: 2, Available: true},
			Pids:      &v1.PidsEvents{Available: false},
			Available: true,
		},
	})
	if !strings.Contains(out, "pids.events could NOT be read") {
		t.Errorf("the unread counter set was not named:\n%s", out)
	}
	if !strings.Contains(out, "memory.max reached ×2") {
		t.Errorf("the measured counter set was not reported:\n%s", out)
	}
}

func TestFormatResourceEvents_CleanReading(t *testing.T) {
	out := formatResourceEvents(&v1.AgentMetricsResponse{
		AgentId: "agent-1",
		ResourceEvents: &v1.ResourceEvents{
			Memory:    &v1.MemoryEvents{Available: true},
			Pids:      &v1.PidsEvents{Available: true},
			Available: true,
		},
	})
	if !strings.Contains(out, "No ceiling events recorded in this agent's cgroup.") {
		t.Errorf("a measured all-zero reading should say so plainly:\n%s", out)
	}
	if strings.Contains(out, "UNAVAILABLE") {
		t.Errorf("a measured all-zero reading was flagged unavailable:\n%s", out)
	}
}

func TestFormatResourceEvents_NoPeakAvailable(t *testing.T) {
	out := formatResourceEvents(&v1.AgentMetricsResponse{
		AgentId: "agent-1",
		ResourceEvents: &v1.ResourceEvents{
			Memory:    &v1.MemoryEvents{Available: true},
			Pids:      &v1.PidsEvents{Available: true},
			Available: true,
		},
	})
	if !strings.Contains(out, "no peak available") {
		t.Errorf("missing 'no peak available' when neither source produced a peak:\n%s", out)
	}
}

// TestFormatResourceEvents_UnavailableMessageDoesNotListStoppedAgent pins the
// DF-BUNKER-111 correction: a stopped agent is READABLE — StopAgent keeps the
// agent user, its home directory and its cgroup slice (see
// internal/agent/manager_lifecycle.go), so its counters stay readable. The
// UNAVAILABLE message must therefore never name a stopped agent as a typical
// cause; only the old phrasing ("stopped or destroyed") did.
func TestFormatResourceEvents_UnavailableMessageDoesNotListStoppedAgent(t *testing.T) {
	out := formatResourceEvents(&v1.AgentMetricsResponse{
		AgentId: "agent-1",
		ResourceEvents: &v1.ResourceEvents{
			Available:  false,
			Detail:     "memory.events (no such file or directory)",
			CgroupPath: "/sys/fs/cgroup/user.slice/user-1001.slice",
		},
	})
	if strings.Contains(out, "stopped or destroyed") {
		t.Errorf("UNAVAILABLE message still carries the old 'stopped or destroyed' phrasing:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Typical cause") && strings.Contains(line, "stopped") {
			t.Errorf("UNAVAILABLE message lists a stopped agent among the typical causes:\n%s", out)
		}
	}
}

// TestFormatResourceEvents_UnavailableMessageListsRealCauses pins the cause
// list to what actually sets Available=false daemon-side
// (internal/resource/cgroup_events.go ReadAgentResourceEvents + the uid<0
// path in internal/server/resource_events.go): the agent user cannot be
// resolved or the user slice cannot be read — a destroyed agent or a deleted
// agent user. The message must also state the stop side of the contract: a
// merely stopped agent IS still readable.
func TestFormatResourceEvents_UnavailableMessageListsRealCauses(t *testing.T) {
	out := formatResourceEvents(&v1.AgentMetricsResponse{
		AgentId: "agent-1",
		ResourceEvents: &v1.ResourceEvents{
			Available:  false,
			Detail:     "memory.events (no such file or directory)",
			CgroupPath: "/sys/fs/cgroup/user.slice/user-1001.slice",
		},
	})
	for _, want := range []string{
		"Typical cause: a destroyed agent",
		"deleted agent",
		"no cgroup left to read",
		"stopped agent is still readable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("UNAVAILABLE message missing %q:\n%s", want, out)
		}
	}
}

// TestFormatResourceEvents_StoppedAgentStillRendersMeasured pins the readable
// side of the same contract: a stopped agent keeps its cgroup slice, so when
// the daemon answers with measured counters for a stopped agent the render is
// a normal measured reading — never the UNAVAILABLE block.
func TestFormatResourceEvents_StoppedAgentStillRendersMeasured(t *testing.T) {
	out := formatResourceEvents(&v1.AgentMetricsResponse{
		AgentId: "agent-1",
		Status:  "stopped",
		ResourceEvents: &v1.ResourceEvents{
			Memory:    &v1.MemoryEvents{Available: true},
			Pids:      &v1.PidsEvents{Available: true},
			Available: true,
		},
	})
	if !strings.Contains(out, "Status:   stopped") {
		t.Errorf("stopped status was not rendered:\n%s", out)
	}
	if strings.Contains(out, "UNAVAILABLE") {
		t.Errorf("a stopped agent's readable counters rendered as unavailable:\n%s", out)
	}
	if !strings.Contains(out, "No ceiling events recorded in this agent's cgroup.") {
		t.Errorf("a stopped agent's measured reading did not render as measured:\n%s", out)
	}
}

func TestResourceEventsBrief_MetricsLine(t *testing.T) {
	tests := []struct {
		name string
		ev   *v1.ResourceEvents
		want string
	}{
		{
			name: "clean",
			ev:   &v1.ResourceEvents{Available: true, Memory: &v1.MemoryEvents{}, Pids: &v1.PidsEvents{}},
			want: "none recorded",
		},
		{
			name: "hits",
			ev: &v1.ResourceEvents{
				Available: true,
				Memory:    &v1.MemoryEvents{Max: 2, OomKill: 1},
				Pids:      &v1.PidsEvents{Max: 5},
			},
			want: "memory.max ×2, oom_kill ×1, pids.max ×5",
		},
		{
			name: "unavailable is not 'none recorded'",
			ev:   &v1.ResourceEvents{Available: false},
			want: "UNAVAILABLE",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resourceEventsBrief(tt.ev)
			if !strings.Contains(got, tt.want) {
				t.Errorf("resourceEventsBrief = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}
