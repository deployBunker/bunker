package cli

// rclone driver client support (MOUNT-007).
//
// The rclone driver is OPT-IN: an agent spawned with --mount-driver rclone
// carries a MountSpec{driver: "rclone", command: "rclone mount :sftp:<home>
// <mountpoint> --sftp-user ... --sftp-key-file ... --sftp-host ...
// --vfs-cache-mode minimal --daemon"} produced by the server
// (internal/agent/mount_driver.go). This file gives that command the same
// client-side rewrite the sshfs command gets in mount.go: the daemon-local
// key path, daemon hostname and default mountpoint are replaced with this
// client's key, resolved host and mountpoint. The sshfs command's rewrite
// (rewriteSSHFSMount) is untouched and byte-identical — these helpers are a
// separate arm, dispatched by driver name at the single dispatch site in
// mount.go.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// rcloneKeyFileFlag names the flag whose value is the server-local key path
// in the stored rclone command. rcloneHostFlag names the flag whose value is
// the daemon hostname. Both are fixed points of the server-side builder, so
// a stored command missing either is a shape this client does not understand.
const (
	rcloneKeyFileFlag = "--sftp-key-file"
	rcloneHostFlag    = "--sftp-host"
)

// rewriteRcloneMount rewrites the server-generated rclone mount command for
// this client (client-local key + resolved host) and returns the argv to
// exec. It refuses loudly rather than guessing: a command that is not an
// rclone mount command, or one missing an anchor whose value we would have
// to rewrite (key file, host), is an error naming the cause — never a
// best-effort exec of a half-rewritten command against the daemon's own
// paths.
//
// The mountpoint argument (last field) is replaced by mountPoint, mirroring
// the sshfs arm's substitution of the stored default (mount.go step 5).
func rewriteRcloneMount(mountCmd, resolvedHost, clientKey, mountPoint string) ([]string, error) {
	parts := strings.Fields(mountCmd)
	if len(parts) < 2 || parts[0] != "rclone" || parts[1] != "mount" {
		return nil, fmt.Errorf("stored rclone mount command does not start with 'rclone mount': %q", mountCmd)
	}

	// Replace the daemon-local key path. Guessing would exec --sftp-key-file
	// with the daemon's unreadable path; refusing names the actual shape.
	foundKey := false
	for i := 0; i < len(parts); i++ {
		if parts[i] == rcloneKeyFileFlag && i+1 < len(parts) {
			parts[i+1] = clientKey
			foundKey = true
			break
		}
	}
	if !foundKey {
		return nil, fmt.Errorf("stored rclone mount command carries no %s value to rewrite: %q", rcloneKeyFileFlag, mountCmd)
	}

	// Replace the daemon hostname with the address this client actually
	// reaches the daemon with (the same resolveSSHHost rule the sshfs arm
	// uses: flag > server-entry URL hostname > embedded value). When the
	// resolved host equals the stored one (server-local use) this is a
	// no-op.
	foundHost := false
	if resolvedHost != "" {
		for i := 0; i < len(parts); i++ {
			if parts[i] == rcloneHostFlag && i+1 < len(parts) {
				parts[i+1] = resolvedHost
				foundHost = true
				break
			}
		}
	}
	if !foundHost {
		return nil, fmt.Errorf("stored rclone mount command carries no %s value to rewrite: %q", rcloneHostFlag, mountCmd)
	}

	// The server-side builder pins --daemon as the LAST field (it is the
	// non-blocking guarantee: without it `bunker mount` would hold the CLI
	// open on rclone's VFS loop). Refuse any other tail shape instead of
	// execing a command that would block.
	if parts[len(parts)-1] != "--daemon" {
		return nil, fmt.Errorf("stored rclone mount command must end with --daemon (the non-blocking contract): %q", mountCmd)
	}

	// Mountpoint: the positional argument immediately AFTER the :sftp:
	// backend token (rclone mount <remote> <mountpoint> [flags] — the
	// brief's stored shape). Replace that positional, refusing any shape
	// that does not have one there.
	backendIdx := -1
	for i := 0; i < len(parts); i++ {
		if strings.HasPrefix(parts[i], ":sftp:") {
			backendIdx = i
			break
		}
	}
	if backendIdx < 0 {
		return nil, fmt.Errorf("stored rclone mount command carries no :sftp: backend token: %q", mountCmd)
	}
	if backendIdx+1 >= len(parts) || strings.HasPrefix(parts[backendIdx+1], "--") {
		return nil, fmt.Errorf("stored rclone mount command has no mountpoint positional after the :sftp: backend: %q", mountCmd)
	}
	parts[backendIdx+1] = mountPoint
	return parts, nil
}

// runRcloneMountCommand is the rclone driver's client mount path, dispatched
// from mount.go BEFORE the sshfs-shaped steps (user@host parsing, sshfs
// version guard, -o option surgery, classifier retry loop). The shared steps
// — server binding, server-namespaced mountpoint resolution, MountSpec
// dispatch with the loud unknown-driver refusal — already ran in the caller.
//
// The stored command is rewritten for this client and exec'd ONCE: no retry
// loop applies because the rclone driver declares NoClassifier (the
// mountdriver registry invariant) — a failure surfaces rclone's own
// diagnostics and asks the operator to resolve and re-run, instead of an
// sshfs-shaped classification guessing at it. --daemon makes rclone return
// once the mountpoint is served, so this does not hold the CLI open.
func runRcloneMountCommand(ctx context.Context, mountCmd string, entry ServerEntry, agentID, mountPoint, sshKey string) error {
	keyPath := sshKey
	if keyPath == "" {
		var err error
		keyPath, err = defaultSSHKeyPath(agentID)
		if err != nil {
			return fmt.Errorf("resolve SSH key: %w", err)
		}
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		return fmt.Errorf("SSH key not found at %q — spawn the agent first or use --ssh-key", keyPath)
	}

	resolvedHost := resolveSSHHost(entry, sshHostFromMount(mountCmd), "")
	parts, err := rewriteRcloneMount(mountCmd, resolvedHost, keyPath, mountPoint)
	if err != nil {
		return err
	}

	// The rclone arm owns its own mountpoint creation (the sshfs path
	// creates it at mount.go step 4, before the rewrite): private 0700,
	// same policy as the sshfs arm.
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		return fmt.Errorf("create mount point %s: %w", mountPoint, err)
	}

	var combined bytes.Buffer
	if err := rcloneRun(ctx, parts[0], parts[1:], &combined, &combined); err != nil {
		return fmt.Errorf("rclone mount failed — the rclone driver declares no failure classifier, resolve the cause and re-run: %w (last output: %s)", err, trimSSHFSOutput(combined.String()))
	}

	fmt.Printf("Mounted %s at %s (driver: rclone)\n", agentID, mountPoint)
	if ok, werr := WriteMountMarker(mountPoint); !ok {
		fmt.Fprintf(os.Stderr, "bunker: WARNING: could not write the do-not-build marker at %s (%v) — a local build inside this mount will NOT be refused\n", mountPoint, werr)
	}
	fmt.Printf("Unmount with: bunker umount %s\n", agentID)
	return nil
}

// rcloneRun runs the rclone binary once through the shared long-lived-child
// contract (own process group, group-wide teardown — GAP-084), streaming its
// output to the terminal exactly like sshfsRun while also forwarding it to
// the caller-provided capture writers. Seam (package var) so tests can stub
// the exec.
var rcloneRun = func(ctx context.Context, path string, args []string, stdout, stderr io.Writer) error {
	cmd := newLongLivedCommand(ctx, path, args...)
	cmd.Stdout = io.MultiWriter(stdout, os.Stdout)
	cmd.Stderr = io.MultiWriter(stderr, os.Stderr)
	return runDetachedChildCommand(cmd)
}
