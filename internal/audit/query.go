package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

// Filter restricts which records Query returns. All fields are ANDed;
// empty fields match everything.
type Filter struct {
	// AgentID is an exact match on the record's agent_id field.
	AgentID string
	// Method is a substring match on the record's method (the full connect
	// procedure, e.g. "/bunker.v1.Bunkerd/SpawnAgent").
	Method string
	// Since, when set, keeps only records whose ts is at or after this
	// instant (inclusive).
	Since *time.Time
	// Until, when set, keeps only records whose ts is at or before this
	// instant (inclusive).
	Until *time.Time
	// Limit caps the number of returned records at the NEWEST end of the
	// chain: records are scanned oldest-first, so with Limit=5 the five
	// most recent matches are returned. 0 means no limit.
	Limit int
}

// Query reads the audit log at path together with its rotated backups
// (path.1 .. path.MaxBackups) in chain order — oldest backup first, the
// live file last — parses every line as a Record, and returns the records
// matching f in chain order (oldest first).
//
// The returned records are lossless: hash and prev_hash are preserved, so
// an export built from Query output reproduces the log byte-for-byte (the
// JSONL writer emits the same struct with the same field order).
//
// Lines that do not parse as a Record are skipped: Query is a read surface
// for forensics, not a verifier — use Verify to detect tampering. Records
// whose ts does not parse as RFC3339Nano are treated as timestamp-less:
// they match when no time filter is set and are excluded by since/until.
//
// A missing live file is an error (like Verify); missing backups are
// skipped. The live file may be empty, in which case the result is nil.
func Query(path string, f Filter) ([]Record, error) {
	var records []Record
	for i := MaxBackups; i >= 1; i-- {
		p := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(p); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, pathError(p, err)
		}
		recs, err := queryFile(p, f)
		if err != nil {
			return nil, err
		}
		records = append(records, recs...)
	}
	recs, err := queryFile(path, f)
	if err != nil {
		return nil, err
	}
	records = append(records, recs...)
	if f.Limit > 0 && len(records) > f.Limit {
		records = records[len(records)-f.Limit:]
	}
	return records, nil
}

// queryFile applies f to every parseable record in one audit file, in line
// order. The file must exist (callers stat first, mirroring Verify).
func queryFile(path string, f Filter) ([]Record, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, pathError(path, err)
	}
	defer file.Close()

	var recs []Record
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue // unparseable line — not our job to flag (see Verify)
		}
		if match(rec, f) {
			recs = append(recs, rec)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return recs, nil
}

// pathError is the audit package's single wrapper for local-file access
// failures (os.Stat / os.Open / os.OpenFile in audit.go, query.go, status.go,
// verify.go). It turns a failure into an error that names the path ONCE, plus
// an actionable hint when the cause is a permission failure.
//
// os.Stat and os.Open already return *os.PathError, whose Error() is
// "<op> <path>: <cause>", so wrapping them as fmt.Errorf("%s %s: %w", op,
// path, err) printed the path twice — the doubled, unactionable
// "open /var/log/bunkerd/audit.log: open /var/log/bunkerd/audit.log:
// permission denied" seen by a non-root `bunker audit export/list`
// (DF-BUNKER-17). The *os.PathError is therefore returned unmodified (it
// already carries op + path + cause) and the permission case gets the hint
// appended. Callers keep full classification: errors.Is(err, fs.ErrPermission)
// / os.IsNotExist still match the wrapped error.
func pathError(path string, err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w — run as root (the audit log is mode 0600 and root-owned by default) or point --path at an audit log you can read", err)
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return err // already names op + path + cause exactly once
	}
	// Not a *os.PathError: name the path ourselves, still exactly once.
	return fmt.Errorf("open %s: %w", path, err)
}

// match reports whether rec satisfies every set field of f.
func match(rec Record, f Filter) bool {
	if f.AgentID != "" && rec.AgentID != f.AgentID {
		return false
	}
	if f.Method != "" && !strings.Contains(rec.Method, f.Method) {
		return false
	}
	if f.Since != nil || f.Until != nil {
		ts, err := time.Parse(time.RFC3339Nano, rec.TS)
		if err != nil {
			// Timestamp-less record: excluded when any time filter is set.
			return false
		}
		if f.Since != nil && ts.Before(*f.Since) {
			return false
		}
		if f.Until != nil && ts.After(*f.Until) {
			return false
		}
	}
	return true
}
