package cli

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// TestStageFromSpawnErrorText: the stage is extracted ONLY from the daemon's
// own stage-carrying failure wording. Free text that happens to contain a
// stage-like word is never mined for one.
func TestStageFromSpawnErrorText(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "daemon stage failure",
			err:  errors.New(`spawn probe failed at stage rootless-install: install rootless docker for bunker-probe: exit status 1`),
			want: agent.StageRootlessInstall,
		},
		{
			name: "daemon stage failure with rollback tail",
			err:  errors.New(`spawn probe failed at stage image-build: build failed (rolled back: userdel ok)`),
			want: agent.StageImageBuild,
		},
		{
			name: "wrapped by the reconcile probe",
			err:  errors.New(`spawn agent: spawn probe failed at stage session-probe: ssh probe timed out; agent probe was NOT found on re-list after 5 attempts`),
			want: agent.StageSessionProbe,
		},
		{
			name: "request-timeout wording without a stage",
			err:  errors.New(`spawn probe failed: spawn was cancelled by the server request timeout (request_timeout, default 300s)`),
			want: "",
		},
		{
			name: "bare deadline",
			err:  connect.NewError(connect.CodeDeadlineExceeded, errors.New("context deadline exceeded")),
			want: "",
		},
		{
			name: "stage word in prose is not a stage",
			err:  errors.New("image-build failed: the image-build stage was skipped"),
			want: "",
		},
		{name: "nil", err: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stageFromSpawnErrorText(tc.err); got != tc.want {
				t.Errorf("stageFromSpawnErrorText(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestSelectSpawnProgress: with a known id the match is exact; an auto-ID spawn
// is attributed only when EXACTLY ONE in-flight entry is within skew. Several
// candidates must yield "unknown", never a guess built from another spawn.
func TestSelectSpawnProgress(t *testing.T) {
	entry := func(id, stage string, elapsed time.Duration) *v1.SpawnProgress {
		return &v1.SpawnProgress{AgentId: id, Stage: stage, ElapsedSeconds: uint64(elapsed / time.Second)}
	}
	cases := []struct {
		name    string
		entries []*v1.SpawnProgress
		agentID string
		elapsed time.Duration
		want    string
		wantOK  bool
	}{
		{
			name:    "exact id",
			entries: []*v1.SpawnProgress{entry("probe-a", agent.StageRootlessInstall, 60*time.Second), entry("probe-b", agent.StageKeygen, 5*time.Second)},
			agentID: "probe-a",
			elapsed: 60 * time.Second,
			want:    agent.StageRootlessInstall,
			wantOK:  true,
		},
		{
			name:    "known id absent from the report",
			entries: []*v1.SpawnProgress{entry("probe-b", agent.StageKeygen, 5*time.Second)},
			agentID: "probe-a",
			elapsed: 5 * time.Second,
			want:    "",
			wantOK:  false,
		},
		{
			name:    "auto id, single candidate",
			entries: []*v1.SpawnProgress{entry("probe-b", agent.StageImageBuild, 120*time.Second)},
			agentID: "",
			elapsed: 121 * time.Second,
			want:    agent.StageImageBuild,
			wantOK:  true,
		},
		{
			name:    "auto id, two candidates is ambiguous",
			entries: []*v1.SpawnProgress{entry("probe-b", agent.StageImageBuild, 120*time.Second), entry("probe-c", agent.StageDockerdStart, 121*time.Second)},
			agentID: "",
			elapsed: 121 * time.Second,
			want:    "",
			wantOK:  false,
		},
		{
			name:    "auto id, candidate outside skew is another spawn",
			entries: []*v1.SpawnProgress{entry("probe-b", agent.StageImageBuild, 900*time.Second)},
			agentID: "",
			elapsed: 30 * time.Second,
			want:    "",
			wantOK:  false,
		},
		{
			name:    "empty report",
			entries: nil,
			agentID: "",
			elapsed: 30 * time.Second,
			want:    "",
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stage, ok := selectSpawnProgress(tc.entries, tc.agentID, tc.elapsed)
			if stage != tc.want || ok != tc.wantOK {
				t.Errorf("selectSpawnProgress = (%q, %v), want (%q, %v)", stage, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestSpawnFailureReport is the message contract (DOGFOOD-015 acceptance 1):
// stage + elapsed + budget + how to raise it, and an explicit "not reported by
// daemon" when the daemon said nothing. Never a bare "context deadline
// exceeded".
func TestSpawnFailureReport(t *testing.T) {
	t.Run("stage from the daemon failure report", func(t *testing.T) {
		run := &spawnRun{agentID: "probe", started: time.Now().Add(-5 * time.Minute), budget: 20 * time.Minute}
		msg := run.failureReport(errors.New(`spawn probe failed at stage rootless-install: install failed`))
		for _, want := range []string{
			`spawn agent "probe" failed after 5m00s`,
			"stage: rootless-install (" + stageSourceFailure + ")",
			"elapsed: 5m00s",
			"daemon spawn budget 20m00s",
			"client deadline " + shortDuration(config.SpawnClientTimeout(20*time.Minute)),
			"raise it: server.spawn_request_timeout",
			"cause: spawn probe failed at stage rootless-install",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("report missing %q, got:\n%s", want, msg)
			}
		}
		if strings.Contains(msg, "exceeded") {
			t.Errorf("5m of a 20m budget must not claim the budget was exceeded, got:\n%s", msg)
		}
	})

	t.Run("stage from in-flight progress when the failure carries none", func(t *testing.T) {
		watch := &spawnProgressWatch{stage: agent.StageImageBuild, out: func(string) {}}
		run := &spawnRun{agentID: "probe", started: time.Now().Add(-6 * time.Minute), budget: 20 * time.Minute, watch: watch}
		msg := run.failureReport(connect.NewError(connect.CodeDeadlineExceeded, errors.New("context deadline exceeded")))
		if !strings.Contains(msg, "stage: image-build ("+stageSourceProgress+")") {
			t.Errorf("report does not carry the last daemon-reported stage, got:\n%s", msg)
		}
	})

	t.Run("no stage reported anywhere", func(t *testing.T) {
		run := &spawnRun{agentID: "probe", started: time.Now().Add(-30 * time.Second), budget: 20 * time.Minute, watch: &spawnProgressWatch{out: func(string) {}}}
		msg := run.failureReport(errors.New("spawn agent: connection reset"))
		if !strings.Contains(msg, "stage: "+stageSourceNone) {
			t.Errorf("report must say the daemon reported no stage, got:\n%s", msg)
		}
		if strings.Contains(msg, "daemon stage:") {
			t.Errorf("report fabricated a stage, got:\n%s", msg)
		}
		if !strings.Contains(msg, "failed after 30s") {
			t.Errorf("report missing elapsed, got:\n%s", msg)
		}
	})

	t.Run("pre-capability daemon has no reported budget", func(t *testing.T) {
		run := &spawnRun{agentID: "probe", started: time.Now().Add(-2 * time.Minute)}
		msg := run.failureReport(errors.New("spawn agent: context deadline exceeded"))
		if !strings.Contains(msg, "budget: not reported by the daemon") {
			t.Errorf("report must say the budget was not reported, got:\n%s", msg)
		}
		if !strings.Contains(msg, shortDuration(config.SpawnClientTimeout(0))) {
			t.Errorf("report must still state the client deadline in force, got:\n%s", msg)
		}
	})

	t.Run("auto-ID spawn names itself honestly", func(t *testing.T) {
		run := &spawnRun{started: time.Now().Add(-time.Minute), budget: 20 * time.Minute}
		msg := run.failureReport(errors.New("spawn agent: context deadline exceeded"))
		if !strings.Contains(msg, "spawn a new (auto-ID) agent failed after 1m00s") {
			t.Errorf("auto-ID subject wrong, got:\n%s", msg)
		}
	})

	t.Run("elapsed past the daemon budget says so", func(t *testing.T) {
		run := &spawnRun{agentID: "probe", started: time.Now().Add(-21 * time.Minute), budget: 20 * time.Minute}
		msg := run.failureReport(errors.New("spawn agent: context deadline exceeded"))
		if !strings.Contains(msg, "the daemon's own budget was exceeded") {
			t.Errorf("report should state the budget was exceeded, got:\n%s", msg)
		}
	})
}

// TestSpawnProgressWatch_Heartbeat drives the real watcher against a live mock
// daemon: a stage the daemon reports reaches the heartbeat line, and Stop()
// always returns. It also proves the poll actually happened (calls > 0) rather
// than assuming a line implies a poll.
func TestSpawnProgressWatch_Heartbeat(t *testing.T) {
	defer func(old time.Duration) { spawnProgressInterval = old }(spawnProgressInterval)
	spawnProgressInterval = 10 * time.Millisecond

	t.Run("daemon stage is printed", func(t *testing.T) {
		mock := &mockBunkerdServer{spawnProgress: []*v1.SpawnProgress{
			{AgentId: "probe", Stage: agent.StageRootlessInstall, ElapsedSeconds: 0},
		}}
		srv := newTestServer(t, mock)
		defer srv.Close()
		client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)

		lines := &lineRecorder{}
		w := startSpawnProgressWatch(client, "probe", time.Now(), lines.add)
		time.Sleep(60 * time.Millisecond)
		w.Stop()

		if mock.spawnProgressCalls.Load() == 0 {
			t.Fatal("the heartbeat never polled GetSpawnProgress")
		}
		got := lines.joined()
		if !strings.Contains(got, "still creating agent...") {
			t.Errorf("no heartbeat line printed, got:\n%s", got)
		}
		if !strings.Contains(got, "daemon stage: "+agent.StageRootlessInstall) {
			t.Errorf("heartbeat does not name the daemon stage, got:\n%s", got)
		}
		if w.LastStage() != agent.StageRootlessInstall {
			t.Errorf("LastStage = %q, want %q", w.LastStage(), agent.StageRootlessInstall)
		}
	})

	t.Run("auto-ID spawn attributes by elapsed", func(t *testing.T) {
		mock := &mockBunkerdServer{spawnProgress: []*v1.SpawnProgress{
			{AgentId: "generated-id", Stage: agent.StageImageBuild, ElapsedSeconds: 0},
		}}
		srv := newTestServer(t, mock)
		defer srv.Close()
		client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)

		lines := &lineRecorder{}
		w := startSpawnProgressWatch(client, "", time.Now(), lines.add)
		time.Sleep(60 * time.Millisecond)
		w.Stop()
		if !strings.Contains(lines.joined(), "daemon stage: "+agent.StageImageBuild) {
			t.Errorf("auto-ID heartbeat did not attribute the entry, got:\n%s", lines.joined())
		}
	})

	t.Run("pre-capability daemon still heartbeats", func(t *testing.T) {
		mock := &mockBunkerdServer{spawnProgressErr: connect.NewError(connect.CodeUnimplemented, errors.New("no such procedure"))}
		srv := newTestServer(t, mock)
		defer srv.Close()
		client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)

		lines := &lineRecorder{}
		w := startSpawnProgressWatch(client, "probe", time.Now(), lines.add)
		time.Sleep(60 * time.Millisecond)
		w.Stop()
		got := lines.joined()
		if !strings.Contains(got, "still creating agent...") {
			t.Errorf("no elapsed-only heartbeat printed, got:\n%s", got)
		}
		if strings.Contains(got, "daemon stage:") {
			t.Errorf("heartbeat claimed a stage from an unsupported daemon, got:\n%s", got)
		}
	})

	t.Run("no in-flight entry means no stage claim", func(t *testing.T) {
		mock := &mockBunkerdServer{}
		srv := newTestServer(t, mock)
		defer srv.Close()
		client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)

		lines := &lineRecorder{}
		w := startSpawnProgressWatch(client, "probe", time.Now(), lines.add)
		time.Sleep(60 * time.Millisecond)
		w.Stop()
		got := lines.joined()
		if !strings.Contains(got, "still creating agent...") || strings.Contains(got, "daemon stage:") {
			t.Errorf("empty report must yield an elapsed-only line, got:\n%s", got)
		}
	})
}

// TestSpawnProgressWatch_StopIsIdempotent: a spawn that fails before the RPC
// (and a second Stop on a recovery path) must not deadlock.
func TestSpawnProgressWatch_StopIsIdempotent(t *testing.T) {
	mock := &mockBunkerdServer{}
	srv := newTestServer(t, mock)
	defer srv.Close()
	client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
	w := startSpawnProgressWatch(client, "probe", time.Now(), func(string) {})
	w.Stop()
	done := make(chan struct{})
	go func() { w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second Stop deadlocked")
	}
}

// TestShortDuration pins the operator-facing rendering used by every progress
// and failure line.
func TestShortDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{500 * time.Millisecond, "1s"},
		{45 * time.Second, "45s"},
		{59*time.Second + 400*time.Millisecond, "59s"},
		{time.Minute, "1m00s"},
		{5*time.Minute + 1*time.Second, "5m01s"},
		{20 * time.Minute, "20m00s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h02m03s"},
		{-time.Second, "0s"},
	}
	for _, tc := range cases {
		if got := shortDuration(tc.in); got != tc.want {
			t.Errorf("shortDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// lineRecorder is a small concurrency-safe line sink for the watcher tests.
type lineRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *lineRecorder) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
}

func (r *lineRecorder) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// TestSpawnCommand_FailureNamesStageElapsedBudget is the end-to-end acceptance
// for DOGFOOD-015 acceptance 1: a spawn that fails on the daemon yields a
// terminal message naming the stage, the elapsed time and the budget — never a
// bare "context deadline exceeded". The mock returns the daemon's own
// stage-carrying failure, so the stage in the message is one the daemon really
// reported.
func TestSpawnCommand_FailureNamesStageElapsedBudget(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockSpawnServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{
				Hostname:                   "bunker-fail",
				Version:                    "v0.2.0",
				SpawnRequestTimeoutSeconds: 1200,
			},
		},
		// A refusal-class failure: no reconcile probe, so the message under
		// test is the failure report itself.
		spawnErr: connect.NewError(connect.CodeInternal,
			errors.New("spawn probe failed at stage image-build: build failed: exit status 1")),
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()
	writeSpawnTestConfig(t, tmpDir, srv.URL)

	var execErr error
	output := captureStdout(t, func() {
		cmd := NewSpawnCommand()
		cmd.SetArgs([]string{"probe"})
		execErr = cmd.Execute()
	})
	if execErr == nil {
		t.Fatal("expected the spawn to fail")
	}
	msg := execErr.Error()
	for _, want := range []string{
		`spawn agent "probe" failed after`,
		"stage: " + agent.StageImageBuild + " (" + stageSourceFailure + ")",
		"budget: daemon spawn budget 20m00s",
		"raise it: server.spawn_request_timeout",
		"cause: internal: spawn probe failed at stage image-build",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message missing %q, got:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "stage not reported by daemon") {
		t.Errorf("daemon reported a stage but the message denies it, got:\n%s", msg)
	}
	// The in-flight line still had to appear (progress while waiting).
	if !strings.Contains(output, "Creating agent...") {
		t.Errorf("startup progress line missing, got:\n%s", output)
	}
	if !strings.Contains(output, "spawn budget: 20m00s on the daemon") {
		t.Errorf("the budget must be stated before the wait, got:\n%s", output)
	}
}

// TestSpawnCommand_HeartbeatWhileSpawning is acceptance 2 at the COMMAND
// level: while a spawn is in flight the CLI prints at least one extra line, and
// when the daemon reports a stage the line names it. The mock holds SpawnAgent
// open for a few heartbeats, so the assertion cannot be satisfied by the
// post-completion output.
func TestSpawnCommand_HeartbeatWhileSpawning(t *testing.T) {
	defer func(old time.Duration) { spawnProgressInterval = old }(spawnProgressInterval)
	spawnProgressInterval = 10 * time.Millisecond

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockSpawnServer{
		mockBunkerdServer: mockBunkerdServer{
			info: &v1.ServerInfoResponse{Hostname: "bunker-hb", Version: "v0.2.0"},
			spawnProgress: []*v1.SpawnProgress{
				{AgentId: "probe", Stage: agent.StageRootlessInstall, ElapsedSeconds: 0},
			},
		},
		spawnResp:  &v1.SpawnAgentResponse{AgentId: "probe"},
		spawnDelay: 120 * time.Millisecond,
	}
	srv := newSpawnTestServer(t, mock)
	defer srv.Close()
	writeSpawnTestConfig(t, tmpDir, srv.URL)

	output := captureStdout(t, func() {
		cmd := NewSpawnCommand()
		cmd.SetArgs([]string{"probe"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if !strings.Contains(output, "still creating agent...") {
		t.Fatalf("no in-flight heartbeat while spawning, got:\n%s", output)
	}
	if !strings.Contains(output, "daemon stage: "+agent.StageRootlessInstall) {
		t.Errorf("heartbeat does not name the reported stage, got:\n%s", output)
	}
	if !strings.Contains(output, "created in ") {
		t.Errorf("missing completion line after a long spawn, got:\n%s", output)
	}
	// The agent id still prints as it did before.
	if !strings.Contains(output, "probe") {
		t.Errorf("output missing agent id, got:\n%s", output)
	}
}

// TestSpawnCommand_DerivesTransportTimeout pins the OTHER half of the client
// deadline. The context deadline alone is not enough: newBunkerdClient builds
// an http.Client with a flat 300s transport timeout, and a spawn that took
// 5m13s on a cold host died there — "Client.Timeout exceeded while awaiting
// headers" — while the derived context still had 18 minutes left. So the spawn
// path must build its client with the derived deadline as the transport
// timeout (measured live on bunker-las-03, 2026-11-15).
func TestSpawnCommand_DerivesTransportTimeout(t *testing.T) {
	src, err := os.ReadFile("spawn.go")
	if err != nil {
		t.Fatalf("read spawn.go: %v", err)
	}
	body := string(src)
	if strings.Contains(body, "client := newBunkerdClient(entry)") {
		t.Error("spawn builds its client with newBunkerdClient (flat http.Client timeout) — a long spawn dies at that bound regardless of the context deadline")
	}
	if !strings.Contains(body, "newBunkerdClientChecked(entry, clientDeadline)") {
		t.Error("spawn does not derive its transport timeout from the spawn deadline")
	}
}
