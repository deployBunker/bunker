package cli

import (
	"bytes"
	"fmt"
	"os/exec"
	"sync/atomic"
	"time"
)

// runWithTimeout runs a command with a hard deadline, returning its combined
// (stdout+stderr) output.
//
// History (DF-BUNKER-38): this function used to call cmd.Start() and then hand
// the same *exec.Cmd to cmd.CombinedOutput() inside a goroutine. CombinedOutput
// calls Start internally, so the second Start failed with
//
//	exec: already started
//
// and the real command ran with its output thrown away. Every mount preflight
// saw an empty probe output and "cannot reach <host> to verify <path>", and
// every umount reported
//
//	unmount <path> failed (normal: exec: already started; lazy: exec: already
//	started, output: )
//
// which killed the fusermount3 -> umount -l fallback chain. The mount tests
// routed around the bug entirely: mount_test.go stubs the remotePathCheck seam
// and the subprocess lifecycle suite sets BUNKER_SKIP_MOUNT_PREFLIGHT=1, so no
// test ever executed the real function.
//
// The rewrite keeps the exact signature (call sites in umount.go and
// mount_preflight.go work unchanged) and:
//
//   - starts the process exactly once, via Start + Wait;
//   - delivers cmd.Stdin to the child (the mount preflight writes its probe
//     script to stdin through `ssh ... sh -s`);
//   - captures stdout and stderr concurrently (no pipe deadlock when a child
//     fills one stream while we wait on the other);
//   - on deadline fires Kill and returns a timeout error naming the command,
//     never "exec: already started".
//
// The kill targets the direct child only. Every production call site (ssh,
// fusermount3, umount, lsof, fuser) is a direct binary, so that is sufficient;
// a shell that spawns its own children can outlive the kill by holding the
// inherited pipes, which the tests avoid by stubbing with `exec <cmd>`.
func runWithTimeout(cmd *exec.Cmd, timeout time.Duration) (string, error) {
	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
		killed atomic.Bool
	)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		// Process never started: nothing to reap, return the start error as-is.
		return "", err
	}

	timer := time.AfterFunc(timeout, func() {
		killed.Store(true)
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	defer timer.Stop()

	waitErr := cmd.Wait()

	combined := make([]byte, 0, stdout.Len()+stderr.Len())
	combined = append(combined, stdout.Bytes()...)
	combined = append(combined, stderr.Bytes()...)

	if waitErr == nil {
		return string(combined), nil
	}
	if killed.Load() {
		// Our deadline killed the child: report a timeout that names what hung
		// (with whatever output it managed to produce), not the raw
		// "signal: killed" wait error.
		return string(combined), fmt.Errorf("timed out after %s running %s", timeout, cmd.Path)
	}
	// The child exited on its own with a non-zero status: a real failure.
	// CombinedOutput semantics: output + the error.
	return string(combined), waitErr
}
