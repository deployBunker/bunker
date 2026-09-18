package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Spawn stages, in execution order. Every failure return from Spawn must
// carry the stage that was executing so an operator can attribute a failure
// (INT-CI-005: the CI hang surfaced as a bare context timeout with no stage
// name, which made the half-created agent unattributable).
const (
	StageValidate           = "validate"
	StageCapacity           = "capacity"
	StagePortAlloc          = "port-alloc"
	StageUserCreate         = "user-create"
	StageIsolationProvision = "isolation-provision"
	StageKeygen             = "keygen"
	StageAuthorizedKeys     = "authorized-keys"
	StageRootlessInstall    = "rootless-install"
	StageDockerdStart       = "dockerd-start"
	StageContainerCap       = "container-cap"
	StageImageBuild         = "image-build"
	StageSliceLimits        = "slice-limits"
	StageSessionProbe       = "session-probe"
	StageRegister           = "register"
)

// ── the rollback budget (DF-BUNKER-21) ──────────────────────────────────────
//
// The compensating actions of a failed spawn must never borrow the request
// deadline: by the time the rollback runs the request context is usually
// already cancelled (INT-CI-005: the 300s server request timeout), and an exec
// on a cancelled context dies instantly — the half-created agent's user, home
// and linger entry were left behind.
//
// Detaching alone was not enough. The rollback used ONE detached 60s context
// for every step in sequence, and exec.CommandContext both REFUSES to start a
// command on an already-expired context and KILLS a running one when its
// context expires. A single slow step therefore consumed the shared budget and
// every step after it was handed a DEAD context and silently did nothing:
//
//	QA-BUNKER-19 (bunker-las-03, 2026-09-18): spawn failed 2/2 while the
//	rootless installer was downloading ~93MB at ~1.7MB/s, the request deadline
//	expired, and the host was left with 11 orphan bunker-* users, 0 registered
//	agents and ~2.5GB under /home.
//
// The budget below is the ONE authority that hands out the context for every
// compensating action. Each step gets a FRESH context that is
//
//   - detached from the request cancellation (a dead request can never cancel
//     the rollback),
//   - bounded on its own (rollbackStepTimeout), so one slow step — a blocking
//     `systemctl stop`/`loginctl terminate-user` against a user manager that a
//     recycled uid keeps resurrecting, a slow `systemctl daemon-reload` — cannot
//     run away with the whole rollback, and
//   - never already expired: once the anchor is spent, a step still gets the
//     reserved rollbackStepFloor, so the critical late steps — `userdel -r`
//     above all — are ATTEMPTED and report their outcome instead of no-op'ing.
//
// The whole rollback is bounded by
// spawnRollbackTimeout + (number of compensating steps) * rollbackStepFloor,
// a small fixed bound because the step list is finite.

// The three budgets are vars (not consts) purely as a test seam: the
// regression suite shrinks them to milliseconds (see shrinkRollbackBudgets).
// Production never writes them.
var (
	// spawnRollbackTimeout anchors the whole rollback: while it lasts, every
	// compensating step may use up to rollbackStepTimeout.
	spawnRollbackTimeout = 60 * time.Second
	// rollbackStepTimeout caps ONE compensating step so it cannot consume the
	// whole rollback and starve the steps after it.
	rollbackStepTimeout = 15 * time.Second
	// rollbackStepFloor is the reserved budget a step gets once the anchor is
	// spent. Setting it to zero would restore the DF-BUNKER-21 leak, where
	// every step after a budget-consuming one was handed a dead context and
	// silently did nothing — userdel included.
	rollbackStepFloor = 5 * time.Second
)

// rollbackBudget hands out one context per compensating action.
type rollbackBudget struct {
	// request is the (usually already cancelled) request context. It is used
	// as a VALUE carrier only: every step context is derived through
	// context.WithoutCancel, which also strips an already-expired deadline.
	request  context.Context
	deadline time.Time
	now      func() time.Time

	// stepped counts the steps handed out; it is what the reserved-floor note
	// names, so an operator can see how far into the rollback the anchor was
	// spent.
	stepped int
}

func newRollbackBudget(requestCtx context.Context) *rollbackBudget {
	return &rollbackBudget{
		request:  requestCtx,
		deadline: time.Now().Add(spawnRollbackTimeout),
		now:      time.Now,
	}
}

// step hands out the context for ONE compensating action, plus whether that
// step had to run on the reserved floor (the anchor was already spent). The
// returned context is never already-done, even when the request context was
// cancelled or its deadline long expired.
func (b *rollbackBudget) step() (context.Context, context.CancelFunc, bool) {
	b.stepped++
	budget := rollbackStepTimeout
	floored := false
	if remaining := b.deadline.Sub(b.now()); remaining < budget {
		if remaining < rollbackStepFloor {
			budget, floored = rollbackStepFloor, true
		} else {
			budget = remaining
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(b.request), budget)
	return ctx, cancel, floored
}

// runStep is the ONE way a compensating action runs: a fresh budget step, a
// breadcrumb note when the anchor was exhausted (so an operator can see WHY a
// late step ran on the floor), and the action's own outcome recorded by the
// action itself.
func (b *rollbackBudget) runStep(action string, res *rollbackResult, fn func(ctx context.Context)) {
	ctx, cancel, floored := b.step()
	defer cancel()
	if floored {
		res.note(fmt.Sprintf("rollback budget exhausted (anchor %s, step %d): %s ran on the reserved %s floor",
			spawnRollbackTimeout, b.stepped, action, rollbackStepFloor))
	}
	fn(ctx)
}

// spawnFailureBreadcrumbPath is where failed/cancelled spawns append their
// structured breadcrumb. It is a package-level var so tests can point it at
// a t.TempDir() file; production appends to /var/lib/bunkerd/, the same
// directory the durable registry (GAP-070) already owns.
var spawnFailureBreadcrumbPath = "/var/lib/bunkerd/spawn-failures.jsonl"

// spawnBreadcrumb is one JSON line describing a failed or cancelled spawn.
type spawnBreadcrumb struct {
	Timestamp string   `json:"timestamp"`
	AgentID   string   `json:"agent_id"`
	Stage     string   `json:"stage"`
	CtxErr    string   `json:"context_error,omitempty"`
	Error     string   `json:"error"`
	Ran       []string `json:"rollback_ran,omitempty"`
	Failed    []string `json:"rollback_failed,omitempty"`
	// Notices carry what is neither an action nor a failure: today, the
	// reserved-floor notes a step records when the rollback anchor was already
	// spent (DF-BUNKER-21). They are separate from Ran so "which compensating
	// actions ran" stays a precise list.
	Notices []string `json:"rollback_notices,omitempty"`
}

// appendSpawnBreadcrumb appends one JSON line to spawnFailureBreadcrumbPath.
// It is intentionally best-effort: a breadcrumb write failure must never fail
// the spawn (the error the caller receives is about the agent, not about
// diagnostics), so the caller logs a warning instead.
func appendSpawnBreadcrumb(b spawnBreadcrumb) error {
	if b.Timestamp == "" {
		b.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("marshal spawn breadcrumb: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(spawnFailureBreadcrumbPath), 0o755); err != nil {
		return fmt.Errorf("create spawn-failure dir: %w", err)
	}
	f, err := os.OpenFile(spawnFailureBreadcrumbPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open spawn-failure journal: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("append spawn breadcrumb: %w", err)
	}
	return nil
}

// writeSpawnFailureBreadcrumb appends the breadcrumb and logs a warning when
// the journal cannot be written — never an error that could reach the spawn
// result.
func writeSpawnFailureBreadcrumb(logger *slog.Logger, b spawnBreadcrumb) {
	if err := appendSpawnBreadcrumb(b); err != nil {
		logger.Warn("could not append spawn-failure breadcrumb",
			"agent_id", b.AgentID,
			"stage", b.Stage,
			"path", spawnFailureBreadcrumbPath,
			"error", err,
		)
	}
}

// rollbackResult records which compensating actions the rollback closure ran
// and which of those failed, so the failure breadcrumb can show a
// partially-rolled-back agent instead of hiding it.
type rollbackResult struct {
	mu      sync.Mutex
	ran     []string
	failed  []string
	notices []string
}

func (r *rollbackResult) ok(action string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ran = append(r.ran, action)
}

func (r *rollbackResult) err(action string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append(r.failed, action)
}

// note records something that is neither an action nor a failure — today, the
// reserved-floor notes written when the rollback anchor was already spent. It is
// deliberately NOT appended to ran or failed: the breadcrumb's action lists stay
// precise, and an operator reading `rollback_notices` learns that the late steps
// ran on the reserved floor instead of the full step budget.
func (r *rollbackResult) note(note string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, note)
}

// snapshot returns copies of the recorded outcomes.
func (r *rollbackResult) snapshot() (ran, failed []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ran...), append([]string(nil), r.failed...)
}

// noticeSnapshot returns a copy of the recorded notices.
func (r *rollbackResult) noticeSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.notices...)
}

// spawnStageErr wraps a Spawn failure with the stage it died in, and names a
// request-context cancellation explicitly: when the server's chi
// middleware.Timeout (config request_timeout, 300s by default) cancels the
// request ctx mid-spawn, the raw "context canceled / deadline exceeded" tells
// an operator nothing — the wrap says the spawn was cancelled by the server
// request timeout and names the stage it died in. The cause text is preserved
// verbatim (%w) so existing cause-substring matching keeps working.
func spawnStageErr(ctx context.Context, agentID, stage string, cause error) error {
	if cause == nil {
		cause = errors.New("unknown error")
	}
	if agentID == "" {
		agentID = "<unassigned>"
	}
	if ctx.Err() != nil {
		return fmt.Errorf("spawn %s failed at stage %s: spawn was cancelled by the server request timeout (request_timeout, default 300s): %w",
			agentID, stage, cause)
	}
	return fmt.Errorf("spawn %s failed at stage %s: %w", agentID, stage, cause)
}

// ctxErrText renders the request-context error (if any) for the breadcrumb.
func ctxErrText(ctx context.Context) string {
	if err := ctx.Err(); err != nil {
		return err.Error()
	}
	return ""
}

// removeAgentUser is the rollback half of user creation: it clears the host
// state that would block (or outlive) the removal, reaps the user's processes,
// then runs `userdel -r bunker-<id>` and records every outcome. Each step draws
// its own context from the rollback budget (b), so a step that blocks until its
// own context expires cannot leave the steps after it — `userdel` above all —
// holding a dead context: under the pre-DF-BUNKER-21 shared budget that is
// exactly how the orphan user survived a cancelled spawn.
//
// Order is load-bearing and pinned by tests: linger first (logind has no reason
// to restart the manager), then the uid's user manager and session, then the
// user's processes, then userdel. The process reap runs BEFORE the first
// attempt, not only between the two attempts: a spawn cancelled while the
// rootless installer was still downloading leaves that installer's own children
// running as the agent user (exec.CommandContext signals only the direct child),
// and they keep the home directory and the user manager busy.
func removeAgentUser(b *rollbackBudget, agentID string, logger *slog.Logger, res *rollbackResult) {
	username := "bunker-" + agentID
	logger.Warn("rolling back: removing user", "agent_id", agentID, "username", username)

	clearUserStateBeforeUserdel(b, username, logger, res)
	reapUserProcesses(b, username, logger, res)

	err := userdelUnderRollbackBudget(b, username, res)
	if err != nil {
		// Processes owned by the user can keep the first pass from removing
		// the home directory. Terminate them again and retry ONCE — the retry
		// gets its own budget step, so it is a real second chance rather than
		// a re-run on an exhausted context. Bounded at two attempts.
		logger.Warn("userdel failed; terminating user processes and retrying",
			"agent_id", agentID, "error", err)
		reapUserProcesses(b, username, logger, res)
		err = userdelUnderRollbackBudget(b, username, res)
	}
	if err != nil {
		// Only the FINAL outcome is a residual failure: an attempt a retry
		// recovered from stays a note (see userdelUnderRollbackBudget), so
		// `rollback_failed` keeps meaning "what the rollback could not fix".
		res.err("userdel " + username + ": " + err.Error())
		logger.Error("rollback userdel failed; user may be left behind",
			"agent_id", agentID, "username", username, "error", err)
	}
}

// userdelUnderRollbackBudget runs `userdel -r <username>` under its OWN budget
// step and records the outcome. A failing attempt records a NOTE rather than a
// failure — a retry may still recover it, and rollback_failed must stay the
// residual the rollback could not fix (the caller escalates the last attempt).
func userdelUnderRollbackBudget(b *rollbackBudget, username string, res *rollbackResult) error {
	var failure error
	b.runStep("userdel "+username, res, func(ctx context.Context) {
		out, err := spawnRollbackRunner(ctx, "userdel", "-r", username)
		if err != nil {
			failure = fmt.Errorf("userdel %s: %w (output: %s)", username, err, strings.TrimSpace(string(out)))
			res.note("userdel attempt failed: " + failure.Error())
			return
		}
		res.ok("userdel " + username)
	})
	return failure
}

// reapUserProcesses terminates every process owned by username under its own
// budget step and records the outcome. It is the step that clears orphaned
// installer children, so it must not be the step that silently does nothing.
func reapUserProcesses(b *rollbackBudget, username string, logger *slog.Logger, res *rollbackResult) {
	b.runStep("process reap "+username, res, func(ctx context.Context) {
		if err := terminateUserProcesses(ctx, username); err != nil {
			res.err("process reap " + username + ": " + err.Error())
			logger.Warn("could not terminate the agent user's processes", "username", username, "error", err)
			return
		}
		res.ok("process reap " + username)
	})
}

// clearUserStateBeforeUserdel removes the two pieces of host state that block
// or outlive the rollback's `userdel -r` (INT-SPAWN-001), in the required order:
//
//  1. resolve the uid (best effort) — a user that no longer resolves has no uid
//     whose manager could hold it busy, so the rollback goes straight to the
//     existing userdel path;
//  2. `loginctl disable-linger <username>` through the SAME seam Destroy uses
//     (disableLinger — a second loginctl wrapper would be a second place for
//     the flags to drift). spawn enables linger for every agent user and no
//     rollback path ever disabled it: the entry survived the user, so
//     /var/lib/systemd/linger kept holding bunker-regr-alpha, bunker-1dd412d0
//     and bunker-ff86d69a for users that no longer existed;
//  3. stop the uid's user manager (`systemctl stop user@<uid>.service`) and
//     terminate the user's session (`loginctl terminate-user <uid>`). For a uid
//     recycled from a previously destroyed agent this live manager is precisely
//     what makes userdel exit 8 with "currently used by process 2168325";
//  4. the caller's process reap + userdel, each on its own budget step.
//
// Every step is best-effort and recorded in the rollback result: a rollback
// that aborted on a failing loginctl would leave MORE host state behind than
// one that continues, and the operator still needs the failure in the
// breadcrumb.
func clearUserStateBeforeUserdel(b *rollbackBudget, username string, logger *slog.Logger, res *rollbackResult) {
	u, err := lookupUser(username)
	if err != nil {
		logger.Warn("skipping linger/user-manager cleanup before userdel: user does not resolve",
			"username", username, "error", err)
		return
	}

	disableLingerBeforeUserdel(b, username, logger, res)
	stopUserManagerBeforeUserdel(b, u.Uid, username, logger, res)
}

// disableLingerBeforeUserdel disables systemd lingering for the failing agent's
// user through the shared disableLinger seam, under its own budget step, and
// records the outcome. Never fatal: the caller continues to the later steps
// regardless of the result.
func disableLingerBeforeUserdel(b *rollbackBudget, username string, logger *slog.Logger, res *rollbackResult) {
	b.runStep("loginctl disable-linger "+username, res, func(ctx context.Context) {
		out, err := disableLinger(ctx, username)
		if err != nil {
			res.err("loginctl disable-linger " + username + ": " + err.Error())
			logger.Warn("loginctl disable-linger failed during spawn rollback (continuing to userdel)",
				"username", username, "error", err, "output", string(out))
			return
		}
		res.ok("loginctl disable-linger " + username)
		logger.Info("disabled linger during spawn rollback", "username", username)
	})
}

// stopUserManagerBeforeUserdel stops the systemd user manager for uid and
// terminates the user's session, so a live (possibly FOREIGN, from a recycled
// uid) manager cannot keep the username busy for userdel. Each call gets its own
// budget step and goes through the userManagerRunner seam — the same one
// resetUserManagerState uses for these exact commands. Never fatal: the outcome
// is recorded and the caller proceeds.
func stopUserManagerBeforeUserdel(b *rollbackBudget, uid, username string, logger *slog.Logger, res *rollbackResult) {
	parsed, err := strconv.Atoi(uid)
	if err != nil {
		logger.Warn("skipping user manager stop before userdel: uid is not numeric",
			"username", username, "uid", uid, "error", err)
		return
	}
	unit := userManagerUnitName(parsed)
	uidArg := strconv.Itoa(parsed)

	b.runStep("systemctl stop "+unit, res, func(ctx context.Context) {
		out, err := userManagerRunner(ctx, "systemctl", "stop", unit)
		if err != nil {
			res.err("systemctl stop " + unit + ": " + err.Error())
			logger.Warn("could not stop the user manager before userdel (continuing)",
				"username", username, "unit", unit, "error", err, "output", string(out))
			return
		}
		res.ok("systemctl stop " + unit)
	})

	b.runStep("loginctl terminate-user "+uidArg, res, func(ctx context.Context) {
		out, err := userManagerRunner(ctx, "loginctl", "terminate-user", uidArg)
		if err != nil {
			res.err("loginctl terminate-user " + uidArg + ": " + err.Error())
			logger.Warn("could not terminate the user session before userdel (continuing)",
				"username", username, "uid", uidArg, "error", err, "output", string(out))
			return
		}
		res.ok("loginctl terminate-user " + uidArg)
	})
}

// spawnRollbackRunner executes one compensating host command of the spawn
// rollback (userdel, pkill, pgrep). Package-level seam mirroring systemRunner's
// other users (userManagerRunner, rootHostRunner): a test can inject a fake that
// records the CONTEXT each command was handed — the property DF-BUNKER-21 is
// about, and the one a PATH stub cannot observe (exec.CommandContext refuses to
// start a command whose context is already done, so a dead context shows up as
// "the command never ran" instead of as an error).
var spawnRollbackRunner systemRunner = runSystemCmd

// userProcessReapTimeout / userProcessReapPoll bound the wait for the agent
// user's processes to disappear after the SIGKILL sweep. Bounded on purpose:
// the reap must never be the step that hangs the rollback.
const (
	userProcessReapTimeout = 2 * time.Second
	userProcessReapPoll    = 100 * time.Millisecond
)

// terminateUserProcesses kills every process owned by username (SIGKILL) and
// waits (bounded) until the user is process-free, so the following userdel is
// not blocked by live processes.
//
// A pkill that found nothing to kill is the SUCCESS case here, not a failure:
// pkill exits 1 when no process matched, so the outcome is decided by the
// bounded pgrep wait, not by pkill's exit status (a false failure entry would
// otherwise be written into the breadcrumb for every rollback of a user that had
// no processes left).
func terminateUserProcesses(ctx context.Context, username string) error {
	// explicit `-u <user>` only: this kills exactly the failing agent's
	// processes and nothing else (never a pattern match).
	out, err := spawnRollbackRunner(ctx, "pkill", "-u", username, "-9")
	if err != nil && ctx.Err() != nil {
		// The sweep itself could not run (expired step context, missing
		// binary). Report that instead of waiting for processes that were
		// never signalled.
		return fmt.Errorf("pkill -u %s: %w (output: %s)", username, err, strings.TrimSpace(string(out)))
	}

	deadline := time.Now().Add(userProcessReapTimeout)
	for {
		if _, probeErr := spawnRollbackRunner(ctx, "pgrep", "-u", username); probeErr != nil {
			return nil // pgrep exits non-zero when no process matches: user is process-free
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("processes for %s still alive %s after pkill", username, userProcessReapTimeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s's processes to exit: %w", username, ctx.Err())
		case <-time.After(userProcessReapPoll):
		}
	}
}

// removeUserSliceLimits removes the drop-in directory for the given agent's
// slice and reloads systemd. It is called during agent destroy and during the
// spawn rollback to prevent stale slice config from accumulating. The error
// return lets the rollback closure record the outcome (an unwarned failure
// would leave limits configured for a user that no longer exists).
func removeUserSliceLimits(ctx context.Context, agentID string, logger *slog.Logger) error {
	username := "bunker-" + agentID
	u, err := user.Lookup(username)
	if err != nil {
		logger.Warn("cannot lookup user for slice cleanup", "username", username, "error", err)
		return fmt.Errorf("lookup %s: %w", username, err)
	}
	sliceName := fmt.Sprintf("user-%s.slice", u.Uid)
	dropinDir := filepath.Join("/etc/systemd/system", sliceName+".d")
	if err := os.RemoveAll(dropinDir); err != nil && !os.IsNotExist(err) {
		logger.Warn("failed to remove user slice drop-in", "slice", sliceName, "error", err)
		return fmt.Errorf("remove %s: %w", dropinDir, err)
	}
	logger.Info("removed user slice drop-in", "slice", sliceName)
	cmd := exec.CommandContext(ctx, "systemctl", "daemon-reload")
	if err := cmd.Run(); err != nil { // best-effort reload
		logger.Warn("daemon-reload after slice cleanup failed", "slice", sliceName, "error", err)
	}
	return nil
}
