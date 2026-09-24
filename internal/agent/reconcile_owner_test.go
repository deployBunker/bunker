package agent

// DF-BUNKER-18 regression tests: the per-orphan port-range test alone cannot
// protect two daemons that share ONE host when (a) the orphan's
// `<home>/.bunker/ports` file is missing or unreadable (spawn only WARNS when
// that write fails), or (b) the two daemons' pools OVERLAP. Both shapes keep
// the orphan classified as ownable, and reconciliation then adopts it (fails)
// or destroys it — from the wrong daemon.
//
// The fix is an explicit ownership marker: spawn stamps this daemon's
// restart-stable instance id into `<home>/.bunker/owner`, and reconciliation
// treats an orphan whose marker names a DIFFERENT instance as foreign — in
// adopt mode and in destroy mode — regardless of the persisted port range.
// An orphan with no marker (or a daemon with no identity of its own) keeps
// today's behaviour byte for byte: disjoint persisted ports are foreign,
// missing/malformed/in-pool-unreservable metadata stays fail-closed.
//
// Every test injects the destroy seam: nothing here may touch host users.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// Fake daemon identities. The marker must be written and read verbatim, so
// the tests use fixed 32-hex-char ids rather than random ones.
const (
	ownInstanceID   = "0123456789abcdef0123456789abcdef"
	otherInstanceID = "fedcba9876543210fedcba9876543210"
)

// instanceIDPattern is the shape loadOrCreateDaemonInstanceID must produce:
// 32 lowercase hex chars (16 random bytes).
var instanceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// seedOwnerMarker writes the `.bunker/owner` marker spawn would have written.
// poolLine "" leaves the informational second line out entirely.
func seedOwnerMarker(t *testing.T, home, instanceID, poolLine string) {
	t.Helper()
	dir := filepath.Join(home, ".bunker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	content := instanceID + "\n"
	if poolLine != "" {
		content += poolLine + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "owner"), []byte(content), 0o644); err != nil {
		t.Fatalf("write owner marker: %v", err)
	}
}

// ownerManager is foreignManager plus a resolved daemon identity: the same
// harness (temp durable registry, real allocator on the 10000-19999 pool,
// recording destroy seam, captured log), with m.instanceID set as
// NewAgentManager would have set it.
func ownerManager(t *testing.T, path, instanceID string, adoptMode bool, buf *bytes.Buffer) (*AgentManager, *destroyRecorder) {
	t.Helper()
	m, rec := foreignManager(t, path, adoptMode, buf)
	m.instanceID = instanceID
	return m, rec
}

// assertOwnerSkipWarning pins the extended skip line: it must still carry the
// original attributes (agent, persisted range, pool bounds, reason) AND name
// the owning instance plus the pool geometry that daemon recorded.
func assertOwnerSkipWarning(t *testing.T, log, ownerID, ownerPool string) {
	t.Helper()
	for _, want := range []string{
		"skipping foreign orphan agent",
		"agent_id=foreign-agent",
		"system_user=bunker-foreign-agent",
		"pool=10000-19999",
		"another daemon instance",
		"owner=" + ownerID,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("ownership-skip warning missing %q — log line:\n%s", want, log)
		}
	}
	if ownerPool != "" {
		if want := "owner_pool=" + ownerPool; !strings.Contains(log, want) {
			t.Errorf("ownership-skip warning missing %q — log line:\n%s", want, log)
		}
	}
}

// ── T1: another instance's marker is never adopted and never destroyed ──

// TestReconcile_ForeignOwnerMarkerIsNeverAdoptedOrDestroyed is the row's PASS
// test. The orphan's persisted port range (12300-12399) lies INSIDE this
// daemon's pool (10000-19999) — exactly the overlapping-pools shape the port
// test cannot catch — and in the nastier rows the port metadata is missing or
// malformed, which today's code treats as ownable. The ownership marker is
// the only thing that can save the agent, in both modes.
func TestReconcile_ForeignOwnerMarkerIsNeverAdoptedOrDestroyed(t *testing.T) {
	cases := []struct {
		name      string
		adoptMode bool
		metadata  string // written to <home>/.bunker/ports; "" writes nothing
		wantRange string // persisted_range the skip line must report
		ownerPool string // the pool line the OTHER daemon recorded
	}{
		{
			name:      "adopt mode, in-pool range, overlapping pools",
			adoptMode: true,
			metadata:  "12300-12399\n",
			wantRange: "12300-12399",
			ownerPool: "10000-19999", // the other daemon runs the SAME pool
		},
		{
			name:      "destroy mode, in-pool range, overlapping pools",
			metadata:  "12300-12399\n",
			wantRange: "12300-12399",
			ownerPool: "10000-19999",
		},
		{
			// In-pool but unreservable (misaligned): today's code fails
			// closed and destroys it — the marker must still win.
			name:      "adopt mode, in-pool misaligned range",
			adoptMode: true,
			metadata:  "10050-10149\n",
			wantRange: "10050-10149",
			ownerPool: "10000-19999",
		},
		{
			name:      "adopt mode, port metadata missing",
			adoptMode: true,
			wantRange: "0-0",
			ownerPool: "10000-19999",
		},
		{
			// The residual shape (a): spawn's metadata write FAILED, so the
			// range is unreadable/absent and the port test cannot classify it.
			name:      "destroy mode, port metadata missing",
			wantRange: "0-0",
			ownerPool: "10000-19999",
		},
		{
			name:      "adopt mode, port metadata malformed",
			adoptMode: true,
			metadata:  "not-a-port-range\n",
			wantRange: "0-0",
			ownerPool: "10000-19999",
		},
		{
			name:      "destroy mode, marker without a pool line",
			metadata:  "12300-12399\n",
			wantRange: "12300-12399",
			ownerPool: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.jsonl")
			var buf bytes.Buffer
			m, rec := ownerManager(t, path, ownInstanceID, tc.adoptMode, &buf)
			defer m.registry.Close()

			home := t.TempDir()
			if tc.metadata != "" {
				writePortMetadata(t, home, tc.metadata)
			}
			seedOwnerMarker(t, home, otherInstanceID, tc.ownerPool)
			m.listSystemAgents = func() ([]SystemAgent, error) {
				return []SystemAgent{{AgentID: "foreign-agent", Username: "bunker-foreign-agent", Home: home}}, nil
			}

			rep := m.Reconcile(context.Background())

			if got := rec.calls(); len(got) != 0 {
				t.Errorf("destroy calls = %v, want 0 — an orphan owned by another daemon instance must never be destroyed", got)
			}
			if rep.Foreign != 1 {
				t.Errorf("Foreign = %d, want 1 — report %+v", rep.Foreign, rep)
			}
			if rep.Destroyed != 0 || rep.Adopted != 0 {
				t.Errorf("report = %+v, want 0 destroyed / 0 adopted", rep)
			}
			// The agent stays exactly as the other daemon left it: no
			// tracker record, no registry record, no port reservation.
			assertNoHalfManagedState(t, m, "foreign-agent")
			if !poolIsEmpty(m.portAlloc) {
				t.Errorf("foreign skip leaked pool capacity: Available() = %d, MaxRanges() = %d",
					m.portAlloc.Available(), m.portAlloc.MaxRanges())
			}
			log := buf.String()
			assertOwnerSkipWarning(t, log, otherInstanceID, tc.ownerPool)
			if want := "persisted_range=" + tc.wantRange; !strings.Contains(log, want) {
				t.Errorf("skip warning missing %q — log line:\n%s", want, log)
			}
		})
	}
}

// ── T2: a marker naming THIS daemon keeps today's handling ─────────────

// TestReconcile_OwnOwnerMarkerKeepsTodaysHandling: our own marker means the
// agent is ours, so an orphan with a valid in-pool reservation is adopted (or
// destroyed in destroy mode) exactly as before. The marker's recorded pool
// line is informational and must NOT change the verdict — a pool-geometry
// change on this daemon would otherwise reclassify its own agents as foreign
// and leak them forever.
func TestReconcile_OwnOwnerMarkerKeepsTodaysHandling(t *testing.T) {
	cases := []struct {
		name       string
		adoptMode  bool
		ownerID    string
		ownerPool  string
		metadata   string
		wantAdopt  bool
		wantDestro bool
	}{
		{
			name:      "adopt mode, own marker, valid in-pool range",
			adoptMode: true,
			ownerID:   ownInstanceID,
			ownerPool: "10000-19999",
			metadata:  "12300-12399\n",
			wantAdopt: true,
		},
		{
			name:       "destroy mode, own marker, valid in-pool range",
			ownerID:    ownInstanceID,
			ownerPool:  "10000-19999",
			metadata:   "12300-12399\n",
			wantDestro: true,
		},
		{
			// The recorded pool DISAGREES with ours (an older geometry):
			// still ours, because the pool line is never part of the
			// ownership decision.
			name:      "adopt mode, own marker, recorded pool disagrees",
			adoptMode: true,
			ownerID:   ownInstanceID,
			ownerPool: "30000-30099",
			metadata:  "12300-12399\n",
			wantAdopt: true,
		},
		{
			name:       "destroy mode, own marker, recorded pool disagrees",
			ownerID:    ownInstanceID,
			ownerPool:  "30000-30099",
			metadata:   "12300-12399\n",
			wantDestro: true,
		},
		{
			name:      "adopt mode, own marker, no recorded pool",
			adoptMode: true,
			ownerID:   ownInstanceID,
			metadata:  "12300-12399\n",
			wantAdopt: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.jsonl")
			var buf bytes.Buffer
			m, rec := ownerManager(t, path, ownInstanceID, tc.adoptMode, &buf)
			defer m.registry.Close()

			home := t.TempDir()
			writePortMetadata(t, home, tc.metadata)
			seedOwnerMarker(t, home, tc.ownerID, tc.ownerPool)
			m.listSystemAgents = func() ([]SystemAgent, error) {
				return []SystemAgent{{AgentID: "foreign-agent", Username: "bunker-foreign-agent", Home: home}}, nil
			}
			// DF-BUNKER-53: the adopt-mode rows resolve the agent's uid/gid
			// for the docker-unit stage; stub the lookup for the fabricated
			// username (gap075 convention) and install no-op host seams so
			// no test ever runs real systemd.
			stubUser(t, "bunker-foreign-agent")
			installNoopAdoptSeams(t, m)

			rep := m.Reconcile(context.Background())

			if rep.Foreign != 0 {
				t.Errorf("Foreign = %d, want 0 — this daemon's OWN agent must not be treated as foreign", rep.Foreign)
			}
			if strings.Contains(buf.String(), "skipping foreign orphan agent") {
				t.Error("own-owned orphan must not be logged as a foreign skip")
			}
			switch {
			case tc.wantAdopt:
				if rep.Adopted != 1 || rep.Destroyed != 0 {
					t.Fatalf("report = %+v, want 1 adopted / 0 destroyed", rep)
				}
				if got := rec.calls(); len(got) != 0 {
					t.Errorf("destroy calls = %v, want 0", got)
				}
				if s, e, ok := m.portAlloc.AllocatedRange("foreign-agent"); !ok || s != 12300 || e != 12399 {
					t.Errorf("adopted reservation = %d-%d (ok=%v), want exact 12300-12399", s, e, ok)
				}
				if m.registry.Get("foreign-agent") == nil {
					t.Error("adopted agent was not persisted to the registry")
				}
			case tc.wantDestro:
				if rep.Destroyed != 1 || rep.Adopted != 0 {
					t.Fatalf("report = %+v, want 1 destroyed / 0 adopted", rep)
				}
				assertDestroyedOnce(t, rec, "foreign-agent")
				assertNoHalfManagedState(t, m, "foreign-agent")
			}
		})
	}
}

// ── T3: our own marker keeps the fail-closed path ─────────────────────

// TestReconcile_OwnOwnerMarkerStillFailsClosedWithoutPortMetadata: the
// ownership check must not loosen the fail-closed rules for our OWN agents.
// A matching marker with missing/malformed port metadata is still destroyed,
// in both modes, and never logged as a foreign skip.
func TestReconcile_OwnOwnerMarkerStillFailsClosedWithoutPortMetadata(t *testing.T) {
	cases := []struct {
		name      string
		adoptMode bool
		metadata  string
	}{
		{name: "adopt mode, port metadata missing", adoptMode: true},
		{name: "adopt mode, port metadata malformed", adoptMode: true, metadata: "not-a-range\n"},
		{name: "adopt mode, in-pool misaligned range", adoptMode: true, metadata: "10050-10149\n"},
		{name: "destroy mode, port metadata missing"},
		{name: "destroy mode, port metadata malformed", metadata: "abc-def\n"},
		{name: "destroy mode, in-pool misaligned range", metadata: "10050-10149\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.jsonl")
			var buf bytes.Buffer
			m, rec := ownerManager(t, path, ownInstanceID, tc.adoptMode, &buf)
			defer m.registry.Close()

			home := t.TempDir()
			if tc.metadata != "" {
				writePortMetadata(t, home, tc.metadata)
			}
			seedOwnerMarker(t, home, ownInstanceID, "10000-19999")
			m.listSystemAgents = func() ([]SystemAgent, error) {
				return []SystemAgent{{AgentID: "foreign-agent", Username: "bunker-foreign-agent", Home: home}}, nil
			}

			rep := m.Reconcile(context.Background())

			if rep.Destroyed != 1 || rep.Adopted != 0 {
				t.Fatalf("report = %+v, want 1 destroyed / 0 adopted (fail-closed unchanged)", rep)
			}
			if rep.Foreign != 0 {
				t.Errorf("Foreign = %d, want 0 — a matching marker must not be foreign", rep.Foreign)
			}
			if strings.Contains(buf.String(), "skipping foreign orphan agent") {
				t.Error("fail-closed destroy must not be logged as a foreign skip")
			}
			assertDestroyedOnce(t, rec, "foreign-agent")
			assertNoHalfManagedState(t, m, "foreign-agent")
		})
	}
}

// TestReconcile_OwnOwnerMarkerWithDisjointPortsTakesTheNormalPath pins the
// deliberate consequence of rule (b): the marker classifies ownership, so an
// agent carrying OUR marker is never parked as "foreign" by the port test,
// even when its persisted range is disjoint from our current pool (a pool
// geometry change). It takes the ordinary path instead — adoption fails
// closed, and the orphan is destroyed from the daemon that actually owns it.
func TestReconcile_OwnOwnerMarkerWithDisjointPortsTakesTheNormalPath(t *testing.T) {
	cases := []struct {
		name      string
		adoptMode bool
	}{
		{name: "adopt mode", adoptMode: true},
		{name: "destroy mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.jsonl")
			var buf bytes.Buffer
			m, rec := ownerManager(t, path, ownInstanceID, tc.adoptMode, &buf)
			defer m.registry.Close()

			home := t.TempDir()
			// 30000-30099 is disjoint from our 10000-19999 pool: the legacy
			// classifier would call it foreign, the marker says it is ours.
			writePortMetadata(t, home, "30000-30099\n")
			seedOwnerMarker(t, home, ownInstanceID, "10000-19999")
			m.listSystemAgents = func() ([]SystemAgent, error) {
				return []SystemAgent{{AgentID: "foreign-agent", Username: "bunker-foreign-agent", Home: home}}, nil
			}

			rep := m.Reconcile(context.Background())
			if rep.Foreign != 0 {
				t.Errorf("Foreign = %d, want 0 — our own marker must not be treated as foreign", rep.Foreign)
			}
			if rep.Destroyed != 1 || rep.Adopted != 0 {
				t.Fatalf("report = %+v, want 1 destroyed / 0 adopted", rep)
			}
			assertDestroyedOnce(t, rec, "foreign-agent")
			assertNoHalfManagedState(t, m, "foreign-agent")
			if strings.Contains(buf.String(), "skipping foreign orphan agent") {
				t.Error("our own orphan must not be logged as a foreign skip")
			}
		})
	}
}

// ── T4: marker-absent (and identity-less) behaviour is unchanged ───────

// TestOrphanIsForeignLegacyCasesWithInstanceIdentity pins rule (c): with a
// marker absent (or unreadable, or empty) the classifier behaves exactly as
// before, byte for byte — even though this daemon DOES have an instance
// identity to compare against. The last two rows pin rule (b) precedence: a
// marker naming THIS daemon makes the agent ours, so the disjoint-port test no
// longer applies to it (a foreign-marker-by-ports agent that our own marker
// claims is handled here, fail-closed, not leaked).
func TestOrphanIsForeignLegacyCasesWithInstanceIdentity(t *testing.T) {
	cases := []struct {
		name     string
		metadata string // <home>/.bunker/ports
		owner    string // <home>/.bunker/owner raw content; "" writes no file
		want     bool
		wantLow  uint32
		wantHigh uint32
	}{
		{name: "no marker, disjoint range above the pool", metadata: "30000-30099\n", want: true, wantLow: 30000, wantHigh: 30099},
		{name: "no marker, disjoint range below the pool", metadata: "9000-9099\n", want: true, wantLow: 9000, wantHigh: 9099},
		{name: "no marker, in-pool range", metadata: "12300-12399\n", wantLow: 12300, wantHigh: 12399},
		{name: "no marker, in-pool misaligned range", metadata: "10050-10149\n", wantLow: 10050, wantHigh: 10149},
		{name: "no marker, malformed metadata", metadata: "not-a-range\n"},
		{name: "no marker, missing metadata"},
		// A marker whose first line is empty/blank is NOT a marker: the
		// legacy port classification applies unchanged (disjoint => foreign).
		{name: "empty marker file", metadata: "30000-30099\n", owner: "\n", want: true, wantLow: 30000, wantHigh: 30099},
		{name: "marker with an empty instance id", metadata: "30000-30099\n", owner: "\n10000-19999\n", want: true, wantLow: 30000, wantHigh: 30099},
		{name: "marker with a blank instance id", metadata: "30000-30099\n", owner: "   \n10000-19999\n", want: true, wantLow: 30000, wantHigh: 30099},
		{name: "blank marker, in-pool range", metadata: "12300-12399\n", owner: "\n10000-19999\n", wantLow: 12300, wantHigh: 12399},
		// Rule (b) precedence: a marker naming THIS daemon makes the agent
		// OURS, so the marker — not the ports — decides. A disjoint range on
		// an agent we stamped is our own leftover and takes the normal
		// adopt/destroy path (see the reconcile-level test below), instead of
		// being parked as "foreign" forever.
		{name: "own marker, disjoint range is ours by marker", metadata: "30000-30099\n", owner: ownInstanceID + "\n10000-19999\n", wantLow: 30000, wantHigh: 30099},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.jsonl")
			m := newRegistryManager(t, path)
			defer m.registry.Close()
			// The comparison target exists; only the marker decides.
			m.instanceID = ownInstanceID

			home := t.TempDir()
			if tc.metadata != "" {
				writePortMetadata(t, home, tc.metadata)
			}
			if tc.owner != "" {
				seedOwnerMarkerRaw(t, home, tc.owner)
			}

			got, start, end := m.orphanIsForeign(SystemAgent{
				AgentID: "orphan", Username: "bunker-orphan", Home: home,
			})
			if got != tc.want {
				t.Errorf("orphanIsForeign = %v, want %v", got, tc.want)
			}
			if start != tc.wantLow || end != tc.wantHigh {
				t.Errorf("reported range = %d-%d, want %d-%d", start, end, tc.wantLow, tc.wantHigh)
			}
		})
	}
}

// TestOrphanIsForeignWithoutAllocatorNeverSkipsByPorts pins the other half of
// the legacy contract: with no allocator this daemon can prove nothing foreign
// from ports — but an ownership marker still stands on its own, which is why
// the skip log is nil-safe.
func TestOrphanIsForeignWithoutAllocatorNeverSkipsByPorts(t *testing.T) {
	cases := []struct {
		name     string
		metadata string
		owner    string
		want     bool
	}{
		{name: "no allocator, no marker", metadata: "30000-30099\n"},
		{name: "no allocator, foreign marker", metadata: "30000-30099\n", owner: otherInstanceID + "\n30000-30099", want: true},
		{name: "no allocator, no marker and no metadata"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.jsonl")
			m := newRegistryManager(t, path)
			defer m.registry.Close()
			m.portAlloc = nil
			m.instanceID = ownInstanceID

			home := t.TempDir()
			if tc.metadata != "" {
				writePortMetadata(t, home, tc.metadata)
			}
			if tc.owner != "" {
				id, poolLine, _ := strings.Cut(tc.owner, "\n")
				seedOwnerMarker(t, home, id, poolLine)
			}

			got, _, _ := m.orphanIsForeign(SystemAgent{AgentID: "orphan", Username: "bunker-orphan", Home: home})
			if got != tc.want {
				t.Errorf("orphanIsForeign = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── Marker format ────────────────────────────────────────────────────

// TestReadPersistedOwnerParsing pins the documented two-line format and its
// lenient second line: the POOL LINE is informational, so a missing or
// unparseable pool still yields a usable instance id, while an unreadable
// file, an empty file or an empty first line all mean "no marker".
func TestReadPersistedOwnerParsing(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		writeFile bool
		wantID    string
		wantPool  [2]uint32
		wantOK    bool
	}{
		{name: "both lines", content: ownInstanceID + "\n10000-19999\n", writeFile: true, wantID: ownInstanceID, wantPool: [2]uint32{10000, 19999}, wantOK: true},
		{name: "id only, no trailing newline", content: ownInstanceID, writeFile: true, wantID: ownInstanceID, wantOK: true},
		{name: "id line only", content: ownInstanceID + "\n", writeFile: true, wantID: ownInstanceID, wantOK: true},
		{name: "unparseable pool line", content: ownInstanceID + "\nnot-a-pool\n", writeFile: true, wantID: ownInstanceID, wantOK: true},
		{name: "empty pool line", content: ownInstanceID + "\n\n", writeFile: true, wantID: ownInstanceID, wantOK: true},
		{name: "blank pool line, third line ignored", content: ownInstanceID + "\n\n30000-30099\n", writeFile: true, wantID: ownInstanceID, wantOK: true},
		{name: "id padded with whitespace", content: "  " + ownInstanceID + "  \n10000-19999\n", writeFile: true, wantID: ownInstanceID, wantPool: [2]uint32{10000, 19999}, wantOK: true},
		{name: "missing file", writeFile: false},
		{name: "empty file", content: "", writeFile: true},
		{name: "whitespace-only first line", content: "   \n10000-19999\n", writeFile: true},
		{name: "leading newline", content: "\n" + ownInstanceID + "\n", writeFile: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.writeFile {
				seedOwnerMarkerRaw(t, home, tc.content)
			}
			id, start, end, ok := readPersistedOwner(home)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (id=%q pool=%d-%d)", ok, tc.wantOK, id, start, end)
			}
			if id != tc.wantID {
				t.Errorf("instance id = %q, want %q", id, tc.wantID)
			}
			if start != tc.wantPool[0] || end != tc.wantPool[1] {
				t.Errorf("pool = %d-%d, want %d-%d", start, end, tc.wantPool[0], tc.wantPool[1])
			}
		})
	}

	t.Run("empty home is not a marker", func(t *testing.T) {
		if id, _, _, ok := readPersistedOwner(""); ok || id != "" {
			t.Errorf("readPersistedOwner(\"\") = %q, ok=%v; want no marker", id, ok)
		}
	})

	t.Run("owner path is a directory", func(t *testing.T) {
		home := t.TempDir()
		if err := os.MkdirAll(persistedOwnerPath(home), 0o755); err != nil {
			t.Fatalf("mkdir over marker path: %v", err)
		}
		if id, _, _, ok := readPersistedOwner(home); ok || id != "" {
			t.Errorf("readPersistedOwner(dir) = %q, ok=%v; want no marker", id, ok)
		}
	})
}

// seedOwnerMarkerRaw writes literal bytes to `<home>/.bunker/owner` so the
// parser's boundary cases can be exercised without the writer's formatting.
func seedOwnerMarkerRaw(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".bunker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "owner"), []byte(content), 0o644); err != nil {
		t.Fatalf("write owner marker: %v", err)
	}
}

// ── Spawn-side writer ────────────────────────────────────────────────

// TestOwnerMarkerWriterMatchesReconcileReader proves the spawn-side writer and
// the reconcile-side reader agree: what a daemon writes for its own agent is
// read back as OUR marker (agent stays ownable), while the same bytes with a
// different id read back as another instance's (agent is skipped). It also
// pins that a daemon with no identity writes NO marker at all — the legacy
// marker-absent case — so spawn never fabricates ownership it cannot prove.
func TestOwnerMarkerWriterMatchesReconcileReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.jsonl")
	m := newRegistryManager(t, path)
	defer m.registry.Close()
	m.instanceID = ownInstanceID

	home := t.TempDir()
	metaDir := filepath.Join(home, ".bunker")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	if err := m.writeOwnerMarker(metaDir); err != nil {
		t.Fatalf("writeOwnerMarker: %v", err)
	}

	id, poolStart, poolEnd, ok := readPersistedOwner(home)
	if !ok {
		t.Fatal("marker written by spawn is not readable by reconcile")
	}
	if id != ownInstanceID {
		t.Errorf("marker instance id = %q, want %q", id, ownInstanceID)
	}
	wantStart, wantEnd := m.portAlloc.Bounds()
	if poolStart != wantStart || poolEnd != wantEnd {
		t.Errorf("marker pool = %d-%d, want the allocator bounds %d-%d", poolStart, poolEnd, wantStart, wantEnd)
	}

	// The format is exactly two newline-terminated lines.
	raw, err := os.ReadFile(persistedOwnerPath(home))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if got, want := string(raw), fmt.Sprintf("%s\n%d-%d\n", ownInstanceID, wantStart, wantEnd); got != want {
		t.Errorf("marker content = %q, want %q", got, want)
	}

	// A daemon with no identity must not claim ownership.
	anonymous := newRegistryManager(t, filepath.Join(t.TempDir(), "agents.jsonl"))
	defer anonymous.registry.Close()
	anonHome := t.TempDir()
	anonDir := filepath.Join(anonHome, ".bunker")
	if err := os.MkdirAll(anonDir, 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	if err := anonymous.writeOwnerMarker(anonDir); err != nil {
		t.Fatalf("writeOwnerMarker without identity: %v", err)
	}
	if _, err := os.Stat(persistedOwnerPath(anonHome)); !os.IsNotExist(err) {
		t.Errorf("daemon without an instance identity must write no marker, stat err = %v", err)
	}
	if _, _, _, ok := readPersistedOwner(anonHome); ok {
		t.Error("no marker written, but readPersistedOwner reported one")
	}
}

// ── T5: the identity is restart-stable ───────────────────────────────

// idManager mirrors newRegistryManager but points the agent data dir at a
// temp dir, so the daemon identity file is created under test control and
// never in the host's /var/lib/bunkerd.
func idManager(t *testing.T, baseDir string) (*AgentManager, *destroyRecorder) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefaultConfig()
	cfg.Agent.BaseDataDir = baseDir
	isolateRegistry(t, cfg)
	if cfg.Agent.PortRangePerAgent == 0 {
		cfg.Agent.PortRangePerAgent = 100
	}
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	m := NewAgentManager(cfg, logger, tracker, nil, nil)
	m.listSystemAgents = func() ([]SystemAgent, error) { return nil, nil }
	rec := &destroyRecorder{}
	m.destroyAgent = rec.seam()
	t.Cleanup(m.Stop)
	return m, rec
}

// TestDaemonInstanceIDIsStableAcrossRestarts: the identity file is created
// once (0600, 32 hex chars) and READ on every later start. Regenerating it
// would make this daemon's own agents read as foreign after every restart and
// leak forever, so stability is the whole contract.
func TestDaemonInstanceIDIsStableAcrossRestarts(t *testing.T) {
	baseDir := t.TempDir()

	first, _ := idManager(t, baseDir)
	if first.instanceID == "" {
		t.Fatal("first daemon start resolved no instance identity")
	}
	if !instanceIDPattern.MatchString(first.instanceID) {
		t.Errorf("instance id = %q, want 32 lowercase hex chars", first.instanceID)
	}

	idPath := daemonInstanceIDPath(baseDir)
	info, err := os.Stat(idPath)
	if err != nil {
		t.Fatalf("stat instance identity: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("instance identity mode = %#o, want 0600 (no group/other bits)", perm)
	}
	before, err := os.ReadFile(idPath)
	if err != nil {
		t.Fatalf("read instance identity: %v", err)
	}
	if got := strings.TrimSpace(string(before)); got != first.instanceID {
		t.Errorf("file holds %q, want the resolved id %q", got, first.instanceID)
	}

	// A restart is a NEW AgentManager over the SAME data dir.
	second, secondRec := idManager(t, baseDir)
	if second.instanceID != first.instanceID {
		t.Fatalf("restart regenerated the instance identity (%q -> %q): this daemon's own agents would become foreign",
			first.instanceID, second.instanceID)
	}
	after, err := os.ReadFile(idPath)
	if err != nil {
		t.Fatalf("re-read instance identity: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("instance identity file changed across restarts: %q -> %q", before, after)
	}

	// Consequence: an agent spawned before the restart still carries THIS
	// daemon's marker, so the restarted daemon does not treat it as foreign.
	home := t.TempDir()
	writePortMetadata(t, home, "12300-12399\n")
	seedOwnerMarker(t, home, first.instanceID, "10000-19999")
	second.listSystemAgents = func() ([]SystemAgent, error) {
		return []SystemAgent{{AgentID: "own-agent", Username: "bunker-own-agent", Home: home}}, nil
	}
	rep := second.Reconcile(context.Background())
	if rep.Foreign != 0 {
		t.Errorf("Foreign = %d, want 0 — a restart must not orphan this daemon's own agent", rep.Foreign)
	}
	if rep.Destroyed != 1 {
		t.Errorf("Destroyed = %d, want 1 (own agent, destroy mode) — report %+v", rep.Destroyed, rep)
	}
	assertDestroyedOnce(t, secondRec, "own-agent")
}

// TestDaemonInstanceIDUnavailableDegradesToLegacyOwnership: an identity that
// cannot be created or read (no base data dir, unwritable dir) must never be
// fatal and must never fail closed into destroying MORE — it degrades to the
// legacy behaviour, where every marker counts as absent.
func TestDaemonInstanceIDUnavailableDegradesToLegacyOwnership(t *testing.T) {
	cases := []struct {
		name    string
		baseDir func(t *testing.T) string
	}{
		{name: "no base data dir configured", baseDir: func(t *testing.T) string { return "" }},
		{name: "identity file is a directory", baseDir: func(t *testing.T) string {
			dir := t.TempDir()
			if err := os.MkdirAll(daemonInstanceIDPath(dir), 0o755); err != nil {
				t.Fatalf("mkdir over identity path: %v", err)
			}
			return dir
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			cfg := config.DefaultConfig()
			cfg.Agent.BaseDataDir = tc.baseDir(t)
			isolateRegistry(t, cfg)
			tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
			m := NewAgentManager(cfg, logger, tracker, nil, nil)
			t.Cleanup(m.Stop)
			if m.instanceID != "" {
				t.Fatalf("instance id = %q, want empty when it cannot be resolved", m.instanceID)
			}

			var buf bytes.Buffer
			m.logger = slog.New(slog.NewTextHandler(&buf, nil))
			rec := &destroyRecorder{}
			m.destroyAgent = rec.seam()

			home := t.TempDir()
			writePortMetadata(t, home, "12300-12399\n")
			// An agent another daemon would have marked: with no identity of
			// our own we cannot honour the marker, so it stays ownable.
			seedOwnerMarker(t, home, otherInstanceID, "10000-19999")
			m.listSystemAgents = func() ([]SystemAgent, error) {
				return []SystemAgent{{AgentID: "foreign-agent", Username: "bunker-foreign-agent", Home: home}}, nil
			}

			rep := m.Reconcile(context.Background())
			if rep.Foreign != 0 {
				t.Errorf("Foreign = %d, want 0 — without an identity every marker reads as absent", rep.Foreign)
			}
			if rep.Destroyed != 1 {
				t.Errorf("Destroyed = %d, want 1 — report %+v", rep.Destroyed, rep)
			}
			assertDestroyedOnce(t, rec, "foreign-agent")
		})
	}
}
