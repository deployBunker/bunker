package cli

// Mount fault-injection harness (GAP-108).
//
// Why this exists: every failure mode the mount durability work addresses is a
// TRANSPORT condition -- a dropped link, a blip, a stranded mountpoint, a hang,
// a swapped identity. Unit tests cannot produce those, so before this harness
// the durability contract could only be checked by hand on a real host, which
// means it could never be regression-tested. This doubles the sshfs/ssh seams
// and can be told to fail in each of those specific ways.
//
// The double is driven by environment variables rather than injected closures
// because the CLI's subprocess tests (proc_lifecycle_test.go) run the real
// binary and cannot reach a package variable. Both routes therefore reach the
// same behaviour, which is what makes the harness usable from either kind of
// test.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Environment knobs the harness honours. They are deliberately few and explicit.
const (
	// envMountFault names the failure to inject. See mountFault* below.
	envMountFault = "BUNKER_TEST_MOUNT_FAULT"
	// envMountFaultN controls how many attempts fail before recovering
	// (only meaningful for the transient/blip fault).
	envMountFaultN = "BUNKER_TEST_MOUNT_FAULT_N"
	// envMountFaultDelay makes the injected fault take this long, for the
	// hang case and for simulating a slow write that dies mid-flight.
	envMountFaultDelay = "BUNKER_TEST_MOUNT_FAULT_DELAY"
)

// Fault names. Each maps to a real-world disconnect shape.
const (
	// FaultPermanent: sshfs fails and will keep failing (auth, no route).
	faultPermanent = "permanent"
	// FaultTransient: sshfs fails N times then succeeds -- the blip that
	// -o reconnect is supposed to absorb.
	faultTransient = "transient"
	// FaultHang: sshfs never returns -- the dead peer that used to block a
	// tool forever before ServerAlive bounded it.
	faultHang = "hang"
	// FaultStrand: sshfs exits leaving the mountpoint directory behind with
	// no live FUSE session -- the crashed-mount case that used to block the
	// next mount with "mountpoint is not empty".
	faultStrand = "strand"
	// FaultPartialWrite: the mount reports success but a write via the seam
	// is lost -- the phantom-space case.
	faultPartialWrite = "partial-write"
	// FaultPreflightMissing: the remote path does not exist.
	faultPreflightMissing = "preflight-missing"
	// FaultPreflightUnreachable: the host cannot be reached at all.
	faultPreflightUnreachable = "preflight-unreachable"
)

// mountFault returns the configured fault, or "" when none is set.
func mountFault() string {
	return strings.TrimSpace(os.Getenv(envMountFault))
}

func mountFaultN() int {
	if v := os.Getenv(envMountFaultN); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

func mountFaultDelay() time.Duration {
	if v := os.Getenv(envMountFaultDelay); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 0
}

// faultedSSHFSRun is the sshfs seam with a fault injected. It returns nil when
// no fault is configured, so callers can install it unconditionally and keep the
// real behaviour in the no-fault case.
//
// attempt counts from 1, matching the mount command's retry loop.
func faultedSSHFSRun(fault string, attempt int, stdout, stderr io.Writer) error {
	switch fault {
	case faultPermanent:
		fmt.Fprint(stderr, "fuse: Permission denied\n")
		return errors.New("exit status 1")

	case faultTransient:
		if attempt <= mountFaultN() {
			fmt.Fprint(stderr, "read: Connection reset by peer\n")
			return errors.New("exit status 1")
		}
		return nil

	case faultHang:
		// Block until the caller's context/deadline ends the attempt. The
		// harness never returns on its own: the contract under test is that
		// the ATTEMPT is bounded, not that the fault is polite.
		d := mountFaultDelay()
		if d == 0 {
			d = 30 * time.Second
		}
		time.Sleep(d)
		return errors.New("hang fault: attempt ended by its deadline")

	case faultStrand:
		// Simulation of a mount that "succeeded" and then lost its transport:
		// the caller sees success, but any later operation is dead. The
		// strand is materialised by the caller creating the directory.
		return nil

	case faultPartialWrite:
		// Success reported, bytes dropped. The test asserts the caller does
		// not treat this as a verified write.
		return nil
	}
	return nil
}

// applyMountFaultToSeam installs the fault-aware sshfs seam and returns a
// restore function. The restore is NOT optional: leaving a fault seam installed
// poisons every later test in the package (the failure looks like "capture
// missing marker", in a test that has nothing to do with faults), so callers
// must always defer the returned func.
func applyMountFaultToSeam() (restore func()) {
	fault := mountFault()
	if fault == "" {
		return func() {}
	}
	old := sshfsRun
	attempt := 0
	sshfsRun = func(ctx context.Context, path string, args []string, stdout, stderr io.Writer) error {
		attempt++
		return faultedSSHFSRun(fault, attempt, stdout, stderr)
	}
	return func() { sshfsRun = old }
}

// faultedRemotePathCheck injects a preflight failure for the two preflight
// faults. Returns nil,false when no preflight fault is configured.
func faultedRemotePathCheck() (error, bool) {
	switch mountFault() {
	case faultPreflightMissing:
		return fmt.Errorf("mount preflight: remote path %q does not exist or is not a directory on the target", "/nonexistent-workspace"), true
	case faultPreflightUnreachable:
		return fmt.Errorf("mount preflight: cannot reach the target to verify the path: dial tcp: i/o timeout"), true
	}
	return nil, false
}

// StrandedMountPoint creates the observable state a crashed sshfs leaves
// behind: the mountpoint directory exists but nothing is mounted on it, so the
// next mount must detect and clear it rather than failing with "not empty".
//
// Returns the path it created, so a test can assert it was cleaned.
func StrandedMountPoint(dir, agentID string) (string, error) {
	path := filepath.Join(dir, agentID)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	// A crashed FUSE mount typically leaves the mountpoint unreadable (the
	// kernel returns ENOTCONN for operations on the dead session). Simulate
	// the detectable part: the directory is present and empty.
	return path, nil
}
