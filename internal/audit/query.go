package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync/atomic"
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

	// Observer, when set, is invoked by the forward scan once per line —
	// before any limit is applied — with the raw line and a nil error for
	// well-formed lines (the error is reserved for read failures). It is
	// instrumentation-only: any non-nil Filter.Observer forces the forward
	// path and suppresses the PERF-003 backward fast path, so an observer
	// always sees the complete scan. Production callers leave it nil.
	Observer func(line []byte, err error) error
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
	// PERF-003: a bounded, window-less query is served by the tail-shaped
	// backward reader, which stops as soon as Limit matches are collected
	// instead of paying O(file) to truncate afterwards. The tail of the
	// chain it returns is provably identical to the forward scan plus
	// truncation (table-driven parity tests pin this). The Observer field
	// is part of the guard: an installed observer needs the whole scan, so
	// its presence alone forces the forward path.
	if backwardGuard(f) {
		return queryChainBackward(path, f)
	}
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
// order. The file must exist (callers stat first, mirroring Verify). When an
// observer is installed it is invoked once per line — including lines that
// fail to unmarshal (observed=false) — BEFORE the limit is applied, so the
// observer sees the whole scan; a negative return stops it early. queryFile
// returns the observed-and-matching records only.
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
		if f.Observer != nil {
			if err := f.Observer(sc.Bytes(), nil); err != nil {
				break
			}
		}
		rec, ok, err := decodeAndMatch(sc.Bytes(), f)
		if err != nil {
			continue // unparseable line — not our job to flag (see Verify)
		}
		if ok {
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

// ---- PERF-003 backward reader (folded from query_backward.go) ----

// backwardChunkSize is how many bytes the backward reader reads per seek.
// A package-level variable so tests can shrink it to exercise the
// chunk-boundary (partial-line) paths on small fixtures.
var backwardChunkSize int64 = 64 << 10

// backwardReads counts queryFileBackward invocations. It is the PERF-003
// test seam for criterion (d): a query carrying since/until must leave this
// counter untouched (the forward path stays in charge of time windows),
// while a plain limited query must move it (proof the seam itself works).
// atomic because the daemon serves audit queries concurrently.
var backwardReads atomic.Int64

// backwardLinesDecoded counts the lines queryFileBackward hands to
// decodeAndMatch. It is the bounded-work seam: a limit-20 query against a
// 60k-line log must decode only the tail (~20-100 lines), not the file —
// the property the PERF-003 row asks the regression test to pin.
var backwardLinesDecoded atomic.Int64

// backwardGuard is the single decision point for the backward reader
// (PERF-003). The tail-shaped scan is semantically safe only when a positive
// limit bounds the work and NO time window needs the full forward scan: a
// backward read cannot know where a window's older bound sits without
// reading everything anyway, so since/until keeps the forward path.
func backwardGuard(f Filter) bool {
	return f.Limit > 0 && f.Since == nil && f.Until == nil && f.Observer == nil
}

// queryChainBackward is Query's bounded path: it walks the rotation chain
// NEWEST first (live, .1, .2, .3), reads each existing file backward, and
// stops once f.Limit matches have been collected. Each file contributes at
// most its newest matches, and older files are PREPENDED, so the result is
// exactly the chain-order (oldest-first) records the forward scan plus
// global truncation returns — the two decompositions of "the N newest
// matches" are provably the same set in the same order.
//
// Like the forward path, missing backups are skipped; a missing LIVE file is
// an error (the Verify contract).
func queryChainBackward(path string, f Filter) ([]Record, error) {
	var records []Record
	for i := 0; i <= MaxBackups; i++ {
		p := path
		if i > 0 {
			p = fmt.Sprintf("%s.%d", path, i)
		}
		if _, err := os.Stat(p); err != nil {
			if os.IsNotExist(err) {
				if i == 0 {
					return nil, pathError(p, err) // missing live file stays an error
				}
				continue
			}
			return nil, pathError(p, err)
		}
		if len(records) >= f.Limit {
			break
		}
		remaining := f.Limit - len(records)
		recs, err := queryFileBackward(p, Filter{AgentID: f.AgentID, Method: f.Method, Limit: remaining})
		if err != nil {
			// Backward read impossible for this file (unreadable,
			// non-regular, read error): fall back to the forward scan,
			// silently, truncated to the same allowance.
			recs, err = queryFile(p, Filter{AgentID: f.AgentID, Method: f.Method})
			if err != nil {
				return nil, err
			}
			if len(recs) > remaining {
				recs = recs[len(recs)-remaining:]
			}
		}
		if len(recs) > 0 {
			records = append(recs, records...) // prepend: older file goes first
		}
	}
	return records, nil
}

// queryFileBackward applies f to the parseable records of one audit file,
// reading newest-first from the end and stopping as soon as f.Limit records
// have matched (the `tail -n` shape, PERF-003). The records are returned
// oldest-first — the same order queryFile produces — so callers cannot tell
// the paths apart by their results.
//
// The reader seeks by byte offset: it reads backwardChunkSize-sized chunks
// from the end, splits each on '\n', reconstructs the line split across a
// chunk boundary by carrying the newer fragment into the older chunk, and
// parses lines newest-first, stopping at the limit. Discovery is guarded:
// an empty file returns no records; a final line with no trailing newline
// is still parsed (queryFile's Scanner yields it too); malformed lines are
// skipped exactly like the forward path.
//
// Callers must fall back to queryFile when this returns an error: the error
// means the file could not be read backward (missing, unreadable, not a
// regular file, or a read failure such as a concurrent rotation invalidating
// a stale offset). f.Limit must be > 0 (backwardGuard enforces it upstream).
func queryFileBackward(path string, f Filter) ([]Record, error) {
	backwardReads.Add(1)

	file, err := os.Open(path)
	if err != nil {
		return nil, pathError(path, err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, pathError(path, err)
	}
	// PERF-003: this reader is seek-shaped by construction — every chunk is
	// read through file.Seek (the io.Seeker interface) followed by a plain
	// Read from the seek position, never io.ReaderAt. The assertion pins the
	// io.Seeker contract in code, not just in a comment.
	if _, ok := any(file).(io.Seeker); !ok {
		return nil, fmt.Errorf("%s does not support seeking (io.Seeker)", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file") // seek-shaped read impossible
	}
	size := info.Size()
	if size == 0 {
		return nil, nil
	}

	chunk := backwardChunkSize
	if chunk <= 0 {
		chunk = 1 // defensive: a torn variable must not loop forever
	}

	// matched accumulates newest-first and is reversed once before
	// returning. pending holds the not-yet-complete OLDEST line seen so
	// far: the fragment(s) of a line whose beginning lies in an older
	// (left) chunk. A line fully inside one chunk never touches it.
	var matched []Record
	var pending []byte
	end := size
	for end > 0 && len(matched) < f.Limit {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		buf := make([]byte, end-start)
		// Seeker-backed read (PERF-003): position the io.Seeker at the
		// chunk start and fill buf with a sequential Read — no io.ReaderAt.
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			return nil, err // seek failure: fall back to the forward scan
		}
		if _, err := io.ReadFull(file, buf); err != nil {
			return nil, err // offsets stale (e.g. concurrent rotation): fall back
		}
		for len(buf) > 0 && len(matched) < f.Limit {
			i := bytes.LastIndexByte(buf, '\n')
			if i < 0 {
				// No newline left in this chunk: the whole remainder is
				// the head of one line that continues (to the right) into
				// the previously-read chunks — carry it via pending.
				pending = appendChunk(buf, pending)
				buf = nil
				break
			}
			// buf[i+1:] is the left part of one line; pending is whatever
			// of that line was already read from newer chunks. Both are
			// copied into a fresh slice so chunk buffers are not retained.
			line := appendChunk(buf[i+1:], pending)
			pending = nil
			backwardLinesDecoded.Add(1)
			if rec, ok, err := decodeAndMatch(line, f); err == nil && ok {
				matched = append(matched, rec)
			} // malformed/empty line: skipped exactly like the forward path
			buf = buf[:i]
		}
		end = start
	}
	// Reaching start-of-file with a leftover pending means the file's FIRST
	// line has no trailing newline (or the file has no newline at all): it
	// is complete, and queryFile's Scanner would yield it too. A leftover
	// pending after hitting the limit is discarded with the unread head.
	if len(matched) < f.Limit && len(pending) > 0 {
		if rec, ok, err := decodeAndMatch(pending, f); err == nil && ok {
			matched = append(matched, rec)
		}
	}
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}
	return matched, nil
}

// decodeAndMatch is the single per-line decode+filter seam shared by the
// forward queryFile and the backward queryFileBackward (PERF-003), so the
// two readers cannot diverge on what is a record or a match. It returns the
// decoded record, whether it satisfies f, and the unmarshal error (both
// paths skip the line on error).
func decodeAndMatch(line []byte, f Filter) (Record, bool, error) {
	var rec Record
	if err := json.Unmarshal(line, &rec); err != nil {
		return Record{}, false, err
	}
	return rec, match(rec, f), nil
}

// appendChunk returns a fresh slice holding left followed by right, so chunk
// buffers are never retained across iterations.
func appendChunk(left, right []byte) []byte {
	out := make([]byte, 0, len(left)+len(right))
	out = append(out, left...)
	return append(out, right...)
}
