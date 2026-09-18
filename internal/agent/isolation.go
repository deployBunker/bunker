package agent

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"path/filepath"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/hostsetup"
)

// lookupAgentUser resolves an agent's uid/gid. It is a variable so tests can
// drive the spawn-time isolation wiring without root privileges (the real
// lookup only succeeds after useradd has run).
var lookupAgentUser = user.Lookup

// Isolation boundary (GAP-075).
//
// Every agent gets an ENFORCED private /tmp and the only sanctioned
// cross-agent exchange point is the bounded shared scratch directory:
//
//   - transient systemd units (rootless dockerd, detached RunAgent) carry
//     --property=PrivateTmp=yes, so their /tmp is a private tmpfs of the unit;
//   - SSH sessions (/tmp for `bunker exec`, scp, sshfs, docker transport) get
//     a private per-session /tmp from pam_namespace. Bunker's sshd block keys
//     on the reserved agent NAME pattern and then requires the agent group
//     membership, the exact Bunker /tmp rule, the instance parent and its own
//     trust chain (root-owned, non-writable helper directory -> root-owned
//     manifest -> the helper's bytes) before the module runs; ordinary
//     operator sessions are jumped over the whole block and keep the host /tmp
//     (see internal/hostsetup);
//   - /srv/bunker-share is what the daemon creates per agent: its root is
//     root-owned, setgid and NOT writable by the agent group or the world
//     (mode 2750), so no agent can create an uncapped plain entry beside the
//     per-agent directories, and each per-agent directory is a size-capped
//     tmpfs with group/setgid semantics.
//
// Membership in the agent group is verified by that precondition, so it is
// provisioned for EVERY agent and is NOT gated by the shared-scratch toggle:
// disabling the optional exchange directory must not hand an agent the host's
// shared /tmp. A spawn that cannot grant the membership fails closed.
//
// The host-level half is idempotent and fail-closed too: a scratch directory
// whose bounded filesystem cannot be mounted is not created at all, so an agent
// never ends up with an unbounded exchange directory.

// dockerdUnitArgs are the inputs of the per-agent rootless-dockerd transient
// unit.
type dockerdUnitArgs struct {
	AgentID        string
	UnitName       string
	UID            string
	GID            string
	UserHome       string
	RuntimeDir     string
	DockerSockPath string
	RootlessBin    string
	CPUQuota       float64
	MemoryMax      uint64
	DiskMax        uint64
	MaxProcesses   uint64
	MaxOpenFiles   uint64
}

// buildRootlessDockerdArgs builds the exact `systemd-run` argv and the
// environment for the rootless-dockerd unit. It is a pure function so every
// unit property — including the GAP-075 PrivateTmp=yes that gives dockerd (and
// everything it starts) a private /tmp — is pinned by unit tests instead of by
// an integration test on a live host.
//
// DOCKERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns avoids needing a separate
// bridge, and the per-agent socket path is passed through DOCKER_HOST so the
// socket is created where bunkerd expects it.
//
// PID namespace isolation (--pidns) is intentionally omitted: rootlesskit
// v1.1.1 does not support --detach-netns, so mixing the two flags makes
// rootlesskit exit immediately. It will be revisited when the installed
// rootlesskit supports it.
//
// There is no systemd property for a container-count cap (docker's daemon and
// systemd neither expose nor enforce one per user); max_docker_containers is
// enforced at spawn time by counting the just-started dockerd's containers,
// and the limit is carried on the agent record for that policy check.
func buildRootlessDockerdArgs(a dockerdUnitArgs) (args []string, env []string) {
	// systemd-run --system with --uid does not inherit the caller's
	// environment, so every variable the rootless stack needs is passed with
	// --setenv.
	env = []string{
		"PATH=" + filepath.Join(a.UserHome, "bin") + ":/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + a.UserHome,
		"USER=" + "bunker-" + a.AgentID,
		"XDG_RUNTIME_DIR=" + a.RuntimeDir,
		"DOCKERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns",
		"DOCKERD_ROOTLESS_ROOTLESSKIT_PORT_DRIVER=builtin",
		// rootlesskit v1.1.1 does not support --detach-netns; the
		// dockerd-rootless.sh shipped with the installer defaults to "true"
		// and appends --detach-netns to ROOTLESSKIT_FLAGS, which makes
		// rootlesskit exit immediately. Force it off.
		"DOCKERD_ROOTLESS_ROOTLESSKIT_DETACH_NETNS=false",
		"DOCKER_HOST=unix://" + a.DockerSockPath,
		// GAP-075: TMPDIR points at /tmp, which PrivateTmp=yes makes private
		// to this unit. The legacy /run/bunker/<id>/tmp is still created but
		// is not advertised here — it is not an isolation boundary.
		"TMPDIR=" + config.IsolationTmpDir,
	}

	args = []string{
		"--system",
		"--unit=" + a.UnitName,
		"--uid=" + a.UID,
		"--gid=" + a.GID,
		"--property=PAMName=login",
		// GAP-075: systemd gives the unit a private mount namespace with its
		// own /tmp, so nothing dockerd starts can observe or collide with the
		// host's (root's) /tmp or with another agent's.
		"--property=PrivateTmp=yes",
	}
	if a.CPUQuota > 0 {
		// CPUQuota is a percentage of one CPU: 100%=1 core, 200%=2 cores.
		// This maps to cgroup v2 cpu.max as quota_us = CPUQuota%/100 * period_us.
		args = append(args, fmt.Sprintf("--property=CPUQuota=%d%%", int(a.CPUQuota*100)))
	}
	if a.MemoryMax > 0 {
		args = append(args, fmt.Sprintf("--property=MemoryMax=%d", a.MemoryMax))
	}
	if a.DiskMax > 0 {
		// LimitFSIZE caps the maximum file size (in bytes) an agent may create.
		// This is a pragmatic systemd-level enforcement for disk_max_bytes when
		// per-user filesystem quotas (xfs_quota) are not configured.
		args = append(args, fmt.Sprintf("--property=LimitFSIZE=%d", a.DiskMax))
	}
	if a.MaxProcesses > 0 {
		args = append(args, fmt.Sprintf("--property=TasksMax=%d", a.MaxProcesses))
	}
	if a.MaxOpenFiles > 0 {
		args = append(args, fmt.Sprintf("--property=LimitNOFILE=%d:%d", a.MaxOpenFiles, a.MaxOpenFiles))
	}
	for _, e := range env {
		args = append(args, "--setenv="+e)
	}
	args = append(args, a.RootlessBin, "--host=unix://"+a.DockerSockPath)
	return args, env
}

// hostSetup returns the host-provisioning options for this manager, with the
// configured agent group, scratch root/cap and the private-/tmp instance root.
func (m *AgentManager) hostSetup() hostsetup.Options {
	iso := m.cfg.Agent.Isolation
	iso.Defaults()
	o := hostsetup.DefaultOptions()
	o.ScratchEnabled = iso.SharedScratchEnabled
	o.ScratchRoot = iso.SharedScratchRoot
	o.AgentGroup = iso.AgentGroup
	o.ScratchPerAgent = iso.SharedScratchPerAgentBytes
	o.TmpInstanceRoot = iso.PrivateTmpRoot
	if m.hostRunner != nil {
		o.Runner = m.hostRunner
	}
	return o.WithDefaults()
}

// provisionIsolation provisions the agent-side half of the boundary.
//
// The agent group membership comes FIRST and is REQUIRED: the sshd pam_exec
// precondition verifies it before pam_namespace runs, so an agent that is not a
// member has its sessions DENIED — a failure here returns an error and the
// caller aborts the spawn (fail closed) instead of leaving an agent that cannot
// open a session at all.
//
// The remaining steps are best-effort with loud logging, deliberately: a
// scratch that cannot be bounded must NOT be replaced by an unbounded directory
// (that is the fail-closed rule implemented in internal/hostsetup), and a
// missing instance directory is created by pam_namespace on the first session
// (owned by root, mirroring /tmp), so neither failure is a reason to abort a
// spawn that is otherwise complete.
func (m *AgentManager) provisionIsolation(ctx context.Context, agentID, username string, uid, gid int) error {
	host := m.hostSetup()

	memRep, err := host.EnsureAgentGroupMembership(ctx, username)
	if err != nil {
		return fmt.Errorf("add %s to the agent isolation group %s: %w", username, host.AgentGroup, err)
	}
	m.logger.Info("agent isolation group membership ensured", "agent_id", agentID, "group", host.AgentGroup)
	m.logger.Debug("isolation membership report", "agent_id", agentID, "report", memRep.String())

	if m.cfg.Agent.Isolation.SharedScratchEnabled {
		rep, err := host.EnsureAgentScratch(ctx, agentID, username, uid, gid)
		if err != nil {
			m.logger.Warn("shared scratch not provisioned; agent keeps its private /tmp and has no cross-agent exchange directory",
				"agent_id", agentID, "error", err)
		} else {
			m.logger.Info("shared scratch ready", "agent_id", agentID)
			m.logger.Debug("shared scratch provisioning report", "agent_id", agentID, "report", rep.String())
		}
	} else {
		m.logger.Info("shared scratch disabled by configuration; private /tmp isolation is unaffected", "agent_id", agentID)
	}

	rep, err := host.EnsureAgentTmpInstance(ctx, agentID, username, uid, gid)
	if err != nil {
		m.logger.Warn("private /tmp instance directory not pre-created; pam_namespace will create it on first session",
			"agent_id", agentID, "error", err)
		return nil
	}
	m.logger.Info("private /tmp instance ready", "agent_id", agentID)
	m.logger.Debug("private /tmp instance provisioning report", "agent_id", agentID, "report", rep.String())
	return nil
}

// removeIsolation removes the agent's scratch and /tmp instance directories
// during destroy. Both removals are idempotent, so a partially provisioned
// (or never provisioned) agent destroys cleanly.
//
// DF-BUNKER-21: the outcome is RETURNED as well as logged. The spawn rollback
// records it in the failure breadcrumb, and a breadcrumb that claimed
// "isolation removed" while both removals had failed was one of the swallowed
// failures the QA foreman had to reconstruct from the host. Destroy keeps
// calling it best-effort (the error is logged, the destroy proceeds).
func (m *AgentManager) removeIsolation(ctx context.Context, agentID string) error {
	host := m.hostSetup()
	var errs []error
	if rep, err := host.RemoveAgentScratch(ctx, agentID); err != nil {
		m.logger.Warn("shared scratch removal incomplete", "agent_id", agentID, "error", err)
		errs = append(errs, fmt.Errorf("shared scratch: %w", err))
	} else {
		m.logger.Debug("shared scratch removed", "agent_id", agentID, "report", rep.String())
	}
	if rep, err := host.RemoveAgentTmpInstance(ctx, agentID); err != nil {
		m.logger.Warn("private /tmp instance removal incomplete", "agent_id", agentID, "error", err)
		errs = append(errs, fmt.Errorf("private /tmp instance: %w", err))
	} else {
		m.logger.Debug("private /tmp instance removed", "agent_id", agentID, "report", rep.String())
	}
	return errors.Join(errs...)
}
