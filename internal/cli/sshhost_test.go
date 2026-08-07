package cli

import (
	"strings"
	"testing"
)

func TestResolveSSHHost(t *testing.T) {
	tests := []struct {
		name       string
		entry      ServerEntry
		serverHost string
		flag       string
		want       string
	}{
		{
			name:       "flag wins over URL host",
			entry:      ServerEntry{URL: "http://10.0.0.5:19090"},
			serverHost: "bunker-mvp",
			flag:       "203.0.113.10",
			want:       "203.0.113.10",
		},
		{
			name:       "flag wins over server host with empty URL",
			entry:      ServerEntry{URL: ""},
			serverHost: "bunker-mvp",
			flag:       "203.0.113.10",
			want:       "203.0.113.10",
		},
		{
			name:       "URL hostname used when no flag",
			entry:      ServerEntry{URL: "http://78.46.173.180:19090"},
			serverHost: "bunker-mvp",
			flag:       "",
			want:       "78.46.173.180",
		},
		{
			name:       "URL hostname without port",
			entry:      ServerEntry{URL: "https://bunker.example.com"},
			serverHost: "bunker-mvp",
			flag:       "",
			want:       "bunker.example.com",
		},
		{
			name:       "URL hostname with TLS-insecure flag",
			entry:      ServerEntry{URL: "https://203.0.113.9:9443", TLSInsecure: true},
			serverHost: "bunker-mvp",
			flag:       "",
			want:       "203.0.113.9",
		},
		{
			name:       "ipv6 URL hostname",
			entry:      ServerEntry{URL: "http://[::1]:19090"},
			serverHost: "bunker-mvp",
			flag:       "",
			want:       "::1",
		},
		{
			name:       "server host fallback when URL empty",
			entry:      ServerEntry{URL: ""},
			serverHost: "bunker-mvp",
			flag:       "",
			want:       "bunker-mvp",
		},
		{
			name:       "server host fallback when URL has no hostname",
			entry:      ServerEntry{URL: "localhost:19090"},
			serverHost: "bunker-mvp",
			flag:       "",
			want:       "bunker-mvp",
		},
		{
			name:       "empty everything",
			entry:      ServerEntry{URL: ""},
			serverHost: "",
			flag:       "",
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveSSHHost(tt.entry, tt.serverHost, tt.flag); got != tt.want {
				t.Errorf("resolveSSHHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveUserAtHost(t *testing.T) {
	const mount = "sshfs -o IdentityFile=/etc/bunkerd/ssh/abc123 -o idmap=user -o allow_other bunker-abc123@bunker-mvp:/home/bunker-abc123 /mnt/bunker/abc123"

	tests := []struct {
		name    string
		entry   ServerEntry
		mount   string
		flag    string
		want    string
		wantErr bool
	}{
		{
			name:  "URL host replaces server host, user preserved",
			entry: ServerEntry{URL: "http://78.46.173.180:19090"},
			mount: mount,
			want:  "bunker-abc123@78.46.173.180",
		},
		{
			name:  "flag overrides URL host",
			entry: ServerEntry{URL: "http://78.46.173.180:19090"},
			mount: mount,
			flag:  "203.0.113.10",
			want:  "bunker-abc123@203.0.113.10",
		},
		{
			name:  "server host kept when URL empty",
			entry: ServerEntry{URL: ""},
			mount: mount,
			want:  "bunker-abc123@bunker-mvp",
		},
		{
			name:    "malformed mount errors",
			entry:   ServerEntry{URL: "http://78.46.173.180:19090"},
			mount:   "sshfs bunker-abc /mnt",
			wantErr: true,
		},
		{
			name:    "empty mount errors",
			entry:   ServerEntry{URL: "http://78.46.173.180:19090"},
			mount:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveUserAtHost(tt.entry, tt.mount, tt.flag)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveUserAtHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSSHHostFromMount(t *testing.T) {
	tests := []struct {
		name  string
		mount string
		want  string
	}{
		{
			name:  "standard mount",
			mount: "sshfs -o IdentityFile=/etc/bunkerd/ssh/abc -o idmap=user -o allow_other bunker-abc@bunker-mvp:/home/bunker-abc /mnt/bunker/abc",
			want:  "bunker-mvp",
		},
		{
			name:  "mount without IdentityFile",
			mount: "sshfs bunker-abc@bunker-host:/home/bunker-abc /mnt/bunker/abc",
			want:  "bunker-host",
		},
		{
			name:  "malformed mount",
			mount: "sshfs bunker-abc /mnt",
			want:  "",
		},
		{
			name:  "empty mount",
			mount: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sshHostFromMount(tt.mount); got != tt.want {
				t.Errorf("sshHostFromMount() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSSHUserHostFromTunnel(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		wantUser string
		wantHost string
		wantOK   bool
	}{
		{
			name:     "standard tunnel command",
			cmd:      "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc -L 2376:/run/bunker/abc/docker.sock bunker-abc@bunker-mvp -N",
			wantUser: "bunker-abc",
			wantHost: "bunker-mvp",
			wantOK:   true,
		},
		{
			name:     "tunnel command with -L spec containing bunker path",
			cmd:      "ssh -L 2376:/run/bunker/abc/docker.sock bunker-abc@host -N",
			wantUser: "bunker-abc",
			wantHost: "host",
			wantOK:   true,
		},
		{
			name:   "no target",
			cmd:    "ssh -L 2376:/run/bunker/abc/docker.sock -N",
			wantOK: false,
		},
		{
			name:   "empty command",
			cmd:    "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user, host, ok := sshUserHostFromTunnel(tt.cmd)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if user != tt.wantUser || host != tt.wantHost {
				t.Errorf("got %q@%q, want %q@%q", user, host, tt.wantUser, tt.wantHost)
			}
		})
	}
}

func TestClientTunnelArgs(t *testing.T) {
	const stored = "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc -L 2376:/run/bunker/abc/docker.sock bunker-abc@bunker-mvp -N"

	tests := []struct {
		name         string
		cmd          string
		resolvedHost string
		clientKey    string
		wantParts    []string
		wantAbsent   []string
	}{
		{
			name:         "key path and host swapped",
			cmd:          stored,
			resolvedHost: "78.46.173.180",
			clientKey:    "/home/kara/.bunker/keys/abc",
			wantParts: []string{
				"ssh", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=no",
				"-i", "/home/kara/.bunker/keys/abc",
				"-L", "2376:/run/bunker/abc/docker.sock",
				"bunker-abc@78.46.173.180", "-N",
			},
			wantAbsent: []string{"/etc/bunkerd/ssh/abc", "bunker-mvp"},
		},
		{
			name:         "host unchanged when resolved equals server host, key still swapped",
			cmd:          stored,
			resolvedHost: "bunker-mvp",
			clientKey:    "/home/kara/.bunker/keys/abc",
			wantParts: []string{
				"ssh", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=no",
				"-i", "/home/kara/.bunker/keys/abc",
				"-L", "2376:/run/bunker/abc/docker.sock",
				"bunker-abc@bunker-mvp", "-N",
			},
			wantAbsent: []string{"/etc/bunkerd/ssh/abc"},
		},
		{
			name:         "command without -i keeps everything else, host swapped",
			cmd:          "ssh -L 2376:/run/bunker/abc/docker.sock bunker-abc@bunker-mvp -N",
			resolvedHost: "203.0.113.10",
			clientKey:    "/home/kara/.bunker/keys/abc",
			wantParts: []string{
				"ssh", "-o", "IdentitiesOnly=yes",
				"-L", "2376:/run/bunker/abc/docker.sock",
				"bunker-abc@203.0.113.10", "-N",
			},
		},
		{
			name:         "already has IdentitiesOnly=yes, not duplicated",
			cmd:          "ssh -o IdentitiesOnly=yes -i /etc/bunkerd/ssh/abc -L 2376:/run/bunker/abc/docker.sock bunker-abc@bunker-mvp -N",
			resolvedHost: "78.46.173.180",
			clientKey:    "/home/kara/.bunker/keys/abc",
			wantParts: []string{
				"ssh", "-o", "IdentitiesOnly=yes",
				"-i", "/home/kara/.bunker/keys/abc",
				"-L", "2376:/run/bunker/abc/docker.sock",
				"bunker-abc@78.46.173.180", "-N",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clientTunnelArgs(tt.cmd, tt.resolvedHost, tt.clientKey)
			if len(got) != len(tt.wantParts) {
				t.Fatalf("got %d parts %q, want %d parts %q", len(got), got, len(tt.wantParts), tt.wantParts)
			}
			for i := range tt.wantParts {
				if got[i] != tt.wantParts[i] {
					t.Errorf("part %d = %q, want %q (full: %q)", i, got[i], tt.wantParts[i], got)
				}
			}
			joined := strings.Join(got, " ")
			for _, absent := range tt.wantAbsent {
				if strings.Contains(joined, absent) {
					t.Errorf("args still contain %q: %q", absent, joined)
				}
			}
		})
	}
}

func TestRewriteTunnelCommand(t *testing.T) {
	const stored = "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc -L 2376:/run/bunker/abc/docker.sock bunker-abc@bunker-mvp -N"

	tests := []struct {
		name         string
		cmd          string
		serverHost   string
		resolvedHost string
		clientKey    string
		want         string
	}{
		{
			name:         "rewritten for remote client",
			cmd:          stored,
			serverHost:   "bunker-mvp",
			resolvedHost: "78.46.173.180",
			clientKey:    "/home/kara/.bunker/keys/abc",
			want:         "ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -i /home/kara/.bunker/keys/abc -L 2376:/run/bunker/abc/docker.sock bunker-abc@78.46.173.180 -N",
		},
		{
			name:         "unchanged when resolved equals server host",
			cmd:          stored,
			serverHost:   "bunker-mvp",
			resolvedHost: "bunker-mvp",
			clientKey:    "/home/kara/.bunker/keys/abc",
			want:         stored,
		},
		{
			name:         "empty command stays empty",
			cmd:          "",
			serverHost:   "bunker-mvp",
			resolvedHost: "78.46.173.180",
			clientKey:    "/home/kara/.bunker/keys/abc",
			want:         "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rewriteTunnelCommand(tt.cmd, tt.serverHost, tt.resolvedHost, tt.clientKey); got != tt.want {
				t.Errorf("rewriteTunnelCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRewriteSSHFSMount(t *testing.T) {
	const stored = "sshfs -o IdentityFile=/etc/bunkerd/ssh/abc -o idmap=user -o allow_other bunker-abc@bunker-mvp:/home/bunker-abc /mnt/bunker/abc"

	tests := []struct {
		name         string
		mount        string
		serverHost   string
		resolvedHost string
		clientKey    string
		want         string
	}{
		{
			name:         "key path and host swapped",
			mount:        stored,
			serverHost:   "bunker-mvp",
			resolvedHost: "78.46.173.180",
			clientKey:    "/home/kara/.bunker/keys/abc",
			want:         "sshfs -o IdentityFile=/home/kara/.bunker/keys/abc -o idmap=user -o allow_other bunker-abc@78.46.173.180:/home/bunker-abc /mnt/bunker/abc",
		},
		{
			name:         "unchanged when resolved equals server host",
			mount:        stored,
			serverHost:   "bunker-mvp",
			resolvedHost: "bunker-mvp",
			clientKey:    "/home/kara/.bunker/keys/abc",
			want:         stored,
		},
		{
			name:         "mount without IdentityFile: host swapped only",
			mount:        "sshfs bunker-abc@bunker-mvp:/home/bunker-abc /mnt/bunker/abc",
			serverHost:   "bunker-mvp",
			resolvedHost: "78.46.173.180",
			clientKey:    "/home/kara/.bunker/keys/abc",
			want:         "sshfs bunker-abc@78.46.173.180:/home/bunker-abc /mnt/bunker/abc",
		},
		{
			name:         "empty clientKey: host swapped only",
			mount:        stored,
			serverHost:   "bunker-mvp",
			resolvedHost: "78.46.173.180",
			clientKey:    "",
			want:         "sshfs -o IdentityFile=/etc/bunkerd/ssh/abc -o idmap=user -o allow_other bunker-abc@78.46.173.180:/home/bunker-abc /mnt/bunker/abc",
		},
		{
			name:         "empty mount stays empty",
			mount:        "",
			serverHost:   "bunker-mvp",
			resolvedHost: "78.46.173.180",
			clientKey:    "/home/kara/.bunker/keys/abc",
			want:         "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rewriteSSHFSMount(tt.mount, tt.serverHost, tt.resolvedHost, tt.clientKey); got != tt.want {
				t.Errorf("rewriteSSHFSMount() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRewriteDockerHostSsh(t *testing.T) {
	tests := []struct {
		name         string
		s            string
		serverHost   string
		resolvedHost string
		want         string
	}{
		{
			name:         "host swapped",
			s:            "DOCKER_HOST=ssh://bunker-abc@bunker-mvp",
			serverHost:   "bunker-mvp",
			resolvedHost: "78.46.173.180",
			want:         "DOCKER_HOST=ssh://bunker-abc@78.46.173.180",
		},
		{
			name:         "unchanged when resolved equals server host",
			s:            "DOCKER_HOST=ssh://bunker-abc@bunker-mvp",
			serverHost:   "bunker-mvp",
			resolvedHost: "bunker-mvp",
			want:         "DOCKER_HOST=ssh://bunker-abc@bunker-mvp",
		},
		{
			name:         "empty stays empty",
			s:            "",
			serverHost:   "bunker-mvp",
			resolvedHost: "78.46.173.180",
			want:         "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rewriteDockerHostSsh(tt.s, tt.serverHost, tt.resolvedHost); got != tt.want {
				t.Errorf("rewriteDockerHostSsh() = %q, want %q", got, tt.want)
			}
		})
	}
}
