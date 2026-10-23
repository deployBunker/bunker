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
// OWNER-UNIT CONTRACT (why ROOT ownership is right, and why the mode is 0711
// rather than 0700). bunkerd provisions agents as ROOT by construction — the
// spawn path runs `chown -R` to arbitrary users (manager_spawn.go Step 4),
// writes /etc/bunkerd, /home/<user>/.ssh and /var/lib/bunkerd, and creates
// systemd SYSTEM units (`systemd-run --system --uid=`, isolation.go
// buildRootlessDockerdArgs) — and its socket/symlink reconciliation
// (waitForDockerd symlinking /run/bunker/<id>/docker.sock →
// /run/user/<uid>/) also runs as root. As root it bypasses DAC, so a
// root-owned parent is transparent to every owner-unit operation, and root
// ownership is asserted below.
//
// What an agent USER may still do is bounded by the MODE, and the mode must
// separate two powers the earlier 0700 conflated:
//
//   - LISTING the parent (glob the tenant set) needs READ — that is the
//     enumeration seam §1.8 names, and group/world READ stays 0.
//   - REACHING its own /run/bunker/<id> needs EXECUTE on the parent — and
//     this is REQUIRED, not incidental: the agent's advertised docker
//     control channel lives at /run/bunker/<id>/docker.sock, so 0700 left
//     every agent unable to open its own DOCKER_HOST (INT-CI-080; measured
//     live on bunker-mvp — see the INT-CI-080 block below). A peer's child
//     is 0700 and agent-owned, so a guessed peer name still refuses at the
//     child; only the agent's OWN child is reachable.
//
// Hence 0711: owner rwx, no group/world READ or WRITE (no enumeration, no
// tampering), others execute-only (traverse to one's own child). The root-side
// paths (`bcli exec` as root, the E2E battery's root-side checks, the daemon's
// own symlink work) are unaffected either way — root bypasses DAC — which is
// exactly why a root-only view could not detect the 0700 breakage.

// ── INT-CI-080: why the parent is 0711 and NOT 0700 ────────────────────────
//
// The 0755→0700 repair above sealed enumeration but ALSO removed the
// others-execute bit, and that broke the contract §6 depends on: the agent's
// own docker control channel is advertised at /run/bunker/<id>/docker.sock
// (authorized_keys environment=, .profile, docker tunnels), which is INSIDE
// this parent. Path traversal needs +x on EVERY ancestor, so a root:0700
// parent makes the agent's own DOCKER_HOST unopenable:
//
//	dial unix /run/bunker/<id>/docker.sock: permission denied
//
// and every agent-side docker call fails (live-reproduced on bunker-mvp: a
// direct `su - bunker-<id> -c 'docker ps'` returned 1 with parent 0700 and 0
// with parent 0711, on the same running dockerd). Root is unaffected — root
// bypasses DAC — so the FAILURE IS INVISIBLE to every root-side check: the
// spawn's own symlink work, `stat -S` on the logical path, and ssh `-L`
// forward setup (the sshd monitor runs as root) all keep succeeding while no
// agent can connect. That is why the E2E battery's root-side "docker socket
// exists" cell went green while the very next agent-side cells failed.
//
// The threat §1.8 names is ENUMERATION (`ls /run/bunker/` listing the tenant
// set), and enumeration needs READ. Traversal of a KNOWN name needs only
// EXECUTE, and a peer's child is 0700 agent-owned, so a guessed peer name
// still refuses at the child. 0711 therefore keeps the seal that matters
// (group/world READ = 0, so no listing) while restoring the agent's reach to
// its OWN socket. The original text's premise — "the daemon does its
// socket/symlink work as root so it does not need to traverse as the agent" —
// only examined the DAEMON's paths and missed the AGENT's.
//
// RunRootMode is the REQUIRED mode of the /run/bunker parent: owner rwx,
// group/world READ and WRITE keep zero bits (nothing can list the tenant set
// or modify the parent), and others keep EXECUTE only — the one bit an agent
// needs to reach its OWN child. Group execute is inert here (the parent is
// root:root, so only root — which bypasses DAC — is in its group). Same
// discipline as SocketDirMode: set explicitly, then stat-ed back — a chmod
// that exited 0 is not proof. (specs/network-isolation.md §1.8 carries the
// same amendment.)
const RunRootMode os.FileMode = 0711

// RunRootOwnerUID is the REQUIRED owner of the /run/bunker parent: root
// (the bunkerd owner-unit identity; see the file comment above — the daemon
// runs every provisioning operation as root by construction).
const RunRootOwnerUID uint32 = 0

// ErrRunRootMode is the sentinel carried by every parent-directory failure
// (mode, owner, or filesystem error) so callers and tests can tell "the
// /run/bunker parent is not 0711 root-owned" apart from unrelated errors.
var ErrRunRootMode = errors.New("spawn runtime root (/run/bunker) mode/ownership assertion failed")

// ErrRunRootForeignMount is the NAMED refusal for a parent whose stat
// device changes across the Ensure repair: the path was re-rooted between
// our two stats (e.g. a bind-mounted foreign filesystem owns the path now),
// and enforcing a mode on a filesystem this daemon does not own would
// manufacture a boundary we cannot hold. The spawn FAILS with this error
// rather than creating an agent directory underneath a parent that may no
// longer be ours.
var ErrRunRootForeignMount = errors.New("spawn runtime root is owned by a foreign mount/filesystem (device identity changed around the mode repair); refusing to create agent state underneath")

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
// (0711 — the negative case reddens for 0755/0750 because group/world READ is
// set, and for 0700 because without others-execute the agent cannot reach its
// OWN /run/bunker/<id>/docker.sock; THE binding half is the mode, and it is
// never identity-scoped), and — only when this process can actually own
// root state, i.e. when running as root — the owner really is root
// (RunRootOwnerUID, the bunkerd owner-unit identity, see the file header).
// The owner half is identity-scoped because the spawning unit IS root in
// production: it must assert, and repairable, ownership it actually
// controls. On a non-root test process (the hermetic suite cannot chown,
// and an agent could not either) the mode half alone still seals the
// enumeration seam: a world/group-readable root reddens regardless of who
// owns it, and a non-root-OWNED 0711 root still gives non-owners zero READ
// (no enumeration) — traversal of a known child name is bounded by that
// child's own 0700 mode.
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
		return fmt.Errorf("%w: %s mode is %#o, required %#o (group/world READ must be zero — enumeration seam, ISO-002 — and others-execute must be SET so an agent can traverse to its own child; 0700 is the INT-CI-080 breakage)", ErrRunRootMode, root, got, RunRootMode)
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
// asserted state: mode 0711, owner root (owner half only where identity
// permits — see repairingChownAgentRunRoot). On a host whose /run is a
// tmpfs (the common case), the mode RESETS on every reboot — that is FINE
// by design: the spawn-time assertion is the mechanism, not boot-time
// config. The Ensure REPAIRS in situ (MkdirAll with RunRootMode then an
// explicit Chmod, which forces the mode on a pre-existing directory and
// strips umask bits — including a stale 0700 or 0755 from an older daemon)
// and the caller asserts back with AssertAgentRunRoot — refusing only if the
// re-assert genuinely cannot pass.
//
// The repair is wrapped by the foreign-mount guard: the stat device of the
// path is captured BEFORE the repair and compared AFTER it. If the device
// changed (e.g. this path became a bind-mounted foreign filesystem, or a
// mount was swapped in around us), the repair did not just fix OUR
// directory — it landed on someone else's filesystem — and the caller
// REFUSES with ErrRunRootForeignMount instead of proceeding.
//
// The mode check is the LAST step of Ensure as well as the caller's Assert:
// if the repair genuinely cannot make the path 0711 (a foreign mount we
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
		return fmt.Errorf("%w: %s stat device changed (%d → %d) across the mode repair", ErrRunRootForeignMount, root, beforeDev, afterDev)
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
