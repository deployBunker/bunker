package cli

// DF-BUNKER-81 criterion 1 and 4, client side: the destroy deadline is derived
// from the agent's measured home size (never a fixed literal that a large
// archive can race), the operator sees the size and the deadline before the
// request is sent, and the archive opt-out (--archive=false / --purge) reaches
// the wire as the request's skip_archive field.

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/deployBunker/bunker/internal/config"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// destroySizeMockServer records the DestroyAgent request and answers
// AgentMetrics with a scripted per-agent disk usage (the daemon serves that
// number from its TTL snapshot cache, and the CLI uses it to size the
// deadline).
type destroySizeMockServer struct {
	mockBunkerdServer
	destroyReq   *v1.DestroyAgentRequest
	metricsBytes uint64
	metricsErr   error
	metricsCalls int
}

func (m *destroySizeMockServer) DestroyAgent(
	ctx context.Context,
	req *connect.Request[v1.DestroyAgentRequest],
) (*connect.Response[v1.DestroyAgentResponse], error) {
	m.destroyReq = req.Msg
	return connect.NewResponse(&v1.DestroyAgentResponse{AgentId: req.Msg.GetAgentId(), Status: "destroyed"}), nil
}

func (m *destroySizeMockServer) AgentMetrics(
	ctx context.Context,
	req *connect.Request[v1.AgentMetricsRequest],
) (*connect.Response[v1.AgentMetricsResponse], error) {
	m.metricsCalls++
	if m.metricsErr != nil {
		return nil, m.metricsErr
	}
	return connect.NewResponse(&v1.AgentMetricsResponse{
		AgentId:       req.Msg.GetAgentId(),
		DiskUsedBytes: m.metricsBytes,
	}), nil
}

// newDestroySizeServer wires the mock into a CLI config the command can load.
func newDestroySizeServer(t *testing.T, mock *destroySizeMockServer) {
	t.Helper()
	t.Setenv(SessionTargetEnvVar, "default")
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	srv := newDestroyTestServer(t, mock)
	t.Cleanup(srv.Close)
	writeDestroyTestConfig(t, tmpDir, srv.URL)
}

// TestDestroyDeadlineForHomeSize pins the derivation and the invariant the
// defect violated: no home size may produce a deadline short enough to
// SIGKILL a legitimate archive (the pre-fix 30s literal did exactly that).
func TestDestroyDeadlineForHomeSize(t *testing.T) {
	const mib = uint64(1) << 20

	tests := []struct {
		name      string
		homeBytes uint64
		want      time.Duration
	}{
		{"unknown size keeps the configured floor", 0, destroyRequestTimeout},
		{"the 688M rootless-docker home from the incident", 688 * mib, config.DestroyRequestTimeoutForHomeSize(int64(688 * mib))},
		{"4 GiB home", 4 << 30, config.DestroyRequestTimeoutForHomeSize(4 << 30)},
		{"50 GiB home", 50 << 30, config.DestroyRequestTimeoutForHomeSize(50 << 30)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := destroyDeadlineForHomeSize(tt.homeBytes)
			if got != tt.want {
				t.Errorf("destroyDeadlineForHomeSize(%d) = %s, want %s", tt.homeBytes, got, tt.want)
			}
			if got < destroyRequestTimeout {
				t.Errorf("deadline %s is shorter than the configured floor %s", got, destroyRequestTimeout)
			}
			if got < config.DefaultServerRequestTimeout {
				t.Errorf("deadline %s is shorter than the daemon request budget %s", got, config.DefaultServerRequestTimeout)
			}
			if tt.homeBytes > 0 {
				if archive := config.ArchiveBudgetForHomeSize(int64(tt.homeBytes)); got <= archive {
					t.Errorf("deadline %s does not outlive the daemon's archive budget %s for the same home", got, archive)
				}
			}
		})
	}

	// The regression shape, stated directly: the OLD 30s literal is
	// unreachable for every size, and even a small home gets minutes.
	for _, size := range []uint64{1, 1 << 20, math.MaxUint32} {
		if got := destroyDeadlineForHomeSize(size); got <= 30*time.Second {
			t.Errorf("destroyDeadlineForHomeSize(%d) = %s — a 30s-class deadline is the defect (the archive is SIGKILLed mid-stream)", size, got)
		}
	}
}

// TestDestroyProgressLine pins what the operator sees before waiting: the
// measured size, whether an archive happens, and the deadline.
func TestDestroyProgressLine(t *testing.T) {
	const mib = uint64(1) << 20
	tests := []struct {
		name        string
		homeBytes   uint64
		skipArchive bool
		want        []string
		notWant     []string
	}{
		{
			name:      "size known, archiving",
			homeBytes: 688 * mib,
			want:      []string{"Destroying agent abc12345", "home 688.0 MB", "archiving before delete", "deadline"},
		},
		{
			name:      "size unknown still states the deadline",
			homeBytes: 0,
			want:      []string{"home size unknown", "archiving before delete", "deadline"},
		},
		{
			name:        "archive skipped is stated as such",
			homeBytes:   688 * mib,
			skipArchive: true,
			want:        []string{"home 688.0 MB", "archive skipped", "NO copy", "deadline"},
			notWant:     []string{"archiving before delete"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := destroyProgressLine("abc12345", tt.homeBytes, 12*time.Minute, tt.skipArchive)
			for _, want := range tt.want {
				if !strings.Contains(line, want) {
					t.Errorf("progress line %q is missing %q", line, want)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(line, notWant) {
					t.Errorf("progress line %q must not contain %q", line, notWant)
				}
			}
			// The progress line must not leak the raw command vocabulary the
			// not-found path is pinned to avoid.
			for _, leak := range []string{"userdel", "exit status", "destroy agent:"} {
				if strings.Contains(line, leak) {
					t.Errorf("progress line %q must not contain %q", line, leak)
				}
			}
		})
	}
}

// TestDestroyCommand_ArchiveChoicePlumbing is criterion 4 end to end on the
// client: the flag choice reaches the wire as skip_archive, the default leaves
// it false, and the contradictory combination is refused before any RPC.
func TestDestroyCommand_ArchiveChoicePlumbing(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantSkip   bool
		wantErr    bool
		wantNoCall bool
	}{
		{name: "default archives", args: nil, wantSkip: false},
		{name: "--archive=true archives", args: []string{"--archive=true"}, wantSkip: false},
		{name: "--archive=false skips", args: []string{"--archive=false"}, wantSkip: true},
		{name: "--purge skips", args: []string{"--purge"}, wantSkip: true},
		{name: "both spellings agree", args: []string{"--archive=false", "--purge"}, wantSkip: true},
		{name: "contradiction is refused", args: []string{"--purge", "--archive=true"}, wantErr: true, wantNoCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &destroySizeMockServer{}
			newDestroySizeServer(t, mock)

			cmd := NewDestroyCommand()
			cmd.SetArgs(append([]string{"abc12345"}, tt.args...))
			var execErr error
			captureStdout(t, func() { execErr = cmd.Execute() })

			if tt.wantErr {
				if execErr == nil {
					t.Fatal("expected the contradictory flag combination to be refused")
				}
				if !strings.Contains(execErr.Error(), "contradict") {
					t.Errorf("error %v does not explain the contradiction", execErr)
				}
			} else if execErr != nil {
				t.Fatalf("Execute: %v", execErr)
			}
			if tt.wantNoCall {
				if mock.destroyReq != nil {
					t.Errorf("a refused flag combination must not reach the daemon; request was %+v", mock.destroyReq)
				}
				return
			}
			if mock.destroyReq == nil {
				t.Fatal("no DestroyAgent request reached the daemon")
			}
			if got := mock.destroyReq.GetSkipArchive(); got != tt.wantSkip {
				t.Errorf("DestroyAgentRequest.skip_archive = %v, want %v", got, tt.wantSkip)
			}
		})
	}
}

// TestDestroyCommand_PrintsHomeSize proves the size probe is wired: a daemon
// that reports the home's footprint gives the operator a sized deadline line,
// and a daemon that cannot answer (or has no footprint) degrades to the
// size-unknown floor WITHOUT failing the destroy.
func TestDestroyCommand_PrintsHomeSize(t *testing.T) {
	const mib = uint64(1) << 20

	t.Run("size reported by the daemon", func(t *testing.T) {
		mock := &destroySizeMockServer{metricsBytes: 688 * mib}
		newDestroySizeServer(t, mock)

		cmd := NewDestroyCommand()
		cmd.SetArgs([]string{"abc12345"})
		var execErr error
		output := captureStdout(t, func() { execErr = cmd.Execute() })
		if execErr != nil {
			t.Fatalf("Execute: %v", execErr)
		}
		if mock.metricsCalls != 1 {
			t.Errorf("AgentMetrics probe calls = %d, want 1", mock.metricsCalls)
		}
		for _, want := range []string{"home 688.0 MB", "deadline"} {
			if !strings.Contains(output, want) {
				t.Errorf("output is missing %q, got:\n%s", want, output)
			}
		}
		if !strings.Contains(output, "abc12345 destroyed") {
			t.Errorf("destroy outcome missing from output:\n%s", output)
		}
	})

	t.Run("size probe unavailable degrades to the floor", func(t *testing.T) {
		// metricsErr makes the probe fail: the destroy must still run, with
		// the size-unknown deadline.
		mock := &destroySizeMockServer{metricsErr: connect.NewError(connect.CodeUnimplemented, nil)}
		newDestroySizeServer(t, mock)

		cmd := NewDestroyCommand()
		cmd.SetArgs([]string{"abc12345"})
		var execErr error
		output := captureStdout(t, func() { execErr = cmd.Execute() })
		if execErr != nil {
			t.Fatalf("a failed size probe must not fail the destroy: %v", execErr)
		}
		if !strings.Contains(output, "home size unknown") {
			t.Errorf("output should state the size is unknown, got:\n%s", output)
		}
		if !strings.Contains(output, "abc12345 destroyed") {
			t.Errorf("destroy outcome missing from output:\n%s", output)
		}
	})
}

// TestDestroyCommand_HelpDocumentsArchiveOptOut is criterion 4's help
// surface: the operator must be able to discover that the archive can be
// skipped, and that skipping it keeps NO copy.
func TestDestroyCommand_HelpDocumentsArchiveOptOut(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewDestroyCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--server", "default", "--help"})
		_ = cmd.Execute()
	})

	for _, want := range []string{"--archive", "--purge", "--archive=false", "NO copy", ".local/share/docker"} {
		if !strings.Contains(output, want) {
			t.Errorf("destroy --help is missing %q; got:\n%s", want, output)
		}
	}
	if !strings.Contains(strings.ToLower(output), "size") {
		t.Errorf("destroy --help should explain that the deadline is sized from the home:\n%s", output)
	}
}
