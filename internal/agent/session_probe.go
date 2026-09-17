// Spawn-time SSH session probe (INT-DEMO-002).
//
// The 2026-09-16 demo outage (INT-DEMO-001) had the daemon reporting a
// RUNNING agent while every SSH session was denied by PAM
// (pam_open_session: System error, ssh exit 254): a user of that agent could
// not exec, mount, or cp, and nothing in the spawn result said so. Spawn()
// used to register and report `Status: "running"` on the strength of the
// authorized_keys/dockerd stages alone — no session probe existed anywhere
// in the spawn path.
//
// probeAgentSession closes that gap: before the agent is registered or
// reported ready, it runs ONE exec-path-identical ssh command — the exact
// target and options buildSSHBaseCommand uses for every exec — with a
// single-token remote command (`whoami`). It is bounded three ways and can
// never hang a spawn or leak an ssh process:
//
//   - hard per-attempt timeout (sessionProbeTimeout, 10s) via
//     context.WithTimeout DERIVED FROM the spawn ctx, so a cancelled
//     request ctx still bounds it and exec.CommandContext kills the child;
//   - at most sessionProbeMaxAttempts attempts (3) with a short backoff;
//   - total worst case ~10+1+10+2+10 ≈ 33s, far below the server request
//     timeout (300s by default).
//
// It fails CLOSED: any probe failure is fatal to the spawn (the caller runs
// the standard rollback and the agent is never registered), never a warning.
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/deployBunker/bunker/internal/sshsig"
)

// sessionProbeTimeout bounds ONE probe attempt (10s). The exec path's ssh
// command already carries -o ConnectTimeout=10 for the TCP connect; this
// context deadline additionally bounds the whole attempt (connect + auth +
// session + remote command), so a PAM-blocked session that never returns
// cannot hang the spawn.
const sessionProbeTimeout = 10 * time.Second

// sessionProbeMaxAttempts is the total attempt count, first try included.
// Three bounded tries ride out a slow sshd/PAM bring-up on a freshly
// created user without ever approaching an unbounded wait.
const sessionProbeMaxAttempts = 3

// probeAgentSession is the spawn-path seam (same pattern as runtimeDirProbe
// in rootless.go: a package-level function var so unit tests can fake the
// probe without running real ssh). Spawn calls this var; the production
// implementation is probeAgentSessionLive below. It returns the number of
// attempts consumed (so the caller can log the attempt count) plus the
// probe error (nil only when a session was proven working).
var probeAgentSession = probeAgentSessionLive

// probeAgentSessionLive runs the exec path's exact ssh target/options with
// remote command `whoami` and returns (attempts, nil) only when the remote
// command ran and exited 0. On failure the error names the cause: when the
// observed signature is the session-denial signature (exit 254, empty
// stderr, non-empty stdout — the INT-DEMO-001 outage), the shared
// session-denial diagnostic is carried in the error; otherwise the exit
// status and stderr are quoted so the operator sees the real reason.
//
// agentID feeds the bunker-<id>@localhost target (the same target the exec
// path builds); username is the local account the session must open as
// as. The spawn ctx must be the live request context — the per-attempt
// deadline is derived from it, so a cancelled request still bounds the
// probe.
func probeAgentSessionLive(ctx context.Context, agentID, username, sshKeyPath string) (int, error) {
	var lastErr error

	for attempt := 1; attempt <= sessionProbeMaxAttempts; attempt++ {
		err := probeAgentSessionOnce(ctx, agentID, sshKeyPath)
		if err == nil {
			return attempt, nil
		}
		lastErr = err

		// Backoff between attempts, but never after the final one, and
		// never past a ctx that is already done (a cancelled request ctx
		// must end the probe immediately — spawnStageErr will attribute
		// the cancellation).
		if attempt < sessionProbeMaxAttempts {
			backoff := time.Duration(attempt) * time.Second
			select {
			case <-ctx.Done():
				return attempt, fmt.Errorf("session probe cancelled while backing off: %w", ctx.Err())
			case <-time.After(backoff):
			}
		}
	}

	return sessionProbeMaxAttempts, lastErr
}

// probeAgentSessionOnce runs one bounded ssh attempt and classifies the
// result. The child inherits the attempt context, so the hard timeout (and
// any request cancellation) kills the ssh process — nothing leaks on
// timeout.
func probeAgentSessionOnce(ctx context.Context, agentID, sshKeyPath string) error {
	attemptCtx, cancel := context.WithTimeout(ctx, sessionProbeTimeout)
	defer cancel()

	// Exec-path-identical command: buildSSHBaseCommand produces exactly
	// ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
	//     -o LogLevel=ERROR -o ConnectTimeout=10 -i <key> bunker-<id>@localhost <remote...>
	// and the probe mirrors it with `whoami` as the remote command.
	cmd := exec.CommandContext(attemptCtx, "ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-i", sshKeyPath,
		fmt.Sprintf("bunker-%s@localhost", agentID),
		"whoami",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// WaitDelay bounds the pipe-drain wait AFTER the context kills the ssh
	// child: Run() does not return until the stdout/stderr pipes close, and
	// a process that inherits them (observed live: a stub whose `sleep`
	// grandchild outlived the killed shell) can hold them open far past the
	// deadline — the probe then hangs despite the ctx. Two seconds after
	// the kill the pipes are force-closed and Wait returns, so the attempt
	// deadline is a genuine hard bound. (Real ssh is a leaf process and
	// closes its own pipes; this is armor for exactly the adversarial
	// cases the bound exists for.)
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()
	if runErr == nil {
		// The command exited 0: the session opened and `whoami` ran as
		// the agent user. The session works — that is all the probe
		// certifies.
		return nil
	}

	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	if attemptCtx.Err() != nil {
		// Deadline or request cancellation: the ssh child was killed by
		// the context (ExitCode -1 when signalled). Report the bound that
		// fired, never a bare "signal: killed".
		if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("session probe attempt timed out after %s (ssh killed by the per-attempt deadline)", sessionProbeTimeout)
		}
		return fmt.Errorf("session probe attempt cancelled: %w", attemptCtx.Err())
	}

	// INT-DEMO-001 signature: the session was denied before the command
	// ran. Name that cause through the shared classifier so spawn and exec
	// speak the same diagnostic.
	if diag, denied := sshsig.ClassifySessionDenial(exitCode, stderr.Len(), stdout.Len()); denied {
		return errors.New(diag)
	}

	if exitCode >= 0 {
		return fmt.Errorf("session probe failed: ssh exited %d (stderr: %s)", exitCode, strings.TrimSpace(stderr.String()))
	}
	return fmt.Errorf("session probe failed: %w (stderr: %s)", runErr, strings.TrimSpace(stderr.String()))
}
