// Package audit provides an append-only structured audit log for bunkerd.
//
// Every authenticated RPC produces exactly one JSONL record capturing who did
// what (caller identity, procedure, remote address, target agent), how long it
// took, and the outcome. Token values are never written: caller identity is
// derived exclusively from the authenticated Claims (agent id / key id /
// subject) that the auth interceptor placed into the request context.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// MaxSize is the audit log rotation threshold: 5 MiB, matching this machine's
// logging config (Hermes rotates logs at 5MB with 3 backups). When the live
// log reaches this size the next record triggers a rotation instead of an
// unbounded append.
const MaxSize = 5 << 20 // 5 MiB

// MaxBackups is how many rotated audit log backups are kept, mirroring the
// host logging config. audit.log is the live file; audit.log.1 is the most
// recent backup, .2 the next oldest, .3 the oldest retained — anything older
// is dropped on rotation.
const MaxBackups = 3

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

	// Hash is the SHA-256 hex digest of this record's canonical bytes — the
	// line exactly as written, with the hash field itself empty (the digest
	// cannot depend on its own value). Everything else, including prev_hash,
	// is covered, so a single edited byte anywhere in the record breaks the
	// digest. Computed by Log; do not set by hand.
	Hash string `json:"hash"`
	// PrevHash is the Hash of the previous record in the chain, binding this
	// record to its predecessor (tamper-evidence). The first record ever
	// written has ""; after a rotation the first record of the fresh file
	// chains to the last record of the rotated file.
	PrevHash string `json:"prev_hash"`
}

// AuditLog is an append-only JSONL writer. All writes are serialized under a
// mutex and each record is emitted as a single Write call on an O_APPEND file,
// so records are never interleaved, rewritten, or truncated mid-stream.
type AuditLog struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	lastHash string // Hash of the most recently written record (chain link)
	rotateAt int64  // size threshold that triggers rotation; MaxSize by default
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
	return &AuditLog{f: f, path: path, rotateAt: MaxSize}, nil
}

// Log appends one record as a single JSON line, rotating the log first when
// the live file has reached the size threshold. Safe for concurrent use.
//
// Each record is hash-chained: it carries prev_hash (the hash of the previous
// record) and hash (the SHA-256 digest of its own canonical line — the line
// with the hash field empty). The chain spans rotations: lastHash survives a
// rotation, so the first record of a fresh file chains to the last record of
// the rotated file. Verify checks both properties.
func (l *AuditLog) Log(rec Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if info, err := l.f.Stat(); err != nil {
		return fmt.Errorf("stat audit log: %w", err)
	} else if l.rotateAt > 0 && info.Size() >= l.rotateAt {
		if err := l.rotateLocked(); err != nil {
			return err
		}
	}

	// Canonical bytes: the record as authored, hash field empty, prev_hash
	// bound to the previous record. The digest covers prev_hash too, so a
	// forged chain link is itself tamper-evident.
	rec.Hash = ""
	rec.PrevHash = l.lastHash
	canonical, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal audit record: %w", err)
	}
	sum := sha256.Sum256(canonical)
	rec.Hash = hex.EncodeToString(sum[:])
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal audit record: %w", err)
	}
	line = append(line, '\n')

	if _, err := l.f.Write(line); err != nil {
		return fmt.Errorf("write audit record: %w", err)
	}
	l.lastHash = rec.Hash
	return nil
}

// rotateLocked performs the rotation under the caller's lock: audit.log ->
// .1, .1 -> .2, .2 -> .3, anything older dropped, then a fresh audit.log is
// opened with mode 0600. Renames preserve the 0600 mode of the rotated files.
// The hash chain is not broken — lastHash persists, so the first record of
// the fresh file chains to the last record of audit.log.1.
func (l *AuditLog) rotateLocked() error {
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("close audit log for rotation: %w", err)
	}
	// Shift backups oldest-first so .1 always ends up holding the file that
	// was live a moment ago.
	for i := MaxBackups - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", l.path, i)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat backup %s: %w", src, err)
		}
		dst := fmt.Sprintf("%s.%d", l.path, i+1)
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale backup %s: %w", dst, err)
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("shift backup %s -> %s: %w", src, dst, err)
		}
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return fmt.Errorf("rotate audit log %s: %w", l.path, err)
	}
	// Drop anything beyond the backup budget (e.g. leftovers from a previous
	// config that kept more backups).
	if err := os.Remove(fmt.Sprintf("%s.%d", l.path, MaxBackups+1)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale backup: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("reopen audit log %s: %w", l.path, err)
	}
	l.f = f
	return nil
}

// Close closes the underlying file. Safe for concurrent use; subsequent Log
// calls return an error.
func (l *AuditLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
