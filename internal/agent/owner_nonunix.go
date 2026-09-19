//go:build !unix

package agent

import "os"

// statOwnerUID reports that this platform exposes no POSIX file owner. Windows
// is the motivating case: its stat results carry Win32 attribute data, not a
// uid, so the owner is UNKNOWN rather than equal to any uid — and
// classifyRuntimeDir treats an unknown owner as stale, which is the fail-SAFE
// direction. bunkerd targets Linux, so this build is a portability fallback.
func statOwnerUID(os.FileInfo) (uint32, bool) { return 0, false }
