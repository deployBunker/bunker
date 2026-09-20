package audit

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	"github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"

	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
)

// GAP-126 / REQ-T1, the audit half: when the daemon serves plaintext on a
// non-loopback listener under the explicit tls.insecure_dev opt-in, EVERY
// record it writes states that fact. These tests drive the real interceptor
// over the wire (so the marker is asserted on the bytes the log actually
// contains, not on an internal struct) and the direct Log path.

// TestInsecurePlaintextRecordMarker is the table-driven proof over the wire.
func TestInsecurePlaintextRecordMarker(t *testing.T) {
	const procedure = "/bunker.v1.Bunkerd/GetAgent"

	tests := []struct {
		name          string
		insecure      bool
		wantMarker    bool
		wantSummary   string // summary when insecure is false (byte-identical requirement)
		wantRecordKey string
	}{
		{
			name:        "secure daemon writes no marker",
			insecure:    false,
			wantMarker:  false,
			wantSummary: "GetAgent agent_id=marker-agent",
		},
		{
			name:        "insecure daemon marks every record",
			insecure:    true,
			wantMarker:  true,
			wantSummary: config.InsecurePlaintextMarker + " GetAgent agent_id=marker-agent",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.log")
			opts := Options{}
			if tc.insecure {
				opts.InsecurePlaintext = true
			}
			l, err := NewWithOptions(path, opts)
			if err != nil {
				t.Fatalf("NewWithOptions: %v", err)
			}
			t.Cleanup(func() { _ = l.Close() })

			if got := l.InsecurePlaintext(); got != tc.insecure {
				t.Fatalf("InsecurePlaintext() = %v, want %v", got, tc.insecure)
			}

			// Drive the real connect handler: the interceptor writes the
			// record exactly as it does in production.
			srv := serveUnary(t, l, &auth.Claims{}, procedure,
				func(_ context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
					return connect.NewResponse(&v1.GetAgentResponse{Agent: &v1.AgentSummary{AgentId: req.Msg.GetAgentId()}}), nil
				})
			client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)

			if _, err := client.GetAgent(context.Background(), connect.NewRequest(&v1.GetAgentRequest{AgentId: "marker-agent"})); err != nil {
				t.Fatalf("GetAgent: %v", err)
			}
			_ = l.Close()

			records := parseRecords(t, readLog(t, path))
			if len(records) != 1 {
				t.Fatalf("got %d records, want 1: %v", len(records), records)
			}
			summary, _ := records[0]["summary"].(string)
			if summary != tc.wantSummary {
				t.Errorf("summary = %q, want %q", summary, tc.wantSummary)
			}
			// The Record field set is unchanged: the marker rides summary,
			// and no key was added or removed.
			for _, key := range requiredFields {
				if _, ok := records[0][key]; !ok {
					t.Errorf("record lost required field %q: %v", key, records[0])
				}
			}
			if _, ok := records[0]["insecure"]; ok {
				t.Error("record grew an 'insecure' field — the marker must ride the existing summary field")
			}
			if tc.wantMarker && !strings.HasPrefix(summary, config.InsecurePlaintextMarker) {
				t.Errorf("summary %q does not start with %q", summary, config.InsecurePlaintextMarker)
			}

			// The marker is part of the hashed canonical line: a marked log
			// still verifies end to end.
			if records, bad, err := Verify(path); err != nil || bad != 0 || records != 1 {
				t.Errorf("Verify(%s) = (records %d, bad %d, err %v), want (1, 0, nil)", path, records, bad, err)
			}
		})
	}
}

// TestInsecurePlaintextMarkerIsStampedOnceAndCoversEveryWriter proves the
// stamp lives on the SINGLE write path: a caller that hands Log a record that
// already carries the marker does not get a second one, a caller that hands it
// an empty summary still gets a marked record, and MarkInsecurePlaintext arms
// the same behaviour after construction.
func TestInsecurePlaintextMarkerIsStampedOnceAndCoversEveryWriter(t *testing.T) {
	tests := []struct {
		name    string
		arm     func(l *AuditLog) // how insecure mode is enabled (nil = via Options)
		summary string
		want    string
	}{
		{
			name:    "empty summary still marked",
			summary: "",
			want:    config.InsecurePlaintextMarker,
		},
		{
			name:    "summary marked once",
			summary: "spawn agent_id=a1",
			want:    config.InsecurePlaintextMarker + " spawn agent_id=a1",
		},
		{
			name:    "already-marked summary is not double-stamped",
			summary: config.InsecurePlaintextMarker + " spawn agent_id=a1",
			want:    config.InsecurePlaintextMarker + " spawn agent_id=a1",
		},
		{
			name:    "MarkInsecurePlaintext arms the same stamp after construction",
			arm:     func(l *AuditLog) { l.MarkInsecurePlaintext() },
			summary: "seal-ish record",
			want:    config.InsecurePlaintextMarker + " seal-ish record",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.log")
			l, err := New(path) // NOT insecure yet
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = l.Close() })
			if tc.arm != nil {
				tc.arm(l)
			} else if err := l.applyOptions(Options{InsecurePlaintext: true}); err != nil {
				t.Fatalf("applyOptions: %v", err)
			}

			if err := l.Log(Record{Method: "/test/op", Outcome: "ok", Summary: tc.summary, TS: "2026-01-01T00:00:00Z"}); err != nil {
				t.Fatalf("Log: %v", err)
			}
			_ = l.Close()

			lines := strings.Split(strings.TrimRight(string(readLog(t, path)), "\n"), "\n")
			if len(lines) != 1 {
				t.Fatalf("got %d records, want 1", len(lines))
			}
			var rec Record
			if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if rec.Summary != tc.want {
				t.Errorf("summary = %q, want %q", rec.Summary, tc.want)
			}
			if got := strings.Count(rec.Summary, config.InsecurePlaintextMarker); got != 1 {
				t.Errorf("marker appears %d times, want exactly 1: %q", got, rec.Summary)
			}
		})
	}
}

// TestSecureLogIsByteIdenticalToPreGap126 pins the non-regression direction:
// a daemon that is NOT in insecure mode writes exactly the record it wrote
// before the marker existed. Without this, the marker could leak into every
// deployment's trail and break downstream parsers.
func TestSecureLogIsByteIdenticalToPreGap126(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{name: "plain New"},
		{name: "explicit insecure=false"},
		{name: "seal key set but insecure off", opts: Options{SealKey: "test-seal-key-material-32-bytes"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.log")
			var (
				l   *AuditLog
				err error
			)
			if tc.opts == (Options{}) {
				l, err = New(path)
			} else {
				l, err = NewWithOptions(path, tc.opts)
			}
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { _ = l.Close() })

			const summary = "spawn agent_id=a1"
			if err := l.Log(Record{Method: "/test/op", Outcome: "ok", Summary: summary, TS: "2026-01-01T00:00:00Z"}); err != nil {
				t.Fatalf("Log: %v", err)
			}
			_ = l.Close()

			rec := parseRecords(t, readLog(t, path))[0]
			if got, _ := rec["summary"].(string); got != summary {
				t.Errorf("summary = %q, want the unmarked %q", got, summary)
			}
			if strings.Contains(string(readLog(t, path)), config.InsecurePlaintextMarker) {
				t.Errorf("marker leaked into a secure log: %q", string(readLog(t, path)))
			}
		})
	}
}

// TestMarkInsecurePlaintextIsStickyAndIdempotent: arming twice is one arming,
// and there is no un-arm — a log that has written marked records must never
// silently start writing unmarked ones.
func TestMarkInsecurePlaintextIsStickyAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	if l.InsecurePlaintext() {
		t.Fatal("a fresh log must not be marked")
	}
	l.MarkInsecurePlaintext()
	l.MarkInsecurePlaintext()
	if !l.InsecurePlaintext() {
		t.Fatal("MarkInsecurePlaintext did not arm the marker")
	}
	if err := l.Log(Record{Summary: "after arming", TS: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatalf("Log: %v", err)
	}
	_ = l.Close()

	rec := parseRecords(t, readLog(t, path))[0]
	if got, _ := rec["summary"].(string); got != config.InsecurePlaintextMarker+" after arming" {
		t.Errorf("summary = %q, want a marked record", got)
	}
}
