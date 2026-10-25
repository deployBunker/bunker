package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
)

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
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, pathError(path, err)
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
		if _, err := file.ReadAt(buf, start); err != nil {
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
