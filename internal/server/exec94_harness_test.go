package server

// GAP-094 service harness: spin up the real bunkerd handler with the exec
// command builders stubbed to local `sh`, exactly like the DF-BUNKER-27 stream
// tests, plus the small helpers shared by exec_encoding_test.go.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"connectrpc.com/connect"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

const exec94AgentID = "exec94-agent"

// exec94Clienter narrows the client surface the round-trip tests need.
type exec94Clienter interface {
	ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest]) (*connect.ServerStreamForClient[v1.ExecAgentResponse], error)
}

// exec94Service stands up the real handler on a local httptest server with the
// builders stubbed to execute the requested shell locally.
func exec94Service(t *testing.T, script string) {
	t.Helper()
	prevShell, prevRaw, prevScript := execSSHCommandBuilder, execSSHRawCommandBuilder, execSSHScriptCommandBuilder
	argv := []string{"sh", "-c", script}
	build := func(ctx context.Context, _, _, _, _ string, _ []string, _ bool, _ string) *exec.Cmd {
		return exec.CommandContext(ctx, argv[0], argv[1:]...)
	}
	buildScript := func(ctx context.Context, _, _, _, _ string, _ bool, _ string) *exec.Cmd {
		return exec.CommandContext(ctx, argv[0], argv[1:]...)
	}
	execSSHCommandBuilder, execSSHRawCommandBuilder, execSSHScriptCommandBuilder = build, build, buildScript
	t.Cleanup(func() {
		execSSHCommandBuilder, execSSHRawCommandBuilder, execSSHScriptCommandBuilder = prevShell, prevRaw, prevScript
	})
}

// exec94Client returns a real connect client to the handler registered by
// exec94Service.
func exec94Client(t *testing.T) exec94Clienter {
	t.Helper()
	logger := testDiscardLogger()
	tracker := resource.NewTracker(10, logger)
	if err := tracker.Register(&resource.AgentRecord{
		AgentID:           exec94AgentID,
		Status:            "running",
		SshPrivateKeyPath: imgTestKeyPath,
	}); err != nil {
		t.Fatalf("register agent: %v", err)
	}
	svc := &bunkerdService{cfg: config.DefaultConfig(), logger: logger, tracker: tracker}

	path, handler := bunkerv1connect.NewBunkerdHandler(svc)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// keep os imported when only WriteFile used above
var _ = os.WriteFile
