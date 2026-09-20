package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// PIPED caller observes must agree with the status a bare caller observes —
// except where the shell convention says otherwise, and that exception is
// pinned explicitly (DF-BUNKER-44): a SUCCESS whose reader walked away exits
// 141, the conventional 128+SIGPIPE status `head`/`grep` also report, never a
// fake 0. The truncated-success cases therefore assert 141 against the table,
// not "whatever the bare run did" — the original `got == direct` form was
// vacuous there (0 == 0) and let the fake success through.
func TestBrokenPipeExitStatus(t *testing.T) {
	bin := buildCLIOnce(t)
	bigLog := newDF43AuditLog(t, df43LogRecords)

	// Cases whose output is larger than the pipe buffer, so the consumer's
	// early exit really does cut the writer off. wantDirect is what the command
	// reports with no pipe at all — the premise, checked before piping.
	// wantPiped pins the piped status; nil means "the same as bare" (the
	// DF-BUNKER-43 contract for FAILURES).
	cases := []struct {
		name         string
		args         []string
		wantDirect   df43ExitStatus
		wantConsumer string
		wantPiped    *df43ExitStatus
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
			name: "truncated success (audit list | true) exits 141, not a fake success",
			// DF-BUNKER-44: this command SUCCEEDS bare (exit 0) and writes a
			// table larger than the pipe buffer, so the reader here walks away
			// mid-table. The producer must exit 141 — the conventional status
			// — NOT the bare 0. The pre-fix binary exited 0 here: the table
			// writer's EPIPE was discarded before ExitOnBrokenPipe could see
			// it. (Before DF-BUNKER-43 the kernel killed the process with
			// signal 13 instead, losing the status entirely.)
			args:         []string{"audit", "list", "--path", bigLog},
			wantDirect:   df43ExitStatus{exited: true, code: 0},
			wantConsumer: "true",
			wantPiped:    df44Exited(141),
		},
		{
			name: "truncated success (audit list | head -1) exits 141, not a fake success",
			// The task's literal repro: a real `| head -1` consumer. Same
			// contract as the `true` consumer above, through a reader that
			// first consumes a line and THEN leaves.
			args:         []string{"audit", "list", "--path", bigLog},
			wantDirect:   df43ExitStatus{exited: true, code: 0},
			wantConsumer: "head -1",
			wantPiped:    df44Exited(141),
		},
		{
			name: "truncated success (audit export | true) exits 141 like list must",
			// Consistency pin: export's JSONL encoder always propagated the
			// write error (141 measured pre-DF-BUNKER-44); list must behave
			// identically. If one audit read path regresses and the other
			// does not, this case and its list sibling diverge.
			args:         []string{"audit", "export", "--path", bigLog},
			wantDirect:   df43ExitStatus{exited: true, code: 0},
			wantConsumer: "true",
			wantPiped:    df44Exited(141),
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

			// The wanted piped status: pinned by the table (DF-BUNKER-44
			// truncated-success cases), else the bare status (the
			// DF-BUNKER-43 contract — a pipe must not change the verdict).
			want := direct
			if tc.wantPiped != nil {
				want = *tc.wantPiped
			}

			// The invariant under test: a SIGNAL DEATH is never acceptable —
			// that is the status a caller cannot attribute to the command.
			if !got.exited {
				t.Fatalf("piped %v (%s): %s — the producer was killed by a signal, "+
					"so its exit status is lost; want %s",
					tc.args, tc.wantConsumer, got, want)
			}
			if got != want {
				if tc.wantPiped != nil {
					t.Fatalf("piped %v (%s) = %s, want %s (the conventional 128+SIGPIPE "+
						"status): a truncated SUCCESS must not read as %s",
						tc.args, tc.wantConsumer, got, want, direct)
				}
				t.Fatalf("piped %v (%s) = %s, want %s (same as the bare run): "+
					"a pipe changed the exit status",
					tc.args, tc.wantConsumer, got, direct)
			}
		})
	}
}

// df44Exited returns a pointer to an exited-with-code status, for wantPiped
// table entries (DF-BUNKER-44).
func df44Exited(code int) *df43ExitStatus {
	s := df43ExitStatus{exited: true, code: code}
	return &s
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

// failingWriter fails every write with the supplied error. DF-BUNKER-44's
// class is "the renderer discarded the writer's error", which only shows up
// when the writer actually fails — os.Pipe in the real-binary tests, a
// deliberately failing writer here, where the error is deterministic.
type df44FailingWriter struct{ err error }

func (w *df44FailingWriter) Write(p []byte) (int, error) { return 0, w.err }

// TestPrintAuditTableReturnsWriteError proves the renderer-level half of
// DF-BUNKER-44: printAuditTable must CHECK and RETURN the writer's error (a
// raw EPIPE, so brokenPipeError matches it and ExitOnBrokenPipe maps it to
// 141), not silently discard it the way the pre-fix version did. It also pins
// the fail-fast behavior: rendering stops at the first failed write instead
// of continuing to format thousands of records into a dead pipe.
func TestPrintAuditTableReturnsWriteError(t *testing.T) {
	epipe := &os.PathError{Op: "write", Path: "/dev/stdout", Err: syscall.EPIPE}
	records := make([]audit.Record, 64)
	for i := range records {
		records[i] = audit.Record{
			TS:      "2026-09-20T12:00:00Z",
			Caller:  "master",
			Method:  fmt.Sprintf("/bunker.v1.Bunkerd/SpawnAgent%02d", i),
			AgentID: "agent-000001",
			Outcome: "ok",
			Summary: "df44 renderer-error fixture record",
		}
	}

	t.Run("returns the writer error", func(t *testing.T) {
		w := &df44FailingWriter{err: epipe}
		err := printAuditTable(w, records)
		if err == nil {
			t.Fatal("printAuditTable returned nil on a failing writer: the write " +
				"error was discarded again (DF-BUNKER-44 regression)")
		}
		if !errors.Is(err, syscall.EPIPE) {
			t.Fatalf("printAuditTable error = %v, want an error matching EPIPE", err)
		}
		var ee *ExitError
		if got := ExitOnBrokenPipe(err); !errors.As(got, &ee) || ee.Code != 141 {
			t.Fatalf("ExitOnBrokenPipe(renderer error) = %v, want *ExitError{141}", got)
		}
	})

	t.Run("nil error with a healthy writer", func(t *testing.T) {
		var buf bytes.Buffer
		if err := printAuditTable(&buf, records); err != nil {
			t.Fatalf("printAuditTable on a healthy writer = %v, want nil", err)
		}
		out := buf.String()
		if !strings.Contains(out, "Total: 64 records") {
			t.Fatalf("rendered output missing the Total line; got %d bytes, tail %q",
				len(out), tailOf(out, 80))
		}
		if !strings.Contains(out, "SpawnAgent63") {
			t.Fatal("rendered output missing the last record: the healthy-writer contract changed")
		}
	})

	t.Run("stops at the first failed write", func(t *testing.T) {
		var writes int
		// n=0: the very FIRST write fails; the renderer must attempt no
		// further writes after it (64 records would otherwise mean 66 calls).
		w := errAfterWriter{n: 0, writes: &writes, err: epipe}
		if err := printAuditTable(w, records); !errors.Is(err, syscall.EPIPE) {
			t.Fatalf("printAuditTable = %v, want EPIPE", err)
		}
		if writes != 1 {
			t.Fatalf("printAuditTable attempted %d writes after the first failed: "+
				"rendering must stop at the first failure", writes)
		}
	})
}

// TestPrintAuditStatusReturnsWriteError is the DF-BUNKER-44 sibling check for
// the audit status renderer: same class (writer errors must not be discarded),
// same wiring contract through ExitOnBrokenPipe.
func TestPrintAuditStatusReturnsWriteError(t *testing.T) {
	epipe := &os.PathError{Op: "write", Path: "/dev/stdout", Err: syscall.EPIPE}

	t.Run("returns the writer error", func(t *testing.T) {
		w := &df44FailingWriter{err: epipe}
		st := &audit.StatusReport{Enabled: true, ChainHead: "ab12", Records: 3}
		err := printAuditStatus(w, "/var/log/bunkerd/audit.log", st)
		if err == nil {
			t.Fatal("printAuditStatus returned nil on a failing writer: the write " +
				"error was discarded (DF-BUNKER-44 regression)")
		}
		if !errors.Is(err, syscall.EPIPE) {
			t.Fatalf("printAuditStatus error = %v, want an error matching EPIPE", err)
		}
		var ee *ExitError
		if got := ExitOnBrokenPipe(err); !errors.As(got, &ee) || ee.Code != 141 {
			t.Fatalf("ExitOnBrokenPipe(renderer error) = %v, want *ExitError{141}", got)
		}
	})

	t.Run("returns the writer error when audit is disabled", func(t *testing.T) {
		w := &df44FailingWriter{err: epipe}
		err := printAuditStatus(w, "/var/log/bunkerd/audit.log", &audit.StatusReport{Enabled: false})
		if !errors.Is(err, syscall.EPIPE) {
			t.Fatalf("printAuditStatus(disabled) error = %v, want EPIPE: the early "+
				"return must not bypass the writer's error", err)
		}
	})

	t.Run("nil error with a healthy writer", func(t *testing.T) {
		var buf bytes.Buffer
		st := &audit.StatusReport{Enabled: true, ChainHead: "ab12", Records: 3, LiveSize: 456}
		if err := printAuditStatus(&buf, "/var/log/bunkerd/audit.log", st); err != nil {
			t.Fatalf("printAuditStatus on a healthy writer = %v, want nil", err)
		}
		for _, want := range []string{"enabled:              true", "chain head:           ab12", "rotation sealing"} {
			if !strings.Contains(buf.String(), want) {
				t.Fatalf("rendered status missing %q; got:\n%s", want, buf.String())
			}
		}
	})
}

// errAfterWriter lets the first n writes succeed, then fails the rest — the
// fail-fast probe's counting writer.
type errAfterWriter struct {
	n      int
	writes *int
	err    error
}

func (w errAfterWriter) Write(p []byte) (int, error) {
	*w.writes++
	if *w.writes > w.n {
		return 0, w.err
	}
	return len(p), nil
}

// tailOf returns at most the last n bytes of s (test-output helper).
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
