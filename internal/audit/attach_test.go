package audit

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecordAttachEvent_WritesOpenAndClose pins the GAP-072 audit contract: an
// attach session contributes two records to the SAME chain the interceptor
// writes - one open, one close - on the AttachAgent procedure's sub-kinds, with
// the phase, the tty posture, the close reason, the exit code and the duration
// stated in fixed vocabulary.
func TestRecordAttachEvent_WritesOpenAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	log, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = log.Close() }()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx := context.Background()

	RecordAttachEvent(ctx, log, logger, AttachRecord{
		AgentID: "abc123",
		Phase:   AttachPhaseOpen,
		TTY:     true,
		Command: RedactCommandSummary("deploy", []string{"--region", "eu-1"}),
	})
	exit := int32(0)
	RecordAttachEvent(ctx, log, logger, AttachRecord{
		AgentID:    "abc123",
		Phase:      AttachPhaseClose,
		TTY:        true,
		Reason:     AttachReasonExited,
		ExitCode:   &exit,
		DurationMS: 4321,
	})

	recs, err := Query(path, Filter{AgentID: "abc123"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2: %+v", len(recs), recs)
	}
	open, close := recs[0], recs[1]

	if open.Method != "/bunker.v1.Bunkerd/AttachAgent"+AttachOpenMethod {
		t.Errorf("open method = %q, want the AttachAgent procedure plus the open sub-kind", open.Method)
	}
	if !strings.Contains(open.Summary, "attach open") || !strings.Contains(open.Summary, "tty=true") {
		t.Errorf("open summary = %q, want the phase and the tty posture", open.Summary)
	}
	if !strings.Contains(open.Summary, "deploy") {
		t.Errorf("open summary %q does not carry the redacted command", open.Summary)
	}
	if open.Outcome != "ok" {
		t.Errorf("open outcome = %q, want ok", open.Outcome)
	}

	if close.Method != "/bunker.v1.Bunkerd/AttachAgent"+AttachCloseMethod {
		t.Errorf("close method = %q, want the AttachAgent procedure plus the close sub-kind", close.Method)
	}
	for _, want := range []string{"attach close", "reason=" + AttachReasonExited, "exit=0", "duration_ms=4321"} {
		if !strings.Contains(close.Summary, want) {
			t.Errorf("close summary = %q, want it to carry %q", close.Summary, want)
		}
	}
	if close.DurationMS != 4321 {
		t.Errorf("close duration_ms = %d, want 4321", close.DurationMS)
	}
	if close.PrevHash != open.Hash {
		t.Errorf("the close record does not chain to the open record (%q vs %q)", close.PrevHash, open.Hash)
	}
	if _, firstBad, err := Verify(path); err != nil || firstBad != 0 {
		t.Errorf("chain verification after attach records (firstBad=%d): %v", firstBad, err)
	}
}

// TestRecordAttachEvent_LoginShellPlaceholder: a command-less attach records
// the login-shell placeholder rather than an empty command field that reads
// like a bug.
func TestRecordAttachEvent_LoginShellPlaceholder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	log, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = log.Close() }()

	RecordAttachEvent(context.Background(), log, nil, AttachRecord{
		AgentID: "abc123", Phase: AttachPhaseOpen, TTY: false,
	})
	recs, err := Query(path, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if !strings.Contains(recs[0].Summary, loginShellSummary) {
		t.Errorf("summary = %q, want the %s placeholder", recs[0].Summary, loginShellSummary)
	}
}

// TestRecordAttachEvent_DropsUnredactedSummary mirrors the exec recorder's
// caution: a caller that hands over a raw credential must not be able to land
// it in the trail.
func TestRecordAttachEvent_DropsUnredactedSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	log, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = log.Close() }()

	RecordAttachEvent(context.Background(), log, nil, AttachRecord{
		AgentID: "abc123",
		Phase:   AttachPhaseOpen,
		Command: "curl -H 'Authorization: Bearer sk-live-0123456789abcdef0123456789abcdef'",
	})
	recs, err := Query(path, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for _, r := range recs {
		if strings.Contains(r.Summary, "sk-live-0123456789abcdef") {
			t.Fatalf("an unredacted credential reached the trail: %q", r.Summary)
		}
	}
}

// TestRecordAttachEvent_NilLogIsNoop: auditing disabled must not change any
// outcome, so the recorder tolerates a nil log.
func TestRecordAttachEvent_NilLogIsNoop(t *testing.T) {
	RecordAttachEvent(context.Background(), nil, nil, AttachRecord{AgentID: "abc123", Phase: AttachPhaseOpen})
}
