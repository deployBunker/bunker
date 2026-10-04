//go:build linux

// Package fsmount — BFS-052's mutation controls.
//
// The cells here prove the assertions above are ALIVE: that they pass on
// the real probe and FAIL when the behaviour they name is broken. A green
// battery whose cells pass on a mutated probe proves nothing — the same
// rule BFS-019/020/025/032/039 all landed with.
package fsmount

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bfs052FixtureFor builds and installs the one fixture every control uses:
// a complete, readable kernel surface for mountpoint with device dev.
func bfs052FixtureFor(t *testing.T, mountpoint, dev string, readAheadKB string) {
	t.Helper()
	fix := bfs052Fixture{
		mountpoint:    mountpoint,
		mountinfoLine: bfs052MountLine(mountpoint, dev),
		connID:        strings.SplitN(dev, ":", 2)[1],
		connFiles:     map[string]string{"max_background": "50", "congestion_threshold": "37"},
		bdiEntry:      dev,
		readAheadKB:   readAheadKB,
	}
	fix.install(t)
}

// TestBFS052MutationClampBecomesGrantedIsCaught proves criterion 2's cell
// is not vacuous: if the probe lost the clamp comparison, the mount's
// over-limit request would be reported granted — the exact "tuned when it
// is not" lie the row exists for.
func TestBFS052MutationClampBecomesGrantedIsCaught(t *testing.T) {
	mp := "/mnt/bfs052-mutation"
	bfs052FixtureFor(t, mp, "0:920", "128") // kernel runs 128 KiB
	// A mutated readAheadCap that always granted would answer granted here;
	// the real one must NOT.
	c := readAheadCap(1<<20, 128<<10)
	if c.State != CapabilityStateDegraded {
		t.Fatalf("control broken: an over-limit request rendered %q — the comparison this row pins is dead", c.State)
	}
	if c.Reason == "" || c.Requested == "" || c.Effective == "" {
		t.Fatalf("control broken: the degradation lost a figure (entry %+v)", c)
	}
}

// TestBFS052MutationUnknownBecomesGrantedIsCaught proves criterion 3's cell
// is not vacuous: a probe that reported unknowns as granted fails here. The
// direct arm drives capUnknown's contract; the file-level arm drives the
// fixture with a missing sysfs file through the real probe.
func TestBFS052MutationUnknownBecomesGrantedIsCaught(t *testing.T) {
	// A granted entry cannot carry "not observable via" — the vocabulary
	// itself forbids it; if a mutation made capUnknown grant, the state and
	// the reason would contradict and every cell in the unknown table fails
	// on the State assert. The arm pins the pair:
	u := capUnknown(capSplice, "on", "not observable via /sys/fs/fuse/connections (x)")
	if u.State != CapabilityStateUnknown {
		t.Fatalf("control broken: the unknown builder produced %q", u.State)
	}
	// And the live probe path on a missing file:
	mp := "/mnt/bfs052-mutation-unknown"
	bfs052FixtureFor(t, mp, "0:921", "128")
	if err := os.Remove(filepath.Join(sysFSFuseConnections, "921", "congestion_threshold")); err != nil {
		t.Fatal(err)
	}
	st := probeFuseCapabilities(mp, bfs052Settings.flags(0), fuseRequestsFixture)
	c := capFor(t, st, capCongestion)
	if c.State != CapabilityStateUnknown {
		t.Fatalf("control broken: a missing connection file rendered %q, not unknown", c.State)
	}
}

// TestBFS052MutationStatusWiringIsCaught proves criterion 4's cell is not
// vacuous: with the probe's stored state REMOVED (as a wiring regression
// that dropped the probe call would leave it), the status document carries
// no capability entries — so the wiring cell's "at least one probed
// capability" assert cannot pass on an unpopulated mount.
func TestBFS052MutationStatusWiringIsCaught(t *testing.T) {
	m, _ := testMount(t, "bfs-052 wiring control\n")
	st := m.Status()
	fs, ok := st.Fuse.(FuseState)
	if !ok {
		t.Fatalf("control broken: Status().Fuse is %T on an unpopulated mount; the field must still type as FuseState so the document shape is stable", st.Fuse)
	}
	if len(fs.Capabilities) != 0 {
		t.Fatalf("control broken: an unprobed mount carries %d capability entries", len(fs.Capabilities))
	}
}

// TestBFS052LiveSourcesResolveOnThisHost runs wherever the tests run: it
// reads the REAL mountinfo and asks the probe's resolution logic about the
// host's own root mount. It is the control that proves the sources and the
// parser work on the real kernel's file shape (not only the fixtures) — the
// part of the live evidence that does not need a FUSE mount. The FUSE-mount
// arms (negotiated values against this kernel) are the live cell above,
// which skips honestly where the environment cannot mount.
func TestBFS052LiveSourcesResolveOnThisHost(t *testing.T) {
	raw, err := os.ReadFile(procMountinfo)
	if err != nil {
		t.Fatalf("this host has no readable %s: %v", procMountinfo, err)
	}
	if !strings.Contains(string(raw), " - ") {
		t.Fatalf("%s does not carry the separator shape the parser reads", procMountinfo)
	}
	// The root mount is on every Linux host; its fstype must survive the
	// parser with its options attached.
	line, ok := findMountLine("/")
	if !ok {
		t.Fatal("the parser could not resolve this host's own root mount from the real mountinfo")
	}
	if line.fstype == "" || line.options == "" {
		t.Fatalf("the parsed root line is missing fstype/options: %+v", line)
	}
	// And the negative: a path no mount carries resolves nothing, on the
	// real table, not just in fixtures.
	if _, ok := findMountLine("/bfs052-definitely-not-a-mount-9f31"); ok {
		t.Fatal("the parser resolved a mountpoint that is not mounted")
	}
}

// fuseRequestsFixture is the shared request view for the controls.
var fuseRequestsFixture = fuseRequests{maxBackground: 32, congestion: 24}
