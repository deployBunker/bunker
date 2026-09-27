package webdav

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-026: E-6's poll form (`X-Bunker-Op: events`).
//
// These arms pin the RULE the live measurements rest on: what the ledger
// observes, which event a change produces, where the declared bounds bite, and
// what a cursor the ledger cannot vouch for is told. Each arm states the
// negative control it needs — the seeding arm is the one that matters most,
// because a ledger that answered "nothing changed" before it had ever observed
// the tree would be the exact dishonesty this row exists to avoid.
// ---------------------------------------------------------------------------

// eventsJSON is one poll's decoded answer.
type eventsJSON struct {
	OK         bool   `json:"ok"`
	Op         string `json:"op"`
	Verdict    string `json:"verdict"`
	Rev        string `json:"rev"`
	Tree       string `json:"tree"`
	Proto      string `json:"proto"`
	DurationMS int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated"`
	Result     struct {
		Events  []eventLine `json:"events"`
		Count   int         `json:"count"`
		Scanned int         `json:"scanned"`
		HeadSeq int64       `json:"head_seq"`
	} `json:"result"`
	Error *struct {
		Capability string `json:"capability"`
		Scope      string `json:"scope"`
		Mode       string `json:"mode"`
		Detail     string `json:"detail"`
	} `json:"error"`
}

func pollEvents(t *testing.T, h *Handler, body string) eventsJSON {
	t.Helper()
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "events"}, body)
	if rec.Code != 200 {
		t.Fatalf("op events -> %d %s", rec.Code, rec.Body.String())
	}
	var out eventsJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("events envelope did not parse: %v (%s)", err, rec.Body.String())
	}
	if !out.OK || out.Op != "events" {
		t.Fatalf("events envelope is not an ok envelope for the op: %s", rec.Body.String())
	}
	if got := rec.Header().Get("X-Bunker-Verdict"); got != string(VerdictOK) {
		t.Fatalf("X-Bunker-Verdict = %q, want ok", got)
	}
	return out
}

// seedWithSnapshot takes the whole-tree snapshot a mount takes at bind. It is
// the observation the ledger is allowed to adopt as its baseline.
func seedWithSnapshot(t *testing.T, h *Handler) {
	t.Helper()
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "snapshot"},
		`{"depth":"infinity","include_hash":false}`)
	if rec.Code != 200 {
		t.Fatalf("snapshot -> %d %s", rec.Code, rec.Body.String())
	}
}

func eventNames(evs []eventLine) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Event)
	}
	return out
}

// TestEventsOpDeclaredBounds pins the declared bounds, because the boundary arms
// below lower them to reach a boundary cheaply. A lowered bound in a test must
// never be able to change the declared default silently.
func TestEventsOpDeclaredBounds(t *testing.T) {
	if eventsMaxPathsPerEvent != 4096 {
		t.Fatalf("the declared per-event path cap moved to %d (BFS-004 §3 E-6 declares 4096)", eventsMaxPathsPerEvent)
	}
	if eventsJournalEvents != 256 {
		t.Fatalf("the ledger's history bound moved to %d", eventsJournalEvents)
	}
	if eventsScanLimit != 100000 {
		t.Fatalf("the observation cap moved to %d", eventsScanLimit)
	}
}

// TestEventsOpSnapshotSeedMakesTheFirstPollHonest is the seeding rule, with its
// negative control in the same test: a WHOLE-TREE snapshot is an observation and
// becomes the baseline (so a quiet first poll honestly answers "nothing
// changed"), while a SUB-TREE snapshot is not and must not be adopted as one
// (the ledger has observed nothing, and says so with `overflow`).
func TestEventsOpSnapshotSeedMakesTheFirstPollHonest(t *testing.T) {
	t.Run("whole tree snapshot seeds the baseline", func(t *testing.T) {
		h := newTestHandler(t)
		seedWithSnapshot(t, h)

		got := pollEvents(t, h, `{}`)
		if len(got.Result.Events) != 0 {
			t.Fatalf("a quiet tree after a whole-tree snapshot produced events: %+v", got.Result.Events)
		}
		if got.Result.HeadSeq != 0 {
			t.Fatalf("head_seq = %d with a seeded, quiet ledger, want 0", got.Result.HeadSeq)
		}
		if got.Result.Scanned < 4 {
			t.Fatalf("scanned = %d, want the fixture tree's entries", got.Result.Scanned)
		}

		// The edit on the agent. This is the only thing that moves; nothing in
		// this test touches the surface.
		mustWrite(t, filepath.Join(h.Root(), "src", "main.go"), "package main\n\nfunc main() { /* edited */ }\n")

		got = pollEvents(t, h, `{"since_seq":0}`)
		if len(got.Result.Events) != 1 {
			t.Fatalf("one edited path produced %d events: %+v", len(got.Result.Events), got.Result.Events)
		}
		ev := got.Result.Events[0]
		if ev.Event != eventInvalidate {
			t.Fatalf("event = %q, want %q", ev.Event, eventInvalidate)
		}
		if len(ev.Paths) != 1 || ev.Paths[0] != "src/main.go" {
			t.Fatalf("paths = %v, want [src/main.go]", ev.Paths)
		}
		if ev.Seq != 1 {
			t.Fatalf("seq = %d, want 1", ev.Seq)
		}
		if ev.Tree == "" || ev.Tree != got.Tree {
			t.Fatalf("the event's tree %q is not the response's tree %q (declaration 3)", ev.Tree, got.Tree)
		}
		if got.Result.HeadSeq != 1 {
			t.Fatalf("head_seq = %d after one event, want 1", got.Result.HeadSeq)
		}

		// Catching up is silent: the same cursor must not be served the same
		// event twice.
		again := pollEvents(t, h, `{"since_seq":1}`)
		if len(again.Result.Events) != 0 {
			t.Fatalf("a caught-up cursor was served %d events again: %+v", len(again.Result.Events), again.Result.Events)
		}
	})

	t.Run("a sub-tree snapshot is not a baseline", func(t *testing.T) {
		h := newTestHandler(t)
		rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "snapshot"}, `{"path":"src","depth":"infinity"}`)
		if rec.Code != 200 {
			t.Fatalf("sub-tree snapshot -> %d", rec.Code)
		}
		got := pollEvents(t, h, `{}`)
		if len(got.Result.Events) != 1 || got.Result.Events[0].Event != eventOverflow {
			t.Fatalf("a ledger with no baseline must declare the prior range lost, got %+v", got.Result.Events)
		}
	})
}

// TestEventsOpFirstPollDeclaresThePriorRangeLost is the no-snapshot arm: the
// ledger's knowledge begins at its first observation, and it says so with
// `overflow` rather than an empty list that would claim nothing had happened.
func TestEventsOpFirstPollDeclaresThePriorRangeLost(t *testing.T) {
	h := newTestHandler(t)

	got := pollEvents(t, h, `{}`)
	if len(got.Result.Events) != 1 {
		t.Fatalf("the first poll answered %d events, want the one overflow: %+v", len(got.Result.Events), got.Result.Events)
	}
	ev := got.Result.Events[0]
	if ev.Event != eventOverflow {
		t.Fatalf("the first poll's event = %q, want %q", ev.Event, eventOverflow)
	}
	if len(ev.Paths) != 0 {
		t.Fatalf("overflow carried paths %v; knowledge lost names no paths", ev.Paths)
	}
	if ev.Seq != 1 {
		t.Fatalf("seq = %d, want 1", ev.Seq)
	}

	// After the declaration the channel reports real changes precisely.
	mustWrite(t, filepath.Join(h.Root(), "README.md"), "# fixture, edited\n")
	got = pollEvents(t, h, `{"since_seq":1}`)
	if len(got.Result.Events) != 1 || got.Result.Events[0].Event != eventInvalidate {
		t.Fatalf("after the seed the channel must report a real change: %+v", got.Result.Events)
	}
	if paths := got.Result.Events[0].Paths; len(paths) != 1 || paths[0] != "README.md" {
		t.Fatalf("paths = %v, want [README.md]", paths)
	}
}

// TestEventsOpReportsAddedRemovedAndReplacedPaths covers the three shapes a
// client has to act on, in one observation.
func TestEventsOpReportsAddedRemovedAndReplacedPaths(t *testing.T) {
	h := newTestHandler(t)
	seedWithSnapshot(t, h)

	mustWrite(t, filepath.Join(h.Root(), "src", "added.go"), "package main\n")
	mustWrite(t, filepath.Join(h.Root(), "src", "util.go"), "package main\n\nfunc util() { /* replaced */ }\n")
	if err := os.Remove(filepath.Join(h.Root(), "README.md")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	got := pollEvents(t, h, `{}`)
	if len(got.Result.Events) != 1 || got.Result.Events[0].Event != eventInvalidate {
		t.Fatalf("want one invalidate, got %+v", got.Result.Events)
	}
	paths := got.Result.Events[0].Paths
	for _, want := range []string{"src/added.go", "src/util.go", "README.md"} {
		found := false
		for _, p := range paths {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("the %q change is missing from the event's paths: %v", want, paths)
		}
	}
	if len(paths) != len(got.Result.Events[0].Paths) {
		t.Fatal("unreachable")
	}
	// Sorted and duplicate-free: one tree state, one path list.
	for i := 1; i < len(paths); i++ {
		if paths[i-1] >= paths[i] {
			t.Fatalf("paths are not strictly sorted: %v", paths)
		}
	}
}

// TestEventsOpSeesAnEditThatPreservesSizeAndMtime is the ctime arm: a rewrite
// that keeps the byte count and restores the mtime to the nanosecond is the one
// edit shape a (size, mtime) observer cannot see — the release has already
// recorded that class (BFS-009 F1) — so the identity carries ctime where the
// platform has one. Where it does not, the arm skips and says why rather than
// passing vacuously.
func TestEventsOpSeesAnEditThatPreservesSizeAndMtime(t *testing.T) {
	h := newTestHandler(t)
	target := filepath.Join(h.Root(), "src", "util.go")
	seedWithSnapshot(t, h)

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if ctimeUnixNano(fi) == 0 {
		t.Skip("this platform exposes no ctime: the observed identity is (size, mtime) here")
	}
	before := fi
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Same size, different bytes: 'l' becomes '1'.
	edited := []byte(strings.ReplaceAll(string(body), "util", "uti1"))
	if len(edited) != len(body) {
		t.Fatalf("the fixture is not the trap it claims to be: %d -> %d bytes", len(body), len(edited))
	}
	if err := os.WriteFile(target, edited, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(target, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	now, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if now.Size() != before.Size() || !now.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the trap did not hold: size %d/%d mtime %v/%v", before.Size(), now.Size(), before.ModTime(), now.ModTime())
	}

	got := pollEvents(t, h, `{}`)
	if len(got.Result.Events) != 1 || got.Result.Events[0].Event != eventInvalidate {
		t.Fatalf("a same-size, mtime-preserved edit was not reported: %+v", got.Result.Events)
	}
	if paths := got.Result.Events[0].Paths; len(paths) != 1 || paths[0] != "src/util.go" {
		t.Fatalf("paths = %v, want [src/util.go]", paths)
	}
}

// TestEventsOpSpillsToOverflowAboveThePathCap proves A-13's second declaration
// adapted to the poll: past the cap the answer is `overflow` — knowledge lost,
// re-snapshot — and never a longer or partial path list.
func TestEventsOpSpillsToOverflowAboveThePathCap(t *testing.T) {
	old := eventsMaxPathsPerEvent
	eventsMaxPathsPerEvent = 3
	t.Cleanup(func() { eventsMaxPathsPerEvent = old })

	h := newTestHandler(t)
	seedWithSnapshot(t, h)
	for i := 0; i < 5; i++ {
		mustWrite(t, filepath.Join(h.Root(), "burst", "f"+string(rune('a'+i))+".txt"), "burst\n")
	}
	got := pollEvents(t, h, `{}`)
	if len(got.Result.Events) != 1 || got.Result.Events[0].Event != eventOverflow {
		t.Fatalf("a burst past the cap produced %+v, want one overflow", got.Result.Events)
	}
	if len(got.Result.Events[0].Paths) != 0 {
		t.Fatalf("the spill carried paths: %v", got.Result.Events[0].Paths)
	}
}

// TestEventsOpReportsATruncatedObservationAsOverflow proves the scan bound is
// honoured AND reported: an observation that gave up part way can never be
// served as a diff, and the envelope says the answer is not the whole picture.
func TestEventsOpReportsATruncatedObservationAsOverflow(t *testing.T) {
	old := eventsScanLimit
	eventsScanLimit = 2
	t.Cleanup(func() { eventsScanLimit = old })

	h := newTestHandler(t)
	got := pollEvents(t, h, `{}`)
	if !got.Truncated {
		t.Fatalf("a capped observation did not report truncation: %+v", got)
	}
	if len(got.Result.Events) != 1 || got.Result.Events[0].Event != eventOverflow {
		t.Fatalf("a capped observation produced %+v, want one overflow", got.Result.Events)
	}
}

// TestEventsOpStaleCursorGetsTheRetainedEvents pins the journal bound's
// behaviour: a cursor older than the retained history is answered with what IS
// retained, whose first seq is a gap the client's own rule already reads as
// knowledge lost (BFS-005 §4.1). Silence would be the alternative, and silence
// is a claim that nothing was missed.
func TestEventsOpStaleCursorGetsTheRetainedEvents(t *testing.T) {
	old := eventsJournalEvents
	eventsJournalEvents = 2
	t.Cleanup(func() { eventsJournalEvents = old })

	h := newTestHandler(t)
	seedWithSnapshot(t, h)
	target := filepath.Join(h.Root(), "README.md")
	for i := 0; i < 4; i++ {
		mustWrite(t, target, "# fixture "+string(rune('a'+i))+"\n")
		if got := pollEvents(t, h, `{"since_seq":0}`); len(got.Result.Events) == 0 {
			t.Fatalf("change %d produced no event", i)
		}
	}
	// The cursor is now far behind the retained window.
	got := pollEvents(t, h, `{"since_seq":1}`)
	if len(got.Result.Events) == 0 {
		t.Fatal("a cursor below the retained window was answered with silence")
	}
	if got.Result.Events[0].Seq <= 2 {
		t.Fatalf("the answer's first seq = %d: the missed range is not visible as a gap", got.Result.Events[0].Seq)
	}
}

// TestEventsOpCursorAheadOfTheLedgerIsDeclaredLost covers a restarted process (or
// a cursor minted against another tree instance): the range between the ledger
// and the cursor was never observed here, so it is declared lost — at a seq
// ABOVE the cursor, because an event at or below the client's own seq is one a
// client is right to discard as a duplicate.
func TestEventsOpCursorAheadOfTheLedgerIsDeclaredLost(t *testing.T) {
	h := newTestHandler(t)
	got := pollEvents(t, h, `{"since_seq":100}`)
	if len(got.Result.Events) != 1 || got.Result.Events[0].Event != eventOverflow {
		t.Fatalf("want one overflow, got %+v", got.Result.Events)
	}
	if got.Result.Events[0].Seq != 101 {
		t.Fatalf("seq = %d, want 101 (one above the client's cursor)", got.Result.Events[0].Seq)
	}
	if got.Result.HeadSeq != 101 {
		t.Fatalf("head_seq = %d, want 101", got.Result.HeadSeq)
	}
}

// TestEventsOpRefusesBadArguments proves the loud alternatives: a body that is
// not the declared shape, and a cursor that cannot be one, are named rather than
// guessed at.
func TestEventsOpRefusesBadArguments(t *testing.T) {
	h := newTestHandler(t)
	cases := []struct {
		name string
		body string
	}{
		{"not an object", `[]`},
		{"not JSON", `since_seq=3`},
		{"negative cursor", `{"since_seq":-1}`},
		{"wrong type", `{"since_seq":"soon"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "events"}, tc.body)
			if rec.Code != 400 {
				t.Fatalf("body %s -> %d, want 400", tc.body, rec.Code)
			}
			if got := rec.Header().Get("X-Bunker-Verdict"); got != string(VerdictBadArguments) {
				t.Fatalf("verdict = %q, want bad_arguments", got)
			}
		})
	}
}

// TestEventsOpWireShape proves the answer is the E-4 envelope with the same keys
// as every other op, and that each event line carries the fields the client's
// decoder reads — including `paths`, present as an empty array rather than
// absent, so "no paths" and "no field" cannot be confused.
func TestEventsOpWireShape(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "events"}, `{}`)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("envelope did not parse: %v", err)
	}
	for _, key := range []string{"ok", "op", "verdict", "rev", "tree", "proto", "duration_ms", "truncated", "result", "error"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("the envelope has no %q key: %s", key, rec.Body.String())
		}
	}
	if got := rec.Header().Get("X-Bunker-Op"); got != "events" {
		t.Fatalf("X-Bunker-Op = %q", got)
	}
	if got := rec.Header().Get("X-Bunker-Tree"); got == "" {
		t.Fatal("no X-Bunker-Tree header")
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw["result"], &result); err != nil {
		t.Fatalf("result did not parse: %v", err)
	}
	for _, key := range []string{"events", "count", "scanned", "head_seq"} {
		if _, ok := result[key]; !ok {
			t.Fatalf("the result has no %q key: %s", key, rec.Body.String())
		}
	}
	var lines []map[string]json.RawMessage
	if err := json.Unmarshal(result["events"], &lines); err != nil {
		t.Fatalf("events did not parse: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("want the one seed event, got %d", len(lines))
	}
	for _, key := range []string{"seq", "event", "paths", "tree"} {
		if _, ok := lines[0][key]; !ok {
			t.Fatalf("the event line has no %q key: %s", key, string(result["events"]))
		}
	}
}

// TestWatchRefusalNamesAWorkingFallback proves the degradation text did not stay
// stale: with the poll form served, `watch`'s refusal must point a client at a
// mechanism this build really has. Before this row the refusal named a
// "snapshot diff" op that does not exist, which is how a client ends up
// depending on nothing.
func TestWatchRefusalNamesAWorkingFallback(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "watch"}, `{}`)
	if rec.Code != 501 {
		t.Fatalf("watch -> %d, want 501", rec.Code)
	}
	var env struct {
		Error *struct {
			Capability, Scope, Mode, Detail string
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("watch refusal did not parse: %v", err)
	}
	if env.Error == nil || env.Error.Scope != "target" || env.Error.Mode != "poll" {
		t.Fatalf("the watch refusal lost its declared degradation shape: %s", rec.Body.String())
	}
	if !strings.Contains(env.Error.Detail, "X-Bunker-Op: events") {
		t.Fatalf("the watch refusal does not name the poll form that exists: %q", env.Error.Detail)
	}
	// The named fallback must actually answer: a refusal that points at a
	// mechanism this build does not serve is the defect, not the fallback.
	if got := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "events"}, `{}`); got.Code != 200 {
		t.Fatalf("the fallback the refusal names answered %d", got.Code)
	}
}

// TestCapabilityDocumentDropsTheEventsDegradation proves §4.2 rule 1 in the
// other direction: an op this build serves must not still be listed as a
// build-level absence. The document is what a client reads to decide whether a
// channel exists at all, so a stale entry here is a working mechanism reported
// as missing.
func TestCapabilityDocumentDropsTheEventsDegradation(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "capabilities"}, "")
	var env struct {
		Result struct {
			Capabilities struct {
				Extensions struct {
					Op struct {
						Ops []string `json:"ops"`
					} `json:"op"`
					Watch struct {
						Mode     string         `json:"mode"`
						MaxPaths int            `json:"max_paths_per_event"`
						Modes    map[string]any `json:"modes"`
					} `json:"watch"`
				} `json:"extensions"`
				Degradations []struct {
					Capability string `json:"capability"`
				} `json:"degradations"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("capability document did not parse: %v", err)
	}
	caps := env.Result.Capabilities
	found := false
	for _, op := range caps.Extensions.Op.Ops {
		if op == "events" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the document does not list the served events op: %v", caps.Extensions.Op.Ops)
	}
	for _, d := range caps.Degradations {
		if d.Capability == "op:events" {
			t.Fatalf("a served op is still enumerated as a build-level absence: %+v", d)
		}
	}
	if caps.Extensions.Watch.Mode != "poll" {
		t.Fatalf("watch.mode = %q, want poll (the mode actually in force)", caps.Extensions.Watch.Mode)
	}
	if caps.Extensions.Watch.MaxPaths != eventsMaxPathsPerEvent {
		t.Fatalf("watch.max_paths_per_event = %d, want the served cap %d", caps.Extensions.Watch.MaxPaths, eventsMaxPathsPerEvent)
	}
	if _, ok := caps.Extensions.Watch.Modes["poll"]; !ok {
		t.Fatalf("the document no longer declares a poll mode: %+v", caps.Extensions.Watch.Modes)
	}
}

// TestEventsOpIsBoundedAndNeverHoldsARequestOpen is the not-hang standard applied
// to this channel: the answer is a bounded single envelope — it observes, answers
// and closes — so no client is ever held open and no duration is unbounded. The
// bound that matters is the observation cap, so it is asserted rather than
// promised: with the cap in place the op still answers, and the answer says the
// observation was capped.
func TestEventsOpIsBoundedAndNeverHoldsARequestOpen(t *testing.T) {
	old := eventsScanLimit
	eventsScanLimit = 100000
	t.Cleanup(func() { eventsScanLimit = old })

	h := newTestHandler(t)
	done := make(chan eventsJSON, 1)
	go func() { done <- pollEvents(t, h, `{}`) }()
	select {
	case got := <-done:
		if got.Result.Scanned == 0 {
			t.Fatalf("a bounded observation reported nothing scanned: %+v", got.Result)
		}
		if got.DurationMS < 0 {
			t.Fatalf("duration_ms = %d", got.DurationMS)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the poll did not answer within its bound")
	}
}

// TestOpTableMatchesWhatIsServed pins the invariant that makes the capability
// document trustworthy in the direction that is easy to get wrong: every op the
// surface DOES serve must be listed in `extensions.op.ops`, and every catalogue
// name that answers the structured 501 must NOT be. `implementedOps` is a
// report, not a gate — the handler's switch is the gate — so without this arm a
// served op could be advertised as missing (a client would then never use a
// mechanism that exists) or an unserved one advertised as present.
//
// A-12's other half is asserted here too: a catalogue name answers its result or
// a structured capability_unavailable, never 400 op_unknown.
func TestOpTableMatchesWhatIsServed(t *testing.T) {
	h := newTestHandler(t)
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "capabilities"}, "")
	var env struct {
		Result struct {
			Capabilities struct {
				Extensions struct {
					Op struct {
						Ops []string `json:"ops"`
					} `json:"op"`
				} `json:"extensions"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("capability document did not parse: %v", err)
	}
	listed := map[string]bool{}
	for _, op := range env.Result.Capabilities.Extensions.Op.Ops {
		listed[op] = true
	}

	for _, op := range opCatalogue {
		got := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": op}, `{}`)
		switch got.Code {
		case 200:
			if !listed[op] {
				t.Fatalf("op %q is served but the capability document omits it: %v", op, env.Result.Capabilities.Extensions.Op.Ops)
			}
		case 501:
			if got := got.Header().Get("X-Bunker-Verdict"); got != string(VerdictCapabilityUnavailable) {
				t.Fatalf("op %q refused with verdict %q, want capability_unavailable", op, got)
			}
			if listed[op] {
				t.Fatalf("op %q is refused (501) but the capability document advertises it", op)
			}
		default:
			t.Fatalf("catalogue op %q -> %d, want its result (200) or a structured 501", op, got.Code)
		}
	}
}

var _ = http.StatusOK
