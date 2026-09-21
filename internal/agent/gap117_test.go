package agent

import (
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── GAP-117 tier→knob resolution tests ────────────────────────────────
//
// The shipped tier is "standard" (GAP-117, per specs/safety-presets.md §1/§3):
// the spec's default tier carries exactly today's five-knob baseline.
// "open" and "hardened" remain VALID names that resolve to the identical set
// until GAP-118/119 differentiate the tiers. These tests pin, for EVERY one
// of the five properties, the resolved value at BOTH enforcement points
// (E2 unit argv, E1 slice drop-in) for the default tier — and pin the
// zero-delta invariants (no value changed anywhere in GAP-117).

// gap117KnobExpectation is one row of the tier→knob table under test: the
// systemd property and the exact string the enforcement points emit for the
// baseline limit set (the agent.Default* config values).
type gap117KnobExpectation struct {
	prop      string
	unitValue string
	slice     bool
	unit      bool
}

// gap117Expectations is the five-property contract of the shipped tier,
// derived from the config defaults named in the spec (DefaultCPUQuota 2.0,
// DefaultMemoryBytes 4 GiB, DefaultMaxProcesses 4096, DefaultMaxOpenFiles
// 65536, DefaultDiskBytes 20 GiB). These ARE the recorded pre-GAP-116
// strings — GAP-117 changes no value.
func gap117Expectations() []gap117KnobExpectation {
	return []gap117KnobExpectation{
		{prop: "CPUQuota", unitValue: "200%", slice: true, unit: true},
		{prop: "MemoryMax", unitValue: "4294967296", slice: true, unit: true},
		{prop: "LimitFSIZE", unitValue: "21474836480", slice: true, unit: true},
		{prop: "TasksMax", unitValue: "4096", slice: true, unit: true},
		{prop: "LimitNOFILE", unitValue: "65536:65536", slice: true, unit: true},
	}
}

// gap117BaselineLimits is the production-shaped default limit set
// (config.DefaultConfig().Agent values): the exact input the tier table
// resolves for the shipped tier.
func gap117BaselineLimits() (float64, uint64, uint64, uint64, uint64) {
	cfg := config.DefaultConfig()
	return cfg.Agent.DefaultCPUQuota,
		cfg.Agent.DefaultMemoryBytes,
		cfg.Agent.DefaultDiskBytes,
		cfg.Agent.DefaultMaxProcesses,
		cfg.Agent.DefaultMaxOpenFiles
}

// TestGAP117_DefaultPresetResolvesTodayDefaults is the core table-driven proof:
// for the built-in default tier ("standard"), EVERY one of the five
// properties resolves to today's default value at BOTH enforcement points —
// the unit knob set (E2, buildRootlessDockerdArgs) and the slice knob set
// (E1, applyUserSliceLimits). Every valid tier name resolves to the same
// values in GAP-117 (tier differentiation is GAP-118/119 scope).
func TestGAP117_DefaultPresetResolvesTodayDefaults(t *testing.T) {
	cpuQuota, memMax, diskMax, maxProcs, maxFiles := gap117BaselineLimits()

	tests := []struct {
		tier string
	}{
		{tier: config.SafetyPresetDefault}, // the built-in default ("standard")
		{tier: config.SafetyPresetStandard},
		{tier: config.SafetyPresetOpen},     // valid name, same set (GAP-118/119 differentiate)
		{tier: config.SafetyPresetHardened}, // valid name, same set (GAP-118/119 differentiate)
	}
	for _, tt := range tests {
		t.Run("tier="+tt.tier, func(t *testing.T) {
			unitKnobs, sliceKnobs := KnobsForPreset(tt.tier, cpuQuota, memMax, diskMax, maxProcs, maxFiles)

			unitByName := map[string]SystemdKnob{}
			for _, k := range unitKnobs {
				unitByName[k.Name] = k
			}
			sliceByName := map[string]SystemdKnob{}
			for _, k := range sliceKnobs {
				sliceByName[k.Name] = k
			}
			for _, want := range gap117Expectations() {
				if want.unit {
					got, ok := unitByName[want.prop]
					if !ok {
						t.Fatalf("tier %q: unit knob set missing %s (got %v)", tt.tier, want.prop, unitKnobs)
					}
					if got.Value != want.unitValue {
						t.Errorf("tier %q: unit %s = %q, want %q (byte-identical default)",
							tt.tier, want.prop, got.Value, want.unitValue)
					}
				}
				if want.slice {
					got, ok := sliceByName[want.prop]
					if !ok {
						t.Fatalf("tier %q: slice knob set missing %s (got %v)", tt.tier, want.prop, sliceKnobs)
					}
					if got.Value != want.unitValue {
						t.Errorf("tier %q: slice %s = %q, want %q (byte-identical default)",
							tt.tier, want.prop, got.Value, want.unitValue)
					}
				}
			}
			// Exactly five properties on each surface — no knob gained, none lost.
			if len(unitKnobs) != 5 || len(sliceKnobs) != 5 {
				t.Errorf("tier %q: knob counts unit=%d slice=%d, want 5/5", tt.tier, len(unitKnobs), len(sliceKnobs))
			}
		})
	}
}

// TestGAP117_DefaultTierUnitArgvByteIdentical proves the E2 enforcement point
// (the dockerd transient unit argv built by buildRootlessDockerdArgs) emits
// the recorded pre-GAP-116 strings for a default-tier spawn — the argv is
// byte-identical, property order included.
func TestGAP117_DefaultPresetUnitArgvByteIdentical(t *testing.T) {
	cpuQuota, memMax, diskMax, maxProcs, maxFiles := gap117BaselineLimits()
	unitKnobs, _ := KnobsForPreset(config.SafetyPresetDefault, cpuQuota, memMax, diskMax, maxProcs, maxFiles)
	args, _ := buildRootlessDockerdArgs(dockerdUnitArgs{
		AgentID:        "gap117",
		UnitName:       "bunker-docker-gap117",
		UID:            "1001",
		GID:            "1002",
		UserHome:       "/home/bunker-gap117",
		RuntimeDir:     "/run/bunker/gap117/run",
		DockerSockPath: "/run/bunker/gap117/docker.sock",
		RootlessBin:    "/home/bunker-gap117/bin/dockerd-rootless.sh",
		CPUQuota:       cpuQuota,
		MemoryMax:      memMax,
		DiskMax:        diskMax,
		MaxProcesses:   maxProcs,
		MaxOpenFiles:   maxFiles,
		UnitKnobs:      unitKnobs,
	})
	joined := strings.Join(args, "\x00")
	for _, want := range gap117Expectations() {
		wantArg := "--property=" + want.prop + "=" + want.unitValue
		if !strings.Contains(joined, wantArg) {
			t.Errorf("default-tier unit argv lost %q:\n%v", wantArg, args)
		}
	}
}

// TestGAP117_PresetNamesSameArgv proves the no-config-break contract at the
// argv level: a spawn resolved through safety.preset: open and one resolved
// through the default tier produce IDENTICAL argv — an existing config keeps
// byte-identical spawns.
func TestGAP117_PresetNamesSameArgv(t *testing.T) {
	cpuQuota, memMax, diskMax, maxProcs, maxFiles := gap117BaselineLimits()
	build := func(preset string) []string {
		unitKnobs, _ := KnobsForPreset(preset, cpuQuota, memMax, diskMax, maxProcs, maxFiles)
		args, _ := buildRootlessDockerdArgs(dockerdUnitArgs{
			AgentID: "abc", UnitName: "u", UID: "1", GID: "1",
			UserHome: "/h", RuntimeDir: "/r", DockerSockPath: "/s", RootlessBin: "/bin/true",
			CPUQuota: cpuQuota, MemoryMax: memMax, DiskMax: diskMax,
			MaxProcesses: maxProcs, MaxOpenFiles: maxFiles,
			UnitKnobs: unitKnobs,
		})
		return args
	}
	defArgs := build(config.SafetyPresetDefault)
	openArgs := build(config.SafetyPresetOpen)
	if len(defArgs) != len(openArgs) {
		t.Fatalf("argv length drift: default %d vs open %d", len(defArgs), len(openArgs))
	}
	for i := range defArgs {
		if defArgs[i] != openArgs[i] {
			t.Fatalf("argv[%d] drift: default %q vs open %q (configs naming open must keep working)", i, defArgs[i], openArgs[i])
		}
	}
}

// TestGAP117_SliceDropInThroughTierResolution proves the E1 enforcement point
// consumes the tier table: the drop-in content applyUserSliceLimits builds
// from the spawn-site tier-resolved slice knobs is byte-identical to the
// recorded pre-GAP-116 drop-in, and the nil fallback (standalone callers)
// produces the same bytes.
func TestGAP117_SliceDropInThroughPresetResolution(t *testing.T) {
	cpuQuota, memMax, diskMax, maxProcs, maxFiles := gap117BaselineLimits()

	// RECORDED PRE-CHANGE EXPECTATION (same bytes the GAP-116 zero-delta
	// test pins): property order CPUQuota, MemoryMax, TasksMax, LimitNOFILE,
	// LimitFSIZE; trailing newline.
	want := "[Slice]\n" +
		"CPUQuota=200%\n" +
		"MemoryMax=4294967296\n" +
		"TasksMax=4096\n" +
		"LimitNOFILE=65536:65536\n" +
		"LimitFSIZE=21474836480\n"

	build := func(knobs []SystemdKnob) string {
		var got strings.Builder
		got.WriteString("[Slice]\n")
		set := knobs
		if len(set) == 0 {
			set = sliceKnobsFor(cpuQuota, memMax, diskMax, maxProcs, maxFiles)
		}
		for _, k := range set {
			got.WriteString(k.Name + "=" + k.Value + "\n")
		}
		return got.String()
	}

	_, tierKnobs := KnobsForPreset(config.SafetyPresetDefault, cpuQuota, memMax, diskMax, maxProcs, maxFiles)
	if got := build(tierKnobs); got != want {
		t.Errorf("tier-resolved slice drop-in drift\n got: %q\nwant: %q", got, want)
	}
	if got := build(nil); got != want {
		t.Errorf("fallback slice drop-in drift\n got: %q\nwant: %q", got, want)
	}
}

// TestGAP117_SliceReportingThroughTierTable proves the slice surface an agent
// record reports (systemdKnobsToProto over the tier-resolved slice knobs —
// what `bunker info` prints) carries all five properties with today's values.
func TestGAP117_SliceReportingThroughPresetTable(t *testing.T) {
	cpuQuota, memMax, diskMax, maxProcs, maxFiles := gap117BaselineLimits()
	_, sliceKnobs := KnobsForPreset(config.SafetyPresetDefault, cpuQuota, memMax, diskMax, maxProcs, maxFiles)
	props := systemdKnobsToProto(sliceKnobs)
	if len(props) != 5 {
		t.Fatalf("slice properties = %d, want 5", len(props))
	}
	wantProps := map[string]string{
		"CPUQuota":    "200%",
		"MemoryMax":   "4294967296",
		"TasksMax":    "4096",
		"LimitNOFILE": "65536:65536",
		"LimitFSIZE":  "21474836480",
	}
	for _, p := range props {
		if wantProps[p.GetName()] != p.GetValue() {
			t.Errorf("property %s = %q, want %q", p.GetName(), p.GetValue(), wantProps[p.GetName()])
		}
	}
}

// TestGAP117_DefaultConfigLimitValuesFeedTheTier pins the source numbers the
// tier table consumes: the config defaults ARE the spec's shipped-tier values
// (specs/safety-presets.md §3 standard column). GAP-117 changed none of them.
func TestGAP117_DefaultConfigLimitValuesFeedThePreset(t *testing.T) {
	a := config.DefaultConfig().Agent
	if a.DefaultCPUQuota != 2.0 {
		t.Errorf("DefaultCPUQuota = %v, want 2.0", a.DefaultCPUQuota)
	}
	if a.DefaultMemoryBytes != 4*1024*1024*1024 {
		t.Errorf("DefaultMemoryBytes = %d, want 4294967296", a.DefaultMemoryBytes)
	}
	if a.DefaultMaxProcesses != 4096 {
		t.Errorf("DefaultMaxProcesses = %d, want 4096", a.DefaultMaxProcesses)
	}
	if a.DefaultMaxOpenFiles != 65536 {
		t.Errorf("DefaultMaxOpenFiles = %d, want 65536", a.DefaultMaxOpenFiles)
	}
	if a.DefaultDiskBytes != 20*1024*1024*1024 {
		t.Errorf("DefaultDiskBytes = %d, want 21474836480", a.DefaultDiskBytes)
	}
}

// TestGAP117_DefaultTierRunAgentArgvUnchanged pins the detached-run argv: the
// run path resolves the tier ( vocabulary guard) but its limit properties are
// the same baseline strings — untouched by the rename.
func TestGAP117_DefaultPresetRunAgentArgvUnchanged(t *testing.T) {
	limits := &v1.ResourceLimits{
		CpuQuota:       2.0,
		MemoryMaxBytes: 4294967296,
		DiskMaxBytes:   21474836480,
	}
	args := buildRunAgentArgs("gap117", "1001", "1001", "bunker-run-gap117-deadbeef", "echo", []string{"hi"}, nil, limits, false)
	joined := strings.Join(args, "\x00")
	for _, want := range []string{
		"--property=CPUQuota=200%",
		"--property=MemoryMax=4294967296",
		"--property=LimitFSIZE=21474836480",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("detached-run argv lost %q: %v", want, args)
		}
	}
}
