//go:build unix

package cli

// MOUNT-008 end-to-end command tests for `bunker mount --driver`.
//
// Every host seam the probe touches is stubbed (see stubIOUringHost), so these
// cells assert the command's BEHAVIOUR — what reaches the sshfs argv, what is
// refused before any side effect, what is printed — without requiring (or
// depending on) io_uring, libfuse3's build options or a modern kernel.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/mountdriver"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// mountTransportEnvironment wires the standard hermetic mount fixture: an
// isolated HOME with a mock GetAgent server, the client key, a stubbed
// workspace preflight, a stubbed healthy sshfs version probe, and the io_uring
// host seams.
func mountTransportEnvironment(t *testing.T, fx iouringHostFixture) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	newMountTestServer(t, mountFixtureSshfsMount)
	writeMountClientKey(t)
	_, restorePreflight := stubRemotePathCheck(t, nil)
	t.Cleanup(restorePreflight)
	stubSSHFSVersionProbe(t, func(ctx context.Context, path string) (string, error) {
		return "SSHFS version " + sshfsPatchedVersion + "\n", nil
	})
	stubIOUringHost(t, fx)
}

// hostWithoutIOUring is the shape of a host that cannot use the transport:
// libfuse3 read successfully, no io_uring in it.
func hostWithoutIOUring(seen *int) iouringHostFixture {
	return iouringHostFixture{
		survey: fixtureSurveyUnsupportedIOUring, release: "6.8.0-117-generic", paramRaw: "N\n", surveySeen: seen,
	}
}

// hostWithIOUring is the shape of a fully capable host (what the control host
// would look like with enable_uring=1).
func hostWithIOUring(seen *int) iouringHostFixture {
	return iouringHostFixture{
		survey: fixtureSurveySupportedIOUring, release: "7.0.0-31-generic", paramRaw: "Y\n", surveySeen: seen,
	}
}

// execMountCommand runs `bunker mount <args...>` and returns everything the
// command wrote to os.Stderr plus its error.
func execMountCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	stderr := captureOSError(t, func() {
		cmd := NewMountCommand()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err = cmd.Execute()
	})
	return stderr, err
}

// TestMountCommand_AutoOnAHostWithoutSupportIsByteIdenticalToPlainFuse is the
// zero-delta criterion: with `--driver=auto` (the default) on a host that
// cannot provide the transport, the sshfs argv must be exactly what
// `--driver=fuse` produces — same length, same order, no io_uring option —
// and nothing extra may be printed.
func TestMountCommand_AutoOnAHostWithoutSupportIsByteIdenticalToPlainFuse(t *testing.T) {
	seen := 0
	mountTransportEnvironment(t, hostWithoutIOUring(&seen))

	autoCalls, restoreAuto := stubSSHFSRun(t, 0, nil)
	autoLog, err := execMountCommand(t, "df0916a", t.TempDir()+"/mnt", "--driver=auto")
	restoreAuto()
	if err != nil {
		t.Fatalf("--driver=auto on an unsupported host failed: %v", err)
	}
	// Non-vacuity: the probe really ran for the auto invocation, so the
	// identical argv below is a FALLBACK decision, not an unprobed default.
	if seen == 0 {
		t.Error("--driver=auto did not probe the host; the zero-delta claim would be vacuous")
	}

	fuseCalls, restoreFuse := stubSSHFSRun(t, 0, nil)
	fuseLog, err := execMountCommand(t, "df0916a", t.TempDir()+"/mnt", "--driver=fuse")
	restoreFuse()
	if err != nil {
		t.Fatalf("--driver=fuse failed: %v", err)
	}

	if len(*autoCalls) != 1 || len(*fuseCalls) != 1 {
		t.Fatalf("expected exactly one sshfs exec each, got auto=%d fuse=%d", len(*autoCalls), len(*fuseCalls))
	}
	// The mountpoint (last element) differs by construction; compare the rest.
	auto, fuse := (*autoCalls)[0], (*fuseCalls)[0]
	if len(auto) != len(fuse) {
		t.Fatalf("auto argv length %d != fuse argv length %d\nauto: %q\nfuse: %q", len(auto), len(fuse), auto, fuse)
	}
	for i := range fuse {
		want := fuse[i]
		if i == len(fuse)-1 {
			continue // the caller's mountpoint
		}
		if auto[i] != want {
			t.Errorf("auto argv[%d] = %q, want %q (zero delta on a host without support)", i, auto[i], want)
		}
	}
	if hasFuseIOUringOption(auto) {
		t.Errorf("auto added the io_uring option on a host without support: %q", auto)
	}
	for _, log := range []string{autoLog, fuseLog} {
		if strings.Contains(log, "io_uring") {
			t.Errorf("a host without support must print nothing about the transport, got: %s", log)
		}
	}
}

// TestMountCommand_PlainTransportNeverProbesAndNeverChangesTheArgv pins
// `--driver=fuse` as the escape hatch: no host probe at all, argv unchanged
// even on a host that COULD provide the transport.
func TestMountCommand_PlainTransportNeverProbesAndNeverChangesTheArgv(t *testing.T) {
	seen := 0
	mountTransportEnvironment(t, hostWithIOUring(&seen))

	calls, restore := stubSSHFSRun(t, 0, nil)
	defer restore()
	log, err := execMountCommand(t, "df0916a", t.TempDir()+"/mnt", "--driver=fuse")
	if err != nil {
		t.Fatalf("--driver=fuse failed on a capable host: %v", err)
	}
	if seen != 0 {
		t.Errorf("--driver=fuse must not probe the host; the libfuse3 survey ran %d time(s)", seen)
	}
	if len(*calls) != 1 {
		t.Fatalf("sshfs execs = %d, want 1", len(*calls))
	}
	if hasFuseIOUringOption((*calls)[0]) {
		t.Errorf("--driver=fuse must never add the io_uring option: %q", (*calls)[0])
	}
	if strings.Contains(log, "io_uring") {
		t.Errorf("--driver=fuse must print nothing about the transport, got: %s", log)
	}
}

// TestMountCommand_IOUringOptionReachesTheArgvWhenTheHostSupportsIt covers
// requirement 3: on a host that can provide the transport, the option is
// passed through (`-o io_uring`, immediately before the mountpoint, which is
// what libfuse3 -> fusermount3 -> mount(2) consume) and the operator is told.
func TestMountCommand_IOUringOptionReachesTheArgvWhenTheHostSupportsIt(t *testing.T) {
	for _, driver := range []string{FuseTransportAuto, FuseTransportFuseIOUring} {
		t.Run(driver, func(t *testing.T) {
			mountTransportEnvironment(t, hostWithIOUring(nil))
			calls, restore := stubSSHFSRun(t, 0, nil)
			defer restore()

			mountpoint := t.TempDir() + "/mnt"
			log, err := execMountCommand(t, "df0916a", mountpoint, "--driver="+driver)
			if err != nil {
				t.Fatalf("--driver=%s on a capable host failed: %v", driver, err)
			}
			if len(*calls) != 1 {
				t.Fatalf("sshfs execs = %d, want 1", len(*calls))
			}
			argv := (*calls)[0]
			if !hasFuseIOUringOption(argv) {
				t.Fatalf("argv does not carry the io_uring option: %q", argv)
			}
			if argv[len(argv)-2] != iouringMountOption || argv[len(argv)-3] != "-o" {
				t.Errorf("the option must sit immediately before the mountpoint: %q", argv)
			}
			if argv[len(argv)-1] != mountpoint {
				t.Errorf("mountpoint must remain last, got %q", argv[len(argv)-1])
			}
			if !strings.Contains(log, iouringMountOption) {
				t.Errorf("the operator was not told which transport is in use: %s", log)
			}
		})
	}
}

// TestMountCommand_ExplicitIOUringIsRefusedBeforeAnySideEffect covers the
// verify-then-enable half of requirement 3: the refusal is named, bounded
// (nothing is executed, so nothing can hang), suggests the fallback, and
// happens before a mountpoint is created.
func TestMountCommand_ExplicitIOUringIsRefusedBeforeAnySideEffect(t *testing.T) {
	mountTransportEnvironment(t, hostWithoutIOUring(nil))
	calls, restore := stubSSHFSRun(t, 0, nil)
	defer restore()

	mountpoint := t.TempDir() + "/mnt"
	log, err := execMountCommand(t, "df0916a", mountpoint, "--driver="+FuseTransportFuseIOUring)
	if err == nil {
		t.Fatal("expected a refusal on a host that cannot provide the transport")
	}
	for _, want := range []string{FuseTransportFuseIOUring, "refusing", "--driver=fuse", "enable_uring", "CONFIG_FUSE_IOURING"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("sshfs was executed %d time(s); an unsupported-transport refusal must execute nothing", len(*calls))
	}
	if _, statErr := os.Stat(mountpoint); statErr == nil {
		t.Errorf("mountpoint %s was created; the refusal must precede it", mountpoint)
	}
	if strings.Contains(log, "Mounted") {
		t.Errorf("nothing may report a mount: %s", log)
	}
}

// TestMountCommand_AutoFallsBackWhenTheIOUringAttemptIsRejected covers the
// auto half of the failed-attempt contract: the attempt is bounded, the
// operator is warned, and the mount then proceeds on the plain transport —
// exactly as it would have without this row.
func TestMountCommand_AutoFallsBackWhenTheIOUringAttemptIsRejected(t *testing.T) {
	mountTransportEnvironment(t, hostWithIOUring(nil))

	oldRun, oldDelay := sshfsRun, sshfsRetryDelay
	sshfsRetryDelay = 0
	var calls [][]string
	sshfsRun = func(ctx context.Context, path string, args []string, stdout, stderr io.Writer) error {
		calls = append(calls, append([]string{path}, args...))
		if len(calls) == 1 {
			// The shape libfuse3 produces when the option is unknown to the
			// build, plus the kernel's own refusal shape.
			_, _ = fmt.Fprint(stderr, "fuse: unknown option(s): `-o io_uring'\n")
			return errors.New("exit status 1")
		}
		return nil
	}
	t.Cleanup(func() {
		sshfsRun = oldRun
		sshfsRetryDelay = oldDelay
	})

	log, err := execMountCommand(t, "df0916a", t.TempDir()+"/mnt", "--driver="+FuseTransportAuto)
	if err != nil {
		t.Fatalf("auto must fall back to the plain transport, got: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("sshfs execs = %d, want 2 (io_uring attempt, then plain retry)\n%q", len(calls), calls)
	}
	if !hasFuseIOUringOption(calls[0]) {
		t.Errorf("the first attempt must carry the option: %q", calls[0])
	}
	if hasFuseIOUringOption(calls[1]) {
		t.Errorf("the fallback attempt must NOT carry the option: %q", calls[1])
	}
	if !strings.Contains(log, "WARNING") || !strings.Contains(log, "falling back") {
		t.Errorf("the fallback was not announced: %s", log)
	}
}

// TestMountCommand_ExplicitIOUringFailureIsNamedAndNotRetried covers the other
// half: an explicitly requested transport that fails with a transport-specific
// cause stops immediately — one bounded attempt, a named error that suggests
// the fallback, and no retry storm.
func TestMountCommand_ExplicitIOUringFailureIsNamedAndNotRetried(t *testing.T) {
	mountTransportEnvironment(t, hostWithIOUring(nil))

	oldRun, oldDelay := sshfsRun, sshfsRetryDelay
	sshfsRetryDelay = 0
	var calls [][]string
	sshfsRun = func(ctx context.Context, path string, args []string, stdout, stderr io.Writer) error {
		calls = append(calls, append([]string{path}, args...))
		_, _ = fmt.Fprint(stderr, "fuse: unknown option(s): `-o io_uring'\n")
		return errors.New("exit status 1")
	}
	t.Cleanup(func() {
		sshfsRun = oldRun
		sshfsRetryDelay = oldDelay
	})

	_, err := execMountCommand(t, "df0916a", t.TempDir()+"/mnt", "--driver="+FuseTransportFuseIOUring)
	if err == nil {
		t.Fatal("expected the explicit io_uring request to fail")
	}
	if len(calls) != 1 {
		t.Errorf("sshfs execs = %d, want exactly 1 (a transport failure is not retried)", len(calls))
	}
	for _, want := range []string{FuseTransportFuseIOUring, iouringMountOption, "--driver=" + FuseTransportFuse, "Not retrying"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// TestMountCommand_InvalidDriverIsRefusedBeforeAnyWork pins the flag's
// vocabulary: a typo is refused before the config load, any probe or any exec.
func TestMountCommand_InvalidDriverIsRefusedBeforeAnyWork(t *testing.T) {
	seen := 0
	mountTransportEnvironment(t, hostWithIOUring(&seen))
	calls, restore := stubSSHFSRun(t, 0, nil)
	defer restore()

	_, err := execMountCommand(t, "df0916a", t.TempDir()+"/mnt", "--driver=fuse-io-uring")
	if err == nil {
		t.Fatal("expected an invalid --driver value to be refused")
	}
	for _, want := range []string{"invalid --driver", FuseTransportAuto, FuseTransportFuse, FuseTransportFuseIOUring} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("sshfs was executed %d time(s) for an invalid flag value", len(*calls))
	}
	if seen != 0 {
		t.Errorf("the host was probed %d time(s) for an invalid flag value", seen)
	}
}

// TestMountCommand_DriverFlagIsRegisteredWithAutoDefault pins the CLI surface:
// the flag exists, defaults to auto, and its help text says what it selects
// (and what it does not).
func TestMountCommand_DriverFlagIsRegisteredWithAutoDefault(t *testing.T) {
	cmd := NewMountCommand()
	flag := cmd.Flags().Lookup("driver")
	if flag == nil {
		t.Fatal("--driver flag not registered")
	}
	if flag.DefValue != FuseTransportAuto {
		t.Errorf("--driver default = %q, want %q", flag.DefValue, FuseTransportAuto)
	}
	for _, want := range []string{FuseTransportAuto, FuseTransportFuse, FuseTransportFuseIOUring, "transport"} {
		if !strings.Contains(flag.Usage, want) {
			t.Errorf("--driver usage does not mention %q: %s", want, flag.Usage)
		}
	}
}

// TestMountCommand_RcloneDriverRefusesIOUringTransportSelection pins the
// boundary: the transport selection belongs to the sshfs arm, so asking for it
// against the rclone mount driver is a named refusal rather than a silently
// dropped option.
func TestMountCommand_RcloneDriverRefusesIOUringTransportSelection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seen := 0
	const rcloneCmd = "rclone mount :sftp,host=bunker-host,user=bunker-agent,key_file=/tmp/k :/ /mnt/bunker/df0916a"
	newMountSpecTestServer(t, &v1.MountSpec{
		Driver:  mountdriver.DriverRclone,
		Command: rcloneCmd,
	}, rcloneCmd)
	writeMountClientKey(t)
	stubIOUringHost(t, hostWithIOUring(&seen))

	_, err := execMountCommand(t, "df0916a", t.TempDir()+"/mnt", "--driver="+FuseTransportFuseIOUring)
	if err == nil {
		t.Fatal("expected the rclone arm to refuse the io_uring transport selection")
	}
	if !strings.Contains(err.Error(), FuseTransportFuseIOUring) || !strings.Contains(err.Error(), mountdriver.DriverRclone) {
		t.Errorf("refusal must name both the transport and the driver: %v", err)
	}
}
