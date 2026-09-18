package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── DF-BUNKER-21: the rollback budget ──────────────────────────────────────
//
// QA-BUNKER-19 (bunker-las-03, 2026-09-18): a spawn failed 2/2 while the
// rootless installer was downloading ~93MB at ~1.7MB/s, the request deadline
// expired, and the host was left holding 11 orphan bunker-* users, 0 registered
// agents and ~2.5GB under /home. The rollback context was already detached
// (INT-CI-005), so the leak had to come from somewhere else: ONE shared 60s
// budget handed to every compensating step in sequence. exec.CommandContext
// never starts a command whose context is already done, so a step that consumed
// the budget — a blocking `systemctl stop`/`loginctl terminate-user` against a
// user manager a recycled uid keeps resurrecting, a slow `systemctl
// daemon-reload` — left every step AFTER it (userdel included) silently doing
// nothing.
//
// The regressions below pin the fix on the three properties that were missing:
// a step context that is never dead (even when the request context is), a step
// budget that cannot be consumed by a sibling step, and a compensating chain
// whose failures are on the record.

// shrinkRollbackBudgets replaces the three rollback budgets for one test. They
// are package-level vars on purpose (see spawn_failure.go): the production
// budgets (60s anchor / 15s step / 5s floor) are far too slow to exercise a step
// that blocks until its own context expires.
func shrinkRollbackBudgets(t *testing.T, total, step, floor time.Duration) func() {
	t.Helper()
	prevTotal, prevStep, prevFloor := spawnRollbackTimeout, rollbackStepTimeout, rollbackStepFloor
	spawnRollbackTimeout, rollbackStepTimeout, rollbackStepFloor = total, step, floor
	restore := func() {
		spawnRollbackTimeout, rollbackStepTimeout, rollbackStepFloor = prevTotal, prevStep, prevFloor
	}
	t.Cleanup(restore)
	return restore
}

// rollbackCommandRecorder is the fake host for the rollback-context
// regressions. It records every compensating command with the CONTEXT it was
// handed and — exactly like the production runner it stands in for — REFUSES to
// start a command whose context is already done. That refusal is what makes "no
// compensating command gets a dead context" a behaviour assertion rather than a
// tautology: under the old shared-budget design the refused list is where
// userdel ended up.
type rollbackCommandRecorder struct {
	mu      sync.Mutex
	started []string
	refused []string

	// blockOn makes a command whose rendered line starts with one of these
	// prefixes hang until its own context expires: the stale-manager state
	// (systemctl/loginctl waiting on a user manager that keeps coming back) and
	// a slow daemon-reload.
	blockOn []string
	blocked int
}

func newRollbackCommandRecorder(blockOn ...string) *rollbackCommandRecorder {
	return &rollbackCommandRecorder{blockOn: blockOn}
}

// checkContext applies the production context policy to one command: a command
// handed a dead context is REFUSED (and recorded as refused), exactly as
// exec.CommandContext refuses to start it. Otherwise the command is recorded as
// started and nil is returned.
func (r *rollbackCommandRecorder) checkContext(ctx context.Context, line string) error {
	if err := ctx.Err(); err != nil {
		r.append(&r.refused, line+" (context: "+err.Error()+")")
		return err
	}
	r.append(&r.started, line)
	return nil
}

// run is the seam body: it applies the context policy, models a scripted hang,
// and otherwise executes the command through the PRODUCTION runner — so the PATH
// stubs the spawn-failure tests already use are the commands that actually run.
func (r *rollbackCommandRecorder) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	if err := r.checkContext(ctx, line); err != nil {
		return nil, err
	}
	if r.shouldBlock(line) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return runSystemCmd(ctx, name, args...)
}

func (r *rollbackCommandRecorder) shouldBlock(line string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.blockOn {
		if p != "" && strings.HasPrefix(line, p) {
			r.blocked++
			return true
		}
	}
	return false
}

func (r *rollbackCommandRecorder) append(dst *[]string, line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*dst = append(*dst, line)
}

func (r *rollbackCommandRecorder) snapshot() (started, refused []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.started...), append([]string(nil), r.refused...)
}

// index returns the position of the first started command with the given
// prefix, or -1 — the ordering assertions read one timeline.
func (r *rollbackCommandRecorder) index(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, line := range r.started {
		if strings.HasPrefix(line, prefix) {
			return i
		}
	}
	return -1
}

// install wires the recorder into every rollback seam and restores them on
// cleanup. The isolation removal goes through the manager's own host seam, so it
// is wrapped too: a dead context must show up there as well.
func (r *rollbackCommandRecorder) install(t *testing.T, m *AgentManager) {
	t.Helper()

	prevRollback := spawnRollbackRunner
	spawnRollbackRunner = r.run
	t.Cleanup(func() { spawnRollbackRunner = prevRollback })

	prevManager := userManagerRunner
	userManagerRunner = r.run
	t.Cleanup(func() { userManagerRunner = prevManager })

	prevLinger := disableLinger
	disableLinger = func(ctx context.Context, username string) ([]byte, error) {
		return r.run(ctx, "loginctl", "disable-linger", username)
	}
	t.Cleanup(func() { disableLinger = prevLinger })

	host := newHostRecorder()
	prevHost := m.hostRunner
	m.hostRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if err := r.checkContext(ctx, strings.TrimSpace("hostsetup "+name+" "+strings.Join(args, " "))); err != nil {
			return nil, err
		}
		return host.run(ctx, name, args...)
	}
	t.Cleanup(func() { m.hostRunner = prevHost })
}

// TestRollbackBudgetStepContextsAreDetachedAndBounded is the unit-level proof of
// the budget contract (AC2/AC3): a step context is LIVE even when the request
// context was cancelled or its deadline long expired, a step never gets more
// than the step budget while the anchor lasts, and once the anchor is spent every
// remaining step still gets the reserved floor instead of a dead context.
func TestRollbackBudgetStepContextsAreDetachedAndBounded(t *testing.T) {
	t.Run("cancelled request still yields live step contexts", func(t *testing.T) {
		requestCtx, cancel := context.WithCancel(context.Background())
		cancel()
		b := newRollbackBudget(requestCtx)
		for i := 0; i < 6; i++ {
			stepCtx, stepCancel, _ := b.step()
			if err := stepCtx.Err(); err != nil {
				t.Fatalf("step %d got a dead context from a cancelled request: %v", i+1, err)
			}
			if _, hasDeadline := stepCtx.Deadline(); !hasDeadline {
				t.Fatalf("step %d context is unbounded", i+1)
			}
			stepCancel()
		}
		if requestCtx.Err() == nil {
			t.Fatal("test premise broken: the request context is not cancelled")
		}
	})

	t.Run("expired request still yields live step contexts", func(t *testing.T) {
		requestCtx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-requestCtx.Done()
		b := newRollbackBudget(requestCtx)
		stepCtx, stepCancel, _ := b.step()
		defer stepCancel()
		if err := stepCtx.Err(); err != nil {
			t.Fatalf("step got a dead context from an expired request: %v", err)
		}
	})

	t.Run("anchor exhaustion hands the later steps the reserved floor", func(t *testing.T) {
		restore := shrinkRollbackBudgets(t, time.Hour, time.Minute, 30*time.Millisecond)
		defer restore()

		b := newRollbackBudget(context.Background())
		// Steps inside the anchor are capped by the step budget.
		ctx, cancel, floored := b.step()
		if floored {
			t.Error("the first step of a fresh budget must not be floored")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > time.Minute+time.Second {
			t.Errorf("step budget exceeds the step cap: %v", deadline)
		}
		cancel()

		// Spend the anchor, then hand out more steps than the rollback has.
		b.deadline = time.Now().Add(-time.Second)
		for i := 0; i < 3; i++ {
			stepCtx, stepCancel, stepFloored := b.step()
			if !stepFloored {
				t.Fatalf("step %d after an exhausted anchor was not reported as floored", i+1)
			}
			if err := stepCtx.Err(); err != nil {
				t.Fatalf("step %d on the reserved floor is dead: %v", i+1, err)
			}
			deadline, ok := stepCtx.Deadline()
			if !ok {
				t.Fatalf("floored step %d context is unbounded", i+1)
			}
			if left := time.Until(deadline); left > rollbackStepFloor+time.Second {
				t.Fatalf("floored step %d got %v, more than the reserved floor %v", i+1, left, rollbackStepFloor)
			}
			stepCancel()
		}
	})
}

// keygenStubBody is an ssh-keygen stand-in that writes the keypair the spawn
// reads back, into the -f path it is handed (the test points TMPDIR inside
// t.TempDir(), so no key material ever lands in the real /tmp).
const keygenStubBody = `keyfile=""
while [ $# -gt 0 ]; do
  case "$1" in
    -f) keyfile="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$keyfile" ] || exit 1
printf 'PRIVATE KEY\n' > "$keyfile"
printf 'ssh-ed25519 AAAATESTKEY bunker\n' > "$keyfile.pub"
exit 0
`

// blockingChownStub is a `chown` stand-in that SIGNALS when it is invoked for the
// agent's socket directory — i.e. inside the rootless stage — and then blocks,
// modelling a spawn whose rootless work is still in flight when the request
// deadline expires. Everything else (the authorized_keys/.profile chowns of the
// earlier stages) succeeds immediately.
func blockingChownStub(sockDir, marker string) string {
	return "if [ \"$2\" = \"" + sockDir + "\" ]; then\n" +
		"  touch " + marker + "\n" +
		// exec: the stub REPLACES itself with sleep, so exactly one process
		// holds the output pipe and the cancellation returns immediately
		// instead of waiting out an orphaned child that inherited it.
		"  exec sleep 30\n" +
		"fi\nexit 0\n"
}

// dfb21Host is the temp "host" the spawn-failure regressions run against: home
// root, run root and TMPDIR all inside t.TempDir(), so the whole spawn path
// (authorized_keys, .profile, the per-agent run dir, the rootless stage) is
// driven end to end without writing into the machine's /home, /run or /tmp.
type dfb21Host struct {
	agentID    string
	username   string
	homeRoot   string
	agentHome  string
	runRoot    string
	sockDir    string
	tmpDir     string
	cmdLogPath string
}

func newDFB21Host(t *testing.T, agentID string) *dfb21Host {
	t.Helper()
	homeRoot := filepath.Join(t.TempDir(), "home")
	runRoot := filepath.Join(t.TempDir(), "run", "bunker")
	tmpDir := t.TempDir()
	if err := os.MkdirAll(homeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	restoreHome := agentHomeRoot
	agentHomeRoot = homeRoot
	t.Cleanup(func() { agentHomeRoot = restoreHome })
	restoreRun := spawnRunRoot
	spawnRunRoot = runRoot
	t.Cleanup(func() { spawnRunRoot = restoreRun })
	t.Setenv("TMPDIR", tmpDir)

	username := "bunker-" + agentID
	// The rollback's linger/manager steps only run for a user that RESOLVES.
	stubLookupUser(t, presentUserStub(username))
	restoreAgentUser := lookupAgentUser
	lookupAgentUser = func(name string) (*user.User, error) {
		return &user.User{
			Username: name,
			Uid:      "61001",
			Gid:      "61001",
			HomeDir:  filepath.Join(homeRoot, name),
		}, nil
	}
	t.Cleanup(func() { lookupAgentUser = restoreAgentUser })

	return &dfb21Host{
		agentID:   agentID,
		username:  username,
		homeRoot:  homeRoot,
		agentHome: filepath.Join(homeRoot, username),
		runRoot:   runRoot,
		sockDir:   filepath.Join(runRoot, agentID),
		tmpDir:    tmpDir,
	}
}

// installStubs puts the stub host commands on PATH. `userdel -r` really removes
// the agent home, so "the rollback removed the user's home" is a filesystem
// fact rather than a stub's promise; `chown` is a no-op (the paths are temp dirs
// owned by the test user) and can be scripted to block.
func (h *dfb21Host) installStubs(t *testing.T, chownBody string) {
	t.Helper()
	binDir := t.TempDir()
	h.cmdLogPath = filepath.Join(t.TempDir(), "cmds.log")
	writeStub(t, binDir, "useradd", stubSucceeds)
	writeStub(t, binDir, "userdel",
		"printf '%s\\n' \"userdel $*\" >> "+h.cmdLogPath+"\n"+
			"rm -rf "+h.agentHome+"\nexit 0\n")
	writeStub(t, binDir, "chown", chownBody)
	writeStub(t, binDir, "pkill", stubSucceeds)
	writeStub(t, binDir, "pgrep", "exit 1\n")
	writeStub(t, binDir, "ssh-keygen", keygenStubBody)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestSpawnRollbackUnderCancelledRequestRunsEveryStepOnALiveContext models the
// observed QA-BUNKER-19 failure end to end and pins the whole compensating
// chain:
//
//  1. the spawn creates the user, the isolation boundary, the SSH key material
//     and reaches the rootless stage — where its rootless host step blocks;
//  2. the request context is CANCELLED while that stage is provably in flight;
//  3. every compensating command must still RUN (none may be handed a dead
//     context), in the safe order, and the durable planes must be clean
//     afterwards: no agent home, no key material, no port range, no tracker
//     slot, no registry row — with the outcomes on the breadcrumb.
func TestSpawnRollbackUnderCancelledRequestRunsEveryStepOnALiveContext(t *testing.T) {
	m := intci5Manager(t)
	journal := redirectBreadcrumbJournal(t)

	h := newDFB21Host(t, uniqueAgentID("dfb21-rootless"))
	rootlessMarker := filepath.Join(t.TempDir(), "rootless-chown-started")
	h.installStubs(t, blockingChownStub(h.sockDir, rootlessMarker))

	rec := newRollbackCommandRecorder()
	rec.install(t, m)

	// The rootless host step never completes: it waits for the request to be
	// cancelled, exactly like a 93MB download at ~1.7MB/s against a 300s
	// request deadline.
	restoreRootHost := rootHostRunner
	rootHostRunner = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(func() { rootHostRunner = restoreRootHost })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := spawnInBackground(m, ctx, &v1.SpawnAgentRequest{AgentId: h.agentID, Ttl: "1h"})

	// Premise first: the spawn is inside the rootless stage (its socket-dir
	// chown has started), so the cancellation below is a mid-rootless-stage
	// cancellation, not a guess about elapsed time.
	awaitPath(t, rootlessMarker, 30*time.Second)
	cancel()

	err := awaitSpawnResult(t, done, 60*time.Second).err
	if err == nil {
		t.Fatal("Spawn() reported success although the rootless stage never completed")
	}
	if !strings.Contains(err.Error(), "failed at stage "+StageRootlessInstall) {
		t.Errorf("error does not name the rootless-install stage: %v", err)
	}
	if !strings.Contains(err.Error(), "cancelled by the server request timeout") {
		t.Errorf("error does not attribute the cancelled request: %v", err)
	}

	// ── AC2: no compensating command was handed a dead context ────────────
	started, refused := rec.snapshot()
	if len(refused) != 0 {
		t.Errorf("compensating commands were handed a dead context (they never ran): %v", refused)
	}

	// ── AC4: the chain ran, in the safe order ─────────────────────────────
	unit := userManagerUnitName(intSpawn001PresentUID)
	for _, want := range []string{
		"loginctl disable-linger " + h.username,
		"systemctl stop " + unit,
		"loginctl terminate-user " + strconv.Itoa(intSpawn001PresentUID),
		"pkill -u " + h.username + " -9",
		"userdel -r " + h.username,
	} {
		if rec.index(want) < 0 {
			t.Errorf("rollback never ran %q; started: %v", want, started)
		}
	}
	// The home-removing userdel stub is driven by the PATH stub log, so the
	// ordering is proven on ONE timeline for the two sides of the seam.
	userdelCalls := readRecord(t, h.cmdLogPath)
	if len(userdelCalls) == 0 {
		t.Errorf("rollback never ran `userdel -r %s` (calls: %v)", h.username, userdelCalls)
	}
	idxLinger := rec.index("loginctl disable-linger ")
	idxStop := rec.index("systemctl stop ")
	idxTerminate := rec.index("loginctl terminate-user ")
	idxReap := rec.index("pkill -u ")
	idxUserdel := rec.index("userdel -r ")
	if idxLinger > idxUserdel || idxStop > idxUserdel || idxTerminate > idxUserdel || idxReap > idxUserdel {
		t.Errorf("user removal ran out of order: linger=%d stop=%d terminate=%d reap=%d userdel=%d",
			idxLinger, idxStop, idxTerminate, idxReap, idxUserdel)
	}
	if rec.index("hostsetup") < 0 {
		t.Errorf("the isolation teardown was never attempted; started: %v", started)
	}

	// ── AC4: no residue on any durable plane ─────────────────────────────
	if _, statErr := os.Stat(h.agentHome); !os.IsNotExist(statErr) {
		t.Errorf("agent home survived the rollback: %s (stat error: %v)", h.agentHome, statErr)
	}
	keyFile := filepath.Join(h.tmpDir, "bunker-key-"+h.agentID)
	if _, statErr := os.Stat(keyFile); !os.IsNotExist(statErr) {
		t.Errorf("temporary key material survived the rollback: %s", keyFile)
	}
	sshKeyPath := filepath.Join(m.cfg.Agent.SSHDir, h.agentID)
	if _, statErr := os.Stat(sshKeyPath); !os.IsNotExist(statErr) {
		t.Errorf("persisted SSH key survived the rollback: %s", sshKeyPath)
	}
	if m.tracker.Get(h.agentID) != nil || m.tracker.Count() != 0 {
		t.Errorf("tracker residue: Get=%v count=%d", m.tracker.Get(h.agentID), m.tracker.Count())
	}
	if m.portAlloc.Has(h.agentID) {
		t.Errorf("port range for %s was never freed", h.agentID)
	}
	if m.registry != nil && m.registry.Get(h.agentID) != nil {
		t.Errorf("durable registry still holds a record for %s", h.agentID)
	}

	// ── the breadcrumb carries the outcomes, not a swallowed failure ─────
	bc := readSingleBreadcrumb(t, journal)
	if bc["agent_id"] != h.agentID {
		t.Errorf("breadcrumb agent_id = %v, want %q", bc["agent_id"], h.agentID)
	}
	if bc["stage"] != StageRootlessInstall {
		t.Errorf("breadcrumb stage = %v, want %q", bc["stage"], StageRootlessInstall)
	}
	ran := breadcrumbList(t, bc, "rollback_ran")
	if !containsPrefix(ran, "userdel "+h.username) {
		t.Errorf("rollback_ran does not carry the userdel: %v", ran)
	}
	// The isolation outcome must be REPORTED, either way. The private-/tmp
	// instance parent is restricted to mode 0000 by provisioning (pam_namespace
	// requires it), so only a root caller can remove it: as root the breadcrumb
	// says "isolation removed", as a non-root caller it carries the failure —
	// and in BOTH cases the rollback no longer prints an unconditional claim
	// (which is what hid the QA host's failed teardown).
	failed := breadcrumbList(t, bc, "rollback_failed")
	isolationOK := containsPrefix(ran, "isolation removed")
	isolationFailed := false
	for _, f := range failed {
		if strings.HasPrefix(f, "isolation: ") {
			isolationFailed = true
		}
	}
	if isolationOK == isolationFailed {
		t.Errorf("the isolation outcome is not reported honestly (ran=%v failed=%v)", ran, failed)
	}
	if rec.index("hostsetup") < 0 {
		t.Errorf("the isolation teardown was never attempted; started: %v", started)
	}
	if isolationFailed {
		// A residual must be diagnosable from the breadcrumb alone.
		for _, f := range failed {
			if strings.HasPrefix(f, "isolation: ") && !strings.Contains(f, h.agentID) {
				t.Errorf("the isolation residual does not name the agent: %q", f)
			}
		}
	}
}

// TestSpawnRollbackSurvivesStuckCleanupStep is the AC3 proof on the real spawn
// path: the cleanup steps that block until their own context expires — the
// stale-uid user manager that logind keeps restarting, which is what made
// `loginctl`/`systemctl` hang on the QA host — must not consume the whole
// rollback. The critical late steps (the process reap and `userdel -r`) still run,
// on their own reserved budget, and the breadcrumb SAYS the anchor was spent.
func TestSpawnRollbackSurvivesStuckCleanupStep(t *testing.T) {
	// Anchor 120ms / step 100ms / floor 50ms: the first blocking step eats most
	// of the anchor, so the steps after it demonstrably run on the floor.
	restore := shrinkRollbackBudgets(t, 120*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond)
	defer restore()

	m := intci5Manager(t)
	journal := redirectBreadcrumbJournal(t)

	h := newDFB21Host(t, uniqueAgentID("dfb21-stuck"))
	rootlessMarker := filepath.Join(t.TempDir(), "rootless-chown-started")
	h.installStubs(t, blockingChownStub(h.sockDir, rootlessMarker))

	// The stale-manager state: linger disable, the manager stop and the session
	// terminate all hang until their step context expires.
	rec := newRollbackCommandRecorder(
		"loginctl disable-linger ",
		"systemctl stop ",
		"loginctl terminate-user ",
	)
	rec.install(t, m)

	restoreRootHost := rootHostRunner
	rootHostRunner = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(func() { rootHostRunner = restoreRootHost })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := spawnInBackground(m, ctx, &v1.SpawnAgentRequest{AgentId: h.agentID, Ttl: "1h"})
	awaitPath(t, rootlessMarker, 30*time.Second)
	cancel()

	// The rollback must FINISH (bounded), not hang: the budget is the bound.
	err := awaitSpawnResult(t, done, 60*time.Second).err
	if err == nil {
		t.Fatal("Spawn() reported success although the rootless stage never completed")
	}

	_, refused := rec.snapshot()
	if len(refused) != 0 {
		t.Errorf("a blocking cleanup step starved the steps after it: %v", refused)
	}
	if calls := readRecord(t, h.cmdLogPath); len(calls) == 0 {
		t.Error("the stuck cleanup steps prevented `userdel -r` from being attempted")
	}
	if _, statErr := os.Stat(h.agentHome); !os.IsNotExist(statErr) {
		t.Errorf("agent home survived the rollback behind the stuck steps: %s", h.agentHome)
	}

	bc := readSingleBreadcrumb(t, journal)
	notices := breadcrumbList(t, bc, "rollback_notices")
	if len(notices) == 0 {
		t.Fatalf("the breadcrumb does not say the rollback anchor was spent (ran=%v failed=%v)",
			breadcrumbList(t, bc, "rollback_ran"), breadcrumbList(t, bc, "rollback_failed"))
	}
	for _, n := range notices {
		if !strings.Contains(n, "rollback budget exhausted") {
			t.Errorf("unexpected notice shape: %q", n)
		}
	}
}

// TestRootlessInstallerTeardownKillsTheWholeSubtree pins the second half of
// DF-BUNKER-21's root cause: the rootless installer is `su - <user> -c
// <installer>`, and exec.CommandContext's default cancellation signals only the
// direct child — leaving the download (`curl`) and the rest of the installer
// subtree running as the agent user, holding the home directory and the user
// manager busy so the rollback's `userdel -r` could not complete.
//
// The control arm runs the same shape with the DEFAULT cancellation and shows the
// grandchild surviving it; the fixed arm drives the REAL installer runner
// (runRootlessInstallerCmd, through a stubbed `su` that ignores its arguments and
// spawns a background child) and shows that subtree gone. The child PID is
// recorded by the stub itself and only that explicit PID is ever signalled.
func TestRootlessInstallerTeardownKillsTheWholeSubtree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process-group teardown and /proc probing are Linux-specific")
	}

	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	// `sleep 30 &` stands in for the installer's own child (the download) and
	// outlives every wait budget in this test, so "still running" and "gone" are
	// both real observations rather than a race with a short sleep.
	script := fmt.Sprintf("sleep 30 & echo $! > %s; wait", pidFile)

	binDir := t.TempDir()
	writeStub(t, binDir, "su", script+"\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	readPid := func(t *testing.T) int {
		t.Helper()
		awaitPath(t, pidFile, 10*time.Second)
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatalf("read grandchild pid: %v", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil || pid <= 0 {
			t.Fatalf("bad grandchild pid %q: %v", raw, err)
		}
		return pid
	}
	// killPid reaps the orphan the CONTROL arm deliberately leaves behind. Only
	// the explicit PID the test started is ever signalled.
	killPid := func(pid int) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Logf("cleanup: kill %d: %v", pid, err)
		}
	}
	// awaitCondition polls cond until it holds or the budget is spent.
	awaitCondition := func(cond func() bool, budget time.Duration) bool {
		deadline := time.Now().Add(budget)
		for time.Now().Before(deadline) {
			if cond() {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return cond()
	}
	removePidFile := func() { _ = os.Remove(pidFile) }

	t.Run("control: default cancellation leaves the subtree running", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() {
			_, err := exec.CommandContext(ctx, "su", "-", "bunker-dfb21", "-c", script).CombinedOutput()
			done <- err
		}()

		pid := readPid(t)
		cancel()
		survived := awaitCondition(func() bool { return processRunning(pid) }, 2*time.Second)
		// Reap the orphan this arm deliberately created BEFORE waiting for the
		// command: the surviving grandchild inherits the output pipe, so the
		// combined-output call cannot return while it lives.
		killPid(pid)
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the control command did not return after cancellation")
		}
		if !survived {
			t.Fatalf("control premise not reproducible here: the grandchild %d did not survive the default cancellation", pid)
		}
	})

	t.Run("fixed: the installer runner ends the whole subtree", func(t *testing.T) {
		removePidFile()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() {
			// The REAL production runner, against the stubbed `su`.
			_, err := runRootlessInstallerCmd(ctx, "bunker-dfb21", t.TempDir(), "true")
			done <- err
		}()

		pid := readPid(t)
		cancel()
		// The grandchild sleeps far longer than this budget, so only a process
		// GROUP kill can end it. The check runs BEFORE waiting for the runner to
		// return: with the default cancellation the runner stays blocked on the
		// output pipe the surviving grandchild still holds, which would hide the
		// leak behind a longer wait.
		if !awaitCondition(func() bool { return !processRunning(pid) }, 5*time.Second) {
			killPid(pid)
			t.Fatalf("the installer subtree survived the cancellation: grandchild %d is still running", pid)
		}
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the installer runner did not return after cancellation")
		}
	})
}

// TestRemoveIsolationReportsFailures pins the DF-BUNKER-21 reporting contract
// that the old signature could not express: removeIsolation RETURNS what the two
// removals did. The rollback used to record "isolation removed" unconditionally,
// so a teardown that had failed was invisible in the breadcrumb — the swallowed
// failure the QA foreman had to reconstruct from the host.
func TestRemoveIsolationReportsFailures(t *testing.T) {
	m := intci5Manager(t)

	t.Run("clean path reports success", func(t *testing.T) {
		// An agent that was never provisioned: both removals are idempotent
		// skips, so there is nothing to report.
		if err := m.removeIsolation(context.Background(), uniqueAgentID("dfb21-iso-clean")); err != nil {
			t.Fatalf("removeIsolation on an unprovisioned agent: %v", err)
		}
	})

	t.Run("failed removal is returned, not swallowed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: the restricted instance parent is traversable, so the removal cannot be made to fail here")
		}
		agentID := uniqueAgentID("dfb21-iso-fail")
		// Provisioning restricts the private-/tmp instance parent to mode 0000
		// (pam_namespace requires it) and a non-root caller can neither stat nor
		// remove what lives under it — the failing plane for this test.
		root := m.cfg.Agent.Isolation.PrivateTmpRoot
		instanceDir := filepath.Join(root, agentID)
		if err := os.MkdirAll(instanceDir, 0o700); err != nil {
			t.Fatalf("test premise: create tmp instance dir: %v", err)
		}
		if err := os.Chmod(root, 0o000); err != nil {
			t.Fatalf("test premise: restrict tmp instance parent: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

		err := m.removeIsolation(context.Background(), agentID)
		if err == nil {
			t.Fatal("removeIsolation swallowed a failed removal: the breadcrumb would claim the isolation was gone")
		}
		if !strings.Contains(err.Error(), agentID) {
			t.Errorf("the returned error does not name the failing plane/path: %v", err)
		}
	})
}

// processRunning reports whether pid is a live process: present in /proc and not
// a zombie. A killed child can linger as an unreaped zombie, which `kill -0`
// cannot distinguish from a running process.
func processRunning(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return false
	}
	return s[i+2] != 'Z'
}

// readSingleBreadcrumb reads the one JSON breadcrumb line the failure journal
// must hold, and returns it decoded.
func readSingleBreadcrumb(t *testing.T, journal string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("no spawn-failure breadcrumb was written: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("breadcrumb journal has %d lines, want exactly 1: %q", len(lines), lines)
	}
	var bc map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &bc); err != nil {
		t.Fatalf("breadcrumb is not a JSON line: %v (%q)", err, lines[0])
	}
	return bc
}

// breadcrumbList renders one of the breadcrumb's string lists.
func breadcrumbList(t *testing.T, bc map[string]any, key string) []string {
	t.Helper()
	raw, ok := bc[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// containsPrefix reports whether any entry starts with prefix.
func containsPrefix(entries []string, prefix string) bool {
	for _, e := range entries {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
