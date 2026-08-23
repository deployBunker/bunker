// Package audit provides an append-only structured audit log for bunkerd.
//
// Every authenticated RPC produces exactly one JSONL record capturing who did
// what (caller identity, procedure, remote address, target agent), how long it
// took, and the outcome. Token values are never written: caller identity is
// derived exclusively from the authenticated Claims (agent id / key id /
// subject) that the auth interceptor placed into the request context.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Record is one audit log entry for a single authenticated request.
// Field names are the on-wire JSON keys; keep them stable — the log is a
// machine-readable trail consumed by forensics tooling.
type Record struct {
	TS         string `json:"ts"`          // RFC3339Nano UTC timestamp
	Caller     string `json:"caller"`      // token/agent identity, never the raw token
	Method     string `json:"method"`      // full connect procedure, e.g. /bunker.v1.Bunkerd/SpawnAgent
	RemoteAddr string `json:"remote_addr"` // client address as seen by the server
	AgentID    string `json:"agent_id"`    // target agent of the request ("" when none)
	DurationMS int64  `json:"duration_ms"` // wall time from request start to completion
	Outcome    string `json:"outcome"`     // "ok" or the connect error code, e.g. "not_found"
	Summary    string `json:"summary"`     // human-readable request summary
}

// AuditLog is an append-only JSONL writer. All writes are serialized under a
// mutex and each record is emitted as a single Write call on an O_APPEND file,
// so records are never interleaved, rewritten, or truncated mid-stream.
type AuditLog struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// New opens (creating if needed) the audit log at path with file mode 0600.
// Missing parent directories are created with mode 0700.
func New(path string) (*AuditLog, error) {
	if path == "" {
		return nil, fmt.Errorf("audit log path is empty")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create audit dir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log %s: %w", path, err)
	}
	return &AuditLog{f: f, path: path}, nil
}

// Log appends one record as a single JSON line. Safe for concurrent use.
func (l *AuditLog) Log(rec Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal audit record: %w", err)
	}
	b = append(b, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(b); err != nil {
		return fmt.Errorf("write audit record: %w", err)
	}
	return nil
}

// Close closes the underlying file. Safe for concurrent use; subsequent Log
// calls return an error.
func (l *AuditLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
