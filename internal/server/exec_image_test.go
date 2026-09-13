package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// GAP-069: exec runs inside the agent's image-spec container when — and only
// when — the agent record carries an image ref.
const (
	imgTestAgentID = "abc123"
	imgTestKeyPath = "/keys/abc123"
	imgTestHome    = "/home/bunker-abc123"
	imgTestRef     = "bunkerd-imagespec-0123456789ab:latest"
)

// Pre-GAP-069 goldens: the EXACT remote command / argv these builders produced
// before the image-container path existed (captured from the pre-change tree).
// They turn "the non-image path is unchanged" into a byte-level assertion
// instead of a promise. Regenerate only for a deliberate change to the
// host-context command shape.
var (
	goldenShellOff = "sh -c 'set -a; [ -f /run/bunker/abc123/env ] && . /run/bunker/abc123/env 2>/dev/null; set +a; env PATH=/home/bunker-abc123/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin DOCKER_HOST=unix:///run/bunker/abc123/docker.sock TMPDIR=/tmp sh -c '\\''docker '\\''\\'\\'''\\''version'\\''\\'\\'''\\'''\\'''"
	goldenShellOn  = "sh -c 'set -a; [ -f /run/bunker/abc123/env ] && . /run/bunker/abc123/env 2>/dev/null; set +a; env PATH=/home/bunker-abc123/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin DOCKER_HOST=unix:///run/bunker/abc123/docker.sock TMPDIR=/tmp BUNKER_SANDBOX=1 sh -c '\\''sh '\\''\\'\\'''\\''-c'\\''\\'\\'''\\'' '\\''\\'\\'''\\''echo SB=$BUNKER_SANDBOX'\\''\\'\\'''\\'''\\'''"
	goldenRawOff   = []string{
		"ssh",
		"-o",
		"StrictHostKeyChecking=no",
		"-o",
		"UserKnownHostsFile=/dev/null",
		"-o",
		"LogLevel=ERROR",
		"-o",
		"ConnectTimeout=10",
		"-i",
		"/keys/abc123",
		"bunker-abc123@localhost",
		"env",
		"PATH=/home/bunker-abc123/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"DOCKER_HOST=unix:///run/bunker/abc123/docker.sock",
		"TMPDIR=/tmp",
		"echo",
		"it's",
		"a b",
	}
	goldenRawOn = []string{
		"ssh",
		"-o",
		"StrictHostKeyChecking=no",
		"-o",
		"UserKnownHostsFile=/dev/null",
		"-o",
		"LogLevel=ERROR",
		"-o",
		"ConnectTimeout=10",
		"-i",
		"/keys/abc123",
		"bunker-abc123@localhost",
		"env",
		"PATH=/home/bunker-abc123/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"DOCKER_HOST=unix:///run/bunker/abc123/docker.sock",
		"TMPDIR=/tmp",
		"BUNKER_SANDBOX=1",
		"echo",
		"hi",
	}
	goldenScriptOff = "sh -c 'mkdir -p \"/home/bunker-abc123/.bunker\" && cat > \"/home/bunker-abc123/.bunker/exec-script.sh\" <<'\\''EOFSCRIPT'\\''\n#!/bin/sh\necho hi\n\nEOFSCRIPT\nchmod +x \"/home/bunker-abc123/.bunker/exec-script.sh\" && set -a; [ -f /run/bunker/abc123/env ] && . /run/bunker/abc123/env 2>/dev/null; set +a; env PATH=/home/bunker-abc123/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin DOCKER_HOST=unix:///run/bunker/abc123/docker.sock TMPDIR=/tmp \"/home/bunker-abc123/.bunker/exec-script.sh\"'"
	goldenScriptOn  = "sh -c 'mkdir -p \"/home/bunker-abc123/.bunker\" && cat > \"/home/bunker-abc123/.bunker/exec-script.sh\" <<'\\''EOFSCRIPT'\\''\n#!/bin/sh\necho hi\n\nEOFSCRIPT\nchmod +x \"/home/bunker-abc123/.bunker/exec-script.sh\" && set -a; [ -f /run/bunker/abc123/env ] && . /run/bunker/abc123/env 2>/dev/null; set +a; env PATH=/home/bunker-abc123/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin DOCKER_HOST=unix:///run/bunker/abc123/docker.sock TMPDIR=/tmp BUNKER_SANDBOX=1 \"/home/bunker-abc123/.bunker/exec-script.sh\"'"
)

// goldenSSHArgv returns the ssh argv carrying the given remote command string.
func goldenSSHArgv(remote string) []string {
	return []string{
		"ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-i", imgTestKeyPath,
		"bunker-abc123@localhost",
		remote,
	}
}

// TestExecBuilders_NonImagePathIsByteIdentical pins the acceptance criterion
// that an agent spawned WITHOUT an image spec keeps the exact pre-change
// command: the image-aware builders, given an empty ref, must produce the
// goldens above byte for byte.
func TestExecBuilders_NonImagePathIsByteIdentical(t *testing.T) {
	ctx := context.Background()
	script := "#!/bin/sh\necho hi\n"

	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{
			"shell/no-disclosure",
			buildExecSSHCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, "docker", []string{"version"}, false, "").Args,
			goldenSSHArgv(goldenShellOff),
		},
		{
			"shell/disclosure",
			buildExecSSHCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, "sh", []string{"-c", "echo SB=$BUNKER_SANDBOX"}, true, "").Args,
			goldenSSHArgv(goldenShellOn),
		},
		{
			"raw/no-disclosure",
			buildExecSSHRawCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, "echo", []string{"it's", "a b"}, false, "").Args,
			goldenRawOff,
		},
		{
			"raw/disclosure",
			buildExecSSHRawCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, "echo", []string{"hi"}, true, "").Args,
			goldenRawOn,
		},
		{
			"script/no-disclosure",
			buildExecSSHScriptCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, script, false, "").Args,
			goldenSSHArgv(goldenScriptOff),
		},
		{
			"script/disclosure",
			buildExecSSHScriptCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, script, true, "").Args,
			goldenSSHArgv(goldenScriptOn),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !reflect.DeepEqual(c.got, c.want) {
				t.Errorf("non-image exec command drifted from the pre-GAP-069 bytes:\n got: %q\nwant: %q", c.got, c.want)
			}
		})
	}
}

// TestExecBuilders_ImageShellMode proves the shell-mode wrapper shape: the
// remote side runs docker against the agent's OWN rootless socket (through the
// DOCKER_HOST the outer env(1) sets) and hands the user command to `sh -lc`
// inside a fresh container of the image, with the agent home bind-mounted at
// the same path and used as the working directory.
func TestExecBuilders_ImageShellMode(t *testing.T) {
	ctx := context.Background()

	wrapPrefix := "docker run --rm -v " + imgTestHome + ":" + imgTestHome + " -w " + imgTestHome + " " + imgTestRef + " sh -lc"

	off := buildExecSSHCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, "which", []string{"jq"}, false, imgTestRef).Args
	remote := off[len(off)-1]
	if !strings.HasPrefix(remote, "sh -c '") {
		t.Fatalf("ssh remote arg must be a single quoted sh -c string, got %q", remote)
	}
	if !strings.Contains(remote, wrapPrefix) {
		t.Errorf("remote command missing container wrapper %q: %q", wrapPrefix, remote)
	}
	if !strings.Contains(remote, "DOCKER_HOST=unix:///run/bunker/"+imgTestAgentID+"/docker.sock") {
		t.Errorf("docker CLI must run on the REMOTE side against the agent socket: %q", remote)
	}
	if strings.Contains(remote, "BUNKER_SANDBOX") {
		t.Errorf("disclosure disabled must not inject the containment marker: %q", remote)
	}

	on := buildExecSSHCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, "which", []string{"jq"}, true, imgTestRef).Args
	onRemote := on[len(on)-1]
	if !strings.Contains(onRemote, "docker run --rm -e "+containmentSandboxEnv+" -v ") {
		t.Errorf("disclosure enabled must inject -e %s into the container: %q", containmentSandboxEnv, onRemote)
	}
}

// TestExecImageCommand_RunsInsideImageContainer is the semantic proof: the
// built remote command is handed to a REAL /bin/sh (the agent sshd shell
// stand-in) with a stub `docker` on the exec PATH, and the stub records the
// argv docker would have received. The nested quoting therefore has to survive
// two shell layers, and the user command must arrive as the exact single
// argument of `sh -lc`.
func TestExecImageCommand_RunsInsideImageContainer(t *testing.T) {
	cases := []struct {
		name      string
		disclosed bool
		wantArgs  []string
	}{
		{
			name:      "disclosure-off",
			disclosed: false,
			wantArgs:  []string{"run", "--rm", "-v", "/AGENTHOME:/AGENTHOME", "-w", "/AGENTHOME", imgTestRef, "sh", "-lc", "echo 'it'\\''s' 'a b'"},
		},
		{
			name:      "disclosure-on",
			disclosed: true,
			wantArgs:  []string{"run", "--rm", "-e", containmentSandboxEnv, "-v", "/AGENTHOME:/AGENTHOME", "-w", "/AGENTHOME", imgTestRef, "sh", "-lc", "echo 'it'\\''s' 'a b'"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir() // agent home: the exec PATH is <home>/bin:..., so the stub docker resolves
			binDir := filepath.Join(home, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			argvFile := filepath.Join(t.TempDir(), "docker-argv")
			stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + argvFile + "\n"
			if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}

			remote := buildAgentImageExecCommand(imgTestAgentID, home, "echo", []string{"it's", "a b"}, c.disclosed, imgTestRef)
			out, err := exec.Command("sh", "-c", remote).CombinedOutput()
			if err != nil {
				t.Fatalf("built remote command failed: %v, output: %s\ncommand: %s", err, out, remote)
			}

			got := readArgvFile(t, argvFile)
			want := make([]string, len(c.wantArgs))
			for i, a := range c.wantArgs {
				want[i] = strings.ReplaceAll(a, "/AGENTHOME", home)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("docker argv through the shell layers:\n got: %q\nwant: %q", got, want)
			}

			// The argument docker passes to `sh -lc` must be a valid command
			// for the container shell and must reproduce the user argv.
			lc := got[len(got)-1]
			inner, err := exec.Command("sh", "-c", lc).CombinedOutput()
			if err != nil {
				t.Fatalf("in-container command %q failed: %v, output: %s", lc, err, inner)
			}
			if string(inner) != "it's a b\n" {
				t.Errorf("in-container output = %q, want %q", inner, "it's a b\n")
			}
		})
	}
}

// readArgvFile reads one argv element per line from a stub-recorded file.
func readArgvFile(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stub argv file: %v", err)
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// TestExecBuilders_ImageRawMode proves raw mode keeps its no-shell contract:
// the env prefix is unchanged, then docker run wraps the command with its args
// verbatim (no sh -lc layer, so nothing is shell-interpreted a layer early).
func TestExecBuilders_ImageRawMode(t *testing.T) {
	ctx := context.Background()
	got := buildExecSSHRawCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, "which", []string{"jq"}, false, imgTestRef).Args

	// The leading elements are the ssh prefix plus the unchanged env(1) prefix,
	// compared against the PRE-CHANGE argv: the golden's trailing user command
	// (`echo` + 2 args = 3 elements) is where the container wrapper starts.
	offPrefixLen := len(goldenRawOff) - 3
	if !reflect.DeepEqual(got[:offPrefixLen], goldenRawOff[:offPrefixLen]) {
		t.Errorf("raw mode env prefix changed:\n got: %q\nwant: %q", got[:offPrefixLen], goldenRawOff[:offPrefixLen])
	}
	wantTail := []string{"docker", "run", "--rm", imgTestRef, "which", "jq"}
	if !reflect.DeepEqual(got[offPrefixLen:], wantTail) {
		t.Errorf("raw mode container argv = %q, want %q", got[offPrefixLen:], wantTail)
	}

	// Disclosure ON adds the marker to the outer env(1) argv exactly as before
	// (golden: env prefix including BUNKER_SANDBOX=1, then `echo hi`) AND as a
	// container env var, so the in-container view matches.
	onPrefixLen := len(goldenRawOn) - 2
	gotOn := buildExecSSHRawCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, "which", []string{"jq"}, true, imgTestRef).Args
	if !reflect.DeepEqual(gotOn[:onPrefixLen], goldenRawOn[:onPrefixLen]) {
		t.Errorf("raw mode env prefix (disclosure) changed:\n got: %q\nwant: %q", gotOn[:onPrefixLen], goldenRawOn[:onPrefixLen])
	}
	wantOnTail := []string{"docker", "run", "--rm", "-e", containmentSandboxEnv, imgTestRef, "which", "jq"}
	if !reflect.DeepEqual(gotOn[onPrefixLen:], wantOnTail) {
		t.Errorf("raw mode container argv (disclosure) = %q, want %q", gotOn[onPrefixLen:], wantOnTail)
	}
}

// TestExecBuilders_ImageScriptMode proves the upload flow is preserved: the
// heredoc write, the chmod +x and the env chain are the PRE-CHANGE bytes (the
// golden prefix), and only the final invocation moves into the container.
func TestExecBuilders_ImageScriptMode(t *testing.T) {
	ctx := context.Background()
	script := "#!/bin/sh\necho hi\n"
	cmd := buildExecSSHScriptCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, script, false, imgTestRef)
	remote := cmd.Args[len(cmd.Args)-1]

	// Premise of the anchor below: the golden really carries the env chain.
	const anchor = "TMPDIR=/tmp "
	if !strings.Contains(goldenScriptOff, anchor) {
		t.Fatalf("golden script command lost its env chain anchor: %q", goldenScriptOff)
	}
	head := goldenScriptOff[:strings.Index(goldenScriptOff, anchor)+len(anchor)]
	if !strings.HasPrefix(remote, head) {
		t.Errorf("script upload/env prefix changed from the pre-GAP-069 bytes:\n got: %q\nwant prefix: %q", remote, head)
	}
	rest := strings.TrimPrefix(remote, head)
	scriptPath := imgTestHome + "/.bunker/exec-script.sh"
	wantWrap := "docker run --rm -v " + imgTestHome + ":" + imgTestHome + " " + imgTestRef + " sh "
	if !strings.HasPrefix(rest, wantWrap) {
		t.Errorf("script exec not container-wrapped:\n got: %q\nwant prefix: %q", rest, wantWrap)
	}
	if !strings.Contains(rest, scriptPath) {
		t.Errorf("container-wrapped script exec lost the uploaded script path: %q", rest)
	}
	if strings.Contains(remote, "BUNKER_SANDBOX") {
		t.Errorf("disclosure disabled must not inject the containment marker: %q", remote)
	}

	on := buildExecSSHScriptCommandImage(ctx, imgTestAgentID, imgTestKeyPath, imgTestHome, script, true, imgTestRef).Args
	onRemote := on[len(on)-1]
	if !strings.Contains(onRemote, "docker run --rm -e "+containmentSandboxEnv+" -v ") {
		t.Errorf("script-mode disclosure must inject -e %s: %q", containmentSandboxEnv, onRemote)
	}
}

// TestExecAgent_ConsultsAgentRecordImage drives the real ExecAgent RPC through
// a real connect handler over httptest with a stub `ssh` on PATH, so the
// recorded ssh argv proves the wiring end to end: an agent whose tracker record
// carries an image ref execs through docker run, and an agent without one execs
// with the byte-identical pre-GAP-069 command.
func TestExecAgent_ConsultsAgentRecordImage(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		imageRef string
		command  string
		args     []string
		check    func(t *testing.T, id string, argv []string)
	}{
		{
			// The battery case this unblocks: an image-spec agent must run the
			// command inside its container so `which jq` finds the image's jq.
			name:     "image-agent",
			id:       "e2e-imgspec",
			imageRef: imgTestRef,
			command:  "which",
			args:     []string{"jq"},
			check: func(t *testing.T, id string, argv []string) {
				home := "/home/bunker-" + id
				remote := argv[len(argv)-1]
				wantWrap := "docker run --rm -v " + home + ":" + home + " -w " + home + " " + imgTestRef + " sh -lc"
				if !strings.Contains(remote, wantWrap) {
					t.Errorf("image agent exec is not container-wrapped:\n got: %q\nwant substring: %q", remote, wantWrap)
				}
				if !strings.Contains(remote, "which") || !strings.Contains(remote, "jq") {
					t.Errorf("user command lost in the wrapper: %q", remote)
				}
				if !strings.Contains(remote, "DOCKER_HOST=unix:///run/bunker/"+id+"/docker.sock") {
					t.Errorf("image agent exec must reach the agent's own socket: %q", remote)
				}
			},
		},
		{
			// Same RPC, same agent id and same command as the golden capture,
			// so the recorded argv can be compared to the PRE-CHANGE bytes.
			name:     "plain-agent",
			id:       "abc123",
			imageRef: "",
			command:  "docker",
			args:     []string{"version"},
			check: func(t *testing.T, id string, argv []string) {
				want := goldenSSHArgv(goldenShellOff)[1:] // the stub records argv without argv[0]
				if !reflect.DeepEqual(argv, want) {
					t.Errorf("plain agent exec drifted from the pre-GAP-069 bytes:\n got: %q\nwant: %q", argv, want)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sshArgvFile := installStubSSH(t)
			logger := testDiscardLogger()
			tracker := resource.NewTracker(10, logger)
			if err := tracker.Register(&resource.AgentRecord{
				AgentID:           c.id,
				Status:            "running",
				SshPrivateKeyPath: imgTestKeyPath,
				Image:             c.imageRef,
			}); err != nil {
				t.Fatalf("register: %v", err)
			}
			svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker}

			path, handler := bunkerv1connect.NewBunkerdHandler(svc)
			mux := http.NewServeMux()
			mux.Handle(path, handler)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
			stream, err := client.ExecAgent(context.Background(), connect.NewRequest(&v1.ExecAgentRequest{
				AgentId: c.id,
				Command: c.command,
				Args:    c.args,
			}))
			if err != nil {
				t.Fatalf("ExecAgent: %v", err)
			}
			for stream.Receive() {
				_ = stream.Msg()
			}
			if err := stream.Err(); err != nil {
				t.Fatalf("stream receive: %v", err)
			}
			if err := stream.Close(); err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("stream close: %v", err)
			}
			c.check(t, c.id, readArgvFile(t, sshArgvFile))
		})
	}
}

// installStubSSH puts a stub `ssh` first on PATH that records the argv it was
// invoked with (one element per line) and returns the record path.
func installStubSSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "ssh-argv")
	stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + out + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return out
}
