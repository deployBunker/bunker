package hostsetup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Shared-scratch layout (GAP-075):
//
//	/srv/bunker-share            root:bunker-agents 2750 root-owned mountpoint tree
//	/srv/bunker-share/<agent-id> bunker-<id>:bunker-agents 2770, tmpfs size=<cap>
//
// The scratch ROOT is setgid (so entries an agent creates inside its OWN
// directory inherit the agent group) but deliberately NOT group-writable and
// not world-accessible: with both write bits clear, a member of the agent group
// can traverse the root and read a peer's directory, but cannot create an
// arbitrary plain directory or file directly underneath it. That matters
// because a plain entry under the root is exactly the escape from the per-agent
// tmpfs cap: an unbounded, uncapped write surface inside the exchange tree.
// Only the daemon (root) creates entries there, and it creates them as bounded
// tmpfs mounts.
//
// Per-agent directories are group-writable and setgid, which is what makes
// cross-agent exchange work (an agent reads a peer's directory; files created
// there inherit the agent group). Every agent directory is a tmpfs mounted with
// an explicit size, so the per-agent bound is enforced by the kernel — a full
// scratch fails with ENOSPC instead of filling the host filesystem or RAM
// unbounded.
const (
	// ScratchRootMode is the mode of the scratch root: root-owned, agent group
	// read/traverse only. The setgid bit makes new entries inherit the agent
	// group; the group and world write bits are CLEAR, so an agent cannot
	// create entries directly under the root (see the package comment above).
	ScratchRootMode os.FileMode = 0o2750
	// ScratchDirMode is the mode of a per-agent scratch directory: agent
	// group read/write/exchange with the setgid bit set so files created
	// inside inherit the agent group.
	ScratchDirMode os.FileMode = 0o2770
	// ScratchFSType is the filesystem mounted for each agent directory.
	ScratchFSType = "tmpfs"
)

// ScratchMountOptions renders the mount(8) -o options for a per-agent scratch
// tmpfs. size= is the per-agent bound. nosuid stops a setuid/setgid binary in
// an exchanged file from gaining privilege, and nodev stops an exchanged device
// node from being opened — neither one restricts EXECUTING a plain file, which
// is deliberately still allowed here: the exchange point exists to hand
// artifacts (including binaries) between agents, and the size cap is what
// bounds it. Any write outside the cap fails with ENOSPC before it can reach
// the host filesystem.
func ScratchMountOptions(sizeBytes uint64) string {
	return fmt.Sprintf("size=%d,mode=0770,nosuid,nodev", sizeBytes)
}

// ScratchMountArgs is the exact `mount` argv that creates the bounded
// per-agent scratch filesystem. Pure so tests can pin it.
func ScratchMountArgs(dir string, sizeBytes uint64) []string {
	return []string{"-t", ScratchFSType, "-o", ScratchMountOptions(sizeBytes), ScratchFSType, dir}
}

// ScratchRemountArgs is the exact `mount` argv that re-applies a changed size
// bound to an already-mounted scratch (non-destructive: contents survive).
func ScratchRemountArgs(dir string, sizeBytes uint64) []string {
	return []string{"-o", fmt.Sprintf("remount,size=%d", sizeBytes), dir}
}

// EnsureSharedScratch provisions the shared scratch root and its group:
// the group exists, and the root directory exists with group ownership, the
// setgid bit and NO group/world write bit (mode 2750). Idempotent: a second
// call reports skips and changes nothing.
//
// The mode is re-asserted on every call, so a root that already existed
// (operator-created, or left over from a failed run) can never keep a wider
// mode; and the result is STAT-ED BACK afterwards, because "the chmod command
// exited 0" is not proof that a group-writable root is gone. A root that is
// still writable by the agent group is the exact bypass this exists to close
// (an agent could create a plain, uncapped entry next to its capped directory),
// so a mode that does not match is a hard error, never a warning.
func (o Options) EnsureSharedScratch(ctx context.Context) (*Report, error) {
	o = o.WithDefaults()
	rep := &Report{}

	if err := o.ensureAgentGroup(ctx, rep); err != nil {
		return rep, err
	}

	if err := o.provisionScratchRoot(ctx, rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// provisionScratchRoot creates the scratch root when it is missing and
// enforces the documented ownership and mode on every call. Idempotent: a
// second call against a correct root issues no command that changes anything
// and reports no mutation.
//
// The mode is re-asserted on every call, so a root that already existed
// (operator-created, or left over from a failed run) can never keep a wider
// mode; and the result is STAT-ED BACK afterwards, because "the chmod command
// exited 0" is not proof that a group-writable root is gone. A root that is
// still writable by the agent group is the exact bypass this exists to close
// (an agent could create a plain, uncapped entry next to its capped directory),
// so a mode that does not match is a hard error, never a warning.
func (o Options) provisionScratchRoot(ctx context.Context, rep *Report) error {
	if err := os.MkdirAll(o.ScratchRoot, ScratchRootMode); err != nil {
		return fmt.Errorf("create scratch root %s: %w", o.ScratchRoot, err)
	}
	rep.Add("ok", o.ScratchRoot, "scratch root exists", true)

	// chown root:<group> and enforce the mode every time: a directory that
	// already existed (operator-created, or left over from a failed run)
	// must not keep a wider mode.
	gid, err := o.agentGroupID(ctx)
	if err != nil {
		return err
	}
	if _, err := o.run(ctx, "chown", fmt.Sprintf("0:%s", gid), o.ScratchRoot); err != nil {
		return fmt.Errorf("chown scratch root: %w", err)
	}
	if _, err := o.run(ctx, "chmod", fmt.Sprintf("%04o", ScratchRootMode), o.ScratchRoot); err != nil {
		return fmt.Errorf("chmod scratch root: %w", err)
	}
	// Re-assert the mode in-process as well. Go's FileMode cannot express the
	// octal 2750 spelling — the setgid flag is ModeSetgid, not 0o2000, so
	// os.Chmod(0o2750) would silently drop the bit — while the shell chmod
	// above is the auditable, operator-facing statement of it. Doing both keeps
	// the enforcement independent of the chmod binary AND makes the stat-back
	// below a real check in every runtime, including a fake command runner.
	if err := os.Chmod(o.ScratchRoot, os.ModeSetgid|ScratchRootMode.Perm()); err != nil {
		return fmt.Errorf("enforce scratch root mode in-process: %w", err)
	}
	rep.Add("chmod", o.ScratchRoot, fmt.Sprintf("owner 0:%s mode %04o (setgid, no group/world write)", gid, ScratchRootMode), true)

	if err := verifyScratchRoot(o.ScratchRoot); err != nil {
		return err
	}
	return nil
}

// ensureSharedScratchRoot brings the exchange root to its documented shape
// before a per-agent directory is provisioned under it, and fails loudly when
// it cannot.
//
// Why the spawn path needs this at all: inside /srv/bunker-share only the
// DAEMON ever creates entries, so the root can only be damaged by an operator
// (or an earlier revision) — and MkdirAll on an agent directory whose parent
// is MISSING inherits the parent mode, which is how the root ends up
// root:root 0750 instead of root:bunker-agents 2750. That root is unusable to
// an agent (mode 0750 denies traversal to non-root), yet before this gate the
// spawn path reported "shared scratch ready" anyway because it only ever
// touched the per-agent directory. The consequence was measured live: three
// agents with correct membership and correctly owned per-agent directories
// could not even `ls` the exchange point.
//
// The remedy is per-agent, not "run host-provision at some point": the root is
// repaired in place where it is only chown/chmod, and created correctly where
// it is missing — and if it cannot be repaired (a root that is not a
// directory, or an ownership/mode that survives root privilege) the per-agent
// step FAILS, naming the operator remedy, instead of provisioning a directory
// the agent cannot reach.
//
// Cost control, which is why the root is verified before it is re-provisioned:
// on a host whose root is already correct this issues at most TWO read-only
// probes (stat, plus getent when the group is unresolvable) and — the part
// that matters for the spawn path — ZERO commands that change host state. The
// unconditional repave (four commands per spawn, on a directory every spawn
// shares) is deliberately avoided.
func (o Options) ensureSharedScratchRoot(ctx context.Context, rep *Report) error {
	// Remediation requires root: chowning the root to root:<group> always
	// needs it, so "repairable in place" is exactly "this process is root" —
	// even when, as on a correct host, no repair turns out to be needed. When
	// it is not, the root is still checked and reported below.
	repairable := scratchRootRepairable()

	stat, err := o.statScratchRoot(ctx)
	if err != nil {
		return err
	}
	// The group's gid is needed to verify ownership: the documented root is
	// root:<agent-group>, so a root left root:root is only half-checked by its
	// mode. When the owner cannot be observed at all there is no ownership to
	// verify and no gid to look up — the mode, which is always observable, is
	// then the check that applies (the same fallback the session-time
	// pam_namespace helper uses when it cannot be root).
	gid := ""
	if stat.Owner != "" {
		if gid, err = o.agentGroupID(ctx); err != nil {
			return err
		}
	}

	// The two independent halves of the boundary, checked before anything is
	// changed: the mode (read from the real directory, so it holds even under
	// a fake runner) and the ownership (read from the probe, since a real host
	// keeps root ownership out of a sandboxed path's reach).
	modeErr := verifyScratchRoot(o.ScratchRoot)
	ownerErr := o.scratchRootOwnershipError(stat, gid)
	if modeErr != nil || ownerErr != nil {
		reason := errors.Join(modeErr, ownerErr)
		if !stat.isDirectory() {
			reason = fmt.Errorf("scratch root %s does not exist, or is not a directory", o.ScratchRoot)
		}
		if !repairable {
			return o.unreachableScratchRoot(reason)
		}
		if perr := o.provisionScratchRoot(ctx, rep); perr != nil {
			return o.unreachableScratchRoot(perr)
		}
		if gid != "" {
			// The ownership half cannot be repaired blindly: the root may have
			// been left root:root by exactly the MkdirAll this replaces, so it
			// is re-observed rather than assumed.
			if verr := o.verifyScratchRootOwnership(gid); verr != nil {
				return o.unreachableScratchRoot(verr)
			}
		}
		// Record the state the AGENT will meet, not the state that triggered
		// the repair.
		if final, ferr := o.statScratchRoot(ctx); ferr == nil {
			stat = final
		}
	}

	// The observation, recorded even when nothing needed repairing: it is the
	// evidence that this spawn checked the exchange root — the check a healthy
	// host does silently — so "the root was never looked at" cannot be
	// mistaken for "the root was checked and was right".
	rep.Add("ok", o.ScratchRoot, fmt.Sprintf("exchange root verified %s mode %s (owner root, group %s, setgid, no group/world write)",
		stat.ownerLabel(), rootModeForReport(o.ScratchRoot, stat), o.AgentGroup), false)
	return nil
}

// scratchRootOwnershipError reports the ownership half of the boundary: the
// root must be owned by root and group-owned by the agent group. An
// unobservable owner is skipped (nil), never invented — the mode check is then
// the one that applies.
func (o Options) scratchRootOwnershipError(stat scratchRootStat, gid string) error {
	if !stat.isDirectory() || stat.Owner == "" {
		return nil
	}
	if stat.Owner != "0" {
		return fmt.Errorf("scratch root %s is owned by uid %s, want 0 (root): a non-root owner is not traversable by the agent sessions, so the exchange tree is unreachable", o.ScratchRoot, stat.Owner)
	}
	if gid != "" && stat.Group != "" && stat.Group != gid {
		return fmt.Errorf("scratch root %s is group-owned by gid %s, want %s (the agent group): agents outside that group cannot read the exchange tree", o.ScratchRoot, stat.Group, gid)
	}
	return nil
}

// ownerLabel renders the observed owner for a report line.
func (s scratchRootStat) ownerLabel() string {
	if s.Owner == "" {
		return "owner unobservable"
	}
	return s.Owner + ":" + s.Group
}

// rootModeForReport renders the mode the root has RIGHT NOW, preferring an
// in-process read (which reflects a repair made earlier in the same call) over
// the octal column of the probe.
func rootModeForReport(root string, st scratchRootStat) string {
	if fi, err := os.Stat(root); err == nil {
		return octalMode(fi.Mode().Perm() | (fi.Mode() & os.ModeSetgid))
	}
	if st.Octal != "" {
		return st.Octal
	}
	return "unknown"
}

// octalMode renders permission bits plus the setgid flag the way `stat -c '%a'`
// does ("2750"). It exists because os.FileMode cannot be printed with %04o:
// ModeSetgid is 1<<22, not 0o2000, so %04o on a mode carrying it prints the raw
// flag word instead of the octal spelling an operator reads.
func octalMode(m os.FileMode) string {
	if m&os.ModeSetgid != 0 {
		return fmt.Sprintf("2%03o", m.Perm())
	}
	return fmt.Sprintf("%03o", m.Perm())
}

// OctalModeForTest renders a FileMode the way `stat -c '%a'` does. It is
// exported only so tests in OTHER packages (internal/agent, whose fake host has
// to answer the `stat` probe) can build a faithful observation without
// duplicating the setgid-with-octal trap.
func OctalModeForTest(m os.FileMode) string { return octalMode(m) }

// unreachableScratchRoot wraps a root that could not be brought to its
// documented shape with the consequence and the operator remedy, so a failed
// spawn names something actionable instead of a bare chmod error.
func (o Options) unreachableScratchRoot(err error) error {
	return fmt.Errorf("shared scratch root %s is not usable as the cross-agent exchange point (%w); the agent would be unable to list or read the exchange tree, and a per-agent directory was NOT created under it — run `bunker host-provision --apply` on the host to repair the exchange root", o.ScratchRoot, err)
}

// scratchRootRepairable reports whether this process may change the exchange
// root's owner and group. It is a seam for the same reason the boundary's other
// privilege checks are: the fail-closed branch (an unprivileged process facing
// a wrong root) cannot be exercised by a test whose runner happens to be root,
// and a branch that cannot fail its test is not a verified branch.
// Production always uses privileged().
var scratchRootRepairable = privileged

// gidOrDefault renders the expected gid for an error message, falling back to
// the group NAME when the gid could not be resolved (an unreadable owner).
func gidOrDefault(gid, group string) string {
	if gid != "" {
		return gid
	}
	return group
}

// scratchRootStat is the observed owner, group and mode of the scratch root,
// as read from the filesystem (never from a chown/chmod argv).
type scratchRootStat struct {
	// Exists reports whether the path lstat-ed to anything at all.
	Exists bool
	// IsDir reports whether the path is a directory.
	IsDir bool
	// Mode carries the permission bits plus the setgid flag.
	Mode os.FileMode
	// Owner, Group and Octal are the numeric uid, numeric gid and the octal
	// mode as reported by `stat -c '%u:%g %a'`. Empty when unobservable.
	Owner string
	Group string
	Octal string
}

func (s scratchRootStat) isDirectory() bool { return s.Exists && s.IsDir }

// statScratchRoot observes the scratch root without changing anything. The
// owner/group columns come from `stat` (a probe on the Runner seam, so a fake
// host can answer them) while existence and mode are read in-process, which is
// always available and is what keeps a mode check meaningful under a fake
// runner.
//
// Only a non-zero exit from `stat` (the path is absent, or the binary is
// missing) leaves the owner unobserved; the Run error already carries the
// command's output, so no second call is needed to quote it.
func (o Options) statScratchRoot(ctx context.Context) (scratchRootStat, error) {
	var st scratchRootStat

	fi, err := os.Lstat(o.ScratchRoot)
	switch {
	case err == nil:
		st.Exists = true
		st.IsDir = fi.IsDir()
		st.Mode = fi.Mode().Perm() | (fi.Mode() & os.ModeSetgid)
	case errors.Is(err, fs.ErrNotExist):
		return st, nil // absent: not an error, the caller provisions it
	default:
		return st, fmt.Errorf("stat scratch root %s: %w", o.ScratchRoot, err)
	}

	out, err := o.run(ctx, "stat", "-c", "%u:%g %a", o.ScratchRoot)
	if err != nil {
		if isExitError(err) {
			return st, nil
		}
		return st, fmt.Errorf("read scratch root owner %s: %w", o.ScratchRoot, err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		// The probe ran and reported nothing: ownership is UNOBSERVABLE
		// (an unprivileged stat, or a filesystem that carries no POSIX
		// owner). That is not a parse failure — the mode, read in-process,
		// remains the check that applies, and an unobservable owner is never
		// turned into a trusted one.
		return st, nil
	}
	cols := strings.Fields(trimmed)
	if len(cols) < 2 {
		return st, fmt.Errorf("unexpected stat output for scratch root %s: %q", o.ScratchRoot, string(out))
	}
	owner := strings.SplitN(cols[0], ":", 2)
	if len(owner) == 2 {
		st.Owner, st.Group = owner[0], owner[1]
	}
	octal := strings.TrimLeft(cols[1], "0")
	if octal == "" {
		octal = "0"
	}
	st.Octal = octal
	return st, nil
}

// verifyScratchRootOwnership STATS the scratch root back and proves it is
// owned by root and group-owned by the agent group. It is the ownership half
// of the boundary, kept separate from verifyScratchRoot (which owns the mode)
// because a filesystem that reports no owner cannot falsify the mode check but
// must not be reported as verified either: an unobservable owner is skipped,
// never trusted.
//
// An empty gid (an owner that could not be read at all) checks root ownership
// only: ownership that cannot be observed must not be invented.
func (o Options) verifyScratchRootOwnership(gid string) error {
	st, err := o.statScratchRoot(context.Background())
	if err != nil {
		return err
	}
	if st.Owner == "" {
		return nil // unobservable owner: the mode check is the one that applies
	}
	if st.Owner != "0" {
		return fmt.Errorf("scratch root %s is owned by uid %s, want 0 (root): an agent could not traverse or exchange through it", o.ScratchRoot, st.Owner)
	}
	if gid != "" && st.Group != "" && st.Group != gid {
		return fmt.Errorf("scratch root %s is group-owned by gid %s, want %s: agents outside that group cannot read the exchange tree", o.ScratchRoot, st.Group, gid)
	}
	return nil
}

// verifyScratchRoot STATS the scratch root back and proves the two properties
// the exchange point depends on: the setgid bit is set (new entries inherit the
// agent group) and neither the group nor the world can write it (an agent
// cannot create a plain, uncapped entry under the root). It reads the real
// directory, not the chmod argv, so a filesystem that dropped the bits or a
// wider pre-existing root is reported as a failure instead of a silent pass.
func verifyScratchRoot(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat scratch root %s after provisioning: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("scratch root %s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("scratch root %s is group/world writable (mode %04o, want %04o): an agent could create an uncapped entry next to its capped directory", dir, fi.Mode().Perm(), ScratchRootMode)
	}
	if fi.Mode()&os.ModeSetgid == 0 {
		return fmt.Errorf("scratch root %s has no setgid bit (mode %04o, want %04o): files created in the exchange tree would not inherit the agent group", dir, fi.Mode().Perm(), ScratchRootMode)
	}
	return nil
}

// EnsureAgentGroup creates the agent isolation group when it is missing.
// Exported because the fail-closed precondition requires it: a session whose
// membership cannot be proven is DENIED (never handed the shared /tmp), so a
// host without the group has no working agent sessions at all — both the
// installer and the spawn path call this.
func (o Options) EnsureAgentGroup(ctx context.Context) (*Report, error) {
	o = o.WithDefaults()
	rep := &Report{}
	if err := o.ensureAgentGroup(ctx, rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// ensureAgentGroup creates the agent group when it is missing.
func (o Options) ensureAgentGroup(ctx context.Context, rep *Report) error {
	if _, err := o.run(ctx, "getent", "group", o.AgentGroup); err == nil {
		rep.Add("ok", "group:"+o.AgentGroup, "agent group exists", true)
		return nil
	}
	if _, err := o.run(ctx, "groupadd", "--system", o.AgentGroup); err != nil {
		return fmt.Errorf("create agent group %s: %w", o.AgentGroup, err)
	}
	rep.Add("create", "group:"+o.AgentGroup, "system group created", true)
	return nil
}

// agentGroupID resolves the agent group's numeric gid.
func (o Options) agentGroupID(ctx context.Context) (string, error) {
	out, err := o.run(ctx, "getent", "group", o.AgentGroup)
	if err != nil {
		return "", fmt.Errorf("resolve agent group %s: %w", o.AgentGroup, err)
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(fields) < 3 {
		return "", fmt.Errorf("unexpected getent group output for %s: %q", o.AgentGroup, string(out))
	}
	return fields[2], nil
}

// EnsureAgentScratch provisions one agent's bounded scratch directory:
//
//  1. the agent's supplementary group membership is added (idempotent),
//  2. the SHARED ROOT is verified, and provisioned/repaired when this process
//     can do so, so the agent's directory lands under a root it can actually
//     traverse (see ensureSharedScratchRoot — DF-BUNKER-40),
//  3. the directory is created as the tmpfs mountpoint,
//  4. a size-capped tmpfs is mounted there,
//  5. ownership/mode are set to the agent with the setgid bunker group.
//
// Step 2 is a GATE, not best effort: a root this process cannot bring to
// root:<agent-group> 2750 fails the whole step, because provisioning a
// per-agent directory under an unreachable root produces a scratch the agent
// can neither list nor write — the exact defect where every spawn reported
// "shared scratch ready" and the exchange point was unusable.
//
// FAIL-CLOSED: if the bounded filesystem cannot be mounted, the mountpoint is
// removed and an error is returned, so a failed provision never leaves an
// unbounded, writable exchange directory behind. Callers treat that as "this
// agent has no shared scratch" rather than falling back to an unbounded one.
func (o Options) EnsureAgentScratch(ctx context.Context, agentID, username string, uid, gid int) (*Report, error) {
	o = o.WithDefaults()
	rep := &Report{}
	if !o.ScratchEnabled {
		rep.Add("skip", o.ScratchDir(agentID), "shared scratch disabled by configuration", false)
		return rep, nil
	}
	if agentID == "" || username == "" {
		return rep, fmt.Errorf("agent id and username are required")
	}

	dir := o.ScratchDir(agentID)

	// The group must exist and the agent must be a member before files in the
	// scratch tree can be exchanged: supplementary groups come from the
	// session, so a missing membership silently breaks cross-agent reads.
	if err := o.ensureAgentGroup(ctx, rep); err != nil {
		return rep, err
	}
	if err := o.ensureAgentGroupMembership(ctx, username, rep); err != nil {
		return rep, err
	}

	groupGID, err := o.agentGroupID(ctx)
	if err != nil {
		return rep, err
	}

	// DF-BUNKER-40: the exchange ROOT is this agent's parent directory, so it
	// must be reachable before the agent directory is created under it.
	if err := o.ensureSharedScratchRoot(ctx, rep); err != nil {
		return rep, err
	}

	mounted, err := o.isMountpoint(ctx, dir)
	if err != nil {
		return rep, err
	}
	if !mounted {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return rep, fmt.Errorf("create scratch mountpoint %s: %w", dir, err)
		}
		if _, err := o.run(ctx, "mount", ScratchMountArgs(dir, o.ScratchPerAgent)...); err != nil {
			// A mountpoint without the tmpfs would be an UNBOUNDED group-
			// writable directory: remove it before reporting the failure.
			removeErr := os.RemoveAll(dir)
			return rep, fmt.Errorf("mount bounded scratch for %s (mountpoint removed: %v): %w", agentID, removeErr, err)
		}
		rep.Add("mount", dir, fmt.Sprintf("%s %s", ScratchFSType, ScratchMountOptions(o.ScratchPerAgent)), true)
	} else {
		// Already mounted: re-assert the size bound (a config change must take
		// effect) and fall through to ownership/mode enforcement.
		opts, err := o.mountOptions(ctx, dir)
		if err != nil {
			return rep, err
		}
		want := fmt.Sprintf("size=%d", o.ScratchPerAgent)
		if !strings.Contains(opts, want) {
			if _, err := o.run(ctx, "mount", ScratchRemountArgs(dir, o.ScratchPerAgent)...); err != nil {
				return rep, fmt.Errorf("remount scratch %s at %s: %w", dir, want, err)
			}
			rep.Add("remount", dir, want, true)
		} else {
			rep.Add("ok", dir, "bounded scratch already mounted at "+want, true)
		}
	}

	if _, err := o.run(ctx, "chown", fmt.Sprintf("%d:%s", uid, groupGID), dir); err != nil {
		return rep, fmt.Errorf("chown scratch dir: %w", err)
	}
	if _, err := o.run(ctx, "chmod", fmt.Sprintf("%04o", ScratchDirMode), dir); err != nil {
		return rep, fmt.Errorf("chmod scratch dir: %w", err)
	}
	rep.Add("chmod", dir, fmt.Sprintf("owner %d:%s mode %04o (setgid exchange)", uid, groupGID, ScratchDirMode), true)
	return rep, nil
}

// EnsureAgentGroupMembership adds username to the agent isolation group when it
// is not already a member, creating the group first when it is missing.
//
// Exported because it is a HARD requirement of the spawn path and is
// deliberately independent of the shared-scratch toggle: membership is the PAM
// guard's input, so an agent without it would run with the host's shared /tmp
// even though the exchange directory is disabled.
func (o Options) EnsureAgentGroupMembership(ctx context.Context, username string) (*Report, error) {
	o = o.WithDefaults()
	rep := &Report{}
	if username == "" {
		return rep, fmt.Errorf("username is required")
	}
	if err := o.ensureAgentGroup(ctx, rep); err != nil {
		return rep, err
	}
	if err := o.ensureAgentGroupMembership(ctx, username, rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// ensureAgentGroupMembership adds username to the agent group when it is not
// already a member. Membership is what the sshd pam_exec precondition verifies
// before pam_namespace runs, and it is also what makes setgid files readable
// across agents.
func (o Options) ensureAgentGroupMembership(ctx context.Context, username string, rep *Report) error {
	out, err := o.run(ctx, "id", "-nG", username)
	if err == nil {
		for _, g := range strings.Fields(string(out)) {
			if g == o.AgentGroup {
				rep.Add("ok", "member:"+username, "already in "+o.AgentGroup, true)
				return nil
			}
		}
	}
	if _, err := o.run(ctx, "usermod", "-aG", o.AgentGroup, username); err != nil {
		return fmt.Errorf("add %s to %s: %w", username, o.AgentGroup, err)
	}
	rep.Add("update", "member:"+username, "added to "+o.AgentGroup, true)
	return nil
}

// RemoveAgentScratch unmounts and removes one agent's scratch directory. It is
// idempotent: an agent whose scratch was never provisioned (or is already
// gone) produces skips, not errors.
func (o Options) RemoveAgentScratch(ctx context.Context, agentID string) (*Report, error) {
	o = o.WithDefaults()
	rep := &Report{}
	dir := o.ScratchDir(agentID)

	mounted, err := o.isMountpoint(ctx, dir)
	if err != nil {
		return rep, err
	}
	if mounted {
		if _, err := o.run(ctx, "umount", "-l", dir); err != nil {
			return rep, fmt.Errorf("unmount scratch %s: %w", dir, err)
		}
		rep.Add("remove", dir, "scratch filesystem unmounted", true)
	}
	if _, err := os.Stat(dir); err == nil {
		if err := os.RemoveAll(dir); err != nil {
			return rep, fmt.Errorf("remove scratch dir %s: %w", dir, err)
		}
		rep.Add("remove", dir, "scratch directory removed", true)
	} else {
		rep.Add("skip", dir, "scratch directory absent", false)
	}
	return rep, nil
}

// ScratchStatus is the observed state of one agent's scratch directory, for
// `bunker host-provision --status` and the E2E battery.
type ScratchStatus struct {
	Dir     string
	Exists  bool
	Mounted bool
	Size    string // the mounted size= option, empty when not mounted
	Group   string
	Mode    os.FileMode
}

// AgentScratchStatus inspects one agent's scratch provision without changing
// anything.
func (o Options) AgentScratchStatus(ctx context.Context, agentID string) (ScratchStatus, error) {
	o = o.WithDefaults()
	st := ScratchStatus{Dir: o.ScratchDir(agentID)}
	fi, err := os.Stat(st.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	st.Exists = true
	st.Mode = fi.Mode().Perm() | (fi.Mode() & os.ModeSetgid)
	mounted, err := o.isMountpoint(ctx, st.Dir)
	if err != nil {
		return st, err
	}
	st.Mounted = mounted
	if mounted {
		opts, err := o.mountOptions(ctx, st.Dir)
		if err != nil {
			return st, err
		}
		for _, f := range strings.Split(opts, ",") {
			if strings.HasPrefix(f, "size=") {
				st.Size = f
			}
		}
	}
	return st, nil
}

// isMountpoint reports whether dir has a filesystem mounted on it. A non-zero
// exit from `mountpoint -q` is the "no" answer; any other failure (missing
// binary, permission) is a real error.
func (o Options) isMountpoint(ctx context.Context, dir string) (bool, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	_, err := o.run(ctx, "mountpoint", "-q", dir)
	if err == nil {
		return true, nil
	}
	if isExitError(err) {
		return false, nil
	}
	return false, err
}

// mountOptions returns the mount options of the filesystem at dir.
func (o Options) mountOptions(ctx context.Context, dir string) (string, error) {
	out, err := o.run(ctx, "findmnt", "-no", "OPTIONS", "--target", dir)
	if err != nil {
		return "", fmt.Errorf("read mount options for %s: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}
