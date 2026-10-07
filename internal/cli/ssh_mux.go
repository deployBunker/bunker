package cli

// SSH connection multiplexing (MOUNT-012): every CLI-spawned ssh/scp
// connection pays a full SSH handshake (~1.6s on Tailscale-relayed hosts).
// ControlMaster=auto with a deterministic per-(user,host,port,key) ControlPath
// lets the first connection become the master and every later one reuse it.
//
// Scope: the CLI's own connection builders only. The sshfs mount command and
// the docker-host tunnel forwarding (clientTunnelArgs) deliberately do NOT
// multiplex: sshfs owns a long-lived connection of its own and a tunnel
// forward must not share a master with unrelated sessions.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// sshMuxControlDir returns the directory holding the ControlMaster sockets.
// It follows the CLI state-dir convention (paths.go): $BUNKER_HOME/mux when
// BUNKER_HOME is set, else ~/.bunker/mux. Respecting BUNKER_HOME matters for
// tests and sandboxed runs: a child CLI with BUNKER_HOME pinned must never
// write its mux sockets into the ambient HOME (the qabunker23 tripwire
// treats any .bunker write under an un-isolated HOME as a leak).
func sshMuxControlDir() (string, error) {
	if bh := os.Getenv("BUNKER_HOME"); bh != "" {
		return filepath.Join(bh, "mux"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home for ssh mux control dir: %w", err)
	}
	return filepath.Join(home, ".bunker", "mux"), nil
}

// muxControlPath derives the ControlPath for one connection identity and
// ensures the parent directory exists (0700). The path is a truncated sha256
// of "user|host|port|keypath", so two different agents (or two keys for the
// same host) never share a master socket.
func muxControlPath(keyPath string, port uint32, userAtHost string) (string, error) {
	user, host, _ := splitUserHost(userAtHost)
	sum := sha256.Sum256([]byte(user + "|" + host + "|" + fmt.Sprintf("%d", port) + "|" + keyPath))
	dir, err := sshMuxControlDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create ssh mux control dir %s: %w", dir, err)
	}
	return filepath.Join(dir, "ctrl-"+hex.EncodeToString(sum[:])[:16]), nil
}

// sshMultiplexArgs emits the ControlMaster option triple for one connection
// identity, creating the control-socket directory first. On a directory
// creation failure it degrades to no multiplexing (the connection still
// works, just slower) rather than failing the whole operation.
func sshMultiplexArgs(keyPath string, port uint32, userAtHost string) []string {
	controlPath, err := muxControlPath(keyPath, port, userAtHost)
	if err != nil {
		return nil
	}
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + controlPath,
		"-o", "ControlPersist=10m",
	}
}

// splitUserHost splits "user@host" into its parts; a missing user yields "".
func splitUserHost(userAtHost string) (string, string, bool) {
	for i := 0; i < len(userAtHost); i++ {
		if userAtHost[i] == '@' {
			return userAtHost[:i], userAtHost[i+1:], true
		}
	}
	return "", userAtHost, false
}
