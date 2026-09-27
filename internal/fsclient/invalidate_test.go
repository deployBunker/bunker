package fsclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// BFS-026, in code: does the channel DELIVER an agent-side edit when the target
// has no watcher, and does the record say which mechanism delivered it?
//
// These arms drive the same objects the mount wires together — a real client
// against the landed surface, the invalidator, the drop callback, the resync
// callback — with no kernel in the middle. What they cannot show (that the
// kernel's caches are actually dropped, that a stale read becomes fresh) is the
// live arm's job; what they CAN show exactly is the rule: which mechanism
// answered, whether an event arrived, and whether silence is reported as
// silence.
// ---------------------------------------------------------------------------

// collectDrops records what the invalidator tells the mount.
type collectDrops struct {
	mu      sync.Mutex
	paths   [][]string
	full    int
	resyncs []string
}

func (c *collectDrops) drop(paths []string, full bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if full {
		c.full++
		return
	}
	c.paths = append(c.paths, append([]string(nil), paths...))
}

func (c *collectDrops) resync(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resyncs = append(c.resyncs, reason)
}

// dropped reports whether any drop carried path p.
func (c *collectDrops) dropped(p string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, batch := range c.paths {
		for _, got := range batch {
			if got == p {
				return true
			}
		}
	}
	return false
}

func (c *collectDrops) summary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return "drops=" + jsonish(c.paths) + " full=" + strconv.Itoa(c.full) + " resyncs=" + jsonish(c.resyncs)
}

func jsonish(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	return string(raw)
}

// waitFor polls cond for up to d, and reports whether it became true.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// TestInvalidatorDeliversThePollFormOfTheChannel is this row's acceptance: with
// the watcher unavailable on the target, an edit made on the agent reaches the
// client through the DECLARED POLL FORM, and the record names that mechanism —
// `events`, which is a poll, never `watch`.
func TestInvalidatorDeliversThePollFormOfTheChannel(t *testing.T) {
	c, root, _ := fixtureEndpoint(t)

	// The state at bind: no watcher on this target, and a poll form this build
	// really serves. If either half is wrong there is no channel at all, which
	// is what this row was filed for.
	info, oerr := c.Handshake(context.Background())
	if oerr != nil {
		t.Fatalf("handshake: %v", oerr)
	}
	if info.WatcherAvailable {
		t.Fatal("the watcher reported available on a target without one")
	}
	if !info.PollOpAvailable {
		t.Fatal("the declared poll form is not served, so the channel has no mechanism (BFS-026)")
	}

	rec := &collectDrops{}
	inv := NewInvalidator(c, InvalidateOptions{
		Mode:         "auto",
		PollInterval: 20 * time.Millisecond,
		OnDrop:       rec.drop,
		OnResync:     rec.resync,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = inv.Run(ctx) }()

	// Wait for the first answered poll rather than assuming a cadence.
	if !waitFor(5*time.Second, func() bool { return inv.State().Available }) {
		t.Fatalf("the channel never answered a poll: %+v", inv.State())
	}
	if got := inv.Mode(); got != ModePoll {
		t.Fatalf("mode = %q, want poll: a mount answered by polling must never report push", got)
	}
	if got := inv.Mechanism(); got != MechanismEvents {
		t.Fatalf("mechanism = %q, want %q (the poll form the surface serves)", got, MechanismEvents)
	}
	if got := inv.Mechanism(); got == MechanismWatch {
		t.Fatal("the record claims the watcher answered on a target without one")
	}

	// An edit ON THE AGENT: no request of ours, no surface call. The client's
	// only way to learn about it is the channel.
	mustWrite(t, filepath.Join(root, "src", "main.go"), "package main\n\nfunc main() { /* edited on the agent */ }\n")

	if !waitFor(5*time.Second, func() bool { return rec.dropped("src/main.go") }) {
		t.Fatalf("no invalidate carried src/main.go within the declared window: %s (state %+v)",
			rec.summary(), inv.State())
	}

	st := inv.State()
	if !st.Available {
		t.Fatalf("the channel reported unavailable after delivering: %+v", st)
	}
	if st.Events == 0 {
		t.Fatalf("an event was applied but events_total stayed 0: %+v", st)
	}
	if st.DroppedPaths == 0 {
		t.Fatalf("a path was dropped but paths_dropped_total stayed 0: %+v", st)
	}
	// A per-path invalidation is what the poll form buys: the mount is told
	// WHICH path moved, so it drops that path rather than re-snapshotting the
	// whole tree. The one full resync allowed here is the ledger's own first
	// answer, which declares the interval before it observed lost.
	if rec.full > 1 {
		t.Fatalf("a single edit caused %d whole-tree resyncs: %s", rec.full, rec.summary())
	}
}

// TestInvalidatorStaysHonestWhenThePollFormIsRefused is the other half of the
// row's first requirement, and it pins the state the row was filed against: a
// server that declares the poll form and does not serve it must leave the
// record saying so — channel_available=false, events_total=0, the server's own
// refusal in `reason` — with NOTHING delivered. A record that claimed a channel
// here would be worse than the bug, so this arm must fail if the honesty is
// ever traded for a greener-looking status.
func TestInvalidatorStaysHonestWhenThePollFormIsRefused(t *testing.T) {
	// A surface that advertises the poll form and refuses both mechanism calls:
	// the shape the landed build had before this row.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("DAV", "1")
			w.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND, POST")
			w.Header().Set("X-Bunker-Capabilities", "1")
			w.Header().Set("X-Bunker-Extensions", "identity,if_match_refuse,rev,tree,op,watch")
			w.Header().Set("X-Bunker-Tree", "tree:0123456789abcdef")
			w.Header().Set("X-Bunker-Rev", "git:0123456789abcdef0123456789abcdef01234567")
			w.WriteHeader(http.StatusOK)
			return
		}
		op := strings.TrimSpace(r.Header.Get("X-Bunker-Op"))
		if op == "capabilities" {
			writeStubEnvelope(w, 200, "ok", map[string]any{"capabilities": map[string]any{
				"surface": "bunkerd-webdav/1", "document_version": 1,
				"extensions": map[string]any{
					"watch": map[string]any{
						"name": "X-Bunker-Op: watch", "v": 1, "mode": "poll",
						"modes": map[string]any{"push": "inotify\u2192stream", "poll": "X-Bunker-Op: events"},
					},
				},
				"degradations": []any{},
			}}, nil)
			return
		}
		// watch and events: the declared degradation, exactly as the landed
		// build answered before this row.
		writeStubEnvelope(w, 501, "capability_unavailable", nil, map[string]any{
			"capability": op, "scope": "target", "mode": "poll",
			"detail": "no inotify watcher on this target",
		})
	}))
	defer srv.Close()

	c, err := NewClient(Options{BaseURL: srv.URL + "/dav", Concurrency: 4, OpTimeout: 5 * time.Second, BindTimeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// The mount's own order: bind (Handshake) and then run the invalidator. The
	// handshake is what records which mechanisms the build serves, and it is
	// measured here rather than assumed — this arm is about what the record says,
	// so it must be reached the way production reaches it.
	if _, oerr := c.Handshake(context.Background()); oerr != nil {
		t.Fatalf("handshake: %v", oerr)
	}
	rec := &collectDrops{}
	inv := NewInvalidator(c, InvalidateOptions{
		Mode:         "auto",
		PollInterval: 20 * time.Millisecond,
		OnDrop:       rec.drop,
		OnResync:     rec.resync,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = inv.Run(ctx) }()

	// Let several poll intervals pass: the fallback must run and keep failing.
	time.Sleep(400 * time.Millisecond)

	st := inv.State()
	if st.Available {
		t.Fatalf("channel_available=true while every mechanism call is refused — the record would be lying: %+v", st)
	}
	if st.Events != 0 {
		t.Fatalf("events_total=%d with no channel: %+v", st.Events, st)
	}
	if st.DroppedPaths != 0 {
		t.Fatalf("paths_dropped_total=%d with no channel: %+v", st.DroppedPaths, st)
	}
	if st.Mechanism == MechanismWatch {
		t.Fatal("the record claims the watcher answered a target that refuses watch")
	}
	if !strings.Contains(st.Reason, "capability_unavailable") {
		t.Fatalf("the server's own refusal is not quoted in the reason: %q", st.Reason)
	}
	if got := len(rec.paths) + rec.full; got != 0 {
		t.Fatalf("the mount was told to drop %d times with no channel: %s", got, rec.summary())
	}
}

// writeStubEnvelope writes the E-4 envelope shape of BFS-004 §10.4.
func writeStubEnvelope(w http.ResponseWriter, status int, verdict string, result any, eerr map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Bunker-Verdict", verdict)
	w.Header().Set("X-Bunker-Tree", "tree:0123456789abcdef")
	w.Header().Set("X-Bunker-Rev", "git:0123456789abcdef0123456789abcdef01234567")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": status == http.StatusOK, "op": "", "verdict": verdict,
		"rev": "git:0123456789abcdef0123456789abcdef01234567", "tree": "tree:0123456789abcdef",
		"proto": "HTTP/1.1", "duration_ms": 0, "truncated": false,
		"result": result, "error": eerr,
	})
}
