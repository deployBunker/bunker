package cli

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// This file holds the client-side SSH host/key resolution helpers shared by
// `bunker cp`, `bunker deploy`, `bunker tunnel`, `bunker spawn` and
// `bunker info`.
//
// The server bakes its SELF-REPORTED hostname (e.g. "bunker-mvp") and a
// server-local key path (/etc/bunkerd/ssh/<id>) into the sshfs mount and
// docker-host tunnel command strings it returns. That hostname does not
// resolve from remote client machines, and the key path is not readable by
// them. These helpers rewrite those strings for the client: the host is
// resolved from the server entry URL (the address the client actually used
// to reach bunkerd, therefore reachable) with an optional --ssh-host flag
// override, and the key path is replaced with the client-local key saved at
// spawn time (~/.bunker/keys/<id>).

// sshTunnelTargetRE matches the "user@host" login target inside an ssh
// command line, e.g. "bunker-abc123@bunker-mvp" in:
//
//	ssh -o ... -i /etc/bunkerd/ssh/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@bunker-mvp -N
var sshTunnelTargetRE = regexp.MustCompile(`\b(bunker-\S+)@(\S+)`)

// sshfsHostRE matches the "user@host:" segment inside an sshfs mount command,
// e.g. "bunker-abc123@bunker-mvp:" in:
//
//	sshfs -o IdentityFile=/etc/bunkerd/ssh/abc123 ... bunker-abc123@bunker-mvp:/home/bunker-abc123 /mnt/bunker/abc123
var sshfsHostRE = regexp.MustCompile(`(\bbunker-\S+@)[^\s:]+(:)`)

// identityFileRE matches an IdentityFile= option value so server-local key
// paths can be replaced with client-local ones.
var identityFileRE = regexp.MustCompile(`IdentityFile=\S+`)

// dockerHostSshRE matches the host inside a DOCKER_HOST= ssh URL, e.g.
// "DOCKER_HOST=ssh://bunker-abc123@bunker-mvp".
var dockerHostSshRE = regexp.MustCompile(`(ssh://\S+@)[^\s/]+`)

// resolveSSHHost returns the SSH host a client should use to reach an agent's
// host machine, in precedence order:
//
//  1. the --ssh-host flag value, if set;
//  2. the hostname of the server entry URL — the address the client actually
//     used to reach bunkerd, so it is reachable from the client;
//  3. the server-provided hostname (embedded in the sshfs/tunnel command) as
//     a fallback for setups where the API URL is not the SSH address.
func resolveSSHHost(entry ServerEntry, serverProvidedHost, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if entry.URL != "" {
		if u, err := url.Parse(entry.URL); err == nil {
			if host := u.Hostname(); host != "" {
				return host
			}
		}
	}
	return serverProvidedHost
}

// resolveUserAtHost builds the "user@host" SSH target for an agent: the user
// is taken from the sshfs mount command returned by the server, and the host
// is resolved via resolveSSHHost (flag > server URL hostname > server value).
func resolveUserAtHost(entry ServerEntry, sshfsMount, flagHost string) (string, error) {
	userAtHost, err := parseSSHUserHost(sshfsMount)
	if err != nil {
		return "", err
	}
	user, serverHost, ok := strings.Cut(userAtHost, "@")
	if !ok {
		return "", fmt.Errorf("cannot parse user@host from %q", userAtHost)
	}
	return user + "@" + resolveSSHHost(entry, serverHost, flagHost), nil
}

// sshHostFromMount returns the host part of the "user@host" token embedded in
// an sshfs mount command, or "" when it cannot be parsed.
func sshHostFromMount(sshfsMount string) string {
	userAtHost, err := parseSSHUserHost(sshfsMount)
	if err != nil {
		return ""
	}
	_, host, _ := strings.Cut(userAtHost, "@")
	return host
}

// sshUserHostFromTunnel returns the user and host of the login target embedded
// in a docker-host tunnel command.
func sshUserHostFromTunnel(tunnelCmd string) (user, host string, ok bool) {
	m := sshTunnelTargetRE.FindStringSubmatch(tunnelCmd)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// clientTunnelArgs rewrites a stored docker-host tunnel command into the
// argument list to execute on the client machine. The server-local -i key
// path is always replaced with clientKey (the server path is not readable
// from a remote client) and the login target host with resolvedHost (a no-op
// when they are already equal). The -L forward spec and all other arguments
// are preserved.
func clientTunnelArgs(tunnelCmd, resolvedHost, clientKey string) []string {
	parts := strings.Fields(tunnelCmd)
	for i := 0; i < len(parts); i++ {
		switch {
		case parts[i] == "-i" && i+1 < len(parts):
			parts[i+1] = clientKey
		case strings.HasPrefix(parts[i], "bunker-") && strings.Contains(parts[i], "@") && !strings.Contains(parts[i], ":"):
			user, _, _ := strings.Cut(parts[i], "@")
			parts[i] = user + "@" + resolvedHost
		}
	}
	return parts
}

// rewriteTunnelCommand rewrites a server-generated docker-host tunnel command
// for display so it is copy-paste runnable from the client machine: the
// server-local -i key path is replaced with clientKey and the login target
// host with resolvedHost. When resolvedHost equals serverHost (server-local
// use) the original command is returned unchanged.
func rewriteTunnelCommand(tunnelCmd, serverHost, resolvedHost, clientKey string) string {
	if tunnelCmd == "" || resolvedHost == serverHost {
		return tunnelCmd
	}
	return strings.Join(clientTunnelArgs(tunnelCmd, resolvedHost, clientKey), " ")
}

// rewriteSSHFSMount rewrites a server-generated sshfs mount command so it is
// copy-paste runnable from the client machine: the server-local IdentityFile
// path is replaced with clientKey and the server-provided host with
// resolvedHost. When resolvedHost equals serverHost (server-local use) the
// original command is returned unchanged.
func rewriteSSHFSMount(mount, serverHost, resolvedHost, clientKey string) string {
	if mount == "" || resolvedHost == serverHost {
		return mount
	}
	out := mount
	if clientKey != "" {
		out = identityFileRE.ReplaceAllString(out, "IdentityFile="+clientKey)
	}
	return sshfsHostRE.ReplaceAllString(out, "${1}"+resolvedHost+"${2}")
}

// rewriteDockerHostSsh rewrites a "DOCKER_HOST=ssh://user@host" string for
// display so the host is the client-resolved one. When resolvedHost equals
// serverHost (server-local use) the original string is returned unchanged.
func rewriteDockerHostSsh(s, serverHost, resolvedHost string) string {
	if s == "" || resolvedHost == serverHost {
		return s
	}
	return dockerHostSshRE.ReplaceAllString(s, "${1}"+resolvedHost)
}
