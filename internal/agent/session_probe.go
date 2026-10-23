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
// single-token remote command (`whoami`). It is bounded four ways and can
// never hang a spawn or leak an ssh process:
//
//   - a per-attempt timeout derived from the call context, so a cancelled
//     request ctx still bounds it and exec.CommandContext kills the child;
//   - at most sessionProbeMaxAttempts attempts (3) with a short backoff;
//   - a TOTAL probe budget (sessionProbeTotalBudgetMax, 90s) that stops the
//     retry loop even when a scaled per-attempt budget would allow more;
//   - the whole probe rides the request context, so the server's request
//     timeout still caps it.
//
// It fails CLOSED: any probe failure is fatal to the spawn (the caller runs
// the standard rollback and the agent is never registered), never a warning.
//
// ── INT-CI-082: the per-attempt budget scales with concurrency ────────────
//
// The per-attempt deadline used to be a FLAT 10s (sessionProbeTimeout). E2E
// battery run 39822820089 (2026-10-23, bunker-mvp) had all four section-8
// concurrent spawns (e2e-agent-2..5) die with
//
//	spawn e2e-agent-<n> failed at stage session-probe: session probe attempt
//	timed out after 10s (ssh killed by the per-attempt deadline)
//
// while the same cell passed 30 minutes later. The measured cause is not CPU
// starvation and not recycled-uid state: systemd-logind's D-Bus service was
// not answering. Measured on bunker-mvp during the wedge (2026-10-23
// 13:52-13:56Z, loadavg 1.04 on 8 CPUs, 87% idle):
//
//	busctl ... org.freedesktop.DBus.Peer Ping   -> 25.03s, rc=1 (timed out) x3
//	loginctl list-users                         -> 25.03s, 25.04s, 25.04s
//	sshd: pam_systemd(sshd:session): Failed to create session: Connection timed out
//	loginctl enable-linger <agent>              -> "Could not enable linger: Connection timed out"
//	the probe's own argv (ssh ... whoami)       -> 12.01s, killed (rc=124), 4/4
//
// i.e. every session open goes through pam_systemd's CreateSession call into
// logind, and while logind is not servicing its bus that call takes longer
// than the probe's flat 10s deadline. A burst makes it worse (each spawn's
// bring-up adds logind work) but the flat deadline is what turns a slow
// session into a failed spawn plus a full rollback.
//
// So the per-attempt budget is scaled by the number of session probes THIS
// PROCESS has in flight, the same shape INT-CI-070 gave the dockerd readiness
// wait:
//
//	budget(n) = min(max(base, perSpawn × n), max)
//
// A lone probe keeps the ORIGINAL 10s fast path (nothing about a single
// healthy spawn gets slower), a 4-way section-8 burst gets 40s — comfortably
// past the 25s logind service latency measured above — and the 40s cap keeps
// a wedged logind bounded. The budget is re-derived on every attempt and
// LATCHED (it only ever grows), so a probe that began alone inherits the
// headroom of peers that joined it.
//
// Fail-fast is preserved on the axis that matters: a session DENIAL (exit
// 254, empty stderr, banner on stdout — the INT-DEMO-001 outage) is
// deterministic, so it is NOT retried at all and it does not run the
// diagnostics gatherer: the probe returns on its FIRST attempt, quickly, with
// the shared session-denial diagnostic. A longer budget therefore cannot mask
// a genuine denial.
//
// Every attempt's wall time is recorded (sessionProbeReport.Latencies) and
// logged by the spawn path, and a final failure is reported WITH the logind
// record and the journalctl _UID=<uid> excerpt for the attempts, so the next
// occurrence names its own mechanism instead of needing a hand-run
// investigation.
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/deployBunker/bunker/internal/sshsig"
)

// sessionProbeBudgetBase is the per-attempt budget for a LONE session probe:
// the exact 10s deadline this probe shipped with (INT-DEMO-002). A single
// spawn on a healthy host is not made slower by the INT-CI-082 change.
var sessionProbeBudgetBase = 10 * time.Second

// sessionProbeBudgetPerSpawn is the budget share each CONCURRENT session probe
// in this process contributes. A section-8 4-way burst grants 40s, which is
// past the 25.03s logind D-Bus latency measured on bunker-mvp during the
// 2026-10-23 wedge (see the file comment).
var sessionProbeBudgetPerSpawn = 10 * time.Second

// sessionProbeBudgetMax is the HARD CAP on the per-attempt budget: however
// many probes are in flight, one attempt can never hold a spawn open longer
// than this.
var sessionProbeBudgetMax = 40 * time.Second

// sessionProbeTotalBudgetMax bounds the WHOLE probe (all attempts plus their
// backoffs). Without it, three attempts at the 40s cap plus 3s of backoff
// would hold a spawn for ~123s; with it, the loop stops retrying once the
// total budget is spent, so a wedged session subsystem fails the spawn in
// ~90s and the rollback starts. It stays far below the server request timeout
// (300s by default).
var sessionProbeTotalBudgetMax = 90 * time.Second

// sessionProbeMaxAttempts is the total attempt count, first try included.
// Three bounded tries ride out a slow sshd/PAM bring-up on a freshly
// created user without ever approaching an unbounded wait.
var sessionProbeMaxAttempts = 3

// The budget/attempt knobs are vars purely as test seams (the
// dockerdReadyBudget* / lingerEnableAttempts convention); production never
// writes them.

// sessionProbesInFlight counts the session probes this process currently has
// in flight — the contention signal sessionProbeBudget scales with
// (INT-CI-082).
var sessionProbesInFlight int32

// sessionProbeEntered registers one in-flight probe and returns the count
// including it.
func sessionProbeEntered() int { return int(atomic.AddInt32(&sessionProbesInFlight, 1)) }

// sessionProbeLeft deregisters a probe.
func sessionProbeLeft() { atomic.AddInt32(&sessionProbesInFlight, -1) }

// sessionProbesActive reports how many probes are in flight right now.
func sessionProbesActive() int { return int(atomic.LoadInt32(&sessionProbesInFlight)) }

// sessionProbeBudget is the per-attempt budget for a probe that observes
// inFlight concurrent session probes:
// min(max(base, perSpawn × inFlight), max) — see the INT-CI-082 block.
func sessionProbeBudget(inFlight int) time.Duration {
	if inFlight < 1 {
		inFlight = 1
	}
	budget := sessionProbeBudgetBase
	if scaled := time.Duration(inFlight) * sessionProbeBudgetPerSpawn; scaled > budget {
		budget = scaled
	}
	if budget > sessionProbeBudgetMax {
		budget = sessionProbeBudgetMax
	}
	return budget
}

// sessionProbeReport is what one probe run measured: how many attempts it
// consumed, each attempt's wall time, the per-attempt budget in force on the
// LAST attempt (the latched maximum), the peak number of probes that were in
// flight while it ran, and whether the INT-DEMO-001 denial signature ended it.
//
// It exists because "the probe timed out" is not diagnosable: E2E battery run
// 39822820089 discarded the spawns' output and the per-attempt latencies had
// never been recorded anywhere (INT-CI-082 AC2). The spawn path logs every
// field of this struct.
type sessionProbeReport struct {
	Attempts   int
	Latencies  []time.Duration
	Budget     time.Duration
	Concurrent int
	Denied     bool
}

// LatencyMillis renders the per-attempt wall times for a structured log line.
func (r sessionProbeReport) LatencyMillis() []int64 {
	out := make([]int64, 0, len(r.Latencies))
	for _, d := range r.Latencies {
		out = append(out, d.Milliseconds())
	}
	return out
}

// sessionProbeDenial is the INT-DEMO-001 signature: PAM denied the session
// before the remote command could run. It is a distinct type because the
// probe treats it as TERMINAL — a denial is deterministic, so retrying it (or
// burning a longer budget on it) would only delay the rollback and hide the
// cause.
type sessionProbeDenial struct {
	diagnostic string
	exitCode   int
}

func (e *sessionProbeDenial) Error() string { return e.diagnostic }

// probeAgentSession is the spawn-path seam (same pattern as runtimeDirProbe
// in rootless.go: a package-level function var so unit tests can fake the
// probe without running real ssh). Spawn calls this var; the production
// implementation is probeAgentSessionLive below. The report carries what the
// run measured (attempt count, per-attempt latencies, budget, concurrency);
// the error is nil only when a session was proven working.
var probeAgentSession = probeAgentSessionLive

// probeAgentSessionLive runs the exec path's exact ssh target/options with
// remote command `whoami` and returns a nil error only when the remote
// command ran and exited 0. On failure the error names the cause: a session
// DENIAL (exit 254, empty stderr, non-empty stdout — the INT-DEMO-001
// outage) is returned on the FIRST attempt with the shared session-denial
// diagnostic; anything else is retried within the total budget, with the exit
// status / stderr / bound that fired quoted.
//
// agentID feeds the bunker-<id>@localhost target (the same target the exec
// path builds); username is the local account the session must open as (and
// the subject of the failure diagnostics). The spawn ctx must be the live
// request context — the per-attempt deadline is derived from it, so a
// cancelled request still bounds the probe.
func probeAgentSessionLive(ctx context.Context, agentID, username, sshKeyPath string) (sessionProbeReport, error) {
	var rep sessionProbeReport

	// This probe IS one of the probes in flight; the count is the contention
	// signal the per-attempt budget scales with.
	rep.Concurrent = sessionProbeEntered()
	defer sessionProbeLeft()

	budget := sessionProbeBudget(rep.Concurrent)
	started := time.Now()
	var lastErr error

	for attempt := 1; attempt <= sessionProbeMaxAttempts; attempt++ {
		// Re-derive the budget from the concurrency observed NOW and LATCH it
		// (it only ever grows): a probe that began alone inherits the
		// headroom of peers that joined it a moment later, and a peer leaving
		// never shortens a probe that is legitimately mid-burst.
		if active := sessionProbesActive(); active > rep.Concurrent {
			rep.Concurrent = active
		}
		if live := sessionProbeBudget(rep.Concurrent); live > budget {
			budget = live
		}
		rep.Attempts = attempt
		rep.Budget = budget

		attemptStarted := time.Now()
		err := probeAgentSessionOnce(ctx, agentID, sshKeyPath, budget, rep.Concurrent)
		rep.Latencies = append(rep.Latencies, time.Since(attemptStarted))
		if err == nil {
			return rep, nil
		}
		lastErr = err

		// A session DENIAL is deterministic: retrying it cannot help, and a
		// longer budget must never turn a fail-closed denial into a slow
		// spawn (INT-CI-082 AC3). Return on the first attempt.
		var denial *sessionProbeDenial
		if errors.As(err, &denial) {
			rep.Denied = true
			return rep, err
		}

		// The caller went away (request deadline or cancellation): stop
		// immediately, nothing to retry.
		if ctx.Err() != nil {
			return rep, err
		}

		if attempt < sessionProbeMaxAttempts {
			if time.Since(started) >= sessionProbeTotalBudgetMax {
				// The probe has already spent its whole budget on this
				// attempt: another one could only repeat the same wait.
				lastErr = fmt.Errorf("%w (probe total budget %s spent after %d attempt(s); not retrying)",
					lastErr, sessionProbeTotalBudgetMax, attempt)
				break
			}
			backoff := time.Duration(attempt) * time.Second
			select {
			case <-ctx.Done():
				return rep, fmt.Errorf("session probe cancelled while backing off: %w", ctx.Err())
			case <-time.After(backoff):
			}
		}
	}

	// Fail CLOSED, and make the failure self-diagnosing: the logind record and
	// the journal for this uid around the attempts are what distinguish "the
	// session subsystem is not answering" from "this agent's session is
	// broken" (INT-CI-082 AC2).
	if diag := sessionProbeDiagnostics(ctx, username); diag != "" {
		lastErr = fmt.Errorf("%w; session diagnostics: %s", lastErr, diag)
	}
	return rep, lastErr
}

// probeAgentSessionOnce runs one bounded ssh attempt and classifies the
// result. The child inherits the attempt context, so the hard timeout (and
// any request cancellation) kills the ssh process — nothing leaks on
// timeout.
func probeAgentSessionOnce(ctx context.Context, agentID, sshKeyPath string, budget time.Duration, concurrent int) error {
	attemptCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// Exec-path-identical command: buildSSHBaseCommand produces exactly
	// ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
	//     -o LogLevel=ERROR -o ConnectTimeout=10 -i <key> bunker-<id>@localhost <remote...>
	// and the probe mirrors it with `whoami` as the remote command.
	//
	// ConnectTimeout=10 bounds the TCP connect only; the WHOLE attempt is
	// bounded by the scaled budget above (a healthy host's session bring-up
	// is milliseconds, so the TCP connect bound never dominates).
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
		// fired, never a bare "signal: killed" — including the budget it
		// was granted and the concurrency that scaled it, so a timeout is
		// attributable from the log alone.
		if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("session probe attempt timed out after %s (ssh killed by the per-attempt deadline; budget scaled to %d concurrent session probe(s))",
				budget, concurrent)
		}
		return fmt.Errorf("session probe attempt cancelled: %w", attemptCtx.Err())
	}

	// INT-DEMO-001 signature: the session was denied before the command
	// ran. Name that cause through the shared classifier so spawn and exec
	// speak the same diagnostic — and type it so the caller does not retry a
	// deterministic denial.
	if diag, denied := sshsig.ClassifySessionDenial(exitCode, stderr.Len(), stdout.Len()); denied {
		return &sessionProbeDenial{diagnostic: diag, exitCode: exitCode}
	}

	if exitCode >= 0 {
		return fmt.Errorf("session probe failed: ssh exited %d (stderr: %s)", exitCode, strings.TrimSpace(stderr.String()))
	}
	return fmt.Errorf("session probe failed: %w (stderr: %s)", runErr, strings.TrimSpace(stderr.String()))
}

// ── failure diagnostics (INT-CI-082 AC2) ───────────────────────────────────
//
// "session probe attempt timed out after 10s" says nothing about WHY. The
// 2026-10-23 occurrence needed a hand investigation (the battery had discarded
// the spawn output) to discover that systemd-logind was not answering its bus.
// So a FAILED probe gathers, best effort and bounded, the two facts that
// classify the failure:
//
//   - logind's record for the agent user (Id/State/Linger — is there a session
//     at all, and is the user lingering?);
//   - the journal for the agent's UID (`journalctl _UID=<uid>`) around the
//     attempts, which is where pam_systemd's "Failed to create session:
//     Connection timed out" and sshd's own messages land.
//
// The gatherer runs DETACHED from the caller's context (context.WithoutCancel)
// so an expired request deadline cannot hollow out the very state that caused
// the failure, and it is bounded (sessionProbeDiagnosticsTimeout) so a wedged
// logind cannot extend the failure report.

// sessionProbeDiagnosticsRunner executes the diagnostics commands. Package
// seam (same type and production impl as userManagerRunner) so unit tests
// never touch a real journal or logind.
var sessionProbeDiagnosticsRunner systemRunner = runSystemCmd

// sessionProbeDiagnosticsTimeout bounds the whole diagnostics gather.
var sessionProbeDiagnosticsTimeout = 5 * time.Second

// sessionProbeJournalLines is how many journal lines the gatherer reads back
// before filtering.
var sessionProbeJournalLines = "30"

// sessionProbeDiagnostics returns a one-line, bounded summary of the session
// state for username, or "" when nothing could be read. It never fails the
// caller: diagnostics are attribution, not verification.
func sessionProbeDiagnostics(ctx context.Context, username string) string {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionProbeDiagnosticsTimeout)
	defer cancel()

	parts := make([]string, 0, 2)

	// logind's own record for the account: an absent record on a user that
	// exists is itself the finding (no session was ever created).
	if out, err := sessionProbeDiagnosticsRunner(dctx, "loginctl", "show-user", username,
		"--property=Name,Id,State,Linger"); err == nil || len(out) > 0 {
		if record := condenseJournal(string(out)); record != "" {
			parts = append(parts, "logind record: "+record)
		}
	}

	// journalctl _UID=<uid> — the AC2 requirement, and where pam_systemd's
	// own failure lands. The uid is resolved through the same seam the spawn
	// path uses; an unresolvable user still gets an unscoped tail.
	args := []string{"--no-pager", "-o", "short-precise", "-n", sessionProbeJournalLines}
	if u, err := userLookup(username); err == nil && u.Uid != "" {
		args = append(args, "_UID="+u.Uid)
	}
	if out, err := sessionProbeDiagnosticsRunner(dctx, "journalctl", args...); err == nil || len(out) > 0 {
		if excerpt := sessionProbeJournalExcerpt(string(out), username); excerpt != "" {
			parts = append(parts, "journalctl: "+excerpt)
		}
	}

	return strings.Join(parts, "; ")
}

// sessionProbeJournalExcerpt reduces a journalctl tail to the lines that
// classify a session failure: the agent user's own lines, pam_systemd's, and
// explicit session-creation failures. Host command-audit noise (snoopy) is
// dropped — it is long, unrelated, and was exactly the kind of line that
// buried the useful evidence on bunker-mvp. At most 4 lines survive, each
// condensed and bounded, so a failure report stays readable.
func sessionProbeJournalExcerpt(raw, username string) string {
	const maxLines = 4
	kept := make([]string, 0, maxLines)
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.Contains(trimmed, "snoopy[") {
			continue
		}
		if !strings.Contains(trimmed, username) &&
			!strings.Contains(trimmed, "pam_systemd") &&
			!strings.Contains(trimmed, "Failed to create session") {
			continue
		}
		kept = append(kept, condenseJournal(trimmed))
		if len(kept) == maxLines {
			break
		}
	}
	return strings.Join(kept, " | ")
}
