//go:build !linux

// Package fsmount — the non-Linux stubs of the capability surface (BFS-052).
//
// The carriers (FuseState, Degradation, Capability, CapabilityState) are
// OS-neutral so the status document's shape is one shape everywhere. The
// probe that fills them reads /sys and mountinfo, which do not exist off
// Linux, so this build states the skip INSTEAD OF A DOCUMENT THAT READS AS
// GRANTED: the Fuse field carries only its source record
// ("not probed: <reason>"), with no capability entries at all — the
// DF-BUNKER-9 rule that unknown is never rendered as a safe value, at
// build granularity.
package fsmount

// fuseProbeSkipped is the !linux record: the status document carries the
// skip as its fuse block's source, never a fabricated document.
func fuseProbeSkipped() (skipped bool, reason string) {
	return true, "not probed: no FUSE binding on this platform (BFS-052 probes a Linux kernel's /sys/fs/fuse and /proc/self/mountinfo)"
}
