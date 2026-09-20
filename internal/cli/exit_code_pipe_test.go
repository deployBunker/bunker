package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/deployBunker/bunker/internal/audit"
)

// DF-BUNKER-43: `bunker status` / `bunker audit` must be a failure to a caller
// that pipes them, not just to a caller that runs them bare.
//
// The class under test is "the exit status lies when stdout is a pipe". It has
// two independent mechanisms and the fixed contract has to name BOTH, because
// they behave differently and only one of them was reachable in the original
// report:
//
//  1. A FAILURE whose output goes through a FULL pipe (the reader consumes
//     everything). The error must still reach main.go's os.Exit(1), so the
//     caller sees 1 — never 0.
//  2. A writer cut off mid-stream. Go's runtime keeps the DEFAULT SIGPIPE
//     disposition for writes to fds 1/2, so a command that writes more than
//     the pipe buffer is KILLED by the kernel (status signal 13). A pipeline
//     that reads only the first lines is then at the mercy of that race, and
//     the failure status is lost. With SIGPIPE ignored the same command exits
//     with CODE 141 — same value a caller observes, but as an ordinary exit
//     status the program chose, not a signal death it cannot control.
//
// These tests drive the REAL binary (the shared per-checkout build from
// TestMain, prodced by buildCLIOnce) because the property is about process exit
// status and pipe semantics; an in-process cmd.Execute() call cannot express
// "my stdout is a pipe with no reader". The fixtures are local files (a
// chained audit log) so nothing here needs a daemon, a network, or root.
//
// Table-driven per repo convention (AGENTS.md: "Prefer table-driven tests").

// df43ExitStatus describes how a child process ended, kept apart from exec's
// error so "exited 3" and "was killed by signal 9" cannot be confused — which
// is the whole point of this file.
type df43ExitStatus struct {
	exited bool
	code   int    // meaningful when exited
	signal string // meaningful when !exited
}

func (s df43ExitStatus) String() string {
	if s.exited {
		return fmt.Sprintf("exited %d", s.code)
	}
	return "killed by signal (" + s.signal + ")"
}

// df43RunToStatus runs cmd to completion and classifies its exit.
func df43RunToStatus(t *testing.T, cmd *exec.Cmd) df43ExitStatus {
	t.Helper()
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run %v: %v", cmd.Args, err)
		}
		if !ee.Exited() {
			return df43ExitStatus{signal: ee.ProcessState.String()}
		}
		return df43ExitStatus{exited: true, code: ee.ExitCode()}
	}
	return df43ExitStatus{exited: true}
}

// df43PipeToCommand starts the CLI with stdout+stderr wired to a PIPE whose
// read end is handed to a consumer shell command, then returns the producer's
// exit status.
//
// The test process must hold NEITHER end of the pipe once both children are
// started, and that is the whole subtlety of this helper: a write to a pipe
// that still has an open read end — even one nobody is reading — BLOCKS until
// buffer space frees up, and only a pipe with NO reader left raises EPIPE. An
// earlier version of this helper kept the read end open for the deferred
// Close, which turned the "reader walked away" cases into an indefinite
// deadlock (the producer blocked in write, the test blocked in Wait) instead of
// the broken pipe they are meant to reproduce. Closing the parent's copies is
// what makes the consumer the ONLY reader, exactly as a real `... | head` is.
func df43PipeToCommand(t *testing.T, bin string, cliArgs []string, consumer string) df43ExitStatus {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	prod := exec.Command(bin, cliArgs...)
	prod.Stdout = pw
	prod.Stderr = pw
	if err := prod.Start(); err != nil {
		pr.Close()
		pw.Close()
		t.Fatalf("start %v: %v", cliArgs, err)
	}
	pw.Close() // the parent holds no write end: only the CLI can write

	consumerCmd := exec.Command("sh", "-c", consumer)
	consumerCmd.Stdin = pr
	if err := consumerCmd.Start(); err != nil {
		pr.Close()
		_ = prod.Process.Kill()
		_ = prod.Wait()
		t.Fatalf("start consumer %q: %v", consumer, err)
	}
	// The consumer now owns the only read end. Dropping the parent's copy is
	// what lets a consumer that exits without reading ({true, echo}) close the
	// pipe for real.
	pr.Close()

	consumerErr := consumerCmd.Wait()
	status, waitErr := df43WaitStatus(prod)
	if waitErr != nil {
		t.Fatalf("wait %v: %v", cliArgs, waitErr)
	}
	if consumerErr != nil {
		t.Logf("consumer %q exited %v (its own status is not under test)", consumer, consumerErr)
	}
	return status
}

// df43WaitStatus waits for cmd and classifies how it ended.
func df43WaitStatus(cmd *exec.Cmd) (df43ExitStatus, error) {
	werr := cmd.Wait()
	if werr == nil {
		return df43ExitStatus{exited: true}, nil
	}
	var ee *exec.ExitError
	if !errors.As(werr, &ee) {
		return df43ExitStatus{}, werr
	}
	if !ee.Exited() {
		return df43ExitStatus{signal: ee.ProcessState.String()}, nil
	}
	return df43ExitStatus{exited: true, code: ee.ExitCode()}, nil
}

// newDF43AuditLog writes a real hash-chained audit log with n records and
// returns its path. n must be large enough that the RENDERED table exceeds the
// 64 KiB pipe buffer, so a `| head -N` consumer provably truncates the stream.
func newDF43AuditLog(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := audit.New(path)
	if err != nil {
		t.Fatalf("audit.New(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	for i := 1; i <= n; i++ {
		rec := audit.Record{
			TS:      fmt.Sprintf("2026-09-20T12:%02d:%02dZ", i%60, (i*7)%60),
			Caller:  "master",
			Method:  "/bunker.v1.Bunkerd/SpawnAgent",
			AgentID: fmt.Sprintf("agent-%06d", i),
			Outcome: "ok",
			// Long enough that n records comfortably exceed the pipe buffer.
			Summary: fmt.Sprintf("fixture record %06d with a deliberately long summary field", i),
		}
		if err := l.Log(rec); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}
	return path
}

// TestBrokenPipeExitStatus is the DF-BUNKER-43 regression: the exit status a
// PIPED caller observes must agree with the status a bare caller observes.
func TestBrokenPipeExitStatus(t *testing.T) {
	bin := buildCLIOnce(t)
	bigLog := newDF43AuditLog(t, df43LogRecords)

	// Cases whose output is larger than the pipe buffer, so the consumer's
	// early exit really does cut the writer off. wantDirect is what the command
	// reports with no pipe at all — the status the piped form must not lose.
	cases := []struct {
		name         string
		args         []string
		wantDirect   df43ExitStatus
		wantConsumer string
	}{
		{
			name: "unknown flag is a failure through a full pipe",
			args: []string{"status", "--json"},
			// A failure must never read as success: the consumer that drains
			// everything must leave the caller with the real failure status.
			wantDirect:   df43ExitStatus{exited: true, code: 1},
			wantConsumer: "cat >/dev/null",
		},
		{
			name:         "unknown flag is a failure through a closed pipe",
			args:         []string{"status", "--json"},
			wantDirect:   df43ExitStatus{exited: true, code: 1},
			wantConsumer: "true",
		},
		{
			name:         "missing audit log is a failure through a closed pipe",
			args:         []string{"audit", "verify", "--path", filepath.Join(t.TempDir(), "absent.log")},
			wantDirect:   df43ExitStatus{exited: true, code: 1},
			wantConsumer: "true",
		},
		{
			name: "truncated success must not die by signal",
			// The real-binary reproduction of the SIBLING defect class: this
			// command SUCCEEDS (exit 0 bare) and writes a table larger than the
			// pipe buffer, so the kernel used to kill it mid-write. The caller
			// must observe an exit STATUS it can reason about.
			args:         []string{"audit", "list", "--path", bigLog},
			wantDirect:   df43ExitStatus{exited: true, code: 0},
			wantConsumer: "true",
		},
		{
			name:         "success with a live reader still exits 0",
			args:         []string{"audit", "list", "--path", bigLog},
			wantDirect:   df43ExitStatus{exited: true, code: 0},
			wantConsumer: "cat >/dev/null",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Premise: the bare invocation reports what we think it does.
			direct := df43RunToStatus(t, exec.Command(bin, tc.args...))
			if direct != tc.wantDirect {
				t.Fatalf("bare %v = %s, want %s (premise for this case is wrong)",
					tc.args, direct, tc.wantDirect)
			}

			got := df43PipeToCommand(t, bin, tc.args, tc.wantConsumer)

			// The invariant under test: a SIGNAL DEATH is never acceptable —
			// that is the status a caller cannot attribute to the command.
			if !got.exited {
				t.Fatalf("piped %v (%s): %s — the producer was killed by a signal, "+
					"so its exit status is lost; want the same status as the bare run (%s)",
					tc.args, tc.wantConsumer, got, direct)
			}
			if got != direct {
				t.Fatalf("piped %v (%s) = %s, want %s (same as the bare run): "+
					"a pipe changed the exit status",
					tc.args, tc.wantConsumer, got, direct)
			}
		})
	}
}

// df43LogRecords is the fixture size: at ~150 bytes rendered per record this
// produces a table comfortably past the 64 KiB pipe buffer, so the truncating
// consumers in TestBrokenPipeExitStatus genuinely cut the writer off. The test
// asserts the SIZE premise rather than assuming it.
const df43LogRecords = 3000

// df43UnreadableLog creates a file that EXISTS but cannot be read, which is the
// failure mode the audit commands must report non-zero. A missing file is not
// the right fixture for every subcommand: `audit status` treats absence as
// "audited off" and exits 0 on purpose. Skipped as root, where mode 0000 is
// still readable and the fixture cannot be built.
func df43UnreadableLog(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0000 does not deny reads, so the unreadable-log fixture cannot be built")
	}
	p := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(p, []byte("{\"ts\":\"t\"}\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatalf("chmod fixture: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	return p
}

// TestDF43FixtureExceedsPipeBuffer pins the fixture premise: if a future change
// shortens the rendered row (or the box grows its pipe buffer past the table
// size), the truncation cases above would pass vacuously — a command that fits
// in the buffer is never cut off. Fail loudly instead of quietly testing
// nothing.
func TestDF43FixtureExceedsPipeBuffer(t *testing.T) {
	bin := buildCLIOnce(t)
	bigLog := newDF43AuditLog(t, df43LogRecords)

	out, err := exec.Command(bin, "audit", "list", "--path", bigLog).Output()
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	const pipeBuffer = 65536
	if len(out) <= pipeBuffer {
		t.Fatalf("fixture renders %d bytes, not more than the %d-byte pipe buffer: "+
			"the truncation cases in TestBrokenPipeExitStatus cannot reproduce the "+
			"cut-off writer and would pass vacuously — increase df43LogRecords",
			len(out), pipeBuffer)
	}
	t.Logf("fixture renders %d bytes (> %d-byte pipe buffer)", len(out), pipeBuffer)

	// Control: the same fixture with a live reader exits 0 — so a non-zero
	// status above can only come from the pipe, not from the fixture.
	if st := df43RunToStatus(t, exec.Command(bin, "audit", "list", "--path", bigLog)); st != (df43ExitStatus{exited: true}) {
		t.Fatalf("control: bare audit list = %s, want exited 0", st)
	}
}

// TestExitOnBrokenPipeClassifies pins the mapping the entry point relies on:
// a broken-pipe error becomes exit code 141, everything else passes through
// untouched (so no other exit code and no error text moves).
func TestExitOnBrokenPipeClassifies(t *testing.T) {
	// Everything that is NOT a broken pipe must pass through byte-identical.
	// An *ExitError is included deliberately: exec/run propagate a remote exit
	// code through this same entry point, and rewriting that error would move
	// the ssh-style exit code (7 below) onto 141.
	base := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain failure", fmt.Errorf("load config: boom")},
		{"permission denied", &os.PathError{Op: "open", Path: "/var/log/x", Err: os.ErrPermission}},
		{"exit error passthrough", &ExitError{Code: 7}},
	}
	for _, tc := range base {
		t.Run(tc.name, func(t *testing.T) {
			got := ExitOnBrokenPipe(tc.err)
			if got != tc.err {
				t.Fatalf("ExitOnBrokenPipe(%v) = %v, want the error unchanged", tc.err, got)
			}
		})
	}

	// ...and the ok/7 check has to be on the SAME value, so it lives outside
	// the loop: the last case carried *ExitError{Code: 7} and the assertion is
	// that the code survived rather than being replaced.
	t.Run("exit code 7 survives", func(t *testing.T) {
		got := ExitOnBrokenPipe(&ExitError{Code: 7})
		var ee *ExitError
		if !errors.As(got, &ee) || ee.Code != 7 {
			t.Fatalf("ExitOnBrokenPipe(&ExitError{Code: 7}) = %v, want *ExitError{Code: 7}", got)
		}
	})

	// The broken-pipe shapes: raw EPIPE, and a wrapped path error from a write.
	broken := []struct {
		name string
		err  error
	}{
		{"raw EPIPE", syscall.EPIPE},
		{"wrapped write EPIPE", &os.PathError{Op: "write", Path: "/dev/stdout", Err: syscall.EPIPE}},
		{"wrapped with %w", fmt.Errorf("print table: %w", syscall.EPIPE)},
		{"io.ErrClosedPipe", io.ErrClosedPipe},
		{"fmt-wrapped closed pipe", fmt.Errorf("encode audit record: %w", io.ErrClosedPipe)},
	}
	for _, tc := range broken {
		t.Run(tc.name, func(t *testing.T) {
			got := ExitOnBrokenPipe(tc.err)
			var ee *ExitError
			if !errors.As(got, &ee) {
				t.Fatalf("ExitOnBrokenPipe(%v) = %T, want *ExitError", tc.err, got)
			}
			if ee.Code != 141 {
				t.Fatalf("ExitOnBrokenPipe(%v) exit code = %d, want 141", tc.err, ee.Code)
			}
			if tc.err.Error() == got.Error() {
				t.Fatalf("broken-pipe error text was preserved verbatim (%q): the entry "+
					"point prints this instead of the real message", got.Error())
			}
		})
	}
}

// TestEveryCommandErrorReachesMain guards mechanism 1 of the DF-BUNKER-43 class
// generically: a command that returns an error must not swallow it. Any RunE
// that printed a failure and returned nil would make a piped failure read as
// success no matter what the entry point does, so the invariant is asserted for
// real invocations of the affected surfaces.
func TestEveryCommandErrorReachesMain(t *testing.T) {
	bin := buildCLIOnce(t)
	cases := []struct {
		name string
		args []string
	}{
		{"unknown flag on status", []string{"status", "--json"}},
		{"unknown flag on audit", []string{"audit", "list", "--nope"}},
		{"missing audit log (verify)", []string{"audit", "verify", "--path", filepath.Join(t.TempDir(), "absent.log")}},
		{"missing audit log (list)", []string{"audit", "list", "--path", filepath.Join(t.TempDir(), "absent.log")}},
		// `audit status` deliberately exits 0 on a MISSING log (it reports
		// enabled: false — audited-off is a legitimate state, and the command is
		// documented as a local inventory summary). The failure case for it is
		// the one that really fails: a log that exists but cannot be read.
		{"unreadable audit log (status)", []string{"audit", "status", "--path", df43UnreadableLog(t)}},
		{"unknown server alias", []string{"status", "--server", "df43-does-not-exist"}},
		{"unknown subcommand", []string{"df43-no-such-command"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := df43RunToStatus(t, exec.Command(bin, tc.args...))
			if !st.exited {
				t.Fatalf("%v: %s — a failure must exit with a status, not die by signal", tc.args, st)
			}
			if st.code == 0 {
				t.Fatalf("%v exited 0: the failure was swallowed before main.go could report it", tc.args)
			}
		})
	}
}
