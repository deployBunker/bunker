package cli

import (
	"errors"
	"fmt"
	"io"
	"os/signal"
	"syscall"
)

// ExitError signals that the CLI process should terminate with a specific
// exit code instead of the default 1. It is used to propagate a remote
// command's exit code (bunker exec / bunker run) to the local shell,
// ssh-style: cmd/bunker/main.go matches it with errors.As and exits
// silently with the carried code, without printing "bunker: ..." noise.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("exit code %d", e.Code)
}

// brokenPipeExitCode is the conventional exit status for a command whose
// stdout reader went away mid-stream (shell 128+SIGPIPE). Interactive
// pipelines expect it; the shell pipeline status of a `... | head` producer
// that is genuinely cut off must stay distinguishable.
const brokenPipeExitCode = 141

// brokenPipeError detects the "reader closed my stdout" condition. It is
// checked by errors.Is so a wrapped *os.PathError, a *fs.PathError from
// os.File, or any other wrapped chain still matches. io.ErrClosedPipe covers
// writers that report the closed pipe as an error value (os.Pipe-backed
// writers, tests); syscall.EPIPE covers the raw write(2) result.
func brokenPipeError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrClosedPipe)
}

// exitOnBrokenPipe inspects the error a command already returned and turns the
// "stdout reader went away" case into the conventional exit code 141. It is
// installed once, on the ROOT command, so every subcommand inherits it: no
// per-command defer, no per-command os.Exit, and no command can forget it.
//
// The GOAL here — stated plainly, because it is easy to get backwards — is to
// make sure a FAILING command is never mistaken for a SUCCESS just because its
// stdout was a pipe. Two independent mechanisms can do that, and this handles
// the second:
//
//  1. errors swallowed on the way out. Guarded by
//     TestEveryCommandErrorReachesMain: a RunE error must reach
//     main.go's os.Exit(1).
//  2. the writer killed mid-stream by SIGPIPE. The Go runtime keeps the
//     default SIGPIPE disposition for writes to fds 1 and 2, so when the
//     command writes more output than the pipe buffer the KERNEL kills the
//     process with signal 13 before main() can run. The exit status is then
//     whatever the last completed syscall implies — which is how a failing
//     `bunker audit list | head -2` can look like a success to a caller.
//
// Mechanism 2 is neutralised in the direction a script cares about by
// IGNORING SIGPIPE for the process (below): the write then returns EPIPE as an
// ordinary error, the failing path keeps its own exit status, and the caller
// sees failure instead of a signal death. The cost is that a command which
// DOES succeed but has a reader that walked away (`bunker version | true`)
// reports 141 instead of 0 — that is the deliberate, conventional choice (the
// same one `head`/`grep` make), not an oversight. A command that must always
// report 0 when its own work succeeded can paper over the truncated-stream
// status itself with errors.Is(err, syscall.EPIPE).
//
// Consequences that matter for review:
//   - Success with a live reader is untouched: nothing is ignored unless the
//     process is about to exit, and the non-pipe case still exits 0.
//   - Human-visible error text is untouched: this only chooses a status code.
func exitOnBrokenPipe(err error) error {
	if !brokenPipeError(err) {
		return err
	}
	return &ExitError{Code: brokenPipeExitCode}
}

// ExitOnBrokenPipe is exitOnBrokenPipe for cmd/bunker's entry point: a
// truncated stdout stream becomes an *ExitError carrying 141, which main
// already knows how to propagate (errors.As → os.Exit(code)).
func ExitOnBrokenPipe(err error) error { return exitOnBrokenPipe(err) }

// IgnoreSIGPIPE is the exported form of ignoreSIGPIPE, for cmd/bunker. See
// ignoreSIGPIPE for why the process entry point — and not an init() — owns
// this.
func IgnoreSIGPIPE() { ignoreSIGPIPE() }

// ignoreSIGPIPE makes writes to a closed pipe return EPIPE to the writer
// instead of killing the process. Call it immediately before Execute(): the
// runtime's special handling of fds 1/2 happens at the signal level, so this
// is the only way Go's own stdout writes can observe the broken pipe as an
// error. It is deliberately NOT installed package-wide at init time — a
// library init() that mutates process-wide signal disposition would leak into
// every importer (including the test binary), so the process entry point owns
// it.
func ignoreSIGPIPE() {
	signal.Ignore(syscall.SIGPIPE)
}
