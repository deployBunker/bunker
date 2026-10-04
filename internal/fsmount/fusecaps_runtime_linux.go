//go:build linux

// Package fsmount — the go-fuse seams and the post-mount wiring (BFS-052).
//
// Everything the capability probe needs from go-fuse but go-fuse does not
// hand back as a typed view lives here: the kernel's INIT exchange read
// back through Server.KernelSettings() (the negotiated flag bits — the
// kernel's answer to the optional behaviours), the MountOptions values this
// mount was constructed with (what we REQUESTED), and the single call site
// that runs the probe and stores the result for the status document.
package fsmount

import (
	"github.com/hanwen/go-fuse/v2/fuse"
)

// fuseKernelSettings is the probe's typed view of the kernel's INIT
// exchange (go-fuse Server.KernelSettings(), an *fuse.InitIn). The
// negotiated flag bits are the kernel's own answer to the optional
// behaviours. flagsKnown is false only when the server has not answered
// INIT yet — no negotiation has happened, so there is genuinely nothing to
// read and the probe reports unknown rather than guessing.
type fuseKernelSettings struct {
	flagsKnown bool
	flags      uint64
	// maxWrite is the figure the INIT reply carried, recorded by the mount
	// path before fs.Mount (the library's own settings view does not retain
	// the reply's MaxWrite). maxWriteKnown is false only when no figure was
	// negotiated — a probe run against a handle that never mounted.
	maxWriteKnown bool
	maxWrite      uint32
}

// newFuseKernelSettings reads the server's stored settings. A nil server (a
// mount that failed before the bridge started) reports nothing known, and
// the probe degrades honestly.
func newFuseKernelSettings(server *fuse.Server) *fuseKernelSettings {
	if server == nil {
		return nil
	}
	ks := server.KernelSettings()
	if ks == nil {
		return nil
	}
	return &fuseKernelSettings{
		flagsKnown: true,
		flags:      ks.Flags64(),
	}
}

// locksGranted reports whether the negotiated flags carry the flock/POSIX
// lock support EnableLocks asks for.
func (k *fuseKernelSettings) locksGranted() bool {
	return k.flags&(fuse.CAP_FLOCK_LOCKS|fuse.CAP_POSIX_LOCKS) != 0
}

// symlinksGranted reports whether the negotiated flags carry
// CAP_CACHE_SYMLINKS (EnableSymlinkCaching's ask).
func (k *fuseKernelSettings) symlinksGranted() bool {
	return k.flags&fuse.CAP_CACHE_SYMLINKS != 0
}

// readdirPlusGranted reports whether the negotiated flags carry
// CAP_READDIRPLUS.
func (k *fuseKernelSettings) readdirPlusGranted() bool {
	return k.flags&fuse.CAP_READDIRPLUS != 0
}

// requestedFrom builds the request view from this mount's constructed
// MountOptions and the MaxWrite figure the mount recorded before fs.Mount
// (see the negotiatedMaxWrite field). It reads what was ASKED and what the
// library actually passed the kernel — never a runtime view inferred after
// the fact.
func requestedFrom(mo fuse.MountOptions, negotiatedMaxWrite int64) fuseRequests {
	mw := int(negotiatedMaxWrite)
	if mw < 0 {
		mw = 0
	}
	return fuseRequests{
		maxBackground:      mo.MaxBackground,
		congestion:         mo.CongestionThreshold,
		readAhead:          mo.MaxReadAhead,
		maxWrite:           mw,
		maxWriteKnown:      mw > 0,
		locks:              mo.EnableLocks,
		symlinkCache:       mo.EnableSymlinkCaching,
		disableReaddirPlus: mo.DisableReadDirPlus,
		disableSplice:      mo.DisableSplice,
	}
}

// probeFuseState is the MOUNT's entry point: resolve what was requested
// from the constructed options, run the kernel probe, and store the result
// for the status document. It is called after fs.Mount succeeds and before
// the status loop starts. The probe NEVER fails the mount: everything
// inside tolerates absence and degrades to unknown.
func (m *Mount) probeFuseState(mo fuse.MountOptions) {
	req := requestedFrom(mo, m.negotiatedMaxWrite.Load())
	var ks *fuseKernelSettings
	if m.server != nil {
		ks = newFuseKernelSettings(m.server)
		// The negotiated MaxWrite the mount recorded is the figure the INIT
		// reply carried — carried into the settings view so maxWriteCap
		// reports it as the effective value (the library's own settings
		// retain only the kernel's side of the exchange).
		if ks != nil && req.maxWriteKnown {
			ks.maxWriteKnown = true
			ks.maxWrite = uint32(req.maxWrite)
		}
	}
	st := probeFuseCapabilities(m.opts.Mountpoint, ks, req)
	m.fuseMu.Lock()
	m.fuseState = st
	m.fuseMu.Unlock()
	for _, d := range st.Degradations {
		m.logf("bunker-fs: fuse capability %s", d.String())
	}
}

// fuseProbeSkipped reports whether the capability probe was skipped — the
// !linux record the status document carries (see the Fuse field's own
// comment). Linux builds never skip: the probe runs on every mount and its
// unknowns carry their own reasons.
func fuseProbeSkipped() (skipped bool, reason string) {
	return false, ""
}

// FuseState returns the probed kernel state for the status document, the
// battery and the live probe path (acceptance criterion 6).
func (m *Mount) FuseState() FuseState {
	m.fuseMu.RLock()
	defer m.fuseMu.RUnlock()
	return m.fuseState
}
