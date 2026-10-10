// Package agent manages agent lifecycle: create users, generate SSH keys, start dockerd.
package agent

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/deployBunker/bunker/internal/registry"
)

// ── DF-BUNKER-63: spawn-side uid-collision precheck, destroy --force exit
// hatch, and TTL-reaper destroy-refusal backoff ─────────────────────────────
//
// On a host that also runs host-docker containers, spawn can hand a new agent
// a uid an UNRELATED container process still runs as (the field case: agent
// fad4b89a spawned as uid 1001 while a production container from a previous,
// orphaned agent of the same uid kept running). The isolation promise — a
// per-agent Linux user — is broken by same-uid signal privilege over that
// foreign process, and destroy correctly refuses to userdel the uid while it
// owns live processes, so the TTL reaper then loops forever: expired →
// destroy refused → repeat every minute. The three limbs:
//
//  1. spawn verifies the freshly created user owns NO live process before
//     reporting ready (create-then-verify-then-rollback — the agent itself
//     has no processes yet, so ANY hit is foreign) and fails with a named
//     stage error listing the colliding pids;
//  2. `destroy --force` bypasses the live-process gate by killing the uid's
//     foreign processes with a bounded SIGTERM → SIGKILL escalation (archive
//     home first, then userdel — unchanged order); without force today's
//     refusal semantics are byte-identical;
//  3. a refused destroy is recorded on the durable registry record (GAP-070)
//     and the reaper backs off exponentially (capped at 1h) instead of
//     retrying every minute, while list/status surface the refusal.

// StageUIDCollision is the named spawn stage a uid-collision precheck fails
// in (DF-BUNKER-63). The stage vocabulary is pinned by
// TestSpawnStagesCoverNamedStages; this constant is added to that table's
// coverage so a rename cannot silently break operator tooling that greps the
// journal or the error text.
const StageUIDCollision = "uid-collision"

// spawnProcessScanner is the seam behind the spawn collision precheck: given
// a uid it returns every live process owned by that uid. The package variable
// is the test seam the DF-BUNKER-63 table-driven unit test stubs (no root, no
// /proc writes); production code never swaps it. It reuses the destroy gate's
// /proc probe (listUserProcesses) as its backing implementation.
var spawnProcessScanner = func(uid uint32) ([]userProcess, error) {
	return listUserProcesses(uid)
}

// gateForceKillRunner executes one signal command of the destroy --force
// escalation (kill -TERM / kill -KILL). Package-level seam mirroring
// spawnRollbackRunner: tests inject a recorder instead of signalling anything
// on the machine running the tests; production code never swaps it.
var gateForceKillRunner systemRunner = runSystemCmd

// forceKillGrace is how long the --force escalation waits for SIGTERM'd
// processes to exit before sending SIGKILL. Package var (not const) as the
// same test seam pattern as terminateUserManagerGrace.
var forceKillGrace = 5 * time.Second

// forceKillPollInterval bounds the SIGTERM wait's polling granularity.
const forceKillPollInterval = 100 * time.Millisecond

// forceKillSelfExclusionSlack is the pid window around the destroy's own
// process that the escalation never signals. The kernel wraps pids around
// pid_max, so a recycled pid equal to our own cannot be excluded by identity
// alone; the whole wrap window around os.Getpid() is excluded and later
// re-checked for liveness (a genuinely foreign process there merely survives
// the kill sweep, which the post-sweep probe reports honestly).
const forceKillSelfExclusionSlack = 32768

// uidCollisionRefusal is the spawn precheck's refusal (DF-BUNKER-63 limb 1)
// carried as a distinct TYPE. The message is rendered by the shared
// spawnUIDCollisionMessage below, so the operator-facing text is byte-identical
// to what the plain error has always produced — the type exists so the spawn's
// bounded uid-candidate walk (INT-CI-044) can tell a RETRYABLE uid collision
// from an UNOBSERVABLE scan (which must fail closed on the spot) without
// parsing error text.
type uidCollisionRefusal struct {
	username string
	uid      uint32
	procs    []userProcess
}

func (e *uidCollisionRefusal) Error() string {
	return spawnUIDCollisionMessage(e.username, e.uid, e.procs)
}

// spawnUIDCollisionMessage renders the operator-facing refusal text for a
// collided uid. It is the single construction for that text: the refusal error,
// the precheck's log line and the retry classifier all read from here.
func spawnUIDCollisionMessage(username string, uid uint32, procs []userProcess) string {
	summary := (&orphanUIDCheck{UID: uid, UIDKnown: true, UserExists: true, Processes: procs}).Describe()
	return fmt.Sprintf(
		"uid collision: user %s was assigned uid %d, which already owns live processes on this host. %s. "+
			"Refusing to spawn an agent into a uid that carries same-uid signal privilege over foreign processes "+
			"(DF-BUNKER-63); the just-created user was rolled back. Retrying the spawn will fail again until the "+
			"foreign processes exit or the uid range is recycled past them",
		username, uid, summary)
}

// buildSpawnUIDCollisionError renders the operator-facing stage error for a
// collided uid (DF-BUNKER-63). It is the single construction for the spawn
// precheck's error, so the returned error, its message and the retry classifier
// cannot drift.
func buildSpawnUIDCollisionError(username string, uid uint32, procs []userProcess) error {
	return &uidCollisionRefusal{username: username, uid: uid, procs: procs}
}

// ── INT-CI-044: bounded uid-candidate retry for the spawn precheck ──────────
//
// CI run 36198102167: spawn conctest-0-87636 was assigned uid 1029 — the
// LOWEST free uid in the passwd database — which is exactly the uid a LEAKED
// CI-residue user was still holding (bunker-b430959d: passwd entry long gone,
// its systemd --user manager and python process still running). The precheck
// refused correctly; the spawn then FAILED at the named uid-collision stage,
// and TestConcurrency_SpawnFiveAgents reads that as a spawn failure. The same
// code was 5/5 green in adjacent runs, because the trigger is host state the
// runner mutates between runs. Refusing to hand out that uid is right; failing
// the spawn over residue it can step over is not.
//
// The spawn therefore retries CANDIDATE SELECTION, bounded. The refusal itself
// is untouched:
//
//   - a uid that carries foreign live processes is SKIPPED, never handed out;
//   - an UNOBSERVABLE scan (the process scanner itself failed) still fails
//     closed IMMEDIATELY — it is not a collision and is never retried;
//   - a PRE-EXISTING agent user (the idempotent re-registration path) is never
//     touched: only a user THIS spawn created may be released for a retry;
//   - an exhausted walk still fails loudly with the DF-BUNKER-63 refusal naming
//     the colliding pids, at the same stage, wrapped with the candidates tried.
//
// Retrying is safe because every new candidate goes through the same scan
// before the spawn proceeds: the walk only ever advances PAST a refused uid.

// uidCollisionCandidateRetries bounds how many ADDITIONAL uid candidates the
// spawn tries after the first one is refused (so at most
// 1+uidCollisionCandidateRetries useradd attempts per spawn). A var, not a
// const, purely as a test seam (the containerCapAttempts convention);
// production never writes it.
var uidCollisionCandidateRetries = 5

// spawnFreshUserReleaseTimeout bounds the single `userdel -rf` that releases a
// just-created candidate user so the walk can bind another uid. A var, not a
// const, purely as a test seam; production never writes it.
var spawnFreshUserReleaseTimeout = 30 * time.Second

// nextUIDCandidate returns the uid the walk pins after uid was refused: the
// next uid in the space, so a refused candidate can never be revisited.
// ok=false at the top of the uint32 range — there is no next candidate.
func nextUIDCandidate(uid uint32) (uint32, bool) {
	if uid == math.MaxUint32 {
		return 0, false
	}
	return uid + 1, true
}

// isUseraddUIDTaken reports whether a failed `useradd -u <uid>` refused the uid
// because an ACCOUNT already holds it ("UID 1030 is not unique"): a candidate
// the walk must skip, never a spawn failure. Matched by message because useradd
// reports it with no distinguishing exit status — the same convention the
// idempotent "already exists" reuse at the spawn site already relies on.
func isUseraddUIDTaken(out []byte) bool {
	return strings.Contains(string(out), "is not unique")
}

// uidCandidatesExhaustedError wraps the LAST refusal (a collision refusal, or a
// pinned candidate the host already uses) with the candidate walk that ran out
// (INT-CI-044). A DF-BUNKER-63 refusal rides inside it unchanged — colliding
// pids, uid, rationale — and the operator additionally learns which uids were
// tried before the spawn gave up.
func uidCandidatesExhaustedError(last error, candidates []uint32) error {
	return fmt.Errorf("uid-candidate walk exhausted after %d refused candidate(s) %v: %w",
		len(candidates), candidates, last)
}

// removeFreshAgentUser releases the user THIS spawn just created so the walk can
// create it again bound to another uid (INT-CI-044).
//
// It runs `userdel -rf` ON PURPOSE: the refused uid is owned by FOREIGN live
// processes, so userdel's busy check (uid-based — shadow's lib/user_busy.c scans
// /proc for the uid) matches them, and a plain `userdel -r` exits E_USER_BUSY
// with "user <name> is currently used by process <pid>" WITHOUT deleting
// anything. `-f` lets the deletion proceed, and it is safe here because the only
// account removed is the one this spawn created moments ago (empty home, no
// processes of its own) — userdel never signals a process, so the foreign
// processes the refusal protects are left exactly as they were. Note that
// `usermod -u` is NOT an alternative: shadow's usermod exits E_USER_BUSY on a
// busy user and has no force escape.
//
// The context is detached and bounded: releasing the candidate is part of the
// spawn, not of the caller's request, and it must not be able to run away
// (spawnRollbackRunner is the seam every spawn-side compensating command uses).
func removeFreshAgentUser(ctx context.Context, username string) error {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spawnFreshUserReleaseTimeout)
	defer cancel()
	out, err := spawnRollbackRunner(releaseCtx, userManagementCommand("userdel"), "-rf", username)
	if err != nil {
		return fmt.Errorf("userdel -rf %s: %w (output: %s)", username, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// checkSpawnUIDCollision is the create-then-verify-then-rollback precheck
// (DF-BUNKER-63 limb 1). It runs AFTER useradd succeeded and BEFORE the spawn
// proceeds to any further stage: the agent itself has no processes yet, so
// ANY live process under the new uid is foreign — a host-docker container, a
// leftover daemon, anything — and handing the uid to the agent would grant it
// same-uid signal privilege over that process (kill -0, signals, /proc reads).
//
// Fail closed: on a collision the caller rolls the just-created user back via
// the standard rollback machinery and the spawn fails with a named stage
// error listing the colliding pids and their cmdlines. The probe itself is
// seam-isolated (spawnProcessScanner); a scanner error FAILS the spawn too —
// "cannot look" must never read as "nothing there" on a security precheck.
// username is passed for error rendering only; the uid comes from the caller,
// which just looked the user up for the isolation stage.
func (m *AgentManager) checkSpawnUIDCollision(ctx context.Context, username string, uid uint32) error {
	procs, err := spawnProcessScanner(uid)
	if err != nil {
		return fmt.Errorf("uid collision precheck for %s (uid %d) could not scan live processes (fail closed): %w",
			username, uid, err)
	}
	if len(procs) == 0 {
		return nil
	}
	err = buildSpawnUIDCollisionError(username, uid, procs)
	m.logger.Error("spawn uid collision precheck failed; rolling back the just-created user",
		"username", username,
		"uid", uid,
		"processes", len(procs),
		"evidence", (&orphanUIDCheck{UID: uid, UIDKnown: true, UserExists: true, Processes: procs}).Describe(),
	)
	return err
}

// reapUIDProcessesBounded terminates every process owned by uid with a
// BOUNDED escalation: SIGTERM to all, wait up to forceKillGrace for exits,
// SIGKILL to the survivors (DF-BUNKER-63 limb 2). The destroy's own process
// (and the kernel's pid-wrap window around it) is excluded — the escalation
// must never signal the destroy that is running it. The kill list is logged:
// every signalled pid, with its command head, at Info, and the survivors
// after SIGKILL at Warn. A liveness re-probe decides the escalation's
// outcome, not the exit status of the kill commands. scope is the log
// prefix identifying the caller ("destroy --force" for the operator hatch,
// "destroy userdel retry" for the QA-BUNKER-61 straggler reap) — the journal
// must never mislabel a non-forced teardown as a forced one. It reports
// nothing itself — the caller decides how the final state is logged
// (reportUIDKillVerdict).
//
// Callers: the destroy --force hatch (killUserProcessesForce) and, since
// QA-BUNKER-61, the bounded straggler reap before the single userdel retry
// (killUserProcessesForDestroyRetry). The non-force destroy gate itself
// still refuses without any kill pass — that semantics is unchanged.
func (m *AgentManager) reapUIDProcessesBounded(ctx context.Context, scope, username string, uid uint32, procs []userProcess) {
	self := os.Getpid()
	excluded := func(pid int) bool {
		d := pid - self
		if d < 0 {
			d = -d
		}
		return d <= forceKillSelfExclusionSlack
	}
	signal := func(procs []userProcess, sig string) {
		for _, p := range procs {
			if excluded(p.PID) {
				continue
			}
			m.logger.Info(scope+" signalling uid process",
				"username", username, "uid", uid, "pid", p.PID, "signal", sig, "cmd", p.Cmd)
			if _, err := gateForceKillRunner(ctx, "kill", "-"+sig, strconv.Itoa(p.PID)); err != nil {
				m.logger.Warn(scope+" signal failed (process may have already exited)",
					"username", username, "pid", p.PID, "signal", sig, "error", err)
			}
		}
	}

	// Round 1: SIGTERM everyone (except our own pid window), then wait the
	// bounded grace for exits.
	signal(procs, "TERM")
	deadline := time.Now().Add(forceKillGrace)
	var survivors []userProcess
	for {
		remaining, err := spawnProcessScanner(uid)
		if err != nil {
			// The re-probe failed: cannot prove the uid is clear. Fall
			// through to the SIGKILL round over the ORIGINAL list — the
			// escalation errs toward completing the forced teardown, and the
			// post-sweep probe at the end reports the honest final state.
			m.logger.Warn("destroy --force could not re-probe uid processes during the grace wait; escalating to SIGKILL over the original list",
				"username", username, "uid", uid, "error", err)
			survivors = procs
			break
		}
		survivors = nil
		for _, p := range remaining {
			if !excluded(p.PID) {
				survivors = append(survivors, p)
			}
		}
		if len(survivors) == 0 || !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			survivors = remaining
		case <-time.After(forceKillPollInterval):
		}
	}

	// Round 2: SIGKILL the survivors.
	if len(survivors) > 0 {
		signal(survivors, "KILL")
		// Give the SIGKILLs a short bounded moment to land before the final
		// verdict probe.
		waitDeadline := time.Now().Add(forceKillGrace)
		for time.Now().Before(waitDeadline) {
			remaining, err := spawnProcessScanner(uid)
			if err != nil {
				break // the final probe below reports the honest state
			}
			clear := true
			for _, p := range remaining {
				if !excluded(p.PID) {
					clear = false
					break
				}
			}
			if clear {
				break
			}
			select {
			case <-ctx.Done():
				goto verdict
			case <-time.After(forceKillPollInterval):
			}
		}
	}

verdict:
	m.reportUIDKillVerdict(username, uid)
}

// reportUIDKillVerdict logs the escalation's outcome: whatever the uid still
// owns after the full TERM→KILL pass, with the destroy's own pid window
// excluded. Shared by the --force hatch (QA-BUNKER-61 rework) and the
// userdel-retry reap wrapper so both report the same evidence vocabulary.
func (m *AgentManager) reportUIDKillVerdict(username string, uid uint32) {
	final, err := spawnProcessScanner(uid)
	if err != nil {
		m.logger.Warn("destroy --force final liveness probe failed; surviving state unobservable",
			"username", username, "uid", uid, "error", err)
		return
	}
	self := os.Getpid()
	var stillAlive []userProcess
	for _, p := range final {
		if d := p.PID - self; d > forceKillSelfExclusionSlack || d < -forceKillSelfExclusionSlack {
			stillAlive = append(stillAlive, p)
		}
	}
	if len(stillAlive) > 0 {
		m.logger.Warn("destroy --force: uid processes survived the SIGTERM/SIGKILL escalation",
			"username", username, "uid", uid,
			"evidence", (&orphanUIDCheck{UID: uid, UIDKnown: true, UserExists: true, Processes: stillAlive}).Describe())
		return
	}
	m.logger.Info("destroy --force cleared the uid's processes",
		"username", username, "uid", uid)
}

// killUserProcessesForce terminates the uid's processes with the bounded
// SIGTERM → SIGKILL escalation (DF-BUNKER-63 limb 2), logged under the
// destroy --force scope.
func (m *AgentManager) killUserProcessesForce(ctx context.Context, username string, uid uint32, procs []userProcess) {
	m.reapUIDProcessesBounded(ctx, forceKillScope, username, uid, procs)
	m.reportUIDKillVerdict(username, uid)
}

// killUserProcessesForDestroyRetry is the NON-force reap the userdel retry
// runs (QA-BUNKER-61): the same bounded SIGTERM → SIGKILL escalation as the
// --force hatch, logged under retry-specific wording so the journal
// distinguishes an operator-forced teardown from a straggler reap before a
// single retry. The final verdict goes through the shared reporter — the
// surviving-process evidence is ON THE RECORD either way.
func (m *AgentManager) killUserProcessesForDestroyRetry(ctx context.Context, username string, uid uint32, procs []userProcess) {
	m.logger.Info("destroy userdel retry: reaping uid processes still holding the agent",
		"username", username, "uid", uid, "processes", len(procs))
	m.reapUIDProcessesBounded(ctx, retryReapScope, username, uid, procs)
	m.reportUIDKillVerdict(username, uid)
}

// forceKillScope is the destroy --force escalation's log scope: every line
// the TERM→KILL pass emits carries it so the journal always shows an
// operator-forced teardown as forced (the guard string forceKillMarker is
// built from it and pinned by tests).
const forceKillScope = "destroy --force"

// retryReapScope is the QA-BUNKER-61 straggler reap's log scope: the same
// escalation before the single userdel retry, but distinguishable in the
// journal from an operator-forced teardown.
const retryReapScope = "destroy userdel retry"

// isDestroyRefusalStatus reports whether a destroy-path response status is a
// REFUSAL the reaper must back off from (DF-BUNKER-63): the destroy deleted
// nothing and a blind retry every minute would loop forever. QA-BUNKER-61
// adds StatusUserdelFailed: after the agent is unregistered a userdel
// failure leaks the user + home, and the reaper must come back for them
// instead of the agent silently vanishing from view.
func isDestroyRefusalStatus(status string) bool {
	switch status {
	case StatusLiveProcesses, StatusHomeRetained, StatusUserdelFailed:
		return true
	}
	return false
}

// backoffForAttempts returns the reaper's retry delay after `attempts`
// recorded refusals: exponential from one minute, capped at one hour.
// attempts 1 → 1m (the next tick), 2 → 2m, 3 → 4m … capped at 60m.
func backoffForAttempts(attempts int) time.Duration {
	const (
		base    = time.Minute
		maxWait = time.Hour
	)
	if attempts < 1 {
		return base
	}
	wait := base
	for i := 1; i < attempts; i++ {
		wait *= 2
		if wait >= maxWait {
			return maxWait
		}
	}
	if wait > maxWait {
		return maxWait
	}
	return wait
}

// recordDestroyRefusal folds a destroy refusal into the agent's durable
// registry record (DF-BUNKER-63 limb 3). Attempts accumulate across restarts
// because the state is durable (GAP-070); the registry write failure is
// logged, never fatal — the destroy refusal itself already happened and the
// next refused attempt will try the append again. Without a registry only
// the in-memory tracker status below marks the agent.
func (m *AgentManager) recordDestroyRefusal(agentID, status string, cause error) {
	if !isDestroyRefusalStatus(status) {
		// The recorder is only for refusals; anything else is a caller bug
		// and must not write durable state.
		m.logger.Error("refusing to record a non-refusal destroy status", "agent_id", agentID, "status", status)
		return
	}
	if m.registry == nil {
		// Registry disabled or unavailable: the durable half of the backoff
		// cannot exist, but the tracker status below still surfaces the
		// refusal for this daemon's lifetime.
		m.logger.Warn("destroy refusal not durably recorded (registry unavailable); marking the tracker status only",
			"agent_id", agentID, "status", status)
	} else {
		prev := m.registry.RefusalOf(agentID)
		refusal := &registry.Refusal{
			Status:        status,
			Attempts:      1,
			LastError:     "",
			LastAttemptAt: time.Now().UTC().Format(time.RFC3339),
		}
		if prev != nil {
			refusal.Attempts = prev.Attempts + 1
			if prev.Status != "" {
				refusal.Status = prev.Status
			}
		}
		if cause != nil {
			refusal.LastError = cause.Error()
		}
		if err := m.registry.AppendRefusal(agentID, refusal); err != nil {
			m.logger.Warn("registry refusal append failed", "agent_id", agentID, "error", err)
		}
	}
	// Surface the state on the live tracker record so list/status show the
	// agent as destroy-refused rather than running-forever. The status keeps
	// the wire vocabulary's "destroy-refused:" prefix distinct from the
	// destroy-path response statuses (which name what the destroy refused to
	// DO), and carries the retry delay for operators.
	wait := backoffForAttempts(m.refusalAttempts(agentID))
	m.tracker.UpdateStatus(agentID, "destroy-refused:"+status)
	m.logger.Warn("destroy refused; recording refusal for reaper backoff",
		"agent_id", agentID,
		"status", status,
		"attempts", m.refusalAttempts(agentID),
		"next_retry_in", wait.String(),
		"error", cause,
	)
}

// refusalAttempts reads the accumulated attempt count for agentID (0 when
// none is recorded or the registry is unavailable).
func (m *AgentManager) refusalAttempts(agentID string) int {
	if m.registry == nil {
		return 0
	}
	if refusal := m.registry.RefusalOf(agentID); refusal != nil {
		return refusal.Attempts
	}
	return 0
}

// reaperBackoffRemaining reports how much longer the reaper must wait before
// retrying agentID's destroy, based on the durable refusal record. Zero means
// the agent may be reaped now (no refusal, no registry, or the backoff has
// elapsed).
func (m *AgentManager) reaperBackoffRemaining(agentID string) time.Duration {
	if m.registry == nil {
		return 0
	}
	refusal := m.registry.RefusalOf(agentID)
	if refusal == nil || refusal.LastAttemptAt == "" {
		return 0
	}
	last, err := time.Parse(time.RFC3339, refusal.LastAttemptAt)
	if err != nil {
		// An unparseable timestamp must not wedge the reaper into silence:
		// treat the refusal as fresh (one full backoff window from now).
		return backoffForAttempts(refusal.Attempts)
	}
	elapsed := time.Since(last)
	wait := backoffForAttempts(refusal.Attempts)
	if elapsed >= wait {
		return 0
	}
	return wait - elapsed
}

// refusalSurfaces is the test-visible shape of the refusal state the CLI's
// list/info read (kept here so the manager owns the vocabulary).
type refusalSurfaces struct {
	Status   string
	Attempts int
	Wait     time.Duration
}

// agentRefusalSurfaces reads the durable refusal state for surfacing.
func (m *AgentManager) agentRefusalSurfaces(agentID string) *refusalSurfaces {
	if m.registry == nil {
		return nil
	}
	refusal := m.registry.RefusalOf(agentID)
	if refusal == nil {
		return nil
	}
	return &refusalSurfaces{
		Status:   refusal.Status,
		Attempts: refusal.Attempts,
		Wait:     backoffForAttempts(refusal.Attempts),
	}
}

// guard strings keep the log-vocabulary greppable (the repo's established
// pattern for test-pinned markers).
const (
	spawnUIDCollisionMarker = "spawn uid collision precheck failed"
	forceKillMarker         = "destroy --force signalling uid process"
	refusalBackoffMarker    = "destroy refused; recording refusal for reaper backoff"
)
