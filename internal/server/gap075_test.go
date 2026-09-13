package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"

	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// containsExact reports whether argv has an element equal to want.
func containsExact(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

// TestExecAgentSessionSeesPrivateTmp is the anti-phantom check for GAP-075 on
// the LIVE exec path: it drives the real ExecAgent RPC through a real connect
// handler with a stub `ssh` on PATH and inspects the argv the server actually
// issued. The session must be pointed at the enforced private /tmp and must
// never be pointed back at the legacy /run/bunker/<id>/tmp directory — the
// change has to be in the command that reaches sshd, not just in a helper.
func TestExecAgentSessionSeesPrivateTmp(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		command  string
		args     []string
		raw      bool
		script   bool
		disclose bool
	}{
		{name: "shell exec", id: "abc123", command: "sh", args: []string{"-c", "echo hi > /tmp/x"}},
		{name: "shell exec with disclosure", id: "abc123", command: "uname", args: []string{"-a"}, disclose: true},
		{name: "raw exec", id: "abc123", command: "printenv", raw: true},
		{name: "script exec", id: "abc123", script: true},
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
			}); err != nil {
				t.Fatalf("register: %v", err)
			}
			cfg := config.DefaultConfig()
			cfg.Containment.Disclosure = c.disclose
			svc := &bunkerdService{cfg: cfg, logger: logger, tracker: tracker}

			path, handler := bunkerv1connect.NewBunkerdHandler(svc)
			mux := http.NewServeMux()
			mux.Handle(path, handler)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			req := &v1.ExecAgentRequest{AgentId: c.id, Command: c.command, Args: c.args, Raw: c.raw}
			if c.script {
				req = &v1.ExecAgentRequest{AgentId: c.id, ScriptContent: "#!/bin/sh\necho hi\n"}
			}

			client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
			stream, err := client.ExecAgent(context.Background(), connect.NewRequest(req))
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

			argv := readArgvFile(t, sshArgvFile)
			joined := strings.Join(argv, " ")

			if len(argv) == 0 {
				t.Fatal("stub ssh recorded no argv")
			}
			// Raw mode passes the env(1) list as separate argv elements, so the
			// TMPDIR assignment must be its own element (it may never be
			// shell-split); shell and script modes pass one quoted string.
			wantTmp := "TMPDIR=" + config.IsolationTmpDir
			if c.raw {
				if !containsExact(argv, wantTmp) {
					t.Errorf("raw session argv has no dedicated %s element\nargv: %q", wantTmp, argv)
				}
			} else if !strings.Contains(argv[len(argv)-1], wantTmp) {
				t.Errorf("session is not pointed at the enforced private /tmp\nargv: %q", argv)
			}
			if strings.Contains(joined, "/run/bunker/"+c.id+"/tmp") {
				t.Errorf("session still advertises the legacy per-agent TMPDIR\nargv: %q", argv)
			}
			if !strings.Contains(joined, wantTmp) {
				t.Errorf("session argv never carries %s\nargv: %q", wantTmp, argv)
			}
			// The transport itself must be untouched: same host/user/port and
			// the agent's own SSH key.
			if !strings.Contains(joined, "bunker-"+c.id+"@localhost") {
				t.Errorf("ssh target changed\nargv: %q", argv)
			}
			if !strings.Contains(joined, imgTestKeyPath) {
				t.Errorf("ssh identity changed\nargv: %q", argv)
			}
			// The private /tmp must come from the session (pam_namespace for
			// members of the agent group), never from a forced command or a
			// per-agent directory: `command=`/ForceCommand would replace the
			// requested command and break scp/sftp/sshfs — i.e. `bunker cp`,
			// `bunker deploy`, `bunker mount` and the docker transport.
			for _, arg := range argv {
				if strings.Contains(arg, "command=") || strings.Contains(arg, "ForceCommand") {
					t.Errorf("exec argv carries a forced-command instruction, which would break scp/sftp transports\nargv: %q", argv)
				}
			}
		})
	}
}
