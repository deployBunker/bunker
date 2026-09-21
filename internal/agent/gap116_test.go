package agent

import (
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── GAP-116 zero-delta proofs ─────────────────────────────────────────
//
// The load-bearing criterion: with NO flag/env/config set (the built-in
// default preset — "standard" since GAP-117), the systemd-run argv and the
// slice drop-in written for a spawned agent must be BYTE-IDENTICAL to
// pre-GAP-116. The expected strings below were recorded from the PRE-CHANGE
// behavior (the exact fmt.Sprintf conditionals in buildRootlessDockerdArgs
// and applyUserSliceLimits at commit 708a875) BEFORE the table-driven
// refactor, and are asserted verbatim.

// gap116BaselineLimits is the production-shaped default limit set the
// zero-delta expectations were recorded with (config defaults: 2.0 CPU
// quota, 4 GiB memory, 20 GiB disk, 4096 processes, 65536 files).
func gap116BaselineLimits() (float64, uint64, uint64, uint64, uint64) {
	return 2.0, 4 * 1024 * 1024 * 1024, 20 * 1024 * 1024 * 1024, 4096, 65536
}

// TestGAP116_ZeroDelta_UnitArgv asserts the default-preset dockerd unit argv
// is byte-identical to the recorded pre-change expectation.
func TestGAP116_ZeroDelta_UnitArgv(t *testing.T) {
	cpuQuota, memMax, diskMax, maxProcs, maxFiles := gap116BaselineLimits()

	unitKnobs, _ := KnobsForPreset(config.SafetyPresetDefault, cpuQuota, memMax, diskMax, maxProcs, maxFiles)
	args, _ := buildRootlessDockerdArgs(dockerdUnitArgs{
		AgentID:        "abc123",
		UnitName:       "bunker-docker-abc123",
		UID:            "1001",
		GID:            "1002",
		UserHome:       "/home/bunker-abc123",
		RuntimeDir:     "/run/bunker/abc123/run",
		DockerSockPath: "/run/bunker/abc123/docker.sock",
		RootlessBin:    "/home/bunker-abc123/bin/dockerd-rootless.sh",
		CPUQuota:       cpuQuota,
		MemoryMax:      memMax,
		DiskMax:        diskMax,
		MaxProcesses:   maxProcs,
		MaxOpenFiles:   maxFiles,
		UnitKnobs:      unitKnobs,
	})

	// RECORDED PRE-CHANGE EXPECTATION (verbatim systemd-run argv prefix):
	// the property block order and formatting are exactly what the
	// pre-GAP-116 conditionals emitted for the baseline limits.
	wantArgv := []string{
		"--system",
		"--unit=bunker-docker-abc123",
		"--uid=1001",
		"--gid=1002",
		"--property=PAMName=login",
		"--property=PrivateTmp=yes",
		"--property=CPUQuota=200%",
		"--property=MemoryMax=4294967296",
		"--property=LimitFSIZE=21474836480",
		"--property=TasksMax=4096",
		"--property=LimitNOFILE=65536:65536",
		"--setenv=PATH=/home/bunker-abc123/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"--setenv=HOME=/home/bunker-abc123",
		"--setenv=USER=bunker-abc123",
		"--setenv=XDG_RUNTIME_DIR=/run/bunker/abc123/run",
		"--setenv=DOCKERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns",
		"--setenv=DOCKERD_ROOTLESS_ROOTLESSKIT_PORT_DRIVER=builtin",
		"--setenv=DOCKERD_ROOTLESS_ROOTLESSKIT_DETACH_NETNS=false",
		"--setenv=DOCKER_HOST=unix:///run/bunker/abc123/docker.sock",
		"--setenv=TMPDIR=/tmp",
		"/home/bunker-abc123/bin/dockerd-rootless.sh",
		"--host=unix:///run/bunker/abc123/docker.sock",
	}
	if len(args) != len(wantArgv) {
		t.Fatalf("argv length = %d, want %d (byte-identical argv)\ngot:  %v\nwant: %v", len(args), len(wantArgv), args, wantArgv)
	}
	for i := range wantArgv {
		if args[i] != wantArgv[i] {
			t.Fatalf("argv[%d] = %q, want %q\nfull got:  %v\nfull want: %v", i, args[i], wantArgv[i], args, wantArgv)
		}
	}
}

// TestGAP116_ZeroDelta_SliceDropIn asserts the default-preset slice drop-in
// content is byte-identical to the recorded pre-change expectation.
func TestGAP116_ZeroDelta_SliceDropIn(t *testing.T) {
	cpuQuota, memMax, diskMax, maxProcs, maxFiles := gap116BaselineLimits()

	// RECORDED PRE-CHANGE EXPECTATION: the exact drop-in the pre-GAP-116
	// applyUserSliceLimits conditionals wrote for the baseline limits
	// (property order: CPUQuota, MemoryMax, TasksMax, LimitNOFILE,
	// LimitFSIZE; trailing newline).
	want := "[Slice]\n" +
		"CPUQuota=200%\n" +
		"MemoryMax=4294967296\n" +
		"TasksMax=4096\n" +
		"LimitNOFILE=65536:65536\n" +
		"LimitFSIZE=21474836480\n"

	var got strings.Builder
	got.WriteString("[Slice]\n")
	for _, k := range sliceKnobsFor(cpuQuota, memMax, diskMax, maxProcs, maxFiles) {
		got.WriteString(k.Name + "=" + k.Value + "\n")
	}
	if got.String() != want {
		t.Fatalf("slice drop-in drift\n got: %q\nwant: %q", got.String(), want)
	}
}

// TestGAP116_ZeroDelta_DefaultPresetMatchesAllNames pins the tier-equivalence
// contract: every vocabulary member resolves to the SAME unit and slice knob
// sets (the shipped tier "standard" since GAP-117; open/hardened stay
// identical until GAP-118/119 differentiate them).
func TestGAP116_ZeroDelta_DefaultPresetMatchesAllNames(t *testing.T) {
	cpuQuota, memMax, diskMax, maxProcs, maxFiles := gap116BaselineLimits()
	openUnit, openSlice := KnobsForPreset(config.SafetyPresetOpen, cpuQuota, memMax, diskMax, maxProcs, maxFiles)
	for _, preset := range []string{config.SafetyPresetStandard, config.SafetyPresetHardened, config.SafetyPresetDefault} {
		unit, slice := KnobsForPreset(preset, cpuQuota, memMax, diskMax, maxProcs, maxFiles)
		if len(unit) != len(openUnit) || len(slice) != len(openSlice) {
			t.Fatalf("preset %q knob-count drift: unit %d/%d slice %d/%d", preset, len(unit), len(openUnit), len(slice), len(openSlice))
		}
		for i := range openUnit {
			if unit[i] != openUnit[i] {
				t.Fatalf("preset %q unit knob %d = %+v, want %+v", preset, i, unit[i], openUnit[i])
			}
		}
		for i := range openSlice {
			if slice[i] != openSlice[i] {
				t.Fatalf("preset %q slice knob %d = %+v, want %+v", preset, i, slice[i], openSlice[i])
			}
		}
	}
}

// TestGAP116_ZeroDelta_RunAgentArgv pins the detached-run argv unchanged for
// every valid preset: buildRunAgentArgs takes no preset parameter in this
// row, so its output cannot drift (the limits it receives are the same).
func TestGAP116_ZeroDelta_RunAgentArgv(t *testing.T) {
	limits := &v1.ResourceLimits{
		CpuQuota:       2.0,
		MemoryMaxBytes: 4294967296,
		DiskMaxBytes:   21474836480,
	}
	args := buildRunAgentArgs("abc123", "1001", "1001", "bunker-run-abc123-deadbeef", "make", []string{"test"}, nil, limits, false)
	joined := strings.Join(args, "\x00")
	for _, want := range []string{
		"--property=CPUQuota=200%",
		"--property=MemoryMax=4294967296",
		"--property=LimitFSIZE=21474836480",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("detached run argv lost %q: %v", want, args)
		}
	}
}

// TestGAP116_ZeroDelta_KnobsNoLimitsOmitsProperties keeps the no-limits shape
// honest through the table: zero limits produce NO property elements.
func TestGAP116_ZeroDelta_KnobsNoLimitsOmitsProperties(t *testing.T) {
	unit, slice := KnobsForPreset(config.SafetyPresetDefault, 0, 0, 0, 0, 0)
	if len(unit) != 0 || len(slice) != 0 {
		t.Fatalf("zero limits must produce empty knob sets, got unit=%v slice=%v", unit, slice)
	}
	args, _ := buildRootlessDockerdArgs(dockerdUnitArgs{
		AgentID: "abc123", UnitName: "u", UID: "1", GID: "1",
		UserHome: "/h", RuntimeDir: "/r", DockerSockPath: "/s", RootlessBin: "/bin/true",
	})
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"CPUQuota", "MemoryMax", "LimitFSIZE", "TasksMax", "LimitNOFILE"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("unconfigured %s property was emitted: %v", forbidden, args)
		}
	}
}

// TestGAP116_KnobsForPreset_UnknownPanics pins the fail-loud contract: the
// knob table never silently answers for a preset outside the vocabulary.
func TestGAP116_KnobsForPreset_UnknownPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("KnobsForPreset accepted an unknown preset")
		}
	}()
	KnobsForPreset("ultra", 2.0, 1, 1, 1, 1)
}

// ── GAP-116 effective-set reporting ───────────────────────────────────

// TestGAP116_EffectiveSetReporting proves the spawn-record stamp carries the
// preset name and both knob surfaces, and that ToAgentSummary ships them on
// the existing GetAgent plumbing (AC4).
func TestGAP116_EffectiveSetReporting(t *testing.T) {
	cpuQuota, memMax, diskMax, maxProcs, maxFiles := gap116BaselineLimits()
	unitKnobs, sliceKnobs := KnobsForPreset(config.SafetyPresetDefault, cpuQuota, memMax, diskMax, maxProcs, maxFiles)

	rec := &resource.AgentRecord{}
	rec.SafetyPreset = config.SafetyPresetDefault
	rec.UnitProperties = systemdKnobsToProto(unitKnobs)
	rec.SliceProperties = systemdKnobsToProto(sliceKnobs)
	rec.SliceDropInState = sliceDropInState(true)

	summary := rec.ToAgentSummary()
	if summary.GetSafetyPreset() != config.SafetyPresetDefault {
		t.Errorf("summary preset = %q, want %q", summary.GetSafetyPreset(), config.SafetyPresetDefault)
	}
	props := summary.GetSystemdProperties()
	if len(props) != 5 {
		t.Fatalf("summary properties = %d, want 5", len(props))
	}
	wantProps := map[string]string{
		"CPUQuota":    "200%",
		"MemoryMax":   "4294967296",
		"LimitFSIZE":  "21474836480",
		"TasksMax":    "4096",
		"LimitNOFILE": "65536:65536",
	}
	for _, p := range props {
		if wantProps[p.GetName()] != p.GetValue() {
			t.Errorf("property %s = %q, want %q", p.GetName(), p.GetValue(), wantProps[p.GetName()])
		}
	}
}

// TestGAP116_EffectiveSetReporting_PreGapRecordIsEmpty keeps the additive
// contract honest: a pre-GAP-116 record (no preset stamped) reports an empty
// preset and no properties — the CLI renders the built-in default name.
func TestGAP116_EffectiveSetReporting_PreGapRecordIsEmpty(t *testing.T) {
	rec := &resource.AgentRecord{}
	summary := rec.ToAgentSummary()
	if summary.GetSafetyPreset() != "" || len(summary.GetSystemdProperties()) != 0 {
		t.Fatalf("pre-GAP-116 record must report empty preset/properties, got %q/%v",
			summary.GetSafetyPreset(), summary.GetSystemdProperties())
	}
}
