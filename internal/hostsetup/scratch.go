package hostsetup

import (
	"context"
	"fmt"
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

	if err := os.MkdirAll(o.ScratchRoot, ScratchRootMode); err != nil {
		return rep, fmt.Errorf("create scratch root %s: %w", o.ScratchRoot, err)
	}
	rep.Add("ok", o.ScratchRoot, "scratch root exists", true)

	// chown root:<group> and enforce the mode every time: a directory that
	// already existed (operator-created, or left over from a failed run)
	// must not keep a wider mode.
	gid, err := o.agentGroupID(ctx)
	if err != nil {
		return rep, err
	}
	if _, err := o.run(ctx, "chown", fmt.Sprintf("0:%s", gid), o.ScratchRoot); err != nil {
		return rep, fmt.Errorf("chown scratch root: %w", err)
	}
	if _, err := o.run(ctx, "chmod", fmt.Sprintf("%04o", ScratchRootMode), o.ScratchRoot); err != nil {
		return rep, fmt.Errorf("chmod scratch root: %w", err)
	}
	// Re-assert the mode in-process as well. Go's FileMode cannot express the
	// octal 2750 spelling — the setgid flag is ModeSetgid, not 0o2000, so
	// os.Chmod(0o2750) would silently drop the bit — while the shell chmod
	// above is the auditable, operator-facing statement of it. Doing both keeps
	// the enforcement independent of the chmod binary AND makes the stat-back
	// below a real check in every runtime, including a fake command runner.
	if err := os.Chmod(o.ScratchRoot, os.ModeSetgid|ScratchRootMode.Perm()); err != nil {
		return rep, fmt.Errorf("enforce scratch root mode in-process: %w", err)
	}
	rep.Add("chmod", o.ScratchRoot, fmt.Sprintf("owner 0:%s mode %04o (setgid, no group/world write)", gid, ScratchRootMode), true)

	if err := verifyScratchRoot(o.ScratchRoot); err != nil {
		return rep, err
	}
	return rep, nil
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
//  2. the directory is created as the tmpfs mountpoint,
//  3. a size-capped tmpfs is mounted there,
//  4. ownership/mode are set to the agent with the setgid bunker group.
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
