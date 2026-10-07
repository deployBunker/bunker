// Package agent: server-side mount-command generation per driver (MOUNT-006).
//
// Before the seam this was one hardcoded sshfs format string inline in
// Spawn (~manager_spawn.go:585). Building it through the mountdriver
// registry keeps the sshfs output BYTE-IDENTICAL (the legacy sshfs_mount
// string and its proto field keep their semantics for old clients) while
// making the driver an explicit, requestable choice whose unknown names
// REFUSE by name — never a silent sshfs fallback.
package agent

import (
	"fmt"
	"path/filepath"

	"github.com/deployBunker/bunker/internal/mountdriver"
)

// resolveMountDriver resolves the requested mount-driver name against the
// registry. An unknown name is a named error wrapping
// mountdriver.ErrUnknownDriver — the spawn maps it to a refusal naming the
// offending driver; it is never a silent fallback to sshfs.
func resolveMountDriver(name string) (mountdriver.Driver, error) {
	d, err := mountdriver.Resolve(name)
	if err != nil {
		return mountdriver.Driver{}, fmt.Errorf("mount driver %q not registered on this server: %w", name, err)
	}
	return d, nil
}

// buildMountCommand returns the stored mount command for the selected
// driver. For the sshfs default the command is byte-identical to the
// pre-seam hardcoded string:
//
//	sshfs -o IdentityFile=<key> -o idmap=user -o allow_other <user>@<host>:<home> /mnt/bunker/<id>
func buildMountCommand(d mountdriver.Driver, sshKeyPath, username, host, userHome, agentID string) (string, error) {
	switch d.Name {
	case mountdriver.DefaultDriver:
		return fmt.Sprintf("sshfs -o IdentityFile=%s -o idmap=user -o allow_other %s@%s:%s %s",
			sshKeyPath, username, host, userHome, filepath.Join("/mnt", "bunker", agentID)), nil
	case mountdriver.DriverRclone:
		// MOUNT-007: rclone is OPT-IN and never the default — only a
		// request that names the driver lands here; DefaultDriver and the
		// sshfs arm above are untouched. The backend is an inline
		// :sftp:<path> spec so no rclone.conf or credential file exists on
		// either side, and the SFTP session is exactly the agent's existing
		// sshd identity (key, user, host) — the agent needs nothing new.
		// The command ends with --daemon so `bunker mount` returns once the
		// mountpoint is served instead of blocking on the VFS loop; the
		// client rewrites the key path, host and mountpoint for its own
		// machine the same way it rewrites the sshfs command (see
		// rewriteRcloneMount). The mountpoint follows the sshfs convention
		// (filepath.Join("/mnt","bunker",agentID)) so every driver stores
		// the same shape and the client's mountpoint substitution is
		// driver-agnostic.
		return fmt.Sprintf("rclone mount :sftp:%s %s --sftp-user %s --sftp-key-file %s --sftp-host %s --vfs-cache-mode minimal --daemon",
			userHome, filepath.Join("/mnt", "bunker", agentID), username, sshKeyPath, host), nil
	default:
		// Reachable only for a driver some future row registers without
		// teaching this builder its command shape; kept as a named guard so
		// that row must add its arm instead of silently producing an empty
		// or sshfs-shaped command.
		return "", fmt.Errorf("mount driver %q has no server-side command builder", d.Name)
	}
}
