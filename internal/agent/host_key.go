package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// hostKeyPaths lists locations to search for a host SSH public key.
// These are checked in order; the first existing key is used.
var hostKeyPaths = []string{
	"/root/.ssh/id_ed25519.pub",
	"/root/.ssh/id_rsa.pub",
	"/home/kara/.ssh/id_ed25519.pub",
}

// provisionHostSSHKey reads the host's SSH public key and appends it to the
// agent's authorized_keys so the host can SSH directly into the agent container.
// This prevents "Permission denied (publickey)" errors when the host needs to
// connect to the agent.
//
// If no host key is found at any of the known locations, the function logs a
// warning and returns nil (spawn still succeeds).
func provisionHostSSHKey(ctx context.Context, username, authKeysFile string, logger *slog.Logger) error {
	hostPubKey, path := findHostPublicKey()
	if hostPubKey == "" {
		logger.Warn("no host SSH public key found, agent will not accept host connections",
			"searched_paths", hostKeyPaths,
		)
		return nil
	}

	logger.Info("found host SSH public key, appending to agent authorized_keys",
		"path", path,
		"agent_user", username,
	)

	// Open authorized_keys for appending.
	f, err := os.OpenFile(authKeysFile, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open authorized_keys for append: %w", err)
	}
	defer f.Close()

	// Append the host key as a plain key line (no environment= prefix — the host
	// connects directly, not through the Docker SSH transport).
	if _, err := f.WriteString(hostPubKey); err != nil {
		return fmt.Errorf("append host key to authorized_keys: %w", err)
	}

	logger.Info("provisioned host SSH key into agent authorized_keys", "agent_user", username)
	return nil
}

// findHostPublicKey scans hostKeyPaths and returns the first existing key's
// content (trimmed of surrounding whitespace) plus the path where it was found.
// Returns ("", "") if no key exists.
func findHostPublicKey() (string, string) {
	for _, path := range hostKeyPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}
		// Ensure the line ends with a newline for authorized_keys compatibility.
		return content + "\n", path
	}
	return "", ""
}
