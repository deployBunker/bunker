//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
)

// SocketDirMode is the REQUIRED mode of the per-agent socket directory
// (/run/bunker/<id>) — NET-BUNKER-007, specs/network-isolation.md §6.1/§6.3.
// Owner rwx, group and world ZERO. It is set explicitly (never the umask),
// then stat-ed back — a chmod that exited 0 is not proof.
const SocketDirMode os.FileMode = 0700

// ErrSocketDirMode is the sentinel carried by every Ensure/Assert failure so
// callers (and tests) can tell "the directory is not 0700/agent-owned" apart
// from ordinary filesystem errors.
var ErrSocketDirMode = errors.New("agent socket directory mode/ownership assertion failed")

// chownForTests can be swapped by tests to observe the chown call without a
// real agent user on the host. The default execs the REAL `chown` through
// CommandContext — the spawn's cancellation must be able to kill it exactly
// like the pre-NET-BUNKER-007 inline call did (the rollback-budget tests pin
// that a blocked chown dies with the request instead of outliving it).
var chownForTests = func(ctx context.Context, username, path string) error {
	out, err := exec.CommandContext(ctx, "chown", username, path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("chown %s %s: %w (output: %s)", username, path, err, string(out))
	}
	return nil
}

// EnsureAgentSocketDir creates (or repairs) the per-agent socket directory to
// the asserted state: mode 0700, owner = the agent user. MkdirAll with an
// explicit 0700 covers fresh creation; the explicit Chmod forces the mode on
// a pre-existing directory AND strips umask/group/world bits MkdirAll may
// have left (the umask only REMOVES bits it does not add them, but the point
// of the assertion law is that nothing is left implied). The chown mirrors
// the spawn path it replaces — same ctx, so cancellation semantics are
// byte-identical to the call it replaced.
func EnsureAgentSocketDir(ctx context.Context, username, dir string) error {
	if err := os.MkdirAll(dir, SocketDirMode); err != nil {
		return fmt.Errorf("%w: mkdir %s: %v", ErrSocketDirMode, dir, err)
	}
	if err := os.Chmod(dir, SocketDirMode); err != nil {
		return fmt.Errorf("%w: chmod %s: %v", ErrSocketDirMode, dir, err)
	}
	if err := chownForTests(ctx, username, dir); err != nil {
		return fmt.Errorf("%w: chown %s: %v", ErrSocketDirMode, dir, err)
	}
	return nil
}

// AssertAgentSocketDir STATS the directory back and fails unless BOTH
// properties hold: mode is exactly SocketDirMode (0700 — the negative case
// reddens for 0755 because group/world bits are set) and the owner is
// wantUID — the uid the spawn already resolved for the agent through
// lookupAgentUser (ONE authority; the assertion never re-resolves the user
// through a second one). This is the §6.3 assertion: the spawn path calls
// it right after Ensure, so a directory that LOOKS isolated but is not
// fails the spawn instead of manufacturing confidence.
//
// statOwnerForTests lets hermetic harnesses whose fake agent uid cannot own
// a real file model the chown's effect; nil uses the real Stat_t owner.
func AssertAgentSocketDir(dir string, wantUID uint32) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("%w: stat %s: %v", ErrSocketDirMode, dir, err)
	}
	if got := fi.Mode().Perm(); got != SocketDirMode {
		return fmt.Errorf("%w: %s mode is %#o, required %#o (group/world bits must be zero)", ErrSocketDirMode, dir, got, SocketDirMode)
	}
	ownerUID, ownerOK := func() (uint32, bool) {
		if statOwnerForTests != nil {
			return statOwnerForTests(dir)
		}
		stat, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, false
		}
		return stat.Uid, true
	}()
	if !ownerOK {
		return fmt.Errorf("%w: cannot read owner of %s (%T)", ErrSocketDirMode, dir, fi.Sys())
	}
	if ownerUID != wantUID {
		return fmt.Errorf("%w: %s owned by uid %d, expected agent uid %d", ErrSocketDirMode, dir, ownerUID, wantUID)
	}
	return nil
}

// statOwnerForTests is the ownership read-back seam (see AssertAgentSocketDir).
var statOwnerForTests func(dir string) (uid uint32, ok bool)

// EnsureSocketFileNotGroupWorld is the §6.1 socket-file half of the
// assertion: the socket file itself must not be group- or world-accessible.
// The listening process (dockerd-rootless) creates the file and applies ITS
// umask — outside this tree — so the daemon ASSERTS the result instead of
// trusting it, and tightens a file that carries group/world bits. It does
// not widen anything: chmod 0600 on a socket file only removes bits.
// A symlink at sockPath is followed (the logical path is a symlink to
// /run/user/<uid>/docker.sock once waitForDockerd reconciles it); the TARGET
// file's mode is what a connect() check consults.
func EnsureSocketFileNotGroupWorld(sockPath string) error {
	fi, err := os.Lstat(sockPath)
	if err != nil {
		return fmt.Errorf("%w: stat %s: %v", ErrSocketDirMode, sockPath, err)
	}
	mode := fi.Mode()
	if mode&os.ModeSymlink != 0 {
		fi, err = os.Stat(sockPath)
		if err != nil {
			return fmt.Errorf("%w: stat (following symlink) %s: %v", ErrSocketDirMode, sockPath, err)
		}
		mode = fi.Mode()
	}
	if mode&os.ModeSocket == 0 {
		return fmt.Errorf("%w: %s is not a socket", ErrSocketDirMode, sockPath)
	}
	perm := mode.Perm()
	if perm&0077 != 0 {
		// Tighten to EXACTLY owner-rw: a socket needs no execute bit, and
		// "owner rwx" (0700) would still carry a bit no socket has a use
		// for. This only ever REMOVES bits — never widen.
		if err := os.Chmod(sockPath, SocketFileMode); err != nil {
			return fmt.Errorf("%w: chmod %s: %v", ErrSocketDirMode, sockPath, err)
		}
	}
	return nil
}

// SocketFileMode is the tightened mode for the socket FILE itself (§6.1:
// not group- or world-accessible): owner read/write, nothing else.
const SocketFileMode os.FileMode = 0600

// AssertSocketFileNotGroupWorld stats the socket file back after Ensure and
// FAILS if any group/world bit survived — the assertion, not the attempt.
func AssertSocketFileNotGroupWorld(sockPath string) error {
	fi, err := os.Stat(sockPath)
	if err != nil {
		return fmt.Errorf("%w: stat %s: %v", ErrSocketDirMode, sockPath, err)
	}
	mode := fi.Mode()
	if mode&os.ModeSocket == 0 {
		return fmt.Errorf("%w: %s is not a socket", ErrSocketDirMode, sockPath)
	}
	if mode.Perm()&0077 != 0 {
		return fmt.Errorf("%w: socket file %s is group/world accessible (%#o)", ErrSocketDirMode, sockPath, mode.Perm())
	}
	return nil
}

// ── SO_PEERCRED peer verification (spec §6.2) ───────────────────────────
//
// The mechanism, verified against this Go version's stdlib (go1.26.5):
//   - syscall.GetsockoptUcred(fd, syscall.SOL_SOCKET, syscall.SO_PEERCRED)
//     is at $(go env GOROOT)/src/syscall/syscall_linux.go:759 and returns
//     the kernel's authoritative *Ucred{Pid, Uid, Gid} for a connected
//     unix-stream peer;
//   - net.UnixConn (therefore also connections accepted from a
//     *net.UnixListener, whose concrete type is *net.UnixConn) exposes
//     SyscallConn() (RawConn, error) for reaching the raw fd.
//
// The SERVER side uses this to refuse any peer whose uid is not the uid the
// endpoint is owned for — SO_PEERCRED is how a socket is made safe even in
// a shared path (§6.2). The client side verifies the SERVER's socket file
// as well (an ownership read-back): it refuses to talk to a socket whose
// owner is not the agent user, so a spawn on a host where the chown was
// lost fails LOUD instead of handing the agent's docker control channel to
// a stranger's listening process.

// peerCredentialFor is the process-global accept hook: a unix-socket
// SERVER that wants owner-uid enforcement calls this once per accepted
// connection, BEFORE any privileged or mutating action on that connection,
// and closes the connection on refusal. Tests swap it to drive verdict
// tables without foreign uids on the host.
var peerCredentialFor = unixPeerCredential

// unixPeerCredential asks the KERNEL which uid connected, via SO_PEERCRED.
// It never trusts anything the peer could have written.
func unixPeerCredential(c net.Conn) (uint32, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("SO_PEERCRED requires a *net.UnixConn, got %T", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("SO_PEERCRED: SyscallConn: %w", err)
	}
	var uid uint32
	var sockErr error
	if ctlErr := raw.Control(func(fd uintptr) {
		ucred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err != nil {
			sockErr = err
			return
		}
		if ucred == nil {
			sockErr = errors.New("kernel returned no peer credentials")
			return
		}
		uid = ucred.Uid
	}); ctlErr != nil {
		return 0, fmt.Errorf("SO_PEERCRED: raw control: %w", ctlErr)
	}
	if sockErr != nil {
		return 0, fmt.Errorf("SO_PEERCRED: GetsockoptUcred: %w", sockErr)
	}
	return uid, nil
}

// CheckUnixSocketPeer is THE owner-uid gate for an accepted unix-socket
// connection. It returns nil only when the kernel-reported peer uid equals
// ownerUID; every refusal wraps ErrForeignPeerUID and names the expected
// and the arriving uid (uids are not secrets — the refusal must be
// diagnosable). It performs no I/O on the connection: call it FIRST, before
// any privileged or mutating action.
func CheckUnixSocketPeer(c net.Conn, ownerUID uint32) error {
	peerUID, err := peerCredentialFor(c)
	if err != nil {
		return fmt.Errorf("%w: could not verify peer uid (expected %d): %v", ErrForeignPeerUID, ownerUID, err)
	}
	if peerUID != ownerUID {
		return fmt.Errorf("%w: unix socket endpoint owned by uid %d, peer connected as uid %d — refusing", ErrForeignPeerUID, ownerUID, peerUID)
	}
	return nil
}

// VerifySocketOwnership is the CLIENT-side read-back of the same property
// (the stat-it-back discipline GAP-075 applies to the scratch root, §6.3).
// A client about to send docker commands over a unix socket refuses a
// socket whose owner is not the expected agent uid: the spawn path chowns
// the socket directory to the agent, so a socket file owned by anyone else
// means the boundary was not provisioned as claimed, and proceeding would
// hand the agent's docker control channel to a foreign listener. "Exists"
// is not ownership — stat the owner back. A symlink at sockPath is
// followed (the logical path is a symlink to /run/user/<uid>/docker.sock
// once waitForDockerd reconciles it); the TARGET's owner is what speaks.
func VerifySocketOwnership(sockPath string, expectedUID uint32) error {
	fi, err := os.Lstat(sockPath)
	if err != nil {
		return fmt.Errorf("%w: socket path %s: %v", ErrForeignPeerUID, sockPath, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		fi, err = os.Stat(sockPath)
		if err != nil {
			return fmt.Errorf("%w: socket path %s (following symlink): %v", ErrForeignPeerUID, sockPath, err)
		}
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: cannot read owner of %s (%T)", ErrForeignPeerUID, sockPath, fi.Sys())
	}
	if st.Uid != expectedUID {
		return fmt.Errorf("%w: socket %s is owned by uid %d, expected agent uid %d — refusing to use it", ErrForeignPeerUID, sockPath, st.Uid, expectedUID)
	}
	return nil
}
