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
)

// ---------------------------------------------------------------------------
// E-6's poll form: `X-Bunker-Op: events` (BFS-004 §3 E-6, §10.5).
//
// The watcher is per-target and this build has none, so `watch` answers
// capability_unavailable with mode=poll. THIS is the mode it names, and the
// client's invalidation path depends on it: `events` is the poll form the
// client prefers over its last-resort revision poll. A build that declares the
// poll form and does not serve it leaves the mount with no mechanism at all —
// which is what BFS-026 found: the client reports the dead channel honestly and
// the cache never invalidates.
//
// What this op can know decides the events it emits, and the difference is not
// cosmetic:
//
//   - It OBSERVES the served tree (one bounded walk, stat-only, no reads) and
//     compares each path against the identity it recorded last. That identity
//     is (size, mtime, ctime): ctime is in it because an edit that preserves
//     size AND mtime is exactly the shape a metadata-keyed observer would
//     otherwise miss, and this release has already recorded that class as a
//     defect (BFS-009 F1). Where the platform exposes no ctime the identity
//     falls back to (size, mtime) and that gap is real (see filetime_other.go).
//   - It CANNOT see anything between two observations, and it holds no history
//     from before its first one. Both are why a cursor it cannot vouch for is
//     answered `overflow` — the channel's own word for "knowledge lost, drop
//     everything and re-snapshot" — rather than an empty list, which would be a
//     claim that nothing happened.
//   - It cannot see a change the filesystem itself does not report (a rewriter
//     that restores size, mtime AND ctime is invisible to any observer that
//     does not read the bytes). The write path's content-hash re-validation
//     (§6.1) is what covers that case; a change channel is not.
//
// Read-only (E-4's invariant, A-9): a walk and a stat per entry, nothing else.
// Bounded: the walk stops at eventsScanLimit entries, a path list is capped at
// eventsMaxPathsPerEvent (spilling to `overflow`, never to a longer or partial
// list), and the ledger keeps eventsJournalEvents of history. A client whose
// cursor is older than that history is answered with the retained events, whose
// first seq is a gap its own rule already knows how to read (BFS-005 §4.1:
// "the missing range is never re-requested").
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
type identity struct {
	Size  int64
	Mtime int64 // unix nanoseconds
	Ctime int64 // unix nanoseconds, or 0 where the platform has none
	Dir   bool
}

func identityOf(fi os.FileInfo) identity {
	return identity{
		Size:  fi.Size(),
		Mtime: fi.ModTime().UnixNano(),
		Ctime: ctimeUnixNano(fi),
		Dir:   fi.IsDir(),
	}
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
	observed map[string]identity
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

// seedEvents records an observation the ledger did not have to walk for. The
// `snapshot` op of a whole tree IS an observation of that tree, so a client
// that binds by taking one has its baseline recorded from the very state it
// received: the first poll then answers a diff against the client's own view
// instead of declaring the interval before it lost. A partial observation
// (a subtree, a truncated result) is not a baseline and is ignored.
func (t *tree) seedEvents(state map[string]identity, whole bool) {
	if !whole || len(state) == 0 {
		return
	}
	l := t.eventLedger()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started {
		return
	}
	l.started = true
	l.observed = state
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

// changed lists the paths whose observed identity moved, appeared or vanished,
// sorted so one tree state always produces one path list.
func (l *eventLog) changed(state map[string]identity) []string {
	var out []string
	for p, id := range state {
		if prev, ok := l.observed[p]; !ok || prev != id {
			out = append(out, p)
		}
	}
	for p := range l.observed {
		if _, ok := state[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// push appends one event, advancing the seq and pruning the journal to its
// bound. Callers hold l.mu.
func (l *eventLog) push(t *tree, name string, paths []string) eventLine {
	l.seq++
	if paths == nil {
		paths = []string{}
	}
	ev := eventLine{Seq: l.seq, Event: name, Paths: paths, Rev: t.revToken(), Tree: t.identity()}
	l.journal = append(l.journal, ev)
	if len(l.journal) > eventsJournalEvents {
		l.journal = append([]eventLine(nil), l.journal[len(l.journal)-eventsJournalEvents:]...)
	}
	l.base = l.journal[0].Seq
	return ev
}

// pollEvents observes the tree once and answers the events a client at cursor
// has not seen.
//
// The ledger lock is taken BEFORE the observation and held across it, so two
// clients polling at once are strictly ordered: each diff is against the state
// the previous observation recorded, and the recorded baseline can never move
// backwards. The price is that a second client's poll waits for one walk
// (measured at ~7 ms on a 2000-path tree, §8 of the row report) — paid only when
// two clients poll the same tree in the same instant.
func (t *tree) pollEvents(cursor int64) (eventsAnswer, error) {
	l := t.eventLedger()
	l.mu.Lock()
	defer l.mu.Unlock()

	state, truncated, err := t.observe()
	if err != nil {
		return eventsAnswer{}, err
	}

	if !l.started {
		// The ledger's knowledge begins here, and the interval before it is
		// unknown rather than empty. `overflow` is that statement in the
		// channel's own vocabulary, and it makes the client re-establish its
		// view — which is also what closes the window between a client's own
		// snapshot and this first observation.
		l.started = true
		l.observed = state
		l.push(t, eventOverflow, nil)
	} else {
		changed := l.changed(state)
		l.observed = state
		switch {
		case truncated || len(changed) > eventsMaxPathsPerEvent:
			// Past the cap (or an incomplete observation): knowledge lost, not a
			// partial path list presented as a diff.
			l.push(t, eventOverflow, nil)
		case len(changed) > 0:
			l.push(t, eventInvalidate, changed)
		}
	}

	if len(l.journal) == 0 {
		// Nothing was ever observed here and nothing is owed: an honest empty
		// answer, not a synthesis of one.
		return eventsAnswer{Events: []eventLine{}, Scanned: len(state), Truncated: truncated, Head: l.seq}, nil
	}
	if cursor > l.seq {
		// The cursor is ahead of anything this ledger has issued: a restarted
		// process, or a cursor minted against a different tree instance. The
		// range in between was never observed here, so it is declared lost —
		// and the seq is advanced PAST the cursor first, because an event at or
		// below the client's own seq is one a client is right to discard as a
		// duplicate (BFS-005 §4.1's replay rule).
		l.seq = cursor
		ev := l.push(t, eventOverflow, nil)
		return eventsAnswer{Events: []eventLine{ev}, Scanned: len(state), Truncated: truncated, Head: l.seq}, nil
	}
	out := make([]eventLine, 0, len(l.journal))
	for _, ev := range l.journal {
		if ev.Seq > cursor {
			out = append(out, ev)
		}
	}
	return eventsAnswer{Events: out, Scanned: len(state), Truncated: truncated, Head: l.seq}, nil
}

// handleEvents serves E-6's poll form: `since_seq?` in, `result.events` out.
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
	cursor := int64(0)
	if args.SinceSeq != nil {
		cursor = *args.SinceSeq
	}
	if cursor < 0 {
		h.writeEnvelope(w, r, start, "events", 400, VerdictBadArguments, false, nil,
			&envelopeError{Detail: "events since_seq must be zero or positive"})
		return
	}
	answer, err := h.tree.pollEvents(cursor)
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
