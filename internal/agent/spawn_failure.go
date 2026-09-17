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

// spawnRollbackTimeout bounds the detached rollback context. The compensating
// actions (userdel, slice-dropin removal, isolation removal) must never borrow
// the request deadline: by the time rollback runs, the request ctx is often
// already cancelled (INT-CI-005: the 300s server request timeout), and an exec
// on a cancelled ctx fails instantly, leaving the half-created agent behind.
const spawnRollbackTimeout = 60 * time.Second

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
	mu     sync.Mutex
	ran    []string
	failed []string
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

// snapshot returns copies of the recorded outcomes.
func (r *rollbackResult) snapshot() (ran, failed []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ran...), append([]string(nil), r.failed...)
}

// rollbackContext derives the context every compensating action must run
// under: DETACHED from the request cancellation (context.WithoutCancel) and
// bounded by its own timeout. A rollback that borrows the request ctx dies
// with it — that is the INT-CI-005 leak: the 300s request timeout cancelled
// the ctx, so userdel/removeIsolation no-op'd and the half-created agent
// (Linux user present, no SSH key, no registry row) was left behind.
func rollbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), spawnRollbackTimeout)
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

// removeAgentUser is the rollback half of user creation: it runs
// `userdel -r bunker-<id>` under the DETACHED rollback context and records
// the outcome. When the first userdel leaves the user behind (classic cause:
// live processes keep the home directory busy), the user's processes are
// terminated and userdel is attempted a second time inside the same detached
// context — a half-removed user is exactly the state INT-CI-005 reddened CI
// with, so a lingering user must be loud AND retried.
func removeAgentUser(ctx context.Context, agentID string, logger *slog.Logger, res *rollbackResult) {
	username := "bunker-" + agentID
	logger.Warn("rolling back: removing user", "agent_id", agentID, "username", username)

	userdel := func() error {
		out, err := exec.CommandContext(ctx, "userdel", "-r", username).CombinedOutput()
		if err != nil {
			return fmt.Errorf("userdel %s: %w (output: %s)", username, err, string(out))
		}
		return nil
	}

	err := userdel()
	if err != nil && ctx.Err() == nil {
		// Processes owned by the user can keep the first pass from removing
		// the home directory. Terminate them, then retry once — still inside
		// the same detached context.
		logger.Warn("userdel failed; terminating user processes and retrying",
			"agent_id", agentID, "error", err)
		if pkillErr := terminateUserProcesses(ctx, username); pkillErr != nil {
			logger.Warn("could not terminate user processes", "agent_id", agentID, "error", pkillErr)
		}
		err = userdel()
	}
	if err != nil {
		res.err("userdel " + username + ": " + err.Error())
		logger.Error("rollback userdel failed; user may be left behind",
			"agent_id", agentID, "username", username, "error", err)
		return
	}
	res.ok("userdel " + username)
}

// terminateUserProcesses kills every process owned by username (SIGKILL) so a
// retrying userdel is not blocked by live processes. Best-effort.
func terminateUserProcesses(ctx context.Context, username string) error {
	if err := exec.CommandContext(ctx, "pkill", "-u", username, "-9").Run(); err != nil {
		return fmt.Errorf("pkill -u %s: %w", username, err)
	}
	// Give the kernel a moment to reap the processes before userdel retries.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := exec.CommandContext(ctx, "pgrep", "-u", username).Run(); err != nil {
			return nil // no process matched: user is process-free
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("processes for %s still alive after pkill", username)
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
