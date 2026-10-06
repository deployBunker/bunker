//go:build !linux && !darwin

package cli

import (
	"errors"
	"os"
)

// errAttachRawUnsupported is returned where this build cannot put a terminal
// into raw mode. It is not fatal to an attach: the CLI falls back to the
// cooked terminal, so the session still works - keystrokes are line-buffered
// and the local terminal's own signal handling stays in charge.
var errAttachRawUnsupported = errors.New("raw terminal mode is not supported on this platform")

// attachIsTerminal reports whether f is a character device (the closest
// portable test this build has).
func attachIsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// attachMakeRaw is unavailable on this platform.
func attachMakeRaw(f *os.File) (func(), error) {
	return nil, errAttachRawUnsupported
}

// attachTerminalSize cannot be read on this platform.
func attachTerminalSize(f *os.File) (cols, rows uint32, ok bool) {
	return 0, 0, false
}

// attachResizeSignal: this platform reports no window-change signal.
func attachResizeSignal() os.Signal { return nil }
