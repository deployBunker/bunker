package webdav

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/invalidation"
)

// ---------------------------------------------------------------------------
// BFS-062 — a frame bound the producing side is allowed to exceed is not a
// bound (the BFS-031 lesson, one bound over: BFS-031 was a COUNT, this is a
// SIZE).
//
// The contract under test is between TWO of our own components, and this file
// owns the PRODUCING half: the event frame the `events` poll form assembles is
// measured by its serialized size against the declared `max_event_bytes`, and a
// frame over it becomes the `overflow` marker — never a longer list, never a
// partial list presented as complete, and never a frame the consumer's reader
// cannot hold.
//
// The consumer half (a per-line reader sized from the declaration, an
// over-limit frame NAMED, COUNTED and NON-RETRYABLE instead of a transport fault
// that reconnects) lives in internal/fsclient (invalidate_bfs062_test.go).
//
// Everything here is measured on the wire the surface actually serves, and the
// bounds used are the SURFACE'S OWN declared numbers (the resolved
// server.invalidation.push.max_event_bytes, its declared minimum), not numbers
// chosen so an arm would pass.
// ---------------------------------------------------------------------------

// bfs062LongTree writes an ANCHOR file (so the ledger has a baseline to diff
// against) and returns the root plus a function that creates n files whose
// relative paths are ~3*segLen bytes long. Long relative paths are what make an
// ordinary change's frame large without inventing a bound: a 700-byte path is
// legal, and a few hundred of them are ordinary for a deep tree.
func bfs062LongTree(t *testing.T) (root string, create func(n, segLen int) []string) {
	t.Helper()
	root = t.TempDir()
	mustWrite(t, filepath.Join(root, "anchor.txt"), "anchor\n")
	return root, func(n, segLen int) []string {
		t.Helper()
		dir1 := strings.Repeat("a", segLen)
		dir2 := strings.Repeat("b", segLen)
		dir3 := strings.Repeat("c", segLen)
		paths := make([]string, 0, n)
		for i := 0; i < n; i++ {
			name := "f" + strings.Repeat("d", 8) + bfs062Digits(i)
			rel := filepath.ToSlash(filepath.Join(dir1, dir2, dir3, name))
			mustWrite(t, filepath.Join(root, rel), "x")
			paths = append(paths, rel)
		}
		return paths
	}
}

// bfs062Digits renders i as a fixed-width 4-digit suffix, so two runs with the
// same n produce paths of exactly the same length (an arm whose two sides
// differ in size would be measuring its own typo).
func bfs062Digits(i int) string {
	const digits = "0123456789"
	return string([]byte{
		digits[(i/1000)%10], digits[(i/100)%10], digits[(i/10)%10], digits[i%10],
	})
}

// bfs062Handler builds the surface over root with one declared frame bound, the
// way a deployment does: through Config.Invalidation, so the value passes the
// surface's own validation gate (New) and lands in the tree the events assembly
// measures against.
func bfs062Handler(t *testing.T, root string, bound int64) *Handler {
	t.Helper()
	iv := invalidation.DefaultValues()
	iv.Push.MaxEventBytes = bound
	if err := iv.Validate(); err != nil {
		t.Fatalf("the surface refused the declared bound %d: %v", bound, err)
	}
	h, err := New(Config{Root: root, Build: "test-build", Invalidation: &iv})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(h.Close)
	if got := h.tree.eventFrameBound(); got != bound {
		t.Fatalf("the tree measures frames against %d, but the surface declared %d: the published bound and the enforced bound are different numbers", got, bound)
	}
	return h
}

// bfs062ChangeFrame drives one change and returns the answer plus the serialized
// size of every event frame in it, measured with the wire encoder.
func bfs062ChangeFrame(t *testing.T, h *Handler, create func(n, segLen int) []string, n, segLen int) (eventsJSON, []int64) {
	t.Helper()
	// The snapshot the mount takes at bind is what gives the ledger a baseline;
	// the cursor it mints is 0 on a freshly-seeded ledger, which is a value and
	// not an absence, so the arm does not treat it as a failure.
	_ = seedWithSnapshot(t, h)
	create(n, segLen)
	answer := pollEvents(t, h, `{"since_seq":0}`)
	sizes := make([]int64, 0, len(answer.Result.Events))
	for _, ev := range answer.Result.Events {
		size, ok := eventFrameBytes(ev)
		if !ok {
			t.Fatalf("the frame could not be measured: %+v", ev)
		}
		sizes = append(sizes, size)
	}
	return answer, sizes
}

// TestBFS062TheCountIsNotASize is the row's own arithmetic and the producing
// side's rule in one cell:
//
//   - a COUNT does not bound a frame. 4096 paths (eventsMaxPathsPerEvent) of
//     PATH_MAX each serialize to ~16.8 MiB on ONE line, which is why a count-only
//     implementation cannot satisfy a consumer's byte cap;
//   - and the assembly therefore measures the SERIALIZED frame: the same tree
//     change that is an `invalidate` under the declared default (1 MiB) becomes
//     an `overflow` under the surface's own declared minimum (64 KiB), with the
//     byte-bound refusal counted and its size reported.
//
// The floor is the surface's declared minimum, not a number this cell invented:
// a test that lowered the bound below what the config surface accepts would be
// measuring a state no deployment can be in.
func TestBFS062TheCountIsNotASize(t *testing.T) {
	// The declared values this row's arithmetic rests on.
	if eventsMaxPathsPerEvent != 4096 {
		t.Fatalf("max_paths_per_event moved to %d; the worst-case arithmetic below pins 4096 (BFS-004 §3 E-6)", eventsMaxPathsPerEvent)
	}
	floor := int64(invalidation.MinPushMaxEventBytes)
	if floor != 64<<10 {
		t.Fatalf("the declared minimum for max_event_bytes moved to %d; this cell uses it as the boundary the surface accepts", floor)
	}

	t.Run("the worst legal frame is arithmetic, not an estimate", func(t *testing.T) {
		// PATH_MAX (4096) includes the terminating NUL, so the longest path the
		// kernel hands a walk is 4095 bytes. JSON escapes the worst byte (a
		// filename of `<`, `>` or `&`) as `\u003c` — six bytes per input byte —
		// and adds a quote at each end plus one comma between list members.
		const pathMax = 4096
		worst := strings.Repeat("<", pathMax-1)
		paths := make([]string, eventsMaxPathsPerEvent)
		for i := range paths {
			paths[i] = worst
		}
		size, ok := eventFrameBytes(eventLine{Seq: 1, Event: eventInvalidate, Paths: paths, Rev: "rev:1", Tree: "tree:0123456789abcdef"})
		if !ok {
			t.Fatal("the worst-case frame could not be measured")
		}
		if size <= int64(eventsMaxPathsPerEvent)*pathMax/4 {
			t.Fatalf("the worst legal frame measured %d bytes, which is not the order of magnitude the count bound claims to be safe for (%d paths × PATH_MAX %d)", size, eventsMaxPathsPerEvent, pathMax)
		}
		// The two caps a consumer can hold today: the client's own 8 MiB line cap
		// and the declared default bound. The worst legal frame exceeds BOTH, and
		// that is the whole reason the byte bound exists.
		if size <= 8<<20 {
			t.Fatalf("the worst legal frame measured %d bytes, which the consumer's 8 MiB reader would hold; the arms below would then be vacuous", size)
		}
		if size <= invalidation.DefaultPushMaxEventBytes {
			t.Fatalf("the worst legal frame measured %d bytes, which the declared default bound %d would admit", size, invalidation.DefaultPushMaxEventBytes)
		}
		t.Logf("worst legal frame: %d paths × %d bytes (escaped) = %d bytes = %.1f MiB", eventsMaxPathsPerEvent, pathMax, size, float64(size)/(1<<20))
	})

	t.Run("under the declared default the change is an invalidate", func(t *testing.T) {
		root, create := bfs062LongTree(t)
		h := bfs062Handler(t, root, invalidation.DefaultPushMaxEventBytes)
		_ = seedWithSnapshot(t, h)
		created := create(120, 230)
		answer := pollEvents(t, h, `{"since_seq":0}`)
		if got := eventNames(answer.Result.Events); len(got) != 1 || got[0] != eventInvalidate {
			t.Fatalf("a 120-path change under the declared 1 MiB bound answered %v, want one invalidate: the byte bound swallowed an ordinary change and the cell below would prove nothing", got)
		}
		paths := answer.Result.Events[0].Paths
		if !bfs062HoldsAll(paths, created) {
			t.Fatalf("the invalidate does not carry every changed path (%d carried, %d created): the arm must not pass on a partial list", len(paths), len(created))
		}
		size, _ := eventFrameBytes(answer.Result.Events[0])
		if size > invalidation.DefaultPushMaxEventBytes {
			t.Fatalf("the frame measured %d bytes, over the declared bound %d: the non-vacuity arm needs a frame that FITS", size, invalidation.DefaultPushMaxEventBytes)
		}
		t.Logf("the same change under the default bound: %d bytes, %d paths (120 files + the directories the change created + the touched root)", size, len(paths))
	})

	t.Run("at the declared minimum the same change overflows, counted", func(t *testing.T) {
		// Measure the frame on a tree the default bound admits, so the size
		// compared against the floor comes from the surface's own encoder.
		rootA, createA := bfs062LongTree(t)
		hA := bfs062Handler(t, rootA, invalidation.DefaultPushMaxEventBytes)
		_, sizes := bfs062ChangeFrame(t, hA, createA, 120, 230)
		frame := sizes[0]
		if frame <= floor {
			t.Fatalf("the change's frame measured %d bytes, which is not over the declared minimum %d: this arm needs a frame the floor refuses", frame, floor)
		}

		before, _ := FrameOverBoundCounters()
		rootB, createB := bfs062LongTree(t)
		hB := bfs062Handler(t, rootB, floor)
		answer, sizesB := bfs062ChangeFrame(t, hB, createB, 120, 230)

		if got := eventNames(answer.Result.Events); len(got) != 1 || got[0] != eventOverflow {
			t.Fatalf("a change whose frame measures %d bytes under a declared bound of %d answered %v, want one overflow: the count bound alone admitted an over-bound frame", frame, floor, got)
		}
		if n := len(answer.Result.Events[0].Paths); n != 0 {
			t.Fatalf("the overflow carried %d paths, want none — the rule is `overflow` with paths: [] and never a truncated or partial list", n)
		}
		if sizesB[0] > floor {
			t.Fatalf("the answer's own frame measures %d bytes, over the declared bound %d: the assembly emitted the frame it refused to assemble", sizesB[0], floor)
		}
		after, lastBytes := FrameOverBoundCounters()
		if after != before+1 {
			t.Fatalf("byte-bound refusals went %d -> %d, want exactly one: an over-bound frame must be COUNTED", before, after)
		}
		if lastBytes != frame {
			t.Fatalf("the counted frame's size is %d, want the measured %d: the count must carry the measurement that decided it", lastBytes, frame)
		}
		t.Logf("declared floor %d bytes refused a %d-byte frame (refusals %d -> %d); the answer is an overflow carrying 0 paths", floor, frame, before, after)
	})

	t.Run("a frame that fits is never rewritten", func(t *testing.T) {
		// The mutation check for the swap itself: a small change on a tree with
		// the floor declared must still be an invalidate with its paths — an
		// implementation that overflowed on every event would satisfy the arms
		// above and be useless.
		root, _ := bfs062LongTree(t)
		h := bfs062Handler(t, root, floor)
		_ = seedWithSnapshot(t, h)
		mustWrite(t, filepath.Join(root, "small.txt"), "small\n")
		answer := pollEvents(t, h, `{"since_seq":0}`)
		if got := eventNames(answer.Result.Events); len(got) != 1 || got[0] != eventInvalidate {
			t.Fatalf("a one-path change under the declared floor answered %v, want an invalidate", got)
		}
		if !bfs062Holds(answer.Result.Events[0].Paths, "small.txt") {
			t.Fatalf("the invalidate carried %v, want the changed path", answer.Result.Events[0].Paths)
		}
	})
}

// TestBFS062TheBoundIsDeclaredAndApplied is the NEGOTIATION half on the
// producing side: the number the assembly measures against is the number the
// capability document publishes, so a consumer can size its reader to it instead
// of assuming one (SPEC-push-channel §7.2, the additive `max_event_bytes`), and
// the read-back says the knob is applied rather than promising a value nothing
// obeys.
func TestBFS062TheBoundIsDeclaredAndApplied(t *testing.T) {
	cases := []struct {
		name  string
		bound int64
	}{
		{"the declared default", invalidation.DefaultPushMaxEventBytes},
		{"the declared minimum", invalidation.MinPushMaxEventBytes},
		{"an operator value", 2 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := bfs062LongTree(t)
			h := bfs062Handler(t, root, tc.bound)

			// (1) The document publishes it, and it is the number in force.
			blk := bfs062WatchBlock(t, h)
			got, ok := blk["max_event_bytes"]
			if !ok {
				t.Fatalf("the watch block does not declare max_event_bytes: a consumer cannot size its reader to a bound that is never published (%v)", bfs062Keys(blk))
			}
			if asInt(t, got) != tc.bound {
				t.Fatalf("extensions.watch.max_event_bytes = %v, want the resolved %d", got, tc.bound)
			}
			// The path bound is unchanged by this row: the count still exists,
			// it simply no longer claims to be a size.
			if asInt(t, blk["max_paths_per_event"]) != int64(eventsMaxPathsPerEvent) {
				t.Fatalf("max_paths_per_event = %v, want the declared %d (this row does not lower a count to make a size fit)", blk["max_paths_per_event"], eventsMaxPathsPerEvent)
			}

			// (2) The read-back reports the knob applied, with the reason.
			w := readInvalidationWire(t, h)
			row := knobRow(t, w, invalidation.KnobPushMaxEventBytes)
			if !row.Applied {
				t.Fatalf("max_event_bytes is not reported applied while the events assembly enforces it: %+v", row)
			}
			if !strings.Contains(row.AppliedReason, "BFS-062") || !strings.Contains(row.AppliedReason, "events") {
				t.Fatalf("the applied reason must name the assembly that obeys it: %q", row.AppliedReason)
			}
			if asInt(t, row.Value) != tc.bound {
				t.Fatalf("the read-back reports %v, want the resolved %d", row.Value, tc.bound)
			}
		})
	}
}

// bfs062WatchBlock decodes `result.capabilities.extensions.watch` verbatim, so
// this cell asserts on the wire bytes rather than on a struct that could agree
// with the encoder by construction.
func bfs062WatchBlock(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	rec := do(t, h, "POST", "/dav/", map[string]string{"X-Bunker-Op": "capabilities"}, "")
	var caps struct {
		Result struct {
			Capabilities struct {
				Extensions map[string]json.RawMessage `json:"extensions"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatalf("capabilities envelope is not JSON: %v (%s)", err, rec.Body.String())
	}
	raw, ok := caps.Result.Capabilities.Extensions["watch"]
	if !ok {
		t.Fatalf("the document carries no watch extension: %s", rec.Body.String())
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the watch block is not a JSON object: %v (%s)", err, raw)
	}
	return out
}

func bfs062Keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// bfs062Holds reports whether one path is in a frame's list.
func bfs062Holds(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// bfs062HoldsAll reports whether EVERY wanted path is in the list. It is the
// right assertion for a frame whose answer also carries the directories the
// change created and the root whose mtime moved: the row's rule is that a path
// list is complete or absent, never partial, so "every changed path is present"
// is the property, not a path count this cell would have to guess.
func bfs062HoldsAll(paths, want []string) bool {
	for _, p := range want {
		if !bfs062Holds(paths, p) {
			return false
		}
	}
	return true
}
