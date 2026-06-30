// Package agent provides rootless Docker setup helpers for unprivileged agents.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// rootlessInstallURL is the official Docker rootless extras installer.
const rootlessInstallURL = "https://get.docker.com/rootless"

// configureSubIDs ensures /etc/subuid and /etc/subgid contain a mapping for the
// given username. Rootless Docker needs a contiguous 65,536 UID/GID range per
// user. We map the range starting at the user's own UID/GID so every agent gets
// a unique namespace derived from its system identity.
func configureSubIDs(ctx context.Context, username string) error {
	u, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("lookup user %s: %w", username, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("parse uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return fmt.Errorf("parse gid %q: %w", u.Gid, err)
	}

	if err := ensureSubIDEntry("/etc/subuid", username, uid); err != nil {
		return fmt.Errorf("subuid: %w", err)
	}
	if err := ensureSubIDEntry("/etc/subgid", username, gid); err != nil {
		return fmt.Errorf("subgid: %w", err)
	}
	return nil
}

// ensureSubIDEntry appends a single mapping line to path when no mapping for
// name exists. The mapping is name:start:65536.
func ensureSubIDEntry(path, name string, start int) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) >= 1 && fields[0] == name {
			return nil // already configured
		}
	}
	entry := fmt.Sprintf("%s:%d:65536\n", name, start)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(entry); err != nil {
		return fmt.Errorf("append %s: %w", path, err)
	}
	return nil
}

// installRootlessDocker ensures the agent has the rootless Docker scripts in
// ~/bin. If dockerd-rootless.sh is not present, it downloads the official
// installer and runs it as the target user. The installer is idempotent.
func installRootlessDocker(ctx context.Context, username, userHome string, logger *slog.Logger) error {
	binDir := filepath.Join(userHome, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return fmt.Errorf("create bin dir: %w", err)
	}
	// The bin directory must be owned by the agent user because the installer
	// runs as that user and writes binaries into it.
	if out, err := exec.CommandContext(ctx, "chown", "-R", username, binDir).CombinedOutput(); err != nil {
		return fmt.Errorf("chown bin dir: %w (output: %s)", err, string(out))
	}

	rootlessScript := filepath.Join(binDir, "dockerd-rootless.sh")
	if _, err := os.Stat(rootlessScript); err == nil {
		logger.Info("rootless docker already installed", "user", username, "path", rootlessScript)
		return nil
	}

	logger.Info("installing rootless docker", "user", username)

	// Download the installer into the agent's home as the agent user so file
	// ownership is correct from the start.
	installerPath := filepath.Join(userHome, "rootless-install.sh")
	curl := exec.CommandContext(ctx, "curl", "-fsSL", "-o", installerPath, rootlessInstallURL)
	if out, err := curl.CombinedOutput(); err != nil {
		return fmt.Errorf("download rootless installer: %w (output: %s)", err, string(out))
	}
	if err := os.Chmod(installerPath, 0755); err != nil {
		return fmt.Errorf("chmod installer: %w", err)
	}

	// Run the installer as the target user. It installs binaries into ~/bin.
	// USERNAME is required by the installer to know which home directory to use.
	cmd := exec.CommandContext(ctx, "su", "-", username, "-c", installerPath)
	cmd.Env = append(os.Environ(),
		"FORCE_ROOTLESS_INSTALL=1",
		"SKIP_IPTABLES=1",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("run rootless installer as %s: %w (output: %s)", username, err, string(out))
	}

	if _, err := os.Stat(rootlessScript); err != nil {
		return fmt.Errorf("rootless install completed but %s is missing: %w", rootlessScript, err)
	}
	return nil
}

// ensureRootlesskitAppArmor writes an AppArmor profile for rootlesskit on
// Ubuntu 24.04+ where unprivileged user namespaces are restricted by
// AppArmor. The profile format matches the one emitted by Docker's own
// rootless installer when it fails on apparmor_restrict_unprivileged_userns.
func ensureRootlesskitAppArmor(ctx context.Context, username string, logger *slog.Logger) error {
	u, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("lookup user %s: %w", username, err)
	}
	rootlesskitBin := filepath.Join(u.HomeDir, "bin", "rootlesskit")
	profileName := fmt.Sprintf("home.%s.bin.rootlesskit", username)
	profilePath := filepath.Join("/etc/apparmor.d", profileName)

	// If the profile already exists, assume it is correct.
	if _, err := os.Stat(profilePath); err == nil {
		return nil
	}

	// apparmor_parser is required; if it's missing we cannot install the profile.
	if _, err := exec.LookPath("apparmor_parser"); err != nil {
		return fmt.Errorf("apparmor_parser not found: %w", err)
	}

	profile := fmt.Sprintf(`# ref: https://ubuntu.com/blog/ubuntu-23-10-restricted-unprivileged-user-namespaces
abi <abi/4.0>,
include <tunables/global>

%s flags=(unconfined) {
  userns,

  # Site-specific additions and overrides. See local/README for details.
  include if exists <local/%s>
}
`, rootlesskitBin, profileName)

	if err := os.WriteFile(profilePath, []byte(profile), 0644); err != nil {
		return fmt.Errorf("write apparmor profile %s: %w", profilePath, err)
	}

	cmd := exec.CommandContext(ctx, "apparmor_parser", "-r", profilePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("load apparmor profile %s: %w (output: %s)", profilePath, err, string(out))
	}
	logger.Info("installed rootlesskit apparmor profile", "profile", profileName)
	return nil
}
