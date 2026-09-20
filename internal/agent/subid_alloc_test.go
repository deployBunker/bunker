package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ── GAP-140 acceptance suite ────────────────────────────────────────────────
//
// SEC-22: every pair of agents received OVERLAPPING subordinate-ID ranges
// because start was the agent's own uid (uid 1001 -> [1001..66536], uid 1002 ->
// [1002..66537]; 65535 shared ids), so one agent's rootless dockerd could map
// container UIDs another agent's dockerd already maps. The tests below pin the
// three properties the fix must have: ranges come from a global pool and never
// overlap, allocation is serialized by a host-wide lock so concurrent spawns
// cannot race into the same block, and every failure mode (malformed database,
// no room, two names already sharing ids) REFUSES the spawn with a message
// naming the users rather than silently reusing or truncating a mapping.

// subIDTestHost points every seam the allocation path touches at a temp host:
// the two databases and the lock directory.
type subIDTestHost struct {
	uidPath string
	gidPath string
	lockDir string
}

func newSubIDTestHost(t *testing.T) *subIDTestHost {
	t.Helper()
	dir := t.TempDir()
	h := &subIDTestHost{
		uidPath: filepath.Join(dir, "subuid"),
		gidPath: filepath.Join(dir, "subgid"),
		lockDir: filepath.Join(dir, "lock"),
	}
	h.swap(t, &subUIDPath, h.uidPath)
	h.swap(t, &subGIDPath, h.gidPath)
	h.swap(t, &subIDLockDir, h.lockDir)
	return h
}

func (h *subIDTestHost) swap(t *testing.T, target *string, value string) {
	t.Helper()
	prev := *target
	*target = value
	t.Cleanup(func() { *target = prev })
}

// seed writes both databases with the given content ("" = empty file).
func (h *subIDTestHost) seed(t *testing.T, subuid, subgid string) {
	t.Helper()
	if err := os.WriteFile(h.uidPath, []byte(subuid), 0o644); err != nil {
		t.Fatalf("seed subuid: %v", err)
	}
	if err := os.WriteFile(h.gidPath, []byte(subgid), 0o644); err != nil {
		t.Fatalf("seed subgid: %v", err)
	}
}

func (h *subIDTestHost) readUID(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(h.uidPath)
	if err != nil {
		t.Fatalf("read subuid: %v", err)
	}
	return string(data)
}

// entriesOf parses one database for the disjointness assertions.
func entriesOf(t *testing.T, path string) []subIDEntry {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		t.Fatalf("stat %s: %v", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	entries, err := parseSubIDEntries(data, path)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return entries
}

// assertPairwiseDisjoint fails when any two entries in the file share an id.
// It also asserts every entry is the full 65536-wide block, because a narrower
// block cannot map container root and would be a different defect.
func assertPairwiseDisjoint(t *testing.T, path string) {
	t.Helper()
	entries := entriesOf(t, path)
	if len(entries) == 0 {
		t.Fatalf("%s carries no entries", path)
	}
	for i, a := range entries {
		if a.count != subIDRangeSize {
			t.Errorf("%s: entry %q width = %d, want %d", path, a.name, a.count, subIDRangeSize)
		}
		for _, b := range entries[i+1:] {
			if a.overlaps(b) {
				t.Errorf("%s: %q [%d..%d] OVERLAPS %q [%d..%d]",
					path, a.name, a.start, a.end()-1, b.name, b.start, b.end()-1)
			}
		}
	}
}

// stubSubIDUser points userLookup at a fixed uid/gid so the spawn's allocation
// step is drivable without real system users.
func stubSubIDUser(t *testing.T, username string, uid int) {
	t.Helper()
	prev := userLookup
	userLookup = func(name string) (*user.User, error) {
		if name != username {
			return nil, user.UnknownUserError(name)
		}
		return &user.User{Username: name, Uid: strconv.Itoa(uid), Gid: strconv.Itoa(uid)}, nil
	}
	t.Cleanup(func() { userLookup = prev })
}

// ── allocator decisions (pure, table-driven) ────────────────────────────────

func TestPlanSubIDAllocation_Table(t *testing.T) {
	const (
		base = int64(subIDPoolBase)
		one  = int64(subIDRangeSize)
	)
	tiled := func() string {
		// Two entries that together tile the pool, leaving no 65536-wide gap.
		half := (int64(subIDPoolLimit) - base + 1) / 2
		return fmt.Sprintf("hog1:%d:%d\nhog2:%d:%d\n", base, half, base+half, half)
	}()

	tests := []struct {
		name      string
		content   string
		allocName string
		wantStart int64
		wantAppnd bool
		wantErr   string // substring; empty means no error
	}{
		{
			name:      "empty database starts at the pool base",
			content:   "",
			allocName: "bunker-a",
			wantStart: base,
			wantAppnd: true,
		},
		{
			name:      "foreign rootless entry below the pool is not overlapped",
			content:   "dockremap:100000:65536\n",
			allocName: "bunker-a",
			wantStart: base,
			wantAppnd: true,
		},
		{
			name:      "human entries below the pool are not overlapped",
			content:   "root:0:65536\nkara:100000:65536\n",
			allocName: "bunker-a",
			wantStart: base,
			wantAppnd: true,
		},
		{
			name:      "base block taken by another name moves to the next block",
			content:   fmt.Sprintf("bunker-x:%d:%d\n", base, one),
			allocName: "bunker-a",
			wantStart: base + one,
			wantAppnd: true,
		},
		{
			name: "a gap in the middle is filled instead of appending after the highest entry",
			content: fmt.Sprintf("bunker-x:%d:%d\nbunker-y:%d:%d\n",
				base, one, base+3*one, one),
			allocName: "bunker-a",
			wantStart: base + one,
			wantAppnd: true,
		},
		{
			name:      "an entry spanning INTO the pool pushes the candidate past it",
			content:   fmt.Sprintf("wide:%d:%d\n", base-one, 3*one),
			allocName: "bunker-a",
			wantStart: base + 2*one,
			wantAppnd: true,
		},
		{
			name:      "existing entry for this name is reused verbatim",
			content:   fmt.Sprintf("bunker-a:%d:%d\n", base+5*one, one),
			allocName: "bunker-a",
			wantStart: base + 5*one,
			wantAppnd: false,
		},
		{
			name:      "empty and comment lines are skipped",
			content:   "# managed by bunker\n\ndockremap:100000:65536\n\n",
			allocName: "bunker-a",
			wantStart: base,
			wantAppnd: true,
		},
		{
			name:      "malformed line refuses the allocation",
			content:   "not-a-mapping\n",
			allocName: "bunker-a",
			wantErr:   "malformed subordinate-id line",
		},
		{
			name:      "non-numeric start refuses instead of guessing",
			content:   "alpha:abc:65536\n",
			allocName: "bunker-a",
			wantErr:   "not a non-negative integer",
		},
		{
			name:      "empty start refuses",
			content:   "alpha::65536\n",
			allocName: "bunker-a",
			wantErr:   "not a non-negative integer",
		},
		{
			name:      "negative count refuses",
			content:   "alpha:100000:-1\n",
			allocName: "bunker-a",
			wantErr:   "not a non-negative integer",
		},
		{
			name:      "empty name refuses",
			content:   ":100000:65536\n",
			allocName: "bunker-a",
			wantErr:   "empty name",
		},
		{
			name:      "four-field line refuses",
			content:   "alpha:100000:65536:extra\n",
			allocName: "bunker-a",
			wantErr:   "malformed subordinate-id line",
		},
		{
			name:      "a same-name entry overlapping another name refuses, naming both users",
			content:   "bunker-b:100000:65536\nbunker-a:120000:65536\n",
			allocName: "bunker-a",
			wantErr:   "overlaps entry for \"bunker-b\"",
		},
		{
			name:      "a same-name entry overlapping a foreign entry refuses too",
			content:   "dockremap:100000:65536\nbunker-a:100001:65536\n",
			allocName: "bunker-a",
			wantErr:   "overlaps entry for \"dockremap\"",
		},
		{
			name:      "pool fully occupied by one wide entry refuses as exhausted",
			content:   fmt.Sprintf("hog:%d:%d\n", base, int64(subIDPoolLimit)-base+1),
			allocName: "bunker-a",
			wantErr:   "no disjoint",
		},
		{
			name:      "every block occupied refuses as exhausted",
			content:   tiled,
			allocName: "bunker-a",
			wantErr:   "no disjoint",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "subuid")
			plan, err := planSubIDAllocation([]byte(tt.content), path, tt.allocName)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got plan %+v", tt.wantErr, plan)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if plan.entry.start != tt.wantStart {
				t.Errorf("start = %d, want %d", plan.entry.start, tt.wantStart)
			}
			if plan.entry.count != subIDRangeSize {
				t.Errorf("count = %d, want %d", plan.entry.count, subIDRangeSize)
			}
			if plan.append != tt.wantAppnd {
				t.Errorf("append = %v, want %v", plan.append, tt.wantAppnd)
			}
			if plan.entry.name != tt.allocName {
				t.Errorf("name = %q, want %q", plan.entry.name, tt.allocName)
			}
			// The headline property, asserted on every accepted plan: the range
			// this name is about to receive shares no id with ANY existing entry.
			if tt.wantAppnd {
				entries, perr := parseSubIDEntries([]byte(tt.content), path)
				if perr != nil {
					t.Fatalf("fixture does not parse: %v", perr)
				}
				for _, e := range entries {
					if plan.entry.overlaps(e) {
						t.Errorf("planned range [%d..%d] overlaps existing %q [%d..%d]",
							plan.entry.start, plan.entry.end()-1, e.name, e.start, e.end()-1)
					}
				}
			}
		})
	}
}

// TestChooseSubIDRangeStart_WalksGapsAscending pins the pool search order: the
// FIRST gap wide enough wins, so allocation is deterministic and fills holes
// rather than marching to the end of the file.
func TestChooseSubIDRangeStart_WalksGapsAscending(t *testing.T) {
	base := int64(subIDPoolBase)
	one := int64(subIDRangeSize)
	entries := []subIDEntry{
		{name: "high", start: base + 10*one, count: one},
		{name: "low", start: base + 2*one, count: one}, // deliberately out of order
		{name: "below-pool", start: 100000, count: one},
	}
	got, ok := chooseSubIDRangeStart(entries)
	if !ok {
		t.Fatal("expected a free range")
	}
	if got != base {
		t.Errorf("first free start = %d, want %d (the base block)", got, base)
	}

	entries = append(entries, subIDEntry{name: "base", start: base, count: one})
	got, ok = chooseSubIDRangeStart(entries)
	if !ok {
		t.Fatal("expected a free range")
	}
	if got != base+one {
		t.Errorf("next free start = %d, want %d", got, base+one)
	}
}

// ── sequential allocation over one database ─────────────────────────────────

// TestEnsureSubIDAllocation_SequentialRangesAreDisjoint is the baseline
// acceptance: N sequential agents get N pairwise-disjoint 65536-wide blocks on
// BOTH databases, and a repeated call for the same name changes nothing.
func TestEnsureSubIDAllocation_SequentialRangesAreDisjoint(t *testing.T) {
	h := newSubIDTestHost(t)
	// The host's real-world baseline: a rootless remap user and a human, both
	// below the pool. Neither may be overlapped or rewritten.
	h.seed(t, "dockremap:100000:65536\nkara:165536:65536\n", "dockremap:100000:65536\nkara:165536:65536\n")
	before := len(entriesOf(t, h.uidPath))

	const agents = 6
	for i := 0; i < agents; i++ {
		name := fmt.Sprintf("bunker-agent-%d", i)
		if err := ensureSubIDAllocation(h.uidPath, name); err != nil {
			t.Fatalf("allocate %s: %v", name, err)
		}
		if err := ensureSubIDAllocation(h.gidPath, name); err != nil {
			t.Fatalf("allocate %s (subgid): %v", name, err)
		}
	}

	assertPairwiseDisjoint(t, h.uidPath)
	assertPairwiseDisjoint(t, h.gidPath)

	if got := len(entriesOf(t, h.uidPath)); got != before+agents {
		t.Errorf("subuid entries = %d, want %d", got, before+agents)
	}
	for _, e := range entriesOf(t, h.uidPath) {
		if e.name == "dockremap" && (e.start != 100000 || e.count != 65536) {
			t.Errorf("foreign entry was rewritten: %+v", e)
		}
	}

	// Idempotent re-spawn: nothing moves, not even byte order.
	snapshot := h.readUID(t)
	if err := ensureSubIDAllocation(h.uidPath, "bunker-agent-2"); err != nil {
		t.Fatalf("re-allocate: %v", err)
	}
	if got := h.readUID(t); got != snapshot {
		t.Errorf("re-allocation rewrote the database:\n got %q\nwant %q", got, snapshot)
	}
}

// TestEnsureSubIDAllocation_ReusedEntryIsNeverReallocated pins that a running
// agent's mapping is stable: once a name has a block, later allocations return
// it even when the pool would now offer a different hole.
func TestEnsureSubIDAllocation_ReusedEntryIsNeverReallocated(t *testing.T) {
	h := newSubIDTestHost(t)
	// The name holds a block ABOVE the base, so a fresh allocation would
	// otherwise pick the base. It must not.
	own := subIDPoolBase + 7*subIDRangeSize
	h.seed(t, fmt.Sprintf("bunker-a:%d:%d\n", own, subIDRangeSize), "")

	if err := ensureSubIDAllocation(h.uidPath, "bunker-a"); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if got, want := h.readUID(t), fmt.Sprintf("bunker-a:%d:%d\n", own, subIDRangeSize); got != want {
		t.Errorf("database changed: got %q, want %q", got, want)
	}
}

// ── concurrency: the pairwise-disjoint spawn proof ──────────────────────────

// TestConfigureSubIDs_ConcurrentSpawnsGetDisjointRanges is the board's
// acceptance criterion for two spawned agents generalised to N=8: eight
// concurrent spawns race through the locked read-choose-append path against one
// database, and every resulting range must be pairwise disjoint. Without the
// lock this is a read-then-append TOCTOU — at least two spawns read the same
// file content and pick the same block, which is how the pre-GAP-140 path
// produced overlapping ranges in the first place.
func TestConfigureSubIDs_ConcurrentSpawnsGetDisjointRanges(t *testing.T) {
	h := newSubIDTestHost(t)
	h.seed(t, "root:0:65536\nkara:100000:65536\n", "root:0:65536\nkara:100000:65536\n")

	// Every goroutine resolves a DIFFERENT agent user whose uid is CLOSE to its
	// siblings' (1001..1008). Under the old start=uid rule any two of these
	// shared 65535 ids; the fix must make uid proximity irrelevant.
	const agents = 8
	prevLookup := userLookup
	userLookup = func(name string) (*user.User, error) {
		if !strings.HasPrefix(name, "bunker-conc-") {
			return nil, user.UnknownUserError(name)
		}
		n, err := strconv.Atoi(strings.TrimPrefix(name, "bunker-conc-"))
		if err != nil {
			return nil, user.UnknownUserError(name)
		}
		uid := 1001 + n
		return &user.User{Username: name, Uid: strconv.Itoa(uid), Gid: strconv.Itoa(uid)}, nil
	}
	t.Cleanup(func() { userLookup = prevLookup })

	start := make(chan struct{})
	errs := make([]error, agents)
	var wg sync.WaitGroup
	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release every goroutine at once to maximize contention
			errs[i] = configureSubIDs(context.Background(), fmt.Sprintf("bunker-conc-%d", i))
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("agent %d failed to allocate: %v", i, err)
		}
	}

	assertPairwiseDisjoint(t, h.uidPath)
	assertPairwiseDisjoint(t, h.gidPath)

	// Every agent owns exactly one entry, and each name's subuid and subgid
	// blocks must MATCH (one namespace per agent, not two).
	uidEntries := entriesOf(t, h.uidPath)
	byName := map[string]subIDEntry{}
	for _, e := range uidEntries {
		if _, dup := byName[e.name]; dup {
			t.Errorf("name %q got more than one entry", e.name)
		}
		byName[e.name] = e
	}
	for i := 0; i < agents; i++ {
		name := fmt.Sprintf("bunker-conc-%d", i)
		if _, ok := byName[name]; !ok {
			t.Errorf("no subuid entry for %q", name)
		}
	}
	for _, e := range entriesOf(t, h.gidPath) {
		u, ok := byName[e.name]
		if !ok {
			continue
		}
		if u.start != e.start {
			t.Errorf("%q: subuid start %d != subgid start %d", e.name, u.start, e.start)
		}
	}

	// The baseline foreign rows survive untouched.
	for _, e := range uidEntries {
		switch e.name {
		case "root":
			if e.start != 0 {
				t.Errorf("root entry moved: %+v", e)
			}
		case "kara":
			if e.start != 100000 {
				t.Errorf("kara entry moved: %+v", e)
			}
		}
	}
}

// TestEnsureSubIDAllocation_ConcurrentWritersOnOneFile drives the lock through
// the lower-level entry point too, so the disjointness guarantee is pinned on
// the allocator itself and not only on configureSubIDs.
func TestEnsureSubIDAllocation_ConcurrentWritersOnOneFile(t *testing.T) {
	h := newSubIDTestHost(t)
	h.seed(t, "", "")

	const agents = 8
	var wg sync.WaitGroup
	errs := make([]error, agents)
	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("worker-%d", i)
			release, err := lockSubIDs()
			if err != nil {
				errs[i] = err
				return
			}
			defer release()
			if err := ensureSubIDAllocation(h.uidPath, name); err != nil {
				errs[i] = err
				return
			}
			errs[i] = ensureSubIDAllocation(h.gidPath, name)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	assertPairwiseDisjoint(t, h.uidPath)
	assertPairwiseDisjoint(t, h.gidPath)
}

// ── refusal paths ───────────────────────────────────────────────────────────

// TestConfigureSubIDs_OverlappingSameNameEntryRefuses pins the board
// requirement "an existing entry for the same name is idempotent — but if that
// existing entry now overlaps ANOTHER entry, refuse naming both users".
func TestConfigureSubIDs_OverlappingSameNameEntryRefuses(t *testing.T) {
	h := newSubIDTestHost(t)
	// The exact legacy defect shape: two agent names whose ranges differ by one
	// id and therefore share 65535 ids.
	const seeded = "bunker-peer:1001:65536\nbunker-target:1002:65536\n"
	h.seed(t, seeded, seeded)
	stubSubIDUser(t, "bunker-target", 1002)

	err := configureSubIDs(context.Background(), "bunker-target")
	if err == nil {
		t.Fatal("overlapping entries did NOT refuse the spawn")
	}
	msg := err.Error()
	for _, want := range []string{"bunker-target", "bunker-peer", "overlaps"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message %q does not name %q", msg, want)
		}
	}
	if got := h.readUID(t); got != seeded {
		t.Errorf("database was modified by a refused spawn: %q", got)
	}
}

// TestEnsureSubIDAllocation_MalformedDatabaseRefusesFromWritePath proves the
// fail-closed contract end to end: a malformed line refuses the spawn and the
// file is left byte-identical (no half-written entry, no truncation, no temp
// sibling left behind).
func TestEnsureSubIDAllocation_MalformedDatabaseRefusesFromWritePath(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"no colon", "garbage\n", "malformed subordinate-id line"},
		{"two fields only", "alpha:100000\n", "malformed subordinate-id line"},
		{"empty start", "alpha::65536\n", "not a non-negative integer"},
		{"overflowing start", "alpha:99999999999999999999:65536\n", "not a non-negative integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSubIDTestHost(t)
			h.seed(t, tc.content, tc.content)

			err := ensureSubIDAllocation(h.uidPath, "bunker-a")
			if err == nil {
				t.Fatal("malformed database did not refuse")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
			if got := h.readUID(t); got != tc.content {
				t.Errorf("refused allocation modified the database:\n got %q\nwant %q", got, tc.content)
			}
			if _, serr := os.Stat(h.uidPath + subIDTempSuffix); !os.IsNotExist(serr) {
				t.Errorf("temp file left behind after refusal: %v", serr)
			}
		})
	}
}

// TestEnsureSubIDAllocation_PoolExhaustionRefusesLoudly pins that running out
// of pool is a refusal, never a reuse of another user's ids.
func TestEnsureSubIDAllocation_PoolExhaustionRefusesLoudly(t *testing.T) {
	h := newSubIDTestHost(t)
	full := fmt.Sprintf("hog:%d:%d\n", subIDPoolBase, int64(subIDPoolLimit)-int64(subIDPoolBase)+1)
	h.seed(t, full, full)

	err := ensureSubIDAllocation(h.uidPath, "bunker-late")
	if err == nil {
		t.Fatal("exhausted pool did not refuse")
	}
	var exhausted *subIDPoolExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("error = %v (%T), want *subIDPoolExhaustedError", err, err)
	}
	for _, want := range []string{"bunker-late", "no disjoint", strconv.Itoa(subIDRangeSize)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
	if got := h.readUID(t); got != full {
		t.Errorf("refused allocation modified the database: %q", got)
	}
}

// TestEnsureSubIDAllocation_UnreadableDatabaseRefuses keeps the failure closed
// when the database cannot be read at all: allocating against content the
// allocator cannot see could hand out ids that are already taken.
func TestEnsureSubIDAllocation_UnreadableDatabaseRefuses(t *testing.T) {
	h := newSubIDTestHost(t)
	// A directory in place of the file: readable path, unreadable content.
	if err := os.MkdirAll(h.uidPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	err := ensureSubIDAllocation(h.uidPath, "bunker-a")
	if err == nil {
		t.Fatal("unreadable database did not refuse")
	}
	if !strings.Contains(err.Error(), "read ") {
		t.Errorf("error = %v, want a read failure", err)
	}
}

// TestEnsureSubIDAllocation_MissingFileIsAnEmptyDatabase pins that a host which
// has never had a subordinate-ID database still allocates (the kernel treats a
// missing file as "no mappings").
func TestEnsureSubIDAllocation_MissingFileIsAnEmptyDatabase(t *testing.T) {
	h := newSubIDTestHost(t) // files deliberately not created

	if err := ensureSubIDAllocation(h.uidPath, "bunker-a"); err != nil {
		t.Fatalf("allocate against a missing database: %v", err)
	}
	want := fmt.Sprintf("bunker-a:%d:%d\n", subIDPoolBase, subIDRangeSize)
	if got := h.readUID(t); got != want {
		t.Errorf("result = %q, want %q", got, want)
	}
}

// TestConfigureSubIDs_LockFailureRefusesSpawn proves the lock error
// participates in configureSubIDs' failure contract: an unusable lock path
// refuses the spawn (nothing is written) rather than appending without
// exclusion.
func TestConfigureSubIDs_LockFailureRefusesSpawn(t *testing.T) {
	h := newSubIDTestHost(t)
	h.seed(t, "", "")
	// The lock directory path is a FILE, so MkdirAll fails.
	if err := os.WriteFile(h.lockDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed lock path: %v", err)
	}
	stubSubIDUser(t, "bunker-a", 1001)

	err := configureSubIDs(context.Background(), "bunker-a")
	if err == nil {
		t.Fatal("unusable lock path did not refuse the spawn")
	}
	if got := h.readUID(t); got != "" {
		t.Errorf("spawn wrote without the lock: %q", got)
	}
}

// ── write path ──────────────────────────────────────────────────────────────

// TestWriteSubIDEntry_PreservesModeAndLeavesNoTemp pins the atomic write: the
// database's permission bits survive the rename and the temp sibling is gone.
func TestWriteSubIDEntry_PreservesModeAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subuid")
	if err := os.WriteFile(path, []byte("root:0:65536\n"), 0o640); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := writeSubIDEntry(path, []byte("root:0:65536\n"),
		subIDEntry{name: "bunker-a", start: subIDPoolBase, count: subIDRangeSize}); err != nil {
		t.Fatalf("write: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o, want 640", fi.Mode().Perm())
	}
	if _, err := os.Stat(path + subIDTempSuffix); !os.IsNotExist(err) {
		t.Errorf("temp file survived the rename: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := fmt.Sprintf("root:0:65536\nbunker-a:%d:%d\n", subIDPoolBase, subIDRangeSize)
	if got := string(data); got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if _, err := parseSubIDEntries(data, path); err != nil {
		t.Errorf("written database does not parse: %v", err)
	}
}

// TestWriteSubIDEntry_TerminatesAnUnterminatedLastLine pins the fix for the
// fused-record corruption: appending to a database whose last line has no
// newline must not produce "first:...:65536second:...:65536", which no parser
// can read.
func TestWriteSubIDEntry_TerminatesAnUnterminatedLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subuid")
	existing := []byte("first:100000:65536")
	if err := os.WriteFile(path, existing, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := writeSubIDEntry(path, existing,
		subIDEntry{name: "second", start: subIDPoolBase, count: subIDRangeSize}); err != nil {
		t.Fatalf("write: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := fmt.Sprintf("first:100000:65536\nsecond:%d:%d\n", subIDPoolBase, subIDRangeSize)
	if got := string(data); got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	entries, err := parseSubIDEntries(data, path)
	if err != nil {
		t.Fatalf("written database does not parse: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("parsed %d entries, want 2: %+v", len(entries), entries)
	}
}

// TestWriteSubIDEntry_NewFileMode pins the mode of a database the allocator
// creates from scratch (shadow's own /etc/subuid is 0644).
func TestWriteSubIDEntry_NewFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subuid")
	if err := writeSubIDEntry(path, nil, subIDEntry{name: "bunker-a", start: subIDPoolBase, count: subIDRangeSize}); err != nil {
		t.Fatalf("write: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o, want 644", fi.Mode().Perm())
	}
}

// TestWriteSubIDEntry_OpenErrorRefuses keeps a write failure loud.
func TestWriteSubIDEntry_OpenErrorRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "subuid")

	err := writeSubIDEntry(path, nil, subIDEntry{name: "bunker-a", start: subIDPoolBase, count: subIDRangeSize})
	if err == nil {
		t.Fatal("write into a missing directory did not fail")
	}
	if !strings.Contains(err.Error(), "open ") {
		t.Errorf("error = %v, want an open failure", err)
	}
}

// ── the legacy defect, reproduced arithmetically ────────────────────────────

// TestSubIDLegacySchemeWouldOverlap is the negative control for the whole
// change: it proves the OLD mapping rule (start = the user's own uid) really
// produced overlapping ranges for neighbouring uids, so the disjointness
// assertions above are testing a defect that existed rather than a tautology.
// The two ranges are computed by the same half-open arithmetic the allocator
// uses, and the overlap is asserted to be 65535 ids wide — the SEC-22 finding.
func TestSubIDLegacySchemeWouldOverlap(t *testing.T) {
	legacy := func(uid int64) subIDEntry {
		return subIDEntry{name: fmt.Sprintf("agent-%d", uid), start: uid, count: subIDRangeSize}
	}
	a, b := legacy(1001), legacy(1002)
	if !a.overlaps(b) {
		t.Fatalf("legacy scheme uid 1001/1002 does not reproduce the reported overlap: %+v %+v", a, b)
	}
	shared := a.end() - b.start
	if shared != 65535 {
		t.Errorf("legacy overlap width = %d, want 65535", shared)
	}

	// Every pair of neighbouring uids overlaps under the legacy rule, which is
	// why the defect was universal rather than occasional.
	for uid := int64(1000); uid < 1100; uid++ {
		if !legacy(uid).overlaps(legacy(uid + 1)) {
			t.Fatalf("legacy scheme uid %d/%d does not overlap", uid, uid+1)
		}
	}

	// And the allocator's answer for the same two users is disjoint.
	first, ok := chooseSubIDRangeStart(nil)
	if !ok {
		t.Fatal("empty pool has no room")
	}
	second, ok := chooseSubIDRangeStart([]subIDEntry{{name: "agent-1001", start: first, count: subIDRangeSize}})
	if !ok {
		t.Fatal("pool has no room after one allocation")
	}
	na := subIDEntry{name: "agent-1001", start: first, count: subIDRangeSize}
	nb := subIDEntry{name: "agent-1002", start: second, count: subIDRangeSize}
	if na.overlaps(nb) {
		t.Errorf("allocator produced overlapping ranges: %+v %+v", na, nb)
	}
	if nb.start != na.end() {
		t.Errorf("second allocation starts at %d, want %d (immediately after the first)", nb.start, na.end())
	}
}
