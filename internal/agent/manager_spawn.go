// Package agent manages agent lifecycle: create users, generate SSH keys, start dockerd.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/imagespec"
	"github.com/deployBunker/bunker/internal/resource"
)

// agentHomeRoot is the root under which agent home directories live. "/home" in
// production; the spawn-failure regressions point it at a temp directory so the
// whole spawn path — authorized_keys, .profile, the rootless install directory,
// the port metadata — can be driven end to end without writing into the real
// /home of the machine running the tests. Declared as a var (not a const) purely
// as that test seam; production never writes it.
var agentHomeRoot = "/home"

// spawnRunRoot is the root of the per-agent runtime state the spawn creates:
// <root>/<agent_id>/docker.sock, <root>/<agent_id>/tmp and
// <root>/<agent_id>/run. "/run/bunker" in production; the same test seam as
// agentHomeRoot, so a regression can drive the spawn into the rootless stage
// without writing into the host's /run (which needs root, and would leave a
// directory behind on a machine that runs the tests as root).
var spawnRunRoot = filepath.Join("/run", "bunker")

func (m *AgentManager) Spawn(ctx context.Context, req *v1.SpawnAgentRequest) (*v1.SpawnAgentResponse, error) {
	// ── Step 1: Validate or generate agent_id ──────────────────────
	agentID := req.GetAgentId()
	if agentID != "" {
		if !validAgentID.MatchString(agentID) {
			return nil, spawnStageErr(ctx, agentID, StageValidate, fmt.Errorf("invalid agent_id %q: must match [a-z0-9-]{1,63}", agentID))
		}
	} else {
		uuid, err := generateUUIDv4()
		if err != nil {
			return nil, spawnStageErr(ctx, agentID, StageValidate, fmt.Errorf("generate agent_id uuid: %w", err))
		}
		// Use first segment of UUID as short ID.
		agentID = strings.SplitN(uuid, "-", 2)[0]
	}

	// ── Step 1a: Validate TTL ─────────────────────────────────────
	// Effective TTL: request > server default. Invalid TTLs are rejected
	// here, before any side effects (user creation, dockerd start), per
	// specs/api.md: TTL format \d+[hmd] (e.g. "6h", "24h", "7d").
	ttl := m.cfg.Agent.DefaultTTL
	if ttl <= 0 {
		ttl = 6 * time.Hour
	}
	if req.GetTtl() != "" {
		parsed, err := ParseAgentTTL(req.GetTtl())
		if err != nil {
			return nil, spawnStageErr(ctx, agentID, StageValidate, fmt.Errorf("invalid ttl %q: %w", req.GetTtl(), err))
		}
		ttl = parsed
	}

	// ── Step 1b: Resolve the safety preset BEFORE any side effect ──
	// GAP-116 precedence: per-spawn flag (req.SafetyPreset) > the
	// BUNKERD_SAFETY_PRESET env > the config global > the built-in default.
	// An unknown name from any source is a hard error (mapped to
	// CodeInvalidArgument by the server) — never a silent fallback. The
	// resolved name is stamped on the agent record so `bunker info` can
	// report the effective preset.
	preset, err := m.cfg.ResolveSafetyPreset(req.GetSafetyPreset())
	if err != nil {
		return nil, spawnStageErr(ctx, agentID, StageValidate, err)
	}
	m.logger.Info("resolved safety preset", "agent_id", agentID, "preset", preset)

	// ── Step 1c: Resolve the mount driver BEFORE any side effect ────
	// MOUNT-006: the requested driver (req.MountDriver, empty = the
	// sshfs default) must be registered on this server. An unknown name
	// REFUSES here — before user creation, port allocation, or dockerd
	// start — with a named error; it never silently falls back to sshfs.
	mountDriver, mountDriverErr := resolveMountDriver(req.GetMountDriver())
	if mountDriverErr != nil {
		return nil, spawnStageErr(ctx, agentID, StageValidate, mountDriverErr)
	}
	m.logger.Info("resolved mount driver", "agent_id", agentID, "driver", mountDriver.Name)

	// ── Step 1.7: Validate the image spec BEFORE any side effect ──
	// GAP-064: an invalid or disallowed image spec must fail with a
	// validation error (mapped to CodeInvalidArgument by the server) without
	// creating the user, allocating ports, starting dockerd, or building an
	// image. The parsed spec is carried through the rest of spawn; the nil
	// spec is the common no-customization case. When the feature is disabled
	// server-side, a supplied spec is likewise rejected up front.
	var imageSpec *imagespec.Spec
	if req.GetImageSpec() != nil {
		if m.imageBuilder == nil {
			return nil, spawnStageErr(ctx, agentID, StageValidate, fmt.Errorf("image spec support is not available on this server"))
		}
		spec, err := imagespec.FromProto(req.GetImageSpec())
		if err != nil {
			return nil, spawnStageErr(ctx, agentID, StageValidate, fmt.Errorf("invalid image spec: %w", err))
		}
		imageSpec = spec
	}

	// ── Step 1.5: Check capacity BEFORE allocating a port range ──
	// (allocating first leaks the range when capacity is full — the
	// allocator is in-memory and only freed on destroy)
	if !m.tracker.HasCapacity(1) {
		return nil, spawnStageErr(ctx, agentID, StageCapacity, fmt.Errorf("capacity full: %d/%d agents", m.tracker.Count(), m.tracker.MaxAgents()))
	}

	// ── Step 1.6: Allocate port range ────────────────────────────
	var portStart, portEnd uint32
	if m.portAlloc != nil {
		var allocErr error
		portStart, portEnd, allocErr = m.portAlloc.Allocate(agentID)
		if allocErr != nil {
			return nil, spawnStageErr(ctx, agentID, StagePortAlloc, fmt.Errorf("port range allocation: %w", allocErr))
		}
	} else {
		// Port allocator disabled — use full configured range as fallback.
		portStart = m.cfg.Agent.PortRangeStart
		portEnd = m.cfg.Agent.PortRangeEnd
	}
	m.logger.Info("allocated port range", "agent_id", agentID, "range", fmt.Sprintf("%d-%d", portStart, portEnd))

	m.logger.Info("spawning agent", "agent_id", agentID)

	// Track what we've created for the rollback on failure.
	var createdUser bool
	var createdUserSlice bool
	var keyFile string
	portRangeAllocated := m.portAlloc != nil

	// INT-CI-005 / DF-BUNKER-21: the rollback must survive request-context
	// cancellation AND must not let one slow compensating step starve the ones
	// after it. The server (internal/server/server.go) wraps the handler in chi
	// middleware.Timeout(cfg.Server.RequestTimeout, 300s by default); when a
	// spawn exceeds it, the request ctx is cancelled and every
	// exec.CommandContext(ctx, ...) in this closure used to die instantly —
	// userdel never ran and the half-created agent (user without key/registry
	// row) was left behind. Detaching the context (context.WithoutCancel) fixed
	// that but still gave every step ONE shared budget, so a step that consumed
	// it left the next ones holding a dead context and silently doing nothing
	// (QA-BUNKER-19: spawn cancelled during the rootless download, host left
	// with 11 orphan bunker-* users and 0 registered agents). Every compensating
	// action now draws its OWN fresh, detached, bounded context from the
	// rollback budget, and every outcome is recorded so the failure breadcrumb
	// can show a partially-rolled-back agent.
	rbRes := &rollbackResult{}
	rb := newRollbackBudget(ctx)
	rollback := func() {
		// Free the port range first — the in-memory allocator leaks
		// permanently if a failed spawn never releases it. No command, no
		// budget: this cannot be starved.
		if portRangeAllocated {
			m.portAlloc.Free(agentID)
			rbRes.ok("port-range freed")
		}
		// Release the tracker slot defensively. The durable-persist gate below
		// unregisters before it calls this closure, but a rolled-back spawn
		// must never leave the in-memory registry holding an agent that does
		// not exist: capacity math and the reconcile sweep both read it.
		if m.tracker.Get(agentID) != nil {
			m.tracker.Unregister(agentID)
			rbRes.ok("tracker slot released")
		}
		if createdUserSlice {
			rb.runStep("slice-limits "+agentID, rbRes, func(ctx context.Context) {
				if sliceErr := removeUserSliceLimits(ctx, agentID, m.logger); sliceErr != nil {
					rbRes.err("slice-limits: " + sliceErr.Error())
					return
				}
				rbRes.ok("slice-limits removed")
			})
		}
		if createdUser {
			removeAgentUser(rb, agentID, m.logger, rbRes)
		}
		// Key files and the persisted SSH key are plain filesystem removals:
		// they need no context, so no budget can starve them.
		if keyFile != "" {
			os.Remove(keyFile)
			os.Remove(keyFile + ".pub")
		}
		// Remove persisted SSH key from config dir
		sshKeyPath := filepath.Join(m.cfg.Agent.SSHDir, agentID)
		os.Remove(sshKeyPath)
		// GAP-075: drop the scratch and private-/tmp instance directories so a
		// failed spawn cannot leave a provisioned exchange point or tmp
		// instance behind for an agent that does not exist.
		rb.runStep("isolation "+agentID, rbRes, func(ctx context.Context) {
			if isoErr := m.removeIsolation(ctx, agentID); isoErr != nil {
				// DF-BUNKER-21: reported, not swallowed. The rollback used to
				// claim "isolation removed" unconditionally, so a breadcrumb
				// could assert a teardown that had failed.
				rbRes.err("isolation: " + isoErr.Error())
				return
			}
			rbRes.ok("isolation removed")
		})
	}

	// INT-CI-005: every failure return from Spawn flows through this helper so
	// the error names the stage, the journal carries an operator-readable
	// breadcrumb, and slow-stage progress is attributable. `rollbackDone`
	// distinguishes the one failure path that already rolled back itself
	// (the durable-persist gate, which must also Unregister the tracker slot)
	// from every other failure, which is rolled back exactly here.
	var rollbackDone bool
	fail := func(stage string, cause error) error {
		if !rollbackDone {
			rollback()
		}
		err := spawnStageErr(ctx, agentID, stage, cause)
		m.logger.Error("spawn failed",
			"agent_id", agentID,
			"stage", stage,
			"error", err,
		)
		ran, failedRb := rbRes.snapshot()
		writeSpawnFailureBreadcrumb(m.logger, spawnBreadcrumb{
			AgentID: agentID,
			Stage:   stage,
			CtxErr:  ctxErrText(ctx),
			Error:   err.Error(),
			Ran:     ran,
			Failed:  failedRb,
			Notices: rbRes.noticeSnapshot(),
		})
		return err
	}

	// ── Step 2: Create Linux user ──────────────────────────────────
	username := "bunker-" + agentID
	m.logger.Info("creating user", "username", username)
	cmd := exec.CommandContext(ctx, "useradd", "-m", "-s", "/bin/bash", username)
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "already exists") {
			// Idempotent re-registration: the agent user (home, rootless
			// dockerd data, running containers) survived a bunkerd restart /
			// registry wipe. Reuse it instead of failing — createdUser stays
			// false so failure cleanup never userdels an existing user.
			// The keypair + authorized_keys below are refreshed, so the
			// spawn response carries a working key for the same agent id.
			m.logger.Info("user already exists; reusing for re-registration", "username", username)
		} else {
			return nil, fail(StageUserCreate, fmt.Errorf("useradd %s failed: %w (output: %s)", username, err, string(out)))
		}
	} else {
		createdUser = true
	}

	// ── Step 2.5: Provision the GAP-075 isolation boundary ─────────
	// The agent's membership in the isolation group must be in place BEFORE
	// its first login: supplementary groups come from the session and the sshd
	// pam_exec precondition verifies that membership, so an agent without it
	// has every session DENIED (it never falls back to the host's shared
	// /tmp). That step is therefore fatal — the spawn is rolled back rather
	// than leaving an agent that cannot open a session — and the private-/tmp
	// instance directory is created with explicit ownership so the boundary
	// exists before any session opens.
	u, err := lookupAgentUser(username)
	if err != nil {
		return nil, fail(StageIsolationProvision, fmt.Errorf("look up agent user %s for isolation provisioning: %w", username, err))
	}
	uid, atoiErr := strconv.Atoi(u.Uid)
	if atoiErr != nil {
		// DF-BUNKER-63: the collision precheck cannot verify a uid it cannot
		// parse, and scanning uid 0 would be nonsense — fail closed at the
		// collision stage rather than handing out an unverifiable identity.
		return nil, fail(StageUIDCollision, fmt.Errorf("uid of agent user %s is not numeric (%q): the uid-collision precheck cannot verify it (fail closed)", username, u.Uid))
	}
	gid, _ := strconv.Atoi(u.Gid)

	// ── Step 2.6 (DF-BUNKER-63): verify the new uid owns NO live process ──
	// The agent itself has no processes yet, so ANY hit is foreign — the
	// fad4b89a shape: the uid an unrelated production container still runs
	// as. Handing the uid out anyway would grant the agent same-uid signal
	// privilege over that process, breaking the per-user isolation promise.
	// Fail closed through the standard rollback: the just-created user is
	// removed, the spawn fails with a named stage error listing the
	// colliding pids, and the JSONL breadcrumb records it. The probe is
	// seam-isolated (spawnProcessScanner) so tests drive both branches
	// without root.
	m.logger.Info("spawn entering stage", "agent_id", agentID, "stage", StageUIDCollision)
	if err := m.checkSpawnUIDCollision(ctx, username, uint32(uid)); err != nil {
		return nil, fail(StageUIDCollision, err)
	}

	if err := m.provisionIsolation(ctx, agentID, username, uid, gid); err != nil {
		return nil, fail(StageIsolationProvision, fmt.Errorf("provision isolation boundary for %s: %w", agentID, err))
	}

	// ── Step 3: Generate SSH keypair ───────────────────────────────
	keyFile = filepath.Join(os.TempDir(), fmt.Sprintf("bunker-key-%s", agentID))
	m.logger.Info("generating SSH keypair", "keyfile", keyFile)
	cmd = exec.CommandContext(ctx, "ssh-keygen",
		"-t", "ed25519",
		"-f", keyFile,
		"-N", "",
		"-C", "bunker-"+agentID,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fail(StageKeygen, fmt.Errorf("ssh-keygen failed: %w (output: %s)", err, string(out)))
	}
	pubKeyFile := keyFile + ".pub"

	// Read keys into memory.
	privKeyBytes, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fail(StageKeygen, fmt.Errorf("read private key %s: %w", keyFile, err))
	}
	pubKeyBytes, err := os.ReadFile(pubKeyFile)
	if err != nil {
		return nil, fail(StageKeygen, fmt.Errorf("read public key %s: %w", pubKeyFile, err))
	}

	// ── Step 4: Set up .ssh/authorized_keys with DOCKER_HOST env ──
	userHome := filepath.Join(agentHomeRoot, username)
	sshDir := filepath.Join(userHome, ".ssh")
	authKeysFile := filepath.Join(sshDir, "authorized_keys")
	dockerSockPath := filepath.Join(spawnRunRoot, agentID, "docker.sock")
	tmpDir := filepath.Join(spawnRunRoot, agentID, "tmp")

	m.logger.Info("setting up authorized_keys", "user", username)
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return nil, fail(StageAuthorizedKeys, fmt.Errorf("create .ssh dir %s: %w", sshDir, err))
	}
	// Chown the .ssh directory to the user.
	if out, err := exec.CommandContext(ctx, "chown", "-R", username, sshDir).CombinedOutput(); err != nil {
		return nil, fail(StageAuthorizedKeys, fmt.Errorf("chown .ssh dir: %w (output: %s)", err, string(out)))
	}

	// Prepend environment= to the public key line so Docker's SSH transport
	// finds the right socket (requires PermitUserEnvironment=yes in sshd_config).
	// The pubKeyBytes end with a newline from ssh-keygen; strip and re-append.
	//
	// GAP-075: TMPDIR points at /tmp, which an agent's SSH session sees as its
	// OWN private instance (pam_namespace binds the session's /tmp instance
	// there). tmpDir — /run/bunker/<id>/tmp — is still created as a legacy
	// per-agent scratch path but is no longer advertised as TMPDIR, because a
	// shared-namespace directory is not an isolation boundary.
	pubKeyLine := strings.TrimSpace(string(pubKeyBytes))
	envPrefix := fmt.Sprintf(`environment="DOCKER_HOST=unix://%s TMPDIR=%s"`, dockerSockPath, config.IsolationTmpDir)
	authKeysContent := envPrefix + " " + pubKeyLine + "\n"

	if err := os.WriteFile(authKeysFile, []byte(authKeysContent), 0600); err != nil {
		return nil, fail(StageAuthorizedKeys, fmt.Errorf("write authorized_keys: %w", err))
	}
	if out, err := exec.CommandContext(ctx, "chown", username, authKeysFile).CombinedOutput(); err != nil {
		return nil, fail(StageAuthorizedKeys, fmt.Errorf("chown authorized_keys: %w (output: %s)", err, string(out)))
	}

	// ── Step 4a: Provision host SSH key so bunkerd can SSH into the agent ──
	if err := provisionHostSSHKey(ctx, username, authKeysFile, m.logger); err != nil {
		m.logger.Warn("failed to provision host SSH key; agent operations requiring host-to-agent SSH may fail",
			"agent_id", agentID,
			"error", err,
		)
	}

	// ── Step 4b: Set up .profile with DOCKER_HOST + TMPDIR (interactive sessions) ──
	profilePath := filepath.Join(userHome, ".profile")
	profileContent := fmt.Sprintf("# bunker: per-agent Docker socket and enforced private /tmp\nexport DOCKER_HOST=unix://%s\nexport TMPDIR=%s\n", dockerSockPath, config.IsolationTmpDir)
	if err := os.WriteFile(profilePath, []byte(profileContent), 0644); err != nil {
		return nil, fail(StageAuthorizedKeys, fmt.Errorf("write .profile: %w", err))
	}
	if out, err := exec.CommandContext(ctx, "chown", username, profilePath).CombinedOutput(); err != nil {
		return nil, fail(StageAuthorizedKeys, fmt.Errorf("chown .profile: %w (output: %s)", err, string(out)))
	}

	// ── Step 4c: Persist private key to the server's SSH directory ──
	sshKeyPath := filepath.Join(m.cfg.Agent.SSHDir, agentID)
	if err := os.MkdirAll(m.cfg.Agent.SSHDir, 0700); err != nil {
		return nil, fail(StageAuthorizedKeys, fmt.Errorf("create ssh dir %s: %w", m.cfg.Agent.SSHDir, err))
	}
	if err := os.WriteFile(sshKeyPath, privKeyBytes, 0600); err != nil {
		return nil, fail(StageAuthorizedKeys, fmt.Errorf("write ssh private key to %s: %w", sshKeyPath, err))
	}
	m.logger.Info("persisted SSH private key", "path", sshKeyPath)

	// ── Step 5: Start rootless dockerd via systemd-run ─────────────
	// Agents are unprivileged Linux users. A privileged dockerd cannot run as
	// a non-root user, so we use Docker's rootless mode. The setup installs
	// rootlesskit, slirp4netns/vpnkit, and configures subuid/subgid for the
	// user, then starts dockerd-rootless.sh as a systemd user unit.
	dockerSockPath = filepath.Join(spawnRunRoot, agentID, "docker.sock")
	unitName := "bunker-docker-" + agentID
	m.logger.Info("starting rootless dockerd", "unit", unitName, "sock", dockerSockPath)

	m.logger.Info("spawn entering stage", "agent_id", agentID, "stage", StageRootlessInstall)
	// Create the socket directory.
	sockDir := filepath.Dir(dockerSockPath)
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		return nil, fail(StageRootlessInstall, fmt.Errorf("create docker sock dir %s: %w", sockDir, err))
	}
	// Chown the socket directory to the agent user so dockerd can create the socket
	// and the SSH transport can access it.
	if out, err := exec.CommandContext(ctx, "chown", username, sockDir).CombinedOutput(); err != nil {
		return nil, fail(StageRootlessInstall, fmt.Errorf("chown socket dir: %w (output: %s)", err, string(out)))
	}

	// Legacy per-agent scratch under /run/bunker/<id>/tmp (mode 0700) for
	// tools that still reference the path directly. GAP-075: this is NOT the
	// isolation boundary any more — the boundary is the enforced private /tmp
	// (pam_namespace per SSH session, PrivateTmp=yes per transient unit), and
	// TMPDIR no longer points here.
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		return nil, fail(StageRootlessInstall, fmt.Errorf("create tmp dir %s: %w", tmpDir, err))
	}
	if out, err := exec.CommandContext(ctx, "chown", username, tmpDir).CombinedOutput(); err != nil {
		return nil, fail(StageRootlessInstall, fmt.Errorf("chown tmp dir: %w (output: %s)", err, string(out)))
	}

	// Determine resource limits: use request limits or server defaults
	cpuQuota := m.cfg.Agent.DefaultCPUQuota
	memMax := m.cfg.Agent.DefaultMemoryBytes
	diskMax := m.cfg.Agent.DefaultDiskBytes
	maxProcs := m.cfg.Agent.DefaultMaxProcesses
	maxFiles := m.cfg.Agent.DefaultMaxOpenFiles
	maxDockerContainers := m.cfg.Agent.DefaultMaxDockerContainers
	if req.GetLimits() != nil {
		if req.GetLimits().GetCpuQuota() > 0 {
			cpuQuota = req.GetLimits().GetCpuQuota()
		}
		if req.GetLimits().GetMemoryMaxBytes() > 0 {
			memMax = req.GetLimits().GetMemoryMaxBytes()
		}
		if req.GetLimits().GetDiskMaxBytes() > 0 {
			diskMax = req.GetLimits().GetDiskMaxBytes()
		}
		if req.GetLimits().GetMaxDockerContainers() > 0 {
			maxDockerContainers = req.GetLimits().GetMaxDockerContainers()
		}
	}

	// Configure subuid/subgid so rootless Docker can map container root to the
	// agent user. Each agent gets a contiguous 65,536-ID range allocated from a
	// global pool under a host-wide lock, guaranteed disjoint from every other
	// name's range (GAP-140: the previous start=<own uid> scheme overlapped for
	// every pair of agents).
	if err := configureSubIDs(ctx, username); err != nil {
		return nil, fail(StageRootlessInstall, fmt.Errorf("configure subuid/subgid for %s: %w", username, err))
	}

	// Ensure an AppArmor profile exists for rootlesskit on Ubuntu 24.04+
	// where unprivileged user namespaces are restricted by AppArmor. This must
	// happen BEFORE running the rootless docker installer, otherwise the
	// installer fails with permission denied.
	if err := ensureRootlesskitAppArmor(ctx, username, m.logger); err != nil {
		m.logger.Warn("could not ensure rootlesskit AppArmor profile; rootless docker may fail",
			"agent_id", agentID, "error", err)
	}

	// Install the rootless Docker tooling into the agent's home directory.
	// This downloads the official docker-ce-rootless-extras installer when the
	// tools are not already present on the server.
	rootlessBin := filepath.Join(userHome, "bin", "dockerd-rootless.sh")
	if err := installRootlessDocker(ctx, username, userHome, m.logger); err != nil {
		return nil, fail(StageRootlessInstall, fmt.Errorf("install rootless docker for %s: %w", username, err))
	}

	// Ensure an AppArmor profile exists for rootlesskit on Ubuntu 24.04+
	// where unprivileged user namespaces are restricted by AppArmor.
	// (Already ensured above before installRootlessDocker; kept here as a
	// defensive re-check in case the first call failed transiently.)
	if err := ensureRootlesskitAppArmor(ctx, username, m.logger); err != nil {
		m.logger.Warn("could not ensure rootlesskit AppArmor profile; rootless docker may fail",
			"agent_id", agentID, "error", err)
	}

	// Build systemd-run args with cgroup resource limits.
	// Use --system (system-wide unit) with --uid/--gid so the unit runs as the
	// agent user without requiring a running systemd --user manager / D-Bus bus
	// for a freshly-created user. This also lets us apply CPUQuota/MemoryMax.
	//
	// Because the unit is a *user* unit created by systemd-run as the agent
	// user, the cgroup v2 controller files will live under:
	//   /sys/fs/cgroup/user.slice/user-<uid>.slice/user@<uid>.service/bunker-docker-<agentID>.service
	// Limits are enforced by systemd through CPUQuota/MemoryMax, not by writing
	// cgroup files directly. Read-back helpers in internal/resource use this
	// path for metrics verification.
	m.logger.Info("spawn entering stage", "agent_id", agentID, "stage", StageDockerdStart)
	u, err = user.Lookup(username)
	if err != nil {
		return nil, fail(StageDockerdStart, fmt.Errorf("lookup user %s: %w", username, err))
	}

	// Rootless dockerd started via systemd-run --system --uid creates its Unix
	// socket under the systemd user runtime directory (/run/user/<uid>), not at
	// the custom XDG_RUNTIME_DIR we set. We keep the logical Bunker socket path
	// (/run/bunker/<id>/docker.sock) for authorized_keys, .profile, and the
	// docker tunnel command, and reconcile the two paths with a symlink once
	// dockerd is running.
	actualDockerSockPath := fmt.Sprintf("/run/user/%s/docker.sock", u.Uid)

	// rootlesskit checks that XDG_RUNTIME_DIR is set and writable. On systems
	// where systemd has not created /run/user/<uid>, point it at a per-agent
	// runtime directory under /run/bunker so dockerd-rootless.sh can start.
	// rootlesskit v1.1.1 also uses --copy-up=/etc and --copy-up=/run by
	// default, which requires a writable XDG_RUNTIME_DIR, and the socket
	// directory (/run/bunker/<id>) is chowned to the agent so dockerd can
	// create the socket there.
	rootlessRuntimeDir := filepath.Join(spawnRunRoot, agentID, "run")
	if err := os.MkdirAll(rootlessRuntimeDir, 0700); err != nil {
		return nil, fail(StageDockerdStart, fmt.Errorf("create rootless runtime dir %s: %w", rootlessRuntimeDir, err))
	}
	if out, err := exec.CommandContext(ctx, "chown", username, rootlessRuntimeDir).CombinedOutput(); err != nil {
		return nil, fail(StageDockerdStart, fmt.Errorf("chown rootless runtime dir: %w (output: %s)", err, string(out)))
	}

	// The unit argv (including the GAP-075 PrivateTmp=yes property) is built by
	// a pure function so it is pinned by unit tests rather than by a live host.
	// GAP-116: the limit property block is table-driven from the resolved
	// preset's knob set (identical values/order for every preset in this row).
	// GAP-118: the DoS-containment set resolves from the same preset through
	// the tier table (matrix verdicts) merged with the daemon's admin
	// overrides, and rides the slice drop-in (below) and the R2 landing check.
	unitKnobs, sliceKnobs := KnobsForPreset(preset, cpuQuota, memMax, diskMax, maxProcs, maxFiles)
	containment := resolveContainmentKnobs(preset, memMax, m.cfg.Agent)
	// INT-CI-37: the containment render is the FIRST thing that can fail for
	// knob reasons, and it must fail LOUD before systemd-run creates any
	// unit state: an explicit IOWriteBandwidthMax request whose whole-disk
	// device cannot be resolved is a StageSliceLimits failure naming the
	// knob — never a silently-omitted property (the R2 no-silent-no-op rule).
	containKnobs, containErr := sliceContainmentKnobs(containment)
	if containErr != nil {
		return nil, fail(StageSliceLimits, containErr)
	}
	sliceKnobs = append(sliceKnobs, containKnobs...)
	systemdArgs, rootlessEnv := buildRootlessDockerdArgs(dockerdUnitArgs{
		AgentID:        agentID,
		UnitName:       unitName,
		UID:            u.Uid,
		GID:            u.Gid,
		UserHome:       userHome,
		RuntimeDir:     rootlessRuntimeDir,
		DockerSockPath: dockerSockPath,
		RootlessBin:    rootlessBin,
		CPUQuota:       cpuQuota,
		MemoryMax:      memMax,
		DiskMax:        diskMax,
		MaxProcesses:   maxProcs,
		MaxOpenFiles:   maxFiles,
		UnitKnobs:      unitKnobs,
	})

	// Clear any leftover state from a previous unit with this name BEFORE
	// systemd-run creates it (see resetStaleDockerdUnit).
	resetStaleDockerdUnit(ctx, unitName, username, m.logger)

	cmd = exec.CommandContext(ctx, "systemd-run", systemdArgs...)
	cmd.Env = append(os.Environ(), rootlessEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fail(StageDockerdStart, fmt.Errorf("systemd-run rootless dockerd failed: %w (output: %s)", err, string(out)))
	}

	// ── Step 5b: Verify dockerd actually started ─────────────────────
	// systemd-run can return success while the unit fails immediately (missing
	// AppArmor profile, bad subuid mapping, etc.). Wait briefly, then check for
	// a dockerd process and the socket. If not present, capture unit status and
	// fail the spawn so the caller knows what went wrong.
	if err := waitForDockerd(ctx, username, dockerSockPath, actualDockerSockPath, unitName, m.logger); err != nil {
		m.logger.Error("dockerd failed to start; cleaning up agent", "agent_id", agentID, "error", err)
		return nil, fail(StageDockerdStart, err)
	}

	// Enforce maxDockerContainers by asking the *just-started* rootless dockerd
	// how many containers it already hosts. If it exceeds the configured limit,
	// fail the spawn before the tracker is populated and run cleanup.
	//
	// This is the ONLY enforcement point for max_docker_containers: systemd has
	// no per-user container property, and docker's daemon exposes no such limit,
	// so the cap is applied by the daemon at spawn time (the unit argv carries
	// no container-count property — see buildRootlessDockerdArgs).
	if maxDockerContainers > 0 {
		containers, err := countAgentContainers(ctx, dockerSockPath)
		if err != nil {
			return nil, fail(StageContainerCap, fmt.Errorf("count existing containers for %s: %w", username, err))
		}
		if containers >= maxDockerContainers {
			return nil, fail(StageContainerCap, fmt.Errorf("max docker containers exceeded for agent %s: %d >= %d", agentID, containers, maxDockerContainers))
		}
	}

	// ── Step 5b.5: Build the customized image (GAP-064) ────────────
	// When the spawn carries an image spec, build (or reuse from cache) the
	// customized image through this agent's rootless socket. Validation
	// already happened in Step 1.7 (before any side effect), so reaching
	// this point means the spec is valid.
	imageRef := ""
	if imageSpec != nil {
		m.logger.Info("spawn entering stage", "agent_id", agentID, "stage", StageImageBuild)
		built, err := m.imageBuilder.BuildValidated(ctx, agentID, imageSpec)
		if err != nil {
			return nil, fail(StageImageBuild, fmt.Errorf("image spec build for %s: %w", agentID, err))
		}
		imageRef = built
		m.logger.Info("customized image ready", "agent_id", agentID, "image", imageRef)
	}

	// ── Step 5c: Apply cgroup limits to the user slice ─────────────
	// The systemd unit above (bunker-docker-<id>) only constrains the dockerd
	// process.  Non-docker commands (bunker exec <id> -- stress, dd, etc.) run
	// directly as the agent user with no cgroup limits.  To close this gap we
	// create a drop-in snippet for user.slice/user-<uid>.slice that applies the
	// same CPU, memory, disk, process-count, and file-descriptor limits to
	// *every* process owned by the agent user — containers and direct commands
	// alike.
	//
	// GAP-117: the drop-in properties resolve from the SAME tier knob table
	// (KnobsForPreset) as the unit argv — both enforcement points route through
	// one tier resolution, so a tier that narrows a knob can never narrow only
	// one surface.
	//
	// INT-CI-37: the apply step now carries the R2 containment landing check
	// with it — the slice drop-in write AND its daemon-reload MUST land before
	// verifyContainmentLanding reads the live cgroup. The pre-fix order
	// (check right after systemd-run, drop-in written minutes later) read
	// memory.swap.max = "max" off the untouched slice and failed EVERY root
	// spawn at StageSliceLimits (CI run 35835000060). The verify half re-reads
	// the cgroup a bounded number of times, so a systemd that consumes the
	// drop-in slightly after daemon-reload returns still converges — without
	// ever weakening the check: exhaustion reports requested-versus-observed
	// and fails the spawn.
	createdUserSlice = false
	dropinContent, sliceErr := m.applyUserSliceLimitsAndVerify(ctx, u, cpuQuota, memMax, diskMax, maxProcs, maxFiles, sliceKnobs, containment)
	if sliceErr != nil {
		// The drop-in write itself failing stays the documented best-effort
		// degradation (agent user constrained by the dockerd unit only). But
		// a drop-in that WROTE and then failed its containment landing check
		// is a hard spawn failure at slice-limits — an unenforced containment
		// default must never pass silently.
		if !errors.Is(sliceErr, errSliceApplyWrite) {
			m.logger.Error("slice-limits stage failed; agent will not be reported ready",
				"agent_id", agentID, "stage", StageSliceLimits, "error", sliceErr)
			return nil, fail(StageSliceLimits, sliceErr)
		}
		m.logger.Warn("failed to apply user slice limits; agent user is unconstrained except for dockerd",
			"agent_id", agentID, "error", sliceErr)
	} else {
		createdUserSlice = true
	}

	// ── Clean up temporary key files (keys are in memory + authorized_keys) ──
	os.Remove(keyFile)
	os.Remove(pubKeyFile)

	// ── Register with tracker ────────────────────────────────────
	effectiveLimits := &v1.ResourceLimits{
		CpuQuota:            cpuQuota,
		MemoryMaxBytes:      memMax,
		DiskMaxBytes:        diskMax,
		MaxDockerContainers: maxDockerContainers,
	}

	// Effective TTL was validated and resolved in Step 1a; `ttl` is used
	// below for the agent record and response ExpiresAt.

	// Build the mount command for the SELECTED driver and the Docker SSH
	// tunnel command once we know the user, home directory, and hostname.
	// MOUNT-006: the sshfs default produces the byte-identical legacy
	// string (sshfsMount keeps feeding SshfsMount/MountSpec.Command).
	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}
	sshfsMount, mountCmdErr := buildMountCommand(mountDriver, sshKeyPath, username, host, userHome, agentID)
	if mountCmdErr != nil {
		return nil, fail(StageValidate, fmt.Errorf("build %s mount command: %w", mountDriver.Name, mountCmdErr))
	}
	mountSpec := &v1.MountSpec{Driver: mountDriver.Name, Command: sshfsMount}
	// Build the SSH tunnel command that forwards a local TCP port to the
	// agent's remote Docker socket.  Port 2376 is the conventional Docker TLS
	// port; on the rare occasion two agents are tunnelled from the same client
	// the user can pick a different local port with `bunker tunnel <id> <port>`.
	dockerHostTunnel := fmt.Sprintf(
		"ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes -i %s -L 2376:%s %s@%s -N",
		sshKeyPath, dockerSockPath, username, host,
	)

	// Persist the assigned port range to the agent's home directory so tools
	// and tests can discover it without querying the tracker.
	bunkerMetaDir := filepath.Join(userHome, ".bunker")
	if err := os.MkdirAll(bunkerMetaDir, 0755); err != nil {
		m.logger.Warn("failed to create .bunker meta dir", "agent_id", agentID, "error", err)
	} else {
		portFile := filepath.Join(bunkerMetaDir, "ports")
		portContent := fmt.Sprintf("%d-%d\n", portStart, portEnd)
		if err := os.WriteFile(portFile, []byte(portContent), 0644); err != nil {
			m.logger.Warn("failed to write port range file", "agent_id", agentID, "error", err)
		}
		// DF-BUNKER-18: stamp THIS daemon's instance identity next to the
		// port metadata, inside the same directory the chown below covers.
		// A second daemon on the same host reads it and leaves the agent
		// alone — the only reliable discrimination when the two pools
		// overlap or this agent's port metadata is unreadable. Best
		// effort, like the port file: spawn's success criteria must not
		// change, and the pool line is recorded for operators only, never
		// consulted by the ownership decision.
		if err := m.writeOwnerMarker(bunkerMetaDir); err != nil {
			m.logger.Warn("failed to write owner marker file", "agent_id", agentID, "error", err)
		}
		if out, err := exec.CommandContext(ctx, "chown", "-R", username+":", bunkerMetaDir).CombinedOutput(); err != nil {
			m.logger.Warn("failed to chown .bunker meta dir", "agent_id", agentID, "error", err, "output", string(out))
		}
	}

	// ── Step 5d: Prove the SSH session works BEFORE reporting ready ──
	// INT-DEMO-002: spawn used to register Status="running" on the strength
	// of the authorized_keys/dockerd stages alone. The INT-DEMO-001 outage
	// had the daemon reporting a RUNNING agent while every SSH session was
	// denied by PAM (ssh exit 254). The probe runs the exec path's exact
	// ssh target/options (single-token `whoami`), is hard-bounded per
	// attempt and capped at three tries, and fails CLOSED: no registration,
	// no persist, no ready response — the standard rollback runs.
	m.logger.Info("spawn entering stage", "agent_id", agentID, "stage", StageSessionProbe)
	attempts, probeErr := probeAgentSession(ctx, agentID, username, sshKeyPath)
	if probeErr != nil {
		m.logger.Error("session probe failed; agent will not be reported ready",
			"agent_id", agentID,
			"stage", StageSessionProbe,
			"attempts", attempts,
			"error", probeErr,
		)
		return nil, fail(StageSessionProbe, probeErr)
	}
	m.logger.Info("session probe succeeded",
		"agent_id", agentID,
		"stage", StageSessionProbe,
		"attempts", attempts,
	)

	rec := &resource.AgentRecord{
		AgentID:           agentID,
		Status:            "running",
		Limits:            effectiveLimits,
		CreatedAt:         time.Now(),
		ExpiresAt:         time.Now().Add(ttl),
		PortRangeStart:    portStart,
		PortRangeEnd:      portEnd,
		SshPrivateKeyPath: sshKeyPath,
		SshfsMount:        sshfsMount,
		DockerHostTunnel:  dockerHostTunnel,
		Image:             imageRef,
		// MOUNT-006: the selected mount driver rides the record so `bunker
		// info`/ListAgents can report an explicit identity and the mount
		// path can dispatch per driver instead of assuming sshfs.
		MountDriver: mountDriver.Name,
		// GAP-116: the effective safety preset and the knob set the agent was
		// actually spawned under — the unit argv's properties, and the slice
		// drop-in's when it was written. `bunker info` reports these as-is.
		SafetyPreset:     preset,
		UnitProperties:   systemdKnobsToProto(unitKnobs),
		SliceProperties:  systemdKnobsToProto(sliceKnobs),
		SliceDropIn:      dropinContent,
		SliceDropInState: sliceDropInState(createdUserSlice),
	}
	if err := m.tracker.Register(rec); err != nil {
		// This shouldn't happen (we checked capacity above), but handle gracefully
		m.logger.Error("tracker register failed", "agent_id", agentID, "error", err)
	}

	// ── Step 3.5: Persist the spawn durably (GAP-070) ──────────────
	// The registry is what survives a daemon restart, so a spawn that
	// cannot be persisted must NOT report success: roll back the user, the
	// SSH key, the port range and the tracker slot, and fail loudly.
	if err := m.persistSpawn(rec); err != nil {
		m.logger.Error("persisting agent spawn failed, rolling back", "agent_id", agentID, "error", err)
		m.tracker.Unregister(agentID)
		rollback()
		rollbackDone = true // this path already rolled back; fail() must not roll back twice
		return nil, fail(StageRegister, fmt.Errorf("persist agent spawn: %w", err))
	}

	// ── Build response ─────────────────────────────────────────────
	// hostname is already determined above for SSHFS/tunnel commands.

	var publicURL string
	if m.tunnelMgr != nil && req.GetNetwork() != nil && req.GetNetwork().GetTrycloudflare() {
		url, err := m.tunnelMgr.Start(ctx, agentID, portStart)
		if err != nil {
			m.logger.Warn("tunnel start failed, continuing without public URL", "agent_id", agentID, "error", err)
		} else {
			publicURL = url
			rec.PublicURL = publicURL
		}
	}

	// ── Start tailscale if network mode requests it ─────────────
	var tailnetIP string
	if m.tailscaleMgr != nil && req.GetNetwork() != nil && req.GetNetwork().GetMode() == v1.NetworkConfig_MODE_TAILSCALE {
		ip, err := m.tailscaleMgr.Start(ctx, agentID)
		if err != nil {
			m.logger.Warn("tailscale start failed, continuing without tailnet IP", "agent_id", agentID, "error", err)
		} else {
			tailnetIP = ip
			rec.TailnetIP = tailnetIP
		}
	}

	// ── Step 3.6: Refresh persisted connection metadata (GAP-070) ──
	// The durability gate above ran before the tunnel/tailscale starts, so
	// the public URL and tailnet IP are upserted here. This second append is
	// best-effort: the agent is already durable, and a lost URL only means a
	// replayed record lacks it until the next spawn.
	if publicURL != "" || tailnetIP != "" {
		if err := m.persistSpawn(rec); err != nil {
			m.logger.Warn("refreshing persisted agent metadata failed", "agent_id", agentID, "error", err)
		}
	}

	resp := &v1.SpawnAgentResponse{
		AgentId:          agentID,
		DockerHostSsh:    fmt.Sprintf("DOCKER_HOST=ssh://%s@%s", username, host),
		DockerHostTunnel: dockerHostTunnel,
		SshfsMount:       sshfsMount,
		Limits:           effectiveLimits,
		PortRangeStart:   portStart,
		PortRangeEnd:     portEnd,
		ExpiresAt:        time.Now().Add(ttl).Format(time.RFC3339),
		PublicUrl:        publicURL,
		TailnetIp:        tailnetIP,
		Image:            imageRef,
		// MOUNT-006: explicit driver identity ALONGSIDE the legacy opaque
		// sshfs_mount string (which keeps its field number and semantics,
		// so a client that ignores mount_spec still mounts via sshfs).
		MountSpec: mountSpec,
	}
	// GAP-128: the wire response carries private key material ONLY when the
	// caller explicitly opted in via return_ssh_private_key. The key itself
	// stays persisted server-side (SshPrivateKeyPath above), so opt-in
	// callers and the GetAgentKey RPC both read the same secret.
	if req.GetReturnSshPrivateKey() {
		resp.SshPrivateKey = string(privKeyBytes)
	}

	m.logger.Info("agent spawned successfully", "agent_id", agentID)
	return resp, nil
}

// Destroy tears down an agent: stops the dockerd systemd user unit,
// removes the Linux user with -r, cleans up /run/bunker/<id>/.
// Returns a DestroyAgentResponse with status "destroyed", "not_found", or "error".
// countAgentContainers runs `docker ps -q` against the per-agent docker socket
// and returns the number of running containers. It is an error if the docker
// CLI cannot be reached, because enforcement requires a working daemon.
var countAgentContainers = func(ctx context.Context, dockerSockPath string) (uint32, error) {
	cmd := exec.CommandContext(ctx, "docker", "--host", "unix://"+dockerSockPath, "ps", "-q")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("docker ps: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	lines := strings.Fields(string(out))
	return uint32(len(lines)), nil
}

// resetStaleDockerdUnit clears leftover state from a previous unit with the
// same name before systemd-run creates it:
//
//   - a loaded or failed transient unit makes `systemd-run --unit=` fail with
//     "already loaded", so the unit is stopped, disabled and reset;
//   - an orphaned dockerd process keeps the socket and the runtime dir busy, so
//     any surviving process is killed directly (systemctl --user targets the
//     wrong user manager for a systemd-run --system --uid unit).
//
// It is a named function (rather than four inline best-effort calls) so the
// sequence is covered by a regression test — losing it makes every re-spawn of
// a stale agent fail with "already loaded".
func resetStaleDockerdUnit(ctx context.Context, unitName, username string, logger *slog.Logger) {
	_ = exec.CommandContext(ctx, "systemctl", "stop", unitName).Run()
	_ = exec.CommandContext(ctx, "systemctl", "disable", unitName).Run()
	_ = exec.CommandContext(ctx, "systemctl", "reset-failed", unitName).Run()
	_ = stopDockerdDirect(ctx, username, unitName, logger)
}

// stopDockerdDirect finds the dockerd process running under the given user
// and sends it SIGTERM, then SIGKILL after a grace period. This avoids the
// systemctl --user session mismatch because the dockerd was started via
// systemd-run --user under the agent's user session, not the root session.
func stopDockerdDirect(ctx context.Context, username, unitName string, logger *slog.Logger) error {
	// Try pgrep first: find dockerd processes owned by the agent user.
	cmd := exec.CommandContext(ctx, "pgrep", "-u", username, "-f", "dockerd")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pgrep dockerd for user %s: %w (output: %s)", username, err, string(out))
	}
	pids := strings.Fields(string(out))
	if len(pids) == 0 {
		return fmt.Errorf("no dockerd process found for user %s", username)
	}

	for _, pidStr := range pids {
		pid := strings.TrimSpace(pidStr)
		if pid == "" {
			continue
		}
		logger.Info("sending SIGTERM to dockerd", "pid", pid, "user", username)
		if err := exec.CommandContext(ctx, "kill", "-TERM", pid).Run(); err != nil {
			logger.Warn("SIGTERM dockerd failed", "pid", pid, "error", err)
		}
	}

	// Wait up to 5s for processes to exit, then SIGKILL.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cmd = exec.CommandContext(ctx, "pgrep", "-u", username, "-f", "dockerd")
		out, err = cmd.CombinedOutput()
		if err != nil || len(strings.Fields(string(out))) == 0 {
			logger.Info("dockerd exited after SIGTERM", "user", username)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	// SIGKILL remaining processes
	for _, pidStr := range pids {
		pid := strings.TrimSpace(pidStr)
		if pid == "" {
			continue
		}
		logger.Info("sending SIGKILL to dockerd", "pid", pid, "user", username)
		_ = exec.CommandContext(ctx, "kill", "-KILL", pid).Run()
	}
	return nil
}

// waitForDockerd polls briefly for a dockerd process owned by the agent user and
// for the Docker socket to exist. It also captures systemd unit status on failure
// so callers get actionable error messages instead of "spawn succeeded".
func waitForDockerd(ctx context.Context, username, dockerSockPath, actualDockerSockPath, unitName string, logger *slog.Logger) error {
	poll := 200 * time.Millisecond
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Check for a dockerd process owned by this user.
		running, _ := dockerdProcessChecker(ctx, username)
		if running {
			// Process is up; now wait for the socket to appear. Rootless dockerd
			// may create the socket at the configured Bunker path or under the
			// systemd user runtime directory (/run/user/<uid>). When the real
			// socket is under /run/user/<uid>, create a symlink from the Bunker
			// path so authorized_keys, .profile, and docker tunnels keep working.
			if _, statErr := os.Stat(dockerSockPath); statErr == nil {
				logger.Info("dockerd ready", "user", username, "sock", dockerSockPath)
				return nil
			}
			if _, statErr := os.Stat(actualDockerSockPath); statErr == nil {
				if err := os.Remove(dockerSockPath); err != nil && !os.IsNotExist(err) {
					logger.Warn("failed to remove stale socket path before symlink", "path", dockerSockPath, "error", err)
				}
				if err := os.Symlink(actualDockerSockPath, dockerSockPath); err != nil {
					logger.Error("dockerd socket under /run/user but symlink failed", "expected", dockerSockPath, "actual", actualDockerSockPath, "error", err)
					return fmt.Errorf("dockerd socket at %s but could not symlink to %s: %w", actualDockerSockPath, dockerSockPath, err)
				}
				logger.Info("dockerd ready", "user", username, "expected_sock", dockerSockPath, "actual_sock", actualDockerSockPath)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}

	// Deadline exceeded — capture unit status and recent journal output for diagnostics.
	statusOut, _ := exec.CommandContext(ctx, "systemctl", "status", unitName, "--no-pager", "-l", "-n", "20").CombinedOutput()
	journalOut, _ := exec.CommandContext(ctx, "journalctl", "-u", unitName, "--no-pager", "-n", "20").CombinedOutput()
	logger.Error("dockerd did not start within deadline",
		"user", username,
		"unit", unitName,
		"sock", dockerSockPath,
		"systemctl_status", string(statusOut),
		"journal", string(journalOut),
	)
	return fmt.Errorf("dockerd did not start for user %s (unit %s, socket %s)", username, unitName, dockerSockPath)
}

// dockerdProcessChecker abstracts the pgrep check so tests can substitute a
// fake process table without requiring a real dockerd process.
var dockerdProcessChecker = func(ctx context.Context, username string) (bool, error) {
	out, err := exec.CommandContext(ctx, "pgrep", "-u", username, "-f", "dockerd").CombinedOutput()
	if err != nil {
		return false, nil // pgrep exits non-zero when no process matches
	}
	return len(strings.Fields(string(out))) > 0, nil
}

// applyUserSliceLimits creates a systemd drop-in for user.slice/user-<uid>.slice
// so that *all* processes owned by the agent user inherit the configured cgroup
// limits — not just the dockerd unit.  The drop-in is written to
// /etc/systemd/system/user-<UID>.slice.d/50-bunker.conf.
//
// GAP-116: the property block is table-driven and the written content is
// returned so the spawn can stamp it on the agent record for `bunker info`
// effective-set reporting.
//
// GAP-117: sliceKnobs is the resolved tier's knob set — the SAME resolution
// the unit argv consumed (KnobsForPreset at the spawn site), so both
// enforcement points answer to one tier table. A nil/empty sliceKnobs falls
// back to the direct derivation (identical values/order/conditional), which
// keeps standalone callers byte-identical; the spawn site always passes the
// tier-resolved set.
func applyUserSliceLimits(ctx context.Context, u *user.User, cpuQuota float64, memMax, diskMax, maxProcs, maxFiles uint64, sliceKnobs []SystemdKnob, logger *slog.Logger) (string, error) {
	sliceName := fmt.Sprintf("user-%s.slice", u.Uid)
	dropinDir := userSliceDropinDir(u.Uid)
	if err := os.MkdirAll(dropinDir, 0755); err != nil {
		// INT-CI-37: the whole write leg is degradable — mark it.
		return "", fmt.Errorf("%w: mkdir %s: %v", errSliceApplyWrite, dropinDir, err)
	}

	if len(sliceKnobs) == 0 {
		sliceKnobs = sliceKnobsFor(cpuQuota, memMax, diskMax, maxProcs, maxFiles)
	}
	var parts []string
	parts = append(parts, "[Slice]")
	for _, k := range sliceKnobs {
		parts = append(parts, k.Name+"="+k.Value)
	}
	content := strings.Join(parts, "\n") + "\n"

	confPath := filepath.Join(dropinDir, "50-bunker.conf")
	if err := os.WriteFile(confPath, []byte(content), 0644); err != nil {
		// INT-CI-37: the write is the DEGRADABLE leg — wrap it in the
		// errSliceApplyWrite sentinel so the caller (the ordered gate) can
		// tell a failed write apart from a failed landing check.
		return "", fmt.Errorf("%w: write %s: %v", errSliceApplyWrite, confPath, err)
	}
	logger.Info("wrote user slice drop-in", "slice", sliceName, "path", confPath)

	// Reload systemd so the slice picks up the new limits immediately.
	// INT-CI-37: this runs through the sliceApplySystemctl seam and MUST
	// complete before the caller's landing check reads the cgroup — the
	// check only ever verifies what the drop-in systemd has consumed.
	if out, err := sliceApplySystemctl(ctx, "systemctl", "daemon-reload"); err != nil {
		return "", fmt.Errorf("%w: daemon-reload: %v (output: %s)", errSliceApplyWrite, err, string(out))
	}
	return content, nil
}

// writeOwnerMarker stamps this daemon's restart-stable instance identity into
// the agent's metadata directory (DF-BUNKER-18), next to `.bunker/ports`:
//
//	<daemon-instance-id>\n<pool-start>-<pool-end>\n
//
// Reconciliation reads it back and treats an orphan carrying a DIFFERENT id as
// foreign — which is the only way a second daemon on the same host can tell
// its own agents apart when the two port pools overlap or the port metadata is
// unreadable. The pool line is informational (operator diagnostics); it never
// takes part in the ownership decision.
//
// No file is written when this daemon has no identity (the identity file could
// not be created or read): an absent marker degrades to the documented legacy
// handling instead of fabricating ownership this daemon cannot honour. The
// caller treats an error as a warning, exactly like the port file — the marker
// must never change spawn's success criteria.
func (m *AgentManager) writeOwnerMarker(metaDir string) error {
	if m.instanceID == "" {
		return nil
	}
	content := fmt.Sprintf("%s\n%s\n", m.instanceID, m.poolFingerprint())
	return os.WriteFile(filepath.Join(metaDir, ownerMarkerFilename), []byte(content), 0644)
}
