package agent

// ISO-001 pin: the PRIVATE-PROCFS wiring IS the agent units' /proc boundary.
// Each test pins, at the argv level, that the transient unit carries
// exactly one ProtectProc=invisible — systemd's per-unit hidepid=2 —
// positioned as a boundary property right after PrivateTmp. That property
// is the ONLY /proc mechanism these units get: there is no host /proc
// remount in the spawn path (the real-/proc gate is the mount-options gate,
// not a remount), so the argv pin is the structural proof of the
// per-unit /proc boundary the mode reports.

import (
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/netmode"
)

// countArgStr / containsArgStr mirror the gap075 helpers without the
// package-level coupling (this file pins by text, not by call site).
func countArgStr(argv []string, want string) int {
	n := 0
	for _, a := range argv {
		if a == want {
			n++
		}
	}
	return n
}

func containsArgStr(argv []string, want string) bool {
	return countArgStr(argv, want) > 0
}

// TestProcVisDockerdUnitCarriesProtectProcInvisible pins the SPAWN unit's
// private-procfs wiring: the mode property rides the dockerd unit argv
// exactly once, immediately after the PrivateTmp boundary — the same shape
// the detached-run pin (procvis_test.go) holds for run units. Two argvs
// pinned = both unit surfaces carry the boundary.
func TestProcVisDockerdUnitCarriesProtectProcInvisible(t *testing.T) {
	shared, _ := buildRootlessDockerdArgs(netmodeTestUnitArgs(netmode.ModeShared))
	proc, _ := buildRootlessDockerdArgs(netmodeTestUnitArgs(netmode.ModeProcVis))

	if n := countArgStr(proc, "--property="+netmode.PropertyProtectProcInvisible); n != 1 {
		t.Fatalf("ProtectProc=invisible appears %d times in the dockerd unit argv, want exactly 1: %v", n, proc)
	}
	// The PrivateTmp boundary is unchanged and the mode property sits
	// IMMEDIATELY after it (same position contract the detached-run unit
	// renders).
	idx := -1
	for i, a := range proc {
		if a == "--property=PrivateTmp=yes" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("PrivateTmp=yes missing from the argv — fixture drift")
	}
	if proc[idx+1] != "--property="+netmode.PropertyProtectProcInvisible {
		t.Fatalf("ProtectProc=invisible is not immediately after PrivateTmp (PrivateTmp at %d): %v", idx, proc)
	}
	// Nothing else about the argv changed versus the shared spawn
	// (one property, one position, byte-delta exactly +1).
	if len(proc) != len(shared)+1 {
		t.Fatalf("procvis argv length %d, want shared %d + 1\nprocvis: %v\nshared:  %v", len(proc), len(shared), proc, shared)
	}

	// The PURE surface must agree with the verified one (procVisUnitProperties
	// delegates to the same producer — one producer, two wrappers).
	if got := procVisUnitProperties(); len(got) != 1 || got[0] != netmode.ProcVisUnitProperties()[0] {
		t.Fatalf("procVisUnitProperties() = %v, want %v", got, netmode.ProcVisUnitProperties())
	}
	// The boundary string HONESTLY reports the /proc boundary and its
	// exclusions (the reporting law: a bound that is not reported is not
	// a bound).
	b := netmode.BoundaryFor(netmode.ModeProcVis)
	for _, want := range []string{"private /proc", "hidepid=2"} {
		if !strings.Contains(b, want) {
			t.Errorf("procvis boundary %q missing %q", b, want)
		}
	}
}

// TestProcVisUnitPropertiesExactlyOneElement is the anti-drift pin: the
// verified property set is EXACTLY the single ProtectProc property — an
// extra element here would change the argv of every procvis spawn under a
// "unit properties" change no test pinned.
func TestProcVisUnitPropertiesExactlyOneElement(t *testing.T) {
	props := netmode.ProcVisUnitProperties()
	if len(props) != 1 {
		t.Fatalf("ProcVisUnitProperties() has %d elements, want exactly 1: %v", len(props), props)
	}
	if props[0] != "--property="+netmode.PropertyProtectProcInvisible {
		t.Fatalf("the element = %q, want --property=%q", props[0], netmode.PropertyProtectProcInvisible)
	}
	if containsArgStr(props, "--property=PrivateNetwork=yes") {
		t.Fatal("the procvis property set leaked the systemd-mode property")
	}
}
