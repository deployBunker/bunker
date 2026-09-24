// disk_cap_semantic_test.go — DF-BUNKER-54 semantic pin, agent side.
//
// The board row's criterion is "a test PINS the intended semantic so it cannot
// silently stay 'per-file'". These tests pin the KNOB: disk_max_bytes is
// emitted as systemd LimitFSIZE (RLIMIT_FSIZE — the maximum size of a SINGLE
// FILE) on both the unit and the slice surface, and it is emitted for NO other
// knob name. Nothing in this package applies a total-usage mechanism, which is
// exactly why every operator-facing label must call the number a per-file cap
// (see internal/cli/disk_semantic_test.go) and why real total-disk enforcement
// is a separate, open row (GAP-161).
package agent

import (
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// p54DiskMax is the shape of the reported defect: a "20 GB disk limit".
const p54DiskMax = uint64(20 * 1024 * 1024 * 1024)

// TestKnobsForPreset_DiskMaxIsPerFileLimitFSIZE pins the mechanism on BOTH
// knob surfaces: the disk cap becomes LimitFSIZE (a per-file cap), on the unit
// AND on the slice, and never some total-usage knob.
func TestKnobsForPreset_DiskMaxIsPerFileLimitFSIZE(t *testing.T) {
	unit, slice := KnobsForPreset("standard", 2.0, 4*1024*1024*1024, p54DiskMax, 4096, 65536)

	for _, surface := range []struct {
		name  string
		knobs []SystemdKnob
		scope string
	}{
		{"unit", unit, KnobScopeUnit},
		{"slice", slice, KnobScopeSlice},
	} {
		t.Run(surface.name, func(t *testing.T) {
			var diskKnobs []SystemdKnob
			for _, k := range surface.knobs {
				if k.Name == "LimitFSIZE" {
					diskKnobs = append(diskKnobs, k)
				}
			}
			if len(diskKnobs) != 1 {
				t.Fatalf("%s surface emits %d LimitFSIZE knobs, want exactly 1: %+v",
					surface.name, len(diskKnobs), surface.knobs)
			}
			got := diskKnobs[0]
			if got.Value != "21474836480" {
				t.Errorf("%s LimitFSIZE value = %q, want %q (the disk_max_bytes value verbatim)",
					surface.name, got.Value, "21474836480")
			}
			if got.Scope != surface.scope {
				t.Errorf("%s LimitFSIZE scope = %q, want %q", surface.name, got.Scope, surface.scope)
			}

			// The pin: LimitFSIZE is RLIMIT_FSIZE, a PER-FILE cap. No knob in
			// this table can cap total disk usage — a total-usage knob would
			// have to be named something else (and does not exist yet; real
			// enforcement is GAP-161). If a future change makes the disk cap
			// emit anything other than LimitFSIZE, this fails.
			for _, k := range surface.knobs {
				switch k.Name {
				case "LimitFSIZE", "CPUQuota", "MemoryMax", "TasksMax", "LimitNOFILE":
					// the five-knob baseline
				default:
					t.Errorf("%s surface emits an unexpected knob %q=%q; the disk cap is applied as "+
						"LimitFSIZE (RLIMIT_FSIZE, per-file) and total-disk enforcement is GAP-161 (DF-BUNKER-54)",
						surface.name, k.Name, k.Value)
				}
			}
		})
	}
}

// TestUnitKnobsFor_DiskCapHasNoTotalUsageKnob pins the honest half of the
// semantic in code: with a disk cap configured there is NO knob that bounds
// aggregate usage, only the per-file LimitFSIZE. That absence is the fact the
// labels must not contradict.
func TestUnitKnobsFor_DiskCapHasNoTotalUsageKnob(t *testing.T) {
	knobs := unitKnobsFor(2.0, 4*1024*1024*1024, p54DiskMax, 4096, 65536)

	names := make([]string, 0, len(knobs))
	for _, k := range knobs {
		names = append(names, k.Name)
	}
	joined := strings.Join(names, ",")

	if !strings.Contains(joined, "LimitFSIZE") {
		t.Fatalf("a configured disk cap must emit LimitFSIZE (the per-file mechanism); got %v", names)
	}
	// No quota/aggregate-usage knob exists in this table. Asserted by name so
	// the day someone adds real enforcement (GAP-161) the intended shape is
	// already declared here.
	for _, quotaKnob := range []string{"DiskQuota", "LimitDISK", "IOReadBandwidthMax", "StorageMax"} {
		if strings.Contains(joined, quotaKnob) {
			t.Errorf("disk cap emitted %q — total-disk enforcement is GAP-161 and, when it lands, "+
				"belongs in its own row rather than being smuggled in beside the per-file cap", quotaKnob)
		}
	}
}

// TestBuildRunAgentArgs_DiskCapIsPerFileLimitFSIZE pins the detached-run path
// (internal/agent/run.go), the second place the cap reaches the host.
func TestBuildRunAgentArgs_DiskCapIsPerFileLimitFSIZE(t *testing.T) {
	args := buildRunAgentArgs("p54", "1001", "1002", "p54-unit", "true", nil, nil,
		&v1.ResourceLimits{DiskMaxBytes: p54DiskMax}, false)
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--property=LimitFSIZE=21474836480") {
		t.Errorf("run-agent args missing the per-file cap --property=LimitFSIZE=21474836480: %v", args)
	}
	// The CLI label for this surface must not claim a disk limit either; the
	// argv is the mechanism, and LimitFSIZE is its honest name.
	if strings.Contains(joined, "--property=DiskQuota") || strings.Contains(joined, "--property=StorageMax") {
		t.Errorf("run-agent args carry a total-disk property that no row implements (GAP-161): %v", args)
	}
}

// TestDiskCapZeroEmitsNoKnob pins the documented-good configuration: 0 means
// the knob is absent entirely (internal/agent/SKILL.md tells operators to set
// default_disk_bytes: 0 because a finite RLIMIT_FSIZE crash-loops .NET apps).
// An absent knob is why the reporting surfaces must render an unset cap as an
// explicit absence instead of a 0-byte cap.
func TestDiskCapZeroEmitsNoKnob(t *testing.T) {
	unit, slice := KnobsForPreset("standard", 2.0, 4*1024*1024*1024, 0, 4096, 65536)
	for _, surface := range []struct {
		name  string
		knobs []SystemdKnob
	}{{"unit", unit}, {"slice", slice}} {
		for _, k := range surface.knobs {
			if k.Name == "LimitFSIZE" {
				t.Errorf("%s surface emitted LimitFSIZE=%q for diskMax=0; the knob must be absent "+
					"(that is the documented-good .NET-safe configuration)", surface.name, k.Value)
			}
		}
	}
}
