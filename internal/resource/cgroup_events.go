package resource

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ── GAP-121: per-agent resource-ceiling observability ───────────────────────
//
// The cgroup v2 filesystem itself records what happened when an agent hit a
// resource ceiling: `memory.events` and `pids.events` are cumulative counters
// the kernel maintains per cgroup. Nothing in the daemon exposed them, so an
// operator could not answer the two questions that matter after a runaway
// agent: "did it hit its memory limit?" and "did the kernel kill it?".
//
// Everything here is READ-ONLY and best-effort, mirroring
// ReadAgentCgroupMetrics: a missing cgroup (stopped/destroyed agent, deleted
// user) yields zero counters with Available=false and a Detail naming what
// could not be read — never an error, never a panic, and never a zero that
// reads as "nothing happened".

// Peak sources reported on the wire (proto ResourceEvents.*_peak_source).
const (
	// PeakSourceCgroup means the value came from the kernel's own
	// memory.peak / pids.peak file (kernel >= 5.19 / >= 6.1). This is the
	// authoritative peak: it survives the daemon's own restarts and covers
	// every moment of the agent's life, not just the moments the daemon
	// looked.
	PeakSourceCgroup = "cgroup"
	// PeakSourceDaemon means the value is the highest usage the daemon's own
	// sampler observed. It is a LOWER BOUND — usage between two samples is
	// invisible — and it is lost when the daemon restarts. It exists so an
	// older kernel still has a peak to show, and so the surface never prints
	// "0" as if that were a measurement.
	PeakSourceDaemon = "daemon"
)

// MemoryEvents mirrors the cgroup v2 `memory.events` counters (proto
// MemoryEvents). Each value is a cumulative event count for the agent's
// cgroup, not a rate and not a current-state reading.
type MemoryEvents struct {
	// Max is the number of times the cgroup's memory usage reached
	// memory.max and reclaim was attempted — a direct ceiling hit.
	Max uint64
	// High is the number of times usage exceeded memory.high.
	High uint64
	// Low is the number of times usage dropped below memory.low.
	Low uint64
	// OOM is the number of times the cgroup entered an OOM state.
	OOM uint64
	// OOMKill is the number of times the kernel killed a process in the
	// cgroup to satisfy a memory limit.
	OOMKill uint64
	// OOMGroupKill is the number of times the whole cgroup was killed
	// (memory.oom.group).
	OOMGroupKill uint64
	// Available is false when memory.events could not be read. When false
	// every counter is zero because nothing was measured — NOT because
	// nothing happened.
	Available bool
}

// PidsEvents mirrors the cgroup v2 `pids.events` counters (proto PidsEvents).
type PidsEvents struct {
	// Max is the number of times a fork/clone was refused because pids.max
	// was already reached.
	Max uint64
	// Available is false when pids.events could not be read.
	Available bool
}

// AgentResourceEvents is the per-agent resource-ceiling observability view:
// the cgroup event counters, the live usage, and the observed peaks.
type AgentResourceEvents struct {
	MemoryEvents MemoryEvents
	PidsEvents   PidsEvents

	// MemoryCurrentBytes / PidsCurrent are the live readings
	// (memory.current / pids.current) at read time.
	MemoryCurrentBytes uint64
	PidsCurrent        uint64

	// MemoryPeakBytes / PidsPeak are the highest observed usage. The source
	// of each value is reported separately (see MemoryPeakSource) because
	// the two are not interchangeable.
	MemoryPeakBytes uint64
	PidsPeak        uint64

	// MemoryPeakSource / PidsPeakSource is PeakSourceCgroup, PeakSourceDaemon,
	// or "" when no peak is available from either.
	MemoryPeakSource string
	PidsPeakSource   string

	// CgroupPath is the agent cgroup slice the readings came from.
	CgroupPath string

	// Available is true when the agent's cgroup was located and at least one
	// event file was read. A false block (see MemoryEvents.Available /
	// PidsEvents.Available) means that counter set was not measured, which is
	// not the same statement as "measured zero".
	Available bool

	// Detail names exactly which reads failed. Empty when everything was
	// read.
	Detail string
}

// HaveAnyCeilingHit reports whether the agent has ever hit or been killed by
// a memory or pids ceiling. It is the one-line answer an operator wants, and
// it is deliberately false for an agent whose cgroup was never read — callers
// must check Available before treating false as "clean".
func (e *AgentResourceEvents) HaveAnyCeilingHit() bool {
	if e == nil {
		return false
	}
	return e.MemoryEvents.Max > 0 ||
		e.MemoryEvents.OOM > 0 ||
		e.MemoryEvents.OOMKill > 0 ||
		e.MemoryEvents.OOMGroupKill > 0 ||
		e.PidsEvents.Max > 0
}

// parseMemoryEvents parses a cgroup v2 memory.events file. ok is false when
// no known counter key was found, so a truncated/foreign file is reported as
// unreadable rather than as an all-zero reading.
func parseMemoryEvents(raw string) (MemoryEvents, bool) {
	ev := MemoryEvents{}
	seen := 0
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		val, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "max":
			ev.Max = val
			seen++
		case "high":
			ev.High = val
			seen++
		case "low":
			ev.Low = val
			seen++
		case "oom":
			ev.OOM = val
			seen++
		case "oom_kill":
			ev.OOMKill = val
			seen++
		case "oom_group_kill":
			ev.OOMGroupKill = val
			seen++
		}
	}
	if seen == 0 {
		return MemoryEvents{}, false
	}
	ev.Available = true
	return ev, true
}

// parsePidsEvents parses a cgroup v2 pids.events file. ok is false when the
// `max` counter was not found.
func parsePidsEvents(raw string) (PidsEvents, bool) {
	ev := PidsEvents{}
	ok := false
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		val, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		if fields[0] == "max" {
			ev.Max = val
			ok = true
		}
	}
	if !ok {
		return PidsEvents{}, false
	}
	ev.Available = true
	return ev, true
}

// readCgroupFile reads one file inside the agent's cgroup directory.
// required distinguishes a file the surface PROMISES (its absence is a
// finding, reported in Detail) from a best-effort one such as the optional
// `memory.peak`/`pids.peak` files that only exist on newer kernels (absent =
// simply unavailable; any OTHER error — EACCES, EIO — is still reported).
func readCgroupFile(base, name string, required bool, detail *[]string) ([]byte, bool) {
	raw, err := os.ReadFile(filepath.Join(base, name))
	if err == nil {
		return raw, true
	}
	if required || !errors.Is(err, os.ErrNotExist) {
		*detail = append(*detail, fmt.Sprintf("%s (%v)", name, err))
	}
	return nil, false
}

// ReadAgentResourceEvents reads the ceiling-event counters, live usage and
// observed peaks for one agent from its own cgroup, using the SAME layout as
// ReadAgentCgroupMetrics: every process the agent owns (the systemd-run
// rootless dockerd unit and each `bunker exec` session scope) lives under the
// agent user's slice, so the slice is the per-agent view.
//
// It never returns an error and never panics: an absent cgroup (stopped or
// destroyed agent, deleted user) yields a zero-valued struct with
// Available=false and a Detail naming the unreadable files. The daemon-side
// peak tracker is NOT consulted here — the caller merges it, so this function
// stays a pure read of the host.
func ReadAgentResourceEvents(uid int, agentID string) *AgentResourceEvents {
	base := agentUserSliceFn(uid)
	ev := &AgentResourceEvents{CgroupPath: base}
	var detail []string

	if raw, ok := readCgroupFile(base, "memory.events", true, &detail); ok {
		if parsed, parsedOK := parseMemoryEvents(string(raw)); parsedOK {
			ev.MemoryEvents = parsed
		} else {
			detail = append(detail, "memory.events (no recognized counter key)")
		}
	}

	if raw, ok := readCgroupFile(base, "pids.events", true, &detail); ok {
		if parsed, parsedOK := parsePidsEvents(string(raw)); parsedOK {
			ev.PidsEvents = parsed
		} else {
			detail = append(detail, "pids.events (no recognized counter key)")
		}
	}

	if raw, ok := readCgroupFile(base, "memory.current", false, &detail); ok {
		if val, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err == nil {
			ev.MemoryCurrentBytes = val
		}
	}
	if raw, ok := readCgroupFile(base, "pids.current", false, &detail); ok {
		if val, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err == nil {
			ev.PidsCurrent = val
		}
	}

	// Optional kernel-exposed peaks. A miss here is not a failure: the
	// daemon-side tracker supplies the value instead (see PeakSourceDaemon).
	if raw, ok := readCgroupFile(base, "memory.peak", false, &detail); ok {
		if val, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err == nil {
			ev.MemoryPeakBytes = val
			ev.MemoryPeakSource = PeakSourceCgroup
		}
	}
	if raw, ok := readCgroupFile(base, "pids.peak", false, &detail); ok {
		if val, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err == nil {
			ev.PidsPeak = val
			ev.PidsPeakSource = PeakSourceCgroup
		}
	}

	// Available reports that the agent's cgroup was located and at least one
	// event file was read. Each block carries its own Available flag; a false
	// block means that counter set was NOT measured (e.g. the pids controller
	// is not enabled for the slice), which is a different statement from
	// "measured zero" and must be rendered as such.
	ev.Available = ev.MemoryEvents.Available || ev.PidsEvents.Available
	sort.Strings(detail)
	ev.Detail = strings.Join(detail, "; ")
	return ev
}

// MergeDaemonPeaks folds the daemon-observed peaks into the cgroup reading.
// A cgroup peak (kernel memory.peak/pids.peak) always wins: it is the true
// high-water mark for the agent's whole life, while the daemon's value is
// only what the sampler happened to see. The daemon value is used when the
// kernel exposes nothing, and the source field says which it was — the two
// are never silently mixed under one label.
func (e *AgentResourceEvents) MergeDaemonPeaks(memPeak, pidsPeak uint64, ok bool) {
	if e == nil || !ok {
		return
	}
	if e.MemoryPeakSource != PeakSourceCgroup {
		if memPeak > e.MemoryPeakBytes {
			e.MemoryPeakBytes = memPeak
		}
		if e.MemoryPeakBytes > 0 {
			e.MemoryPeakSource = PeakSourceDaemon
		}
	}
	if e.PidsPeakSource != PeakSourceCgroup {
		if pidsPeak > e.PidsPeak {
			e.PidsPeak = pidsPeak
		}
		if e.PidsPeak > 0 {
			e.PidsPeakSource = PeakSourceDaemon
		}
	}
}
