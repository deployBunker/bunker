package webdav

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/deployBunker/bunker/internal/invalidation"
)

// ---------------------------------------------------------------------------
// E-6's poll form: `X-Bunker-Op: events` (BFS-004 §3 E-6, §10.5).
//
// The watcher is per-target and this build has none, so `watch` answers
// capability_unavailable with mode=poll. THIS is the mode it names, and the
// client's invalidation path depends on it: `events` is the poll form the
// client prefers over its last-resort revision poll — and the one mechanism
// that observes uncommitted edits on a git tree, where the revision poll's
// HEAD token moves on commits only (BFS-048). A build that declares the
// poll form and does not serve it leaves the mount with no mechanism at all —
// which is what BFS-026 found: the client reports the dead channel honestly and
// the cache never invalidates.
//
// What this op can know decides the events it emits, and the difference is not
// cosmetic:
//
//   - It OBSERVES the served tree (one bounded walk, one stat per entry) and
//     compares each path against the identity it recorded last. That identity
//     is (size, mtime, ctime): ctime is in it because an edit that preserves
//     size AND mtime is exactly the shape a metadata-keyed observer would
//     otherwise miss, and this release has already recorded that class as a
//     defect (BFS-009 F1). Where the platform exposes no ctime the identity
//     falls back to (size, mtime) and that gap is real (see filetime_other.go).
//   - METADATA IS NOT TRUSTED WHERE IT CANNOT BE TRUSTED (QA-BUNKER-36). The
//     kernel stamps filesystem metadata from the COARSE clock (CONFIG_HZ), so an
//     edit performed within one tick of the write that came before it carries
//     the SAME mtime and the SAME ctime. A same-size, mtime-preserving rewrite
//     landing in such a tick is then identical to the observation recorded just
//     before it in every field of the identity — measured at CONFIG_HZ=250 (a
//     4 ms tick, no sleep between the seed write and the edit): ctime did not
//     move, 5 times out of 5, on ext4 and on tmpfs — and a metadata-only diff
//     would answer "unchanged" for bytes that moved. The ledger therefore reads
//     the BYTES of a regular file whose recorded metadata cannot rule that out
//     (see coarseClockAmbiguous) and compares content digests: the edit is
//     reported from the bytes, not from a clock the filesystem never advanced.
//     The read is narrow by construction — see the block comment above
//     eventsCoarseClockWindow — so a settled tree still costs one stat per path.
//   - It CANNOT see anything between two observations, and it holds no history
//     from before its first one. Both are why a cursor it cannot vouch for is
//     answered `overflow` — the channel's own word for "knowledge lost, drop
//     everything and re-snapshot" — rather than an empty list, which would be a
//     claim that nothing happened.
//   - What remains outside it is a change the filesystem does not record at all:
//     a rewrite that restores size, mtime, ctime AND the bytes. That is not an
//     edit to report (nothing moved), and the write path's content-hash
//     re-validation (§6.1) is what refuses the case where it matters.
//
// Read-only (E-4's invariant, A-9): a walk, a stat per entry, and — only for the
// paths whose metadata cannot vouch for their bytes — a streamed read. Nothing
// is ever written or created.
// Bounded: the walk stops at eventsScanLimit entries, a path list is capped at
// eventsMaxPathsPerEvent (spilling to `overflow`, never to a longer or partial
// list), a frame is capped at the declared `max_event_bytes` (BFS-062: a COUNT
// does not bound a frame, and the frame is what a consumer's reader must hold),
// and the ledger keeps eventsJournalEvents of history.
//
// The resume rule (BFS-063, and the reason the ledger cannot delegate the last
// step). A poll is answered from the retained journal only when the ledger can
// vouch for the interval the presenting client asked about, and there are two
// cases in which NO answer may stand in for that gap (SPEC-push-channel §3.3
// R-3, SPEC-watcher-capability §5.1):
//
//   - The client presents NO cursor. It has observed nothing, so no interval is
//     vouched for it and it has no cursor against which a partial tail could
//     read as a gap. The answer is `overflow`.
//   - The client presents cursor 0 while seq 1 is no longer retained. This is
//     the one case where a retained tail is NOT self-describing, because the
//     client's own monotonicity rule is disabled at zero (`invalidate.go`: the
//     gap check is guarded on a non-zero cursor) — exactly the window BFS-041's
//     hole H-10 measured. The answer is `overflow`.
//
// A cursor above zero keeps the documented behaviour (SPEC-watcher-capability
// §2.2, §5.3): the retained tail is served and its first seq IS a gap the
// client's own rule reads as knowledge lost, so nothing is silently skipped
// there.
//
// The cursor itself is MINTED by the server: the whole-tree `snapshot` answer
// carries `head_seq`, the ledger cursor read before the observation walk, and a
// client that holds that observation presents it here (seedEvents records the
// same observation as the ledger's own baseline). That is what separates a
// client whose interval the ledger genuinely covers — a client that bound with a
// snapshot, and whose snapshot is at the cursor it presents — from a client that
// has observed nothing. The server cannot tell those two apart from the request
// alone; the declaration is what makes them distinguishable, and it is
// checkable because the cursor is one the server itself issued.
//
// It is a SINGLE-ENVELOPE request: it observes, answers and closes. There is no
// long-poll and no stream here, so nothing is held open on a dead client and no
// request timeout governs a session — see the note on where this sits in
// `internal/server/server.go` (the surface is mounted ahead of the chi router,
// which is where middleware.Timeout lives).
// ---------------------------------------------------------------------------

// The two event names this op emits. It never emits `heartbeat`: a heartbeat
// keeps a STREAM alive, and a poll that answers is its own liveness signal.
const (
	eventInvalidate = "invalidate"
	eventOverflow   = "overflow"
)

// The ledger's declared bounds. They are variables rather than constants for one
// reason: the boundary behaviour (spill to overflow, journal pruning) is part of
// the contract, and a test must be able to exercise it without creating 4097
// files or driving 257 polls. TestEventsOpDeclaredBounds pins the declared
// values, so lowering them in a test cannot silently change the default.
var (
	// eventsMaxPathsPerEvent is BFS-004 §3 E-6's declaration 2 (4096).
	eventsMaxPathsPerEvent = 4096
	// eventsJournalEvents bounds the history one answer can carry.
	eventsJournalEvents = 256
	// eventsScanLimit bounds a single observation.
	eventsScanLimit = 100000
)

// eventLine is one E-6 event object: the same three names the client's Event
// type decodes, and the same fields, including `tree` on every line (declaration
// 3). `paths` is always present — an empty array for `overflow`, as §10.5's own
// bytes show — so a consumer never has to tell "no paths" from "absent".
type eventLine struct {
	Seq   int64    `json:"seq"`
	Event string   `json:"event"`
	Paths []string `json:"paths"`
	Rev   string   `json:"rev,omitempty"`
	Tree  string   `json:"tree,omitempty"`
}

// identity is what the filesystem reports about one path. It is comparable, so
// a diff is a map comparison and not a second interpretation of the tree.
//
// Regular says whether the path is a regular file. It is a member because the
// content-verified fallback below needs exactly that fact: only a regular file
// has bytes that can move while every metadata field stands still.
type identity struct {
	Size    int64
	Mtime   int64 // unix nanoseconds
	Ctime   int64 // unix nanoseconds, or 0 where the platform has none
	Dir     bool
	Regular bool
}

func identityOf(fi os.FileInfo) identity {
	return identity{
		Size:    fi.Size(),
		Mtime:   fi.ModTime().UnixNano(),
		Ctime:   ctimeUnixNano(fi),
		Dir:     fi.IsDir(),
		Regular: fi.Mode().IsRegular(),
	}
}

// The three comparisons the surface is allowed to make about an observed
// identity, and the reason they are METHODS ON THE ONE IDENTITY TYPE rather
// than tuples written out at each call site (BFS-049, SPEC-watcher-capability
// R-V6: "the derived artifacts must agree about what a change is").
//
// Before this row the hash cache compared a private (size, mtime) pair while
// this ledger compared a whole identity, so the two observers disagreed about a
// same-size, mtime-restored edit: the ledger reported the path as moved and the
// cache answered "unchanged". A second copy of the tuple is how that comes
// back, so the projections below are the only places the question is asked.

// sameIdentity reports whether two observations are the SAME observation of a
// path, kind included. This is the LEDGER's comparison: the event channel
// answers "must the client drop what it holds for this path?", so a path that
// changed from a file to a collection with byte-identical metadata is still a
// change it must report.
func (id identity) sameIdentity(other identity) bool { return id == other }

// sameContentKey reports whether two observations agree about the path's
// CONTENT identity: the fields a metadata-keyed observer of the BYTES may key
// on. It is what the hash cache decides "unchanged" with (tree.go, hashFile),
// and it deliberately excludes Dir — hashFile only ever caches a regular file,
// so the kind is fixed by construction rather than by the comparison.
//
// ctime is a member because an edit that preserves size AND mtime is exactly
// the shape a (size, mtime) key cannot see — the class this release already
// recorded as a defect (BFS-009 F1), and the shape that made the cache and the
// ledger disagree.
func (id identity) sameContentKey(other identity) bool {
	return id.Size == other.Size && id.Mtime == other.Mtime && id.Ctime == other.Ctime
}

// sameMetadataKey reports whether two observations agree about the fields the
// hash cache keyed on BEFORE BFS-049 — (size, mtime) and nothing else. Nothing
// decides anything with it. It exists to NAME the class this row closes, so
// that class can be counted in production instead of being a historical
// anecdote: see the counters below.
func (id identity) sameMetadataKey(other identity) bool {
	return id.Size == other.Size && id.Mtime == other.Mtime
}

// The report of the class this row closes. Until the identity was shared, a
// path whose size and mtime were restored to the nanosecond was CURRENT to the
// hash cache and MOVED to this ledger; the two consumers it feeds are different
// and so is the harm — the cache decides whether content is re-sent (a GET can
// answer 304 with a stale ETag) while the ledger decides whether a client is
// told to drop. That is the divergence, and it was silent.
//
// It is counted rather than assumed away, because the code fix only makes
// TODAY's two observers agree: a future observer added with its own tuple would
// not be caught by any test that only asserts today's pair agrees. A non-zero
// count says "these paths moved in a way the (size, mtime) key could not see" —
// which is the sharpest honest statement available, and this is what it is not:
// it is not a claim that stale bytes were served. A chmod puts a path in this
// class and changes no bytes at all; only the write path's content-hash
// re-validation (BFS-004 §6.1 step 5) can tell whether bytes moved in a way no
// metadata observation sees.
//
// It is process-wide rather than per-ledger for the same reason
// frameOverBoundTotal is: the fact is about this SERVER's observations, and it
// must survive a tree's lazily-built ledger being rebuilt. A cell measures the
// DELTA across its own stimulus.
var (
	identityDivergenceMu     sync.Mutex
	metadataKeyBlindMoves    int64
	lastMetadataKeyBlindPath string
)

// recordMetadataKeyBlindMove notes one path whose observed identity moved while
// the (size, mtime) key compared equal.
func recordMetadataKeyBlindMove(path string) {
	identityDivergenceMu.Lock()
	defer identityDivergenceMu.Unlock()
	metadataKeyBlindMoves++
	lastMetadataKeyBlindPath = path
}

// IdentityDivergenceCounters reports how many paths this process has observed
// move in the class the pre-BFS-049 (size, mtime) hash-cache key could not see,
// and the last such path (relative, "/"-separated). A zero count is the honest
// "no observation of this process has produced one", which is the state of a
// tree nothing restores timestamps on.
//
// The result names are deliberately not the variables': a named result that
// shadows a package variable returns the zero value, and this accessor DID
// report 0 while the last path was being filled in until the cell below caught
// it.
func IdentityDivergenceCounters() (count int64, lastPath string) {
	identityDivergenceMu.Lock()
	defer identityDivergenceMu.Unlock()
	return metadataKeyBlindMoves, lastMetadataKeyBlindPath
}

// ---------------------------------------------------------------------------
// QA-BUNKER-36 — THE CONTENT-VERIFIED FALLBACK.
//
// The identity above is enough on a kernel whose clock can separate two writes.
// It is NOT enough on a kernel that cannot: the filesystem stamps metadata from
// the COARSE clock (one tick of CONFIG_HZ), so a write landing within the same
// tick as the write before it is stamped with the SAME mtime and the SAME ctime.
// A same-size rewrite inside one such tick — the class BFS-009 F1 filed, and a
// shape `git checkout`, an editor or a restore tool can all produce — is then
// indistinguishable from the observation recorded just before it in EVERY field
// of the identity, and a metadata-only diff answers "unchanged" for bytes that
// moved. Measured on the host this row was filed from (Debian, CONFIG_HZ=250, a
// 4 ms tick): seed write, edit, `os.Chtimes` restoring the mtime, with no sleep
// between them, left ctime delta = 0 ns on 5/5 iterations, on ext4 and on tmpfs.
//
// The fallback is deliberately narrow, because its price is reading bytes:
//
//   - ONLY A REGULAR FILE is ever verified. Nothing else has content that can
//     move under a standing metadata observation, so a collection, a symlink or
//     a device is taken at its metadata exactly as before.
//   - ONLY A RECORD WHOSE METADATA CANNOT VOUCH FOR IT is verified. A record
//     taken when an invisible edit was impossible — the observation is more than
//     one coarse-clock window after the path's own ctime — is trusted on its
//     metadata, and not one byte is read. That is the whole cheapness rule, and
//     it is what keeps a settled tree at one stat per path: the window expires
//     and the read expires with it.
//   - METADATA THAT MOVED is never re-read to decide anything: the move IS the
//     change, and that path is reported on its own evidence. The one read a
//     moved path can pay is the BASELINE read (ledgerRecord), and only while its
//     new metadata can still be masked: without it the next observation would
//     have no bytes to compare against, and would have to choose between a
//     redundant event and a silent one.
//
// The digest is also what keeps the two observers of this tree — this ledger and
// the hash cache (tree.go, BFS-049/R-V6) — agreeing in this class: both consult
// the same window, so neither can call a frozen edit "unchanged" while the other
// reports it.
// ---------------------------------------------------------------------------

// eventsCoarseClockWindow bounds how long after a path's last metadata change an
// edit can still hide inside the filesystem's own timestamps. The filesystem
// stamps from the coarse clock, whose granularity is one tick (1/HZ): 1 ms at
// CONFIG_HZ=1000, 4 ms at 250, 10 ms at 100 — so 50 ms is conservative for every
// HZ a Linux kernel ships. It is a variable rather than a constant for the same
// reason the declared bounds above are: a cell must be able to drive both sides
// of it without waiting, and the DECLARED value is pinned by a test so lowering
// it in a cell cannot move the default.
var eventsCoarseClockWindow = 50 * time.Millisecond

// coarseClockAmbiguous reports whether an observation taken at `at` of a path
// whose metadata carries `ctimeNano` can be masked by an edit that leaves every
// metadata field identical.
//
// The derivation is exact rather than a guess. A filesystem timestamp is
// quantised to the tick, so a value C is the start of the tick that produced it
// and an edit at wall time t is stamped coarse(t). An edit is invisible to a
// metadata comparison exactly when coarse(t) == C — that is the only way the
// ctime can stand still — which requires t < C + J for a tick J. So an edit
// after an observation made at `at` can be invisible iff `at < C + J`, and with
// the window standing in for J (window >= J) that is `at-C < window`.
func coarseClockAmbiguous(at time.Time, ctimeNano int64) bool {
	if ctimeNano == 0 {
		// No change time at all (filetime_other.go): there is no window to
		// reason about, because the field never moves — the metadata can never
		// vouch for the bytes on its own, so every regular file is verified.
		// bunkerd serves Linux, where this does not arise; it is the honest
		// reading of the same rule on a build that has no ctime, and it is
		// stated rather than papered over, exactly as filetime_other.go states
		// the weaker identity.
		return true
	}
	return at.UnixNano()-ctimeNano < int64(eventsCoarseClockWindow)
}

// observedEntry is the ledger's record of one path: the metadata identity it
// observed, the digest of the bytes as of that observation — empty when the
// bytes were never read — and when the observation was taken.
type observedEntry struct {
	id         identity
	digest     string
	recordedAt time.Time
}

// vouched reports whether this record can be trusted WITHOUT reading the bytes:
// an edit after `recordedAt` cannot leave every metadata field identical, so the
// metadata alone proves the content did not move. It is a fixed property of the
// pair (recordedAt, ctime), which is why a quiet tree stops paying for reads
// instead of paying for them forever.
func (e observedEntry) vouched() bool {
	return !coarseClockAmbiguous(e.recordedAt, e.id.Ctime)
}

// The report of the fallback, in the same shape as the divergence counter above:
// how many times this process has had to read a path's bytes because its
// metadata could not vouch for it, and the last such path. It is the number that
// answers "what does the fallback cost this tree?" from production rather than
// from a benchmark, and it is process-wide for the same reason
// metadataKeyBlindMoves is: the fact is about this server's observations, and it
// must survive a tree's lazily-built ledger being rebuilt.
var (
	contentVerifyMu       sync.Mutex
	contentVerifyTotal    int64
	lastContentVerifyPath string
)

// recordContentVerification notes one path whose bytes the ledger had to read.
func recordContentVerification(path string) {
	contentVerifyMu.Lock()
	defer contentVerifyMu.Unlock()
	contentVerifyTotal++
	lastContentVerifyPath = path
}

// ContentVerificationCounters reports how many paths this process has had to
// verify BY CONTENT — their metadata could not vouch for their bytes — and the
// last such path (relative, "/"-separated).
//
// A zero count is the honest "every observation this process took was vouchable
// on metadata alone", which is the state of a tree whose paths were last touched
// longer ago than the coarse-clock window. Like IdentityDivergenceCounters, a
// cell measures the DELTA across its own stimulus.
func ContentVerificationCounters() (count int64, lastPath string) {
	contentVerifyMu.Lock()
	defer contentVerifyMu.Unlock()
	return contentVerifyTotal, lastContentVerifyPath
}

// ledgerRecord builds the record the ledger keeps for one observed path.
//
// It reads the bytes of a regular file exactly when the record it is building
// cannot be vouched for on its own: the observation lands inside the coarse
// window opened by the path's own ctime, so a same-size, mtime-preserving edit
// could have happened and left every metadata field standing. Everything else —
// every settled path, and every path with no bytes to compare — is metadata-only,
// and that is what keeps a quiet poll a walk and a stat per entry.
//
// The read is streamed (contentDigest) and never buffered: this is a poll path,
// and a 4 GiB file must not become a 4 GiB allocation to be observed.
func (t *tree) ledgerRecord(rel string, id identity, now time.Time) observedEntry {
	e := observedEntry{id: id, recordedAt: now}
	if !id.Regular || !coarseClockAmbiguous(now, id.Ctime) {
		return e
	}
	digest, err := contentDigest(filepath.Join(t.rootPath(), filepath.FromSlash(rel)))
	if err != nil {
		// A path whose bytes cannot be read cannot be vouched for either. The
		// digest stays empty, and an empty digest compares unequal to any real
		// one, so the next observation reports the path rather than claiming a
		// quiet that was never established.
		return e
	}
	recordContentVerification(rel)
	e.digest = digest
	return e
}

// ledgerDiff compares one observation against the ledger's recorded baseline and
// returns the paths whose identity moved, sorted, together with the records the
// next observation diffs against.
//
// A move is judged with the ONE identity comparison (sameIdentity) — plus, for
// the paths whose metadata cannot vouch for their bytes, that path's content
// digest. The class the pre-BFS-049 (size, mtime) hash-cache key could not see
// is counted as it is found, so "these two observers no longer disagree" is a
// fact an operator can read rather than a claim they must take on faith. A
// digest-only move lands in that class too, and it is its sharpest instance: the
// whole identity stood still while the bytes moved.
//
// A nil prev is the ledger's FIRST observation (a fresh ledger, or one being
// seeded from a whole-tree snapshot): nothing is a "change" there — there is no
// baseline it could have changed from — but every record is built, which is
// where a baseline's content digests come from. Callers that need no path list
// (the seed) ignore it; the watcher's rescan uses it exactly as the old
// whole-map comparison did, where an empty baseline reported every path.
func (t *tree) ledgerDiff(prev map[string]observedEntry, state map[string]identity, now time.Time) ([]string, map[string]observedEntry) {
	next := make(map[string]observedEntry, len(state))
	var out []string
	for p, id := range state {
		before, known := prev[p]
		switch {
		case !known:
			// A path the ledger has never observed: its whole content is new
			// knowledge for the client, and there is nothing to compare against.
			out = append(out, p)
			next[p] = t.ledgerRecord(p, id, now)
		case !before.id.sameIdentity(id):
			// The metadata moved, so the path is a change on its own evidence.
			if before.id.sameMetadataKey(id) {
				// (size, mtime) compare equal: this path is in the class the
				// pre-BFS-049 hash-cache key could not see.
				recordMetadataKeyBlindMove(p)
			}
			out = append(out, p)
			next[p] = t.ledgerRecord(p, id, now)
		default:
			// The metadata is identical, so the metadata alone decides nothing.
			if before.vouched() {
				// No edit after the record was taken can leave the metadata
				// identical, so the bytes cannot have moved: the record stands,
				// and no byte is read. This is the path a settled tree takes.
				next[p] = before
				continue
			}
			// Inside the coarse window the metadata may be standing still over
			// bytes that moved. A regular file is verified by content; a path
			// with no content to compare is taken at its metadata, because
			// nothing under it can move without moving the metadata.
			cur := t.ledgerRecord(p, id, now)
			if id.Regular && cur.digest == "" {
				// The bytes could not be read (they vanished mid-observation,
				// or the read failed). That is NOT evidence of a change: a
				// comparison that never happened may not produce an event, and
				// the record is left exactly as it was — so the verification is
				// RETRIED rather than renewed — instead of being renewed into a
				// record that nobody verified. A path whose bytes cannot be read
				// here is one the client cannot read either, and the observation
				// that can see the change (a later walk, which sees the path
				// gone or its metadata moved) is the one that reports it.
				next[p] = before
				continue
			}
			// A verdict from the bytes requires bytes on BOTH sides. When the
			// previous record carries no digest — its own read failed — there is
			// nothing to compare against, so the path is quietly recorded with
			// the digest just taken and reported only if a LATER observation
			// finds the bytes moved away from it.
			if id.Regular && before.digest != "" && before.digest != cur.digest {
				if before.id.sameMetadataKey(id) {
					recordMetadataKeyBlindMove(p)
				}
				out = append(out, p)
			}
			next[p] = cur
		}
	}
	for p := range prev {
		if _, ok := state[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, next
}

// eventLog is the per-tree ledger: what this process has observed, and the
// events it owes a client that polls. It is guarded by its own mutex, held
// across one observation so two concurrent polls cannot interleave two diffs
// against the same baseline.
type eventLog struct {
	mu       sync.Mutex
	seq      int64
	base     int64 // seq of the oldest retained journal entry
	started  bool
	observed map[string]observedEntry
	journal  []eventLine
}

// eventsAnswer is one poll's answer.
type eventsAnswer struct {
	Events []eventLine
	// Scanned is how many paths the observation saw.
	Scanned int
	// Truncated is true when the observation hit eventsScanLimit: the answer is
	// then an `overflow`, never a diff of the part that fit.
	Truncated bool
	// Head is the ledger's seq at answer time, so a client (and a test) can see
	// that a quiet answer means "caught up" rather than "channel dead".
	Head int64
}

// eventLedger returns this tree's ledger, creating it on first use.
func (t *tree) eventLedger() *eventLog {
	t.evMu.Lock()
	defer t.evMu.Unlock()
	if t.ev == nil {
		t.ev = &eventLog{}
	}
	return t.ev
}

// ledgerCursor is the ledger's seq at the moment a whole-tree observation begins
// — the cursor the snapshot answer mints (`head_seq`) for the client that takes
// it.
//
// It is read BEFORE the walk and not after, and that direction is the load-
// bearing one: any event pushed while the walk runs then has a seq ABOVE the
// minted cursor and is delivered to the client that holds it, so a change the
// walk raced is reported rather than skipped. Reading it after the walk would
// mint a cursor that already covers changes the walk may have missed, which is
// the silent-gap class this row exists to close. The price of reading it first
// is at most a redundant invalidate for a path the walk already saw — a drop,
// never a miss.
func (t *tree) ledgerCursor() int64 {
	l := t.eventLedger()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// seedEvents records an observation the ledger did not have to walk for. The
// `snapshot` op of a whole tree IS an observation of that tree, so a client
// that binds by taking one has its baseline recorded from the very state it
// received: the first poll then answers a diff against the client's own view
// instead of declaring the interval before it lost. A partial observation
// (a subtree, a truncated result) is not a baseline and is ignored.
//
// The records are built with the content-verified fallback (ledgerDiff), which
// is where a baseline's digests come from: without them the first poll would
// have no bytes to compare an identical-metadata path against, and the baseline
// would be exactly as blind as the diff it exists to make honest.
//
// The reads the fallback pays happen BEFORE the ledger lock is taken, so a
// binding client's snapshot cannot park a concurrent poll behind them; a ledger
// that has already started is left exactly as it was.
func (t *tree) seedEvents(state map[string]identity, whole bool) {
	if !whole || len(state) == 0 {
		return
	}
	l := t.eventLedger()
	l.mu.Lock()
	started := l.started
	l.mu.Unlock()
	if started {
		return
	}
	_, entries := t.ledgerDiff(nil, state, time.Now())
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started {
		return
	}
	l.started = true
	l.observed = entries
}

// observe walks the served tree and records one identity per path. Paths are
// spelled as the snapshot op spells them (relative, "/"-separated, "." for the
// root), so a client's two views of the tree are in one vocabulary, and the
// surface's own staging files are skipped exactly as every other listing skips
// them.
func (t *tree) observe() (map[string]identity, bool, error) {
	root := t.rootPath()
	state := make(map[string]identity, 256)
	truncated := false
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if p == root {
				return walkErr
			}
			// A path that vanished or is unreadable mid-walk is not a reason to
			// fail the observation: the next one reports it.
			return nil
		}
		if isTempName(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if len(state) >= eventsScanLimit {
			truncated = true
			return fs.SkipAll
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		state[filepath.ToSlash(rel)] = identityOf(fi)
		return nil
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		return nil, truncated, err
	}
	return state, truncated, nil
}

// push appends one event, advancing the seq and pruning the journal to its
// bound. Callers hold l.mu.
//
// The frames this funnel emits respect BOTH bounds, and that is the whole point
// of measuring here rather than at the path list (BFS-062, the BFS-031 lesson
// one bound over): a COUNT cannot bound a frame — 4096 paths of PATH_MAX is
// ~16.8 MiB of JSON on one line — so an `invalidate` whose SERIALIZED frame
// crosses the declared byte bound is emitted as the `overflow` marker instead
// of as a path list. The swap is deliberate and it is the spec's rule
// (SPEC-push-channel §7.2): over either bound ⇒ `overflow` with `paths: []`,
// never a truncated list and never a partial list presented as complete.
//
// Measuring in the funnel is what keeps every answer honest, not just the one
// that happens to check: the JOURNAL is the tree's event history, and a frame
// that entered it oversized would be served to every client that reads the
// retained tail afterwards.
func (l *eventLog) push(t *tree, name string, paths []string) eventLine {
	l.seq++
	if paths == nil {
		paths = []string{}
	}
	ev := eventLine{Seq: l.seq, Event: name, Paths: paths, Rev: t.revToken(), Tree: t.identity()}
	if name == eventInvalidate {
		if n, over := frameOverBound(ev, t.eventFrameBound()); over {
			ev = eventLine{Seq: ev.Seq, Event: eventOverflow, Paths: []string{}, Rev: ev.Rev, Tree: ev.Tree}
			recordFrameOverBound(n)
		}
	}
	l.journal = append(l.journal, ev)
	if len(l.journal) > eventsJournalEvents {
		l.journal = append([]eventLine(nil), l.journal[len(l.journal)-eventsJournalEvents:]...)
	}
	l.base = l.journal[0].Seq
	// R-5 (§3.4): every line that advances the counter is delivered to every
	// attached subscriber. The fan-out happens HERE, in the one funnel, rather
	// than at each call site, so a line cannot enter the journal without reaching
	// the stream — and the two mechanisms can never hold two different histories
	// of one tree. It appends to per-subscriber buffers and performs no I/O, so
	// the watcher's read loop never blocks on a subscriber (§6.1 B-8).
	t.broadcast(ev)
	return ev
}

// eventFrameBound is the byte bound ONE serialized event frame must respect on
// this tree. A tree built without a resolved surface answers the DECLARED
// default (invalidation.DefaultPushMaxEventBytes) rather than zero-means-no-
// bound: silently disabling a bound because a field was not filled in would be
// the same defect as a bound the producing side may exceed (BFS-062).
func (t *tree) eventFrameBound() int64 {
	if t.eventMaxBytes > 0 {
		return t.eventMaxBytes
	}
	return invalidation.DefaultPushMaxEventBytes
}

// eventFrameBytes measures one event frame the way the wire form does: the
// serialized JSON object of the line, which is exactly the bytes a consumer's
// per-line reader must be able to hold. It is exported to the package's cells
// so the arithmetic in the evidence is taken from the same encoder the wire
// uses, never from a hand-counted estimate.
func eventFrameBytes(ev eventLine) (int64, bool) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return 0, false
	}
	return int64(len(raw)), true
}

// frameOverBound reports an event frame's measured size and whether it crosses
// the bound. A frame that cannot be MEASURED is reported as over the bound:
// "unmeasurable" is not admissible, and admitting it would be a bound that
// fails open.
func frameOverBound(ev eventLine, maxEventBytes int64) (int64, bool) {
	n, ok := eventFrameBytes(ev)
	if !ok {
		return 0, true
	}
	return n, maxEventBytes > 0 && n > maxEventBytes
}

// frameOverBoundTotal counts, per process, the frames the BYTE bound refused to
// assemble as a path list. It exists because the two bounds fail for different
// reasons and an operator reading the record must be able to tell "the tree
// changed a lot" from "the tree's paths are long" (PRD §2.8: a bound the owner
// cannot see is not a bound). It is a process-wide count rather than a
// per-ledger one on purpose: the fact is about frames this SERVER refused, and
// it must survive the ledger being rebuilt (a tree's ledger is per-tree state,
// created lazily). A cell that reads it measures the DELTA across its own
// stimulus, because the counter is deliberately not reset by construction.
var (
	frameOverBoundMu    sync.Mutex
	frameOverBoundTotal int64
	frameOverBoundLast  int64 // the measured size of the last refused frame, in bytes
)

// recordFrameOverBound notes one frame the byte bound refused.
func recordFrameOverBound(bytes int64) {
	frameOverBoundMu.Lock()
	defer frameOverBoundMu.Unlock()
	frameOverBoundTotal++
	frameOverBoundLast = bytes
}

// FrameOverBoundCounters reports the byte-bound refusals this process has
// decided and the measured size of the last one, in bytes. A zero total is the
// honest "no frame this process assembled ever crossed the bound", which is the
// state a small tree produces.
func FrameOverBoundCounters() (total, lastBytes int64) {
	frameOverBoundMu.Lock()
	defer frameOverBoundMu.Unlock()
	return frameOverBoundTotal, frameOverBoundLast
}

// resumePoint is the position a client presents on the poll. It is two facts and
// not one, and the difference is the whole of BFS-063:
//
//   - Cursor is the ledger seq the client's view corresponds to.
//   - Baseline says whether the client HOLDS an observation at that cursor.
//
// A request that presents no cursor at all (`since_seq` absent) is the second
// fact's negative: the client has observed nothing. A request that presents a
// cursor is a claim about the client's own view, and the only cursor that makes
// that claim true is one this server minted for an observation the client
// actually took — the `head_seq` of a whole-tree `snapshot` answer. So the
// client cannot turn the declaration into a way of asking for quiet it did not
// earn: a cursor it was never issued either reads as a gap (nothing was
// retained for it) or, if it is above the ledger, is declared lost outright.
type resumePoint struct {
	Cursor   int64
	Baseline bool
}

// pollEvents observes the tree once and answers the events a client at p has not
// seen.
//
// The ledger lock is taken BEFORE the observation and held across it, so two
// clients polling at once are strictly ordered: each diff is against the state
// the previous observation recorded, and the recorded baseline can never move
// backwards. The price is that a second client's poll waits for one walk
// (measured at ~7 ms on a 2000-path tree, §8 of the row report) plus whatever
// content verification the coarse-clock fallback has to pay for the paths the
// walk could not vouch for — paid only when two clients poll the same tree in
// the same instant, and bounded by the paths whose metadata is younger than the
// coarse-clock window rather than by the tree.
func (t *tree) pollEvents(p resumePoint) (eventsAnswer, error) {
	l := t.eventLedger()
	l.mu.Lock()
	defer l.mu.Unlock()

	state, truncated, err := t.observe()
	if err != nil {
		return eventsAnswer{}, err
	}
	now := time.Now()

	if !l.started {
		// The ledger's knowledge begins here, and the interval before it is
		// unknown rather than empty. `overflow` is that statement in the
		// channel's own vocabulary, and it makes the client re-establish its
		// view — which is also what closes the window between a client's own
		// snapshot and this first observation.
		// The records are built even though no diff is owed: this observation
		// IS the baseline the next poll diffs against, digests included.
		l.started = true
		_, l.observed = t.ledgerDiff(nil, state, now)
		l.push(t, eventOverflow, nil)
	} else {
		changed, next := t.ledgerDiff(l.observed, state, now)
		l.observed = next
		switch {
		case truncated || len(changed) > eventsMaxPathsPerEvent:
			// Past the cap (or an incomplete observation): knowledge lost, not a
			// partial path list presented as a diff.
			l.push(t, eventOverflow, nil)
		case len(changed) > 0:
			l.push(t, eventInvalidate, changed)
		}
	}

	// An empty journal with a presented cursor is an honest empty answer: the
	// ledger has observed this tree and nothing has moved since the cursor the
	// client holds. With no cursor the client holds nothing to be caught up to,
	// so the answer falls through to the declaration below instead.
	if len(l.journal) == 0 && p.Baseline {
		return eventsAnswer{Events: []eventLine{}, Scanned: len(state), Truncated: truncated, Head: l.seq}, nil
	}

	// The resume decision, shared with the stream (resumeLocked below): the poll
	// and the pushed channel answer the same cursor on the same ledger, so the
	// two mechanisms cannot drift into two different accounts of one history.
	events := l.resumeLocked(t, p)
	return eventsAnswer{Events: events, Scanned: len(state), Truncated: truncated, Head: l.seq}, nil
}

// resumeLocked answers a resume request from the ledger's own history, WITHOUT
// observing the tree. It is the single implementation of §3.2's four cases:
//
//	cursor inside the retained journal — the retained lines above it, oldest first;
//	cursor exactly at head          — nothing (the caller's liveness line follows);
//	cursor behind the retained journal — an `overflow` first, whose seq is a gap;
//	cursor ahead of the ledger      — the counter advances PAST the cursor and an
//	                                  `overflow` is pushed, so the notice cannot
//	                                  be discarded as a duplicate (R-4).
//
// A client that presents NO cursor has told the ledger nothing about what it
// holds, so no interval is vouched for it and it is answered `overflow` rather
// than a tail it has no way to recognise as partial (R-3; the same rule the poll
// implements, and the one BFS-063's measurement is about).
//
// Callers hold l.mu. The lines it returns are the lines to ANSWER WITH; nothing
// else is appended to the journal by this function except the R-4 advance, which
// is a ledger-level correction and is journaled exactly as the poll journals it.
func (l *eventLog) resumeLocked(t *tree, p resumePoint) []eventLine {
	switch {
	case p.Cursor > l.seq:
		// The cursor is ahead of anything this ledger has issued: a restarted
		// process, or a cursor minted against a different tree instance. The
		// range in between was never observed here, so it is declared lost — and
		// the seq is advanced PAST the cursor first, so the notice cannot be
		// discarded as a duplicate (BFS-041 §3.3 R-4).
		l.seq = p.Cursor
		return []eventLine{l.push(t, eventOverflow, nil)}
	case !p.Baseline || (p.Cursor == 0 && l.base > 1):
		// The interval is not vouched for, so nothing may stand in for it: not
		// an empty tail, not a partial one, and not a tail whose gap this client
		// has no way to recognise (SPEC-push-channel §3.3 R-3).
		//
		// The second form is the narrow one and it is the whole point: a retained
		// tail is self-describing only because the client's own rule reads its
		// first seq as a gap — and that rule is disabled at cursor 0. When seq 1
		// is still retained (`base <= 1`) the tail IS the whole history and needs
		// no marker; once it is not, the tail starts above the interval this
		// client asked about and only an `overflow` says so.
		return []eventLine{l.overflow(t, p)}
	}
	out := make([]eventLine, 0, len(l.journal))
	for _, ev := range l.journal {
		if ev.Seq > p.Cursor {
			out = append(out, ev)
		}
	}
	return out
}

// streamResume is resumeLocked for the pushed channel, and it exists for the one
// case the poll gets for free: the poll OBSERVES the tree first, so a ledger that
// has never observed anything is started by that very observation before any
// resume question is asked. The stream does not observe — it replays what the
// tree's ledger already knows — so a ledger that holds no history at all cannot
// answer "nothing moved"; the interval it was asked about was never observed here
// at all, and that is knowledge lost, not quiet (R-3).
//
// It is the same ledger, the same counter and the same case table as the poll:
// one implementation (resumeLocked) serves both, which is what makes the two
// mechanisms comparable rather than merely similar (SPEC-push-channel §11.1 O-1).
func (l *eventLog) streamResume(t *tree, p resumePoint) []eventLine {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p.Cursor > l.seq {
		l.seq = p.Cursor
		return []eventLine{l.push(t, eventOverflow, nil)}
	}
	if !l.started {
		return []eventLine{l.overflow(t, p)}
	}
	return l.resumeLocked(t, p)
}

// overflow returns the `overflow` notice owed to a client whose interval the
// ledger cannot vouch for. Callers hold l.mu.
//
// The notice is NOT appended to the journal, and that is a correctness decision
// rather than a saving: the journal is the tree's event history, which every
// client on this tree reads, while this notice is a statement about ONE request
// ("you have told me nothing about what you hold, so nothing is vouched for
// you"). Journaling it would put it in every other client's tail, where it would
// buy them a resync they did not earn — the arm that must stay green.
//
// Its seq is the ledger's head. That is above the presented cursor in every case
// that can reach here except one: a cursor of 0 against a ledger that has issued
// NOTHING yet (a freshly seeded, quiet tree). There is then no seq above the
// cursor to name, and the notice cannot be mistaken for a duplicate either — it
// is the only line that ledger has ever produced — so it is emitted at 0 rather
// than inventing a seq the next real event would then collide with. A client
// whose cursor is 0 reads it as the resync it is (invalidate.go: the overflow
// branch runs whatever the seq is).
func (l *eventLog) overflow(t *tree, p resumePoint) eventLine {
	return eventLine{Seq: l.seq, Event: eventOverflow, Paths: []string{}, Rev: t.revToken(), Tree: t.identity()}
}

// handleEvents serves E-6's poll form: `since_seq?` in, `result.events` out.
//
// `since_seq` present is a claim to hold an observation of the tree at that
// cursor; absent is the claim to hold none, and it is answered `overflow` —
// never a quiet or partial tail (BFS-063, see the resume rule above). The two
// are different requests, which is why the field is decoded as a pointer: a
// client that presents `0` is saying "my view is the tree as of seq 0", and a
// client that presents nothing is saying "I have no view" — a distinction the
// server cannot infer, and the reason the whole-tree `snapshot` answer carries
// the cursor it minted (ops.go, `head_seq`).
func (h *Handler) handleEvents(w http.ResponseWriter, r *http.Request, start time.Time) {
	body, f := h.readBody(w, r)
	if f != nil {
		h.failEnvelope(w, r, start, "events", *f)
		return
	}
	args := struct {
		SinceSeq *int64 `json:"since_seq"`
	}{}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &args); err != nil {
			h.writeEnvelope(w, r, start, "events", 400, VerdictBadArguments, false, nil,
				&envelopeError{Detail: `events arguments are not a JSON object of {"since_seq"}`})
			return
		}
	}
	point := resumePoint{}
	if args.SinceSeq != nil {
		if *args.SinceSeq < 0 {
			h.writeEnvelope(w, r, start, "events", 400, VerdictBadArguments, false, nil,
				&envelopeError{Detail: "events since_seq must be zero or positive"})
			return
		}
		point = resumePoint{Cursor: *args.SinceSeq, Baseline: true}
	}
	answer, err := h.tree.pollEvents(point)
	if err != nil {
		h.writeEnvelope(w, r, start, "events", 500, VerdictInternal, false, nil,
			&envelopeError{Detail: "the served tree could not be observed"})
		return
	}
	h.writeEnvelope(w, r, start, "events", 200, VerdictOK, answer.Truncated,
		map[string]any{
			"events":   answer.Events,
			"count":    len(answer.Events),
			"scanned":  answer.Scanned,
			"head_seq": answer.Head,
		}, nil)
}
