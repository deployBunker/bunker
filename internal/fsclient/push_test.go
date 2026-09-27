package fsclient

// ============================================================================
// BFS-036 — THE CLIENT'S HALF OF THE PUSH FORM.
//
// Three claims are made here, and each one has a control that can fail:
//
//	1. THE SWITCH READS THE OFFER. Where the server's capability document
//	   declares the pushed mode, the client learns that WITHOUT opening a stream —
//	   because opening one to ask is not a probe: `Op` applies the operation
//	   deadline and holds a pool slot while it reads a body that never ends.
//	2. A PUSHED CHANGE IS APPLIED AND ATTRIBUTED. A change made by something that
//	   is not this surface reaches the mount through the pushed channel, and the
//	   record says `mode=push`, `mechanism=watch`.
//	3. THE RECORD CANNOT BE MADE TO LIE. Against the SAME push-capable endpoint,
//	   a mount told to poll reports `mechanism=events` for a change the poll
//	   delivered — and the control in the same cell shows the same endpoint
//	   reported as `watch` when the stream is the mechanism in force, so the
//	   assertion is not vacuous (SPEC-push-channel §11.1 O-2, §12 P-16).
//
// Plus the resume half: the stream must present the SAME cursor the poll presents.
// A stream that presented a raw line counter instead would tell a mount that bound
// with a whole-tree snapshot that it holds nothing, and the server would answer
// with the interval it cannot vouch for — a resync the client did not earn, which
// is the cost BFS-063 removed for the poll.
// ============================================================================

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/davserve"
	"github.com/deployBunker/bunker/internal/invalidation"
)

// pushEndpointFor starts the landed surface over a fresh fixture tree WITH a
// watcher enabled, so the target really offers the push form. A host whose kernel
// cannot establish a watcher skips, and says so rather than passing.
func pushEndpointFor(t *testing.T, mutate func(*invalidation.Values)) (*Client, string) {
	t.Helper()
	root := fixtureTree(t)
	v := invalidation.DefaultValues()
	v.Watch.Enabled = true
	// The knob table's declared ranges (BFS-043): heartbeat_ms ∈ [100, 30000] and
	// write_deadline_ms ∈ [100, heartbeat). A cell drives the smallest HONOURABLE
	// pair rather than dialling a knob outside its own contract.
	v.Watch.HeartbeatMS = 300
	v.Push.WriteDeadlineMS = 100
	if mutate != nil {
		mutate(&v)
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("the cell's own invalidation config is invalid: %v", err)
	}
	srv, err := davserve.Serve(root, "127.0.0.1:0", davserve.WithInvalidation(v))
	if err != nil {
		t.Fatalf("davserve.Serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	c, err := NewClient(Options{BaseURL: srv.URL, Concurrency: 8, OpTimeout: 5 * time.Second, BindTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, root
}

// ---------------------------------------------------------------------------
// CELL C-1 — the switch reads the DECLARATION, and the bind does not pay a
// stream's deadline to find out.
//
// the defect it catches: a client that discovers the offer by probing the op it
// is being offered. Once `watch` is a stream, `Op` reads a body that never ends:
// the bind pays the whole bind deadline, one of the connection pool's slots is
// held for its duration, and the answer is not an envelope — so the mount is
// slower AND reports the watcher unavailable.
// ---------------------------------------------------------------------------
func TestPushClientSwitchReadsTheDeclarationNotAStreamProbe(t *testing.T) {
	c, _ := pushEndpointFor(t, nil)

	start := time.Now()
	info, oerr := c.Handshake(context.Background())
	elapsed := time.Since(start)
	if oerr != nil {
		t.Fatalf("handshake: %v", oerr)
	}
	if !info.Capabilities.WatchPushDeclared() {
		t.Skipf("this endpoint does not declare the pushed mode (%q), so there is no offer to switch on — the cell says so rather than passing", info.Capabilities.WatchMode())
	}
	if !info.WatcherAvailable {
		t.Fatalf("the document declares mode=push and the handshake still reports the watcher unavailable (source=%q): the client asked by opening a stream instead of reading the offer", info.WatcherSource)
	}
	if info.WatcherSource != WatchSourceDeclared {
		t.Fatalf("watcher source = %q, want %q: the offer must be read from the document, not measured with a second request", info.WatcherSource, WatchSourceDeclared)
	}
	// The cost is the point. A probe would pay the 2 s bind deadline this cell
	// configured; reading a field costs a round trip.
	if elapsed > time.Second {
		t.Fatalf("the bind took %s: reading the declared mode is one round trip, so a bind this slow means the client probed the stream op", elapsed)
	}
}

// ---------------------------------------------------------------------------
// CELL C-2 — the change is PUSHED, applied, and reported as the watch mechanism.
// ---------------------------------------------------------------------------
func TestPushClientAppliesAPushedChangeAndReportsTheWatchMechanism(t *testing.T) {
	c, root := pushEndpointFor(t, nil)
	info, oerr := c.Handshake(context.Background())
	if oerr != nil {
		t.Fatalf("handshake: %v", oerr)
	}
	if !info.WatcherAvailable {
		t.Skipf("no watcher is establishable on this host (source=%q): the push form is not offered, and the cell says so rather than passing", info.WatcherSource)
	}

	rec := &collectDrops{}
	// The mount's own wiring: the bind-time snapshot, then the channel, and a
	// re-observation on every resync.
	inv := mountWiring(t, c, rec, nil, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = inv.Run(ctx) }()

	waitFor(5*time.Second, func() bool { return inv.Mechanism() == MechanismWatch })
	if got := inv.Mode(); got != ModePush {
		t.Fatalf("mode = %q, want push: the client was offered the stream and took it", got)
	}
	if got := inv.State().Available; !got {
		t.Fatalf("the channel reports unavailable while it is established: %+v", inv.State())
	}

	// An edit ON THE TARGET, by something that is not this surface: no request of
	// ours is anywhere in the write path.
	mustWrite(t, filepath.Join(root, "src", "main.go"), "package main\n\nfunc main() { /* pushed */ }\n")

	waitFor(10*time.Second, func() bool { return rec.dropped("src/main.go") })
	st := inv.State()
	if st.Mechanism != MechanismWatch || st.Mode != ModePush {
		t.Fatalf("the record does not name the pushed channel after it delivered: %+v", st)
	}
	if st.Events == 0 || st.DroppedPaths == 0 {
		t.Fatalf("an event was applied but the counters stayed at zero: %+v", st)
	}
	if strings.Contains(st.Reason, "poll") {
		t.Fatalf("the record carries a poll reason while the watch mechanism is in force: %+v", st)
	}
	// The pushed channel costs O(changes): the poll's O(paths) observation is not
	// happening behind this delivery, which is the whole point of the row.
	if st.PollIntervalMS != nil {
		t.Fatalf("poll_interval_ms is reported (%d) while the client is on the pushed channel: %+v", *st.PollIntervalMS, st)
	}
}

// ---------------------------------------------------------------------------
// CELL C-3 — MECHANISM HONESTY: the record cannot be made to say `watch` when the
// poll delivered the change, AND it does say `watch` when the stream did.
//
// the defect it catches: a record that attributes a change to the mechanism the
// server OFFERED rather than the mechanism that ANSWERED — the lie
// SPEC-push-channel §11.1 O-2 forbids, and the one that makes a broken channel
// indistinguishable from a working one. The control is inside the cell: the same
// endpoint, the same change, one mount pinned to poll and one on auto, and the
// two records must differ in exactly the field under test.
// ---------------------------------------------------------------------------
func TestPushClientNeverReportsWatchWhenThePollDelivered(t *testing.T) {
	// The pinned arm: poll, against a server that offers push.
	c, root := pushEndpointFor(t, nil)
	info, oerr := c.Handshake(context.Background())
	if oerr != nil {
		t.Fatalf("handshake: %v", oerr)
	}
	if !info.Capabilities.WatchPushDeclared() {
		t.Skip("the endpoint does not offer push on this host, so the arm cannot distinguish an offer from a delivery")
	}
	rec := &collectDrops{}
	inv := NewInvalidator(c, InvalidateOptions{
		Mode:         ModePoll,
		PollInterval: 20 * time.Millisecond,
		OnDrop:       rec.drop,
		OnResync:     rec.resync,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = inv.Run(ctx) }()
	waitFor(5*time.Second, func() bool { return inv.State().Available })

	mustWrite(t, filepath.Join(root, "src", "util.go"), "package main\n\nfunc util() { /* edit */ }\n")
	waitFor(10*time.Second, func() bool { return rec.dropped("src/util.go") })

	if got := inv.Mechanism(); got != MechanismEvents {
		t.Fatalf("mechanism = %q, want %q: the poll delivered this change and the record must say so", got, MechanismEvents)
	}
	if got := inv.Mechanism(); got == MechanismWatch {
		t.Fatal("the record claims the watcher delivered a change the poll answered: the client can be made to imply PUSH (SPEC-push-channel §11.1 O-2)")
	}
	if got := inv.Mode(); got != ModePoll {
		t.Fatalf("mode = %q, want poll", got)
	}

	// THE CONTROL: the same offer, the same kind of change, a mount that is NOT
	// pinned to poll — and here the mechanism MUST be watch. Without this arm the
	// assertion above would pass for a client that had simply lost the channel
	// (there is a landed cell for the poll-only case; this one is about the
	// ATTRIBUTION, and the attribution only means something if the other value is
	// reachable on the same endpoint).
	c2, root2 := pushEndpointFor(t, nil)
	if _, oerr := c2.Handshake(context.Background()); oerr != nil {
		t.Fatalf("handshake: %v", oerr)
	}
	rec2 := &collectDrops{}
	inv2 := mountWiring(t, c2, rec2, nil, true)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { _ = inv2.Run(ctx2) }()
	waitFor(5*time.Second, func() bool { return inv2.Mechanism() == MechanismWatch })
	mustWrite(t, filepath.Join(root2, "src", "util.go"), "package main\n\nfunc util() { /* edit */ }\n")
	waitFor(10*time.Second, func() bool { return rec2.dropped("src/util.go") })
	if got := inv2.Mechanism(); got != MechanismWatch {
		t.Fatalf("the control arm reports mechanism=%q on a push-capable endpoint: the cell above would then be proving nothing about attribution", got)
	}
}

// ---------------------------------------------------------------------------
// CELL C-4 — the stream presents the OBSERVATION cursor, and nothing when it has
// observed nothing.
//
// the defect it catches: a client that resumes the stream from a raw line counter
// while the poll resumes from the declaration it holds. A mount that bound with a
// whole-tree snapshot holds an observation at a minted cursor and has applied no
// line yet, so its counter is 0 — and a server told `since_seq: 0` by a client
// whose journal has rotated answers the interval it cannot vouch for. The mount
// would then re-snapshot the tree it was told not to re-walk, on every single
// bind: the cost BFS-063 removed for the poll, reintroduced on the stream.
// ---------------------------------------------------------------------------
func TestPushClientStreamPresentsTheObservationCursor(t *testing.T) {
	var mu sync.Mutex
	var bodies []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("DAV", "1")
			w.Header().Set("Allow", "OPTIONS, GET, HEAD, POST")
			w.Header().Set("X-Bunker-Capabilities", "1")
			w.Header().Set("X-Bunker-Tree", "tree:0123456789abcdef")
			w.Header().Set("X-Bunker-Rev", "git:0123456789abcdef0123456789abcdef01234567")
			w.WriteHeader(http.StatusOK)
			return
		}
		op := strings.TrimSpace(r.Header.Get("X-Bunker-Op"))
		switch op {
		case "capabilities":
			writeStubEnvelope(w, 200, "ok", map[string]any{"capabilities": map[string]any{
				"surface": "bunkerd-webdav/1", "document_version": 1,
				"extensions": map[string]any{
					"watch": map[string]any{
						"name": "X-Bunker-Op: watch", "v": 1,
						// The OFFER: this is the carrier the client switches on.
						"mode": "push", "heartbeat_ms": 500, "max_event_bytes": 1 << 20,
						"max_paths_per_event": 4096,
						"modes":               map[string]any{"push": "X-Bunker-Op: watch", "poll": "X-Bunker-Op: events"},
					},
				},
				"degradations": []any{},
			}}, nil)
			return
		case "watch":
			raw, _ := readAllBody(r)
			mu.Lock()
			bodies = append(bodies, raw)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("X-Bunker-Tree", "tree:0123456789abcdef")
			w.WriteHeader(http.StatusOK)
			// W-1: a line, FLUSHED, so the client's establish step returns and its
			// read loop has something to time against.
			_, _ = w.Write([]byte(`{"seq":1,"event":"heartbeat","paths":[],"tree":"tree:0123456789abcdef"}` + "\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Then hold the stream open the way a real one is held.
			<-r.Context().Done()
			return
		default:
			writeStubEnvelope(w, 404, "not_found", nil, map[string]any{"detail": op})
		}
	}))
	defer srv.Close()

	c, err := NewClient(Options{BaseURL: srv.URL + "/dav", Concurrency: 4, OpTimeout: 2 * time.Second, BindTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, oerr := c.Handshake(context.Background()); oerr != nil {
		t.Fatalf("handshake: %v", oerr)
	}

	// The mount's bind: a whole-tree snapshot whose minted cursor is the
	// observation the client HOLDS, with no line applied yet.
	inv := NewInvalidator(c, InvalidateOptions{Mode: "auto", PollInterval: 50 * time.Millisecond, IdleTimeout: 30 * time.Second})
	inv.Observed(4242, true)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = inv.Run(ctx) }()

	waitFor(5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(bodies) > 0
	})
	cancel()
	mu.Lock()
	first := bodies[0]
	mu.Unlock()
	if !strings.Contains(first, `"since_seq":4242`) {
		t.Fatalf("the stream presented %s, want the observation cursor 4242: a raw line counter would present 0, and a server whose journal has rotated answers that with the interval it cannot vouch for — a resync on every bind, the cost BFS-063 removed for the poll", first)
	}

	// The honest other half: a client that has observed nothing must not name a
	// cursor it does not hold. Its counter is 0, and 0 is what it presents —
	// "my view starts at the beginning", which the server answers with the complete
	// history when it still retains it, or with the interval it cannot vouch for.
	// What must never appear is a cursor the client cannot back.
	mu.Lock()
	bodies = nil
	mu.Unlock()
	blind := NewInvalidator(c, InvalidateOptions{Mode: "auto", PollInterval: 50 * time.Millisecond, IdleTimeout: 30 * time.Second})
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { _ = blind.Run(ctx2) }()
	waitFor(5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(bodies) > 0
	})
	cancel2()
	mu.Lock()
	second := bodies[0]
	mu.Unlock()
	if !strings.Contains(second, `"since_seq":0`) {
		t.Fatalf("a client that has observed nothing presented %s: it must present cursor 0 (the honest statement of holding nothing), never a fabricated cursor", second)
	}
}

func readAllBody(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	defer r.Body.Close()
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return string(buf), nil
		}
	}
}
