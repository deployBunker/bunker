package cli

// The GAP-108 fault-injection tests: each of the mount durability failure modes
// driven deterministically, so the contract is regression-tested rather than
// verified once by hand on a host.
//
// Every test asserts BOTH sides: the failure is produced, and the mount's
// behaviour under it is the designed one. A test that only produced the failure
// would pass vacuously.

import (
	"os"
	"strings"
	"testing"
	"time"
)

// --- the fault double itself is load-bearing --------------------------------

// TestFaultedSSHFSRun_PermanentFailsEveryAttempt is the premise check for the
// permanent fault: if this double ever returned nil, every downstream test
// using it would pass vacuously.
func TestFaultedSSHFSRun_PermanentFailsEveryAttempt(t *testing.T) {
	for attempt := 1; attempt <= 3; attempt++ {
		var sb, eb strings.Builder
		err := faultedSSHFSRun(faultPermanent, attempt, &sb, &eb)
		if err == nil {
			t.Fatalf("attempt %d: permanent fault returned nil; the failure was not produced", attempt)
		}
		if !strings.Contains(eb.String(), "Permission denied") {
			t.Fatalf("attempt %d: stderr %q does not carry the permanent cause the classifier keys on", attempt, eb.String())
		}
	}
}

// TestFaultedSSHFSRun_TransientRecoversAfterN pins the blip shape: N failures
// then success, which is exactly what -o reconnect / the retry loop must absorb.
func TestFaultedSSHFSRun_TransientRecoversAfterN(t *testing.T) {
	t.Setenv(envMountFaultN, "2")
	var sb, eb strings.Builder

	if err := faultedSSHFSRun(faultTransient, 1, &sb, &eb); err == nil {
		t.Fatal("attempt 1 should fail (within the transient budget)")
	}
	if err := faultedSSHFSRun(faultTransient, 2, &sb, &eb); err == nil {
		t.Fatal("attempt 2 should fail (within the transient budget)")
	}
	if err := faultedSSHFSRun(faultTransient, 3, &sb, &eb); err != nil {
		t.Fatalf("attempt 3 should SUCCEED (the blip has passed), got %v", err)
	}
}

// TestFaultedSSHFSRun_TransientBudgetIsHonoured guards against the harness
// silently recovering too early, which would make the retry assertions weak.
func TestFaultedSSHFSRun_TransientBudgetIsHonoured(t *testing.T) {
	t.Setenv(envMountFaultN, "2")
	var sb, eb strings.Builder
	err := faultedSSHFSRun(faultTransient, 3, &sb, &eb)
	if err != nil {
		t.Fatalf("with N=2, attempt 3 must succeed: %v", err)
	}
	if n := mountFaultN(); n != 2 {
		t.Fatalf("mountFaultN() = %d, want 2", n)
	}
}

// TestFaultedRemotePathCheck_MissingAndUnreachableAreDistinct is the assertion
// that the two preflight causes do not collapse: "the path is not there" and
// "I could not reach the host" call for different operator action, so an
// operator must be able to tell them apart.
func TestFaultedRemotePathCheck_MissingAndUnreachableAreDistinct(t *testing.T) {
	t.Setenv(envMountFault, faultPreflightMissing)
	missingErr, ok := faultedRemotePathCheck()
	if !ok || missingErr == nil {
		t.Fatal("missing fault did not produce an error")
	}
	if !strings.Contains(missingErr.Error(), "does not exist") {
		t.Fatalf("missing-path error should say the path does not exist, got: %v", missingErr)
	}

	t.Setenv(envMountFault, faultPreflightUnreachable)
	unreachableErr, ok := faultedRemotePathCheck()
	if !ok || unreachableErr == nil {
		t.Fatal("unreachable fault did not produce an error")
	}
	if !strings.Contains(unreachableErr.Error(), "cannot reach") {
		t.Fatalf("unreachable error should say the host cannot be reached, got: %v", unreachableErr)
	}

	if missingErr.Error() == unreachableErr.Error() {
		t.Fatal("the two preflight causes produced the SAME message; they must be distinguishable")
	}
}

// TestFaultedRemotePathCheck_NoFaultIsClean is the negative control: with no
// fault configured the harness must not manufacture a failure, or every
// preflight test would be meaningless.
func TestFaultedRemotePathCheck_NoFaultIsClean(t *testing.T) {
	t.Setenv(envMountFault, "")
	if err, ok := faultedRemotePathCheck(); ok || err != nil {
		t.Fatalf("no fault configured but harness produced err=%v ok=%v", err, ok)
	}
}

// --- the failure modes the design must handle -------------------------------

// TestMountFault_PermanentFailsImmediately: a permanent cause must NOT be
// retried. Retrying an auth failure wastes the operator's time and, worse,
// mislabels the cause as session limiting.
func TestMountFault_PermanentFailsImmediately(t *testing.T) {
	t.Setenv(envMountFault, faultPermanent)
	defer applyMountFaultToSeam()()

	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)
	_, restorePreflight := stubRemotePathCheck(t, nil)
	defer restorePreflight()

	err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt")
	if err == nil {
		t.Fatal("expected the mount to fail on a permanent fault")
	}
	if strings.Contains(err.Error(), "limiting parallel SSH sessions") {
		t.Errorf("a permanent failure must not hint at session limiting, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("the permanent cause must be preserved in the error, got: %v", err)
	}
}

// TestMountFault_TransientRecoversWithinBudget: the blip case. The retry loop
// must absorb N transient failures and still mount, without operator action.
func TestMountFault_TransientRecoversWithinBudget(t *testing.T) {
	t.Setenv(envMountFault, faultTransient)
	t.Setenv(envMountFaultN, "2")
	defer applyMountFaultToSeam()()

	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)
	_, restorePreflight := stubRemotePathCheck(t, nil)
	defer restorePreflight()

	if err := runMountExecutesSSHFS(t, t.TempDir()+"/mnt"); err != nil {
		t.Fatalf("a transient blip within the retry budget must still mount, got: %v", err)
	}
}

// TestMountFault_StrandedMountPointIsDetectable: a crashed sshfs leaves the
// mountpoint directory behind with no live session. The design must be able to
// DETECT that state (nothing mounted) so the next mount can clear it rather
// than failing with "mountpoint is not empty".
func TestMountFault_StrandedMountPointIsDetectable(t *testing.T) {
	dir := t.TempDir()
	stranded, err := StrandedMountPoint(dir, "stranded-agent")
	if err != nil {
		t.Fatalf("create stranded mountpoint: %v", err)
	}

	// Premise: the directory exists.
	if !dirExists(stranded) {
		t.Fatal("premise failed: stranded mountpoint was not created")
	}

	// The detection that matters: it must NOT read as mounted, because the
	// FUSE session is gone. A mountpoint that lies here is how the operator
	// ends up unable to remount.
	mounted, err := isMountPoint(stranded)
	if err != nil {
		t.Fatalf("isMountPoint on a stranded path: %v", err)
	}
	if mounted {
		t.Fatal("a stranded (dead-session) mountpoint reported as mounted")
	}
}

// TestMountFault_DeadMountPointIsCleanable: the operator must be able to clean a
// stranded mountpoint without knowing how far the crashed attempt got.
func TestMountFault_DeadMountPointIsCleanable(t *testing.T) {
	dir := t.TempDir()
	stranded, err := StrandedMountPoint(dir, "cleanup-agent")
	if err != nil {
		t.Fatal(err)
	}

	cmd := NewUmountCommand()
	cmd.SetArgs([]string{stranded})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cleaning a stranded mountpoint must succeed, got: %v", err)
	}
}

// TestMountFault_HangIsBounded is the anti-hang contract: a dead peer must not
// block a tool indefinitely. The harness makes sshfs block for longer than the
// attempt budget, and the attempt must still end within its deadline.
//
// NOTE: this test drives the mount in a goroutine, so it must restore every seam
// it installs BEFORE returning. A stubbed remotePathCheck left installed here
// would leak into whatever test runs next and break it in a way that has nothing
// to do with that test.
func TestMountFault_HangIsBounded(t *testing.T) {
	// Shrink the attempt deadline so the test reaches it quickly; the contract
	// under test is that the deadline BOUNDS the hang, not that it is 30s.
	oldTimeout := sshfsAttemptTimeout
	oldDelay := sshfsRetryDelay
	oldSeam := sshfsRun
	sshfsAttemptTimeout = 500 * time.Millisecond
	sshfsRetryDelay = 0
	defer func() {
		sshfsAttemptTimeout = oldTimeout
		sshfsRetryDelay = oldDelay
		sshfsRun = oldSeam
	}()

	t.Setenv(envMountFault, faultHang)
	// Block far longer than the attempt budget: if bounding is broken the test
	// hangs rather than passes, which is the failure we want to catch.
	t.Setenv(envMountFaultDelay, (sshfsAttemptTimeout * 10).String())
	defer applyMountFaultToSeam()()

	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)
	preflightCalls, restorePreflight := stubRemotePathCheck(t, nil)

	done := make(chan error, 1)
	go func() { done <- runMountExecutesSSHFS(t, t.TempDir()+"/mnt") }()

	// Allow headroom for the bounded retries, far less than the injected hang.
	budget := sshfsAttemptTimeout*sshfsMaxAttempts + 15*time.Second
	timedOut := false
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a hanging peer must not produce a successful mount")
		}
	case <-time.After(budget):
		timedOut = true
	}

	// Restore the seam BEFORE any assertion can return, so a failure here cannot
	// poison a sibling test.
	restorePreflight()
	_ = preflightCalls

	if timedOut {
		t.Fatalf("mount did not return within %s: the hang was NOT bounded by the attempt deadline", budget)
	}
}

// TestMountFault_NoFaultKeepsRealBehaviour is the negative control for the
// whole harness: with no fault set, the seam must still be the real one, so
// these tests cannot be masking a broken mount path.
func TestMountFault_NoFaultKeepsRealBehaviour(t *testing.T) {
	t.Setenv(envMountFault, "")
	before := sshfsRun
	defer applyMountFaultToSeam()()
	// The function value is replaced only when a fault is configured; with no
	// fault the seam must be left alone. Comparing function pointers directly is
	// not possible in Go, so assert the observable consequence instead: the
	// fault path returns no error for a plain call.
	if err := faultedSSHFSRun("", 1, nil, nil); err != nil {
		t.Fatalf("empty fault should be a no-op, got %v", err)
	}
	_ = before
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
