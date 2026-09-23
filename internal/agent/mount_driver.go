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
	default:
		// Unreachable while only sshfs is registered (Resolve refuses
		// everything else), but kept as a named guard so a future driver
		// row must teach this builder its command shape instead of
		// silently producing an empty or sshfs-shaped command.
		return "", fmt.Errorf("mount driver %q has no server-side command builder", d.Name)
	}
}
