package fsclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// BFS-031, the second half: the mount's OWN state, and its own bound.
//
// The cache directory holds the cache (layout.go). This file is about the two
// files that are NOT the cache and cannot be squeezed into the cache's bound:
//
//   - status.json is the mount's self-description, rewritten on a 1 s cadence.
//     A fixed schema, but a document: measured at 7,243 B on the live route
//     while this row was written, and 4,354 B on BFS-045's. Under a 1 KiB cache
//     bound there is no version of it that fits, so it does not live there.
//   - conflicts.jsonl is the refusal log: appended once per refused write, so it
//     grows with EDIT VOLUME rather than with tree size. That is the growth
//     direction nobody was watching, and it has no size of its own to fit —
//     only a bound.
//
// Both are BOUNDED here, each by a declared cap that is ENFORCED where the bytes
// are written and REPORTED where the owner reads:
//
//   - the refusal log is capped at ConflictsMaxBytes, enforced by DROPPING THE
//     OLDEST ENTRIES, with the number dropped kept durably beside it, so the log
//     stays a record rather than becoming a truncation nobody can see; one
//     entry's `detail` is capped at ConflictDetailMaxBytes first, so a single
//     server response can never make a line exceed the whole log's cap;
//   - the status document is capped at StatusMaxBytes, enforced by capping the
//     oversized string fields it carries (with the marker in the text) and, if
//     that is still not enough, by omitting optional passthrough blocks — with
//     the reduction NAMED in the document itself (Reduced/ReducedReason),
//     because a document that silently stops describing the mount is worse than
//     one that says it had to.
//
// And the whole local footprint is reported: the cache directory's bytes, the
// state's bytes, the write-buffer spills, and their sum beside its bound. Every
// byte the client writes is inside one of those bounds, and the record says
// which. Nothing is excluded by omission.
// ---------------------------------------------------------------------------

const (
	// StateMaxBytes is the bound of the mount's state: the log's cap plus the
	// document's cap. It is what `state.max_bytes` reports.
	// (Computed by StateMaxBytes() below, so a test can drive a small cap.)
	// ConflictsDroppedFile is the durable count of log entries the bound forced
	// out. It is what keeps a bounded log a RECORD: entries are dropped, the
	// number that were dropped is not.
	ConflictsDroppedFile = "conflicts.dropped"
	// WriteBufPrefix names one open write handle's spill file inside the mount
	// directory. Those bytes are neither cache bytes nor state bytes, and they
	// are counted here so the footprint's classes add up.
	WriteBufPrefix = "writebuf-"
)

// The state's declared bounds. They are VARIABLES for the same reason
// DirMeasureTTL is one: a test must be able to drive a small cap rather than
// write 4 MiB of refusals to reach it. Nothing in the running client writes
// them.
var (
	// StatusMaxBytes bounds the status document. 64 KiB is ~10x the measured
	// live document (7,243 B): the cap exists so that an unbounded field (a
	// server's own sentence, a path) cannot turn the document into the
	// local-storage growth this row is about — not to squeeze the document
	// anyone actually reads.
	StatusMaxBytes int64 = 64 << 10
	// StatusStringMaxBytes caps ONE string field of the document. The schema's
	// string fields are fixed in number, so capping each one bounds the document
	// by construction as well as by the check at write time.
	StatusStringMaxBytes = 2 << 10
	// ConflictsMaxBytes bounds the refusal log.
	ConflictsMaxBytes int64 = 4 << 20
	// ConflictDetailMaxBytes bounds ONE log entry's detail: the server's own
	// error body. Bounded here so that a single refused write can never produce
	// a line larger than the log's own bound, which would leave the bound with
	// no way to hold anything.
	ConflictDetailMaxBytes int64 = 4 << 10
)

// StateMaxBytes is the bound of the mount's state: the refusal log's cap plus
// the status document's cap.
func StateMaxBytes() int64 { return ConflictsMaxBytes + StatusMaxBytes }

// StateStats is the mount's own state, measured, beside its bound. Every figure
// is an independent measurement of the filesystem (a handful of stat calls), for
// the same reason CacheStats.DirBytes is one — a figure that agrees only with
// itself is what BFS-031 was.
type StateStats struct {
	// Bytes is the mount directory's own bytes: the state files plus the
	// write-buffer spills. It EXCLUDES the cache directory, whose bytes are
	// `cache.dir_bytes` and whose bound is `cache.max_bytes`.
	Bytes int64 `json:"bytes"`
	// MaxBytes is the declared bound of this state (StateMaxBytes).
	MaxBytes int64 `json:"max_bytes"`
	// StatusBytes/StatusMaxBytes: the status document, measured and bounded.
	StatusBytes    int64 `json:"status_bytes"`
	StatusMaxBytes int64 `json:"status_max_bytes"`
	// Conflicts*: the refusal log, measured, bounded, and with the count of
	// entries its bound forced out — a bounded log that hid the loss would be
	// the same defect one file over.
	ConflictsBytes        int64 `json:"conflicts_bytes"`
	ConflictsMaxBytes     int64 `json:"conflicts_max_bytes"`
	ConflictsDroppedTotal int64 `json:"conflicts_dropped_total"`
	// SpillBytes is what the write path's per-handle buffers hold on disk:
	// bounded per handle by --write-buffer-max-bytes, reported here in aggregate
	// so the footprint's classes are complete.
	SpillBytes int64 `json:"spill_bytes"`
	// FootprintBytes is the mount's WHOLE local footprint for this endpoint: the
	// cache directory plus this state. FootprintMaxBytes is its bound: the two
	// bounds plus the spill budget the open handles imply, which is why the two
	// are reported separately as well.
	FootprintBytes    int64 `json:"footprint_bytes"`
	FootprintMaxBytes int64 `json:"footprint_max_bytes"`
	// Reason explains an absent measurement (a directory that could not be
	// read), from the closed four-class vocabulary. Empty means measured.
	Reason string `json:"reason,omitempty"`
}

// MeasureState measures the mount's own state. cacheBytes/cacheMaxBytes are the
// cache directory's measured bytes and its bound (already reported as
// `cache.dir_bytes`/`cache.max_bytes`); spillBudget is the write-buffer budget
// the mount has in force (open buffered handles × the per-handle bound).
//
// The walk is deliberately not memoised: it stats three or four files, and a
// figure with no age beside it must be a figure measured now.
func MeasureState(mountDir string, cacheBytes, cacheMaxBytes, spillBudget int64) StateStats {
	st := StateStats{
		MaxBytes:          StateMaxBytes(),
		StatusMaxBytes:    StatusMaxBytes,
		ConflictsMaxBytes: ConflictsMaxBytes,
		FootprintMaxBytes: cacheMaxBytes + StateMaxBytes() + spillBudget,
	}
	if mountDir == "" {
		st.Reason = ReasonUnknown + ": the mount has no directory, so its state has no source to measure"
		return st
	}
	entries, err := os.ReadDir(mountDir)
	if err != nil {
		// Absent WITH A REASON: never a zero that reads as "the state is empty".
		st.Reason = fmt.Sprintf("%s: the mount directory %s could not be read: %v", ReasonUnknown, mountDir, err)
		return st
	}
	for _, e := range entries {
		if e.IsDir() {
			// The cache directory (and anything else nested) is measured by its
			// own walk and reported as `cache.dir_bytes`; counting it here would
			// double it.
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		name, size := e.Name(), info.Size()
		switch {
		case name == StatusFile, strings.HasPrefix(name, StatusFile+"."):
			st.StatusBytes += size
			st.Bytes += size
		case name == ConflictsFile:
			st.ConflictsBytes += size
			st.Bytes += size
		case strings.HasPrefix(name, WriteBufPrefix):
			st.SpillBytes += size
			st.Bytes += size
		default:
			st.Bytes += size
		}
	}
	st.ConflictsDroppedTotal = ReadConflictsDropped(mountDir)
	st.FootprintBytes = cacheBytes + st.Bytes
	return st
}

// ---------------------------------------------------------------------------
// The refusal log's bound.
// ---------------------------------------------------------------------------

// ReadConflictsDropped reports how many log entries the bound has forced out.
// Zero when the count file is absent: nothing has been dropped, which is a
// measurement rather than an absence.
func ReadConflictsDropped(dir string) int64 {
	raw, err := os.ReadFile(filepath.Join(dir, ConflictsDroppedFile))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return n
}

// rotateConflictLog rewrites the log with only the newest entries that fit
// `incoming` bytes of headroom under the cap, and returns how many it dropped.
// The newest entry is always kept even if it alone would pass the cap: at that
// point the per-entry detail cap has failed and a log that records nothing is
// worse than one over its bound. The per-entry cap is what makes that
// unreachable in practice.
func rotateConflictLog(path string, incoming int64) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	lines := splitLogLines(raw)
	budget := ConflictsMaxBytes - incoming
	kept := make([][]byte, 0, len(lines))
	var used int64
	for i := len(lines) - 1; i >= 0; i-- {
		n := int64(len(lines[i])) + 1
		if used+n > budget && len(kept) > 0 {
			break
		}
		kept = append(kept, lines[i])
		used += n
	}
	if len(kept) >= len(lines) {
		return 0, nil
	}
	dropped := int64(len(lines) - len(kept))
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	var buf []byte
	for _, l := range kept {
		buf = append(buf, l...)
		buf = append(buf, '\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return dropped, nil
}

// splitLogLines splits a log body into non-empty lines, without the newlines.
func splitLogLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range raw {
		if b != '\n' {
			continue
		}
		if i > start {
			out = append(out, raw[start:i])
		}
		start = i + 1
	}
	if start < len(raw) {
		out = append(out, raw[start:])
	}
	return out
}

// addConflictsDropped adds to the durable dropped count.
func addConflictsDropped(dir string, n int64) error {
	path := filepath.Join(dir, ConflictsDroppedFile)
	var have int64
	if raw, err := os.ReadFile(path); err == nil {
		have, _ = strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	}
	return os.WriteFile(path, []byte(strconv.FormatInt(have+n, 10)+"\n"), 0o600)
}

// capConflictDetail enforces the per-entry detail cap, leaving the marker in the
// text so a reader of the log sees a truncated detail rather than a sentence
// that happens to stop.
func capConflictDetail(c *Conflict) {
	if int64(len(c.Detail)) <= ConflictDetailMaxBytes {
		return
	}
	over := int64(len(c.Detail)) - ConflictDetailMaxBytes
	c.Detail = fmt.Sprintf("%s…[truncated %d bytes: the refusal log's per-entry detail cap]",
		c.Detail[:ConflictDetailMaxBytes], over)
}

// ---------------------------------------------------------------------------
// The status document's bound.
// ---------------------------------------------------------------------------

// optionalStatusBlocks are the blocks the reduction drops first, in order: the
// server's own passthrough document (re-readable from the server), the class
// split (derivable from the totals), the most recent conflict's detail (the log
// holds the entries themselves), and the snapshot census.
var optionalStatusBlocks = []string{
	"invalidation.server",
	"cache.dir_bytes_by_class",
	"conflicts.last.detail",
	"snapshot",
}

// capStatusStrings caps every string field of a decoded status document at
// maxBytes, returning the rebuilt value and how many fields it capped.
//
// It works on the ENCODED document (the schema's own shape, not this build's
// struct tags) deliberately: a field added later is bounded by this pass without
// anyone remembering to come back here — the failure mode a hand-written list of
// "the big fields" has.
func capStatusStrings(v any, maxBytes int, capped *int) any {
	switch tv := v.(type) {
	case map[string]any:
		for k, sub := range tv {
			tv[k] = capStatusStrings(sub, maxBytes, capped)
		}
		return tv
	case []any:
		for i, sub := range tv {
			tv[i] = capStatusStrings(sub, maxBytes, capped)
		}
		return tv
	case string:
		if len(tv) > maxBytes {
			*capped++
			return fmt.Sprintf("%s…[truncated %d bytes: the status document's per-field cap]",
				tv[:maxBytes], len(tv)-maxBytes)
		}
		return tv
	}
	return v
}

// dropOptionalBlock removes the first block that is present and whose parent
// exists, and reports its path.
func dropOptionalBlock(doc map[string]any, order []string) (string, bool) {
	for _, path := range order {
		parts := strings.Split(path, ".")
		parent := doc
		ok := true
		for _, p := range parts[:len(parts)-1] {
			next, isMap := parent[p].(map[string]any)
			if !isMap {
				ok = false
				break
			}
			parent = next
		}
		if !ok {
			continue
		}
		last := parts[len(parts)-1]
		if _, present := parent[last]; !present {
			continue
		}
		delete(parent, last)
		return path, true
	}
	return "", false
}

// encodeStatus renders a status document the one way the record is rendered, so
// the size the bound is checked against is the size that lands on disk.
func encodeStatus(st Status) ([]byte, error) {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// reducedStatusDoc is the fallback document: identity, the cap, and the reason.
// It is built as a MAP rather than by zeroing the struct on purpose — a zeroed
// Status still renders every block of the schema (~4.5 KB indented), so a
// "reduce to nothing" that rendered the struct could itself pass the cap it was
// trying to satisfy.
//
// It is a valid document for every consumer: the fields it carries are the ones
// the reader needs to know WHICH mount this is and why the figures are missing.
func reducedStatusDoc(st Status, original int) map[string]any {
	return map[string]any{
		"mount":      st.Mount,
		"mode":       st.Mode,
		"endpoint":   st.Endpoint,
		"mountpoint": st.Mountpoint,
		"reduced":    true,
		"reduced_reason": fmt.Sprintf("%s: the status document was %d bytes and could not be reduced under the declared cap %d (this client's smallest document is larger than that); the mount is running, and this notice is shown instead of figures that would not fit",
			ReasonUnknown, original, StatusMaxBytes),
		"updated_ms": timeNow().UnixMilli(),
	}
}
