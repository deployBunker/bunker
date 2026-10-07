package cli

// MOUNT-012: ControlMaster multiplexing on the CLI's own ssh paths.
// The sshfs mount preflight and the docker-host tunnel forwarding must NOT
// multiplex (sshfs owns a long-lived connection; tunnel forwards must not
// share a master) — asserted below so a future refactor cannot leak it in.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// findOption returns the value of `-o <key>=<value>` (or the bare `-o <val>`
// form) from an argv slice, or "" when the option is absent.
func findOption(args []string, key string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-o" && strings.HasPrefix(args[i+1], key+"=") {
			return strings.TrimPrefix(args[i+1], key+"=")
		}
	}
	return ""
}

// findMuxControlPath derives the expected ControlPath for an identity via the
// real helper (creating the mux dir under the test's isolated HOME).
func findMuxControlPath(t *testing.T, keyPath string, port uint32, userAtHost string) string {
	t.Helper()
	p, err := muxControlPath(keyPath, port, userAtHost)
	if err != nil {
		t.Fatalf("muxControlPath: %v", err)
	}
	return p
}

func TestSSHArgsIncludeControlMasterOptions(t *testing.T) {
	// MOUNT-012: mux options create ~/.bunker/mux — isolate HOME first.
	t.Setenv("HOME", t.TempDir())
	// No BUNKER_HOME etc. needed for the option-presence assertions; the HOME
	// relocation matters only in the distinct-ControlPath tests.
	const keyPath = "/home/u/.bunker/keys/abc123"
	args := buildSSHArgs(keyPath, 22, "bunker-abc123@10.0.0.5", nil)
	if got := findOption(args, "ControlMaster"); got != "auto" {
		t.Errorf("buildSSHArgs ControlMaster = %q, want auto; args=%v", got, args)
	}
	if got := findOption(args, "ControlPath"); got == "" {
		t.Errorf("buildSSHArgs missing ControlPath; args=%v", args)
	}
	if got := findOption(args, "ControlPersist"); got != "10m" {
		t.Errorf("buildSSHArgs ControlPersist = %q, want 10m; args=%v", got, args)
	}
	// Options must precede user@host (ssh CLI grammar) and the remote command
	// position is untouched.
	idx := -1
	for i, a := range args {
		if a == "bunker-abc123@10.0.0.5" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("userAtHost missing from args: %v", args)
	}
	if !strings.Contains(strings.Join(args[:idx], " "), "ControlMaster=auto") {
		t.Errorf("multiplex options must precede user@host: %v", args)
	}
}

func TestMultiplexOptionsAcrossConnectionBuilders(t *testing.T) {
	// MOUNT-012: mux options create ~/.bunker/mux — isolate HOME first.
	t.Setenv("HOME", t.TempDir())
	const (
		keyPath    = "/home/u/.bunker/keys/abc123"
		userAtHost = "bunker-abc123@10.0.0.5"
	)
	builders := map[string]func() []string{
		"buildSSHArgs":      func() []string { return buildSSHArgs(keyPath, 22, userAtHost, nil) },
		"buildSCPArgs":      func() []string { return buildSCPArgs(keyPath, 22, "/tmp/a", userAtHost, "/tmp/b", false) },
		"buildSSHProbeArgs": func() []string { return buildSSHProbeArgs(keyPath, 22, userAtHost, "/tmp/b") },
	}
	for name, build := range builders {
		args := build()
		if findOption(args, "ControlMaster") != "auto" {
			t.Errorf("%s: ControlMaster=auto missing: %v", name, args)
		}
		if findOption(args, "ControlPath") == "" {
			t.Errorf("%s: ControlPath missing: %v", name, args)
		}
		if findOption(args, "ControlPersist") != "10m" {
			t.Errorf("%s: ControlPersist=10m missing: %v", name, args)
		}
	}
}

// TestControlPathDiffersPerIdentity verifies the ControlPath is deterministic
// per (host, port, key) and distinct agents never share a socket. Uses
// t.Setenv("HOME") so the mux dir lands in a temp tree.
func TestControlPathDiffersPerIdentity(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	paths := map[string]string{}
	identities := []struct {
		key        string
		port       uint32
		userAtHost string
	}{
		{"a", 22, "bunker-a@10.0.0.5"},
		{"b", 22, "bunker-b@10.0.0.6"},
		{"a", 2222, "bunker-a@10.0.0.5"}, // same host/key, different port
		{"b", 22, "bunker-a@10.0.0.5"},   // same host/port, different key
	}
	for _, id := range identities {
		got := findOption(buildSSHArgs("/home/u/.bunker/keys/"+id.key, id.port, id.userAtHost, nil), "ControlPath")
		if got == "" {
			t.Fatalf("no ControlPath for %+v", id)
		}
		if prev, dup := paths[got]; dup {
			t.Errorf("ControlPath collision between %+v and %+v: %s", id, prev, got)
		}
		paths[got] = fmt.Sprintf("%+v", id)
		// Deterministic: rebuilding yields the same path.
		if again := findOption(buildSSHArgs("/home/u/.bunker/keys/"+id.key, id.port, id.userAtHost, nil), "ControlPath"); again != got {
			t.Errorf("ControlPath not deterministic for %+v: %q vs %q", id, got, again)
		}
		if !strings.HasPrefix(got, tmp+"/.bunker/mux/ctrl-") {
			t.Errorf("ControlPath %q not under %s/.bunker/mux/ctrl-*", got, tmp)
		}
	}
}

// TestMuxControlDirCreated0700 checks the parent dir is created with 0700.
func TestMuxControlDirCreated0700(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	_, err := muxControlPath("/k", 22, "u@h")
	if err != nil {
		t.Fatalf("muxControlPath: %v", err)
	}
	fi, err := os.Stat(filepath.Join(tmp, ".bunker", "mux"))
	if err != nil {
		t.Fatalf("stat mux dir: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("mux dir mode = %o, want 700", perm)
	}
}

// TestMultiplexDoesNotLeakIntoTunnelArgs pins the sshhost tunnel builder as
// multiplex-free (clientTunnelArgs rewrites a stored docker-host forward).
func TestMultiplexDoesNotLeakIntoTunnelArgs(t *testing.T) {
	args := clientTunnelArgs("ssh -N -L 8443:localhost:8443 bunker-abc@bunker-agent-host", "10.0.0.5", "/k")
	if findOption(args, "ControlMaster") != "" {
		t.Errorf("clientTunnelArgs must not multiplex: %v", args)
	}
}

// TestMultiplexDoesNotLeakIntoMountPreflight pins the sshfs preflight ssh
// builder as multiplex-free (the row NOTE: sshfs mounts its own long-lived
// connection; a shared master breaks it). It reads the source file itself so
// a future ControlMaster leak there fails this test.
func TestMultiplexDoesNotLeakIntoMountPreflight(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("mount_preflight.go"))
	if err != nil {
		t.Skipf("mount_preflight.go not readable: %v", err)
	}
	if strings.Contains(string(src), "ControlMaster") {
		t.Errorf("mount_preflight ssh invocation must not multiplex (sshfs owns its own long-lived connection)")
	}
}
