package server

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/descriptorpb"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// This file pins the GAP-128 contract: the spawn wire default carries NO
// private key material, opt-in callers still get it, and the GetAgentKey RPC
// serves the server-side persisted copy with the same not-found / stopped
// semantics the exec path uses.
//
// The GetAgentKey tests run entirely without root: the service is built
// struct-direct (the existing service_lifecycle_test.go pattern) over a
// seeded tracker. The SpawnAgent flag behavior itself is covered by the
// manager-level suite in internal/agent.

func gap128Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// gap128Service builds a bunkerdService over a tracker seeded with the given
// records, with key files materialized on disk for records whose
// SshPrivateKeyPath is set (mode 0600, like the spawn path writes them).
// Exception: a record whose key file lives under <tmp>/absent/ stays absent —
// that naming convention drives the vanished-key-file error case without
// extra plumbing on every test.
func gap128Service(t *testing.T, records []*resource.AgentRecord) *bunkerdService {
	t.Helper()
	cfg := config.DefaultConfig()
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, gap128Logger())
	for _, rec := range records {
		tracker.Register(rec)
		if rec.SshPrivateKeyPath != "" && !strings.Contains(rec.SshPrivateKeyPath, string(filepath.Separator)+"absent"+string(filepath.Separator)) {
			if err := os.MkdirAll(filepath.Dir(rec.SshPrivateKeyPath), 0700); err != nil {
				t.Fatalf("mkdir key dir: %v", err)
			}
			if err := os.WriteFile(rec.SshPrivateKeyPath, []byte("TEST-PRIVATE-KEY-"+rec.AgentID), 0600); err != nil {
				t.Fatalf("write key file: %v", err)
			}
		}
	}
	return &bunkerdService{cfg: cfg, logger: gap128Logger(), tracker: tracker}
}

func TestGetAgentKey_ServesPersistedCopy(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "ssh", "agent-1")
	svc := gap128Service(t, []*resource.AgentRecord{
		{AgentID: "agent-1", Status: "running", SshPrivateKeyPath: keyPath},
	})

	resp, err := svc.GetAgentKey(context.Background(), connect.NewRequest(&v1.GetAgentKeyRequest{AgentId: "agent-1"}))
	if err != nil {
		t.Fatalf("GetAgentKey: %v", err)
	}
	if resp.Msg.GetAgentId() != "agent-1" {
		t.Errorf("AgentId %q, want agent-1", resp.Msg.GetAgentId())
	}
	if resp.Msg.GetSshPrivateKey() != "TEST-PRIVATE-KEY-agent-1" {
		t.Errorf("SshPrivateKey %q, want the persisted copy", resp.Msg.GetSshPrivateKey())
	}
}

func TestGetAgentKey_ErrorMatrix(t *testing.T) {
	tmp := t.TempDir()
	keyPath := filepath.Join(tmp, "ssh", "agent-live")
	running := &resource.AgentRecord{AgentID: "agent-live", Status: "running", SshPrivateKeyPath: keyPath}
	stopped := &resource.AgentRecord{
		AgentID:           "agent-stopped",
		Status:            agent.StatusStopped,
		SshPrivateKeyPath: filepath.Join(tmp, "ssh", "agent-stopped"),
	}

	tests := []struct {
		name    string
		agentID string
		seed    []*resource.AgentRecord
		code    connect.Code
		wantErr string // substring of the message
	}{
		{
			name:    "unknown agent is CodeNotFound",
			agentID: "agent-ghost",
			seed:    []*resource.AgentRecord{running},
			code:    connect.CodeNotFound,
			wantErr: "not found",
		},
		{
			name:    "stopped agent keeps the exec-path precondition",
			agentID: "agent-stopped",
			seed:    []*resource.AgentRecord{stopped},
			code:    connect.CodeFailedPrecondition,
			wantErr: "agent_stopped",
		},
		{
			name:    "record without a persisted key is CodeNotFound",
			agentID: "agent-nokey",
			seed:    []*resource.AgentRecord{{AgentID: "agent-nokey", Status: "running"}},
			code:    connect.CodeNotFound,
			wantErr: "no persisted SSH private key",
		},
		{
			name:    "vanished key file is CodeNotFound naming the path",
			agentID: "agent-gone",
			// <tmp>/absent/<name> tells gap128Service to leave this one absent.
			seed:    []*resource.AgentRecord{{AgentID: "agent-gone", Status: "running", SshPrivateKeyPath: filepath.Join(tmp, "absent", "never-written")}},
			code:    connect.CodeNotFound,
			wantErr: "missing at",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := gap128Service(t, tt.seed)
			_, err := svc.GetAgentKey(context.Background(), connect.NewRequest(&v1.GetAgentKeyRequest{AgentId: tt.agentID}))
			if err == nil {
				t.Fatalf("GetAgentKey(%s) succeeded, want error", tt.agentID)
			}
			if connect.CodeOf(err) != tt.code {
				t.Fatalf("code = %v, want %v (err: %v)", connect.CodeOf(err), tt.code, err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q missing %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestGetAgentKey_KeyFileIsNeverLogged swaps the discard logger for a
// recording one and asserts no key material reaches the log sink through the
// retrieval path.
func TestGetAgentKey_KeyFileIsNeverLogged(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "ssh", "agent-1")
	svc := gap128Service(t, []*resource.AgentRecord{
		{AgentID: "agent-1", Status: "running", SshPrivateKeyPath: keyPath},
	})
	var logged strings.Builder
	svc.logger = slog.New(slog.NewTextHandler(&logged, nil))

	if _, err := svc.GetAgentKey(context.Background(), connect.NewRequest(&v1.GetAgentKeyRequest{AgentId: "agent-1"})); err != nil {
		t.Fatalf("GetAgentKey: %v", err)
	}
	if strings.Contains(logged.String(), "TEST-PRIVATE-KEY") {
		t.Errorf("key material leaked into logs:\n%s", logged.String())
	}
}

// TestSpawnAgentFlag_Contract pins the wire shape: the request field exists
// at number 8 and the response field carries the Deprecated option. The
// behavioral half — manager.Spawn populates SshPrivateKey only when the flag
// is set — lives in the internal/agent GAP-128 tests.
func TestSpawnAgentFlag_Contract(t *testing.T) {
	reqFile := (&v1.SpawnAgentRequest{}).ProtoReflect().Descriptor().ParentFile()
	respFile := (&v1.SpawnAgentResponse{}).ProtoReflect().Descriptor().ParentFile()

	reqMsg := reqFile.Messages().ByName("SpawnAgentRequest")
	reqField := reqMsg.Fields().ByName("return_ssh_private_key")
	if reqField == nil {
		t.Fatal("SpawnAgentRequest.return_ssh_private_key missing from the wire contract")
	}
	if got := reqField.Number(); got != 8 {
		t.Errorf("field number %d, want 8", got)
	}

	respMsg := respFile.Messages().ByName("SpawnAgentResponse")
	respField := respMsg.Fields().ByName("ssh_private_key")
	if respField == nil {
		t.Fatal("SpawnAgentResponse.ssh_private_key missing from the wire contract")
	}
	opts, ok := respField.Options().(*descriptorpb.FieldOptions)
	if !ok || opts == nil || !opts.GetDeprecated() {
		t.Error("SpawnAgentResponse.ssh_private_key must carry the deprecated option (GAP-128)")
	}
}
