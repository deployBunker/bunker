package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unitFor is the test helper mirroring production unit naming.
func unitFor(uid int) string { return fmt.Sprintf("user@%d.service", uid) }

// fakeCtl is an in-memory systemctl. All closures touch only local state;
// safe for parallel table entries because each case gets a fresh instance.
type fakeCtl struct {
	states       map[string]string // unit -> is-active state
	startErr     map[string]int    // unit -> remaining failing starts
	resetCount   map[string]int    // unit -> reset-failed calls
	startCount   map[string]int    // unit -> start attempts
	showExitFail bool              // make `show -p Result --value` fail
	startOut     string            // text captured from a failing start
}

func (f *fakeCtl) run(_ context.Context, name string, args ...string) ([]byte, error) {
	if f.startCount == nil {
		f.startCount = map[string]int{}
	}
	if f.resetCount == nil {
		f.resetCount = map[string]int{}
	}
	if name != "systemctl" || len(args) == 0 {
		return nil, fmt.Errorf("fakeCtl: unexpected command %s %v", name, args)
	}
	unit := ""
	if len(args) > 1 {
		unit = args[len(args)-1]
	}
	switch args[0] {
	case "is-active":
		state, ok := f.states[unit]
		if !ok {
			return []byte("unknown\n"), fmt.Errorf("exit status 3")
		}
		if state == "failed" {
			return []byte("failed\n"), fmt.Errorf("exit status 3")
		}
		return []byte(state + "\n"), nil
	case "show":
		if f.showExitFail {
			return []byte(""), fmt.Errorf("exit status 1")
		}
		return []byte("exit-code\n"), nil
	case "start":
		f.startCount[unit]++
		if f.startErr[unit] > 0 {
			f.startErr[unit]--
			return []byte(f.startOut), fmt.Errorf("exit status 1")
		}
		// A successful start only flips the unit to active when it is not
		// scripted as permanently "activating" (that state models a manager
		// stuck mid-startup: start returns success, is-active never says
		// active).
		if f.states[unit] != "activating" {
			f.states[unit] = "active"
		}
		return []byte(""), nil
	case "reset-failed":
		f.resetCount[unit]++
		if f.states[unit] == "failed" {
			f.states[unit] = "inactive"
		}
		return []byte(""), nil
	default:
		return nil, fmt.Errorf("fakeCtl: unexpected systemctl verb %q", args[0])
	}
}

// installFakeCtl swaps in a fakeCtl runner plus the temp linger dir and
// restores all seams on cleanup.
func installFakeCtl(t *testing.T, f *fakeCtl, lingerDirPath string) {
	t.Helper()
	prevRunner := userManagerRunner
	prevLinger := lingerDir
	userManagerRunner = f.run
	lingerDir = lingerDirPath
	t.Cleanup(func() {
		userManagerRunner = prevRunner
		lingerDir = prevLinger
	})
}

// ctlLogger discards everything; log assertions are done on the returned
// error text instead.
func ctlLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ── ensureUserManagerRunning: the five required scenarios ──────────────────

// TestEnsureUserManagerRunning_Scenarios covers the required scenarios:
//   - already_active:    healthy path, zero start attempts, immediate return
//   - reset_then_start:  first start fails, reset-failed + second start works
//   - both_starts_fail:  attribution error with stage, unit, and unit state
//   - ctx_canceled:      prompt return with the context error
//   - not_state_active:  a successful start that does NOT report active must
//     not be treated as success (start error and state must be disjoint)
func TestEnsureUserManagerRunning_Scenarios(t *testing.T) {
	unit := unitFor(1002)

	t.Run("already_active", func(t *testing.T) {
		f := &fakeCtl{states: map[string]string{unit: "active"}}
		installFakeCtl(t, f, t.TempDir())
		if err := ensureUserManagerRunning(context.Background(), 1002, ctlLogger()); err != nil {
			t.Fatalf("expected nil error for already-active unit, got: %v", err)
		}
		if f.startCount[unit] != 0 {
			t.Errorf("healthy path must not attempt starts, got %d", f.startCount[unit])
		}
	})

	t.Run("reset_then_start", func(t *testing.T) {
		f := &fakeCtl{
			states:   map[string]string{unit: "inactive"},
			startErr: map[string]int{unit: 1},
		}
		installFakeCtl(t, f, t.TempDir())
		if err := ensureUserManagerRunning(context.Background(), 1002, ctlLogger()); err != nil {
			t.Fatalf("expected nil after reset-failed + successful retry, got: %v", err)
		}
		if f.startCount[unit] != 2 {
			t.Errorf("expected exactly 2 start attempts, got %d", f.startCount[unit])
		}
		if f.resetCount[unit] != 1 {
			t.Errorf("expected exactly 1 reset-failed, got %d", f.resetCount[unit])
		}
	})

	t.Run("both_starts_fail", func(t *testing.T) {
		f := &fakeCtl{
			states:   map[string]string{unit: "failed"},
			startErr: map[string]int{unit: 2},
			startOut: "Job for user@1002.service failed",
		}
		lingerDirPath := t.TempDir()
		for _, name := range []string{"a", "b", "c"} {
			if err := os.WriteFile(filepath.Join(lingerDirPath, name), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		installFakeCtl(t, f, lingerDirPath)
		err := ensureUserManagerRunning(context.Background(), 1002, ctlLogger())
		if err == nil {
			t.Fatal("expected attribution error after both starts failed")
		}
		msg := err.Error()
		for _, want := range []string{
			"user-manager-start",
			"user@1002.service",
			"failed",
			"linger_entries=3",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("attribution error missing %q, got: %s", want, msg)
			}
		}
		if f.resetCount[unit] != 2 {
			t.Errorf("expected reset-failed after each failed start, got %d", f.resetCount[unit])
		}
		if f.startCount[unit] != 2 {
			t.Errorf("expected exactly 2 bounded attempts, got %d", f.startCount[unit])
		}
	})

	t.Run("ctx_canceled", func(t *testing.T) {
		f := &fakeCtl{states: map[string]string{unit: "inactive"}}
		installFakeCtl(t, f, t.TempDir())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := ensureUserManagerRunning(ctx, 1002, ctlLogger())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
		if err != context.Canceled {
			t.Errorf("error must be exactly context.Canceled, got %T: %v", err, err)
		}
	})

	t.Run("not_state_active", func(t *testing.T) {
		// Start reports success but is-active says "activating": the state
		// check wins, a bounded second attempt runs, and the attribution
		// error carries the real observed state.
		f := &fakeCtl{states: map[string]string{unit: "activating"}}
		installFakeCtl(t, f, t.TempDir())
		err := ensureUserManagerRunning(context.Background(), 1002, ctlLogger())
		if err == nil {
			t.Fatal("expected attribution error: start success must not imply active state")
		}
		msg := err.Error()
		for _, want := range []string{"user-manager-start", "user@1002.service", "activating"} {
			if !strings.Contains(msg, want) {
				t.Errorf("attribution error missing %q, got: %s", want, msg)
			}
		}
		if f.startCount[unit] != 2 {
			t.Errorf("expected exactly 2 bounded attempts, got %d", f.startCount[unit])
		}
	})
}

// ── waitForUserManager: own-budget polling ─────────────────────────────────

// TestWaitForUserManager_OwnBudget covers the required scenarios:
//   - bus_after_n_polls:  baseline success when the socket appears later
//   - caller_deadline_ignored:  a caller ctx with a short deadline does NOT
//     end the poll when the socket appears (the budget is our own)
//   - budget_expires:  expired budget yields the stage-naming error
//   - ctx_canceled_prompt:  pre-canceled caller ctx returns exactly
//     context.Canceled with no runner invocations
func TestWaitForUserManager_OwnBudget(t *testing.T) {
	prevPoll := userManagerPollInterval
	userManagerPollInterval = 1 * time.Millisecond
	defer func() { userManagerPollInterval = prevPoll }()

	newRuntimeDir := func(t *testing.T) string {
		t.Helper()
		return t.TempDir()
	}

	t.Run("bus_after_n_polls", func(t *testing.T) {
		userManagerWaitTimeoutOverride = 2 * time.Second
		defer func() { userManagerWaitTimeoutOverride = 0 }()
		dir := newRuntimeDir(t)
		go func() {
			time.Sleep(25 * time.Millisecond)
			_ = os.WriteFile(filepath.Join(dir, "bus"), nil, 0o644)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		start := time.Now()
		if err := waitForUserManager(ctx, dir); err != nil {
			t.Fatalf("waitForUserManager: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("returned after %v; poll should finish promptly once the bus appears", elapsed)
		}
	})

	t.Run("caller_deadline_ignored", func(t *testing.T) {
		userManagerWaitTimeoutOverride = 2 * time.Second
		defer func() { userManagerWaitTimeoutOverride = 0 }()
		dir := newRuntimeDir(t)
		go func() {
			time.Sleep(30 * time.Millisecond)
			_ = os.WriteFile(filepath.Join(dir, "bus"), nil, 0o644)
		}()
		// Caller deadline expires BEFORE the bus appears but the budget
		// outlives it: with the caller's deadline inherited (pre-fix), the
		// ctx.Done branch fires and the poll fails with context.DeadlineExceeded.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if err := waitForUserManager(ctx, dir); err != nil {
			t.Fatalf("own budget must outlive the caller deadline, got: %v", err)
		}
	})

	t.Run("budget_expires", func(t *testing.T) {
		userManagerWaitTimeoutOverride = 5 * time.Millisecond
		defer func() { userManagerWaitTimeoutOverride = 0 }()
		dir := newRuntimeDir(t) // no bus socket will ever appear
		err := waitForUserManager(context.Background(), dir)
		if err == nil {
			t.Fatal("expected timeout error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "user-manager-start") {
			t.Errorf("timeout error must name the stage, got: %s", msg)
		}
		if strings.Contains(msg, "context") {
			t.Errorf("budget expiry must NOT be reported as a context error, got: %s", msg)
		}
	})

	t.Run("ctx_canceled_prompt", func(t *testing.T) {
		userManagerWaitTimeoutOverride = 5 * time.Second
		defer func() { userManagerWaitTimeoutOverride = 0 }()
		dir := newRuntimeDir(t)
		called := 0
		prev := userManagerRunner
		userManagerRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			called++
			return prev(ctx, name, args...)
		}
		defer func() { userManagerRunner = prev }()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := waitForUserManager(ctx, dir); err != context.Canceled {
			t.Fatalf("expected exactly context.Canceled, got: %v", err)
		}
		if called != 0 {
			t.Errorf("cancel path must not invoke systemctl, got %d calls", called)
		}
	})
}

// TestWaitForUserManager_BudgetBeatsCallerDeadline is the regression core for
// INT-CI-007: the pre-fix implementation inherited the caller's deadline, so a
// spawn whose client gave up after 300s burned the whole budget in a passive
// wait. The budget must be independent of the caller's deadline in BOTH
// directions: longer caller deadlines must not extend the poll, and short
// caller deadlines must not truncate a poll that would have succeeded.
func TestWaitForUserManager_BudgetBeatsCallerDeadline(t *testing.T) {
	prevPoll := userManagerPollInterval
	userManagerPollInterval = 1 * time.Millisecond
	defer func() { userManagerPollInterval = prevPoll }()
	userManagerWaitTimeoutOverride = 2 * time.Second
	defer func() { userManagerWaitTimeoutOverride = 0 }()

	dir := t.TempDir()
	go func() {
		time.Sleep(40 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(dir, "bus"), nil, 0o644)
	}()
	// Caller deadline (5ms) is far shorter than both the poll budget and the
	// 40ms socket delay. The pre-fix code returned context.DeadlineExceeded;
	// the fixed code must ride out the caller deadline on its own budget.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := waitForUserManager(ctx, dir); err != nil {
		t.Fatalf("own budget must outlive the caller's short deadline, got: %v", err)
	}
}

// TestCountLingerEntries exercises the counter against a real directory so the
// "linger_entries=N" attribution fragment reflects actual directory contents.
func TestCountLingerEntries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"bunker-a", "bunker-b", "legacy-user"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lingerDir = dir
	t.Cleanup(func() { lingerDir = "/var/lib/systemd/linger" })

	if got := countLingerEntries(); got != 3 {
		t.Errorf("countLingerEntries() = %d, want 3", got)
	}

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	lingerDir = missing
	if got := countLingerEntries(); got != -1 {
		t.Errorf("missing dir: countLingerEntries() = %d, want -1", got)
	}
}

// TestUserManagerStateDescribe pins the attribution line format the foreman
// greps journal breadcrumbs for.
func TestUserManagerStateDescribe(t *testing.T) {
	cases := []struct {
		name  string
		state userManagerState
		want  string
	}{
		{
			name:  "with_result",
			state: userManagerState{active: "failed", result: "exit-code"},
			want:  "unit user@1002.service: is-active=failed (Result=exit-code)",
		},
		{
			name:  "without_result",
			state: userManagerState{active: "inactive"},
			want:  "unit user@1002.service: is-active=inactive",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.describe(unitFor(1002)); got != tc.want {
				t.Errorf("describe() = %q, want %q", got, tc.want)
			}
		})
	}
}
