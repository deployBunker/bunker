//go:build linux

package agent

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ISO-002 — the enumeration seam on the /run/bunker PARENT.
//
// Each per-agent socket directory (/run/bunker/<id>) is already 0700 and
// agent-owned (EnsureAgentSocketDir + AssertAgentSocketDir, spec
// specs/network-isolation.md §6), but the PARENT was created implicitly by
// MkdirAll's side effect at mode 0755, which let every agent `ls /run/bunker/`
// and enumerate the tenant set (ISO-002, demonstrated live on bunker-las-03,
// spec §1.8 "Path visibility").
//
// OWNER-UNIT CONTRACT (why 0700 here cannot break the daemon). The design
// decision is anchored to spec §1.8: "root:0700 on the parent and per-agent
// 0700 children ... the daemon does its socket/symlink work as root so it
// does not need to traverse as the agent". Verified against code: bunkerd
// provisions agents as ROOT by construction — the spawn path runs
// `chown -R` to arbitrary users (manager_spawn.go Step 4), writes
// /etc/bunkerd, /home/<user>/.ssh and /var/lib/bunkerd, and creates systemd
// SYSTEM units (`systemd-run --system --uid=`, isolation.go
// buildRootlessDockerdArgs) — none of which an unprivileged process can do —
// and its socket/symlink reconciliation (waitForDockerd symlinking
// /run/bunker/<id>/docker.sock → /run/user/<uid>/) also runs as root. As
// root it bypasses DAC, so a 0700 root-owned parent is transparent to every
// owner-unit operation. What an agent USER loses is the design goal:
// list/traverse rights on PEERS' directories (spec §1.8) — enumeration is
// dead even though a peer directory NAME under our own parent would still be
// walkable by uid if one could be guessed. Full access to an agent's OWN
// child directory (0700, owner rwx, chowned to the agent) is unchanged; the
// daemon and the owner's administrative sessions (`bcli exec`, the E2E
// battery's root-side checks) act as root or via the owner daemon, never as
// the peer agent.

// RunRootMode is the REQUIRED mode of the /run/bunker parent — group and
// world keep zero bits, so an agent cannot enumerate or traverse peers.
// Same discipline as SocketDirMode: set explicitly, then stat-ed back — a
// chmod that exited 0 is not proof.
const RunRootMode os.FileMode = 0700

// RunRootOwnerUID is the REQUIRED owner of the /run/bunker parent: root
// (the bunkerd owner-unit identity; see the file comment above — the daemon
// runs every provisioning operation as root by construction).
const RunRootOwnerUID uint32 = 0

// ErrRunRootMode is the sentinel carried by every parent-directory failure
// (mode, owner, or filesystem error) so callers and tests can tell "the
// /run/bunker parent is not 0700 root-owned" apart from unrelated errors.
var ErrRunRootMode = errors.New("spawn runtime root (/run/bunker) mode/ownership assertion failed")

// ErrRunRootForeignMount is the NAMED refusal for a parent whose stat
// device changes across the Ensure repair: the path was re-rooted between
// our two stats (e.g. a bind-mounted foreign filesystem owns the path now),
// and enforcing a mode on a filesystem this daemon does not own would
// manufacture a boundary we cannot hold. The spawn FAILS with this error
// rather than creating an agent directory underneath a parent that may no
// longer be ours.
var ErrRunRootForeignMount = errors.New("spawn runtime root is owned by a foreign mount/filesystem (device identity changed around the 0700 repair); refusing to create agent state underneath")

// statRunRootMountForTests lets hermetic harnesses model the before/after
// device identity the foreign-mount refusal compares: nil uses the REAL
// Stat_t device from the host (the production behavior).
var statRunRootMountForTests func(root string) (dev uint64, ok bool)

// runRootDevice reads the stat device of root (the host's mount identity).
func runRootDevice(root string) (uint64, bool) {
	if statRunRootMountForTests != nil {
		return statRunRootMountForTests(root)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(root, &st); err != nil {
		return 0, false
	}
	return st.Dev, true
}

// AssertAgentRunRoot STATS the parent back and fails unless the
// enumerations-sealing properties hold: the mode is exactly RunRootMode
// (0700 — the negative case reddens for 0755 because group/world bits are
// set — THE binding half: mode is what kills `ls /run/bunker/`, and it is
// never identity-scoped), and — only when this process can actually own
// root state, i.e. when running as root — the owner really is root
// (RunRootOwnerUID, the bunkerd owner-unit identity, see the file header).
// The owner half is identity-scoped because the spawning unit IS root in
// production: it must assert, and repairable, ownership it actually
// controls. On a non-root test process (the hermetic suite cannot chown,
// and an agent could not either) the mode half alone still seals the
// enumeration seam: a world/group-readable root reddens regardless of who
// owns it, and a non-root-OWNED 0700 root still gives non-owners zero
// enumerate/traverse reach.
//
// This is the read-back half of the ISO-002 seam: the spawn calls it right
// after EnsureAgentRunRoot and BEFORE the first per-agent directory is
// created, so a parent that LOOKS restricted but is not fails the spawn
// instead of manufacturing confidence — the same assert-not-assume law the
// per-agent directory already follows (AssertAgentSocketDir, §6.3).
//
// runRootOwnerForTests is the ownership read-back seam, SEPARATE from the
// socket-dir assertion's statOwnerForTests: the recycled-uid and rollback
// fixtures stub statOwnerForTests to the AGENT uid for long spans, and the
// root's owner must never inherit that modeling — the root's ownership is
// the one boundary this assertion exists to prove. nil uses the real
// Stat_t owner.
var runRootOwnerForTests func(root string) (uid uint32, ok bool)

func AssertAgentRunRoot(root string) error {
	fi, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("%w: stat %s: %v", ErrRunRootMode, root, err)
	}
	if got := fi.Mode().Perm(); got != RunRootMode {
		return fmt.Errorf("%w: %s mode is %#o, required %#o (group/world bits must be zero — enumeration seam, ISO-002)", ErrRunRootMode, root, got, RunRootMode)
	}
	// Owner assertion: identity-scoped to the root spawning unit. In
	// production bunkerd IS root (see the file header); the assertion there
	// is a real read-back of the real chown. Under a non-root test process
	// the chown is impossible, the identity scope does not apply, and the
	// mode half (above) still carries the enumeration seal.
	if syscall.Geteuid() == 0 {
		ownerUID, ownerOK := func() (uint32, bool) {
			if runRootOwnerForTests != nil {
				return runRootOwnerForTests(root)
			}
			stat, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, false
			}
			return stat.Uid, true
		}()
		if !ownerOK {
			return fmt.Errorf("%w: cannot read owner of %s (%T)", ErrRunRootMode, root, fi.Sys())
		}
		if ownerUID != RunRootOwnerUID {
			return fmt.Errorf("%w: %s owned by uid %d, expected root (uid %d)", ErrRunRootMode, root, ownerUID, RunRootOwnerUID)
		}
	}
	return nil
}

// repairingChownAgentRunRoot is the owner-assertion half of the repair: on
// a process that CAN own root state (production bunkerd runs as root) it
// chowns the parent back to root — the owner read-back that follows proves
// it. On a non-root process it is a NO-OP: os.Chown would ENOFAIL, the
// assertion would then demand an ownership repair that is impossible, and
// a hermetic fixture would fail on an identity half it never controlled.
func repairingChownAgentRunRoot(root string) error {
	if syscall.Geteuid() != 0 {
		return nil
	}
	if err := syscall.Chown(root, int(RunRootOwnerUID), -1); err != nil {
		return fmt.Errorf("%w: chown %s to root: %v", ErrRunRootMode, root, err)
	}
	return nil
}

// EnsureAgentRunRoot creates (or repairs) the /run/bunker parent to the
// asserted state: mode 0700, owner root (owner half only where identity
// permits — see repairingChownAgentRunRoot). On a host whose /run is a
// tmpfs (the common case), the mode RESETS on every reboot — that is FINE
// by design: the spawn-time assertion is the mechanism, not boot-time
// config. The Ensure REPAIRS in situ (MkdirAll with 0700 then an explicit
// Chmod, which forces the mode on a pre-existing directory and strips
// umask bits) and the caller asserts back with AssertAgentRunRoot —
// refusing only if the re-assert genuinely cannot pass.
//
// The repair is wrapped by the foreign-mount guard: the stat device of the
// path is captured BEFORE the repair and compared AFTER it. If the device
// changed (e.g. this path became a bind-mounted foreign filesystem, or a
// mount was swapped in around us), the repair did not just fix OUR
// directory — it landed on someone else's filesystem — and the caller
// REFUSES with ErrRunRootForeignMount instead of proceeding.
//
// The mode check is the LAST step of Ensure as well as the caller's Assert:
// if the repair genuinely cannot make the path 0700 (a foreign mount we
// cannot chmod, for example), Ensure fails with the assertion error, not a
// silent "attempted". The named "foreign filesystem mount owns the path"
// refusal lives on the DEVICE evidence (ErrRunRootForeignMount); a plain
// chmod failure surfaces through ErrRunRootMode.
func EnsureAgentRunRoot(root string) error {
	beforeDev, hadMount := runRootDevice(root)
	if err := os.MkdirAll(root, RunRootMode); err != nil {
		return fmt.Errorf("%w: mkdir %s: %v", ErrRunRootMode, root, err)
	}
	if err := os.Chmod(root, RunRootMode); err != nil {
		return fmt.Errorf("%w: chmod %s: %v", ErrRunRootMode, root, err)
	}
	afterDev, fsOK := runRootDevice(root)
	if !fsOK {
		return fmt.Errorf("%w: cannot stat %s to verify the mount identity", ErrRunRootForeignMount, root)
	}
	if hadMount && afterDev != beforeDev {
		// The path was re-rooted between the two stats. Refuse with the
		// NAMED error (ISO-002 §2) instead of asserting a boundary on a
		// filesystem we do not own.
		return fmt.Errorf("%w: %s stat device changed (%d → %d) across the 0700 repair", ErrRunRootForeignMount, root, beforeDev, afterDev)
	}
	// Owner repair: the identity-scoped chown back to root (a no-op where
	// the process cannot own root state — see repairingChownAgentRunRoot).
	if err := repairingChownAgentRunRoot(root); err != nil {
		return err
	}
	// Assert the repaired state back — the assertion, not the attempt, is
	// the proof (mirror of the socket-dir read-back, §6.3).
	return AssertAgentRunRoot(root)
}
