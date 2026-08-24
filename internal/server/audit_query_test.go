package server

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/audit"
	"github.com/deployBunker/bunker/internal/config"
)

// newAuditQueryService builds a bunkerdService with a live audit log in a
// temp dir and n written records (agents alternating a0/a1).
func newAuditQueryService(t *testing.T, n int) (*bunkerdService, string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	l, path := newServerAuditLog(t, n)
	return &bunkerdService{
		cfg:      config.DefaultConfig(),
		logger:   logger,
		auditLog: l,
	}, path
}

// newServerAuditLog creates an audit log with n records in a temp dir and
// returns the open log plus its path.
func newServerAuditLog(t *testing.T, n int) (*audit.AuditLog, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := audit.New(path)
	if err != nil {
		t.Fatalf("audit.New(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		rec := audit.Record{
			TS:      base.Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339Nano),
			Caller:  "master",
			Method:  "/bunker.v1.Bunkerd/SpawnAgent",
			AgentID: "a" + string(rune('0'+i%2)),
			Outcome: "ok",
			Summary: "rec",
		}
		if err := l.Log(rec); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}
	return l, path
}

func TestQueryAudit_Filters(t *testing.T) {
	svc, _ := newAuditQueryService(t, 4) // agents: a0 a1 a0 a1
	ctx := context.Background()

	// Agent filter.
	resp, err := svc.QueryAudit(ctx, connect.NewRequest(&v1.QueryAuditRequest{AgentId: "a1"}))
	if err != nil {
		t.Fatalf("QueryAudit(agent): %v", err)
	}
	if len(resp.Msg.Records) != 2 {
		t.Fatalf("agent filter: got %d records, want 2", len(resp.Msg.Records))
	}
	for _, r := range resp.Msg.Records {
		if r.AgentId != "a1" {
			t.Errorf("record agent_id = %q, want a1", r.AgentId)
		}
	}

	// Method filter (substring).
	resp, err = svc.QueryAudit(ctx, connect.NewRequest(&v1.QueryAuditRequest{Method: "SpawnAgent"}))
	if err != nil {
		t.Fatalf("QueryAudit(method): %v", err)
	}
	if len(resp.Msg.Records) != 4 {
		t.Fatalf("method filter: got %d records, want 4", len(resp.Msg.Records))
	}

	// Since/until (inclusive range covering records 1..2 of 4).
	base := time.Date(2026, 8, 20, 12, 0, 1, 0, time.UTC)
	until := time.Date(2026, 8, 20, 12, 0, 2, 0, time.UTC)
	resp, err = svc.QueryAudit(ctx, connect.NewRequest(&v1.QueryAuditRequest{
		Since: base.Format(time.RFC3339),
		Until: until.Format(time.RFC3339),
	}))
	if err != nil {
		t.Fatalf("QueryAudit(since/until): %v", err)
	}
	if len(resp.Msg.Records) != 2 {
		t.Fatalf("since/until: got %d records, want 2", len(resp.Msg.Records))
	}

	// Limit keeps the newest records.
	resp, err = svc.QueryAudit(ctx, connect.NewRequest(&v1.QueryAuditRequest{Limit: 2}))
	if err != nil {
		t.Fatalf("QueryAudit(limit): %v", err)
	}
	if len(resp.Msg.Records) != 2 {
		t.Fatalf("limit: got %d records, want 2", len(resp.Msg.Records))
	}
	if resp.Msg.Records[0].AgentId != "a0" || resp.Msg.Records[1].AgentId != "a1" {
		t.Errorf("limit did not keep the newest records (a0, a1): got %q, %q",
			resp.Msg.Records[0].AgentId, resp.Msg.Records[1].AgentId)
	}

	// Records carry the full lossless field set.
	r := resp.Msg.Records[0]
	if r.Hash == "" || r.PrevHash == "" || r.Ts == "" || r.Caller == "" || r.Method == "" || r.Outcome == "" {
		t.Errorf("record missing lossless fields: %+v", r)
	}
}

func TestQueryAudit_Disabled(t *testing.T) {
	svc := &bunkerdService{
		cfg:    config.DefaultConfig(),
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		// auditLog nil = audit logging disabled
	}
	_, err := svc.QueryAudit(context.Background(), connect.NewRequest(&v1.QueryAuditRequest{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("disabled audit: got %v (code %v), want CodeUnavailable", err, connect.CodeOf(err))
	}
}

func TestQueryAudit_InvalidTimestamp(t *testing.T) {
	svc, _ := newAuditQueryService(t, 1)
	_, err := svc.QueryAudit(context.Background(), connect.NewRequest(&v1.QueryAuditRequest{
		Since: "not-a-time",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid since: got %v (code %v), want CodeInvalidArgument", err, connect.CodeOf(err))
	}
}
