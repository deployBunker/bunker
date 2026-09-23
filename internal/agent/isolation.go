package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strings"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/hostsetup"
)

// ── Safety preset knob table (GAP-116) ────────────────────────────────
//
// This row is PLUMBING ONLY: the preset resolves to the SAME five knobs the
// spawn path already applied, with the SAME values, so a default-preset spawn
// is byte-identical to pre-GAP-116 (pinned by the zero-delta tests). The
// table exists so later rows (GAP-118/119) differentiate the tiers by
// extending it instead of growing new conditionals through the spawn path.

// SystemdKnob is one systemd property of an agent's effective knob set:
// exactly the property line written into the spawn unit argv / the slice
// drop-in.
type SystemdKnob struct {
	// Name is the systemd property name (CPUQuota, MemoryMax, TasksMax,
	// LimitNOFILE, LimitFSIZE).
	Name string
	// Value is the property value EXACTLY as written ("200%", "4294967296",
	// "65536:65536").
	Value string
	// Scope tells where the knob is applied: dockerd unit only ("unit"),
	// slice drop-in only ("slice"), or both ("both").
	Scope string
}

// Knob scopes. The dockerd unit carries CPUQuota/MemoryMax/LimitFSIZE/
// TasksMax/LimitNOFILE as --property; the slice drop-in carries the same set
// in ITS order (CPUQuota, MemoryMax, TasksMax, LimitNOFILE, LimitFSIZE). The
// scope column records membership so a future row can narrow a knob to one
// surface without re-deriving the tables.
const (
	KnobScopeUnit  = "unit"
	KnobScopeSlice = "slice"
	KnobScopeBoth  = "both"
)

// unitKnobsFor builds the dockerd-unit property set for one agent's resolved
// limits — the single source the unit argv builder consumes. The values and
// the conditional (only configured >0 limits emit a property) are exactly the
// pre-GAP-116 buildRootlessDockerdArgs logic, moved here unmodified.
func unitKnobsFor(cpuQuota float64, memMax, diskMax, maxProcs, maxFiles uint64) []SystemdKnob {
	var knobs []SystemdKnob
	if cpuQuota > 0 {
		knobs = append(knobs, SystemdKnob{Name: "CPUQuota", Value: fmt.Sprintf("%d%%", int(cpuQuota*100)), Scope: KnobScopeUnit})
	}
	if memMax > 0 {
		knobs = append(knobs, SystemdKnob{Name: "MemoryMax", Value: fmt.Sprintf("%d", memMax), Scope: KnobScopeUnit})
	}
	if diskMax > 0 {
		// LimitFSIZE caps the maximum file size (in bytes) an agent may
		// create. This is a pragmatic systemd-level enforcement for
		// disk_max_bytes when per-user filesystem quotas (xfs_quota) are not
		// configured.
		knobs = append(knobs, SystemdKnob{Name: "LimitFSIZE", Value: fmt.Sprintf("%d", diskMax), Scope: KnobScopeUnit})
	}
	if maxProcs > 0 {
		knobs = append(knobs, SystemdKnob{Name: "TasksMax", Value: fmt.Sprintf("%d", maxProcs), Scope: KnobScopeUnit})
	}
	if maxFiles > 0 {
		knobs = append(knobs, SystemdKnob{Name: "LimitNOFILE", Value: fmt.Sprintf("%d:%d", maxFiles, maxFiles), Scope: KnobScopeUnit})
	}
	return knobs
}

// KnobsForPreset is the preset → knob-set resolution (GAP-116 plumbing,
// GAP-117 naming). The vocabulary is {"standard", "open", "hardened"}; the
// shipped/default tier "standard" resolves to today's five-knob baseline, and
// "open"/"hardened" resolve to the SAME set until GAP-118/119 differentiate
// the tiers. The CALLER resolves the preset name through
// config.ResolveSafetyPreset (flag > env > config global > default) — here the
// name arrives already validated, and an unknown name is a programming error
// that fails LOUD (never a silent fallback).
//
// The knobs describe the SAME five properties the spawn path has always
// applied: unit surface in the unit argv's order (CPUQuota, MemoryMax,
// LimitFSIZE, TasksMax, LimitNOFILE), slice surface in the drop-in's order
// (CPUQuota, MemoryMax, TasksMax, LimitNOFILE, LimitFSIZE).
func KnobsForPreset(preset string, cpuQuota float64, memMax, diskMax, maxProcs, maxFiles uint64) (unit, slice []SystemdKnob) {
	switch preset {
	case config.SafetyPresetOpen, config.SafetyPresetStandard, config.SafetyPresetHardened:
		// Plumbing row: identical knob sets. Later rows differentiate here.
	default:
		panic(fmt.Sprintf("knobs for unknown safety preset %q — resolve through config.ResolveSafetyPreset", preset))
	}
	unit = unitKnobsFor(cpuQuota, memMax, diskMax, maxProcs, maxFiles)
	slice = sliceKnobsFor(cpuQuota, memMax, diskMax, maxProcs, maxFiles)
	return unit, slice
}

// ── GAP-118 DoS-containment knobs ──────────────────────────────────────────

// containmentKnobs is the DoS-containment knob set one tier requests on top
// of the five-knob baseline. The zero value requests NOTHING, which is
// exactly the open-tier shape — an absent knob is never emitted by the table,
// so a tier that does not request a knob can never trip its landing check
// (the matrix's MemoryOOMGroup rule, applied to every GAP-118 knob).
type containmentKnobs struct {
	// swapMax: true = emit MemorySwapMax=0 (bar swap); false = emit nothing
	// (host default). The field is a bool rather than a number because the
	// matrix's tier verdict is BINARY (bar / leave host default) — the bytes
	// value is always 0 when set.
	swapMax bool
	// memHighPct is the MemoryHigh cushion as a PERCENT of the agent's
	// resolved MemoryMax (the matrix's "90% of Max" cell); 0 = emit nothing.
	// The table carries the percent — the matrix's design unit — and the
	// resolver derives the bytes per agent.
	memHighPct int
	// oomGroup requests MemoryOOMGroup=yes; no tier in this table sets it.
	oomGroup bool
	// ioWeight requests IOWeight=N; 0 = emit nothing (no tier defaults it).
	ioWeight int64
	// ioWriteBps requests IOWriteBandwidthMax=<whole-disk> <bps>; 0 = emit
	// nothing.
	ioWriteBps int64
}

// containmentForPreset is the GAP-118 tier table. It is the binding code of
// docs/presets/knob-safety-matrix.md's "Tier matrix" section (GAP-114,
// measured on this host 2026-09-22); every cell cites the matrix:
//
//	MemorySwapMax  standard/hardened: 0 (bar)   open: host default
//	  (matrix: with swap barred an at-limit workload dies loudly at the cap;
//	  with swap allowed the same under-sizing is masked by paging. Bar-swap
//	  must NEVER reach open — it converts a would-have-completed run into a
//	  hard OOM.)
//	MemoryHigh     standard/hardened: 90% of MemoryMax   open: off
//	  (matrix: measured graceful — throttle + swap spill, zero kills; the
//	  peak pins exactly at the cap. systemd MemoryHigh= only: docker
//	  --memory-reservation measured a cgroup-v2 no-op.)
//	MemoryOOMGroup open/standard/hardened: off
//	  (matrix: UNMEASURED-here — the systemd 259 user manager refuses the
//	  property and delegated cgroupfs writes EACCES; blocked from
//	  default-on. A tier that does not request it must never hit its
//	  landing check.)
//	IOWeight       off on every tier (matrix: measured INERT on uncontended
//	  NVMe — 900 vs 100 weights, identical throughput). Config opt-in only.
//	IO write bound standard: opt-in (emit nothing here), open: off.
//	  (matrix: the bound measured effective and exact; standard stays
//	  opt-in so a 1.4GB docker load keeps today's throughput. An emitted
//	  bound must resolve to the WHOLE disk device — io.max rejects
//	  partitions, measured ENODEV.)
//
// Tier-name mapping note (matrix "Findings" #1): the matrix names the four
// tiers open/standard/guarded/hostile; this code's vocabulary is
// open/standard/hardened, and hardened is the guarded-and-above reading —
// the containment cells are identical for the upper tiers, so the table
// maps hardened to the matrix's guarded/hostile rows without changing a
// measured value.
func containmentForPreset(preset string) containmentKnobs {
	switch preset {
	case config.SafetyPresetOpen:
		return containmentKnobs{}
	case config.SafetyPresetStandard, config.SafetyPresetHardened:
		return containmentKnobs{
			swapMax:    true,
			memHighPct: 90, // matrix: 90% of MemoryMax, measured-graceful
		}
	default:
		panic(fmt.Sprintf("containment knobs for unknown safety preset %q — resolve through config.ResolveSafetyPreset", preset))
	}
}

// ── the resolution-order seam (GAP-118) ────────────────────────────────────
//
// The spec (specs/safety-presets.md §3, as GAP-117 pinned it) makes the
// agent.Default* config values the SOURCE of the shipped tier's five baseline
// numbers — the preset selects the bundle, it does not duplicate the
// arithmetic. The GAP-118 containment knobs inherit exactly that shape: the
// tier table above is the DESIGN authority (the matrix's verdicts), and the
// config fields on AgentConfig are the ADMIN-OVERRIDE seam in the documented
// precedence tier-table → daemon config → per-spawn request. The tier table
// holds the matrix's authority and config/release wins only where an
// operator explicitly sets them; the per-spawn leg lands with the row that
// adds the flag (run.go's vocabulary guard already resolves the tier for the
// detached-run path).
//
// resolveOrderErr anchors that documented order in code so the precedence
// test has a named seam to pin; production never reads it.
var resolveOrderErr = errors.New("resolve order: tier table -> daemon config -> per-spawn request")

// sliceKnobsFor builds the slice drop-in property set in the drop-in's own
// order — the single source applyUserSliceLimits consumes. The values and the
// conditional are exactly the pre-GAP-116 applyUserSliceLimits logic, moved
// here unmodified.
func sliceKnobsFor(cpuQuota float64, memMax, diskMax, maxProcs, maxFiles uint64) []SystemdKnob {
	var knobs []SystemdKnob
	if cpuQuota > 0 {
		knobs = append(knobs, SystemdKnob{Name: "CPUQuota", Value: fmt.Sprintf("%d%%", int(cpuQuota*100)), Scope: KnobScopeSlice})
	}
	if memMax > 0 {
		knobs = append(knobs, SystemdKnob{Name: "MemoryMax", Value: fmt.Sprintf("%d", memMax), Scope: KnobScopeSlice})
	}
	if maxProcs > 0 {
		knobs = append(knobs, SystemdKnob{Name: "TasksMax", Value: fmt.Sprintf("%d", maxProcs), Scope: KnobScopeSlice})
	}
	if maxFiles > 0 {
		knobs = append(knobs, SystemdKnob{Name: "LimitNOFILE", Value: fmt.Sprintf("%d:%d", maxFiles, maxFiles), Scope: KnobScopeSlice})
	}
	if diskMax > 0 {
		knobs = append(knobs, SystemdKnob{Name: "LimitFSIZE", Value: fmt.Sprintf("%d", diskMax), Scope: KnobScopeSlice})
	}
	return knobs
}

// containmentResolved is one agent's effective containment knob set after the
// documented precedence (tier table → daemon config overrides) has been
// applied. It is also the landing-check request: every field the enforcer
// emitted must be verifiable against the live cgroup, and every field a tier
// did not request must never trip a check.
type containmentResolved struct {
	// swapBarred is true when MemorySwapMax=0 must be applied AND verified.
	swapBarred bool
	// memHigh is the requested MemoryHigh in bytes; 0 = not requested (no
	// emission, no landing check).
	memHigh uint64
	// ioWeight / ioWriteBps: requested values; 0 = not requested.
	ioWeight   int64
	ioWriteBps int64
	// oomGroup is the request for memory.oom.group. The tier table never
	// sets it (matrix: UNMEASURED-here, blocked from default-on); the field
	// exists so a future capable-host measurement can arm the knob through
	// the table in one place, and so the landing check's request gate is
	// testable via the same struct the enforcer consumes.
	oomGroup bool
	// swapOverride / highOverride record that the tier's value was replaced
	// by a daemon-config override (reported, never silent — the operator
	// must be able to see that a spawn ran on an overridden knob).
	swapOverride bool
	highOverride bool
	ioOverride   bool
	// containerOnly names knobs that belong to the container/hosted layer
	// per the matrix and must NOT be claimed from a user-unit knob list.
	// Populated only when such a knob is explicitly configured; no tier
	// table entry sets it.
	containerOnly []string
}

// resolveContainmentKnobs merges the tier table with the daemon's admin
// overrides (config.Agent, populated by the caller from cfg). Precedence is
// the spec's: the tier table decides unless the operator explicitly set a
// value; -1 releases a tier knob back to the host default (the only sane
// override direction for bar-swap — matrix: forcing swap OFF open is the
// measured-UNSAFE direction, and 0 on an int64 field means "unset").
// The emitNothing rule: a knob resolved to "not requested" emits NO property
// on ANY surface, so an unrequested knob can never trip its landing check.
func resolveContainmentKnobs(preset string, memMax uint64, a config.AgentConfig) containmentResolved {
	tier := containmentForPreset(preset)
	r := containmentResolved{
		swapBarred: tier.swapMax,
		ioWeight:   tier.ioWeight,
		ioWriteBps: tier.ioWriteBps,
	}
	// The tier's cushion is a PERCENT of this agent's resolved MemoryMax;
	// an open-tier table (0%) leaves the knob unrequested — emitNothing.
	if tier.memHighPct > 0 {
		r.memHigh = memMax / 100 * uint64(tier.memHighPct)
		if r.memHigh == 0 {
			r.memHigh = memMax
		}
	}
	if c := a.Containment; c.MemorySwapMaxBytes == -1 || a.DefaultMemorySwapMaxBytes == -1 {
		r.swapBarred = false
		r.swapOverride = true
	}
	if c := a.Containment; c.MemoryHighBytes > 0 || a.DefaultMemoryHighBytes > 0 {
		v := c.MemoryHighBytes
		if v == 0 {
			v = a.DefaultMemoryHighBytes
		}
		r.memHigh = uint64(v)
		r.highOverride = true
	}
	if c := a.Containment; c.IOWeight > 0 || a.DefaultIOWeight > 0 {
		v := c.IOWeight
		if v == 0 {
			v = a.DefaultIOWeight
		}
		r.ioWeight = v
		r.ioOverride = true
	}
	if c := a.Containment; c.IOWriteBps > 0 || a.DefaultIOWriteBps > 0 {
		v := c.IOWriteBps
		if v == 0 {
			v = a.DefaultIOWriteBps
		}
		r.ioWriteBps = v
		r.ioOverride = true
	}
	return r
}

// sliceContainmentKnobs renders the containment set in the slice drop-in's
// own order. The five-knob baseline set is untouched — this function only
// renders the GAP-118 additions. The write-bound property carries the WHOLE
// disk device resolved from the root filesystem's backing device (io.max
// rejects partitions — matrix correction #5); an unresolvable device yields
// no property rather than a wrong one, and the caller's landing check then
// fails the spawn loudly (never a silent no-op).
func sliceContainmentKnobs(r containmentResolved) []SystemdKnob {
	var knobs []SystemdKnob
	if r.swapBarred {
		knobs = append(knobs, SystemdKnob{Name: "MemorySwapMax", Value: "0", Scope: KnobScopeSlice})
	}
	if r.memHigh > 0 {
		knobs = append(knobs, SystemdKnob{Name: "MemoryHigh", Value: fmt.Sprintf("%d", r.memHigh), Scope: KnobScopeSlice})
	}
	if r.ioWeight > 0 {
		knobs = append(knobs, SystemdKnob{Name: "IOWeight", Value: fmt.Sprintf("%d", r.ioWeight), Scope: KnobScopeSlice})
	}
	if r.ioWriteBps > 0 {
		if dev, err := resolveWholeDiskDevice(); err == nil {
			knobs = append(knobs, SystemdKnob{
				Name:  "IOWriteBandwidthMax",
				Value: fmt.Sprintf("%s %d", dev, r.ioWriteBps),
				Scope: KnobScopeSlice,
			})
		}
	}
	return knobs
}

// resolveWholeDiskDevice resolves the whole-disk device for the ROOT
// filesystem (the matrix's bound applies to the agent host's main device).
// io.max rejects partition devices (measured ENODEV on nvme0n1p2), so the
// resolver walks /sys/block until it finds a disk holding the root fs's
// device. It is a var so tests can pin the resolution without a real NVMe.
var resolveWholeDiskDevice = resolveWholeDiskDeviceReal

func resolveWholeDiskDeviceReal() (string, error) {
	// Find the partition holding / from /proc/self/mountinfo, then map it to
	// its parent disk via the /sys/block/<disk>/<part>/ hierarchy.
	partName, err := rootDeviceName()
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(wholeDiskSysBlockRoot)
	if err != nil {
		return "", fmt.Errorf("list %s: %w", wholeDiskSysBlockRoot, err)
	}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(wholeDiskSysBlockRoot, e.Name(), partName)); err == nil {
			return "/dev/" + e.Name(), nil
		}
	}
	return "", fmt.Errorf("no whole-disk device in /sys/block holds partition %q", partName)
}

// rootDeviceMountInfoPath is the mount table read to find the root fs's
// device. Var so tests can point it at a fixture (same seam pattern as
// runtimeDirMountInfoPath).
var rootDeviceMountInfoPath = "/proc/self/mountinfo"

// wholeDiskSysBlockRoot is the sysfs block hierarchy walked to map a
// partition to its parent disk. Var for the same reason.
var wholeDiskSysBlockRoot = "/sys/block"

// rootDeviceName returns the kernel device name backing the root filesystem
// from the mount table (e.g. "nvme0n1p2" for /dev/nvme0n1p2). The line is
// located by mountpoint "/" and the source is taken from AFTER the "-"
// separator (fstype, source, super-options), which sidesteps overlayfs-style
// wrapper lines where the source is not the real device.
func rootDeviceName() (string, error) {
	data, err := os.ReadFile(rootDeviceMountInfoPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", rootDeviceMountInfoPath, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[4] != "/" {
			continue
		}
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+2 >= len(fields) {
			continue
		}
		dev := path.Base(fields[sep+2])
		if dev == "" || dev == "none" || dev == "overlay" {
			continue
		}
		return dev, nil
	}
	return "", fmt.Errorf("root filesystem device not found in %s", rootDeviceMountInfoPath)
}

// ── the R2 landing check (write-then-read-back, fail loud) ─────────────────

// cgroupV2Root is where the unified hierarchy is mounted. Var: the landing
// tests point it at a fixture tree; production never writes it.
var cgroupV2Root = "/sys/fs/cgroup"

// verifyContainmentLanding verifies the requested containment knobs against
// the LIVE cgroup of the agent's user slice: each REQUESTED knob is read back
// and compared; an unrequested knob is skipped (never failed — a tier that
// does not ask for memory.oom.group must not die on its landing check); a
// knob the cgroup does not expose at all, or exposes at a different value,
// fails LOUD with the requested-vs-observed pair (the no-silent-no-op rule —
// R2). On this host an ENOENT or EACCES read is exactly how a refused knob
// presents (matrix: delegated controller writes EACCES), so both degrade to
// the same loud failure path.
//
// It is a function on the manager (not a free function) only to keep the
// read seam next to the spawn flow that consumes it.
func (m *AgentManager) verifyContainmentLanding(uid string, want containmentResolved) error {
	base := filepath.Join(cgroupV2Root, "user.slice", "user-"+uid+".slice")
	read := func(file string) (string, error) {
		b, err := os.ReadFile(filepath.Join(base, file))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	if want.swapBarred {
		got, err := read("memory.swap.max")
		if err != nil {
			return fmt.Errorf("containment landing: read memory.swap.max: %w", err)
		}
		if got != "0" {
			return fmt.Errorf("containment landing: memory.swap.max = %q, want \"0\" (bar swap)", got)
		}
	}
	if want.memHigh > 0 {
		got, err := read("memory.high")
		if err != nil {
			return fmt.Errorf("containment landing: read memory.high: %w", err)
		}
		if got != fmt.Sprintf("%d", want.memHigh) {
			return fmt.Errorf("containment landing: memory.high = %q, want %d", got, want.memHigh)
		}
	}
	if want.oomGroupRequested() {
		got, err := read("memory.oom.group")
		if err != nil {
			// Honest degradation on a host whose manager refuses the
			// property: the refusal IS the fault path, asserted — not
			// swept. The wrap says so explicitly.
			return fmt.Errorf("containment landing: memory.oom.group unreadable (the host's manager likely refuses the property; see the matrix's UNMEASURED register): %w", err)
		}
		if got != "1" {
			return fmt.Errorf("containment landing: memory.oom.group = %q, want \"1\"", got)
		}
	}
	if want.ioWeight > 0 {
		got, err := read("io.weight")
		if err != nil {
			return fmt.Errorf("containment landing: read io.weight: %w (the io controller may not be delegated to the user slice on this host)", err)
		}
		if got != fmt.Sprintf("%d", want.ioWeight) {
			return fmt.Errorf("containment landing: io.weight = %q, want %d", got, want.ioWeight)
		}
	}
	if want.ioWriteBps > 0 {
		got, err := read("io.max")
		if err != nil {
			return fmt.Errorf("containment landing: read io.max: %w (the io controller may not be delegated to the user slice on this host)", err)
		}
		dev, devErr := resolveWholeDiskDevice()
		if devErr != nil {
			return fmt.Errorf("containment landing: resolve whole-disk device: %w", devErr)
		}
		wantVal := fmt.Sprintf("%s wbps=%d", dev, want.ioWriteBps)
		found := false
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), dev+" ") && strings.Contains(line, fmt.Sprintf("wbps=%d", want.ioWriteBps)) {
				found = true
				break
			}
			if strings.HasPrefix(strings.TrimSpace(line), dev+" ") {
				// Right device, wrong/no bound: report the observed line so
				// the failure names what actually landed.
				return fmt.Errorf("containment landing: io.max for %s = %q, want %q", dev, strings.TrimSpace(line), wantVal)
			}
		}
		if !found {
			return fmt.Errorf("containment landing: io.max has no line for %s (want %q)", dev, wantVal)
		}
	}
	return nil
}

// oomGroupRequested reports whether the resolved set asks for
// memory.oom.group — the tier-table gate that keeps an unrequested knob from
// ever reaching its landing check. The tier table never sets oomGroup
// (matrix: UNMEASURED, blocked from default-on), so a request can only come
// from a future capable-host row that extends containmentForPreset.
func (r containmentResolved) oomGroupRequested() bool { return r.oomGroup }

// lookupAgentUser resolves an agent's uid/gid. It is a variable so tests can
// drive the spawn-time isolation wiring without root privileges (the real
// lookup only succeeds after useradd has run).
var lookupAgentUser = user.Lookup

// Isolation boundary (GAP-075).
//
// Every agent gets an ENFORCED private /tmp and the only sanctioned
// cross-agent exchange point is the bounded shared scratch directory:
//
//   - transient systemd units (rootless dockerd, detached RunAgent) carry
//     --property=PrivateTmp=yes, so their /tmp is a private tmpfs of the unit;
//   - SSH sessions (/tmp for `bunker exec`, scp, sshfs, docker transport) get
//     a private per-session /tmp from pam_namespace. Bunker's sshd block keys
//     on the reserved agent NAME pattern and then requires the agent group
//     membership, the exact Bunker /tmp rule, the instance parent and its own
//     trust chain (root-owned, non-writable helper directory -> root-owned
//     manifest -> the helper's bytes) before the module runs; ordinary
//     operator sessions are jumped over the whole block and keep the host /tmp
//     (see internal/hostsetup);
//   - /srv/bunker-share is what the daemon creates per agent: its root is
//     root-owned, setgid and NOT writable by the agent group or the world
//     (mode 2750), so no agent can create an uncapped plain entry beside the
//     per-agent directories, and each per-agent directory is a size-capped
//     tmpfs with group/setgid semantics.
//
// Membership in the agent group is verified by that precondition, so it is
// provisioned for EVERY agent and is NOT gated by the shared-scratch toggle:
// disabling the optional exchange directory must not hand an agent the host's
// shared /tmp. A spawn that cannot grant the membership fails closed.
//
// The host-level half is idempotent and fail-closed too: a scratch directory
// whose bounded filesystem cannot be mounted is not created at all, so an agent
// never ends up with an unbounded exchange directory.

// dockerdUnitArgs are the inputs of the per-agent rootless-dockerd transient
// unit.
type dockerdUnitArgs struct {
	AgentID        string
	UnitName       string
	UID            string
	GID            string
	UserHome       string
	RuntimeDir     string
	DockerSockPath string
	RootlessBin    string
	CPUQuota       float64
	MemoryMax      uint64
	DiskMax        uint64
	MaxProcesses   uint64
	MaxOpenFiles   uint64
	// UnitKnobs is the GAP-116 resolved limit-property set (the preset's knob
	// table for this agent's limits). When nil/empty the builder derives the
	// set from the limits above — the derivation is identical to pre-GAP-116,
	// so both paths produce the same argv for the same limits.
	UnitKnobs []SystemdKnob
}

// buildRootlessDockerdArgs builds the exact `systemd-run` argv and the
// environment for the rootless-dockerd unit. It is a pure function so every
// unit property — including the GAP-075 PrivateTmp=yes that gives dockerd (and
// everything it starts) a private /tmp — is pinned by unit tests instead of by
// an integration test on a live host.
//
// The limit properties are built from the GAP-116 knob table (unitKnobsFor):
// the preset resolves to the same five properties with the same values as
// pre-GAP-116, so a default-preset argv is byte-identical (pinned by the
// zero-delta test).
//
// DOCKERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns avoids needing a separate
// bridge, and the per-agent socket path is passed through DOCKER_HOST so the
// socket is created where bunkerd expects it.
//
// PID namespace isolation (--pidns) is intentionally omitted: rootlesskit
// v1.1.1 does not support --detach-netns, so mixing the two flags makes
// rootlesskit exit immediately. It will be revisited when the installed
// rootlesskit supports it.
//
// There is no systemd property for a container-count cap (docker's daemon and
// systemd neither expose nor enforce one per user); max_docker_containers is
// enforced at spawn time by counting the just-started dockerd's containers,
// and the limit is carried on the agent record for that policy check.
func buildRootlessDockerdArgs(a dockerdUnitArgs) (args []string, env []string) {
	// systemd-run --system with --uid does not inherit the caller's
	// environment, so every variable the rootless stack needs is passed with
	// --setenv.
	env = []string{
		"PATH=" + filepath.Join(a.UserHome, "bin") + ":/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + a.UserHome,
		"USER=" + "bunker-" + a.AgentID,
		"XDG_RUNTIME_DIR=" + a.RuntimeDir,
		"DOCKERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns",
		"DOCKERD_ROOTLESS_ROOTLESSKIT_PORT_DRIVER=builtin",
		// rootlesskit v1.1.1 does not support --detach-netns; the
		// dockerd-rootless.sh shipped with the installer defaults to "true"
		// and appends --detach-netns to ROOTLESSKIT_FLAGS, which makes
		// rootlesskit exit immediately. Force it off.
		"DOCKERD_ROOTLESS_ROOTLESSKIT_DETACH_NETNS=false",
		"DOCKER_HOST=unix://" + a.DockerSockPath,
		// GAP-075: TMPDIR points at /tmp, which PrivateTmp=yes makes private
		// to this unit. The legacy /run/bunker/<id>/tmp is still created but
		// is not advertised here — it is not an isolation boundary.
		"TMPDIR=" + config.IsolationTmpDir,
	}

	args = []string{
		"--system",
		"--unit=" + a.UnitName,
		"--uid=" + a.UID,
		"--gid=" + a.GID,
		"--property=PAMName=login",
		// GAP-075: systemd gives the unit a private mount namespace with its
		// own /tmp, so nothing dockerd starts can observe or collide with the
		// host's (root's) /tmp or with another agent's.
		"--property=PrivateTmp=yes",
	}
	// GAP-116: the limit property block is table-driven (same values, same
	// order, same conditional as pre-GAP-116 — pinned byte-for-byte by the
	// zero-delta test). The preset name itself carries no knob information in
	// this row: every valid preset resolves to the baseline set, and the
	// resolved limits above ARE the baseline when the caller spawned with the
	// built-in default.
	knobs := a.UnitKnobs
	if len(knobs) == 0 {
		knobs = unitKnobsFor(a.CPUQuota, a.MemoryMax, a.DiskMax, a.MaxProcesses, a.MaxOpenFiles)
	}
	for _, k := range knobs {
		args = append(args, "--property="+k.Name+"="+k.Value)
	}
	for _, e := range env {
		args = append(args, "--setenv="+e)
	}
	args = append(args, a.RootlessBin, "--host=unix://"+a.DockerSockPath)
	return args, env
}

// hostSetup returns the host-provisioning options for this manager, with the
// configured agent group, scratch root/cap and the private-/tmp instance root.
func (m *AgentManager) hostSetup() hostsetup.Options {
	iso := m.cfg.Agent.Isolation
	iso.Defaults()
	o := hostsetup.DefaultOptions()
	o.ScratchEnabled = iso.SharedScratchEnabled
	o.ScratchRoot = iso.SharedScratchRoot
	o.AgentGroup = iso.AgentGroup
	o.ScratchPerAgent = iso.SharedScratchPerAgentBytes
	o.TmpInstanceRoot = iso.PrivateTmpRoot
	if m.hostRunner != nil {
		o.Runner = m.hostRunner
	}
	return o.WithDefaults()
}

// IsolationGrantCapability is advertised in `bunkerd --version` by any build
// that ships the spawn-side isolation grant (provisionIsolation in this file,
// StageIsolationProvision in manager_spawn.go). The installer's daemon-skew
// probe REQUIRES this token, because a version number cannot prove the grant:
// a bare `go build` of any revision reports the package default version, so a
// binary claiming a version like 0.1.4 may carry no grant at all.
//
// The token is declared HERE, in the same package as the grant it proves, so a
// build without the grant cannot advertise it: dropping provisionIsolation
// drops this constant with it. internal/hostsetup keeps its own literal copy
// (it cannot import this package — internal/agent imports internal/hostsetup,
// so the reverse would be an import cycle); a test in this package pins the two
// copies equal.
const IsolationGrantCapability = "isolation-grant"

// SpawnCapabilities lists the spawn-side capability tokens this build reports
// in `bunkerd --version`. It is the single source of the advertised list, so
// adding a token here is the only way to advertise one.
func SpawnCapabilities() []string { return []string{IsolationGrantCapability} }

// provisionIsolation provisions the agent-side half of the boundary.
//
// The agent group membership comes FIRST and is REQUIRED: the sshd pam_exec
// precondition verifies it before pam_namespace runs, so an agent that is not a
// member has its sessions DENIED — a failure here returns an error and the
// caller aborts the spawn (fail closed) instead of leaving an agent that cannot
// open a session at all.
//
// The remaining steps are best-effort with loud logging, deliberately: a
// scratch that cannot be bounded must NOT be replaced by an unbounded directory
// (that is the fail-closed rule implemented in internal/hostsetup), and a
// missing instance directory is created by pam_namespace on the first session
// (owned by root, mirroring /tmp), so neither failure is a reason to abort a
// spawn that is otherwise complete.
func (m *AgentManager) provisionIsolation(ctx context.Context, agentID, username string, uid, gid int) error {
	host := m.hostSetup()

	memRep, err := host.EnsureAgentGroupMembership(ctx, username)
	if err != nil {
		return fmt.Errorf("add %s to the agent isolation group %s: %w", username, host.AgentGroup, err)
	}
	m.logger.Info("agent isolation group membership ensured", "agent_id", agentID, "group", host.AgentGroup)
	m.logger.Debug("isolation membership report", "agent_id", agentID, "report", memRep.String())

	if m.cfg.Agent.Isolation.SharedScratchEnabled {
		rep, err := host.EnsureAgentScratch(ctx, agentID, username, uid, gid)
		if err != nil {
			// DF-BUNKER-40: the likely cause is the exchange ROOT, and the
			// message must say so — a root left root:root (or group-writable)
			// still produced "shared scratch ready" before, while the agent
			// could not even list the directory. `bunker host-provision
			// --apply` repairs the root on this and every other host; it is
			// also the fail-closed remedy when the spawn path could not.
			m.logger.Warn("shared scratch not provisioned; agent keeps its private /tmp and has no cross-agent exchange directory (check the exchange root, then run `bunker host-provision --apply`)",
				"agent_id", agentID, "error", err)
		} else {
			m.logger.Info("shared scratch ready", "agent_id", agentID)
			m.logger.Debug("shared scratch provisioning report", "agent_id", agentID, "report", rep.String())
		}
	} else {
		m.logger.Info("shared scratch disabled by configuration; private /tmp isolation is unaffected", "agent_id", agentID)
	}

	rep, err := host.EnsureAgentTmpInstance(ctx, agentID, username, uid, gid)
	if err != nil {
		m.logger.Warn("private /tmp instance directory not pre-created; pam_namespace will create it on first session",
			"agent_id", agentID, "error", err)
		return nil
	}
	m.logger.Info("private /tmp instance ready", "agent_id", agentID)
	m.logger.Debug("private /tmp instance provisioning report", "agent_id", agentID, "report", rep.String())
	return nil
}

// removeIsolation removes the agent's scratch and /tmp instance directories
// during destroy. Both removals are idempotent, so a partially provisioned
// (or never provisioned) agent destroys cleanly.
//
// DF-BUNKER-21: the outcome is RETURNED as well as logged. The spawn rollback
// records it in the failure breadcrumb, and a breadcrumb that claimed
// "isolation removed" while both removals had failed was one of the swallowed
// failures the QA foreman had to reconstruct from the host. Destroy keeps
// calling it best-effort (the error is logged, the destroy proceeds).
func (m *AgentManager) removeIsolation(ctx context.Context, agentID string) error {
	host := m.hostSetup()
	var errs []error
	if rep, err := host.RemoveAgentScratch(ctx, agentID); err != nil {
		m.logger.Warn("shared scratch removal incomplete", "agent_id", agentID, "error", err)
		errs = append(errs, fmt.Errorf("shared scratch: %w", err))
	} else {
		m.logger.Debug("shared scratch removed", "agent_id", agentID, "report", rep.String())
	}
	if rep, err := host.RemoveAgentTmpInstance(ctx, agentID); err != nil {
		m.logger.Warn("private /tmp instance removal incomplete", "agent_id", agentID, "error", err)
		errs = append(errs, fmt.Errorf("private /tmp instance: %w", err))
	} else {
		m.logger.Debug("private /tmp instance removed", "agent_id", agentID, "report", rep.String())
	}
	return errors.Join(errs...)
}
