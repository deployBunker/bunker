package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// DF-BUNKER-27 regression battery.
//
// ExecAgent used to start TWO goroutines — one per pipe — that each called
// connect's stream.Send directly, plus the handler itself sending the
// containment marker / session-denial / exit-code frames after wg.Wait().
// connect's ServerStream.Send is NOT safe for concurrent use, so a command
// writing to both pipes interleaved frames inside one HTTP response: measured
// live on bunker-las-02 (v0.1.4) with a stdlib REST client, "echo OUT" was
// decodable 10/10, "echo ERR 1>&2" 10/10, and "echo OUT; echo ERR 1>&2" 0/10
// (raw bytes carried the envelope frames followed by a literal second
// "HTTP/1.1 200 OK" status line, and the daemon journal logged
// "http: superfluous response.WriteHeader call" at the same second).
//
// Everything here drives the REAL connect handler over httptest with the exec
// builder seam replaced by a local `sh -c` command, so both pipes are written
// by a real child process and the frames are parsed by a real connect client.
// The both-pipe cases are repeated so a reintroduced concurrent Send has many
// chances to interleave.
const (
	dfb27AgentID = "dfb27a"

	// dfb27Iterations is the repeat count for the both-pipe cases: the bug is
	// an interleaving race, so one pass is not evidence.
	dfb27Iterations = 20

	// dfb27HeavyLines is the line count per stream for the interleaved variant
	// (stdout and stderr alternate, a few hundred lines in total).
	dfb27HeavyLines = 200
)

// dfb27InjectExecCommand replaces every exec builder seam for this test so the
// handler runs argv locally instead of ssh. The previous builders are restored
// on cleanup; this package's other ExecAgent tests either run sequentially or
// fail before the seam is reached, so no test observes the injection.
func dfb27InjectExecCommand(t *testing.T, argv ...string) {
	t.Helper()
	prevShell, prevRaw, prevScript := execSSHCommandBuilder, execSSHRawCommandBuilder, execSSHScriptCommandBuilder
	build := func(ctx context.Context, _, _, _, _ string, _ []string, _ bool, _ string) *exec.Cmd {
		return exec.CommandContext(ctx, argv[0], argv[1:]...)
	}
	buildScript := func(ctx context.Context, _, _, _, _ string, _ bool, _ string) *exec.Cmd {
		return exec.CommandContext(ctx, argv[0], argv[1:]...)
	}
	execSSHCommandBuilder, execSSHRawCommandBuilder, execSSHScriptCommandBuilder = build, build, buildScript
	t.Cleanup(func() {
		execSSHCommandBuilder, execSSHRawCommandBuilder, execSSHScriptCommandBuilder = prevShell, prevRaw, prevScript
	})
}

// dfb27TestClient mounts the real service on a real connect handler and returns
// a client for it.
func dfb27TestClient(t *testing.T) bunkerv1connect.BunkerdClient {
	t.Helper()
	logger := testDiscardLogger()
	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{
		AgentID:           dfb27AgentID,
		Status:            "running",
		SshPrivateKeyPath: imgTestKeyPath,
	}); err != nil {
		t.Fatalf("register agent: %v", err)
	}
	svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker}

	path, handler := bunkerv1connect.NewBunkerdHandler(svc)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
}

// dfb27Run is what one ExecAgent call produced.
type dfb27Run struct {
	stdout string
	stderr string

	// frames counts every received message; exitFrame is the index of the
	// single frame that carried no output payload (the exit-code frame), or
	// -1 when absent.
	frames    int
	exitFrame int
	exitCode  int32
}

// dfb27RunExec performs one ExecAgent call and decodes every frame. The
// returned error is the stream error: it is nil only when the client decoded
// the whole response.
func dfb27RunExec(t *testing.T, client bunkerv1connect.BunkerdClient, script string) (dfb27Run, error) {
	t.Helper()
	run := dfb27Run{exitFrame: -1}

	stream, err := client.ExecAgent(context.Background(), connect.NewRequest(&v1.ExecAgentRequest{
		AgentId: dfb27AgentID,
		Command: "sh",
		Args:    []string{"-c", script},
	}))
	if err != nil {
		return run, fmt.Errorf("ExecAgent call: %w", err)
	}
	for stream.Receive() {
		msg := stream.Msg()
		switch {
		case msg.GetStdout() != nil:
			run.stdout += string(msg.GetStdout())
			run.frames++
		case msg.GetStderr() != nil:
			run.stderr += string(msg.GetStderr())
			run.frames++
		default:
			if run.exitFrame >= 0 {
				return run, fmt.Errorf("second exit-code frame at index %d (first at %d)", run.frames, run.exitFrame)
			}
			run.exitFrame = run.frames
			run.exitCode = msg.GetExitCode()
			run.frames++
		}
	}
	serr := stream.Err()
	// Best-effort: the stream is already exhausted; a Close error would be
	// reported by Err() above.
	_ = stream.Close()
	return run, serr
}

// dfb27AssertRun checks one call: no stream error, every frame decoded, both
// streams byte-complete (no loss and no duplication), the exit frame last, and
// the exit code the child actually produced.
func dfb27AssertRun(t *testing.T, label string, run dfb27Run, err error, wantStdout, wantStderr string, wantExit int32) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: stream error after %d frames (%d stdout bytes, %d stderr bytes): %v",
			label, run.frames, len(run.stdout), len(run.stderr), err)
	}
	if run.exitFrame < 0 {
		t.Fatalf("%s: no exit-code frame (frames=%d stdout=%q stderr=%q)",
			label, run.frames, dfb27Clip(run.stdout), dfb27Clip(run.stderr))
	}
	if run.exitFrame != run.frames-1 {
		t.Errorf("%s: exit-code frame at index %d of %d frames — it must be the last frame",
			label, run.exitFrame, run.frames)
	}
	if run.exitCode != wantExit {
		t.Errorf("%s: exit code = %d, want %d", label, run.exitCode, wantExit)
	}
	if run.stdout != wantStdout {
		t.Errorf("%s: stdout incomplete (%d bytes, want %d): %s",
			label, len(run.stdout), len(wantStdout), dfb27Diff(run.stdout, wantStdout))
	}
	if run.stderr != wantStderr {
		t.Errorf("%s: stderr incomplete (%d bytes, want %d): %s",
			label, len(run.stderr), len(wantStderr), dfb27Diff(run.stderr, wantStderr))
	}
}

// dfb27Diff names the first byte that differs between the streamed output and
// what the child wrote, so truncation, duplication and reordering are all
// visible in the failure text.
func dfb27Diff(got, want string) string {
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	i := 0
	for i < n && got[i] == want[i] {
		i++
	}
	lo := i - 20
	if lo < 0 {
		lo = 0
	}
	gotHi, wantHi := i+20, i+20
	if gotHi > len(got) {
		gotHi = len(got)
	}
	if wantHi > len(want) {
		wantHi = len(want)
	}
	return fmt.Sprintf("first difference at byte %d: got %q want %q", i, got[lo:gotHi], want[lo:wantHi])
}

// dfb27Clip shortens a value for a failure message.
func dfb27Clip(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// dfb27HeavyScript alternates one stdout line and one stderr line, a few
// hundred times, so both sinks are sending at the same moment.
func dfb27HeavyScript() string {
	return fmt.Sprintf(
		`i=1; while [ $i -le %d ]; do echo "out-$i"; echo "err-$i" 1>&2; i=$((i+1)); done`,
		dfb27HeavyLines)
}

// dfb27HeavyExpected is what dfb27HeavyScript writes, in write order.
func dfb27HeavyExpected() (string, string) {
	var out, errOut strings.Builder
	for i := 1; i <= dfb27HeavyLines; i++ {
		fmt.Fprintf(&out, "out-%d\n", i)
		fmt.Fprintf(&errOut, "err-%d\n", i)
	}
	return out.String(), errOut.String()
}

// TestExecStreamBothPipesNotCorrupted is the case that was measured broken:
// one command writing one line to each pipe, repeated so the race window is hit
// many times. Every iteration must decode completely and carry every byte.
func TestExecStreamBothPipesNotCorrupted(t *testing.T) {
	const script = "echo OUT; echo ERR 1>&2"
	dfb27InjectExecCommand(t, "sh", "-c", script)
	client := dfb27TestClient(t)

	for i := 0; i < dfb27Iterations; i++ {
		run, err := dfb27RunExec(t, client, script)
		dfb27AssertRun(t, fmt.Sprintf("both-pipes iteration %d/%d", i+1, dfb27Iterations),
			run, err, "OUT\n", "ERR\n", 0)
		if run.frames < 3 {
			t.Errorf("both-pipes iteration %d/%d: %d frames, want >= 3 (stdout frame, stderr frame, exit frame)",
				i+1, dfb27Iterations, run.frames)
		}
	}
}

// TestExecStreamInterleavedBothPipesNotCorrupted is the heavy variant: the child
// alternates stdout and stderr for a few hundred lines, so both sinks send
// concurrently for the whole run. A single dropped, duplicated or reordered
// chunk shows up as an exact-content mismatch.
func TestExecStreamInterleavedBothPipesNotCorrupted(t *testing.T) {
	script := dfb27HeavyScript()
	wantStdout, wantStderr := dfb27HeavyExpected()
	dfb27InjectExecCommand(t, "sh", "-c", script)
	client := dfb27TestClient(t)

	for i := 0; i < dfb27Iterations; i++ {
		label := fmt.Sprintf("interleaved iteration %d/%d", i+1, dfb27Iterations)
		run, err := dfb27RunExec(t, client, script)
		dfb27AssertRun(t, label, run, err, wantStdout, wantStderr, 0)
		if run.frames < 3 {
			t.Errorf("%s: %d frames, want >= 3 (each pipe must produce at least one frame)", label, run.frames)
		}
	}
}

// TestExecStreamBothPipesNonZeroExit keeps the exit-code contract pinned on the
// both-pipe path: a non-zero child exit must arrive as an exitCode frame with a
// nil handler error, after every byte of output.
func TestExecStreamBothPipesNonZeroExit(t *testing.T) {
	const script = "echo OUT; echo ERR 1>&2; exit 7"
	dfb27InjectExecCommand(t, "sh", "-c", script)
	client := dfb27TestClient(t)

	for i := 0; i < 5; i++ {
		run, err := dfb27RunExec(t, client, script)
		dfb27AssertRun(t, fmt.Sprintf("non-zero exit iteration %d/5", i+1),
			run, err, "OUT\n", "ERR\n", 7)
	}
}

// TestExecStreamLargeOutputIsComplete covers the other half of the fix: the
// handler used to call cmd.Wait() while both pipe readers were still in flight,
// and os/exec closes the read ends when the process exits — a fast-exiting
// command could therefore lose the tail of its output. The child here writes
// well past the 64 KiB pipe buffer on BOTH streams, so the drain has to reach
// the last byte of each pipe before the process is reaped.
func TestExecStreamLargeOutputIsComplete(t *testing.T) {
	const lines = 3000
	script := fmt.Sprintf(
		`i=1; while [ $i -le %d ]; do echo "stdout-payload-line-$i"; echo "stderr-payload-line-$i" 1>&2; i=$((i+1)); done`,
		lines)

	var wantStdout, wantStderr strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&wantStdout, "stdout-payload-line-%d\n", i)
		fmt.Fprintf(&wantStderr, "stderr-payload-line-%d\n", i)
	}
	if len(wantStdout.String()) <= 65536 || len(wantStderr.String()) <= 65536 {
		t.Fatalf("test premise: each stream must exceed the 64 KiB pipe buffer, got %d/%d bytes",
			wantStdout.Len(), wantStderr.Len())
	}

	dfb27InjectExecCommand(t, "sh", "-c", script)
	client := dfb27TestClient(t)

	for i := 0; i < 5; i++ {
		run, err := dfb27RunExec(t, client, script)
		dfb27AssertRun(t, fmt.Sprintf("large-output iteration %d/5", i+1),
			run, err, wantStdout.String(), wantStderr.String(), 0)
	}
}

// TestExecStreamSinglePipeStillStreams keeps the pre-existing single-stream
// behaviour covered on both pipes, with and without output.
func TestExecStreamSinglePipeStillStreams(t *testing.T) {
	cases := []struct {
		name                   string
		script                 string
		wantStdout, wantStderr string
		wantExit               int32
	}{
		{name: "stdout-only", script: "echo OUT", wantStdout: "OUT\n"},
		{name: "stderr-only", script: "echo ERR 1>&2", wantStderr: "ERR\n"},
		{name: "no-output", script: "true"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dfb27InjectExecCommand(t, "sh", "-c", c.script)
			client := dfb27TestClient(t)
			for i := 0; i < 10; i++ {
				run, err := dfb27RunExec(t, client, c.script)
				dfb27AssertRun(t, fmt.Sprintf("%s iteration %d/10", c.name, i+1),
					run, err, c.wantStdout, c.wantStderr, c.wantExit)
			}
		})
	}
}
