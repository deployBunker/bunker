package agent

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Subordinate-id integrity checks and remediation (GAP-140 / SEC-22).
//
// planSubIDAllocation already REFUSES a spawn that would collide (in
// subid_alloc.go). These two helpers cover the two cases a spawn-time refusal
// cannot:
//
//   - CheckSubIDOverlaps: a fail-closed STARTUP gate, so a daemon does not run
//     against a host whose /etc/subuid or /etc/subgid already contains an
//     overlap (e.g. written by an older build, or by hand).
//   - MigrateSubIDPath: a remediation that rewrites ONLY managed agent entries
//     (username prefix "bunker-") that overlap another entry or sit below the
//     pool, assigning each a fresh disjoint block and preserving every other
//     line verbatim.

// subIDOverlaps returns the first overlapping pair of *distinct* users in
// entries, or nil. Two lines naming the same user are that user's own mapping
// (a duplicate), which the allocator treats as one agent's entry — not a
// cross-tenant overlap — so they are not reported here.
func subIDOverlaps(entries []subIDEntry) (subIDEntry, subIDEntry, bool) {
	sorted := make([]subIDEntry, 0, len(entries))
	for _, e := range entries {
		if e.count > 0 {
			sorted = append(sorted, e)
		}
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].start != sorted[j].start {
			return sorted[i].start < sorted[j].start
		}
		return sorted[i].end() < sorted[j].end()
	})
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].start >= sorted[i].end() {
				break // sorted by start: nothing further can overlap sorted[i]
			}
			if sorted[i].name == sorted[j].name {
				continue // same user, duplicate line
			}
			return sorted[i], sorted[j], true
		}
	}
	return subIDEntry{}, subIDEntry{}, false
}

// VerifySubIDPath parses path and returns an error naming the first
// cross-tenant overlap, or nil. A missing file is clean; a malformed line is
// reported (it is a line whose ids cannot be proven unoccupied).
func VerifySubIDPath(path string) error {
	data, err := readSubIDFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	entries, err := parseSubIDEntries(data, path)
	if err != nil {
		return err
	}
	if a, b, ok := subIDOverlaps(entries); ok {
		return fmt.Errorf("%s: subordinate-id ranges overlap: %q [%d..%d] and %q [%d..%d]",
			path, a.name, a.start, a.end()-1, b.name, b.start, b.end()-1)
	}
	return nil
}

// CheckSubIDOverlaps verifies both subordinate-id databases. It backs the
// daemon's fail-closed startup gate: a host with an existing overlap must not
// run bunkerd, because the isolation guarantee between agents would be false.
func CheckSubIDOverlaps() error {
	for _, p := range []string{subUIDPath, subGIDPath} {
		if err := VerifySubIDPath(p); err != nil {
			return err
		}
	}
	return nil
}

// MigrateSubIDPath rewrites the managed agent entries (username prefix
// "bunker-") in path that overlap another entry or start below the pool, giving
// each a fresh disjoint block. Non-managed users and malformed/comment lines
// are preserved verbatim. It returns the number of entries rewritten. The whole
// read-plan-write runs under the same host-wide allocation lock concurrent
// spawns take.
func MigrateSubIDPath(path string) (int, error) {
	var moved int
	release, err := lockSubIDs()
	if err != nil {
		return 0, err
	}
	defer release()

	data, err := readSubIDFile(path)
	if err != nil {
		return 0, err
	}
	entries, err := parseSubIDEntries(data, path)
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return 0, nil
	}

	lines := strings.Split(string(data), "\n")
	// A trailing newline yields a final empty element; keep the original shape.
	hadTrailingNewline := strings.HasSuffix(string(data), "\n")
	if hadTrailingNewline {
		lines = lines[:len(lines)-1]
	}

	// held = every range that is NOT a managed agent's, plus managed ranges
	// already validated as disjoint. Managed entries are examined in file
	// order; each rewrite appends its new range to held so later ones avoid it.
	var held []subIDEntry
	for _, e := range entries {
		if isManagedAgentName(e.name) {
			continue
		}
		held = append(held, e)
	}

	// Pass 1: which managed entries must move? An entry moves if it starts
	// below the pool or overlaps a held range.
	type move struct {
		lineIdx int
		entry   subIDEntry
		newStar int64
	}
	var moves []move
	stayRanges := make([]subIDEntry, 0, len(entries))
	for _, e := range entries {
		if !isManagedAgentName(e.name) {
			continue
		}
		needMove := e.start < int64(subIDPoolBase)
		if !needMove {
			for _, h := range held {
				if e.overlaps(h) {
					needMove = true
					break
				}
			}
		}
		if needMove {
			moves = append(moves, move{lineIdx: e.line - 1, entry: e})
		} else {
			stayRanges = append(stayRanges, e)
		}
	}
	if len(moves) == 0 {
		return 0, nil
	}

	// Held set for allocation starts from non-managed + staying managed ranges.
	allocHeld := append([]subIDEntry{}, held...)
	allocHeld = append(allocHeld, stayRanges...)
	for i := range moves {
		start, ok := chooseSubIDRangeStart(allocHeld)
		if !ok {
			return 0, &subIDPoolExhaustedError{
				path: path, name: moves[i].entry.name,
				first: int64(subIDPoolBase), last: int64(subIDPoolLimit),
			}
		}
		moves[i].newStar = start
		allocHeld = append(allocHeld, subIDEntry{name: moves[i].entry.name, start: start, count: subIDRangeSize})
		moved++
	}

	for _, m := range moves {
		if m.lineIdx < 0 || m.lineIdx >= len(lines) {
			return 0, fmt.Errorf("%s: internal line index %d out of range", path, m.lineIdx)
		}
		lines[m.lineIdx] = fmt.Sprintf("%s:%d:%d", m.entry.name, m.newStar, subIDRangeSize)
	}

	content := strings.Join(lines, "\n")
	if hadTrailingNewline {
		content += "\n"
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := path + subIDTempSuffix
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("rename %s to %s: %w", tmp, path, err)
	}
	return moved, nil
}

// isManagedAgentName reports whether name belongs to a Bunker-managed agent
// user ("bunker-<id>"). Only these entries are ever rewritten by a migration.
func isManagedAgentName(name string) bool {
	return strings.HasPrefix(strings.TrimSpace(name), managedAgentPrefix)
}

// managedAgentPrefix is the username prefix Bunker gives every agent user.
const managedAgentPrefix = "bunker-"

// MigrateSubIDs is the CLI's entry point: it migrates both databases and
// returns the total number of ranges rewritten.
func MigrateSubIDs() (int, error) {
	total := 0
	for _, p := range []string{subUIDPath, subGIDPath} {
		n, err := MigrateSubIDPath(p)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
