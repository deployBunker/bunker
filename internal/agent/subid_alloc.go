package agent

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── Subordinate-ID allocation (GAP-140) ──────────────────────────────────────
//
// /etc/subuid and /etc/subgid hand each user a contiguous block of IDs that a
// rootless container runtime may map into a user namespace. Two users'
// namespaces are disjoint ONLY if their blocks are: sharing an ID means the
// second user's container root can own an ID the first user's container root
// also owns.
//
// Before GAP-140 every agent was handed `start = <its own host uid>` with a
// fixed width of 65536 (name:start:65536). Host uids for agents come from
// bunker's own user allocation, so any two agents whose uids differ by less
// than 65536 received overlapping blocks: uid 1001 got [1001, 66536] and uid
// 1002 got [1002, 66537], sharing 65535 IDs. That is an isolation break
// between tenants — the product's core claim — not a bookkeeping bug.
//
// This file allocates blocks from a pool that starts above the host's normal
// uid range and refuses to place a block anywhere an existing line of the
// database already occupies an ID, regardless of which name that line belongs
// to. Allocation is idempotent per name: an agent that already has a block
// keeps it, so a re-spawn never invalidates the mapping of a running container.

const (
	// subIDRangeSize is the width of one agent's subordinate ID block. 65536
	// is the docker/rootless convention and is deliberately not configurable
	// per agent: a narrower block cannot map container root.
	subIDRangeSize = 65536

	// subIDPoolBase is the first subordinate ID the allocator may hand out
	// (8 * 65536 = 524288). It sits above Debian/Ubuntu's default human uid
	// range (1000-60000) and above their rootless handling — the first
	// 65536-wide block at 100000 — so an allocation can never land on an
	// existing "user:100000:65536" line. This single constant is the knob a
	// future config option would replace; nothing else encodes the offset.
	subIDPoolBase = 8 * subIDRangeSize

	// subIDPoolLimit is the highest ID a block may contain: the subordinate-ID
	// space is unsigned 32-bit, so 4294967294 is the last usable id and
	// 4294967295 the sentinel. Practically unreachable (it would take ~65k
	// agents) but the boundary must not silently wrap.
	subIDPoolLimit = 4294967294

	// subIDTempSuffix names the sibling file an entry is written through
	// before being renamed over the database, so readers never observe a
	// partially written mapping.
	subIDTempSuffix = ".bunker-tmp"
)

// Host-wide advisory lock guarding subordinate-ID allocation.
var (
	// subIDLockDir holds the lock file. It matches the daemon's other durable
	// state (/var/lib/bunkerd/…) so it lives on a real, root-writable
	// filesystem rather than /run. Var so unit tests can point it at a temp
	// directory; production never changes it.
	subIDLockDir = "/var/lib/bunkerd"

	// subIDLockFileName is the single lock both /etc/subuid and /etc/subgid
	// are edited under (see lockSubIDs).
	subIDLockFileName = "subid-alloc.lock"

	// subIDLockTimeout bounds how long one spawn waits for the allocation
	// lock. The critical section is a few file writes, so the wait is only
	// ever as long as other spawns; the bound exists so a wedged holder
	// cannot pin a spawn slot indefinitely.
	subIDLockTimeout = 60 * time.Second

	// subIDLockRetryInterval is the retry cadence between flock attempts.
	subIDLockRetryInterval = 20 * time.Millisecond
)

// subIDEntry is one parsed line of a subordinate-ID database.
type subIDEntry struct {
	name  string
	start int64
	count int64
	line  int // 1-based line number, for refusal messages
}

// end is the first ID past the range; a range is half-open [start, end).
func (e subIDEntry) end() int64 { return e.start + e.count }

// overlaps reports whether two half-open ranges share at least one ID.
func (e subIDEntry) overlaps(other subIDEntry) bool {
	return e.start < other.end() && other.start < e.end()
}

// subIDPlan is the allocator's decision for one database file.
type subIDPlan struct {
	path   string
	entry  subIDEntry // the block this name has (existing) or is being given
	append bool       // false when the file already carries a usable entry
}

// subIDPoolExhaustedError reports that no unoccupied 65536-wide block remains
// inside the pool. It is a REFUSAL, never a fallback to reusing another user's
// IDs: the caller fails the spawn.
type subIDPoolExhaustedError struct {
	path  string
	name  string
	first int64
	last  int64
}

func (e *subIDPoolExhaustedError) Error() string {
	return fmt.Sprintf("%s: no disjoint %d-ID range available for %q in the pool [%d..%d]: "+
		"every block is occupied by an existing entry — refusing to reuse another user's subordinate IDs "+
		"(widen the pool or remove stale entries)", e.path, subIDRangeSize, e.name, e.first, e.last)
}

// parseSubIDEntries parses a subordinate-ID database. Every non-blank,
// non-comment line must be `name:start:count` with numeric start/count; any
// other shape is refused with the file, line number and offending text, because
// a line the allocator cannot account for is a line whose IDs it cannot prove
// unoccupied. Blank lines and `#` comments carry no mapping and are skipped.
func parseSubIDEntries(data []byte, path string) ([]subIDEntry, error) {
	lines := strings.Split(string(data), "\n")
	entries := make([]subIDEntry, 0, len(lines))
	for i, raw := range lines {
		lineNo := i + 1
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Split(trimmed, ":")
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s:%d: malformed subordinate-id line %q: want name:start:count",
				path, lineNo, trimmed)
		}
		if fields[0] == "" {
			return nil, fmt.Errorf("%s:%d: malformed subordinate-id line %q: empty name",
				path, lineNo, trimmed)
		}
		start, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || start < 0 {
			return nil, fmt.Errorf("%s:%d: malformed subordinate-id line %q: start %q is not a non-negative integer",
				path, lineNo, trimmed, fields[1])
		}
		count, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || count < 0 {
			return nil, fmt.Errorf("%s:%d: malformed subordinate-id line %q: count %q is not a non-negative integer",
				path, lineNo, trimmed, fields[2])
		}
		entries = append(entries, subIDEntry{name: fields[0], start: start, count: count, line: lineNo})
	}
	return entries, nil
}

// hasSubIDEntry reports whether the database text already carries a line for
// name. Lenient by design: it is the idempotence guard of the write primitive,
// not the allocator's validator (parseSubIDEntries is).
func hasSubIDEntry(data []byte, name string) bool {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) >= 1 && fields[0] == name {
			return true
		}
	}
	return false
}

// chooseSubIDRangeStart returns the first 65536-wide block inside the pool that
// shares no ID with any entry, or false when the pool has no room left.
// Occupants are visited in ascending order, so the first gap wide enough to
// hold a block wins and no earlier candidate can be skipped.
func chooseSubIDRangeStart(entries []subIDEntry) (int64, bool) {
	occupied := make([]subIDEntry, 0, len(entries))
	for _, e := range entries {
		if e.count > 0 {
			occupied = append(occupied, e)
		}
	}
	sort.Slice(occupied, func(i, j int) bool {
		if occupied[i].start != occupied[j].start {
			return occupied[i].start < occupied[j].start
		}
		return occupied[i].end() < occupied[j].end()
	})

	cand := int64(subIDPoolBase)
	for _, u := range occupied {
		if u.end() <= cand {
			continue // entirely below the candidate
		}
		if u.start >= cand+subIDRangeSize {
			break // the block fits in the gap in front of this occupant
		}
		cand = u.end() // jump past the occupant and look again
	}
	if cand < int64(subIDPoolBase) || cand+subIDRangeSize-1 > subIDPoolLimit {
		return 0, false
	}
	return cand, true
}

// planSubIDAllocation decides what must be appended to one database file for
// name. Three outcomes, in order:
//
//  1. name already has an entry: it is REUSED untouched (idempotent re-spawn),
//     unless it now overlaps another name's entry — then the spawn is refused
//     naming both users, because two tenants already share an ID space.
//  2. name is absent: the first unoccupied block in the pool is chosen.
//  3. no block fits, or any line is malformed: refuse.
func planSubIDAllocation(data []byte, path, name string) (subIDPlan, error) {
	entries, err := parseSubIDEntries(data, path)
	if err != nil {
		return subIDPlan{}, err
	}

	var mine []subIDEntry
	for _, e := range entries {
		if e.name == name {
			mine = append(mine, e)
		}
	}
	if len(mine) > 0 {
		for _, m := range mine {
			for _, other := range entries {
				if other.name == name {
					// A duplicate line for this same name is this agent's own
					// mapping, not a cross-tenant overlap.
					continue
				}
				if m.overlaps(other) {
					return subIDPlan{}, fmt.Errorf("%s:%d: entry for %q [%d..%d] overlaps entry for %q [%d..%d] (line %d): "+
						"two users share subordinate IDs — refusing to spawn on a host with an isolation break; "+
						"resolve the overlap (e.g. reassign one user's range) before retrying",
						path, m.line, m.name, m.start, m.end()-1, other.name, other.start, other.end()-1, other.line)
				}
			}
		}
		// Reuse verbatim: a running container's mapping must not move.
		return subIDPlan{path: path, entry: mine[0]}, nil
	}

	start, ok := chooseSubIDRangeStart(entries)
	if !ok {
		return subIDPlan{}, &subIDPoolExhaustedError{
			path:  path,
			name:  name,
			first: subIDPoolBase,
			last:  subIDPoolLimit,
		}
	}
	return subIDPlan{
		path:   path,
		entry:  subIDEntry{name: name, start: start, count: subIDRangeSize},
		append: true,
	}, nil
}

// readSubIDFile returns the raw content of a subordinate-ID database. A missing
// file is an empty database (the kernel treats it as "no mappings"), but any
// OTHER error — a directory, a permission problem, an I/O failure — is
// returned so the caller fails closed instead of allocating against content it
// cannot read.
func readSubIDFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}

// writeSubIDEntry writes existing plus one new entry through a sibling temp
// file and renames it over path, so a reader (or a crash) never observes a
// half-written database. The original file's permission bits are preserved; a
// file that does not exist yet is created 0644, the mode shadow ships /etc/subuid
// with. The caller must hold the allocation lock.
func writeSubIDEntry(path string, existing []byte, e subIDEntry) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}

	buf := make([]byte, 0, len(existing)+64)
	buf = append(buf, existing...)
	// A database whose last line lacks a newline would otherwise have the new
	// entry fused onto it, producing exactly the malformed line the allocator
	// refuses to read later.
	if len(buf) > 0 && buf[len(buf)-1] != '\n' {
		buf = append(buf, '\n')
	}
	buf = append(buf, e.name...)
	buf = append(buf, ':')
	buf = strconv.AppendInt(buf, e.start, 10)
	buf = append(buf, ':')
	buf = strconv.AppendInt(buf, e.count, 10)
	buf = append(buf, '\n')

	tmp := path + subIDTempSuffix
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("open %s: %w", tmp, err)
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s to %s: %w", tmp, path, err)
	}
	return nil
}

// ensureSubIDAllocation gives name a subordinate-ID block in the database at
// path: the existing one if it has one, otherwise the first free block in the
// pool. It is the policy entry point (strict parse, overlap refusal, pool
// exhaustion) and must run with the allocation lock held.
func ensureSubIDAllocation(path, name string) error {
	data, err := readSubIDFile(path)
	if err != nil {
		return err
	}
	plan, err := planSubIDAllocation(data, path, name)
	if err != nil {
		return err
	}
	if !plan.append {
		return nil
	}
	return writeSubIDEntry(path, data, plan.entry)
}

// ensureSubIDEntry appends a single mapping line to path when no mapping for
// name exists. The mapping is name:start:65536. This is the write primitive
// (atomic, idempotent); validity and overlap policy live in
// planSubIDAllocation, which every spawn runs first.
func ensureSubIDEntry(path, name string, start int) error {
	data, err := readSubIDFile(path)
	if err != nil {
		return err
	}
	if hasSubIDEntry(data, name) {
		return nil // already configured
	}
	return writeSubIDEntry(path, data, subIDEntry{name: name, start: int64(start), count: subIDRangeSize})
}
