package server

// GAP-094 round-trip tests: stdin piping, base64 encoding, truncation notice —
// through the REAL handler and a REAL connect client, with the command builders
// stubbed to run locally (same harness as the DF-BUNKER-27 stream tests).

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// execEncodingRun collects one ExecAgent call's frames, decoding base64 when
// the request asked for it.
type exec94Run struct {
	stdout   string // decoded
	stderr   string // decoded
	exitCode int32
	notice   string
}

func exec94RunExec(t *testing.T, client exec94Clienter, stdin []byte, enc v1.ExecEncoding, script string) (exec94Run, error) {
	t.Helper()
	run := exec94Run{}
	stream, err := client.ExecAgent(context.Background(), connect.NewRequest(&v1.ExecAgentRequest{
		AgentId:          exec94AgentID,
		Command:          "sh",
		Args:             []string{"-c", script},
		StdinPayload:     stdin,
		ResponseEncoding: enc,
	}))
	if err != nil {
		return run, err
	}
	for stream.Receive() {
		msg := stream.Msg()
		switch {
		case msg.GetStdout() != nil:
			run.stdout += decode94(enc, msg.GetStdout())
		case msg.GetStderr() != nil:
			run.stderr += decode94(enc, msg.GetStderr())
		default:
			run.exitCode = msg.GetExitCode()
			run.notice = msg.GetTruncationNotice()
		}
	}
	return run, stream.Err()
}

// exec94RunExecCapped sends an exec with a per-request cap override.
func exec94RunExecCapped(t *testing.T, client exec94Clienter, capBytes uint64, script string) (exec94Run, error) {
	t.Helper()
	run := exec94Run{}
	stream, err := client.ExecAgent(context.Background(), connect.NewRequest(&v1.ExecAgentRequest{
		AgentId:          exec94AgentID,
		Command:          "sh",
		Args:             []string{"-c", script},
		ResponseCapBytes: capBytes,
	}))
	if err != nil {
		return run, err
	}
	for stream.Receive() {
		msg := stream.Msg()
		switch {
		case msg.GetStdout() != nil:
			run.stdout += string(msg.GetStdout())
		case msg.GetStderr() != nil:
			run.stderr += string(msg.GetStderr())
		default:
			run.exitCode = msg.GetExitCode()
			run.notice = msg.GetTruncationNotice()
		}
	}
	return run, stream.Err()
}

func decode94(enc v1.ExecEncoding, raw []byte) string {
	if enc == v1.ExecEncoding_EXEC_ENCODING_BASE64 {
		d, err := base64.StdEncoding.DecodeString(string(raw))
		if err != nil {
			return fmt.Sprintf("<undecodable: %v>", err)
		}
		return string(d)
	}
	return string(raw)
}

// TestExec94_CompatNoPayloadTextEncoding: no stdin, no encoding flag ->
// byte-identical legacy behavior, no notice.
func TestExec94_CompatNoPayloadTextEncoding(t *testing.T) {
	exec94Service(t, "echo hello")
	client := exec94Client(t)
	run, err := exec94RunExec(t, client, nil, v1.ExecEncoding_EXEC_ENCODING_TEXT, "hello-marker")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(run.stdout) != "hello" {
		t.Errorf("stdout = %q", run.stdout)
	}
	if run.exitCode != 0 {
		t.Errorf("exitCode = %d", run.exitCode)
	}
	if run.notice != "" {
		t.Errorf("a clean small run must not carry a truncation notice, got: %s", run.notice)
	}
}

// TestExec94_StdinRoundTrip: the piped payload reaches the command AND the
// command TERMINATES because the pipe closes after EOF (a hang would fail the
// test's deadline, not assert).
func TestExec94_StdinRoundTrip(t *testing.T) {
	exec94Service(t, "wc -l")
	client := exec94Client(t)
	payload := "stdin line one\nstdin line two\n"
	run, err := exec94RunExec(t, client, []byte(payload), v1.ExecEncoding_EXEC_ENCODING_TEXT, "count-lines")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(run.stdout); got != "2" {
		t.Errorf("wc -l over the piped stdin = %q, want 2", got)
	}
}

// TestExec94_StdinBinaryRoundTrip: sha256 of the piped bytes is computed over
// exactly what was sent — non-UTF8-safe stdin plumbing.
func TestExec94_StdinBinaryRoundTrip(t *testing.T) {
	exec94Service(t, "sha256sum")
	client := exec94Client(t)
	payload := make([]byte, 0, 256)
	for i := 0; i < 256; i++ {
		payload = append(payload, byte(i))
	}
	run, err := exec94RunExec(t, client, payload, v1.ExecEncoding_EXEC_ENCODING_TEXT, "sum-stdin")
	if err != nil {
		t.Fatal(err)
	}
	want := sha256Hex(payload)
	if !strings.HasPrefix(run.stdout, want) {
		t.Errorf("sha256 over stdin = %q, want prefix %q; stderr: %q", run.stdout, want, run.stderr)
	}
}

// TestExec94_Base64BinaryOutput: a command emitting invalid-UTF8 arrives
// intact when base64 encoding is on.
func TestExec94_Base64BinaryOutput(t *testing.T) {
	exec94Service(t, `printf '\000\001\377\376\005'`)
	client := exec94Client(t)
	run, err := exec94RunExec(t, client, nil, v1.ExecEncoding_EXEC_ENCODING_BASE64, "emit-binary")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x00, 0x01, 0xff, 0xfe, 0x05}
	if !bytes.Equal([]byte(run.stdout), want) {
		t.Errorf("base64-decoded stdout = %x, want %x", []byte(run.stdout), want)
	}
}

// TestExec94_CapTruncatesWithNotice: output beyond the cap is dropped and the
// final frame says so. Uses a fork of the writer so the cap is small enough to
// test without shipping megabytes through the fake local socket.
func TestExec94_CapTruncatesWithNotice(t *testing.T) {
	exec94Service(t, `head -c 100000 /dev/zero | tr '\0' 'x'`)
	client := exec94Client(t)
	// Produce well over the default cap in a way that still terminates fast.
	run, err := exec94RunExecCapped(t, client, 4096, "big-output")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.stdout) > 4096 {
		t.Fatalf("expected truncation at the 4096-byte cap, got %d bytes", len(run.stdout))
	}
	if run.notice == "" {
		t.Fatal("truncated output must carry a notice on the final frame")
	}
	re := regexp.MustCompile(`cap is (\d+) bytes`)
	m := re.FindStringSubmatch(run.notice)
	if m == nil {
		t.Fatalf("notice must name the cap, got: %s", run.notice)
	}
	if m[1] != "4096" {
		t.Errorf("notice cap = %s, want 4096", m[1])
	}
	if !strings.Contains(run.notice, "bunker cp") {
		t.Errorf("notice must name the remedy, got: %s", run.notice)
	}
}
