//go:build linux

// Package fsmount — the kernel-side FUSE capability probe (BFS-052).
//
// THE LAW THIS IMPLEMENTS (SPEC-linux-io-max §5, §6): kernel capability
// support is version-dependent and must be probed, never assumed — a
// requested cap the kernel lacks fails or SILENTLY NO-OPS, so a mount that
// asked for something it did not get must not report itself as tuned. The
// probe reads the kernel's OWN observables after fs.Mount succeeds and
// compares them against what this mount requested:
//
//   - /proc/self/mountinfo — the mount's OWN line, matched by mountpoint.
//     It names the filesystem type (fuse/fuseblk/fusectl with the source
//     name), the mount options, and — for FUSE mounts — the kernel device
//     <major>:<minor> whose minor IS the connection id. Matching the
//     mountpoint (rather than "the newest connection") is what makes these
//     figures this mount's and not another mount's: the live table shows
//     several FUSE connections at different negotiated values (12/9, 32/24,
//     50/37), so a probe that guessed a connection would report someone
//     else's negotiation.
//   - /sys/fs/fuse/connections/<id>/ — the per-connection knobs the kernel
//     itself documents as per-mount: max_background,
//     congestion_threshold, waiting. The directory is owned by root and
//     readable by the connection owner (the process that still holds the
//     /dev/fuse descriptor); for a non-owner the kernel's documented
//     fallback is fusectl's FUSE_DEV_IOC_CLONE, which this probe does not
//     invoke — a benign mount must not open a second device for reporting's
//     sake — so an unreadable connection degrades its capabilities to
//     unknown with the named reason instead.
//   - /sys/class/bdi/<major:minor>/read_ahead_kb — the backing device's
//     READAHEAD. go-fuse hands the kernel MaxReadAhead at INIT; the kernel
//     clamps it to this knob's own limit, so the knob is the EFFECTIVE
//     value and MaxReadAhead was only the request (spec §2's lever table:
//     "kernel-clamped from our requested value").
//
// EVERY read tolerates absence: a missing /sys entry, a connection directory
// that vanished (the kernel drops it at unmount), or an unreadable file
// DEGRADES THAT CAPABILITY TO CapabilityStateUnknown with a reason — it
// never fails the mount. The probe runs after the mount exists and before
// the status loop starts, so the mount's whole lifetime carries the
// figures.
package fsmount

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// sysFSFuseConnections is the kernel's per-connection directory. It is a var
// only so the unit cells can point it at a fixture tree; production reads
// the constant path.
var sysFSFuseConnections = "/sys/fs/fuse/connections"

// procMountinfo is the mount table the probe resolves this mount's own line
// from. It is a var only so the negative-control test can point it at a
// fixture; production code reads the constant path.
var procMountinfo = "/proc/self/mountinfo"

// sysClassBDI is the backing-device directory tree (read_ahead_kb lives
// under /sys/class/bdi/<major:minor>/). Var for the same fixture reason as
// procMountinfo and sysFSFuseConnections.
var sysClassBDI = "/sys/class/bdi"

// The numeric capability names and the behavioural ones. The "fuse:"
// prefix keeps them namespaced beside the server-side vocabulary ("watch",
// "h3", "op:<name>", "lock") in the same capability field.
const (
	capMaxBackground = "fuse:max_background"
	capCongestion    = "fuse:congestion_threshold"
	capMaxReadAhead  = "fuse:max_readahead"
	capMaxWrite      = "fuse:max_write"
	capLocks         = "fuse:locks"
	capSymlinkCache  = "fuse:symlink_caching"
	capReadDirPlus   = "fuse:readdirplus"
	capSplice        = "fuse:splice"
)

// The named reasons a probe records. They are the kernel's MECHANISM, not
// prose: a consumer (or a test) matches on them.
const (
	// reasonKernelClamp: the kernel runs a SMALLER value than requested —
	// its own limit binds.
	reasonKernelClamp = "kernel_clamp"
	// reasonKernelSetOther: the kernel runs a different value without a
	// clamp being the mechanism (a default it chose, a config of its own).
	reasonKernelSetOther = "kernel_set_other_value"
	// reasonFeatureAbsent: the kernel's negotiated flag bits lack the
	// feature the mount requested — the silent no-op this row exists to
	// surface.
	reasonFeatureAbsent = "feature_absent_in_kernel_flags"
)

// mountLine is this mount's own /proc/self/mountinfo line fields.
type mountLine struct {
	// dev is the "major:minor" device string, e.g. "0:905" — for a FUSE
	// mount, the connection id.
	dev     string
	fstype  string
	source  string
	options string
}

// findMountLine returns this mount's own mountinfo line, matched by the
// mountpoint path. The mountpoint field is field 5 (index 4); everything
// after the " - " separator is fstype, source, super options.
func findMountLine(mountpoint string) (mountLine, bool) {
	raw, err := os.ReadFile(procMountinfo)
	if err != nil {
		return mountLine{}, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Split(line, " - ")
		if len(fields) != 2 {
			continue
		}
		head := strings.Fields(fields[0])
		tail := strings.Fields(fields[1])
		if len(head) < 5 || len(tail) < 3 {
			continue
		}
		// mountinfo escapes space/tab/newline as \040 etc.; a mountpoint
		// carrying one cannot appear raw, so unescape before comparing.
		if unescapeMountPath(head[4]) != mountpoint {
			continue
		}
		return mountLine{dev: head[2], fstype: tail[0], source: tail[1], options: tail[2]}, true
	}
	return mountLine{}, false
}

// unescapeMountPath decodes mountinfo's octal escapes (\040, \011, \012,
// \134) the kernel spec defines. Anything else passes through unchanged.
func unescapeMountPath(p string) string {
	if !strings.Contains(p, "\\") {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] != '\\' || i+3 >= len(p) {
			b.WriteByte(p[i])
			continue
		}
		v, err := strconv.ParseUint(p[i+1:i+4], 8, 8)
		if err != nil {
			b.WriteByte(p[i])
			continue
		}
		b.WriteByte(byte(v))
		i += 3
	}
	return b.String()
}

// connectionDir resolves this mount's connection directory: the device's
// major:minor IS the connection id for FUSE mounts. The kernel drops the
// directory at unmount, so absence is reported as absence, not error.
func connectionDir(dev string) (string, bool) {
	if dev == "" {
		return "", false
	}
	major, minor, ok := splitDevID(dev)
	if !ok {
		return "", false
	}
	// FUSE connections carry major 0 (misc device). Another major is not a
	// FUSE connection id — say so by not guessing one.
	if major != 0 {
		return "", false
	}
	dir := filepath.Join(sysFSFuseConnections, strconv.FormatInt(minor, 10))
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", false
	}
	return dir, true
}

// splitDevID parses "major:minor" into its two numbers.
func splitDevID(dev string) (int64, int64, bool) {
	parts := strings.SplitN(dev, ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	major, err1 := strconv.ParseInt(parts[0], 10, 64)
	minor, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// readSysInt reads one integer from a sysfs file. Absence, a permission
// failure and a non-integer are all "cannot observe" — the caller degrades
// that capability to unknown. The kernel prints these knobs unsigned; a
// negative value is not a number this probe accepts.
func readSysInt(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// bdiReadAheadKB resolves this mount's backing-device readahead in BYTES.
// The BDI id for a FUSE mount is the connection's major:minor, and the
// knob's unit is its name: read_ahead_kb is KILOBYTES, so the probe
// converts — the status document reports bytes, the unit every other
// requested/effective figure in the fuse block uses.
func bdiReadAheadKB(dev string) (uint64, bool) {
	major, minor, ok := splitDevID(dev)
	if !ok {
		return 0, false
	}
	v, ok := readSysInt(filepath.Join(sysClassBDI,
		fmt.Sprintf("%d:%d", major, minor), "read_ahead_kb"))
	if !ok {
		return 0, false
	}
	return v * 1024, true
}

// fuseRequests is what this mount REQUESTED from the kernel — the
// MountOptions values the mount was built with, in the probe's own
// vocabulary so the comparison cells can drive it without a server. Built
// by requestedFrom (fusecaps_runtime_linux.go) from the constructed options.
type fuseRequests struct {
	maxBackground      int
	congestion         int
	readAhead          int // the MaxReadAhead figure, in bytes; 0 = kernel default
	maxWrite           int
	maxWriteKnown      bool // a request of 0 means "unset"; known==true means an explicit figure was given
	locks              bool
	symlinkCache       bool
	disableReaddirPlus bool
	disableSplice      bool
}

// probeFuseCapabilities compares what this mount requested against what the
// kernel is actually running, from the observables above. It is called
// after fs.Mount succeeds; the mount line may legitimately be absent (an
// exotic namespace, a raced unmount) — every capability then degrades to
// unknown and the mount proceeds, because a probe must never fail the
// mount.
func probeFuseCapabilities(mountpoint string, ks *fuseKernelSettings, req fuseRequests) FuseState {
	st := FuseState{}
	line, ok := findMountLine(mountpoint)
	if !ok {
		st.Source = fmt.Sprintf("absent from %s", procMountinfo)
		st.Capabilities = append(st.Capabilities,
			capUnknown(capMaxBackground, formatReqInt(req.maxBackground),
				fmt.Sprintf("not observable via %s (this mount's line is absent)", procMountinfo)),
			capUnknown(capCongestion, formatReqInt(req.congestion),
				fmt.Sprintf("not observable via %s (this mount's line is absent)", procMountinfo)),
			capUnknown(capMaxReadAhead, formatReqInt(req.readAhead),
				fmt.Sprintf("not observable via %s (this mount's line is absent)", procMountinfo)),
		)
		st.Capabilities = append(st.Capabilities, behaviouralCaps(ks, req)...)
		st.Degradations = fuseDegradations(st)
		return st
	}
	connDir, haveConn := connectionDir(line.dev)
	if haveConn {
		st.Connection = connIDPtr(line.dev)
		st.Source = fmt.Sprintf("%s + %s", procMountinfo, connDir)
	} else {
		st.Source = fmt.Sprintf("%s (connection dir for device %s absent or unreadable)", procMountinfo, line.dev)
	}

	// max_background / congestion_threshold: the kernel's own per-connection
	// files. A mount that requested nothing (<=0) reports the kernel's value
	// as granted — the kernel's default IS in force and there is nothing of
	// ours to refuse.
	st.Capabilities = append(st.Capabilities,
		numericCap(capMaxBackground, req.maxBackground, connDir, "max_background"),
		numericCap(capCongestion, req.congestion, connDir, "congestion_threshold"),
	)

	// max_readahead: the KERNEL-CLAMPED value lives in the BDI knob.
	if v, ok := bdiReadAheadKB(line.dev); ok {
		st.Capabilities = append(st.Capabilities, readAheadCap(req.readAhead, v))
	} else {
		st.Capabilities = append(st.Capabilities,
			capUnknown(capMaxReadAhead, formatReqInt(req.readAhead),
				fmt.Sprintf("not observable via %s (no read_ahead_kb for device %s)", sysClassBDI, line.dev)))
	}

	// max_write: the INIT reply's MaxWrite is the number the kernel agreed
	// to — the only observable the protocol exposes for it (there is no
	// per-connection sysfs file), so its source is named as the INIT reply.
	st.Capabilities = append(st.Capabilities, maxWriteCap(ks, req))

	st.Capabilities = append(st.Capabilities, behaviouralCaps(ks, req)...)
	sortCapabilities(st.Capabilities)
	st.Degradations = fuseDegradations(st)
	return st
}

// numericCap compares one requested numeric knob with the value the kernel
// is running, read from the connection file <connDir>/<file>. requested<=0
// means the mount did not set the knob — the kernel's own default applies,
// there is nothing of ours to refuse, and the entry reports the kernel's
// figure as granted.
func numericCap(name string, requested int, connDir, file string) Capability {
	path := filepath.Join(connDir, file)
	effective, ok := readSysInt(path)
	reqText := formatReqInt(requested)
	if !ok {
		return capUnknown(name, reqText,
			fmt.Sprintf("not observable via %s (the file could not be read, or the connection directory is absent)", path))
	}
	if requested > 0 && int64(requested) != int64(effective) {
		reason := reasonKernelSetOther
		if int64(requested) > int64(effective) {
			reason = reasonKernelClamp
		}
		return Capability{
			Capability: name,
			State:      CapabilityStateDegraded,
			Requested:  reqText,
			Effective:  fmt.Sprintf("%d (%s)", effective, path),
			Reason:     reason,
		}
	}
	return Capability{
		Capability: name,
		State:      CapabilityStateGranted,
		Requested:  reqText,
		Effective:  fmt.Sprintf("%d (%s)", effective, path),
	}
}

// maxWriteCap reports the max_write negotiation. go-fuse silently caps a
// larger request at MAX_KERNEL_WRITE (1 MiB), and the INIT reply's MaxWrite
// field is the number the kernel agreed to — the only observable the
// protocol exposes for it, so the source is named as such.
func maxWriteCap(ks *fuseKernelSettings, req fuseRequests) Capability {
	if ks == nil || !ks.maxWriteKnown {
		return capUnknown(capMaxWrite, formatReqInt(req.maxWrite),
			"not observable via the INIT reply (no kernel settings recorded)")
	}
	if req.maxWrite > 0 && req.maxWrite != int(ks.maxWrite) {
		return Capability{
			Capability: capMaxWrite,
			State:      CapabilityStateDegraded,
			Requested:  formatReqInt(req.maxWrite),
			Effective:  fmt.Sprintf("%d bytes", ks.maxWrite),
			Reason:     reasonKernelClamp,
		}
	}
	return Capability{
		Capability: capMaxWrite,
		State:      CapabilityStateGranted,
		Requested:  formatReqInt(req.maxWrite),
		Effective:  fmt.Sprintf("%d bytes", ks.maxWrite),
	}
}

// readAheadCap compares the requested readahead with the BDI's effective
// knob. The kernel clamps a request ABOVE its own limit down to it — the
// case acceptance criterion 2 pins — and a kernel running some other value
// without a clamp is reported with its own reason rather than blurred into
// the clamp class.
func readAheadCap(requested int, effectiveBytes uint64) Capability {
	if requested > 0 && int64(requested) != int64(effectiveBytes) {
		reason := reasonKernelSetOther
		if int64(requested) > int64(effectiveBytes) {
			reason = reasonKernelClamp
		}
		return Capability{
			Capability: capMaxReadAhead,
			State:      CapabilityStateDegraded,
			Requested:  formatReqInt(requested),
			Effective:  fmt.Sprintf("%d bytes (/sys/class/bdi read_ahead_kb)", effectiveBytes),
			Reason:     reason,
		}
	}
	return Capability{
		Capability: capMaxReadAhead,
		State:      CapabilityStateGranted,
		Requested:  formatReqInt(requested),
		Effective:  fmt.Sprintf("%d bytes (/sys/class/bdi read_ahead_kb)", effectiveBytes),
	}
}

// behaviouralCaps reports the requested optional behaviours. Where the
// kernel exposes evidence it took them — the INIT reply's negotiated flag
// bits, read back through go-fuse's KernelSettings — the entry states it;
// splice is the honest example of the other case: go-fuse derives the
// behaviour from a local flag and per-request state, the kernel grants no
// per-connection observable for it, and the entry stays unknown with the
// source named. An unknown is correct here; a claimed-granted is not.
func behaviouralCaps(ks *fuseKernelSettings, req fuseRequests) []Capability {
	var out []Capability
	// locks: the kernel's negotiated bits carry FLOCK/POSIX lock support.
	name, want := capLocks, req.locks
	switch {
	case ks == nil || !ks.flagsKnown:
		out = append(out, capUnknown(name, formatValue(want),
			"not observable via the INIT reply (no kernel settings recorded)"))
	case want && !ks.locksGranted():
		out = append(out, Capability{
			Capability: name,
			State:      CapabilityStateDegraded,
			Requested:  formatValue(want),
			Effective:  formatValue(ks.locksGranted()),
			Reason:     reasonFeatureAbsent,
		})
	default:
		out = append(out, Capability{
			Capability: name,
			State:      CapabilityStateGranted,
			Requested:  formatValue(want),
			Effective:  formatValue(ks.locksGranted()),
		})
	}

	// symlink caching: same INIT-bits evidence.
	name, want = capSymlinkCache, req.symlinkCache
	switch {
	case ks == nil || !ks.flagsKnown:
		out = append(out, capUnknown(name, formatValue(want),
			"not observable via the INIT reply (no kernel settings recorded)"))
	case want && !ks.symlinksGranted():
		out = append(out, Capability{
			Capability: name,
			State:      CapabilityStateDegraded,
			Requested:  formatValue(want),
			Effective:  formatValue(ks.symlinksGranted()),
			Reason:     reasonFeatureAbsent,
		})
	default:
		out = append(out, Capability{
			Capability: name,
			State:      CapabilityStateGranted,
			Requested:  formatValue(want),
			Effective:  formatValue(ks.symlinksGranted()),
		})
	}

	// readdirplus: the INIT bits again; a mount that DISABLED it requested
	// off and gets off — that is granted, not degraded. A REQUESTED-ON
	// readdirplus the kernel flags lack is exactly the silently-no-oped
	// behaviour the spec forbids reporting as tuned.
	name, want = capReadDirPlus, !req.disableReaddirPlus
	switch {
	case ks == nil || !ks.flagsKnown:
		out = append(out, capUnknown(name, formatValue(want),
			"not observable via the INIT reply (no kernel settings recorded)"))
	case want && !ks.readdirPlusGranted():
		out = append(out, Capability{
			Capability: name,
			State:      CapabilityStateDegraded,
			Requested:  formatValue(want),
			Effective:  formatValue(ks.readdirPlusGranted()),
			Reason:     reasonFeatureAbsent,
		})
	default:
		out = append(out, Capability{
			Capability: name,
			State:      CapabilityStateGranted,
			Requested:  formatValue(want),
			Effective:  formatValue(ks.readdirPlusGranted()),
		})
	}

	// splice: NO kernel-side observable exists — no sysfs file, no INIT bit
	// (it is a userspace transport decision on our side of /dev/fuse, and
	// go-fuse can also fall back mid-flight). The honest report is unknown
	// with the source named; the reason names the option the state derives
	// from, so the entry is auditable without claiming an evidence it does
	// not have.
	out = append(out, capUnknown(capSplice, formatValue(!req.disableSplice),
		fmt.Sprintf("not observable via %s (the kernel exposes no per-connection splice observable; go-fuse derives it from the local DisableSplice flag and per-request state)", sysFSFuseConnections)))
	return out
}

// capUnknown builds the honest-unknown entry (unknown is its own state,
// never rendered as granted).
func capUnknown(name, requested, reason string) Capability {
	return Capability{
		Capability: name,
		State:      CapabilityStateUnknown,
		Requested:  requested,
		Reason:     reason,
	}
}

// connIDPtr parses the device string into the connection id pointer.
func connIDPtr(dev string) *int {
	_, minor, ok := splitDevID(dev)
	if !ok {
		return nil
	}
	v := int(minor)
	return &v
}

// formatReqInt renders a requested byte figure in the shared "N bytes" form.
func formatReqInt(v int) string {
	if v <= 0 {
		return ""
	}
	return fmt.Sprintf("%d bytes", v)
}
