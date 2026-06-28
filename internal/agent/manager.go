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
)

// validAgentID matches agent IDs: lowercase, digits, hyphens, 1-63 chars.
var validAgentID = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// AgentManager handles the agent spawn lifecycle.
type AgentManager struct {
	cfg    *config.Config
	logger *slog.Logger
}

// NewAgentManager creates a new AgentManager.
func NewAgentManager(cfg *config.Config, logger *slog.Logger) *AgentManager {
	return &AgentManager{cfg: cfg, logger: logger}
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

	// ── Step 4: Set up .ssh/authorized_keys ────────────────────────
	userHome := "/home/" + username
	sshDir := filepath.Join(userHome, ".ssh")
	authKeysFile := filepath.Join(sshDir, "authorized_keys")

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
	if err := os.WriteFile(authKeysFile, pubKeyBytes, 0600); err != nil {
		cleanup()
		return nil, fmt.Errorf("write authorized_keys: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "chown", username, authKeysFile).CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("chown authorized_keys: %w (output: %s)", err, string(out))
	}

	// ── Step 5: Start dockerd via systemd-run ──────────────────────
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	unitName := "bunker-docker-" + agentID
	m.logger.Info("starting dockerd", "unit", unitName, "sock", dockerSockPath)

	// Create the socket directory.
	sockDir := filepath.Dir(dockerSockPath)
	if err := os.MkdirAll(sockDir, 0755); err != nil {
		cleanup()
		return nil, fmt.Errorf("create docker sock dir %s: %w", sockDir, err)
	}

	cmd = exec.CommandContext(ctx, "systemd-run",
		"--user",
		"--unit="+unitName,
		"dockerd",
		"--host=unix://"+dockerSockPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("systemd-run dockerd failed: %w (output: %s)", err, string(out))
	}

	// ── Clean up temporary key files (keys are in memory + authorized_keys) ──
	os.Remove(keyFile)
	os.Remove(pubKeyFile)

	// ── Build response ─────────────────────────────────────────────
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}

	resp := &v1.SpawnAgentResponse{
		AgentId:        agentID,
		DockerHostSsh:  fmt.Sprintf("DOCKER_HOST=ssh://%s@%s", username, hostname),
		SshPrivateKey:  string(privKeyBytes),
		Limits:         req.GetLimits(),
		PortRangeStart: m.cfg.Agent.PortRangeStart,
		PortRangeEnd:   m.cfg.Agent.PortRangeEnd,
		ExpiresAt:      time.Now().Add(6 * time.Hour).Format(time.RFC3339),
	}

	m.logger.Info("agent spawned successfully", "agent_id", agentID)
	return resp, nil
}
