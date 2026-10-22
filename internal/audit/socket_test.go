package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SURF-B4 — audit continuity for socket-based tool delivery.
//
// These tests pin the trail-side half of the SPEC-toolsd-agent-surfaces §3.4
// decision (option a): verbs performed over the agent-owned toolsd socket are
// written into the daemon's hash-chained trail with the SAME facts an exec
// record carries — who (caller/agent_id), what verb (method), what path and
// under what confinement root (summary) — and the chain verifies afterward.
// The negative arm pins that a mutating socket write which is absent from the
// hash chain FAILS the row (a gap in the chain is a tamper, and an
// unattributed mutating verb is refused, never silently recorded).

// TestRecordSocketVerbAppearsInVerifiedChain: a mutating verb recorded
// through RecordSocketVerb is (1) present in the log with the same
// attribution fields an exec record carries and (2) covered by a chain that
// `bunker audit verify` accepts — `Verify` exits clean and the record's hash
// is the chain head the anchor names.
func TestRecordSocketVerbAppearsInVerifiedChain(t *testing.T) {
	l, path := newTestLog(t)

	// A daemon record precedes the socket verb, so the socket record must
	// CHAIN ONTO existing trail content, not start a fresh segment.
	writeRecords(t, l, 2)

	err := RecordSocketVerb(l, SocketRecord{
		Verb:        "toolsd_write",
		AgentID:     "sock-proof",
		Path:        "src/main.go",
		Root:        "/home/agent/proj",
		ArgsSummary: "len=142",
		SessionID:   "sess-abc123",
		Outcome:     "ok",
		DurationMS:  7,
	})
	if err != nil {
		t.Fatalf("RecordSocketVerb: %v", err)
	}

	recs := parseRecords(t, readLog(t, path))
	if len(recs) != 3 {
		t.Fatalf("records = %d, want 3 (2 daemon + 1 socket)", len(recs))
	}
	rec := recs[2]

	// Who.
	if got, want := rec["caller"], "agent:sock-proof"; got != want {
		t.Errorf("caller = %v, want %q (socket identity rendered like exec)", got, want)
	}
	if got, want := rec["agent_id"], "sock-proof"; got != want {
		t.Errorf("agent_id = %v, want %q", got, want)
	}
	// What verb — suffixed, so --method finds it either way.
	if got, want := rec["method"], "toolsd_write@socket"; got != want {
		t.Errorf("method = %v, want %q", got, want)
	}
	// What path + under what confinement root — in the summary.
	summary, _ := rec["summary"].(string)
	for _, want := range []string{"path=src/main.go", "root=/home/agent/proj"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q does not carry %q (the trail must record path and root)", summary, want)
		}
	}
	// Honest transport naming: a socket row must be distinguishable from a
	// daemon RPC row.
	if got, want := rec["remote_addr"], SocketRemote; got != want {
		t.Errorf("remote_addr = %v, want %q", got, want)
	}
	// Session attribution survives (redaction applied by logLocked).
	if got, want := rec["session_id"], "sess-abc123"; got != want {
		t.Errorf("session_id = %v, want %q", got, want)
	}
	if got := rec["outcome"]; got != "ok" {
		t.Errorf("outcome = %v, want ok", got)
	}

	// The chain — including the socket record — verifies, and the anchor
	// names the socket record's hash as the head.
	res, err := VerifyDetailed(path, "")
	if err != nil {
		t.Fatalf("VerifyDetailed after socket record: %v", err)
	}
	if !res.Verified() {
		t.Fatalf("chain+anchor did not fully verify: %+v", res)
	}
	if got := rec["hash"].(string); got != res.Anchor.Head {
		t.Errorf("anchor head = %q, want the socket record's hash %q", res.Anchor.Head, got)
	}
}

// TestRecordSocketVerbChainsAcrossRestart: the toolsd record can arrive from
// a DIFFERENT process than the one that wrote the last record (the daemon
// stays up; toolsd is connection-activated). A fresh AuditLog on the same
// file must chain the socket record onto the pre-existing tail and stay
// verify-clean — the DF-BUNKER-29 reseed is what makes cross-process
// attribution possible at all.
func TestRecordSocketVerbChainsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")

	l1, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeRecords(t, l1, 1)
	if err := l1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// toolsd's view: a brand-new AuditLog over the daemon's file.
	l2, err := New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	if err := RecordSocketVerb(l2, SocketRecord{
		Verb:    "toolsd_apply",
		AgentID: "sock-proof",
		Path:    "docs/note.md",
		Root:    "/home/agent/proj",
		Outcome: "ok",
	}); err != nil {
		t.Fatalf("RecordSocketVerb: %v", err)
	}

	recs := parseRecords(t, readLog(t, path))
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	if got, want := recs[1]["prev_hash"], recs[0]["hash"]; got != want {
		t.Errorf("socket record prev_hash = %v, want daemon tail %v (must chain, not restart)", got, want)
	}
	if _, firstBad, err := Verify(path); err != nil || firstBad != 0 {
		t.Fatalf("Verify = (%d, %v), want clean", firstBad, err)
	}
}

// TestSocketMutationVerbsRequireAttribution: the mutating set is exactly the
// verbs that change the tree, and every member is REQUIRED-recorded. A
// mutating verb with no attribution identity is REFUSED — the SPEC's other
// arm ("or the surface refuses operations that require attribution") — and
// the refusal is a named error, not a silent drop.
func TestSocketMutationVerbsRequireAttribution(t *testing.T) {
	for verb := range SocketFileOps {
		if !IsSocketMutation(verb) {
			t.Errorf("IsSocketMutation(%q) = false, want true", verb)
		}
	}
	if IsSocketMutation("toolsd_read") || IsSocketMutation("toolsd_list") {
		t.Error("read-only verbs must not be in the required-record set")
	}
	// Case-sensitivity: a renamed/respelled verb must not silently escape.
	if IsSocketMutation("Toolsd_Write") {
		t.Error("verb matching must be exact-case")
	}

	l, path := newTestLog(t)
	err := RecordSocketVerb(l, SocketRecord{Verb: "toolsd_write", AgentID: "", Path: "x", Outcome: "ok"})
	if err == nil {
		t.Fatal("unattributed mutating verb was recorded — this is the SURF-B4 hole, not its fix")
	}
	if !strings.Contains(err.Error(), "unattributed") {
		t.Errorf("refusal error does not name the cause: %v", err)
	}
	if recs := parseRecords(t, readLog(t, path)); len(recs) != 0 {
		t.Fatalf("refused verb left %d record(s) in the trail", len(recs))
	}
	// The non-mutating arm: RequireSocketRecord is what the surface calls
	// before executing, and it refuses unattributed verbs with a named error.
	if err := RequireSocketRecord("toolsd_write", "  "); err == nil ||
		!strings.Contains(err.Error(), "requires attribution") {
		t.Errorf("RequireSocketRecord = %v, want named attribution refusal", err)
	}
	if err := RequireSocketRecord("toolsd_write", "sock-proof"); err != nil {
		t.Errorf("RequireSocketRecord with identity = %v, want nil", err)
	}
}

// TestRecordSocketVerbRejectsUnredactedSummary: the recorder RE-CHECKS the
// rendered summary (mirroring RecordExecCommand's belt-and-braces: the caller
// renders via RedactCommandSummary, the recorder is the independent second
// arm). A summary that still carries credential material ANYWHERE — args,
// path or root — is DROPPED with an error, never written. A caller that
// renders properly gets its record with the masked placeholder, and no raw
// secret bytes reach the file either way.
func TestRecordSocketVerbRejectsUnredactedSummary(t *testing.T) {
	const secret = "hunter2supersecret"

	// Raw credential in the args summary → dropped, nothing written.
	l, path := newTestLog(t)
	err := RecordSocketVerb(l, SocketRecord{
		Verb:        "toolsd_write",
		AgentID:     "sock-proof",
		Path:        "env",
		ArgsSummary: "API_SECRET=" + secret,
		Outcome:     "ok",
	})
	if err == nil || !strings.Contains(err.Error(), "not fully redacted") {
		t.Fatalf("summary carrying raw credential material was not dropped: err=%v", err)
	}
	if recs := parseRecords(t, readLog(t, path)); len(recs) != 0 {
		t.Fatalf("dropped summary still wrote %d record(s)", len(recs))
	}

	// Raw credential in the PATH → same verdict; the re-check covers the
	// whole rendered summary, not just the args field.
	l2, path2 := newTestLog(t)
	err = RecordSocketVerb(l2, SocketRecord{
		Verb:    "toolsd_write",
		AgentID: "sock-proof",
		Path:    "log --token=" + secret,
		Outcome: "ok",
	})
	if err == nil {
		t.Fatal("summary carrying credential syntax in the path was written; want refusal")
	}
	if recs := parseRecords(t, readLog(t, path2)); len(recs) != 0 {
		t.Fatalf("dropped summary still wrote %d record(s)", len(recs))
	}

	// The honest path: the caller renders through RedactCommandSummary first
	// (as toolsd must), and the record lands with the masked placeholder.
	l3, path3 := newTestLog(t)
	err = RecordSocketVerb(l3, SocketRecord{
		Verb:        "toolsd_write",
		AgentID:     "sock-proof",
		Path:        "env",
		ArgsSummary: RedactCommandSummary("write", []string{"--secret=" + secret}),
		Outcome:     "ok",
	})
	if err != nil {
		t.Fatalf("RecordSocketVerb with properly rendered summary: %v", err)
	}
	raw := string(readLog(t, path3))
	if strings.Contains(raw, secret) {
		t.Fatal("raw credential material reached the audit file")
	}
	recs := parseRecords(t, readLog(t, path3))
	if len(recs) != 1 || !strings.Contains(recs[0]["summary"].(string), "[REDACTED:") {
		t.Fatalf("masked placeholder missing from the trail: %+v", recs)
	}
}

// TestRecordSocketVerbChainsAcrossRotation: the trail-side machinery must not
// depend on the record landing in a fresh file — a socket record that
// triggers rotation chains across the segment boundary and the whole
// retained chain still verifies (rotation is where naive second writers have
// historically broken, e.g. restart-chain breakage DF-BUNKER-29).
func TestRecordSocketVerbChainsAcrossRotation(t *testing.T) {
	l, path := newTestLog(t)
	l.rotateAt = 256 // force the socket record to push past the threshold
	writeRecords(t, l, 2)

	if err := RecordSocketVerb(l, SocketRecord{
		Verb:    "toolsd_patch",
		AgentID: "sock-proof",
		Path:    "a/b/c.txt",
		Root:    "/srv/agent",
		Outcome: "ok",
	}); err != nil {
		t.Fatalf("RecordSocketVerb: %v", err)
	}

	if _, firstBad, err := Verify(path); err != nil || firstBad != 0 {
		t.Fatalf("Verify across rotation = (firstBad %d, %v), want clean", firstBad, err)
	}
	recs := parseRecords(t, readLog(t, path))
	if len(recs) == 0 || recs[len(recs)-1]["method"] != "toolsd_patch@socket" {
		t.Fatalf("socket record is not the live-file tail after rotation: %+v", recs)
	}
}

// TestSocketRPCEndToEnd: the AC3 proof, mechanical and repeatable — a real
// JSON-RPC request exchanged over a real UNIX socket (the agent-owned-socket
// shape: the toolsd side is reached through the socket boundary itself,
// exactly as the SSH-forwarded surface presents it) performs a MUTATING verb;
// the verb is recorded through RecordSocketVerb into the daemon's chain; the
// chain — socket record included — is accepted by audit.Verify, and the
// record is findable by the attribution fields an exec record carries. The
// negative arm: the same socket exchange SKIPPING the record leaves the write
// absent from the chain, which is the SURF-B4 hole this row closes (the E2E
// battery's check fails on it).
func TestSocketRPCEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	// The daemon's trail: one pre-existing record, as a live daemon has.
	daemonLog, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeRecords(t, daemonLog, 1)
	if err := daemonLog.Close(); err != nil {
		t.Fatalf("daemon Close: %v", err)
	}

	// The toolsd side: its own AuditLog over the same file (a separate
	// process in production — connection-activated), serving JSON-RPC on a
	// 0600 unix socket in the agent's runtime dir.
	sockPath := filepath.Join(dir, "toolsd.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if err := os.Chmod(sockPath, 0o600); err != nil {
		t.Fatalf("chmod socket: %v", err)
	}

	toolsdLog, err := New(path)
	if err != nil {
		t.Fatalf("toolsd New: %v", err)
	}
	t.Cleanup(func() { _ = toolsdLog.Close() })

	srv := &unixRPCServer{log: toolsdLog, agentID: "sock-proof", root: "/home/agent/proj"}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			srv.serveConn(conn)
		}
	}()

	// The client speaks JSON-RPC over the socket — write, then read.
	call := func(method string, params map[string]any) map[string]any {
		t.Helper()
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer func() { _ = conn.Close() }()
		req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		if _, err := conn.Write(append(req, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
		buf, err := io.ReadAll(conn)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var resp map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf), &resp); err != nil {
			t.Fatalf("decode %q: %v", buf, err)
		}
		return resp
	}

	resp := call("toolsd_write", map[string]any{"path": "src/main.go", "bytes": 142})
	if resp["error"] != nil {
		t.Fatalf("socket write verb failed: %v", resp["error"])
	}

	// The RPC performed over the agent-owned socket is in the chain, with
	// exec-equivalent attribution, and the WHOLE chain verifies.
	recs, err := Query(path, Filter{Method: "toolsd_write"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("socket RPC absent from the trail — this is the SURF-B4 hole: %d matching records", len(recs))
	}
	rec := recs[0]
	if rec.Caller != "agent:sock-proof" || rec.AgentID != "sock-proof" ||
		rec.RemoteAddr != SocketRemote || rec.Outcome != "ok" ||
		!strings.Contains(rec.Summary, "path=src/main.go") ||
		!strings.Contains(rec.Summary, "root=/home/agent/proj") {
		t.Fatalf("socket record lacks exec-equivalent attribution: %+v", rec)
	}

	if _, firstBad, err := Verify(path); err != nil || firstBad != 0 {
		t.Fatalf("bunker audit verify must accept the chain with the socket record in it: firstBad=%d err=%v", firstBad, err)
	}
}

// unixRPCServer is the minimal toolsd-side contract this row pins: a
// JSON-RPC-over-unix-socket surface whose mutating verbs write their
// attributed record before answering ok. unattributed mutating calls are
// REFUSED (RequireSocketRecord), never executed silently.
type unixRPCServer struct {
	log     *AuditLog
	agentID string
	root    string
}

func (s *unixRPCServer) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	dec := json.NewDecoder(bufio.NewReader(conn))
	var req struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := dec.Decode(&req); err != nil {
		return
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": 1}
	if IsSocketMutation(req.Method) {
		if err := RequireSocketRecord(req.Method, s.agentID); err != nil {
			resp["error"] = map[string]any{"code": -32001, "message": err.Error()}
			_ = json.NewEncoder(conn).Encode(resp)
			return
		}
		p, _ := req.Params["path"].(string)
		err := RecordSocketVerb(s.log, SocketRecord{
			Verb:    req.Method,
			AgentID: s.agentID,
			Path:    p,
			Root:    s.root,
			Outcome: "ok",
		})
		if err != nil {
			resp["error"] = map[string]any{"code": -32002, "message": err.Error()}
			_ = json.NewEncoder(conn).Encode(resp)
			return
		}
	}
	resp["result"] = "ok"
	_ = json.NewEncoder(conn).Encode(resp)
}

func TestSocketMethodQueryable(t *testing.T) {
	l, path := newTestLog(t)
	writeRecords(t, l, 1)
	if err := RecordSocketVerb(l, SocketRecord{
		Verb: "toolsd_write", AgentID: "sock-proof", Path: "x", Outcome: "ok",
	}); err != nil {
		t.Fatalf("RecordSocketVerb: %v", err)
	}
	got, err := Query(path, Filter{Method: "toolsd_write"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 || got[0].Method != "toolsd_write@socket" {
		t.Fatalf("Query by verb = %+v, want exactly the socket record", got)
	}
}
