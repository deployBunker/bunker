// Package agent manages agent lifecycle: create users, generate SSH keys, start dockerd.
package agent

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	"github.com/deployBunker/bunker/internal/tailscale"
	"github.com/deployBunker/bunker/internal/tunnel"
)

// validAgentID matches agent IDs: lowercase, digits, hyphens, 1-63 chars.
var validAgentID = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// AgentManager handles the agent spawn lifecycle.
type AgentManager struct {
	cfg          *config.Config
	logger       *slog.Logger
	tracker      *resource.Tracker
	portAlloc    *resource.PortAllocator
	tunnelMgr    *tunnel.TunnelManager
	tailscaleMgr *tailscale.TailscaleManager
}

// NewAgentManager creates a new AgentManager.
func NewAgentManager(cfg *config.Config, logger *slog.Logger, tracker *resource.Tracker, tunnelMgr *tunnel.TunnelManager, tailscaleMgr *tailscale.TailscaleManager) *AgentManager {
	pa, err := resource.NewPortAllocator(
		cfg.Agent.PortRangeStart,
		cfg.Agent.PortRangeEnd,
		cfg.Agent.PortRangePerAgent,
	)
	if err != nil {
		logger.Warn("port allocator disabled — invalid port range config",
			"start", cfg.Agent.PortRangeStart,
			"end", cfg.Agent.PortRangeEnd,
			"per_agent", cfg.Agent.PortRangePerAgent,
			"error", err,
		)
		// Port allocator is nil when disabled; spawn will use the full range as fallback.
	}
	return &AgentManager{cfg: cfg, logger: logger, tracker: tracker, portAlloc: pa, tunnelMgr: tunnelMgr, tailscaleMgr: tailscaleMgr}
}

// generateUUIDv4 creates a version-4 UUID using crypto/rand.
func generateUUIDv4() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand read: %w", err)
	}
	// Set version 4 and variant bits.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Spawn creates a new agent: validates/generates agent_id, creates a Linux user,
// generates an SSH keypair, sets up authorized_keys, and starts dockerd via systemd-run.
// On failure, previous steps are cleaned up.
func (m *AgentManager) Spawn(ctx context.Context, req *v1.SpawnAgentRequest) (*v1.SpawnAgentResponse, error) {
	// ── Step 1: Validate or generate agent_id ──────────────────────
	agentID := req.GetAgentId()
	if agentID != "" {
		if !validAgentID.MatchString(agentID) {
			return nil, fmt.Errorf("invalid agent_id %q: must match [a-z0-9-]{1,63}", agentID)
		}
	} else {
		uuid, err := generateUUIDv4()
		if err != nil {
			return nil, fmt.Errorf("generate agent_id uuid: %w", err)
		}
		// Use first segment of UUID as short ID.
		agentID = strings.SplitN(uuid, "-", 2)[0]
	}

	// ── Step 1.5: Allocate port range ────────────────────────────
	var portStart, portEnd uint32
	if m.portAlloc != nil {
		var allocErr error
		portStart, portEnd, allocErr = m.portAlloc.Allocate(agentID)
		if allocErr != nil {
			return nil, fmt.Errorf("port range allocation: %w", allocErr)
		}
	} else {
		// Port allocator disabled — use full configured range as fallback.
		portStart = m.cfg.Agent.PortRangeStart
		portEnd = m.cfg.Agent.PortRangeEnd
	}
	m.logger.Info("allocated port range", "agent_id", agentID, "range", fmt.Sprintf("%d-%d", portStart, portEnd))

	// ── Step 1.6: Check capacity ─────────────────────────────────
	if !m.tracker.HasCapacity(1) {
		return nil, fmt.Errorf("capacity full: %d/%d agents", m.tracker.Count(), m.tracker.MaxAgents())
	}

	m.logger.Info("spawning agent", "agent_id", agentID)

	// Track what we've created for cleanup on failure.
	var createdUser bool
	var keyFile string

	cleanup := func() {
		if createdUser {
			m.logger.Warn("rolling back: removing user", "username", "bunker-"+agentID)
			cmd := exec.CommandContext(ctx, "userdel", "-r", "bunker-"+agentID)
			if out, err := cmd.CombinedOutput(); err != nil {
				m.logger.Error("rollback userdel failed",
					"agent_id", agentID,
					"error", err,
					"output", string(out),
				)
			}
		}
		if keyFile != "" {
			os.Remove(keyFile)
			os.Remove(keyFile + ".pub")
		}
		// Remove persisted SSH key from config dir
		sshKeyPath := filepath.Join(m.cfg.Agent.SSHDir, agentID)
		os.Remove(sshKeyPath)
	}

	// ── Step 2: Create Linux user ──────────────────────────────────
	username := "bunker-" + agentID
	m.logger.Info("creating user", "username", username)
	cmd := exec.CommandContext(ctx, "useradd", "-m", "-s", "/bin/bash", username)
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("useradd %s failed: %w (output: %s)", username, err, string(out))
	}
	createdUser = true

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
		cleanup()
		return nil, fmt.Errorf("ssh-keygen failed: %w (output: %s)", err, string(out))
	}
	pubKeyFile := keyFile + ".pub"

	// Read keys into memory.
	privKeyBytes, err := os.ReadFile(keyFile)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("read private key %s: %w", keyFile, err)
	}
	pubKeyBytes, err := os.ReadFile(pubKeyFile)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("read public key %s: %w", pubKeyFile, err)
	}

	// ── Step 4: Set up .ssh/authorized_keys with DOCKER_HOST env ──
	userHome := "/home/" + username
	sshDir := filepath.Join(userHome, ".ssh")
	authKeysFile := filepath.Join(sshDir, "authorized_keys")
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)

	m.logger.Info("setting up authorized_keys", "user", username)
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		cleanup()
		return nil, fmt.Errorf("create .ssh dir %s: %w", sshDir, err)
	}
	// Chown the .ssh directory to the user.
	if out, err := exec.CommandContext(ctx, "chown", "-R", username, sshDir).CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("chown .ssh dir: %w (output: %s)", err, string(out))
	}

	// Prepend environment= to the public key line so Docker's SSH transport
	// finds the right socket (requires PermitUserEnvironment=yes in sshd_config).
	// The pubKeyBytes end with a newline from ssh-keygen; strip and re-append.
	pubKeyLine := strings.TrimSpace(string(pubKeyBytes))
	envPrefix := fmt.Sprintf(`environment="DOCKER_HOST=unix://%s"`, dockerSockPath)
	authKeysContent := envPrefix + " " + pubKeyLine + "\n"

	if err := os.WriteFile(authKeysFile, []byte(authKeysContent), 0600); err != nil {
		cleanup()
		return nil, fmt.Errorf("write authorized_keys: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "chown", username, authKeysFile).CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("chown authorized_keys: %w (output: %s)", err, string(out))
	}

	// ── Step 4b: Set up .profile with DOCKER_HOST (interactive sessions) ──
	profilePath := filepath.Join(userHome, ".profile")
	profileContent := fmt.Sprintf("# bunker: per-agent Docker socket\nexport DOCKER_HOST=unix://%s\n", dockerSockPath)
	if err := os.WriteFile(profilePath, []byte(profileContent), 0644); err != nil {
		cleanup()
		return nil, fmt.Errorf("write .profile: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "chown", username, profilePath).CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("chown .profile: %w (output: %s)", err, string(out))
	}

	// ── Step 4c: Persist private key to the server's SSH directory ──
	sshKeyPath := filepath.Join(m.cfg.Agent.SSHDir, agentID)
	if err := os.MkdirAll(m.cfg.Agent.SSHDir, 0700); err != nil {
		cleanup()
		return nil, fmt.Errorf("create ssh dir %s: %w", m.cfg.Agent.SSHDir, err)
	}
	if err := os.WriteFile(sshKeyPath, privKeyBytes, 0600); err != nil {
		cleanup()
		return nil, fmt.Errorf("write ssh private key to %s: %w", sshKeyPath, err)
	}
	m.logger.Info("persisted SSH private key", "path", sshKeyPath)

	// ── Step 5: Start dockerd via systemd-run ──────────────────────
	dockerSockPath = fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	unitName := "bunker-docker-" + agentID
	m.logger.Info("starting dockerd", "unit", unitName, "sock", dockerSockPath)

	// Create the socket directory.
	sockDir := filepath.Dir(dockerSockPath)
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		cleanup()
		return nil, fmt.Errorf("create docker sock dir %s: %w", sockDir, err)
	}
	// Chown the socket directory to the agent user so dockerd can create the socket
	// and the SSH transport can access it.
	if out, err := exec.CommandContext(ctx, "chown", username, sockDir).CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("chown socket dir: %w (output: %s)", err, string(out))
	}

	// Determine resource limits: use request limits or server defaults
	cpuQuota := m.cfg.Agent.DefaultCPUQuota
	memMax := m.cfg.Agent.DefaultMemoryBytes
	if req.GetLimits() != nil {
		if req.GetLimits().GetCpuQuota() > 0 {
			cpuQuota = req.GetLimits().GetCpuQuota()
		}
		if req.GetLimits().GetMemoryMaxBytes() > 0 {
			memMax = req.GetLimits().GetMemoryMaxBytes()
		}
	}

	// Build systemd-run args with cgroup resource limits
	systemdArgs := []string{
		"--user",
		"--unit=" + unitName,
	}
	if cpuQuota > 0 {
		// CPUQuota is a percentage of one CPU: 100%=1 core, 200%=2 cores
		systemdArgs = append(systemdArgs, fmt.Sprintf("--property=CPUQuota=%d%%", int(cpuQuota*100)))
	}
	if memMax > 0 {
		systemdArgs = append(systemdArgs, fmt.Sprintf("--property=MemoryMax=%d", memMax))
	}
	systemdArgs = append(systemdArgs, "dockerd", "--host=unix://"+dockerSockPath)

	// If a stale unit exists (from a previous incomplete destroy), stop and disable it
	// under the agent's user session.  systemctl --user from the root foreman targets
	// the root user manager, not the agent's.  Use --machine to reach the right session.
	stopCmd := exec.CommandContext(ctx, "systemctl", "--user", "--machine="+username+"@", "stop", unitName)
	stopCmd.Run() // ignore error — unit may not exist
	stopCmd = exec.CommandContext(ctx, "systemctl", "--user", "--machine="+username+"@", "disable", unitName)
	stopCmd.Run() // ignore error — unit may not exist

	// Also kill any orphaned dockerd process directly (belt-and-suspenders).
	_ = stopDockerdDirect(ctx, username, unitName, m.logger)

	cmd = exec.CommandContext(ctx, "systemd-run", systemdArgs...)
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("systemd-run dockerd failed: %w (output: %s)", err, string(out))
	}

	// ── Clean up temporary key files (keys are in memory + authorized_keys) ──
	os.Remove(keyFile)
	os.Remove(pubKeyFile)

	// ── Register with tracker ────────────────────────────────────
	effectiveLimits := &v1.ResourceLimits{
		CpuQuota:       cpuQuota,
		MemoryMaxBytes: memMax,
	}
	if req.GetLimits() != nil {
		effectiveLimits.DiskMaxBytes = req.GetLimits().GetDiskMaxBytes()
		effectiveLimits.MaxDockerContainers = req.GetLimits().GetMaxDockerContainers()
	}

	rec := &resource.AgentRecord{
		AgentID:           agentID,
		Status:            "running",
		Limits:            effectiveLimits,
		CreatedAt:         time.Now(),
		ExpiresAt:         time.Now().Add(6 * time.Hour),
		PortRangeStart:    portStart,
		PortRangeEnd:      portEnd,
		SshPrivateKeyPath: sshKeyPath,
	}
	if err := m.tracker.Register(rec); err != nil {
		// This shouldn't happen (we checked capacity above), but handle gracefully
		m.logger.Error("tracker register failed", "agent_id", agentID, "error", err)
	}

	// ── Build response ─────────────────────────────────────────────
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}

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

	resp := &v1.SpawnAgentResponse{
		AgentId:        agentID,
		DockerHostSsh:  fmt.Sprintf("DOCKER_HOST=ssh://%s@%s", username, hostname),
		SshPrivateKey:  string(privKeyBytes),
		Limits:         effectiveLimits,
		PortRangeStart: portStart,
		PortRangeEnd:   portEnd,
		ExpiresAt:      time.Now().Add(6 * time.Hour).Format(time.RFC3339),
		PublicUrl:      publicURL,
		TailnetIp:      tailnetIP,
	}

	m.logger.Info("agent spawned successfully", "agent_id", agentID)
	return resp, nil
}

// Destroy tears down an agent: stops the dockerd systemd user unit,
// removes the Linux user with -r, cleans up /run/bunker/<id>/.
// Returns a DestroyAgentResponse with status "destroyed", "not_found", or "error".
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

func (m *AgentManager) Destroy(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error) {
	// Step 0: validate agent_id
	if agentID == "" || !validAgentID.MatchString(agentID) {
		return &v1.DestroyAgentResponse{AgentId: agentID, Status: "error"},
			fmt.Errorf("invalid agent_id %q", agentID)
	}

	m.logger.Info("destroying agent", "agent_id", agentID)

	// Step 1: Stop the dockerd systemd user unit
	unitName := "bunker-docker-" + agentID
	username := "bunker-" + agentID

	// The dockerd unit was started via systemd-run --user, so it runs under
	// the agent's user session. systemctl --user from the root foreman session
	// targets the wrong user manager. We must either:
	//   (a) use systemctl --user --machine=<user>@.host, or
	//   (b) find the dockerd PID and kill it directly.
	// Option (b) is simpler and avoids DBus/machined dependencies.
	if err := stopDockerdDirect(ctx, username, unitName, m.logger); err != nil {
		m.logger.Warn("direct dockerd stop failed", "unit", unitName, "error", err)
		// Fallback: try systemctl --user (may work if user linger is enabled)
		cmd := exec.CommandContext(ctx, "systemctl", "--user", "stop", unitName)
		if out, err := cmd.CombinedOutput(); err != nil {
			m.logger.Warn("systemctl stop failed (may not exist)", "unit", unitName, "error", err, "output", string(out))
		}
	}

	// Step 2: Disable the unit (prevent auto-restart)
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "disable", unitName)
	if out, err := cmd.CombinedOutput(); err != nil {
		m.logger.Warn("systemctl disable failed", "unit", unitName, "error", err, "output", string(out))
	}

	// Step 3: Remove the Linux user
	cmd = exec.CommandContext(ctx, "userdel", "-r", username)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Check if user doesn't exist (already destroyed)
		if !force {
			return &v1.DestroyAgentResponse{AgentId: agentID, Status: "not_found"},
				fmt.Errorf("userdel %s failed: %w (output: %s)", username, err, string(out))
		}
		// Force mode: log and continue even if userdel fails
		m.logger.Warn("userdel failed in force mode", "username", username, "error", err, "output", string(out))
	}

	// Step 4: Clean up /run/bunker/<id>/ directory
	runDir := fmt.Sprintf("/run/bunker/%s", agentID)
	if err := os.RemoveAll(runDir); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("failed to remove run dir", "dir", runDir, "error", err)
	}

	// Step 4.5: Clean up persisted SSH key
	sshKeyPath := filepath.Join(m.cfg.Agent.SSHDir, agentID)
	if err := os.Remove(sshKeyPath); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("failed to remove ssh key", "path", sshKeyPath, "error", err)
	}

	if m.tunnelMgr != nil {
		if err := m.tunnelMgr.Stop(agentID); err != nil {
			m.logger.Warn("tunnel stop failed", "agent_id", agentID, "error", err)
		}
	}

	if m.tailscaleMgr != nil {
		if err := m.tailscaleMgr.Stop(agentID); err != nil {
			m.logger.Warn("tailscale stop failed", "agent_id", agentID, "error", err)
		}
	}

	m.tracker.Unregister(agentID)

	if m.portAlloc != nil {
		m.portAlloc.Free(agentID)
		m.logger.Info("freed port range", "agent_id", agentID)
	}

	m.logger.Info("agent destroyed", "agent_id", agentID)
	return &v1.DestroyAgentResponse{AgentId: agentID, Status: "destroyed"}, nil
}
