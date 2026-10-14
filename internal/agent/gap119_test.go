package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/netmode"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── GAP-119 unit-sandboxing knobs ─────────────────────────────────────
//
// The load-bearing properties of the feature:
//
//  1. OFF (the default) adds NOTHING — the systemd-run argv is byte-identical
//     to a daemon without the feature (the zero-delta law every preset row in
//     this family obeys);
//  2. ON adds exactly the classified SAFE properties, each once, in table
//     order, inside the same boundary block as PrivateTmp;
//  3. the properties the probe measured as BREAKERS of the rootless start path
//     can never appear — the test drives that off the same classification table
//     the builder reads, so a row cannot be flipped to InSet without the
//     verdict and the risk line changing with it.

// gap119UnitArgs builds the spawn unit argv for the baseline limits, in the
// given network mode, with the GAP-119 flag as passed.
func gap119UnitArgs(hardened bool, networkMode string) ([]string, []string) {
	cpuQuota, memMax, diskMax, maxProcs, maxFiles := gap116BaselineLimits()
	unitKnobs, _ := KnobsForPreset(config.SafetyPresetDefault, cpuQuota, memMax, diskMax, maxProcs, maxFiles)
	return buildRootlessDockerdArgs(dockerdUnitArgs{
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
		// GAP-119
		HardenedUnitProperties: hardened,
		NetworkMode:            networkMode,
	})
}

// gap119PropertyArgs returns the argv elements that are systemd properties.
func gap119PropertyArgs(args []string) []string {
	var out []string
	for _, a := range args {
		if strings.HasPrefix(a, "--property=") {
			out = append(out, strings.TrimPrefix(a, "--property="))
		}
	}
	return out
}

// countGap119Arg counts exact argv element occurrences.
func countGap119Arg(args []string, want string) int {
	n := 0
	for _, a := range args {
		if a == want {
			n++
		}
	}
	return n
}

// TestGAP119_FlagOffAddsNoProperties is the zero-delta half: with the flag off
// the property block is exactly the recorded pre-change block — no hardening
// property is present in any network mode, with or without limits.
func TestGAP119_FlagOffAddsNoProperties(t *testing.T) {
	// RECORDED PRE-CHANGE PROPERTY BLOCK (from gap116_test.go's verbatim
	// expectation at commit 708a875, before the table refactor): the property
	// elements in argv order for the baseline limits in the default mode.
	wantBaseline := []string{
		"PAMName=login",
		"PrivateTmp=yes",
		"CPUQuota=200%",
		"MemoryMax=4294967296",
		"LimitFSIZE=21474836480",
		"TasksMax=4096",
		"LimitNOFILE=65536:65536",
	}

	for _, tc := range []struct {
		name string
		mode string
	}{
		{"default-mode", ""},
		{"shared", netmode.ModeShared},
		{"systemd", netmode.ModeSystemd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := gap119UnitArgs(false, tc.mode)
			got := gap119PropertyArgs(args)

			if tc.mode == "" || tc.mode == config.NetworkModeShared {
				if len(got) != len(wantBaseline) {
					t.Fatalf("flag off changed the property block:\n got: %v\nwant: %v", got, wantBaseline)
				}
				for i := range wantBaseline {
					if got[i] != wantBaseline[i] {
						t.Fatalf("flag off changed property %d: got %q, want %q\nfull: %v", i, got[i], wantBaseline[i], got)
					}
				}
			}
			for _, p := range unitHardeningTable() {
				for _, a := range args {
					if strings.HasPrefix(a, "--property="+p.Name) {
						t.Errorf("flag off emitted hardening property %q: %v", a, args)
					}
				}
			}
		})
	}
}

// TestGAP119_UnitArgvUnchangedByFieldDefault proves the field is a pure
// addition: a struct without it (the zero value) and a struct with it set to
// false produce the same bytes, so no existing caller changes behavior.
func TestGAP119_UnitArgvUnchangedByFieldDefault(t *testing.T) {
	off, _ := gap119UnitArgs(false, "")
	cpuQuota, memMax, diskMax, maxProcs, maxFiles := gap116BaselineLimits()
	unitKnobs, _ := KnobsForPreset(config.SafetyPresetDefault, cpuQuota, memMax, diskMax, maxProcs, maxFiles)
	zeroVal, _ := buildRootlessDockerdArgs(dockerdUnitArgs{
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
		// HardenedUnitProperties deliberately unset (the zero value).
	})
	if len(off) != len(zeroVal) {
		t.Fatalf("argv length drift: off=%d zero-value=%d", len(off), len(zeroVal))
	}
	for i := range off {
		if off[i] != zeroVal[i] {
			t.Fatalf("argv[%d] drift: off=%q zero-value=%q", i, off[i], zeroVal[i])
		}
	}
}

// TestGAP119_FlagOnEmitsMeasuredSet is the positive half, table-driven off the
// classification table: every InSet property appears exactly once, directly
// after the PrivateTmp boundary, in table order.
func TestGAP119_FlagOnEmitsMeasuredSet(t *testing.T) {
	args, _ := gap119UnitArgs(true, "")
	wantKnobs := hardenedUnitKnobs(true)
	if len(wantKnobs) == 0 {
		t.Fatal("hardenedUnitKnobs(true) is empty — the flag arms nothing")
	}

	for _, k := range wantKnobs {
		want := "--property=" + k.Name + "=" + k.Value
		if n := countGap119Arg(args, want); n != 1 {
			t.Errorf("%q appears %d times, want exactly 1\nargv: %v", want, n, args)
		}
	}

	// Position: the block sits between PrivateTmp and the first limit property
	// (the sandbox boundary block), not appended after the limits.
	privateTmp := -1
	firstHardening := -1
	firstLimit := -1
	for i, a := range args {
		switch {
		case a == "--property=PrivateTmp=yes":
			privateTmp = i
		case strings.HasPrefix(a, "--property="+wantKnobs[0].Name+"="):
			if firstHardening < 0 {
				firstHardening = i
			}
		case a == "--property=CPUQuota=200%":
			firstLimit = i
		}
	}
	if privateTmp < 0 || firstHardening < 0 || firstLimit < 0 {
		t.Fatalf("missing anchors: privateTmp=%d firstHardening=%d firstLimit=%d\nargv: %v", privateTmp, firstHardening, firstLimit, args)
	}
	if privateTmp >= firstHardening || firstHardening >= firstLimit {
		t.Errorf("hardening block must sit after PrivateTmp and before the limit block: privateTmp=%d firstHardening=%d firstLimit=%d\nargv: %v",
			privateTmp, firstHardening, firstLimit, args)
	}

	// Exactly the set: the property block is the pre-change block plus the
	// hardening properties (nothing else moved).
	offArgs, _ := gap119UnitArgs(false, "")
	if len(args) != len(offArgs)+len(wantKnobs) {
		t.Errorf("argv grew by %d elements, want %d (one per hardening property)", len(args)-len(offArgs), len(wantKnobs))
	}

	// The rest of the unit contract is untouched by the sandbox block.
	for _, want := range []string{"--property=PAMName=login", "--setenv=DOCKER_HOST=unix:///run/bunker/abc123/docker.sock", "/home/bunker-abc123/bin/dockerd-rootless.sh"} {
		if countGap119Arg(args, want) != 1 {
			t.Errorf("unit argv lost or duplicated %q: %v", want, args)
		}
	}
}

// TestGAP119_ExcludedPropertiesNeverEmitted is the negative half, driven off
// the same table: every candidate the probe measured as a BREAKER (or as
// needing an exception) is absent from the argv with the flag OFF and ON, in
// every network mode. This is the "a property known to break dockerd-start is
// NOT in the set" acceptance criterion, generalized: it fails if anyone flips a
// row to InSet without re-classifying it.
func TestGAP119_ExcludedPropertiesNeverEmitted(t *testing.T) {
	var excluded []unitHardeningProperty
	for _, p := range unitHardeningTable() {
		if !p.InSet {
			excluded = append(excluded, p)
		}
	}
	if len(excluded) == 0 {
		t.Fatal("no excluded property rows — the exclusion list must be non-empty")
	}
	// The four properties the probe measured to break the rootless start path
	// must be in that list by name, so a future edit that removes one fails
	// here as well as in the table-integrity test.
	for _, must := range []string{"NoNewPrivileges", "CapabilityBoundingSet", "RestrictNamespaces", "ProtectSystem", "ProtectHome"} {
		found := false
		for _, p := range excluded {
			if p.Name == must {
				found = true
			}
		}
		if !found {
			t.Errorf("measured BREAKER %q is not in the exclusion table", must)
		}
	}

	for _, hardened := range []bool{false, true} {
		for _, mode := range []string{"", netmode.ModeShared, netmode.ModeSystemd} {
			args, _ := gap119UnitArgs(hardened, mode)
			for _, p := range excluded {
				for _, a := range args {
					if strings.HasPrefix(a, "--property="+p.Name+"=") || a == "--property="+p.Name {
						t.Errorf("flag=%v mode=%q emitted excluded property %q (verdict %s): %v",
							hardened, mode, a, p.Verdict, args)
					}
				}
			}
		}
	}
}

// TestGAP119_PrivateTmpBoundaryNotDuplicated keeps the GAP-075 boundary honest:
// the hardening set does not re-emit the property that is already unconditional.
func TestGAP119_PrivateTmpBoundaryNotDuplicated(t *testing.T) {
	for _, hardened := range []bool{false, true} {
		args, _ := gap119UnitArgs(hardened, "")
		if n := countGap119Arg(args, "--property=PrivateTmp=yes"); n != 1 {
			t.Errorf("flag=%v: PrivateTmp=yes appears %d times, want 1: %v", hardened, n, args)
		}
	}
}

// TestGAP119_TableClassificationIntegrity pins the table as the single source of
// truth: every candidate carries a verdict from the vocabulary, a risk line and
// its evidence; only SAFE rows may be armed; and the armed rows render exactly
// the set the builder emits.
func TestGAP119_TableClassificationIntegrity(t *testing.T) {
	verdicts := map[string]bool{
		unitHardeningSafe:           true,
		unitHardeningNeedsException: true,
		unitHardeningBreaks:         true,
	}
	seen := map[string]bool{}
	for _, p := range unitHardeningTable() {
		if p.Name == "" {
			t.Error("table row without a property name")
			continue
		}
		if seen[p.Name] {
			t.Errorf("duplicate property name %q in the classification table", p.Name)
		}
		seen[p.Name] = true
		if !verdicts[p.Verdict] {
			t.Errorf("%s: verdict %q is outside the vocabulary", p.Name, p.Verdict)
		}
		if strings.TrimSpace(p.Risk) == "" {
			t.Errorf("%s: no risk line — every property must document what it costs a workload", p.Name)
		}
		if strings.TrimSpace(p.Evidence) == "" {
			t.Errorf("%s: no evidence — a verdict without a measurement is taste", p.Name)
		}
		if p.InSet && p.Verdict != unitHardeningSafe {
			t.Errorf("%s: armed with verdict %s — only measured-SAFE properties may be armed", p.Name, p.Verdict)
		}
		if !p.InSet && p.Verdict == unitHardeningSafe {
			t.Errorf("%s: classified SAFE but not armed — either arm it or classify the exclusion", p.Name)
		}
	}

	// The armed rows render the set exactly (order included).
	knobs := hardenedUnitKnobs(true)
	var wantNames []string
	for _, p := range unitHardeningTable() {
		if p.InSet {
			wantNames = append(wantNames, p.Name)
		}
	}
	if len(knobs) != len(wantNames) {
		t.Fatalf("hardenedUnitKnobs(true) has %d properties, table has %d armed rows", len(knobs), len(wantNames))
	}
	for i, k := range knobs {
		if k.Name != wantNames[i] {
			t.Errorf("knob[%d] = %q, want %q (table order)", i, k.Name, wantNames[i])
		}
		if k.Scope != KnobScopeUnit {
			t.Errorf("%s: scope = %q, want %q (sandbox properties are per-unit)", k.Name, k.Scope, KnobScopeUnit)
		}
	}

	// Off is nil, not an empty slice: nothing to render, nothing to report.
	if got := hardenedUnitKnobs(false); got != nil {
		t.Errorf("hardenedUnitKnobs(false) = %v, want nil", got)
	}
	if got := hardenedUnitKnobNames(false); got != nil {
		t.Errorf("hardenedUnitKnobNames(false) = %v, want nil", got)
	}
	if got := hardenedUnitKnobNames(true); len(got) != len(wantNames) {
		t.Errorf("hardenedUnitKnobNames(true) = %v, want %d names", got, len(wantNames))
	}
}

// TestGAP119_ReportedSetMatchesArmedArgv is the reporting half (the family's
// "a bound that is not reported is not a bound" rule): the property set stamped
// on the agent record is the set the unit argv carries — PAMName and PrivateTmp
// excepted, which the pre-GAP-116 record never reported either.
func TestGAP119_ReportedSetMatchesArmedArgv(t *testing.T) {
	args, _ := gap119UnitArgs(true, "")
	cpuQuota, memMax, diskMax, maxProcs, maxFiles := gap116BaselineLimits()
	unitKnobs, _ := KnobsForPreset(config.SafetyPresetDefault, cpuQuota, memMax, diskMax, maxProcs, maxFiles)
	// The spawn site builds exactly this (sandbox block first, then limits).
	reported := append(append([]SystemdKnob{}, hardenedUnitKnobs(true)...), unitKnobs...)

	props := gap119PropertyArgs(args)
	// Two unconditional properties are not part of the reported set.
	unreported := map[string]bool{"PAMName=login": true, "PrivateTmp=yes": true}
	var wantProps []string
	for _, p := range props {
		if unreported[p] {
			continue
		}
		wantProps = append(wantProps, p)
	}
	if len(wantProps) != len(reported) {
		t.Fatalf("reported %d properties but the argv carries %d reportable ones\nargv: %v\nreported: %+v",
			len(reported), len(wantProps), props, reported)
	}
	for i, k := range reported {
		want := k.Name + "=" + k.Value
		if wantProps[i] != want {
			t.Errorf("reported[%d] = %q but argv carries %q (the reported set must mirror the argv)", i, want, wantProps[i])
		}
	}
}

// TestGAP119_DetachedRunUnitCarriesNoHardening documents the scope boundary: the
// flag governs the SPAWN unit. A detached `bunker run` unit is built by a
// different function that does not read the flag, so nothing changes there.
func TestGAP119_DetachedRunUnitCarriesNoHardening(t *testing.T) {
	limits := &v1.ResourceLimits{
		CpuQuota:       2.0,
		MemoryMaxBytes: 4294967296,
		DiskMaxBytes:   21474836480,
	}
	args := buildRunAgentArgs("abc123", "1001", "1001", "bunker-run-abc123-deadbeef", "make", []string{"test"}, nil, limits, false)
	for _, p := range unitHardeningTable() {
		for _, a := range args {
			if strings.HasPrefix(a, "--property="+p.Name) {
				t.Errorf("detached-run argv carries hardening property %q — the flag is spawn-scoped: %v", a, args)
			}
		}
	}
}

// TestGAP119_SpawnSiteWiresTheFlag is the anti-phantom check for the wiring: the
// config flag must reach BOTH the unit builder and the reported set. A refactor
// that drops either assignment leaves every builder-level test above green while
// the feature is dead in production, so the spawn source is read directly (the
// alternative proof — a full live spawn — belongs to the root-gated battery).
func TestGAP119_SpawnSiteWiresTheFlag(t *testing.T) {
	raw, err := os.ReadFile("manager_spawn.go")
	if err != nil {
		t.Fatalf("read spawn source: %v", err)
	}
	src := string(raw)
	for _, want := range []struct {
		snippet string
		why     string
	}{
		{"hardenedUnitKnobs(m.cfg.Agent.HardenedUnitProperties)", "the flag is resolved from config"},
		{"HardenedUnitProperties: m.cfg.Agent.HardenedUnitProperties", "the resolved flag reaches buildRootlessDockerdArgs"},
		{"UnitProperties:   systemdKnobsToProto(reportedUnitKnobs)", "the armed set is reported on the agent record"},
	} {
		if !strings.Contains(src, want.snippet) {
			t.Errorf("spawn site is missing %q — %s", want.snippet, want.why)
		}
	}
}
