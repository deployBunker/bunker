package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
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

	// Seal is the GAP-073 rotation seal, present ONLY on seal records
	// (Method=/internal/audit/seal) when audit.seal_key is configured; the
	// omitempty keeps every other record's on-wire bytes byte-identical to
	// the pre-GAP-073 format. A seal binds a completed segment to the live
	// file: HMAC-SHA256(key=seal_key, msg=<sealed chain head>) hex, so
	// holders of shipped copies can prove a local file was truncated or
	// replaced. Seals chain like any record: the seal's prev_hash IS the
	// sealed head, so the hash chain continues through it.
	Seal string `json:"seal,omitempty"`
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

	// GAP-073: opt-in retention hardening. sealKey "" = no seal records;
	// shipper nil = no remote shipping. Both default to off — New(path)
	// leaves them zero and the behavior is byte-identical to pre-GAP-073.
	sealKey string
	shipper *Shipper
	logger  *slog.Logger
}

// New opens (creating if needed) the audit log at path with file mode 0600.
// Missing parent directories are created with mode 0700. GAP-073 retention
// hardening (remote shipping, rotation seals) is OFF: use NewWithOptions or
// SetShipper to enable it.
func New(path string) (*AuditLog, error) {
	return newAuditLog(path)
}

// Options are the GAP-073 retention-hardening knobs for NewWithOptions.
// Zero values keep the feature off.
type Options struct {
	// ShipTo is a validated audit.ship_to endpoint URI (https://…, http://…
	// or syslog://host[:port]); "" disables shipping.
	ShipTo string
	// SealKey, when non-empty, appends a chained SEAL record to the fresh
	// log after every rotation, binding the rotated segment to the live
	// file: HMAC-SHA256(key=SealKey, msg=<chain head>) hex.
	SealKey string
	// Logger receives ship/seal warnings; nil falls back to slog.Default().
	Logger *slog.Logger
}

// NewWithOptions opens the audit log like New and attaches the GAP-073
// retention hardening described by opts. An invalid ShipTo URI returns an
// error: callers must WARN and fall back (server.go disables shipping), never
// crash the daemon or block the write path.
func NewWithOptions(path string, opts Options) (*AuditLog, error) {
	l, err := newAuditLog(path)
	if err != nil {
		return nil, err
	}
	if err := l.applyOptions(opts); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// newAuditLog is the shared constructor body.
func newAuditLog(path string) (*AuditLog, error) {
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

// applyOptions validates opts and attaches the enabled features. Used by
// NewWithOptions and SetShipper.
func (l *AuditLog) applyOptions(opts Options) error {
	if opts.Logger != nil {
		l.logger = opts.Logger
	}
	if opts.SealKey != "" {
		l.sealKey = opts.SealKey
	}
	if opts.ShipTo != "" {
		s, err := NewShipper(l.path, opts.ShipTo, opts.Logger)
		if err != nil {
			return err
		}
		l.shipper = s
	}
	return nil
}

// SetShipper attaches a remote shipper after construction (used by callers
// that build the log with New and then wire shipping from config). An invalid
// shipTo URI returns an error and leaves the log unchanged. An empty shipTo
// disables shipping.
func (l *AuditLog) SetShipper(shipTo string, logger *slog.Logger) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if shipTo == "" {
		if l.shipper != nil {
			l.shipper.Stop()
			l.shipper = nil
		}
		return nil
	}
	if logger != nil {
		l.logger = logger
	}
	s, err := NewShipper(l.path, shipTo, logger)
	if err != nil {
		return err
	}
	l.shipper = s
	return nil
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
	return l.logLocked(rec)
}

// logLocked is Log's body with the lock already held (l.mu is NOT
// re-entrant — this is the only sanctioned way for the rotation path to
// append records). All locking discipline lives in Log and rotateLocked.
func (l *AuditLog) logLocked(rec Record) error {
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

// writeSealRecordLocked appends the GAP-073 rotation seal record to the FRESH
// audit.log after a rotation: Method=/internal/audit/seal, chained like any
// record (its prev_hash is the sealed segment's final chain head — the same
// value the seal HMAC was computed over, which is exactly what makes the
// binding tamper-evident). Called from rotateLocked WITH l.mu already held
// (l.mu is not re-entrant, so it delegates to logLocked, never Log). A seal
// write failure is logged and never fails the rotation or the triggering
// Log() call.
func (l *AuditLog) writeSealRecordLocked(chainHead, seal string) {
	rec := Record{
		TS:      time.Now().UTC().Format(time.RFC3339Nano),
		Caller:  "bunkerd",
		Method:  SealMethod,
		Outcome: "ok",
		Summary: "rotation seal",
		Seal:    seal,
	}
	if err := l.logLocked(rec); err != nil {
		l.logWarn("audit rotation seal write failed", "error", err)
	}
}

// rotateLocked performs the rotation under the caller's lock: audit.log ->
// .1, .1 -> .2, .2 -> .3, anything older dropped, then a fresh audit.log is
// opened with mode 0600. Renames preserve the 0600 mode of the rotated files.
// The hash chain is not broken — lastHash persists, so the first record of
// the fresh file chains to the last record of audit.log.1.
//
// GAP-073: after the fresh file is open, the just-rotated segment (now at
// path.1) is handed to the remote shipper (fire-and-forget) and, when a seal
// key is configured, a chained SEAL record is appended to the fresh file.
// Neither step can fail the rotation or block the write path: shipping runs
// on its own goroutine and seal failures are only logged.
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

	// GAP-073 hardening (both steps fire-and-forget, post-rotation):
	head := l.lastHash
	seal := ""
	if l.sealKey != "" && head != "" {
		seal = computeSeal(l.sealKey, head)
	}
	if l.shipper != nil && head != "" {
		l.shipper.ShipSegment(l.path+".1", head, seal)
	}
	if seal != "" {
		l.writeSealRecordLocked(head, seal)
	}
	return nil
}

// Path returns the log file path this AuditLog writes to. It exists so the
// daemon's QueryAudit handler can re-read the trail from disk without
// carrying the path around a second time.
func (l *AuditLog) Path() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.path
}

// logWarn emits a warning on the GAP-073 logger configured at construction
// (nil logger = slog.Default()).
func (l *AuditLog) logWarn(msg string, args ...any) {
	if l.logger != nil {
		l.logger.Warn(msg, args...)
		return
	}
	slog.Warn(msg, args...)
}

// StatusSnapshot returns the current GAP-073 hardening state: whether
// shipping/sealing are enabled, the last ship attempt result, and the retry
// queue depth. With everything off (the default) only the Enabled flags are
// false — the accessor exists for `bunker audit status` and daemon tooling.
func (l *AuditLog) StatusSnapshot() AuditStatus {
	l.mu.Lock()
	shipper, sealing := l.shipper, l.sealKey != ""
	l.mu.Unlock()
	st := AuditStatus{SealingEnabled: sealing}
	if shipper == nil {
		return st
	}
	st.ShippingEnabled = true
	st.ShipTo = shipper.target.Redacted()
	st.LastShipAttempt, st.LastShipSuccess, st.LastShipResult, st.ShipQueueDepth =
		shipper.snapshot()
	return st
}

// Close closes the underlying file and stops the remote shipper (GAP-073)
// when one is attached. The shipper is stopped FIRST and fully awaited, so
// its worker can never touch the filesystem after the log file (or the test
// temp dir) is gone. Safe for concurrent use; subsequent Log calls return
// an error.
func (l *AuditLog) Close() error {
	l.mu.Lock()
	shipper := l.shipper
	l.shipper = nil
	l.mu.Unlock()

	// Stop the shipper before the file: Shipper.Stop blocks until the
	// worker has exited, so no late writeState can recreate a file (or
	// race temp-dir cleanup in tests) after Close returns.
	shipper.Stop()

	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
