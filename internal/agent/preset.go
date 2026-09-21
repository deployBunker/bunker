package agent

import (
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// GAP-116 safety-preset plumbing helpers shared by the spawn and run paths.

// systemdKnobsToProto converts the knob table into the wire representation
// carried on AgentSummary.systemd_properties. The unit properties are the
// canonical reported set (the slice carries the same five knobs in ITS order;
// both are stamped on the record).
func systemdKnobsToProto(knobs []SystemdKnob) []*v1.SystemdProperty {
	if len(knobs) == 0 {
		return nil
	}
	out := make([]*v1.SystemdProperty, 0, len(knobs))
	for _, k := range knobs {
		out = append(out, &v1.SystemdProperty{Name: k.Name, Value: k.Value})
	}
	return out
}

// SliceDropInStates are the honest states of the per-agent slice drop-in.
const (
	// SliceDropInWritten — the drop-in was written with exactly this content.
	SliceDropInWritten = "written"
	// SliceDropInFailed — the drop-in could not be written (best-effort
	// step); the agent keeps dockerd-unit limits only. Reported, never
	// silently omitted.
	SliceDropInFailed = "failed"
)

// sliceDropInState maps the spawn outcome to the reported state.
func sliceDropInState(written bool) string {
	if written {
		return SliceDropInWritten
	}
	return SliceDropInFailed
}
